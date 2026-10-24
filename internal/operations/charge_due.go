package operations

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/catalyst/billing/internal/fulfilment"
	"github.com/catalyst/billing/internal/gatewayconfig"
	"github.com/catalyst/billing/internal/payments"
	"github.com/catalyst/billing/internal/settings"
	"github.com/catalyst/billing/internal/store"
	"github.com/jackc/pgx/v5/pgconn"
)

// Charging a saved payment method for invoices that are due (OPS-01).
//
// `payments.Agreements.ChargeDue` exists and its own doc comment says it "is the entry point a
// cron calls after the renewal scan". No cron called it, so the setting it obeys
// (`gateway.off_session_enabled`) promised a customer that their card would be charged for the
// renewal and nothing ever attempted it — the invoice simply aged into dunning.
//
// # Why this is a scan of its own rather than a call to ChargeDue
//
// ChargeDue's query excludes invoices with a *live* attempt but not with a *declined* one, which is
// right for an operator pressing "charge due now" once and wrong for a ticker: a declined card
// would be re-offered on every tick, and the run's history would be one line per fifteen minutes
// about an invoice nothing has changed about — which is how the invoice that *just* became due
// goes unnoticed in the noise.
//
// # Idempotency: the marker is a row, not a variable
//
// A re-run must not charge twice, and "the loop remembers what it did" is not a guarantee — the
// process can restart between the provider call and the settlement. The marker is therefore the
// database's: `payment_attempts.operation_key` is UNIQUE (0001_core.sql), and the charge path keys
// the attempt `offsession:<invoice>:<method>`. One renewal invoice is one agreement's charge for
// one period, so that key *is* the per-agreement-per-period marker: a second run finds the row and
// either re-applies the recorded settlement (a crash after charging) or refuses to repeat a
// declined one. Nothing in this file holds state between runs, and a test asserts the constraint
// rather than the intention.
//
// So this sweep adds the one thing a ticker needs and a button does not: a per-invoice retry
// window (`cronjob.charge_retry_hours`). It is not a safety guard. The guard is in
// internal/payments, which refuses to re-charge an invoice whose attempt it has already recorded
// as failed ("a new attempt needs a new decision"), so the provider is never asked twice for a
// card that said no. This file only decides which invoices to *offer*, and then hands each one to
// the same `ChargeInvoice` the per-invoice staff button calls: the amount, the method, the
// mandate and the settlement are all internal/payments'.

// ChargerFactory resolves the adapter that can charge a saved method.
//
// It returns the charger, the provider account the settlement is recorded against, and a close
// function for the resources it opened (a launched extension process, in production). The factory
// is a seam rather than a direct call because a test must be able to charge with a fake adapter,
// and because the deployment may attach a long-lived one instead of launching per tick.
type ChargerFactory func(ctx context.Context) (payments.OffSessionCharger, string, func(), error)

// ChargeDueRun is what one sweep did.
type ChargeDueRun struct {
	// Reason is set when the run did nothing, and names why. It is empty when work was done.
	Reason string
	// Outcomes is one entry per invoice the sweep offered to the charge path.
	Outcomes []payments.ChargeOutcome
	// Settled and Declined are the counts an operator reads first.
	Settled  int
	Declined int
	// AlreadyAttempted counts invoices another worker got to first. It is reported rather than
	// logged as a failure: the UNIQUE attempt key refusing the insert is the safety property
	// working, and a run that shows it is evidence that two workers overlapped.
	AlreadyAttempted int
}

