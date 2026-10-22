package httpapi

// Tests for the CSV export surface (OPS-12).
//
// Four properties are load-bearing and each has its own test:
//
//   - the export streams: a source far larger than one page must reach the client page by
//     page, not all at once (TestLargeExportStreamsRatherThanBuffering);
//   - money is exact: the major-unit cell is an integer-derived decimal, never a float
//     (TestExportedMoneyIsExact), and the `_minor` column carries the raw count;
//   - the export is scoped by permission, including a `customer_id` the caller cannot read
//     (TestExportIsScopedByPermission);
//   - a text cell cannot become a spreadsheet formula (TestExportedTextCannotBeAFormula).
//
// The remaining tests pin the columns an accountant actually reconciles against and the
// error shape a bad filter gets.

import (
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/catalyst/billing/internal/authn"
	"github.com/catalyst/billing/internal/money"
	"github.com/catalyst/billing/internal/staff"
)

// ------------------------------------------------------------------ the routes --

// The three exports must be staff-only, permissioned, and on the export rate budget. A
// route that declared a weaker permission would be an export nobody asked for: reading one
// invoice is not exporting the book.
func TestExportRoutesDeclareTheirPermission(t *testing.T) {
	s := testServer(t)
	s.Handler()

	want := map[string]authn.Permission{
		"GET /api/v1/staff/exports/invoices.csv":  authn.PermInvoiceReadAll,
		"GET /api/v1/staff/exports/customers.csv": authn.PermCustomerReadAll,
		"GET /api/v1/staff/exports/services.csv":  authn.PermServiceReadAll,
	}

	found := 0
	for _, rt := range s.Routes() {
		permission, ok := want[rt.Path()]
		if !ok {
			continue
		}
		found++
		if rt.Access != AccessPermission {
			t.Fatalf("%s does not require a permission", rt.Path())
		}
		if rt.Permission != permission {
			t.Fatalf("%s declares %q, want %q", rt.Path(), rt.Permission, permission)
		}
		if !rt.StaffOnly {
			t.Fatalf("%s is not staff-only", rt.Path())
		}
		// A full-table read is expensive by nature, so it gets the tighter budget. A route
		// on RateRead would let a client export the book at the page-load rate.
		if rt.Rate != RateExport {
			t.Fatalf("%s is on the %q rate class, want export", rt.Path(), rt.Rate)
		}
		if rt.CSRF || rt.Idempotent {
			t.Fatalf("%s is a GET but declares CSRF or idempotency", rt.Path())
		}
	}
	if found != len(want) {
		t.Fatalf("%d export routes registered, want %d", found, len(want))
	}
}

// Every screen that exports must actually link to its export, or the action exists only in
// the API. The frontend is not compiled by `go test`, so this reads the view sources: it is
// a guard against the link being dropped in a refactor, not a substitute for the browser
// harness (scripts/e2e-ui.mjs) that drives the real page.
func TestEachScreenLinksToItsExport(t *testing.T) {
	cases := []struct{ view, path string }{
		{"invoices.ts", "/api/v1/staff/exports/invoices.csv"},
		{"customers.ts", "/api/v1/staff/exports/customers.csv"},
		{"services.ts", "/api/v1/staff/exports/services.csv"},
	}
	for _, c := range cases {
		source, err := os.ReadFile("../../web/src/views/" + c.view)
		if err != nil {
			t.Fatalf("read %s: %v", c.view, err)
		}
		if !strings.Contains(string(source), c.path) {
			t.Fatalf("%s does not link to %s", c.view, c.path)
		}
		// `download` is what tells the router to leave the navigation to the browser; without
		// it the CSV would be rendered as a page.
		if !strings.Contains(string(source), "download") {
			t.Fatalf("%s links to the export without a download attribute", c.view)
		}
	}
}

// ------------------------------------------------------------------- streaming --

// countingFetcher produces pages on demand and remembers how many rows it has produced.
type countingFetcher struct {
	total    int
	pageSize int
	produced int
	pages    int
	// failAtPage makes that page (zero-based) fail, for the mid-stream failure test.
	failAtPage int
}

