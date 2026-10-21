package billing_test

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"testing"

	"github.com/catalyst/billing/internal/billing"
	"github.com/catalyst/billing/internal/ledger"
	"github.com/catalyst/billing/internal/money"
	"github.com/catalyst/billing/internal/store"
	"github.com/catalyst/billing/internal/store/storetest"
	"github.com/jackc/pgx/v5"
)

// TestLedgerInvariantsUnderRandomPayments is the property test the plan asks for
// under "property-based tests cover ... journal balancing".
//
// It drives random sequences of real operations — issue, partial payment,
// duplicate provider id, overpayment — and after every step asserts the
// invariants that must never be false:
//
//  1. every journal transaction balances, in one currency;
//  2. the ledger as a whole sums to zero per currency;
//  3. no invoice is ever paid beyond its total;
//  4. the sum of allocations equals the invoice's paid amount;
//  5. the customer's receivable equals the unpaid total of their invoices.
//
// Invariants are checked against the database, not against the code's own
// bookkeeping, so a bug that is internally consistent is still caught.
func TestLedgerInvariantsUnderRandomPayments(t *testing.T) {
	pool := storetest.DB(t)
	ctx := context.Background()

	var customerID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO customers (email, name) VALUES ('prop@example.test', 'Prop') RETURNING id`,
	).Scan(&customerID); err != nil {
		t.Fatal(err)
	}

	noTaxIssuer := billing.NewIssuer(billing.FixedTaxPricer{
		Rate: money.MustParseRatio("0%"), Rounding: money.RoundHalfUp, Policy: "none",
	})
	vatIssuer := billing.NewIssuer(billing.FixedTaxPricer{
		Rate: money.MustParseRatio("20%"), Rounding: money.RoundHalfUp, Policy: "vat",
	})

	rng := rand.New(rand.NewSource(20261021))
	expectedPaid := make(map[string]int64)
	expectedTotal := make(map[string]int64)

	for step := 0; step < 60; step++ {
		// Occasionally issue a new invoice.
		if step%4 == 0 {
			amount := int64(1 + rng.Intn(20000))
			useVAT := rng.Intn(2) == 0
			var invoiceID string
			err := pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
				iss := noTaxIssuer
				if useVAT {
					iss = vatIssuer
				}
				id, totals, _, err := iss.CreateDraft(ctx, tx, billing.IssueRequest{
					CustomerID: customerID,
					Lines: []billing.Line{{
						Description: "line", QuantityMilli: 1000,
						UnitPrice: money.New(amount, usd),
					}},
					Customer: billing.CustomerDetails{ID: customerID},
				})
				if err != nil {
					return err
				}
				issued, err := iss.Issue(ctx, tx, id, billing.DefaultSeries)
				if err != nil {
					return err
				}
				invoiceID = issued.InvoiceID
				expectedTotal[invoiceID] = totals.Total.Minor()
				return nil
			})
			if err != nil {
				t.Fatalf("step %d: issue: %v", step, err)
			}
			continue
		}

		// Pay something: a random open invoice, a random amount, sometimes a
		// reused provider id, sometimes an overpayment.
		openIDs, err := openInvoiceIDs(ctx, pool)
		if err != nil {
			t.Fatalf("step %d: %v", step, err)
		}
		if len(openIDs) == 0 {
			continue
		}

		invoiceID := openIDs[rng.Intn(len(openIDs))]
		outstanding := expectedTotal[invoiceID] - expectedPaid[invoiceID]
		if outstanding <= 0 {
			continue
		}

		var amount int64
		overpay := false
		switch rng.Intn(4) {
		case 0:
			amount = outstanding // exact settlement
		case 1:
			amount = 1 + rng.Int63n(outstanding) // partial
		case 2:
			amount = outstanding + 1 + rng.Int63n(100) // must be refused
			overpay = true
		default:
			amount = 1 + rng.Int63n(outstanding)
		}

		// Reuse a previous provider id sometimes, to exercise duplicate handling.
		providerID := fmt.Sprintf("pi_%d", step)
		if rng.Intn(5) == 0 && step > 4 {
			providerID = fmt.Sprintf("pi_%d", step-4)
		}

		var applied billing.AllocationResult
		payErr := pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			applied, err = billing.ApplyPayment(ctx, tx, billing.AllocationRequest{
				Provider:          "stripe",
				ProviderAccount:   "acct",
				ProviderPaymentID: providerID,
				InvoiceID:         invoiceID,
				Amount:            money.New(amount, usd),
			})
			return err
		})

		switch {
		case overpay:
			if payErr == nil {
				t.Fatalf("step %d: overpayment of %d against %d outstanding was accepted",
					step, amount, outstanding)
			}
			if !strings.Contains(payErr.Error(), "exceeds the amount due") {
				t.Fatalf("step %d: overpayment failed with %v, want an overpayment error", step, payErr)
			}
		case payErr == nil && !applied.Duplicate:
			expectedPaid[invoiceID] += amount
			if expectedPaid[invoiceID] > expectedTotal[invoiceID] {
				expectedPaid[invoiceID] = expectedTotal[invoiceID]
			}
		case payErr == nil && applied.Duplicate:
			// Already applied: nothing changes, which is the point.
		default:
			// A reused provider id presented for another invoice, or with a
			// different amount, is refused. That is expected behaviour, not a
			// defect, and it is exactly the money-creation hole this test hunts.
			msg := payErr.Error()
			if !strings.Contains(msg, "exceeds the amount due") &&
				!strings.Contains(msg, "already applied") {
				t.Fatalf("step %d: payment failed unexpectedly: %v", step, payErr)
			}
		}

		if err := checkInvariants(ctx, pool, customerID, expectedPaid); err != nil {
			t.Fatalf("step %d: %v", step, err)
		}
	}

	if err := checkInvariants(ctx, pool, customerID, expectedPaid); err != nil {
		t.Fatalf("final: %v", err)
	}
}

func openInvoiceIDs(ctx context.Context, pool *store.Pool) ([]string, error) {
	rows, err := pool.Query(ctx, `SELECT id FROM invoices WHERE status = 'open' ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// checkInvariants verifies the five properties against the database itself.
func checkInvariants(ctx context.Context, pool *store.Pool, customerID string, expectedPaid map[string]int64) error {
	// 1. Every journal transaction balances, in one currency.
	rows, err := pool.Query(ctx, `
		SELECT t.id,
		       COALESCE(SUM(e.debit_minor), 0),
		       COALESCE(SUM(e.credit_minor), 0),
		       COUNT(DISTINCT e.currency)
		FROM journal_transactions t
		JOIN journal_entries e ON e.transaction_id = t.id
		GROUP BY t.id`)
	if err != nil {
		return fmt.Errorf("query journal transactions: %w", err)
	}
	for rows.Next() {
		var id string
		var debits, credits int64
		var currencies int
		if err := rows.Scan(&id, &debits, &credits, &currencies); err != nil {
			rows.Close()
			return err
		}
		if debits != credits {
			rows.Close()
			return fmt.Errorf("journal transaction %s is unbalanced: %d vs %d", id, debits, credits)
		}
		if currencies > 1 {
			rows.Close()
			return fmt.Errorf("journal transaction %s mixes currencies", id)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	// 2. The ledger sums to zero per currency.
	balances, err := ledger.TrialBalance(ctx, pool)
	if err != nil {
		return err
	}
	for code, net := range balances {
		if !net.IsZero() {
			return fmt.Errorf("trial balance for %s is %s, want 0", code, net)
		}
	}

	// 3 and 4: no overpayment, and allocations agree with the paid amount.
	rows, err = pool.Query(ctx, `
		SELECT i.id, i.total_minor, i.amount_paid_minor,
		       COALESCE((SELECT SUM(a.amount_minor) FROM payment_allocations a WHERE a.invoice_id = i.id), 0)
		FROM invoices i`)
	if err != nil {
		return fmt.Errorf("query invoices: %w", err)
	}
	var unpaidTotal int64
	for rows.Next() {
		var id string
		var total, paid, allocated int64
		if err := rows.Scan(&id, &total, &paid, &allocated); err != nil {
			rows.Close()
			return err
		}
		if paid > total {
			rows.Close()
			return fmt.Errorf("invoice %s is paid %d of %d", id, paid, total)
		}
		if paid != allocated {
			rows.Close()
			return fmt.Errorf("invoice %s paid %d but allocated %d", id, paid, allocated)
		}
		if want, ok := expectedPaid[id]; ok && want != paid {
			rows.Close()
			return fmt.Errorf("invoice %s paid %d, expected %d", id, paid, want)
		}
		unpaidTotal += total - paid
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	// 5. The receivable equals the unpaid total.
	receivable, err := ledger.ReceivableForCustomer(ctx, pool, customerID, usd)
	if err != nil {
		return err
	}
	if receivable.Minor() != unpaidTotal {
		return fmt.Errorf("receivable %s, want %s", receivable.Format(),
			money.New(unpaidTotal, usd).Format())
	}
	return nil
}

// A void invoice cannot be paid: money must not attach to a document that no
// longer exists in the legal sense.
func TestPaymentOnVoidInvoiceRefused(t *testing.T) {
	w := newWorld(t, noTax())
	issued := w.issue(t, w.request(line("VPS Small monthly", "12.00", 1000)))
	ctx := context.Background()

	if _, err := w.pool.Exec(ctx, `UPDATE invoices SET status = 'void' WHERE id = $1`, issued.InvoiceID); err != nil {
		t.Fatal(err)
	}

	err := w.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := billing.ApplyPayment(ctx, tx, billing.AllocationRequest{
			Provider:          "stripe",
			ProviderAccount:   "acct",
			ProviderPaymentID: "pi_void",
			InvoiceID:         issued.InvoiceID,
			Amount:            money.MustParse("12.00", usd),
		})
		return err
	})
	if err == nil {
		t.Fatal("payment applied to a void invoice")
	}

	var postings int
	if err := w.pool.QueryRow(ctx,
		`SELECT count(*) FROM journal_transactions WHERE kind = 'payment_received'`).Scan(&postings); err != nil {
		t.Fatal(err)
	}
	if postings != 0 {
		t.Fatalf("%d payment postings for a void invoice, want 0", postings)
	}
}

// A provider payment id can only ever fund one invoice. Reusing it for a second
// invoice would post cash the provider never took, so it is refused and nothing
// is written.
func TestPaymentCannotBeReusedAcrossInvoices(t *testing.T) {
	w := newWorld(t, noTax())
	first := w.issue(t, w.request(line("VPS Small monthly", "12.00", 1000)))
	second := w.issue(t, w.request(line("VPS Large monthly", "12.00", 1000)))
	ctx := context.Background()

	pay := func(invoiceID, amount string) error {
		return w.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			_, err := billing.ApplyPayment(ctx, tx, billing.AllocationRequest{
				Provider:          "stripe",
				ProviderAccount:   "acct",
				ProviderPaymentID: "pi_shared",
				InvoiceID:         invoiceID,
				Amount:            money.MustParse(amount, usd),
			})
			return err
		})
	}

	if err := pay(first.InvoiceID, "12.00"); err != nil {
		t.Fatalf("first application: %v", err)
	}

	// Same provider id, same amount, different invoice: refused.
	err := pay(second.InvoiceID, "12.00")
	if !errors.Is(err, billing.ErrPaymentReused) {
		t.Fatalf("got %v, want ErrPaymentReused", err)
	}
	var paid int64
	if err := w.pool.QueryRow(ctx,
		`SELECT amount_paid_minor FROM invoices WHERE id = $1`, second.InvoiceID).Scan(&paid); err != nil {
		t.Fatal(err)
	}
	if paid != 0 {
		t.Fatalf("second invoice credited %d from a reused payment", paid)
	}

	// The ledger still balances, and only one payment exists.
	tb, err := ledger.TrialBalance(ctx, w.pool)
	if err != nil {
		t.Fatal(err)
	}
	for code, net := range tb {
		if !net.IsZero() {
			t.Fatalf("trial balance for %s is %s", code, net)
		}
	}
	var payments int
	if err := w.pool.QueryRow(ctx, `SELECT count(*) FROM payments`).Scan(&payments); err != nil {
		t.Fatal(err)
	}
	if payments != 1 {
		t.Fatalf("%d payments, want 1", payments)
	}
}

