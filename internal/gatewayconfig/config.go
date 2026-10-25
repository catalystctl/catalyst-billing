package gatewayconfig

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/catalyst/billing/internal/settings"
	"github.com/catalyst/billing/internal/store"
)

// Where a value came from. The screen renders this verbatim, because "set" is not enough:
// an operator needs to know whether saving a new value will actually change anything.
const (
	// SourceEnvironment means the deployment's environment named it, and the panel cannot
	// override it. The form is read-only for that field.
	SourceEnvironment = "environment"
	// SourcePanel means the value is a settings row the panel wrote.
	SourcePanel = "panel"
	// SourceDefault means no row and no variable exist, and the code chose a value (the
	// extension binary under bin/).
	SourceDefault = "default"
	// SourceUnset means there is no value at all.
	SourceUnset = "unset"
)

// Errors callers branch on.
var (
	// ErrNotConfigured is "there is no gateway / provisioner to launch". It is the normal
	// state of a fresh installation, not a failure: the screen renders it as a prompt.
	ErrNotConfigured = errors.New("gatewayconfig: no provider is configured")
	// ErrUnknownProvider is a stored provider id the panel cannot launch. It is an error
	// rather than a default because launching the wrong binary is worse than refusing.
	ErrUnknownProvider = errors.New("gatewayconfig: unknown provider")
	// ErrDisabled is "configured, but switched off". Payments are off by default because a
	// gateway that starts taking money on first boot is a surprise, not a feature.
	ErrDisabled = errors.New("gatewayconfig: the integration is disabled")
)

// Value is one credential as a read path sees it.
//
// A secret's text is never placed in an exported field, so `json.Marshal` of a Config
// cannot leak it even if a handler forgets to think about redaction. That is the same
// defence settings.Value uses, applied one layer up where the credential is resolved.
type Value struct {
	// Key is the settings key, e.g. "secret_key".
	Key string `json:"key"`
	// Label is what the operator sees.
	Label string `json:"label"`
	// Env is the environment variable the extension reads.
	Env string `json:"env"`
	// Hint explains the value and where to get it.
	Hint string `json:"hint,omitempty"`
	// IsSecret marks a write-only value.
	IsSecret bool `json:"is_secret"`
	// IsSet says a value exists. It is the only thing a secret's read reveals.
	IsSet bool `json:"is_set"`
	// Source is SourceEnvironment, SourcePanel, SourceDefault or SourceUnset.
	Source string `json:"source"`
	// Editable is false when the environment supplies the value. A form that accepts a
	// value the runtime ignores is worse than no form: the operator saves it, the screen
	// says "saved", and nothing changes.
	Editable bool `json:"editable"`
	// value is the real text, for the extension host and nobody else.
	value string
}

// Config is one integration resolved: which provider, which binary, which credentials.
type Config struct {
	Kind Kind `json:"kind"`
	// Provider is the stored provider id, e.g. "stripe".
	Provider string `json:"provider"`
	// ProviderName is the provider's human name.
	ProviderName string `json:"provider_name"`
	// ProviderSource and ProviderEditable follow the same rule as a Value's.
	ProviderSource   string `json:"provider_source"`
	ProviderEditable bool   `json:"provider_editable"`
	// Enabled is whether the runtime should use this integration at all.
	Enabled bool `json:"enabled"`
	// EnabledSource is where Enabled came from; for the provisioner there is no switch, so
	// it is "panel" whenever a provider is stored.
	EnabledSource string `json:"enabled_source"`
	// EnabledEditable is false when the environment decided the enabled state.
	EnabledEditable bool `json:"enabled_editable"`
	// OffSession is gateway.off_session_enabled: may a renewal charge a saved method with
	// no customer present. It is reported for the gateway and ignored for a provisioner.
	OffSession bool `json:"off_session_enabled"`
	// ExtensionPath is the binary the runtime launches.
	ExtensionPath string `json:"extension_path"`
	// ExtensionSource is where ExtensionPath came from.
	ExtensionSource string `json:"extension_source"`
	// ProviderExtension is the binary name the configured provider expects under the bin directory.
	ProviderExtension string `json:"provider_extension,omitempty"`
	// ExtensionMismatch is true when the environment points the integration at a binary that is not the one
	// the configured provider names. That combination is a trap: the settings screen says one provider, the
	// process runs another, and every failure is attributed to the wrong component. It is reported rather
	// than refused, because a deployment may deliberately override the path to a build elsewhere — but it is
	// never silent.
	ExtensionMismatch bool `json:"extension_mismatch,omitempty"`
	// Values are the provider's credentials, in render order.
	Values []Value `json:"values"`
	// WebhookURL is where the provider must deliver events, derived from
	// BILLING_PUBLIC_URL. Empty when the public URL is unknown.
	WebhookURL string `json:"webhook_url,omitempty"`
	// Providers is the catalogue the screen offers, so a client needs no second request.
	Providers []Provider `json:"providers"`
	// Notes are non-fatal remarks about the configuration (a missing secret, a binary the
	// runtime could not find). They are what makes the screen explain itself instead of
	// offering a Test button that will fail.
	Notes []string `json:"notes,omitempty"`

	// env is the environment the extension process must be started with. It holds real
	// secret text and is deliberately unexported.
	env []string
	// signingSecret is the webhook signing secret the core's own verifier uses.
	signingSecret string
}

