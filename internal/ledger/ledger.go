// Package ledger implements the operational subledger: a balanced, append-only
// record of receivables, payment clearing, customer credit, refunds and
// adjustments.
//
// This is not a general ledger and does not attempt to be one. It is the
// internal consistency spine that makes "what does this customer owe" and "where
// did this money go" answerable without trusting a cached balance.
//
// Rules enforced here:
//
//   - every posting is a set of entries that sum to zero per currency;
//   - a posting is identified by (kind, reference type, reference id), so
//     replaying the same economic event is a no-op rather than a second posting;
//   - corrections are reversals, never edits;
//   - balances are derived from entries, never stored as a mutable number.
package ledger

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/catalyst/billing/internal/money"
	"github.com/catalyst/billing/internal/store"
	"github.com/jackc/pgx/v5"
)

// Account codes are a closed set defined by the schema. They are constants
// rather than free strings so a typo cannot create an orphan account.
const (
	AccountReceivable   = "ar"
	AccountCashClearing = "cash_clearing"
	AccountCredit       = "customer_credit"
	AccountTaxPayable   = "tax_payable"
	AccountRevenue      = "revenue"
	AccountRefunds      = "refunds"
)

// Kind classifies a journal transaction. The value is persisted, so the set is
// closed and adding one is a migration.
type Kind string

const (
	KindInvoiceIssued Kind = "invoice_issued"
	KindPayment       Kind = "payment_received"
	KindRefund        Kind = "refund"
	KindCreditNote    Kind = "credit_note"
	KindAdjustment    Kind = "adjustment"
)

// Valid reports whether the kind is one the schema accepts.
func (k Kind) Valid() bool {
	switch k {
	case KindInvoiceIssued, KindPayment, KindRefund, KindCreditNote, KindAdjustment:
		return true
	default:
		return false
	}
}

// ReferenceType names the document a posting refers to.
type ReferenceType string

const (
	RefInvoice ReferenceType = "invoice"
	RefPayment ReferenceType = "payment"
	RefRefund  ReferenceType = "refund"
	RefCredit  ReferenceType = "credit_note"
)

// Entry is one side of a posting. Exactly one of Debit and Credit is set.
type Entry struct {
	Account string
	Amount  money.Amount
	Memo    string
}

// Posting is one balanced journal transaction.
type Posting struct {
	Kind      Kind
	Reference ReferenceType
	// ReferenceID is the document id. It, together with Kind and Reference,
	// makes the posting idempotent.
	ReferenceID string
	// CustomerID attributes the entries to a customer. It is required for any
	// posting that touches a customer-facing account, because an
	// unattributable receivable cannot be reconciled.
	CustomerID string
	Currency   money.Currency
	// Debits and Credits are separate so the direction is impossible to
	// misread. Their totals must be equal.
	Debits  []Entry
	Credits []Entry
}

// ErrUnbalanced is returned when a posting's debits and credits differ.
var ErrUnbalanced = errors.New("ledger: posting does not balance")

// ErrEmptyPosting is returned for a posting with no entries.
var ErrEmptyPosting = errors.New("ledger: posting has no entries")

// ErrMixedCurrency is returned when a posting mixes currencies.
var ErrMixedCurrency = errors.New("ledger: posting mixes currencies")

// ErrInvalidPosting is returned for malformed postings.
var ErrInvalidPosting = errors.New("ledger: invalid posting")

// Validate checks a posting before it reaches the database, so a caller gets a
// clear domain error instead of a deferred constraint failure at commit.
func (p Posting) Validate() error {
	if !p.Kind.Valid() {
		return fmt.Errorf("%w: unknown kind %q", ErrInvalidPosting, p.Kind)
	}
	if p.Reference == "" || p.ReferenceID == "" {
		return fmt.Errorf("%w: missing reference", ErrInvalidPosting)
	}
	if !p.Currency.IsValid() {
		return fmt.Errorf("%w: no currency", ErrInvalidPosting)
	}
	if len(p.Debits)+len(p.Credits) == 0 {
		return ErrEmptyPosting
	}
	if p.CustomerID == "" {
		for _, e := range append(append([]Entry{}, p.Debits...), p.Credits...) {
			if e.Account == AccountReceivable || e.Account == AccountCredit {
				return fmt.Errorf("%w: %s entry needs a customer", ErrInvalidPosting, e.Account)
			}
		}
	}

	var debitTotal, creditTotal money.Amount
	debitTotal = money.Zero(p.Currency)
	creditTotal = money.Zero(p.Currency)

	for _, side := range []struct {
		entries []Entry
		total   *money.Amount
		name    string
	}{
		{p.Debits, &debitTotal, "debit"},
		{p.Credits, &creditTotal, "credit"},
	} {
		for _, e := range side.entries {
			if e.Account == "" {
				return fmt.Errorf("%w: %s entry with no account", ErrInvalidPosting, side.name)
			}
			if !e.Amount.IsValid() {
				return fmt.Errorf("%w: %s entry on %s has no currency", ErrInvalidPosting, side.name, e.Account)
			}
			if !e.Amount.Currency().Equal(p.Currency) {
				return fmt.Errorf("%w: %s entry on %s is %s, posting is %s",
					ErrMixedCurrency, side.name, e.Account, e.Amount.Currency(), p.Currency)
			}
			if !e.Amount.IsPositive() {
				return fmt.Errorf("%w: %s entry on %s must be positive, got %s",
					ErrInvalidPosting, side.name, e.Account, e.Amount)
			}
			next, err := side.total.Add(e.Amount)
			if err != nil {
				return fmt.Errorf("%w: %s total: %v", ErrInvalidPosting, side.name, err)
			}
			*side.total = next
		}
	}

	cmp, err := debitTotal.Cmp(creditTotal)
	if err != nil {
		return err
	}
	if cmp != 0 {
		return fmt.Errorf("%w: debits %s != credits %s", ErrUnbalanced, debitTotal, creditTotal)
	}
	return nil
}

