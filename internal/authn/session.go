package authn

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/catalyst/billing/internal/store"
	"github.com/jackc/pgx/v5"
)

var (
	// ErrNoSession is returned when a token does not identify a live session.
	ErrNoSession = errors.New("authn: no valid session")
	// ErrSessionExpired is returned when a session existed but has expired.
	ErrSessionExpired = errors.New("authn: session expired")
	// ErrCSRF is returned when a CSRF token does not match the session.
	ErrCSRF = errors.New("authn: csrf token mismatch")
)

// Session is an authenticated session.
type Session struct {
	ID        string
	UserID    string
	CreatedAt time.Time
	ExpiresAt time.Time
	// CSRFToken is the double-submit value the client must echo on mutations. It
	// is derived from the session's secret, so it can be handed to the browser
	// without storing anything extra.
	CSRFToken string
}

// User is the authenticated identity.
type User struct {
	ID                string
	Email             string
	DisplayName       string
	Status            string
	EmailVerified     bool
	CredentialVersion int
	// Roles are the staff roles held. Empty means a customer-only account.
	Roles []string
}

// IsStaff reports whether the user holds any staff role.
func (u User) IsStaff() bool { return len(u.Roles) > 0 }

// Config controls session behaviour.
type Config struct {
	// SessionLifetime is how long a session stays valid without re-authenticating.
	SessionLifetime time.Duration
	// AbsoluteLifetime caps a session's total age regardless of activity, so a
	// stolen token cannot be refreshed forever.
	AbsoluteLifetime time.Duration
	// ThrottleWindow is how far back failed attempts are counted.
	ThrottleWindow time.Duration
	// MaxAccountAttempts and MaxAddressAttempts bound failures per window.
	MaxAccountAttempts int
	MaxAddressAttempts int
	// MaxConcurrentHashes bounds how many password verifications run at once.
	//
	// Argon2id is deliberately expensive: one verification costs tens of MiB. Without
	// a bound, an unauthenticated attacker drives the process out of memory simply by
	// opening connections, because even an unknown account pays the full cost of the
	// dummy-hash path. The default is sized for a 4 GiB process running 64 MiB
	// verifications with headroom for everything else.
	MaxConcurrentHashes int
	// HashWaitTimeout bounds how long a login waits for a verification slot. Beyond
	// it the login is refused as overloaded rather than queued indefinitely, so a
	// flood cannot hold every request open.
	HashWaitTimeout time.Duration
	// Now is injectable for tests.
	Now func() time.Time
}

// DefaultConfig is a conservative starting point.
func DefaultConfig() Config {
	return Config{
		SessionLifetime:     12 * time.Hour,
		AbsoluteLifetime:    7 * 24 * time.Hour,
		ThrottleWindow:      15 * time.Minute,
		MaxAccountAttempts:  5,
		MaxAddressAttempts:  20,
		MaxConcurrentHashes: 8,
		HashWaitTimeout:     5 * time.Second,
		Now:                 func() time.Time { return time.Now().UTC() },
	}
}

func (c Config) withDefaults() Config {
	d := DefaultConfig()
	if c.SessionLifetime <= 0 {
		c.SessionLifetime = d.SessionLifetime
	}
	if c.AbsoluteLifetime <= 0 {
		c.AbsoluteLifetime = d.AbsoluteLifetime
	}
	if c.ThrottleWindow <= 0 {
		c.ThrottleWindow = d.ThrottleWindow
	}
	if c.MaxAccountAttempts <= 0 {
		c.MaxAccountAttempts = d.MaxAccountAttempts
	}
	if c.MaxAddressAttempts <= 0 {
		c.MaxAddressAttempts = d.MaxAddressAttempts
	}
	if c.MaxConcurrentHashes <= 0 {
		c.MaxConcurrentHashes = d.MaxConcurrentHashes
	}
	if c.HashWaitTimeout <= 0 {
		c.HashWaitTimeout = d.HashWaitTimeout
	}
	if c.Now == nil {
		c.Now = d.Now
	}
	return c
}

// Service authenticates users and manages sessions.
type Service struct {
	pool   *store.Pool
	hasher *Hasher
	cfg    Config
	// dummy is the hash verified against when the account does not exist, so the
	// response time does not reveal whether an address is registered.
	dummy string
	// hashSlots bounds concurrent password verifications. It is a semaphore rather
	// than a worker pool because each verification must complete for the caller to
	// get an answer; queuing is the only alternative to running them all at once.
	hashSlots chan struct{}
}

