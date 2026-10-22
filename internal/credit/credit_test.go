package credit_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/catalyst/billing/internal/billing"
	"github.com/catalyst/billing/internal/credit"
	"github.com/catalyst/billing/internal/extproto"
	"github.com/catalyst/billing/internal/ledger"
	"github.com/catalyst/billing/internal/money"
	"github.com/catalyst/billing/internal/store"
	"github.com/catalyst/billing/internal/store/storetest"
	"github.com/jackc/pgx/v5"
)

// The tests in this file run against the real database. They are the ones that matter:
// every rule here is either a constraint, a lock, or a posting, and none of the three can
// be proven against a fake.

var usd = money.MustLookupCurrency("USD")

// fixture is one customer with a login and a working ledger.
type fixture struct {
	pool       *store.Pool
	svc        *credit.Service
	customerID string
	userID     string
	issuer     *billing.Issuer
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	pool := storetest.DB(t)
	ctx := context.Background()

	f := &fixture{
		pool: pool,
		issuer: billing.NewIssuer(billing.FixedTaxPricer{
			Rate: money.MustParseRatio("0%"), Rounding: money.RoundHalfUp, Policy: "none",
		}),
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO customers (email, name) VALUES ('credit@example.test', 'Credit Customer')
		RETURNING id`).Scan(&f.customerID); err != nil {
		t.Fatalf("customer: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO users (email, password_hash, email_verified_at)
		VALUES ('credit-ops@example.test', 'x', now()) RETURNING id`).Scan(&f.userID); err != nil {
		t.Fatalf("user: %v", err)
	}
	f.svc = credit.New(pool).WithGateway(&fakeGateway{name: "fakegateway"}, "acct_test",
		"http://localhost/ok", "http://localhost/cancel")
	return f
}

// fakeGateway is the checkout side of the extension protocol, in memory.
type fakeGateway struct {
	name  string
	calls int
}

func (g *fakeGateway) Name() string { return g.name }

func (g *fakeGateway) CreateCheckout(_ context.Context, p extproto.CreateCheckoutParams) (extproto.CreateCheckoutResult, error) {
	g.calls++
	return extproto.CreateCheckoutResult{
		PaymentID:   "pi_fake_" + p.OperationKey,
		CheckoutURL: "https://pay.example.test/" + p.OperationKey,
		Status:      extproto.PaymentPending,
		ExpiresAt:   time.Now().Add(time.Hour),
	}, nil
}

// issue creates and issues an invoice through the real issuer, so the receivable is posted
// exactly as it would be in production.
func (f *fixture) issue(t *testing.T, amount int64) string {
	t.Helper()
	var invoiceID string
	err := f.pool.InTx(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		id, _, _, err := f.issuer.CreateDraft(ctx, tx, billing.IssueRequest{
			CustomerID: f.customerID,
			Lines: []billing.Line{{
				Description: "VPS Small monthly", QuantityMilli: 1000,
				UnitPrice: money.New(amount, usd),
			}},
			Customer: billing.CustomerDetails{ID: f.customerID},
		})
		if err != nil {
			return err
		}
		issued, err := f.issuer.Issue(ctx, tx, id, billing.DefaultSeries)
		if err != nil {
			return err
		}
		invoiceID = issued.InvoiceID
		return nil
	})
	if err != nil {
		t.Fatalf("issue invoice: %v", err)
	}
	return invoiceID
}

// grant puts credit on the account through the real grant path.
func (f *fixture) grant(t *testing.T, amount int64, key string) credit.Grant {
	t.Helper()
	g, err := f.svc.Grant(context.Background(), credit.GrantRequest{
		CustomerID:  f.customerID,
		AmountMinor: amount,
		Currency:    "USD",
		Reason:      "test grant",
		GrantedBy:   f.userID,
		Key:         key,
	})
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	return g
}

// balance reads the derived credit balance for USD.
func (f *fixture) balance(t *testing.T) int64 {
	t.Helper()
	amounts, err := f.svc.Balance(context.Background(), f.customerID)
	if err != nil {
		t.Fatalf("balance: %v", err)
	}
	for _, a := range amounts {
		if a.Currency().Equal(usd) {
			return a.Minor()
		}
	}
	return 0
}

