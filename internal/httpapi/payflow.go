package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/catalyst/billing/internal/audit"
	"github.com/catalyst/billing/internal/authn"
	"github.com/catalyst/billing/internal/billing"
	"github.com/catalyst/billing/internal/credit"
	"github.com/catalyst/billing/internal/events"
	"github.com/catalyst/billing/internal/extproto"
	"github.com/catalyst/billing/internal/fulfilment"
	"github.com/catalyst/billing/internal/money"
	"github.com/catalyst/billing/internal/payments"
	"github.com/catalyst/billing/internal/portal"
	"github.com/catalyst/billing/internal/refunds"
	"github.com/jackc/pgx/v5"
)

// The customer pay flow (PAY-17, PAY-18, PAY-19, PAY-13, PAY-14).
//
// Three customer-facing endpoints and one operator endpoint:
//
//   - Pay with credit: spend the balance on one invoice and answer with the remainder.
//   - Payment status: report the *local* state of the invoice's newest checkout attempt so
//     the UI can wait for the webhook instead of guessing. It deliberately does not call
//     the provider: a browser polling us must not turn into one provider API call per
//     poll, and the local state is the one that decides whether the invoice is settled.
//   - Payment methods: the gateways this installation has configured, so the pay dialog
//     offers real options. There is exactly one gateway in this build (the extension named
//     by BILLING_GATEWAY_PROVIDER), and this endpoint reports that one rather than
//     inventing a registry the deployment cannot back.
//   - Manual payment (staff): an operator records money that arrived outside the gateway
//     (a bank transfer, a cheque). It allocates through billing.ApplyPayment, so the
//     ledger posting, the invoice transition and the idempotency are the same code every
//     other payment path uses.
//
// The rule that shapes all four: a browser redirect is not proof of payment and a poll is
// not proof of payment. Only a settled allocation moves an invoice to paid, and every
// response here derives `settled` from the allocation, not from the attempt's status.

func init() {
	RegisterRoutes("36-payflow", registerPayFlow)
}

func registerPayFlow(r *router) {
	r.mutation(http.MethodPost, "/api/v1/portal/invoices/{id}/pay-with-credit",
		authn.PermInvoicePaySelf, r.s.handlePortalPayWithCredit)
	r.get("/api/v1/portal/invoices/{id}/payment-status",
		authn.PermInvoiceReadSelf, r.s.handlePortalPaymentStatus)
	r.get("/api/v1/portal/payment-methods",
		authn.PermCustomerReadSelf, r.s.handlePortalPaymentMethods)

	// PAY-14. Recording money that arrived outside the gateway is a reconciliation action,
	// so it needs payment:reconcile: the same permission that reads the payment list, and
	// deliberately not invoice:write:all, because an operator who may edit a document is
	// not necessarily the person who may assert that money arrived.
	r.staffMutation(http.MethodPost, "/api/v1/staff/payments",
		authn.PermPaymentReconcile, r.s.handleStaffRecordPayment)
}

