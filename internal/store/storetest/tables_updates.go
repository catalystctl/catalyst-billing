package storetest

// Table classification for the updates, telemetry and marketplace tables (0042).
//
// This file exists because `storetest.go` is shared, and a feature that adds a table must be
// able to classify it without editing the shared list — that shared edit is where parallel
// work collides. Registering from an init in this package rather than from a `_test.go` file
// in the feature's own package matters: `Truncate` runs the classification check in *every*
// test binary that uses the harness, so a table classified only in one package's tests would
// still fail every other package's first truncate with "neither truncated nor declared
// reference data".
//
// All eight are state a test creates or edits:
//
//	update_config, marketplace_config      the operator's configuration, one row each
//	update_check, update_approval          the cached check and the approved version
//	marketplace_index                      the cached listing
//	installed_extensions, installed_extension_artifacts   a verified install
//	telemetry_sends                        what a test sent
//
// The two configuration tables are truncated even though 0042 seeds a default row, because
// both reference `users` and `TRUNCATE ... CASCADE` reaches them anyway whenever a test
// truncates `users`. Rather than pretend otherwise, the stores recreate the default row on
// read — "a missing row is a default, not an error", the rule `internal/settings` already
// follows. A test therefore starts from the state a fresh installation has, and a test that
// edits the configuration cannot leak it into the next one.
//
// Order is children first, matching the convention in storetest.go, so a plain TRUNCATE would
// work even without CASCADE.
func init() {
	RegisterTables(
		"installed_extension_artifacts",
		"installed_extensions",
		"telemetry_sends",
		"update_approval",
		"update_check",
		"marketplace_index",
		"update_config",
		"marketplace_config",
	)
}
