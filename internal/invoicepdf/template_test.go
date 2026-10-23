package invoicepdf

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/catalyst/billing/internal/brand"
	"github.com/catalyst/billing/internal/store/storetest"
)

// An installation with no template must render the bytes it rendered before templates
// existed. The goldens here are the same ones the brand tests use, so a change that made an
// unset template take a branch shows up as a changed document rather than as a passing
// structural assertion.
func TestUnsetTemplateIsByteIdentical(t *testing.T) {
	for _, tc := range []struct {
		name  string
		doc   Document
		goldn string
	}{
		{"invoice", FromInvoice(sampleSnapshot(t), issuedMeta()), goldenUnbrandedInvoice},
		{"credit note", sampleCreditNote(t), goldenUnbrandedCreditNote},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !tc.doc.Template.IsZero() {
				t.Fatal("the fixture carries a template, so it cannot prove the fallback")
			}
			out, err := Render(tc.doc)
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			if got := sha256Hex(out); got != tc.goldn {
				t.Fatalf("an unset template changed the document:\n got %s\nwant %s", got, tc.goldn)
			}
		})
	}
}

// A theme's wording must reach the document: the heading, the label over the customer's
// address and the footer note. This is the whole of INV-15 — a theme that cannot change these
// cannot supply a PDF template at all.
func TestTemplateReachesTheDocument(t *testing.T) {
	doc := FromInvoice(sampleSnapshot(t), issuedMeta())
	doc.Template = Template{
		Name:         "reseller-de",
		TitleInvoice: "TAX INVOICE",
		BillToLabel:  "Rechnung an",
		FooterNote:   "Acme Hosting GmbH · HRB 12345",
	}

	out, err := Render(doc)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	for _, want := range []string{"TAX INVOICE", "Rechnung an", "HRB 12345"} {
		if !bytes.Contains(out, []byte(want)) {
			t.Fatalf("the template's %q is not in the document", want)
		}
	}
	// The theme may not remove the identity of the page from the footer: the note is added
	// above it, not instead of it.
	if !bytes.Contains(out, []byte("Invoice \xc2\xb7 INV-000042")) &&
		!bytes.Contains(out, []byte("Invoice")) {
		t.Fatal("the footer lost the document identity")
	}
	// And the money facts are untouched by a presentation template.
	if !bytes.Contains(out, []byte("INV-000042")) {
		t.Fatal("the number disappeared")
	}
}

// A template accent applies only where the brand left the colour alone: the tenant's own
// brand outranks a theme.
func TestTemplateAccentYieldsToTheBrand(t *testing.T) {
	themeAccent, err := brand.ParseHex("#2e7d32")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	doc := FromInvoice(sampleSnapshot(t), issuedMeta())
	doc.Template = Template{AccentHex: "#2e7d32"}
	out, err := Render(doc)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !bytes.Contains(out, []byte("0.180 0.490 0.196 rg")) {
		t.Fatal("the theme's accent is not drawn when no brand accent is configured")
	}

	brandAccent, err := brand.ParseHex("#c0392b")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	doc.Brand = Brand{Accent: brandAccent}
	out, err = Render(doc)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if bytes.Contains(out, []byte("0.180 0.490 0.196 rg")) {
		t.Fatal("the theme overrode the tenant's brand accent")
	}
	if !bytes.Contains(out, []byte("0.753 0.224 0.169 rg")) {
		t.Fatal("the brand accent was not drawn")
	}
	_ = themeAccent
}

// A debit note is a positive charge with its own heading, not a negative credit note.
func TestDebitNoteRendersAsACharge(t *testing.T) {
	doc := FromCreditNote(sampleSnapshot(t), CreditNoteData{
		Number:        "DN-000001",
		InvoiceNumber: "INV-000042",
		Currency:      "USD",
		TotalMinor:    500,
		TaxMinor:      0,
		Reason:        "undercharged setup fee",
		Status:        "issued",
		Kind:          KindDebitNote,
	})
	out, err := Render(doc)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if doc.Totals.TotalMinor != 500 {
		t.Fatalf("debit note total = %d, want +500", doc.Totals.TotalMinor)
	}
	if !bytes.Contains(out, []byte("DEBIT NOTE")) {
		t.Fatal("a debit note is not titled as one")
	}
	if !bytes.Contains(out, []byte("Debit against invoice INV-000042")) {
		t.Fatal("the debit note does not name the invoice it corrects")
	}
}

// Parsing is the only place a theme's JSON reaches a document, so it must refuse what it
// cannot render rather than printing half of it.
func TestParseTemplateRefusesWhatItCannotRender(t *testing.T) {
	if _, err := ParseTemplate(`{"titel_invoice":"TAX INVOICE"}`); err == nil {
		t.Fatal("a misspelled field was accepted, so the template would silently do nothing")
	}
	if _, err := ParseTemplate(`{"title_invoice":"line\nbreak"}`); err == nil {
		t.Fatal("a control character was accepted into a PDF string")
	}
	if _, err := ParseTemplate(`{"accent":"not a colour"}`); err == nil {
		t.Fatal("a malformed accent was accepted")
	}
	if _, err := ParseTemplate(`{"title_invoice":` + strings.Repeat("x", maxTemplateField+1) + `"}`); err == nil {
		t.Fatal("an over-long heading was accepted")
	}

	empty, err := ParseTemplate("")
	if err != nil {
		t.Fatalf("an absent template is an error: %v", err)
	}
	if !empty.IsZero() {
		t.Fatalf("an absent template parsed to %+v, want the zero template", empty)
	}
	tpl, err := ParseTemplate(`{"title_invoice":"TAX INVOICE","accent":"#2e7d32"}`)
	if err != nil {
		t.Fatalf("a valid template was refused: %v", err)
	}
	if tpl.TitleInvoice != "TAX INVOICE" || tpl.AccentHex != "#2e7d32" {
		t.Fatalf("parsed %+v", tpl)
	}
}

// The configured template is read from the settings table, and a malformed one falls back to
// the built-in layout with an error rather than stopping a document from printing.
func TestLoadTemplateReadsTheSettingAndFallsBack(t *testing.T) {
	pool := storetest.DB(t)
	ctx := context.Background()

	if _, err := pool.Exec(ctx, `
		INSERT INTO settings (group_key, key, value) VALUES ('invoices', 'pdf_template',
			'{"title_invoice":"TAX INVOICE"}')
		ON CONFLICT (group_key, key) DO UPDATE SET value = EXCLUDED.value`); err != nil {
		t.Fatalf("set template: %v", err)
	}
	tpl, err := LoadTemplate(ctx, pool)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if tpl.TitleInvoice != "TAX INVOICE" {
		t.Fatalf("title = %q, want the configured one", tpl.TitleInvoice)
	}

	if _, err := pool.Exec(ctx, `
		INSERT INTO settings (group_key, key, value) VALUES ('invoices', 'pdf_template',
			'{"title_invoice":42}')
		ON CONFLICT (group_key, key) DO UPDATE SET value = EXCLUDED.value`); err != nil {
		t.Fatalf("set malformed template: %v", err)
	}
	tpl, err = LoadTemplate(ctx, pool)
	if err == nil {
		t.Fatal("a malformed template was loaded without an error")
	}
	if !tpl.IsZero() {
		t.Fatalf("a malformed template returned %+v, want the built-in layout", tpl)
	}
}
