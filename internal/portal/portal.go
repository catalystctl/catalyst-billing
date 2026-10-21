// Package portal implements the customer-facing view of billing.
//
// Every query here is scoped to the authenticated customer, and the scoping is done
// in SQL rather than by filtering afterwards. That distinction matters: a handler
// that fetches by identifier and then checks ownership is one forgotten check away
// from an IDOR, whereas a query that can only ever see its own customer's rows
// cannot leak them at all.
//
// The package exposes exactly one mutation — scheduling a cancellation — and it is
// reversible by nobody but an operator. Paying is a checkout, which is a separate
// flow with its own amount verification.
package portal

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/catalyst/billing/internal/ledger"
	"github.com/catalyst/billing/internal/money"
	"github.com/catalyst/billing/internal/store"
	"github.com/jackc/pgx/v5"
)

// ErrNotFound is returned when a record does not exist *or* belongs to someone
// else. The two are deliberately indistinguishable, so a probing request cannot
// learn that an identifier exists.
var ErrNotFound = errors.New("portal: not found")

// Invoice is a customer-visible invoice.
type Invoice struct {
	ID       string
	Number   string
	Status   string
	Currency string
	Total    money.Amount
	Paid     money.Amount
	// Due is what remains payable.
	Due      money.Amount
	IssuedAt *time.Time
	DueAt    *time.Time
	// Overdue is derived, not stored: an invoice becomes overdue by the passage of
	// time, not because a job happened to run.
	Overdue bool
	// Payable is true when the customer can start a checkout.
	Payable bool
}

// Service is a customer-visible service.
type Service struct {
	ID       string
	Name     string
	Status   string
	NextBill *time.Time
	// Actions are the operations the customer may request. Offering an action the
	// server would refuse is a broken promise, so this list is computed from the
	// same rules the action handlers enforce.
	Actions []string
}

// Customer is the billing identity.
type Customer struct {
	ID    string
	Name  string
	Email string
}

// Balance is what the customer owes and holds.
type Balance struct {
	// Outstanding is the total unpaid across open invoices, per currency. A
	// customer with invoices in two currencies sees two lines: summing them would
	// be meaningless without a conversion rate.
	Outstanding []money.Amount
	// Credit is money the customer has with us and has not spent.
	Credit []money.Amount
}

// Overview is the customer landing page: what do I owe, what is renewing, is my
// service ready.
type Overview struct {
	Customer       Customer
	Balance        Balance
	Upcoming       []Service
	RecentInvoices []Invoice
	OpenInvoices   []Invoice
}

// Manager reads customer-scoped data.
type Manager struct {
	pool *store.Pool
	// Now is injectable for tests.
	Now func() time.Time
}

// New constructs the portal manager.
func New(pool *store.Pool) *Manager {
	return &Manager{pool: pool, Now: func() time.Time { return time.Now().UTC() }}
}

// CustomerIDFor resolves the billing customer for a user.
//
// A user without a customer record is not an error the UI should present as broken:
// it is a staff member, or an account that has not bought anything yet.
func (m *Manager) CustomerIDFor(ctx context.Context, userID string) (string, error) {
	var id string
	err := m.pool.QueryRow(ctx, `
		SELECT customer_id FROM customer_accounts WHERE user_id = $1
		ORDER BY created_at LIMIT 1`, userID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("portal: resolve customer: %w", err)
	}
	return id, nil
}

