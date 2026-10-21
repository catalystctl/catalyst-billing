package creditnote_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/catalyst/billing/internal/creditnote"
	"github.com/catalyst/billing/internal/ledger"
	"github.com/catalyst/billing/internal/money"
	"github.com/catalyst/billing/internal/store"
	"github.com/catalyst/billing/internal/store/storetest"
	"github.com/jackc/pgx/v5"
)

// env is a customer with an issued, partly paid invoice.
type env struct {
	pool       *store.Pool
	svc        *creditnote.Service
	customerID string
	invoiceID  string
	userID     string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	pool := storetest.DB(t)
	ctx := context.Background()
	e := &env{pool: pool, svc: creditnote.New(pool)}

	if err := pool.QueryRow(ctx, `
		INSERT INTO customers (email, name) VALUES ('credit@example.test', 'Credit Me')
		RETURNING id`).Scan(&e.customerID); err != nil {
		t.Fatalf("customer: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO users (email, password_hash, email_verified_at)
		VALUES ('ops@example.test', 'x', now()) RETURNING id`).Scan(&e.userID); err != nil {
		t.Fatalf("user: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO invoices (customer_id, number, status, currency, subtotal_minor, tax_minor,
			total_minor, amount_paid_minor, issued_at, due_at, snapshot)
		VALUES ($1, 'INV-C1', 'open', 'USD', 10000, 2000, 12000, 0, now(),
		        now() + interval '14 days', '{"schema_version":1}'::jsonb)
		RETURNING id`, e.customerID).Scan(&e.invoiceID); err != nil {
		t.Fatalf("invoice: %v", err)
	}

	usd, err := money.NewCurrency("USD", 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, _, err := ledger.Post(ctx, tx, ledger.Posting{
			Kind: ledger.KindInvoiceIssued, Reference: ledger.RefInvoice, ReferenceID: e.invoiceID,
			CustomerID: e.customerID, Currency: usd,
			Debits: []ledger.Entry{{Account: ledger.AccountReceivable, Amount: money.New(12000, usd)}},
			Credits: []ledger.Entry{
				{Account: ledger.AccountRevenue, Amount: money.New(10000, usd)},
				{Account: ledger.AccountTaxPayable, Amount: money.New(2000, usd)},
			},
		})
		return err
	}); err != nil {
		t.Fatalf("post invoice: %v", err)
	}
	return e
}

func (e *env) issue(t *testing.T, amount, tax int64) (creditnote.CreditNote, error) {
	t.Helper()
	return e.svc.Issue(context.Background(), creditnote.IssueRequest{
		InvoiceID:   e.invoiceID,
		AmountMinor: amount,
		TaxMinor:    tax,
		Reason:      "service credit",
		IssuedBy:    e.userID,
	})
}

func (e *env) apply(t *testing.T, amount int64) (creditnote.Application, error) {
	t.Helper()
	return e.svc.Apply(context.Background(), creditnote.ApplyRequest{
		CustomerID:  e.customerID,
		InvoiceID:   e.invoiceID,
		AmountMinor: amount,
		AppliedBy:   e.userID,
	})
}

// balanced asserts every currency nets to zero.
func (e *env) balanced(t *testing.T) {
	t.Helper()
	rows, err := e.pool.Query(context.Background(), `
		SELECT currency, SUM(debit_minor) - SUM(credit_minor)
		FROM journal_entries GROUP BY currency`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			code  string
			delta int64
		)
		if err := rows.Scan(&code, &delta); err != nil {
			t.Fatal(err)
		}
		if delta != 0 {
			t.Fatalf("the ledger is unbalanced in %s by %d", code, delta)
		}
	}
}

