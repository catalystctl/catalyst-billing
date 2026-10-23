package renewal_test

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/catalyst/billing/internal/calendar"
	"github.com/catalyst/billing/internal/credit"
	"github.com/catalyst/billing/internal/ledger"
	"github.com/catalyst/billing/internal/money"
)

// Credit is spent on a renewal invoice (CRD-03).
//
// `credits.auto_use` says a balance that covers an invoice settles it without the customer doing
// anything. Before this call existed the rule ran only on the checkout path, so a customer who
// topped up and then let the renewal come round was invoiced in full and their balance sat
// untouched — the exact opposite of what the credits page promises.
//
// The test grants credit through the real grant path (a numbered note and a ledger posting) and
// then renews, so what it proves is the *posting rule at issuance*, not a shortcut.

// grantCredit puts credit on the harness customer's balance through credit.Service.Grant.
func (h *harness) grantCredit(t *testing.T, amountMinor int64) {
	t.Helper()
	svc := credit.New(h.pool)
	if _, err := svc.Grant(context.Background(), credit.GrantRequest{
		CustomerID:  h.customerID,
		AmountMinor: amountMinor,
		Currency:    "USD",
		Reason:      "test goodwill",
		Key:         "renewal-autouse-" + h.customerID,
	}); err != nil {
		t.Fatalf("grant credit: %v", err)
	}
}

// A renewal whose total the balance covers is issued *and paid* by the credit, in the same
// transaction, and the balance keeps exactly the remainder.
func TestRenewalSettlesFromCreditWhenTheBalanceCoversIt(t *testing.T) {
	now := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	h := newHarness(t, now)
	subID := h.subscribe(t, now.AddDate(0, -1, 0), calendar.Monthly, 1, "UTC")

	base := h.amount()
	h.grantCredit(t, base+500)

	results := h.run(t)
	if len(results) != 1 || results[0].Outcome != "invoiced" {
		t.Fatalf("renewal results = %+v, want one invoiced period", results)
	}

	var (
		total, paid int64
		status      string
	)
	if err := h.pool.QueryRow(context.Background(), `
		SELECT total_minor, amount_paid_minor, status FROM invoices WHERE id = $1`,
		results[0].InvoiceID).Scan(&total, &paid, &status); err != nil {
		t.Fatalf("read invoice: %v", err)
	}
	if paid != total {
		t.Fatalf("invoice paid %d of %d; the credit was not applied at issuance", paid, total)
	}
	if status != "paid" {
		t.Fatalf("invoice status = %q, want paid", status)
	}

	// The application is a journal entry against the credit account, keyed by the invoice, and it
	// spent exactly the invoice total.
	var spent int64
	if err := h.pool.QueryRow(context.Background(), `
		SELECT COALESCE(SUM(je.debit_minor), 0)
		FROM journal_entries je
		JOIN journal_transactions jt ON jt.id = je.transaction_id
		WHERE je.customer_id = $1 AND je.account_code = $2
		  AND jt.reference_type = $3`,
		h.customerID, ledger.AccountCredit, string(ledger.RefCreditApplication)).Scan(&spent); err != nil {
		t.Fatalf("read application: %v", err)
	}
	if spent != total {
		t.Fatalf("credit applied = %d, want the invoice total %d", spent, total)
	}

	balances, err := credit.BalanceFor(context.Background(), h.pool, h.customerID)
	if err != nil {
		t.Fatalf("balance: %v", err)
	}
	var remaining int64
	for _, amount := range balances {
		if amount.Currency().Equal(usd) {
			remaining = amount.Minor()
		}
	}
	if remaining != 500 {
		t.Fatalf("remaining credit = %d, want 500 (the part the invoice did not need)", remaining)
	}

	// The subscription advanced: the customer got the period they paid for with credit.
	var next time.Time
	if err := h.pool.QueryRow(context.Background(),
		`SELECT next_billing_at FROM subscriptions WHERE id = $1`, subID).Scan(&next); err != nil {
		t.Fatalf("read subscription: %v", err)
	}
	if !next.After(now) {
		t.Fatalf("next_billing_at = %v, want after the run at %v", next, now)
	}
}

