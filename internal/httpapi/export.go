// CSV export (OPS-12).
//
// An accountant cannot work from a browser: they need the book in a spreadsheet, and before
// this file the only way to get data out of the panel was to read the database. Three
// datasets are exportable — invoices, customers and services — because those are the three
// an AR reconciliation is built from.
//
// Five decisions, each of which is a way this could have been wrong:
//
//   - **The export is the screen, unpaginated.** Every export takes the same query
//     parameters as the list it sits on and hands them to the same `staff` query through
//     the same filter struct. There is no second SQL dialect to keep in step and no way for
//     the file to contain a different set of rows than the list the operator was looking at
//     when they clicked export. The one difference is that the export has no `limit` or
//     `cursor`: it is every matching row, in the order the list would have shown them.
//
//   - **It streams.** The rows are read one page at a time (`paginate.MaxLimit` rows per
//     query, keyset-paginated) and each page is written and flushed before the next query
//     runs. Memory is bounded by one page, not by the size of the table, so exporting a
//     million invoices is the same amount of memory as exporting ten. A test drives
//     streamExport with a source far larger than one page and asserts that bytes reach the
//     client while rows remain unread — a handler that collected the rows first could not
//     pass it.
//
//   - **Money is never a float.** Every amount is exported twice: once as an exact decimal
//     string in the currency's major unit (`"1234567890123.45"`, produced by integer
//     arithmetic over minor units, so a spreadsheet reads it as a number and no cent can
//     move) and once as the integer minor-unit count in a `_minor` column, which is the
//     number a reconciliation should be checked against. A single float column is how
//     9.999 becomes 10.00 and the books stop agreeing with the bank.
//
//   - **The export is scoped by permission, twice.** The route declares the dataset's
//     `:read:all` permission, so an operator who cannot read invoices cannot export them.
//     A `customer_id` filter is a second question — "may this operator read *that*
//     customer" — and it is checked against customer:read:all, so a caller who cannot open
//     a customer record cannot bulk-extract their documents, their services or their
//     balance. A customer-facing session holds only the `:self` permissions and reaches
//     none of this.
//
//   - **A text cell cannot become a formula.** A customer whose name is
//     `=cmd|' /C calc'!A0` must not be able to run anything when an accountant opens the
//     file, so every free-text cell is neutralised (see csvCell). Numbers, dates and the
//     panel's own enum values are written raw: prefixing them would corrupt the value
//     rather than protect anything.
//
// What this file deliberately does not do: it does not write an audit row per export. The
// read permission and the request log are the record; an export is a read, and a table that
// grows one row per download would be noise in the log an operator actually reads.
package httpapi

import (
	"encoding/csv"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/catalyst/billing/internal/authn"
	"github.com/catalyst/billing/internal/money"
	"github.com/catalyst/billing/internal/paginate"
	"github.com/catalyst/billing/internal/staff"
)

func init() {
	RegisterRoutes("55-exports", func(r *router) { r.s.registerExportRoutes(r) })
}

// exportPageSize is how many rows one query may materialise.
//
// It is paginate.MaxLimit rather than a number of its own so that a page of the export is
// exactly a full page of the list: the same query, the same index, the same bounded memory.
// The cost is one round trip per hundred rows, which is the price of not holding the table.
const exportPageSize = paginate.MaxLimit

// exportRoute is one exportable dataset.
type exportRoute struct {
	// dataset names the file and the log line: "invoices" produces invoices-<stamp>.csv.
	dataset string
	// header is the first CSV row.
	header []string
	// parse reads the request's filters and returns the page reader. It is a separate step
	// from the write so that a bad filter is a 400 *before* the first byte of the body: a
	// download that starts and then fails cannot report an error.
	parse func(w http.ResponseWriter, r *http.Request) (exportFetcher, bool)
}

// exportPage is one page of an export: rows already rendered as CSV cells, plus the cursor
// that asks for the next page (empty on the last one).
type exportPage struct {
	rows [][]string
	next string
}

