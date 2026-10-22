package storetest

// Table classification for 0037_i18n.sql.
//
// `locales` and `message_catalog` are *reference data*, not mutable state: the migration seeds
// `en` (the default) and `de` (disabled, fully translated), and a store whose default locale row
// has been truncated cannot write a translation at all — it answers "no such locale" for the one
// locale every installation is guaranteed to have.
//
// So they are never truncated, and Truncate restores the seeded rows instead, the same way it
// restores the chart of accounts and the retention policies. A test that enables `de` or edits a
// translation therefore starts from the migrated state rather than from whatever the previous
// test left behind.
func init() { RegisterReferenceTables("locales", "message_catalog") }
