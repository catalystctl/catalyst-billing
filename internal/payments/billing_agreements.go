package payments

// Billing agreements: the record of permission, not just the card.
//
// A payment method answers "which card". A billing agreement answers the question a
// customer, a support agent or a dispute asks a year later: "why were we allowed to
// charge it?". The row therefore carries the provider's own mandate reference, the
// customer it belongs to, the method it authorises, and the circumstances of consent.
//
// The table is append-only in spirit: a cancelled mandate is a status change with a
// timestamp, never a delete, because deleting it would delete the evidence. Removing the
// *method* does cascade the agreement away with the credential (there is no card left to
// charge), which is why the schema's foreign key is ON DELETE CASCADE.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

var (
	// ErrAgreementNotFound is returned for an unknown agreement, or one belonging to
	// another customer. The scoped query cannot tell the two apart, deliberately.
	ErrAgreementNotFound = errors.New("payments: no such billing agreement")
	// ErrNoAgreement is returned when an off-session charge is attempted for a method
	// that has no active mandate. A stored token alone is not permission to use it.
	ErrNoAgreement = errors.New("payments: the saved method has no active billing agreement")
	// ErrInvalidAgreement is returned for a mandate that cannot be recorded.
	ErrInvalidAgreement = errors.New("payments: invalid billing agreement")
)

// BillingAgreement is one customer's permission to charge a method off-session.
type BillingAgreement struct {
	ID                  string
	CustomerID          string
	PaymentMethodID     string
	Provider            string
	ProviderAgreementID string
	ProviderCustomerID  string
	Status              string
	ConsentAt           *time.Time
	ConsentIP           string
	ConsentUserAgent    string
	TermsVersion        string
	CreatedAt           time.Time
	UpdatedAt           time.Time
	CancelledAt         *time.Time
}

// AgreementRequest is one instruction to record a mandate.
type AgreementRequest struct {
	CustomerID          string
	Provider            string
	ProviderAgreementID string
	ProviderCustomerID  string
	ConsentIP           string
	ConsentUserAgent    string
	TermsVersion        string
}

func (r AgreementRequest) normalize() (AgreementRequest, error) {
	r.CustomerID = strings.TrimSpace(r.CustomerID)
	r.Provider = strings.ToLower(strings.TrimSpace(r.Provider))
	r.ProviderAgreementID = strings.TrimSpace(r.ProviderAgreementID)
	r.ProviderCustomerID = strings.TrimSpace(r.ProviderCustomerID)
	r.ConsentIP = strings.TrimSpace(r.ConsentIP)
	r.ConsentUserAgent = strings.TrimSpace(r.ConsentUserAgent)
	r.TermsVersion = strings.TrimSpace(r.TermsVersion)
	switch {
	case r.CustomerID == "":
		return r, fmt.Errorf("%w: the agreement needs a customer", ErrInvalidAgreement)
	case r.Provider == "":
		return r, fmt.Errorf("%w: the agreement needs a provider", ErrInvalidAgreement)
	case len(r.Provider) > 64:
		return r, fmt.Errorf("%w: the provider name is too long", ErrInvalidAgreement)
	case len(r.ProviderAgreementID) > 512:
		return r, fmt.Errorf("%w: the provider reference is too long", ErrInvalidAgreement)
	case len(r.ProviderCustomerID) > 512:
		return r, fmt.Errorf("%w: the provider customer reference is too long", ErrInvalidAgreement)
	case len(r.ConsentIP) > 64:
		return r, fmt.Errorf("%w: the consent address is too long", ErrInvalidAgreement)
	case len(r.ConsentUserAgent) > 512:
		return r, fmt.Errorf("%w: the consent user agent is too long", ErrInvalidAgreement)
	case len(r.TermsVersion) > 80:
		return r, fmt.Errorf("%w: the terms version is too long", ErrInvalidAgreement)
	}
	for _, ch := range r.Provider {
		if !(ch >= 'a' && ch <= 'z') && !(ch >= '0' && ch <= '9') && ch != '.' && ch != '_' && ch != '-' {
			return r, fmt.Errorf("%w: the provider name has an unexpected character", ErrInvalidAgreement)
		}
	}
	return r, nil
}

