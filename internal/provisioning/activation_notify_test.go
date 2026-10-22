package provisioning_test

// The activation mail is wired where the service actually becomes live.
//
// internal/notify proves what the message says; this proves it is *sent*. The difference
// matters: `products.email_template` was a column nothing read for two migrations precisely
// because no test connected the status change to the message. This test drives a real
// provisioning operation through the worker's own `finish` transaction, so removing the
// notification call from the "create" branch is a red test rather than a silent regression.

import (
	"context"
	"strings"
	"testing"

	"github.com/catalyst/billing/internal/notify"
	"github.com/catalyst/billing/internal/provisioning"
	"github.com/catalyst/billing/internal/store"
	"github.com/catalyst/billing/internal/store/storetest"
	"github.com/jackc/pgx/v5"
)

// seedActivationWorld creates a customer with a login, a product carrying an activation snippet,
// a pending service and one queued create operation.
func seedActivationWorld(t *testing.T, pool *store.Pool, snippet string) (serviceID, userID string) {
	t.Helper()
	ctx := context.Background()
	if err := pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var customerID, productID string
		if err := tx.QueryRow(ctx,
			`INSERT INTO customers (email, name) VALUES ('buyer@example.test', 'Buyer') RETURNING id`).
			Scan(&customerID); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx,
			`INSERT INTO products (slug, name, email_template) VALUES ('game', 'Game Server', $1) RETURNING id`,
			snippet).Scan(&productID); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `
			INSERT INTO users (email, password_hash, email_verified_at)
			VALUES ('buyer@example.test', 'x', now()) RETURNING id`).Scan(&userID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO customer_accounts (user_id, customer_id) VALUES ($1, $2)`,
			userID, customerID); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `
			INSERT INTO services (customer_id, product_id, status, generation, config)
			VALUES ($1, $2, 'pending', 1, '{"plan_id":"small","region":"eu-west"}')
			RETURNING id`, customerID, productID).Scan(&serviceID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO service_operations (service_id, kind, operation_key, generation, status)
			VALUES ($1, 'create', $2, 1, 'queued')`, serviceID, "create:"+serviceID)
		return err
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return serviceID, userID
}

// A successful create operation activates the service and mails the customer the product's own
// instructions, in the same transaction.
func TestActivationSendsTheProductInstructions(t *testing.T) {
	pool := storetest.DB(t)
	ctx := context.Background()
	if _, err := notify.SeedTemplates(ctx, pool); err != nil {
		t.Fatalf("seed templates: %v", err)
	}

	const snippet = "Your server listens on port 27015. Use the console in the panel to restart it."
	serviceID, userID := seedActivationWorld(t, pool, snippet)

	worker := provisioning.NewWorker(pool, newAdapter("fake", &memoryBackend{resources: map[string]string{}})).
		WithNotifier(notify.NewQueue(pool))

	res, err := worker.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if res.Outcome != "succeeded" {
		t.Fatalf("outcome = %q (%s)", res.Outcome, res.Detail)
	}
	if status, externalID := serviceState(t, pool, serviceID); status != "active" || externalID == "" {
		t.Fatalf("service = %q/%q, want active with an external id", status, externalID)
	}

	var body string
	if err := pool.QueryRow(ctx, `
		SELECT body FROM notification_deliveries
		WHERE kind = $1 AND to_address = 'buyer@example.test'`, notify.EventNewServerCreated).
		Scan(&body); err != nil {
		t.Fatalf("the activation mail was not queued: %v", err)
	}
	if !strings.Contains(body, snippet) {
		t.Fatalf("the activation mail does not carry the product snippet:\n%s", body)
	}
	if !strings.Contains(body, "Game Server") {
		t.Fatalf("the activation mail does not name the product:\n%s", body)
	}

	// The inbox row is written too, so the customer sees the activation in the panel.
	var inboxed int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM notifications
		WHERE user_id = $1 AND event_key = $2`, userID, notify.EventNewServerCreated).Scan(&inboxed); err != nil {
		t.Fatal(err)
	}
	if inboxed != 1 {
		t.Fatalf("inbox rows = %d, want 1", inboxed)
	}
}
