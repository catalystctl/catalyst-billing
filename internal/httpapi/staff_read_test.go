package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"bytes"

	"github.com/catalyst/billing/internal/portal"
)

// staffEnv is a server with the staff surface wired and one staff identity per role.
type staffEnv struct {
	*authEnv
	admin,
	billing,
	support *http.Cookie
	adminCSRF string
}

func newStaffEnv(t *testing.T) *staffEnv {
	t.Helper()
	env := newAuthEnv(t)
	out := &staffEnv{authEnv: env}

	env.createUser(t, "admin@example.test", "admin password 1", "administrator")
	out.admin, out.adminCSRF = env.login(t, "admin@example.test", "admin password 1")

	env.createUser(t, "billing@example.test", "billing password 1", "billing")
	out.billing, _ = env.login(t, "billing@example.test", "billing password 1")

	env.createUser(t, "support@example.test", "support password 1", "support")
	out.support, _ = env.login(t, "support@example.test", "support password 1")

	return out
}

// seedCustomer creates a customer with a login attached.
func (e *authEnv) seedCustomer(t *testing.T, email, name string) (customerID, userID string) {
	t.Helper()
	ctx := context.Background()
	if err := e.pool.QueryRow(ctx,
		`INSERT INTO customers (email, name) VALUES ($1, $2) RETURNING id`,
		email, name).Scan(&customerID); err != nil {
		t.Fatalf("create customer: %v", err)
	}
	hash, err := e.hasher.Hash("customer password 1")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if err := e.pool.QueryRow(ctx, `
		INSERT INTO users (email, password_hash, email_verified_at)
		VALUES ($1, $2, now()) RETURNING id`, email, hash).Scan(&userID); err != nil {
		t.Fatalf("create user: %v", err)
	}
	if _, err := e.pool.Exec(ctx, `
		INSERT INTO customer_accounts (user_id, customer_id) VALUES ($1, $2)`,
		userID, customerID); err != nil {
		t.Fatalf("link account: %v", err)
	}
	return customerID, userID
}

// seedInvoice inserts an invoice directly. It bypasses the issuer on purpose: these tests
// assert the read surface, and an issuer bug should not be able to make them pass.
func (e *authEnv) seedInvoice(t *testing.T, customerID, status, currency string, total, paid int64, dueAt *time.Time) string {
	t.Helper()
	ctx := context.Background()
	var (
		id     string
		number any
		issued any
	)
	if status != "draft" {
		number = "INV-" + time.Now().Format("150405.000000")
		issued = time.Now().UTC()
	}
	if err := e.pool.QueryRow(ctx, `
		INSERT INTO invoices (customer_id, number, status, currency, subtotal_minor, tax_minor,
			total_minor, amount_paid_minor, issued_at, due_at, snapshot)
		VALUES ($1, $2, $3, $4, $5, 0, $5, $6, $7, $8, '{"schema_version":1}'::jsonb)
		RETURNING id`,
		customerID, number, status, currency, total, paid, issued, dueAt).Scan(&id); err != nil {
		t.Fatalf("create invoice: %v", err)
	}
	return id
}

// post performs a mutation with the CSRF token and an idempotency key.
func (e *authEnv) postJSON(t *testing.T, path string, cookie *http.Cookie, csrf string, body any) *httptest.ResponseRecorder {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, path, bytesReader(encoded))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	if csrf != "" {
		req.Header.Set("X-CSRF-Token", csrf)
	}
	req.Header.Set("Idempotency-Key", "test-"+path+"-"+time.Now().Format("150405.000000000"))
	rec := httptest.NewRecorder()
	e.srv.Handler().ServeHTTP(rec, req)
	return rec
}

// ---------------------------------------------------------------- customers --

