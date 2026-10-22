package authn_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/catalyst/billing/internal/authn"
	"github.com/catalyst/billing/internal/store"
	"github.com/jackc/pgx/v5"
)

// Staff user administration, at the domain level.
//
// These tests exist because the screen that creates a login is the screen that can also take
// the panel's last administrator away. The properties asserted here are the ones a handler
// test cannot reach: that the refusal survives two concurrent demotions, that it is the
// database that refuses rather than a check that raced, and that a password set by an
// operator obeys the policy registration applies.

// createAs creates an account through the service, which is what the screen does.
func createAs(t *testing.T, f *identityFixture, email, role string) authn.UserRecord {
	t.Helper()
	rec, err := f.svc.CreateUser(context.Background(), authn.CreateUserRequest{
		Email:         email,
		DisplayName:   "Test Person",
		Password:      "a sufficiently long password",
		Role:          role,
		EmailVerified: true,
	}, nil)
	if err != nil {
		t.Fatalf("create %s (%s): %v", email, role, err)
	}
	return rec
}

// activeAdminCount counts the active administrators, straight from the table.
func activeAdminCount(t *testing.T, f *identityFixture) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(context.Background(), `
		SELECT count(*) FROM staff_roles sr JOIN users u ON u.id = sr.user_id
		WHERE sr.role = 'administrator' AND u.status = 'active'`).Scan(&n); err != nil {
		t.Fatalf("count administrators: %v", err)
	}
	return n
}

// A staff user created through the service can sign in, and the role it was created with is
// the role the session carries. This is the headline gap: before it, only the CLI could do
// this.
func TestCreatedStaffUserCanSignIn(t *testing.T) {
	f := newIdentityFixture(t)

	created := createAs(t, f, "agent@example.test", "support")

	res, err := f.svc.Login(context.Background(), authn.LoginRequest{
		Email:    "agent@example.test",
		Password: "a sufficiently long password",
		IP:       "203.0.113.9",
	})
	if err != nil {
		t.Fatalf("the created account could not sign in: %v", err)
	}
	if res.User.ID != created.ID {
		t.Fatalf("signed in as %s, want %s", res.User.ID, created.ID)
	}
	if len(res.User.Roles) != 1 || res.User.Roles[0] != "support" {
		t.Fatalf("roles = %v, want [support]", res.User.Roles)
	}
	if !authn.NewAuthorizer().Can(res.User, authn.PermTicketReadAll) {
		t.Fatal("a support agent created here cannot read tickets")
	}
}

// Each of the four roles must produce the permissions the code declares for it — and, just as
// importantly, not the ones the separation of duties withholds. A create path that wrote a
// role name the resolver did not recognise would produce a staff account with no staff
// permissions, which looks like a working account and is not one.
func TestCreatedUserGetsTheRoleItWasGiven(t *testing.T) {
	f := newIdentityFixture(t)
	authz := authn.NewAuthorizer()

	cases := []struct {
		role    string
		holds   []authn.Permission
		without []authn.Permission
	}{
		{
			role:    "administrator",
			holds:   []authn.Permission{authn.PermStaffManage, authn.PermPaymentRefund, authn.PermServiceTerminate},
			without: nil,
		},
		{
			role:    "billing",
			holds:   []authn.Permission{authn.PermInvoiceWriteAll, authn.PermPaymentRefund},
			without: []authn.Permission{authn.PermServiceTerminate, authn.PermStaffManage},
		},
		{
			role:    "support",
			holds:   []authn.Permission{authn.PermServiceHold, authn.PermTicketWriteAll},
			without: []authn.Permission{authn.PermPaymentRefund, authn.PermServiceTerminate, authn.PermStaffManage},
		},
		{
			role:    "infrastructure",
			holds:   []authn.Permission{authn.PermServiceTerminate, authn.PermServiceProvision},
			without: []authn.Permission{authn.PermPaymentRefund, authn.PermStaffManage},
		},
	}

	for _, tc := range cases {
		t.Run(tc.role, func(t *testing.T) {
			email := tc.role + "-created@example.test"
			createAs(t, f, email, tc.role)

			res, err := f.svc.Login(context.Background(), authn.LoginRequest{
				Email:    email,
				Password: "a sufficiently long password",
				IP:       "203.0.113.10",
			})
			if err != nil {
				t.Fatalf("sign in: %v", err)
			}
			for _, p := range tc.holds {
				if !authz.Can(res.User, p) {
					t.Errorf("%s does not hold %s", tc.role, p)
				}
			}
			for _, p := range tc.without {
				if authz.Can(res.User, p) {
					t.Errorf("%s holds %s, which the separation of duties withholds", tc.role, p)
				}
			}
		})
	}
}

// A customer account is the same act with no role, and it must not be staff.
func TestCreatedCustomerIsNotStaff(t *testing.T) {
	f := newIdentityFixture(t)
	rec := createAs(t, f, "customer@example.test", "")
	if rec.IsStaff() {
		t.Fatalf("role = %q, want a customer account", rec.Role)
	}
}

