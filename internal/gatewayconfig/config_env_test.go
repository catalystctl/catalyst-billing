package gatewayconfig

import (
	"context"
	"strings"
	"testing"

	"github.com/catalyst/billing/internal/store/storetest"
)

// A stored adapter setting reaches the child's environment (PAY-10).
//
// The extension is a separate process and inherits PATH plus exactly the entries Config.Env
// returns. A setting the panel stores but the resolver forgets to place in that list is a
// configuration that looks saved and changes nothing — the defect this test exists to catch.
func TestSavedCardsSettingReachesTheExtensionEnvironment(t *testing.T) {
	pool := storetest.DB(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
		INSERT INTO settings (group_key, key, value) VALUES ('gateway', 'provider', 'stripe')
		ON CONFLICT (group_key, key) DO UPDATE SET value = EXCLUDED.value`); err != nil {
		t.Fatalf("set provider: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO settings (group_key, key, value) VALUES ('gateway', 'saved_cards_enabled', 'false')
		ON CONFLICT (group_key, key) DO UPDATE SET value = EXCLUDED.value`); err != nil {
		t.Fatalf("set saved cards: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM settings WHERE group_key = 'gateway' AND key IN ('provider', 'saved_cards_enabled')`)
	})

	r := &Resolver{pool: pool, LookupEnv: func(string) (string, bool) { return "", false }}
	cfg, err := r.Load(ctx, GatewayKind)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !cfg.Configured() || cfg.Provider != "stripe" {
		t.Fatalf("config = %+v", cfg)
	}
	env := strings.Join(cfg.Env(), "\n")
	if !strings.Contains(env, "STRIPE_SAVED_CARDS=false") {
		t.Fatalf("the stored setting did not reach the extension environment:\n%s", env)
	}
}
