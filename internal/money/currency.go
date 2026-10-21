package money

import (
	"fmt"
	"strings"
)

// Currency is a currency code together with its minor-unit exponent, matching
// ISO 4217 "minor unit". USD and EUR use 2, JPY uses 0, KWD uses 3.
//
// The exponent is never inferred from the code at runtime unless the code is in
// the built-in ISO table, because guessing a scale silently changes the value of
// persisted data.
type Currency struct {
	code  string
	scale uint8
}

// MaxScale is the largest supported minor-unit exponent. ISO 4217 currently
// tops out at 4 (CLF, UYW).
const MaxScale = 4

// NewCurrency validates and constructs a currency. code is upper-cased and must
// be 2..12 characters of A-Z/0-9; scale must be <= MaxScale.
func NewCurrency(code string, scale uint8) (Currency, error) {
	code = strings.ToUpper(strings.TrimSpace(code))
	if len(code) < 2 || len(code) > 12 {
		return Currency{}, fmt.Errorf("%w: %q must be 2..12 characters", ErrInvalidCurrency, code)
	}
	for _, r := range code {
		if (r < 'A' || r > 'Z') && (r < '0' || r > '9') {
			return Currency{}, fmt.Errorf("%w: %q contains invalid character %q", ErrInvalidCurrency, code, r)
		}
	}
	if scale > MaxScale {
		return Currency{}, fmt.Errorf("%w: %q scale %d exceeds %d", ErrInvalidCurrency, code, scale, MaxScale)
	}
	return Currency{code: code, scale: scale}, nil
}

// MustCurrency is NewCurrency for static definitions, where a failure is a
// programming error and must panic at startup rather than at request time.
func MustCurrency(code string, scale uint8) Currency {
	c, err := NewCurrency(code, scale)
	if err != nil {
		panic(err)
	}
	return c
}

// Code returns the upper-case currency code.
func (c Currency) Code() string { return c.code }

// Scale returns the minor-unit exponent.
func (c Currency) Scale() uint8 { return c.scale }

// IsValid reports whether the currency was constructed.
func (c Currency) IsValid() bool { return c.code != "" }

// Equal reports whether two currencies are the same code and scale.
func (c Currency) Equal(o Currency) bool { return c.code == o.code && c.scale == o.scale }

func (c Currency) String() string {
	if !c.IsValid() {
		return "<invalid currency>"
	}
	return c.code
}

// iso4217Exponent is the ISO 4217 minor-unit table, restricted to currencies
// whose exponent differs from the default of 2 plus the common 2-exponent set
// used in tests. Codes absent from this table must be declared explicitly with
// NewCurrency.
var iso4217Exponent = map[string]uint8{
	// exponent 0
	"BIF": 0, "CLP": 0, "DJF": 0, "GNF": 0, "ISK": 0, "JPY": 0, "KMF": 0,
	"KRW": 0, "PYG": 0, "RWF": 0, "UGX": 0, "UYI": 0, "VND": 0, "VUV": 0,
	"XAF": 0, "XOF": 0, "XPF": 0,
	// exponent 3
	"BHD": 3, "IQD": 3, "JOD": 3, "KWD": 3, "LYD": 3, "OMR": 3, "TND": 3,
	// exponent 4
	"CLF": 4, "UYW": 4,
	// exponent 2 (the common case)
	"AUD": 2, "BRL": 2, "CAD": 2, "CHF": 2, "CNY": 2, "CZK": 2, "DKK": 2,
	"EUR": 2, "GBP": 2, "HKD": 2, "HUF": 2, "IDR": 2, "ILS": 2, "INR": 2,
	"MXN": 2, "MYR": 2, "NOK": 2, "NZD": 2, "PHP": 2, "PLN": 2, "RON": 2,
	"SEK": 2, "SGD": 2, "THB": 2, "TRY": 2, "TWD": 2, "USD": 2, "ZAR": 2,
}

// LookupCurrency returns the ISO 4217 definition for code.
func LookupCurrency(code string) (Currency, bool) {
	scale, ok := iso4217Exponent[strings.ToUpper(strings.TrimSpace(code))]
	if !ok {
		return Currency{}, false
	}
	c, err := NewCurrency(code, scale)
	if err != nil {
		return Currency{}, false
	}
	return c, true
}

// MustLookupCurrency is LookupCurrency for static definitions.
func MustLookupCurrency(code string) Currency {
	c, ok := LookupCurrency(code)
	if !ok {
		panic(fmt.Sprintf("money: unknown ISO 4217 currency %q", code))
	}
	return c
}
