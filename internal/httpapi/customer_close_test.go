package httpapi_test

// Password, second factor and account closure — from the customer overview and from the portal.
//
// These tests drive the real router and a real database, because every property under test is a
// composition rather than a unit: a permission declared on a route, a credential that the customer
// can actually sign in with afterwards, an audit row that names both the operator and the target,
// and a refusal that names the service or the invoice standing in the way. A test that called the
// service method directly would prove none of those.
//
// Each test is written so that deleting the behaviour makes it fail: the password test signs in
// with the password the operator set, the MFA test signs in again and reads the enrolment demand,
// the closure tests assert the refusal *and* that the record is still open, and the delete test
// reads the retained invoice, service and audit row back out of the database.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/catalyst/billing/internal/authn"
)

// newClosureEnv is a staff environment with an MFA encryption key configured.
//
// The key is set for the whole test because the second-factor paths refuse to run without one, and
// a key that changed mid-test would make an existing enrolment undecryptable.
func newClosureEnv(t *testing.T) *staffEnv {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("random mfa key: %v", err)
	}
	t.Setenv("BILLING_MFA_KEY", hex.EncodeToString(key))
	t.Setenv("BILLING_MFA_KEY_ID", "test-key")
	return newStaffEnv(t)
}

// enrolMFA drives the real enrolment flow for a signed-in account and returns its TOTP secret.
func (e *authEnv) enrolMFA(t *testing.T, cookie *http.Cookie, csrf string) string {
	t.Helper()
	rec := e.postJSON(t, "/api/v1/auth/mfa/enrol", cookie, csrf, map[string]any{})
	if rec.Code != http.StatusOK {
		t.Fatalf("enrol: status %d, body %s", rec.Code, rec.Body)
	}
	var enrolment struct {
		Secret string `json:"secret"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &enrolment); err != nil {
		t.Fatalf("decode enrolment: %v", err)
	}
	if enrolment.Secret == "" {
		t.Fatal("enrolment returned no secret, so the factor cannot be confirmed")
	}

	code, err := authn.TOTPCodeForTest(enrolment.Secret, time.Now().UTC())
	if err != nil {
		t.Fatalf("compute totp: %v", err)
	}
	confirmed := e.postJSON(t, "/api/v1/auth/mfa/confirm", cookie, csrf, map[string]any{"code": code})
	if confirmed.Code != http.StatusOK {
		t.Fatalf("confirm: status %d, body %s", confirmed.Code, confirmed.Body)
	}
	return enrolment.Secret
}

// --------------------------------------------------------------- password --

// A staff operator sets a customer's password from the customer overview, and the customer signs in
// with it.
//
// The account is a *customer*: the credential lives in the same `users` table as an agent's, and
// the route is the same one. What this test adds over the staff-user test is the path the screen
// actually takes — the customer overview's payload names the login, the operator sets its password,
// and the person on the other end of the phone can get in.
func TestStaffSetCustomerPasswordFromTheCustomerOverview(t *testing.T) {
	env := newStaffEnv(t)
	customerID, userID := env.seedCustomer(t, "setpw@example.test", "Set Password")

	// The overview is where the operator finds the login id: nothing here guesses it.
	detail := env.get(t, "/api/v1/staff/customers/"+customerID, env.admin)
	if detail.Code != http.StatusOK {
		t.Fatalf("customer overview: status %d, body %s", detail.Code, detail.Body)
	}
	var overview struct {
		Accounts []struct {
			UserID string `json:"user_id"`
			Email  string `json:"email"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(detail.Body.Bytes(), &overview); err != nil {
		t.Fatalf("decode overview: %v", err)
	}
	if len(overview.Accounts) != 1 || overview.Accounts[0].UserID != userID {
		t.Fatalf("overview accounts = %+v, want the seeded login %s", overview.Accounts, userID)
	}

	const chosen = "a password the operator set"
	rec := env.doUser(t, http.MethodPost, "/api/v1/staff/users/"+userID+"/password",
		env.admin, env.adminCSRF, map[string]any{"password": chosen})
	if rec.Code != http.StatusOK {
		t.Fatalf("set password: status %d, body %s", rec.Code, rec.Body)
	}
	// A credential in a response body is a credential in a log and in browser history.
	if strings.Contains(rec.Body.String(), chosen) {
		t.Fatalf("the response echoed the password: %s", rec.Body)
	}
	var set struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &set); err != nil {
		t.Fatalf("decode set response: %v", err)
	}
	if set.Status != "password_set" {
		t.Fatalf("status = %q, want password_set", set.Status)
	}

	// The point of the whole operation: the customer can sign in with it.
	env.login(t, "setpw@example.test", chosen)

	// And the password it replaced no longer works, which is what makes it a reset rather than an
	// addition.
	old := env.postPublic(t, "/api/v1/auth/login", map[string]any{
		"email": "setpw@example.test", "password": "customer password 1",
	})
	if old.Code != http.StatusUnauthorized {
		t.Fatalf("the old password still works: status %d, body %s", old.Code, old.Body)
	}

	// The audit row names the operator and the account it changed.
	var (
		actorID  string
		targetID string
	)
	if err := env.pool.QueryRow(context.Background(), `
		SELECT actor_id::text, target_id::text FROM audit_events
		WHERE action = 'user.password_set' ORDER BY created_at DESC LIMIT 1`).
		Scan(&actorID, &targetID); err != nil {
		t.Fatalf("read the audit row: %v", err)
	}
	if targetID != userID {
		t.Fatalf("audit target = %s, want the customer's login %s", targetID, userID)
	}
	var adminID string
	if err := env.pool.QueryRow(context.Background(),
		`SELECT id::text FROM users WHERE email = 'admin@example.test'`).Scan(&adminID); err != nil {
		t.Fatalf("read admin id: %v", err)
	}
	if actorID != adminID {
		t.Fatalf("audit actor = %s, want the operator %s", actorID, adminID)
	}
}