// handlePortalPayWithCredit applies the customer's credit to one of their invoices.
func (s *Server) handlePortalPayWithCredit(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "storage_unavailable", "no database configured")
		return
	}
	customerID, ok := s.customerFor(w, r)
	if !ok {
		return
	}

	invoiceID := r.PathValue("id")
	// A malformed id is a 404 that never reaches the database, so the response cannot
	// differ between "malformed" and "someone else's invoice".
	if !portal.ValidID(invoiceID) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such record")
		return
	}

	user, _ := userFrom(r.Context())

	// The key makes the call idempotent. The idempotency wrapper replays a stored response
	// for a repeated key; when the client sends none, the domain layer still derives one
	// from the invoice, so a double-click cannot spend the balance twice.
	key := strings.TrimSpace(r.Header.Get(idempotencyHeader))
	if key == "" {
		key = "portal:" + invoiceID
	}

	svc := s.creditService(r)
	app, err := svc.PayWithCredit(r.Context(), customerID, invoiceID, key, user.ID)
	switch {
	case err == nil:
	case errors.Is(err, credit.ErrNotFound):
		writeProblem(w, http.StatusNotFound, "not_found", "no such record")
		return
	case errors.Is(err, credit.ErrInsufficientCredit):
		writeProblem(w, http.StatusConflict, "insufficient_credit",
			"there is no credit available to apply")
		return
	case errors.Is(err, credit.ErrDisabled):
		writeProblem(w, http.StatusConflict, "credit_disabled",
			"customer credit is disabled for this installation")
		return
	case errors.Is(err, credit.ErrInvalidRequest):
		writeProblem(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	default:
		s.cfg.Logger.ErrorContext(r.Context(), "pay with credit failed",
			"invoice_id", invoiceID, "error", err)
		writeProblem(w, http.StatusInternalServerError, "internal_error",
			"the request could not be completed")
		return
	}

	if app.AmountMinor == 0 && !app.AlreadyApplied {
		// Nothing was spent because there was nothing to spend, or nothing to spend it on.
		// Answering 409 is the honest outcome: the invoice is unchanged and the customer
		// must choose another method.
		writeProblem(w, http.StatusConflict, "nothing_to_apply",
			"no credit is available, or this invoice owes nothing")
		return
	}

	cur := moneyFor(app.Currency)
	balances, err := svc.Balance(r.Context(), customerID)
	if err != nil {
		s.staffError(w, r, err)
		return
	}

	if app.InvoicePaid && !app.AlreadyApplied {
		s.publish(events.Event{
			Kind:       "invoice.paid",
			Permission: string(authn.PermInvoiceReadAll),
			Data:       map[string]any{"invoice_id": invoiceID, "via": "credit"},
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"application_id":  app.ID,
		"invoice_id":      app.InvoiceID,
		"amount":          amountJSON(money.New(app.AmountMinor, cur)),
		"total":           amountJSON(money.New(app.TotalMinor, cur)),
		"paid":            amountJSON(money.New(app.PaidMinor, cur)),
		"remaining":       amountJSON(money.New(app.RemainingMinor, cur)),
		"invoice_paid":    app.InvoicePaid,
		"already_applied": app.AlreadyApplied,
		"credit_balance":  creditAmounts(balances),
	})
}

// attemptView is the wire shape of one checkout attempt.
type attemptView struct {
	ID          string
	Gateway     string
	Status      string
	ProviderRef string
	AmountMinor int64
	Currency    string
	RedirectURL string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// handlePortalPaymentStatus reports the local state of the invoice's newest attempt.
//
// The two facts the UI needs are separated on purpose:
//
//   - `status` is what the gateway session is doing (pending, requires_action, succeeded,
//     failed, cancelled). It is a hint about the redirect, not about the money.
//   - `settled` is whether an allocation exists for the invoice. Only the webhook creates
//     one, so a client that polls until `settled` is waiting for the authoritative event
//     rather than for its own redirect to come back.
func (s *Server) handlePortalPaymentStatus(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "storage_unavailable", "no database configured")
		return
	}
	if s.portal == nil {
		writeProblem(w, http.StatusServiceUnavailable, "portal_unavailable", "the portal is not configured")
		return
	}
	user, _ := userFrom(r.Context())

	invoiceID := r.PathValue("id")
	if !portal.ValidID(invoiceID) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such record")
		return
	}

	// Ownership is checked with the portal's scoped query, so another customer's invoice is
	// indistinguishable from a nonexistent one.
	inv, err := s.portal.Invoice(r.Context(), user.ID, invoiceID)
	if err != nil {
		s.writePortalError(w, r, err)
		return
	}

	attempt, err := newestAttempt(r.Context(), s.pool, invoiceID)
	if err != nil {
		s.cfg.Logger.ErrorContext(r.Context(), "read payment attempt",
			"invoice_id", invoiceID, "error", err)
		writeProblem(w, http.StatusInternalServerError, "internal_error",
			"the request could not be completed")
		return
	}

	settled := inv.Status == "paid" || !inv.Due.IsPositive()
	if attempt != nil && !settled {
		// A settled allocation for this attempt is the authoritative signal that the
		// webhook landed, even if the invoice transition is somehow behind it.
		settled, err = allocationExists(r.Context(), s.pool, invoiceID, attempt.ProviderRef)
		if err != nil {
			s.cfg.Logger.ErrorContext(r.Context(), "check allocation",
				"invoice_id", invoiceID, "error", err)
			writeProblem(w, http.StatusInternalServerError, "internal_error",
				"the request could not be completed")
			return
		}
	}

	status := "none"
	if attempt != nil {
		status = attempt.Status
	}

	body := map[string]any{
		"invoice_id": invoiceID,
		"status":     status,
		"settled":    settled,
		"remaining":  amountJSON(inv.Due),
		"paid":       amountJSON(inv.Paid),
		"total":      amountJSON(inv.Total),
	}
	if attempt != nil {
		body["attempt"] = map[string]any{
			"id":           attempt.ID,
			"gateway":      attempt.Gateway,
			"status":       attempt.Status,
			"provider_ref": attempt.ProviderRef,
			"amount":       amountJSON(money.New(attempt.AmountMinor, moneyFor(attempt.Currency))),
			"redirect_url": attempt.RedirectURL,
			"created_at":   attempt.CreatedAt.UTC(),
			"updated_at":   attempt.UpdatedAt.UTC(),
		}
	}
	writeJSON(w, http.StatusOK, body)
}

