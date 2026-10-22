package payments

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/catalyst/billing/internal/billing"
	"github.com/jackc/pgx/v5"
)

// Offline payments: money that arrived without a gateway.
//
// The panel could already take money *away* from an invoice — refund it, credit it, void it —
// but there was no way to record money *arriving* outside a card gateway. A customer who pays
// by bank transfer therefore could never have their invoice marked paid: the operator's only
// options were to leave the invoice open forever or to fake a gateway payment, and the second
// one is worse than the first, because it writes a provider id that never existed and the
// reconciliation that follows is built on a lie.
//
// This file is the honest path. The rules it implements:
//
//  1. **A manual payment is still a payment.** It goes through the same applier, the same
//     allocation, the same ledger posting and the same invoice transition as a webhook
//     settlement. Nothing here reimplements ApplyPayment; a second implementation of the
//     subledger is a second set of bugs.
//  2. **The provider stays truthful.** The row is written with provider='manual' and a fixed
//     account, so `provider` continues to mean "who took the money" — a person did, not a
//     gateway — and `method` says how it arrived.
//  3. **The reference is the payment's identity.** Two staff members recording the same bank
//     transfer produce one payment, because the unique (provider, account, provider id) index
//     absorbs the second one. That is the same mechanism a redelivered webhook relies on, and
//     it is why a reference is mandatory: without one there is no identity, and two identical
//     cash payments would be indistinguishable from one recorded twice.
//  4. **An overpayment is refused, never converted to credit.** The reasoning is in
//     ErrOverpayment below.

var (
	// ErrInvalidOfflineMethod is returned when a method is outside the closed set.
	ErrInvalidOfflineMethod = errors.New("payments: unknown offline payment method")
	// ErrInvalidManualPayment is returned when the request is incomplete or nonsensical.
	ErrInvalidManualPayment = errors.New("payments: invalid manual payment")
	// ErrReferenceConflict is returned when the same reference was already recorded with a
	// different method. The reference identifies one payment; "BANK-1 is both a bank transfer
	// and cash" is a contradiction, not a correction, so it is refused rather than overwritten.
	ErrReferenceConflict = errors.New("payments: reference already recorded with a different method")
)

// ManualProvider is the provider value for a payment a person recorded.
//
// It is a constant rather than a configuration value on purpose: a deployment that could rename
// it would split its own payment history, and a bank reference recorded under the old name
// could then be recorded a second time under the new one.
const ManualProvider = "manual"

// ManualAccount scopes manual payment identity. It is not the gateway's merchant account: a
// later BILLING_GATEWAY_ACCOUNT change must not split manual payment identity, or the same
// transfer could be recorded twice.
const ManualAccount = "manual"

// OfflineMethod is how an offline payment arrived.
//
// The set is closed and mirrored by the CHECK constraint in migration 0057, so a value that
// reaches the database without passing through this type is still refused by the schema.
type OfflineMethod string

const (
	OfflineBankTransfer OfflineMethod = "bank_transfer"
	OfflineCash         OfflineMethod = "cash"
	OfflineCheque       OfflineMethod = "cheque"
	// OfflineOther is the escape hatch that keeps the set closed. Without it an operator whose
	// money arrived in a way the list does not name would be forced to pick a wrong value,
	// which is exactly the kind of lie this whole path exists to avoid.
	OfflineOther OfflineMethod = "other"
)

// Methods returns the closed set in the order a form should offer it.
func OfflineMethods() []OfflineMethod {
	return []OfflineMethod{OfflineBankTransfer, OfflineCash, OfflineCheque, OfflineOther}
}

// Valid reports whether m is one of the four methods.
func (m OfflineMethod) Valid() bool {
	switch m {
	case OfflineBankTransfer, OfflineCash, OfflineCheque, OfflineOther:
		return true
	}
	return false
}

// Label renders a method for a human. It is used in error messages and by the API so the
// frontend does not keep its own copy of the vocabulary.
func (m OfflineMethod) Label() string {
	switch m {
	case OfflineBankTransfer:
		return "Bank transfer"
	case OfflineCash:
		return "Cash"
	case OfflineCheque:
		return "Cheque"
	case OfflineOther:
		return "Other"
	}
	return string(m)
}

// ParseOfflineMethod normalises a client-supplied method and validates it against the closed set.
//
// Case and surrounding space are forgiven because they carry no meaning; anything else is a
// 400 rather than a silently recorded wrong channel.
func ParseOfflineMethod(raw string) (OfflineMethod, error) {
	m := OfflineMethod(strings.ToLower(strings.TrimSpace(raw)))
	if m.Valid() {
		return m, nil
	}
	return "", fmt.Errorf("%w: %q (want bank_transfer, cash, cheque or other)", ErrInvalidOfflineMethod, raw)
}