const agreementColumns = `id, customer_id, COALESCE(payment_method_id::text, ''), provider,
	COALESCE(provider_agreement_id, ''), COALESCE(provider_customer_id, ''),
	status, consent_at, COALESCE(consent_ip, ''), COALESCE(consent_user_agent, ''),
	terms_version, created_at, updated_at, cancelled_at`

func scanAgreement(row pgx.Row) (BillingAgreement, error) {
	var a BillingAgreement
	if err := row.Scan(&a.ID, &a.CustomerID, &a.PaymentMethodID, &a.Provider,
		&a.ProviderAgreementID, &a.ProviderCustomerID, &a.Status, &a.ConsentAt,
		&a.ConsentIP, &a.ConsentUserAgent, &a.TermsVersion, &a.CreatedAt,
		&a.UpdatedAt, &a.CancelledAt); err != nil {
		return BillingAgreement{}, err
	}
	return a, nil
}

// CreatePendingAgreement records a mandate the customer has started but not finished.
//
// It is separate from a saved method because the customer has not entered a card yet;
// the provider's hosted page is open, and the agreement waits until it reports a token.
func (a *Agreements) CreatePendingAgreement(ctx context.Context, req AgreementRequest) (BillingAgreement, error) {
	req, err := req.normalize()
	if err != nil {
		return BillingAgreement{}, err
	}
	var out BillingAgreement
	err = a.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			INSERT INTO billing_agreements
				(customer_id, provider, provider_agreement_id, provider_customer_id, status,
				 consent_ip, consent_user_agent, terms_version)
			VALUES ($1, $2, NULLIF($3, ''), NULLIF($4, ''), 'pending', NULLIF($5, ''), NULLIF($6, ''), $7)
			ON CONFLICT (provider, provider_agreement_id)
				WHERE provider_agreement_id IS NOT NULL AND provider_agreement_id <> ''
			DO UPDATE SET updated_at = now()
			RETURNING `+agreementColumns,
			req.CustomerID, req.Provider, req.ProviderAgreementID, req.ProviderCustomerID,
			req.ConsentIP, req.ConsentUserAgent, req.TermsVersion)
		m, err := scanAgreement(row)
		if err != nil {
			return fmt.Errorf("payments: record pending agreement: %w", err)
		}
		out = m
		return a.recordAgreementAudit(ctx, tx, "billing_agreement.pending", m.ID, map[string]any{
			"provider": m.Provider,
			"status":   m.Status,
		})
	})
	if err != nil {
		return BillingAgreement{}, err
	}
	return out, nil
}

// AttachProviderReference fills in the provider's mandate id after the hosted session is
// created. Until this point the local agreement has no provider reference, because the
// provider had not issued one.
func (a *Agreements) AttachProviderReference(ctx context.Context, customerID, id, providerRef, providerCustomerID string) (BillingAgreement, error) {
	var out BillingAgreement
	err := a.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			UPDATE billing_agreements
			SET provider_agreement_id = NULLIF($3, ''),
			    provider_customer_id = COALESCE(NULLIF($4, ''), provider_customer_id),
			    updated_at = now()
			WHERE id = $1 AND customer_id = $2 AND status = 'pending'
			RETURNING `+agreementColumns,
			id, customerID, providerRef, providerCustomerID)
		m, err := scanAgreement(row)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: %s", ErrAgreementNotFound, id)
		}
		if err != nil {
			return fmt.Errorf("payments: attach agreement reference: %w", err)
		}
		out = m
		return nil
	})
	if err != nil {
		return BillingAgreement{}, err
	}
	return out, nil
}

