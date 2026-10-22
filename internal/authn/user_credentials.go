package authn

// Credential operations an operator performs on somebody else's account.
//
// `users.go` owns the account lifecycle — create, edit, disable, delete — and it already has
// `SetPasswordByOperator`, which is the staff side of "set this customer's password". This file
// is the second half of the same screen: the two-factor state of an account, and an operator's
// reset of it.
//
// Why a reset exists at all
// -------------------------
// `DisableMFA` in mfa.go is *self* service, on purpose: it demands the account's own password
// **and** a current TOTP code, because a stolen session alone must not remove a second factor and
// a stolen password alone must not either. That is the right rule for a customer acting on their
// own account and the wrong rule for the situation support actually faces — "I lost my phone and
// my recovery codes". A staff member cannot supply either secret, and the only remaining answers
// would be "post us the old phone" or a SQL statement against `mfa_credentials`.
//
// So `ResetMFA` is a separate operation with a different justification, not a weakened
// `DisableMFA`:
//
//   - **It removes the enrolment, it does not reveal it.** The encrypted secret, the recovery
//     codes and any live login challenge are deleted. An operator cannot learn the customer's
//     secret, cannot generate a code, and cannot sign in as the customer with the second factor.
//     Removing a factor is destructive to the *customer's* security, never additive to the
//     operator's access — which is why it is auditable rather than silent.
//
//   - **It is audited with both parties.** The audit row names the operator as the actor and the
//     target account as the target, and the target's own authentication trail
//     (`auth_events.mfa_reset_by_operator`) records it too, because the customer's "what happened
//     to my account" question is answered from that trail rather than from the operator's audit
//     log. `mfa_reset_at` / `mfa_reset_by` on the account are the current fact a screen can read
//     without joining the audit table.
//
//   - **It leaves a demand behind.** `mfa_enrolment_required` is set, so the account's next
//     session is told to enrol a new factor and the portal will not leave the enrolment screen
//     until one is confirmed (`ConfirmMFA` clears the flag). Without that, a reset would quietly
//     *downgrade* the account: the customer would be back to a password-only login and nobody
//     would ever be asked to fix it. The reset removes a factor that no longer works; the demand
//     is what makes it a reset rather than a removal.
//
//   - **It does not revoke sessions.** `SetPasswordByOperator` does, because the old password may
//     be in somebody else's hands. A lost phone is not a compromised session, and ending every
//     session of a customer who is already locked out of their second factor would add an
//     outage to an inconvenience. The operator who believes the *account* is compromised has the
//     password route, which revokes everything.
//
// The permission is `staff:manage`, the same one that governs creating logins, setting passwords
// and impersonating: taking a second factor away is an act of account administration, not a
// support ticket reply.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/catalyst/billing/internal/audit"
	"github.com/jackc/pgx/v5"
)

// MFAStatus is the second-factor state of an account, as the security screens read it.
type MFAStatus struct {
	// HasMFA reports a *confirmed* TOTP credential. An unconfirmed enrolment is not a second
	// factor: it does not challenge a login, so reporting it as one would be a lie a customer
	// relies on.
	HasMFA bool
	// EnrolledAt is when the current credential was confirmed.
	EnrolledAt *time.Time
	// RecoveryCodesRemaining is how many unused recovery codes are left. It is the number that
	// turns "I have recovery codes somewhere" into "I have two left", which is the warning that
	// arrives before the lockout rather than after it.
	RecoveryCodesRemaining int
	// EnrolmentRequired is set by a staff reset and cleared by a confirmed enrolment.
	EnrolmentRequired bool
	// ResetAt and ResetBy name the last staff reset, when there was one.
	ResetAt *time.Time
	ResetBy string
}

// MFAResetResult reports what a reset removed.
type MFAResetResult struct {
	// RecoveryCodesRemoved is how many recovery codes went with the credential.
	RecoveryCodesRemoved int
	// TargetEmail is the account the reset applied to, echoed so the caller can audit and report
	// it without a second read.
	TargetEmail string
}

