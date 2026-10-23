package gatewayconfig_test

import (
	"context"
	"strings"
	"testing"

	"github.com/catalyst/billing/internal/gatewayconfig"
	"github.com/catalyst/billing/internal/settings"
	"github.com/catalyst/billing/internal/store/storetest"
)

// The second real provisioner adapter must be selectable and configurable from the panel
// (SVC-18, SVC-19).
//
// The defect this pins down is specific and was real: extensions/cpanel existed, spoke the
// protocol and had its own tests, but `serverProviders` named only Pterodactyl and the fixture.
// So an operator with a WHM server had no form, the CPANEL_* variables never reached the child
// process, and the adapter was unreachable dead code. Two things have to stay true for that not
// to come back, and they are the two assertions below: the provider is in the catalogue, and its
// credential fields are *declared settings* whose values reach Config.Env().

// TestEveryProviderFieldIsAStoredSetting is the general form: a field whose key the settings
// store does not declare cannot be saved from the panel at all, so the form would accept a value
// and the write would fail with unknown_setting.
func TestEveryProviderFieldIsAStoredSetting(t *testing.T) {
	for _, kind := range []gatewayconfig.Kind{gatewayconfig.GatewayKind, gatewayconfig.ServerKind} {
		for _, provider := range gatewayconfig.Providers(kind) {
			for _, field := range provider.Fields {
				if _, ok := settings.Lookup(groupName(kind), field.Key); !ok {
					t.Fatalf("%s provider %q declares field %q, but settings has no %s.%s; saving it would fail",
						kind, provider.ID, field.Key, groupName(kind), field.Key)
				}
				if field.Env == "" {
					t.Fatalf("%s provider %q field %q names no environment variable, so the value could never reach the adapter",
						kind, provider.ID, field.Key)
				}
			}
		}
	}
}

func groupName(kind gatewayconfig.Kind) string {
	if kind == gatewayconfig.ServerKind {
		return "servers"
	}
	return "gateway"
}

// TestCPanelCredentialsReachTheChildEnvironment is the end of the wire: what the panel stores is
// what the extension process is launched with.
func TestCPanelCredentialsReachTheChildEnvironment(t *testing.T) {
	pool := storetest.DB(t)
	ctx := context.Background()

	resolver := gatewayconfig.New(pool)
	// No environment overrides: this test is about the panel's own values.
	resolver.LookupEnv = func(string) (string, bool) { return "", false }

	cfg, err := resolver.Save(ctx, gatewayconfig.ServerKind, map[string]string{
		"provider":  "cpanel",
		"whm_url":   "https://whm.example.test:2087",
		"api_token": "whm-token-value",
	}, "", nil)
	if err != nil {
		t.Fatalf("save cpanel configuration: %v", err)
	}
	if cfg.Provider != "cpanel" {
		t.Fatalf("provider = %q, want cpanel", cfg.Provider)
	}

	env := strings.Join(cfg.Env(), "\n")
	for _, want := range []string{
		"CPANEL_WHM_URL=https://whm.example.test:2087",
		"CPANEL_API_TOKEN=whm-token-value",
	} {
		if !strings.Contains(env, want) {
			t.Fatalf("the child environment is missing %q:\n%s", want, env)
		}
	}
	// The Pterodactyl variables are a different provider's: sending them to a cPanel process
	// would be configuration for an adapter that is not running.
	if strings.Contains(env, "PTERODACTYL_") {
		t.Fatalf("the cPanel environment carries Pterodactyl variables:\n%s", env)
	}

	// And the secret is not readable back: the read path reports that it is set, never its text.
	reloaded, err := resolver.Load(ctx, gatewayconfig.ServerKind)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	for _, value := range reloaded.Values {
		if value.Key == "api_token" {
			if !value.IsSet || !value.IsSecret {
				t.Fatalf("api_token read back as is_set=%t is_secret=%t, want both true", value.IsSet, value.IsSecret)
			}
		}
	}
	// The runtime path still needs the real value; only the read API redacts it.
	if !strings.Contains(strings.Join(reloaded.Env(), "\n"), "whm-token-value") {
		t.Fatal("the runtime environment lost the stored token")
	}
}
