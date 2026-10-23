package payments_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/catalyst/billing/internal/billing"
	"github.com/catalyst/billing/internal/dunning"
	"github.com/catalyst/billing/internal/fulfilment"
	"github.com/catalyst/billing/internal/money"
	"github.com/catalyst/billing/internal/payments"
	"github.com/catalyst/billing/internal/store"
	"github.com/catalyst/billing/internal/store/storetest"
	"github.com/jackc/pgx/v5"
)

// Billing agreements against the real database (PAY-10, PAY-11).
//
// What these tests are for, in the order the acceptance criteria name them:
//
//   - a method is saved, is replaced rather than duplicated, and belongs to one customer;
//   - saving charges nothing and touches no invoice;
//   - a renewal charges the default method off-session, through the settlement path every
//     other payment uses;
//   - a declined charge leaves the invoice open and enters dunning;
//   - the token is not part of the type the HTTP layer serialises.

// fixtureSeq keeps fixture identifiers unique inside one test binary, since several tests
// build several worlds.
var fixtureSeq atomic.Int64

// env is one customer, one open invoice and a service wired to a fake charger.
type env struct {
	pool       *store.Pool
	svc        *payments.Agreements
	charger    *fakeCharger
	customerID string
	invoiceID  string
	now        time.Time
}

// fakeCharger is the adapter the protocol does not have yet.
//
// It stands in for a real gateway exactly where the protocol gap is: ChargeOffSession is the
// method extproto does not declare. Everything downstream of it — the attempt, the
// settlement, the ledger, the invoice transition — is the real code.
type fakeCharger struct {
	result payments.OffSessionResult
	err    error
	calls  []payments.OffSessionCharge
}

func (f *fakeCharger) Name() string { return "fakegateway" }

func (f *fakeCharger) ChargeOffSession(_ context.Context, req payments.OffSessionCharge) (payments.OffSessionResult, error) {
	f.calls = append(f.calls, req)
	if f.err != nil {
		return payments.OffSessionResult{}, f.err
	}
	return f.result, nil
}

// newEnv creates a customer with one issued, open invoice for 12.00 USD.
func newEnv(t *testing.T) *env {
	t.Helper()
	pool := storetest.DB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	e := &env{pool: pool, now: now, charger: &fakeCharger{}}

	n := fixtureSeq.Add(1)
	email := fmt.Sprintf("agreement+%d@example.test", n)

	issuer := billing.NewIssuer(billing.FixedTaxPricer{
		Rate:     money.MustParseRatio("0%"),
		Rounding: money.RoundHalfUp,
		Policy:   "none",
	})

	err := pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx,
			`INSERT INTO customers (email, name) VALUES ($1, 'Agreement Customer') RETURNING id`,
			email).Scan(&e.customerID); err != nil {
			return err
		}
		invoiceID, _, _, err := issuer.CreateDraft(ctx, tx, billing.IssueRequest{
			CustomerID: e.customerID,
			Lines: []billing.Line{{
				Description:   "VPS, monthly",
				QuantityMilli: 1000,
				UnitPrice:     money.New(1200, usd),
			}},
			Merchant: billing.Merchant{Name: "Catalyst Hosting Ltd", CountryCode: "GB"},
			Customer: billing.CustomerDetails{ID: e.customerID, Email: email},
			Terms:    "Pay within seven days.",
			// Due yesterday, so the invoice is already past due: dunning's own scan then
			// treats a declined charge exactly the way it treats any unpaid renewal.
			DueAt: now.AddDate(0, 0, -1),
		})
		if err != nil {
			return err
		}
		issued, err := issuer.Issue(ctx, tx, invoiceID, billing.DefaultSeries)
		if err != nil {
			return err
		}
		e.invoiceID = issued.InvoiceID
		return nil
	})
	if err != nil {
		t.Fatalf("fixtures: %v", err)
	}

	e.svc = payments.NewAgreements(pool).
		WithCharger(e.charger).
		WithApplier(fulfilment.NewApplier(pool)).
		WithAccount("acct_test")
	e.svc.Now = func() time.Time { return e.now }
	return e
}

// addCustomer creates a second customer in the same database.
//
// It exists because storetest.DB(t) must be called once per test: the harness holds one
// advisory lock per test and its reference count is only released by that test's cleanup, so
// a second call inside the same test would leave the lock held for every later test.
func (e *env) addCustomer(t *testing.T) string {
	t.Helper()
	var id string
	if err := e.pool.QueryRow(context.Background(),
		`INSERT INTO customers (email, name) VALUES ($1, 'Other Customer') RETURNING id`,
		fmt.Sprintf("other+%d@example.test", fixtureSeq.Add(1))).Scan(&id); err != nil {
		t.Fatalf("create second customer: %v", err)
	}
	return id
}

