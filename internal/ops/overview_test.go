package ops_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/catalyst/billing/internal/billing"
	"github.com/catalyst/billing/internal/money"
	"github.com/catalyst/billing/internal/ops"
	"github.com/catalyst/billing/internal/store"
	"github.com/catalyst/billing/internal/store/storetest"
	"github.com/jackc/pgx/v5"
)

var usd = money.MustLookupCurrency("USD")

type fixture struct {
	pool       *store.Pool
	customerID string
	orderID    string
	productID  string
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	pool := storetest.DB(t)
	ctx := context.Background()
	var f fixture
	f.pool = pool
	err := pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx,
			`INSERT INTO customers (email, name) VALUES ('ops@example.test', 'Ops') RETURNING id`,
		).Scan(&f.customerID); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx,
			`INSERT INTO products (slug, name) VALUES ('vps', 'VPS') RETURNING id`).Scan(&f.productID); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `
			INSERT INTO orders (customer_id, status, currency, total_minor)
			VALUES ($1, 'paid', 'USD', 1200) RETURNING id`, f.customerID).Scan(&f.orderID)
	})
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	return f
}

// paidInvoice issues and settles an invoice, returning its id.
func (f fixture) paidInvoice(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	issuer := billing.NewIssuer(billing.FixedTaxPricer{
		Rate: money.MustParseRatio("0%"), Rounding: money.RoundHalfUp, Policy: "none",
	})
	var invoiceID string
	err := f.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		id, _, _, err := issuer.CreateDraft(ctx, tx, billing.IssueRequest{
			CustomerID: f.customerID,
			OrderID:    f.orderID,
			Lines: []billing.Line{{
				Description: "VPS", QuantityMilli: 1000, UnitPrice: money.New(1200, usd),
			}},
			Customer: billing.CustomerDetails{ID: f.customerID},
		})
		if err != nil {
			return err
		}
		issued, err := issuer.Issue(ctx, tx, id, billing.DefaultSeries)
		if err != nil {
			return err
		}
		invoiceID = issued.InvoiceID
		_, err = billing.ApplyPayment(ctx, tx, billing.AllocationRequest{
			Provider:          "stripe",
			ProviderAccount:   "acct",
			ProviderPaymentID: "pi_" + issued.Number,
			InvoiceID:         issued.InvoiceID,
			Amount:            money.New(1200, usd),
		})
		return err
	})
	if err != nil {
		t.Fatalf("paid invoice: %v", err)
	}
	return invoiceID
}

// codes returns the exception codes present, for order-independent assertions.
func codes(o ops.Overview) map[string]ops.Exception {
	out := make(map[string]ops.Exception, len(o.Exceptions))
	for _, e := range o.Exceptions {
		if e.Code != "" {
			out[e.Code] = e
		}
	}
	return out
}

// A healthy system reports no exceptions at all: an empty overview is the
// signal that nothing needs attention.
func TestHealthySystemHasNoExceptions(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	// Give the paid order a running service so nothing is actually broken.
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO services (customer_id, product_id, status, generation)
		VALUES ($1, $2, 'active', 1)`, f.customerID, f.productID); err != nil {
		t.Fatal(err)
	}
	f.paidInvoice(t)

	overview := ops.New(f.pool).Overview(ctx)
	if len(codes(overview)) != 0 {
		t.Fatalf("expected no exceptions, got %+v", overview.Exceptions)
	}
	if overview.Counts.Customers != 1 {
		t.Fatalf("customers = %d, want 1", overview.Counts.Customers)
	}
	if overview.Counts.Services != 1 {
		t.Fatalf("services = %d, want 1", overview.Counts.Services)
	}
}

// Paid but unprovisioned is the worst customer-facing state and must be critical.
func TestPaidNotProvisionedIsCritical(t *testing.T) {
	f := newFixture(t)
	f.paidInvoice(t)

	overview := ops.New(f.pool).Overview(context.Background())
	exc, ok := codes(overview)["paid_not_provisioned"]
	if !ok {
		t.Fatalf("paid_not_provisioned missing: %+v", overview.Exceptions)
	}
	if exc.Severity != ops.SeverityCritical {
		t.Fatalf("severity = %s, want critical", exc.Severity)
	}
	if exc.Count != 1 {
		t.Fatalf("count = %d, want 1", exc.Count)
	}
	if exc.Action == "" {
		t.Fatal("exception has no recovery action")
	}
}

// A terminated service does not satisfy a paid order: the customer has nothing.
func TestTerminatedServiceStillCountsAsUnprovisioned(t *testing.T) {
	f := newFixture(t)
	f.paidInvoice(t)
	if _, err := f.pool.Exec(context.Background(), `
		INSERT INTO services (customer_id, product_id, status, generation)
		VALUES ($1, $2, 'terminated', 1)`, f.customerID, f.productID); err != nil {
		t.Fatal(err)
	}

	overview := ops.New(f.pool).Overview(context.Background())
	if _, ok := codes(overview)["paid_not_provisioned"]; !ok {
		t.Fatalf("a terminated service must not count as delivered: %+v", overview.Exceptions)
	}
}

func TestUnknownOperationsSurfaced(t *testing.T) {
	f := newFixture(t)
	var serviceID string
	ctx := context.Background()
	if err := f.pool.QueryRow(ctx, `
		INSERT INTO services (customer_id, product_id, status, generation)
		VALUES ($1, $2, 'provisioning', 1) RETURNING id`, f.customerID, f.productID).Scan(&serviceID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO service_operations (service_id, kind, operation_key, generation, status)
		VALUES ($1, 'create', 'create:x', 1, 'unknown')`, serviceID); err != nil {
		t.Fatal(err)
	}

	exc, ok := codes(ops.New(f.pool).Overview(ctx))["unknown_operations"]
	if !ok {
		t.Fatal("unknown_operations missing")
	}
	if exc.Count != 1 || exc.Severity != ops.SeverityCritical {
		t.Fatalf("exception = %+v, want count 1 and critical", exc)
	}
}

