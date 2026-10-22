package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// Dashboard widgets over a selectable period (ADM-01, ADM-36, ADM-37).
//
// The properties asserted here are the ones a dashboard gets wrong: revenue that comes from
// invoice totals rather than the ledger, a figure that changes its answer when the period
// changes for the wrong reason, a period that is really a constant, and a broken widget that
// reports zero instead of saying it could not be read.

// postRevenue posts a balanced revenue transaction at an explicit instant.
//
// The instant is explicit because the whole point of these tests is the period: an entry is
// inserted with the created_at it is meant to have, rather than backdated afterwards, because
// journal_entries is append-only and refuses an UPDATE (0001's trigger).
func (e *authEnv) postRevenue(t *testing.T, currency string, minor int64, at time.Time) {
	t.Helper()
	ctx := context.Background()
	if err := e.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var txID string
		if err := tx.QueryRow(ctx, `
			INSERT INTO journal_transactions (kind, reference_type, reference_id, currency, created_at)
			VALUES ('invoice_issued', 'invoice', gen_random_uuid(), $1, $2)
			RETURNING id`, currency, at).Scan(&txID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO journal_entries (transaction_id, account_code, currency, debit_minor, created_at)
			VALUES ($1, 'ar', $2, $3, $4)`, txID, currency, minor, at); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO journal_entries (transaction_id, account_code, currency, credit_minor, created_at)
			VALUES ($1, 'revenue', $2, $3, $4)`, txID, currency, minor, at)
		return err
	}); err != nil {
		t.Fatalf("post revenue: %v", err)
	}
}

// widgetBody is the response shape.
type widgetBody struct {
	Period      string    `json:"period"`
	From        time.Time `json:"from"`
	To          time.Time `json:"to"`
	Days        int       `json:"days"`
	GeneratedAt time.Time `json:"generated_at"`
	Available   bool      `json:"available"`
	Unavailable []string  `json:"unavailable"`
	Revenue     []struct {
		Currency string `json:"currency"`
		Minor    int64  `json:"minor"`
		Display  string `json:"display"`
	} `json:"revenue"`
	ActiveCustomers *int `json:"active_customers"`
	NewCustomers    *int `json:"new_customers"`
	OpenTickets     *int `json:"open_tickets"`
	TicketsOpened   *int `json:"tickets_opened"`
	OverdueInvoices *struct {
		Count       int    `json:"count"`
		OldestDueAt string `json:"oldest_due_at"`
		Money       []struct {
			Currency string `json:"currency"`
			Minor    int64  `json:"minor"`
		} `json:"money"`
	} `json:"overdue_invoices"`
	PaidNotProvisioned *struct {
		Code      string `json:"code"`
		Count     int    `json:"count"`
		Truncated bool   `json:"truncated"`
		OldestAt  string `json:"oldest_at"`
		Money     []struct {
			Currency string `json:"currency"`
			Minor    int64  `json:"minor"`
		} `json:"money"`
		Items []struct {
			InvoiceID string         `json:"invoice_id"`
			Number    string         `json:"number"`
			OrderID   string         `json:"order_id"`
			Total     map[string]any `json:"total"`
		} `json:"items"`
	} `json:"paid_not_provisioned"`
}

// widgets fetches the widget set as the billing clerk and decodes it.
func (e *staffEnv) widgets(t *testing.T, query string) widgetBody {
	t.Helper()
	rec := e.get(t, "/api/v1/staff/dashboard/widgets"+query, e.billing)
	if rec.Code != http.StatusOK {
		t.Fatalf("widgets %s: status %d, body %s", query, rec.Code, rec.Body)
	}
	var body widgetBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v; body %s", err, rec.Body)
	}
	return body
}

