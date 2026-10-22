package httpapi

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/catalyst/billing/internal/authn"
	"github.com/catalyst/billing/internal/credit"
	"github.com/catalyst/billing/internal/events"
	"github.com/catalyst/billing/internal/money"
	"github.com/catalyst/billing/internal/portal"
)

// Customer credit over HTTP (CRD-02, CRD-05, CRD-07, CRD-08, CRD-10, PAY-17).
//
// The handlers are thin on purpose. The rules that matter — the minimum and maximum
// deposit, the maximum balance including pending top-ups, the idempotency of an
// application, the posting that turns a settled payment into a balance — live in
// internal/credit, where a test can prove them without an HTTP round trip.
//
// What belongs here is the part that is genuinely about the request:
//
//   - Who may do it. A customer reads and spends only their own credit, resolved from the
//     session; an operator needs credit:write, which is deliberately separate from
//     payment:refund because a grant is not a refund of money a provider took.
//   - What a refusal means. A deposit below the minimum is a 400 (the request is wrong),
//     a balance-limit breach is a 409 (the request is well-formed but the account cannot
//     absorb it), and an unconfigured gateway is a 503 (the deployment, not the customer).
//   - What the customer is told. Every response states the amount and the balance after it,
//     so the UI never has to guess by re-reading a balance that may have moved.

func init() {
	RegisterRoutes("35-credits", registerCredits)
}

func registerCredits(r *router) {
	// The customer's own balance, per currency.
	r.get("/api/v1/portal/credits", authn.PermCustomerReadSelf, r.s.handlePortalCredits)

	// The customer's own credit movements, newest first. Read from the ledger, because that is
	// where the balance above comes from: a statement assembled from anywhere else could disagree
	// with the number it is meant to explain.
	r.get("/api/v1/portal/credits/transactions",
		authn.PermCustomerReadSelf, r.s.handlePortalCreditTransactions)

	// A top-up. It is a payment, so it uses the same permission as paying an invoice: a
	// customer who may pay may top up, and one who may not may not.
	r.mutation(http.MethodPost, "/api/v1/portal/credits/deposit",
		authn.PermInvoicePaySelf, r.s.handlePortalCreditDeposit)

	// An operator's grant. credit:write is the permission; the amount, currency and reason
	// are all required, and the reason reaches the audit row.
	r.staffMutation(http.MethodPost, "/api/v1/staff/customers/{id}/credit",
		authn.PermCreditWrite, r.s.handleStaffGrantCredit)
}

// creditService builds the credit service with the configured gateway and auditor.
func (s *Server) creditService(r *http.Request) *credit.Service {
	return s.creditServiceWithReason(r, "")
}

// creditServiceWithReason builds the credit service with an auditor that records a reason on
// every audit row it writes.
//
// It exists so a money movement's audit row explains itself even when the domain method's changes
// map does not carry the reason: the auditor stamps the request's actor, correlation id and source
// address, and a reason it is given is added rather than overwritten.
func (s *Server) creditServiceWithReason(r *http.Request, reason string) *credit.Service {
	svc := credit.New(s.pool).WithAuditor(s.auditorFor(r, reason))
	if s.gateway != nil {
		svc = svc.WithGateway(s.gateway, s.account, s.checkoutSuccessURL, s.checkoutCancelURL)
	}
	return svc
}

// creditAmounts renders a per-currency balance list.
func creditAmounts(amounts []money.Amount) []map[string]any {
	out := make([]map[string]any, 0, len(amounts))
	for _, a := range amounts {
		out = append(out, amountJSON(a))
	}
	return out
}