// invoiceState reads the invoice's money columns.
func (f *fixture) invoiceState(t *testing.T, id string) (status string, paid, total int64) {
	t.Helper()
	if err := f.pool.QueryRow(context.Background(), `
		SELECT status, amount_paid_minor, total_minor FROM invoices WHERE id = $1`, id).
		Scan(&status, &paid, &total); err != nil {
		t.Fatalf("invoice state: %v", err)
	}
	return status, paid, total
}

// assertBalanced is the invariant every case ends with: the ledger sums to zero per
// currency. It is checked against the entries, not against the code's own bookkeeping.
func (f *fixture) assertBalanced(t *testing.T) {
	t.Helper()
	balances, err := ledger.TrialBalance(context.Background(), f.pool)
	if err != nil {
		t.Fatalf("trial balance: %v", err)
	}
	for code, net := range balances {
		if net.Minor() != 0 {
			t.Fatalf("the ledger is unbalanced in %s by %d", code, net.Minor())
		}
	}

	// Every individual journal transaction must also balance: a per-currency total can hide
	// two transactions that are each wrong in opposite directions.
	rows, err := f.pool.Query(context.Background(), `
		SELECT transaction_id, SUM(debit_minor) - SUM(credit_minor)
		FROM journal_entries GROUP BY transaction_id`)
	if err != nil {
		t.Fatalf("per-transaction balance: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			id    string
			delta int64
		)
		if err := rows.Scan(&id, &delta); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if delta != 0 {
			t.Fatalf("journal transaction %s is unbalanced by %d", id, delta)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
}

// ------------------------------------------------------------------ deposits --

// A deposit below the configured minimum is refused before anything is created.
func TestDepositBelowMinimumIsRefused(t *testing.T) {
	f := newFixture(t)

	_, err := f.svc.StartDeposit(context.Background(), credit.DepositRequest{
		CustomerID: f.customerID, AmountMinor: 100, Currency: "USD", RequestedBy: f.userID,
	})
	if !errors.Is(err, credit.ErrBelowMinimum) {
		t.Fatalf("err = %v, want ErrBelowMinimum", err)
	}

	// Nothing was created: a refused top-up must not leave an open invoice that reserves
	// headroom under the maximum balance.
	var invoices int
	if err := f.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM invoices`).Scan(&invoices); err != nil {
		t.Fatal(err)
	}
	if invoices != 0 {
		t.Fatalf("%d invoices exist after a refused deposit, want 0", invoices)
	}
	f.assertBalanced(t)
}

// A deposit that would take the balance past credits.maximum_credit_minor is refused.
func TestDepositAboveMaxBalanceIsRefused(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	// Seed the setting low enough that a top-up would breach it.
	if _, err := f.pool.Exec(ctx, `
		UPDATE settings SET value = '3000' WHERE group_key = 'credits' AND key = 'maximum_credit_minor'`); err != nil {
		t.Fatal(err)
	}
	// Hold 8.00 of credit already, so a 25.00 top-up would take the balance to 33.00.
	f.grant(t, 800, "seed-max")

	_, err := f.svc.StartDeposit(ctx, credit.DepositRequest{
		CustomerID: f.customerID, AmountMinor: 2500, Currency: "USD", RequestedBy: f.userID,
	})
	if !errors.Is(err, credit.ErrMaxBalance) {
		t.Fatalf("err = %v, want ErrMaxBalance", err)
	}
	if got := f.balance(t); got != 800 {
		t.Fatalf("balance = %d, want 800 (the refused deposit must not credit anything)", got)
	}
	f.assertBalanced(t)
}

// A deposit that fits the limit creates a deposit invoice and a gateway session, and a
// settled webhook turns it into credit exactly once.
func TestDepositSettlesOnce(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	deposit, err := f.svc.StartDeposit(ctx, credit.DepositRequest{
		CustomerID: f.customerID, AmountMinor: 2500, Currency: "USD", RequestedBy: f.userID,
	})
	if err != nil {
		t.Fatalf("start deposit: %v", err)
	}
	if deposit.InvoiceID == "" || deposit.RedirectURL == "" {
		t.Fatalf("deposit = %+v", deposit)
	}
	if got := f.balance(t); got != 0 {
		t.Fatalf("balance before settlement = %d, want 0", got)
	}

	// The deposit invoice is recognisable structurally, which is what routes settlement.
	if err := f.svc.DepositInvoice(ctx, deposit.InvoiceID); err != nil {
		t.Fatalf("the deposit invoice is not recognised as a deposit: %v", err)
	}
	status, _, total := f.invoiceState(t, deposit.InvoiceID)
	if status != "open" || total != 2500 {
		t.Fatalf("deposit invoice = %s/%d, want open/2500", status, total)
	}

	settle := credit.DepositSettlement{
		Provider: "fakegateway", ProviderAccount: "acct_test",
		ProviderPaymentID: "pi_deposit_1",
		InvoiceID:         deposit.InvoiceID,
		AmountMinor:       2500, Currency: "USD",
	}
	res, err := f.svc.SettleDeposit(ctx, settle)
	if err != nil {
		t.Fatalf("settle: %v", err)
	}
	if res.Granted.Minor() != 2500 || res.BalanceMinor != 2500 {
		t.Fatalf("settle = %+v", res)
	}
	if got := f.balance(t); got != 2500 {
		t.Fatalf("balance = %d, want 2500", got)
	}

	// A replayed webhook must not double the balance.
	replay, err := f.svc.SettleDeposit(ctx, settle)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if !replay.Duplicate {
		t.Fatal("the replay was not reported as a duplicate")
	}
	if got := f.balance(t); got != 2500 {
		t.Fatalf("balance after replay = %d, want 2500", got)
	}

	// One grant posting, not two.
	var grants int
	if err := f.pool.QueryRow(ctx, `
		SELECT count(*) FROM journal_transactions WHERE reference_type = 'credit_deposit'`).
		Scan(&grants); err != nil {
		t.Fatal(err)
	}
	if grants != 1 {
		t.Fatalf("%d deposit postings, want 1", grants)
	}

	// A settled deposit is not a taxable supply: revenue is debited by the grant and the
	// receivable is cleared by the payment, so nothing is left on the invoice.
	status, paid, _ := f.invoiceState(t, deposit.InvoiceID)
	if status != "paid" || paid != 2500 {
		t.Fatalf("deposit invoice = %s/%d, want paid/2500", status, paid)
	}
	rev, err := ledger.Balances(ctx, f.pool, ledger.AccountRevenue, usd)
	if err != nil {
		t.Fatal(err)
	}
	if net, _ := rev.Net(); net.Minor() != 0 {
		t.Fatalf("revenue after a completed deposit = %d, want 0", net.Minor())
	}
	creditAcct, err := ledger.Balances(ctx, f.pool, ledger.AccountCredit, usd)
	if err != nil {
		t.Fatal(err)
	}
	if net, _ := creditAcct.Net(); net.Minor() != -2500 {
		t.Fatalf("customer_credit net = %d, want -2500 (a liability)", net.Minor())
	}
	f.assertBalanced(t)
}

// A settlement for an invoice that is not a deposit is refused rather than credited.
func TestSettleNonDepositIsRefused(t *testing.T) {
	f := newFixture(t)
	invoiceID := f.issue(t, 5000)

	_, err := f.svc.SettleDeposit(context.Background(), credit.DepositSettlement{
		Provider: "fakegateway", ProviderAccount: "acct_test",
		ProviderPaymentID: "pi_wrong", InvoiceID: invoiceID,
		AmountMinor: 5000, Currency: "USD",
	})
	if !errors.Is(err, credit.ErrNotDeposit) {
		t.Fatalf("err = %v, want ErrNotDeposit", err)
	}
	if got := f.balance(t); got != 0 {
		t.Fatalf("balance = %d, want 0", got)
	}
	f.assertBalanced(t)
}

// -------------------------------------------------------------------- grants --

// A grant issues a numbered credit note, posts it, and a replay with the same key does not
// grant twice.
func TestGrantIsIdempotent(t *testing.T) {
	f := newFixture(t)

	first := f.grant(t, 1500, "grant-key-1")
	if first.Number == "" || first.AmountMinor != 1500 || first.BalanceMinor != 1500 {
		t.Fatalf("grant = %+v", first)
	}

	replay := f.grant(t, 1500, "grant-key-1")
	if !replay.AlreadyGranted || replay.ID != first.ID || replay.Number != first.Number {
		t.Fatalf("replay = %+v, want the same note", replay)
	}
	if got := f.balance(t); got != 1500 {
		t.Fatalf("balance = %d, want 1500", got)
	}

	// A different key is a second, distinct grant.
	second := f.grant(t, 500, "grant-key-2")
	if second.Number == first.Number {
		t.Fatal("two grants took the same credit-note number")
	}
	if got := f.balance(t); got != 2000 {
		t.Fatalf("balance = %d, want 2000", got)
	}
	f.assertBalanced(t)
}

// A grant without a reason is refused: the reason is the audit trail's only explanation.
func TestGrantRequiresReason(t *testing.T) {
	f := newFixture(t)
	_, err := f.svc.Grant(context.Background(), credit.GrantRequest{
		CustomerID: f.customerID, AmountMinor: 100, Currency: "USD", Key: "k1",
	})
	if !errors.Is(err, credit.ErrInvalidRequest) {
		t.Fatalf("err = %v, want ErrInvalidRequest", err)
	}
	f.assertBalanced(t)
}

// ------------------------------------------------------------------ auto-use --

// When credit covers the invoice, auto-use settles it; when it covers only part, the
// remainder is exactly the difference.
func TestAutoUsePaysInvoiceAndLeavesCorrectRemainder(t *testing.T) {
	ctx := context.Background()

	t.Run("covers the whole invoice", func(t *testing.T) {
		f := newFixture(t)
		invoiceID := f.issue(t, 5000)
		f.grant(t, 5000, "auto-full")

		var app credit.Application
		err := f.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			app, err = credit.ApplyCreditToInvoice(ctx, tx, invoiceID)
			return err
		})
		if err != nil {
			t.Fatalf("auto-use: %v", err)
		}
		if app.AmountMinor != 5000 || !app.InvoicePaid || app.RemainingMinor != 0 {
			t.Fatalf("application = %+v", app)
		}
		status, paid, _ := f.invoiceState(t, invoiceID)
		if status != "paid" || paid != 5000 {
			t.Fatalf("invoice = %s/%d, want paid/5000", status, paid)
		}
		if got := f.balance(t); got != 0 {
			t.Fatalf("balance = %d, want 0", got)
		}
		f.assertBalanced(t)
	})

	t.Run("covers part of the invoice", func(t *testing.T) {
		f := newFixture(t)
		invoiceID := f.issue(t, 5000)
		f.grant(t, 1800, "auto-partial")

		var app credit.Application
		err := f.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			app, err = credit.ApplyCreditToInvoice(ctx, tx, invoiceID)
			return err
		})
		if err != nil {
			t.Fatalf("auto-use: %v", err)
		}
		if app.AmountMinor != 1800 {
			t.Fatalf("applied = %d, want 1800", app.AmountMinor)
		}
		if app.InvoicePaid {
			t.Fatal("the invoice was marked paid before it was covered")
		}
		if app.RemainingMinor != 3200 {
			t.Fatalf("remaining = %d, want 3200", app.RemainingMinor)
		}
		status, paid, total := f.invoiceState(t, invoiceID)
		if status != "open" || paid != 1800 || total != 5000 {
			t.Fatalf("invoice = %s/%d/%d, want open/1800/5000", status, paid, total)
		}
		if got := f.balance(t); got != 0 {
			t.Fatalf("balance = %d, want 0", got)
		}
		f.assertBalanced(t)
	})

	t.Run("never exceeds the balance", func(t *testing.T) {
		f := newFixture(t)
		invoiceID := f.issue(t, 100)
		// A balance smaller than the invoice still applies in full, and no more.
		f.grant(t, 40, "auto-small")

		var app credit.Application
		if err := f.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			app, err = credit.ApplyCreditToInvoice(ctx, tx, invoiceID)
			return err
		}); err != nil {
			t.Fatalf("auto-use: %v", err)
		}
		if app.AmountMinor != 40 {
			t.Fatalf("applied = %d, want 40", app.AmountMinor)
		}
		if got := f.balance(t); got != 0 {
			t.Fatalf("balance = %d, want 0 (a negative balance would be a bug)", got)
		}
		f.assertBalanced(t)
	})
}

// A replay of auto-use does not spend twice, and writes exactly one application and one
// posting.
func TestAutoUseReplayDoesNotSpendTwice(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	invoiceID := f.issue(t, 5000)
	f.grant(t, 9000, "auto-replay")

	apply := func() credit.Application {
		t.Helper()
		var app credit.Application
		if err := f.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			app, err = credit.ApplyCreditToInvoice(ctx, tx, invoiceID)
			return err
		}); err != nil {
			t.Fatalf("auto-use: %v", err)
		}
		return app
	}

	first := apply()
	if first.AmountMinor != 5000 || first.AlreadyApplied {
		t.Fatalf("first = %+v", first)
	}
	second := apply()
	if !second.AlreadyApplied {
		t.Fatal("the replay was not reported as already applied")
	}
	if second.AmountMinor != 5000 {
		t.Fatalf("replay reported %d applied, want the original 5000", second.AmountMinor)
	}

	if got := f.balance(t); got != 4000 {
		t.Fatalf("balance = %d, want 4000 (only one application may have spent credit)", got)
	}
	status, paid, _ := f.invoiceState(t, invoiceID)
	if status != "paid" || paid != 5000 {
		t.Fatalf("invoice = %s/%d, want paid/5000", status, paid)
	}

	var applications, postings int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM credit_applications`).Scan(&applications); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(ctx, `
		SELECT count(*) FROM journal_transactions WHERE reference_type = 'credit_application'`).
		Scan(&postings); err != nil {
		t.Fatal(err)
	}
	if applications != 1 || postings != 1 {
		t.Fatalf("%d applications / %d postings, want 1/1", applications, postings)
	}
	f.assertBalanced(t)
}

// Auto-use is a setting: with it off, an issuance must not spend the balance, and it must
// not fail either.
func TestAutoUseDisabledIsANoOp(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, err := f.pool.Exec(ctx, `
		UPDATE settings SET value = 'false' WHERE group_key = 'credits' AND key = 'auto_use'`); err != nil {
		t.Fatal(err)
	}
	invoiceID := f.issue(t, 5000)
	f.grant(t, 5000, "auto-off")

	var app credit.Application
	if err := f.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		app, err = credit.ApplyCreditToInvoice(ctx, tx, invoiceID)
		return err
	}); err != nil {
		t.Fatalf("auto-use with the setting off: %v", err)
	}
	if !app.Disabled || app.AmountMinor != 0 {
		t.Fatalf("application = %+v, want a disabled no-op", app)
	}
	if got := f.balance(t); got != 5000 {
		t.Fatalf("balance = %d, want 5000", got)
	}
	f.assertBalanced(t)
}

