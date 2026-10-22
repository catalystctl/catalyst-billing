package captcha_test

// Tests for the human-challenge verifier (IDN-18).
//
// Two things are being proved here, and neither can be proved without a real database or a
// real HTTP server:
//
//   - the outbound exchange is one implementation for all three providers, and it behaves
//     correctly against a server that says yes, a server that says no, a server that is
//     broken and a server that never answers;
//   - with no provider configured nothing happens at all — no request, no refusal — which is
//     what makes the feature safe to switch off.
//
// The fake siteverify server is an httptest.Server, so the verifier's real HTTP client,
// real form encoding and real JSON decoding are exercised. A stub transport would test the
// mock rather than the code.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/catalyst/billing/internal/captcha"
	"github.com/catalyst/billing/internal/settings"
	"github.com/catalyst/billing/internal/store"
	"github.com/catalyst/billing/internal/store/storetest"
)

// fakeSiteverify is one provider's siteverify endpoint, answering whatever the test sets.
type fakeSiteverify struct {
	server *httptest.Server

	mu       sync.Mutex
	calls    int
	lastForm url.Values
	status   int
	body     string
	delay    time.Duration
}

func newFakeSiteverify(t *testing.T) *fakeSiteverify {
	t.Helper()
	f := &fakeSiteverify{status: http.StatusOK, body: `{"success":true}`}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("siteverify: parse form: %v", err)
		}
		f.mu.Lock()
		f.calls++
		f.lastForm = r.PostForm
		status, body, delay := f.status, f.body, f.delay
		f.mu.Unlock()

		if delay > 0 {
			select {
			case <-time.After(delay):
			case <-r.Context().Done():
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(f.server.Close)
	return f
}

// respond sets the answer for subsequent calls.
func (f *fakeSiteverify) respond(status int, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status, f.body = status, body
}

// stall makes the handler sleep before answering.
func (f *fakeSiteverify) stall(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.delay = d
}

// count is how many verification requests reached the fake.
func (f *fakeSiteverify) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// form is the last form body the fake received.
func (f *fakeSiteverify) form() url.Values {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastForm
}

// captchaSettings writes the 'captcha' group the way the settings screen does, so the
// is_secret flag and the declared choices are the real ones.
func captchaSettings(t *testing.T, pool *store.Pool, values map[string]string) {
	t.Helper()
	settingsStore := settings.New(pool)
	for key, value := range values {
		if _, err := settingsStore.Set(context.Background(), "captcha", key, value, "", nil); err != nil {
			t.Fatalf("set captcha.%s: %v", key, err)
		}
	}
}

// The package's provider list and the settings declaration must be the same list. Two lists
// that can drift are two lists where a provider is offered and not accepted, or accepted and
// not offered.
func TestKnownProvidersMatchSettingsChoices(t *testing.T) {
	def, ok := settings.Lookup("captcha", "provider")
	if !ok {
		t.Fatal("captcha.provider is not declared in internal/settings")
	}
	if len(def.Choices) != len(captcha.KnownProviders) {
		t.Fatalf("settings offers %v, captcha accepts %v", def.Choices, captcha.KnownProviders)
	}
	for i, choice := range def.Choices {
		if string(captcha.KnownProviders[i]) != choice {
			t.Fatalf("choice %d is %q in settings and %q in captcha", i, choice, captcha.KnownProviders[i])
		}
	}
}

// Every provider is one Spec and one code path: the three of them must differ in the
// endpoint, and each Spec must name the fields the provider's own documentation names.
func TestSpecsDescribeEveryProvider(t *testing.T) {
	endpoints := map[string]captcha.Provider{}
	for _, p := range captcha.KnownProviders {
		if p == captcha.ProviderNone {
			if _, ok := captcha.SpecFor(p); ok {
				t.Fatal("provider none must have no siteverify endpoint")
			}
			continue
		}
		spec, ok := captcha.SpecFor(p)
		if !ok {
			t.Fatalf("%s has no spec", p)
		}
		if spec.Provider != p {
			t.Fatalf("%s spec names provider %s", p, spec.Provider)
		}
		if !strings.HasPrefix(spec.Endpoint, "https://") {
			t.Fatalf("%s endpoint %q is not TLS", p, spec.Endpoint)
		}
		if spec.TokenField == "" || spec.SecretField == "" || spec.SuccessField == "" {
			t.Fatalf("%s spec is missing a field name: %+v", p, spec)
		}
		if other, dup := endpoints[spec.Endpoint]; dup {
			t.Fatalf("%s and %s share an endpoint", p, other)
		}
		endpoints[spec.Endpoint] = p
	}
	if len(endpoints) != 3 {
		t.Fatalf("want three distinct endpoints, got %d", len(endpoints))
	}
}

// ParseProvider is how a stored value becomes a provider; an unknown one is an error rather
// than a silent "none".
func TestParseProvider(t *testing.T) {
	cases := []struct {
		in      string
		want    captcha.Provider
		wantErr bool
	}{
		{"", captcha.ProviderNone, false},
		{"none", captcha.ProviderNone, false},
		{" TURNSTILE ", captcha.ProviderTurnstile, false},
		{"recaptcha", captcha.ProviderRecaptcha, false},
		{"hcaptcha", captcha.ProviderHCaptcha, false},
		{"recaptcha-v2", "", true},
		{"cloudflare", "", true},
	}
	for _, c := range cases {
		got, err := captcha.ParseProvider(c.in)
		if c.wantErr {
			if !errors.Is(err, captcha.ErrUnknownProvider) {
				t.Fatalf("ParseProvider(%q) error = %v, want ErrUnknownProvider", c.in, err)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Fatalf("ParseProvider(%q) = %q, %v; want %q", c.in, got, err, c.want)
		}
	}
}

// A good token is accepted by all three providers, and the request carries the token, the
// secret and the visitor's address under the names the Spec declares.
func TestVerifierAcceptsAGoodTokenForEveryProvider(t *testing.T) {
	const secret = "site-secret-value"
	for _, provider := range []captcha.Provider{
		captcha.ProviderTurnstile, captcha.ProviderRecaptcha, captcha.ProviderHCaptcha,
	} {
		t.Run(string(provider), func(t *testing.T) {
			fake := newFakeSiteverify(t)
			fake.respond(http.StatusOK, `{"success":true,"challenge_ts":"2026-01-01T00:00:00Z"}`)

			spec, _ := captcha.SpecFor(provider)
			verifier, err := captcha.NewVerifier(provider, secret)
			if err != nil {
				t.Fatalf("NewVerifier: %v", err)
			}
			if err := verifier.WithEndpoint(fake.server.URL).
				Verify(context.Background(), "a-good-token", "203.0.113.9"); err != nil {
				t.Fatalf("Verify: %v", err)
			}

			if fake.count() != 1 {
				t.Fatalf("the fake was called %d times, want 1", fake.count())
			}
			form := fake.form()
			if got := form.Get(spec.TokenField); got != "a-good-token" {
				t.Fatalf("%s = %q, want the token", spec.TokenField, got)
			}
			if got := form.Get(spec.SecretField); got != secret {
				t.Fatalf("%s = %q, want the secret", spec.SecretField, got)
			}
			if got := form.Get(spec.RemoteIPField); got != "203.0.113.9" {
				t.Fatalf("%s = %q, want the visitor's address", spec.RemoteIPField, got)
			}
		})
	}
}

// A refused challenge is ErrChallengeFailed — a different error from an outage, so the
// operator can tell an attack from a broken provider.
func TestVerifierRefusesABadToken(t *testing.T) {
	fake := newFakeSiteverify(t)
	fake.respond(http.StatusOK, `{"success":false,"error-codes":["invalid-input-response"]}`)

	verifier, err := captcha.NewVerifier(captcha.ProviderTurnstile, "secret")
	if err != nil {
		t.Fatal(err)
	}
	err = verifier.WithEndpoint(fake.server.URL).Verify(context.Background(), "bad-token", "")
	if !errors.Is(err, captcha.ErrChallengeFailed) {
		t.Fatalf("error = %v, want ErrChallengeFailed", err)
	}
	if errors.Is(err, captcha.ErrUnavailable) {
		t.Fatalf("a refused challenge must not read as an outage: %v", err)
	}
	// The provider's own reason code is kept for the operator's log.
	if !strings.Contains(err.Error(), "invalid-input-response") {
		t.Fatalf("the refusal does not name the provider's reason: %v", err)
	}
}

// An empty token never reaches the provider: it is ErrMissingToken, which the HTTP layer
// answers with its own code.
func TestVerifierRequiresAToken(t *testing.T) {
	fake := newFakeSiteverify(t)
	verifier, err := captcha.NewVerifier(captcha.ProviderHCaptcha, "secret")
	if err != nil {
		t.Fatal(err)
	}
	err = verifier.WithEndpoint(fake.server.URL).Verify(context.Background(), "   ", "")
	if !errors.Is(err, captcha.ErrMissingToken) {
		t.Fatalf("error = %v, want ErrMissingToken", err)
	}
	if fake.count() != 0 {
		t.Fatalf("an empty token produced %d outbound call(s)", fake.count())
	}
}

// An outage is ErrUnavailable: a server error, an unreachable host, a provider that never
// answers, and a body that is not a verdict.
func TestVerifierReportsAnOutage(t *testing.T) {
	t.Run("server error", func(t *testing.T) {
		fake := newFakeSiteverify(t)
		fake.respond(http.StatusInternalServerError, `upstream is sad`)
		verifier, _ := captcha.NewVerifier(captcha.ProviderRecaptcha, "secret")
		err := verifier.WithEndpoint(fake.server.URL).Verify(context.Background(), "token", "")
		if !errors.Is(err, captcha.ErrUnavailable) {
			t.Fatalf("error = %v, want ErrUnavailable", err)
		}
		if errors.Is(err, captcha.ErrChallengeFailed) {
			t.Fatalf("an outage must not read as a refused challenge: %v", err)
		}
	})

	t.Run("not json", func(t *testing.T) {
		fake := newFakeSiteverify(t)
		fake.respond(http.StatusOK, `<html>maintenance</html>`)
		verifier, _ := captcha.NewVerifier(captcha.ProviderRecaptcha, "secret")
		err := verifier.WithEndpoint(fake.server.URL).Verify(context.Background(), "token", "")
		if !errors.Is(err, captcha.ErrUnavailable) {
			t.Fatalf("error = %v, want ErrUnavailable", err)
		}
	})

	t.Run("no verdict field", func(t *testing.T) {
		fake := newFakeSiteverify(t)
		fake.respond(http.StatusOK, `{"ok":true}`)
		verifier, _ := captcha.NewVerifier(captcha.ProviderRecaptcha, "secret")
		err := verifier.WithEndpoint(fake.server.URL).Verify(context.Background(), "token", "")
		if !errors.Is(err, captcha.ErrUnavailable) {
			t.Fatalf("error = %v, want ErrUnavailable", err)
		}
	})

	t.Run("unreachable", func(t *testing.T) {
		verifier, _ := captcha.NewVerifier(captcha.ProviderTurnstile, "secret")
		// Port 1 is closed, so the dial fails immediately.
		err := verifier.WithEndpoint("http://127.0.0.1:1/siteverify").
			Verify(context.Background(), "token", "")
		if !errors.Is(err, captcha.ErrUnavailable) {
			t.Fatalf("error = %v, want ErrUnavailable", err)
		}
	})

	// A provider that accepts the connection and then says nothing must not hold the
	// request open: the timeout bounds the whole exchange.
	t.Run("slow provider", func(t *testing.T) {
		fake := newFakeSiteverify(t)
		fake.stall(2 * time.Second)
		verifier, _ := captcha.NewVerifier(captcha.ProviderTurnstile, "secret")
		start := time.Now()
		err := verifier.WithEndpoint(fake.server.URL).
			WithTimeout(50*time.Millisecond).
			Verify(context.Background(), "token", "")
		elapsed := time.Since(start)
		if !errors.Is(err, captcha.ErrUnavailable) {
			t.Fatalf("error = %v, want ErrUnavailable", err)
		}
		if elapsed > time.Second {
			t.Fatalf("the timeout did not bound the call: %s", elapsed)
		}
	})
}

// The response is untrusted: a body larger than the bound is an outage, not an allocation.
func TestVerifierBoundsTheResponseSize(t *testing.T) {
	fake := newFakeSiteverify(t)
	huge := `{"success":true,"pad":"` + strings.Repeat("a", 64<<10) + `"}`
	fake.respond(http.StatusOK, huge)

	verifier, _ := captcha.NewVerifier(captcha.ProviderTurnstile, "secret")
	err := verifier.WithEndpoint(fake.server.URL).Verify(context.Background(), "token", "")
	if !errors.Is(err, captcha.ErrUnavailable) {
		t.Fatalf("error = %v, want ErrUnavailable for an oversized response", err)
	}

	// A smaller bound bites on a smaller body too, so the test is not passing on the
	// default alone.
	small := newFakeSiteverify(t)
	small.respond(http.StatusOK, `{"success":true}`)
	err = verifier.WithEndpoint(small.server.URL).
		WithMaxResponseBytes(4).
		Verify(context.Background(), "token", "")
	if !errors.Is(err, captcha.ErrUnavailable) {
		t.Fatalf("error = %v, want ErrUnavailable when the bound is 4 bytes", err)
	}
}

// A token far beyond any provider's format is refused without an outbound call.
func TestVerifierRefusesAnAbsurdToken(t *testing.T) {
	fake := newFakeSiteverify(t)
	verifier, _ := captcha.NewVerifier(captcha.ProviderTurnstile, "secret")
	err := verifier.WithEndpoint(fake.server.URL).
		Verify(context.Background(), strings.Repeat("x", captcha.MaxTokenLength+1), "")
	if !errors.Is(err, captcha.ErrChallengeFailed) {
		t.Fatalf("error = %v, want ErrChallengeFailed", err)
	}
	if fake.count() != 0 {
		t.Fatalf("an oversized token produced %d outbound call(s)", fake.count())
	}
}

// The secret must not appear in an error, even when the endpoint itself carries it — which
// is what a future GET-based provider, or a proxy that logs the body, would produce.
func TestVerifierNeverPutsTheSecretInAnError(t *testing.T) {
	const secret = "very-secret-value"
	verifier, _ := captcha.NewVerifier(captcha.ProviderTurnstile, secret)

	err := verifier.WithEndpoint("http://127.0.0.1:1/siteverify?secret="+secret).
		Verify(context.Background(), "token", "")
	if err == nil {
		t.Fatal("expected an error from a closed port")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("the error leaks the secret: %v", err)
	}
	if !strings.Contains(err.Error(), settings.RedactedMarker) {
		t.Fatalf("the secret was not replaced with the redaction marker: %v", err)
	}
}

// With no provider configured, nothing is read, nothing is called and nothing is refused.
// This is the property that makes switching the feature off indistinguishable from not
// having it.
func TestNoProviderMakesNoOutboundCall(t *testing.T) {
	pool := storetest.DB(t)
	fake := newFakeSiteverify(t)

	svc := captcha.NewService(pool).WithEndpoint(fake.server.URL)

	cfg, err := svc.Config(context.Background())
	if err != nil {
		t.Fatalf("Config: %v", err)
	}
	if cfg.Enabled() {
		t.Fatalf("a fresh installation reports provider %q", cfg.Provider)
	}
	if public := cfg.Public(); public.Enabled || public.SiteKey != "" || public.Provider != captcha.ProviderNone {
		t.Fatalf("public config for a fresh installation = %+v", public)
	}

	// No token, and a token: both are accepted without a challenge.
	if err := svc.Check(context.Background(), "", "203.0.113.9"); err != nil {
		t.Fatalf("Check with no provider and no token: %v", err)
	}
	if err := svc.Check(context.Background(), "some-token", "203.0.113.9"); err != nil {
		t.Fatalf("Check with no provider and a token: %v", err)
	}
	if fake.count() != 0 {
		t.Fatalf("provider none produced %d outbound call(s)", fake.count())
	}
}

// A configured provider is read from the settings table and used: a good token passes, a bad
// one is a refused challenge, and the secret the fake sees is the stored one.
func TestConfiguredProviderIsEnforcedFromSettings(t *testing.T) {
	pool := storetest.DB(t)
	fake := newFakeSiteverify(t)
	captchaSettings(t, pool, map[string]string{
		"provider":   "turnstile",
		"site_key":   "0x4AAAAAAA-public",
		"secret_key": "0x4AAAAAAA-private",
	})

	svc := captcha.NewService(pool).WithEndpoint(fake.server.URL)

	public, err := svc.Public(context.Background())
	if err != nil {
		t.Fatalf("Public: %v", err)
	}
	if !public.Enabled || public.Provider != captcha.ProviderTurnstile || public.SiteKey != "0x4AAAAAAA-public" {
		t.Fatalf("public config = %+v", public)
	}

	if err := svc.Check(context.Background(), "a-good-token", "203.0.113.9"); err != nil {
		t.Fatalf("a good token was refused: %v", err)
	}
	if got := fake.form().Get("secret"); got != "0x4AAAAAAA-private" {
		t.Fatalf("siteverify was given secret %q, want the stored one", got)
	}

	fake.respond(http.StatusOK, `{"success":false,"error-codes":["timeout-or-duplicate"]}`)
	if err := svc.Check(context.Background(), "a-stale-token", ""); !errors.Is(err, captcha.ErrChallengeFailed) {
		t.Fatalf("a bad token: error = %v, want ErrChallengeFailed", err)
	}
}

// A provider selected without a secret or a site key refuses rather than falling open: an
// operator who asked for a challenge must not get an unprotected form because they did not
// finish filling it in.
func TestHalfConfiguredProviderRefuses(t *testing.T) {
	for _, missing := range []string{"secret_key", "site_key"} {
		t.Run("no "+missing, func(t *testing.T) {
			pool := storetest.DB(t)
			fake := newFakeSiteverify(t)
			values := map[string]string{
				"provider":   "hcaptcha",
				"site_key":   "10000000-ffff-ffff-ffff-000000000001",
				"secret_key": "0x0000000000000000000000000000000000000000",
			}
			delete(values, missing)
			captchaSettings(t, pool, values)

			svc := captcha.NewService(pool).WithEndpoint(fake.server.URL)
			err := svc.Check(context.Background(), "a-token", "")
			if !errors.Is(err, captcha.ErrNotConfigured) {
				t.Fatalf("error = %v, want ErrNotConfigured", err)
			}
			if fake.count() != 0 {
				t.Fatalf("an incomplete configuration produced %d outbound call(s)", fake.count())
			}
		})
	}
}

// A provider name that is not one of the declared choices is an error, not "none": a typo
// must not disable a security control silently.
func TestUnknownProviderIsAnError(t *testing.T) {
	pool := storetest.DB(t)
	// Written with SQL because the settings layer's validation is what usually stops this;
	// the point of the test is the read path behind it.
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO settings (group_key, key, value, is_secret)
		VALUES ('captcha', 'provider', 'cloudflare-turnstile', false)`); err != nil {
		t.Fatalf("insert: %v", err)
	}

	svc := captcha.NewService(pool)
	if _, err := svc.Config(context.Background()); !errors.Is(err, captcha.ErrUnknownProvider) {
		t.Fatalf("Config: error = %v, want ErrUnknownProvider", err)
	}
	if err := svc.Check(context.Background(), "a-token", ""); !errors.Is(err, captcha.ErrUnknownProvider) {
		t.Fatalf("Check: error = %v, want ErrUnknownProvider", err)
	}
}

// The public configuration is a response shape, so it must not be able to carry the secret.
func TestPublicConfigHasNoSecret(t *testing.T) {
	cfg := captcha.Config{
		Provider: captcha.ProviderRecaptcha,
		SiteKey:  "public-site-key",
		Secret:   "the-server-side-secret",
	}
	encoded, err := json.Marshal(cfg.Public())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(encoded), cfg.Secret) {
		t.Fatalf("the public configuration contains the secret: %s", encoded)
	}
	if !strings.Contains(string(encoded), "public-site-key") {
		t.Fatalf("the public configuration lost the site key: %s", encoded)
	}
}
