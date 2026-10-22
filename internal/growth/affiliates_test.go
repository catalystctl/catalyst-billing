package growth_test

import (
	"context"
	"errors"
	"testing"

	"github.com/catalyst/billing/internal/growth"
	"github.com/catalyst/billing/internal/money"
	"github.com/catalyst/billing/internal/store"
	"github.com/catalyst/billing/internal/store/storetest"
	"github.com/jackc/pgx/v5"
)

// ------------------------------------------------------------------ fixtures --

// world is a referred customer with an affiliate, and the order/invoice that pays them.
type world struct {
	pool          *store.Pool
	store         *growth.Store
	affiliateID   string
	affiliateUser string
	customerID    string
	customerUser  string
	orderID       string
	invoiceID     string
}

// newWorld creates an affiliate and a referred customer whose signup was attributed in
// the same transaction, then a paid order and its paid invoice.
//
// The attribution is done the way registration will do it — customer and attribution in
// one transaction — because the "customer must be new in this transaction" rule is what
// makes an existing email refuse.
func newWorld(t *testing.T, rate string) *world {
	t.Helper()
	pool := storetest.DB(t)
	ctx := context.Background()
	w := &world{pool: pool, store: growth.New(pool)}

	// The affiliate's own user and customer account.
	if err := pool.QueryRow(ctx, `
		INSERT INTO users (email, password_hash, email_verified_at)
		VALUES ('affiliate@example.test', 'x', now()) RETURNING id`).Scan(&w.affiliateUser); err != nil {
		t.Fatalf("affiliate user: %v", err)
	}
	var affiliateCustomer string
	if err := pool.QueryRow(ctx, `
		INSERT INTO customers (email, name) VALUES ('affiliate@example.test', 'Affiliate')
		RETURNING id`).Scan(&affiliateCustomer); err != nil {
		t.Fatalf("affiliate customer: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO customer_accounts (user_id, customer_id) VALUES ($1, $2)`,
		w.affiliateUser, affiliateCustomer); err != nil {
		t.Fatalf("affiliate account link: %v", err)
	}
	if err := pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		created, err := w.store.Create(ctx, tx, growth.NewAffiliate{
			UserID:         w.affiliateUser,
			Code:           "PARTNER10",
			CommissionRate: rate,
			PayoutMethod:   "credit",
		})
		if err != nil {
			return err
		}
		w.affiliateID = created.ID
		return nil
	}); err != nil {
		t.Fatalf("create affiliate: %v", err)
	}

	// The referred signup: customer + user + account + attribution in one transaction,
	// which is the contract AttributeSignup documents.
	if err := pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `
			INSERT INTO customers (email, name) VALUES ('referred@example.test', 'Referred')
			RETURNING id`).Scan(&w.customerID); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `
			INSERT INTO users (email, password_hash, email_verified_at)
			VALUES ('referred@example.test', 'x', now()) RETURNING id`).Scan(&w.customerUser); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO customer_accounts (user_id, customer_id) VALUES ($1, $2)`,
			w.customerUser, w.customerID); err != nil {
			return err
		}
		_, err := growth.AttributeSignup(ctx, tx, "partner10", w.customerID)
		return err
	}); err != nil {
		t.Fatalf("attribute signup: %v", err)
	}

	// The order and its invoice, already paid, with an odd total so rounding is visible.
	if err := pool.QueryRow(ctx, `
		INSERT INTO orders (customer_id, status, currency, total_minor)
		VALUES ($1, 'paid', 'USD', 12345) RETURNING id`, w.customerID).Scan(&w.orderID); err != nil {
		t.Fatalf("order: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO invoices (number, customer_id, order_id, status, currency,
			subtotal_minor, tax_minor, total_minor, amount_paid_minor, issued_at, snapshot)
		VALUES ('INV-G1', $1, $2, 'paid', 'USD', 12345, 0, 12345, 12345, now(),
		        '{"schema_version":1}'::jsonb)
		RETURNING id`, w.customerID, w.orderID).Scan(&w.invoiceID); err != nil {
		t.Fatalf("invoice: %v", err)
	}
	return w
}

// usd resolves USD for assertions.
func usd(t *testing.T) money.Currency {
	t.Helper()
	cur, ok := money.LookupCurrency("USD")
	if !ok {
		t.Fatal("USD is not a known currency")
	}
	return cur
}

// ------------------------------------------------------------- exact commission --

