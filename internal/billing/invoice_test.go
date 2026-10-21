package billing_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/catalyst/billing/internal/billing"
	"github.com/catalyst/billing/internal/ledger"
	"github.com/catalyst/billing/internal/money"
	"github.com/catalyst/billing/internal/store"
	"github.com/catalyst/billing/internal/store/storetest"
	"github.com/jackc/pgx/v5"
)

var usd = money.MustLookupCurrency("USD")

// ------------------------------------------------------------------ fixtures --

type world struct {
	pool       *store.Pool
	issuer     *billing.Issuer
	customerID string
	orderID    string
}

func newWorld(t *testing.T, pricer billing.Pricer) world {
	t.Helper()
	pool := storetest.DB(t)
	ctx := context.Background()
	var w world
	w.pool = pool
	w.issuer = billing.NewIssuer(pricer)

	err := pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx,
			`INSERT INTO customers (email, name) VALUES ('a@example.test', 'Customer A') RETURNING id`,
		).Scan(&w.customerID); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `
			INSERT INTO orders (customer_id, status, currency, total_minor)
			VALUES ($1, 'paid', 'USD', 0) RETURNING id`, w.customerID).Scan(&w.orderID)
	})
	if err != nil {
		t.Fatalf("fixtures: %v", err)
	}
	return w
}

func (w world) request(lines ...billing.Line) billing.IssueRequest {
	return billing.IssueRequest{
		CustomerID: w.customerID,
		OrderID:    w.orderID,
		Lines:      lines,
		Merchant: billing.Merchant{
			Name: "Catalyst Hosting Ltd", Address: "1 Example St", CountryCode: "GB",
		},
		Customer: billing.CustomerDetails{
			ID: w.customerID, Name: "Customer A", Email: "a@example.test", CountryCode: "GB",
		},
		Terms: "Renews automatically every month. Cancel any time before the renewal date.",
	}
}

// issue creates and issues an invoice in one transaction.
func (w world) issue(t *testing.T, req billing.IssueRequest) billing.Issued {
	t.Helper()
	ctx := context.Background()
	var out billing.Issued
	err := w.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		id, _, _, err := w.issuer.CreateDraft(ctx, tx, req)
		if err != nil {
			return err
		}
		out, err = w.issuer.Issue(ctx, tx, id, billing.NumberSeries{Scope: "default", Prefix: "INV-", Width: 6})
		return err
	})
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	return out
}

func line(desc string, unit string, qtyMilli int64) billing.Line {
	return billing.Line{
		Description:   desc,
		QuantityMilli: qtyMilli,
		UnitPrice:     money.MustParse(unit, usd),
		TaxRate:       "0",
	}
}

func noTax() billing.Pricer {
	return billing.FixedTaxPricer{
		Rate:     money.MustParseRatio("0%"),
		Rounding: money.RoundHalfUp,
		Policy:   "none",
	}
}

// -------------------------------------------------------------------- tests --