// exportFetcher reads one page. The empty cursor asks for the first.
type exportFetcher func(cursor string) (exportPage, error)

func (s *Server) registerExportRoutes(r *router) {
	// staffExport, not staffGet: a full-table read has its own, tighter rate budget
	// (RateExport), so a client cannot turn an export into a load test.
	r.staffExport("/api/v1/staff/exports/invoices.csv", authn.PermInvoiceReadAll,
		s.handleExportInvoices)
	r.staffExport("/api/v1/staff/exports/customers.csv", authn.PermCustomerReadAll,
		s.handleExportCustomers)
	r.staffExport("/api/v1/staff/exports/services.csv", authn.PermServiceReadAll,
		s.handleExportServices)
}

// ---------------------------------------------------------------- the stream --

// streamExport writes the header and then every page, flushing after each one.
//
// It returns the number of data rows written, which is what the caller logs when a write
// fails part-way: the operator's only clue that the download they hold is short.
func streamExport(w io.Writer, flush func(), header []string, fetch exportFetcher) (int64, error) {
	cw := csv.NewWriter(w)
	if err := cw.Write(header); err != nil {
		return 0, err
	}
	// The header goes out on its own so a client sees the columns immediately, and so that
	// a first page that is slow to arrive is visibly slow rather than silent.
	cw.Flush()
	if err := cw.Error(); err != nil {
		return 0, err
	}
	flush()

	var rows int64
	cursor := ""
	for {
		page, err := fetch(cursor)
		if err != nil {
			return rows, err
		}
		for _, cells := range page.rows {
			if err := cw.Write(cells); err != nil {
				return rows, err
			}
			rows++
		}
		cw.Flush()
		if err := cw.Error(); err != nil {
			return rows, err
		}
		flush()

		if page.next == "" {
			return rows, nil
		}
		if page.next == cursor {
			// A cursor that does not advance would export forever. The staff queries always
			// encode the last row's key, so this is unreachable — but an endless download is
			// a worse failure than a short one, so it is checked rather than assumed.
			return rows, fmt.Errorf("export: the cursor did not advance")
		}
		cursor = page.next
	}
}

// serveExport is the shared handler: parse, then stream, then report.
func (s *Server) serveExport(w http.ResponseWriter, r *http.Request, route exportRoute) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "storage_unavailable",
			"the export needs a database")
		return
	}

	// Filters are validated before anything is written, so every client mistake is a
	// problem+json with a status code instead of a truncated file.
	fetch, ok := route.parse(w, r)
	if !ok {
		return
	}

	flusher, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Disposition",
		`attachment; filename="`+exportFilename(route.dataset)+`"`)
	w.WriteHeader(http.StatusOK)

	flush := func() {
		if flusher != nil {
			flusher.Flush()
		}
	}
	rows, err := streamExport(w, flush, route.header, fetch)
	if err != nil {
		// The status line and probably a page of rows are already on the wire, so the only
		// honest thing left is the log. The row count is there because "the file has 3,400
		// rows and should have 12,000" is the sentence the operator needs.
		s.cfg.Logger.ErrorContext(r.Context(), "csv export failed after the response started",
			"dataset", route.dataset, "rows_written", rows, "error", err)
		return
	}
}

// exportFilename stamps the download so two exports of the same dataset do not overwrite
// each other in the accountant's downloads folder.
func exportFilename(dataset string) string {
	return dataset + "-" + time.Now().UTC().Format("20060102T150405Z") + ".csv"
}

// ------------------------------------------------------------------ invoices --

var invoiceExportHeader = []string{
	"invoice_id", "invoice_number", "status", "currency",
	"issued_at", "due_at", "due_date",
	"customer_id", "customer_email",
	"total", "total_minor", "paid", "paid_minor", "balance_due", "balance_due_minor",
	"overdue",
}

func (s *Server) handleExportInvoices(w http.ResponseWriter, r *http.Request) {
	s.serveExport(w, r, exportRoute{
		dataset: "invoices",
		header:  invoiceExportHeader,
		parse:   s.parseInvoiceExport,
	})
}

