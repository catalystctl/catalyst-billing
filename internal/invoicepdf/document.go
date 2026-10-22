package invoicepdf

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/catalyst/billing/internal/billing"
)

// Kind is which document is being printed. It decides the title, and for a
// credit note it decides that every amount is negative.
type Kind int

const (
	// KindInvoice is a tax invoice.
	KindInvoice Kind = iota
	// KindCreditNote is the counter-document that corrects an invoice.
	KindCreditNote
)

// ErrEmptyDocument is returned when there is nothing to print.
var ErrEmptyDocument = errors.New("invoicepdf: nothing to render")

// Meta carries the facts about the document that are not in the invoice
// snapshot.
//
// The snapshot is the frozen evidence: merchant, customer, lines, totals, tax
// evidence, period and terms. What it does not carry is the *document's own*
// identity and lifecycle, because those live in columns: the number (which a
// proforma does not have), the status, the timestamps, the client-visible note,
// the void reason and the bill-to text.
//
// BillToText is deliberately not snapshotted. It is a presentation block an
// operator configures once (Paymenter's `invoices.bill_to_text`) and may override
// per customer, in the same category as the merchant's letterhead: it says "make
// cheques payable to X", not what was charged. Freezing it would require a
// snapshot schema change owned by internal/billing.
type Meta struct {
	Number   string
	Status   string
	IssuedAt *time.Time
	DueAt    *time.Time
	// Proforma marks a provisional document. It has no number and says so.
	Proforma bool
	// ClientNote is printed when non-empty. The caller decides whether the
	// installation shows client notes at all (`invoices.notes_client_visible`),
	// so the policy lives on the read path and this package stays a renderer.
	ClientNote string
	// VoidReason is printed as a void banner when Status is "void".
	VoidReason string
	// BillToText is the free-text block above the customer's address.
	BillToText string
	// InvoiceNumber, on a credit note, is the number of the invoice corrected.
	InvoiceNumber string
}

// Line is one rendered row.
//
// Amounts are integer minor units so a negative credit note needs no special
// case: FormatAmount keeps the sign.
type Line struct {
	Description   string
	QuantityMilli int64
	UnitMinor     int64
	NetMinor      int64
	TaxMinor      int64
	TotalMinor    int64
	TaxRate       string
}

// Totals is the money summary printed under the table.
type Totals struct {
	SubtotalMinor int64
	TaxMinor      int64
	TotalMinor    int64
}

// Document is a fully resolved document, independent of where it came from.
type Document struct {
	Kind     Kind
	Meta     Meta
	Currency string
	Merchant billing.Merchant
	Customer billing.CustomerDetails
	Period   *billing.Period
	Terms    string
	Lines    []Line
	Totals   Totals
	// TaxEvidence is printed next to the tax total so the rate and the
	// jurisdiction that produced it are on the document itself.
	TaxEvidence billing.TaxEvidence
}

// FromInvoice builds a Document from an issued invoice's snapshot and its
// columns.
func FromInvoice(snap billing.Snapshot, meta Meta) Document {
	lines := make([]Line, 0, len(snap.Lines))
	for _, l := range snap.Lines {
		lines = append(lines, Line{
			Description:   l.Description,
			QuantityMilli: l.QuantityMilli,
			UnitMinor:     l.UnitMinor,
			NetMinor:      l.Net.Minor(),
			TaxMinor:      l.Tax.Minor(),
			TotalMinor:    l.Total.Minor(),
			TaxRate:       l.TaxRate,
		})
	}
	return Document{
		Kind:     KindInvoice,
		Meta:     meta,
		Currency: snap.Currency,
		Merchant: snap.Merchant,
		Customer: snap.Customer,
		Period:   snap.Period,
		Terms:    snap.Terms,
		Lines:    lines,
		Totals: Totals{
			SubtotalMinor: snap.Totals.Subtotal.Minor(),
			TaxMinor:      snap.Totals.Tax.Minor(),
			TotalMinor:    snap.Totals.Total.Minor(),
		},
		TaxEvidence: snap.TaxEvidence,
	}
}

// CreditNoteData is the part of a credit note the document needs.
type CreditNoteData struct {
	Number string
	// InvoiceNumber is the invoice the note corrects. A credit note that does
	// not name its invoice is a document a customer cannot reconcile.
	InvoiceNumber string
	IssuedAt      *time.Time
	Currency      string
	TotalMinor    int64
	TaxMinor      int64
	Reason        string
	Status        string
}

