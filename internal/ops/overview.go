// Package ops answers the questions an operator actually asks at the start of a
// day: what is broken, what needs a decision, and what did the system do while
// nobody was watching.
//
// It is deliberately read-only and deliberately narrow. The overview is not a
// wall of statistics; it is a list of exceptions that each have a defined
// recovery path, ordered by how much money or trust they cost if ignored. A
// number that nobody can act on is not on this page.
package ops

import (
	"context"
	"fmt"
	"time"

	"github.com/catalyst/billing/internal/ledger"
	"github.com/catalyst/billing/internal/money"
	"github.com/catalyst/billing/internal/store"
)

// Severity orders exceptions by what happens if they are ignored.
type Severity string

const (
	// SeverityCritical means money or service state is already wrong.
	SeverityCritical Severity = "critical"
	// SeverityWarning means a customer is affected and it will get worse.
	SeverityWarning Severity = "warning"
	// SeverityInfo is worth knowing but needs no action today.
	SeverityInfo Severity = "info"
)

// Exception is one thing that needs attention, with a link to the records and a
// statement of what to do about it.
type Exception struct {
	// Code is a stable identifier, safe to alert on.
	Code string `json:"code"`
	// Title is a short human sentence.
	Title string `json:"title"`
	// Detail explains the cause in the operator's terms.
	Detail string `json:"detail"`
	// Count is how many records are affected.
	Count int `json:"count"`
	// Severity orders the list.
	Severity Severity `json:"severity"`
	// Money is the amount at stake, when the exception concerns money.
	Money *string `json:"money,omitempty"`
	// OldestAt is when the oldest affected record appeared, so an operator can
	// tell a fresh blip from a long-running leak.
	OldestAt *time.Time `json:"oldest_at,omitempty"`
	// Action is what the operator should do, or why no action is possible.
	Action string `json:"action"`
}

// Overview is the operator dashboard payload.
type Overview struct {
	GeneratedAt time.Time   `json:"generated_at"`
	Exceptions  []Exception `json:"exceptions"`
	// Counts are the plain totals that belong in a header, not in an alert.
	Counts Counts `json:"counts"`
}

// Counts are the headline numbers.
type Counts struct {
	Customers        int `json:"customers"`
	Services         int `json:"services"`
	OpenInvoices     int `json:"open_invoices"`
	DueSubscriptions int `json:"due_subscriptions"`
}

// Service reports the overview.
type Service struct {
	pool *store.Pool
	// Now is injectable for tests.
	Now func() time.Time
}

// New constructs a Service.
func New(pool *store.Pool) *Service {
	return &Service{pool: pool, Now: func() time.Time { return time.Now().UTC() }}
}

// Overview builds the operator dashboard.
//
// Each check is independent: one failing query reports itself as an exception
// rather than blanking the whole page, because an operator looking at a broken
// system needs to see the parts that still work.
func (s *Service) Overview(ctx context.Context) Overview {
	out := Overview{GeneratedAt: s.Now()}

	// Order matters: the list is presented in this order, most costly first.
	checks := []func(context.Context) Exception{
		// Paid but not provisioned: the customer has paid and has nothing. This
		// is the worst customer-facing state in the product.
		s.paidNotProvisioned,
		// Remote outcomes that could not be established. These block a customer
		// and must never be retried blindly.
		s.unknownOperations,
		// Invoices that are open and past due.
		s.overdueInvoices,
		// Webhook events pending long enough that a worker is probably not
		// running.
		s.stalledWebhooks,
		// Operations that exhausted their retry budget.
		s.exhaustedOperations,
		// Renewals held back by the catch-up safety guard.
		s.renewalsNeedingReview,
		// Suspensions that were withheld by a hold or a policy rule are not
		// failures, but an operator should know they happened: a customer kept
		// service without paying, on purpose.
		s.withheldSuspensions,
		// The subledger must always balance. A failure here is a bug, not a
		// queue.
		s.ledgerImbalance,
	}

	// A check that found nothing returns the zero Exception. Dropping it here
	// rather than in the handler keeps the JSON contract clean: every entry in
	// the list has a code, a severity and an action.
	for _, check := range checks {
		if exc := check(ctx); exc.Code != "" {
			out.Exceptions = append(out.Exceptions, exc)
		}
	}

	out.Counts = s.counts(ctx)
	return out
}

