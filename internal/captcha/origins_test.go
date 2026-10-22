package captcha_test

import (
	"strings"
	"testing"

	"github.com/catalyst/billing/internal/captcha"
)

// The origin list is what the CSP is widened by, so it must be exactly the hosts a widget needs
// and nothing more. A list that is too short breaks the widget; one that is too long hands a
// third party an origin it never asked for.
func TestChallengeOriginsCoverEveryProviderAndNothingElse(t *testing.T) {
	cases := map[captcha.Provider][]string{
		captcha.ProviderTurnstile: {"https://challenges.cloudflare.com"},
		// reCAPTCHA frames from www.google.com and loads its script from www.gstatic.com.
		captcha.ProviderRecaptcha: {"https://www.google.com", "https://www.gstatic.com"},
		// hCaptcha loads from js.hcaptcha.com, frames newassets, and its widget calls api.
		captcha.ProviderHCaptcha: {
			"https://js.hcaptcha.com", "https://newassets.hcaptcha.com", "https://api.hcaptcha.com",
		},
		// No provider: no widening at all. This is the case most installations are in.
		captcha.ProviderNone: nil,
	}
	for provider, want := range cases {
		got := captcha.ChallengeOrigins(provider)
		if len(got) != len(want) {
			t.Fatalf("%s: origins = %v, want %v", provider, got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("%s: origins = %v, want %v", provider, got, want)
			}
		}
		for _, origin := range got {
			if !strings.HasPrefix(origin, "https://") {
				t.Fatalf("%s: origin %q is not https", provider, origin)
			}
			// A wildcard or a bare host would widen far more than intended.
			if strings.Contains(origin, "*") || !strings.HasPrefix(origin, "https://") {
				t.Fatalf("%s: origin %q is not a specific https origin", provider, origin)
			}
		}
	}

	// An unknown provider is not a reason to widen anything.
	if got := captcha.ChallengeOrigins(captcha.Provider("something-else")); got != nil {
		t.Fatalf("an unknown provider produced origins %v", got)
	}
}
