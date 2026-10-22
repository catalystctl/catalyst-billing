package storetest

// Tables added by 0039_imports.sql.
//
// Both are mutable state: an import run is created, stepped through, and its records written as it
// goes. They cannot be reference data, because the whole point of the tables is that an operator
// starts a run and the run accumulates rows.
//
// `import_records` references `import_runs`, and Truncate issues one TRUNCATE ... CASCADE over the
// whole list, so the order here does not matter — but it is written child-first anyway, so the
// list reads the way the schema does.
func init() { RegisterTables("import_records", "import_runs") }
