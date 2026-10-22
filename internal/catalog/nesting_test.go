package catalog_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/catalyst/billing/internal/catalog"
	"github.com/catalyst/billing/internal/extproto"
	"github.com/catalyst/billing/internal/provisioning"
	"github.com/catalyst/billing/internal/store"
	"github.com/catalyst/billing/internal/store/storetest"
	"github.com/jackc/pgx/v5/pgxpool"
)

// newStoreWithStorePool returns the catalogue store, its raw pgx pool, and the store pool the
// provisioning worker needs.
//
// storetest.DB must be called exactly once per test: it refcounts the package session but
// registers only one cleanup, so a second call inside the same test leaves the advisory lock
// held and wedges every test that runs after it.
func newStoreWithStorePool(t *testing.T) (*catalog.Store, *pgxpool.Pool, *store.Pool) {
	t.Helper()
	pool := storetest.DB(t)
	return catalog.New(pool.Pool), pool.Pool, pool
}

// The catalogue's structure: the category tree and the product → server assignment.
//
// Every test here is written so it fails if the invariant is only *present* rather than true:
// a cycle that the service refuses but the database accepts, a depth bound checked on the row
// instead of the branch, a delete that silently reparents, and a server that round-trips through
// the panel but never reaches the provisioner.

// ---------------------------------------------------------------- the tree --

// mustChildCategory creates a category and moves it under a parent, or fails the test.
//
// The two steps are the two requests the panel makes (create, then move), and they are
// deliberately not wrapped in one helper transaction: a refused move must leave the created
// category where it is, visible at the root, rather than silently disappearing from under the
// operator's cursor.
func mustChildCategory(t *testing.T, s *catalog.Store, pool *pgxpool.Pool, ctx context.Context,
	slug string, parentID *string) catalog.CategoryNode {
	t.Helper()
	created := mustCategory(t, s, ctx, slug)
	if _, err := s.SetCategoryParentTx(ctx, pool, created.ID, parentID); err != nil {
		t.Fatalf("move category %q under %v: %v", slug, parentID, err)
	}
	node, err := s.CategoryNodeByID(ctx, created.ID)
	if err != nil {
		t.Fatalf("read category %q: %v", slug, err)
	}
	return node
}

func TestCategoryTreeRoundTripsAndAChildsProductIsFoundFromTheParent(t *testing.T) {
	s, pool := newStoreWithPool(t)
	ctx := context.Background()

	gameServers := mustCategory(t, s, ctx, "game-servers")
	minecraft := mustChildCategory(t, s, pool, ctx, "minecraft", &gameServers.ID)
	modded := mustChildCategory(t, s, pool, ctx, "modded", &minecraft.ID)

	// A priced product in the grandchild, and one directly in the root.
	mustProduct(t, s, ctx, modded.ID, "atm10", false)
	mustProduct(t, s, ctx, gameServers.ID, "rust", false)

	tree, err := s.CategoryTree(ctx)
	if err != nil {
		t.Fatalf("tree: %v", err)
	}
	if len(tree) != 1 || tree[0].Slug != "game-servers" {
		t.Fatalf("tree roots = %v, want the one root", categorySlugs(tree))
	}
	root := tree[0]
	if root.Depth != 1 || root.Path != "game-servers" || root.ParentID != nil {
		t.Fatalf("root = depth %d path %q parent %v", root.Depth, root.Path, root.ParentID)
	}
	if len(root.Children) != 1 || root.Children[0].Slug != "minecraft" {
		t.Fatalf("root children = %v", categorySlugs(root.Children))
	}
	child := root.Children[0]
	if child.Depth != 2 || child.Path != "game-servers/minecraft" {
		t.Fatalf("child = depth %d path %q", child.Depth, child.Path)
	}
	if child.ParentID == nil || *child.ParentID != root.ID {
		t.Fatalf("child parent = %v, want %s", child.ParentID, root.ID)
	}
	if len(child.Children) != 1 || child.Children[0].Path != "game-servers/minecraft/modded" {
		t.Fatalf("grandchild path = %v", categorySlugs(child.Children))
	}
	if child.Children[0].Depth != 3 {
		t.Fatalf("grandchild depth = %d, want 3", child.Children[0].Depth)
	}

	// The branch count is the whole point of the tree in the storefront: a product filed under
	// "Modded" has to be counted under "Game servers" too.
	if root.ProductCount != 1 || root.SubtreeProductCount != 2 {
		t.Fatalf("root counts = %d own / %d branch, want 1 / 2", root.ProductCount, root.SubtreeProductCount)
	}

	// And it has to be *findable*: the category page for the parent lists both products.
	node, products, err := s.PublicCategoryProducts(ctx, "game-servers")
	if err != nil {
		t.Fatalf("category page: %v", err)
	}
	if node.Slug != "game-servers" || len(node.Children) != 1 {
		t.Fatalf("category page node = %+v", node)
	}
	if got := slugs(products); !sameSet(got, "atm10", "rust") {
		t.Fatalf("products under the parent = %v, want the grandchild's product too", got)
	}

	// The child's own page lists only its branch.
	_, minecraftProducts, err := s.PublicCategoryProducts(ctx, "minecraft")
	if err != nil {
		t.Fatalf("child category page: %v", err)
	}
	if got := slugs(minecraftProducts); !sameSet(got, "atm10") {
		t.Fatalf("products under the child = %v, want only atm10", got)
	}

	// The flat storefront read is untouched: it still lists both products, unchanged.
	flat, err := s.Public(ctx)
	if err != nil {
		t.Fatalf("public: %v", err)
	}
	if len(flat) != 2 {
		t.Fatalf("flat public read = %v, want both products exactly as before", slugs(flat))
	}
}