// FromCreditNote builds a credit-note Document.
//
// The merchant and the customer come from the invoice the note corrects: a credit
// note is not a standalone document, it is a correction of a named invoice, and a
// customer who receives one must be able to match it to the document it reduces.
// The base snapshot supplies that, and it is why a credit note needs no separate
// customer snapshot of its own.
//
// The note renders as a single negative line. One line is honest: a credit note
// corrects an amount, not a list of prices, and inventing per-item negatives from
// the original invoice would state a breakdown the credit never asserted.
func FromCreditNote(base billing.Snapshot, cn CreditNoteData) Document {
	meta := Meta{
		Number:        cn.Number,
		Status:        cn.Status,
		IssuedAt:      cn.IssuedAt,
		InvoiceNumber: cn.InvoiceNumber,
	}
	netMinor := cn.TotalMinor - cn.TaxMinor
	desc := "Credit against invoice " + meta.InvoiceNumber
	if meta.InvoiceNumber == "" {
		desc = "Credit"
	}
	if cn.Reason != "" {
		desc += " — " + cn.Reason
	}
	return Document{
		Kind:     KindCreditNote,
		Meta:     meta,
		Currency: cn.Currency,
		Merchant: base.Merchant,
		Customer: base.Customer,
		// Terms are deliberately not inherited. The invoice's terms say when the
		// customer must pay; a credit note is not payable at all, and printing
		// "payable within 7 days" on one is a statement that is simply false.
		Lines: []Line{{
			Description:   desc,
			QuantityMilli: 1000,
			UnitMinor:     -cn.TotalMinor,
			NetMinor:      -netMinor,
			TaxMinor:      -cn.TaxMinor,
			TotalMinor:    -cn.TotalMinor,
		}},
		Totals: Totals{
			SubtotalMinor: -netMinor,
			TaxMinor:      -cn.TaxMinor,
			TotalMinor:    -cn.TotalMinor,
		},
		TaxEvidence: base.TaxEvidence,
	}
}

// Render produces the PDF bytes.
func Render(doc Document) ([]byte, error) {
	if doc.Currency == "" {
		return nil, fmt.Errorf("%w: no currency", ErrEmptyDocument)
	}
	if len(doc.Lines) == 0 {
		return nil, fmt.Errorf("%w: no lines", ErrEmptyDocument)
	}
	r := newRenderer(doc)
	r.drawHeader()
	r.drawParties()
	r.drawTable()
	r.drawTotals()
	r.drawNotes()
	return r.finish()
}

// Layout constants, in points.
const (
	marginLeft   = 50.0
	marginRight  = 50.0
	marginTop    = 50.0
	marginBottom = 60.0

	contentLeft  = marginLeft
	contentRight = pageWidth - marginRight
	contentWidth = contentRight - contentLeft

	bodySize  = 9.0
	smallSize = 8.0
	rowSize   = 8.0
	lineGap   = 11.5
)

// Column x positions for the line table. Each money column is right-aligned, so
// its value is the right edge.
const (
	colQtyRight   = 300.0
	colUnitRight  = 375.0
	colNetRight   = 445.0
	colTaxRight   = 495.0
	colTotalRight = contentRight
	colDescLeft   = contentLeft
	colDescWidth  = colQtyRight - 55 - colDescLeft
)

type renderer struct {
	doc   Document
	pages []*canvas
	cur   *canvas
	y     float64
}

func newRenderer(doc Document) *renderer {
	r := &renderer{doc: doc}
	r.newPage()
	return r
}

// newPage starts a page and resets the cursor to the top margin.
func (r *renderer) newPage() {
	r.cur = &canvas{}
	r.pages = append(r.pages, r.cur)
	r.y = pageHeight - marginTop
}

// space reports whether h points fit above the bottom margin.
func (r *renderer) space(h float64) bool { return r.y-h >= marginBottom }

// need breaks to a new page when h points do not fit.
func (r *renderer) need(h float64) {
	if !r.space(h) {
		r.newPage()
	}
}

