package tax

// Sales-tax nexus: where the installation may collect subnational tax, and where it may not.
//
// A nexus declaration is an assertion about the seller — a warehouse, employees, or an economic
// threshold crossed in a state — and it is the one fact in the subnational rule that a panel
// cannot derive. So it is stored, with the note and the person who declared it, and read by the
// rules engine as "collect here".
//
// Two properties keep it honest:
//
//   - **One declaration per jurisdiction.** The unique index is the same device the rate table uses
//     for its open rate: with two rows, "is there nexus" would depend on row order.
//   - **Deleting is allowed, retiring is preferred.** A nexus declaration that turned out to be
//     wrong is a mistake an operator must be able to correct; unlike a rate, no issued invoice
//     points at it (the invoice snapshot records the *decision*, not the nexus row). Deleting is
//     therefore permitted, and the audit row is what keeps the history.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Errors the nexus surface maps to problem codes.
var (
	// ErrNexusExists is a duplicate declaration for one jurisdiction.
	ErrNexusExists = errors.New("tax: a nexus declaration already exists for this jurisdiction")
	// ErrInvalidNexus is a declaration that is not a jurisdiction at all.
	ErrInvalidNexus = errors.New("tax: invalid nexus declaration")
	// ErrNexusNotFound is a declaration that has already been removed.
	ErrNexusNotFound = errors.New("tax: no such nexus declaration")
)

// Nexus is one declaration.
type Nexus struct {
	ID          string    `json:"id"`
	CountryCode string    `json:"country_code"`
	RegionCode  string    `json:"region_code"`
	Note        string    `json:"note"`
	Enabled     bool      `json:"enabled"`
	CreatedBy   string    `json:"created_by,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	// UpdatedBy is who last changed the declaration, when anyone has.
	UpdatedBy string `json:"updated_by,omitempty"`
}

// Jurisdiction renders the declaration the way the rate table renders a place of supply:
// "US-CA" or "US" for a national registration.
func (n Nexus) Jurisdiction() string {
	if n.RegionCode == "" {
		return n.CountryCode
	}
	return n.CountryCode + "-" + n.RegionCode
}

// NexusInput is the wire shape for declaring nexus.
type NexusInput struct {
	CountryCode string `json:"country_code"`
	RegionCode  string `json:"region_code"`
	Note        string `json:"note"`
	// Enabled defaults to true. A retired declaration is kept so the screen can show that the
	// installation used to collect somewhere and stopped, which is a question an auditor asks.
	Enabled *bool `json:"enabled"`
}

const nexusColumns = `id, country_code, region_code, note, enabled, created_by, created_at, updated_at, updated_by`

func scanNexus(row pgx.Row) (Nexus, error) {
	var (
		n                    Nexus
		createdBy, updatedBy *string
	)
	if err := row.Scan(&n.ID, &n.CountryCode, &n.RegionCode, &n.Note, &n.Enabled,
		&createdBy, &n.CreatedAt, &n.UpdatedAt, &updatedBy); err != nil {
		return Nexus{}, err
	}
	if createdBy != nil {
		n.CreatedBy = *createdBy
	}
	if updatedBy != nil {
		n.UpdatedBy = *updatedBy
	}
	return n, nil
}

// NexusList lists every declaration, country then region.
func (r *Rules) NexusList(ctx context.Context) ([]Nexus, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT `+nexusColumns+` FROM tax_nexus
		 ORDER BY country_code, region_code`)
	if err != nil {
		return nil, fmt.Errorf("tax: list nexus declarations: %w", err)
	}
	defer rows.Close()
	out := make([]Nexus, 0, 8)
	for rows.Next() {
		n, err := scanNexus(rows)
		if err != nil {
			return nil, fmt.Errorf("tax: scan nexus declaration: %w", err)
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// DeclareNexus records a nexus declaration inside the caller's transaction, so the audit row that
// says who declared it commits with it.
func (r *Rules) DeclareNexus(ctx context.Context, q querier, in NexusInput, actorID string) (Nexus, error) {
	country := strings.ToUpper(strings.TrimSpace(in.CountryCode))
	if len(country) != 2 {
		return Nexus{}, fmt.Errorf("%w: country_code must be two letters, e.g. US", ErrInvalidNexus)
	}
	for _, c := range country {
		if c < 'A' || c > 'Z' {
			return Nexus{}, fmt.Errorf("%w: country_code must be two upper-case letters", ErrInvalidNexus)
		}
	}
	region := strings.ToUpper(strings.TrimSpace(in.RegionCode))
	if strings.ContainsAny(region, " -./") {
		return Nexus{}, fmt.Errorf("%w: region_code must be a compact subdivision code, e.g. CA", ErrInvalidNexus)
	}
	enabled := true
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	var id string
	err := q.QueryRow(ctx, `
		INSERT INTO tax_nexus (country_code, region_code, note, enabled, created_by, updated_by)
		VALUES ($1, $2, $3, $4, NULLIF($5, '')::uuid, NULLIF($5, '')::uuid)
		RETURNING id`,
		country, region, strings.TrimSpace(in.Note), enabled, actorID).Scan(&id)
	if err != nil {
		if isUniqueViolation(err) {
			return Nexus{}, fmt.Errorf("%w: %s", ErrNexusExists, jurisdictionText(country, region))
		}
		return Nexus{}, fmt.Errorf("tax: declare nexus: %w", err)
	}
	return r.nexusByID(ctx, q, id)
}

// RetireNexus deletes a declaration. Nothing points at it: an invoice records the decision the
// rules made, not the row, so removing a declaration that was wrong does not rewrite a document.
func (r *Rules) RetireNexus(ctx context.Context, q querier, id string) error {
	tag, err := q.Exec(ctx, `DELETE FROM tax_nexus WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("tax: delete nexus declaration: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNexusNotFound
	}
	return nil
}

func (r *Rules) nexusByID(ctx context.Context, q querier, id string) (Nexus, error) {
	n, err := scanNexus(q.QueryRow(ctx, `SELECT `+nexusColumns+` FROM tax_nexus WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Nexus{}, ErrNexusNotFound
	}
	if err != nil {
		return Nexus{}, fmt.Errorf("tax: read nexus declaration: %w", err)
	}
	return n, nil
}

// hasNexus reports whether an enabled declaration covers exactly this jurisdiction (region "" is
// the country-wide declaration).
func (r *Rules) hasNexus(ctx context.Context, country, region string) (bool, error) {
	var exists bool
	err := r.pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM tax_nexus
			 WHERE enabled AND country_code = $1 AND region_code = $2)`,
		country, region).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("tax: read nexus declaration: %w", err)
	}
	return exists, nil
}

// nexusGoverns reports whether the installation uses nexus as the governing rule in a country,
// which is what *any* enabled declaration there means.
func (r *Rules) nexusGoverns(ctx context.Context, country string) (bool, error) {
	var exists bool
	err := r.pool.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM tax_nexus WHERE enabled AND country_code = $1)`,
		country).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("tax: read nexus declarations for %s: %w", country, err)
	}
	return exists, nil
}

