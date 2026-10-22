package storetest

// The web push subscription table (migration 0034), classified as mutable state: a test must
// start with no devices registered, and a row leaked from one test into the next presents as an
// impossible intermittent failure.
//
// # Why this is a file in this package rather than an init in the feature
//
// RegisterTables exists so a feature can classify its table without editing the shared list, and
// for most tables that is exactly right. It is not sufficient for this one, because DB(t) is the
// only entry point to the harness and an init in a feature package is only linked into a test
// binary that imports that package. internal/store's own schema tests do not — they import this
// package directly — so a registration living in internal/httpapi or internal/notify would leave
// them failing the classification check, which is a red build for a table they never touch.
//
// A registration here is linked into every test binary that uses the harness, which is every
// test binary that touches the database. It is a separate file rather than an edit to
// storetest.go so that two packages adding two tables in parallel do not collide on one line —
// the same reason RegisterTables exists at all.
func init() { RegisterTables("push_subscriptions") }
