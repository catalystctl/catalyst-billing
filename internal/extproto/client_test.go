package extproto

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/catalyst/billing/internal/money"
)

// echoParams is a test-only payload for the fixtures' "fixture.echo" method,
// which returns the params it received verbatim.
type echoParams struct {
	CallMeta
	Marker string `json:"marker"`
}

// TestGatewayHappyPath is the round-trip conformance test: handshake, capability
// negotiation, then every gateway method against the real fixture process.
func TestGatewayHappyPath(t *testing.T) {
	c := launchGateway(t)
	ctx := testCtx(t)

	info := c.Info()
	if info.Name != "fakegateway" {
		t.Errorf("name = %q, want fakegateway", info.Name)
	}
	if info.Publisher == "" {
		t.Error("publisher is empty")
	}
	if info.Version != Version {
		t.Errorf("version = %d, want %d", info.Version, Version)
	}
	caps := c.Capabilities()
	for _, want := range []string{
		MethodGatewayDescribe,
		MethodGatewayCreateCheckout,
		MethodGatewayQueryPayment,
		MethodGatewayRefund,
		MethodGatewayVerifyWebhook,
	} {
		if !contains(caps, want) {
			t.Errorf("capabilities %v do not include %q", caps, want)
		}
		if !info.HasCapability(want) {
			t.Errorf("HasCapability(%q) = false", want)
		}
	}

	var health HealthResult
	if err := c.Call(ctx, MethodHealth, &HealthParams{}, &health); err != nil {
		t.Fatalf("health: %v", err)
	}
	if health.Status != StatusOK {
		t.Fatalf("health status = %q, want %q", health.Status, StatusOK)
	}

	var desc GatewayDescribeResult
	if err := c.Call(ctx, MethodGatewayDescribe, &GatewayDescribeParams{}, &desc); err != nil {
		t.Fatalf("describe: %v", err)
	}
	if desc.Name != "fakegateway" || !desc.SupportsRefunds || len(desc.Methods) == 0 {
		t.Fatalf("unexpected describe result: %+v", desc)
	}
	if !contains(desc.Currencies, "USD") {
		t.Errorf("currencies %v do not include USD", desc.Currencies)
	}

	usd := mustUSD(t)
	checkout := &CreateCheckoutParams{
		MutatingMeta: MutatingMeta{OperationKey: "gw-checkout-1"},
		CustomerID:   "cus_1",
		Amount:       AmountOf(money.MustParse("12.34", usd)),
		Description:  "Pro plan, March",
		Metadata:     map[string]string{"invoice_id": "inv_1"},
	}
	var created CreateCheckoutResult
	if err := c.Call(ctx, MethodGatewayCreateCheckout, checkout, &created); err != nil {
		t.Fatalf("create_checkout: %v", err)
	}
	if created.PaymentID == "" || created.CheckoutURL == "" {
		t.Fatalf("incomplete checkout: %+v", created)
	}
	if created.Status != PaymentPending {
		t.Errorf("status = %q, want %q", created.Status, PaymentPending)
	}

	// Replaying the operation key must not create a second payment.
	var replay CreateCheckoutResult
	if err := c.Call(ctx, MethodGatewayCreateCheckout, checkout, &replay); err != nil {
		t.Fatalf("create_checkout replay: %v", err)
	}
	if replay.PaymentID != created.PaymentID {
		t.Fatalf("replay created %q, want the original %q", replay.PaymentID, created.PaymentID)
	}

	var queried QueryPaymentResult
	if err := c.Call(ctx, MethodGatewayQueryPayment, &QueryPaymentParams{PaymentID: created.PaymentID}, &queried); err != nil {
		t.Fatalf("query_payment: %v", err)
	}
	got, err := queried.Amount.Money()
	if err != nil {
		t.Fatalf("query_payment amount: %v", err)
	}
	if got.Minor() != 1234 || got.Currency().Code() != "USD" {
		t.Errorf("amount = %s, want 12.34 USD", got.Format())
	}
	if queried.AmountRefunded.Minor != 0 {
		t.Errorf("amount_refunded = %d, want 0", queried.AmountRefunded.Minor)
	}

	partial := AmountOf(money.MustParse("2.00", usd))
	refund := &RefundParams{
		MutatingMeta: MutatingMeta{OperationKey: "gw-refund-1"},
		PaymentID:    created.PaymentID,
		Amount:       &partial,
		Reason:       "customer request",
	}
	var refunded RefundResult
	if err := c.Call(ctx, MethodGatewayRefund, refund, &refunded); err != nil {
		t.Fatalf("refund: %v", err)
	}
	if refunded.Amount.Minor != 200 {
		t.Errorf("refunded %d minor units, want 200", refunded.Amount.Minor)
	}
	var refundReplay RefundResult
	if err := c.Call(ctx, MethodGatewayRefund, refund, &refundReplay); err != nil {
		t.Fatalf("refund replay: %v", err)
	}
	if refundReplay.RefundID != refunded.RefundID {
		t.Fatalf("refund replay created %q, want %q", refundReplay.RefundID, refunded.RefundID)
	}
	if err := c.Call(ctx, MethodGatewayQueryPayment, &QueryPaymentParams{PaymentID: created.PaymentID}, &queried); err != nil {
		t.Fatalf("query_payment after refund: %v", err)
	}
	if queried.AmountRefunded.Minor != 200 {
		t.Errorf("amount_refunded = %d, want 200", queried.AmountRefunded.Minor)
	}

	body := []byte(fmt.Sprintf(`{"event_id":"evt_1","event_type":"payment.succeeded","payment_id":%q,"amount":{"minor":1234,"currency":"USD"}}`, created.PaymentID))
	var webhook VerifyWebhookResult
	if err := c.Call(ctx, MethodGatewayVerifyWebhook, &VerifyWebhookParams{
		Headers: map[string]string{"X-Signature": "sha256=fake"},
		Body:    body,
	}, &webhook); err != nil {
		t.Fatalf("verify_webhook: %v", err)
	}
	if !webhook.Valid || webhook.EventID != "evt_1" || webhook.PaymentID != created.PaymentID {
		t.Fatalf("unexpected webhook result: %+v", webhook)
	}
	if webhook.Amount == nil || webhook.Amount.Minor != 1234 {
		t.Fatalf("webhook amount = %+v, want 1234 minor units", webhook.Amount)
	}

	// A bad signature is a valid answer, not a transport error.
	var badWebhook VerifyWebhookResult
	if err := c.Call(ctx, MethodGatewayVerifyWebhook, &VerifyWebhookParams{
		Headers: map[string]string{"X-Signature": "sha256=wrong"},
		Body:    body,
	}, &badWebhook); err != nil {
		t.Fatalf("verify_webhook with a bad signature: %v", err)
	}
	if badWebhook.Valid || badWebhook.Reason == "" {
		t.Fatalf("bad signature accepted: %+v", badWebhook)
	}

	// Unknown ids are a remote error with a code, not a crash.
	var missing QueryPaymentResult
	err = c.Call(ctx, MethodGatewayQueryPayment, &QueryPaymentParams{PaymentID: "pay_missing"}, &missing)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown payment: got %v, want ErrNotFound", err)
	}
	if !errors.Is(err, ErrRemote) {
		t.Fatalf("unknown payment: got %v, want ErrRemote too", err)
	}
	var rpcErr *Error
	if !errors.As(err, &rpcErr) {
		t.Fatalf("unknown payment: %v is not an *Error", err)
	}
	if rpcErr.Code != CodeNotFound || rpcErr.Method != MethodGatewayQueryPayment {
		t.Fatalf("unexpected error detail: %+v", rpcErr)
	}
}

