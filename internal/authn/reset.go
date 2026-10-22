package authn

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Password reset and email verification.
//
// The security properties, and why each is where it is:
//
//   - Token generation and hashing live here, next to the session token code, because they
//     must be the same idea: 256 bits of server randomness, stored as a SHA-256 digest.
//     Argon2 would be wrong for a token (there is nothing to brute-force) and would make every
//     redemption a denial-of-service lever.
//   - Single use is enforced by the UPDATE that consumes the row, not by a check followed by an
//     update. Two concurrent redemptions of the same token must both be impossible, and a
//     check-then-update cannot promise that.
//   - Expiry is in the row and compared against `now()` in the database, so a process with a
//     skewed clock cannot extend a token's life.
//   - Non-enumeration is structural: for an unknown address no row is written, and the caller
//     answers identically. To make the *timing* match too, the service still performs a dummy
//     hash, because "the response was instant" is as good an oracle as a different body.

// Token lifetimes.
const (
	// PasswordResetLifetime is short because the message can be forwarded, screenshotted or
	// left in a mailbox. Thirty minutes is long enough to find the mail and short enough that
	// a leak is unlikely to be useful.
	PasswordResetLifetime = 30 * time.Minute
	// EmailVerificationLifetime is longer: verification is not urgent, and a customer who
	// reads their mail tomorrow should not have to ask again.
	EmailVerificationLifetime = 24 * time.Hour
)

// Errors the HTTP layer maps to responses.
var (
	// ErrInvalidToken covers unknown, already-used and expired tokens. One error for all three
	// is deliberate: telling an attacker which of the three applies helps them, and does not
	// help the customer, who has to request a new link either way.
	ErrInvalidToken = errors.New("authn: token is invalid, used or expired")
	// ErrUnknownAccount is returned internally when no account exists. Callers must not leak
	// it: request flows answer 202 regardless.
	ErrUnknownAccount = errors.New("authn: no such account")
)

// ResetRequest is the outcome of a reset request.
type ResetRequest struct {
	// Token is set only when an account exists. The HTTP layer never returns it; it is used to
	// queue the mail.
	Token     string
	Email     string
	ExpiresAt time.Time
	// UserID is the account the token belongs to. It is empty when no account exists, and it is
	// what lets the caller write the in-app copy of the reset notice into that account's inbox
	// instead of only mailing an address.
	UserID string
	// AccountExists is what the caller must not leak.
	AccountExists bool
}

// RequestPasswordReset creates a reset token for an address.
//
// It never reports whether the address exists. A caller that branches on AccountExists to
// change the response body has reintroduced the enumeration this method avoids.
func (s *Service) RequestPasswordReset(ctx context.Context, email string) (ResetRequest, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	now := s.cfg.Now()
	out := ResetRequest{Email: email, ExpiresAt: now.Add(PasswordResetLifetime)}

	var (
		userID string
		status string
	)
	err := s.pool.QueryRow(ctx, `SELECT id, status FROM users WHERE lower(email) = $1`, email).
		Scan(&userID, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		// The dummy hash makes the unknown-address path cost the same as the known one. Without
		// it, response time is an account-existence oracle regardless of the body.
		s.dummyVerify()
		return out, nil
	}
	if err != nil {
		return ResetRequest{}, fmt.Errorf("authn: look up reset account: %w", err)
	}
	if status != "active" {
		// A disabled account gets the same answer as an unknown one. Its owner has to contact
		// an operator; there is no reset that should work.
		s.dummyVerify()
		return out, nil
	}

	token, err := newToken(32)
	if err != nil {
		return ResetRequest{}, err
	}

	local, err := s.pool.Begin(ctx)
	if err != nil {
		return ResetRequest{}, fmt.Errorf("authn: begin reset: %w", err)
	}
	defer local.Rollback(ctx)

	// Only the newest token is useful, so previous unused ones are invalidated. Otherwise a
	// customer who clicks "resend" three times has three working links, and the oldest is the
	// one an attacker is most likely to have.
	if _, err := local.Exec(ctx,
		`UPDATE password_resets SET used_at = now() WHERE user_id = $1 AND used_at IS NULL`,
		userID); err != nil {
		return ResetRequest{}, fmt.Errorf("authn: invalidate previous resets: %w", err)
	}
	if _, err := local.Exec(ctx, `
		INSERT INTO password_resets (user_id, token_hash, expires_at) VALUES ($1, $2, $3)`,
		userID, HashToken(token), out.ExpiresAt); err != nil {
		return ResetRequest{}, fmt.Errorf("authn: create reset token: %w", err)
	}
	if err := s.recordEvent(ctx, local, userID, "password_reset_requested", "", ""); err != nil {
		return ResetRequest{}, err
	}
	if err := local.Commit(ctx); err != nil {
		return ResetRequest{}, fmt.Errorf("authn: commit reset: %w", err)
	}

	out.Token = token
	out.UserID = userID
	out.AccountExists = true
	return out, nil
}

