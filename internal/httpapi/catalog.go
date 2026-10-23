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
	// Merchandising: stock, the per-customer limit, the quantity policy and the featured flag.
	// It is its own route rather than fields on the product PUT, because that PUT replaces the
	// whole metadata record and its other callers (the description editor, the hide toggle) know
	// nothing about stock — a description save would restock a sold-out product.
	r.staffMutation(http.MethodPut, "/api/v1/staff/catalog/products/{id}/merchandising",
		authn.PermCatalogWrite, s.handleSetMerchandising)

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

// storefrontSettings is the handful of operator choices the storefront reads.
//
// A failed settings read falls back to the defaults rather than failing the shop: settings are
// configuration, and a storefront that refuses to render because one row is unreadable is a
// worse outcome than one that shows stock it was told to hide. The defaults are the behaviour
// this handler had before the keys existed, so a degraded read is invisible.
type storefrontSettings struct {
	// HomeCategories is "top_level" (Paymenter's rule) or "all".
	HomeCategories string
	// ShowStock says whether a remaining count is published at all.
	ShowStock bool
	// LowStockThreshold is the number under which "only N left" is worth saying.
	LowStockThreshold int
}

func (s *Server) storefrontSettings(ctx context.Context) storefrontSettings {
	out := storefrontSettings{
		HomeCategories:    "top_level",
		ShowStock:         true,
		LowStockThreshold: catalog.LowStockThreshold,
	}
	store := s.settingsStore()
	if v, err := store.String(ctx, "storefront", "home_categories"); err == nil && v != "" {
		out.HomeCategories = v
	}
	if v, err := store.Bool(ctx, "storefront", "show_stock"); err == nil {
		out.ShowStock = v
	}
	if v, err := store.Int(ctx, "storefront", "low_stock_threshold"); err == nil && v >= 0 {
		out.LowStockThreshold = int(v)
	}
	return out
}

// currencyParam reads the storefront's `?currency=`, upper-cased and bounded to the code shape a
// price row uses. A malformed value is ignored rather than refused: the storefront is anonymous
// and a bad query string must not be an error page.
func currencyParam(r *http.Request) string {
	code := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("currency")))
	if len(code) < 2 || len(code) > 12 {
		return ""
	}
	for _, c := range code {
		if !(c >= 'A' && c <= 'Z') && !(c >= '0' && c <= '9') {
			return ""
		}
	}
	return code
}

func (s *Server) handlePublicCatalog(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	// The switcher is only honest if it changes what is offered, so the currency travels into
	// the read: a product sold only in USD is not on the shelf of a EUR visitor, and a EUR
	// visitor is never quoted a converted amount.
	currency := currencyParam(r)
	products, err := s.catalogStore().PublicIn(ctx, currency)
	if err != nil {
		s.staffError(w, r, err)
		return
	}
	// The category strip is the tree, pruned to what a visitor can actually reach (SF-01). It
	// is one read rather than two: the branch totals the strip needs are the tree's own counts.
	nodes, err := s.catalogStore().CategoryTree(ctx)
	if err != nil {
		s.staffError(w, r, err)
		return
	}
	prefs := s.storefrontSettings(ctx)
	// The strip is built from the products the visitor was actually offered, so a branch whose
	// goods are all priced in another currency is not a tab that opens an empty page.
	strip := catalog.HomeCategories(nodes, products, prefs.HomeCategories)
	out, err := s.publicProductList(ctx, products, prefs)
	if err != nil {
		s.staffError(w, r, err)
		return
	}
	// A short cache is deliberate: the storefront is anonymous, and a price change should be
	// visible within a minute without every visitor hitting the database. It is keyed by the
	// query string by any shared cache, so a euro visitor cannot be served a dollar page.
	w.Header().Set("Cache-Control", "public, max-age=60")
	writeJSON(w, http.StatusOK, map[string]any{
		"currency":          currency,
		"currency_switcher": s.currencySwitcherEnabled(ctx),
		"categories":        publicCategoryTree(strip),
		"products":          out,
	})
}