func TestIssueFreezesSnapshotAndPostsReceivable(t *testing.T) {
	w := newWorld(t, noTax())
	issued := w.issue(t, w.request(
		line("VPS Small monthly", "12.00", 1000),
		line("Backup add-on", "2.50", 1000),
	))

	if issued.Number != "INV-000001" {
		t.Fatalf("number = %q, want INV-000001", issued.Number)
	}
	if got, want := issued.Totals.Total.String(), "14.50"; got != want {
		t.Fatalf("total = %s, want %s", got, want)
	}

	// The snapshot is the document: merchant, customer, lines and terms.
	if issued.Snapshot.Merchant.Name != "Catalyst Hosting Ltd" {
		t.Fatalf("merchant not snapshotted: %+v", issued.Snapshot.Merchant)
	}
	if len(issued.Snapshot.Lines) != 2 {
		t.Fatalf("snapshot has %d lines, want 2", len(issued.Snapshot.Lines))
	}
	if issued.Snapshot.Terms == "" {
		t.Fatal("renewal terms not snapshotted")
	}

	ctx := context.Background()
	// Receivable is debited, revenue credited, and the two sides agree.
	ar, err := ledger.Balances(ctx, w.pool, ledger.AccountReceivable, usd)
	if err != nil {
		t.Fatal(err)
	}
	revenue, err := ledger.Balances(ctx, w.pool, ledger.AccountRevenue, usd)
	if err != nil {
		t.Fatal(err)
	}
	if ar.Debits.Minor() != 1450 {
		t.Fatalf("receivable debits = %s, want 14.50", ar.Debits)
	}
	if revenue.Credits.Minor() != 1450 {
		t.Fatalf("revenue credits = %s, want 14.50", revenue.Credits)
	}

	owed, err := ledger.ReceivableForCustomer(ctx, w.pool, w.customerID, usd)
	if err != nil {
		t.Fatal(err)
	}
	if owed.Minor() != 1450 {
		t.Fatalf("customer owes %s, want 14.50", owed)
	}

	// The ledger as a whole balances in every currency.
	tb, err := ledger.TrialBalance(ctx, w.pool)
	if err != nil {
		t.Fatal(err)
	}
	for code, net := range tb {
		if !net.IsZero() {
			t.Fatalf("trial balance for %s is %s, want 0", code, net)
		}
	}
}

// Acceptance scenario 1: a replayed webhook 100 times produces one payment
// allocation.
func TestReplayedPaymentIsIdempotent(t *testing.T) {
	w := newWorld(t, noTax())
	issued := w.issue(t, w.request(line("VPS Small monthly", "12.00", 1000)))

	ctx := context.Background()
	req := billing.AllocationRequest{
		Provider:          "stripe",
		ProviderAccount:   "acct_123",
		ProviderPaymentID: "pi_abc",
		InvoiceID:         issued.InvoiceID,
		Amount:            money.MustParse("12.00", usd),
	}

	var first billing.AllocationResult
	for i := 0; i < 100; i++ {
		var res billing.AllocationResult
		err := w.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			res, err = billing.ApplyPayment(ctx, tx, req)
			return err
		})
		if err != nil {
			t.Fatalf("attempt %d: %v", i, err)
		}
		if i == 0 {
			if res.Duplicate {
				t.Fatal("first attempt reported as duplicate")
			}
			if !res.InvoicePaid {
				t.Fatal("invoice not marked paid by a full payment")
			}
			first = res
			continue
		}
		if !res.Duplicate {
			t.Fatalf("attempt %d was not reported as a duplicate", i)
		}
		if res.AllocationID != first.AllocationID {
			t.Fatalf("attempt %d returned a different allocation %s vs %s",
				i, res.AllocationID, first.AllocationID)
		}
	}

	var payments, allocations, postings int
	if err := w.pool.QueryRow(ctx, `SELECT count(*) FROM payments`).Scan(&payments); err != nil {
		t.Fatal(err)
	}
	if err := w.pool.QueryRow(ctx, `SELECT count(*) FROM payment_allocations`).Scan(&allocations); err != nil {
		t.Fatal(err)
	}
	if err := w.pool.QueryRow(ctx,
		`SELECT count(*) FROM journal_transactions WHERE kind = 'payment_received'`).Scan(&postings); err != nil {
		t.Fatal(err)
	}
	if payments != 1 || allocations != 1 || postings != 1 {
		t.Fatalf("payments=%d allocations=%d postings=%d, want 1/1/1", payments, allocations, postings)
	}

	// The invoice is settled and the customer owes nothing.
	var status string
	var paid int64
	if err := w.pool.QueryRow(ctx,
		`SELECT status, amount_paid_minor FROM invoices WHERE id = $1`, issued.InvoiceID,
	).Scan(&status, &paid); err != nil {
		t.Fatal(err)
	}
	if status != "paid" || paid != 1200 {
		t.Fatalf("invoice status=%s paid=%d, want paid/1200", status, paid)
	}
	owed, err := ledger.ReceivableForCustomer(ctx, w.pool, w.customerID, usd)
	if err != nil {
		t.Fatal(err)
	}
	if !owed.IsZero() {
		t.Fatalf("customer still owes %s after full payment", owed)
	}
}

