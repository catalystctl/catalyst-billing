package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/catalyst/billing/internal/audit"
	"github.com/catalyst/billing/internal/authn"
	"github.com/catalyst/billing/internal/paginate"
	"github.com/jackc/pgx/v5"
)

// The security screen: every live session, and the sign-in trail behind it.
//
// The existing session routes answer "where am I signed in". That is the right question for a
// person and the wrong one for an incident: an operator investigating a compromise needs to
// see *everyone's* sessions and *everyone's* sign-in history, because the first question is
// "which accounts are being attacked and which sessions are already open".
//
// Three rules shape this file, and each one is a way this screen could do harm:
//
//   - Revoking a session is an administrative act, so it is audited in the same transaction
//     that ends the session. An audit row for a revocation that rolled back is a lie; a
//     revocation with no audit row is a gap an incident review cannot close.
//
//   - An operator cannot revoke the session making the request *through this screen*. The
//     portal route allows it, because there it is "sign out this device" and the person
//     expects it. Here it is almost always a mis-click in a table of rows that all look alike,
//     and its consequence is being thrown out of the panel mid-incident. The refusal names the
//     route that does mean it (POST /api/v1/auth/logout), so the answer is a next step rather
//     than a dead end.
//
//   - The sign-in history is the *authentication* trail, not the audit trail: it is
//     auth_events, which already records both `login_succeeded` and `login_failed` (with the
//     account and the address), so "is this account being attacked" is a question about rows
//     that already exist rather than a new log to start writing today.

func init() {
	RegisterRoutes("63-security-admin", registerSecurityAdmin)
}

func registerSecurityAdmin(r *router) {
	// staff:manage is the permission that already governs other people's accounts: roles,
	// impersonation, disabling. Reading everyone's sessions belongs to the same act.
	r.staffGet("/api/v1/staff/security/sessions", authn.PermStaffManage, r.s.handleAllSessions)
	r.staffMutation(http.MethodDelete, "/api/v1/staff/security/sessions/{id}",
		authn.PermStaffManage, r.s.handleRevokeAnySession)
	r.staffGet("/api/v1/staff/security/sign-ins", authn.PermStaffManage, r.s.handleSignInHistory)
}

// securitySession is one live session with its owner attached.
type securitySession struct {
	ID             string
	UserID         string
	Email          string
	DisplayName    string
	CreatedAt      time.Time
	LastSeenAt     time.Time
	ExpiresAt      time.Time
	IP             string
	UserAgent      string
	ImpersonatedBy string
	Impersonator   string
}

func securitySessionJSON(row securitySession, currentID string) map[string]any {
	out := map[string]any{
		"id": row.ID,
		"user": map[string]any{
			"id":           row.UserID,
			"email":        row.Email,
			"display_name": row.DisplayName,
		},
		"created_at":      row.CreatedAt.UTC(),
		"last_seen_at":    row.LastSeenAt.UTC(),
		"expires_at":      row.ExpiresAt.UTC(),
		"ip":              row.IP,
		"user_agent":      row.UserAgent,
		"current":         row.ID == currentID,
		"impersonated_by": nil,
		"impersonator":    nil,
	}
	if row.ImpersonatedBy != "" {
		out["impersonated_by"] = row.ImpersonatedBy
		out["impersonator"] = row.Impersonator
	}
	return out
}

