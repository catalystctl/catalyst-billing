package notify

// The in-app inbox.
//
// The outbox answers "did the mail go out"; this answers "what has this customer been told".
// They are separate tables because they are separate promises: an inbox row is part of the
// product surface (it is read, it has a read flag, it can be deleted), while a delivery row
// is an operations record that the retention policy eventually drops. A customer's inbox
// must not disappear because the mail log aged out.
//
// The write rule is the same as the outbox's: the row is inserted in the transaction that
// made the change. A crash between "the payment was applied" and "the inbox row was written"
// would otherwise leave a customer who was charged and never told, which is precisely the
// support ticket this feature exists to prevent.
//
// # Dedupe
//
// A provider retries a webhook; a job re-runs after a partial failure; a caller replays a
// request with the same idempotency key. All three must produce one inbox row, not three.
// The unique index on (user_id, dedupe_key) makes that a database guarantee rather than a
// caller's discipline, exactly as notification_deliveries.dedupe_key does for the mail.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/catalyst/billing/internal/paginate"
	"github.com/jackc/pgx/v5"
)

// Notification is one inbox row.
type Notification struct {
	ID        string
	UserID    string
	EventKey  string
	Title     string
	Body      string
	URL       string
	ReadAt    *time.Time
	CreatedAt time.Time
	// DedupeKey is the replay guard. It is not returned to the client.
	DedupeKey string
}

// Unread reports whether the row has been read.
func (n Notification) Unread() bool { return n.ReadAt == nil }

// Create writes one inbox row inside the caller's transaction.
//
// The second return value is false when an identical event already produced a row, which is
// not an error: "already there" and "written" are the same outcome for the caller, exactly
// like Queue.Enqueue's `created` result.
func Create(ctx context.Context, q Querier, n Notification) (bool, error) {
	if n.UserID == "" {
		return false, errors.New("notify: notification has no user")
	}
	if n.EventKey == "" || n.Title == "" {
		return false, errors.New("notify: notification is incomplete")
	}
	var key any
	if n.DedupeKey != "" {
		key = n.DedupeKey
	}
	var id string
	err := q.QueryRow(ctx, `
		INSERT INTO notifications (user_id, event_key, title, body, url, dedupe_key)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (user_id, dedupe_key) WHERE dedupe_key IS NOT NULL DO NOTHING
		RETURNING id`,
		n.UserID, n.EventKey, n.Title, n.Body, n.URL, key).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("notify: create notification: %w", err)
	}
	return true, nil
}

// InboxFilter narrows a list.
type InboxFilter struct {
	// UnreadOnly returns only rows with no read_at.
	UnreadOnly bool
}

// List returns one page of a user's notifications, newest first, and the cursor for the next
// page.
//
// Every query in this file takes the user id from the caller and puts it in the WHERE
// clause. That is the security property, not a detail: the HTTP layer passes the session's
// user id, so a request naming somebody else's notification id matches no row and returns
// 404 rather than being refused after the fact.
func List(ctx context.Context, q Querier, userID string, f InboxFilter, limit int, cursor string) ([]Notification, string, error) {
	if userID == "" {
		return nil, "", errors.New("notify: list needs a user")
	}
	limit = clampLimit(limit)

	args := []any{userID}
	where := "user_id = $1"
	if f.UnreadOnly {
		where += " AND read_at IS NULL"
	}
	if cursor != "" {
		when, id, err := decodeInboxCursor(cursor)
		if err != nil {
			return nil, "", err
		}
		args = append(args, when, id)
		where += fmt.Sprintf(" AND (created_at, id) < ($%d, $%d)", len(args)-1, len(args))
	}
	args = append(args, limit+1)

	rows, err := q.Query(ctx, `
		SELECT id, user_id, event_key, title, body, url, read_at, created_at,
		       COALESCE(dedupe_key, '')
		FROM notifications
		WHERE `+where+`
		ORDER BY created_at DESC, id DESC
		LIMIT $`+fmt.Sprint(len(args)), args...)
	if err != nil {
		return nil, "", fmt.Errorf("notify: list notifications: %w", err)
	}
	defer rows.Close()

	out := make([]Notification, 0, limit)
	for rows.Next() {
		var n Notification
		if err := rows.Scan(&n.ID, &n.UserID, &n.EventKey, &n.Title, &n.Body, &n.URL,
			&n.ReadAt, &n.CreatedAt, &n.DedupeKey); err != nil {
			return nil, "", fmt.Errorf("notify: scan notification: %w", err)
		}
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("notify: read notifications: %w", err)
	}

	next := ""
	if len(out) > limit {
		out = out[:limit]
		last := out[len(out)-1]
		next = encodeInboxCursor(last.CreatedAt, last.ID)
	}
	return out, next, nil
}

