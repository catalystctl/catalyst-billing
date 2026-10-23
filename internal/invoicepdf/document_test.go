package invoicepdf

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"
	"time"

	"github.com/catalyst/billing/internal/billing"
	"github.com/catalyst/billing/internal/brand"
	"github.com/catalyst/billing/internal/money"
)

var usd = money.MustLookupCurrency("USD")

// sampleSnapshot is a small but complete invoice snapshot: two lines, a tax
// rate, a period and terms.
func sampleSnapshot(t *testing.T) billing.Snapshot {
	t.Helper()
	cur := usd
	line := func(desc string, qtyMilli, unit, net, tax, total int64) billing.LineResult {
		return billing.LineResult{
			Description:   desc,
			QuantityMilli: qtyMilli,
			UnitMinor:     unit,
			Net:           money.New(net, cur),
			Tax:           money.New(tax, cur),
			Total:         money.New(total, cur),
			TaxRate:       "0.19",
		}
	}
	lines := []billing.LineResult{
		line("VPS Small monthly", 1000, 1200, 1200, 228, 1428),
		line("Extra backup storage", 1500, 200, 300, 57, 357),
	}
	subtotal := int64(1500)
	tax := int64(285)
	return billing.Snapshot{
		SchemaVersion: 1,
		Merchant: billing.Merchant{
			Name:        "Catalyst Hosting GmbH",
			Address:     "Hauptstrasse 1\n10115 Berlin\nGermany",
			TaxID:       "DE123456789",
			CountryCode: "DE",
		},
		Customer: billing.CustomerDetails{
			ID:             "8c8d0f5a-0000-0000-0000-000000000001",
			Name:           "Ada Lovelace",
			Email:          "ada@example.test",
			BillingAddress: "1 Analytical Way, London, N1 9GU, GB",
			CountryCode:    "GB",
		},
		Lines:    lines,
		Totals:   billing.Totals{Subtotal: money.New(subtotal, cur), Tax: money.New(tax, cur), Total: money.New(subtotal+tax, cur)},
		Currency: "USD",
		Period:   &billing.Period{Start: "2026-11-01", End: "2026-11-30", Key: "2026-11"},
		Terms:    "Payable within 7 days. Late payment may suspend the service.",
		TaxEvidence: billing.TaxEvidence{
			Mode:         "exclusive",
			Policy:       "de-standard",
			Rate:         "0.19",
			Jurisdiction: "DE",
		},
	}
}

func issuedMeta() Meta {
	issued := time.Date(2026, 11, 7, 9, 30, 0, 0, time.UTC)
	due := issued.AddDate(0, 0, 7)
	return Meta{
		Number:     "INV-000042",
		Status:     "open",
		IssuedAt:   &issued,
		DueAt:      &due,
		ClientNote: "Thank you for your business.",
	}
}

// The produced bytes must be a PDF: the header is how every reader recognises the
// format, and a file that does not start with it is not one.
func TestRenderStartsWithPDFHeader(t *testing.T) {
	doc := FromInvoice(sampleSnapshot(t), issuedMeta())
	out, err := Render(doc)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !bytes.HasPrefix(out, []byte("%PDF-")) {
		t.Fatalf("output does not start with %%PDF-: %q", out[:min(16, len(out))])
	}
	if !bytes.HasSuffix(bytes.TrimSpace(out), []byte("%%EOF")) {
		t.Fatal("output does not end with the EOF marker")
	}
	// A reader locates the cross-reference table through startxref; without it
	// the file is unopenable even though every object is present.
	if !bytes.Contains(out, []byte("startxref")) || !bytes.Contains(out, []byte("/Type /Catalog")) {
		t.Fatal("output has no cross-reference table or catalogue")
	}
}

// The invoice number and the formatted total must be in the document. A customer
// pays *this* document, so the two facts that identify it have to be readable.
func TestRenderContainsNumberAndTotal(t *testing.T) {
	doc := FromInvoice(sampleSnapshot(t), issuedMeta())
	out, err := Render(doc)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	text := string(out)
	if !strings.Contains(text, "INV-000042") {
		t.Fatal("the invoice number is not in the document")
	}
	// 1500 + 285 = 1785 minor units, rendered by string insertion.
	if !strings.Contains(text, "17.85 USD") {
		t.Fatalf("the total 17.85 USD is not in the document")
	}
	if !strings.Contains(text, "Ada Lovelace") {
		t.Fatal("the bill-to name is not in the document")
	}
	if !strings.Contains(text, "Catalyst Hosting GmbH") {
		t.Fatal("the merchant block is not in the document")
	}
	// The tax line carries the jurisdiction and the rate, which is the evidence a
	// tax authority reads.
	if !strings.Contains(text, "DE 19%") {
		t.Fatalf("the tax jurisdiction and rate are not in the document")
	}
	if !strings.Contains(text, "Thank you for your business.") {
		t.Fatal("the client-visible note is not in the document")
	}
	if !strings.Contains(text, "Payable within 7 days") {
		t.Fatal("the terms are not in the document")
	}
}

