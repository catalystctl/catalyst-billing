package credit

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/catalyst/billing/internal/ledger"
	"github.com/catalyst/billing/internal/money"
	"github.com/jackc/pgx/v5"
)

// Applying credit to an invoice (CRD-03, CRD-04, PAY-17).
//
// An application is one economic event: "this much of the customer's balance settled this
// much of this invoice". It is recorded as a `credit_applications` row and posted as an
// adjustment (debit customer_credit, credit receivable). Both are keyed by the *same* id,
// and that id is derived deterministically from the request's identity:
//
//	id = uuidv5("credit-application:" + key)
//
// so a replay of the same request finds the existing row and the existing posting instead
// of spending the balance a second time. The ledger's
// (kind, reference_type, reference_id) uniqueness is the second line of defence: even if
// the row lookup were skipped, the posting would collide rather than duplicate.
//
// The key has two forms:
//
//   - "auto:" + invoice id, for the automatic application at issue time. One invoice has
//     exactly one automatic application, so a replayed issuance (or a retried integrator
//     call) is a no-op rather than a second spend.
//   - a caller-supplied idempotency key, for a customer's explicit "pay with credit". The
//     HTTP layer replays a stored response for the same key, and this derivation makes the
//     domain layer safe even when the handler runs twice.
//
// The amount is never taken from the caller when it can be computed: it is
// min(available balance, what the invoice still owes). That is what makes a partial
// application leave exactly the right remainder, and it is why a customer cannot spend
// credit they do not have or overpay an invoice into a negative balance.

// Application is the outcome of applying credit to an invoice.
type Application struct {
	// ID is the credit application's id, empty when nothing was applied.
	ID string
	// InvoiceID and CustomerID identify what was paid.
	InvoiceID  string
	CustomerID string
	// AmountMinor is the credit spent. Zero when nothing was applied.
	AmountMinor int64
	Currency    string
	// RemainingMinor is what the invoice still owes afterwards, in the invoice currency.
	RemainingMinor int64
	// TotalMinor and PaidMinor are the invoice's totals after the application.
	TotalMinor int64
	PaidMinor  int64
	// InvoicePaid is true when this application settled the invoice.
	InvoicePaid bool
	// AlreadyApplied is true when this exact request had already been applied. The
	// amounts describe the existing application, not a new one.
	AlreadyApplied bool
	// Disabled is true when auto-use is off, so nothing was applied and nothing failed.
	Disabled  bool
	CreatedAt time.Time
}

// ApplyRequest describes an explicit application.
type ApplyRequest struct {
	InvoiceID string
	// Key is the request's idempotency identity. Empty derives it from the invoice, which
	// makes an unkeyed retry a no-op. Callers that legitimately apply credit to the same
	// invoice twice must supply a different key each time.
	Key string
	// AmountMinor caps the application. Zero means "as much as the balance and the invoice
	// allow", which is the normal case.
	AmountMinor int64
	// AppliedBy is the user id recorded on the application row, when there is one.
	AppliedBy string
}

// ApplyCreditToInvoice applies available credit to an invoice inside the caller's
// transaction.
//
// This is the function the issuer calls when it issues an invoice: calling it in the same
// transaction as the issuance is what makes "the invoice was issued and the credit was
// spent" one atomic fact instead of two that a crash can separate.
//
// It honours credits.auto_use: when the setting is off the call is a no-op that reports
// Disabled, never an error, because an issuance must not fail over a configuration choice.
// A replay is a no-op keyed by the invoice id.
func ApplyCreditToInvoice(ctx context.Context, tx pgx.Tx, invoiceID string) (Application, error) {
	settings, err := LoadSettings(ctx, tx)
	if err != nil {
		return Application{}, err
	}
	if !settings.AutoUse {
		return Application{InvoiceID: invoiceID, Disabled: true}, nil
	}
	return ApplyCredit(ctx, tx, ApplyRequest{
		InvoiceID: invoiceID,
		Key:       "auto:" + invoiceID,
	})
}

