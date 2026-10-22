// Nested categories and the product → server assignment.
//
// This file is the structural half of the catalogue. Two things live here that the rest of the
// package deliberately does not do:
//
//   - **A tree.** `categories.parent_id` turns a flat list into a tree, and a tree needs a depth
//     bound, a cycle refusal and a read that returns the shape rather than the rows. The
//     invariants are enforced by the database (0049's `categories_guard_tree` trigger) because
//     two concurrent writes can each look safe and still form a cycle; the checks here exist so
//     an operator gets a sentence and a stable code instead of a SQLSTATE, never so the database
//     can be trusted less.
//
//   - **A provisioner target.** `products.server_id` says which node a product is sold onto. It
//     does not open a second routing path: the value becomes the *same* `region` that a price
//     version's `provision_region` already fills, which checkout copies into `services.config`
//     and the provisioning worker hands to the adapter as `CreateRequest.Region`. A plan that
//     names a region still wins, so an existing price keeps provisioning exactly where it did,
//     and a product with no server behaves precisely as it did before 0049.
//
// Everything here is additive. `CreateCategory`, `UpdateCategory`, `List` and the price paths are
// untouched, so a flat catalogue with no servers reads and writes exactly as it always has. The
// two storefront reads gained one predicate each — `hiddenBranchesCTE`, below — because hiding a
// category now has to hide what is under it, which a flat list never had to express.
package catalog

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// MaxCategoryDepth bounds the tree at four levels, root included. It is enforced by the
// database as well (0049, constraint `categories_depth_bound`), so a caller that forgets to
// check cannot store a fifth level; the constant exists so the API can say the number in a
// message and the panel can stop offering a control that cannot work.
const MaxCategoryDepth = 4

// Errors this file adds. They are values rather than strings for the same reason the others
// are: the HTTP layer maps them to codes a client branches on, and a handler must not be able
// to change the code by rewording a message.
var (
	// ErrCategoryCycle means the write would make a category its own ancestor.
	ErrCategoryCycle = errors.New("catalog: that move would make a category its own ancestor")
	// ErrCategoryTooDeep means the write would push a category past MaxCategoryDepth.
	ErrCategoryTooDeep = errors.New("catalog: category nesting is too deep")
)

// The trigger raises 23514 (check_violation) with a constraint name rather than a custom
// SQLSTATE, because a client that already understands the standard classes should keep
// understanding these; the name is what tells the two refusals apart.
const (
	categoryCycleConstraint = "categories_no_cycle"
	categoryDepthConstraint = "categories_depth_bound"
)

// CategoryNode is a category plus its place in the tree: the parent it hangs from, how deep it
// is (a root is 1), its URL path, and the number of products in the branch below it.
//
// It embeds Category rather than replacing it, so every field a caller already reads — slug,
// name, hidden, product_count — keeps its name and its meaning, and a client that ignores the
// tree fields sees exactly the flat shape it saw before.
type CategoryNode struct {
	Category
	ParentID *string `json:"parent_id"`
	// Depth is 1 for a root. The database refuses anything deeper than MaxCategoryDepth.
	Depth int `json:"depth"`
	// Path is the slash-joined slug chain ("game-servers/minecraft"), which is what a
	// breadcrumb and a category URL want without walking the tree again.
	Path string `json:"path"`
	// SubtreeProductCount counts the products in this category *and* in every category below
	// it. It is the number a storefront filter needs: clicking "Game servers" must show the
	// Minecraft products, and ProductCount (this category only) cannot answer that.
	SubtreeProductCount int            `json:"subtree_product_count"`
	Children            []CategoryNode `json:"children,omitempty"`
}