// ResetMFA removes another account's second factor and demands a new one.
//
// It is refused when the account has no confirmed factor: a reset of nothing would still set the
// enrolment demand, which would tell a customer who never enrolled that they must enrol. The
// refusal is `ErrMFANotEnrolled`, the same error the self-service path answers with.
func (s *Service) ResetMFA(ctx context.Context, userID, actorID string, auditor audit.Auditor) (MFAResetResult, error) {
	var out MFAResetResult

	err := s.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var email string
		err := tx.QueryRow(ctx,
			`SELECT email FROM users WHERE id = $1 FOR UPDATE`, userID).Scan(&email)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrUserNotFound
		}
		if err != nil {
			return fmt.Errorf("authn: load account for mfa reset: %w", err)
		}
		out.TargetEmail = email

		// Counted before the delete, and counted from the *confirmed* rows only: an abandoned
		// enrolment is not a factor, and deleting it is housekeeping rather than a reset.
		var confirmed int
		if err := tx.QueryRow(ctx, `
			SELECT count(*) FROM mfa_credentials
			WHERE user_id = $1 AND kind = 'totp' AND confirmed_at IS NOT NULL`,
			userID).Scan(&confirmed); err != nil {
			return fmt.Errorf("authn: count mfa credentials: %w", err)
		}
		if confirmed == 0 {
			return ErrMFANotEnrolled
		}

		// The recovery codes are counted so the audit row can say how many credentials were
		// destroyed. The number is a fact about the reset; the codes themselves are hashes and
		// are never read out.
		var codes int
		if err := tx.QueryRow(ctx,
			`SELECT count(*) FROM mfa_recovery_codes WHERE user_id = $1`, userID).Scan(&codes); err != nil {
			return fmt.Errorf("authn: count recovery codes: %w", err)
		}
		out.RecoveryCodesRemoved = codes

		// All three tables, in the same transaction as the demand. A reset that removed the
		// credential but left a recovery code would leave a working second factor behind; a
		// reset that removed both but left a challenge would let a half-finished login through.
		if _, err := tx.Exec(ctx,
			`DELETE FROM mfa_credentials WHERE user_id = $1`, userID); err != nil {
			return fmt.Errorf("authn: delete mfa credential: %w", err)
		}
		if _, err := tx.Exec(ctx,
			`DELETE FROM mfa_recovery_codes WHERE user_id = $1`, userID); err != nil {
			return fmt.Errorf("authn: delete recovery codes: %w", err)
		}
		if _, err := tx.Exec(ctx,
			`DELETE FROM mfa_challenges WHERE user_id = $1`, userID); err != nil {
			return fmt.Errorf("authn: delete challenges: %w", err)
		}

		// The demand, and who left it. `ConfirmMFA` clears all three.
		if _, err := tx.Exec(ctx, `
			UPDATE users
			SET mfa_enrolment_required = true, mfa_reset_at = now(), mfa_reset_by = $2,
			    updated_at = now()
			WHERE id = $1`, userID, nullableID(actorID)); err != nil {
			return fmt.Errorf("authn: record mfa reset: %w", err)
		}

		// The target's own trail. The detail names the operator, because the customer reading
		// their login history is the person who needs to know who removed their second factor.
		if err := s.recordEvent(ctx, tx, userID, "mfa_reset_by_operator",
			fmt.Sprintf("reset by operator %s", actorID), ""); err != nil {
			return err
		}

		return recordAudit(ctx, tx, auditor, "user.mfa_reset", "user", userID, map[string]any{
			"target_user_id":         userID,
			"target_email":           email,
			"reset_by":               actorID,
			"recovery_codes_removed": codes,
			"enrolment_required":     true,
		})
	})
	if err != nil {
		return MFAResetResult{}, err
	}
	return out, nil
}

// MFAStatusFor reads the second-factor state of an account.
//
// It answers for any account, not only the caller's, because the same three facts are what the
// staff screen needs to decide whether "Reset two-factor" is a meaningful button.
func (s *Service) MFAStatusFor(ctx context.Context, userID string) (MFAStatus, error) {
	var (
		out       MFAStatus
		enrolled  *time.Time
		resetAt   *time.Time
		resetBy   *string
		required  bool
		remaining int
	)
	err := s.pool.QueryRow(ctx, `
		SELECT u.mfa_enrolment_required, u.mfa_reset_at, u.mfa_reset_by::text,
		       (SELECT m.confirmed_at FROM mfa_credentials m
		         WHERE m.user_id = u.id AND m.kind = 'totp' AND m.confirmed_at IS NOT NULL),
		       (SELECT count(*) FROM mfa_recovery_codes r
		         WHERE r.user_id = u.id AND r.used_at IS NULL)
		FROM users u WHERE u.id = $1`, userID).
		Scan(&required, &resetAt, &resetBy, &enrolled, &remaining)
	if errors.Is(err, pgx.ErrNoRows) {
		return MFAStatus{}, ErrUserNotFound
	}
	if err != nil {
		return MFAStatus{}, fmt.Errorf("authn: read mfa status: %w", err)
	}

	out.EnrolmentRequired = required
	out.ResetAt = resetAt
	if resetBy != nil {
		out.ResetBy = *resetBy
	}
	out.EnrolledAt = enrolled
	out.HasMFA = enrolled != nil
	out.RecoveryCodesRemaining = remaining
	return out, nil
}
