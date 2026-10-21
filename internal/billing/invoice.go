// Package billing implements the domain rules that move money: invoice
// issuance, payment application, and the transitions that must be atomic.
//
// The package works exclusively inside caller-supplied transactions so that a
// single commit covers the document, the subledger and the state change. A
// caller cannot accidentally record a payment without its allocation, or issue
// an invoice without its posting.
package billing

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/catalyst/billing/internal/ledger"
	"github.com/catalyst/billing/internal/money"
	"github.com/jackc/pgx/v5"
)

var (
	// ErrInvoiceNotDraft is returned when an operation requires a draft invoice.
	ErrInvoiceNotDraft = errors.New("billing: invoice is not a draft")
	// ErrInvoiceNotFound is returned when the invoice id does not exist.
	ErrInvoiceNotFound = errors.New("billing: invoice not found")
	// ErrAlreadyPaid is returned when an invoice is already settled.
	ErrAlreadyPaid = errors.New("billing: invoice is already paid")
	// ErrOverpayment is returned when an allocation exceeds the amount due.
	ErrOverpayment = errors.New("billing: allocation exceeds the amount due")
	// ErrCurrencyMismatch is returned when a payment and invoice differ.
	ErrCurrencyMismatch = errors.New("billing: currency mismatch")
	// ErrInvalidInvoice is returned for a malformed invoice request.
	ErrInvalidInvoice = errors.New("billing: invalid invoice")
)

// RoundingStage records where rounding was applied, so a recomputation
// reproduces the same totals. It is persisted in the invoice snapshot.
type RoundingStage string

const (
	// RoundPerLine rounds each line's tax before summing, which is the common
	// invoice convention and the one this product uses.
	RoundPerLine RoundingStage = "per_line"
	// RoundPerInvoice rounds once on the summed total.
	RoundPerInvoice RoundingStage = "per_invoice"
)

// Line is a requested invoice line before pricing is frozen.
type Line struct {
	Description string
	// Quantity is expressed in thousandths (milli-units) so a fractional
	// quantity such as 1.5 GB is exact without floating point.
	QuantityMilli int64
	UnitPrice     money.Amount
	TaxRate       string
}

// LineResult is the priced, taxed outcome for one line.
type LineResult struct {
	Description   string
	QuantityMilli int64
	UnitMinor     int64
	Net           money.Amount
	Tax           money.Amount
	Total         money.Amount
	TaxRate       string
}

// Totals is the money summary of an invoice.
type Totals struct {
	Subtotal money.Amount
	Tax      money.Amount
	Total    money.Amount
}

// Merchant is the legal entity issuing the document, snapshotted at issuance so
// a later settings change cannot alter a historic invoice.
type Merchant struct {
	Name        string `json:"name"`
	Address     string `json:"address"`
	TaxID       string `json:"tax_id,omitempty"`
	CountryCode string `json:"country_code"`
}

// CustomerDetails is the billed party, snapshotted for the same reason.
type CustomerDetails struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email"`
	// BillingAddress and TaxID are empty when the customer has not supplied
	// them; the tax engine decides whether that blocks issuance.
	BillingAddress string `json:"billing_address,omitempty"`
	TaxID          string `json:"tax_id,omitempty"`
	CountryCode    string `json:"country_code,omitempty"`
}

// Period describes the service period an invoice covers.
type Period struct {
	Start string `json:"start"`
	End   string `json:"end"`
	Key   string `json:"key"`
}

