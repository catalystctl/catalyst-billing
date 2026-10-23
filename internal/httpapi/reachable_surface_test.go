// The reachable-surface contract: three reads that had no screen, and four route pairs that
// look like duplicates and are deliberately kept.
//
// This file is the executable half of docs/reachability.md. The sweep (`scripts/reachability.sh`)
// proves that a view references each surface; it cannot prove *which* permission guards the
// route, nor that the two spellings of a payout still move the same money. Both are asserted
// here, because both are decisions that are made once and then quietly undone by a later edit:
//
//   - The pending-upgrade queue is an operations read. Guarding it on `catalog:write` — the
//     permission the graph beside it needs — is what made it invisible to the people who answer
//     the ticket. The test pins `service:read:all` so it cannot drift back.
//   - The portal affiliate view resolves the affiliate from the session. The test proves a second
//     customer cannot read the first one's code, because that is the property the screen depends
//     on and the one an id-in-the-URL refactor would break silently.
//   - The all-affiliates payout report and the per-affiliate reconciliation are two reads of one
//     ledger. They must agree, or the summary on `/affiliates` becomes a number nobody can check.
//   - The kept aliases (`/payout` and `/payouts`) must settle the same balance. A duplicate route
//     is only acceptable while both spellings are the same operation; the moment one of them
//     moves a different balance, the "duplicate" is a defect with two front doors.
package httpapi_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/catalyst/billing/internal/growth"
	"github.com/jackc/pgx/v5"
)

// TestReachableSurfaceGuards pins the permission of each surface this pass made reachable.
//
// The set is deliberately small: it is the routes whose *guard* was the reason a screen could
// not exist, plus the four alias pairs whose survival is a decision rather than an accident.
// The rest of the domain is pinned by the service-lifecycle and growth route tables.
func TestReachableSurfaceGuards(t *testing.T) {
	env := newAuthEnv(t)
	env.srv.Handler()

	want := map[string]string{
		// The queue: an operations read, not catalog configuration.
		"GET /api/v1/staff/upgrades": "service:read:all",
		// The graph: catalog configuration, beside the queue on the same screen.
		"GET /api/v1/staff/upgrade-graph": "catalog:write",
		// The customer's own view: their own record, resolved from the session.
		"GET /api/v1/portal/affiliate": "customer:read:self",
		// The all-affiliates report and the per-payee reconciliation: one question asked twice,
		// so one permission.
		"GET /api/v1/staff/affiliates/payouts":      "report:read",
		"GET /api/v1/staff/affiliates/{id}/payouts": "report:read",

		// The payout pair. They disagree about the guard on purpose — `credit:write` on the older
		// route, `credit:read:all` on the newer one, which is a known borrow documented in
		// affiliates.go — but both must stay served: retiring either breaks a caller.
		"POST /api/v1/staff/affiliates/{id}/payout":  "credit:write",
		"POST /api/v1/staff/affiliates/{id}/payouts": "credit:read:all",

		// The void pair. Not a plain duplicate: `void-reason` also writes invoices.void_reason.
		"POST /api/v1/staff/invoices/{id}/void-reason": "invoice:void",
		"POST /api/v1/staff/invoices/{id}/void":        "invoice:void",

		// The session pair. The reads answer two questions — the caller's own sessions
		// (customer:read:all) versus every account's (staff:manage) — so "deduplicating" them
		// would either widen one guard or narrow the other. The two DELETEs share staff:manage
		// and still differ: the security route refuses your own current session (that is a
		// sign-out), the older one performs it and clears the cookies.
		"GET /api/v1/staff/sessions":                  "customer:read:all",
		"GET /api/v1/staff/security/sessions":         "staff:manage",
		"DELETE /api/v1/staff/sessions/{id}":          "staff:manage",
		"DELETE /api/v1/staff/security/sessions/{id}": "staff:manage",

		// The attachment pair. Not a duplicate either: the portal route resolves the caller's own
		// customer, so a staff login attaching to a customer's ticket needs the staff route.
		"POST /api/v1/portal/tickets/{id}/attachments": "ticket:write:self",
		"POST /api/v1/staff/tickets/{id}/attachments":  "ticket:write:all",
	}

	seen := map[string]bool{}
	for _, rt := range env.srv.Routes() {
		permission, ok := want[rt.Path()]
		if !ok {
			continue
		}
		seen[rt.Path()] = true
		if string(rt.Permission) != permission {
			t.Errorf("%s permission = %q, want %q", rt.Path(), rt.Permission, permission)
		}
	}
	for path := range want {
		if !seen[path] {
			t.Errorf("%s is not in the route table: a screen or an integration was built against a route that is gone", path)
		}
	}
}

