package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/catalyst/billing/internal/growth"
	"github.com/jackc/pgx/v5"
)

// Affiliate payouts: the reconciliation and the action that records one (GRW-03, ADM-37).
//
// These assert the HTTP contract around the money path that internal/growth owns: who may
// read the report, who may record a payout, that the report reconciles, that a payout is a
// row with an id rather than an edit, and that a second attempt does not pay twice.

// reconciliation is the wire shape of one currency's books.
type reconciliation struct {
	AffiliateID string         `json:"affiliate_id"`
	Code        string         `json:"code"`
	Currency    string         `json:"currency"`
	Earned      map[string]any `json:"earned"`
	Paid        map[string]any `json:"paid"`
	Outstanding map[string]any `json:"outstanding"`
	Payouts     int            `json:"payouts"`
	Reconciles  bool           `json:"reconciles"`
	Rows        []struct {
		Kind       string         `json:"kind"`
		ID         string         `json:"id"`
		OrderID    string         `json:"order_id"`
		InvoiceID  string         `json:"invoice_id"`
		Amount     map[string]any `json:"amount"`
		Status     string         `json:"status"`
		PayoutID   string         `json:"payout_id"`
		OccurredAt string         `json:"occurred_at"`
	} `json:"rows"`
}

// payoutReport is the wire shape of the report endpoint.
type payoutReport struct {
	Data    []reconciliation `json:"data"`
	History []struct {
		ID        string         `json:"id"`
		Currency  string         `json:"currency"`
		Amount    map[string]any `json:"amount"`
		Method    string         `json:"method"`
		Reference string         `json:"reference"`
	} `json:"history"`
	Period struct {
		From time.Time `json:"from"`
		To   time.Time `json:"to"`
	} `json:"period"`
}

// payoutReportOf fetches and decodes the report for one affiliate.
func (e *staffEnv) payoutReportOf(t *testing.T, affiliateID, query string) payoutReport {
	t.Helper()
	rec := e.get(t, "/api/v1/staff/affiliates/"+affiliateID+"/payouts"+query, e.billing)
	if rec.Code != http.StatusOK {
		t.Fatalf("payout report: status %d, body %s", rec.Code, rec.Body)
	}
	var body payoutReport
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v; body %s", err, rec.Body)
	}
	return body
}

// seedRenewalCommission gives the affiliate a renewal commission, so a report has both kinds
// of row and the payout has both to settle.
//
// It goes through growth.RecordRecurringCommission rather than inserting rows: the point is
// that the rows the report reads are the rows the settlement path writes.
func (e *authEnv) seedRenewalCommission(t *testing.T, affiliateID, customerID string, totalMinor int64) string {
	t.Helper()
	ctx := context.Background()

	// The affiliate programme's recurring gate.
	if _, err := e.pool.Exec(ctx, `
		INSERT INTO settings (group_key, key, value) VALUES ('affiliates', 'recurring', 'true')
		ON CONFLICT (group_key, key) DO UPDATE SET value = 'true'`); err != nil {
		t.Fatalf("enable recurring: %v", err)
	}
	t.Cleanup(func() {
		_, _ = e.pool.Exec(context.Background(),
			`DELETE FROM settings WHERE group_key = 'affiliates' AND key = 'recurring'`)
	})

	var productID string
	if err := e.pool.QueryRow(ctx, `
		INSERT INTO products (slug, name) VALUES ('renewal-plan', 'Renewal plan') RETURNING id`,
	).Scan(&productID); err != nil {
		t.Fatalf("product: %v", err)
	}
	var priceID string
	if err := e.pool.QueryRow(ctx, `
		INSERT INTO price_versions (product_id, version, currency, amount_minor, interval,
			anchor_day, anchor_timezone)
		VALUES ($1, 1, 'USD', 5000, 'monthly', 1, 'UTC') RETURNING id`, productID).Scan(&priceID); err != nil {
		t.Fatalf("price: %v", err)
	}
	var subscriptionID string
	if err := e.pool.QueryRow(ctx, `
		INSERT INTO subscriptions (customer_id, price_version_id, status, interval, anchor_day,
			timezone, current_period_start, current_period_end, next_billing_at)
		VALUES ($1, $2, 'active', 'monthly', 1, 'UTC', now(), now() + interval '1 month',
		        now() + interval '1 month') RETURNING id`, customerID, priceID).Scan(&subscriptionID); err != nil {
		t.Fatalf("subscription: %v", err)
	}
	var invoiceID string
	if err := e.pool.QueryRow(ctx, `
		INSERT INTO invoices (number, customer_id, subscription_id, period_key, status, currency,
			subtotal_minor, tax_minor, total_minor, amount_paid_minor, issued_at, snapshot)
		VALUES ('INV-RENEW', $1, $2, 'p-1', 'paid', 'USD', $3, 0, $3, $3, now(),
		        '{"schema_version":1}'::jsonb)
		RETURNING id`, customerID, subscriptionID, totalMinor).Scan(&invoiceID); err != nil {
		t.Fatalf("renewal invoice: %v", err)
	}
	if err := e.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := growth.RecordRecurringCommission(ctx, tx, invoiceID)
		return err
	}); err != nil {
		t.Fatalf("renewal commission: %v", err)
	}
	return invoiceID
}

