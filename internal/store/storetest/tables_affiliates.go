// Package storetest's truncate list, extended by the affiliate payout work (0043).
//
// The harness refuses to run when a base table exists that it does not know about, because
// an unclassified table leaks rows between tests and presents as an impossible intermittent
// failure. That check runs inside every test binary that calls storetest.DB, and each
// package has its own database, so a table added by a migration has to be registered in a
// way every binary sees. RegisterTables is the sanctioned mechanism for exactly this: it
// appends to the list at init time, so a feature can classify its own tables without
// editing the shared literal in storetest.go, which is where parallel work collides.
//
// It lives in this package rather than in the feature's package because a registration made
// from internal/growth would only reach the test binaries that import internal/growth, and
// the other twenty-odd packages that use the harness would then fail the classification
// check. Adding a table is therefore a decision every package has to see.
//
// Children before parents: both tables are referenced by, and reference, tables already in
// the list, and Truncate uses a single TRUNCATE ... CASCADE, so ordering here is not load
// bearing — but the two are listed before `affiliates` to keep the "children first" reading
// of the list intact.
package storetest

func init() {
	RegisterTables(
		"affiliate_payouts",
		"affiliate_recurring_commissions",
	)
}