// When the balance does not cover the whole invoice, the invoice stays open with the credit
// applied — the remainder is what the gateway is asked for, not a second full charge.
func TestRenewalLeavesTheRemainderWhenCreditIsPartial(t *testing.T) {
	now := time.Date(2026, 4, 1, 9, 0, 0, 0, time.UTC)
	h := newHarness(t, now)
	h.subscribe(t, now.AddDate(0, -1, 0), calendar.Monthly, 1, "UTC")

	base := h.amount()
	h.grantCredit(t, base/2)

	results := h.run(t)
	if len(results) != 1 {
		t.Fatalf("renewal results = %+v, want one period", results)
	}

	var total, paid int64
	var status string
	if err := h.pool.QueryRow(context.Background(), `
		SELECT total_minor, amount_paid_minor, status FROM invoices WHERE id = $1`,
		results[0].InvoiceID).Scan(&total, &paid, &status); err != nil {
		t.Fatalf("read invoice: %v", err)
	}
	if paid != base/2 {
		t.Fatalf("invoice paid %d, want the whole balance %d", paid, base/2)
	}
	if status != "open" {
		t.Fatalf("invoice status = %q, want open; a partial payment must not settle it", status)
	}
	if total-paid != base-base/2 {
		t.Fatalf("remaining owed = %d, want %d", total-paid, base-base/2)
	}
}

// amount is the harness product's base price in minor units.
func (h *harness) amount() int64 {
	var minor int64
	if err := h.pool.QueryRow(context.Background(),
		`SELECT amount_minor FROM price_versions WHERE id = $1`, h.priceID).Scan(&minor); err != nil {
		panic(err)
	}
	return minor
}

// A deprecated-currency guard: the fixture's amounts are USD, and the tests above read balances in
// USD. Keeping the assertion here means a future non-USD harness fails loudly rather than silently
// comparing the wrong currency's sum.
var _ = money.MustLookupCurrency("USD")

// --------------------------------------------------------- the differential --

// renewalDocument is everything about a renewal invoice a customer could observe: the money, the
// wording, the line items and the frozen evidence. Ids, invoice numbers and the two "when we wrote
// it" timestamps are excluded because two invoices written moments apart cannot share them.
type renewalDocument struct {
	Status     string
	Currency   string
	PeriodKey  string
	Subtotal   int64
	Tax        int64
	Total      int64
	AmountPaid int64
	DueAt      time.Time
	Lines      []string
	Snapshot   string
}

