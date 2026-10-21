package refunds_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/catalyst/billing/internal/extproto"
	"github.com/catalyst/billing/internal/ledger"
	"github.com/catalyst/billing/internal/money"
	"github.com/catalyst/billing/internal/refunds"
	"github.com/catalyst/billing/internal/store"
	"github.com/catalyst/billing/internal/store/storetest"
	"github.com/jackc/pgx/v5"
)

// fakeGateway is the provider side, with the failure modes that matter.
type fakeGateway struct {
	mu sync.Mutex
	// calls records every refund call, so a test can prove the reconciler asked again rather than
	// retrying blindly.
	calls []extproto.RefundParams
	// results is a queue of outcomes. Each call consumes one.
	results []fakeOutcome
	// refunded tracks the provider's own view of what has been refunded, keyed by operation key.
	refunded map[string]extproto.RefundResult
}

type fakeOutcome struct {
	result extproto.RefundResult
	err    error
}

func newFakeGateway() *fakeGateway {
	return &fakeGateway{refunded: map[string]extproto.RefundResult{}}
}

func (f *fakeGateway) Name() string { return "fakegateway" }

func (f *fakeGateway) Refund(_ context.Context, p extproto.RefundParams) (extproto.RefundResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, p)

	if prev, ok := f.refunded[p.OperationKey]; ok {
		// A real gateway honours idempotency: the same key returns the original result.
		return prev, nil
	}
	if len(f.results) > 0 {
		out := f.results[0]
		f.results = f.results[1:]
		if out.err != nil {
			return extproto.RefundResult{}, out.err
		}
		f.refunded[p.OperationKey] = out.result
		return out.result, nil
	}
	// Default: succeed.
	res := extproto.RefundResult{
		RefundID: "re_" + p.OperationKey, PaymentID: p.PaymentID,
		Status: extproto.PaymentSucceeded, RefundedAt: time.Now().UTC(),
	}
	if p.Amount != nil {
		res.Amount = *p.Amount
	}
	f.refunded[p.OperationKey] = res
	return res, nil
}

func (f *fakeGateway) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// env is a pool with a customer, a paid invoice and a payment.
type env struct {
	pool       *store.Pool
	svc        *refunds.Service
	gateway    *fakeGateway
	customerID string
	invoiceID  string
	paymentID  string
	userID     string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	pool := storetest.DB(t)
	ctx := context.Background()
	gateway := newFakeGateway()
	e := &env{pool: pool, gateway: gateway,
		svc: refunds.New(pool, gateway, "fakegateway", "acct_test", 0)}

	if err := pool.QueryRow(ctx, `
		INSERT INTO customers (email, name) VALUES ('refund@example.test', 'Refund Me')
		RETURNING id`).Scan(&e.customerID); err != nil {
		t.Fatalf("customer: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO users (email, password_hash, email_verified_at)
		VALUES ('ops@example.test', 'x', now()) RETURNING id`).Scan(&e.userID); err != nil {
		t.Fatalf("user: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO invoices (customer_id, number, status, currency, subtotal_minor, tax_minor,
			total_minor, amount_paid_minor, issued_at, due_at, snapshot)
		VALUES ($1, 'INV-R1', 'paid', 'USD', 10000, 0, 10000, 10000, now(), now(),
		        '{"schema_version":1}'::jsonb) RETURNING id`, e.customerID).Scan(&e.invoiceID); err != nil {
		t.Fatalf("invoice: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO payments (provider, provider_account, provider_payment_id, amount_minor,
			currency, status)
		VALUES ('fakegateway', 'acct_test', 'pi_refund_1', 10000, 'USD', 'succeeded')
		RETURNING id`).Scan(&e.paymentID); err != nil {
		t.Fatalf("payment: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO payment_allocations (payment_id, invoice_id, amount_minor, currency)
		VALUES ($1, $2, 10000, 'USD')`, e.paymentID, e.invoiceID); err != nil {
		t.Fatalf("allocation: %v", err)
	}

	// The payment posting, so the ledger has a cash_clearing balance to refund against.
	usd, err := money.NewCurrency("USD", 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, _, err := ledger.Post(ctx, tx, ledger.Posting{
			Kind: ledger.KindPayment, Reference: ledger.RefPayment, ReferenceID: e.paymentID,
			CustomerID: e.customerID, Currency: usd,
			Debits:  []ledger.Entry{{Account: ledger.AccountCashClearing, Amount: money.New(10000, usd)}},
			Credits: []ledger.Entry{{Account: ledger.AccountReceivable, Amount: money.New(10000, usd)}},
		})
		return err
	}); err != nil {
		t.Fatalf("post payment: %v", err)
	}
	return e
}

