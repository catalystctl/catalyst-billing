package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/catalyst/billing/internal/authn"
	"github.com/catalyst/billing/internal/httpapi"
	"github.com/catalyst/billing/internal/payments"
	"github.com/catalyst/billing/internal/store"
	"github.com/catalyst/billing/internal/store/storetest"
	"io"
	"log/slog"
)

// proxyEnv is a server with a configured trusted-proxy network.
func newProxyEnv(t *testing.T, trusted ...string) (*authEnv, *store.Pool) {
	t.Helper()
	pool := storetest.DB(t)
	hasher, err := authn.NewHasher(authParams())
	if err != nil {
		t.Fatal(err)
	}
	env := &authEnv{pool: pool, hasher: hasher}

	cfg := authn.DefaultConfig()
	svc, err := authn.New(pool, hasher, cfg)
	if err != nil {
		t.Fatal(err)
	}

	nets := make([]*net.IPNet, 0, len(trusted))
	for _, cidr := range trusted {
		_, n, err := net.ParseCIDR(cidr)
		if err != nil {
			t.Fatalf("bad cidr %q: %v", cidr, err)
		}
		nets = append(nets, n)
	}

	env.srv = httpapi.New(httpapi.Config{
		Version: "test",
		Logger:  quietLogger(),
	}, pool, payments.NewInbox(pool), "stripe", "acct_test", nil).WithAuth(&httpapi.Auth{
		Service:        svc,
		Authz:          authn.NewAuthorizer(),
		Cookies:        httpapi.CookieConfig{Secure: false},
		SessionTTL:     cfg.SessionLifetime,
		TrustedProxies: nets,
	})
	return env, pool
}

// loginFrom performs a login from a given peer address with optional headers.
func (e *authEnv) loginFrom(t *testing.T, email, password, remoteAddr string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"email": email, "password": password})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", bytes.NewReader(body))
	req.RemoteAddr = remoteAddr
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	e.srv.Handler().ServeHTTP(rec, req)
	return rec
}

// H2: with no trusted proxy configured, X-Forwarded-For must be ignored, so a
// client cannot choose its own throttling bucket and spray passwords unthrottled.
func TestForwardedForIgnoredWithoutTrustedProxy(t *testing.T) {
	env, pool := newProxyEnv(t) // no trusted proxies
	env.createUser(t, "victim@example.test", "the real password 1", "")

	// Twenty-five failures, each claiming a different client address.
	for i := 0; i < 25; i++ {
		rec := env.loginFrom(t, "victim@example.test", "wrong", "203.0.113.9:5000",
			map[string]string{"X-Forwarded-For": "198.51.100." + itoa(i)})
		if rec.Code == http.StatusTooManyRequests {
			// Throttling kicked in: the header was correctly ignored.
			var failures int
			if err := pool.QueryRow(context.Background(),
				`SELECT count(*) FROM login_attempts WHERE bucket='address' AND NOT succeeded`).
				Scan(&failures); err != nil {
				t.Fatal(err)
			}
			if failures == 0 {
				t.Fatal("throttled with no recorded failures")
			}
			return
		}
	}
	t.Fatal("25 failures from one peer with spoofed X-Forwarded-For were never throttled")
}

// H2: behind a trusted proxy, the client address from X-Forwarded-For is used, so
// one abusive client cannot lock out everyone else sharing the proxy address.
func TestForwardedForHonouredFromTrustedProxy(t *testing.T) {
	env, _ := newProxyEnv(t, "127.0.0.0/8")
	env.createUser(t, "victim@example.test", "the real password 1", "")
	env.createUser(t, "other@example.test", "another password 1", "")

	// Five failures for the victim account from one client.
	for i := 0; i < 5; i++ {
		env.loginFrom(t, "victim@example.test", "wrong", "127.0.0.1:5000",
			map[string]string{"X-Forwarded-For": "198.51.100.7"})
	}
	// The account is now locked, so even the right password is refused.
	rec := env.loginFrom(t, "victim@example.test", "the real password 1", "127.0.0.1:5000",
		map[string]string{"X-Forwarded-For": "198.51.100.7"})
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("locked account: status %d, want 429", rec.Code)
	}

	// A different client address behind the same proxy is unaffected.
	rec = env.loginFrom(t, "other@example.test", "another password 1", "127.0.0.1:5000",
		map[string]string{"X-Forwarded-For": "198.51.100.8"})
	if rec.Code != http.StatusOK {
		t.Fatalf("unrelated client behind the same proxy: status %d, want 200", rec.Code)
	}
}