// -------------------------------------------------------------------- mfa --

// A staff operator resets a customer's second factor from the customer overview. The reset is
// audited naming the actor and the target, and the account's next session is told to enrol again.
func TestStaffResetCustomerMFAFromTheCustomerOverview(t *testing.T) {
	env := newClosureEnv(t)
	_, userID := env.seedCustomer(t, "mfa-customer@example.test", "MFA Customer")

	cookie, csrf := env.login(t, "mfa-customer@example.test", "customer password 1")
	env.enrolMFA(t, cookie, csrf)

	// A sign-in now stops at the code. This is the state support is telephoned about.
	challenge := env.postPublic(t, "/api/v1/auth/login", map[string]any{
		"email": "mfa-customer@example.test", "password": "customer password 1",
	})
	var step struct {
		MFARequired bool `json:"mfa_required"`
	}
	if err := json.Unmarshal(challenge.Body.Bytes(), &step); err != nil {
		t.Fatalf("decode login: %v", err)
	}
	if !step.MFARequired {
		t.Fatalf("the login did not demand the second factor: %s", challenge.Body)
	}

	// The overview reads the state before offering the action.
	state := env.get(t, "/api/v1/staff/users/"+userID+"/security", env.admin)
	if state.Code != http.StatusOK {
		t.Fatalf("security state: status %d, body %s", state.Code, state.Body)
	}
	var before struct {
		HasMFA          bool `json:"has_mfa"`
		RecoveryCodes   int  `json:"recovery_codes_remaining"`
		EnrolmentNeeded bool `json:"enrolment_required"`
	}
	if err := json.Unmarshal(state.Body.Bytes(), &before); err != nil {
		t.Fatalf("decode state: %v", err)
	}
	if !before.HasMFA || before.EnrolmentNeeded {
		t.Fatalf("state = %+v, want a confirmed factor and no outstanding demand", before)
	}
	if before.RecoveryCodes != authn.RecoveryCodeCount {
		t.Fatalf("recovery codes = %d, want %d", before.RecoveryCodes, authn.RecoveryCodeCount)
	}

	reset := env.doUser(t, http.MethodPost, "/api/v1/staff/users/"+userID+"/mfa/reset",
		env.admin, env.adminCSRF, nil)
	if reset.Code != http.StatusOK {
		t.Fatalf("reset: status %d, body %s", reset.Code, reset.Body)
	}
	var resetBody struct {
		Status               string `json:"status"`
		EnrolmentRequired    bool   `json:"enrolment_required"`
		NextSignInNeedsEnrol bool   `json:"next_sign_in_requires_enrolment"`
		RecoveryRemoved      int    `json:"recovery_codes_removed"`
	}
	if err := json.Unmarshal(reset.Body.Bytes(), &resetBody); err != nil {
		t.Fatalf("decode reset: %v", err)
	}
	if resetBody.Status != "mfa_reset" || !resetBody.EnrolmentRequired || !resetBody.NextSignInNeedsEnrol {
		t.Fatalf("reset body = %+v, want mfa_reset with an outstanding enrolment demand", resetBody)
	}
	if resetBody.RecoveryRemoved != authn.RecoveryCodeCount {
		t.Fatalf("recovery codes removed = %d, want %d", resetBody.RecoveryRemoved, authn.RecoveryCodeCount)
	}

	// Audited, naming the operator and the target — the two facts a review asks for.
	var (
		actorID  string
		targetID string
		changes  string
	)
	if err := env.pool.QueryRow(context.Background(), `
		SELECT actor_id::text, target_id::text, changes::text FROM audit_events
		WHERE action = 'user.mfa_reset' ORDER BY created_at DESC LIMIT 1`).
		Scan(&actorID, &targetID, &changes); err != nil {
		t.Fatalf("read the audit row: %v", err)
	}
	var adminID string
	if err := env.pool.QueryRow(context.Background(),
		`SELECT id::text FROM users WHERE email = 'admin@example.test'`).Scan(&adminID); err != nil {
		t.Fatalf("read admin id: %v", err)
	}
	if actorID != adminID {
		t.Fatalf("audit actor = %s, want the operator %s", actorID, adminID)
	}
	if targetID != userID {
		t.Fatalf("audit target = %s, want the customer's login %s", targetID, userID)
	}
	if !strings.Contains(changes, "mfa-customer@example.test") {
		t.Fatalf("the audit row does not name the target's address: %s", changes)
	}

	// The customer's own trail records it too: "what happened to my account" is answered from the
	// trail they can see, not only from the operator's audit log.
	var event string
	if err := env.pool.QueryRow(context.Background(), `
		SELECT event FROM auth_events WHERE user_id = $1
		ORDER BY created_at DESC LIMIT 1`, userID).Scan(&event); err != nil {
		t.Fatalf("read the customer's trail: %v", err)
	}
	if event != "mfa_reset_by_operator" {
		t.Fatalf("latest auth event = %q, want mfa_reset_by_operator", event)
	}

	// The credential, the recovery codes and any live challenge are gone: a reset removes an
	// enrolment rather than revealing it, and nothing is left behind that would still work.
	var credentials, codes, challenges int
	if err := env.pool.QueryRow(context.Background(), `
		SELECT (SELECT count(*) FROM mfa_credentials WHERE user_id = $1),
		       (SELECT count(*) FROM mfa_recovery_codes WHERE user_id = $1),
		       (SELECT count(*) FROM mfa_challenges WHERE user_id = $1)`, userID).
		Scan(&credentials, &codes, &challenges); err != nil {
		t.Fatalf("count remaining mfa rows: %v", err)
	}
	if credentials != 0 || codes != 0 || challenges != 0 {
		t.Fatalf("rows left behind: %d credentials, %d codes, %d challenges", credentials, codes, challenges)
	}

	// The next sign-in is password-only, and the account is told it owes the panel a factor. That
	// is the difference between a reset and a quiet downgrade to password-only.
	cookie2, csrf2, signInBody := env.loginReadingDemand(t, "mfa-customer@example.test", "customer password 1")
	if !signInBody.EnrolmentRequired {
		t.Fatal("the sign-in response did not carry the enrolment demand, so the shell cannot " +
			"send the customer to the enrolment screen")
	}

	// The identity read carries it too, which is what a page load, a second tab and a bookmarked
	// URL all go through.
	me := env.get(t, "/api/v1/auth/me", cookie2)
	if me.Code != http.StatusOK {
		t.Fatalf("identity: status %d, body %s", me.Code, me.Body)
	}
	var identity struct {
		MFAEnrolmentRequired bool `json:"mfa_enrolment_required"`
	}
	if err := json.Unmarshal(me.Body.Bytes(), &identity); err != nil {
		t.Fatalf("decode identity: %v", err)
	}
	if !identity.MFAEnrolmentRequired {
		t.Fatal("the identity read does not report the demand, so every navigation would be allowed")
	}

	mfa := env.get(t, "/api/v1/portal/mfa", cookie2)
	if mfa.Code != http.StatusOK {
		t.Fatalf("portal mfa state: status %d, body %s", mfa.Code, mfa.Body)
	}
	var after struct {
		HasMFA            bool `json:"has_mfa"`
		EnrolmentRequired bool `json:"enrolment_required"`
	}
	if err := json.Unmarshal(mfa.Body.Bytes(), &after); err != nil {
		t.Fatalf("decode portal mfa: %v", err)
	}
	if after.HasMFA {
		t.Fatal("the reset left a confirmed second factor behind")
	}
	if !after.EnrolmentRequired {
		t.Fatal("the account was not told to enrol a new factor, so the reset silently weakened it")
	}

	// Enrolling and confirming answers the demand.
	env.enrolMFA(t, cookie2, csrf2)
	mfa = env.get(t, "/api/v1/portal/mfa", cookie2)
	if err := json.Unmarshal(mfa.Body.Bytes(), &after); err != nil {
		t.Fatalf("decode portal mfa: %v", err)
	}
	if !after.HasMFA || after.EnrolmentRequired {
		t.Fatalf("after re-enrolment: %+v, want a confirmed factor and no demand", after)
	}

	// And the identity read agrees, which is what lets the shell release the redirect. A demand
	// that stayed set would trap the customer on the enrolment screen for ever.
	me = env.get(t, "/api/v1/auth/me", cookie2)
	if err := json.Unmarshal(me.Body.Bytes(), &identity); err != nil {
		t.Fatalf("decode identity: %v", err)
	}
	if identity.MFAEnrolmentRequired {
		t.Fatal("the demand survived a confirmed enrolment, so the customer would be redirected " +
			"back to the enrolment screen on every navigation")
	}
}