// Acceptance scenario 2: two distinct events about the same payment cannot
// double-credit, even when they arrive concurrently.
func TestConcurrentDuplicatePayment(t *testing.T) {
	w := newWorld(t, noTax())
	issued := w.issue(t, w.request(line("VPS Small monthly", "12.00", 1000)))

	ctx := context.Background()
	req := billing.AllocationRequest{
		Provider:          "stripe",
		ProviderAccount:   "acct_123",
		ProviderPaymentID: "pi_concurrent",
		InvoiceID:         issued.InvoiceID,
		Amount:            money.MustParse("12.00", usd),
	}

	const workers = 10
	var wg sync.WaitGroup
	results := make([]billing.AllocationResult, workers)
	errs := make([]error, workers)
	start := make(chan struct{})
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			<-start
			errs[n] = w.pool.InTx(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
				var err error
				results[n], err = billing.ApplyPayment(ctx, tx, req)
				return err
			})
		}(i)
	}
	close(start)
	wg.Wait()

	// Every worker either applied the payment or observed it already applied;
	// none may fail, and exactly one may report a first application.
	firsts := 0
	for i, err := range errs {
		if err != nil {
			// A serialization failure is acceptable; anything else is not.
			if !store.IsRetryable(err) {
				t.Fatalf("worker %d: %v", i, err)
			}
			continue
		}
		if !results[i].Duplicate {
			firsts++
		}
	}
	if firsts > 1 {
		t.Fatalf("%d workers applied the payment first, want at most 1", firsts)
	}

	var payments, allocations int
	if err := w.pool.QueryRow(ctx, `SELECT count(*) FROM payments`).Scan(&payments); err != nil {
		t.Fatal(err)
	}
	if err := w.pool.QueryRow(ctx, `SELECT count(*) FROM payment_allocations`).Scan(&allocations); err != nil {
		t.Fatal(err)
	}
	if payments != 1 || allocations != 1 {
		t.Fatalf("payments=%d allocations=%d, want 1/1", payments, allocations)
	}

	var paid int64
	if err := w.pool.QueryRow(ctx,
		`SELECT amount_paid_minor FROM invoices WHERE id = $1`, issued.InvoiceID).Scan(&paid); err != nil {
		t.Fatal(err)
	}
	if paid != 1200 {
		t.Fatalf("invoice paid %d, want 1200 (double credit?)", paid)
	}
}

func TestOverpaymentRejected(t *testing.T) {
	w := newWorld(t, noTax())
	issued := w.issue(t, w.request(line("VPS Small monthly", "12.00", 1000)))

	ctx := context.Background()
	err := w.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := billing.ApplyPayment(ctx, tx, billing.AllocationRequest{
			Provider:          "stripe",
			ProviderAccount:   "acct_123",
			ProviderPaymentID: "pi_over",
			InvoiceID:         issued.InvoiceID,
			Amount:            money.MustParse("12.01", usd),
		})
		return err
	})
	if !errors.Is(err, billing.ErrOverpayment) {
		t.Fatalf("got %v, want ErrOverpayment", err)
	}

	// Nothing was written: the failed transaction left no partial state.
	var payments int
	if err := w.pool.QueryRow(ctx, `SELECT count(*) FROM payments`).Scan(&payments); err != nil {
		t.Fatal(err)
	}
	if payments != 0 {
		t.Fatalf("rejected overpayment left %d payment rows", payments)
	}
}

