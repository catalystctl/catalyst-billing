package invoicepdf

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/catalyst/billing/internal/billing"
	"github.com/jackc/pgx/v5"
)

// Document numbering (INV-03, INV-18).
//
// The sequence itself already exists and is gap-free: billing.NumberSeries
// allocates inside the issuing transaction under a row lock, so two issuers never
// take the same number and a rolled-back issuance returns the number to the
// series. What was missing is the *format*: the prefix and the width were
// compiled into billing.DefaultSeries.
//
// The format is a setting, read at issue time, and this file is the one place it
// is turned into a series. That is deliberate. If every issuance site read the
// settings itself, each would need its own parse and its own defaults, and two of
// them would eventually disagree — a customer would see INV-000042 and INV-42 for
// the same sequence.
//
// # Why a helper rather than a change to billing.DefaultSeries
//
// DefaultSeries is the fallback the issuer applies when it is handed a zero
// NumberSeries. Making it read the database would give the billing package a
// dependency on settings and on a transaction it does not own, and would make
// issuance fail whenever the settings table was unreachable. Keeping the read
// here means the *integrator* decides: read the series inside the transaction
// that issues, and pass it to Issuer.Issue.
//
// # Tokens, and one honest limitation
//
// A prefix may contain {year}, {month} and {day}. They are expanded against the
// issuance date — the date the number is consumed, not the date the draft was
// created. That matters at a month boundary: a draft written on 31 January and
// issued on 1 February belongs to February's format, and only the issuance date
// says so.
//
// {number} marks where the digits go. billing.InvoiceNumber.Format renders
// "prefix + zero-padded value", so a pattern is expressible exactly when
// {number} is the last token or absent: "INV-{number}", "INV-{year}-{number}",
// "INV-" all reduce to a prefix that Format completes. A pattern that puts text
// *after* {number} — "INV-{number}/2026" — cannot be expressed by
// billing.NumberSeries at all, because Format has no suffix. Such a pattern is
// rejected here with an error naming the alternative, rather than silently
// producing "INV-{number}/2026000042" on a customer's document.
//
// Expressing a suffix needs a Suffix field on billing.NumberSeries and a change
// to InvoiceNumber.Format, both in internal/billing/invoice.go, which this
// package does not own. That is recorded in the package report.

// Setting keys, as seeded by migration 0020.
const (
	// SettingInvoicePrefix is `invoices.invoice_number_prefix`.
	SettingInvoicePrefix = "invoice_number_prefix"
	// SettingInvoicePadding is `invoices.invoice_number_padding`.
	SettingInvoicePadding = "invoice_number_padding"
	// SettingCreditNotePrefix is `invoices.credit_note_prefix`.
	SettingCreditNotePrefix = "credit_note_prefix"
	// SettingCreditNotePadding is `invoices.credit_note_padding`.
	SettingCreditNotePadding = "credit_note_padding"
	// SettingProforma is `invoices.proforma`: whether issuance produces a
	// provisional, unnumbered document.
	SettingProforma = "proforma"
)

// Code-side defaults, used when a key is absent or unparseable.
//
// They match the seeded values and billing.DefaultSeries, so an installation that
// has never opened its settings page numbers documents exactly as it did before
// numbering became configurable.
const (
	DefaultInvoicePrefix     = "INV-"
	DefaultInvoicePadding    = 6
	DefaultCreditNotePrefix  = "CN-"
	DefaultCreditNotePadding = 6

	// CreditNoteScope is the persisted sequence credit notes allocate from. It
	// matches creditnote.CreditSeries.Scope; forking it would restart numbering
	// at 1 and let two notes share a number.
	CreditNoteScope = "credit_note"

	// maxPadding bounds a configured width. A width of 10^9 would ask the
	// formatter to build a gigabyte of zeros; a document number is never wider
	// than this.
	maxPadding = 20
	// maxPrefixLen bounds a configured pattern. The prefix lands in a filename
	// and in a PDF string, and an unbounded pattern is a denial of service on
	// both.
	maxPrefixLen = 64
)

// Errors returned when a configured pattern cannot produce a number.
var (
	// ErrInvalidSeries is returned for a pattern that cannot be expressed as a
	// billing.NumberSeries.
	ErrInvalidSeries = errors.New("invoicepdf: invalid number series")
	// ErrSuffixAfterNumber is returned when text follows {number}. It is a
	// distinct error so a settings screen can explain the fix precisely.
	ErrSuffixAfterNumber = errors.New("invoicepdf: text after {number} needs a suffix, which billing.NumberSeries cannot express; move it before {number} or drop it")
)

// Numbering is the configured document numbering.
type Numbering struct {
	InvoicePrefix     string
	InvoicePadding    int
	CreditNotePrefix  string
	CreditNotePadding int
	// Proforma reports whether a new document is provisional by default.
	Proforma bool
}

// DefaultNumbering is the numbering an installation with no settings rows uses.
func DefaultNumbering() Numbering {
	return Numbering{
		InvoicePrefix:     DefaultInvoicePrefix,
		InvoicePadding:    DefaultInvoicePadding,
		CreditNotePrefix:  DefaultCreditNotePrefix,
		CreditNotePadding: DefaultCreditNotePadding,
	}
}