// Resetting a factor that is not there is refused rather than silently creating a demand.
func TestStaffMFAResetRefusedWhenNothingIsEnrolled(t *testing.T) {
	env := newStaffEnv(t)
	_, userID := env.seedCustomer(t, "nomfa@example.test", "No Factor")

	rec := env.doUser(t, http.MethodPost, "/api/v1/staff/users/"+userID+"/mfa/reset",
		env.admin, env.adminCSRF, nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("reset with no factor: status %d, want 409; body %s", rec.Code, rec.Body)
	}
	if code := problemCode(t, rec); code != "mfa_not_enrolled" {
		t.Fatalf("code = %q, want mfa_not_enrolled", code)
	}

	var required bool
	if err := env.pool.QueryRow(context.Background(),
		`SELECT mfa_enrolment_required FROM users WHERE id = $1`, userID).Scan(&required); err != nil {
		t.Fatalf("read demand: %v", err)
	}
	if required {
		t.Fatal("a refused reset left an enrolment demand on the account")
	}
}

// loginReadingDemand signs in and returns the session cookie, the CSRF token and the part of the
// response the shell branches on.
//
// `authEnv.login` throws the body away, and the enrolment demand is *only* in the body — it is the
// fact the client uses to send a customer to the enrolment screen, so a test that never reads it
// proves nothing about that behaviour.
func (e *authEnv) loginReadingDemand(t *testing.T, email, password string) (*http.Cookie, string, loginDemand) {
	t.Helper()
	rec := e.postPublic(t, "/api/v1/auth/login", map[string]any{"email": email, "password": password})
	if rec.Code != http.StatusOK {
		t.Fatalf("login: status %d, body %s", rec.Code, rec.Body)
	}
	var body struct {
		EnrolmentRequired bool   `json:"mfa_enrolment_required"`
		CSRFToken         string `json:"csrf_token"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode login: %v", err)
	}
	for _, cookie := range rec.Result().Cookies() {
		if cookie.Name == "billing_session" {
			return cookie, body.CSRFToken, loginDemand{EnrolmentRequired: body.EnrolmentRequired}
		}
	}
	t.Fatal("no session cookie was set")
	return nil, "", loginDemand{}
}

// loginDemand is the part of the sign-in response the shell reads.
type loginDemand struct {
	EnrolmentRequired bool `json:"mfa_enrolment_required"`
}

// ---------------------------------------------------------------- closure --

// A customer closes their own account from the portal: the confirmation states what happens to
// services and invoices, the record is closed rather than deleted, and the login stops working.
func TestCustomerClosesTheirOwnAccount(t *testing.T) {
	env := newStaffEnv(t)
	customerID, userID := env.seedCustomer(t, "leaving@example.test", "Leaving")
	cookie, csrf := env.login(t, "leaving@example.test", "customer password 1")

	// The preflight is what the confirmation dialog is built from.
	pre := env.get(t, "/api/v1/portal/account-closure", cookie)
	if pre.Code != http.StatusOK {
		t.Fatalf("preflight: status %d, body %s", pre.Code, pre.Body)
	}
	var preflight struct {
		CanClose  bool             `json:"can_close"`
		Blockers  []map[string]any `json:"blockers"`
		Statement string           `json:"statement"`
		Phrase    string           `json:"confirm_phrase"`
		Effects   map[string]struct {
			Detail string `json:"detail"`
		} `json:"effects"`
	}
	if err := json.Unmarshal(pre.Body.Bytes(), &preflight); err != nil {
		t.Fatalf("decode preflight: %v", err)
	}
	if !preflight.CanClose || len(preflight.Blockers) != 0 {
		t.Fatalf("preflight = %+v, want a closeable account", preflight)
	}
	// The confirmation must state what happens to live services and unpaid invoices, from the
	// server's own facts rather than from prose in the screen.
	for _, key := range []string{"services", "invoices", "record", "portal_access"} {
		effect, ok := preflight.Effects[key]
		if !ok || strings.TrimSpace(effect.Detail) == "" {
			t.Fatalf("effects[%q] is missing or empty: %+v", key, preflight.Effects)
		}
	}
	if !strings.Contains(preflight.Statement, "service") || !strings.Contains(preflight.Statement, "invoice") {
		t.Fatalf("statement does not say what happens to services and invoices: %q", preflight.Statement)
	}
	if preflight.Phrase == "" {
		t.Fatal("no confirmation phrase was served, so the dialog cannot ask for one")
	}

	// The typed phrase is enforced by the server, not only by the dialog.
	wrong := env.postJSON(t, "/api/v1/portal/account-closure", cookie, csrf,
		map[string]any{"confirm": "yes please"})
	if wrong.Code != http.StatusUnprocessableEntity {
		t.Fatalf("wrong phrase: status %d, want 422; body %s", wrong.Code, wrong.Body)
	}
	if code := problemCode(t, wrong); code != "confirmation_mismatch" {
		t.Fatalf("code = %q, want confirmation_mismatch", code)
	}
	var closedAt *time.Time
	if err := env.pool.QueryRow(context.Background(),
		`SELECT deleted_at FROM customers WHERE id = $1`, customerID).Scan(&closedAt); err != nil {
		t.Fatalf("read customer: %v", err)
	}
	if closedAt != nil {
		t.Fatal("a refused confirmation closed the account")
	}

	closed := env.postJSON(t, "/api/v1/portal/account-closure", cookie, csrf,
		map[string]any{"confirm": preflight.Phrase, "reason": "moving to another host"})
	if closed.Code != http.StatusOK {
		t.Fatalf("close: status %d, body %s", closed.Code, closed.Body)
	}
	var result struct {
		Status  string `json:"status"`
		Effects map[string]struct {
			Detail string `json:"detail"`
		} `json:"effects"`
	}
	if err := json.Unmarshal(closed.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode close: %v", err)
	}
	if result.Status != "closed" || len(result.Effects) == 0 {
		t.Fatalf("close body = %+v", result)
	}

	// Closed, not deleted: the row, its login and its history survive, and the closure says it was
	// the customer's own act.
	var (
		kind    string
		reason  string
		deleted *string
	)
	if err := env.pool.QueryRow(context.Background(), `
		SELECT deletion_kind, deletion_reason, deleted_by::text FROM customers WHERE id = $1`,
		customerID).Scan(&kind, &reason, &deleted); err != nil {
		t.Fatalf("read closure: %v", err)
	}
	if kind != "self" {
		t.Fatalf("deletion_kind = %q, want self", kind)
	}
	if reason != "moving to another host" {
		t.Fatalf("deletion_reason = %q", reason)
	}
	if deleted != nil {
		t.Fatalf("deleted_by = %v, want null: nobody acted on the customer's behalf", *deleted)
	}
	var users int
	if err := env.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM users WHERE id = $1`, userID).Scan(&users); err != nil {
		t.Fatalf("count logins: %v", err)
	}
	if users != 1 {
		t.Fatal("the login was deleted; a close must keep the record")
	}

	// The session is gone and the login no longer works.
	if me := env.get(t, "/api/v1/auth/me", cookie); me.Code != http.StatusUnauthorized {
		t.Fatalf("the closed account's session still works: status %d", me.Code)
	}
	signIn := env.postPublic(t, "/api/v1/auth/login", map[string]any{
		"email": "leaving@example.test", "password": "customer password 1",
	})
	if signIn.Code != http.StatusForbidden {
		t.Fatalf("a closed account could still sign in: status %d, body %s", signIn.Code, signIn.Body)
	}

	// Audited: the customer is the actor and the record is the target.
	var (
		actorID  string
		targetID string
		action   string
	)
	if err := env.pool.QueryRow(context.Background(), `
		SELECT actor_id::text, target_id::text, action FROM audit_events
		WHERE action = 'customer.close' ORDER BY created_at DESC LIMIT 1`).
		Scan(&actorID, &targetID, &action); err != nil {
		t.Fatalf("read the audit row: %v", err)
	}
	if actorID != userID || targetID != customerID {
		t.Fatalf("audit actor/target = %s/%s, want %s/%s", actorID, targetID, userID, customerID)
	}
}