// addCustomerWithInvoice creates a second customer with their own past-due invoice, so a scan
// test can prove that the sweep picks the right rows rather than the only row.
func (e *env) addCustomerWithInvoice(t *testing.T) (customerID, invoiceID string) {
	t.Helper()
	customerID = e.addCustomer(t)
	issuer := billing.NewIssuer(billing.FixedTaxPricer{
		Rate:     money.MustParseRatio("0%"),
		Rounding: money.RoundHalfUp,
		Policy:   "none",
	})
	err := e.pool.InTx(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		id, _, _, err := issuer.CreateDraft(ctx, tx, billing.IssueRequest{
			CustomerID: customerID,
			Lines: []billing.Line{{
				Description:   "VPS, monthly",
				QuantityMilli: 1000,
				UnitPrice:     money.New(900, usd),
			}},
			Merchant: billing.Merchant{Name: "Catalyst Hosting Ltd", CountryCode: "GB"},
			Customer: billing.CustomerDetails{ID: customerID},
			DueAt:    e.now.AddDate(0, 0, -1),
		})
		if err != nil {
			return err
		}
		issued, err := issuer.Issue(ctx, tx, id, billing.DefaultSeries)
		if err != nil {
			return err
		}
		invoiceID = issued.InvoiceID
		return nil
	})
	if err != nil {
		t.Fatalf("second invoice: %v", err)
	}
	return customerID, invoiceID
}

// save stores a method the way the HTTP layer would.
func (e *env) save(t *testing.T, token, brand, last4 string, makeDefault bool) payments.Method {
	t.Helper()
	m, err := e.svc.Save(context.Background(), payments.SaveMethodRequest{
		CustomerID:  e.customerID,
		Provider:    "fakegateway",
		Token:       token,
		Brand:       brand,
		Last4:       last4,
		MakeDefault: makeDefault,
	})
	if err != nil {
		t.Fatalf("save method: %v", err)
	}
	return m
}

// enableOffSession writes the operator's setting the way the settings page would.
func (e *env) enableOffSession(t *testing.T, on bool) {
	t.Helper()
	value := "false"
	if on {
		value = "true"
	}
	if _, err := e.pool.Exec(context.Background(), `
		INSERT INTO settings (group_key, key, value) VALUES ('gateway', 'off_session_enabled', $1)
		ON CONFLICT (group_key, key) DO UPDATE SET value = EXCLUDED.value`, value); err != nil {
		t.Fatalf("set off-session setting: %v", err)
	}
}

