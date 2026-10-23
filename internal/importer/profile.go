package importer

import (
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// Source profiles.
//
// A "WHMCS-style dump" is five CSV documents whose columns are named after the old
// installation's tables (tblclients.userid, tblinvoices.invoicenum, tblhosting.domainstatus,
// tblaccounts.transid). A Paymenter dump carries the same five entities under its own names
// (users.email, invoices.number, services.plan, transactions.transaction_id).
//
// The difference between the two sources is therefore *data*, not code: a profile is a list
// of canonical field names with the column aliases that may carry each one. The importer
// reads one dialect at a time and names it in the report, so a report always says which
// shape of dump produced it. Unknown columns are ignored rather than refused: an export
// carries dozens of columns nobody needs, and refusing a dump because it also contains
// `tblclients.notes` would make the importer unusable.
//
// The canonical name is always accepted as its own alias, so a hand-written CSV using this
// package's vocabulary works with either source.

// field is one column the importer needs, and the names it may appear under.
type field struct {
	// key is the canonical name. It appears in rejection messages, so it is the name a
	// person can search the dump for.
	key string
	// aliases are the column names that may carry it, in preference order.
	aliases []string
	// required means the document cannot be read without the column. A missing optional
	// column is not an error: the row simply has an empty value and the rules for that
	// entity decide whether that is acceptable.
	required bool
}

// profile is one dump dialect.
type profile struct {
	source string
	// columns is the field list per document.
	columns map[string][]field
}

// lookup returns the aliases of one canonical field.
func (p *profile) lookup(file, key string) []string {
	for _, f := range p.columns[file] {
		if f.key == key {
			return f.aliases
		}
	}
	return []string{key}
}

// profileFor resolves a source name.
func profileFor(source string) (*profile, error) {
	switch source {
	case "whmcs":
		return whmcsProfile(), nil
	case "paymenter":
		return paymenterProfile(), nil
	}
	return nil, fmt.Errorf("%w: %q (known sources: paymenter, whmcs)", ErrUnknownSource, source)
}

// Sources lists the dump dialects this build reads.
func Sources() []string { return []string{"paymenter", "whmcs"} }

// checkFiles verifies the request carries exactly the documents the source declares.
//
// Every document is required even when the installation had no rows of that kind: an empty
// file with a header says "there were no services", and omitting the file says "I forgot to
// export services". The importer cannot tell those apart from the data, so it insists on
// being told.
func (p *profile) checkFiles(files map[string]string) error {
	known := make(map[string]struct{}, len(Files)+len(OptionalFiles))
	for _, name := range Files {
		known[name] = struct{}{}
	}
	// The optional documents are named but not required: a dump that carries tickets must not be
	// refused for omitting currencies, and one that carries neither is imported as far as it goes
	// (the report says which documents it did not see).
	for _, name := range OptionalFiles {
		known[name] = struct{}{}
	}
	for _, name := range Files {
		if _, ok := files[name]; !ok {
			return fmt.Errorf("%w: %q is required (a header-only file is fine)", ErrMissingFile, name)
		}
	}
	for name := range files {
		if _, ok := known[name]; !ok {
			return fmt.Errorf("%w: %q is not one of the documents this importer reads (%s, %s)",
				ErrInvalidInput, name, strings.Join(Files, ", "), strings.Join(OptionalFiles, ", "))
		}
	}
	return nil
}

// ---------------------------------------------------------------- dialects --

func whmcsProfile() *profile {
	return &profile{
		source: "whmcs",
		columns: map[string][]field{
			FileClients: {
				{key: "id", aliases: []string{"userid", "clientid"}, required: true},
				{key: "email", aliases: []string{"email"}, required: true},
				{key: "first_name", aliases: []string{"firstname"}},
				{key: "last_name", aliases: []string{"lastname"}},
				{key: "company", aliases: []string{"companyname"}},
				{key: "created_at", aliases: []string{"datecreated", "registrationdate"}},
				{key: "currency", aliases: []string{"currency", "currency_code"}},
			},
			FileInvoices: {
				{key: "id", aliases: []string{"invoiceid"}, required: true},
				{key: "client_id", aliases: []string{"userid"}, required: true},
				{key: "number", aliases: []string{"invoicenum"}, required: true},
				{key: "status", aliases: []string{"status"}, required: true},
				{key: "total", aliases: []string{"total"}, required: true},
				{key: "subtotal", aliases: []string{"subtotal"}},
				{key: "tax", aliases: []string{"tax"}},
				{key: "amount_paid", aliases: []string{"amountpaid"}},
				{key: "currency", aliases: []string{"currency", "currency_code"}},
				{key: "issued_at", aliases: []string{"date", "datecreated"}},
				{key: "due_at", aliases: []string{"duedate"}},
				{key: "paid_at", aliases: []string{"datepaid"}},
			},
			FileInvoiceItems: {
				{key: "id", aliases: []string{"id", "itemid"}, required: true},
				{key: "invoice_id", aliases: []string{"invoiceid"}, required: true},
				{key: "description", aliases: []string{"description"}, required: true},
				{key: "amount", aliases: []string{"amount", "net"}, required: true},
				{key: "taxed", aliases: []string{"taxed"}},
			},
			FileServices: {
				{key: "id", aliases: []string{"id", "hostingid", "serviceid"}, required: true},
				{key: "client_id", aliases: []string{"userid"}, required: true},
				{key: "product", aliases: []string{"product", "package", "product_slug"}, required: true},
				{key: "status", aliases: []string{"domainstatus", "status"}, required: true},
				{key: "external_id", aliases: []string{"domain", "servername", "username"}},
				{key: "billing_cycle", aliases: []string{"billingcycle"}},
				{key: "created_at", aliases: []string{"regdate"}},
				{key: "next_due_at", aliases: []string{"nextduedate"}},
			},
			FileCurrencies: {
				{key: "code", aliases: []string{"code", "currency_code"}, required: true},
				{key: "name", aliases: []string{"name", "currency_name"}},
				{key: "symbol", aliases: []string{"symbol", "prefix"}},
				{key: "symbol_position", aliases: []string{"symbol_position", "format"}},
				// WHMCS stores the number of decimal places; this panel calls it the scale.
				{key: "scale", aliases: []string{"scale", "decimal_places", "decimals"}, required: true},
				{key: "enabled", aliases: []string{"enabled", "active"}},
			},
			FileAdmins: {
				{key: "id", aliases: []string{"id", "adminid"}, required: false},
				{key: "email", aliases: []string{"email", "emailaddress"}, required: true},
				{key: "first_name", aliases: []string{"first_name", "firstname"}},
				{key: "last_name", aliases: []string{"last_name", "lastname"}},
				{key: "name", aliases: []string{"name", "full_name"}},
				{key: "role", aliases: []string{"role", "role_name", "staff_role"}, required: true},
			},
			FileProducts: {
				{key: "id", aliases: []string{"id", "productid", "pid"}},
				{key: "name", aliases: []string{"name", "product_name"}, required: true},
				{key: "slug", aliases: []string{"slug", "product_slug"}},
				{key: "price", aliases: []string{"price", "amount"}},
				{key: "currency", aliases: []string{"currency", "currency_code"}},
				{key: "interval", aliases: []string{"interval", "billing_cycle", "billingcycle"}},
			},
			FileTickets: {
				{key: "id", aliases: []string{"id", "ticketid"}, required: true},
				{key: "number", aliases: []string{"number", "ticket_number", "tid"}},
				{key: "client_id", aliases: []string{"client_id", "userid", "user_id"}, required: true},
				{key: "subject", aliases: []string{"subject", "title"}, required: true},
				{key: "department", aliases: []string{"department", "dept"}},
				{key: "priority", aliases: []string{"priority", "urgency"}},
				{key: "status", aliases: []string{"status"}, required: true},
				{key: "created_at", aliases: []string{"created_at", "date", "datecreated"}},
			},
			FileTransactions: {
				{key: "id", aliases: []string{"id", "accountid"}, required: true},
				{key: "transaction_id", aliases: []string{"transid"}, required: true},
				{key: "gateway", aliases: []string{"gateway"}, required: true},
				{key: "amount", aliases: []string{"amount", "amountin"}, required: true},
				{key: "client_id", aliases: []string{"userid"}},
				{key: "invoice_id", aliases: []string{"invoiceid"}},
				{key: "currency", aliases: []string{"currency", "currency_code"}},
				{key: "received_at", aliases: []string{"date", "datecreated"}},
				{key: "description", aliases: []string{"description", "transnotes"}},
			},
		},
	}
}

func paymenterProfile() *profile {
	return &profile{
		source: "paymenter",
		columns: map[string][]field{
			FileClients: {
				{key: "id", aliases: []string{"user_id", "id"}, required: true},
				{key: "email", aliases: []string{"email"}, required: true},
				{key: "first_name", aliases: []string{"first_name", "firstname"}},
				{key: "last_name", aliases: []string{"last_name", "lastname"}},
				{key: "company", aliases: []string{"company"}},
				{key: "created_at", aliases: []string{"created_at"}},
				{key: "currency", aliases: []string{"currency_code", "currency"}},
			},
			FileInvoices: {
				{key: "id", aliases: []string{"invoice_id", "id"}, required: true},
				{key: "client_id", aliases: []string{"user_id"}, required: true},
				{key: "number", aliases: []string{"number"}, required: true},
				{key: "status", aliases: []string{"status"}, required: true},
				{key: "total", aliases: []string{"total"}, required: true},
				{key: "subtotal", aliases: []string{"subtotal"}},
				{key: "tax", aliases: []string{"tax"}},
				{key: "amount_paid", aliases: []string{"amount_paid"}},
				{key: "currency", aliases: []string{"currency_code", "currency"}},
				{key: "issued_at", aliases: []string{"created_at", "issued_at"}},
				{key: "due_at", aliases: []string{"due_at"}},
				{key: "paid_at", aliases: []string{"paid_at"}},
			},
			FileInvoiceItems: {
				{key: "id", aliases: []string{"invoice_item_id", "id"}, required: true},
				{key: "invoice_id", aliases: []string{"invoice_id"}, required: true},
				{key: "description", aliases: []string{"description"}, required: true},
				{key: "amount", aliases: []string{"amount", "price"}, required: true},
			},
			FileServices: {
				{key: "id", aliases: []string{"service_id", "id"}, required: true},
				{key: "client_id", aliases: []string{"user_id"}, required: true},
				{key: "product", aliases: []string{"product", "plan", "product_slug"}, required: true},
				{key: "status", aliases: []string{"status"}, required: true},
				{key: "external_id", aliases: []string{"external_id", "server_id"}},
				{key: "billing_cycle", aliases: []string{"billing_cycle", "plan_interval"}},
				{key: "created_at", aliases: []string{"created_at"}},
				{key: "next_due_at", aliases: []string{"expires_at", "next_due_at"}},
			},
			FileCurrencies: {
				{key: "code", aliases: []string{"code"}, required: true},
				{key: "name", aliases: []string{"name"}},
				{key: "symbol", aliases: []string{"symbol"}},
				{key: "symbol_position", aliases: []string{"symbol_position"}},
				{key: "scale", aliases: []string{"scale", "decimal_places"}, required: true},
				{key: "enabled", aliases: []string{"enabled"}},
			},
			FileAdmins: {
				{key: "id", aliases: []string{"id", "user_id"}},
				{key: "email", aliases: []string{"email"}, required: true},
				{key: "first_name", aliases: []string{"first_name"}},
				{key: "last_name", aliases: []string{"last_name"}},
				{key: "name", aliases: []string{"name"}},
				{key: "role", aliases: []string{"role"}, required: true},
			},
			FileProducts: {
				{key: "id", aliases: []string{"id", "product_id"}},
				{key: "name", aliases: []string{"name"}, required: true},
				{key: "slug", aliases: []string{"slug"}},
				{key: "price", aliases: []string{"price", "amount"}},
				{key: "currency", aliases: []string{"currency", "currency_code"}},
				{key: "interval", aliases: []string{"interval", "billing_cycle"}},
			},
			FileTickets: {
				{key: "id", aliases: []string{"id", "ticket_id"}, required: true},
				{key: "number", aliases: []string{"number"}},
				{key: "client_id", aliases: []string{"client_id", "user_id"}, required: true},
				{key: "subject", aliases: []string{"subject", "title"}, required: true},
				{key: "department", aliases: []string{"department"}},
				{key: "priority", aliases: []string{"priority"}},
				{key: "status", aliases: []string{"status"}, required: true},
				{key: "created_at", aliases: []string{"created_at"}},
			},
			FileTransactions: {
				{key: "id", aliases: []string{"transaction_id", "id"}, required: true},
				{key: "transaction_id", aliases: []string{"reference", "transaction_reference", "transaction_id"}, required: true},
				{key: "gateway", aliases: []string{"gateway", "payment_method"}, required: true},
				{key: "amount", aliases: []string{"amount"}, required: true},
				{key: "client_id", aliases: []string{"user_id"}},
				{key: "invoice_id", aliases: []string{"invoice_id"}},
				{key: "currency", aliases: []string{"currency_code", "currency"}},
				{key: "received_at", aliases: []string{"created_at", "date"}},
				{key: "description", aliases: []string{"description", "notes"}},
			},
		},
	}
}

// ------------------------------------------------------------------ reading --

// rowError marks one bad row. It is not fatal: the reader continues, and the executor
// records a rejection with the row number the operator needs.
type rowError struct {
	code   string
	detail string
}

func (e *rowError) Error() string { return e.code + ": " + e.detail }

// table is the header of one document.
type table struct {
	file string
	// index maps a normalised column name to its position.
	index map[string]int
	// width is the number of header cells, which is what a row must match.
	width int
}

// reader streams one document.
type reader struct {
	file string
	t    *table
	csv  *csv.Reader
	prof *profile
	rows int
}

// newReader parses a document's header and verifies the required columns are present.
//
// A missing required column fails the whole document rather than every row: there is
// nothing to reject row by row when the column that names the entity is absent.
func newReader(file, text string, prof *profile) (*reader, error) {
	cr := csv.NewReader(strings.NewReader(text))
	// Ragged rows are a data error to report per row, not a reason to stop reading: a dump
	// with one short line should still import the other thousand.
	cr.FieldsPerRecord = -1
	cr.TrimLeadingSpace = true

	header, err := cr.Read()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return nil, &rowError{code: CodeInvalidCSV, detail: "the file is empty; a header row is required"}
		}
		return nil, &rowError{code: CodeInvalidCSV, detail: "the header could not be read: " + err.Error()}
	}
	t := &table{file: file, index: make(map[string]int, len(header)), width: len(header)}
	for i, cell := range header {
		name := normalizeColumn(cell)
		if i == 0 {
			// Excel writes a UTF-8 byte-order mark before the first header cell. Left in
			// place it makes the first column name unmatchable, and the failure looks like
			// "the dump has no id column", which sends the operator hunting in the wrong
			// place.
			name = strings.TrimPrefix(name, "\ufeff")
		}
		if name == "" {
			continue
		}
		if _, dup := t.index[name]; dup {
			// Two columns with one name is ambiguous. The first wins, which matches what a
			// spreadsheet shows when the duplicate is scrolled off screen.
			continue
		}
		t.index[name] = i
	}

	var missing []string
	for _, f := range prof.columns[file] {
		if !f.required {
			continue
		}
		if !t.has(f, prof) {
			missing = append(missing, f.key)
		}
	}
	if len(missing) > 0 {
		return nil, &rowError{
			code: CodeMissingColumn,
			detail: fmt.Sprintf("%s is missing the column(s) %s; the dump cannot be read without them",
				file, strings.Join(missing, ", ")),
		}
	}
	return &reader{file: file, t: t, csv: cr, prof: prof}, nil
}

