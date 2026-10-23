package settings

// Payment-depth settings.
//
// They live in their own file because the shared settings slice is a merge hotspot: a
// feature that owns a group registers it from its own init, and Register panics on a
// duplicate at startup rather than letting one declaration silently win.
//
// `saved_cards_enabled` is the adapter's own kill switch for the billing-agreement surface.
// It is a stored value, not an environment-only knob, because the operator who configures
// Stripe in the panel must be able to turn saved cards off there too. gatewayconfig resolves
// it into STRIPE_SAVED_CARDS, which is the environment entry the adapter starts with; the
// child inherits exactly that and PATH, nothing else.
func init() {
	Register(Definition{
		Group:   "gateway",
		Key:     "saved_cards_enabled",
		Kind:    KindBool,
		Default: "true",
	})
}
