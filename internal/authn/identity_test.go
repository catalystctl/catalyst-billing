package authn_test

import (
	"context"
	"errors"
	"sort"
	"testing"
	"time"

	"github.com/catalyst/billing/internal/authn"
	"github.com/catalyst/billing/internal/store"
	"github.com/catalyst/billing/internal/store/storetest"
)

// identityFixture is a service plus a handful of users, for the session, impersonation and
// role tests.
//
// It is separate from the login fixture in authn_test.go because those tests care about
// password verification and these care about rows: the fastest way to a session here is to
// issue one through LoginWithID, not to hash a password.
type identityFixture struct {
	pool   *store.Pool
	svc    *authn.Service
	hasher *authn.Hasher
	now    time.Time
}

func newIdentityFixture(t *testing.T) *identityFixture {
	t.Helper()
	pool := storetest.DB(t)
	h := newHasher(t)
	f := &identityFixture{
		pool:   pool,
		hasher: h,
		now:    time.Date(2026, 4, 1, 10, 0, 0, 0, time.UTC),
	}
	cfg := authn.DefaultConfig()
	cfg.Now = func() time.Time { return f.now }
	svc, err := authn.New(pool, h, cfg)
	if err != nil {
		t.Fatalf("authn.New: %v", err)
	}
	f.svc = svc
	return f
}

// user inserts an active user and returns its id.
func (f *identityFixture) user(t *testing.T, email string) string {
	t.Helper()
	var id string
	if err := f.pool.QueryRow(context.Background(), `
		INSERT INTO users (email, password_hash, display_name, email_verified_at)
		VALUES ($1, 'x', 'Test', now()) RETURNING id`, email).Scan(&id); err != nil {
		t.Fatalf("create user %s: %v", email, err)
	}
	return id
}

// session issues a real session for a user.
func (f *identityFixture) session(t *testing.T, userID string) authn.LoginResult {
	t.Helper()
	res, err := f.svc.LoginWithID(context.Background(), userID, "203.0.113.5", "test-agent/1.0")
	if err != nil {
		t.Fatalf("login as %s: %v", userID, err)
	}
	return res
}

