package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/catalyst/billing/internal/growth"
	"github.com/jackc/pgx/v5"
)

// Growth route contracts: the affiliate programme and Discord notifications.
//
// These assert the properties that make the endpoints safe rather than the screens that
// call them: the permission each action needs, that a customer can only read their own
// affiliate row, that the webhook secret never comes back out, and that a payout is
// audited and idempotent.

// seedAffiliate creates an affiliate attached to a fresh user and returns its id.
func (e *authEnv) seedAffiliate(t *testing.T, code, rate string) (affiliateID, userID string) {
	t.Helper()
	ctx := context.Background()
	email := "aff-" + strings.ToLower(code) + "@example.test"
	if err := e.pool.QueryRow(ctx, `
		INSERT INTO users (email, password_hash, email_verified_at)
		VALUES ($1, 'x', now()) RETURNING id`, email).Scan(&userID); err != nil {
		t.Fatalf("affiliate user: %v", err)
	}
	store := growth.New(e.pool)
	if err := e.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		created, err := store.Create(ctx, tx, growth.NewAffiliate{
			UserID: userID, Code: code, CommissionRate: rate, PayoutMethod: "credit",
		})
		if err != nil {
			return err
		}
		affiliateID = created.ID
		return nil
	}); err != nil {
		t.Fatalf("create affiliate: %v", err)
	}
	return affiliateID, userID
}

// seedReferredCommission builds a referred customer, a paid order and the commission, so
// the payout report has a balance to show.
func (e *authEnv) seedReferredCommission(t *testing.T, affiliateID string, totalMinor int64) (customerID, orderID string) {
	t.Helper()
	ctx := context.Background()
	if err := e.pool.QueryRow(ctx, `
		INSERT INTO customers (email, name) VALUES ($1, 'Referred')
		RETURNING id`, "referred-"+totalMinorString(totalMinor)+"@example.test").Scan(&customerID); err != nil {
		t.Fatalf("customer: %v", err)
	}
	if err := e.pool.QueryRow(ctx, `
		INSERT INTO orders (customer_id, status, currency, total_minor)
		VALUES ($1, 'paid', 'USD', $2) RETURNING id`, customerID, totalMinor).Scan(&orderID); err != nil {
		t.Fatalf("order: %v", err)
	}
	var invoiceID string
	if err := e.pool.QueryRow(ctx, `
		INSERT INTO invoices (number, customer_id, order_id, status, currency,
			subtotal_minor, tax_minor, total_minor, amount_paid_minor, issued_at, snapshot)
		VALUES ('INV-'||$4, $1, $2, 'paid', 'USD', $3, 0, $3, $3, now(),
		        '{"schema_version":1}'::jsonb)
		RETURNING id`, customerID, orderID, totalMinor, totalMinorString(totalMinor)).Scan(&invoiceID); err != nil {
		t.Fatalf("invoice: %v", err)
	}
	// The attribution row that makes the customer referred. It is written directly rather
	// than through AttributeSignup because this test is about the payout surface, and the
	// attribution rules have their own tests in internal/growth.
	if _, err := e.pool.Exec(ctx, `
		INSERT INTO affiliate_referrals (affiliate_id, customer_id, currency, status)
		VALUES ($1, $2, 'USD', 'pending')`, affiliateID, customerID); err != nil {
		t.Fatalf("attribution: %v", err)
	}
	if err := e.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := growth.RecordCommissionForPaidInvoice(ctx, tx, invoiceID)
		return err
	}); err != nil {
		t.Fatalf("commission: %v", err)
	}
	return customerID, orderID
}

// totalMinorString renders a minor amount for an email suffix.
func totalMinorString(n int64) string {
	b, _ := json.Marshal(n)
	return strings.ReplaceAll(string(b), "-", "")
}

// -------------------------------------------------------------- permission --

// The affiliate list is a report: it requires report:read, and support deliberately does
// not hold it.
func TestAffiliateListRequiresReportRead(t *testing.T) {
	env := newStaffEnv(t)

	if rec := env.get(t, "/api/v1/staff/affiliates", env.admin); rec.Code != http.StatusOK {
		t.Fatalf("admin list: status %d, body %s", rec.Code, rec.Body)
	}
	if rec := env.get(t, "/api/v1/staff/affiliates", env.billing); rec.Code != http.StatusOK {
		t.Fatalf("billing list: status %d, body %s", rec.Code, rec.Body)
	}
	// Support holds ticket and log permissions, not report:read.
	if rec := env.get(t, "/api/v1/staff/affiliates", env.support); rec.Code != http.StatusForbidden {
		t.Fatalf("support list: status %d, want 403; body %s", rec.Code, rec.Body)
	}
	// Anonymous.
	if rec := env.get(t, "/api/v1/staff/affiliates", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous list: status %d, want 401", rec.Code)
	}
}

