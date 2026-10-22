package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/catalyst/billing/internal/extproto"
	"github.com/catalyst/billing/internal/gatewayconfig"
)

// The launch configuration reaches the extension process.
//
// This is a regression test for a bug that had no symptom until a real charge. Both the checkout
// adapter and the worker built `extproto.Spec` with no Env, and `Spec.Env` is an allowlist: the
// core deliberately passes only PATH to a child. So an operator could configure a gateway in the
// panel, press Test, see the provider's currencies come back, and then have every charge fail
// with "STRIPE_SECRET_KEY is required" — because the test screen resolved the configuration
// through internal/gatewayconfig and the two paths that actually charge did not.
//
// The test drives a real child process rather than a fake client, because the thing under test is
// what crosses the process boundary. An in-process fake would prove nothing about cmd.Env.
//
// The reporter advertises its visible secrets as protocol capabilities. That is a little
// dishonest about what "capabilities" means, and it is deliberate: the handshake is the only
// structured channel back to the parent before any method is called, and using it keeps the test
// to one file with no new protocol surface.

const envReporter = `package main

import (
	"bufio"
	"encoding/json"
	"os"
	"strings"
)

func main() {
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 64*1024), 4<<20)
	enc := json.NewEncoder(os.Stdout)
	for scanner.Scan() {
		var req struct {
			ID     *uint64         ` + "`json:\"id\"`" + `
			Method string          ` + "`json:\"method\"`" + `
		}
		if err := json.Unmarshal(scanner.Bytes(), &req); err != nil || req.ID == nil {
			continue
		}
		switch req.Method {
		case "initialize":
			var seen []string
			for _, kv := range os.Environ() {
				if strings.HasPrefix(kv, "TEST_SECRET_") {
					seen = append(seen, kv)
				}
			}
			if seen == nil {
				seen = []string{}
			}
			_ = enc.Encode(map[string]any{
				"jsonrpc": "2.0", "id": *req.ID,
				"result": map[string]any{
					"version": 1, "name": "env-reporter", "publisher": "test",
					"capabilities": seen,
				},
			})
		default:
			_ = enc.Encode(map[string]any{
				"jsonrpc": "2.0", "id": *req.ID,
				"error": map[string]any{"code": -32601, "message": "method not found"},
			})
		}
	}
}
`

// buildEnvReporter compiles the reporter and returns its path.
func buildEnvReporter(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	src := filepath.Join(dir, "main.go")
	if err := os.WriteFile(src, []byte(envReporter), 0o600); err != nil {
		t.Fatalf("write reporter: %v", err)
	}
	bin := filepath.Join(dir, "env-reporter")
	out, err := exec.Command("go", "build", "-o", bin, src).CombinedOutput()
	if err != nil {
		t.Fatalf("build reporter: %v\n%s", err, out)
	}
	return bin
}

// The checkout adapter launches its child with the environment the configuration resolved.
func TestTheGatewayAdapterPassesTheResolvedEnvironment(t *testing.T) {
	bin := buildEnvReporter(t)

	adapter := newGatewayAdapter("stripe").withLaunch(bin, []string{
		"TEST_SECRET_API_KEY=sk_test_abc",
		"TEST_SECRET_WEBHOOK=whsec_abc",
	})

	client, err := adapter.clientOrLaunch(context.Background())
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	defer func() { _ = client.Close() }()

	seen := strings.Join(client.Capabilities(), "|")
	for _, want := range []string{"TEST_SECRET_API_KEY=sk_test_abc", "TEST_SECRET_WEBHOOK=whsec_abc"} {
		if !strings.Contains(seen, want) {
			t.Fatalf("the child did not see %q; it saw %v", want, client.Capabilities())
		}
	}
}

// And the same for the worker's cache, which is the path a renewal takes.
func TestTheWorkerCachePassesTheResolvedEnvironment(t *testing.T) {
	bin := buildEnvReporter(t)

	cache := &extensionCache{
		ctx: context.Background(),
		launchOverride: ptr(gatewayconfig.Config{
			Provider:      "stripe",
			ExtensionPath: bin,
		}.WithEnvironment([]string{"TEST_SECRET_API_KEY=sk_test_worker"})),
	}
	defer cache.Close()

	gw, err := cache.gateway()
	if err != nil {
		t.Fatalf("gateway: %v", err)
	}
	caps := gw.client.Capabilities()
	if len(caps) != 1 || caps[0] != "TEST_SECRET_API_KEY=sk_test_worker" {
		t.Fatalf("the worker's child saw %v, want the one configured secret", caps)
	}
}

// A child inherits PATH and nothing else it was not given. This is the property that makes the
// allowlist worth having: a core-only secret — the database URL, the signing key — must not reach
// an extension that has no business reading it.
func TestTheChildDoesNotInheritTheCoresOwnSecrets(t *testing.T) {
	bin := buildEnvReporter(t)
	t.Setenv("TEST_SECRET_LEAKED_FROM_CORE", "must-not-appear")

	adapter := newGatewayAdapter("stripe").withLaunch(bin, nil)
	client, err := adapter.clientOrLaunch(context.Background())
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	defer func() { _ = client.Close() }()

	if caps := client.Capabilities(); len(caps) != 0 {
		t.Fatalf("the child inherited %v, which the core did not pass", caps)
	}
}

// A configuration that names a provider but no binary is an error, not a silent fall-through to
// the fixture. Launching the wrong gateway is worse than refusing to take a payment, because the
// fixture accepts everything.
func TestAnUnresolvableConfigurationIsNotSilentlyReplaced(t *testing.T) {
	cache := &extensionCache{
		ctx:            context.Background(),
		launchOverride: ptr(gatewayconfig.Config{Provider: "stripe"}),
	}
	if _, err := cache.gateway(); err == nil {
		t.Fatal("a provider with no binary launched something anyway")
	}
}

// The spec's Env field is what carries this, so a refactor that stops setting it fails to compile
// here rather than at runtime.
var _ = extproto.Spec{Env: []string{}}

// ptr is the pointer a cache's launchOverride takes.
func ptr[T any](v T) *T { return &v }
