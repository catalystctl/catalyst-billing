package httpapi_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/catalyst/billing/internal/authn"
	"github.com/catalyst/billing/internal/portal"
)

// Account lifecycle tests (Gate C1, C2).
//
// The properties under test are the ones that make these endpoints safe to expose to the
// internet: a reset request cannot be used to enumerate accounts, a token is single use and
// expires, and a completed reset revokes every session.

// lifecycleEnv is an auth environment with a portal and a public URL configured.
type lifecycleEnv struct {
	*authEnv
}

func newLifecycleEnv(t *testing.T) *lifecycleEnv {
	t.Helper()
	t.Setenv("BILLING_PUBLIC_URL", "https://panel.example.test")
	env := newAuthEnv(t)
	env.srv.WithPortal(portal.New(env.pool))
	return &lifecycleEnv{authEnv: env}
}

// latestResetToken reads the token digest that was stored, so a test can drive the flow without
// the plaintext ever leaving the service.
//
// The plaintext is deliberately not recoverable from the database: that is the security property.
// To drive the flow the test intercepts it from the outbox body, which is what the customer would
// receive.
func (e *lifecycleEnv) latestResetToken(t *testing.T, email string) string {
	t.Helper()
	var body string
	err := e.pool.QueryRow(context.Background(), `
		SELECT body FROM notification_deliveries
		WHERE kind = 'password_reset' AND to_address = $1
		ORDER BY created_at DESC LIMIT 1`, email).Scan(&body)
	if err != nil {
		t.Fatalf("read reset message: %v", err)
	}
	const marker = "token="
	idx := strings.Index(body, marker)
	if idx < 0 {
		t.Fatalf("no token in the message: %s", body)
	}
	token := body[idx+len(marker):]
	if nl := strings.IndexAny(token, "\r\n"); nl >= 0 {
		token = token[:nl]
	}
	return token
}

// latestVerifyToken reads the verification token from the queued message.
func (e *lifecycleEnv) latestVerifyToken(t *testing.T, email string) string {
	t.Helper()
	var body string
	err := e.pool.QueryRow(context.Background(), `
		SELECT body FROM notification_deliveries
		WHERE kind = 'email_verification' AND to_address = $1
		ORDER BY created_at DESC LIMIT 1`, email).Scan(&body)
	if err != nil {
		t.Fatalf("read verification message: %v", err)
	}
	const marker = "token="
	idx := strings.Index(body, marker)
	if idx < 0 {
		t.Fatalf("no token in the message: %s", body)
	}
	token := body[idx+len(marker):]
	if nl := strings.IndexAny(token, "\r\n"); nl >= 0 {
		token = token[:nl]
	}
	return token
}

// A reset request for a known and an unknown address must be indistinguishable: same status and a
// byte-identical body.
func TestResetRequestDoesNotEnumerate(t *testing.T) {
	env := newLifecycleEnv(t)
	env.createUser(t, "known@example.test", "a good password 1", "")

	known := env.postPublic(t, "/api/v1/auth/password-reset/request",
		map[string]any{"email": "known@example.test"})
	unknown := env.postPublic(t, "/api/v1/auth/password-reset/request",
		map[string]any{"email": "nobody@example.test"})

	if known.Code != http.StatusAccepted || unknown.Code != http.StatusAccepted {
		t.Fatalf("statuses = %d, %d; want 202 for both", known.Code, unknown.Code)
	}
	if known.Body.String() != unknown.Body.String() {
		t.Fatalf("bodies differ:\n known:   %s\n unknown: %s", known.Body, unknown.Body)
	}
	if strings.Contains(known.Body.String(), "token") {
		t.Fatalf("the response contains a token: %s", known.Body)
	}

	// Only the known address produced a message.
	var messages int
	if err := env.pool.QueryRow(context.Background(), `
		SELECT count(*) FROM notification_deliveries
		WHERE kind = 'password_reset' AND to_address = 'nobody@example.test'`).Scan(&messages); err != nil {
		t.Fatal(err)
	}
	if messages != 0 {
		t.Fatalf("an unknown address produced %d message(s)", messages)
	}
}

