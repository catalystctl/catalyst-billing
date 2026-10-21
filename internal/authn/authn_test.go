package authn_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/catalyst/billing/internal/authn"
	"github.com/catalyst/billing/internal/store"
	"github.com/catalyst/billing/internal/store/storetest"
)

// testParams are fast enough for a test suite and still inside the accepted range.
func testParams() authn.Argon2Params {
	p := authn.DefaultArgon2Params()
	p.Memory = 19 * 1024 // the documented minimum
	p.Iterations = 1
	return p
}

func newHasher(t *testing.T) *authn.Hasher {
	t.Helper()
	h, err := authn.NewHasher(testParams())
	if err != nil {
		t.Fatalf("hasher: %v", err)
	}
	return h
}

type fixture struct {
	pool   *store.Pool
	svc    *authn.Service
	hasher *authn.Hasher
	now    time.Time
	userID string
	email  string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	pool := storetest.DB(t)
	h := newHasher(t)
	f := &fixture{
		pool:   pool,
		hasher: h,
		now:    time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC),
		email:  "user@example.test",
	}
	cfg := authn.DefaultConfig()
	cfg.Now = func() time.Time { return f.now }
	svc, err := authn.New(pool, h, cfg)
	if err != nil {
		t.Fatalf("authn.New: %v", err)
	}
	f.svc = svc

	hash, err := h.Hash("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(context.Background(), `
		INSERT INTO users (email, password_hash, display_name)
		VALUES ($1, $2, 'Test User') RETURNING id`, f.email, hash).Scan(&f.userID); err != nil {
		t.Fatalf("create user: %v", err)
	}
	return f
}

func (f *fixture) login(t *testing.T, password string) authn.LoginResult {
	t.Helper()
	res, err := f.svc.Login(context.Background(), authn.LoginRequest{
		Email:     f.email,
		Password:  password,
		IP:        "203.0.113.10",
		UserAgent: "test-agent",
	})
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	return res
}

// ---------------------------------------------------------------- passwords --

func TestHashAndVerify(t *testing.T) {
	t.Parallel()
	h := newHasher(t)

	hash, err := h.Hash("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(hash, "$argon2id$") {
		t.Fatalf("hash = %q, want an argon2id encoding", hash)
	}
	if strings.Contains(hash, "correct horse") {
		t.Fatal("the hash contains the password")
	}
	if _, err := h.Verify("correct horse battery staple", hash); err != nil {
		t.Fatalf("verify: %v", err)
	}
	if _, err := h.Verify("wrong password entirely", hash); !errors.Is(err, authn.ErrInvalidCredentials) {
		t.Fatalf("wrong password: got %v, want ErrInvalidCredentials", err)
	}

	// Two hashes of the same password must differ: the salt is random.
	other, err := h.Hash("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if other == hash {
		t.Fatal("two hashes of the same password are identical; the salt is not random")
	}
	if _, err := h.Verify("correct horse battery staple", other); err != nil {
		t.Fatalf("verify second hash: %v", err)
	}
}

func TestPasswordPolicy(t *testing.T) {
	t.Parallel()
	h := newHasher(t)
	if _, err := h.Hash("short"); err == nil {
		t.Fatal("a short password was accepted")
	}
	if _, err := h.Hash(strings.Repeat("x", authn.MaxPasswordLength+1)); err == nil {
		t.Fatal("an oversized password was accepted")
	}
	// A password exactly at the limit is accepted: the bound must be inclusive.
	if _, err := h.Hash(strings.Repeat("x", authn.MaxPasswordLength)); err != nil {
		t.Fatalf("password at the limit was rejected: %v", err)
	}
}

func TestRehashDetection(t *testing.T) {
	t.Parallel()
	weak := testParams()
	weakHash, err := func() (string, error) {
		h, err := authn.NewHasher(weak)
		if err != nil {
			return "", err
		}
		return h.Hash("correct horse battery staple")
	}()
	if err != nil {
		t.Fatal(err)
	}

	strong := weak
	strong.Memory = 32 * 1024
	h, err := authn.NewHasher(strong)
	if err != nil {
		t.Fatal(err)
	}
	if !h.NeedsRehash(weakHash) {
		t.Fatal("a hash with weaker parameters was not flagged for rehashing")
	}

	strongHash, err := h.Hash("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if h.NeedsRehash(strongHash) {
		t.Fatal("a current hash was flagged for rehashing")
	}
}

// A malformed or hostile hash must be rejected, not parsed into absurd work.
func TestMalformedHashesRejected(t *testing.T) {
	t.Parallel()
	h := newHasher(t)
	for _, encoded := range []string{
		"",
		"plaintext",
		"$argon2i$v=19$m=65536,t=3,p=2$c2FsdA$a2V5",
		"$argon2id$v=18$m=65536,t=3,p=2$c2FsdA$a2V5",
		"$argon2id$v=19$m=0,t=3,p=2$c2FsdA$a2V5",
		"$argon2id$v=19$m=65536,t=0,p=2$c2FsdA$a2V5",
		"$argon2id$v=19$m=65536,t=3,p=2$notbase64!!!$a2V5",
		"$argon2id$v=19$m=65536,t=3,p=2$c2FsdA$",
		"$argon2id$v=19$m=999999999,t=3,p=2$c2FsdA$a2V5",
	} {
		if _, err := h.Verify("anything", encoded); err == nil {
			t.Fatalf("malformed hash %q was accepted", encoded)
		}
	}
}

func TestParamsValidation(t *testing.T) {
	t.Parallel()
	bad := []authn.Argon2Params{
		{Memory: 1024, Iterations: 3, Parallelism: 2, SaltLength: 16, KeyLength: 32},
		{Memory: 64 * 1024, Iterations: 0, Parallelism: 2, SaltLength: 16, KeyLength: 32},
		{Memory: 64 * 1024, Iterations: 3, Parallelism: 0, SaltLength: 16, KeyLength: 32},
		{Memory: 64 * 1024, Iterations: 3, Parallelism: 2, SaltLength: 4, KeyLength: 32},
		{Memory: 64 * 1024, Iterations: 3, Parallelism: 2, SaltLength: 16, KeyLength: 8},
	}
	for i, p := range bad {
		if err := p.Validate(); err == nil {
			t.Fatalf("params %d were accepted: %+v", i, p)
		}
		if _, err := authn.NewHasher(p); err == nil {
			t.Fatalf("NewHasher accepted params %d", i)
		}
	}
}

// ------------------------------------------------------------------ sessions --

func TestLoginCreatesUsableSession(t *testing.T) {
	f := newFixture(t)
	res := f.login(t, "correct horse battery staple")

	if res.Token == "" {
		t.Fatal("no token returned")
	}
	if res.Session.CSRFToken == "" {
		t.Fatal("no CSRF token returned")
	}
	if res.User.ID != f.userID || res.User.Email != f.email {
		t.Fatalf("user = %+v, want id %s and email %s", res.User, f.userID, f.email)
	}
	if res.User.IsStaff() {
		t.Fatal("a plain customer reported as staff")
	}

	// The token must not be stored in the clear.
	var stored []byte
	if err := f.pool.QueryRow(context.Background(),
		`SELECT token_hash FROM sessions`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if string(stored) == res.Token {
		t.Fatal("the session token is stored verbatim")
	}
	if len(stored) != 32 {
		t.Fatalf("stored token hash is %d bytes, want 32", len(stored))
	}

	sess, user, err := f.svc.Authenticate(context.Background(), res.Token)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if sess.ID != res.Session.ID || user.ID != f.userID {
		t.Fatalf("authenticated as %+v / %+v", sess, user)
	}
}

func TestWrongPasswordRejected(t *testing.T) {
	f := newFixture(t)
	_, err := f.svc.Login(context.Background(), authn.LoginRequest{
		Email: f.email, Password: "wrong", IP: "203.0.113.10",
	})
	if !errors.Is(err, authn.ErrInvalidCredentials) {
		t.Fatalf("got %v, want ErrInvalidCredentials", err)
	}
}

// An unknown account must fail exactly like a wrong password, so the endpoint
// cannot be used to discover which addresses are registered.
func TestUnknownAccountIndistinguishable(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	_, unknownErr := f.svc.Login(ctx, authn.LoginRequest{
		Email: "nobody@example.test", Password: "correct horse battery staple", IP: "203.0.113.11",
	})
	if !errors.Is(unknownErr, authn.ErrInvalidCredentials) {
		t.Fatalf("unknown account: got %v, want ErrInvalidCredentials", unknownErr)
	}
}

func TestDisabledAccountRejected(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, err := f.pool.Exec(ctx, `UPDATE users SET status = 'disabled' WHERE id = $1`, f.userID); err != nil {
		t.Fatal(err)
	}

	_, err := f.svc.Login(ctx, authn.LoginRequest{
		Email: f.email, Password: "correct horse battery staple", IP: "203.0.113.10",
	})
	if !errors.Is(err, authn.ErrAccountDisabled) {
		t.Fatalf("got %v, want ErrAccountDisabled", err)
	}
}

func TestThrottlingAfterRepeatedFailures(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	// The account limit is 5 failures: five wrong passwords are answered as wrong
	// passwords, and the sixth attempt is refused outright.
	for i := 0; i < 5; i++ {
		_, err := f.svc.Login(ctx, authn.LoginRequest{
			Email: f.email, Password: "wrong", IP: "203.0.113.10",
		})
		if !errors.Is(err, authn.ErrInvalidCredentials) {
			t.Fatalf("attempt %d: got %v, want ErrInvalidCredentials", i, err)
		}
	}
	// The sixth is refused before the password is even checked: the correct
	// password must not get through either.
	_, err := f.svc.Login(ctx, authn.LoginRequest{
		Email: f.email, Password: "correct horse battery staple", IP: "203.0.113.10",
	})
	if !errors.Is(err, authn.ErrTooManyAttempts) {
		t.Fatalf("got %v, want ErrTooManyAttempts", err)
	}

	// After the window passes, the correct password works again.
	f.now = f.now.Add(16 * time.Minute)
	if _, err := f.svc.Login(ctx, authn.LoginRequest{
		Email: f.email, Password: "correct horse battery staple", IP: "203.0.113.10",
	}); err != nil {
		t.Fatalf("after the window: %v", err)
	}
}

// Throttling one account must not lock out a different account from the same
// address, and a successful login clears the account counter.
func TestSuccessfulLoginResetsThrottle(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	for i := 0; i < 4; i++ {
		_, _ = f.svc.Login(ctx, authn.LoginRequest{Email: f.email, Password: "wrong", IP: "203.0.113.10"})
	}
	if _, err := f.svc.Login(ctx, authn.LoginRequest{
		Email: f.email, Password: "correct horse battery staple", IP: "203.0.113.10",
	}); err != nil {
		t.Fatalf("login after four failures: %v", err)
	}
	// The success cleared the account's failure history, so a full budget of
	// failures is available again: the sixth new attempt is the one that is
	// refused, exactly as for a fresh account.
	for i := 0; i < 5; i++ {
		_, err := f.svc.Login(ctx, authn.LoginRequest{
			Email: f.email, Password: "wrong", IP: "203.0.113.10",
		})
		if errors.Is(err, authn.ErrTooManyAttempts) {
			t.Fatalf("throttled after only %d new failures; the success did not reset the counter", i)
		}
		if !errors.Is(err, authn.ErrInvalidCredentials) {
			t.Fatalf("attempt %d: got %v, want ErrInvalidCredentials", i, err)
		}
	}
	if _, err := f.svc.Login(ctx, authn.LoginRequest{
		Email: f.email, Password: "correct horse battery staple", IP: "203.0.113.10",
	}); !errors.Is(err, authn.ErrTooManyAttempts) {
		t.Fatalf("after a fresh budget of failures: got %v, want ErrTooManyAttempts", err)
	}
}

// A session expires at its deadline. The check happens before any sliding
// renewal, so an idle session does not outlive its expiry.
func TestSessionExpiry(t *testing.T) {
	f := newFixture(t)
	res := f.login(t, "correct horse battery staple")

	f.now = res.Session.ExpiresAt
	if _, _, err := f.svc.Authenticate(context.Background(), res.Token); !errors.Is(err, authn.ErrSessionExpired) {
		t.Fatalf("at expiry: got %v, want ErrSessionExpired", err)
	}
}

// A session used inside its lifetime is extended, which is what makes a sliding
// session usable without re-authenticating every 12 hours.
func TestSessionSlidesOnActivity(t *testing.T) {
	f := newFixture(t)
	res := f.login(t, "correct horse battery staple")
	ctx := context.Background()

	// Use it an hour before it would expire.
	f.now = res.Session.ExpiresAt.Add(-time.Hour)
	sess, _, err := f.svc.Authenticate(ctx, res.Token)
	if err != nil {
		t.Fatalf("before expiry: %v", err)
	}
	if !sess.ExpiresAt.After(res.Session.ExpiresAt) {
		t.Fatalf("expiry was not extended: %s -> %s", res.Session.ExpiresAt, sess.ExpiresAt)
	}
}

// The absolute lifetime must cap sliding renewal: a stolen token cannot be kept
// alive forever by using it.
func TestAbsoluteLifetimeCapsRenewal(t *testing.T) {
	f := newFixture(t)
	res := f.login(t, "correct horse battery staple")
	ctx := context.Background()

	// Keep using the session in steps small enough that sliding renewal would keep
	// it alive forever if the absolute cap were not enforced.
	for i := 0; i < 40; i++ {
		f.now = f.now.Add(12 * time.Hour)
		_, _, err := f.svc.Authenticate(ctx, res.Token)
		if err != nil {
			// Expiry is the expected end state, and it must have happened no later
			// than the absolute lifetime.
			if !errors.Is(err, authn.ErrSessionExpired) {
				t.Fatalf("step %d: %v", i, err)
			}
			if f.now.Sub(res.Session.CreatedAt) > authn.DefaultConfig().AbsoluteLifetime+6*time.Hour {
				t.Fatalf("session outlived the absolute cap: created %s, now %s",
					res.Session.CreatedAt, f.now)
			}
			return
		}
	}
	t.Fatal("the session never expired despite the absolute lifetime")
}

func TestRevokeSession(t *testing.T) {
	f := newFixture(t)
	res := f.login(t, "correct horse battery staple")
	ctx := context.Background()

	if err := f.svc.Revoke(ctx, res.Token); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.svc.Authenticate(ctx, res.Token); !errors.Is(err, authn.ErrNoSession) {
		t.Fatalf("revoked session: got %v, want ErrNoSession", err)
	}
}

// A password change must revoke every session issued before it. This is what makes
// "I think I was compromised" recoverable.
func TestPasswordChangeRevokesSessions(t *testing.T) {
	f := newFixture(t)
	first := f.login(t, "correct horse battery staple")
	second := f.login(t, "correct horse battery staple")
	ctx := context.Background()

	if err := f.svc.ChangePassword(ctx, f.userID,
		"correct horse battery staple", "a brand new passphrase"); err != nil {
		t.Fatalf("change password: %v", err)
	}

	for name, token := range map[string]string{"first": first.Token, "second": second.Token} {
		if _, _, err := f.svc.Authenticate(ctx, token); !errors.Is(err, authn.ErrNoSession) {
			t.Fatalf("%s session after password change: got %v, want ErrNoSession", name, err)
		}
	}

	// The new password works and the old one does not.
	if _, err := f.svc.Login(ctx, authn.LoginRequest{
		Email: f.email, Password: "a brand new passphrase", IP: "203.0.113.10",
	}); err != nil {
		t.Fatalf("login with the new password: %v", err)
	}
	if _, err := f.svc.Login(ctx, authn.LoginRequest{
		Email: f.email, Password: "correct horse battery staple", IP: "203.0.113.11",
	}); !errors.Is(err, authn.ErrInvalidCredentials) {
		t.Fatalf("old password still works: %v", err)
	}
}

func TestChangePasswordRequiresCurrent(t *testing.T) {
	f := newFixture(t)
	err := f.svc.ChangePassword(context.Background(), f.userID,
		"not the current password", "a brand new passphrase")
	if !errors.Is(err, authn.ErrInvalidCredentials) {
		t.Fatalf("got %v, want ErrInvalidCredentials", err)
	}
}

// Two concurrent logins must produce two distinct sessions.
func TestConcurrentLogins(t *testing.T) {
	f := newFixture(t)
	const n = 8
	tokens := make([]string, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start
			res, err := f.svc.Login(context.Background(), authn.LoginRequest{
				Email: f.email, Password: "correct horse battery staple", IP: "203.0.113.10",
			})
			if err != nil {
				t.Errorf("login %d: %v", idx, err)
				return
			}
			tokens[idx] = res.Token
		}(i)
	}
	close(start)
	wg.Wait()

	seen := make(map[string]bool, n)
	for i, tok := range tokens {
		if tok == "" {
			t.Fatalf("login %d produced no token", i)
		}
		if seen[tok] {
			t.Fatalf("token collision at %d", i)
		}
		seen[tok] = true
		if _, _, err := f.svc.Authenticate(context.Background(), tok); err != nil {
			t.Fatalf("token %d does not authenticate: %v", i, err)
		}
	}
}

func TestCSRFBoundToSession(t *testing.T) {
	f := newFixture(t)
	a := f.login(t, "correct horse battery staple")
	b := f.login(t, "correct horse battery staple")

	if a.Session.CSRFToken == b.Session.CSRFToken {
		t.Fatal("two sessions share a CSRF secret")
	}
	if err := authn.VerifyCSRF(a.Session, a.Session.CSRFToken); err != nil {
		t.Fatalf("own token rejected: %v", err)
	}
	// A token from another session must not be accepted.
	if err := authn.VerifyCSRF(a.Session, b.Session.CSRFToken); !errors.Is(err, authn.ErrCSRF) {
		t.Fatalf("cross-session token: got %v, want ErrCSRF", err)
	}
	if err := authn.VerifyCSRF(a.Session, ""); !errors.Is(err, authn.ErrCSRF) {
		t.Fatalf("empty token: got %v, want ErrCSRF", err)
	}
}

func TestGarbageTokenRejected(t *testing.T) {
	f := newFixture(t)
	for _, tok := range []string{"", "garbage", strings.Repeat("a", 100)} {
		if _, _, err := f.svc.Authenticate(context.Background(), tok); !errors.Is(err, authn.ErrNoSession) {
			t.Fatalf("token %q: got %v, want ErrNoSession", tok, err)
		}
	}
}

// Auth events must be recorded: an incident review needs to know that someone
// tried and failed.
func TestAuthEventsRecorded(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.login(t, "correct horse battery staple")
	_, _ = f.svc.Login(ctx, authn.LoginRequest{Email: f.email, Password: "wrong", IP: "203.0.113.10"})

	rows, err := f.pool.Query(ctx, `SELECT event FROM auth_events ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var events []string
	for rows.Next() {
		var e string
		if err := rows.Scan(&e); err != nil {
			t.Fatal(err)
		}
		events = append(events, e)
	}
	joined := strings.Join(events, ",")
	if !strings.Contains(joined, "login_succeeded") || !strings.Contains(joined, "login_failed") {
		t.Fatalf("auth events = %v, want both a success and a failure", events)
	}
}