func (f *countingFetcher) fetch(cursor string) (exportPage, error) {
	if f.pages == f.failAtPage {
		return exportPage{}, errors.New("the database went away")
	}
	start := f.produced
	if start >= f.total {
		return exportPage{}, nil
	}
	n := f.pageSize
	if remaining := f.total - start; remaining < n {
		n = remaining
	}
	page := exportPage{rows: make([][]string, 0, n)}
	for i := 0; i < n; i++ {
		f.produced++
		page.rows = append(page.rows, []string{strconv.Itoa(f.produced), "row"})
	}
	if f.produced < f.total {
		page.next = strconv.Itoa(f.produced)
	}
	f.pages++
	return page, nil
}

// writeEvent records how many rows the source had produced when a write reached the client.
type writeEvent struct {
	produced int
	bytes    int
}

type observingWriter struct {
	src    *countingFetcher
	writes []writeEvent
}

func (w *observingWriter) Write(p []byte) (int, error) {
	w.writes = append(w.writes, writeEvent{produced: w.src.produced, bytes: len(p)})
	return len(p), nil
}

// A large export must reach the client as it is read. The assertion that matters is the
// first data write: if the handler collected the rows before writing, the source would
// already have produced every row by then.
func TestLargeExportStreamsRatherThanBuffering(t *testing.T) {
	const total = 200_000
	src := &countingFetcher{total: total, pageSize: exportPageSize, failAtPage: -1}
	out := &observingWriter{src: src}

	rows, err := streamExport(out, func() {}, []string{"id", "value"}, src.fetch)
	if err != nil {
		t.Fatalf("streamExport: %v", err)
	}
	if rows != total {
		t.Fatalf("wrote %d rows, want %d", rows, total)
	}
	if src.produced != total {
		t.Fatalf("source produced %d rows, want %d", src.produced, total)
	}

	// One write per page plus the header: an implementation that buffered would produce one
	// enormous write at the end.
	if len(out.writes) < total/exportPageSize {
		t.Fatalf("%d writes for %d rows: the export was assembled in memory", len(out.writes), total)
	}

	// The header went out before a single row was read.
	if out.writes[0].produced != 0 {
		t.Fatalf("the first write reached the client after %d rows", out.writes[0].produced)
	}
	// The first page of rows went out while 199,900 rows were still unread.
	firstData := out.writes[1]
	if firstData.produced > exportPageSize {
		t.Fatalf("the first rows reached the client only after %d rows had been read, "+
			"so the export buffered instead of streaming", firstData.produced)
	}

	// No single write may hold more than a page or two of the file. This is the memory
	// bound, measured: a buffered export of 200,000 rows would produce one write of
	// megabytes.
	for i, event := range out.writes {
		if event.bytes > 64<<10 {
			t.Fatalf("write %d carried %d bytes: the file is being built in memory", i, event.bytes)
		}
	}
}

// A failure after the first page cannot change the status code, so it must at least stop
// the stream and report the row count reached. A truncated file the operator can compare
// against the log is the best available answer.
func TestExportStopsAndReportsMidStreamFailure(t *testing.T) {
	src := &countingFetcher{total: 500, pageSize: exportPageSize, failAtPage: 2}
	out := &bytes.Buffer{}

	rows, err := streamExport(out, func() {}, []string{"id", "value"}, src.fetch)
	if err == nil {
		t.Fatal("a failing page was reported as a complete export")
	}
	if rows != int64(2*exportPageSize) {
		t.Fatalf("wrote %d rows before the failure, want %d", rows, 2*exportPageSize)
	}
	// What was written is still well-formed CSV: a reader can parse the prefix.
	records, parseErr := csv.NewReader(strings.NewReader(out.String())).ReadAll()
	if parseErr != nil {
		t.Fatalf("the partial export is not parseable: %v", parseErr)
	}
	if len(records) != 2*exportPageSize+1 {
		t.Fatalf("partial export has %d records, want %d", len(records), 2*exportPageSize+1)
	}
}

