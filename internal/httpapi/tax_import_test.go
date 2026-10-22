package httpapi_test

// The tax-rate import surface, driven through HTTP the way the screen drives it.
//
// The source is a real `httptest` server, so these tests prove the whole path: a URL configured
// through the API is fetched over HTTP, previewed, imported, and the provenance it records is
// readable afterwards. Nothing here stubs the fetch.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// taxSourceServer is a rate document served over HTTP, with a request counter.
type taxSourceServer struct {
	mu    sync.Mutex
	calls int
	body  string
	// status is the HTTP status to answer with.
	status  int
	content string
	server  *httptest.Server
}

func newTaxSourceServer(t *testing.T, body string) *taxSourceServer {
	t.Helper()
	fake := &taxSourceServer{body: body, status: http.StatusOK, content: "application/json"}
	fake.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fake.mu.Lock()
		fake.calls++
		status, body, content := fake.status, fake.body, fake.content
		fake.mu.Unlock()
		w.Header().Set("Content-Type", content)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(fake.server.Close)
	return fake
}

func (f *taxSourceServer) url() string { return f.server.URL + "/rates" }

func (f *taxSourceServer) set(body, content string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.body, f.content = body, content
}

func (f *taxSourceServer) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// staffAdmin signs in as an administrator and returns the session and CSRF token.
func staffAdmin(t *testing.T, e *authEnv) (*http.Cookie, string) {
	t.Helper()
	email := "tax-admin-" + t.Name() + "@example.test"
	e.createUser(t, email, "tax-admin-password-1", "administrator")
	return e.login(t, email, "tax-admin-password-1")
}

// taxSourceResponse is the source screen's payload.
type taxSourceResponse struct {
	Configured bool `json:"configured"`
	Source     struct {
		URL                  string `json:"url"`
		Format               string `json:"format"`
		Enabled              bool   `json:"enabled"`
		RefreshIntervalHours int    `json:"refresh_interval_hours"`
	} `json:"source"`
}

// taxChange is one row of a preview.
type taxChange struct {
	Line         int    `json:"line"`
	Jurisdiction string `json:"jurisdiction"`
	Outcome      string `json:"outcome"`
	Reason       string `json:"reason"`
	Code         string `json:"code"`
}

type taxPreviewResponse struct {
	Preview struct {
		SourceURL  string      `json:"source_url"`
		FetchedAt  string      `json:"fetched_at"`
		Rows       int         `json:"rows"`
		Creates    int         `json:"creates"`
		Updates    int         `json:"updates"`
		Unchanged  int         `json:"unchanged"`
		Protected  int         `json:"protected"`
		Rejected   int         `json:"rejected"`
		Changes    []taxChange `json:"changes"`
		Rejections []struct {
			Line   int    `json:"line"`
			Code   string `json:"code"`
			Detail string `json:"detail"`
		} `json:"rejections"`
	} `json:"preview"`
}

type taxImportResponse struct {
	Run struct {
		ID      string `json:"id"`
		Outcome string `json:"outcome"`
		Trigger string `json:"trigger"`
		Counts  struct {
			Rows      int `json:"rows"`
			Created   int `json:"created"`
			Updated   int `json:"updated"`
			Unchanged int `json:"unchanged"`
			Protected int `json:"protected"`
			Rejected  int `json:"rejected"`
		} `json:"counts"`
	} `json:"run"`
	Preview struct {
		Changes    []taxChange `json:"changes"`
		Rejections []struct {
			Line int    `json:"line"`
			Code string `json:"code"`
		} `json:"rejections"`
	} `json:"preview"`
}

// taxProvenanceResponse is one rate's origin.
type taxProvenanceResponse struct {
	Data []struct {
		RateID    string  `json:"rate_id"`
		Origin    string  `json:"origin"`
		Source    string  `json:"source"`
		FetchedAt *string `json:"fetched_at"`
	} `json:"data"`
}

const taxJSONDocument = `[
  {"name":"VAT - Germany","country_code":"DE","rate":"0.19","inclusive":true,"effective_from":"2024-01-01"},
  {"name":"TVA - France","country_code":"FR","rate":"0.2"}
]`

