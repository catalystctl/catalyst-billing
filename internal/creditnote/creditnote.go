// Package creditnote issues credit notes and applies customer credit.
//
// A credit note exists because an issued invoice cannot be edited. Once a document has a number and
// has reached a customer, changing it is not a correction, it is a forgery: the customer's copy and
// ours would disagree. So the correction is a second document, numbered in its own series, that
// points at the first.
//
// Two operations live here because they share that model:
//
//   - Issuing a credit note credits the customer. It reduces revenue (and the tax payable, if the
//     credit is a tax reversal) and creates a customer-credit balance.
//   - Applying credit to an invoice spends that balance. It is a separate economic event with its
//     own reference, so applying credit twice is two rows rather than one mutation of an invoice.
//
// Both post to the ledger with a reference that makes them idempotent. A replayed request is a
// no-op, not a second posting, and the (kind, reference_type, reference_id) uniqueness is what
// guarantees it.
package creditnote

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/catalyst/billing/internal/audit"
	"github.com/catalyst/billing/internal/ledger"
	"github.com/catalyst/billing/internal/money"
	"github.com/catalyst/billing/internal/paginate"
	"github.com/catalyst/billing/internal/store"
	"github.com/jackc/pgx/v5"
)

// Statuses.
const (
	StatusDraft   = "draft"
	StatusIssued  = "issued"
	StatusApplied = "applied"
	StatusVoid    = "void"
)

var (
	// ErrNotFound is returned for an unknown credit note or invoice.
	ErrNotFound = errors.New("creditnote: not found")
	// ErrExceedsInvoice is returned when the credit is more than the invoice can absorb.
	ErrExceedsInvoice = errors.New("creditnote: the credit exceeds what remains on the invoice")
	// ErrInsufficientCredit is returned when the customer's credit balance is too small.
	ErrInsufficientCredit = errors.New("creditnote: insufficient customer credit")
	// ErrNotIssuable is returned when the invoice is not in a state that accepts a credit.
	ErrNotIssuable = errors.New("creditnote: this invoice cannot be credited")
)

// CreditSeries is the number series credit notes use.
//
// It is separate from the invoice series on purpose: a customer's paperwork, and a tax filing, refer
// to "invoice numbers" and "credit note numbers" as two sequences, and interleaving them would make
// both harder to audit.
var CreditSeries = struct {
	Scope  string
	Prefix string
	Width  int
}{Scope: "credit_note", Prefix: "CN-", Width: 6}

// Service issues credit notes and applies credit.
type Service struct {
	pool *store.Pool
	// Now is injectable for tests.
	Now func() time.Time
}

// New constructs the service.
func New(pool *store.Pool) *Service {
	return &Service{pool: pool, Now: func() time.Time { return time.Now().UTC() }}
}

// IssueRequest describes a credit note.
type IssueRequest struct {
	InvoiceID string
	// AmountMinor is the total credit. It must not exceed what remains on the invoice.
	AmountMinor int64
	// TaxMinor is the tax share of the credit, which posts to tax_payable rather than revenue.
	TaxMinor int64
	Reason   string
	IssuedBy string
}

// CreditNote is an issued counter-document.
type CreditNote struct {
	ID         string
	Number     string
	CustomerID string
	InvoiceID  string
	Currency   string
	TotalMinor int64
	TaxMinor   int64
	Reason     string
	Status     string
	IssuedAt   *time.Time
	CreatedAt  time.Time
}

// Amount renders the credit as money.
func (c CreditNote) Amount() (money.Amount, error) {
	cur, err := currencyOf(c.Currency)
	if err != nil {
		return money.Amount{}, err
	}
	return money.New(c.TotalMinor, cur), nil
}