// currencySwitcherEnabled reports whether the storefront offers the currency control. A failed
// read means "yes": an unreadable settings row must not remove a control the shop has.
func (s *Server) currencySwitcherEnabled(ctx context.Context) bool {
	enabled, err := s.settingsStore().Bool(ctx, "storefront", "currency_switcher")
	if err != nil {
		return true
	}
	return enabled
}

// handlePublicProduct resolves one storefront slug to *either* a product or a category.
//
// Paymenter's URLs are `/products/{category}/{product}`, and a category therefore has its own
// shareable, crawlable address. One route serving both is what makes `/products/{slug}` a real
// URL for a category without a second SPA route: the server decides which record the slug names,
// and the response says which (`kind`). A product wins a collision, because a visitor following
// a product link meant the product.
func (s *Server) handlePublicProduct(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	slug := r.PathValue("slug")
	currency := currencyParam(r)
	p, err := s.catalogStore().PublicProductIn(ctx, slug, currency)
	if err == nil {
		options, err := s.publicConfigOptions(ctx, p)
		if err != nil {
			s.staffError(w, r, err)
			return
		}
		w.Header().Set("Cache-Control", "public, max-age=60")
		body := s.publicProductJSON(p, s.storefrontSettings(ctx), options)
		body["kind"] = "product"
		writeJSON(w, http.StatusOK, body)
		return
	}
	if !errors.Is(err, catalog.ErrNotFound) {
		s.catalogError(w, r, err)
		return
	}

	node, products, catErr := s.catalogStore().PublicCategoryProductsIn(ctx, slug, currency)
	if catErr != nil {
		s.catalogStructureError(w, r, catErr)
		return
	}
	prefs := s.storefrontSettings(ctx)
	out, err := s.publicProductList(ctx, products, prefs)
	if err != nil {
		s.staffError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=60")
	writeJSON(w, http.StatusOK, map[string]any{
		"kind":              "category",
		"currency":          currency,
		"currency_switcher": s.currencySwitcherEnabled(ctx),
		"category":          publicCategoryNode(node),
		"products":          out,
	})
}

// publicProductList shapes a list of products for a visitor, reading their config options once
// each. Every storefront list goes through it, so the index, a product page and a category page
// cannot return different shapes for the same product.
func (s *Server) publicProductList(ctx context.Context, products []catalog.Product,
	prefs storefrontSettings) ([]map[string]any, error) {
	out := make([]map[string]any, 0, len(products))
	for _, p := range products {
		options, err := s.publicConfigOptions(ctx, p)
		if err != nil {
			return nil, err
		}
		out = append(out, s.publicProductJSON(p, prefs, options))
	}
	return out, nil
}

// publicConfigOptions reads a product's options, hiding the ones an operator marked hidden and
// keeping the parent link so the client can nest a dependent option under its parent. The
// amounts stay in the plan's own currency: an option is priced per price version, and this read
// does not convert anything.
func (s *Server) publicConfigOptions(ctx context.Context, p catalog.Product) ([]map[string]any, error) {
	options, err := s.catalogStore().ConfigOptions(ctx, p.ID)
	if err != nil {
		return nil, err
	}
	currencies := make(map[string]string, len(p.Prices))
	for _, price := range p.Prices {
		currencies[price.ID] = price.Currency
	}
	out := make([]map[string]any, 0, len(options))
	for _, o := range options {
		if o.Hidden {
			continue
		}
		prices := make([]map[string]any, 0, len(o.OptionPrices))
		for _, op := range o.OptionPrices {
			// A price version the visitor is not shopping in is not offered, so its option
			// amounts are not either.
			currency, visible := currencies[op.PriceVersionID]
			if !visible {
				continue
			}
			prices = append(prices, map[string]any{
				"price_id": op.PriceVersionID,
				"choice":   op.Choice,
				"amount":   s.priceAmount(op.AmountMinor, currency),
				"setup":    s.priceAmount(op.SetupMinor, currency),
			})
		}
		out = append(out, map[string]any{
			"id":          o.ID,
			"name":        o.Name,
			"description": o.Description,
			"type":        o.OptionType,
			"choices":     o.Choices,
			"required":    o.Required,
			"parent_id":   o.ParentID,
			"prices":      prices,
		})
	}
	return out, nil
}

// handleSetMerchandising writes the presentation fields of a product.
func (s *Server) handleSetMerchandising(w http.ResponseWriter, r *http.Request) {
	var in catalog.Merchandising
	if !s.decodeJSON(w, r, &in, 16<<10) {
		return
	}
	id := r.PathValue("id")
	var p catalog.Product
	err := s.withCatalogAudit(r, "catalog.product.merchandising", "product", id,
		map[string]any{
			"stock":          in.Stock,
			"per_user_limit": in.PerUserLimit,
			"allow_quantity": in.AllowQuantity,
			"featured":       in.Featured,
		},
		func(ctx context.Context, tx pgx.Tx) error {
			var err error
			p, err = s.catalogStore().SetMerchandisingTx(ctx, tx, id, in)
			return err
		})
	if err != nil {
		s.catalogError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, s.productJSON(p))
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
		"email_template": p.EmailTemplate,
		"category_id":    p.CategoryID,
		"category_name":  p.CategoryName,
		"category_slug":  p.CategorySlug,
		"image_id":       p.ImageID,
		"image_url":      p.ImageURL(),
		"hidden":         p.Hidden,
		"sort_order":     p.SortOrder,
		"featured":       p.Featured,
		// The merchandising fields. They are here because an operator has to be able to see,
		// and therefore edit, what the storefront is enforcing.
		"stock":          p.Stock,
		"per_user_limit": p.PerUserLimit,
		"allow_quantity": p.AllowQuantity,
		// Where the product is sold from. It is staff-only: publicProductJSON below omits it,
		// because a node name tells a visitor which host a customer lands on.
		"server_id":            p.ServerID,
		"server_name":          p.ServerName,
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
//
// What it adds is what a buyer needs in order to decide: whether the product is in stock, how
// many are left (when the operator publishes that), whether a quantity may be chosen, whether it
// is promoted, and the config options with the amounts for each plan. Without the last one a
// product with a required option renders a plan nobody can buy.
func (s *Server) publicProductJSON(p catalog.Product, prefs storefrontSettings,
	options []map[string]any) map[string]any {
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
			// monthly one. The label is the server's, so a new period is not a client
			// release (SF-14).
			"interval_label": catalog.IntervalLabel(price.Interval),
			"recurring":      price.Recurring(),
			"sort_order":     price.SortOrder,
		})
	}
	// The stock count is published only when the operator wants it published; `in_stock` is
	// always present, because refusing to sell a sold-out product is correctness rather than
	// display. A nil stock is unlimited, which is not zero and must not render as "sold out".
	var stock any
	if prefs.ShowStock {
		stock = p.Stock
	}
	return map[string]any{
		"slug":                p.Slug,
		"name":                p.Name,
		"short_description":   p.ShortDescription,
		"description":         p.Description,
		"description_format":  p.DescriptionFormat,
		"category_name":       p.CategoryName,
		"category_slug":       p.CategorySlug,
		"image_url":           p.ImageURL(),
		"prices":              prices,
		"featured":            p.Featured,
		"stock":               stock,
		"in_stock":            p.InStock(),
		"low_stock":           prefs.ShowStock && p.LowStock(prefs.LowStockThreshold),
		"low_stock_threshold": prefs.LowStockThreshold,
		"allow_quantity":      p.AllowQuantity,
		"config_options":      options,
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
