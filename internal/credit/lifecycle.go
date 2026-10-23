package credit

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/catalyst/billing/internal/audit"
	"github.com/catalyst/billing/internal/ledger"
	"github.com/catalyst/billing/internal/money"
	"github.com/catalyst/billing/internal/settings"
	"github.com/jackc/pgx/v5"
)

// The credit that a price change owes (CRD-06, UPG-04, CRD-10).
//
// An upgrade and a downgrade are one arithmetic in two directions. `service.Store` computes the
// prorated difference exactly (one big.Rat, one RoundHalfUp) and records it on a
// `service_upgrades` row. The positive side became money long ago: the upgrade path issues a
// prorated invoice in the same transaction as the row. The negative side did not. The row said
// "the customer is owed 8.42" and nothing ever posted 8.42 anywhere, so:
//
//   - the customer's balance did not move, which is a customer paying for time they will not use;
//   - `ApplyDowngrade` refused to move the service, correctly, because the cheaper price would
//     otherwise be granted before the credit was given — so the customer also kept renewing at the
//     higher price. Two wrongs that hid each other.
//
// This file is the missing half. It posts a *credit note* (numbered, immutable, from the same
// series as a goodwill grant) and the ledger entry behind it, and it links the note to the row so
// the state "credited but not yet applied" is visible rather than inferred.
//
// # Why the ledger reference is a new type
//
// A grant (`credit_grant`) and a deposit (`credit_deposit`) already post credit. A downgrade is
// the third economic event and it is not either of those: no operator decided it, and no money
// arrived. The reference type is what makes the posting idempotent on its own row — the unique
// key is (kind, reference_type, reference_id) — so a retried job finds the existing entry instead
// of crediting twice. Reusing `credit_grant` would collide with a real grant whose id happened to
// be derived from the same string, and it would make the customer's statement claim a goodwill
// note was issued.
//
// # Why posting and applying are two steps
//
// `service.Store.ApplyDowngrade` owns its own transaction (it reprices the subscription and
// enqueues a provisioning operation), so it cannot be nested inside this one. Two idempotent
// steps, in this order, is the safe shape: if the process dies between them the row is pending
// with `credit_note_id` set, and the next run posts nothing and applies it. The reverse order
// would give the cheaper price for time that was never credited.

// RefCreditDowngrade is the return of the unused part of a paid period when a service moves to a
// cheaper price. It is separate from RefCreditGrant because a goodwill note and a prorated return
// are different events, and a replay of one must not be mistaken for the other.
const RefCreditDowngrade ledger.ReferenceType = "credit_downgrade"

// Errors specific to a downgrade credit.
var (
	// ErrNotDowngrade is returned when a row that does not owe the customer anything is asked
	// to pay a credit: an upgrade, or a change whose prorated difference rounded to zero.
	ErrNotDowngrade = errors.New("credit: that change did not owe the customer a credit")
	// ErrDowngradeDisabled is returned when `credits.credit_on_downgrade` is off. It is a
	// separate value from ErrDisabled so a caller can say "this installation does not return
	// credit on a downgrade" rather than "customer credit is switched off entirely" — two very
	// different sentences to put in front of a customer.
	ErrDowngradeDisabled = errors.New("credit: downgrade credits are disabled")
	// ErrNotPending is returned when a downgrade row is not in the state a posting requires:
	// already applied, cancelled or failed. Answering it would pay a credit for a change that is
	// not going to happen.
	ErrNotPending = errors.New("credit: the downgrade is not pending")
)

// DowngradeCreditRequest asks to post the credit a downgrade owes.
type DowngradeCreditRequest struct {
	// UpgradeID is the `service_upgrades` row. The credit's note and its ledger reference are
	// both derived from it, so the request carries no idempotency key: the row *is* the identity,
	// and two callers asking at once must produce one note.
	UpgradeID string
	// PostedBy is the operator's user id when a person asked for it. Empty means the automation
	// did, and the audit row records a system actor.
	PostedBy string
}