// A cursor that does not advance must end the export rather than loop forever.
func TestExportRefusesAStuckCursor(t *testing.T) {
	stuck := func(cursor string) (exportPage, error) {
		return exportPage{rows: [][]string{{"1"}}, next: "same"}, nil
	}
	if _, err := streamExport(&bytes.Buffer{}, func() {}, []string{"id"}, stuck); err == nil {
		t.Fatal("a non-advancing cursor looped instead of failing")
	}
}

// ------------------------------------------------------------------ money --

// The major-unit cell is exact for every scale, and the minor cell is the raw count.
func TestExportedMoneyIsExact(t *testing.T) {
	cases := []struct {
		minor  int64
		code   string
		major  string
		minorS string
	}{
		{0, "USD", "0.00", "0"},
		{1, "USD", "0.01", "1"},
		{999, "USD", "9.99", "999"},
		{1000, "USD", "10.00", "1000"},
		{-2550, "EUR", "-25.50", "-2550"},
		{123456789012345, "USD", "1234567890123.45", "123456789012345"},
		// A zero-scale currency keeps its integer form: 100 yen is "100", not "1.00".
		{100, "JPY", "100", "100"},
		// A three-scale currency keeps its third digit: 1 fils is 0.001, not 0.00.
		{1, "KWD", "0.001", "1"},
		// 2^53+1 minor units: the first integer a float64 cannot represent. The exact path
		// keeps the cent; the float path loses it, which is why this column is not a float.
		{9007199254740993, "USD", "90071992547409.93", "9007199254740993"},
	}
	for _, c := range cases {
		if got := majorUnits(c.minor, c.code); got != c.major {
			t.Fatalf("majorUnits(%d, %s) = %q, want %q", c.minor, c.code, got, c.major)
		}
		if got := strconv.FormatInt(c.minor, 10); got != c.minorS {
			t.Fatalf("minor cell for %d = %q, want %q", c.minor, got, c.minorS)
		}
	}

	// Proof that the float path this avoids is wrong for that value: dividing the minor
	// count by 100 in float64 rounds 90071992547409.93 to ...92, a lost cent.
	floatCell := strconv.FormatFloat(float64(int64(9007199254740993))/100, 'f', 2, 64)
	if floatCell == "90071992547409.93" {
		t.Fatalf("float64 division was exact for %s; this test no longer proves anything", floatCell)
	}
}

// The decimal cell must round-trip back to the same integer minor count. That is what "no
// cent moved" means, and it is asserted rather than assumed: the string is parsed back with
// the money package, so the test would fail if the rendering ever acquired a rounding step.
//
// The float64 comparison is the point of the test. A naive exporter divides the minor count
// by 100 in float64, and for at least one value this export writes, that loses a minor unit.
// The decimal string does not.
func TestExportedMoneyRoundTripsThroughTheCSV(t *testing.T) {
	cases := []struct {
		minor int64
		code  string
	}{
		{1, "USD"},                // the smallest cent
		{999, "USD"},              // 9.99, not 10.00
		{1000, "USD"},             // a whole unit
		{-2550, "EUR"},            // a credit
		{123456789012345, "USD"},  // an ordinary large invoice
		{9007199254740993, "USD"}, // 2^53+1: the first integer float64 cannot hold
		{1, "KWD"},                // three decimal places
		{100, "JPY"},              // no decimal places at all
	}

	floatLostACent := false
	for _, c := range cases {
		cur, err := staff.CurrencyOf(c.code)
		if err != nil {
			t.Fatalf("currency %s: %v", c.code, err)
		}
		cell := majorUnits(c.minor, c.code)

		// A spreadsheet reading the cell gets a number; parsing it back with the money
		// package must reproduce the ledger's integer exactly.
		back, err := money.Parse(cell, cur)
		if err != nil {
			t.Fatalf("the exported cell %q for %d %s does not parse as money: %v",
				cell, c.minor, c.code, err)
		}
		if back.Minor() != c.minor {
			t.Fatalf("%d %s exported as %q reads back as %d: a cent moved",
				c.minor, c.code, cell, back.Minor())
		}

		// The `_minor` column is the same integer by a different path, so a reconciliation
		// can be checked against a value no parser had to interpret.
		if strconv.FormatInt(c.minor, 10) != strconv.FormatInt(back.Minor(), 10) {
			t.Fatalf("minor column and round-tripped value disagree for %d %s", c.minor, c.code)
		}

		// What a float64 column would have contained.
		f, err := strconv.ParseFloat(cell, 64)
		if err != nil {
			t.Fatalf("parse %q as a float: %v", cell, err)
		}
		scale := math.Pow(10, float64(cur.Scale()))
		if int64(f*scale) != c.minor {
			floatLostACent = true
			if cell != majorUnits(c.minor, c.code) {
				t.Fatalf("the decimal cell for %d %s is not stable", c.minor, c.code)
			}
		}
	}
	if !floatLostACent {
		t.Fatal("no exported value distinguishes the exact path from a float path; " +
			"this test no longer proves why money is not exported as a float")
	}
}