// A reset token works once, and the second use is refused with the same message as an unknown one.
func TestResetTokenIsSingleUse(t *testing.T) {
	env := newLifecycleEnv(t)
	env.createUser(t, "reset@example.test", "a good password 1", "")

	if rec := env.postPublic(t, "/api/v1/auth/password-reset/request",
		map[string]any{"email": "reset@example.test"}); rec.Code != http.StatusAccepted {
		t.Fatalf("request: %d %s", rec.Code, rec.Body)
	}
	token := env.latestResetToken(t, "reset@example.test")

	first := env.postPublic(t, "/api/v1/auth/password-reset/complete",
		map[string]any{"token": token, "password": "a brand new password 2"})
	if first.Code != http.StatusOK {
		t.Fatalf("complete: %d %s", first.Code, first.Body)
	}

	second := env.postPublic(t, "/api/v1/auth/password-reset/complete",
		map[string]any{"token": token, "password": "another password 3"})
	if second.Code != http.StatusBadRequest {
		t.Fatalf("second use: status %d, want 400; body %s", second.Code, second.Body)
	}

	// An unknown token answers identically: the customer's next step is the same in both cases.
	unknown := env.postPublic(t, "/api/v1/auth/password-reset/complete",
		map[string]any{"token": "not-a-real-token", "password": "another password 3"})
	if unknown.Code != second.Code || unknown.Body.String() != second.Body.String() {
		t.Fatalf("a used token is distinguishable from an unknown one:\n used:    %s\n unknown: %s",
			second.Body, unknown.Body)
	}

	// The new password works and the old one does not.
	env.login(t, "reset@example.test", "a brand new password 2")
	if rec := env.tryLogin(t, "reset@example.test", "a good password 1"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("the old password still works: status %d", rec.Code)
	}
}

// An expired token is refused, and the message is the same one as for an unknown token.
func TestResetTokenExpires(t *testing.T) {
	env := newLifecycleEnv(t)
	env.createUser(t, "expired@example.test", "a good password 1", "")

	if rec := env.postPublic(t, "/api/v1/auth/password-reset/request",
		map[string]any{"email": "expired@example.test"}); rec.Code != http.StatusAccepted {
		t.Fatalf("request: %d", rec.Code)
	}
	token := env.latestResetToken(t, "expired@example.test")

	// Move the expiry into the past. The comparison happens in the database against now(), so this
	// is the same path a genuinely old token takes.
	// The row's CHECK requires expires_at > created_at, so both move: the row must look like a
	// token that was issued and has since expired, not like an invalid row.
	if _, err := env.pool.Exec(context.Background(), `
		UPDATE password_resets
		SET created_at = now() - interval '2 hours',
		    expires_at = now() - interval '1 minute'`); err != nil {
		t.Fatal(err)
	}

	rec := env.postPublic(t, "/api/v1/auth/password-reset/complete",
		map[string]any{"token": token, "password": "a brand new password 2"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expired token: status %d, want 400; body %s", rec.Code, rec.Body)
	}
	if !containsCode(rec.Body.Bytes(), "invalid_token") {
		t.Fatalf("body = %s", rec.Body)
	}
}

// A reset revokes every existing session, which is the reason a customer performs one.
func TestResetRevokesEverySession(t *testing.T) {
	env := newLifecycleEnv(t)
	env.createUser(t, "revoke@example.test", "a good password 1", "")

	// Two sessions, as if the customer were signed in on a laptop and a phone.
	first, _ := env.login(t, "revoke@example.test", "a good password 1")
	second, _ := env.login(t, "revoke@example.test", "a good password 1")
	for i, cookie := range []*http.Cookie{first, second} {
		if rec := env.get(t, "/api/v1/auth/me", cookie); rec.Code != http.StatusOK {
			t.Fatalf("session %d is not valid before the reset: %d", i, rec.Code)
		}
	}

	if rec := env.postPublic(t, "/api/v1/auth/password-reset/request",
		map[string]any{"email": "revoke@example.test"}); rec.Code != http.StatusAccepted {
		t.Fatalf("request: %d", rec.Code)
	}
	token := env.latestResetToken(t, "revoke@example.test")
	if rec := env.postPublic(t, "/api/v1/auth/password-reset/complete",
		map[string]any{"token": token, "password": "a brand new password 2"}); rec.Code != http.StatusOK {
		t.Fatalf("complete: %d %s", rec.Code, rec.Body)
	}

	for i, cookie := range []*http.Cookie{first, second} {
		if rec := env.get(t, "/api/v1/auth/me", cookie); rec.Code != http.StatusUnauthorized {
			t.Fatalf("session %d survived the reset: status %d", i, rec.Code)
		}
	}
}

// A weak password is a 422 with its own code, not a 400: the request was well formed.
func TestResetRejectsWeakPassword(t *testing.T) {
	env := newLifecycleEnv(t)
	env.createUser(t, "weak@example.test", "a good password 1", "")

	if rec := env.postPublic(t, "/api/v1/auth/password-reset/request",
		map[string]any{"email": "weak@example.test"}); rec.Code != http.StatusAccepted {
		t.Fatalf("request: %d", rec.Code)
	}
	token := env.latestResetToken(t, "weak@example.test")

	rec := env.postPublic(t, "/api/v1/auth/password-reset/complete",
		map[string]any{"token": token, "password": "short"})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body %s", rec.Code, rec.Body)
	}
	if !containsCode(rec.Body.Bytes(), "weak_password") {
		t.Fatalf("body = %s", rec.Body)
	}

	// The token was not consumed, so the customer can retry with a better password.
	rec = env.postPublic(t, "/api/v1/auth/password-reset/complete",
		map[string]any{"token": token, "password": "a much better password 3"})
	if rec.Code != http.StatusOK {
		t.Fatalf("retry with a strong password: status %d; body %s", rec.Code, rec.Body)
	}
}