// The customer list reports the aggregates the workspace shows, and search works by email
// and by name.
func TestStaffCustomerListAndSearch(t *testing.T) {
	env := newStaffEnv(t)
	first, _ := env.seedCustomer(t, "ada@example.test", "Ada Lovelace")
	env.seedCustomer(t, "grace@example.test", "Grace Hopper")

	// An open invoice gives the first customer a non-zero outstanding balance.
	due := time.Now().Add(72 * time.Hour)
	env.seedInvoice(t, first, "open", "USD", 1200, 0, &due)

	rec := env.get(t, "/api/v1/staff/customers", env.admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body %s", rec.Code, rec.Body)
	}
	var page struct {
		Data []struct {
			ID           string `json:"id"`
			Email        string `json:"email"`
			OpenInvoices int    `json:"open_invoices"`
			Outstanding  []struct {
				Minor    int64  `json:"minor"`
				Currency string `json:"currency"`
			} `json:"outstanding"`
			Services int  `json:"services"`
			HasLogin bool `json:"has_login"`
		} `json:"data"`
		NextCursor *string `json:"next_cursor"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatalf("body: %v\n%s", err, rec.Body)
	}
	if len(page.Data) != 2 {
		t.Fatalf("customers = %d, want 2", len(page.Data))
	}
	var found bool
	for _, row := range page.Data {
		if row.ID != first {
			continue
		}
		found = true
		if row.OpenInvoices != 1 {
			t.Fatalf("open_invoices = %d, want 1", row.OpenInvoices)
		}
		if len(row.Outstanding) != 1 || row.Outstanding[0].Minor != 1200 {
			t.Fatalf("outstanding = %+v", row.Outstanding)
		}
		if !row.HasLogin {
			t.Fatal("has_login = false for a customer with an account")
		}
	}
	if !found {
		t.Fatal("the seeded customer is missing from the list")
	}

	// Search by email.
	rec = env.get(t, "/api/v1/staff/customers?q=ada@", env.admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("search status = %d", rec.Code)
	}
	var searched struct {
		Data []struct{ Email string } `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &searched); err != nil {
		t.Fatal(err)
	}
	if len(searched.Data) != 1 || searched.Data[0].Email != "ada@example.test" {
		t.Fatalf("search by email = %+v", searched.Data)
	}

	// Search by name.
	rec = env.get(t, "/api/v1/staff/customers?q=hopper", env.admin)
	if err := json.Unmarshal(rec.Body.Bytes(), &searched); err != nil {
		t.Fatal(err)
	}
	if len(searched.Data) != 1 || searched.Data[0].Email != "grace@example.test" {
		t.Fatalf("search by name = %+v", searched.Data)
	}

	// A one-character search is refused rather than run as a scan.
	rec = env.get(t, "/api/v1/staff/customers?q=a", env.admin)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("one-character search: status %d, want 400", rec.Code)
	}
}

// A customer with two currencies shows two outstanding lines, never a sum.
func TestStaffCustomerTwoCurrencies(t *testing.T) {
	env := newStaffEnv(t)
	id, _ := env.seedCustomer(t, "multi@example.test", "Multi Currency")
	due := time.Now().Add(48 * time.Hour)
	env.seedInvoice(t, id, "open", "USD", 1000, 0, &due)
	env.seedInvoice(t, id, "open", "EUR", 2000, 0, &due)

	rec := env.get(t, "/api/v1/staff/customers/"+id, env.admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body %s", rec.Code, rec.Body)
	}
	var detail struct {
		Balance struct {
			Outstanding []struct {
				Minor    int64  `json:"minor"`
				Currency string `json:"currency"`
			} `json:"outstanding"`
		} `json:"balance"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("body: %v\n%s", err, rec.Body)
	}
	if len(detail.Balance.Outstanding) != 2 {
		t.Fatalf("outstanding lines = %d, want one per currency: %s",
			len(detail.Balance.Outstanding), rec.Body)
	}
	seen := map[string]int64{}
	for _, line := range detail.Balance.Outstanding {
		seen[line.Currency] = line.Minor
	}
	if seen["USD"] != 1000 || seen["EUR"] != 2000 {
		t.Fatalf("outstanding = %v", seen)
	}
}

// A customer with no login is a legitimate state and explains the most common support
// question.
func TestStaffCustomerWithoutLogin(t *testing.T) {
	env := newStaffEnv(t)
	var id string
	if err := env.pool.QueryRow(context.Background(),
		`INSERT INTO customers (email, name) VALUES ('nologin@example.test', 'No Login')
		 RETURNING id`).Scan(&id); err != nil {
		t.Fatalf("create customer: %v", err)
	}

	rec := env.get(t, "/api/v1/staff/customers/"+id, env.admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body %s", rec.Code, rec.Body)
	}
	var detail struct {
		Customer struct {
			HasLogin bool `json:"has_login"`
		} `json:"customer"`
		Accounts []any `json:"accounts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	if detail.Customer.HasLogin {
		t.Fatal("has_login = true for a customer with no account")
	}
	if len(detail.Accounts) != 0 {
		t.Fatalf("accounts = %d, want none", len(detail.Accounts))
	}
}

