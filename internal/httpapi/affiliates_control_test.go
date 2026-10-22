package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/catalyst/billing/internal/growth"
	"github.com/jackc/pgx/v5"
)

// Staff control over an affiliate (0053): suspend, reinstate, delete, and a custom rate.
//
// These assert the HTTP contract around the domain rules that internal/growth owns. The
// domain tests prove the money does not move; these prove the routes that let an operator
// cause it exist, are guarded by credit:write rather than the read permission the list uses,
// and answer with the state the operator now sees.

// affiliateRequest performs a mutation with a CSRF token and an idempotency key, for the
// methods the shared postJSON helper does not cover.
func (e *authEnv) affiliateRequest(t *testing.T, method, path string, cookie *http.Cookie,
	csrf string, body any) *httptest.ResponseRecorder {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	req := httptest.NewRequest(method, path, bytesReader(encoded))
	req.Header.Set("Content-Type", "application/json")
	if cookie != nil {
		req.AddCookie(cookie)
	}
	if csrf != "" {
		req.Header.Set("X-CSRF-Token", csrf)
	}
	req.Header.Set("Idempotency-Key",
		"affiliate-"+method+"-"+path+"-"+time.Now().Format("150405.000000000"))
	rec := httptest.NewRecorder()
	e.srv.Handler().ServeHTTP(rec, req)
	return rec
}

// affiliateBody is the wire shape of one affiliate.
type affiliateBody struct {
	ID              string  `json:"id"`
	Code            string  `json:"code"`
	CommissionRate  string  `json:"commission_rate"`
	Enabled         bool    `json:"enabled"`
	Status          string  `json:"status"`
	SuspendedAt     *string `json:"suspended_at"`
	SuspendedReason string  `json:"suspended_reason"`
	DeletedAt       *string `json:"deleted_at"`
	DeletedReason   string  `json:"deleted_reason"`
}

// decodeAffiliate reads one affiliate out of a response.
func decodeAffiliate(t *testing.T, rec *httptest.ResponseRecorder) affiliateBody {
	t.Helper()
	var body affiliateBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v; body %s", err, rec.Body)
	}
	return body
}

// seedPaidOrderFor adds another paid order and its paid invoice for an existing customer, so
// a test can ask what the *next* order earns.
func (e *authEnv) seedPaidOrderFor(t *testing.T, customerID string, totalMinor int64, number string) string {
	t.Helper()
	ctx := context.Background()
	var orderID string
	if err := e.pool.QueryRow(ctx, `
		INSERT INTO orders (customer_id, status, currency, total_minor)
		VALUES ($1, 'paid', 'USD', $2) RETURNING id`, customerID, totalMinor).Scan(&orderID); err != nil {
		t.Fatalf("order: %v", err)
	}
	var invoiceID string
	if err := e.pool.QueryRow(ctx, `
		INSERT INTO invoices (number, customer_id, order_id, status, currency,
			subtotal_minor, tax_minor, total_minor, amount_paid_minor, issued_at, snapshot)
		VALUES ($1, $2, $3, 'paid', 'USD', $4, 0, $4, $4, now(), '{"schema_version":1}'::jsonb)
		RETURNING id`, number, customerID, orderID, totalMinor).Scan(&invoiceID); err != nil {
		t.Fatalf("invoice: %v", err)
	}
	return invoiceID
}

// recordCommissionOnInvoice runs the commission path for a paid invoice and returns its
// error, so a test can assert the refusal a suspended affiliate produces.
func (e *authEnv) recordCommissionOnInvoice(t *testing.T, invoiceID string) error {
	t.Helper()
	ctx := context.Background()
	return e.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := growth.RecordCommissionForPaidInvoice(ctx, tx, invoiceID)
		return err
	})
}

// ------------------------------------------------------------- permissions --