// A live service stops a customer closing their account, and the refusal names it.
func TestCustomerCannotCloseWithALiveService(t *testing.T) {
	env := newStaffEnv(t)
	customerID, _ := env.seedCustomer(t, "has-service@example.test", "Has Service")
	env.seedService(t, customerID, "active")
	cookie, csrf := env.login(t, "has-service@example.test", "customer password 1")

	pre := env.get(t, "/api/v1/portal/account-closure", cookie)
	var preflight struct {
		CanClose bool `json:"can_close"`
		Blockers []struct {
			Kind   string `json:"kind"`
			Status string `json:"status"`
			Detail string `json:"detail"`
		} `json:"blockers"`
	}
	if err := json.Unmarshal(pre.Body.Bytes(), &preflight); err != nil {
		t.Fatalf("decode preflight: %v", err)
	}
	if preflight.CanClose {
		t.Fatal("an account with a live service was reported as closeable")
	}
	if len(preflight.Blockers) != 1 || preflight.Blockers[0].Kind != "live_service" {
		t.Fatalf("blockers = %+v, want one live_service", preflight.Blockers)
	}
	if !strings.Contains(preflight.Blockers[0].Detail, "VPS") ||
		!strings.Contains(preflight.Blockers[0].Detail, "active") {
		t.Fatalf("blocker detail does not name the service and its state: %q", preflight.Blockers[0].Detail)
	}

	rec := env.postJSON(t, "/api/v1/portal/account-closure", cookie, csrf,
		map[string]any{"confirm": "close my account"})
	if rec.Code != http.StatusConflict {
		t.Fatalf("close with a live service: status %d, want 409; body %s", rec.Code, rec.Body)
	}
	if code := problemCode(t, rec); code != "account_close_blocked" {
		t.Fatalf("code = %q, want account_close_blocked", code)
	}
	// The refusal has to name the blocking thing: a generic refusal is a support ticket.
	for _, want := range []string{"VPS", "active"} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("the refusal does not name %q: %s", want, rec.Body)
		}
	}

	// And nothing was written.
	var deletedAt *time.Time
	if err := env.pool.QueryRow(context.Background(),
		`SELECT deleted_at FROM customers WHERE id = $1`, customerID).Scan(&deletedAt); err != nil {
		t.Fatalf("read customer: %v", err)
	}
	if deletedAt != nil {
		t.Fatal("the refused close marked the account closed anyway")
	}
}

