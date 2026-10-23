package authn_test

import (
	"context"
	"errors"
	"testing"

	"github.com/catalyst/billing/internal/authn"
	"github.com/catalyst/billing/internal/store"
)

// setSetting writes one settings row the way the settings page would.
func setSetting(t *testing.T, pool *store.Pool, group, key, value string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO settings (group_key, key, value) VALUES ($1, $2, $3)
		ON CONFLICT (group_key, key) DO UPDATE SET value = $3`, group, key, value); err != nil {
		t.Fatalf("set %s.%s: %v", group, key, err)
	}
}

// A session signed in while binding is off carries no binding, so a change of address or
// browser is not a refusal. Without this the default would sign people out.
func TestSessionBindingOffRecordsNothingAndRefusesNothing(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	res := f.login(t, "correct horse battery staple")
	if res.Session.BoundMode != "" {
		t.Fatalf("bound mode = %q, want empty when the policy is off", res.Session.BoundMode)
	}
	if err := f.svc.EnforceSessionBinding(ctx, res.Session, "198.51.100.7", "some other browser"); err != nil {
		t.Fatalf("an unbound session was refused: %v", err)
	}
}

// User-agent mode ignores the address and refuses a different browser. The IP half matters:
// a mobile network changes address mid-session, which is why this mode exists.
func TestSessionBindingUserAgent(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	setSetting(t, f.pool, "session", "binding", "user_agent")

	res, err := f.svc.Login(ctx, authn.LoginRequest{
		Email: f.email, Password: "correct horse battery staple",
		IP: "203.0.113.10", UserAgent: "browser-a",
	})
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if res.Session.BoundMode != authn.BindUserAgent {
		t.Fatalf("bound mode = %q, want user_agent", res.Session.BoundMode)
	}
	if res.Session.BoundIP != "" {
		t.Fatalf("bound ip = %q, want empty under user_agent mode", res.Session.BoundIP)
	}
	if res.Session.BoundUserAgent != "browser-a" {
		t.Fatalf("bound agent = %q, want browser-a", res.Session.BoundUserAgent)
	}

	if err := f.svc.EnforceSessionBinding(ctx, res.Session, "198.51.100.7", "browser-a"); err != nil {
		t.Fatalf("a changed address was refused under user_agent mode: %v", err)
	}

	err = f.svc.EnforceSessionBinding(ctx, res.Session, "203.0.113.10", "browser-b")
	if !errors.Is(err, authn.ErrSessionBinding) {
		t.Fatalf("a different browser was allowed: %v", err)
	}
	var mismatch *authn.BindingMismatch
	if !errors.As(err, &mismatch) || mismatch.Field != "user_agent" {
		t.Fatalf("mismatch detail = %#v, want user_agent", err)
	}

	// The refusal is recorded: an investigation needs to find attempted reuse.
	var n int
	if err := f.pool.QueryRow(ctx, `
		SELECT count(*) FROM auth_events WHERE event = 'session_binding_refused'`).Scan(&n); err != nil {
		t.Fatalf("count auth events: %v", err)
	}
	if n == 0 {
		t.Fatal("the refusal wrote no auth event")
	}
}

// IP-and-agent mode compares both, and the recorded values come back from the row on a later
// request rather than from the login result the caller happens to hold.
func TestSessionBindingIPAndUserAgentSurvivesReload(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	setSetting(t, f.pool, "session", "binding", "ip_and_user_agent")

	res, err := f.svc.Login(ctx, authn.LoginRequest{
		Email: f.email, Password: "correct horse battery staple",
		IP: "203.0.113.10", UserAgent: "browser-a",
	})
	if err != nil {
		t.Fatalf("login: %v", err)
	}

	// What a later request sees is what was stored, not what the login returned.
	sess, _, err := f.svc.Authenticate(ctx, res.Token)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if sess.BoundMode != authn.BindIPAndUserAgent {
		t.Fatalf("reloaded bound mode = %q, want ip_and_user_agent", sess.BoundMode)
	}
	if sess.BoundIP != "203.0.113.10" || sess.BoundUserAgent != "browser-a" {
		t.Fatalf("reloaded binding = %q / %q, want the values recorded at sign-in",
			sess.BoundIP, sess.BoundUserAgent)
	}

	if err := f.svc.EnforceSessionBinding(ctx, sess, "203.0.113.10", "browser-a"); err != nil {
		t.Fatalf("the bound context was refused: %v", err)
	}

	err = f.svc.EnforceSessionBinding(ctx, sess, "198.51.100.7", "browser-a")
	var mismatch *authn.BindingMismatch
	if !errors.As(err, &mismatch) || mismatch.Field != "ip" {
		t.Fatalf("a changed address was not refused as an ip mismatch: %v", err)
	}

	err = f.svc.EnforceSessionBinding(ctx, sess, "203.0.113.10", "browser-b")
	if !errors.As(err, &mismatch) || mismatch.Field != "user_agent" {
		t.Fatalf("a changed browser was not refused as an agent mismatch: %v", err)
	}
}

// The rule a session was issued under is the rule it keeps: turning the setting off does not
// retroactively free sessions that were bound, and turning it on does not retroactively bind
// sessions that were not. That is what makes the control predictable.
func TestSessionBindingIsDecidedAtSignIn(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	setSetting(t, f.pool, "session", "binding", "user_agent")

	res, err := f.svc.Login(ctx, authn.LoginRequest{
		Email: f.email, Password: "correct horse battery staple",
		IP: "203.0.113.10", UserAgent: "browser-a",
	})
	if err != nil {
		t.Fatalf("login: %v", err)
	}

	setSetting(t, f.pool, "session", "binding", "off")
	if err := f.svc.EnforceSessionBinding(ctx, res.Session, "", "browser-b"); !errors.Is(err, authn.ErrSessionBinding) {
		t.Fatalf("turning the setting off retroactively unbound an open session: %v", err)
	}

	// A session created after the change is unbound, and the next login is what applies the
	// new policy.
	second, err := f.svc.Login(ctx, authn.LoginRequest{
		Email: f.email, Password: "correct horse battery staple",
		IP: "203.0.113.10", UserAgent: "browser-a",
	})
	if err != nil {
		t.Fatalf("second login: %v", err)
	}
	if second.Session.BoundMode != "" {
		t.Fatalf("a session signed in with the policy off is bound to %q", second.Session.BoundMode)
	}
}

// The scope narrows the policy to one class of account: the "admin / client / both" half of
// the parity row. A staff-only rule must not bind a customer session, and a customer-only
// rule must not bind a staff one.
func TestSessionBindingScope(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	// The fixture's user is a customer. Under a staff-only rule their session is unbound.
	setSetting(t, f.pool, "session", "binding", "user_agent")
	setSetting(t, f.pool, "session", "binding_scope", "staff")
	res, err := f.svc.Login(ctx, authn.LoginRequest{
		Email: f.email, Password: "correct horse battery staple",
		IP: "203.0.113.10", UserAgent: "browser-a",
	})
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if res.Session.BoundMode != "" {
		t.Fatalf("a customer session was bound by a staff-only policy: %q", res.Session.BoundMode)
	}

	// A staff account under the same rule is bound.
	if _, err := f.pool.Exec(ctx, `INSERT INTO staff_roles (user_id, role) VALUES ($1, 'support')`,
		f.userID); err != nil {
		t.Fatalf("grant role: %v", err)
	}
	res, err = f.svc.Login(ctx, authn.LoginRequest{
		Email: f.email, Password: "correct horse battery staple",
		IP: "203.0.113.10", UserAgent: "browser-a",
	})
	if err != nil {
		t.Fatalf("staff login: %v", err)
	}
	if res.Session.BoundMode != authn.BindUserAgent {
		t.Fatalf("a staff session was not bound by a staff-only policy: %q", res.Session.BoundMode)
	}
}
