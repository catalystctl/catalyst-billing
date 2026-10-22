// Affiliate programme HTTP surface (GRW-02, GRW-03).
//
// Three audiences, three access levels, and the split is the security model:
//
//   - /api/v1/staff/affiliates** is staff-only. Reads require report:read (an affiliate
//     balance is a report), and the two actions that can move money — registering a payee
//     with a commission rate, and paying a balance out — require credit:write. No new
//     permission is introduced: credit:write already means "may grant a customer value
//     without an invoice behind it", which is exactly what a commission payout is.
//
//   - /api/v1/portal/affiliate is the customer's own view. The affiliate is resolved from
//     the *session*, never from the URL, so identifier substitution cannot read somebody
//     else's earnings. A customer who is not an affiliate gets 404, not 403: "you are not
//     enrolled" is not information about another record.
//
//   - Attribution itself is not a route here. Registration is owned by the customer
//     purchase package, so this file exports growth.AttributeSignup for that transaction
//     to call, and the storefront captures the ?ref= code (see captureReferral below and
//     the integrator note in the package report).
package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/catalyst/billing/internal/audit"
	"github.com/catalyst/billing/internal/authn"
	"github.com/catalyst/billing/internal/growth"
	"github.com/catalyst/billing/internal/money"
	"github.com/jackc/pgx/v5"
)

func init() {
	RegisterRoutes("75-growth-affiliates", func(r *router) { r.s.registerAffiliateRoutes(r) })
}

func (s *Server) registerAffiliateRoutes(r *router) {
	// Reads are a report: an affiliate list is "who do we owe", which is what report:read
	// already covers.
	r.staffGet("/api/v1/staff/affiliates", authn.PermReportRead, s.handleListAffiliates)
	r.staffGet("/api/v1/staff/affiliates/payouts", authn.PermReportRead, s.handleAffiliatePayouts)

	// Registering an affiliate fixes the rate at which money will later leave the
	// business, so it carries the same permission as the payout itself.
	r.staffMutation(http.MethodPost, "/api/v1/staff/affiliates",
		authn.PermCreditWrite, s.handleCreateAffiliate)

	// The payout is a money action: idempotent, audited, and gated on credit:write.
	r.staffMutation(http.MethodPost, "/api/v1/staff/affiliates/{id}/payout",
		authn.PermCreditWrite, s.handleAffiliatePayout)

	// The customer's own view. customer:read:self is the permission a signed-in customer
	// already holds for their own record, and the query is scoped in SQL.
	r.get("/api/v1/portal/affiliate", authn.PermCustomerReadSelf, s.handlePortalAffiliate)
}

// growthStore returns the affiliate store for this server.
//
// It is constructed per call rather than cached: growth.Store is a single pointer to the
// pool and holds no state, so a cache would be a mutex guarding nothing. That also keeps
// this feature out of Server's field list, which is the integrator's file.
func (s *Server) growthStore() *growth.Store { return growth.New(s.pool) }

// ---------------------------------------------------------------- rendering --

// affiliateJSON is the one description of an affiliate for the API.
func affiliateJSON(a growth.Affiliate) map[string]any {
	balances := make([]map[string]any, 0, len(a.Balances))
	for _, b := range a.Balances {
		balances = append(balances, map[string]any{
			"currency": b.Currency,
			"owed":     amountJSON(b.Owed),
			"paid":     amountJSON(b.Paid),
		})
	}
	return map[string]any{
		"id":              a.ID,
		"user_id":         a.UserID,
		"email":           a.Email,
		"code":            a.Code,
		"commission_rate": a.CommissionRate,
		"payout_method":   a.PayoutMethod,
		"enabled":         a.Enabled,
		"referred_count":  a.ReferredCount,
		"balances":        balances,
		"created_at":      a.CreatedAt,
	}
}

// referralJSON renders one attribution or commission row.
func referralJSON(r growth.Referral) map[string]any {
	out := map[string]any{
		"id":           r.ID,
		"affiliate_id": r.AffiliateID,
		"customer_id":  r.CustomerID,
		"status":       r.Status,
		"attributed_at": r.AttributedAt,
	}
	if r.OrderID != "" {
		out["order_id"] = r.OrderID
		out["commission"] = amountJSON(r.Commission)
		out["rate"] = r.Rate
	}
	if r.PaidAt != "" {
		out["paid_at"] = r.PaidAt
	}
	return out
}