// A duplicate address is refused with its own error, so the screen can say so. Registration
// deliberately hides this; an operator typing an address needs to know.
func TestCreateUserRefusesDuplicateEmail(t *testing.T) {
	f := newIdentityFixture(t)
	createAs(t, f, "twice@example.test", "support")

	_, err := f.svc.CreateUser(context.Background(), authn.CreateUserRequest{
		Email:         "TWICE@example.test",
		DisplayName:   "Second",
		Password:      "a sufficiently long password",
		EmailVerified: true,
	}, nil)
	if !errors.Is(err, authn.ErrEmailTaken) {
		t.Fatalf("duplicate email: got %v, want ErrEmailTaken", err)
	}
}

// A weak password is refused by the same validator registration uses, so it is the same
// error — and therefore the same `weak_password` code over HTTP.
func TestCreateUserRefusesWeakPassword(t *testing.T) {
	f := newIdentityFixture(t)
	_, err := f.svc.CreateUser(context.Background(), authn.CreateUserRequest{
		Email:         "weak@example.test",
		DisplayName:   "Weak",
		Password:      "short",
		EmailVerified: true,
	}, nil)
	if !errors.Is(err, authn.ErrWeakPassword) {
		t.Fatalf("weak password: got %v, want ErrWeakPassword", err)
	}

	// And nothing was written: a refused password must not leave a half-created account.
	var n int
	if err := f.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM users WHERE lower(email) = 'weak@example.test'`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("%d account(s) exist after a refused password", n)
	}
}

// A role that is not in the table is refused rather than written: `staff_roles.role` is a
// foreign key, so the alternative is a 500 from a constraint.
func TestCreateUserRefusesUnknownRole(t *testing.T) {
	f := newIdentityFixture(t)
	_, err := f.svc.CreateUser(context.Background(), authn.CreateUserRequest{
		Email:         "norole@example.test",
		DisplayName:   "No Role",
		Password:      "a sufficiently long password",
		Role:          "superuser",
		EmailVerified: true,
	}, nil)
	if !errors.Is(err, authn.ErrUnknownRole) {
		t.Fatalf("unknown role: got %v, want ErrUnknownRole", err)
	}
}

// The single most damaging change the screen can make: demoting the only active
// administrator.
func TestOperatorCannotDemoteTheLastAdministrator(t *testing.T) {
	f := newIdentityFixture(t)
	only := createAs(t, f, "only-admin@example.test", "administrator")

	_, err := f.svc.UpdateUser(context.Background(), only.ID, authn.UpdateUserRequest{
		Role:    ptr("support"),
		ActorID: only.ID,
	}, nil)
	if !errors.Is(err, authn.ErrLastAdministrator) {
		t.Fatalf("demoting the last administrator: got %v, want ErrLastAdministrator", err)
	}
	if got := activeAdminCount(t, f); got != 1 {
		t.Fatalf("active administrators = %d, want 1", got)
	}
}

// Disabling the last administrator is the same act as demoting them, and it is guarded by the
// database as well: the trigger on users.status refuses it even when the service is bypassed.
func TestLastAdministratorCannotBeDisabled(t *testing.T) {
	f := newIdentityFixture(t)
	only := createAs(t, f, "only-admin@example.test", "administrator")

	_, err := f.svc.UpdateUser(context.Background(), only.ID, authn.UpdateUserRequest{
		Status:  ptr("disabled"),
		ActorID: "00000000-0000-0000-0000-000000000000",
	}, nil)
	if !errors.Is(err, authn.ErrLastAdministrator) {
		t.Fatalf("disabling the last administrator: got %v, want ErrLastAdministrator", err)
	}

	// Straight SQL, no service: the database is the authority.
	_, err = f.pool.Exec(context.Background(),
		`UPDATE users SET status = 'disabled' WHERE id = $1`, only.ID)
	if store.SQLState(err) != store.StateRestrictViolation {
		t.Fatalf("raw disable: got %v (state %q), want a restrict violation",
			err, store.SQLState(err))
	}
}

// The race the guard exists for: two operators demoting two administrators at the same
// moment. Without the advisory lock inside the trigger, both transactions count the other
// administrator as still present and both commit, leaving none.
//
// The two transactions are interleaved deliberately: each holds its transaction open after
// the UPDATE, so both statements have been issued before either commits. The first to reach
// the trigger takes the lock; the second blocks until the first commits and then counts the
// committed result.
func TestConcurrentDemotionsCannotBothSucceed(t *testing.T) {
	f := newIdentityFixture(t)
	first := createAs(t, f, "first-admin@example.test", "administrator")
	second := createAs(t, f, "second-admin@example.test", "administrator")

	if got := activeAdminCount(t, f); got != 2 {
		t.Fatalf("active administrators = %d, want 2 before the race", got)
	}

	demote := func(userID string, hold time.Duration) error {
		ctx := context.Background()
		tx, err := f.pool.Begin(ctx)
		if err != nil {
			return fmt.Errorf("begin: %w", err)
		}
		defer store.Rollback(ctx, tx)
		if _, err := tx.Exec(ctx,
			`UPDATE staff_roles SET role = 'support' WHERE user_id = $1`, userID); err != nil {
			return err
		}
		// Keep the transaction open so both statements are in flight at once.
		time.Sleep(hold)
		return tx.Commit(ctx)
	}

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		results []error
	)
	start := make(chan struct{})
	for _, id := range []string{first.ID, second.ID} {
		wg.Add(1)
		go func(userID string) {
			defer wg.Done()
			<-start
			err := demote(userID, 150*time.Millisecond)
			mu.Lock()
			results = append(results, err)
			mu.Unlock()
		}(id)
	}
	close(start)
	wg.Wait()

	succeeded := 0
	refused := 0
	for _, err := range results {
		switch {
		case err == nil:
			succeeded++
		case store.SQLState(err) == store.StateRestrictViolation:
			refused++
		default:
			t.Fatalf("unexpected error from a concurrent demotion: %v", err)
		}
	}
	if succeeded != 1 || refused != 1 {
		t.Fatalf("%d demotion(s) succeeded and %d were refused, want exactly one of each "+
			"(both succeeding would leave the panel with no administrator)", succeeded, refused)
	}
	if got := activeAdminCount(t, f); got != 1 {
		t.Fatalf("active administrators = %d after the race, want 1", got)
	}
}

// An operator cannot disable the account they are signed in as, even when another
// administrator exists: locking yourself out is never the intended result of a click.
func TestOperatorCannotDisableThemselves(t *testing.T) {
	f := newIdentityFixture(t)
	me := createAs(t, f, "me@example.test", "administrator")
	other := createAs(t, f, "other@example.test", "administrator")

	_, err := f.svc.UpdateUser(context.Background(), me.ID, authn.UpdateUserRequest{
		Status:  ptr("disabled"),
		ActorID: me.ID,
	}, nil)
	if !errors.Is(err, authn.ErrSelfLockout) {
		t.Fatalf("disabling yourself: got %v, want ErrSelfLockout", err)
	}

	// The same change against somebody else is allowed, because two administrators remain.
	if _, err := f.svc.UpdateUser(context.Background(), other.ID, authn.UpdateUserRequest{
		Status:  ptr("disabled"),
		ActorID: me.ID,
	}, nil); err != nil {
		t.Fatalf("disabling another administrator: %v", err)
	}
}

// A disabled account's existing session stops working. The session row is revoked and the
// status check refuses it even if it were not.
func TestDisablingAnAccountEndsItsSessions(t *testing.T) {
	f := newIdentityFixture(t)
	admin := createAs(t, f, "admin@example.test", "administrator")
	target := createAs(t, f, "target@example.test", "support")

	res, err := f.svc.Login(context.Background(), authn.LoginRequest{
		Email:    "target@example.test",
		Password: "a sufficiently long password",
		IP:       "203.0.113.20",
	})
	if err != nil {
		t.Fatalf("sign in: %v", err)
	}
	if _, _, err := f.svc.Authenticate(context.Background(), res.Token); err != nil {
		t.Fatalf("the fresh session does not authenticate: %v", err)
	}

	if _, err := f.svc.UpdateUser(context.Background(), target.ID, authn.UpdateUserRequest{
		Status:  ptr("disabled"),
		ActorID: admin.ID,
	}, nil); err != nil {
		t.Fatalf("disable: %v", err)
	}

	if _, _, err := f.svc.Authenticate(context.Background(), res.Token); !errors.Is(err, authn.ErrNoSession) {
		t.Fatalf("a disabled account's session still authenticates: %v", err)
	}
	if ids := f.sessionIDs(t, target.ID); len(ids) != 0 {
		t.Fatalf("%d session(s) are still live for a disabled account", len(ids))
	}
}

// Deleting an account with billing history is refused, and the refusal names what holds it
// rather than surfacing a foreign-key error. This is the "do not cascade a customer's billing
// history away" rule.
func TestDeleteUserWithInvoicesIsRefused(t *testing.T) {
	f := newIdentityFixture(t)
	ctx := context.Background()

	var customerID string
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO customers (email, name) VALUES ('payer@example.test', 'Payer') RETURNING id`).
		Scan(&customerID); err != nil {
		t.Fatalf("create customer: %v", err)
	}
	// The create path adopts a customer record that already has this address, which is how a
	// phone customer gets a login.
	rec := createAs(t, f, "payer@example.test", "")
	if rec.CustomerID != customerID {
		t.Fatalf("customer_id = %q, want the existing record %q", rec.CustomerID, customerID)
	}
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO invoices (customer_id, status, currency, subtotal_minor, tax_minor, total_minor)
		VALUES ($1, 'draft', 'USD', 1000, 0, 1000)`, customerID); err != nil {
		t.Fatalf("create invoice: %v", err)
	}

	admin := createAs(t, f, "admin@example.test", "administrator")
	err := f.svc.DeleteUser(ctx, rec.ID, admin.ID, nil)
	if !errors.Is(err, authn.ErrUserHasHistory) {
		t.Fatalf("deleting an account with an invoice: got %v, want ErrUserHasHistory", err)
	}
	if !strings.Contains(err.Error(), "invoice") {
		t.Fatalf("the refusal does not say what holds the account: %v", err)
	}
}

// An account named by the audit trail cannot be deleted either: the trail is append-only, so
// the reference cannot be cleared and the row has to outlive the account.
func TestDeleteUserNamedInTheAuditTrailIsRefused(t *testing.T) {
	f := newIdentityFixture(t)
	ctx := context.Background()

	rec := createAs(t, f, "audited@example.test", "support")
	admin := createAs(t, f, "admin@example.test", "administrator")
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO audit_events (actor_id, actor_kind, action, target_type, outcome)
		VALUES ($1, 'user', 'invoice.void', 'invoice', 'success')`, rec.ID); err != nil {
		t.Fatalf("write audit row: %v", err)
	}

	err := f.svc.DeleteUser(ctx, rec.ID, admin.ID, nil)
	if !errors.Is(err, authn.ErrUserHasHistory) {
		t.Fatalf("deleting an audited account: got %v, want ErrUserHasHistory", err)
	}
}

