package authn

// Staff user administration.
//
// Until this file existed the only way to create a login was `billing admin create-user`,
// which needs shell access to the host. That is a deployment bootstrap, not an onboarding
// story: an administrator could not add a support agent, and nobody could add a second
// administrator, without ssh. This file is the domain side of the screen that fixes it.
//
// Four rules shape what is here, and each one is a decision rather than an accident:
//
//   - **The last active administrator is a database invariant.** The checks in this file
//     produce a code an operator can act on, but the authoritative refusal is the trigger in
//     `0045_user_admin.sql`, which counts under an advisory transaction lock. A check here
//     alone would let two concurrent demotions each see the other's row and both succeed.
//
//   - **An operator cannot lock themselves out.** Disabling or deleting your own account is
//     refused outright; demoting yourself is allowed only while another active administrator
//     remains, because a second administrator demoting themselves is a legitimate handover.
//
//   - **Disable is the answer; delete is the exception.** Nothing in the schema cascades a
//     customer's invoices or an operator's audit trail away, so an account with a history
//     cannot be deleted at all. The delete path names what holds the account and points at
//     disabling it instead of failing with a foreign-key error.
//
//   - **A password set here obeys the same policy registration does.** It goes through the
//     same `Hasher.Hash` that `register` and the reset flow use, so a weak password is the
//     same `weak_password` refusal with the same message. The password is never returned by
//     any path in this file, and it never enters the audit trail (the redactor would catch
//     it, but it is not put there in the first place).
//
//   - **A staff account is also a customer of the business.** Creating a login creates its
//     billing identity in the same transaction, because an agent buys and holds services
//     like anybody else and "an operator links the two rows by hand" was the only way to
//     make that work — which is a step that is skipped once and then quietly broken. The
//     link is to *their own* record and grants nothing over anyone else's: every
//     customer-scoped query in the panel is scoped by that same link, and a staff member's
//     reach is still decided by the role. `ensureBillingIdentity` states the three rules
//     that keep it that way, and migration `0056` backfills the accounts that predate it.
//
// The role a user holds is a *name* rather than a table id on purpose: `staff_roles.role`
// is the column the session code already reads, and 0026 keeps it in step with `role_id`.
// A custom role therefore works here without this file knowing anything about the
// permission catalogue.

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/catalyst/billing/internal/audit"
	"github.com/catalyst/billing/internal/paginate"
	"github.com/catalyst/billing/internal/store"
	"github.com/jackc/pgx/v5"
)

// Errors the HTTP layer maps to a stable code.
var (
	// ErrUserNotFound is returned for an unknown user id.
	ErrUserNotFound = errors.New("authn: no such user")
	// ErrEmailTaken is returned when the address already has an account. It is a
	// distinguishable refusal here, unlike on the registration path: the caller is an
	// operator who typed the address, and "that address already has an account" is the
	// answer they need.
	ErrEmailTaken = errors.New("authn: that address already has an account")
	// ErrLastAdministrator is returned when a change would leave no active administrator.
	ErrLastAdministrator = errors.New("authn: the last active administrator cannot be removed")
	// ErrSelfLockout is returned when an operator targets their own account with a change
	// that would end their own access.
	ErrSelfLockout = errors.New("authn: an operator cannot lock themselves out")
	// ErrUserHasHistory is returned when deleting an account would break a record that has
	// to outlive it. The wrapped message names what holds the account.
	ErrUserHasHistory = errors.New("authn: the account is referenced by records that must outlive it")
	// ErrInvalidUser is returned for a malformed request: a bad status, a short search.
	ErrInvalidUser = errors.New("authn: invalid user request")
	// ErrInvalidEmail is returned for an address that cannot be one.
	ErrInvalidEmail = errors.New("authn: a valid email address is required")
	// ErrUnknownRole is returned for a role name the roles table does not hold.
	ErrUnknownRole = errors.New("authn: no such role")
	// ErrBillingAddressTaken is returned when the address already is *another login's*
	// billing account. Two logins on one billing account would give the second one access to
	// the first one's invoices through the customer-scoped path, so the account is refused
	// rather than linked.
	//
	// It wraps ErrEmailTaken deliberately: the staff-create route answers a taken address with
	// 409 `email_taken`, and the mapping lives in a handler this change does not own. Wrapping
	// keeps the refusal correct and truthful — the address *does* already have a record — while
	// the domain, an operator log and a test can still tell the two apart, and the message says
	// which kind of record is in the way.
	ErrBillingAddressTaken = fmt.Errorf("%w (the address is another login's billing record)", ErrEmailTaken)
)

// User statuses, mirroring the CHECK on users.status.
const (
	UserStatusActive   = "active"
	UserStatusDisabled = "disabled"
	UserStatusPending  = "pending"
)

// maxDisplayNameLength mirrors the limit the registration form applies.
const maxDisplayNameLength = 200

// maxUserEmailLength is the RFC 5321 forward-path limit.
const maxUserEmailLength = 254

// UserRecord is one account as the administration screen sees it.
type UserRecord struct {
	ID            string
	Email         string
	DisplayName   string
	Status        string
	EmailVerified bool
	// Role is the staff role name, empty for a customer-only account. A user holds at
	// most one: `staff_roles.user_id` is the primary key.
	Role      string
	CreatedAt time.Time
	// LastSignInAt is the last successful sign-in, or nil if the account has never signed
	// in. It comes from the authentication trail rather than from `sessions`, because a
	// revoked session is still evidence that somebody signed in.
	LastSignInAt *time.Time
	// CustomerID is the billing record this login owns, when it owns one. It is what tells
	// the screen whether a customer account can actually see an account yet: a login with
	// no customer record can sign in and find nothing.
	CustomerID string
}

