package storetest

// The tax-rate import tables (migration 0055).
//
// They live in their own file for the reason RegisterTables exists: a feature that adds a table
// classifies it here instead of editing one shared list, which is where parallel work collides. The
// registration is in this package rather than in the feature's test file because every test binary
// imports storetest and only some of them import the feature — a table registered from
// `internal/tax`'s tests is invisible to `internal/store`'s, which then fails the classification
// check for a table it has never heard of.
//
// Both are mutable state a test creates: a configured source and the runs that used it. Neither is
// seeded by a migration, so neither belongs in ReferenceTables.
func init() {
	RegisterTables("tax_import_runs", "tax_import_sources")
}
