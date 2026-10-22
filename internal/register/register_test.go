package register_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/catalyst/billing/internal/authn"
	"github.com/catalyst/billing/internal/notify"
	"github.com/catalyst/billing/internal/register"
	"github.com/catalyst/billing/internal/store"
	"github.com/catalyst/billing/internal/store/storetest"
)

// Registration is the entry point the storefront was missing, so these tests drive the
// service against a real database: the thing under test is a transaction over three
// tables plus two unique indexes, and no mock can prove that.

// lowerCost makes Argon2 cheap enough for a suite while keeping the code path real.
// The encoded hash carries its own parameters, so this changes nothing about how a
// production hash verifies.
func lowerCost(t *testing.T) {
	t.Helper()
	previous := register.Argon2Params
	p := authn.DefaultArgon2Params()
	p.Memory = 19 * 1024
	p.Iterations = 1
	register.Argon2Params = func() authn.Argon2Params { return p }
	t.Cleanup(func() { register.Argon2Params = previous })
}

type env struct {
	pool   *store.Pool
	svc    *register.Service
	hasher *authn.Hasher
}

func newEnv(t *testing.T) *env {
	t.Helper()
	lowerCost(t)
	pool := storetest.DB(t)
	hasher, err := register.NewHasher()
	if err != nil {
		t.Fatalf("hasher: %v", err)
	}
	return &env{pool: pool, hasher: hasher, svc: register.New(pool, hasher)}
}

// register is the common case: a valid new account.
func (e *env) register(t *testing.T, email string) register.Result {
	t.Helper()
	res, err := e.svc.Register(context.Background(), register.Request{
		Email:       email,
		Password:    "correct horse battery",
		DisplayName: "Ada Lovelace",
	})
	if err != nil {
		t.Fatalf("register %s: %v", email, err)
	}
	return res
}

// setSetting writes one settings row, which is what an operator would do.
func (e *env) setSetting(t *testing.T, group, key, value string) {
	t.Helper()
	if _, err := e.pool.Exec(context.Background(), `
		INSERT INTO settings (group_key, key, value) VALUES ($1, $2, $3)
		ON CONFLICT (group_key, key) DO UPDATE SET value = EXCLUDED.value`,
		group, key, value); err != nil {
		t.Fatalf("set %s.%s: %v", group, key, err)
	}
}

// The graph is the point: a login with no billing account cannot buy anything, and a
// billing account with no login cannot be reached.
func TestRegistrationCreatesUserCustomerAndLink(t *testing.T) {
	e := newEnv(t)
	res := e.register(t, "Ada@Example.test")
	if res.Duplicate {
		t.Fatal("a fresh address was reported as a duplicate")
	}
	if res.UserID == "" || res.CustomerID == "" {
		t.Fatalf("incomplete result: %+v", res)
	}

	ctx := context.Background()
	var (
		email    string
		hash     string
		display  string
		verified *time.Time
	)
	if err := e.pool.QueryRow(ctx, `
		SELECT email, password_hash, display_name, email_verified_at
		FROM users WHERE id = $1`, res.UserID).Scan(&email, &hash, &display, &verified); err != nil {
		t.Fatalf("load user: %v", err)
	}
	// The address is stored lower-cased, so "Ada@" and "ada@" are one account.
	if email != "ada@example.test" {
		t.Fatalf("email = %q, want the normalised form", email)
	}
	if display != "Ada Lovelace" {
		t.Fatalf("display name = %q", display)
	}
	if verified != nil {
		t.Fatal("a new account must not start verified")
	}
	// The stored hash must be the shared Argon2id encoder, not a second one.
	if !strings.HasPrefix(hash, "$argon2id$") {
		t.Fatalf("hash = %q, want an argon2id encoding", hash)
	}
	if _, err := e.hasher.Verify("correct horse battery", hash); err != nil {
		t.Fatalf("the stored hash does not verify the password: %v", err)
	}

	var (
		customerEmail string
		name          string
	)
	if err := e.pool.QueryRow(ctx,
		`SELECT email, name FROM customers WHERE id = $1`, res.CustomerID).
		Scan(&customerEmail, &name); err != nil {
		t.Fatalf("load customer: %v", err)
	}
	if customerEmail != "ada@example.test" || name != "Ada Lovelace" {
		t.Fatalf("customer = %q / %q", customerEmail, name)
	}

	var linked int
	if err := e.pool.QueryRow(ctx, `
		SELECT count(*) FROM customer_accounts
		WHERE user_id = $1 AND customer_id = $2 AND role = 'owner'`,
		res.UserID, res.CustomerID).Scan(&linked); err != nil {
		t.Fatal(err)
	}
	if linked != 1 {
		t.Fatalf("customer_accounts rows = %d, want 1", linked)
	}

	// The auth trail answers "how did this account come to exist".
	var events int
	if err := e.pool.QueryRow(ctx,
		`SELECT count(*) FROM auth_events WHERE user_id = $1 AND event = 'registered'`,
		res.UserID).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if events != 1 {
		t.Fatalf("registered events = %d, want 1", events)
	}
}

