package portal_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/catalyst/billing/internal/authn"
	"github.com/catalyst/billing/internal/billing"
	"github.com/catalyst/billing/internal/money"
	"github.com/catalyst/billing/internal/portal"
	"github.com/catalyst/billing/internal/store"
	"github.com/catalyst/billing/internal/store/storetest"
	"github.com/jackc/pgx/v5"
)

var usd = money.MustLookupCurrency("USD")

// tenant is one customer with a login, a service and an invoice.
type tenant struct {
	userID       string
	customerID   string
	serviceID    string
	subscription string
	invoiceID    string
}

type world struct {
	pool    *store.Pool
	manager *portal.Manager
	alice   tenant
	bob     tenant
}

func newWorld(t *testing.T) *world {
	t.Helper()
	pool := storetest.DB(t)
	w := &world{pool: pool, manager: portal.New(pool)}
	w.manager.Now = func() time.Time { return time.Date(2026, 3, 15, 12, 0, 0, 0, time.UTC) }

	w.alice = w.createTenant(t, "alice@example.test", "Alice", "12.00", time.Date(2026, 3, 20, 0, 0, 0, 0, time.UTC))
	w.bob = w.createTenant(t, "bob@example.test", "Bob", "25.00", time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC))
	return w
}

// createTenant builds a complete customer: user, customer, subscription, service
// and one issued invoice.
func (w *world) createTenant(t *testing.T, email, name, amount string, dueAt time.Time) tenant {
	t.Helper()
	ctx := context.Background()
	var out tenant

	hasher, err := authn.NewHasher(fastParams())
	if err != nil {
		t.Fatal(err)
	}
	hash, err := hasher.Hash("a long enough password")
	if err != nil {
		t.Fatal(err)
	}

	err = w.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `
			INSERT INTO users (email, password_hash, display_name, email_verified_at)
			VALUES ($1, $2, $3, now()) RETURNING id`, email, hash, name).Scan(&out.userID); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx,
			`INSERT INTO customers (email, name) VALUES ($1, $2) RETURNING id`, email, name).
			Scan(&out.customerID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO customer_accounts (user_id, customer_id, role)
			VALUES ($1, $2, 'owner')`, out.userID, out.customerID); err != nil {
			return err
		}

		var productID, priceID string
		slug := "vps-" + out.customerID[:8]
		if err := tx.QueryRow(ctx,
			`INSERT INTO products (slug, name) VALUES ($1, 'VPS Small') RETURNING id`, slug).
			Scan(&productID); err != nil {
			return err
		}
		price := money.MustParse(amount, usd)
		if err := tx.QueryRow(ctx, `
			INSERT INTO price_versions (product_id, version, currency, amount_minor, interval, anchor_day, anchor_timezone)
			VALUES ($1, 1, 'USD', $2, 'monthly', 1, 'UTC') RETURNING id`, productID, price.Minor()).
			Scan(&priceID); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `
			INSERT INTO subscriptions (customer_id, price_version_id, status, interval, anchor_day,
				timezone, current_period_start, current_period_end, next_billing_at)
			VALUES ($1, $2, 'active', 'monthly', 1, 'UTC', $3, $4, $4)
			RETURNING id`,
			out.customerID, priceID,
			time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC),
			time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)).Scan(&out.subscription); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `
			INSERT INTO services (customer_id, subscription_id, product_id, status, generation, external_id)
			VALUES ($1, $2, $3, 'active', 1, 'srv-1') RETURNING id`,
			out.customerID, out.subscription, productID).Scan(&out.serviceID); err != nil {
			return err
		}

		issuer := billing.NewIssuer(billing.FixedTaxPricer{
			Rate: money.MustParseRatio("0%"), Rounding: money.RoundHalfUp, Policy: "none",
		})
		id, _, _, err := issuer.CreateDraft(ctx, tx, billing.IssueRequest{
			CustomerID:     out.customerID,
			SubscriptionID: out.subscription,
			PeriodKey:      "2026-03-01/2026-04-01",
			Lines: []billing.Line{{
				Description: "VPS Small monthly", QuantityMilli: 1000, UnitPrice: price,
			}},
			Customer: billing.CustomerDetails{ID: out.customerID},
			DueAt:    dueAt,
		})
		if err != nil {
			return err
		}
		issued, err := issuer.Issue(ctx, tx, id, billing.DefaultSeries)
		if err != nil {
			return err
		}
		out.invoiceID = issued.InvoiceID
		return nil
	})
	if err != nil {
		t.Fatalf("create tenant %s: %v", email, err)
	}
	return out
}

func fastParams() authn.Argon2Params {
	p := authn.DefaultArgon2Params()
	p.Memory = 19 * 1024
	p.Iterations = 1
	return p
}

// ------------------------------------------------------------------- tests --

// Acceptance scenario 10: customer A cannot reach customer B's records by
// substituting identifiers.
func TestCrossTenantAccessDenied(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()

	// Alice can read her own invoice and service.
	if _, err := w.manager.Invoice(ctx, w.alice.userID, w.alice.invoiceID); err != nil {
		t.Fatalf("alice reading her own invoice: %v", err)
	}
	if _, err := w.manager.Service(ctx, w.alice.userID, w.alice.serviceID); err != nil {
		t.Fatalf("alice reading her own service: %v", err)
	}

	// Alice must not read Bob's, and the error must be the same "not found" as for a
	// nonexistent id, so the response cannot be used to probe for identifiers.
	_, err := w.manager.Invoice(ctx, w.alice.userID, w.bob.invoiceID)
	if !errors.Is(err, portal.ErrNotFound) {
		t.Fatalf("alice reading bob's invoice: got %v, want ErrNotFound", err)
	}
	_, err = w.manager.Service(ctx, w.alice.userID, w.bob.serviceID)
	if !errors.Is(err, portal.ErrNotFound) {
		t.Fatalf("alice reading bob's service: got %v, want ErrNotFound", err)
	}

	// A made-up id gives exactly the same answer.
	_, err = w.manager.Invoice(ctx, w.alice.userID, "00000000-0000-0000-0000-000000000000")
	if !errors.Is(err, portal.ErrNotFound) {
		t.Fatalf("nonexistent invoice: got %v, want ErrNotFound", err)
	}

	// Listing returns only Alice's records.
	invoices, err := w.manager.Invoices(ctx, w.alice.userID)
	if err != nil {
		t.Fatal(err)
	}
	if len(invoices) != 1 || invoices[0].ID != w.alice.invoiceID {
		t.Fatalf("alice sees %d invoices, want only her own", len(invoices))
	}
	services, err := w.manager.Services(ctx, w.alice.userID)
	if err != nil {
		t.Fatal(err)
	}
	if len(services) != 1 || services[0].ID != w.alice.serviceID {
		t.Fatalf("alice sees %d services, want only her own", len(services))
	}

	// The overview balance is hers alone: Bob owes 25.00, Alice owes 12.00.
	overview, err := w.manager.Overview(ctx, w.alice.userID)
	if err != nil {
		t.Fatal(err)
	}
	if len(overview.Balance.Outstanding) != 1 {
		t.Fatalf("outstanding = %v, want one currency line", overview.Balance.Outstanding)
	}
	if got := overview.Balance.Outstanding[0].Format(); got != "12.00 USD" {
		t.Fatalf("alice's balance = %s, want 12.00 USD (not bob's)", got)
	}
	if overview.Customer.Email != "alice@example.test" {
		t.Fatalf("overview customer = %q", overview.Customer.Email)
	}
}

// Cancelling must also be tenant-scoped: Alice cannot cancel Bob's service.
func TestCrossTenantCancellationDenied(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()

	_, err := w.manager.RequestCancellation(ctx, w.alice.userID, w.bob.serviceID)
	if !errors.Is(err, portal.ErrNotFound) {
		t.Fatalf("got %v, want ErrNotFound", err)
	}
	// Bob's subscription is untouched.
	var cancelAtEnd bool
	if err := w.pool.QueryRow(ctx,
		`SELECT cancel_at_period_end FROM subscriptions WHERE id = $1`, w.bob.subscription).
		Scan(&cancelAtEnd); err != nil {
		t.Fatal(err)
	}
	if cancelAtEnd {
		t.Fatal("bob's subscription was cancelled by alice")
	}
}

// The overview answers the three questions the plan says it must.
func TestOverviewAnswersCustomerQuestions(t *testing.T) {
	w := newWorld(t)
	overview, err := w.manager.Overview(context.Background(), w.alice.userID)
	if err != nil {
		t.Fatal(err)
	}

	// What do I owe?
	if len(overview.Balance.Outstanding) != 1 || overview.Balance.Outstanding[0].Format() != "12.00 USD" {
		t.Fatalf("outstanding = %v", overview.Balance.Outstanding)
	}
	// What is renewing?
	if len(overview.Upcoming) != 1 || overview.Upcoming[0].NextBill == nil {
		t.Fatalf("upcoming = %+v", overview.Upcoming)
	}
	// Compare instants, not formatted strings: the driver returns timestamptz in
	// the session's location, which is the same moment with a different offset.
	if want := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC); !overview.Upcoming[0].NextBill.Equal(want) {
		t.Fatalf("next bill = %s, want %s", overview.Upcoming[0].NextBill, want)
	}
	// Is my service ready?
	if overview.Upcoming[0].Status != "active" {
		t.Fatalf("service status = %s", overview.Upcoming[0].Status)
	}
	// And what needs paying?
	if len(overview.OpenInvoices) != 1 {
		t.Fatalf("open invoices = %d, want 1", len(overview.OpenInvoices))
	}
	if !overview.OpenInvoices[0].Payable {
		t.Fatal("the open invoice is not payable")
	}
}

// Overdue is derived from the due date, not from a job having run.
func TestOverdueIsDerived(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()

	// Bob's invoice was due 1 March; "now" is 15 March.
	bob, err := w.manager.Invoice(ctx, w.bob.userID, w.bob.invoiceID)
	if err != nil {
		t.Fatal(err)
	}
	if !bob.Overdue {
		t.Fatal("an invoice past its due date is not marked overdue")
	}

	// Alice's is due 20 March, so it is not.
	alice, err := w.manager.Invoice(ctx, w.alice.userID, w.alice.invoiceID)
	if err != nil {
		t.Fatal(err)
	}
	if alice.Overdue {
		t.Fatal("an invoice before its due date is marked overdue")
	}

	// Moving the clock past the due date changes the answer with no writes.
	w.manager.Now = func() time.Time { return time.Date(2026, 3, 21, 0, 0, 0, 0, time.UTC) }
	alice, err = w.manager.Invoice(ctx, w.alice.userID, w.alice.invoiceID)
	if err != nil {
		t.Fatal(err)
	}
	if !alice.Overdue {
		t.Fatal("overdue is not derived from the current time")
	}
}

// A paid invoice is not payable and contributes nothing to the balance.
func TestPaidInvoiceIsSettled(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()

	err := w.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := billing.ApplyPayment(ctx, tx, billing.AllocationRequest{
			Provider:          "stripe",
			ProviderAccount:   "acct",
			ProviderPaymentID: "pi_alice",
			InvoiceID:         w.alice.invoiceID,
			Amount:            money.MustParse("12.00", usd),
		})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	inv, err := w.manager.Invoice(ctx, w.alice.userID, w.alice.invoiceID)
	if err != nil {
		t.Fatal(err)
	}
	if inv.Payable {
		t.Fatal("a paid invoice is still payable")
	}
	if !inv.Due.IsZero() {
		t.Fatalf("due = %s, want zero", inv.Due)
	}

	balance, err := w.manager.Balance(ctx, w.alice.userID)
	if err != nil {
		t.Fatal(err)
	}
	if len(balance.Outstanding) != 0 {
		t.Fatalf("outstanding = %v, want none", balance.Outstanding)
	}
}

// Cancelling schedules the end of the paid period; it does not take the service
// away immediately.
func TestCancellationIsAtPeriodEnd(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()

	endsAt, err := w.manager.RequestCancellation(ctx, w.alice.userID, w.alice.serviceID)
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if want := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC); !endsAt.Equal(want) {
		t.Fatalf("ends at %s, want %s", endsAt, want)
	}

	// The service is still active: the customer paid for this period.
	svc, err := w.manager.Service(ctx, w.alice.userID, w.alice.serviceID)
	if err != nil {
		t.Fatal(err)
	}
	if svc.Status != "active" {
		t.Fatalf("service status = %s, want active until the period ends", svc.Status)
	}
	// And the cancel action is no longer offered.
	for _, a := range svc.Actions {
		if a == "cancel" {
			t.Fatal("a cancelled service still offers the cancel action")
		}
	}

	// Cancelling again is idempotent, not an error: a double click is normal.
	again, err := w.manager.RequestCancellation(ctx, w.alice.userID, w.alice.serviceID)
	if err != nil {
		t.Fatalf("second cancel: %v", err)
	}
	if !again.Equal(endsAt) {
		t.Fatalf("second cancel returned %s, want %s", again, endsAt)
	}
}

// A user with no customer record gets ErrNotFound rather than a broken page.
func TestStaffWithoutCustomerGetsNotFound(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()

	var staffID string
	hash, err := authn.NewHasher(fastParams())
	if err != nil {
		t.Fatal(err)
	}
	h, err := hash.Hash("a long enough password")
	if err != nil {
		t.Fatal(err)
	}
	if err := w.pool.QueryRow(ctx, `
		INSERT INTO users (email, password_hash) VALUES ('staff@example.test', $1) RETURNING id`, h).
		Scan(&staffID); err != nil {
		t.Fatal(err)
	}

	if _, err := w.manager.Overview(ctx, staffID); !errors.Is(err, portal.ErrNotFound) {
		t.Fatalf("got %v, want ErrNotFound", err)
	}
}

// The JSON contract must use empty arrays so a client can iterate without checks.
func TestEmptyCollectionsAreNotNil(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()

	// A customer with nothing at all.
	var userID, customerID string
	hash, _ := authn.NewHasher(fastParams())
	h, _ := hash.Hash("a long enough password")
	if err := w.pool.QueryRow(ctx, `
		INSERT INTO users (email, password_hash) VALUES ('empty@example.test', $1) RETURNING id`, h).
		Scan(&userID); err != nil {
		t.Fatal(err)
	}
	if err := w.pool.QueryRow(ctx,
		`INSERT INTO customers (email, name) VALUES ('empty@example.test', 'Empty') RETURNING id`).
		Scan(&customerID); err != nil {
		t.Fatal(err)
	}
	if _, err := w.pool.Exec(ctx, `
		INSERT INTO customer_accounts (user_id, customer_id, role) VALUES ($1, $2, 'owner')`,
		userID, customerID); err != nil {
		t.Fatal(err)
	}

	overview, err := w.manager.Overview(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	if overview.Upcoming == nil || overview.RecentInvoices == nil || overview.OpenInvoices == nil {
		t.Fatalf("overview contains nil slices: %+v", overview)
	}
	if overview.Balance.Outstanding == nil || overview.Balance.Credit == nil {
		t.Fatalf("balance contains nil slices: %+v", overview.Balance)
	}
}