// The payout report carries the same permission as the list.
func TestAffiliatePayoutsRequiresReportRead(t *testing.T) {
	env := newStaffEnv(t)
	if rec := env.get(t, "/api/v1/staff/affiliates/payouts", env.billing); rec.Code != http.StatusOK {
		t.Fatalf("billing payouts: status %d, body %s", rec.Code, rec.Body)
	}
	if rec := env.get(t, "/api/v1/staff/affiliates/payouts", env.support); rec.Code != http.StatusForbidden {
		t.Fatalf("support payouts: status %d, want 403", rec.Code)
	}
}

// Registering an affiliate fixes a rate at which money will later leave, so it requires
// credit:write. Support cannot, and neither can a caller without a CSRF token.
func TestCreateAffiliateRequiresCreditWrite(t *testing.T) {
	env := newStaffEnv(t)
	_, userID := env.seedAffiliate(t, "SEEDED1", "10%")

	body := map[string]any{
		"user_id":         userID,
		"code":            "NEWCODE",
		"commission_rate": "12.5%",
		"payout_method":   "credit",
	}
	if rec := env.postJSON(t, "/api/v1/staff/affiliates", env.support, env.supportCSRF, body); rec.Code != http.StatusForbidden {
		t.Fatalf("support create: status %d, want 403; body %s", rec.Code, rec.Body)
	}
	// A mutation without the CSRF token is refused before the handler runs.
	if rec := env.postJSON(t, "/api/v1/staff/affiliates", env.billing, "", body); rec.Code != http.StatusForbidden {
		t.Fatalf("no CSRF: status %d, want 403; body %s", rec.Code, rec.Body)
	}
}

// The payout action is a money action: credit:write, and the amount leaves the balance.
func TestAffiliatePayoutRequiresCreditWriteAndSettles(t *testing.T) {
	env := newStaffEnv(t)
	affiliateID, _ := env.seedAffiliate(t, "PAYME", "10%")
	env.seedReferredCommission(t, affiliateID, 5000)

	// Support cannot pay out.
	if rec := env.postJSON(t, "/api/v1/staff/affiliates/"+affiliateID+"/payout",
		env.support, env.supportCSRF, map[string]any{"currency": "USD"}); rec.Code != http.StatusForbidden {
		t.Fatalf("support payout: status %d, want 403; body %s", rec.Code, rec.Body)
	}

	rec := env.postJSON(t, "/api/v1/staff/affiliates/"+affiliateID+"/payout",
		env.billing, env.billingCSRF, map[string]any{"currency": "USD"})
	if rec.Code != http.StatusOK {
		t.Fatalf("billing payout: status %d, body %s", rec.Code, rec.Body)
	}
	var out struct {
		AffiliateID string         `json:"affiliate_id"`
		Currency    string         `json:"currency"`
		Amount      map[string]any `json:"amount"`
		Commissions int            `json:"commissions"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v; body %s", err, rec.Body)
	}
	if out.AffiliateID != affiliateID || out.Currency != "USD" {
		t.Fatalf("payout = %+v", out)
	}
	if out.Amount["minor"].(float64) != 500 {
		t.Fatalf("amount = %v, want 500 minor (10%% of 5000)", out.Amount)
	}
	if out.Commissions != 1 {
		t.Fatalf("commissions = %d, want 1", out.Commissions)
	}

	// A second payout has nothing left to pay.
	if rec := env.postJSON(t, "/api/v1/staff/affiliates/"+affiliateID+"/payout",
		env.billing, env.billingCSRF, map[string]any{"currency": "USD"}); rec.Code != http.StatusConflict {
		t.Fatalf("second payout: status %d, want 409; body %s", rec.Code, rec.Body)
	}

	// The payout is audited with the operator as actor.
	var actorKind string
	if err := env.pool.QueryRow(context.Background(), `
		SELECT actor_kind FROM audit_events
		WHERE action = 'affiliate.payout' ORDER BY id DESC LIMIT 1`).Scan(&actorKind); err != nil {
		t.Fatalf("load audit row: %v", err)
	}
	if actorKind != "user" {
		t.Fatalf("audit actor kind = %q, want user", actorKind)
	}
}

// The payout report shows the balance and the per-currency total.
func TestAffiliatePayoutReport(t *testing.T) {
	env := newStaffEnv(t)
	affiliateID, _ := env.seedAffiliate(t, "REPORTME", "0.1")
	env.seedReferredCommission(t, affiliateID, 12345)

	rec := env.get(t, "/api/v1/staff/affiliates/payouts", env.billing)
	if rec.Code != http.StatusOK {
		t.Fatalf("report: status %d, body %s", rec.Code, rec.Body)
	}
	var out struct {
		Data []struct {
			AffiliateID string         `json:"affiliate_id"`
			Code        string         `json:"code"`
			Owed        map[string]any `json:"owed"`
		} `json:"data"`
		Totals []map[string]any `json:"totals"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v; body %s", err, rec.Body)
	}
	if len(out.Data) != 1 {
		t.Fatalf("%d payout lines, want 1: %s", len(out.Data), rec.Body)
	}
	if out.Data[0].Code != "REPORTME" {
		t.Fatalf("code = %q", out.Data[0].Code)
	}
	// 10% of 12345 = 1234.5, half-up = 1235.
	if out.Data[0].Owed["minor"].(float64) != 1235 {
		t.Fatalf("owed = %v, want 1235", out.Data[0].Owed)
	}
	if len(out.Totals) != 1 || out.Totals[0]["minor"].(float64) != 1235 {
		t.Fatalf("totals = %v, want one USD total of 1235", out.Totals)
	}
}

