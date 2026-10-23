// The storefront's merchandising: how a catalogue row is presented to a visitor, as opposed to
// what it costs.
//
// Three things live here, and they share a property that keeps them out of catalog.go: none of
// them can change a price, a visibility rule or an issued document.
//
//   - **Merchandising fields.** `stock`, `per_user_limit`, `allow_quantity` and `featured` are
//     row properties that existed in SQL and could not be written through the API, so an
//     operator could not put a product out of stock or sell two of it. They are written by one
//     narrow endpoint rather than through the product PUT, because the PUT is a full replacement
//     of metadata: a caller that did not know about `stock` (the description editor, the
//     hide/unhide toggle) would reset it to unlimited on every save.
//
//   - **Config options, as a visitor reads them.** The options exist since 0020 with a price per
//     price version; what was missing is a read that hands the storefront one product's options
//     with the amounts for the plan the visitor picked. Without it a product with a required
//     option renders a plan nobody can actually buy.
//
//   - **The home page's category strip.** Paymenter shows top-level categories that have
//     children or visible products, ordered by `sort`. The catalogue read returns every visible
//     category, so the rule is applied here, on the tree, where a branch's product total is
//     already computed.
//
// Everything here is additive: a catalogue with no category images, no featured product and the
// default quantity policy reads and writes exactly as it did.
package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

// The quantity policies, spelled once. The database CHECK (0020) accepts exactly these, so a
// value outside the set is refused here with a sentence rather than by a constraint.
const (
	QuantityDisabled  = "disabled"
	QuantitySeparated = "separated"
	QuantityCombined  = "combined"
)

// QuantityPolicies is the closed set, in the order a plan editor offers them.
var QuantityPolicies = []string{QuantityDisabled, QuantitySeparated, QuantityCombined}

// Merchandising is the presentation half of a product: how many there are, how many one
// customer may hold, whether a quantity field is offered, and whether the shop pushes it.
//
// All four are written together, because they are one screen and one decision. The struct is
// also the read shape, so the panel renders what it is about to replace.
type Merchandising struct {
	// Stock is units available; nil means unlimited, 0 means sold out.
	Stock *int `json:"stock"`
	// PerUserLimit caps how many one customer may hold; nil means no cap.
	PerUserLimit *int `json:"per_user_limit"`
	// AllowQuantity is one of QuantityPolicies. Empty means "leave it as it is", which is what
	// a client that only wants to change the stock sends.
	AllowQuantity string `json:"allow_quantity"`
	// Featured puts the product at the front of the storefront.
	Featured bool `json:"featured"`
}

// Validate refuses a merchandising write the database would refuse, with a sentence.
//
// The checks mirror 0020's CHECK constraints on purpose: an operator gets "a negative stock is
// not a thing", not "23514". They are not a substitute for the constraints, which are what make
// the invariant true for a write that never comes through this function.
func (m Merchandising) Validate() error {
	if m.Stock != nil && *m.Stock < 0 {
		return fmt.Errorf("%w: stock cannot be negative (leave it empty for unlimited)", ErrInvalidInput)
	}
	if m.PerUserLimit != nil && *m.PerUserLimit < 1 {
		return fmt.Errorf("%w: a per-customer limit must be at least one (leave it empty for no limit)", ErrInvalidInput)
	}
	switch m.AllowQuantity {
	case "":
		// Unchanged.
	case QuantityDisabled, QuantitySeparated, QuantityCombined:
	default:
		return fmt.Errorf("%w: allow_quantity must be one of %s",
			ErrInvalidInput, strings.Join(QuantityPolicies, ", "))
	}
	return nil
}

// InStock reports whether the product can be bought right now. A nil stock is unlimited.
func (p Product) InStock() bool { return p.Stock == nil || *p.Stock > 0 }

// LowStock reports whether the remaining stock is worth telling a visitor about. It is a
// separate question from InStock so a page cannot accidentally show "only 0 left".
func (p Product) LowStock(threshold int) bool {
	return p.Stock != nil && *p.Stock > 0 && *p.Stock <= threshold
}

// MerchandisingOf reads a product's merchandising fields.
func (s *Store) MerchandisingOf(ctx context.Context, productID string) (Merchandising, error) {
	var m Merchandising
	err := s.pool.QueryRow(ctx, `
		SELECT stock, per_user_limit, allow_quantity, featured FROM products WHERE id = $1`,
		productID).Scan(&m.Stock, &m.PerUserLimit, &m.AllowQuantity, &m.Featured)
	if errors.Is(err, pgx.ErrNoRows) {
		return Merchandising{}, ErrNotFound
	}
	if err != nil {
		return Merchandising{}, fmt.Errorf("catalog: read merchandising: %w", err)
	}
	return m, nil
}

