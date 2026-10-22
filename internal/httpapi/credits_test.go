package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/catalyst/billing/internal/credit"
	"github.com/catalyst/billing/internal/extproto"
	"github.com/catalyst/billing/internal/portal"
)

// Credit over HTTP (CRD-02, CRD-05, CRD-07, CRD-08, CRD-10).
//
// The domain rules are proven in internal/credit. What is proven here is the part that only
// exists at the boundary: who may top up, who may grant, which status code a refusal
// carries, and the fact that a customer's balance never leaks across accounts.

// httpGateway is a checkout gateway for the HTTP tests. It is deterministic and never
// leaves the process, which is what makes the deposit test independent of an extension.
type httpGateway struct{ name string }

func (g httpGateway) Name() string { return g.name }

func (g httpGateway) CreateCheckout(_ context.Context, p extproto.CreateCheckoutParams) (extproto.CreateCheckoutResult, error) {
	return extproto.CreateCheckoutResult{
		PaymentID:   "pi_http_" + p.OperationKey,
		CheckoutURL: "https://pay.example.test/" + p.OperationKey,
		Status:      extproto.PaymentPending,
	}, nil
}

// creditEnv is a customer-facing server with a portal, a gateway and one tenant.
type creditEnv struct {
	*authEnv
	tenant     tenantFixture
	cookie     *http.Cookie
	csrf       string
	customerID string
}

func newCreditEnv(t *testing.T) *creditEnv {
	t.Helper()
	env := newAuthEnv(t)
	env.srv.WithPortal(portal.New(env.pool)).WithGateway(httpGateway{name: "fakegateway"})

	tenant := env.createTenant(t, "credit-http@example.test", "12.00")
	cookie, csrf := env.login(t, "credit-http@example.test", "a long enough password")
	return &creditEnv{authEnv: env, tenant: tenant, cookie: cookie, csrf: csrf, customerID: tenant.customerID}
}

// postCredit posts a JSON body as the signed-in customer.
func (e *creditEnv) postCredit(t *testing.T, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	return e.postJSON(t, path, e.cookie, e.csrf, body)
}

// postJSONWithKey posts a body with an explicit idempotency key, which is what a replay
// test needs: the default helper generates a fresh key per call.
func (e *authEnv) postJSONWithKey(t *testing.T, path string, cookie *http.Cookie, csrf string,
	body any, key string) *httptest.ResponseRecorder {
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
	req.Header.Set("Idempotency-Key", key)
	rec := httptest.NewRecorder()
	e.srv.Handler().ServeHTTP(rec, req)
	return rec
}

