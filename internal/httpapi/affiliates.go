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
//   - GET /api/v1/staff/affiliates/{id}/payouts is the reconciliation for one payee over a
//     period: earned, paid, outstanding, and the rows that make up the difference.
//
//   - POST /api/v1/staff/affiliates/{id}/payouts records a payout. It is the same money path
//     as POST .../payout (both go through recordPayout), with one difference that matters: it
//     answers with the payout's own id, method and reference, so the settlement is a record
//     an operator can quote rather than a number they have to trust. Its guard is
//     credit:read:all, which is a borrow — every staff role holds it, support included. That
//     is a known weakness, recorded here rather than papered over: a dedicated
//     `affiliate:payout` is the right answer, and until it exists the report says what the
//     action currently requires.
//
//   - /api/v1/portal/affiliate is the customer's own view. The affiliate is resolved from
//     the *session*, never from the URL, so identifier substitution cannot read somebody
//     else's earnings. A customer who is not an affiliate gets 404, not 403: "you are not
//     enrolled" is not information about another record.
//
//   - Attribution itself is not a route here. Registration is owned by the customer
//     purchase package, so this file exports growth.AttributeSignup for that transaction
//     to call, and the storefront captures the ?ref= code via ReferralCodeFromRequest (see
//     the integrator note in the package report).
package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

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

	// The reconciliation for one affiliate over a period: what is owed, what has been paid,
	// and the rows that make up the difference. report:read, like the two above, because it
	// is the same question asked about one payee.
	r.staffGet("/api/v1/staff/affiliates/{id}/payouts", authn.PermReportRead,
		s.handleAffiliatePayoutReport)

	// Registering an affiliate fixes the rate at which money will later leave the
	// business, so it carries the same permission as the payout itself.
	r.staffMutation(http.MethodPost, "/api/v1/staff/affiliates",
		authn.PermCreditWrite, s.handleCreateAffiliate)

	// The four controls that change what an affiliate is worth, all on credit:write for the
	// same reason the registration is: each of them changes how much money will later leave
	// the business. A rate change sets what future commission pays; a suspension stops it
	// accruing; a reinstatement starts it again; a deletion retires the payee while leaving
	// the debt they are owed intact. report:read — the permission the list needs — is a read
	// permission, and a read permission must not be able to do any of that.
	r.staffMutation(http.MethodPatch, "/api/v1/staff/affiliates/{id}",
		authn.PermCreditWrite, s.handleUpdateAffiliate)
	r.staffMutation(http.MethodPost, "/api/v1/staff/affiliates/{id}/suspend",
		authn.PermCreditWrite, s.handleSuspendAffiliate)
	r.staffMutation(http.MethodPost, "/api/v1/staff/affiliates/{id}/reinstate",
		authn.PermCreditWrite, s.handleReinstateAffiliate)
	r.staffMutation(http.MethodDelete, "/api/v1/staff/affiliates/{id}",
		authn.PermCreditWrite, s.handleDeleteAffiliate)

	// The payout is a money action: idempotent, audited, and gated on credit:write.
	r.staffMutation(http.MethodPost, "/api/v1/staff/affiliates/{id}/payout",
		authn.PermCreditWrite, s.handleAffiliatePayout)

	// Recording a payout: one append-only row that links every commission it settled. It is
	// guarded by credit:read:all, which is a *borrow* — see the note in
	// internal/growth/payouts.go and the package report. credit:write (the permission the
	// legacy route above already uses) is the better existing guard, and a dedicated
	// `affiliate:payout` is the right long-term answer; neither exists as a decision this
	// package is allowed to make, so the action is guarded by the permission the assignment
	// named and the weakness is written down rather than hidden.
	r.staffMutation(http.MethodPost, "/api/v1/staff/affiliates/{id}/payouts",
		authn.PermCreditReadAll, s.handleRecordAffiliatePayout)

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
		// status is the field to branch on: 'active', 'suspended' or 'deleted'. It is
		// derived server-side from the state that actually gates earning, so a client that
		// re-derives it from `enabled` cannot disagree with what the server will do.
		"status":           a.Status,
		"suspended_at":     nullableString(a.SuspendedAt),
		"suspended_reason": a.SuspendedReason,
		"deleted_at":       nullableString(a.DeletedAt),
		"deleted_reason":   a.DeletedReason,
		"referred_count":   a.ReferredCount,
		"balances":         balances,
		"created_at":       a.CreatedAt,
	}
}

// nullableString (defined in staff_read.go) renders an optional timestamp as JSON null
// rather than an empty string, so a client can tell "never happened" from "happened at an
// empty instant".

