// Package refunds gives money back.
//
// A refund is the most dangerous operation in the product, because it is the one that turns a
// database row into money leaving an account, and because the provider call happens outside the
// transaction. The design is built around that:
//
//   - The refundable amount is computed under `SELECT ... FOR UPDATE` on the payment. Two
//     operators clicking "refund" at the same time must not each be told the full amount is
//     available, or the second click refunds money that is already gone.
//   - The refund row is written BEFORE the provider is called, with a stable operation key. That
//     row is the record that we asked; if the process dies mid-call, the row is what a reconciler
//     finds and resolves. A refund that exists only in a provider dashboard is a refund nobody can
//     reconcile.
//   - A provider timeout leaves the refund `unknown` and posts NOTHING. "We do not know" is a
//     truthful state; guessing would either double-refund or lose a refund.
//   - The journal posting happens only once the provider has confirmed, and it is idempotent on
//     (kind, reference_type, reference_id), so a reconciler that runs twice cannot post twice.
//   - A refund never touches the invoice's paid amount. The invoice was paid; the refund is a
//     separate economic event. Rewriting the invoice would make the customer's document disagree
//     with what actually happened.
package refunds

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/catalyst/billing/internal/audit"
	"github.com/catalyst/billing/internal/extproto"
	"github.com/catalyst/billing/internal/ledger"
	"github.com/catalyst/billing/internal/money"
	"github.com/catalyst/billing/internal/paginate"
	"github.com/catalyst/billing/internal/store"
	"github.com/jackc/pgx/v5"
)

// Refund statuses.
//
// The set is closed and persisted. `unknown` and `needs_review` are the two that matter: they are
// the states in which the product does not know whether money moved, and they are the reason a
// reconciler exists at all.
const (
	StatusCreated     = "created"
	StatusPending     = "pending"
	StatusSucceeded   = "succeeded"
	StatusFailed      = "failed"
	StatusUnknown     = "unknown"
	StatusNeedsReview = "needs_review"
)

var (
	// ErrNotFound is returned for an unknown refund.
	ErrNotFound = errors.New("refunds: not found")
	// ErrExceedsRefundable is returned when the amount is more than what is left.
	ErrExceedsRefundable = errors.New("refunds: amount exceeds the refundable balance")
	// ErrNotRefundable is returned for a payment that cannot be refunded.
	ErrNotRefundable = errors.New("refunds: this payment cannot be refunded")
	// ErrProviderRefused is returned when the provider said no. Nothing was posted.
	ErrProviderRefused = errors.New("refunds: the provider refused the refund")
	// ErrApprovalRequired is returned when the amount is above the threshold.
	ErrApprovalRequired = errors.New("refunds: a second staff signature is required")
)

// Gateway is the provider side of a refund.
//
// It is an interface rather than the concrete client so the tests can drive every branch,
// including the one that matters most: a call that times out with an unknown outcome.
type Gateway interface {
	Name() string
	Refund(ctx context.Context, params extproto.RefundParams) (extproto.RefundResult, error)
}

// Service creates and reconciles refunds.
type Service struct {
	pool    *store.Pool
	gateway Gateway
	// provider and account identify the gateway the payment came from.
	provider string
	account  string
	// ThresholdMinor is the amount at or above which a second staff signature is required. Zero
	// disables the check, which an installation may choose deliberately.
	ThresholdMinor int64
	// Now is injectable for tests.
	Now func() time.Time
	// Auditor, when set, writes the audit row inside the transaction that records the intent, so the
	// request's correlation id and source address are part of the record.
	auditor audit.Auditor
}

// WithAuditor returns a copy of the service that writes its audit row through a.
//
// A copy, not a mutation: the service is shared by every request, and an actor stored on it would be
// whichever request set it last.
func (s *Service) WithAuditor(a audit.Auditor) *Service {
	clone := *s
	clone.auditor = a
	return &clone
}

// New constructs the service.
func New(pool *store.Pool, gateway Gateway, provider, account string, thresholdMinor int64) *Service {
	return &Service{
		pool: pool, gateway: gateway, provider: provider, account: account,
		ThresholdMinor: thresholdMinor,
		Now:            func() time.Time { return time.Now().UTC() },
	}
}

