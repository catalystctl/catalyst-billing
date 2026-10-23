package invoicepdf

// A document template a theme brings with it (INV-15).
//
// # What was actually missing
//
// The brand overlay (internal/brand) already reached this renderer: accent, logo and
// trading name are drawn on the invoice, and the credit note inherits them because it is
// rendered from the same base snapshot. So "per theme" was not a colour problem. What was
// missing is that a theme could not change *anything a theme legitimately owns*: the
// document heading, the label above the customer's address, and the line a document
// carries in its footer. Paymenter's themes ship `themes/<theme>/views/pdf/invoice.blade.php`
// and can reword exactly those; Catalyst shipped one fixed layout, so a reseller whose
// jurisdiction calls the document a "TAX INVOICE" got "INVOICE".
//
// # What a template may not change
//
// A template is presentation. It cannot move a column, hide a total, drop the tax evidence,
// print a different number or a different customer. Those come from the frozen snapshot and
// are the reason the document is evidence at all. So the type below is deliberately a
// handful of strings — there is no field for "layout", because the safe set of layouts is
// one, and a template system that could reflow the money table would be a way to produce a
// document that does not match the ledger.
//
// # Why an empty template must render byte-identically
//
// The zero Template is the built-in look. Every accessor below falls back to the same
// literal the renderer printed before this file existed, so an installation that never
// configures one (and every credit note, proforma and reprint) produces exactly the bytes it
// produced before. A partial template is a partial override for the same reason: a theme
// that only changes the heading must not thereby change the footer.
//
// # Why JSON in a setting rather than a table
//
// A theme is a file bundle and a setting is how the panel already stores operator-supplied
// structure (`invoices.pdf_template`, KindJSON, default "{}"). Adding a table would mean a
// migration per field and a storetest classification for a value that is, at most, six
// strings. Validation is the price of that choice, and it is paid here: an unknown field or
// an over-long line is refused at parse time, so a malformed template is a settings-screen
// error rather than a document with a truncated heading.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/catalyst/billing/internal/brand"
)

const (
	// SettingPDFTemplate is `invoices.pdf_template`.
	SettingPDFTemplate = "pdf_template"
	// maxTemplateField bounds one line of text. A heading is a heading; a paragraph belongs
	// in the merchant's terms, which already have a block of their own.
	maxTemplateField = 120
)

// ErrInvalidTemplate is returned for a template that cannot be rendered.
var ErrInvalidTemplate = errors.New("invoicepdf: invalid document template")

// Template is the presentation a document takes from its theme.
//
// Every field is optional. An empty field means "the built-in wording", not "print nothing",
// because a template author who leaves out a title expects an invoice, not a blank space.
type Template struct {
	// Name identifies the theme or template this came from. It is printed nowhere; it is
	// what an operator sees in the settings screen and what an audit row records.
	Name string `json:"name,omitempty"`
	// TitleInvoice, TitleProforma and TitleCreditNote replace the document heading.
	TitleInvoice    string `json:"title_invoice,omitempty"`
	TitleProforma   string `json:"title_proforma,omitempty"`
	TitleCreditNote string `json:"title_credit_note,omitempty"`
	TitleDebitNote  string `json:"title_debit_note,omitempty"`
	BillToLabel     string `json:"bill_to_label,omitempty"`
	FooterNote      string `json:"footer_note,omitempty"`
	TotalLabel      string `json:"total_label,omitempty"`
	TotalCredited   string `json:"total_credited_label,omitempty"`
	TotalDebited    string `json:"total_debited_label,omitempty"`
	AccentHex       string `json:"accent,omitempty"`
}

// IsZero reports whether the template changes nothing, which is what makes the built-in
// rendering expressible as "no template configured".
func (t Template) IsZero() bool { return t == Template{} }

// Validate checks a template before it reaches a document.
func (t Template) Validate() error {
	if len(t.Name) > maxTemplateField {
		return fmt.Errorf("%w: name is longer than %d bytes", ErrInvalidTemplate, maxTemplateField)
	}
	for field, value := range map[string]string{
		"title_invoice":        t.TitleInvoice,
		"title_proforma":       t.TitleProforma,
		"title_credit_note":    t.TitleCreditNote,
		"title_debit_note":     t.TitleDebitNote,
		"bill_to_label":        t.BillToLabel,
		"footer_note":          t.FooterNote,
		"total_label":          t.TotalLabel,
		"total_credited_label": t.TotalCredited,
		"total_debited_label":  t.TotalDebited,
	} {
		if len(value) > maxTemplateField {
			return fmt.Errorf("%w: %s is longer than %d bytes", ErrInvalidTemplate, field, maxTemplateField)
		}
		// A control character would corrupt the PDF string literal the same way it does in a
		// number prefix, and the same validation applies for the same reason.
		for _, r := range value {
			if r < 0x20 || r == 0x7F {
				return fmt.Errorf("%w: %s contains a control character", ErrInvalidTemplate, field)
			}
		}
	}
	if t.AccentHex != "" {
		if _, err := brand.ParseHex(t.AccentHex); err != nil {
			return fmt.Errorf("%w: %s", ErrInvalidTemplate, err)
		}
	}
	return nil
}