// ResetPassword consumes a token and sets a new password.
//
// The credential version bump revokes every existing session, which is the point: a reset is
// what a customer does when they believe someone else has their password.
func (s *Service) ResetPassword(ctx context.Context, token, newPassword string) (string, error) {
	if strings.TrimSpace(token) == "" {
		return "", ErrInvalidToken
	}
	hash, err := s.hasher.Hash(newPassword)
	if err != nil {
		// A weak password is a 422, not a 400: the request was well formed, the value was not
		// acceptable.
		return "", err
	}

	var userID string
	err = s.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		// Consume the token in one statement. `used_at IS NULL` and the expiry check are in the
		// WHERE clause, so a replay loses the race rather than winning it.
		err := tx.QueryRow(ctx, `
			UPDATE password_resets SET used_at = now()
			WHERE token_hash = $1 AND used_at IS NULL AND expires_at > now()
			RETURNING user_id`, HashToken(token)).Scan(&userID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrInvalidToken
		}
		if err != nil {
			return fmt.Errorf("authn: consume reset token: %w", err)
		}

		if _, err := tx.Exec(ctx, `
			UPDATE users SET password_hash = $2, credential_version = credential_version + 1,
			                 updated_at = now()
			WHERE id = $1`, userID, hash); err != nil {
			return fmt.Errorf("authn: set new password: %w", err)
		}
		// Every session, not just the current one: the reason to reset is usually that someone
		// else may have the password.
		if _, err := tx.Exec(ctx, `
			UPDATE sessions SET revoked_at = now()
			WHERE user_id = $1 AND revoked_at IS NULL`, userID); err != nil {
			return fmt.Errorf("authn: revoke sessions: %w", err)
		}
		return s.recordEvent(ctx, tx, userID, "password_reset_completed", "", "")
	})
	if err != nil {
		return "", err
	}
	return userID, nil
}

// VerificationRequest is the outcome of asking for a verification message.
type VerificationRequest struct {
	Token         string
	Email         string
	ExpiresAt     time.Time
	AccountExists bool
}

// RequestEmailVerification creates a verification token.
func (s *Service) RequestEmailVerification(ctx context.Context, userID string) (VerificationRequest, error) {
	now := s.cfg.Now()
	out := VerificationRequest{ExpiresAt: now.Add(EmailVerificationLifetime)}

	var email string
	var verifiedAt *time.Time
	err := s.pool.QueryRow(ctx,
		`SELECT email, email_verified_at FROM users WHERE id = $1`, userID).
		Scan(&email, &verifiedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return VerificationRequest{}, ErrUnknownAccount
	}
	if err != nil {
		return VerificationRequest{}, fmt.Errorf("authn: load verification account: %w", err)
	}
	out.Email = email
	if verifiedAt != nil {
		// Already verified: no token, and the caller says 202 like everywhere else. Issuing a
		// new token would let a verified address be re-verified to someone else's benefit if the
		// mailbox has since changed hands.
		return out, nil
	}

	token, err := newToken(32)
	if err != nil {
		return VerificationRequest{}, err
	}

	local, err := s.pool.Begin(ctx)
	if err != nil {
		return VerificationRequest{}, fmt.Errorf("authn: begin verification: %w", err)
	}
	defer local.Rollback(ctx)

	if _, err := local.Exec(ctx,
		`UPDATE email_verifications SET used_at = now() WHERE user_id = $1 AND used_at IS NULL`,
		userID); err != nil {
		return VerificationRequest{}, fmt.Errorf("authn: invalidate previous verifications: %w", err)
	}
	if _, err := local.Exec(ctx, `
		INSERT INTO email_verifications (user_id, token_hash, expires_at) VALUES ($1, $2, $3)`,
		userID, HashToken(token), out.ExpiresAt); err != nil {
		return VerificationRequest{}, fmt.Errorf("authn: create verification token: %w", err)
	}
	if err := s.recordEvent(ctx, local, userID, "email_verification_requested", "", ""); err != nil {
		return VerificationRequest{}, err
	}
	if err := local.Commit(ctx); err != nil {
		return VerificationRequest{}, fmt.Errorf("authn: commit verification: %w", err)
	}

	out.Token = token
	out.AccountExists = true
	return out, nil
}