// handleAllSessions lists every live session across every account.
//
// Query:
//
//	q       matches the account's email or display name
//	user_id scopes the list to one account
//	limit   default 25, max 100
//	cursor  keyset cursor from a previous page
//
// The order is by creation time, not last-seen: a session's last_seen_at changes while a page
// is being read, which would let a keyset cursor skip or repeat a row. Creation time is fixed
// for a session, so the page is stable in the way the cursor promises.
func (s *Server) handleAllSessions(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "database_not_configured",
			"no database is configured")
		return
	}
	currentSession, _ := sessionFrom(r.Context())

	limit, cursor, ok := s.pageParams(w, r)
	if !ok {
		return
	}

	query := r.URL.Query()
	args := []any{}
	where := []string{`s.revoked_at IS NULL`, `s.expires_at > now()`}

	if raw := strings.TrimSpace(query.Get("user_id")); raw != "" {
		if !validUUID(raw) {
			writeProblem(w, http.StatusBadRequest, "invalid_user", "user_id must be a user id")
			return
		}
		args = append(args, raw)
		where = append(where, fmt.Sprintf(`s.user_id = $%d::uuid`, len(args)))
	}
	if q := strings.TrimSpace(query.Get("q")); q != "" {
		args = append(args, "%"+q+"%")
		where = append(where, fmt.Sprintf(
			`(u.email ILIKE $%d OR u.display_name ILIKE $%d)`, len(args), len(args)))
	}
	if cursor != "" {
		sortKey, id, err := paginate.Decode(cursor)
		if err != nil || !validUUID(id) {
			writeProblem(w, http.StatusBadRequest, "invalid_cursor", "the cursor is invalid")
			return
		}
		when, err := time.Parse(time.RFC3339Nano, sortKey)
		if err != nil {
			writeProblem(w, http.StatusBadRequest, "invalid_cursor", "the cursor is invalid")
			return
		}
		args = append(args, when, id)
		where = append(where, fmt.Sprintf(
			`(s.created_at, s.id) < ($%d::timestamptz, $%d::uuid)`, len(args)-1, len(args)))
	}
	args = append(args, limit+1)

	rows, err := s.pool.Query(r.Context(), `
		SELECT s.id, s.user_id, u.email, COALESCE(u.display_name, ''),
		       s.created_at, s.last_seen_at, s.expires_at,
		       COALESCE(host(s.ip), ''), s.user_agent,
		       COALESCE(s.impersonated_by::text, ''), COALESCE(iu.email, '')
		FROM sessions s
		JOIN users u ON u.id = s.user_id
		LEFT JOIN users iu ON iu.id = s.impersonated_by
		WHERE `+strings.Join(where, " AND ")+`
		ORDER BY s.created_at DESC, s.id DESC
		LIMIT $`+strconv.Itoa(len(args)), args...)
	if err != nil {
		s.securityError(w, r, "list sessions", err)
		return
	}
	defer rows.Close()

	scanned := []securitySession{}
	for rows.Next() {
		var row securitySession
		if err := rows.Scan(&row.ID, &row.UserID, &row.Email, &row.DisplayName,
			&row.CreatedAt, &row.LastSeenAt, &row.ExpiresAt, &row.IP, &row.UserAgent,
			&row.ImpersonatedBy, &row.Impersonator); err != nil {
			s.securityError(w, r, "scan session", err)
			return
		}
		scanned = append(scanned, row)
	}
	if err := rows.Err(); err != nil {
		s.securityError(w, r, "list sessions", err)
		return
	}

	var next *string
	if len(scanned) > limit {
		scanned = scanned[:limit]
		last := scanned[len(scanned)-1]
		encoded := paginate.Encode(last.CreatedAt.UTC().Format(time.RFC3339Nano), last.ID)
		next = &encoded
	}
	out := make([]map[string]any, 0, len(scanned))
	for _, row := range scanned {
		out = append(out, securitySessionJSON(row, currentSession.ID))
	}

	summary, err := s.sessionSummary(r)
	if err != nil {
		s.securityError(w, r, "count sessions", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"data":        out,
		"next_cursor": next,
		"summary":     summary,
	})
}

// sessionSummary answers the header question: how many sessions exist, and how many accounts
// they belong to. A count of rows is not the same number as a count of people, and the
// difference is the whole reason to look at this screen.
func (s *Server) sessionSummary(r *http.Request) (map[string]any, error) {
	var sessions, users, impersonations int
	err := s.pool.QueryRow(r.Context(), `
		SELECT count(*), count(DISTINCT user_id),
		       count(*) FILTER (WHERE impersonated_by IS NOT NULL)
		FROM sessions
		WHERE revoked_at IS NULL AND expires_at > now()`).Scan(&sessions, &users, &impersonations)
	if err != nil {
		return nil, fmt.Errorf("security: count sessions: %w", err)
	}
	return map[string]any{
		"sessions":       sessions,
		"users":          users,
		"impersonations": impersonations,
	}, nil
}