func TestHiddenCategoryHidesItsWholeBranch(t *testing.T) {
	s, pool := newStoreWithPool(t)
	ctx := context.Background()

	parent := mustCategory(t, s, ctx, "servers")
	child := mustChildCategory(t, s, pool, ctx, "minecraft", &parent.ID)
	mustProduct(t, s, ctx, child.ID, "atm10", false)

	// Hiding the parent hides what is under it: a branch an operator stopped selling must not
	// keep selling one level down.
	if _, err := s.UpdateCategory(ctx, parent.ID, catalog.Category{
		Slug: "servers", Name: "SERVERS", Hidden: true,
	}); err != nil {
		t.Fatalf("hide parent: %v", err)
	}

	tree, err := s.CategoryTree(ctx)
	if err != nil {
		t.Fatalf("tree: %v", err)
	}
	if visible := catalog.VisibleCategoryTree(tree); len(visible) != 0 {
		t.Fatalf("visible tree = %v, want nothing under a hidden parent", categorySlugs(visible))
	}
	if _, _, err := s.PublicCategoryProducts(ctx, "minecraft"); !errors.Is(err, catalog.ErrNotFound) {
		t.Fatalf("a product under a hidden parent is still reachable: %v", err)
	}
	if _, _, err := s.PublicCategoryProducts(ctx, "servers"); !errors.Is(err, catalog.ErrNotFound) {
		t.Fatalf("the hidden category page = %v, want ErrNotFound", err)
	}
	// The flat storefront read agrees: a hidden branch is not offered at all.
	if products, err := s.Public(ctx); err != nil {
		t.Fatalf("public: %v", err)
	} else if len(products) != 0 {
		t.Fatalf("flat public read leaked a product from a hidden branch: %v", slugs(products))
	}
	// And a bookmark to the product's own URL does not keep it on sale either.
	if _, err := s.PublicProduct(ctx, "atm10"); !errors.Is(err, catalog.ErrNotFound) {
		t.Fatalf("a product under a hidden branch is still reachable by slug: %v", err)
	}
}

// ------------------------------------------------------------------ cycles --

