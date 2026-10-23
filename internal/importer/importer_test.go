package importer_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/catalyst/billing/internal/importer"
	"github.com/catalyst/billing/internal/store"
	"github.com/catalyst/billing/internal/store/storetest"
)

// The importer against the real database.
//
// The properties these tests assert are the ones a screen cannot show and a unit test with a
// fake pool cannot prove: that a dry run leaves the database byte-identical, that a second
// run of the same dump creates nothing, that a refused invoice does not renumber itself, and
// that the transaction is all-or-nothing.
func init() {
	storetest.RegisterTables("import_runs", "import_records")
}

// ---------------------------------------------------------------- fixtures --

// dump builds the five documents from a set of column/row pairs, so each test states only
// what it is about.
type dump struct {
	files map[string]string
}

func newDump() *dump {
	return &dump{files: map[string]string{}}
}

func (d *dump) file(name string, header string, rows ...string) *dump {
	var b strings.Builder
	b.WriteString(header)
	b.WriteString("\n")
	for _, r := range rows {
		b.WriteString(r)
		b.WriteString("\n")
	}
	d.files[name] = b.String()
	return d
}

// emptyFiles fills in every document with a header, so a test that is not about a file does
// not have to mention it.
func (d *dump) emptyFiles() *dump {
	headers := map[string]string{
		importer.FileClients:      "userid,firstname,lastname,email,datecreated,currency",
		importer.FileInvoices:     "invoiceid,userid,invoicenum,date,duedate,subtotal,tax,total,status,currency",
		importer.FileInvoiceItems: "id,invoiceid,description,amount",
		importer.FileServices:     "id,userid,package,domain,domainstatus,regdate",
		importer.FileTransactions: "id,userid,invoiceid,gateway,transid,date,amount,currency",
	}
	for name, header := range headers {
		if _, ok := d.files[name]; !ok {
			d.file(name, header)
		}
	}
	return d
}

// run builds the request. A fixed clock keeps the report's timestamps stable.
func (d *dump) run(commit bool) importer.Input {
	return importer.Input{
		Source: "whmcs",
		Commit: commit,
		Files:  d.files,
		Now:    time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC),
		// Most fixtures carry no currency column; a request-level default is what an operator
		// sets for a single-currency installation.
		DefaultCurrency: "USD",
	}
}

