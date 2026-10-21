package extproto_test

import (
	"context"
	"os"
	"os/exec"
	"testing"

	"github.com/catalyst/billing/internal/extproto"
)

// Prove that a core-only secret in the parent environment is invisible to the
// extension process.
func TestParentSecretsNotInherited(t *testing.T) {
	os.Setenv("BILLING_DATABASE_URL", "postgres://user:supersecret@db/app")
	os.Setenv("BILLING_WEBHOOK_SECRET", "whsec_topsecret")
	defer os.Unsetenv("BILLING_DATABASE_URL")
	defer os.Unsetenv("BILLING_WEBHOOK_SECRET")

	bin := buildFake(t, "fakegateway")
	c, err := extproto.Launch(context.Background(), extproto.Spec{
		Path: bin,
		Env:  []string{"FAKE_GATEWAY_MODE=ok"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	var out struct {
		Vars map[string]string `json:"vars"`
	}
	if err := c.Call(context.Background(), "fixture.env", struct{}{}, &out); err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{"BILLING_DATABASE_URL", "BILLING_WEBHOOK_SECRET"} {
		if v, ok := out.Vars[leak]; ok {
			t.Fatalf("%s leaked into the extension environment: %q", leak, v)
		}
	}
	if _, ok := out.Vars["FAKE_GATEWAY_MODE"]; !ok {
		t.Fatal("the explicitly passed variable did not arrive")
	}
	if _, ok := out.Vars["PATH"]; !ok {
		t.Fatal("PATH is missing; the child cannot resolve anything")
	}
	t.Logf("child env: %v", out.Vars)
}

func buildFake(t *testing.T, name string) string {
	t.Helper()
	dir := t.TempDir()
	out := dir + "/" + name
	cmd := exec.Command("go", "build", "-o", out, "github.com/catalyst/billing/extensions/"+name)
	cmd.Dir = "../.."
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build %s: %v\n%s", name, err, b)
	}
	return out
}
