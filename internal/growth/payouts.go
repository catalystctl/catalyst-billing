package growth

// Affiliate payouts: the report an operator reconciles, and the action that settles it.
//
// The rule this file exists to enforce is that a payout is a *record*, not an edit. Before
// it, paying an affiliate flipped every commission row to `paid` and zeroed the materialised
// balance; the result was a number nobody could explain, because nothing said which payment
// settled which commission. Now:
//
//   - `affiliate_payouts` is an append-only row per settlement: how much, in what currency,
//     by what method, against what reference, for what period, by whom.
//   - Every commission row carries `payout_id`, the payout that settled it. The amount on a
//     commission is never rewritten by a payout; only the fact that it was settled, and by
//     what, is written.
//   - The report states three figures and checks them against each other: earned (the sum of
//     every commission), paid (the sum of every payout), and outstanding (the sum of the
//     commission rows no payout has claimed). `earned - paid == outstanding` is the
//     reconciliation, and the report carries the rows that make it up so an operator can
//     read the difference instead of recomputing it.
//   - The payout refuses to run when the materialised balance and the commission rows
//     disagree. That is the one condition under which paying would make the books worse, so
//     it is a refusal with a code rather than a silent overwrite.
//
// Money is integer minor units throughout, and a currency is never mixed with another: an
// affiliate with earnings in USD and EUR has two balances, two reports and two payouts,
// because there is no exchange rate anywhere in this system.

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/catalyst/billing/internal/money"
	"github.com/jackc/pgx/v5"
)

// ErrBalanceMismatch is returned by a payout when the materialised balance and the commission
// rows it summarises disagree.
//
// It is a refusal, not a warning: paying a balance the rows do not support would move money
// nobody can account for afterwards, and silently rewriting the balance to the row total
// would hide the defect that produced the disagreement. The report shows both figures, so an
// operator can see which row is wrong.
var ErrBalanceMismatch = errors.New("growth: the affiliate balance disagrees with its commission rows")

// currencyPattern is the CHECK the tables apply. Validating here turns a bad request into a
// 422 instead of a 500 from a constraint violation.
var currencyPattern = regexp.MustCompile(`^[A-Z0-9]{2,12}$`)

// payoutRowLimit bounds the rows a report returns. A report is a reconciliation, not an
// export: an operator reads the window they are paying for, and the totals cover all of it.
const payoutRowLimit = 500

// PayoutRowKind names what one report row is.
const (
	// PayoutRowCommission is a commission earned on an order (the signup commission).
	PayoutRowCommission = "commission"
	// PayoutRowRenewal is a commission earned on a renewal invoice (recurring commission).
	PayoutRowRenewal = "renewal"
	// PayoutRowPayout is money that left: one settlement.
	PayoutRowPayout = "payout"
)

// PayoutRow is one row that makes up the difference between earned and paid.
type PayoutRow struct {
	Kind string
	ID   string
	// InvoiceID is set on a renewal commission; OrderID on an order commission. Both are
	// empty on a payout row, which is not about an invoice at all.
	InvoiceID string
	OrderID   string
	Amount    money.Amount
	// Status is the commission's status (`pending`, `approved`, `paid`) or `recorded` for a
	// payout row, which has no lifecycle of its own.
	Status string
	// Rate is the canonical rational captured on the commission row.
	Rate string
	// OccurredAt is when the row came into existence: earned_at for a commission,
	// created_at for a payout.
	OccurredAt string
	PaidAt     string
	// PayoutID is the payout that settled a commission, empty while it is still owed.
	PayoutID string
	// RenewalIndex is 1 for the first renewal of a referred customer, 0 for anything else.
	RenewalIndex int
}