// ErrOverloaded is returned when the server cannot admit another password
// verification. It is distinct from a credential failure so a client does not
// interpret it as a wrong password, and so an operator can alert on it.
var ErrOverloaded = errors.New("authn: too many concurrent authentications")

// New constructs a Service.
//
// It returns an error because the dummy hash has to be generated, and failing to
// generate one would silently make the login endpoint an account oracle.
func New(pool *store.Pool, hasher *Hasher, cfg Config) (*Service, error) {
	dummy, err := prepareDummy(hasher)
	if err != nil {
		return nil, fmt.Errorf("authn: prepare dummy hash: %w", err)
	}
	resolved := cfg.withDefaults()
	return &Service{
		pool:      pool,
		hasher:    hasher,
		cfg:       resolved,
		dummy:     dummy,
		hashSlots: make(chan struct{}, resolved.MaxConcurrentHashes),
	}, nil
}

// LoginRequest is one authentication attempt.
type LoginRequest struct {
	Email     string
	Password  string
	IP        string
	UserAgent string
}

// LoginResult is a successful authentication.
type LoginResult struct {
	// Token is the session token. It is returned once and never stored, so the
	// caller must put it in a cookie immediately.
	Token   string
	Session Session
	User    User
}

// Login authenticates a user and creates a session.
//
// The failure modes are deliberately indistinguishable to the caller: an unknown
// account still performs a hash verification against a dummy hash so the response
// time does not reveal whether the account exists.
func (s *Service) Login(ctx context.Context, req LoginRequest) (LoginResult, error) {
	email := strings.ToLower(strings.TrimSpace(req.Email))
	if email == "" || req.Password == "" {
		// Still record the attempt: an empty-password probe is an attempt.
		s.recordAttempt(ctx, "address", req.IP, false)
		return LoginResult{}, ErrInvalidCredentials
	}

	// Throttling is checked before any hashing: a rejected attempt must cost
	// nothing, or the throttle itself becomes the amplifier.
	if err := s.checkThrottle(ctx, email, req.IP); err != nil {
		return LoginResult{}, err
	}

	// Take a verification slot before reading the user, so the expensive paths
	// (a real hash and the dummy hash) are both bounded by the same limit.
	release, err := s.acquireHashSlot(ctx)
	if err != nil {
		return LoginResult{}, err
	}
	defer release()

	var (
		userID     string
		hash       string
		status     string
		credVer    int
		verifiedAt *time.Time
		display    string
	)
	err = s.pool.QueryRow(ctx, `
		SELECT id, password_hash, status, credential_version, email_verified_at, display_name
		FROM users WHERE lower(email) = $1`, email).
		Scan(&userID, &hash, &status, &credVer, &verifiedAt, &display)
	if errors.Is(err, pgx.ErrNoRows) {
		// Burn comparable time so a missing account is not faster than a wrong
		// password. The result of this verification is deliberately ignored.
		_, _ = s.hasher.Verify(req.Password, s.dummy)
		s.recordAttempt(ctx, "account", email, false)
		s.recordAttempt(ctx, "address", req.IP, false)
		s.audit(ctx, "", "login_failed", "unknown account", req.IP)
		return LoginResult{}, ErrInvalidCredentials
	}
	if err != nil {
		return LoginResult{}, fmt.Errorf("authn: load user: %w", err)
	}

	params, err := s.hasher.Verify(req.Password, hash)
	if err != nil {
		s.recordAttempt(ctx, "account", email, false)
		s.recordAttempt(ctx, "address", req.IP, false)
		s.audit(ctx, userID, "login_failed", "wrong password", req.IP)
		return LoginResult{}, ErrInvalidCredentials
	}

	// The password was correct, so it is now safe to say why the account cannot
	// be used: this cannot help someone who does not already have the password.
	if status != "active" {
		s.recordAttempt(ctx, "account", email, false)
		s.audit(ctx, userID, "login_rejected", "account "+status, req.IP)
		return LoginResult{}, ErrAccountDisabled
	}

	// Opportunistically upgrade the hash now that the plaintext is available.
	if s.hasher.NeedsRehash(hash) || params.Memory < s.hasher.Params().Memory {
		if newHash, herr := s.hasher.Hash(req.Password); herr == nil {
			if _, uerr := s.pool.Exec(ctx,
				`UPDATE users SET password_hash = $2, updated_at = now() WHERE id = $1`,
				userID, newHash); uerr != nil {
				// A failed upgrade must not fail the login.
				s.audit(ctx, userID, "rehash_failed", uerr.Error(), req.IP)
			}
		}
	}

	// A successful login clears this account's failure history. Without it, a user
	// who mistyped their password a few times would be locked out by one more typo
	// even after proving who they are. The address bucket is deliberately left
	// alone: it exists to slow a single source down, and one valid account must not
	// be able to reset it for everyone behind that address.
	if _, cerr := s.pool.Exec(ctx, `
		DELETE FROM login_attempts
		WHERE bucket = 'account' AND subject = $1 AND NOT succeeded`, email); cerr != nil {
		s.audit(ctx, userID, "throttle_reset_failed", cerr.Error(), req.IP)
	}
	s.recordAttempt(ctx, "account", email, true)
	s.recordAttempt(ctx, "address", req.IP, true)

	roles, err := s.staffRoles(ctx, userID)
	if err != nil {
		return LoginResult{}, err
	}

	token, sess, err := s.createSession(ctx, userID, credVer, req.IP, req.UserAgent)
	if err != nil {
		return LoginResult{}, err
	}
	s.audit(ctx, userID, "login_succeeded", "", req.IP)

	return LoginResult{
		Token:   token,
		Session: sess,
		User: User{
			ID:                userID,
			Email:             email,
			DisplayName:       display,
			Status:            status,
			EmailVerified:     verifiedAt != nil,
			CredentialVersion: credVer,
			Roles:             roles,
		},
	}, nil
}

