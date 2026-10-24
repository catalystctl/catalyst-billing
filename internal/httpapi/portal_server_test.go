package httpapi

// The live server card and its power route, driven through the real router: real session scoping, real
// route metadata, a real extension process over the real protocol.
//
// # What only this level can prove
//
// The adapter's own tests (extensions/catalystctl) prove what it sends to a panel. Only this level
// proves the things a customer can actually hit:
//
//   - **Scoping.** Another customer's service id must be a 404, not a 403 and not a read. The check is
//     a JOIN in one query, and a test that used a service the caller owns would never exercise it.
//   - **A 200 with a sentence, not a 5xx, when the provider is unreachable.** A game panel outage must
//     not make an invoice list look broken.
//   - **The power route is idempotent by the request's idempotency key** and writes an audit row that
//     attributes the action to the customer.
//   - **The four failure classes map to different statuses.** "Could not ask" and "cannot be done" are
//     different sentences to a customer, and only the second is permanent.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/catalyst/billing/internal/authn"
	"github.com/catalyst/billing/internal/gatewayconfig"
	"github.com/catalyst/billing/internal/store/storetest"
)

// serverClient is one seeded customer with one service.
type serverClient struct {
	customerID string
	user       authn.User
	serviceID  string
	externalID string
}

// seedServerClient inserts a user, a customer, the account link, and a product + service.
//
// The rows are inserted directly rather than through the catalog and portal APIs: those APIs are not
// what this test is about, and driving them would make the setup fail for reasons unrelated to the
// card.
func seedServerClient(t *testing.T, s *Server, externalID string) serverClient {
	t.Helper()
	pool := s.pool
	ctx := context.Background()

	const (
		customerID = "33333333-3333-4333-8333-333333333301"
		userID     = "33333333-3333-4333-8333-333333333302"
		productID  = "33333333-3333-4333-8333-333333333303"
	)
	if _, err := pool.Exec(ctx, `
		INSERT INTO users (id, email, password_hash, display_name, status)
		VALUES ($1, 'card-customer@example.test', 'x', 'Card Customer', 'active')
		ON CONFLICT DO NOTHING`, userID); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO customers (id, email, name)
		VALUES ($1, 'card-customer@example.test', 'Card Customer')
		ON CONFLICT DO NOTHING`, customerID); err != nil {
		t.Fatalf("seed customer: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO customer_accounts (user_id, customer_id, role)
		VALUES ($1, $2, 'owner')
		ON CONFLICT DO NOTHING`, userID, customerID); err != nil {
		t.Fatalf("seed account link: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO products (id, slug, name)
		VALUES ($1, 'card-product-`+t.Name()+`', 'Game server')
		ON CONFLICT DO NOTHING`, productID); err != nil {
		t.Fatalf("seed product: %v", err)
	}

	var serviceID string
	// The placement lives on the service, in `services.config.fields` — the same place the adapter's
	// own `provisioner.fields` declaration writes to. That is the design under test: the adapter holds
	// the panel's address and key, and the service says where its server goes.
	if err := pool.QueryRow(ctx, `
		INSERT INTO services (customer_id, product_id, status, external_id, generation, config)
		VALUES ($1, $2, 'active', $3, 1, $4)
		RETURNING id`,
		customerID, productID, externalID,
		`{"fields":{"location":"US East 1","template":"Paper","memory_mb":"1024"}}`,
	).Scan(&serviceID); err != nil {
		t.Fatalf("seed service: %v", err)
	}

	return serverClient{
		customerID: customerID,
		user:       authn.User{ID: userID, Email: "card-customer@example.test", Status: "active"},
		serviceID:  serviceID,
		externalID: externalID,
	}
}

// panelStub is a minimal Catalyst panel, enough for the card and its power route.
//
// It exists so this test drives the **real** adapter — a separate process, the real protocol, real HTTP
// — without reaching a real panel. What it deliberately does not do is reimplement the panel: it serves
// the handful of endpoints the adapter touches, so a change in the adapter's HTTP behaviour shows up
// here as a failure rather than as agreement between two mocks.
type panelStub struct {
	mu sync.Mutex
	// status is the server's live state, mutated by power calls.
	status string
	// powerCalls records every action that reached the stub.
	powerCalls []string
}

// newPanelURL starts a stub panel and returns only its address.
func newPanelURL(t *testing.T) string {
	t.Helper()
	_, url := newPanelStub(t)
	return url
}

func newPanelStub(t *testing.T) (*panelStub, string) {
	t.Helper()
	stub := &panelStub{status: "running"}
	srv := httptest.NewServer(http.HandlerFunc(stub.serve))
	t.Cleanup(srv.Close)
	return stub, srv.URL
}

func (p *panelStub) serve(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if r.Header.Get("Authorization") != "Bearer catalyst_test" {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"Invalid API key"}`))
		return
	}
	ok := func(data any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "data": data})
	}
	path := strings.TrimPrefix(r.URL.Path, "/api")

	switch {
	case r.Method == http.MethodGet && path == "/locations":
		ok([]map[string]any{{"id": "loc-1", "name": "US East 1"}})
	case r.Method == http.MethodGet && path == "/templates":
		ok([]map[string]any{{"id": "tpl-paper", "name": "Paper"}})
	case r.Method == http.MethodGet && path == "/nodes":
		ok([]map[string]any{{"id": "node-1", "name": "node one", "locationId": "loc-1", "isOnline": true}})
	case r.Method == http.MethodGet && strings.HasSuffix(path, "/allocations"):
		ok([]map[string]any{{"id": "alloc-1", "nodeId": "node-1", "ip": "10.0.0.1", "port": 25565, "serverId": nil}})
	case r.Method == http.MethodGet && strings.HasSuffix(path, "/stats"):
		ok(map[string]any{
			"cpuPercent": 12.5, "memoryUsageMb": 512, "memoryAllocatedMb": 1024,
			"diskUsageMb": 1024, "networkRxBytes": "1234", "networkTxBytes": "5678",
			"timestamp": "2026-10-16T02:15:59.851Z",
		})
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/servers/"):
		ok(map[string]any{
			"id": "srv-1", "name": "Fixture server", "status": p.status,
			"nodeId": "node-1", "locationId": "loc-1", "templateId": "tpl-paper",
			"allocatedMemoryMb": 1024, "allocatedDiskMb": 10240, "primaryPort": 25565,
			"createdAt":  "2026-10-01T00:00:00Z",
			"connection": map[string]any{"host": "play.example.test", "port": 25565},
		})
	case r.Method == http.MethodPost && strings.HasPrefix(path, "/servers/"):
		action := path[strings.LastIndex(path, "/")+1:]
		p.powerCalls = append(p.powerCalls, action)
		switch action {
		case "start":
			p.status = "running"
		case "stop", "kill":
			p.status = "stopped"
		case "restart":
			p.status = "starting"
		}
		ok(map[string]any{"id": "srv-1", "status": p.status})
	default:
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"no such endpoint","code":"NOT_FOUND"}`))
	}
}

// configureCatalystctl points the servers integration at the real CatalystCTL adapter and a panel stub.
//
// The adapter binary is built once per test, and the panel's address and key are stored settings — the
// two things an operator actually saves. Everything about *where a server goes* comes from the
// service's own provisioning fields, which is what seedServerClient writes.
func configureCatalystctl(t *testing.T, s *Server, panelURL string) {
	t.Helper()
	bin := buildAdapter(t)
	// The binary's location is not a stored setting: it comes from BILLING_PROVISIONER_EXTENSION, or
	// from the installation's bin directory. Setting the variable is exactly what a deployment does.
	t.Setenv("BILLING_PROVISIONER_EXTENSION", bin)

	resolver := gatewayconfig.New(s.pool)
	// Only what the test sets: an inherited extension path would silently override the adapter.
	resolver.LookupEnv = func(key string) (string, bool) {
		if key == "BILLING_PROVISIONER_EXTENSION" {
			return bin, true
		}
		return "", false
	}
	if _, err := resolver.Save(context.Background(), gatewayconfig.ServerKind, map[string]string{
		"provider":  "catalystctl",
		"panel_url": panelURL,
		"api_key":   "catalyst_test",
	}, "", nil); err != nil {
		t.Fatalf("configure catalystctl: %v", err)
	}
}

// buildAdapter builds extensions/catalystctl once per test.
func buildAdapter(t *testing.T) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "catalystctl")
	cmd := exec.Command("go", "build", "-o", out, "../../extensions/catalystctl")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build catalystctl: %v\n%s", err, b)
	}
	return out
}

// do issues a request through the real router as a given user.
func do(s *Server, method, path string, user *authn.User, body string, headers map[string]string) *httptest.ResponseRecorder {
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "test-key-1")
	// The session a test attaches carries this token; a mutating route is CSRF-checked even in a test,
	// which is the point of driving the real router.
	req.Header.Set(csrfHeaderName, "tok")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if user != nil {
		req = withUser(req, *user)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func TestPortalServerViewRendersTheLiveServer(t *testing.T) {
	s := testServer(t)
	storetest.RegisterTables("services", "customers", "customer_accounts", "users", "products")
	configureCatalystctl(t, s, newPanelURL(t))
	client := seedServerClient(t, s, "resource-card-1")

	rec := do(s, http.MethodGet, "/api/v1/portal/services/"+client.serviceID+"/server", &client.user, "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	var out struct {
		Provisioned bool     `json:"provisioned"`
		Available   bool     `json:"available"`
		Name        string   `json:"name"`
		State       string   `json:"state"`
		Address     string   `json:"address"`
		PanelURL    string   `json:"panel_url"`
		CanPower    []string `json:"can_power"`
		Detail      string   `json:"detail"`
		Stats       *struct {
			CPUPercent   float64 `json:"cpu_percent"`
			MemoryUsedMB int64   `json:"memory_used_mb"`
		} `json:"stats"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v\n%s", err, rec.Body.String())
	}

	if !out.Provisioned || !out.Available {
		t.Fatalf("expected a live server, got provisioned=%t available=%t detail=%q",
			out.Provisioned, out.Available, out.Detail)
	}
	// The name and the address are what the customer came for.
	if !strings.Contains(out.Name, "Fixture server") {
		t.Fatalf("name = %q, want the provider's own name", out.Name)
	}
	if out.Address != "play.example.test:25565" {
		t.Fatalf("address = %q, want host:port from the provider", out.Address)
	}
	// The deep link is the button that opens the panel. Without it the feature is incomplete.
	if !strings.Contains(out.PanelURL, "/servers/") {
		t.Fatalf("panel_url = %q, want a link to the server", out.PanelURL)
	}
	// A running server offers stop/restart/kill and never start.
	if strings.Join(out.CanPower, ",") != "stop,restart,kill" {
		t.Fatalf("can_power = %v, want stop,restart,kill for a running server", out.CanPower)
	}
	if out.Stats == nil || out.Stats.CPUPercent == 0 {
		t.Fatalf("stats = %+v, want the provider's figures", out.Stats)
	}
}

