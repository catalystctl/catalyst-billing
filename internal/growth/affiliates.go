package growth

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/catalyst/billing/internal/money"
	"github.com/catalyst/billing/internal/store"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Affiliate attribution, commission and payout.
//
// The shape of the data is migration 0024's decision (one referrals table with two row
// shapes, two partial unique indexes). This file is the only writer of those rows, and
// every write that could move money is expressed so that the database, not the code, is
// what refuses a duplicate.

var (
	codePattern = regexp.MustCompile(`^[A-Z0-9][A-Z0-9_-]{2,31}$`)
	uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-` +
		`[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
)

// payoutMethods is the closed set the table's CHECK enforces. Validating here as well
// turns a bad request into ErrInvalidInput instead of a 500 from a constraint violation.
var payoutMethods = map[string]struct{}{
	"credit":        {},
	"paypal":        {},
	"bank_transfer": {},
	"manual":        {},
}

// ValidID reports whether s looks like a UUID, so a malformed path parameter is a 404
// rather than a database error.
func ValidID(s string) bool { return uuidPattern.MatchString(s) }

// NewAffiliate is the input to Create.
type NewAffiliate struct {
	UserID         string
	Code           string
	CommissionRate string
	PayoutMethod   string
}

// Create registers a customer as an affiliate.
//
// It does not create the user: the affiliate is an existing customer who may refer
// others, so a missing user is a foreign-key error the handler maps to 422 rather than a
// silently invented account.
func (s *Store) Create(ctx context.Context, tx pgx.Tx, in NewAffiliate) (Affiliate, error) {
	if !ValidID(in.UserID) {
		return Affiliate{}, fmt.Errorf("%w: user_id must be a uuid", ErrInvalidInput)
	}
	code := NormalizeCode(in.Code)
	if !codePattern.MatchString(code) {
		return Affiliate{}, fmt.Errorf("%w: code must be 3-32 characters of A-Z, 0-9, _ or -",
			ErrInvalidInput)
	}
	_, canonical, err := ParseRate(in.CommissionRate)
	if err != nil {
		return Affiliate{}, err
	}
	method := strings.TrimSpace(in.PayoutMethod)
	if method == "" {
		method = "credit"
	}
	if _, ok := payoutMethods[method]; !ok {
		return Affiliate{}, fmt.Errorf("%w: unknown payout method %q", ErrInvalidInput, method)
	}

	var id string
	err = tx.QueryRow(ctx, `
		INSERT INTO affiliates (user_id, code, commission_rate, payout_method)
		VALUES ($1, $2, $3, $4)
		RETURNING id`, in.UserID, code, canonical, method).Scan(&id)
	if err != nil {
		if store.IsUniqueViolation(err) {
			// One code per affiliate and one affiliate per user are both unique indexes;
			// either conflict means "this code or this person is already registered".
			return Affiliate{}, fmt.Errorf("%w: %s", ErrCodeTaken, code)
		}
		return Affiliate{}, fmt.Errorf("growth: create affiliate: %w", err)
	}
	return s.byID(ctx, tx, id)
}

// ByUserID resolves the affiliate for a signed-in user. A user who is not an affiliate
// is ErrNotFound, which the portal endpoint turns into "you are not an affiliate" rather
// than an error.
func (s *Store) ByUserID(ctx context.Context, userID string) (Affiliate, error) {
	if !ValidID(userID) {
		return Affiliate{}, ErrNotFound
	}
	var id string
	err := s.db().QueryRow(ctx, `SELECT id FROM affiliates WHERE user_id = $1`, userID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return Affiliate{}, ErrNotFound
	}
	if err != nil {
		return Affiliate{}, fmt.Errorf("growth: load affiliate: %w", err)
	}
	return s.byID(ctx, s.db(), id)
}

// Get loads one affiliate by id.
func (s *Store) Get(ctx context.Context, id string) (Affiliate, error) {
	if !ValidID(id) {
		return Affiliate{}, ErrNotFound
	}
	return s.byID(ctx, s.db(), id)
}

// List returns every affiliate, newest first, with balances and referral counts.
func (s *Store) List(ctx context.Context) ([]Affiliate, error) {
	rows, err := s.db().Query(ctx, `
		SELECT a.id, a.user_id, coalesce(u.email, ''), a.code, a.commission_rate,
		       a.payout_method, a.enabled, a.created_at,
		       (SELECT count(*) FROM affiliate_referrals r
		         WHERE r.affiliate_id = a.id AND r.order_id IS NULL)
		FROM affiliates a
		LEFT JOIN users u ON u.id = a.user_id
		ORDER BY a.created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("growth: list affiliates: %w", err)
	}
	defer rows.Close()

	out := make([]Affiliate, 0, 16)
	index := make(map[string]int, 16)
	for rows.Next() {
		a, err := scanAffiliate(rows)
		if err != nil {
			return nil, err
		}
		index[a.ID] = len(out)
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("growth: list affiliates: %w", err)
	}
	if len(out) == 0 {
		return out, nil
	}
	if err := s.attachBalances(ctx, out, index); err != nil {
		return nil, err
	}
	return out, nil
}

// byID loads one affiliate and its balances.
func (s *Store) byID(ctx context.Context, q Querier, id string) (Affiliate, error) {
	a, err := scanAffiliate(q.QueryRow(ctx, `
		SELECT a.id, a.user_id, coalesce(u.email, ''), a.code, a.commission_rate,
		       a.payout_method, a.enabled, a.created_at,
		       (SELECT count(*) FROM affiliate_referrals r
		         WHERE r.affiliate_id = a.id AND r.order_id IS NULL)
		FROM affiliates a
		LEFT JOIN users u ON u.id = a.user_id
		WHERE a.id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Affiliate{}, ErrNotFound
	}
	if err != nil {
		return Affiliate{}, err
	}
	items := []Affiliate{a}
	if err := s.attachBalances(ctx, items, map[string]int{a.ID: 0}); err != nil {
		return Affiliate{}, err
	}
	return items[0], nil
}

// rowScanner is satisfied by both pgx.Row and pgx.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

// scanAffiliate reads the shared affiliate projection.
func scanAffiliate(row rowScanner) (Affiliate, error) {
	var (
		a         Affiliate
		createdAt time.Time
	)
	if err := row.Scan(&a.ID, &a.UserID, &a.Email, &a.Code, &a.CommissionRate,
		&a.PayoutMethod, &a.Enabled, &createdAt, &a.ReferredCount); err != nil {
		return Affiliate{}, err
	}
	a.CreatedAt = createdAt.UTC().Format(time.RFC3339)
	return a, nil
}

// attachBalances fills Balances for the given affiliates with one query.
func (s *Store) attachBalances(ctx context.Context, items []Affiliate, index map[string]int) error {
	ids := make([]string, 0, len(items))
	for _, a := range items {
		ids = append(ids, a.ID)
	}
	rows, err := s.db().Query(ctx, `
		SELECT affiliate_id, currency, balance_minor, paid_minor
		FROM affiliate_balances WHERE affiliate_id = ANY($1::uuid[])
		ORDER BY currency`, ids)
	if err != nil {
		return fmt.Errorf("growth: load balances: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var affiliateID, currency string
		var owed, paid int64
		if err := rows.Scan(&affiliateID, &currency, &owed, &paid); err != nil {
			return fmt.Errorf("growth: scan balance: %w", err)
		}
		i, ok := index[affiliateID]
		if !ok {
			continue
		}
		cur, err := currencyOf(currency)
		if err != nil {
			return err
		}
		items[i].Balances = append(items[i].Balances, Balance{
			Currency: currency,
			Owed:     money.New(owed, cur),
			Paid:     money.New(paid, cur),
		})
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("growth: load balances: %w", err)
	}
	return nil
}

// Referrals returns an affiliate's referral rows, newest first. Attribution rows (no
// order) and commission rows are both included: the portal screen shows "you referred X"
// and "you earned Y" from one list.
func (s *Store) Referrals(ctx context.Context, affiliateID string, limit int) ([]Referral, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.db().Query(ctx, `
		SELECT id, affiliate_id, customer_id, coalesce(order_id::text, ''), commission_minor,
		       currency, commission_rate, status, attributed_at, paid_at
		FROM affiliate_referrals
		WHERE affiliate_id = $1
		ORDER BY created_at DESC
		LIMIT $2`, affiliateID, limit)
	if err != nil {
		return nil, fmt.Errorf("growth: list referrals: %w", err)
	}
	defer rows.Close()

	out := make([]Referral, 0, limit)
	for rows.Next() {
		var (
			r            Referral
			attributedAt time.Time
			paidAt       *time.Time
			minor        int64
		)
		if err := rows.Scan(&r.ID, &r.AffiliateID, &r.CustomerID, &r.OrderID, &minor,
			&r.Currency, &r.Rate, &r.Status, &attributedAt, &paidAt); err != nil {
			return nil, fmt.Errorf("growth: scan referral: %w", err)
		}
		cur, err := currencyOf(r.Currency)
		if err != nil {
			return nil, err
		}
		r.Commission = money.New(minor, cur)
		r.AttributedAt = attributedAt.UTC().Format(time.RFC3339)
		if paidAt != nil {
			r.PaidAt = paidAt.UTC().Format(time.RFC3339)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("growth: list referrals: %w", err)
	}
	return out, nil
}

// AttributeSignup records that the customer signing up now was referred by code.
//
// It is the function the registration flow calls, inside the transaction that created
// the customer. Three refusals make the attribution trustworthy:
//
//   - The customer must have been created in *this* transaction (created_at equals the
//     transaction timestamp). A customer that already existed is ErrExistingAccount: an
//     existing account is never silently absorbed into a referral, because that would pay
//     an affiliate for a customer they did not bring, and would move an established
//     customer's future orders to that affiliate.
//   - An affiliate cannot refer their own account (ErrSelfReferral).
//   - A customer who already has an attribution keeps it. Re-presenting the *same* code
//     is idempotent (Duplicate); presenting a *different* code is ErrAlreadyAttributed,
//     because silently re-pointing the attribution moves money between affiliates.
func AttributeSignup(ctx context.Context, tx pgx.Tx, code, customerID string) (Referral, error) {
	if tx == nil {
		return Referral{}, fmt.Errorf("%w: attribution needs a transaction", ErrInvalidInput)
	}
	code = NormalizeCode(code)
	if code == "" {
		return Referral{}, fmt.Errorf("%w: empty referral code", ErrInvalidInput)
	}
	if !ValidID(customerID) {
		return Referral{}, ErrNotFound
	}

	affiliateID, err := affiliateForCode(ctx, tx, code)
	if err != nil {
		return Referral{}, err
	}

	// The customer must exist, and must be new in this transaction. `now()` is the
	// transaction start time, so a row inserted by this transaction has exactly that
	// created_at; any earlier transaction's row is strictly older.
	var isNew bool
	err = tx.QueryRow(ctx, `
		SELECT created_at = now() FROM customers WHERE id = $1 FOR SHARE`, customerID,
	).Scan(&isNew)
	if errors.Is(err, pgx.ErrNoRows) {
		return Referral{}, ErrNotFound
	}
	if err != nil {
		return Referral{}, fmt.Errorf("growth: load customer: %w", err)
	}
	if !isNew {
		return Referral{}, fmt.Errorf("%w: customer %s", ErrExistingAccount, customerID)
	}

	// Self-referral: the affiliate's own user is the owner of this customer account.
	var self bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM affiliates a
			JOIN customer_accounts ca ON ca.user_id = a.user_id
			WHERE a.id = $1 AND ca.customer_id = $2)`, affiliateID, customerID).Scan(&self); err != nil {
		return Referral{}, fmt.Errorf("growth: check self referral: %w", err)
	}
	if self {
		return Referral{}, ErrSelfReferral
	}

	// The attribution row. The partial unique index on (customer_id) WHERE order_id IS
	// NULL is what makes a second attribution impossible.
	var out Referral
	err = tx.QueryRow(ctx, `
		INSERT INTO affiliate_referrals (affiliate_id, customer_id, currency, status)
		VALUES ($1, $2, 'USD', 'pending')
		ON CONFLICT (customer_id) WHERE order_id IS NULL DO NOTHING
		RETURNING id, affiliate_id, customer_id, currency, status`,
		affiliateID, customerID).Scan(&out.ID, &out.AffiliateID, &out.CustomerID,
		&out.Currency, &out.Status)
	if errors.Is(err, pgx.ErrNoRows) {
		// Already attributed: same code is a replay, a different code is a conflict.
		var existingAffiliate string
		if err := tx.QueryRow(ctx, `
			SELECT affiliate_id FROM affiliate_referrals
			WHERE customer_id = $1 AND order_id IS NULL`, customerID).Scan(&existingAffiliate); err != nil {
			return Referral{}, fmt.Errorf("growth: load attribution: %w", err)
		}
		if existingAffiliate != affiliateID {
			return Referral{}, fmt.Errorf("%w: customer %s", ErrAlreadyAttributed, customerID)
		}
		return Referral{
			AffiliateID: affiliateID,
			CustomerID:  customerID,
			Currency:    "USD",
			Status:      "pending",
			Duplicate:   true,
		}, nil
	}
	if err != nil {
		return Referral{}, fmt.Errorf("growth: attribute signup: %w", err)
	}
	return out, nil
}

// affiliateForCode resolves an enabled affiliate by code, or ErrUnknownCode.
func affiliateForCode(ctx context.Context, q Querier, code string) (string, error) {
	var id string
	var enabled bool
	err := q.QueryRow(ctx,
		`SELECT id, enabled FROM affiliates WHERE code = $1`, NormalizeCode(code)).Scan(&id, &enabled)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrUnknownCode
	}
	if err != nil {
		return "", fmt.Errorf("growth: resolve referral code: %w", err)
	}
	if !enabled {
		return "", ErrUnknownCode
	}
	return id, nil
}

// ErrNotReferred is the no-op outcome of a commission call: the customer who paid was not
// referred, so there is nothing to record. Callers on an event path treat it as success
// and log nothing; it is a sentinel rather than nil so a caller that expected a
// commission can tell the two apart.
var ErrNotReferred = errors.New("growth: the customer was not referred")

// RecordCommission records the commission for a paid order.
//
// It is idempotent: the unique index on (order_id) WHERE order_id IS NOT NULL makes a
// replayed "order paid" event insert nothing, and the call returns the existing row with
// Duplicate set. The balance is incremented only on a genuine insert, so a replay cannot
// inflate it either.
func RecordCommission(ctx context.Context, tx pgx.Tx, orderID string) (Referral, error) {
	if tx == nil {
		return Referral{}, fmt.Errorf("%w: commission needs a transaction", ErrInvalidInput)
	}
	if !ValidID(orderID) {
		return Referral{}, ErrNotFound
	}

	var (
		customerID string
		status     string
		currency   string
		totalMinor int64
	)
	err := tx.QueryRow(ctx, `
		SELECT customer_id, status, currency, total_minor
		FROM orders WHERE id = $1 FOR SHARE`, orderID,
	).Scan(&customerID, &status, &currency, &totalMinor)
	if errors.Is(err, pgx.ErrNoRows) {
		return Referral{}, ErrNotFound
	}
	if err != nil {
		return Referral{}, fmt.Errorf("growth: load order: %w", err)
	}
	if status != "paid" {
		// A commission on an unpaid order is a promise the books do not support.
		return Referral{}, fmt.Errorf("%w: order %s is %s, not paid", ErrInvalidInput, orderID, status)
	}
	return recordCommission(ctx, tx, customerID, orderID, currency, totalMinor)
}

// RecordCommissionForPaidInvoice is the hook the payment path calls.
//
// It reads the invoice rather than the order because the invoice is the document that
// was actually settled: an order whose total was adjusted before issue must pay
// commission on what the customer paid, not on the draft total. The invoice must be paid
// and must have come from an order; anything else is ErrNotReferred (nothing to do).
func RecordCommissionForPaidInvoice(ctx context.Context, tx pgx.Tx, invoiceID string) (Referral, error) {
	if tx == nil {
		return Referral{}, fmt.Errorf("%w: commission needs a transaction", ErrInvalidInput)
	}
	if !ValidID(invoiceID) {
		return Referral{}, ErrNotFound
	}
	var (
		orderID    *string
		customerID string
		status     string
		currency   string
		totalMinor int64
	)
	err := tx.QueryRow(ctx, `
		SELECT order_id, customer_id, status, currency, total_minor
		FROM invoices WHERE id = $1 FOR SHARE`, invoiceID,
	).Scan(&orderID, &customerID, &status, &currency, &totalMinor)
	if errors.Is(err, pgx.ErrNoRows) {
		return Referral{}, ErrNotFound
	}
	if err != nil {
		return Referral{}, fmt.Errorf("growth: load invoice: %w", err)
	}
	if status != "paid" || orderID == nil {
		// A renewal invoice has no order: there is no signup to attribute it to, and a
		// recurring commission on a renewal is a policy decision this package does not
		// make silently. (GRW-03 recurring commission is left open; see the report.)
		return Referral{}, ErrNotReferred
	}
	return recordCommission(ctx, tx, customerID, *orderID, currency, totalMinor)
}

// recordCommission is the shared body of both entry points.
func recordCommission(ctx context.Context, tx pgx.Tx, customerID, orderID, currency string, totalMinor int64) (Referral, error) {
	var affiliateID string
	err := tx.QueryRow(ctx, `
		SELECT affiliate_id FROM affiliate_referrals
		WHERE customer_id = $1 AND order_id IS NULL`, customerID).Scan(&affiliateID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Referral{}, ErrNotReferred
	}
	if err != nil {
		return Referral{}, fmt.Errorf("growth: load attribution: %w", err)
	}

	var rateText string
	var enabled bool
	err = tx.QueryRow(ctx,
		`SELECT commission_rate, enabled FROM affiliates WHERE id = $1`, affiliateID,
	).Scan(&rateText, &enabled)
	if errors.Is(err, pgx.ErrNoRows) {
		return Referral{}, ErrNotReferred
	}
	if err != nil {
		return Referral{}, fmt.Errorf("growth: load affiliate: %w", err)
	}
	if !enabled {
		return Referral{}, ErrAffiliateDisabled
	}

	_, canonical, err := ParseRate(rateText)
	if err != nil {
		return Referral{}, err
	}
	cur, err := currencyOf(currency)
	if err != nil {
		return Referral{}, err
	}
	// Exact rational arithmetic; rounding happens once, here, on the exact product.
	commission, err := Commission(money.New(totalMinor, cur), canonical)
	if err != nil {
		return Referral{}, err
	}

	out := Referral{
		AffiliateID: affiliateID,
		CustomerID:  customerID,
		OrderID:     orderID,
		Commission:  commission,
		Currency:    currency,
		Rate:        canonical,
		Status:      "pending",
	}
	var minor int64
	err = tx.QueryRow(ctx, `
		INSERT INTO affiliate_referrals
			(affiliate_id, customer_id, order_id, commission_minor, currency, status, commission_rate)
		VALUES ($1, $2, $3, $4, $5, 'pending', $6)
		ON CONFLICT (order_id) WHERE order_id IS NOT NULL DO NOTHING
		RETURNING id`,
		affiliateID, customerID, orderID, commission.Minor(), currency, canonical).Scan(&out.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		// Replay: the commission for this order already exists. Return it unchanged, and
		// do not touch the balance.
		out.Duplicate = true
		if err := tx.QueryRow(ctx, `
			SELECT id, commission_minor, commission_rate, status
			FROM affiliate_referrals WHERE order_id = $1`, orderID,
		).Scan(&out.ID, &minor, &out.Rate, &out.Status); err != nil {
			return Referral{}, fmt.Errorf("growth: load existing commission: %w", err)
		}
		out.Commission = money.New(minor, cur)
		return out, nil
	}
	if err != nil {
		return Referral{}, fmt.Errorf("growth: record commission: %w", err)
	}

	// The materialised payable, incremented in the same transaction as the row it
	// summarises. A zero commission creates no balance row: nothing is owed.
	if commission.IsPositive() {
		if _, err := tx.Exec(ctx, `
			INSERT INTO affiliate_balances (affiliate_id, currency, balance_minor)
			VALUES ($1, $2, $3)
			ON CONFLICT (affiliate_id, currency) DO UPDATE
				SET balance_minor = affiliate_balances.balance_minor + EXCLUDED.balance_minor,
				    updated_at = now()`,
			affiliateID, currency, commission.Minor()); err != nil {
			return Referral{}, fmt.Errorf("growth: update affiliate balance: %w", err)
		}
	}
	return out, nil
}

// PayoutReport lists every affiliate with an outstanding balance, per currency.
//
// It reads the materialised balance rather than summing commissions, because a payout
// run must be cheap and must agree with what the payout action will pay. The pending
// commission count is included so a reconciler can compare the summary with the rows.
func (s *Store) PayoutReport(ctx context.Context) ([]PayoutLine, error) {
	rows, err := s.db().Query(ctx, `
		SELECT b.affiliate_id, a.code, a.user_id, coalesce(u.email, ''), b.currency,
		       b.balance_minor, b.paid_minor,
		       (SELECT count(*) FROM affiliate_referrals r
		         WHERE r.affiliate_id = b.affiliate_id
		           AND r.currency = b.currency
		           AND r.order_id IS NOT NULL
		           AND r.status IN ('pending', 'approved'))
		FROM affiliate_balances b
		JOIN affiliates a ON a.id = b.affiliate_id
		LEFT JOIN users u ON u.id = a.user_id
		WHERE b.balance_minor > 0
		ORDER BY b.currency, a.code`)
	if err != nil {
		return nil, fmt.Errorf("growth: payout report: %w", err)
	}
	defer rows.Close()

	out := make([]PayoutLine, 0, 16)
	for rows.Next() {
		var (
			line           PayoutLine
			owed, lifetime int64
		)
		if err := rows.Scan(&line.AffiliateID, &line.Code, &line.UserID, &line.Email,
			&line.Currency, &owed, &lifetime, &line.PendingCommissions); err != nil {
			return nil, fmt.Errorf("growth: scan payout line: %w", err)
		}
		cur, err := currencyOf(line.Currency)
		if err != nil {
			return nil, err
		}
		line.Owed = money.New(owed, cur)
		line.LifetimePaid = money.New(lifetime, cur)
		out = append(out, line)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("growth: payout report: %w", err)
	}
	return out, nil
}

// PayOut settles one affiliate's outstanding balance in one currency.
//
// The whole settlement is one transaction: the referral rows move to paid, the balance
// moves to paid_minor, and the audit row (written by the caller) names the operator. A
// second call finds a zero balance and returns ErrNothingToPay, so a double-click pays
// once even without the HTTP idempotency layer.
func (s *Store) PayOut(ctx context.Context, affiliateID, currency string) (PaidOut, error) {
	if !ValidID(affiliateID) {
		return PaidOut{}, ErrNotFound
	}
	var out PaidOut
	err := s.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var owed int64
		err := tx.QueryRow(ctx, `
			SELECT balance_minor FROM affiliate_balances
			WHERE affiliate_id = $1 AND currency = $2 FOR UPDATE`,
			affiliateID, currency).Scan(&owed)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNothingToPay
		}
		if err != nil {
			return fmt.Errorf("growth: lock balance: %w", err)
		}
		if owed <= 0 {
			return ErrNothingToPay
		}
		cur, err := currencyOf(currency)
		if err != nil {
			return err
		}

		tag, err := tx.Exec(ctx, `
			UPDATE affiliate_referrals
			SET status = 'paid', paid_at = now()
			WHERE affiliate_id = $1 AND currency = $2
			  AND order_id IS NOT NULL AND status IN ('pending', 'approved')`,
			affiliateID, currency)
		if err != nil {
			return fmt.Errorf("growth: mark commissions paid: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE affiliate_balances
			SET balance_minor = 0, paid_minor = paid_minor + $3, updated_at = now()
			WHERE affiliate_id = $1 AND currency = $2`,
			affiliateID, currency, owed); err != nil {
			return fmt.Errorf("growth: settle balance: %w", err)
		}
		out = PaidOut{
			AffiliateID: affiliateID,
			Currency:    currency,
			Amount:      money.New(owed, cur),
			Commissions: int(tag.RowsAffected()),
		}
		return nil
	})
	if err != nil {
		return PaidOut{}, err
	}
	return out, nil
}

// db returns the pool as a Querier. A Store built with a nil pool (the pure-computation
// tests) yields a failing Querier, so a read refuses with ErrNotFound instead of
// panicking on a nil pointer.
func (s *Store) db() Querier {
	if s.pool == nil {
		return nilQuerier{}
	}
	return s.pool
}

// nilQuerier fails every call with ErrNotFound.
type nilQuerier struct{}

func (nilQuerier) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, ErrNotFound
}
func (nilQuerier) QueryRow(context.Context, string, ...any) pgx.Row { return nilRow{} }
func (nilQuerier) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, ErrNotFound
}

// nilRow fails every Scan with ErrNotFound.
type nilRow struct{}

func (nilRow) Scan(...any) error { return ErrNotFound }
