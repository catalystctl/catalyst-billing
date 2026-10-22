// Package captcha verifies a human challenge from Cloudflare Turnstile, Google reCAPTCHA
// or hCaptcha (IDN-18).
//
// # One verifier, three providers
//
// The three services are one protocol with three names: POST a form carrying the token and
// the site's secret to a siteverify URL, receive `{"success": bool, "error-codes": [...]}`.
// They differ in the URL and — today only in principle — in the names of the fields. So
// there is exactly ONE HTTP verifier here, parameterised by a Spec (endpoint plus field
// names), and three Specs. Three near-copies would be three places for the timeout, the
// size bound and the error mapping to drift apart, and the copy that was forgotten is the
// one that would be discovered in production. Adding a fourth provider is a Spec, not a
// file.
//
// # The outbound call is untrusted
//
// A captcha provider is a third party on the critical path of registration and password
// reset, so the call is treated like any other untrusted dependency:
//
//   - the exchange is bounded in time (DefaultTimeout), because a provider that stops
//     answering must not hold a registration request open until the server's write
//     timeout;
//   - the response is bounded in bytes before it is decoded, because "the provider sent a
//     large body" is not a reason to allocate it;
//   - a failed challenge and an unreachable provider are DIFFERENT errors
//     (ErrChallengeFailed vs ErrUnavailable), because an operator needs to tell an attack
//     from an outage, and because the right response differs: one is "try the challenge
//     again", the other is "come back later";
//   - the secret never appears in an error, a log line or a response. It travels in the
//     form body only, and the errors below describe the failure without the request.
//
// # provider=none is not a mode
//
// When `captcha.provider` is `none` — the default — nothing happens: no widget is offered,
// no outbound request is made, and no request is refused that would have been accepted
// before this package existed. That is deliberate: a security feature that changes
// behaviour when it is switched off is a security feature nobody can roll back.
package captcha

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/catalyst/billing/internal/settings"
	"github.com/catalyst/billing/internal/store"
)

// Provider is the challenge service an installation uses.
type Provider string

const (
	// ProviderNone is the default. It means "no challenge", not "no captcha package".
	ProviderNone Provider = "none"
	// ProviderTurnstile is Cloudflare Turnstile.
	ProviderTurnstile Provider = "turnstile"
	// ProviderRecaptcha is Google reCAPTCHA (v2 checkbox or v3).
	ProviderRecaptcha Provider = "recaptcha"
	// ProviderHCaptcha is hCaptcha.
	ProviderHCaptcha Provider = "hcaptcha"
)

// KnownProviders is every provider the settings screen may offer, in the order it should
// offer them. A test asserts it agrees with the declared choices of `captcha.provider`, so
// the two lists cannot drift.
var KnownProviders = []Provider{ProviderNone, ProviderTurnstile, ProviderRecaptcha, ProviderHCaptcha}

// Errors a caller branches on. They are the difference between "you did not solve the
// challenge", "we could not ask", and "you did not send anything to check".
var (
	// ErrChallengeFailed is the provider answering success=false: the token was wrong,
	// expired, already used, or belongs to another site. It is the caller's fault and
	// retrying the challenge is the remedy.
	ErrChallengeFailed = errors.New("captcha: the challenge was not solved")
	// ErrUnavailable is the provider not answering, answering too slowly, answering
	// with a server error, or answering something that is not a verdict. It is an
	// outage, not an attack, and the remedy is to try again later.
	ErrUnavailable = errors.New("captcha: the verification service is unavailable")
	// ErrMissingToken is a request that carried no challenge response at all while a
	// provider is configured. It is distinct from ErrChallengeFailed so the client can
	// tell "you skipped the widget" from "the widget said no".
	ErrMissingToken = errors.New("captcha: no challenge token was supplied")
	// ErrNotConfigured is a provider selected without the site key or the secret that
	// make it usable. It refuses rather than falling open: an operator who asked for a
	// challenge and did not finish configuring it gets an error, not an unprotected
	// registration form.
	ErrNotConfigured = errors.New("captcha: the provider is not fully configured")
	// ErrUnknownProvider is a stored provider name that is not one of KnownProviders. It
	// is an error rather than "none" for the same reason: a typo must not disable a
	// security control silently.
	ErrUnknownProvider = errors.New("captcha: unknown provider")
)