// Configured reports whether there is a provider to launch.
func (c Config) Configured() bool { return c.Provider != "" }

// Ready reports whether the runtime should launch this integration now: a provider is
// configured and it is switched on.
func (c Config) Ready() bool { return c.Configured() && c.Enabled }

// Env returns the environment for the extension process.
//
// This is the one method that hands out secret text. It exists because the extension host
// is the only consumer that needs it: the child process inherits PATH and these entries and
// nothing else, so a core-only secret cannot leak through.
func (c Config) Env() []string {
	out := make([]string, len(c.env))
	copy(out, c.env)
	return out
}

// WithEnvironment returns a copy of the configuration carrying an explicit environment.
//
// It is what the resolver produces internally, exposed because two callers outside this package
// legitimately need to build one: a test that asserts the environment reaches a child process, and
// a deployment that supplies the whole configuration from its own secret store rather than from
// the settings table. Both would otherwise reach into an unexported field, and a test that does
// that stops testing the thing it claims to.
//
// It copies rather than aliases, so the caller cannot mutate a configuration already in use.
func (c Config) WithEnvironment(env []string) Config {
	out := make([]string, len(env))
	copy(out, env)
	c.env = out
	return c
}

// Value returns a credential's real text. It is the in-process path the extension host and
// the webhook verifier use; it is not, and must not become, an HTTP response.
func (c Config) Value(key string) (string, bool) {
	for _, v := range c.Values {
		if v.Key == key {
			return v.value, v.IsSet
		}
	}
	return "", false
}

// WebhookSecret returns the secret the core verifies deliveries with.
func (c Config) WebhookSecret() (string, bool) {
	return c.signingSecret, c.signingSecret != ""
}

// Resolver resolves a configuration from the environment and the settings table.
//
// The seams exist so a test can prove the precedence without touching the process
// environment, which is shared with every other test in the binary.
type Resolver struct {
	pool *store.Pool
	// LookupEnv is the environment seam; nil means os.LookupEnv.
	LookupEnv func(string) (string, bool)
	// BinDir is where a provider's default binary lives; empty means "bin", the layout
	// `make build` and the README produce.
	BinDir string
}

// New returns a Resolver backed by a pool.
func New(pool *store.Pool) *Resolver {
	return &Resolver{pool: pool}
}

// Pool returns the pool the resolver reads through. It exists so the write path can build
// the settings store per call, which is the same shape the operator surface uses: a pool
// handle and a clock, not shared mutable state.
func (r *Resolver) Pool() *store.Pool { return r.pool }

// lookup reads an environment variable, treating whitespace-only as unset: a variable set
// to "" in a compose file is an operator saying "not configured", not a credential.
func (r *Resolver) lookup(key string) string {
	fn := r.LookupEnv
	if fn == nil {
		fn = os.LookupEnv
	}
	return strings.TrimSpace(mustLookup(fn, key))
}

func mustLookup(fn func(string) (string, bool), key string) string {
	v, _ := fn(key)
	return v
}