// handleRevokeAnySession ends somebody's session from the security screen.
//
// It refuses the caller's own current session: that is a sign-out, it belongs on the logout
// route, and doing it here by accident throws the operator out of the panel in the middle of
// the incident they are handling.
func (s *Server) handleRevokeAnySession(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "database_not_configured",
			"no database is configured")
		return
	}
	caller, _ := userFrom(r.Context())
	current, _ := sessionFrom(r.Context())

	id := strings.TrimSpace(r.PathValue("id"))
	if !validUUID(id) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such session")
		return
	}
	if current.ID != "" && id == current.ID {
		// Refused explicitly, with the route that does mean it, so this is a next step rather
		// than a mystery.
		writeProblem(w, http.StatusConflict, "own_session",
			"this is the session making the request: sign out with POST /api/v1/auth/logout "+
				"instead of revoking it from the security screen")
		return
	}

	var (
		targetID    string
		targetEmail string
		wasCurrent  bool
	)
	err := s.pool.InTx(r.Context(), func(ctx context.Context, tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `
			SELECT s.user_id, u.email
			FROM sessions s
			JOIN users u ON u.id = s.user_id
			WHERE s.id = $1 AND s.revoked_at IS NULL
			FOR UPDATE OF s`, id).Scan(&targetID, &targetEmail)
		if errors.Is(err, pgx.ErrNoRows) {
			return errAdminNoSession
		}
		if err != nil {
			return fmt.Errorf("security: lock session: %w", err)
		}
		wasCurrent = targetID == caller.ID

		if _, err := tx.Exec(ctx, `
			UPDATE sessions SET revoked_at = now() WHERE id = $1`, id); err != nil {
			return fmt.Errorf("security: revoke session: %w", err)
		}
		// The account's own sign-in trail records who ended the session: a customer whose
		// session disappeared is entitled to see that it was an operator and not a compromise.
		if _, err := tx.Exec(ctx, `
			INSERT INTO auth_events (user_id, event, detail, ip)
			VALUES ($1, 'session_revoked', $2, NULLIF($3, '')::inet)`,
			targetID, "by operator "+caller.Email, s.clientIP(r)); err != nil {
			return fmt.Errorf("security: record session revocation: %w", err)
		}
		return audit.Record(ctx, tx, s.auditEvent(r, "session.revoke", "user", targetID,
			audit.OutcomeSuccess).WithChanges(map[string]any{
			"session_id": id,
			"user_email": targetEmail,
			// Whether it was another session of the operator's own account is the fact an
			// incident review wants; the session id alone does not say it.
			"own_account": wasCurrent,
		}))
	})
	switch {
	case errors.Is(err, errAdminNoSession):
		writeProblem(w, http.StatusNotFound, "not_found", "no such session")
		return
	case err != nil:
		s.securityError(w, r, "revoke session", err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"status":  "revoked",
		"user_id": targetID,
		"email":   targetEmail,
		"current": false,
	})
}

// signInEvent is one row of the sign-in trail.
type signInEvent struct {
	ID        int64
	UserID    string
	Email     string
	Event     string
	Detail    string
	IP        string
	CreatedAt time.Time
}

// handleSignInHistory lists successful and failed sign-ins across every account.
//
// Query:
//
//	outcome success | failure | all (default)
//	q       matches the account's email
//	limit   default 25, max 100
//	cursor  keyset cursor from a previous page
//
// `outcome` is computed here rather than left to the client: the vocabulary in auth_events is
// stable codes (login_succeeded, login_failed, login_rejected, login_throttled), and a screen
// that branched on those strings would silently show nothing if one were added.
func (s *Server) handleSignInHistory(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "database_not_configured",
			"no database is configured")
		return
	}
	limit, cursor, ok := s.pageParams(w, r)
	if !ok {
		return
	}

	query := r.URL.Query()
	outcome := strings.TrimSpace(query.Get("outcome"))
	switch outcome {
	case "":
		outcome = "all"
	case "success", "failure", "all":
	default:
		writeProblem(w, http.StatusBadRequest, "invalid_outcome",
			"outcome must be success, failure or all")
		return
	}

	args := []any{signInEvents}
	where := []string{`e.event = ANY($1::text[])`}
	switch outcome {
	case "success":
		where = append(where, `e.event = 'login_succeeded'`)
	case "failure":
		where = append(where, `e.event <> 'login_succeeded'`)
	}
	if q := strings.TrimSpace(query.Get("q")); q != "" {
		args = append(args, "%"+q+"%")
		where = append(where, fmt.Sprintf(`u.email ILIKE $%d`, len(args)))
	}
	if cursor != "" {
		sortKey, id, err := paginate.Decode(cursor)
		if err != nil {
			writeProblem(w, http.StatusBadRequest, "invalid_cursor", "the cursor is invalid")
			return
		}
		when, err := time.Parse(time.RFC3339Nano, sortKey)
		if err != nil {
			writeProblem(w, http.StatusBadRequest, "invalid_cursor", "the cursor is invalid")
			return
		}
		rowID, err := strconv.ParseInt(id, 10, 64)
		if err != nil {
			writeProblem(w, http.StatusBadRequest, "invalid_cursor", "the cursor is invalid")
			return
		}
		args = append(args, when, rowID)
		where = append(where, fmt.Sprintf(
			`(e.created_at, e.id) < ($%d::timestamptz, $%d::bigint)`, len(args)-1, len(args)))
	}
	args = append(args, limit+1)

	rows, err := s.pool.Query(r.Context(), `
		SELECT e.id, COALESCE(e.user_id::text, ''), COALESCE(u.email, ''),
		       e.event, e.detail, COALESCE(host(e.ip), ''), e.created_at
		FROM auth_events e
		LEFT JOIN users u ON u.id = e.user_id
		WHERE `+strings.Join(where, " AND ")+`
		ORDER BY e.created_at DESC, e.id DESC
		LIMIT $`+strconv.Itoa(len(args)), args...)
	if err != nil {
		s.securityError(w, r, "list sign-ins", err)
		return
	}
	defer rows.Close()

	scanned := []signInEvent{}
	for rows.Next() {
		var ev signInEvent
		if err := rows.Scan(&ev.ID, &ev.UserID, &ev.Email, &ev.Event, &ev.Detail, &ev.IP,
			&ev.CreatedAt); err != nil {
			s.securityError(w, r, "scan sign-in", err)
			return
		}
		scanned = append(scanned, ev)
	}
	if err := rows.Err(); err != nil {
		s.securityError(w, r, "list sign-ins", err)
		return
	}

	var next *string
	if len(scanned) > limit {
		scanned = scanned[:limit]
		last := scanned[len(scanned)-1]
		encoded := paginate.Encode(last.CreatedAt.UTC().Format(time.RFC3339Nano),
			strconv.FormatInt(last.ID, 10))
		next = &encoded
	}
	out := make([]map[string]any, 0, len(scanned))
	for _, ev := range scanned {
		row := map[string]any{
			"id":         ev.ID,
			"event":      ev.Event,
			"outcome":    signInOutcome(ev.Event),
			"detail":     ev.Detail,
			"ip":         ev.IP,
			"created_at": ev.CreatedAt.UTC(),
			"user":       nil,
		}
		if ev.UserID != "" {
			row["user"] = map[string]any{"id": ev.UserID, "email": ev.Email}
		}
		out = append(out, row)
	}

	summary, err := s.signInSummary(r)
	if err != nil {
		s.securityError(w, r, "count sign-ins", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"data":        out,
		"next_cursor": next,
		"summary":     summary,
	})
}