// The four controls change what the programme pays, so they need credit:write — not the
// report:read that reading the list needs, and not something support holds.
func TestAffiliateControlRoutesRequireCreditWrite(t *testing.T) {
	env := newStaffEnv(t)
	affiliateID, _ := env.seedAffiliate(t, "CTRLPERM", "10%")
	base := "/api/v1/staff/affiliates/" + affiliateID

	cases := []struct {
		name   string
		method string
		path   string
		body   map[string]any
	}{
		{"rate", http.MethodPatch, base, map[string]any{"commission_rate": "12%"}},
		{"suspend", http.MethodPost, base + "/suspend", map[string]any{"reason": "policy"}},
		{"reinstate", http.MethodPost, base + "/reinstate", map[string]any{}},
		{"delete", http.MethodDelete, base, map[string]any{"reason": "left"}},
	}
	for _, tc := range cases {
		// Support holds no credit:write, so it is refused on every one of them.
		if rec := env.affiliateRequest(t, tc.method, tc.path, env.support, env.supportCSRF,
			tc.body); rec.Code != http.StatusForbidden {
			t.Fatalf("%s as support: status %d, want 403; body %s", tc.name, rec.Code, rec.Body)
		}
		// Anonymous is a 401, not a 403: there is no session to authorise.
		if rec := env.affiliateRequest(t, tc.method, tc.path, nil, "", tc.body); rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s anonymously: status %d, want 401; body %s", tc.name, rec.Code, rec.Body)
		}
	}

	// The billing role does hold it, and the same routes succeed for it.
	if rec := env.affiliateRequest(t, http.MethodPatch, base, env.billing, env.billingCSRF,
		map[string]any{"commission_rate": "12%"}); rec.Code != http.StatusOK {
		t.Fatalf("rate as billing: status %d, body %s", rec.Code, rec.Body)
	}
	if rec := env.affiliateRequest(t, http.MethodPost, base+"/suspend", env.billing, env.billingCSRF,
		map[string]any{"reason": "policy"}); rec.Code != http.StatusOK {
		t.Fatalf("suspend as billing: status %d, body %s", rec.Code, rec.Body)
	}
	if rec := env.affiliateRequest(t, http.MethodPost, base+"/reinstate", env.billing, env.billingCSRF,
		map[string]any{}); rec.Code != http.StatusOK {
		t.Fatalf("reinstate as billing: status %d, body %s", rec.Code, rec.Body)
	}
	if rec := env.affiliateRequest(t, http.MethodDelete, base, env.billing, env.billingCSRF,
		map[string]any{"reason": "left"}); rec.Code != http.StatusOK {
		t.Fatalf("delete as billing: status %d, body %s", rec.Code, rec.Body)
	}
}

// --------------------------------------------------------------- custom rate --

