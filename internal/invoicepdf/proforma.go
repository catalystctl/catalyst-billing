package invoicepdf

// Provisional documents: issuing a proforma and turning it into a real invoice.
//
// # Why this lives in the document package
//
// A proforma is a numbering state, not a separate document type: it is an invoice
// that has been issued to a customer and asked for payment, but which has not yet
// earned a number. The two operations that define it are "issue without allocating
// a number" and "allocate the number later", which are the same concern as the
// numbering in numbering.go. Keeping them together means the one rule that matters
// — the number is allocated exactly once, in the transition that stops the document
// being provisional — has a single implementation.
//
// The package is therefore two halves: a renderer (document.go, pdf.go, layout.go)
// and a document lifecycle (numbering.go, proforma.go). The renderer never touches
// a database; this half never formats a page.
//
// # The ledger question
//
// A proforma is posted as a receivable when it is issued, exactly like a numbered
// invoice, and conversion posts nothing.
//
// That is deliberate. The customer owes the money the moment they are asked to pay
// it, and the subledger's job is to record what is owed — not to decide when a
// document became legal. If the receivable were deferred to conversion, a payment
// that arrived before conversion (the normal case: the customer pays the proforma
// precisely because they were sent it) would credit receivable that had never been
// debited, and the books would not balance. Conversion is a numbering event; it
// changes no money, which is exactly the boundary the database trigger draws.
//
// The tax position is unaffected: a proforma states the tax that *will* apply, the
// tax liability is recognised on the invoice, and the renderer prints the proforma
// as "not a tax invoice" for the customer's benefit.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/catalyst/billing/internal/billing"
	"github.com/catalyst/billing/internal/ledger"
	"github.com/catalyst/billing/internal/money"
	"github.com/jackc/pgx/v5"
)

// Errors returned by the provisional-document operations.
var (
	// ErrProformaNotFound is returned when the invoice id does not exist.
	ErrProformaNotFound = errors.New("invoicepdf: invoice not found")
	// ErrNotProforma is returned when conversion is asked of a document that was
	// never provisional. Converting a real invoice would be a renumbering, which
	// is forgery rather than conversion.
	ErrNotProforma = errors.New("invoicepdf: the document is not a proforma")
	// ErrProformaNumbered is returned for a proforma that already carries a
	// number, which the schema forbids and which means the row is corrupt.
	ErrProformaNumbered = errors.New("invoicepdf: the proforma already has a number")
	// ErrProformaNotConvertible is returned when a proforma is in a state that must
	// not consume a number (void, uncollectible, or still a draft).
	ErrProformaNotConvertible = errors.New("invoicepdf: the proforma cannot be converted from its current status")
)