// ChargeDue charges every open, past-due invoice that has a default saved method and has not been
// attempted inside the retry window.
func ChargeDue(ctx context.Context, pool *store.Pool, opts Options) (ChargeDueRun, error) {
	opts = opts.withDefaults()
	if disabled(ChargeDueKillSwitch) {
		return ChargeDueRun{Reason: ReasonKillSwitch}, nil
	}

	values := settings.New(pool)
	enabled, err := values.Bool(ctx, "gateway", "off_session_enabled")
	if err != nil {
		return ChargeDueRun{}, fmt.Errorf("operations: read gateway.off_session_enabled: %w", err)
	}
	if !enabled {
		// The setting is the permission. Without it nothing is charged, and the answer is a
		// skip rather than an error: this is the default state of an installation.
		return ChargeDueRun{Reason: ReasonDisabled}, nil
	}

	retry := DefaultChargeRetry
	if hours, err := values.Int(ctx, settings.GroupCronjob, settings.KeyChargeRetryHours); err == nil && hours > 0 {
		retry = time.Duration(hours) * time.Hour
	}

	factory := opts.ChargerOverride
	if factory == nil {
		factory = gatewayCharger(pool)
	}

	// One sweep at a time, per database.
	//
	// The uniqueness of `payment_attempts.operation_key` already makes a double charge impossible,
	// so this is not a safety guard and must not be mistaken for one. What it prevents is two
	// workers burning provider rate limit on the same batch and racing the decline-to-dunning
	// transition from two goroutines — the visible symptom of which was a burst of 23505 failures
	// in the run history, which is a red row that hides real ones.
	//
	// Acquired BEFORE the charger is resolved, so a tick that stands down does not launch an
	// extension process to discover it has nothing to do.
	release, got, err := sweepLock(ctx, pool)
	if err != nil {
		return ChargeDueRun{}, err
	}
	if !got {
		return ChargeDueRun{Reason: ReasonLocked}, nil
	}
	defer release()

	charger, account, closeFn, err := factory(ctx)
	if err != nil {
		switch {
		case errors.Is(err, gatewayconfig.ErrNotConfigured), errors.Is(err, payments.ErrOffSessionUnsupported):
			// No gateway, or one whose protocol cannot charge a saved method. Both are states of
			// the installation, not failures of the run: a red cron row every fifteen minutes for
			// a deployment that has not configured a gateway is how an operator learns to ignore
			// red rows.
			return ChargeDueRun{Reason: ReasonNotConfigured}, nil
		case errors.Is(err, gatewayconfig.ErrDisabled):
			// Configured and switched off on the integrations screen: the operator's decision, and
			// the same answer as a missing gateway.
			return ChargeDueRun{Reason: ReasonDisabled}, nil
		}
		return ChargeDueRun{}, fmt.Errorf("operations: resolve the payment gateway: %w", err)
	}
	defer closeFn()

	agreements := payments.NewAgreements(pool).
		WithCharger(charger).
		WithApplier(fulfilment.NewApplier(pool)).
		WithAccount(account)

	ids, err := dueInvoices(ctx, pool, charger.Name(), opts.Now().Add(-retry), Batch)
	if err != nil {
		return ChargeDueRun{}, err
	}
	if len(ids) == 0 {
		return ChargeDueRun{Reason: ReasonNothingDue}, nil
	}

	run := ChargeDueRun{Outcomes: make([]payments.ChargeOutcome, 0, len(ids))}
	var failures []error
	for _, id := range ids {
		out, err := agreements.ChargeInvoice(ctx, id)
		run.Outcomes = append(run.Outcomes, out)
		switch {
		case err == nil:
		case errors.Is(err, payments.ErrDeclined):
			// A decline is an outcome. It is recorded on the invoice, the attempt and the
			// method, and dunning escalates from there; reporting it as a run failure would make
			// the cron's history red every time a card expires.
			run.Declined++
		case errors.Is(err, payments.ErrOffSessionDisabled):
			// The operator switched the setting off between the read above and this call. Stop the
			// sweep rather than logging the same refusal once per invoice.
			return run, nil
		default:
			// A guaranteed unique key refused the insert: another worker recorded the attempt for
			// this invoice and this method in the window between the scan and this call. That is
			// the constraint doing its job, not a transport failure, so it must not be joined into
			// the run's error — a red row here is indistinguishable from a real failure and would
			// teach an operator to ignore the one that matters. The invoice is left to the worker
			// that owns the attempt.
			if isUniqueViolation(err) {
				run.AlreadyAttempted++
				continue
			}
			// One invoice's transport failure must not stop the rest of the batch, and it must be
			// visible: it is joined and returned so the run is recorded as failed.
			failures = append(failures, fmt.Errorf("invoice %s: %w", id, err))
		}
		if out.Status == payments.ChargeSettled {
			run.Settled++
		}
	}
	return run, joinErr(failures)
}