// A malformed id is a 404 that never reaches the database, and it is byte-identical to an
// unknown id.
func TestStaffCustomerNotFound(t *testing.T) {
	env := newStaffEnv(t)
	unknown := env.get(t, "/api/v1/staff/customers/0f8fad5b-d9cb-469f-a165-70867728950e", env.admin)
	if unknown.Code != http.StatusNotFound {
		t.Fatalf("unknown: status %d", unknown.Code)
	}
	malformed := env.get(t, "/api/v1/staff/customers/not-a-uuid", env.admin)
	if malformed.Code != http.StatusNotFound {
		t.Fatalf("malformed: status %d", malformed.Code)
	}
	if unknown.Body.String() != malformed.Body.String() {
		t.Fatalf("a malformed id is distinguishable from an unknown one:\n%s\n%s",
			unknown.Body, malformed.Body)
	}
}

// Permission denial: support may read customers, and no role without the permission may.
func TestStaffReadPermissions(t *testing.T) {
	env := newStaffEnv(t)
	id, _ := env.seedCustomer(t, "perm@example.test", "Permissions")

	// Support holds customer:read:all.
	if rec := env.get(t, "/api/v1/staff/customers/"+id, env.support); rec.Code != http.StatusOK {
		t.Fatalf("support read: status %d", rec.Code)
	}
	// Support does not hold payment:reconcile, so the payment history is refused.
	if rec := env.get(t, "/api/v1/staff/payments", env.support); rec.Code != http.StatusForbidden {
		t.Fatalf("support payments: status %d, want 403", rec.Code)
	}
	// A plain customer holds no staff permission at all.
	env.createUser(t, "customer@example.test", "customer password 1", "")
	cookie, _ := env.login(t, "customer@example.test", "customer password 1")
	if rec := env.get(t, "/api/v1/staff/customers", cookie); rec.Code != http.StatusForbidden {
		t.Fatalf("customer staff list: status %d, want 403", rec.Code)
	}
	// Anonymous is 401, not 403: the distinction is what the UI branches on.
	if rec := env.get(t, "/api/v1/staff/customers", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous: status %d, want 401", rec.Code)
	}
}

// ----------------------------------------------------------------- invoices --

