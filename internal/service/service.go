// Package service implements the lifecycle of a service after it has been sold:
// cancellation requests, upgrades and downgrades, configurable options, and the stock
// and per-customer limits that guard a purchase.
//
// Three ideas shape it, and each is recorded because the next person will otherwise
// "simplify" it back into the defect it prevents.
//
//  1. **A cancellation is a request row, not a boolean.** `subscriptions.cancel_at_period_end`
//     says the schedule stops; it cannot say who asked, why, or whether an operator ever
//     looked at it. `service_cancellations` can, and an immediate cancellation is modelled as
//     a *pending request* that staff or a cron executes rather than a switch a customer can
//     throw. Immediate cancellation destroys time the customer has already paid for; that is
//     not a self-service action, and the audit trail Paymenter lacks is the by-product.
//
//  2. **Proration is exact.** A prorated difference is `(to - from) * remaining`, computed
//     with big.Rat and rounded exactly once, with the rounding mode named at the call site.
//     There is no float anywhere, and no "30 days per month" assumption: the period length
//     comes from internal/calendar, so a month-end period is 28 days when it is 28 days.
//
//  3. **An upgrade is a priced change with a paper trail.** `service_upgrades` records what
//     the service moved from, what it moved to, what the customer was charged or credited,
//     and which invoice carries it. A half-applied upgrade — invoice issued, server not yet
//     changed — is therefore visible and recoverable rather than an inference from two
//     tables that disagree.
//
// The package owns its transactions, like internal/creditnote: a caller cannot issue the
// upgrade invoice without the row that explains it, and cannot enqueue the termination
// without recording that the cancellation was executed.
package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/catalyst/billing/internal/audit"
	"github.com/catalyst/billing/internal/billing"
	"github.com/catalyst/billing/internal/calendar"
	"github.com/catalyst/billing/internal/money"
	"github.com/catalyst/billing/internal/store"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Errors callers branch on. They are values rather than strings so a PostgreSQL message
// change cannot alter control flow.
var (
	// ErrNotFound is returned for a record that does not exist *or* belongs to another
	// customer. The two are deliberately indistinguishable.
	ErrNotFound = errors.New("service: not found")
	// ErrInvalidInput is returned for a request the domain refuses on its merits.
	ErrInvalidInput = errors.New("service: invalid input")
	// ErrNotCancellable is returned when the service's state does not allow a cancellation
	// request (it is terminated, or has no billing schedule to stop).
	ErrNotCancellable = errors.New("service: this service cannot be cancelled")
	// ErrAlreadyRequested is returned when an open cancellation request already exists.
	// Answering with the existing request rather than an error would hide a double click
	// from the customer, but it would also hide a second, different reason.
	ErrAlreadyRequested = errors.New("service: a cancellation request is already open")
	// ErrNotPending is returned when a record is not in the state the action requires.
	ErrNotPending = errors.New("service: the record is not in a pending state")
	// ErrOutOfStock is returned when a product has fewer units left than requested.
	ErrOutOfStock = errors.New("service: the product is out of stock")
	// ErrPerUserLimit is returned when a customer already holds the maximum number of a
	// product.
	ErrPerUserLimit = errors.New("service: the per-customer limit for this product is reached")
	// ErrNotUpgradable is returned when the target price is not reachable from the
	// service's product in the upgrade graph.
	ErrNotUpgradable = errors.New("service: that product is not an upgrade target for this service")
	// ErrCurrencyMismatch is returned when a price is quoted in a different currency from
	// the subscription. There is no exchange rate in this system, so this cannot be
	// converted away.
	ErrCurrencyMismatch = errors.New("service: the currencies differ")
	// ErrIntervalMismatch is returned when the target price bills on a different cycle.
	// A prorated difference between a monthly and an annual price is not a number that
	// means anything.
	ErrIntervalMismatch = errors.New("service: the billing periods differ")
	// ErrOptionNotAvailable is returned for a selection naming an option the product does
	// not offer, or a value the option does not offer.
	ErrOptionNotAvailable = errors.New("service: the option selection is not available for this product")
	// ErrOptionNotUpgradable is returned when a live service's option may not be changed.
	ErrOptionNotUpgradable = errors.New("service: that option cannot be changed on a live service")
	// ErrNoInvoice is returned when an upgrade row that should carry an invoice does not.
	ErrNoInvoice = errors.New("service: the upgrade has no invoice")
	// ErrCreditNotPosted is returned when a downgrade is applied before its credit has been
	// posted. Applying it early would give the customer the cheaper price and keep the money
	// they are owed.
	ErrCreditNotPosted = errors.New("service: the downgrade credit has not been posted")
)