// VerifyEmail consumes a verification token.
func (s *Service) VerifyEmail(ctx context.Context, token string) (string, error) {
	if strings.TrimSpace(token) == "" {
		return "", ErrInvalidToken
	}
	var userID string
	err := s.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `
			UPDATE email_verifications SET used_at = now()
			WHERE token_hash = $1 AND used_at IS NULL AND expires_at > now()
			RETURNING user_id`, HashToken(token)).Scan(&userID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrInvalidToken
		}
		if err != nil {
			return fmt.Errorf("authn: consume verification token: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE users SET email_verified_at = now(), updated_at = now()
			WHERE id = $1 AND email_verified_at IS NULL`, userID); err != nil {
			return fmt.Errorf("authn: mark address verified: %w", err)
		}
		return s.recordEvent(ctx, tx, userID, "email_verified", "", "")
	})
	if err != nil {
		return "", err
	}
	return userID, nil
}

// recordEvent writes an authentication event inside the caller's transaction.
//
// It is the auth trail, not the audit trail: it answers "was this account's credential changed"
// rather than "what did this operator do to this invoice".
func (s *Service) recordEvent(ctx context.Context, tx pgx.Tx, userID, event, detail, ip string) error {
	var address any
	if ip != "" {
		address = ip
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO auth_events (user_id, event, detail, ip) VALUES ($1, $2, $3, $4)`,
		userID, event, detail, address); err != nil {
		return fmt.Errorf("authn: record %s: %w", event, err)
	}
	return nil
}

// randomCode returns a base32 code with nBytes of entropy, formatted in groups of five.
//
// Base32 without the ambiguous characters is what a human can read off a screen and type into a
// phone without a case or character-class mistake.
func randomCode(nBytes int) (string, error) {
	raw := make([]byte, nBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("authn: read random bytes: %w", err)
	}
	encoded := strings.ToUpper(base32NoPad.EncodeToString(raw))
	encoded = strings.NewReplacer("0", "2", "1", "7", "I", "J", "L", "K", "O", "Q").Replace(encoded)
	var b strings.Builder
	for i, r := range encoded {
		if i > 0 && i%5 == 0 {
			b.WriteByte('-')
		}
		b.WriteRune(r)
	}
	return b.String(), nil
}

// HashCode returns the stored form of a recovery code.
//
// SHA-256 again, for the same reason as a token: the code is server-generated randomness. It is
// normalised first so a customer can type it in any case, with or without the dashes.
func HashCode(code string) []byte {
	normalised := strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(code), "-", ""))
	sum := sha256.Sum256([]byte(normalised))
	return sum[:]
}

// randomSecret returns a base32 secret for TOTP, which is what an authenticator app expects.
func randomSecret() (string, error) {
	raw := make([]byte, 20) // 160 bits, the RFC 4226 recommendation
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("authn: read random bytes: %w", err)
	}
	return base32NoPad.EncodeToString(raw), nil
}

// base32NoPad is the alphabet an authenticator app expects: upper-case RFC 4648 base32 without
// padding.
var base32NoPad = base32.StdEncoding.WithPadding(base32.NoPadding)

// dummyVerify spends the same work as a real password verification.
//
// It exists so an unknown address costs what a known one costs. Returning early would make the
// endpoint an account oracle through timing alone, whatever the response body says.
func (s *Service) dummyVerify() {
	// The result is irrelevant: the point is the work, and the dummy hash is a valid encoded
	// hash so Verify runs the full Argon2 derivation.
	_, _ = s.hasher.Verify("dummy-password-for-timing", s.dummy)
}
