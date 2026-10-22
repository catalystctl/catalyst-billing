package credit_test

import (
	"context"
	"errors"
	"testing"

	"github.com/catalyst/billing/internal/credit"
	"github.com/catalyst/billing/internal/ledger"
)

// Staff debits (CRD-07, the missing direction).
//
// These are the tests that matter for this feature, and each one corresponds to a way a
// deduction can go wrong in production:
//
//   - it takes the wrong amount, or takes it twice (idempotency);
//   - it takes the balance below zero, or silently takes part of what was asked for;
//   - it edits the ledger instead of appending to it, so the history stops explaining the
//     balance;
//   - the customer cannot find out why it happened.
//
// The last one is not a nicety. A deduction with no explanation, on a screen the customer reads,
// is indistinguishable from a bug — and the fix for that is not a support reply, it is the memo
// on the entry they are already looking at.

// debit performs a staff debit through the real path.
func (f *fixture) debit(t *testing.T, amount int64, key, reason string) credit.Debit {
	t.Helper()
	d, err := f.svc.Debit(context.Background(), credit.DebitRequest{
		CustomerID:  f.customerID,
		AmountMinor: amount,
		Currency:    "USD",
		Reason:      reason,
		DebitedBy:   f.userID,
		Key:         key,
	})
	if err != nil {
		t.Fatalf("debit: %v", err)
	}
	return d
}

// creditEntries counts the customer's customer_credit entries and returns their signed sum.
//
// Both halves of the append-only claim are checked from here: the sum is what the balance must
// equal, and the count is what proves the debit appended rather than replaced.
func (f *fixture) creditEntries(t *testing.T) (count int, sum int64) {
	t.Helper()
	if err := f.pool.QueryRow(context.Background(), `
		SELECT count(*), COALESCE(SUM(credit_minor - debit_minor), 0)
		FROM journal_entries
		WHERE customer_id = $1 AND account_code = $2`,
		f.customerID, ledger.AccountCredit).Scan(&count, &sum); err != nil {
		t.Fatalf("credit entries: %v", err)
	}
	return count, sum
}

// A debit takes the amount away, and the balance stays the sum of the entries.
func TestDebitReducesBalanceAsANewEntry(t *testing.T) {
	f := newFixture(t)
	f.grant(t, 2500, "debit-seed")

	before, beforeSum := f.creditEntries(t)
	if beforeSum != 2500 {
		t.Fatalf("seeded balance = %d, want 2500", beforeSum)
	}

	d, err := f.svc.Debit(context.Background(), credit.DebitRequest{
		CustomerID:  f.customerID,
		AmountMinor: 1000,
		Currency:    "USD",
		Reason:      "granted in error: duplicate goodwill",
		DebitedBy:   f.userID,
		Key:         "debit-1",
	})
	if err != nil {
		t.Fatalf("debit: %v", err)
	}
	if d.AmountMinor != 1000 || d.BalanceMinor != 1500 || d.ID == "" {
		t.Fatalf("debit = %+v, want 1000 taken leaving 1500", d)
	}
	if d.AlreadyDebited {
		t.Fatal("a first debit is reported as a replay")
	}

	// Append-only, checked two ways: the balance is the sum of the entries, and the entry count
	// grew by exactly one posting's two sides rather than staying still (which is what an edit
	// of the grant's entry would have done).
	if got := f.balance(t); got != 1500 {
		t.Fatalf("balance = %d, want 1500", got)
	}
	after, afterSum := f.creditEntries(t)
	if afterSum != 1500 {
		t.Fatalf("sum of entries = %d, want the balance 1500", afterSum)
	}
	if after != before+1 {
		t.Fatalf("credit entries went from %d to %d, want exactly one more", before, after)
	}

	// The grant's own entry is untouched: an edit would have moved it.
	var grantCredited int64
	if err := f.pool.QueryRow(context.Background(), `
		SELECT SUM(credit_minor) FROM journal_entries
		WHERE customer_id = $1 AND account_code = $2 AND credit_minor > 0`,
		f.customerID, ledger.AccountCredit).Scan(&grantCredited); err != nil {
		t.Fatalf("grant entry: %v", err)
	}
	if grantCredited != 2500 {
		t.Fatalf("the grant's entry now credits %d, want the original 2500", grantCredited)
	}

	f.assertBalanced(t)
}