// IsStaff reports whether the account holds a staff role.
func (u UserRecord) IsStaff() bool { return u.Role != "" }

// UserFilter narrows the user list.
type UserFilter struct {
	// Query searches the email and the display name. Fewer than two characters is
	// refused rather than scanned.
	Query string
	// Role matches a role name exactly.
	Role string
	// Staff selects staff only (true) or customers only (false). Nil means both.
	Staff *bool
	// Status matches users.status.
	Status string
}

// ListUsers returns one page of accounts, newest first, and the cursor of the next page.
func (s *Service) ListUsers(ctx context.Context, f UserFilter, limit int, cursor string) ([]UserRecord, string, error) {
	limit = paginate.Clamp(limit)

	var (
		args  []any
		where []string
	)

	if q := strings.TrimSpace(f.Query); q != "" {
		if len([]rune(q)) < 2 {
			// One character matches most of the table and is a sequential scan. Refusing it
			// is better than a screen that looks broken because it is slow.
			return nil, "", fmt.Errorf("%w: the search needs at least two characters", ErrInvalidUser)
		}
		pattern := "%" + escapeLikePattern(strings.ToLower(q)) + "%"
		args = append(args, pattern)
		where = append(where, fmt.Sprintf(
			"(lower(u.email) LIKE $%d ESCAPE '\\' OR lower(u.display_name) LIKE $%d ESCAPE '\\')",
			len(args), len(args)))
	}
	if role := strings.TrimSpace(f.Role); role != "" {
		args = append(args, role)
		where = append(where, fmt.Sprintf("sr.role = $%d", len(args)))
	}
	if f.Staff != nil {
		if *f.Staff {
			where = append(where, "sr.role IS NOT NULL")
		} else {
			where = append(where, "sr.role IS NULL")
		}
	}
	if status := strings.TrimSpace(f.Status); status != "" {
		switch status {
		case UserStatusActive, UserStatusDisabled, UserStatusPending:
		default:
			return nil, "", fmt.Errorf("%w: unknown status %q", ErrInvalidUser, status)
		}
		args = append(args, status)
		where = append(where, fmt.Sprintf("u.status = $%d", len(args)))
	}
	if cursor != "" {
		sortKey, id, err := paginate.Decode(cursor)
		if err != nil {
			return nil, "", err
		}
		createdAt, err := time.Parse(time.RFC3339Nano, sortKey)
		if err != nil {
			return nil, "", fmt.Errorf("%w: the cursor is not a position in this list", paginate.ErrInvalidCursor)
		}
		args = append(args, createdAt, id)
		where = append(where, fmt.Sprintf("(u.created_at, u.id) < ($%d, $%d)", len(args)-1, len(args)))
	}

	whereClause := ""
	if len(where) > 0 {
		whereClause = "WHERE " + strings.Join(where, " AND ")
	}

	args = append(args, limit+1)
	stmt := `
		SELECT u.id, u.email, u.display_name, u.status, u.email_verified_at IS NOT NULL,
		       COALESCE(sr.role, ''), u.created_at, last.sign_in_at, COALESCE(own.customer_id::text, '')
		FROM users u
		LEFT JOIN staff_roles sr ON sr.user_id = u.id
		LEFT JOIN LATERAL (
			SELECT max(e.created_at) AS sign_in_at
			FROM auth_events e
			WHERE e.user_id = u.id AND e.event = 'login_succeeded'
		) last ON true
		LEFT JOIN LATERAL (
			SELECT ca.customer_id FROM customer_accounts ca
			WHERE ca.user_id = u.id ORDER BY ca.created_at LIMIT 1
		) own ON true
		` + whereClause + `
		ORDER BY u.created_at DESC, u.id DESC
		LIMIT $` + strconv.Itoa(len(args))

	rows, err := s.pool.Query(ctx, stmt, args...)
	if err != nil {
		return nil, "", fmt.Errorf("authn: list users: %w", err)
	}
	defer rows.Close()

	out := make([]UserRecord, 0, limit)
	for rows.Next() {
		var (
			rec       UserRecord
			createdAt time.Time
			signInAt  *time.Time
		)
		if err := rows.Scan(&rec.ID, &rec.Email, &rec.DisplayName, &rec.Status,
			&rec.EmailVerified, &rec.Role, &createdAt, &signInAt, &rec.CustomerID); err != nil {
			return nil, "", fmt.Errorf("authn: scan user: %w", err)
		}
		rec.CreatedAt = createdAt
		rec.LastSignInAt = signInAt
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("authn: read users: %w", err)
	}

	next := ""
	if len(out) > limit {
		out = out[:limit]
		last := out[len(out)-1]
		next = paginate.Encode(last.CreatedAt.UTC().Format(time.RFC3339Nano), last.ID)
	}
	return out, next, nil
}