// drawHeader prints the merchant block on the left and the document identity on
// the right.
func (r *renderer) drawHeader() {
	const (
		leftWidth  = 300.0
		rightRight = contentRight
	)

	y := r.y
	// Merchant.
	name := r.doc.Merchant.Name
	if name == "" {
		name = "—"
	}
	r.cur.text(contentLeft, y, truncate(HelveticaBold, 16, name, leftWidth), HelveticaBold, 16)
	y -= 20

	// The letterhead is bounded, not paginated. A merchant address is a handful of
	// lines; one that fills the page is a data-entry mistake, and letting it run
	// past the margin would silently print it off the paper. The bound is generous
	// (twelve lines) and the overflow is marked, so nothing disappears without a
	// trace.
	addrLines := wrapAll(Helvetica, bodySize, r.doc.Merchant.Address, leftWidth)
	y = r.drawBounded(addrLines, contentLeft, y, maxAddressLines, Helvetica, bodySize)
	if r.doc.Merchant.TaxID != "" {
		r.cur.text(contentLeft, y, "Tax ID: "+r.doc.Merchant.TaxID, Helvetica, bodySize)
		y -= lineGap
	}
	if r.doc.Merchant.CountryCode != "" {
		r.cur.text(contentLeft, y, r.doc.Merchant.CountryCode, Helvetica, bodySize)
		y -= lineGap
	}
	merchantBottom := y

	// Document identity, right-aligned.
	ty := r.y
	title := r.title()
	r.cur.textRight(rightRight, ty, title, HelveticaBold, 18)
	ty -= 24

	rows := [][2]string{}
	if r.doc.Meta.Number != "" {
		rows = append(rows, [2]string{"Number", r.doc.Meta.Number})
	} else if r.doc.Meta.Proforma {
		rows = append(rows, [2]string{"Number", "not yet assigned"})
	}
	if r.doc.Meta.InvoiceNumber != "" && r.doc.Kind == KindCreditNote {
		rows = append(rows, [2]string{"Invoice", r.doc.Meta.InvoiceNumber})
	}
	if r.doc.Meta.IssuedAt != nil {
		label := "Issued"
		if r.doc.Kind == KindCreditNote {
			label = "Issued"
		}
		rows = append(rows, [2]string{label, formatDate(*r.doc.Meta.IssuedAt)})
	}
	if r.doc.Meta.DueAt != nil {
		rows = append(rows, [2]string{"Due", formatDate(*r.doc.Meta.DueAt)})
	}
	if r.doc.Meta.Status != "" {
		rows = append(rows, [2]string{"Status", strings.ToUpper(r.doc.Meta.Status)})
	}
	for _, row := range rows {
		label := row[0] + ":"
		r.cur.textRight(rightRight-TextWidth(HelveticaBold, bodySize, row[1])-6,
			ty, label, Helvetica, bodySize)
		r.cur.textRight(rightRight, ty, row[1], HelveticaBold, bodySize)
		ty -= lineGap
	}

	if ty < merchantBottom {
		merchantBottom = ty
	}
	r.y = merchantBottom - 10

	// A rule separates the letterhead from the parties.
	r.cur.line(contentLeft, r.y, contentRight, r.y, 0.8)
	r.y -= 18

	if r.doc.Meta.Proforma {
		r.paragraph("Provisional document", "This is a proforma invoice. It is not a tax invoice and no invoice number has "+
			"been assigned. The number is issued, and this document becomes a tax invoice, when payment is received.")
	}
	if r.doc.Meta.Status == "void" {
		reason := r.doc.Meta.VoidReason
		if reason == "" {
			reason = "no reason was recorded"
		}
		r.paragraph("Void", "This document has been voided: "+reason+". It is not payable.")
	}
}

// title is the document heading.
func (r *renderer) title() string {
	switch r.doc.Kind {
	case KindCreditNote:
		return "CREDIT NOTE"
	default:
		if r.doc.Meta.Proforma {
			return "PROFORMA INVOICE"
		}
		return "INVOICE"
	}
}

// maxAddressLines bounds a letterhead or address block. It is a limit on a field
// that should hold a few lines, not a page.
const maxAddressLines = 12

// wrapAll wraps every line of a multi-line field, flattening the result.
func wrapAll(f Font, size float64, text string, width float64) []string {
	var out []string
	for _, line := range splitLines(text) {
		out = append(out, wrap(f, size, line, width)...)
	}
	return out
}

// drawBounded prints at most limit lines and marks the elision, returning the new
// cursor.
//
// Silently dropping the tail would be worse than truncating it: a document that
// says nothing is missing looks complete.
func (r *renderer) drawBounded(lines []string, x, y float64, limit int, f Font, size float64) float64 {
	for i, line := range lines {
		if i == limit {
			r.cur.text(x, y, "(continued)", Helvetica, size)
			y -= lineGap
			break
		}
		r.cur.text(x, y, line, f, size)
		y -= lineGap
	}
	return y
}

