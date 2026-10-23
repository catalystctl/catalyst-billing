package importer_test

import (
	"context"
	"strings"
	"testing"

	"github.com/catalyst/billing/internal/importer"
	"github.com/catalyst/billing/internal/store/storetest"
)

// The extra WHMCS documents (OPS-10): currencies, admins, products and tickets.
//
// The five required documents were imported and the rest of a real dump was not: an operator
// migrating from WHMCS got their customers, invoices, services and payments, and lost their staff
// accounts, their catalogue and their open tickets. These tests assert that each of the four is
// imported, that a row which cannot be read exactly is refused with a code, and that a document
// which is not supplied is reported rather than silently skipped.

// extendedDump is the shared fixture: one client (so a ticket has a parent), one currency this
// panel does not know, one administrator, one priced product and one ticket.
func extendedDump() *dump {
	return newDump().
		emptyFiles().
		file(importer.FileClients,
			"userid,firstname,lastname,email,datecreated,currency",
			"1,Ada,Lovelace,ada@example.test,2021-03-04,USD").
		file(importer.FileCurrencies,
			"code,name,symbol,scale,enabled",
			"XYZ,Example Dollar,$,2,1",
			"USD,US Dollar,$,2,1").
		file(importer.FileAdmins,
			"id,email,firstname,lastname,role",
			"7,ops@example.test,Ops,Person,administrator",
			"8,strange@example.test,Strange,Role,wizard").
		file(importer.FileProducts,
			"id,name,slug,price,currency,interval",
			"40,Starter VPS,starter-vps,12.00,USD,monthly",
			"41,Unpriced Box,unpriced-box,,,")
}

// A currency this panel does not know is created at the dump's scale; an ISO code this panel
// already defines is left alone and reported.
func TestCurrenciesAreImportedWithTheirScale(t *testing.T) {
	pool := storetest.DB(t)
	rep, err := importer.Run(context.Background(), pool, extendedDump().run(true))
	if err != nil {
		t.Fatalf("import: %v", err)
	}

	var (
		name   string
		scale  int
		symbol string
	)
	if err := pool.QueryRow(context.Background(),
		`SELECT name, scale, symbol FROM currencies WHERE code = 'XYZ'`).
		Scan(&name, &scale, &symbol); err != nil {
		t.Fatalf("read imported currency: %v", err)
	}
	if name != "Example Dollar" || scale != 2 || symbol != "$" {
		t.Fatalf("currency = %q/%d/%q, want the dump's own values", name, scale, symbol)
	}

	// USD is defined by the panel; the row was not rewritten from the dump.
	if !hasWarning(rep, importer.WarnCurrencyExistingNotChanged) {
		t.Fatalf("no warning about the already-defined currency; warnings: %v", warningCodes(rep))
	}
	var defaultCount int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM currencies WHERE code = 'XYZ' AND is_default`).Scan(&defaultCount); err != nil {
		t.Fatal(err)
	}
	if defaultCount != 0 {
		t.Fatal("an imported currency became the default: the dump does not get to decide that")
	}
}

// An administrator is created with a role and *no usable password*, and the report says so.
func TestAdminsAreImportedWithoutUsablePasswords(t *testing.T) {
	pool := storetest.DB(t)
	rep, err := importer.Run(context.Background(), pool, extendedDump().run(true))
	if err != nil {
		t.Fatalf("import: %v", err)
	}

	var (
		userID string
		hash   string
		email  string
	)
	if err := pool.QueryRow(context.Background(),
		`SELECT id::text, password_hash, email FROM users WHERE email = 'ops@example.test'`).
		Scan(&userID, &hash, &email); err != nil {
		t.Fatalf("read imported admin: %v", err)
	}
	if hash != "!imported-no-password" {
		t.Fatalf("password_hash = %q, want the marker that no verifier accepts", hash)
	}
	var role string
	if err := pool.QueryRow(context.Background(),
		`SELECT role FROM staff_roles WHERE user_id = $1`, userID).Scan(&role); err != nil {
		t.Fatalf("read role: %v", err)
	}
	if role != "administrator" {
		t.Fatalf("role = %q, want administrator", role)
	}
	if !hasWarning(rep, importer.WarnAdminMustResetPassword) {
		t.Fatalf("no warning telling the operator to reset the password; warnings: %v",
			warningCodes(rep))
	}

	// The unknown role is refused with its own code, not imported as something else.
	rej := rejectionFor(t, rep, "8")
	if rej.Code != importer.CodeInvalidRole {
		t.Fatalf("rejection = %+v, want invalid_role", rej)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM users WHERE email = 'strange@example.test'`); n != 0 {
		t.Fatalf("%d account(s) were created for an unknown role", n)
	}
}

