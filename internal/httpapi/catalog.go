// The catalog API: categories, products, logos, prices, and the public storefront read.
//
// Three audiences, three access levels, and the split is the security model:
//
//   - /api/v1/staff/catalog/** requires a permission (catalog:write to change, invoice:read:all
//     to read) and may see hidden products and drafts.
//   - /api/v1/catalog/** is public and unauthenticated, and returns only published products in
//     visible categories that have at least one price. The filtering happens in SQL, not here,
//     so no handler can accidentally widen it.
//   - /api/v1/media/{id} is public, because an <img> on the storefront has no session. It
//     serves stored bytes with a fixed content type, a sniffing refusal, and a sandbox for
//     SVG — the one image format that can carry a script.

package httpapi

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/catalyst/billing/internal/audit"
	"github.com/catalyst/billing/internal/authn"
	"github.com/catalyst/billing/internal/catalog"
	"github.com/catalyst/billing/internal/staff"
	"github.com/jackc/pgx/v5"
)

// catalogStore returns the catalog store, constructing it on first use.
func (s *Server) catalogStore() *catalog.Store {
	s.catalogMu.Lock()
	defer s.catalogMu.Unlock()
	if s.productCatalog == nil {
		s.productCatalog = catalog.New(s.pool.Pool)
	}
	return s.productCatalog
}

func init() {
	RegisterRoutes("45-catalog", func(r *router) { r.s.registerCatalogRoutes(r) })
}

func (s *Server) registerCatalogRoutes(r *router) {
	// Staff reads. Reading the catalog is a precondition for pricing an invoice, so it is
	// gated on invoice:read:all rather than catalog:write.
	r.staffGet("/api/v1/staff/catalog/categories", authn.PermInvoiceReadAll, s.handleListCategories)
	r.staffGet("/api/v1/staff/catalog/products", authn.PermInvoiceReadAll, s.handleListProducts)
	r.staffGet("/api/v1/staff/catalog/products/{id}", authn.PermInvoiceReadAll, s.handleGetProduct)

	// Staff writes. Every one is CSRF-protected and requires an Idempotency-Key, because a
	// double-submitted "create product" is a duplicate product.
	r.staffMutation(http.MethodPost, "/api/v1/staff/catalog/categories", authn.PermCatalogWrite, s.handleCreateCategory)
	r.staffMutation(http.MethodPut, "/api/v1/staff/catalog/categories/{id}", authn.PermCatalogWrite, s.handleUpdateCategory)
	r.staffMutation(http.MethodDelete, "/api/v1/staff/catalog/categories/{id}", authn.PermCatalogWrite, s.handleDeleteCategory)

	r.staffMutation(http.MethodPost, "/api/v1/staff/catalog/products", authn.PermCatalogWrite, s.handleCreateProduct)
	r.staffMutation(http.MethodPut, "/api/v1/staff/catalog/products/{id}", authn.PermCatalogWrite, s.handleUpdateProduct)
	r.staffMutation(http.MethodDelete, "/api/v1/staff/catalog/products/{id}", authn.PermCatalogWrite, s.handleDeleteProduct)
	r.staffMutation(http.MethodPost, "/api/v1/staff/catalog/products/{id}/prices", authn.PermCatalogWrite, s.handleAddPrice)

	// A logo upload is a staff mutation with a raw body, not JSON. It still needs CSRF and an
	// idempotency key; the dedupe by digest makes a retry harmless anyway.
	r.staffMutation(http.MethodPost, "/api/v1/staff/catalog/images", authn.PermCatalogWrite, s.handleUploadImage)

	// Public storefront.
	r.publicGet("/api/v1/catalog", s.handlePublicCatalog)
	r.publicGet("/api/v1/catalog/{slug}", s.handlePublicProduct)
	// Media is public: the storefront shows logos to visitors with no session.
	r.publicGet("/api/v1/media/{id}", s.handleMedia)
}

// ---------------------------------------------------------------- staff read --

func (s *Server) handleListCategories(w http.ResponseWriter, r *http.Request) {
	cats, err := s.catalogStore().Categories(r.Context())
	if err != nil {
		s.staffError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": cats})
}

func (s *Server) handleListProducts(w http.ResponseWriter, r *http.Request) {
	products, err := s.catalogStore().List(r.Context())
	if err != nil {
		s.staffError(w, r, err)
		return
	}
	// Shape every row the same way the detail endpoint does. Returning the raw struct here
	// meant `prices[].amount` was absent from the list and present in the detail, so a client
	// reading a price from a list rendered "—" without an error: the catalog screen's price
	// column and the order form both hit it.
	out := make([]map[string]any, 0, len(products))
	for _, p := range products {
		out = append(out, s.productJSON(p))
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out})
}