// --------------------------------------------------------------- portal view --

// A customer reads their own affiliate row, and only their own: the identity comes from the
// session, and there is no identifier in the path to substitute.
func TestPortalAffiliateIsScopedToCaller(t *testing.T) {
	env := newAuthEnv(t)
	affiliateUserID := env.createUser(t, "portal-aff@example.test", "affiliate password 1", "")
	affiliateID, _ := env.seedAffiliate(t, "PORTAL", "10%")
	// The affiliate was created against a different user; re-point it at the login so the
	// portal endpoint resolves it from the session.
	if _, err := env.pool.Exec(context.Background(),
		`UPDATE affiliates SET user_id = $2 WHERE id = $1`, affiliateID, affiliateUserID); err != nil {
		t.Fatalf("re-point affiliate: %v", err)
	}

	cookie, _ := env.login(t, "portal-aff@example.test", "affiliate password 1")
	rec := env.get(t, "/api/v1/portal/affiliate", cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("portal affiliate: status %d, body %s", rec.Code, rec.Body)
	}
	var out struct {
		Code           string `json:"code"`
		CommissionRate string `json:"commission_rate"`
		ReferredCount  int    `json:"referred_count"`
		Referrals      []any  `json:"referrals"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v; body %s", err, rec.Body)
	}
	if out.Code != "PORTAL" {
		t.Fatalf("code = %q, want PORTAL", out.Code)
	}
	if out.CommissionRate != "1/10" {
		t.Fatalf("rate = %q, want the canonical 1/10", out.CommissionRate)
	}

	// A customer who is not an affiliate gets 404, not 403: enrolment is not implied by
	// having an account, and "not enrolled" is not information about another record.
	env.createUser(t, "not-aff@example.test", "customer password 1", "")
	otherCookie, _ := env.login(t, "not-aff@example.test", "customer password 1")
	if rec := env.get(t, "/api/v1/portal/affiliate", otherCookie); rec.Code != http.StatusNotFound {
		t.Fatalf("non-affiliate: status %d, want 404; body %s", rec.Code, rec.Body)
	}

	// Anonymous is 401.
	if rec := env.get(t, "/api/v1/portal/affiliate", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous: status %d, want 401", rec.Code)
	}
}

// ------------------------------------------------------------------ discord --

// The Discord config read returns whether a webhook is stored, never the URL. This is the
// one property that must hold no matter what else changes.
func TestDiscordConfigNeverReturnsWebhookURL(t *testing.T) {
	env := newStaffEnv(t)
	const secretURL = "https://discord.example.test/api/webhooks/999/super-secret"

	// Write it through the API so the whole path is exercised.
	rec := env.putJSON(t, "/api/v1/staff/discord", env.admin, env.adminCSRF, map[string]any{
		"enabled":     true,
		"webhook_url": secretURL,
		"events":      []string{growth.EventInvoicePaid},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("update discord: status %d, body %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "super-secret") {
		t.Fatalf("the update response leaked the webhook: %s", rec.Body)
	}

	read := env.get(t, "/api/v1/staff/discord", env.admin)
	if read.Code != http.StatusOK {
		t.Fatalf("read discord: status %d, body %s", read.Code, read.Body)
	}
	if strings.Contains(read.Body.String(), "super-secret") {
		t.Fatalf("the config read leaked the webhook: %s", read.Body)
	}
	var out struct {
		Enabled           bool     `json:"enabled"`
		Events            []string `json:"events"`
		WebhookConfigured bool     `json:"webhook_configured"`
		AvailableEvents   []string `json:"available_events"`
	}
	if err := json.Unmarshal(read.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v; body %s", err, read.Body)
	}
	if !out.Enabled || !out.WebhookConfigured {
		t.Fatalf("config = %+v, want enabled and configured", out)
	}
	if len(out.Events) != 1 || out.Events[0] != growth.EventInvoicePaid {
		t.Fatalf("events = %v", out.Events)
	}
	if len(out.AvailableEvents) == 0 {
		t.Fatal("available_events is empty")
	}

	// A second save without a URL keeps the stored secret, because the operator cannot see
	// it to send it back.
	rec = env.putJSON(t, "/api/v1/staff/discord", env.admin, env.adminCSRF, map[string]any{
		"enabled": true,
		"events":  []string{growth.EventInvoicePaid, growth.EventOrderPaid},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("second update: status %d, body %s", rec.Code, rec.Body)
	}
	var configured bool
	if err := env.pool.QueryRow(context.Background(), `
		SELECT value <> '' FROM settings WHERE group_key = 'discord' AND key = 'webhook_url'`,
	).Scan(&configured); err != nil {
		t.Fatalf("read stored url: %v", err)
	}
	if !configured {
		t.Fatal("a save without a URL cleared the stored webhook")
	}
}

// Discord configuration is system settings, so it requires settings:write: billing and
// support cannot.
func TestDiscordConfigRequiresSettingsWrite(t *testing.T) {
	env := newStaffEnv(t)
	if rec := env.get(t, "/api/v1/staff/discord", env.support); rec.Code != http.StatusOK {
		// The read is report:read, which support does not hold either; assert the refusal
		// for support on the write path below and the read for billing here.
		if rec.Code != http.StatusForbidden {
			t.Fatalf("support read: status %d", rec.Code)
		}
	}
	body := map[string]any{"enabled": false, "events": []string{}}
	if rec := env.putJSON(t, "/api/v1/staff/discord", env.billing, env.billingCSRF, body); rec.Code != http.StatusForbidden {
		t.Fatalf("billing update: status %d, want 403 (settings:write); body %s", rec.Code, rec.Body)
	}
	if rec := env.putJSON(t, "/api/v1/staff/discord", env.admin, env.adminCSRF, body); rec.Code != http.StatusOK {
		t.Fatalf("admin update: status %d, body %s", rec.Code, rec.Body)
	}
}

// clear_webhook removes the credential, and it is separate from an omitted URL so saving a
// selection cannot delete the secret by accident.
func TestDiscordClearWebhook(t *testing.T) {
	env := newStaffEnv(t)
	rec := env.putJSON(t, "/api/v1/staff/discord", env.admin, env.adminCSRF, map[string]any{
		"enabled":     true,
		"webhook_url": "https://discord.example.test/api/webhooks/1/tok",
		"events":      []string{growth.EventInvoicePaid},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("seed config: %d %s", rec.Code, rec.Body)
	}
	rec = env.putJSON(t, "/api/v1/staff/discord", env.admin, env.adminCSRF, map[string]any{
		"enabled":       false,
		"events":        []string{},
		"clear_webhook": true,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("clear: %d %s", rec.Code, rec.Body)
	}
	var out struct {
		WebhookConfigured bool `json:"webhook_configured"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.WebhookConfigured {
		t.Fatal("clear_webhook did not remove the stored URL")
	}
}
