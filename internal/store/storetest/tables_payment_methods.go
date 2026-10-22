package storetest

// Tables added by 0036_payment_methods.sql.
//
// A saved payment method is mutable state a customer creates, replaces and deletes, so it is
// truncated between tests. It is registered from a file in this package rather than from a
// _test.go in internal/payments because the truncate list is per *test binary*: every package
// that calls DB(t) builds its own list from the init functions linked into that binary, so a
// registration in one package's tests would leave every other package failing the
// table-classification check.
//
// That check is not busywork. A table nobody classifies is either mutable state leaking between
// tests or reference data being truncated, and both failures are silent: the second one shows up
// as a test that passes alone and fails in a suite, which is the most expensive kind of bug to
// diagnose after the fact.
func init() { RegisterTables("payment_methods") }
