package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/catalyst/billing/internal/payments"
)

// Webhook verification is delegated to the gateway extension.
//
// This is a regression test for a defect with a very specific symptom: every real Stripe delivery
// was refused with `invalid_signature`, `detail: "missing signature headers"`, and no invoice could
// ever be settled from a webhook. The core used payments.HMACSigner, which expected a separate
// `Stripe-Timestamp` header and a flat `{event_id, event_type, payment_id}` body. Stripe sends
// `Stripe-Signature: t=<ts>,v1=<sig>` — the timestamp inside the header — and nests the payment
// under `data.object`, so the core was verifying a scheme no provider speaks.
//
// The fix is not a better core-side parser: it is to stop parsing here at all. The extension is the
// only component that knows a provider's payload shape, so the core asks it. This test pins that
// wiring, because the failure is silent — the endpoint answers 400, which looks like a
// misconfiguration rather than a code path that can never succeed.
//
// It drives a real child process, because what is under test is a process boundary: an in-process
// fake would prove that the interface is callable, not that the core reaches the extension.
func TestTheWebhookVerifierDelegatesToTheExtension(t *testing.T) {
	bin := buildVerifyReporter(t)

	adapter := newGatewayAdapter("stripe").withLaunch(bin, nil)
	verifier := payments.ExtensionVerifier{Gateway: adapter}

	event, err := verifier.Verify(context.Background(),
		[]byte(`{"id":"evt_1","type":"checkout.session.completed"}`),
		map[string]string{"Stripe-Signature": "t=1700000000,v1=abc"})
	if err != nil {
		t.Fatalf("verify through the extension: %v", err)
	}

	// The extension is the authority on the event's identity, so these values must be the ones the
	// child returned, not anything the core derived.
	if event.EventID != "evt_from_extension" {
		t.Fatalf("event id = %q, want the extension's answer", event.EventID)
	}
	if event.EventType != "checkout.session.completed" {
		t.Fatalf("event type = %q, want the extension's answer", event.EventType)
	}
	if event.PaymentID != "pi_from_extension" {
		t.Fatalf("payment id = %q, want the extension's answer", event.PaymentID)
	}
}

// A rejection from the extension must surface as ErrInvalidSignature, not as a transport error.
//
// The distinction is the whole contract: the caller records a rejected delivery and acknowledges it,
// while a transport failure must be retried because the signature was never actually judged.
func TestAVerifierRejectionIsNotATransportFailure(t *testing.T) {
	bin := buildVerifyReporter(t)

	adapter := newGatewayAdapter("stripe").withLaunch(bin, []string{"VERIFY_REPORTER_MODE=reject"})
	verifier := payments.ExtensionVerifier{Gateway: adapter}

	_, err := verifier.Verify(context.Background(), []byte(`{"id":"evt_1"}`),
		map[string]string{"Stripe-Signature": "t=1700000000,v1=wrong"})
	if err == nil {
		t.Fatal("a rejected delivery was accepted")
	}
	if !isInvalidSignature(err) {
		t.Fatalf("error = %v, want payments.ErrInvalidSignature so the caller records a rejection "+
			"rather than retrying forever", err)
	}
}

func isInvalidSignature(err error) bool {
	for err != nil {
		if err == payments.ErrInvalidSignature {
			return true
		}
		unwrapper, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = unwrapper.Unwrap()
	}
	return false
}

// verifyReporter is a minimal extension: it speaks the handshake and implements
// `gateway.verify_webhook`, returning a fixed event so the test can tell the extension's answer
// apart from anything the core might have invented.
const verifyReporter = `package main

import (
	"bufio"
	"encoding/json"
	"os"
)

type req struct {
	JSONRPC string          ` + "`json:\"jsonrpc\"`" + `
	ID      json.RawMessage ` + "`json:\"id\"`" + `
	Method  string          ` + "`json:\"method\"`" + `
}

func main() {
	mode := os.Getenv("VERIFY_REPORTER_MODE")
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 64*1024), 4<<20)
	enc := json.NewEncoder(os.Stdout)
	for scanner.Scan() {
		var r req
		if err := json.Unmarshal(scanner.Bytes(), &r); err != nil {
			continue
		}
		var result any
		switch r.Method {
		case "initialize":
			result = map[string]any{
				"version":      1,
				"name":         "verify-reporter",
				"publisher":    "test",
				"capabilities": []string{"gateway.verify_webhook"},
			}
		case "gateway.verify_webhook":
			if mode == "reject" {
				result = map[string]any{"valid": false, "reason": "signature mismatch"}
			} else {
				result = map[string]any{
					"valid":      true,
					"event_id":   "evt_from_extension",
					"event_type": "checkout.session.completed",
					"payment_id": "pi_from_extension",
				}
			}
		default:
			_ = enc.Encode(map[string]any{
				"jsonrpc": "2.0", "id": r.ID,
				"error": map[string]any{"code": -32601, "message": "method not found"},
			})
			continue
		}
		_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "id": r.ID, "result": result})
	}
}
`

// buildVerifyReporter compiles the fixture above into a temporary binary.
func buildVerifyReporter(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	src := filepath.Join(dir, "main.go")
	if err := os.WriteFile(src, []byte(verifyReporter), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	bin := filepath.Join(dir, "verify-reporter")
	cmd := exec.Command("go", "build", "-o", bin, src)
	cmd.Env = os.Environ()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build fixture: %v\n%s", err, out)
	}
	return bin
}