// SetMerchandisingTx writes a product's presentation fields and returns the row it wrote.
//
// It is its own write rather than part of UpdateProductTx for the reason in the package
// comment: the product PUT is a full replacement, and the two other callers of it (the
// description editor and the hide toggle) know nothing about stock. Making them send it would
// mean every description save silently restocked a sold-out product.
//
// An empty AllowQuantity leaves the stored policy alone; everything else is written as sent, so
// clearing stock back to unlimited is an explicit nil rather than an omission.
func (s *Store) SetMerchandisingTx(ctx context.Context, q querier, productID string, m Merchandising) (Product, error) {
	if err := m.Validate(); err != nil {
		return Product{}, err
	}
	var policy any
	if m.AllowQuantity != "" {
		policy = m.AllowQuantity
	}
	tag, err := q.Exec(ctx, `
		UPDATE products
		   SET stock = $2, per_user_limit = $3,
		       allow_quantity = COALESCE($4, allow_quantity), featured = $5
		 WHERE id = $1`,
		productID, m.Stock, m.PerUserLimit, policy, m.Featured)
	if err != nil {
		return Product{}, fmt.Errorf("catalog: set merchandising: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return Product{}, ErrNotFound
	}
	return s.productTx(ctx, q, productID)
}

// ------------------------------------------------------------------ options --

// ConfigOption is one choice a customer makes on a product: "8 GB", "extra backup", a hostname.
//
// It carries its per-plan amounts rather than one amount, because an option is priced per price
// version: "8 GB" costs one thing on the monthly plan and another on the annual one. A visitor
// picking a plan therefore needs the amounts that belong to that plan, and a read that merged
// them would quote a price the checkout does not charge.
type ConfigOption struct {
	ID           string              `json:"id"`
	Name         string              `json:"name"`
	Description  string              `json:"description"`
	OptionType   string              `json:"option_type"`
	Choices      []string            `json:"choices"`
	Required     bool                `json:"required"`
	Hidden       bool                `json:"hidden"`
	SortOrder    int                 `json:"sort_order"`
	ParentID     *string             `json:"parent_id"`
	EnvVariable  string              `json:"env_variable"`
	Upgradable   bool                `json:"upgradable"`
	OptionPrices []ConfigOptionPrice `json:"prices"`
}

// ConfigOptionPrice is one choice's amount on one price version. A choice with no row is free.
type ConfigOptionPrice struct {
	PriceVersionID string `json:"price_id"`
	Choice         string `json:"choice"`
	AmountMinor    int64  `json:"amount_minor"`
	SetupMinor     int64  `json:"setup_minor"`
}

// ConfigOptions reads a product's options, hidden ones included: whether an option is offered to
// a visitor is a decision for the read path, not for this query, because the panel has to be able
// to show what it is hiding.
func (s *Store) ConfigOptions(ctx context.Context, productID string) ([]ConfigOption, error) {
	return s.configOptionsWith(ctx, s.pool, productID)
}

func (s *Store) configOptionsWith(ctx context.Context, q querier, productID string) ([]ConfigOption, error) {
	rows, err := q.Query(ctx, `
		SELECT id, name, description, option_type, choices, required, hidden, sort_order,
		       parent_id, env_variable, upgradable
		  FROM config_options
		 WHERE product_id = $1
		 ORDER BY sort_order, name`, productID)
	if err != nil {
		return nil, fmt.Errorf("catalog: read config options: %w", err)
	}
	defer rows.Close()
	out := make([]ConfigOption, 0, 4)
	index := map[string]int{}
	for rows.Next() {
		var o ConfigOption
		var choices []byte
		if err := rows.Scan(&o.ID, &o.Name, &o.Description, &o.OptionType, &choices,
			&o.Required, &o.Hidden, &o.SortOrder, &o.ParentID, &o.EnvVariable,
			&o.Upgradable); err != nil {
			return nil, fmt.Errorf("catalog: scan config option: %w", err)
		}
		o.Choices = decodeChoices(choices)
		o.OptionPrices = []ConfigOptionPrice{}
		index[o.ID] = len(out)
		out = append(out, o)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("catalog: read config options: %w", err)
	}
	if len(out) == 0 {
		return out, nil
	}
	priceRows, err := q.Query(ctx, `
		SELECT p.config_option_id, p.price_version_id, p.choice, p.amount_minor, p.setup_minor
		  FROM config_option_prices p
		  JOIN config_options o ON o.id = p.config_option_id
		 WHERE o.product_id = $1`, productID)
	if err != nil {
		return nil, fmt.Errorf("catalog: read config option prices: %w", err)
	}
	defer priceRows.Close()
	for priceRows.Next() {
		var optionID string
		var p ConfigOptionPrice
		if err := priceRows.Scan(&optionID, &p.PriceVersionID, &p.Choice, &p.AmountMinor,
			&p.SetupMinor); err != nil {
			return nil, fmt.Errorf("catalog: scan config option price: %w", err)
		}
		if i, ok := index[optionID]; ok {
			out[i].OptionPrices = append(out[i].OptionPrices, p)
		}
	}
	return out, priceRows.Err()
}

// decodeChoices reads the jsonb choices array. A malformed value yields no choices rather than
// an error: a broken option must not take the whole product page down, and an option with no
// choices is visibly empty rather than silently wrong.
func decodeChoices(raw []byte) []string {
	var choices []string
	if len(raw) == 0 {
		return []string{}
	}
	if err := json.Unmarshal(raw, &choices); err != nil {
		return []string{}
	}
	return choices
}

// ------------------------------------------------------------------ home page --

// HomeCategories is the storefront's category strip (SF-01).
//
// Paymenter lists the *top-level* categories that have children or visible products, ordered by
// sort. The tree read returns every visible category, so the rule is applied here, and it is
// applied to the products the visitor was actually offered: a branch whose only products are
// priced in another currency is an empty tab for this visitor and is not drawn.
//
// Two shapes, both of them a decision an operator makes in the settings:
//
//   - `top_level` (the default, and Paymenter's rule) keeps the roots and nests below them. A
//     root whose only child is empty is not offered, because clicking it would show an empty
//     page — the difference between "has children" and "has children worth listing".
//   - `all` flattens the visible tree, so a shop that wants its child categories in the strip
//     does not have to unnest them.
//
// Order is the tree's order, which is `sort_order` then name; a strip that re-sorted itself by
// product count would be a second, unexplained decision.
//
// `products` is the visibility- and currency-filtered list from the same request. Passing it in
// rather than re-deriving counts from the tree is what keeps the strip and the grid agreeing: a
// category cannot be offered when nothing in it survived the filter.
func HomeCategories(nodes []CategoryNode, products []Product, mode string) []CategoryNode {
	visible := VisibleCategoryTree(nodes)
	direct := make(map[string]int, len(products))
	for _, p := range products {
		if p.CategoryID != nil {
			direct[*p.CategoryID]++
		}
	}
	pruned, _ := pruneBranches(visible, direct)
	if strings.EqualFold(mode, "all") {
		return flattenBranches(pruned)
	}
	return pruned
}

// pruneBranches drops every category with no product in its branch and recomputes the branch
// totals from the products that survived, so the count on a tab is the count the grid shows.
//
// It returns the branch total as well as the nodes because the caller's recursion needs it: a
// parent survives on its children's products, not only on its own.
func pruneBranches(nodes []CategoryNode, direct map[string]int) ([]CategoryNode, int) {
	out := make([]CategoryNode, 0, len(nodes))
	total := 0
	for _, n := range nodes {
		children, childTotal := pruneBranches(n.Children, direct)
		here := direct[n.ID]
		if here+childTotal == 0 {
			continue
		}
		n.Children = children
		n.SubtreeProductCount = here + childTotal
		out = append(out, n)
		total += here + childTotal
	}
	return out, total
}

// flattenBranches turns a tree into the depth-first list a flat strip renders, dropping the
// nested copies so no category is drawn twice.
func flattenBranches(nodes []CategoryNode) []CategoryNode {
	out := make([]CategoryNode, 0, len(nodes))
	for _, n := range nodes {
		children := n.Children
		n.Children = nil
		out = append(out, n)
		out = append(out, flattenBranches(children)...)
	}
	return out
}

// --------------------------------------------------------------- currency --

// inCurrency drops the prices a visitor is not shopping in.
//
// The SQL already refuses a product with no price in the requested currency; this removes the
// *other* prices from what is left. Both halves are needed: filtering only in Go would still
// transfer a USD amount to a EUR visitor's browser, and filtering only in SQL would list a card
// with the wrong currency under `from`.
//
// The original slice is not modified: these products came from one query and may be shared with
// a caller that asked for every currency.
func inCurrency(products []Product, currency string) []Product {
	currency = strings.ToUpper(strings.TrimSpace(currency))
	if currency == "" {
		return products
	}
	out := make([]Product, 0, len(products))
	for _, p := range products {
		kept := make([]Price, 0, len(p.Prices))
		for _, price := range p.Prices {
			if strings.EqualFold(price.Currency, currency) {
				kept = append(kept, price)
			}
		}
		p.Prices = kept
		out = append(out, p)
	}
	return out
}

// LowStockThreshold is the default number below which "only N left" is shown. It is a default
// rather than a constant because an operator selling three units a week and one selling thirty a
// day mean different things by "low"; the setting that overrides it lives in internal/settings.
const LowStockThreshold = 5