// referralJSON renders one attribution or commission row.
func referralJSON(r growth.Referral) map[string]any {
	out := map[string]any{
		"id":            r.ID,
		"affiliate_id":  r.AffiliateID,
		"customer_id":   r.CustomerID,
		"status":        r.Status,
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
	case errors.Is(err, growth.ErrAffiliateDeleted):
		// A deleted affiliate is archived, not gone: their debt is still payable, but the
		// record is closed and nothing new can be decided about it.
		writeProblem(w, http.StatusConflict, "affiliate_deleted", "this affiliate has been deleted")
	case errors.Is(err, growth.ErrNothingToPay):
		writeProblem(w, http.StatusConflict, "nothing_to_pay", "there is no outstanding balance to pay")
	case errors.Is(err, growth.ErrBalanceMismatch):
		// The materialised balance and the commission rows disagree. Paying either figure would
		// move money nobody can account for, so the payout refuses and the report shows both.
		writeProblem(w, http.StatusConflict, "balance_mismatch", err.Error())
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
	// Deleted affiliates are excluded unless asked for. They remain reachable here because a
	// deleted affiliate can still be owed money, and the screen that settles a debt has to be
	// able to name the payee.
	includeDeleted := r.URL.Query().Get("include_deleted")
	items, err := s.growthStore().List(r.Context(), growth.ListOptions{
		IncludeDeleted: includeDeleted == "1" || strings.EqualFold(includeDeleted, "true"),
	})
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
			"deleted":             line.Deleted,
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

// --------------------------------------------------------- state changes --
//
// The four controls an operator has over a payee, each one an audited transaction: the
// store call and the audit row commit together, so an action nobody signed for cannot
// happen — the same rule the payout follows.

// updateAffiliateRequest is the body of a PATCH.
type updateAffiliateRequest struct {
	// CommissionRate is a pointer so "absent" and "the empty string" are different things.
	// An absent field is refused rather than defaulted: this route edits exactly one thing,
	// and a request that names nothing is a client bug worth reporting.
	CommissionRate *string `json:"commission_rate"`
}

// affiliateStateRequest is the body of a suspension or a deletion. The reason is required by
// the store: a ban or an archive that the next operator cannot understand is a decision that
// cannot be reviewed.
type affiliateStateRequest struct {
	Reason string `json:"reason"`
}

// affiliateActorID returns the signed-in operator's id, or "" for a caller without a session
// (which the route's permission check makes impossible in practice).
func affiliateActorID(r *http.Request) string {
	if user, ok := userFrom(r.Context()); ok {
		return user.ID
	}
	return ""
}

// handleUpdateAffiliate sets an affiliate's commission rate.
//
// The rate is an exact rational (`10%`, `0.1`, `1/10`) bounded at 100%, and it applies to
// future commission only: every commission row captures the rate that produced it, so this
// cannot restate what a past order earned. That is a property of the ledger, not a promise
// in a comment.
func (s *Server) handleUpdateAffiliate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !growth.ValidID(id) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such record")
		return
	}
	var in updateAffiliateRequest
	if !s.decodeJSON(w, r, &in, 8<<10) {
		return
	}
	if in.CommissionRate == nil {
		writeProblem(w, http.StatusUnprocessableEntity, "invalid_input",
			"commission_rate is required: it is the only field this route edits")
		return
	}

	store := s.growthStore()
	// The previous rate is read first so the audit row can state the change. A concurrent
	// change would make "from" stale; it cannot make the write wrong, because the rate this
	// request asked for is the rate that is written.
	before, err := store.Get(r.Context(), id)
	if err != nil {
		s.growthError(w, r, err)
		return
	}

	var updated growth.Affiliate
	err = s.pool.InTx(r.Context(), func(ctx context.Context, tx pgx.Tx) error {
		var err error
		updated, err = store.SetCommissionRate(ctx, tx, id, *in.CommissionRate)
		if err != nil {
			return err
		}
		ev := s.auditEvent(r, "affiliate.rate", "affiliate", updated.ID, audit.OutcomeSuccess).
			WithChanges(map[string]any{
				"from": before.CommissionRate,
				"to":   updated.CommissionRate,
				// Said in the audit row because a reader will otherwise assume a rate change
				// restates what has already been earned. It does not: the rows keep the rate
				// they were computed at.
				"applies_to": "future commissions only",
			})
		return audit.Record(ctx, tx, ev)
	})
	if err != nil {
		s.growthError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, affiliateJSON(updated))
}

