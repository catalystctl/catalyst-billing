package creditnote_test

import (
	"context"
	"errors"
	"testing"

	"github.com/catalyst/billing/internal/creditnote"
)

// A debit note is a charge: it posts a receivable up and revenue up, and it numbers from its
// own series (INV-10). If it numbered from the credit series, a customer quoting a note would
// be quoting the wrong document.
func TestIssueDebitNoteChargesMore(t *testing.T) {
	e := newEnv(t)
	note, err := e.svc.Issue(context.Background(), creditnote.IssueRequest{
		InvoiceID:   e.invoiceID,
		AmountMinor: 800,
		TaxMinor:    100,
		Reason:      "setup fee was undercharged",
		IssuedBy:    e.userID,
		Kind:        creditnote.KindDebit,
	})
	if err != nil {
		t.Fatalf("issue debit note: %v", err)
	}
	if note.Kind != creditnote.KindDebit {
		t.Fatalf("kind = %q, want debit", note.Kind)
	}
	if len(note.Number) < 3 || note.Number[:3] != "DN-" {
		t.Fatalf("number = %q, want the DN- series", note.Number)
	}

	// The posting is the mirror of a credit: receivable up, revenue and tax payable up, and
	// the customer's credit balance untouched (a debit note is not paid from credit).
	rows, err := e.pool.Query(context.Background(), `
		SELECT je.account_code, SUM(je.debit_minor), SUM(je.credit_minor)
		FROM journal_entries je
		JOIN journal_transactions jt ON jt.id = je.transaction_id
		WHERE jt.reference_id = $1 GROUP BY je.account_code`, note.ID)
	if err != nil {
		t.Fatalf("read postings: %v", err)
	}
	defer rows.Close()
	got := map[string][2]int64{}
	for rows.Next() {
		var (
			account       string
			debit, credit int64
		)
		if err := rows.Scan(&account, &debit, &credit); err != nil {
			t.Fatalf("scan posting: %v", err)
		}
		got[account] = [2]int64{debit, credit}
	}
	if got["ar"] != [2]int64{800, 0} {
		t.Fatalf("receivable entries = %v, want a debit of 800", got["ar"])
	}
	if got["revenue"] != [2]int64{0, 700} {
		t.Fatalf("revenue entries = %v, want a credit of 700", got["revenue"])
	}
	if got["tax_payable"] != [2]int64{0, 100} {
		t.Fatalf("tax entries = %v, want a credit of 100", got["tax_payable"])
	}
	if _, ok := got["customer_credit"]; ok {
		t.Fatal("a debit note credited the customer's balance")
	}
	e.balanced(t)
}

// A debit note is not capped by what is left on the invoice: it charges more, which is the
// whole reason it exists.
func TestDebitNoteIsNotCappedByTheInvoiceTotal(t *testing.T) {
	e := newEnv(t)
	// The invoice is 12000; a credit of 12000 is the most it can absorb.
	note, err := e.svc.Issue(context.Background(), creditnote.IssueRequest{
		InvoiceID:   e.invoiceID,
		AmountMinor: 20000,
		Reason:      "recovered cost",
		IssuedBy:    e.userID,
		Kind:        creditnote.KindDebit,
	})
	if err != nil {
		t.Fatalf("issue debit note: %v", err)
	}
	if note.TotalMinor != 20000 {
		t.Fatalf("total = %d, want 20000", note.TotalMinor)
	}
}

// Voiding cancels the note and reverses its posting. The row is kept and the ledger gains a
// contra transaction: an append-only subledger cannot forget, and an auditor has to be able
// to tell "cancelled" from "never issued" (ADM-25).
func TestVoidCreditNoteReversesItsPosting(t *testing.T) {
	e := newEnv(t)
	note, err := e.issue(t, 1500, 250)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	voided, err := e.svc.Void(context.Background(), creditnote.VoidRequest{
		ID: note.ID, Reason: "raised against the wrong customer",
	})
	if err != nil {
		t.Fatalf("void: %v", err)
	}
	if voided.Status != creditnote.StatusVoid {
		t.Fatalf("status = %q, want void", voided.Status)
	}
	if voided.VoidReason != "raised against the wrong customer" {
		t.Fatalf("void reason = %q", voided.VoidReason)
	}
	if voided.VoidedAt == nil {
		t.Fatal("a voided note has no void timestamp")
	}

	// The credit balance is back to zero: the note's credit was reversed.
	var balance int64
	if err := e.pool.QueryRow(context.Background(), `
		SELECT COALESCE(SUM(credit_minor - debit_minor), 0) FROM journal_entries
		WHERE account_code = 'customer_credit'`).Scan(&balance); err != nil {
		t.Fatalf("read credit balance: %v", err)
	}
	if balance != 0 {
		t.Fatalf("customer credit balance = %d after voiding the only note, want 0", balance)
	}
	e.balanced(t)

	// A second void is refused, and the reason cannot be rewritten afterwards.
	if _, err := e.svc.Void(context.Background(), creditnote.VoidRequest{
		ID: note.ID, Reason: "again",
	}); !errors.Is(err, creditnote.ErrAlreadyVoid) {
		t.Fatalf("second void: %v, want ErrAlreadyVoid", err)
	}
	if _, err := e.pool.Exec(context.Background(),
		`UPDATE credit_notes SET void_reason = 'rewritten' WHERE id = $1`, note.ID); err == nil {
		t.Fatal("the void reason of a cancelled note was rewritten")
	}
}

// A note whose credit has already been applied is not voidable: un-spending it here would
// silently un-pay the invoice it paid.
func TestAppliedCreditNoteCannotBeVoided(t *testing.T) {
	e := newEnv(t)
	note, err := e.issue(t, 5000, 0)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if _, err := e.apply(t, 5000); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if _, err := e.svc.Void(context.Background(), creditnote.VoidRequest{
		ID: note.ID, Reason: "changed our mind",
	}); !errors.Is(err, creditnote.ErrNotVoidable) {
		t.Fatalf("voiding an applied note: %v, want ErrNotVoidable", err)
	}
}

// A voided note no longer counts against the room to credit, so the same amount can be
// re-issued correctly without the invoice appearing twice-credited.
func TestVoidedCreditFreesRoomToCreditAgain(t *testing.T) {
	e := newEnv(t)
	first, err := e.issue(t, 12000, 0)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if _, err := e.svc.Void(context.Background(), creditnote.VoidRequest{
		ID: first.ID, Reason: "wrong amount",
	}); err != nil {
		t.Fatalf("void: %v", err)
	}
	if _, err := e.issue(t, 12000, 0); err != nil {
		t.Fatalf("re-issue after void: %v", err)
	}
}

// A kind the product does not know is refused rather than stored as a credit.
func TestUnknownAdjustmentKindIsRefused(t *testing.T) {
	e := newEnv(t)
	_, err := e.svc.Issue(context.Background(), creditnote.IssueRequest{
		InvoiceID:   e.invoiceID,
		AmountMinor: 100,
		Reason:      "mystery",
		Kind:        "refund_note",
	})
	if !errors.Is(err, creditnote.ErrInvalidKind) {
		t.Fatalf("err = %v, want ErrInvalidKind", err)
	}
}