// A description is customer-influenced data. An unescaped parenthesis or
// backslash in a PDF literal string corrupts the document, so the escape must be
// provably applied.
func TestRenderEscapesParenthesesAndBackslash(t *testing.T) {
	snap := sampleSnapshot(t)
	hostile := `Widget (large) \ deluxe (2026)`
	snap.Lines[0].Description = hostile
	snap.Lines[0].Net = money.New(1200, usd)

	doc := FromInvoice(snap, issuedMeta())
	out, err := Render(doc)
	if err != nil {
		t.Fatalf("render: %v", err)
	}

	want := `Widget \(large\) \\ deluxe \(2026\)`
	if !strings.Contains(string(out), want) {
		t.Fatalf("the description was not escaped:\nwant %q in the content stream", want)
	}
	// The raw, unescaped form must not appear: if it did, a reader would end the
	// string at the first ')' and interpret the rest as operators.
	if strings.Contains(string(out), "("+hostile+")") {
		t.Fatal("the raw description appears unescaped in a string literal")
	}

	// Every literal in the content stream must be balanced once escapes are
	// honoured. This is the property a corrupting description breaks.
	if err := checkLiteralsBalanced(out); err != nil {
		t.Fatalf("unbalanced string literals: %v", err)
	}
}

// checkLiteralsBalanced walks the stream objects and verifies that parentheses
// balance, treating a backslash as escaping the next byte.
func checkLiteralsBalanced(pdf []byte) error {
	inString := false
	depth := 0
	for i := 0; i < len(pdf); i++ {
		b := pdf[i]
		if inString && b == '\\' {
			i++ // skip the escaped byte
			continue
		}
		switch b {
		case '(':
			inString = true
			depth++
		case ')':
			if !inString || depth == 0 {
				return errUnbalanced
			}
			depth--
			if depth == 0 {
				inString = false
			}
		}
	}
	if depth != 0 {
		return errUnbalanced
	}
	return nil
}

type unbalancedError struct{}

func (unbalancedError) Error() string { return "a parenthesis closes a literal that was never opened" }

var errUnbalanced = unbalancedError{}

// A credit note renders with negative amounts and its own number.
func TestRenderCreditNoteIsNegative(t *testing.T) {
	base := sampleSnapshot(t)
	issued := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	doc := FromCreditNote(base, CreditNoteData{
		Number:        "CN-000007",
		InvoiceNumber: "INV-000042",
		IssuedAt:      &issued,
		Currency:      "USD",
		TotalMinor:    357,
		TaxMinor:      57,
		Reason:        "backup storage removed",
		Status:        "issued",
	})

	out, err := Render(doc)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	text := string(out)
	if !bytes.HasPrefix(out, []byte("%PDF-")) {
		t.Fatal("the credit note is not a PDF")
	}
	if !strings.Contains(text, "CREDIT NOTE") {
		t.Fatal("the credit note is not titled as one")
	}
	if !strings.Contains(text, "CN-000007") {
		t.Fatal("the credit note number is missing")
	}
	if !strings.Contains(text, "INV-000042") {
		t.Fatal("the credit note does not name the invoice it corrects")
	}
	if !strings.Contains(text, "-3.57 USD") {
		t.Fatalf("the total is not negative: want -3.57 USD")
	}
	if !strings.Contains(text, "-3.00 USD") {
		t.Fatalf("the net is not negative: want -3.00 USD")
	}
	if !strings.Contains(text, "-0.57 USD") {
		t.Fatalf("the tax is not negative: want -0.57 USD")
	}
	// The positive total must not appear as a standalone amount, or a reader
	// would think the customer is owed nothing.
	if strings.Contains(text, "(3.57 USD)") {
		t.Fatal("the credit total appears without its sign")
	}
}

