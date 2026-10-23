package i18n_test

import (
	"context"
	"errors"
	"testing"

	"github.com/catalyst/billing/internal/i18n"
	"github.com/catalyst/billing/internal/store/storetest"
)

// A stored preference is only worth storing if it is one the catalogue can render.
//
// `de` ships disabled with a full translation set and `fr` was never configured at all; the store
// must refuse both, because a preference for a language nobody offers leaves every label on the
// page falling through to the key itself. An operator switching a language off is a decision that
// has to outrank a customer's earlier choice, which is why the write is refused now rather than
// kept and ignored later.

func TestSetUserLocaleRefusesALanguageTheInstallationDoesNotOffer(t *testing.T) {
	// One DB(t) call per test: it takes a session advisory lock, so a second call in the same
	// test would block on itself until the deadline.
	pool := storetest.DB(t)
	s := i18n.New(pool)
	ctx := context.Background()

	var userID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO users (email, password_hash, display_name)
		 VALUES ('i18n-refused@example.test', 'not-a-real-hash', 'Refused')
		 RETURNING id`).Scan(&userID); err != nil {
		t.Fatalf("seed user: %v", err)
	}

	// `de` exists and ships disabled; `fr` was never configured. Both are "not offered", and a
	// client needs one answer rather than two.
	for _, code := range []string{"de", "fr"} {
		if _, err := s.SetUserLocale(ctx, userID, code); !errors.Is(err, i18n.ErrLocaleNotEnabled) {
			t.Fatalf("SetUserLocale(%q) error = %v, want ErrLocaleNotEnabled", code, err)
		}
		if got, err := s.UserLocale(ctx, userID); err != nil || got != "" {
			t.Fatalf("after refusing %q: stored locale = %q, %v; want empty", code, got, err)
		}
	}

	// Enabling it is the only thing that changed, and it is enough.
	if _, err := s.Update(ctx, "de", i18n.LocaleInput{Code: "de", Name: "German", Enabled: boolPtr(true)}); err != nil {
		t.Fatalf("enable de: %v", err)
	}
	if got, err := s.SetUserLocale(ctx, userID, "DE"); err != nil {
		t.Fatalf("SetUserLocale after enabling: %v", err)
	} else if got != "de" {
		t.Fatalf("SetUserLocale returned %q, want the normalised code", got)
	}

	// The chain then resolves the stored preference ahead of a header asking for something else,
	// which is the behaviour the write exists to make reachable. The preference is read back from
	// the column rather than reused from the call, so this is the round trip and not the argument.
	stored, err := s.UserLocale(ctx, userID)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	resolved, err := s.Resolve(ctx, stored, "fr-FR,fr;q=0.9")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if resolved.Locale != "de" || resolved.Source != i18n.SourcePreference {
		t.Fatalf("resolved %q from %q, want de from the stored preference", resolved.Locale, resolved.Source)
	}

	// Clearing is a write too, and it is how a customer goes back to their browser's guess.
	if _, err := s.SetUserLocale(ctx, userID, ""); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if stored, err = s.UserLocale(ctx, userID); err != nil || stored != "" {
		t.Fatalf("after clearing: stored locale = %q, %v; want empty", stored, err)
	}
	resolved, err = s.Resolve(ctx, stored, "fr-FR")
	if err != nil {
		t.Fatalf("resolve after clearing: %v", err)
	}
	if resolved.Source == i18n.SourcePreference {
		t.Fatalf("resolved %q from the preference after clearing", resolved.Locale)
	}
}