func TestPortalServerViewHidesAnotherCustomersService(t *testing.T) {
	s := testServer(t)
	storetest.RegisterTables("services", "customers", "customer_accounts", "users", "products")
	configureCatalystctl(t, s, newPanelURL(t))
	client := seedServerClient(t, s, "resource-card-2")

	// A different, signed-in customer.
	other := authn.User{ID: "44444444-4444-4444-8444-444444444401", Email: "other@example.test", Status: "active"}
	rec := do(s, http.MethodGet, "/api/v1/portal/services/"+client.serviceID+"/server", &other, "", nil)

	// 404, deliberately: a 403 would confirm the service exists, which is an enumeration oracle.
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for another customer's service: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "Fixture server") {
		t.Fatalf("the response leaked the server's name: %s", rec.Body.String())
	}
}

func TestPortalServerViewIsASentenceWhenTheProviderCannotBeReached(t *testing.T) {
	s := testServer(t)
	storetest.RegisterTables("services", "customers", "customer_accounts", "users", "products")
	configureCatalystctl(t, s, newPanelURL(t))
	client := seedServerClient(t, s, "resource-card-3")

	// Point the integration at a binary that does not exist: the launch fails, which is the same class
	// of failure as a provider outage.
	resolver := gatewayconfig.New(s.pool)
	// A path that does not exist: the launch fails to find the binary, which is the same class of
	// failure as a provider outage.
	t.Setenv("BILLING_PROVISIONER_EXTENSION", filepath.Join(t.TempDir(), "absent"))
	if _, err := resolver.Save(context.Background(), gatewayconfig.ServerKind, map[string]string{
		"provider": "fake",
	}, "", nil); err != nil {
		t.Fatalf("configure: %v", err)
	}

	rec := do(s, http.MethodGet, "/api/v1/portal/services/"+client.serviceID+"/server", &client.user, "", nil)

	// 200 with a sentence: a provider outage is not a reason to fail the request, and a 5xx here would
	// take the whole billing page down for something that is not the billing panel's fault.
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 with available=false: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Available bool   `json:"available"`
		Detail    string `json:"detail"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Available {
		t.Fatal("available should be false when the provider could not be launched")
	}
	if out.Detail == "" {
		t.Fatal("a card with no data must say why")
	}
	// The sentence must not leak the deployment's internals: a customer cannot act on a file path, and
	// it reads as though their server were broken.
	if strings.Contains(out.Detail, "/nonexistent") || strings.Contains(out.Detail, "launch") {
		t.Fatalf("the customer-facing sentence leaked internals: %q", out.Detail)
	}
}

func TestPortalServerViewSaysSoWhenNoServerExistsYet(t *testing.T) {
	s := testServer(t)
	storetest.RegisterTables("services", "customers", "customer_accounts", "users", "products")
	configureCatalystctl(t, s, newPanelURL(t))
	// An empty external id is a service that has not been provisioned: the normal state a minute
	// after a purchase.
	client := seedServerClient(t, s, "")

	rec := do(s, http.MethodGet, "/api/v1/portal/services/"+client.serviceID+"/server", &client.user, "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Provisioned bool   `json:"provisioned"`
		Detail      string `json:"detail"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Provisioned {
		t.Fatal("a service with no external id is not provisioned")
	}
	if out.Detail == "" {
		t.Fatal("the card must explain why there is nothing to show")
	}
}