// FormatAmount must never introduce a float, and must place the mark from the
// currency's own scale.
func TestFormatAmount(t *testing.T) {
	cases := []struct {
		minor int64
		code  string
		want  string
	}{
		{0, "USD", "0.00 USD"},
		{5, "USD", "0.05 USD"},
		{99, "USD", "0.99 USD"},
		{1234, "USD", "12.34 USD"},
		{100000, "USD", "1,000.00 USD"},
		{123456789, "USD", "1,234,567.89 USD"},
		{-1234, "USD", "-12.34 USD"},
		{-1, "USD", "-0.01 USD"},
		{1234, "JPY", "1,234 JPY"}, // scale 0
		{1234, "KWD", "1.234 KWD"}, // scale 3
		{1000, "EUR", "10.00 EUR"},
		// A currency the ISO table does not list is rendered at scale 2, which is
		// what the schema default assumes.
		{1234, "ZZZ", "12.34 ZZZ"},
	}
	for _, tc := range cases {
		if got := FormatAmount(tc.minor, tc.code); got != tc.want {
			t.Errorf("FormatAmount(%d, %q) = %q, want %q", tc.minor, tc.code, got, tc.want)
		}
	}
}

// FormatQuantity trims trailing zeros without changing the integer part.
func TestFormatQuantity(t *testing.T) {
	cases := map[int64]string{
		1000:  "1",
		1500:  "1.5",
		1234:  "1.234",
		1:     "0.001",
		100:   "0.1",
		0:     "0",
		-1500: "-1.5",
	}
	for in, want := range cases {
		if got := FormatQuantity(in); got != want {
			t.Errorf("FormatQuantity(%d) = %q, want %q", in, got, want)
		}
	}
}

// A rate is exact evidence. 19% must print as 19%, and a repeating rate must not
// print higher than it is.
func TestFormatRate(t *testing.T) {
	cases := map[string]string{
		"0.19":  "19%",
		"19%":   "19%",
		"0.2":   "20%",
		"0":     "0%",
		"1/5":   "20%",
		"0.075": "7.5%",
		"1/3":   "33.3333%", // truncated, never 33.3334%
	}
	for in, want := range cases {
		if got := formatRate(in); got != want {
			t.Errorf("formatRate(%q) = %q, want %q", in, got, want)
		}
	}
}

// A long description wraps instead of running off the page, and a single
// unbroken token is broken rather than dropped.
func TestWrapAndTruncate(t *testing.T) {
	long := strings.Repeat("word ", 60)
	lines := wrap(Helvetica, bodySize, long, 100)
	if len(lines) < 5 {
		t.Fatalf("a long description produced %d lines, want several", len(lines))
	}
	for _, line := range lines {
		if w := TextWidth(Helvetica, bodySize, line); w > 100.5 {
			t.Fatalf("line %q is %.2f wide, over the 100pt column", line, w)
		}
	}

	unbroken := strings.Repeat("x", 500)
	for _, line := range wrap(Helvetica, bodySize, unbroken, 100) {
		if w := TextWidth(Helvetica, bodySize, line); w > 100.5 {
			t.Fatalf("unbroken line is %.2f wide, over the column", w)
		}
	}

	if got := truncate(Helvetica, bodySize, "short", 200); got != "short" {
		t.Fatalf("truncate shortened a string that fits: %q", got)
	}
	if got := truncate(Helvetica, bodySize, strings.Repeat("y", 100), 40); !strings.HasSuffix(got, "...") {
		t.Fatalf("truncate did not mark the elision: %q", got)
	}
}

// A document with no lines or no currency is a programming error, not an empty
// page a customer could be sent.
func TestRenderRejectsEmptyDocuments(t *testing.T) {
	if _, err := Render(Document{}); err == nil {
		t.Fatal("an empty document was rendered")
	}
	doc := FromInvoice(sampleSnapshot(t), issuedMeta())
	doc.Lines = nil
	if _, err := Render(doc); err == nil {
		t.Fatal("a document with no lines was rendered")
	}
}