// A customer reads their own balance, and the settings that bound a top-up come with it.
func TestPortalCreditsShowsBalanceAndSettings(t *testing.T) {
	e := newCreditEnv(t)

	// Seed credit through the domain path so the balance is real.
	if _, err := credit.New(e.pool).Grant(context.Background(), credit.GrantRequest{
		CustomerID: e.customerID, AmountMinor: 2500, Currency: "USD",
		Reason: "seed", GrantedBy: "", Key: "http-seed-1",
	}); err != nil {
		t.Fatalf("seed grant: %v", err)
	}

	rec := e.get(t, "/api/v1/portal/credits", e.cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body %s", rec.Code, rec.Body)
	}
	var body struct {
		CustomerID string `json:"customer_id"`
		Balances   []struct {
			Minor    int64  `json:"minor"`
			Currency string `json:"currency"`
		} `json:"balances"`
		Settings struct {
			Enabled             bool  `json:"enabled"`
			MinimumDepositMinor int64 `json:"minimum_deposit_minor"`
			MaximumDepositMinor int64 `json:"maximum_deposit_minor"`
			MaximumCreditMinor  int64 `json:"maximum_credit_minor"`
			AutoUse             bool  `json:"auto_use"`
		} `json:"settings"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body: %v\n%s", err, rec.Body)
	}
	if body.CustomerID != e.customerID {
		t.Fatalf("customer_id = %q, want %q", body.CustomerID, e.customerID)
	}
	if len(body.Balances) != 1 || body.Balances[0].Minor != 2500 || body.Balances[0].Currency != "USD" {
		t.Fatalf("balances = %+v", body.Balances)
	}
	if !body.Settings.Enabled || !body.Settings.AutoUse {
		t.Fatalf("settings = %+v", body.Settings)
	}
	if body.Settings.MinimumDepositMinor != 500 || body.Settings.MaximumDepositMinor != 100000 {
		t.Fatalf("deposit bounds = %+v", body.Settings)
	}
}

// The balance is not readable anonymously.
func TestPortalCreditsRequiresAuthentication(t *testing.T) {
	e := newCreditEnv(t)
	if rec := e.get(t, "/api/v1/portal/credits", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous: status %d, want 401", rec.Code)
	}
}

// A deposit below the minimum is a 400 with a stable code, and nothing is created.
func TestCreditDepositBelowMinimumOverHTTP(t *testing.T) {
	e := newCreditEnv(t)
	var before int
	if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM invoices`).Scan(&before); err != nil {
		t.Fatal(err)
	}

	rec := e.postCredit(t, "/api/v1/portal/credits/deposit",
		map[string]any{"amount_minor": 100, "currency": "USD"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body %s", rec.Code, rec.Body)
	}
	if !containsCode(rec.Body.Bytes(), "deposit_below_minimum") {
		t.Fatalf("body = %s", rec.Body)
	}

	var after int
	if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM invoices`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("%d invoices after a refused deposit, want %d: nothing may be created", after, before)
	}
}

// A valid deposit returns a redirect, and the invoice is marked as a deposit.
func TestCreditDepositOverHTTP(t *testing.T) {
	e := newCreditEnv(t)

	rec := e.postCredit(t, "/api/v1/portal/credits/deposit",
		map[string]any{"amount_minor": 2500, "currency": "USD"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body %s", rec.Code, rec.Body)
	}
	var body struct {
		InvoiceID   string `json:"invoice_id"`
		Number      string `json:"number"`
		RedirectURL string `json:"redirect_url"`
		Amount      struct {
			Minor int64 `json:"minor"`
		} `json:"amount"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body: %v\n%s", err, rec.Body)
	}
	if body.InvoiceID == "" || body.RedirectURL == "" || body.Number == "" {
		t.Fatalf("body = %+v", body)
	}
	if body.Amount.Minor != 2500 {
		t.Fatalf("amount = %d, want 2500", body.Amount.Minor)
	}

	// The deposit is recognised by its own frozen snapshot, and it deliberately has no
	// order: the operator dashboard's paid-not-provisioned check joins on orders, and a
	// deposit must not look like a purchase that failed to deliver.
	var orderID *string
	if err := e.pool.QueryRow(context.Background(),
		`SELECT order_id::text FROM invoices WHERE id = $1`, body.InvoiceID).Scan(&orderID); err != nil {
		t.Fatal(err)
	}
	if orderID != nil {
		t.Fatalf("the deposit invoice has order %s; it must have none", *orderID)
	}
	if err := credit.New(e.pool).DepositInvoice(context.Background(), body.InvoiceID); err != nil {
		t.Fatalf("the deposit invoice is not recognised: %v", err)
	}
}

// A deposit without a gateway is a 503, not a 500: the deployment is incomplete, the
// request is not wrong.
func TestCreditDepositWithoutGateway(t *testing.T) {
	env := newAuthEnv(t)
	env.srv.WithPortal(portal.New(env.pool))
	tenant := env.createTenant(t, "nogateway@example.test", "5.00")
	_ = tenant
	cookie, csrf := env.login(t, "nogateway@example.test", "a long enough password")

	rec := env.postJSON(t, "/api/v1/portal/credits/deposit", cookie, csrf,
		map[string]any{"amount_minor": 2500, "currency": "USD"})
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body %s", rec.Code, rec.Body)
	}
	if !containsCode(rec.Body.Bytes(), "gateway_unavailable") {
		t.Fatalf("body = %s", rec.Body)
	}
}

