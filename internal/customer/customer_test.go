package customer_test

// Tests for the two write paths that had no route before: creating a customer, and creating a
// service for one.
//
// They run against a real PostgreSQL because every property worth asserting here is a property of
// the rows: the login exists with a credential nobody knows, the subscription the renewal engine
// scans is there with the right period, the stock counter moved, the audit row names the operator,
// and a comped service has no schedule behind it. A mock would let all of those pass while the
// database refused the insert.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/catalyst/billing/internal/authn"
	"github.com/catalyst/billing/internal/billing"
	"github.com/catalyst/billing/internal/customer"
	"github.com/catalyst/billing/internal/renewal"
	"github.com/catalyst/billing/internal/store"
	"github.com/catalyst/billing/internal/store/storetest"
	"github.com/jackc/pgx/v5"
)

// cheapHasher keeps the Argon2 cost down. The hash is not what is under test; that it is a real,
// verifiable-format hash nobody holds the plaintext for is.
func cheapHasher(t *testing.T) *authn.Hasher {
	t.Helper()
	p := authn.DefaultArgon2Params()
	p.Memory = 19 * 1024
	p.Iterations = 1
	h, err := authn.NewHasher(p)
	if err != nil {
		t.Fatalf("hasher: %v", err)
	}
	return h
}

type env struct {
	pool  *store.Pool
	store *customer.Store
	// operator is the staff account whose id lands in the audit row and on the service.
	operator string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	pool := storetest.DB(t)
	return &env{
		pool:     pool,
		store:    customer.New(pool).WithHasher(cheapHasher(t)),
		operator: seedUser(t, pool, "operator@example.test"),
	}
}

// seedUser inserts an account, because both `services.origin_actor` and the audit row reference
// one: a name that is not a user would make the trail a string rather than a link.
func seedUser(t *testing.T, pool *store.Pool, email string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(), `
		INSERT INTO users (email, password_hash, display_name, email_verified_at)
		VALUES ($1, 'x', 'Operator', now()) RETURNING id`, email).Scan(&id); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return id
}