// Load resolves one integration.
//
// It never fails because nothing is configured: an unconfigured panel is a state the screen
// must render, not an error. It fails only when the settings table cannot be read.
func (r *Resolver) Load(ctx context.Context, kind Kind) (Config, error) {
	group := "gateway"
	if kind == ServerKind {
		group = "servers"
	}
	cfg := Config{Kind: kind, Providers: Providers(kind)}

	// The provider name. The environment wins, and naming a provider there also enables the
	// gateway: a deployment that names one is asking for payments to work.
	envProvider := ""
	if name := providerEnv(kind); name != "" {
		envProvider = r.lookup(name)
	}
	storedProvider, err := r.setting(ctx, group, "provider")
	if err != nil {
		return Config{}, err
	}
	switch {
	case envProvider != "":
		cfg.Provider = envProvider
		cfg.ProviderSource = SourceEnvironment
		cfg.ProviderEditable = false
	case storedProvider != "":
		cfg.Provider = storedProvider
		cfg.ProviderSource = SourcePanel
		cfg.ProviderEditable = true
	default:
		cfg.ProviderSource = SourceUnset
		cfg.ProviderEditable = true
	}

	// The enabled switch. There is no environment variable for the provisioner's, because a
	// deployment names its provisioner by the binary it points at.
	if kind == GatewayKind {
		if envProvider != "" {
			cfg.Enabled = true
			cfg.EnabledSource = SourceEnvironment
			cfg.EnabledEditable = false
		} else {
			enabled, src, err := r.settingBool(ctx, group, "enabled", false)
			if err != nil {
				return Config{}, err
			}
			cfg.Enabled = enabled
			cfg.EnabledSource = src
			cfg.EnabledEditable = src != SourceEnvironment
		}
		offSession, _, err := r.settingBool(ctx, group, "off_session_enabled", false)
		if err != nil {
			return Config{}, err
		}
		cfg.OffSession = offSession
	} else {
		cfg.Enabled = cfg.Provider != ""
		cfg.EnabledSource = SourcePanel
		cfg.EnabledEditable = cfg.ProviderSource != SourceEnvironment
	}

	provider, known := providerFor(kind, cfg.Provider)
	if cfg.Provider != "" && !known {
		cfg.ProviderName = cfg.Provider
		cfg.Notes = append(cfg.Notes,
			fmt.Sprintf("%q is not a provider this build can launch; choose one of the listed providers.", cfg.Provider))
	}

	if known {
		cfg.ProviderName = provider.Name
		cfg.Values, err = r.resolveFields(ctx, group, provider)
		if err != nil {
			return Config{}, err
		}
		cfg.env = append(cfg.env, r.passthrough(provider)...)
		for _, v := range cfg.Values {
			if v.IsSet && v.value != "" {
				cfg.env = append(cfg.env, v.Env+"="+v.value)
			}
		}
	}

	if known {
		cfg.ProviderExtension = provider.Extension
	}

	// The binary. The environment wins; otherwise the provider's name under bin/.
	envPath := r.lookup(pathEnv(kind))
	switch {
	case envPath != "":
		cfg.ExtensionPath = envPath
		cfg.ExtensionSource = SourceEnvironment
		// The environment override beats the configured provider, which is intended for a deployment that
		// builds the binary elsewhere — and is a trap when it names a *different* adapter. "provider=catalystctl"
		// with BILLING_PROVISIONER_EXTENSION=bin/fakeprov runs the fixture, so every real service reports
		// "the server panel does not recognise this service" while the settings screen looks correct.
		if known && !strings.EqualFold(filepath.Base(envPath), provider.Extension) {
			cfg.ExtensionMismatch = true
			cfg.Notes = append(cfg.Notes, fmt.Sprintf(
				"%s points at %s, but the configured provider is %q, which expects %s. "+
					"The process is running %s — every request goes to the wrong adapter.",
				pathEnv(kind), filepath.Base(envPath), cfg.Provider, provider.Extension, filepath.Base(envPath)))
		}
	case known:
		cfg.ExtensionPath = filepath.Join(r.binDir(), provider.Extension)
		cfg.ExtensionSource = SourceDefault
	default:
		cfg.ExtensionSource = SourceUnset
	}
	if cfg.ExtensionPath != "" {
		if _, statErr := os.Stat(cfg.ExtensionPath); statErr != nil {
			cfg.Notes = append(cfg.Notes,
				fmt.Sprintf("The extension binary %s was not found. Set %s to its path, or build it into %s.",
					cfg.ExtensionPath, pathEnv(kind), r.binDir()))
		}
	}

	// The webhook URL and the core's own verifier. Only the gateway has them.
	if kind == GatewayKind {
		cfg.WebhookURL = WebhookURL(r.lookup("BILLING_PUBLIC_URL"), cfg.Provider)
		// The core verifies deliveries with BILLING_WEBHOOK_SECRET when the deployment sets
		// it, otherwise with the secret saved in the panel. Two sources, one value, and the
		// environment wins for the same reason it wins everywhere else.
		if secret := r.lookup("BILLING_WEBHOOK_SECRET"); secret != "" {
			cfg.signingSecret = secret
		} else if known {
			if v, ok := cfg.Value("webhook_secret"); ok {
				cfg.signingSecret = v
			}
		}
	}

	// A missing credential is the most common reason a test fails, so say it here rather
	// than letting the operator discover it from a child process's stderr.
	for _, v := range cfg.Values {
		if !v.IsSet && v.IsSecret {
			cfg.Notes = append(cfg.Notes,
				fmt.Sprintf("%s is not set. The connection test will fail until it is.", v.Label))
		}
	}

	return cfg, nil
}

