package payments

// Billing agreements (PAY-10, PAY-11).
//
// A billing agreement is the customer's permission to charge them again without being
// present. This file implements the three things that permission needs:
//
//  1. **A saved method.** A row holding the provider's reusable token plus the display
//     metadata a human needs to recognise it (brand, last four, expiry). The token is the
//     provider's, not ours: it is stored, used server-side, and never leaves this process.
//     There is no field on the wire type and no JSON tag that could carry it, because the
//     one way to be sure a secret never reaches a browser is for it to have no path there.
//
//  2. **A default.** "The method a renewal charges" is a single fact per customer, enforced
//     by a partial unique index rather than by this code. Two rows claiming it would make an
//     automatic charge ambiguous, and ambiguity in an automatic charge is how the wrong card
//     gets billed.
//
//  3. **The off-session charge.** A renewal charges the default method with nobody present.
//     The charge itself is an adapter call the v1 protocol does not have yet, so it sits
//     behind OffSessionCharger: an implementation that cannot charge refuses with
//     ErrOffSessionUnsupported, and the invoice stays open. Nothing here ever reports a
//     renewal as paid on the strength of a request it could not make.
//
// # The settlement path is not duplicated
//
// A successful off-session charge is applied by Applier.ApplySettledPayment — the same
// interface the webhook processor calls, implemented by internal/fulfilment, which calls
// billing.ApplyPayment. So the payment row, its allocation, the ledger posting, the invoice
// transition, the proforma conversion, the affiliate commission and the provisioning job are
// that function's, not a second copy of them. An off-session charge is therefore
// indistinguishable, downstream, from a card payment that arrived through a browser: which
// is exactly what makes reconciliation possible.
//
// # Failure is a state, not an exception
//
// A declined charge records the attempt, records the failure on the method (so the customer
// can see which card was refused) and leaves the invoice open. It does not void the invoice,
// does not mark it paid, and does not suspend anything: the invoice is now simply unpaid and
// past due, which is the state dunning already knows how to escalate. The caller gets a
// *DeclinedError so it can log or notify, but the invoice's own state is the record.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/catalyst/billing/internal/audit"
	"github.com/catalyst/billing/internal/money"
	"github.com/catalyst/billing/internal/notify"
	"github.com/catalyst/billing/internal/settings"
	"github.com/catalyst/billing/internal/store"
	"github.com/jackc/pgx/v5"
)

var (
	// ErrMethodNotFound is returned for an unknown method, or one that belongs to another
	// customer. The two are the same error on purpose: a scoped query cannot tell them
	// apart, so the HTTP layer answers 404 for both and an id cannot be probed.
	ErrMethodNotFound = errors.New("payments: no such saved payment method")
	// ErrInvalidMethod is returned for a method that cannot be stored: an empty token, an
	// expiry with only one half, a brand that is too long.
	ErrInvalidMethod = errors.New("payments: invalid payment method")
	// ErrOffSessionDisabled is returned when gateway.off_session_enabled is off. It is the
	// one setting that moves money with no human in the loop, so it is off by default and
	// every charge checks it rather than trusting the caller to have checked.
	ErrOffSessionDisabled = errors.New("payments: charging a saved method off-session is disabled")
	// ErrOffSessionUnsupported is returned when the configured adapter cannot charge a
	// saved method. The v1 extension protocol has no charge method, so this is the honest
	// answer for most gateways in this build — and the alternative (pretending the renewal
	// was paid) would silently give away service.
	ErrOffSessionUnsupported = errors.New("payments: the gateway cannot charge a saved method off-session")
	// ErrNotConfigured is returned when the off-session path has no applier or no provider
	// account. Both are needed for the settlement to be the same one every other payment
	// uses, so neither is guessed.
	ErrNotConfigured = errors.New("payments: off-session charging is not configured")
	// ErrNotChargeable is returned when the invoice is not open, or nothing is owed.
	ErrNotChargeable = errors.New("payments: the invoice cannot be charged")
	// ErrNoDefaultMethod is returned when a charge is attempted for a customer who has no
	// default method for the gateway. It is reported as a skip by ChargeDue, not a failure.
	ErrNoDefaultMethod = errors.New("payments: the customer has no default payment method")
	// ErrDeclined is matched by *DeclinedError: the provider refused the charge.
	ErrDeclined = errors.New("payments: the saved payment method was declined")
)

// Method is one saved payment method as the rest of the program sees it.
//
// The provider token is deliberately absent from every exported field: it is kept in the
// unexported token field, reachable only through Token(), and that accessor is used by the
// charge path and by nothing else. `encoding/json` cannot see an unexported field, so a
// future handler that marshals this struct by mistake still cannot leak the token — and
// internal/httpapi builds its response by hand anyway, which the token test asserts.
type Method struct {
	ID         string
	CustomerID string
	// Provider is the gateway's own name, e.g. "stripe". A method saved for one gateway
	// cannot be charged by another, so it is part of the default lookup.
	Provider string
	Brand    string
	Last4    string
	Label    string
	// ExpiryMonth and ExpiryYear are both set or both nil.
	ExpiryMonth *int
	ExpiryYear  *int
	IsDefault   bool
	CreatedAt   time.Time
	UpdatedAt   time.Time
	LastUsedAt  *time.Time
	// LastFailureAt and LastFailureCode describe the most recent declined off-session charge,
	// so the customer's screen can say "this card was refused on the 3rd" instead of nothing.
	LastFailureAt   *time.Time
	LastFailureCode string

	token string
}

// Token returns the provider's reusable reference.
//
// It exists for the adapter call and for nothing else. It is not part of the JSON shape of
// Method (which has no JSON shape at all) and must never be written to a response body, a log
// line or an audit row. internal/httpapi/payment_methods.go is the only caller of this
// package's HTTP surface, and it never touches this method.
func (m Method) Token() string { return m.token }