// Issue creates and posts a credit note in one transaction.
//
// The invoice is locked first: two operators crediting the same invoice at once must not each be
// told the full remaining amount is available, which would credit the customer twice.
func (s *Service) Issue(ctx context.Context, req IssueRequest) (CreditNote, error) {
	if req.AmountMinor <= 0 {
		return CreditNote{}, errors.New("creditnote: the amount must be positive")
	}
	if req.TaxMinor < 0 || req.TaxMinor > req.AmountMinor {
		return CreditNote{}, errors.New("creditnote: the tax share must be between zero and the total")
	}
	if req.Reason == "" {
		return CreditNote{}, errors.New("creditnote: a reason is required")
	}

	var out CreditNote
	err := s.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var (
			customerID, currency, status string
			totalMinor, paidMinor        int64
		)
		err := tx.QueryRow(ctx, `
			SELECT customer_id, currency, status, total_minor, amount_paid_minor
			FROM invoices WHERE id = $1 FOR UPDATE`, req.InvoiceID).
			Scan(&customerID, &currency, &status, &totalMinor, &paidMinor)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("creditnote: lock invoice: %w", err)
		}
		if status == "draft" || status == "void" {
			// A draft was never issued and a void document is already cancelled: neither has
			// anything to correct.
			return fmt.Errorf("%w: its status is %s", ErrNotIssuable, status)
		}

		cur, err := currencyOf(currency)
		if err != nil {
			return err
		}

		// What is left to credit is the invoice total minus the credits already issued against it.
		// It is not reduced by payments: a customer can be credited for an amount they paid, and the
		// refund path is what returns money.
		var credited int64
		if err := tx.QueryRow(ctx, `
			SELECT COALESCE(SUM(total_minor), 0) FROM credit_notes
			WHERE invoice_id = $1 AND status IN ('issued', 'applied')`,
			req.InvoiceID).Scan(&credited); err != nil {
			return fmt.Errorf("creditnote: sum existing credits: %w", err)
		}
		remaining := totalMinor - credited
		if req.AmountMinor > remaining {
			return fmt.Errorf("%w: %s requested, %s remaining",
				ErrExceedsInvoice,
				money.New(req.AmountMinor, cur).Format(),
				money.New(remaining, cur).Format())
		}

		// The number is allocated inside this transaction from the same persisted sequence the
		// issuer uses, so a concurrent credit note cannot take the same number.
		number, err := allocateNumber(ctx, tx)
		if err != nil {
			return err
		}

		var (
			id       string
			issuedAt time.Time
		)
		if err := tx.QueryRow(ctx, `
			INSERT INTO credit_notes (number, customer_id, invoice_id, currency, total_minor,
				tax_minor, reason, status, issued_at, snapshot, created_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7, 'issued', now(), $8, $9)
			RETURNING id, issued_at`,
			number, customerID, req.InvoiceID, currency, req.AmountMinor, req.TaxMinor,
			req.Reason, map[string]any{
				"schema_version":  1,
				"invoice_id":      req.InvoiceID,
				"invoice_total":   totalMinor,
				"credited_before": credited,
				"amount":          req.AmountMinor,
				"tax":             req.TaxMinor,
				"reason":          req.Reason,
			}, req.IssuedBy).Scan(&id, &issuedAt); err != nil {
			return fmt.Errorf("creditnote: insert: %w", err)
		}

		// Debit revenue (and tax payable), credit the customer's credit balance. The tax share is
		// separated because a tax authority cares about the difference: crediting revenue for the tax
		// would understate what is owed.
		revenueMinor := req.AmountMinor - req.TaxMinor
		debits := []ledger.Entry{{Account: ledger.AccountRevenue, Amount: money.New(revenueMinor, cur)}}
		if req.TaxMinor > 0 {
			debits = append(debits,
				ledger.Entry{Account: ledger.AccountTaxPayable, Amount: money.New(req.TaxMinor, cur)})
		}
		if _, _, err := ledger.Post(ctx, tx, ledger.Posting{
			Kind:        ledger.KindCreditNote,
			Reference:   ledger.RefRefund, // credit notes share the "refund" reference family
			ReferenceID: id,
			CustomerID:  customerID,
			Currency:    cur,
			Debits:      debits,
			Credits: []ledger.Entry{{
				Account: ledger.AccountCredit,
				Amount:  money.New(req.AmountMinor, cur),
			}},
		}); err != nil {
			return fmt.Errorf("creditnote: post: %w", err)
		}

		if err := audit.Record(ctx, tx, audit.Event{
			Actor:      audit.Actor{ID: req.IssuedBy, Kind: audit.ActorUser},
			Action:     "credit_note.issue",
			TargetType: "credit_note",
			TargetID:   id,
			Outcome:    audit.OutcomeSuccess,
			Changes: map[string]any{
				"invoice_id": req.InvoiceID,
				"number":     number,
				"amount":     req.AmountMinor,
				"tax":        req.TaxMinor,
				"reason":     req.Reason,
			},
		}); err != nil {
			return err
		}

		out = CreditNote{
			ID: id, Number: number, CustomerID: customerID, InvoiceID: req.InvoiceID,
			Currency: currency, TotalMinor: req.AmountMinor, TaxMinor: req.TaxMinor,
			Reason: req.Reason, Status: StatusIssued, IssuedAt: &issuedAt,
		}
		return nil
	})
	if err != nil {
		return CreditNote{}, err
	}
	return out, nil
}