// newestAttempt reads the most recent checkout attempt for an invoice.
func newestAttempt(ctx context.Context, pool interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}, invoiceID string) (*attemptView, error) {
	var (
		a        attemptView
		ref      *string
		redirect *string
	)
	err := pool.QueryRow(ctx, `
		SELECT id, gateway, status, provider_ref, amount_minor, currency, redirect_url,
		       created_at, updated_at
		FROM payment_attempts
		WHERE invoice_id = $1
		ORDER BY created_at DESC, id DESC
		LIMIT 1`, invoiceID).
		Scan(&a.ID, &a.Gateway, &a.Status, &ref, &a.AmountMinor, &a.Currency, &redirect,
			&a.CreatedAt, &a.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if ref != nil {
		a.ProviderRef = *ref
	}
	if redirect != nil {
		a.RedirectURL = *redirect
	}
	return &a, nil
}

// allocationExists reports whether a provider payment settled this invoice.
//
// It is the local record the webhook writes. An empty provider reference cannot have been
// allocated, so it short-circuits rather than matching every allocation with a NULL ref.
func allocationExists(ctx context.Context, pool interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}, invoiceID, providerRef string) (bool, error) {
	if providerRef == "" {
		return false, nil
	}
	var found bool
	err := pool.QueryRow(ctx, `
		SELECT true
		FROM payment_allocations pa
		JOIN payments p ON p.id = pa.payment_id
		WHERE pa.invoice_id = $1 AND p.provider_payment_id = $2
		LIMIT 1`, invoiceID, providerRef).Scan(&found)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return found, nil
}

// gatewayDescriber is the optional richer description a gateway extension can supply.
//
// It is optional because the checkout interface the server holds is deliberately narrow
// (name + create_checkout): a gateway that cannot describe itself is still a usable
// gateway, and the endpoint falls back to what it knows. When the adapter does implement
// it, the dialog gets the real currency and method lists without a second endpoint.
type gatewayDescriber interface {
	DescribeGateway(ctx context.Context) (extproto.GatewayDescribeResult, error)
}

