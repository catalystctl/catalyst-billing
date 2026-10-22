// Tax: a dated rate table, resolved per jurisdiction.
//
// The invoice path already takes an exact rate and records evidence
// (`billing.Pricer`, `billing.TaxEvidence`). What was missing is *where the rate comes
// from*. This file is that answer: a table of dated rows keyed by
// (country, region, tax class), with `all` as the wildcard country and an effective
// window so a rate change appends a row instead of rewriting a filed period.
//
// Two consequences of it being a `billing.Pricer` rather than a new money path:
//
//   - The arithmetic is `billing.FixedTaxPricer`'s, unchanged: one exact rational rate,
//     one explicit inclusive/exclusive mode, one rounding step per line. There is no
//     second implementation of "how tax is computed" to drift.
//   - The ledger posting (`tax_payable`) does not change, because the evidence shape does
//     not change. A resolved rate looks exactly like a configured one to the issuer.
//
// The jurisdiction *policy* — which country a digital service is supplied in, whether a
// VAT ID is validated — is deliberately not decided here. `TaxPricer` is configured with
// the jurisdiction the caller has already decided on (usually the customer's billing
// country), and the resolution below is a lookup, not a judgement.

package commerce

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/catalyst/billing/internal/billing"
	"github.com/catalyst/billing/internal/money"
	"github.com/jackc/pgx/v5"
)

// WildcardCountry is the country code that matches anywhere. It is the same literal the
// migration's CHECK allows, and it is exported so the UI and the tests name one constant.
const WildcardCountry = "all"

// DefaultTaxClass is what a product with no explicit class is taxed as.
const DefaultTaxClass = "standard"

// TaxRate is one dated rate row.
type TaxRate struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// CountryCode is an ISO 3166-1 alpha-2 code, or WildcardCountry.
	CountryCode string `json:"country_code"`
	// RegionCode is an optional subdivision (a US state, a Canadian province). Empty means
	// the whole country.
	RegionCode string `json:"region_code"`
	TaxClass   string `json:"tax_class"`
	// Rate is the exact rational, stored and returned as a string ("0.19", "1/5").
	Rate string `json:"rate"`
	// Inclusive means the rate is contained in the quoted price rather than added to it.
	Inclusive     bool       `json:"inclusive"`
	EffectiveFrom time.Time  `json:"effective_from"`
	EffectiveTo   *time.Time `json:"effective_to"`
	Enabled       bool       `json:"enabled"`
	CreatedAt     time.Time  `json:"created_at"`
}

// TaxRateInput is the wire shape for creating a rate.
type TaxRateInput struct {
	Name        string `json:"name"`
	CountryCode string `json:"country_code"`
	RegionCode  string `json:"region_code"`
	TaxClass    string `json:"tax_class"`
	// Rate is an exact rational: "0.19", "19%", "1/5". A float is never accepted, because
	// 0.1 is not representable and a tax rate is money.
	Rate string `json:"rate"`
	// Inclusive defaults to the installation's tax mode when nil.
	Inclusive     *bool      `json:"inclusive"`
	EffectiveFrom *time.Time `json:"effective_from"`
	// EffectiveTo closes a rate. Setting it is how a rate change is recorded without
	// rewriting the period it applied to.
	EffectiveTo *time.Time `json:"effective_to"`
	Enabled     *bool      `json:"enabled"`
}

// TaxRateColumns is the projection every tax read shares.
const taxRateColumns = `
	id, name, country_code, region_code, tax_class, rate, inclusive,
	effective_from, effective_to, enabled, created_at`

func scanTaxRate(row pgx.Row) (TaxRate, error) {
	var t TaxRate
	if err := row.Scan(&t.ID, &t.Name, &t.CountryCode, &t.RegionCode, &t.TaxClass,
		&t.Rate, &t.Inclusive, &t.EffectiveFrom, &t.EffectiveTo, &t.Enabled,
		&t.CreatedAt); err != nil {
		return TaxRate{}, err
	}
	return t, nil
}