// UnreadCount is the number the badge shows.
func UnreadCount(ctx context.Context, q Querier, userID string) (int, error) {
	if userID == "" {
		return 0, nil
	}
	var n int
	err := q.QueryRow(ctx,
		`SELECT count(*) FROM notifications WHERE user_id = $1 AND read_at IS NULL`,
		userID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("notify: unread count: %w", err)
	}
	return n, nil
}

// ErrNoNotification is returned when a notification id does not belong to the caller.
//
// It is deliberately the same error whether the row does not exist or belongs to somebody
// else. Distinguishing them would make the endpoint an oracle for "does this id exist",
// which is a small leak but a free one to avoid.
var ErrNoNotification = errors.New("notify: no such notification")

// MarkRead marks one notification read.
//
// Marking an already-read row is a no-op that still succeeds: the caller's intent
// ("it is read") is satisfied, and an error there would make a double click look like a
// failure. The UPDATE is scoped by user_id, so an id belonging to another account affects
// zero rows and returns ErrNoNotification.
func MarkRead(ctx context.Context, q Querier, userID, notificationID string) error {
	tag, err := q.Exec(ctx, `
		UPDATE notifications
		SET read_at = COALESCE(read_at, now())
		WHERE id = $1 AND user_id = $2`, notificationID, userID)
	if err != nil {
		return fmt.Errorf("notify: mark read: %w", err)
	}
	if tag.RowsAffected() == 0 {
		// Either no such row, or it belongs to someone else. Both answer 404.
		//
		// A malformed id would make this statement fail rather than match nothing, so the
		// handler validates the shape before calling; see httpapi.notificationID.
		return ErrNoNotification
	}
	return nil
}

// MarkAllRead marks every unread notification read and returns how many changed.
//
// It is one statement rather than a loop over ids because a "mark all read" that partially
// fails leaves a badge showing a number the user already dismissed.
func MarkAllRead(ctx context.Context, q Querier, userID string) (int, error) {
	tag, err := q.Exec(ctx, `
		UPDATE notifications
		SET read_at = now()
		WHERE user_id = $1 AND read_at IS NULL`, userID)
	if err != nil {
		return 0, fmt.Errorf("notify: mark all read: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// Delete removes one notification, scoped to its owner.
//
// Not exposed by the API yet; it exists because "mark read" is not "delete" and a future
// inbox screen will want both.
func Delete(ctx context.Context, q Querier, userID, notificationID string) error {
	tag, err := q.Exec(ctx,
		`DELETE FROM notifications WHERE id = $1 AND user_id = $2`, notificationID, userID)
	if err != nil {
		return fmt.Errorf("notify: delete notification: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNoNotification
	}
	return nil
}

// ------------------------------------------------------------------ cursor --

// The inbox uses the same opaque keyset cursor as every other list endpoint, so the client
// has one "load more" implementation. The sort key is created_at; the tie-breaker is the id,
// because two notifications can share a timestamp when one event fans out to several rows.

// encodeInboxCursor renders the position of the last row on a page.
func encodeInboxCursor(createdAt time.Time, id string) string {
	return paginate.Encode(createdAt.UTC().Format(time.RFC3339Nano), id)
}

// decodeInboxCursor parses one, reporting paginate.ErrInvalidCursor rather than silently
// restarting at page one: a silent restart makes a client loop forever.
func decodeInboxCursor(cursor string) (time.Time, string, error) {
	sortKey, id, err := paginate.Decode(cursor)
	if err != nil {
		return time.Time{}, "", err
	}
	if sortKey == "" {
		return time.Time{}, "", fmt.Errorf("%w: missing timestamp", paginate.ErrInvalidCursor)
	}
	when, err := time.Parse(time.RFC3339Nano, sortKey)
	if err != nil {
		return time.Time{}, "", fmt.Errorf("%w: bad timestamp", paginate.ErrInvalidCursor)
	}
	return when, id, nil
}

// clampLimit applies the shared default and ceiling.
func clampLimit(limit int) int { return paginate.Clamp(limit) }