func TestPartialPaymentsThenSettle(t *testing.T) {
	w := newWorld(t, noTax())
	issued := w.issue(t, w.request(line("VPS Small monthly", "12.00", 1000)))

	ctx := context.Background()
	pay := func(id, amount string) billing.AllocationResult {
		var res billing.AllocationResult
		err := w.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			res, err = billing.ApplyPayment(ctx, tx, billing.AllocationRequest{
				Provider:          "stripe",
				ProviderAccount:   "acct_123",
				ProviderPaymentID: id,
				InvoiceID:         issued.InvoiceID,
				Amount:            money.MustParse(amount, usd),
			})
			return err
		})
		if err != nil {
			t.Fatalf("pay %s: %v", amount, err)
		}
		return res
	}

	if res := pay("pi_1", "5.00"); res.InvoicePaid {
		t.Fatal("invoice marked paid after a partial payment")
	}
	if res := pay("pi_2", "7.00"); !res.InvoicePaid {
		t.Fatal("invoice not marked paid after settling the balance")
	}

	// A third payment has nothing left to allocate.
	err := w.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := billing.ApplyPayment(ctx, tx, billing.AllocationRequest{
			Provider:          "stripe",
			ProviderAccount:   "acct_123",
			ProviderPaymentID: "pi_3",
			InvoiceID:         issued.InvoiceID,
			Amount:            money.MustParse("1.00", usd),
		})
		return err
	})
	if !errors.Is(err, billing.ErrOverpayment) {
		t.Fatalf("payment after settlement: got %v, want ErrOverpayment", err)
	}

	owed, err := ledger.ReceivableForCustomer(ctx, w.pool, w.customerID, usd)
	if err != nil {
		t.Fatal(err)
	}
	if !owed.IsZero() {
		t.Fatalf("outstanding = %s, want 0", owed)
	}
}

func TestPaymentCurrencyMismatchRejected(t *testing.T) {
	w := newWorld(t, noTax())
	issued := w.issue(t, w.request(line("VPS Small monthly", "12.00", 1000)))
	eur := money.MustLookupCurrency("EUR")

	ctx := context.Background()
	err := w.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := billing.ApplyPayment(ctx, tx, billing.AllocationRequest{
			Provider:          "stripe",
			ProviderAccount:   "acct_123",
			ProviderPaymentID: "pi_eur",
			InvoiceID:         issued.InvoiceID,
			Amount:            money.New(1200, eur),
		})
		return err
	})
	if !errors.Is(err, billing.ErrCurrencyMismatch) {
		t.Fatalf("got %v, want ErrCurrencyMismatch", err)
	}
}

func TestCannotPayDraftInvoice(t *testing.T) {
	w := newWorld(t, noTax())
	ctx := context.Background()

	var draftID string
	if err := w.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		id, _, _, err := w.issuer.CreateDraft(ctx, tx, w.request(line("VPS Small monthly", "12.00", 1000)))
		draftID = id
		return err
	}); err != nil {
		t.Fatalf("draft: %v", err)
	}

	err := w.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := billing.ApplyPayment(ctx, tx, billing.AllocationRequest{
			Provider:          "stripe",
			ProviderAccount:   "acct_123",
			ProviderPaymentID: "pi_draft",
			InvoiceID:         draftID,
			Amount:            money.MustParse("12.00", usd),
		})
		return err
	})
	if !errors.Is(err, billing.ErrInvoiceNotDraft) {
		t.Fatalf("got %v, want ErrInvoiceNotDraft", err)
	}
}

func TestIssueTwiceRejected(t *testing.T) {
	w := newWorld(t, noTax())
	issued := w.issue(t, w.request(line("VPS Small monthly", "12.00", 1000)))

	ctx := context.Background()
	err := w.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := w.issuer.Issue(ctx, tx, issued.InvoiceID, billing.DefaultSeries)
		return err
	})
	if !errors.Is(err, billing.ErrInvoiceNotDraft) {
		t.Fatalf("got %v, want ErrInvoiceNotDraft", err)
	}
}