func TestStaffInvoiceListFilters(t *testing.T) {
	env := newStaffEnv(t)
	customer, _ := env.seedCustomer(t, "inv@example.test", "Invoiced")

	past := time.Now().Add(-72 * time.Hour)
	future := time.Now().Add(72 * time.Hour)
	env.seedInvoice(t, customer, "open", "USD", 5000, 0, &past)   // overdue
	env.seedInvoice(t, customer, "open", "USD", 7000, 0, &future) // not overdue
	env.seedInvoice(t, customer, "paid", "USD", 3000, 3000, &past)

	// Everything for this customer.
	rec := env.get(t, "/api/v1/staff/invoices?customer_id="+customer, env.billing)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body %s", rec.Code, rec.Body)
	}
	var page struct {
		Data []struct {
			ID      string `json:"id"`
			Status  string `json:"status"`
			Overdue bool   `json:"overdue"`
			Due     struct {
				Minor int64 `json:"minor"`
			} `json:"due"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatalf("body: %v\n%s", err, rec.Body)
	}
	if len(page.Data) != 3 {
		t.Fatalf("invoices = %d, want 3", len(page.Data))
	}

	// The overdue filter is derived, not stored.
	rec = env.get(t, "/api/v1/staff/invoices?customer_id="+customer+"&overdue=true", env.billing)
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Data) != 1 || !page.Data[0].Overdue {
		t.Fatalf("overdue filter returned %+v", page.Data)
	}

	// The status filter.
	rec = env.get(t, "/api/v1/staff/invoices?customer_id="+customer+"&status=paid", env.billing)
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Data) != 1 || page.Data[0].Status != "paid" {
		t.Fatalf("status filter returned %+v", page.Data)
	}
	// A paid invoice has nothing due.
	if page.Data[0].Due.Minor != 0 {
		t.Fatalf("paid invoice due = %d, want 0", page.Data[0].Due.Minor)
	}
}

// Invoice detail returns the snapshot verbatim and the journal, and a paid invoice's paid
// amount equals the sum of its allocations.
func TestStaffInvoiceDetail(t *testing.T) {
	env := newStaffEnv(t)
	customer, _ := env.seedCustomer(t, "detail@example.test", "Detail")
	due := time.Now().Add(48 * time.Hour)
	invoiceID := env.seedInvoice(t, customer, "draft", "USD", 1200, 0, &due)

	// Lines are frozen once an invoice is issued, so they are added while it is a draft.
	ctx := context.Background()
	if _, err := env.pool.Exec(ctx, `
		INSERT INTO invoice_lines (invoice_id, position, description, quantity, unit_minor,
			net_minor, tax_rate, tax_minor, total_minor)
		VALUES ($1, 1, 'Hosting', 1, 1000, 1000, '20%', 200, 1200)`, invoiceID); err != nil {
		t.Fatalf("line: %v", err)
	}
	// Then issue it, which is what makes it open and payable.
	if _, err := env.pool.Exec(ctx, `
		UPDATE invoices SET status = 'open', number = 'INV-DETAIL', issued_at = now(),
		       amount_paid_minor = 1200
		WHERE id = $1`, invoiceID); err != nil {
		t.Fatalf("issue: %v", err)
	}
	var paymentID string
	if err := env.pool.QueryRow(ctx, `
		INSERT INTO payments (provider, provider_account, provider_payment_id, amount_minor,
			currency, status)
		VALUES ('fakegateway', 'acct_test', 'pi_detail', 1200, 'USD', 'succeeded')
		RETURNING id`).Scan(&paymentID); err != nil {
		t.Fatalf("payment: %v", err)
	}
	if _, err := env.pool.Exec(ctx, `
		INSERT INTO payment_allocations (payment_id, invoice_id, amount_minor, currency)
		VALUES ($1, $2, 1200, 'USD')`, paymentID, invoiceID); err != nil {
		t.Fatalf("allocation: %v", err)
	}

	rec := env.get(t, "/api/v1/staff/invoices/"+invoiceID, env.billing)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body %s", rec.Code, rec.Body)
	}
	var detail struct {
		Invoice struct {
			Snapshot map[string]any `json:"snapshot"`
			Lines    []struct {
				Description string `json:"description"`
			} `json:"lines"`
			Paid struct {
				Minor int64 `json:"minor"`
			} `json:"paid"`
		} `json:"invoice"`
		Payments    []any `json:"payments"`
		Allocations []struct {
			Amount struct {
				Minor int64 `json:"minor"`
			} `json:"amount"`
		} `json:"allocations"`
		Journal []any `json:"journal"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("body: %v\n%s", err, rec.Body)
	}
	if _, ok := detail.Invoice.Snapshot["schema_version"]; !ok {
		t.Fatalf("the snapshot was not returned verbatim: %v", detail.Invoice.Snapshot)
	}
	if len(detail.Invoice.Lines) != 1 || detail.Invoice.Lines[0].Description != "Hosting" {
		t.Fatalf("lines = %+v", detail.Invoice.Lines)
	}
	if len(detail.Payments) != 1 {
		t.Fatalf("payments = %d", len(detail.Payments))
	}

	// The paid amount must equal the sum of the allocations: this is the invariant an
	// operator debugging a total relies on.
	var allocated int64
	for _, a := range detail.Allocations {
		allocated += a.Amount.Minor
	}
	if allocated != detail.Invoice.Paid.Minor {
		t.Fatalf("paid %d != allocated %d", detail.Invoice.Paid.Minor, allocated)
	}
}

// ----------------------------------------------------------------- services --

