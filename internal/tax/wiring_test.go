package tax_test

// Wiring tests: the contract between the cart and the immutable invoice snapshot.
//
// The cart prices through `Rules.PricerForCustomer`; every production issuance path now
// prices through `billing.Issuer` configured with `Rules.ResolvePricer`. These tests pin
// the property the two paths must share: for the same customer at the same instant, the
// cart total and the invoice total are the same number, and the snapshot records the same
// rate, mode and exemption reason the cart disclosed. They also pin the refusal path: an
// undecidable supply stops issuance instead of producing a zero-rated document.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/catalyst/billing/internal/billing"
	"github.com/catalyst/billing/internal/commerce"
	"github.com/catalyst/billing/internal/money"
	"github.com/catalyst/billing/internal/tax"
	"github.com/jackc/pgx/v5"
)

func (e *rulesEnv) invoiceIssuer() *billing.Issuer {
	// The same fallback the server constructs: tax switched off is the shipped no-tax
	// policy, and the resolver overrides it only when tax is enabled and a rate resolves.
	return billing.NewIssuer(billing.FixedTaxPricer{Policy: "no-tax", Jurisdiction: "none"}).
		WithResolver(e.rules)
}

func (e *rulesEnv) cartAndInvoice(t *testing.T, customerID string, unitMinor int64) (
	cartTotals, invoiceTotals billing.Totals, snap billing.Snapshot, invoiceID string) {
	t.Helper()
	ctx := context.Background()
	cur := money.MustLookupCurrency("EUR")
	lines := []billing.Line{{
		Description: "Hosting", QuantityMilli: 1000, UnitPrice: money.New(unitMinor, cur),
	}}

	cartPricer, err := e.rules.PricerForCustomer(ctx, customerID, rulesNow)
	if err != nil {
		t.Fatalf("cart pricer: %v", err)
	}
	if cartPricer == nil {
		t.Fatal("tax is enabled but the cart got no pricer")
	}
	_, cartTotals, _, err = cartPricer.Price(ctx, lines, cur)
	if err != nil {
		t.Fatalf("cart price: %v", err)
	}

	err = e.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		issuer := e.invoiceIssuer()
		var err error
		invoiceID, invoiceTotals, snap, err = issuer.CreateDraft(ctx, tx, billing.IssueRequest{
			CustomerID: customerID,
			Lines:      lines,
			Merchant:   billing.Merchant{Name: "Catalyst Hosting", CountryCode: "DE"},
			Customer:   billing.CustomerDetails{ID: customerID},
			At:         rulesNow,
		})
		return err
	})
	if err != nil {
		t.Fatalf("issuer draft: %v", err)
	}
	return cartTotals, invoiceTotals, snap, invoiceID
}

// Inclusive tax: the quoted price contains the tax, and the cart and the invoice agree on
// both the amount and the evidence. A regression that made the issuer resolve outside the
// transaction, or use a different pricer, would show up as a mismatch here.
func TestCartAndInvoiceAgreeOnInclusiveTax(t *testing.T) {
	env := newRulesEnv(t)
	env.set(t, "enabled", "true")
	env.rate(t, "DE", "", "standard", "0.19", true)
	customerID := env.customer(t, "DE", "", "")

	cart, invoice, snap, invoiceID := env.cartAndInvoice(t, customerID, 10000)
	// 19% inclusive on 10000: 10000*19/119 = 1596.63.., rounded once per line to 1597.
	if cart.Tax.Minor() != 1597 || cart.Total.Minor() != 10000 {
		t.Fatalf("cart = tax %d total %d, want 1597 / 10000", cart.Tax.Minor(), cart.Total.Minor())
	}
	if invoice.Tax.Minor() != cart.Tax.Minor() || invoice.Total.Minor() != cart.Total.Minor() {
		t.Fatalf("invoice = tax %d total %d, cart = tax %d total %d",
			invoice.Tax.Minor(), invoice.Total.Minor(), cart.Tax.Minor(), cart.Total.Minor())
	}
	if snap.TaxEvidence.Mode != "inclusive" {
		t.Fatalf("evidence mode = %q, want inclusive", snap.TaxEvidence.Mode)
	}
	if snap.TaxEvidence.Jurisdiction != "DE/standard" {
		t.Fatalf("evidence jurisdiction = %q, want DE/standard", snap.TaxEvidence.Jurisdiction)
	}
	if invoiceID == "" {
		t.Fatal("no invoice was written")
	}
}

// Exclusive tax: tax is added to the net and the customer pays net+tax, again identical on
// both paths.
func TestCartAndInvoiceAgreeOnExclusiveTax(t *testing.T) {
	env := newRulesEnv(t)
	env.set(t, "enabled", "true")
	env.rate(t, "US", "CA", "standard", "0.0725", false)
	customerID := env.customer(t, "US", "CA", "")

	cart, invoice, snap, _ := env.cartAndInvoice(t, customerID, 10000)
	// 7.25% of 10000 = 725 exactly.
	if cart.Tax.Minor() != 725 || cart.Total.Minor() != 10725 {
		t.Fatalf("cart = tax %d total %d, want 725 / 10725", cart.Tax.Minor(), cart.Total.Minor())
	}
	if invoice.Tax.Minor() != cart.Tax.Minor() || invoice.Total.Minor() != cart.Total.Minor() {
		t.Fatalf("invoice = tax %d total %d, cart = tax %d total %d",
			invoice.Tax.Minor(), invoice.Total.Minor(), cart.Tax.Minor(), cart.Total.Minor())
	}
	if snap.TaxEvidence.Mode != "exclusive" {
		t.Fatalf("evidence mode = %q, want exclusive", snap.TaxEvidence.Mode)
	}
	if snap.TaxEvidence.Jurisdiction != "US-CA/standard" {
		t.Fatalf("evidence jurisdiction = %q, want US-CA/standard", snap.TaxEvidence.Jurisdiction)
	}
}