// A credit note posts revenue and tax down, customer credit up, and the journal balances.
func TestIssueCreditNote(t *testing.T) {
	e := newEnv(t)
	note, err := e.issue(t, 1200, 200)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if note.Number == "" || note.Status != creditnote.StatusIssued {
		t.Fatalf("note = %+v", note)
	}
	if note.Number[:3] != "CN-" {
		t.Fatalf("number = %q, want the CN- series", note.Number)
	}

	e.balanced(t)

	// The credit balance is what the customer can now spend.
	balance, err := e.svc.CreditBalance(context.Background(), e.customerID)
	if err != nil {
		t.Fatalf("balance: %v", err)
	}
	if len(balance) != 1 || balance[0].Minor() != 1200 {
		t.Fatalf("credit balance = %v", balance)
	}

	// Revenue went down by the net share and tax by the tax share, not the whole amount.
	var revenue, tax int64
	if err := e.pool.QueryRow(context.Background(), `
		SELECT COALESCE(SUM(debit_minor - credit_minor), 0) FROM journal_entries
		WHERE account_code = $1`, ledger.AccountRevenue).Scan(&revenue); err != nil {
		t.Fatal(err)
	}
	if err := e.pool.QueryRow(context.Background(), `
		SELECT COALESCE(SUM(debit_minor - credit_minor), 0) FROM journal_entries
		WHERE account_code = $1`, ledger.AccountTaxPayable).Scan(&tax); err != nil {
		t.Fatal(err)
	}
	// Revenue started at -10000 (credit) and the note debits 1000.
	if revenue != -9000 {
		t.Fatalf("net revenue = %d, want -9000", revenue)
	}
	// Tax started at -2000 (credit) and the note debits 200.
	if tax != -1800 {
		t.Fatalf("net tax = %d, want -1800", tax)
	}
}

// Credits cannot exceed the invoice total.
func TestCreditCannotExceedInvoice(t *testing.T) {
	e := newEnv(t)
	if _, err := e.issue(t, 8000, 0); err != nil {
		t.Fatalf("first: %v", err)
	}
	if _, err := e.issue(t, 8000, 0); !errors.Is(err, creditnote.ErrExceedsInvoice) {
		t.Fatalf("error = %v, want ErrExceedsInvoice", err)
	}
	// Exactly the remainder is allowed.
	if _, err := e.issue(t, 4000, 0); err != nil {
		t.Fatalf("exact remainder: %v", err)
	}
}

// A draft or void invoice cannot be credited: neither has anything to correct.
func TestCreditRefusesDraftAndVoid(t *testing.T) {
	e := newEnv(t)

	// A draft is a document that was never issued.
	var draftID string
	if err := e.pool.QueryRow(context.Background(), `
		INSERT INTO invoices (customer_id, status, currency, subtotal_minor, tax_minor,
			total_minor, snapshot)
		VALUES ($1, 'draft', 'USD', 1000, 0, 1000, '{"schema_version":1}'::jsonb)
		RETURNING id`, e.customerID).Scan(&draftID); err != nil {
		t.Fatalf("draft: %v", err)
	}
	if _, err := e.svc.Issue(context.Background(), creditnote.IssueRequest{
		InvoiceID: draftID, AmountMinor: 100, Reason: "x", IssuedBy: e.userID,
	}); !errors.Is(err, creditnote.ErrNotIssuable) {
		t.Fatalf("draft: error = %v, want ErrNotIssuable", err)
	}

	// A void document is already cancelled. The trigger permits open -> void, which is how an
	// operator cancels one.
	if _, err := e.pool.Exec(context.Background(),
		`UPDATE invoices SET status = 'void' WHERE id = $1`, e.invoiceID); err != nil {
		t.Fatalf("void: %v", err)
	}
	if _, err := e.issue(t, 100, 0); !errors.Is(err, creditnote.ErrNotIssuable) {
		t.Fatalf("void: error = %v, want ErrNotIssuable", err)
	}
}