// paidNotProvisioned finds invoices that are paid while their order still has no
// service in a working state.
func (s *Service) paidNotProvisioned(ctx context.Context) Exception {
	exc := Exception{
		Code:     "paid_not_provisioned",
		Title:    "Paid, but the service is not running",
		Severity: SeverityCritical,
		Action:   "Inspect the service operation; if it is stuck, requeue it or provision manually and record the external id.",
	}
	var count int
	var oldest *time.Time
	// The check is per *order*, not per customer: a customer with one working
	// service and a second paid order that was never delivered is broken, and a
	// customer-wide check would report them as fine.
	err := s.pool.QueryRow(ctx, `
		SELECT count(*), min(i.issued_at)
		FROM invoices i
		JOIN orders o ON o.id = i.order_id
		WHERE i.status = 'paid'
		  AND NOT EXISTS (
		      SELECT 1
		      FROM order_items oi
		      JOIN services s ON s.product_id = (
		          SELECT pv.product_id FROM price_versions pv WHERE pv.id = oi.price_version_id
		      )
		                     AND s.customer_id = i.customer_id
		                     AND s.status IN ('active', 'suspended')
		      WHERE oi.order_id = o.id
		  )`).Scan(&count, &oldest)
	if err != nil {
		return queryFailure(exc, err)
	}
	if count == 0 {
		return Exception{}
	}
	exc.Count = count
	exc.OldestAt = oldest
	exc.Detail = "The invoice is settled and no active service exists for what the order bought."
	return exc
}

// unknownOperations lists provisioning operations whose remote effect could not
// be established.
func (s *Service) unknownOperations(ctx context.Context) Exception {
	exc := Exception{
		Code:     "unknown_operations",
		Title:    "Remote outcome unknown",
		Severity: SeverityCritical,
		Action:   "Reconcile against the provider, then mark the operation succeeded or failed. Do not retry blindly.",
	}
	var count int
	var oldest *time.Time
	err := s.pool.QueryRow(ctx, `
		SELECT count(*), min(created_at) FROM service_operations
		WHERE status IN ('unknown', 'needs_review')`).Scan(&count, &oldest)
	if err != nil {
		return queryFailure(exc, err)
	}
	if count == 0 {
		return Exception{}
	}
	exc.Count = count
	exc.OldestAt = oldest
	exc.Detail = "A provisioning call did not produce a trustworthy result, so the true state is not known."
	return exc
}

// overdueInvoices lists open invoices past their due date.
func (s *Service) overdueInvoices(ctx context.Context) Exception {
	exc := Exception{
		Code:     "overdue_invoices",
		Title:    "Overdue invoices",
		Severity: SeverityWarning,
		Action:   "Chase payment or let dunning run; suspension follows the configured grace period.",
	}
	var count int
	var oldest *time.Time
	var minor int64
	var currency *string
	err := s.pool.QueryRow(ctx, `
		SELECT count(*), min(due_at), COALESCE(SUM(total_minor - amount_paid_minor), 0), min(currency)
		FROM invoices
		WHERE status = 'open' AND due_at < $1`, s.Now()).Scan(&count, &oldest, &minor, &currency)
	if err != nil {
		return queryFailure(exc, err)
	}
	if count == 0 {
		return Exception{}
	}
	exc.Count = count
	exc.OldestAt = oldest
	exc.Detail = "These invoices are past due and still open."
	if currency != nil {
		if amt, err := amountOf(minor, *currency); err == nil {
			s := amt.Format()
			exc.Money = &s
		}
	}
	return exc
}

// stalledWebhooks lists events that have not been processed.
func (s *Service) stalledWebhooks(ctx context.Context) Exception {
	exc := Exception{
		Code:     "stalled_webhooks",
		Title:    "Webhook events are not being processed",
		Severity: SeverityWarning,
		Action:   "Check that the worker is running and that the gateway extension starts; the events are safely stored.",
	}
	var count int
	var oldest *time.Time
	err := s.pool.QueryRow(ctx, `
		SELECT count(*), min(received_at) FROM gateway_events
		WHERE processed_at IS NULL AND received_at < $1`,
		s.Now().Add(-10*time.Minute)).Scan(&count, &oldest)
	if err != nil {
		return queryFailure(exc, err)
	}
	if count == 0 {
		return Exception{}
	}
	exc.Count = count
	exc.OldestAt = oldest
	exc.Detail = "Events received more than ten minutes ago are still unprocessed."
	return exc
}