// The screen's whole journey: configure, preview, import, and read the provenance back.
func TestTaxImportCanBeConfiguredPreviewedAndImported(t *testing.T) {
	env := newAuthEnv(t)
	cookie, csrf := staffAdmin(t, env)
	source := newTaxSourceServer(t, taxJSONDocument)

	// 1. Nothing is configured yet, and that is a state rather than an error.
	rec := env.get(t, "/api/v1/staff/tax-import/source", cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("source: status %d, body %s", rec.Code, rec.Body)
	}
	var before taxSourceResponse
	decodeTax(t, rec, &before)
	if before.Configured {
		t.Fatalf("a fresh installation reports a configured source: %+v", before)
	}

	// 2. Configure it.
	rec = env.putJSON(t, "/api/v1/staff/tax-import/source", cookie, csrf, map[string]any{
		"url": source.url(), "format": "json", "refresh_interval_hours": 12,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("save source: status %d, body %s", rec.Code, rec.Body)
	}
	var saved taxSourceResponse
	decodeTax(t, rec, &saved)
	if !saved.Configured || saved.Source.URL != source.url() {
		t.Fatalf("saved source = %+v, want %s", saved, source.url())
	}
	if saved.Source.RefreshIntervalHours != 12 || saved.Source.Format != "json" {
		t.Fatalf("saved source = %+v, want 12 hours and json", saved.Source)
	}

	// 3. Preview: a real HTTP fetch, and nothing written.
	rec = env.doJSON(t, http.MethodPost, "/api/v1/staff/tax-import/preview", cookie, csrf,
		map[string]any{})
	if rec.Code != http.StatusOK {
		t.Fatalf("preview: status %d, body %s", rec.Code, rec.Body)
	}
	var preview taxPreviewResponse
	decodeTax(t, rec, &preview)
	if source.callCount() != 1 {
		t.Fatalf("the preview fetched the source %d times, want 1", source.callCount())
	}
	if preview.Preview.Creates != 2 || preview.Preview.Rows != 2 {
		t.Fatalf("preview = %+v, want 2 creates", preview.Preview)
	}
	if preview.Preview.SourceURL != source.url() || preview.Preview.FetchedAt == "" {
		t.Fatalf("the preview does not say what it fetched: %+v", preview.Preview)
	}
	var rates int
	if err := env.pool.QueryRow(context.Background(), `SELECT count(*) FROM tax_rates`).Scan(&rates); err != nil {
		t.Fatalf("count: %v", err)
	}
	if rates != 0 {
		t.Fatalf("the preview wrote %d rates", rates)
	}

	// 4. Import.
	rec = env.doJSON(t, http.MethodPost, "/api/v1/staff/tax-import/import", cookie, csrf,
		map[string]any{})
	if rec.Code != http.StatusOK {
		t.Fatalf("import: status %d, body %s", rec.Code, rec.Body)
	}
	var imported taxImportResponse
	decodeTax(t, rec, &imported)
	if imported.Run.Counts.Created != 2 || imported.Run.Outcome != "ok" {
		t.Fatalf("import = %+v, want 2 created and ok", imported.Run)
	}
	if imported.Run.Trigger != "manual" {
		t.Fatalf("trigger = %s, want manual", imported.Run.Trigger)
	}
	if len(imported.Preview.Changes) != 2 {
		t.Fatalf("the import report has %d changes, want 2", len(imported.Preview.Changes))
	}

	// 5. The rate table the screen renders now carries the imported rates.
	rec = env.get(t, "/api/v1/staff/tax-rates", cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("rates: status %d, body %s", rec.Code, rec.Body)
	}
	var listed struct {
		Data []struct {
			ID          string `json:"id"`
			CountryCode string `json:"country_code"`
			Rate        string `json:"rate"`
		} `json:"data"`
	}
	decodeTax(t, rec, &listed)
	if len(listed.Data) != 2 {
		t.Fatalf("the rate list has %d rows, want 2: %s", len(listed.Data), rec.Body)
	}
	byCountry := map[string]string{}
	for _, row := range listed.Data {
		byCountry[row.CountryCode] = row.Rate
	}
	if byCountry["DE"] != "0.19" || byCountry["FR"] != "0.2" {
		t.Fatalf("imported rates = %+v, want DE 0.19 and FR 0.2", byCountry)
	}

	// 6. Provenance is answerable afterwards, per row.
	rec = env.get(t, "/api/v1/staff/tax-import/provenance", cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("provenance: status %d, body %s", rec.Code, rec.Body)
	}
	var provenance taxProvenanceResponse
	decodeTax(t, rec, &provenance)
	if len(provenance.Data) != 2 {
		t.Fatalf("provenance has %d entries, want 2", len(provenance.Data))
	}
	for _, entry := range provenance.Data {
		if entry.Origin != "import" {
			t.Fatalf("origin = %s, want import", entry.Origin)
		}
		if entry.Source != source.url() {
			t.Fatalf("source = %q, want %q", entry.Source, source.url())
		}
		if entry.FetchedAt == nil || *entry.FetchedAt == "" {
			t.Fatal("the provenance has no fetched_at")
		}
	}

	// 7. The run is in the history, with its report.
	rec = env.get(t, "/api/v1/staff/tax-import/runs", cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("runs: status %d, body %s", rec.Code, rec.Body)
	}
	var runs struct {
		Data []struct {
			ID      string `json:"id"`
			Outcome string `json:"outcome"`
			Counts  struct {
				Created int `json:"created"`
			} `json:"counts"`
		} `json:"data"`
	}
	decodeTax(t, rec, &runs)
	if len(runs.Data) != 1 || runs.Data[0].Counts.Created != 2 {
		t.Fatalf("runs = %+v, want one run that created 2 rates", runs.Data)
	}
}

// A rate an operator typed is not replaced unless the request says so, and the response says which
// rows were left alone.
func TestTaxImportDoesNotOverwriteAManualRateByDefault(t *testing.T) {
	env := newAuthEnv(t)
	cookie, csrf := staffAdmin(t, env)
	source := newTaxSourceServer(t, `[{"country_code":"IT","rate":"0.20"}]`)

	if _, err := env.pool.Exec(context.Background(), `
		INSERT INTO tax_rates (name, country_code, region_code, tax_class, rate, inclusive,
			effective_from, enabled)
		VALUES ('Typed by an operator', 'IT', '', 'standard', '0.22', false,
		        now() - interval '1 day', true)`); err != nil {
		t.Fatalf("seed manual rate: %v", err)
	}
	rec := env.putJSON(t, "/api/v1/staff/tax-import/source", cookie, csrf, map[string]any{
		"url": source.url(), "format": "json",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("save source: status %d, body %s", rec.Code, rec.Body)
	}

	rec = env.doJSON(t, http.MethodPost, "/api/v1/staff/tax-import/import", cookie, csrf,
		map[string]any{})
	if rec.Code != http.StatusOK {
		t.Fatalf("import: status %d, body %s", rec.Code, rec.Body)
	}
	var first taxImportResponse
	decodeTax(t, rec, &first)
	if first.Run.Counts.Protected != 1 || first.Run.Counts.Updated != 0 {
		t.Fatalf("counts = %+v, want one protected row and no update", first.Run.Counts)
	}
	if len(first.Preview.Changes) != 1 || first.Preview.Changes[0].Outcome != "protected" {
		t.Fatalf("changes = %+v, want a protected row", first.Preview.Changes)
	}
	if first.Preview.Changes[0].Reason == "" {
		t.Fatal("the protected row does not say why, so the screen cannot explain it")
	}

	var rate string
	if err := env.pool.QueryRow(context.Background(), `
		SELECT rate FROM tax_rates WHERE country_code = 'IT' AND effective_to IS NULL`).Scan(&rate); err != nil {
		t.Fatalf("read rate: %v", err)
	}
	if rate != "0.22" {
		t.Fatalf("the manual rate was changed to %s; an import must not do that unasked", rate)
	}

	// With explicit permission the same import replaces it — by closing the manual row and
	// appending a new one, so the period it covered is still explained by the row that covered it.
	rec = env.doJSON(t, http.MethodPost, "/api/v1/staff/tax-import/import", cookie, csrf,
		map[string]any{"overwrite_manual": true})
	if rec.Code != http.StatusOK {
		t.Fatalf("import with permission: status %d, body %s", rec.Code, rec.Body)
	}
	var second taxImportResponse
	decodeTax(t, rec, &second)
	if second.Run.Counts.Updated != 1 {
		t.Fatalf("counts = %+v, want one update", second.Run.Counts)
	}
	var open, closed int
	if err := env.pool.QueryRow(context.Background(), `
		SELECT count(*) FILTER (WHERE effective_to IS NULL), count(*) FILTER (WHERE effective_to IS NOT NULL)
		  FROM tax_rates WHERE country_code = 'IT'`).Scan(&open, &closed); err != nil {
		t.Fatalf("count rates: %v", err)
	}
	if open != 1 || closed != 1 {
		t.Fatalf("IT has %d open and %d closed rows, want 1 and 1", open, closed)
	}
}

// A malformed row is refused on its own, and the refusals are in the response.
func TestTaxImportReportsPerRowRejections(t *testing.T) {
	env := newAuthEnv(t)
	cookie, csrf := staffAdmin(t, env)
	document := "country_code,rate\nDE,0.19\nFR,not-a-rate\nES,0.21\n"
	source := newTaxSourceServer(t, document)
	source.set(document, "text/csv")

	rec := env.putJSON(t, "/api/v1/staff/tax-import/source", cookie, csrf, map[string]any{
		"url": source.url(), "format": "csv",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("save source: status %d, body %s", rec.Code, rec.Body)
	}

	rec = env.doJSON(t, http.MethodPost, "/api/v1/staff/tax-import/import", cookie, csrf,
		map[string]any{})
	if rec.Code != http.StatusOK {
		t.Fatalf("import: status %d, body %s", rec.Code, rec.Body)
	}
	var imported taxImportResponse
	decodeTax(t, rec, &imported)
	if imported.Run.Counts.Created != 2 {
		t.Fatalf("created = %d, want the 2 well-formed rows", imported.Run.Counts.Created)
	}
	if imported.Run.Counts.Rejected != 1 {
		t.Fatalf("rejected = %d, want 1", imported.Run.Counts.Rejected)
	}
	if len(imported.Preview.Rejections) != 1 {
		t.Fatalf("rejections = %+v, want 1", imported.Preview.Rejections)
	}
	if imported.Preview.Rejections[0].Code != "invalid_rate" {
		t.Fatalf("rejection code = %s, want invalid_rate", imported.Preview.Rejections[0].Code)
	}
	if imported.Preview.Rejections[0].Line != 3 {
		t.Fatalf("rejection line = %d, want 3", imported.Preview.Rejections[0].Line)
	}
}

// The error codes the screen branches on.
func TestTaxImportErrorCodes(t *testing.T) {
	env := newAuthEnv(t)
	cookie, csrf := staffAdmin(t, env)

	// No source yet.
	if code := problemCode(t, env.doJSON(t, http.MethodPost,
		"/api/v1/staff/tax-import/preview", cookie, csrf, map[string]any{})); code != "no_source" {
		t.Fatalf("preview with no source: code = %q, want no_source", code)
	}

	// A URL the panel will not store.
	if code := problemCode(t, env.putJSON(t, "/api/v1/staff/tax-import/source", cookie, csrf,
		map[string]any{"url": "file:///etc/passwd", "format": "json"})); code != "invalid_source" {
		t.Fatalf("file:// source: code = %q, want invalid_source", code)
	}

	// A source that is switched off.
	off := newTaxSourceServer(t, taxJSONDocument)
	rec := env.putJSON(t, "/api/v1/staff/tax-import/source", cookie, csrf, map[string]any{
		"url": off.url(), "format": "json", "enabled": false,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("save source: status %d, body %s", rec.Code, rec.Body)
	}
	if code := problemCode(t, env.doJSON(t, http.MethodPost,
		"/api/v1/staff/tax-import/preview", cookie, csrf, map[string]any{})); code != "source_disabled" {
		t.Fatalf("preview of a disabled source: code = %q, want source_disabled", code)
	}

	// A document that cannot be read at all.
	broken := newTaxSourceServer(t, `{"rates":`)
	rec = env.putJSON(t, "/api/v1/staff/tax-import/source", cookie, csrf, map[string]any{
		"url": broken.url(), "format": "json",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("save source: status %d, body %s", rec.Code, rec.Body)
	}
	if code := problemCode(t, env.doJSON(t, http.MethodPost,
		"/api/v1/staff/tax-import/preview", cookie, csrf, map[string]any{})); code != "source_document_invalid" {
		t.Fatalf("preview of a broken document: code = %q, want source_document_invalid", code)
	}

	// A source that cannot be reached.
	gone := newTaxSourceServer(t, taxJSONDocument)
	url := gone.url()
	gone.server.Close()
	rec = env.putJSON(t, "/api/v1/staff/tax-import/source", cookie, csrf, map[string]any{
		"url": url, "format": "json",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("save source: status %d, body %s", rec.Code, rec.Body)
	}
	if code := problemCode(t, env.doJSON(t, http.MethodPost,
		"/api/v1/staff/tax-import/preview", cookie, csrf, map[string]any{})); code != "source_unreachable" {
		t.Fatalf("preview of an unreachable source: code = %q, want source_unreachable", code)
	}
}

// The surface is guarded by tax:write, like the rate table it feeds.
func TestTaxImportNeedsTheTaxPermission(t *testing.T) {
	env := newAuthEnv(t)
	email := "tax-support-" + t.Name() + "@example.test"
	env.createUser(t, email, "tax-support-password-1", "support")
	cookie, csrf := env.login(t, email, "tax-support-password-1")

	if rec := env.get(t, "/api/v1/staff/tax-import/source", cookie); rec.Code != http.StatusForbidden {
		t.Fatalf("a support agent read the tax source: status %d", rec.Code)
	}
	if code := problemCode(t, env.doJSON(t, http.MethodPost,
		"/api/v1/staff/tax-import/import", cookie, csrf, map[string]any{})); code != "forbidden" {
		t.Fatalf("a support agent ran an import: code = %q", code)
	}
}

// decodeTax decodes a response body, failing with the body when it is not JSON.
func decodeTax(t *testing.T, rec *httptest.ResponseRecorder, dst any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), dst); err != nil {
		t.Fatalf("decode %s: %v", rec.Body.String(), err)
	}
}