// Cancellation types. The set matches the database CHECK in 0020_commerce.sql.
const (
	// CancelImmediate asks for the service to be destroyed now. It is recorded as a
	// pending request and executed by staff or the due-work cron.
	CancelImmediate = "immediate"
	// CancelEndOfPeriod stops the schedule at the paid-through date and keeps the service
	// running until then.
	CancelEndOfPeriod = "end_of_period"
)

// Cancellation statuses. The set matches the database CHECK.
const (
	StatusPending   = "pending"
	StatusScheduled = "scheduled"
	StatusExecuted  = "executed"
	StatusCancelled = "cancelled"
	StatusRejected  = "rejected"
)

// Upgrade statuses. The set matches the database CHECK on service_upgrades.
const (
	UpgradePending   = "pending"
	UpgradeApplied   = "applied"
	UpgradeCancelled = "cancelled"
	UpgradeFailed    = "failed"
)

// Querier is the subset of pgx that both a pool and a transaction satisfy, so the stock
// helpers can be called from inside a checkout transaction or from a job.
type Querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// Store is the service-lifecycle data access.
//
// The issuer and merchant fields are the document defaults an upgrade invoice is written
// with. They are fields rather than parameters so a caller constructs one Store per process
// and every upgrade invoice is numbered by the same series.
type Store struct {
	pool *store.Pool
	// Now is injectable for tests. Every write stamps its own time rather than trusting the
	// database clock, so a test can move time without touching the server.
	Now func() time.Time
	// Issuer creates and issues the prorated upgrade invoice.
	Issuer *billing.Issuer
	// Merchant, Terms and Series are the document defaults for that invoice.
	Merchant billing.Merchant
	Terms    string
	Series   billing.NumberSeries
	// DueDays is how long an upgrade invoice has before it is overdue.
	DueDays int
	// auditor, when set, writes the audit row inside the transaction that made the change,
	// with the caller's actor context (request id, source address).
	auditor audit.Auditor
}

// New constructs a Store.
func New(pool *store.Pool) *Store {
	return &Store{
		pool:    pool,
		Now:     func() time.Time { return time.Now().UTC() },
		DueDays: 7,
		Series:  billing.DefaultSeries,
	}
}

// WithAuditor returns a copy of the Store that writes its audit rows through a.
func (s *Store) WithAuditor(a audit.Auditor) *Store {
	clone := *s
	clone.auditor = a
	return &clone
}

func (s *Store) clock() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func (s *Store) dueDays() int {
	if s.DueDays <= 0 {
		return 7
	}
	return s.DueDays
}

// audit records one action, through the request auditor when there is one.
//
// An empty actor is the system: the due-work cron has no session, and recording it as a
// user would make the trail lie about who acted.
func (s *Store) audit(ctx context.Context, tx pgx.Tx, action, targetType, targetID, actorID string,
	changes map[string]any) error {
	if s.auditor != nil {
		return s.auditor.RecordAction(ctx, tx, action, targetType, targetID, changes)
	}
	actor := audit.Actor{Kind: audit.ActorSystem}
	if actorID != "" {
		actor = audit.Actor{ID: actorID, Kind: audit.ActorUser}
	}
	return audit.Record(ctx, tx, audit.Event{
		Actor:      actor,
		Action:     action,
		TargetType: targetType,
		TargetID:   targetID,
		Outcome:    audit.OutcomeSuccess,
		Changes:    changes,
	})
}

// enqueueOperation inserts a provisioning operation, or returns the one already queued
// under the same key.
//
// The key format is the contract the worker and the staff terminate handler already use —
// "terminate:<service>:<generation>" — which is what makes a staff termination and an
// executed cancellation the *same* operation rather than two calls to the adapter. That is
// why this helper reuses the ON CONFLICT ... DO UPDATE ... RETURNING idiom from
// internal/httpapi/staff_write.go rather than inserting a row and hoping.
func enqueueOperation(ctx context.Context, tx pgx.Tx, serviceID, kind, key string, generation int64) (string, error) {
	var id string
	err := tx.QueryRow(ctx, `
		INSERT INTO service_operations (service_id, kind, operation_key, generation, status)
		VALUES ($1, $2, $3, $4, 'queued')
		ON CONFLICT (operation_key) DO UPDATE SET operation_key = EXCLUDED.operation_key
		RETURNING id`, serviceID, kind, key, generation).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("service: enqueue %s operation: %w", kind, err)
	}
	return id, nil
}