// The report is a read: report:read, like the affiliate list it belongs with.
func TestAffiliatePayoutReportRequiresReportRead(t *testing.T) {
	env := newStaffEnv(t)
	affiliateID, _ := env.seedAffiliate(t, "RECON", "10%")

	if rec := env.get(t, "/api/v1/staff/affiliates/"+affiliateID+"/payouts", env.billing); rec.Code != http.StatusOK {
		t.Fatalf("billing report: status %d, body %s", rec.Code, rec.Body)
	}
	if rec := env.get(t, "/api/v1/staff/affiliates/"+affiliateID+"/payouts", env.support); rec.Code != http.StatusForbidden {
		t.Fatalf("support report: status %d, want 403", rec.Code)
	}
	if rec := env.get(t, "/api/v1/staff/affiliates/"+affiliateID+"/payouts", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous report: status %d, want 401", rec.Code)
	}
	// An unknown affiliate is a 404, not an empty report.
	if rec := env.get(t, "/api/v1/staff/affiliates/00000000-0000-0000-0000-000000000000/payouts",
		env.billing); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown affiliate: status %d, want 404", rec.Code)
	}
}

// The report states what was earned, what was paid and what is owed, and the rows explain it.
// The renewal commission is included, which is the point of GRW-03: a renewal is attributable
// to the referral recorded on the customer.
func TestAffiliatePayoutReportReconciles(t *testing.T) {
	env := newStaffEnv(t)
	affiliateID, _ := env.seedAffiliate(t, "RECON2", "10%")
	customerID, orderID := env.seedReferredCommission(t, affiliateID, 12345)
	renewalInvoice := env.seedRenewalCommission(t, affiliateID, customerID, 8000)

	body := env.payoutReportOf(t, affiliateID, "?period=30d")
	if len(body.Data) != 1 {
		t.Fatalf("%d currency lines, want 1: %+v", len(body.Data), body.Data)
	}
	line := body.Data[0]
	// 10% of 12345 half-up is 1235, and 10% of 8000 is 800.
	if line.Earned["minor"].(float64) != 2035 {
		t.Fatalf("earned = %v, want 2035", line.Earned)
	}
	if line.Paid["minor"].(float64) != 0 {
		t.Fatalf("paid = %v, want 0", line.Paid)
	}
	if line.Outstanding["minor"].(float64) != 2035 {
		t.Fatalf("outstanding = %v, want 2035", line.Outstanding)
	}
	if !line.Reconciles {
		t.Fatalf("the report does not reconcile: %+v", line)
	}

	var kinds []string
	var commissionOrder, renewalRow string
	for _, row := range line.Rows {
		kinds = append(kinds, row.Kind)
		switch row.Kind {
		case growth.PayoutRowCommission:
			commissionOrder = row.OrderID
		case growth.PayoutRowRenewal:
			renewalRow = row.InvoiceID
		}
		if row.PayoutID != "" {
			t.Fatalf("row %s is settled by a payout that has not happened", row.ID)
		}
	}
	if len(kinds) != 2 {
		t.Fatalf("%d rows, want an order commission and a renewal: %v", len(kinds), kinds)
	}
	if commissionOrder != orderID {
		t.Fatalf("order commission points at %q, want %q", commissionOrder, orderID)
	}
	if renewalRow != renewalInvoice {
		t.Fatalf("renewal row points at %q, want %q", renewalRow, renewalInvoice)
	}
	if len(body.History) != 0 {
		t.Fatalf("%d payouts recorded before any was made", len(body.History))
	}
}