// growthError maps a domain error to a stable problem code. A client branches on the
// code, so the mapping lives in one place rather than in each handler.
func (s *Server) growthError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, growth.ErrNotFound), errors.Is(err, growth.ErrUnknownCode):
		writeProblem(w, http.StatusNotFound, "not_found", "no such record")
	case errors.Is(err, growth.ErrCodeTaken):
		writeProblem(w, http.StatusConflict, "code_taken", "that referral code is already in use")
	case errors.Is(err, growth.ErrAlreadyAttributed), errors.Is(err, growth.ErrSelfReferral),
		errors.Is(err, growth.ErrExistingAccount):
		writeProblem(w, http.StatusConflict, "attribution_refused", err.Error())
	case errors.Is(err, growth.ErrAffiliateDisabled):
		writeProblem(w, http.StatusConflict, "affiliate_disabled", "this affiliate is not enabled")
	case errors.Is(err, growth.ErrNothingToPay):
		writeProblem(w, http.StatusConflict, "nothing_to_pay", "there is no outstanding balance to pay")
	case errors.Is(err, growth.ErrInvalidRate), errors.Is(err, growth.ErrInvalidInput):
		writeProblem(w, http.StatusUnprocessableEntity, "invalid_input", err.Error())
	default:
		s.cfg.Logger.ErrorContext(r.Context(), "affiliate request failed",
			"path", r.URL.Path, "error", err)
		writeProblem(w, http.StatusInternalServerError, "internal_error",
			"the request could not be completed")
	}
}

// ---------------------------------------------------------------- staff read --

func (s *Server) handleListAffiliates(w http.ResponseWriter, r *http.Request) {
	items, err := s.growthStore().List(r.Context())
	if err != nil {
		s.growthError(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(items))
	for _, a := range items {
		out = append(out, affiliateJSON(a))
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out})
}

// handleAffiliatePayouts serves the payout report: every affiliate with an outstanding
// balance, per currency, plus the totals an operator reconciles against.
func (s *Server) handleAffiliatePayouts(w http.ResponseWriter, r *http.Request) {
	lines, err := s.growthStore().PayoutReport(r.Context())
	if err != nil {
		s.growthError(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(lines))
	totals := make([]map[string]any, 0, 2)
	byCurrency := map[string]int64{}
	order := make([]string, 0, 2)
	for _, line := range lines {
		out = append(out, map[string]any{
			"affiliate_id":        line.AffiliateID,
			"code":                line.Code,
			"user_id":             line.UserID,
			"email":               line.Email,
			"currency":            line.Currency,
			"owed":                amountJSON(line.Owed),
			"lifetime_paid":       amountJSON(line.LifetimePaid),
			"pending_commissions": line.PendingCommissions,
		})
		if _, seen := byCurrency[line.Currency]; !seen {
			order = append(order, line.Currency)
		}
		byCurrency[line.Currency] += line.Owed.Minor()
	}
	for _, code := range order {
		cur, err := money.LookupCurrency(code)
		if !err {
			continue
		}
		totals = append(totals, amountJSON(money.New(byCurrency[code], cur)))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"data":   out,
		"totals": totals,
	})
}

// --------------------------------------------------------------- staff write --

// createAffiliateRequest is the body of a registration.
type createAffiliateRequest struct {
	UserID         string `json:"user_id"`
	Code           string `json:"code"`
	CommissionRate string `json:"commission_rate"`
	PayoutMethod   string `json:"payout_method"`
}

func (s *Server) handleCreateAffiliate(w http.ResponseWriter, r *http.Request) {
	var in createAffiliateRequest
	if !s.decodeJSON(w, r, &in, 16<<10) {
		return
	}
	var created growth.Affiliate
	err := s.pool.InTx(r.Context(), func(ctx context.Context, tx pgx.Tx) error {
		var err error
		created, err = s.growthStore().Create(ctx, tx, growth.NewAffiliate{
			UserID:         in.UserID,
			Code:           in.Code,
			CommissionRate: in.CommissionRate,
			PayoutMethod:   in.PayoutMethod,
		})
		if err != nil {
			return err
		}
		ev := s.auditEvent(r, "affiliate.create", "affiliate", created.ID, audit.OutcomeSuccess).
			WithChanges(map[string]any{
				"user_id":         created.UserID,
				"code":            created.Code,
				"commission_rate": created.CommissionRate,
				"payout_method":   created.PayoutMethod,
			})
		return audit.Record(ctx, tx, ev)
	})
	if err != nil {
		s.growthError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, affiliateJSON(created))
}

