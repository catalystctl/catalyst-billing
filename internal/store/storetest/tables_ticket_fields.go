package storetest

// Tables added by 0054_ticket_fields.sql.
//
// Registered from this package rather than from a _test.go in internal/tickets or internal/httpapi
// for the reason the other tables_*.go files give: the truncate list is per test *binary*, built
// from the init functions linked into it, so a registration in one package's tests would leave
// every other package failing the table-classification check.
//
// Both are mutable state an operator or a customer creates — a field definition, and a ticket's
// answer to it — so both are truncated between tests. `tickets.opened_by_staff` is a column on a
// table that is already in the list and needs no entry of its own.
func init() { RegisterTables("ticket_field_values", "ticket_fields") }