func (s *Server) parseInvoiceExport(w http.ResponseWriter, r *http.Request) (exportFetcher, bool) {
	if !rejectUnknownParams(w, r, "status", "customer_id", "currency", "from", "to", "overdue") {
		return nil, false
	}
	from, ok := s.timeParam(w, r, "from")
	if !ok {
		return nil, false
	}
	to, ok := s.timeParam(w, r, "to")
	if !ok {
		return nil, false
	}
	overdue, ok := s.boolParam(w, r, "overdue")
	if !ok {
		return nil, false
	}

	q := r.URL.Query()
	filter := staff.InvoiceFilter{
		Status:     strings.TrimSpace(q.Get("status")),
		CustomerID: strings.TrimSpace(q.Get("customer_id")),
		Currency:   strings.ToUpper(strings.TrimSpace(q.Get("currency"))),
		From:       from,
		To:         to,
	}
	// A false `overdue` means "do not narrow", exactly as it does on the list: the two must
	// answer the same question or a spot check of the file against the screen fails.
	if overdue != nil {
		filter.Overdue = *overdue
	}
	if !s.exportCustomerScope(w, r, filter.CustomerID) {
		return nil, false
	}

	return func(cursor string) (exportPage, error) {
		rows, next, err := s.staffService().Invoices(r.Context(), filter, exportPageSize, cursor)
		if err != nil {
			return exportPage{}, err
		}
		page := exportPage{next: next, rows: make([][]string, 0, len(rows))}
		for _, row := range rows {
			page.rows = append(page.rows, invoiceExportRow(row))
		}
		return page, nil
	}, true
}

// invoiceExportRow renders one invoice as cells.
//
// The columns are the list's columns: what the document is, whose it is, what it is worth,
// what has been paid and what is still owed. Subtotal and tax are deliberately absent —
// they are on the invoice document itself (and in its PDF), and re-deriving them here would
// be a second implementation of a split the ledger already fixed.
func invoiceExportRow(row staff.InvoiceRow) []string {
	return []string{
		row.ID,
		csvCell(row.Number),
		row.Status,
		row.Currency,
		instantCell(row.IssuedAt),
		instantCell(row.DueAt),
		dateCell(row.DueAt),
		row.CustomerID,
		csvCell(row.CustomerEmail),
		majorUnits(row.Total, row.Currency),
		strconv.FormatInt(row.Total, 10),
		majorUnits(row.Paid, row.Currency),
		strconv.FormatInt(row.Paid, 10),
		majorUnits(row.Due, row.Currency),
		strconv.FormatInt(row.Due, 10),
		strconv.FormatBool(row.Overdue),
	}
}

// ----------------------------------------------------------------- customers --

var customerExportHeader = []string{
	"customer_id", "email", "name", "created_at",
	"open_invoices", "services", "has_login",
	"outstanding_currency", "outstanding", "outstanding_minor",
}

func (s *Server) handleExportCustomers(w http.ResponseWriter, r *http.Request) {
	s.serveExport(w, r, exportRoute{
		dataset: "customers",
		header:  customerExportHeader,
		parse:   s.parseCustomerExport,
	})
}

func (s *Server) parseCustomerExport(w http.ResponseWriter, r *http.Request) (exportFetcher, bool) {
	if !rejectUnknownParams(w, r, "q") {
		return nil, false
	}
	q := r.URL.Query()
	filter := staff.CustomerFilter{Query: strings.TrimSpace(q.Get("q"))}
	// The list refuses a one-character search rather than scanning; the export refuses it
	// for the same reason, and the message is the staff query's own.
	if filter.Query != "" && len([]rune(filter.Query)) < 2 {
		writeProblem(w, http.StatusBadRequest, "invalid_query",
			"the search needs at least two characters")
		return nil, false
	}

	return func(cursor string) (exportPage, error) {
		rows, next, err := s.staffService().Customers(r.Context(), filter, exportPageSize, cursor)
		if err != nil {
			return exportPage{}, err
		}
		page := exportPage{next: next}
		for _, row := range rows {
			// One row per currency the customer owes, because a balance is not one number:
			// adding a USD balance to a EUR one is arithmetic on unrelated quantities. A
			// customer who owes nothing is one row with no currency.
			if len(row.Outstanding) == 0 {
				page.rows = append(page.rows, customerExportRow(row, nil))
				continue
			}
			for i := range row.Outstanding {
				page.rows = append(page.rows, customerExportRow(row, &row.Outstanding[i]))
			}
		}
		return page, nil
	}, true
}