// priceVersion is one immutable catalog price.
type priceVersion struct {
	ID              string
	ProductID       string
	Version         int
	Name            string
	Currency        string
	AmountMinor     int64
	SetupMinor      int64
	Interval        string
	ProvisionPlan   string
	ProvisionRegion string
}

func loadPriceVersion(ctx context.Context, q Querier, id string) (priceVersion, error) {
	var p priceVersion
	err := q.QueryRow(ctx, `
		SELECT id, product_id, version, name, currency, amount_minor, setup_minor, interval,
		       COALESCE(provision_plan, ''), COALESCE(provision_region, '')
		FROM price_versions WHERE id = $1`, id).
		Scan(&p.ID, &p.ProductID, &p.Version, &p.Name, &p.Currency, &p.AmountMinor,
			&p.SetupMinor, &p.Interval, &p.ProvisionPlan, &p.ProvisionRegion)
	if errors.Is(err, pgx.ErrNoRows) {
		return priceVersion{}, ErrNotFound
	}
	if err != nil {
		return priceVersion{}, fmt.Errorf("service: load price version: %w", err)
	}
	return p, nil
}

// subscription is the billing schedule a service runs on.
type subscription struct {
	ID                 string
	CustomerID         string
	PriceVersionID     string
	Status             string
	IntervalName       string
	Interval           calendar.Interval
	Anchor             calendar.Anchor
	Timezone           string
	CurrentPeriodStart time.Time
	CurrentPeriodEnd   time.Time
	CancelAtPeriodEnd  bool
	Version            int64
}

func scanSubscription(row pgx.Row) (subscription, error) {
	var sub subscription
	var anchorDay, anchorHour, anchorMinute int
	err := row.Scan(&sub.ID, &sub.CustomerID, &sub.PriceVersionID, &sub.Status,
		&sub.IntervalName, &anchorDay, &anchorHour, &anchorMinute, &sub.Timezone,
		&sub.CurrentPeriodStart, &sub.CurrentPeriodEnd, &sub.CancelAtPeriodEnd, &sub.Version)
	if errors.Is(err, pgx.ErrNoRows) {
		return subscription{}, ErrNotFound
	}
	if err != nil {
		return subscription{}, fmt.Errorf("service: load subscription: %w", err)
	}
	// A day-count interval ("every 45 days") has no calendar Interval, so it is left
	// at the zero value rather than rejected: the schedule below is built from the
	// stored name, which is what decides which period model applies.
	if _, dayCount := calendar.ParseDayInterval(sub.IntervalName); !dayCount {
		interval, err := calendar.ParseInterval(sub.IntervalName)
		if err != nil {
			return subscription{}, fmt.Errorf("service: %w", err)
		}
		sub.Interval = interval
	}
	sub.Anchor = calendar.Anchor{Day: anchorDay, Hour: anchorHour, Minute: anchorMinute}
	return sub, nil
}

const subscriptionColumns = `
	id, customer_id, price_version_id, status, interval, anchor_day, anchor_hour, anchor_minute,
	timezone, current_period_start, current_period_end, cancel_at_period_end, version`

// billingPeriod returns the period a proration is computed against.
//
// The stored boundary is authoritative — it is the date the customer actually paid through —
// but the calendar is consulted first so a month-end or DST period is measured by the length
// it really has rather than by a remembered column that a timezone change could have made
// stale. When the two disagree the stored boundary wins, because the customer was billed on it.
//
// A day-count period is counted from that same stored boundary, which is its grid origin, so a
// mid-cycle upgrade on "every 45 days" prorates against the 45 days the customer paid for —
// not against a remembered start and end that a re-anchor could have moved.
func billingPeriod(sub subscription, now time.Time) (time.Time, time.Time) {
	loc, err := time.LoadLocation(sub.Timezone)
	if err == nil {
		if schedule, err := calendar.ScheduleFor(sub.IntervalName, sub.Anchor, sub.CurrentPeriodStart, loc); err == nil {
			if p := schedule.PeriodAt(now); p.Start.Equal(sub.CurrentPeriodStart) {
				return p.Start, p.End
			}
		}
	}
	return sub.CurrentPeriodStart, sub.CurrentPeriodEnd
}

// currencyOf resolves a stored currency code to a Currency.
func currencyOf(code string) (money.Currency, error) {
	if cur, ok := money.LookupCurrency(code); ok {
		return cur, nil
	}
	return money.NewCurrency(code, 2)
}
