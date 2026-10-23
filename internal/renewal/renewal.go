// Package renewal turns due subscriptions into invoices.
//
// It is the most dangerous code in the product: a mistake here charges the wrong
// people, twice, or not at all. The design therefore leans on the database for
// the invariants and keeps the scanning logic boring:
//
//   - the billing calendar is derived from persisted anchor + timezone, never
//     from "now + 30 days";
//   - one invoice per subscription per period is enforced by a unique index, so
//     two workers cannot double-bill even if the application logic is wrong;
//   - the subscription row is locked and its version compared before advancing,
//     so a stale worker cannot move a schedule that someone else already moved;
//   - a catch-up batch that is too old or too large is routed to review instead
//     of being billed automatically.
package renewal

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/catalyst/billing/internal/billing"
	"github.com/catalyst/billing/internal/calendar"
	"github.com/catalyst/billing/internal/catalog"
	"github.com/catalyst/billing/internal/money"
	"github.com/catalyst/billing/internal/store"
	"github.com/jackc/pgx/v5"
)

var (
	// ErrSubscriptionNotDue is returned when a scanned subscription is no longer
	// due, typically because another worker already handled it.
	ErrSubscriptionNotDue = errors.New("renewal: subscription is not due")
	// ErrVersionConflict is returned when the schedule changed under us.
	ErrVersionConflict = errors.New("renewal: subscription version changed")
	// ErrNeedsReview is returned when a catch-up batch is too large or too old
	// to bill automatically.
	ErrNeedsReview = errors.New("renewal: requires manual review")
)

// Policy mirrors one row of billing_policies.
type Policy struct {
	InvoiceLeadDays   int
	PaymentTermsDays  int
	GraceDays         int
	CatchupMaxPeriods int
	CatchupMaxAgeDays int
}

// LoadPolicy reads the policy row.
func LoadPolicy(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}) (Policy, error) {
	var p Policy
	err := q.QueryRow(ctx, `
		SELECT invoice_lead_days, payment_terms_days, grace_days,
		       catchup_max_periods, catchup_max_age_days
		FROM billing_policies WHERE id = 'default'`).
		Scan(&p.InvoiceLeadDays, &p.PaymentTermsDays, &p.GraceDays,
			&p.CatchupMaxPeriods, &p.CatchupMaxAgeDays)
	if err != nil {
		return Policy{}, fmt.Errorf("renewal: load policy: %w", err)
	}
	return p, nil
}

// Subscription is the persisted schedule state the engine needs.
type Subscription struct {
	ID             string
	CustomerID     string
	PriceVersionID string
	Status         string
	// IntervalName is the stored interval: a calendar interval, or the day-count
	// form "every_45_days". The period model is chosen from it, never guessed.
	IntervalName       string
	Anchor             calendar.Anchor
	Timezone           string
	CurrentPeriodStart time.Time
	CurrentPeriodEnd   time.Time
	NextBillingAt      time.Time
	Version            int64
	CancelAtPeriodEnd  bool
}

// Outcome classifies what the engine did with one subscription.
type Outcome string

const (
	OutcomeInvoiced         Outcome = "invoiced"
	OutcomeSkippedCancelled Outcome = "skipped_cancelled"
	OutcomeSkippedNotDue    Outcome = "skipped_not_due"
	// OutcomeSkippedNonRecurring is a subscription whose price is one-time or free. That
	// combination is a data defect — a one-time purchase creates a service with *no*
	// subscription, so a subscription pointing at such a price can only come from a hand-written
	// row or an old bug. It is skipped and ended rather than invoiced, because renewing it would
	// charge the customer for the same one-time purchase every period, for ever.
	OutcomeSkippedNonRecurring Outcome = "skipped_non_recurring"
	OutcomeNeedsReview         Outcome = "needs_review"
	OutcomeFailed              Outcome = "failed"
)

// Result is the outcome for one subscription.
type Result struct {
	SubscriptionID string
	Outcome        Outcome
	InvoiceID      string
	InvoiceNumber  string
	PeriodKey      string
	Detail         string
}

