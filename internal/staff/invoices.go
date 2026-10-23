package staff

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/catalyst/billing/internal/paginate"
	"github.com/jackc/pgx/v5"
)

// jsonUnmarshal is json.Unmarshal, named so the file reads uniformly with the other
// decoders and so a future switch to a faster parser is one place.
func jsonUnmarshal(raw []byte, dst any) error { return json.Unmarshal(raw, dst) }

// encodeCursorValue and decodeCursorValue handle cursors whose sort key is a string rather
// than a timestamp.
func encodeCursorValue(sortKey, id string) string { return paginate.Encode(sortKey, id) }

func decodeCursorValue(cursor string) (string, string, error) {
	sortKey, id, err := paginate.Decode(cursor)
	if err != nil {
		return "", "", err
	}
	return sortKey, id, nil
}

// InvoiceFilter narrows the invoice list.
type InvoiceFilter struct {
	Status     string
	CustomerID string
	Currency   string
	// Overdue selects only open invoices past their due date. It is a derived filter: the
	// column does not exist, and storing it would make it wrong between job runs.
	Overdue bool
	From    *time.Time
	To      *time.Time
}

// Invoices lists invoices across all customers, newest first.
func (s *Service) Invoices(ctx context.Context, f InvoiceFilter, limit int, cursor string) ([]InvoiceRow, string, error) {
	limit = pageLimit(limit)

	var b queryBuilder
	if f.Status != "" {
		b.add("i.status = $%d", f.Status)
	}
	if f.CustomerID != "" {
		b.add("i.customer_id = $%d", f.CustomerID)
	}
	if f.Currency != "" {
		b.add("i.currency = $%d", f.Currency)
	}
	if f.Overdue {
		b.addRaw("i.status = 'open' AND i.due_at < now()")
	}
	if f.From != nil {
		b.add("i.issued_at >= $%d", *f.From)
	}
	if f.To != nil {
		b.add("i.issued_at <= $%d", *f.To)
	}
	if cursor != "" {
		when, id, err := decodeTimeCursor(cursor)
		if err != nil {
			return nil, "", err
		}
		b.args = append(b.args, when, id)
		// COALESCE matches the ORDER BY: a draft has no issued_at, and a cursor that
		// compared NULL to a timestamp would silently stop paging.
		b.where = append(b.where, fmt.Sprintf(
			"(COALESCE(i.issued_at, i.created_at), i.id) < ($%d, $%d)", len(b.args)-1, len(b.args)))
	}
	b.args = append(b.args, limit+1)

	stmt := `
		SELECT i.id, COALESCE(i.number, ''), i.status, i.currency, i.total_minor,
		       i.amount_paid_minor, i.issued_at, i.due_at, i.customer_id, c.email,
		       (i.status = 'open' AND i.due_at IS NOT NULL AND i.due_at < now())
		FROM invoices i
		JOIN customers c ON c.id = i.customer_id
		WHERE ` + b.whereClause() + `
		ORDER BY COALESCE(i.issued_at, i.created_at) DESC, i.id DESC
		LIMIT $` + fmt.Sprint(len(b.args))

	rows, err := s.pool.Query(ctx, stmt, b.args...)
	if err != nil {
		return nil, "", fmt.Errorf("staff: list invoices: %w", err)
	}
	defer rows.Close()

	out := make([]InvoiceRow, 0, limit)
	var sortKeys []time.Time
	for rows.Next() {
		row, sortKey, err := scanInvoice(rows)
		if err != nil {
			return nil, "", err
		}
		out = append(out, row)
		sortKeys = append(sortKeys, sortKey)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("staff: read invoices: %w", err)
	}

	next := ""
	if len(out) > limit {
		out = out[:limit]
		last := out[len(out)-1]
		next = CursorOf(sortKeys[len(out)-1], last.ID)
	}
	return out, next, nil
}

// scanInvoice reads one invoice row, returning its sort key alongside.
func scanInvoice(row interface{ Scan(...any) error }) (InvoiceRow, time.Time, error) {
	var (
		inv      InvoiceRow
		issuedAt *time.Time
		dueAt    *time.Time
		sortKey  time.Time
	)
	if err := row.Scan(&inv.ID, &inv.Number, &inv.Status, &inv.Currency, &inv.Total,
		&inv.Paid, &issuedAt, &dueAt, &inv.CustomerID, &inv.CustomerEmail,
		&inv.Overdue); err != nil {
		return InvoiceRow{}, time.Time{}, fmt.Errorf("staff: scan invoice: %w", err)
	}
	inv.Due = inv.Total - inv.Paid
	inv.IssuedAt = issuedAt
	inv.DueAt = dueAt
	switch {
	case issuedAt != nil:
		sortKey = *issuedAt
	default:
		// A draft has no issued_at; its position in the list is by creation, which the
		// query's COALESCE already assumes.
		sortKey = time.Now().UTC()
	}
	return inv, sortKey, nil
}