// PayoutReconciliation is one affiliate's books in one currency.
//
// Earned minus Paid must equal Outstanding, and Outstanding must equal the sum of the
// commission rows no payout has claimed. `Reconciles` reports whether both held when the
// report was read; a false there is a defect in the data, not in the report.
type PayoutReconciliation struct {
	AffiliateID string
	Code        string
	Email       string
	Currency    string
	// Earned is every commission ever recorded for this affiliate in this currency.
	Earned money.Amount
	// Paid is every payout ever recorded.
	Paid money.Amount
	// Outstanding is what a payout would settle now: every commission that is neither
	// settled by a payout nor already marked paid.
	Outstanding money.Amount
	// Payouts is how many settlements have been recorded.
	Payouts int
	// Rows are the commission and payout rows inside the reporting period, newest first.
	Rows []PayoutRow
	// From and To bound the rows (not the totals).
	From time.Time
	To   time.Time
	// Reconciles is `Earned - Paid == Outstanding` for the totals.
	Reconciles bool
}

// NewPayout is the input to RecordPayout.
type NewPayout struct {
	AffiliateID string
	Currency    string
	// Method defaults to the affiliate's configured payout method when empty.
	Method string
	// Reference is a provider or bank reference, quoted when reconciling a statement.
	Reference string
	Note      string
	// ActorID is the operator recording the payout. It is nullable in the schema so a
	// system-driven settlement can exist, but every operator action sets it.
	ActorID string
	// PeriodFrom and PeriodTo record which window the operator was looking at. They do not
	// filter what is settled: a payout settles the whole outstanding balance, because a
	// commission is not divisible.
	PeriodFrom *time.Time
	PeriodTo   *time.Time
}

// RecordedPayout is the outcome of a settlement.
type RecordedPayout struct {
	ID          string
	AffiliateID string
	Currency    string
	Amount      money.Amount
	Method      string
	Reference   string
	// Commissions is how many commission rows the payout settled, across both kinds.
	Commissions int
	// CommissionRows is how many of those were order commissions and how many were
	// renewals, so a caller can say what it paid without a second query.
	OrderCommissions   int
	RenewalCommissions int
	CreatedAt          string
}

// Payout is one recorded settlement, for the history list.
type Payout struct {
	ID          string
	AffiliateID string
	Currency    string
	Amount      money.Amount
	Method      string
	Reference   string
	Note        string
	PeriodFrom  string
	PeriodTo    string
	CreatedBy   string
	CreatedAt   string
}

// ------------------------------------------------------------------ the report --

// Reconcile builds the payout report for one affiliate in one currency.
func (s *Store) Reconcile(ctx context.Context, affiliateID, currency string, from, to time.Time) (PayoutReconciliation, error) {
	if !ValidID(affiliateID) {
		return PayoutReconciliation{}, ErrNotFound
	}
	currency = strings.ToUpper(strings.TrimSpace(currency))
	if !currencyPattern.MatchString(currency) {
		return PayoutReconciliation{}, fmt.Errorf("%w: currency %q is not a currency code",
			ErrInvalidInput, currency)
	}
	cur, err := currencyOf(currency)
	if err != nil {
		return PayoutReconciliation{}, err
	}

	out := PayoutReconciliation{
		AffiliateID: affiliateID,
		Currency:    currency,
		From:        from,
		To:          to,
		Earned:      money.New(0, cur),
		Paid:        money.New(0, cur),
		Outstanding: money.New(0, cur),
		Rows:        []PayoutRow{},
	}
	if err := s.db().QueryRow(ctx, `
		SELECT a.code, coalesce(u.email, '')
		FROM affiliates a LEFT JOIN users u ON u.id = a.user_id
		WHERE a.id = $1`, affiliateID).Scan(&out.Code, &out.Email); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return PayoutReconciliation{}, ErrNotFound
		}
		return PayoutReconciliation{}, fmt.Errorf("growth: load affiliate: %w", err)
	}

	// One statement, one snapshot: the three totals cannot be read between two commits and
	// disagree with each other. Each branch is an index range on (affiliate_id, currency),
	// and the settled ones are partial indexes, so this does not walk history.
	var (
		earnedMinor int64
		paidMinor   int64
		owedMinor   int64
		pay         int
	)
	if err := s.db().QueryRow(ctx, `
		SELECT
		  coalesce(sum(commission_minor), 0),
		  coalesce(sum(amount_minor), 0),
		  coalesce(sum(commission_minor) FILTER (WHERE kind <> 'payout'
		                                           AND payout_id IS NULL
		                                           AND status <> 'paid'), 0),
		  count(*) FILTER (WHERE kind = 'payout')
		FROM (
		    SELECT 'order'::text AS kind, commission_minor, payout_id, status,
		           0::bigint AS amount_minor
		    FROM affiliate_referrals
		    WHERE affiliate_id = $1 AND currency = $2 AND order_id IS NOT NULL
		    UNION ALL
		    SELECT 'renewal', commission_minor, payout_id, status, 0::bigint
		    FROM affiliate_recurring_commissions
		    WHERE affiliate_id = $1 AND currency = $2
		    UNION ALL
		    SELECT 'payout', 0::bigint, NULL::uuid, 'recorded'::text, amount_minor
		    FROM affiliate_payouts
		    WHERE affiliate_id = $1 AND currency = $2
		) t`, affiliateID, currency,
	).Scan(&earnedMinor, &paidMinor, &owedMinor, &pay); err != nil {
		return PayoutReconciliation{}, fmt.Errorf("growth: reconcile affiliate: %w", err)
	}

	out.Earned = money.New(earnedMinor, cur)
	out.Paid = money.New(paidMinor, cur)
	out.Outstanding = money.New(owedMinor, cur)
	out.Payouts = pay
	// The identity a reconciler checks: every cent earned is either owed or paid, so
	// `earned - paid == outstanding`. If it does not hold, one of the three figures is wrong
	// — a commission marked settled that no payout accounts for is the usual cause, which is
	// exactly the pre-0043 shape this migration reconstructs away — and the rows below are how
	// an operator finds out which.
	out.Reconciles = out.Earned.Minor()-out.Paid.Minor() == out.Outstanding.Minor()

	rows, err := s.reconcileRows(ctx, affiliateID, currency, cur, from, to)
	if err != nil {
		return PayoutReconciliation{}, err
	}
	out.Rows = rows
	return out, nil
}