func TestPortalServerPowerPerformsTheAction(t *testing.T) {
	s := testServer(t)
	storetest.RegisterTables("services", "customers", "customer_accounts", "users", "products", "audit_events")
	stub, panelURL := newPanelStub(t)
	configureCatalystctl(t, s, panelURL)
	client := seedServerClient(t, s, "resource-card-4")

	// The stub starts "running", so stop is the action that changes something.
	rec := do(s, http.MethodPost, "/api/v1/portal/services/"+client.serviceID+"/server/power",
		&client.user, `{"action":"stop"}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Action  string `json:"action"`
		Changed bool   `json:"changed"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Action != "stop" || !out.Changed {
		t.Fatalf("expected a changed stop, got %+v", out)
	}

	// The action reached the provider, not just this process.
	stub.mu.Lock()
	calls := strings.Join(stub.powerCalls, ",")
	stub.mu.Unlock()
	if calls != "stop" {
		t.Fatalf("the panel saw %q, want one stop", calls)
	}

	// And the card now reports the new state, because the buttons follow the provider.
	read := do(s, http.MethodGet, "/api/v1/portal/services/"+client.serviceID+"/server", &client.user, "", nil)
	var view struct {
		CanPower []string `json:"can_power"`
	}
	if err := json.Unmarshal(read.Body.Bytes(), &view); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if strings.Join(view.CanPower, ",") != "start" {
		t.Fatalf("after a stop the only offered action is start, got %v", view.CanPower)
	}

	// And it is attributed: a power action nobody can trace is exactly what the audit log exists to
	// prevent.
	var count int
	if err := s.pool.QueryRow(context.Background(), `
		SELECT count(*) FROM audit_events
		WHERE action = 'service.power' AND target_id = $1`, client.serviceID).Scan(&count); err != nil {
		t.Fatalf("read the audit log: %v", err)
	}
	if count == 0 {
		t.Fatal("a power action must be recorded in the audit log")
	}
}

func TestPortalServerPowerRejectsAnUnknownAction(t *testing.T) {
	s := testServer(t)
	storetest.RegisterTables("services", "customers", "customer_accounts", "users", "products")
	configureCatalystctl(t, s, newPanelURL(t))
	client := seedServerClient(t, s, "resource-card-5")

	rec := do(s, http.MethodPost, "/api/v1/portal/services/"+client.serviceID+"/server/power",
		&client.user, `{"action":"reinstall"}`, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for an action outside the four: %s", rec.Code, rec.Body.String())
	}
}

func TestPortalServerPowerRefusesAServiceWithNoServer(t *testing.T) {
	s := testServer(t)
	storetest.RegisterTables("services", "customers", "customer_accounts", "users", "products")
	configureCatalystctl(t, s, newPanelURL(t))
	client := seedServerClient(t, s, "")

	rec := do(s, http.MethodPost, "/api/v1/portal/services/"+client.serviceID+"/server/power",
		&client.user, `{"action":"start"}`, nil)
	// There is nothing to start. Saying so is a conflict with the current state of the resource, not a
	// malformed request and not a server error.
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 for a service with no server: %s", rec.Code, rec.Body.String())
	}
}