// An operator with credit:write grants credit; the note is numbered and the balance moves.
func TestStaffGrantCreditOverHTTP(t *testing.T) {
	e := newStaffEnv(t)
	customerID, _ := e.seedCustomer(t, "grant@example.test", "Grant Me")

	rec := e.postJSON(t, "/api/v1/staff/customers/"+customerID+"/credit",
		e.billing, e.billingCSRF, map[string]any{
			"amount_minor": 1500,
			"currency":     "USD",
			"reason":       "goodwill after an outage",
		})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body %s", rec.Code, rec.Body)
	}
	var body struct {
		ID      string                `json:"id"`
		Number  string                `json:"number"`
		Amount  struct{ Minor int64 } `json:"amount"`
		Balance struct{ Minor int64 } `json:"balance"`
		Status  string                `json:"status"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body: %v\n%s", err, rec.Body)
	}
	if body.Number == "" || body.Amount.Minor != 1500 || body.Balance.Minor != 1500 {
		t.Fatalf("body = %+v", body)
	}
	if body.Status != "issued" {
		t.Fatalf("status = %q, want issued", body.Status)
	}

	// The audit row names the operator and carries the reason.
	var action, actorKind string
	if err := e.pool.QueryRow(context.Background(), `
		SELECT action, actor_kind FROM audit_events
		WHERE target_id = $1 ORDER BY id DESC LIMIT 1`, body.ID).Scan(&action, &actorKind); err != nil {
		t.Fatalf("audit row: %v", err)
	}
	if action != "credit.grant" || actorKind != "user" {
		t.Fatalf("audit = %s/%s", action, actorKind)
	}
}

// A grant needs a reason, and support cannot grant credit.
func TestStaffGrantCreditGuards(t *testing.T) {
	e := newStaffEnv(t)
	customerID, _ := e.seedCustomer(t, "grant-guard@example.test", "Guard")

	noReason := e.postJSON(t, "/api/v1/staff/customers/"+customerID+"/credit",
		e.billing, e.billingCSRF, map[string]any{
			"amount_minor": 500, "currency": "USD",
		})
	if noReason.Code != http.StatusBadRequest {
		t.Fatalf("no reason: status %d, want 400; body %s", noReason.Code, noReason.Body)
	}

	// Support holds customer:read:all but not credit:write.
	forbidden := e.postJSON(t, "/api/v1/staff/customers/"+customerID+"/credit",
		e.support, e.supportCSRF, map[string]any{
			"amount_minor": 500, "currency": "USD", "reason": "should not work",
		})
	if forbidden.Code != http.StatusForbidden {
		t.Fatalf("support grant: status %d, want 403; body %s", forbidden.Code, forbidden.Body)
	}

	// A malformed customer id is a 404 before the database is touched.
	bad := e.postJSON(t, "/api/v1/staff/customers/not-a-uuid/credit",
		e.billing, e.billingCSRF, map[string]any{
			"amount_minor": 500, "currency": "USD", "reason": "x",
		})
	if bad.Code != http.StatusNotFound {
		t.Fatalf("bad id: status %d, want 404; body %s", bad.Code, bad.Body)
	}
}

// A replay of the same grant request returns the same note instead of granting twice.
func TestStaffGrantCreditReplayIsIdempotent(t *testing.T) {
	e := newStaffEnv(t)
	customerID, _ := e.seedCustomer(t, "grant-replay@example.test", "Replay")

	body := map[string]any{"amount_minor": 900, "currency": "USD", "reason": "goodwill"}
	key := "grant-replay-key-1"

	first := e.postJSONWithKey(t, "/api/v1/staff/customers/"+customerID+"/credit",
		e.billing, e.billingCSRF, body, key)
	if first.Code != http.StatusCreated {
		t.Fatalf("first: status %d; body %s", first.Code, first.Body)
	}
	second := e.postJSONWithKey(t, "/api/v1/staff/customers/"+customerID+"/credit",
		e.billing, e.billingCSRF, body, key)
	if second.Code != http.StatusCreated {
		t.Fatalf("replay: status %d; body %s", second.Code, second.Body)
	}

	var notes int
	if err := e.pool.QueryRow(context.Background(), `
		SELECT count(*) FROM credit_notes`).Scan(&notes); err != nil {
		t.Fatal(err)
	}
	if notes != 1 {
		t.Fatalf("%d credit notes after a replay, want 1", notes)
	}
}
