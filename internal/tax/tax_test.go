package tax_test

// The importer's contract, against a real database and a fake source server.
//
// The fake is an `httptest` server, not a stubbed fetch function: the acceptance criterion is that
// the fetch is a *real* HTTP call, so the tests assert what the source actually received (the path,
// the method, the User-Agent) and what happens when it answers badly. A stubbed transport would
// prove only that the parser can read its own output.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/catalyst/billing/internal/store"
	"github.com/catalyst/billing/internal/store/storetest"
	"github.com/catalyst/billing/internal/tax"
)

// fakeSource is a tax-rate document served over HTTP.
//
// It records every request so a test can prove the importer really called it, and it can be told to
// answer with a status or a body that the importer must refuse.
type fakeSource struct {
	mu       sync.Mutex
	requests []recordedRequest
	body     string
	status   int
	content  string
	// failNextStatus makes the next request answer with this status and then revert.
	server *httptest.Server
}

type recordedRequest struct {
	Method string
	Path   string
	Agent  string
	Accept string
}

func newFakeSource(t *testing.T, body string) *fakeSource {
	t.Helper()
	fake := &fakeSource{body: body, status: http.StatusOK, content: "application/json"}
	fake.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fake.mu.Lock()
		fake.requests = append(fake.requests, recordedRequest{
			Method: r.Method, Path: r.URL.Path, Agent: r.Header.Get("User-Agent"),
			Accept: r.Header.Get("Accept"),
		})
		status, body, content := fake.status, fake.body, fake.content
		fake.mu.Unlock()
		w.Header().Set("Content-Type", content)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(fake.server.Close)
	return fake
}

func (f *fakeSource) url() string { return f.server.URL + "/rates.json" }

func (f *fakeSource) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

func (f *fakeSource) lastRequest(t *testing.T) recordedRequest {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) == 0 {
		t.Fatal("the source was never fetched")
	}
	return f.requests[len(f.requests)-1]
}

func (f *fakeSource) respond(status int, body, content string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status, f.body, f.content = status, body, content
}

// ---------------------------------------------------------------- fixtures --

// jsonRates is a small, valid document: two new jurisdictions.
const jsonRates = `[
  {"name":"VAT - Germany","country_code":"DE","rate":"0.19","inclusive":true,"effective_from":"2024-01-01"},
  {"name":"TVA - France","country_code":"FR","rate":"0.2"}
]`

// insertManualRate inserts an open rate an operator typed.
func insertManualRate(t *testing.T, ctx context.Context, env *env, country, rate string, inclusive bool) string {
	t.Helper()
	var id string
	if err := env.pool.QueryRow(ctx, `
		INSERT INTO tax_rates (name, country_code, region_code, tax_class, rate, inclusive,
			effective_from, enabled)
		VALUES ($1, $2, '', 'standard', $3, $4, now() - interval '1 day', true)
		RETURNING id`, "Manual "+country, country, rate, inclusive).Scan(&id); err != nil {
		t.Fatalf("insert manual rate: %v", err)
	}
	return id
}

type env struct {
	pool     *store.Pool
	importer *tax.Importer
	now      time.Time
}

// newEnv builds an importer against the test database with a pinned clock.
func newEnv(t *testing.T) *env {
	t.Helper()
	pool := storetest.DB(t)
	e := &env{pool: pool, now: time.Date(2024, 3, 1, 12, 0, 0, 0, time.UTC)}
	e.importer = tax.New(pool)
	e.importer.Now = func() time.Time { return e.now }
	return e
}

func (e *env) configure(t *testing.T, url, format string) tax.Source {
	t.Helper()
	src, err := e.importer.SaveSource(context.Background(), tax.SourceInput{
		URL: url, Format: format,
	}, "")
	if err != nil {
		t.Fatalf("save source: %v", err)
	}
	return src
}

// ------------------------------------------------------------------- fetch --