func (e *staffEnv) seedService(t *testing.T, customerID string, status string) (serviceID, productID string) {
	t.Helper()
	ctx := context.Background()
	if err := e.pool.QueryRow(ctx, `
		INSERT INTO products (slug, name) VALUES ('vps-'||gen_random_uuid(), 'VPS')
		RETURNING id`).Scan(&productID); err != nil {
		t.Fatalf("product: %v", err)
	}
	if err := e.pool.QueryRow(ctx, `
		INSERT INTO services (customer_id, product_id, status)
		VALUES ($1, $2, $3) RETURNING id`, customerID, productID, status).Scan(&serviceID); err != nil {
		t.Fatalf("service: %v", err)
	}
	return serviceID, productID
}

// A service with a failed operation reports its error, and one with no subscription shows
// next_bill_at as null.
func TestStaffServiceDetail(t *testing.T) {
	env := newStaffEnv(t)
	customer, _ := env.seedCustomer(t, "svc@example.test", "Service Owner")
	serviceID, _ := env.seedService(t, customer, "active")

	ctx := context.Background()
	if _, err := env.pool.Exec(ctx, `
		INSERT INTO service_operations (service_id, kind, operation_key, generation, status,
			attempts, last_error)
		VALUES ($1::uuid, 'create', 'create:'||$1::text, 1, 'failed', 3, 'adapter refused the plan')`,
		serviceID); err != nil {
		t.Fatalf("operation: %v", err)
	}

	rec := env.get(t, "/api/v1/staff/services/"+serviceID, env.support)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body %s", rec.Code, rec.Body)
	}
	var detail struct {
		Service struct {
			LastError  *string `json:"last_error"`
			NextBillAt *string `json:"next_bill_at"`
		} `json:"service"`
		Subscription *struct {
			ID string `json:"id"`
		} `json:"subscription"`
		Operations []struct {
			Status    string `json:"status"`
			LastError string `json:"last_error"`
		} `json:"operations"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("body: %v\n%s", err, rec.Body)
	}
	if detail.Service.LastError == nil || *detail.Service.LastError != "adapter refused the plan" {
		t.Fatalf("last_error = %v", detail.Service.LastError)
	}
	if detail.Service.NextBillAt != nil {
		t.Fatalf("next_bill_at = %v, want null for a service with no subscription", *detail.Service.NextBillAt)
	}
	if detail.Subscription != nil {
		t.Fatalf("subscription = %+v, want null", detail.Subscription)
	}
	if len(detail.Operations) != 1 || detail.Operations[0].Status != "failed" {
		t.Fatalf("operations = %+v", detail.Operations)
	}
}

// The eligibility endpoint records a decision, which is why it is separate from the read.
func TestStaffSuspensionEligibilityRecordsDecision(t *testing.T) {
	env := newStaffEnv(t)
	customer, _ := env.seedCustomer(t, "elig@example.test", "Eligibility")
	serviceID, _ := env.seedService(t, customer, "active")

	before := env.countRows(t, "suspension_decisions")
	rec := env.get(t, "/api/v1/staff/services/"+serviceID+"/suspension-eligibility", env.admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body %s", rec.Code, rec.Body)
	}
	var body struct {
		Eligible bool   `json:"eligible"`
		Reason   string `json:"reason"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body: %v\n%s", err, rec.Body)
	}
	if body.Eligible {
		t.Fatalf("a service with no invoice was reported eligible: %s", rec.Body)
	}
	if body.Reason == "" {
		t.Fatal("the decision has no reason")
	}
	if after := env.countRows(t, "suspension_decisions"); after != before+1 {
		t.Fatalf("decisions = %d, want %d: the evaluation did not record one", after, before+1)
	}

	// The plain detail read must NOT record a decision: a read endpoint that writes on every
	// page load fills the audit trail with noise.
	before = env.countRows(t, "suspension_decisions")
	if rec := env.get(t, "/api/v1/staff/services/"+serviceID, env.admin); rec.Code != http.StatusOK {
		t.Fatalf("detail status = %d", rec.Code)
	}
	if after := env.countRows(t, "suspension_decisions"); after != before {
		t.Fatalf("the detail read recorded %d decision(s)", after-before)
	}
}

func (e *staffEnv) countRows(t *testing.T, table string) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(),
		"SELECT count(*) FROM "+table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// ------------------------------------------------------------- operations --