// Endpoints are the published siteverify URLs.
//
// They are constants rather than settings: they are part of each provider's contract, and
// a deployment that could point them elsewhere could be pointed at an attacker's endpoint
// that answers `{"success": true}`.
const (
	endpointTurnstile = "https://challenges.cloudflare.com/turnstile/v0/siteverify"
	endpointRecaptcha = "https://www.google.com/recaptcha/api/siteverify"
	endpointHCaptcha  = "https://api.hcaptcha.com/siteverify"
)

// Bounds on the outbound exchange.
const (
	// DefaultTimeout bounds one verification, from dial to the last byte of the body. It
	// is well under the HTTP server's write timeout: a slow provider delays a signup by
	// three seconds at worst, and then reports an outage.
	DefaultTimeout = 3 * time.Second
	// MaxResponseBytes bounds the siteverify body. The real response is a few hundred
	// bytes; sixteen kibibytes is generous and still not an allocation an attacker
	// chooses.
	MaxResponseBytes = 16 << 10
	// MaxTokenLength bounds the token the client sends. A reCAPTCHA token is around 2 KiB;
	// anything far beyond that is not a token.
	MaxTokenLength = 4096
)

// Spec is the wire contract of one provider's siteverify endpoint: where to POST, what to
// call the fields, and what the verdict field is called.
//
// It exists so the verifier below can be one implementation. The three providers use the
// same names today; that is precisely why the names are parameters — the day one of them
// renames a field, one Spec changes and the code does not.
type Spec struct {
	Provider Provider
	// Endpoint is the absolute siteverify URL.
	Endpoint string
	// TokenField carries the challenge response from the browser.
	TokenField string
	// SecretField carries the site's secret. It is the only secret in the exchange.
	SecretField string
	// RemoteIPField optionally carries the visitor's address. An empty name omits it.
	RemoteIPField string
	// SuccessField is the boolean verdict.
	SuccessField string
	// ErrorCodesField is the array of machine-readable reasons, used for the operator's
	// log and never for the client's response.
	ErrorCodesField string
}

// specs is the whole of the per-provider knowledge in this package.
var specs = map[Provider]Spec{
	ProviderTurnstile: {
		Provider: ProviderTurnstile,
		Endpoint: endpointTurnstile,
		// https://developers.cloudflare.com/turnstile/get-started/server-side-validation/
		TokenField: "response", SecretField: "secret", RemoteIPField: "remoteip",
		SuccessField: "success", ErrorCodesField: "error-codes",
	},
	ProviderRecaptcha: {
		Provider: ProviderRecaptcha,
		Endpoint: endpointRecaptcha,
		// https://developers.google.com/recaptcha/docs/verify
		TokenField: "response", SecretField: "secret", RemoteIPField: "remoteip",
		SuccessField: "success", ErrorCodesField: "error-codes",
	},
	ProviderHCaptcha: {
		Provider: ProviderHCaptcha,
		Endpoint: endpointHCaptcha,
		// https://docs.hcaptcha.com/#verify-the-user-response-server-side
		TokenField: "response", SecretField: "secret", RemoteIPField: "remoteip",
		SuccessField: "success", ErrorCodesField: "error-codes",
	},
}

// SpecFor returns the wire contract of a provider.
func SpecFor(p Provider) (Spec, bool) {
	spec, ok := specs[p]
	return spec, ok
}

// ParseProvider normalises a stored provider name. The empty string is `none`, because a
// fresh installation has no row and its default is "none".
func ParseProvider(value string) (Provider, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", string(ProviderNone):
		return ProviderNone, nil
	case string(ProviderTurnstile):
		return ProviderTurnstile, nil
	case string(ProviderRecaptcha):
		return ProviderRecaptcha, nil
	case string(ProviderHCaptcha):
		return ProviderHCaptcha, nil
	default:
		return "", fmt.Errorf("%w: %q", ErrUnknownProvider, value)
	}
}