// seedProduct creates a product and one recurring price version.
func seedProduct(t *testing.T, pool *store.Pool, opts productOptions) (productID, priceVersionID string) {
	t.Helper()
	ctx := context.Background()
	interval := opts.interval
	if interval == "" {
		interval = "monthly"
	}
	// The column defaults to 'disabled', which is what a product that sells one per order has.
	allowQuantity := opts.allowQuantity
	if allowQuantity == "" {
		allowQuantity = "disabled"
	}
	slug := "prod-" + strings.ToLower(strings.ReplaceAll(t.Name(), "/", "-"))
	if err := pool.QueryRow(ctx, `
		INSERT INTO products (slug, name, stock, per_user_limit, allow_quantity)
		VALUES ($1, $2, $3, $4, $5) RETURNING id`,
		slug, "Manual Test VPS", opts.stock, opts.perUserLimit, allowQuantity).
		Scan(&productID); err != nil {
		t.Fatalf("seed product: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO price_versions (product_id, version, currency, amount_minor, setup_minor,
			interval, anchor_day, anchor_timezone, provision_plan, provision_region)
		VALUES ($1, 1, 'USD', 5000, 0, $2, 1, 'UTC', 'plan-manual', 'eu-west')
		RETURNING id`, productID, interval).Scan(&priceVersionID); err != nil {
		t.Fatalf("seed price version: %v", err)
	}
	return productID, priceVersionID
}

// productOptions are the product rules a test cares about. The zero value means "unlimited, no
// per-user limit, any quantity" — the defaults a migration gives.
type productOptions struct {
	stock         *int
	perUserLimit  *int
	allowQuantity string
	interval      string
}

func intPtr(v int) *int { return &v }

// ---------------------------------------------------------------- customers --

// A staff-created customer is a login with no usable password. The account exists, the row is
// linked, and no credential comes back.
func TestCreateCustomerWritesALoginWithNoUsablePassword(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	var record customer.Record
	if err := e.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		record, err = e.store.Create(ctx, tx, customer.CreateInput{
			Email:   "New.Customer@Example.test",
			Name:    "New Customer",
			ActorID: e.operator,
		})
		return err
	}); err != nil {
		t.Fatalf("create: %v", err)
	}

	// The address is stored lower-cased, so the case-insensitive unique index is the one that
	// matches and a later lookup by the typed form still finds the record.
	if record.Email != "new.customer@example.test" {
		t.Fatalf("email = %q, want the lower-cased form", record.Email)
	}

	var (
		hash     string
		linked   bool
		verified bool
	)
	if err := e.pool.QueryRow(ctx, `
		SELECT u.password_hash, u.email_verified_at IS NOT NULL,
		       EXISTS (SELECT 1 FROM customer_accounts ca
		                WHERE ca.user_id = u.id AND ca.customer_id = $2)
		FROM users u WHERE u.id = $1`, record.UserID, record.CustomerID).
		Scan(&hash, &verified, &linked); err != nil {
		t.Fatalf("read login: %v", err)
	}
	if !linked {
		t.Fatal("the login is not linked to the billing account, so the customer could never reach it")
	}
	if !verified {
		t.Fatal("a staff-created address should be stamped verified: otherwise the customer can set a password and still not sign in")
	}

	// The stored value is a real Argon2id hash of randomness nobody kept, so no guess verifies.
	hasher := cheapHasher(t)
	for _, guess := range []string{"", "password1234", "New Customer", record.UserID} {
		if _, err := hasher.Verify(guess, hash); err == nil {
			t.Fatalf("%q verified against the initial credential, so the account has a known password", guess)
		}
	}

	// The audit row names the operator and records that no password was set.
	var (
		actor   string
		action  string
		changes []byte
	)
	if err := e.pool.QueryRow(ctx, `
		SELECT actor_id, action, changes FROM audit_events
		WHERE target_type = 'customer' AND target_id = $1
		ORDER BY id DESC LIMIT 1`, record.CustomerID).Scan(&actor, &action, &changes); err != nil {
		t.Fatalf("read audit row: %v", err)
	}
	if actor != e.operator {
		t.Fatalf("audit actor = %q, want the operator %q", actor, e.operator)
	}
	if action != "customer.create" {
		t.Fatalf("audit action = %q, want customer.create", action)
	}
	if !strings.Contains(string(changes), `"login_state": "invitation_pending"`) {
		t.Fatalf("the audit row does not record that the account waits for an invitation: %s", changes)
	}
}

// A duplicate address is refused, in either case, and nothing is written.
func TestCreateCustomerRefusesADuplicateAddress(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	create := func(email string) error {
		return e.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			_, err := e.store.Create(ctx, tx, customer.CreateInput{
				Email: email, Name: "First", ActorID: e.operator,
			})
			return err
		})
	}
	if err := create("dup@example.test"); err != nil {
		t.Fatalf("first create: %v", err)
	}
	if err := create("DUP@Example.Test"); !errors.Is(err, customer.ErrEmailTaken) {
		t.Fatalf("second create = %v, want ErrEmailTaken", err)
	}

	var customers, users int
	if err := e.pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM customers), (SELECT count(*) FROM users)`).
		Scan(&customers, &users); err != nil {
		t.Fatalf("count: %v", err)
	}
	if customers != 1 || users != 2 {
		// Two users: the operator and the customer.
		t.Fatalf("customers = %d, users = %d; a refused create must write nothing", customers, users)
	}
}

// An exemption without a reason is refused: the tax engine would otherwise record a zero rate with
// nothing to justify it.
func TestCreateCustomerRefusesAnExemptionWithoutAReason(t *testing.T) {
	e := newEnv(t)
	err := e.pool.InTx(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := e.store.Create(ctx, tx, customer.CreateInput{
			Email: "exempt@example.test", Name: "Exempt", TaxExempt: true, ActorID: e.operator,
		})
		return err
	})
	if !errors.Is(err, customer.ErrTaxExemptionReason) {
		t.Fatalf("create = %v, want ErrTaxExemptionReason", err)
	}
}