// A proforma has no number, and the document must say so rather than printing a
// blank field a customer would read as a mistake.
func TestProformaSaysNumberIsNotAssigned(t *testing.T) {
	meta := issuedMeta()
	meta.Number = ""
	meta.Proforma = true
	doc := FromInvoice(sampleSnapshot(t), meta)
	out, err := Render(doc)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	text := string(out)
	if !strings.Contains(text, "PROFORMA INVOICE") {
		t.Fatal("a proforma is not titled as one")
	}
	if !strings.Contains(text, "not yet assigned") {
		t.Fatal("a proforma does not say the number is unassigned")
	}
	if !strings.Contains(text, "not a tax invoice") {
		t.Fatal("a proforma does not state that it is not a tax invoice")
	}
}

// A void invoice prints the reason, so the copy the customer holds explains
// itself.
func TestVoidInvoicePrintsReason(t *testing.T) {
	meta := issuedMeta()
	meta.Status = "void"
	meta.VoidReason = "issued in error"
	doc := FromInvoice(sampleSnapshot(t), meta)
	out, err := Render(doc)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(string(out), "issued in error") {
		t.Fatal("the void reason is not on the document")
	}
}

// A very long invoice paginates, and every page carries the number.
func TestLongInvoicePaginates(t *testing.T) {
	snap := sampleSnapshot(t)
	snap.Lines = nil
	var subtotal int64
	for i := 0; i < 120; i++ {
		snap.Lines = append(snap.Lines, billing.LineResult{
			Description:   "Monthly hosting plan with a reasonably long descriptive name",
			QuantityMilli: 1000,
			UnitMinor:     1200,
			Net:           money.New(1200, usd),
			Tax:           money.New(228, usd),
			Total:         money.New(1428, usd),
			TaxRate:       "0.19",
		})
		subtotal += 1200
	}
	snap.Totals = billing.Totals{
		Subtotal: money.New(subtotal, usd),
		Tax:      money.New(subtotal*19/100, usd),
		Total:    money.New(subtotal+subtotal*19/100, usd),
	}

	out, err := Render(FromInvoice(snap, issuedMeta()))
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	pages := bytes.Count(out, []byte("/Type /Page "))
	if pages < 2 {
		t.Fatalf("120 lines rendered on %d page(s), want more than one", pages)
	}
	if got := bytes.Count(out, []byte("INV-000042")); got < pages {
		t.Fatalf("the number appears %d times for %d pages; every page must carry it", got, pages)
	}
	if !bytes.Contains(out, []byte("/Count "+itoa(pages))) {
		t.Fatal("the page tree count does not match the number of pages")
	}
}

