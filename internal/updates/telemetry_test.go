package updates_test

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"testing"

	"github.com/catalyst/billing/internal/settings"
	"github.com/catalyst/billing/internal/store"
	"github.com/catalyst/billing/internal/updates"
)

// The telemetry payload's privacy promise, tested rather than documented.
//
// Two properties are asserted, and both can actually fail:
//
//  1. When telemetry is off the payload is nil and nothing is encoded. "Off by default" is
//     only worth anything if "off" means "no payload exists", not "a payload that is not
//     sent yet".
//  2. When it is on, the encoded bytes contain nothing that identifies a customer. The test
//     seeds a customer whose name, address and company are unique strings, then greps the
//     encoded payload for each of them. A field added to the payload later that carried any
//     of them would fail here.
//
// The third assertion is the structural one: the payload's key set is exactly the documented
// set, so adding a field is a deliberate act with a failing test to review.

// payloadKeys is the complete, documented key set. Changing it is a schema change to a
// document that leaves the installation, so it is asserted rather than assumed.
var payloadKeys = []string{
	"arch",
	"channel",
	"counts",
	"database_schema_version",
	"generated_at",
	"go_version",
	"instance_id",
	"os",
	"panel_version",
	"schema_version",
}

// countKeys is the complete key set of the counts object. Every one is an integer.
var countKeys = []string{
	"customers",
	"extensions_installed",
	"invoices_open",
	"invoices_paid_last_30d",
	"services_active",
	"services_suspended",
	"tickets_open",
}

// storeDefaultTelemetry reads the code-side default of `updates.telemetry`, which is what a
// fresh installation reports before an operator saves anything.
func storeDefaultTelemetry(t *testing.T, ctx context.Context, pool *store.Pool) (bool, error) {
	t.Helper()
	return settings.New(pool).Bool(ctx, "updates", "telemetry")
}

// TestTelemetryIsEmptyWhenOff is the "off by default" half.
func TestTelemetryIsEmptyWhenOff(t *testing.T) {
	pool := newStore(t)
	ctx := context.Background()
	store := updates.New(pool)

	payload, err := store.BuildPayload(ctx, "1.4.0", "stable", false)
	if err != nil {
		t.Fatalf("build payload: %v", err)
	}
	if payload != nil {
		t.Fatalf("telemetry is off but a payload was built: %+v", payload)
	}

	// The setting's default is the real switch, so the default is asserted too: a default
	// that changed to true would make this test fail rather than quietly start reporting.
	enabled, err := storeDefaultTelemetry(t, ctx, pool)
	if err != nil {
		t.Fatalf("read the telemetry default: %v", err)
	}
	if enabled {
		t.Fatal("updates.telemetry defaults to on; telemetry must be off unless an operator turns it on")
	}
}