// An account with no history can be deleted, which is the only case where delete is the right
// answer — a mistyped address created a minute ago.
func TestDeleteUserWithoutHistoryWorks(t *testing.T) {
	f := newIdentityFixture(t)
	ctx := context.Background()
	rec := createAs(t, f, "typo@example.test", "support")
	admin := createAs(t, f, "admin@example.test", "administrator")

	if err := f.svc.DeleteUser(ctx, rec.ID, admin.ID, nil); err != nil {
		t.Fatalf("deleting an unused account: %v", err)
	}
	if _, err := f.svc.UserByIDRecord(ctx, rec.ID); !errors.Is(err, authn.ErrUserNotFound) {
		t.Fatalf("the account still exists: %v", err)
	}

	// The billing identity created for the account goes with it. It holds nothing but an
	// address, and leaving it behind would put a customer nobody can reach into the customer
	// list — which is the state an operator would then have to clean up by hand.
	var orphans int
	if err := f.pool.QueryRow(ctx,
		`SELECT count(*) FROM customers WHERE lower(email) = 'typo@example.test'`).Scan(&orphans); err != nil {
		t.Fatalf("count: %v", err)
	}
	if orphans != 0 {
		t.Fatalf("%d billing record(s) survived the account, want none", orphans)
	}
}