// Revenue comes from the ledger and is scoped to the period, per currency.
//
// Three postings: one in the window, one 40 days old, and one that is a debit to revenue (a
// credit note). The 30-day figure must be the net of the first and third; the 90-day figure
// must include the second as well.
func TestDashboardWidgetsRevenueIsFromTheLedgerAndScopedToThePeriod(t *testing.T) {
	env := newStaffEnv(t)
	now := time.Now().UTC()

	env.postRevenue(t, "USD", 10000, now.Add(-2*24*time.Hour))
	env.postRevenue(t, "USD", 5000, now.Add(-40*24*time.Hour))
	// A credit note: a debit to the revenue account reduces revenue. This is the entry that
	// makes the ledger the only honest source — no invoice total records it.
	if err := env.pool.InTx(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		var txID string
		if err := tx.QueryRow(ctx, `
			INSERT INTO journal_transactions (kind, reference_type, reference_id, currency, created_at)
			VALUES ('credit_note', 'credit_note', gen_random_uuid(), 'USD', $1)
			RETURNING id`, now.Add(-time.Hour)).Scan(&txID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO journal_entries (transaction_id, account_code, currency, debit_minor, created_at)
			VALUES ($1, 'revenue', 'USD', 1000, $2)`, txID, now.Add(-time.Hour)); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO journal_entries (transaction_id, account_code, currency, credit_minor, created_at)
			VALUES ($1, 'ar', 'USD', 1000, $2)`, txID, now.Add(-time.Hour))
		return err
	}); err != nil {
		t.Fatalf("post credit note: %v", err)
	}
	// A second currency, so the response cannot collapse them.
	env.postRevenue(t, "EUR", 700, now.Add(-3*time.Hour))

	thirty := env.widgets(t, "?period=30d")
	byCurrency := map[string]int64{}
	for _, r := range thirty.Revenue {
		byCurrency[r.Currency] = r.Minor
	}
	if byCurrency["USD"] != 9000 {
		t.Fatalf("USD revenue over 30d = %d, want 9000 (10000 in, 1000 credited)", byCurrency["USD"])
	}
	if byCurrency["EUR"] != 700 {
		t.Fatalf("EUR revenue over 30d = %d, want 700", byCurrency["EUR"])
	}
	if len(thirty.Revenue) != 2 {
		t.Fatalf("%d revenue lines, want one per currency: %+v", len(thirty.Revenue), thirty.Revenue)
	}
	if thirty.Period != "30d" || thirty.Days != 30 {
		t.Fatalf("period = %q days = %d, want 30d and 30", thirty.Period, thirty.Days)
	}
	if !thirty.Available {
		t.Fatalf("available = false on a healthy database; unavailable = %v", thirty.Unavailable)
	}

	ninety := env.widgets(t, "?period=90d")
	byCurrency = map[string]int64{}
	for _, r := range ninety.Revenue {
		byCurrency[r.Currency] = r.Minor
	}
	if byCurrency["USD"] != 14000 {
		t.Fatalf("USD revenue over 90d = %d, want 14000 (the 40-day-old posting is in range)",
			byCurrency["USD"])
	}
	if ninety.Days != 90 {
		t.Fatalf("90d reports %d days", ninety.Days)
	}
}

// A custom range is honoured, and a date-only bound means the whole day.
func TestDashboardWidgetsCustomPeriod(t *testing.T) {
	env := newStaffEnv(t)
	now := time.Now().UTC()

	// One posting well outside the custom window, one inside it.
	env.postRevenue(t, "USD", 2000, now.Add(-20*24*time.Hour))
	env.postRevenue(t, "USD", 300, now.Add(-3*time.Hour))

	from := now.Add(-5 * 24 * time.Hour).Format("2006-01-02")
	to := now.Format("2006-01-02")
	body := env.widgets(t, "?period=custom&from="+from+"&to="+to)

	if body.Period != "custom" {
		t.Fatalf("period = %q, want custom", body.Period)
	}
	var usd int64
	for _, r := range body.Revenue {
		if r.Currency == "USD" {
			usd = r.Minor
		}
	}
	if usd != 300 {
		t.Fatalf("USD revenue in the custom window = %d, want 300", usd)
	}
	// A date-only `to` covers that whole day, so the window ends after now, not at midnight.
	if body.To.Before(now) {
		t.Fatalf("the custom window ends at %s, before the posting at %s", body.To, now)
	}
}

// An empty period reports zero, not an error. This is the difference between a quiet month
// and an outage, and the dashboard must not blur it.
func TestDashboardWidgetsEmptyPeriodIsZeroNotAnError(t *testing.T) {
	env := newStaffEnv(t)
	body := env.widgets(t, "?period=7d")

	if !body.Available {
		t.Fatalf("available = false on an empty database; unavailable = %v", body.Unavailable)
	}
	if body.ActiveCustomers == nil || *body.ActiveCustomers != 0 {
		t.Fatalf("active_customers = %v, want 0", body.ActiveCustomers)
	}
	if body.NewCustomers == nil || *body.NewCustomers != 0 {
		t.Fatalf("new_customers = %v, want 0", body.NewCustomers)
	}
	if body.OpenTickets == nil || *body.OpenTickets != 0 {
		t.Fatalf("open_tickets = %v, want 0", body.OpenTickets)
	}
	if body.OverdueInvoices == nil || body.OverdueInvoices.Count != 0 {
		t.Fatalf("overdue_invoices = %+v, want a zero count", body.OverdueInvoices)
	}
	if body.PaidNotProvisioned == nil || body.PaidNotProvisioned.Count != 0 {
		t.Fatalf("paid_not_provisioned = %+v, want a zero count", body.PaidNotProvisioned)
	}
	// An empty list, not null: a client that iterates must not have to guard for it.
	if body.Revenue == nil {
		t.Fatal("revenue is null for an empty period; it must be an empty list")
	}
}