// drawParties prints the bill-to block and the period.
func (r *renderer) drawParties() {
	const leftWidth = 260.0
	startY := r.y

	r.cur.text(contentLeft, r.y, "Bill to", HelveticaBold, smallSize)
	r.y -= lineGap + 1

	c := r.doc.Customer
	name := c.Name
	if name == "" {
		name = "—"
	}
	r.cur.text(contentLeft, r.y, truncate(HelveticaBold, bodySize, name, leftWidth), HelveticaBold, bodySize)
	r.y -= lineGap

	for _, text := range []string{c.Email, c.BillingAddress} {
		lines := wrapAll(Helvetica, bodySize, text, leftWidth)
		r.y = r.drawBounded(lines, contentLeft, r.y, maxAddressLines, Helvetica, bodySize)
	}
	if c.TaxID != "" {
		r.cur.text(contentLeft, r.y, "Tax ID: "+c.TaxID, Helvetica, bodySize)
		r.y -= lineGap
	}
	if c.CountryCode != "" {
		r.cur.text(contentLeft, r.y, c.CountryCode, Helvetica, bodySize)
		r.y -= lineGap
	}
	leftBottom := r.y

	// The free-text bill-to block sits to the right of the address, where a
	// "make cheques payable to" note belongs.
	if r.doc.Meta.BillToText != "" {
		const (
			rightLeft  = 330.0
			rightWidth = contentRight - rightLeft
		)
		ry := startY - lineGap - 1
		for _, line := range splitLines(r.doc.Meta.BillToText) {
			for _, wrapped := range wrap(Helvetica, bodySize, line, rightWidth) {
				r.cur.text(rightLeft, ry, wrapped, Helvetica, bodySize)
				ry -= lineGap
			}
		}
		if ry < leftBottom {
			leftBottom = ry
		}
	}

	// The service period, when the document covers one.
	if p := r.doc.Period; p != nil && (p.Start != "" || p.End != "") {
		r.y = leftBottom - 4
		r.cur.text(contentLeft, r.y, "Period: "+p.Start+" – "+p.End, Helvetica, bodySize)
		leftBottom = r.y - lineGap
	}

	r.y = leftBottom - 8
}

// drawTable prints the column headers and one row per line, paginating.
func (r *renderer) drawTable() {
	r.tableHeader()
	for _, line := range r.doc.Lines {
		r.tableRow(line)
	}
	r.y -= 4
	r.cur.line(contentLeft, r.y, contentRight, r.y, 0.5)
	r.y -= 12
}

// tableHeader prints the header row and returns with the cursor below it.
func (r *renderer) tableHeader() {
	// A header that would be the last thing on a page is pointless.
	r.need(60)
	r.cur.fillRect(contentLeft, r.y-4, contentWidth, 16, 0.92)
	y := r.y
	r.cur.text(colDescLeft, y, "Description", HelveticaBold, smallSize)
	r.cur.textRight(colQtyRight, y, "Qty", HelveticaBold, smallSize)
	r.cur.textRight(colUnitRight, y, "Unit", HelveticaBold, smallSize)
	r.cur.textRight(colNetRight, y, "Net", HelveticaBold, smallSize)
	r.cur.textRight(colTaxRight, y, "Tax", HelveticaBold, smallSize)
	r.cur.textRight(colTotalRight, y, "Total", HelveticaBold, smallSize)
	r.y = y - 18
}

// tableRow prints one line, breaking the description across as many rows as it
// needs and starting a new page (with a repeated header) when it runs out of
// room.
func (r *renderer) tableRow(line Line) {
	lines := wrap(Helvetica, rowSize, line.Description, colDescWidth)
	if len(lines) == 0 {
		lines = []string{""}
	}
	rowHeight := float64(len(lines))*lineGap + 6

	// Keep a row whole: a two-line description split across a page break reads
	// as two line items.
	if !r.space(rowHeight) {
		r.newPage()
		r.tableHeader()
	}

	y := r.y
	for i, text := range lines {
		r.cur.text(colDescLeft, y, text, Helvetica, rowSize)
		if i == 0 {
			// The money columns align with the first line of the description.
			r.cur.textRight(colQtyRight, y, FormatQuantity(line.QuantityMilli), Helvetica, rowSize)
			r.cur.textRight(colUnitRight, y, FormatAmount(line.UnitMinor, r.doc.Currency), Helvetica, rowSize)
			r.cur.textRight(colNetRight, y, FormatAmount(line.NetMinor, r.doc.Currency), Helvetica, rowSize)
			r.cur.textRight(colTaxRight, y, FormatAmount(line.TaxMinor, r.doc.Currency), Helvetica, rowSize)
			r.cur.textRight(colTotalRight, y, FormatAmount(line.TotalMinor, r.doc.Currency), Helvetica, rowSize)
		}
		y -= lineGap
	}
	r.y = y - 6
}