// TestProvisionerHappyPath is the same round trip for the provisioner fixture.
func TestProvisionerHappyPath(t *testing.T) {
	c := launchProv(t)
	ctx := testCtx(t)

	if got := c.Info().Name; got != "fakeprov" {
		t.Errorf("name = %q, want fakeprov", got)
	}
	for _, want := range []string{
		MethodProvisionerDescribe,
		MethodProvisionerValidate,
		MethodProvisionerCreate,
		MethodProvisionerInspect,
		MethodProvisionerSuspend,
		MethodProvisionerResume,
		MethodProvisionerTerminate,
	} {
		if !contains(c.Capabilities(), want) {
			t.Errorf("capabilities %v do not include %q", c.Capabilities(), want)
		}
	}

	var desc ProvisionerDescribeResult
	if err := c.Call(ctx, MethodProvisionerDescribe, &ProvisionerDescribeParams{}, &desc); err != nil {
		t.Fatalf("describe: %v", err)
	}
	if len(desc.Plans) == 0 || len(desc.Regions) == 0 {
		t.Fatalf("unexpected describe result: %+v", desc)
	}
	if desc.Plans[0].Amount.Currency != "USD" || desc.Plans[0].Amount.Minor <= 0 {
		t.Fatalf("plan amount = %+v, want positive USD minor units", desc.Plans[0].Amount)
	}

	var valid ProvisionValidateResult
	if err := c.Call(ctx, MethodProvisionerValidate, &ProvisionValidateParams{
		PlanID: "small", Region: "eu-west", Quantity: 2,
	}, &valid); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if !valid.Valid || len(valid.Problems) != 0 {
		t.Fatalf("unexpected validate result: %+v", valid)
	}
	if valid.Amount == nil || valid.Amount.Minor != 1000 {
		t.Fatalf("validate amount = %+v, want 1000 minor units", valid.Amount)
	}

	var invalid ProvisionValidateResult
	if err := c.Call(ctx, MethodProvisionerValidate, &ProvisionValidateParams{
		PlanID: "enormous", Region: "mars", Quantity: 0,
	}, &invalid); err != nil {
		t.Fatalf("validate invalid: %v", err)
	}
	if invalid.Valid || len(invalid.Problems) != 3 {
		t.Fatalf("unexpected validate result: %+v", invalid)
	}

	create := &ProvisionCreateParams{
		MutatingMeta:   MutatingMeta{OperationKey: "sub-1-create"},
		SubscriptionID: "sub_1",
		PlanID:         "small",
		Region:         "eu-west",
		Quantity:       1,
	}
	var created ProvisionCreateResult
	if err := c.Call(ctx, MethodProvisionerCreate, create, &created); err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.ExternalID != "srv_1" || created.Status != ResourceActive {
		t.Fatalf("unexpected create result: %+v", created)
	}
	var replay ProvisionCreateResult
	if err := c.Call(ctx, MethodProvisionerCreate, create, &replay); err != nil {
		t.Fatalf("create replay: %v", err)
	}
	if replay.ExternalID != created.ExternalID {
		t.Fatalf("replay created %q, want %q", replay.ExternalID, created.ExternalID)
	}

	var inspected ProvisionInspectResult
	if err := c.Call(ctx, MethodProvisionerInspect, &ProvisionInspectParams{ExternalID: created.ExternalID}, &inspected); err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if inspected.Status != ResourceActive || inspected.PlanID != "small" {
		t.Fatalf("unexpected inspect result: %+v", inspected)
	}

	for _, tc := range []struct {
		method string
		params *ProvisionSuspendParams
		key    string
		want   ResourceStatus
	}{
		{MethodProvisionerSuspend, &ProvisionSuspendParams{MutatingMeta: MutatingMeta{OperationKey: "sub-1-suspend"}, ExternalID: created.ExternalID, Reason: "nonpayment"}, "sub-1-suspend", ResourceSuspended},
		{MethodProvisionerResume, nil, "sub-1-resume", ResourceActive},
		{MethodProvisionerTerminate, nil, "sub-1-terminate", ResourceTerminated},
	} {
		var res ProvisionStateResult
		var err error
		switch tc.method {
		case MethodProvisionerSuspend:
			err = c.Call(ctx, tc.method, tc.params, &res)
		case MethodProvisionerResume:
			err = c.Call(ctx, tc.method, &ProvisionResumeParams{
				MutatingMeta: MutatingMeta{OperationKey: tc.key},
				ExternalID:   created.ExternalID,
			}, &res)
		default:
			err = c.Call(ctx, tc.method, &ProvisionTerminateParams{
				MutatingMeta: MutatingMeta{OperationKey: tc.key},
				ExternalID:   created.ExternalID,
				Reason:       "subscription canceled",
			}, &res)
		}
		if err != nil {
			t.Fatalf("%s: %v", tc.method, err)
		}
		if res.Status != tc.want {
			t.Fatalf("%s status = %q, want %q", tc.method, res.Status, tc.want)
		}
		if !res.Changed {
			t.Fatalf("%s reported no change, want a change", tc.method)
		}
	}
}

