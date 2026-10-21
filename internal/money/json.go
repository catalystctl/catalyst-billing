package money

import (
	"encoding/json"
	"fmt"
)

// JSON encoding is lossless and explicit: an amount is always a minor-unit
// integer plus a currency code and scale. Floats are never used, and the scale
// travels with the value so a currency without a built-in ISO definition still
// round-trips exactly.
//
// This matters because invoice snapshots are persisted as JSONB: a snapshot that
// silently lost its currency would make an issued document unreproducible.

type currencyJSON struct {
	Code  string `json:"code"`
	Scale uint8  `json:"scale"`
}

type amountJSON struct {
	Minor    int64        `json:"minor"`
	Currency currencyJSON `json:"currency"`
}

// MarshalJSON implements json.Marshaler.
func (c Currency) MarshalJSON() ([]byte, error) {
	if !c.IsValid() {
		return []byte("null"), nil
	}
	return json.Marshal(currencyJSON{Code: c.code, Scale: c.scale})
}

// UnmarshalJSON implements json.Unmarshaler. It validates the code and scale
// instead of trusting the payload.
func (c *Currency) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		*c = Currency{}
		return nil
	}
	var raw currencyJSON
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("%w: currency: %v", ErrInvalidCurrency, err)
	}
	parsed, err := NewCurrency(raw.Code, raw.Scale)
	if err != nil {
		return err
	}
	*c = parsed
	return nil
}

// MarshalJSON implements json.Marshaler. An unset amount marshals as null rather
// than as a zero in a guessed currency.
func (a Amount) MarshalJSON() ([]byte, error) {
	if !a.IsValid() {
		return []byte("null"), nil
	}
	return json.Marshal(amountJSON{
		Minor:    a.minor,
		Currency: currencyJSON{Code: a.cur.code, Scale: a.cur.scale},
	})
}

// UnmarshalJSON implements json.Unmarshaler.
func (a *Amount) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		*a = Amount{}
		return nil
	}
	var raw amountJSON
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("%w: amount: %v", ErrInvalidAmount, err)
	}
	cur, err := NewCurrency(raw.Currency.Code, raw.Currency.Scale)
	if err != nil {
		return err
	}
	*a = Amount{minor: raw.Minor, cur: cur}
	return nil
}
