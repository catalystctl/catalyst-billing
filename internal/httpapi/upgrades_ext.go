package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/catalyst/billing/internal/audit"
	"github.com/catalyst/billing/internal/authn"
	"github.com/catalyst/billing/internal/money"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// The product upgrade graph (CAT-09).
//
// `product_upgrades` is one directed table, and a downgrade is the same edge read backwards,
// so the graph is exactly "which product may a customer on this one move to". The storefront
// promises it, `internal/service` enforces it (`reachable`), and until now nobody could see
// or edit it except through psql.
//
// Two properties of the edit surface are deliberate:
//
//   - A self-edge is refused by the database (`product_upgrades_not_self`). This file checks
//     the same rule first so the operator gets a sentence, and it still maps the constraint
//     violation if the pre-check is ever bypassed — a 500 is not an answer to "you drew a loop".
//   - An edge between two products with no shared currency and interval is *accepted and
//     flagged*. `UpgradeOptions` only offers a target price in the subscription's own currency
//     and interval (there is no exchange rate in this system), so such an edge is invisible to
//     every customer. Refusing it would stop an operator staging the graph before the prices
//     exist; saying nothing would let them believe it works. So the response carries
//     `compatible` and a warning the screen renders.
//
// The proration arithmetic is not duplicated here. It lives in internal/service
// (`ComputeProration`, `ProratedDifference`) and the screen explains it in words; this file
// only supplies the two prices an operator needs to read that explanation.

func init() {
	RegisterRoutes("62-upgrade-graph", registerUpgradeGraph)
}

func registerUpgradeGraph(r *router) {
	// The graph is catalog configuration, so it is gated on catalog:write in both directions:
	// an operator who cannot change it has no use for it, and the read exists to serve the edit.
	r.staffGet("/api/v1/staff/upgrade-graph", authn.PermCatalogWrite, r.s.handleUpgradeGraph)
	r.staffMutation(http.MethodPost, "/api/v1/staff/upgrade-graph",
		authn.PermCatalogWrite, r.s.handleAddUpgradeEdge)
	r.staffMutation(http.MethodDelete,
		"/api/v1/staff/upgrade-graph/{product_id}/{upgrade_to_id}",
		authn.PermCatalogWrite, r.s.handleRemoveUpgradeEdge)
}

// graphPrice is one current price of a product: the newest version per (currency, interval).
type graphPrice struct {
	PriceVersionID string
	Version        int
	Name           string
	Currency       string
	AmountMinor    int64
	SetupMinor     int64
	Interval       string
}

// graphProduct is a node in the graph.
type graphProduct struct {
	ID     string
	Name   string
	Slug   string
	Hidden bool
	Prices []graphPrice
}

func graphPriceJSON(p graphPrice) map[string]any {
	return map[string]any{
		"price_version_id": p.PriceVersionID,
		"version":          p.Version,
		"name":             p.Name,
		"currency":         p.Currency,
		"amount_minor":     p.AmountMinor,
		"setup_minor":      p.SetupMinor,
		"interval":         p.Interval,
		// The exact money shape beside the raw minor units: the screen shows the amount with
		// the currency's own scale and grouping, and never has to guess a scale from a code.
		"amount": amountJSON(money.New(p.AmountMinor, mustCurrency(p.Currency))),
	}
}

func graphProductJSON(p graphProduct) map[string]any {
	prices := make([]map[string]any, 0, len(p.Prices))
	for _, price := range p.Prices {
		prices = append(prices, graphPriceJSON(price))
	}
	return map[string]any{
		"id":     p.ID,
		"name":   p.Name,
		"slug":   p.Slug,
		"hidden": p.Hidden,
		"prices": prices,
	}
}

// compatibleCurrencies is the set of "CURRENCY/interval" pairs both products price in.
//
// It is the exact condition `UpgradeOptions` uses, so a non-empty result means a customer on
// the first product will actually be offered the second. Empty means the edge is a promise the
// storefront cannot keep yet.
func compatibleCurrencies(from, to graphProduct) []string {
	have := map[string]struct{}{}
	for _, p := range to.Prices {
		have[p.Currency+"/"+p.Interval] = struct{}{}
	}
	seen := map[string]struct{}{}
	out := []string{}
	for _, p := range from.Prices {
		key := p.Currency + "/" + p.Interval
		if _, ok := have[key]; !ok {
			continue
		}
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, key)
	}
	return out
}