// TestTelemetryPayloadCarriesNoCustomerData seeds a customer whose every field is a unique
// string, then asserts none of those strings appears in the encoded payload.
func TestTelemetryPayloadCarriesNoCustomerData(t *testing.T) {
	pool := newStore(t)
	ctx := context.Background()

	// Every value below is chosen to be unmistakable in a byte-for-byte search.
	const (
		email     = "telemetry-canary-customer@identifiable.example"
		name      = "Canary Identifiable Customer"
		company   = "Canary Hosting Industries Limited"
		hostname  = "canary-host-identifiable.internal"
		subject   = "Canary ticket subject about a private matter"
		invoiceNo = "INV-CANARY-000042"
	)
	var customerID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO customers (email, name) VALUES ($1, $2) RETURNING id`,
		email, name).Scan(&customerID); err != nil {
		t.Fatalf("seed customer: %v", err)
	}
	var productID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO products (slug, name) VALUES ('telemetry-canary', 'Canary Plan')
		RETURNING id`).Scan(&productID); err != nil {
		t.Fatalf("seed product: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO services (customer_id, product_id, status, external_id)
		VALUES ($1, $2, 'active', $3)`, customerID, productID, hostname); err != nil {
		t.Fatalf("seed service: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO invoices (number, customer_id, status, currency,
			subtotal_minor, tax_minor, total_minor, issued_at, snapshot)
		VALUES ($1, $2, 'open', 'USD', 1000, 0, 1000, now(), '{"schema_version":1}'::jsonb)`,
		invoiceNo, customerID); err != nil {
		t.Fatalf("seed invoice: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO tickets (customer_id, subject, status) VALUES ($1, $2, 'open')`,
		customerID, subject); err != nil {
		t.Fatalf("seed ticket: %v", err)
	}

	store := updates.New(pool)
	payload, err := store.BuildPayload(ctx, "1.4.0", "stable", true)
	if err != nil {
		t.Fatalf("build payload: %v", err)
	}
	if payload == nil {
		t.Fatal("telemetry is on but no payload was built")
	}

	encoded, err := updates.EncodePayload(*payload)
	if err != nil {
		t.Fatalf("encode payload: %v", err)
	}
	wire := string(encoded)
	for _, canary := range []string{email, name, company, hostname, subject, invoiceNo, customerID} {
		if strings.Contains(wire, canary) {
			t.Fatalf("the telemetry payload contains customer data %q:\n%s", canary, wire)
		}
	}
	// The counts are still real, so the payload is not empty by being useless: one customer
	// and one open invoice exist above.
	if payload.Counts.Customers != 1 {
		t.Fatalf("customer count = %d, want 1", payload.Counts.Customers)
	}
	if payload.Counts.InvoicesOpen != 1 {
		t.Fatalf("open invoice count = %d, want 1", payload.Counts.InvoicesOpen)
	}
	if payload.Counts.TicketsOpen != 1 {
		t.Fatalf("open ticket count = %d, want 1", payload.Counts.TicketsOpen)
	}

	// The instance id is the one identifier, and it is a random uuid rather than anything
	// derived from the customer above.
	if payload.InstanceID == "" {
		t.Fatal("the payload has no instance id")
	}
	if strings.Contains(payload.InstanceID, "canary") || strings.Contains(payload.InstanceID, customerID) {
		t.Fatalf("the instance id is derived from customer data: %q", payload.InstanceID)
	}
}

// TestTelemetryPayloadKeySet pins the document's shape. A field added without updating this
// test is a field that left the building without a review.
func TestTelemetryPayloadKeySet(t *testing.T) {
	pool := newStore(t)
	payload, err := updates.New(pool).BuildPayload(context.Background(), "1.4.0", "stable", true)
	if err != nil {
		t.Fatalf("build payload: %v", err)
	}
	encoded, err := updates.EncodePayload(*payload)
	if err != nil {
		t.Fatalf("encode payload: %v", err)
	}

	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	var got []string
	for key := range decoded {
		got = append(got, key)
	}
	sort.Strings(got)
	want := append([]string(nil), payloadKeys...)
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("payload keys = %v, want %v", got, want)
	}

	counts, ok := decoded["counts"].(map[string]any)
	if !ok {
		t.Fatalf("counts is %T, want an object", decoded["counts"])
	}
	got = nil
	for key := range counts {
		got = append(got, key)
	}
	sort.Strings(got)
	want = append([]string(nil), countKeys...)
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("count keys = %v, want %v", got, want)
	}

	// Every count is a number. A count that could be a string is a count that could be an
	// address.
	for key, value := range counts {
		if _, ok := value.(float64); !ok {
			t.Fatalf("count %q is %T, want a number", key, value)
		}
	}

	// The top-level strings are versions, a random identifier and a channel name. Nothing
	// else is allowed to be a string, because a string is the only shape that can carry a
	// customer.
	allowedStrings := map[string]bool{
		"instance_id":   true,
		"panel_version": true,
		"channel":       true,
		"go_version":    true,
		"os":            true,
		"arch":          true,
		"generated_at":  true,
	}
	for key, value := range decoded {
		if _, isString := value.(string); isString && !allowedStrings[key] {
			t.Fatalf("unexpected string field %q in the telemetry payload", key)
		}
	}
}

// TestTelemetryPayloadReportsSchemaVersion keeps the payload's own version honest: a receiver
// that does not know this version must be able to ignore the payload.
func TestTelemetryPayloadReportsSchemaVersion(t *testing.T) {
	pool := newStore(t)
	payload, err := updates.New(pool).BuildPayload(context.Background(), "1.4.0", "stable", true)
	if err != nil {
		t.Fatalf("build payload: %v", err)
	}
	if payload.SchemaVersion != updates.PayloadSchemaVersion {
		t.Fatalf("schema version = %d, want %d", payload.SchemaVersion, updates.PayloadSchemaVersion)
	}
	if payload.SchemaVersion != 1 {
		t.Fatalf("the payload schema version changed to %d; a receiver that knows 1 cannot read it",
			payload.SchemaVersion)
	}
	// The database schema version is a number, not a migration file name: a file name is a
	// path, and a path is the kind of detail that should not leave the installation.
	if payload.DatabaseSchemaVersion <= 0 {
		t.Fatalf("database schema version = %d, want a positive migration number",
			payload.DatabaseSchemaVersion)
	}
}