// IssueProforma turns an existing draft into a provisional, unnumbered document.
//
// It is the counterpart of billing.Issuer.Issue and shares its shape: it takes the
// caller's transaction, locks the row, and posts the receivable in that same
// transaction. What it does not do is allocate a number — that is the entire
// difference, and it is why the CHECK constraint `invoices_proforma_unnumbered`
// exists.
//
// The invoice must be a draft. Issuing twice is refused by the status check rather
// than by a duplicate-key error, so the caller gets a message it can act on.
func IssueProforma(ctx context.Context, tx pgx.Tx, invoiceID string, dueAt time.Time) error {
	var (
		status     string
		currency   string
		totalMinor int64
		snap       billing.Snapshot
		customerID string
		number     *string
		proforma   bool
	)
	err := tx.QueryRow(ctx, `
		SELECT status, currency, total_minor, snapshot, customer_id, number, proforma
		FROM invoices WHERE id = $1 FOR UPDATE`, invoiceID).
		Scan(&status, &currency, &totalMinor, &snap, &customerID, &number, &proforma)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: %s", ErrProformaNotFound, invoiceID)
	}
	if err != nil {
		return fmt.Errorf("invoicepdf: load invoice for proforma: %w", err)
	}
	if status != "draft" {
		return fmt.Errorf("%w: %s is %s", billing.ErrInvoiceNotDraft, invoiceID, status)
	}
	if number != nil || proforma {
		// A draft cannot carry a number (the schema forbids it) and cannot already
		// be a proforma. Reaching here means the row is inconsistent, so refuse
		// rather than write a second, contradictory state.
		return fmt.Errorf("%w: %s", ErrProformaNumbered, invoiceID)
	}

	cur, err := currencyOf(currency)
	if err != nil {
		return err
	}

	// One UPDATE sets every column the transition needs. `number` stays NULL, which
	// is what the CHECK requires of a proforma, and `issued_at` records when the
	// customer was first asked to pay.
	if _, err := tx.Exec(ctx, `
		UPDATE invoices
		SET status = 'open', proforma = true, issued_at = now(),
		    due_at = COALESCE(due_at, $2)
		WHERE id = $1`, invoiceID, dueAt); err != nil {
		return fmt.Errorf("invoicepdf: mark invoice provisional: %w", err)
	}

	// The receivable is posted now: the customer owes it now. See the note at the
	// top of this file for why conversion posts nothing.
	totalAmount := money.New(totalMinor, cur)
	taxMinor := snap.Totals.Tax.Minor()
	netMinor := totalMinor - taxMinor
	if netMinor < 0 {
		return fmt.Errorf("%w: snapshot tax %d exceeds total %d",
			billing.ErrInvalidInvoice, taxMinor, totalMinor)
	}

	posting := ledger.Posting{
		Kind:        ledger.KindInvoiceIssued,
		Reference:   ledger.RefInvoice,
		ReferenceID: invoiceID,
		CustomerID:  customerID,
		Currency:    cur,
		Debits: []ledger.Entry{{
			Account: ledger.AccountReceivable,
			Amount:  totalAmount,
			Memo:    "proforma invoice",
		}},
	}
	if netMinor > 0 {
		posting.Credits = append(posting.Credits, ledger.Entry{
			Account: ledger.AccountRevenue, Amount: money.New(netMinor, cur), Memo: "service revenue",
		})
	}
	if taxMinor > 0 {
		posting.Credits = append(posting.Credits, ledger.Entry{
			Account: ledger.AccountTaxPayable, Amount: money.New(taxMinor, cur), Memo: "tax collected",
		})
	}
	if _, _, err := ledger.Post(ctx, tx, posting); err != nil {
		return err
	}
	return nil
}

// Converted is what ConvertProforma reports back.
type Converted struct {
	InvoiceID string
	// Number is the document number the conversion assigned. It is the fact the
	// caller needs: the customer's proforma copy and the real invoice differ by
	// exactly this string.
	Number string
}

// ConvertProforma allocates a number and makes a provisional document a real
// invoice.
//
// # One UPDATE, and why it must be one
//
// `proforma = false` and `number = 'INV-...'` are written by a single statement.
// The schema has a CHECK forbidding a numbered proforma, so a two-statement
// version would be rejected outright — and even without the CHECK, a concurrent
// reader between the two statements would observe either a numbered proforma or an
// unnumbered invoice, and both are states the system promises never exist.
//
// The database trigger permits exactly this transition and nothing else: a row that
// was a proforma may gain its number, and every other frozen field must be
// unchanged. So this function sets no money, no snapshot and no customer.
//
// # When the caller must invoke it
//
// On payment, after billing.ApplyPayment reports InvoicePaid — in the same
// transaction, so a crash cannot leave a paid document that is still provisional.
// The function takes the caller's pgx.Tx for exactly that reason. It is idempotent
// by construction: converting an already-converted invoice returns ErrNotProforma
// rather than assigning a second number, so a replayed payment is refused loudly
// instead of burning a number.
func ConvertProforma(ctx context.Context, tx pgx.Tx, invoiceID string, series billing.NumberSeries) (Converted, error) {
	var (
		status   string
		proforma bool
		number   *string
	)
	err := tx.QueryRow(ctx, `
		SELECT status, proforma, number FROM invoices WHERE id = $1 FOR UPDATE`, invoiceID).
		Scan(&status, &proforma, &number)
	if errors.Is(err, pgx.ErrNoRows) {
		return Converted{}, fmt.Errorf("%w: %s", ErrProformaNotFound, invoiceID)
	}
	if err != nil {
		return Converted{}, fmt.Errorf("invoicepdf: load proforma: %w", err)
	}
	if !proforma {
		return Converted{}, fmt.Errorf("%w: %s", ErrNotProforma, invoiceID)
	}
	if number != nil {
		return Converted{}, fmt.Errorf("%w: %s", ErrProformaNumbered, invoiceID)
	}
	if status != "open" && status != "paid" {
		// A void or uncollectible proforma has nothing to number: nobody can pay it,
		// so a number attached to one would be a permanent gap in the series that
		// means nothing. A draft is not a proforma at all.
		//
		// `paid` is permitted, and is in fact the common case: the payment path
		// settles the invoice and *then* converts it, because the conversion is the
		// consequence of the settlement. Refusing `paid` would break the primary
		// path and leave a settled document unnumbered.
		return Converted{}, fmt.Errorf("%w: %s is %s", ErrProformaNotConvertible, invoiceID, status)
	}

	if series.Scope == "" {
		series = billing.DefaultSeries
	}
	allocated, err := series.Allocate(ctx, tx)
	if err != nil {
		return Converted{}, err
	}

	// The single UPDATE. The trigger's carve-out is keyed on the transition
	// `OLD.proforma AND NOT NEW.proforma`, so this is the only shape it accepts.
	if _, err := tx.Exec(ctx, `
		UPDATE invoices SET proforma = false, number = $2 WHERE id = $1`,
		invoiceID, allocated.Format(),
	); err != nil {
		return Converted{}, fmt.Errorf("invoicepdf: convert proforma: %w", err)
	}

	return Converted{InvoiceID: invoiceID, Number: allocated.Format()}, nil
}