// The counts are real figures, and the paid-not-provisioned queue empties when the service
// exists.
func TestDashboardWidgetsCountsAndExceptionQueue(t *testing.T) {
	env := newStaffEnv(t)
	ctx := context.Background()

	// Two customers: one with an open overdue invoice, one that just signed up.
	overdueCustomer, _ := env.seedCustomer(t, "overdue@example.test", "Overdue")
	freshCustomer, _ := env.seedCustomer(t, "fresh@example.test", "Fresh")
	past := time.Now().UTC().Add(-72 * time.Hour)
	env.seedInvoice(t, overdueCustomer, "open", "USD", 4000, 1000, &past)

	// One open ticket and one closed one.
	var ticketCustomer string
	if err := env.pool.QueryRow(ctx, `
		INSERT INTO customers (email, name) VALUES ('ticket@example.test', 'Ticket')
		RETURNING id`).Scan(&ticketCustomer); err != nil {
		t.Fatalf("ticket customer: %v", err)
	}
	for _, status := range []string{"open", "closed"} {
		if _, err := env.pool.Exec(ctx, `
			INSERT INTO tickets (customer_id, subject, status) VALUES ($1, $2, $3)`,
			ticketCustomer, "A ticket", status); err != nil {
			t.Fatalf("ticket: %v", err)
		}
	}

	// A paid order whose product has no service: the paid-not-provisioned case.
	var productID, orderID, paidInvoice string
	if err := env.pool.QueryRow(ctx, `
		INSERT INTO products (slug, name) VALUES ('unprovisioned', 'Unprovisioned') RETURNING id`,
	).Scan(&productID); err != nil {
		t.Fatalf("product: %v", err)
	}
	var priceID string
	if err := env.pool.QueryRow(ctx, `
		INSERT INTO price_versions (product_id, version, currency, amount_minor, interval,
			anchor_day, anchor_timezone)
		VALUES ($1, 1, 'USD', 2500, 'monthly', 1, 'UTC') RETURNING id`, productID).Scan(&priceID); err != nil {
		t.Fatalf("price: %v", err)
	}
	if err := env.pool.QueryRow(ctx, `
		INSERT INTO orders (customer_id, status, currency, total_minor)
		VALUES ($1, 'paid', 'USD', 2500) RETURNING id`, overdueCustomer).Scan(&orderID); err != nil {
		t.Fatalf("order: %v", err)
	}
	if _, err := env.pool.Exec(ctx, `
		INSERT INTO order_items (order_id, price_version_id, description, quantity, unit_minor,
			line_minor, position)
		VALUES ($1, $2, 'Unprovisioned', 1, 2500, 2500, 1)`, orderID, priceID); err != nil {
		t.Fatalf("order item: %v", err)
	}
	if err := env.pool.QueryRow(ctx, `
		INSERT INTO invoices (number, customer_id, order_id, status, currency,
			subtotal_minor, tax_minor, total_minor, amount_paid_minor, issued_at, snapshot)
		VALUES ('INV-PNP', $1, $2, 'paid', 'USD', 2500, 0, 2500, 2500, now(),
		        '{"schema_version":1}'::jsonb)
		RETURNING id`, overdueCustomer, orderID).Scan(&paidInvoice); err != nil {
		t.Fatalf("paid invoice: %v", err)
	}

	body := env.widgets(t, "?period=30d")

	if body.ActiveCustomers == nil || *body.ActiveCustomers < 1 {
		t.Fatalf("active_customers = %v, want at least 1", body.ActiveCustomers)
	}
	if body.NewCustomers == nil || *body.NewCustomers < 3 {
		t.Fatalf("new_customers = %v, want at least 3", body.NewCustomers)
	}
	if body.OpenTickets == nil || *body.OpenTickets != 1 {
		t.Fatalf("open_tickets = %v, want 1 (the closed one is not in the queue)", body.OpenTickets)
	}
	if body.TicketsOpened == nil || *body.TicketsOpened != 2 {
		t.Fatalf("tickets_opened = %v, want 2", body.TicketsOpened)
	}

	if body.OverdueInvoices == nil || body.OverdueInvoices.Count != 1 {
		t.Fatalf("overdue_invoices = %+v, want 1", body.OverdueInvoices)
	}
	// The amount is what is still owed (4000 - 1000), not the invoice total.
	if len(body.OverdueInvoices.Money) != 1 || body.OverdueInvoices.Money[0].Minor != 3000 {
		t.Fatalf("overdue money = %+v, want 3000 USD outstanding", body.OverdueInvoices.Money)
	}

	queue := body.PaidNotProvisioned
	if queue == nil || queue.Count != 1 {
		t.Fatalf("paid_not_provisioned = %+v, want one invoice", queue)
	}
	if len(queue.Items) != 1 || queue.Items[0].InvoiceID != paidInvoice {
		t.Fatalf("queue items = %+v, want the paid invoice %s", queue.Items, paidInvoice)
	}
	if queue.Items[0].OrderID != orderID || queue.Items[0].Number != "INV-PNP" {
		t.Fatalf("queue item = %+v", queue.Items[0])
	}
	if queue.Items[0].Total["minor"].(float64) != 2500 {
		t.Fatalf("queue item total = %v, want 2500", queue.Items[0].Total)
	}
	if len(queue.Money) != 1 || queue.Money[0].Minor != 2500 {
		t.Fatalf("queue money = %+v, want 2500 USD at stake", queue.Money)
	}
	if queue.Truncated {
		t.Fatal("a one-row queue reports itself truncated")
	}

	// A customer who has only signed up is not active. Invoicing them is what makes them
	// active, and the figure must move by exactly one: a count of every row in `customers`
	// would never move, and a business that only ever grows is the most flattering bug.
	beforeActive := *body.ActiveCustomers
	env.seedInvoice(t, freshCustomer, "open", "USD", 1000, 0, nil)
	grown := env.widgets(t, "?period=30d")
	if grown.ActiveCustomers == nil || *grown.ActiveCustomers != beforeActive+1 {
		t.Fatalf("active_customers went from %d to %v after invoicing one more customer, want %d",
			beforeActive, grown.ActiveCustomers, beforeActive+1)
	}

	// Provision the service: the exception clears, which is what makes the queue a queue
	// rather than a counter.
	if _, err := env.pool.Exec(ctx, `
		INSERT INTO services (customer_id, product_id, status)
		VALUES ($1, $2, 'active')`, overdueCustomer, productID); err != nil {
		t.Fatalf("service: %v", err)
	}
	after := env.widgets(t, "?period=30d")
	if after.PaidNotProvisioned == nil || after.PaidNotProvisioned.Count != 0 {
		t.Fatalf("paid_not_provisioned after provisioning = %+v, want 0", after.PaidNotProvisioned)
	}
}