// signInEvents is the vocabulary this screen reports, as one place to change it.
//
// login_rejected is an attempt against a disabled or unverified account and login_throttled is
// the limiter firing; both are failures an operator investigating an attack needs to see, and
// both are already written by internal/authn.
var signInEvents = []string{
	"login_succeeded", "login_failed", "login_rejected", "login_throttled",
}

// signInOutcome classifies an event for the screen.
func signInOutcome(event string) string {
	if event == "login_succeeded" {
		return "success"
	}
	return "failure"
}

// signInSummary answers "is this account being attacked" with the last day's numbers: how many
// attempts failed, and which account and which address they were aimed at.
func (s *Server) signInSummary(r *http.Request) (map[string]any, error) {
	var successes, failures int
	err := s.pool.QueryRow(r.Context(), `
		SELECT count(*) FILTER (WHERE event = 'login_succeeded'),
		       count(*) FILTER (WHERE event <> 'login_succeeded')
		FROM auth_events
		WHERE event = ANY($1::text[])
		  AND created_at > now() - interval '24 hours'`, signInEvents).Scan(&successes, &failures)
	if err != nil {
		return nil, fmt.Errorf("security: count sign-ins: %w", err)
	}

	type target struct {
		Label string `json:"label"`
		Count int    `json:"count"`
	}
	collect := func(query string) ([]target, error) {
		rows, err := s.pool.Query(r.Context(), query, signInEvents)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		out := []target{}
		for rows.Next() {
			var t target
			if err := rows.Scan(&t.Label, &t.Count); err != nil {
				return nil, err
			}
			out = append(out, t)
		}
		return out, rows.Err()
	}

	accounts, err := collect(`
		SELECT COALESCE(u.email, 'unknown account') AS label, count(*)::int AS count
		FROM auth_events e
		LEFT JOIN users u ON u.id = e.user_id
		WHERE e.event = ANY($1::text[]) AND e.event <> 'login_succeeded'
		  AND e.created_at > now() - interval '24 hours'
		GROUP BY 1 ORDER BY 2 DESC, 1 LIMIT 5`)
	if err != nil {
		return nil, fmt.Errorf("security: count failed accounts: %w", err)
	}
	addresses, err := collect(`
		SELECT COALESCE(host(ip), 'unknown address') AS label, count(*)::int AS count
		FROM auth_events
		WHERE event = ANY($1::text[]) AND event <> 'login_succeeded'
		  AND created_at > now() - interval '24 hours'
		GROUP BY 1 ORDER BY 2 DESC, 1 LIMIT 5`)
	if err != nil {
		return nil, fmt.Errorf("security: count failed addresses: %w", err)
	}

	return map[string]any{
		"window_hours":         24,
		"successes":            successes,
		"failures":             failures,
		"top_failing_accounts": accounts,
		"top_failing_ips":      addresses,
	}, nil
}

// errAdminNoSession means "there is no such live session". It is a package error so the handler's
// switch cannot match a different one by accident.
var errAdminNoSession = errors.New("security: no such session")

// securityError logs and reports a failure without leaking its detail.
func (s *Server) securityError(w http.ResponseWriter, r *http.Request, action string, err error) {
	s.cfg.Logger.ErrorContext(r.Context(), "security request failed",
		"action", action, "error", err)
	writeProblem(w, http.StatusInternalServerError, "internal_error",
		"the request could not be completed")
}
