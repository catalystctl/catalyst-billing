package authn

// Customer-baseline permission helpers.
//
// `customerPermissions` is the set every signed-in user holds, staff included: a support
// agent is also a person who can read their own invoice. Anything that wants to talk about
// a user's *staff* grants has to subtract this baseline, and doing that by enumerating the
// list in a test — which is what the role test used to do — silently breaks the moment a
// permission is added to it.
//
// These two functions exist so callers ask the question instead of copying the answer.

// IsCustomerPermission reports whether p is part of the baseline every signed-in user holds.
func IsCustomerPermission(p Permission) bool {
	for _, candidate := range customerPermissions {
		if candidate == p {
			return true
		}
	}
	return false
}

// CustomerPermissions returns the baseline permission set, sorted by declaration order, so a
// caller can render what a customer may do without holding the unexported slice.
func CustomerPermissions() []Permission {
	out := make([]Permission, len(customerPermissions))
	copy(out, customerPermissions)
	return out
}