// Verifier is the one HTTP verifier. A provider is a Spec; this is the code that speaks it.
type Verifier struct {
	spec     Spec
	secret   string
	client   *http.Client
	timeout  time.Duration
	maxBytes int64
}

// NewVerifier builds the verifier for a provider and its secret.
func NewVerifier(provider Provider, secret string) (*Verifier, error) {
	if provider == ProviderNone || provider == "" {
		return nil, fmt.Errorf("%w: provider is %q", ErrNotConfigured, provider)
	}
	spec, ok := SpecFor(provider)
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownProvider, provider)
	}
	if strings.TrimSpace(secret) == "" {
		return nil, fmt.Errorf("%w: no secret for %s", ErrNotConfigured, provider)
	}
	return &Verifier{
		spec:     spec,
		secret:   secret,
		client:   &http.Client{},
		timeout:  DefaultTimeout,
		maxBytes: MaxResponseBytes,
	}, nil
}

// WithEndpoint points the verifier at a different siteverify URL.
//
// It exists so a test can use a fake siteverify server instead of calling Cloudflare,
// Google or hCaptcha; a deployment never calls it, because the endpoint is part of the
// provider's contract and a movable one is a verifiable-by-nobody one.
func (v *Verifier) WithEndpoint(endpoint string) *Verifier {
	if endpoint != "" {
		v.spec.Endpoint = endpoint
	}
	return v
}

// WithHTTPClient replaces the HTTP client, for a test that wants to observe or stall the
// call.
func (v *Verifier) WithHTTPClient(c *http.Client) *Verifier {
	if c != nil {
		v.client = c
	}
	return v
}

// WithTimeout replaces the bound on the exchange.
func (v *Verifier) WithTimeout(d time.Duration) *Verifier {
	if d > 0 {
		v.timeout = d
	}
	return v
}

// WithMaxResponseBytes replaces the bound on the response body.
func (v *Verifier) WithMaxResponseBytes(n int64) *Verifier {
	if n > 0 {
		v.maxBytes = n
	}
	return v
}

// Endpoint reports the siteverify URL in use. It is the endpoint only: the secret is not
// part of it and is never returned by anything here.
func (v *Verifier) Endpoint() string { return v.spec.Endpoint }