// TestVersionMismatch covers both ways negotiation can fail: the extension
// refuses, or it answers with a version the core never offered.
func TestVersionMismatch(t *testing.T) {
	tests := []struct {
		name string
		env  []string
	}{
		{"extension refuses negotiation", []string{"FAKE_GATEWAY_NEGOTIATE=fail"}},
		{"extension answers an unoffered version", []string{"FAKE_GATEWAY_VERSION=99"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, err := Launch(context.Background(), gatewaySpec(tc.env...))
			if err == nil {
				c.Close()
				t.Fatal("Launch succeeded, want a version mismatch")
			}
			if c != nil {
				t.Fatal("Launch returned a client alongside an error")
			}
			if !errors.Is(err, ErrVersionMismatch) {
				t.Fatalf("got %v, want ErrVersionMismatch", err)
			}
		})
	}
}

// TestOversizedFrame proves that an oversized response is rejected and leaves the
// client unusable instead of hung.
func TestOversizedFrame(t *testing.T) {
	c := launchGateway(t, "FAKE_GATEWAY_MODE=oversized")
	ctx := testCtx(t)

	var health HealthResult
	start := time.Now()
	err := c.Call(ctx, MethodHealth, &HealthParams{}, &health)
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("oversized frame took %s to reject", elapsed)
	}
	if !errors.Is(err, ErrOversized) {
		t.Fatalf("got %v, want ErrOversized", err)
	}
	select {
	case <-c.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("client was not marked unusable after an oversized frame")
	}

	// The client must fail fast, not hang, on the next call.
	start = time.Now()
	err = c.Call(ctx, MethodHealth, &HealthParams{}, &health)
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("second call after an oversized frame took %s", elapsed)
	}
	if !errors.Is(err, ErrOversized) && !errors.Is(err, ErrClosed) {
		t.Fatalf("got %v, want ErrOversized or ErrClosed", err)
	}
}

