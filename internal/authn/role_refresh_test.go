package authn_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/catalyst/billing/internal/authn"
)

// Live role refresh.
//
// The Authorizer caches the custom-role table because a permission check runs on every
// guarded request (internal/authn/authz.go says why). These tests prove the other half of
// that trade: a change made straight in the database becomes effective without a restart,
// and the request path still costs no query.

// A permission inserted directly into role_permissions — by psql, by a restore, by another
// instance — becomes effective while the process is running.
//
// This is the regression that motivated the refresher: before it, the snapshot was taken at
// boot and at an HTTP role edit, so a hand-inserted row stayed inert until the next restart
// and the operator concluded the guard was broken.
func TestDirectPermissionInsertTakesEffectWithoutRestart(t *testing.T) {
	f := newIdentityFixture(t)
	ctx := context.Background()

	// A custom role holding one permission, held by one active user.
	rec := createRole(t, f, "live-refresh", "Role the refresh test widens",
		[]authn.Permission{authn.PermReportRead})
	userID := f.user(t, "live-refresh@example.test")
	if _, err := f.pool.Exec(ctx,
		`INSERT INTO staff_roles (user_id, role_id) VALUES ($1, $2)`, userID, rec.ID); err != nil {
		t.Fatalf("assign role: %v", err)
	}
	user, err := f.svc.UserByID(ctx, userID)
	if err != nil {
		t.Fatalf("load user: %v", err)
	}

	// The boot snapshot.
	authz := authn.NewAuthorizer()
	table, err := f.svc.AllRolePermissions(ctx)
	if err != nil {
		t.Fatalf("load role permissions: %v", err)
	}
	authz.SetTableRoles(table)

	if !authz.Can(user, authn.PermReportRead) {
		t.Fatal("the role's own permission is not effective in the boot snapshot")
	}
	if authz.Can(user, authn.PermLogRead) {
		t.Fatal("log:read is effective before it was granted")
	}

	// Bypass the service entirely: a raw INSERT, exactly like `psql -c` or a restore.
	if _, err := f.pool.Exec(ctx,
		`INSERT INTO role_permissions (role_id, permission) VALUES ($1, $2)`,
		rec.ID, string(authn.PermLogRead)); err != nil {
		t.Fatalf("insert permission: %v", err)
	}

	// Nothing has refreshed yet, so the cached table is still the old one. Asserting this is
	// what makes the rest of the test mean something: the permission did not become effective
	// because the authorizer reads through to the database.
	if authz.Can(user, authn.PermLogRead) {
		t.Fatal("the permission is effective without a refresh; the authorizer is querying per check")
	}

	loopCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	refresher := authn.NewRoleRefresher(authz, f.svc, 20*time.Millisecond)
	refresher.Start(loopCtx)
	defer refresher.Stop()

	deadline := time.Now().Add(10 * time.Second)
	for !authz.Can(user, authn.PermLogRead) {
		if time.Now().After(deadline) {
			t.Fatalf("the inserted permission is still not effective after %d loads", refresher.Loads())
		}
		time.Sleep(10 * time.Millisecond)
	}

	// The refresh is a reload, not a widening: the permission the role already had is still
	// there, and a permission it does not hold is still refused.
	if !authz.Can(user, authn.PermReportRead) {
		t.Fatal("the refresh dropped a permission the role already held")
	}
	if authz.Can(user, authn.PermPaymentRefund) {
		t.Fatal("the refresh granted a permission no role holds")
	}
	if refresher.Applied() == 0 {
		t.Fatal("the refresher never installed a snapshot")
	}
}

