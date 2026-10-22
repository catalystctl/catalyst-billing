// Plans: the vocabulary of billing periods, the order a product's plans are shown in, and the
// one action that copies a product.
//
// # The period vocabulary
//
// A price is sold in one of six ways. Four of them are recurring periods, one is charged once,
// and one costs nothing. The distinction that shapes the rest of the codebase is *recurring or
// not*: only a recurring price produces a subscription, and only a subscription can be renewed.
// `Recurring` is the single predicate for that, so a new caller cannot invent its own list and
// forget a value.
//
// Arbitrary `N day/week/month/year` periods are deliberately absent. They need a period model
// that measures by days, prorates by days and still lands on a stable boundary across DST;
// `internal/calendar` is anchored to a day-of-month and to calendar months. Adding a number to
// this file would be a rewrite of that package, not a new constant (see 0028's header and
// SF-14 in docs/paymenter-parity.md).
package catalog

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
)

// The closed set of price intervals. The database enforces the same set (0028), so a value
// added here without a migration fails loudly on the first write instead of quietly living in
// a column nothing understands.
const (
	IntervalMonthly    = "monthly"
	IntervalQuarterly  = "quarterly"
	IntervalSemiannual = "semiannual"
	IntervalAnnual     = "annual"
	// IntervalOneTime is charged once. It creates a service with no subscription, so the
	// renewal engine can never see it and nothing can renew it.
	IntervalOneTime = "one_time"
	// IntervalFree is charged once for nothing. It activates the service without an invoice
	// because there is nothing to invoice.
	IntervalFree = "free"
)

// Intervals is the closed set, in the order a plan editor offers them.
var Intervals = []string{
	IntervalMonthly, IntervalQuarterly, IntervalSemiannual, IntervalAnnual,
	IntervalOneTime, IntervalFree,
}

// Recurring reports whether an interval bills again.
//
// This is the predicate the rest of the system asks, rather than comparing against a list of
// its own: `one_time` and `free` are the two values that do not recur, and a caller that
// hard-codes the four recurring names will one day be handed a fifth.
func Recurring(interval string) bool {
	switch interval {
	case IntervalMonthly, IntervalQuarterly, IntervalSemiannual, IntervalAnnual:
		return true
	default:
		return false
	}
}

// ValidateInterval rejects anything outside the closed set.
func ValidateInterval(interval string) error {
	for _, known := range Intervals {
		if interval == known {
			return nil
		}
	}
	return fmt.Errorf("%w: interval must be one of %s", ErrInvalidInput, strings.Join(Intervals, ", "))
}

// IntervalLabel is the customer-facing name of an interval, used where a page has to render
// one without a client-side table to consult.
func IntervalLabel(interval string) string {
	switch interval {
	case IntervalMonthly:
		return "per month"
	case IntervalQuarterly:
		return "per quarter"
	case IntervalSemiannual:
		return "every six months"
	case IntervalAnnual:
		return "per year"
	case IntervalOneTime:
		return "one-time"
	case IntervalFree:
		return "free"
	default:
		return interval
	}
}

// Description normalises a product description and reports the format it is stored in.
//
// A `plain` description is stored exactly as typed and rendered as text, so a `<script>` in it
// is a visible word rather than a script. An `html` description is sanitised here, on write,
// against the allowlist in sanitize.go: the stored value is what a browser will one day parse,
// so it has to be safe at rest rather than safe at render time.
func Description(text, format string) (string, string, error) {
	if strings.TrimSpace(text) == "" {
		// An empty description has no format worth remembering, and "plain" is the honest
		// default for the empty case.
		return "", "plain", nil
	}
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "", "plain":
		return text, "plain", nil
	case "html":
		clean, err := SanitizeHTML(text)
		if err != nil {
			return "", "", err
		}
		return clean, "html", nil
	default:
		return "", "", fmt.Errorf("%w: description_format must be plain or html", ErrInvalidInput)
	}
}

// Plans returns the prices in the order a product page shows them: the operator's plan order,
// and the newest version first within one order position (which is what a product with several
// legacy versions and no explicit order has always done).
func Plans(prices []Price) []Price {
	out := make([]Price, len(prices))
	copy(out, prices)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].SortOrder != out[j].SortOrder {
			return out[i].SortOrder < out[j].SortOrder
		}
		return out[i].Version > out[j].Version
	})
	return out
}