func TestSourceIsFetchedOverRealHTTP(t *testing.T) {
	fake := newFakeSource(t, jsonRates)
	e := newEnv(t)
	e.configure(t, fake.url(), tax.FormatJSON)

	preview, err := e.importer.Preview(context.Background(), tax.Options{})
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if fake.calls() != 1 {
		t.Fatalf("the source was fetched %d times, want exactly 1", fake.calls())
	}
	last := fake.lastRequest(t)
	if last.Method != http.MethodGet {
		t.Fatalf("method = %s, want GET", last.Method)
	}
	if last.Path != "/rates.json" {
		t.Fatalf("path = %s, want /rates.json", last.Path)
	}
	if !strings.Contains(last.Agent, "catalyst-billing") {
		t.Fatalf("the source was not told who polled it: User-Agent = %q", last.Agent)
	}
	if !strings.Contains(last.Accept, "application/json") {
		t.Fatalf("Accept = %q, want JSON", last.Accept)
	}
	if preview.Rows != 2 || preview.Creates != 2 {
		t.Fatalf("preview = %+v, want 2 rows and 2 creates", preview)
	}
	if preview.SourceURL != fake.url() {
		t.Fatalf("preview source = %q, want %q", preview.SourceURL, fake.url())
	}
	if preview.FetchedAt.IsZero() {
		t.Fatal("the preview has no fetched_at, so a rate's origin cannot be dated")
	}
}

func TestFetchRefusesAnErrorStatus(t *testing.T) {
	fake := newFakeSource(t, `{"error":"nope"}`)
	fake.respond(http.StatusInternalServerError, `{"error":"nope"}`, "application/json")
	e := newEnv(t)
	e.configure(t, fake.url(), tax.FormatJSON)

	if _, err := e.importer.Preview(context.Background(), tax.Options{}); !errors.Is(err, tax.ErrFetch) {
		t.Fatalf("preview of a 500 = %v, want ErrFetch", err)
	}
}

func TestFetchRefusesADocumentOverTheLimit(t *testing.T) {
	fake := newFakeSource(t, strings.Repeat("x", 4096))
	e := newEnv(t)
	e.configure(t, fake.url(), tax.FormatJSON)
	e.importer.MaxBody = 1024

	if _, err := e.importer.Preview(context.Background(), tax.Options{}); !errors.Is(err, tax.ErrFetch) {
		t.Fatalf("preview of an oversized body = %v, want ErrFetch", err)
	}
}

func TestDisabledSourceIsNeverFetched(t *testing.T) {
	fake := newFakeSource(t, jsonRates)
	e := newEnv(t)
	off := false
	if _, err := e.importer.SaveSource(context.Background(), tax.SourceInput{
		URL: fake.url(), Format: tax.FormatJSON, Enabled: &off,
	}, ""); err != nil {
		t.Fatalf("save source: %v", err)
	}

	if _, err := e.importer.Preview(context.Background(), tax.Options{}); !errors.Is(err, tax.ErrSourceDisabled) {
		t.Fatalf("preview of a disabled source = %v, want ErrSourceDisabled", err)
	}
	result, err := e.importer.Refresh(context.Background())
	if err != nil || result != nil {
		t.Fatalf("refresh of a disabled source = (%v, %v), want (nil, nil)", result, err)
	}
	if fake.calls() != 0 {
		t.Fatalf("a disabled source was fetched %d times", fake.calls())
	}
}

func TestSourceConfigurationIsValidated(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		in   tax.SourceInput
	}{
		{"file scheme", tax.SourceInput{URL: "file:///etc/passwd", Format: tax.FormatJSON}},
		{"credentials in the url", tax.SourceInput{URL: "https://user:pw@example.test/rates.json", Format: tax.FormatJSON}},
		{"no host", tax.SourceInput{URL: "https://", Format: tax.FormatJSON}},
		{"unknown format", tax.SourceInput{URL: "https://example.test/rates.xml", Format: "xml"}},
		{"zero interval", tax.SourceInput{URL: "https://example.test/r.json", Format: tax.FormatJSON,
			RefreshIntervalHours: intPtr(0)}},
		{"empty url", tax.SourceInput{Format: tax.FormatJSON}},
	} {
		if _, err := e.importer.SaveSource(ctx, tc.in, ""); !errors.Is(err, tax.ErrInvalidSource) {
			t.Fatalf("%s: err = %v, want ErrInvalidSource", tc.name, err)
		}
	}
	if _, err := e.importer.Source(ctx); !errors.Is(err, tax.ErrNoSource) {
		t.Fatalf("a refused source was stored: %v", err)
	}
}