// exhaustedOperations lists operations that gave up.
func (s *Service) exhaustedOperations(ctx context.Context) Exception {
	exc := Exception{
		Code:     "failed_operations",
		Title:    "Provisioning gave up",
		Severity: SeverityWarning,
		Action:   "Read the last error, fix the cause, then requeue with the same operation key.",
	}
	var count int
	var oldest *time.Time
	err := s.pool.QueryRow(ctx, `
		SELECT count(*), min(created_at) FROM service_operations WHERE status = 'failed'`).
		Scan(&count, &oldest)
	if err != nil {
		return queryFailure(exc, err)
	}
	if count == 0 {
		return Exception{}
	}
	exc.Count = count
	exc.OldestAt = oldest
	exc.Detail = "These operations exhausted their retry budget."
	return exc
}

// renewalsNeedingReview lists catch-up work held back by the safety guard.
func (s *Service) renewalsNeedingReview(ctx context.Context) Exception {
	exc := Exception{
		Code:     "renewals_needing_review",
		Title:    "Renewals held for review",
		Severity: SeverityInfo,
		Action:   "Review the affected subscriptions before billing: the backlog is larger or older than the automatic limit.",
	}
	var count int
	var oldest *time.Time
	err := s.pool.QueryRow(ctx, `
		SELECT count(*), min(created_at) FROM renewal_events WHERE outcome = 'needs_review'`).
		Scan(&count, &oldest)
	if err != nil {
		return queryFailure(exc, err)
	}
	if count == 0 {
		return Exception{}
	}
	exc.Count = count
	exc.OldestAt = oldest
	exc.Detail = "Catch-up billing was withheld rather than charging many customers automatically."
	return exc
}

// withheldSuspensions lists services that automation decided not to suspend.
func (s *Service) withheldSuspensions(ctx context.Context) Exception {
	exc := Exception{
		Code:     "suspensions_withheld",
		Title:    "Suspension withheld",
		Severity: SeverityInfo,
		Action:   "Check the reason: a manual hold, a fresh payment or a policy limit. Clear the hold if service should stop.",
	}
	var count int
	var oldest *time.Time
	err := s.pool.QueryRow(ctx, `
		SELECT count(*), min(created_at)
		FROM suspension_decisions
		WHERE decision = 'held' AND created_at > $1`,
		s.Now().AddDate(0, 0, -7)).Scan(&count, &oldest)
	if err != nil {
		return queryFailure(exc, err)
	}
	if count == 0 {
		return Exception{}
	}
	exc.Count = count
	exc.OldestAt = oldest
	exc.Detail = "In the last seven days, automation declined to suspend a service for non-payment."
	return exc
}

// ledgerImbalance asserts the subledger sums to zero per currency.
func (s *Service) ledgerImbalance(ctx context.Context) Exception {
	exc := Exception{
		Code:     "ledger_imbalance",
		Title:    "The subledger does not balance",
		Severity: SeverityCritical,
		Action:   "Stop and investigate: this is a defect, not a queue. Every posting is append-only, so the cause is findable.",
	}
	balances, err := ledger.TrialBalance(ctx, s.pool)
	if err != nil {
		return queryFailure(exc, err)
	}
	var bad []string
	for code, net := range balances {
		if !net.IsZero() {
			bad = append(bad, fmt.Sprintf("%s off by %s", code, net.Format()))
		}
	}
	if len(bad) == 0 {
		return Exception{}
	}
	exc.Count = len(bad)
	exc.Detail = fmt.Sprintf("%v", bad)
	return exc
}

func (s *Service) counts(ctx context.Context) Counts {
	var c Counts
	_ = s.pool.QueryRow(ctx, `SELECT count(*) FROM customers`).Scan(&c.Customers)
	_ = s.pool.QueryRow(ctx, `SELECT count(*) FROM services WHERE status <> 'terminated'`).Scan(&c.Services)
	_ = s.pool.QueryRow(ctx, `SELECT count(*) FROM invoices WHERE status = 'open'`).Scan(&c.OpenInvoices)
	_ = s.pool.QueryRow(ctx, `
		SELECT count(*) FROM subscriptions
		WHERE status IN ('active','past_due') AND next_billing_at <= $1`,
		s.Now().AddDate(0, 0, 7)).Scan(&c.DueSubscriptions)
	return c
}

// queryFailure converts a broken check into a visible exception instead of
// hiding it: an operator must know that a check did not run.
func queryFailure(exc Exception, err error) Exception {
	exc.Severity = SeverityWarning
	exc.Count = 0
	exc.Detail = "This check could not run: " + err.Error()
	return exc
}

func amountOf(minor int64, code string) (money.Amount, error) {
	cur, ok := money.LookupCurrency(code)
	if !ok {
		var err error
		if cur, err = money.NewCurrency(code, 2); err != nil {
			return money.Amount{}, err
		}
	}
	return money.New(minor, cur), nil
}