// Refund is the record of a refund attempt.
type Refund struct {
	ID               string
	PaymentID        string
	ProviderRefundID string
	OperationKey     string
	AmountMinor      int64
	Currency         string
	Status           string
	Reason           string
	CreatedAt        time.Time
	FinishedAt       *time.Time
}

// Amount renders the refund as money.
func (r Refund) Amount() (money.Amount, error) {
	cur, err := currencyOf(r.Currency)
	if err != nil {
		return money.Amount{}, err
	}
	return money.New(r.AmountMinor, cur), nil
}

// Request describes a refund to create.
type Request struct {
	PaymentID string
	// AmountMinor is the part to refund. Zero means the full refundable balance.
	AmountMinor int64
	Reason      string
	RequestedBy string
	// ApprovalID names the second signature when the amount is above the threshold.
	ApprovalID string
	// OperationKey is the caller's idempotency token for this refund.
	//
	// It is deliberately NOT derived from the amount. Deriving it would make two deliberate
	// partial refunds of the same amount indistinguishable, so the second would be treated as a
	// replay of the first and silently do nothing — the operator would see a success and the
	// customer would get half of what was promised. A caller that wants retry safety supplies the
	// same key twice; one that wants a second refund supplies a new one.
	//
	// When empty, a key is generated: the refund is still recorded with a stable key that the
	// reconciler can use, but it is a new event rather than a replay.
	OperationKey string
}

// Result is the outcome of a refund attempt.
type Result struct {
	Refund Refund
	// Outcome is one of the status constants: succeeded, failed, unknown.
	Outcome string
	// Replayed is true when an identical refund already existed and its result is being returned.
	Replayed bool
}

