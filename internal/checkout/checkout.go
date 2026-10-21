// Package checkout starts payment sessions and records what the customer was
// asked to pay.
//
// The recorded attempt is what makes amount verification possible later. Without
// it, a gateway could report a settled payment of any amount and the core would
// apply it: a 0.01 payment against a 12.00 invoice would become a legitimate
// partial payment instead of an anomaly. The plan requires verifying the amount
// and currency against what was expected, so the expectation has to be persisted
// before the customer is sent to pay.
package checkout

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/catalyst/billing/internal/extproto"
	"github.com/catalyst/billing/internal/money"
	"github.com/catalyst/billing/internal/payments"
	"github.com/catalyst/billing/internal/store"
	"github.com/jackc/pgx/v5"
)

var (
	// ErrInvoiceNotPayable is returned for an invoice that is not open.
	ErrInvoiceNotPayable = errors.New("checkout: invoice is not payable")
	// ErrInvoiceSettled is returned when there is nothing left to pay.
	ErrInvoiceSettled = errors.New("checkout: invoice is already settled")
	// ErrCurrencyMismatch is returned when the gateway cannot take the currency.
	ErrCurrencyMismatch = errors.New("checkout: unsupported currency")
)

// Gateway is the checkout side of a payment extension.
type Gateway interface {
	Name() string
	CreateCheckout(ctx context.Context, p extproto.CreateCheckoutParams) (extproto.CreateCheckoutResult, error)
}

// Session is a payable checkout.
type Session struct {
	AttemptID string
	InvoiceID string
	// URL is where to send the customer.
	URL string
	// Amount is what the customer will be charged.
	Amount money.Amount
	// Reused is true when an existing session was returned instead of creating a
	// new one at the gateway.
	Reused bool
}

// Service starts checkouts.
type Service struct {
	pool    *store.Pool
	gateway Gateway
	// Account scopes provider identifiers to one merchant account.
	Account string
	// SuccessURL and CancelURL are where the gateway returns the customer.
	SuccessURL string
	CancelURL  string
	// Now is injectable for tests.
	Now func() time.Time
}

// New constructs a Service.
func New(pool *store.Pool, gateway Gateway, account, successURL, cancelURL string) *Service {
	return &Service{
		pool:       pool,
		gateway:    gateway,
		Account:    account,
		SuccessURL: successURL,
		CancelURL:  cancelURL,
		Now:        func() time.Time { return time.Now().UTC() },
	}
}

// OperationKey is the stable identity of "pay this invoice". Deriving it from the
// invoice rather than from a random value is what makes a retried checkout return
// the same session instead of opening a second one at the gateway.
func OperationKey(invoiceID string) string { return "checkout:" + invoiceID }

// Start opens (or reuses) a payment session for an invoice.
func (s *Service) Start(ctx context.Context, invoiceID string) (Session, error) {
	if invoiceID == "" {
		return Session{}, fmt.Errorf("checkout: missing invoice id")
	}
	key := OperationKey(invoiceID)

	// Read the invoice and any existing attempt in one transaction so the amount
	// recorded on the attempt is the amount the customer is asked to pay, not a
	// value that changed in between.
	var (
		cur     money.Currency
		due     int64
		attempt *attemptRow
	)
	err := s.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var (
			invStatus string
			currency  string
			total     int64
			paid      int64
		)
		err := tx.QueryRow(ctx, `
			SELECT status, currency, total_minor, amount_paid_minor
			FROM invoices WHERE id = $1 FOR UPDATE`, invoiceID).
			Scan(&invStatus, &currency, &total, &paid)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("checkout: invoice %s not found", invoiceID)
		}
		if err != nil {
			return fmt.Errorf("checkout: load invoice: %w", err)
		}
		if invStatus != "open" {
			if invStatus == "paid" {
				return fmt.Errorf("%w: %s is %s", ErrInvoiceSettled, invoiceID, invStatus)
			}
			return fmt.Errorf("%w: %s is %s", ErrInvoiceNotPayable, invoiceID, invStatus)
		}
		cur, err = currencyOf(currency)
		if err != nil {
			return err
		}
		due = total - paid
		if due <= 0 {
			return fmt.Errorf("%w: nothing outstanding on %s", ErrInvoiceSettled, invoiceID)
		}

		row, err := loadAttempt(ctx, tx, key)
		if err != nil {
			return err
		}
		attempt = row
		return nil
	})
	if err != nil {
		return Session{}, err
	}

	amount := money.New(due, cur)

	// An attempt already exists: reuse it. Creating a second gateway session for
	// the same invoice is how customers end up charged twice.
	if attempt != nil {
		switch attempt.Status {
		case "succeeded":
			return Session{}, fmt.Errorf("%w: %s already has a successful payment", ErrInvoiceSettled, invoiceID)
		case "pending", "requires_action", "created":
			if attempt.AmountMinor != due || !attempt.Currency.Equal(cur) {
				// The invoice changed (a partial payment landed) after the session
				// was opened. The old session is for the wrong amount, so it is
				// cancelled rather than reused.
				if err := s.cancelAttempt(ctx, attempt.ID); err != nil {
					return Session{}, err
				}
			} else if attempt.RedirectURL != "" {
				return Session{
					AttemptID: attempt.ID,
					InvoiceID: invoiceID,
					URL:       attempt.RedirectURL,
					Amount:    amount,
					Reused:    true,
				}, nil
			}
		}
	}

	// Record the intent before calling the gateway: if the call succeeds and the
	// process dies, the attempt row is what lets a retry find the session.
	attemptID, err := s.recordAttempt(ctx, invoiceID, key, amount)
	if err != nil {
		return Session{}, err
	}

	res, err := s.gateway.CreateCheckout(ctx, extproto.CreateCheckoutParams{
		MutatingMeta: extproto.MutatingMeta{OperationKey: key},
		CustomerID:   customerIDOf(ctx, s.pool, invoiceID),
		Amount:       extproto.Amount{Minor: amount.Minor(), Currency: cur.Code()},
		Description:  "Invoice " + invoiceID,
		SuccessURL:   s.SuccessURL,
		CancelURL:    s.CancelURL,
		// The invoice travels in metadata so the payment can be attributed without
		// trusting the webhook body.
		Metadata: map[string]string{"invoice_id": invoiceID},
	})
	if err != nil {
		// The session was not created; leave the attempt cancelled so a retry is
		// not blocked by a dead row.
		_ = s.cancelAttempt(ctx, attemptID)
		return Session{}, fmt.Errorf("checkout: create gateway session: %w", err)
	}

	if _, err := s.pool.Exec(ctx, `
		UPDATE payment_attempts
		SET status = $2, provider_ref = $3, redirect_url = $4, updated_at = now()
		WHERE id = $1`,
		attemptID, string(res.Status), res.PaymentID, res.CheckoutURL); err != nil {
		return Session{}, fmt.Errorf("checkout: record gateway session: %w", err)
	}

	return Session{
		AttemptID: attemptID,
		InvoiceID: invoiceID,
		URL:       res.CheckoutURL,
		Amount:    amount,
	}, nil
}