// ManualPayment is a payment a staff member recorded because it arrived outside a gateway.
type ManualPayment struct {
	// InvoiceID is the invoice the money is allocated to.
	InvoiceID string
	// AmountMinor is what actually arrived. It is never defaulted from the invoice: an
	// operator records the amount on the statement, and a partial payment is the normal case.
	AmountMinor int64
	// Currency is optional. When set it must match the invoice's currency; when empty the
	// invoice's currency is used. Either way the invoice decides, never the request.
	Currency string
	// Method is how the money arrived.
	Method OfflineMethod
	// Reference is the payment's identity: a bank reference, a cheque number, a receipt id.
	// It is required, and it is what makes a second recording of the same transfer a no-op.
	Reference string
	// Note is optional free text for the human reading the payment list later.
	Note string
}

// maxReferenceLen and maxNoteLen bound what a payment row can carry. A reference longer than
// this is a pasted document, not an identifier, and it would break the display of every list
// that shows it.
const (
	maxReferenceLen = 200
	maxNoteLen      = 500
)

// Validate checks the request before a transaction is opened, so a malformed body costs no
// connection and produces one clear error rather than a rollback.
func (m ManualPayment) Validate() error {
	if strings.TrimSpace(m.InvoiceID) == "" {
		return fmt.Errorf("%w: an invoice is required", ErrInvalidManualPayment)
	}
	if m.AmountMinor <= 0 {
		return fmt.Errorf("%w: the amount must be positive", ErrInvalidManualPayment)
	}
	if !m.Method.Valid() {
		return fmt.Errorf("%w: %q (want bank_transfer, cash, cheque or other)",
			ErrInvalidOfflineMethod, string(m.Method))
	}
	ref := strings.TrimSpace(m.Reference)
	if ref == "" {
		return fmt.Errorf("%w: a reference is required; it is the payment's identity",
			ErrInvalidManualPayment)
	}
	if len(ref) > maxReferenceLen {
		return fmt.Errorf("%w: the reference is longer than %d characters",
			ErrInvalidManualPayment, maxReferenceLen)
	}
	if len(m.Note) > maxNoteLen {
		return fmt.Errorf("%w: the note is longer than %d characters",
			ErrInvalidManualPayment, maxNoteLen)
	}
	return nil
}

// ManualResult reports what recording a manual payment did.
type ManualResult struct {
	PaymentID string
	InvoiceID string
	// AmountMinor is the amount that was allocated, which on a replay is the amount already
	// recorded rather than the amount in the request (they must be equal, or the replay is
	// refused by billing.ApplyPayment).
	AmountMinor int64
	Currency    string
	Method      OfflineMethod
	Reference   string
	Note        string
	// Duplicate is true when this reference had already been recorded against this invoice.
	// It is the expected outcome of a retry, not an error.
	Duplicate bool
	// InvoicePaid is true when the allocation settled the invoice.
	InvoicePaid bool
	// InvoiceTotalMinor, InvoicePaidMinor and RemainingMinor are read from the invoice after
	// the allocation, inside the same transaction, so the caller never computes a balance
	// from numbers it read before the money moved.
	InvoiceTotalMinor int64
	InvoicePaidMinor  int64
	RemainingMinor    int64
}

