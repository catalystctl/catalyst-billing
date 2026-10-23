package storetest

// The tax-jurisdiction tables (migration 0066).
//
// They live in their own file for the reason RegisterTables exists: a feature that adds a table
// classifies it here instead of editing one shared list, which is where parallel work collides.
//
// Both are state a test creates. `tax_nexus` is configuration an operator declares, not data a
// migration seeds; `vat_id_validations` is the history of checks a test performs against its own
// fake validation service. A test that leaves either behind would make the next test's "no nexus
// declared" or "this VAT ID was never checked" assertion depend on execution order.
func init() {
	RegisterTables("tax_nexus", "vat_id_validations")
}