// Server is one provisioner target: a node on the provider the installation is configured with.
//
// It is not a second gateway configuration — that is the `servers.*` settings group, which says
// *which provider*. This says *which node on it*, and it reaches the adapter through the field
// the price version already fills (`CreateRequest.Region`), never through a route of its own.
type Server struct {
	ID string `json:"id"`
	// Slug is stable and human-readable; an operator reads it in a report.
	Slug string `json:"slug"`
	Name string `json:"name"`
	// Provider is the provisioning extension this target belongs to. Empty means "whichever
	// extension the installation is configured with", which is what a single-provider shop
	// wants and what every row this migration leaves behind says.
	Provider string `json:"provider"`
	// Region is what the adapter receives as CreateRequest.Region.
	Region string `json:"region"`
	// Node is the provider's own node/group label, passed beside the region in the metadata
	// map, because most panels address a target by node id rather than by region name.
	Node string `json:"node"`
	// Enabled false keeps the products already sold on this server and their services, and
	// stops it being offered for a new assignment. Hiding is not deleting.
	Enabled      bool      `json:"enabled"`
	SortOrder    int       `json:"sort_order"`
	ProductCount int       `json:"product_count"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// ProductPlacement is where a product sits: the server it is provisioned onto and its place in
// the category tree. It is a separate read rather than fields on Product because the product
// shape is read by the storefront as well, and a visitor must never be handed a node name.
type ProductPlacement struct {
	ProductID string  `json:"product_id"`
	ServerID  *string `json:"server_id"`
	// Server is the resolved target, or nil when the product has none. It is resolved here so
	// the panel can show "Hetzner / eu-west" without a second request.
	Server *Server `json:"server"`
	// CategoryID is the product's category (unchanged behaviour), and CategoryPath is where
	// that category sits in the tree, so the panel can show "Game servers › Minecraft".
	CategoryID            *string  `json:"category_id"`
	CategoryParentID      *string  `json:"category_parent_id"`
	CategoryPath          string   `json:"category_path"`
	CategoryAncestorSlugs []string `json:"category_ancestor_slugs"`
}

// ---------------------------------------------------------------- tree reads --

// CategoryTree returns every category, nested, in display order.
//
// It is one query plus an in-memory tree build rather than one query per level: the recursive
// part computes each row's depth, and the parent links are already in the rows. A category whose
// parent chain is broken (only possible through a hand-written DELETE) is still returned, at
// depth 1, because a category an operator cannot see is a category they cannot fix.
func (s *Store) CategoryTree(ctx context.Context) ([]CategoryNode, error) {
	return s.categoryTreeWith(ctx, s.pool)
}

func (s *Store) categoryTreeWith(ctx context.Context, q querier) ([]CategoryNode, error) {
	rows, err := q.Query(ctx, `
		WITH RECURSIVE tree AS (
			SELECT c.id, 1 AS depth
			  FROM categories c
			 WHERE c.parent_id IS NULL
			UNION ALL
			SELECT c.id, t.depth + 1
			  FROM categories c
			  JOIN tree t ON c.parent_id = t.id
			 WHERE t.depth < 64
		)
		SELECT c.id, c.slug, c.name, c.description, c.sort_order, c.hidden, c.created_at,
		       c.updated_at, c.parent_id, COALESCE(t.depth, 1),
		       (SELECT count(*) FROM products p WHERE p.category_id = c.id) AS product_count
		  FROM categories c
		  LEFT JOIN tree t ON t.id = c.id
		 ORDER BY COALESCE(t.depth, 1), c.sort_order, c.name`)
	if err != nil {
		return nil, fmt.Errorf("catalog: read category tree: %w", err)
	}
	defer rows.Close()

	flat := make([]CategoryNode, 0, 16)
	for rows.Next() {
		var n CategoryNode
		if err := rows.Scan(&n.ID, &n.Slug, &n.Name, &n.Description, &n.SortOrder, &n.Hidden,
			&n.CreatedAt, &n.UpdatedAt, &n.ParentID, &n.Depth, &n.ProductCount); err != nil {
			return nil, fmt.Errorf("catalog: scan category node: %w", err)
		}
		n.Children = nil
		flat = append(flat, n)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("catalog: read category tree: %w", err)
	}
	return buildTree(flat), nil
}

// buildTree turns depth-ordered rows into a nested tree, filling Path and SubtreeProductCount.
//
// Children are collected by parent id and the shape is then built recursively, rather than by
// appending to the parent's slice as rows are read: a struct append copies the child, so a
// grandchild attached after its parent was appended would land on the copy and disappear.
func buildTree(flat []CategoryNode) []CategoryNode {
	index := make(map[string]int, len(flat))
	byParent := make(map[string][]int, len(flat))
	for i := range flat {
		flat[i].Children = nil
		index[flat[i].ID] = i
	}
	roots := make([]int, 0, 4)
	for i := range flat {
		parent := flat[i].ParentID
		if parent == nil {
			roots = append(roots, i)
			continue
		}
		if _, ok := index[*parent]; !ok {
			// A parent we cannot see means a broken chain. Show the node at the root rather
			// than dropping it: a category nobody can see is a category nobody can fix.
			flat[i].ParentID = nil
			flat[i].Depth = 1
			roots = append(roots, i)
			continue
		}
		byParent[*parent] = append(byParent[*parent], i)
	}

	var build func(i int, parentPath string) CategoryNode
	build = func(i int, parentPath string) CategoryNode {
		n := flat[i]
		if parentPath == "" {
			n.Path = n.Slug
		} else {
			n.Path = parentPath + "/" + n.Slug
		}
		n.Children = nil
		total := n.ProductCount
		for _, child := range byParent[n.ID] {
			built := build(child, n.Path)
			total += built.SubtreeProductCount
			n.Children = append(n.Children, built)
		}
		n.SubtreeProductCount = total
		return n
	}

	out := make([]CategoryNode, 0, len(roots))
	for _, i := range roots {
		out = append(out, build(i, ""))
	}
	return out
}

// VisibleCategoryTree is the storefront's view: hidden categories are removed, and so is
// everything below them. Hiding a branch is how an operator stops selling it; a child that
// stayed visible under a hidden parent would keep selling it one level down.
func VisibleCategoryTree(nodes []CategoryNode) []CategoryNode {
	out := make([]CategoryNode, 0, len(nodes))
	for _, n := range nodes {
		if n.Hidden {
			continue
		}
		n.Children = VisibleCategoryTree(n.Children)
		out = append(out, n)
	}
	return out
}

// CategoryNodeBySlug reads one node from the tree by slug, hidden or not.
func (s *Store) CategoryNodeBySlug(ctx context.Context, slug string) (CategoryNode, error) {
	return s.categoryNodeBySlugWith(ctx, s.pool, slug)
}

func (s *Store) categoryNodeBySlugWith(ctx context.Context, q querier, slug string) (CategoryNode, error) {
	nodes, err := s.categoryTreeWith(ctx, q)
	if err != nil {
		return CategoryNode{}, err
	}
	if n, ok := findNode(nodes, func(c CategoryNode) bool {
		return strings.EqualFold(c.Slug, slug)
	}); ok {
		return n, nil
	}
	return CategoryNode{}, ErrNotFound
}

// CategoryNodeByID reads one node from the tree by id.
func (s *Store) CategoryNodeByID(ctx context.Context, id string) (CategoryNode, error) {
	return s.categoryNodeByIDWith(ctx, s.pool, id)
}

func (s *Store) categoryNodeByIDWith(ctx context.Context, q querier, id string) (CategoryNode, error) {
	nodes, err := s.categoryTreeWith(ctx, q)
	if err != nil {
		return CategoryNode{}, err
	}
	if n, ok := findNode(nodes, func(c CategoryNode) bool { return c.ID == id }); ok {
		return n, nil
	}
	return CategoryNode{}, ErrNotFound
}

// findNode searches a nested tree depth-first.
func findNode(nodes []CategoryNode, match func(CategoryNode) bool) (CategoryNode, bool) {
	for _, n := range nodes {
		if match(n) {
			return n, true
		}
		if child, ok := findNode(n.Children, match); ok {
			return child, true
		}
	}
	return CategoryNode{}, false
}

// --------------------------------------------------------------- tree writes --

// SetCategoryParentTx moves a category under a new parent (or to the root, when parentID is
// nil), and returns the tree as it stands afterwards.
//
// The checks below are for the operator's benefit, not the database's: the trigger in 0049
// refuses both a cycle and a too-deep move even when the service layer is bypassed, which is
// what makes the invariant true rather than probable. Here the same conditions are tested first
// so the caller gets "that would make a cycle" instead of "23514".
func (s *Store) SetCategoryParentTx(ctx context.Context, q querier, id string, parentID *string) ([]CategoryNode, error) {
	if strings.TrimSpace(id) == "" {
		return nil, ErrNotFound
	}
	if parentID != nil && *parentID == id {
		return nil, fmt.Errorf("%w: a category cannot be its own parent", ErrCategoryCycle)
	}
	tag, err := q.Exec(ctx, `UPDATE categories SET parent_id = $2 WHERE id = $1`, id, parentID)
	if err != nil {
		switch {
		case isCategoryCycle(err):
			return nil, fmt.Errorf("%w: that move would close a loop", ErrCategoryCycle)
		case isCategoryTooDeep(err):
			return nil, fmt.Errorf("%w: nesting is limited to %d levels", ErrCategoryTooDeep, MaxCategoryDepth)
		case isForeignKeyViolation(err):
			// The parent does not exist. A missing category is caught by RowsAffected below,
			// so a foreign-key failure here can only be the parent.
			return nil, fmt.Errorf("%w: no such parent category", ErrNotFound)
		}
		return nil, fmt.Errorf("catalog: set category parent: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return nil, ErrNotFound
	}
	return s.categoryTreeWith(ctx, q)
}

// refuseCategoryWithChildren is the guard `DeleteCategoryTx` runs before it deletes.
//
// The children rule is 0049's decision 3: a child is not reparented silently, because that
// changes a URL a customer may have bookmarked and a grouping an operator chose. The database
// refuses the delete too (`ON DELETE RESTRICT`); counting first is what turns a foreign-key
// violation into a sentence and a stable `in_use` code. It lives here rather than in catalog.go
// because it belongs to the tree, and catalog.go keeps only the call.
func (s *Store) refuseCategoryWithChildren(ctx context.Context, q querier, id string) error {
	var children int
	if err := q.QueryRow(ctx, `SELECT count(*) FROM categories WHERE parent_id = $1`, id).
		Scan(&children); err != nil {
		return fmt.Errorf("catalog: count child categories: %w", err)
	}
	if children > 0 {
		return fmt.Errorf("%w: %d sub-category(ies) still hang from it; move or delete them first",
			ErrInUse, children)
	}
	return nil
}

// hiddenBranchesCTE selects every category that is hidden *or* sits under a hidden category.
//
// It is the rule "hiding a category hides what is under it", expressed once so the two storefront
// reads cannot drift apart. Before categories could nest, hiding the one category a product sat
// in was enough; with a tree, a visible child under a hidden parent would keep selling a branch
// an operator had stopped offering. It is a `WITH RECURSIVE` prefix rather than a subquery per
// row so the set is computed once for the whole read.
const hiddenBranchesCTE = `WITH RECURSIVE hidden_branches AS (
			SELECT c.id
			  FROM categories c
			 WHERE c.hidden
			UNION ALL
			SELECT c.id
			  FROM categories c
			  JOIN hidden_branches h ON c.parent_id = h.id
		)`

// -------------------------------------------------------------------- servers --

// Servers lists the provisioner targets, disabled ones included: the panel has to be able to
// re-enable one.
func (s *Store) Servers(ctx context.Context) ([]Server, error) {
	return s.serversWith(ctx, s.pool)
}

func (s *Store) serversWith(ctx context.Context, q querier) ([]Server, error) {
	rows, err := q.Query(ctx, `
		SELECT s.id, s.slug, s.name, s.provider, s.region, s.node, s.enabled, s.sort_order,
		       s.created_at, s.updated_at,
		       (SELECT count(*) FROM products p WHERE p.server_id = s.id) AS product_count
		  FROM servers s
		 ORDER BY s.sort_order, s.name`)
	if err != nil {
		return nil, fmt.Errorf("catalog: list servers: %w", err)
	}
	defer rows.Close()
	out := make([]Server, 0, 4)
	for rows.Next() {
		var srv Server
		if err := rows.Scan(&srv.ID, &srv.Slug, &srv.Name, &srv.Provider, &srv.Region,
			&srv.Node, &srv.Enabled, &srv.SortOrder, &srv.CreatedAt, &srv.UpdatedAt,
			&srv.ProductCount); err != nil {
			return nil, fmt.Errorf("catalog: scan server: %w", err)
		}
		out = append(out, srv)
	}
	return out, rows.Err()
}

// Server reads one provisioner target.
func (s *Store) Server(ctx context.Context, id string) (Server, error) {
	return s.serverByIDWith(ctx, s.pool, id)
}

func (s *Store) serverByIDWith(ctx context.Context, q querier, id string) (Server, error) {
	var srv Server
	err := q.QueryRow(ctx, `
		SELECT s.id, s.slug, s.name, s.provider, s.region, s.node, s.enabled, s.sort_order,
		       s.created_at, s.updated_at,
		       (SELECT count(*) FROM products p WHERE p.server_id = s.id)
		  FROM servers s
		 WHERE s.id = $1`, id).
		Scan(&srv.ID, &srv.Slug, &srv.Name, &srv.Provider, &srv.Region, &srv.Node, &srv.Enabled,
			&srv.SortOrder, &srv.CreatedAt, &srv.UpdatedAt, &srv.ProductCount)
	if errors.Is(err, pgx.ErrNoRows) {
		return Server{}, ErrNotFound
	}
	if err != nil {
		return Server{}, fmt.Errorf("catalog: read server: %w", err)
	}
	return srv, nil
}

// CreateServerTx inserts a provisioner target.
func (s *Store) CreateServerTx(ctx context.Context, q querier, srv Server) (Server, error) {
	if err := validateServer(srv); err != nil {
		return Server{}, err
	}
	var out Server
	err := q.QueryRow(ctx, `
		INSERT INTO servers (slug, name, provider, region, node, enabled, sort_order)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING id, slug, name, provider, region, node, enabled, sort_order, created_at, updated_at`,
		strings.TrimSpace(srv.Slug), strings.TrimSpace(srv.Name), strings.TrimSpace(srv.Provider),
		strings.TrimSpace(srv.Region), strings.TrimSpace(srv.Node), srv.Enabled, srv.SortOrder,
	).Scan(&out.ID, &out.Slug, &out.Name, &out.Provider, &out.Region, &out.Node, &out.Enabled,
		&out.SortOrder, &out.CreatedAt, &out.UpdatedAt)
	if err != nil {
		if isUniqueViolation(err) {
			return Server{}, ErrSlugTaken
		}
		return Server{}, fmt.Errorf("catalog: create server: %w", err)
	}
	return out, nil
}

// UpdateServerTx edits a provisioner target. A change here affects where the *next* purchase
// provisions; services that already exist keep the target they were created with, because their
// region was copied into their own config at checkout.
func (s *Store) UpdateServerTx(ctx context.Context, q querier, id string, srv Server) (Server, error) {
	if err := validateServer(srv); err != nil {
		return Server{}, err
	}
	var out Server
	err := q.QueryRow(ctx, `
		UPDATE servers
		   SET slug = $2, name = $3, provider = $4, region = $5, node = $6, enabled = $7,
		       sort_order = $8
		 WHERE id = $1
		RETURNING id, slug, name, provider, region, node, enabled, sort_order, created_at, updated_at`,
		id, strings.TrimSpace(srv.Slug), strings.TrimSpace(srv.Name), strings.TrimSpace(srv.Provider),
		strings.TrimSpace(srv.Region), strings.TrimSpace(srv.Node), srv.Enabled, srv.SortOrder,
	).Scan(&out.ID, &out.Slug, &out.Name, &out.Provider, &out.Region, &out.Node, &out.Enabled,
		&out.SortOrder, &out.CreatedAt, &out.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Server{}, ErrNotFound
	}
	if err != nil {
		if isUniqueViolation(err) {
			return Server{}, ErrSlugTaken
		}
		return Server{}, fmt.Errorf("catalog: update server: %w", err)
	}
	return out, nil
}

// DeleteServerTx removes a provisioner target, refusing while a product still points at it.
// Disabling is what "stop selling onto this node" means; deleting would silently move those
// products back to their plan's region, which is a provisioning change nobody asked for.
func (s *Store) DeleteServerTx(ctx context.Context, q querier, id string) error {
	var products int
	if err := q.QueryRow(ctx, `SELECT count(*) FROM products WHERE server_id = $1`, id).
		Scan(&products); err != nil {
		return fmt.Errorf("catalog: count products on server: %w", err)
	}
	if products > 0 {
		return fmt.Errorf("%w: %d product(s) are sold on it; disable it instead", ErrInUse, products)
	}
	tag, err := q.Exec(ctx, `DELETE FROM servers WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("catalog: delete server: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// validateServer normalises and bounds a provisioner target.
func validateServer(srv Server) error {
	if err := ValidateSlug(strings.TrimSpace(srv.Slug)); err != nil {
		return err
	}
	if strings.TrimSpace(srv.Name) == "" {
		return fmt.Errorf("%w: name is required", ErrInvalidInput)
	}
	if len(srv.Region) > 128 || len(srv.Node) > 128 || len(srv.Provider) > 128 {
		return fmt.Errorf("%w: region, node and provider are limited to 128 characters", ErrInvalidInput)
	}
	return nil
}

// --------------------------------------------------------- product placement --

// ProductPlacement reads where a product sits: its server and its category branch.
func (s *Store) ProductPlacement(ctx context.Context, productID string) (ProductPlacement, error) {
	return s.productPlacementWith(ctx, s.pool, productID)
}

func (s *Store) productPlacementWith(ctx context.Context, q querier, productID string) (ProductPlacement, error) {
	out := ProductPlacement{ProductID: productID, CategoryAncestorSlugs: []string{}}
	err := q.QueryRow(ctx, `SELECT server_id, category_id FROM products WHERE id = $1`, productID).
		Scan(&out.ServerID, &out.CategoryID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ProductPlacement{}, ErrNotFound
	}
	if err != nil {
		return ProductPlacement{}, fmt.Errorf("catalog: read product placement: %w", err)
	}

	if out.ServerID != nil {
		srv, err := s.serverByIDWith(ctx, q, *out.ServerID)
		if err != nil {
			return ProductPlacement{}, err
		}
		out.Server = &srv
	}
	if out.CategoryID != nil {
		chain, err := s.categoryChainWith(ctx, q, *out.CategoryID)
		if err != nil {
			return ProductPlacement{}, err
		}
		if len(chain) > 0 {
			slugs := make([]string, 0, len(chain))
			for _, c := range chain {
				slugs = append(slugs, c.slug)
			}
			out.CategoryPath = strings.Join(slugs, "/")
			out.CategoryAncestorSlugs = slugs
			// The last entry is the category itself, so its parent is the one before it.
			if len(chain) > 1 {
				parent := chain[len(chain)-2].id
				out.CategoryParentID = &parent
			}
		}
	}
	return out, nil
}

// categoryChain is one step of an ancestor walk: id, slug, and whether the step is hidden.
type categoryChain struct {
	id, slug string
	hidden   bool
}

// categoryChainWith returns the ancestor chain of a category, root first, ending with the
// category itself. One recursive CTE rather than a loop of queries, so a deep tree costs one
// round trip.
func (s *Store) categoryChainWith(ctx context.Context, q querier, id string) ([]categoryChain, error) {
	rows, err := q.Query(ctx, `
		WITH RECURSIVE chain AS (
			SELECT c.id, c.slug, c.parent_id, c.hidden, 1 AS depth
			  FROM categories c
			 WHERE c.id = $1
			UNION ALL
			SELECT c.id, c.slug, c.parent_id, c.hidden, ch.depth + 1
			  FROM categories c
			  JOIN chain ch ON c.id = ch.parent_id
			 WHERE ch.depth < 64
		)
		SELECT id, slug, hidden FROM chain ORDER BY depth DESC`, id)
	if err != nil {
		return nil, fmt.Errorf("catalog: read category chain: %w", err)
	}
	defer rows.Close()
	out := make([]categoryChain, 0, 4)
	for rows.Next() {
		var c categoryChain
		if err := rows.Scan(&c.id, &c.slug, &c.hidden); err != nil {
			return nil, fmt.Errorf("catalog: scan category chain: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// SetProductServerTx assigns a product to a provisioner target, or clears the assignment when
// serverID is nil. It returns the placement as it stands afterwards.
//
// The change is catalogue metadata: it decides where the next purchase is provisioned and
// nothing about services that already exist, whose region was copied into their own config at
// checkout.
func (s *Store) SetProductServerTx(ctx context.Context, q querier, productID string, serverID *string) (ProductPlacement, error) {
	tag, err := q.Exec(ctx, `UPDATE products SET server_id = $2 WHERE id = $1`, productID, serverID)
	if err != nil {
		if isForeignKeyViolation(err) {
			return ProductPlacement{}, fmt.Errorf("%w: no such server", ErrNotFound)
		}
		return ProductPlacement{}, fmt.Errorf("catalog: set product server: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ProductPlacement{}, ErrNotFound
	}
	return s.productPlacementWith(ctx, q, productID)
}

// --------------------------------------------------------- storefront subtree --

// PublicCategoryProducts reads one visible category, its visible children, and every product in
// the branch below it.
//
// This is what makes nesting worth having: clicking "Game servers" lists the Minecraft
// products, which are filed under "Game servers › Minecraft". The predicates are the same ones
// Public uses — not hidden, priced, in a visible category — plus one the flat read cannot
// express: a product whose *ancestor* is hidden is not offered, because hiding a branch means
// hiding what is under it.
func (s *Store) PublicCategoryProducts(ctx context.Context, slug string) (CategoryNode, []Product, error) {
	node, err := s.categoryNodeBySlugWith(ctx, s.pool, slug)
	if err != nil {
		return CategoryNode{}, nil, err
	}
	if node.Hidden {
		// A hidden category is indistinguishable from a missing one to a visitor.
		return CategoryNode{}, nil, ErrNotFound
	}
	// So is a visible category under a hidden ancestor: hiding a branch means hiding what is
	// under it, and a page that rendered anyway would keep selling it one level down.
	chain, err := s.categoryChainWith(ctx, s.pool, node.ID)
	if err != nil {
		return CategoryNode{}, nil, err
	}
	for _, c := range chain {
		if c.hidden {
			return CategoryNode{}, nil, ErrNotFound
		}
	}
	node.Children = VisibleCategoryTree(node.Children)

	products, err := s.query(ctx, `
		WITH RECURSIVE branch AS (
			SELECT c.id, c.parent_id
			  FROM categories c
			 WHERE lower(c.slug) = lower($1) AND NOT c.hidden
			UNION ALL
			SELECT c.id, c.parent_id
			  FROM categories c
			  JOIN branch b ON c.parent_id = b.id
			 WHERE NOT c.hidden
		)
		SELECT p.id, p.slug, p.name, p.short_description, p.description, p.description_format,
		       p.category_id, c.name, c.slug, p.image_id, p.hidden, p.sort_order,
		       p.created_at, p.updated_at, 0, p.email_template
		  FROM products p
		  JOIN categories c ON c.id = p.category_id AND NOT c.hidden
		 WHERE p.category_id IN (SELECT id FROM branch)
		   AND NOT p.hidden
		   AND EXISTS (SELECT 1 FROM price_versions pv WHERE pv.product_id = p.id)
		 ORDER BY c.sort_order, c.name, p.sort_order, p.name`, slug)
	if err != nil {
		return CategoryNode{}, nil, err
	}
	return node, products, nil
}

// ----------------------------------------------------------------- sqlstate --

// The two refusals the tree trigger raises are both 23514 (check_violation) and are told apart
// by the constraint name, which is why the trigger names them.
func isCategoryCycle(err error) bool {
	return constraintName(err) == categoryCycleConstraint
}

func isCategoryTooDeep(err error) bool {
	return constraintName(err) == categoryDepthConstraint
}

func constraintName(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.ConstraintName
	}
	return ""
}
