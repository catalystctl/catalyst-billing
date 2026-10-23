package settings

// The provisioning-server credentials the `servers` group needs beyond the first provider's
// (SVC-18, SVC-19).
//
// The group itself and its Pterodactyl keys are declared in the main slice, because they were
// there first. A second adapter's credentials are registered from here so that adding a provider
// does not mean editing the one eight-hundred-line slice every other feature also edits — which is
// what Register exists for, and what a merge conflict in a settings table costs.
//
// # Why these keys are in the same group
//
// `servers` is one integration configured at a time: there is a single provisioner, and the
// credentials of the *configured* one are what the screen shows and what the child process
// receives. A group per provider would multiply the settings page by the number of adapters and
// would let two be configured at once, which the runtime does not support.
//
// # Why registering them is load-bearing rather than cosmetic
//
// `settings.Update` refuses an undeclared key, so before these definitions existed the WHM URL and
// token could not be saved from the panel at all. The adapter could still be launched by
// environment variables, which is why extensions/cpanel worked in its own tests and could not be
// configured by an operator.

func init() {
	Register(
		// A URL is stored as text and validated where it is used: the adapter is the only
		// component that knows whether it also needs a port, and a strict check here would
		// refuse a value the adapter would have accepted.
		Definition{Group: "servers", Key: "whm_url", Kind: KindString, Default: ""},
		// The token is a credential and is redacted on every read path. The column is also
		// persisted as secret, so a row written before this definition existed is still
		// redacted.
		Definition{Group: "servers", Key: "api_token", Kind: KindString, Default: "", Secret: true},
	)
}