// Overview builds the customer landing page.
func (m *Manager) Overview(ctx context.Context, userID string) (Overview, error) {
	customerID, err := m.CustomerIDFor(ctx, userID)
	if err != nil {
		return Overview{}, err
	}

	out := Overview{
		Customer:       Customer{ID: customerID},
		Upcoming:       []Service{},
		RecentInvoices: []Invoice{},
		OpenInvoices:   []Invoice{},
	}
	if err := m.pool.QueryRow(ctx, `SELECT name, email FROM customers WHERE id = $1`, customerID).
		Scan(&out.Customer.Name, &out.Customer.Email); err != nil {
		return Overview{}, fmt.Errorf("portal: load customer: %w", err)
	}

	invoices, err := m.Invoices(ctx, userID)
	if err != nil {
		return Overview{}, err
	}
	for _, inv := range invoices {
		if inv.Payable {
			out.OpenInvoices = append(out.OpenInvoices, inv)
		}
		if len(out.RecentInvoices) < 10 {
			out.RecentInvoices = append(out.RecentInvoices, inv)
		}
	}

	if out.Balance, err = m.Balance(ctx, userID); err != nil {
		return Overview{}, err
	}
	if out.Upcoming, err = m.Services(ctx, userID); err != nil {
		return Overview{}, err
	}
	return out, nil
}

// Invoices lists the customer's invoices, newest first.
func (m *Manager) Invoices(ctx context.Context, userID string) ([]Invoice, error) {
	customerID, err := m.CustomerIDFor(ctx, userID)
	if err != nil {
		return nil, err
	}
	rows, err := m.pool.Query(ctx, `
		SELECT id, COALESCE(number, ''), status, currency, total_minor, amount_paid_minor,
		       issued_at, due_at
		FROM invoices
		WHERE customer_id = $1 AND status <> 'draft'
		ORDER BY COALESCE(issued_at, created_at) DESC`, customerID)
	if err != nil {
		return nil, fmt.Errorf("portal: list invoices: %w", err)
	}
	defer rows.Close()

	now := m.Now()
	out := []Invoice{}
	for rows.Next() {
		var (
			inv    Invoice
			total  int64
			paid   int64
			issued *time.Time
			dueAt  *time.Time
		)
		if err := rows.Scan(&inv.ID, &inv.Number, &inv.Status, &inv.Currency,
			&total, &paid, &issued, &dueAt); err != nil {
			return nil, fmt.Errorf("portal: scan invoice: %w", err)
		}
		cur, err := currencyOf(inv.Currency)
		if err != nil {
			return nil, err
		}
		inv.Total = money.New(total, cur)
		inv.Paid = money.New(paid, cur)
		if inv.Due, err = inv.Total.Sub(inv.Paid); err != nil {
			return nil, err
		}
		inv.IssuedAt = issued
		inv.DueAt = dueAt
		inv.Overdue = inv.Status == "open" && dueAt != nil && dueAt.Before(now)
		inv.Payable = inv.Status == "open" && inv.Due.IsPositive()
		out = append(out, inv)
	}
	return out, rows.Err()
}

// Invoice fetches one invoice, scoped to the customer.
//
// The scoping is in the WHERE clause, so a wrong identifier returns ErrNotFound
// rather than someone else's document.
func (m *Manager) Invoice(ctx context.Context, userID, invoiceID string) (Invoice, error) {
	customerID, err := m.CustomerIDFor(ctx, userID)
	if err != nil {
		return Invoice{}, err
	}
	var (
		inv    Invoice
		total  int64
		paid   int64
		issued *time.Time
		dueAt  *time.Time
	)
	err = m.pool.QueryRow(ctx, `
		SELECT id, COALESCE(number, ''), status, currency, total_minor, amount_paid_minor,
		       issued_at, due_at
		FROM invoices
		WHERE id = $1 AND customer_id = $2 AND status <> 'draft'`, invoiceID, customerID).
		Scan(&inv.ID, &inv.Number, &inv.Status, &inv.Currency, &total, &paid, &issued, &dueAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Invoice{}, ErrNotFound
	}
	if err != nil {
		return Invoice{}, fmt.Errorf("portal: load invoice: %w", err)
	}
	cur, err := currencyOf(inv.Currency)
	if err != nil {
		return Invoice{}, err
	}
	inv.Total = money.New(total, cur)
	inv.Paid = money.New(paid, cur)
	if inv.Due, err = inv.Total.Sub(inv.Paid); err != nil {
		return Invoice{}, err
	}
	inv.IssuedAt = issued
	inv.DueAt = dueAt
	inv.Overdue = inv.Status == "open" && dueAt != nil && dueAt.Before(m.Now())
	inv.Payable = inv.Status == "open" && inv.Due.IsPositive()
	return inv, nil
}

