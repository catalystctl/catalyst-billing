package httpapi_test

// A staff account is also a customer of the business.
//
// This is the other half of `internal/authn/users_test.go`: the domain tests prove that a
// login is *given* a billing identity, and these prove the two things only the composed
// server can show —
//
//   - a staff user created through the `/users` screen can actually buy a service, billed to
//     their own billing account, with no manual linking step in between; and
//   - **the link grants nothing over anybody else.** A support agent who buys a server is a
//     customer like any other, so the customer-scoped path (`/api/v1/portal/...`, which
//     resolves the customer from the *session*) must still refuse another customer's invoice.
//     That refusal is the whole reason this feature is safe to ship: the convenience is a
//     `customer_accounts` row pointing at the account's own record, and nothing else.
//
// The staff path is asserted alongside it, because the interesting failure mode is not "the
// agent was refused" — it is an implementation that scopes the *staff* surface by the new link
// and quietly breaks the operator's job. Both must hold at once: refused as a customer,
// allowed as an agent.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"
)

// billingUser is the account shape from `userJSON`, narrowed to the fields these tests read.
type billingUser struct {
	ID         string  `json:"id"`
	Email      string  `json:"email"`
	Role       string  `json:"role"`
	IsStaff    bool    `json:"is_staff"`
	CustomerID *string `json:"customer_id"`
}

// createStaffAccount creates an account through the screen's endpoint, as the administrator.
func createStaffAccount(t *testing.T, env *staffEnv, email, role string) billingUser {
	t.Helper()
	rec := env.doUser(t, http.MethodPost, "/api/v1/staff/users", env.admin, env.adminCSRF,
		map[string]any{
			"email":        email,
			"display_name": "Test Agent",
			"password":     "a long enough password",
			"role":         role,
		})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create %s: status %d; body %s", email, rec.Code, rec.Body)
	}
	var out billingUser
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode created account: %v; body %s", err, rec.Body)
	}
	return out
}