// Editing a customer moves the login address with the account and stores the exemption where the
// tax engine reads it.
func TestUpdateCustomerMovesTheLoginAndSetsTheExemption(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	var record customer.Record
	if err := e.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		record, err = e.store.Create(ctx, tx, customer.CreateInput{
			Email: "edit-me@example.test", Name: "Before", ActorID: e.operator,
		})
		return err
	}); err != nil {
		t.Fatalf("create: %v", err)
	}

	newName, newEmail, exempt, reason := "After", "after@example.test", true, "EU reverse charge, VAT ID on file"
	if err := e.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := e.store.Update(ctx, tx, record.CustomerID, customer.UpdateInput{
			Name: &newName, Email: &newEmail, TaxExempt: &exempt, TaxExemptionReason: &reason,
			ActorID: e.operator,
		})
		return err
	}); err != nil {
		t.Fatalf("update: %v", err)
	}

	var (
		name, email, loginEmail, storedReason string
		storedExempt                          bool
	)
	if err := e.pool.QueryRow(ctx, `
		SELECT c.name, c.email, u.email, bp.tax_exempt, bp.tax_exemption_reason
		FROM customers c
		JOIN customer_accounts ca ON ca.customer_id = c.id
		JOIN users u ON u.id = ca.user_id
		JOIN billing_profiles bp ON bp.customer_id = c.id
		WHERE c.id = $1`, record.CustomerID).
		Scan(&name, &email, &loginEmail, &storedExempt, &storedReason); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if name != newName || email != newEmail {
		t.Fatalf("customer = %q/%q, want %q/%q", name, email, newName, newEmail)
	}
	if loginEmail != newEmail {
		t.Fatalf("login address = %q, want it to follow the account (%q)", loginEmail, newEmail)
	}
	if !storedExempt || storedReason != reason {
		t.Fatalf("exemption = %v/%q, want true/%q", storedExempt, storedReason, reason)
	}

	// Clearing the flag clears the reason with it: a leftover sentence on a taxable account is a
	// contradiction an auditor reads.
	cleared := false
	if err := e.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := e.store.Update(ctx, tx, record.CustomerID, customer.UpdateInput{
			TaxExempt: &cleared, ActorID: e.operator,
		})
		return err
	}); err != nil {
		t.Fatalf("clear exemption: %v", err)
	}
	if err := e.pool.QueryRow(ctx, `
		SELECT tax_exempt, tax_exemption_reason FROM billing_profiles WHERE customer_id = $1`,
		record.CustomerID).Scan(&storedExempt, &storedReason); err != nil {
		t.Fatalf("read exemption: %v", err)
	}
	if storedExempt || storedReason != "" {
		t.Fatalf("exemption = %v/%q after clearing, want false/\"\"", storedExempt, storedReason)
	}

	var actor, action string
	if err := e.pool.QueryRow(ctx, `
		SELECT actor_id, action FROM audit_events
		WHERE target_type = 'customer' AND target_id = $1 AND action = 'customer.update'
		ORDER BY id DESC LIMIT 1`, record.CustomerID).Scan(&actor, &action); err != nil {
		t.Fatalf("read audit row: %v", err)
	}
	if actor != e.operator {
		t.Fatalf("audit actor = %q, want %q", actor, e.operator)
	}
}

// ----------------------------------------------------------------- services --