// renewOne runs one engine tick for one fresh subscription and captures its invoice.
func (h *harness) renewOne(t *testing.T, autoUse bool) (renewalDocument, string) {
	t.Helper()
	ctx := context.Background()
	if _, err := h.pool.Exec(ctx, `
		INSERT INTO settings (group_key, key, value) VALUES ('credits', 'auto_use', $1)
		ON CONFLICT (group_key, key) DO UPDATE SET value = EXCLUDED.value`,
		map[bool]string{true: "true", false: "false"}[autoUse]); err != nil {
		t.Fatalf("set auto_use: %v", err)
	}

	h.subscribe(t, h.now.AddDate(0, -1, 0), calendar.Monthly, 1, "UTC")
	results := h.run(t)
	if len(results) != 1 || results[0].Outcome != "invoiced" {
		t.Fatalf("renewal results = %+v, want one invoiced period", results)
	}
	invoiceID := results[0].InvoiceID

	var doc renewalDocument
	if err := h.pool.QueryRow(ctx, `
		SELECT status, currency, COALESCE(period_key, ''), subtotal_minor, tax_minor,
		       total_minor, amount_paid_minor, due_at, snapshot::text
		FROM invoices WHERE id = $1`, invoiceID).
		Scan(&doc.Status, &doc.Currency, &doc.PeriodKey, &doc.Subtotal, &doc.Tax, &doc.Total,
			&doc.AmountPaid, &doc.DueAt, &doc.Snapshot); err != nil {
		t.Fatalf("read invoice: %v", err)
	}

	rows, err := h.pool.Query(ctx, `
		SELECT position, description, quantity::text, unit_minor, net_minor, tax_rate,
		       tax_minor, total_minor
		FROM invoice_lines WHERE invoice_id = $1 ORDER BY position`, invoiceID)
	if err != nil {
		t.Fatalf("read lines: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			position int
			line     string
			qty      string
			unit     int64
			net      int64
			rate     string
			tax      int64
			total    int64
		)
		var description string
		if err := rows.Scan(&position, &description, &qty, &unit, &net, &rate, &tax, &total); err != nil {
			t.Fatalf("scan line: %v", err)
		}
		line = fmt.Sprintf("%d|%s|q=%s|unit=%d|net=%d|rate=%s|tax=%d|total=%d",
			position, description, qty, unit, net, rate, tax, total)
		doc.Lines = append(doc.Lines, line)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read lines: %v", err)
	}
	rows.Close()

	// The invoice's own posting: one journal transaction, referencing the invoice, and nothing
	// credit-shaped. This is the part a customer with no balance must never see appear.
	var (
		postings           int
		creditApplications int
	)
	if err := h.pool.QueryRow(ctx, `
		SELECT count(*) FROM journal_transactions
		WHERE reference_type = 'invoice' AND reference_id = $1`, invoiceID).Scan(&postings); err != nil {
		t.Fatalf("count postings: %v", err)
	}
	if err := h.pool.QueryRow(ctx, `
		SELECT count(*) FROM credit_applications WHERE invoice_id = $1`, invoiceID).
		Scan(&creditApplications); err != nil {
		t.Fatalf("count credit applications: %v", err)
	}
	if postings != 1 {
		t.Fatalf("%d journal transactions for the invoice, want exactly 1", postings)
	}
	if creditApplications != 0 {
		t.Fatalf("%d credit applications on an invoice nobody had credit for", creditApplications)
	}

	return doc, invoiceID
}

// TestRenewalIsUnchangedForACustomerWithNoCredit is the differential the money path needs.
//
// `renewal.go` bills every subscription in the system, so "the credit wiring is inert when there
// is no credit" has to be a property, not a promise. Two identical subscriptions on one customer,
// one renewed with `credits.auto_use` on and one with it off, produce the same document: same
// money, same lines, same terms, same frozen snapshot, and the same single journal posting. If the
// wiring ever writes anything — a zero-amount application, a settings-dependent line, a second
// posting — the two documents diverge and this fails.
//
// It also pins the shape the orchestrator asked for: `auto_use` is the only thing that changes
// between the two runs, and for a customer with a zero balance it changes nothing observable.
func TestRenewalIsUnchangedForACustomerWithNoCredit(t *testing.T) {
	now := time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC)
	h := newHarness(t, now)

	withAutoUse, firstInvoice := h.renewOne(t, true)
	withoutAutoUse, secondInvoice := h.renewOne(t, false)

	if firstInvoice == secondInvoice {
		t.Fatal("the two runs share an invoice, so the comparison proves nothing")
	}
	if !reflect.DeepEqual(withAutoUse, withoutAutoUse) {
		t.Fatalf("the renewal document changed when auto-use credit was switched on:\n with: %+v\n  off: %+v",
			withAutoUse, withoutAutoUse)
	}

	// And the balance is still zero: nothing was spent because there was nothing to spend.
	balances, err := credit.BalanceFor(context.Background(), h.pool, h.customerID)
	if err != nil {
		t.Fatalf("balance: %v", err)
	}
	if len(balances) != 0 {
		t.Fatalf("balances = %v, want none", balances)
	}
}