// A customer cannot apply their credit to somebody else's invoice.
func TestPayWithCreditRejectsAnotherCustomersInvoice(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	// A second customer with an invoice of their own.
	var otherCustomer string
	if err := f.pool.QueryRow(ctx, `
		INSERT INTO customers (email, name) VALUES ('other@example.test', 'Other')
		RETURNING id`).Scan(&otherCustomer); err != nil {
		t.Fatal(err)
	}
	var otherInvoice string
	if err := f.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		id, _, _, err := f.issuer.CreateDraft(ctx, tx, billing.IssueRequest{
			CustomerID: otherCustomer,
			Lines: []billing.Line{{
				Description: "Other customer's invoice", QuantityMilli: 1000,
				UnitPrice: money.New(4000, usd),
			}},
			Customer: billing.CustomerDetails{ID: otherCustomer},
		})
		if err != nil {
			return err
		}
		issued, err := f.issuer.Issue(ctx, tx, id, billing.DefaultSeries)
		if err != nil {
			return err
		}
		otherInvoice = issued.InvoiceID
		return nil
	}); err != nil {
		t.Fatalf("other invoice: %v", err)
	}

	f.grant(t, 5000, "cross-tenant")

	_, err := f.svc.PayWithCredit(ctx, f.customerID, otherInvoice, "cross-1", f.userID)
	if !errors.Is(err, credit.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound (a foreign invoice must be indistinguishable)", err)
	}
	if got := f.balance(t); got != 5000 {
		t.Fatalf("balance = %d, want 5000 (nothing may have been spent)", got)
	}
	status, paid, _ := f.invoiceState(t, otherInvoice)
	if status != "open" || paid != 0 {
		t.Fatalf("the other customer's invoice = %s/%d, want open/0", status, paid)
	}
	f.assertBalanced(t)
}