// The number series is gap-free under concurrency: two simultaneous notes get two numbers.
func TestCreditNoteNumbersAreUnique(t *testing.T) {
	e := newEnv(t)
	var wg sync.WaitGroup
	numbers := make([]string, 4)
	errs := make([]error, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			note, err := e.svc.Issue(context.Background(), creditnote.IssueRequest{
				InvoiceID:   e.invoiceID,
				AmountMinor: 100,
				Reason:      "concurrent",
				IssuedBy:    e.userID,
			})
			numbers[idx] = note.Number
			errs[idx] = err
		}(i)
	}
	wg.Wait()

	seen := map[string]bool{}
	for i, err := range errs {
		if err != nil {
			t.Fatalf("issue %d: %v", i, err)
		}
		if seen[numbers[i]] {
			t.Fatalf("number %q was issued twice", numbers[i])
		}
		seen[numbers[i]] = true
	}
}

// An issued credit note is immutable: the document the customer received cannot be rewritten.
func TestIssuedCreditNoteIsImmutable(t *testing.T) {
	e := newEnv(t)
	note, err := e.issue(t, 500, 0)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	if _, err := e.pool.Exec(context.Background(),
		`UPDATE credit_notes SET total_minor = 1 WHERE id = $1`, note.ID); err == nil {
		t.Fatal("an issued credit note's total was rewritten")
	}
	if _, err := e.pool.Exec(context.Background(),
		`DELETE FROM credit_notes WHERE id = $1`, note.ID); err == nil {
		t.Fatal("an issued credit note was deleted")
	}
}

// Applying credit settles the invoice exactly at zero, and only then.
func TestApplyCreditSettlesInvoice(t *testing.T) {
	e := newEnv(t)
	if _, err := e.issue(t, 5000, 0); err != nil {
		t.Fatalf("issue: %v", err)
	}

	first, err := e.apply(t, 5000)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if first.InvoicePaid {
		t.Fatal("the invoice was marked paid after a partial application")
	}
	var status string
	var paid int64
	if err := e.pool.QueryRow(context.Background(),
		`SELECT status, amount_paid_minor FROM invoices WHERE id = $1`, e.invoiceID).
		Scan(&status, &paid); err != nil {
		t.Fatal(err)
	}
	if status != "open" || paid != 5000 {
		t.Fatalf("status = %q, paid = %d", status, paid)
	}

	// The remaining balance is still owed, and the credit is now exhausted.
	if _, err := e.apply(t, 0); !errors.Is(err, creditnote.ErrInsufficientCredit) {
		t.Fatalf("error = %v, want ErrInsufficientCredit", err)
	}

	// More credit, then settle the rest.
	if _, err := e.issue(t, 7000, 0); err != nil {
		t.Fatalf("second credit note: %v", err)
	}
	final, err := e.apply(t, 7000)
	if err != nil {
		t.Fatalf("final apply: %v", err)
	}
	if !final.InvoicePaid {
		t.Fatal("the invoice was not marked paid at exactly zero")
	}
	if err := e.pool.QueryRow(context.Background(),
		`SELECT status, amount_paid_minor FROM invoices WHERE id = $1`, e.invoiceID).
		Scan(&status, &paid); err != nil {
		t.Fatal(err)
	}
	if status != "paid" || paid != 12000 {
		t.Fatalf("status = %q, paid = %d, want paid and 12000", status, paid)
	}
	e.balanced(t)
}