func (s *Server) handleGetProduct(w http.ResponseWriter, r *http.Request) {
	p, err := s.catalogStore().Product(r.Context(), r.PathValue("id"))
	if err != nil {
		s.catalogError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, s.productJSON(p))
}

// --------------------------------------------------------------- staff write --

func (s *Server) handleCreateCategory(w http.ResponseWriter, r *http.Request) {
	var in catalog.Category
	if !s.decodeJSON(w, r, &in, 64<<10) {
		return
	}
	var cat catalog.Category
	err := s.withCatalogAudit(r, "catalog.category.create", "category", "", nil,
		func(ctx context.Context, tx pgx.Tx) error {
			var err error
			cat, err = s.catalogStore().CreateCategoryTx(ctx, tx, in)
			return err
		})
	if err != nil {
		s.catalogError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, cat)
}

func (s *Server) handleUpdateCategory(w http.ResponseWriter, r *http.Request) {
	var in catalog.Category
	if !s.decodeJSON(w, r, &in, 64<<10) {
		return
	}
	id := r.PathValue("id")
	var cat catalog.Category
	err := s.withCatalogAudit(r, "catalog.category.update", "category", id, nil,
		func(ctx context.Context, tx pgx.Tx) error {
			var err error
			cat, err = s.catalogStore().UpdateCategoryTx(ctx, tx, id, in)
			return err
		})
	if err != nil {
		s.catalogError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, cat)
}

