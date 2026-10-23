package httpapi

import (
	"testing"

	"github.com/catalyst/billing/internal/authn"
)

// A social sign-in is the one place the server, not the client, chooses where a signed-in user
// lands. It used to redirect to "/", which was "the panel for staff and the portal for a customer"
// only because the browser decided that — now "/" is the public storefront, so a hard-coded "/"
// would leave a user who just authenticated looking at the product list.
func TestLandingPathSendsEachAudienceHome(t *testing.T) {
	staff := authn.User{ID: "u1", Email: "admin@example.test", Roles: []string{"administrator"}}
	customer := authn.User{ID: "u2", Email: "customer@example.test"}

	if got := landingPath(staff); got != "/dashboard" {
		t.Errorf("staff land on %q, want /dashboard: the panel is not the shop", got)
	}
	if got := landingPath(customer); got != "/portal" {
		t.Errorf("customer lands on %q, want /portal", got)
	}
	// The specific regression, asserted directly: nobody lands on the storefront after signing in.
	for name, user := range map[string]authn.User{"staff": staff, "customer": customer} {
		if got := landingPath(user); got == "/" {
			t.Errorf("%s lands on /, which is the public storefront", name)
		}
	}
}

// An account with a role is staff, and a role list that is nil is a customer. That is the same
// rule `is_staff` follows everywhere else; this pins it at the redirect, where getting it backwards
// would send a customer to a panel that answers 403 on every request.
func TestLandingPathTreatsNilRolesAsCustomer(t *testing.T) {
	if got := landingPath(authn.User{ID: "u3", Roles: nil}); got != "/portal" {
		t.Errorf("an account with no roles lands on %q, want /portal", got)
	}
	if got := landingPath(authn.User{ID: "u4", Roles: []string{}}); got != "/portal" {
		t.Errorf("an account with an empty role list lands on %q, want /portal", got)
	}
}