// A support agent created on the users screen owns a billing account immediately, and can buy
// a service through the customer checkout path. Before this, the same agent got an account
// with no billing identity, and checkout answered "no customer record" until somebody linked
// the two rows by hand.
func TestStaffUserCreatedOnTheScreenCanBuyAService(t *testing.T) {
	env := shopEnv(t)
	_, paidPrice := env.seedProduct(t, "staff-vps", 2500, 0)
	_, freePrice := env.seedProduct(t, "staff-free", 0, 0)

	agent := createStaffAccount(t, env, "agent@example.test", "support")
	if agent.CustomerID == nil || *agent.CustomerID == "" {
		t.Fatalf("a created staff account has no billing identity: %+v", agent)
	}
	if !agent.IsStaff || agent.Role != "support" {
		t.Fatalf("created account = %+v, want a support agent", agent)
	}

	cookie, csrf := env.login(t, "agent@example.test", "a long enough password")

	// Buying something that costs money: the ordinary customer checkout, with the customer
	// taken from the session. No customer id in the body — the point is that the session
	// resolves one.
	rec := env.checkout(t, cookie, csrf, map[string]any{
		"lines": []map[string]any{{"price_version_id": paidPrice, "quantity": 1}},
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("a staff user cannot buy: status %d; body %s", rec.Code, rec.Body)
	}
	paid := decodeOrder(t, rec)
	if paid.InvoiceID == "" || len(paid.SubscriptionID) != 1 {
		t.Fatalf("incomplete order: %+v", paid)
	}

	// …and it is billed to the agent's *own* billing account, which is the record the create
	// made, not some other customer's.
	var billedTo string
	if err := env.pool.QueryRow(t.Context(),
		`SELECT customer_id::text FROM invoices WHERE id = $1`, paid.InvoiceID).Scan(&billedTo); err != nil {
		t.Fatalf("read invoice customer: %v", err)
	}
	if billedTo != *agent.CustomerID {
		t.Fatalf("the invoice is billed to %s, want the agent's own record %s",
			billedTo, *agent.CustomerID)
	}

	// Holding a service: a zero-total order activates immediately, so the agent ends up owning
	// a service on their own account, visible on their own portal — the thing that was
	// impossible without somebody linking two rows by hand.
	rec = env.checkout(t, cookie, csrf, map[string]any{
		"lines": []map[string]any{{"price_version_id": freePrice}},
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("a staff user cannot take a free service: status %d; body %s", rec.Code, rec.Body)
	}
	free := decodeOrder(t, rec)
	if !free.Activated || len(free.ServiceIDs) != 1 {
		t.Fatalf("a zero-total order did not give the agent a service: %+v", free)
	}

	services := env.get(t, "/api/v1/portal/services", cookie)
	if services.Code != http.StatusOK {
		t.Fatalf("portal services: status %d; body %s", services.Code, services.Body)
	}
	if !containsID(services.Body.Bytes(), free.ServiceIDs[0]) {
		t.Fatalf("the purchased service is not on the agent's own list: %s", services.Body)
	}
}

// **The separation-of-duties test.** A staff user with their own billing account must gain no
// reach over another customer's invoice through the customer-scoped path. The portal resolves
// the customer from the session, so a substituted identifier has to select no row — the same
// 404 a request for an id that never existed gets.
func TestStaffUserCannotReadAnotherCustomersInvoiceThroughThePortal(t *testing.T) {
	env := shopEnv(t)
	tenant := env.createTenant(t, "tenant@example.test", "40.00")

	agent := createStaffAccount(t, env, "agent@example.test", "support")
	if agent.CustomerID == nil {
		t.Fatal("the staff account has no billing identity, so this test would prove nothing")
	}
	if *agent.CustomerID == tenant.customerID {
		t.Fatal("the staff account was given the tenant's billing record")
	}
	cookie, _ := env.login(t, "agent@example.test", "a long enough password")

	// The invoice itself and its document: both scoped by `customer_accounts.user_id`, so the
	// tenant's identifier selects nothing.
	for _, path := range []string{
		"/api/v1/portal/invoices/" + tenant.invoiceID,
		"/api/v1/portal/invoices/" + tenant.invoiceID + "/pdf",
	} {
		rec := env.get(t, path, cookie)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("GET %s as the staff user: status %d, want 404; body %s",
				path, rec.Code, rec.Body)
		}
	}

	// The list is scoped too, so the invoice is not merely un-openable by id: it is absent.
	list := env.get(t, "/api/v1/portal/invoices", cookie)
	if list.Code != http.StatusOK {
		t.Fatalf("portal invoices: status %d; body %s", list.Code, list.Body)
	}
	if containsID(list.Body.Bytes(), tenant.invoiceID) {
		t.Fatalf("another customer's invoice is on the staff user's own list: %s", list.Body)
	}

	// The agent's own billing account works, which is what makes the refusals above scoping
	// rather than a broken account: same session, same endpoint, their own record.
	own := env.seedInvoice(t, *agent.CustomerID, "open", "USD", 1000, 0, nil)
	if rec := env.get(t, "/api/v1/portal/invoices/"+own, cookie); rec.Code != http.StatusOK {
		t.Fatalf("the agent cannot read their own invoice: status %d; body %s", rec.Code, rec.Body)
	}

	// And the operator surface still works: `invoice:read:all` is a staff grant and the new
	// link must not have narrowed it. Refused as a customer, allowed as an agent — both at
	// once is the property.
	if rec := env.get(t, "/api/v1/staff/invoices/"+tenant.invoiceID, cookie); rec.Code != http.StatusOK {
		t.Fatalf("a support agent can no longer read an invoice through the staff path: "+
			"status %d; body %s", rec.Code, rec.Body)
	}
}

// Both screens answer "which billing account does this login own": the users list, which links
// to it, and the customer overview, which shows the login as the account's portal user.
func TestStaffBillingAccountIsVisibleOnTheUsersAndCustomerScreens(t *testing.T) {
	env := shopEnv(t)
	agent := createStaffAccount(t, env, "agent@example.test", "billing")
	if agent.CustomerID == nil {
		t.Fatal("the created staff account has no billing identity")
	}

	// /users: the list the screen renders reports the billing account on the row.
	list := env.get(t, "/api/v1/staff/users?q=agent", env.admin)
	if list.Code != http.StatusOK {
		t.Fatalf("users list: status %d; body %s", list.Code, list.Body)
	}
	var page struct {
		Data []billingUser `json:"data"`
	}
	if err := json.Unmarshal(list.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode users list: %v; body %s", err, list.Body)
	}
	if len(page.Data) != 1 || page.Data[0].CustomerID == nil {
		t.Fatalf("the users list does not report the staff account's billing record: %+v", page.Data)
	}
	if *page.Data[0].CustomerID != *agent.CustomerID {
		t.Fatalf("users list customer_id = %s, want %s", *page.Data[0].CustomerID, *agent.CustomerID)
	}

	// The customer overview: the billing record the create made is a normal customer, and the
	// staff login is its portal account — the same shape any other customer has.
	detail := env.get(t, "/api/v1/staff/customers/"+*agent.CustomerID, env.admin)
	if detail.Code != http.StatusOK {
		t.Fatalf("customer overview: status %d; body %s", detail.Code, detail.Body)
	}
	var view struct {
		Customer struct {
			Email    string `json:"email"`
			HasLogin bool   `json:"has_login"`
		} `json:"customer"`
		Accounts []struct {
			Email string `json:"email"`
			Role  string `json:"role"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(detail.Body.Bytes(), &view); err != nil {
		t.Fatalf("decode customer overview: %v; body %s", err, detail.Body)
	}
	if view.Customer.Email != "agent@example.test" || !view.Customer.HasLogin {
		t.Fatalf("customer overview = %+v, want the agent's own record with a login", view.Customer)
	}
	if len(view.Accounts) != 1 || view.Accounts[0].Email != "agent@example.test" ||
		view.Accounts[0].Role != "owner" {
		t.Fatalf("customer overview accounts = %+v, want the agent as owner", view.Accounts)
	}
}

// The one refusal the automatic link adds: an address that is already another login's billing
// account is not shared. Two logins on one billing record would expose the first customer's
// invoices to the second, so the create is refused with a code that says which record is in
// the way rather than a 500 from the unique index.
func TestCreatingAUserAtAnotherLoginsBillingAddressIsRefused(t *testing.T) {
	env := shopEnv(t)

	// A billing record at an address that no *login* uses yet, owned by an existing customer's
	// login: this is the "the customer's login address was corrected, the record kept the old
	// one" case, which is the only way it can arise.
	var customerID string
	if err := env.pool.QueryRow(t.Context(), `
		INSERT INTO customers (email, name) VALUES ('moved@example.test', 'Moved Customer')
		RETURNING id`).Scan(&customerID); err != nil {
		t.Fatalf("create customer: %v", err)
	}
	owner := env.createUser(t, "owner@example.test", "owner password 1", "")
	if _, err := env.pool.Exec(t.Context(), `
		INSERT INTO customer_accounts (user_id, customer_id) VALUES ($1, $2)`, owner, customerID); err != nil {
		t.Fatalf("link owner: %v", err)
	}

	rec := env.doUser(t, http.MethodPost, "/api/v1/staff/users", env.admin, env.adminCSRF,
		map[string]any{
			"email":        "moved@example.test",
			"display_name": "Not The Owner",
			"password":     "a long enough password",
			"role":         "support",
		})
	if rec.Code != http.StatusConflict {
		t.Fatalf("status %d, want 409; body %s", rec.Code, rec.Body)
	}
	// The code is the shared `email_taken`: the staff-create route already answers a taken
	// address with it, and the domain's `ErrBillingAddressTaken` wraps `ErrEmailTaken` so this
	// refusal reaches the screen through the mapping that exists rather than through a second
	// code in a handler another change owns. What matters is the outcome — a 409, nothing
	// written — and that the *domain* still distinguishes the two.
	if code := decodeProblemCode(t, rec); code != "email_taken" {
		t.Fatalf("code = %q, want email_taken", code)
	}

	// Nothing was written: a refused create must not leave a login behind that owns somebody
	// else's billing record.
	var logins, links int
	if err := env.pool.QueryRow(t.Context(), `
		SELECT (SELECT count(*) FROM users WHERE lower(email) = 'moved@example.test'),
		       (SELECT count(*) FROM customer_accounts WHERE customer_id = $1)`,
		customerID).Scan(&logins, &links); err != nil {
		t.Fatalf("count: %v", err)
	}
	if logins != 0 {
		t.Fatalf("%d login(s) exist after a refused create", logins)
	}
	if links != 1 {
		t.Fatalf("%d login(s) own the billing record, want the original one", links)
	}
}

// containsID reports whether a JSON response body mentions the id anywhere. It is deliberately
// blunt: for the negative assertions above, a substring match cannot miss a nested field.
func containsID(body []byte, id string) bool {
	return len(id) > 0 && bytes.Contains(body, []byte(id))
}