// Snapshot is the immutable evidence attached to an issued invoice. Everything
// needed to reprint the document and to justify its numbers is here, so catalog,
// tax and customer-profile edits can never rewrite history.
type Snapshot struct {
	SchemaVersion int             `json:"schema_version"`
	Merchant      Merchant        `json:"merchant"`
	Customer      CustomerDetails `json:"customer"`
	Lines         []LineResult    `json:"lines"`
	Totals        Totals          `json:"totals"`
	// Currency is repeated here so a reader of the snapshot needs nothing else.
	Currency      string        `json:"currency"`
	RoundingMode  string        `json:"rounding_mode"`
	RoundingStage RoundingStage `json:"rounding_stage"`
	Period        *Period       `json:"period,omitempty"`
	// Terms is the human-readable renewal/cancellation text the customer saw.
	Terms string `json:"terms,omitempty"`
	// TaxEvidence records why the tax amount is what it is.
	TaxEvidence TaxEvidence `json:"tax_evidence"`
	// PriceVersionIDs records which catalog versions were used.
	PriceVersionIDs []string `json:"price_version_ids,omitempty"`
}

// TaxEvidence documents the tax decision. Technical fields do not establish
// compliance; they make the decision auditable.
type TaxEvidence struct {
	// Mode is "exclusive" (tax added to the net price) or "inclusive" (tax
	// contained in the net price).
	Mode string `json:"mode"`
	// Policy is the identifier of the configured policy that was applied.
	Policy string `json:"policy"`
	// Rate is the exact rational rate as a string, e.g. "0.2" or "1/5".
	Rate string `json:"rate"`
	// Jurisdiction is the place of supply the policy resolved to.
	Jurisdiction string `json:"jurisdiction,omitempty"`
	// ExemptionReason is set when tax was not charged.
	ExemptionReason string `json:"exemption_reason,omitempty"`
}

// Pricer computes line money from requested lines. It is an interface so the tax
// policy can be swapped without touching invoice mechanics, and so a test can
// pin an exact policy.
type Pricer interface {
	// Price returns the priced lines and totals for the requested lines.
	Price(ctx context.Context, lines []Line, cur money.Currency) ([]LineResult, Totals, TaxEvidence, error)
}

// InvoiceNumber is an allocated document number.
type InvoiceNumber struct {
	Scope string
	Value int64
	// Prefix and padding are part of the series definition, not the value.
	Prefix string
	Width  int
}

// Format renders the number, e.g. "INV-000042".
func (n InvoiceNumber) Format() string {
	width := n.Width
	if width <= 0 {
		width = 6
	}
	return fmt.Sprintf("%s%0*d", n.Prefix, width, n.Value)
}

// NumberSeries allocates invoice numbers from a persisted sequence.
type NumberSeries struct {
	Scope  string
	Prefix string
	Width  int
}

// DefaultSeries is the single series a one-merchant installation uses.
var DefaultSeries = NumberSeries{Scope: "default", Prefix: "INV-", Width: 6}

// Allocate consumes the next number in the series.
//
// The allocation happens inside the issuing transaction and takes a row lock, so
// two concurrent issuers are serialised. A rolled-back issuance returns the
// number to the series: the invoice never existed, so the number was never used.
// That is the opposite of "never recycle": a number that was rolled back was
// never on a document a customer saw.
func (s NumberSeries) Allocate(ctx context.Context, tx pgx.Tx) (InvoiceNumber, error) {
	var value int64
	err := tx.QueryRow(ctx, `
		INSERT INTO invoice_number_sequences (scope, next_value)
		VALUES ($1, 2)
		ON CONFLICT (scope) DO UPDATE SET next_value = invoice_number_sequences.next_value + 1
		RETURNING next_value - 1`,
		s.Scope,
	).Scan(&value)
	if err != nil {
		return InvoiceNumber{}, fmt.Errorf("billing: allocate invoice number: %w", err)
	}
	return InvoiceNumber{Scope: s.Scope, Value: value, Prefix: s.Prefix, Width: s.Width}, nil
}

// IssueRequest is everything needed to turn a draft into an issued invoice.
type IssueRequest struct {
	CustomerID     string
	OrderID        string
	SubscriptionID string
	PeriodKey      string
	Lines          []Line
	Merchant       Merchant
	Customer       CustomerDetails
	Period         *Period
	Terms          string
	// DueAt is when payment becomes overdue.
	DueAt time.Time
	// Series selects the numbering series; zero value uses DefaultSeries.
	Series NumberSeries
	// PriceVersionIDs records catalog provenance.
	PriceVersionIDs []string
}