// drawTotals prints subtotal, tax and total.
func (r *renderer) drawTotals() {
	const (
		labelLeft  = 330.0
		valueRight = contentRight
	)
	r.need(80)

	y := r.y
	subtotal := FormatAmount(r.doc.Totals.SubtotalMinor, r.doc.Currency)
	r.cur.text(labelLeft, y, "Subtotal", Helvetica, bodySize)
	r.cur.textRight(valueRight, y, subtotal, Helvetica, bodySize)
	y -= lineGap + 1

	taxLabel := r.taxLabel()
	tax := FormatAmount(r.doc.Totals.TaxMinor, r.doc.Currency)
	r.cur.text(labelLeft, y, truncate(Helvetica, bodySize, taxLabel, 150), Helvetica, bodySize)
	r.cur.textRight(valueRight, y, tax, Helvetica, bodySize)
	y -= lineGap + 1

	// The exemption reason belongs on the document, not only in the snapshot.
	if reason := r.doc.TaxEvidence.ExemptionReason; reason != "" {
		for _, wrapped := range wrap(Helvetica, smallSize, "Tax not charged: "+reason, 210) {
			r.cur.text(labelLeft, y, wrapped, Helvetica, smallSize)
			y -= lineGap - 1
		}
	}

	y -= 3
	r.cur.line(labelLeft, y+8, valueRight, y+8, 0.8)
	y -= 4

	label := "Total"
	if r.doc.Kind == KindCreditNote {
		label = "Total credited"
	}
	if r.doc.Meta.Proforma {
		label = "Total due"
	}
	r.cur.text(labelLeft, y, label, HelveticaBold, 11)
	r.cur.textRight(valueRight, y, FormatAmount(r.doc.Totals.TotalMinor, r.doc.Currency), HelveticaBold, 11)
	r.y = y - 20
}

// taxLabel names the tax with its jurisdiction and rate, which is the whole point
// of recording tax evidence.
func (r *renderer) taxLabel() string {
	ev := r.doc.TaxEvidence
	rate := ""
	if ev.Rate != "" {
		rate = formatRate(ev.Rate)
	}
	jurisdiction := strings.TrimSpace(ev.Jurisdiction)
	switch {
	case jurisdiction != "" && rate != "":
		return fmt.Sprintf("Tax (%s %s)", jurisdiction, rate)
	case rate != "":
		return fmt.Sprintf("Tax (%s)", rate)
	case jurisdiction != "":
		return fmt.Sprintf("Tax (%s)", jurisdiction)
	default:
		return "Tax"
	}
}

// drawNotes prints the client-visible note and the terms, each with its own
// heading and its own page break.
func (r *renderer) drawNotes() {
	if note := strings.TrimSpace(r.doc.Meta.ClientNote); note != "" {
		r.section("Note", note)
	}
	if terms := strings.TrimSpace(r.doc.Terms); terms != "" {
		r.section("Terms", terms)
	}
}

// section prints a titled block of prose, wrapping and paginating.
func (r *renderer) section(title, body string) {
	r.need(40)
	r.y -= 6
	r.cur.text(contentLeft, r.y, title, HelveticaBold, bodySize)
	r.y -= lineGap + 2
	for _, para := range splitLines(body) {
		for _, wrapped := range wrap(Helvetica, smallSize, para, contentWidth) {
			r.need(lineGap + 2)
			r.cur.text(contentLeft, r.y, wrapped, Helvetica, smallSize)
			r.y -= lineGap - 1
		}
	}
}

// paragraph prints a highlighted block, used for the proforma and void banners.
func (r *renderer) paragraph(title, body string) {
	r.need(46)
	top := r.y + 4
	r.cur.text(contentLeft, r.y, title, HelveticaBold, bodySize)
	r.y -= lineGap + 1
	for _, wrapped := range wrap(Helvetica, smallSize, body, contentWidth) {
		r.need(lineGap)
		r.cur.text(contentLeft, r.y, wrapped, Helvetica, smallSize)
		r.y -= lineGap - 1
	}
	// A box around the block, drawn after the text so it frames it.
	r.cur.rect(contentLeft-6, r.y+2, contentWidth+12, top-r.y+2, 0.6)
	r.y -= 12
}