// A debit larger than the balance is refused, and nothing moves.
//
// This is the assertion that keeps credit from going negative silently: a clamped debit would
// leave the operator believing they took 5000 while the customer lost 1000, and the customer's
// screen would show a number nobody agreed to.
func TestDebitAboveBalanceIsRefused(t *testing.T) {
	f := newFixture(t)
	f.grant(t, 1000, "over-debit-seed")

	beforeCount, beforeSum := f.creditEntries(t)

	_, err := f.svc.Debit(context.Background(), credit.DebitRequest{
		CustomerID:  f.customerID,
		AmountMinor: 1001,
		Currency:    "USD",
		Reason:      "one minor unit too far",
		DebitedBy:   f.userID,
		Key:         "debit-over",
	})
	if !errors.Is(err, credit.ErrInsufficientCredit) {
		t.Fatalf("err = %v, want ErrInsufficientCredit", err)
	}

	afterCount, afterSum := f.creditEntries(t)
	if afterCount != beforeCount || afterSum != beforeSum {
		t.Fatalf("a refused debit wrote entries: %d -> %d, %d -> %d",
			beforeCount, afterCount, beforeSum, afterSum)
	}
	if got := f.balance(t); got != 1000 {
		t.Fatalf("balance = %d, want the untouched 1000", got)
	}
	// The refusal must not leave a posting behind either: a journal transaction with no entries
	// would be refused at commit, but a transaction with a wrong amount would not.
	var postings int
	if err := f.pool.QueryRow(context.Background(), `
		SELECT count(*) FROM journal_transactions WHERE reference_type = $1`,
		string(ledger.RefCreditDebit)).Scan(&postings); err != nil {
		t.Fatalf("posting count: %v", err)
	}
	if postings != 0 {
		t.Fatalf("%d postings exist for a refused debit, want 0", postings)
	}

	// Exactly the balance is allowed: the rule is "not below zero", not "not all of it".
	if _, err := f.svc.Debit(context.Background(), credit.DebitRequest{
		CustomerID:  f.customerID,
		AmountMinor: 1000,
		Currency:    "USD",
		Reason:      "the whole balance, deliberately",
		DebitedBy:   f.userID,
		Key:         "debit-exact",
	}); err != nil {
		t.Fatalf("debit of the exact balance: %v", err)
	}
	if got := f.balance(t); got != 0 {
		t.Fatalf("balance = %d, want 0", got)
	}
	f.assertBalanced(t)
}

// A double-submit adds once.
func TestDebitIsIdempotent(t *testing.T) {
	f := newFixture(t)
	f.grant(t, 2000, "idempotent-seed")

	first := f.debit(t, 750, "debit-key-1", "chargeback outside the gateway")
	before, _ := f.creditEntries(t)

	replay, err := f.svc.Debit(context.Background(), credit.DebitRequest{
		CustomerID:  f.customerID,
		AmountMinor: 750,
		Currency:    "USD",
		Reason:      "chargeback outside the gateway",
		DebitedBy:   f.userID,
		Key:         "debit-key-1",
	})
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if !replay.AlreadyDebited || replay.ID != first.ID || replay.AmountMinor != 750 {
		t.Fatalf("replay = %+v, want the same entry", replay)
	}
	if replay.BalanceMinor != 1250 {
		t.Fatalf("replay reports balance %d, want the current 1250", replay.BalanceMinor)
	}
	after, afterSum := f.creditEntries(t)
	if after != before {
		t.Fatalf("a replay wrote %d new entries", after-before)
	}
	if afterSum != 1250 || f.balance(t) != 1250 {
		t.Fatalf("balance after a replay = %d / %d, want 1250", afterSum, f.balance(t))
	}

	// A key reused for a *different* deduction is refused rather than silently replayed: the
	// caller must not be told their 900 was taken when 750 was.
	_, err = f.svc.Debit(context.Background(), credit.DebitRequest{
		CustomerID:  f.customerID,
		AmountMinor: 900,
		Currency:    "USD",
		Reason:      "a different amount under the same key",
		DebitedBy:   f.userID,
		Key:         "debit-key-1",
	})
	if !errors.Is(err, credit.ErrKeyReused) {
		t.Fatalf("err = %v, want ErrKeyReused", err)
	}
	if got := f.balance(t); got != 1250 {
		t.Fatalf("balance = %d, want the unchanged 1250", got)
	}
	f.assertBalanced(t)
}