// SaveMethodRequest is one "save this method" instruction.
type SaveMethodRequest struct {
	CustomerID string
	// Provider names the gateway that issued the token.
	Provider string
	// Token is the provider's reusable reference. It is required and is never echoed.
	Token string
	Brand string
	Last4 string
	Label string
	// ExpiryMonth and ExpiryYear are optional, but both or neither.
	ExpiryMonth *int
	ExpiryYear  *int
	// MakeDefault asks for this method to become the one recurring charges use. The first
	// method a customer saves becomes the default regardless, because a customer with one
	// card and no default is a renewal that silently stops.
	MakeDefault bool
	// ProviderAgreementID and ProviderCustomerID are the provider's mandate references,
	// present when the method was created through the provider's hosted agreement flow.
	// They are identifiers, never credentials; the token remains in Token.
	ProviderAgreementID string
	ProviderCustomerID  string
	// ConsentAt, ConsentIP, ConsentUserAgent and TermsVersion are the evidence written
	// beside the mandate in the same transaction. A charge without this record is refused.
	ConsentAt        *time.Time
	ConsentIP        string
	ConsentUserAgent string
	TermsVersion     string
}

// Agreements stores saved methods and charges them off-session.
type Agreements struct {
	pool *store.Pool
	// charger performs the provider call. It is nil when the configured gateway cannot
	// charge a saved method, which is a refusal, not a crash.
	charger OffSessionCharger
	// applier settles a confirmed charge. It is payments.Applier, implemented by
	// internal/fulfilment, so the settlement path is the webhook path.
	applier Applier
	// account is the provider account the settlement is recorded against. It must match the
	// account the webhook path uses: a payment recorded under a different account would not
	// be recognised when the provider's own webhook for it arrives, and the invoice could be
	// credited twice.
	account string
	// Now is injectable so a test can pin the clock.
	Now func() time.Time
	// auditor, when set, attributes the audit rows to the request that caused them.
	auditor audit.Auditor
	// notifier, when set, tells the customer a charge was declined. It is nil in the paths that
	// have no outbox (a test, a CLI), and the decline record is written either way: the message is
	// a courtesy, the state change is the fact.
	notifier *notify.Queue
}

// NewAgreements constructs an Agreements store.
func NewAgreements(pool *store.Pool) *Agreements {
	return &Agreements{pool: pool, Now: func() time.Time { return time.Now().UTC() }}
}

// WithNotifier attaches the outbox that carries the payment-failed notice.
//
// It is optional because the attempt record and the method's failure state are the durable facts;
// a charge path that only works when a mail queue is wired would silently stop recording declines
// in a deployment without one.
func (a *Agreements) WithNotifier(q *notify.Queue) *Agreements {
	a.notifier = q
	return a
}

// WithCharger attaches the adapter that can charge a saved method.
func (a *Agreements) WithCharger(c OffSessionCharger) *Agreements {
	a.charger = c
	return a
}

// WithApplier attaches the settlement path. It must be the same Applier the webhook
// processor uses (internal/fulfilment's), or the two paths would post different ledgers.
func (a *Agreements) WithApplier(app Applier) *Agreements {
	a.applier = app
	return a
}

// WithAccount sets the provider account the settlement is recorded against.
func (a *Agreements) WithAccount(account string) *Agreements {
	a.account = account
	return a
}

// WithAuditor returns a copy of the service that writes audit rows attributed to the caller.
//
// A copy, not a mutation, for the same reason internal/dunning copies: the service is shared
// by every request, and an actor stored on it would be whichever request set it last.
func (a *Agreements) WithAuditor(au audit.Auditor) *Agreements {
	clone := *a
	clone.auditor = au
	return &clone
}

// CanChargeOffSession reports whether this installation is configured to charge a saved
// method: an adapter that can, an applier, an account, and the operator's setting. It is what
// the portal uses to decide whether to promise "we charge this automatically" or to say
// plainly that it cannot.
func (a *Agreements) CanChargeOffSession(ctx context.Context) (bool, string) {
	switch {
	case a.charger == nil:
		return false, "the configured payment gateway cannot charge a saved method"
	case a.applier == nil || a.account == "":
		return false, "off-session charging is not wired on this installation"
	}
	enabled, err := a.offSessionEnabled(ctx)
	if err != nil {
		return false, "the off-session setting could not be read"
	}
	if !enabled {
		return false, "charging a saved method automatically is turned off"
	}
	return true, ""
}

// offSessionEnabled reads gateway.off_session_enabled.
func (a *Agreements) offSessionEnabled(ctx context.Context) (bool, error) {
	enabled, err := settings.New(a.pool).Bool(ctx, "gateway", "off_session_enabled")
	if err != nil {
		return false, fmt.Errorf("payments: read off-session setting: %w", err)
	}
	return enabled, nil
}

// OffSessionEnabled reports the operator's switch without requiring a charger. It is what
// the portal asks before promising an automatic charge on an installation whose adapter has
// not been attached yet.
func (a *Agreements) OffSessionEnabled(ctx context.Context) (bool, error) {
	return a.offSessionEnabled(ctx)
}

// ------------------------------------------------------------------ storage --

// methodColumns is the select list, in the order scanMethod expects.
//
// One list, used by every read, so a new column cannot be added to the list query and
// forgotten in the detail query: that class of bug is a nil pointer in a template.
const methodColumns = `id, customer_id, provider, provider_token, brand, last4, label,
	expiry_month, expiry_year, is_default, created_at, updated_at, last_used_at,
	last_failure_at, COALESCE(last_failure_code, '')`

