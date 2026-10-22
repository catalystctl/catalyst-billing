// The catalogue's structure: the category tree and the product → server assignment.
//
// These routes are their own registrar because they are structural edits rather than catalogue
// CRUD. Two things make them different from `catalog.go`'s:
//
//   - **They are about shape, not content.** Moving a category changes where a whole branch sits
//     in the storefront, and assigning a server changes where the *next* purchase is
//     provisioned. Both are one field, and both are refused in ways that have to be explained
//     (a cycle, a branch that is already too deep, a server still in use).
//
//   - **They are additive.** A flat catalogue and a product with no server behave exactly as
//     they did before 0049, which is why the storefront read and the product shape are not
//     touched here: the new fields arrive on their own reads (`/categories/tree`,
//     `/products/{id}/placement`, `/catalog/categories`, `/catalog/category/{slug}`), so nothing
//     a client already reads can change shape underneath it.
//
// Every mutation is `catalog:write`, CSRF-protected and requires an Idempotency-Key (a retried
// "move this branch" must not move it twice), and every one writes its audit row inside the
// transaction that made the change.

package httpapi

import (
	"context"
	"errors"
	"net/http"

	"github.com/catalyst/billing/internal/authn"
	"github.com/catalyst/billing/internal/catalog"
	"github.com/jackc/pgx/v5"
)

func init() {
	// Sorted after "46-catalog-polish"; the registrars never share a pattern, so the order only
	// decides where the routes appear in the generated document.
	RegisterRoutes("47-catalog-nesting", func(r *router) { r.s.registerCatalogNestingRoutes(r) })
}

func (s *Server) registerCatalogNestingRoutes(r *router) {
	// Reads. The tree is a precondition for editing a category, so it is gated like the other
	// catalogue reads (invoice:read:all) rather than on catalog:write.
	r.staffGet("/api/v1/staff/catalog/categories/tree", authn.PermInvoiceReadAll, s.handleCategoryTree)
	r.staffGet("/api/v1/staff/catalog/products/{id}/placement", authn.PermInvoiceReadAll, s.handleProductPlacement)
	r.staffGet("/api/v1/staff/catalog/servers", authn.PermInvoiceReadAll, s.handleListServers)

	// Structural writes.
	r.staffMutation(http.MethodPut, "/api/v1/staff/catalog/categories/{id}/parent",
		authn.PermCatalogWrite, s.handleSetCategoryParent)
	r.staffMutation(http.MethodPut, "/api/v1/staff/catalog/products/{id}/server",
		authn.PermCatalogWrite, s.handleSetProductServer)

	r.staffMutation(http.MethodPost, "/api/v1/staff/catalog/servers", authn.PermCatalogWrite, s.handleCreateServer)
	r.staffMutation(http.MethodPut, "/api/v1/staff/catalog/servers/{id}", authn.PermCatalogWrite, s.handleUpdateServer)
	r.staffMutation(http.MethodDelete, "/api/v1/staff/catalog/servers/{id}", authn.PermCatalogWrite, s.handleDeleteServer)

	// Public: the storefront renders the tree, and a category page lists the branch below it.
	// Both are anonymous reads with the same short cache as the rest of the storefront.
	r.publicGet("/api/v1/catalog/categories", s.handlePublicCategoryTree)
	r.publicGet("/api/v1/catalog/category/{slug}", s.handlePublicCategory)
}

// ----------------------------------------------------------------- staff reads --

func (s *Server) handleCategoryTree(w http.ResponseWriter, r *http.Request) {
	nodes, err := s.catalogStore().CategoryTree(r.Context())
	if err != nil {
		s.staffError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": nodes, "max_depth": catalog.MaxCategoryDepth})
}

func (s *Server) handleProductPlacement(w http.ResponseWriter, r *http.Request) {
	placement, err := s.catalogStore().ProductPlacement(r.Context(), r.PathValue("id"))
	if err != nil {
		s.catalogStructureError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, placementJSON(placement))
}

func (s *Server) handleListServers(w http.ResponseWriter, r *http.Request) {
	servers, err := s.catalogStore().Servers(r.Context())
	if err != nil {
		s.staffError(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(servers))
	for _, srv := range servers {
		out = append(out, serverJSON(srv))
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out})
}

// --------------------------------------------------------------- staff writes --

// categoryParentRequest is the whole of a move: the parent it should hang from, or null for the
// root. There is deliberately no "position among siblings" field — that is `sort_order`, which
// the category editor already owns.
type categoryParentRequest struct {
	ParentID *string `json:"parent_id"`
}

func (s *Server) handleSetCategoryParent(w http.ResponseWriter, r *http.Request) {
	var in categoryParentRequest
	if !s.decodeJSON(w, r, &in, 16<<10) {
		return
	}
	id := r.PathValue("id")
	var nodes []catalog.CategoryNode
	err := s.withCatalogAudit(r, "catalog.category.move", "category", id,
		map[string]any{"parent_id": in.ParentID},
		func(ctx context.Context, tx pgx.Tx) error {
			var err error
			nodes, err = s.catalogStore().SetCategoryParentTx(ctx, tx, id, in.ParentID)
			return err
		})
	if err != nil {
		s.catalogStructureError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"data":      nodes,
		"max_depth": catalog.MaxCategoryDepth,
	})
}

// productServerRequest assigns a product to a provisioner target, or clears the assignment.
type productServerRequest struct {
	ServerID *string `json:"server_id"`
}