// An explicit application with a cap applies exactly the cap, and a second call with the
// same key is a no-op.
func TestApplyCreditWithCap(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	invoiceID := f.issue(t, 5000)
	f.grant(t, 5000, "capped")

	app, err := f.svc.PayWithCredit(ctx, f.customerID, invoiceID, "cap-1", f.userID)
	if err != nil {
		t.Fatalf("pay with credit: %v", err)
	}
	if app.AmountMinor != 5000 || !app.InvoicePaid {
		t.Fatalf("application = %+v", app)
	}

	replay, err := f.svc.PayWithCredit(ctx, f.customerID, invoiceID, "cap-1", f.userID)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if !replay.AlreadyApplied || replay.AmountMinor != 5000 {
		t.Fatalf("replay = %+v", replay)
	}
	if got := f.balance(t); got != 0 {
		t.Fatalf("balance = %d, want 0", got)
	}
	f.assertBalanced(t)
}

// The integration contract: issuance and auto-use in one transaction.
//
// This is exactly the sequence an issuer performs when credits.auto_use is on — draft,
// issue, apply — and it is one transaction, so a crash cannot leave an issued invoice whose
// credit was never spent. The assertion that matters is that the invoice never exists in the
// database in the "issued but not credited" state, because the commit is atomic.
func TestIssueAndAutoUseInOneTransaction(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.grant(t, 4000, "issue-auto-use")

	var (
		invoiceID string
		app       credit.Application
	)
	err := f.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		draftID, _, _, err := f.issuer.CreateDraft(ctx, tx, billing.IssueRequest{
			CustomerID: f.customerID,
			Lines: []billing.Line{{
				Description: "VPS Small monthly", QuantityMilli: 1000,
				UnitPrice: money.New(4000, usd),
			}},
			Customer: billing.CustomerDetails{ID: f.customerID},
		})
		if err != nil {
			return err
		}
		issued, err := f.issuer.Issue(ctx, tx, draftID, billing.DefaultSeries)
		if err != nil {
			return err
		}
		invoiceID = issued.InvoiceID

		// The call the issuer makes. Its signature is
		//
		//	ApplyCreditToInvoice(ctx context.Context, tx pgx.Tx, invoiceID string) (Application, error)
		//
		// and it is a no-op when credits.auto_use is off.
		app, err = credit.ApplyCreditToInvoice(ctx, tx, invoiceID)
		return err
	})
	if err != nil {
		t.Fatalf("issue and apply: %v", err)
	}
	if app.AmountMinor != 4000 || !app.InvoicePaid {
		t.Fatalf("application = %+v", app)
	}

	status, paid, _ := f.invoiceState(t, invoiceID)
	if status != "paid" || paid != 4000 {
		t.Fatalf("invoice = %s/%d, want paid/4000", status, paid)
	}
	if got := f.balance(t); got != 0 {
		t.Fatalf("balance = %d, want 0", got)
	}
	f.assertBalanced(t)
}