// Balance derives what the customer owes.
//
// Outstanding comes from the open invoices; credit comes from the subledger's
// customer-credit account. Both are derived, so neither can drift from the records
// they summarise.
func (m *Manager) Balance(ctx context.Context, userID string) (Balance, error) {
	customerID, err := m.CustomerIDFor(ctx, userID)
	if err != nil {
		return Balance{}, err
	}
	out := Balance{Outstanding: []money.Amount{}, Credit: []money.Amount{}}

	rows, err := m.pool.Query(ctx, `
		SELECT currency, COALESCE(SUM(total_minor - amount_paid_minor), 0)
		FROM invoices
		WHERE customer_id = $1 AND status = 'open'
		GROUP BY currency
		ORDER BY currency`, customerID)
	if err != nil {
		return Balance{}, fmt.Errorf("portal: sum outstanding: %w", err)
	}
	for rows.Next() {
		var code string
		var minor int64
		if err := rows.Scan(&code, &minor); err != nil {
			rows.Close()
			return Balance{}, fmt.Errorf("portal: scan outstanding: %w", err)
		}
		if minor <= 0 {
			continue
		}
		cur, err := currencyOf(code)
		if err != nil {
			rows.Close()
			return Balance{}, err
		}
		out.Outstanding = append(out.Outstanding, money.New(minor, cur))
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return Balance{}, fmt.Errorf("portal: read outstanding: %w", err)
	}
	rows.Close()

	credRows, err := m.pool.Query(ctx, `
		SELECT currency, COALESCE(SUM(credit_minor - debit_minor), 0)
		FROM journal_entries
		WHERE customer_id = $1 AND account_code = $2
		GROUP BY currency
		ORDER BY currency`, customerID, ledger.AccountCredit)
	if err != nil {
		return Balance{}, fmt.Errorf("portal: sum credit: %w", err)
	}
	defer credRows.Close()
	for credRows.Next() {
		var code string
		var minor int64
		if err := credRows.Scan(&code, &minor); err != nil {
			return Balance{}, fmt.Errorf("portal: scan credit: %w", err)
		}
		if minor <= 0 {
			continue
		}
		cur, err := currencyOf(code)
		if err != nil {
			return Balance{}, err
		}
		out.Credit = append(out.Credit, money.New(minor, cur))
	}
	return out, credRows.Err()
}

// Services lists the customer's services with the actions they may take.
func (m *Manager) Services(ctx context.Context, userID string) ([]Service, error) {
	customerID, err := m.CustomerIDFor(ctx, userID)
	if err != nil {
		return nil, err
	}
	rows, err := m.pool.Query(ctx, `
		SELECT s.id, p.name, s.status, sub.next_billing_at, sub.cancel_at_period_end
		FROM services s
		JOIN products p ON p.id = s.product_id
		LEFT JOIN subscriptions sub ON sub.id = s.subscription_id
		WHERE s.customer_id = $1
		ORDER BY s.created_at DESC`, customerID)
	if err != nil {
		return nil, fmt.Errorf("portal: list services: %w", err)
	}
	defer rows.Close()

	out := []Service{}
	for rows.Next() {
		var (
			svc         Service
			nextBill    *time.Time
			cancelAtEnd *bool
		)
		if err := rows.Scan(&svc.ID, &svc.Name, &svc.Status, &nextBill, &cancelAtEnd); err != nil {
			return nil, fmt.Errorf("portal: scan service: %w", err)
		}
		svc.NextBill = nextBill
		svc.Actions = actionsFor(svc.Status, cancelAtEnd)
		out = append(out, svc)
	}
	return out, rows.Err()
}

