package httpapi

import (
	"net/http"

	"github.com/catalyst/billing/internal/paginate"
)

// Page is the response envelope for every list endpoint.
//
// One shape for all lists means the client has one list component, one loading state
// and one "load more" affordance, instead of one per screen.
type Page[T any] struct {
	Data       []T     `json:"data"`
	NextCursor *string `json:"next_cursor"`
}

// ParsePage reads the pagination parameters of a request.
//
// limit defaults to 25 and is clamped to 100. cursor is empty for the first page. An
// error is returned for a malformed limit or cursor; the caller answers 400
// invalid_cursor, never a silent reset to page one.
func ParsePage(r *http.Request) (limit int, cursor string, err error) {
	q := r.URL.Query()
	limit, err = paginate.ParseLimit(q.Get("limit"))
	if err != nil {
		return 0, "", err
	}
	cursor = q.Get("cursor")
	if _, _, err := paginate.Decode(cursor); err != nil {
		return 0, "", err
	}
	return limit, cursor, nil
}

// EncodeCursor renders an opaque cursor for (sortKey, id).
func EncodeCursor(sortKey, id string) string { return paginate.Encode(sortKey, id) }

// DecodeCursor parses an opaque cursor.
func DecodeCursor(c string) (sortKey, id string, err error) { return paginate.Decode(c) }

// newPage builds a page response, computing the next cursor only when the page was
// full.
func newPage[T any](rows []T, limit int, sortKey func(T) string, id func(T) string) Page[T] {
	if rows == nil {
		rows = []T{}
	}
	return Page[T]{Data: rows, NextCursor: paginate.Next(rows, limit, sortKey, id)}
}

// writePage writes a page response.
func writePage[T any](w http.ResponseWriter, p Page[T]) {
	writeJSON(w, http.StatusOK, p)
}
