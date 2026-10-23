package notify

// The payment-failed notice, loaded from the invoice it is about.
//
// `invoice_payment_failed` was the one event in the catalogue with no call site: the template
// existed, the vocabulary was documented, the preferences screen offered it, and no declined card
// ever produced it. The customer learned about a failed renewal from dunning, a week later, in a
// suspension notice.
//
// The charge path (`payments.Agreements`) records the decline and the method's failure state in one
// transaction. It has the invoice id and the gateway's sentence, and nothing else this template
// needs: the number, the amount in the invoice's own currency, and the recipient. That join is
// written here, next to the template that consumes it, for the same reason ServiceActivated's is:
// a copy in the payments package would format money a second way.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/catalyst/billing/internal/money"
	"github.com/jackc/pgx/v5"
)

// InvoicePaymentFailedForInvoice tells a customer a charge for an invoice was declined.
//
// A missing invoice is not an error — the caller holds the record it just wrote, and a customer
// who no longer has the invoice does not need a notice about it.
func InvoicePaymentFailedForInvoice(ctx context.Context, tx pgx.Tx, queue *Queue,
	invoiceID, reason string, at time.Time) (Outcome, error) {

	res := Outcome{EventKey: EventInvoicePaymentFailed}
	if invoiceID == "" {
		return res, errors.New("notify: declined payment has no invoice")
	}

	var (
		number   string
		total    int64
		currency string
		email    string
		userID   *string
	)
	err := tx.QueryRow(ctx, `
		SELECT COALESCE(i.number, ''), i.total_minor, i.currency, c.email,
		       (SELECT ca.user_id::text FROM customer_accounts ca
		         WHERE ca.customer_id = c.id
		         ORDER BY (ca.role = 'owner') DESC, ca.created_at
		         LIMIT 1)
		  FROM invoices i
		  JOIN customers c ON c.id = i.customer_id
		 WHERE i.id = $1`, invoiceID).
		Scan(&number, &total, &currency, &email, &userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return res, nil
	}
	if err != nil {
		return res, fmt.Errorf("notify: read declined invoice: %w", err)
	}

	cur, err := notifyCurrency(currency)
	if err != nil {
		return res, fmt.Errorf("notify: declined invoice currency: %w", err)
	}
	in := InvoicePaymentFailedInput{
		Email:       email,
		InvoiceID:   invoiceID,
		Number:      number,
		Total:       money.New(total, cur).Format(),
		AttemptedAt: at,
		Reason:      reason,
		InvoiceURL:  "/portal/invoices/" + invoiceID,
	}
	if userID != nil {
		in.UserID = *userID
	}
	out, err := InvoicePaymentFailed(ctx, tx, queue, in)
	if err != nil {
		return out, fmt.Errorf("notify: declined payment: %w", err)
	}
	return out, nil
}

// notifyCurrency resolves a persisted currency code.
//
// Codes outside the ISO table are reconstructed at scale 2, which is the schema's default. It is a
// local copy of billing.currencyOf for the same reason the other packages have one: this package
// must not import the billing engine to format a sentence, and money.NewCurrency is the authority
// either way.
func notifyCurrency(code string) (money.Currency, error) {
	if cur, ok := money.LookupCurrency(code); ok {
		return cur, nil
	}
	return money.NewCurrency(code, 2)
}