// SettingsQuerier is the read surface a numbering load needs.
//
// It is the narrowest interface that works, so a caller can pass a pgx.Tx, a
// pool, or a test double without this package importing a connection type it does
// not otherwise need.
type SettingsQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// LoadNumbering reads the document numbering settings, falling back to the
// code-side defaults for every key that is absent, empty or unparseable.
//
// A malformed padding is not an error: refusing to issue an invoice because
// somebody typed "six" into a settings field would take the revenue path down
// over a typo. A malformed *prefix*, however, is reported by InvoiceSeries when
// the number is allocated, where it can be attributed to the issuance that needs
// it.
func LoadNumbering(ctx context.Context, q SettingsQuerier) (Numbering, error) {
	out := DefaultNumbering()
	rows, err := q.Query(ctx, `
		SELECT key, value FROM settings
		WHERE group_key = 'invoices'
		  AND key IN ('invoice_number_prefix', 'invoice_number_padding',
		              'credit_note_prefix', 'credit_note_padding', 'proforma')`)
	if err != nil {
		return out, fmt.Errorf("invoicepdf: read numbering settings: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return out, fmt.Errorf("invoicepdf: scan numbering setting: %w", err)
		}
		value = strings.TrimSpace(value)
		switch key {
		case SettingInvoicePrefix:
			if p, err := NormalisePrefix(value); err == nil {
				out.InvoicePrefix = p
			}
		case SettingInvoicePadding:
			if n, ok := parsePadding(value); ok {
				out.InvoicePadding = n
			}
		case SettingCreditNotePrefix:
			if p, err := NormalisePrefix(value); err == nil {
				out.CreditNotePrefix = p
			}
		case SettingCreditNotePadding:
			if n, ok := parsePadding(value); ok {
				out.CreditNotePadding = n
			}
		case SettingProforma:
			out.Proforma = value == "true" || value == "1"
		}
	}
	if err := rows.Err(); err != nil {
		return out, fmt.Errorf("invoicepdf: read numbering settings: %w", err)
	}
	return out, nil
}

// parsePadding reads a padding width, accepting only a plausible one.
func parsePadding(v string) (int, bool) {
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 || n > maxPadding {
		return 0, false
	}
	return n, true
}

// NormalisePrefix validates a configured prefix.
//
// A blank prefix is returned unchanged and means "the default" to the caller: an
// absent setting and an empty setting are indistinguishable in the table, and a
// document numbered "42" in a system whose series is "INV-" would be a surprise.
func NormalisePrefix(p string) (string, error) {
	if p == "" {
		return "", nil
	}
	if len(p) > maxPrefixLen {
		return "", fmt.Errorf("%w: prefix is longer than %d bytes", ErrInvalidSeries, maxPrefixLen)
	}
	for _, r := range p {
		if r < 0x20 || r == 0x7F {
			return "", fmt.Errorf("%w: prefix contains a control character", ErrInvalidSeries)
		}
	}
	return p, nil
}

// InvoiceSeries returns the series the invoice issue path must pass to
// billing.Issuer.Issue.
//
// The scope is billing.DefaultSeries.Scope: the scope names the persisted
// sequence, and a configurable *format* must not fork the sequence, or a change of
// prefix would restart numbering at 1 and two invoices would share a number.
//
// now is the issuance date, used to expand {year}/{month}/{day}.
func (n Numbering) InvoiceSeries(now time.Time) (billing.NumberSeries, error) {
	prefix, err := seriesPrefix(n.InvoicePrefix, now)
	if err != nil {
		return billing.NumberSeries{}, err
	}
	width := n.InvoicePadding
	if width <= 0 || width > maxPadding {
		width = DefaultInvoicePadding
	}
	return billing.NumberSeries{Scope: billing.DefaultSeries.Scope, Prefix: prefix, Width: width}, nil
}

// CreditNoteSeries returns the series the credit-note issue path must use.
//
// internal/creditnote allocates through its own creditnote.CreditSeries rather
// than through billing.NumberSeries, so this value cannot be passed to
// creditnote.Issue today. It exists so the format has exactly one definition, and
// so that giving creditnote.Issue a series parameter — a change in a package this
// one does not own — is a one-line call at the issue site.
func (n Numbering) CreditNoteSeries(now time.Time) (billing.NumberSeries, error) {
	prefix, err := seriesPrefix(n.CreditNotePrefix, now)
	if err != nil {
		return billing.NumberSeries{}, err
	}
	width := n.CreditNotePadding
	if width <= 0 || width > maxPadding {
		width = DefaultCreditNotePadding
	}
	return billing.NumberSeries{Scope: CreditNoteScope, Prefix: prefix, Width: width}, nil
}

// seriesPrefix turns a configured pattern into the prefix a NumberSeries carries.
//
// It expands the date tokens, then reduces a trailing {number} to nothing, which
// is what makes "INV-{year}-{number}" render as "INV-2026-000042" through
// billing.InvoiceNumber.Format.
func seriesPrefix(pattern string, now time.Time) (string, error) {
	if pattern == "" {
		return "", nil
	}
	if _, err := NormalisePrefix(pattern); err != nil {
		return "", err
	}
	expanded := expandDateTokens(pattern, now)
	idx := strings.Index(expanded, "{number}")
	if idx < 0 {
		return expanded, nil
	}
	if idx != len(expanded)-len("{number}") {
		return "", fmt.Errorf("%w: pattern %q", ErrSuffixAfterNumber, pattern)
	}
	return expanded[:idx], nil
}

// expandDateTokens substitutes {year}, {month} and {day}.
//
// {number} is deliberately left alone: it is not a date, and seriesPrefix decides
// what to do with it.
func expandDateTokens(pattern string, now time.Time) string {
	if !strings.ContainsRune(pattern, '{') {
		return pattern
	}
	t := now.UTC()
	return strings.NewReplacer(
		"{year}", fmt.Sprintf("%04d", t.Year()),
		"{month}", fmt.Sprintf("%02d", int(t.Month())),
		"{day}", fmt.Sprintf("%02d", t.Day()),
	).Replace(pattern)
}

// ProformaEnabled reports whether the installation issues proforma documents by
// default.
func (n Numbering) ProformaEnabled() bool { return n.Proforma }