// Poster writes postings inside an existing transaction.
type Poster struct{}

// ErrReferenceReused is returned when a reference is re-posted with different
// content. Silently returning the existing transaction would tell the caller its
// entries were written when they were not, which is how a payment can be marked
// applied with no ledger behind it.
var ErrReferenceReused = errors.New("ledger: reference already posted with different content")

// Post writes a posting, or returns the existing transaction id if this exact
// economic event was already posted.
//
// It runs inside the caller's transaction on purpose: the invoice transition,
// the payment allocation and the journal entries must commit together or not at
// all.
func Post(ctx context.Context, tx pgx.Tx, p Posting) (transactionID string, alreadyPosted bool, err error) {
	if err := p.Validate(); err != nil {
		return "", false, err
	}

	err = tx.QueryRow(ctx, `
		INSERT INTO journal_transactions (kind, reference_type, reference_id, currency)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (kind, reference_type, reference_id) DO NOTHING
		RETURNING id`,
		string(p.Kind), string(p.Reference), p.ReferenceID, p.Currency.Code(),
	).Scan(&transactionID)
	if errors.Is(err, pgx.ErrNoRows) {
		// Already posted. The existing transaction is only accepted if it has the
		// same shape: same currency, same number of entries, and the same accounts
		// with the same amounts. A re-post of the same event is a no-op; a
		// different event sharing a reference is a bug that must not be hidden.
		if err := tx.QueryRow(ctx, `
			SELECT id FROM journal_transactions
			WHERE kind = $1 AND reference_type = $2 AND reference_id = $3`,
			string(p.Kind), string(p.Reference), p.ReferenceID,
		).Scan(&transactionID); err != nil {
			return "", false, fmt.Errorf("ledger: look up existing posting: %w", err)
		}
		if err := samePosting(ctx, tx, transactionID, p); err != nil {
			return "", false, err
		}
		return transactionID, true, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("ledger: insert journal transaction: %w", err)
	}

	insert := func(account string, debit, credit money.Amount, memo string) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO journal_entries (transaction_id, account_code, currency, debit_minor, credit_minor, memo, customer_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			transactionID, account, p.Currency.Code(), debit.Minor(), credit.Minor(), memo, nullableUUID(p.CustomerID))
		if err != nil {
			return fmt.Errorf("ledger: insert entry on %s: %w", account, err)
		}
		return nil
	}
	zero := money.Zero(p.Currency)
	for _, e := range p.Debits {
		if err := insert(e.Account, e.Amount, zero, e.Memo); err != nil {
			return "", false, err
		}
	}
	for _, e := range p.Credits {
		if err := insert(e.Account, zero, e.Amount, e.Memo); err != nil {
			return "", false, err
		}
	}
	return transactionID, false, nil
}

// AccountBalance is a derived balance for one account and currency.
type AccountBalance struct {
	Account  string
	Currency money.Currency
	// Debits and Credits are the totals on each side.
	Debits  money.Amount
	Credits money.Amount
}

// Net returns debits minus credits. For an asset account that is the amount
// outstanding; for a liability it is the negative of the amount owed.
func (b AccountBalance) Net() (money.Amount, error) { return b.Debits.Sub(b.Credits) }

// Querier is the read surface a caller must provide: a pool, a transaction or a
// connection all satisfy it.
type Querier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

// Balances derives account totals directly from journal entries. Nothing is
// cached, so this cannot disagree with the entries it is computed from.
func Balances(ctx context.Context, q Querier, account string, cur money.Currency) (AccountBalance, error) {
	var debitMinor, creditMinor int64
	err := q.QueryRow(ctx, `
		SELECT COALESCE(SUM(debit_minor), 0), COALESCE(SUM(credit_minor), 0)
		FROM journal_entries
		WHERE account_code = $1 AND currency = $2`,
		account, cur.Code()).Scan(&debitMinor, &creditMinor)
	if err != nil {
		return AccountBalance{}, fmt.Errorf("ledger: balance %s: %w", account, err)
	}
	return AccountBalance{
		Account:  account,
		Currency: cur,
		Debits:   money.New(debitMinor, cur),
		Credits:  money.New(creditMinor, cur),
	}, nil
}