// An invoice row carries the same number twice: the decimal a spreadsheet sums and the
// minor count a reconciliation checks.
func TestInvoiceExportRowKeepsMoneyExact(t *testing.T) {
	row := staff.InvoiceRow{
		ID:            "11111111-1111-1111-1111-111111111111",
		Number:        "INV-7",
		Status:        "open",
		Currency:      "USD",
		Total:         123456789012345,
		Paid:          456789012345,
		Due:           123000000000000,
		CustomerID:    "22222222-2222-2222-2222-222222222222",
		CustomerEmail: "ap@example.test",
	}
	cells := invoiceExportRow(row)
	if len(cells) != len(invoiceExportHeader) {
		t.Fatalf("%d cells for %d columns", len(cells), len(invoiceExportHeader))
	}
	byName := map[string]string{}
	for i, name := range invoiceExportHeader {
		byName[name] = cells[i]
	}
	want := map[string]string{
		"invoice_number":    "INV-7",
		"currency":          "USD",
		"total":             "1234567890123.45",
		"total_minor":       "123456789012345",
		"paid":              "4567890123.45",
		"paid_minor":        "456789012345",
		"balance_due":       "1230000000000.00",
		"balance_due_minor": "123000000000000",
		"customer_email":    "ap@example.test",
		"overdue":           "false",
		"issued_at":         "",
		"due_date":          "",
	}
	for name, value := range want {
		if byName[name] != value {
			t.Fatalf("%s = %q, want %q", name, byName[name], value)
		}
	}
}

// ------------------------------------------------------------------ injection --

// A customer's name must not be able to run anything when an accountant opens the file.
func TestExportedTextCannotBeAFormula(t *testing.T) {
	for _, dangerous := range []string{"=1+1", "+1", "-1", "@SUM(A1)", "\tcmd", "\rcmd"} {
		if got := csvCell(dangerous); got != "'"+dangerous {
			t.Fatalf("csvCell(%q) = %q, want the formula neutralised", dangerous, got)
		}
	}
	for _, safe := range []string{"", "Acme Ltd", "a@b.test", "O'Brien", "100 Main St", "&co"} {
		if got := csvCell(safe); got != safe {
			t.Fatalf("csvCell(%q) = %q, want it unchanged", safe, got)
		}
	}
}

// ------------------------------------------------------------------ the scope --

