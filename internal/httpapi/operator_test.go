package httpapi_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/catalyst/billing/internal/httpapi"
	"github.com/catalyst/billing/internal/ledger"
	"github.com/catalyst/billing/internal/money"
	"github.com/jackc/pgx/v5"
)

// The operator surface (P8): system settings, the log viewers, cron history, the dashboard
// and announcements.
//
// These tests assert the properties that a screen can get wrong while looking right:
//
//   - a secret is redacted in the settings response (and the redaction survives a save);
//   - revenue is per currency and never summed;
//   - a staff member without the permission is refused, and one with it is not;
//   - the log lists are keyset-paginated and the cursor is stable across an insert;
//   - every failure is problem+json with a stable code.

// ---------------------------------------------------------------- settings --

// A secret is redacted in the settings response, and saving the redacted form back does not
// destroy the credential.
func TestSettingsSecretIsRedacted(t *testing.T) {
	env := newStaffEnv(t)
	const secret = "smtp-password-value-1"

	rec := env.doJSON(t, http.MethodPut, "/api/v1/staff/settings", env.admin, env.adminCSRF,
		map[string]any{
			"group": "mail",
			"values": map[string]string{
				"smtp_host":     "smtp.example.test",
				"smtp_password": secret,
			},
		})
	if rec.Code != http.StatusOK {
		t.Fatalf("write settings: status %d; body %s", rec.Code, rec.Body)
	}
	if containsString(rec.Body.String(), secret) {
		t.Fatalf("the write response echoed the secret: %s", rec.Body)
	}

	rec = env.get(t, "/api/v1/staff/settings", env.admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("read settings: status %d; body %s", rec.Code, rec.Body)
	}
	if containsString(rec.Body.String(), secret) {
		t.Fatalf("the settings read leaked the secret: %s", rec.Body)
	}

	// The response carries the marker and is_set instead, which is the only thing a client
	// may render for a credential.
	var body struct {
		Data []struct {
			Group string `json:"group"`
			Keys  []struct {
				Key      string `json:"key"`
				Value    string `json:"value"`
				IsSecret bool   `json:"is_secret"`
				IsSet    bool   `json:"is_set"`
			} `json:"keys"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v\n%s", err, rec.Body)
	}
	var found bool
	for _, g := range body.Data {
		if g.Group != "mail" {
			continue
		}
		for _, k := range g.Keys {
			if k.Key != "smtp_password" {
				continue
			}
			found = true
			if k.Value != "[redacted]" {
				t.Fatalf("smtp_password value = %q, want the redacted marker", k.Value)
			}
			if !k.IsSecret {
				t.Fatal("smtp_password is not marked is_secret")
			}
			if !k.IsSet {
				t.Fatal("smtp_password is_set = false after a write")
			}
		}
	}
	if !found {
		t.Fatal("the mail group does not contain smtp_password")
	}

	// Saving the loaded (redacted) form must not overwrite the stored credential.
	rec = env.doJSON(t, http.MethodPut, "/api/v1/staff/settings", env.admin, env.adminCSRF,
		map[string]any{
			"group":  "mail",
			"values": map[string]string{"smtp_password": "[redacted]", "smtp_host": "smtp2.example.test"},
		})
	if rec.Code != http.StatusOK {
		t.Fatalf("re-save: status %d; body %s", rec.Code, rec.Body)
	}
	var stored string
	if err := env.pool.QueryRow(context.Background(),
		`SELECT value FROM settings WHERE group_key = 'mail' AND key = 'smtp_password'`).
		Scan(&stored); err != nil {
		t.Fatalf("read stored secret: %v", err)
	}
	if stored != secret {
		t.Fatalf("stored secret = %q, want the original preserved", stored)
	}

	// The write is audited, naming the key and the actor, and never the secret's value.
	var (
		action string
		actor  string
		value  string
	)
	if err := env.pool.QueryRow(context.Background(), `
		SELECT action, COALESCE(actor_id::text, ''), changes->>'value'
		FROM audit_events
		WHERE action = 'setting.update' AND target_type = 'setting'
		  AND changes->>'key' = 'smtp_password'
		ORDER BY id DESC LIMIT 1`).Scan(&action, &actor, &value); err != nil {
		t.Fatalf("read audit row: %v", err)
	}
	if actor == "" {
		t.Fatal("the settings audit row has no actor")
	}
	if value != "[redacted]" {
		t.Fatalf("the audit row recorded the secret's value: %q", value)
	}
}

// A settings read is refused to a staff member without settings:write, and allowed to one
// with it. Support holds neither.
func TestSettingsRequirePermission(t *testing.T) {
	env := newStaffEnv(t)

	// Support holds log:read but not settings:write.
	if rec := env.get(t, "/api/v1/staff/settings", env.support); rec.Code != http.StatusForbidden {
		t.Fatalf("support settings read: status %d, want 403", rec.Code)
	}
	if rec := env.doJSON(t, http.MethodPut, "/api/v1/staff/settings", env.support,
		env.supportCSRF, map[string]any{
			"group":  "general",
			"values": map[string]string{"company_name": "Hijacked"},
		}); rec.Code != http.StatusForbidden {
		t.Fatalf("support settings write: status %d, want 403", rec.Code)
	}

	// The administrator holds it.
	if rec := env.get(t, "/api/v1/staff/settings", env.admin); rec.Code != http.StatusOK {
		t.Fatalf("admin settings read: status %d; body %s", rec.Code, rec.Body)
	}
}

// An invalid value is refused as problem+json and writes nothing.
func TestSettingsInvalidValueIsProblemJSON(t *testing.T) {
	env := newStaffEnv(t)

	rec := env.doJSON(t, http.MethodPut, "/api/v1/staff/settings", env.admin, env.adminCSRF,
		map[string]any{
			"group": "general",
			"values": map[string]string{
				"company_name":          "New Name",
				"registration_disabled": "perhaps",
			},
		})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body %s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); !containsString(ct, "application/problem+json") {
		t.Fatalf("Content-Type = %q, want problem+json", ct)
	}
	if !containsCode(rec.Body.Bytes(), "invalid_value") {
		t.Fatalf("body = %s", rec.Body)
	}

	// The valid key in the same request must not have been written: validation is before
	// any write.
	var name string
	if err := env.pool.QueryRow(context.Background(),
		`SELECT value FROM settings WHERE group_key = 'general' AND key = 'company_name'`).
		Scan(&name); err != nil {
		t.Fatalf("read company_name: %v", err)
	}
	if name == "New Name" {
		t.Fatal("a rejected bulk write applied the valid keys in it")
	}
}

// A group read returns every declared key exactly once, and an unknown group is a 404.
func TestSettingsGroupRead(t *testing.T) {
	env := newStaffEnv(t)

	rec := env.get(t, "/api/v1/staff/settings?group=invoices", env.admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body %s", rec.Code, rec.Body)
	}
	var body struct {
		Group string `json:"group"`
		Keys  []struct {
			Key   string `json:"key"`
			Value string `json:"value"`
		} `json:"keys"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v\n%s", err, rec.Body)
	}
	if body.Group != "invoices" {
		t.Fatalf("group = %q", body.Group)
	}
	seen := map[string]int{}
	for _, k := range body.Keys {
		seen[k.Key]++
	}
	for _, key := range []string{"due_days", "invoice_number_prefix", "proforma", "bill_to_text"} {
		if seen[key] != 1 {
			t.Fatalf("%s appears %d times, want 1", key, seen[key])
		}
	}

	if rec := env.get(t, "/api/v1/staff/settings?group=nonsense", env.admin); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown group: status %d, want 404", rec.Code)
	}
}