// UserByIDRecord loads one account with its role.
func (s *Service) UserByIDRecord(ctx context.Context, userID string) (UserRecord, error) {
	var (
		rec       UserRecord
		createdAt time.Time
		signInAt  *time.Time
	)
	err := s.pool.QueryRow(ctx, `
		SELECT u.id, u.email, u.display_name, u.status, u.email_verified_at IS NOT NULL,
		       COALESCE(sr.role, ''), u.created_at, last.sign_in_at, COALESCE(own.customer_id::text, '')
		FROM users u
		LEFT JOIN staff_roles sr ON sr.user_id = u.id
		LEFT JOIN LATERAL (
			SELECT max(e.created_at) AS sign_in_at
			FROM auth_events e
			WHERE e.user_id = u.id AND e.event = 'login_succeeded'
		) last ON true
		LEFT JOIN LATERAL (
			SELECT ca.customer_id FROM customer_accounts ca
			WHERE ca.user_id = u.id ORDER BY ca.created_at LIMIT 1
		) own ON true
		WHERE u.id = $1`, userID).
		Scan(&rec.ID, &rec.Email, &rec.DisplayName, &rec.Status, &rec.EmailVerified,
			&rec.Role, &createdAt, &signInAt, &rec.CustomerID)
	if errors.Is(err, pgx.ErrNoRows) {
		return UserRecord{}, ErrUserNotFound
	}
	if err != nil {
		return UserRecord{}, fmt.Errorf("authn: load user: %w", err)
	}
	rec.CreatedAt = createdAt
	rec.LastSignInAt = signInAt
	return rec, nil
}

// CreateUserRequest is one account to create.
type CreateUserRequest struct {
	Email       string
	DisplayName string
	Password    string
	// Role is a role name: one of the four system roles or a custom one. Empty creates a
	// customer-only account, which is the same screen: onboarding a customer who rang up
	// is not a different job from onboarding an agent.
	Role string
	// EmailVerified records that the operator has confirmed the address. It is a decision
	// the caller makes explicitly; the default belongs to the handler, not here.
	EmailVerified bool
	// ActorID is the operator creating the account, recorded as `staff_roles.granted_by`.
	ActorID string
}

// CreateUser creates an account and, in the same transaction, the role it holds.
//
// The two rows commit together because a staff account with no role is an account that can
// sign in and do nothing: a half-created agent is worse than a failed create.
func (s *Service) CreateUser(ctx context.Context, req CreateUserRequest, auditor audit.Auditor) (UserRecord, error) {
	email := normaliseEmail(req.Email)
	if !validEmail(email) {
		return UserRecord{}, ErrInvalidEmail
	}
	name := strings.TrimSpace(req.DisplayName)
	if name == "" || len([]rune(name)) > maxDisplayNameLength {
		return UserRecord{}, fmt.Errorf("%w: a display name is required (at most %d characters)",
			ErrInvalidUser, maxDisplayNameLength)
	}
	role := strings.TrimSpace(req.Role)

	// Hash before the transaction: Argon2 is deliberately slow, and holding a row lock (or
	// an open transaction) for its duration would serialise every other create behind it.
	hash, err := s.hasher.Hash(req.Password)
	if err != nil {
		// ErrWeakPassword passes through unchanged so the caller answers with the same
		// code registration does.
		return UserRecord{}, err
	}

	var userID string
	err = s.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if role != "" {
			if err := roleExists(ctx, tx, role); err != nil {
				return err
			}
		}

		var verified any
		if req.EmailVerified {
			verified = time.Now().UTC()
		}
		err := tx.QueryRow(ctx, `
			INSERT INTO users (email, password_hash, display_name, email_verified_at)
			VALUES ($1, $2, $3, $4)
			RETURNING id`, email, hash, name, verified).Scan(&userID)
		if err != nil {
			if isUniqueViolation(err) {
				return fmt.Errorf("%w: %s", ErrEmailTaken, email)
			}
			return fmt.Errorf("authn: create user: %w", err)
		}

		if role != "" {
			if _, err := tx.Exec(ctx, `
				INSERT INTO staff_roles (user_id, role, granted_by) VALUES ($1, $2, $3)`,
				userID, role, nullableID(req.ActorID)); err != nil {
				return fmt.Errorf("authn: grant role: %w", err)
			}
		}

		// Staff and customers alike: an account that cannot be billed cannot buy, and
		// "the operator will link it afterwards" is a step that gets forgotten. It runs
		// after the role row so the billing record is never the only thing a failed create
		// left behind.
		identity, err := ensureBillingIdentity(ctx, tx, userID, email, name)
		if err != nil {
			return err
		}

		// The authentication trail answers "when did this account appear"; the audit trail
		// answers "who created it". Both are written here, in the same transaction, so
		// neither can describe a creation that rolled back.
		if err := s.recordEvent(ctx, tx, userID, "user_created_by_operator", req.ActorID, ""); err != nil {
			return err
		}
		return recordAudit(ctx, tx, auditor, "user.create", "user", userID, map[string]any{
			"email":          email,
			"display_name":   name,
			"role":           role,
			"email_verified": req.EmailVerified,
			// An access review asks "did creating this login attach it to a billing record
			// that already existed", so the answer is in the trail rather than inferred.
			"billing_account":         identity.CustomerID,
			"billing_account_adopted": identity.Adopted,
		})
	})
	if err != nil {
		return UserRecord{}, err
	}
	return s.UserByIDRecord(ctx, userID)
}

// UpdateUserRequest is a partial update. A nil field is left alone.
type UpdateUserRequest struct {
	DisplayName *string
	Email       *string
	// Role replaces the staff role. An empty string demotes the account to a customer.
	Role *string
	// Status is active, disabled or pending.
	Status  *string
	ActorID string
}

