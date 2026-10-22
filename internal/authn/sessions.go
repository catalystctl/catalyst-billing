package authn

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/catalyst/billing/internal/paginate"
	"github.com/jackc/pgx/v5"
)

// Session management (IDN-15, IDN-17).
//
// The session table already existed: a token is stored hashed, a session can be revoked,
// and a password change bumps credential_version so every older session dies. What was
// missing is the operator's and the customer's view of it — "where am I signed in, and
// how do I end that one" — and the login history.
//
// Two rules shape this file:
//
//   - Every query is scoped by user id in SQL, never by a check after the fact. A caller
//     asking for someone else's session must get ErrNoSession (the caller turns that into
//     404), which is indistinguishable from asking for one that never existed.
//   - Revoking the current session is allowed. "Sign out this device" is exactly the case
//     where the session being revoked is the one making the request, and refusing it
//     would be a bug the user experiences as "the button does nothing".

// ErrNoSessionRow is returned when a session id does not belong to the caller.
//
// It is deliberately distinct from ErrNoSession: that one means "this token does not
// authenticate", this one means "there is no such row for you". The HTTP layer maps both
// to 404 so the distinction never reaches a client.
var ErrNoSessionRow = errors.New("authn: no such session")

// SessionInfo is one row of the "where am I signed in" list.
//
// It never carries the token or the CSRF secret: the list is a description of sessions,
// not a way to obtain one.
type SessionInfo struct {
	ID         string
	CreatedAt  time.Time
	LastSeenAt time.Time
	ExpiresAt  time.Time
	// IP is the address the session was created from, or empty when unknown.
	IP string
	// UserAgent is the recorded agent, truncated at creation time.
	UserAgent string
	// Current marks the session that made the request.
	Current bool
	// ImpersonatedBy is the operator behind an impersonated session, empty otherwise.
	ImpersonatedBy string
	// UserID is the session's subject. It is what makes a staff view of another user's
	// sessions possible without a second query.
	UserID string
}

// Sessions lists a user's live sessions, newest first.
//
// currentID is the caller's own session; it is marked rather than filtered out, because
// "which of these is this browser" is the first thing the screen has to answer. Expired
// sessions are excluded: a row that can no longer authenticate is history, and it is
// already in the login history.
func (s *Service) Sessions(ctx context.Context, userID, currentID string) ([]SessionInfo, error) {
	now := s.cfg.Now()
	rows, err := s.pool.Query(ctx, `
		SELECT id, created_at, last_seen_at, expires_at,
		       COALESCE(host(ip), ''), user_agent,
		       COALESCE(impersonated_by::text, ''), user_id
		FROM sessions
		WHERE user_id = $1 AND revoked_at IS NULL AND expires_at > $2
		ORDER BY last_seen_at DESC, created_at DESC`, userID, now)
	if err != nil {
		return nil, fmt.Errorf("authn: list sessions: %w", err)
	}
	defer rows.Close()

	out := []SessionInfo{}
	for rows.Next() {
		var info SessionInfo
		if err := rows.Scan(&info.ID, &info.CreatedAt, &info.LastSeenAt, &info.ExpiresAt,
			&info.IP, &info.UserAgent, &info.ImpersonatedBy, &info.UserID); err != nil {
			return nil, fmt.Errorf("authn: scan session: %w", err)
		}
		info.Current = info.ID == currentID
		out = append(out, info)
	}
	return out, rows.Err()
}