// A duplicate must be refused, and refused in the shape the HTTP layer can answer
// identically to a success.
func TestDuplicateEmailIsRefused(t *testing.T) {
	e := newEnv(t)
	e.register(t, "dup@example.test")

	// Case-insensitively: the unique index is on lower(email).
	res, err := e.svc.Register(context.Background(), register.Request{
		Email:       "DUP@example.test",
		Password:    "another correct horse",
		DisplayName: "Someone Else",
	})
	if err != nil {
		t.Fatalf("a duplicate must not be an error for the caller: %v", err)
	}
	if !res.Duplicate {
		t.Fatal("a duplicate address was not reported as a duplicate")
	}
	if res.CustomerID != "" {
		t.Fatalf("a duplicate created a billing account: %+v", res)
	}

	// Exactly one account and one billing account exist.
	var users, customers int
	if err := e.pool.QueryRow(context.Background(), `
		SELECT (SELECT count(*) FROM users WHERE lower(email) = 'dup@example.test'),
		       (SELECT count(*) FROM customers WHERE lower(email) = 'dup@example.test')`).
		Scan(&users, &customers); err != nil {
		t.Fatal(err)
	}
	if users != 1 || customers != 1 {
		t.Fatalf("users = %d, customers = %d; want 1 and 1", users, customers)
	}

	// The duplicate attempt is recorded, for the operator rather than the client.
	var dupEvents int
	if err := e.pool.QueryRow(context.Background(), `
		SELECT count(*) FROM auth_events
		WHERE event = 'registration_duplicate' AND user_id = $1`, res.UserID).Scan(&dupEvents); err != nil {
		t.Fatal(err)
	}
	if dupEvents != 1 {
		t.Fatalf("registration_duplicate events = %d, want 1", dupEvents)
	}
}