// TestCategoryCycleIsRefusedByTheDatabase is the assertion that matters: it does not go through
// the service layer at all. A code-only check cannot enforce this, because two concurrent writes
// can each look safe; if this test ever passes with the service's check removed, the invariant
// is a comment rather than a rule.
func TestCategoryCycleIsRefusedByTheDatabase(t *testing.T) {
	s, pool := newStoreWithPool(t)
	ctx := context.Background()

	a := mustCategory(t, s, ctx, "cycle-a")
	b := mustChildCategory(t, s, pool, ctx, "cycle-b", &a.ID)

	// B is under A. Making A a child of B closes the loop A → B → A.
	if _, err := pool.Exec(ctx, `UPDATE categories SET parent_id = $1 WHERE id = $2`, b.ID, a.ID); err == nil {
		t.Fatal("the database accepted A → B → A: a cycle in a self-referencing tree is unreadable")
	}

	// A category cannot be its own parent either, which is the shortest cycle.
	if _, err := pool.Exec(ctx, `UPDATE categories SET parent_id = id WHERE id = $1`, a.ID); err == nil {
		t.Fatal("the database accepted a category as its own parent")
	}

	// The service refuses it too, with a code a client can branch on.
	if _, err := s.SetCategoryParentTx(ctx, pool, a.ID, &b.ID); !errors.Is(err, catalog.ErrCategoryCycle) {
		t.Fatalf("service cycle = %v, want ErrCategoryCycle", err)
	}

	// A deeper loop is refused as well: A → B → C, then C under A is fine, A under C is not.
	c := mustChildCategory(t, s, pool, ctx, "cycle-c", &b.ID)
	if _, err := pool.Exec(ctx, `UPDATE categories SET parent_id = $1 WHERE id = $2`, c.ID, a.ID); err == nil {
		t.Fatal("the database accepted A → C → B → C")
	}

	// The tree is still readable, which is the property a cycle would destroy.
	if _, err := s.CategoryTree(ctx); err != nil {
		t.Fatalf("tree after the refused writes: %v", err)
	}
}

// TestConcurrentOppositeMovesCannotBothWin is the reason the trigger takes an advisory lock.
//
// Two transactions each propose the half of a cycle that looks safe on its own: A under B, and B
// under A. Both start before either commits, so neither ancestor walk can see the other's row
// unless the writes are serialised. Exactly one must succeed.
func TestConcurrentOppositeMovesCannotBothWin(t *testing.T) {
	s, pool := newStoreWithPool(t)
	ctx := context.Background()

	a := mustCategory(t, s, ctx, "race-a")
	b := mustCategory(t, s, ctx, "race-b")

	var successes int
	var mu sync.Mutex
	start := make(chan struct{})
	var wg sync.WaitGroup
	for _, move := range []struct{ id, parent string }{{a.ID, b.ID}, {b.ID, a.ID}} {
		wg.Add(1)
		go func(id, parent string) {
			defer wg.Done()
			<-start
			err := func() error {
				tx, err := pool.Begin(context.Background())
				if err != nil {
					return err
				}
				defer func() { _ = tx.Rollback(context.Background()) }()
				if _, err := tx.Exec(context.Background(),
					`UPDATE categories SET parent_id = $2 WHERE id = $1`, id, parent); err != nil {
					return err
				}
				return tx.Commit(context.Background())
			}()
			if err == nil {
				mu.Lock()
				successes++
				mu.Unlock()
			}
		}(move.id, move.parent)
	}
	close(start)
	wg.Wait()

	if successes != 1 {
		t.Fatalf("%d of the two opposite moves succeeded, want exactly 1: without the advisory "+
			"lock both look safe and the tree ends up with a cycle", successes)
	}
	tree, err := s.CategoryTree(ctx)
	if err != nil {
		t.Fatalf("tree: %v", err)
	}
	if len(tree) != 1 || len(tree[0].Children) != 1 {
		t.Fatalf("tree after the race = %v, want one root with one child", categorySlugs(tree))
	}
}

// ------------------------------------------------------------- the depth bound --

