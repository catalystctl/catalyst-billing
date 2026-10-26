package theme

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Diff.
//
// # Why a structured diff rather than two documents side by side
//
// The history screen has to answer "what changed between the revision I published and the one I am
// about to publish", and the answer has to be readable: "color.dark.bg: #171413 → #0e1116", not two
// collapsed JSON blobs. Flattening both documents to `path → value` and comparing makes that a
// table, and it makes "nothing changed" a fact rather than a judgement.
//
// The paths are the JSON keys an operator sees in the export envelope, so a diff line can be pasted
// into an import or matched against the document editor's field names.

// Change is one difference between two documents.
type Change struct {
	// Path is the dotted JSON path, e.g. `color.dark.bg`.
	Path string `json:"path"`
	// From is the previous value rendered as JSON (a quoted string, a number, an object), or
	// empty when the field did not exist.
	From string `json:"from"`
	// To is the new value, or empty when the field was removed.
	To string `json:"to"`
}

// Diff returns the differences between two documents, ordered by path.
//
// Both are flattened through the same JSON representation the merge uses, so a difference the merge
// would not see is a difference Diff does not report either — which is the property that makes the
// preview trustworthy.
func Diff(before, after Document) ([]Change, error) {
	beforeMap, err := toMap(before)
	if err != nil {
		return nil, err
	}
	afterMap, err := toMap(after)
	if err != nil {
		return nil, err
	}

	beforeFlat := flatten("", beforeMap, map[string]string{})
	afterFlat := flatten("", afterMap, map[string]string{})

	// The union of both key sets, sorted, so the diff is stable across runs.
	seen := make(map[string]struct{}, len(beforeFlat)+len(afterFlat))
	for k := range beforeFlat {
		seen[k] = struct{}{}
	}
	for k := range afterFlat {
		seen[k] = struct{}{}
	}
	paths := make([]string, 0, len(seen))
	for k := range seen {
		paths = append(paths, k)
	}
	sort.Strings(paths)

	var out []Change
	for _, path := range paths {
		from, fromOK := beforeFlat[path]
		to, toOK := afterFlat[path]
		if fromOK && toOK && from == to {
			continue
		}
		out = append(out, Change{Path: path, From: from, To: to})
	}
	return out, nil
}

// flatten renders a nested object as `path → JSON value` for every leaf.
//
// An empty object is a leaf (`{}`) so that "the field was present but empty" and "the field was
// absent" are different diffs — which they are, because only one of them inherits.
func flatten(prefix string, node map[string]any, out map[string]string) map[string]string {
	if len(node) == 0 {
		if prefix != "" {
			out[prefix] = "{}"
		}
		return out
	}
	for key, value := range node {
		path := key
		if prefix != "" {
			path = prefix + "." + key
		}
		switch typed := value.(type) {
		case map[string]any:
			flatten(path, typed, out)
		default:
			out[path] = render(value)
		}
	}
	return out
}

// render formats a leaf value the way an operator reads it.
func render(value any) string {
	if s, ok := value.(string); ok {
		// A quoted string is unambiguous: `none` (a shadow) and `"none"` (a string) are different
		// values, and an unquoted rendering would make them look the same.
		return `"` + s + `"`
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return fmt.Sprintf("%v", value)
	}
	return string(raw)
}

// DiffSummary renders a diff as one line, for an audit row or a confirmation dialog.
//
// It names the count and the first few paths, because "12 changes" alone does not tell an operator
// whether they are about to publish a palette or a whole layout.
func DiffSummary(changes []Change) string {
	if len(changes) == 0 {
		return "no change"
	}
	paths := make([]string, 0, len(changes))
	for _, c := range changes {
		paths = append(paths, c.Path)
	}
	if len(paths) > 5 {
		return fmt.Sprintf("%d changes: %s and %d more",
			len(paths), strings.Join(paths[:5], ", "), len(paths)-5)
	}
	return fmt.Sprintf("%d change(s): %s", len(paths), strings.Join(paths, ", "))
}