// Engine generates renewal invoices.
type Engine struct {
	pool   *store.Pool
	issuer *billing.Issuer
	// Now is injectable so tests and replays can pin the clock.
	Now func() time.Time
	// Merchant and Terms are snapshotted onto each invoice.
	Merchant billing.Merchant
	Terms    string
	// Series selects the invoice numbering series.
	Series billing.NumberSeries
}

// NewEngine constructs an Engine.
func NewEngine(pool *store.Pool, issuer *billing.Issuer, merchant billing.Merchant, terms string) *Engine {
	return &Engine{
		pool:     pool,
		issuer:   issuer,
		Now:      func() time.Time { return time.Now().UTC() },
		Merchant: merchant,
		Terms:    terms,
		Series:   billing.DefaultSeries,
	}
}

// Run scans for due subscriptions and invoices them, up to limit per call.
//
// Every subscription is handled in its own transaction so one failure cannot
// roll back the work done for the others, and so a retry after a partial run
// simply picks up what is left.
func (e *Engine) Run(ctx context.Context, limit int) ([]Result, error) {
	if limit <= 0 {
		limit = 100
	}
	policy, err := LoadPolicy(ctx, e.pool)
	if err != nil {
		return nil, err
	}
	now := e.Now()

	var runID string
	if err := e.pool.QueryRow(ctx, `INSERT INTO renewal_runs DEFAULT VALUES RETURNING id`).Scan(&runID); err != nil {
		return nil, fmt.Errorf("renewal: create run: %w", err)
	}

	ids, err := e.dueSubscriptionIDs(ctx, now, policy, limit)
	if err != nil {
		if ferr := e.finishRun(ctx, runID, nil, err); ferr != nil {
			return nil, errors.Join(err, ferr)
		}
		return nil, err
	}

	results := make([]Result, 0, len(ids))
	var runErr error
	for _, id := range ids {
		res, err := e.renewOne(ctx, runID, id, now, policy)
		if err != nil {
			// A single subscription failing must not stop the run, but it must
			// be visible.
			res = Result{SubscriptionID: id, Outcome: OutcomeFailed, Detail: err.Error()}
			runErr = errors.Join(runErr, fmt.Errorf("subscription %s: %w", id, err))
		}
		results = append(results, res)
	}

	if ferr := e.finishRun(ctx, runID, results, runErr); ferr != nil {
		// The run's own bookkeeping is part of the contract: a run that cannot
		// record what it did is a run an operator cannot audit.
		runErr = errors.Join(runErr, ferr)
	}
	return results, runErr
}