// finish stamps the footers and serialises the file.
func (r *renderer) finish() ([]byte, error) {
	w := &pdfWriter{}

	// Reserve the objects the catalogue and the page tree need before the pages,
	// because each page must name its parent and the tree must name every page.
	catalogNum := w.reserve()
	pagesNum := w.reserve()

	fontNums := map[Font]int{}
	for _, f := range []Font{Helvetica, HelveticaBold} {
		num := w.reserve()
		fontNums[f] = num
		w.set(num, []byte(fmt.Sprintf("<< /Type /Font /Subtype /Type1 /BaseFont /%s /Encoding /WinAnsiEncoding >>",
			f.baseFont())))
	}

	pageNums := make([]int, 0, len(r.pages))
	contentNums := make([]int, 0, len(r.pages))
	for range r.pages {
		pageNums = append(pageNums, w.reserve())
		contentNums = append(contentNums, w.reserve())
	}

	total := len(r.pages)
	for i, page := range r.pages {
		// The footer is drawn last so it can say "page i of n": the count is only
		// known once every page exists.
		r.drawFooter(page, i+1, total)
		w.set(contentNums[i], page.contentStream())
		w.set(pageNums[i], []byte(fmt.Sprintf(
			"<< /Type /Page /Parent %d 0 R /MediaBox [0 0 %.0f %.0f] "+
				"/Resources << /Font << /F1 %d 0 R /F2 %d 0 R >> >> /Contents %d 0 R >>",
			pagesNum, pageWidth, pageHeight,
			fontNums[Helvetica], fontNums[HelveticaBold], contentNums[i])))
	}

	var kids strings.Builder
	for _, num := range pageNums {
		fmt.Fprintf(&kids, "%d 0 R ", num)
	}
	w.set(pagesNum, []byte(fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>",
		strings.TrimSpace(kids.String()), total)))
	w.set(catalogNum, []byte(fmt.Sprintf("<< /Type /Catalog /Pages %d 0 R >>", pagesNum)))

	return w.finish(catalogNum), nil
}

// drawFooter prints the document identity and the page number at the bottom of
// one page.
func (r *renderer) drawFooter(page *canvas, n, total int) {
	const y = 36.0
	left := r.footerText()
	page.text(contentLeft, y, truncate(Helvetica, 7.5, left, contentWidth-120), Helvetica, 7.5)
	page.textRight(contentRight, y, fmt.Sprintf("Page %d of %d", n, total), Helvetica, 7.5)
}

// footerText is the identifying line at the bottom of every page.
func (r *renderer) footerText() string {
	parts := []string{}
	switch {
	case r.doc.Kind == KindCreditNote:
		parts = append(parts, "Credit note")
	case r.doc.Meta.Proforma:
		parts = append(parts, "Proforma invoice")
	default:
		parts = append(parts, "Invoice")
	}
	if r.doc.Meta.Number != "" {
		parts = append(parts, r.doc.Meta.Number)
	}
	if r.doc.Merchant.Name != "" {
		parts = append(parts, r.doc.Merchant.Name)
	}
	return strings.Join(parts, " · ")
}

// formatDate renders a date the way a document reads: "7 November 2026".
//
// A date without a zone is printed in UTC, because the timestamps here were
// recorded in UTC and shifting them would make a document issued late on the last
// day of a month appear to be issued the next month in some zones.
func formatDate(t time.Time) string { return t.UTC().Format("2 January 2006") }

// splitLines splits a multi-line field on newlines, dropping empty trailing
// entries.
func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	raw := strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
	out := make([]string, 0, len(raw))
	for _, line := range raw {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		out = append(out, line)
	}
	return out
}

// SafeFilename derives a download filename from a document number.
//
// A number is usually "INV-000042", but it comes from a configurable prefix, so
// it is sanitised: a path separator or a quote in a configured prefix must not be
// able to shape a Content-Disposition header or escape a directory.
func SafeFilename(number string) string {
	var b strings.Builder
	for _, r := range number {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '_' || r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := strings.Trim(b.String(), "._")
	if out == "" {
		return "document"
	}
	return out
}