// loadGraphProducts reads every product with its current prices.
func (s *Server) loadGraphProducts(ctx context.Context) ([]graphProduct, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, name, slug, hidden FROM products ORDER BY sort_order, name`)
	if err != nil {
		return nil, fmt.Errorf("upgrade graph: list products: %w", err)
	}
	index := map[string]int{}
	out := []graphProduct{}
	for rows.Next() {
		var p graphProduct
		if err := rows.Scan(&p.ID, &p.Name, &p.Slug, &p.Hidden); err != nil {
			rows.Close()
			return nil, fmt.Errorf("upgrade graph: scan product: %w", err)
		}
		index[p.ID] = len(out)
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("upgrade graph: list products: %w", err)
	}
	rows.Close()

	// The newest version of each (product, currency, interval) is the price a customer would
	// be offered today; older versions stay in the catalog for the invoices that point at them.
	priceRows, err := s.pool.Query(ctx, `
		SELECT DISTINCT ON (product_id, currency, interval)
		       product_id, id, version, name, currency, amount_minor, setup_minor, interval
		FROM price_versions
		ORDER BY product_id, currency, interval, version DESC`)
	if err != nil {
		return nil, fmt.Errorf("upgrade graph: list prices: %w", err)
	}
	defer priceRows.Close()
	for priceRows.Next() {
		var (
			productID string
			p         graphPrice
		)
		if err := priceRows.Scan(&productID, &p.PriceVersionID, &p.Version, &p.Name,
			&p.Currency, &p.AmountMinor, &p.SetupMinor, &p.Interval); err != nil {
			return nil, fmt.Errorf("upgrade graph: scan price: %w", err)
		}
		if idx, ok := index[productID]; ok {
			out[idx].Prices = append(out[idx].Prices, p)
		}
	}
	return out, priceRows.Err()
}

// handleUpgradeGraph lists every edge with both endpoints and their prices.
func (s *Server) handleUpgradeGraph(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "database_not_configured",
			"no database is configured")
		return
	}

	products, err := s.loadGraphProducts(r.Context())
	if err != nil {
		s.graphError(w, r, "list products", err)
		return
	}
	byID := map[string]graphProduct{}
	for _, p := range products {
		byID[p.ID] = p
	}

	rows, err := s.pool.Query(r.Context(), `
		SELECT product_id, upgrade_to_id, created_at
		FROM product_upgrades
		ORDER BY product_id, upgrade_to_id`)
	if err != nil {
		s.graphError(w, r, "list edges", err)
		return
	}
	defer rows.Close()

	edges := []map[string]any{}
	for rows.Next() {
		var (
			fromID, toID string
			createdAt    any
		)
		if err := rows.Scan(&fromID, &toID, &createdAt); err != nil {
			s.graphError(w, r, "scan edge", err)
			return
		}
		from, fromOK := byID[fromID]
		to, toOK := byID[toID]
		if !fromOK || !toOK {
			// A cascade should make this impossible; reporting the ids is better than
			// dropping the row silently.
			edges = append(edges, map[string]any{
				"product_id": fromID, "upgrade_to_id": toID, "created_at": createdAt,
				"compatible": false, "compatible_prices": []string{},
				"warning": "one end of this edge is missing from the catalog",
			})
			continue
		}
		compatible := compatibleCurrencies(from, to)
		edge := map[string]any{
			"product_id":        from.ID,
			"product_name":      from.Name,
			"product_slug":      from.Slug,
			"product_hidden":    from.Hidden,
			"upgrade_to_id":     to.ID,
			"upgrade_to_name":   to.Name,
			"upgrade_to_slug":   to.Slug,
			"upgrade_to_hidden": to.Hidden,
			"created_at":        createdAt,
			"compatible":        len(compatible) > 0,
			"compatible_prices": compatible,
			"from_prices":       graphPricesJSON(from),
			"to_prices":         graphPricesJSON(to),
		}
		if len(compatible) == 0 {
			edge["warning"] = "the two products share no currency and billing interval, so " +
				"this edge is invisible to every customer until they do"
		}
		edges = append(edges, edge)
	}
	if err := rows.Err(); err != nil {
		s.graphError(w, r, "list edges", err)
		return
	}

	productsOut := make([]map[string]any, 0, len(products))
	for _, p := range products {
		productsOut = append(productsOut, graphProductJSON(p))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"edges":    edges,
		"products": productsOut,
	})
}

func graphPricesJSON(p graphProduct) []map[string]any {
	out := make([]map[string]any, 0, len(p.Prices))
	for _, price := range p.Prices {
		out = append(out, graphPriceJSON(price))
	}
	return out
}

// upgradeEdgeRequest is the body of an edge.
type upgradeEdgeRequest struct {
	ProductID   string `json:"product_id"`
	UpgradeToID string `json:"upgrade_to_id"`
}

// handleAddUpgradeEdge adds one edge to the graph.
func (s *Server) handleAddUpgradeEdge(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "database_not_configured",
			"no database is configured")
		return
	}

	var req upgradeEdgeRequest
	if !s.decodeJSON(w, r, &req, 8<<10) {
		return
	}
	from := strings.TrimSpace(req.ProductID)
	to := strings.TrimSpace(req.UpgradeToID)
	if !validUUID(from) || !validUUID(to) {
		writeProblem(w, http.StatusBadRequest, "invalid_product",
			"product_id and upgrade_to_id must both be product ids")
		return
	}
	if from == to {
		// The database refuses this (product_upgrades_not_self), and so does this handler:
		// the constraint message is a 500 nobody can act on, and the rule deserves a sentence.
		writeProblem(w, http.StatusBadRequest, "self_edge",
			"a product cannot be its own upgrade target: the edge would say a customer can "+
				"move from a product to itself")
		return
	}

	var (
		edge       map[string]any
		compatible []string
	)
	err := s.pool.InTx(r.Context(), func(ctx context.Context, tx pgx.Tx) error {
		// Lock both endpoints so a concurrent delete cannot leave an edge pointing at a
		// product that is gone between the check and the insert.
		var fromName, fromSlug, toName, toSlug string
		var fromHidden, toHidden bool
		if err := tx.QueryRow(ctx, `
			SELECT name, slug, hidden FROM products WHERE id = $1 FOR UPDATE`, from).
			Scan(&fromName, &fromSlug, &fromHidden); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return errGraphNoProduct
			}
			return fmt.Errorf("upgrade graph: lock product: %w", err)
		}
		if err := tx.QueryRow(ctx, `
			SELECT name, slug, hidden FROM products WHERE id = $1 FOR UPDATE`, to).
			Scan(&toName, &toSlug, &toHidden); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return errGraphNoProduct
			}
			return fmt.Errorf("upgrade graph: lock product: %w", err)
		}

		tag, err := tx.Exec(ctx, `
			INSERT INTO product_upgrades (product_id, upgrade_to_id) VALUES ($1, $2)
			ON CONFLICT (product_id, upgrade_to_id) DO NOTHING`, from, to)
		if err != nil {
			// The CHECK constraint, if the pre-check above is ever bypassed or the rule
			// changes shape. Mapping it keeps the answer a sentence instead of a 500.
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23514" {
				return errGraphSelfEdge
			}
			return fmt.Errorf("upgrade graph: add edge: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return errGraphEdgeExists
		}

		fromPrices, toPrices, err := loadGraphPrices(ctx, tx, from, to)
		if err != nil {
			return err
		}
		compatible = compatibleCurrencies(
			graphProduct{ID: from, Prices: fromPrices},
			graphProduct{ID: to, Prices: toPrices})
		edge = map[string]any{
			"product_id":        from,
			"product_name":      fromName,
			"product_slug":      fromSlug,
			"product_hidden":    fromHidden,
			"upgrade_to_id":     to,
			"upgrade_to_name":   toName,
			"upgrade_to_slug":   toSlug,
			"upgrade_to_hidden": toHidden,
			"compatible":        len(compatible) > 0,
			"compatible_prices": compatible,
			"from_prices":       graphPricesJSON(graphProduct{Prices: fromPrices}),
			"to_prices":         graphPricesJSON(graphProduct{Prices: toPrices}),
		}
		if len(compatible) == 0 {
			edge["warning"] = "the two products share no currency and billing interval, so " +
				"this edge is invisible to every customer until they do"
		}

		return audit.Record(ctx, tx, s.auditEvent(r, "catalog.upgrade_edge_added",
			"product", from, audit.OutcomeSuccess).WithChanges(map[string]any{
			"product_id":        from,
			"upgrade_to_id":     to,
			"compatible_prices": compatible,
		}))
	})
	switch {
	case errors.Is(err, errGraphNoProduct):
		writeProblem(w, http.StatusNotFound, "not_found", "no such product")
		return
	case errors.Is(err, errGraphSelfEdge):
		writeProblem(w, http.StatusBadRequest, "self_edge",
			"a product cannot be its own upgrade target")
		return
	case errors.Is(err, errGraphEdgeExists):
		writeProblem(w, http.StatusConflict, "edge_exists",
			"this edge is already in the graph")
		return
	case err != nil:
		s.graphError(w, r, "add edge", err)
		return
	}

	writeJSON(w, http.StatusCreated, edge)
}

// handleRemoveUpgradeEdge removes one edge.
func (s *Server) handleRemoveUpgradeEdge(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "database_not_configured",
			"no database is configured")
		return
	}
	from := strings.TrimSpace(r.PathValue("product_id"))
	to := strings.TrimSpace(r.PathValue("upgrade_to_id"))
	if !validUUID(from) || !validUUID(to) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such edge")
		return
	}

	err := s.pool.InTx(r.Context(), func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			DELETE FROM product_upgrades WHERE product_id = $1 AND upgrade_to_id = $2`,
			from, to)
		if err != nil {
			return fmt.Errorf("upgrade graph: remove edge: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return errGraphNoEdge
		}
		return audit.Record(ctx, tx, s.auditEvent(r, "catalog.upgrade_edge_removed",
			"product", from, audit.OutcomeSuccess).WithChanges(map[string]any{
			"product_id":    from,
			"upgrade_to_id": to,
		}))
	})
	if errors.Is(err, errGraphNoEdge) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such edge")
		return
	}
	if err != nil {
		s.graphError(w, r, "remove edge", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// loadGraphPrices reads the current prices of two products inside a transaction.
func loadGraphPrices(ctx context.Context, tx pgx.Tx, productIDs ...string) ([]graphPrice, []graphPrice, error) {
	rows, err := tx.Query(ctx, `
		SELECT DISTINCT ON (product_id, currency, interval)
		       product_id, id, version, name, currency, amount_minor, setup_minor, interval
		FROM price_versions
		WHERE product_id = ANY($1::uuid[])
		ORDER BY product_id, currency, interval, version DESC`, productIDs)
	if err != nil {
		return nil, nil, fmt.Errorf("upgrade graph: read prices: %w", err)
	}
	defer rows.Close()

	byProduct := map[string][]graphPrice{}
	for rows.Next() {
		var (
			productID string
			p         graphPrice
		)
		if err := rows.Scan(&productID, &p.PriceVersionID, &p.Version, &p.Name,
			&p.Currency, &p.AmountMinor, &p.SetupMinor, &p.Interval); err != nil {
			return nil, nil, fmt.Errorf("upgrade graph: scan price: %w", err)
		}
		byProduct[productID] = append(byProduct[productID], p)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("upgrade graph: read prices: %w", err)
	}
	return byProduct[productIDs[0]], byProduct[productIDs[1]], nil
}

// The three outcomes an edge write can have that are not a server fault. They are package
// errors rather than strings so the handler's switch cannot match the wrong one.
var (
	errGraphNoProduct  = errors.New("upgrade graph: no such product")
	errGraphSelfEdge   = errors.New("upgrade graph: a product cannot upgrade to itself")
	errGraphEdgeExists = errors.New("upgrade graph: the edge already exists")
	errGraphNoEdge     = errors.New("upgrade graph: no such edge")
)

// graphError logs and reports a failure without leaking its detail.
func (s *Server) graphError(w http.ResponseWriter, r *http.Request, action string, err error) {
	s.cfg.Logger.ErrorContext(r.Context(), "upgrade graph request failed",
		"action", action, "error", err)
	writeProblem(w, http.StatusInternalServerError, "internal_error",
		"the request could not be completed")
}