func TestCategoryDepthBoundIsEnforced(t *testing.T) {
	s, pool := newStoreWithPool(t)
	ctx := context.Background()

	// Four levels is allowed: root, child, grandchild, great-grandchild.
	level1 := mustCategory(t, s, ctx, "d1")
	level2 := mustChildCategory(t, s, pool, ctx, "d2", &level1.ID)
	level3 := mustChildCategory(t, s, pool, ctx, "d3", &level2.ID)
	level4 := mustChildCategory(t, s, pool, ctx, "d4", &level3.ID)

	if level4.Depth != catalog.MaxCategoryDepth {
		t.Fatalf("depth of the deepest allowed category = %d, want %d", level4.Depth, catalog.MaxCategoryDepth)
	}

	// A fifth level is refused by the service, with a code a client can branch on...
	orphan := mustCategory(t, s, ctx, "d5")
	if _, err := s.SetCategoryParentTx(ctx, pool, orphan.ID, &level4.ID); !errors.Is(err, catalog.ErrCategoryTooDeep) {
		t.Fatalf("a fifth level = %v, want ErrCategoryTooDeep", err)
	}
	// ...and by the database, even when the service is bypassed.
	if _, err := pool.Exec(ctx, `INSERT INTO categories (slug, name, parent_id) VALUES ('d5-raw', 'D5', $1)`, level4.ID); err == nil {
		t.Fatal("the database accepted a fifth level of nesting")
	}
	// The refused move left the category where it was, at the root, rather than half-moved.
	if node, err := s.CategoryNodeByID(ctx, orphan.ID); err != nil {
		t.Fatalf("read the refused category: %v", err)
	} else if node.ParentID != nil || node.Depth != 1 {
		t.Fatalf("the refused move left the category at depth %d under %v", node.Depth, node.ParentID)
	}

	// The bound is on the branch, not on the row: a three-deep branch moved under a two-deep
	// parent is refused even though the moved node itself would only sit at level three,
	// because its leaf would land at level five.
	other := mustCategory(t, s, ctx, "d1b")
	otherChild := mustChildCategory(t, s, pool, ctx, "d2b", &other.ID)
	branchRoot := mustCategory(t, s, ctx, "e1")
	branchMid := mustChildCategory(t, s, pool, ctx, "e2", &branchRoot.ID)
	mustChildCategory(t, s, pool, ctx, "e3", &branchMid.ID)
	if _, err := s.SetCategoryParentTx(ctx, pool, branchRoot.ID, &otherChild.ID); !errors.Is(err, catalog.ErrCategoryTooDeep) {
		t.Fatalf("moving a three-deep branch under a level-2 parent = %v, want ErrCategoryTooDeep", err)
	}
	// The database refuses it too, for a caller that does not ask the service first.
	if _, err := pool.Exec(ctx, `UPDATE categories SET parent_id = $1 WHERE id = $2`,
		otherChild.ID, branchRoot.ID); err == nil {
		t.Fatal("the database accepted a move that pushes a leaf to level five")
	}

	// A legal move still works, and the tree reports the new shape.
	if _, err := s.SetCategoryParentTx(ctx, pool, level4.ID, nil); err != nil {
		t.Fatalf("moving the leaf to the root: %v", err)
	}
	tree, err := s.CategoryTree(ctx)
	if err != nil {
		t.Fatalf("tree: %v", err)
	}
	// d1 and d1b still hold their chains, e1 still holds its own, d4 was moved to the root
	// above, and d5 stayed at the root because its move was refused.
	if !sameSet(categorySlugs(tree), "d1", "d1b", "d4", "d5", "e1") {
		t.Fatalf("roots after the moves = %v", categorySlugs(tree))
	}
}

// ---------------------------------------------------------- deleting a branch --

func TestDeletingACategoryWithChildrenIsRefused(t *testing.T) {
	s, pool := newStoreWithPool(t)
	ctx := context.Background()

	parent := mustCategory(t, s, ctx, "del-parent")
	child := mustChildCategory(t, s, pool, ctx, "del-child", &parent.ID)

	// Refused with a code, not reparented: moving the child to the root would change a URL a
	// customer may have bookmarked.
	if err := s.DeleteCategory(ctx, parent.ID); !errors.Is(err, catalog.ErrInUse) {
		t.Fatalf("delete of a parent = %v, want ErrInUse", err)
	}
	// And refused by the database too, in case a caller forgets to ask.
	if _, err := pool.Exec(ctx, `DELETE FROM categories WHERE id = $1`, parent.ID); err == nil {
		t.Fatal("the database deleted a category that still has children")
	}

	// The child is still there, still under its parent: nothing was reparented behind our back.
	node, err := s.CategoryNodeByID(ctx, child.ID)
	if err != nil {
		t.Fatalf("child after the refused delete: %v", err)
	}
	if node.ParentID == nil || *node.ParentID != parent.ID {
		t.Fatalf("child parent after the refused delete = %v", node.ParentID)
	}

	// Deleting bottom-up works, which is the deliberate path the operator is offered.
	if err := s.DeleteCategory(ctx, child.ID); err != nil {
		t.Fatalf("delete child: %v", err)
	}
	if err := s.DeleteCategory(ctx, parent.ID); err != nil {
		t.Fatalf("delete parent: %v", err)
	}
}

// ------------------------------------------------------------- servers --