// UpdateUser applies a partial update to an account.
//
// PATCH rather than PUT on purpose: the screen sends the field the operator touched, and a
// full replacement would let a dialog that only renamed somebody silently clear their role.
func (s *Service) UpdateUser(ctx context.Context, userID string, req UpdateUserRequest, auditor audit.Auditor) (UserRecord, error) {
	var changes map[string]any

	err := s.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		changes = map[string]any{}

		var email, name, status, role string
		err := tx.QueryRow(ctx, `
			SELECT u.email, u.display_name, u.status, COALESCE(sr.role, '')
			FROM users u
			LEFT JOIN staff_roles sr ON sr.user_id = u.id
			WHERE u.id = $1
			FOR UPDATE OF u`, userID).
			Scan(&email, &name, &status, &role)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrUserNotFound
		}
		if err != nil {
			return fmt.Errorf("authn: load user: %w", err)
		}

		newEmail, newName, newStatus, newRole := email, name, status, role

		if req.Email != nil {
			candidate := normaliseEmail(*req.Email)
			if !validEmail(candidate) {
				return ErrInvalidEmail
			}
			newEmail = candidate
		}
		if req.DisplayName != nil {
			candidate := strings.TrimSpace(*req.DisplayName)
			if candidate == "" || len([]rune(candidate)) > maxDisplayNameLength {
				return fmt.Errorf("%w: a display name is required (at most %d characters)",
					ErrInvalidUser, maxDisplayNameLength)
			}
			newName = candidate
		}
		if req.Status != nil {
			switch *req.Status {
			case UserStatusActive, UserStatusDisabled, UserStatusPending:
				newStatus = *req.Status
			default:
				return fmt.Errorf("%w: unknown status %q", ErrInvalidUser, *req.Status)
			}
		}
		if req.Role != nil {
			newRole = strings.TrimSpace(*req.Role)
			if newRole != "" {
				if err := roleExists(ctx, tx, newRole); err != nil {
					return err
				}
			}
		}

		// Locking yourself out is refused before anything is written, because the point of
		// the check is that the operator cannot do it even once.
		if req.ActorID != "" && req.ActorID == userID {
			if newStatus != UserStatusActive && status == UserStatusActive {
				return fmt.Errorf("%w: you cannot disable the account you are signed in as",
					ErrSelfLockout)
			}
		}

		// Losing administrator (by demotion or by disabling) is the same act as far as the
		// remaining administrators are concerned. The trigger in 0045 is the authority; this
		// check exists so the common case gets a code instead of a constraint violation.
		losesAdmin := role == string(RoleAdministrator) &&
			(newRole != string(RoleAdministrator) || newStatus != UserStatusActive)
		if losesAdmin {
			remaining, err := activeAdministrators(ctx, tx, userID)
			if err != nil {
				return err
			}
			if remaining == 0 {
				return ErrLastAdministrator
			}
		}

		if newEmail != email {
			// A changed address is unverified until somebody proves it again. Keeping the
			// old verification would let a typo or a deliberate edit inherit a confirmation
			// that was given for a different mailbox.
			//
			// The billing record's address is deliberately *not* rewritten. It is the address
			// documents were issued to and the one future invoices print, so it follows the
			// customer's billing details rather than their login; silently re-addressing a
			// customer's invoices is a decision for the customers screen, which shows what it
			// is about to change. It also keeps adoption safe: the record keeps the address it
			// was created with, so a later login at the *old* address still finds it and is
			// still refused it (it belongs to this account).
			if _, err := tx.Exec(ctx, `
				UPDATE users SET email = $2, email_verified_at = NULL, updated_at = now()
				WHERE id = $1`, userID, newEmail); err != nil {
				if isUniqueViolation(err) {
					return fmt.Errorf("%w: %s", ErrEmailTaken, newEmail)
				}
				return fmt.Errorf("authn: update email: %w", err)
			}
			changes["email"] = newEmail
			changes["previous_email"] = email
		}
		if newName != name {
			if _, err := tx.Exec(ctx,
				`UPDATE users SET display_name = $2, updated_at = now() WHERE id = $1`,
				userID, newName); err != nil {
				return fmt.Errorf("authn: update display name: %w", err)
			}
			changes["display_name"] = newName
		}
		if newStatus != status {
			if _, err := tx.Exec(ctx,
				`UPDATE users SET status = $2, updated_at = now() WHERE id = $1`,
				userID, newStatus); err != nil {
				return fmt.Errorf("authn: update status: %w", err)
			}
			changes["status"] = newStatus
			changes["previous_status"] = status
			if newStatus != UserStatusActive {
				// A disabled account already cannot authenticate, but leaving live session
				// rows behind would make the session list of a disabled account look like an
				// account somebody is still using.
				if _, err := tx.Exec(ctx, `
					UPDATE sessions SET revoked_at = now()
					WHERE user_id = $1 AND revoked_at IS NULL`, userID); err != nil {
					return fmt.Errorf("authn: revoke sessions: %w", err)
				}
				changes["sessions_revoked"] = true
			}
		}
		if newRole != role {
			if newRole == "" {
				if _, err := tx.Exec(ctx,
					`DELETE FROM staff_roles WHERE user_id = $1`, userID); err != nil {
					return fmt.Errorf("authn: remove role: %w", err)
				}
			} else if _, err := tx.Exec(ctx, `
				INSERT INTO staff_roles (user_id, role, granted_by) VALUES ($1, $2, $3)
				ON CONFLICT (user_id) DO UPDATE
					SET role = EXCLUDED.role, granted_by = EXCLUDED.granted_by`,
				userID, newRole, nullableID(req.ActorID)); err != nil {
				return fmt.Errorf("authn: assign role: %w", err)
			}
			changes["role"] = newRole
			changes["previous_role"] = role
		}

		if len(changes) == 0 {
			// Nothing changed, and an audit row saying so would be noise. The caller still
			// gets the record it asked about.
			return nil
		}
		return recordAudit(ctx, tx, auditor, "user.update", "user", userID, changes)
	})
	if err != nil {
		// The trigger is the authority, so its refusal has to reach the caller as the same
		// error the pre-check returns rather than as a 500.
		if store.SQLState(err) == store.StateRestrictViolation {
			return UserRecord{}, fmt.Errorf("%w: %v", ErrLastAdministrator, err)
		}
		return UserRecord{}, err
	}
	return s.UserByIDRecord(ctx, userID)
}

