package ledger_test

import (
	"context"
	"errors"
	"testing"

	"github.com/catalyst/billing/internal/ledger"
	"github.com/catalyst/billing/internal/money"
	"github.com/catalyst/billing/internal/store"
	"github.com/catalyst/billing/internal/store/storetest"
	"github.com/jackc/pgx/v5"
)

var usd = money.MustLookupCurrency("USD")

func newPool(t *testing.T) (*store.Pool, string) {
	t.Helper()
	pool := storetest.DB(t)
	var customerID string
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO customers (email, name) VALUES ('l@example.test', 'L') RETURNING id`).
		Scan(&customerID); err != nil {
		t.Fatalf("customer: %v", err)
	}
	return pool, customerID
}

// refID produces a fresh document id. References are UUIDs in the schema, so a
// test cannot invent a readable one.
//
// It takes the pool rather than calling storetest.DB: re-entering the harness from
// a subtest would try to take the same advisory lock the parent already holds.
func refID(t *testing.T, pool *store.Pool) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(),
		`SELECT gen_random_uuid()::text`).Scan(&id); err != nil {
		t.Fatalf("uuid: %v", err)
	}
	return id
}

func posting(refID, customerID string, debit, credit int64) ledger.Posting {
	return ledger.Posting{
		Kind:        ledger.KindInvoiceIssued,
		Reference:   ledger.RefInvoice,
		ReferenceID: refID,
		CustomerID:  customerID,
		Currency:    usd,
		Debits:      []ledger.Entry{{Account: ledger.AccountReceivable, Amount: money.New(debit, usd)}},
		Credits:     []ledger.Entry{{Account: ledger.AccountRevenue, Amount: money.New(credit, usd)}},
	}
}

// Re-posting the same event is a no-op, not a second posting.
func TestPostIsIdempotentForTheSameEvent(t *testing.T) {
	pool, customerID := newPool(t)
	ctx := context.Background()
	p := posting(refID(t, pool), customerID, 1200, 1200)

	var firstID, secondID string
	var alreadyPosted bool
	err := pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		firstID, _, err = ledger.Post(ctx, tx, p)
		return err
	})
	if err != nil {
		t.Fatalf("first post: %v", err)
	}
	err = pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		secondID, alreadyPosted, err = ledger.Post(ctx, tx, p)
		return err
	})
	if err != nil {
		t.Fatalf("second post: %v", err)
	}
	if !alreadyPosted {
		t.Fatal("re-posting was not reported as already posted")
	}
	if firstID != secondID {
		t.Fatalf("different transactions: %s vs %s", firstID, secondID)
	}

	var entries int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM journal_entries`).Scan(&entries); err != nil {
		t.Fatal(err)
	}
	if entries != 2 {
		t.Fatalf("%d entries, want 2", entries)
	}
}

// The reviewer's finding: posting a different amount under the same reference used
// to return success while writing nothing, so a caller believed its entries
// existed.
func TestPostRejectsDifferentContentForSameReference(t *testing.T) {
	pool, customerID := newPool(t)
	ctx := context.Background()

	ref := refID(t, pool)
	err := pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, _, err := ledger.Post(ctx, tx, posting(ref, customerID, 100, 100))
		return err
	})
	if err != nil {
		t.Fatalf("first post: %v", err)
	}

	cases := []struct {
		name string
		p    ledger.Posting
	}{
		{"different amount", posting(ref, customerID, 999, 999)},
		{"different accounts", ledger.Posting{
			Kind: ledger.KindInvoiceIssued, Reference: ledger.RefInvoice, ReferenceID: ref,
			CustomerID: customerID, Currency: usd,
			Debits:  []ledger.Entry{{Account: ledger.AccountReceivable, Amount: money.New(100, usd)}},
			Credits: []ledger.Entry{{Account: ledger.AccountTaxPayable, Amount: money.New(100, usd)}},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
				_, _, err := ledger.Post(ctx, tx, tc.p)
				return err
			})
			if !errors.Is(err, ledger.ErrReferenceReused) {
				t.Fatalf("got %v, want ErrReferenceReused", err)
			}
		})
	}

	// The original posting is untouched.
	balance, err := ledger.Balances(ctx, pool, ledger.AccountReceivable, usd)
	if err != nil {
		t.Fatal(err)
	}
	if balance.Debits.Minor() != 100 {
		t.Fatalf("receivable = %s, want 1.00", balance.Debits)
	}
}