// SafeFilename keeps a configured prefix from shaping a header or a path.
func TestSafeFilename(t *testing.T) {
	cases := map[string]string{
		"INV-000042":    "INV-000042",
		"CN-000007":     "CN-000007",
		"../etc/passwd": "etc_passwd", // leading dots stripped: no hidden or relative path
		`a"b`:           "a_b",
		"":              "document",
		"///":           "document",
		"INV 000042":    "INV_000042",
	}
	for in, want := range cases {
		if got := SafeFilename(in); got != want {
			t.Errorf("SafeFilename(%q) = %q, want %q", in, got, want)
		}
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// A credit note must not inherit the invoice's payment terms: a credit note is
// not payable, so printing "payable within 7 days" on one is false.
func TestCreditNoteDoesNotInheritPaymentTerms(t *testing.T) {
	base := sampleSnapshot(t)
	issued := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	out, err := Render(FromCreditNote(base, CreditNoteData{
		Number: "CN-000008", InvoiceNumber: "INV-000042", IssuedAt: &issued,
		Currency: "USD", TotalMinor: 100, TaxMinor: 0, Reason: "goodwill", Status: "issued",
	}))
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if bytes.Contains(out, []byte("Payable within 7 days")) {
		t.Fatal("the credit note inherited the invoice's payment terms")
	}
}

// The tax label must degrade honestly: a document with no evidence prints "Tax",
// not "Tax ()" or "Tax (  )".
func TestTaxLabelDegrades(t *testing.T) {
	cases := []struct {
		ev   billing.TaxEvidence
		want string
	}{
		{billing.TaxEvidence{Rate: "0.19", Jurisdiction: "DE"}, "Tax (DE 19%)"},
		{billing.TaxEvidence{Rate: "0.19"}, "Tax (19%)"},
		{billing.TaxEvidence{Jurisdiction: "DE"}, "Tax (DE)"},
		{billing.TaxEvidence{}, "Tax"},
	}
	for _, tc := range cases {
		r := &renderer{doc: Document{TaxEvidence: tc.ev}}
		if got := r.taxLabel(); got != tc.want {
			t.Errorf("taxLabel(%+v) = %q, want %q", tc.ev, got, tc.want)
		}
	}
}

// A pathological address must not print off the page: the block is bounded and the
// elision is marked, so nothing disappears without a trace.
func TestOverlongAddressIsBoundedAndMarked(t *testing.T) {
	snap := sampleSnapshot(t)
	snap.Merchant.Address = strings.Repeat("Very long address line number one\n", 40)
	snap.Customer.BillingAddress = strings.Repeat("Long billing address line\n", 40)

	out, err := Render(FromInvoice(snap, issuedMeta()))
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	// The marker is a PDF literal, so its parentheses are escaped on the page.
	if !bytes.Contains(out, []byte(`\(continued\)`)) {
		t.Fatal("an over-long address was truncated without marking it")
	}
	// The document must still be one page: the header is bounded, not paginated,
	// and the table has room below it.
	if pages := bytes.Count(out, []byte("/Type /Page ")); pages != 1 {
		t.Fatalf("an over-long address produced %d pages", pages)
	}
}

// A discount line (migration 0022, `invoice_lines.line_kind = 'discount'`) renders
// as a negative line with zero tax, and the printed arithmetic reconciles: the
// subtotal is the sum of the line nets, so a customer can check the page.
//
// The renderer needs no knowledge of `line_kind`: the snapshot carries signed
// amounts, and a discount is simply a line whose numbers are negative. That is the
// property this test pins — if the renderer ever starts assuming non-negative
// lines, a discounted invoice becomes unprintable or, worse, prints a total that
// disagrees with its own lines.
func TestRenderDiscountLine(t *testing.T) {
	snap := sampleSnapshot(t)
	cur := usd
	snap.Lines = []billing.LineResult{
		{
			Description: "VPS Small monthly", QuantityMilli: 1000, UnitMinor: 10000,
			Net: money.New(10000, cur), Tax: money.New(1900, cur),
			Total: money.New(11900, cur), TaxRate: "0.19",
		},
		{
			Description: "Coupon SAVE20", QuantityMilli: 1000, UnitMinor: -2000,
			Net: money.New(-2000, cur), Tax: money.New(0, cur),
			Total: money.New(-2000, cur), TaxRate: "0",
		},
	}
	snap.Totals = billing.Totals{
		Subtotal: money.New(8000, cur),
		Tax:      money.New(1520, cur),
		Total:    money.New(9520, cur),
	}

	out, err := Render(FromInvoice(snap, issuedMeta()))
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	text := string(out)
	for _, want := range []string{
		"Coupon SAVE20",
		"-20.00 USD", // the discount's unit, net and total
		"80.00 USD",  // subtotal: 100.00 - 20.00
		"15.20 USD",  // tax on the discounted net
		"95.20 USD",  // total
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("%q is not on the document", want)
		}
	}
	// The undiscounted charge still prints at its catalog price, so the customer can
	// see what was discounted from.
	if !strings.Contains(text, "100.00 USD") {
		t.Fatal("the charge line does not print its catalog price")
	}
	// And the arithmetic on the page must reconcile.
	if snap.Totals.Subtotal.Minor() != snap.Lines[0].Net.Minor()+snap.Lines[1].Net.Minor() {
		t.Fatal("the test fixture's subtotal is not the sum of its line nets")
	}
}

// ---------------------------------------------------------------- branding --

// The bytes an unbranded document produced before branding existed.
//
// These are hashes, not descriptions, and that is the point: "an unset brand falls back to
// the current appearance" is a claim about *every* byte, including the cross-reference
// offsets and the resource dictionary. A test that asserted "no colour operators appear"
// would pass while a stray `q`/`Q` pair or an unused XObject shifted every offset in the
// file. The hash was recorded from the renderer with no brand support at all, so it cannot
// be made to agree with a changed layout.
const (
	goldenUnbrandedInvoice    = "07a2e2b875912c04df1c0f7da6a9bc1ccebe60fd604d1bb97e6dec7bcdf27e1f"
	goldenUnbrandedCreditNote = "2d883d81e9d25c543079afb8ced9217574d850ab6b1984abb61cc0d0612e7c38"
)

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// sampleCreditNote is the credit note the golden hash covers.
func sampleCreditNote(t *testing.T) Document {
	t.Helper()
	issued := time.Date(2026, 11, 8, 10, 0, 0, 0, time.UTC)
	return FromCreditNote(sampleSnapshot(t), CreditNoteData{
		Number:        "CN-000007",
		InvoiceNumber: "INV-000042",
		IssuedAt:      &issued,
		Currency:      "USD",
		TotalMinor:    357,
		TaxMinor:      57,
		Reason:        "Service credit",
		Status:        "issued",
	})
}

// An unset brand must be a no-op, byte for byte.
//
// This is the test the acceptance criterion names. It is a hash rather than a structural
// assertion because the requirement is byte identity, and because a structural assertion
// would keep passing through exactly the changes that break it — a reserved object number,
// an extra graphics-state pair, a resource added to the page dictionary.
func TestUnsetBrandIsByteIdentical(t *testing.T) {
	for _, tc := range []struct {
		name  string
		doc   Document
		goldn string
	}{
		{"invoice", FromInvoice(sampleSnapshot(t), issuedMeta()), goldenUnbrandedInvoice},
		{"credit note", sampleCreditNote(t), goldenUnbrandedCreditNote},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !tc.doc.Brand.IsZero() {
				t.Fatal("the fixture carries a brand, so it cannot prove the unbranded fallback")
			}
			out, err := Render(tc.doc)
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			if got := sha256Hex(out); got != tc.goldn {
				t.Fatalf("an unset brand changed the document:\n got %s\nwant %s", got, tc.goldn)
			}
			// And no brand operator may appear: a colour would mean the fallback took a
			// branch that a hash collision could otherwise hide.
			for _, op := range []string{" rg ", "/XObject", " Do\n", " RG "} {
				if strings.Contains(string(out), op) {
					t.Fatalf("an unbranded document contains %q", op)
				}
			}
		})
	}
}