func intPtr(v int) *int { return &v }

// ------------------------------------------------------------------- parse --

func TestParseRejectsBadRowsWithoutLosingTheGoodOnes(t *testing.T) {
	document := strings.Join([]string{
		"country_code,region_code,tax_class,rate,inclusive,effective_from,name",
		"DE,,standard,0.19,true,2024-01-01,VAT - Germany",
		"X1,,standard,0.20,false,,Bad country",
		"FR,,standard,n/a,false,,Bad rate",
		"IT,,standard,,false,,No rate",
		",,standard,0.22,false,,No country",
		"ES,,standard,0.21,false,,VAT - Spain",
	}, "\n")

	rows, rejections, err := tax.Parse(tax.FormatCSV, []byte(document))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("parsed %d rows, want the 2 good ones: %+v", len(rows), rows)
	}
	if len(rejections) != 4 {
		t.Fatalf("rejections = %+v, want 4", rejections)
	}
	want := map[int]string{
		3: tax.CodeInvalidCountry,
		4: tax.CodeInvalidRate,
		5: tax.CodeMissingRate,
		6: tax.CodeMissingCountry,
	}
	for _, rejection := range rejections {
		code, ok := want[rejection.Line]
		if !ok {
			t.Fatalf("unexpected rejection on line %d: %+v", rejection.Line, rejection)
		}
		if rejection.Code != code {
			t.Fatalf("line %d: code = %s, want %s", rejection.Line, rejection.Code, code)
		}
		if rejection.Detail == "" {
			t.Fatalf("line %d: rejection has no detail", rejection.Line)
		}
		delete(want, rejection.Line)
	}
	if len(want) != 0 {
		t.Fatalf("missing rejections for lines %v", want)
	}
}