// H2: the right-most untrusted address is the client. Taking the left-most entry,
// as the common shortcut does, would let the client choose its own bucket.
func TestForwardedForUsesRightmostUntrusted(t *testing.T) {
	env, _ := newProxyEnv(t, "127.0.0.0/8")
	env.createUser(t, "u@example.test", "a good password 1", "")

	// The client claims to be 1.1.1.1, but the proxy appended the real address.
	rec := env.loginFrom(t, "u@example.test", "wrong", "127.0.0.1:5000",
		map[string]string{"X-Forwarded-For": "1.1.1.1, 198.51.100.7"})
	if rec.Code == http.StatusOK {
		t.Fatal("a wrong password succeeded")
	}

	var subject string
	if err := env.pool.QueryRow(context.Background(), `
		SELECT subject FROM login_attempts
		WHERE bucket = 'address' ORDER BY id DESC LIMIT 1`).Scan(&subject); err != nil {
		t.Fatal(err)
	}
	if subject != "198.51.100.7" {
		t.Fatalf("throttle bucket = %q, want the right-most untrusted address 198.51.100.7", subject)
	}
}

// L6: a non-IP X-Forwarded-For must not turn a correct login into a failure, and
// must not be stored as the audit address.
func TestMalformedForwardedForDoesNotBreakLogin(t *testing.T) {
	env, pool := newProxyEnv(t, "127.0.0.0/8")
	env.createUser(t, "u@example.test", "a good password 1", "")

	rec := env.loginFrom(t, "u@example.test", "a good password 1", "127.0.0.1:5000",
		map[string]string{"X-Forwarded-For": "not-an-ip"})
	if rec.Code != http.StatusOK {
		t.Fatalf("correct login with a malformed header: status %d, want 200; body %s",
			rec.Code, rec.Body)
	}

	var ip *string
	if err := pool.QueryRow(context.Background(), `
		SELECT host(ip) FROM auth_events WHERE event = 'login_succeeded' ORDER BY id DESC LIMIT 1`).
		Scan(&ip); err != nil {
		t.Fatal(err)
	}
	if ip == nil {
		t.Fatal("the audit row has no address at all")
	}
	if *ip == "not-an-ip" {
		t.Fatal("a client-chosen string was stored as the audit address")
	}
}

// M1: X-Forwarded-Host must not be believed from an untrusted peer, or a client can
// choose the value the Origin check compares against.
func TestForwardedHostIgnoredWithoutTrustedProxy(t *testing.T) {
	env, _ := newProxyEnv(t) // no trusted proxies
	env.createUser(t, "u@example.test", "a good password 1", "")

	body, _ := json.Marshal(map[string]string{"email": "u@example.test", "password": "a good password 1"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", bytes.NewReader(body))
	req.Host = "panel.example"
	req.RemoteAddr = "203.0.113.9:5000"
	req.Header.Set("Origin", "https://evil.example")
	req.Header.Set("X-Forwarded-Host", "evil.example")
	rec := httptest.NewRecorder()
	env.srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-origin login with a spoofed X-Forwarded-Host: status %d, want 403", rec.Code)
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == "billing_session" && c.Value != "" {
			t.Fatal("a session was issued to a cross-origin request")
		}
	}
}

// M1: behind a trusted proxy the forwarded host is honoured, because that is how a
// proxy preserves the public host name.
func TestForwardedHostHonouredFromTrustedProxy(t *testing.T) {
	env, _ := newProxyEnv(t, "127.0.0.0/8")
	env.createUser(t, "u@example.test", "a good password 1", "")

	body, _ := json.Marshal(map[string]string{"email": "u@example.test", "password": "a good password 1"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", bytes.NewReader(body))
	req.Host = "internal-proxy:8080"
	req.RemoteAddr = "127.0.0.1:5000"
	req.Header.Set("Origin", "https://panel.example")
	req.Header.Set("X-Forwarded-Host", "panel.example")
	rec := httptest.NewRecorder()
	env.srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("login through a trusted proxy: status %d, want 200; body %s", rec.Code, rec.Body)
	}
}

// The Origin comparison must be exact, not a suffix test: a suffix test accepts
// "https://panel.example.evil.test" for host "evil.test".
func TestOriginComparisonIsExact(t *testing.T) {
	env, _ := newProxyEnv(t, "127.0.0.0/8")
	env.createUser(t, "u@example.test", "a good password 1", "")

	cases := []struct {
		origin string
		host   string
		want   int
	}{
		{"https://panel.example", "panel.example", http.StatusOK},
		{"https://panel.example.evil.test", "panel.example", http.StatusForbidden},
		{"https://evil.test", "panel.example", http.StatusForbidden},
		{"https://panel.example", "panel.example.evil.test", http.StatusForbidden},
	}
	for _, tc := range cases {
		body, _ := json.Marshal(map[string]string{"email": "u@example.test", "password": "a good password 1"})
		req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", bytes.NewReader(body))
		req.Host = "proxy-internal"
		req.RemoteAddr = "127.0.0.1:5000"
		req.Header.Set("Origin", tc.origin)
		req.Header.Set("X-Forwarded-Host", tc.host)
		rec := httptest.NewRecorder()
		env.srv.Handler().ServeHTTP(rec, req)
		if rec.Code != tc.want {
			t.Fatalf("origin %q against host %q: status %d, want %d",
				tc.origin, tc.host, rec.Code, tc.want)
		}
	}
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var digits []byte
	for i > 0 {
		digits = append([]byte{byte('0' + i%10)}, digits...)
		i /= 10
	}
	return string(digits)
}