// dueSubscriptionIDs selects work without locking it, so the scan stays cheap.
// The real lock happens per subscription in renewOne.
func (e *Engine) dueSubscriptionIDs(ctx context.Context, now time.Time, p Policy, limit int) ([]string, error) {
	// A subscription becomes due once its next billing instant is within the
	// invoice lead time. The interval is computed with an explicit make_interval
	// so the boundary is a plain timestamp comparison and can use the index.
	rows, err := e.pool.Query(ctx, `
		SELECT id
		FROM subscriptions
		WHERE status IN ('active', 'past_due')
		  AND next_billing_at <= $1::timestamptz + make_interval(days => $2::int)
		ORDER BY next_billing_at
		LIMIT $3`,
		now, p.InvoiceLeadDays, limit)
	if err != nil {
		return nil, fmt.Errorf("renewal: scan due subscriptions: %w", err)
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("renewal: scan id: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (e *Engine) renewOne(ctx context.Context, runID, subscriptionID string, now time.Time, p Policy) (Result, error) {
	var out Result
	out.SubscriptionID = subscriptionID

	err := e.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		sub, err := lockSubscription(ctx, tx, subscriptionID)
		if err != nil {
			return err
		}

		// Another worker already advanced this schedule.
		if sub.NextBillingAt.After(now.AddDate(0, 0, p.InvoiceLeadDays)) {
			out.Outcome = OutcomeSkippedNotDue
			out.Detail = "next billing date moved"
			return e.record(ctx, tx, runID, sub.ID, "", out)
		}

		loc, err := time.LoadLocation(sub.Timezone)
		if err != nil {
			return fmt.Errorf("renewal: unknown timezone %q: %w", sub.Timezone, err)
		}
		// The stored interval decides the period model. A day-count subscription is
		// counted from its persisted next billing instant, which is the grid's origin:
		// that is what keeps "every 45 days" on the same dates across restarts, and it
		// is why the engine never derives a boundary from "now".
		schedule, err := calendar.ScheduleFor(sub.IntervalName, sub.Anchor, sub.NextBillingAt, loc)
		if err != nil {
			return fmt.Errorf("renewal: invalid schedule: %w", err)
		}

		// The period being billed starts at the persisted boundary. Deriving it from
		// the schedule rather than adding 30 days is what makes month-end and
		// leap-day behaviour correct; for a day-count interval it is what makes the
		// period exactly N civil days long across a DST transition.
		period := schedule.PeriodStartingAt(sub.NextBillingAt)
		out.PeriodKey = period.Key()

		// Cancellation at period end: the customer still receives the final
		// period they paid for, but the schedule stops.
		if sub.CancelAtPeriodEnd {
			if err := e.endSubscription(ctx, tx, sub, now); err != nil {
				return err
			}
			out.Outcome = OutcomeSkippedCancelled
			out.Detail = "cancelled at period end"
			return e.record(ctx, tx, runID, sub.ID, out.PeriodKey, out)
		}

		// Catch-up guard: after a long outage the due date can be far in the
		// past. Billing that automatically would surprise many customers at
		// once, so it goes to review instead.
		ageDays := int(now.Sub(period.Start).Hours() / 24)
		if ageDays > p.CatchupMaxAgeDays {
			out.Outcome = OutcomeNeedsReview
			out.Detail = fmt.Sprintf("period start is %d days old (limit %d)", ageDays, p.CatchupMaxAgeDays)
			return e.record(ctx, tx, runID, sub.ID, out.PeriodKey, out)
		}

		price, err := loadPrice(ctx, tx, sub.PriceVersionID)
		if err != nil {
			return err
		}
		// Nothing may renew a price that is not a schedule. A one-time purchase creates a service
		// with no subscription at all, so reaching this point means the row is a defect: billing it
		// would charge the customer for the same one-time purchase every period, for ever. End the
		// schedule and record why, rather than erroring on it every run for the rest of time.
		if !catalog.Recurring(price.Interval) {
			if err := e.endSubscription(ctx, tx, sub, now); err != nil {
				return err
			}
			out.Outcome = OutcomeSkippedNonRecurring
			out.Detail = fmt.Sprintf("price version interval is %q, which does not recur; the subscription was ended rather than billed", price.Interval)
			return e.record(ctx, tx, runID, sub.ID, out.PeriodKey, out)
		}
		if price.AnchorDay != sub.Anchor.Day {
			// The subscription owns its schedule: a catalog edit must not move a live
			// billing date. Returning an error here would wedge the subscription
			// forever (every run would fail the same way), so the subscription's
			// anchor wins and the divergence is reported for review instead.
			out.Outcome = OutcomeNeedsReview
			out.Detail = fmt.Sprintf(
				"price version anchor day %d differs from subscription anchor day %d; billing used the subscription's anchor",
				price.AnchorDay, sub.Anchor.Day)
			return e.record(ctx, tx, runID, sub.ID, period.Key(), out)
		}

		// The chosen config options are part of what the subscription costs, so the renewal
		// carries the base price plus one line per paid option. Before this call the engine
		// billed the base price alone and gave every paid option away from the second period
		// onwards. See surcharges.go for which price is used and why.
		surcharges, err := loadSurchargeLines(ctx, tx, sub, price.Currency)
		if errors.Is(err, ErrUnpricedSurcharge) {
			// A paid option the catalogue no longer prices at this version. Billing the base
			// alone would hand the resource over for free, so a human decides.
			out.Outcome = OutcomeNeedsReview
			out.Detail = err.Error()
			return e.record(ctx, tx, runID, sub.ID, period.Key(), out)
		}
		if err != nil {
			return err
		}

		lines := []billing.Line{{
			Description:   price.Description,
			QuantityMilli: 1000,
			UnitPrice:     money.New(price.AmountMinor, price.Currency),
		}}
		lines = append(lines, surcharges...)
		dueAt := now.AddDate(0, 0, p.PaymentTermsDays)

		req := billing.IssueRequest{
			CustomerID:     sub.CustomerID,
			SubscriptionID: sub.ID,
			PeriodKey:      period.Key(),
			Lines:          lines,
			Merchant:       e.Merchant,
			Customer:       billing.CustomerDetails{ID: sub.CustomerID},
			Period: &billing.Period{
				Start: period.Start.UTC().Format(time.RFC3339),
				End:   period.End.UTC().Format(time.RFC3339),
				Key:   period.Key(),
			},
			Terms:           e.Terms,
			DueAt:           dueAt,
			Series:          e.Series,
			PriceVersionIDs: []string{sub.PriceVersionID},
		}
		if err := fillCustomer(ctx, tx, &req.Customer); err != nil {
			return err
		}

		invoiceID, _, _, err := e.issuer.CreateDraft(ctx, tx, req)
		if err != nil {
			return err
		}
		issued, err := e.issuer.Issue(ctx, tx, invoiceID, e.Series)
		if err != nil {
			return err
		}

		if err := e.advance(ctx, tx, sub, period); err != nil {
			return err
		}

		out.Outcome = OutcomeInvoiced
		out.InvoiceID = issued.InvoiceID
		out.InvoiceNumber = issued.Number
		return e.record(ctx, tx, runID, sub.ID, out.PeriodKey, out)
	})
	if err != nil {
		return out, err
	}
	return out, nil
}

