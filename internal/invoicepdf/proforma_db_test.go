package invoicepdf_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/catalyst/billing/internal/billing"
	"github.com/catalyst/billing/internal/invoicepdf"
	"github.com/catalyst/billing/internal/money"
	"github.com/catalyst/billing/internal/store"
	"github.com/catalyst/billing/internal/store/storetest"
	"github.com/jackc/pgx/v5"
)

var usdLifecycle = money.MustLookupCurrency("USD")

// draftInvoice creates a draft invoice through the issuer, so the row is exactly
// what the issue path produces: a priced snapshot and no number.
func draftInvoice(t *testing.T, pool *store.Pool) string {
	t.Helper()
	ctx := context.Background()
	var invoiceID string
	err := pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var customerID string
		if err := tx.QueryRow(ctx,
			`INSERT INTO customers (email, name) VALUES ($1, 'Ada') RETURNING id`,
			"proforma-"+time.Now().Format("150405.000000000")+"@example.test").
			Scan(&customerID); err != nil {
			return err
		}

		issuer := billing.NewIssuer(billing.FixedTaxPricer{
			Rate: money.MustParseRatio("19%"), Rounding: money.RoundHalfUp,
			Policy: "test", Jurisdiction: "DE",
		})
		id, _, _, err := issuer.CreateDraft(ctx, tx, billing.IssueRequest{
			CustomerID: customerID,
			Lines: []billing.Line{{
				Description: "Monthly hosting", QuantityMilli: 1000,
				UnitPrice: money.New(1200, usdLifecycle),
			}},
			Merchant: billing.Merchant{Name: "Catalyst Hosting GmbH", CountryCode: "DE"},
			Customer: billing.CustomerDetails{ID: customerID, Name: "Ada"},
			Terms:    "Payable within 7 days.",
		})
		if err != nil {
			return err
		}
		invoiceID = id
		return nil
	})
	if err != nil {
		t.Fatalf("create draft: %v", err)
	}
	return invoiceID
}

// A proforma is issued, payable and unnumbered; conversion assigns exactly one
// number and posts nothing.
func TestProformaLifecycle(t *testing.T) {
	pool := storetest.DB(t)
	ctx := context.Background()
	invoiceID := draftInvoice(t, pool)

	due := time.Now().UTC().AddDate(0, 0, 7)
	if err := pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return invoicepdf.IssueProforma(ctx, tx, invoiceID, due)
	}); err != nil {
		t.Fatalf("issue proforma: %v", err)
	}

	state, err := invoicepdf.ReadProformaState(ctx, pool, invoiceID)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	if !state.Proforma {
		t.Fatal("the invoice is not marked provisional")
	}
	if state.Number != "" {
		t.Fatalf("a proforma has a number: %q", state.Number)
	}
	if state.Status != "open" {
		t.Fatalf("status = %q, want open: a proforma is payable", state.Status)
	}

	// Issuance posted the receivable, so the customer owes the money now.
	var posted int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM journal_transactions
		WHERE reference_type = 'invoice' AND reference_id = $1`, invoiceID).Scan(&posted); err != nil {
		t.Fatalf("count postings: %v", err)
	}
	if posted != 1 {
		t.Fatalf("%d postings for the proforma, want 1", posted)
	}

	// Conversion.
	var converted invoicepdf.Converted
	if err := pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		converted, err = invoicepdf.ConvertProforma(ctx, tx, invoiceID, billing.DefaultSeries)
		return err
	}); err != nil {
		t.Fatalf("convert: %v", err)
	}
	if converted.Number == "" {
		t.Fatal("conversion did not assign a number")
	}
	if converted.InvoiceID != invoiceID {
		t.Fatalf("converted id = %q", converted.InvoiceID)
	}

	state, err = invoicepdf.ReadProformaState(ctx, pool, invoiceID)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	if state.Proforma {
		t.Fatal("the invoice is still provisional after conversion")
	}
	if state.Number != converted.Number {
		t.Fatalf("stored number %q != returned %q", state.Number, converted.Number)
	}

	// Conversion posts nothing: the receivable was already there.
	var after int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM journal_transactions
		WHERE reference_type = 'invoice' AND reference_id = $1`, invoiceID).Scan(&after); err != nil {
		t.Fatalf("count postings: %v", err)
	}
	if after != posted {
		t.Fatalf("conversion created a posting: %d -> %d", posted, after)
	}
}