// A custom rate is stored canonically, is bounded at 100%, and applies to the next commission
// rather than rewriting the ones already recorded.
func TestAffiliateCommissionRateIsCustomAndNotRetroactive(t *testing.T) {
	env := newStaffEnv(t)
	affiliateID, _ := env.seedAffiliate(t, "RATECUSTOM", "10%")
	customerID, _ := env.seedReferredCommission(t, affiliateID, 12345)

	// The first commission: 10% of 12345 is 1234.5, half-up 1235.
	var firstMinor int64
	var firstRate string
	if err := env.pool.QueryRow(context.Background(), `
		SELECT commission_minor, commission_rate FROM affiliate_referrals
		WHERE affiliate_id = $1 AND order_id IS NOT NULL`, affiliateID).Scan(&firstMinor, &firstRate); err != nil {
		t.Fatalf("load commission: %v", err)
	}
	if firstMinor != 1235 || firstRate != "1/10" {
		t.Fatalf("first commission = %d at %q, want 1235 at 1/10", firstMinor, firstRate)
	}

	rec := env.affiliateRequest(t, http.MethodPatch, "/api/v1/staff/affiliates/"+affiliateID,
		env.billing, env.billingCSRF, map[string]any{"commission_rate": "30%"})
	if rec.Code != http.StatusOK {
		t.Fatalf("set rate: status %d, body %s", rec.Code, rec.Body)
	}
	updated := decodeAffiliate(t, rec)
	if updated.CommissionRate != "3/10" {
		t.Fatalf("rate = %q, want 3/10 (the canonical rational)", updated.CommissionRate)
	}
	if updated.Status != growth.AffiliateActive {
		t.Fatalf("status = %q, want active", updated.Status)
	}

	// The recorded commission is untouched — the rate is captured on the row, which is what
	// makes a rate change non-retroactive rather than a promise.
	var minor int64
	var rate string
	if err := env.pool.QueryRow(context.Background(), `
		SELECT commission_minor, commission_rate FROM affiliate_referrals
		WHERE affiliate_id = $1 AND order_id IS NOT NULL`, affiliateID).Scan(&minor, &rate); err != nil {
		t.Fatalf("load commission: %v", err)
	}
	if minor != 1235 || rate != "1/10" {
		t.Fatalf("the recorded commission changed to %d at %q, want 1235 at 1/10", minor, rate)
	}

	// The next order earns the new rate: 30% of 20000 is 6000.
	invoiceID := env.seedPaidOrderFor(t, customerID, 20000, "INV-RATE-2")
	if err := env.recordCommissionOnInvoice(t, invoiceID); err != nil {
		t.Fatalf("commission at the new rate: %v", err)
	}
	var nextMinor int64
	if err := env.pool.QueryRow(context.Background(), `
		SELECT commission_minor FROM affiliate_referrals
		WHERE order_id = (SELECT order_id FROM invoices WHERE id = $1)`, invoiceID).Scan(&nextMinor); err != nil {
		t.Fatalf("load second commission: %v", err)
	}
	if nextMinor != 6000 {
		t.Fatalf("second commission = %d, want 6000", nextMinor)
	}

	// Above 100% is refused as invalid input, and nothing changes.
	for _, bad := range []string{"150%", "1.5", "3/2", "-1/10", "abc", ""} {
		rec := env.affiliateRequest(t, http.MethodPatch, "/api/v1/staff/affiliates/"+affiliateID,
			env.billing, env.billingCSRF, map[string]any{"commission_rate": bad})
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("rate %q: status %d, want 422; body %s", bad, rec.Code, rec.Body)
		}
		if !containsCode(rec.Body.Bytes(), "invalid_input") {
			t.Fatalf("rate %q: body %s does not carry invalid_input", bad, rec.Body)
		}
	}
	// A body that names no field is refused rather than treated as a no-op: the route edits
	// exactly one thing.
	rec = env.affiliateRequest(t, http.MethodPatch, "/api/v1/staff/affiliates/"+affiliateID,
		env.billing, env.billingCSRF, map[string]any{})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("empty patch: status %d, want 422; body %s", rec.Code, rec.Body)
	}
	// The rate survived every refusal.
	after := env.get(t, "/api/v1/staff/affiliates", env.billing)
	if !strings.Contains(after.Body.String(), "3/10") {
		t.Fatalf("the list no longer shows the custom rate: %s", after.Body)
	}
}

// --------------------------------------------------------- suspend/reinstate --

// Suspending stops the link accruing commission, and reinstating starts it again. The
// commission already earned is untouched and still owed.
func TestAffiliateSuspendStopsCommissionAndReinstateRestoresIt(t *testing.T) {
	env := newStaffEnv(t)
	affiliateID, _ := env.seedAffiliate(t, "SUSPENDME", "10%")
	customerID, _ := env.seedReferredCommission(t, affiliateID, 12345)

	// A suspension without a reason is refused: a ban the next operator cannot understand is
	// not a decision they can review.
	rec := env.affiliateRequest(t, http.MethodPost, "/api/v1/staff/affiliates/"+affiliateID+"/suspend",
		env.billing, env.billingCSRF, map[string]any{})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("suspend without a reason: status %d, want 422; body %s", rec.Code, rec.Body)
	}

	rec = env.affiliateRequest(t, http.MethodPost, "/api/v1/staff/affiliates/"+affiliateID+"/suspend",
		env.billing, env.billingCSRF, map[string]any{"reason": "chargebacks"})
	if rec.Code != http.StatusOK {
		t.Fatalf("suspend: status %d, body %s", rec.Code, rec.Body)
	}
	suspended := decodeAffiliate(t, rec)
	if suspended.Status != growth.AffiliateSuspended || suspended.Enabled {
		t.Fatalf("after suspension: %+v", suspended)
	}
	if suspended.SuspendedAt == nil || suspended.SuspendedReason != "chargebacks" {
		t.Fatalf("the suspension was not recorded: %+v", suspended)
	}

	// The next order of the referred customer earns nothing.
	invoiceID := env.seedPaidOrderFor(t, customerID, 20000, "INV-SUSP-2")
	if err := env.recordCommissionOnInvoice(t, invoiceID); err == nil {
		t.Fatal("a suspended affiliate's referral earned a commission")
	}
	var rows int
	if err := env.pool.QueryRow(context.Background(), `
		SELECT count(*) FROM affiliate_referrals
		WHERE affiliate_id = $1 AND order_id IS NOT NULL`, affiliateID).Scan(&rows); err != nil {
		t.Fatalf("count commissions: %v", err)
	}
	if rows != 1 {
		t.Fatalf("%d commissions after a suspended affiliate's order, want 1 (the pre-ban one)", rows)
	}

	// What was earned before the ban is still owed.
	report := env.payoutReportOf(t, affiliateID, "?period=30d")
	if len(report.Data) != 1 || report.Data[0].Outstanding["minor"].(float64) != 1235 {
		t.Fatalf("outstanding after suspension = %+v, want 1235", report.Data)
	}

	rec = env.affiliateRequest(t, http.MethodPost, "/api/v1/staff/affiliates/"+affiliateID+"/reinstate",
		env.billing, env.billingCSRF, map[string]any{})
	if rec.Code != http.StatusOK {
		t.Fatalf("reinstate: status %d, body %s", rec.Code, rec.Body)
	}
	reinstated := decodeAffiliate(t, rec)
	if reinstated.Status != growth.AffiliateActive || !reinstated.Enabled {
		t.Fatalf("after reinstatement: %+v", reinstated)
	}
	if reinstated.SuspendedAt != nil || reinstated.SuspendedReason != "" {
		t.Fatalf("the suspension record survived reinstatement: %+v", reinstated)
	}
	if err := env.recordCommissionOnInvoice(t, invoiceID); err != nil {
		t.Fatalf("commission after reinstatement: %v", err)
	}
}