// countRows counts rows of a table for a where clause.
func (e *env) countRows(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

// invoiceState is the money and status of one invoice.
type invoiceState struct {
	Status     string
	TotalMinor int64
	PaidMinor  int64
}

func (e *env) invoice(t *testing.T) invoiceState {
	t.Helper()
	var s invoiceState
	if err := e.pool.QueryRow(context.Background(), `
		SELECT status, total_minor, amount_paid_minor FROM invoices WHERE id = $1`,
		e.invoiceID).Scan(&s.Status, &s.TotalMinor, &s.PaidMinor); err != nil {
		t.Fatalf("read invoice: %v", err)
	}
	return s
}

// ---------------------------------------------------------------- saving --

// Saving the same provider token twice updates the row instead of adding a second one.
func TestSavingTheSameTokenReplacesTheMethod(t *testing.T) {
	e := newEnv(t)
	first := e.save(t, "tok_visa_4242", "visa", "4242", false)
	if !first.IsDefault {
		t.Fatal("the first saved method must become the default: a customer with one card and no default is a renewal that silently stops")
	}

	second := e.save(t, "tok_visa_4242", "visa", "4242", true)
	if second.ID != first.ID {
		t.Fatalf("the same token produced two methods: %s and %s", first.ID, second.ID)
	}

	list, err := e.svc.List(context.Background(), e.customerID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("list has %d methods, want 1", len(list))
	}

	var rows int
	if err := e.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM payment_methods WHERE customer_id = $1`, e.customerID).Scan(&rows); err != nil {
		t.Fatalf("count: %v", err)
	}
	if rows != 1 {
		t.Fatalf("payment_methods has %d rows, want 1", rows)
	}
}

// Saving a method is not a charge: no payment, no allocation, and the invoice is untouched.
func TestSavingAMethodChargesNothing(t *testing.T) {
	e := newEnv(t)
	before := e.invoice(t)
	// The fixture issued an invoice, which posts one transaction. What matters is that
	// saving a method posts nothing *more*.
	postingsBefore := e.countRows(t, `SELECT count(*) FROM journal_transactions`)

	e.save(t, "tok_visa_4242", "visa", "4242", true)

	after := e.invoice(t)
	if after != before {
		t.Fatalf("invoice changed from %+v to %+v when a method was saved", before, after)
	}
	if n := e.countRows(t, `SELECT count(*) FROM payments`); n != 0 {
		t.Fatalf("saving a method created %d payment rows", n)
	}
	if n := e.countRows(t, `SELECT count(*) FROM payment_attempts`); n != 0 {
		t.Fatalf("saving a method created %d payment attempts", n)
	}
	if n := e.countRows(t, `SELECT count(*) FROM journal_transactions`); n != postingsBefore {
		t.Fatalf("saving a method posted %d journal transactions, want %d", n, postingsBefore)
	}
}

// One default per customer, and setting a new one clears the old one in the same transaction.
func TestOneDefaultPerCustomer(t *testing.T) {
	e := newEnv(t)
	visa := e.save(t, "tok_visa", "visa", "4242", true)
	mastercard := e.save(t, "tok_mc", "mastercard", "4444", false)

	// The second save did not claim the default, so visa still holds it.
	list, err := e.svc.List(context.Background(), e.customerID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("list has %d methods, want 2", len(list))
	}
	defaults := 0
	for _, m := range list {
		if m.IsDefault {
			defaults++
			if m.ID != visa.ID {
				t.Fatalf("default is %s, want %s", m.ID, visa.ID)
			}
		}
	}
	if defaults != 1 {
		t.Fatalf("%d methods claim to be the default", defaults)
	}

	got, err := e.svc.SetDefault(context.Background(), e.customerID, mastercard.ID)
	if err != nil {
		t.Fatalf("set default: %v", err)
	}
	if !got.IsDefault || got.ID != mastercard.ID {
		t.Fatalf("set default returned %+v", got)
	}

	// The database is the authority: exactly one row is flagged.
	if n := e.countRows(t,
		`SELECT count(*) FROM payment_methods WHERE customer_id = $1 AND is_default`, e.customerID); n != 1 {
		t.Fatalf("%d rows are flagged default, want 1", n)
	}

	def, err := e.svc.Default(context.Background(), e.customerID, "fakegateway")
	if err != nil {
		t.Fatalf("default: %v", err)
	}
	if def.ID != mastercard.ID {
		t.Fatalf("default method is %s, want %s", def.ID, mastercard.ID)
	}
}

// Another customer's method id is not found, and is indistinguishable from an id that does
// not exist.
func TestAnotherCustomersMethodIsNotFound(t *testing.T) {
	e := newEnv(t)
	mine := e.save(t, "tok_visa", "visa", "4242", true)

	// A second customer in the same database: the query is scoped in SQL, so this is the
	// real question rather than a mock of it.
	otherID := e.addCustomer(t)

	if _, err := e.svc.Get(context.Background(), otherID, mine.ID); !errors.Is(err, payments.ErrMethodNotFound) {
		t.Fatalf("Get on another customer's method: %v, want ErrMethodNotFound", err)
	}
	if _, err := e.svc.SetDefault(context.Background(), otherID, mine.ID); !errors.Is(err, payments.ErrMethodNotFound) {
		t.Fatalf("SetDefault on another customer's method: %v, want ErrMethodNotFound", err)
	}
	if err := e.svc.Remove(context.Background(), otherID, mine.ID); !errors.Is(err, payments.ErrMethodNotFound) {
		t.Fatalf("Remove on another customer's method: %v, want ErrMethodNotFound", err)
	}
	// The other customer's list is empty, not a filtered view of someone else's.
	if list, err := e.svc.List(context.Background(), otherID); err != nil {
		t.Fatalf("list for the other customer: %v", err)
	} else if len(list) != 0 {
		t.Fatalf("the other customer sees %d methods", len(list))
	}

	// The row is still there: a refused cross-customer operation must not delete anything.
	if _, err := e.svc.Get(context.Background(), e.customerID, mine.ID); err != nil {
		t.Fatalf("the owner can no longer read their own method: %v", err)
	}
}

// Removing the default does not promote another card: promotion would silently change which
// card an automatic charge uses.
func TestRemovingTheDefaultPromotesNothing(t *testing.T) {
	e := newEnv(t)
	visa := e.save(t, "tok_visa", "visa", "4242", true)
	e.save(t, "tok_mc", "mastercard", "4444", false)

	if err := e.svc.Remove(context.Background(), e.customerID, visa.ID); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if n := e.countRows(t,
		`SELECT count(*) FROM payment_methods WHERE customer_id = $1 AND is_default`, e.customerID); n != 0 {
		t.Fatalf("%d rows became default by promotion, want 0", n)
	}
	if _, err := e.svc.Default(context.Background(), e.customerID, "fakegateway"); !errors.Is(err, payments.ErrNoDefaultMethod) {
		t.Fatalf("Default after removing it: %v, want ErrNoDefaultMethod", err)
	}
}

// -------------------------------------------------------------- charging --

// A renewal charges the default method off-session, and the settlement is the ordinary one.
func TestOffSessionChargeSettlesThroughTheOrdinaryPath(t *testing.T) {
	e := newEnv(t)
	e.enableOffSession(t, true)
	e.save(t, "tok_visa", "visa", "4242", true)

	e.charger.result = payments.OffSessionResult{
		ProviderPaymentID: "pi_offsession_1",
		Status:            "succeeded",
		AmountMinor:       1200,
	}

	out, err := e.svc.ChargeInvoice(context.Background(), e.invoiceID)
	if err != nil {
		t.Fatalf("charge: %v", err)
	}
	if out.Status != payments.ChargeSettled {
		t.Fatalf("status = %s, want settled (%s)", out.Status, out.Detail)
	}

	// The adapter was asked for the invoice's own amount, in minor units, with the token.
	if len(e.charger.calls) != 1 {
		t.Fatalf("%d charger calls, want 1", len(e.charger.calls))
	}
	call := e.charger.calls[0]
	if call.AmountMinor != 1200 || call.Currency != "USD" {
		t.Fatalf("charged %d %s, want 1200 USD", call.AmountMinor, call.Currency)
	}
	if call.ProviderToken != "tok_visa" {
		t.Fatalf("adapter got token %q", call.ProviderToken)
	}

	// The invoice is paid by billing.ApplyPayment, not by a second implementation: the paid
	// amount, the payment row, the allocation and the ledger postings all exist.
	inv := e.invoice(t)
	if inv.Status != "paid" || inv.PaidMinor != 1200 {
		t.Fatalf("invoice = %+v, want paid 1200", inv)
	}
	if n := e.countRows(t, `SELECT count(*) FROM payments WHERE provider_payment_id = $1`,
		"pi_offsession_1"); n != 1 {
		t.Fatalf("%d payment rows for the provider payment, want 1", n)
	}
	if n := e.countRows(t, `SELECT count(*) FROM payment_allocations WHERE invoice_id = $1`,
		e.invoiceID); n != 1 {
		t.Fatalf("%d allocations, want 1", n)
	}
	// Debit cash clearing, credit receivable: the posting every settled payment makes.
	if n := e.countRows(t, `
		SELECT count(*) FROM journal_entries je
		JOIN journal_transactions jt ON jt.id = je.transaction_id
		WHERE jt.kind = 'payment_received' AND je.account_code = 'cash_clearing'
		  AND je.debit_minor = 1200`); n != 1 {
		t.Fatalf("%d cash-clearing debits of 1200, want 1", n)
	}

	// The method records that it was used.
	m, err := e.svc.Default(context.Background(), e.customerID, "fakegateway")
	if err != nil {
		t.Fatalf("default: %v", err)
	}
	if m.LastUsedAt == nil {
		t.Fatal("the method has no last_used_at after a successful charge")
	}

	// A second call finds the invoice settled and does nothing: no second charge.
	again, err := e.svc.ChargeInvoice(context.Background(), e.invoiceID)
	if err != nil {
		t.Fatalf("second charge: %v", err)
	}
	if again.Status != payments.ChargeSkipped {
		t.Fatalf("second charge status = %s, want skipped", again.Status)
	}
	if len(e.charger.calls) != 1 {
		t.Fatalf("the adapter was called %d times, want 1", len(e.charger.calls))
	}
}

// A declined charge leaves the invoice open, records the failure, and enters dunning rather
// than crashing or being swallowed.
func TestDeclinedChargeLeavesTheInvoiceOpenAndEntersDunning(t *testing.T) {
	e := newEnv(t)
	e.enableOffSession(t, true)
	e.save(t, "tok_declined", "visa", "4000", true)

	e.charger.result = payments.OffSessionResult{
		Status:         "failed",
		FailureCode:    "card_declined",
		FailureMessage: "Your card was declined.",
	}

	out, err := e.svc.ChargeInvoice(context.Background(), e.invoiceID)
	var declined *payments.DeclinedError
	if !errors.As(err, &declined) {
		t.Fatalf("charge error = %v, want a *DeclinedError", err)
	}
	if !errors.Is(err, payments.ErrDeclined) {
		t.Fatalf("the decline does not match ErrDeclined: %v", err)
	}
	if declined.Code != "card_declined" {
		t.Fatalf("decline code = %q, want card_declined", declined.Code)
	}
	if out.Status != payments.ChargeDeclined {
		t.Fatalf("status = %s, want declined", out.Status)
	}

	// The invoice is untouched: still open, still owed, no money recorded.
	inv := e.invoice(t)
	if inv.Status != "open" || inv.PaidMinor != 0 {
		t.Fatalf("invoice = %+v, want open and unpaid", inv)
	}
	if n := e.countRows(t, `SELECT count(*) FROM payments`); n != 0 {
		t.Fatalf("a declined charge recorded %d payments", n)
	}
	if n := e.countRows(t, `SELECT count(*) FROM payment_allocations`); n != 0 {
		t.Fatalf("a declined charge recorded %d allocations", n)
	}

	// The failure is visible on the method, so the customer's screen can say which card was
	// refused.
	var failureCode string
	var failedAt *time.Time
	if err := e.pool.QueryRow(context.Background(), `
		SELECT COALESCE(last_failure_code, ''), last_failure_at
		FROM payment_methods WHERE customer_id = $1 AND is_default`, e.customerID).
		Scan(&failureCode, &failedAt); err != nil {
		t.Fatalf("read method failure: %v", err)
	}
	if failureCode != "card_declined" || failedAt == nil {
		t.Fatalf("method failure = %q at %v, want card_declined with a timestamp", failureCode, failedAt)
	}

	// Dunning picks the invoice up the way it picks up any unpaid renewal.
	engine := dunning.NewEngine(e.pool, dunning.DefaultPolicy())
	engine.Now = func() time.Time { return e.now }
	results, err := engine.Advance(context.Background(), 100)
	if err != nil {
		t.Fatalf("dunning advance: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("dunning advanced %d invoices, want 1", len(results))
	}
	if results[0].InvoiceID != e.invoiceID {
		t.Fatalf("dunning advanced invoice %s, want %s", results[0].InvoiceID, e.invoiceID)
	}
	if results[0].State != dunning.StateOverdue {
		t.Fatalf("dunning state = %s, want overdue", results[0].State)
	}

	var state string
	if err := e.pool.QueryRow(context.Background(),
		`SELECT state FROM dunning_states WHERE invoice_id = $1`, e.invoiceID).Scan(&state); err != nil {
		t.Fatalf("read dunning state: %v", err)
	}
	if state != string(dunning.StateOverdue) {
		t.Fatalf("stored dunning state = %s, want overdue", state)
	}

	// A second attempt does not re-charge a card the provider just refused: that is how a
	// customer's card gets blocked by their bank.
	if _, err := e.svc.ChargeInvoice(context.Background(), e.invoiceID); err == nil {
		t.Fatal("a second charge on a declined invoice succeeded")
	}
	if len(e.charger.calls) != 1 {
		t.Fatalf("the adapter was called %d times after a decline, want 1", len(e.charger.calls))
	}
}

// The operator's setting is a guard, not a hint: with it off nothing is charged.
func TestOffSessionChargeRefusesWhenDisabled(t *testing.T) {
	e := newEnv(t)
	e.enableOffSession(t, false)
	e.save(t, "tok_visa", "visa", "4242", true)
	e.charger.result = payments.OffSessionResult{ProviderPaymentID: "pi_1", Status: "succeeded", AmountMinor: 1200}

	if _, err := e.svc.ChargeInvoice(context.Background(), e.invoiceID); !errors.Is(err, payments.ErrOffSessionDisabled) {
		t.Fatalf("charge with the setting off: %v, want ErrOffSessionDisabled", err)
	}
	if len(e.charger.calls) != 0 {
		t.Fatalf("the adapter was called %d times with the setting off", len(e.charger.calls))
	}
	if n := e.countRows(t, `SELECT count(*) FROM payment_attempts`); n != 0 {
		t.Fatalf("a disabled charge left %d attempts behind", n)
	}
	if inv := e.invoice(t); inv.Status != "open" || inv.PaidMinor != 0 {
		t.Fatalf("invoice = %+v after a disabled charge", inv)
	}

	ok, reason := e.svc.CanChargeOffSession(context.Background())
	if ok {
		t.Fatalf("CanChargeOffSession = true with the setting off (%s)", reason)
	}
}

// Without an adapter that can charge, the code refuses with a stable error and leaves no
// trace: it never pretends the renewal was paid.
func TestOffSessionChargeRefusesWithoutAnAdapter(t *testing.T) {
	e := newEnv(t)
	e.enableOffSession(t, true)
	e.save(t, "tok_visa", "visa", "4242", true)

	// The service as an installation whose gateway cannot charge: no charger attached.
	bare := payments.NewAgreements(e.pool).
		WithApplier(fulfilment.NewApplier(e.pool)).
		WithAccount("acct_test")
	bare.Now = func() time.Time { return e.now }

	if _, err := bare.ChargeInvoice(context.Background(), e.invoiceID); !errors.Is(err, payments.ErrOffSessionUnsupported) {
		t.Fatalf("charge without an adapter: %v, want ErrOffSessionUnsupported", err)
	}
	if ok, _ := bare.CanChargeOffSession(context.Background()); ok {
		t.Fatal("CanChargeOffSession = true without an adapter")
	}
	if n := e.countRows(t, `SELECT count(*) FROM payment_attempts`); n != 0 {
		t.Fatalf("a refused charge left %d attempts behind", n)
	}
	if inv := e.invoice(t); inv.Status != "open" {
		t.Fatalf("invoice status = %s after a refused charge, want open", inv.Status)
	}
}

// A charge the provider accepts but has not settled is recorded as pending and settled later
// by the webhook, using the recorded attempt.
func TestPendingChargeIsRecordedNotSettled(t *testing.T) {
	e := newEnv(t)
	e.enableOffSession(t, true)
	e.save(t, "tok_visa", "visa", "4242", true)
	e.charger.result = payments.OffSessionResult{
		ProviderPaymentID: "pi_pending_1",
		Status:            "pending",
	}

	out, err := e.svc.ChargeInvoice(context.Background(), e.invoiceID)
	if err != nil {
		t.Fatalf("charge: %v", err)
	}
	if out.Status != payments.ChargePending {
		t.Fatalf("status = %s, want pending", out.Status)
	}
	if inv := e.invoice(t); inv.Status != "open" || inv.PaidMinor != 0 {
		t.Fatalf("invoice = %+v after a pending charge, want open and unpaid", inv)
	}
	var status, ref string
	if err := e.pool.QueryRow(context.Background(), `
		SELECT status, COALESCE(provider_ref, '') FROM payment_attempts WHERE invoice_id = $1`,
		e.invoiceID).Scan(&status, &ref); err != nil {
		t.Fatalf("read attempt: %v", err)
	}
	if status != "pending" || ref != "pi_pending_1" {
		t.Fatalf("attempt = %s/%s, want pending/pi_pending_1", status, ref)
	}
}

// A charge that the provider settles for a different amount is refused rather than applied:
// a mismatch is an anomaly, not a partial payment.
func TestChargeForTheWrongAmountIsRefused(t *testing.T) {
	e := newEnv(t)
	e.enableOffSession(t, true)
	e.save(t, "tok_visa", "visa", "4242", true)
	e.charger.result = payments.OffSessionResult{
		ProviderPaymentID: "pi_wrong",
		Status:            "succeeded",
		AmountMinor:       999,
	}

	if _, err := e.svc.ChargeInvoice(context.Background(), e.invoiceID); !errors.Is(err, payments.ErrAmountMismatch) {
		t.Fatalf("charge: %v, want ErrAmountMismatch", err)
	}
	if inv := e.invoice(t); inv.Status != "open" || inv.PaidMinor != 0 {
		t.Fatalf("invoice = %+v after a mismatched charge", inv)
	}
	if n := e.countRows(t, `SELECT count(*) FROM payments`); n != 0 {
		t.Fatalf("a mismatched charge recorded %d payments", n)
	}
}

// A customer with no default method is skipped, not failed: nothing is charged and nothing is
// recorded.
func TestChargeWithoutADefaultIsSkipped(t *testing.T) {
	e := newEnv(t)
	e.enableOffSession(t, true)

	out, err := e.svc.ChargeInvoice(context.Background(), e.invoiceID)
	if err != nil {
		t.Fatalf("charge: %v", err)
	}
	if out.Status != payments.ChargeSkipped {
		t.Fatalf("status = %s, want skipped", out.Status)
	}
	if len(e.charger.calls) != 0 {
		t.Fatalf("the adapter was called %d times with no saved method", len(e.charger.calls))
	}
}

// The sweep charges exactly the customers who have a default method, and reports one outcome
// per invoice it considered.
func TestChargeDueSweepsOnlyCustomersWithADefault(t *testing.T) {
	e := newEnv(t)
	e.enableOffSession(t, true)
	e.save(t, "tok_visa", "visa", "4242", true)
	e.charger.result = payments.OffSessionResult{
		ProviderPaymentID: "pi_sweep_1", Status: "succeeded", AmountMinor: 1200,
	}

	// A second customer, past due, with no saved method: the sweep must skip them rather than
	// failing the run.
	otherCustomer, otherInvoice := e.addCustomerWithInvoice(t)

	outcomes, err := e.svc.ChargeDue(context.Background(), 50)
	if err != nil {
		t.Fatalf("charge due: %v", err)
	}
	if len(outcomes) != 1 {
		t.Fatalf("%d outcomes, want 1 (only the customer with a default)", len(outcomes))
	}
	if outcomes[0].InvoiceID != e.invoiceID || outcomes[0].Status != payments.ChargeSettled {
		t.Fatalf("outcome = %+v, want the first invoice settled", outcomes[0])
	}
	if inv := e.invoice(t); inv.Status != "paid" {
		t.Fatalf("the swept invoice is %s, want paid", inv.Status)
	}

	// The other customer's invoice is untouched: no default method means no charge.
	var status string
	var paid int64
	if err := e.pool.QueryRow(context.Background(),
		`SELECT status, amount_paid_minor FROM invoices WHERE id = $1`, otherInvoice).
		Scan(&status, &paid); err != nil {
		t.Fatalf("read second invoice: %v", err)
	}
	if status != "open" || paid != 0 {
		t.Fatalf("the skipped invoice is %s/%d, want open/0", status, paid)
	}
	var methods int
	if err := e.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM payment_methods WHERE customer_id = $1`, otherCustomer).Scan(&methods); err != nil {
		t.Fatalf("count methods: %v", err)
	}
	if methods != 0 {
		t.Fatalf("the other customer has %d methods", methods)
	}
}