// reconcileRows reads the rows inside the reporting period.
func (s *Store) reconcileRows(ctx context.Context, affiliateID, currency string, cur money.Currency, from, to time.Time) ([]PayoutRow, error) {
	rows, err := s.db().Query(ctx, `
		SELECT kind, id::text, coalesce(invoice_id::text, ''), coalesce(order_id::text, ''),
		       amount, status, rate, occurred_at, paid_at, coalesce(payout_id::text, ''),
		       renewal_index
		FROM (
		    SELECT 'commission'::text AS kind, r.id, NULL::uuid AS invoice_id, r.order_id,
		           r.commission_minor AS amount, r.status, r.commission_rate AS rate,
		           r.created_at AS occurred_at, r.paid_at, r.payout_id,
		           0 AS renewal_index
		    FROM affiliate_referrals r
		    WHERE r.affiliate_id = $1 AND r.currency = $2 AND r.order_id IS NOT NULL
		      AND r.created_at >= $3 AND r.created_at <= $4
		    UNION ALL
		    SELECT 'renewal', c.id, c.invoice_id, NULL::uuid,
		           c.commission_minor, c.status, c.commission_rate,
		           c.earned_at, c.paid_at, c.payout_id, c.renewal_index
		    FROM affiliate_recurring_commissions c
		    WHERE c.affiliate_id = $1 AND c.currency = $2
		      AND c.earned_at >= $3 AND c.earned_at <= $4
		    UNION ALL
		    SELECT 'payout', p.id, NULL::uuid, NULL::uuid,
		           p.amount_minor, 'recorded'::text, '',
		           p.created_at, NULL::timestamptz, NULL::uuid, 0
		    FROM affiliate_payouts p
		    WHERE p.affiliate_id = $1 AND p.currency = $2
		      AND p.created_at >= $3 AND p.created_at <= $4
		) t
		ORDER BY occurred_at DESC
		LIMIT $5`, affiliateID, currency, from, to, payoutRowLimit)
	if err != nil {
		return nil, fmt.Errorf("growth: read payout rows: %w", err)
	}
	defer rows.Close()

	out := make([]PayoutRow, 0, 16)
	for rows.Next() {
		var (
			row        PayoutRow
			minor      int64
			occurredAt time.Time
			paidAt     *time.Time
		)
		if err := rows.Scan(&row.Kind, &row.ID, &row.InvoiceID, &row.OrderID, &minor,
			&row.Status, &row.Rate, &occurredAt, &paidAt, &row.PayoutID,
			&row.RenewalIndex); err != nil {
			return nil, fmt.Errorf("growth: scan payout row: %w", err)
		}
		row.Amount = money.New(minor, cur)
		row.OccurredAt = occurredAt.UTC().Format(time.RFC3339)
		if paidAt != nil {
			row.PaidAt = paidAt.UTC().Format(time.RFC3339)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("growth: read payout rows: %w", err)
	}
	return out, nil
}

// ReconcileAll builds the report for every currency the affiliate has touched.
//
// The currencies come from the rows themselves rather than from the balance table: a
// commission of zero minor units creates no balance row, and a report that silently omitted
// it would be a report with a missing line.
func (s *Store) ReconcileAll(ctx context.Context, affiliateID string, from, to time.Time) ([]PayoutReconciliation, error) {
	if !ValidID(affiliateID) {
		return nil, ErrNotFound
	}
	// The affiliate must exist. Without this check an unknown id answers an empty report,
	// which reads as "this affiliate has earned nothing" — the same 200 for a typo and for a
	// payee with no earnings, and only one of those is true.
	var exists bool
	if err := s.db().QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM affiliates WHERE id = $1)`, affiliateID).Scan(&exists); err != nil {
		return nil, fmt.Errorf("growth: load affiliate: %w", err)
	}
	if !exists {
		return nil, ErrNotFound
	}
	rows, err := s.db().Query(ctx, `
		SELECT currency FROM (
		    SELECT currency FROM affiliate_referrals
		    WHERE affiliate_id = $1 AND order_id IS NOT NULL
		    UNION SELECT currency FROM affiliate_recurring_commissions WHERE affiliate_id = $1
		    UNION SELECT currency FROM affiliate_payouts WHERE affiliate_id = $1
		) t ORDER BY currency`, affiliateID)
	if err != nil {
		return nil, fmt.Errorf("growth: list payout currencies: %w", err)
	}
	defer rows.Close()

	var currencies []string
	for rows.Next() {
		var code string
		if err := rows.Scan(&code); err != nil {
			return nil, fmt.Errorf("growth: scan currency: %w", err)
		}
		currencies = append(currencies, code)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("growth: list payout currencies: %w", err)
	}

	out := make([]PayoutReconciliation, 0, len(currencies))
	for _, code := range currencies {
		line, err := s.Reconcile(ctx, affiliateID, code, from, to)
		if err != nil {
			return nil, err
		}
		out = append(out, line)
	}
	return out, nil
}

// Payouts returns an affiliate's recorded settlements, newest first.
func (s *Store) Payouts(ctx context.Context, affiliateID string, limit int) ([]Payout, error) {
	if !ValidID(affiliateID) {
		return nil, ErrNotFound
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.db().Query(ctx, `
		SELECT id::text, affiliate_id::text, currency, amount_minor, method, reference, note,
		       period_from, period_to, coalesce(created_by::text, ''), created_at
		FROM affiliate_payouts
		WHERE affiliate_id = $1
		ORDER BY created_at DESC
		LIMIT $2`, affiliateID, limit)
	if err != nil {
		return nil, fmt.Errorf("growth: list payouts: %w", err)
	}
	defer rows.Close()

	out := make([]Payout, 0, limit)
	for rows.Next() {
		var (
			p          Payout
			minor      int64
			createdAt  time.Time
			periodFrom *time.Time
			periodTo   *time.Time
		)
		if err := rows.Scan(&p.ID, &p.AffiliateID, &p.Currency, &minor, &p.Method,
			&p.Reference, &p.Note, &periodFrom, &periodTo, &p.CreatedBy,
			&createdAt); err != nil {
			return nil, fmt.Errorf("growth: scan payout: %w", err)
		}
		cur, err := currencyOf(p.Currency)
		if err != nil {
			return nil, err
		}
		p.Amount = money.New(minor, cur)
		p.CreatedAt = createdAt.UTC().Format(time.RFC3339)
		if periodFrom != nil {
			p.PeriodFrom = periodFrom.UTC().Format(time.RFC3339)
		}
		if periodTo != nil {
			p.PeriodTo = periodTo.UTC().Format(time.RFC3339)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("growth: list payouts: %w", err)
	}
	return out, nil
}

// ------------------------------------------------------------- the settlement --

// RecordPayout settles an affiliate's outstanding balance in one currency and records it.
//
// It takes a transaction because the payout row, the commission links and the audit row that
// names the operator must commit together: a payout nobody signed for is worse than no
// payout. The caller owns the transaction; this function owns the ordering inside it.
//
// What it does not do is rewrite a commission's amount. It writes one row saying money left,
// and sets `payout_id` on the rows that money settled — the same rows the report shows as
// outstanding, so the report after a payout reads earned = paid and outstanding = 0.
func (s *Store) RecordPayout(ctx context.Context, tx pgx.Tx, in NewPayout) (RecordedPayout, error) {
	if tx == nil {
		return RecordedPayout{}, fmt.Errorf("%w: a payout needs a transaction", ErrInvalidInput)
	}
	if !ValidID(in.AffiliateID) {
		return RecordedPayout{}, ErrNotFound
	}
	currency := strings.ToUpper(strings.TrimSpace(in.Currency))
	if !currencyPattern.MatchString(currency) {
		return RecordedPayout{}, fmt.Errorf("%w: currency %q is not a currency code",
			ErrInvalidInput, currency)
	}
	cur, err := currencyOf(currency)
	if err != nil {
		return RecordedPayout{}, err
	}

	// The balance is the first thing locked, so two operators paying the same affiliate at
	// the same time serialise here rather than both reading "owed" and both paying it.
	var owed int64
	var configuredMethod string
	err = tx.QueryRow(ctx, `
		SELECT b.balance_minor, a.payout_method
		FROM affiliate_balances b
		JOIN affiliates a ON a.id = b.affiliate_id
		WHERE b.affiliate_id = $1 AND b.currency = $2
		FOR UPDATE OF b`, in.AffiliateID, currency).Scan(&owed, &configuredMethod)
	if errors.Is(err, pgx.ErrNoRows) {
		return RecordedPayout{}, ErrNothingToPay
	}
	if err != nil {
		return RecordedPayout{}, fmt.Errorf("growth: lock balance: %w", err)
	}
	if owed <= 0 {
		return RecordedPayout{}, ErrNothingToPay
	}

	// The period is context, not a filter, so an incoherent one is refused rather than
	// silently dropped: it is what an auditor reads to understand why this payout happened
	// when it did.
	if (in.PeriodFrom == nil) != (in.PeriodTo == nil) {
		return RecordedPayout{}, fmt.Errorf("%w: a payout period needs both bounds", ErrInvalidInput)
	}
	if in.PeriodFrom != nil && !in.PeriodTo.After(*in.PeriodFrom) {
		return RecordedPayout{}, fmt.Errorf("%w: the payout period ends before it starts",
			ErrInvalidInput)
	}

	method := strings.TrimSpace(in.Method)
	if method == "" {
		method = configuredMethod
	}
	if _, ok := payoutMethods[method]; !ok {
		return RecordedPayout{}, fmt.Errorf("%w: unknown payout method %q", ErrInvalidInput, method)
	}

	// The rows being settled, locked so the links and the balance move together.
	orderIDs, orderMinor, err := lockCommissionRows(ctx, tx, `
		SELECT id, commission_minor FROM affiliate_referrals
		WHERE affiliate_id = $1 AND currency = $2 AND order_id IS NOT NULL
		  AND payout_id IS NULL AND status <> 'paid'
		ORDER BY created_at
		FOR UPDATE`, in.AffiliateID, currency)
	if err != nil {
		return RecordedPayout{}, err
	}
	renewalIDs, renewalMinor, err := lockCommissionRows(ctx, tx, `
		SELECT id, commission_minor FROM affiliate_recurring_commissions
		WHERE affiliate_id = $1 AND currency = $2
		  AND payout_id IS NULL AND status <> 'paid'
		ORDER BY earned_at
		FOR UPDATE`, in.AffiliateID, currency)
	if err != nil {
		return RecordedPayout{}, err
	}
	total := orderMinor + renewalMinor
	if total <= 0 {
		return RecordedPayout{}, ErrNothingToPay
	}
	if total != owed {
		// The materialised balance and the rows disagree. Paying either figure would make the
		// books worse; the report shows both, so this is the moment to stop and look.
		return RecordedPayout{}, fmt.Errorf("%w: balance %d, commission rows %d, currency %s",
			ErrBalanceMismatch, owed, total, currency)
	}

	var (
		payoutID  string
		createdAt time.Time
	)
	if err := tx.QueryRow(ctx, `
		INSERT INTO affiliate_payouts
			(affiliate_id, currency, amount_minor, method, reference, note,
			 period_from, period_to, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		RETURNING id, created_at`,
		in.AffiliateID, currency, total, method, strings.TrimSpace(in.Reference),
		strings.TrimSpace(in.Note), in.PeriodFrom, in.PeriodTo,
		nullableID(in.ActorID)).Scan(&payoutID, &createdAt); err != nil {
		return RecordedPayout{}, fmt.Errorf("growth: record payout: %w", err)
	}

	// The link, and the settled marker the tables' CHECKs require. The amount is untouched:
	// a payout records what a commission was worth, it does not restate it.
	if len(orderIDs) > 0 {
		if _, err := tx.Exec(ctx, `
			UPDATE affiliate_referrals
			SET payout_id = $2, status = 'paid', paid_at = now()
			WHERE id = ANY($1::uuid[])`, orderIDs, payoutID); err != nil {
			return RecordedPayout{}, fmt.Errorf("growth: settle order commissions: %w", err)
		}
	}
	if len(renewalIDs) > 0 {
		if _, err := tx.Exec(ctx, `
			UPDATE affiliate_recurring_commissions
			SET payout_id = $2, status = 'paid', paid_at = now()
			WHERE id = ANY($1::uuid[])`, renewalIDs, payoutID); err != nil {
			return RecordedPayout{}, fmt.Errorf("growth: settle renewal commissions: %w", err)
		}
	}

	if _, err := tx.Exec(ctx, `
		UPDATE affiliate_balances
		SET balance_minor = 0, paid_minor = paid_minor + $3, updated_at = now()
		WHERE affiliate_id = $1 AND currency = $2`,
		in.AffiliateID, currency, total); err != nil {
		return RecordedPayout{}, fmt.Errorf("growth: settle balance: %w", err)
	}

	return RecordedPayout{
		ID:                 payoutID,
		AffiliateID:        in.AffiliateID,
		Currency:           currency,
		Amount:             money.New(total, cur),
		Method:             method,
		Reference:          strings.TrimSpace(in.Reference),
		Commissions:        len(orderIDs) + len(renewalIDs),
		OrderCommissions:   len(orderIDs),
		RenewalCommissions: len(renewalIDs),
		CreatedAt:          createdAt.UTC().Format(time.RFC3339),
	}, nil
}

// lockCommissionRows runs a locking select and returns the ids and their total.
func lockCommissionRows(ctx context.Context, tx pgx.Tx, query, affiliateID, currency string) ([]string, int64, error) {
	rows, err := tx.Query(ctx, query, affiliateID, currency)
	if err != nil {
		return nil, 0, fmt.Errorf("growth: lock commission rows: %w", err)
	}
	defer rows.Close()

	var (
		ids   []string
		total int64
	)
	for rows.Next() {
		var (
			id    string
			minor int64
		)
		if err := rows.Scan(&id, &minor); err != nil {
			return nil, 0, fmt.Errorf("growth: scan commission row: %w", err)
		}
		ids = append(ids, id)
		total += minor
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("growth: lock commission rows: %w", err)
	}
	return ids, total, nil
}

// nullableID turns an empty id into NULL for a nullable uuid column.
func nullableID(id string) any {
	if id == "" {
		return nil
	}
	return id
}
