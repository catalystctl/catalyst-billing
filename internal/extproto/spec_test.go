package extproto

import (
	"encoding/json"
	"errors"
	"math/rand"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/catalyst/billing/internal/money"
)

// TestAmountWireRoundTrip is a property test: any minor-unit value in any ISO
// currency must survive the wire unchanged. Money that loses a unit on the way to
// a gateway is a wrong charge, so this is asserted for random values rather than
// for a few hand-picked ones.
func TestAmountWireRoundTrip(t *testing.T) {
	codes := []string{"USD", "EUR", "JPY", "KWD", "CLF", "GBP", "BHD"}
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 500; i++ {
		code := codes[rng.Intn(len(codes))]
		cur := money.MustLookupCurrency(code)
		minor := rng.Int63() - (1 << 62) // exercise both signs
		if i%7 == 0 {
			minor = int64(i) // and small values, including zero
		}
		in := money.New(minor, cur)

		wire := AmountOf(in)
		if wire.Minor != minor || wire.Currency != code {
			t.Fatalf("AmountOf(%s) = %+v", in.Format(), wire)
		}
		encoded, err := json.Marshal(wire)
		if err != nil {
			t.Fatalf("marshal %+v: %v", wire, err)
		}
		var decoded Amount
		if err := json.Unmarshal(encoded, &decoded); err != nil {
			t.Fatalf("unmarshal %s: %v", encoded, err)
		}
		back, err := decoded.Money()
		if err != nil {
			t.Fatalf("Money() for %s: %v", encoded, err)
		}
		if !back.Equal(in) {
			t.Fatalf("round trip changed %s into %s (json %s)", in.Format(), back.Format(), encoded)
		}
		if back.Currency().Scale() != cur.Scale() {
			t.Fatalf("round trip changed the scale of %s", code)
		}
	}
}

// TestAmountJSONIsInteger proves no amount ever reaches the wire as a float.
func TestAmountJSONIsInteger(t *testing.T) {
	usd := money.MustLookupCurrency("USD")
	rng := rand.New(rand.NewSource(2))
	for i := 0; i < 200; i++ {
		minor := rng.Int63n(1 << 40)
		encoded, err := json.Marshal(AmountOf(money.New(minor, usd)))
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var probe struct {
			Minor json.RawMessage `json:"minor"`
		}
		if err := json.Unmarshal(encoded, &probe); err != nil {
			t.Fatalf("unmarshal %s: %v", encoded, err)
		}
		raw := string(probe.Minor)
		if strings.ContainsAny(raw, ".eE") {
			t.Fatalf("amount %d encoded as a float: %s", minor, encoded)
		}
		if raw != strconv.FormatInt(minor, 10) {
			t.Fatalf("amount %d encoded as %s", minor, raw)
		}
	}
}

// TestAmountValidation covers the local refusals on the wire type.
func TestAmountValidation(t *testing.T) {
	tests := []struct {
		name    string
		amount  Amount
		wantErr bool
	}{
		{"usd", Amount{Minor: 100, Currency: "USD"}, false},
		{"zero usd", Amount{Minor: 0, Currency: "USD"}, false},
		{"negative usd", Amount{Minor: -5, Currency: "USD"}, false},
		{"jpy", Amount{Minor: 100, Currency: "JPY"}, false},
		{"unknown code", Amount{Minor: 100, Currency: "XYZ"}, true},
		{"lower case", Amount{Minor: 100, Currency: "usd"}, true},
		{"padded", Amount{Minor: 100, Currency: " USD"}, true},
		{"empty", Amount{Minor: 100}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.amount.Validate()
			if tc.wantErr != (err != nil) {
				t.Fatalf("Validate() = %v, want error: %v", err, tc.wantErr)
			}
			if err != nil && !errors.Is(err, ErrInvalidParams) {
				t.Fatalf("Validate() = %v, want ErrInvalidParams", err)
			}
		})
	}
}

// TestOperationKeyValidation covers the idempotency-token rules.
func TestOperationKeyValidation(t *testing.T) {
	tests := []struct {
		name    string
		key     string
		wantErr bool
	}{
		{"uuid", "8f14e45f-ceea-467a-9575-1b1c1f0b0b1e", false},
		{"simple", "sub_42:create:1", false},
		{"empty", "", true},
		{"only spaces", "   ", true},
		{"leading space", " key", true},
		{"trailing newline", "key\n", true},
		{"control character", "key\x00", true},
		{"too long", strings.Repeat("k", 201), true},
		{"just long enough", strings.Repeat("k", 200), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateOperationKey(tc.key)
			if tc.wantErr != (err != nil) {
				t.Fatalf("ValidateOperationKey(%q) = %v, want error: %v", tc.key, err, tc.wantErr)
			}
		})
	}
}

