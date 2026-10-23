package invoicepdf

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/catalyst/billing/internal/billing"
)

var numberingAt = time.Date(2026, 3, 9, 15, 0, 0, 0, time.UTC)

// The defaults must reproduce exactly what billing.DefaultSeries produced, or
// making numbering configurable would silently renumber every installation.
func TestDefaultNumberingMatchesBillingDefault(t *testing.T) {
	n := DefaultNumbering()
	series, err := n.InvoiceSeries(numberingAt)
	if err != nil {
		t.Fatalf("series: %v", err)
	}
	if series.Scope != billing.DefaultSeries.Scope {
		t.Fatalf("scope = %q, want %q", series.Scope, billing.DefaultSeries.Scope)
	}
	if series.Prefix != billing.DefaultSeries.Prefix {
		t.Fatalf("prefix = %q, want %q", series.Prefix, billing.DefaultSeries.Prefix)
	}
	if series.Width != billing.DefaultSeries.Width {
		t.Fatalf("width = %d, want %d", series.Width, billing.DefaultSeries.Width)
	}

	// And the number it produces is the same string.
	got := (billing.InvoiceNumber{Scope: series.Scope, Value: 42, Prefix: series.Prefix, Width: series.Width}).Format()
	want := (billing.InvoiceNumber{Scope: "default", Value: 42, Prefix: "INV-", Width: 6}).Format()
	if got != want || got != "INV-000042" {
		t.Fatalf("formatted number = %q, want %q", got, want)
	}
}

// A configured prefix and width must reach the series, and the date tokens must
// expand against the issuance date.
func TestInvoiceSeriesExpandsTokens(t *testing.T) {
	cases := []struct {
		prefix string
		width  int
		want   string
	}{
		{"INV-", 6, "INV-"},
		{"INV-{year}-", 4, "INV-2026-"},
		{"{year}{month}{day}-", 3, "20260309-"},
		{"{year}/{month}/INV-{number}", 5, "2026/03/INV-"},
		{"INV-{number}", 6, "INV-"},
		{"", 6, ""},
		{"BILL-", 1, "BILL-"},
	}
	for _, tc := range cases {
		n := Numbering{InvoicePrefix: tc.prefix, InvoicePadding: tc.width}
		series, err := n.InvoiceSeries(numberingAt)
		if err != nil {
			t.Fatalf("%q: %v", tc.prefix, err)
		}
		if series.Prefix != tc.want {
			t.Errorf("prefix %q -> %q, want %q", tc.prefix, series.Prefix, tc.want)
		}
		if series.Width != tc.width {
			t.Errorf("prefix %q -> width %d, want %d", tc.prefix, series.Width, tc.width)
		}
	}

	// The full number for a tokenised prefix.
	n := Numbering{InvoicePrefix: "INV-{year}-{number}", InvoicePadding: 4}
	series, err := n.InvoiceSeries(numberingAt)
	if err != nil {
		t.Fatalf("series: %v", err)
	}
	num := billing.InvoiceNumber{Value: 42, Prefix: series.Prefix, Width: series.Width}
	if got := num.Format(); got != "INV-2026-0042" {
		t.Fatalf("formatted = %q, want INV-2026-0042", got)
	}
}

// A pattern that puts text after {number} becomes a suffix, because that is the
// format Paymenter documents ("{number}/{year}") and the one an operator will
// type first.
func TestSuffixAfterNumberIsRendered(t *testing.T) {
	n := Numbering{InvoicePrefix: "{number}/{year}", InvoicePadding: 6}
	series, err := n.InvoiceSeries(numberingAt)
	if err != nil {
		t.Fatalf("series: %v", err)
	}
	if series.Prefix != "" || series.Suffix != "/2026" {
		t.Fatalf("prefix %q suffix %q, want \"\" and /2026", series.Prefix, series.Suffix)
	}
	got := (billing.InvoiceNumber{Value: 7, Prefix: series.Prefix, Width: series.Width,
		Suffix: series.Suffix}).Format()
	if got != "000007/2026" {
		t.Fatalf("formatted = %q, want 000007/2026", got)
	}
}

// A brace placeholder this package does not know is refused rather than printed
// literally on every document.
func TestUnknownPlaceholderIsRefused(t *testing.T) {
	n := Numbering{InvoicePrefix: "INV-{yaer}-", InvoicePadding: 6}
	if _, err := n.InvoiceSeries(numberingAt); !errors.Is(err, ErrMissingNumberToken) {
		t.Fatalf("error = %v, want ErrMissingNumberToken", err)
	}
}

// An implausible width falls back to the default rather than building a huge
// string of zeros.
func TestPaddingIsBounded(t *testing.T) {
	for _, width := range []int{0, -1, 21, 1 << 20} {
		n := Numbering{InvoicePrefix: "INV-", InvoicePadding: width}
		series, err := n.InvoiceSeries(numberingAt)
		if err != nil {
			t.Fatalf("width %d: %v", width, err)
		}
		if series.Width != DefaultInvoicePadding {
			t.Fatalf("width %d produced %d, want the default %d", width, series.Width, DefaultInvoicePadding)
		}
	}
}

// A prefix with a control character would corrupt a PDF string literal and a
// Content-Disposition header, so it is refused.
func TestPrefixRejectsControlCharacters(t *testing.T) {
	for _, bad := range []string{"INV\n-", "INV\x00-", "INV\r-"} {
		if _, err := NormalisePrefix(bad); err == nil {
			t.Fatalf("prefix %q was accepted", bad)
		}
	}
	if _, err := NormalisePrefix(strings.Repeat("x", maxPrefixLen+1)); err == nil {
		t.Fatal("an over-long prefix was accepted")
	}
}

// The credit-note series keeps its own scope, so a change of prefix cannot make
// the two sequences collide.
func TestCreditNoteSeriesHasItsOwnScope(t *testing.T) {
	n := DefaultNumbering()
	n.CreditNotePrefix = "CN-{year}-"
	n.CreditNotePadding = 5
	series, err := n.CreditNoteSeries(numberingAt)
	if err != nil {
		t.Fatalf("series: %v", err)
	}
	if series.Scope != CreditNoteScope {
		t.Fatalf("scope = %q, want %q", series.Scope, CreditNoteScope)
	}
	if series.Scope == billing.DefaultSeries.Scope {
		t.Fatal("the credit note shares the invoice sequence")
	}
	if series.Prefix != "CN-2026-" || series.Width != 5 {
		t.Fatalf("series = %+v", series)
	}
}

// expandDateTokens must be pure: the same pattern and date always produce the
// same string, and {number} is never touched by it.
func TestExpandDateTokensLeavesNumber(t *testing.T) {
	got := expandDateTokens("A{year}B{month}C{day}D{number}E", numberingAt)
	want := "A2026B03C09D{number}E"
	if got != want {
		t.Fatalf("expandDateTokens = %q, want %q", got, want)
	}
	// A pattern with no tokens is returned unchanged, including one with a brace
	// that is not a token.
	if got := expandDateTokens("INV-{unknown}-", numberingAt); got != "INV-{unknown}-" {
		t.Fatalf("an unknown token was rewritten: %q", got)
	}
}