// handleSuspendAffiliate bans an affiliate: their code stops attributing and no further
// commission accrues. What they have already earned is untouched and still payable.
func (s *Server) handleSuspendAffiliate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !growth.ValidID(id) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such record")
		return
	}
	var in affiliateStateRequest
	if !s.decodeJSON(w, r, &in, 8<<10) {
		return
	}

	var suspended growth.Affiliate
	err := s.pool.InTx(r.Context(), func(ctx context.Context, tx pgx.Tx) error {
		var err error
		suspended, err = s.growthStore().Suspend(ctx, tx, id, in.Reason, affiliateActorID(r))
		if err != nil {
			return err
		}
		ev := s.auditEvent(r, "affiliate.suspend", "affiliate", suspended.ID, audit.OutcomeSuccess).
			WithChanges(map[string]any{
				"reason": in.Reason,
				"status": suspended.Status,
			})
		return audit.Record(ctx, tx, ev)
	})
	if err != nil {
		s.growthError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, affiliateJSON(suspended))
}

// handleReinstateAffiliate puts a suspended affiliate back into the programme.
func (s *Server) handleReinstateAffiliate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !growth.ValidID(id) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such record")
		return
	}

	var reinstated growth.Affiliate
	err := s.pool.InTx(r.Context(), func(ctx context.Context, tx pgx.Tx) error {
		var err error
		reinstated, err = s.growthStore().Reinstate(ctx, tx, id, affiliateActorID(r))
		if err != nil {
			return err
		}
		ev := s.auditEvent(r, "affiliate.reinstate", "affiliate", reinstated.ID, audit.OutcomeSuccess).
			WithChanges(map[string]any{"status": reinstated.Status})
		return audit.Record(ctx, tx, ev)
	})
	if err != nil {
		s.growthError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, affiliateJSON(reinstated))
}

// handleDeleteAffiliate archives an affiliate. The row stays and so does every commission it
// earned: a deleted affiliate with unpaid commission is a debt, and the payout report keeps
// naming them until it is settled.
func (s *Server) handleDeleteAffiliate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !growth.ValidID(id) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such record")
		return
	}
	// A DELETE carries its reason in the body. The client's del() sends one, and a body on
	// DELETE is what lets the reason be recorded rather than inferred.
	var in affiliateStateRequest
	if !s.decodeJSON(w, r, &in, 8<<10) {
		return
	}

	var deleted growth.Affiliate
	err := s.pool.InTx(r.Context(), func(ctx context.Context, tx pgx.Tx) error {
		var err error
		deleted, err = s.growthStore().Delete(ctx, tx, id, in.Reason, affiliateActorID(r))
		if err != nil {
			return err
		}
		// What is still owed is written into the audit row, because that is the sentence an
		// operator needs when they archive a payee: the affiliate is gone, the debt is not.
		owed := make([]map[string]any, 0, len(deleted.Balances))
		for _, b := range deleted.Balances {
			if b.Owed.Minor() > 0 {
				owed = append(owed, map[string]any{
					"currency": b.Currency,
					"minor":    b.Owed.Minor(),
				})
			}
		}
		ev := s.auditEvent(r, "affiliate.delete", "affiliate", deleted.ID, audit.OutcomeSuccess).
			WithChanges(map[string]any{
				"reason":      in.Reason,
				"status":      deleted.Status,
				"still_owed":  owed,
				"ledger_kept": true,
			})
		return audit.Record(ctx, tx, ev)
	})
	if err != nil {
		s.growthError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, affiliateJSON(deleted))
}

// payoutRequest is the body of a payout. The currency is required because a balance is
// per currency and paying "everything" would mix currencies without a conversion rate.
type payoutRequest struct {
	Currency string `json:"currency"`
	// Method, Reference and Note are optional on the legacy route and meaningful on the new
	// one: a payout row records how the money left and what it can be reconciled against.
	Method    string `json:"method"`
	Reference string `json:"reference"`
	Note      string `json:"note"`
	// From and To record which window the operator was looking at. They do not filter what
	// is settled — a payout settles the whole outstanding balance — but they are the context
	// an auditor asks for.
	From string `json:"from"`
	To   string `json:"to"`
}