// A configured brand must reach the document: the accent, the name and the logo.
func TestBrandReachesTheDocument(t *testing.T) {
	accent, err := brand.ParseHex("#c0392b")
	if err != nil {
		t.Fatalf("parse accent: %v", err)
	}
	logo := mustLogo(t, logoPNG(t, 40, 20, 0x1e, 0x88, 0xe5))

	doc := FromInvoice(sampleSnapshot(t), issuedMeta())
	doc.Brand = Brand{Name: "Acme Hosting", Accent: accent, Logo: logo}

	out, renderErr := Render(doc)
	if renderErr != nil {
		t.Fatalf("render: %v", renderErr)
	}
	text := string(out)

	// The name replaces the merchant name in the letterhead and the footer.
	if !strings.Contains(text, "Acme Hosting") {
		t.Fatal("the brand name is not on the document")
	}
	if strings.Contains(text, "Catalyst Hosting GmbH") {
		t.Fatal("the merchant name was printed even though a brand name was configured")
	}
	// The accent is drawn: a filled band and a coloured rule, in the stored colour.
	if !strings.Contains(text, "0.753 0.224 0.169 rg") {
		t.Fatal("the accent colour does not appear in the content stream")
	}
	// The logo is embedded once and referenced by every page.
	if !strings.Contains(text, "/Subtype /Image") {
		t.Fatal("the logo is not embedded as an image")
	}
	if !strings.Contains(text, "/XObject << /Im1") {
		t.Fatal("the page resources do not reference the logo")
	}
	if strings.Count(text, "/Subtype /Image") != 1 {
		t.Fatal("the logo is embedded more than once")
	}
	// The document is still a valid PDF: branding must not corrupt the structure.
	if !bytes.HasPrefix(out, []byte("%PDF-")) || !bytes.HasSuffix(bytes.TrimSpace(out), []byte("%%EOF")) {
		t.Fatal("a branded document is not a well-formed PDF")
	}
	// The content stream must still escape its literals. The check walks text, so it runs on
	// the colour-and-name document: the image stream is compressed binary and would be read
	// as operators, which is a property of the checker rather than of the document.
	textOnly := doc
	textOnly.Brand.Logo = nil
	textOut, textErr := Render(textOnly)
	if textErr != nil {
		t.Fatalf("render without logo: %v", textErr)
	}
	if err := checkLiteralsBalanced(textOut); err != nil {
		t.Fatalf("a branded document has unbalanced string literals: %v", err)
	}
	// The facts of the document are untouched.
	for _, want := range []string{"INV-000042", "17.85 USD", "Ada Lovelace", "DE 19%"} {
		if !strings.Contains(text, want) {
			t.Fatalf("%q disappeared from the branded document", want)
		}
	}
}