// Create performs a refund.
//
// The sequence is: lock the payment, compute what is refundable, write the refund row, call the
// provider, then post and settle. Each step's failure mode is handled explicitly, because "the
// provider call failed" and "the provider call may have succeeded" need different answers.
func (s *Service) Create(ctx context.Context, req Request) (Result, error) {
	if s.gateway == nil {
		return Result{}, errors.New("refunds: no gateway is configured")
	}
	if req.PaymentID == "" {
		return Result{}, errors.New("refunds: a payment is required")
	}
	if req.RequestedBy == "" {
		return Result{}, errors.New("refunds: a requester is required")
	}

	var (
		out      Result
		payment  paymentSnapshot
		refundID string
		opKey    string
	)

	// Phase one: decide, and write the intent. This transaction commits before the provider is
	// called, so the intent survives a crash.
	err := s.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		payment, err = lockPayment(ctx, tx, req.PaymentID)
		if err != nil {
			return err
		}
		if payment.Status != "succeeded" && payment.Status != "partially_refunded" {
			return fmt.Errorf("%w: its status is %s", ErrNotRefundable, payment.Status)
		}

		cur, err := currencyOf(payment.Currency)
		if err != nil {
			return err
		}

		refundable, err := refundableMinor(ctx, tx, payment.ID, payment.AmountMinor)
		if err != nil {
			return err
		}
		amount := req.AmountMinor
		if amount == 0 {
			amount = refundable
		}
		if amount <= 0 {
			return fmt.Errorf("%w: nothing is left to refund", ErrExceedsRefundable)
		}
		if amount > refundable {
			return fmt.Errorf("%w: %d requested, %d available",
				ErrExceedsRefundable, amount, refundable)
		}

		// The threshold is checked here, inside the lock, so two sub-threshold refunds cannot be
		// split into a total that should have needed approval.
		if s.ThresholdMinor > 0 && amount >= s.ThresholdMinor && req.ApprovalID == "" {
			return fmt.Errorf("%w: %s or more needs a second staff signature",
				ErrApprovalRequired, money.New(amount, cur).Format())
		}

		// The caller's key, or a fresh one. Either way the row records it, so a reconciler can ask
		// the provider the same question later.
		opKey = req.OperationKey
		if opKey == "" {
			generated, err := newOperationKey()
			if err != nil {
				return err
			}
			opKey = generated
		}
		err = tx.QueryRow(ctx, `
			INSERT INTO refunds (payment_id, operation_key, amount_minor, currency, status,
			                     reason, requested_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
			ON CONFLICT (operation_key) DO NOTHING
			RETURNING id`,
			payment.ID, opKey, amount, payment.Currency, StatusCreated, req.Reason,
			req.RequestedBy).Scan(&refundID)
		if errors.Is(err, pgx.ErrNoRows) {
			// Already attempted: return what happened rather than doing it again.
			out.Replayed = true
			out.Refund, err = refundByOperationKey(ctx, tx, opKey)
			out.Outcome = out.Refund.Status
			return err
		}
		if err != nil {
			return fmt.Errorf("refunds: record refund intent: %w", err)
		}

		changes := map[string]any{
			"payment_id": payment.ID,
			"amount":     amount,
			"currency":   payment.Currency,
			"reason":     req.Reason,
		}
		if s.auditor != nil {
			return s.auditor.RecordAction(ctx, tx, "refund.create", "refund", refundID, changes)
		}
		return audit.Record(ctx, tx, audit.Event{
			Actor:      audit.Actor{ID: req.RequestedBy, Kind: audit.ActorUser},
			Action:     "refund.create",
			TargetType: "refund",
			TargetID:   refundID,
			Outcome:    audit.OutcomeSuccess,
			Changes:    changes,
		})
	})
	if err != nil {
		return Result{}, err
	}
	if out.Replayed {
		return out, nil
	}

	// Phase two: ask the provider. This is the call that can leave us not knowing.
	amountWire := extproto.Amount{Minor: req.AmountMinor, Currency: payment.Currency}
	if amountWire.Minor == 0 {
		// The full balance was chosen; the provider needs the concrete amount, not "everything".
		amountWire.Minor = refundAmountFor(ctx, s.pool, refundID)
	}
	params := extproto.RefundParams{
		MutatingMeta: extproto.MutatingMeta{OperationKey: opKey},
		PaymentID:    payment.ProviderPaymentID,
		Amount:       &amountWire,
		Reason:       req.Reason,
	}

	providerResult, callErr := s.gateway.Refund(ctx, params)
	switch {
	case callErr == nil:
		// The provider answered. Settle.
		err = s.settle(ctx, refundID, providerResult)
		out.Outcome = StatusSucceeded
		if err != nil {
			return Result{}, err
		}
	case isRejection(callErr):
		// The provider said no, definitively. Nothing was posted, and the customer must be told.
		if err := s.markFailed(ctx, refundID, callErr.Error()); err != nil {
			return Result{}, err
		}
		return Result{}, fmt.Errorf("%w: %v", ErrProviderRefused, callErr)
	default:
		// A timeout or a transport failure: we do not know whether the refund happened. Record
		// that, post nothing, and let the reconciler find out. Guessing here is how a customer gets
		// refunded twice or not at all.
		if err := s.markUnknown(ctx, refundID, callErr.Error()); err != nil {
			return Result{}, err
		}
		out.Outcome = StatusUnknown
	}

	out.Refund, err = s.Get(ctx, refundID)
	if err != nil {
		return Result{}, err
	}
	return out, nil
}

