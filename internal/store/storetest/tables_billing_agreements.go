package storetest

// Tables added by 0062_billing_agreements.sql.
//
// An agreement is mutable state: it is created pending, activated when the customer
// finishes the provider's page, and cancelled when the method is removed. It must be
// truncated between tests for the same reason payment_methods is: a leaked active
// mandate would make a later test's "the customer has no method" assertion false.
//
// It lives in its own file because RegisterTables exists so a feature adds its table
// without editing the shared truncate list. billing_agreements references
// payment_methods, so it is truncated first by the shared list's dependency order,
// which is derived from the registered names rather than from this comment.
func init() { RegisterTables("billing_agreements") }