// acquireHashSlot takes one verification slot, or fails when the server is at
// capacity for longer than the configured wait.
func (s *Service) acquireHashSlot(ctx context.Context) (func(), error) {
	select {
	case s.hashSlots <- struct{}{}:
		return func() { <-s.hashSlots }, nil
	default:
	}

	timer := time.NewTimer(s.cfg.HashWaitTimeout)
	defer timer.Stop()
	select {
	case s.hashSlots <- struct{}{}:
		return func() { <-s.hashSlots }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timer.C:
		return nil, ErrOverloaded
	}
}

// createSession issues a new session and returns its token.
func (s *Service) createSession(ctx context.Context, userID string, credVersion int, ip, userAgent string) (string, Session, error) {
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

	var (
		id        string
		createdAt time.Time
		expiresAt time.Time
	)
	err = s.pool.QueryRow(ctx, `
		INSERT INTO sessions (user_id, token_hash, csrf_secret, credential_version,
			created_at, last_seen_at, expires_at, user_agent, ip)
		VALUES ($1, $2, $3, $4, $5, $5, $6, $7, NULLIF($8, '')::inet)
		RETURNING id, created_at, expires_at`,
		userID, HashToken(token), []byte(csrfToken), credVersion,
		now, expires, truncate(userAgent, 512), ip).
		Scan(&id, &createdAt, &expiresAt)
	if err != nil {
		return "", Session{}, fmt.Errorf("authn: create session: %w", err)
	}

	return token, Session{
		ID:        id,
		UserID:    userID,
		CreatedAt: createdAt,
		ExpiresAt: expiresAt,
		CSRFToken: csrfToken,
	}, nil
}

// Authenticate resolves a session token to a user.
//
// It checks four things: the session exists, it is not revoked, it has not
// expired, and its credential version still matches the user's. The last check is
// what makes a password change revoke every existing session without needing to
// enumerate them.
func (s *Service) Authenticate(ctx context.Context, token string) (Session, User, error) {
	if token == "" {
		return Session{}, User{}, ErrNoSession
	}
	now := s.cfg.Now()

	var (
		sess       Session
		credVer    int
		user       User
		revokedAt  *time.Time
		verified   *time.Time
		csrfSecret []byte
	)
	err := s.pool.QueryRow(ctx, `
		SELECT s.id, s.user_id, s.created_at, s.expires_at, s.credential_version,
		       s.revoked_at, s.csrf_secret,
		       u.email, u.display_name, u.status, u.credential_version, u.email_verified_at
		FROM sessions s
		JOIN users u ON u.id = s.user_id
		WHERE s.token_hash = $1`, HashToken(token)).
		Scan(&sess.ID, &sess.UserID, &sess.CreatedAt, &sess.ExpiresAt, &credVer,
			&revokedAt, &csrfSecret,
			&user.Email, &user.DisplayName, &user.Status, &user.CredentialVersion, &verified)
	if errors.Is(err, pgx.ErrNoRows) {
		return Session{}, User{}, ErrNoSession
	}
	if err != nil {
		return Session{}, User{}, fmt.Errorf("authn: load session: %w", err)
	}
	sess.CSRFToken = string(csrfSecret)

	if revokedAt != nil {
		return Session{}, User{}, ErrNoSession
	}
	if !now.Before(sess.ExpiresAt) {
		return Session{}, User{}, ErrSessionExpired
	}
	if credVer != user.CredentialVersion {
		// The password changed after this session was issued.
		return Session{}, User{}, ErrNoSession
	}
	if user.Status != "active" {
		return Session{}, User{}, ErrNoSession
	}

	user.ID = sess.UserID
	user.EmailVerified = verified != nil
	user.Roles, err = s.staffRoles(ctx, sess.UserID)
	if err != nil {
		return Session{}, User{}, err
	}

	// Slide the session on activity, but never past its absolute cap.
	if now.Sub(sess.ExpiresAt.Add(-s.cfg.SessionLifetime)) > time.Minute {
		next := now.Add(s.cfg.SessionLifetime)
		if absolute := sess.CreatedAt.Add(s.cfg.AbsoluteLifetime); absolute.Before(next) {
			next = absolute
		}
		if _, err := s.pool.Exec(ctx,
			`UPDATE sessions SET last_seen_at = $2, expires_at = $3 WHERE id = $1`,
			sess.ID, now, next); err != nil {
			return Session{}, User{}, fmt.Errorf("authn: refresh session: %w", err)
		}
		sess.ExpiresAt = next
	}

	return sess, user, nil
}

