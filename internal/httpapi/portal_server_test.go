package httpapi_test

// The live server card and its power buttons.
//
// These routes hand a customer a button that starts and stops a machine they pay for, so the
// properties worth testing are the ones a mistake makes dangerous:
//
//   - Scoping. Another customer's service id must be indistinguishable from one that does not
//     exist. The distinction is not cosmetic: 403-for-somebody-else's-id confirms the id is real.
//   - Honesty about the provider. A provider that is not configured, an adapter that cannot report
//     state, and a machine that has not been created yet are three different answers, and the panel
//     must not collapse them into an empty card or a 500.
//   - Refusal before effect. An action the provider does not support, and a service with no
//     server, are refused before anything is asked of the provider.

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

// serverViewResponse is the subset of the card these tests assert on.
type serverViewResponse struct {
	ServiceID     string `json:"service_id"`
	ServiceStatus string `json:"service_status"`
	Provisioned   bool   `json:"provisioned"`
	Available     bool   `json:"available"`
	Detail        string `json:"detail"`
	Provider      string `json:"provider"`
}

// provisionService gives the fixture's service a provider-side identity, which is what distinguishes
// "provisioned but unreachable" from "no server yet".
func provisionService(t *testing.T, env *authEnv, serviceID, externalID string) {
	t.Helper()
	if _, err := env.pool.Exec(context.Background(),
		`UPDATE services SET external_id = $2 WHERE id = $1`, serviceID, externalID); err != nil {
		t.Fatalf("give the service a provider identity: %v", err)
	}
}

// A customer reads their own server, and the answer is a 200 even when there is nothing to show:
// the card is a place to explain, not a failure mode.
func TestServerViewExplainsAnUnprovisionedService(t *testing.T) {
	env := newAuthEnv(t)
	tenant := env.createTenant(t, "server-view@example.test", "12.00")
	cookie, _ := env.login(t, "server-view@example.test", "a long enough password")

	rec := env.get(t, "/api/v1/portal/services/"+tenant.serviceID+"/server", cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("view: %d %s", rec.Code, rec.Body)
	}
	var out serverViewResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body)
	}
	if out.Provisioned {
		t.Fatal("a service with no external id reports itself provisioned")
	}
	if out.Available {
		t.Fatal("a service with no server reports live state as available")
	}
	if out.Detail == "" {
		t.Fatal("no sentence explains why the card is empty: the page would render a blank box")
	}
	// The read is a question about now, and a cached answer to it is a wrong answer.
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
}

// Another customer's service must answer exactly as one that has never existed.
func TestServerViewIsScopedToTheCaller(t *testing.T) {
	env := newAuthEnv(t)
	alice := env.createTenant(t, "server-alice@example.test", "12.00")
	env.createTenant(t, "server-bob@example.test", "12.00")
	provisionService(t, env, alice.serviceID, "remote-alice")
	bobCookie, _ := env.login(t, "server-bob@example.test", "a long enough password")

	other := env.get(t, "/api/v1/portal/services/"+alice.serviceID+"/server", bobCookie)
	if other.Code != http.StatusNotFound {
		t.Fatalf("another customer's service answered %d, want 404", other.Code)
	}

	// The same request for an id that genuinely does not exist must be indistinguishable — same
	// status and same problem code — or the 404 becomes an oracle for which ids are real.
	missing := env.get(t,
		"/api/v1/portal/services/0199f8ff-0000-7000-8000-000000000000/server", bobCookie)
	if missing.Code != other.Code {
		t.Fatalf("a nonexistent id answered %d while another customer's answered %d",
			missing.Code, other.Code)
	}
	var otherBody, missingBody struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(other.Body.Bytes(), &otherBody)
	_ = json.Unmarshal(missing.Body.Bytes(), &missingBody)
	if otherBody.Code != missingBody.Code {
		t.Fatalf("problem codes differ: another customer's %q vs nonexistent %q",
			otherBody.Code, missingBody.Code)
	}
}

// A power action on a service that has no server is refused before anything is asked of a provider,
// and the refusal names the reason rather than reporting a generic failure.
func TestPowerRefusesAServiceWithNoServer(t *testing.T) {
	env := newAuthEnv(t)
	tenant := env.createTenant(t, "server-nopower@example.test", "12.00")
	cookie, csrf := env.login(t, "server-nopower@example.test", "a long enough password")

	rec := env.postJSON(t, "/api/v1/portal/services/"+tenant.serviceID+"/server/power",
		cookie, csrf, map[string]any{"action": "start"})
	if rec.Code != http.StatusConflict {
		t.Fatalf("power on an unprovisioned service: %d %s", rec.Code, rec.Body)
	}
	var problem struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &problem)
	if problem.Code != "not_provisioned" {
		t.Fatalf("code = %q, want not_provisioned", problem.Code)
	}
}

// An action outside the four the API accepts is a 400, not a 409 or a silent no-op.
func TestPowerRefusesAnUnknownAction(t *testing.T) {
	env := newAuthEnv(t)
	tenant := env.createTenant(t, "server-badaction@example.test", "12.00")
	provisionService(t, env, tenant.serviceID, "remote-badaction")
	cookie, csrf := env.login(t, "server-badaction@example.test", "a long enough password")

	rec := env.postJSON(t, "/api/v1/portal/services/"+tenant.serviceID+"/server/power",
		cookie, csrf, map[string]any{"action": "reboot"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown action: %d %s", rec.Code, rec.Body)
	}
	var problem struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &problem)
	if problem.Code != "invalid_action" {
		t.Fatalf("code = %q, want invalid_action", problem.Code)
	}
}

// With no provisioner configured, a power request is refused as not-configured rather than reported
// as a provider failure. The difference decides whether the operator checks their provider settings
// or their game panel.
func TestPowerSaysWhenServerControlIsNotConfigured(t *testing.T) {
	env := newAuthEnv(t)
	tenant := env.createTenant(t, "server-noprov@example.test", "12.00")
	provisionService(t, env, tenant.serviceID, "remote-noprov")
	cookie, csrf := env.login(t, "server-noprov@example.test", "a long enough password")

	rec := env.postJSON(t, "/api/v1/portal/services/"+tenant.serviceID+"/server/power",
		cookie, csrf, map[string]any{"action": "start"})
	// No provider is wired in this harness, so the honest answer is one of the two configuration
	// refusals — never a 200 claiming the machine started.
	if rec.Code != http.StatusConflict {
		t.Fatalf("power with no provider: %d %s, want 409", rec.Code, rec.Body)
	}
	var problem struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &problem)
	if problem.Code != "provider_not_configured" && problem.Code != "power_unsupported" {
		t.Fatalf("code = %q, want provider_not_configured or power_unsupported", problem.Code)
	}
}

// The staff read is not a customer route: a customer session must not be able to inspect another
// account's machine through it.
func TestStaffServerViewRefusesACustomerSession(t *testing.T) {
	env := newAuthEnv(t)
	tenant := env.createTenant(t, "server-staffgate@example.test", "12.00")
	cookie, _ := env.login(t, "server-staffgate@example.test", "a long enough password")

	rec := env.get(t, "/api/v1/staff/services/"+tenant.serviceID+"/server", cookie)
	if rec.Code != http.StatusForbidden && rec.Code != http.StatusUnauthorized {
		t.Fatalf("a customer read the staff server card: %d %s", rec.Code, rec.Body)
	}
}