// Filtering by status returns exactly the operations in that state.
func TestStaffOperationFilters(t *testing.T) {
	env := newStaffEnv(t)
	customer, _ := env.seedCustomer(t, "ops@example.test", "Ops")
	serviceID, _ := env.seedService(t, customer, "active")

	ctx := context.Background()
	for i, status := range []string{"unknown", "succeeded", "unknown"} {
		if _, err := env.pool.Exec(ctx, `
			INSERT INTO service_operations (service_id, kind, operation_key, generation, status)
			VALUES ($1::uuid, 'create', $2::text, 1, $3::text)`,
			serviceID, "op-"+status+"-"+string(rune('a'+i)), status); err != nil {
			t.Fatalf("operation: %v", err)
		}
	}

	rec := env.get(t, "/api/v1/staff/operations?status=unknown", env.admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body %s", rec.Code, rec.Body)
	}
	var page struct {
		Data []struct {
			Status string `json:"status"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Data) != 2 {
		t.Fatalf("unknown operations = %d, want 2", len(page.Data))
	}
	for _, row := range page.Data {
		if row.Status != "unknown" {
			t.Fatalf("filter returned a %s operation", row.Status)
		}
	}
}

// A processed webhook is excluded by processed=false.
func TestStaffWebhookFilter(t *testing.T) {
	env := newStaffEnv(t)
	ctx := context.Background()
	for i, processed := range []any{nil, time.Now().UTC()} {
		if _, err := env.pool.Exec(ctx, `
			INSERT INTO gateway_events (provider, provider_account, event_id, event_type,
				payload, signature_valid, processed_at)
			VALUES ('stripe', 'acct_test', $1, 'payment.succeeded', '{}'::jsonb, true, $2)`,
			"evt_"+string(rune('a'+i)), processed); err != nil {
			t.Fatalf("event: %v", err)
		}
	}

	rec := env.get(t, "/api/v1/staff/webhooks?processed=false", env.billing)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body %s", rec.Code, rec.Body)
	}
	var page struct {
		Data []struct {
			ProcessedAt *string `json:"processed_at"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Data) != 1 || page.Data[0].ProcessedAt != nil {
		t.Fatalf("unprocessed events = %+v", page.Data)
	}
}

// ------------------------------------------------------------------ catalog --

// The catalog lists price versions, and a version is never mutated: the editor creates
// version N+1.
func TestStaffCatalog(t *testing.T) {
	env := newStaffEnv(t)
	ctx := context.Background()

	var productID string
	if err := env.pool.QueryRow(ctx,
		`INSERT INTO products (slug, name) VALUES ('vps-small', 'VPS Small') RETURNING id`).
		Scan(&productID); err != nil {
		t.Fatalf("product: %v", err)
	}
	for version := 1; version <= 2; version++ {
		if _, err := env.pool.Exec(ctx, `
			INSERT INTO price_versions (product_id, version, currency, amount_minor, interval,
				anchor_day, anchor_timezone, provision_plan, provision_region)
			VALUES ($1, $2, 'USD', $3, 'monthly', 1, 'UTC', 'plan-small', 'eu-west')`,
			productID, version, 1000*version); err != nil {
			t.Fatalf("price version: %v", err)
		}
	}

	rec := env.get(t, "/api/v1/staff/catalog/products/"+productID, env.billing)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body %s", rec.Code, rec.Body)
	}
	var body struct {
		PriceVersions []struct {
			Version         int    `json:"version"`
			ProvisionPlan   string `json:"provision_plan"`
			ProvisionRegion string `json:"provision_region"`
		} `json:"price_versions"`
		ActiveSubscriptions int `json:"active_subscriptions"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body: %v\n%s", err, rec.Body)
	}
	if len(body.PriceVersions) != 2 {
		t.Fatalf("price versions = %d, want 2", len(body.PriceVersions))
	}
	if body.PriceVersions[0].Version != 2 {
		t.Fatalf("versions are not newest first: %+v", body.PriceVersions)
	}
	if body.PriceVersions[0].ProvisionPlan != "plan-small" ||
		body.PriceVersions[0].ProvisionRegion != "eu-west" {
		t.Fatalf("catalog configuration missing: %+v", body.PriceVersions[0])
	}
	if body.ActiveSubscriptions != 0 {
		t.Fatalf("active_subscriptions = %d, want 0", body.ActiveSubscriptions)
	}
}

// -------------------------------------------------------------- pagination --

// Page two does not repeat or skip rows when a row is inserted between the pages.
func TestStaffPaginationIsStable(t *testing.T) {
	env := newStaffEnv(t)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		if _, err := env.pool.Exec(ctx, `
			INSERT INTO customers (email, name, created_at)
			VALUES ($1, 'Paged', now() - make_interval(mins => $2))`,
			"page"+string(rune('a'+i))+"@example.test", i); err != nil {
			t.Fatalf("customer: %v", err)
		}
	}

	first := env.get(t, "/api/v1/staff/customers?limit=2", env.admin)
	if first.Code != http.StatusOK {
		t.Fatalf("page one: %d %s", first.Code, first.Body)
	}
	var page struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
		NextCursor *string `json:"next_cursor"`
	}
	if err := json.Unmarshal(first.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Data) != 2 || page.NextCursor == nil {
		t.Fatalf("page one = %d rows, cursor %v", len(page.Data), page.NextCursor)
	}
	seen := map[string]bool{}
	for _, row := range page.Data {
		seen[row.ID] = true
	}

	// Insert a newer row between the pages: the window must not shift.
	if _, err := env.pool.Exec(ctx,
		`INSERT INTO customers (email, name) VALUES ('newest@example.test', 'Newest')`); err != nil {
		t.Fatal(err)
	}

	second := env.get(t, "/api/v1/staff/customers?limit=2&cursor="+*page.NextCursor, env.admin)
	if err := json.Unmarshal(second.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	for _, row := range page.Data {
		if seen[row.ID] {
			t.Fatalf("page two repeated row %s", row.ID)
		}
	}
	if len(page.Data) != 2 {
		t.Fatalf("page two = %d rows, want 2", len(page.Data))
	}

	// A malformed cursor is 400, never a silent reset to page one.
	bad := env.get(t, "/api/v1/staff/customers?cursor=not-a-cursor", env.admin)
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("malformed cursor: status %d, want 400", bad.Code)
	}
}

// ------------------------------------------------------------------ checkout --

// fakeGateway is a checkout gateway that records how often it was called.
type fakeGateway struct {
	calls int
	url   string
}

func (f *fakeGateway) Name() string { return "fakegateway" }

func (f *fakeGateway) CreateCheckout(_ context.Context, _ any) (any, error) {
	f.calls++
	return nil, nil
}

// The portal routes are exercised through the real server in portal_test.go; here we only
// assert that the checkout route exists, requires a permission, and refuses without a
// gateway. The domain behaviour is covered by internal/checkout.
func TestPortalCheckoutRequiresGateway(t *testing.T) {
	env := newAuthEnv(t)
	env.srv.WithPortal(portal.New(env.pool))
	customerID, _ := env.seedCustomer(t, "pay@example.test", "Customer")
	due := time.Now().Add(48 * time.Hour)
	invoiceID := env.seedInvoice(t, customerID, "open", "USD", 1200, 0, &due)

	cookie, csrf := env.login(t, "pay@example.test", "customer password 1")

	// No gateway is attached, so the route must say so rather than fail halfway through.
	rec := env.postJSON(t, "/api/v1/portal/invoices/"+invoiceID+"/checkout",
		cookie, csrf, map[string]any{})
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body %s", rec.Code, rec.Body)
	}
	if !containsCode(rec.Body.Bytes(), "gateway_unavailable") {
		t.Fatalf("body = %s", rec.Body)
	}
}

// A malformed invoice id is a 404 before the gateway is consulted.
func TestPortalCheckoutMalformedID(t *testing.T) {
	env := newAuthEnv(t)
	env.srv.WithPortal(portal.New(env.pool))
	env.createUser(t, "pay@example.test", "pay password 1", "")
	cookie, csrf := env.login(t, "pay@example.test", "pay password 1")

	rec := env.postJSON(t, "/api/v1/portal/invoices/not-a-uuid/checkout", cookie, csrf, map[string]any{})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body %s", rec.Code, rec.Body)
	}
}

// bytesReader wraps a byte slice for a request body.
func bytesReader(b []byte) *bytes.Reader { return bytes.NewReader(b) }

// containsCode reports whether a problem body carries a code.
func containsCode(body []byte, code string) bool {
	var problem struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(body, &problem); err != nil {
		return false
	}
	return problem.Code == code
}