// An unpaid invoice stops a customer closing their account, and the refusal names it with the
// amount outstanding. A paid invoice does not block.
func TestCustomerCannotCloseWithAnUnpaidInvoice(t *testing.T) {
	env := newStaffEnv(t)
	customerID, _ := env.seedCustomer(t, "owes@example.test", "Owes Money")
	// A settled invoice is history, not a debt: it must not stop anything.
	env.seedInvoice(t, customerID, "paid", "USD", 5000, 5000, nil)
	cookie, csrf := env.login(t, "owes@example.test", "customer password 1")

	pre := env.get(t, "/api/v1/portal/account-closure", cookie)
	var openOnly struct {
		CanClose bool `json:"can_close"`
	}
	if err := json.Unmarshal(pre.Body.Bytes(), &openOnly); err != nil {
		t.Fatalf("decode preflight: %v", err)
	}
	if !openOnly.CanClose {
		t.Fatalf("a paid invoice blocked the close: %s", pre.Body)
	}

	due := time.Now().Add(-24 * time.Hour)
	env.seedInvoice(t, customerID, "open", "USD", 1200, 0, &due)

	rec := env.postJSON(t, "/api/v1/portal/account-closure", cookie, csrf,
		map[string]any{"confirm": "close my account"})
	if rec.Code != http.StatusConflict {
		t.Fatalf("close with an unpaid invoice: status %d, want 409; body %s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	for _, want := range []string{"INV-", "12.00", "USD", "unpaid"} {
		if !strings.Contains(body, want) {
			t.Fatalf("the refusal does not name %q: %s", want, body)
		}
	}
}

// Staff closing a customer record is refused while a service is live, and the refusal names it.
func TestStaffDeleteCustomerRefusedWhileAServiceIsLive(t *testing.T) {
	env := newStaffEnv(t)
	customerID, _ := env.seedCustomer(t, "live-svc@example.test", "Live Service")
	env.seedService(t, customerID, "suspended")

	rec := env.doUser(t, http.MethodDelete, "/api/v1/staff/customers/"+customerID,
		env.admin, env.adminCSRF, map[string]any{"reason": "duplicate record"})
	if rec.Code != http.StatusConflict {
		t.Fatalf("delete with a live service: status %d, want 409; body %s", rec.Code, rec.Body)
	}
	if code := problemCode(t, rec); code != "account_close_blocked" {
		t.Fatalf("code = %q, want account_close_blocked", code)
	}
	for _, want := range []string{"VPS", "suspended"} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("the refusal does not name %q: %s", want, rec.Body)
		}
	}

	var deletedAt *time.Time
	if err := env.pool.QueryRow(context.Background(),
		`SELECT deleted_at FROM customers WHERE id = $1`, customerID).Scan(&deletedAt); err != nil {
		t.Fatalf("read customer: %v", err)
	}
	if deletedAt != nil {
		t.Fatal("the refused delete closed the record anyway")
	}
}