func TestParseReadsAJSONNumberAsAFraction(t *testing.T) {
	// 0.19 is nineteen per cent. A number is never a percentage: reading 19 as 19% would be a
	// guess, and reading it as 1900% would charge a customer twenty times their price.
	rows, rejections, err := tax.Parse(tax.FormatJSON,
		[]byte(`{"rates":[{"country_code":"DE","rate":0.19},{"country_code":"FR","rate":19}]}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(rows) != 1 || rows[0].Rate != "0.19" {
		t.Fatalf("rows = %+v, want one row at 0.19", rows)
	}
	if len(rejections) != 1 || rejections[0].Code != tax.CodeInvalidRate {
		t.Fatalf("rejections = %+v, want a single invalid_rate", rejections)
	}
	if !strings.Contains(rejections[0].Detail, "100%") {
		t.Fatalf("the detail does not say why 19 is refused: %q", rejections[0].Detail)
	}
}

func TestParseRefusesADuplicateJurisdiction(t *testing.T) {
	rows, rejections, err := tax.Parse(tax.FormatJSON, []byte(`[
	  {"country_code":"DE","rate":"0.19"},
	  {"country_code":"DE","rate":"0.16"}
	]`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %+v, want the first statement only", rows)
	}
	if len(rejections) != 1 || rejections[0].Code != tax.CodeDuplicateInSource {
		t.Fatalf("rejections = %+v, want duplicate_in_source", rejections)
	}
	if !strings.Contains(rejections[0].Detail, "line 1") {
		t.Fatalf("the duplicate does not name the first row: %q", rejections[0].Detail)
	}
}

func TestParseRefusesADocumentItCannotRead(t *testing.T) {
	for _, tc := range []struct{ name, format, body string }{
		{"invalid json", tax.FormatJSON, `{"rates":`},
		{"object without rates", tax.FormatJSON, `{"data":[]}`},
		{"csv without a header", tax.FormatCSV, ""},
		{"csv without a required column", tax.FormatCSV, "name,value\nVAT,0.19\n"},
	} {
		if _, _, err := tax.Parse(tc.format, []byte(tc.body)); !errors.Is(err, tax.ErrBadDocument) {
			t.Fatalf("%s: err = %v, want ErrBadDocument", tc.name, err)
		}
	}
}

// ----------------------------------------------------------------- preview --

func TestPreviewReportsWhatWouldChangeAndWritesNothing(t *testing.T) {
	fake := newFakeSource(t, jsonRates)
	e := newEnv(t)
	ctx := context.Background()
	e.configure(t, fake.url(), tax.FormatJSON)

	// DE already says 19% (unchanged), IT has a manual rate the source disagrees with (protected),
	// FR is absent (create), and the wildcard has no rate (create).
	insertManualRate(t, ctx, e, "DE", "0.19", true)
	manualIT := insertManualRate(t, ctx, e, "IT", "0.22", false)

	preview, err := e.importer.Preview(ctx, tax.Options{})
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	outcomes := map[string]string{}
	for _, change := range preview.Changes {
		outcomes[change.Jurisdiction] = change.Outcome
	}
	if outcomes["DE/standard"] != tax.OutcomeUnchanged {
		t.Fatalf("DE = %s, want unchanged", outcomes["DE/standard"])
	}
	if outcomes["FR/standard"] != tax.OutcomeCreate {
		t.Fatalf("FR = %s, want create", outcomes["FR/standard"])
	}
	if preview.Creates != 1 || preview.Unchanged != 1 {
		t.Fatalf("preview = %+v, want 1 create and 1 unchanged", preview)
	}

	var rows int
	if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM tax_rates`).Scan(&rows); err != nil {
		t.Fatalf("count: %v", err)
	}
	if rows != 2 {
		t.Fatalf("the preview wrote %d rows; it must write nothing", rows-2)
	}
	var openIT int
	if err := e.pool.QueryRow(ctx,
		`SELECT count(*) FROM tax_rates WHERE id = $1 AND effective_to IS NULL`, manualIT).Scan(&openIT); err != nil {
		t.Fatalf("count: %v", err)
	}
	if openIT != 1 {
		t.Fatal("the preview closed the manual IT rate")
	}
}

func TestPreviewProtectsAManualRateAndSaysSo(t *testing.T) {
	fake := newFakeSource(t, `[{"country_code":"IT","rate":"0.20"}]`)
	e := newEnv(t)
	ctx := context.Background()
	e.configure(t, fake.url(), tax.FormatJSON)
	manualID := insertManualRate(t, ctx, e, "IT", "0.22", false)

	preview, err := e.importer.Preview(ctx, tax.Options{})
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if len(preview.Changes) != 1 {
		t.Fatalf("changes = %+v, want one", preview.Changes)
	}
	change := preview.Changes[0]
	if change.Outcome != tax.OutcomeProtected {
		t.Fatalf("outcome = %s, want protected", change.Outcome)
	}
	if change.Reason == "" {
		t.Fatal("a protected row has no reason, so the screen cannot explain it")
	}
	if change.Current == nil || change.Current.ID != manualID || change.Current.Origin != tax.OriginManual {
		t.Fatalf("the change does not show the rate it protects: %+v", change.Current)
	}
	if preview.Protected != 1 || preview.Updates != 0 {
		t.Fatalf("preview = %+v, want 1 protected and no update", preview)
	}

	// The same preview with permission reports an update instead — the protection is a policy the
	// caller opts out of, not a hard refusal.
	allowed, err := e.importer.Preview(ctx, tax.Options{OverwriteManual: true})
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if allowed.Changes[0].Outcome != tax.OutcomeUpdate {
		t.Fatalf("with overwrite_manual the outcome is %s, want update", allowed.Changes[0].Outcome)
	}
}

// ------------------------------------------------------------------- apply --

func TestImportRecordsProvenanceOnEveryRate(t *testing.T) {
	fake := newFakeSource(t, jsonRates)
	e := newEnv(t)
	ctx := context.Background()
	e.configure(t, fake.url(), tax.FormatJSON)

	result, err := e.importer.Apply(ctx, tax.Options{Trigger: tax.TriggerManual})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if result.Run.Counts.Created != 2 {
		t.Fatalf("run = %+v, want 2 created", result.Run.Counts)
	}
	if result.Run.Outcome != tax.OutcomeRunOK {
		t.Fatalf("run outcome = %s, want ok", result.Run.Outcome)
	}

	for _, country := range []string{"DE", "FR"} {
		var (
			origin    string
			source    string
			fetchedAt *time.Time
		)
		if err := e.pool.QueryRow(ctx, `
			SELECT origin, source, fetched_at
			  FROM tax_rates
			 WHERE country_code = $1 AND effective_to IS NULL`, country).
			Scan(&origin, &source, &fetchedAt); err != nil {
			t.Fatalf("%s: %v", country, err)
		}
		if origin != tax.OriginImport {
			t.Fatalf("%s origin = %s, want import", country, origin)
		}
		if source != fake.url() {
			t.Fatalf("%s source = %q, want %q", country, source, fake.url())
		}
		if fetchedAt == nil || fetchedAt.IsZero() {
			t.Fatalf("%s has no fetched_at, so its origin cannot be dated", country)
		}
	}

	provenance, err := e.importer.Provenance(ctx)
	if err != nil {
		t.Fatalf("provenance: %v", err)
	}
	if len(provenance) != 2 {
		t.Fatalf("provenance has %d entries, want 2", len(provenance))
	}
	for _, entry := range provenance {
		if entry.Origin != tax.OriginImport || entry.FetchedAt == nil {
			t.Fatalf("provenance entry = %+v, want import with a fetch time", entry)
		}
	}
}

func TestImportNeverOverwritesAManualRateUnlessAsked(t *testing.T) {
	fake := newFakeSource(t, `[{"country_code":"DE","rate":"0.19"}]`)
	e := newEnv(t)
	ctx := context.Background()
	e.configure(t, fake.url(), tax.FormatJSON)
	manualID := insertManualRate(t, ctx, e, "DE", "0.16", false)

	// 1. Without permission: the manual rate is reported as protected and left exactly as it was.
	result, err := e.importer.Apply(ctx, tax.Options{Trigger: tax.TriggerManual})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if result.Run.Counts.Protected != 1 || result.Run.Counts.Updated != 0 || result.Run.Counts.Created != 0 {
		t.Fatalf("counts = %+v, want one protected row and no writes", result.Run.Counts)
	}
	assertOpenRate(t, e, "DE", "0.16", manualID, tax.OriginManual)

	// 2. With permission: the manual row is closed (not rewritten) and a new imported row is
	// appended from the moment the import ran.
	result, err = e.importer.Apply(ctx, tax.Options{Trigger: tax.TriggerManual, OverwriteManual: true})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if result.Run.Counts.Updated != 1 {
		t.Fatalf("counts = %+v, want one update", result.Run.Counts)
	}
	var closed *time.Time
	var closedRate string
	if err := e.pool.QueryRow(ctx,
		`SELECT effective_to, rate FROM tax_rates WHERE id = $1`, manualID).
		Scan(&closed, &closedRate); err != nil {
		t.Fatalf("read manual rate: %v", err)
	}
	if closed == nil {
		t.Fatal("the replaced manual rate is still open; the change was a rewrite, not an append")
	}
	if closedRate != "0.16" {
		t.Fatalf("the manual row's rate was rewritten to %s; a filed rate is evidence", closedRate)
	}
	var newID, origin string
	if err := e.pool.QueryRow(ctx, `
		SELECT id, origin FROM tax_rates
		 WHERE country_code = 'DE' AND effective_to IS NULL`).Scan(&newID, &origin); err != nil {
		t.Fatalf("read new rate: %v", err)
	}
	if newID == manualID {
		t.Fatal("the open DE rate is still the manual row")
	}
	if origin != tax.OriginImport {
		t.Fatalf("the new row's origin = %s, want import", origin)
	}
	assertOpenRate(t, e, "DE", "0.19", newID, tax.OriginImport)
}

func assertOpenRate(t *testing.T, e *env, country, wantRate, wantID, wantOrigin string) {
	t.Helper()
	var id, rate, origin string
	if err := e.pool.QueryRow(context.Background(), `
		SELECT id, rate, origin FROM tax_rates
		 WHERE country_code = $1 AND effective_to IS NULL AND enabled`, country).
		Scan(&id, &rate, &origin); err != nil {
		t.Fatalf("%s: no open rate: %v", country, err)
	}
	if id != wantID {
		t.Fatalf("%s open rate id = %s, want %s", country, id, wantID)
	}
	if rate != wantRate {
		t.Fatalf("%s rate = %s, want %s", country, rate, wantRate)
	}
	if origin != wantOrigin {
		t.Fatalf("%s origin = %s, want %s", country, origin, wantOrigin)
	}
}

func TestImportAppliesTheGoodRowsAndReportsTheBadOnes(t *testing.T) {
	document := strings.Join([]string{
		"country_code,rate,effective_from",
		"DE,0.19,2024-01-01",
		"FR,n/a,",
		"ES,0.21,",
	}, "\n")
	fake := newFakeSource(t, document)
	fake.respond(http.StatusOK, document, "text/csv")
	e := newEnv(t)
	ctx := context.Background()
	e.configure(t, fake.url(), tax.FormatCSV)

	result, err := e.importer.Apply(ctx, tax.Options{Trigger: tax.TriggerManual})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if result.Run.Counts.Created != 2 {
		t.Fatalf("created = %d, want the 2 well-formed rows", result.Run.Counts.Created)
	}
	if result.Run.Counts.Rejected != 1 {
		t.Fatalf("rejected = %d, want 1", result.Run.Counts.Rejected)
	}
	if result.Run.Outcome != tax.OutcomeRunOK {
		t.Fatalf("outcome = %s; a refused row is not a failed import", result.Run.Outcome)
	}
	if len(result.Preview.Rejections) != 1 {
		t.Fatalf("report rejections = %+v, want 1", result.Preview.Rejections)
	}
	rejection := result.Preview.Rejections[0]
	if rejection.Code != tax.CodeInvalidRate || rejection.Line != 3 {
		t.Fatalf("rejection = %+v, want invalid_rate on line 3", rejection)
	}

	// The refusals are readable later: the run's stored report is the same document.
	runs, err := e.importer.Runs(ctx, 10)
	if err != nil {
		t.Fatalf("runs: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("runs = %d, want 1", len(runs))
	}
	if runs[0].Preview == nil || len(runs[0].Preview.Rejections) != 1 {
		t.Fatalf("the stored run lost its rejections: %+v", runs[0].Preview)
	}
	if runs[0].Preview.Rejections[0].Code != tax.CodeInvalidRate {
		t.Fatalf("stored rejection = %+v", runs[0].Preview.Rejections[0])
	}

	// The good rows are in the table, with provenance.
	assertOpenRateCountry(t, e, "DE")
	assertOpenRateCountry(t, e, "ES")
}

func assertOpenRateCountry(t *testing.T, e *env, country string) {
	t.Helper()
	var origin string
	if err := e.pool.QueryRow(context.Background(), `
		SELECT origin FROM tax_rates
		 WHERE country_code = $1 AND effective_to IS NULL`, country).Scan(&origin); err != nil {
		t.Fatalf("%s was not imported: %v", country, err)
	}
	if origin != tax.OriginImport {
		t.Fatalf("%s origin = %s, want import", country, origin)
	}
}

func TestFailedFetchIsRecordedAsAFailedRun(t *testing.T) {
	fake := newFakeSource(t, `{"rates":`)
	e := newEnv(t)
	ctx := context.Background()
	e.configure(t, fake.url(), tax.FormatJSON)

	if _, err := e.importer.Apply(ctx, tax.Options{Trigger: tax.TriggerManual}); !errors.Is(err, tax.ErrBadDocument) {
		t.Fatalf("apply of an unreadable document = %v, want ErrBadDocument", err)
	}
	runs, err := e.importer.Runs(ctx, 10)
	if err != nil {
		t.Fatalf("runs: %v", err)
	}
	if len(runs) != 1 || runs[0].Outcome != tax.OutcomeRunFailed {
		t.Fatalf("runs = %+v, want one failed run", runs)
	}
	if runs[0].Detail == "" {
		t.Fatal("the failed run has no detail, so the operator cannot see why")
	}
	var rates int
	if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM tax_rates`).Scan(&rates); err != nil {
		t.Fatalf("count: %v", err)
	}
	if rates != 0 {
		t.Fatalf("a failed import wrote %d rates", rates)
	}
}

// ---------------------------------------------------------------- schedule --

func TestScheduledRefreshWaitsForItsInterval(t *testing.T) {
	fake := newFakeSource(t, `[{"country_code":"DE","rate":"0.19"}]`)
	e := newEnv(t)
	ctx := context.Background()
	e.configure(t, fake.url(), tax.FormatJSON)

	first, err := e.importer.Refresh(ctx)
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if first == nil || first.Run.Trigger != tax.TriggerScheduled {
		t.Fatalf("first refresh = %+v, want a scheduled run", first)
	}
	if fake.calls() != 1 {
		t.Fatalf("the source was fetched %d times, want 1", fake.calls())
	}

	// Same instant: the interval has not elapsed, so nothing is fetched.
	again, err := e.importer.Refresh(ctx)
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if again != nil {
		t.Fatalf("a second refresh at the same instant ran again: %+v", again)
	}
	if fake.calls() != 1 {
		t.Fatalf("the source was fetched %d times, want 1", fake.calls())
	}

	// Past the interval: it fetches, and the rate is unchanged so nothing is written.
	e.now = e.now.Add(25 * time.Hour)
	third, err := e.importer.Refresh(ctx)
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if third == nil {
		t.Fatal("the refresh did not run after its interval elapsed")
	}
	if third.Run.Counts.Unchanged != 1 || third.Run.Counts.Created != 0 || third.Run.Counts.Updated != 0 {
		t.Fatalf("counts = %+v, want the rate reported unchanged", third.Run.Counts)
	}
	if fake.calls() != 2 {
		t.Fatalf("the source was fetched %d times, want 2", fake.calls())
	}
}

func TestScheduledRefreshNeverOverwritesAManualRate(t *testing.T) {
	fake := newFakeSource(t, `[{"country_code":"DE","rate":"0.19"}]`)
	e := newEnv(t)
	ctx := context.Background()
	e.configure(t, fake.url(), tax.FormatJSON)
	manualID := insertManualRate(t, ctx, e, "DE", "0.16", false)

	result, err := e.importer.Refresh(ctx)
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if result == nil {
		t.Fatal("the refresh did not run")
	}
	if result.Run.Counts.Protected != 1 {
		t.Fatalf("counts = %+v, want the manual rate protected", result.Run.Counts)
	}
	assertOpenRate(t, e, "DE", "0.16", manualID, tax.OriginManual)
}

func TestRefreshWithoutASourceDoesNothingAndIsNotAFailure(t *testing.T) {
	e := newEnv(t)
	result, err := e.importer.Refresh(context.Background())
	if err != nil {
		t.Fatalf("refresh with no source = %v, want nil", err)
	}
	if result != nil {
		t.Fatalf("refresh with no source = %+v, want nil", result)
	}
}

func TestRefreshRetriesSoonAfterAFailure(t *testing.T) {
	fake := newFakeSource(t, `not json`)
	e := newEnv(t)
	ctx := context.Background()
	e.configure(t, fake.url(), tax.FormatJSON)

	if _, err := e.importer.Refresh(ctx); err == nil {
		t.Fatal("a refresh of an unreadable document returned no error")
	}
	// The default interval is a day, but a failure is retried in fifteen minutes: a source that
	// was briefly broken must not leave the table a day stale.
	e.now = e.now.Add(20 * time.Minute)
	if _, err := e.importer.Refresh(ctx); err == nil {
		t.Fatal("the refresh did not retry after a failure")
	}
	if fake.calls() != 2 {
		t.Fatalf("the source was fetched %d times, want 2", fake.calls())
	}
}