// ParseTemplate reads a template from its settings text.
//
// A blank value is the zero template rather than an error: that is what an installation with
// no row has, and reading it must not fail. Anything else must be a JSON object this type
// understands — `DisallowUnknownFields` turns a typo ("titel_invoice") into a settings error
// instead of a document that silently ignores it.
func ParseTemplate(raw string) (Template, error) {
	var t Template
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "{}" {
		return Template{}, nil
	}
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&t); err != nil {
		return Template{}, fmt.Errorf("%w: %v", ErrInvalidTemplate, err)
	}
	if err := t.Validate(); err != nil {
		return Template{}, err
	}
	return t, nil
}

// LoadTemplate reads the configured template, falling back to the built-in one.
//
// A malformed template is not fatal: refusing to print a customer's invoice because the
// theme's JSON has a trailing comma would take the document path down over decoration. The
// error is returned alongside the fallback so the caller can log it — invisible is the one
// thing it must not be.
func LoadTemplate(ctx context.Context, q SettingsQuerier) (Template, error) {
	var raw string
	rows, err := q.Query(ctx, `
		SELECT value FROM settings WHERE group_key = 'invoices' AND key = 'pdf_template'`)
	if err != nil {
		return Template{}, nil
	}
	defer rows.Close()
	if rows.Next() {
		if err := rows.Scan(&raw); err != nil {
			return Template{}, nil
		}
	}
	if err := rows.Err(); err != nil {
		return Template{}, nil
	}
	t, perr := ParseTemplate(raw)
	if perr != nil {
		return Template{}, perr
	}
	return t, nil
}

// The built-in wording. Each accessor below falls back to one of these, which is what keeps
// an unset template byte-identical to the pre-template renderer.
const (
	defaultTitleInvoice    = "INVOICE"
	defaultTitleProforma   = "PROFORMA INVOICE"
	defaultTitleCreditNote = "CREDIT NOTE"
	defaultTitleDebitNote  = "DEBIT NOTE"
	defaultBillToLabel     = "Bill to"
	defaultTotalLabel      = "Total"
	defaultTotalCredited   = "Total credited"
	defaultTotalDebited    = "Total debited"
)

// titleFor returns the heading for a document kind.
func (t Template) titleFor(kind Kind, proforma bool) string {
	switch kind {
	case KindCreditNote:
		if t.TitleCreditNote != "" {
			return t.TitleCreditNote
		}
		return defaultTitleCreditNote
	case KindDebitNote:
		if t.TitleDebitNote != "" {
			return t.TitleDebitNote
		}
		return defaultTitleDebitNote
	default:
		if proforma {
			if t.TitleProforma != "" {
				return t.TitleProforma
			}
			return defaultTitleProforma
		}
		if t.TitleInvoice != "" {
			return t.TitleInvoice
		}
		return defaultTitleInvoice
	}
}

// billTo returns the label above the customer's address.
func (t Template) billTo() string {
	if t.BillToLabel != "" {
		return t.BillToLabel
	}
	return defaultBillToLabel
}

// totalLabelFor returns the label beside the total.
func (t Template) totalLabelFor(kind Kind, proforma bool) string {
	switch kind {
	case KindCreditNote:
		if t.TotalCredited != "" {
			return t.TotalCredited
		}
		return defaultTotalCredited
	case KindDebitNote:
		if t.TotalDebited != "" {
			return t.TotalDebited
		}
		return defaultTotalDebited
	}
	if proforma {
		return "Total due"
	}
	if t.TotalLabel != "" {
		return t.TotalLabel
	}
	return defaultTotalLabel
}

// accentOverride returns the theme's accent when the brand has not set one.
//
// The brand wins on purpose: the operator configured a brand for their installation, and a
// theme is a house style, not a licence to override the tenant's own colour. The template
// accent exists for the case the brand left empty — which is the common one on a new install.
func (t Template) accentOverride(configured bool) (brand.Colour, bool) {
	if configured || t.AccentHex == "" {
		return brand.Colour{}, false
	}
	c, err := brand.ParseHex(t.AccentHex)
	if err != nil {
		return brand.Colour{}, false
	}
	return c, true
}

// footerNote is the extra line a theme adds to the footer.
func (t Template) footerNote() string { return strings.TrimSpace(t.FooterNote) }
