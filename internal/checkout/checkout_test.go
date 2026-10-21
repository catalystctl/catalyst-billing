package checkout_test

import (
	"context"
	"errors"
	"testing"

	"github.com/catalyst/billing/internal/billing"
	"github.com/catalyst/billing/internal/checkout"
	"github.com/catalyst/billing/internal/extproto"
	"github.com/catalyst/billing/internal/money"
	"github.com/catalyst/billing/internal/payments"
	"github.com/catalyst/billing/internal/store"
	"github.com/catalyst/billing/internal/store/storetest"
	"github.com/jackc/pgx/v5"
)

var usd = money.MustLookupCurrency("USD")

// fakeGateway records what it was asked for and returns deterministic sessions.
type fakeGateway struct {
	calls []extproto.CreateCheckoutParams
	// amount overrides the amount the gateway reports as settled.
	amount *extproto.Amount
	// settled is the state query_payment reports.
	settled extproto.PaymentStatus
	// invoiceID is echoed back in the payment metadata, as a real gateway does.
	invoiceID string
	fail      error
	seq       int
}

func (f *fakeGateway) Name() string { return "fakegateway" }

func (f *fakeGateway) CreateCheckout(_ context.Context, p extproto.CreateCheckoutParams) (extproto.CreateCheckoutResult, error) {
	f.calls = append(f.calls, p)
	if f.fail != nil {
		return extproto.CreateCheckoutResult{}, f.fail
	}
	f.seq++
	return extproto.CreateCheckoutResult{
		PaymentID:   "pay_" + p.OperationKey,
		CheckoutURL: "https://pay.example.test/session",
		Status:      extproto.PaymentPending,
	}, nil
}

func (f *fakeGateway) QueryPayment(_ context.Context, paymentID string) (extproto.QueryPaymentResult, error) {
	amt := extproto.Amount{Minor: 1200, Currency: "USD"}
	if f.amount != nil {
		amt = *f.amount
	}
	status := f.settled
	if status == "" {
		status = extproto.PaymentSucceeded
	}
	return extproto.QueryPaymentResult{
		PaymentID: paymentID,
		Status:    status,
		Amount:    amt,
		Metadata:  map[string]string{"invoice_id": f.invoiceID},
	}, nil
}

type fixture struct {
	pool       *store.Pool
	customerID string
	invoiceID  string
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	pool := storetest.DB(t)
	ctx := context.Background()
	var f fixture
	f.pool = pool
	err := pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx,
			`INSERT INTO customers (email, name) VALUES ('c@example.test', 'C') RETURNING id`,
		).Scan(&f.customerID); err != nil {
			return err
		}
		issuer := billing.NewIssuer(billing.FixedTaxPricer{
			Rate: money.MustParseRatio("0%"), Rounding: money.RoundHalfUp, Policy: "none",
		})
		id, _, _, err := issuer.CreateDraft(ctx, tx, billing.IssueRequest{
			CustomerID: f.customerID,
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
		f.invoiceID = issued.InvoiceID
		return nil
	})
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	return f
}

func TestStartRecordsAttemptWithExactAmount(t *testing.T) {
	f := newFixture(t)
	gw := &fakeGateway{}
	svc := checkout.New(f.pool, gw, "acct", "https://ok", "https://cancel")

	session, err := svc.Start(context.Background(), f.invoiceID)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if session.URL == "" {
		t.Fatal("no redirect url")
	}
	if got, want := session.Amount.Minor(), int64(1200); got != want {
		t.Fatalf("amount = %d, want %d", got, want)
	}
	if len(gw.calls) != 1 {
		t.Fatalf("%d gateway calls, want 1", len(gw.calls))
	}
	if got := gw.calls[0].Metadata["invoice_id"]; got != f.invoiceID {
		t.Fatalf("metadata invoice = %q, want %q", got, f.invoiceID)
	}
	if got := gw.calls[0].OperationKey; got != checkout.OperationKey(f.invoiceID) {
		t.Fatalf("operation key = %q, want %q", got, checkout.OperationKey(f.invoiceID))
	}

	// The attempt is persisted with the provider reference, which is what amount
	// verification later looks up.
	invoiceID, minor, currency, found, err := checkout.AttemptForPayment(
		context.Background(), f.pool, "fakegateway", "pay_"+checkout.OperationKey(f.invoiceID))
	if err != nil {
		t.Fatal(err)
	}
	if !found || invoiceID != f.invoiceID || minor != 1200 || currency != "USD" {
		t.Fatalf("attempt lookup = (%q,%d,%q,%v), want (%q,1200,USD,true)",
			invoiceID, minor, currency, found, f.invoiceID)
	}
}

// Starting a second checkout for the same invoice must not open a second gateway
// session: that is how customers get charged twice.
func TestStartReusesSession(t *testing.T) {
	f := newFixture(t)
	gw := &fakeGateway{}
	svc := checkout.New(f.pool, gw, "acct", "", "")

	first, err := svc.Start(context.Background(), f.invoiceID)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	second, err := svc.Start(context.Background(), f.invoiceID)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if len(gw.calls) != 1 {
		t.Fatalf("%d gateway sessions created, want 1", len(gw.calls))
	}
	if !second.Reused {
		t.Fatal("second checkout was not reported as reused")
	}
	if first.AttemptID != second.AttemptID {
		t.Fatalf("different attempts: %s vs %s", first.AttemptID, second.AttemptID)
	}
}