// -------------------------------------------------------------------- authz --

func TestDenyByDefault(t *testing.T) {
	t.Parallel()
	az := authn.NewAuthorizer()

	customer := authn.User{ID: "u1", Status: "active"}
	if !az.Can(customer, authn.PermInvoiceReadSelf) {
		t.Fatal("a customer cannot read their own invoice")
	}
	// A customer must not reach staff capabilities.
	for _, p := range []authn.Permission{
		authn.PermInvoiceReadAll, authn.PermPaymentRefund, authn.PermServiceTerminate,
		authn.PermSettingsWrite, authn.PermStaffManage, authn.PermAuditRead,
	} {
		if az.Can(customer, p) {
			t.Fatalf("a plain customer holds %s", p)
		}
	}

	// Unknown permission: denied, not an error and not an allowance.
	if az.Can(customer, authn.Permission("made:up")) {
		t.Fatal("an undeclared permission was granted")
	}
	// Disabled and anonymous users hold nothing.
	if az.Can(authn.User{ID: "u1", Status: "disabled"}, authn.PermInvoiceReadSelf) {
		t.Fatal("a disabled user was granted a permission")
	}
	if az.Can(authn.User{Status: "active"}, authn.PermInvoiceReadSelf) {
		t.Fatal("a user with no id was granted a permission")
	}
}