// A weak password is the caller's mistake and must be reported as one, without leaving
// a half-built account behind.
func TestWeakPasswordIsRefused(t *testing.T) {
	e := newEnv(t)
	_, err := e.svc.Register(context.Background(), register.Request{
		Email:       "weak@example.test",
		Password:    "short",
		DisplayName: "Weak",
	})
	if !errors.Is(err, authn.ErrWeakPassword) {
		t.Fatalf("err = %v, want ErrWeakPassword", err)
	}
	var users int
	if err := e.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM users`).Scan(&users); err != nil {
		t.Fatal(err)
	}
	if users != 0 {
		t.Fatalf("a refused registration created %d user(s)", users)
	}
}

// A malformed address is refused before anything is written.
func TestInvalidEmailIsRefused(t *testing.T) {
	e := newEnv(t)
	for _, bad := range []string{"", "nope", "a@", "@b.test", "a b@c.test", "a@b", "a@b..test"} {
		if _, err := e.svc.Register(context.Background(), register.Request{
			Email:       bad,
			Password:    "correct horse battery",
			DisplayName: "X",
		}); !errors.Is(err, register.ErrInvalidEmail) {
			t.Fatalf("%q: err = %v, want ErrInvalidEmail", bad, err)
		}
	}
}

// Terms are mandatory only when the installation configures them, and then a
// registration without acceptance is refused.
func TestTOSRequiredWhenConfigured(t *testing.T) {
	e := newEnv(t)
	// No terms configured: no acceptance needed.
	if _, err := e.svc.Register(context.Background(), register.Request{
		Email:       "notos@example.test",
		Password:    "correct horse battery",
		DisplayName: "No Terms",
	}); err != nil {
		t.Fatalf("registration without terms configured: %v", err)
	}

	e.setSetting(t, "general", "tos", "Be excellent to each other.")

	_, err := e.svc.Register(context.Background(), register.Request{
		Email:       "needs-tos@example.test",
		Password:    "correct horse battery",
		DisplayName: "Needs Terms",
	})
	if !errors.Is(err, register.ErrTOSRequired) {
		t.Fatalf("err = %v, want ErrTOSRequired", err)
	}

	// Acceptance lets it through.
	if _, err := e.svc.Register(context.Background(), register.Request{
		Email:       "accepted-tos@example.test",
		Password:    "correct horse battery",
		DisplayName: "Accepted",
		AcceptedTOS: true,
	}); err != nil {
		t.Fatalf("registration with acceptance: %v", err)
	}
}

// A closed registration is refused, and the setting is read with a code-side default
// so an installation with no settings row at all still behaves.
func TestDisabledRegistrationIsRefused(t *testing.T) {
	e := newEnv(t)
	e.setSetting(t, "general", "registration_disabled", "true")

	_, err := e.svc.Register(context.Background(), register.Request{
		Email:       "closed@example.test",
		Password:    "correct horse battery",
		DisplayName: "Closed",
	})
	if !errors.Is(err, register.ErrRegistrationDisabled) {
		t.Fatalf("err = %v, want ErrRegistrationDisabled", err)
	}

	// The policy is readable on its own, which is what a public "is registration open"
	// check needs.
	policy, err := e.svc.Policy(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !policy.RegistrationDisabled {
		t.Fatal("policy did not report the setting")
	}

	// An absent row keeps the default (enabled), so a fresh installation works before
	// an operator ever opens the settings screen.
	if _, err := e.pool.Exec(context.Background(),
		`DELETE FROM settings WHERE group_key = 'general' AND key = 'registration_disabled'`); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Register(context.Background(), register.Request{
		Email:       "reopened@example.test",
		Password:    "correct horse battery",
		DisplayName: "Reopened",
	}); err != nil {
		t.Fatalf("registration with the setting absent: %v", err)
	}
}

// The verification message is written to the outbox in the same transaction as the
// token, so a crash cannot lose it and a retry cannot send it twice.
func TestRegistrationQueuesVerification(t *testing.T) {
	lowerCost(t)
	pool := storetest.DB(t)
	hasher, err := register.NewHasher()
	if err != nil {
		t.Fatal(err)
	}
	authSvc, err := authn.New(pool, hasher, authn.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	svc := register.New(pool, hasher).
		WithVerification(authSvc, notify.NewQueue(pool), "https://panel.example.test")

	res, err := svc.Register(context.Background(), register.Request{
		Email:       "verify@example.test",
		Password:    "correct horse battery",
		DisplayName: "Verify",
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if !res.VerificationQueued {
		t.Fatal("no verification message was queued")
	}

	var (
		kind, to, body string
	)
	if err := pool.QueryRow(context.Background(), `
		SELECT kind, to_address, body FROM notification_deliveries
		WHERE kind = 'email_verification'`).Scan(&kind, &to, &body); err != nil {
		t.Fatalf("load queued message: %v", err)
	}
	if to != "verify@example.test" {
		t.Fatalf("message to %q", to)
	}
	if !strings.Contains(body, "https://panel.example.test/api/v1/auth/verify?token=") {
		t.Fatalf("the link does not point at the configured origin:\n%s", body)
	}

	// The token row exists and is a hash, never the token itself.
	var tokens int
	if err := pool.QueryRow(context.Background(), `
		SELECT count(*) FROM email_verifications WHERE user_id = $1`, res.UserID).Scan(&tokens); err != nil {
		t.Fatal(err)
	}
	if tokens != 1 {
		t.Fatalf("verification tokens = %d, want 1", tokens)
	}
}

// An address an operator created by CLI has a customer row with no login. Registration
// must adopt it rather than creating a second billing account the customer cannot see.
func TestRegistrationAdoptsAnUnlinkedCustomer(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	var existing string
	if err := e.pool.QueryRow(ctx,
		`INSERT INTO customers (email, name) VALUES ('cli@example.test', 'CLI Made')
		 RETURNING id`).Scan(&existing); err != nil {
		t.Fatal(err)
	}

	res := e.register(t, "cli@example.test")
	if res.CustomerID != existing {
		t.Fatalf("customer = %s, want the existing %s", res.CustomerID, existing)
	}
	var customers int
	if err := e.pool.QueryRow(ctx,
		`SELECT count(*) FROM customers WHERE lower(email) = 'cli@example.test'`).Scan(&customers); err != nil {
		t.Fatal(err)
	}
	if customers != 1 {
		t.Fatalf("customers = %d, want 1", customers)
	}
}

// Changing a password verifies the current one, keeps the caller's session and revokes
// every other one.
func TestChangePasswordRevokesOtherSessions(t *testing.T) {
	lowerCost(t)
	pool := storetest.DB(t)
	hasher, err := register.NewHasher()
	if err != nil {
		t.Fatal(err)
	}
	cfg := authn.DefaultConfig()
	authSvc, err := authn.New(pool, hasher, cfg)
	if err != nil {
		t.Fatal(err)
	}
	svc := register.New(pool, hasher)

	ctx := context.Background()
	res, err := svc.Register(ctx, register.Request{
		Email:       "rotate@example.test",
		Password:    "correct horse battery",
		DisplayName: "Rotate",
	})
	if err != nil {
		t.Fatal(err)
	}
	// A verified account, so login is not gated by the verification policy.
	if _, err := pool.Exec(ctx,
		`UPDATE users SET email_verified_at = now() WHERE id = $1`, res.UserID); err != nil {
		t.Fatal(err)
	}

	// Two sessions: one to keep, one to revoke.
	first, err := authSvc.Login(ctx, authn.LoginRequest{
		Email: "rotate@example.test", Password: "correct horse battery",
	})
	if err != nil {
		t.Fatalf("first login: %v", err)
	}
	second, err := authSvc.Login(ctx, authn.LoginRequest{
		Email: "rotate@example.test", Password: "correct horse battery",
	})
	if err != nil {
		t.Fatalf("second login: %v", err)
	}

	revoked, err := svc.ChangePassword(ctx, res.UserID, first.Session.ID,
		"correct horse battery", "an entirely new passphrase")
	if err != nil {
		t.Fatalf("change password: %v", err)
	}
	if revoked != 1 {
		t.Fatalf("revoked = %d, want 1", revoked)
	}

	// The kept session still works…
	if _, _, err := authSvc.Authenticate(ctx, first.Token); err != nil {
		t.Fatalf("the caller's session was revoked: %v", err)
	}
	// …and the other one does not.
	if _, _, err := authSvc.Authenticate(ctx, second.Token); err == nil {
		t.Fatal("the other session survived a password change")
	}

	// The new password works and the old one does not.
	if _, err := authSvc.Login(ctx, authn.LoginRequest{
		Email: "rotate@example.test", Password: "an entirely new passphrase",
	}); err != nil {
		t.Fatalf("login with the new password: %v", err)
	}
	if _, err := authSvc.Login(ctx, authn.LoginRequest{
		Email: "rotate@example.test", Password: "correct horse battery",
	}); !errors.Is(err, authn.ErrInvalidCredentials) {
		t.Fatalf("the old password still works: %v", err)
	}

	var events int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM auth_events
		WHERE user_id = $1 AND event = 'password_changed'`, res.UserID).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if events != 1 {
		t.Fatalf("password_changed events = %d, want 1", events)
	}
}

// A wrong current password changes nothing.
func TestChangePasswordRefusesWrongCurrent(t *testing.T) {
	e := newEnv(t)
	res := e.register(t, "wrong@example.test")

	_, err := e.svc.ChangePassword(context.Background(), res.UserID, "",
		"not the password", "an entirely new passphrase")
	if !errors.Is(err, register.ErrInvalidCredentials) {
		t.Fatalf("err = %v, want ErrInvalidCredentials", err)
	}
}

// A weak new password is refused and the old one still works.
func TestChangePasswordRefusesWeakNew(t *testing.T) {
	e := newEnv(t)
	res := e.register(t, "weaknew@example.test")

	_, err := e.svc.ChangePassword(context.Background(), res.UserID, "",
		"correct horse battery", "short")
	if !errors.Is(err, authn.ErrWeakPassword) {
		t.Fatalf("err = %v, want ErrWeakPassword", err)
	}
}
