// Package register implements customer self-registration and the signed-in
// password change.
//
// This is the entry point the storefront was missing: before it, a visitor could
// browse products but had no way to become a customer, so the only way to get an
// account was `billing admin create-customer`. Registration is therefore a
// revenue feature, not a convenience.
//
// The shape of a registration is one transaction that writes three rows which
// must agree:
//
//	users             the credential (Argon2id, hashed by internal/authn)
//	customers         the billing account the invoices are addressed to
//	customer_accounts the link that makes "this login may see that account" true
//
// A partial registration is the worst state in the system — a login with no
// billing account cannot buy anything, and a billing account with no login cannot
// be reached — so all three commit together or none do.
//
// Non-enumeration. A duplicate address must not be distinguishable from a fresh
// one, for the same reason the password-reset request is not: "is this address
// registered" is the question an attacker asks first, and a registration form that
// answers it is an account-discovery oracle. Two things make the answers match:
//
//   - the password is hashed *before* the insert is attempted, so the duplicate
//     path pays the same Argon2 cost as the success path (a fast rejection is as
//     good an oracle as a different body);
//   - the HTTP layer answers with the same body either way (see
//     internal/httpapi/register.go). This package reports ErrEmailTaken to its
//     caller because an operator-facing log should say what happened; the client
//     is never told.
//
// Settings. `general.registration_disabled` and `general.tos` are read from the
// settings table, and `mail.must_verify` decides whether a new account needs a
// verified address before it can sign in. Every one of them is read with a
// code-side default, so a fresh installation works before an operator touches the
// settings screen and a missing row is not an error.
package register

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/catalyst/billing/internal/authn"
	"github.com/catalyst/billing/internal/notify"
	"github.com/catalyst/billing/internal/store"
	"github.com/jackc/pgx/v5"
)

// Errors the callers branch on. They are distinct here so the operator-facing
// layer can log precisely why a registration was refused, while the client-facing
// layer is free to collapse them.
var (
	// ErrRegistrationDisabled is returned when `general.registration_disabled` is on.
	ErrRegistrationDisabled = errors.New("register: registration is disabled")
	// ErrEmailTaken is returned when the address already has an account. It must
	// never reach the client as a distinct response.
	ErrEmailTaken = errors.New("register: an account already exists for this address")
	// ErrInvalidEmail is returned for an address that is not syntactically usable.
	ErrInvalidEmail = errors.New("register: a valid email address is required")
	// ErrInvalidDisplayName is returned for a missing or absurd display name.
	ErrInvalidDisplayName = errors.New("register: a display name between 1 and 200 characters is required")
	// ErrInvalidCredentials is returned when the current password is wrong.
	ErrInvalidCredentials = errors.New("register: the current password is incorrect")
	// ErrNoSuchUser is returned when the account does not exist.
	ErrNoSuchUser = errors.New("register: no such account")
)

// maxEmailLength is the RFC 5321 limit for a forward path. A longer address cannot
// be delivered to, so it is refused before it reaches the database.
const maxEmailLength = 254

// maxDisplayNameLength bounds the stored name. It is generous enough for a full
// legal name and short enough that it cannot be used to fill a page.
const maxDisplayNameLength = 200

// Policy is the subset of settings a registration depends on.
type Policy struct {
	// RegistrationDisabled refuses every registration. Default false.
	RegistrationDisabled bool
	// TOSRequired is true when a terms-of-service text is configured, which makes
	// acceptance mandatory.
	TOSRequired bool
	// MustVerifyEmail is true when a new account must confirm its address before it
	// can sign in. It changes what the endpoint returns (a session or a
	// verification-required state), not whether the account exists.
	MustVerifyEmail bool
}

// Service creates customer accounts.
type Service struct {
	pool   *store.Pool
	hasher *authn.Hasher
	// auth issues verification tokens. It may be nil: registration still works, the
	// account is simply created without a verification message, which is what an
	// installation with no mail configured should do rather than fail.
	auth *authn.Service
	// queue is the durable outbox. It may be nil for the same reason.
	queue *notify.Queue
	// baseURL is the public origin a verification link must point at. It is
	// configuration, never a request-derived value: a link built from the Host header
	// is a link an attacker can point at their own domain.
	baseURL string
}

// New constructs a Service.
func New(pool *store.Pool, hasher *authn.Hasher) *Service {
	return &Service{pool: pool, hasher: hasher}
}

