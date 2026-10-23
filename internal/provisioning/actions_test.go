package provisioning_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/catalyst/billing/internal/extproto"
	"github.com/catalyst/billing/internal/provisioning"
	"github.com/catalyst/billing/internal/store"
	"github.com/catalyst/billing/internal/store/storetest"
	"github.com/jackc/pgx/v5"
)

// The operator actions the queue could not execute: a resize, a remote deletion and a
// subscription cancellation (SVC-16, SVC-22, UPG-05, SVC-14, CT-12, UPG-10, UPG-12).
//
// Every test here is about the same invariant: the remote call happens in the *worker*, from a
// row, with the instruction the row carries. A test that proved the handler enqueued something
// would not prove the server changed.

// recordingAdapter is a provisioner that remembers what it was asked, so the assertions can be
// about the request rather than about a mock's call count.
type recordingAdapter struct {
	mu       sync.Mutex
	creates  []provisioning.CreateRequest
	upgrades []provisioning.UpgradeRequest
	terms    []string
	// upgradeErr, when set, is what Upgrade returns. It stands in for an adapter that does
	// not advertise the method.
	upgradeErr error
}

func (a *recordingAdapter) Name() string           { return "recording" }
func (a *recordingAdapter) Capabilities() []string { return []string{"create", "upgrade"} }

func (a *recordingAdapter) Create(_ context.Context, req provisioning.CreateRequest) (extproto.ProvisionCreateResult, error) {
	a.mu.Lock()
	a.creates = append(a.creates, req)
	a.mu.Unlock()
	return extproto.ProvisionCreateResult{ExternalID: "ext-created", Status: extproto.ResourceActive}, nil
}

func (a *recordingAdapter) Lookup(context.Context, string) (extproto.ProvisionLookupResult, error) {
	return extproto.ProvisionLookupResult{Found: false}, nil
}

func (a *recordingAdapter) Inspect(_ context.Context, externalID string) (extproto.ProvisionInspectResult, error) {
	return extproto.ProvisionInspectResult{ExternalID: externalID, Status: extproto.ResourceActive}, nil
}

func (a *recordingAdapter) Suspend(context.Context, string, string) (extproto.ProvisionStateResult, error) {
	return extproto.ProvisionStateResult{Status: extproto.ResourceSuspended}, nil
}

func (a *recordingAdapter) Resume(context.Context, string) (extproto.ProvisionStateResult, error) {
	return extproto.ProvisionStateResult{Status: extproto.ResourceActive}, nil
}

func (a *recordingAdapter) Terminate(_ context.Context, externalID, reason string) (extproto.ProvisionStateResult, error) {
	a.mu.Lock()
	a.terms = append(a.terms, externalID+"/"+reason)
	a.mu.Unlock()
	return extproto.ProvisionStateResult{ExternalID: externalID, Status: extproto.ResourceTerminated, Changed: true}, nil
}

func (a *recordingAdapter) Upgrade(_ context.Context, req provisioning.UpgradeRequest) (extproto.ProvisionUpgradeResult, error) {
	a.mu.Lock()
	a.upgrades = append(a.upgrades, req)
	a.mu.Unlock()
	if a.upgradeErr != nil {
		return extproto.ProvisionUpgradeResult{}, a.upgradeErr
	}
	return extproto.ProvisionUpgradeResult{
		ExternalID: req.ExternalID, Status: extproto.ResourceActive,
		PlanID: req.PlanID, Changed: true,
	}, nil
}

// recordingCanceller is the gateway side of a subscription cancellation.
type recordingCanceller struct {
	mu       sync.Mutex
	requests []provisioning.CancelSubscriptionRequest
}

func (c *recordingCanceller) CancelSubscription(_ context.Context, req provisioning.CancelSubscriptionRequest) (extproto.GatewayCancelSubscriptionResult, error) {
	c.mu.Lock()
	c.requests = append(c.requests, req)
	c.mu.Unlock()
	return extproto.GatewayCancelSubscriptionResult{
		SubscriptionID: req.SubscriptionID, Status: "canceled", Canceled: true,
	}, nil
}

