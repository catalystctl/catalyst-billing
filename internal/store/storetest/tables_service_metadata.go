package storetest

// Tables added by 0029_service_metadata.sql.
//
// They are registered from a file in this package rather than from a _test.go in
// internal/service because the truncate list is per *test binary*: every package that calls
// DB(t) builds its own list from the init functions linked into that binary, so a
// registration in one package's tests would leave the other twenty-eight packages failing the
// table-classification check.
//
// service_property_definitions is mutable state an operator creates — a property definition,
// its label, whether it prints on a document — so it is truncated between tests.
// `services.custom_properties` is a column on a table that is already in the list, and needs
// no entry of its own.
func init() { RegisterTables("service_property_definitions") }