// Issued is the result of issuance.
type Issued struct {
	InvoiceID     string
	Number        string
	Totals        Totals
	Snapshot      Snapshot
	JournalTxID   string
	AlreadyIssued bool
}

// Issuer creates invoices.
type Issuer struct {
	pricer Pricer
}

// NewIssuer constructs an Issuer.
func NewIssuer(p Pricer) *Issuer { return &Issuer{pricer: p} }

// CreateDraft prices the request and stores an editable draft invoice.
//
// Drafts are mutable on purpose: a customer may add a line or correct an address
// before the document exists in the legal sense. The moment it is issued it
// becomes immutable.
func (s *Issuer) CreateDraft(ctx context.Context, tx pgx.Tx, req IssueRequest) (string, Totals, Snapshot, error) {
	if req.CustomerID == "" {
		return "", Totals{}, Snapshot{}, fmt.Errorf("%w: missing customer", ErrInvalidInvoice)
	}
	if len(req.Lines) == 0 {
		return "", Totals{}, Snapshot{}, fmt.Errorf("%w: no lines", ErrInvalidInvoice)
	}

	cur := req.Lines[0].UnitPrice.Currency()
	if !cur.IsValid() {
		return "", Totals{}, Snapshot{}, fmt.Errorf("%w: line has no currency", ErrInvalidInvoice)
	}
	for i, l := range req.Lines {
		if l.Description == "" {
			return "", Totals{}, Snapshot{}, fmt.Errorf("%w: line %d has no description", ErrInvalidInvoice, i)
		}
		if l.QuantityMilli <= 0 {
			return "", Totals{}, Snapshot{}, fmt.Errorf("%w: line %d has non-positive quantity", ErrInvalidInvoice, i)
		}
		if !l.UnitPrice.Currency().Equal(cur) {
			return "", Totals{}, Snapshot{}, fmt.Errorf("%w: line %d is %s, invoice is %s",
				ErrCurrencyMismatch, i, l.UnitPrice.Currency(), cur)
		}
	}

	results, totals, evidence, err := s.pricer.Price(ctx, req.Lines, cur)
	if err != nil {
		return "", Totals{}, Snapshot{}, err
	}

	snap := Snapshot{
		SchemaVersion:   1,
		Merchant:        req.Merchant,
		Customer:        req.Customer,
		Lines:           results,
		Totals:          totals,
		Currency:        cur.Code(),
		RoundingMode:    money.RoundHalfUp.String(),
		RoundingStage:   RoundPerLine,
		Period:          req.Period,
		Terms:           req.Terms,
		TaxEvidence:     evidence,
		PriceVersionIDs: req.PriceVersionIDs,
	}

	var invoiceID string
	err = tx.QueryRow(ctx, `
		INSERT INTO invoices (customer_id, order_id, subscription_id, period_key, status, currency,
			subtotal_minor, tax_minor, total_minor, due_at, snapshot)
		VALUES ($1, $2, $3, $4, 'draft', $5, $6, $7, $8, $9, $10)
		RETURNING id`,
		req.CustomerID, nullable(req.OrderID), nullable(req.SubscriptionID), nullable(req.PeriodKey),
		cur.Code(), totals.Subtotal.Minor(), totals.Tax.Minor(), totals.Total.Minor(),
		req.DueAt, snap,
	).Scan(&invoiceID)
	if err != nil {
		return "", Totals{}, Snapshot{}, fmt.Errorf("billing: insert draft invoice: %w", err)
	}

	for i, l := range results {
		if _, err := tx.Exec(ctx, `
			INSERT INTO invoice_lines (invoice_id, position, description, quantity,
				unit_minor, net_minor, tax_rate, tax_minor, total_minor)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
			invoiceID, i+1, l.Description,
			floatQuantity(l.QuantityMilli), l.UnitMinor,
			l.Net.Minor(), l.TaxRate, l.Tax.Minor(), l.Total.Minor(),
		); err != nil {
			return "", Totals{}, Snapshot{}, fmt.Errorf("billing: insert invoice line %d: %w", i, err)
		}
	}

	return invoiceID, totals, snap, nil
}

// Issue freezes a draft: it allocates the number, records the issuance time,
// marks the invoice open, and posts the receivable.
//
// This is the single point where a document becomes legally meaningful, so it is
// the single place the number is allocated and the single place the ledger is
// written.
func (s *Issuer) Issue(ctx context.Context, tx pgx.Tx, invoiceID string, series NumberSeries) (Issued, error) {
	var (
		status     string
		currency   string
		total      int64
		snap       Snapshot
		customerID string
	)
	err := tx.QueryRow(ctx, `
		SELECT status, currency, total_minor, snapshot, customer_id
		FROM invoices WHERE id = $1 FOR UPDATE`, invoiceID,
	).Scan(&status, &currency, &total, &snap, &customerID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Issued{}, fmt.Errorf("%w: %s", ErrInvoiceNotFound, invoiceID)
	}
	if err != nil {
		return Issued{}, fmt.Errorf("billing: load invoice: %w", err)
	}
	if status != "draft" {
		return Issued{}, fmt.Errorf("%w: %s is %s", ErrInvoiceNotDraft, invoiceID, status)
	}

	cur, err := currencyOf(currency)
	if err != nil {
		return Issued{}, err
	}
	if series.Scope == "" {
		series = DefaultSeries
	}
	number, err := series.Allocate(ctx, tx)
	if err != nil {
		return Issued{}, err
	}

	if _, err := tx.Exec(ctx, `
		UPDATE invoices SET status = 'open', number = $2, issued_at = now() WHERE id = $1`,
		invoiceID, number.Format(),
	); err != nil {
		return Issued{}, fmt.Errorf("billing: mark invoice issued: %w", err)
	}

	// Post the receivable: the customer now owes the total, and revenue is
	// recognised. Tax is split out so the liability is visible on its own.
	totalAmount := money.New(total, cur)
	taxMinor := snap.Totals.Tax.Minor()
	if !snap.Totals.Tax.Currency().Equal(cur) {
		return Issued{}, fmt.Errorf("%w: snapshot tax currency %s vs invoice %s",
			ErrCurrencyMismatch, snap.Totals.Tax.Currency(), cur)
	}
	netMinor := total - taxMinor

	posting := ledger.Posting{
		Kind:        ledger.KindInvoiceIssued,
		Reference:   ledger.RefInvoice,
		ReferenceID: invoiceID,
		CustomerID:  customerID,
		Currency:    cur,
		Debits: []ledger.Entry{
			{Account: ledger.AccountReceivable, Amount: totalAmount, Memo: "invoice " + number.Format()},
		},
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
	journalID, _, err := ledger.Post(ctx, tx, posting)
	if err != nil {
		return Issued{}, err
	}

	return Issued{
		InvoiceID:   invoiceID,
		Number:      number.Format(),
		Totals:      snap.Totals,
		Snapshot:    snap,
		JournalTxID: journalID,
	}, nil
}

// AllocationRequest records a confirmed payment against an invoice.
type AllocationRequest struct {
	// Provider identifies the gateway, e.g. "stripe".
	Provider string
	// ProviderAccount scopes provider identifiers to one merchant account.
	ProviderAccount string
	// ProviderPaymentID is the gateway's authoritative payment identity.
	ProviderPaymentID string
	InvoiceID         string
	// Amount is the money received. It must not exceed the outstanding balance.
	Amount money.Amount
}

// AllocationResult reports what happened. Duplicate is true when the payment was
// already recorded, which is the expected outcome of a replayed webhook.
type AllocationResult struct {
	PaymentID    string
	InvoiceID    string
	AllocationID string
	JournalTxID  string
	// Duplicate is true when this payment had already been applied.
	Duplicate bool
	// InvoicePaid is true when this allocation settled the invoice.
	InvoicePaid bool
}

// ApplyPayment records a confirmed payment and allocates it to one invoice.
//
// The whole operation is idempotent on the provider payment identity: a replayed
// event finds the existing payment and returns Duplicate without creating a
// second allocation or a second posting. That property comes from database
// uniqueness plus this lookup, not from trusting the caller.
func ApplyPayment(ctx context.Context, tx pgx.Tx, req AllocationRequest) (AllocationResult, error) {
	if req.Provider == "" || req.ProviderAccount == "" || req.ProviderPaymentID == "" {
		return AllocationResult{}, fmt.Errorf("%w: incomplete provider identity", ErrInvalidInvoice)
	}
	if !req.Amount.IsPositive() {
		return AllocationResult{}, fmt.Errorf("%w: payment amount must be positive", ErrInvalidInvoice)
	}

	// Lock the invoice first: the outstanding balance and the status transition
	// must be computed against a row nobody else can change.
	var (
		status     string
		currency   string
		totalMinor int64
		paidMinor  int64
		customerID string
	)
	err := tx.QueryRow(ctx, `
		SELECT status, currency, total_minor, amount_paid_minor, customer_id
		FROM invoices WHERE id = $1 FOR UPDATE`, req.InvoiceID,
	).Scan(&status, &currency, &totalMinor, &paidMinor, &customerID)
	if errors.Is(err, pgx.ErrNoRows) {
		return AllocationResult{}, fmt.Errorf("%w: %s", ErrInvoiceNotFound, req.InvoiceID)
	}
	if err != nil {
		return AllocationResult{}, fmt.Errorf("billing: load invoice: %w", err)
	}
	if status == "draft" {
		return AllocationResult{}, fmt.Errorf("%w: cannot pay a draft invoice", ErrInvoiceNotDraft)
	}
	if status == "void" {
		return AllocationResult{}, fmt.Errorf("%w: cannot pay a void invoice", ErrInvalidInvoice)
	}

	cur, err := currencyOf(currency)
	if err != nil {
		return AllocationResult{}, err
	}
	if !req.Amount.Currency().Equal(cur) {
		return AllocationResult{}, fmt.Errorf("%w: payment %s, invoice %s",
			ErrCurrencyMismatch, req.Amount.Currency(), cur)
	}

	// Insert the payment, or find the one that already exists.
	var paymentID string
	err = tx.QueryRow(ctx, `
		INSERT INTO payments (provider, provider_account, provider_payment_id, amount_minor, currency, status)
		VALUES ($1, $2, $3, $4, $5, 'succeeded')
		ON CONFLICT (provider, provider_account, provider_payment_id) DO NOTHING
		RETURNING id`,
		req.Provider, req.ProviderAccount, req.ProviderPaymentID,
		req.Amount.Minor(), cur.Code(),
	).Scan(&paymentID)
	duplicate := false
	if errors.Is(err, pgx.ErrNoRows) {
		duplicate = true
		if err := tx.QueryRow(ctx, `
			SELECT id FROM payments
			WHERE provider = $1 AND provider_account = $2 AND provider_payment_id = $3`,
			req.Provider, req.ProviderAccount, req.ProviderPaymentID,
		).Scan(&paymentID); err != nil {
			return AllocationResult{}, fmt.Errorf("billing: load existing payment: %w", err)
		}
		// Report the allocation that already exists, if any.
		var allocationID string
		err := tx.QueryRow(ctx, `
			SELECT id FROM payment_allocations WHERE payment_id = $1 AND invoice_id = $2`,
			paymentID, req.InvoiceID).Scan(&allocationID)
		if err == nil {
			return AllocationResult{
				PaymentID:    paymentID,
				InvoiceID:    req.InvoiceID,
				AllocationID: allocationID,
				Duplicate:    true,
				InvoicePaid:  status == "paid",
			}, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return AllocationResult{}, fmt.Errorf("billing: look up allocation: %w", err)
		}
		// The payment exists but was never allocated to this invoice (for
		// example it was applied elsewhere, or a previous attempt crashed
		// between the two writes). Fall through and allocate it.
	} else if err != nil {
		return AllocationResult{}, fmt.Errorf("billing: insert payment: %w", err)
	}

	outstanding := money.New(totalMinor-paidMinor, cur)
	cmp, err := req.Amount.Cmp(outstanding)
	if err != nil {
		return AllocationResult{}, err
	}
	if cmp > 0 {
		return AllocationResult{}, fmt.Errorf("%w: %s outstanding, %s offered",
			ErrOverpayment, outstanding, req.Amount)
	}

	var allocationID string
	err = tx.QueryRow(ctx, `
		INSERT INTO payment_allocations (payment_id, invoice_id, amount_minor, currency)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (payment_id, invoice_id) DO NOTHING
		RETURNING id`,
		paymentID, req.InvoiceID, req.Amount.Minor(), cur.Code(),
	).Scan(&allocationID)
	if errors.Is(err, pgx.ErrNoRows) {
		// A concurrent worker allocated it first; the outcome is the same.
		duplicate = true
		if err := tx.QueryRow(ctx, `
			SELECT id FROM payment_allocations WHERE payment_id = $1 AND invoice_id = $2`,
			paymentID, req.InvoiceID).Scan(&allocationID); err != nil {
			return AllocationResult{}, fmt.Errorf("billing: load allocation: %w", err)
		}
	} else if err != nil {
		return AllocationResult{}, fmt.Errorf("billing: insert allocation: %w", err)
	}

	newPaid := paidMinor + req.Amount.Minor()
	newStatus := status
	if newPaid == totalMinor {
		newStatus = "paid"
	}
	if _, err := tx.Exec(ctx, `
		UPDATE invoices SET amount_paid_minor = $2, status = $3 WHERE id = $1`,
		req.InvoiceID, newPaid, newStatus,
	); err != nil {
		return AllocationResult{}, fmt.Errorf("billing: update invoice paid amount: %w", err)
	}

	// Move money from clearing into the bank-equivalent account and clear the
	// receivable. Customer credit is untouched: an overpayment is a separate
	// decision, not a silent balance change.
	posting := ledger.Posting{
		Kind:        ledger.KindPayment,
		Reference:   ledger.RefPayment,
		ReferenceID: paymentID,
		CustomerID:  customerID,
		Currency:    cur,
		Debits: []ledger.Entry{
			{Account: ledger.AccountCashClearing, Amount: req.Amount, Memo: "payment " + req.ProviderPaymentID},
		},
		Credits: []ledger.Entry{
			{Account: ledger.AccountReceivable, Amount: req.Amount, Memo: "settle invoice " + req.InvoiceID},
		},
	}
	journalID, _, err := ledger.Post(ctx, tx, posting)
	if err != nil {
		return AllocationResult{}, err
	}

	return AllocationResult{
		PaymentID:    paymentID,
		InvoiceID:    req.InvoiceID,
		AllocationID: allocationID,
		JournalTxID:  journalID,
		Duplicate:    duplicate,
		InvoicePaid:  newStatus == "paid",
	}, nil
}

// currencyOf resolves a persisted currency code. Codes outside the ISO table are
// reconstructed at scale 2, which is what the schema's default assumes; a
// currency with a different scale must be in the ISO table or declared
// explicitly by the caller that created it.
func currencyOf(code string) (money.Currency, error) {
	if cur, ok := money.LookupCurrency(code); ok {
		return cur, nil
	}
	return money.NewCurrency(code, 2)
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// floatQuantity converts milli-units to the numeric quantity stored on the line.
// The conversion is exact for the values the catalog produces (multiples of
// 0.001) and is only ever used for display and audit, never for money.
func floatQuantity(milli int64) string {
	whole := milli / 1000
	frac := milli % 1000
	if frac < 0 {
		frac = -frac
	}
	return fmt.Sprintf("%d.%03d", whole, frac)
}
