package gatewayconfig

// The providers the panel knows how to launch, and the fields each one stores.
//
// A provider is not just a name: it is the mapping between a settings key, the environment
// variable the adapter reads, and whether the value is a secret. That mapping is why this is
// a table in code rather than a free-text provider field — a provider whose credentials the
// panel cannot translate into environment variables is a form that saves nothing, which is
// exactly the defect this package exists to fix.
//
// A knob an adapter accepts but the settings table has no key for (STRIPE_API_BASE,
// PTERODACTYL_EGG_ID, …) is listed as an `EnvOnly` entry: the screen names it so an operator
// can find it, and the runtime passes it through from the environment untouched.

// Kind is which of the two integration surfaces a configuration belongs to.
type Kind string

const (
	// GatewayKind is the payment gateway: one configured provider at a time.
	GatewayKind Kind = "gateway"
	// ServerKind is the provisioning server: one configured provider at a time.
	ServerKind Kind = "server"
)

// Field is one credential the panel stores for a provider.
type Field struct {
	// Key is the settings key inside the provider's group.
	Key string `json:"key"`
	// Label is what the operator sees. The settings key is an identifier, not copy.
	Label string `json:"label"`
	// Env is the environment variable the extension process reads.
	Env string `json:"env"`
	// Secret marks a value that is write-only: never returned by any read path.
	Secret bool `json:"is_secret"`
	// Hint explains what the value is, and where to get it from the provider.
	Hint string `json:"hint,omitempty"`
}

// EnvKnob is an adapter setting the panel has no field for. It is documented so the screen
// can list it instead of hiding a configuration surface an operator needs.
type EnvKnob struct {
	Env  string `json:"env"`
	Hint string `json:"hint,omitempty"`
}

// Provider is one extension the panel can launch.
type Provider struct {
	// ID is what is stored in `gateway.provider` / `servers.provider`.
	ID string `json:"id"`
	// Name is the human name of the provider, not the extension.
	Name string `json:"name"`
	// Extension is the binary name under bin/ when no explicit path is configured.
	Extension string `json:"extension"`
	// Fields are the values the panel stores and the runtime passes to the extension.
	Fields []Field `json:"fields"`
	// EnvOnly are the adapter's remaining environment variables, listed for the operator.
	EnvOnly []EnvKnob `json:"env_only,omitempty"`
	// Note is one sentence about the provider, shown on the screen.
	Note string `json:"note,omitempty"`
}

// gatewayProviders is the payment side. The fixture is listed deliberately: an installation
// with no Stripe account can still prove the wiring end to end, which is what the browser
// harness and the package tests do.
var gatewayProviders = []Provider{
	{
		ID:        "stripe",
		Name:      "Stripe",
		Extension: "stripe",
		Note:      "Cards, wallets and bank debits through Stripe Checkout. Webhooks settle an invoice.",
		Fields: []Field{
			{
				Key: "publishable_key", Label: "Publishable key",
				Env:  "STRIPE_PUBLISHABLE_KEY",
				Hint: "pk_live_… or pk_test_…. It is public by design: it identifies the account to the browser.",
			},
			{
				Key: "secret_key", Label: "Secret key", Env: "STRIPE_SECRET_KEY", Secret: true,
				Hint: "sk_live_… or sk_test_…. Write-only: it is never returned, only reported as set.",
			},
			{
				Key: "webhook_secret", Label: "Webhook signing secret", Env: "STRIPE_WEBHOOK_SECRET", Secret: true,
				Hint: "whsec_…. Without it every delivery is rejected, which is the correct default.",
			},
		},
		EnvOnly: []EnvKnob{
			{Env: "STRIPE_API_BASE", Hint: "Defaults to https://api.stripe.com."},
			{Env: "STRIPE_CURRENCIES", Hint: "Comma-separated codes to advertise; defaults to the usual set."},
			{Env: "STRIPE_TIMEOUT_MS", Hint: "Per-call HTTP timeout, default 20000."},
			{Env: "STRIPE_WEBHOOK_TOLERANCE_S", Hint: "Replay window for a delivery, default 300 seconds."},
		},
	},
	{
		ID:        "fake",
		Name:      "Fixture gateway",
		Extension: "fakegateway",
		Note:      "The conformance fixture. It takes no credentials and settles nothing; use it to prove the wiring.",
	},
}

// serverProviders is the provisioning side.
var serverProviders = []Provider{
	{
		ID:        "pterodactyl",
		Name:      "Pterodactyl",
		Extension: "pterodactyl",
		Note:      "Game servers through Pterodactyl's application API. Suspend, resume and terminate are real calls.",
		Fields: []Field{
			{
				Key: "panel_url", Label: "Panel URL", Env: "PTERODACTYL_PANEL_URL",
				Hint: "The panel's origin, e.g. https://panel.example.com. No /api suffix: the adapter adds it.",
			},
			{
				Key: "api_key", Label: "Application API key", Env: "PTERODACTYL_API_KEY", Secret: true,
				Hint: "ptla_…. An application key, not a client key. Write-only: it is never returned.",
			},
		},
		EnvOnly: []EnvKnob{
			{Env: "PTERODACTYL_NEST_ID", Hint: "Required. The nest the egg lives in."},
			{Env: "PTERODACTYL_EGG_ID", Hint: "Required. The egg new servers are created from."},
			{Env: "PTERODACTYL_NODE_ID", Hint: "Optional default node when a plan names none."},
			{Env: "PTERODACTYL_DOCKER_IMAGE", Hint: "Optional override of the egg's own image."},
			{Env: "PTERODACTYL_ALLOCATION_ID", Hint: "Optional fixed allocation instead of the first free one."},
			{Env: "PTERODACTYL_PLANS", Hint: "Comma-separated id:memory_mb:disk_mb:cpu_percent bundles."},
			{Env: "PTERODACTYL_REGIONS", Hint: "Comma-separated region labels to advertise."},
		},
	},
	{
		ID:        "fake",
		Name:      "Fixture provisioner",
		Extension: "fakeprov",
		Note:      "The conformance fixture. It takes no credentials and creates nothing; use it to prove the wiring.",
	},
}

// Providers returns the providers of one kind, in render order.
func Providers(kind Kind) []Provider {
	src := gatewayProviders
	if kind == ServerKind {
		src = serverProviders
	}
	out := make([]Provider, len(src))
	copy(out, src)
	return out
}

// providerFor finds a provider by its stored id.
func providerFor(kind Kind, id string) (Provider, bool) {
	for _, p := range Providers(kind) {
		if p.ID == id {
			return p, true
		}
	}
	return Provider{}, false
}

// pathEnv is the environment variable that names the extension binary for a kind.
func pathEnv(kind Kind) string {
	if kind == ServerKind {
		return "BILLING_PROVISIONER_EXTENSION"
	}
	return "BILLING_GATEWAY_EXTENSION"
}

// providerEnv is the environment variable that can override the stored provider name. Only
// the gateway has one: the provisioner is identified by the binary its path variable names.
func providerEnv(kind Kind) string {
	if kind == ServerKind {
		return ""
	}
	return "BILLING_GATEWAY_PROVIDER"
}
