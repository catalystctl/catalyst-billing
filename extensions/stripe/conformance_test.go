package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/catalyst/billing/internal/extproto"
	"github.com/catalyst/billing/internal/money"
)

// Conformance: the core's own extension client, driving this adapter as a separate
// process, over real pipes.
//
// The unit tests above call the handler in-process, which proves the adapter's logic
// but not that it speaks the protocol. This test launches the built binary the way
// the panel does and drives it through extproto.Client, so a change to the envelope,
// the handshake, the deadline stamping or the error mapping fails here instead of in
// production. It is the same shape as the core's own conformance suite, applied to a
// real adapter rather than a fixture.

// stripeBin is the adapter, built once for the package's tests.
var stripeBin string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "stripe-ext-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "stripe tests: temp dir: %v\n", err)
		os.Exit(1)
	}
	defer os.RemoveAll(dir)

	out := filepath.Join(dir, "stripe")
	build := exec.Command("go", "build", "-o", out, ".")
	if b, err := build.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "stripe tests: building the adapter: %v\n%s", err, b)
		os.Exit(1)
	}
	stripeBin = out
	os.Exit(m.Run())
}

// launch drives the adapter through the core's client, pointed at a fake Stripe.
//
// The environment is the whole configuration surface: the client hands the child
// nothing else, which is also what proves the adapter needs no secret from the
// core's own environment.
func launch(t *testing.T, f *fakeStripe) *extproto.Client {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	client, err := extproto.Launch(ctx, extproto.Spec{
		Path: stripeBin,
		Env: []string{
			"STRIPE_SECRET_KEY=sk_test_123",
			"STRIPE_WEBHOOK_SECRET=" + f.webhookKey,
			"STRIPE_API_BASE=" + f.baseURL(),
			"STRIPE_CURRENCIES=USD,EUR",
			"STRIPE_SAVED_CARDS=true",
		},
		CoreVersion:      "test",
		HandshakeTimeout: 10 * time.Second,
		CallTimeout:      10 * time.Second,
	})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// The handshake must advertise the five gateway methods, because the core refuses
// to call one that was not advertised.
func TestConformanceHandshakeAdvertisesGatewayMethods(t *testing.T) {
	f := newFakeStripe(t)
	client := launch(t, f)

	init := client.Info()
	if init.Name != "stripe-gateway" {
		t.Fatalf("name = %q", init.Name)
	}
	for _, method := range []string{
		extproto.MethodGatewayDescribe,
		extproto.MethodGatewayCreateCheckout,
		extproto.MethodGatewayQueryPayment,
		extproto.MethodGatewayRefund,
		extproto.MethodGatewayVerifyWebhook,
	} {
		if !init.HasCapability(method) {
			t.Fatalf("the adapter did not advertise %s", method)
		}
	}
}

// Describe over the wire: the core reads this to decide whether to offer the
// gateway at all, so a malformed reply would silently disable payments.
func TestConformanceDescribe(t *testing.T) {
	f := newFakeStripe(t)
	client := launch(t, f)
	ctx := context.Background()

	var out extproto.GatewayDescribeResult
	if err := client.Call(ctx, extproto.MethodGatewayDescribe, &extproto.GatewayDescribeParams{}, &out); err != nil {
		t.Fatalf("describe: %v", err)
	}
	if out.Name != "stripe" {
		t.Fatalf("name = %q", out.Name)
	}
	if !out.SupportsRefunds || !out.SupportsWebhooks {
		t.Fatalf("describe understates: %+v", out)
	}
	if len(out.Currencies) == 0 {
		t.Fatal("no currencies advertised, so the panel would offer nothing")
	}
}

// The full money path through the real client: create a checkout, have the
// customer pay, read the provider's state back, refund part of it, then verify the
// webhook that reports the settlement.
//
// This is the sequence the panel performs, and every step goes through the wire.
func TestConformanceCheckoutSettleRefundAndWebhook(t *testing.T) {
	f := newFakeStripe(t)
	client := launch(t, f)
	ctx := context.Background()

	// 1. Create the checkout the way the pay flow does.
	amount := money.New(4200, money.MustLookupCurrency("USD"))
	var created extproto.CreateCheckoutResult
	if err := client.Call(ctx, extproto.MethodGatewayCreateCheckout, &extproto.CreateCheckoutParams{
		MutatingMeta: extproto.MutatingMeta{OperationKey: "conformance-checkout-1"},
		CustomerID:   "cust_conformance",
		Amount:       extproto.AmountOf(amount),
		Description:  "Conformance plan",
		SuccessURL:   "https://panel.test/checkout/complete",
		CancelURL:    "https://panel.test/checkout/cancelled",
		Metadata:     map[string]string{"invoice_id": "inv_conformance"},
	}, &created); err != nil {
		t.Fatalf("create checkout: %v", err)
	}
	if created.PaymentID == "" || created.CheckoutURL == "" {
		t.Fatalf("incomplete checkout: %+v", created)
	}
	if created.Status != extproto.PaymentPending {
		t.Fatalf("status = %q, want pending", created.Status)
	}

	// 2. The customer pays.
	f.markPaid(created.PaymentID)

	// 3. The core asks the provider what happened, rather than trusting a redirect.
	var queried extproto.QueryPaymentResult
	if err := client.Call(ctx, extproto.MethodGatewayQueryPayment, &extproto.QueryPaymentParams{
		PaymentID: created.PaymentID,
	}, &queried); err != nil {
		t.Fatalf("query payment: %v", err)
	}
	if queried.Status != extproto.PaymentSucceeded {
		t.Fatalf("status = %q, want succeeded", queried.Status)
	}
	// The amount the provider reports is what the core verifies against its
	// recorded checkout, so it must survive the wire exactly.
	gotAmount, err := queried.Amount.Money()
	if err != nil {
		t.Fatalf("amount: %v", err)
	}
	if gotAmount.Minor() != 4200 || gotAmount.Currency().Code() != "USD" {
		t.Fatalf("amount = %s, want 4200 USD", gotAmount.Format())
	}
	// The invoice the checkout was opened for must come back with it: the core
	// reads it from the payment's metadata, not from the webhook body.
	if queried.Metadata["invoice_id"] != "inv_conformance" {
		t.Fatalf("metadata = %v, want the invoice id", queried.Metadata)
	}

	// 4. A partial refund, then the same refund again: a retry must not refund twice.
	refundParams := extproto.RefundParams{
		MutatingMeta: extproto.MutatingMeta{OperationKey: "conformance-refund-1"},
		PaymentID:    created.PaymentID,
		Amount:       &extproto.Amount{Minor: 1200, Currency: "USD"},
		Reason:       "requested_by_customer",
	}
	var first extproto.RefundResult
	if err := client.Call(ctx, extproto.MethodGatewayRefund, &refundParams, &first); err != nil {
		t.Fatalf("refund: %v", err)
	}
	if first.Amount.Minor != 1200 {
		t.Fatalf("refunded %d, want 1200", first.Amount.Minor)
	}
	var second extproto.RefundResult
	if err := client.Call(ctx, extproto.MethodGatewayRefund, &refundParams, &second); err != nil {
		t.Fatalf("replayed refund: %v", err)
	}
	if second.RefundID != first.RefundID {
		t.Fatalf("a replayed refund produced a second refund: %q then %q", first.RefundID, second.RefundID)
	}

	// 5. The webhook that reports the settlement, signed as the provider signs it.
	now := time.Now()
	body := []byte(fmt.Sprintf(`{
		"id": "evt_conformance",
		"type": "checkout.session.completed",
		"created": %d,
		"data": {"object": {
			"id": %q,
			"object": "checkout.session",
			"payment_status": "paid",
			"amount_total": 4200,
			"currency": "usd",
			"payment_intent": %q
		}}
	}`, now.Unix(), created.PaymentID, f.sessions[created.PaymentID].IntentID))

	var verified extproto.VerifyWebhookResult
	if err := client.Call(ctx, extproto.MethodGatewayVerifyWebhook, &extproto.VerifyWebhookParams{
		Headers: map[string]string{"Stripe-Signature": f.signWebhook(body, now)},
		Body:    body,
	}, &verified); err != nil {
		t.Fatalf("verify webhook: %v", err)
	}
	if !verified.Valid {
		t.Fatalf("an authentic delivery was refused over the wire: %s", verified.Reason)
	}
	if verified.EventID != "evt_conformance" {
		t.Fatalf("event id = %q", verified.EventID)
	}
	if verified.PaymentID != created.PaymentID {
		t.Fatalf("payment id = %q, want %q", verified.PaymentID, created.PaymentID)
	}
	if verified.Amount == nil || verified.Amount.Minor != 4200 {
		t.Fatalf("amount = %+v", verified.Amount)
	}
}

// A declined card must reach the core as a rejection, not a transport failure:
// that distinction is what decides whether the invoice stays open with a message
// the customer can act on, or disappears into a retry queue.
func TestConformanceDeclineIsReportedAsARejection(t *testing.T) {
	f := newFakeStripe(t)
	client := launch(t, f)
	ctx := context.Background()
	f.failWith(402, `{"error":{"type":"card_error","code":"card_declined","decline_code":"insufficient_funds","message":"Your card has insufficient funds."}}`)

	var out extproto.CreateCheckoutResult
	err := client.Call(ctx, extproto.MethodGatewayCreateCheckout, &extproto.CreateCheckoutParams{
		MutatingMeta: extproto.MutatingMeta{OperationKey: "conformance-decline"},
		CustomerID:   "cust_decline",
		Amount:       extproto.Amount{Minor: 500, Currency: "USD"},
		SuccessURL:   "https://panel.test/complete",
		CancelURL:    "https://panel.test/cancelled",
	}, &out)
	if err == nil {
		t.Fatal("a declined card was reported as a successful checkout")
	}
	if !errors.Is(err, extproto.ErrRejected) {
		t.Fatalf("a decline was not reported as a rejection: %v", err)
	}
}

// An unconfigured adapter must fail to start rather than start and refuse every
// payment, so a missing key is a deployment error the operator sees immediately.
func TestConformanceRefusesToStartWithoutASecretKey(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, err := extproto.Launch(ctx, extproto.Spec{
		Path:             stripeBin,
		Env:              []string{"STRIPE_API_BASE=http://127.0.0.1:1"},
		CoreVersion:      "test",
		HandshakeTimeout: 5 * time.Second,
		CallTimeout:      5 * time.Second,
	})
	if err == nil {
		t.Fatal("the adapter started with no STRIPE_SECRET_KEY")
	}
}

// jsonBody is a helper for asserting on a raw result when a test needs the wire
// form rather than a decoded struct.
func jsonBody(t *testing.T, v any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return out
}