// An unpaid invoice stops an operator closing a customer record too, and the refusal names it.
//
// The operator's guard is the same as the customer's on purpose: closing a record is not a way to
// make a debt disappear, and a delete that quietly orphaned an open invoice would leave a demand
// for money attached to an account nobody is watching.
func TestStaffDeleteCustomerRefusedWhileAnInvoiceIsUnpaid(t *testing.T) {
	env := newStaffEnv(t)
	customerID, _ := env.seedCustomer(t, "owes-staff@example.test", "Owes Staff")
	due := time.Now().Add(-48 * time.Hour)
	env.seedInvoice(t, customerID, "open", "EUR", 9900, 0, &due)

	rec := env.doUser(t, http.MethodDelete, "/api/v1/staff/customers/"+customerID,
		env.admin, env.adminCSRF, map[string]any{"reason": "customer gone away"})
	if rec.Code != http.StatusConflict {
		t.Fatalf("delete with an unpaid invoice: status %d, want 409; body %s", rec.Code, rec.Body)
	}
	if code := problemCode(t, rec); code != "account_close_blocked" {
		t.Fatalf("code = %q, want account_close_blocked", code)
	}
	for _, want := range []string{"INV-", "99.00", "EUR", "unpaid"} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("the refusal does not name %q: %s", want, rec.Body)
		}
	}

	var deletedAt *time.Time
	if err := env.pool.QueryRow(context.Background(),
		`SELECT deleted_at FROM customers WHERE id = $1`, customerID).Scan(&deletedAt); err != nil {
		t.Fatalf("read customer: %v", err)
	}
	if deletedAt != nil {
		t.Fatal("the refused delete closed the record anyway")
	}
}