// TaxRates lists every rate, current and historic, newest effective date first.
func (s *Store) TaxRates(ctx context.Context) ([]TaxRate, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+taxRateColumns+`
		  FROM tax_rates
		 ORDER BY country_code, region_code, tax_class, effective_from DESC`)
	if err != nil {
		return nil, fmt.Errorf("commerce: list tax rates: %w", err)
	}
	defer rows.Close()
	out := make([]TaxRate, 0, 16)
	for rows.Next() {
		t, err := scanTaxRate(rows)
		if err != nil {
			return nil, fmt.Errorf("commerce: scan tax rate: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// TaxRate reads one rate by id.
func (s *Store) TaxRate(ctx context.Context, id string) (TaxRate, error) {
	t, err := scanTaxRate(s.pool.QueryRow(ctx,
		`SELECT `+taxRateColumns+` FROM tax_rates WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return TaxRate{}, ErrNotFound
	}
	if err != nil {
		return TaxRate{}, fmt.Errorf("commerce: read tax rate: %w", err)
	}
	return t, nil
}

// CreateTaxRate appends a rate.
//
// A rate is never edited in place: the migration's unique index allows exactly one
// open-ended enabled rate per (country, region, class), so appending a new one means
// closing the old one first. Doing that automatically here is what makes "the rate changed
// on 1 July" a fact rather than a guess.
func (s *Store) CreateTaxRate(ctx context.Context, in TaxRateInput) (TaxRate, error) {
	var out TaxRate
	err := s.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = s.CreateTaxRateTx(ctx, tx, in)
		return err
	})
	return out, err
}

// CreateTaxRateTx appends a rate inside a caller's transaction.
func (s *Store) CreateTaxRateTx(ctx context.Context, q querier, in TaxRateInput) (TaxRate, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return TaxRate{}, invalid("name is required")
	}
	country := strings.ToUpper(strings.TrimSpace(in.CountryCode))
	if country == "" {
		return TaxRate{}, invalid("country_code is required (use %q for a wildcard)", WildcardCountry)
	}
	if strings.EqualFold(country, WildcardCountry) {
		// The column stores the literal lower-case 'all' the migration's CHECK allows, so a
		// client sending "ALL" is normalised rather than refused.
		country = WildcardCountry
	} else if len(country) != 2 || !isUpperAlpha(country) {
		return TaxRate{}, invalid("country_code must be two upper-case letters or %q", WildcardCountry)
	}
	region := strings.ToUpper(strings.TrimSpace(in.RegionCode))
	if country == WildcardCountry && region != "" {
		return TaxRate{}, invalid("a wildcard country cannot have a region")
	}
	class := strings.TrimSpace(in.TaxClass)
	if class == "" {
		class = DefaultTaxClass
	}
	rate, err := parseTaxRate(in.Rate)
	if err != nil {
		return TaxRate{}, err
	}
	inclusive := false
	if in.Inclusive != nil {
		inclusive = *in.Inclusive
	} else {
		// Fall back to the installation's configured default mode. A missing setting is
		// "exclusive", which is the same default the settings seed writes.
		var mode string
		if err := q.QueryRow(ctx,
			`SELECT value FROM settings WHERE group_key = 'tax' AND key = 'default_mode'`).
			Scan(&mode); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return TaxRate{}, fmt.Errorf("commerce: read tax default mode: %w", err)
		}
		inclusive = strings.EqualFold(strings.TrimSpace(mode), "inclusive")
	}
	from := stamp(timeOrZero(in.EffectiveFrom))
	if in.EffectiveFrom == nil {
		from = s.now()
	}
	if in.EffectiveTo != nil && !in.EffectiveTo.After(from) {
		return TaxRate{}, invalid("effective_to must be after effective_from")
	}
	enabled := true
	if in.Enabled != nil {
		enabled = *in.Enabled
	}

	var id string
	err = q.QueryRow(ctx, `
		INSERT INTO tax_rates (name, country_code, region_code, tax_class, rate, inclusive,
			effective_from, effective_to, enabled)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		RETURNING id`,
		name, country, region, class, rate, inclusive, from, in.EffectiveTo, enabled).Scan(&id)
	if err != nil {
		if isUniqueViolation(err) {
			return TaxRate{}, fmt.Errorf(
				"%w: an open-ended rate for %s/%s/%s already exists; close it first",
				ErrInUse, country, displayRegion(region), class)
		}
		if isCheckViolation(err) {
			return TaxRate{}, invalid("the tax rate violates a constraint")
		}
		return TaxRate{}, fmt.Errorf("commerce: create tax rate: %w", err)
	}
	return s.taxRateWith(ctx, q, id)
}

// UpdateTaxRate edits a rate's presentation and its window, but not its arithmetic.
//
// Changing `rate` would rewrite what a past period was taxed at, so it is refused once the
// rate has taken effect. The supported operation is: close this rate and append a new one.
func (s *Store) UpdateTaxRate(ctx context.Context, id string, in TaxRateInput) (TaxRate, error) {
	var out TaxRate
	err := s.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = s.UpdateTaxRateTx(ctx, tx, id, in)
		return err
	})
	return out, err
}

// UpdateTaxRateTx edits a rate inside a caller's transaction.
func (s *Store) UpdateTaxRateTx(ctx context.Context, q querier, id string, in TaxRateInput) (TaxRate, error) {
	current, err := s.taxRateWith(ctx, q, id)
	if err != nil {
		return TaxRate{}, err
	}
	name := current.Name
	if strings.TrimSpace(in.Name) != "" {
		name = strings.TrimSpace(in.Name)
	}
	rate := current.Rate
	if strings.TrimSpace(in.Rate) != "" {
		parsed, err := parseTaxRate(in.Rate)
		if err != nil {
			return TaxRate{}, err
		}
		if parsed != current.Rate {
			// A filed rate is evidence. Rewriting it would change what an invoice issued
			// last quarter can be explained by.
			if !current.EffectiveFrom.After(s.now()) {
				return TaxRate{}, fmt.Errorf(
					"%w: this rate is already in effect; close it and append a new one",
					ErrInUse)
			}
			rate = parsed
		}
	}
	inclusive := current.Inclusive
	if in.Inclusive != nil {
		inclusive = *in.Inclusive
	}
	from := current.EffectiveFrom
	if in.EffectiveFrom != nil {
		from = in.EffectiveFrom.UTC()
	}
	to := current.EffectiveTo
	if in.EffectiveTo != nil {
		to = in.EffectiveTo
	}
	if to != nil && !to.After(from) {
		return TaxRate{}, invalid("effective_to must be after effective_from")
	}
	enabled := current.Enabled
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	country := current.CountryCode
	region := current.RegionCode
	class := current.TaxClass
	if _, err := q.Exec(ctx, `
		UPDATE tax_rates
		   SET name = $2, rate = $3, inclusive = $4, effective_from = $5, effective_to = $6,
		       enabled = $7
		 WHERE id = $1`,
		id, name, rate, inclusive, from, to, enabled); err != nil {
		if isUniqueViolation(err) {
			return TaxRate{}, fmt.Errorf(
				"%w: an open-ended rate for %s/%s/%s already exists",
				ErrInUse, country, displayRegion(region), class)
		}
		if isCheckViolation(err) {
			return TaxRate{}, invalid("the tax rate violates a constraint")
		}
		return TaxRate{}, fmt.Errorf("commerce: update tax rate: %w", err)
	}
	return s.taxRateWith(ctx, q, id)
}

func (s *Store) taxRateWith(ctx context.Context, q querier, id string) (TaxRate, error) {
	t, err := scanTaxRate(q.QueryRow(ctx,
		`SELECT `+taxRateColumns+` FROM tax_rates WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return TaxRate{}, ErrNotFound
	}
	if err != nil {
		return TaxRate{}, fmt.Errorf("commerce: read tax rate: %w", err)
	}
	return t, nil
}

// DeleteTaxRate removes a rate that never took effect. A rate that has applied to an issued
// invoice is refused, because deleting it would leave that invoice's evidence pointing at
// nothing.
func (s *Store) DeleteTaxRate(ctx context.Context, id string) error {
	return s.DeleteTaxRateTx(ctx, s.pool, id)
}

// DeleteTaxRateTx removes a rate inside a caller's transaction.
func (s *Store) DeleteTaxRateTx(ctx context.Context, q querier, id string) error {
	current, err := s.taxRateWith(ctx, q, id)
	if err != nil {
		return err
	}
	if !current.EffectiveFrom.After(s.now()) {
		return fmt.Errorf("%w: this rate has already taken effect; disable it instead", ErrInUse)
	}
	tag, err := q.Exec(ctx, `DELETE FROM tax_rates WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("commerce: delete tax rate: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ------------------------------------------------------------------ lookup --

// ResolveRate finds the rate that applies at an instant.
//
// The resolution order is the whole point of the wildcard, and it is ordered from most to
// least specific:
//
//  1. country + region + class, for a jurisdiction that taxes a subdivision differently
//  2. country + (no region) + class, the ordinary national rate
//  3. `all` + class, the fallback for "everything not otherwise matched"
//  4. `all` + `standard`, the installation-wide fallback
//
// A row matches when `effective_from <= at` and (`effective_to is null` or `at <
// effective_to`). Among matches for one key the latest `effective_from` wins, so an
// appended rate shadows the one it replaced without needing the old one closed.
//
// It returns ErrNotFound when nothing matches, which the pricer treats as "no tax" only
// when the installation has tax disabled. A silent zero rate for an unconfigured
// jurisdiction is the one outcome a tax table must never produce by accident.
func (s *Store) ResolveRate(ctx context.Context, country, region, class string, at time.Time) (TaxRate, error) {
	return s.resolveRateWith(ctx, s.pool, country, region, class, at)
}

func (s *Store) resolveRateWith(ctx context.Context, q querier, country, region, class string, at time.Time) (TaxRate, error) {
	country = strings.ToUpper(strings.TrimSpace(country))
	region = strings.ToUpper(strings.TrimSpace(region))
	class = strings.TrimSpace(class)
	if class == "" {
		class = DefaultTaxClass
	}
	if at.IsZero() {
		at = s.now()
	}
	at = at.UTC()

	// The query is a single round trip with an explicit priority, so the precedence is in
	// one place rather than in a Go loop that a later edit could reorder.
	row := q.QueryRow(ctx, `
		SELECT `+taxRateColumns+`
		  FROM (
		    SELECT `+taxRateColumns+`,
		           CASE
		             WHEN country_code = $1 AND region_code = $2 AND tax_class = $3 THEN 0
		             WHEN country_code = $1 AND region_code = '' AND tax_class = $3 THEN 1
		             WHEN country_code = $4 AND tax_class = $3 THEN 2
		             WHEN country_code = $4 AND tax_class = $5 THEN 3
		           END AS priority
		      FROM tax_rates
		     WHERE enabled
		       AND effective_from <= $6
		       AND (effective_to IS NULL OR $6 < effective_to)
		       AND (
		            (country_code = $1 AND region_code = $2 AND tax_class = $3)
		         OR (country_code = $1 AND region_code = '' AND tax_class = $3 AND $2 <> '')
		         OR (country_code = $4 AND tax_class = $3)
		         OR (country_code = $4 AND tax_class = $5)
		       )
		  ) ranked
		 WHERE priority IS NOT NULL
		 ORDER BY priority, effective_from DESC
		 LIMIT 1`,
		country, region, class, WildcardCountry, DefaultTaxClass, at)

	t, err := scanTaxRate(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return TaxRate{}, ErrNotFound
	}
	if err != nil {
		return TaxRate{}, fmt.Errorf("commerce: resolve tax rate: %w", err)
	}
	return t, nil
}

// TaxJurisdiction is the place a rate resolved to, rendered for evidence.
func TaxJurisdiction(t TaxRate) string {
	if t.CountryCode == WildcardCountry {
		return WildcardCountry + "/" + t.TaxClass
	}
	if t.RegionCode != "" {
		return t.CountryCode + "-" + t.RegionCode + "/" + t.TaxClass
	}
	return t.CountryCode + "/" + t.TaxClass
}

// ------------------------------------------------------------------ pricer --

// TaxPricer is a billing.Pricer that resolves its rate from the tax table.
//
// It is configured with the jurisdiction the caller has already decided on — normally the
// customer's billing country and region — plus the tax class of what is being sold. Each
// call to Price resolves the rate *at that instant*, so a rate appended today applies to
// today's invoice and last quarter's invoice still reproduces from the row that applied
// then.
//
// When no rate matches, the pricer charges nothing and records why. That is the honest
// outcome for an installation that has tax enabled but has not configured a jurisdiction:
// the invoice says "no rate for XX" in its evidence rather than silently pretending the
// rate is zero.
type TaxPricer struct {
	Store *Store
	// CountryCode and RegionCode locate the supply. Empty country means "resolve nothing",
	// which is the no-tax path.
	CountryCode string
	RegionCode  string
	// TaxClass defaults to DefaultTaxClass.
	TaxClass string
	// Exempt, when true, charges no tax and records ExemptionReason. It is the reverse
	// charge / valid-VAT-ID case: the *decision* is the caller's, the evidence is ours.
	Exempt          bool
	ExemptionReason string
	// Inclusive overrides the resolved rate's own mode when set. Nil keeps the rate's mode,
	// which is what a per-jurisdiction inclusive/exclusive rule needs.
	Inclusive *bool
	// Rounding defaults to money.RoundHalfUp.
	Rounding money.Rounding
	// At pins the instant the rate is resolved at. Zero means "now".
	At time.Time
}

// Price implements billing.Pricer.
func (p TaxPricer) Price(ctx context.Context, lines []billing.Line, cur money.Currency) ([]billing.LineResult, billing.Totals, billing.TaxEvidence, error) {
	at := p.At
	if at.IsZero() {
		at = time.Now().UTC()
	}
	class := p.TaxClass
	if class == "" {
		class = DefaultTaxClass
	}

	// Exemption short-circuits before any lookup: a tax-exempt customer is charged zero
	// with a reason, and a reason is what makes the zero defensible.
	if p.Exempt {
		reason := strings.TrimSpace(p.ExemptionReason)
		if reason == "" {
			reason = "tax exempt"
		}
		return p.zeroTax(ctx, lines, cur, billing.TaxEvidence{
			Mode:            "exclusive",
			Policy:          "tax-exempt",
			Rate:            "0",
			Jurisdiction:    strings.TrimSpace(p.CountryCode),
			ExemptionReason: reason,
		})
	}

	if strings.TrimSpace(p.CountryCode) == "" {
		return p.zeroTax(ctx, lines, cur, billing.TaxEvidence{
			Mode:            "exclusive",
			Policy:          "no-jurisdiction",
			Rate:            "0",
			ExemptionReason: "no billing country on file",
		})
	}
	if p.Store == nil {
		return nil, billing.Totals{}, billing.TaxEvidence{},
			fmt.Errorf("commerce: tax pricer has no store")
	}

	rate, err := p.Store.resolveRateWith(ctx, p.Store.pool, p.CountryCode, p.RegionCode, class, at)
	if err != nil {
		if !errors.Is(err, ErrNotFound) {
			return nil, billing.Totals{}, billing.TaxEvidence{}, err
		}
		// No configured rate for this jurisdiction. Charge nothing, and say exactly why.
		return p.zeroTax(ctx, lines, cur, billing.TaxEvidence{
			Mode:            "exclusive",
			Policy:          "no-rate",
			Rate:            "0",
			Jurisdiction:    TaxJurisdiction(TaxRate{CountryCode: strings.ToUpper(p.CountryCode), RegionCode: p.RegionCode, TaxClass: class}),
			ExemptionReason: "no tax rate is configured for this jurisdiction",
		})
	}

	ratio, err := money.ParseRatio(rate.Rate)
	if err != nil {
		return nil, billing.Totals{}, billing.TaxEvidence{},
			fmt.Errorf("commerce: tax rate %s is not a rational: %w", rate.ID, err)
	}
	inclusive := rate.Inclusive
	if p.Inclusive != nil {
		inclusive = *p.Inclusive
	}
	rounding := p.Rounding
	if !rounding.Valid() {
		rounding = money.RoundHalfUp
	}
	mode := "exclusive"
	if inclusive {
		mode = "inclusive"
	}

	// The arithmetic is the existing pricer's, not a second copy of it.
	delegate := billing.FixedTaxPricer{
		Rate:         ratio,
		Inclusive:    inclusive,
		Rounding:     rounding,
		Policy:       "tax-rate:" + rate.ID,
		Jurisdiction: TaxJurisdiction(rate),
	}
	results, totals, evidence, err := delegate.Price(ctx, lines, cur)
	if err != nil {
		return nil, billing.Totals{}, billing.TaxEvidence{}, err
	}
	// Record which dated row was used, so the document explains itself a year later.
	evidence.Mode = mode
	return results, totals, evidence, nil
}

// zeroTax prices every line with no tax, preserving the line shape so the invoice and the
// ledger do not need a special case for "no tax".
func (p TaxPricer) zeroTax(ctx context.Context, lines []billing.Line, cur money.Currency, evidence billing.TaxEvidence) ([]billing.LineResult, billing.Totals, billing.TaxEvidence, error) {
	delegate := billing.FixedTaxPricer{
		Rate:         money.ZeroRatio(),
		Inclusive:    false,
		Rounding:     money.RoundHalfUp,
		Policy:       evidence.Policy,
		Jurisdiction: evidence.Jurisdiction,
	}
	results, totals, _, err := delegate.Price(ctx, lines, cur)
	if err != nil {
		return nil, billing.Totals{}, billing.TaxEvidence{}, err
	}
	return results, totals, evidence, nil
}

// TaxEnabled reports whether the installation charges tax at all. The setting is the
// master switch; a rate table with the switch off must not start charging.
func (s *Store) TaxEnabled(ctx context.Context) (bool, error) {
	var value string
	err := s.pool.QueryRow(ctx,
		`SELECT value FROM settings WHERE group_key = 'tax' AND key = 'enabled'`).Scan(&value)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("commerce: read tax setting: %w", err)
	}
	return strings.EqualFold(strings.TrimSpace(value), "true"), nil
}

// parseTaxRate validates and canonicalises an exact rational rate.
func parseTaxRate(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", invalid("rate is required")
	}
	ratio, err := money.ParseRatio(trimmed)
	if err != nil {
		return "", invalid("rate %q is not an exact number (use \"0.19\", \"19%%\" or \"1/5\")", raw)
	}
	if ratio.IsNegative() {
		return "", invalid("rate must not be negative")
	}
	// 100% is the ceiling; above it the tax exceeds the price.
	if ratio.Float64() > 1 {
		return "", invalid("rate must not exceed 100%%")
	}
	return exactDecimal(ratio), nil
}

// exactDecimal renders a rational as an exact decimal string when the denominator is a
// product of 2s and 5s (which every human-written rate is), and as a fraction otherwise.
//
// The point is readability rather than precision: "0.19" is what an operator typed and what
// they expect to see back, while "19/100" is mathematically identical but reads like a
// machine's answer. Neither form is ever parsed as a float.
func exactDecimal(r money.Ratio) string {
	rat := r.Rat()
	den := new(big.Int).Set(rat.Denom())
	twos, fives := 0, 0
	two, five := big.NewInt(2), big.NewInt(5)
	zero, rem := big.NewInt(0), new(big.Int)
	for {
		if rem.Mod(den, two).Cmp(zero) == 0 {
			den.Div(den, two)
			twos++
			continue
		}
		if rem.Mod(den, five).Cmp(zero) == 0 {
			den.Div(den, five)
			fives++
			continue
		}
		break
	}
	if den.Cmp(big.NewInt(1)) != 0 {
		// A denominator with another prime factor has no finite decimal expansion, so the
		// exact fraction is the honest representation.
		return rat.RatString()
	}
	places := twos
	if fives > places {
		places = fives
	}
	if places == 0 {
		return rat.Num().String()
	}
	// Scale by 10^places and insert the decimal point by string surgery.
	scaled := new(big.Rat).Mul(rat, new(big.Rat).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(places)), nil)))
	digits := scaled.Num().String()
	negative := strings.HasPrefix(digits, "-")
	if negative {
		digits = digits[1:]
	}
	for len(digits) <= places {
		digits = "0" + digits
	}
	out := digits[:len(digits)-places] + "." + digits[len(digits)-places:]
	out = strings.TrimRight(out, "0")
	out = strings.TrimSuffix(out, ".")
	if negative {
		out = "-" + out
	}
	return out
}

func isUpperAlpha(s string) bool {
	for _, r := range s {
		if r < 'A' || r > 'Z' {
			return false
		}
	}
	return true
}

func displayRegion(region string) string {
	if region == "" {
		return "*"
	}
	return region
}

func timeOrZero(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return t.UTC()
}
