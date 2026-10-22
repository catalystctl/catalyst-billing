package storetest

// Table classification for the OAuth2 and social-login tables (migration 0038).
//
// It lives in this package rather than in internal/oauth because the harness's own package
// tests — internal/store — do not import any feature package, so a registration that only
// linked from internal/oauth would leave `go test ./internal/store/` failing its
// table-classification check. A new file per feature keeps the shared list untouched, which is
// the point of RegisterTables: two people adding two tables do not edit one line.
//
// `oauth_signing_keys` is reference data: 0038 seeds it and it must survive truncation, because
// a signing key that vanished between tests would make every social-login state unverifiable and
// the failure would look like a bug in the signature check.
func init() {
	RegisterTables(
		// Children before parents; Truncate cascades, but the order documents the shape.
		"oauth_access_tokens",
		"oauth_grants",
		"oauth_auth_codes",
		"oauth_consents",
		"social_login_states",
		"social_identities",
		"social_providers",
		"oauth_clients",
	)
	RegisterReferenceTables("oauth_signing_keys")
}