// An invoice with a checkout session in flight is not charged off-session: the customer is on
// the provider's page, and two paths charging the same invoice is two charges.
func TestChargeDueSkipsAnInvoiceWithACheckoutInFlight(t *testing.T) {
	e := newEnv(t)
	e.enableOffSession(t, true)
	e.save(t, "tok_visa", "visa", "4242", true)
	e.charger.result = payments.OffSessionResult{
		ProviderPaymentID: "pi_should_not_happen", Status: "succeeded", AmountMinor: 1200,
	}

	// The checkout the customer started a minute ago.
	if _, err := e.pool.Exec(context.Background(), `
		INSERT INTO payment_attempts (invoice_id, gateway, operation_key, status, amount_minor, currency)
		VALUES ($1, 'fakegateway', $2, 'pending', 1200, 'USD')`,
		e.invoiceID, "checkout:"+e.invoiceID); err != nil {
		t.Fatalf("seed checkout attempt: %v", err)
	}

	outcomes, err := e.svc.ChargeDue(context.Background(), 50)
	if err != nil {
		t.Fatalf("charge due: %v", err)
	}
	if len(outcomes) != 0 {
		t.Fatalf("%d outcomes, want none while a checkout is in flight", len(outcomes))
	}
	if len(e.charger.calls) != 0 {
		t.Fatalf("the adapter was called %d times while a checkout was in flight", len(e.charger.calls))
	}
	if inv := e.invoice(t); inv.Status != "open" {
		t.Fatalf("invoice status = %s, want open", inv.Status)
	}
}

