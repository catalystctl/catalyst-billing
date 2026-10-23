package authn

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/catalyst/billing/internal/paginate"
	"github.com/catalyst/billing/internal/settings"
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

// ---------------------------------------------------------- session binding --
//
// Session binding (IDN-16): a session may be locked to the address and the user agent it
// was signed in from, and a request from outside that lock is refused.
//
// Three decisions carry this feature, and each one is a trade rather than a truth:
//
//  1. **It is a policy, recorded per session, not a fixed rule.** The mode in force when a
//     session is issued is written onto the row (`bound_mode`) and the request path enforces
//     *that*. Reading the current setting on every request instead would re-judge sessions
//     that are already open: an operator narrowing the policy at 10:00 would sign out every
//     staff member who signed in from the office at 09:00, and neither of them could tell
//     which change did it. A session therefore keeps the rule it was born under, and a
//     policy change applies to the next sign-in. The cost is that tightening the setting is
//     not immediate for existing sessions; the alternative cost is an unexplained mass
//     sign-out, which is worse and is why the setting exists at all.
//
//  2. **A mismatch refuses the request, it does not end the session.** Signing the session
//     out on a mismatch would turn "your mobile network changed address" into "enter your
//     password again", which is the exact experience that makes an operator switch the
//     control off. Refusing is reversible — the next request from the bound context works —
//     and it is still enough to make a stolen cookie useless from the thief's network,
//     because every request from there is refused for as long as the session lives. The
//     operator keeps the other lever for the stolen-cookie case: revoking the session from
//     the device list.
//
//  3. **Nothing is silently downgraded.** An unknown recorded mode, or a mode whose recorded
//     value is missing, refuses rather than allows: a binding that cannot be checked must not
//     behave like no binding. The one thing that *is* allowed is the empty mode, which is
//     the explicit "this session was issued while binding was off" state.

// The binding modes a session row can record, re-exported from the settings package so a
// caller that already holds a session can name its mode without importing the settings table.
const (
	BindUserAgent      = settings.BindUserAgent
	BindIPAndUserAgent = settings.BindIPAndUserAgent
)

// ErrSessionBinding is returned when a request is made from outside what the session was
// bound to. Callers map it to a refusal with its own problem code, distinct from
// "authentication required": the session is real, it is just not being used from where it
// was signed in.
var ErrSessionBinding = errors.New("authn: session used outside its binding")

// BindingMismatch is the detail of a refused binding: which recorded value no longer
// matches. It deliberately does not carry the recorded values themselves — the refusal is
// shown to whoever holds the cookie, and a stolen cookie should not learn the owner's
// address from it.
type BindingMismatch struct {
	// Field is "ip" or "user_agent".
	Field string
}

func (m *BindingMismatch) Error() string {
	return "authn: session binding mismatch on " + m.Field
}

// Unwrap makes errors.Is(err, ErrSessionBinding) work through the detail type.
func (m *BindingMismatch) Unwrap() error { return ErrSessionBinding }

// bindingForSession decides what a session for an account of this class is bound to.
//
// scope narrows the mode to staff or customer sessions, which is the shape the parity row
// describes as "admin/client/both". A settings read failure means `off`: binding is a
// hardening measure, and an installation whose settings table cannot be read has a larger
// problem than an unbound session — failing closed would lock every user out of the panel
// over a configuration read.
func (s *Service) bindingForSession(ctx context.Context, isStaff bool) string {
	if s == nil || s.pool == nil {
		return ""
	}
	mode, scope := settings.SessionBinding(ctx, s.pool)
	switch scope {
	case settings.BindScopeStaff:
		if !isStaff {
			return ""
		}
	case settings.BindScopeCustomers:
		if isStaff {
			return ""
		}
	}
	switch mode {
	case settings.BindUserAgent, settings.BindIPAndUserAgent:
		return mode
	}
	return ""
}

// bindingValues returns the values to record for a mode.
//
// Only what the mode actually checks is recorded. Storing an address under user-agent mode
// would make the row look stricter than the rule applied to it, and the next person to read
// the table — including whoever investigates an incident — would draw the wrong conclusion
// about what was enforced.
func bindingValues(binding, ip, userAgent string) (boundIP, boundUserAgent string) {
	switch binding {
	case settings.BindUserAgent:
		return "", truncate(userAgent, 512)
	case settings.BindIPAndUserAgent:
		return normaliseIP(ip), truncate(userAgent, 512)
	}
	return "", ""
}

// EnforceSessionBinding refuses a request made from outside what the session was bound to.
//
// It is called on every authenticated request, so it does no I/O of its own beyond the audit
// row written when a refusal happens: everything it needs is on the session row that
// Authenticate already read.
func (s *Service) EnforceSessionBinding(ctx context.Context, sess Session, ip, userAgent string) error {
	if sess.BoundMode == "" {
		// Issued while binding was off. There is nothing to compare, and pretending
		// otherwise would sign out every session that predates the setting.
		return nil
	}
	switch sess.BoundMode {
	case settings.BindUserAgent:
		if truncate(userAgent, 512) == sess.BoundUserAgent {
			return nil
		}
		return s.refuseBinding(ctx, sess, "user_agent")
	case settings.BindIPAndUserAgent:
		if normaliseIP(ip) != sess.BoundIP {
			return s.refuseBinding(ctx, sess, "ip")
		}
		if truncate(userAgent, 512) != sess.BoundUserAgent {
			return s.refuseBinding(ctx, sess, "user_agent")
		}
		return nil
	}
	// A mode this code does not understand. Fail closed: a binding that cannot be checked
	// must not behave like no binding.
	return s.refuseBinding(ctx, sess, "unknown")
}

// refuseBinding records the refusal and returns it.
//
// The audit row is best-effort and deliberately not rate limited here: a refusal means
// somebody is using a session from somewhere it was not issued, which is exactly the event
// an investigation needs in order, and an attacker who can generate refusals already holds a
// valid session cookie.
func (s *Service) refuseBinding(ctx context.Context, sess Session, field string) error {
	s.audit(ctx, sess.UserID, "session_binding_refused",
		"session "+sess.ID+" does not match its bound "+field, "")
	return &BindingMismatch{Field: field}
}

// BindingDescription renders what a session is bound to, for the device list and for the
// refusal detail. It says the mode in words an operator reads, never the recorded values.
func BindingDescription(mode string) string {
	switch mode {
	case settings.BindUserAgent:
		return "this session is bound to the browser it was signed in from"
	case settings.BindIPAndUserAgent:
		return "this session is bound to the address and browser it was signed in from"
	}
	return ""
}

// normaliseIP renders an address in one canonical form so the comparison is textual and
// exact.
//
// Exact, not subnet-based: a /24 or /64 rule would be friendlier on a mobile network and
// would also let anyone in the same carrier range reuse a stolen cookie, which is most of the
// threat this control exists for. An installation that finds exact matching too strict
// switches the mode off or scopes it to staff; that is the knob, and it is declared in
// internal/settings/group_session.go.
func normaliseIP(ip string) string {
	parsed := net.ParseIP(strings.TrimSpace(ip))
	if parsed == nil {
		return ""
	}
	return parsed.String()
}