// 10% of 12.34 (1234 minor) is 1.234, which is not representable in cents. The commission
// must be exactly 123 minor units (half-up on the exact rational 1234/10), and it must be
// the same number every time it is computed: a float would give 123.39999... and a
// different result under a different rounding.
func TestCommissionIsExactForOddAmount(t *testing.T) {
	cur := usd(t)
	amount := money.New(1234, cur)

	got, err := growth.Commission(amount, "10%")
	if err != nil {
		t.Fatalf("commission: %v", err)
	}
	if got.Minor() != 123 {
		t.Fatalf("10%% of %s = %d, want 123", amount, got.Minor())
	}

	// The three spellings of the same rate must agree exactly.
	for _, rate := range []string{"10%", "0.1", "1/10"} {
		alt, err := growth.Commission(amount, rate)
		if err != nil {
			t.Fatalf("commission %q: %v", rate, err)
		}
		if alt.Minor() != got.Minor() {
			t.Fatalf("rate %q gave %d, want %d (rates must be exact and equivalent)",
				rate, alt.Minor(), got.Minor())
		}
	}

	// A one-cent amount at 10% is 0.1 of a cent: half-up rounds it to 0, not 1, because
	// the exact value is below one half. This is the case a float drifts on.
	penny, err := growth.Commission(money.New(1, cur), "10%")
	if err != nil {
		t.Fatalf("commission on a penny: %v", err)
	}
	if penny.Minor() != 0 {
		t.Fatalf("10%% of 0.01 = %d, want 0", penny.Minor())
	}

	// And the canonical form of the rate is the exact rational, not the decimal that
	// happened to parse.
	_, canonical, err := growth.ParseRate("10%")
	if err != nil {
		t.Fatalf("parse rate: %v", err)
	}
	if canonical != "1/10" {
		t.Fatalf("canonical rate = %q, want 1/10", canonical)
	}
}

// A rate outside [0, 1] is refused rather than silently clamped: a 150% commission is a
// configuration mistake that would pay out more than the order took.
func TestParseRateRejectsOutOfRange(t *testing.T) {
	for _, bad := range []string{"", "-0.1", "150%", "2", "abc", "1/0"} {
		if _, _, err := growth.ParseRate(bad); !errors.Is(err, growth.ErrInvalidRate) {
			t.Fatalf("rate %q: err = %v, want ErrInvalidRate", bad, err)
		}
	}
	for _, good := range []string{"0", "0.1", "10%", "1/10", "100%", "1"} {
		if _, _, err := growth.ParseRate(good); err != nil {
			t.Fatalf("rate %q: unexpected error %v", good, err)
		}
	}
}

// --------------------------------------------------------------- replay guard --

// A replayed "order paid" event must not double the commission or the balance. The guard
// is the unique index on (order_id), not a service check, so this asserts the second call
// returns Duplicate and the money is unchanged.
func TestReplayedOrderPaidDoesNotDoubleCommission(t *testing.T) {
	w := newWorld(t, "10%")
	ctx := context.Background()

	var first, second growth.Referral
	if err := w.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		first, err = growth.RecordCommissionForPaidInvoice(ctx, tx, w.invoiceID)
		return err
	}); err != nil {
		t.Fatalf("first commission: %v", err)
	}
	if first.Duplicate {
		t.Fatal("the first call reported a duplicate")
	}
	if first.Commission.Minor() != 1235 {
		// 10% of 12345 is 1234.5, which half-up rounds to 1235. Asserting the exact value
		// means a rounding change is caught here rather than in a payout.
		t.Fatalf("commission = %d, want 1235 (10%% of 12345, half-up)", first.Commission.Minor())
	}

	// The replay.
	if err := w.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		second, err = growth.RecordCommissionForPaidInvoice(ctx, tx, w.invoiceID)
		return err
	}); err != nil {
		t.Fatalf("replayed commission: %v", err)
	}
	if !second.Duplicate {
		t.Fatal("the replay was not reported as a duplicate")
	}
	if second.Commission.Minor() != first.Commission.Minor() {
		t.Fatalf("replay commission = %d, first = %d", second.Commission.Minor(), first.Commission.Minor())
	}

	var rows int
	if err := w.pool.QueryRow(ctx, `
		SELECT count(*) FROM affiliate_referrals WHERE order_id = $1`, w.orderID).Scan(&rows); err != nil {
		t.Fatalf("count commissions: %v", err)
	}
	if rows != 1 {
		t.Fatalf("%d commission rows for one order, want 1", rows)
	}

	var balance int64
	if err := w.pool.QueryRow(ctx, `
		SELECT balance_minor FROM affiliate_balances WHERE affiliate_id = $1 AND currency = 'USD'`,
		w.affiliateID).Scan(&balance); err != nil {
		t.Fatalf("load balance: %v", err)
	}
	if balance != first.Commission.Minor() {
		t.Fatalf("balance = %d, want %d (the replay must not increment it)",
			balance, first.Commission.Minor())
	}

	// The order-based entry point is guarded by the same index, so it agrees.
	if err := w.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		again, err := growth.RecordCommission(ctx, tx, w.orderID)
		if err != nil {
			return err
		}
		if !again.Duplicate {
			t.Fatal("RecordCommission on a paid order was not a duplicate")
		}
		return nil
	}); err != nil {
		t.Fatalf("order-based replay: %v", err)
	}
}

