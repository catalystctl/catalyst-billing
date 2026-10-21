package billing

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// Billing profiles are the customer's own account details: the legal name, the
// address and the tax identifier that appear on an invoice.
//
// They are *input to the next invoice*, never a rewrite of a past one. The issuer copies
// them into the issued document's snapshot, and the database trigger freezes that
// snapshot, so correcting an address today cannot change what a tax authority already
// received.
//
// The rules encoded here:
//
//   - A missing profile is not an error. A customer created by CLI or by an import has
//     no profile, and issuing an invoice for them must still work; the snapshot simply
//     carries what is known.
//   - The country code is validated as ISO 3166-1 alpha-2 *format* only. Validating it
//     against a real jurisdiction list would imply a compliance claim this product does
//     not make.
//   - The tax id is stored and printed as given. It is not validated, because a
//     validation failure would be a claim about the customer's jurisdiction rather than
//     about our data.

// Profile is a customer's billing details.
type Profile struct {
	CustomerID   string `json:"customer_id"`
	LegalName    string `json:"legal_name"`
	AddressLine1 string `json:"address_line1"`
	AddressLine2 string `json:"address_line2"`
	City         string `json:"city"`
	PostalCode   string `json:"postal_code"`
	Region       string `json:"region"`
	CountryCode  string `json:"country_code"`
	TaxID        string `json:"tax_id"`
}

// ErrInvalidProfile is returned for a profile that cannot be stored.
var ErrInvalidProfile = errors.New("billing: invalid billing profile")

// Querier is the read surface a profile load needs.
type Querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// LoadProfile reads a customer's billing details, falling back to the customer record
// when no profile row exists.
//
// The fallback matters: a customer with no profile must still produce a usable invoice,
// and the name and email on the customer record are the minimum a document needs.
func LoadProfile(ctx context.Context, q Querier, customerID string) (Profile, error) {
	p := Profile{CustomerID: customerID}
	var (
		name, email string
		legalName   *string
		addr1, addr2,
		city, postal, region, country, taxID *string
	)
	err := q.QueryRow(ctx, `
		SELECT c.name, c.email, bp.legal_name,
		       bp.address_line1, bp.address_line2, bp.city, bp.postal_code,
		       bp.region, bp.country_code, bp.tax_id
		FROM customers c
		LEFT JOIN billing_profiles bp ON bp.customer_id = c.id
		WHERE c.id = $1`, customerID).
		Scan(&name, &email, &legalName, &addr1, &addr2, &city, &postal, &region, &country, &taxID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Profile{}, fmt.Errorf("%w: customer %s", ErrInvalidProfile, customerID)
	}
	if err != nil {
		return Profile{}, fmt.Errorf("billing: load billing profile: %w", err)
	}
	// The profile's own legal name wins; the customer record's name is the fallback so a
	// customer with no profile still has something to print on a document.
	p.LegalName = name
	derefInto(&p.LegalName, legalName)
	derefInto(&p.AddressLine1, addr1)
	derefInto(&p.AddressLine2, addr2)
	derefInto(&p.City, city)
	derefInto(&p.PostalCode, postal)
	derefInto(&p.Region, region)
	derefInto(&p.CountryCode, country)
	derefInto(&p.TaxID, taxID)
	if p.LegalName == "" {
		p.LegalName = name
	}
	return p, nil
}

// CustomerDetailsFor renders a profile as the snapshot's customer block.
//
// The address is composed into one string because that is what the snapshot field has
// always held, and a snapshot schema change would invalidate documents already issued.
func CustomerDetailsFor(p Profile, email string) CustomerDetails {
	lines := make([]string, 0, 4)
	for _, part := range []string{p.AddressLine1, p.AddressLine2, p.City, p.Region, p.PostalCode, p.CountryCode} {
		if part != "" {
			lines = append(lines, part)
		}
	}
	address := ""
	for i, line := range lines {
		if i > 0 {
			address += ", "
		}
		address += line
	}
	return CustomerDetails{
		ID:             p.CustomerID,
		Name:           p.LegalName,
		Email:          email,
		BillingAddress: address,
		TaxID:          p.TaxID,
		CountryCode:    p.CountryCode,
	}
}

// ValidateProfile checks a profile before it is stored.
func ValidateProfile(p Profile) error {
	if p.CustomerID == "" {
		return fmt.Errorf("%w: missing customer", ErrInvalidProfile)
	}
	if p.CountryCode != "" {
		if len(p.CountryCode) != 2 {
			return fmt.Errorf("%w: country_code must be two letters", ErrInvalidProfile)
		}
		for _, r := range p.CountryCode {
			if r < 'A' || r > 'Z' {
				return fmt.Errorf("%w: country_code must be upper-case ISO 3166-1 alpha-2",
					ErrInvalidProfile)
			}
		}
	}
	const maxField = 200
	for _, f := range []struct {
		name  string
		value string
	}{
		{"legal_name", p.LegalName},
		{"address_line1", p.AddressLine1},
		{"address_line2", p.AddressLine2},
		{"city", p.City},
		{"postal_code", p.PostalCode},
		{"region", p.Region},
		{"tax_id", p.TaxID},
	} {
		if len(f.value) > maxField {
			return fmt.Errorf("%w: %s is longer than %d bytes", ErrInvalidProfile, f.name, maxField)
		}
	}
	return nil
}

// SaveProfile writes a profile inside the caller's transaction.
//
// It upserts: a customer either has a profile or does not, and "save" should not require
// the client to know which.
func SaveProfile(ctx context.Context, tx pgx.Tx, p Profile) error {
	if err := ValidateProfile(p); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO billing_profiles (customer_id, legal_name, address_line1, address_line2,
			city, postal_code, region, country_code, tax_id, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, now())
		ON CONFLICT (customer_id) DO UPDATE
			SET legal_name = EXCLUDED.legal_name,
			    address_line1 = EXCLUDED.address_line1,
			    address_line2 = EXCLUDED.address_line2,
			    city = EXCLUDED.city,
			    postal_code = EXCLUDED.postal_code,
			    region = EXCLUDED.region,
			    country_code = EXCLUDED.country_code,
			    tax_id = EXCLUDED.tax_id,
			    updated_at = now()`,
		p.CustomerID, p.LegalName, p.AddressLine1, p.AddressLine2,
		p.City, p.PostalCode, p.Region, p.CountryCode, p.TaxID)
	if err != nil {
		return fmt.Errorf("billing: save billing profile: %w", err)
	}
	return nil
}

// customerEmail returns the address on the customer record.
func customerEmail(ctx context.Context, q Querier, customerID string) (string, error) {
	var email string
	if err := q.QueryRow(ctx, `SELECT email FROM customers WHERE id = $1`, customerID).Scan(&email); err != nil {
		return "", fmt.Errorf("billing: load customer email: %w", err)
	}
	return email, nil
}

// FillCustomer populates an IssueRequest's customer block from the customer record and
// its billing profile.
//
// It is what makes "the next invoice carries the new details" true for every issuance
// path, not only the one that remembered to load them.
func FillCustomer(ctx context.Context, q Querier, c *CustomerDetails) error {
	if c == nil || c.ID == "" {
		return fmt.Errorf("%w: customer has no id", ErrInvalidInvoice)
	}
	profile, err := LoadProfile(ctx, q, c.ID)
	if err != nil {
		return err
	}
	email, err := customerEmail(ctx, q, c.ID)
	if err != nil {
		return err
	}
	*c = CustomerDetailsFor(profile, email)
	return nil
}

func derefInto(dst *string, src *string) {
	if src != nil {
		*dst = *src
	}
}