// A malformed address is refused, because there is nothing to look up.
func TestResetRequestRejectsMalformedEmail(t *testing.T) {
	env := newLifecycleEnv(t)
	rec := env.postPublic(t, "/api/v1/auth/password-reset/request",
		map[string]any{"email": "not-an-address"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body %s", rec.Code, rec.Body)
	}
}

// ------------------------------------------------------------ verification --

// Verification works once, marks the address, and does not require a session to complete.
func TestEmailVerificationFlow(t *testing.T) {
	env := newLifecycleEnv(t)
	// createUser verifies the address, so this user is inserted directly to start unverified.
	var userID string
	if err := env.pool.QueryRow(context.Background(), `
		INSERT INTO users (email, password_hash) VALUES ('unverified@example.test', 'x')
		RETURNING id`).Scan(&userID); err != nil {
		t.Fatalf("create user: %v", err)
	}
	// A hash the login path accepts is not needed: this test only drives verification.
	cookie, csrf := env.issueSessionFor(t, userID)

	if rec := env.postJSON(t, "/api/v1/auth/verify/request", cookie, csrf,
		map[string]any{}); rec.Code != http.StatusAccepted {
		t.Fatalf("request: %d %s", rec.Code, rec.Body)
	}
	token := env.latestVerifyToken(t, "unverified@example.test")

	rec := env.get(t, "/api/v1/auth/verify?token="+token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("verify: %d %s", rec.Code, rec.Body)
	}
	var body struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Status != "verified" {
		t.Fatalf("status = %q", body.Status)
	}

	var verified bool
	if err := env.pool.QueryRow(context.Background(),
		`SELECT email_verified_at IS NOT NULL FROM users WHERE id = $1`, userID).Scan(&verified); err != nil {
		t.Fatal(err)
	}
	if !verified {
		t.Fatal("the address was not marked verified")
	}

	// Second use is refused.
	if rec := env.get(t, "/api/v1/auth/verify?token="+token, nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("second use: status %d, want 400", rec.Code)
	}
}

// Verification is not required to sign in: an operator created by CLI must be able to work before
// mail is configured.
func TestUnverifiedUserCanSignIn(t *testing.T) {
	env := newLifecycleEnv(t)
	hash, err := env.hasher.Hash("an unverified password 1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.pool.Exec(context.Background(), `
		INSERT INTO users (email, password_hash) VALUES ('pending@example.test', $1)`, hash); err != nil {
		t.Fatalf("create user: %v", err)
	}

	cookie, _ := env.login(t, "pending@example.test", "an unverified password 1")
	if rec := env.get(t, "/api/v1/auth/me", cookie); rec.Code != http.StatusOK {
		t.Fatalf("an unverified user could not sign in: %d %s", rec.Code, rec.Body)
	}
}

// --------------------------------------------------------------------- MFA --

// mfaEnv configures an encryption key.
func mfaEnv(t *testing.T) *lifecycleEnv {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BILLING_MFA_KEY", hex.EncodeToString(key))
	t.Setenv("BILLING_MFA_KEY_ID", "test-key")
	return newLifecycleEnv(t)
}

// Enrolment, confirmation, the login second step and a recovery code, end to end.
func TestMFAEnrolmentAndLogin(t *testing.T) {
	env := mfaEnv(t)
	env.createUser(t, "mfa@example.test", "a good password 1", "")
	cookie, csrf := env.login(t, "mfa@example.test", "a good password 1")

	// Enrol.
	rec := env.postJSON(t, "/api/v1/auth/mfa/enrol", cookie, csrf, map[string]any{})
	if rec.Code != http.StatusOK {
		t.Fatalf("enrol: %d %s", rec.Code, rec.Body)
	}
	var enrolment struct {
		Secret     string `json:"secret"`
		OtpauthURL string `json:"otpauth_url"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &enrolment); err != nil {
		t.Fatal(err)
	}
	if enrolment.Secret == "" || !strings.HasPrefix(enrolment.OtpauthURL, "otpauth://totp/") {
		t.Fatalf("enrolment = %+v", enrolment)
	}

	// The secret must not be readable from the database in plaintext.
	var stored []byte
	if err := env.pool.QueryRow(context.Background(),
		`SELECT secret_enc FROM mfa_credentials WHERE user_id = (SELECT id FROM users WHERE email = 'mfa@example.test')`).
		Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(stored), enrolment.Secret) {
		t.Fatal("the TOTP secret is stored in plaintext")
	}

	// Confirm with a wrong code first: it must not confirm.
	wrong := env.postJSON(t, "/api/v1/auth/mfa/confirm", cookie, csrf,
		map[string]any{"code": "000000"})
	if wrong.Code != http.StatusUnauthorized {
		t.Fatalf("wrong code: status %d, want 401", wrong.Code)
	}

	code, err := authn.TOTPCodeForTest(enrolment.Secret, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	rec = env.postJSON(t, "/api/v1/auth/mfa/confirm", cookie, csrf, map[string]any{"code": code})
	if rec.Code != http.StatusOK {
		t.Fatalf("confirm: %d %s", rec.Code, rec.Body)
	}
	var confirmed struct {
		RecoveryCodes []string `json:"recovery_codes"`
		ShownOnce     bool     `json:"shown_once"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &confirmed); err != nil {
		t.Fatal(err)
	}
	if len(confirmed.RecoveryCodes) != authn.RecoveryCodeCount {
		t.Fatalf("recovery codes = %d, want %d", len(confirmed.RecoveryCodes), authn.RecoveryCodeCount)
	}
	if !confirmed.ShownOnce {
		t.Fatal("the response does not warn that the codes are shown once")
	}

	// A login now returns a challenge and no session cookie.
	login := env.postPublic(t, "/api/v1/auth/login", map[string]any{
		"email": "mfa@example.test", "password": "a good password 1",
	})
	if login.Code != http.StatusOK {
		t.Fatalf("login: %d %s", login.Code, login.Body)
	}
	if len(login.Result().Cookies()) > 0 {
		t.Fatalf("a session cookie was set before the second factor: %v", login.Result().Cookies())
	}
	var challenge struct {
		MFARequired    bool   `json:"mfa_required"`
		ChallengeToken string `json:"challenge_token"`
	}
	if err := json.Unmarshal(login.Body.Bytes(), &challenge); err != nil {
		t.Fatal(err)
	}
	if !challenge.MFARequired || challenge.ChallengeToken == "" {
		t.Fatalf("login = %+v, want an MFA challenge", challenge)
	}

	// A wrong code does not consume the challenge.
	bad := env.postPublic(t, "/api/v1/auth/mfa/verify", map[string]any{
		"challenge_token": challenge.ChallengeToken, "code": "000000",
	})
	if bad.Code != http.StatusUnauthorized {
		t.Fatalf("wrong code: status %d, want 401", bad.Code)
	}

	// The right code completes the login.
	freshCode, err := authn.TOTPCodeForTest(enrolment.Secret, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	done := env.postPublic(t, "/api/v1/auth/mfa/verify", map[string]any{
		"challenge_token": challenge.ChallengeToken, "code": freshCode,
	})
	if done.Code != http.StatusOK {
		t.Fatalf("verify: %d %s", done.Code, done.Body)
	}
	if len(done.Result().Cookies()) == 0 {
		t.Fatal("no session cookie was set after a successful second factor")
	}

	// A recovery code works, and only once.
	recovery := confirmed.RecoveryCodes[0]
	login = env.postPublic(t, "/api/v1/auth/login", map[string]any{
		"email": "mfa@example.test", "password": "a good password 1",
	})
	if err := json.Unmarshal(login.Body.Bytes(), &challenge); err != nil {
		t.Fatal(err)
	}
	withRecovery := env.postPublic(t, "/api/v1/auth/mfa/verify", map[string]any{
		"challenge_token": challenge.ChallengeToken, "code": recovery,
	})
	if withRecovery.Code != http.StatusOK {
		t.Fatalf("recovery code: %d %s", withRecovery.Code, withRecovery.Body)
	}

	login = env.postPublic(t, "/api/v1/auth/login", map[string]any{
		"email": "mfa@example.test", "password": "a good password 1",
	})
	if err := json.Unmarshal(login.Body.Bytes(), &challenge); err != nil {
		t.Fatal(err)
	}
	reused := env.postPublic(t, "/api/v1/auth/mfa/verify", map[string]any{
		"challenge_token": challenge.ChallengeToken, "code": recovery,
	})
	if reused.Code != http.StatusUnauthorized {
		t.Fatalf("a reused recovery code was accepted: status %d", reused.Code)
	}
}

// A wrong code does not consume the challenge: otherwise someone with the password could lock the
// real user out by submitting garbage.
func TestMFAWrongCodeKeepsChallenge(t *testing.T) {
	env := mfaEnv(t)
	env.createUser(t, "keep@example.test", "a good password 1", "")
	cookie, csrf := env.login(t, "keep@example.test", "a good password 1")

	rec := env.postJSON(t, "/api/v1/auth/mfa/enrol", cookie, csrf, map[string]any{})
	var enrolment struct {
		Secret string `json:"secret"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &enrolment); err != nil {
		t.Fatal(err)
	}
	code, err := authn.TOTPCodeForTest(enrolment.Secret, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if rec := env.postJSON(t, "/api/v1/auth/mfa/confirm", cookie, csrf,
		map[string]any{"code": code}); rec.Code != http.StatusOK {
		t.Fatalf("confirm: %d %s", rec.Code, rec.Body)
	}

	login := env.postPublic(t, "/api/v1/auth/login", map[string]any{
		"email": "keep@example.test", "password": "a good password 1",
	})
	var challenge struct {
		ChallengeToken string `json:"challenge_token"`
	}
	if err := json.Unmarshal(login.Body.Bytes(), &challenge); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 3; i++ {
		if rec := env.postPublic(t, "/api/v1/auth/mfa/verify", map[string]any{
			"challenge_token": challenge.ChallengeToken, "code": "111111",
		}); rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: status %d", i, rec.Code)
		}
	}

	// The challenge still works with the right code.
	fresh, err := authn.TOTPCodeForTest(enrolment.Secret, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if rec := env.postPublic(t, "/api/v1/auth/mfa/verify", map[string]any{
		"challenge_token": challenge.ChallengeToken, "code": fresh,
	}); rec.Code != http.StatusOK {
		t.Fatalf("the challenge was consumed by the failed attempts: %d %s", rec.Code, rec.Body)
	}
}

// Disabling needs both the password and a code: a stolen session alone must not remove the second
// factor.
func TestMFADisableRequiresBothFactors(t *testing.T) {
	env := mfaEnv(t)
	env.createUser(t, "disable@example.test", "a good password 1", "")
	cookie, csrf := env.login(t, "disable@example.test", "a good password 1")

	rec := env.postJSON(t, "/api/v1/auth/mfa/enrol", cookie, csrf, map[string]any{})
	var enrolment struct {
		Secret string `json:"secret"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &enrolment); err != nil {
		t.Fatal(err)
	}
	code, err := authn.TOTPCodeForTest(enrolment.Secret, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if rec := env.postJSON(t, "/api/v1/auth/mfa/confirm", cookie, csrf,
		map[string]any{"code": code}); rec.Code != http.StatusOK {
		t.Fatalf("confirm: %d %s", rec.Code, rec.Body)
	}

	// A wrong password is refused even with a valid code.
	fresh, err := authn.TOTPCodeForTest(enrolment.Secret, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if rec := env.postJSON(t, "/api/v1/auth/mfa/disable", cookie, csrf,
		map[string]any{"password": "not the password", "code": fresh}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong password: status %d, want 401", rec.Code)
	}
	// A wrong code is refused even with the right password.
	if rec := env.postJSON(t, "/api/v1/auth/mfa/disable", cookie, csrf,
		map[string]any{"password": "a good password 1", "code": "000000"}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong code: status %d, want 401", rec.Code)
	}
	// Both together work.
	fresh, err = authn.TOTPCodeForTest(enrolment.Secret, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if rec := env.postJSON(t, "/api/v1/auth/mfa/disable", cookie, csrf,
		map[string]any{"password": "a good password 1", "code": fresh}); rec.Code != http.StatusNoContent {
		t.Fatalf("disable: %d %s", rec.Code, rec.Body)
	}

	// A plain login now succeeds without a challenge.
	login := env.postPublic(t, "/api/v1/auth/login", map[string]any{
		"email": "disable@example.test", "password": "a good password 1",
	})
	var body struct {
		MFARequired bool `json:"mfa_required"`
	}
	if err := json.Unmarshal(login.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.MFARequired {
		t.Fatal("MFA is still required after being disabled")
	}
	if len(login.Result().Cookies()) == 0 {
		t.Fatal("no session cookie after disabling MFA")
	}
}

// The account screens have to be able to say whether a second factor is on. The state is reported
// by `/auth/me` (as `mfa`) and by `/auth/mfa`, both from `authn.MFAStatusFor`, and it tracks the
// credential through its whole life: absent, confirmed, and removed.
//
// The two carriers are compared rather than tested separately: two ways to answer one question is
// how a panel and its enforcement path drift apart, and a test that only checked one of them would
// not notice.
func TestMFAStateIsReported(t *testing.T) {
	env := mfaEnv(t)
	env.createUser(t, "state@example.test", "a good password 1", "")
	cookie, csrf := env.login(t, "state@example.test", "a good password 1")

	type state struct {
		HasMFA                 bool    `json:"has_mfa"`
		EnrolledAt             *string `json:"enrolled_at"`
		RecoveryCodesRemaining int     `json:"recovery_codes_remaining"`
		EnrolmentRequired      bool    `json:"enrolment_required"`
		Available              bool    `json:"available"`
	}
	read := func() state {
		t.Helper()
		var me struct {
			MFA state `json:"mfa"`
		}
		rec := env.get(t, "/api/v1/auth/me", cookie)
		if rec.Code != http.StatusOK {
			t.Fatalf("auth/me: %d %s", rec.Code, rec.Body)
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &me); err != nil {
			t.Fatalf("auth/me body: %v (%s)", err, rec.Body)
		}
		var direct state
		if rec := env.get(t, "/api/v1/auth/mfa", cookie); rec.Code != http.StatusOK {
			t.Fatalf("auth/mfa: %d %s", rec.Code, rec.Body)
		} else if err := json.Unmarshal(rec.Body.Bytes(), &direct); err != nil {
			t.Fatalf("auth/mfa body: %v", err)
		}
		// Compared field by field rather than as structs: `EnrolledAt` is a pointer, and comparing
		// two pointers would compare their addresses instead of the instants they name.
		sameTime := (direct.EnrolledAt == nil) == (me.MFA.EnrolledAt == nil) &&
			(direct.EnrolledAt == nil || *direct.EnrolledAt == *me.MFA.EnrolledAt)
		if direct.HasMFA != me.MFA.HasMFA || !sameTime ||
			direct.RecoveryCodesRemaining != me.MFA.RecoveryCodesRemaining ||
			direct.EnrolmentRequired != me.MFA.EnrolmentRequired ||
			direct.Available != me.MFA.Available {
			t.Fatalf("auth/mfa = %+v disagrees with auth/me.mfa = %+v", direct, me.MFA)
		}
		return me.MFA
	}

	if got := read(); got.HasMFA || got.EnrolledAt != nil || !got.Available {
		t.Fatalf("a fresh account reports %+v, want nothing enrolled and readable", got)
	}

	rec := env.postJSON(t, "/api/v1/auth/mfa/enrol", cookie, csrf, map[string]any{})
	if rec.Code != http.StatusOK {
		t.Fatalf("enrol: %d %s", rec.Code, rec.Body)
	}
	var enrolment struct {
		Secret string `json:"secret"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &enrolment); err != nil {
		t.Fatal(err)
	}
	// An unconfirmed enrolment is not a second factor: it does not challenge a login, so reporting
	// it as one would be a lie a customer relies on.
	if got := read(); got.HasMFA || got.EnrolledAt != nil {
		t.Fatalf("after enrol the state is %+v, want still not enrolled", got)
	}

	code, err := authn.TOTPCodeForTest(enrolment.Secret, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if rec := env.postJSON(t, "/api/v1/auth/mfa/confirm", cookie, csrf,
		map[string]any{"code": code}); rec.Code != http.StatusOK {
		t.Fatalf("confirm: %d %s", rec.Code, rec.Body)
	}
	got := read()
	if !got.HasMFA || got.EnrolledAt == nil {
		t.Fatalf("after confirm the state is %+v, want enrolled with a time", got)
	}
	if got.RecoveryCodesRemaining != authn.RecoveryCodeCount {
		t.Fatalf("recovery codes remaining = %d, want %d", got.RecoveryCodesRemaining, authn.RecoveryCodeCount)
	}

	fresh, err := authn.TOTPCodeForTest(enrolment.Secret, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if rec := env.postJSON(t, "/api/v1/auth/mfa/disable", cookie, csrf,
		map[string]any{"password": "a good password 1", "code": fresh}); rec.Code != http.StatusNoContent {
		t.Fatalf("disable: %d %s", rec.Code, rec.Body)
	}
	if got := read(); got.HasMFA || got.EnrolledAt != nil || got.RecoveryCodesRemaining != 0 {
		t.Fatalf("after disable the state is %+v, want nothing enrolled", got)
	}
}

// The state is the caller's own, and it is not readable through an impersonated session: an
// operator acting as a customer must not be able to inspect that customer's credentials.
func TestMFAStateIsScopedToTheCaller(t *testing.T) {
	env := mfaEnv(t)
	env.createUser(t, "state-scope@example.test", "a good password 1", "")
	cookie, _ := env.login(t, "state-scope@example.test", "a good password 1")

	// A second account with a confirmed factor, so a leak would be visible rather than invisible.
	env.createUser(t, "state-other@example.test", "another good password 1", "")
	otherCookie, otherCSRF := env.login(t, "state-other@example.test", "another good password 1")
	rec := env.postJSON(t, "/api/v1/auth/mfa/enrol", otherCookie, otherCSRF, map[string]any{})
	var enrolment struct {
		Secret string `json:"secret"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &enrolment); err != nil {
		t.Fatal(err)
	}
	code, err := authn.TOTPCodeForTest(enrolment.Secret, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if rec := env.postJSON(t, "/api/v1/auth/mfa/confirm", otherCookie, otherCSRF,
		map[string]any{"code": code}); rec.Code != http.StatusOK {
		t.Fatalf("confirm: %d %s", rec.Code, rec.Body)
	}

	var mine struct {
		HasMFA bool `json:"has_mfa"`
	}
	rec = env.get(t, "/api/v1/auth/mfa", cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &mine); err != nil {
		t.Fatal(err)
	}
	if mine.HasMFA {
		t.Fatal("the endpoint reported somebody else's second factor")
	}
}

// Without a sealing key the state is still readable — the read is a plain SELECT and does not need
// the key — and it reports nothing enrolled. Enrolment itself answers 503, which is the answer the
// screens render; the state does not pretend the installation is broken.
func TestMFAStateIsReadableWithoutAKey(t *testing.T) {
	t.Setenv("BILLING_MFA_KEY", "")
	env := newLifecycleEnv(t)
	env.createUser(t, "nokey-state@example.test", "a good password 1", "")
	cookie, _ := env.login(t, "nokey-state@example.test", "a good password 1")

	rec := env.get(t, "/api/v1/auth/mfa", cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", rec.Code, rec.Body)
	}
	var state struct {
		HasMFA    bool `json:"has_mfa"`
		Available bool `json:"available"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	if state.HasMFA || !state.Available {
		t.Fatalf("state = %+v, want has_mfa=false available=true", state)
	}
}

// Enrolment is refused when no encryption key is configured, rather than storing a plaintext
// secret.
func TestMFARequiresAKey(t *testing.T) {
	t.Setenv("BILLING_MFA_KEY", "")
	env := newLifecycleEnv(t)
	env.createUser(t, "nokey@example.test", "a good password 1", "")
	cookie, csrf := env.login(t, "nokey@example.test", "a good password 1")

	rec := env.postJSON(t, "/api/v1/auth/mfa/enrol", cookie, csrf, map[string]any{})
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body %s", rec.Code, rec.Body)
	}
	var n int
	if err := env.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM mfa_credentials`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("a credential was stored without a key: %d row(s)", n)
	}
}

// ---------------------------------------------------------------- api keys --

// A key is shown once, works as a bearer credential, and can be revoked.
func TestAPIKeyLifecycle(t *testing.T) {
	env := newStaffEnv(t)

	rec := env.postJSON(t, "/api/v1/staff/api-keys", env.admin, env.adminCSRF, map[string]any{
		"name":            "CI reader",
		"scopes":          []string{"invoice:read:all"},
		"expires_in_days": 30,
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	var created struct {
		ID        string   `json:"id"`
		Key       string   `json:"key"`
		Prefix    string   `json:"prefix"`
		Scopes    []string `json:"scopes"`
		ShownOnce bool     `json:"shown_once"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(created.Key, "bk_") || len(created.Key) < 40 {
		t.Fatalf("key = %q", created.Key)
	}
	if !created.ShownOnce {
		t.Fatal("the response does not warn that the key is shown once")
	}

	// The key is not stored: the database has only a digest and a prefix.
	var stored []byte
	if err := env.pool.QueryRow(context.Background(),
		`SELECT key_hash FROM api_keys WHERE id = $1`, created.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(stored), created.Key) {
		t.Fatal("the key is stored in plaintext")
	}

	// The list never contains a key.
	list := env.get(t, "/api/v1/staff/api-keys", env.admin)
	if strings.Contains(list.Body.String(), created.Key) {
		t.Fatalf("the list exposes a key: %s", list.Body)
	}
	if !strings.Contains(list.Body.String(), created.Prefix) {
		t.Fatalf("the list does not show the prefix: %s", list.Body)
	}

	// The key authenticates, and is narrowed by its scopes: it can read invoices and cannot refund.
	bearer := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+created.Key)
		return env.doRequest(t, req)
	}
	if rec := bearer("/api/v1/staff/invoices"); rec.Code != http.StatusOK {
		t.Fatalf("invoice read with the key: %d %s", rec.Code, rec.Body)
	}
	if rec := bearer("/api/v1/staff/payments"); rec.Code != http.StatusForbidden {
		t.Fatalf("a payment read with an invoice-only key: status %d, want 403", rec.Code)
	}

	// Revoked: immediately unusable.
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/staff/api-keys/"+created.ID, nil)
	req.AddCookie(env.admin)
	req.Header.Set("X-CSRF-Token", env.adminCSRF)
	req.Header.Set("Idempotency-Key", "revoke-key-0001")
	if rec := env.doRequest(t, req); rec.Code != http.StatusNoContent {
		t.Fatalf("revoke: %d %s", rec.Code, rec.Body)
	}
	if rec := bearer("/api/v1/staff/invoices"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("a revoked key still works: status %d", rec.Code)
	}
}

// A key cannot be given a scope its owner does not hold: that would be a way to escalate.
func TestAPIKeyCannotExceedOwner(t *testing.T) {
	env := newStaffEnv(t)
	supportCookie, supportCSRF := env.login(t, "support@example.test", "support password 1")

	rec := env.postJSON(t, "/api/v1/staff/api-keys", supportCookie, supportCSRF, map[string]any{
		"name":   "overreach",
		"scopes": []string{"payment:refund"},
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body %s", rec.Code, rec.Body)
	}

	// A scope it does hold is accepted.
	ok := env.postJSON(t, "/api/v1/staff/api-keys", supportCookie, supportCSRF, map[string]any{
		"name":   "support reader",
		"scopes": []string{"invoice:read:all"},
	})
	if ok.Code != http.StatusCreated {
		t.Fatalf("status = %d; body %s", ok.Code, ok.Body)
	}
}

// An expired key is refused.
func TestAPIKeyExpiry(t *testing.T) {
	env := newStaffEnv(t)
	rec := env.postJSON(t, "/api/v1/staff/api-keys", env.admin, env.adminCSRF, map[string]any{
		"name": "short lived",
	})
	var created struct {
		ID  string `json:"id"`
		Key string `json:"key"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if _, err := env.pool.Exec(context.Background(),
		`UPDATE api_keys SET expires_at = now() - interval '1 hour' WHERE id = $1`, created.ID); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/staff/invoices", nil)
	req.Header.Set("Authorization", "Bearer "+created.Key)
	if rec := env.doRequest(t, req); rec.Code != http.StatusUnauthorized {
		t.Fatalf("an expired key was accepted: status %d", rec.Code)
	}
}

// A key belongs to its owner: another user cannot revoke it, and an unknown id is indistinguishable.
func TestAPIKeyOwnership(t *testing.T) {
	env := newStaffEnv(t)
	rec := env.postJSON(t, "/api/v1/staff/api-keys", env.admin, env.adminCSRF,
		map[string]any{"name": "admin only"})
	var created struct {
		ID  string `json:"id"`
		Key string `json:"key"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}

	// Support holds apikey:manage, so the route lets the request through; the ownership check in
	// the service is what refuses it, and it answers 404 so another user's key is indistinguishable
	// from a nonexistent one.
	supportCookie, supportCSRF := env.login(t, "support@example.test", "support password 1")
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/staff/api-keys/"+created.ID, nil)
	req.AddCookie(supportCookie)
	req.Header.Set("X-CSRF-Token", supportCSRF)
	req.Header.Set("Idempotency-Key", "cross-revoke-0001")
	if rec := env.doRequest(t, req); rec.Code != http.StatusNotFound {
		t.Fatalf("cross-owner revoke: status %d, want 404", rec.Code)
	}

	// The key still works.
	req = httptest.NewRequest(http.MethodGet, "/api/v1/staff/invoices", nil)
	req.Header.Set("Authorization", "Bearer "+created.Key)
	if rec := env.doRequest(t, req); rec.Code != http.StatusOK {
		t.Fatalf("the key stopped working: %d", rec.Code)
	}
}

// ------------------------------------------------------------------ helpers --

// postPublic performs an unauthenticated POST with a JSON body.
func (e *authEnv) postPublic(t *testing.T, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(encoded)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.srv.Handler().ServeHTTP(rec, req)
	return rec
}

// tryLogin attempts a login and returns the raw response.
func (e *authEnv) tryLogin(t *testing.T, email, password string) *httptest.ResponseRecorder {
	t.Helper()
	return e.postPublic(t, "/api/v1/auth/login", map[string]any{"email": email, "password": password})
}

// issueSessionFor creates a session row for a user without going through the password check.
//
// It exists so a test can drive an authenticated flow for a user whose password hash is not a real
// Argon2 hash. It writes the same columns the service writes.
func (e *authEnv) issueSessionFor(t *testing.T, userID string) (*http.Cookie, string) {
	t.Helper()
	token := "test-session-" + userID[:8]
	csrf := "test-csrf-" + userID[:8]
	if _, err := e.pool.Exec(context.Background(), `
		INSERT INTO sessions (user_id, token_hash, csrf_secret, credential_version, expires_at)
		SELECT id, $2, $3, credential_version, now() + interval '1 hour'
		FROM users WHERE id = $1`,
		userID, authn.HashToken(token), []byte(csrf)); err != nil {
		t.Fatalf("issue session: %v", err)
	}
	return &http.Cookie{Name: "billing_session", Value: token}, csrf
}