// The same provider id presented with a different amount is refused: the recorded
// amount is what the provider actually took.
func TestPaymentAmountCannotChange(t *testing.T) {
	w := newWorld(t, noTax())
	issued := w.issue(t, w.request(line("VPS Small monthly", "12.00", 1000)))
	ctx := context.Background()

	pay := func(amount string) error {
		return w.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			_, err := billing.ApplyPayment(ctx, tx, billing.AllocationRequest{
				Provider:          "stripe",
				ProviderAccount:   "acct",
				ProviderPaymentID: "pi_amount",
				InvoiceID:         issued.InvoiceID,
				Amount:            money.MustParse(amount, usd),
			})
			return err
		})
	}

	// A partial payment, then the same provider id claiming a larger amount.
	if err := pay("5.00"); err != nil {
		t.Fatalf("first: %v", err)
	}
	if err := pay("7.00"); !errors.Is(err, billing.ErrPaymentReused) {
		t.Fatalf("got %v, want ErrPaymentReused", err)
	}

	var paid int64
	if err := w.pool.QueryRow(ctx,
		`SELECT amount_paid_minor FROM invoices WHERE id = $1`, issued.InvoiceID).Scan(&paid); err != nil {
		t.Fatal(err)
	}
	if paid != 500 {
		t.Fatalf("paid = %d, want 500 (the recorded amount)", paid)
	}
}