type attemptRow struct {
	ID          string
	Status      string
	AmountMinor int64
	Currency    money.Currency
	ProviderRef string
	RedirectURL string
	InvoiceID   string
}

func loadAttempt(ctx context.Context, tx pgx.Tx, key string) (*attemptRow, error) {
	var (
		a        attemptRow
		currency string
		ref      *string
		url      *string
	)
	err := tx.QueryRow(ctx, `
		SELECT id, status, amount_minor, currency, provider_ref, redirect_url, invoice_id
		FROM payment_attempts WHERE operation_key = $1`, key).
		Scan(&a.ID, &a.Status, &a.AmountMinor, &currency, &ref, &url, &a.InvoiceID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("checkout: load attempt: %w", err)
	}
	if ref != nil {
		a.ProviderRef = *ref
	}
	if url != nil {
		a.RedirectURL = *url
	}
	cur, err := currencyOf(currency)
	if err != nil {
		return nil, err
	}
	a.Currency = cur
	return &a, nil
}

func (s *Service) recordAttempt(ctx context.Context, invoiceID, key string, amount money.Amount) (string, error) {
	var id string
	err := s.pool.QueryRow(ctx, `
		INSERT INTO payment_attempts (invoice_id, gateway, operation_key, status, amount_minor, currency)
		VALUES ($1, $2, $3, 'created', $4, $5)
		ON CONFLICT (operation_key) DO UPDATE
			SET status = 'created', amount_minor = EXCLUDED.amount_minor,
			    currency = EXCLUDED.currency, updated_at = now()
		RETURNING id`,
		invoiceID, s.gateway.Name(), key, amount.Minor(), amount.Currency().Code()).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("checkout: record attempt: %w", err)
	}
	return id, nil
}

func (s *Service) cancelAttempt(ctx context.Context, attemptID string) error {
	if _, err := s.pool.Exec(ctx, `
		UPDATE payment_attempts SET status = 'cancelled', updated_at = now() WHERE id = $1`,
		attemptID); err != nil {
		return fmt.Errorf("checkout: cancel attempt: %w", err)
	}
	return nil
}

// Querier is the read surface a lookup needs.
type Querier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

// LookupFor returns the attempt lookup bound to a querier, in the shape the
// payments processor expects. Binding it here keeps the payments package free of
// any knowledge of the checkout tables.
func LookupFor(q Querier) payments.AttemptLookup {
	return func(ctx context.Context, provider, providerRef string) (string, int64, string, bool, error) {
		return AttemptForPayment(ctx, q, provider, providerRef)
	}
}

// AttemptForPayment looks up the attempt that produced a provider payment. It is
// what amount verification checks against.
func AttemptForPayment(ctx context.Context, q Querier, provider, providerRef string) (invoiceID string, amountMinor int64, currency string, found bool, err error) {
	err = q.QueryRow(ctx, `
		SELECT invoice_id, amount_minor, currency
		FROM payment_attempts
		WHERE gateway = $1 AND provider_ref = $2`, provider, providerRef).
		Scan(&invoiceID, &amountMinor, &currency)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", 0, "", false, nil
	}
	if err != nil {
		return "", 0, "", false, fmt.Errorf("checkout: look up attempt: %w", err)
	}
	return invoiceID, amountMinor, currency, true, nil
}

// customerIDOf resolves the invoice's customer for the gateway payload. A lookup
// failure is not fatal here: the customer id is informational to the gateway, and
// the invoice id in metadata is what attribution uses.
func customerIDOf(ctx context.Context, pool *store.Pool, invoiceID string) string {
	var id string
	_ = pool.QueryRow(ctx, `SELECT customer_id FROM invoices WHERE id = $1`, invoiceID).Scan(&id)
	return id
}

func currencyOf(code string) (money.Currency, error) {
	if cur, ok := money.LookupCurrency(code); ok {
		return cur, nil
	}
	return money.NewCurrency(code, 2)
}