func (s *Server) handleSetProductServer(w http.ResponseWriter, r *http.Request) {
	var in productServerRequest
	if !s.decodeJSON(w, r, &in, 16<<10) {
		return
	}
	id := r.PathValue("id")
	var placement catalog.ProductPlacement
	err := s.withCatalogAudit(r, "catalog.product.set_server", "product", id,
		map[string]any{"server_id": in.ServerID},
		func(ctx context.Context, tx pgx.Tx) error {
			var err error
			placement, err = s.catalogStore().SetProductServerTx(ctx, tx, id, in.ServerID)
			return err
		})
	if err != nil {
		s.catalogStructureError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, placementJSON(placement))
}

func (s *Server) handleCreateServer(w http.ResponseWriter, r *http.Request) {
	var in catalog.Server
	if !s.decodeJSON(w, r, &in, 16<<10) {
		return
	}
	var created catalog.Server
	err := s.withCatalogAudit(r, "catalog.server.create", "server", "", nil,
		func(ctx context.Context, tx pgx.Tx) error {
			var err error
			created, err = s.catalogStore().CreateServerTx(ctx, tx, in)
			return err
		})
	if err != nil {
		s.catalogStructureError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, serverJSON(created))
}

func (s *Server) handleUpdateServer(w http.ResponseWriter, r *http.Request) {
	var in catalog.Server
	if !s.decodeJSON(w, r, &in, 16<<10) {
		return
	}
	id := r.PathValue("id")
	var updated catalog.Server
	err := s.withCatalogAudit(r, "catalog.server.update", "server", id, nil,
		func(ctx context.Context, tx pgx.Tx) error {
			var err error
			updated, err = s.catalogStore().UpdateServerTx(ctx, tx, id, in)
			return err
		})
	if err != nil {
		s.catalogStructureError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, serverJSON(updated))
}

func (s *Server) handleDeleteServer(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	err := s.withCatalogAudit(r, "catalog.server.delete", "server", id, nil,
		func(ctx context.Context, tx pgx.Tx) error {
			return s.catalogStore().DeleteServerTx(ctx, tx, id)
		})
	if err != nil {
		s.catalogStructureError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --------------------------------------------------------------------- public --

func (s *Server) handlePublicCategoryTree(w http.ResponseWriter, r *http.Request) {
	nodes, err := s.catalogStore().CategoryTree(r.Context())
	if err != nil {
		s.staffError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=60")
	writeJSON(w, http.StatusOK, map[string]any{
		"categories": publicCategoryTree(catalog.VisibleCategoryTree(nodes)),
	})
}

func (s *Server) handlePublicCategory(w http.ResponseWriter, r *http.Request) {
	node, products, err := s.catalogStore().PublicCategoryProducts(r.Context(), r.PathValue("slug"))
	if err != nil {
		s.catalogStructureError(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(products))
	for _, p := range products {
		out = append(out, s.publicProductJSON(p))
	}
	w.Header().Set("Cache-Control", "public, max-age=60")
	writeJSON(w, http.StatusOK, map[string]any{
		"category": publicCategoryNode(node),
		"products": out,
	})
}

// -------------------------------------------------------------------- shaping --

// publicCategoryTree renders visible nodes without the staff-only fields (hidden is always
// false here, and the counts a visitor has no use for are left out).
func publicCategoryTree(nodes []catalog.CategoryNode) []map[string]any {
	out := make([]map[string]any, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, publicCategoryNode(n))
	}
	return out
}

func publicCategoryNode(n catalog.CategoryNode) map[string]any {
	children := publicCategoryTree(n.Children)
	return map[string]any{
		"id":          n.ID,
		"slug":        n.Slug,
		"name":        n.Name,
		"description": n.Description,
		"sort_order":  n.SortOrder,
		"parent_id":   n.ParentID,
		"depth":       n.Depth,
		"path":        n.Path,
		// The branch total, which is what a filter click needs: a product in a child category
		// has to be findable from the parent.
		"product_count": n.SubtreeProductCount,
		"children":      children,
	}
}

// placementJSON is the staff shape of a product's structural placement.
func placementJSON(p catalog.ProductPlacement) map[string]any {
	var server any
	if p.Server != nil {
		server = serverJSON(*p.Server)
	}
	return map[string]any{
		"product_id":              p.ProductID,
		"server_id":               p.ServerID,
		"server":                  server,
		"category_id":             p.CategoryID,
		"category_parent_id":      p.CategoryParentID,
		"category_path":           p.CategoryPath,
		"category_ancestor_slugs": p.CategoryAncestorSlugs,
	}
}

// serverJSON is the staff shape of a provisioner target.
func serverJSON(srv catalog.Server) map[string]any {
	return map[string]any{
		"id":            srv.ID,
		"slug":          srv.Slug,
		"name":          srv.Name,
		"provider":      srv.Provider,
		"region":        srv.Region,
		"node":          srv.Node,
		"enabled":       srv.Enabled,
		"sort_order":    srv.SortOrder,
		"product_count": srv.ProductCount,
		"created_at":    srv.CreatedAt.UTC(),
		"updated_at":    srv.UpdatedAt.UTC(),
	}
}

// ---------------------------------------------------------------- error codes --

// catalogStructureError maps the two structural refusals to their own codes and hands
// everything else to the catalogue's mapping, so a client can tell "that would make a loop"
// from "that is one level too deep" without reading English.
//
// The codes are stable API: `category_cycle` and `category_too_deep`. A 409 for the cycle
// because the request conflicts with the shape the tree is already in; a 422 for the depth
// because the request is understood and well-formed and cannot be satisfied.
func (s *Server) catalogStructureError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, catalog.ErrCategoryCycle):
		writeProblem(w, http.StatusConflict, "category_cycle", err.Error())
	case errors.Is(err, catalog.ErrCategoryTooDeep):
		writeProblem(w, http.StatusUnprocessableEntity, "category_too_deep", err.Error())
	default:
		s.catalogError(w, r, err)
	}
}