// invoicesFor lists one customer's invoices.
func (s *Service) invoicesFor(ctx context.Context, customerID string, limit int) ([]InvoiceRow, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT i.id, COALESCE(i.number, ''), i.status, i.currency, i.total_minor,
		       i.amount_paid_minor, i.issued_at, i.due_at, i.customer_id, c.email,
		       (i.status = 'open' AND i.due_at IS NOT NULL AND i.due_at < now())
		FROM invoices i
		JOIN customers c ON c.id = i.customer_id
		WHERE i.customer_id = $1
		ORDER BY COALESCE(i.issued_at, i.created_at) DESC
		LIMIT $2`, customerID, limit)
	if err != nil {
		return nil, fmt.Errorf("staff: list customer invoices: %w", err)
	}
	defer rows.Close()

	out := []InvoiceRow{}
	for rows.Next() {
		row, _, err := scanInvoice(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// InvoiceLine is one invoice line.
type InvoiceLine struct {
	Position    int
	Description string
	Quantity    string
	UnitMinor   int64
	NetMinor    int64
	TaxRate     string
	TaxMinor    int64
	TotalMinor  int64
}

// PaymentRow is one recorded payment.
type PaymentRow struct {
	ID                string
	Provider          string
	ProviderAccount   string
	ProviderPaymentID string
	Amount            int64
	Currency          string
	Status            string
	ReceivedAt        time.Time
	// Method and Note are set only on payments a staff member recorded outside a gateway
	// (provider='manual'). They are nil for gateway payments, whose method the provider owns
	// and which have no operator note — nil here means "not an offline payment", not "unknown".
	Method *string
	Note   *string
}

// AllocationRow is one payment allocation.
type AllocationRow struct {
	ID        string
	PaymentID string
	InvoiceID string
	Amount    int64
	Currency  string
	CreatedAt time.Time
}

// RefundRow is one refund.
type RefundRow struct {
	ID               string
	PaymentID        string
	ProviderRefundID string
	OperationKey     string
	Amount           int64
	Currency         string
	Status           string
	Reason           string
	CreatedAt        time.Time
	FinishedAt       *time.Time
}

// CreditNoteRow is one adjustment note. The name predates debit notes; the table is shared
// and `Kind` is what tells the direction apart (INV-10).
type CreditNoteRow struct {
	ID         string
	Number     string
	InvoiceID  string
	Currency   string
	TotalMinor int64
	TaxMinor   int64
	Reason     string
	Status     string
	// Kind is "credit" or "debit".
	Kind string
	// VoidReason and VoidedAt are set when the note was cancelled.
	VoidReason string
	VoidedAt   *time.Time
	IssuedAt   *time.Time
	CreatedAt  time.Time
}

// JournalEntry is one posting line.
type JournalEntry struct {
	AccountCode string
	DebitMinor  int64
	CreditMinor int64
	Memo        string
}

// JournalTransaction is one balanced posting.
type JournalTransaction struct {
	ID            string
	Kind          string
	ReferenceType string
	ReferenceID   string
	Currency      string
	CreatedAt     time.Time
	Entries       []JournalEntry
}

// InvoiceDetail is the full invoice view.
type InvoiceDetail struct {
	Invoice     InvoiceRow
	Snapshot    map[string]any
	Lines       []InvoiceLine
	Payments    []PaymentRow
	Allocations []AllocationRow
	Refunds     []RefundRow
	CreditNotes []CreditNoteRow
	Journal     []JournalTransaction
	Customer    CustomerRow
}

// Invoice returns one invoice with everything needed to explain its total.
//
// The snapshot is returned verbatim: it is the evidence of what was billed, and an operator
// debugging a total needs the document the customer received, not a reconstruction.
func (s *Service) Invoice(ctx context.Context, id string) (InvoiceDetail, error) {
	var (
		out      InvoiceDetail
		issuedAt *time.Time
		dueAt    *time.Time
		snapshot []byte
	)
	err := s.pool.QueryRow(ctx, `
		SELECT i.id, COALESCE(i.number, ''), i.status, i.currency, i.total_minor,
		       i.amount_paid_minor, i.issued_at, i.due_at, i.customer_id, c.email,
		       (i.status = 'open' AND i.due_at IS NOT NULL AND i.due_at < now()),
		       i.snapshot
		FROM invoices i
		JOIN customers c ON c.id = i.customer_id
		WHERE i.id = $1`, id).
		Scan(&out.Invoice.ID, &out.Invoice.Number, &out.Invoice.Status, &out.Invoice.Currency,
			&out.Invoice.Total, &out.Invoice.Paid, &issuedAt, &dueAt,
			&out.Invoice.CustomerID, &out.Invoice.CustomerEmail, &out.Invoice.Overdue, &snapshot)
	if errors.Is(err, pgx.ErrNoRows) {
		return InvoiceDetail{}, ErrNotFound
	}
	if err != nil {
		return InvoiceDetail{}, fmt.Errorf("staff: load invoice: %w", err)
	}
	out.Invoice.Due = out.Invoice.Total - out.Invoice.Paid
	out.Invoice.IssuedAt = issuedAt
	out.Invoice.DueAt = dueAt
	if len(snapshot) > 0 {
		if err := jsonUnmarshal(snapshot, &out.Snapshot); err != nil {
			return InvoiceDetail{}, fmt.Errorf("staff: decode snapshot: %w", err)
		}
	}

	if out.Customer, err = s.customerBrief(ctx, out.Invoice.CustomerID); err != nil {
		return InvoiceDetail{}, err
	}
	if out.Lines, err = s.invoiceLines(ctx, id); err != nil {
		return InvoiceDetail{}, err
	}
	if out.Payments, err = s.paymentsForInvoice(ctx, id); err != nil {
		return InvoiceDetail{}, err
	}
	if out.Allocations, err = s.allocationsForInvoice(ctx, id); err != nil {
		return InvoiceDetail{}, err
	}
	if out.Refunds, err = s.refundsForInvoice(ctx, id); err != nil {
		return InvoiceDetail{}, err
	}
	if out.CreditNotes, err = s.creditNotesForInvoice(ctx, id); err != nil {
		return InvoiceDetail{}, err
	}
	if out.Journal, err = s.journalForInvoice(ctx, id); err != nil {
		return InvoiceDetail{}, err
	}
	return out, nil
}

// customerBrief loads the name and email for an invoice header.
func (s *Service) customerBrief(ctx context.Context, id string) (CustomerRow, error) {
	var row CustomerRow
	err := s.pool.QueryRow(ctx, `
		SELECT id, email, name, created_at FROM customers WHERE id = $1`, id).
		Scan(&row.ID, &row.Email, &row.Name, &row.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return CustomerRow{}, ErrNotFound
	}
	if err != nil {
		return CustomerRow{}, fmt.Errorf("staff: load customer brief: %w", err)
	}
	return row, nil
}

func (s *Service) invoiceLines(ctx context.Context, invoiceID string) ([]InvoiceLine, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT position, description, quantity::text, unit_minor, net_minor, tax_rate,
		       tax_minor, total_minor
		FROM invoice_lines WHERE invoice_id = $1 ORDER BY position`, invoiceID)
	if err != nil {
		return nil, fmt.Errorf("staff: list invoice lines: %w", err)
	}
	defer rows.Close()

	out := []InvoiceLine{}
	for rows.Next() {
		var line InvoiceLine
		if err := rows.Scan(&line.Position, &line.Description, &line.Quantity, &line.UnitMinor,
			&line.NetMinor, &line.TaxRate, &line.TaxMinor, &line.TotalMinor); err != nil {
			return nil, fmt.Errorf("staff: scan invoice line: %w", err)
		}
		out = append(out, line)
	}
	return out, rows.Err()
}