// ApplyCredit spends customer credit on one invoice inside the caller's transaction.
//
// It locks the invoice first (so two applications cannot each believe the whole amount is
// owed) and reads the balance from the subledger under the same transaction (so two
// applications cannot each believe the whole balance is available).
func ApplyCredit(ctx context.Context, tx pgx.Tx, req ApplyRequest) (Application, error) {
	if req.InvoiceID == "" {
		return Application{}, fmt.Errorf("%w: an invoice is required", ErrInvalidRequest)
	}
	if req.AmountMinor < 0 {
		return Application{}, fmt.Errorf("%w: the amount cannot be negative", ErrInvalidRequest)
	}

	key := req.Key
	if key == "" {
		key = "invoice:" + req.InvoiceID
	}
	applicationID := derivedUUID("credit-application:" + key)

	var (
		customerID, currency, status string
		totalMinor, paidMinor        int64
	)
	err := tx.QueryRow(ctx, `
		SELECT customer_id, currency, status, total_minor, amount_paid_minor
		FROM invoices WHERE id = $1 FOR UPDATE`, req.InvoiceID).
		Scan(&customerID, &currency, &status, &totalMinor, &paidMinor)
	if errors.Is(err, pgx.ErrNoRows) {
		return Application{}, ErrNotFound
	}
	if err != nil {
		return Application{}, fmt.Errorf("credit: lock invoice: %w", err)
	}
	cur, err := resolveCurrency(currency)
	if err != nil {
		return Application{}, err
	}

	out := Application{
		InvoiceID:      req.InvoiceID,
		CustomerID:     customerID,
		Currency:       currency,
		RemainingMinor: totalMinor - paidMinor,
		TotalMinor:     totalMinor,
		PaidMinor:      paidMinor,
		InvoicePaid:    status == "paid",
	}

	// A replay is detected before the amount is computed. Computing first and then finding
	// the posting would mean comparing against a balance that has moved, which is how a
	// "no-op" turns into a spurious reference-reuse error.
	existing, err := existingApplication(ctx, tx, applicationID)
	if err != nil {
		return Application{}, err
	}
	if existing != nil {
		out.ID = existing.ID
		out.AmountMinor = existing.AmountMinor
		out.CreatedAt = existing.CreatedAt
		out.AlreadyApplied = true
		out.PaidMinor = paidMinor
		out.RemainingMinor = totalMinor - paidMinor
		out.InvoicePaid = status == "paid"
		return out, nil
	}

	if status == "paid" {
		// The invoice is already settled, so nothing can be applied. If credit *was* what
		// settled it, report that application rather than "nothing to do": a retry with a
		// fresh idempotency key (a browser that reloaded, a client that lost its key) must
		// get the same answer as the original call, and it must not be told the payment
		// failed when the invoice is in fact paid.
		if prior, err := latestApplication(ctx, tx, req.InvoiceID, customerID); err != nil {
			return Application{}, err
		} else if prior != nil {
			out.ID = prior.ID
			out.AmountMinor = prior.AmountMinor
			out.CreatedAt = prior.CreatedAt
			out.AlreadyApplied = true
		}
		return out, nil
	}
	if status != "open" {
		// A draft was never issued and a void document is cancelled: neither can absorb
		// credit. Returning the state rather than an error keeps the auto-use call site free
		// of special cases.
		return out, nil
	}

	owed := totalMinor - paidMinor
	if owed <= 0 {
		return out, nil
	}

	available, err := creditBalance(ctx, tx, customerID, cur)
	if err != nil {
		return Application{}, err
	}
	if available <= 0 {
		return out, nil
	}

	amount := min64(owed, available)
	if req.AmountMinor > 0 {
		amount = min64(amount, req.AmountMinor)
	}
	if amount <= 0 {
		return out, nil
	}

	var createdAt time.Time
	if err := tx.QueryRow(ctx, `
		INSERT INTO credit_applications (id, customer_id, invoice_id, amount_minor, currency, applied_by)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (id) DO NOTHING
		RETURNING created_at`,
		applicationID, customerID, req.InvoiceID, amount, cur.Code(), nullableUUID(req.AppliedBy),
	).Scan(&createdAt); errors.Is(err, pgx.ErrNoRows) {
		// A concurrent caller inserted the same application first. Its posting is the one
		// that counts; report the row rather than writing a second one.
		existing, lerr := existingApplication(ctx, tx, applicationID)
		if lerr != nil {
			return Application{}, lerr
		}
		if existing == nil {
			return Application{}, fmt.Errorf("credit: application %s disappeared", applicationID)
		}
		out.ID = existing.ID
		out.AmountMinor = existing.AmountMinor
		out.CreatedAt = existing.CreatedAt
		out.AlreadyApplied = true
		return out, nil
	} else if err != nil {
		return Application{}, fmt.Errorf("credit: record application: %w", err)
	}

	// Debit customer credit, credit receivable. The application's own id is the reference,
	// which is what makes a replay a no-op instead of a second posting.
	if _, _, err := ledger.Post(ctx, tx, ledger.Posting{
		Kind:        ledger.KindAdjustment,
		Reference:   ledger.RefCreditApplication,
		ReferenceID: applicationID,
		CustomerID:  customerID,
		Currency:    cur,
		Debits: []ledger.Entry{{
			Account: ledger.AccountCredit,
			Amount:  money.New(amount, cur),
			Memo:    "credit applied to invoice " + req.InvoiceID,
		}},
		Credits: []ledger.Entry{{
			Account: ledger.AccountReceivable,
			Amount:  money.New(amount, cur),
			Memo:    "settled with customer credit",
		}},
	}); err != nil {
		return Application{}, fmt.Errorf("credit: post application: %w", err)
	}

	// The invoice's paid amount moves and the invoice may become paid. The 0008 trigger
	// validates the transition, so an illegal move is refused by the database rather than
	// trusted here.
	newPaid := paidMinor + amount
	newStatus := "open"
	if newPaid >= totalMinor {
		newStatus = "paid"
		newPaid = totalMinor
	}
	if _, err := tx.Exec(ctx, `
		UPDATE invoices SET amount_paid_minor = $2, status = $3 WHERE id = $1`,
		req.InvoiceID, newPaid, newStatus); err != nil {
		return Application{}, fmt.Errorf("credit: update invoice: %w", err)
	}

	// An order is "paid" when the invoice that came from it is settled. The same transition
	// billing.ApplyPayment makes, for the same reason: the status must be a fact derived
	// from the money, not a column somebody remembers to update.
	if newStatus == "paid" {
		if _, err := tx.Exec(ctx, `
			UPDATE orders SET status = 'paid', updated_at = now(), version = version + 1
			WHERE id = (SELECT order_id FROM invoices WHERE id = $1)
			  AND status = 'pending_payment'`, req.InvoiceID); err != nil {
			return Application{}, fmt.Errorf("credit: mark order paid: %w", err)
		}
	}

	out.ID = applicationID
	out.AmountMinor = amount
	out.CreatedAt = createdAt
	out.PaidMinor = newPaid
	out.RemainingMinor = totalMinor - newPaid
	out.InvoicePaid = newStatus == "paid"
	return out, nil
}

