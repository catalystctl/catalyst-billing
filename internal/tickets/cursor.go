package tickets

import (
	"fmt"
	"time"

	"github.com/catalyst/billing/internal/paginate"
)

// Cursors.
//
// Lists are keyset-paginated on (created_at, id). OFFSET would repeat and skip rows
// when a customer opens a ticket while an operator scrolls, and the skipped row is a
// real support request. The cursor is opaque and carries no authority: the query it
// feeds is already scoped by the caller's identity or permission.

// encodeCursor renders the position after a row.
func encodeCursor(createdAt time.Time, id string) string {
	return paginate.Encode(createdAt.UTC().Format(time.RFC3339Nano), id)
}

// decodeCursor parses a cursor into its sort key and id.
func decodeCursor(cursor string) (time.Time, string, error) {
	sortKey, id, err := paginate.Decode(cursor)
	if err != nil {
		return time.Time{}, "", err
	}
	when, err := time.Parse(time.RFC3339Nano, sortKey)
	if err != nil {
		return time.Time{}, "", fmt.Errorf("%w: bad timestamp in cursor", paginate.ErrInvalidCursor)
	}
	return when, id, nil
}