// GetAgreement returns one agreement scoped to its customer.
func (a *Agreements) GetAgreement(ctx context.Context, customerID, id string) (BillingAgreement, error) {
	m, err := scanAgreement(a.pool.QueryRow(ctx, `
		SELECT `+agreementColumns+` FROM billing_agreements WHERE id = $1 AND customer_id = $2`,
		id, customerID))
	if errors.Is(err, pgx.ErrNoRows) {
		return BillingAgreement{}, fmt.Errorf("%w: %s", ErrAgreementNotFound, id)
	}
	if err != nil {
		return BillingAgreement{}, fmt.Errorf("payments: load billing agreement: %w", err)
	}
	return m, nil
}

// ListAgreements returns a customer's mandates, newest first.
func (a *Agreements) ListAgreements(ctx context.Context, customerID string) ([]BillingAgreement, error) {
	rows, err := a.pool.Query(ctx, `
		SELECT `+agreementColumns+` FROM billing_agreements
		WHERE customer_id = $1 ORDER BY created_at DESC`, customerID)
	if err != nil {
		return nil, fmt.Errorf("payments: list billing agreements: %w", err)
	}
	defer rows.Close()
	out := make([]BillingAgreement, 0, 4)
	for rows.Next() {
		m, err := scanAgreement(rows)
		if err != nil {
			return nil, fmt.Errorf("payments: scan billing agreement: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// CancelAgreement revokes a mandate without deleting the evidence.
func (a *Agreements) CancelAgreement(ctx context.Context, customerID, id string) error {
	return a.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE billing_agreements
			SET status = 'cancelled', cancelled_at = now(), updated_at = now()
			WHERE id = $1 AND customer_id = $2 AND status <> 'cancelled'`, id, customerID)
		if err != nil {
			return fmt.Errorf("payments: cancel billing agreement: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return fmt.Errorf("%w: %s", ErrAgreementNotFound, id)
		}
		return a.recordAgreementAudit(ctx, tx, "billing_agreement.cancelled", id, nil)
	})
}

// recordAgreement is the consent written by Save, in the same transaction as the method.
//
// Hosted confirmation calls Save with the provider reference, so this upserts the pending
// row created when the customer was sent to the provider. A direct token save has no
// provider reference and creates its own active row. Either way, the invariant is that a
// saved method never exists without a mandate that says when it was allowed.
func (a *Agreements) recordAgreement(ctx context.Context, tx pgx.Tx, m Method, req SaveMethodRequest) error {
	consentAt := req.ConsentAt
	if consentAt == nil {
		now := a.Now()
		consentAt = &now
	}

	var id string
	if req.ProviderAgreementID == "" {
		// A directly tokenised method has no provider mandate object. Re-saving the same
		// token must update the one active row, not insert a second one: the partial unique
		// index is the database saying the same thing.
		err := tx.QueryRow(ctx, `
			UPDATE billing_agreements
			SET provider_customer_id = COALESCE(NULLIF($2, ''), provider_customer_id),
			    consent_at = COALESCE(consent_at, $3),
			    consent_ip = COALESCE(NULLIF($4, ''), consent_ip),
			    consent_user_agent = COALESCE(NULLIF($5, ''), consent_user_agent),
			    terms_version = CASE WHEN $6 <> '' THEN $6 ELSE terms_version END,
			    status = 'active', cancelled_at = NULL, updated_at = now()
			WHERE payment_method_id = $1 AND status = 'active'
			RETURNING id`,
			m.ID, req.ProviderCustomerID, consentAt, req.ConsentIP, req.ConsentUserAgent,
			req.TermsVersion).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			err = tx.QueryRow(ctx, `
				INSERT INTO billing_agreements
					(customer_id, payment_method_id, provider, status, consent_at,
					 consent_ip, consent_user_agent, terms_version)
				VALUES ($1, $2, $3, 'active', $4, NULLIF($5, ''), NULLIF($6, ''), $7)
				RETURNING id`,
				m.CustomerID, m.ID, m.Provider, consentAt, req.ConsentIP,
				req.ConsentUserAgent, req.TermsVersion).Scan(&id)
		}
		if err != nil {
			return fmt.Errorf("payments: record billing agreement: %w", err)
		}
	} else {
		// Activating a hosted mandate makes it the one active agreement for this method.
		// A different active row (for example, one written by a direct token save while the
		// hosted page was open) is cancelled first, because two active mandates would make
		// "the permission this charge relied on" ambiguous.
		if _, err := tx.Exec(ctx, `
			UPDATE billing_agreements
			SET status = 'cancelled', cancelled_at = now(), updated_at = now()
			WHERE payment_method_id = $1 AND status = 'active'
			  AND NOT (provider = $2 AND provider_agreement_id = $3)`,
			m.ID, m.Provider, req.ProviderAgreementID); err != nil {
			return fmt.Errorf("payments: replace active agreement: %w", err)
		}
		err := tx.QueryRow(ctx, `
			INSERT INTO billing_agreements
				(customer_id, payment_method_id, provider, provider_agreement_id,
				 provider_customer_id, status, consent_at, consent_ip, consent_user_agent,
				 terms_version)
			VALUES ($1, $2, $3, $4, NULLIF($5, ''), 'active', $6,
			        NULLIF($7, ''), NULLIF($8, ''), $9)
			ON CONFLICT (provider, provider_agreement_id)
				WHERE provider_agreement_id IS NOT NULL AND provider_agreement_id <> ''
			DO UPDATE SET
				payment_method_id = EXCLUDED.payment_method_id,
				provider_customer_id = COALESCE(EXCLUDED.provider_customer_id, billing_agreements.provider_customer_id),
				status = 'active',
				consent_at = COALESCE(billing_agreements.consent_at, EXCLUDED.consent_at),
				consent_ip = COALESCE(EXCLUDED.consent_ip, billing_agreements.consent_ip),
				consent_user_agent = COALESCE(EXCLUDED.consent_user_agent, billing_agreements.consent_user_agent),
				terms_version = CASE WHEN EXCLUDED.terms_version <> '' THEN EXCLUDED.terms_version
				                     ELSE billing_agreements.terms_version END,
				cancelled_at = NULL,
				updated_at = now()
			RETURNING id`,
			m.CustomerID, m.ID, m.Provider, req.ProviderAgreementID, req.ProviderCustomerID,
			consentAt, req.ConsentIP, req.ConsentUserAgent, req.TermsVersion).Scan(&id)
		if err != nil {
			return fmt.Errorf("payments: record billing agreement: %w", err)
		}
	}

	return a.recordAgreementAudit(ctx, tx, "billing_agreement.active", id, map[string]any{
		"provider":          m.Provider,
		"payment_method_id": m.ID,
		"last4":             m.Last4,
		"terms_version":     req.TermsVersion,
	})
}

// activeAgreementForMethod reads the mandate a charge relies on.
func (a *Agreements) activeAgreementForMethod(ctx context.Context, methodID string) (BillingAgreement, error) {
	ag, err := scanAgreement(a.pool.QueryRow(ctx, `
		SELECT `+agreementColumns+` FROM billing_agreements
		WHERE payment_method_id = $1 AND status = 'active'
		ORDER BY created_at DESC LIMIT 1`, methodID))
	if errors.Is(err, pgx.ErrNoRows) {
		return BillingAgreement{}, fmt.Errorf("%w: method %s", ErrNoAgreement, methodID)
	}
	if err != nil {
		return BillingAgreement{}, fmt.Errorf("payments: load active agreement: %w", err)
	}
	return ag, nil
}

func (a *Agreements) recordAgreementAudit(ctx context.Context, tx pgx.Tx, action, id string, changes map[string]any) error {
	return a.recordFor(ctx, tx, action, "billing_agreement", id, changes)
}