// scanMethod reads one row.
func scanMethod(row pgx.Row) (Method, error) {
	var (
		m     Method
		month *int32
		year  *int32
	)
	if err := row.Scan(&m.ID, &m.CustomerID, &m.Provider, &m.token, &m.Brand, &m.Last4,
		&m.Label, &month, &year, &m.IsDefault, &m.CreatedAt, &m.UpdatedAt,
		&m.LastUsedAt, &m.LastFailureAt, &m.LastFailureCode); err != nil {
		return Method{}, err
	}
	if month != nil {
		v := int(*month)
		m.ExpiryMonth = &v
	}
	if year != nil {
		v := int(*year)
		m.ExpiryYear = &v
	}
	return m, nil
}

// normalize validates and canonicalises a save request.
func (r SaveMethodRequest) normalize() (SaveMethodRequest, error) {
	r.Provider = strings.ToLower(strings.TrimSpace(r.Provider))
	r.Token = strings.TrimSpace(r.Token)
	r.Brand = strings.TrimSpace(r.Brand)
	r.Last4 = strings.TrimSpace(r.Last4)
	r.Label = strings.TrimSpace(r.Label)
	r.ProviderAgreementID = strings.TrimSpace(r.ProviderAgreementID)
	r.ProviderCustomerID = strings.TrimSpace(r.ProviderCustomerID)
	r.ConsentIP = strings.TrimSpace(r.ConsentIP)
	r.ConsentUserAgent = strings.TrimSpace(r.ConsentUserAgent)
	r.TermsVersion = strings.TrimSpace(r.TermsVersion)

	switch {
	case r.CustomerID == "":
		return r, fmt.Errorf("%w: the method needs a customer", ErrInvalidMethod)
	case r.Provider == "":
		return r, fmt.Errorf("%w: the method needs a provider", ErrInvalidMethod)
	case len(r.Provider) > 64:
		return r, fmt.Errorf("%w: the provider name is too long", ErrInvalidMethod)
	case r.Token == "":
		return r, fmt.Errorf("%w: the provider token is required", ErrInvalidMethod)
	case len(r.Token) > 512:
		return r, fmt.Errorf("%w: the provider token is too long", ErrInvalidMethod)
	case len(r.Brand) > 40:
		return r, fmt.Errorf("%w: the brand is too long", ErrInvalidMethod)
	case len(r.Label) > 80:
		return r, fmt.Errorf("%w: the label is too long", ErrInvalidMethod)
	case len(r.ProviderAgreementID) > 512:
		return r, fmt.Errorf("%w: the provider agreement reference is too long", ErrInvalidMethod)
	case len(r.ProviderCustomerID) > 512:
		return r, fmt.Errorf("%w: the provider customer reference is too long", ErrInvalidMethod)
	case len(r.ConsentIP) > 64:
		return r, fmt.Errorf("%w: the consent address is too long", ErrInvalidMethod)
	case len(r.ConsentUserAgent) > 512:
		return r, fmt.Errorf("%w: the consent user agent is too long", ErrInvalidMethod)
	case len(r.TermsVersion) > 80:
		return r, fmt.Errorf("%w: the terms version is too long", ErrInvalidMethod)
	case r.Last4 != "" && (len(r.Last4) != 4 || !digits(r.Last4)):
		return r, fmt.Errorf("%w: the last four must be four digits", ErrInvalidMethod)
	case (r.ExpiryMonth == nil) != (r.ExpiryYear == nil):
		return r, fmt.Errorf("%w: an expiry needs both a month and a year", ErrInvalidMethod)
	}
	for _, ch := range r.Provider {
		if !(ch >= 'a' && ch <= 'z') && !(ch >= '0' && ch <= '9') && ch != '.' && ch != '_' && ch != '-' {
			return r, fmt.Errorf("%w: the provider name has an unexpected character", ErrInvalidMethod)
		}
	}
	if r.ExpiryMonth != nil {
		if *r.ExpiryMonth < 1 || *r.ExpiryMonth > 12 {
			return r, fmt.Errorf("%w: the expiry month is not 1-12", ErrInvalidMethod)
		}
		if *r.ExpiryYear < 1970 || *r.ExpiryYear > 9999 {
			return r, fmt.Errorf("%w: the expiry year is out of range", ErrInvalidMethod)
		}
	}
	return r, nil
}

func digits(s string) bool {
	for _, ch := range s {
		if ch < '0' || ch > '9' {
			return false
		}
	}
	return true
}