// NexusGaps lists the jurisdictions with an open rate but no enabled nexus declaration, for a
// country whose tax the installation governs by nexus.
//
// It exists for the screen rather than for the engine: a rate in a state where the installation
// declared no nexus is charged nowhere, and "a number is configured, therefore tax is collected" is
// the assumption an operator makes. This is the query that disproves it before an audit does.
func (r *Rules) NexusGaps(ctx context.Context, country string) ([]string, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT DISTINCT tr.country_code, tr.region_code
		  FROM tax_rates tr
		 WHERE tr.enabled
		   AND tr.country_code = $1
		   AND NOT EXISTS (
		         SELECT 1 FROM tax_nexus tn
		          WHERE tn.enabled
		            AND tn.country_code = tr.country_code
		            AND (tn.region_code = tr.region_code OR tn.region_code = ''))
		 ORDER BY tr.country_code, tr.region_code`, strings.ToUpper(strings.TrimSpace(country)))
	if err != nil {
		return nil, fmt.Errorf("tax: read nexus gaps for %s: %w", country, err)
	}
	defer rows.Close()
	out := make([]string, 0, 8)
	for rows.Next() {
		var c, region string
		if err := rows.Scan(&c, &region); err != nil {
			return nil, fmt.Errorf("tax: scan nexus gap: %w", err)
		}
		out = append(out, jurisdictionText(c, region))
	}
	return out, rows.Err()
}

// isUniqueViolation reports a duplicate key, so a second declaration for one jurisdiction is a
// conflict the caller can explain rather than a 500.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "23505"
	}
	return false
}

func jurisdictionText(country, region string) string {
	if region == "" {
		return country
	}
	return country + "-" + region
}