// DeleteUser removes an account that nothing depends on.
//
// It is deliberately hard. An account that has done anything has `audit_events` rows, and a
// customer's login is referenced by `customer_accounts` and by the invoices behind it. Those
// references are NO ACTION in the schema, so the database would refuse the delete anyway —
// this function turns that into a named refusal that says what holds the account and points
// at disabling it, which is the operation an operator almost always wants.
//
// The account's own authentication history is the one thing that does *not* block: it is a
// login trail with a retention policy rather than an append-only record, so it is removed
// with the account. Without that, every account created through the screen would be
// permanently undeletable, because creating one writes an authentication event.
//
// The billing identity is the second thing that does not block, and it has to be stated
// because 0056 gives every login one. A record `ensureBillingIdentity` created for this login
// (`customers.provisioned_for_login`) is removed with it: it holds nothing but an address, and
// leaving it behind would put a customer nobody can reach into the customer list. A record an
// operator or an import created is *not* deleted — it carries a name, an address and possibly
// attribution that outlive a login — so that case still refuses with "disable it instead".
func (s *Service) DeleteUser(ctx context.Context, userID, actorID string, auditor audit.Auditor) error {
	return s.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var role string
		err := tx.QueryRow(ctx, `
			SELECT COALESCE(sr.role, '')
			FROM users u LEFT JOIN staff_roles sr ON sr.user_id = u.id
			WHERE u.id = $1
			FOR UPDATE OF u`, userID).Scan(&role)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrUserNotFound
		}
		if err != nil {
			return fmt.Errorf("authn: load user: %w", err)
		}

		if actorID != "" && actorID == userID {
			return fmt.Errorf("%w: you cannot delete the account you are signed in as", ErrSelfLockout)
		}

		if role == string(RoleAdministrator) {
			remaining, err := activeAdministrators(ctx, tx, userID)
			if err != nil {
				return err
			}
			if remaining == 0 {
				return ErrLastAdministrator
			}
		}

		// What holds this account. One query so the refusal can name every reason rather
		// than the first one a series of EXISTS checks happened to hit.
		//
		// `auth_events` is deliberately not in this list. It is the account's *own*
		// authentication history — a login trail with a 180-day retention policy — not an
		// append-only audit record, so it is deleted with the account below. The audit trail
		// (`audit_events`) is the evidence, and it is what refuses the delete.
		//
		// The billing records are listed one by one because since 0056 every login has one, so
		// "this account is linked to a customer record" can no longer be a refusal on its own:
		// it would make every account undeletable, including the mistyped address created a
		// minute ago. What decides is whether the record *is the login's own*, which is
		// `customers.provisioned_for_login` — true only for a record this package created
		// because a login needed one. A record an operator or an import created carries a name,
		// an address and possibly attribution that outlive the login, and is never deleted
		// here.
		type billingLink struct {
			customerID  string
			provisioned bool
			shared      bool
		}
		var (
			links     []billingLink
			auditRows int
			invoices  int
		)
		if err := tx.QueryRow(ctx,
			`SELECT (SELECT count(*) FROM audit_events WHERE actor_id = $1),
			        (SELECT count(*) FROM customer_accounts ca
			           JOIN invoices i ON i.customer_id = ca.customer_id
			          WHERE ca.user_id = $1)`, userID).
			Scan(&auditRows, &invoices); err != nil {
			return fmt.Errorf("authn: check references: %w", err)
		}

		rows, err := tx.Query(ctx, `
			SELECT ca.customer_id, c.provisioned_for_login,
			       EXISTS (SELECT 1 FROM customer_accounts other
			                WHERE other.customer_id = ca.customer_id
			                  AND other.user_id <> ca.user_id)
			FROM customer_accounts ca
			JOIN customers c ON c.id = ca.customer_id
			WHERE ca.user_id = $1
			ORDER BY ca.created_at`, userID)
		if err != nil {
			return fmt.Errorf("authn: check billing records: %w", err)
		}
		for rows.Next() {
			var l billingLink
			if err := rows.Scan(&l.customerID, &l.provisioned, &l.shared); err != nil {
				rows.Close()
				return fmt.Errorf("authn: scan billing record: %w", err)
			}
			links = append(links, l)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("authn: read billing records: %w", err)
		}

		switch {
		case invoices > 0:
			return fmt.Errorf("%w: the account is the portal login for a customer with %d invoice(s); disable it instead",
				ErrUserHasHistory, invoices)
		case auditRows > 0:
			return fmt.Errorf("%w: %d audit row(s) name it as the actor; disable it instead",
				ErrUserHasHistory, auditRows)
		}
		for _, l := range links {
			switch {
			case l.shared:
				return fmt.Errorf("%w: the account shares a billing record with another login; disable it instead",
					ErrUserHasHistory)
			case !l.provisioned:
				// The record was not created for this login (an import, the customers screen, or a
				// self-registration), so it may be the only place its name, address and attribution
				// exist. Refuse rather than guess.
				return fmt.Errorf("%w: the account is the portal login for a billing record that outlives it; disable it instead",
					ErrUserHasHistory)
			}
		}

		// The billing records this account was given are removed with it. They were created
		// because a login needed one, they hold no invoices (the check above), and every
		// other reference — a service, an order, a subscription, a billing profile — is a
		// foreign key that refuses the delete below rather than letting billing history be
		// cascaded away.
		for _, l := range links {
			if _, err := tx.Exec(ctx,
				`DELETE FROM customer_accounts WHERE user_id = $1 AND customer_id = $2`,
				userID, l.customerID); err != nil {
				return fmt.Errorf("authn: unlink billing record: %w", err)
			}
			if _, err := tx.Exec(ctx, `DELETE FROM customers WHERE id = $1`, l.customerID); err != nil {
				if store.SQLState(err) == store.StateForeignKeyViolation {
					return fmt.Errorf("%w: the account's billing record still holds services, orders or other billing history; disable it instead",
						ErrUserHasHistory)
				}
				return fmt.Errorf("authn: delete billing record: %w", err)
			}
		}

		// The role row is removed explicitly: `staff_roles.user_id` is NO ACTION, and a
		// cascade here would let a delete silently strip a role the guard above just
		// counted.
		if _, err := tx.Exec(ctx, `DELETE FROM staff_roles WHERE user_id = $1`, userID); err != nil {
			return fmt.Errorf("authn: remove role: %w", err)
		}
		// The login trail goes with the account, for the reason in the comment above. Every
		// other table that references a user cascades (sessions, resets, verifications, MFA
		// credentials, API keys), so this is the only reference that has to be removed by
		// hand.
		if _, err := tx.Exec(ctx, `DELETE FROM auth_events WHERE user_id = $1`, userID); err != nil {
			return fmt.Errorf("authn: remove authentication history: %w", err)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM users WHERE id = $1`, userID); err != nil {
			if store.SQLState(err) == store.StateForeignKeyViolation {
				return fmt.Errorf("%w: another record still references it; disable it instead",
					ErrUserHasHistory)
			}
			return fmt.Errorf("authn: delete user: %w", err)
		}
		return recordAudit(ctx, tx, auditor, "user.delete", "user", userID, map[string]any{
			"role": role,
		})
	})
}

// SetPasswordByOperator sets a password for another account.
//
// **The decision: set it directly, rather than mailing a reset token.** The reason is that
// this screen is the onboarding path for an installation that may have no mail transport
// configured at all — the same situation that made the CLI the only way to create the first
// account. A reset link that cannot be delivered would leave the operator with no way to
// hand somebody their first password. The cost is that the operator knows the password, so
// the screen tells them to have the user change it, and the audit row records who set it.
//
// Every session of the target is revoked, by the same credential-version bump the reset
// flow uses: whoever held the old password may still be holding a live session. The
// password itself is never returned and never enters the audit trail.
func (s *Service) SetPasswordByOperator(ctx context.Context, userID, password, actorID string, auditor audit.Auditor) (int, error) {
	// Hash before the transaction, for the same reason CreateUser does.
	hash, err := s.hasher.Hash(password)
	if err != nil {
		return 0, err
	}

	revoked := 0
	err = s.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var exists bool
		if err := tx.QueryRow(ctx,
			`SELECT true FROM users WHERE id = $1 FOR UPDATE`, userID).Scan(&exists); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrUserNotFound
			}
			return fmt.Errorf("authn: load user: %w", err)
		}

		// The version bump is what makes the change take effect for sessions: an old session
		// stops authenticating because its recorded version no longer matches.
		if _, err := tx.Exec(ctx, `
			UPDATE users SET password_hash = $2, credential_version = credential_version + 1,
			                 updated_at = now()
			WHERE id = $1`, userID, hash); err != nil {
			return fmt.Errorf("authn: set password: %w", err)
		}
		tag, err := tx.Exec(ctx, `
			UPDATE sessions SET revoked_at = now()
			WHERE user_id = $1 AND revoked_at IS NULL`, userID)
		if err != nil {
			return fmt.Errorf("authn: revoke sessions: %w", err)
		}
		revoked = int(tag.RowsAffected())

		if err := s.recordEvent(ctx, tx, userID, "password_set_by_operator", actorID, ""); err != nil {
			return err
		}
		return recordAudit(ctx, tx, auditor, "user.password_set", "user", userID, map[string]any{
			"sessions_revoked": revoked,
			"set_by":           actorID,
		})
	})
	if err != nil {
		return 0, err
	}
	return revoked, nil
}

// ------------------------------------------------------------- role usage --

// RoleUsage is how many accounts hold a role.
//
// It is the access-review question — "who can do this" — answered per role rather than per
// user, because the screen that grants a permission is the screen that has to show who
// already has it. Active is counted separately: a role held only by disabled accounts grants
// nothing, and an operator deciding whether a role is safe to delete needs to know that.
type RoleUsage struct {
	Holders int
	Active  int
}

// RoleHolderCounts returns the number of accounts holding each role, keyed by role name.
func (s *Service) RoleHolderCounts(ctx context.Context) (map[string]RoleUsage, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT r.name,
		       count(sr.user_id),
		       count(sr.user_id) FILTER (WHERE u.status = 'active')
		FROM roles r
		LEFT JOIN staff_roles sr ON sr.role = r.name
		LEFT JOIN users u ON u.id = sr.user_id
		GROUP BY r.name`)
	if err != nil {
		return nil, fmt.Errorf("authn: count role holders: %w", err)
	}
	defer rows.Close()

	out := map[string]RoleUsage{}
	for rows.Next() {
		var (
			name string
			use  RoleUsage
		)
		if err := rows.Scan(&name, &use.Holders, &use.Active); err != nil {
			return nil, fmt.Errorf("authn: scan role holders: %w", err)
		}
		out[name] = use
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("authn: read role holders: %w", err)
	}
	return out, nil
}