// resolveFields resolves one provider's credentials.
func (r *Resolver) resolveFields(ctx context.Context, group string, provider Provider) ([]Value, error) {
	out := make([]Value, 0, len(provider.Fields))
	for _, f := range provider.Fields {
		v := Value{
			Key: f.Key, Label: f.Label, Env: f.Env, Hint: f.Hint,
			IsSecret: f.Secret, Source: SourceUnset, Editable: true,
		}
		if envValue := r.lookup(f.Env); envValue != "" {
			// The environment wins and the form is read-only for this field.
			v.value = envValue
			v.IsSet = true
			v.Source = SourceEnvironment
			v.Editable = false
		} else {
			panelValue, err := r.setting(ctx, group, f.Key)
			if err != nil {
				return nil, err
			}
			if panelValue != "" {
				v.value = panelValue
				v.IsSet = true
				v.Source = SourcePanel
			}
		}
		out = append(out, v)
	}
	return out, nil
}

// passthrough forwards an adapter's environment-only settings untouched.
//
// They have no settings key, so there is nothing to resolve: the environment is the only
// source, and dropping them would break an adapter configured the way its package comment
// documents.
func (r *Resolver) passthrough(provider Provider) []string {
	var out []string
	for _, knob := range provider.EnvOnly {
		if value := r.lookup(knob.Env); value != "" {
			out = append(out, knob.Env+"="+value)
		}
	}
	return out
}

// setting reads one stored value, or "" when no row exists.
func (r *Resolver) setting(ctx context.Context, group, key string) (string, error) {
	value, err := settings.New(r.pool).String(ctx, group, key)
	if err != nil {
		return "", fmt.Errorf("gatewayconfig: read %s.%s: %w", group, key, err)
	}
	return strings.TrimSpace(value), nil
}

// settingBool reads one stored boolean, reporting where it came from.
func (r *Resolver) settingBool(ctx context.Context, group, key string, def bool) (bool, string, error) {
	raw, err := r.setting(ctx, group, key)
	if err != nil {
		return false, "", err
	}
	if raw == "" {
		return def, SourceDefault, nil
	}
	switch strings.ToLower(raw) {
	case "true", "1", "yes":
		return true, SourcePanel, nil
	case "false", "0", "no":
		return false, SourcePanel, nil
	default:
		// The settings table validates booleans on write, so this is a row written by hand.
		// Reading it as the default is honest; failing the whole page is not.
		return def, SourceDefault, nil
	}
}

// binDir is where a provider's default binary lives.
func (r *Resolver) binDir() string {
	if r.BinDir != "" {
		return r.BinDir
	}
	return "bin"
}

// WebhookURL derives the URL an operator pastes into the provider's dashboard.
//
// It is derived from BILLING_PUBLIC_URL rather than from the request's Host header: the
// address a webhook must reach is the deployment's public origin, and a request can arrive
// through an internal name the provider cannot resolve.
func WebhookURL(publicURL, provider string) string {
	base := strings.TrimRight(strings.TrimSpace(publicURL), "/")
	if base == "" || provider == "" {
		return ""
	}
	return base + "/webhooks/" + provider
}