// paymentsForInvoice returns the payments allocated to an invoice.
func (s *Service) paymentsForInvoice(ctx context.Context, invoiceID string) ([]PaymentRow, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT p.id, p.provider, p.provider_account, p.provider_payment_id,
		       p.amount_minor, p.currency, p.status, p.received_at, p.method, p.note
		FROM payments p
		JOIN payment_allocations pa ON pa.payment_id = p.id
		WHERE pa.invoice_id = $1
		ORDER BY p.received_at DESC`, invoiceID)
	if err != nil {
		return nil, fmt.Errorf("staff: list invoice payments: %w", err)
	}
	defer rows.Close()

	out := []PaymentRow{}
	for rows.Next() {
		var row PaymentRow
		if err := rows.Scan(&row.ID, &row.Provider, &row.ProviderAccount,
			&row.ProviderPaymentID, &row.Amount, &row.Currency, &row.Status,
			&row.ReceivedAt, &row.Method, &row.Note); err != nil {
			return nil, fmt.Errorf("staff: scan invoice payment: %w", err)
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

func (s *Service) allocationsForInvoice(ctx context.Context, invoiceID string) ([]AllocationRow, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, payment_id, invoice_id, amount_minor, currency, created_at
		FROM payment_allocations WHERE invoice_id = $1 ORDER BY created_at`, invoiceID)
	if err != nil {
		return nil, fmt.Errorf("staff: list allocations: %w", err)
	}
	defer rows.Close()

	out := []AllocationRow{}
	for rows.Next() {
		var row AllocationRow
		if err := rows.Scan(&row.ID, &row.PaymentID, &row.InvoiceID, &row.Amount,
			&row.Currency, &row.CreatedAt); err != nil {
			return nil, fmt.Errorf("staff: scan allocation: %w", err)
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// refundsForInvoice returns the refunds of any payment allocated to the invoice.
func (s *Service) refundsForInvoice(ctx context.Context, invoiceID string) ([]RefundRow, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT r.id, r.payment_id, COALESCE(r.provider_refund_id, ''), r.operation_key,
		       r.amount_minor, r.currency, r.status, r.reason, r.created_at, r.finished_at
		FROM refunds r
		WHERE r.payment_id IN (SELECT payment_id FROM payment_allocations WHERE invoice_id = $1)
		ORDER BY r.created_at DESC`, invoiceID)
	if err != nil {
		return nil, fmt.Errorf("staff: list refunds: %w", err)
	}
	defer rows.Close()

	out := []RefundRow{}
	for rows.Next() {
		var row RefundRow
		if err := rows.Scan(&row.ID, &row.PaymentID, &row.ProviderRefundID, &row.OperationKey,
			&row.Amount, &row.Currency, &row.Status, &row.Reason, &row.CreatedAt,
			&row.FinishedAt); err != nil {
			return nil, fmt.Errorf("staff: scan refund: %w", err)
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

func (s *Service) creditNotesForInvoice(ctx context.Context, invoiceID string) ([]CreditNoteRow, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, COALESCE(number, ''), COALESCE(invoice_id::text, ''), currency,
		       total_minor, tax_minor, reason, status, kind, COALESCE(void_reason, ''),
		       voided_at, issued_at, created_at
		FROM credit_notes WHERE invoice_id = $1 ORDER BY created_at DESC`, invoiceID)
	if err != nil {
		return nil, fmt.Errorf("staff: list credit notes: %w", err)
	}
	defer rows.Close()

	out := []CreditNoteRow{}
	for rows.Next() {
		var row CreditNoteRow
		if err := rows.Scan(&row.ID, &row.Number, &row.InvoiceID, &row.Currency,
			&row.TotalMinor, &row.TaxMinor, &row.Reason, &row.Status, &row.Kind,
			&row.VoidReason, &row.VoidedAt, &row.IssuedAt, &row.CreatedAt); err != nil {
			return nil, fmt.Errorf("staff: scan credit note: %w", err)
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// journalForInvoice reads the postings that reference the invoice.
//
// The journal is read-only everywhere in this API. There is no endpoint that edits a
// posting, because a correction is a reversal posting, not an update.
func (s *Service) journalForInvoice(ctx context.Context, invoiceID string) ([]JournalTransaction, error) {
	return s.journal(ctx, "invoice", invoiceID)
}

// journal reads every posting for one reference.
func (s *Service) journal(ctx context.Context, referenceType, referenceID string) ([]JournalTransaction, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT jt.id, jt.kind, jt.reference_type, jt.reference_id, jt.currency, jt.created_at,
		       je.account_code, je.debit_minor, je.credit_minor, je.memo
		FROM journal_transactions jt
		JOIN journal_entries je ON je.transaction_id = jt.id
		WHERE jt.reference_type = $1 AND jt.reference_id = $2
		ORDER BY jt.created_at, je.id`, referenceType, referenceID)
	if err != nil {
		return nil, fmt.Errorf("staff: read journal: %w", err)
	}
	defer rows.Close()

	var (
		out  []JournalTransaction
		byID = map[string]int{}
	)
	for rows.Next() {
		var (
			txID, kind, refType, refID, currency string
			createdAt                            time.Time
			entry                                JournalEntry
		)
		if err := rows.Scan(&txID, &kind, &refType, &refID, &currency, &createdAt,
			&entry.AccountCode, &entry.DebitMinor, &entry.CreditMinor, &entry.Memo); err != nil {
			return nil, fmt.Errorf("staff: scan journal entry: %w", err)
		}
		idx, ok := byID[txID]
		if !ok {
			out = append(out, JournalTransaction{
				ID: txID, Kind: kind, ReferenceType: refType, ReferenceID: refID,
				Currency: currency, CreatedAt: createdAt,
			})
			idx = len(out) - 1
			byID[txID] = idx
		}
		out[idx].Entries = append(out[idx].Entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("staff: read journal: %w", err)
	}
	if out == nil {
		out = []JournalTransaction{}
	}
	return out, nil
}

// PaymentFilter narrows the payment history.
type PaymentFilter struct {
	Provider   string
	Status     string
	CustomerID string
	// Method filters offline payments by how the money arrived. It is exact, and a
	// non-empty value that no row carries simply returns an empty page.
	Method string
	From   *time.Time
	To     *time.Time
}

// PaymentDetail is the payment view.
type PaymentDetail struct {
	Payment     PaymentRow
	Allocations []AllocationRow
	Refunds     []RefundRow
	Journal     []JournalTransaction
}

// Payments lists payments, newest first.
func (s *Service) Payments(ctx context.Context, f PaymentFilter, limit int, cursor string) ([]PaymentRow, []string, string, error) {
	limit = pageLimit(limit)

	var b queryBuilder
	if f.Provider != "" {
		b.add("p.provider = $%d", f.Provider)
	}
	if f.Status != "" {
		b.add("p.status = $%d", f.Status)
	}
	if f.Method != "" {
		b.add("p.method = $%d", f.Method)
	}
	if f.CustomerID != "" {
		b.add(`EXISTS (SELECT 1 FROM payment_allocations pa
		                JOIN invoices i ON i.id = pa.invoice_id
		                WHERE pa.payment_id = p.id AND i.customer_id = $%d)`, f.CustomerID)
	}
	if f.From != nil {
		b.add("p.received_at >= $%d", *f.From)
	}
	if f.To != nil {
		b.add("p.received_at <= $%d", *f.To)
	}
	if cursor != "" {
		when, id, err := decodeTimeCursor(cursor)
		if err != nil {
			return nil, nil, "", err
		}
		b.args = append(b.args, when, id)
		b.where = append(b.where, fmt.Sprintf("(p.received_at, p.id) < ($%d, $%d)",
			len(b.args)-1, len(b.args)))
	}
	b.args = append(b.args, limit+1)

	stmt := `
		SELECT p.id, p.provider, p.provider_account, p.provider_payment_id,
		       p.amount_minor, p.currency, p.status, p.received_at, p.method, p.note
		FROM payments p
		WHERE ` + b.whereClause() + `
		ORDER BY p.received_at DESC, p.id DESC
		LIMIT $` + fmt.Sprint(len(b.args))

	rows, err := s.pool.Query(ctx, stmt, b.args...)
	if err != nil {
		return nil, nil, "", fmt.Errorf("staff: list payments: %w", err)
	}
	defer rows.Close()

	out := make([]PaymentRow, 0, limit)
	for rows.Next() {
		var row PaymentRow
		if err := rows.Scan(&row.ID, &row.Provider, &row.ProviderAccount,
			&row.ProviderPaymentID, &row.Amount, &row.Currency, &row.Status,
			&row.ReceivedAt, &row.Method, &row.Note); err != nil {
			return nil, nil, "", fmt.Errorf("staff: scan payment: %w", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, "", fmt.Errorf("staff: read payments: %w", err)
	}

	next := ""
	if len(out) > limit {
		out = out[:limit]
		last := out[len(out)-1]
		next = CursorOf(last.ReceivedAt, last.ID)
	}

	// Which invoices each payment settled, in one query rather than one per row.
	invoiceIDs, err := s.invoiceIDsForPayments(ctx, out)
	if err != nil {
		return nil, nil, "", err
	}
	return out, invoiceIDs, next, nil
}

// invoiceIDsForPayments maps each payment to the invoices it settled.
func (s *Service) invoiceIDsForPayments(ctx context.Context, payments []PaymentRow) ([]string, error) {
	out := make([]string, len(payments))
	if len(payments) == 0 {
		return out, nil
	}
	ids := make([]string, len(payments))
	for i, p := range payments {
		ids[i] = p.ID
	}
	rows, err := s.pool.Query(ctx, `
		SELECT payment_id, string_agg(invoice_id::text, ',' ORDER BY created_at)
		FROM payment_allocations WHERE payment_id = ANY($1)
		GROUP BY payment_id`, ids)
	if err != nil {
		return nil, fmt.Errorf("staff: list payment invoices: %w", err)
	}
	defer rows.Close()

	byPayment := map[string]string{}
	for rows.Next() {
		var paymentID, invoices string
		if err := rows.Scan(&paymentID, &invoices); err != nil {
			return nil, fmt.Errorf("staff: scan payment invoices: %w", err)
		}
		byPayment[paymentID] = invoices
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("staff: read payment invoices: %w", err)
	}
	for i, p := range payments {
		out[i] = byPayment[p.ID]
	}
	return out, nil
}

// Payment returns one payment with its allocations, refunds and postings.
func (s *Service) Payment(ctx context.Context, id string) (PaymentDetail, error) {
	var out PaymentDetail
	err := s.pool.QueryRow(ctx, `
		SELECT id, provider, provider_account, provider_payment_id, amount_minor,
		       currency, status, received_at, method, note
		FROM payments WHERE id = $1`, id).
		Scan(&out.Payment.ID, &out.Payment.Provider, &out.Payment.ProviderAccount,
			&out.Payment.ProviderPaymentID, &out.Payment.Amount, &out.Payment.Currency,
			&out.Payment.Status, &out.Payment.ReceivedAt, &out.Payment.Method, &out.Payment.Note)
	if errors.Is(err, pgx.ErrNoRows) {
		return PaymentDetail{}, ErrNotFound
	}
	if err != nil {
		return PaymentDetail{}, fmt.Errorf("staff: load payment: %w", err)
	}

	rows, err := s.pool.Query(ctx, `
		SELECT id, payment_id, invoice_id, amount_minor, currency, created_at
		FROM payment_allocations WHERE payment_id = $1 ORDER BY created_at`, id)
	if err != nil {
		return PaymentDetail{}, fmt.Errorf("staff: list payment allocations: %w", err)
	}
	out.Allocations = []AllocationRow{}
	for rows.Next() {
		var row AllocationRow
		if err := rows.Scan(&row.ID, &row.PaymentID, &row.InvoiceID, &row.Amount,
			&row.Currency, &row.CreatedAt); err != nil {
			rows.Close()
			return PaymentDetail{}, fmt.Errorf("staff: scan payment allocation: %w", err)
		}
		out.Allocations = append(out.Allocations, row)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return PaymentDetail{}, fmt.Errorf("staff: read payment allocations: %w", err)
	}

	refundRows, err := s.pool.Query(ctx, `
		SELECT id, payment_id, COALESCE(provider_refund_id, ''), operation_key, amount_minor,
		       currency, status, reason, created_at, finished_at
		FROM refunds WHERE payment_id = $1 ORDER BY created_at DESC`, id)
	if err != nil {
		return PaymentDetail{}, fmt.Errorf("staff: list payment refunds: %w", err)
	}
	out.Refunds = []RefundRow{}
	for refundRows.Next() {
		var row RefundRow
		if err := refundRows.Scan(&row.ID, &row.PaymentID, &row.ProviderRefundID,
			&row.OperationKey, &row.Amount, &row.Currency, &row.Status, &row.Reason,
			&row.CreatedAt, &row.FinishedAt); err != nil {
			refundRows.Close()
			return PaymentDetail{}, fmt.Errorf("staff: scan payment refund: %w", err)
		}
		out.Refunds = append(out.Refunds, row)
	}
	refundRows.Close()
	if err := refundRows.Err(); err != nil {
		return PaymentDetail{}, fmt.Errorf("staff: read payment refunds: %w", err)
	}

	if out.Journal, err = s.journal(ctx, "payment", id); err != nil {
		return PaymentDetail{}, err
	}
	return out, nil
}

// OrderRow is one order.
type OrderRow struct {
	ID            string
	CustomerID    string
	CustomerEmail string
	Status        string
	Currency      string
	TotalMinor    int64
	CreatedAt     time.Time
	UpdatedAt     time.Time
	InvoiceID     string
}

// OrderItem is one order line with its catalog provenance.
type OrderItem struct {
	ID              string
	Description     string
	Quantity        int
	UnitMinor       int64
	LineMinor       int64
	SetupMinor      int64
	PriceVersionID  string
	ProvisionPlan   string
	ProvisionRegion string
}

// OrderDetail is the order view.
type OrderDetail struct {
	Order    OrderRow
	Items    []OrderItem
	Invoices []InvoiceRow
	Attempts []AttemptRow
}

// AttemptRow is one checkout attempt.
type AttemptRow struct {
	ID          string
	Gateway     string
	Status      string
	ProviderRef string
	AmountMinor int64
	Currency    string
	CreatedAt   time.Time
}

// OrderFilter narrows the order list.
type OrderFilter struct {
	Status     string
	CustomerID string
}

// Orders lists orders, newest first.
func (s *Service) Orders(ctx context.Context, f OrderFilter, limit int, cursor string) ([]OrderRow, string, error) {
	limit = pageLimit(limit)

	var b queryBuilder
	if f.Status != "" {
		b.add("o.status = $%d", f.Status)
	}
	if f.CustomerID != "" {
		b.add("o.customer_id = $%d", f.CustomerID)
	}
	if cursor != "" {
		when, id, err := decodeTimeCursor(cursor)
		if err != nil {
			return nil, "", err
		}
		b.args = append(b.args, when, id)
		b.where = append(b.where, fmt.Sprintf("(o.created_at, o.id) < ($%d, $%d)",
			len(b.args)-1, len(b.args)))
	}
	b.args = append(b.args, limit+1)

	stmt := `
		SELECT o.id, o.customer_id, c.email, o.status, o.currency, o.total_minor,
		       o.created_at, o.updated_at,
		       COALESCE((SELECT id::text FROM invoices WHERE order_id = o.id
		                  ORDER BY created_at LIMIT 1), '')
		FROM orders o
		JOIN customers c ON c.id = o.customer_id
		WHERE ` + b.whereClause() + `
		ORDER BY o.created_at DESC, o.id DESC
		LIMIT $` + fmt.Sprint(len(b.args))

	rows, err := s.pool.Query(ctx, stmt, b.args...)
	if err != nil {
		return nil, "", fmt.Errorf("staff: list orders: %w", err)
	}
	defer rows.Close()

	out := make([]OrderRow, 0, limit)
	for rows.Next() {
		var row OrderRow
		if err := rows.Scan(&row.ID, &row.CustomerID, &row.CustomerEmail, &row.Status,
			&row.Currency, &row.TotalMinor, &row.CreatedAt, &row.UpdatedAt,
			&row.InvoiceID); err != nil {
			return nil, "", fmt.Errorf("staff: scan order: %w", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("staff: read orders: %w", err)
	}

	next := ""
	if len(out) > limit {
		out = out[:limit]
		last := out[len(out)-1]
		next = CursorOf(last.CreatedAt, last.ID)
	}
	return out, next, nil
}

// Order returns one order with its items, invoices and attempts.
func (s *Service) Order(ctx context.Context, id string) (OrderDetail, error) {
	var out OrderDetail
	err := s.pool.QueryRow(ctx, `
		SELECT o.id, o.customer_id, c.email, o.status, o.currency, o.total_minor,
		       o.created_at, o.updated_at,
		       COALESCE((SELECT id::text FROM invoices WHERE order_id = o.id
		                  ORDER BY created_at LIMIT 1), '')
		FROM orders o
		JOIN customers c ON c.id = o.customer_id
		WHERE o.id = $1`, id).
		Scan(&out.Order.ID, &out.Order.CustomerID, &out.Order.CustomerEmail,
			&out.Order.Status, &out.Order.Currency, &out.Order.TotalMinor,
			&out.Order.CreatedAt, &out.Order.UpdatedAt, &out.Order.InvoiceID)
	if errors.Is(err, pgx.ErrNoRows) {
		return OrderDetail{}, ErrNotFound
	}
	if err != nil {
		return OrderDetail{}, fmt.Errorf("staff: load order: %w", err)
	}

	rows, err := s.pool.Query(ctx, `
		SELECT oi.id, oi.description, oi.quantity, oi.unit_minor, oi.line_minor,
		       oi.setup_minor, oi.price_version_id, pv.provision_plan, pv.provision_region
		FROM order_items oi
		JOIN price_versions pv ON pv.id = oi.price_version_id
		WHERE oi.order_id = $1
		ORDER BY oi.position`, id)
	if err != nil {
		return OrderDetail{}, fmt.Errorf("staff: list order items: %w", err)
	}
	out.Items = []OrderItem{}
	for rows.Next() {
		var item OrderItem
		if err := rows.Scan(&item.ID, &item.Description, &item.Quantity, &item.UnitMinor,
			&item.LineMinor, &item.SetupMinor, &item.PriceVersionID, &item.ProvisionPlan,
			&item.ProvisionRegion); err != nil {
			rows.Close()
			return OrderDetail{}, fmt.Errorf("staff: scan order item: %w", err)
		}
		out.Items = append(out.Items, item)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return OrderDetail{}, fmt.Errorf("staff: read order items: %w", err)
	}

	invoiceRows, err := s.pool.Query(ctx, `
		SELECT i.id, COALESCE(i.number, ''), i.status, i.currency, i.total_minor,
		       i.amount_paid_minor, i.issued_at, i.due_at, i.customer_id, c.email,
		       (i.status = 'open' AND i.due_at IS NOT NULL AND i.due_at < now())
		FROM invoices i
		JOIN customers c ON c.id = i.customer_id
		WHERE i.order_id = $1
		ORDER BY i.created_at DESC`, id)
	if err != nil {
		return OrderDetail{}, fmt.Errorf("staff: list order invoices: %w", err)
	}
	out.Invoices = []InvoiceRow{}
	for invoiceRows.Next() {
		row, _, err := scanInvoice(invoiceRows)
		if err != nil {
			invoiceRows.Close()
			return OrderDetail{}, err
		}
		out.Invoices = append(out.Invoices, row)
	}
	invoiceRows.Close()
	if err := invoiceRows.Err(); err != nil {
		return OrderDetail{}, fmt.Errorf("staff: read order invoices: %w", err)
	}

	attemptRows, err := s.pool.Query(ctx, `
		SELECT pa.id, pa.gateway, pa.status, COALESCE(pa.provider_ref, ''),
		       pa.amount_minor, pa.currency, pa.created_at
		FROM payment_attempts pa
		WHERE pa.invoice_id IN (SELECT id FROM invoices WHERE order_id = $1)
		ORDER BY pa.created_at DESC`, id)
	if err != nil {
		return OrderDetail{}, fmt.Errorf("staff: list order attempts: %w", err)
	}
	out.Attempts = []AttemptRow{}
	for attemptRows.Next() {
		var row AttemptRow
		if err := attemptRows.Scan(&row.ID, &row.Gateway, &row.Status, &row.ProviderRef,
			&row.AmountMinor, &row.Currency, &row.CreatedAt); err != nil {
			attemptRows.Close()
			return OrderDetail{}, fmt.Errorf("staff: scan order attempt: %w", err)
		}
		out.Attempts = append(out.Attempts, row)
	}
	attemptRows.Close()
	if err := attemptRows.Err(); err != nil {
		return OrderDetail{}, fmt.Errorf("staff: read order attempts: %w", err)
	}
	return out, nil
}

// ProductRow is one catalog product with its versions.
type ProductRow struct {
	ID   string
	Slug string
	Name string
	// CreatedAt orders the list.
	CreatedAt time.Time
	// PriceVersions is populated by the detail read and by the list.
	PriceVersions []PriceVersionRow
	// ActiveSubscriptions explains why a version cannot be deleted, which is the question
	// the editor will ask.
	ActiveSubscriptions int
}

// PriceVersionRow is one immutable price version.
type PriceVersionRow struct {
	ID              string
	Version         int
	Currency        string
	AmountMinor     int64
	SetupMinor      int64
	Interval        string
	AnchorDay       int
	AnchorTimezone  string
	ProvisionPlan   string
	ProvisionRegion string
	CreatedAt       time.Time
}

// Products lists the catalog with every price version.
func (s *Service) Products(ctx context.Context, limit int, cursor string) ([]ProductRow, string, error) {
	limit = pageLimit(limit)

	var b queryBuilder
	if cursor != "" {
		when, id, err := decodeTimeCursor(cursor)
		if err != nil {
			return nil, "", err
		}
		b.args = append(b.args, when, id)
		b.where = append(b.where, fmt.Sprintf("(p.created_at, p.id) < ($%d, $%d)",
			len(b.args)-1, len(b.args)))
	}
	b.args = append(b.args, limit+1)

	stmt := `
		SELECT p.id, p.slug, p.name, p.created_at
		FROM products p
		WHERE ` + b.whereClause() + `
		ORDER BY p.created_at DESC, p.id DESC
		LIMIT $` + fmt.Sprint(len(b.args))

	rows, err := s.pool.Query(ctx, stmt, b.args...)
	if err != nil {
		return nil, "", fmt.Errorf("staff: list products: %w", err)
	}
	defer rows.Close()

	out := make([]ProductRow, 0, limit)
	for rows.Next() {
		var row ProductRow
		if err := rows.Scan(&row.ID, &row.Slug, &row.Name, &row.CreatedAt); err != nil {
			return nil, "", fmt.Errorf("staff: scan product: %w", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("staff: read products: %w", err)
	}

	if len(out) > limit {
		out = out[:limit]
	}
	for i := range out {
		versions, err := s.priceVersions(ctx, out[i].ID)
		if err != nil {
			return nil, "", err
		}
		out[i].PriceVersions = versions
	}

	next := ""
	if len(out) == limit && len(out) > 0 {
		last := out[len(out)-1]
		next = CursorOf(last.CreatedAt, last.ID)
	}
	return out, next, nil
}

// Product returns one product.
func (s *Service) Product(ctx context.Context, id string) (ProductRow, error) {
	var row ProductRow
	err := s.pool.QueryRow(ctx, `
		SELECT p.id, p.slug, p.name, p.created_at FROM products p WHERE p.id = $1`, id).
		Scan(&row.ID, &row.Slug, &row.Name, &row.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return ProductRow{}, ErrNotFound
	}
	if err != nil {
		return ProductRow{}, fmt.Errorf("staff: load product: %w", err)
	}
	if row.PriceVersions, err = s.priceVersions(ctx, id); err != nil {
		return ProductRow{}, err
	}
	if err := s.pool.QueryRow(ctx, `
		SELECT count(*) FROM subscriptions sub
		JOIN price_versions pv ON pv.id = sub.price_version_id
		WHERE pv.product_id = $1 AND sub.status IN ('active', 'past_due')`, id).
		Scan(&row.ActiveSubscriptions); err != nil {
		return ProductRow{}, fmt.Errorf("staff: count active subscriptions: %w", err)
	}
	return row, nil
}

func (s *Service) priceVersions(ctx context.Context, productID string) ([]PriceVersionRow, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, version, currency, amount_minor, setup_minor, interval, anchor_day,
		       anchor_timezone, provision_plan, provision_region, created_at
		FROM price_versions WHERE product_id = $1 ORDER BY version DESC`, productID)
	if err != nil {
		return nil, fmt.Errorf("staff: list price versions: %w", err)
	}
	defer rows.Close()

	out := []PriceVersionRow{}
	for rows.Next() {
		var row PriceVersionRow
		if err := rows.Scan(&row.ID, &row.Version, &row.Currency, &row.AmountMinor,
			&row.SetupMinor, &row.Interval, &row.AnchorDay, &row.AnchorTimezone,
			&row.ProvisionPlan, &row.ProvisionRegion, &row.CreatedAt); err != nil {
			return nil, fmt.Errorf("staff: scan price version: %w", err)
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// holdsForCustomer lists the holds on every service the customer owns.
func (s *Service) holdsForCustomer(ctx context.Context, customerID string) ([]HoldRow, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT h.id, h.service_id, h.reason, h.expires_at, h.created_by, h.created_at
		FROM service_holds h
		JOIN services svc ON svc.id = h.service_id
		WHERE svc.customer_id = $1
		ORDER BY h.created_at DESC`, customerID)
	if err != nil {
		return nil, fmt.Errorf("staff: list customer holds: %w", err)
	}
	defer rows.Close()
	return scanHolds(rows)
}

// Holds lists the holds on one service.
func (s *Service) Holds(ctx context.Context, serviceID string) ([]HoldRow, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, service_id, reason, expires_at, created_by, created_at
		FROM service_holds WHERE service_id = $1 ORDER BY created_at DESC`, serviceID)
	if err != nil {
		return nil, fmt.Errorf("staff: list holds: %w", err)
	}
	defer rows.Close()
	return scanHolds(rows)
}

func scanHolds(rows pgx.Rows) ([]HoldRow, error) {
	out := []HoldRow{}
	for rows.Next() {
		var row HoldRow
		if err := rows.Scan(&row.ID, &row.ServiceID, &row.Reason, &row.ExpiresAt,
			&row.CreatedBy, &row.CreatedAt); err != nil {
			return nil, fmt.Errorf("staff: scan hold: %w", err)
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// Service returns one service with its subscription, operations, holds and invoices.
func (s *Service) Service(ctx context.Context, id string) (ServiceDetail, error) {
	var out ServiceDetail
	var (
		createdAt time.Time
		nextBill  *time.Time
	)
	err := s.pool.QueryRow(ctx, `
		SELECT s.id, s.customer_id, c.email, p.name, p.id, s.status,
		       COALESCE(s.external_id, ''), s.generation, s.created_at, sub.next_billing_at,
		       COALESCE(ops.open_count, 0), COALESCE(ops.last_error, '')
		FROM services s
		JOIN customers c ON c.id = s.customer_id
		JOIN products p ON p.id = s.product_id
		LEFT JOIN subscriptions sub ON sub.id = s.subscription_id
		LEFT JOIN LATERAL (
			SELECT count(*) FILTER (WHERE status IN
			           ('queued','running','retry_wait','unknown','needs_review')) AS open_count,
			       (SELECT last_error FROM service_operations
			         WHERE service_id = s.id AND last_error IS NOT NULL
			         ORDER BY created_at DESC LIMIT 1) AS last_error
			FROM service_operations WHERE service_id = s.id
		) ops ON true
		WHERE s.id = $1`, id).
		Scan(&out.Service.ID, &out.Service.CustomerID, &out.Service.CustomerMail,
			&out.Service.Product, &out.Service.ProductID, &out.Service.Status,
			&out.Service.ExternalID, &out.Service.Generation, &createdAt, &nextBill,
			&out.Service.OpenOps, &out.Service.LastError)
	if errors.Is(err, pgx.ErrNoRows) {
		return ServiceDetail{}, ErrNotFound
	}
	if err != nil {
		return ServiceDetail{}, fmt.Errorf("staff: load service: %w", err)
	}
	out.Service.CreatedAt = createdAt
	out.Service.NextBillAt = nextBill

	if out.Operations, err = s.operationsFor(ctx, id, 20); err != nil {
		return ServiceDetail{}, err
	}
	if out.Holds, err = s.Holds(ctx, id); err != nil {
		return ServiceDetail{}, err
	}
	if out.Subscription, err = s.subscriptionForService(ctx, id); err != nil {
		return ServiceDetail{}, err
	}
	if out.Invoices, err = s.invoicesForService(ctx, id); err != nil {
		return ServiceDetail{}, err
	}
	return out, nil
}

// ServiceDetail is the full service view.
type ServiceDetail struct {
	Service      ServiceRow
	Subscription *SubscriptionRow
	Operations   []OperationRow
	Holds        []HoldRow
	Invoices     []InvoiceRow
}

func (s *Service) operationsFor(ctx context.Context, serviceID string, limit int) ([]OperationRow, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT so.id, so.service_id, s.customer_id, c.email, so.kind, so.operation_key,
		       so.generation, so.status, so.attempts, COALESCE(so.last_error, ''),
		       so.created_at, so.started_at, so.finished_at
		FROM service_operations so
		JOIN services s ON s.id = so.service_id
		JOIN customers c ON c.id = s.customer_id
		WHERE so.service_id = $1
		ORDER BY so.created_at DESC LIMIT $2`, serviceID, limit)
	if err != nil {
		return nil, fmt.Errorf("staff: list service operations: %w", err)
	}
	defer rows.Close()

	out := []OperationRow{}
	for rows.Next() {
		var row OperationRow
		if err := rows.Scan(&row.ID, &row.ServiceID, &row.CustomerID, &row.CustomerMail,
			&row.Kind, &row.OperationKey, &row.Generation, &row.Status, &row.Attempts,
			&row.LastError, &row.CreatedAt, &row.StartedAt, &row.FinishedAt); err != nil {
			return nil, fmt.Errorf("staff: scan service operation: %w", err)
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

func (s *Service) subscriptionForService(ctx context.Context, serviceID string) (*SubscriptionRow, error) {
	var row SubscriptionRow
	err := s.pool.QueryRow(ctx, `
		SELECT sub.id, sub.status, sub.interval, sub.next_billing_at,
		       sub.current_period_start, sub.current_period_end, sub.cancel_at_period_end,
		       p.name
		FROM services s
		JOIN subscriptions sub ON sub.id = s.subscription_id
		JOIN price_versions pv ON pv.id = sub.price_version_id
		JOIN products p ON p.id = pv.product_id
		WHERE s.id = $1`, serviceID).
		Scan(&row.ID, &row.Status, &row.Interval, &row.NextBillingAt,
			&row.CurrentPeriodStart, &row.CurrentPeriodEnd, &row.CancelAtPeriodEnd, &row.Product)
	if errors.Is(err, pgx.ErrNoRows) {
		// A service with no subscription is a legitimate state (a one-off purchase), not an
		// error: the UI shows next_bill_at as null.
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("staff: load service subscription: %w", err)
	}
	return &row, nil
}

func (s *Service) invoicesForService(ctx context.Context, serviceID string) ([]InvoiceRow, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT i.id, COALESCE(i.number, ''), i.status, i.currency, i.total_minor,
		       i.amount_paid_minor, i.issued_at, i.due_at, i.customer_id, c.email,
		       (i.status = 'open' AND i.due_at IS NOT NULL AND i.due_at < now())
		FROM invoices i
		JOIN customers c ON c.id = i.customer_id
		WHERE i.subscription_id = (SELECT subscription_id FROM services WHERE id = $1)
		ORDER BY COALESCE(i.issued_at, i.created_at) DESC
		LIMIT 20`, serviceID)
	if err != nil {
		return nil, fmt.Errorf("staff: list service invoices: %w", err)
	}
	defer rows.Close()

	out := []InvoiceRow{}
	for rows.Next() {
		row, _, err := scanInvoice(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// balance derives the customer's balance per currency.
//
// It reuses the portal's rule rather than reimplementing it: outstanding comes from the
// open invoices, credit from the subledger's customer-credit account, and both are derived
// so neither can drift from the records they summarise.
func (s *Service) balance(ctx context.Context, customerID string) (Balance, error) {
	out := Balance{Outstanding: []Amount{}, Credit: []Amount{}}

	rows, err := s.pool.Query(ctx, `
		SELECT currency, SUM(total_minor - amount_paid_minor) AS outstanding
		FROM invoices
		WHERE customer_id = $1 AND status = 'open'
		GROUP BY currency ORDER BY currency`, customerID)
	if err != nil {
		return Balance{}, fmt.Errorf("staff: sum outstanding: %w", err)
	}
	for rows.Next() {
		var (
			code  string
			minor int64
		)
		if err := rows.Scan(&code, &minor); err != nil {
			rows.Close()
			return Balance{}, fmt.Errorf("staff: scan outstanding: %w", err)
		}
		if minor <= 0 {
			continue
		}
		amt, err := Amounts(minor, code)
		if err != nil {
			rows.Close()
			return Balance{}, err
		}
		out.Outstanding = append(out.Outstanding, amt)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return Balance{}, fmt.Errorf("staff: read outstanding: %w", err)
	}

	creditRows, err := s.pool.Query(ctx, `
		SELECT currency, SUM(credit_minor - debit_minor) AS credit
		FROM journal_entries
		WHERE customer_id = $1 AND account_code = 'customer_credit'
		GROUP BY currency ORDER BY currency`, customerID)
	if err != nil {
		return Balance{}, fmt.Errorf("staff: sum credit: %w", err)
	}
	defer creditRows.Close()
	for creditRows.Next() {
		var (
			code  string
			minor int64
		)
		if err := creditRows.Scan(&code, &minor); err != nil {
			return Balance{}, fmt.Errorf("staff: scan credit: %w", err)
		}
		if minor <= 0 {
			continue
		}
		amt, err := Amounts(minor, code)
		if err != nil {
			return Balance{}, err
		}
		out.Credit = append(out.Credit, amt)
	}
	return out, creditRows.Err()
}

// authEvents returns the recent authentication events for a customer's logins.
func (s *Service) authEvents(ctx context.Context, customerID string, limit int) ([]AuthEventRow, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT ae.event, ae.detail, COALESCE(host(ae.ip), ''), ae.created_at
		FROM auth_events ae
		WHERE ae.user_id IN (SELECT user_id FROM customer_accounts WHERE customer_id = $1)
		ORDER BY ae.created_at DESC LIMIT $2`, customerID, limit)
	if err != nil {
		return nil, fmt.Errorf("staff: list auth events: %w", err)
	}
	defer rows.Close()

	out := []AuthEventRow{}
	for rows.Next() {
		var row AuthEventRow
		if err := rows.Scan(&row.Event, &row.Detail, &row.IP, &row.CreatedAt); err != nil {
			return nil, fmt.Errorf("staff: scan auth event: %w", err)
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// Exists reports whether a customer exists, so a handler can answer 404 before doing work.
func (s *Service) CustomerExists(ctx context.Context, id string) (bool, error) {
	var exists bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM customers WHERE id = $1)`, id).
		Scan(&exists); err != nil {
		return false, fmt.Errorf("staff: check customer: %w", err)
	}
	return exists, nil
}

// ServiceExists reports whether a service exists.
func (s *Service) ServiceExists(ctx context.Context, id string) (bool, error) {
	var exists bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM services WHERE id = $1)`, id).
		Scan(&exists); err != nil {
		return false, fmt.Errorf("staff: check service: %w", err)
	}
	return exists, nil
}

// ErrNoPool is returned when the service has no database.
var ErrNoPool = errors.New("staff: no database configured")