// A debit needs a positive amount, a currency, a customer and a reason.
func TestDebitRequiresReasonAndAmount(t *testing.T) {
	f := newFixture(t)
	f.grant(t, 500, "validation-seed")
	ctx := context.Background()

	cases := []struct {
		name string
		req  credit.DebitRequest
	}{
		{"no reason", credit.DebitRequest{CustomerID: f.customerID, AmountMinor: 100,
			Currency: "USD", Key: "v1", DebitedBy: f.userID}},
		{"blank reason", credit.DebitRequest{CustomerID: f.customerID, AmountMinor: 100,
			Currency: "USD", Reason: "   ", Key: "v2", DebitedBy: f.userID}},
		{"zero amount", credit.DebitRequest{CustomerID: f.customerID, AmountMinor: 0,
			Currency: "USD", Reason: "why", Key: "v3", DebitedBy: f.userID}},
		{"negative amount", credit.DebitRequest{CustomerID: f.customerID, AmountMinor: -100,
			Currency: "USD", Reason: "why", Key: "v4", DebitedBy: f.userID}},
		{"no key", credit.DebitRequest{CustomerID: f.customerID, AmountMinor: 100,
			Currency: "USD", Reason: "why", DebitedBy: f.userID}},
		{"no customer", credit.DebitRequest{AmountMinor: 100,
			Currency: "USD", Reason: "why", Key: "v5", DebitedBy: f.userID}},
		{"no currency", credit.DebitRequest{CustomerID: f.customerID, AmountMinor: 100,
			Reason: "why", Key: "v6", DebitedBy: f.userID}},
	}
	for _, tc := range cases {
		if _, err := f.svc.Debit(ctx, tc.req); !errors.Is(err, credit.ErrInvalidRequest) {
			t.Fatalf("%s: err = %v, want ErrInvalidRequest", tc.name, err)
		}
	}
	if got := f.balance(t); got != 500 {
		t.Fatalf("balance = %d, want the untouched 500", got)
	}

	// An unknown customer is a not-found, not a validation error: the request is well-formed.
	_, err := f.svc.Debit(ctx, credit.DebitRequest{
		CustomerID: "00000000-0000-0000-0000-000000000000", AmountMinor: 100,
		Currency: "USD", Reason: "why", Key: "v7", DebitedBy: f.userID,
	})
	if !errors.Is(err, credit.ErrNotFound) {
		t.Fatalf("unknown customer: err = %v, want ErrNotFound", err)
	}
	f.assertBalanced(t)
}

// The reason reaches the customer's own statement.
//
// This is the acceptance criterion that a deduction must not be silent: the memo on the
// customer_credit entry is what the portal's credit history renders, so the operator's sentence
// is what the customer reads beside the negative amount.
func TestDebitReasonAppearsOnTheCustomerStatement(t *testing.T) {
	f := newFixture(t)
	f.grant(t, 3000, "statement-seed")
	const reason = "refund already paid by the provider on 3 March"
	f.debit(t, 1200, "statement-debit", reason)

	entries, err := credit.TransactionsFor(context.Background(), f.pool, f.customerID, 50)
	if err != nil {
		t.Fatalf("transactions: %v", err)
	}
	var found bool
	for _, e := range entries {
		if e.Memo != reason {
			continue
		}
		found = true
		if e.Kind != string(ledger.KindCreditDebit) {
			t.Fatalf("kind = %q, want %q", e.Kind, ledger.KindCreditDebit)
		}
		if e.Amount.Minor() != -1200 {
			t.Fatalf("amount = %d, want -1200 (a deduction reads as negative)", e.Amount.Minor())
		}
	}
	if !found {
		t.Fatalf("the debit's reason is not on the customer's statement: %+v", entries)
	}

	// And the operator's list reads the same rows, so the two screens cannot disagree.
	rows, err := credit.DebitsFor(context.Background(), f.pool, f.customerID, 10)
	if err != nil {
		t.Fatalf("debits: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("%d debits listed, want 1: %+v", len(rows), rows)
	}
	if rows[0].Reason != reason || rows[0].AmountMinor != 1200 {
		t.Fatalf("listed debit = %+v", rows[0])
	}
	if rows[0].DebitedBy != f.userID {
		t.Fatalf("debited_by = %q, want the operator %q", rows[0].DebitedBy, f.userID)
	}
}