// TestStaffUpgradeQueueIsReadableWithoutCatalogWrite is the acceptance criterion stated as a
// test: a holder of service:read:all sees the queue, and a holder of the graph's permission sees
// the graph. It reads through the real handler, because the table above only proves the route is
// registered — a handler that refused a support session for its own reasons would pass that test
// and still leave the screen empty.
func TestStaffUpgradeQueueIsReadableWithoutCatalogWrite(t *testing.T) {
	env := newStaffEnv(t)
	customer, _ := env.seedCustomer(t, "queue-reader@example.test", "Queue Owner")
	_, _ = env.seedUpgradableService(t, customer)

	// Support holds service:read:all and not catalog:write. The queue must answer, and the graph
	// must refuse — which is exactly why the screen renders two halves instead of one refusal.
	rec := env.get(t, "/api/v1/staff/upgrades", env.support)
	if rec.Code != http.StatusOK {
		t.Fatalf("support reading the queue: status = %d, want 200; body %s", rec.Code, rec.Body)
	}
	if body := decodeBody[map[string]any](t, rec); body["upgrades"] == nil {
		t.Fatalf("the queue answered without an upgrades array: %s", rec.Body)
	}

	if rec := env.get(t, "/api/v1/staff/upgrade-graph", env.support); rec.Code != http.StatusForbidden {
		t.Fatalf("support reading the graph: status = %d, want 403", rec.Code)
	}

	// And the queue is still not a customer route. `loginAsCustomer` seeds its own customer, so
	// it takes an address of its own rather than reusing the one above.
	custCookie, _ := env.loginAsCustomer(t, "queue-reader-customer@example.test")
	if rec := env.get(t, "/api/v1/staff/upgrades", custCookie); rec.Code != http.StatusForbidden {
		t.Fatalf("a customer reading the staff queue: status = %d, want 403", rec.Code)
	}
}

// TestPortalAffiliateIsScopedToTheSession is the property the portal screen depends on: the
// affiliate is resolved from the caller's session, so the payload carries no identifier a client
// could substitute. A second customer gets their own answer — `not_an_affiliate` — never the
// first one's code.
func TestPortalAffiliateIsScopedToTheSession(t *testing.T) {
	env := newAuthEnv(t)
	ctx := context.Background()

	_, ownerUser := env.seedCustomer(t, "aff-owner@example.test", "Aff Owner")
	env.seedCustomer(t, "aff-other@example.test", "Aff Other")

	if err := env.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := growth.New(env.pool).Create(ctx, tx, growth.NewAffiliate{
			UserID: ownerUser, Code: "OWNER10", CommissionRate: "10%", PayoutMethod: "manual",
		})
		return err
	}); err != nil {
		t.Fatalf("seed affiliate: %v", err)
	}

	ownerCookie, _ := env.login(t, "aff-owner@example.test", "customer password 1")
	rec := env.get(t, "/api/v1/portal/affiliate", ownerCookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("the affiliate reading their own record: status = %d; body %s", rec.Code, rec.Body)
	}
	body := decodeBody[map[string]any](t, rec)
	if code, _ := body["code"].(string); code != "OWNER10" {
		t.Fatalf("code = %q, want OWNER10", code)
	}
	// The two arrays the screen renders. `null` would be a payload the page has to guess at, and
	// the page's normalisation exists for a build that changes this — it must not be needed today.
	if _, ok := body["referrals"].([]any); !ok {
		t.Fatalf("referrals is %T, want an array: %s", body["referrals"], rec.Body)
	}
	if _, ok := body["balances"].([]any); !ok {
		t.Fatalf("balances is %T, want an array: %s", body["balances"], rec.Body)
	}

	// The other customer is not an affiliate, and the answer must not mention the first one.
	otherCookie, _ := env.login(t, "aff-other@example.test", "customer password 1")
	rec = env.get(t, "/api/v1/portal/affiliate", otherCookie)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("a non-affiliate: status = %d, want 404; body %s", rec.Code, rec.Body)
	}
	if code := problemCode(t, rec); code != "not_an_affiliate" {
		t.Fatalf("a non-affiliate: code = %q, want not_an_affiliate", code)
	}
	if containsString(rec.Body.String(), "OWNER10") {
		t.Fatalf("the non-affiliate's answer leaked the other affiliate's code: %s", rec.Body)
	}
}