// TestCallTimeout proves a hung extension cannot block a call past its deadline.
func TestCallTimeout(t *testing.T) {
	t.Run("caller deadline", func(t *testing.T) {
		c := launchGateway(t, "FAKE_GATEWAY_MODE=hang")
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		defer cancel()
		var health HealthResult
		start := time.Now()
		err := c.Call(ctx, MethodHealth, &HealthParams{}, &health)
		elapsed := time.Since(start)
		if !errors.Is(err, ErrTimeout) {
			t.Fatalf("got %v, want ErrTimeout", err)
		}
		if elapsed > 2*time.Second {
			t.Fatalf("call returned after %s, want it bounded by the 300ms deadline", elapsed)
		}
	})
	t.Run("call timeout", func(t *testing.T) {
		spec := gatewaySpec("FAKE_GATEWAY_MODE=hang")
		spec.CallTimeout = 250 * time.Millisecond
		c, err := Launch(context.Background(), spec)
		if err != nil {
			t.Fatalf("Launch: %v", err)
		}
		t.Cleanup(func() { c.Close() })
		var health HealthResult
		start := time.Now()
		err = c.Call(context.Background(), MethodHealth, &HealthParams{}, &health)
		elapsed := time.Since(start)
		if !errors.Is(err, ErrTimeout) {
			t.Fatalf("got %v, want ErrTimeout", err)
		}
		if elapsed > 2*time.Second {
			t.Fatalf("call returned after %s, want it bounded by the 250ms CallTimeout", elapsed)
		}
	})
}

// TestCancellation proves that a canceled call returns the canceled sentinel and
// leaves the client usable, and that the late reply is discarded rather than
// handed to the next caller.
func TestCancellation(t *testing.T) {
	c := launchGateway(t, "FAKE_GATEWAY_MODE=slow", "FAKE_GATEWAY_SLOW_MS=400")

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	var desc GatewayDescribeResult
	start := time.Now()
	err := c.Call(ctx, MethodGatewayDescribe, &GatewayDescribeParams{}, &desc)
	elapsed := time.Since(start)
	if !errors.Is(err, ErrCanceled) {
		t.Fatalf("got %v, want ErrCanceled", err)
	}
	if elapsed > 300*time.Millisecond {
		t.Fatalf("canceled call returned after %s", elapsed)
	}

	// The extension is still working on the abandoned request; the next call must
	// get its own answer, not the stale one.
	var health HealthResult
	if err := c.Call(testCtx(t), MethodHealth, &HealthParams{}, &health); err != nil {
		t.Fatalf("call after cancellation: %v", err)
	}
	if health.Status != StatusOK {
		t.Fatalf("call after cancellation returned %+v, want the health result", health)
	}
	select {
	case <-c.Done():
		t.Fatal("canceling one call killed the client")
	default:
	}
}

