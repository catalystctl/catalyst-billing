// Package customer creates and edits customer records from the operator panel, and creates a
// service for an existing customer.
//
// The panel could list customers and services and act on them, but it could not create either:
// a phone order, a manually onboarded account, a comped customer, or a service an operator adds
// to an existing customer all needed SQL. This package is that missing write path, and the shape
// of it is the whole argument.
//
// # A customer is a login with no password
//
// A staff-created customer is three rows that must agree — `customers` (the billing account),
// `users` (the credential) and `customer_accounts` (the link that makes "this login may see that
// account" true) — written in one transaction, exactly as self-registration writes them. The
// difference is the credential: nobody has chosen a password, so the row carries the hash of 32
// bytes of server randomness that is generated, hashed and then discarded. The account therefore
// exists and cannot be signed into until the customer sets a password through the ordinary reset
// flow. That is why no response from this package ever contains a credential, and why the
// invitation mail is a reset link rather than a password.
//
// # A manually created service is not a purchase, and must not pretend to be
//
// A purchase is four rows that agree: an order, its items, a subscription and an invoice. A manual
// creation has none of them and cannot be given them without inventing a second money path, which
// is the one thing this codebase refuses to do. So:
//
//   - **It writes a real subscription**, with the period derived from the price's own anchor and
//     timezone, because that is what the renewal engine scans. A service it cannot see would be
//     unbilled for ever.
//   - **It writes no invoice and no order.** Billing a customer for a service is a different
//     action with a different document, and the panel already has exactly one path for it:
//     `POST /api/v1/staff/invoices` with a product line, which fulfilment provisions when it
//     settles. This path deliberately does not become a second one.
//   - **It can be comped**, which means no subscription at all. The absence of the row is what
//     makes "nothing will ever invoice this" true in the database rather than in a code path —
//     the same idiom `0028_plan_types.sql` uses for a one-time purchase.
//   - **It obeys stock and the per-user limit**, through the same helpers
//     (`service.CheckPurchaseAllowed`, `service.DecrementStock`) the customer checkout calls, and
//     through the same helper that prices a config selection (`service.Store.ResolveSelectionTx`).
//     A manual creation that could bypass a sold-out product is a bug an operator finds.
//   - **It names the operator and the reason**, in the audit log (append-only, in the same
//     transaction) and in `services.origin*` (the current fact). A service with no order behind it
//     is exactly what an auditor asks about.
//
// # Transactions
//
// Every method takes a pgx.Tx from the caller. The audit row, the stock decrement, the
// subscription, the service and its queued provisioning operation all commit together or not at
// all: a stock decrement that is not in the same transaction as the service it was sold for can
// succeed while the service rolls back, which sells a unit twice.
package customer

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/catalyst/billing/internal/audit"
	"github.com/catalyst/billing/internal/authn"
	"github.com/catalyst/billing/internal/billing"
	"github.com/catalyst/billing/internal/store"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Errors callers branch on. They are values rather than strings so a change to a message cannot
// change a status code.
var (
	// ErrInvalidEmail is returned for an address that is not syntactically usable.
	ErrInvalidEmail = errors.New("customer: a valid email address is required")
	// ErrInvalidName is returned for a missing or absurd display name.
	ErrInvalidName = errors.New("customer: a display name between 1 and 200 characters is required")
	// ErrEmailTaken is returned when the address already belongs to a customer or a login. It is
	// deliberately one error for both: the operator's next step ("open that record instead") is
	// the same, and the difference is in the detail they are shown, not in the control flow.
	ErrEmailTaken = errors.New("customer: this address already has a record")
	// ErrNotFound is returned for a customer id that does not exist.
	ErrNotFound = errors.New("customer: no such customer")
	// ErrTaxExemptionReason is returned when a customer is marked exempt with no reason. An
	// exemption without a reason is a number on a tax return nobody can defend.
	ErrTaxExemptionReason = errors.New("customer: a tax exemption needs a reason")
)

// Bounds. They match the register package's, because the same address and the same name are
// stored in the same columns whichever door they came through.
const (
	maxEmailLength      = 254
	maxDisplayNameLen   = 200
	unusablePasswordLen = 32
)

// Store is the customer write surface.
type Store struct {
	pool *store.Pool
	// hasher produces the unusable credential a staff-created account starts with. It may be
	// nil, in which case the shipped Argon2id parameters are used; a test injects a cheaper
	// cost, exactly as the auth tests do.
	hasher *authn.Hasher
	// Now is the clock, injectable for tests.
	Now func() time.Time
}

