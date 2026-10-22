package captcha

// ChallengeOrigins returns the extra origins a provider's widget needs in the panel's
// Content-Security-Policy: the host its script is loaded from, and the host it frames.
//
// It exists so the policy is widened by exactly one provider's origins at startup, from the same
// setting the verifier reads. A provider that is switched off leaves the policy untouched, and the
// widening is never derived from a request — a CSP that a caller can influence is not a policy.
//
// The script host and the frame host differ for two of the three providers, which is why this
// returns a list rather than a single origin: reCAPTCHA loads from www.google.com and frames
// www.google.com, hCaptcha loads from js.hcaptcha.com and frames newassets.hcaptcha.com, and
// Turnstile does both from challenges.cloudflare.com.
func ChallengeOrigins(p Provider) []string {
	switch p {
	case ProviderTurnstile:
		return []string{"https://challenges.cloudflare.com"}
	case ProviderRecaptcha:
		return []string{"https://www.google.com", "https://www.gstatic.com"}
	case ProviderHCaptcha:
		return []string{"https://js.hcaptcha.com", "https://newassets.hcaptcha.com", "https://api.hcaptcha.com"}
	default:
		// No provider: no widening. Returning an empty list rather than a default set is the
		// point — a shop that has not configured a challenge keeps the strictest policy.
		return nil
	}
}