// TestPayoutReportAgreesWithTheReconciliation is the invariant behind putting the summary above
// the detail on `/affiliates`: the all-payees report and the per-payee reconciliation are two
// reads of one ledger, and a screen that showed both would be showing two numbers for the same
// debt if they ever disagreed.
func TestPayoutReportAgreesWithTheReconciliation(t *testing.T) {
	env := newStaffEnv(t)
	affiliateID, _ := env.seedAffiliate(t, "AGREE10", "10%")
	env.seedReferredCommission(t, affiliateID, 1250)

	// The all-payees report.
	rec := env.get(t, "/api/v1/staff/affiliates/payouts", env.billing)
	if rec.Code != http.StatusOK {
		t.Fatalf("the payout report: status = %d; body %s", rec.Code, rec.Body)
	}
	report := decodeBody[struct {
		Data   []map[string]any `json:"data"`
		Totals []map[string]any `json:"totals"`
	}](t, rec)

	reportOwed, found := 0.0, false
	for _, line := range report.Data {
		if id, _ := line["affiliate_id"].(string); id != affiliateID {
			continue
		}
		found = true
		owed, _ := line["owed"].(map[string]any)
		reportOwed, _ = owed["minor"].(float64)
	}
	if !found {
		t.Fatalf("the payout report does not name affiliate %s: %s", affiliateID, rec.Body)
	}
	if reportOwed == 0 {
		t.Fatalf("the report says nothing is owed on a 1250 commission; the seed or the query is wrong: %s", rec.Body)
	}

	// The reconciliation for that one payee.
	rec = env.get(t, "/api/v1/staff/affiliates/"+affiliateID+"/payouts", env.billing)
	if rec.Code != http.StatusOK {
		t.Fatalf("the reconciliation: status = %d; body %s", rec.Code, rec.Body)
	}
	detail := decodeBody[struct {
		Data []map[string]any `json:"data"`
	}](t, rec)

	detailOwed := 0.0
	for _, line := range detail.Data {
		if currency, _ := line["currency"].(string); currency != "USD" {
			continue
		}
		outstanding, _ := line["outstanding"].(map[string]any)
		detailOwed, _ = outstanding["minor"].(float64)
	}

	if reportOwed != detailOwed {
		t.Fatalf("the report says %v is owed and the reconciliation says %v: the two reads of one "+
			"ledger disagree, so /affiliates would show two numbers for one debt", reportOwed, detailOwed)
	}
}

// TestPayoutAliasesSettleTheSameBalance proves the kept alias is a real alias: a payout recorded
// through the older spelling settles the debt the newer spelling reports, and a second attempt
// is refused as `nothing_to_pay` rather than paying the same commission twice.
func TestPayoutAliasesSettleTheSameBalance(t *testing.T) {
	env := newStaffEnv(t)
	affiliateID, _ := env.seedAffiliate(t, "ALIAS10", "10%")
	env.seedReferredCommission(t, affiliateID, 500)

	// The older spelling: credit:write, answers with the affiliate and the amount.
	rec := env.postJSON(t, "/api/v1/staff/affiliates/"+affiliateID+"/payout",
		env.billing, env.billingCSRF, map[string]any{"currency": "USD", "method": "manual", "reference": "ALIAS-1"})
	if rec.Code != http.StatusOK {
		t.Fatalf("the alias payout: status = %d, want 200; body %s", rec.Code, rec.Body)
	}

	// The newer spelling now has nothing to settle. If the alias had written to a different
	// ledger, this would pay the same 500 a second time.
	rec = env.postJSON(t, "/api/v1/staff/affiliates/"+affiliateID+"/payouts",
		env.billing, env.billingCSRF, map[string]any{"currency": "USD", "method": "manual", "reference": "ALIAS-2"})
	if rec.Code != http.StatusConflict {
		t.Fatalf("a second payout: status = %d, want 409; body %s", rec.Code, rec.Body)
	}
	if code := problemCode(t, rec); code != "nothing_to_pay" {
		t.Fatalf("a second payout: code = %q, want nothing_to_pay", code)
	}

	// And the reconciliation reports exactly one payout, through the same ledger.
	rec = env.get(t, "/api/v1/staff/affiliates/"+affiliateID+"/payouts", env.billing)
	if rec.Code != http.StatusOK {
		t.Fatalf("the reconciliation after a payout: status = %d; body %s", rec.Code, rec.Body)
	}
	body := decodeBody[struct {
		History []map[string]any `json:"history"`
	}](t, rec)
	if len(body.History) != 1 {
		t.Fatalf("history has %d payouts after one payout; body %s", len(body.History), rec.Body)
	}
}