// Recording a payout is a money action: it writes a row with an id, links the commissions it
// settled, and leaves the report stating earned == paid.
func TestRecordAffiliatePayoutWritesARowAndReconciles(t *testing.T) {
	env := newStaffEnv(t)
	affiliateID, _ := env.seedAffiliate(t, "PAYROW", "10%")
	customerID, _ := env.seedReferredCommission(t, affiliateID, 12345)
	env.seedRenewalCommission(t, affiliateID, customerID, 8000)

	rec := env.postJSON(t, "/api/v1/staff/affiliates/"+affiliateID+"/payouts",
		env.billing, env.billingCSRF, map[string]any{
			"currency":  "USD",
			"method":    "bank_transfer",
			"reference": "SEPA-9001",
			"note":      "March run",
			"from":      time.Now().UTC().Add(-30 * 24 * time.Hour).Format("2006-01-02"),
			"to":        time.Now().UTC().Format("2006-01-02"),
		})
	if rec.Code != http.StatusCreated {
		t.Fatalf("payout: status %d, body %s", rec.Code, rec.Body)
	}
	var recorded struct {
		ID                 string         `json:"id"`
		AffiliateID        string         `json:"affiliate_id"`
		Currency           string         `json:"currency"`
		Amount             map[string]any `json:"amount"`
		Method             string         `json:"method"`
		Reference          string         `json:"reference"`
		Commissions        int            `json:"commissions"`
		OrderCommissions   int            `json:"order_commissions"`
		RenewalCommissions int            `json:"renewal_commissions"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &recorded); err != nil {
		t.Fatalf("decode: %v; body %s", err, rec.Body)
	}
	if recorded.ID == "" {
		t.Fatalf("the payout has no id: %s", rec.Body)
	}
	if recorded.Amount["minor"].(float64) != 2035 {
		t.Fatalf("amount = %v, want 2035", recorded.Amount)
	}
	if recorded.Method != "bank_transfer" || recorded.Reference != "SEPA-9001" {
		t.Fatalf("method/reference = %q/%q", recorded.Method, recorded.Reference)
	}
	if recorded.Commissions != 2 || recorded.OrderCommissions != 1 || recorded.RenewalCommissions != 1 {
		t.Fatalf("settled %d (%d order, %d renewal) rows, want 2 (1 and 1)",
			recorded.Commissions, recorded.OrderCommissions, recorded.RenewalCommissions)
	}

	// The row exists, names the operator, and carries the period the operator was reviewing.
	var createdBy, method, reference string
	var periodFrom, periodTo *time.Time
	if err := env.pool.QueryRow(context.Background(), `
		SELECT coalesce(created_by::text, ''), method, reference, period_from, period_to
		FROM affiliate_payouts WHERE id = $1`, recorded.ID).
		Scan(&createdBy, &method, &reference, &periodFrom, &periodTo); err != nil {
		t.Fatalf("load payout row: %v", err)
	}
	if createdBy == "" {
		t.Fatal("the payout row does not name the operator who ran it")
	}
	if periodFrom == nil || periodTo == nil {
		t.Fatal("the payout row does not record the period the operator was reviewing")
	}

	// The report now reconciles to zero outstanding, and the history names the payout.
	body := env.payoutReportOf(t, affiliateID, "?period=30d")
	line := body.Data[0]
	if line.Outstanding["minor"].(float64) != 0 {
		t.Fatalf("outstanding = %v after a payout, want 0", line.Outstanding)
	}
	if line.Paid["minor"].(float64) != line.Earned["minor"].(float64) {
		t.Fatalf("paid = %v, earned = %v", line.Paid, line.Earned)
	}
	if !line.Reconciles {
		t.Fatal("the report does not reconcile after a payout")
	}
	for _, row := range line.Rows {
		if row.Kind == growth.PayoutRowPayout {
			if row.ID != recorded.ID {
				t.Fatalf("payout row id = %q, want %q", row.ID, recorded.ID)
			}
			continue
		}
		if row.PayoutID != recorded.ID {
			t.Fatalf("commission row %s is not linked to the payout", row.ID)
		}
	}
	if len(body.History) != 1 || body.History[0].ID != recorded.ID {
		t.Fatalf("history = %+v, want the payout %s", body.History, recorded.ID)
	}
	if body.History[0].Method != "bank_transfer" || body.History[0].Reference != "SEPA-9001" {
		t.Fatalf("history row = %+v", body.History[0])
	}
}

// A second payout has nothing left to settle, so a double-click pays once.
func TestRecordAffiliatePayoutRefusesASecondTime(t *testing.T) {
	env := newStaffEnv(t)
	affiliateID, _ := env.seedAffiliate(t, "PAYTWICE", "10%")
	env.seedReferredCommission(t, affiliateID, 5000)

	for i := 0; i < 2; i++ {
		rec := env.postJSON(t, "/api/v1/staff/affiliates/"+affiliateID+"/payouts",
			env.billing, env.billingCSRF, map[string]any{"currency": "USD"})
		if i == 0 && rec.Code != http.StatusCreated {
			t.Fatalf("first payout: status %d, body %s", rec.Code, rec.Body)
		}
		if i == 1 {
			if rec.Code != http.StatusConflict {
				t.Fatalf("second payout: status %d, want 409; body %s", rec.Code, rec.Body)
			}
			if !containsCode(rec.Body.Bytes(), "nothing_to_pay") {
				t.Fatalf("second payout body %s does not carry nothing_to_pay", rec.Body)
			}
		}
	}

	var rows int
	if err := env.pool.QueryRow(context.Background(), `
		SELECT count(*) FROM affiliate_payouts WHERE affiliate_id = $1`, affiliateID).Scan(&rows); err != nil {
		t.Fatalf("count payouts: %v", err)
	}
	if rows != 1 {
		t.Fatalf("%d payout rows after a refused second attempt, want 1", rows)
	}
}

// postAnonymously posts a JSON body with no session and no CSRF token.
func (e *authEnv) postAnonymously(t *testing.T, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, path, bytesReader(encoded))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "anon-"+time.Now().Format("150405.000000000"))
	rec := httptest.NewRecorder()
	e.srv.Handler().ServeHTTP(rec, req)
	return rec
}

// The payout action is guarded by credit:read:all, and this test states the consequence
// rather than hiding it: every staff role holds that permission, support included. That is
// why a dedicated `affiliate:payout` is the right answer and why the report says so; until
// then the guard is what the assignment named, and an anonymous or customer caller is still
// refused.
func TestRecordAffiliatePayoutGuard(t *testing.T) {
	env := newStaffEnv(t)
	affiliateID, _ := env.seedAffiliate(t, "PAYGUARD", "10%")
	env.seedReferredCommission(t, affiliateID, 5000)

	// Anonymous: no session at all. postJSON insists on a cookie, so this one is built by
	// hand — an anonymous request is a case worth stating rather than one worth skipping.
	if rec := env.postAnonymously(t, "/api/v1/staff/affiliates/"+affiliateID+"/payouts",
		map[string]any{"currency": "USD"}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous: status %d, want 401; body %s", rec.Code, rec.Body)
	}

	// A customer, signed in, is refused by the staff-only wrapper even though the route's
	// permission is a read permission every staff role holds.
	env.createUser(t, "payout-customer@example.test", "customer password 1", "")
	cookie, csrf := env.login(t, "payout-customer@example.test", "customer password 1")
	if rec := env.postJSON(t, "/api/v1/staff/affiliates/"+affiliateID+"/payouts",
		cookie, csrf, map[string]any{"currency": "USD"}); rec.Code != http.StatusForbidden {
		t.Fatalf("customer: status %d, want 403; body %s", rec.Code, rec.Body)
	}

	// Nothing was paid by either attempt.
	var rows int
	if err := env.pool.QueryRow(context.Background(), `
		SELECT count(*) FROM affiliate_payouts WHERE affiliate_id = $1`, affiliateID).Scan(&rows); err != nil {
		t.Fatalf("count payouts: %v", err)
	}
	if rows != 0 {
		t.Fatalf("%d payout rows after two refused attempts, want 0", rows)
	}
}
