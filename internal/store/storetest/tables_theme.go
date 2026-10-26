// Table classification for the theme registry (0073).
//
// They live in their own file for the reason RegisterTables exists: a feature that adds a table
// classifies it in its own file instead of editing one shared list, which is where parallel work
// collides.
package storetest

func init() {
	// Mutable state a test must start from empty. Children before parents so a plain TRUNCATE
	// (which this harness runs with CASCADE) leaves nothing behind.
	RegisterTables(
		"theme_publications",
		"theme_assignments",
		"theme_revisions",
		"theme_assets",
		"themes",
	)
}