// Setting a password from the screen sets the password, ends every session the account had,
// and obeys the same policy as registration.
func TestSetPasswordByOperatorRevokesSessionsAndUsesThePolicy(t *testing.T) {
	f := newIdentityFixture(t)
	ctx := context.Background()

	admin := createAs(t, f, "admin@example.test", "administrator")
	target := createAs(t, f, "target@example.test", "support")

	res, err := f.svc.Login(ctx, authn.LoginRequest{
		Email:    "target@example.test",
		Password: "a sufficiently long password",
		IP:       "203.0.113.21",
	})
	if err != nil {
		t.Fatalf("sign in: %v", err)
	}

	if _, err := f.svc.SetPasswordByOperator(ctx, target.ID, "short", admin.ID, nil); !errors.Is(err, authn.ErrWeakPassword) {
		t.Fatalf("weak password: got %v, want ErrWeakPassword", err)
	}

	revoked, err := f.svc.SetPasswordByOperator(ctx, target.ID, "a different long password", admin.ID, nil)
	if err != nil {
		t.Fatalf("set password: %v", err)
	}
	if revoked == 0 {
		t.Fatal("no session was revoked by a password change")
	}
	if _, _, err := f.svc.Authenticate(ctx, res.Token); !errors.Is(err, authn.ErrNoSession) {
		t.Fatalf("the old session still authenticates: %v", err)
	}
	if _, err := f.svc.Login(ctx, authn.LoginRequest{
		Email:    "target@example.test",
		Password: "a different long password",
		IP:       "203.0.113.22",
	}); err != nil {
		t.Fatalf("the new password does not work: %v", err)
	}
}