// UpdateRoleDetails renames a custom role and/or changes its description.
//
// Renaming is safe because `staff_roles.role` is a foreign key to `roles.name` with ON UPDATE
// CASCADE: the accounts holding the role follow the rename in the same statement, and their
// permissions are resolved by name at read time, so nothing has to be re-granted. A system
// role is refused, as it is for every other write: the four seeded roles are what the
// separation of duties is written against.
func (s *Service) UpdateRoleDetails(ctx context.Context, roleID string, name, description *string, auditor audit.Auditor) (RoleRecord, error) {
	var changes map[string]any
	err := s.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		changes = map[string]any{}

		var (
			currentName, currentDescription string
			system                          bool
		)
		err := tx.QueryRow(ctx, `
			SELECT name, description, is_system FROM roles WHERE id = $1 FOR UPDATE`, roleID).
			Scan(&currentName, &currentDescription, &system)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrRoleNotFound
		}
		if err != nil {
			return fmt.Errorf("authn: load role: %w", err)
		}
		if system {
			return ErrSystemRole
		}

		newName, newDescription := currentName, currentDescription
		if name != nil {
			candidate := strings.ToLower(strings.TrimSpace(*name))
			if err := validateRoleName(candidate); err != nil {
				return err
			}
			newName = candidate
		}
		if description != nil {
			candidate := strings.TrimSpace(*description)
			if len(candidate) > 500 {
				return fmt.Errorf("%w: the description is too long", ErrInvalidRole)
			}
			newDescription = candidate
		}
		if newName == currentName && newDescription == currentDescription {
			return nil
		}

		if _, err := tx.Exec(ctx,
			`UPDATE roles SET name = $2, description = $3 WHERE id = $1`,
			roleID, newName, newDescription); err != nil {
			if isUniqueViolation(err) {
				return fmt.Errorf("%w: a role named %q already exists", ErrInvalidRole, newName)
			}
			return fmt.Errorf("authn: update role: %w", err)
		}
		if newName != currentName {
			changes["name"] = newName
			changes["previous_name"] = currentName
		}
		if newDescription != currentDescription {
			changes["description"] = newDescription
		}
		return recordAudit(ctx, tx, auditor, "role.update", "role", roleID, changes)
	})
	if err != nil {
		return RoleRecord{}, err
	}
	return s.RoleByID(ctx, roleID)
}

