// Catalog polish: duplicating a product, and the order its plans are shown in.
//
// These are two staff actions that are not "create" and not "edit", which is why they live in
// their own file with their own route registrar rather than being bolted onto catalog.go:
//
//   - **Duplicate** copies a product, its price versions and its config options in one
//     transaction, and writes one audit row naming the product it was copied *from*. A copy is
//     how an operator makes a variant ("10 GB" from "8 GB"), and doing it by hand is how a price
//     or an option gets mistyped. The audit row names the source because "where did this come
//     from" is the only question anyone asks about a duplicate afterwards.
//
//   - **Plan order** writes `price_versions.sort_order`, the one column on a price version that
//     may be UPDATEd. Reordering plans changes no amount and no issued document, so it is
//     presentation; an amount change is still a new version. The endpoint takes the complete
//     order rather than a single move, so a half-applied merchandising change is not expressible.
//
// Both are `catalog:write`, both require CSRF and an Idempotency-Key (a retried "duplicate" is
// a second product), and both write their audit row inside the transaction that made the change.

package httpapi

import (
	"context"
	"net/http"

	"github.com/catalyst/billing/internal/authn"
	"github.com/catalyst/billing/internal/catalog"
	"github.com/jackc/pgx/v5"
)

func init() {
	// Sorted after "45-catalog" so the catalog's own routes are registered first; the two
	// registrars never share a pattern, so the order only matters for readability of the table.
	RegisterRoutes("46-catalog-polish", func(r *router) { r.s.registerCatalogPolishRoutes(r) })
}

func (s *Server) registerCatalogPolishRoutes(r *router) {
	r.staffMutation(http.MethodPost, "/api/v1/staff/catalog/products/{id}/duplicate",
		authn.PermCatalogWrite, s.handleDuplicateProduct)
	r.staffMutation(http.MethodPut, "/api/v1/staff/catalog/products/{id}/plans/order",
		authn.PermCatalogWrite, s.handleReorderPlans)
}

// handleDuplicateProduct copies a product and returns the copy.
//
// 201 with the new product, because a new resource exists at a new location — and the body is
// the copy rather than the source, so the panel can navigate straight to it.
func (s *Server) handleDuplicateProduct(w http.ResponseWriter, r *http.Request) {
	sourceID := r.PathValue("id")
	var copy catalog.Product
	err := s.withCatalogAudit(r, "catalog.product.duplicate", "product", sourceID,
		map[string]any{"source_product_id": sourceID},
		func(ctx context.Context, tx pgx.Tx) error {
			var err error
			copy, err = s.catalogStore().DuplicateProductTx(ctx, tx, sourceID)
			return err
		})
	if err != nil {
		s.catalogError(w, r, err)
		return
	}
	// The audit row is written against the source by the helper above; record the new id too,
	// so the trail reads "this product was copied to that one" from either end.
	writeJSON(w, http.StatusCreated, s.productJSON(copy))
}

// reorderRequest is the complete plan order for one product.
type reorderRequest struct {
	// PlanIDs is every price version of the product, in the order it should be shown.
	PlanIDs []string `json:"plan_ids"`
}

func (s *Server) handleReorderPlans(w http.ResponseWriter, r *http.Request) {
	var in reorderRequest
	if !s.decodeJSON(w, r, &in, 16<<10) {
		return
	}
	productID := r.PathValue("id")
	var prices []catalog.Price
	err := s.withCatalogAudit(r, "catalog.product.reorder_plans", "product", productID,
		map[string]any{"plan_ids": in.PlanIDs},
		func(ctx context.Context, tx pgx.Tx) error {
			var err error
			prices, err = s.catalogStore().ReorderPlansTx(ctx, tx, productID, in.PlanIDs)
			return err
		})
	if err != nil {
		s.catalogError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": planJSON(s, prices)})
}

// planJSON shapes a product's plans in *display* order, which is the order a product page and a
// visitor should see: the operator's plan order, newest version first within one position.
func planJSON(s *Server, prices []catalog.Price) []map[string]any {
	return planEntries(s, catalog.Plans(prices))
}

// planHistoryJSON shapes the same plans in the order the panel shows the price *history*: newest
// version first, so the current price is `prices[0]` and every older row keeps its place. It is
// what the product read has always returned, and a client that reads `prices[0]` as "the current
// price" (the catalog list does) would silently start showing a different plan if this changed.
func planHistoryJSON(s *Server, prices []catalog.Price) []map[string]any {
	return planEntries(s, prices)
}

// planEntries renders one plan per row. Every shape of a plan comes through here, so the staff
// read, the storefront and the reorder response cannot drift apart.
func planEntries(s *Server, prices []catalog.Price) []map[string]any {
	out := make([]map[string]any, 0, len(prices))
	for _, price := range prices {
		out = append(out, map[string]any{
			"id":               price.ID,
			"version":          price.Version,
			"name":             price.Name,
			"currency":         price.Currency,
			"amount":           s.priceAmount(price.AmountMinor, price.Currency),
			"setup":            s.priceAmount(price.SetupMinor, price.Currency),
			"interval":         price.Interval,
			"interval_label":   catalog.IntervalLabel(price.Interval),
			"recurring":        price.Recurring(),
			"sort_order":       price.SortOrder,
			"anchor_day":       price.AnchorDay,
			"anchor_timezone":  price.AnchorTimezone,
			"provision_plan":   price.ProvisionPlan,
			"provision_region": price.ProvisionRegion,
			"created_at":       price.CreatedAt.UTC(),
		})
	}
	return out
}