// The list is the screen: search, the staff/customer split, and the last sign-in.
func TestListUsersSearchesAndFilters(t *testing.T) {
	f := newIdentityFixture(t)
	ctx := context.Background()

	createAs(t, f, "agent@example.test", "support")
	createAs(t, f, "someone@example.test", "")

	all, _, err := f.svc.ListUsers(ctx, authn.UserFilter{}, 50, "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("listed %d accounts, want 2", len(all))
	}

	staffOnly := true
	staff, _, err := f.svc.ListUsers(ctx, authn.UserFilter{Staff: &staffOnly}, 50, "")
	if err != nil {
		t.Fatalf("list staff: %v", err)
	}
	if len(staff) != 1 || staff[0].Role != "support" {
		t.Fatalf("staff list = %+v, want the one support account", staff)
	}

	// The search is case-insensitive and matches the display name as well as the address.
	found, _, err := f.svc.ListUsers(ctx, authn.UserFilter{Query: "AGENT"}, 50, "")
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(found) != 1 || found[0].Email != "agent@example.test" {
		t.Fatalf("search returned %+v", found)
	}

	if _, _, err := f.svc.ListUsers(ctx, authn.UserFilter{Query: "a"}, 50, ""); !errors.Is(err, authn.ErrInvalidUser) {
		t.Fatalf("a one-character search: got %v, want ErrInvalidUser", err)
	}

	// Signing in is recorded, and the list reports it: "has this person ever signed in" is
	// the first question of most support conversations about an account.
	if _, err := f.svc.Login(ctx, authn.LoginRequest{
		Email: "agent@example.test", Password: "a sufficiently long password", IP: "203.0.113.23",
	}); err != nil {
		t.Fatalf("sign in: %v", err)
	}
	after, _, err := f.svc.ListUsers(ctx, authn.UserFilter{Query: "agent"}, 50, "")
	if err != nil {
		t.Fatalf("list after sign-in: %v", err)
	}
	if len(after) != 1 || after[0].LastSignInAt == nil {
		t.Fatalf("last sign-in was not reported: %+v", after)
	}
}

// The roles screen asks "who holds this role", because that is the access-review question.
func TestRoleHolderCounts(t *testing.T) {
	f := newIdentityFixture(t)
	ctx := context.Background()

	createAs(t, f, "one@example.test", "support")
	createAs(t, f, "two@example.test", "support")

	usage, err := f.svc.RoleHolderCounts(ctx)
	if err != nil {
		t.Fatalf("count holders: %v", err)
	}
	if got := usage["support"].Holders; got != 2 {
		t.Fatalf("support holders = %d, want 2", got)
	}
	if got := usage["administrator"].Holders; got != 0 {
		t.Fatalf("administrator holders = %d, want 0", got)
	}
	// Every seeded role is reported, including the ones nobody holds: a role that is absent
	// from the map renders as "unknown" on the screen rather than as zero.
	for _, name := range []string{"administrator", "billing", "support", "infrastructure"} {
		if _, ok := usage[name]; !ok {
			t.Errorf("role %q is missing from the usage map", name)
		}
	}
}

// Renaming a custom role moves the accounts holding it, because `staff_roles.role` is a
// foreign key with ON UPDATE CASCADE and permissions are resolved by name.
func TestRenameRoleMovesItsHolders(t *testing.T) {
	f := newIdentityFixture(t)
	ctx := context.Background()

	rec := createRole(t, f, "rename", "before", []authn.Permission{authn.PermTicketReadAll})
	holder := createAs(t, f, "holder@example.test", rec.Name)

	updated, err := f.svc.UpdateRoleDetails(ctx, rec.ID, ptr("t-renamed"), ptr("after"), nil)
	if err != nil {
		t.Fatalf("rename: %v", err)
	}
	if updated.Name != "t-renamed" || updated.Description != "after" {
		t.Fatalf("renamed role = %+v", updated)
	}
	if _, err := f.pool.Exec(ctx, `DELETE FROM roles WHERE id = $1`, rec.ID); err == nil {
		t.Fatal("a held role was deleted, which the delete path is supposed to refuse")
	}
	got, err := f.svc.UserByIDRecord(ctx, holder.ID)
	if err != nil {
		t.Fatalf("reload holder: %v", err)
	}
	if got.Role != "t-renamed" {
		t.Fatalf("holder's role = %q, want t-renamed", got.Role)
	}
}

// A system role cannot be renamed either. The four seeded roles are what the separation of
// duties is written against, and a rename would be a way to make "administrator" mean
// something else.
func TestSystemRoleCannotBeRenamed(t *testing.T) {
	f := newIdentityFixture(t)
	roles, err := f.svc.ListRoles(context.Background())
	if err != nil {
		t.Fatalf("list roles: %v", err)
	}
	for _, rec := range roles {
		if !rec.System {
			continue
		}
		if _, err := f.svc.UpdateRoleDetails(context.Background(), rec.ID, ptr("t-hijacked"), nil, nil); !errors.Is(err, authn.ErrSystemRole) {
			t.Fatalf("renaming system role %q: got %v, want ErrSystemRole", rec.Name, err)
		}
	}
}