// allocateNumber consumes the next number in the credit-note series.
//
// The UPDATE ... RETURNING is the allocation: two concurrent callers get different numbers because
// the row lock serialises them, and a rolled-back transaction returns the number to the sequence
// rather than leaving a gap.
func allocateNumber(ctx context.Context, tx pgx.Tx) (string, error) {
	var next int64
	err := tx.QueryRow(ctx, `
		INSERT INTO invoice_number_sequences (scope, next_value)
		VALUES ($1, 2)
		ON CONFLICT (scope) DO UPDATE
			SET next_value = invoice_number_sequences.next_value + 1
		RETURNING next_value - 1`, CreditSeries.Scope).Scan(&next)
	if err != nil {
		return "", fmt.Errorf("creditnote: allocate number: %w", err)
	}
	digits := strconv.FormatInt(next, 10)
	for len(digits) < CreditSeries.Width {
		digits = "0" + digits
	}
	return CreditSeries.Prefix + digits, nil
}

// Get returns one credit note.
func (s *Service) Get(ctx context.Context, id string) (CreditNote, error) {
	return scanNote(s.pool.QueryRow(ctx, selectColumns+` WHERE id = $1`, id))
}

// List returns credit notes, newest first.
func (s *Service) List(ctx context.Context, invoiceID string, limit int, cursor string) ([]CreditNote, string, error) {
	limit = paginate.Clamp(limit)
	args := []any{}
	where := "true"
	if invoiceID != "" {
		args = append(args, invoiceID)
		where = fmt.Sprintf("invoice_id = $%d", len(args))
	}
	if cursor != "" {
		sortKey, id, err := paginate.Decode(cursor)
		if err != nil {
			return nil, "", err
		}
		when, perr := time.Parse(time.RFC3339Nano, sortKey)
		if perr != nil {
			return nil, "", fmt.Errorf("%w: bad timestamp", paginate.ErrInvalidCursor)
		}
		args = append(args, when, id)
		where += fmt.Sprintf(" AND (created_at, id) < ($%d, $%d)", len(args)-1, len(args))
	}
	args = append(args, limit+1)

	rows, err := s.pool.Query(ctx, selectColumns+` WHERE `+where+
		` ORDER BY created_at DESC, id DESC LIMIT $`+fmt.Sprint(len(args)), args...)
	if err != nil {
		return nil, "", fmt.Errorf("creditnote: list: %w", err)
	}
	defer rows.Close()

	out := make([]CreditNote, 0, limit)
	for rows.Next() {
		n, err := scanNote(rows)
		if err != nil {
			return nil, "", err
		}
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("creditnote: list: %w", err)
	}

	next := ""
	if len(out) > limit {
		out = out[:limit]
		last := out[len(out)-1]
		next = paginate.Encode(last.CreatedAt.UTC().Format(time.RFC3339Nano), last.ID)
	}
	return out, next, nil
}

// ------------------------------------------------------------------ applying --

// ApplyRequest describes applying credit to an invoice.
type ApplyRequest struct {
	CustomerID string
	InvoiceID  string
	// AmountMinor is the part to apply. Zero means "as much as possible", bounded by both the
	// customer's credit and what the invoice still owes.
	AmountMinor int64
	AppliedBy   string
}