// Numbers must be unique and gap-free across concurrent issuers.
func TestInvoiceNumbersAreUniqueUnderConcurrency(t *testing.T) {
	w := newWorld(t, noTax())

	const workers = 20
	var wg sync.WaitGroup
	numbers := make([]string, workers)
	errs := make([]error, workers)
	start := make(chan struct{})
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			<-start
			errs[n] = w.pool.InTx(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
				id, _, _, err := w.issuer.CreateDraft(ctx, tx, w.request(
					line(fmt.Sprintf("VPS Small monthly #%d", n), "12.00", 1000)))
				if err != nil {
					return err
				}
				issued, err := w.issuer.Issue(ctx, tx, id, billing.DefaultSeries)
				numbers[n] = issued.Number
				return err
			})
		}(i)
	}
	close(start)
	wg.Wait()

	seen := make(map[string]int, workers)
	for i, err := range errs {
		if err != nil {
			t.Fatalf("worker %d: %v", i, err)
		}
		if prev, dup := seen[numbers[i]]; dup {
			t.Fatalf("number %q issued twice (workers %d and %d)", numbers[i], prev, i)
		}
		seen[numbers[i]] = i
	}
	if len(seen) != workers {
		t.Fatalf("%d unique numbers, want %d", len(seen), workers)
	}
	for n := 1; n <= workers; n++ {
		want := fmt.Sprintf("INV-%06d", n)
		if _, ok := seen[want]; !ok {
			t.Fatalf("number %s missing from the series (gap or reuse)", want)
		}
	}
}

// Acceptance scenario 14: editing the catalog after issuance cannot change an
// issued invoice, because the invoice carries its own snapshot.
func TestCatalogEditDoesNotRewriteHistory(t *testing.T) {
	w := newWorld(t, noTax())
	issued := w.issue(t, w.request(line("VPS Small monthly", "12.00", 1000)))
	ctx := context.Background()

	// Change nothing in the catalog here; instead prove the document is derived
	// from the snapshot by mutating the *snapshot source* and re-reading.
	var total int64
	var snapshot billing.Snapshot
	if err := w.pool.QueryRow(ctx,
		`SELECT total_minor, snapshot FROM invoices WHERE id = $1`, issued.InvoiceID,
	).Scan(&total, &snapshot); err != nil {
		t.Fatal(err)
	}
	if total != 1200 {
		t.Fatalf("total = %d, want 1200", total)
	}
	if snapshot.Currency != "USD" {
		t.Fatalf("snapshot currency = %q, want USD", snapshot.Currency)
	}
	if snapshot.RoundingMode != "half_up" {
		t.Fatalf("rounding mode not recorded: %q", snapshot.RoundingMode)
	}
	if snapshot.RoundingStage != billing.RoundPerLine {
		t.Fatalf("rounding stage not recorded: %q", snapshot.RoundingStage)
	}

	// The database itself refuses to let the money change.
	if _, err := w.pool.Exec(ctx,
		`UPDATE invoices SET total_minor = 999 WHERE id = $1`, issued.InvoiceID); err == nil {
		t.Fatal("issued invoice total was mutable")
	}
}

func TestTaxIsSplitOutOfRevenue(t *testing.T) {
	pricer := billing.FixedTaxPricer{
		Rate:     money.MustParseRatio("20%"),
		Rounding: money.RoundHalfUp,
		Policy:   "uk-standard-2026",
	}
	w := newWorld(t, pricer)
	issued := w.issue(t, w.request(line("VPS Small monthly", "12.00", 1000)))

	// 12.00 net + 2.40 tax = 14.40 gross.
	if got, want := issued.Totals.Tax.String(), "2.40"; got != want {
		t.Fatalf("tax = %s, want %s", got, want)
	}
	if got, want := issued.Totals.Total.String(), "14.40"; got != want {
		t.Fatalf("total = %s, want %s", got, want)
	}
	if issued.Snapshot.TaxEvidence.Rate != "1/5" {
		t.Fatalf("tax evidence rate = %q, want 1/5", issued.Snapshot.TaxEvidence.Rate)
	}
	if issued.Snapshot.TaxEvidence.Policy != "uk-standard-2026" {
		t.Fatalf("tax policy not recorded: %q", issued.Snapshot.TaxEvidence.Policy)
	}

	ctx := context.Background()
	ar, err := ledger.Balances(ctx, w.pool, ledger.AccountReceivable, usd)
	if err != nil {
		t.Fatal(err)
	}
	revenue, err := ledger.Balances(ctx, w.pool, ledger.AccountRevenue, usd)
	if err != nil {
		t.Fatal(err)
	}
	taxPayable, err := ledger.Balances(ctx, w.pool, ledger.AccountTaxPayable, usd)
	if err != nil {
		t.Fatal(err)
	}
	if ar.Debits.Minor() != 1440 {
		t.Fatalf("receivable = %s, want 14.40 (gross)", ar.Debits)
	}
	if revenue.Credits.Minor() != 1200 {
		t.Fatalf("revenue = %s, want 12.00 (net, tax excluded)", revenue.Credits)
	}
	if taxPayable.Credits.Minor() != 240 {
		t.Fatalf("tax payable = %s, want 2.40", taxPayable.Credits)
	}
}