// RecordManual records an offline payment against its invoice inside tx.
//
// It is a function taking a transaction rather than a method that opens one, because the caller
// must be able to write its audit row in the *same* transaction: an audit row for a rolled-back
// payment is a lie, and a payment with no audit row is a gap. The allocation itself is
// billing.ApplyPayment, reached through the applier the webhook uses, so a manual payment and a
// card payment cannot diverge in how they move the ledger.
//
// Overpayment policy: **refused**, with billing.ErrOverpayment. The alternative — turning the
// excess into customer credit — was rejected for three reasons. It would create a liability
// (credit) as a side effect of a request that said nothing about credit; it would make the
// invoice screen silently more powerful than the credit screen, which is the auditable path for
// granting credit; and it would be indistinguishable, in the ledger, from an operator
// deliberately granting credit, so nobody reconciling later could tell which happened. An
// operator who really did receive more than the invoice owed records the invoice amount and
// grants the remainder as credit as a separate, visible action. billing.ApplyPayment already
// refuses the overpayment, so this path inherits the rule rather than restating it.
func RecordManual(ctx context.Context, tx pgx.Tx, applier Applier, mp ManualPayment) (ManualResult, error) {
	if err := mp.Validate(); err != nil {
		return ManualResult{}, err
	}
	if applier == nil {
		return ManualResult{}, errors.New("payments: no applier configured for manual payments")
	}

	var invoiceCurrency string
	err := tx.QueryRow(ctx, `
		SELECT currency FROM invoices WHERE id = $1`, mp.InvoiceID).Scan(&invoiceCurrency)
	if errors.Is(err, pgx.ErrNoRows) {
		return ManualResult{}, fmt.Errorf("%w: %s", billing.ErrInvoiceNotFound, mp.InvoiceID)
	}
	if err != nil {
		return ManualResult{}, fmt.Errorf("payments: load invoice for manual payment: %w", err)
	}

	cur, err := currencyOfCode(invoiceCurrency)
	if err != nil {
		return ManualResult{}, fmt.Errorf("payments: manual payment currency: %w", err)
	}
	if want := strings.TrimSpace(mp.Currency); want != "" && !strings.EqualFold(want, cur.Code()) {
		return ManualResult{}, fmt.Errorf("%w: payment %s, invoice %s",
			billing.ErrCurrencyMismatch, want, cur.Code())
	}

	ref := strings.TrimSpace(mp.Reference)
	res, err := applier.ApplySettledPayment(ctx, tx, SettledPayment{
		Provider:          ManualProvider,
		ProviderAccount:   ManualAccount,
		ProviderPaymentID: ref,
		InvoiceID:         mp.InvoiceID,
		AmountMinor:       mp.AmountMinor,
		Currency:          cur.Code(),
	})
	if err != nil {
		// billing.ErrOverpayment, ErrPaymentReused and the invoice-state errors travel
		// unchanged so the HTTP layer can answer each with its own code.
		return ManualResult{}, err
	}

	// The method and the note live on the payment row, and the row was written by
	// billing.ApplyPayment a moment ago. Filling them in here — inside the same transaction —
	// is what keeps "the payment exists" and "we know how it arrived" one fact. COALESCE
	// makes a replay a no-op that cannot rewrite what the first recording said.
	var existingMethod *string
	err = tx.QueryRow(ctx, `SELECT method FROM payments WHERE id = $1`, res.PaymentID).
		Scan(&existingMethod)
	if err != nil {
		return ManualResult{}, fmt.Errorf("payments: read manual payment method: %w", err)
	}
	if existingMethod != nil && *existingMethod != "" && OfflineMethod(*existingMethod) != mp.Method {
		return ManualResult{}, fmt.Errorf("%w: %s was recorded as %s, presented as %s",
			ErrReferenceConflict, ref, OfflineMethod(*existingMethod).Label(), mp.Method.Label())
	}

	if _, err := tx.Exec(ctx, `
		UPDATE payments
		SET method = COALESCE(NULLIF(method, ''), $2),
		    note   = COALESCE(NULLIF(note, ''), NULLIF($3, ''))
		WHERE id = $1`, res.PaymentID, string(mp.Method), mp.Note); err != nil {
		return ManualResult{}, fmt.Errorf("payments: record manual payment method: %w", err)
	}

	// Read the balance back from the invoice the allocation just updated. Reading it before
	// the allocation and adding the amount would be the same number only until two payments
	// race; reading it after is the number the database actually holds.
	var (
		totalMinor int64
		paidMinor  int64
	)
	if err := tx.QueryRow(ctx, `
		SELECT total_minor, amount_paid_minor FROM invoices WHERE id = $1`,
		mp.InvoiceID).Scan(&totalMinor, &paidMinor); err != nil {
		return ManualResult{}, fmt.Errorf("payments: read invoice balance: %w", err)
	}
	remaining := totalMinor - paidMinor
	if remaining < 0 {
		// Not reachable through this path — an overpayment is refused above — but a negative
		// balance is never something to report as "owed", so it is clamped rather than leaked.
		remaining = 0
	}

	var recordedNote string
	if err := tx.QueryRow(ctx, `SELECT COALESCE(note, '') FROM payments WHERE id = $1`,
		res.PaymentID).Scan(&recordedNote); err != nil {
		return ManualResult{}, fmt.Errorf("payments: read manual payment note: %w", err)
	}

	return ManualResult{
		PaymentID:         res.PaymentID,
		InvoiceID:         mp.InvoiceID,
		AmountMinor:       mp.AmountMinor,
		Currency:          cur.Code(),
		Method:            mp.Method,
		Reference:         ref,
		Note:              recordedNote,
		Duplicate:         res.Duplicate,
		InvoicePaid:       res.InvoicePaid,
		InvoiceTotalMinor: totalMinor,
		InvoicePaidMinor:  paidMinor,
		RemainingMinor:    remaining,
	}, nil
}