// --------------------------------------------------------------- dashboard --

// Revenue is reported per currency and never summed, and it comes from the ledger rather
// than from invoice totals.
func TestDashboardRevenueIsPerCurrency(t *testing.T) {
	env := newStaffEnv(t)
	ctx := context.Background()

	// Two customers, two currencies, both invoiced in the period. The revenue account is
	// what the dashboard sums, so the postings are what matter.
	usdCustomer, _ := env.seedCustomer(t, "usd@example.test", "Dollar")
	eurCustomer, _ := env.seedCustomer(t, "eur@example.test", "Euro")

	usdInvoice := env.seedInvoice(t, usdCustomer, "open", "USD", 10000, 0, nil)
	eurInvoice := env.seedInvoice(t, eurCustomer, "open", "EUR", 5000, 0, nil)

	usd, err := money.NewCurrency("USD", 2)
	if err != nil {
		t.Fatalf("currency: %v", err)
	}
	eur, err := money.NewCurrency("EUR", 2)
	if err != nil {
		t.Fatalf("currency: %v", err)
	}
	if err := env.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if _, _, err := ledger.Post(ctx, tx, ledger.Posting{
			Kind:        ledger.KindInvoiceIssued,
			Reference:   ledger.RefInvoice,
			ReferenceID: usdInvoice,
			CustomerID:  usdCustomer,
			Currency:    usd,
			Debits:      []ledger.Entry{{Account: ledger.AccountReceivable, Amount: money.New(10000, usd)}},
			Credits:     []ledger.Entry{{Account: ledger.AccountRevenue, Amount: money.New(10000, usd)}},
		}); err != nil {
			return err
		}
		_, _, err := ledger.Post(ctx, tx, ledger.Posting{
			Kind:        ledger.KindInvoiceIssued,
			Reference:   ledger.RefInvoice,
			ReferenceID: eurInvoice,
			CustomerID:  eurCustomer,
			Currency:    eur,
			Debits:      []ledger.Entry{{Account: ledger.AccountReceivable, Amount: money.New(5000, eur)}},
			Credits:     []ledger.Entry{{Account: ledger.AccountRevenue, Amount: money.New(5000, eur)}},
		})
		return err
	}); err != nil {
		t.Fatalf("post revenue: %v", err)
	}

	rec := env.get(t, "/api/v1/staff/dashboard?period=30d", env.billing)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body %s", rec.Code, rec.Body)
	}

	var body struct {
		Period  string `json:"period"`
		Revenue []struct {
			Currency string `json:"currency"`
			Minor    int64  `json:"minor"`
		} `json:"revenue"`
		Invoices []struct {
			Status string `json:"status"`
			Count  int    `json:"count"`
		} `json:"invoices"`
		ActiveCustomers int  `json:"active_customers"`
		Available       bool `json:"available"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v\n%s", err, rec.Body)
	}
	if body.Period != "30d" {
		t.Fatalf("period = %q", body.Period)
	}

	byCurrency := map[string]int64{}
	for _, r := range body.Revenue {
		byCurrency[r.Currency] = r.Minor
	}
	if byCurrency["USD"] != 10000 {
		t.Fatalf("USD revenue = %d, want 10000", byCurrency["USD"])
	}
	if byCurrency["EUR"] != 5000 {
		t.Fatalf("EUR revenue = %d, want 5000", byCurrency["EUR"])
	}
	// The important negative assertion: no combined figure exists, because 10000 USD + 5000
	// EUR is not 15000 of anything.
	if len(body.Revenue) != 2 {
		t.Fatalf("revenue has %d entries, want one per currency: %+v", len(body.Revenue), body.Revenue)
	}
	for _, r := range body.Revenue {
		if r.Minor == 15000 {
			t.Fatalf("a cross-currency total leaked into the response: %+v", body.Revenue)
		}
	}

	// A raw grep of the JSON: there must be no `total` field carrying a summed amount.
	if containsString(rec.Body.String(), `"total_minor"`) {
		t.Fatalf("the dashboard exposes a summed total: %s", rec.Body)
	}

	if !body.Available {
		t.Fatal("available = false on a healthy database")
	}
	if body.ActiveCustomers < 2 {
		t.Fatalf("active_customers = %d, want at least 2", body.ActiveCustomers)
	}
}

// A void invoice is not revenue, and a credit note reduces it: both are ledger facts the
// invoice-total approach would get wrong.
func TestDashboardRevenueReflectsCreditNotes(t *testing.T) {
	env := newStaffEnv(t)
	ctx := context.Background()

	customerID, _ := env.seedCustomer(t, "credit@example.test", "Credited")
	invoiceID := env.seedInvoice(t, customerID, "open", "USD", 10000, 0, nil)

	usd, err := money.NewCurrency("USD", 2)
	if err != nil {
		t.Fatalf("currency: %v", err)
	}
	if err := env.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		// The invoice: revenue credited 10000.
		if _, _, err := ledger.Post(ctx, tx, ledger.Posting{
			Kind:        ledger.KindInvoiceIssued,
			Reference:   ledger.RefInvoice,
			ReferenceID: invoiceID,
			CustomerID:  customerID,
			Currency:    usd,
			Debits:      []ledger.Entry{{Account: ledger.AccountReceivable, Amount: money.New(10000, usd)}},
			Credits:     []ledger.Entry{{Account: ledger.AccountRevenue, Amount: money.New(10000, usd)}},
		}); err != nil {
			return err
		}
		// The credit note: revenue debited 2500, so the period's revenue is 7500 even
		// though the invoice total still reads 10000.
		_, _, err := ledger.Post(ctx, tx, ledger.Posting{
			Kind:        ledger.KindCreditNote,
			Reference:   ledger.RefRefund,
			ReferenceID: "0f8fad5b-d9cb-469f-a165-70867728950e",
			CustomerID:  customerID,
			Currency:    usd,
			Debits:      []ledger.Entry{{Account: ledger.AccountRevenue, Amount: money.New(2500, usd)}},
			Credits:     []ledger.Entry{{Account: ledger.AccountCredit, Amount: money.New(2500, usd)}},
		})
		return err
	}); err != nil {
		t.Fatalf("post: %v", err)
	}

	rec := env.get(t, "/api/v1/staff/dashboard?period=7d", env.billing)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body %s", rec.Code, rec.Body)
	}
	var body struct {
		Revenue []struct {
			Currency string `json:"currency"`
			Minor    int64  `json:"minor"`
		} `json:"revenue"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Revenue) != 1 || body.Revenue[0].Minor != 7500 {
		t.Fatalf("revenue = %+v, want USD 7500 (invoice total minus the credit note)", body.Revenue)
	}
}

