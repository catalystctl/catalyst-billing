package httpapi_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/catalyst/billing/internal/store/storetest"
)

// The importer's HTTP contract (OPS-10, OPS-11).
//
// These assert what the screen relies on: the route is guarded by import:write, a request that
// does not ask to commit writes nothing, a replayed request replays the report instead of
// importing twice, and every refusal is problem+json with a stable code.

func init() {
	storetest.RegisterTables("import_runs", "import_records")
}

// importDump is the five documents, with the columns a WHMCS export carries.
func importDump() map[string]string {
	return map[string]string{
		"clients": "userid,firstname,lastname,email,datecreated,currency\n" +
			"1,Ada,Lovelace,ada@example.test,2021-03-04,USD\n" +
			"2,Bad,Email,not-an-address,2021-05-06,USD\n",
		"invoices": "invoiceid,userid,invoicenum,date,duedate,subtotal,tax,total,status,currency\n" +
			"10,1,INV-000101,2021-03-04,2021-03-18,100.00,20.00,120.00,Paid,USD\n",
		"invoice_items": "id,invoiceid,description,amount\n" +
			"100,10,VPS Small - monthly,100.00\n",
		"services":     "id,userid,package,domain,domainstatus\n",
		"transactions": "id,userid,invoiceid,gateway,transid,date,amount,currency\n",
	}
}

// importBody is a request body. commit is explicit so a test reads as what it asks for.
func importBody(commit bool) map[string]any {
	return map[string]any{
		"source":           "whmcs",
		"commit":           commit,
		"default_currency": "USD",
		"files":            importDump(),
	}
}

// postWithKey performs a POST with a chosen idempotency key, so a test can replay one.
func (e *authEnv) postWithKey(t *testing.T, path string, cookie *http.Cookie, csrf, key string,
	body any) *httptest.ResponseRecorder {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(encoded))
	req.Header.Set("Content-Type", "application/json")
	if cookie != nil {
		req.AddCookie(cookie)
	}
	if csrf != "" {
		req.Header.Set("X-CSRF-Token", csrf)
	}
	req.Header.Set("Idempotency-Key", key)
	rec := httptest.NewRecorder()
	e.srv.Handler().ServeHTTP(rec, req)
	return rec
}

// importReport is the subset of the report these tests assert on.
type importReport struct {
	Source  string `json:"source"`
	DryRun  bool   `json:"dry_run"`
	RunID   string `json:"run_id"`
	Totals  struct {
		RowsRead  int `json:"rows_read"`
		Created   int `json:"created"`
		Updated   int `json:"updated"`
		Unchanged int `json:"unchanged"`
		Rejected  int `json:"rejected"`
	} `json:"totals"`
	Rejections []struct {
		File     string `json:"file"`
		Row      int    `json:"row"`
		Line     int    `json:"line"`
		Entity   string `json:"entity"`
		LegacyID string `json:"legacy_id"`
		Code     string `json:"code"`
		Detail   string `json:"detail"`
	} `json:"rejections"`
}

func decodeImportReport(t *testing.T, rec *httptest.ResponseRecorder) importReport {
	t.Helper()
	var out importReport
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode report: %v\n%s", err, rec.Body)
	}
	return out
}

// An import is staff work that writes customers and invoices in bulk, so it needs import:write.
// Support deliberately does not hold it.
func TestImportRoutesRequireImportWrite(t *testing.T) {
	env := newStaffEnv(t)

	if rec := env.doJSON(t, http.MethodPost, "/api/v1/staff/imports", env.admin, env.adminCSRF,
		importBody(false)); rec.Code != http.StatusOK {
		t.Fatalf("admin dry run: status %d, body %s", rec.Code, rec.Body)
	}
	if rec := env.doJSON(t, http.MethodPost, "/api/v1/staff/imports", env.billing, env.billingCSRF,
		importBody(false)); rec.Code != http.StatusOK {
		t.Fatalf("billing dry run: status %d, body %s", rec.Code, rec.Body)
	}
	// Support holds ticket and log permissions, not import:write.
	rec := env.doJSON(t, http.MethodPost, "/api/v1/staff/imports", env.support, env.supportCSRF,
		importBody(false))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("support import: status %d, want 403; body %s", rec.Code, rec.Body)
	}
	if code := decodeProblemCode(t, rec); code != "forbidden" && code != "permission_denied" {
		t.Fatalf("support refusal code = %q", code)
	}
	if rec := env.get(t, "/api/v1/staff/imports", env.support); rec.Code != http.StatusForbidden {
		t.Fatalf("support list: status %d, want 403", rec.Code)
	}
	if rec := env.get(t, "/api/v1/staff/imports", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous list: status %d, want 401", rec.Code)
	}
}