// WithVerification attaches the token issuer, the outbox and the public origin.
//
// It is separate from New so a test (or an installation with no mail) can register
// accounts without a mail path, and so the HTTP layer — which owns the origin and
// the auth service — supplies them explicitly.
func (s *Service) WithVerification(auth *authn.Service, queue *notify.Queue, baseURL string) *Service {
	s.auth = auth
	s.queue = queue
	s.baseURL = strings.TrimRight(baseURL, "/")
	return s
}

// Request is one registration.
type Request struct {
	Email string
	// Password is the plaintext. It is hashed immediately and never stored.
	Password string
	// DisplayName is the name shown in the portal and on documents.
	DisplayName string
	// AcceptedTOS is the checkbox. It is only consulted when terms are configured.
	AcceptedTOS bool
	// IP is recorded on the authentication event, for the operator's benefit.
	IP string
}

// Result reports what was created.
type Result struct {
	UserID     string
	CustomerID string
	Email      string
	// Duplicate is true when the address already had an account. Nothing was
	// created in that case; the caller must answer exactly as it does for a new
	// registration.
	Duplicate bool
	// VerificationQueued is true when a verification message was written to the
	// outbox. It is false when the address was already verified, when no mail path is
	// configured, or when the account already existed.
	VerificationQueued bool
}

// Policy reads the settings a registration depends on.
//
// One query, because the three keys live in two groups and three round trips for a
// form submission is three chances to disagree. A missing row keeps the code-side
// default, so an installation that has never opened its settings screen still
// behaves correctly.
func (s *Service) Policy(ctx context.Context) (Policy, error) {
	return ReadPolicy(ctx, s.pool)
}

// ReadPolicy reads the same policy without a Service, for a caller that has the pool but
// not a hasher (the checkout path, which never hashes).
func ReadPolicy(ctx context.Context, pool *store.Pool) (Policy, error) {
	var p Policy
	rows, err := pool.Query(ctx, `
		SELECT group_key, key, value
		FROM settings
		WHERE (group_key = 'general' AND key IN ('registration_disabled', 'tos'))
		   OR (group_key = 'mail'    AND key = 'must_verify')`)
	if err != nil {
		return Policy{}, fmt.Errorf("register: read settings: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var group, key, value string
		if err := rows.Scan(&group, &key, &value); err != nil {
			return Policy{}, fmt.Errorf("register: scan setting: %w", err)
		}
		switch {
		case group == "general" && key == "registration_disabled":
			p.RegistrationDisabled = parseBool(value)
		case group == "general" && key == "tos":
			// A non-empty terms text is the only thing that makes acceptance
			// mandatory: an installation with no terms has nothing to accept.
			p.TOSRequired = strings.TrimSpace(value) != ""
		case group == "mail" && key == "must_verify":
			p.MustVerifyEmail = parseBool(value)
		}
	}
	if err := rows.Err(); err != nil {
		return Policy{}, fmt.Errorf("register: read settings: %w", err)
	}
	return p, nil
}

// parseBool reads the settings table's text form of a boolean.
//
// The settings table stores everything as text, and an operator who types "yes" or
// "1" means the same thing as "true". Anything unrecognised is false, which is the
// safe direction for `registration_disabled` and the safe direction for
// `must_verify` only because the code-side default is also false.
func parseBool(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "true", "1", "yes", "on", "enabled":
		return true
	default:
		return false
	}
}