// Converting twice must not burn a second number. A replayed payment is refused
// loudly rather than silently renumbering a document the customer already holds.
func TestConvertProformaIsNotRepeatable(t *testing.T) {
	pool := storetest.DB(t)
	ctx := context.Background()
	invoiceID := draftInvoice(t, pool)
	due := time.Now().UTC().AddDate(0, 0, 7)

	if err := pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return invoicepdf.IssueProforma(ctx, tx, invoiceID, due)
	}); err != nil {
		t.Fatalf("issue proforma: %v", err)
	}
	var first invoicepdf.Converted
	if err := pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		first, err = invoicepdf.ConvertProforma(ctx, tx, invoiceID, billing.DefaultSeries)
		return err
	}); err != nil {
		t.Fatalf("first convert: %v", err)
	}

	err := pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := invoicepdf.ConvertProforma(ctx, tx, invoiceID, billing.DefaultSeries)
		return err
	})
	if !errors.Is(err, invoicepdf.ErrNotProforma) {
		t.Fatalf("second convert: %v, want ErrNotProforma", err)
	}

	// The number is unchanged: no second allocation happened.
	state, err := invoicepdf.ReadProformaState(ctx, pool, invoiceID)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	if state.Number != first.Number {
		t.Fatalf("the number changed on a repeated conversion: %q -> %q", first.Number, state.Number)
	}
}

// A numbered invoice is not convertible: that would be a renumbering, which is a
// forgery rather than a conversion.
func TestConvertRefusesNonProforma(t *testing.T) {
	pool := storetest.DB(t)
	ctx := context.Background()
	invoiceID := draftInvoice(t, pool)

	issuer := billing.NewIssuer(billing.FixedTaxPricer{
		Rate: money.MustParseRatio("0%"), Rounding: money.RoundHalfUp, Policy: "none",
	})
	if err := pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := issuer.Issue(ctx, tx, invoiceID, billing.DefaultSeries)
		return err
	}); err != nil {
		t.Fatalf("issue: %v", err)
	}

	err := pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := invoicepdf.ConvertProforma(ctx, tx, invoiceID, billing.DefaultSeries)
		return err
	})
	if !errors.Is(err, invoicepdf.ErrNotProforma) {
		t.Fatalf("converting a real invoice: %v, want ErrNotProforma", err)
	}
}

// Issuing a proforma twice is refused, and a non-draft is refused.
func TestIssueProformaRefusesNonDraft(t *testing.T) {
	pool := storetest.DB(t)
	ctx := context.Background()
	invoiceID := draftInvoice(t, pool)
	due := time.Now().UTC().AddDate(0, 0, 7)

	if err := pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return invoicepdf.IssueProforma(ctx, tx, invoiceID, due)
	}); err != nil {
		t.Fatalf("first issue: %v", err)
	}
	err := pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return invoicepdf.IssueProforma(ctx, tx, invoiceID, due)
	})
	if !errors.Is(err, billing.ErrInvoiceNotDraft) {
		t.Fatalf("second issue: %v, want ErrInvoiceNotDraft", err)
	}
}

// The payment-path helper must be a no-op for an ordinary invoice, so the caller
// can invoke it unconditionally after a settlement.
func TestConvertOnPaymentIsNoOpForRealInvoice(t *testing.T) {
	pool := storetest.DB(t)
	ctx := context.Background()
	invoiceID := draftInvoice(t, pool)

	issuer := billing.NewIssuer(billing.FixedTaxPricer{
		Rate: money.MustParseRatio("0%"), Rounding: money.RoundHalfUp, Policy: "none",
	})
	if err := pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := issuer.Issue(ctx, tx, invoiceID, billing.DefaultSeries)
		return err
	}); err != nil {
		t.Fatalf("issue: %v", err)
	}

	var out invoicepdf.Converted
	if err := pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = invoicepdf.ConvertProformaOnPayment(ctx, tx, invoiceID, billing.DefaultSeries)
		return err
	}); err != nil {
		t.Fatalf("no-op conversion returned an error: %v", err)
	}
	if out != (invoicepdf.Converted{}) {
		t.Fatalf("no-op conversion reported %+v", out)
	}
}

