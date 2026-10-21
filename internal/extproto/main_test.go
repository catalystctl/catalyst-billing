package extproto

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/catalyst/billing/internal/money"
)

// The conformance fixtures are real executables, built once per test binary.
// Building them here rather than using "go run" matters: "go run" needs HOME and
// GOCACHE in the child's environment, and this package deliberately hands the
// child nothing but PATH.
var (
	fakeGatewayBin string
	fakeProvBin    string
)

func TestMain(m *testing.M) {
	os.Exit(runWithFixtures(m))
}

func runWithFixtures(m *testing.M) int {
	dir, err := os.MkdirTemp("", "extproto-fixtures-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "extproto tests: temp dir: %v\n", err)
		return 1
	}
	defer os.RemoveAll(dir)

	root, err := filepath.Abs("../..")
	if err != nil {
		fmt.Fprintf(os.Stderr, "extproto tests: module root: %v\n", err)
		return 1
	}
	for _, name := range []string{"fakegateway", "fakeprov"} {
		out := filepath.Join(dir, name)
		build := exec.Command("go", "build", "-o", out, "./extensions/"+name)
		build.Dir = root
		if b, err := build.CombinedOutput(); err != nil {
			fmt.Fprintf(os.Stderr, "extproto tests: building %s: %v\n%s", name, err, b)
			return 1
		}
	}
	fakeGatewayBin = filepath.Join(dir, "fakegateway")
	fakeProvBin = filepath.Join(dir, "fakeprov")
	return m.Run()
}

// gatewaySpec is a Spec pointed at the gateway fixture. The timeouts are long
// enough that a slow CI machine does not flake and short enough that a hung
// fixture cannot stall the suite.
func gatewaySpec(env ...string) Spec {
	return Spec{
		Path:             fakeGatewayBin,
		Env:              env,
		HandshakeTimeout: 10 * time.Second,
		CallTimeout:      10 * time.Second,
		CoreVersion:      "test",
	}
}

// provSpec is a Spec pointed at the provisioner fixture.
func provSpec(env ...string) Spec {
	return Spec{
		Path:             fakeProvBin,
		Env:              env,
		HandshakeTimeout: 10 * time.Second,
		CallTimeout:      10 * time.Second,
		CoreVersion:      "test",
	}
}

// launchGateway starts the gateway fixture and registers its cleanup.
func launchGateway(t *testing.T, env ...string) *Client {
	t.Helper()
	c, err := Launch(context.Background(), gatewaySpec(env...))
	if err != nil {
		t.Fatalf("Launch(fakegateway, %v): %v", env, err)
	}
	t.Cleanup(func() {
		if err := c.Close(); err != nil {
			t.Errorf("Close(fakegateway): %v", err)
		}
	})
	return c
}

// launchProv starts the provisioner fixture and registers its cleanup.
func launchProv(t *testing.T, env ...string) *Client {
	t.Helper()
	c, err := Launch(context.Background(), provSpec(env...))
	if err != nil {
		t.Fatalf("Launch(fakeprov, %v): %v", env, err)
	}
	t.Cleanup(func() {
		if err := c.Close(); err != nil {
			t.Errorf("Close(fakeprov): %v", err)
		}
	})
	return c
}

// testCtx returns a context bounded well inside the spec's CallTimeout.
func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func mustUSD(t *testing.T) money.Currency {
	t.Helper()
	cur, ok := money.LookupCurrency("USD")
	if !ok {
		t.Fatal("USD missing from the ISO 4217 table")
	}
	return cur
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// waitFor polls cond until it is true or the deadline passes.
func waitFor(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", d, what)
}