// checking a permission does not touch the database.
//
// This is the constraint the cache exists for, asserted rather than assumed: the refresher
// is the only thing that queries, and it queries on its own goroutine.
func TestPermissionChecksDoNotQuery(t *testing.T) {
	source := &countingSource{table: map[authn.Role][]authn.Permission{
		"t-reader": {authn.PermReportRead},
	}}
	authz := authn.NewAuthorizer()
	authz.SetTableRoles(source.table)

	// An interval of zero disables the loop, so any load counted below came from the request
	// path and nowhere else.
	refresher := authn.NewRoleRefresher(authz, source, 0)
	user := authn.User{ID: "11111111-1111-1111-1111-111111111111", Status: "active", Roles: []string{"t-reader"}}

	for i := 0; i < 500; i++ {
		if !authz.Can(user, authn.PermReportRead) {
			t.Fatal("the cached permission is not effective")
		}
		if authz.Can(user, authn.PermLogRead) {
			t.Fatal("a permission outside the role is effective")
		}
		_ = authz.Permissions(user)
	}

	if got := source.calls(); got != 0 {
		t.Fatalf("%d loads during 500 permission checks, want 0", got)
	}
	if got := refresher.Loads(); got != 0 {
		t.Fatalf("%d refresher loads during permission checks, want 0", got)
	}

	// The explicit hook is what performs the load, and it is the only thing that does.
	if err := refresher.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if got := source.calls(); got != 1 {
		t.Fatalf("%d loads after one explicit refresh, want 1", got)
	}
}

// A failed refresh keeps the previous snapshot.
//
// The alternative — clearing the table on error — turns a database blip into "every custom
// role lost its permissions", which is an outage caused by a failed read.
func TestFailedRefreshKeepsTheLastSnapshot(t *testing.T) {
	source := &countingSource{table: map[authn.Role][]authn.Permission{
		"t-reader": {authn.PermReportRead},
	}}
	authz := authn.NewAuthorizer()
	refresher := authn.NewRoleRefresher(authz, source, 0)
	user := authn.User{ID: "11111111-1111-1111-1111-111111111111", Status: "active", Roles: []string{"t-reader"}}

	if err := refresher.Refresh(context.Background()); err != nil {
		t.Fatalf("first refresh: %v", err)
	}
	if !authz.Can(user, authn.PermReportRead) {
		t.Fatal("the loaded permission is not effective")
	}

	source.fail(errors.New("connection refused"))
	if err := refresher.Refresh(context.Background()); err == nil {
		t.Fatal("a failed load reported success")
	}
	if !authz.Can(user, authn.PermReportRead) {
		t.Fatal("a failed refresh dropped the permissions it could not replace")
	}
	if got := refresher.Applied(); got != 1 {
		t.Fatalf("%d applied snapshots, want 1 (the failed load must not count)", got)
	}
}

// Start is idempotent and Stop is safe to call twice, because a caller that runs setup more
// than once must not end up with two pollers fighting over one authorizer.
func TestRefresherStartAndStopAreIdempotent(t *testing.T) {
	source := &countingSource{table: map[authn.Role][]authn.Permission{}}
	refresher := authn.NewRoleRefresher(authn.NewAuthorizer(), source, 5*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	refresher.Start(ctx)
	refresher.Start(ctx)
	time.Sleep(30 * time.Millisecond)
	refresher.Stop()
	refresher.Stop()

	first := refresher.Loads()
	// Two loops would keep counting after the first Stop returned; one loop does not.
	time.Sleep(30 * time.Millisecond)
	if got := refresher.Loads(); got != first {
		t.Fatalf("%d loads after Stop, was %d: the loop is still running", got, first)
	}
	if first == 0 {
		t.Fatal("the loop never polled")
	}
}

// countingSource is a RolePermissionSource that counts its calls and can be made to fail.
type countingSource struct {
	mu    sync.Mutex
	loads int
	table map[authn.Role][]authn.Permission
	err   error
}

func (c *countingSource) AllRolePermissions(context.Context) (map[authn.Role][]authn.Permission, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.loads++
	if c.err != nil {
		return nil, c.err
	}
	out := make(map[authn.Role][]authn.Permission, len(c.table))
	for role, perms := range c.table {
		out[role] = append([]authn.Permission(nil), perms...)
	}
	return out, nil
}

func (c *countingSource) calls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.loads
}

func (c *countingSource) fail(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.err = err
}
