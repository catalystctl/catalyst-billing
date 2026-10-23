// The charge plan: what the unattended charger would take right now (OPS-01).
//
// The sweep and this route are the same decision asked twice. `POST /staff/payments/charge-due`
// performs it; `GET /staff/payments/charge-due` describes it without touching a provider. An
// operator will not enable a job that moves money with nobody watching without being able to see
// what it would move, and "trust the scan" is not a preview.
//
// # Why the permission is the reconcile permission and not a read permission
//
// The plan names customers, invoices and outstanding amounts — information the charger acts on.
// Anyone who may charge may see it; anyone who may not charge has no reason to enumerate the
// cards the panel is about to use.
//
// # Why it answers even when the job is off
//
// "The switch is off and it would have taken 412.00 across 9 invoices" is the sentence that makes
// the switch a decision rather than an abstraction. The route reports the kill switch and the
// setting separately, because a switch set in the process environment and a permission not granted
// are different problems with different fixes.
package httpapi

import (
	"net/http"

	"github.com/catalyst/billing/internal/authn"
	"github.com/catalyst/billing/internal/operations"
)

func init() {
	RegisterRoutes("38-charge-due-plan", func(r *router) {
		r.staffGet("/api/v1/staff/payments/charge-due",
			authn.PermPaymentReconcile, r.s.handleChargeDuePlan)
	})
}

// handleChargeDuePlan answers "what would the sweep charge now".
func (s *Server) handleChargeDuePlan(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "storage_unavailable", "no database configured")
		return
	}
	// The provider name, not the gateway: resolving a charger would make this route capable of
	// charging, and a preview that can charge is one more place a charge can happen.
	plan, err := operations.PlanChargeDue(r.Context(), s.pool, s.savedMethodProvider(), operations.Options{})
	if err != nil {
		s.cfg.Logger.ErrorContext(r.Context(), "charge plan failed", "error", err)
		writeProblem(w, http.StatusInternalServerError, "charge_plan_failed",
			"the plan could not be computed; see the server log")
		return
	}
	// A plan is a read of money that has not moved yet: caching it would let a page show a charge
	// that the next tick already made.
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, plan)
}