// VerifyCSRF checks a submitted CSRF token against the session's secret.
//
// The comparison is constant time. The token is bound to the session, so a token
// leaked from one session is useless in another, which is what a shared global
// secret cannot offer.
func VerifyCSRF(sess Session, submitted string) error {
	if submitted == "" {
		return ErrCSRF
	}
	if subtle.ConstantTimeCompare([]byte(sess.CSRFToken), []byte(submitted)) != 1 {
		return ErrCSRF
	}
	return nil
}

// LoginWithID issues a session for a user whose password was already proven.
//
// It exists for the second step of a multi-factor login: the password was verified when the
// challenge was created, so re-verifying it here would mean asking for it twice. It is not
// exported to the HTTP layer as a bypass — the only caller is the challenge completion path, which
// cannot be reached without a valid single-use challenge.
func (s *Service) LoginWithID(ctx context.Context, userID, ip, userAgent string) (LoginResult, error) {
	user, err := s.UserByID(ctx, userID)
	if err != nil {
		return LoginResult{}, err
	}
	if user.Status != "active" {
		return LoginResult{}, ErrAccountDisabled
	}

	var credVersion int
	if err := s.pool.QueryRow(ctx,
		`SELECT credential_version FROM users WHERE id = $1`, userID).Scan(&credVersion); err != nil {
		return LoginResult{}, fmt.Errorf("authn: read credential version: %w", err)
	}

	token, sess, err := s.createSession(ctx, userID, credVersion, ip, userAgent)
	if err != nil {
		return LoginResult{}, err
	}
	return LoginResult{Token: token, Session: sess, User: user}, nil
}

// UserByID loads a user with their roles resolved.
//
// It is the same projection a session login produces, so an API key authenticated request and a
// cookie authenticated request see exactly the same identity and the same permissions.
func (s *Service) UserByID(ctx context.Context, userID string) (User, error) {
	var (
		u        User
		verified *time.Time
	)
	err := s.pool.QueryRow(ctx, `
		SELECT id, email, COALESCE(display_name, ''), status, email_verified_at
		FROM users WHERE id = $1`, userID).
		Scan(&u.ID, &u.Email, &u.DisplayName, &u.Status, &verified)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, ErrUnknownAccount
	}
	if err != nil {
		return User{}, fmt.Errorf("authn: load user: %w", err)
	}
	u.EmailVerified = verified != nil
	roles, err := s.staffRoles(ctx, userID)
	if err != nil {
		return User{}, err
	}
	u.Roles = roles
	return u, nil
}