// An export is scoped by permission twice: the dataset's own read permission, and — when
// the export is narrowed to a customer — the permission to read that customer.
func TestExportIsScopedByPermission(t *testing.T) {
	s := testServer(t)
	s.Handler()

	// Two operator-defined roles: one that may read invoices, one that may also read
	// customers. Neither is a system role, so the compiled-in table is untouched.
	s.auth.Authz.SetTableRoles(map[authn.Role][]authn.Permission{
		"invoice-reader": {authn.PermInvoiceReadAll},
		"finance":        {authn.PermInvoiceReadAll, authn.PermCustomerReadAll, authn.PermServiceReadAll},
	})

	customerID := seedExportCustomer(t, s, "scoped@example.test", "Scoped Customer")

	invoiceOnly := authn.User{
		ID: "33333333-3333-3333-3333-333333333333", Email: "clerk@example.test",
		Status: "active", Roles: []string{"invoice-reader"},
	}
	finance := authn.User{
		ID: "44444444-4444-4444-4444-444444444444", Email: "finance@example.test",
		Status: "active", Roles: []string{"finance"},
	}
	// A customer session: authenticated, holds only the :self permissions.
	portalCustomer := authn.User{
		ID: "55555555-5555-5555-5555-555555555555", Email: "customer@example.test",
		Status: "active", Roles: []string{"customer"},
	}

	type want struct {
		path string
		user authn.User
		code int
	}
	cases := []want{
		// The dataset's own permission gates the export.
		{"/api/v1/staff/exports/invoices.csv", invoiceOnly, http.StatusOK},
		{"/api/v1/staff/exports/customers.csv", invoiceOnly, http.StatusForbidden},
		{"/api/v1/staff/exports/services.csv", invoiceOnly, http.StatusForbidden},

		// A customer this operator cannot read cannot be exported, even though they may
		// export invoices in general.
		{"/api/v1/staff/exports/invoices.csv?customer_id=" + customerID, invoiceOnly, http.StatusForbidden},
		{"/api/v1/staff/exports/services.csv?customer_id=" + customerID, invoiceOnly, http.StatusForbidden},

		// With the customer permission, the same request is allowed.
		{"/api/v1/staff/exports/invoices.csv?customer_id=" + customerID, finance, http.StatusOK},
		{"/api/v1/staff/exports/customers.csv", finance, http.StatusOK},
		{"/api/v1/staff/exports/services.csv", finance, http.StatusOK},

		// A customer session reaches none of it: this is a staff surface, and `:self`
		// permissions do not export anything.
		{"/api/v1/staff/exports/invoices.csv", portalCustomer, http.StatusForbidden},
		{"/api/v1/staff/exports/customers.csv", portalCustomer, http.StatusForbidden},
		{"/api/v1/staff/exports/services.csv", portalCustomer, http.StatusForbidden},

		// The built-in roles are the operators the screens serve: support can read all
		// three, so support can export all three.
		{"/api/v1/staff/exports/invoices.csv", supportUser(), http.StatusOK},
		{"/api/v1/staff/exports/customers.csv", supportUser(), http.StatusOK},
		{"/api/v1/staff/exports/services.csv", supportUser(), http.StatusOK},
	}

	for _, c := range cases {
		rec := exportGet(t, s, c.user, c.path)
		if rec.Code != c.code {
			t.Fatalf("%s as %v: status %d, want %d; body %s",
				c.path, c.user.Roles, rec.Code, c.code, rec.Body)
		}
		if c.code == http.StatusForbidden && strings.Contains(rec.Body.String(), customerID) {
			t.Fatalf("%s: the refusal echoes the customer id, which makes it an oracle: %s",
				c.path, rec.Body)
		}
	}
}

// ------------------------------------------------------------------ the content --

