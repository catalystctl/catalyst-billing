// Package catalog owns the product catalog: categories, products, their images, and the
// immutable price versions an invoice prices from.
//
// It exists as its own package rather than another file in `staff` because the catalog is read
// by two audiences with different rules. Staff may see hidden products, drafts and costs. A
// visitor may see only what is published, and must never receive a hidden product, a hidden
// category, or a product that has no price. Keeping the two queries in one package makes that
// distinction reviewable in one place: `List` is the staff read, `Public` is the storefront
// read, and the SQL that separates them is the security boundary.
//
// The one rule that shapes everything else: a product's *metadata* is editable, and a product's
// *price* is not. Changing a price appends a `price_versions` row. An issued invoice records
// the version it priced from, so an UPDATE to a price would rewrite a document a customer
// already has. The API enforces this by having no update-price endpoint at all.
//
// Every write takes a `querier` rather than reaching for the pool, so the HTTP layer can run
// the change and its audit row in one transaction. `Store.X` is the pool-bound convenience;
// `Store.XTx` is what the handlers call.
package catalog

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Errors the HTTP layer maps to stable codes. They are values rather than strings so a
// handler cannot silently change the code a client branches on.
var (
	ErrNotFound      = errors.New("catalog: not found")
	ErrSlugTaken     = errors.New("catalog: slug already in use")
	ErrInvalidInput  = errors.New("catalog: invalid input")
	ErrImageTooLarge = errors.New("catalog: image too large")
	ErrBadImageType  = errors.New("catalog: unsupported image type")
	ErrInUse         = errors.New("catalog: still in use")
)

// MaxImageBytes caps an uploaded logo. 256 KiB is generous for a logo and small enough that
// the bytes can live in a row without turning the table into a blob store.
const MaxImageBytes = 256 << 10

// MaxEmailTemplateBytes caps a product's activation snippet.
//
// The snippet is operator prose that is rendered into the activation mail as plain text, so it
// is not markup and needs no escaping; what it needs is a bound. The column is `text`, and an
// unbounded `text` column a UI can write to is a blob store: a 200 KB "snippet" is not a
// message, it is an attachment. 4000 bytes is several paragraphs, which is more than any setup
// instruction needs, and it is the bound `0031_product_email_template.sql` enforces in the
// database too.
const MaxEmailTemplateBytes = 4000

// AllowedImageTypes is the closed set the endpoint accepts and the table enforces.
var AllowedImageTypes = map[string]bool{
	"image/png":     true,
	"image/jpeg":    true,
	"image/webp":    true,
	"image/svg+xml": true,
}

var slugPattern = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

// ValidateSlug rejects a slug a URL cannot carry. Checked here rather than only in the handler
// so the admin CLI and any future importer get the same rule.
func ValidateSlug(slug string) error {
	if len(slug) < 2 || len(slug) > 64 {
		return fmt.Errorf("%w: slug must be 2-64 characters", ErrInvalidInput)
	}
	if !slugPattern.MatchString(slug) {
		return fmt.Errorf("%w: slug must be lowercase letters, digits and single hyphens", ErrInvalidInput)
	}
	return nil
}