// handleAffiliatePayout settles one affiliate's balance in one currency.
//
// It is the original endpoint and it keeps its response shape and its credit:write guard. It
// delegates to the same implementation the new route uses, so there is one money path rather
// than two that can disagree.
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

	recorded, err := s.recordPayout(r, id, in, currency)
	if err != nil {
		s.growthError(w, r, err)
		return
	}

	// The shape this endpoint has always answered with, unchanged: a client that was written
	// against it keeps working.
	writeJSON(w, http.StatusOK, map[string]any{
		"affiliate_id": recorded.AffiliateID,
		"currency":     recorded.Currency,
		"amount":       amountJSON(recorded.Amount),
		"commissions":  recorded.Commissions,
	})
}

// handleRecordAffiliatePayout records a payout: an append-only row that links every
// commission it settled.
//
// The difference from the route above is not the money, it is the record. This one answers
// with the payout's own id, its method and reference, and how many of each kind of commission
// it settled, so an operator can quote it when reconciling a statement.
func (s *Server) handleRecordAffiliatePayout(w http.ResponseWriter, r *http.Request) {
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

	recorded, err := s.recordPayout(r, id, in, currency)
	if err != nil {
		s.growthError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, payoutJSON(recorded))
}

// recordPayout runs the settlement and writes its audit row in the same transaction.
//
// Both payout routes go through here so the money path is one path. The audit row is inside
// the transaction — unlike the original implementation, which audited afterwards and logged a
// failure — because a payout nobody signed for is exactly the record this feature exists to
// make impossible: if the audit write fails, the money does not move.
func (s *Server) recordPayout(r *http.Request, affiliateID string, in payoutRequest, currency string) (growth.RecordedPayout, error) {
	var (
		recorded growth.RecordedPayout
		from, to *time.Time
	)
	// The operator's id is recorded on the payout row itself, so the row and the audit event
	// name the same person. It is empty only for a caller with no session, which the route's
	// permission check makes impossible in practice.
	var actorID string
	if user, ok := userFrom(r.Context()); ok {
		actorID = user.ID
	}
	if strings.TrimSpace(in.From) != "" || strings.TrimSpace(in.To) != "" {
		f, err := parseWidgetTime(in.From, false)
		if err != nil {
			return growth.RecordedPayout{}, fmt.Errorf("%w: `from` must be an RFC3339 timestamp or a YYYY-MM-DD date", growth.ErrInvalidInput)
		}
		t, err := parseWidgetTime(in.To, true)
		if err != nil {
			return growth.RecordedPayout{}, fmt.Errorf("%w: `to` must be an RFC3339 timestamp or a YYYY-MM-DD date", growth.ErrInvalidInput)
		}
		if !t.After(f) {
			return growth.RecordedPayout{}, fmt.Errorf("%w: `to` must be after `from`", growth.ErrInvalidInput)
		}
		from, to = &f, &t
	}

	err := s.pool.InTx(r.Context(), func(ctx context.Context, tx pgx.Tx) error {
		var err error
		recorded, err = s.growthStore().RecordPayout(ctx, tx, growth.NewPayout{
			AffiliateID: affiliateID,
			Currency:    currency,
			Method:      in.Method,
			Reference:   in.Reference,
			Note:        in.Note,
			ActorID:     actorID,
			PeriodFrom:  from,
			PeriodTo:    to,
		})
		if err != nil {
			return err
		}
		ev := s.auditEvent(r, "affiliate.payout", "affiliate", recorded.AffiliateID, audit.OutcomeSuccess).
			WithChanges(map[string]any{
				"payout_id":           recorded.ID,
				"currency":            recorded.Currency,
				"amount_minor":        recorded.Amount.Minor(),
				"method":              recorded.Method,
				"reference":           recorded.Reference,
				"order_commissions":   recorded.OrderCommissions,
				"renewal_commissions": recorded.RenewalCommissions,
			})
		return audit.Record(ctx, tx, ev)
	})
	if err != nil {
		return growth.RecordedPayout{}, err
	}
	return recorded, nil
}

// payoutJSON renders a recorded payout.
func payoutJSON(p growth.RecordedPayout) map[string]any {
	return map[string]any{
		"id":                  p.ID,
		"affiliate_id":        p.AffiliateID,
		"currency":            p.Currency,
		"amount":              amountJSON(p.Amount),
		"method":              p.Method,
		"reference":           p.Reference,
		"commissions":         p.Commissions,
		"order_commissions":   p.OrderCommissions,
		"renewal_commissions": p.RenewalCommissions,
		"created_at":          p.CreatedAt,
	}
}