func mustServer(t *testing.T, s *catalog.Store, pool *pgxpool.Pool, ctx context.Context,
	slug, region, node string) catalog.Server {
	t.Helper()
	srv, err := s.CreateServerTx(ctx, pool, catalog.Server{
		Slug: slug, Name: strings.ToUpper(slug), Region: region, Node: node, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create server %q: %v", slug, err)
	}
	return srv
}

func TestServerRoundTripsAndItsDeleteIsRefusedWhileInUse(t *testing.T) {
	s, pool := newStoreWithPool(t)
	ctx := context.Background()

	srv := mustServer(t, s, pool, ctx, "eu-west", "eu-west", "node-7")
	if srv.Region != "eu-west" || srv.Node != "node-7" || !srv.Enabled {
		t.Fatalf("server = %+v", srv)
	}

	// The slug is an identity an operator reads, so it is unique.
	if _, err := s.CreateServerTx(ctx, pool, catalog.Server{Slug: "eu-west", Name: "Again"}); !errors.Is(err, catalog.ErrSlugTaken) {
		t.Fatalf("duplicate slug = %v, want ErrSlugTaken", err)
	}

	cat := mustCategory(t, s, ctx, "srv-cat")
	product := mustProduct(t, s, ctx, cat.ID, "on-a-server", false)
	placement, err := s.SetProductServerTx(ctx, pool, product.ID, &srv.ID)
	if err != nil {
		t.Fatalf("assign server: %v", err)
	}
	if placement.ServerID == nil || *placement.ServerID != srv.ID {
		t.Fatalf("placement = %+v", placement)
	}
	if placement.Server == nil || placement.Server.Region != "eu-west" {
		t.Fatalf("placement did not resolve the server: %+v", placement.Server)
	}

	// A server a product is sold on cannot be deleted: that would silently move the product back
	// to its plan's region, which is a provisioning change nobody asked for.
	if err := s.DeleteServerTx(ctx, pool, srv.ID); !errors.Is(err, catalog.ErrInUse) {
		t.Fatalf("delete of an in-use server = %v, want ErrInUse", err)
	}

	// Clearing the assignment is how an operator retires it, and then the delete works.
	if _, err := s.SetProductServerTx(ctx, pool, product.ID, nil); err != nil {
		t.Fatalf("clear server: %v", err)
	}
	if err := s.DeleteServerTx(ctx, pool, srv.ID); err != nil {
		t.Fatalf("delete after clearing: %v", err)
	}
}

// TestProductServerReachesTheProvisioner is the part of the feature that matters: the column is
// not decoration, it is the region the adapter is told to provision into.
func TestProductServerReachesTheProvisioner(t *testing.T) {
	s, pool, storePool := newStoreWithStorePool(t)
	ctx := context.Background()

	srv := mustServer(t, s, pool, ctx, "hetzner-fsn", "eu-central", "node-fsn-1")
	cat := mustCategory(t, s, ctx, "prov-cat")

	// Three products: one assigned to the server and with no region on its plan, one assigned
	// but whose plan names its own region, and one with no server at all.
	assigned := mustProduct(t, s, ctx, cat.ID, "assigned", false)
	if _, err := s.SetProductServerTx(ctx, pool, assigned.ID, &srv.ID); err != nil {
		t.Fatalf("assign: %v", err)
	}
	planWins := mustProduct(t, s, ctx, cat.ID, "plan-wins", false)
	if _, err := s.SetProductServerTx(ctx, pool, planWins.ID, &srv.ID); err != nil {
		t.Fatalf("assign: %v", err)
	}
	noServer := mustProduct(t, s, ctx, cat.ID, "no-server", false)

	customerID := mustCustomer(t, pool, ctx, "prov@example.test")
	// The config each service carries is what checkout wrote: the plan's region when the price
	// version names one, and nothing when it does not.
	services := map[string]string{}
	for _, c := range []struct {
		productID string
		config    string
	}{
		{assigned.ID, `{"plan_id":"small"}`},
		{planWins.ID, `{"plan_id":"small","region":"us-east"}`},
		{noServer.ID, `{"plan_id":"small"}`},
	} {
		id := mustService(t, pool, ctx, customerID, c.productID, c.config)
		services[c.productID] = id
		mustQueuedCreate(t, pool, ctx, id)
	}

	adapter := &recordingAdapter{}
	worker := provisioning.NewWorker(storePool, adapter)
	if _, err := worker.Run(ctx, 3); err != nil {
		t.Fatalf("run worker: %v", err)
	}
	if len(adapter.reqs) != 3 {
		t.Fatalf("the adapter saw %d create requests, want 3", len(adapter.reqs))
	}

	byService := map[string]provisioning.CreateRequest{}
	for _, req := range adapter.reqs {
		byService[req.ServiceID] = req
	}

	// The product's server supplies the region when the plan names none...
	got := byService[services[assigned.ID]]
	if got.Region != "eu-central" {
		t.Fatalf("region for the assigned product = %q, want the server's eu-central", got.Region)
	}
	// ...and the node travels with it, because a provider addresses a target by node id.
	if got.Metadata["server_node"] != "node-fsn-1" || got.Metadata["server_id"] != srv.ID {
		t.Fatalf("metadata = %v, want the server's id and node", got.Metadata)
	}

	// The plan's own region still wins, so an existing price keeps provisioning exactly where it
	// did before 0049.
	if got := byService[services[planWins.ID]]; got.Region != "us-east" {
		t.Fatalf("region for the plan that names one = %q, want us-east", got.Region)
	}

	// A product with no server behaves exactly as it did before this feature existed.
	if got := byService[services[noServer.ID]]; got.Region != "" || got.Metadata["server_node"] != "" {
		t.Fatalf("a product with no server got region %q / node %q, want nothing",
			got.Region, got.Metadata["server_node"])
	}
}

// ------------------------------------------------------------------ helpers --

// recordingAdapter records the create requests it is handed.
type recordingAdapter struct{ reqs []provisioning.CreateRequest }

func (a *recordingAdapter) Name() string           { return "recording" }
func (a *recordingAdapter) Capabilities() []string { return []string{"create", "lookup"} }

func (a *recordingAdapter) Create(_ context.Context, req provisioning.CreateRequest) (extproto.ProvisionCreateResult, error) {
	a.reqs = append(a.reqs, req)
	return extproto.ProvisionCreateResult{ExternalID: "ext-" + req.ServiceID, Status: "running"}, nil
}

func (a *recordingAdapter) Lookup(context.Context, string) (extproto.ProvisionLookupResult, error) {
	return extproto.ProvisionLookupResult{}, nil
}

func (a *recordingAdapter) Inspect(_ context.Context, externalID string) (extproto.ProvisionInspectResult, error) {
	return extproto.ProvisionInspectResult{ExternalID: externalID, Status: "running"}, nil
}

func (a *recordingAdapter) Suspend(context.Context, string, string) (extproto.ProvisionStateResult, error) {
	return extproto.ProvisionStateResult{}, nil
}

func (a *recordingAdapter) Resume(context.Context, string) (extproto.ProvisionStateResult, error) {
	return extproto.ProvisionStateResult{}, nil
}

func (a *recordingAdapter) Terminate(context.Context, string, string) (extproto.ProvisionStateResult, error) {
	return extproto.ProvisionStateResult{}, nil
}

func mustCustomer(t *testing.T, pool *pgxpool.Pool, ctx context.Context, email string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(ctx, `INSERT INTO customers (email, name) VALUES ($1, 'P') RETURNING id`,
		email).Scan(&id); err != nil {
		t.Fatalf("create customer: %v", err)
	}
	return id
}

func mustService(t *testing.T, pool *pgxpool.Pool, ctx context.Context, customerID, productID, config string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(ctx, `
		INSERT INTO services (customer_id, product_id, status, generation, config)
		VALUES ($1, $2, 'pending', 1, $3::jsonb) RETURNING id`,
		customerID, productID, config).Scan(&id); err != nil {
		t.Fatalf("create service: %v", err)
	}
	return id
}

func mustQueuedCreate(t *testing.T, pool *pgxpool.Pool, ctx context.Context, serviceID string) {
	t.Helper()
	if _, err := pool.Exec(ctx, `
		INSERT INTO service_operations (service_id, kind, operation_key, generation, status)
		VALUES ($1, 'create', $2, 1, 'queued')`, serviceID, "create:"+serviceID); err != nil {
		t.Fatalf("queue create: %v", err)
	}
}

func categorySlugs(nodes []catalog.CategoryNode) []string {
	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, n.Slug)
	}
	return out
}

// sameSet compares a list of strings against the wanted values regardless of order, so a test
// asserting membership does not fail because two categories happen to sort differently.
func sameSet(got []string, want ...string) bool {
	if len(got) != len(want) {
		return false
	}
	seen := map[string]int{}
	for _, g := range got {
		seen[g]++
	}
	for _, w := range want {
		seen[w]--
		if seen[w] < 0 {
			return false
		}
	}
	return true
}