// sessions returns the live session ids for a user, for assertions.
func (f *identityFixture) sessionIDs(t *testing.T, userID string) []string {
	t.Helper()
	rows, err := f.pool.Query(context.Background(), `
		SELECT id::text FROM sessions
		WHERE user_id = $1 AND revoked_at IS NULL
		ORDER BY created_at`, userID)
	if err != nil {
		t.Fatalf("list sessions: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// createRole creates a custom role and removes it when the test ends.
//
// roles is configuration an operator edits, so the harness keeps it between tests rather than
// truncating it — the four seeded system roles must survive every truncate. That means a role
// created here outlives the test unless it is deleted, and a fixed name would then collide on
// the next run. Deriving the name from the test and deleting on cleanup makes the suite
// repeatable, which is the property that matters.
func createRole(t *testing.T, f *identityFixture, base, description string,
	perms []authn.Permission) authn.RoleRecord {
	t.Helper()
	name := "t-" + base
	rec, err := f.svc.CreateRole(context.Background(), name, description, perms)
	if err != nil {
		t.Fatalf("create role %s: %v", name, err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		// Unassign first: a held role is refused by design, and the cleanup must not mask
		// that rule.
		if _, err := f.pool.Exec(ctx, `DELETE FROM staff_roles WHERE role_id = $1`, rec.ID); err != nil {
			t.Logf("cleanup: unassign %s: %v", rec.Name, err)
		}
		if err := f.svc.DeleteRole(ctx, rec.ID); err != nil {
			t.Logf("cleanup: delete %s: %v", rec.Name, err)
		}
	})
	return rec
}

// ------------------------------------------------------------- sessions --

// Revoking one session must leave every other session working. This is the whole point of a
// per-session logout: a customer signing out a lost phone must not sign out their laptop.
func TestRevokeOneSessionLeavesTheOthers(t *testing.T) {
	f := newIdentityFixture(t)
	ctx := context.Background()
	userID := f.user(t, "multi@example.test")

	first := f.session(t, userID)
	second := f.session(t, userID)
	third := f.session(t, userID)

	if got := len(f.sessionIDs(t, userID)); got != 3 {
		t.Fatalf("%d live sessions, want 3", got)
	}

	if err := f.svc.RevokeSession(ctx, userID, second.Session.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	// The revoked one no longer authenticates.
	if _, _, err := f.svc.Authenticate(ctx, second.Token); !errors.Is(err, authn.ErrNoSession) {
		t.Fatalf("revoked session still authenticates: %v", err)
	}
	// The others still do.
	for name, res := range map[string]authn.LoginResult{"first": first, "third": third} {
		if _, _, err := f.svc.Authenticate(ctx, res.Token); err != nil {
			t.Fatalf("%s session died when another was revoked: %v", name, err)
		}
	}

	live := f.sessionIDs(t, userID)
	if len(live) != 2 {
		t.Fatalf("%d live sessions after one revoke, want 2: %v", len(live), live)
	}
}

// A session id belonging to somebody else must not be revocable, and must be reported
// exactly like an id that does not exist.
func TestRevokeSessionIsScopedToTheOwner(t *testing.T) {
	f := newIdentityFixture(t)
	ctx := context.Background()
	owner := f.user(t, "owner@example.test")
	other := f.user(t, "other@example.test")

	victim := f.session(t, owner)

	err := f.svc.RevokeSession(ctx, other, victim.Session.ID)
	if !errors.Is(err, authn.ErrNoSessionRow) {
		t.Fatalf("cross-user revoke: got %v, want ErrNoSessionRow", err)
	}
	// The session must still work: a rejected revoke that nevertheless revoked would be
	// the worst of both.
	if _, _, err := f.svc.Authenticate(ctx, victim.Token); err != nil {
		t.Fatalf("the session was revoked by a caller who does not own it: %v", err)
	}
}

// "Sign out everywhere else" must keep the session that asked, or the security action signs
// the user out of the screen they are standing on.
func TestRevokeOthersKeepsTheCurrentSession(t *testing.T) {
	f := newIdentityFixture(t)
	ctx := context.Background()
	userID := f.user(t, "everywhere@example.test")

	keep := f.session(t, userID)
	other1 := f.session(t, userID)
	other2 := f.session(t, userID)

	n, err := f.svc.RevokeOtherSessions(ctx, userID, keep.Session.ID)
	if err != nil {
		t.Fatalf("revoke others: %v", err)
	}
	if n != 2 {
		t.Fatalf("revoked %d sessions, want 2", n)
	}

	if _, _, err := f.svc.Authenticate(ctx, keep.Token); err != nil {
		t.Fatalf("the current session was revoked by revoke-others: %v", err)
	}
	for name, res := range map[string]authn.LoginResult{"other1": other1, "other2": other2} {
		if _, _, err := f.svc.Authenticate(ctx, res.Token); !errors.Is(err, authn.ErrNoSession) {
			t.Fatalf("%s survived revoke-others: %v", name, err)
		}
	}
}

// The list marks the caller's own session and never exposes a token.
func TestSessionListMarksCurrentAndHidesSecrets(t *testing.T) {
	f := newIdentityFixture(t)
	ctx := context.Background()
	userID := f.user(t, "list@example.test")

	current := f.session(t, userID)
	f.session(t, userID)

	sessions, err := f.svc.Sessions(ctx, userID, current.Session.ID)
	if err != nil {
		t.Fatalf("sessions: %v", err)
	}
	if len(sessions) != 2 {
		t.Fatalf("%d sessions, want 2", len(sessions))
	}
	currentCount := 0
	for _, info := range sessions {
		if info.Current {
			currentCount++
			if info.ID != current.Session.ID {
				t.Fatalf("the wrong session is marked current")
			}
		}
		if info.IP == "" || info.UserAgent == "" {
			t.Fatalf("session metadata is missing: %+v", info)
		}
	}
	if currentCount != 1 {
		t.Fatalf("%d sessions marked current, want 1", currentCount)
	}

	// A revoked session is history, not a live session.
	if err := f.svc.RevokeSession(ctx, userID, current.Session.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	sessions, err = f.svc.Sessions(ctx, userID, "")
	if err != nil {
		t.Fatalf("sessions: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("%d sessions after revoking one, want 1", len(sessions))
	}
}

// The login history is the caller's own and is paginated without gaps.
func TestLoginHistoryIsPaginatedAndScoped(t *testing.T) {
	f := newIdentityFixture(t)
	ctx := context.Background()
	userID := f.user(t, "history@example.test")
	otherID := f.user(t, "history-other@example.test")

	for i := 0; i < 3; i++ {
		if _, err := f.pool.Exec(ctx,
			`INSERT INTO auth_events (user_id, event, detail) VALUES ($1, 'login_succeeded', $2)`,
			userID, "attempt"); err != nil {
			t.Fatalf("insert auth event: %v", err)
		}
	}
	if _, err := f.pool.Exec(ctx,
		`INSERT INTO auth_events (user_id, event) VALUES ($1, 'login_succeeded')`, otherID); err != nil {
		t.Fatalf("insert other auth event: %v", err)
	}

	first, err := f.svc.LoginHistory(ctx, userID, 2, "")
	if err != nil {
		t.Fatalf("login history: %v", err)
	}
	if len(first.Events) != 2 {
		t.Fatalf("%d events on page one, want 2", len(first.Events))
	}
	if first.NextCursor == "" {
		t.Fatal("a full page produced no cursor")
	}

	second, err := f.svc.LoginHistory(ctx, userID, 2, first.NextCursor)
	if err != nil {
		t.Fatalf("login history page two: %v", err)
	}
	if len(second.Events) != 1 {
		t.Fatalf("%d events on page two, want 1", len(second.Events))
	}
	if second.NextCursor != "" {
		t.Fatal("the last page produced a cursor")
	}
	// No event may appear twice across the two pages.
	for _, a := range first.Events {
		for _, b := range second.Events {
			if a.ID == b.ID {
				t.Fatalf("event %d appears on both pages", a.ID)
			}
		}
	}
	// The other user's event must never appear.
	for _, ev := range append(first.Events, second.Events...) {
		if ev.Detail == "" && ev.Event == "login_succeeded" && ev.ID == 4 {
			t.Fatal("the history leaked another user's event")
		}
	}
}

// -------------------------------------------------------- impersonation --

// An impersonated session is a session for the target with the operator recorded, and
// stopping it returns the operator to their own identity.
func TestImpersonationMarksSessionAndReturnsToOperator(t *testing.T) {
	f := newIdentityFixture(t)
	ctx := context.Background()

	operatorID := f.user(t, "admin@example.test")
	if _, err := f.pool.Exec(ctx,
		`INSERT INTO staff_roles (user_id, role) VALUES ($1, 'administrator')`, operatorID); err != nil {
		t.Fatalf("grant role: %v", err)
	}
	targetID := f.user(t, "customer@example.test")

	operatorSession := f.session(t, operatorID)

	res, err := f.svc.Impersonate(ctx, operatorID, operatorSession.Session.ID, targetID,
		"198.51.100.7", "operator-browser")
	if err != nil {
		t.Fatalf("impersonate: %v", err)
	}
	if res.Session.UserID != targetID {
		t.Fatalf("the impersonated session belongs to %s, want %s", res.Session.UserID, targetID)
	}
	if res.Operator.ID != operatorID || res.Target.ID != targetID {
		t.Fatalf("operator/target mixed up: %+v", res)
	}

	// The session authenticates as the target...
	sess, user, err := f.svc.Authenticate(ctx, res.Token)
	if err != nil {
		t.Fatalf("authenticate impersonated: %v", err)
	}
	if user.ID != targetID {
		t.Fatalf("authenticated as %s, want the target %s", user.ID, targetID)
	}
	// ...and is marked with the operator.
	impersonated, by, err := f.svc.IsImpersonated(ctx, sess.ID)
	if err != nil {
		t.Fatalf("is impersonated: %v", err)
	}
	if !impersonated || by != operatorID {
		t.Fatalf("session not marked: impersonated=%v by=%q", impersonated, by)
	}
	// The list shows it too, which is what the UI banner reads.
	list, err := f.svc.Sessions(ctx, targetID, sess.ID)
	if err != nil {
		t.Fatalf("sessions: %v", err)
	}
	if len(list) != 1 || list[0].ImpersonatedBy != operatorID {
		t.Fatalf("the session list does not report the impersonator: %+v", list)
	}

	// The operator's own session is untouched while impersonating.
	if _, _, err := f.svc.Authenticate(ctx, operatorSession.Token); err != nil {
		t.Fatalf("the operator's own session was consumed: %v", err)
	}

	// Stopping ends the borrowed session and returns the operator.
	stop, err := f.svc.StopImpersonation(ctx, targetID, sess.ID, "198.51.100.7")
	if err != nil {
		t.Fatalf("stop: %v", err)
	}
	if stop.OperatorID != operatorID || stop.User.ID != operatorID {
		t.Fatalf("stop returned %+v, want the operator", stop)
	}
	if _, _, err := f.svc.Authenticate(ctx, res.Token); !errors.Is(err, authn.ErrNoSession) {
		t.Fatalf("the impersonated session survived stop: %v", err)
	}
	// The rotated operator token works, and the old one does not.
	if _, _, err := f.svc.Authenticate(ctx, stop.Token); err != nil {
		t.Fatalf("the operator's new token does not work: %v", err)
	}
	if _, _, err := f.svc.Authenticate(ctx, operatorSession.Token); !errors.Is(err, authn.ErrNoSession) {
		t.Fatalf("the operator's pre-impersonation token still works: %v", err)
	}
}

// Impersonating a staff account would hand one role another role's permissions.
func TestImpersonationRefusesStaffTarget(t *testing.T) {
	f := newIdentityFixture(t)
	ctx := context.Background()

	operatorID := f.user(t, "admin2@example.test")
	if _, err := f.pool.Exec(ctx,
		`INSERT INTO staff_roles (user_id, role) VALUES ($1, 'administrator')`, operatorID); err != nil {
		t.Fatalf("grant role: %v", err)
	}
	targetID := f.user(t, "billing2@example.test")
	if _, err := f.pool.Exec(ctx,
		`INSERT INTO staff_roles (user_id, role) VALUES ($1, 'billing')`, targetID); err != nil {
		t.Fatalf("grant role: %v", err)
	}

	sess := f.session(t, operatorID)
	if _, err := f.svc.Impersonate(ctx, operatorID, sess.Session.ID, targetID, "", ""); !errors.Is(err, authn.ErrForbidden) {
		t.Fatalf("impersonating staff: got %v, want ErrForbidden", err)
	}
}

func TestImpersonationRefusesSelf(t *testing.T) {
	f := newIdentityFixture(t)
	ctx := context.Background()

	operatorID := f.user(t, "self@example.test")
	if _, err := f.pool.Exec(ctx,
		`INSERT INTO staff_roles (user_id, role) VALUES ($1, 'administrator')`, operatorID); err != nil {
		t.Fatalf("grant role: %v", err)
	}
	sess := f.session(t, operatorID)
	if _, err := f.svc.Impersonate(ctx, operatorID, sess.Session.ID, operatorID, "", ""); !errors.Is(err, authn.ErrSelfImpersonation) {
		t.Fatalf("self impersonation: got %v, want ErrSelfImpersonation", err)
	}
}

// Stopping a normal session is a logout, not a stop-impersonating.
func TestStopImpersonationRefusesNormalSession(t *testing.T) {
	f := newIdentityFixture(t)
	ctx := context.Background()
	userID := f.user(t, "normal@example.test")
	sess := f.session(t, userID)

	if _, err := f.svc.StopImpersonation(ctx, userID, sess.Session.ID, ""); !errors.Is(err, authn.ErrImpersonationNotAllowed) {
		t.Fatalf("stop on a normal session: got %v, want ErrImpersonationNotAllowed", err)
	}
}

// ----------------------------------------------------------------- roles --

// A custom role grants exactly the subset it was given: nothing more, and nothing less.
func TestRoleGrantsExactlyItsSubset(t *testing.T) {
	f := newIdentityFixture(t)
	ctx := context.Background()

	rec := createRole(t, f, "reports", "Reads reports, nothing else",
		[]authn.Permission{authn.PermReportRead, authn.PermCustomerReadAll})
	if rec.System {
		t.Fatal("a created role is marked as a system role")
	}

	userID := f.user(t, "reports@example.test")
	if _, err := f.pool.Exec(ctx,
		`INSERT INTO staff_roles (user_id, role_id) VALUES ($1, $2)`, userID, rec.ID); err != nil {
		t.Fatalf("assign role: %v", err)
	}

	perms, err := f.svc.PermissionsForUser(ctx, userID)
	if err != nil {
		t.Fatalf("permissions: %v", err)
	}

	// Exactly the two granted staff permissions are present.
	var staffPerms []string
	for p := range perms {
		switch p {
		case authn.PermCustomerReadSelf, authn.PermCustomerWriteSelf,
			authn.PermInvoiceReadSelf, authn.PermInvoicePaySelf,
			authn.PermServiceReadSelf, authn.PermServiceCancelSelf,
			authn.PermServiceUpgradeSelf, authn.PermTicketReadSelf, authn.PermTicketWriteSelf:
			continue // the customer baseline every signed-in user holds
		}
		staffPerms = append(staffPerms, string(p))
	}
	sort.Strings(staffPerms)
	want := []string{"customer:read:all", "report:read"}
	if len(staffPerms) != len(want) || staffPerms[0] != want[0] || staffPerms[1] != want[1] {
		t.Fatalf("staff permissions = %v, want %v", staffPerms, want)
	}

	// A permission the role does not hold is absent, not merely unenforced.
	if _, ok := perms[authn.PermPaymentRefund]; ok {
		t.Fatal("a permission outside the subset was granted")
	}

	// Narrowing the role narrows the user immediately: permissions are read from the table.
	if _, err := f.svc.UpdateRolePermissions(ctx, rec.ID, []authn.Permission{authn.PermCustomerReadAll}); err != nil {
		t.Fatalf("update permissions: %v", err)
	}
	perms, err = f.svc.PermissionsForUser(ctx, userID)
	if err != nil {
		t.Fatalf("permissions: %v", err)
	}
	if _, ok := perms[authn.PermReportRead]; ok {
		t.Fatal("a removed permission is still granted")
	}
}

// A permission this build does not declare must be refused, not stored.
func TestRoleRejectsUndeclaredPermission(t *testing.T) {
	f := newIdentityFixture(t)
	ctx := context.Background()

	_, err := f.svc.CreateRole(ctx, "t-bogus", "", []authn.Permission{"invoice:delete:everything"})
	if !errors.Is(err, authn.ErrInvalidRole) {
		t.Fatalf("undeclared permission: got %v, want ErrInvalidRole", err)
	}
}

// The four roles the code's separation of duties depends on cannot be deleted or edited.
func TestSystemRoleCannotBeDeletedOrEdited(t *testing.T) {
	f := newIdentityFixture(t)
	ctx := context.Background()

	roles, err := f.svc.ListRoles(ctx)
	if err != nil {
		t.Fatalf("list roles: %v", err)
	}
	systemCount := 0
	for _, rec := range roles {
		if !rec.System {
			continue
		}
		systemCount++
		if !authn.IsStaffRole(rec.Name) {
			t.Fatalf("system role %q is not a declared role", rec.Name)
		}
		if err := f.svc.DeleteRole(ctx, rec.ID); !errors.Is(err, authn.ErrSystemRole) {
			t.Fatalf("delete %s: got %v, want ErrSystemRole", rec.Name, err)
		}
		if _, err := f.svc.UpdateRolePermissions(ctx, rec.ID, []authn.Permission{authn.PermPaymentRefund}); !errors.Is(err, authn.ErrSystemRole) {
			t.Fatalf("edit %s: got %v, want ErrSystemRole", rec.Name, err)
		}
	}
	if systemCount != 4 {
		t.Fatalf("%d system roles, want 4", systemCount)
	}

	// The database refuses too: a bug that bypasses the service must still fail.
	admin, err := f.svc.RoleByName(ctx, "administrator")
	if err != nil {
		t.Fatalf("role by name: %v", err)
	}
	if _, err := f.pool.Exec(ctx, `DELETE FROM roles WHERE id = $1`, admin.ID); err == nil {
		t.Fatal("the database allowed a system role to be deleted")
	}
	if _, err := f.pool.Exec(ctx,
		`DELETE FROM role_permissions WHERE role_id = $1 AND permission = 'payment:refund'`,
		admin.ID); err == nil {
		t.Fatal("the database allowed a system role's permissions to be edited")
	}
}

// The seeded permission sets must equal the code map, or an existing staff member's access
// would change silently on deploy.
func TestSeededSystemRolesMatchTheCodeMap(t *testing.T) {
	f := newIdentityFixture(t)
	ctx := context.Background()

	for _, name := range authn.AllRoles() {
		rec, err := f.svc.RoleByName(ctx, string(name))
		if err != nil {
			t.Fatalf("role %s: %v", name, err)
		}
		if !rec.System {
			t.Fatalf("%s is not marked as a system role", name)
		}

		// The table copy, read directly, must match what the service reports.
		rows, err := f.pool.Query(ctx,
			`SELECT permission FROM role_permissions WHERE role_id = $1 ORDER BY permission`, rec.ID)
		if err != nil {
			t.Fatalf("read seeded permissions: %v", err)
		}
		var seeded []string
		for rows.Next() {
			var p string
			if err := rows.Scan(&p); err != nil {
				t.Fatalf("scan: %v", err)
			}
			seeded = append(seeded, p)
		}
		rows.Close()

		reported := make([]string, 0, len(rec.Permissions))
		for _, p := range rec.Permissions {
			reported = append(reported, string(p))
		}
		sort.Strings(reported)
		if len(seeded) != len(reported) {
			t.Fatalf("%s: %d seeded permissions, %d declared", name, len(seeded), len(reported))
		}
		for i := range seeded {
			if seeded[i] != reported[i] {
				t.Fatalf("%s: seeded %q, declared %q", name, seeded[i], reported[i])
			}
		}
	}
}

// A role somebody holds cannot be deleted: a cascade would strip their access silently.
func TestRoleInUseCannotBeDeleted(t *testing.T) {
	f := newIdentityFixture(t)
	ctx := context.Background()

	rec := createRole(t, f, "temp", "", []authn.Permission{authn.PermReportRead})
	userID := f.user(t, "holder@example.test")
	if _, err := f.pool.Exec(ctx,
		`INSERT INTO staff_roles (user_id, role_id) VALUES ($1, $2)`, userID, rec.ID); err != nil {
		t.Fatalf("assign role: %v", err)
	}

	if err := f.svc.DeleteRole(ctx, rec.ID); !errors.Is(err, authn.ErrRoleInUse) {
		t.Fatalf("delete a held role: got %v, want ErrRoleInUse", err)
	}

	if _, err := f.pool.Exec(ctx, `DELETE FROM staff_roles WHERE user_id = $1`, userID); err != nil {
		t.Fatalf("unassign: %v", err)
	}
	if err := f.svc.DeleteRole(ctx, rec.ID); err != nil {
		t.Fatalf("delete an unheld role: %v", err)
	}
}

// A reserved name cannot be re-created as a custom role: two roles named "administrator"
// would make the legacy staff_roles.role mirror ambiguous.
func TestRoleNameIsReservedForSystemRoles(t *testing.T) {
	f := newIdentityFixture(t)
	ctx := context.Background()

	if _, err := f.svc.CreateRole(ctx, "administrator", "", nil); !errors.Is(err, authn.ErrInvalidRole) {
		t.Fatalf("reserved name: got %v, want ErrInvalidRole", err)
	}
}

// The legacy role column and role_id must not be able to disagree, whichever one a writer
// sets: internal/authn/session.go reads `role`, and the resolver reads `role_id`.
func TestStaffRoleColumnsStayInStep(t *testing.T) {
	f := newIdentityFixture(t)
	ctx := context.Background()

	userA := f.user(t, "mirror-a@example.test")
	if _, err := f.pool.Exec(ctx,
		`INSERT INTO staff_roles (user_id, role) VALUES ($1, 'support')`, userA); err != nil {
		t.Fatalf("insert by name: %v", err)
	}
	var (
		mirrorName string
		mirrorID   string
	)
	if err := f.pool.QueryRow(ctx,
		`SELECT role, role_id::text FROM staff_roles WHERE user_id = $1`, userA).
		Scan(&mirrorName, &mirrorID); err != nil {
		t.Fatalf("read: %v", err)
	}
	if mirrorName != "support" || mirrorID == "" {
		t.Fatalf("role_id was not backfilled: role=%q role_id=%q", mirrorName, mirrorID)
	}

	// Writing role_id alone must fill in the name.
	rec := createRole(t, f, "ops", "", []authn.Permission{authn.PermServiceReadAll})
	opsName := rec.Name
	userB := f.user(t, "mirror-b@example.test")
	if _, err := f.pool.Exec(ctx,
		`INSERT INTO staff_roles (user_id, role_id) VALUES ($1, $2)`, userB, rec.ID); err != nil {
		t.Fatalf("insert by id: %v", err)
	}
	if err := f.pool.QueryRow(ctx,
		`SELECT role FROM staff_roles WHERE user_id = $1`, userB).Scan(&mirrorName); err != nil {
		t.Fatalf("read: %v", err)
	}
	if mirrorName != opsName {
		t.Fatalf("role = %q, want the name derived from role_id (%q)", mirrorName, opsName)
	}
}
