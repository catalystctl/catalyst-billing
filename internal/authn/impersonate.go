package authn

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Impersonation (IDN-14, ADM-30).
//
// A staff member who needs to see what a customer sees does not need the customer's
// password. They need a session that *is* the customer's, for a bounded time, with a
// record of who is behind it.
//
// The design follows from that sentence:
//
//   - The session is a normal session in every other respect. Every existing query that
//     scopes by user id keeps working, so an impersonated request can do exactly what the
//     customer can do and nothing more. A separate "as-user" principal would have needed
//     every query to learn about it, and the one that forgot would be the hole.
//   - `impersonated_by` is the operator. It is on the session, not on the user, because
//     two operators may be looking at the same customer at once and the audit trail has
//     to tell them apart.
//   - The operator's own session is linked, not consumed. Ending the impersonation rotates
//     that session's token and hands the caller the new one, so "stop" returns the operator
//     to their own identity instead of logging them out. Rotation is deliberate: the
//     impersonated token was in a browser that may have been shared, and a token that
//     outlives the impersonation is the one thing that must not happen.
//   - Only a staff member holding PermStaffManage may start one. That is the same
//     permission that grants roles, because impersonation is a way to act as somebody
//     else and belongs with the other ways to change who can do what.

var (
	// ErrImpersonationNotAllowed is returned when a session is not an impersonation.
	ErrImpersonationNotAllowed = errors.New("authn: this session is not an impersonation")
	// ErrSelfImpersonation is returned when an operator targets themselves.
	ErrSelfImpersonation = errors.New("authn: an operator cannot impersonate themselves")
)

// ImpersonationResult is the new session plus the identities it involves.
type ImpersonationResult struct {
	// Token is the impersonated session's token. It is returned once and must be put in a
	// cookie immediately, exactly like a login.
	Token   string
	Session Session
	// Target is the user being impersonated, with their roles resolved.
	Target User
	// Operator is the staff member who started it.
	Operator User
}

// Impersonate issues a session for targetID on behalf of operatorID.
//
// operatorSessionID is the operator's own live session; it is linked so StopImpersonation
// can return to it. The caller (the HTTP layer) is responsible for having checked
// PermStaffManage; this function re-reads both users so a stale identity in the request
// cannot impersonate a disabled account.
func (s *Service) Impersonate(ctx context.Context, operatorID, operatorSessionID, targetID, ip, userAgent string) (ImpersonationResult, error) {
	if operatorID == "" || targetID == "" {
		return ImpersonationResult{}, ErrUnknownAccount
	}
	if operatorID == targetID {
		// Impersonating yourself is a no-op that would leave an audit trail claiming an
		// operator impersonated themselves, which is noise in exactly the report that
		// should be short.
		return ImpersonationResult{}, ErrSelfImpersonation
	}

	operator, err := s.UserByID(ctx, operatorID)
	if err != nil {
		return ImpersonationResult{}, err
	}
	if operator.Status != "active" || !operator.IsStaff() {
		return ImpersonationResult{}, ErrForbidden
	}
	target, err := s.UserByID(ctx, targetID)
	if err != nil {
		return ImpersonationResult{}, err
	}
	if target.Status != "active" {
		return ImpersonationResult{}, ErrAccountDisabled
	}
	if target.IsStaff() {
		// Impersonating another staff member would let a support agent borrow an
		// administrator's identity, which defeats the permission model entirely.
		return ImpersonationResult{}, fmt.Errorf("%w: a staff account cannot be impersonated", ErrForbidden)
	}

	var credVersion int
	if err := s.pool.QueryRow(ctx,
		`SELECT credential_version FROM users WHERE id = $1`, targetID).Scan(&credVersion); err != nil {
		return ImpersonationResult{}, fmt.Errorf("authn: read credential version: %w", err)
	}

	token, sess, err := s.createImpersonatedSession(ctx, targetID, operatorID,
		operatorSessionID, credVersion, ip, userAgent)
	if err != nil {
		return ImpersonationResult{}, err
	}
	s.audit(ctx, targetID, "impersonation_started", "by "+operatorID, ip)
	s.audit(ctx, operatorID, "impersonation_started", "as "+targetID, ip)

	return ImpersonationResult{Token: token, Session: sess, Target: target, Operator: operator}, nil
}

// createImpersonatedSession is createSession with the operator recorded.
//
// It is a separate function rather than a parameter on createSession so that a normal
// login cannot accidentally pass an operator: the only way to set impersonated_by is to
// call this, and this is only called from Impersonate.
func (s *Service) createImpersonatedSession(ctx context.Context, userID, operatorID, operatorSessionID string,
	credVersion int, ip, userAgent string) (string, Session, error) {
	token, err := newToken(32)
	if err != nil {
		return "", Session{}, err
	}
	csrfToken, err := newToken(32)
	if err != nil {
		return "", Session{}, err
	}

	now := s.cfg.Now()
	expires := now.Add(s.cfg.SessionLifetime)
	if absolute := now.Add(s.cfg.AbsoluteLifetime); absolute.Before(expires) {
		expires = absolute
	}

	// An impersonated session is bound by the class of the account it acts as, which is
	// always a customer: staff accounts cannot be impersonated. A staff-only binding is
	// therefore not bypassed here — the operator's own session, which is what authorised
	// the impersonation, carries that binding and had to pass it to get this far.
	binding := s.bindingForSession(ctx, false)
	boundIP, boundAgent := bindingValues(binding, ip, userAgent)

	var (
		id        string
		createdAt time.Time
		expiresAt time.Time
	)
	err = s.pool.QueryRow(ctx, `
		INSERT INTO sessions (user_id, token_hash, csrf_secret, credential_version,
			created_at, last_seen_at, expires_at, user_agent, ip,
			impersonated_by, impersonated_from_session,
			bound_mode, bound_ip, bound_user_agent)
		VALUES ($1, $2, $3, $4, $5, $5, $6, $7, NULLIF($8, '')::inet, $9, NULLIF($10, '')::uuid,
			$11, NULLIF($12, '')::inet, $13)
		RETURNING id, created_at, expires_at`,
		userID, HashToken(token), []byte(csrfToken), credVersion,
		now, expires, truncate(userAgent, 512), ip, operatorID, operatorSessionID,
		binding, boundIP, boundAgent).
		Scan(&id, &createdAt, &expiresAt)
	if err != nil {
		return "", Session{}, fmt.Errorf("authn: create impersonated session: %w", err)
	}

	return token, Session{
		ID:             id,
		UserID:         userID,
		CreatedAt:      createdAt,
		BoundMode:      binding,
		BoundIP:        boundIP,
		BoundUserAgent: boundAgent,
		ExpiresAt:      expiresAt,
		CSRFToken:      csrfToken,
	}, nil
}

