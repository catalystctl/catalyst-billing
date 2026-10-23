package settings_test

// The mail-branding group (NTF-11, I18N-06).
//
// The keys are declared here rather than in the big definitions table, so these assertions are
// about the *declaration*: that the group exists at all, that the defaults keep a mail working
// (branding on, nothing hard-coded), and that the two values which reach a renderer — the accent
// and the logo URL — cannot carry something a mail client would interpret as anything but a colour
// and an address.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/catalyst/billing/internal/settings"
	"github.com/catalyst/billing/internal/store/storetest"
)

// The group is declared and a fresh installation reads a working default for every key.
func TestMailBrandGroupIsDeclaredWithWorkingDefaults(t *testing.T) {
	pool := storetest.DB(t)
	values, err := settings.New(pool).Group(context.Background(), "mail")
	if err != nil {
		t.Fatalf("read mail group: %v", err)
	}
	byKey := map[string]string{}
	for _, v := range values {
		byKey[v.Key] = v.Value
	}
	for _, key := range []string{"brand_html", "brand_header", "brand_footer", "brand_accent", "brand_logo_url"} {
		if _, ok := byKey[key]; !ok {
			t.Fatalf("mail group is missing %q", key)
		}
	}
	// Branding is on by default, and the words come from settings the operator already has: a
	// default header of "Catalyst Billing" would put this repository's demo name in a mail.
	if byKey["brand_html"] != "true" {
		t.Fatalf("brand_html default = %q, want true", byKey["brand_html"])
	}
	if byKey["brand_header"] != "" || byKey["brand_footer"] != "" {
		t.Fatalf("header/footer defaulted to text: %q / %q", byKey["brand_header"], byKey["brand_footer"])
	}
}

// The accent accepts a hex colour or nothing, and nothing else.
func TestMailBrandAccentIsAValidatedColour(t *testing.T) {
	pool := storetest.DB(t)
	store := settings.New(pool)
	ctx := context.Background()

	for _, bad := range []string{"rgb(1,2,3)", "#12345", "blue", "#3b5bdg"} {
		_, err := store.Update(ctx, "mail", map[string]string{"brand_accent": bad}, "", nil)
		if !errors.Is(err, settings.ErrInvalidValue) {
			t.Fatalf("brand_accent=%q was accepted (err %v)", bad, err)
		}
	}
	if _, err := store.Update(ctx, "mail", map[string]string{"brand_accent": "#0F766E"}, "", nil); err != nil {
		t.Fatalf("a valid accent was refused: %v", err)
	}
	// Empty is "not configured", which is how an operator removes the override.
	if _, err := store.Update(ctx, "mail", map[string]string{"brand_accent": ""}, "", nil); err != nil {
		t.Fatalf("clearing the accent was refused: %v", err)
	}
}

// The logo is an absolute http(s) URL or empty: a relative path cannot be fetched by a mail client,
// and any other scheme is not an image reference.
func TestMailBrandLogoMustBeAnAbsoluteHTTPURL(t *testing.T) {
	pool := storetest.DB(t)
	store := settings.New(pool)
	ctx := context.Background()

	for _, bad := range []string{
		"/logo.png",
		"javascript:alert(1)",
		"data:image/png;base64,AAAA",
		"ftp://example.test/logo.png",
		"https://",
	} {
		_, err := store.Update(ctx, "mail", map[string]string{"brand_logo_url": bad}, "", nil)
		if !errors.Is(err, settings.ErrInvalidValue) {
			t.Fatalf("brand_logo_url=%q was accepted (err %v)", bad, err)
		}
	}
	if _, err := store.Update(ctx, "mail",
		map[string]string{"brand_logo_url": "https://cdn.example.test/logo.png"}, "", nil); err != nil {
		t.Fatalf("a valid logo URL was refused: %v", err)
	}
}

// The header shares the panel name's rule, so a mail cannot be made unreadable by its own header.
func TestMailBrandHeaderIsBoundedAndControlFree(t *testing.T) {
	pool := storetest.DB(t)
	store := settings.New(pool)
	ctx := context.Background()

	long := strings.Repeat("x", settings.MaxBrandName+1)
	if _, err := store.Update(ctx, "mail", map[string]string{"brand_header": long}, "", nil); !errors.Is(err, settings.ErrInvalidValue) {
		t.Fatalf("an over-long header was accepted (err %v)", err)
	}
	if _, err := store.Update(ctx, "mail", map[string]string{"brand_header": "line\r\nBcc: x"}, "", nil); !errors.Is(err, settings.ErrInvalidValue) {
		t.Fatalf("a header with a control character was accepted (err %v)", err)
	}
}