// The dashboard is refused to a role without report:read and accepts only the declared
// periods.
func TestDashboardRequiresReportRead(t *testing.T) {
	env := newStaffEnv(t)

	// Support holds log:read and ticket:read:all, not report:read.
	if rec := env.get(t, "/api/v1/staff/dashboard", env.support); rec.Code != http.StatusForbidden {
		t.Fatalf("support dashboard: status %d, want 403", rec.Code)
	}
	if rec := env.get(t, "/api/v1/staff/dashboard", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous dashboard: status %d, want 401", rec.Code)
	}
	if rec := env.get(t, "/api/v1/staff/dashboard?period=10y", env.billing); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad period: status %d, want 400", rec.Code)
	}
	if !containsCode(env.get(t, "/api/v1/staff/dashboard?period=1d", env.billing).Body.Bytes(), "invalid_period") {
		t.Fatal("an invalid period did not answer invalid_period")
	}
	// The billing role holds report:read.
	if rec := env.get(t, "/api/v1/staff/dashboard?period=90d", env.billing); rec.Code != http.StatusOK {
		t.Fatalf("billing dashboard: status %d; body %s", rec.Code, rec.Body)
	}
}

// -------------------------------------------------------------------- logs --

// The email log lists deliveries, and its keyset cursor stays stable across an insert.
func TestEmailLogKeysetPagination(t *testing.T) {
	env := newStaffEnv(t)
	ctx := context.Background()

	// Five deliveries, oldest first, so the newest is last.
	var ids []string
	for i := 0; i < 5; i++ {
		var id string
		if err := env.pool.QueryRow(ctx, `
			INSERT INTO notification_deliveries (kind, to_address, subject, body, status,
				created_at, sent_at)
			VALUES ('invoice_issued', $1, 'Subject', 'Body', 'sent', $2, $2)
			RETURNING id`,
			fmt.Sprintf("customer%d@example.test", i),
			time.Now().UTC().Add(time.Duration(i)*time.Second)).Scan(&id); err != nil {
			t.Fatalf("insert delivery: %v", err)
		}
		ids = append(ids, id)
	}

	rec := env.get(t, "/api/v1/staff/logs/email?limit=2", env.support)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body %s", rec.Code, rec.Body)
	}
	var first struct {
		Data []struct {
			ID string `json:"id"`
			To string `json:"to"`
		} `json:"data"`
		NextCursor *string `json:"next_cursor"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &first); err != nil {
		t.Fatalf("decode: %v\n%s", err, rec.Body)
	}
	if len(first.Data) != 2 {
		t.Fatalf("first page has %d rows, want 2", len(first.Data))
	}
	// Newest first: customer4 then customer3.
	if first.Data[0].ID != ids[4] || first.Data[1].ID != ids[3] {
		t.Fatalf("first page = %+v, want the two newest", first.Data)
	}
	if first.NextCursor == nil {
		t.Fatal("a full page did not return a cursor")
	}

	// Insert a row that sorts before the cursor. A keyset page must not repeat or skip: the
	// new row is newer than the cursor, so it must not appear on the second page.
	var newerID string
	if err := env.pool.QueryRow(ctx, `
		INSERT INTO notification_deliveries (kind, to_address, subject, body, status,
			created_at, sent_at)
		VALUES ('invoice_issued', 'newest@example.test', 'Subject', 'Body', 'sent', now(), now())
		RETURNING id`).Scan(&newerID); err != nil {
		t.Fatalf("insert newer: %v", err)
	}

	rec = env.get(t, "/api/v1/staff/logs/email?limit=2&cursor="+*first.NextCursor, env.support)
	if rec.Code != http.StatusOK {
		t.Fatalf("second page: status %d; body %s", rec.Code, rec.Body)
	}
	var second struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &second); err != nil {
		t.Fatalf("decode: %v\n%s", err, rec.Body)
	}
	if len(second.Data) != 2 {
		t.Fatalf("second page has %d rows, want 2", len(second.Data))
	}
	if second.Data[0].ID != ids[2] || second.Data[1].ID != ids[1] {
		t.Fatalf("second page = %+v, want the rows below the cursor (no skip, no repeat)", second.Data)
	}
	for _, row := range second.Data {
		if row.ID == newerID {
			t.Fatal("a row newer than the cursor appeared on the second page")
		}
	}

	// A forged cursor is a 400 with the stable code, not a silent restart.
	rec = env.get(t, "/api/v1/staff/logs/email?cursor=not-a-cursor", env.support)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad cursor: status %d, want 400", rec.Code)
	}
	if !containsCode(rec.Body.Bytes(), "invalid_cursor") {
		t.Fatalf("body = %s", rec.Body)
	}
}

// The error and HTTP logs are readable, filterable and redacted at write time.
func TestErrorAndHTTPLogs(t *testing.T) {
	env := newStaffEnv(t)
	ctx := context.Background()

	if _, err := env.pool.Exec(ctx, `
		INSERT INTO error_logs (level, source, message, detail) VALUES
			('error', 'worker.renewals', 'renewal failed', 'Authorization: Bearer sk_live_abc123'),
			('warn',  'http.webhook',   'slow request',  'ok')`); err != nil {
		t.Fatalf("insert error logs: %v", err)
	}
	if _, err := env.pool.Exec(ctx, `
		INSERT INTO http_logs (method, path, status, duration_ms, ip) VALUES
			('GET', '/api/v1/staff/invoices', 200, 12, '127.0.0.1'),
			('POST', '/api/v1/staff/invoices', 500, 40, '127.0.0.1')`); err != nil {
		t.Fatalf("insert http logs: %v", err)
	}

	rec := env.get(t, "/api/v1/staff/logs/errors", env.support)
	if rec.Code != http.StatusOK {
		t.Fatalf("errors: status %d; body %s", rec.Code, rec.Body)
	}
	var errorsPage struct {
		Data []struct {
			Level  string `json:"level"`
			Source string `json:"source"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &errorsPage); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(errorsPage.Data) != 2 {
		t.Fatalf("error rows = %d, want 2", len(errorsPage.Data))
	}

	rec = env.get(t, "/api/v1/staff/logs/errors?level=warn", env.support)
	if rec.Code != http.StatusOK {
		t.Fatalf("filtered errors: status %d", rec.Code)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &errorsPage); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(errorsPage.Data) != 1 || errorsPage.Data[0].Level != "warn" {
		t.Fatalf("filtered rows = %+v, want the one warn row", errorsPage.Data)
	}

	rec = env.get(t, "/api/v1/staff/logs/http?min_status=500", env.support)
	if rec.Code != http.StatusOK {
		t.Fatalf("http log: status %d; body %s", rec.Code, rec.Body)
	}
	var httpPage struct {
		Data []struct {
			Status int `json:"status"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &httpPage); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(httpPage.Data) != 1 || httpPage.Data[0].Status != 500 {
		t.Fatalf("http rows = %+v, want the one 500", httpPage.Data)
	}

	// The write-time redaction is what protects a detail that reaches a backup. The stored
	// row was inserted directly here, so assert the redactor through the exported writer.
	if err := writeTestError(env, "Authorization: Bearer sk_live_abc123"); err != nil {
		t.Fatalf("write error: %v", err)
	}
	var detail string
	if err := env.pool.QueryRow(ctx, `
		SELECT detail FROM error_logs WHERE source = 'test.redaction'`).Scan(&detail); err != nil {
		t.Fatalf("read redacted row: %v", err)
	}
	if containsString(detail, "sk_live_abc123") {
		t.Fatalf("a credential survived into the error log: %q", detail)
	}
	if !containsString(detail, "[redacted]") {
		t.Fatalf("the credential was not replaced by the marker: %q", detail)
	}
}

// The log viewers require log:read; a billing clerk does not hold it.
func TestLogsRequireLogRead(t *testing.T) {
	env := newStaffEnv(t)

	for _, path := range []string{
		"/api/v1/staff/logs/email",
		"/api/v1/staff/logs/errors",
		"/api/v1/staff/logs/http",
		"/api/v1/staff/cron",
		"/api/v1/staff/cron/heartbeat",
	} {
		if rec := env.get(t, path, env.billing); rec.Code != http.StatusForbidden {
			t.Fatalf("%s as billing: status %d, want 403", path, rec.Code)
		}
		if rec := env.get(t, path, env.support); rec.Code != http.StatusOK {
			t.Fatalf("%s as support: status %d, want 200; body %s", path, rec.Code, rec.Body)
		}
	}
}

// -------------------------------------------------------------------- cron --

// Cron runs are listed newest first, filterable by job, and the heartbeat reports staleness.
func TestCronRunsAndHeartbeat(t *testing.T) {
	env := newStaffEnv(t)
	ctx := context.Background()

	if _, err := env.pool.Exec(ctx, `
		INSERT INTO cron_runs (job, started_at, finished_at, outcome, detail, items) VALUES
			('renewals', now() - interval '10 minutes', now() - interval '9 minutes', 'ok', 'invoiced 3', 3),
			('renewals', now() - interval '5 minutes', now() - interval '4 minutes', 'failed', 'boom', 0),
			('dunning',  now() - interval '2 minutes', now() - interval '2 minutes', 'ok', 'advanced 1', 1)`); err != nil {
		t.Fatalf("insert runs: %v", err)
	}

	rec := env.get(t, "/api/v1/staff/cron", env.support)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body %s", rec.Code, rec.Body)
	}
	var page struct {
		Data []struct {
			Job        string `json:"job"`
			Outcome    string `json:"outcome"`
			DurationMS *int64 `json:"duration_ms"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode: %v\n%s", err, rec.Body)
	}
	if len(page.Data) != 3 {
		t.Fatalf("runs = %d, want 3", len(page.Data))
	}
	if page.Data[0].Job != "dunning" {
		t.Fatalf("first row = %+v, want the newest", page.Data[0])
	}
	if page.Data[0].DurationMS == nil {
		t.Fatal("a finished run has no duration_ms")
	}

	rec = env.get(t, "/api/v1/staff/cron?job=renewals", env.support)
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(page.Data) != 2 {
		t.Fatalf("filtered runs = %d, want 2", len(page.Data))
	}
	for _, row := range page.Data {
		if row.Job != "renewals" {
			t.Fatalf("filter returned a %q run", row.Job)
		}
	}

	// No heartbeat yet: present false, stale true, and a 200 rather than a 404.
	rec = env.get(t, "/api/v1/staff/cron/heartbeat", env.support)
	if rec.Code != http.StatusOK {
		t.Fatalf("heartbeat: status %d; body %s", rec.Code, rec.Body)
	}
	var absent struct {
		Present bool `json:"present"`
		Stale   bool `json:"stale"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &absent); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if absent.Present || !absent.Stale {
		t.Fatalf("absent heartbeat = %+v, want present=false stale=true", absent)
	}

	// A fresh beat is not stale.
	if _, err := env.pool.Exec(ctx, `
		INSERT INTO worker_heartbeat (id, beat_at, jobs, version) VALUES ('worker', now(), 6, 'test')
		ON CONFLICT (id) DO UPDATE SET beat_at = now()`); err != nil {
		t.Fatalf("insert heartbeat: %v", err)
	}
	rec = env.get(t, "/api/v1/staff/cron/heartbeat", env.support)
	var fresh struct {
		Heartbeat struct {
			Jobs    int    `json:"jobs"`
			Version string `json:"version"`
			Stale   bool   `json:"stale"`
		} `json:"heartbeat"`
		Present bool `json:"present"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &fresh); err != nil {
		t.Fatalf("decode: %v\n%s", err, rec.Body)
	}
	if !fresh.Present || fresh.Heartbeat.Stale {
		t.Fatalf("fresh heartbeat = %+v, want present and not stale", fresh)
	}
	if fresh.Heartbeat.Jobs != 6 || fresh.Heartbeat.Version != "test" {
		t.Fatalf("heartbeat = %+v", fresh.Heartbeat)
	}

	// An old beat is stale: a row existing is not a worker being alive.
	if _, err := env.pool.Exec(ctx,
		`UPDATE worker_heartbeat SET beat_at = now() - interval '1 hour' WHERE id = 'worker'`); err != nil {
		t.Fatalf("age heartbeat: %v", err)
	}
	rec = env.get(t, "/api/v1/staff/cron/heartbeat", env.support)
	if err := json.Unmarshal(rec.Body.Bytes(), &fresh); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !fresh.Heartbeat.Stale {
		t.Fatal("a heartbeat an hour old was reported as fresh")
	}
}