// seedProduct creates a product so an imported service has something to point at.
func seedProduct(t *testing.T, pool *store.Pool, slug string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO products (slug, name) VALUES ($1, $2) RETURNING id::text`,
		slug, strings.ToUpper(slug)).Scan(&id); err != nil {
		t.Fatalf("seed product %q: %v", slug, err)
	}
	return id
}

// countRows is the assertion most of these tests make: "there are N of these".
func countRows(t *testing.T, pool *store.Pool, query string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatalf("count (%s): %v", query, err)
	}
	return n
}

// rejectionFor finds the rejection for one legacy id, so a test can assert on a code without
// depending on the order of the list.
func rejectionFor(t *testing.T, rep importer.Report, legacyID string) importer.Rejection {
	t.Helper()
	for _, r := range rep.Rejections {
		if r.LegacyID == legacyID {
			return r
		}
	}
	t.Fatalf("no rejection for legacy id %q; rejections: %+v", legacyID, rep.Rejections)
	return importer.Rejection{}
}

func warningCodes(rep importer.Report) []string {
	out := make([]string, 0, len(rep.Warnings))
	for _, w := range rep.Warnings {
		out = append(out, w.Code)
	}
	return out
}

func hasWarning(rep importer.Report, code string) bool {
	for _, w := range rep.Warnings {
		if w.Code == code {
			return true
		}
	}
	return false
}

// fullDump is the fixture the round-trip tests share: two good clients, one bad; a paid
// invoice with a line and a matching payment, an unpaid invoice, and an invoice with no
// number; one service that resolves and one that does not.
func fullDump() *dump {
	return newDump().
		file(importer.FileClients,
			"userid,firstname,lastname,email,companyname,datecreated,currency",
			"1,Ada,Lovelace,ada@example.test,Analytical Engines,2021-03-04,USD",
			"2,Grace,Hopper,grace@example.test,,2021-05-06,USD",
			"3,Bad,Email,not-an-address,,2021-05-06,USD",
		).
		file(importer.FileInvoices,
			"invoiceid,userid,invoicenum,date,duedate,datepaid,subtotal,tax,total,status,currency",
			"10,1,INV-000101,2021-03-04,2021-03-18,2021-03-05,100.00,20.00,120.00,Paid,USD",
			"11,2,INV-000102,2021-04-01,2021-04-15,,50.00,0.00,50.00,Unpaid,USD",
			"12,1,,2021-04-02,2021-04-16,,10.00,0.00,10.00,Unpaid,USD",
		).
		file(importer.FileInvoiceItems,
			"id,invoiceid,description,amount",
			"100,10,VPS Small - monthly,100.00",
			"101,11,Managed DNS,50.00",
		).
		file(importer.FileServices,
			"id,userid,package,domain,domainstatus,billingcycle,regdate,nextduedate",
			"200,1,vps-small,ada.example.test,Active,Monthly,2021-03-04,2021-04-04",
			"201,2,no-such-product,grace.example.test,Active,Monthly,2021-05-06,2021-06-06",
		).
		file(importer.FileTransactions,
			"id,userid,invoiceid,gateway,transid,date,amount,currency",
			"300,1,10,Stripe,ch_123,2021-03-05,120.00,USD",
			"301,2,11,Stripe,ch_124,2021-04-02,10.00,USD",
		)
}

// ------------------------------------------------------------------ dry run --

// A dry run reports every row and writes nothing at all — not a customer, not a run row.
func TestDryRunWritesNothing(t *testing.T) {
	pool := storetest.DB(t)
	seedProduct(t, pool, "vps-small")

	rep, err := importer.Run(context.Background(), pool, fullDump().run(false))
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if !rep.DryRun {
		t.Fatal("the report says this was not a dry run")
	}
	if rep.RunID != "" {
		t.Fatalf("a dry run has a run id (%q); it must not be recorded", rep.RunID)
	}
	if rep.Totals.RowsRead != 12 {
		t.Fatalf("rows read = %d, want 12", rep.Totals.RowsRead)
	}
	if rep.Totals.Created != 9 {
		t.Fatalf("created = %d, want 9; report %+v", rep.Totals, rep.Tables)
	}
	if rep.Totals.Rejected != 3 {
		t.Fatalf("rejected = %d, want 3; report %+v", rep.Totals, rep.Rejections)
	}

	for _, q := range []string{
		`SELECT count(*) FROM customers`,
		`SELECT count(*) FROM invoices`,
		`SELECT count(*) FROM invoice_lines`,
		`SELECT count(*) FROM services`,
		`SELECT count(*) FROM payments`,
		`SELECT count(*) FROM payment_allocations`,
		`SELECT count(*) FROM import_runs`,
		`SELECT count(*) FROM import_records`,
		`SELECT count(*) FROM audit_events WHERE action = 'import.run'`,
	} {
		if n := countRows(t, pool, q); n != 0 {
			t.Fatalf("%s returned %d rows after a dry run; a dry run writes nothing", q, n)
		}
	}
	// And it does not reserve invoice numbers either: the dry run's numbers are not printed
	// on any document, so the series must not move.
	if n := countRows(t, pool,
		`SELECT count(*) FROM invoice_number_sequences WHERE scope = 'default'`); n != 0 {
		t.Fatal("a dry run created an invoice number sequence")
	}
}

// Every rejection carries the row it came from, and the row number counts data rows while the
// line number counts the file.
func TestRejectionsCarryRowAndReason(t *testing.T) {
	pool := storetest.DB(t)
	seedProduct(t, pool, "vps-small")

	rep, err := importer.Run(context.Background(), pool, fullDump().run(false))
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}

	badEmail := rejectionFor(t, rep, "3")
	if badEmail.Code != importer.CodeInvalidEmail {
		t.Fatalf("client 3 code = %q, want %q", badEmail.Code, importer.CodeInvalidEmail)
	}
	if badEmail.File != importer.FileClients || badEmail.Row != 3 || badEmail.Line != 4 {
		t.Fatalf("client 3 rejection at %s row %d line %d, want clients row 3 line 4",
			badEmail.File, badEmail.Row, badEmail.Line)
	}
	if !strings.Contains(badEmail.Detail, "not-an-address") {
		t.Fatalf("the rejection does not name the offending value: %q", badEmail.Detail)
	}

	noNumber := rejectionFor(t, rep, "12")
	if noNumber.Code != importer.CodeMissingNumber {
		t.Fatalf("invoice 12 code = %q, want %q", noNumber.Code, importer.CodeMissingNumber)
	}
	if noNumber.Row != 3 || noNumber.Line != 4 {
		t.Fatalf("invoice 12 at row %d line %d, want 3/4", noNumber.Row, noNumber.Line)
	}

	noProduct := rejectionFor(t, rep, "201")
	if noProduct.Code != importer.CodeUnknownProduct {
		t.Fatalf("service 201 code = %q, want %q", noProduct.Code, importer.CodeUnknownProduct)
	}
	if !strings.Contains(noProduct.Detail, "no-such-product") {
		t.Fatalf("the rejection does not name the slug: %q", noProduct.Detail)
	}
}

// A file that cannot be read fails as a file, with the reason, and does not stop the rest.
func TestUnreadableFileIsReportedAndOthersStillImport(t *testing.T) {
	pool := storetest.DB(t)

	d := newDump().
		file(importer.FileClients, "userid,email", "1,ada@example.test").
		// No invoicenum column: the document cannot be read at all.
		file(importer.FileInvoices, "invoiceid,userid,date,total,status", "10,1,2021-03-04,10.00,Unpaid").
		emptyFiles()

	rep, err := importer.Run(context.Background(), pool, d.run(false))
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	var invoices importer.FileReport
	for _, f := range rep.Files {
		if f.File == importer.FileInvoices {
			invoices = f
		}
	}
	if invoices.Error == "" {
		t.Fatal("the unreadable file has no error")
	}
	if !strings.Contains(invoices.Error, "number") {
		t.Fatalf("the file error does not name the missing column: %q", invoices.Error)
	}
	if rep.Totals.Created != 1 {
		t.Fatalf("created = %d, want the client to have imported anyway", rep.Totals.Created)
	}
}

// -------------------------------------------------------------- commit path --

// A commit writes the rows, and a second commit of the same dump writes nothing: the natural
// key is the legacy id, and the mapping is the idempotency.
func TestCommitThenReplayCreatesNothing(t *testing.T) {
	pool := storetest.DB(t)
	seedProduct(t, pool, "vps-small")
	ctx := context.Background()

	first, err := importer.Run(ctx, pool, fullDump().run(true))
	if err != nil {
		t.Fatalf("first commit: %v", err)
	}
	if first.DryRun {
		t.Fatal("the report says a commit was a dry run")
	}
	if first.RunID == "" {
		t.Fatal("a commit has no run id")
	}
	if first.Totals.Created != 9 {
		t.Fatalf("created = %d, want 9; rejections %+v", first.Totals.Created, first.Rejections)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM customers`); n != 2 {
		t.Fatalf("customers = %d, want 2", n)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM invoices`); n != 2 {
		t.Fatalf("invoices = %d, want 2", n)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM payments`); n != 2 {
		t.Fatalf("payments = %d, want 2", n)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM payment_allocations`); n != 2 {
		t.Fatalf("allocations = %d, want 2", n)
	}

	// The paid invoice carries the dump's own numbers: 120.00 total, 20.00 of tax, fully paid.
	var (
		total, tax, paid int64
		status           string
	)
	if err := pool.QueryRow(ctx, `
		SELECT total_minor, tax_minor, amount_paid_minor, status
		FROM invoices WHERE number = 'INV-000101'`).Scan(&total, &tax, &paid, &status); err != nil {
		t.Fatalf("read imported invoice: %v", err)
	}
	if total != 12000 || tax != 2000 || paid != 12000 || status != "paid" {
		t.Fatalf("imported invoice = %d/%d/%d %s, want 12000/2000/12000 paid", total, tax, paid, status)
	}

	// The number series moved past the dump's highest number, so the next invoice this panel
	// issues cannot print a number a customer already holds.
	var next int64
	if err := pool.QueryRow(ctx,
		`SELECT next_value FROM invoice_number_sequences WHERE scope = 'default'`).Scan(&next); err != nil {
		t.Fatalf("read invoice sequence: %v", err)
	}
	if next != 103 {
		t.Fatalf("invoice sequence next_value = %d, want 103 (the dump printed INV-000102)", next)
	}

	mappings := countRows(t, pool, `SELECT count(*) FROM import_records`)

	second, err := importer.Run(ctx, pool, fullDump().run(true))
	if err != nil {
		t.Fatalf("second commit: %v", err)
	}
	if second.Totals.Created != 0 {
		t.Fatalf("a replay created %d rows; it must create none", second.Totals.Created)
	}
	if second.Totals.Updated != 0 {
		t.Fatalf("a replay updated %d rows; nothing in the dump changed", second.Totals.Updated)
	}
	if second.Totals.Unchanged != 9 {
		t.Fatalf("a replay reports %d unchanged, want 9; rejections %+v",
			second.Totals.Unchanged, second.Rejections)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM import_records`); n != mappings {
		t.Fatalf("import_records went from %d to %d on a replay", mappings, n)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM customers`); n != 2 {
		t.Fatalf("a replay created customers: %d", n)
	}
	// The rejected rows are still rejected, with the same reasons: fixing the export is the
	// only thing that changes that.
	if len(second.Rejections) != 3 {
		t.Fatalf("a replay reported %d rejections, want the same 3", len(second.Rejections))
	}
}

// An imported invoice that the dump later changes is refused rather than rewritten: an issued
// invoice is immutable, and the importer is not an exception to that.
func TestChangedInvoiceIsRefusedNotRewritten(t *testing.T) {
	pool := storetest.DB(t)
	ctx := context.Background()

	base := newDump().
		file(importer.FileClients, "userid,email", "1,ada@example.test").
		file(importer.FileInvoices,
			"invoiceid,userid,invoicenum,date,subtotal,tax,total,status",
			"10,1,INV-000501,2021-03-04,100.00,0.00,100.00,Unpaid").
		emptyFiles()

	if _, err := importer.Run(ctx, pool, base.run(true)); err != nil {
		t.Fatalf("first commit: %v", err)
	}

	changed := newDump().
		file(importer.FileClients, "userid,email", "1,ada@example.test").
		file(importer.FileInvoices,
			"invoiceid,userid,invoicenum,date,subtotal,tax,total,status",
			"10,1,INV-000501,2021-03-04,200.00,0.00,200.00,Unpaid").
		emptyFiles()

	rep, err := importer.Run(ctx, pool, changed.run(true))
	if err != nil {
		t.Fatalf("second commit: %v", err)
	}
	r := rejectionFor(t, rep, "10")
	if r.Code != importer.CodeInvoiceImmutable {
		t.Fatalf("code = %q, want %q", r.Code, importer.CodeInvoiceImmutable)
	}
	var total int64
	if err := pool.QueryRow(ctx,
		`SELECT total_minor FROM invoices WHERE number = 'INV-000501'`).Scan(&total); err != nil {
		t.Fatalf("read invoice: %v", err)
	}
	if total != 10000 {
		t.Fatalf("the invoice total is now %d; an issued invoice must not be rewritten", total)
	}
}

// A number that is already printed on another invoice is a conflict, never a renumbering.
func TestInvoiceNumberIsNeverInventedOrReused(t *testing.T) {
	pool := storetest.DB(t)
	ctx := context.Background()

	var customerID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO customers (email) VALUES ('existing@example.test') RETURNING id::text`).
		Scan(&customerID); err != nil {
		t.Fatalf("seed customer: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO invoices (number, customer_id, status, currency, subtotal_minor, tax_minor,
			total_minor, issued_at, snapshot)
		VALUES ('INV-000900', $1, 'open', 'USD', 500, 0, 500, now(), '{"schema_version":1}'::jsonb)`,
		customerID); err != nil {
		t.Fatalf("seed invoice: %v", err)
	}

	d := newDump().
		file(importer.FileClients, "userid,email", "1,ada@example.test").
		file(importer.FileInvoices,
			"invoiceid,userid,invoicenum,date,subtotal,tax,total,status",
			"20,1,INV-000900,2021-03-04,100.00,0.00,100.00,Unpaid",
			"21,1,,2021-03-04,100.00,0.00,100.00,Unpaid",
			"22,1,INV-000901,2021-03-04,100.00,0.00,100.00,Unpaid",
			"23,1,INV-000901,2021-03-04,100.00,0.00,100.00,Unpaid",
		).
		emptyFiles()

	rep, err := importer.Run(ctx, pool, d.run(true))
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if r := rejectionFor(t, rep, "20"); r.Code != importer.CodeNumberTaken {
		t.Fatalf("colliding number code = %q, want %q", r.Code, importer.CodeNumberTaken)
	}
	if r := rejectionFor(t, rep, "21"); r.Code != importer.CodeMissingNumber {
		t.Fatalf("missing number code = %q, want %q", r.Code, importer.CodeMissingNumber)
	}
	if r := rejectionFor(t, rep, "23"); r.Code != importer.CodeDuplicateNumber {
		t.Fatalf("duplicate number code = %q, want %q", r.Code, importer.CodeDuplicateNumber)
	}
	// The seeded invoice still carries its own number and its own total.
	var total int64
	if err := pool.QueryRow(ctx,
		`SELECT total_minor FROM invoices WHERE number = 'INV-000900'`).Scan(&total); err != nil {
		t.Fatalf("read seeded invoice: %v", err)
	}
	if total != 500 {
		t.Fatalf("the seeded invoice was rewritten to %d", total)
	}
}

// A child row whose parent was refused in the same run says so, instead of claiming the parent
// does not exist.
func TestChildOfRefusedParentSaysSo(t *testing.T) {
	pool := storetest.DB(t)

	d := newDump().
		file(importer.FileClients, "userid,email", "1,not-an-address").
		file(importer.FileInvoices,
			"invoiceid,userid,invoicenum,date,subtotal,tax,total,status",
			"10,1,INV-000700,2021-03-04,10.00,0.00,10.00,Unpaid").
		emptyFiles()

	rep, err := importer.Run(context.Background(), pool, d.run(false))
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	r := rejectionFor(t, rep, "10")
	if r.Code != importer.CodeParentRejected {
		t.Fatalf("code = %q, want %q", r.Code, importer.CodeParentRejected)
	}
	if !strings.Contains(r.Detail, importer.CodeInvalidEmail) {
		t.Fatalf("the rejection does not say why the parent was refused: %q", r.Detail)
	}
}

// The payments table is unique on the provider's payment id, and the importer treats that as
// the identity: a payment that already exists is adopted, never duplicated.
func TestPaymentIsIdempotentOnProviderPaymentID(t *testing.T) {
	pool := storetest.DB(t)
	ctx := context.Background()

	if _, err := pool.Exec(ctx, `
		INSERT INTO payments (provider, provider_account, provider_payment_id, amount_minor,
			currency, status)
		VALUES ('stripe', 'import:whmcs', 'ch_live_1', 4200, 'USD', 'succeeded')`); err != nil {
		t.Fatalf("seed payment: %v", err)
	}

	d := newDump().
		file(importer.FileClients, "userid,email", "1,ada@example.test").
		file(importer.FileInvoices,
			"invoiceid,userid,invoicenum,date,subtotal,tax,total,status",
			"10,1,INV-000800,2021-03-04,42.00,0.00,42.00,Paid").
		file(importer.FileTransactions,
			"id,userid,invoiceid,gateway,transid,date,amount,currency",
			"500,1,10,Stripe,ch_live_1,2021-03-05,42.00,USD").
		emptyFiles()

	rep, err := importer.Run(ctx, pool, d.run(true))
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if rep.Totals.Created != 2 { // the client and the invoice; the payment was adopted
		t.Fatalf("created = %d, want 2; report %+v", rep.Totals.Created, rep.Tables)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM payments`); n != 1 {
		t.Fatalf("payments = %d, want the seeded one only", n)
	}
	// The adopted payment was allocated to the invoice it names.
	if n := countRows(t, pool, `SELECT count(*) FROM payment_allocations`); n != 1 {
		t.Fatalf("allocations = %d, want 1", n)
	}
}