func TestRolePermissions(t *testing.T) {
	t.Parallel()
	az := authn.NewAuthorizer()

	billing := authn.User{ID: "b", Status: "active", Roles: []string{"billing"}}
	if !az.Can(billing, authn.PermPaymentRefund) {
		t.Fatal("billing cannot refund")
	}
	// Separation of duties: billing must not be able to destroy infrastructure.
	if az.Can(billing, authn.PermServiceTerminate) {
		t.Fatal("billing can terminate services; separation of duties is broken")
	}

	infra := authn.User{ID: "i", Status: "active", Roles: []string{"infrastructure"}}
	if !az.Can(infra, authn.PermServiceTerminate) {
		t.Fatal("infrastructure cannot terminate")
	}
	if az.Can(infra, authn.PermPaymentRefund) {
		t.Fatal("infrastructure can refund; separation of duties is broken")
	}

	support := authn.User{ID: "s", Status: "active", Roles: []string{"support"}}
	if !az.Can(support, authn.PermCustomerReadAll) {
		t.Fatal("support cannot read customers")
	}
	if az.CanAny(support, authn.PermPaymentRefund, authn.PermServiceTerminate) {
		t.Fatal("support holds a destructive permission")
	}

	admin := authn.User{ID: "a", Status: "active", Roles: []string{"administrator"}}
	for _, p := range authn.AllPermissions() {
		if !az.Can(admin, p) {
			t.Fatalf("administrator lacks %s", p)
		}
	}
}