// existingApplication reads an application row by its derived id.
func existingApplication(ctx context.Context, tx pgx.Tx, id string) (*Application, error) {
	var (
		out       Application
		createdAt time.Time
	)
	err := tx.QueryRow(ctx, `
		SELECT id, customer_id, invoice_id, amount_minor, currency, created_at
		FROM credit_applications WHERE id = $1`, id).
		Scan(&out.ID, &out.CustomerID, &out.InvoiceID, &out.AmountMinor, &out.Currency, &createdAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("credit: load application: %w", err)
	}
	out.CreatedAt = createdAt
	return &out, nil
}

// latestApplication reads the most recent credit application for an invoice, if any.
//
// It is used only when an invoice is already settled: the question there is not "how much
// can be applied" but "was it credit that paid this, and how much".
func latestApplication(ctx context.Context, tx pgx.Tx, invoiceID, customerID string) (*Application, error) {
	var (
		out       Application
		createdAt time.Time
	)
	err := tx.QueryRow(ctx, `
		SELECT id, customer_id, invoice_id, amount_minor, currency, created_at
		FROM credit_applications
		WHERE invoice_id = $1 AND customer_id = $2
		ORDER BY created_at DESC, id DESC
		LIMIT 1`, invoiceID, customerID).
		Scan(&out.ID, &out.CustomerID, &out.InvoiceID, &out.AmountMinor, &out.Currency, &createdAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("credit: load prior application: %w", err)
	}
	out.CreatedAt = createdAt
	return &out, nil
}

