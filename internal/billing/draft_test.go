package billing_test

import (
	"context"
	"errors"
	"testing"

	"github.com/catalyst/billing/internal/billing"
	"github.com/catalyst/billing/internal/money"
	"github.com/jackc/pgx/v5"
)

// A draft is a worksheet: the operator who typed the wrong price must be able to fix it
// before the document has a number (INV-05). If this stops working, an invoice either cannot
// be corrected at all or has to be corrected after issue, which is a different document.
func TestDraftLinesCanBeReplacedWhileDraft(t *testing.T) {
	w := newWorld(t, billing.FixedTaxPricer{
		Rate: money.MustParseRatio("0%"), Rounding: money.RoundHalfUp, Policy: "none",
	})
	ctx := context.Background()

	var invoiceID string
	if err := w.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		id, _, _, err := w.issuer.CreateDraft(ctx, tx, w.request(line("VPS", "10.00", 1000)))
		invoiceID = id
		return err
	}); err != nil {
		t.Fatalf("create draft: %v", err)
	}

	var totals billing.Totals
	var snap billing.Snapshot
	if err := w.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		updated, updatedSnap, err := w.issuer.UpdateDraft(ctx, tx, invoiceID, billing.DraftUpdate{
			Lines: []billing.Line{
				line("VPS", "12.50", 1000),
				line("Backup add-on", "2.50", 1000),
			},
			Customer: billing.CustomerDetails{ID: w.customerID, Name: "Customer A"},
			Terms:    "Payable on receipt.",
		})
		totals, snap = updated, updatedSnap
		return err
	}); err != nil {
		t.Fatalf("update draft: %v", err)
	}

	if got, want := totals.Total.Minor(), int64(1500); got != want {
		t.Fatalf("draft total = %d, want %d", got, want)
	}
	if snap.Terms != "Payable on receipt." {
		t.Fatalf("snapshot terms = %q, want the replacement", snap.Terms)
	}

	var (
		lineCount int
		stored    int64
	)
	if err := w.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM invoice_lines WHERE invoice_id = $1`, invoiceID).Scan(&lineCount); err != nil {
		t.Fatalf("count lines: %v", err)
	}
	if lineCount != 2 {
		t.Fatalf("the draft has %d lines, want 2: the old line was not replaced", lineCount)
	}
	if err := w.pool.QueryRow(ctx,
		`SELECT total_minor FROM invoices WHERE id = $1`, invoiceID).Scan(&stored); err != nil {
		t.Fatalf("read total: %v", err)
	}
	if stored != 1500 {
		t.Fatalf("stored total = %d, want 1500", stored)
	}
}

// Once the document has a number it is evidence, and the edit path refuses it. The database
// trigger is the backstop; this proves the domain refuses before reaching it, with an error a
// handler can map to a stable code.
func TestIssuedInvoiceCannotBeEditedAsADraft(t *testing.T) {
	w := newWorld(t, billing.FixedTaxPricer{
		Rate: money.MustParseRatio("0%"), Rounding: money.RoundHalfUp, Policy: "none",
	})
	ctx := context.Background()
	issued := w.issue(t, w.request(line("VPS", "10.00", 1000)))

	err := w.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, _, err := w.issuer.UpdateDraft(ctx, tx, issued.InvoiceID, billing.DraftUpdate{
			Lines: []billing.Line{line("Discount", "1.00", 1000)},
		})
		return err
	})
	if !errors.Is(err, billing.ErrInvoiceNotDraft) {
		t.Fatalf("editing an issued invoice: err = %v, want ErrInvoiceNotDraft", err)
	}

	var description string
	if err := w.pool.QueryRow(ctx, `
		SELECT description FROM invoice_lines WHERE invoice_id = $1 ORDER BY position`,
		issued.InvoiceID).Scan(&description); err != nil {
		t.Fatalf("read line: %v", err)
	}
	if description != "VPS" {
		t.Fatalf("the issued invoice's line changed to %q", description)
	}
}