// Applying more than is owed is refused: an over-application would be a credit note, not a payment.
func TestApplyCannotExceedOwed(t *testing.T) {
	e := newEnv(t)
	// Half the invoice is already paid, so the credit (capped at the invoice total) is larger than
	// what is still owed.
	if _, err := e.pool.Exec(context.Background(),
		`UPDATE invoices SET amount_paid_minor = 6000 WHERE id = $1`, e.invoiceID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.issue(t, 12000, 0); err != nil {
		t.Fatalf("issue: %v", err)
	}

	if _, err := e.apply(t, 12000); !errors.Is(err, creditnote.ErrExceedsInvoice) {
		t.Fatalf("error = %v, want ErrExceedsInvoice", err)
	}
	// The automatic amount takes the smaller of owed and available: 6000, not 12000.
	app, err := e.apply(t, 0)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if app.AmountMinor != 6000 {
		t.Fatalf("amount = %d, want the 6000 still owed", app.AmountMinor)
	}
	if !app.InvoicePaid {
		t.Fatal("the invoice was not settled")
	}
}

// With no credit, applying is refused.
func TestApplyWithoutCredit(t *testing.T) {
	e := newEnv(t)
	if _, err := e.apply(t, 100); !errors.Is(err, creditnote.ErrInsufficientCredit) {
		t.Fatalf("error = %v, want ErrInsufficientCredit", err)
	}
}

// Applying credit to another customer's invoice is not found, not forbidden: the two must be
// indistinguishable.
func TestApplyCrossCustomerIsNotFound(t *testing.T) {
	e := newEnv(t)
	if _, err := e.issue(t, 1000, 0); err != nil {
		t.Fatal(err)
	}
	var otherID string
	if err := e.pool.QueryRow(context.Background(), `
		INSERT INTO customers (email, name) VALUES ('other@example.test', 'Other') RETURNING id`).
		Scan(&otherID); err != nil {
		t.Fatal(err)
	}

	_, err := e.svc.Apply(context.Background(), creditnote.ApplyRequest{
		CustomerID:  otherID,
		InvoiceID:   e.invoiceID,
		AmountMinor: 100,
		AppliedBy:   e.userID,
	})
	if !errors.Is(err, creditnote.ErrNotFound) {
		t.Fatalf("error = %v, want ErrNotFound", err)
	}
}

// A settled invoice cannot be credited further by applying: there is nothing owed.
func TestApplyToSettledInvoice(t *testing.T) {
	e := newEnv(t)
	if _, err := e.pool.Exec(context.Background(),
		`UPDATE invoices SET status = 'paid', amount_paid_minor = 12000 WHERE id = $1`,
		e.invoiceID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.issue(t, 1000, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := e.apply(t, 0); !errors.Is(err, creditnote.ErrInsufficientCredit) {
		t.Fatalf("error = %v, want ErrInsufficientCredit", err)
	}
}

// The posting is idempotent per application: applying twice creates two applications with two
// distinct postings, and each posting is balanced.
func TestApplyIsPerApplication(t *testing.T) {
	e := newEnv(t)
	if _, err := e.issue(t, 3000, 0); err != nil {
		t.Fatal(err)
	}
	a, err := e.apply(t, 1000)
	if err != nil {
		t.Fatal(err)
	}
	b, err := e.apply(t, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if a.ID == b.ID {
		t.Fatal("two applications share an id")
	}

	var postings int
	if err := e.pool.QueryRow(context.Background(), `
		SELECT count(*) FROM journal_transactions
		WHERE kind = 'adjustment' AND reference_type = $1`,
		string(ledger.RefCreditApplication)).Scan(&postings); err != nil {
		t.Fatal(err)
	}
	if postings != 2 {
		t.Fatalf("postings = %d, want one per application", postings)
	}
	e.balanced(t)
}

// A credit note without a reason is refused: an unexplained credit is unauditable.
func TestCreditNoteRequiresReason(t *testing.T) {
	e := newEnv(t)
	_, err := e.svc.Issue(context.Background(), creditnote.IssueRequest{
		InvoiceID: e.invoiceID, AmountMinor: 100, IssuedBy: e.userID,
	})
	if err == nil {
		t.Fatal("a credit note without a reason was accepted")
	}
	// The tax share cannot exceed the total.
	if _, err := e.svc.Issue(context.Background(), creditnote.IssueRequest{
		InvoiceID: e.invoiceID, AmountMinor: 100, TaxMinor: 200,
		Reason: "bad", IssuedBy: e.userID,
	}); err == nil {
		t.Fatal("a tax share larger than the total was accepted")
	}
}

var _ = time.Now