// StopResult is the operator's re-issued session.
type StopResult struct {
	OperatorID string
	// Token is the operator's new session token. The previous token is dead: the caller
	// must set the new cookie in the same response.
	Token   string
	Session Session
	User    User
}

// StopImpersonation ends an impersonated session and returns to the operator.
//
// It is scoped to the target user, so the only session it can end is the one the caller is
// already using. The operator's own session is rotated rather than re-created: the same row
// keeps its id and creation time, so the "where am I signed in" list does not grow a new
// entry every time somebody stops impersonating.
func (s *Service) StopImpersonation(ctx context.Context, targetUserID, sessionID, ip string) (StopResult, error) {
	var (
		operatorID      string
		operatorSession string
	)
	err := s.pool.QueryRow(ctx, `
		SELECT COALESCE(impersonated_by::text, ''), COALESCE(impersonated_from_session::text, '')
		FROM sessions
		WHERE id = $1 AND user_id = $2 AND revoked_at IS NULL`, sessionID, targetUserID).
		Scan(&operatorID, &operatorSession)
	if errors.Is(err, pgx.ErrNoRows) {
		return StopResult{}, ErrNoSessionRow
	}
	if err != nil {
		return StopResult{}, fmt.Errorf("authn: read impersonated session: %w", err)
	}
	if operatorID == "" {
		// Ending a normal session is a logout, not "stop impersonating". Saying so keeps
		// the two operations from being silently interchangeable.
		return StopResult{}, ErrImpersonationNotAllowed
	}

	now := s.cfg.Now()
	if _, err := s.pool.Exec(ctx, `
		UPDATE sessions SET revoked_at = $3
		WHERE id = $1 AND user_id = $2 AND revoked_at IS NULL`,
		sessionID, targetUserID, now); err != nil {
		return StopResult{}, fmt.Errorf("authn: end impersonation: %w", err)
	}

	// The operator's own session must still be alive. If it was revoked (or expired) while
	// the impersonation was running, the honest answer is "sign in again": handing back a
	// token for a dead session would produce a browser that looks signed in and is not.
	var expiresAt time.Time
	if err := s.pool.QueryRow(ctx, `
		SELECT expires_at FROM sessions
		WHERE id = $1 AND revoked_at IS NULL AND expires_at > $2`, operatorSession, now).
		Scan(&expiresAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return StopResult{OperatorID: operatorID}, ErrNoSessionRow
		}
		return StopResult{}, fmt.Errorf("authn: load operator session: %w", err)
	}

	token, err := newToken(32)
	if err != nil {
		return StopResult{}, err
	}
	csrfToken, err := newToken(32)
	if err != nil {
		return StopResult{}, err
	}
	if _, err := s.pool.Exec(ctx, `
		UPDATE sessions SET token_hash = $2, csrf_secret = $3, last_seen_at = $4
		WHERE id = $1`, operatorSession, HashToken(token), []byte(csrfToken), now); err != nil {
		return StopResult{}, fmt.Errorf("authn: rotate operator session: %w", err)
	}

	operator, err := s.UserByID(ctx, operatorID)
	if err != nil {
		return StopResult{}, err
	}

	s.audit(ctx, targetUserID, "impersonation_stopped", "by "+operatorID, ip)
	s.audit(ctx, operatorID, "impersonation_stopped", "as "+targetUserID, ip)

	return StopResult{
		OperatorID: operatorID,
		Token:      token,
		Session: Session{
			ID:        operatorSession,
			UserID:    operatorID,
			ExpiresAt: expiresAt,
			CSRFToken: csrfToken,
		},
		User: operator,
	}, nil
}

// IsImpersonated reports whether a session is an impersonation, and who is behind it.
//
// It is used by the HTTP layer to mark /api/v1/auth/me and to refuse the sensitive actions
// an operator should perform as themselves rather than as somebody else.
func (s *Service) IsImpersonated(ctx context.Context, sessionID string) (bool, string, error) {
	var operator string
	err := s.pool.QueryRow(ctx,
		`SELECT COALESCE(impersonated_by::text, '') FROM sessions WHERE id = $1`, sessionID).
		Scan(&operator)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, "", ErrNoSessionRow
	}
	if err != nil {
		return false, "", fmt.Errorf("authn: read impersonation: %w", err)
	}
	return operator != "", operator, nil
}