// A payment larger than the invoice it names would take the invoice past its total, which the
// schema forbids. It is refused with the room that is left, and no payment is written.
func TestOverpaymentIsRefused(t *testing.T) {
	pool := storetest.DB(t)

	d := newDump().
		file(importer.FileClients, "userid,email", "1,ada@example.test").
		file(importer.FileInvoices,
			"invoiceid,userid,invoicenum,date,subtotal,tax,total,status",
			"10,1,INV-000810,2021-03-04,10.00,0.00,10.00,Unpaid").
		file(importer.FileTransactions,
			"id,userid,invoiceid,gateway,transid,date,amount,currency",
			"510,1,10,Stripe,ch_over,2021-03-05,15.00,USD").
		emptyFiles()

	rep, err := importer.Run(context.Background(), pool, d.run(true))
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	r := rejectionFor(t, rep, "510")
	if r.Code != importer.CodeOverpayment {
		t.Fatalf("code = %q, want %q", r.Code, importer.CodeOverpayment)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM payments`); n != 0 {
		t.Fatalf("payments = %d; a refused payment must not be written", n)
	}
}

// A paid invoice whose payments are not in the dump is imported as paid, and the report says
// the two documents disagree rather than inventing a receipt.
func TestPaidInvoiceWithoutPaymentsWarns(t *testing.T) {
	pool := storetest.DB(t)

	d := newDump().
		file(importer.FileClients, "userid,email", "1,ada@example.test").
		file(importer.FileInvoices,
			"invoiceid,userid,invoicenum,date,subtotal,tax,total,status",
			"10,1,INV-000820,2021-03-04,10.00,0.00,10.00,Paid").
		emptyFiles()

	rep, err := importer.Run(context.Background(), pool, d.run(false))
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if !hasWarning(rep, importer.WarnPaidWithoutPayments) {
		t.Fatalf("no paid-without-payments warning; warnings %v", warningCodes(rep))
	}
	// The invoice carries no tax and no lines here, so the two warnings about tax and line
	// sums must not appear: a warning that fires on every import is noise, not information.
	if hasWarning(rep, importer.WarnTaxNotReconstructed) || hasWarning(rep, importer.WarnSubtotalMismatch) {
		t.Fatalf("unexpected warnings: %v", warningCodes(rep))
	}
}

// A malformed amount is refused with the value and the reason, not rounded into a plausible
// one.
func TestMalformedMoneyIsRefused(t *testing.T) {
	pool := storetest.DB(t)

	d := newDump().
		file(importer.FileClients, "userid,email", "1,ada@example.test").
		file(importer.FileInvoices,
			"invoiceid,userid,invoicenum,date,subtotal,tax,total,status",
			"10,1,INV-000830,2021-03-04,10.001,0.00,10.001,Unpaid",
			"11,1,INV-000831,2021-03-04,10.00,0.00,12.00,Unpaid",
			"12,1,INV-000832,2021-03-04,10.00,0.00,10.00,Nonsense").
		emptyFiles()

	rep, err := importer.Run(context.Background(), pool, d.run(false))
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if r := rejectionFor(t, rep, "10"); r.Code != importer.CodeInvalidAmount {
		t.Fatalf("code = %q, want %q", r.Code, importer.CodeInvalidAmount)
	}
	if r := rejectionFor(t, rep, "11"); r.Code != importer.CodeTotalMismatch {
		t.Fatalf("code = %q, want %q", r.Code, importer.CodeTotalMismatch)
	}
	if r := rejectionFor(t, rep, "12"); r.Code != importer.CodeUnknownStatus {
		t.Fatalf("code = %q, want %q", r.Code, importer.CodeUnknownStatus)
	}
}

// ---------------------------------------------------------------- the API contract --

// A request the importer cannot act on is refused before any row is read.
func TestInvalidRequestsAreRefused(t *testing.T) {
	pool := storetest.DB(t)
	ctx := context.Background()

	if _, err := importer.Run(ctx, nil, importer.Input{Source: "whmcs"}); !errors.Is(err, importer.ErrUnavailable) {
		t.Fatalf("no pool: err = %v, want ErrUnavailable", err)
	}
	if _, err := importer.Run(ctx, pool, importer.Input{Source: "sage50"}); !errors.Is(err, importer.ErrUnknownSource) {
		t.Fatalf("unknown source: err = %v, want ErrUnknownSource", err)
	}
	in := fullDump().run(false)
	delete(in.Files, importer.FileTransactions)
	if _, err := importer.Run(ctx, pool, in); !errors.Is(err, importer.ErrMissingFile) {
		t.Fatalf("missing file: err = %v, want ErrMissingFile", err)
	}
	// A document this build does not read is refused rather than ignored. `tickets` used to be
	// this test's example and is now a *supported* optional document (OPS-10), so the example had
	// to become one nothing reads.
	in = fullDump().run(false)
	in.Files["sage50_ledger"] = "id,amount"
	if _, err := importer.Run(ctx, pool, in); !errors.Is(err, importer.ErrInvalidInput) {
		t.Fatalf("unknown file: err = %v, want ErrInvalidInput", err)
	}
}

// The run history lists committed runs only, and returns the report verbatim.
func TestRunHistory(t *testing.T) {
	pool := storetest.DB(t)
	seedProduct(t, pool, "vps-small")
	ctx := context.Background()

	dry, err := importer.Run(ctx, pool, fullDump().run(false))
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	committed, err := importer.Run(ctx, pool, fullDump().run(true))
	if err != nil {
		t.Fatalf("commit: %v", err)
	}

	list, err := importer.ListRuns(ctx, pool, 10, "")
	if err != nil {
		t.Fatalf("list runs: %v", err)
	}
	if len(list.Runs) != 1 {
		t.Fatalf("runs = %d, want only the committed one (the dry run is not recorded)", len(list.Runs))
	}
	if list.Runs[0].ID != committed.RunID {
		t.Fatalf("listed run %q, want %q", list.Runs[0].ID, committed.RunID)
	}
	if list.Runs[0].Created != committed.Totals.Created {
		t.Fatalf("stored created = %d, report says %d", list.Runs[0].Created, committed.Totals.Created)
	}

	got, err := importer.GetRun(ctx, pool, committed.RunID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if len(got.Report.Rejections) != len(committed.Rejections) {
		t.Fatalf("the stored report has %d rejections, the response had %d",
			len(got.Report.Rejections), len(committed.Rejections))
	}
	if got.Report.Rejections[0].Code == "" || got.Report.Rejections[0].Detail == "" {
		t.Fatalf("the stored rejection lost its reason: %+v", got.Report.Rejections[0])
	}

	if _, err := importer.GetRun(ctx, pool, "00000000-0000-4000-8000-000000000000"); !errors.Is(err, importer.ErrNotFound) {
		t.Fatalf("missing run: err = %v, want ErrNotFound", err)
	}
	// The dry run above ran first and must not appear in the history at all.
	for _, r := range list.Runs {
		if r.ID == dry.RunID {
			t.Fatalf("the dry run (%q) was recorded", dry.RunID)
		}
	}
}

// A commit is one transaction: a failure part-way leaves nothing behind.
func TestFailedCommitWritesNothing(t *testing.T) {
	pool := storetest.DB(t)
	ctx := context.Background()

	// The audit table refuses UPDATE, and the audit row is written at the end of the run, so a
	// trigger that rejects the insert is a clean way to fail the transaction after the rows are
	// written.
	if _, err := pool.Exec(ctx, `CREATE FUNCTION reject_import_audit() RETURNS trigger AS $fn$
		BEGIN
			RAISE EXCEPTION 'audit is closed for this test';
		END;
		$fn$ LANGUAGE plpgsql`); err != nil {
		t.Fatalf("create trigger function: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DROP FUNCTION IF EXISTS reject_import_audit() CASCADE`)
	})
	if _, err := pool.Exec(ctx, `
		CREATE TRIGGER test_reject_import_audit BEFORE INSERT ON audit_events
		FOR EACH ROW EXECUTE FUNCTION reject_import_audit()`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}

	if _, err := importer.Run(ctx, pool, fullDump().run(true)); err == nil {
		t.Fatal("the commit succeeded although the audit write failed")
	}
	if n := countRows(t, pool, `SELECT count(*) FROM customers`); n != 0 {
		t.Fatalf("customers = %d after a failed commit; the run must be all-or-nothing", n)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM import_runs`); n != 0 {
		t.Fatalf("import_runs = %d after a failed commit", n)
	}
}

// A service is a record this panel keeps up to date, so a changed row is an update — and a
// customer row whose email matches an existing customer is adopted rather than duplicated.
func TestServiceUpdateAndCustomerAdoption(t *testing.T) {
	pool := storetest.DB(t)
	ctx := context.Background()
	seedProduct(t, pool, "vps-small")

	var existingID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO customers (email, name) VALUES ('ada@example.test', 'A. Lovelace') RETURNING id::text`).
		Scan(&existingID); err != nil {
		t.Fatalf("seed customer: %v", err)
	}

	first := newDump().
		file(importer.FileClients, "userid,email", "1,ada@example.test").
		file(importer.FileServices,
			"id,userid,package,domain,domainstatus",
			"200,1,vps-small,ada.example.test,Active").
		emptyFiles()
	if _, err := importer.Run(ctx, pool, first.run(true)); err != nil {
		t.Fatalf("first commit: %v", err)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM customers`); n != 1 {
		t.Fatalf("customers = %d, want the seeded one to have been adopted", n)
	}

	second := newDump().
		file(importer.FileClients, "userid,email", "1,ada@example.test").
		file(importer.FileServices,
			"id,userid,package,domain,domainstatus",
			"200,1,vps-small,ada.example.test,Suspended").
		emptyFiles()
	rep, err := importer.Run(ctx, pool, second.run(true))
	if err != nil {
		t.Fatalf("second commit: %v", err)
	}
	if rep.Totals.Updated != 1 {
		t.Fatalf("updated = %d, want the service; report %+v", rep.Totals.Updated, rep.Tables)
	}
	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM services`).Scan(&status); err != nil {
		t.Fatalf("read service: %v", err)
	}
	if status != "suspended" {
		t.Fatalf("service status = %q, want suspended", status)
	}
}

