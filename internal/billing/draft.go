package billing

// Editing a draft (INV-05).
//
// An invoice is immutable from the moment it has a number: the customer holds a copy, the
// ledger records a receivable, and a tax filing may quote it. Before that moment it is a
// worksheet — a line was typed at the wrong price, a customer's address was the old one —
// and the operator who spotted it must be able to fix it. Paymenter calls this "editable
// while draft"; the trigger in `0021_proforma.sql` already refuses every write to an issued
// row, so the only thing that was missing is a supported way to rewrite a draft.
//
// Two rules keep this from becoming an edit path for issued documents:
//
//  1. The status is checked under `SELECT ... FOR UPDATE` inside this transaction, so a
//     concurrent issuance either commits first (and this update is refused with
//     ErrInvoiceNotDraft) or waits. There is no window where a draft looks editable while
//     its number is being allocated.
//  2. The database is the backstop, not this function: the trigger refuses the UPDATE if
//     the row left draft, so a future caller that forgets the check still cannot rewrite a
//     document a customer already received.
//
// The snapshot is rebuilt rather than patched. A draft's frozen evidence is not evidence
// yet — it is what the document will say when it is issued — so the honest operation is
// "price these lines again", which also re-runs the pricer and keeps the tax evidence
// consistent with the totals.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// DraftUpdate replaces everything on a draft that is still editable.
//
// Lines are required; the rest is optional and left alone when the zero value is passed.
// DueAt in particular is a pointer because "keep the current date" and "clear the date"
// must be distinguishable.
type DraftUpdate struct {
	// Lines is the complete new set of lines. Replacement, not merge: a merge needs a line
	// identity the draft does not have, and "remove line 2 and renumber" is easier to get
	// wrong than "here is the document".
	Lines []Line
	// Customer, when it has an ID, replaces the snapshotted billed party: a draft raised
	// against the old billing address is exactly the case this exists for.
	Customer CustomerDetails
	// Terms, when non-empty, replaces the printed terms.
	Terms string
	// DueAt, when non-nil, replaces the due date.
	DueAt *time.Time
	// Merchant, when it has a name, replaces the letterhead.
	Merchant Merchant
	// Period, when non-nil, replaces the service period.
	Period *Period
}

// UpdateDraft re-prices and rewrites a draft invoice.
//
// It returns the new totals and snapshot so the caller can report what the document now
// says without a second read.
func (s *Issuer) UpdateDraft(ctx context.Context, tx pgx.Tx, invoiceID string, upd DraftUpdate) (Totals, Snapshot, error) {
	var (
		status     string
		currency   string
		customerID string
		existing   Snapshot
	)
	err := tx.QueryRow(ctx, `
		SELECT status, currency, customer_id, snapshot FROM invoices WHERE id = $1 FOR UPDATE`,
		invoiceID).Scan(&status, &currency, &customerID, &existing)
	if errors.Is(err, pgx.ErrNoRows) {
		return Totals{}, Snapshot{}, fmt.Errorf("%w: %s", ErrInvoiceNotFound, invoiceID)
	}
	if err != nil {
		return Totals{}, Snapshot{}, fmt.Errorf("billing: lock draft: %w", err)
	}
	if status != "draft" {
		return Totals{}, Snapshot{}, fmt.Errorf("%w: %s is %s", ErrInvoiceNotDraft, invoiceID, status)
	}

	cur, err := currencyOf(currency)
	if err != nil {
		return Totals{}, Snapshot{}, err
	}
	// The persisted currency is what the document is denominated in, so a reprice may not
	// silently switch it: the customer's payment method, the ledger and every later
	// reconciliation are in the original currency.
	for i, l := range upd.Lines {
		if !l.UnitPrice.Currency().Equal(cur) {
			return Totals{}, Snapshot{}, fmt.Errorf("%w: line %d is %s, invoice is %s",
				ErrCurrencyMismatch, i, l.UnitPrice.Currency(), cur)
		}
	}

	// Fields the caller did not supply keep what the draft already says. Replacing them with
	// zero values would silently blank the letterhead or the billed party on a re-price, which
	// is not what "edit the lines" means.
	if upd.Merchant.Name == "" {
		upd.Merchant = existing.Merchant
	}
	if upd.Customer.ID == "" {
		upd.Customer = existing.Customer
	}
	if upd.Terms == "" {
		upd.Terms = existing.Terms
	}
	if upd.Period == nil {
		upd.Period = existing.Period
	}

	_, results, snap, err := s.prepare(ctx, IssueRequest{
		CustomerID: customerID,
		Lines:      upd.Lines,
		Merchant:   upd.Merchant,
		Customer:   upd.Customer,
		Terms:      upd.Terms,
		Period:     upd.Period,
	})
	if err != nil {
		return Totals{}, Snapshot{}, err
	}

	if _, err := tx.Exec(ctx, `
		UPDATE invoices
		SET subtotal_minor = $2, tax_minor = $3, total_minor = $4, snapshot = $5,
		    due_at = COALESCE($6, due_at)
		WHERE id = $1`,
		invoiceID, snap.Totals.Subtotal.Minor(), snap.Totals.Tax.Minor(),
		snap.Totals.Total.Minor(), snap, upd.DueAt); err != nil {
		return Totals{}, Snapshot{}, fmt.Errorf("billing: update draft: %w", err)
	}

	// Lines are replaced as a set, so a removed line cannot linger as an orphan row and a
	// renumbering cannot collide with the (invoice_id, position) uniqueness.
	if _, err := tx.Exec(ctx, `DELETE FROM invoice_lines WHERE invoice_id = $1`, invoiceID); err != nil {
		return Totals{}, Snapshot{}, fmt.Errorf("billing: clear draft lines: %w", err)
	}
	if err := insertLines(ctx, tx, invoiceID, results); err != nil {
		return Totals{}, Snapshot{}, err
	}

	return snap.Totals, snap, nil
}

// DraftTotals sums the lines of a draft without writing anything.
//
// The edit screen shows the operator what the document will total before they save it, and
// it must be the same arithmetic the save performs. Doing that sum in the browser would
// re-implement rounding and tax, which is how a preview and an invoice disagree.
func (s *Issuer) DraftTotals(ctx context.Context, lines []Line) (Totals, error) {
	_, _, snap, err := s.prepare(ctx, IssueRequest{CustomerID: "preview", Lines: lines})
	if err != nil {
		return Totals{}, err
	}
	return snap.Totals, nil
}