// A void proforma must not consume a number: nobody can pay a void document, so a
// number attached to one would be a permanent gap in the series that means nothing.
func TestConvertRefusesVoidProforma(t *testing.T) {
	pool := storetest.DB(t)
	ctx := context.Background()
	invoiceID := draftInvoice(t, pool)
	due := time.Now().UTC().AddDate(0, 0, 7)

	if err := pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return invoicepdf.IssueProforma(ctx, tx, invoiceID, due)
	}); err != nil {
		t.Fatalf("issue proforma: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE invoices SET status = 'void', void_reason = 'raised in error' WHERE id = $1`,
		invoiceID); err != nil {
		t.Fatalf("void: %v", err)
	}

	err := pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := invoicepdf.ConvertProforma(ctx, tx, invoiceID, billing.DefaultSeries)
		return err
	})
	if !errors.Is(err, invoicepdf.ErrProformaNotConvertible) {
		t.Fatalf("converting a void proforma: %v, want ErrProformaNotConvertible", err)
	}
}

// The schema must keep its promise that a proforma carries no number, even if a
// caller tries to write one directly.
func TestProformaCannotCarryNumberAtTheSchema(t *testing.T) {
	pool := storetest.DB(t)
	ctx := context.Background()
	invoiceID := draftInvoice(t, pool)
	due := time.Now().UTC().AddDate(0, 0, 7)

	if err := pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return invoicepdf.IssueProforma(ctx, tx, invoiceID, due)
	}); err != nil {
		t.Fatalf("issue proforma: %v", err)
	}

	_, err := pool.Exec(ctx, `UPDATE invoices SET number = 'INV-999999' WHERE id = $1`, invoiceID)
	if err == nil {
		t.Fatal("a numbered proforma was accepted; the CHECK did not hold")
	}
}

// The payment path settles the invoice and then converts it, so a `paid` proforma
// must be convertible. This is the primary path: refusing it would leave every
// settled proforma unnumbered.
func TestConvertProformaAfterSettlement(t *testing.T) {
	pool := storetest.DB(t)
	ctx := context.Background()
	invoiceID := draftInvoice(t, pool)
	due := time.Now().UTC().AddDate(0, 0, 7)

	if err := pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return invoicepdf.IssueProforma(ctx, tx, invoiceID, due)
	}); err != nil {
		t.Fatalf("issue proforma: %v", err)
	}

	// Settle it, as billing.ApplyPayment does.
	if _, err := pool.Exec(ctx, `
		UPDATE invoices SET amount_paid_minor = total_minor, status = 'paid' WHERE id = $1`,
		invoiceID); err != nil {
		t.Fatalf("settle: %v", err)
	}

	var out invoicepdf.Converted
	if err := pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = invoicepdf.ConvertProformaOnPayment(ctx, tx, invoiceID, billing.DefaultSeries)
		return err
	}); err != nil {
		t.Fatalf("convert after settlement: %v", err)
	}
	if out.Number == "" {
		t.Fatal("a settled proforma was left without a number")
	}

	state, err := invoicepdf.ReadProformaState(ctx, pool, invoiceID)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	if state.Proforma || state.Number != out.Number || state.Status != "paid" {
		t.Fatalf("state after conversion = %+v", state)
	}
}

// An uncollectible proforma is also refused: like a void one, nobody will pay it,
// so a number would be a meaningless gap.
func TestConvertRefusesUncollectibleProforma(t *testing.T) {
	pool := storetest.DB(t)
	ctx := context.Background()
	invoiceID := draftInvoice(t, pool)
	due := time.Now().UTC().AddDate(0, 0, 7)

	if err := pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return invoicepdf.IssueProforma(ctx, tx, invoiceID, due)
	}); err != nil {
		t.Fatalf("issue proforma: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE invoices SET status = 'uncollectible' WHERE id = $1`, invoiceID); err != nil {
		t.Fatalf("mark uncollectible: %v", err)
	}
	err := pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := invoicepdf.ConvertProforma(ctx, tx, invoiceID, billing.DefaultSeries)
		return err
	})
	if !errors.Is(err, invoicepdf.ErrProformaNotConvertible) {
		t.Fatalf("converting an uncollectible proforma: %v", err)
	}
}