// paymentMethodIcon names the glyph the pay dialog draws for one method. It is derived from
// the method's own id/name rather than from a brand table in the browser, so a gateway that
// advertises "sepa_debit" or "card" gets the right glyph without a frontend release.
func paymentMethodIcon(id, name string) string {
	value := strings.ToLower(strings.TrimSpace(id + " " + name))
	switch {
	case strings.Contains(value, "card"):
		return "card"
	case strings.Contains(value, "sepa"), strings.Contains(value, "bank"), strings.Contains(value, "debit"):
		return "bank"
	case strings.Contains(value, "wallet"), strings.Contains(value, "apple"), strings.Contains(value, "google"):
		return "wallet"
	case strings.Contains(value, "paypal"), strings.Contains(value, "mollie"):
		return "globe"
	default:
		return "card"
	}
}

// brandHint derives a display hint from the provider name.
//
// The panel knows the provider by its configured name, not by a brand table, and inventing
// a logo for an unknown provider would be a claim it cannot keep. The hint is therefore
// about *what kind of thing* the customer will see (a hosted card page), which is true of
// every gateway this protocol can host today.
func brandHint(provider string) string {
	switch strings.ToLower(provider) {
	case "":
		return ""
	case "stripe", "paypal", "mollie":
		return "card"
	default:
		return "hosted"
	}
}

// handlePortalPaymentMethods lists the gateways this installation has configured.
func (s *Server) handlePortalPaymentMethods(w http.ResponseWriter, r *http.Request) {
	if s.gateway == nil {
		// No gateway is a deployment state, not an error the customer caused. An empty list
		// is the truthful answer: the pay dialog then offers only "pay with credit".
		writeJSON(w, http.StatusOK, map[string]any{"payment_methods": []map[string]any{}})
		return
	}

	name := s.gateway.Name()
	methods := []map[string]any{{"id": "hosted", "name": "Hosted checkout"}}
	var currencies []string
	if desc, ok := s.gateway.(gatewayDescriber); ok {
		if res, err := desc.DescribeGateway(r.Context()); err == nil {
			if res.Name != "" {
				name = res.Name
			}
			if len(res.Methods) > 0 {
				methods = make([]map[string]any, 0, len(res.Methods))
				for _, m := range res.Methods {
					methods = append(methods, map[string]any{
						"id": m.ID, "name": m.Name, "icon": paymentMethodIcon(m.ID, m.Name),
					})
				}
			}
			currencies = res.Currencies
		} else {
			// A gateway that cannot describe itself must not break the dialog: the fallback
			// below is still a usable answer.
			s.cfg.Logger.WarnContext(r.Context(), "gateway describe failed",
				"provider", name, "error", err)
		}
	}
	if currencies == nil {
		currencies = []string{}
	}

	_, canRefund := s.gateway.(refunds.Gateway)

	writeJSON(w, http.StatusOK, map[string]any{
		"payment_methods": []map[string]any{{
			"id":               name,
			"provider":         name,
			"name":             displayName(name),
			"brand_hint":       brandHint(name),
			"icon":             brandHint(name),
			"methods":          methods,
			"currencies":       currencies,
			"supports_refunds": canRefund,
			"enabled":          true,
		}},
	})
}

// displayName renders a provider id for a human.
func displayName(provider string) string {
	if provider == "" {
		return "Payment gateway"
	}
	// Upper-case the first letter and leave the rest alone: "stripe" -> "Stripe",
	// "paypal_ipn" -> "Paypal_ipn". A brand table would be nicer and would also be a lie for
	// any provider it does not list.
	return strings.ToUpper(provider[:1]) + provider[1:]
}

// recordPaymentRequest is the body of a manual payment.
type recordPaymentRequest struct {
	InvoiceID string `json:"invoice_id"`
	// AmountMinor is what arrived. It is never taken from the invoice: a manual payment is
	// usually a partial one (a bank transfer for the amount the customer decided to send),
	// and defaulting it would record money nobody received.
	AmountMinor int64 `json:"amount_minor"`
	// Currency is optional; when present it must match the invoice.
	Currency string `json:"currency"`
	// Reference is the caller-supplied provider payment id (a bank reference, a cheque
	// number). It is the idempotency anchor: presenting the same reference twice finds the
	// existing payment instead of recording it again.
	Reference string `json:"reference"`
	// FeeMinor is the transaction fee the provider charged (PAY-13). It is recorded for
	// reconciliation and does not post: the ledger records the gross the customer paid, and
	// the fee is a cost of collecting it that a report nets off.
	FeeMinor int64  `json:"fee_minor"`
	Reason   string `json:"reason"`
}