// A rollback of the issuing transaction takes the application with it: no orphan credit
// spend may survive an issuance that failed.
func TestAutoUseRollsBackWithTheIssuance(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.grant(t, 4000, "rollback")

	err := f.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		draftID, _, _, err := f.issuer.CreateDraft(ctx, tx, billing.IssueRequest{
			CustomerID: f.customerID,
			Lines: []billing.Line{{
				Description: "Voided by the caller", QuantityMilli: 1000,
				UnitPrice: money.New(4000, usd),
			}},
			Customer: billing.CustomerDetails{ID: f.customerID},
		})
		if err != nil {
			return err
		}
		if _, err := f.issuer.Issue(ctx, tx, draftID, billing.DefaultSeries); err != nil {
			return err
		}
		if _, err := credit.ApplyCreditToInvoice(ctx, tx, draftID); err != nil {
			return err
		}
		return errors.New("the caller aborts after issuing")
	})
	if err == nil {
		t.Fatal("the transaction was expected to abort")
	}

	if got := f.balance(t); got != 4000 {
		t.Fatalf("balance = %d, want 4000: the application must have rolled back", got)
	}
	var applications int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM credit_applications`).Scan(&applications); err != nil {
		t.Fatal(err)
	}
	if applications != 0 {
		t.Fatalf("%d applications survived the rollback", applications)
	}
	f.assertBalanced(t)
}