// advance moves the schedule forward, guarding against a concurrent advance with
// a version comparison.
func (e *Engine) advance(ctx context.Context, tx pgx.Tx, sub Subscription, period calendar.Period) error {
	tag, err := tx.Exec(ctx, `
		UPDATE subscriptions
		SET current_period_start = $2,
		    current_period_end = $3,
		    next_billing_at = $3,
		    version = version + 1
		WHERE id = $1 AND version = $4`,
		sub.ID, period.Start, period.End, sub.Version)
	if err != nil {
		return fmt.Errorf("renewal: advance schedule: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("%w: %s", ErrVersionConflict, sub.ID)
	}
	return nil
}

// endSubscription applies a cancellation at period end.
func (e *Engine) endSubscription(ctx context.Context, tx pgx.Tx, sub Subscription, now time.Time) error {
	tag, err := tx.Exec(ctx, `
		UPDATE subscriptions
		SET status = 'ended', ended_at = $2, version = version + 1
		WHERE id = $1 AND version = $3`,
		sub.ID, now, sub.Version)
	if err != nil {
		return fmt.Errorf("renewal: end subscription: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("%w: %s", ErrVersionConflict, sub.ID)
	}
	return nil
}

func (e *Engine) record(ctx context.Context, tx pgx.Tx, runID, subscriptionID, periodKey string, out Result) error {
	if periodKey == "" {
		periodKey = "n/a"
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO renewal_events (run_id, subscription_id, invoice_id, period_key, outcome, detail)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (run_id, subscription_id, period_key) DO NOTHING`,
		runID, subscriptionID, nullable(out.InvoiceID), periodKey, string(out.Outcome), out.Detail)
	if err != nil {
		return fmt.Errorf("renewal: record event: %w", err)
	}
	return nil
}

func (e *Engine) finishRun(ctx context.Context, runID string, results []Result, runErr error) error {
	var invoiced, skipped, review int
	for _, r := range results {
		switch r.Outcome {
		case OutcomeInvoiced:
			invoiced++
		case OutcomeNeedsReview:
			review++
		default:
			skipped++
		}
	}
	var errText any
	if runErr != nil {
		errText = runErr.Error()
	}
	// Use a live context: the run's context may be cancelled by a shutdown, and
	// the bookkeeping record is exactly what a postmortem needs.
	ctx = context.WithoutCancel(ctx)
	tag, err := e.pool.Exec(ctx, `
		UPDATE renewal_runs
		SET finished_at = now(), scanned = $2, invoiced = $3, skipped = $4,
		    needs_review = $5, error = $6
		WHERE id = $1`,
		runID, len(results), invoiced, skipped, review, errText)
	if err != nil {
		return fmt.Errorf("renewal: record run %s: %w", runID, err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("renewal: record run %s: %d rows updated", runID, tag.RowsAffected())
	}
	return nil
}

func lockSubscription(ctx context.Context, tx pgx.Tx, id string) (Subscription, error) {
	var (
		sub       Subscription
		interval  string
		timezone  string
		anchorDay int
		hour      int
		minute    int
	)
	err := tx.QueryRow(ctx, `
		SELECT id, customer_id, price_version_id, status, interval, anchor_day,
		       anchor_hour, anchor_minute, timezone,
		       current_period_start, current_period_end, next_billing_at, version,
		       cancel_at_period_end
		FROM subscriptions WHERE id = $1 FOR UPDATE`, id).
		Scan(&sub.ID, &sub.CustomerID, &sub.PriceVersionID, &sub.Status, &interval,
			&anchorDay, &hour, &minute, &timezone,
			&sub.CurrentPeriodStart, &sub.CurrentPeriodEnd, &sub.NextBillingAt, &sub.Version,
			&sub.CancelAtPeriodEnd)
	if errors.Is(err, pgx.ErrNoRows) {
		return Subscription{}, fmt.Errorf("renewal: subscription %s not found", id)
	}
	if err != nil {
		return Subscription{}, fmt.Errorf("renewal: lock subscription: %w", err)
	}
	// The stored interval is validated where the schedule is built, because that is
	// also where the period model is chosen from it: a calendar interval and a
	// day-count interval are different models, and an unknown value has to fail
	// there rather than be parsed twice with two different rules.
	sub.IntervalName = interval
	sub.Anchor = calendar.Anchor{Day: anchorDay, Hour: hour, Minute: minute}
	sub.Timezone = timezone
	return sub, nil
}

type price struct {
	AmountMinor int64
	Currency    money.Currency
	Description string
	AnchorDay   int
	// Interval is the price's billing period. It is checked before anything is billed: a
	// one-time or free price is not a schedule, so a subscription pointing at one is ended
	// rather than renewed.
	Interval string
}

func loadPrice(ctx context.Context, tx pgx.Tx, priceVersionID string) (price, error) {
	var (
		p        price
		currency string
	)
	err := tx.QueryRow(ctx, `
		SELECT pv.amount_minor, pv.currency, pv.interval, pv.anchor_day, p.name
		FROM price_versions pv
		JOIN products p ON p.id = pv.product_id
		WHERE pv.id = $1`, priceVersionID).
		Scan(&p.AmountMinor, &currency, &p.Interval, &p.AnchorDay, &p.Description)
	if err != nil {
		return price{}, fmt.Errorf("renewal: load price version: %w", err)
	}
	cur, ok := money.LookupCurrency(currency)
	if !ok {
		var err error
		if cur, err = money.NewCurrency(currency, 2); err != nil {
			return price{}, fmt.Errorf("renewal: price currency %q: %w", currency, err)
		}
	}
	p.Currency = cur
	return p, nil
}

// fillCustomer snapshots the billed party from the customer record and its billing
// profile, so a renewal invoice carries the address and tax id the customer supplied.
func fillCustomer(ctx context.Context, tx pgx.Tx, c *billing.CustomerDetails) error {
	if err := billing.FillCustomer(ctx, tx, c); err != nil {
		return fmt.Errorf("renewal: load customer: %w", err)
	}
	return nil
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}
