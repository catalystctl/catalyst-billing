package httpapi

import (
	"context"
	"net/http"
	"time"
)

// The caller's own second-factor state, in the identity response.
//
// `GET /api/v1/portal/mfa` already answers this question for the customer's security screen, and
// `mfa_enrolment_required` is already carried on both the sign-in and identity responses. What was
// still missing is the fact an operator's account page and the panel shell need on every page
// load: *does my own account have a confirmed second factor?* Without it both account screens said
// "this page cannot tell you whether a second factor is on", and the shell had nothing to build a
// prompt from.
//
// So the same read — `authn.MFAStatusFor`, one indexed query — is reported as one object under
// `mfa` on `/api/v1/auth/me`, using the field names `GET /api/v1/portal/mfa` already uses. There
// is one Go source for the facts and one wire shape; the identity response is a carrier so the
// shell can render without a second request, and the dedicated endpoint stays the way a screen
// refreshes just this after an enrolment or a disable. Nothing here is a credential: it says
// whether a factor exists, when it was confirmed, how many recovery codes are left, and whether an
// operator's reset has left an enrolment demand outstanding.
//
// One addition to that shape: `available` is false when the state could not be read. A failed read
// is not a claim that the factor is off, and the UI must not offer to enrol a second factor on top
// of an unknown state. Whether the *installation* can seal a secret is deliberately not reported
// here — the authn service owns that answer and gives it as a 503 `mfa_unavailable` when somebody
// tries, which is one authority instead of two.

// mfaStateJSON is the caller's own second-factor state as `/auth/me` reports it.
//
// The field names are `GET /api/v1/portal/mfa`'s, on purpose: two carriers of one fact must not
// disagree about what to call it.
type mfaStateJSON struct {
	HasMFA                 bool    `json:"has_mfa"`
	EnrolledAt             *string `json:"enrolled_at"`
	RecoveryCodesRemaining int     `json:"recovery_codes_remaining"`
	EnrolmentRequired      bool    `json:"enrolment_required"`
	ResetAt                *string `json:"reset_at"`
	ResetBy                *string `json:"reset_by"`
	// Available is false when the state could not be read, so no screen offers to enrol on top of
	// a fact it could not see. It is *not* a report on whether the installation has a sealing key:
	// that is the authn service's business, and it answers for itself with a 503 `mfa_unavailable`
	// on enrolment, which the account screens render as "not configured on this server".
	Available bool `json:"available"`
}

// timePtr renders a nullable timestamp for JSON, in UTC like every other time on the wire.
func timePtr(t *time.Time) *string {
	if t == nil {
		return nil
	}
	at := t.UTC().Format(time.RFC3339)
	return &at
}

// mfaStateFor builds the state for one account.
func (s *Server) mfaStateFor(ctx context.Context, userID string) mfaStateJSON {
	state := mfaStateJSON{}
	if s.auth == nil || s.auth.Service == nil || userID == "" {
		return state
	}

	status, err := s.auth.Service.MFAStatusFor(ctx, userID)
	if err != nil {
		// A read failure is not "off". `available` stays false so no screen offers to enrol on top
		// of a state it could not see, and the error is logged rather than swallowed.
		s.cfg.Logger.ErrorContext(ctx, "could not read mfa state", "error", err)
		return state
	}

	state.Available = true
	state.HasMFA = status.HasMFA
	state.EnrolledAt = timePtr(status.EnrolledAt)
	state.RecoveryCodesRemaining = status.RecoveryCodesRemaining
	state.EnrolmentRequired = status.EnrolmentRequired
	state.ResetAt = timePtr(status.ResetAt)
	if status.ResetBy != "" {
		by := status.ResetBy
		state.ResetBy = &by
	}
	return state
}

// handleMFAState serves `GET /api/v1/auth/mfa`: the same object as `/auth/me`'s `mfa`, on its own.
//
// It exists so an account screen can refresh this one fact after an enrolment or a disable without
// re-reading the identity it already holds (and without the shell's session refresh, which would
// re-render the navigation). It is scoped to the caller by construction — there is no id in the
// path — and it applies the impersonation guard for the same reason enrolment does: an operator
// acting as a customer must not read or change that customer's credentials.
func (s *Server) handleMFAState(w http.ResponseWriter, r *http.Request) {
	if !s.impersonationGuard(w, r) {
		return
	}
	user, ok := userFrom(r.Context())
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "authentication_required", "not signed in")
		return
	}
	writeJSON(w, http.StatusOK, s.mfaStateFor(r.Context(), user.ID))
}