// Staff deleting a customer is a soft close: the record, its invoices, its services and the audit
// trail survive, the login stops working, and the audit row names the operator and the reason.
func TestStaffDeleteCustomerKeepsTheRecordAndItsHistory(t *testing.T) {
	env := newStaffEnv(t)
	customerID, userID := env.seedCustomer(t, "closing@example.test", "Closing Down")
	invoiceID := env.seedInvoice(t, customerID, "paid", "USD", 4200, 4200, nil)
	serviceID, _ := env.seedService(t, customerID, "terminated")
	cookie, _ := env.login(t, "closing@example.test", "customer password 1")

	const why = "customer asked us to remove their record"
	rec := env.doUser(t, http.MethodDelete, "/api/v1/staff/customers/"+customerID,
		env.admin, env.adminCSRF, map[string]any{"reason": why})
	if rec.Code != http.StatusOK {
		t.Fatalf("delete: status %d, body %s", rec.Code, rec.Body)
	}

	// The record is closed, and it says who closed it and why.
	var (
		kind     string
		reason   string
		closedBy string
		closedAt *time.Time
	)
	if err := env.pool.QueryRow(context.Background(), `
		SELECT deletion_kind, deletion_reason, deleted_by::text, deleted_at
		FROM customers WHERE id = $1`, customerID).Scan(&kind, &reason, &closedBy, &closedAt); err != nil {
		t.Fatalf("read closure: %v", err)
	}
	if closedAt == nil || kind != "staff" || reason != why {
		t.Fatalf("closure = %v/%q/%q, want a staff close with the stated reason", closedAt, kind, reason)
	}

	// Nothing was cascaded: the invoice and the service are still there, which is the whole reason
	// this is a soft delete.
	var invoices, services int
	if err := env.pool.QueryRow(context.Background(), `
		SELECT (SELECT count(*) FROM invoices WHERE id = $1),
		       (SELECT count(*) FROM services WHERE id = $2)`, invoiceID, serviceID).
		Scan(&invoices, &services); err != nil {
		t.Fatalf("count retained records: %v", err)
	}
	if invoices != 1 || services != 1 {
		t.Fatalf("retained %d invoices and %d services, want 1 and 1", invoices, services)
	}

	// The login is disabled and its session is gone.
	var status string
	if err := env.pool.QueryRow(context.Background(),
		`SELECT status FROM users WHERE id = $1`, userID).Scan(&status); err != nil {
		t.Fatalf("read login: %v", err)
	}
	if status != "disabled" {
		t.Fatalf("login status = %q, want disabled", status)
	}
	if me := env.get(t, "/api/v1/auth/me", cookie); me.Code != http.StatusUnauthorized {
		t.Fatalf("the deleted customer's session still works: status %d", me.Code)
	}

	// Audited: the operator, the record, and the reason.
	var (
		actorID  string
		targetID string
		changes  string
	)
	if err := env.pool.QueryRow(context.Background(), `
		SELECT actor_id::text, target_id::text, changes::text FROM audit_events
		WHERE action = 'customer.delete' ORDER BY created_at DESC LIMIT 1`).
		Scan(&actorID, &targetID, &changes); err != nil {
		t.Fatalf("read the audit row: %v", err)
	}
	var adminID string
	if err := env.pool.QueryRow(context.Background(),
		`SELECT id::text FROM users WHERE email = 'admin@example.test'`).Scan(&adminID); err != nil {
		t.Fatalf("read admin id: %v", err)
	}
	if actorID != adminID || targetID != customerID {
		t.Fatalf("audit actor/target = %s/%s, want %s/%s", actorID, targetID, adminID, customerID)
	}
	if !strings.Contains(changes, why) {
		t.Fatalf("the audit row does not carry the reason: %s", changes)
	}

	// A second delete is refused rather than repeated.
	again := env.doUser(t, http.MethodDelete, "/api/v1/staff/customers/"+customerID,
		env.admin, env.adminCSRF, map[string]any{"reason": "again"})
	if again.Code != http.StatusConflict {
		t.Fatalf("second delete: status %d, want 409; body %s", again.Code, again.Body)
	}
	if code := problemCode(t, again); code != "already_closed" {
		t.Fatalf("code = %q, want already_closed", code)
	}
}