func (s *Server) handleDeleteCategory(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	err := s.withCatalogAudit(r, "catalog.category.delete", "category", id, nil,
		func(ctx context.Context, tx pgx.Tx) error {
			return s.catalogStore().DeleteCategoryTx(ctx, tx, id)
		})
	if err != nil {
		s.catalogError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleCreateProduct(w http.ResponseWriter, r *http.Request) {
	var in catalog.ProductInput
	if !s.decodeJSON(w, r, &in, 64<<10) {
		return
	}
	var p catalog.Product
	err := s.withCatalogAudit(r, "catalog.product.create", "product", "", nil,
		func(ctx context.Context, tx pgx.Tx) error {
			var err error
			p, err = s.catalogStore().CreateProductTx(ctx, tx, in)
			return err
		})
	if err != nil {
		s.catalogError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, s.productJSON(p))
}

func (s *Server) handleUpdateProduct(w http.ResponseWriter, r *http.Request) {
	var in catalog.ProductInput
	if !s.decodeJSON(w, r, &in, 64<<10) {
		return
	}
	id := r.PathValue("id")
	var p catalog.Product
	err := s.withCatalogAudit(r, "catalog.product.update", "product", id, nil,
		func(ctx context.Context, tx pgx.Tx) error {
			var err error
			p, err = s.catalogStore().UpdateProductTx(ctx, tx, id, in)
			return err
		})
	if err != nil {
		s.catalogError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, s.productJSON(p))
}

func (s *Server) handleDeleteProduct(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	err := s.withCatalogAudit(r, "catalog.product.delete", "product", id, nil,
		func(ctx context.Context, tx pgx.Tx) error {
			return s.catalogStore().DeleteProductTx(ctx, tx, id)
		})
	if err != nil {
		s.catalogError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleAddPrice(w http.ResponseWriter, r *http.Request) {
	var in catalog.Price
	if !s.decodeJSON(w, r, &in, 64<<10) {
		return
	}
	productID := r.PathValue("id")
	var price catalog.Price
	err := s.withCatalogAudit(r, "catalog.price.add", "product", productID, nil,
		func(ctx context.Context, tx pgx.Tx) error {
			var err error
			price, err = s.catalogStore().AddPriceTx(ctx, tx, productID, in)
			return err
		})
	if err != nil {
		s.catalogError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, price)
}

// handleUploadImage accepts raw image bytes. A multipart form would be the conventional choice,
// but a raw body with the content type in the header is one fewer parser in the request path
// and the panel has a File object in hand already.
func (s *Server) handleUploadImage(w http.ResponseWriter, r *http.Request) {
	contentType := strings.TrimSpace(strings.Split(r.Header.Get("Content-Type"), ";")[0])
	if contentType == "" {
		writeProblem(w, http.StatusBadRequest, "invalid_body", "a Content-Type is required")
		return
	}
	// One byte over the cap so the reader can tell "exactly at the limit" from "over it".
	body, err := io.ReadAll(io.LimitReader(r.Body, catalog.MaxImageBytes+1))
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_body", "the upload could not be read")
		return
	}
	if len(body) > catalog.MaxImageBytes {
		writeProblem(w, http.StatusRequestEntityTooLarge, "image_too_large",
			"the image exceeds 256 KiB")
		return
	}
	var img catalog.Image
	err = s.withCatalogAudit(r, "catalog.image.upload", "image", "", nil,
		func(ctx context.Context, tx pgx.Tx) error {
			var err error
			img, err = s.catalogStore().PutImageTx(ctx, tx, contentType, body)
			return err
		})
	if err != nil {
		s.catalogError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"id":           img.ID,
		"content_type": img.ContentType,
		"byte_size":    img.ByteSize,
		"url":          "/api/v1/media/" + img.ID,
	})
}

// ------------------------------------------------------------------- public --

func (s *Server) handlePublicCatalog(w http.ResponseWriter, r *http.Request) {
	products, err := s.catalogStore().Public(r.Context())
	if err != nil {
		s.staffError(w, r, err)
		return
	}
	cats, err := s.catalogStore().Categories(r.Context())
	if err != nil {
		s.staffError(w, r, err)
		return
	}
	visible := make([]catalog.Category, 0, len(cats))
	for _, c := range cats {
		if !c.Hidden {
			visible = append(visible, c)
		}
	}
	out := make([]map[string]any, 0, len(products))
	for _, p := range products {
		out = append(out, s.publicProductJSON(p))
	}
	// A short cache is deliberate: the storefront is anonymous, and a price change should be
	// visible within a minute without every visitor hitting the database.
	w.Header().Set("Cache-Control", "public, max-age=60")
	writeJSON(w, http.StatusOK, map[string]any{
		"categories": visible,
		"products":   out,
	})
}

func (s *Server) handlePublicProduct(w http.ResponseWriter, r *http.Request) {
	p, err := s.catalogStore().PublicProduct(r.Context(), r.PathValue("slug"))
	if err != nil {
		s.catalogError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=60")
	writeJSON(w, http.StatusOK, s.publicProductJSON(p))
}

// handleMedia serves a stored logo.
//
// The headers are the point of this handler:
//   - Content-Type is the stored type, never sniffed: `X-Content-Type-Options: nosniff` stops
//     the browser second-guessing it.
//   - The cache is immutable: the id names one exact byte sequence (deduped by digest), so the
//     bytes behind an id can never change.
//
// SVG is the one format that needs a decision. An SVG is a document, and a document can carry
// a script; served inline from our own origin it would run with our origin's privileges. But
// serving it as an attachment makes it useless as a logo, which is the whole point of the
// endpoint. The resolution is to render it as an image, not as a document: `sandbox` with no
// allowances plus `default-src 'none'` means that even if the file contains a script, the
// browser will not execute it — an <img> never executes scripts in the first place, and this
// header is the second line of defence for the case where someone opens the URL directly.
func (s *Server) handleMedia(w http.ResponseWriter, r *http.Request) {
	img, err := s.catalogStore().Image(r.Context(), r.PathValue("id"))
	if err != nil {
		if errors.Is(err, catalog.ErrNotFound) {
			writeProblem(w, http.StatusNotFound, "not_found", "no such image")
			return
		}
		s.staffError(w, r, err)
		return
	}
	h := w.Header()
	h.Set("Content-Type", img.ContentType)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cache-Control", "public, max-age=31536000, immutable")
	h.Set("Content-Length", strconv.Itoa(len(img.Bytes)))
	if img.ContentType == "image/svg+xml" {
		h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; sandbox")
	}
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(img.Bytes)
	}
}

// ---------------------------------------------------------------- shaping --

// productJSON is the staff shape: everything, including hidden state and cost-bearing fields.
func (s *Server) productJSON(p catalog.Product) map[string]any {
	// Version order, not plan order: the price table is a history and `prices[0]` is the current
	// price. Display order is `sort_order`, which every row carries.
	prices := planHistoryJSON(s, p.Prices)
	return map[string]any{
		"id":                 p.ID,
		"slug":               p.Slug,
		"name":               p.Name,
		"short_description":  p.ShortDescription,
		"description":        p.Description,
		"description_format": p.DescriptionFormat,
		// The per-product activation snippet. It is staff-only by construction: this shape is
		// the panel's, and publicProductJSON below deliberately omits it. An operator writes it
		// for someone who has already bought the product, not for a visitor deciding whether to.
		"email_template":       p.EmailTemplate,
		"category_id":          p.CategoryID,
		"category_name":        p.CategoryName,
		"category_slug":        p.CategorySlug,
		"image_id":             p.ImageID,
		"image_url":            p.ImageURL(),
		"hidden":               p.Hidden,
		"sort_order":           p.SortOrder,
		"active_subscriptions": p.ActiveSubscriptions,
		"created_at":           p.CreatedAt.UTC(),
		"updated_at":           p.UpdatedAt.UTC(),
		// `prices` is the shape this API documents. `price_versions` is kept as an alias
		// because the earlier read-only endpoint used that name and a client (and the test
		// suite) already depends on it. Removing the alias is a breaking change and needs a
		// deprecation, not a rename in passing.
		"prices":         prices,
		"price_versions": prices,
	}
}

// publicProductJSON is the visitor shape. It omits hidden state (always false here), the
// active-subscription count (business internals) and provision plan/region (infrastructure
// details that tell an attacker which node a customer lands on).
func (s *Server) publicProductJSON(p catalog.Product) map[string]any {
	// Display order, not history order: a visitor sees the plans in the order the operator
	// arranged them, and `sort_order` is the only thing that decides it.
	ordered := catalog.Plans(p.Prices)
	prices := make([]map[string]any, 0, len(ordered))
	for _, price := range ordered {
		prices = append(prices, map[string]any{
			"id":       price.ID,
			"version":  price.Version,
			"name":     price.Name,
			"currency": price.Currency,
			"amount":   s.priceAmount(price.AmountMinor, price.Currency),
			"setup":    s.priceAmount(price.SetupMinor, price.Currency),
			"interval": price.Interval,
			// A visitor needs to know whether a plan bills again, and asking them to
			// interpret the interval name is how a one-time product gets rendered as a
			// monthly one.
			"interval_label": catalog.IntervalLabel(price.Interval),
			"recurring":      price.Recurring(),
			"sort_order":     price.SortOrder,
		})
	}
	return map[string]any{
		"slug":               p.Slug,
		"name":               p.Name,
		"short_description":  p.ShortDescription,
		"description":        p.Description,
		"description_format": p.DescriptionFormat,
		"category_name":      p.CategoryName,
		"category_slug":      p.CategorySlug,
		"image_url":          p.ImageURL(),
		"prices":             prices,
	}
}

// withCatalogAudit runs a catalog change and its audit row in one transaction.
//
// The row is written in the same transaction as the change, so a catalog edit cannot commit
// without a record of who made it. That is the same rule the money paths follow, and it is
// why this is a transaction rather than a best-effort write afterwards: "who renamed this
// product" has to be answerable, and an audit write that can fail silently is not an audit.
func (s *Server) withCatalogAudit(r *http.Request, action, targetType, targetID string,
	changes map[string]any, fn func(ctx context.Context, tx pgx.Tx) error) error {
	return s.pool.InTx(r.Context(), func(ctx context.Context, tx pgx.Tx) error {
		if err := fn(ctx, tx); err != nil {
			return err
		}
		ev := s.auditEvent(r, action, targetType, targetID, audit.OutcomeSuccess).WithChanges(changes)
		return audit.Record(ctx, tx, ev)
	})
}

// priceAmount renders a minor amount for the API, falling back to a zero-decimal rendering when
// the stored currency code is not one the money package knows. A price row can only exist with
// a valid code (the table checks the shape, the API validates against the currency list), so the
// fallback is for a hand-inserted row rather than a normal path.
func (s *Server) priceAmount(minor int64, code string) map[string]any {
	amount, err := staff.Amounts(minor, code)
	if err != nil {
		return map[string]any{"minor": minor, "currency": code, "scale": 2, "display": formatMinor(minor, 2)}
	}
	return amountMap(amount)
}

// catalogError maps a domain error to a stable code. A client branches on the code, so the
// mapping lives in one place rather than in each handler.
func (s *Server) catalogError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, catalog.ErrNotFound):
		writeProblem(w, http.StatusNotFound, "not_found", "no such record")
	case errors.Is(err, catalog.ErrSlugTaken):
		writeProblem(w, http.StatusConflict, "slug_taken", "that slug is already in use")
	case errors.Is(err, catalog.ErrInUse):
		writeProblem(w, http.StatusConflict, "in_use", err.Error())
	case errors.Is(err, catalog.ErrImageTooLarge):
		writeProblem(w, http.StatusRequestEntityTooLarge, "image_too_large", err.Error())
	case errors.Is(err, catalog.ErrBadImageType):
		writeProblem(w, http.StatusUnsupportedMediaType, "unsupported_media_type", err.Error())
	case errors.Is(err, catalog.ErrInvalidInput):
		writeProblem(w, http.StatusUnprocessableEntity, "invalid_input", err.Error())
	default:
		s.staffError(w, r, err)
	}
}
