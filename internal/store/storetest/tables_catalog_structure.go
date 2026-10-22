package storetest

// Tables added by 0049_catalog_structure.sql.
//
// Registered from a file in this package rather than from a _test.go in internal/catalog for
// the same reason tables_service_metadata.go explains: the truncate list is per *test binary*,
// so a registration in one package's tests would leave every other package failing the
// table-classification check.
//
// `servers` is mutable state an operator creates — a provisioner target, its region and its
// node — so it is truncated between tests. The other half of that migration is two columns
// (`categories.parent_id`, `products.server_id`) on tables that are already in the list, and
// needs no entry of its own.
func init() { RegisterTables("servers") }