// payoutRequest is the body of a payout. The currency is required because a balance is
// per currency and paying "everything" would mix currencies without a conversion rate.
type payoutRequest struct {
	Currency string `json:"currency"`
}

func (s *Server) handleAffiliatePayout(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !growth.ValidID(id) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such record")
		return
	}
	var in payoutRequest
	if !s.decodeJSON(w, r, &in, 8<<10) {
		return
	}
	currency := strings.ToUpper(strings.TrimSpace(in.Currency))
	if currency == "" {
		writeProblem(w, http.StatusUnprocessableEntity, "invalid_input", "currency is required")
		return
	}

	paid, err := s.growthStore().PayOut(r.Context(), id, currency)
	if err != nil {
		s.growthError(w, r, err)
		return
	}

	// The audit row is written after the settlement committed, in its own transaction.
	// That is a deliberate trade: PayOut owns its transaction (it locks the balance), and
	// wrapping it would mean a second connection for a row that describes a committed
	// fact. A failed audit write is logged rather than reported as a failed payout,
	// because the money has already moved and telling the operator otherwise invites a
	// second payout attempt.
	if err := s.pool.InTx(r.Context(), func(ctx context.Context, tx pgx.Tx) error {
		ev := s.auditEvent(r, "affiliate.payout", "affiliate", paid.AffiliateID, audit.OutcomeSuccess).
			WithChanges(map[string]any{
				"currency":    paid.Currency,
				"amount_minor": paid.Amount.Minor(),
				"commissions": paid.Commissions,
			})
		return audit.Record(ctx, tx, ev)
	}); err != nil {
		s.cfg.Logger.ErrorContext(r.Context(), "affiliate payout audit failed",
			"affiliate", paid.AffiliateID, "error", err)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"affiliate_id": paid.AffiliateID,
		"currency":     paid.Currency,
		"amount":       amountJSON(paid.Amount),
		"commissions":  paid.Commissions,
	})
}

// -------------------------------------------------------------------- portal --

// handlePortalAffiliate returns the caller's own affiliate code and earnings.
//
// The affiliate is resolved from the session's user id, and the referral rows are
// fetched by that affiliate id, so a caller can only ever see their own. A customer who
// is not an affiliate gets 404: enrolment is not something the endpoint implies.
func (s *Server) handlePortalAffiliate(w http.ResponseWriter, r *http.Request) {
	user, ok := userFrom(r.Context())
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "authentication_required",
			"an authenticated session is required")
		return
	}
	store := s.growthStore()
	affiliate, err := store.ByUserID(r.Context(), user.ID)
	if err != nil {
		if errors.Is(err, growth.ErrNotFound) {
			writeProblem(w, http.StatusNotFound, "not_an_affiliate",
				"this account is not enrolled in the affiliate programme")
			return
		}
		s.growthError(w, r, err)
		return
	}
	referrals, err := store.Referrals(r.Context(), affiliate.ID, 50)
	if err != nil {
		s.growthError(w, r, err)
		return
	}
	rows := make([]map[string]any, 0, len(referrals))
	for _, ref := range referrals {
		rows = append(rows, referralJSON(ref))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"code":            affiliate.Code,
		"commission_rate": affiliate.CommissionRate,
		"payout_method":   affiliate.PayoutMethod,
		"enabled":         affiliate.Enabled,
		"referred_count":  affiliate.ReferredCount,
		"balances":        affiliateJSON(affiliate)["balances"],
		"referrals":       rows,
	})
}

// --------------------------------------------------------------- storefront --

// captureReferral reads a ?ref= code from a public storefront request.
//
// The storefront is anonymous, so the code is not stored server-side here: it is echoed
// back in the catalog response so the frontend can keep it (a cookie or a query parameter
// carried into registration) and present it to POST /api/v1/auth/register, which passes it
// to growth.AttributeSignup. Echoing it rather than trusting it is deliberate — the code is
// validated against the affiliates table only when attribution happens, inside the
// registration transaction, where a forged code simply fails.
func captureReferral(r *http.Request) string {
	raw := strings.TrimSpace(r.URL.Query().Get("ref"))
	if raw == "" {
		return ""
	}
	// A code is short; anything longer is not one, and echoing it back unbounded would put
	// attacker-controlled text in a cached response.
	if len(raw) > 64 {
		return ""
	}
	return growth.NormalizeCode(raw)
}