// ------------------------------------------------------------------ delete --

// Deleting an affiliate keeps the ledger: the commission rows stay, the debt stays payable,
// and the payee leaves the default list while remaining reachable for an audit.
func TestAffiliateDeletePreservesCommissionAndDebt(t *testing.T) {
	env := newStaffEnv(t)
	affiliateID, _ := env.seedAffiliate(t, "DELETEME", "10%")
	env.seedReferredCommission(t, affiliateID, 12345)

	rec := env.affiliateRequest(t, http.MethodDelete, "/api/v1/staff/affiliates/"+affiliateID,
		env.billing, env.billingCSRF, map[string]any{"reason": "left the programme"})
	if rec.Code != http.StatusOK {
		t.Fatalf("delete: status %d, body %s", rec.Code, rec.Body)
	}
	deleted := decodeAffiliate(t, rec)
	if deleted.Status != growth.AffiliateDeleted || deleted.Enabled {
		t.Fatalf("after deletion: %+v", deleted)
	}
	if deleted.DeletedAt == nil || deleted.DeletedReason != "left the programme" {
		t.Fatalf("the deletion was not recorded: %+v", deleted)
	}

	// The commission row is still there, still worth what it was worth.
	var minor int64
	if err := env.pool.QueryRow(context.Background(), `
		SELECT commission_minor FROM affiliate_referrals
		WHERE affiliate_id = $1 AND order_id IS NOT NULL`, affiliateID).Scan(&minor); err != nil {
		t.Fatalf("the commission row did not survive the deletion: %v", err)
	}
	if minor != 1235 {
		t.Fatalf("commission after deletion = %d, want 1235", minor)
	}

	// The debt is still on the payout report, marked deleted, and still payable.
	report := env.payoutReportOf(t, affiliateID, "?period=30d")
	if len(report.Data) != 1 || report.Data[0].Outstanding["minor"].(float64) != 1235 {
		t.Fatalf("outstanding after deletion = %+v, want 1235", report.Data)
	}
	payout := env.postJSON(t, "/api/v1/staff/affiliates/"+affiliateID+"/payouts",
		env.billing, env.billingCSRF, map[string]any{"currency": "USD"})
	if payout.Code != http.StatusCreated {
		t.Fatalf("paying a deleted affiliate: status %d, body %s", payout.Code, payout.Body)
	}

	// The list hides them by default and shows them on request, so the archive is reachable
	// without putting a deleted payee in front of an operator who is enrolling one.
	active := env.get(t, "/api/v1/staff/affiliates", env.billing)
	if strings.Contains(active.Body.String(), "DELETEME") {
		t.Fatalf("the default list still shows the deleted affiliate: %s", active.Body)
	}
	archived := env.get(t, "/api/v1/staff/affiliates?include_deleted=1", env.billing)
	if !strings.Contains(archived.Body.String(), "DELETEME") {
		t.Fatalf("the archived affiliate is not reachable: %s", archived.Body)
	}
	var listed struct {
		Data []affiliateBody `json:"data"`
	}
	if err := json.Unmarshal(archived.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(listed.Data) != 1 || listed.Data[0].Status != growth.AffiliateDeleted {
		t.Fatalf("archived list = %+v, want one deleted affiliate", listed.Data)
	}

	// Nothing new can be decided about an archived affiliate.
	rec = env.affiliateRequest(t, http.MethodPost, "/api/v1/staff/affiliates/"+affiliateID+"/suspend",
		env.billing, env.billingCSRF, map[string]any{"reason": "too late"})
	if rec.Code != http.StatusConflict {
		t.Fatalf("suspend after delete: status %d, want 409; body %s", rec.Code, rec.Body)
	}
	if !containsCode(rec.Body.Bytes(), "affiliate_deleted") {
		t.Fatalf("suspend after delete: body %s does not carry affiliate_deleted", rec.Body)
	}
	rec = env.affiliateRequest(t, http.MethodPost, "/api/v1/staff/affiliates/"+affiliateID+"/reinstate",
		env.billing, env.billingCSRF, map[string]any{})
	if rec.Code != http.StatusConflict {
		t.Fatalf("reinstate after delete: status %d, want 409; body %s", rec.Code, rec.Body)
	}
	rec = env.affiliateRequest(t, http.MethodPatch, "/api/v1/staff/affiliates/"+affiliateID,
		env.billing, env.billingCSRF, map[string]any{"commission_rate": "20%"})
	if rec.Code != http.StatusConflict {
		t.Fatalf("rate change after delete: status %d, want 409; body %s", rec.Code, rec.Body)
	}
}

// An unknown affiliate is a 404 on every control, not a 500 from a foreign key.
func TestAffiliateControlUnknownID(t *testing.T) {
	env := newStaffEnv(t)
	const missing = "/api/v1/staff/affiliates/00000000-0000-0000-0000-000000000000"
	cases := []struct {
		name   string
		method string
		path   string
		body   map[string]any
	}{
		{"rate", http.MethodPatch, missing, map[string]any{"commission_rate": "10%"}},
		{"suspend", http.MethodPost, missing + "/suspend", map[string]any{"reason": "policy"}},
		{"reinstate", http.MethodPost, missing + "/reinstate", map[string]any{}},
		{"delete", http.MethodDelete, missing, map[string]any{"reason": "left"}},
	}
	for _, tc := range cases {
		rec := env.affiliateRequest(t, tc.method, tc.path, env.billing, env.billingCSRF, tc.body)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s on an unknown affiliate: status %d, want 404; body %s",
				tc.name, rec.Code, rec.Body)
		}
	}
}