// actionFixture builds a provisioned service with one priced config option, and returns the ids
// the operations are queued against.
type actionFixture struct {
	pool       *store.Pool
	serviceID  string
	customerID string
	productID  string
}

func newActionFixture(t *testing.T, config string, externalID string) actionFixture {
	t.Helper()
	pool := storetest.DB(t)
	ctx := context.Background()
	f := actionFixture{pool: pool}

	err := pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx,
			`INSERT INTO customers (email, name) VALUES ('a@example.test', 'A') RETURNING id`).
			Scan(&f.customerID); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx,
			`INSERT INTO products (slug, name) VALUES ('game', 'Game') RETURNING id`).
			Scan(&f.productID); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `
			INSERT INTO services (customer_id, product_id, status, generation, external_id, config)
			VALUES ($1, $2, 'active', 1, NULLIF($3, ''), $4::jsonb)
			RETURNING id`, f.customerID, f.productID, externalID, config).Scan(&f.serviceID); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	return f
}

// addOption adds a priced config option with an environment variable and selects it on the
// service, which is the UPG-10 shape: the selection lives on the service, and the option names
// the variable the adapter must set.
func (f actionFixture) addOption(t *testing.T, name, env, value string) {
	t.Helper()
	ctx := context.Background()
	err := f.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var optionID string
		if err := tx.QueryRow(ctx, `
			INSERT INTO config_options (product_id, name, option_type, choices, env_variable)
			VALUES ($1, $2, 'text', '[]'::jsonb, $3)
			RETURNING id`, f.productID, name, env).Scan(&optionID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO service_configs (service_id, config_option_id, value, amount_minor)
			VALUES ($1, $2, $3, 0)`, f.serviceID, optionID, value)
		return err
	})
	if err != nil {
		t.Fatalf("add option: %v", err)
	}
}

// enqueue inserts one operation the way an operator action does.
func (f actionFixture) enqueue(t *testing.T, kind, key string, payload map[string]any) {
	t.Helper()
	if payload == nil {
		payload = map[string]any{}
	}
	encoded := encodeJSON(t, payload)
	_, err := f.pool.Exec(context.Background(), `
		INSERT INTO service_operations (service_id, kind, operation_key, generation, status, payload)
		VALUES ($1, $2, $3, 1, 'queued', $4::jsonb)`, f.serviceID, kind, key, encoded)
	if err != nil {
		t.Fatalf("enqueue %s: %v", kind, err)
	}
}

func (f actionFixture) operation(t *testing.T, key string) (status, detail string) {
	t.Helper()
	if err := f.pool.QueryRow(context.Background(),
		`SELECT status, COALESCE(last_error, '') FROM service_operations WHERE operation_key = $1`,
		key).Scan(&status, &detail); err != nil {
		t.Fatalf("read operation: %v", err)
	}
	return status, detail
}

func (f actionFixture) auditActions(t *testing.T) []string {
	t.Helper()
	rows, err := f.pool.Query(context.Background(), `
		SELECT action FROM audit_events WHERE target_id = $1 ORDER BY id`, f.serviceID)
	if err != nil {
		t.Fatalf("read audit: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var action string
		if err := rows.Scan(&action); err != nil {
			t.Fatalf("scan audit: %v", err)
		}
		out = append(out, action)
	}
	return out
}

// TestResizeReachesTheAdapterAndRecordsThePlan is SVC-22: the ledger moved and the server did
// not. The assertion is that the adapter was told the *target* plan and that the service now
// says so, because the next create or inspect reads the plan from the service.
func TestResizeReachesTheAdapterAndRecordsThePlan(t *testing.T) {
	f := newActionFixture(t, `{"plan_id":"small","region":"eu-west"}`, "srv-1")
	f.addOption(t, "Minecraft version", "MINECRAFT_VERSION", "1.21.4")
	f.enqueue(t, "resize", "resize:1:price-2:1", map[string]any{
		"plan_id": "medium", "region": "eu-central", "price_version_id": "price-2",
	})

	adapter := &recordingAdapter{}
	worker := provisioning.NewWorker(f.pool, adapter)
	if _, err := worker.RunOnce(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}

	if len(adapter.upgrades) != 1 {
		t.Fatalf("%d upgrade calls, want 1", len(adapter.upgrades))
	}
	req := adapter.upgrades[0]
	if req.PlanID != "medium" {
		t.Fatalf("plan = %q, want the payload's medium (not the service's old small)", req.PlanID)
	}
	if req.ExternalID != "srv-1" {
		t.Fatalf("external id = %q, want srv-1", req.ExternalID)
	}
	// UPG-10 / SVC-21: the customer's chosen options travel with the resize.
	if req.Metadata["env.MINECRAFT_VERSION"] != "1.21.4" {
		t.Fatalf("metadata after resize = %v, want env.MINECRAFT_VERSION", req.Metadata)
	}

	var plan, region string
	if err := f.pool.QueryRow(context.Background(),
		`SELECT config->>'plan_id', config->>'region' FROM services WHERE id = $1`,
		f.serviceID).Scan(&plan, &region); err != nil {
		t.Fatalf("read config: %v", err)
	}
	if plan != "medium" || region != "eu-central" {
		t.Fatalf("service config = %s/%s, want medium/eu-central", plan, region)
	}

	status, _ := f.operation(t, "resize:1:price-2:1")
	if status != "succeeded" {
		t.Fatalf("operation status = %q, want succeeded", status)
	}
	actions := f.auditActions(t)
	found := false
	for _, a := range actions {
		if a == "service.resized" {
			found = true
		}
	}
	if !found {
		t.Fatalf("audit actions = %v, want service.resized", actions)
	}
}

// A resize of a service that was never provisioned is a no-op with a truthful outcome: retrying
// it cannot help, and failing it would leave the row stuck for a service that has nothing remote.
func TestResizeWithoutRemoteResourceSucceedsWithoutCallingTheAdapter(t *testing.T) {
	f := newActionFixture(t, `{"plan_id":"small"}`, "")
	f.enqueue(t, "resize", "resize:1:price-2:1", map[string]any{"plan_id": "medium"})

	adapter := &recordingAdapter{}
	worker := provisioning.NewWorker(f.pool, adapter)
	res, err := worker.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Outcome != "succeeded" {
		t.Fatalf("outcome = %q, want succeeded", res.Outcome)
	}
	if len(adapter.upgrades) != 0 {
		t.Fatalf("%d upgrade calls on an unprovisioned service", len(adapter.upgrades))
	}
}

// An adapter that does not implement the upgrade is parked for review, not retried: the remedy
// is an operator's decision, and five fast attempts would bury the reason.
func TestResizeOnAnAdapterWithoutUpgradeParksForReview(t *testing.T) {
	f := newActionFixture(t, `{"plan_id":"small"}`, "srv-1")
	f.enqueue(t, "resize", "resize:1:price-2:1", map[string]any{"plan_id": "medium"})

	adapter := &recordingAdapter{upgradeErr: provisioning.ErrUnsupportedOperation}
	worker := provisioning.NewWorker(f.pool, adapter)
	res, err := worker.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Outcome != "needs_review" {
		t.Fatalf("outcome = %q, want needs_review", res.Outcome)
	}
	status, detail := f.operation(t, "resize:1:price-2:1")
	if status != "needs_review" {
		t.Fatalf("status = %q, want needs_review", status)
	}
	if detail == "" {
		t.Fatal("the parked operation carries no reason for the operator")
	}
}

// TestDeleteRemoteClearsTheIdAndKeepsTheService is SVC-12's "delete remote": the provider's
// resource is gone and the customer keeps what they bought.
func TestDeleteRemoteClearsTheIdAndKeepsTheService(t *testing.T) {
	f := newActionFixture(t, `{"plan_id":"small"}`, "srv-1")
	f.enqueue(t, "delete_remote", "delete_remote:1:1", map[string]any{"reason": "rebuilt by hand"})

	adapter := &recordingAdapter{}
	worker := provisioning.NewWorker(f.pool, adapter)
	if _, err := worker.RunOnce(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(adapter.terms) != 1 || adapter.terms[0] != "srv-1/delete_remote" {
		t.Fatalf("terminate calls = %v, want one delete_remote", adapter.terms)
	}

	var status string
	var externalID *string
	if err := f.pool.QueryRow(context.Background(),
		`SELECT status, external_id FROM services WHERE id = $1`, f.serviceID).
		Scan(&status, &externalID); err != nil {
		t.Fatalf("read service: %v", err)
	}
	if status != "active" {
		t.Fatalf("service status = %q, want active: deleting the remote resource is not a termination", status)
	}
	if externalID != nil {
		t.Fatalf("external id = %q, want it cleared", *externalID)
	}
}

// TestCancelSubscriptionCallsTheGatewayAndClearsTheId is SVC-14 / CT-12: the provider-side
// subscription is ended by the *gateway*, from a queue row, and the panel stops pointing at it.
func TestCancelSubscriptionCallsTheGatewayAndClearsTheId(t *testing.T) {
	f := newActionFixture(t, `{"plan_id":"small"}`, "srv-1")
	if _, err := f.pool.Exec(context.Background(),
		`UPDATE services SET external_subscription_id = 'sub_live_1' WHERE id = $1`,
		f.serviceID); err != nil {
		t.Fatalf("record subscription id: %v", err)
	}
	f.enqueue(t, "cancel_subscription", "cancel_subscription:1:sub_live_1",
		map[string]any{"subscription_id": "sub_live_1", "reason": "moved to a card"})

	canceller := &recordingCanceller{}
	worker := provisioning.NewWorker(f.pool, &recordingAdapter{})
	worker.Canceller = canceller
	if _, err := worker.RunOnce(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(canceller.requests) != 1 || canceller.requests[0].SubscriptionID != "sub_live_1" {
		t.Fatalf("gateway calls = %+v, want one cancellation of sub_live_1", canceller.requests)
	}

	var stored string
	if err := f.pool.QueryRow(context.Background(),
		`SELECT external_subscription_id FROM services WHERE id = $1`, f.serviceID).Scan(&stored); err != nil {
		t.Fatalf("read service: %v", err)
	}
	if stored != "" {
		t.Fatalf("external subscription id = %q, want it cleared after a cancellation", stored)
	}
	actions := f.auditActions(t)
	found := false
	for _, a := range actions {
		if a == "service.subscription_cancelled" {
			found = true
		}
	}
	if !found {
		t.Fatalf("audit actions = %v, want service.subscription_cancelled", actions)
	}
}

// A cancellation with no id to cancel is parked: there is nothing to call, and an operator has to
// record which subscription this service is billed by.
func TestCancelSubscriptionWithoutAnIdParksForReview(t *testing.T) {
	f := newActionFixture(t, `{"plan_id":"small"}`, "srv-1")
	f.enqueue(t, "cancel_subscription", "cancel_subscription:1:none", map[string]any{})

	canceller := &recordingCanceller{}
	worker := provisioning.NewWorker(f.pool, &recordingAdapter{})
	worker.Canceller = canceller
	res, err := worker.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Outcome != "needs_review" {
		t.Fatalf("outcome = %q, want needs_review", res.Outcome)
	}
	if len(canceller.requests) != 0 {
		t.Fatal("the gateway was called with no subscription id")
	}
}

// TestCreateCarriesTheSelectionAndTheExtensionFields is UPG-10 + UPG-12 together: the adapter
// receives the config options as environment entries and the extension's own declared inputs
// under their own keys.
func TestCreateCarriesTheSelectionAndTheExtensionFields(t *testing.T) {
	f := newActionFixture(t,
		`{"plan_id":"small","fields":{"pterodactyl_user_id":"42","domain":"example.com"}}`, "")
	f.addOption(t, "Max players", "MAX_PLAYERS", "40")
	f.enqueue(t, "create", "create:1", nil)

	adapter := &recordingAdapter{}
	worker := provisioning.NewWorker(f.pool, adapter)
	if _, err := worker.RunOnce(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(adapter.creates) != 1 {
		t.Fatalf("%d create calls, want 1", len(adapter.creates))
	}
	meta := adapter.creates[0].Metadata
	if meta["env.MAX_PLAYERS"] != "40" {
		t.Fatalf("metadata = %v, want env.MAX_PLAYERS=40", meta)
	}
	if meta["pterodactyl_user_id"] != "42" {
		t.Fatalf("metadata = %v, want the extension's declared field", meta)
	}
	if meta["domain"] != "example.com" {
		t.Fatalf("metadata = %v, want the extension's declared domain", meta)
	}
	// The bookkeeping keys are not overwritten by an extension field.
	if meta["service_id"] != f.serviceID {
		t.Fatalf("service_id = %q, want %q", meta["service_id"], f.serviceID)
	}
}

// The gateway is not the provisioner: a cancellation must not be dispatched to the adapter that
// creates servers, even when that is the only integration configured.
func TestSubscriptionCancellationNeverCallsTheProvisioner(t *testing.T) {
	f := newActionFixture(t, `{"plan_id":"small"}`, "srv-1")
	f.enqueue(t, "cancel_subscription", "cancel_subscription:1:sub_x",
		map[string]any{"subscription_id": "sub_x"})

	adapter := &recordingAdapter{}
	worker := provisioning.NewWorker(f.pool, adapter)
	canceller := &recordingCanceller{}
	worker.Canceller = canceller
	if _, err := worker.RunOnce(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(adapter.terms) != 0 || len(adapter.upgrades) != 0 {
		t.Fatal("a subscription cancellation reached the provisioner")
	}
	if len(canceller.requests) != 1 {
		t.Fatalf("%d gateway calls, want 1", len(canceller.requests))
	}
}

// A gateway that does not know the id is not a success: the panel's record refers to nothing and
// an operator must see that rather than a green queue row.
func TestCancellationOfAnUnknownSubscriptionParksForReview(t *testing.T) {
	f := newActionFixture(t, `{"plan_id":"small"}`, "srv-1")
	f.enqueue(t, "cancel_subscription", "cancel_subscription:1:sub_gone",
		map[string]any{"subscription_id": "sub_gone"})

	worker := provisioning.NewWorker(f.pool, &recordingAdapter{})
	worker.Canceller = goneCanceller{}
	res, err := worker.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Outcome != "needs_review" {
		t.Fatalf("outcome = %q, want needs_review", res.Outcome)
	}
}

// goneCanceller is a gateway that has already forgotten the subscription.
type goneCanceller struct{}

func (goneCanceller) CancelSubscription(_ context.Context, req provisioning.CancelSubscriptionRequest) (extproto.GatewayCancelSubscriptionResult, error) {
	return extproto.GatewayCancelSubscriptionResult{
		SubscriptionID: req.SubscriptionID, Status: "canceled", AlreadyGone: true,
	}, nil
}

// The reader that powers the purchase form reports "no fields" rather than a failure when the
// adapter does not implement the method, and a failure when nothing is configured: an operator
// needs to know which of those two it is.
func TestReadProvisionerFieldsWithoutConfigurationFails(t *testing.T) {
	pool := storetest.DB(t)
	_, err := provisioning.ReadProvisionerFields(context.Background(), pool, time.Second)
	if !errors.Is(err, provisioning.ErrNotConfigured) {
		t.Fatalf("err = %v, want ErrNotConfigured", err)
	}
}

// encodeJSON is a tiny helper so a test can pass a payload as a Go map.
func encodeJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return string(raw)
}