// Changing an address clears its verification: a confirmation given for one mailbox does not
// transfer to another.
func TestChangingEmailClearsVerification(t *testing.T) {
	f := newIdentityFixture(t)
	ctx := context.Background()
	rec := createAs(t, f, "old@example.test", "support")
	if !rec.EmailVerified {
		t.Fatal("the account was created unverified")
	}

	updated, err := f.svc.UpdateUser(ctx, rec.ID, authn.UpdateUserRequest{
		Email: ptr("new@example.test"),
	}, nil)
	if err != nil {
		t.Fatalf("change email: %v", err)
	}
	if updated.Email != "new@example.test" {
		t.Fatalf("email = %q", updated.Email)
	}
	if updated.EmailVerified {
		t.Fatal("the new address inherited the old address's verification")
	}
}

func ptr(s string) *string { return &s }

// ------------------------------------------------- staff billing identity --

// The migration that backfills existing staff, executed exactly as the migrator would run it.
//
// It runs the *embedded file*, not a copy of its statements: a test that pasted its own SQL
// would keep passing after somebody edited the migration, which is the drift this is meant to
// catch. The file is a BEGIN/COMMIT script, so it needs the simple protocol — the extended one
// refuses more than one statement per Exec.
func runStaffBillingBackfill(t *testing.T, f *identityFixture) {
	t.Helper()
	migrations, err := store.LoadMigrations()
	if err != nil {
		t.Fatalf("load migrations: %v", err)
	}
	var body string
	for _, mig := range migrations {
		if mig.Version == 56 {
			body = mig.SQL
		}
	}
	if body == "" {
		t.Fatal("migration 0056 is not embedded, so there is nothing to backfill with")
	}
	if _, err := f.pool.Exec(context.Background(), body, pgx.QueryExecModeSimpleProtocol); err != nil {
		t.Fatalf("run migration 0056: %v", err)
	}
}

// grant writes a staff role straight to the table, the way the CLI bootstrap does.
func (f *identityFixture) grant(t *testing.T, userID, role string) {
	t.Helper()
	if _, err := f.pool.Exec(context.Background(),
		`INSERT INTO staff_roles (user_id, role) VALUES ($1, $2)`, userID, role); err != nil {
		t.Fatalf("grant %s to %s: %v", role, userID, err)
	}
}