// Save stores a method, or updates the row that already holds this token.
//
// Saving is not a charge. It records what the provider told us about a card the customer
// already entered on the provider's page; no invoice is read, no amount is computed and no
// payment row is written. The test for that is the absence of both.
func (a *Agreements) Save(ctx context.Context, req SaveMethodRequest) (Method, error) {
	req, err := req.normalize()
	if err != nil {
		return Method{}, err
	}

	var out Method
	err = a.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		// Serialise saves for one customer on the customer row. Locking the methods
		// themselves would lock nothing when the customer has none yet, so two first
		// saves could both believe there is no default and race the partial unique index.
		if err := lockCustomer(ctx, tx, req.CustomerID); err != nil {
			return err
		}

		var hasDefault bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS (SELECT 1 FROM payment_methods WHERE customer_id = $1 AND is_default)`,
			req.CustomerID).Scan(&hasDefault); err != nil {
			return fmt.Errorf("payments: read existing default: %w", err)
		}
		wantDefault := req.MakeDefault || !hasDefault

		if wantDefault {
			// Clear first: the unique index permits one default, so setting the new one before
			// clearing the old one is a constraint violation rather than a swap.
			if _, err := tx.Exec(ctx, `
				UPDATE payment_methods SET is_default = false, updated_at = now()
				WHERE customer_id = $1 AND is_default`, req.CustomerID); err != nil {
				return fmt.Errorf("payments: clear previous default: %w", err)
			}
		}

		row := tx.QueryRow(ctx, `
			INSERT INTO payment_methods
				(customer_id, provider, provider_token, brand, last4, label,
				 expiry_month, expiry_year, is_default)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
			ON CONFLICT (customer_id, provider, provider_token) DO UPDATE
			SET brand = EXCLUDED.brand,
			    last4 = EXCLUDED.last4,
			    expiry_month = EXCLUDED.expiry_month,
			    expiry_year = EXCLUDED.expiry_year,
			    -- An empty label does not erase one the customer typed: saving the same card
			    -- again from a checkout page must not rename it back to nothing.
			    label = CASE WHEN EXCLUDED.label <> '' THEN EXCLUDED.label
			                 ELSE payment_methods.label END,
			    is_default = payment_methods.is_default OR EXCLUDED.is_default,
			    updated_at = now()
			RETURNING `+methodColumns,
			req.CustomerID, req.Provider, req.Token, req.Brand, req.Last4, req.Label,
			req.ExpiryMonth, req.ExpiryYear, wantDefault)
		m, err := scanMethod(row)
		if err != nil {
			return fmt.Errorf("payments: save payment method: %w", err)
		}
		out = m

		// The mandate is written in the same transaction as the method: a saved token
		// without a record of when and how it was authorised is a charge nobody can
		// explain later. recordAgreement upserts the pending hosted row when there is one.
		if err := a.recordAgreement(ctx, tx, m, req); err != nil {
			return err
		}

		// The audit row names the display metadata and the provider, never the token: an
		// append-only table is the last place a credential should be readable.
		return a.record(ctx, tx, "payment_method.saved", m.ID, map[string]any{
			"provider":   m.Provider,
			"brand":      m.Brand,
			"last4":      m.Last4,
			"is_default": m.IsDefault,
		})
	})
	if err != nil {
		return Method{}, err
	}
	return out, nil
}

// List returns a customer's methods, newest first, default first.
func (a *Agreements) List(ctx context.Context, customerID string) ([]Method, error) {
	rows, err := a.pool.Query(ctx, `
		SELECT `+methodColumns+`
		FROM payment_methods
		WHERE customer_id = $1
		ORDER BY is_default DESC, created_at DESC`, customerID)
	if err != nil {
		return nil, fmt.Errorf("payments: list payment methods: %w", err)
	}
	defer rows.Close()

	out := make([]Method, 0, 4)
	for rows.Next() {
		m, err := scanMethod(rows)
		if err != nil {
			return nil, fmt.Errorf("payments: scan payment method: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("payments: read payment methods: %w", err)
	}
	return out, nil
}

// Get returns one method, scoped to the customer in SQL.
//
// The customer id is part of the WHERE clause rather than a check after the read, which is
// what makes another customer's id indistinguishable from an id that does not exist.
func (a *Agreements) Get(ctx context.Context, customerID, id string) (Method, error) {
	m, err := scanMethod(a.pool.QueryRow(ctx, `
		SELECT `+methodColumns+` FROM payment_methods WHERE id = $1 AND customer_id = $2`,
		id, customerID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Method{}, fmt.Errorf("%w: %s", ErrMethodNotFound, id)
	}
	if err != nil {
		return Method{}, fmt.Errorf("payments: load payment method: %w", err)
	}
	return m, nil
}

// SetDefault makes one method the default for recurring charges.
//
// Two statements rather than one, because the partial unique index is checked per row and
// a single UPDATE would depend on the order the planner happened to choose.
func (a *Agreements) SetDefault(ctx context.Context, customerID, id string) (Method, error) {
	var out Method
	err := a.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if err := lockCustomer(ctx, tx, customerID); err != nil {
			return err
		}
		var exists bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS (SELECT 1 FROM payment_methods WHERE id = $1 AND customer_id = $2)`,
			id, customerID).Scan(&exists); err != nil {
			return fmt.Errorf("payments: look up payment method: %w", err)
		}
		if !exists {
			return fmt.Errorf("%w: %s", ErrMethodNotFound, id)
		}

		if _, err := tx.Exec(ctx, `
			UPDATE payment_methods SET is_default = false, updated_at = now()
			WHERE customer_id = $1 AND is_default AND id <> $2`, customerID, id); err != nil {
			return fmt.Errorf("payments: clear previous default: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE payment_methods SET is_default = true, updated_at = now()
			WHERE id = $1 AND customer_id = $2`, id, customerID); err != nil {
			return fmt.Errorf("payments: set default: %w", err)
		}

		m, err := scanMethod(tx.QueryRow(ctx, `
			SELECT `+methodColumns+` FROM payment_methods WHERE id = $1 AND customer_id = $2`,
			id, customerID))
		if err != nil {
			return fmt.Errorf("payments: load payment method: %w", err)
		}
		out = m
		return a.record(ctx, tx, "payment_method.default_set", m.ID, map[string]any{
			"provider": m.Provider,
			"last4":    m.Last4,
		})
	})
	if err != nil {
		return Method{}, err
	}
	return out, nil
}

// Remove deletes a method.
//
// Removing the default deliberately does not promote another card. Promotion would silently
// change which card an automatic charge uses, and "the panel decided to charge your other
// card" is a complaint, not a feature. The customer picks a new default; until then a
// renewal has no method and stays unpaid, which is visible.
func (a *Agreements) Remove(ctx context.Context, customerID, id string) error {
	return a.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			`DELETE FROM payment_methods WHERE id = $1 AND customer_id = $2`, id, customerID)
		if err != nil {
			return fmt.Errorf("payments: remove payment method: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return fmt.Errorf("%w: %s", ErrMethodNotFound, id)
		}
		return a.record(ctx, tx, "payment_method.removed", id, nil)
	})
}

// Default returns the customer's default method for one provider.
func (a *Agreements) Default(ctx context.Context, customerID, provider string) (Method, error) {
	m, err := scanMethod(a.pool.QueryRow(ctx, `
		SELECT `+methodColumns+`
		FROM payment_methods
		WHERE customer_id = $1 AND provider = $2 AND is_default`, customerID, provider))
	if errors.Is(err, pgx.ErrNoRows) {
		return Method{}, fmt.Errorf("%w: customer %s, provider %s", ErrNoDefaultMethod, customerID, provider)
	}
	if err != nil {
		return Method{}, fmt.Errorf("payments: load default payment method: %w", err)
	}
	return m, nil
}

// lockCustomer serialises the operations that must agree about "the default".
//
// Two statements — clear the old default, set the new one — cannot be made atomic by a single
// UPDATE because the partial unique index is checked row by row, so the operations that move
// the default take a lock on the customer first. The index remains the backstop; this is what
// keeps the ordinary path from ever reaching it. Locking the methods themselves would lock
// nothing when the customer has none yet, which is the case that used to race.
func lockCustomer(ctx context.Context, tx pgx.Tx, customerID string) error {
	var id string
	err := tx.QueryRow(ctx, `SELECT id FROM customers WHERE id = $1 FOR UPDATE`, customerID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: no customer %s", ErrMethodNotFound, customerID)
	}
	if err != nil {
		return fmt.Errorf("payments: lock customer: %w", err)
	}
	return nil
}

// record writes a payment-method audit row inside the caller's transaction.
//
// Without an auditor the row is still written, attributed to the system: a change to a
// stored payment credential must not be able to happen without a trace just because no
// request caused it.
func (a *Agreements) record(ctx context.Context, tx pgx.Tx, action, targetID string, changes map[string]any) error {
	return a.recordFor(ctx, tx, action, "payment_method", targetID, changes)
}

func (a *Agreements) recordFor(ctx context.Context, tx pgx.Tx, action, targetType, targetID string, changes map[string]any) error {
	if a.auditor != nil {
		return a.auditor.RecordAction(ctx, tx, action, targetType, targetID, changes)
	}
	return audit.Record(ctx, tx, audit.Event{
		Actor:      audit.Actor{Kind: audit.ActorSystem},
		Action:     action,
		TargetType: targetType,
		TargetID:   targetID,
		Outcome:    audit.OutcomeSuccess,
		Changes:    changes,
	})
}

// ------------------------------------------------------------ off-session charge --

// OffSessionCharge is one request to charge a saved method with no customer present.
type OffSessionCharge struct {
	InvoiceID  string
	CustomerID string
	MethodID   string
	Provider   string
	// ProviderToken is the saved token. It travels to the adapter and nowhere else.
	ProviderToken string
	// AgreementID is the mandate this charge relies on, and ProviderCustomerID is the
	// provider's customer reference the mandate was issued to. A charge with no agreement
	// is refused before this struct is built.
	AgreementID        string
	ProviderCustomerID string
	// AmountMinor and Currency are what the invoice still owes, in integer minor units.
	AmountMinor int64
	Currency    string
	// OperationKey is the idempotency token. Replaying it must return the result of the first
	// charge rather than performing a second one; the core also records it in
	// payment_attempts, so a retry after a crash is recognised even if the adapter forgot.
	OperationKey string
	// Description is shown to the customer on their statement where the provider supports it.
	Description string
}

// OffSessionResult is the adapter's answer.
type OffSessionResult struct {
	// ProviderPaymentID is the provider's identity for the charge. It is required for a
	// settled charge: it is what makes the payment row unique and the settlement idempotent.
	ProviderPaymentID string
	// Status is the provider's state: "succeeded", "pending", "requires_action", "failed" or
	// "canceled". A pending charge is recorded and left for the webhook to settle.
	Status string
	// AmountMinor is what was actually charged, when the adapter reports it. A settled amount
	// that differs from what the invoice owes is refused rather than absorbed.
	AmountMinor int64
	// FeeMinor is the provider's own transaction fee, when it reports one. It is recorded on
	// the payment row for reconciliation and never posted to the ledger.
	FeeMinor int64
	// FailureCode is the provider's decline code, e.g. "card_declined".
	FailureCode string
	// FailureMessage is the provider's human explanation. It is shown to the customer.
	FailureMessage string
}

// OffSessionCharger is the provider call an off-session charge needs.
//
// It is an interface in this package rather than a method on extproto.Client because the v1
// extension protocol does not have a charge method: adding one is a protocol version bump
// with its own conformance fixture, and this package must not invent a half-version of it.
// Until then the only implementations are a test fake and — once the protocol lands — an
// adapter over it. A gateway that cannot charge simply does not implement this, and
// CanChargeOffSession says so.
type OffSessionCharger interface {
	ChargeOffSession(ctx context.Context, req OffSessionCharge) (OffSessionResult, error)
	// Name is the gateway's provider name, used to scope the saved method and to record the
	// payment under the same provider the webhook path uses.
	Name() string
}

// DeclinedError reports a charge the provider refused.
//
// It wraps ErrDeclined so a caller can branch with errors.Is, and it carries the provider's
// own code and message so the customer is told what the provider said rather than a generic
// failure.
type DeclinedError struct {
	InvoiceID string
	MethodID  string
	Code      string
	Message   string
}

// Error implements error.
func (e *DeclinedError) Error() string {
	if e.Code == "" {
		return fmt.Sprintf("payments: the saved method was declined: %s", e.Message)
	}
	return fmt.Sprintf("payments: the saved method was declined (%s): %s", e.Code, e.Message)
}

// Unwrap makes errors.Is(err, ErrDeclined) true.
func (e *DeclinedError) Unwrap() error { return ErrDeclined }

// ChargeStatus classifies what a charge attempt did.
type ChargeStatus string

const (
	// ChargeSettled means the money arrived and the invoice was settled through the ordinary
	// settlement path.
	ChargeSettled ChargeStatus = "settled"
	// ChargePending means the provider accepted the charge but has not settled it; the
	// webhook settles it, using the recorded attempt.
	ChargePending ChargeStatus = "pending"
	// ChargeDeclined means the provider refused; the invoice is still open.
	ChargeDeclined ChargeStatus = "declined"
	// ChargeSkipped means there was nothing to do: no default method, or nothing owed.
	ChargeSkipped ChargeStatus = "skipped"
)

// ChargeOutcome reports what one attempt did.
type ChargeOutcome struct {
	InvoiceID string        `json:"invoice_id"`
	MethodID  string        `json:"method_id,omitempty"`
	PaymentID string        `json:"payment_id,omitempty"`
	Status    ChargeStatus  `json:"status"`
	Detail    string        `json:"detail,omitempty"`
	Amount    *money.Amount `json:"amount,omitempty"`
}

// ChargeInvoice charges the customer's default saved method for one open invoice.
//
// The order of the guards matters and is deliberate: the operator's setting is checked before
// anything is read, and the adapter's capability before any attempt is recorded, so a
// deployment that cannot charge leaves no failed attempts behind to explain.
func (a *Agreements) ChargeInvoice(ctx context.Context, invoiceID string) (ChargeOutcome, error) {
	out := ChargeOutcome{InvoiceID: invoiceID}

	if a.charger == nil {
		return out, ErrOffSessionUnsupported
	}
	if a.applier == nil || a.account == "" {
		return out, ErrNotConfigured
	}
	enabled, err := a.offSessionEnabled(ctx)
	if err != nil {
		return out, err
	}
	if !enabled {
		return out, ErrOffSessionDisabled
	}

	// Read the invoice. The amount comes from the invoice row, never from a caller: an
	// off-session charge for an amount the caller chose would be a way to move arbitrary money.
	var (
		customerID string
		status     string
		currency   string
		totalMinor int64
		paidMinor  int64
	)
	err = a.pool.QueryRow(ctx, `
		SELECT customer_id, status, currency, total_minor, amount_paid_minor
		FROM invoices WHERE id = $1`, invoiceID).
		Scan(&customerID, &status, &currency, &totalMinor, &paidMinor)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, fmt.Errorf("%w: no invoice %s", ErrNotChargeable, invoiceID)
	}
	if err != nil {
		return out, fmt.Errorf("payments: load invoice: %w", err)
	}
	if status != "open" {
		out.Status = ChargeSkipped
		out.Detail = fmt.Sprintf("the invoice is %s", status)
		return out, nil
	}
	cur, err := currencyOfCode(currency)
	if err != nil {
		return out, err
	}
	outstanding := money.New(totalMinor-paidMinor, cur)
	if !outstanding.IsPositive() {
		out.Status = ChargeSkipped
		out.Detail = "nothing is owed"
		return out, nil
	}

	method, err := a.Default(ctx, customerID, a.charger.Name())
	if errors.Is(err, ErrNoDefaultMethod) {
		out.Status = ChargeSkipped
		out.Detail = "the customer has no default payment method"
		return out, nil
	}
	if err != nil {
		return out, err
	}
	out.MethodID = method.ID
	out.Amount = &outstanding

	// The mandate is read before the attempt is recorded. A token without an active
	// agreement is not permission to charge it, and discovering that after a provider call
	// would mean charging first and asking afterwards.
	agreement, err := a.activeAgreementForMethod(ctx, method.ID)
	if err != nil {
		return out, err
	}

	opKey := "offsession:" + invoiceID + ":" + method.ID
	attemptID, priorStatus, priorRef, err := a.attempt(ctx, opKey)
	if err != nil {
		return out, err
	}
	switch priorStatus {
	case "succeeded":
		// A previous run charged and settled, or charged and crashed before settling. The
		// settlement is idempotent on the provider payment id, so re-applying it is how the
		// second case heals rather than double-charging.
		if priorRef == "" {
			return out, fmt.Errorf("%w: attempt %s is settled but has no provider reference",
				ErrNotChargeable, attemptID)
		}
		if err := a.settle(ctx, invoiceID, method, priorRef, outstanding, 0); err != nil {
			return out, err
		}
		out.Status = ChargeSettled
		out.Detail = "already charged; the settlement was completed"
		return out, nil
	case "pending", "requires_action":
		out.Status = ChargePending
		out.Detail = "a charge is already in progress"
		return out, nil
	case "created":
		// A recorded attempt with no provider answer: the process died between the insert and
		// the call, or the call failed in transport. Replaying is safe because the operation key
		// is the same one, and the protocol requires a mutating call to return the first result
		// for a repeated key rather than performing a second charge.
	case "failed", "cancelled":
		// A retry loop that re-charges a declined card every minute is how a customer's card
		// gets blocked by their bank. A failed attempt is reported, not repeated; a new attempt
		// needs a new decision (a different method, or a manual payment).
		out.Status = ChargeDeclined
		out.Detail = "a previous attempt for this invoice was declined"
		return out, &DeclinedError{
			InvoiceID: invoiceID,
			MethodID:  method.ID,
			Code:      "previous_decline",
			Message:   "an earlier attempt for this invoice was declined; pay it manually or save a different method",
		}
	}

	if attemptID == "" {
		if err := a.pool.QueryRow(ctx, `
			INSERT INTO payment_attempts
				(invoice_id, gateway, operation_key, status, amount_minor, currency)
			VALUES ($1, $2, $3, 'created', $4, $5)
			RETURNING id`,
			invoiceID, a.charger.Name(), opKey, outstanding.Minor(), cur.Code(),
		).Scan(&attemptID); err != nil {
			return out, fmt.Errorf("payments: record off-session attempt: %w", err)
		}
	}

	res, err := a.charger.ChargeOffSession(ctx, OffSessionCharge{
		InvoiceID:          invoiceID,
		CustomerID:         customerID,
		MethodID:           method.ID,
		Provider:           method.Provider,
		ProviderToken:      method.Token(),
		AgreementID:        agreement.ID,
		ProviderCustomerID: agreement.ProviderCustomerID,
		AmountMinor:        outstanding.Minor(),
		Currency:           cur.Code(),
		OperationKey:       opKey,
		Description:        "Invoice " + invoiceID,
	})
	if err != nil {
		// A transport failure is not a decline: the charge may or may not have happened, so
		// the attempt stays open and the provider's webhook (or reconciliation) decides. Marking
		// it failed here would be a lie in the direction that loses money.
		return out, fmt.Errorf("payments: off-session charge: %w", err)
	}

	switch res.Status {
	case "succeeded":
		if res.ProviderPaymentID == "" {
			return out, fmt.Errorf("%w: the provider reported success without a payment id",
				ErrNotChargeable)
		}
		if res.AmountMinor > 0 && res.AmountMinor != outstanding.Minor() {
			// Recorded as a failure rather than applied: an amount that is not what was asked
			// for is an anomaly, and allocating it would launder it into revenue.
			_ = a.failAttempt(ctx, attemptID, invoiceID, method.ID, "amount_mismatch",
				"the amount the provider charged did not match the invoice, so it was not applied; the invoice is still open")
			return out, fmt.Errorf("%w: charged %d, owed %d",
				ErrAmountMismatch, res.AmountMinor, outstanding.Minor())
		}
		if err := a.settle(ctx, invoiceID, method, res.ProviderPaymentID, outstanding, res.FeeMinor); err != nil {
			return out, err
		}
		out.Status = ChargeSettled
		out.PaymentID = res.ProviderPaymentID
		out.Detail = "charged and settled"
		return out, nil

	case "pending", "requires_action":
		if _, err := a.pool.Exec(ctx, `
			UPDATE payment_attempts
			SET status = $2, provider_ref = $3, updated_at = now()
			WHERE id = $1`, attemptID, res.Status, nullableString(res.ProviderPaymentID)); err != nil {
			return out, fmt.Errorf("payments: record pending charge: %w", err)
		}
		out.Status = ChargePending
		out.Detail = "the provider accepted the charge and will confirm it"
		return out, nil
	}

	// Everything else is a decline: failed, canceled, or a status this code does not know.
	// An unknown status is treated as a failure on purpose — the safe reading of "I do not
	// know whether I took your money" is not "assume I did".
	code := res.FailureCode
	if code == "" {
		code = res.Status
	}
	if err := a.failAttempt(ctx, attemptID, invoiceID, method.ID, code,
		res.FailureMessage); err != nil {
		return out, err
	}
	out.Status = ChargeDeclined
	out.Detail = res.FailureMessage
	return out, &DeclinedError{
		InvoiceID: invoiceID,
		MethodID:  method.ID,
		Code:      code,
		Message:   res.FailureMessage,
	}
}

// ChargeDue charges every open, past-due invoice that has a default method.
//
// It is the entry point a cron calls after the renewal scan, and it is deliberately a scan
// rather than a hook inside internal/renewal: the renewal engine issues invoices and knows
// nothing about gateways, and a payment failure inside it would roll the invoice back.
// Splitting them means an invoice is always issued first and charged second, so a failed
// charge leaves a document a human can pay rather than nothing at all.
func (a *Agreements) ChargeDue(ctx context.Context, limit int) ([]ChargeOutcome, error) {
	if limit <= 0 {
		limit = 50
	}
	if a.charger == nil {
		return nil, ErrOffSessionUnsupported
	}
	enabled, err := a.offSessionEnabled(ctx)
	if err != nil {
		return nil, err
	}
	if !enabled {
		return nil, ErrOffSessionDisabled
	}

	rows, err := a.pool.Query(ctx, `
		SELECT i.id
		FROM invoices i
		JOIN payment_methods pm
		  ON pm.customer_id = i.customer_id AND pm.is_default AND pm.provider = $2
		WHERE i.status = 'open'
		  AND i.amount_paid_minor < i.total_minor
		  AND i.due_at IS NOT NULL
		  AND i.due_at <= $3
		  -- A customer with a checkout session open is about to pay by hand. Skipping is
		  -- the difference between one charge and two. This sweep's own attempts are
		  -- excluded by their key prefix, so a crashed attempt is retried rather than
		  -- blocking the invoice forever.
		  AND NOT EXISTS (
		      SELECT 1 FROM payment_attempts pa
		      WHERE pa.invoice_id = i.id
		        AND pa.status IN ('created', 'pending', 'requires_action')
		        AND pa.operation_key NOT LIKE 'offsession:%')
		ORDER BY i.due_at
		LIMIT $1`, limit, a.charger.Name(), a.Now())
	if err != nil {
		return nil, fmt.Errorf("payments: scan invoices to charge: %w", err)
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("payments: scan invoice id: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("payments: read invoices to charge: %w", err)
	}

	out := make([]ChargeOutcome, 0, len(ids))
	var runErr error
	for _, id := range ids {
		res, err := a.ChargeInvoice(ctx, id)
		out = append(out, res)
		switch {
		case err == nil:
		case errors.Is(err, ErrDeclined):
			// A decline is an outcome, not a failure of the run: it is recorded on the invoice,
			// on the attempt and on the method, and dunning escalates it. Reporting it as an
			// error here would make an operator's cron alert fire every time a card expired.
		default:
			// Anything else — a transport failure, a misconfigured account — must be visible, and
			// one invoice's problem must not stop the rest of the run.
			runErr = errors.Join(runErr, fmt.Errorf("invoice %s: %w", id, err))
		}
	}
	return out, runErr
}

// settle applies a provider-confirmed charge through the ordinary settlement path.
//
// Everything in here is one transaction: the attempt's status, the payment row, its
// allocation, the ledger postings, the invoice transition, the provisioning job and the
// audit row. That is the same set the webhook path commits, because it is the same code.
func (a *Agreements) settle(ctx context.Context, invoiceID string, m Method, providerPaymentID string, amount money.Amount, feeMinor int64) error {
	return a.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			UPDATE payment_attempts
			SET status = 'succeeded', provider_ref = $2, updated_at = now()
			WHERE operation_key = $1`,
			"offsession:"+invoiceID+":"+m.ID, providerPaymentID); err != nil {
			return fmt.Errorf("payments: mark attempt settled: %w", err)
		}

		if _, err := a.applier.ApplySettledPayment(ctx, tx, SettledPayment{
			Provider:          a.charger.Name(),
			ProviderAccount:   a.account,
			ProviderPaymentID: providerPaymentID,
			InvoiceID:         invoiceID,
			AmountMinor:       amount.Minor(),
			Currency:          amount.Currency().Code(),
		}); err != nil {
			return err
		}

		// The fee is the provider's cost of collecting the money, not revenue. It is
		// written beside the payment for reconciliation; the ledger records the gross.
		if feeMinor > 0 {
			if _, err := tx.Exec(ctx, `
				UPDATE payments SET fee_minor = $3, payment_method_id = $4
				WHERE provider = $1 AND provider_payment_id = $2`,
				a.charger.Name(), providerPaymentID, feeMinor, m.ID); err != nil {
				return fmt.Errorf("payments: record provider fee: %w", err)
			}
		} else {
			if _, err := tx.Exec(ctx, `
				UPDATE payments SET payment_method_id = $3
				WHERE provider = $1 AND provider_payment_id = $2`,
				a.charger.Name(), providerPaymentID, m.ID); err != nil {
				return fmt.Errorf("payments: record payment method: %w", err)
			}
		}

		if _, err := tx.Exec(ctx, `
			UPDATE payment_methods
			SET last_used_at = now(), last_failure_at = NULL, last_failure_code = NULL,
			    updated_at = now()
			WHERE id = $1`, m.ID); err != nil {
			return fmt.Errorf("payments: record method use: %w", err)
		}

		return a.record(ctx, tx, "payment_method.charged_off_session", m.ID, map[string]any{
			"invoice_id":          invoiceID,
			"provider_payment_id": providerPaymentID,
			"amount_minor":        amount.Minor(),
			"currency":            amount.Currency().Code(),
			"fee_minor":           feeMinor,
		})
	})
}

