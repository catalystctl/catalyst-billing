package service_test

import (
	"context"
	"errors"
	"testing"

	"github.com/catalyst/billing/internal/service"
)

// Cancelling a provider-side subscription (SVC-14, CT-12).
//
// The property under test is that the panel's record and the queue row agree *before* the
// gateway is called: the worker can only be handed an id that the database already names, which
// is what makes the cancellation retryable after a crash.

func TestCancelExternalSubscriptionRecordsTheIdAndQueuesOneOperation(t *testing.T) {
	f := newFixture(t, 1000, "monthly")
	ctx := context.Background()

	// With no id anywhere there is nothing to cancel, and the refusal names what is missing.
	if _, err := f.store.CancelExternalSubscription(ctx, service.ExternalSubscriptionRequest{
		ServiceID: f.serviceID, Reason: "test",
	}); !errors.Is(err, service.ErrNoExternalSubscription) {
		t.Fatalf("err = %v, want ErrNoExternalSubscription", err)
	}

	res, err := f.store.CancelExternalSubscription(ctx, service.ExternalSubscriptionRequest{
		ServiceID:      f.serviceID,
		SubscriptionID: "sub_legacy_1",
		Reason:         "customer moved to a card",
		ActorID:        f.userID,
	})
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if res.OperationID == "" {
		t.Fatal("no operation was queued")
	}

	var (
		stored string
		kind   string
		key    string
		planID string
	)
	if err := f.pool.QueryRow(ctx,
		`SELECT external_subscription_id FROM services WHERE id = $1`, f.serviceID).Scan(&stored); err != nil {
		t.Fatalf("read service: %v", err)
	}
	if stored != "sub_legacy_1" {
		t.Fatalf("stored id = %q, want the recorded subscription", stored)
	}
	if err := f.pool.QueryRow(ctx, `
		SELECT kind, operation_key, payload->>'subscription_id'
		FROM service_operations WHERE id = $1`, res.OperationID).
		Scan(&kind, &key, &planID); err != nil {
		t.Fatalf("read operation: %v", err)
	}
	if kind != "cancel_subscription" {
		t.Fatalf("kind = %q, want cancel_subscription", kind)
	}
	if planID != "sub_legacy_1" {
		t.Fatalf("payload subscription = %q, want the id the worker must cancel", planID)
	}

	// A second click is the same operation: the key is the subscription, not the request.
	again, err := f.store.CancelExternalSubscription(ctx, service.ExternalSubscriptionRequest{
		ServiceID: f.serviceID, Reason: "again", ActorID: f.userID,
	})
	if err != nil {
		t.Fatalf("cancel again: %v", err)
	}
	if again.OperationID != res.OperationID {
		t.Fatalf("a second cancellation queued a different operation: %s vs %s",
			again.OperationID, res.OperationID)
	}
	if n := countRows(t, f.pool, "service_operations"); n != 1 {
		t.Fatalf("%d operations queued, want 1", n)
	}
}

// A terminated service has nothing left to stop, so the action is refused rather than queued
// forever against a record that no longer exists.
func TestCancelExternalSubscriptionRefusesATerminatedService(t *testing.T) {
	f := newFixture(t, 1000, "monthly")
	ctx := context.Background()
	if _, err := f.pool.Exec(ctx, `
		UPDATE services SET status = 'terminated', external_subscription_id = 'sub_x'
		WHERE id = $1`, f.serviceID); err != nil {
		t.Fatalf("terminate: %v", err)
	}
	_, err := f.store.CancelExternalSubscription(ctx, service.ExternalSubscriptionRequest{
		ServiceID: f.serviceID,
	})
	if !errors.Is(err, service.ErrNotCancellable) {
		t.Fatalf("err = %v, want ErrNotCancellable", err)
	}
}

// The order-level action cancels every service of the order that carries a provider subscription:
// stopping at the first one would leave the customer charged for the rest.
func TestCancelOrderExternalSubscriptionsQueuesEveryServiceOfTheOrder(t *testing.T) {
	f := newFixture(t, 1000, "monthly")
	ctx := context.Background()

	var orderID string
	if err := f.pool.QueryRow(ctx, `
		INSERT INTO orders (customer_id, status, currency, total_minor)
		VALUES ($1, 'paid', 'USD', 1000) RETURNING id`, f.customerID).Scan(&orderID); err != nil {
		t.Fatalf("insert order: %v", err)
	}
	if _, err := f.pool.Exec(ctx, `
		UPDATE services SET order_id = $2, external_subscription_id = 'sub_first'
		WHERE id = $1`, f.serviceID, orderID); err != nil {
		t.Fatalf("attach order: %v", err)
	}
	// A second service on the same order, with its own provider subscription.
	var secondID string
	if err := f.pool.QueryRow(ctx, `
		INSERT INTO services (customer_id, product_id, status, order_id, external_subscription_id)
		VALUES ($1, $2, 'active', $3, 'sub_second') RETURNING id`,
		f.customerID, f.productID, orderID).Scan(&secondID); err != nil {
		t.Fatalf("insert second service: %v", err)
	}

	queued, failures, err := f.store.CancelOrderExternalSubscriptions(ctx, orderID, f.userID, "closing the account")
	if err != nil {
		t.Fatalf("cancel order: %v", err)
	}
	if len(failures) != 0 {
		t.Fatalf("failures = %v, want none", failures)
	}
	if len(queued) != 2 {
		t.Fatalf("queued %d cancellations, want 2", len(queued))
	}
	if n := countRows(t, f.pool, "service_operations"); n != 2 {
		t.Fatalf("%d operations queued, want one per service", n)
	}

	// An order whose services are billed by this panel alone has nothing to cancel, and says so
	// instead of reporting a success that did nothing.
	if _, err := f.pool.Exec(ctx,
		`UPDATE services SET external_subscription_id = '' WHERE order_id = $1`, orderID); err != nil {
		t.Fatalf("clear ids: %v", err)
	}
	if _, _, err := f.store.CancelOrderExternalSubscriptions(ctx, orderID, f.userID, ""); !errors.Is(err, service.ErrNoExternalSubscription) {
		t.Fatalf("err = %v, want ErrNoExternalSubscription", err)
	}
}