// New constructs a Store.
func New(pool *store.Pool) *Store {
	return &Store{pool: pool, Now: func() time.Time { return time.Now().UTC() }}
}

// WithHasher overrides the hasher used for the unusable initial credential.
func (s *Store) WithHasher(h *authn.Hasher) *Store {
	clone := *s
	clone.hasher = h
	return &clone
}

// WithClock overrides the clock.
func (s *Store) WithClock(now func() time.Time) *Store {
	if now != nil {
		clone := *s
		clone.Now = now
		return &clone
	}
	return s
}

func (s *Store) clock() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

// hasherFor returns the hasher to use.
//
// A fresh Hasher is constructed when none was injected rather than cached on the Store, because
// constructing one is a parameter validation and nothing else: it holds no pool, no salt and no
// state, so a per-call construction cannot race and cannot go stale.
func (s *Store) hasherFor() (*authn.Hasher, error) {
	if s.hasher != nil {
		return s.hasher, nil
	}
	h, err := authn.NewHasher(authn.DefaultArgon2Params())
	if err != nil {
		return nil, fmt.Errorf("customer: construct hasher: %w", err)
	}
	return h, nil
}

// Record is a customer as the write endpoints return it.
//
// It is not the staff read surface's row (that one carries invoice counts and outstanding
// balances); this is the record the create and edit endpoints just wrote, so a client can render
// the result without a second request.
type Record struct {
	CustomerID string
	UserID     string
	Email      string
	Name       string
	CreatedAt  time.Time
	// HasLogin is true when at least one login is attached. A staff-created customer always has
	// one; the field exists because the same shape describes an adopted or edited record.
	HasLogin bool
	// TaxExempt and TaxExemptionReason are the billing profile's tax fields. They are read from
	// `billing_profiles` because the tax engine reads them from there; see setTaxExemption.
	TaxExempt          bool
	TaxExemptionReason string
}

// CreateInput is one staff-created customer.
type CreateInput struct {
	Email string
	Name  string
	// Profile is the optional bill-to block. A nil profile stores no address, which is the
	// honest state for a customer an operator only knows an address for: an empty row would
	// claim someone cleared fields they never filled in.
	Profile *billing.Profile
	// TaxExempt marks the account exempt from tax, with the reason it is exempt. The reason is
	// required when the flag is set.
	TaxExempt          bool
	TaxExemptionReason string
	// ActorID is the operator, for the audit row. It comes from the session, never from a body.
	ActorID string
	// CorrelationID and IP are the request's, so the audit row ties back to it.
	CorrelationID string
	IP            string
}