// A period that is not one of the accepted shapes is a stable problem code, not a 500 and
// not a silent fallback.
func TestDashboardWidgetsPeriodValidation(t *testing.T) {
	env := newStaffEnv(t)

	for _, query := range []string{
		"?period=10y",
		"?period=1d",
		"?period=custom",                 // no from/to
		"?period=custom&from=2026-01-01", // no to
		"?period=custom&from=2026-02-01&to=2026-01-01", // to before from
		"?period=custom&from=2020-01-01&to=2026-01-01", // longer than 366 days
		"?period=custom&from=yesterday&to=today",       // unparseable
	} {
		rec := env.get(t, "/api/v1/staff/dashboard/widgets"+query, env.billing)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: status %d, want 400; body %s", query, rec.Code, rec.Body)
		}
		if !containsCode(rec.Body.Bytes(), "invalid_period") {
			t.Fatalf("%s: body %s does not carry invalid_period", query, rec.Body)
		}
	}
}

// The widget set carries the same permission as the dashboard it extends.
func TestDashboardWidgetsRequireReportRead(t *testing.T) {
	env := newStaffEnv(t)

	if rec := env.get(t, "/api/v1/staff/dashboard/widgets", env.admin); rec.Code != http.StatusOK {
		t.Fatalf("admin: status %d, body %s", rec.Code, rec.Body)
	}
	// Support holds ticket and log permissions, not report:read.
	if rec := env.get(t, "/api/v1/staff/dashboard/widgets", env.support); rec.Code != http.StatusForbidden {
		t.Fatalf("support: status %d, want 403; body %s", rec.Code, rec.Body)
	}
	if rec := env.get(t, "/api/v1/staff/dashboard/widgets", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous: status %d, want 401", rec.Code)
	}
}