func TestOverdueInvoiceTotalsMoney(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	issuer := billing.NewIssuer(billing.FixedTaxPricer{
		Rate: money.MustParseRatio("0%"), Rounding: money.RoundHalfUp, Policy: "none",
	})
	err := f.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		id, _, _, err := issuer.CreateDraft(ctx, tx, billing.IssueRequest{
			CustomerID: f.customerID,
			OrderID:    f.orderID,
			Lines: []billing.Line{{
				Description: "VPS", QuantityMilli: 1000, UnitPrice: money.New(1200, usd),
			}},
			Customer: billing.CustomerDetails{ID: f.customerID},
			DueAt:    time.Now().AddDate(0, 0, -5),
		})
		if err != nil {
			return err
		}
		_, err = issuer.Issue(ctx, tx, id, billing.DefaultSeries)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	exc, ok := codes(ops.New(f.pool).Overview(ctx))["overdue_invoices"]
	if !ok {
		t.Fatal("overdue_invoices missing")
	}
	if exc.Money == nil || *exc.Money != "12.00 USD" {
		t.Fatalf("money = %v, want 12.00 USD", exc.Money)
	}
	if exc.OldestAt == nil {
		t.Fatal("oldest_at not set: an operator cannot tell a new blip from a leak")
	}
}

func TestStalledWebhooksDetected(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	if _, err := f.pool.Exec(ctx, `
		INSERT INTO gateway_events (provider, provider_account, event_id, event_type, payload, signature_valid, received_at)
		VALUES ('stripe', 'acct', 'evt_old', 'payment.succeeded', '{}'::jsonb, true, now() - interval '1 hour')`); err != nil {
		t.Fatal(err)
	}

	exc, ok := codes(ops.New(f.pool).Overview(ctx))["stalled_webhooks"]
	if !ok {
		t.Fatal("stalled_webhooks missing")
	}
	if exc.Count != 1 {
		t.Fatalf("count = %d, want 1", exc.Count)
	}
}

// A recent unprocessed event is normal and must not raise an alarm.
func TestRecentWebhookIsNotStalled(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO gateway_events (provider, provider_account, event_id, event_type, payload, signature_valid)
		VALUES ('stripe', 'acct', 'evt_new', 'payment.succeeded', '{}'::jsonb, true)`); err != nil {
		t.Fatal(err)
	}
	if _, ok := codes(ops.New(f.pool).Overview(ctx))["stalled_webhooks"]; ok {
		t.Fatal("a fresh event must not be reported as stalled")
	}
}

// Every exception must carry a code, a severity and an action: an alert an
// operator cannot act on is noise.
func TestEveryExceptionIsActionable(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.paidInvoice(t)

	if _, err := f.pool.Exec(ctx, `
		INSERT INTO gateway_events (provider, provider_account, event_id, event_type, payload, signature_valid, received_at)
		VALUES ('stripe', 'acct', 'evt_old', 'payment.succeeded', '{}'::jsonb, true, now() - interval '1 hour')`); err != nil {
		t.Fatal(err)
	}

	overview := ops.New(f.pool).Overview(ctx)
	if len(overview.Exceptions) == 0 {
		t.Fatal("expected exceptions")
	}
	seen := make(map[string]bool)
	for _, e := range overview.Exceptions {
		if e.Code == "" {
			t.Fatalf("exception without a code: %+v", e)
		}
		if seen[e.Code] {
			t.Fatalf("duplicate exception code %q", e.Code)
		}
		seen[e.Code] = true
		switch e.Severity {
		case ops.SeverityCritical, ops.SeverityWarning, ops.SeverityInfo:
		default:
			t.Fatalf("%s has severity %q", e.Code, e.Severity)
		}
		if e.Action == "" {
			t.Fatalf("%s has no action", e.Code)
		}
		if e.Title == "" || e.Detail == "" {
			t.Fatalf("%s is missing title or detail", e.Code)
		}
	}
}

// A check that cannot run must report itself, not disappear: silence would look
// like health.
func TestBrokenCheckReportsItself(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	// Break the schema out from under the check.
	if _, err := f.pool.Exec(ctx, `ALTER TABLE service_operations RENAME TO service_operations_moved`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = f.pool.Exec(context.Background(),
			`ALTER TABLE service_operations_moved RENAME TO service_operations`)
	})

	overview := ops.New(f.pool).Overview(ctx)
	exc, ok := codes(overview)["unknown_operations"]
	if !ok {
		t.Fatal("a failing check must still appear in the overview")
	}
	if exc.Count != 0 {
		t.Fatalf("count = %d, want 0 for a failed check", exc.Count)
	}
	if want := "could not run"; !contains(exc.Detail, want) {
		t.Fatalf("detail = %q, want it to mention %q", exc.Detail, want)
	}
}

// The overview must not be slow: it is the first page an operator opens.
func TestOverviewIsFast(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.paidInvoice(t)

	start := time.Now()
	overview := ops.New(f.pool).Overview(ctx)
	elapsed := time.Since(start)
	if elapsed > 500*time.Millisecond {
		t.Fatalf("overview took %s on a tiny dataset", elapsed)
	}
	if overview.GeneratedAt.IsZero() {
		t.Fatal("generated_at not set")
	}
	_ = fmt.Sprint(overview)
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (func() bool {
		for i := 0; i+len(needle) <= len(haystack); i++ {
			if haystack[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	})()
}