func TestMultipleRolesUnion(t *testing.T) {
	t.Parallel()
	az := authn.NewAuthorizer()
	both := authn.User{ID: "x", Status: "active", Roles: []string{"billing", "infrastructure"}}
	if !az.CanAll(both, authn.PermPaymentRefund, authn.PermServiceTerminate) {
		t.Fatal("union of two roles did not grant both permissions")
	}
}

// Every declared permission must be reachable, or it is dead policy.
func TestEveryPermissionIsGrantedSomewhere(t *testing.T) {
	t.Parallel()
	az := authn.NewAuthorizer()
	admin := authn.User{ID: "a", Status: "active", Roles: []string{"administrator"}}
	for _, p := range authn.AllPermissions() {
		if !az.Can(admin, p) {
			t.Fatalf("%s is declared but no role grants it", p)
		}
	}
	if len(authn.AllRoles()) != 4 {
		t.Fatalf("%d roles declared, want 4", len(authn.AllRoles()))
	}
	if authn.IsStaffRole("customer") {
		t.Fatal("customer must not be a staff role")
	}
}

func TestRequire(t *testing.T) {
	t.Parallel()
	az := authn.NewAuthorizer()
	customer := authn.User{ID: "u", Status: "active"}
	if err := az.Require(customer, authn.PermInvoiceReadSelf); err != nil {
		t.Fatalf("require own invoice: %v", err)
	}
	err := az.Require(customer, authn.PermPaymentRefund)
	if !errors.Is(err, authn.ErrForbidden) {
		t.Fatalf("got %v, want ErrForbidden", err)
	}
}
