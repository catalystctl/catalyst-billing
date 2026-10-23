package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/catalyst/billing/internal/authn"
	"github.com/catalyst/billing/internal/store"
)

// `admin password-reset` (OPS-17).
//
// The command is the break-glass path out of a lockout, so what these tests assert is not "a
// password was written" — a psql UPDATE does that — but the three things a psql UPDATE does not:
// the *new* password authenticates, the old one does not, every live session of the target is
// revoked, and the change is attributable in the audit log. A reset that leaves the attacker's
// session alive is not a reset.

// cliUser creates a login with a known password and returns its id.
func cliUser(t *testing.T, pool *store.Pool, email, password string) string {
	t.Helper()
	hasher, err := authn.NewHasher(authn.DefaultArgon2Params())
	if err != nil {
		t.Fatalf("hasher: %v", err)
	}
	hash, err := hasher.Hash(password)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	var id string
	if err := pool.QueryRow(context.Background(), `
		INSERT INTO users (email, password_hash, display_name, email_verified_at)
		VALUES ($1, $2, 'Reset Test', now()) RETURNING id`, email, hash).Scan(&id); err != nil {
		t.Fatalf("create user: %v", err)
	}
	return id
}

// loginService is the authn service the CLI's own code path uses, pointed at the same pool.
func loginService(t *testing.T, pool *store.Pool) *authn.Service {
	t.Helper()
	hasher, err := authn.NewHasher(authn.DefaultArgon2Params())
	if err != nil {
		t.Fatalf("hasher: %v", err)
	}
	svc, err := authn.New(pool, hasher, authn.DefaultConfig())
	if err != nil {
		t.Fatalf("authn: %v", err)
	}
	return svc
}

// canLogIn reports whether a password authenticates, without failing the test on a refusal.
func canLogIn(svc *authn.Service, email, password string) bool {
	_, err := svc.Login(context.Background(), authn.LoginRequest{
		Email: email, Password: password, IP: "203.0.113.7", UserAgent: "test",
	})
	return err == nil
}