// Two rows with the same legacy id are one rejection, not a failed transaction.
func TestDuplicateLegacyIDIsOneRejection(t *testing.T) {
	pool := storetest.DB(t)

	d := newDump().
		file(importer.FileClients, "userid,email",
			"1,ada@example.test",
			"1,ada-again@example.test").
		emptyFiles()

	rep, err := importer.Run(context.Background(), pool, d.run(true))
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	r := rejectionFor(t, rep, "1")
	if r.Code != importer.CodeDuplicateID {
		t.Fatalf("code = %q, want %q", r.Code, importer.CodeDuplicateID)
	}
	if r.Row != 2 || r.Line != 3 {
		t.Fatalf("duplicate at row %d line %d, want 2/3", r.Row, r.Line)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM customers`); n != 1 {
		t.Fatalf("customers = %d, want 1", n)
	}
}

// A line cannot be added to an invoice that was issued by an earlier run: the database freezes
// it, and the importer reports that as a rejection rather than aborting the transaction.
func TestLineCannotBeAddedToAnIssuedInvoice(t *testing.T) {
	pool := storetest.DB(t)
	ctx := context.Background()

	first := newDump().
		file(importer.FileClients, "userid,email", "1,ada@example.test").
		file(importer.FileInvoices,
			"invoiceid,userid,invoicenum,date,subtotal,tax,total,status",
			"10,1,INV-000910,2021-03-04,10.00,0.00,10.00,Unpaid").
		emptyFiles()
	if _, err := importer.Run(ctx, pool, first.run(true)); err != nil {
		t.Fatalf("first commit: %v", err)
	}

	second := newDump().
		file(importer.FileClients, "userid,email", "1,ada@example.test").
		file(importer.FileInvoices,
			"invoiceid,userid,invoicenum,date,subtotal,tax,total,status",
			"10,1,INV-000910,2021-03-04,10.00,0.00,10.00,Unpaid").
		file(importer.FileInvoiceItems,
			"id,invoiceid,description,amount",
			"100,10,A line that arrived late,10.00").
		emptyFiles()

	rep, err := importer.Run(ctx, pool, second.run(true))
	if err != nil {
		t.Fatalf("second commit: %v", err)
	}
	r := rejectionFor(t, rep, "100")
	if r.Code != importer.CodeInvoiceImmutable {
		t.Fatalf("code = %q, want %q", r.Code, importer.CodeInvoiceImmutable)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM invoice_lines`); n != 0 {
		t.Fatalf("invoice_lines = %d; the issued invoice gained a line", n)
	}
}