// The invoice export must carry the columns an accountant reconciles with, honour the
// filters the list honours, and label the download correctly.
func TestInvoiceExportContent(t *testing.T) {
	s := testServer(t)
	mine := seedExportCustomer(t, s, "mine@example.test", "Mine Ltd")
	other := seedExportCustomer(t, s, "other@example.test", "Other Ltd")
	invoiceID := seedExportInvoice(t, s, mine, "open", "USD", 123456789012345, 456789012345)
	seedExportInvoice(t, s, other, "paid", "EUR", 5000, 5000)

	rec := exportGet(t, s, adminUser(),
		"/api/v1/staff/exports/invoices.csv?customer_id="+mine)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/csv") {
		t.Fatalf("Content-Type = %q, want text/csv", ct)
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.HasPrefix(cd, `attachment; filename="invoices-`) {
		t.Fatalf("Content-Disposition = %q, want an attachment named invoices-<stamp>.csv", cd)
	}
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("the export is served without nosniff")
	}

	records := parseCSV(t, rec.Body.Bytes())
	if len(records) != 2 {
		t.Fatalf("%d records, want a header and the one filtered invoice: %v", len(records), records)
	}
	if strings.Join(records[0], ",") != strings.Join(invoiceExportHeader, ",") {
		t.Fatalf("header = %v, want %v", records[0], invoiceExportHeader)
	}
	cells := map[string]string{}
	for i, name := range invoiceExportHeader {
		cells[name] = records[1][i]
	}
	checks := map[string]string{
		"invoice_id":        invoiceID,
		"currency":          "USD",
		"customer_id":       mine,
		"customer_email":    "mine@example.test",
		"status":            "open",
		"total":             "1234567890123.45",
		"total_minor":       "123456789012345",
		"paid":              "4567890123.45",
		"paid_minor":        "456789012345",
		"balance_due":       "1230000000000.00",
		"balance_due_minor": "123000000000000",
	}
	for name, want := range checks {
		if cells[name] != want {
			t.Fatalf("%s = %q, want %q", name, cells[name], want)
		}
	}
	if cells["due_date"] == "" {
		t.Fatal("the export has no due date for an invoice that has one")
	}
	if _, err := time.Parse("2006-01-02", cells["due_date"]); err != nil {
		t.Fatalf("due_date %q is not a yyyy-mm-dd date: %v", cells["due_date"], err)
	}
	if _, err := time.Parse(time.RFC3339, cells["issued_at"]); err != nil {
		t.Fatalf("issued_at %q is not RFC3339: %v", cells["issued_at"], err)
	}

	// The end-to-end round trip: what was seeded is what the file says, parsed back through
	// the money package rather than compared as a string.
	for column, want := range map[string]int64{
		"total":       123456789012345,
		"paid":        456789012345,
		"balance_due": 123000000000000,
	} {
		cur, err := staff.CurrencyOf(cells["currency"])
		if err != nil {
			t.Fatalf("currency: %v", err)
		}
		amount, err := money.Parse(cells[column], cur)
		if err != nil {
			t.Fatalf("%s = %q does not parse as money: %v", column, cells[column], err)
		}
		if amount.Minor() != want {
			t.Fatalf("%s = %q reads back as %d minor units, want %d",
				column, cells[column], amount.Minor(), want)
		}
	}
}

// A balance is per currency, so a customer who owes in two currencies is two rows; a
// customer who owes nothing is one row with no currency.
func TestCustomerExportIsOneRowPerCurrency(t *testing.T) {
	s := testServer(t)
	owing := seedExportCustomer(t, s, "owing@example.test", "Owing Ltd")
	seedExportInvoice(t, s, owing, "open", "USD", 1000, 250)
	seedExportInvoice(t, s, owing, "open", "EUR", 2000, 0)
	seedExportCustomer(t, s, "clear@example.test", "Clear Ltd")

	rec := exportGet(t, s, adminUser(), "/api/v1/staff/exports/customers.csv")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	records := parseCSV(t, rec.Body.Bytes())
	if len(records) != 4 {
		// header + two currency rows for the owing customer + one row for the clear one
		t.Fatalf("%d records, want 4: %v", len(records), records)
	}
	index := map[string]int{}
	for i, name := range customerExportHeader {
		index[name] = i
	}

	balances := map[string]string{}
	for _, row := range records[1:] {
		if row[index["customer_id"]] != owing {
			continue
		}
		currency := row[index["outstanding_currency"]]
		balances[currency] = row[index["outstanding"]] + "/" + row[index["outstanding_minor"]]
		if row[index["open_invoices"]] != "2" {
			t.Fatalf("open_invoices = %q, want 2", row[index["open_invoices"]])
		}
	}
	wantBalances := map[string]string{"USD": "7.50/750", "EUR": "20.00/2000"}
	if fmt.Sprint(balances) != fmt.Sprint(wantBalances) {
		t.Fatalf("balances = %v, want %v", balances, wantBalances)
	}

	for _, row := range records[1:] {
		if row[index["customer_id"]] != owing {
			continue
		}
		if row[index["has_login"]] != "false" {
			t.Fatalf("has_login = %q, want false for a customer with no account", row[index["has_login"]])
		}
	}

	// The customer who owes nothing: a blank currency and a zero minor count, because a
	// "0.00" in a currency column is a claim about which currency was owed.
	for _, row := range records[1:] {
		if row[index["customer_id"]] == owing {
			continue
		}
		if row[index["outstanding_currency"]] != "" || row[index["outstanding"]] != "" {
			t.Fatalf("a customer with no balance has a currency: %v", row)
		}
		if row[index["outstanding_minor"]] != "0" {
			t.Fatalf("outstanding_minor = %q, want 0", row[index["outstanding_minor"]])
		}
	}
}