// Entry order is not part of a posting's identity.
func TestPostAcceptsReorderedEntries(t *testing.T) {
	pool, customerID := newPool(t)
	ctx := context.Background()

	p := ledger.Posting{
		Kind: ledger.KindInvoiceIssued, Reference: ledger.RefInvoice, ReferenceID: refID(t, pool),
		CustomerID: customerID, Currency: usd,
		Debits: []ledger.Entry{
			{Account: ledger.AccountReceivable, Amount: money.New(1000, usd)},
			{Account: ledger.AccountReceivable, Amount: money.New(200, usd)},
		},
		Credits: []ledger.Entry{
			{Account: ledger.AccountRevenue, Amount: money.New(800, usd)},
			{Account: ledger.AccountTaxPayable, Amount: money.New(400, usd)},
		},
	}
	if err := pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, _, err := ledger.Post(ctx, tx, p)
		return err
	}); err != nil {
		t.Fatalf("first post: %v", err)
	}

	reordered := p
	reordered.Debits = []ledger.Entry{p.Debits[1], p.Debits[0]}
	reordered.Credits = []ledger.Entry{p.Credits[1], p.Credits[0]}
	var already bool
	err := pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		_, already, err = ledger.Post(ctx, tx, reordered)
		return err
	})
	if err != nil {
		t.Fatalf("reordered post: %v", err)
	}
	if !already {
		t.Fatal("reordered entries were not recognised as the same posting")
	}
}

func TestPostValidation(t *testing.T) {
	pool, customerID := newPool(t)
	ctx := context.Background()

	eur := money.MustLookupCurrency("EUR")
	cases := []struct {
		name string
		p    ledger.Posting
		want error
	}{
		{"unbalanced", ledger.Posting{
			Kind: ledger.KindInvoiceIssued, Reference: ledger.RefInvoice, ReferenceID: refID(t, pool),
			CustomerID: customerID, Currency: usd,
			Debits:  []ledger.Entry{{Account: ledger.AccountReceivable, Amount: money.New(100, usd)}},
			Credits: []ledger.Entry{{Account: ledger.AccountRevenue, Amount: money.New(99, usd)}},
		}, ledger.ErrUnbalanced},
		{"empty", ledger.Posting{
			Kind: ledger.KindInvoiceIssued, Reference: ledger.RefInvoice, ReferenceID: refID(t, pool),
			CustomerID: customerID, Currency: usd,
		}, ledger.ErrEmptyPosting},
		{"mixed currency", ledger.Posting{
			Kind: ledger.KindInvoiceIssued, Reference: ledger.RefInvoice, ReferenceID: refID(t, pool),
			CustomerID: customerID, Currency: usd,
			Debits:  []ledger.Entry{{Account: ledger.AccountReceivable, Amount: money.New(100, eur)}},
			Credits: []ledger.Entry{{Account: ledger.AccountRevenue, Amount: money.New(100, eur)}},
		}, ledger.ErrMixedCurrency},
		{"receivable without customer", ledger.Posting{
			Kind: ledger.KindInvoiceIssued, Reference: ledger.RefInvoice, ReferenceID: refID(t, pool),
			Currency: usd,
			Debits:   []ledger.Entry{{Account: ledger.AccountReceivable, Amount: money.New(100, usd)}},
			Credits:  []ledger.Entry{{Account: ledger.AccountRevenue, Amount: money.New(100, usd)}},
		}, ledger.ErrInvalidPosting},
		{"unknown kind", ledger.Posting{
			Kind: "made_up", Reference: ledger.RefInvoice, ReferenceID: refID(t, pool),
			CustomerID: customerID, Currency: usd,
			Debits:  []ledger.Entry{{Account: ledger.AccountReceivable, Amount: money.New(100, usd)}},
			Credits: []ledger.Entry{{Account: ledger.AccountRevenue, Amount: money.New(100, usd)}},
		}, ledger.ErrInvalidPosting},
		{"zero amount", ledger.Posting{
			Kind: ledger.KindInvoiceIssued, Reference: ledger.RefInvoice, ReferenceID: refID(t, pool),
			CustomerID: customerID, Currency: usd,
			Debits:  []ledger.Entry{{Account: ledger.AccountReceivable, Amount: money.New(0, usd)}},
			Credits: []ledger.Entry{{Account: ledger.AccountRevenue, Amount: money.New(0, usd)}},
		}, ledger.ErrInvalidPosting},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.p.Validate(); !errors.Is(err, tc.want) {
				t.Fatalf("Validate: got %v, want %v", err, tc.want)
			}
			err := pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
				_, _, err := ledger.Post(ctx, tx, tc.p)
				return err
			})
			if err == nil {
				t.Fatal("Post accepted an invalid posting")
			}
		})
	}
}