func TestStartRefusesSettledInvoice(t *testing.T) {
	f := newFixture(t)
	gw := &fakeGateway{}
	svc := checkout.New(f.pool, gw, "acct", "", "")

	ctx := context.Background()
	err := f.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := billing.ApplyPayment(ctx, tx, billing.AllocationRequest{
			Provider:          "fakegateway",
			ProviderAccount:   "acct",
			ProviderPaymentID: "pi_full",
			InvoiceID:         f.invoiceID,
			Amount:            money.New(1200, usd),
		})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = svc.Start(ctx, f.invoiceID)
	if !errors.Is(err, checkout.ErrInvoiceSettled) {
		t.Fatalf("got %v, want ErrInvoiceSettled", err)
	}
	if len(gw.calls) != 0 {
		t.Fatalf("%d gateway calls for a settled invoice, want 0", len(gw.calls))
	}
}

// A draft invoice is not payable: issuing it is a separate decision.
func TestStartRefusesDraft(t *testing.T) {
	f := newFixture(t)
	gw := &fakeGateway{}
	svc := checkout.New(f.pool, gw, "acct", "", "")

	var draftID string
	ctx := context.Background()
	err := f.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		issuer := billing.NewIssuer(billing.FixedTaxPricer{
			Rate: money.MustParseRatio("0%"), Rounding: money.RoundHalfUp, Policy: "none",
		})
		id, _, _, err := issuer.CreateDraft(ctx, tx, billing.IssueRequest{
			CustomerID: f.customerID,
			Lines: []billing.Line{{
				Description: "VPS", QuantityMilli: 1000, UnitPrice: money.New(1200, usd),
			}},
			Customer: billing.CustomerDetails{ID: f.customerID},
		})
		draftID = id
		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = svc.Start(ctx, draftID)
	if !errors.Is(err, checkout.ErrInvoiceNotPayable) {
		t.Fatalf("got %v, want ErrInvoiceNotPayable", err)
	}
}

// A gateway failure must leave no attempt that blocks a retry.
func TestGatewayFailureCancelsAttempt(t *testing.T) {
	f := newFixture(t)
	gw := &fakeGateway{fail: errors.New("gateway down")}
	svc := checkout.New(f.pool, gw, "acct", "", "")

	ctx := context.Background()
	if _, err := svc.Start(ctx, f.invoiceID); err == nil {
		t.Fatal("expected an error")
	}

	var status string
	if err := f.pool.QueryRow(ctx, `SELECT status FROM payment_attempts`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "cancelled" {
		t.Fatalf("attempt status = %q, want cancelled", status)
	}

	// A retry must now be able to create a session.
	gw.fail = nil
	if _, err := svc.Start(ctx, f.invoiceID); err != nil {
		t.Fatalf("retry after failure: %v", err)
	}
}

// ---------------------------------------------- amount verification ----------

// The settled amount must match what the customer was asked to pay. An
// underpayment is an anomaly, not a partial payment.
func TestProcessorRejectsAmountMismatch(t *testing.T) {
	f := newFixture(t)
	gw := &fakeGateway{}
	svc := checkout.New(f.pool, gw, "acct", "", "")
	if _, err := svc.Start(context.Background(), f.invoiceID); err != nil {
		t.Fatal(err)
	}
	gw.invoiceID = f.invoiceID

	// The gateway reports a settled amount of 1.00 against a 12.00 checkout.
	gw.amount = &extproto.Amount{Minor: 100, Currency: "USD"}

	inbox := payments.NewInbox(f.pool)
	processor := payments.NewProcessor(f.pool, inbox, gw, "acct", nil).
		WithAttempts(checkout.LookupFor(f.pool))

	body := []byte(`{"event_id":"evt_mismatch","event_type":"payment.succeeded","payment_id":"pay_x"}`)
	if _, err := inbox.Receive(context.Background(), "fakegateway", "acct",
		payments.VerifiedEvent{EventID: "evt_mismatch", EventType: "payment.succeeded", PaymentID: "pay_x"},
		body); err != nil {
		t.Fatal(err)
	}

	_, err := processor.Process(context.Background(), 10)
	if !errors.Is(err, payments.ErrAmountMismatch) {
		t.Fatalf("got %v, want ErrAmountMismatch", err)
	}

	// Nothing was credited.
	var paid int64
	if err := f.pool.QueryRow(context.Background(),
		`SELECT amount_paid_minor FROM invoices WHERE id = $1`, f.invoiceID).Scan(&paid); err != nil {
		t.Fatal(err)
	}
	if paid != 0 {
		t.Fatalf("invoice credited %d on a mismatched amount", paid)
	}
}

// A payment with no recorded checkout cannot be applied: there is nothing to
// verify it against.
func TestProcessorRejectsUnrecordedPayment(t *testing.T) {
	f := newFixture(t)
	gw := &fakeGateway{invoiceID: f.invoiceID}

	inbox := payments.NewInbox(f.pool)
	processor := payments.NewProcessor(f.pool, inbox, gw, "acct", nil).
		WithAttempts(checkout.LookupFor(f.pool))

	body := []byte(`{"event_id":"evt_unknown","event_type":"payment.succeeded","payment_id":"pay_unknown"}`)
	if _, err := inbox.Receive(context.Background(), "fakegateway", "acct",
		payments.VerifiedEvent{EventID: "evt_unknown", EventType: "payment.succeeded", PaymentID: "pay_unknown"},
		body); err != nil {
		t.Fatal(err)
	}

	if _, err := processor.Process(context.Background(), 10); !errors.Is(err, payments.ErrAmountMismatch) {
		t.Fatalf("got %v, want ErrAmountMismatch", err)
	}
}
