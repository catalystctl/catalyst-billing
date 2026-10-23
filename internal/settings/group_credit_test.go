package settings_test

import (
	"context"
	"testing"

	"github.com/catalyst/billing/internal/settings"
	"github.com/catalyst/billing/internal/store/storetest"
)

// The credit policy the parity inventory found missing (CRD-10).
//
// The credits group already declared `enabled`, the deposit limits, the maximum balance and
// `auto_use`. What it did not declare was whether a downgrade returns the unused part of the
// period, so the downgrade path had no choice to make: it recorded an obligation nobody could
// switch on or off. These assertions are about the *declaration* — the key's presence, its type,
// and its default — because that is what the settings page, the API validator and internal/credit
// all read.

// The key is declared, of the right type, and defaults to returning the credit: an installation
// that never opened the settings page must not silently keep money it owes.
func TestCreditOnDowngradeIsDeclared(t *testing.T) {
	def, ok := settings.Lookup("credits", "credit_on_downgrade")
	if !ok {
		t.Fatal("credits.credit_on_downgrade is not declared, so a downgrade has no policy to read")
	}
	if def.Kind != settings.KindBool {
		t.Fatalf("kind = %q, want bool", def.Kind)
	}
	if def.Default != "true" {
		t.Fatalf("default = %q, want true (the defect being fixed is credit that nothing posts)", def.Default)
	}
	if def.Secret {
		t.Fatal("a policy switch is not a credential")
	}
}

// The group read carries it, which is what makes the settings page render a control for it rather
// than showing a value nobody can change.
func TestCreditGroupReadIncludesTheDowngradeSwitch(t *testing.T) {
	pool := storetest.DB(t)
	store := settings.New(pool)

	values, err := store.Group(context.Background(), "credits")
	if err != nil {
		t.Fatalf("group: %v", err)
	}
	found := false
	for _, v := range values {
		if v.Key == "credit_on_downgrade" {
			found = true
			// The seeded migration row is the value; a fixture that cleared settings falls back to
			// the code default, and both are "true" by design.
			if v.Value != "true" {
				t.Fatalf("value = %q, want true", v.Value)
			}
		}
	}
	if !found {
		t.Fatalf("the credits group read does not carry credit_on_downgrade: %+v", values)
	}
}

// And a value that is not a boolean is refused rather than quietly read as one, so a typo on the
// settings page cannot change the policy by accident.
func TestCreditOnDowngradeRejectsANonBoolean(t *testing.T) {
	pool := storetest.DB(t)
	store := settings.New(pool)

	if _, err := store.Update(context.Background(), "credits",
		map[string]string{"credit_on_downgrade": "sometimes"}, "", nil); err == nil {
		t.Fatal("a non-boolean value was accepted for a boolean setting")
	}
}