// Create writes a customer, its login and the link between them, in the caller's transaction.
//
// The login has no usable password: see the package comment. Nothing in the returned Record is a
// credential, and there is no path here that returns one.
func (s *Store) Create(ctx context.Context, tx pgx.Tx, in CreateInput) (Record, error) {
	email, err := normaliseEmail(in.Email)
	if err != nil {
		return Record{}, err
	}
	name, err := normaliseName(in.Name)
	if err != nil {
		return Record{}, err
	}
	if in.TaxExempt && strings.TrimSpace(in.TaxExemptionReason) == "" {
		return Record{}, ErrTaxExemptionReason
	}

	// Refuse a duplicate before writing anything. The unique indexes are the real guard — a
	// concurrent create is caught below — but checking first means the operator gets a 409 with a
	// sentence rather than a constraint name.
	if err := s.refuseDuplicate(ctx, tx, email, ""); err != nil {
		return Record{}, err
	}

	var record Record
	err = tx.QueryRow(ctx, `
		INSERT INTO customers (email, name) VALUES ($1, $2)
		RETURNING id, email, name, created_at`, email, name).
		Scan(&record.CustomerID, &record.Email, &record.Name, &record.CreatedAt)
	if err != nil {
		if isUniqueViolation(err) {
			return Record{}, ErrEmailTaken
		}
		return Record{}, fmt.Errorf("customer: create billing account: %w", err)
	}

	hash, err := s.unusableCredential()
	if err != nil {
		return Record{}, err
	}

	// email_verified_at is stamped because an operator typed the address, which is the same
	// assertion a verification mail collects from the customer. Leaving it null would create an
	// account that can set a password through the reset flow and still not sign in when the
	// installation requires verification — a lockout nobody could act on. It does not widen who
	// can reach the account: the only way in is still a link sent to that address.
	err = tx.QueryRow(ctx, `
		INSERT INTO users (email, password_hash, display_name, email_verified_at, status)
		VALUES ($1, $2, $3, now(), 'active')
		RETURNING id`, email, hash, name).Scan(&record.UserID)
	if err != nil {
		if isUniqueViolation(err) {
			return Record{}, ErrEmailTaken
		}
		return Record{}, fmt.Errorf("customer: create login: %w", err)
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO customer_accounts (user_id, customer_id, role) VALUES ($1, $2, 'owner')`,
		record.UserID, record.CustomerID); err != nil {
		return Record{}, fmt.Errorf("customer: link billing account: %w", err)
	}
	record.HasLogin = true

	if in.Profile != nil {
		profile := *in.Profile
		profile.CustomerID = record.CustomerID
		// An empty legal name would blank the name printed on the invoice: LoadProfile treats a
		// stored empty string as the value, and only a NULL falls back to the customer's name.
		if strings.TrimSpace(profile.LegalName) == "" {
			profile.LegalName = name
		}
		// SaveProfile validates. A bad address therefore fails the whole transaction, which is the
		// same outcome as validating first: nothing is written either way.
		if err := billing.SaveProfile(ctx, tx, profile); err != nil {
			return Record{}, err
		}
	}

	if in.TaxExempt {
		if err := setTaxExemption(ctx, tx, record.CustomerID, true, in.TaxExemptionReason); err != nil {
			return Record{}, err
		}
	}
	record.TaxExempt = in.TaxExempt
	record.TaxExemptionReason = strings.TrimSpace(in.TaxExemptionReason)

	// The audit row is inside the transaction, and it names the operator: an account that can be
	// billed but has no password is exactly the kind of record whose provenance is asked about.
	if err := audit.Record(ctx, tx, audit.Event{
		Actor:         audit.Actor{ID: in.ActorID, Kind: audit.ActorUser},
		Action:        "customer.create",
		TargetType:    "customer",
		TargetID:      record.CustomerID,
		Outcome:       audit.OutcomeSuccess,
		CorrelationID: in.CorrelationID,
		IP:            in.IP,
		Changes: map[string]any{
			"email":      record.Email,
			"name":       record.Name,
			"user_id":    record.UserID,
			"tax_exempt": record.TaxExempt,
			// Recorded so the trail says why the account cannot be signed into yet. The field
			// name deliberately avoids the words the audit redactor treats as credentials: this
			// is a state, not a secret, and a redacted marker would read as "something was
			// hidden" when the point is that nothing was issued.
			"login_state": "invitation_pending",
			"profile":     in.Profile != nil,
		},
	}); err != nil {
		return Record{}, err
	}
	return record, nil
}

// UpdateInput is a partial edit of a customer. A nil field is left alone; that is what keeps a
// rename from clearing an address the operator did not touch.
type UpdateInput struct {
	Name  *string
	Email *string
	// Profile replaces the bill-to block when it is not nil.
	Profile *billing.Profile
	// TaxExempt and TaxExemptionReason are applied when either is not nil. A nil pair leaves the
	// exemption exactly as it was.
	TaxExempt          *bool
	TaxExemptionReason *string
	ActorID            string
	CorrelationID      string
	IP                 string
}

// Update edits a customer's identity, address and tax exemption — never their invoices.
//
// An issued invoice carries a frozen snapshot of the details it was issued with (the database
// trigger enforces it), so correcting an address today changes the next document and nothing that
// has already been sent to a tax authority.
func (s *Store) Update(ctx context.Context, tx pgx.Tx, customerID string, in UpdateInput) (Record, error) {
	if !validID(customerID) {
		return Record{}, ErrNotFound
	}

	var before Record
	err := tx.QueryRow(ctx, `
		SELECT c.id, c.email, c.name, c.created_at,
		       COALESCE(bp.tax_exempt, false), COALESCE(bp.tax_exemption_reason, ''),
		       EXISTS (SELECT 1 FROM customer_accounts ca WHERE ca.customer_id = c.id)
		FROM customers c
		LEFT JOIN billing_profiles bp ON bp.customer_id = c.id
		WHERE c.id = $1
		FOR UPDATE OF c`, customerID).
		Scan(&before.CustomerID, &before.Email, &before.Name, &before.CreatedAt,
			&before.TaxExempt, &before.TaxExemptionReason, &before.HasLogin)
	if errors.Is(err, pgx.ErrNoRows) {
		return Record{}, ErrNotFound
	}
	if err != nil {
		return Record{}, fmt.Errorf("customer: lock customer: %w", err)
	}
	after := before

	if in.Name != nil {
		name, err := normaliseName(*in.Name)
		if err != nil {
			return Record{}, err
		}
		if _, err := tx.Exec(ctx, `UPDATE customers SET name = $2 WHERE id = $1`, customerID, name); err != nil {
			return Record{}, fmt.Errorf("customer: rename: %w", err)
		}
		after.Name = name
	}

	if in.Email != nil {
		email, err := normaliseEmail(*in.Email)
		if err != nil {
			return Record{}, err
		}
		if email != strings.ToLower(before.Email) {
			if err := s.refuseDuplicate(ctx, tx, email, customerID); err != nil {
				return Record{}, err
			}
			if _, err := tx.Exec(ctx, `UPDATE customers SET email = $2 WHERE id = $1`, customerID, email); err != nil {
				if isUniqueViolation(err) {
					return Record{}, ErrEmailTaken
				}
				return Record{}, fmt.Errorf("customer: change address: %w", err)
			}
			// The owner's login moves with the account. An account whose address says one thing and
			// whose login says another is two identities that only look like one, and the customer
			// would never be able to reset their password at the address the operator just set.
			//
			// Only the `owner` is moved, not every login attached to the billing account: a
			// `technical` contact is a different person, and rewriting their address because the
			// billing address changed would hand them a login they never asked for.
			//
			// The verification stamp is kept, for the same reason it is stamped at creation: the
			// operator asserted the address, and clearing it here would lock out a customer who
			// cannot reach the signed-in "resend verification" screen.
			if _, err := tx.Exec(ctx, `
				UPDATE users u SET email = $2, updated_at = now()
				FROM customer_accounts ca
				WHERE ca.user_id = u.id AND ca.customer_id = $1 AND ca.role = 'owner'`,
				customerID, email); err != nil {
				if isUniqueViolation(err) {
					return Record{}, ErrEmailTaken
				}
				return Record{}, fmt.Errorf("customer: change login address: %w", err)
			}
			after.Email = email
		}
	}

	if in.Profile != nil {
		profile := *in.Profile
		profile.CustomerID = customerID
		if strings.TrimSpace(profile.LegalName) == "" {
			profile.LegalName = after.Name
		}
		if err := billing.SaveProfile(ctx, tx, profile); err != nil {
			return Record{}, err
		}
	}

	if in.TaxExempt != nil || in.TaxExemptionReason != nil {
		exempt := before.TaxExempt
		if in.TaxExempt != nil {
			exempt = *in.TaxExempt
		}
		reason := before.TaxExemptionReason
		if in.TaxExemptionReason != nil {
			reason = strings.TrimSpace(*in.TaxExemptionReason)
		}
		if exempt && reason == "" {
			return Record{}, ErrTaxExemptionReason
		}
		if !exempt {
			// Clearing the flag clears the reason with it. A reason left behind on a taxable
			// account is a sentence an auditor reads and cannot reconcile with the invoices.
			reason = ""
		}
		if err := setTaxExemption(ctx, tx, customerID, exempt, reason); err != nil {
			return Record{}, err
		}
		after.TaxExempt, after.TaxExemptionReason = exempt, reason
	}

	if err := audit.Record(ctx, tx, audit.Event{
		Actor:         audit.Actor{ID: in.ActorID, Kind: audit.ActorUser},
		Action:        "customer.update",
		TargetType:    "customer",
		TargetID:      customerID,
		Outcome:       audit.OutcomeSuccess,
		CorrelationID: in.CorrelationID,
		IP:            in.IP,
		Changes: map[string]any{
			"before": map[string]any{
				"email": before.Email, "name": before.Name,
				"tax_exempt": before.TaxExempt,
			},
			"after": map[string]any{
				"email": after.Email, "name": after.Name, "tax_exempt": after.TaxExempt,
				"tax_exemption_reason": after.TaxExemptionReason,
			},
		},
	}); err != nil {
		return Record{}, err
	}
	return after, nil
}

// Load reads one customer record.
func (s *Store) Load(ctx context.Context, q interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}, customerID string) (Record, error) {
	if !validID(customerID) {
		return Record{}, ErrNotFound
	}
	var record Record
	err := q.QueryRow(ctx, `
		SELECT c.id, c.email, c.name, c.created_at,
		       COALESCE(bp.tax_exempt, false), COALESCE(bp.tax_exemption_reason, ''),
		       EXISTS (SELECT 1 FROM customer_accounts ca WHERE ca.customer_id = c.id)
		FROM customers c
		LEFT JOIN billing_profiles bp ON bp.customer_id = c.id
		WHERE c.id = $1`, customerID).
		Scan(&record.CustomerID, &record.Email, &record.Name, &record.CreatedAt,
			&record.TaxExempt, &record.TaxExemptionReason, &record.HasLogin)
	if errors.Is(err, pgx.ErrNoRows) {
		return Record{}, ErrNotFound
	}
	if err != nil {
		return Record{}, fmt.Errorf("customer: load customer: %w", err)
	}
	return record, nil
}

// setTaxExemption writes the two tax fields on the billing profile.
//
// They are written directly rather than through billing.SaveProfile because billing.Profile does
// not carry them: the tax engine reads `billing_profiles.tax_exempt` and
// `billing_profiles.tax_exemption_reason` itself (see httpapi.taxPricerFor), and the profile type
// is the bill-to block. A profile row is created when there is none, with the customer's name as
// the legal name, so that a customer exempted before they have an address still prints a name on
// their first document.
func setTaxExemption(ctx context.Context, tx pgx.Tx, customerID string, exempt bool, reason string) error {
	if _, err := tx.Exec(ctx, `
		INSERT INTO billing_profiles (customer_id, legal_name, tax_exempt, tax_exemption_reason)
		SELECT c.id, c.name, $2, $3 FROM customers c WHERE c.id = $1
		ON CONFLICT (customer_id) DO UPDATE
			SET tax_exempt = EXCLUDED.tax_exempt,
			    tax_exemption_reason = EXCLUDED.tax_exemption_reason,
			    updated_at = now()`, customerID, exempt, reason); err != nil {
		return fmt.Errorf("customer: set tax exemption: %w", err)
	}
	return nil
}

// refuseDuplicate reports ErrEmailTaken when the address already belongs to a customer or a login.
// `except` is the customer being edited, which may keep its own address.
func (s *Store) refuseDuplicate(ctx context.Context, tx pgx.Tx, email, except string) error {
	var taken bool
	// `except` is rendered as NULL rather than "" when the caller is creating rather than editing:
	// an empty string is not a uuid and PostgreSQL refuses to compare one.
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM customers WHERE lower(email) = $1 AND id <> $2::uuid)
		    OR EXISTS (SELECT 1 FROM users WHERE lower(email) = $1)`,
		email, nullableID(except)).Scan(&taken); err != nil {
		return fmt.Errorf("customer: check address: %w", err)
	}
	if taken {
		return ErrEmailTaken
	}
	return nil
}