// handlePortalCredits serves the customer's credit balance and the settings that bound a
// top-up, so the deposit form can show the real minimum and maximum instead of a guess.
func (s *Server) handlePortalCredits(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "storage_unavailable", "no database configured")
		return
	}
	customerID, ok := s.customerFor(w, r)
	if !ok {
		return
	}

	svc := s.creditService(r)
	balances, err := svc.Balance(r.Context(), customerID)
	if err != nil {
		s.staffError(w, r, err)
		return
	}
	settings, err := svc.Settings(r.Context())
	if err != nil {
		s.staffError(w, r, err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"customer_id": customerID,
		"balances":    creditAmounts(balances),
		"settings": map[string]any{
			"enabled":               settings.Enabled,
			"minimum_deposit_minor": settings.MinimumDepositMinor,
			"maximum_deposit_minor": settings.MaximumDepositMinor,
			"maximum_credit_minor":  settings.MaximumCreditMinor,
			"auto_use":              settings.AutoUse,
		},
	})
}

// handlePortalCreditDeposit starts a top-up through the configured gateway.
func (s *Server) handlePortalCreditDeposit(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "storage_unavailable", "no database configured")
		return
	}
	customerID, ok := s.customerFor(w, r)
	if !ok {
		return
	}

	var req struct {
		AmountMinor int64  `json:"amount_minor"`
		Currency    string `json:"currency"`
	}
	if !s.decodeJSON(w, r, &req, 8<<10) {
		return
	}
	if req.AmountMinor <= 0 {
		writeProblem(w, http.StatusBadRequest, "invalid_amount", "the amount must be positive")
		return
	}
	if strings.TrimSpace(req.Currency) == "" {
		writeProblem(w, http.StatusBadRequest, "invalid_currency", "a currency is required")
		return
	}
	user, _ := userFrom(r.Context())

	deposit, err := s.creditService(r).StartDeposit(r.Context(), credit.DepositRequest{
		CustomerID:  customerID,
		AmountMinor: req.AmountMinor,
		Currency:    req.Currency,
		RequestedBy: user.ID,
	})
	switch {
	case err == nil:
	case errors.Is(err, credit.ErrDisabled):
		writeProblem(w, http.StatusConflict, "credit_disabled",
			"credit top-ups are disabled for this installation")
		return
	case errors.Is(err, credit.ErrBelowMinimum):
		writeProblem(w, http.StatusBadRequest, "deposit_below_minimum", err.Error())
		return
	case errors.Is(err, credit.ErrAboveMaximum):
		writeProblem(w, http.StatusBadRequest, "deposit_above_maximum", err.Error())
		return
	case errors.Is(err, credit.ErrMaxBalance):
		writeProblem(w, http.StatusConflict, "max_credit_balance", err.Error())
		return
	case errors.Is(err, credit.ErrNoGateway):
		writeProblem(w, http.StatusServiceUnavailable, "gateway_unavailable",
			"no payment gateway is configured")
		return
	case errors.Is(err, credit.ErrNotFound):
		writeProblem(w, http.StatusNotFound, "not_found", "no such record")
		return
	case errors.Is(err, credit.ErrInvalidRequest):
		writeProblem(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	default:
		s.cfg.Logger.ErrorContext(r.Context(), "credit deposit failed",
			"customer_id", customerID, "error", err)
		writeProblem(w, http.StatusServiceUnavailable, "gateway_unavailable",
			"the payment gateway is unavailable; try again")
		return
	}

	writeJSON(w, http.StatusCreated, map[string]any{
		"invoice_id":   deposit.InvoiceID,
		"number":       deposit.Number,
		"attempt_id":   deposit.AttemptID,
		"redirect_url": deposit.RedirectURL,
		"amount":       amountJSON(deposit.Amount),
		"status":       deposit.Status,
	})
}