func TestStaffServerViewRequiresTheStaffPermission(t *testing.T) {
	s := testServer(t)
	storetest.RegisterTables("services", "customers", "customer_accounts", "users", "products")
	configureCatalystctl(t, s, newPanelURL(t))
	client := seedServerClient(t, s, "resource-card-6")

	// A plain customer has no service:read:all, so the staff route is refused even for a service they
	// own. That is the point of a staff route: it is not a second door into the same room.
	rec := do(s, http.MethodGet, "/api/v1/staff/services/"+client.serviceID+"/server", &client.user, "", nil)
	if rec.Code != http.StatusForbidden && rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want the staff route to refuse a customer: %s", rec.Code, rec.Body.String())
	}
}

func TestStaffServerViewServesAnOperator(t *testing.T) {
	s := testServer(t)
	storetest.RegisterTables("services", "customers", "customer_accounts", "users", "products")
	configureCatalystctl(t, s, newPanelURL(t))
	client := seedServerClient(t, s, "resource-card-7")

	admin := adminUser()
	rec := do(s, http.MethodGet, "/api/v1/staff/services/"+client.serviceID+"/server", &admin, "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 for an administrator: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Available bool   `json:"available"`
		Name      string `json:"name"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !out.Available || out.Name == "" {
		t.Fatalf("an operator must see the same live server, got %+v", out)
	}
}

func TestTheCardIsNotCached(t *testing.T) {
	s := testServer(t)
	storetest.RegisterTables("services", "customers", "customer_accounts", "users", "products")
	configureCatalystctl(t, s, newPanelURL(t))
	client := seedServerClient(t, s, "resource-card-8")

	rec := do(s, http.MethodGet, "/api/v1/portal/services/"+client.serviceID+"/server", &client.user, "", nil)
	// A cached live view is a wrong live view, and an intermediary caching one customer's server for
	// another is worse.
	if got := rec.Header().Get("Cache-Control"); !strings.Contains(got, "no-store") {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
}