// ---------------------------------------------------------------- helpers --

// activeAdministrators counts active administrator accounts other than `excluding`.
//
// It mirrors the database function of the same name so the handler and the trigger agree
// about what "an administrator remains" means. The trigger is still the authority: this
// count can be stale the moment it returns, which is exactly why the trigger re-counts
// under a lock.
func activeAdministrators(ctx context.Context, tx pgx.Tx, excluding string) (int, error) {
	var n int
	if err := tx.QueryRow(ctx,
		`SELECT active_administrators($1)`, excluding).Scan(&n); err != nil {
		return 0, fmt.Errorf("authn: count administrators: %w", err)
	}
	return n, nil
}

// roleExists reports whether the roles table holds the name.
//
// The lookup is against the table rather than against the compiled-in set because a custom
// role is a row: refusing to assign one would make the roles screen pointless.
func roleExists(ctx context.Context, tx pgx.Tx, name string) error {
	var found string
	err := tx.QueryRow(ctx, `SELECT name FROM roles WHERE name = $1`, name).Scan(&found)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: %q", ErrUnknownRole, name)
	}
	if err != nil {
		return fmt.Errorf("authn: look up role: %w", err)
	}
	return nil
}

// billingIdentity is the billing record a login was given, and how it got it.
type billingIdentity struct {
	// CustomerID is the `customers` row the login now owns.
	CustomerID string
	// Adopted is true when the record already existed — an imported customer whose address
	// nobody could sign in with — and false when this call created it. The audit row records
	// it because "did creating this login attach it to an existing billing record" is the
	// question an access review asks about exactly this feature.
	Adopted bool
}

// EnsureBillingIdentity gives an existing login its billing identity, for a caller that writes
// the `users` row itself.
//
// The screen goes through `CreateUser`, which does this in the same transaction. The CLI
// bootstrap cannot: `billing admin create-user` writes the row directly, because it has to work
// before the panel does. Without this the first administrator of a fresh installation — the
// account an operator is most likely to buy a test server with — would be the one account that
// cannot buy anything, because migration `0056` had already run by the time it was created.
//
// It is the same function, so it is the same three rules: an existing link wins, an unowned
// billing record at the address is adopted, and a record another login owns is refused with
// `ErrBillingAddressTaken`.
func EnsureBillingIdentity(ctx context.Context, tx pgx.Tx, userID, email, name string) (string, error) {
	identity, err := ensureBillingIdentity(ctx, tx, userID, email, name)
	if err != nil {
		return "", err
	}
	return identity.CustomerID, nil
}