// handleStaffGrantCredit issues goodwill credit to a customer.
func (s *Server) handleStaffGrantCredit(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "storage_unavailable", "no database configured")
		return
	}
	customerID := r.PathValue("id")
	if !portal.ValidID(customerID) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such record")
		return
	}

	var req struct {
		AmountMinor int64  `json:"amount_minor"`
		Currency    string `json:"currency"`
		Reason      string `json:"reason"`
	}
	if !s.decodeJSON(w, r, &req, 8<<10) {
		return
	}
	if req.AmountMinor <= 0 {
		writeProblem(w, http.StatusBadRequest, "invalid_amount", "the amount must be positive")
		return
	}
	if strings.TrimSpace(req.Reason) == "" {
		writeProblem(w, http.StatusBadRequest, "reason_required", "a grant needs a reason")
		return
	}
	if strings.TrimSpace(req.Currency) == "" {
		writeProblem(w, http.StatusBadRequest, "invalid_currency", "a currency is required")
		return
	}

	// The idempotency key is required by the route wrapper, so a replay of the same request
	// returns the stored response instead of minting a second grant. The key also becomes
	// the note's identity inside the domain layer, which is what makes the replay safe even
	// if the wrapper is bypassed.
	user, _ := userFrom(r.Context())
	key := strings.TrimSpace(r.Header.Get(idempotencyHeader))

	grant, err := credit.New(s.pool).WithAuditor(s.auditorFor(r, req.Reason)).
		Grant(r.Context(), credit.GrantRequest{
			CustomerID:  customerID,
			AmountMinor: req.AmountMinor,
			Currency:    req.Currency,
			Reason:      req.Reason,
			GrantedBy:   user.ID,
			Key:         key,
		})
	switch {
	case err == nil:
	case errors.Is(err, credit.ErrNotFound):
		writeProblem(w, http.StatusNotFound, "not_found", "no such customer")
		return
	case errors.Is(err, credit.ErrInvalidRequest):
		writeProblem(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	default:
		s.staffError(w, r, err)
		return
	}

	if !grant.AlreadyGranted {
		s.publish(events.Event{
			Kind:       "credit.granted",
			Permission: string(authn.PermCustomerReadAll),
			Data: map[string]any{
				"customer_id":    grant.CustomerID,
				"credit_note_id": grant.ID,
				"number":         grant.Number,
				"amount_minor":   grant.AmountMinor,
				"currency":       grant.Currency,
			},
		})
	}

	cur := moneyFor(grant.Currency)
	// The audit row is read back after the commit rather than assumed. The screen that shows a
	// grant shows its note, its audit row and the balance it left; the note and the balance come
	// from the domain result, and the audit row is the evidence that the change was recorded —
	// which is the part a client cannot infer. It is nil when the read fails, and the grant is
	// still reported as successful: the change is committed, and answering "failed" for a
	// successful grant is worse than a display field the client has to fetch from the list.
	writeJSON(w, http.StatusCreated, map[string]any{
		"id":          grant.ID,
		"number":      grant.Number,
		"customer_id": grant.CustomerID,
		"amount":      amountJSON(money.New(grant.AmountMinor, cur)),
		"balance":     amountJSON(money.New(grant.BalanceMinor, cur)),
		"reason":      grant.Reason,
		"status":      "issued",
		"replayed":    grant.AlreadyGranted,
		"audit":       s.latestCreditGrantAudit(r.Context(), grant.ID),
	})
}

// handlePortalCreditTransactions serves a customer's credit statement.
//
// It is scoped in SQL by the session's customer id, so substituting another customer's id is not
// possible: there is no id in the request to substitute.
//
// Each row carries `kind` (the journal transaction's kind) and `description` (the entry's memo)
// beside the amount, because a statement that shows only numbers cannot answer the question the
// customer actually has. A debit an operator made is memoed with their reason, and that memo is
// the only place the "why" can reach the person whose balance moved; without it, a deduction is
// indistinguishable from a bug. The fields are additive: a client that reads only `amount` and
// `memo` is unaffected.
func (s *Server) handlePortalCreditTransactions(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "storage_unavailable", "no database configured")
		return
	}
	customerID, ok := s.customerFor(w, r)
	if !ok {
		return
	}

	limit := 50
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil {
			limit = n
		}
	}

	entries, err := credit.TransactionsFor(r.Context(), s.pool, customerID, limit)
	if err != nil {
		s.staffError(w, r, err)
		return
	}

	out := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		out = append(out, map[string]any{
			"id":          e.ID,
			"kind":        e.Kind,
			"amount":      amountJSON(e.Amount),
			"memo":        e.Memo,
			"description": e.Memo,
			"created_at":  e.CreatedAt.UTC().Format(time.RFC3339),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"transactions": out})
}