// failAttempt records a declined charge: the attempt, the method's failure metadata and the
// audit row, in one transaction.
//
// The invoice is not touched. It stays open, and dunning — which already escalates an open,
// past-due invoice — takes it from there.
func (a *Agreements) failAttempt(ctx context.Context, attemptID, invoiceID, methodID, code,
	customerMessage string) error {
	return a.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			UPDATE payment_attempts SET status = 'failed', updated_at = now() WHERE id = $1`,
			attemptID); err != nil {
			return fmt.Errorf("payments: mark attempt failed: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE payment_methods
			SET last_failure_at = now(), last_failure_code = $2, updated_at = now()
			WHERE id = $1`, methodID, code); err != nil {
			return fmt.Errorf("payments: record method failure: %w", err)
		}
		if err := a.record(ctx, tx, "payment_method.charge_declined", methodID, map[string]any{
			"code": code,
		}); err != nil {
			return err
		}
		// Tell the customer, in the same transaction as the decline. The error is deliberately
		// swallowed: rolling this transaction back would leave the attempt in 'created', and the
		// next run would charge the card again — the exact retry loop this function exists to
		// prevent. A missing template is an operator problem to fix; it must not resurrect a
		// declined charge.
		if a.notifier != nil {
			_, _ = notify.InvoicePaymentFailedForInvoice(ctx, tx, a.notifier, invoiceID,
				customerMessage, a.Now())
		}
		return nil
	})
}

// attempt looks up a recorded off-session attempt by its operation key.
func (a *Agreements) attempt(ctx context.Context, opKey string) (id, status, providerRef string, err error) {
	err = a.pool.QueryRow(ctx, `
		SELECT id, status, COALESCE(provider_ref, '')
		FROM payment_attempts WHERE operation_key = $1`, opKey).Scan(&id, &status, &providerRef)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", "", nil
	}
	if err != nil {
		return "", "", "", fmt.Errorf("payments: look up off-session attempt: %w", err)
	}
	return id, status, providerRef, nil
}

// currencyOfCode resolves a persisted currency code, at scale 2 for codes outside the ISO
// table, which is the schema's default assumption.
func currencyOfCode(code string) (money.Currency, error) {
	if cur, ok := money.LookupCurrency(code); ok {
		return cur, nil
	}
	return money.NewCurrency(code, 2)
}

func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}