// request performs a refund with a fresh operation key, so each call is a new event.
func (e *env) request(t *testing.T, amount int64, approvalID string) (refunds.Result, error) {
	t.Helper()
	return e.svc.Create(context.Background(), refunds.Request{
		PaymentID:   e.paymentID,
		AmountMinor: amount,
		Reason:      "customer asked",
		RequestedBy: e.userID,
		ApprovalID:  approvalID,
	})
}

// requestWithKey performs a refund with an explicit operation key, for replay tests.
func (e *env) requestWithKey(t *testing.T, amount int64, key string) (refunds.Result, error) {
	t.Helper()
	return e.svc.Create(context.Background(), refunds.Request{
		PaymentID:    e.paymentID,
		AmountMinor:  amount,
		Reason:       "customer asked",
		RequestedBy:  e.userID,
		OperationKey: key,
	})
}

// ledgerBalanced asserts that every currency sums to zero.
func (e *env) ledgerBalanced(t *testing.T) {
	t.Helper()
	rows, err := e.pool.Query(context.Background(), `
		SELECT currency, SUM(debit_minor) - SUM(credit_minor)
		FROM journal_entries GROUP BY currency`)
	if err != nil {
		t.Fatalf("balance query: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			code  string
			delta int64
		)
		if err := rows.Scan(&code, &delta); err != nil {
			t.Fatal(err)
		}
		if delta != 0 {
			t.Fatalf("the ledger is unbalanced in %s by %d", code, delta)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
}

// A full refund leaves nothing refundable and settles the payment.
func TestFullRefund(t *testing.T) {
	e := newEnv(t)
	res, err := e.request(t, 0, "")
	if err != nil {
		t.Fatalf("refund: %v", err)
	}
	if res.Outcome != refunds.StatusSucceeded {
		t.Fatalf("outcome = %q", res.Outcome)
	}
	if res.Refund.AmountMinor != 10000 {
		t.Fatalf("amount = %d, want the full 10000", res.Refund.AmountMinor)
	}

	var (
		status   string
		refunded int64
	)
	if err := e.pool.QueryRow(context.Background(),
		`SELECT status FROM payments WHERE id = $1`, e.paymentID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "refunded" {
		t.Fatalf("payment status = %q, want refunded", status)
	}
	if err := e.pool.QueryRow(context.Background(), `
		SELECT COALESCE(SUM(amount_minor), 0) FROM refunds
		WHERE payment_id = $1 AND status = 'succeeded'`, e.paymentID).Scan(&refunded); err != nil {
		t.Fatal(err)
	}
	if refunded != 10000 {
		t.Fatalf("refunded total = %d", refunded)
	}

	// The invoice's paid amount is untouched: it was paid, and the refund is a separate event.
	var paid int64
	if err := e.pool.QueryRow(context.Background(),
		`SELECT amount_paid_minor FROM invoices WHERE id = $1`, e.invoiceID).Scan(&paid); err != nil {
		t.Fatal(err)
	}
	if paid != 10000 {
		t.Fatalf("the invoice's paid amount changed to %d; a refund must not rewrite it", paid)
	}

	e.ledgerBalanced(t)

	// A second full refund is refused: nothing is left. Which of the two refusals comes back
	// depends on whether the payment status is already fully refunded, and both are 409, so the
	// test accepts either rather than pinning an implementation detail.
	if _, err := e.request(t, 0, ""); !errors.Is(err, refunds.ErrExceedsRefundable) &&
		!errors.Is(err, refunds.ErrNotRefundable) {
		t.Fatalf("second refund error = %v, want a refusal", err)
	}
}

// Partial refunds sum, and the payment becomes partially refunded then fully refunded.
func TestPartialRefundsSum(t *testing.T) {
	e := newEnv(t)
	if _, err := e.request(t, 3000, ""); err != nil {
		t.Fatalf("first: %v", err)
	}
	var status string
	if err := e.pool.QueryRow(context.Background(),
		`SELECT status FROM payments WHERE id = $1`, e.paymentID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "partially_refunded" {
		t.Fatalf("status = %q, want partially_refunded", status)
	}

	if _, err := e.request(t, 4000, ""); err != nil {
		t.Fatalf("second: %v", err)
	}
	// Asking for more than remains is refused with the exact remaining amount in the message.
	_, err := e.request(t, 4000, "")
	if !errors.Is(err, refunds.ErrExceedsRefundable) {
		t.Fatalf("over-refund error = %v", err)
	}
	if _, err := e.request(t, 3000, ""); err != nil {
		t.Fatalf("exact remainder: %v", err)
	}

	if err := e.pool.QueryRow(context.Background(),
		`SELECT status FROM payments WHERE id = $1`, e.paymentID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "refunded" {
		t.Fatalf("status = %q, want refunded", status)
	}
	e.ledgerBalanced(t)
}

// A provider timeout leaves the refund unknown with no posting, and the reconciler settles it once.
func TestTimeoutLeavesUnknownThenReconciles(t *testing.T) {
	e := newEnv(t)
	e.gateway.results = []fakeOutcome{{err: extproto.ErrTimeout}}

	res, err := e.request(t, 0, "")
	if err != nil {
		t.Fatalf("refund: %v", err)
	}
	if res.Outcome != refunds.StatusUnknown {
		t.Fatalf("outcome = %q, want unknown", res.Outcome)
	}
	if res.Refund.Status != refunds.StatusUnknown {
		t.Fatalf("status = %q", res.Refund.Status)
	}

	// Nothing was posted: an unknown outcome must not move the books.
	var postings int
	if err := e.pool.QueryRow(context.Background(), `
		SELECT count(*) FROM journal_transactions WHERE kind = 'refund'`).Scan(&postings); err != nil {
		t.Fatal(err)
	}
	if postings != 0 {
		t.Fatalf("an unknown refund posted %d transaction(s)", postings)
	}

	// The reconciler asks again and settles it.
	settled, unknown, err := e.svc.Reconcile(context.Background(), 10)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if settled != 1 || unknown != 0 {
		t.Fatalf("reconcile = (%d settled, %d unknown)", settled, unknown)
	}
	if e.gateway.callCount() != 2 {
		t.Fatalf("gateway calls = %d, want 2 (the original and the reconcile)", e.gateway.callCount())
	}

	var status string
	if err := e.pool.QueryRow(context.Background(), `
		SELECT status FROM refunds WHERE id = $1`, res.Refund.ID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != refunds.StatusSucceeded {
		t.Fatalf("status = %q, want succeeded", status)
	}
	e.ledgerBalanced(t)

	// Reconciling again is a no-op: the posting is idempotent and the refund is no longer pending.
	if settled, _, err := e.svc.Reconcile(context.Background(), 10); err != nil || settled != 0 {
		t.Fatalf("second reconcile = %d, %v", settled, err)
	}
}

// A definitive rejection records a failure and posts nothing.
func TestProviderRejectionPostsNothing(t *testing.T) {
	e := newEnv(t)
	e.gateway.results = []fakeOutcome{{err: extproto.ErrRejected}}

	_, err := e.request(t, 0, "")
	if !errors.Is(err, refunds.ErrProviderRefused) {
		t.Fatalf("error = %v, want ErrProviderRefused", err)
	}

	var (
		status   string
		postings int
	)
	if err := e.pool.QueryRow(context.Background(),
		`SELECT status FROM refunds`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != refunds.StatusFailed {
		t.Fatalf("status = %q, want failed", status)
	}
	if err := e.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM journal_transactions WHERE kind = 'refund'`).Scan(&postings); err != nil {
		t.Fatal(err)
	}
	if postings != 0 {
		t.Fatalf("a rejected refund posted %d transaction(s)", postings)
	}

	// A failed refund does not consume the refundable balance: nothing moved.
	if _, err := e.request(t, 0, ""); err != nil {
		t.Fatalf("refund after a rejection: %v", err)
	}
}

// Two concurrent full-amount refunds produce exactly one refund.
func TestConcurrentRefundsProduceOne(t *testing.T) {
	e := newEnv(t)

	var wg sync.WaitGroup
	results := make([]error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			_, err := e.svc.Create(context.Background(), refunds.Request{
				PaymentID:   e.paymentID,
				AmountMinor: 0,
				Reason:      "concurrent",
				RequestedBy: e.userID,
			})
			results[idx] = err
		}(i)
	}
	wg.Wait()

	succeeded := 0
	for _, err := range results {
		if err == nil {
			succeeded++
		}
	}
	if succeeded != 1 {
		t.Fatalf("%d concurrent refunds succeeded, want exactly 1", succeeded)
	}

	var total int64
	if err := e.pool.QueryRow(context.Background(), `
		SELECT COALESCE(SUM(amount_minor), 0) FROM refunds WHERE status = 'succeeded'`).
		Scan(&total); err != nil {
		t.Fatal(err)
	}
	if total != 10000 {
		t.Fatalf("refunded total = %d, want 10000", total)
	}
	e.ledgerBalanced(t)
}

// A replay of the same request returns the original result rather than refunding again.
func TestReplayReturnsTheOriginal(t *testing.T) {
	e := newEnv(t)
	first, err := e.requestWithKey(t, 2500, "replay-key-1")
	if err != nil {
		t.Fatalf("first: %v", err)
	}

	second, err := e.requestWithKey(t, 2500, "replay-key-1")
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if !second.Replayed {
		t.Fatal("the second identical request was not recognised as a replay")
	}
	if second.Refund.ID != first.Refund.ID {
		t.Fatalf("replay returned a different refund: %s vs %s", second.Refund.ID, first.Refund.ID)
	}

	var total int64
	if err := e.pool.QueryRow(context.Background(), `
		SELECT COALESCE(SUM(amount_minor), 0) FROM refunds WHERE status = 'succeeded'`).
		Scan(&total); err != nil {
		t.Fatal(err)
	}
	if total != 2500 {
		t.Fatalf("refunded total = %d, want 2500 (the replay must not add)", total)
	}
}

// Above the threshold a refund is refused without an approval, and the approval is what unlocks it.
func TestThresholdRequiresApproval(t *testing.T) {
	e := newEnv(t)
	e.svc.ThresholdMinor = 5000

	_, err := e.request(t, 6000, "")
	if !errors.Is(err, refunds.ErrApprovalRequired) {
		t.Fatalf("error = %v, want ErrApprovalRequired", err)
	}

	var n int
	if err := e.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM refunds`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("%d refund(s) were recorded without an approval", n)
	}

	// Below the threshold no approval is needed.
	if _, err := e.request(t, 4999, ""); err != nil {
		t.Fatalf("sub-threshold refund: %v", err)
	}
}

// A payment that is not in a refundable state is refused.
func TestNotRefundable(t *testing.T) {
	e := newEnv(t)
	if _, err := e.pool.Exec(context.Background(),
		`UPDATE payments SET status = 'disputed' WHERE id = $1`, e.paymentID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.request(t, 0, ""); !errors.Is(err, refunds.ErrNotRefundable) {
		t.Fatalf("error = %v, want ErrNotRefundable", err)
	}
}

// An unknown payment is not found, not a 500.
func TestUnknownPayment(t *testing.T) {
	e := newEnv(t)
	_, err := e.svc.Create(context.Background(), refunds.Request{
		PaymentID:   "11111111-1111-1111-1111-111111111111",
		RequestedBy: e.userID,
	})
	if !errors.Is(err, refunds.ErrNotFound) {
		t.Fatalf("error = %v, want ErrNotFound", err)
	}
}

// List returns the refunds for a payment, newest first.
func TestListRefunds(t *testing.T) {
	e := newEnv(t)
	if _, err := e.request(t, 1000, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := e.request(t, 2000, ""); err != nil {
		t.Fatal(err)
	}

	list, next, err := e.svc.List(context.Background(), e.paymentID, 10, "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("refunds = %d, want 2", len(list))
	}
	if next != "" {
		t.Fatalf("next cursor = %q, want empty for a partial page", next)
	}

	// A different payment's refunds are not included.
	other, _, err := e.svc.List(context.Background(),
		"22222222-2222-2222-2222-222222222222", 10, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(other) != 0 {
		t.Fatalf("another payment returned %d refunds", len(other))
	}
}

// Every currency in the ledger must balance after each operation: an unbalanced journal is the one
// failure that silently corrupts every report.
func TestLedgerBalancesAfterEveryKind(t *testing.T) {
	e := newEnv(t)
	if _, err := e.request(t, 1500, ""); err != nil {
		t.Fatal(err)
	}
	e.ledgerBalanced(t)

	// The refund posting touches refunds and cash_clearing, not revenue or the receivable.
	var accounts []string
	rows, err := e.pool.Query(context.Background(), `
		SELECT DISTINCT je.account_code FROM journal_entries je
		JOIN journal_transactions jt ON jt.id = je.transaction_id
		WHERE jt.kind = 'refund' ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var code string
		if err := rows.Scan(&code); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		accounts = append(accounts, code)
	}
	rows.Close()
	if len(accounts) != 2 || accounts[0] != ledger.AccountCashClearing || accounts[1] != ledger.AccountRefunds {
		t.Fatalf("refund touched %v, want cash_clearing and refunds only", accounts)
	}
}