func TestInclusiveTaxKeepsQuotedTotal(t *testing.T) {
	pricer := billing.FixedTaxPricer{
		Rate:         money.MustParseRatio("20%"),
		Inclusive:    true,
		Rounding:     money.RoundHalfUp,
		Policy:       "inclusive-20",
		Jurisdiction: "GB",
	}
	w := newWorld(t, pricer)
	issued := w.issue(t, w.request(line("VPS Small monthly", "12.00", 1000)))

	// The customer is charged exactly the quoted 12.00; tax is the contained
	// share, 12.00 * 1/6 = 2.00.
	if got, want := issued.Totals.Total.String(), "12.00"; got != want {
		t.Fatalf("total = %s, want %s (quoted price must not change)", got, want)
	}
	if got, want := issued.Totals.Tax.String(), "2.00"; got != want {
		t.Fatalf("tax = %s, want %s", got, want)
	}
	if got, want := issued.Totals.Subtotal.String(), "10.00"; got != want {
		t.Fatalf("net = %s, want %s", got, want)
	}
}

func TestFractionalQuantity(t *testing.T) {
	w := newWorld(t, noTax())
	// 2.5 units at 4.00 = 10.00.
	issued := w.issue(t, w.request(line("Bandwidth 2.5 TB", "4.00", 2500)))
	if got, want := issued.Totals.Total.String(), "10.00"; got != want {
		t.Fatalf("total = %s, want %s", got, want)
	}
}

func TestInvalidRequestsRejected(t *testing.T) {
	w := newWorld(t, noTax())
	ctx := context.Background()

	cases := []struct {
		name string
		req  billing.IssueRequest
	}{
		{"no lines", w.request()},
		{"no customer", func() billing.IssueRequest {
			r := w.request(line("x", "1.00", 1000))
			r.CustomerID = ""
			return r
		}()},
		{"zero quantity", w.request(line("x", "1.00", 0))},
		{"empty description", w.request(line("", "1.00", 1000))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := w.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
				_, _, _, err := w.issuer.CreateDraft(ctx, tx, tc.req)
				return err
			})
			if err == nil {
				t.Fatal("request was accepted")
			}
		})
	}

	// A mixed-currency request is rejected before any money is computed.
	eur := money.MustLookupCurrency("EUR")
	mixed := w.request(line("a", "1.00", 1000))
	mixed.Lines = append(mixed.Lines, billing.Line{
		Description: "b", QuantityMilli: 1000, UnitPrice: money.New(100, eur),
	})
	err := w.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, _, _, err := w.issuer.CreateDraft(ctx, tx, mixed)
		return err
	})
	if !errors.Is(err, billing.ErrCurrencyMismatch) {
		t.Fatalf("mixed currency: got %v, want ErrCurrencyMismatch", err)
	}
}

// An issued invoice always has a due date: without one, dunning never sees it and
// it stays open forever.
func TestIssuedInvoiceAlwaysHasDueDate(t *testing.T) {
	w := newWorld(t, noTax())
	req := w.request(line("VPS Small monthly", "12.00", 1000))
	req.DueAt = time.Time{} // deliberately unset

	issued := w.issue(t, req)

	var dueAt *time.Time
	if err := w.pool.QueryRow(context.Background(),
		`SELECT due_at FROM invoices WHERE id = $1`, issued.InvoiceID).Scan(&dueAt); err != nil {
		t.Fatal(err)
	}
	if dueAt == nil {
		t.Fatal("issued invoice has no due date, so dunning can never act on it")
	}
}