// TestCrash proves a dying extension fails the in-flight call and every later
// call, instead of leaving callers blocked forever.
func TestCrash(t *testing.T) {
	c := launchGateway(t, "FAKE_GATEWAY_MODE=crash")
	ctx := testCtx(t)

	usd := mustUSD(t)
	const callers = 10
	var wg sync.WaitGroup
	errs := make([]error, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			var res CreateCheckoutResult
			errs[i] = c.Call(ctx, MethodGatewayCreateCheckout, &CreateCheckoutParams{
				MutatingMeta: MutatingMeta{OperationKey: fmt.Sprintf("crash-%d", i)},
				CustomerID:   "cus_crash",
				Amount:       AmountOf(money.MustParse("1.00", usd)),
			}, &res)
		}(i)
	}
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("pending calls hung after the extension crashed")
	}
	for i, err := range errs {
		if !errors.Is(err, ErrCrashed) {
			t.Fatalf("caller %d: got %v, want ErrCrashed", i, err)
		}
	}

	// Later calls fail fast.
	start := time.Now()
	var health HealthResult
	err := c.Call(ctx, MethodHealth, &HealthParams{}, &health)
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("call after a crash took %s", elapsed)
	}
	if !errors.Is(err, ErrCrashed) {
		t.Fatalf("got %v, want ErrCrashed", err)
	}
	if !errors.Is(c.Err(), ErrCrashed) {
		t.Fatalf("Err() = %v, want ErrCrashed", c.Err())
	}
}

// TestConcurrentCalls proves 50 callers get their own results: no id mixups, no
// lost responses.
func TestConcurrentCalls(t *testing.T) {
	c := launchGateway(t)
	const callers = 50

	t.Run("echo", func(t *testing.T) {
		var wg sync.WaitGroup
		got := make([]string, callers)
		errs := make([]error, callers)
		for i := 0; i < callers; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				var out echoParams
				if err := c.Call(testCtx(t), "fixture.echo", &echoParams{Marker: fmt.Sprintf("marker-%02d", i)}, &out); err != nil {
					errs[i] = err
					return
				}
				got[i] = out.Marker
			}(i)
		}
		wg.Wait()
		for i := 0; i < callers; i++ {
			if errs[i] != nil {
				t.Fatalf("caller %d: %v", i, errs[i])
			}
			if want := fmt.Sprintf("marker-%02d", i); got[i] != want {
				t.Fatalf("caller %d got %q, want %q", i, got[i], want)
			}
		}
	})

	t.Run("create", func(t *testing.T) {
		usd := mustUSD(t)
		var wg sync.WaitGroup
		ids := make([]string, callers)
		errs := make([]error, callers)
		for i := 0; i < callers; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				var res CreateCheckoutResult
				err := c.Call(testCtx(t), MethodGatewayCreateCheckout, &CreateCheckoutParams{
					MutatingMeta: MutatingMeta{OperationKey: fmt.Sprintf("concurrent-%02d", i)},
					CustomerID:   fmt.Sprintf("cus_%02d", i),
					Amount:       AmountOf(money.MustParse("1.00", usd)),
				}, &res)
				if err != nil {
					errs[i] = err
					return
				}
				ids[i] = res.PaymentID
			}(i)
		}
		wg.Wait()
		seen := make(map[string]int, callers)
		for i := 0; i < callers; i++ {
			if errs[i] != nil {
				t.Fatalf("caller %d: %v", i, errs[i])
			}
			if ids[i] == "" {
				t.Fatalf("caller %d got an empty payment id", i)
			}
			if prev, dup := seen[ids[i]]; dup {
				t.Fatalf("callers %d and %d both got payment %q", prev, i, ids[i])
			}
			seen[ids[i]] = i
		}
	})
}