// ------------------------------------------------------------ existing account --

// An existing email is not silently merged into a referral. The registration flow creates
// the customer in its own transaction; attributing a customer that already existed must be
// refused, because the affiliate did not bring them and their future orders must not move.
func TestAttributionRefusesExistingCustomer(t *testing.T) {
	w := newWorld(t, "10%")
	ctx := context.Background()

	// w.customerID was created by an earlier transaction, so attributing it again in a new
	// transaction is the "existing account" case.
	err := w.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := growth.AttributeSignup(ctx, tx, "PARTNER10", w.customerID)
		return err
	})
	if !errors.Is(err, growth.ErrExistingAccount) {
		t.Fatalf("attributing an existing customer: err = %v, want ErrExistingAccount", err)
	}

	// Nothing was written for the attempt: still exactly one attribution row.
	var rows int
	if err := w.pool.QueryRow(ctx, `
		SELECT count(*) FROM affiliate_referrals
		WHERE customer_id = $1 AND order_id IS NULL`, w.customerID).Scan(&rows); err != nil {
		t.Fatalf("count attributions: %v", err)
	}
	if rows != 1 {
		t.Fatalf("%d attribution rows, want 1 (the refusal must write nothing)", rows)
	}
}

// A different code for an already-attributed customer is a conflict, not a re-point: moving
// an attribution moves money between affiliates.
func TestAttributionRefusesSecondCode(t *testing.T) {
	w := newWorld(t, "10%")
	ctx := context.Background()

	// A second affiliate.
	var otherUser string
	if err := w.pool.QueryRow(ctx, `
		INSERT INTO users (email, password_hash, email_verified_at)
		VALUES ('other@example.test', 'x', now()) RETURNING id`).Scan(&otherUser); err != nil {
		t.Fatalf("other user: %v", err)
	}
	if err := w.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := w.store.Create(ctx, tx, growth.NewAffiliate{
			UserID: otherUser, Code: "RIVAL5", CommissionRate: "0.05", PayoutMethod: "credit",
		})
		return err
	}); err != nil {
		t.Fatalf("second affiliate: %v", err)
	}

	// Attribute the existing customer to the rival code, from a transaction that did not
	// create them: the existing-account refusal fires first, which is the stronger rule.
	err := w.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := growth.AttributeSignup(ctx, tx, "RIVAL5", w.customerID)
		return err
	})
	if !errors.Is(err, growth.ErrExistingAccount) {
		t.Fatalf("err = %v, want ErrExistingAccount", err)
	}
}

// An unknown or disabled code is ErrUnknownCode.
func TestAttributionUnknownCode(t *testing.T) {
	w := newWorld(t, "10%")
	ctx := context.Background()

	var fresh string
	err := w.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `
			INSERT INTO customers (email, name) VALUES ('newbie@example.test', 'Newbie')
			RETURNING id`).Scan(&fresh); err != nil {
			return err
		}
		_, err := growth.AttributeSignup(ctx, tx, "NOPE123", fresh)
		return err
	})
	if !errors.Is(err, growth.ErrUnknownCode) {
		t.Fatalf("err = %v, want ErrUnknownCode", err)
	}
}

// -------------------------------------------------------------------- payout --