// Closing a customer record must never disable a staff login.
//
// A login can hold a staff role *and* be the portal account of a customer record — a staff member
// who is also a customer of the company, and, since every login is given a billing record at
// creation, every operator. Closing that billing relationship ends the customer's access to the
// portal; it must not revoke the person's access to the panel, because that would be an operator
// locking themselves (or a colleague) out of the product by closing an account record.
func TestClosingACustomerRecordNeverDisablesAStaffLogin(t *testing.T) {
	env := newStaffEnv(t)
	customerID, _ := env.seedCustomer(t, "dual-role@example.test", "Dual Role")

	// The same login is an administrator and the portal account of this customer.
	var staffUserID string
	if err := env.pool.QueryRow(context.Background(),
		`SELECT id::text FROM users WHERE email = 'admin@example.test'`).Scan(&staffUserID); err != nil {
		t.Fatalf("read admin id: %v", err)
	}
	if _, err := env.pool.Exec(context.Background(), `
		INSERT INTO customer_accounts (user_id, customer_id) VALUES ($1, $2)`,
		staffUserID, customerID); err != nil {
		t.Fatalf("link the staff login to the customer: %v", err)
	}
	// The seeded customer login is detached, so the staff account is the only login on the record
	// and nothing else can keep it alive by accident.
	if _, err := env.pool.Exec(context.Background(),
		`DELETE FROM customer_accounts WHERE customer_id = $1 AND user_id <> $2`,
		customerID, staffUserID); err != nil {
		t.Fatalf("detach the seeded login: %v", err)
	}

	// A second operator closes the record.
	env.createUser(t, "second-admin@example.test", "second admin password 1", "administrator")
	otherCookie, otherCSRF := env.login(t, "second-admin@example.test", "second admin password 1")

	rec := env.doUser(t, http.MethodDelete, "/api/v1/staff/customers/"+customerID,
		otherCookie, otherCSRF, map[string]any{"reason": "billing relationship ended"})
	if rec.Code != http.StatusOK {
		t.Fatalf("delete: status %d, body %s", rec.Code, rec.Body)
	}

	// The record is closed, and the staff login is untouched.
	var (
		status   string
		roleHeld string
	)
	if err := env.pool.QueryRow(context.Background(), `
		SELECT u.status, COALESCE(sr.role, '')
		FROM users u LEFT JOIN staff_roles sr ON sr.user_id = u.id
		WHERE u.id = $1`, staffUserID).Scan(&status, &roleHeld); err != nil {
		t.Fatalf("read the staff login: %v", err)
	}
	if status != "active" {
		t.Fatalf("closing a customer record disabled a staff login: status = %q, role = %q",
			status, roleHeld)
	}

	// And the operator is still signed in and still able to work.
	if me := env.get(t, "/api/v1/auth/me", env.admin); me.Code != http.StatusOK {
		t.Fatalf("closing a customer record ended a staff session: status %d, body %s", me.Code, me.Body)
	}
	if list := env.get(t, "/api/v1/staff/customers", env.admin); list.Code != http.StatusOK {
		t.Fatalf("the operator can no longer read the customer list: status %d", list.Code)
	}
}

// Closing a customer record needs a stated reason, and a role without customer:write:all cannot do
// it at all.
func TestStaffDeleteCustomerRequiresReasonAndPermission(t *testing.T) {
	env := newStaffEnv(t)
	customerID, _ := env.seedCustomer(t, "guarded@example.test", "Guarded")

	// No body at all: the answer is "a reason is required", not "invalid JSON".
	noReason := env.doUser(t, http.MethodDelete, "/api/v1/staff/customers/"+customerID,
		env.admin, env.adminCSRF, nil)
	if noReason.Code != http.StatusBadRequest {
		t.Fatalf("delete with no reason: status %d, want 400; body %s", noReason.Code, noReason.Body)
	}
	if code := problemCode(t, noReason); code != "reason_required" {
		t.Fatalf("code = %q, want reason_required", code)
	}

	// Support can read a customer but not close one.
	denied := env.doUser(t, http.MethodDelete, "/api/v1/staff/customers/"+customerID,
		env.support, env.supportCSRF, map[string]any{"reason": "not my job"})
	if denied.Code != http.StatusForbidden {
		t.Fatalf("support delete: status %d, want 403; body %s", denied.Code, denied.Body)
	}
}

// The overview's account state is what the staff screen renders: the closure facts, the blockers
// and the second-factor state of every login, in one request.
func TestStaffCustomerAccountState(t *testing.T) {
	env := newClosureEnv(t)
	customerID, userID := env.seedCustomer(t, "state@example.test", "State Reader")
	env.seedService(t, customerID, "active")
	cookie, csrf := env.login(t, "state@example.test", "customer password 1")
	env.enrolMFA(t, cookie, csrf)

	rec := env.get(t, "/api/v1/staff/customers/"+customerID+"/account-state", env.admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("account state: status %d, body %s", rec.Code, rec.Body)
	}
	var state struct {
		Closed   bool `json:"closed"`
		CanClose bool `json:"can_close"`
		Blockers []struct {
			Kind string `json:"kind"`
		} `json:"blockers"`
		Logins []struct {
			UserID    string `json:"user_id"`
			HasMFA    bool   `json:"has_mfa"`
			Remaining int    `json:"recovery_codes_remaining"`
		} `json:"logins"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &state); err != nil {
		t.Fatalf("decode state: %v", err)
	}
	if state.Closed || state.CanClose {
		t.Fatalf("state = %+v, want open but not closeable", state)
	}
	if len(state.Blockers) != 1 || state.Blockers[0].Kind != "live_service" {
		t.Fatalf("blockers = %+v, want one live_service", state.Blockers)
	}
	if len(state.Logins) != 1 || state.Logins[0].UserID != userID {
		t.Fatalf("logins = %+v, want the seeded login", state.Logins)
	}
	if !state.Logins[0].HasMFA || state.Logins[0].Remaining != authn.RecoveryCodeCount {
		t.Fatalf("login security = %+v, want a confirmed factor with %d recovery codes",
			state.Logins[0], authn.RecoveryCodeCount)
	}
}