// settle marks a refund succeeded and posts it.
func (s *Service) settle(ctx context.Context, refundID string, providerResult extproto.RefundResult) error {
	return s.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var (
			paymentID string
			amount    int64
			currency  string
			customer  string
			status    string
		)
		if err := tx.QueryRow(ctx, `
			SELECT r.payment_id, r.amount_minor, r.currency,
			       COALESCE((SELECT i.customer_id::text FROM payment_allocations pa
			                  JOIN invoices i ON i.id = pa.invoice_id
			                  WHERE pa.payment_id = r.payment_id
			                  ORDER BY pa.created_at LIMIT 1), '') AS customer_id,
			       r.status
			FROM refunds r
			WHERE r.id = $1 FOR UPDATE`, refundID).
			Scan(&paymentID, &amount, &currency, &customer, &status); err != nil {
			return fmt.Errorf("refunds: lock refund: %w", err)
		}
		if status == StatusSucceeded {
			// Already settled by an earlier attempt or a concurrent reconciler.
			return nil
		}

		cur, err := currencyOf(currency)
		if err != nil {
			return err
		}
		if customer == "" {
			// A refund with no attributable customer cannot be posted: an unattributable
			// entry cannot be reconciled. It is a data problem, not a user error.
			return errors.New("refunds: the payment is not allocated to any customer")
		}

		// The posting is the point of no return for the books. It is idempotent on the refund id,
		// so a reconciler that runs after a successful settle is a no-op.
		if _, _, err := ledger.Post(ctx, tx, ledger.Posting{
			Kind:        ledger.KindRefund,
			Reference:   ledger.RefRefund,
			ReferenceID: refundID,
			CustomerID:  customer,
			Currency:    cur,
			// A refund returns money: the expense account grows, the clearing account shrinks. It
			// deliberately does NOT touch revenue or the invoice, because the invoice was correct.
			Debits:  []ledger.Entry{{Account: ledger.AccountRefunds, Amount: money.New(amount, cur)}},
			Credits: []ledger.Entry{{Account: ledger.AccountCashClearing, Amount: money.New(amount, cur)}},
		}); err != nil {
			return fmt.Errorf("refunds: post refund: %w", err)
		}

		if _, err := tx.Exec(ctx, `
			UPDATE refunds SET status = $2, provider_refund_id = $3, finished_at = now()
			WHERE id = $1`, refundID, StatusSucceeded, providerResult.RefundID); err != nil {
			return fmt.Errorf("refunds: mark refund succeeded: %w", err)
		}

		// The payment's status reflects the total refunded. It is derived from the refunds table
		// rather than incremented, so a replay cannot double-count.
		return refreshPaymentStatus(ctx, tx, paymentID)
	})
}

// markFailed records a definitive refusal.
func (s *Service) markFailed(ctx context.Context, refundID, reason string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE refunds SET status = $2, finished_at = now(), reason = CASE
			WHEN reason = '' THEN $3 ELSE reason || ' | ' || $3 END
		WHERE id = $1`, refundID, StatusFailed, reason)
	if err != nil {
		return fmt.Errorf("refunds: mark refund failed: %w", err)
	}
	return nil
}

// markUnknown records that we do not know whether the refund happened.
func (s *Service) markUnknown(ctx context.Context, refundID, reason string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE refunds SET status = $2, reason = CASE
			WHEN reason = '' THEN $3 ELSE reason || ' | ' || $3 END
		WHERE id = $1`, refundID, StatusUnknown, reason)
	if err != nil {
		return fmt.Errorf("refunds: mark refund unknown: %w", err)
	}
	return nil
}