// handleStaffRecordPayment records a payment that arrived outside the gateway.
func (s *Server) handleStaffRecordPayment(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "storage_unavailable", "no database configured")
		return
	}
	var req recordPaymentRequest
	if !s.decodeJSON(w, r, &req, 16<<10) {
		return
	}
	if !portal.ValidID(req.InvoiceID) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such invoice")
		return
	}
	if req.AmountMinor <= 0 {
		writeProblem(w, http.StatusBadRequest, "invalid_amount", "the amount must be positive")
		return
	}
	if strings.TrimSpace(req.Reference) == "" {
		writeProblem(w, http.StatusBadRequest, "reference_required",
			"a manual payment needs a reference; it is the payment's identity")
		return
	}
	if req.FeeMinor < 0 {
		writeProblem(w, http.StatusBadRequest, "invalid_fee", "the fee cannot be negative")
		return
	}
	if req.FeeMinor > req.AmountMinor {
		writeProblem(w, http.StatusBadRequest, "invalid_fee",
			"the fee cannot exceed the amount received")
		return
	}
	if strings.TrimSpace(req.Reason) == "" {
		writeProblem(w, http.StatusBadRequest, "reason_required",
			"recording a payment needs a reason")
		return
	}

	// A manual payment is scoped to a constant account, not the gateway's merchant account.
	//
	// The provider is already "manual", so the two never collide; scoping it separately is
	// what keeps the identity stable. Reusing the configured merchant account would mean a
	// later BILLING_GATEWAY_ACCOUNT change splits manual payment identity, and a bank
	// reference recorded under the old value could then be recorded a second time.
	const manualAccount = "manual"

	cur, err := resolveMoney(req.Currency)
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_currency", err.Error())
		return
	}

	// The applier chain is used rather than billing.ApplyPayment directly, because two things
	// must happen after the allocation and neither belongs in this handler:
	//
	//   - fulfilment enqueues provisioning for anything the paid invoice bought, so a manual
	//     payment for a service actually delivers the service;
	//   - credit routing turns a settled *deposit* invoice into a balance, so an operator who
	//     records a bank transfer against a top-up credits the customer instead of leaving
	//     the deposit invoice paid and the balance empty.
	//
	// Both are the same code the webhook uses. The allocation, its ledger posting and its
	// idempotency are billing.ApplyPayment's, inside both of them. The notifier is attached
	// for the same reason: a customer whose bank transfer was recorded deserves the same
	// "your invoice is paid" message as one whose card settled, and the outbox write belongs
	// in the settlement transaction either way.
	next := fulfilment.NewApplier(s.pool)
	if s.notify != nil {
		next = next.WithNotifier(s.notify)
	}
	applier := credit.NewDepositApplier(s.pool, next)
	user, _ := userFrom(r.Context())

	var (
		res        payments.ApplyResult
		currencyIn string
		totalMinor int64
		paidMinor  int64
	)
	err = s.pool.InTx(r.Context(), func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `
			SELECT currency, total_minor, amount_paid_minor FROM invoices WHERE id = $1`,
			req.InvoiceID).Scan(&currencyIn, &totalMinor, &paidMinor); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return errInvoiceNotFound
			}
			return err
		}
		invoiceCur := moneyFor(currencyIn)
		if !invoiceCur.IsValid() {
			return errCurrencyMismatch
		}
		amountCur := invoiceCur
		if cur.IsValid() {
			if !cur.Equal(invoiceCur) {
				return errCurrencyMismatch
			}
			amountCur = cur
		}

		var aerr error
		res, aerr = applier.ApplySettledPayment(ctx, tx, payments.SettledPayment{
			Provider:          "manual",
			ProviderAccount:   manualAccount,
			ProviderPaymentID: req.Reference,
			InvoiceID:         req.InvoiceID,
			AmountMinor:       req.AmountMinor,
			Currency:          amountCur.Code(),
		})
		if aerr != nil {
			return aerr
		}

		// PAY-13: the fee is a property of the payment row, recorded once. A replay finds
		// the same payment and rewrites the same value, which is a no-op.
		if req.FeeMinor > 0 {
			if _, err := tx.Exec(ctx,
				`UPDATE payments SET fee_minor = $2 WHERE id = $1`,
				res.PaymentID, req.FeeMinor); err != nil {
				return err
			}
		}

		return audit.Record(ctx, tx, s.auditEvent(r, "payment.record", "payment", res.PaymentID,
			audit.OutcomeSuccess).WithChanges(map[string]any{
			"invoice_id":      req.InvoiceID,
			"amount":          req.AmountMinor,
			"currency":        amountCur.Code(),
			"reference":       req.Reference,
			"fee":             req.FeeMinor,
			"reason":          req.Reason,
			"duplicate":       res.Duplicate,
			"recorded_by":     user.ID,
			"invoice_paid":    res.InvoicePaid,
			"invoice_total":   totalMinor,
			"invoice_paid_to": paidMinor,
		}))
	})
	if err != nil {
		switch {
		case errors.Is(err, errInvoiceNotFound):
			writeProblem(w, http.StatusNotFound, "not_found", "no such invoice")
		case errors.Is(err, errCurrencyMismatch):
			writeProblem(w, http.StatusBadRequest, "currency_mismatch",
				"the payment currency does not match the invoice")
		case errors.Is(err, billing.ErrOverpayment):
			writeProblem(w, http.StatusConflict, "overpayment", err.Error())
		case errors.Is(err, billing.ErrPaymentReused):
			writeProblem(w, http.StatusConflict, "reference_reused", err.Error())
		case errors.Is(err, billing.ErrInvoiceNotFound):
			writeProblem(w, http.StatusNotFound, "not_found", "no such invoice")
		case errors.Is(err, billing.ErrInvalidInvoice):
			writeProblem(w, http.StatusBadRequest, "invalid_payment", err.Error())
		default:
			s.staffError(w, r, err)
		}
		return
	}

	if res.InvoicePaid && !res.Duplicate {
		s.publish(events.Event{Kind: "invoice.paid", Permission: string(authn.PermInvoiceReadAll),
			Data: map[string]any{"invoice_id": req.InvoiceID, "via": "manual"}})
	}

	curOut := moneyFor(currencyIn)
	remaining := totalMinor - paidMinor - req.AmountMinor
	if remaining < 0 {
		remaining = 0
	}
	if res.Duplicate {
		// The amount is already reflected in the invoice's paid total; recomputing the
		// remainder from the columns is the truthful answer for a replay.
		remaining = totalMinor - paidMinor
	}

	writeJSON(w, http.StatusCreated, map[string]any{
		"payment_id":       res.PaymentID,
		"invoice_id":       req.InvoiceID,
		"provider":         "manual",
		"reference":        req.Reference,
		"amount":           amountJSON(money.New(req.AmountMinor, curOut)),
		"fee":              amountJSON(money.New(req.FeeMinor, curOut)),
		"remaining":        amountJSON(money.New(remaining, curOut)),
		"invoice_paid":     res.InvoicePaid,
		"already_recorded": res.Duplicate,
	})
}

var (
	errInvoiceNotFound  = errors.New("httpapi: invoice not found")
	errCurrencyMismatch = errors.New("httpapi: currency mismatch")
)

// resolveMoney validates an optional currency code from a request body.
func resolveMoney(code string) (money.Currency, error) {
	if strings.TrimSpace(code) == "" {
		return money.Currency{}, nil
	}
	cur, ok := money.LookupCurrency(code)
	if !ok {
		return money.Currency{}, errors.New("unknown currency")
	}
	return cur, nil
}