// Application is one use of customer credit.
type Application struct {
	ID          string
	CustomerID  string
	InvoiceID   string
	AmountMinor int64
	Currency    string
	CreatedAt   time.Time
	// InvoicePaid is true when this application settled the invoice.
	InvoicePaid bool
}

// Apply spends customer credit on an invoice.
//
// Both the invoice and the customer's credit balance are read under a lock: the first so two
// applications cannot each think the invoice still owes the full amount, the second so two
// applications cannot each spend the same credit.
func (s *Service) Apply(ctx context.Context, req ApplyRequest) (Application, error) {
	if req.CustomerID == "" || req.InvoiceID == "" {
		return Application{}, errors.New("creditnote: a customer and an invoice are required")
	}

	var out Application
	err := s.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var (
			invoiceCustomer, currency string
			totalMinor, paidMinor     int64
		)
		err := tx.QueryRow(ctx, `
			SELECT customer_id, currency, total_minor, amount_paid_minor
			FROM invoices WHERE id = $1 FOR UPDATE`, req.InvoiceID).
			Scan(&invoiceCustomer, &currency, &totalMinor, &paidMinor)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("creditnote: lock invoice: %w", err)
		}
		if invoiceCustomer != req.CustomerID {
			// The invoice belongs to someone else. Answering not-found rather than forbidden keeps
			// another customer's invoice indistinguishable from a nonexistent one.
			return ErrNotFound
		}

		cur, err := currencyOf(currency)
		if err != nil {
			return err
		}

		owed := totalMinor - paidMinor
		if owed <= 0 {
			return fmt.Errorf("%w: the invoice is already settled", ErrInsufficientCredit)
		}

		// The customer's credit, read from the subledger rather than a column: the subledger is the
		// authority, and a cached balance would drift.
		available, err := creditBalance(ctx, tx, req.CustomerID, cur)
		if err != nil {
			return err
		}
		if available <= 0 {
			return fmt.Errorf("%w: the customer has no credit in %s", ErrInsufficientCredit, cur.Code())
		}

		amount := req.AmountMinor
		if amount == 0 {
			amount = min64(owed, available)
		}
		if amount <= 0 {
			return fmt.Errorf("%w: the amount must be positive", ErrInsufficientCredit)
		}
		if amount > available {
			return fmt.Errorf("%w: %s requested, %s available", ErrInsufficientCredit,
				money.New(amount, cur).Format(), money.New(available, cur).Format())
		}
		if amount > owed {
			// Applying more than is owed would leave the customer with a negative invoice, which is
			// a credit note, not a payment.
			return fmt.Errorf("%w: %s requested, %s owed", ErrExceedsInvoice,
				money.New(amount, cur).Format(), money.New(owed, cur).Format())
		}

		var id string
		var createdAt time.Time
		if err := tx.QueryRow(ctx, `
			INSERT INTO credit_applications (customer_id, invoice_id, amount_minor, currency, applied_by)
			VALUES ($1, $2, $3, $4, $5) RETURNING id, created_at`,
			req.CustomerID, req.InvoiceID, amount, currency, req.AppliedBy).
			Scan(&id, &createdAt); err != nil {
			return fmt.Errorf("creditnote: record application: %w", err)
		}

		// Debit customer credit, credit receivable. The application's own id is the reference, which
		// is what makes a replay a no-op instead of a second application.
		if _, _, err := ledger.Post(ctx, tx, ledger.Posting{
			Kind:        ledger.KindAdjustment,
			Reference:   ledger.RefCreditApplication,
			ReferenceID: id,
			CustomerID:  req.CustomerID,
			Currency:    cur,
			Debits:      []ledger.Entry{{Account: ledger.AccountCredit, Amount: money.New(amount, cur)}},
			Credits:     []ledger.Entry{{Account: ledger.AccountReceivable, Amount: money.New(amount, cur)}},
		}); err != nil {
			return fmt.Errorf("creditnote: post application: %w", err)
		}

		// The invoice's paid amount moves; the invoice may become paid. The 0008 trigger validates
		// the transition, so an illegal move is refused by the database rather than trusted here.
		newPaid := paidMinor + amount
		newStatus := "open"
		if newPaid >= totalMinor {
			newStatus = "paid"
			newPaid = totalMinor
		}
		if _, err := tx.Exec(ctx, `
			UPDATE invoices SET amount_paid_minor = $2, status = $3 WHERE id = $1`,
			req.InvoiceID, newPaid, newStatus); err != nil {
			return fmt.Errorf("creditnote: update invoice: %w", err)
		}

		if err := audit.Record(ctx, tx, audit.Event{
			Actor:      audit.Actor{ID: req.AppliedBy, Kind: audit.ActorUser},
			Action:     "credit.apply",
			TargetType: "invoice",
			TargetID:   req.InvoiceID,
			Outcome:    audit.OutcomeSuccess,
			Changes: map[string]any{
				"application_id": id,
				"amount":         amount,
				"invoice_status": newStatus,
			},
		}); err != nil {
			return err
		}

		out = Application{
			ID: id, CustomerID: req.CustomerID, InvoiceID: req.InvoiceID,
			AmountMinor: amount, Currency: currency, CreatedAt: createdAt,
			InvoicePaid: newStatus == "paid",
		}
		return nil
	})
	if err != nil {
		return Application{}, err
	}
	return out, nil
}