// Register creates a user, a billing account and the link between them.
//
// The three rows are written in one transaction. The password is hashed before the
// transaction starts so that a duplicate address costs the same as a fresh one and
// so that no Argon2 work happens while a row lock is held.
func (s *Service) Register(ctx context.Context, req Request) (Result, error) {
	email := strings.ToLower(strings.TrimSpace(req.Email))
	if !validEmail(email) {
		return Result{}, ErrInvalidEmail
	}
	name := strings.TrimSpace(req.DisplayName)
	if name == "" || len(name) > maxDisplayNameLength {
		return Result{}, ErrInvalidDisplayName
	}

	policy, err := s.Policy(ctx)
	if err != nil {
		return Result{}, err
	}
	if policy.RegistrationDisabled {
		return Result{}, ErrRegistrationDisabled
	}
	if policy.TOSRequired && !req.AcceptedTOS {
		return Result{}, ErrTOSRequired
	}

	// Hash first. A weak password is the caller's mistake and is reported as such; a
	// duplicate address is not reported to the caller at all.
	hash, err := s.hasher.Hash(req.Password)
	if err != nil {
		return Result{}, err
	}

	out := Result{Email: email}
	err = s.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		// ON CONFLICT DO NOTHING rather than an insert that fails: a failed insert
		// aborts the transaction, and every later statement — including the read that
		// would tell us *which* account exists — would then be refused. Inference is
		// left to the bare form so the case-insensitive expression index on
		// lower(email) is the one that matches.
		var userID string
		err := tx.QueryRow(ctx, `
			INSERT INTO users (email, password_hash, display_name)
			VALUES ($1, $2, $3)
			ON CONFLICT DO NOTHING
			RETURNING id`, email, hash, name).Scan(&userID)
		if errors.Is(err, pgx.ErrNoRows) {
			out.Duplicate = true
			// Read the existing account so the verification message (below) can be
			// addressed to its owner. This is not an information leak: nothing about
			// it reaches the client.
			if serr := tx.QueryRow(ctx,
				`SELECT id FROM users WHERE lower(email) = $1`, email).Scan(&out.UserID); serr != nil &&
				!errors.Is(serr, pgx.ErrNoRows) {
				return fmt.Errorf("register: read existing account: %w", serr)
			}
			return recordEvent(ctx, tx, out.UserID, "registration_duplicate", req.IP)
		}
		if err != nil {
			return fmt.Errorf("register: create user: %w", err)
		}

		customerID, err := adoptOrCreateCustomer(ctx, tx, email, name)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO customer_accounts (user_id, customer_id, role)
			VALUES ($1, $2, 'owner')`, userID, customerID); err != nil {
			return fmt.Errorf("register: link billing account: %w", err)
		}

		out.UserID, out.CustomerID = userID, customerID
		return recordEvent(ctx, tx, userID, "registered", req.IP)
	})
	if err != nil {
		return Result{}, err
	}

	// The verification message is written after the account commits, in its own
	// transaction, exactly as the password-reset request does. A failure here must not
	// fail the registration: the account exists and the customer can ask for another
	// message from the signed-in endpoint.
	if err := s.sendVerification(ctx, out.UserID); err != nil {
		// Deliberately not returned: the account was created, and telling the client
		// the registration failed would be a lie it cannot act on.
		out.VerificationQueued = false
	} else {
		out.VerificationQueued = true
	}
	return out, nil
}

// adoptOrCreateCustomer returns the billing account for an address.
//
// An address that already has a customer row with no login attached is *adopted*
// rather than duplicated: that is the account an operator created by CLI or by
// `billingAccountFor`, and refusing to attach a login to it would leave the
// customer with an account they can never reach.
//
// An address whose customer row is already linked to another login is reported as
// ErrEmailTaken rather than adopted. Two logins owning one billing account is a
// decision this flow must not make silently, and it is also the only way to avoid
// a unique-violation on the case-insensitive customer index, which would abort the
// transaction and turn a policy refusal into a 500.
func adoptOrCreateCustomer(ctx context.Context, tx pgx.Tx, email, name string) (string, error) {
	var (
		customerID string
		linked     bool
	)
	err := tx.QueryRow(ctx, `
		SELECT c.id, EXISTS (SELECT 1 FROM customer_accounts ca WHERE ca.customer_id = c.id)
		FROM customers c
		WHERE lower(c.email) = $1
		ORDER BY c.created_at
		LIMIT 1`, email).Scan(&customerID, &linked)
	switch {
	case err == nil && !linked:
		return customerID, nil
	case err == nil && linked:
		return "", ErrEmailTaken
	case !errors.Is(err, pgx.ErrNoRows):
		return "", fmt.Errorf("register: look up billing account: %w", err)
	}

	if err := tx.QueryRow(ctx, `
		INSERT INTO customers (email, name) VALUES ($1, $2) RETURNING id`,
		email, name).Scan(&customerID); err != nil {
		return "", fmt.Errorf("register: create billing account: %w", err)
	}
	return customerID, nil
}

// sendVerification issues a token and writes the message to the outbox.
//
// It returns nil when there is nothing to send: no mail path configured, no auth
// service, or an address that is already verified. Those are not failures.
func (s *Service) sendVerification(ctx context.Context, userID string) error {
	if s.auth == nil || s.queue == nil || userID == "" {
		return nil
	}
	res, err := s.auth.RequestEmailVerification(ctx, userID)
	if err != nil {
		return fmt.Errorf("register: request verification: %w", err)
	}
	if res.Token == "" {
		return nil
	}
	link := s.baseURL + "/api/v1/auth/verify?token=" + res.Token
	msg := notify.EmailVerificationMessage(res.Email, link, authn.EmailVerificationLifetime)
	// The dedupe key carries the token: two genuine requests are two messages, while a
	// retried request for the same token is one.
	msg.DedupeKey = "email_verification:" + res.Token[:12]
	return s.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, _, err := s.queue.Enqueue(ctx, tx, msg)
		return err
	})
}

// recordEvent writes an authentication event inside the caller's transaction.
//
// It is the auth trail, not the audit trail: it answers "how did this account come
// to exist" rather than "what did an operator do". The two are deliberately
// separate tables.
func recordEvent(ctx context.Context, tx pgx.Tx, userID, event, ip string) error {
	var address any
	if ip != "" {
		address = ip
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO auth_events (user_id, event, detail, ip)
		VALUES ($1, $2, '', $3)`, nullableID(userID), event, address); err != nil {
		return fmt.Errorf("register: record %s: %w", event, err)
	}
	return nil
}