// derivedUUID turns an arbitrary identity string into a deterministic UUID.
//
// The ledger's reference_id is a uuid column, so a request key that is not a uuid has to be
// mapped onto one. SHA-256 truncated to 16 bytes with the version-5 and RFC 4122 variant
// bits set is the same construction a name-based UUID uses: the same key always produces
// the same id, and two different keys collide only if SHA-256 does.
func derivedUUID(key string) string {
	sum := sha256.Sum256([]byte(key))
	var b [16]byte
	copy(b[:], sum[:16])
	b[6] = (b[6] & 0x0f) | 0x50 // version 5 (name-based)
	b[8] = (b[8] & 0x3f) | 0x80 // RFC 4122 variant
	hexed := hex.EncodeToString(b[:])
	return hexed[0:8] + "-" + hexed[8:12] + "-" + hexed[12:16] + "-" +
		hexed[16:20] + "-" + hexed[20:32]
}

// PayWithCredit applies the customer's available credit to their own invoice.
//
// It is the customer-facing entry point, so it checks ownership itself: an invoice that
// belongs to someone else is reported exactly like one that does not exist, which is what
// stops the endpoint being used to discover identifiers.
func (s *Service) PayWithCredit(ctx context.Context, customerID, invoiceID, key, appliedBy string) (Application, error) {
	if s.pool == nil {
		return Application{}, ErrInvalidRequest
	}
	if customerID == "" || invoiceID == "" {
		return Application{}, fmt.Errorf("%w: a customer and an invoice are required", ErrInvalidRequest)
	}

	var out Application
	err := s.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var owner string
		if err := tx.QueryRow(ctx, `SELECT customer_id FROM invoices WHERE id = $1`,
			invoiceID).Scan(&owner); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return fmt.Errorf("credit: load invoice owner: %w", err)
		}
		if owner != customerID {
			return ErrNotFound
		}

		app, err := ApplyCredit(ctx, tx, ApplyRequest{
			InvoiceID: invoiceID,
			Key:       key,
			AppliedBy: appliedBy,
		})
		if err != nil {
			return err
		}
		out = app
		if app.AmountMinor == 0 && !app.AlreadyApplied {
			return nil
		}
		status := "open"
		if app.InvoicePaid {
			status = "paid"
		}
		return s.recordAudit(ctx, tx, "credit.apply", "invoice", invoiceID, appliedBy,
			map[string]any{
				"customer_id":     customerID,
				"application_id":  app.ID,
				"amount":          app.AmountMinor,
				"currency":        app.Currency,
				"remaining":       app.RemainingMinor,
				"invoice_status":  status,
				"already_applied": app.AlreadyApplied,
			})
	})
	if err != nil {
		return Application{}, err
	}
	return out, nil
}
