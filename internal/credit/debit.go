package credit

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/catalyst/billing/internal/ledger"
	"github.com/catalyst/billing/internal/money"
	"github.com/jackc/pgx/v5"
)

// Staff debits (CRD-07, the direction the product was missing).
//
// A grant adds credit; an application spends it on an invoice. Neither takes credit *away*
// without a document to hang it on, so before this file an operator who had granted too much,
// or who had to claw credit back after a chargeback that arrived outside the gateway, had one
// tool: SQL against the journal. That is unauditable by construction and invisible to the
// customer.
//
// A debit is therefore three things at once, and all three are the point:
//
//   - **A journal entry, never an edit.** The posting is debit `customer_credit`, credit
//     `revenue`, with kind `credit_debit` and a reference id derived from the request's
//     idempotency key. Nothing is updated, so the ledger stays append-only and the balance
//     stays the sum of its entries. A mistake is corrected with another entry, exactly as a
//     mistake in an invoice is corrected with a credit note.
//   - **A refusal, not a clamp.** A debit larger than the balance is refused with
//     ErrInsufficientCredit. The alternative — taking what is there and leaving the rest — would
//     mean the customer's balance and the operator's intent silently differ, and a negative
//     balance reaching the customer's own screen is indistinguishable from a bug in their eyes.
//   - **A reason, mandatory and customer-visible.** The reason is the memo on the
//     `customer_credit` entry, which is what the customer's own credit history renders, and it
//     is in the audit row written in the same transaction. A deduction nobody can explain is
//     the failure this feature exists to prevent.
//
// The counter-account is `revenue`, which is the mirror of the grant posting (debit revenue,
// credit customer_credit). Debiting the liability has to land somewhere, and the two honest
// answers are both revenue: reversing an allowance that was granted (the grant recognised a
// contra-revenue expense, so taking it back reverses it) and recognising a forfeited deposit as
// income (the money is already in cash_clearing and is no longer owed to anyone). Nothing moves
// to `receivable` — a debit does not pay an invoice, and booking it as if it did would settle
// debt the customer still owes.

// DebitRequest describes an operator's deduction from a customer's credit balance.
type DebitRequest struct {
	CustomerID  string
	AmountMinor int64
	Currency    string
	// Reason is mandatory and reaches both the customer's statement and the audit row.
	Reason string
	// DebitedBy is the user id recorded in the audit row, when there is one.
	DebitedBy string
	// Key is the request's idempotency identity. It is required: a debit with no key cannot be
	// told apart from a second debit, and "the network dropped, press it again" must not take
	// the money twice.
	Key string
}

// Debit is one staff deduction.
type Debit struct {
	// ID is the posting's reference id, derived from the request key.
	ID         string
	CustomerID string
	Currency   string
	// AmountMinor is the amount taken, always positive.
	AmountMinor int64
	Reason      string
	CreatedAt   time.Time
	// BalanceMinor is the customer's balance in this currency after the debit.
	BalanceMinor int64
	// AlreadyDebited is true when this exact request had already been recorded. The amounts
	// describe the existing entry, not a new one.
	AlreadyDebited bool
}