// querier is the subset of pgx both a pool and a transaction satisfy.
type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// Store is the catalog's data access.
type Store struct {
	pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// ------------------------------------------------------------------ types --

// Category groups products for the storefront.
type Category struct {
	ID           string    `json:"id"`
	Slug         string    `json:"slug"`
	Name         string    `json:"name"`
	Description  string    `json:"description"`
	SortOrder    int       `json:"sort_order"`
	Hidden       bool      `json:"hidden"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
	ProductCount int       `json:"product_count,omitempty"`
}

// Image is a stored logo.
type Image struct {
	ID          string
	ContentType string
	Bytes       []byte
	ByteSize    int
	Digest      string
}

// Price is one immutable price version.
type Price struct {
	ID          string `json:"id"`
	Version     int    `json:"version"`
	Name        string `json:"name"`
	Currency    string `json:"currency"`
	AmountMinor int64  `json:"amount_minor"`
	SetupMinor  int64  `json:"setup_minor"`
	Interval    string `json:"interval"`
	// SortOrder is where this plan sits in the product's plan list. It is the one column on
	// a price version that may be UPDATEd: an order is presentation, an amount is history.
	SortOrder       int       `json:"sort_order"`
	AnchorDay       int       `json:"anchor_day"`
	AnchorTimezone  string    `json:"anchor_timezone"`
	ProvisionPlan   string    `json:"provision_plan"`
	ProvisionRegion string    `json:"provision_region"`
	CreatedAt       time.Time `json:"created_at"`
}

// Recurring reports whether this price bills again. A one-time or free price does not, so
// nothing that renews may ever be built from it.
func (p Price) Recurring() bool { return Recurring(p.Interval) }

// Product is one catalog entry.
type Product struct {
	ID               string `json:"id"`
	Slug             string `json:"slug"`
	Name             string `json:"name"`
	ShortDescription string `json:"short_description"`
	Description      string `json:"description"`
	// DescriptionFormat says whether Description is `plain` text or sanitised `html`. The
	// renderer needs to know which, because escaping HTML shows the tags and parsing plain
	// text runs whatever was typed as markup.
	DescriptionFormat string `json:"description_format"`
	// EmailTemplate is the per-product activation snippet: what the customer is told about how
	// to use what they bought. It is rendered into the activation mail as
	// {{product_instructions}}, and an empty value means the shipped default sentence.
	EmailTemplate       string    `json:"email_template"`
	CategoryID          *string   `json:"category_id"`
	CategoryName        string    `json:"category_name,omitempty"`
	CategorySlug        string    `json:"category_slug,omitempty"`
	ImageID             *string   `json:"image_id"`
	Hidden              bool      `json:"hidden"`
	SortOrder           int       `json:"sort_order"`
	CreatedAt           time.Time `json:"created_at"`
	UpdatedAt           time.Time `json:"updated_at"`
	Prices              []Price   `json:"prices"`
	ActiveSubscriptions int       `json:"active_subscriptions"`
}

// ImageURL is the path the panel and the storefront put in an <img src>. It is derived, not
// stored: the URL is a function of the id, so there is nothing to keep in sync.
func (p Product) ImageURL() string {
	if p.ImageID == nil {
		return ""
	}
	return "/api/v1/media/" + *p.ImageID
}

// ProductInput is what the panel sends when creating or editing a product.
type ProductInput struct {
	Slug             string `json:"slug"`
	Name             string `json:"name"`
	ShortDescription string `json:"short_description"`
	Description      string `json:"description"`
	// DescriptionFormat is `plain` or `html`; empty means "unchanged if the text did not
	// change, otherwise plain", which is what the plain metadata form needs to be safe.
	DescriptionFormat string  `json:"description_format"`
	CategoryID        *string `json:"category_id"`
	ImageID           *string `json:"image_id"`
	Hidden            bool    `json:"hidden"`
	SortOrder         int     `json:"sort_order"`
	// EmailTemplate is the activation snippet, and it is part of the product's *metadata*: it is
	// editable here and deliberately absent from the storefront read, because it is written for
	// the person who already bought the product rather than for a visitor deciding whether to.
	//
	// It is a pointer because "not sent" and "set to empty" are different requests: the panel's
	// product form predates this field, and a PUT that omits it must not silently erase an
	// operator's instructions. An explicit "" clears it and falls back to the shipped default.
	EmailTemplate *string `json:"email_template"`
}

// validateEmailTemplate normalises and bounds a product's activation snippet.
//
// It trims the value and normalises CRLF so a snippet pasted from a Windows editor renders the
// same on every platform, and it refuses one that is over the bound rather than truncating it:
// a silently shortened instruction is worse than a rejected one, because the customer acts on
// half a sentence.
func validateEmailTemplate(s string) (string, error) {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\r\n", "\n"))
	if len(s) > MaxEmailTemplateBytes {
		return "", fmt.Errorf("%w: the activation snippet is limited to %d bytes",
			ErrInvalidInput, MaxEmailTemplateBytes)
	}
	return s, nil
}

// derefString reads an optional string, treating nil as the empty string.
func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// --------------------------------------------------------------- categories --

// CreateCategory inserts a category.
func (s *Store) CreateCategory(ctx context.Context, c Category) (Category, error) {
	return s.CreateCategoryTx(ctx, s.pool, c)
}

// CreateCategoryTx inserts a category inside a caller's transaction.
func (s *Store) CreateCategoryTx(ctx context.Context, q querier, c Category) (Category, error) {
	if err := ValidateSlug(c.Slug); err != nil {
		return Category{}, err
	}
	if strings.TrimSpace(c.Name) == "" {
		return Category{}, fmt.Errorf("%w: name is required", ErrInvalidInput)
	}
	var out Category
	err := q.QueryRow(ctx, `
		INSERT INTO categories (slug, name, description, sort_order, hidden)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id, slug, name, description, sort_order, hidden, created_at, updated_at`,
		c.Slug, strings.TrimSpace(c.Name), c.Description, c.SortOrder, c.Hidden,
	).Scan(&out.ID, &out.Slug, &out.Name, &out.Description, &out.SortOrder, &out.Hidden,
		&out.CreatedAt, &out.UpdatedAt)
	if err != nil {
		if isUniqueViolation(err) {
			return Category{}, ErrSlugTaken
		}
		return Category{}, fmt.Errorf("catalog: create category: %w", err)
	}
	return out, nil
}

// UpdateCategory edits a category. Slug is editable: a category is not referenced by an
// invoice, so changing its URL does not rewrite history.
func (s *Store) UpdateCategory(ctx context.Context, id string, c Category) (Category, error) {
	return s.UpdateCategoryTx(ctx, s.pool, id, c)
}

// UpdateCategoryTx edits a category inside a caller's transaction.
func (s *Store) UpdateCategoryTx(ctx context.Context, q querier, id string, c Category) (Category, error) {
	if err := ValidateSlug(c.Slug); err != nil {
		return Category{}, err
	}
	if strings.TrimSpace(c.Name) == "" {
		return Category{}, fmt.Errorf("%w: name is required", ErrInvalidInput)
	}
	var out Category
	err := q.QueryRow(ctx, `
		UPDATE categories
		   SET slug = $2, name = $3, description = $4, sort_order = $5, hidden = $6
		 WHERE id = $1
		RETURNING id, slug, name, description, sort_order, hidden, created_at, updated_at`,
		id, c.Slug, strings.TrimSpace(c.Name), c.Description, c.SortOrder, c.Hidden,
	).Scan(&out.ID, &out.Slug, &out.Name, &out.Description, &out.SortOrder, &out.Hidden,
		&out.CreatedAt, &out.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Category{}, ErrNotFound
	}
	if err != nil {
		if isUniqueViolation(err) {
			return Category{}, ErrSlugTaken
		}
		return Category{}, fmt.Errorf("catalog: update category: %w", err)
	}
	return out, nil
}

// DeleteCategory removes a category, refusing while products still reference it. ON DELETE SET
// NULL would silently ungroup products, which is a data change the operator did not ask for.
func (s *Store) DeleteCategory(ctx context.Context, id string) error {
	return s.DeleteCategoryTx(ctx, s.pool, id)
}

// DeleteCategoryTx removes a category inside a caller's transaction.
func (s *Store) DeleteCategoryTx(ctx context.Context, q querier, id string) error {
	var n int
	if err := q.QueryRow(ctx, `SELECT count(*) FROM products WHERE category_id = $1`, id).Scan(&n); err != nil {
		return fmt.Errorf("catalog: count products: %w", err)
	}
	if n > 0 {
		return fmt.Errorf("%w: %d product(s) still use it", ErrInUse, n)
	}
	tag, err := q.Exec(ctx, `DELETE FROM categories WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("catalog: delete category: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// Categories lists categories with a product count, ordered for display.
func (s *Store) Categories(ctx context.Context) ([]Category, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT c.id, c.slug, c.name, c.description, c.sort_order, c.hidden, c.created_at, c.updated_at,
		       (SELECT count(*) FROM products p WHERE p.category_id = c.id) AS product_count
		  FROM categories c
		 ORDER BY c.sort_order, c.name`)
	if err != nil {
		return nil, fmt.Errorf("catalog: list categories: %w", err)
	}
	defer rows.Close()
	out := make([]Category, 0, 8)
	for rows.Next() {
		var c Category
		if err := rows.Scan(&c.ID, &c.Slug, &c.Name, &c.Description, &c.SortOrder, &c.Hidden,
			&c.CreatedAt, &c.UpdatedAt, &c.ProductCount); err != nil {
			return nil, fmt.Errorf("catalog: scan category: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ------------------------------------------------------------------ images --

// PutImage stores bytes, collapsing identical uploads to one row by digest. That is what lets
// the media endpoint serve an immutable cache header: the id names one exact byte sequence.
func (s *Store) PutImage(ctx context.Context, contentType string, data []byte) (Image, error) {
	return s.PutImageTx(ctx, s.pool, contentType, data)
}

// PutImageTx stores bytes inside a caller's transaction.
func (s *Store) PutImageTx(ctx context.Context, q querier, contentType string, data []byte) (Image, error) {
	if !AllowedImageTypes[contentType] {
		return Image{}, fmt.Errorf("%w: %s", ErrBadImageType, contentType)
	}
	if len(data) == 0 {
		return Image{}, fmt.Errorf("%w: empty upload", ErrInvalidInput)
	}
	if len(data) > MaxImageBytes {
		return Image{}, fmt.Errorf("%w: %d bytes exceeds %d", ErrImageTooLarge, len(data), MaxImageBytes)
	}
	sum := sha256.Sum256(data)
	digest := hex.EncodeToString(sum[:])

	var img Image
	err := q.QueryRow(ctx, `
		INSERT INTO product_images (content_type, bytes, byte_size, digest)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (digest) DO UPDATE SET digest = EXCLUDED.digest
		RETURNING id, content_type, byte_size, digest`,
		contentType, data, len(data), digest,
	).Scan(&img.ID, &img.ContentType, &img.ByteSize, &img.Digest)
	if err != nil {
		return Image{}, fmt.Errorf("catalog: store image: %w", err)
	}
	img.Bytes = data
	return img, nil
}

// Image reads an image for the media endpoint.
func (s *Store) Image(ctx context.Context, id string) (Image, error) {
	var img Image
	err := s.pool.QueryRow(ctx, `
		SELECT id, content_type, bytes, byte_size, digest FROM product_images WHERE id = $1`, id,
	).Scan(&img.ID, &img.ContentType, &img.Bytes, &img.ByteSize, &img.Digest)
	if errors.Is(err, pgx.ErrNoRows) {
		return Image{}, ErrNotFound
	}
	if err != nil {
		return Image{}, fmt.Errorf("catalog: read image: %w", err)
	}
	return img, nil
}

// ---------------------------------------------------------------- products --

// CreateProduct inserts a product. It starts with no price: a price is a separate, immutable
// append, and a product with no price is simply not shown in the storefront.
func (s *Store) CreateProduct(ctx context.Context, in ProductInput) (Product, error) {
	return s.CreateProductTx(ctx, s.pool, in)
}

// CreateProductTx inserts a product inside a caller's transaction.
func (s *Store) CreateProductTx(ctx context.Context, q querier, in ProductInput) (Product, error) {
	if err := ValidateSlug(in.Slug); err != nil {
		return Product{}, err
	}
	if strings.TrimSpace(in.Name) == "" {
		return Product{}, fmt.Errorf("%w: name is required", ErrInvalidInput)
	}
	description, format, err := Description(in.Description, in.DescriptionFormat)
	if err != nil {
		return Product{}, err
	}
	template, err := validateEmailTemplate(derefString(in.EmailTemplate))
	if err != nil {
		return Product{}, err
	}
	var out Product
	err = q.QueryRow(ctx, `
		INSERT INTO products (slug, name, short_description, description, description_format,
		                     email_template, category_id, image_id, hidden, sort_order)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		RETURNING id, slug, name, short_description, description, description_format,
		          email_template, category_id, image_id, hidden, sort_order, created_at, updated_at`,
		in.Slug, strings.TrimSpace(in.Name), in.ShortDescription, description, format,
		template, in.CategoryID, in.ImageID, in.Hidden, in.SortOrder,
	).Scan(&out.ID, &out.Slug, &out.Name, &out.ShortDescription, &out.Description,
		&out.DescriptionFormat, &out.EmailTemplate, &out.CategoryID, &out.ImageID, &out.Hidden,
		&out.SortOrder, &out.CreatedAt, &out.UpdatedAt)
	if err != nil {
		if isUniqueViolation(err) {
			return Product{}, ErrSlugTaken
		}
		if isForeignKeyViolation(err) {
			return Product{}, fmt.Errorf("%w: category or image does not exist", ErrInvalidInput)
		}
		return Product{}, fmt.Errorf("catalog: create product: %w", err)
	}
	out.Prices = []Price{}
	return out, nil
}

// UpdateProduct edits metadata only. There is deliberately no price parameter: a price is
// appended with AddPrice, never updated.
func (s *Store) UpdateProduct(ctx context.Context, id string, in ProductInput) (Product, error) {
	return s.UpdateProductTx(ctx, s.pool, id, in)
}

// UpdateProductTx edits metadata inside a caller's transaction.
func (s *Store) UpdateProductTx(ctx context.Context, q querier, id string, in ProductInput) (Product, error) {
	if err := ValidateSlug(in.Slug); err != nil {
		return Product{}, err
	}
	if strings.TrimSpace(in.Name) == "" {
		return Product{}, fmt.Errorf("%w: name is required", ErrInvalidInput)
	}
	// A caller that does not say which format it wrote has to keep the stored one when it
	// re-sends the same text (the plain metadata form does exactly that, and it must not
	// silently downgrade a rich description to escaped markup). If it *changed* the text
	// without declaring a format, it wrote plain text, and saying so is the honest answer.
	format := in.DescriptionFormat
	if strings.TrimSpace(format) == "" {
		var storedText, storedFormat string
		err := q.QueryRow(ctx, `SELECT description, description_format FROM products WHERE id = $1`, id).
			Scan(&storedText, &storedFormat)
		if errors.Is(err, pgx.ErrNoRows) {
			return Product{}, ErrNotFound
		}
		if err != nil {
			return Product{}, fmt.Errorf("catalog: read product description: %w", err)
		}
		if storedText == in.Description {
			format = storedFormat
		}
	}
	description, format, err := Description(in.Description, format)
	if err != nil {
		return Product{}, err
	}
	// nil means "leave the snippet alone"; an explicit empty string clears it. The COALESCE in
	// the statement is what makes that true for the database as well as for Go.
	var template any
	if in.EmailTemplate != nil {
		v, err := validateEmailTemplate(*in.EmailTemplate)
		if err != nil {
			return Product{}, err
		}
		template = v
	}
	tag, err := q.Exec(ctx, `
		UPDATE products
		   SET slug = $2, name = $3, short_description = $4, description = $5,
		       description_format = $6, email_template = COALESCE($7, email_template),
		       category_id = $8, image_id = $9, hidden = $10, sort_order = $11
		 WHERE id = $1`,
		id, in.Slug, strings.TrimSpace(in.Name), in.ShortDescription, description, format,
		template, in.CategoryID, in.ImageID, in.Hidden, in.SortOrder)
	if err != nil {
		if isUniqueViolation(err) {
			return Product{}, ErrSlugTaken
		}
		if isForeignKeyViolation(err) {
			return Product{}, fmt.Errorf("%w: category or image does not exist", ErrInvalidInput)
		}
		return Product{}, fmt.Errorf("catalog: update product: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return Product{}, ErrNotFound
	}
	return s.productTx(ctx, q, id)
}

// DeleteProduct removes a product that nothing depends on. A product with services or order
// items is refused: deleting it would orphan a service or a paid order, and "stop selling" is
// what the hidden flag is for.
func (s *Store) DeleteProduct(ctx context.Context, id string) error {
	return s.DeleteProductTx(ctx, s.pool, id)
}

// DeleteProductTx removes a product inside a caller's transaction.
func (s *Store) DeleteProductTx(ctx context.Context, q querier, id string) error {
	var services, orders int
	if err := q.QueryRow(ctx, `SELECT count(*) FROM services WHERE product_id = $1`, id).Scan(&services); err != nil {
		return fmt.Errorf("catalog: count services: %w", err)
	}
	if services > 0 {
		return fmt.Errorf("%w: %d service(s) use it; hide it instead", ErrInUse, services)
	}
	if err := q.QueryRow(ctx, `
		SELECT count(*) FROM order_items oi
		  JOIN price_versions pv ON pv.id = oi.price_version_id
		 WHERE pv.product_id = $1`, id).Scan(&orders); err != nil {
		return fmt.Errorf("catalog: count orders: %w", err)
	}
	if orders > 0 {
		return fmt.Errorf("%w: %d order(s) reference it; hide it instead", ErrInUse, orders)
	}
	if _, err := q.Exec(ctx, `DELETE FROM price_versions WHERE product_id = $1`, id); err != nil {
		return fmt.Errorf("catalog: delete prices: %w", err)
	}
	tag, err := q.Exec(ctx, `DELETE FROM products WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("catalog: delete product: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// AddPrice appends a price version. The version number is computed inside the statement so two
// concurrent appends cannot both claim version N.
func (s *Store) AddPrice(ctx context.Context, productID string, p Price) (Price, error) {
	return s.AddPriceTx(ctx, s.pool, productID, p)
}

// AddPriceTx appends a price version inside a caller's transaction.
func (s *Store) AddPriceTx(ctx context.Context, q querier, productID string, p Price) (Price, error) {
	if strings.TrimSpace(p.Name) == "" {
		return Price{}, fmt.Errorf("%w: a price needs a name (\"8 GB\", \"Annual\")", ErrInvalidInput)
	}
	if p.Currency == "" {
		return Price{}, fmt.Errorf("%w: currency is required", ErrInvalidInput)
	}
	if p.AmountMinor < 0 || p.SetupMinor < 0 {
		return Price{}, fmt.Errorf("%w: amounts cannot be negative", ErrInvalidInput)
	}
	if err := ValidateInterval(p.Interval); err != nil {
		return Price{}, err
	}
	if p.Interval == IntervalFree && (p.AmountMinor != 0 || p.SetupMinor != 0) {
		// The database refuses this too; refusing here gives the operator a sentence instead
		// of a constraint violation.
		return Price{}, fmt.Errorf("%w: a free price cannot have an amount or a setup fee", ErrInvalidInput)
	}
	if p.AnchorDay < 1 || p.AnchorDay > 31 {
		return Price{}, fmt.Errorf("%w: anchor_day must be 1-31", ErrInvalidInput)
	}
	if strings.TrimSpace(p.AnchorTimezone) == "" {
		return Price{}, fmt.Errorf("%w: anchor_timezone is required", ErrInvalidInput)
	}

	// A new plan lands after the ones already on the product unless the caller says
	// otherwise. "Wherever max(version) put it" is not an order an operator chose.
	sortOrder := p.SortOrder
	if sortOrder == 0 {
		if err := q.QueryRow(ctx, `
			SELECT COALESCE(max(sort_order) + 1, 0) FROM price_versions WHERE product_id = $1`,
			productID).Scan(&sortOrder); err != nil {
			return Price{}, fmt.Errorf("catalog: next plan order: %w", err)
		}
	}

	var out Price
	err := q.QueryRow(ctx, `
		INSERT INTO price_versions (product_id, version, name, currency, amount_minor, setup_minor,
		                            interval, sort_order, anchor_day, anchor_timezone,
		                            provision_plan, provision_region)
		SELECT $1,
		       COALESCE((SELECT max(version) FROM price_versions WHERE product_id = $1), 0) + 1,
		       $2, $3, $4, $5, $6, $7, $8, $9, $10, $11
		RETURNING id, version, name, currency, amount_minor, setup_minor, interval, sort_order,
		          anchor_day, anchor_timezone, provision_plan, provision_region, created_at`,
		productID, strings.TrimSpace(p.Name), p.Currency, p.AmountMinor, p.SetupMinor,
		p.Interval, sortOrder, p.AnchorDay, p.AnchorTimezone, p.ProvisionPlan, p.ProvisionRegion,
	).Scan(&out.ID, &out.Version, &out.Name, &out.Currency, &out.AmountMinor, &out.SetupMinor,
		&out.Interval, &out.SortOrder, &out.AnchorDay, &out.AnchorTimezone, &out.ProvisionPlan,
		&out.ProvisionRegion, &out.CreatedAt)
	if err != nil {
		if isForeignKeyViolation(err) {
			return Price{}, ErrNotFound
		}
		return Price{}, fmt.Errorf("catalog: add price: %w", err)
	}
	return out, nil
}

// ------------------------------------------------------------- staff reads --

// List returns every product, hidden included, for the panel.
func (s *Store) List(ctx context.Context) ([]Product, error) {
	return s.query(ctx, `
		SELECT p.id, p.slug, p.name, p.short_description, p.description, p.description_format,
		       p.category_id, COALESCE(c.name, ''), COALESCE(c.slug, ''), p.image_id,
		       p.hidden, p.sort_order, p.created_at, p.updated_at,
		       (SELECT count(*) FROM subscriptions sub
		          JOIN price_versions pv ON pv.id = sub.price_version_id
		         WHERE pv.product_id = p.id) AS active_subscriptions,
		       p.email_template
		  FROM products p
		  LEFT JOIN categories c ON c.id = p.category_id
		 ORDER BY c.sort_order NULLS LAST, p.sort_order, p.name`)
}

// Public returns what a visitor may see: not hidden, in a visible category, and priced.
//
// The EXISTS on price_versions is load-bearing. A product with no price is not something anyone
// can buy, and showing it in a storefront produces a card whose button fails. Filtering in SQL
// rather than in the client also means the API cannot be asked for it.
func (s *Store) Public(ctx context.Context) ([]Product, error) {
	return s.query(ctx, `
		SELECT p.id, p.slug, p.name, p.short_description, p.description, p.description_format,
		       p.category_id, c.name, c.slug, p.image_id, p.hidden, p.sort_order,
		       p.created_at, p.updated_at, 0, p.email_template
		  FROM products p
		  JOIN categories c ON c.id = p.category_id AND NOT c.hidden
		 WHERE NOT p.hidden
		   AND EXISTS (SELECT 1 FROM price_versions pv WHERE pv.product_id = p.id)
		 ORDER BY c.sort_order, c.name, p.sort_order, p.name`)
}

// PublicProduct reads one visible product by slug, or ErrNotFound. The predicate is the same
// as Public's: a hidden product is indistinguishable from a missing one to a visitor.
func (s *Store) PublicProduct(ctx context.Context, slug string) (Product, error) {
	rows, err := s.query(ctx, `
		SELECT p.id, p.slug, p.name, p.short_description, p.description, p.description_format,
		       p.category_id, c.name, c.slug, p.image_id, p.hidden, p.sort_order,
		       p.created_at, p.updated_at, 0, p.email_template
		  FROM products p
		  JOIN categories c ON c.id = p.category_id AND NOT c.hidden
		 WHERE NOT p.hidden AND lower(p.slug) = lower($1)
		   AND EXISTS (SELECT 1 FROM price_versions pv WHERE pv.product_id = p.id)`, slug)
	if err != nil {
		return Product{}, err
	}
	if len(rows) == 0 {
		return Product{}, ErrNotFound
	}
	return rows[0], nil
}

// Product reads one product by id for the panel, hidden included.
func (s *Store) Product(ctx context.Context, id string) (Product, error) {
	return s.productTx(ctx, s.pool, id)
}

// productTx reads one product inside a caller's transaction, so an update can return the row it
// just wrote rather than racing a concurrent edit.
func (s *Store) productTx(ctx context.Context, q querier, id string) (Product, error) {
	rows, err := s.queryWith(ctx, q, `
		SELECT p.id, p.slug, p.name, p.short_description, p.description, p.description_format,
		       p.category_id, COALESCE(c.name, ''), COALESCE(c.slug, ''), p.image_id,
		       p.hidden, p.sort_order, p.created_at, p.updated_at,
		       (SELECT count(*) FROM subscriptions sub
		          JOIN price_versions pv ON pv.id = sub.price_version_id
		         WHERE pv.product_id = p.id) AS active_subscriptions,
		       p.email_template
		  FROM products p
		  LEFT JOIN categories c ON c.id = p.category_id
		 WHERE p.id = $1`, id)
	if err != nil {
		return Product{}, err
	}
	if len(rows) == 0 {
		return Product{}, ErrNotFound
	}
	return rows[0], nil
}

// query runs the product query on the pool and attaches prices.
func (s *Store) query(ctx context.Context, stmt string, args ...any) ([]Product, error) {
	return s.queryWith(ctx, s.pool, stmt, args...)
}

// queryWith runs the product query on a querier and attaches prices, so every caller gets the
// same shape.
func (s *Store) queryWith(ctx context.Context, q querier, stmt string, args ...any) ([]Product, error) {
	rows, err := q.Query(ctx, stmt, args...)
	if err != nil {
		return nil, fmt.Errorf("catalog: list products: %w", err)
	}
	defer rows.Close()
	out := make([]Product, 0, 16)
	for rows.Next() {
		var p Product
		if err := rows.Scan(&p.ID, &p.Slug, &p.Name, &p.ShortDescription, &p.Description,
			&p.DescriptionFormat, &p.CategoryID, &p.CategoryName, &p.CategorySlug, &p.ImageID,
			&p.Hidden, &p.SortOrder, &p.CreatedAt, &p.UpdatedAt, &p.ActiveSubscriptions,
			&p.EmailTemplate); err != nil {
			return nil, fmt.Errorf("catalog: scan product: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("catalog: read products: %w", err)
	}
	for i := range out {
		prices, err := s.pricesWith(ctx, q, out[i].ID)
		if err != nil {
			return nil, err
		}
		out[i].Prices = prices
	}
	return out, nil
}

// pricesWith reads every version, newest first: a card shows the current price and the panel
// shows the history. Display order is `Plans`, which sorts by the operator's plan order.
func (s *Store) pricesWith(ctx context.Context, q querier, productID string) ([]Price, error) {
	rows, err := q.Query(ctx, `
		SELECT id, version, name, currency, amount_minor, setup_minor, interval, sort_order,
		       anchor_day, anchor_timezone, provision_plan, provision_region, created_at
		  FROM price_versions
		 WHERE product_id = $1
		 ORDER BY version DESC`, productID)
	if err != nil {
		return nil, fmt.Errorf("catalog: list prices: %w", err)
	}
	defer rows.Close()
	out := make([]Price, 0, 4)
	for rows.Next() {
		var p Price
		if err := rows.Scan(&p.ID, &p.Version, &p.Name, &p.Currency, &p.AmountMinor, &p.SetupMinor,
			&p.Interval, &p.SortOrder, &p.AnchorDay, &p.AnchorTimezone, &p.ProvisionPlan,
			&p.ProvisionRegion, &p.CreatedAt); err != nil {
			return nil, fmt.Errorf("catalog: scan price: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ------------------------------------------------------------------ utils --

// sqlState exposes the SQLSTATE of a Postgres error. Matching on the code rather than the
// message keeps the mapping stable across Postgres versions and locales.
func sqlState(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

func isUniqueViolation(err error) bool     { return sqlState(err) == "23505" }
func isForeignKeyViolation(err error) bool { return sqlState(err) == "23503" }