// TestMutatingPayloadsCarryOperationKey is a structural conformance check: every
// payload that changes remote state must embed MutatingMeta, expose the key to the
// client and accept a deadline. A new mutating method that forgets this would
// otherwise silently lose idempotency.
func TestMutatingPayloadsCarryOperationKey(t *testing.T) {
	payloads := []struct {
		method  string
		payload interface {
			SetDeadline(time.Time)
			OperationKeyValue() string
		}
	}{
		{MethodGatewayCreateCheckout, &CreateCheckoutParams{MutatingMeta: MutatingMeta{OperationKey: "k"}}},
		{MethodGatewayRefund, &RefundParams{MutatingMeta: MutatingMeta{OperationKey: "k"}}},
		{MethodProvisionerCreate, &ProvisionCreateParams{MutatingMeta: MutatingMeta{OperationKey: "k"}}},
		{MethodProvisionerSuspend, &ProvisionSuspendParams{MutatingMeta: MutatingMeta{OperationKey: "k"}}},
		{MethodProvisionerResume, &ProvisionResumeParams{MutatingMeta: MutatingMeta{OperationKey: "k"}}},
		{MethodProvisionerTerminate, &ProvisionTerminateParams{MutatingMeta: MutatingMeta{OperationKey: "k"}}},
	}
	for _, tc := range payloads {
		t.Run(tc.method, func(t *testing.T) {
			if got := tc.payload.OperationKeyValue(); got != "k" {
				t.Fatalf("OperationKeyValue() = %q", got)
			}
			deadline := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
			tc.payload.SetDeadline(deadline)
			encoded, err := json.Marshal(tc.payload)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var probe map[string]json.RawMessage
			if err := json.Unmarshal(encoded, &probe); err != nil {
				t.Fatalf("unmarshal %s: %v", encoded, err)
			}
			if _, ok := probe["operation_key"]; !ok {
				t.Fatalf("%s payload has no operation_key: %s", tc.method, encoded)
			}
			if _, ok := probe["deadline"]; !ok {
				t.Fatalf("%s payload has no deadline: %s", tc.method, encoded)
			}
		})
	}
}

// TestZeroDeadlineOmitted proves a payload without a deadline does not invent one.
func TestZeroDeadlineOmitted(t *testing.T) {
	encoded, err := json.Marshal(&HealthParams{})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(encoded) != "{}" {
		t.Fatalf("HealthParams encoded as %s, want {}", encoded)
	}
}

// TestCapabilitiesMatchMethods proves the capability strings are the method names,
// so an extension's handshake can be checked against the calls the core makes.
func TestCapabilitiesMatchMethods(t *testing.T) {
	pairs := map[string]string{
		CapGatewayDescribe:       MethodGatewayDescribe,
		CapGatewayCreateCheckout: MethodGatewayCreateCheckout,
		CapGatewayQueryPayment:   MethodGatewayQueryPayment,
		CapGatewayRefund:         MethodGatewayRefund,
		CapGatewayVerifyWebhook:  MethodGatewayVerifyWebhook,
		CapProvisionerDescribe:   MethodProvisionerDescribe,
		CapProvisionerValidate:   MethodProvisionerValidate,
		CapProvisionerCreate:     MethodProvisionerCreate,
		CapProvisionerInspect:    MethodProvisionerInspect,
		CapProvisionerSuspend:    MethodProvisionerSuspend,
		CapProvisionerResume:     MethodProvisionerResume,
		CapProvisionerTerminate:  MethodProvisionerTerminate,
	}
	for cap, method := range pairs {
		if cap != method {
			t.Errorf("capability %q is not the method name %q", cap, method)
		}
	}
	if Protocol != "billing.ext/1" {
		t.Errorf("Protocol = %q", Protocol)
	}
	if Version != 1 {
		t.Errorf("Version = %d", Version)
	}
}

// TestInitializeResultHasCapability covers the lookup helper.
func TestInitializeResultHasCapability(t *testing.T) {
	info := InitializeResult{Capabilities: []string{MethodGatewayRefund}}
	if !info.HasCapability(MethodGatewayRefund) {
		t.Error("HasCapability missed a listed capability")
	}
	if info.HasCapability(MethodGatewayCreateCheckout) {
		t.Error("HasCapability reported an unlisted capability")
	}
	if (InitializeResult{}).HasCapability(MethodGatewayRefund) {
		t.Error("HasCapability reported a capability on an empty result")
	}
}
