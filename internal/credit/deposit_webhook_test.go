package credit_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/catalyst/billing/internal/checkout"
	"github.com/catalyst/billing/internal/credit"
	"github.com/catalyst/billing/internal/extproto"
	"github.com/catalyst/billing/internal/fulfilment"
	"github.com/catalyst/billing/internal/payments"
)

// A deposit settles through the real webhook pipeline, not through a shortcut.
//
// The payments processor is the component that turns a stored event into money, and it has
// exactly one applier. `credit.DepositApplier` wraps the ordinary one and routes: a deposit
// invoice becomes customer credit, everything else is handed on unchanged. This test drives
// the whole chain — inbox, authoritative re-query, amount verification against the recorded
// attempt, allocation, grant — so the claim "the settled webhook posts customer_credit" is
// proven at the seam where a mistake would actually happen.
//
// It is also the integration contract for cmd/billing: the worker must construct
//
//	credit.NewDepositApplier(pool, fulfilment.NewApplier(pool))
//
// in place of fulfilment.NewApplier(pool). Nothing else changes.

// settlingGateway answers QueryPayment with a settled payment for any id.
type settlingGateway struct {
	// amount is what the provider reports having charged.
	amount extproto.Amount
	// invoiceID is echoed in the metadata, exactly as the gateway does for a real checkout.
	invoiceID string
}

func (g settlingGateway) Name() string { return "fakegateway" }

func (g settlingGateway) QueryPayment(_ context.Context, paymentID string) (extproto.QueryPaymentResult, error) {
	return extproto.QueryPaymentResult{
		PaymentID: paymentID,
		Status:    extproto.PaymentSucceeded,
		Amount:    g.amount,
		Metadata:  map[string]string{"invoice_id": g.invoiceID},
	}, nil
}

// deliver stores one signed-off event, the way the webhook handler does after verification.
func deliver(t *testing.T, inbox *payments.Inbox, paymentID string) {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"event_id":   "evt_" + paymentID,
		"event_type": "payment.succeeded",
		"payment_id": paymentID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := inbox.Receive(context.Background(), "fakegateway", "acct_test",
		payments.VerifiedEvent{
			EventID:   "evt_" + paymentID,
			EventType: "payment.succeeded",
			PaymentID: paymentID,
		}, body); err != nil {
		t.Fatalf("receive: %v", err)
	}
}

// TestDepositSettlesThroughTheWebhookPipeline proves the processor routes a deposit to the
// grant and that a redelivery does not credit twice.
func TestDepositSettlesThroughTheWebhookPipeline(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	deposit, err := f.svc.StartDeposit(ctx, credit.DepositRequest{
		CustomerID: f.customerID, AmountMinor: 2500, Currency: "USD", RequestedBy: f.userID,
	})
	if err != nil {
		t.Fatalf("start deposit: %v", err)
	}

	// The attempt the checkout recorded is what amount verification resolves. A settlement
	// with no recorded attempt is refused by the processor, so this is not optional setup.
	attempt, err := newestAttemptRef(ctx, f, deposit.InvoiceID)
	if err != nil {
		t.Fatal(err)
	}
	if attempt == "" {
		t.Fatal("the deposit invoice has no recorded attempt")
	}

	inbox := payments.NewInbox(f.pool)
	processor := payments.NewProcessor(f.pool, inbox,
		settlingGateway{
			amount:    extproto.Amount{Minor: 2500, Currency: "USD"},
			invoiceID: deposit.InvoiceID,
		},
		"acct_test",
		credit.NewDepositApplier(f.pool, fulfilment.NewApplier(f.pool)),
	).WithAttempts(checkout.LookupFor(f.pool))

	deliver(t, inbox, attempt)
	results, err := processor.Process(ctx, 10)
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	if len(results) != 1 || results[0].Duplicate {
		t.Fatalf("results = %+v", results)
	}
	if got := f.balance(t); got != 2500 {
		t.Fatalf("balance = %d, want 2500", got)
	}

	// A second, *different* event about the same payment is the case the provider documents
	// and the one the unique provider payment id exists for. It must not grant again.
	body, err := json.Marshal(map[string]any{
		"event_id": "evt_second", "event_type": "payment.succeeded", "payment_id": attempt,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := inbox.Receive(ctx, "fakegateway", "acct_test", payments.VerifiedEvent{
		EventID: "evt_second", EventType: "payment.succeeded", PaymentID: attempt,
	}, body); err != nil {
		t.Fatalf("receive second: %v", err)
	}
	if _, err := processor.Process(ctx, 10); err != nil {
		t.Fatalf("process second: %v", err)
	}
	if got := f.balance(t); got != 2500 {
		t.Fatalf("balance after a second event = %d, want 2500", got)
	}
	f.assertBalanced(t)
}

// A settlement that does not match the recorded deposit is refused rather than absorbed: a
// 0.01 payment against a 25.00 top-up is an anomaly, not a small deposit.
func TestDepositAmountMismatchIsRefused(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	deposit, err := f.svc.StartDeposit(ctx, credit.DepositRequest{
		CustomerID: f.customerID, AmountMinor: 2500, Currency: "USD", RequestedBy: f.userID,
	})
	if err != nil {
		t.Fatalf("start deposit: %v", err)
	}
	attempt, err := newestAttemptRef(ctx, f, deposit.InvoiceID)
	if err != nil || attempt == "" {
		t.Fatalf("attempt: %v (%q)", err, attempt)
	}

	inbox := payments.NewInbox(f.pool)
	processor := payments.NewProcessor(f.pool, inbox,
		settlingGateway{
			amount:    extproto.Amount{Minor: 1, Currency: "USD"},
			invoiceID: deposit.InvoiceID,
		},
		"acct_test",
		credit.NewDepositApplier(f.pool, fulfilment.NewApplier(f.pool)),
	).WithAttempts(checkout.LookupFor(f.pool))

	deliver(t, inbox, attempt)
	if _, err := processor.Process(ctx, 10); err == nil {
		t.Fatal("a mismatched settlement was accepted")
	}
	if got := f.balance(t); got != 0 {
		t.Fatalf("balance = %d, want 0: a refused settlement must credit nothing", got)
	}
	f.assertBalanced(t)
}

// newestAttemptRef reads the provider reference of an invoice's newest attempt.
func newestAttemptRef(ctx context.Context, f *fixture, invoiceID string) (string, error) {
	var ref *string
	err := f.pool.QueryRow(ctx, `
		SELECT provider_ref FROM payment_attempts WHERE invoice_id = $1
		ORDER BY created_at DESC LIMIT 1`, invoiceID).Scan(&ref)
	if err != nil {
		return "", err
	}
	if ref == nil {
		return "", nil
	}
	return *ref, nil
}