// ReceivableForCustomer derives the outstanding receivable for one customer by
// joining journal entries back to the documents they belong to. It is the
// authoritative "what does this customer owe" answer, and it deliberately does
// not read any cached balance column.
func ReceivableForCustomer(ctx context.Context, q Querier, customerID string, cur money.Currency) (money.Amount, error) {
	var minor int64
	err := q.QueryRow(ctx, `
		SELECT COALESCE(SUM(debit_minor - credit_minor), 0)
		FROM journal_entries
		WHERE account_code = $1
		  AND currency = $2
		  AND customer_id = $3`,
		AccountReceivable, cur.Code(), customerID).Scan(&minor)
	if err != nil {
		return money.Amount{}, fmt.Errorf("ledger: receivable for customer: %w", err)
	}
	return money.New(minor, cur), nil
}

// TrialBalance asserts that every currency in the ledger sums to zero across all
// accounts. Any non-zero result means an entry was written outside the posting
// service, which is the bug this function exists to detect.
func TrialBalance(ctx context.Context, pool *store.Pool) (map[string]money.Amount, error) {
	rows, err := pool.Query(ctx, `
		SELECT currency,
		       COALESCE(SUM(debit_minor), 0) - COALESCE(SUM(credit_minor), 0) AS net
		FROM journal_entries
		GROUP BY currency
		ORDER BY currency`)
	if err != nil {
		return nil, fmt.Errorf("ledger: trial balance: %w", err)
	}
	defer rows.Close()

	out := make(map[string]money.Amount)
	for rows.Next() {
		var code string
		var net int64
		if err := rows.Scan(&code, &net); err != nil {
			return nil, fmt.Errorf("ledger: scan trial balance: %w", err)
		}
		cur, ok := money.LookupCurrency(code)
		if !ok {
			cur, err = money.NewCurrency(code, 2)
			if err != nil {
				return nil, fmt.Errorf("ledger: unknown currency %q in ledger: %w", code, err)
			}
		}
		out[code] = money.New(net, cur)
	}
	return out, rows.Err()
}

// nullableUUID turns an empty attribution into SQL NULL so the foreign key does
// not reject a posting that legitimately concerns no customer (a manual
// adjustment, for example).
func nullableUUID(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// samePosting verifies that an existing journal transaction has exactly the
// entries the caller is trying to post.
func samePosting(ctx context.Context, tx pgx.Tx, transactionID string, p Posting) error {
	var currency string
	if err := tx.QueryRow(ctx,
		`SELECT currency FROM journal_transactions WHERE id = $1`, transactionID).
		Scan(&currency); err != nil {
		return fmt.Errorf("ledger: read existing posting: %w", err)
	}
	if currency != p.Currency.Code() {
		return fmt.Errorf("%w: existing posting is in %s, this one is %s",
			ErrReferenceReused, currency, p.Currency.Code())
	}

	type row struct {
		account string
		debit   int64
		credit  int64
	}
	want := make([]row, 0, len(p.Debits)+len(p.Credits))
	for _, e := range p.Debits {
		want = append(want, row{e.Account, e.Amount.Minor(), 0})
	}
	for _, e := range p.Credits {
		want = append(want, row{e.Account, 0, e.Amount.Minor()})
	}

	rows, err := tx.Query(ctx, `
		SELECT account_code, debit_minor, credit_minor
		FROM journal_entries WHERE transaction_id = $1
		ORDER BY account_code, debit_minor, credit_minor`, transactionID)
	if err != nil {
		return fmt.Errorf("ledger: read existing entries: %w", err)
	}
	defer rows.Close()

	got := make([]row, 0, len(want))
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.account, &r.debit, &r.credit); err != nil {
			return fmt.Errorf("ledger: scan existing entry: %w", err)
		}
		got = append(got, r)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("ledger: read existing entries: %w", err)
	}

	// Compare as multisets: the insertion order of entries is not part of the
	// posting's identity.
	sortRows := func(rs []row) {
		sort.Slice(rs, func(i, j int) bool {
			if rs[i].account != rs[j].account {
				return rs[i].account < rs[j].account
			}
			if rs[i].debit != rs[j].debit {
				return rs[i].debit < rs[j].debit
			}
			return rs[i].credit < rs[j].credit
		})
	}
	sortRows(want)
	sortRows(got)

	if len(want) != len(got) {
		return fmt.Errorf("%w: existing posting has %d entries, this one has %d",
			ErrReferenceReused, len(got), len(want))
	}
	for i := range want {
		if want[i] != got[i] {
			return fmt.Errorf("%w: entry %d is %s/%d/%d, this posting wants %s/%d/%d",
				ErrReferenceReused, i,
				got[i].account, got[i].debit, got[i].credit,
				want[i].account, want[i].debit, want[i].credit)
		}
	}
	return nil
}