// A manual service writes the subscription the renewal engine scans, with the period the price's
// own anchor produces — and no invoice.
func TestCreateServiceWritesTheSubscriptionTheRenewalEngineSees(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	customerID := seedCustomer(t, e.pool, "manual@example.test")
	productID, priceVersionID := seedProduct(t, e.pool, productOptions{stock: intPtr(10)})

	var result customer.ServiceResult
	if err := e.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		result, err = e.store.CreateService(ctx, tx, customer.ServiceInput{
			CustomerID: customerID, PriceVersionID: priceVersionID, Reason: "phone order, paid by transfer",
			ActorID: e.operator,
		})
		return err
	}); err != nil {
		t.Fatalf("create service: %v", err)
	}

	if result.SubscriptionID == "" {
		t.Fatal("no subscription was created, so the renewal engine would never see this service")
	}

	// The engine's own query is the assertion: an active subscription that is due at its period
	// end. Anything else would be a row it skips.
	var (
		status                       string
		interval                     string
		periodStart, periodEnd, next time.Time
	)
	if err := e.pool.QueryRow(ctx, `
		SELECT status, interval, current_period_start, current_period_end, next_billing_at
		FROM subscriptions WHERE id = $1`, result.SubscriptionID).
		Scan(&status, &interval, &periodStart, &periodEnd, &next); err != nil {
		t.Fatalf("read subscription: %v", err)
	}
	if status != "active" {
		t.Fatalf("subscription status = %q, want active: a pending subscription never renews", status)
	}
	if interval != "monthly" {
		t.Fatalf("interval = %q, want monthly", interval)
	}
	if !next.Equal(periodEnd) {
		t.Fatalf("next_billing_at = %s, want the period end %s", next, periodEnd)
	}
	// Anchor day 1 in the price's timezone (UTC): the period starts at midnight on the first of the
	// month and ends exactly one calendar month later. Deriving it from the anchor rather than
	// adding 30 days is the rule this proves. The values are normalised to UTC first, because a
	// timestamptz read back from PostgreSQL arrives in the session's location.
	start, end := periodStart.UTC(), periodEnd.UTC()
	if start.Day() != 1 || start.Hour() != 0 || start.Minute() != 0 {
		t.Fatalf("period start = %s, want midnight on the anchor day", start)
	}
	if !end.Equal(start.AddDate(0, 1, 0)) {
		t.Fatalf("period end = %s, want one calendar month after %s", end, start)
	}
	if !next.After(time.Now().UTC()) {
		t.Fatalf("next_billing_at = %s is in the past", next)
	}

	// The service row: pending (the queue delivers it), manual, and it says why and who.
	var (
		serviceStatus, origin, reason, actor string
		subscriptionID                       string
	)
	if err := e.pool.QueryRow(ctx, `
		SELECT s.status, s.origin, s.origin_reason, s.origin_actor::text,
		       COALESCE(s.subscription_id::text, '')
		FROM services s WHERE s.id = $1`, result.ServiceID).
		Scan(&serviceStatus, &origin, &reason, &actor, &subscriptionID); err != nil {
		t.Fatalf("read service: %v", err)
	}
	if serviceStatus != "pending" {
		t.Fatalf("service status = %q, want pending: the queue has not delivered it yet", serviceStatus)
	}
	if origin != "manual" || reason != "phone order, paid by transfer" || actor != e.operator {
		t.Fatalf("origin = %q/%q/%q, want manual/reason/operator", origin, reason, actor)
	}
	if subscriptionID != result.SubscriptionID {
		t.Fatalf("service subscription = %q, want %q", subscriptionID, result.SubscriptionID)
	}

	// Provisioning is queued, with a key that cannot collide with a purchase's.
	var (
		opKind, opStatus, opKey string
	)
	if err := e.pool.QueryRow(ctx, `
		SELECT kind, status, operation_key FROM service_operations WHERE service_id = $1`,
		result.ServiceID).Scan(&opKind, &opStatus, &opKey); err != nil {
		t.Fatalf("read operation: %v", err)
	}
	if opKind != "create" || opStatus != "queued" {
		t.Fatalf("operation = %q/%q, want create/queued", opKind, opStatus)
	}
	if opKey != "create:manual:"+result.ServiceID {
		t.Fatalf("operation key = %q, want create:manual:<service id>", opKey)
	}

	// Stock moved by exactly one, in the same transaction.
	var stock int
	if err := e.pool.QueryRow(ctx, `SELECT stock FROM products WHERE id = $1`, productID).Scan(&stock); err != nil {
		t.Fatalf("read stock: %v", err)
	}
	if stock != 9 {
		t.Fatalf("stock = %d, want 9", stock)
	}

	// No invoice and no order: a manual creation is a delivery, not a demand for money.
	var invoices, orders int
	if err := e.pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM invoices WHERE customer_id = $1),
		       (SELECT count(*) FROM orders WHERE customer_id = $1)`, customerID).
		Scan(&invoices, &orders); err != nil {
		t.Fatalf("count invoices: %v", err)
	}
	if invoices != 0 || orders != 0 {
		t.Fatalf("invoices = %d, orders = %d; a manual service must not raise a bill", invoices, orders)
	}

	// The audit row names the operator, the reason and the fact that nothing was invoiced.
	var (
		auditActor  string
		auditChange []byte
	)
	if err := e.pool.QueryRow(ctx, `
		SELECT actor_id, changes FROM audit_events
		WHERE action = 'service.create_manual' AND target_id = $1
		ORDER BY id DESC LIMIT 1`, result.ServiceID).Scan(&auditActor, &auditChange); err != nil {
		t.Fatalf("read audit row: %v", err)
	}
	if auditActor != e.operator {
		t.Fatalf("audit actor = %q, want the operator %q", auditActor, e.operator)
	}
	for _, want := range []string{"phone order, paid by transfer", `"invoiced": false`} {
		if !strings.Contains(string(auditChange), want) {
			t.Fatalf("audit changes do not mention %s: %s", want, auditChange)
		}
	}
}

// A comped service has no schedule behind it, so nothing can ever invoice it.
func TestCompedServiceHasNoSubscriptionAndNoInvoice(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	customerID := seedCustomer(t, e.pool, "comped@example.test")
	_, priceVersionID := seedProduct(t, e.pool, productOptions{})

	var result customer.ServiceResult
	if err := e.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		result, err = e.store.CreateService(ctx, tx, customer.ServiceInput{
			CustomerID: customerID, PriceVersionID: priceVersionID,
			Reason: "staff account, comped", Comp: true, ActorID: e.operator,
		})
		return err
	}); err != nil {
		t.Fatalf("create service: %v", err)
	}
	if !result.Comped {
		t.Fatal("the result does not report the service as comped")
	}
	if result.SubscriptionID != "" {
		t.Fatalf("subscription = %q, want none: a comped service must have no schedule", result.SubscriptionID)
	}

	var subscriptions, invoices int
	if err := e.pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM subscriptions WHERE customer_id = $1),
		       (SELECT count(*) FROM invoices WHERE customer_id = $1)`, customerID).
		Scan(&subscriptions, &invoices); err != nil {
		t.Fatalf("count: %v", err)
	}
	if subscriptions != 0 || invoices != 0 {
		t.Fatalf("subscriptions = %d, invoices = %d; a comped service must have neither", subscriptions, invoices)
	}
}