// ------------------------------------------------------------ plan ordering --

// ReorderPlansTx writes the display order of a product's plans.
//
// This is the only UPDATE the catalog performs on `price_versions`, and it is narrow on
// purpose: an amount, a currency or an interval is what an issued invoice priced from, so those
// change by appending a version. Where a plan sits in a list changes no document, so it is a
// column — and the API exposes this one action rather than a general price update.
//
// The list must name every plan of the product exactly once. A partial list would leave the
// unnamed plans at whatever order they happened to have, which is the kind of half-applied
// merchandising change nobody can explain later.
func (s *Store) ReorderPlansTx(ctx context.Context, q querier, productID string, planIDs []string) ([]Price, error) {
	if len(planIDs) == 0 {
		return nil, fmt.Errorf("%w: the plan order must name at least one plan", ErrInvalidInput)
	}
	rows, err := q.Query(ctx, `SELECT id FROM price_versions WHERE product_id = $1`, productID)
	if err != nil {
		return nil, fmt.Errorf("catalog: read plan ids: %w", err)
	}
	known := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, fmt.Errorf("catalog: scan plan id: %w", err)
		}
		known[id] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("catalog: read plan ids: %w", err)
	}
	if len(known) == 0 {
		return nil, ErrNotFound
	}
	seen := map[string]bool{}
	for _, id := range planIDs {
		if !known[id] {
			return nil, fmt.Errorf("%w: %s is not a plan of this product", ErrInvalidInput, id)
		}
		if seen[id] {
			return nil, fmt.Errorf("%w: %s appears twice in the order", ErrInvalidInput, id)
		}
		seen[id] = true
	}
	if len(seen) != len(known) {
		return nil, fmt.Errorf("%w: the order must name all %d plans", ErrInvalidInput, len(known))
	}
	for i, id := range planIDs {
		if _, err := q.Exec(ctx, `
			UPDATE price_versions SET sort_order = $3 WHERE id = $1 AND product_id = $2`,
			id, productID, i); err != nil {
			return nil, fmt.Errorf("catalog: set plan order: %w", err)
		}
	}
	return s.pricesWith(ctx, q, productID)
}

// --------------------------------------------------------------- duplicating --