// Reconcile resolves refunds whose outcome is unknown.
//
// It asks the provider again rather than retrying the refund: a retry of a refund that actually
// succeeded is a second refund. The operation key makes the question idempotent, so asking is safe
// and the answer is authoritative.
func (s *Service) Reconcile(ctx context.Context, limit int) (settled, stillUnknown int, err error) {
	if s.gateway == nil {
		return 0, 0, errors.New("refunds: no gateway is configured")
	}
	if limit <= 0 {
		limit = 20
	}

	// `created` is included: a row in that state means the intent was committed but the process may
	// have died before (or during) the provider call. Asking the provider with the same operation key
	// is safe and is the only way to learn what happened.
	rows, err := s.pool.Query(ctx, `
		SELECT id, payment_id, operation_key, amount_minor, currency
		FROM refunds
		WHERE status IN ($1, $2, $3)
		ORDER BY created_at
		LIMIT $4`, StatusCreated, StatusUnknown, StatusNeedsReview, limit)
	if err != nil {
		return 0, 0, fmt.Errorf("refunds: list unresolved: %w", err)
	}
	type pending struct {
		id, paymentID, opKey, currency string
		amount                         int64
	}
	var list []pending
	for rows.Next() {
		var p pending
		if err := rows.Scan(&p.id, &p.paymentID, &p.opKey, &p.amount, &p.currency); err != nil {
			rows.Close()
			return 0, 0, fmt.Errorf("refunds: scan unresolved: %w", err)
		}
		list = append(list, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, 0, fmt.Errorf("refunds: read unresolved: %w", err)
	}

	for _, p := range list {
		var providerPaymentID string
		if err := s.pool.QueryRow(ctx,
			`SELECT provider_payment_id FROM payments WHERE id = $1`, p.paymentID).
			Scan(&providerPaymentID); err != nil {
			return settled, stillUnknown, fmt.Errorf("refunds: load payment: %w", err)
		}

		// The same operation key asks the same question. A provider that honours idempotency
		// answers with the original result rather than performing a second refund.
		result, callErr := s.gateway.Refund(ctx, extproto.RefundParams{
			MutatingMeta: extproto.MutatingMeta{OperationKey: p.opKey},
			PaymentID:    providerPaymentID,
			Amount:       &extproto.Amount{Minor: p.amount, Currency: p.currency},
		})
		if callErr != nil {
			stillUnknown++
			continue
		}
		if err := s.settle(ctx, p.id, result); err != nil {
			return settled, stillUnknown, err
		}
		settled++
	}
	return settled, stillUnknown, nil
}

// Get returns one refund.
func (s *Service) Get(ctx context.Context, id string) (Refund, error) {
	return scanRefund(s.pool.QueryRow(ctx, selectColumns+` WHERE id = $1`, id))
}

// List returns refunds, newest first.
func (s *Service) List(ctx context.Context, paymentID string, limit int, cursor string) ([]Refund, string, error) {
	limit = paginate.Clamp(limit)

	args := []any{}
	where := "true"
	if paymentID != "" {
		args = append(args, paymentID)
		where = fmt.Sprintf("payment_id = $%d", len(args))
	}
	if cursor != "" {
		sortKey, id, err := paginate.Decode(cursor)
		if err != nil {
			return nil, "", err
		}
		when, perr := time.Parse(time.RFC3339Nano, sortKey)
		if perr != nil {
			return nil, "", fmt.Errorf("%w: bad timestamp", paginate.ErrInvalidCursor)
		}
		args = append(args, when, id)
		where += fmt.Sprintf(" AND (created_at, id) < ($%d, $%d)", len(args)-1, len(args))
	}
	args = append(args, limit+1)

	rows, err := s.pool.Query(ctx, selectColumns+` WHERE `+where+
		` ORDER BY created_at DESC, id DESC LIMIT $`+fmt.Sprint(len(args)), args...)
	if err != nil {
		return nil, "", fmt.Errorf("refunds: list: %w", err)
	}
	defer rows.Close()

	out := make([]Refund, 0, limit)
	for rows.Next() {
		r, err := scanRefund(rows)
		if err != nil {
			return nil, "", err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("refunds: list: %w", err)
	}

	next := ""
	if len(out) > limit {
		out = out[:limit]
		last := out[len(out)-1]
		next = paginate.Encode(last.CreatedAt.UTC().Format(time.RFC3339Nano), last.ID)
	}
	return out, next, nil
}

const selectColumns = `
	SELECT id, payment_id, COALESCE(provider_refund_id, ''), operation_key, amount_minor,
	       currency, status, reason, created_at, finished_at
	FROM refunds`

type scanner interface{ Scan(...any) error }

func scanRefund(row scanner) (Refund, error) {
	var r Refund
	err := row.Scan(&r.ID, &r.PaymentID, &r.ProviderRefundID, &r.OperationKey, &r.AmountMinor,
		&r.Currency, &r.Status, &r.Reason, &r.CreatedAt, &r.FinishedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Refund{}, ErrNotFound
	}
	if err != nil {
		return Refund{}, fmt.Errorf("refunds: scan: %w", err)
	}
	return r, nil
}

// paymentSnapshot is what the refund decision needs to know about a payment.
type paymentSnapshot struct {
	ID                string
	ProviderPaymentID string
	AmountMinor       int64
	Currency          string
	Status            string
}

// lockPayment reads a payment under a row lock.
//
// The lock is the concurrency control for the whole refund decision: without it, two requests can
// each compute the full balance as refundable and both proceed.
func lockPayment(ctx context.Context, tx pgx.Tx, paymentID string) (paymentSnapshot, error) {
	var p paymentSnapshot
	err := tx.QueryRow(ctx, `
		SELECT id, provider_payment_id, amount_minor, currency, status
		FROM payments WHERE id = $1 FOR UPDATE`, paymentID).
		Scan(&p.ID, &p.ProviderPaymentID, &p.AmountMinor, &p.Currency, &p.Status)
	if errors.Is(err, pgx.ErrNoRows) {
		return paymentSnapshot{}, ErrNotFound
	}
	if err != nil {
		return paymentSnapshot{}, fmt.Errorf("refunds: lock payment: %w", err)
	}
	return p, nil
}

// refundableMinor is the amount still available to refund.
//
// It counts `created` as well as the settled and in-flight states. `created` is the intent row that
// this service commits BEFORE calling the provider, so a concurrent refund must treat it as money
// that may already be on its way — otherwise two operators clicking at the same time each see the
// full balance, and the payment is refunded twice. That was a real bug, caught by the concurrent
// test: the second transaction saw the first refund as `created`, did not count it, and proceeded.
//
// `failed` is deliberately excluded: a definitive refusal means nothing moved, so the balance is
// genuinely available again.
func refundableMinor(ctx context.Context, tx pgx.Tx, paymentID string, amountMinor int64) (int64, error) {
	var committed int64
	if err := tx.QueryRow(ctx, `
		SELECT COALESCE(SUM(amount_minor), 0) FROM refunds
		WHERE payment_id = $1 AND status IN ($2, $3, $4, $5, $6)`,
		paymentID, StatusCreated, StatusSucceeded, StatusPending, StatusUnknown,
		StatusNeedsReview).Scan(&committed); err != nil {
		return 0, fmt.Errorf("refunds: sum refunded: %w", err)
	}
	remaining := amountMinor - committed
	if remaining < 0 {
		remaining = 0
	}
	return remaining, nil
}

// refundAmountFor reads a refund's recorded amount.
func refundAmountFor(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, refundID string) int64 {
	var amount int64
	_ = q.QueryRow(ctx, `SELECT amount_minor FROM refunds WHERE id = $1`, refundID).Scan(&amount)
	return amount
}

// refundByOperationKey reads a refund that already exists for a key.
func refundByOperationKey(ctx context.Context, tx pgx.Tx, opKey string) (Refund, error) {
	return scanRefund(tx.QueryRow(ctx, selectColumns+` WHERE operation_key = $1`, opKey))
}

// refreshPaymentStatus derives the payment's status from its refunds.
func refreshPaymentStatus(ctx context.Context, tx pgx.Tx, paymentID string) error {
	if _, err := tx.Exec(ctx, `
		UPDATE payments p
		SET status = CASE
			WHEN (SELECT COALESCE(SUM(r.amount_minor), 0) FROM refunds r
			       WHERE r.payment_id = p.id AND r.status = 'succeeded') >= p.amount_minor
			     THEN 'refunded'
			WHEN (SELECT COALESCE(SUM(r.amount_minor), 0) FROM refunds r
			       WHERE r.payment_id = p.id AND r.status = 'succeeded') > 0
			     THEN 'partially_refunded'
			ELSE p.status
		END
		WHERE p.id = $1`, paymentID); err != nil {
		return fmt.Errorf("refunds: refresh payment status: %w", err)
	}
	return nil
}

// isRejection reports whether an error is a definitive refusal rather than an unknown outcome.
//
// The distinction is the whole reason the outcome states exist: a rejection means the provider
// answered and nothing happened; a timeout or a crash means we do not know, and guessing is how a
// customer is refunded twice or not at all.
func isRejection(err error) bool {
	return errors.Is(err, extproto.ErrRejected) ||
		errors.Is(err, extproto.ErrInvalidParams) ||
		errors.Is(err, extproto.ErrNotFound)
}

// newOperationKey returns a random refund operation key.
//
// It is prefixed so a provider dashboard makes it obvious that the key came from this product rather
// than from the provider's own API.
func newOperationKey() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("refunds: read random bytes: %w", err)
	}
	return "refund_" + hex.EncodeToString(b[:]), nil
}

// currencyOf resolves a persisted currency code.
func currencyOf(code string) (money.Currency, error) {
	if cur, ok := money.LookupCurrency(code); ok {
		return cur, nil
	}
	return money.NewCurrency(code, 2)
}