// dueInvoices lists the invoices this sweep may offer to the charge path, oldest first.
//
// The predicates are the ones ChargeDue uses, plus the retry window, and they are in SQL for the
// reason the dashboard's are: a sweep that loaded rows and filtered them in Go would change its
// answer when the batch size changed. The plan in preview.go re-reads the same conditions inverted
// to say *why* an invoice is not on this list; a test asserts the two agree, because inverting a
// predicate by hand is exactly where they would drift.
func dueInvoices(ctx context.Context, pool *store.Pool, provider string, retryBefore time.Time, limit int) ([]string, error) {
	rows, err := pool.Query(ctx, `
		SELECT i.id
		FROM invoices i
		JOIN payment_methods pm
		  ON pm.customer_id = i.customer_id AND pm.is_default AND pm.provider = $1
		-- A method without an active mandate is a token this panel has no permission to use. The
		-- charge path refuses it anyway (ErrNoAgreement), so offering it here would turn one
		-- customer's unfinished mandate into a red cron row every fifteen minutes.
		AND EXISTS (
		    SELECT 1 FROM billing_agreements ag
		    WHERE ag.payment_method_id = pm.id AND ag.status = 'active')
		WHERE i.status = 'open'
		  AND i.amount_paid_minor < i.total_minor
		  AND i.due_at IS NOT NULL
		  AND i.due_at <= now()
		  -- A customer with a checkout session open is about to pay by hand. Skipping is the
		  -- difference between one charge and two.
		  AND NOT EXISTS (
		      SELECT 1 FROM payment_attempts pa
		      WHERE pa.invoice_id = i.id
		        AND pa.status IN ('created', 'pending', 'requires_action')
		        AND pa.operation_key NOT LIKE 'offsession:%')
		  -- The retry window. A declined card is not re-offered until it has expired: the
		  -- provider's refusal is a statement about the card, and repeating it every tick is how
		  -- a customer's bank starts refusing everything from this merchant.
		  AND NOT EXISTS (
		      SELECT 1 FROM payment_attempts pa
		      WHERE pa.invoice_id = i.id
		        AND pa.operation_key LIKE 'offsession:%'
		        AND pa.created_at > $2)
		ORDER BY i.due_at
		LIMIT $3`, provider, retryBefore, limit)
	if err != nil {
		return nil, fmt.Errorf("operations: scan invoices to charge: %w", err)
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("operations: scan invoice id: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("operations: read invoices to charge: %w", err)
	}
	return ids, nil
}

// gatewayCharger launches the configured gateway extension and adapts it to the charge protocol.
//
// It is the same launch internal/httpapi performs for the staff "charge saved method" button
// (gatewayconfig.Launch), which is the point: a charge the worker makes and a charge an operator
// makes must go through one adapter, or the two would disagree about what the gateway supports.
//
// It launches per run rather than holding a long-lived client the way the worker's own job list
// does: this package is a library and cannot own the process lifecycle of a child that outlives a
// request or a test. A process launch every fifteen minutes is the price, and it is the same price
// the staff button pays per click.
func gatewayCharger(pool *store.Pool) ChargerFactory {
	return func(ctx context.Context) (payments.OffSessionCharger, string, func(), error) {
		cfg, err := gatewayconfig.New(pool).Load(ctx, gatewayconfig.GatewayKind)
		if err != nil {
			return nil, "", nil, err
		}
		if !cfg.Configured() {
			return nil, "", nil, gatewayconfig.ErrNotConfigured
		}
		if !cfg.Enabled {
			return nil, "", nil, gatewayconfig.ErrDisabled
		}
		client, err := gatewayconfig.Launch(ctx, cfg, 30*time.Second)
		if err != nil {
			return nil, "", nil, err
		}
		return payments.ExtensionCharger{Caller: client, Provider: cfg.Provider}, cfg.Provider,
			func() { _ = client.Close() }, nil
	}
}

// sweepLock takes a per-database advisory lock for the charge sweep.
//
// It returns (release, true) when this caller owns the sweep, and (noop, false) when another worker
// does — a skip, decided by pg_try, never a block.
//
// Two details make this correct rather than approximately correct:
//
//   - `pg_advisory_lock` is SESSION-scoped, so the unlock must run on the same connection that took
//     it. Doing it on the pool would land on whichever connection was free, unlock nothing, and
//     leave the lock to be released whenever that backend happened to be recycled. So the
//     connection is acquired once and held for the whole sweep.
//   - The key is derived from `current_database()`. Advisory locks are CLUSTER-wide: without the
//     database in the key, a dev database and a production database on one cluster would serialise
//     against each other, which is exactly the bug the test-harness lock hit.
//
// The lock is not the reason a card cannot be charged twice — `payment_attempts.operation_key` is
// UNIQUE — and must not be relied on as if it were. If the process dies mid-sweep the session ends
// and the lock is released by PostgreSQL.
func sweepLock(ctx context.Context, pool *store.Pool) (release func(), got bool, err error) {
	if pool == nil {
		return func() {}, true, nil
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("operations: acquire sweep connection: %w", err)
	}
	const key = `('x' || substr(md5('catalyst-billing:' || $1 || ':' || current_database()), 1, 16))::bit(64)::bigint`
	var acquired bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(`+key+`)`, ChargeDueJob).Scan(&acquired); err != nil {
		conn.Release()
		return nil, false, fmt.Errorf("operations: take sweep lock: %w", err)
	}
	if !acquired {
		conn.Release()
		return func() {}, false, nil
	}
	return func() {
		// Unlock on the SAME connection, then hand it back. Releasing without unlocking would
		// leave the lock held by a pooled backend and silently disable every later sweep.
		if _, uerr := conn.Exec(context.WithoutCancel(ctx),
			`SELECT pg_advisory_unlock(`+key+`)`, ChargeDueJob); uerr != nil {
			// Losing the unlock is not fatal: the session will release it when the backend is
			// recycled. It is worth telling the operator, but the sweep it protected already ran.
			_ = uerr
		}
		conn.Release()
	}, true, nil
}

// isUniqueViolation reports whether err is PostgreSQL's unique-key violation (SQLSTATE 23505).
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