// has reports whether any alias of f is present.
func (t *table) has(f field, prof *profile) bool {
	for _, alias := range append(append([]string{}, f.aliases...), f.key) {
		if _, ok := t.index[normalizeColumn(alias)]; ok {
			return true
		}
	}
	return false
}

// csvRow is one data row with its position.
type csvRow struct {
	t      *table
	prof   *profile
	values []string
	// row is the 1-based data row number (the header is not a row).
	row int
	// line is the 1-based physical line the record starts on.
	line int
}

// next returns the next data row, or io.EOF.
//
// A malformed record is returned as a *rowError so the caller can reject it with a row
// number and carry on. Blank lines are skipped: a trailing newline is not a row.
func (rd *reader) next() (*csvRow, error) {
	for {
		record, err := rd.csv.Read()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil, io.EOF
			}
			var parseErr *csv.ParseError
			if errors.As(err, &parseErr) {
				return nil, &rowError{
					code:   CodeInvalidCSV,
					detail: fmt.Sprintf("line %d: %s", parseErr.Line, parseErr.Err),
				}
			}
			return nil, err
		}
		line := 0
		if len(record) > 0 {
			if l, _ := rd.csv.FieldPos(0); l > 0 {
				line = l
			}
		}
		blank := true
		for _, cell := range record {
			if strings.TrimSpace(cell) != "" {
				blank = false
				break
			}
		}
		if blank {
			continue
		}
		rd.rows++
		if len(record) != rd.t.width {
			return &csvRow{t: rd.t, prof: rd.prof, values: record, row: rd.rows, line: line},
				&rowError{
					code: CodeFieldCount,
					detail: fmt.Sprintf("line %d has %d fields, the header has %d",
						line, len(record), rd.t.width),
				}
		}
		return &csvRow{t: rd.t, prof: rd.prof, values: record, row: rd.rows, line: line}, nil
	}
}