// A pale accent is a legitimate brand colour and an illegible heading, so the title is
// darkened until it reads on paper. This is the case a real tenant produces by picking
// yellow.
func TestPaleAccentIsDarkenedForTheTitle(t *testing.T) {
	pale, err := brand.ParseHex("#f1e05a")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if ratio := brand.Contrast(pale, brand.Colour{R: 0xff, G: 0xff, B: 0xff}); ratio >= brand.MinTextContrast {
		t.Fatalf("the fixture is not pale: contrast %.2f already passes", ratio)
	}

	doc := FromInvoice(sampleSnapshot(t), issuedMeta())
	doc.Brand = Brand{Accent: pale}
	out, renderErr := Render(doc)
	if renderErr != nil {
		t.Fatalf("render: %v", renderErr)
	}

	// The title is drawn in the readable relative, not the stored colour.
	readable := brand.Readable(pale, brand.Colour{R: 0xff, G: 0xff, B: 0xff})
	if ratio := brand.Contrast(readable, brand.Colour{R: 0xff, G: 0xff, B: 0xff}); ratio < brand.MinTextContrast {
		t.Fatalf("the title colour still fails contrast: %.2f", ratio)
	}
	titleOp := fmt.Sprintf("q %.3f %.3f %.3f rg BT /F2 18.00 Tf",
		float64(readable.R)/255, float64(readable.G)/255, float64(readable.B)/255)
	if !strings.Contains(string(out), titleOp) {
		t.Fatal("the document title is not drawn in a readable version of the accent")
	}
	// The band still uses the accent exactly as stored: it is a shape, not text, so it needs
	// no contrast adjustment and the brand colour is honoured where it is safe.
	bandOp := fmt.Sprintf("q %.3f %.3f %.3f rg",
		float64(pale.R)/255, float64(pale.G)/255, float64(pale.B)/255)
	if !strings.Contains(string(out), bandOp) {
		t.Fatal("the accent band does not use the stored colour")
	}
}

// A logo that cannot be decoded must not stop the invoice. A document is owed to a customer
// whether or not the file behind its letterhead is still readable.
func TestUndecodableLogoFallsBackToNoLogo(t *testing.T) {
	accent, _ := brand.ParseHex("#3b5bdb")
	doc := FromInvoice(sampleSnapshot(t), issuedMeta())
	doc.Brand = Brand{
		Name:   "Acme Hosting",
		Accent: accent,
		Logo:   &Logo{ContentType: "image/png", Bytes: []byte("not an image")},
	}
	out, err := Render(doc)
	if err != nil {
		t.Fatalf("a corrupt logo failed the document: %v", err)
	}
	text := string(out)
	if strings.Contains(text, "/Subtype /Image") {
		t.Fatal("a corrupt logo was embedded")
	}
	// The rest of the brand still applies: one broken field is not a broken brand.
	if !strings.Contains(text, "Acme Hosting") {
		t.Fatal("the brand name was dropped along with the logo")
	}
	if !bytes.HasPrefix(out, []byte("%PDF-")) {
		t.Fatal("the document is not a PDF")
	}
}

// An SVG logo is a document that can carry a script. The renderer cannot draw one, so it
// must be treated as absent rather than embedded as bytes.
func TestSVGLogoIsNotEmbedded(t *testing.T) {
	doc := FromInvoice(sampleSnapshot(t), issuedMeta())
	doc.Brand = Brand{Logo: &Logo{
		ContentType: "image/svg+xml",
		Bytes:       []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`),
	}}
	out, err := Render(doc)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if strings.Contains(string(out), "alert(1)") || strings.Contains(string(out), "image/svg") {
		t.Fatal("an SVG logo reached the document")
	}
}

// mustLogo wraps encoded bytes as a stored logo.
func mustLogo(t *testing.T, b []byte) *Logo {
	t.Helper()
	if len(b) == 0 {
		t.Fatal("the fixture encoder produced no bytes")
	}
	return &Logo{ContentType: "image/png", Bytes: b}
}

// logoPNG encodes a solid-colour PNG of the given size. It is generated rather than checked
// in so the test carries no binary fixture and the size is visible at the call site.
func logoPNG(t *testing.T, w, h int, r, g, b uint8) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: r, G: g, B: b, A: 0xff})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode logo: %v", err)
	}
	return buf.Bytes()
}