// The state changes are audited with the operator as actor and the reason in the diff: the
// audit row is how "who banned this affiliate, and why" is answerable later.
func TestAffiliateStateChangesAreAudited(t *testing.T) {
	env := newStaffEnv(t)
	affiliateID, _ := env.seedAffiliate(t, "AUDITME", "10%")

	if rec := env.affiliateRequest(t, http.MethodPost, "/api/v1/staff/affiliates/"+affiliateID+"/suspend",
		env.billing, env.billingCSRF, map[string]any{"reason": "fraud"}); rec.Code != http.StatusOK {
		t.Fatalf("suspend: status %d, body %s", rec.Code, rec.Body)
	}
	var action string
	var changes map[string]any
	if err := env.pool.QueryRow(context.Background(), `
		SELECT action, changes FROM audit_events
		WHERE target_type = 'affiliate' AND target_id = $1
		ORDER BY created_at DESC LIMIT 1`, affiliateID).Scan(&action, &changes); err != nil {
		t.Fatalf("load audit row: %v", err)
	}
	if action != "affiliate.suspend" {
		t.Fatalf("audit action = %q, want affiliate.suspend", action)
	}
	if reason, _ := changes["reason"].(string); reason != "fraud" {
		t.Fatalf("audit changes do not carry the reason: %+v", changes)
	}
}