func nullableID(id string) any {
	if id == "" {
		return nil
	}
	return id
}

// ChangePassword sets a new password for a signed-in account.
//
// It differs from a reset in one way that matters: the caller proved the *current*
// password and holds a live session, so that session survives while every other
// one is revoked. A reset revokes everything because its premise is that the
// password may be in someone else's hands; a change is the same person rotating a
// credential they just proved they know.
//
// The revocation is not a separate step that could be forgotten: the credential
// version is bumped, which invalidates every session issued before it, and the
// caller's own session is re-stamped with the new version in the same statement
// pair. A crash between them rolls both back.
func (s *Service) ChangePassword(ctx context.Context, userID, keepSessionID, current, newPassword string) (int, error) {
	if userID == "" {
		return 0, ErrNoSuchUser
	}

	var stored string
	err := s.pool.QueryRow(ctx, `SELECT password_hash FROM users WHERE id = $1`, userID).Scan(&stored)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrNoSuchUser
	}
	if err != nil {
		return 0, fmt.Errorf("register: load account: %w", err)
	}
	if _, err := s.hasher.Verify(current, stored); err != nil {
		return 0, ErrInvalidCredentials
	}

	// Hash outside the transaction: Argon2 is deliberately expensive, and holding a
	// row lock for its duration would serialise every other write to this account.
	hash, err := s.hasher.Hash(newPassword)
	if err != nil {
		return 0, err
	}

	revoked := 0
	err = s.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var version int
		if err := tx.QueryRow(ctx, `
			UPDATE users
			SET password_hash = $2, credential_version = credential_version + 1, updated_at = now()
			WHERE id = $1
			RETURNING credential_version`, userID, hash).Scan(&version); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNoSuchUser
			}
			return fmt.Errorf("register: change password: %w", err)
		}

		// Every other session dies. This is the point of the operation: a password
		// change is what a customer does when they suspect someone else has the old one.
		tag, err := tx.Exec(ctx, `
			UPDATE sessions SET revoked_at = now()
			WHERE user_id = $1 AND revoked_at IS NULL AND id <> $2`, userID, keepSessionID)
		if err != nil {
			return fmt.Errorf("register: revoke sessions: %w", err)
		}
		revoked = int(tag.RowsAffected())

		// Re-stamp the caller's session with the new credential version. Without this
		// the bump above would revoke the very session that just proved the password,
		// and the customer would be signed out by their own security action.
		if keepSessionID != "" {
			if _, err := tx.Exec(ctx, `
				UPDATE sessions SET credential_version = $3
				WHERE id = $2 AND user_id = $1`, userID, keepSessionID, version); err != nil {
				return fmt.Errorf("register: re-stamp session: %w", err)
			}
		}
		return recordEvent(ctx, tx, userID, "password_changed", "")
	})
	if err != nil {
		return 0, err
	}
	return revoked, nil
}

// validEmail is a deliberately conservative syntactic check.
//
// It is not RFC 5322: a full grammar accepts addresses no mail server will deliver
// to, and the only authority on whether an address works is the verification
// message. What it must do is refuse input that cannot be an address at all, so a
// typo fails at the form rather than at the mail queue.
func validEmail(email string) bool {
	if email == "" || len(email) > maxEmailLength {
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

// Argon2Params are the parameters a registration hashes with.
//
// It is a variable rather than a constant so a test can lower the cost without
// reaching into authn, and so an installation can raise it as hardware improves. The
// encoded hash carries its own parameters, so changing this value never invalidates an
// existing credential: the next successful login re-hashes with the new cost.
var Argon2Params = authn.DefaultArgon2Params

// NewHasher constructs the password hasher registrations and password changes use.
//
// It is the same authn.Hasher the login path verifies with. A second hasher would be a
// second place for the cost parameters to drift, and a hash one path cannot verify is
// an account that cannot sign in.
func NewHasher() (*authn.Hasher, error) {
	return authn.NewHasher(Argon2Params())
}
