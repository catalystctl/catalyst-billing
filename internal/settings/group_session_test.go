package settings_test

import (
	"context"
	"testing"

	"github.com/catalyst/billing/internal/settings"
	"github.com/catalyst/billing/internal/store/storetest"
)

// Session binding defaults to off for everyone. The control is opt-in: an installation that
// upgrades must not start signing people out, which is what a stricter default would do.
func TestSessionBindingDefaultsToOffForEveryone(t *testing.T) {
	pool := storetest.DB(t)
	mode, scope := settings.SessionBinding(context.Background(), pool)
	if mode != settings.BindOff {
		t.Fatalf("binding mode = %q, want off", mode)
	}
	if scope != settings.BindScopeAll {
		t.Fatalf("binding scope = %q, want all", scope)
	}
}

// The two keys are read independently, and a value outside the closed set falls back to the
// safe default rather than to a rule nobody wrote.
func TestSessionBindingReadsModeAndScope(t *testing.T) {
	pool := storetest.DB(t)
	ctx := context.Background()

	set := func(key, value string) {
		t.Helper()
		if _, err := pool.Exec(ctx, `
			INSERT INTO settings (group_key, key, value) VALUES ('session', $1, $2)
			ON CONFLICT (group_key, key) DO UPDATE SET value = $2`, key, value); err != nil {
			t.Fatalf("set session.%s: %v", key, err)
		}
	}

	set("binding", settings.BindUserAgent)
	mode, scope := settings.SessionBinding(ctx, pool)
	if mode != settings.BindUserAgent || scope != settings.BindScopeAll {
		t.Fatalf("mode/scope = %q/%q, want user_agent/all", mode, scope)
	}

	set("binding_scope", settings.BindScopeCustomers)
	if _, scope = settings.SessionBinding(ctx, pool); scope != settings.BindScopeCustomers {
		t.Fatalf("scope = %q, want customers", scope)
	}

	// A hand-edited value is not trusted: an unparseable mode must not become a stricter rule
	// than the one written, because a typo would then sign out an installation.
	set("binding", "ip+agent")
	if mode, _ = settings.SessionBinding(ctx, pool); mode != settings.BindOff {
		t.Fatalf("binding mode = %q after an invalid value, want off", mode)
	}
	set("binding_scope", "everyone")
	if _, scope = settings.SessionBinding(ctx, pool); scope != settings.BindScopeAll {
		t.Fatalf("binding scope = %q after an invalid value, want all", scope)
	}
}