// TestEnvironmentIsolation proves the parent's environment never reaches the
// extension: a third-party program must not inherit the core's credentials.
func TestEnvironmentIsolation(t *testing.T) {
	t.Setenv("CATALYST_PARENT_ONLY_SECRET", "s3cr3t-dsn")
	c := launchGateway(t, "FAKE_GATEWAY_MODE=ok")

	var env struct {
		Vars map[string]string `json:"vars"`
	}
	if err := c.Call(testCtx(t), "fixture.env", nil, &env); err != nil {
		t.Fatalf("fixture.env: %v", err)
	}
	if v, leaked := env.Vars["CATALYST_PARENT_ONLY_SECRET"]; leaked {
		t.Fatalf("parent-only variable leaked into the extension: %q", v)
	}
	if got := env.Vars["FAKE_GATEWAY_MODE"]; got != "ok" {
		t.Fatalf("FAKE_GATEWAY_MODE = %q, want the explicitly passed value", got)
	}
	if _, ok := env.Vars["PATH"]; !ok {
		t.Fatal("PATH is missing from the child's environment")
	}
	// Spot-check that nothing else from the parent came along.
	for _, name := range []string{"HOME", "GOCACHE", "PWD"} {
		if _, ok := env.Vars[name]; ok {
			t.Errorf("%s leaked into the extension's environment", name)
		}
	}
}

// TestIdempotentCreateAcrossTimeout is the reason operation keys exist: the first
// create provisions a server and then loses its reply, so the retry must return
// that server instead of provisioning a second one.
func TestIdempotentCreateAcrossTimeout(t *testing.T) {
	c := launchProv(t, "FAKE_PROV_MODE=create_then_timeout")
	create := &ProvisionCreateParams{
		MutatingMeta:   MutatingMeta{OperationKey: "sub-42-create"},
		SubscriptionID: "sub_42",
		PlanID:         "small",
		Region:         "eu-west",
		Quantity:       1,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	var first ProvisionCreateResult
	err := c.Call(ctx, MethodProvisionerCreate, create, &first)
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("first create: got %v, want ErrTimeout (the fixture created the resource and never answered)", err)
	}

	// The retry replays the same operation key.
	var second ProvisionCreateResult
	if err := c.Call(testCtx(t), MethodProvisionerCreate, create, &second); err != nil {
		t.Fatalf("retried create: %v", err)
	}
	if second.ExternalID != "srv_1" {
		t.Fatalf("retry returned %q, want the resource created by the first attempt (srv_1)", second.ExternalID)
	}
	if second.Status != ResourceActive {
		t.Fatalf("retry status = %q, want active", second.Status)
	}

	// A different operation key is a different purchase, so the counter proves the
	// retry did not provision anything new.
	var other ProvisionCreateResult
	otherCreate := *create
	otherCreate.OperationKey = "sub-43-create"
	if err := c.Call(testCtx(t), MethodProvisionerCreate, &otherCreate, &other); err != nil {
		t.Fatalf("create with a new key: %v", err)
	}
	if other.ExternalID != "srv_2" {
		t.Fatalf("next purchase returned %q, want srv_2: the retry must not have provisioned a second resource", other.ExternalID)
	}

	// The first resource is still inspectable under the id the retry returned.
	var inspected ProvisionInspectResult
	if err := c.Call(testCtx(t), MethodProvisionerInspect, &ProvisionInspectParams{ExternalID: second.ExternalID}, &inspected); err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if inspected.Status != ResourceActive {
		t.Fatalf("inspected status = %q, want active", inspected.Status)
	}
}

// TestDuplicateFixtureIsNotIdempotent is the negative control for the test above:
// with the operation key ignored, the same retry provisions a second server. It
// documents what a non-idempotent extension looks like, so the assertion in
// TestIdempotentCreateAcrossTimeout is known to be able to fail.
func TestDuplicateFixtureIsNotIdempotent(t *testing.T) {
	c := launchProv(t, "FAKE_PROV_MODE=duplicate")
	create := &ProvisionCreateParams{
		MutatingMeta:   MutatingMeta{OperationKey: "sub-7-create"},
		SubscriptionID: "sub_7",
		PlanID:         "small",
		Region:         "eu-west",
		Quantity:       1,
	}
	var a, b ProvisionCreateResult
	if err := c.Call(testCtx(t), MethodProvisionerCreate, create, &a); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := c.Call(testCtx(t), MethodProvisionerCreate, create, &b); err != nil {
		t.Fatalf("create replay: %v", err)
	}
	if a.ExternalID == b.ExternalID {
		t.Fatalf("duplicate fixture returned the same id %q twice; it is supposed to be non-idempotent", a.ExternalID)
	}
}