// customerExportRow renders one customer, with one currency's balance when amount is not nil.
func customerExportRow(row staff.CustomerRow, amount *staff.Amount) []string {
	out := []string{
		row.ID,
		csvCell(row.Email),
		csvCell(row.Name),
		instantCell(&row.CreatedAt),
		strconv.Itoa(row.OpenInvoices),
		strconv.Itoa(row.Services),
		strconv.FormatBool(row.HasLogin),
	}
	if amount == nil {
		// No currency to name: the amount cell is blank rather than "0.00", because a zero
		// with a currency in it is a claim about which currency was owed.
		return append(out, "", "", "0")
	}
	code := amount.Currency.Code()
	return append(out, code, majorUnits(amount.Minor, code), strconv.FormatInt(amount.Minor, 10))
}

// ------------------------------------------------------------------ services --

var serviceExportHeader = []string{
	"service_id", "customer_id", "customer_email",
	"product_id", "product_name", "status", "external_id",
	"generation", "created_at", "next_bill_at", "open_operations", "last_error",
}

func (s *Server) handleExportServices(w http.ResponseWriter, r *http.Request) {
	s.serveExport(w, r, exportRoute{
		dataset: "services",
		header:  serviceExportHeader,
		parse:   s.parseServiceExport,
	})
}

func (s *Server) parseServiceExport(w http.ResponseWriter, r *http.Request) (exportFetcher, bool) {
	if !rejectUnknownParams(w, r, "status", "customer_id", "product_id", "has_open_operation") {
		return nil, false
	}
	open, ok := s.boolParam(w, r, "has_open_operation")
	if !ok {
		return nil, false
	}
	q := r.URL.Query()
	filter := staff.ServiceFilter{
		Status:     strings.TrimSpace(q.Get("status")),
		CustomerID: strings.TrimSpace(q.Get("customer_id")),
		ProductID:  strings.TrimSpace(q.Get("product_id")),
	}
	if open != nil {
		filter.OnlyOpenOperations = *open
	}
	if !s.exportCustomerScope(w, r, filter.CustomerID) {
		return nil, false
	}

	return func(cursor string) (exportPage, error) {
		rows, next, err := s.staffService().Services(r.Context(), filter, exportPageSize, cursor)
		if err != nil {
			return exportPage{}, err
		}
		page := exportPage{next: next, rows: make([][]string, 0, len(rows))}
		for _, row := range rows {
			page.rows = append(page.rows, serviceExportRow(row))
		}
		return page, nil
	}, true
}

// serviceExportRow renders one service.
//
// There is no money column here on purpose: a service is what the customer owns, and what it
// costs is on the invoice export and on the invoice itself. The recurring price lives on a
// price version the service's subscription points at, and joining that per row would make
// this the one export whose rows are not the list's rows.
func serviceExportRow(row staff.ServiceRow) []string {
	return []string{
		row.ID,
		row.CustomerID,
		csvCell(row.CustomerMail),
		row.ProductID,
		csvCell(row.Product),
		row.Status,
		csvCell(row.ExternalID),
		strconv.FormatInt(row.Generation, 10),
		instantCell(&row.CreatedAt),
		instantCell(row.NextBillAt),
		strconv.Itoa(row.OpenOps),
		csvCell(row.LastError),
	}
}

// ------------------------------------------------------------------ the scope --

