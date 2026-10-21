package money

import (
	"encoding/json"
	"testing"
)

func TestAmountJSONRoundTrip(t *testing.T) {
	t.Parallel()
	usd := MustLookupCurrency("USD")
	jpy := MustLookupCurrency("JPY")
	kwd := MustLookupCurrency("KWD")
	custom := MustCurrency("ZZT", 3)

	cases := []Amount{
		New(1234, usd),
		New(-1234, usd),
		New(0, usd),
		New(1234, jpy),
		New(12345, kwd),
		New(999, custom),
	}
	for _, want := range cases {
		data, err := json.Marshal(want)
		if err != nil {
			t.Fatalf("marshal %s: %v", want.Format(), err)
		}
		var got Amount
		if err := json.Unmarshal(data, &got); err != nil {
			t.Fatalf("unmarshal %s: %v", data, err)
		}
		if !got.Equal(want) {
			t.Fatalf("round trip changed %s into %s (json %s)", want.Format(), got.Format(), data)
		}
		if got.Currency().Scale() != want.Currency().Scale() {
			t.Fatalf("scale changed: %d -> %d", want.Currency().Scale(), got.Currency().Scale())
		}
	}
}

func TestAmountJSONNeverUsesFloat(t *testing.T) {
	t.Parallel()
	usd := MustLookupCurrency("USD")
	data, err := json.Marshal(New(1234, usd))
	if err != nil {
		t.Fatal(err)
	}
	var probe map[string]any
	if err := json.Unmarshal(data, &probe); err != nil {
		t.Fatal(err)
	}
	if _, ok := probe["minor"].(float64); !ok {
		t.Fatalf("minor is not a JSON number: %s", data)
	}
	// The wire form must be the integer minor unit, not a decimal string or a
	// float, so no consumer can reintroduce binary floating point.
	if probe["minor"].(float64) != 1234 {
		t.Fatalf("minor = %v, want 1234", probe["minor"])
	}
}

func TestInvalidAmountMarshalsAsNull(t *testing.T) {
	t.Parallel()
	data, err := json.Marshal(Amount{})
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "null" {
		t.Fatalf("invalid amount marshalled as %s, want null", data)
	}
	var got Amount
	if err := json.Unmarshal([]byte("null"), &got); err != nil {
		t.Fatal(err)
	}
	if got.IsValid() {
		t.Fatal("null must unmarshal to an invalid amount")
	}
}

func TestCurrencyJSONRejectsBadPayload(t *testing.T) {
	t.Parallel()
	for _, payload := range []string{
		`{"code":"","scale":2}`,
		`{"code":"USD","scale":9}`,
		`{"code":"US$","scale":2}`,
		`{"code":123,"scale":2}`,
	} {
		var c Currency
		if err := json.Unmarshal([]byte(payload), &c); err == nil {
			t.Fatalf("%s was accepted", payload)
		}
	}
}
