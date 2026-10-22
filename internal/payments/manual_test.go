package payments

import (
	"errors"
	"strings"
	"testing"
)

// The offline payment vocabulary and request validation.
//
// The allocation itself is proven against a real database in internal/httpapi (the endpoint) and
// internal/billing (the subledger). What is proven here is the part that decides what is even
// allowed through: the closed method set and the fields a manual payment must carry. Those are
// cheap to test without a database and expensive to get wrong — a method that is not in the set
// would fail the database CHECK at commit time, long after the operator filled in the form.

func TestOfflineMethodsIsTheClosedSet(t *testing.T) {
	got := OfflineMethods()
	want := []OfflineMethod{OfflineBankTransfer, OfflineCash, OfflineCheque, OfflineOther}
	if len(got) != len(want) {
		t.Fatalf("OfflineMethods() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("OfflineMethods()[%d] = %q, want %q", i, got[i], want[i])
		}
		if !got[i].Valid() {
			t.Fatalf("%q is not Valid()", got[i])
		}
		if got[i].Label() == "" || got[i].Label() == string(got[i]) {
			t.Fatalf("%q has no human label (got %q)", got[i], got[i].Label())
		}
	}

	// The values are the migration's CHECK constraint, so they must not drift silently.
	for _, m := range got {
		switch string(m) {
		case "bank_transfer", "cash", "cheque", "other":
		default:
			t.Fatalf("method %q is not in 0057's CHECK constraint", m)
		}
	}
}

func TestParseOfflineMethodNormalisesAndRefuses(t *testing.T) {
	for _, raw := range []string{"bank_transfer", "BANK_TRANSFER", " Bank_Transfer ", "cash", "CHEQUE", "other"} {
		m, err := ParseOfflineMethod(raw)
		if err != nil {
			t.Fatalf("ParseOfflineMethod(%q) = %v", raw, err)
		}
		if !m.Valid() {
			t.Fatalf("ParseOfflineMethod(%q) = %q, which is not valid", raw, m)
		}
	}

	for _, raw := range []string{"", "   ", "card", "Bank Transfer", "stripe", "wire"} {
		if _, err := ParseOfflineMethod(raw); !errors.Is(err, ErrInvalidOfflineMethod) {
			t.Fatalf("ParseOfflineMethod(%q) = %v, want ErrInvalidOfflineMethod", raw, err)
		}
	}
}

func TestManualPaymentValidate(t *testing.T) {
	valid := ManualPayment{
		InvoiceID:   "11111111-1111-1111-1111-111111111111",
		AmountMinor: 4200,
		Method:      OfflineBankTransfer,
		Reference:   "SEPA-1",
		Note:        "paid by the customer's accountant",
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("a valid payment was refused: %v", err)
	}

	cases := []struct {
		name   string
		mutate func(*ManualPayment)
		want   error
	}{
		{"no invoice", func(m *ManualPayment) { m.InvoiceID = "" }, ErrInvalidManualPayment},
		{"zero amount", func(m *ManualPayment) { m.AmountMinor = 0 }, ErrInvalidManualPayment},
		{"negative amount", func(m *ManualPayment) { m.AmountMinor = -1 }, ErrInvalidManualPayment},
		{"no method", func(m *ManualPayment) { m.Method = "" }, ErrInvalidOfflineMethod},
		{"unknown method", func(m *ManualPayment) { m.Method = "card" }, ErrInvalidOfflineMethod},
		{"empty reference", func(m *ManualPayment) { m.Reference = "" }, ErrInvalidManualPayment},
		{"whitespace reference", func(m *ManualPayment) { m.Reference = "   " }, ErrInvalidManualPayment},
		{"long reference", func(m *ManualPayment) { m.Reference = strings.Repeat("x", maxReferenceLen+1) }, ErrInvalidManualPayment},
		{"long note", func(m *ManualPayment) { m.Note = strings.Repeat("x", maxNoteLen+1) }, ErrInvalidManualPayment},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mp := valid
			tc.mutate(&mp)
			err := mp.Validate()
			if !errors.Is(err, tc.want) {
				t.Fatalf("Validate() = %v, want %v", err, tc.want)
			}
		})
	}

	// A reference at exactly the limit is allowed: the bound is a limit, not a threshold.
	atLimit := valid
	atLimit.Reference = strings.Repeat("r", maxReferenceLen)
	if err := atLimit.Validate(); err != nil {
		t.Fatalf("a reference at the limit was refused: %v", err)
	}
}