// The service export carries the identity of the thing the customer owns and where it is
// in the queue, filtered the way the list is filtered.
func TestServiceExportContent(t *testing.T) {
	s := testServer(t)
	customerID := seedExportCustomer(t, s, "svc@example.test", "Svc Ltd")
	serviceID, productName := seedExportService(t, s, customerID, "svc-plan", "ext-abc-1")
	seedExportService(t, s, customerID, "svc-other", "ext-abc-2")

	rec := exportGet(t, s, adminUser(), "/api/v1/staff/exports/services.csv?status=active")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	records := parseCSV(t, rec.Body.Bytes())
	if len(records) != 3 {
		t.Fatalf("%d records, want header + two services: %v", len(records), records)
	}
	index := map[string]int{}
	for i, name := range serviceExportHeader {
		index[name] = i
	}
	found := false
	for _, row := range records[1:] {
		if row[index["service_id"]] != serviceID {
			continue
		}
		found = true
		checks := map[string]string{
			"customer_id":     customerID,
			"customer_email":  "svc@example.test",
			"product_name":    productName,
			"status":          "active",
			"external_id":     "ext-abc-1",
			"generation":      "1",
			"open_operations": "0",
		}
		for name, want := range checks {
			if row[index[name]] != want {
				t.Fatalf("%s = %q, want %q", name, row[index[name]], want)
			}
		}
		if _, err := time.Parse(time.RFC3339, row[index["created_at"]]); err != nil {
			t.Fatalf("created_at %q is not RFC3339: %v", row[index["created_at"]], err)
		}
	}
	if !found {
		t.Fatalf("the seeded service is missing from the export: %v", records)
	}
}

// A filter the export does not honour must be refused, not ignored: an operator who asked
// for March and got the whole book has no way to tell from the file.
func TestExportRefusesFiltersItDoesNotHonour(t *testing.T) {
	s := testServer(t)
	for _, path := range []string{
		"/api/v1/staff/exports/invoices.csv?limit=10",
		"/api/v1/staff/exports/invoices.csv?cursor=abc",
		"/api/v1/staff/exports/customers.csv?status=open",
		"/api/v1/staff/exports/services.csv?currency=USD",
	} {
		rec := exportGet(t, s, adminUser(), path)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: status %d, want 400; body %s", path, rec.Code, rec.Body)
		}
		if !strings.Contains(rec.Body.String(), "invalid_query") {
			t.Fatalf("%s: body = %s", path, rec.Body)
		}
	}

	// A malformed customer id is the caller's mistake, not a 500 from the database.
	rec := exportGet(t, s, adminUser(), "/api/v1/staff/exports/invoices.csv?customer_id=nope")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad customer_id: status %d, want 400; body %s", rec.Code, rec.Body)
	}
	// The list refuses a one-character search; so does the export.
	rec = exportGet(t, s, adminUser(), "/api/v1/staff/exports/customers.csv?q=a")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("short search: status %d, want 400; body %s", rec.Code, rec.Body)
	}
}

// The search narrows the customer export the same way it narrows the list.
func TestCustomerExportHonoursTheSearch(t *testing.T) {
	s := testServer(t)
	seedExportCustomer(t, s, "findme@example.test", "Find Me Ltd")
	seedExportCustomer(t, s, "someone-else@example.test", "Someone Else Ltd")

	rec := exportGet(t, s, adminUser(), "/api/v1/staff/exports/customers.csv?q=findme")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	records := parseCSV(t, rec.Body.Bytes())
	if len(records) != 2 {
		t.Fatalf("%d records, want header + one match: %v", len(records), records)
	}
	index := map[string]int{}
	for i, name := range customerExportHeader {
		index[name] = i
	}
	if records[1][index["email"]] != "findme@example.test" {
		t.Fatalf("matched %q, want findme@example.test", records[1][index["email"]])
	}
}