// Deleting the customer deletes the stored credential. A purge must not leave a token behind,
// and it must not fail because one table was forgotten.
func TestDeletingTheCustomerRemovesTheMethod(t *testing.T) {
	e := newEnv(t)
	// A customer with nothing but a saved method: the invoice fixture would otherwise block the
	// delete on its own foreign key, and the question here is only about this table.
	customerID := e.addCustomer(t)
	if _, err := e.svc.Save(context.Background(), payments.SaveMethodRequest{
		CustomerID: customerID, Provider: "fakegateway", Token: "tok_purge", Last4: "4242",
	}); err != nil {
		t.Fatalf("save: %v", err)
	}

	if _, err := e.pool.Exec(context.Background(), `DELETE FROM customers WHERE id = $1`,
		customerID); err != nil {
		t.Fatalf("delete customer: %v", err)
	}

	var left int
	if err := e.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM payment_methods WHERE customer_id = $1`, customerID).Scan(&left); err != nil {
		t.Fatalf("count: %v", err)
	}
	if left != 0 {
		t.Fatalf("%d saved methods survived their customer", left)
	}
}

// The token is not part of the type's serialised shape. The HTTP layer builds its own body,
// and its test asserts on the bytes; this asserts the cheaper, stronger property that there is
// no field to serialise.
func TestMethodHasNoSerialisableToken(t *testing.T) {
	e := newEnv(t)
	m := e.save(t, "tok_secret_value", "visa", "4242", true)

	if m.Token() != "tok_secret_value" {
		t.Fatalf("Token() = %q", m.Token())
	}
	// `json.Marshal` on Method must not contain the token, because the field is unexported.
	// It also must not panic: a Method marshalled by accident is a leak, and this is the
	// cheapest place to notice.
	blob, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(blob), "tok_secret_value") {
		t.Fatalf("Method serialises its provider token: %s", blob)
	}
}

// -------------------------------------------------------------- agreements --

// Saving a method records the mandate and the evidence around it. A token without this row
// is exactly the state the feature exists to prevent: a card the server can charge but
// cannot explain.
func TestSavingAMethodRecordsItsMandate(t *testing.T) {
	e := newEnv(t)
	consent := e.now.Add(-time.Minute)
	m, err := e.svc.Save(context.Background(), payments.SaveMethodRequest{
		CustomerID:       e.customerID,
		Provider:         "fakegateway",
		Token:            "tok_mandate_4242",
		Brand:            "visa",
		Last4:            "4242",
		MakeDefault:      true,
		ConsentAt:        &consent,
		ConsentIP:        "203.0.113.9",
		ConsentUserAgent: "test-browser/1",
		TermsVersion:     "recurring-2026-01",
	})
	if err != nil {
		t.Fatalf("save: %v", err)
	}

	agreements, err := e.svc.ListAgreements(context.Background(), e.customerID)
	if err != nil {
		t.Fatalf("list agreements: %v", err)
	}
	if len(agreements) != 1 {
		t.Fatalf("%d agreements, want 1", len(agreements))
	}
	ag := agreements[0]
	if ag.Status != "active" || ag.PaymentMethodID != m.ID || ag.Provider != "fakegateway" {
		t.Fatalf("agreement = %+v", ag)
	}
	if ag.ConsentAt == nil || ag.ConsentAt.Sub(consent).Abs() > time.Second {
		t.Fatalf("consent_at = %v, want %v", ag.ConsentAt, consent)
	}
	if ag.ConsentIP != "203.0.113.9" || ag.TermsVersion != "recurring-2026-01" {
		t.Fatalf("consent evidence = %+v", ag)
	}

	// The mandate row must not become a second copy of the credential.
	var text string
	if err := e.pool.QueryRow(context.Background(), `
		SELECT coalesce(provider_agreement_id, '') || '|' || coalesce(provider_customer_id, '')
		FROM billing_agreements WHERE id = $1`, ag.ID).Scan(&text); err != nil {
		t.Fatalf("read agreement references: %v", err)
	}
	if strings.Contains(text, "tok_mandate_4242") {
		t.Fatalf("the mandate row contains the provider token: %q", text)
	}
}

// A cancelled mandate is not permission: the charge is refused before the adapter is called.
func TestChargeRequiresAnActiveAgreement(t *testing.T) {
	e := newEnv(t)
	e.enableOffSession(t, true)
	e.save(t, "tok_visa", "visa", "4242", true)
	e.charger.result = payments.OffSessionResult{
		ProviderPaymentID: "pi_should_not_happen", Status: "succeeded", AmountMinor: 1200,
	}

	agreements, err := e.svc.ListAgreements(context.Background(), e.customerID)
	if err != nil || len(agreements) != 1 {
		t.Fatalf("agreements = %+v, err %v", agreements, err)
	}
	if err := e.svc.CancelAgreement(context.Background(), e.customerID, agreements[0].ID); err != nil {
		t.Fatalf("cancel agreement: %v", err)
	}

	if _, err := e.svc.ChargeInvoice(context.Background(), e.invoiceID); !errors.Is(err, payments.ErrNoAgreement) {
		t.Fatalf("charge with a cancelled mandate: %v, want ErrNoAgreement", err)
	}
	if len(e.charger.calls) != 0 {
		t.Fatalf("the adapter was called %d times without a mandate", len(e.charger.calls))
	}
	if n := e.countRows(t, `SELECT count(*) FROM payment_attempts`); n != 0 {
		t.Fatalf("a refused charge left %d attempts", n)
	}
}

// The provider's fee is recorded on the payment row; it is not posted to the ledger and it
// does not change the amount the customer paid.
func TestOffSessionChargeRecordsTheProviderFee(t *testing.T) {
	e := newEnv(t)
	e.enableOffSession(t, true)
	e.save(t, "tok_visa", "visa", "4242", true)
	e.charger.result = payments.OffSessionResult{
		ProviderPaymentID: "pi_fee_1", Status: "succeeded", AmountMinor: 1200, FeeMinor: 59,
	}
	if _, err := e.svc.ChargeInvoice(context.Background(), e.invoiceID); err != nil {
		t.Fatalf("charge: %v", err)
	}
	var fee int64
	var methodID string
	if err := e.pool.QueryRow(context.Background(), `
		SELECT fee_minor, COALESCE(payment_method_id::text, '')
		FROM payments WHERE provider_payment_id = 'pi_fee_1'`).Scan(&fee, &methodID); err != nil {
		t.Fatalf("read payment: %v", err)
	}
	if fee != 59 {
		t.Fatalf("fee_minor = %d, want 59", fee)
	}
	if methodID == "" {
		t.Fatal("the payment has no saved-method reference")
	}
	if inv := e.invoice(t); inv.PaidMinor != 1200 || inv.Status != "paid" {
		t.Fatalf("invoice = %+v, want paid 1200 (the fee must not reduce the paid amount)", inv)
	}
}