// Revoke ends one session.
func (s *Service) Revoke(ctx context.Context, token string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE sessions SET revoked_at = now()
		WHERE token_hash = $1 AND revoked_at IS NULL`, HashToken(token))
	if err != nil {
		return fmt.Errorf("authn: revoke session: %w", err)
	}
	return nil
}

// RevokeAll ends every session for a user, optionally keeping one.
//
// This is the "sign out everywhere" operation, and it is also what a password
// change calls.
func (s *Service) RevokeAll(ctx context.Context, userID, keepSessionID string) (int, error) {
	var tag interface{ RowsAffected() int64 }
	var err error
	if keepSessionID == "" {
		tag, err = s.pool.Exec(ctx, `
			UPDATE sessions SET revoked_at = now()
			WHERE user_id = $1 AND revoked_at IS NULL`, userID)
	} else {
		tag, err = s.pool.Exec(ctx, `
			UPDATE sessions SET revoked_at = now()
			WHERE user_id = $1 AND revoked_at IS NULL AND id <> $2`, userID, keepSessionID)
	}
	if err != nil {
		return 0, fmt.Errorf("authn: revoke sessions: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// ChangePassword sets a new password, bumps the credential version (which revokes
// every session issued before the change) and re-issues the caller's session.
func (s *Service) ChangePassword(ctx context.Context, userID, currentPassword, newPassword string) error {
	var hash string
	if err := s.pool.QueryRow(ctx, `SELECT password_hash FROM users WHERE id = $1`, userID).
		Scan(&hash); err != nil {
		return fmt.Errorf("authn: load user: %w", err)
	}
	if _, err := s.hasher.Verify(currentPassword, hash); err != nil {
		return ErrInvalidCredentials
	}
	newHash, err := s.hasher.Hash(newPassword)
	if err != nil {
		return err
	}
	// The version bump is what invalidates existing sessions; doing it in the same
	// statement means there is no window where the new password is live but old
	// sessions still work.
	if _, err := s.pool.Exec(ctx, `
		UPDATE users SET password_hash = $2, credential_version = credential_version + 1,
		                 updated_at = now()
		WHERE id = $1`, userID, newHash); err != nil {
		return fmt.Errorf("authn: change password: %w", err)
	}
	s.audit(ctx, userID, "password_changed", "", "")
	return nil
}

// staffRoles reads the user's staff roles.
func (s *Service) staffRoles(ctx context.Context, userID string) ([]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT role FROM staff_roles WHERE user_id = $1 ORDER BY role`, userID)
	if err != nil {
		return nil, fmt.Errorf("authn: load roles: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var r string
		if err := rows.Scan(&r); err != nil {
			return nil, fmt.Errorf("authn: scan role: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// checkThrottle rejects an attempt when either bucket is over its limit.
func (s *Service) checkThrottle(ctx context.Context, email, ip string) error {
	since := s.cfg.Now().Add(-s.cfg.ThrottleWindow)

	count := func(bucket, subject string, limit int) (bool, error) {
		if subject == "" {
			return false, nil
		}
		var n int
		err := s.pool.QueryRow(ctx, `
			SELECT count(*) FROM login_attempts
			WHERE bucket = $1 AND subject = $2 AND NOT succeeded AND created_at > $3`,
			bucket, subject, since).Scan(&n)
		if err != nil {
			return false, fmt.Errorf("authn: count attempts: %w", err)
		}
		return n >= limit, nil
	}

	blocked, err := count("account", email, s.cfg.MaxAccountAttempts)
	if err != nil {
		return err
	}
	if !blocked {
		blocked, err = count("address", ip, s.cfg.MaxAddressAttempts)
		if err != nil {
			return err
		}
	}
	if blocked {
		s.audit(ctx, "", "login_throttled", "", ip)
		return ErrTooManyAttempts
	}
	return nil
}

func (s *Service) recordAttempt(ctx context.Context, bucket, subject string, succeeded bool) {
	if subject == "" {
		return
	}
	// Recording is best-effort: a failure to write an audit row must not block a
	// legitimate login, and must not let an attacker disable throttling by
	// causing write errors.
	_, _ = s.pool.Exec(ctx, `
		INSERT INTO login_attempts (bucket, subject, succeeded, created_at)
		VALUES ($1, $2, $3, $4)`, bucket, subject, succeeded, s.cfg.Now())
}

func (s *Service) audit(ctx context.Context, userID, event, detail, ip string) {
	var uid any
	if userID != "" {
		uid = userID
	}
	_, _ = s.pool.Exec(ctx, `
		INSERT INTO auth_events (user_id, event, detail, ip)
		VALUES ($1, $2, $3, NULLIF($4, '')::inet)`, uid, event, detail, ip)
}

// dummyHash is a valid Argon2id hash of a random value, used to make the
// unknown-account path cost the same as the wrong-password path.
//
// It is generated once per Service rather than stored in a package variable: a
// package-level cache would be written at startup and read by every request, which
// is a data race under concurrency (and was caught by the race detector).
func (s *Service) dummyHash() string { return s.dummy }

// prepareDummy generates the dummy hash for this service. It is called by New, so
// a Service is always ready to authenticate.
func prepareDummy(h *Hasher) (string, error) {
	// 32 random bytes, base64'd: long enough to pass the length check and not a
	// password anyone could guess.
	secret, err := newToken(32)
	if err != nil {
		return "", err
	}
	return h.Hash(secret)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