// The payout settles the balance, marks the commissions paid, and a second call is
// refused: a double-click pays once.
func TestPayoutSettlesAndRefusesSecondCall(t *testing.T) {
	w := newWorld(t, "10%")
	ctx := context.Background()

	if err := w.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := growth.RecordCommissionForPaidInvoice(ctx, tx, w.invoiceID)
		return err
	}); err != nil {
		t.Fatalf("commission: %v", err)
	}

	lines, err := w.store.PayoutReport(ctx)
	if err != nil {
		t.Fatalf("payout report: %v", err)
	}
	if len(lines) != 1 {
		t.Fatalf("%d payout lines, want 1", len(lines))
	}
	if lines[0].Owed.Minor() != 1235 {
		t.Fatalf("owed = %d, want 1235", lines[0].Owed.Minor())
	}

	paid, err := w.store.PayOut(ctx, w.affiliateID, "USD")
	if err != nil {
		t.Fatalf("payout: %v", err)
	}
	if paid.Amount.Minor() != 1235 || paid.Commissions != 1 {
		t.Fatalf("payout = %d minor across %d commissions, want 1235 across 1",
			paid.Amount.Minor(), paid.Commissions)
	}

	// The report is now empty and a second payout is refused.
	lines, err = w.store.PayoutReport(ctx)
	if err != nil {
		t.Fatalf("payout report after: %v", err)
	}
	if len(lines) != 0 {
		t.Fatalf("%d payout lines after settling, want 0", len(lines))
	}
	if _, err := w.store.PayOut(ctx, w.affiliateID, "USD"); !errors.Is(err, growth.ErrNothingToPay) {
		t.Fatalf("second payout: err = %v, want ErrNothingToPay", err)
	}

	// The balance moved to lifetime paid, and the commission row records when.
	var balance, lifetime int64
	if err := w.pool.QueryRow(ctx, `
		SELECT balance_minor, paid_minor FROM affiliate_balances
		WHERE affiliate_id = $1 AND currency = 'USD'`, w.affiliateID).Scan(&balance, &lifetime); err != nil {
		t.Fatalf("load balance: %v", err)
	}
	if balance != 0 || lifetime != 1235 {
		t.Fatalf("balance/paid = %d/%d, want 0/1235", balance, lifetime)
	}
	var paidAt *string
	if err := w.pool.QueryRow(ctx, `
		SELECT paid_at::text FROM affiliate_referrals WHERE order_id = $1`, w.orderID).Scan(&paidAt); err != nil {
		t.Fatalf("load commission: %v", err)
	}
	if paidAt == nil {
		t.Fatal("the commission has no paid_at after a payout")
	}
}

// A commission on an unpaid order is refused: a promise the books do not support.
func TestCommissionRequiresPaidOrder(t *testing.T) {
	w := newWorld(t, "10%")
	ctx := context.Background()

	var pending string
	if err := w.pool.QueryRow(ctx, `
		INSERT INTO orders (customer_id, status, currency, total_minor)
		VALUES ($1, 'pending_payment', 'USD', 5000) RETURNING id`, w.customerID).Scan(&pending); err != nil {
		t.Fatalf("pending order: %v", err)
	}
	err := w.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := growth.RecordCommission(ctx, tx, pending)
		return err
	})
	if !errors.Is(err, growth.ErrInvalidInput) {
		t.Fatalf("err = %v, want ErrInvalidInput", err)
	}
}

// A customer who was not referred produces no commission, and that is a sentinel rather
// than an error so an event path can ignore it.
func TestCommissionForUnreferredCustomerIsNoop(t *testing.T) {
	w := newWorld(t, "10%")
	ctx := context.Background()

	var stranger string
	var strangerOrder string
	if err := w.pool.QueryRow(ctx, `
		INSERT INTO customers (email, name) VALUES ('stranger@example.test', 'Stranger')
		RETURNING id`).Scan(&stranger); err != nil {
		t.Fatalf("stranger: %v", err)
	}
	if err := w.pool.QueryRow(ctx, `
		INSERT INTO orders (customer_id, status, currency, total_minor)
		VALUES ($1, 'paid', 'USD', 5000) RETURNING id`, stranger).Scan(&strangerOrder); err != nil {
		t.Fatalf("stranger order: %v", err)
	}
	err := w.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := growth.RecordCommission(ctx, tx, strangerOrder)
		return err
	})
	if !errors.Is(err, growth.ErrNotReferred) {
		t.Fatalf("err = %v, want ErrNotReferred", err)
	}
}

// The portal view is scoped by user id, and an affiliate sees only their own rows.
func TestByUserIDIsScoped(t *testing.T) {
	w := newWorld(t, "10%")
	ctx := context.Background()

	got, err := w.store.ByUserID(ctx, w.affiliateUser)
	if err != nil {
		t.Fatalf("by user id: %v", err)
	}
	if got.ID != w.affiliateID {
		t.Fatalf("resolved affiliate %s, want %s", got.ID, w.affiliateID)
	}
	if got.ReferredCount != 1 {
		t.Fatalf("referred_count = %d, want 1", got.ReferredCount)
	}

	// The referred customer is not an affiliate.
	if _, err := w.store.ByUserID(ctx, w.customerUser); !errors.Is(err, growth.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}