// billingIdentityOf reads the billing records a login owns.
func billingIdentityOf(t *testing.T, f *identityFixture, userID string) []struct{ ID, Email, Role string } {
	t.Helper()
	rows, err := f.pool.Query(context.Background(), `
		SELECT c.id::text, c.email, ca.role
		FROM customer_accounts ca
		JOIN customers c ON c.id = ca.customer_id
		WHERE ca.user_id = $1
		ORDER BY ca.created_at`, userID)
	if err != nil {
		t.Fatalf("read billing identity: %v", err)
	}
	defer rows.Close()

	var out []struct{ ID, Email, Role string }
	for rows.Next() {
		var row struct{ ID, Email, Role string }
		if err := rows.Scan(&row.ID, &row.Email, &row.Role); err != nil {
			t.Fatalf("scan billing identity: %v", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read billing identity: %v", err)
	}
	return out
}

// A staff account owns a billing account from the moment it exists, and it is its own: the
// record carries the account's address, nothing marks it as an operator's, and the two calls
// the `/users` screen reads both report it.
func TestCreatedStaffUserGetsABillingIdentity(t *testing.T) {
	f := newIdentityFixture(t)
	ctx := context.Background()

	rec := createAs(t, f, "agent@example.test", "support")
	if rec.CustomerID == "" {
		t.Fatal("the created staff account has no billing identity, so it cannot buy anything")
	}

	owned := billingIdentityOf(t, f, rec.ID)
	if len(owned) != 1 {
		t.Fatalf("the staff account owns %d billing records, want exactly 1", len(owned))
	}
	if owned[0].ID != rec.CustomerID {
		t.Fatalf("customer_id = %q, want the linked record %q", rec.CustomerID, owned[0].ID)
	}
	if owned[0].Email != "agent@example.test" {
		t.Fatalf("the billing record's address is %q, want the account's own", owned[0].Email)
	}
	if owned[0].Role != "owner" {
		t.Fatalf("link role = %q, want owner", owned[0].Role)
	}

	loaded, err := f.svc.UserByIDRecord(ctx, rec.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if loaded.CustomerID != owned[0].ID {
		t.Fatalf("the record the screen loads reports customer_id %q, want %q",
			loaded.CustomerID, owned[0].ID)
	}
	listed, _, err := f.svc.ListUsers(ctx, authn.UserFilter{Query: "agent"}, 50, "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(listed) != 1 || listed[0].CustomerID != owned[0].ID {
		t.Fatalf("the list reports %+v, want the account with its billing record", listed)
	}
}

// The same rule for a customer-only account: one path, not a staff special case. A login
// without a billing identity is a login that cannot buy, whichever kind it is.
func TestCreatedCustomerAlsoGetsABillingIdentity(t *testing.T) {
	f := newIdentityFixture(t)

	rec := createAs(t, f, "buyer@example.test", "")
	owned := billingIdentityOf(t, f, rec.ID)
	if len(owned) != 1 {
		t.Fatalf("the customer account owns %d billing records, want exactly 1", len(owned))
	}
	if owned[0].Email != "buyer@example.test" || owned[0].Role != "owner" {
		t.Fatalf("billing identity = %+v, want the account's own address as owner", owned[0])
	}
}

// An address an operator imported as a customer months ago is *adopted*, not duplicated: one
// address is one billing account, and refusing to attach a login to it would leave that
// customer with a record they can never reach.
func TestBillingIdentityAdoptsAnImportedCustomerRecord(t *testing.T) {
	f := newIdentityFixture(t)
	ctx := context.Background()

	var imported string
	if err := f.pool.QueryRow(ctx, `
		INSERT INTO customers (email, name) VALUES ('imported@example.test', 'Imported Ltd')
		RETURNING id`).Scan(&imported); err != nil {
		t.Fatalf("create customer: %v", err)
	}

	rec := createAs(t, f, "imported@example.test", "support")
	if rec.CustomerID != imported {
		t.Fatalf("customer_id = %q, want the imported record %q", rec.CustomerID, imported)
	}

	var records int
	if err := f.pool.QueryRow(ctx,
		`SELECT count(*) FROM customers WHERE lower(email) = 'imported@example.test'`).Scan(&records); err != nil {
		t.Fatalf("count: %v", err)
	}
	if records != 1 {
		t.Fatalf("%d billing records exist for one address, want 1", records)
	}
}

// **The escalation this feature must not introduce.** An address that is already another
// login's billing account is never shared: a second link would let the new account read that
// customer's invoices through the customer-scoped path, which is scoped by exactly that link.
// The refusal is a named error, and the failed create leaves nothing behind.
func TestBillingIdentityRefusesAnotherLoginsRecord(t *testing.T) {
	f := newIdentityFixture(t)
	ctx := context.Background()

	var customerID string
	if err := f.pool.QueryRow(ctx, `
		INSERT INTO customers (email, name) VALUES ('shared@example.test', 'The Owner')
		RETURNING id`).Scan(&customerID); err != nil {
		t.Fatalf("create customer: %v", err)
	}
	owner := f.user(t, "owner@example.test")
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO customer_accounts (user_id, customer_id, role) VALUES ($1, $2, 'owner')`,
		owner, customerID); err != nil {
		t.Fatalf("link owner: %v", err)
	}

	_, err := f.svc.CreateUser(ctx, authn.CreateUserRequest{
		Email:         "shared@example.test",
		DisplayName:   "Not The Owner",
		Password:      "a sufficiently long password",
		Role:          "support",
		EmailVerified: true,
	}, nil)
	if !errors.Is(err, authn.ErrBillingAddressTaken) {
		t.Fatalf("creating a login at another account's billing address: got %v, want ErrBillingAddressTaken", err)
	}

	var logins, links int
	if err := f.pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM users WHERE lower(email) = 'shared@example.test'),
		       (SELECT count(*) FROM customer_accounts WHERE customer_id = $1)`,
		customerID).Scan(&logins, &links); err != nil {
		t.Fatalf("count: %v", err)
	}
	if logins != 0 {
		t.Fatalf("%d login(s) exist after a refused create, want none", logins)
	}
	if links != 1 {
		t.Fatalf("%d login(s) own the billing record, want only the original one", links)
	}
}

// The accounts that predate this feature get the identity they never had: the CLI bootstrap,
// the older build, and the imported customer whose record was waiting for a login. The two
// cases the migration must *not* touch — an address another login already owns, and a
// customer-only account (out of scope: the create path covers new ones) — are asserted too,
// because "backfill everyone" would have been the easy and wrong version.
func TestMigrationBackfillsExistingStaff(t *testing.T) {
	f := newIdentityFixture(t)
	ctx := context.Background()

	// A staff account written straight to the tables, the way `billing admin create-user` does.
	plain := f.user(t, "legacy-agent@example.test")
	f.grant(t, plain, "support")

	// A staff account whose address an import already turned into a billing record nobody
	// could sign in with: adopted, not duplicated.
	imported := f.user(t, "imported-agent@example.test")
	f.grant(t, imported, "billing")
	var importedID string
	if err := f.pool.QueryRow(ctx, `
		INSERT INTO customers (email, name) VALUES ('imported-agent@example.test', 'Imported')
		RETURNING id`).Scan(&importedID); err != nil {
		t.Fatalf("create customer: %v", err)
	}

	// A staff account whose address is already another login's billing account: left alone.
	collides := f.user(t, "shared-agent@example.test")
	f.grant(t, collides, "infrastructure")
	var sharedID string
	if err := f.pool.QueryRow(ctx, `
		INSERT INTO customers (email, name) VALUES ('shared-agent@example.test', 'Somebody Else')
		RETURNING id`).Scan(&sharedID); err != nil {
		t.Fatalf("create customer: %v", err)
	}
	owner := f.user(t, "owner@example.test")
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO customer_accounts (user_id, customer_id, role) VALUES ($1, $2, 'owner')`,
		owner, sharedID); err != nil {
		t.Fatalf("link owner: %v", err)
	}

	// A customer-only login with no identity. The migration is about staff: a customer's
	// identity is created with the account, and reusing this file to invent billing records for
	// every old login would be a wider change than the one being made.
	shopper := f.user(t, "shopper@example.test")

	runStaffBillingBackfill(t, f)

	if got := billingIdentityOf(t, f, plain); len(got) != 1 || got[0].Email != "legacy-agent@example.test" {
		t.Fatalf("a legacy staff account was not given its own billing record: %+v", got)
	}
	if got := billingIdentityOf(t, f, imported); len(got) != 1 || got[0].ID != importedID {
		t.Fatalf("the imported record was not adopted: %+v", got)
	}
	if got := billingIdentityOf(t, f, collides); len(got) != 0 {
		t.Fatalf("a staff account was linked to another login's billing record: %+v", got)
	}
	if got := billingIdentityOf(t, f, owner); len(got) != 1 || got[0].ID != sharedID {
		t.Fatalf("the original owner lost their billing record: %+v", got)
	}
	if got := billingIdentityOf(t, f, shopper); len(got) != 0 {
		t.Fatalf("a customer-only account was backfilled by the staff migration: %+v", got)
	}

	// The file is idempotent, which is what makes it safe to run against a database that
	// already has it — including the one this test just migrated.
	runStaffBillingBackfill(t, f)
	for _, id := range []string{plain, imported, collides, owner, shopper} {
		if got := billingIdentityOf(t, f, id); len(got) > 1 {
			t.Fatalf("a second run duplicated the billing identity of %s: %+v", id, got)
		}
	}
}

// The other half of the delete rule: a billing record an *operator* created is never deleted
// with a login. It carries a name, an address and possibly attribution that outlive the login,
// so the refusal stands and points at disabling instead — the behaviour every account had
// before the identity became automatic.
func TestDeleteUserKeepsABillingRecordAnOperatorCreated(t *testing.T) {
	f := newIdentityFixture(t)
	ctx := context.Background()

	var imported string
	if err := f.pool.QueryRow(ctx, `
		INSERT INTO customers (email, name) VALUES ('imported@example.test', 'Imported Ltd')
		RETURNING id`).Scan(&imported); err != nil {
		t.Fatalf("create customer: %v", err)
	}
	rec := createAs(t, f, "imported@example.test", "support")
	admin := createAs(t, f, "admin@example.test", "administrator")

	err := f.svc.DeleteUser(ctx, rec.ID, admin.ID, nil)
	if !errors.Is(err, authn.ErrUserHasHistory) {
		t.Fatalf("deleting an account whose billing record an operator created: got %v, want ErrUserHasHistory", err)
	}
	var name string
	if err := f.pool.QueryRow(ctx,
		`SELECT name FROM customers WHERE id = $1`, imported).Scan(&name); err != nil {
		t.Fatalf("the imported record was deleted: %v", err)
	}
	if name != "Imported Ltd" {
		t.Fatalf("the imported record's name is %q, want it untouched", name)
	}
}

// A login whose billing record already holds a service cannot be deleted, because the record is
// what the service hangs off: the foreign key refuses it and the refusal says so rather than
// cascading a service away. A zero-total purchase produces exactly this state — a service and no
// invoice — so it is not a hypothetical.
func TestDeleteUserWithABilledServiceIsRefused(t *testing.T) {
	f := newIdentityFixture(t)
	ctx := context.Background()

	rec := createAs(t, f, "holder@example.test", "support")
	admin := createAs(t, f, "admin@example.test", "administrator")

	var productID string
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO products (slug, name) VALUES ('delete-guard', 'Guard') RETURNING id`).
		Scan(&productID); err != nil {
		t.Fatalf("create product: %v", err)
	}
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO services (customer_id, product_id, status, generation)
		VALUES ($1, $2, 'active', 1)`, rec.CustomerID, productID); err != nil {
		t.Fatalf("create service: %v", err)
	}

	err := f.svc.DeleteUser(ctx, rec.ID, admin.ID, nil)
	if !errors.Is(err, authn.ErrUserHasHistory) {
		t.Fatalf("deleting an account whose billing record holds a service: got %v, want ErrUserHasHistory", err)
	}
	if !strings.Contains(err.Error(), "billing record") {
		t.Fatalf("the refusal does not say what holds the account: %v", err)
	}
	if _, err := f.svc.UserByIDRecord(ctx, rec.ID); err != nil {
		t.Fatalf("the account was removed despite the refusal: %v", err)
	}
}
