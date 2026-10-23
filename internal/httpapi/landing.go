package httpapi

import "github.com/catalyst/billing/internal/authn"

// landingPath is where a signed-in account belongs in the browser.
//
// It restates the frontend's `homeFor` (web/src/lib/session.ts) because exactly one redirect is
// decided on the server: a completed social sign-in answers with a 302, so the browser never
// passes through the client-side guard that would otherwise pick the destination.
//
// The rule has to exist at all because `/` is not a person's home any more — it is the public
// storefront. A hard-coded "/" here (which is what this replaced) dropped a freshly signed-in
// operator or customer on the product list with no sign that the sign-in had worked.
func landingPath(u authn.User) string {
	if u.IsStaff() {
		return "/dashboard"
	}
	return "/portal"
}