// Debit takes credit away from a customer and posts it to the subledger.
//
// The customer row is locked for the duration, so two concurrent debits cannot each read the
// same balance and each believe the whole of it is available. That lock is also what makes the
// over-debit check meaningful: without it the check is a race that a busy support desk could
// lose, and the loser would be a negative balance.
func (s *Service) Debit(ctx context.Context, req DebitRequest) (Debit, error) {
	if s.pool == nil {
		return Debit{}, ErrInvalidRequest
	}
	if req.CustomerID == "" {
		return Debit{}, fmt.Errorf("%w: a customer is required", ErrInvalidRequest)
	}
	if req.AmountMinor <= 0 {
		return Debit{}, fmt.Errorf("%w: the amount must be positive", ErrInvalidRequest)
	}
	if strings.TrimSpace(req.Reason) == "" {
		return Debit{}, fmt.Errorf("%w: a reason is required", ErrInvalidRequest)
	}
	if strings.TrimSpace(req.Key) == "" {
		return Debit{}, fmt.Errorf("%w: an idempotency key is required", ErrInvalidRequest)
	}
	cur, err := resolveCurrency(req.Currency)
	if err != nil {
		return Debit{}, err
	}

	debitID := derivedUUID("credit-debit:" + req.Key)

	var out Debit
	err = s.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		out = Debit{}

		// A replay is detected first, by its derived id. Looking for it before the balance is
		// read matters: the balance has moved by then (this debit moved it), so a check against
		// the current balance would refuse a replay of a debit that already happened.
		existing, err := existingDebit(ctx, tx, debitID)
		if err != nil {
			return err
		}
		if existing != nil {
			if existing.CustomerID != req.CustomerID || existing.Currency != cur.Code() ||
				existing.AmountMinor != req.AmountMinor {
				return fmt.Errorf("%w: %s", ErrKeyReused, debitID)
			}
			balance, berr := creditBalance(ctx, tx, req.CustomerID, cur)
			if berr != nil {
				return berr
			}
			existing.BalanceMinor = balance
			existing.AlreadyDebited = true
			out = *existing
			return nil
		}

		// Lock the customer so a concurrent debit cannot pass the same balance check, and so a
		// missing customer is a clean not-found rather than a foreign-key failure at commit.
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT true FROM customers WHERE id = $1 FOR UPDATE`,
			req.CustomerID).Scan(&exists); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return fmt.Errorf("credit: lock customer: %w", err)
		}

		balance, err := creditBalance(ctx, tx, req.CustomerID, cur)
		if err != nil {
			return err
		}
		if req.AmountMinor > balance {
			// Refused, not clamped: the operator asked for something the account cannot
			// support, and the honest answer is no. The balance is unchanged, because nothing
			// was posted.
			return fmt.Errorf("%w: the balance is %d %s minor units, the debit is %d",
				ErrInsufficientCredit, balance, cur.Code(), req.AmountMinor)
		}

		if _, _, err := ledger.Post(ctx, tx, ledger.Posting{
			Kind:        ledger.KindCreditDebit,
			Reference:   ledger.RefCreditDebit,
			ReferenceID: debitID,
			CustomerID:  req.CustomerID,
			Currency:    cur,
			Debits: []ledger.Entry{{
				Account: ledger.AccountCredit,
				Amount:  money.New(req.AmountMinor, cur),
				// The reason is the memo, deliberately: the customer's own credit history is
				// read from these entries, so this is the only place the "why" can live and
				// still reach them.
				Memo: req.Reason,
			}},
			Credits: []ledger.Entry{{
				Account: ledger.AccountRevenue,
				Amount:  money.New(req.AmountMinor, cur),
				Memo:    "staff debit " + debitID,
			}},
		}); err != nil {
			return fmt.Errorf("credit: post debit: %w", err)
		}

		out = Debit{
			ID:           debitID,
			CustomerID:   req.CustomerID,
			Currency:     cur.Code(),
			AmountMinor:  req.AmountMinor,
			Reason:       req.Reason,
			CreatedAt:    s.depositNow(),
			BalanceMinor: balance - req.AmountMinor,
		}

		return s.recordAudit(ctx, tx, "credit.debit", "credit_debit", debitID, req.DebitedBy,
			map[string]any{
				"customer_id":    req.CustomerID,
				"amount":         req.AmountMinor,
				"currency":       cur.Code(),
				"reason":         req.Reason,
				"balance_before": balance,
				"balance_after":  balance - req.AmountMinor,
			})
	})
	if err != nil {
		return Debit{}, err
	}
	return out, nil
}

// existingDebit reads a debit posting by its derived reference id.
//
// It reads the ledger rather than a table of its own because the ledger is the record: a second
// table describing the same deduction would be a second source of truth, and the first time the
// two disagreed the ledger would be right and the table wrong.
func existingDebit(ctx context.Context, tx pgx.Tx, id string) (*Debit, error) {
	var (
		out       Debit
		createdAt time.Time
	)
	err := tx.QueryRow(ctx, `
		SELECT jt.reference_id::text, je.customer_id::text, jt.currency, je.debit_minor,
		       je.memo, jt.created_at
		FROM journal_transactions jt
		JOIN journal_entries je ON je.transaction_id = jt.id
		WHERE jt.kind = $1 AND jt.reference_type = $2 AND jt.reference_id = $3
		  AND je.account_code = $4`,
		string(ledger.KindCreditDebit), string(ledger.RefCreditDebit), id,
		ledger.AccountCredit).
		Scan(&out.ID, &out.CustomerID, &out.Currency, &out.AmountMinor, &out.Reason, &createdAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("credit: load debit: %w", err)
	}
	out.CreatedAt = createdAt
	return &out, nil
}

// DebitRow is one staff debit in the operator's list.
type DebitRow struct {
	ID            string
	CustomerID    string
	CustomerEmail string
	Currency      string
	AmountMinor   int64
	Reason        string
	CreatedAt     time.Time
	DebitedBy     string
	DebitedByMail string
}

// DebitsFor lists staff debits, newest first, optionally for one customer.
//
// Like the balance, the list is read from the journal rather than from a table of deductions:
// what an operator sees here is the same rows the customer's balance is summed from, so the list
// cannot disagree with the number it explains. The operator is read from the audit row written
// with the posting, and is empty rather than absent when that read finds nothing — a debit whose
// audit row is missing is still a debit, and hiding it would hide the deduction itself.
func DebitsFor(ctx context.Context, q Querier, customerID string, limit int) ([]DebitRow, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	var filter any
	if strings.TrimSpace(customerID) != "" {
		filter = customerID
	}
	rows, err := q.Query(ctx, `
		SELECT jt.reference_id::text, je.customer_id::text, c.email, jt.currency,
		       je.debit_minor, je.memo, jt.created_at,
		       COALESCE(a.actor_id::text, ''), COALESCE(u.email, '')
		FROM journal_transactions jt
		JOIN journal_entries je ON je.transaction_id = jt.id AND je.account_code = $2
		JOIN customers c ON c.id = je.customer_id
		LEFT JOIN LATERAL (
			SELECT actor_id FROM audit_events
			WHERE target_type = 'credit_debit' AND target_id = jt.reference_id
			  AND action = 'credit.debit'
			ORDER BY id DESC LIMIT 1
		) a ON true
		LEFT JOIN users u ON u.id = a.actor_id
		WHERE jt.kind = $1 AND ($3::uuid IS NULL OR je.customer_id = $3::uuid)
		ORDER BY jt.created_at DESC, jt.reference_id DESC
		LIMIT $4`,
		string(ledger.KindCreditDebit), ledger.AccountCredit, filter, limit)
	if err != nil {
		return nil, fmt.Errorf("credit: read debits: %w", err)
	}
	defer rows.Close()

	out := []DebitRow{}
	for rows.Next() {
		var row DebitRow
		if err := rows.Scan(&row.ID, &row.CustomerID, &row.CustomerEmail, &row.Currency,
			&row.AmountMinor, &row.Reason, &row.CreatedAt, &row.DebitedBy,
			&row.DebitedByMail); err != nil {
			return nil, fmt.Errorf("credit: scan debit: %w", err)
		}
		out = append(out, row)
	}
	return out, rows.Err()
}