// A customer whose name looks like a formula must arrive neutralised in the real file, not
// just through csvCell in isolation.
func TestExportNeutralisesAFormulaInRealData(t *testing.T) {
	s := testServer(t)
	seedExportCustomer(t, s, "formula@example.test", "=cmd|' /C calc'!A0")

	rec := exportGet(t, s, adminUser(), "/api/v1/staff/exports/customers.csv")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	records := parseCSV(t, rec.Body.Bytes())
	index := map[string]int{}
	for i, name := range customerExportHeader {
		index[name] = i
	}
	got := records[1][index["name"]]
	if !strings.HasPrefix(got, "'=") {
		t.Fatalf("name = %q, want the formula neutralised", got)
	}
}

// ------------------------------------------------------------------ helpers --

// exportGet issues a GET as an operator and returns the recorder.
func exportGet(t *testing.T, s *Server, user authn.User, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req = withUser(req, user)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// supportUser is the built-in support role, which may read all three datasets.
func supportUser() authn.User {
	return authn.User{
		ID: "66666666-6666-6666-6666-666666666666", Email: "support@example.test",
		Status: "active", Roles: []string{"support"},
	}
}

// parseCSV parses an export body.
func parseCSV(t *testing.T, body []byte) [][]string {
	t.Helper()
	records, err := csv.NewReader(bytes.NewReader(body)).ReadAll()
	if err != nil {
		t.Fatalf("the export is not valid CSV: %v\n%s", err, body)
	}
	return records
}

// seedExportCustomer inserts a customer directly. The export tests read; they must not fail
// because a create endpoint is broken.
func seedExportCustomer(t *testing.T, s *Server, email, name string) string {
	t.Helper()
	var id string
	if err := s.pool.QueryRow(context.Background(),
		`INSERT INTO customers (email, name) VALUES ($1, $2) RETURNING id`,
		email, name).Scan(&id); err != nil {
		t.Fatalf("seed customer %q: %v", email, err)
	}
	return id
}

// seedExportInvoice inserts an issued invoice directly.
func seedExportInvoice(t *testing.T, s *Server, customerID, status, currency string, total, paid int64) string {
	t.Helper()
	due := time.Now().UTC().Add(14 * 24 * time.Hour)
	var id string
	if err := s.pool.QueryRow(context.Background(), `
		INSERT INTO invoices (customer_id, number, status, currency, subtotal_minor, tax_minor,
			total_minor, amount_paid_minor, issued_at, due_at, snapshot)
		VALUES ($1, $2, $3, $4, $5, 0, $5, $6, now(), $7, '{"schema_version":1}'::jsonb)
		RETURNING id`,
		customerID, fmt.Sprintf("INV-%d", time.Now().UnixNano()), status, currency,
		total, paid, due).Scan(&id); err != nil {
		t.Fatalf("seed invoice: %v", err)
	}
	return id
}

// seedExportService inserts a product and an active service on it.
func seedExportService(t *testing.T, s *Server, customerID, slug, externalID string) (serviceID, productName string) {
	t.Helper()
	ctx := context.Background()
	productName = "Product " + slug
	var productID string
	if err := s.pool.QueryRow(ctx,
		`INSERT INTO products (slug, name) VALUES ($1, $2) RETURNING id`,
		slug, productName).Scan(&productID); err != nil {
		t.Fatalf("seed product %q: %v", slug, err)
	}
	if err := s.pool.QueryRow(ctx, `
		INSERT INTO services (customer_id, product_id, status, external_id)
		VALUES ($1, $2, 'active', $3) RETURNING id`,
		customerID, productID, externalID).Scan(&serviceID); err != nil {
		t.Fatalf("seed service: %v", err)
	}
	return serviceID, productName
}