// A sold-out product is refused, exactly as checkout refuses it.
func TestCreateServiceRefusesASoldOutProduct(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	customerID := seedCustomer(t, e.pool, "soldout@example.test")
	_, priceVersionID := seedProduct(t, e.pool, productOptions{stock: intPtr(0)})

	err := e.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := e.store.CreateService(ctx, tx, customer.ServiceInput{
			CustomerID: customerID, PriceVersionID: priceVersionID,
			Reason: "phone order", ActorID: e.operator,
		})
		return err
	})
	if !errors.Is(err, customer.ErrOutOfStock) {
		t.Fatalf("create on a sold-out product = %v, want ErrOutOfStock", err)
	}

	var services int
	if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM services`).Scan(&services); err != nil {
		t.Fatalf("count services: %v", err)
	}
	if services != 0 {
		t.Fatalf("services = %d; a refused creation must write nothing", services)
	}
}

// The per-customer limit counts what the customer already holds, and a manual creation cannot
// step over it.
func TestCreateServiceRespectsThePerUserLimit(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	customerID := seedCustomer(t, e.pool, "limited@example.test")
	_, priceVersionID := seedProduct(t, e.pool, productOptions{perUserLimit: intPtr(1)})

	create := func() error {
		return e.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			_, err := e.store.CreateService(ctx, tx, customer.ServiceInput{
				CustomerID: customerID, PriceVersionID: priceVersionID,
				Reason: "phone order", ActorID: e.operator,
			})
			return err
		})
	}
	if err := create(); err != nil {
		t.Fatalf("first create: %v", err)
	}
	if err := create(); !errors.Is(err, customer.ErrPerUserLimit) {
		t.Fatalf("second create = %v, want ErrPerUserLimit", err)
	}
}

// A nonexistent customer is a distinct refusal, so the handler can answer 404 rather than letting a
// foreign-key violation become a 500.
func TestCreateServiceForAMissingCustomerIsNotFound(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	_, priceVersionID := seedProduct(t, e.pool, productOptions{})

	err := e.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := e.store.CreateService(ctx, tx, customer.ServiceInput{
			CustomerID:     "00000000-0000-0000-0000-000000000000",
			PriceVersionID: priceVersionID, Reason: "typo", ActorID: e.operator,
		})
		return err
	})
	if !errors.Is(err, customer.ErrCustomerNotFound) {
		t.Fatalf("create for a missing customer = %v, want ErrCustomerNotFound", err)
	}
}

// A selection the product does not offer is refused rather than silently dropped, and a valid one
// is stored with the amount captured at selection time.
func TestCreateServiceValidatesAndStoresTheConfigSelection(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	customerID := seedCustomer(t, e.pool, "config@example.test")
	productID, priceVersionID := seedProduct(t, e.pool, productOptions{})

	var optionID string
	if err := e.pool.QueryRow(ctx, `
		INSERT INTO config_options (product_id, name, option_type, choices, required)
		VALUES ($1, 'Memory', 'dropdown', '["2 GB","8 GB"]'::jsonb, true)
		RETURNING id`, productID).Scan(&optionID); err != nil {
		t.Fatalf("seed option: %v", err)
	}
	if _, err := e.pool.Exec(ctx, `
		INSERT INTO config_option_prices (config_option_id, price_version_id, choice, amount_minor, setup_minor)
		VALUES ($1, $2, '8 GB', 1500, 0)`, optionID, priceVersionID); err != nil {
		t.Fatalf("seed option price: %v", err)
	}

	// A required option that was not chosen is refused.
	err := e.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := e.store.CreateService(ctx, tx, customer.ServiceInput{
			CustomerID: customerID, PriceVersionID: priceVersionID,
			Reason: "phone order", ActorID: e.operator,
		})
		return err
	})
	if !errors.Is(err, customer.ErrInvalidConfig) {
		t.Fatalf("create without the required option = %v, want ErrInvalidConfig", err)
	}

	var result customer.ServiceResult
	if err := e.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		result, err = e.store.CreateService(ctx, tx, customer.ServiceInput{
			CustomerID: customerID, PriceVersionID: priceVersionID,
			Config: map[string]string{optionID: "8 GB"},
			Reason: "phone order", ActorID: e.operator,
		})
		return err
	}); err != nil {
		t.Fatalf("create with the option: %v", err)
	}
	if len(result.Options) != 1 {
		t.Fatalf("options = %d, want 1", len(result.Options))
	}

	var (
		value  string
		amount int64
	)
	if err := e.pool.QueryRow(ctx, `
		SELECT value, amount_minor FROM service_configs WHERE service_id = $1`,
		result.ServiceID).Scan(&value, &amount); err != nil {
		t.Fatalf("read service config: %v", err)
	}
	if value != "8 GB" || amount != 1500 {
		t.Fatalf("stored selection = %q/%d, want 8 GB/1500", value, amount)
	}
}

// A reason is required, because a service with no order behind it is exactly what an auditor asks
// about.
func TestCreateServiceRequiresAReason(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	customerID := seedCustomer(t, e.pool, "noreason@example.test")
	_, priceVersionID := seedProduct(t, e.pool, productOptions{})

	err := e.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := e.store.CreateService(ctx, tx, customer.ServiceInput{
			CustomerID: customerID, PriceVersionID: priceVersionID,
			Reason: "   ", ActorID: e.operator,
		})
		return err
	})
	if !errors.Is(err, customer.ErrReasonRequired) {
		t.Fatalf("create without a reason = %v, want ErrReasonRequired", err)
	}
}

// seedCustomer inserts a billing account without a login: these tests are about the service, and
// the customer's login is covered above.
func seedCustomer(t *testing.T, pool *store.Pool, email string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO customers (email, name) VALUES ($1, 'Manual Customer') RETURNING id`,
		email).Scan(&id); err != nil {
		t.Fatalf("seed customer: %v", err)
	}
	return id
}