// TestHandshakeGate proves no call other than initialize and health can be made
// before the handshake completes.
func TestHandshakeGate(t *testing.T) {
	c, err := launch(context.Background(), gatewaySpec(), false)
	if err != nil {
		t.Fatalf("launch without handshake: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	ctx := testCtx(t)

	var desc GatewayDescribeResult
	err = c.Call(ctx, MethodGatewayDescribe, &GatewayDescribeParams{}, &desc)
	if !errors.Is(err, ErrNotInitialized) {
		t.Fatalf("describe before initialize: got %v, want ErrNotInitialized", err)
	}
	var health HealthResult
	if err := c.Call(ctx, MethodHealth, &HealthParams{}, &health); err != nil {
		t.Fatalf("health before initialize: %v", err)
	}
	var info InitializeResult
	if err := c.Call(ctx, MethodInitialize, &InitializeParams{Protocol: Protocol, Versions: []int{Version}}, &info); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	if err := c.Call(ctx, MethodGatewayDescribe, &GatewayDescribeParams{}, &desc); err != nil {
		t.Fatalf("describe after initialize: %v", err)
	}
}

// TestDeadlineOnTheWire proves every call carries the effective deadline.
func TestDeadlineOnTheWire(t *testing.T) {
	c := launchGateway(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	start := time.Now()
	var out echoParams
	if err := c.Call(ctx, "fixture.echo", &echoParams{Marker: "deadline"}, &out); err != nil {
		t.Fatalf("fixture.echo: %v", err)
	}
	if out.Marker != "deadline" {
		t.Fatalf("marker = %q", out.Marker)
	}
	if out.Deadline.IsZero() {
		t.Fatal("no deadline reached the extension")
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("test context has no deadline")
	}
	if d := out.Deadline.Sub(deadline); d > time.Millisecond || d < -time.Millisecond {
		t.Fatalf("wire deadline %s differs from the context deadline %s", out.Deadline, deadline)
	}
	if out.Deadline.Before(start) {
		t.Fatalf("wire deadline %s is before the call started", out.Deadline)
	}
}

// TestOperationKeyRequired proves the client refuses to send a mutating call
// without an idempotency key, before it reaches the wire.
func TestOperationKeyRequired(t *testing.T) {
	c := launchGateway(t)
	usd := mustUSD(t)
	var res CreateCheckoutResult
	err := c.Call(testCtx(t), MethodGatewayCreateCheckout, &CreateCheckoutParams{
		CustomerID: "cus_1",
		Amount:     AmountOf(money.MustParse("1.00", usd)),
	}, &res)
	if !errors.Is(err, ErrInvalidParams) {
		t.Fatalf("got %v, want ErrInvalidParams", err)
	}
	// The client is untouched by a local validation failure.
	var health HealthResult
	if err := c.Call(testCtx(t), MethodHealth, &HealthParams{}, &health); err != nil {
		t.Fatalf("health after a refused call: %v", err)
	}
}

// TestStderrCapture proves stderr is captured, bounded and redacted.
func TestStderrCapture(t *testing.T) {
	noisy := "token=supersecret\n" + strings.Repeat("noise line\n", 4000)
	c, err := Launch(context.Background(), Spec{
		Path: fakeGatewayBin,
		Env:  []string{"FAKE_GATEWAY_STDERR=" + noisy},
		Redact: func(s string) string {
			return strings.ReplaceAll(s, "supersecret", "[redacted]")
		},
	})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	t.Cleanup(func() { c.Close() })

	waitFor(t, 5*time.Second, "stderr to be captured", func() bool {
		return len(c.Stderr()) > 0
	})
	// The extension writes 44 kB; only the bounded tail is kept.
	waitFor(t, 5*time.Second, "stderr to fill", func() bool {
		return len(c.Stderr()) >= stderrLimit-64
	})
	got := c.Stderr()
	if len(got) > stderrLimit {
		t.Fatalf("captured %d bytes of stderr, want at most %d", len(got), stderrLimit)
	}
	if !strings.Contains(got, "noise line") {
		t.Fatalf("captured stderr does not contain the output: %q", got)
	}
	if strings.Contains(got, "supersecret") {
		t.Fatalf("secret was not redacted: %q", got)
	}
}

// TestCloseIsIdempotentAndFinal proves Close can be called twice and that a closed
// client refuses work immediately.
func TestCloseIsIdempotentAndFinal(t *testing.T) {
	c, err := Launch(context.Background(), gatewaySpec())
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	select {
	case <-c.Done():
	default:
		t.Fatal("Done is not closed after Close")
	}
	start := time.Now()
	var health HealthResult
	err = c.Call(context.Background(), MethodHealth, &HealthParams{}, &health)
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("call after Close took %s", elapsed)
	}
	if !errors.Is(err, ErrClosed) {
		t.Fatalf("got %v, want ErrClosed", err)
	}
}

// TestNotify proves notifications are accepted and do not consume an id.
func TestNotify(t *testing.T) {
	c := launchGateway(t)
	if err := c.Notify(CancelMethod, &CancelParams{ID: 1, Reason: "test"}); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	var health HealthResult
	if err := c.Call(testCtx(t), MethodHealth, &HealthParams{}, &health); err != nil {
		t.Fatalf("health after a notification: %v", err)
	}
}

// TestNoGoroutineLeak proves a launch/close cycle leaves no goroutines behind.
func TestNoGoroutineLeak(t *testing.T) {
	// Warm up so one-off runtime goroutines (timers, pollers) are already up.
	for i := 0; i < 2; i++ {
		c, err := Launch(context.Background(), gatewaySpec())
		if err != nil {
			t.Fatalf("warm-up Launch: %v", err)
		}
		if err := c.Close(); err != nil {
			t.Fatalf("warm-up Close: %v", err)
		}
	}
	waitFor(t, 5*time.Second, "goroutines to settle", func() bool {
		runtime.GC()
		return runtime.NumGoroutine() <= 8
	})
	before := runtime.NumGoroutine()

	for i := 0; i < 3; i++ {
		c, err := Launch(context.Background(), gatewaySpec())
		if err != nil {
			t.Fatalf("Launch: %v", err)
		}
		var health HealthResult
		if err := c.Call(testCtx(t), MethodHealth, &HealthParams{}, &health); err != nil {
			t.Fatalf("health: %v", err)
		}
		if err := c.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	}

	waitFor(t, 5*time.Second, fmt.Sprintf("goroutine count to return to %d", before), func() bool {
		return runtime.NumGoroutine() <= before
	})
	if after := runtime.NumGoroutine(); after > before {
		buf := make([]byte, 1<<16)
		n := runtime.Stack(buf, true)
		t.Fatalf("goroutines grew from %d to %d after launch/close cycles:\n%s", before, after, buf[:n])
	}
}

// TestSpecValidation covers the local refusals.
func TestSpecValidation(t *testing.T) {
	tests := []struct {
		name string
		spec Spec
		want error
	}{
		{"no path", Spec{}, ErrInvalidSpec},
		{"frame too small", Spec{Path: "/bin/true", MaxFrame: 16}, ErrInvalidSpec},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Launch(context.Background(), tc.spec); !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
		})
	}
}