// A product is created with its price and interval; a product without a price is created and
// flagged, and a second import changes nothing.
func TestProductsAreImportedAndPriced(t *testing.T) {
	pool := storetest.DB(t)
	ctx := context.Background()
	rep, err := importer.Run(ctx, pool, extendedDump().run(true))
	if err != nil {
		t.Fatalf("import: %v", err)
	}

	var (
		productID string
		name      string
	)
	if err := pool.QueryRow(ctx,
		`SELECT id::text, name FROM products WHERE slug = 'starter-vps'`).Scan(&productID, &name); err != nil {
		t.Fatalf("read product: %v", err)
	}
	if name != "Starter VPS" {
		t.Fatalf("name = %q", name)
	}
	var (
		minor    int64
		currency string
		interval string
	)
	if err := pool.QueryRow(ctx, `
		SELECT amount_minor, currency, interval FROM price_versions WHERE product_id = $1`,
		productID).Scan(&minor, &currency, &interval); err != nil {
		t.Fatalf("read price version: %v", err)
	}
	if minor != 1200 || currency != "USD" || interval != "monthly" {
		t.Fatalf("price = %d %s %s, want 1200 USD monthly", minor, currency, interval)
	}

	// The unpriced product exists and cannot be sold; the warning is the only thing that makes
	// that visible, because a missing price looks exactly like a free product otherwise.
	if n := countRows(t, pool, `SELECT count(*) FROM products WHERE slug = 'unpriced-box'`); n != 1 {
		t.Fatalf("the unpriced product was not created (%d rows)", n)
	}
	if !hasWarning(rep, importer.WarnProductWithoutPrice) {
		t.Fatalf("no product-without-price warning; warnings: %v", warningCodes(rep))
	}

	// A second run of the same dump creates nothing and reports it.
	again, err := importer.Run(ctx, pool, extendedDump().run(true))
	if err != nil {
		t.Fatalf("second import: %v", err)
	}
	if again.Totals.Created != 0 {
		t.Fatalf("the second run created %d rows; it must be idempotent on the legacy id: %+v",
			again.Totals.Created, again.Totals)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM price_versions`); n != 1 {
		t.Fatalf("%d price versions after two runs, want 1", n)
	}
}

// A ticket is imported with its number and its customer, and the report warns that the
// conversation stayed behind.
func TestTicketsAreImportedWithAThreadWarning(t *testing.T) {
	pool := storetest.DB(t)
	ctx := context.Background()
	d := extendedDump().file(importer.FileTickets,
		"id,tid,userid,title,dept,priority,status,date",
		"500,41221,1,Cannot reach my server,technical,high,Open,2021-06-02 09:30:00",
		"501,41222,9,Unknown customer,general,normal,Open,2021-06-03 10:00:00")

	rep, err := importer.Run(ctx, pool, d.run(true))
	if err != nil {
		t.Fatalf("import: %v", err)
	}

	var (
		number   string
		subject  string
		status   string
		priority string
	)
	if err := pool.QueryRow(ctx,
		`SELECT number, subject, status, priority FROM tickets WHERE number = '41221'`).
		Scan(&number, &subject, &status, &priority); err != nil {
		t.Fatalf("read ticket: %v", err)
	}
	if subject != "Cannot reach my server" || status != "open" || priority != "high" {
		t.Fatalf("ticket = %q/%q/%q", subject, status, priority)
	}
	if !hasWarning(rep, importer.WarnTicketThreadNotImported) {
		t.Fatalf("no warning that the thread was not imported; warnings: %v", warningCodes(rep))
	}

	// A ticket whose customer was not imported is refused, not attached to nobody.
	rej := rejectionFor(t, rep, "501")
	if rej.Code != importer.CodeUnknownClient {
		t.Fatalf("rejection = %+v, want unknown_client", rej)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM tickets`); n != 1 {
		t.Fatalf("%d tickets, want the one with a known customer", n)
	}
}

// Every document a dump may omit is reported as not supplied, so "we forgot to export tickets"
// is visible rather than being indistinguishable from "there were no tickets".
func TestUnsuppliedOptionalDocumentsAreReported(t *testing.T) {
	pool := storetest.DB(t)
	rep, err := importer.Run(context.Background(), pool, extendedDump().run(false))
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	var missing []string
	for _, w := range rep.Warnings {
		if w.Code == "document_not_supplied" {
			missing = append(missing, w.File)
		}
	}
	// The fixture supplies currencies, admins and products; tickets is the one it omits.
	if len(missing) != 1 || missing[0] != importer.FileTickets {
		t.Fatalf("missing-document notes = %v, want exactly tickets", missing)
	}
}

// A currency row whose scale is not a number is refused with the row number, and the import
// continues with the rows that are readable.
func TestBadCurrencyRowIsRejectedWithoutAborting(t *testing.T) {
	pool := storetest.DB(t)
	rep, err := importer.Run(context.Background(), pool, extendedDump().
		file(importer.FileCurrencies,
			"code,name,symbol,scale",
			"ZZA,Alpha,%,two",
			"ZZB,Delta,%,9").
		run(true))
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	// The codes are unique to this test because `currencies` is a reference table: the ISO rows
	// survive the per-test truncate, so a code shared with another fixture would leak between tests.
	if n := countRows(t, pool, `SELECT count(*) FROM currencies WHERE code IN ('ZZA', 'ZZB')`); n != 0 {
		t.Fatalf("%d unreadable currencies were created", n)
	}
	var codes []string
	for _, r := range rep.Rejections {
		if strings.HasPrefix(r.LegacyID, "ZZA") || strings.HasPrefix(r.LegacyID, "ZZB") {
			codes = append(codes, r.Code)
		}
	}
	if len(codes) != 2 || codes[0] != importer.CodeInvalidValue {
		t.Fatalf("rejections = %v, want two invalid_value refusals", codes)
	}
	// The rest of the dump still went in: a bad currency does not abort the customers.
	if n := countRows(t, pool, `SELECT count(*) FROM customers`); n != 1 {
		t.Fatalf("%d customers were imported alongside a bad currency row", n)
	}
}
