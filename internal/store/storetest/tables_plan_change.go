package storetest

// Tables added by 0064_credit_lifecycle.sql.
//
// They are registered from a file in this package rather than from a _test.go in internal/service
// because the truncate list is per *test binary*: every package that calls DB(t) builds its own
// list from the init functions linked into that binary, so a registration in one package's tests
// would leave the other packages failing the table-classification check.
//
// service_upgrade_configs is the option selection a pending plan change carries (UPG-02). It is
// mutable state, created by a customer's request and deleted when the change is applied or
// cancelled, so it is truncated between tests like every other row this product creates.
//
// The migration adds no new settings *table*; `settings` is already classified as configuration,
// and the row it seeds for credits.credit_on_downgrade is restored by the harness like the rest of
// the settings seed.
func init() { RegisterTables("service_upgrade_configs") }