// A request that does not ask to commit writes nothing, and says so.
func TestImportIsADryRunUnlessAsked(t *testing.T) {
	env := newStaffEnv(t)
	ctx := t.Context()

	rec := env.doJSON(t, http.MethodPost, "/api/v1/staff/imports", env.admin, env.adminCSRF,
		importBody(false))
	if rec.Code != http.StatusOK {
		t.Fatalf("dry run: status %d, body %s", rec.Code, rec.Body)
	}
	report := decodeImportReport(t, rec)
	if !report.DryRun {
		t.Fatal("the response does not say it was a dry run")
	}
	if report.RunID != "" {
		t.Fatalf("a dry run returned a run id (%q)", report.RunID)
	}
	if report.Totals.Created != 3 {
		t.Fatalf("created = %d, want 3; report %+v", report.Totals, report)
	}
	if report.Totals.Rejected != 1 {
		t.Fatalf("rejected = %d, want 1", report.Totals.Rejected)
	}
	if len(report.Rejections) != 1 || report.Rejections[0].Code != "invalid_email" {
		t.Fatalf("rejections = %+v, want one invalid_email", report.Rejections)
	}
	if report.Rejections[0].Row != 2 || report.Rejections[0].Line != 3 {
		t.Fatalf("rejection at row %d line %d, want 2/3",
			report.Rejections[0].Row, report.Rejections[0].Line)
	}

	var customers, runs int
	if err := env.pool.QueryRow(ctx, `SELECT count(*) FROM customers`).Scan(&customers); err != nil {
		t.Fatalf("count customers: %v", err)
	}
	if err := env.pool.QueryRow(ctx, `SELECT count(*) FROM import_runs`).Scan(&runs); err != nil {
		t.Fatalf("count runs: %v", err)
	}
	if customers != 0 || runs != 0 {
		t.Fatalf("a dry run wrote %d customers and %d runs; it must write nothing", customers, runs)
	}

	// Now ask for it. The same body with commit set is a real import.
	rec = env.doJSON(t, http.MethodPost, "/api/v1/staff/imports", env.admin, env.adminCSRF,
		importBody(true))
	if rec.Code != http.StatusOK {
		t.Fatalf("commit: status %d, body %s", rec.Code, rec.Body)
	}
	committed := decodeImportReport(t, rec)
	if committed.DryRun {
		t.Fatal("the commit response still says dry run")
	}
	if committed.RunID == "" {
		t.Fatal("a commit returned no run id")
	}
	if err := env.pool.QueryRow(ctx, `SELECT count(*) FROM customers`).Scan(&customers); err != nil {
		t.Fatalf("count customers: %v", err)
	}
	if customers != 1 {
		t.Fatalf("customers = %d, want 1", customers)
	}

	// The history has the committed run and its report.
	rec = env.get(t, "/api/v1/staff/imports", env.admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("list runs: status %d, body %s", rec.Code, rec.Body)
	}
	var list struct {
		Data []struct {
			ID       string       `json:"id"`
			Source   string       `json:"source"`
			Created  int          `json:"created"`
			Rejected int          `json:"rejected"`
			Report   importReport `json:"report"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode list: %v\n%s", err, rec.Body)
	}
	if len(list.Data) != 1 {
		t.Fatalf("runs = %d, want 1 (the dry run is not recorded)", len(list.Data))
	}
	if list.Data[0].ID != committed.RunID || list.Data[0].Source != "whmcs" {
		t.Fatalf("listed run = %+v, want %q from whmcs", list.Data[0], committed.RunID)
	}
	if len(list.Data[0].Report.Rejections) != 1 {
		t.Fatalf("the stored report lost its rejection: %+v", list.Data[0].Report)
	}

	rec = env.get(t, "/api/v1/staff/imports/"+committed.RunID, env.admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("get run: status %d, body %s", rec.Code, rec.Body)
	}
	var one struct {
		ID       string       `json:"id"`
		Report   importReport `json:"report"`
		Rejected int          `json:"rejected"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &one); err != nil {
		t.Fatalf("decode run: %v\n%s", err, rec.Body)
	}
	if one.Report.Totals.Created != committed.Totals.Created {
		t.Fatalf("stored report created = %d, response said %d",
			one.Report.Totals.Created, committed.Totals.Created)
	}
}

// A retried request replays the stored report: it does not import twice.
func TestImportReplaysTheSameKey(t *testing.T) {
	env := newStaffEnv(t)
	ctx := t.Context()
	key := "import-replay-0001"

	first := env.postWithKey(t, "/api/v1/staff/imports", env.admin, env.adminCSRF, key,
		importBody(true))
	if first.Code != http.StatusOK {
		t.Fatalf("first: status %d, body %s", first.Code, first.Body)
	}
	second := env.postWithKey(t, "/api/v1/staff/imports", env.admin, env.adminCSRF, key,
		importBody(true))
	if second.Code != http.StatusOK {
		t.Fatalf("replay: status %d, body %s", second.Code, second.Body)
	}
	firstReport := decodeImportReport(t, first)
	secondReport := decodeImportReport(t, second)
	if firstReport.RunID == "" || firstReport.RunID != secondReport.RunID {
		t.Fatalf("the replay returned a different run (%q vs %q); it must replay the stored report",
			firstReport.RunID, secondReport.RunID)
	}
	if firstReport.Totals != secondReport.Totals {
		t.Fatalf("replayed totals %+v differ from the original %+v",
			secondReport.Totals, firstReport.Totals)
	}
	if len(firstReport.Rejections) != len(secondReport.Rejections) {
		t.Fatalf("the replay lost the rejections: %d vs %d",
			len(secondReport.Rejections), len(firstReport.Rejections))
	}

	var runs, customers int
	if err := env.pool.QueryRow(ctx, `SELECT count(*) FROM import_runs`).Scan(&runs); err != nil {
		t.Fatalf("count runs: %v", err)
	}
	if err := env.pool.QueryRow(ctx, `SELECT count(*) FROM customers`).Scan(&customers); err != nil {
		t.Fatalf("count customers: %v", err)
	}
	if runs != 1 || customers != 1 {
		t.Fatalf("a replay imported again: %d runs, %d customers", runs, customers)
	}

	// A different body under the same key is a different question, and is refused rather than
	// answered with a stale report.
	changed := importBody(true)
	changed["default_currency"] = "EUR"
	third := env.postWithKey(t, "/api/v1/staff/imports", env.admin, env.adminCSRF, key, changed)
	if third.Code != http.StatusUnprocessableEntity {
		t.Fatalf("different body, same key: status %d, want 422; body %s", third.Code, third.Body)
	}
}

// A dump that omits a document is refused before any row is read.
func TestImportRequiresEveryDocument(t *testing.T) {
	env := newStaffEnv(t)

	body := importBody(false)
	files := body["files"].(map[string]string)
	delete(files, "transactions")

	rec := env.doJSON(t, http.MethodPost, "/api/v1/staff/imports", env.admin, env.adminCSRF, body)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status %d, want 422; body %s", rec.Code, rec.Body)
	}
	if code := decodeProblemCode(t, rec); code != "missing_file" {
		t.Fatalf("code = %q, want missing_file", code)
	}

	body = importBody(false)
	body["source"] = "sage50"
	rec = env.doJSON(t, http.MethodPost, "/api/v1/staff/imports", env.admin, env.adminCSRF, body)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("unknown source: status %d, want 422", rec.Code)
	}
	if code := decodeProblemCode(t, rec); code != "unknown_source" {
		t.Fatalf("code = %q, want unknown_source", code)
	}
}