// ProformaState is the lifecycle view a handler needs to decide what to offer.
type ProformaState struct {
	InvoiceID string
	Status    string
	Proforma  bool
	// Number is empty for a proforma, which is the visible difference the UI shows.
	Number string
}

// ReadProformaState reports whether an invoice is provisional and what its number
// is.
func ReadProformaState(ctx context.Context, q interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}, invoiceID string) (ProformaState, error) {
	var (
		out    ProformaState
		number *string
	)
	err := q.QueryRow(ctx, `
		SELECT id, status, proforma, number FROM invoices WHERE id = $1`, invoiceID).
		Scan(&out.InvoiceID, &out.Status, &out.Proforma, &number)
	if errors.Is(err, pgx.ErrNoRows) {
		return ProformaState{}, fmt.Errorf("%w: %s", ErrProformaNotFound, invoiceID)
	}
	if err != nil {
		return ProformaState{}, fmt.Errorf("invoicepdf: read proforma state: %w", err)
	}
	if number != nil {
		out.Number = *number
	}
	return out, nil
}

// currencyOf resolves a persisted currency code.
//
// Codes outside the ISO table are reconstructed at scale 2, which is what the
// schema's default assumes; a currency with a different scale must be in the ISO
// table or declared explicitly by the caller that created it.
func currencyOf(code string) (money.Currency, error) {
	if cur, ok := money.LookupCurrency(code); ok {
		return cur, nil
	}
	return money.NewCurrency(code, 2)
}

// ConvertProformaOnPayment is the call the payment path makes.
//
// It exists so wiring conversion into a payment is one line that cannot be got
// wrong. The common case — the invoice was never a proforma — returns a zero
// Converted and a nil error, so the caller does not have to branch on it:
//
//	if res.InvoicePaid {
//		if _, err := invoicepdf.ConvertProformaOnPayment(ctx, tx, invoiceID, series); err != nil {
//			return err
//		}
//	}
//
// It must be called *after* billing.ApplyPayment, in the same transaction: the
// settlement is what makes the conversion happen, so the status is already `paid`
// when this runs, and ConvertProforma accepts that status for exactly that reason.
//
// A genuine failure is still returned: a proforma that cannot be converted means a
// paid document is still provisional, and swallowing that would leave the customer
// holding a numbered-invoice promise the system did not keep.
func ConvertProformaOnPayment(ctx context.Context, tx pgx.Tx, invoiceID string, series billing.NumberSeries) (Converted, error) {
	out, err := ConvertProforma(ctx, tx, invoiceID, series)
	if errors.Is(err, ErrNotProforma) {
		return Converted{}, nil
	}
	return out, err
}
