// Package paginate implements the keyset cursor used by every list endpoint.
//
// Keyset, not OFFSET. An operator scrolling an invoice list while renewals are being
// created is not a rare case: OFFSET would repeat or skip rows, and the row it skips is
// a real invoice. A keyset cursor names the last row seen, so concurrent inserts cannot
// shift the window.
//
// The cursor is opaque to clients: base64url of "<sort key>\x00<id>". It is not signed,
// because it carries no authority — it is a position, and the query it feeds is already
// scoped by the caller's permissions. A forged cursor can only ask for a different page
// of rows the caller is already allowed to read.
package paginate

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// DefaultLimit is the page size when the client does not ask for one.
const DefaultLimit = 25

// MaxLimit is the largest page a client may request. A list endpoint that will return
// "everything" is a list endpoint that will one day return a million rows.
const MaxLimit = 100

// ErrInvalidCursor is returned for a cursor that cannot be decoded. The API turns this
// into 400 invalid_cursor rather than silently restarting at page one, because a silent
// restart makes a client loop forever over the first page.
var ErrInvalidCursor = errors.New("paginate: invalid cursor")

// separator cannot appear in a base64url-decoded sort key, so the split is unambiguous.
const separator = "\x00"

// Encode builds a cursor from the last row's sort key and id.
func Encode(sortKey, id string) string {
	raw := sortKey + separator + id
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// Decode parses a cursor.
func Decode(cursor string) (sortKey, id string, err error) {
	if cursor == "" {
		return "", "", nil
	}
	if len(cursor) > 512 {
		return "", "", fmt.Errorf("%w: too long", ErrInvalidCursor)
	}
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return "", "", fmt.Errorf("%w: not base64", ErrInvalidCursor)
	}
	parts := strings.SplitN(string(raw), separator, 2)
	if len(parts) != 2 || parts[1] == "" {
		return "", "", fmt.Errorf("%w: missing components", ErrInvalidCursor)
	}
	return parts[0], parts[1], nil
}

// Clamp applies the default and the maximum to a requested page size.
//
// A negative or zero limit is treated as "unspecified" rather than as an error: a UI
// that computes limit from a page size setting should not get a 400 when the setting is
// zero.
func Clamp(limit int) int {
	if limit <= 0 {
		return DefaultLimit
	}
	if limit > MaxLimit {
		return MaxLimit
	}
	return limit
}

// ParseLimit reads a limit from a query string value.
func ParseLimit(raw string) (int, error) {
	if strings.TrimSpace(raw) == "" {
		return DefaultLimit, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%w: limit is not a number", ErrInvalidCursor)
	}
	return Clamp(n), nil
}

// Next returns the cursor for the next page, or nil when this page is the last.
//
// The caller passes the limit it asked for and the number of rows it got: fewer rows
// than the limit means there is nothing after this page, and returning a cursor anyway
// would make every client perform one extra request that returns nothing.
func Next[T any](rows []T, limit int, sortKey func(T) string, id func(T) string) *string {
	if len(rows) < limit || len(rows) == 0 {
		return nil
	}
	last := rows[len(rows)-1]
	cursor := Encode(sortKey(last), id(last))
	return &cursor
}