// A staff exemption survives the wiring: the invoice charges nothing and keeps the reason.
func TestInvoicePreservesStaffExemption(t *testing.T) {
	env := newRulesEnv(t)
	env.set(t, "enabled", "true")
	env.rate(t, "DE", "", "standard", "0.19", true)
	customerID := env.customer(t, "DE", "", "")
	if _, err := env.pool.Exec(context.Background(), `
		UPDATE billing_profiles
		   SET tax_exempt = true, tax_exemption_reason = 'Registered charity'
		 WHERE customer_id = $1`, customerID); err != nil {
		t.Fatalf("exempt profile: %v", err)
	}

	cart, invoice, snap, _ := env.cartAndInvoice(t, customerID, 10000)
	if cart.Tax.Minor() != 0 || invoice.Tax.Minor() != 0 {
		t.Fatalf("tax = cart %d invoice %d, want zero", cart.Tax.Minor(), invoice.Tax.Minor())
	}
	if invoice.Total.Minor() != 10000 {
		t.Fatalf("exempt total = %d, want the quoted 10000", invoice.Total.Minor())
	}
	if snap.TaxEvidence.Policy != tax.PolicyStaffExempt {
		t.Fatalf("policy = %q, want %s", snap.TaxEvidence.Policy, tax.PolicyStaffExempt)
	}
	if snap.TaxEvidence.ExemptionReason != "Registered charity" {
		t.Fatalf("exemption reason = %q, want the staff reason", snap.TaxEvidence.ExemptionReason)
	}
}

// Refusal: a VAT ID that cannot be validated stops the invoice. The test asserts no invoice
// row exists, because "charged zero because we could not check" is the liability this rule
// exists to prevent.
func TestInvoiceRefusesAnUnverifiableSupply(t *testing.T) {
	env := newRulesEnv(t)
	env.set(t, "enabled", "true")
	env.set(t, "seller_country", "DE")
	env.set(t, "reverse_charge", "true")
	env.set(t, "vat_validation_url", "")
	env.rate(t, "FR", "", "standard", "0.2", false)
	customerID := env.customer(t, "FR", "", "FR40303265045")

	ctx := context.Background()
	cur := money.MustLookupCurrency("EUR")
	err := env.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, _, _, err := env.invoiceIssuer().CreateDraft(ctx, tx, billing.IssueRequest{
			CustomerID: customerID,
			Lines: []billing.Line{{
				Description: "Hosting", QuantityMilli: 1000, UnitPrice: money.New(10000, cur),
			}},
			Merchant: billing.Merchant{Name: "Catalyst Hosting", CountryCode: "DE"},
			Customer: billing.CustomerDetails{ID: customerID},
			At:       rulesNow,
		})
		return err
	})
	var refusal *tax.Refusal
	if !errors.As(err, &refusal) {
		t.Fatalf("err = %v, want a *tax.Refusal", err)
	}
	if !errors.Is(err, tax.ErrRefused) {
		t.Fatal("refusal must be recognisable with errors.Is(err, tax.ErrRefused)")
	}
	if refusal.Code != tax.CodeVATIDUnverified {
		t.Fatalf("refusal code = %q, want %s", refusal.Code, tax.CodeVATIDUnverified)
	}
	var invoices int
	if err := env.pool.QueryRow(ctx,
		`SELECT count(*) FROM invoices WHERE customer_id = $1`, customerID).Scan(&invoices); err != nil {
		t.Fatal(err)
	}
	if invoices != 0 {
		t.Fatalf("invoices = %d after a refusal, want 0", invoices)
	}
}

// The resolver must use the caller's transaction, not the pool. This proves it by making
// the pool path unreadable inside the transaction: the rate is inserted in the same tx and
// is visible to the resolver only if it uses that tx.
func TestResolvePricerUsesTheCallersTransaction(t *testing.T) {
	env := newRulesEnv(t)
	env.set(t, "enabled", "true")
	customerID := env.customer(t, "DE", "", "")

	ctx := context.Background()
	cur := money.MustLookupCurrency("EUR")
	err := env.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		// This rate is uncommitted. A resolver reading the pool cannot see it and would
		// return the no-rate decision; one reading tx resolves it.
		from := rulesNow.Add(-2 * time.Hour)
		if _, err := env.store.CreateTaxRateTx(ctx, tx, commerce.TaxRateInput{
			Name: "Uncommitted German VAT", CountryCode: "DE", TaxClass: "standard",
			Rate: "0.19", EffectiveFrom: &from,
		}); err != nil {
			return err
		}
		issuer := env.invoiceIssuer()
		_, totals, snap, err := issuer.CreateDraft(ctx, tx, billing.IssueRequest{
			CustomerID: customerID,
			Lines: []billing.Line{{
				Description: "Hosting", QuantityMilli: 1000, UnitPrice: money.New(10000, cur),
			}},
			Merchant: billing.Merchant{Name: "Catalyst Hosting", CountryCode: "DE"},
			Customer: billing.CustomerDetails{ID: customerID},
			At:       rulesNow,
		})
		if err != nil {
			return err
		}
		if totals.Tax.Minor() != 1900 {
			t.Fatalf("tax = %d, want 1900 from the uncommitted rate", totals.Tax.Minor())
		}
		if snap.TaxEvidence.Jurisdiction != "DE/standard" {
			t.Fatalf("jurisdiction = %q, want DE/standard", snap.TaxEvidence.Jurisdiction)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("in-transaction issuance: %v", err)
	}
}