// ensureBillingIdentity guarantees that a login has a billing account of its own.
//
// **The decision: every account gets one, staff included.** A staff member is also a
// customer of the business — they buy a server, they hold a subscription, they get an
// invoice — and before this the only way to make that work was for an operator to create a
// `customers` row by hand and link it, which is the kind of manual step that is done wrong
// once and then left broken. A login with no billing identity cannot buy anything at all:
// checkout and the portal both resolve the customer through `customer_accounts`.
//
// **What this must not do is grant anything.** The row written here is a `customer_accounts`
// link, and every customer-scoped query in the panel is scoped by exactly that link
// (`WHERE ca.user_id = $n`). A link to *their own* record therefore adds no reach over
// anybody else's: a staff member's invoice access is still decided by `invoice:read:all`,
// and their customer view sees only the record this function made for them. Three rules are
// what keeps that true, and each one is deliberate:
//
//   - **An existing link wins.** The function is idempotent, so a second call cannot produce
//     a second identity and no caller has to know whether one exists.
//   - **An address that already has a billing record with no login is adopted, not
//     duplicated.** `customers.email` is unique, one address is one billing account, and
//     that record is the same person — the case where an operator imported the customer
//     months ago and is now giving them a way in.
//   - **An address whose billing record already belongs to another login is refused.** It is
//     never adopted and never shared: a second link would let this account read somebody
//     else's invoices through the customer-scoped path, which is the privilege escalation
//     this feature must not introduce. It is also the one case where the unique index on
//     `lower(customers.email)` would fail anyway, so the refusal is a code rather than a
//     constraint violation.
func ensureBillingIdentity(ctx context.Context, tx pgx.Tx, userID, email, name string) (billingIdentity, error) {
	var existing string
	err := tx.QueryRow(ctx, `
		SELECT customer_id FROM customer_accounts
		WHERE user_id = $1
		ORDER BY created_at
		LIMIT 1`, userID).Scan(&existing)
	switch {
	case err == nil:
		return billingIdentity{CustomerID: existing, Adopted: true}, nil
	case !errors.Is(err, pgx.ErrNoRows):
		return billingIdentity{}, fmt.Errorf("authn: look up billing identity: %w", err)
	}

	var (
		customerID string
		linked     bool
	)
	err = tx.QueryRow(ctx, `
		SELECT c.id, EXISTS (SELECT 1 FROM customer_accounts ca WHERE ca.customer_id = c.id)
		FROM customers c
		WHERE lower(c.email) = $1
		ORDER BY c.created_at
		LIMIT 1`, email).Scan(&customerID, &linked)

	created := false
	switch {
	case err == nil && linked:
		// The address is somebody else's billing account. Refuse: linking the two logins
		// would hand this one their invoices, and "the operator typed the address" is not
		// proof that the two are the same person.
		return billingIdentity{}, fmt.Errorf("%w: %s", ErrBillingAddressTaken, email)
	case err == nil:
		// Adopted below.
	case !errors.Is(err, pgx.ErrNoRows):
		return billingIdentity{}, fmt.Errorf("authn: look up billing record: %w", err)
	default:
		if err := tx.QueryRow(ctx, `
			INSERT INTO customers (email, name, provisioned_for_login)
			VALUES ($1, $2, true)
			RETURNING id`,
			email, name).Scan(&customerID); err != nil {
			if isUniqueViolation(err) {
				// A concurrent writer took the address between the read and the insert.
				return billingIdentity{}, fmt.Errorf("%w: %s", ErrBillingAddressTaken, email)
			}
			return billingIdentity{}, fmt.Errorf("authn: create billing record: %w", err)
		}
		created = true
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO customer_accounts (user_id, customer_id, role) VALUES ($1, $2, 'owner')`,
		userID, customerID); err != nil {
		return billingIdentity{}, fmt.Errorf("authn: link billing record: %w", err)
	}
	return billingIdentity{CustomerID: customerID, Adopted: !created}, nil
}

// recordAudit writes one audit row inside the caller's transaction.
//
// A nil auditor is allowed: the domain tests drive these methods without an HTTP request, and
// an audit row is the request's job rather than the domain's. When an auditor is supplied the
// row commits or rolls back with the change it describes.
func recordAudit(ctx context.Context, tx pgx.Tx, auditor audit.Auditor,
	action, targetType, targetID string, changes map[string]any) error {
	if auditor == nil {
		return nil
	}
	return auditor.RecordAction(ctx, tx, action, targetType, targetID, changes)
}

// nullableID renders an empty id as SQL NULL.
//
// `granted_by` and the actor column of `auth_events` are nullable references: an empty
// string is not a uuid and would fail the insert, so the two have to be distinguished.
func nullableID(id string) any {
	if id == "" {
		return nil
	}
	return id
}

// normaliseEmail lower-cases and trims an address, matching the unique index on
// lower(email).
func normaliseEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

// validEmail is the same conservative syntactic check internal/register applies.
//
// It is duplicated rather than imported because `register` imports this package: the
// dependency already points this way. The rule is deliberately narrow — the only authority on
// whether an address works is a verification message — and the copy is kept honest by
// TestUserEmailValidationMatchesRegistration in the HTTP layer, which compares the two on a
// table of addresses.
func validEmail(email string) bool {
	if email == "" || len(email) > maxUserEmailLength {
		return false
	}
	if strings.ContainsAny(email, " \t\r\n<>\"(),;:\\") {
		return false
	}
	at := strings.LastIndexByte(email, '@')
	if at <= 0 || at == len(email)-1 {
		return false
	}
	local, domain := email[:at], email[at+1:]
	if local == "" || strings.Contains(local, "@") {
		return false
	}
	if !strings.Contains(domain, ".") || strings.HasPrefix(domain, ".") ||
		strings.HasSuffix(domain, ".") || strings.Contains(domain, "..") {
		return false
	}
	return true
}

// escapeLikePattern escapes the LIKE metacharacters so a search for "50%" looks for a
// literal percent sign rather than matching everything.
func escapeLikePattern(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 8)
	for _, r := range s {
		switch r {
		case '\\', '%', '_':
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}