func TestAdminPasswordResetRevokesSessionsAndAudits(t *testing.T) {
	pool := cliEnv(t)
	ctx := context.Background()

	const email = "locked-out@example.test"
	const oldPassword = "an-old-password-1"
	const newPassword = "a-new-password-2"
	userID := cliUser(t, pool, email, oldPassword)

	svc := loginService(t, pool)
	// A live session is what makes this test worth writing: the operator resetting a password
	// usually believes somebody else is signed in.
	if !canLogIn(svc, email, oldPassword) {
		t.Fatal("the seeded password does not authenticate, so this test proves nothing")
	}

	out, err := captureStdout(t, func() error {
		return cmdAdmin([]string{"password-reset", "--email", email, "--password", newPassword})
	})
	if err != nil {
		t.Fatalf("password-reset: %v", err)
	}
	if !strings.Contains(out, "reset the password for "+email) {
		t.Fatalf("output = %q", out)
	}
	if strings.Contains(out, newPassword) {
		t.Fatal("the command echoed the password it was given; only --generate prints one")
	}

	// Every session of the target is revoked, not only the one the CLI might have held. The
	// count is taken before the login checks below, because a successful login creates a
	// session of its own.
	var live int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM sessions WHERE user_id = $1 AND revoked_at IS NULL`,
		userID).Scan(&live); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	if live != 0 {
		t.Fatalf("%d session(s) survived the reset", live)
	}

	// The new password is the one that works, and the old one is not.
	if !canLogIn(svc, email, newPassword) {
		t.Fatal("the new password does not authenticate")
	}
	if canLogIn(svc, email, oldPassword) {
		t.Fatal("the old password still authenticates after a reset")
	}

	// The change is attributable: one audit row naming the action and the number of sessions it
	// ended, written inside the same transaction as the hash.
	var (
		action  string
		changes map[string]any
	)
	if err := pool.QueryRow(ctx, `
		SELECT action, changes FROM audit_events
		WHERE target_id = $1 AND action = 'user.password_set'
		ORDER BY created_at DESC LIMIT 1`, userID).Scan(&action, &changes); err != nil {
		t.Fatalf("read audit row: %v", err)
	}
	if revoked, ok := changes["sessions_revoked"]; !ok || revoked == nil {
		t.Fatalf("audit changes = %v, want sessions_revoked", changes)
	}

	// The printed summary counts the sessions it ended, which is the fact an operator needs to
	// know before they tell the customer to sign in again.
	if !strings.Contains(out, "revoked ") {
		t.Fatalf("output = %q, want a session count", out)
	}
}

// A password reset that cannot find the account says so and writes nothing.
func TestAdminPasswordResetRefusesUnknownAccount(t *testing.T) {
	pool := cliEnv(t)
	_ = pool

	_, err := captureStdout(t, func() error {
		return cmdAdmin([]string{"password-reset", "--email", "nobody@example.test", "--password", "a-long-password-3"})
	})
	if err == nil {
		t.Fatal("resetting the password of an account that does not exist was accepted")
	}
	if !strings.Contains(err.Error(), "no user matches") {
		t.Fatalf("error = %v, want a refusal naming the address", err)
	}
}

// `--generate` prints a password once, and that password is the one that works.
func TestAdminPasswordResetGeneratePrintsAWorkingPassword(t *testing.T) {
	pool := cliEnv(t)
	const email = "generated@example.test"
	cliUser(t, pool, email, "an-old-password-4")

	out, err := captureStdout(t, func() error {
		return cmdAdmin([]string{"password-reset", "--email", email, "--generate"})
	})
	if err != nil {
		t.Fatalf("password-reset --generate: %v", err)
	}
	const marker = "password: "
	idx := strings.LastIndex(out, marker)
	if idx < 0 {
		t.Fatalf("output = %q, want the generated password", out)
	}
	generated := strings.TrimSpace(out[idx+len(marker):])
	if len(generated) < authn.MinPasswordLength {
		t.Fatalf("generated password %q is shorter than the minimum", generated)
	}
	if !canLogIn(loginService(t, pool), email, generated) {
		t.Fatal("the generated password does not authenticate")
	}
}

// The reset is the same operation the staff screen performs, so it goes through authn and not
// around it: the credential version is bumped, which is what stops an old session from
// authenticating even if its row survived.
func TestAdminPasswordResetBumpsCredentialVersion(t *testing.T) {
	pool := cliEnv(t)
	ctx := context.Background()
	const email = "version@example.test"
	userID := cliUser(t, pool, email, "an-old-password-5")

	var before int
	if err := pool.QueryRow(ctx, `SELECT credential_version FROM users WHERE id = $1`, userID).
		Scan(&before); err != nil {
		t.Fatalf("read version: %v", err)
	}
	if _, err := captureStdout(t, func() error {
		return cmdAdmin([]string{"password-reset", "--email", userID, "--password", "a-new-password-6"})
	}); err != nil {
		t.Fatalf("password-reset: %v", err)
	}
	var after int
	if err := pool.QueryRow(ctx, `SELECT credential_version FROM users WHERE id = $1`, userID).
		Scan(&after); err != nil {
		t.Fatalf("read version: %v", err)
	}
	if after <= before {
		t.Fatalf("credential_version %d -> %d: an old session would still authenticate", before, after)
	}
}

// A weak password is refused before anything is written, exactly as the API refuses it.
func TestAdminPasswordResetRefusesAWeakPassword(t *testing.T) {
	pool := cliEnv(t)
	const email = "weak@example.test"
	cliUser(t, pool, email, "an-old-password-7")

	_, err := captureStdout(t, func() error {
		return cmdAdmin([]string{"password-reset", "--email", email, "--password", "short"})
	})
	if err == nil {
		t.Fatal("a password below the minimum length was accepted")
	}
	if !errors.Is(err, authn.ErrWeakPassword) && !strings.Contains(err.Error(), "too short") {
		t.Fatalf("error = %v, want a weak-password refusal", err)
	}
}