// DuplicateProductTx copies a product, its price versions and its config options.
//
// Three decisions, each of which a reviewer should check rather than assume:
//
//  1. **The copy is hidden.** A duplicate that appears in the storefront the instant it is made
//     is a second listing nobody asked for, at a price nobody reviewed. `hidden = true` makes
//     "duplicate" produce a draft, and publishing it stays a deliberate act.
//  2. **The name and slug are disambiguated here, not by the caller.** `<slug>-copy`,
//     `<slug>-copy-2`, … and `<name> (copy)`, `<name> (copy 2)`, … until both are free. The
//     slug is what a URL carries, so it must be valid (lowercase, 2–64 characters) even when
//     the source slug was at the limit.
//  3. **Config options and their per-version prices are copied too.** A product without its
//     options is not a copy of the product: a hosting product's options are what the customer
//     actually chooses. Their parent links are remapped, so a nested option tree stays a tree.
//
// Price versions keep their version numbers, because those numbers are what an operator's
// notes and the panel's history refer to. The copied rows are new rows with new ids, so nothing
// that points at the source — an issued invoice, a subscription — is redirected.
func (s *Store) DuplicateProductTx(ctx context.Context, q querier, sourceID string) (Product, error) {
	type source struct {
		Slug, Name, ShortDescription, Description, DescriptionFormat string
		CategoryID, ImageID                                          *string
		Stock, PerUserLimit                                          *int
		AllowQuantity, ServerExtension, EmailTemplate                string
		SortOrder                                                    int
	}
	var src source
	err := q.QueryRow(ctx, `
		SELECT slug, name, short_description, description, description_format,
		       category_id, image_id, stock, per_user_limit, allow_quantity,
		       server_extension, email_template, sort_order
		  FROM products WHERE id = $1`, sourceID).
		Scan(&src.Slug, &src.Name, &src.ShortDescription, &src.Description, &src.DescriptionFormat,
			&src.CategoryID, &src.ImageID, &src.Stock, &src.PerUserLimit, &src.AllowQuantity,
			&src.ServerExtension, &src.EmailTemplate, &src.SortOrder)
	if err != nil {
		if err == pgx.ErrNoRows {
			return Product{}, ErrNotFound
		}
		return Product{}, fmt.Errorf("catalog: read source product: %w", err)
	}

	slug, name, err := s.freeNamesTx(ctx, q, src.Slug, src.Name)
	if err != nil {
		return Product{}, err
	}

	var newID string
	err = q.QueryRow(ctx, `
		INSERT INTO products (slug, name, short_description, description, description_format,
		                      category_id, image_id, hidden, sort_order, stock, per_user_limit,
		                      allow_quantity, server_extension, email_template)
		VALUES ($1, $2, $3, $4, $5, $6, $7, true, $8, $9, $10, $11, $12, $13)
		RETURNING id`,
		slug, name, src.ShortDescription, src.Description, src.DescriptionFormat,
		src.CategoryID, src.ImageID, src.SortOrder+1, src.Stock, src.PerUserLimit,
		src.AllowQuantity, src.ServerExtension, src.EmailTemplate).Scan(&newID)
	if err != nil {
		if isUniqueViolation(err) {
			return Product{}, ErrSlugTaken
		}
		return Product{}, fmt.Errorf("catalog: duplicate product: %w", err)
	}

	// Price versions: new ids, same version numbers and same plan order, with the old id kept
	// in a map so the config-option prices can be re-pointed at the copies.
	priceMap := map[string]string{}
	rows, err := q.Query(ctx, `
		SELECT id, version, name, currency, amount_minor, setup_minor, interval, sort_order,
		       anchor_day, anchor_timezone, provision_plan, provision_region
		  FROM price_versions WHERE product_id = $1 ORDER BY version`, sourceID)
	if err != nil {
		return Product{}, fmt.Errorf("catalog: read source prices: %w", err)
	}
	type priceRow struct {
		id, name, currency, interval, timezone, plan, region string
		version, sortOrder, anchorDay                        int
		amount, setup                                        int64
	}
	var priceRows []priceRow
	for rows.Next() {
		var r priceRow
		if err := rows.Scan(&r.id, &r.version, &r.name, &r.currency, &r.amount, &r.setup,
			&r.interval, &r.sortOrder, &r.anchorDay, &r.timezone, &r.plan, &r.region); err != nil {
			rows.Close()
			return Product{}, fmt.Errorf("catalog: scan source price: %w", err)
		}
		priceRows = append(priceRows, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return Product{}, fmt.Errorf("catalog: read source prices: %w", err)
	}
	for _, r := range priceRows {
		var newPriceID string
		err := q.QueryRow(ctx, `
			INSERT INTO price_versions (product_id, version, name, currency, amount_minor,
			                            setup_minor, interval, sort_order, anchor_day,
			                            anchor_timezone, provision_plan, provision_region)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
			RETURNING id`,
			newID, r.version, r.name, r.currency, r.amount, r.setup, r.interval, r.sortOrder,
			r.anchorDay, r.timezone, r.plan, r.region).Scan(&newPriceID)
		if err != nil {
			return Product{}, fmt.Errorf("catalog: copy price version %d: %w", r.version, err)
		}
		priceMap[r.id] = newPriceID
	}

	// Config options, in two passes: insert every option, then fix up the parent links. A
	// single pass cannot do it because a child may be read before its parent's new id exists.
	type optionRow struct {
		id, parentID, name, description, optionType, envVariable string
		choices                                                  []byte
		required, hidden, upgradable                             bool
		sortOrder                                                int
	}
	rows, err = q.Query(ctx, `
		SELECT id, COALESCE(parent_id::text, ''), name, description, option_type, choices,
		       env_variable, required, hidden, upgradable, sort_order
		  FROM config_options WHERE product_id = $1 ORDER BY sort_order, name`, sourceID)
	if err != nil {
		return Product{}, fmt.Errorf("catalog: read source options: %w", err)
	}
	var optionRows []optionRow
	for rows.Next() {
		var r optionRow
		if err := rows.Scan(&r.id, &r.parentID, &r.name, &r.description, &r.optionType,
			&r.choices, &r.envVariable, &r.required, &r.hidden, &r.upgradable, &r.sortOrder); err != nil {
			rows.Close()
			return Product{}, fmt.Errorf("catalog: scan source option: %w", err)
		}
		optionRows = append(optionRows, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return Product{}, fmt.Errorf("catalog: read source options: %w", err)
	}
	optionMap := map[string]string{}
	for _, r := range optionRows {
		var newOptionID string
		err := q.QueryRow(ctx, `
			INSERT INTO config_options (product_id, name, description, option_type, choices,
			                            env_variable, required, hidden, upgradable, sort_order)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
			RETURNING id`,
			newID, r.name, r.description, r.optionType, r.choices, r.envVariable,
			r.required, r.hidden, r.upgradable, r.sortOrder).Scan(&newOptionID)
		if err != nil {
			return Product{}, fmt.Errorf("catalog: copy config option: %w", err)
		}
		optionMap[r.id] = newOptionID
	}
	for _, r := range optionRows {
		if r.parentID == "" {
			continue
		}
		newParent, ok := optionMap[r.parentID]
		if !ok {
			continue
		}
		if _, err := q.Exec(ctx, `UPDATE config_options SET parent_id = $2 WHERE id = $1`,
			optionMap[r.id], newParent); err != nil {
			return Product{}, fmt.Errorf("catalog: copy config option parent: %w", err)
		}
	}
	// Per-version option prices, re-pointed at both the new options and the new versions.
	for oldOption, newOption := range optionMap {
		priceRows, err := q.Query(ctx, `
			SELECT price_version_id, choice, amount_minor, setup_minor
			  FROM config_option_prices WHERE config_option_id = $1`, oldOption)
		if err != nil {
			return Product{}, fmt.Errorf("catalog: read source option prices: %w", err)
		}
		type optionPrice struct {
			priceVersionID, choice string
			amount, setup          int64
		}
		var prices []optionPrice
		for priceRows.Next() {
			var p optionPrice
			if err := priceRows.Scan(&p.priceVersionID, &p.choice, &p.amount, &p.setup); err != nil {
				priceRows.Close()
				return Product{}, fmt.Errorf("catalog: scan source option price: %w", err)
			}
			prices = append(prices, p)
		}
		priceRows.Close()
		if err := priceRows.Err(); err != nil {
			return Product{}, fmt.Errorf("catalog: read source option prices: %w", err)
		}
		for _, p := range prices {
			newPriceVersion, ok := priceMap[p.priceVersionID]
			if !ok {
				continue
			}
			if _, err := q.Exec(ctx, `
				INSERT INTO config_option_prices (config_option_id, price_version_id, choice,
				                                 amount_minor, setup_minor)
				VALUES ($1, $2, $3, $4, $5)`,
				newOption, newPriceVersion, p.choice, p.amount, p.setup); err != nil {
				return Product{}, fmt.Errorf("catalog: copy option price: %w", err)
			}
		}
	}

	return s.productTx(ctx, q, newID)
}

// freeNamesTx finds a slug and a name for a copy that are not already taken.
//
// It is a loop rather than a suffix on a timestamp because an operator will read the slug in a
// URL and in a report; "-copy-2" is something a person can recognise, and a random suffix is
// not. The attempt cap keeps a pathological catalog from spinning.
func (s *Store) freeNamesTx(ctx context.Context, q querier, slug, name string) (string, string, error) {
	base := slug
	if len(base) > 58 {
		// Room for "-copy-99" inside the 64-character slug limit.
		base = strings.Trim(base[:58], "-")
	}
	for attempt := 1; attempt <= 100; attempt++ {
		candidateSlug := base + "-copy"
		candidateName := name + " (copy)"
		if attempt > 1 {
			candidateSlug = fmt.Sprintf("%s-copy-%d", base, attempt)
			candidateName = fmt.Sprintf("%s (copy %d)", name, attempt)
		}
		var taken bool
		if err := q.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM products WHERE lower(slug) = lower($1))`,
			candidateSlug).Scan(&taken); err != nil {
			return "", "", fmt.Errorf("catalog: check slug availability: %w", err)
		}
		if taken {
			continue
		}
		if err := ValidateSlug(candidateSlug); err != nil {
			return "", "", err
		}
		return candidateSlug, candidateName, nil
	}
	return "", "", fmt.Errorf("%w: could not find a free slug for a copy of %s", ErrInvalidInput, slug)
}