// Service fetches one service, scoped to the customer.
func (m *Manager) Service(ctx context.Context, userID, serviceID string) (Service, error) {
	customerID, err := m.CustomerIDFor(ctx, userID)
	if err != nil {
		return Service{}, err
	}
	var (
		svc         Service
		nextBill    *time.Time
		cancelAtEnd *bool
	)
	err = m.pool.QueryRow(ctx, `
		SELECT s.id, p.name, s.status, sub.next_billing_at, sub.cancel_at_period_end
		FROM services s
		JOIN products p ON p.id = s.product_id
		LEFT JOIN subscriptions sub ON sub.id = s.subscription_id
		WHERE s.id = $1 AND s.customer_id = $2`, serviceID, customerID).
		Scan(&svc.ID, &svc.Name, &svc.Status, &nextBill, &cancelAtEnd)
	if errors.Is(err, pgx.ErrNoRows) {
		return Service{}, ErrNotFound
	}
	if err != nil {
		return Service{}, fmt.Errorf("portal: load service: %w", err)
	}
	svc.NextBill = nextBill
	svc.Actions = actionsFor(svc.Status, cancelAtEnd)
	return svc, nil
}

// actionsFor is the single source of truth for which actions a service offers.
//
// The action handlers enforce the same rules, so the UI cannot offer an action the
// server would refuse, and cannot hide one the server would allow.
func actionsFor(status string, cancelAtEnd *bool) []string {
	switch status {
	case "active":
		// Cancelling at period end is the only self-service cancellation: an
		// immediate termination would destroy time the customer already paid for.
		if cancelAtEnd != nil && *cancelAtEnd {
			return []string{}
		}
		return []string{"cancel"}
	case "suspended":
		// A suspended service is restored by paying, not by a button.
		return []string{"pay_to_restore"}
	default:
		return []string{}
	}
}

// RequestCancellation schedules cancellation at the end of the paid period.
//
// It is the only customer-initiated change to a service. It does not stop the
// service immediately: the customer has paid for the current period, and taking it
// away early is both unfair and a refund event.
func (m *Manager) RequestCancellation(ctx context.Context, userID, serviceID string) (time.Time, error) {
	customerID, err := m.CustomerIDFor(ctx, userID)
	if err != nil {
		return time.Time{}, err
	}

	var (
		status       string
		subscription *string
	)
	err = m.pool.QueryRow(ctx, `
		SELECT status, subscription_id FROM services
		WHERE id = $1 AND customer_id = $2`, serviceID, customerID).
		Scan(&status, &subscription)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, ErrNotFound
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("portal: load service: %w", err)
	}
	if status != "active" {
		return time.Time{}, fmt.Errorf("portal: a %s service cannot be cancelled", status)
	}
	if subscription == nil {
		return time.Time{}, errors.New("portal: this service has no billing schedule to cancel")
	}

	var endsAt time.Time
	err = m.pool.QueryRow(ctx, `
		UPDATE subscriptions
		SET cancel_at_period_end = true, cancelled_at = COALESCE(cancelled_at, $2),
		    version = version + 1
		WHERE id = $1 AND cancel_at_period_end = false
		RETURNING current_period_end`,
		*subscription, m.Now()).Scan(&endsAt)
	if errors.Is(err, pgx.ErrNoRows) {
		// Already cancelled: report the same answer rather than an error, so a
		// double click is not a failure.
		if err := m.pool.QueryRow(ctx,
			`SELECT current_period_end FROM subscriptions WHERE id = $1`, *subscription).
			Scan(&endsAt); err != nil {
			return time.Time{}, fmt.Errorf("portal: load schedule: %w", err)
		}
		return endsAt, nil
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("portal: schedule cancellation: %w", err)
	}
	return endsAt, nil
}

func currencyOf(code string) (money.Currency, error) {
	if cur, ok := money.LookupCurrency(code); ok {
		return cur, nil
	}
	return money.NewCurrency(code, 2)
}