// TestMissingExecutable proves a bad Path is reported as a crashed extension
// rather than a bare exec error.
func TestMissingExecutable(t *testing.T) {
	_, err := Launch(context.Background(), Spec{Path: "/nonexistent/extension-binary"})
	if !errors.Is(err, ErrCrashed) {
		t.Fatalf("got %v, want ErrCrashed", err)
	}
}

// TestRemoteErrorDecoding proves the structured error survives the wire.
func TestRemoteErrorDecoding(t *testing.T) {
	c := launchGateway(t, "FAKE_GATEWAY_MODE=fail")
	err := c.Call(testCtx(t), MethodGatewayDescribe, &GatewayDescribeParams{}, nil)
	if !errors.Is(err, ErrRemote) {
		t.Fatalf("got %v, want ErrRemote", err)
	}
	var rpcErr *Error
	if !errors.As(err, &rpcErr) {
		t.Fatalf("%v is not an *Error", err)
	}
	if rpcErr.Code != -32000 {
		t.Errorf("code = %d, want -32000", rpcErr.Code)
	}
	if rpcErr.Method != MethodGatewayDescribe {
		t.Errorf("method = %q, want %q", rpcErr.Method, MethodGatewayDescribe)
	}
	var data map[string]string
	if err := json.Unmarshal(rpcErr.Data, &data); err != nil {
		t.Fatalf("error data %q is not a JSON object: %v", rpcErr.Data, err)
	}
	if data["mode"] != "fail" {
		t.Errorf("error data = %v, want mode=fail", data)
	}
	if code, ok := CodeOf(err); !ok || code != -32000 {
		t.Errorf("CodeOf = %d, %v", code, ok)
	}
}
