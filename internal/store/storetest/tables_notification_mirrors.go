package storetest

// The Discord mirror marker (migration 0068).
//
// It is state a test creates: a row says "this event instance was already posted", and a test
// that leaves one behind would make the next test's "the drain posts this event" assertion depend
// on execution order — the drain would find the marker and post nothing.
//
// `notification_deliveries.body_html` needs no entry here: the column belongs to a table the
// harness already truncates.
func init() {
	RegisterTables("notification_mirrors")
}