// A rejected posting must leave nothing behind.
func TestFailedPostWritesNothing(t *testing.T) {
	pool, customerID := newPool(t)
	ctx := context.Background()

	err := pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, _, err := ledger.Post(ctx, tx, posting(refID(t, pool), customerID, 100, 50))
		return err
	})
	if err == nil {
		t.Fatal("unbalanced posting committed")
	}

	var transactions, entries int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM journal_transactions`).Scan(&transactions); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM journal_entries`).Scan(&entries); err != nil {
		t.Fatal(err)
	}
	if transactions != 0 || entries != 0 {
		t.Fatalf("%d transactions and %d entries left behind", transactions, entries)
	}
}

// Balances must be derived from the entries, not cached anywhere, and must be zero
// for an account with no activity rather than an error.
func TestBalancesAndNet(t *testing.T) {
	pool, customerID := newPool(t)
	ctx := context.Background()

	// No activity yet.
	empty, err := ledger.Balances(ctx, pool, ledger.AccountReceivable, usd)
	if err != nil {
		t.Fatal(err)
	}
	if !empty.Debits.IsZero() || !empty.Credits.IsZero() {
		t.Fatalf("empty balance = %+v, want zero", empty)
	}
	net, err := empty.Net()
	if err != nil {
		t.Fatal(err)
	}
	if !net.IsZero() {
		t.Fatalf("net of an empty account = %s, want zero", net)
	}

	// Post something and read it back.
	if err := pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, _, err := ledger.Post(ctx, tx, posting(refID(t, pool), customerID, 1500, 1500))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	ar, err := ledger.Balances(ctx, pool, ledger.AccountReceivable, usd)
	if err != nil {
		t.Fatal(err)
	}
	if ar.Debits.Minor() != 1500 || ar.Credits.Minor() != 0 {
		t.Fatalf("receivable = %s debits / %s credits, want 15.00 / 0.00", ar.Debits, ar.Credits)
	}
	if net, err = ar.Net(); err != nil {
		t.Fatal(err)
	}
	if net.Minor() != 1500 {
		t.Fatalf("net = %s, want 15.00", net)
	}

	// The revenue account holds the credit side.
	rev, err := ledger.Balances(ctx, pool, ledger.AccountRevenue, usd)
	if err != nil {
		t.Fatal(err)
	}
	if rev.Credits.Minor() != 1500 {
		t.Fatalf("revenue credits = %s, want 15.00", rev.Credits)
	}

	// A different currency sees nothing.
	eur := money.MustLookupCurrency("EUR")
	other, err := ledger.Balances(ctx, pool, ledger.AccountReceivable, eur)
	if err != nil {
		t.Fatal(err)
	}
	if !other.Debits.IsZero() {
		t.Fatalf("EUR receivable = %s, want zero", other.Debits)
	}
}

// A customer with no entries owes nothing, and one customer's balance never
// includes another's.
func TestReceivableIsPerCustomer(t *testing.T) {
	pool, alice := newPool(t)
	ctx := context.Background()

	var bob string
	if err := pool.QueryRow(ctx,
		`INSERT INTO customers (email, name) VALUES ('bob@example.test', 'B') RETURNING id`).
		Scan(&bob); err != nil {
		t.Fatal(err)
	}

	if err := pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if _, _, err := ledger.Post(ctx, tx, posting(refID(t, pool), alice, 1000, 1000)); err != nil {
			return err
		}
		_, _, err := ledger.Post(ctx, tx, posting(refID(t, pool), bob, 2500, 2500))
		return err
	}); err != nil {
		t.Fatal(err)
	}

	aliceOwed, err := ledger.ReceivableForCustomer(ctx, pool, alice, usd)
	if err != nil {
		t.Fatal(err)
	}
	if aliceOwed.Minor() != 1000 {
		t.Fatalf("alice owes %s, want 10.00", aliceOwed)
	}
	bobOwed, err := ledger.ReceivableForCustomer(ctx, pool, bob, usd)
	if err != nil {
		t.Fatal(err)
	}
	if bobOwed.Minor() != 2500 {
		t.Fatalf("bob owes %s, want 25.00", bobOwed)
	}

	// A customer with no postings owes nothing.
	var stranger string
	if err := pool.QueryRow(ctx,
		`INSERT INTO customers (email, name) VALUES ('s@example.test', 'S') RETURNING id`).
		Scan(&stranger); err != nil {
		t.Fatal(err)
	}
	owed, err := ledger.ReceivableForCustomer(ctx, pool, stranger, usd)
	if err != nil {
		t.Fatal(err)
	}
	if !owed.IsZero() {
		t.Fatalf("stranger owes %s, want zero", owed)
	}
}