// unusableCredential hashes random bytes nobody keeps.
//
// The stored value is a real Argon2id hash, not a sentinel: the login path verifies a submitted
// password against it, and a value it cannot parse would turn "wrong password" into a server
// error. Because the plaintext is 32 bytes of /dev/urandom that are never returned, stored or
// logged, no password verifies against it, and the only way in is the reset link the customer is
// sent.
func (s *Store) unusableCredential() (string, error) {
	hasher, err := s.hasherFor()
	if err != nil {
		return "", err
	}
	var raw [unusablePasswordLen]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("customer: generate credential: %w", err)
	}
	hash, err := hasher.Hash(hex.EncodeToString(raw[:]))
	if err != nil {
		return "", fmt.Errorf("customer: hash credential: %w", err)
	}
	return hash, nil
}

// normaliseEmail lower-cases and trims an address and checks that it is one.
//
// The check is deliberately modest — one "@", something either side, no spaces, bounded length —
// because a stricter grammar refuses addresses that exist and a looser one stores addresses that
// cannot receive the invitation.
func normaliseEmail(raw string) (string, error) {
	email := strings.ToLower(strings.TrimSpace(raw))
	if email == "" || len(email) > maxEmailLength {
		return "", ErrInvalidEmail
	}
	at := strings.IndexByte(email, '@')
	if at <= 0 || at != strings.LastIndexByte(email, '@') || at == len(email)-1 {
		return "", ErrInvalidEmail
	}
	if strings.ContainsAny(email, " \t\r\n,;<>\"") {
		return "", ErrInvalidEmail
	}
	if !strings.Contains(email[at+1:], ".") {
		return "", ErrInvalidEmail
	}
	return email, nil
}

// normaliseName trims and bounds a display name.
func normaliseName(raw string) (string, error) {
	name := strings.TrimSpace(raw)
	if name == "" || len([]rune(name)) > maxDisplayNameLen {
		return "", ErrInvalidName
	}
	return name, nil
}

// isUniqueViolation reports whether err is PostgreSQL's duplicate-key error.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// validID is a UUID shape check, so a malformed id is a 404 that never reaches the database and
// "not a uuid" cannot be told apart from "does not exist".
func validID(id string) bool {
	if len(id) != 36 {
		return false
	}
	for i, r := range id {
		switch i {
		case 8, 13, 18, 23:
			if r != '-' {
				return false
			}
		default:
			if (r < '0' || r > '9') && (r < 'a' || r > 'f') && (r < 'A' || r > 'F') {
				return false
			}
		}
	}
	return true
}