// exportCustomerScope refuses an export narrowed to a customer the caller cannot read.
//
// The route's declared permission answers "may this operator export invoices at all"; this
// answers the second question: may they export *this customer's* rows. A caller who cannot
// read the customer record cannot bulk-extract their documents, their services or their
// balance, and `customer_id` is the only way to name one — so it is the only place the check
// has to happen.
func (s *Server) exportCustomerScope(w http.ResponseWriter, r *http.Request, customerID string) bool {
	if customerID == "" {
		return true
	}
	if !validUUID(customerID) {
		writeProblem(w, http.StatusBadRequest, "invalid_query", "customer_id must be a uuid")
		return false
	}
	if !s.callerHolds(r, authn.PermCustomerReadAll) {
		// The refusal does not distinguish "no such customer" from "not yours to read", so
		// it cannot be used to enumerate customer ids.
		writeProblem(w, http.StatusForbidden, "forbidden",
			"you cannot export a customer you cannot read")
		return false
	}
	return true
}

// callerHolds reports whether the request's principal holds a permission.
//
// It applies the same two checks requirePermission applies to a route's declared
// permission: the user's roles, and the API key's own scopes, which may only narrow. A check
// performed inside a handler that skipped the key narrowing would be a hole a key could walk
// through.
func (s *Server) callerHolds(r *http.Request, p authn.Permission) bool {
	user, ok := userFrom(r.Context())
	if !ok {
		return false
	}
	if !s.userHolds(r, user, p) {
		return false
	}
	if key, ok := apiKeyFrom(r.Context()); ok && !key.Allows(string(p)) {
		return false
	}
	return true
}

// rejectUnknownParams refuses a query parameter the export does not honour.
//
// A filter that is silently ignored is worse in an export than in a list: the operator asked
// for March, got the whole book, and the file does not say so. `limit` and `cursor` are
// refused with everything else — an export is not a page.
func rejectUnknownParams(w http.ResponseWriter, r *http.Request, allowed ...string) bool {
	known := make(map[string]struct{}, len(allowed))
	for _, name := range allowed {
		known[name] = struct{}{}
	}
	for name := range r.URL.Query() {
		if _, ok := known[name]; !ok {
			writeProblem(w, http.StatusBadRequest, "invalid_query",
				"the export does not filter on "+name)
			return false
		}
	}
	return true
}

// -------------------------------------------------------------- cell rendering --

// majorUnits renders minor units as an exact decimal string in the currency's major unit.
//
// It is money.Amount.String, which divides integer minor units by the currency's scale in
// decimal arithmetic. No float appears anywhere on the path, so the value in the file is the
// value in the ledger, digit for digit, and a spreadsheet reads it as a number.
func majorUnits(minor int64, code string) string {
	cur, err := staff.CurrencyOf(code)
	if err != nil {
		// The schema's CHECK constraint makes this unreachable. The raw minor count is the
		// honest fallback: guessing a scale would change the value of money.
		return strconv.FormatInt(minor, 10)
	}
	return money.New(minor, cur).String()
}

// csvCell neutralises a free-text field a spreadsheet would treat as a formula.
//
// A customer whose name is `=cmd|' /C calc'!A0` must not be able to run anything when an
// accountant opens the export; prefixing an apostrophe is the standard mitigation, and the
// spreadsheet then shows the literal text. Only free text goes through this: an integer, an
// RFC3339 timestamp or one of the panel's own enum values cannot begin with a formula
// character, and prefixing them would corrupt the value instead of protecting anything.
func csvCell(s string) string {
	if s == "" {
		return ""
	}
	switch s[0] {
	case '=', '+', '-', '@', '\t', '\r':
		return "'" + s
	}
	return s
}

// instantCell renders a timestamp as RFC3339 in UTC, or empty for "not set".
//
// UTC and not the server's local zone: the file is read next to a bank statement and a
// timestamp without a zone is a timestamp that means something different in each office.
func instantCell(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// dateCell renders the date part, which is what a pivot table groups by.
func dateCell(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.UTC().Format("2006-01-02")
}