// A manual service is not just a row: the renewal engine bills it at the end of the period it was
// given, because the subscription it created is the one the engine scans. That is the whole reason
// this path writes a subscription instead of leaving the operator to invoice by hand.
func TestAManualServiceIsBilledByTheRenewalEngine(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	customerID := seedCustomer(t, e.pool, "renew-me@example.test")
	_, priceVersionID := seedProduct(t, e.pool, productOptions{})

	var result customer.ServiceResult
	if err := e.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		result, err = e.store.CreateService(ctx, tx, customer.ServiceInput{
			CustomerID: customerID, PriceVersionID: priceVersionID,
			Reason: "phone order", ActorID: e.operator,
		})
		return err
	}); err != nil {
		t.Fatalf("create service: %v", err)
	}

	// Pin the engine to the instant the first period ends: that is when the subscription becomes
	// due, and it is the only way to test the schedule without waiting a month.
	engine := renewal.NewEngine(e.pool,
		billing.NewIssuer(billing.FixedTaxPricer{Policy: "no-tax", Jurisdiction: "none"}),
		billing.Merchant{Name: "Test Hosting", CountryCode: "DE"}, "net 14")
	engine.Now = func() time.Time { return result.NextBillAt }

	results, err := engine.Run(ctx, 10)
	if err != nil {
		t.Fatalf("renewal run: %v", err)
	}
	var invoiced int
	for _, res := range results {
		if res.SubscriptionID == result.SubscriptionID {
			if res.Outcome != renewal.OutcomeInvoiced {
				t.Fatalf("renewal outcome = %q (%s), want invoiced", res.Outcome, res.Detail)
			}
			invoiced++
		}
	}
	if invoiced != 1 {
		t.Fatalf("the renewal engine invoiced the manual subscription %d times, want once", invoiced)
	}

	// One invoice, priced from the catalog row the service was built from.
	var (
		total   int64
		orderID *string
	)
	if err := e.pool.QueryRow(ctx, `
		SELECT total_minor, order_id FROM invoices WHERE customer_id = $1`, customerID).
		Scan(&total, &orderID); err != nil {
		t.Fatalf("read renewal invoice: %v", err)
	}
	if total != 5000 {
		t.Fatalf("renewal total = %d, want the catalog amount 5000", total)
	}
	if orderID != nil {
		t.Fatal("the renewal invoice carries an order id; a manual service has no order")
	}
}

// The reason is a database constraint, not only a handler check: a row that would leave "why is
// this service here" unanswerable cannot be stored, whoever writes it.
func TestDatabaseRefusesAManualServiceWithNoReason(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	customerID := seedCustomer(t, e.pool, "no-reason-row@example.test")
	productID, _ := seedProduct(t, e.pool, productOptions{})

	_, err := e.pool.Exec(ctx, `
		INSERT INTO services (customer_id, product_id, status, origin, origin_reason)
		VALUES ($1, $2, 'pending', 'manual', '   ')`, customerID, productID)
	if err == nil {
		t.Fatal("a manual service with a blank reason was stored")
	}
	if !strings.Contains(err.Error(), "services_manual_reason") {
		t.Fatalf("refusal = %v, want the services_manual_reason constraint", err)
	}
}
