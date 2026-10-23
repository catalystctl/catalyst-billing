// Paying the credit a downgrade owes (CRD-06, UPG-04, CRD-10).
//
// A downgrade is recorded by `service.Store.Upgrade` as a `service_upgrades` row with a negative
// prorated amount and reported as `credit_pending`. Posting that credit is the credits package's
// job — the service package refuses to reprice the subscription until it has happened, because
// otherwise the customer gets the cheaper price and keeps the money they are owed.
//
// The automation posts it (internal/lifecycle's `service-upgrades` job). This route is the other
// half: an operator looking at the pending-upgrade queue can post one now, and see the note it
// produced. Two entry points, one implementation — `credit.Service.PostDowngradeCredit` — because
// a "post it now" button that took a different path through the ledger would be a second way to
// spend credit, which is exactly what this file must not create.
//
// The two steps are deliberately separate and both idempotent:
//
//  1. Post the credit. Keyed on the upgrade row, so a replay finds the note instead of minting a
//     second one.
//  2. Apply the downgrade. `ApplyDowngrade` refuses a row that is not pending and requires the
//     credit, so a replay is a no-op rather than a second reprice or a second resize operation.
//
// A failure between them leaves the row pending *with* its credit linked, which is a state an
// operator can see and finish — not a customer with a missing refund.

package httpapi

import (
	"errors"
	"net/http"
	"time"

	"github.com/catalyst/billing/internal/authn"
	"github.com/catalyst/billing/internal/credit"
	"github.com/catalyst/billing/internal/portal"
	"github.com/catalyst/billing/internal/service"
)

func init() {
	RegisterRoutes("63-service-downgrade-credit", registerServiceDowngradeCredit)
}

func registerServiceDowngradeCredit(r *router) {
	// Money moves here, so it is credit:write rather than a service permission: the operator is
	// authorising a liability, and the reprice that follows is the consequence rather than the
	// point. The route also requires an Idempotency-Key (staffMutation), so a retried click
	// replays the stored response instead of posting twice.
	r.staffMutation(http.MethodPost, "/api/v1/staff/upgrades/{id}/credit",
		authn.PermCreditWrite, r.s.handlePostDowngradeCredit)
}

// handlePostDowngradeCredit posts the credit a pending downgrade owes and applies it.
func (s *Server) handlePostDowngradeCredit(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "storage_unavailable", "no database configured")
		return
	}
	id := r.PathValue("id")
	if !portal.ValidID(id) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such record")
		return
	}
	user, _ := userFrom(r.Context())

	posted, err := credit.New(s.pool).
		WithAuditor(s.auditorFor(r, "prorated downgrade credit")).
		PostDowngradeCredit(r.Context(), credit.DowngradeCreditRequest{
			UpgradeID: id,
			PostedBy:  user.ID,
		})
	switch {
	case err == nil:
	case errors.Is(err, credit.ErrNotFound):
		writeProblem(w, http.StatusNotFound, "not_found", "no such upgrade")
		return
	case errors.Is(err, credit.ErrNotDowngrade):
		writeProblem(w, http.StatusConflict, "not_a_downgrade",
			"that change charged the customer; there is no credit to post")
		return
	case errors.Is(err, credit.ErrDowngradeDisabled):
		writeProblem(w, http.StatusConflict, "downgrade_credit_disabled",
			"this installation does not return credit on a downgrade")
		return
	case errors.Is(err, credit.ErrDisabled):
		writeProblem(w, http.StatusConflict, "credit_disabled",
			"customer credit is disabled for this installation")
		return
	case errors.Is(err, credit.ErrNotPending):
		writeProblem(w, http.StatusConflict, "not_pending", err.Error())
		return
	case errors.Is(err, credit.ErrInvalidRequest):
		writeProblem(w, http.StatusBadRequest, "invalid_input", err.Error())
		return
	default:
		s.staffError(w, r, err)
		return
	}

	// The service moves to the cheaper price now that the money is posted. A refusal here is not a
	// failure of the request — the credit is committed — so it is reported as the row's state
	// rather than as an error. `already_applied` is the normal answer to a replay.
	applied := false
	applyErr := s.serviceStore(r).ApplyDowngrade(r.Context(), id)
	switch {
	case applyErr == nil:
		applied = true
	case errors.Is(applyErr, service.ErrNotPending):
		// Another worker (or a previous click) already applied it.
		applied = true
	default:
		s.cfg.Logger.ErrorContext(r.Context(), "downgrade credit posted but the downgrade was not applied",
			"upgrade_id", id, "error", applyErr)
	}

	s.publishServiceEvent(r, posted.ServiceID, "service.downgrade_credited", map[string]any{
		"service_id":      posted.ServiceID,
		"upgrade_id":      posted.UpgradeID,
		"credit_note_id":  posted.CreditNoteID,
		"credit_number":   posted.Number,
		"amount_minor":    posted.AmountMinor,
		"currency":        posted.Currency,
		"already_posted":  posted.AlreadyPosted,
		"downgrade_apply": applied,
	})

	body := map[string]any{
		"upgrade_id":     posted.UpgradeID,
		"service_id":     posted.ServiceID,
		"customer_id":    posted.CustomerID,
		"credit_note_id": posted.CreditNoteID,
		"number":         posted.Number,
		"currency":       posted.Currency,
		"amount_minor":   posted.AmountMinor,
		"reason":         posted.Reason,
		"created_at":     posted.CreatedAt.UTC().Format(time.RFC3339),
		"already_posted": posted.AlreadyPosted,
		"applied":        applied,
	}
	if posted.AlreadyPosted {
		writeJSON(w, http.StatusOK, body)
		return
	}
	writeJSON(w, http.StatusCreated, body)
}