// The exported writers are what the integrator wires into the request middleware and the
// worker. They are exercised here so their signatures and their redaction are pinned.
func TestOperatorWriters(t *testing.T) {
	env := newStaffEnv(t)
	ctx := context.Background()

	// WriteHTTP stores the path without its query string: a reset token in a query string
	// must not be copied into a table that an operator can read.
	if err := httpapi.WriteHTTP(ctx, env.pool, httpapi.HTTPEntry{
		Method:     "get",
		Path:       "/api/v1/portal/invoices?token=super-secret-reset-token",
		Status:     200,
		DurationMS: 7,
		IP:         "127.0.0.1",
	}); err != nil {
		t.Fatalf("WriteHTTP: %v", err)
	}
	var (
		path   string
		method string
	)
	if err := env.pool.QueryRow(ctx,
		`SELECT path, method FROM http_logs ORDER BY id DESC LIMIT 1`).Scan(&path, &method); err != nil {
		t.Fatalf("read http log: %v", err)
	}
	if path != "/api/v1/portal/invoices" {
		t.Fatalf("path = %q, want the query string stripped", path)
	}
	if method != "GET" {
		t.Fatalf("method = %q, want it upper-cased", method)
	}

	// A malformed IP is dropped rather than failing the whole insert.
	if err := httpapi.WriteHTTP(ctx, env.pool, httpapi.HTTPEntry{
		Method: "GET", Path: "/x", Status: 200, IP: "not-an-ip",
	}); err != nil {
		t.Fatalf("WriteHTTP with a bad IP: %v", err)
	}

	// StartRun / FinishRun leave a visible running row and then close it.
	id, err := httpapi.StartRun(ctx, env.pool, "renewals")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	if id == "" {
		t.Fatal("StartRun returned no id")
	}
	var outcome string
	if err := env.pool.QueryRow(ctx, `SELECT outcome FROM cron_runs WHERE id = $1`, id).Scan(&outcome); err != nil {
		t.Fatalf("read run: %v", err)
	}
	if outcome != "running" {
		t.Fatalf("outcome = %q, want running", outcome)
	}
	if err := httpapi.FinishRun(ctx, env.pool, id, "ok", "invoiced 2", 2); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}
	var items int
	if err := env.pool.QueryRow(ctx,
		`SELECT outcome, items FROM cron_runs WHERE id = $1`, id).Scan(&outcome, &items); err != nil {
		t.Fatalf("read finished run: %v", err)
	}
	if outcome != "ok" || items != 2 {
		t.Fatalf("finished run = %q/%d, want ok/2", outcome, items)
	}
	// A second close does not overwrite the first outcome.
	if err := httpapi.FinishRun(ctx, env.pool, id, "failed", "late", 9); err != nil {
		t.Fatalf("second FinishRun: %v", err)
	}
	if err := env.pool.QueryRow(ctx,
		`SELECT outcome, items FROM cron_runs WHERE id = $1`, id).Scan(&outcome, &items); err != nil {
		t.Fatalf("read run: %v", err)
	}
	if outcome != "ok" || items != 2 {
		t.Fatalf("a second close overwrote the run: %q/%d", outcome, items)
	}

	// RecordRun is the one-shot form, and an unknown outcome folds to failed rather than
	// losing the row to the CHECK constraint.
	if _, err := httpapi.RecordRun(ctx, env.pool, "dunning", "timeout", "took too long", 0); err != nil {
		t.Fatalf("RecordRun: %v", err)
	}
	if err := env.pool.QueryRow(ctx,
		`SELECT outcome FROM cron_runs WHERE job = 'dunning' ORDER BY started_at DESC LIMIT 1`).
		Scan(&outcome); err != nil {
		t.Fatalf("read dunning run: %v", err)
	}
	if outcome != "failed" {
		t.Fatalf("unknown outcome folded to %q, want failed", outcome)
	}

	// Beat upserts per INSTANCE: the same worker beating twice is one row, two different workers are
	// two rows. The old behaviour was a single row keyed 'worker', which made the second assertion
	// impossible — two workers overwrote each other, so the panel could report that *a* worker was
	// alive and never that there were two, and `BILLING_EMBEDDED_WORKER=1` alongside a separate
	// `billing worker` was invisible right up until the charge sweep started logging 23505s.
	if err := httpapi.Beat(ctx, env.pool, "worker-alpha", 6, "v1.2.3"); err != nil {
		t.Fatalf("Beat: %v", err)
	}
	if err := httpapi.Beat(ctx, env.pool, "worker-alpha", 7, "v1.2.4"); err != nil {
		t.Fatalf("second Beat: %v", err)
	}
	var count, jobs int
	var version string
	if err := env.pool.QueryRow(ctx,
		`SELECT count(*), max(jobs), max(version) FROM worker_heartbeat`).Scan(&count, &jobs, &version); err != nil {
		t.Fatalf("read heartbeat: %v", err)
	}
	if count != 1 || jobs != 7 || version != "v1.2.4" {
		t.Fatalf("heartbeat rows = %d (%d jobs, %q), want one upserted row for one instance", count, jobs, version)
	}

	// A second worker adds a row rather than replacing the first.
	if err := httpapi.Beat(ctx, env.pool, "worker-beta", 9, "v1.2.4"); err != nil {
		t.Fatalf("Beat for the second worker: %v", err)
	}
	if err := env.pool.QueryRow(ctx,
		`SELECT count(*) FROM worker_heartbeat`).Scan(&count); err != nil {
		t.Fatalf("count heartbeats: %v", err)
	}
	if count != 2 {
		t.Fatalf("heartbeat rows = %d after two instances beat, want 2: a second worker is invisible", count)
	}

	// And the reader reports the count, which is the field that makes two workers actionable.
	rec := env.get(t, "/api/v1/staff/cron/heartbeat", env.admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("heartbeat read: %d %s", rec.Code, rec.Body)
	}
	var body struct {
		Workers   int   `json:"workers"`
		Instances []any `json:"instances"`
		Stale     bool  `json:"stale"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode heartbeat: %v (%s)", err, rec.Body)
	}
	if body.Workers != 2 || len(body.Instances) != 2 {
		t.Fatalf("workers = %d with %d instances, want 2 and 2", body.Workers, len(body.Instances))
	}
	if body.Stale {
		t.Fatal("a freshly beaten fleet is reported stale")
	}
}

// ----------------------------------------------------------- announcements --

// Staff publish an announcement; it appears in the public feed until unpublished, and a
// draft never appears.
func TestAnnouncementsLifecycle(t *testing.T) {
	env := newStaffEnv(t)

	// A draft.
	rec := env.doJSON(t, http.MethodPost, "/api/v1/staff/announcements", env.admin, env.adminCSRF,
		map[string]any{"title": "Draft only", "body": "Not yet", "placement": "storefront"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create draft: status %d; body %s", rec.Code, rec.Body)
	}
	var created struct {
		ID        string `json:"id"`
		Published bool   `json:"published"`
		Live      bool   `json:"live"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v\n%s", err, rec.Body)
	}
	if created.Published || created.Live {
		t.Fatalf("a new announcement was published: %+v", created)
	}

	// The public feed does not show a draft.
	feed := publicFeed(t, env, "")
	if len(feed) != 0 {
		t.Fatalf("the public feed shows a draft: %+v", feed)
	}

	// Publish it.
	rec = env.doJSON(t, http.MethodPut, "/api/v1/staff/announcements/"+created.ID,
		env.admin, env.adminCSRF, map[string]any{
			"title":     "Maintenance tonight",
			"body":      "The panel is read-only from 02:00.",
			"placement": "both",
			"published": true,
		})
	if rec.Code != http.StatusOK {
		t.Fatalf("publish: status %d; body %s", rec.Code, rec.Body)
	}
	var published struct {
		Published   bool    `json:"published"`
		PublishedAt *string `json:"published_at"`
		Live        bool    `json:"live"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &published); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !published.Published || !published.Live || published.PublishedAt == nil {
		t.Fatalf("published announcement = %+v", published)
	}

	feed = publicFeed(t, env, "")
	if len(feed) != 1 || feed[0].Title != "Maintenance tonight" {
		t.Fatalf("public feed = %+v, want the published announcement", feed)
	}

	// A placement filter that does not match returns nothing; `both` matches either.
	if got := publicFeed(t, env, "dashboard"); len(got) != 1 {
		t.Fatalf("dashboard feed = %+v, want one row", got)
	}

	// Unpublish and it disappears.
	rec = env.doJSON(t, http.MethodPut, "/api/v1/staff/announcements/"+created.ID,
		env.admin, env.adminCSRF, map[string]any{
			"title":     "Maintenance tonight",
			"placement": "both",
			"published": false,
		})
	if rec.Code != http.StatusOK {
		t.Fatalf("unpublish: status %d; body %s", rec.Code, rec.Body)
	}
	if got := publicFeed(t, env, ""); len(got) != 0 {
		t.Fatalf("the public feed still shows an unpublished announcement: %+v", got)
	}
}

// An expired announcement is not live, even when it is published.
func TestAnnouncementExpiryHidesIt(t *testing.T) {
	env := newStaffEnv(t)
	past := time.Now().UTC().Add(-time.Hour)

	rec := env.doJSON(t, http.MethodPost, "/api/v1/staff/announcements", env.admin, env.adminCSRF,
		map[string]any{
			"title":      "Already over",
			"published":  true,
			"placement":  "both",
			"expires_at": past.Format(time.RFC3339),
		})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: status %d; body %s", rec.Code, rec.Body)
	}
	if got := publicFeed(t, env, ""); len(got) != 0 {
		t.Fatalf("an expired announcement is in the public feed: %+v", got)
	}
}

// Announcement writes require announcement:write; support does not hold it.
func TestAnnouncementsRequirePermission(t *testing.T) {
	env := newStaffEnv(t)

	if rec := env.get(t, "/api/v1/staff/announcements", env.support); rec.Code != http.StatusForbidden {
		t.Fatalf("support list: status %d, want 403", rec.Code)
	}
	if rec := env.doJSON(t, http.MethodPost, "/api/v1/staff/announcements", env.support,
		env.supportCSRF, map[string]any{"title": "Nope"}); rec.Code != http.StatusForbidden {
		t.Fatalf("support create: status %d, want 403", rec.Code)
	}
	// The public feed needs no session at all.
	if rec := env.get(t, "/api/v1/announcements", nil); rec.Code != http.StatusOK {
		t.Fatalf("public feed: status %d, want 200", rec.Code)
	}
}

// A public announcement feed failure is an empty feed, not a 500: the storefront must render.
func TestPublicAnnouncementsWithoutDatabase(t *testing.T) {
	env := newAuthEnv(t)
	rec := env.get(t, "/api/v1/announcements", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !containsString(ct, "application/json") {
		t.Fatalf("Content-Type = %q", ct)
	}
}

// ------------------------------------------------------------------ helpers --

// publicAnnouncement is the subset of the public feed the tests read.
type publicAnnouncement struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

// publicFeed fetches the public announcement feed.
func publicFeed(t *testing.T, env *staffEnv, placement string) []publicAnnouncement {
	t.Helper()
	path := "/api/v1/announcements"
	if placement != "" {
		path += "?placement=" + placement
	}
	rec := env.get(t, path, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("public feed: status %d; body %s", rec.Code, rec.Body)
	}
	var body struct {
		Data []publicAnnouncement `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode feed: %v\n%s", err, rec.Body)
	}
	return body.Data
}

// writeTestError exercises the exported WriteError writer, which is what the integrator
// wires into the request middleware and the worker.
func writeTestError(env *staffEnv, detail string) error {
	return httpapi.WriteError(context.Background(), env.pool, httpapi.ErrorEntry{
		Level:   "error",
		Source:  "test.redaction",
		Message: "redaction test",
		Detail:  detail,
	})
}

// containsString reports whether s contains needle.
func containsString(s, needle string) bool {
	return len(needle) == 0 || (len(s) >= len(needle) && indexOf(s, needle) >= 0)
}

// indexOf is strings.Index without the import in this file's helpers.
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