// Verify exchanges a token for a verdict.
//
// The returned error is ErrMissingToken, ErrChallengeFailed or ErrUnavailable, wrapped with
// a short description that never includes the secret, the token or the response body.
func (v *Verifier) Verify(ctx context.Context, token, remoteIP string) error {
	token = strings.TrimSpace(token)
	if token == "" {
		return ErrMissingToken
	}
	if len(token) > MaxTokenLength {
		// Not a token: refuse it as a failed challenge rather than send it on.
		return fmt.Errorf("%w: the token is longer than %d bytes", ErrChallengeFailed, MaxTokenLength)
	}

	form := url.Values{}
	form.Set(v.spec.TokenField, token)
	form.Set(v.spec.SecretField, v.secret)
	if v.spec.RemoteIPField != "" && remoteIP != "" {
		form.Set(v.spec.RemoteIPField, remoteIP)
	}

	// The bound covers the whole exchange: connect, write, wait, read. A provider that
	// accepts the connection and then says nothing is the case this exists for.
	ctx, cancel := context.WithTimeout(ctx, v.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, v.spec.Endpoint,
		strings.NewReader(form.Encode()))
	if err != nil {
		// The endpoint is a constant, so this is a build-time mistake, not an outage.
		return fmt.Errorf("%w: build request: %v", ErrUnavailable, err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := v.client.Do(req)
	if err != nil {
		// No verdict was received: DNS, TLS, connection refused, timeout, a cancelled
		// request. That is an outage, and it must not read as "the visitor failed the
		// challenge".
		return fmt.Errorf("%w: %v", ErrUnavailable, redact(err, v.secret))
	}
	defer func() {
		// Drain a little so the connection can be reused, then close. The body has
		// already been read through a limiter below.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: siteverify answered %d", ErrUnavailable, resp.StatusCode)
	}

	// The body is untrusted and of unknown size. Read at most one byte more than the
	// bound so an oversized response is detected rather than truncated into something
	// that might decode.
	body, err := io.ReadAll(io.LimitReader(resp.Body, v.maxBytes+1))
	if err != nil {
		return fmt.Errorf("%w: read response: %v", ErrUnavailable, redact(err, v.secret))
	}
	if int64(len(body)) > v.maxBytes {
		return fmt.Errorf("%w: the response exceeded %d bytes", ErrUnavailable, v.maxBytes)
	}

	// Decoded through a map because the verdict's field name is part of the Spec, not
	// something this file is allowed to assume.
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		return fmt.Errorf("%w: the response is not JSON", ErrUnavailable)
	}
	raw, ok := payload[v.spec.SuccessField]
	if !ok {
		return fmt.Errorf("%w: the response has no %q field", ErrUnavailable, v.spec.SuccessField)
	}
	var success bool
	if err := json.Unmarshal(raw, &success); err != nil {
		return fmt.Errorf("%w: %q is not a boolean", ErrUnavailable, v.spec.SuccessField)
	}
	if success {
		return nil
	}

	// A refusal: report the provider's own codes to the caller's log. They are the
	// provider's vocabulary ("invalid-input-response", "timeout-or-duplicate"), not the
	// visitor's, and they say nothing about whether an account exists.
	return fmt.Errorf("%w%s", ErrChallengeFailed, errorCodes(payload[v.spec.ErrorCodesField]))
}

// errorCodes renders the provider's reason codes for a log line, e.g. " (invalid-input-response)".
func errorCodes(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var codes []string
	if err := json.Unmarshal(raw, &codes); err != nil || len(codes) == 0 {
		return ""
	}
	for i, code := range codes {
		// The codes are provider-controlled strings; keep them short and printable so a
		// hostile provider cannot inject newlines into a log line.
		clean := make([]rune, 0, len(code))
		for _, r := range code {
			if r < 0x20 || r > 0x7e {
				continue
			}
			clean = append(clean, r)
		}
		codes[i] = string(clean)
	}
	return " (" + strings.Join(codes, ",") + ")"
}

// redact removes the secret from an error message.
//
// A transport error carries the URL, and the URL of a siteverify endpoint does not contain
// the secret — but this is the one place where a future change (a GET-based provider, a
// proxy that logs the body) could put it in a message that ends up in a log, so the check
// is explicit rather than assumed.
func redact(err error, secret string) error {
	if err == nil || secret == "" {
		return err
	}
	if msg := err.Error(); strings.Contains(msg, secret) {
		return errors.New(strings.ReplaceAll(msg, secret, settings.RedactedMarker))
	}
	return err
}

// Config is the resolved captcha configuration, secret included.
//
// It is never serialised: the HTTP surface returns Public(), which has no secret field at
// all. Keeping the secret in a type that cannot be encoded is cheaper than remembering not
// to encode it.
type Config struct {
	Provider Provider
	// SiteKey is public: it is in the page the visitor loads.
	SiteKey string
	// Secret is the server-side half. It is redacted on every read path.
	Secret string
}

// Enabled reports whether a provider is selected.
func (c Config) Enabled() bool { return c.Provider != "" && c.Provider != ProviderNone }

// Complete reports whether the configuration can actually be enforced: a provider, a site
// key for the browser and a secret for the server.
//
// A half-filled configuration is refused rather than ignored. An operator who selected a
// provider asked for a challenge; silently running without one is the failure mode that
// looks like success.
func (c Config) Complete() bool { return c.Enabled() && c.SiteKey != "" && c.Secret != "" }

// Public is the browser-facing half of the configuration.
type PublicConfig struct {
	// Provider is the widget to render, or "none".
	Provider Provider `json:"provider"`
	// SiteKey is the public key the widget is initialised with. Empty when unconfigured.
	SiteKey string `json:"site_key"`
	// Enabled says whether a widget can be rendered at all: a provider AND a site key.
	Enabled bool `json:"enabled"`
}

// Public returns the shape an unauthenticated page may see.
func (c Config) Public() PublicConfig {
	return PublicConfig{Provider: c.Provider, SiteKey: c.SiteKey, Enabled: c.Enabled() && c.SiteKey != ""}
}

// Service reads the operator's captcha configuration and verifies tokens with it.
//
// It holds a pool and nothing else: the configuration lives in the settings table, so an
// operator can turn a challenge on or off without a restart, and a second instance sees the
// same answer.
type Service struct {
	pool *store.Pool
	// client, timeout and endpoint override the verifier. They exist for tests, which
	// must not reach a real provider; a deployment leaves them zero.
	client   *http.Client
	timeout  time.Duration
	endpoint string
}

// NewService constructs the gate.
func NewService(pool *store.Pool) *Service { return &Service{pool: pool} }

// WithHTTPClient replaces the client used for outbound verification.
func (s *Service) WithHTTPClient(c *http.Client) *Service {
	if c != nil {
		s.client = c
	}
	return s
}

// WithTimeout replaces the bound on the outbound verification.
func (s *Service) WithTimeout(d time.Duration) *Service {
	if d > 0 {
		s.timeout = d
	}
	return s
}

// WithEndpoint points verification at a different siteverify URL. It exists so a test can
// use a fake server; a deployment never calls it.
func (s *Service) WithEndpoint(endpoint string) *Service {
	s.endpoint = endpoint
	return s
}

// Setting keys, in the 'captcha' group.
const (
	settingProvider  = "provider"
	settingSiteKey   = "site_key"
	settingSecretKey = "secret_key"
)

// Config reads the configuration.
//
// Three reads rather than one group read, because a group read redacts the secret (that is
// the whole point of it) and this is the in-process path that must see the real value. Each
// read carries the code-side default, so an installation with no captcha rows reads "none"
// and an empty key rather than failing.
func (s *Service) Config(ctx context.Context) (Config, error) {
	out := Config{Provider: ProviderNone}
	if s.pool == nil {
		return out, nil
	}
	settingsStore := settings.New(s.pool)

	provider, err := settingsStore.String(ctx, "captcha", settingProvider)
	if err != nil {
		return Config{}, err
	}
	p, err := ParseProvider(provider)
	if err != nil {
		return Config{}, err
	}
	siteKey, err := settingsStore.String(ctx, "captcha", settingSiteKey)
	if err != nil {
		return Config{}, err
	}
	secret, err := settingsStore.String(ctx, "captcha", settingSecretKey)
	if err != nil {
		return Config{}, err
	}

	out.Provider = p
	out.SiteKey = strings.TrimSpace(siteKey)
	out.Secret = strings.TrimSpace(secret)
	return out, nil
}

// Public returns the browser-facing configuration.
func (s *Service) Public(ctx context.Context) (PublicConfig, error) {
	cfg, err := s.Config(ctx)
	if err != nil {
		return PublicConfig{}, err
	}
	return cfg.Public(), nil
}

// Check reads the configuration and verifies one token against it.
//
// With no provider configured it returns nil without making any outbound call: that is the
// property that makes "captcha off" indistinguishable from "captcha did not exist".
func (s *Service) Check(ctx context.Context, token, remoteIP string) error {
	cfg, err := s.Config(ctx)
	if err != nil {
		return err
	}
	return s.Verify(ctx, cfg, token, remoteIP)
}

// Verify runs the challenge against an already-read configuration.
func (s *Service) Verify(ctx context.Context, cfg Config, token, remoteIP string) error {
	if !cfg.Enabled() {
		return nil
	}
	if !cfg.Complete() {
		return ErrNotConfigured
	}
	verifier, err := NewVerifier(cfg.Provider, cfg.Secret)
	if err != nil {
		return err
	}
	if s.endpoint != "" {
		verifier.WithEndpoint(s.endpoint)
	}
	if s.client != nil {
		verifier.WithHTTPClient(s.client)
	}
	if s.timeout > 0 {
		verifier.WithTimeout(s.timeout)
	}
	return verifier.Verify(ctx, token, remoteIP)
}