// A malformed id is a client mistake, not a missing record.
func TestImportRunLookupErrors(t *testing.T) {
	env := newStaffEnv(t)

	rec := env.get(t, "/api/v1/staff/imports/not-a-uuid", env.admin)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed id: status %d, want 400", rec.Code)
	}
	if code := decodeProblemCode(t, rec); code != "invalid_id" {
		t.Fatalf("code = %q, want invalid_id", code)
	}

	rec = env.get(t, "/api/v1/staff/imports/00000000-0000-4000-8000-000000000000", env.admin)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown run: status %d, want 404", rec.Code)
	}
	if code := decodeProblemCode(t, rec); code != "not_found" {
		t.Fatalf("code = %q, want not_found", code)
	}
}

// An unparseable body is refused with the shared envelope, and never as a dry run that
// silently did nothing.
func TestImportRejectsAMalformedBody(t *testing.T) {
	env := newStaffEnv(t)

	rec := env.postWithKey(t, "/api/v1/staff/imports", env.admin, env.adminCSRF,
		"import-bad-body-1", map[string]any{"source": "whmcs", "committed": true})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400; body %s", rec.Code, rec.Body)
	}
	if code := decodeProblemCode(t, rec); code != "invalid_body" {
		t.Fatalf("code = %q, want invalid_body", code)
	}
	_ = time.Now
}