// handleAffiliatePayoutReport serves the reconciliation for one affiliate over a period.
//
// It is the report an operator reads before paying: what was earned, what has already been
// paid, what is still owed, and the rows that make up the difference — each commission with
// the invoice or order it came from and the payout that settled it, and each payout with its
// method and reference. The totals are all-time so that the period only scopes the rows: a
// window that predates the commissions still states what is owed.
func (s *Server) handleAffiliatePayoutReport(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !growth.ValidID(id) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such record")
		return
	}
	from, to, _, err := widgetWindow(r)
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_period", err.Error())
		return
	}

	store := s.growthStore()
	var lines []growth.PayoutReconciliation
	currency := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("currency")))
	if currency != "" {
		line, err := store.Reconcile(r.Context(), id, currency, from, to)
		if err != nil {
			s.growthError(w, r, err)
			return
		}
		lines = []growth.PayoutReconciliation{line}
	} else {
		lines, err = store.ReconcileAll(r.Context(), id, from, to)
		if err != nil {
			s.growthError(w, r, err)
			return
		}
	}

	history, err := store.Payouts(r.Context(), id, 50)
	if err != nil {
		s.growthError(w, r, err)
		return
	}

	data := make([]map[string]any, 0, len(lines))
	for _, line := range lines {
		data = append(data, reconciliationJSON(line))
	}
	payments := make([]map[string]any, 0, len(history))
	for _, p := range history {
		payments = append(payments, payoutHistoryJSON(p))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"data":    data,
		"history": payments,
		"period": map[string]any{
			"from": from,
			"to":   to,
		},
	})
}

// reconciliationJSON renders one currency's books.
func reconciliationJSON(line growth.PayoutReconciliation) map[string]any {
	rows := make([]map[string]any, 0, len(line.Rows))
	for _, row := range line.Rows {
		entry := map[string]any{
			"kind":        row.Kind,
			"id":          row.ID,
			"amount":      amountJSON(row.Amount),
			"status":      row.Status,
			"occurred_at": row.OccurredAt,
		}
		if row.InvoiceID != "" {
			entry["invoice_id"] = row.InvoiceID
		}
		if row.OrderID != "" {
			entry["order_id"] = row.OrderID
		}
		if row.Rate != "" {
			entry["rate"] = row.Rate
		}
		if row.PaidAt != "" {
			entry["paid_at"] = row.PaidAt
		}
		if row.PayoutID != "" {
			entry["payout_id"] = row.PayoutID
		}
		if row.RenewalIndex > 0 {
			entry["renewal_index"] = row.RenewalIndex
		}
		rows = append(rows, entry)
	}
	return map[string]any{
		"affiliate_id": line.AffiliateID,
		"code":         line.Code,
		"email":        line.Email,
		"currency":     line.Currency,
		"earned":       amountJSON(line.Earned),
		"paid":         amountJSON(line.Paid),
		"outstanding":  amountJSON(line.Outstanding),
		"payouts":      line.Payouts,
		"reconciles":   line.Reconciles,
		"rows":         rows,
	}
}

// payoutHistoryJSON renders one recorded payout.
func payoutHistoryJSON(p growth.Payout) map[string]any {
	out := map[string]any{
		"id":           p.ID,
		"affiliate_id": p.AffiliateID,
		"currency":     p.Currency,
		"amount":       amountJSON(p.Amount),
		"method":       p.Method,
		"reference":    p.Reference,
		"note":         p.Note,
		"created_at":   p.CreatedAt,
	}
	if p.CreatedBy != "" {
		out["created_by"] = p.CreatedBy
	}
	if p.PeriodFrom != "" {
		out["period_from"] = p.PeriodFrom
	}
	if p.PeriodTo != "" {
		out["period_to"] = p.PeriodTo
	}
	return out
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
		"status":          affiliate.Status,
		"referred_count":  affiliate.ReferredCount,
		"balances":        affiliateJSON(affiliate)["balances"],
		"referrals":       rows,
	})
}

// --------------------------------------------------------------- storefront --

// ReferralCodeFromRequest reads a ?ref= code from a public storefront request.
//
// The storefront is anonymous, so the code is not stored server-side here: it is echoed
// back in the catalog response so the frontend can keep it (a cookie, or a query parameter
// carried into registration) and present it to POST /api/v1/auth/register, which passes it
// to growth.AttributeSignup. Echoing it rather than trusting it is deliberate — the code is
// validated against the affiliates table only when attribution happens, inside the
// registration transaction, where a forged code simply fails.
//
// This is exported so the catalog handler (owned by the commerce package) can add it in one
// line without this package owning that file:
//
//	if code := ReferralCodeFromRequest(r); code != "" {
//		body["referral_code"] = code
//	}
//
// See the P12 report for the exact call site.
func ReferralCodeFromRequest(r *http.Request) string {
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