// CreditBalance returns a customer's available credit per currency.
func (s *Service) CreditBalance(ctx context.Context, customerID string) ([]money.Amount, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT currency, SUM(credit_minor - debit_minor) AS balance
		FROM journal_entries
		WHERE customer_id = $1 AND account_code = $2
		GROUP BY currency HAVING SUM(credit_minor - debit_minor) > 0
		ORDER BY currency`, customerID, ledger.AccountCredit)
	if err != nil {
		return nil, fmt.Errorf("creditnote: read credit balance: %w", err)
	}
	defer rows.Close()

	out := []money.Amount{}
	for rows.Next() {
		var (
			code  string
			minor int64
		)
		if err := rows.Scan(&code, &minor); err != nil {
			return nil, fmt.Errorf("creditnote: scan credit balance: %w", err)
		}
		cur, err := currencyOf(code)
		if err != nil {
			return nil, err
		}
		out = append(out, money.New(minor, cur))
	}
	return out, rows.Err()
}

// creditBalance reads one currency's credit balance inside a transaction.
func creditBalance(ctx context.Context, tx pgx.Tx, customerID string, cur money.Currency) (int64, error) {
	var minor int64
	err := tx.QueryRow(ctx, `
		SELECT COALESCE(SUM(credit_minor - debit_minor), 0)
		FROM journal_entries
		WHERE customer_id = $1 AND account_code = $2 AND currency = $3`,
		customerID, ledger.AccountCredit, cur.Code()).Scan(&minor)
	if err != nil {
		return 0, fmt.Errorf("creditnote: read credit: %w", err)
	}
	return minor, nil
}

const selectColumns = `
	SELECT id, COALESCE(number, ''), customer_id, COALESCE(invoice_id::text, ''), currency,
	       total_minor, tax_minor, reason, status, issued_at, created_at
	FROM credit_notes`

type scanner interface{ Scan(...any) error }

func scanNote(row scanner) (CreditNote, error) {
	var c CreditNote
	err := row.Scan(&c.ID, &c.Number, &c.CustomerID, &c.InvoiceID, &c.Currency, &c.TotalMinor,
		&c.TaxMinor, &c.Reason, &c.Status, &c.IssuedAt, &c.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return CreditNote{}, ErrNotFound
	}
	if err != nil {
		return CreditNote{}, fmt.Errorf("creditnote: scan: %w", err)
	}
	return c, nil
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

// currencyOf resolves a persisted currency code.
func currencyOf(code string) (money.Currency, error) {
	if cur, ok := money.LookupCurrency(code); ok {
		return cur, nil
	}
	return money.NewCurrency(code, 2)
}