// RevokeSession ends one session belonging to userID.
//
// Ownership is in the WHERE clause: a session belonging to someone else affects zero
// rows, which the caller reports as 404 exactly like an unknown id. The token is not
// needed, only the id, because this is the "log out that other device" path where the
// caller does not hold the other device's credential.
func (s *Service) RevokeSession(ctx context.Context, userID, sessionID string) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE sessions SET revoked_at = $3
		WHERE id = $1 AND user_id = $2 AND revoked_at IS NULL`,
		sessionID, userID, s.cfg.Now())
	if err != nil {
		return fmt.Errorf("authn: revoke session: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNoSessionRow
	}
	s.audit(ctx, userID, "session_revoked", sessionID, "")
	return nil
}

// RevokeOtherSessions ends every session but keepID and reports how many it ended.
//
// It reuses RevokeAll, which is the same statement "sign out everywhere" and a password
// change already use: a second implementation would be a second place for the keep-list
// to be wrong.
func (s *Service) RevokeOtherSessions(ctx context.Context, userID, keepID string) (int, error) {
	n, err := s.RevokeAll(ctx, userID, keepID)
	if err != nil {
		return 0, err
	}
	if n > 0 {
		s.audit(ctx, userID, "sessions_revoked_others", strconv.Itoa(n), "")
	}
	return n, nil
}

// AuthEvent is one row of the login history.
type AuthEvent struct {
	ID        int64
	Event     string
	Detail    string
	IP        string
	CreatedAt time.Time
}

// LoginHistory is one page of a user's authentication events.
//
// Keyset pagination on (created_at, id) for the same reason every other list uses it: a
// page read while a login is being recorded must not skip a row, and a login history with
// a gap is worse than no login history.
type LoginHistory struct {
	Events     []AuthEvent
	NextCursor string
}

// LoginHistory returns the caller's own auth_events, newest first.
func (s *Service) LoginHistory(ctx context.Context, userID string, limit int, cursor string) (LoginHistory, error) {
	limit = paginate.Clamp(limit)

	args := []any{userID}
	where := "user_id = $1"
	if cursor != "" {
		sortKey, id, err := paginate.Decode(cursor)
		if err != nil {
			return LoginHistory{}, err
		}
		when, err := time.Parse(time.RFC3339Nano, sortKey)
		if err != nil {
			return LoginHistory{}, fmt.Errorf("%w: bad timestamp", paginate.ErrInvalidCursor)
		}
		rowID, err := strconv.ParseInt(id, 10, 64)
		if err != nil {
			return LoginHistory{}, fmt.Errorf("%w: bad id", paginate.ErrInvalidCursor)
		}
		args = append(args, when, rowID)
		where += " AND (created_at, id) < ($2, $3)"
	}
	args = append(args, limit+1)

	rows, err := s.pool.Query(ctx, `
		SELECT id, event, detail, COALESCE(host(ip), ''), created_at
		FROM auth_events
		WHERE `+where+`
		ORDER BY created_at DESC, id DESC
		LIMIT $`+strconv.Itoa(len(args)), args...)
	if err != nil {
		return LoginHistory{}, fmt.Errorf("authn: list login history: %w", err)
	}
	defer rows.Close()

	out := []AuthEvent{}
	for rows.Next() {
		var ev AuthEvent
		if err := rows.Scan(&ev.ID, &ev.Event, &ev.Detail, &ev.IP, &ev.CreatedAt); err != nil {
			return LoginHistory{}, fmt.Errorf("authn: scan auth event: %w", err)
		}
		out = append(out, ev)
	}
	if err := rows.Err(); err != nil {
		return LoginHistory{}, fmt.Errorf("authn: read login history: %w", err)
	}

	res := LoginHistory{}
	if len(out) > limit {
		out = out[:limit]
		last := out[len(out)-1]
		res.NextCursor = paginate.Encode(last.CreatedAt.UTC().Format(time.RFC3339Nano),
			strconv.FormatInt(last.ID, 10))
	}
	res.Events = out
	return res, nil
}

// SessionByID loads one live session belonging to userID.
//
// It exists for the staff view, which has to show a session that is not the caller's own,
// and for impersonation, which has to know whether the session it is ending is one.
func (s *Service) SessionByID(ctx context.Context, userID, sessionID string) (SessionInfo, error) {
	var info SessionInfo
	err := s.pool.QueryRow(ctx, `
		SELECT id, created_at, last_seen_at, expires_at,
		       COALESCE(host(ip), ''), user_agent,
		       COALESCE(impersonated_by::text, ''), user_id
		FROM sessions
		WHERE id = $1 AND user_id = $2 AND revoked_at IS NULL`,
		sessionID, userID).
		Scan(&info.ID, &info.CreatedAt, &info.LastSeenAt, &info.ExpiresAt,
			&info.IP, &info.UserAgent, &info.ImpersonatedBy, &info.UserID)
	if errors.Is(err, pgx.ErrNoRows) {
		return SessionInfo{}, ErrNoSessionRow
	}
	if err != nil {
		return SessionInfo{}, fmt.Errorf("authn: load session: %w", err)
	}
	return info, nil
}