// get returns the first non-empty value among the canonical field's aliases.
//
// The first non-empty value wins rather than the first present one, so a dump that carries
// both `transid` and an empty `transaction_id` still imports. Preference order is the
// profile's, which puts the dialect's own column first.
func (r *csvRow) get(file, key string) string {
	for _, alias := range r.prof.lookup(file, key) {
		if i, ok := r.t.index[normalizeColumn(alias)]; ok && i < len(r.values) {
			if v := strings.TrimSpace(r.values[i]); v != "" {
				return v
			}
		}
	}
	if i, ok := r.t.index[normalizeColumn(key)]; ok && i < len(r.values) {
		return strings.TrimSpace(r.values[i])
	}
	return ""
}

// normalizeColumn canonicalises a header cell so `Invoice ID`, `invoice-id` and
// `invoice_id` are one column.
func normalizeColumn(name string) string {
	var b strings.Builder
	b.Grow(len(name))
	for _, r := range strings.TrimSpace(strings.ToLower(name)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '_' || r == ' ' || r == '-' || r == '.':
			b.WriteByte('_')
		}
	}
	return strings.Trim(b.String(), "_")
}

// minorString renders a legacy id for a message without letting a newline break the JSON
// shape of a detail sentence.
func sanitizeID(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 64 {
		s = s[:64]
	}
	return strconv.Quote(s)
}