// DowngradeCredit is the note that paid a downgrade.
type DowngradeCredit struct {
	UpgradeID    string
	ServiceID    string
	CustomerID   string
	CreditNoteID string
	Number       string
	Currency     string
	// AmountMinor is the credit posted, always positive.
	AmountMinor int64
	Reason      string
	CreatedAt   time.Time
	// AlreadyPosted is true when this row had already been credited. The amounts describe the
	// existing note; nothing new was posted.
	AlreadyPosted bool
}

// PostDowngradeCredit posts the credit a pending downgrade owes, in its own transaction.
func (s *Service) PostDowngradeCredit(ctx context.Context, req DowngradeCreditRequest) (DowngradeCredit, error) {
	if s.pool == nil {
		return DowngradeCredit{}, ErrInvalidRequest
	}
	if strings.TrimSpace(req.UpgradeID) == "" {
		return DowngradeCredit{}, fmt.Errorf("%w: an upgrade is required", ErrInvalidRequest)
	}

	var out DowngradeCredit
	err := s.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = postDowngradeCreditTx(ctx, tx, req.UpgradeID, req.PostedBy, s.auditor, s.depositNow())
		return err
	})
	if err != nil {
		return DowngradeCredit{}, err
	}
	return out, nil
}

func postDowngradeCreditTx(ctx context.Context, tx pgx.Tx, upgradeID, actorID string,
	auditor audit.Auditor, now time.Time) (DowngradeCredit, error) {
	var (
		serviceID    string
		customerID   string
		currencyCode string
		prorated     int64
		status       string
		noteID       *string
	)
	err := tx.QueryRow(ctx, `
		SELECT u.service_id, s.customer_id, u.currency, u.prorated_minor, u.status,
		       u.credit_note_id::text
		FROM service_upgrades u
		JOIN services s ON s.id = u.service_id
		WHERE u.id = $1
		FOR UPDATE OF u`, upgradeID).
		Scan(&serviceID, &customerID, &currencyCode, &prorated, &status, &noteID)
	if errors.Is(err, pgx.ErrNoRows) {
		return DowngradeCredit{}, ErrNotFound
	}
	if err != nil {
		return DowngradeCredit{}, fmt.Errorf("credit: lock downgrade: %w", err)
	}

	// A replay is answered before the settings are read. An operator who turned downgrade
	// credits off after a note was issued must still get that note back rather than a refusal:
	// the money is already the customer's.
	if noteID != nil {
		var (
			out       DowngradeCredit
			number    *string
			amount    int64
			reason    string
			createdAt time.Time
			currency  string
		)
		if err := tx.QueryRow(ctx, `
			SELECT number, total_minor, currency, reason, created_at
			FROM credit_notes WHERE id = $1`, *noteID).
			Scan(&number, &amount, &currency, &reason, &createdAt); err != nil {
			return DowngradeCredit{}, fmt.Errorf("credit: load downgrade note: %w", err)
		}
		out = DowngradeCredit{
			UpgradeID: upgradeID, ServiceID: serviceID, CustomerID: customerID,
			CreditNoteID: *noteID, Number: derefString(number), Currency: currency,
			AmountMinor: amount, Reason: reason, CreatedAt: createdAt, AlreadyPosted: true,
		}
		return out, nil
	}

	if status != "pending" {
		return DowngradeCredit{}, fmt.Errorf("%w: its status is %s", ErrNotPending, status)
	}
	if prorated >= 0 {
		return DowngradeCredit{}, fmt.Errorf("%w: the prorated difference is %d minor units",
			ErrNotDowngrade, prorated)
	}

	onDowngrade, err := creditOnDowngrade(ctx, tx)
	if err != nil {
		return DowngradeCredit{}, err
	}
	if !onDowngrade {
		return DowngradeCredit{}, ErrDowngradeDisabled
	}

	cur, err := resolveCurrency(currencyCode)
	if err != nil {
		return DowngradeCredit{}, err
	}

	// Lock the customer before allocating a number, so a missing customer is a clean not-found
	// rather than a foreign-key failure after a sequence value has been burned, and so two
	// concurrent credits for the same customer serialise.
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT true FROM customers WHERE id = $1 FOR UPDATE`,
		customerID).Scan(&exists); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return DowngradeCredit{}, ErrNotFound
		}
		return DowngradeCredit{}, fmt.Errorf("credit: lock customer: %w", err)
	}

	allocated, err := allocateCreditNumber(ctx, tx)
	if err != nil {
		return DowngradeCredit{}, err
	}

	newNoteID := derivedUUID("credit-downgrade:" + upgradeID)
	amount := -prorated
	reason := "Prorated credit for the unused part of the billing period"

	if _, err := tx.Exec(ctx, `
		INSERT INTO credit_notes (id, number, customer_id, invoice_id, currency,
			total_minor, tax_minor, reason, status, issued_at, snapshot, created_by)
		VALUES ($1, $2, $3, NULL, $4, $5, 0, $6, 'issued', $7, $8, $9)`,
		newNoteID, allocated, customerID, cur.Code(), amount, reason, now,
		map[string]any{
			"schema_version": 1,
			"kind":           "downgrade",
			"amount":         amount,
			"currency":       cur.Code(),
			"reason":         reason,
			"upgrade_id":     upgradeID,
			"service_id":     serviceID,
		}, nullableUUID(actorID),
	); err != nil {
		return DowngradeCredit{}, fmt.Errorf("credit: insert downgrade note: %w", err)
	}

	// The posting mirrors a grant: an allowance reduces revenue and creates a liability to the
	// customer. It is not a refund (no money leaves the shop) and not a receivable movement (no
	// invoice total changed), which is why the counter-account is revenue rather than either of
	// those.
	if _, _, err := ledger.Post(ctx, tx, ledger.Posting{
		Kind:        ledger.KindAdjustment,
		Reference:   RefCreditDowngrade,
		ReferenceID: newNoteID,
		CustomerID:  customerID,
		Currency:    cur,
		Debits: []ledger.Entry{{
			Account: ledger.AccountRevenue,
			Amount:  money.New(amount, cur),
			Memo:    "downgrade credit " + allocated,
		}},
		Credits: []ledger.Entry{{
			Account: ledger.AccountCredit,
			Amount:  money.New(amount, cur),
			Memo:    reason,
		}},
	}); err != nil {
		return DowngradeCredit{}, fmt.Errorf("credit: post downgrade credit: %w", err)
	}

	// The link is written in the same transaction as the posting, so its presence is evidence the
	// money moved rather than a plan to move it.
	if _, err := tx.Exec(ctx, `
		UPDATE service_upgrades
		SET credit_note_id = $2, credit_posted_at = $3
		WHERE id = $1`, upgradeID, newNoteID, now); err != nil {
		return DowngradeCredit{}, fmt.Errorf("credit: link downgrade credit: %w", err)
	}

	if err := recordCreditAudit(ctx, tx, auditor, "service.downgrade_credited", "service",
		serviceID, actorID, map[string]any{
			"upgrade_id":     upgradeID,
			"credit_note_id": newNoteID,
			"number":         allocated,
			"amount":         amount,
			"currency":       cur.Code(),
			"customer_id":    customerID,
		}); err != nil {
		return DowngradeCredit{}, err
	}

	return DowngradeCredit{
		UpgradeID: upgradeID, ServiceID: serviceID, CustomerID: customerID,
		CreditNoteID: newNoteID, Number: allocated, Currency: cur.Code(),
		AmountMinor: amount, Reason: reason, CreatedAt: now,
	}, nil
}

// PendingDowngrades lists the downgrades that are waiting to be credited, or that have been
// credited and are waiting to be applied.
//
// `creditedOnly` selects the second group. Both are bounded because they feed a worker: a
// backlog drains over several ticks rather than in one transaction that holds row locks, and a
// scan that returned everything would eventually be a scan that returned nothing useful.
func PendingDowngrades(ctx context.Context, q Querier, creditedOnly bool, limit int) ([]string, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := q.Query(ctx, `
		SELECT id::text FROM service_upgrades
		WHERE status = 'pending' AND prorated_minor < 0
		  AND (NOT $1 OR credit_note_id IS NOT NULL)
		ORDER BY created_at
		LIMIT $2`, creditedOnly, limit)
	if err != nil {
		return nil, fmt.Errorf("credit: list pending downgrades: %w", err)
	}
	defer rows.Close()

	out := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("credit: scan pending downgrade: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// PostPendingDowngradeCredits credits every pending downgrade that owes one, in a bounded batch.
//
// The two return values are the upgrades it acted on and the ones it could not. One unpostable
// row must not stop the batch: a downgrade whose customer was deleted is an operator's problem,
// and the next tick retries it while the row stays visibly pending.
func (s *Service) PostPendingDowngradeCredits(ctx context.Context, limit int) (posted, failed []string, err error) {
	pending, err := PendingDowngrades(ctx, s.pool, false, limit)
	if err != nil {
		return nil, nil, err
	}
	for _, upgradeID := range pending {
		res, perr := s.PostDowngradeCredit(ctx, DowngradeCreditRequest{UpgradeID: upgradeID})
		if perr != nil {
			if errors.Is(perr, ErrDowngradeDisabled) || errors.Is(perr, ErrDisabled) {
				// The operator has decided downgrades are not refunded. That is not a failed
				// batch, and retrying every tick would fill the log with it.
				return posted, failed, nil
			}
			failed = append(failed, upgradeID)
			continue
		}
		if res.AlreadyPosted {
			// A row another caller credited first is not this batch's work. Counting it would
			// make a re-run report the same upgrade twice, and the caller could not tell a
			// drained queue from a re-credited one.
			continue
		}
		posted = append(posted, upgradeID)
	}
	return posted, failed, nil
}

// creditOnDowngrade reads `credits.credit_on_downgrade`, with the registry's default when the
// row is absent.
//
// The default comes from internal/settings rather than a literal here, for the same reason every
// other credit default does: a second copy of "true" would drift the first time an operator
// changed the registry, and the drift would show up as a settings page that says one thing and a
// posting rule that does another.
func creditOnDowngrade(ctx context.Context, q Querier) (bool, error) {
	fallback := true
	if def, ok := settings.Lookup(settings.CreditGroup, settings.CreditOnDowngradeKey); ok {
		if parsed, err := strconv.ParseBool(strings.TrimSpace(def.Default)); err == nil {
			fallback = parsed
		}
	}

	var raw string
	err := q.QueryRow(ctx, `SELECT value FROM settings WHERE group_key = $1 AND key = $2`,
		settings.CreditGroup, settings.CreditOnDowngradeKey).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return fallback, nil
	}
	if err != nil {
		return fallback, fmt.Errorf("credit: read downgrade setting: %w", err)
	}
	return parseBool(raw, fallback), nil
}

// recordCreditAudit writes an audit row through the caller's auditor when one is attached, and
// otherwise as a system action. It is the package-level twin of Service.recordAudit, needed by
// the transaction-shaped entry point, which has no Service to read an auditor from.
func recordCreditAudit(ctx context.Context, tx pgx.Tx, auditor audit.Auditor, action, targetType,
	targetID, actorID string, changes map[string]any) error {
	if auditor != nil {
		return auditor.RecordAction(ctx, tx, action, targetType, targetID, changes)
	}
	ev := audit.Event{
		Actor:      audit.Actor{ID: actorID, Kind: audit.ActorUser},
		Action:     action,
		TargetType: targetType,
		TargetID:   targetID,
		Outcome:    audit.OutcomeSuccess,
		Changes:    changes,
	}
	if actorID == "" {
		ev.Actor = audit.Actor{Kind: audit.ActorSystem}
	}
	return audit.Record(ctx, tx, ev)
}
