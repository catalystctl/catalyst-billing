package credit

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/catalyst/billing/internal/ledger"
	"github.com/catalyst/billing/internal/money"
	"github.com/jackc/pgx/v5"
)

// Staff grants (CRD-07): credit with no money behind it.
//
// A grant is still a *document*. It creates a credit note with its own number, because the
// customer's paperwork and a tax filing refer to "credit note numbers" as a sequence, and
// a silent balance change that appears on no document is unauditable by construction. The
// note has no invoice: the table allows that on purpose ("a goodwill credit need not
// correct a specific invoice").
//
// The posting is an adjustment — debit revenue, credit customer_credit — rather than the
// `credit_note` posting the invoice-correcting path makes, because nothing was corrected:
// no invoice total moved. Both are contra-revenue, which is the correct treatment of a
// sales allowance; only the reference differs, and the reference is what makes each of
// them idempotent on its own document.

// CreditSeries mirrors the credit-note number series.
//
// It is the *same* scope the credit-note issuer uses, deliberately: there is one credit
// note sequence per installation, and a grant that allocated from a second one would hand
// out a number that a real credit note later repeats. The scope string is duplicated here
// rather than imported because internal/creditnote does not export it, and adding an
// exported accessor there would couple two packages that share only a row in a sequence
// table.
var CreditSeries = struct {
	Scope  string
	Prefix string
	Width  int
}{Scope: "credit_note", Prefix: "CN-", Width: 6}

// GrantRequest describes an operator's grant of credit.
type GrantRequest struct {
	CustomerID  string
	AmountMinor int64
	Currency    string
	Reason      string
	GrantedBy   string
	// Key is the request's idempotency identity. It is required: a grant with no key cannot
	// be distinguished from a second grant, and "I clicked twice" must not mint money.
	Key string
}

// Grant is an issued goodwill credit.
type Grant struct {
	ID          string
	Number      string
	CustomerID  string
	Currency    string
	AmountMinor int64
	Reason      string
	CreatedAt   time.Time
	// BalanceMinor is the customer's credit in this currency after the grant.
	BalanceMinor int64
	// AlreadyGranted is true when this exact request had already been granted.
	AlreadyGranted bool
}

// Grant issues credit to a customer and posts it to the subledger.
func (s *Service) Grant(ctx context.Context, req GrantRequest) (Grant, error) {
	if s.pool == nil {
		return Grant{}, ErrInvalidRequest
	}
	if req.CustomerID == "" {
		return Grant{}, fmt.Errorf("%w: a customer is required", ErrInvalidRequest)
	}
	if req.AmountMinor <= 0 {
		return Grant{}, fmt.Errorf("%w: the amount must be positive", ErrInvalidRequest)
	}
	if strings.TrimSpace(req.Reason) == "" {
		return Grant{}, fmt.Errorf("%w: a reason is required", ErrInvalidRequest)
	}
	if strings.TrimSpace(req.Key) == "" {
		return Grant{}, fmt.Errorf("%w: an idempotency key is required", ErrInvalidRequest)
	}
	cur, err := resolveCurrency(req.Currency)
	if err != nil {
		return Grant{}, err
	}

	noteID := derivedUUID("credit-grant:" + req.Key)

	var out Grant
	err = s.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		out = Grant{}

		// A replay finds the existing note and reports it. The id is derived from the
		// request key, so this is a lookup rather than a scan.
		var (
			number    *string
			amount    int64
			currency  string
			reason    string
			createdAt time.Time
		)
		err := tx.QueryRow(ctx, `
			SELECT number, total_minor, currency, reason, created_at
			FROM credit_notes WHERE id = $1`, noteID).
			Scan(&number, &amount, &currency, &reason, &createdAt)
		if err == nil {
			balance, berr := creditBalance(ctx, tx, req.CustomerID, cur)
			if berr != nil {
				return berr
			}
			out = Grant{
				ID: noteID, Number: derefString(number), CustomerID: req.CustomerID,
				Currency: currency, AmountMinor: amount, Reason: reason,
				CreatedAt: createdAt, BalanceMinor: balance, AlreadyGranted: true,
			}
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("credit: look up grant: %w", err)
		}

		// Lock the customer so two concurrent grants cannot each pass a balance check with
		// stale numbers, and so a missing customer is a clean not-found.
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT true FROM customers WHERE id = $1 FOR UPDATE`,
			req.CustomerID).Scan(&exists); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return fmt.Errorf("credit: lock customer: %w", err)
		}

		allocated, err := allocateCreditNumber(ctx, tx)
		if err != nil {
			return err
		}

		if _, err := tx.Exec(ctx, `
			INSERT INTO credit_notes (id, number, customer_id, invoice_id, currency,
				total_minor, tax_minor, reason, status, issued_at, snapshot, created_by)
			VALUES ($1, $2, $3, NULL, $4, $5, 0, $6, 'issued', now(), $7, $8)`,
			noteID, allocated, req.CustomerID, cur.Code(), req.AmountMinor, req.Reason,
			map[string]any{
				"schema_version": 1,
				"kind":           "grant",
				"amount":         req.AmountMinor,
				"currency":       cur.Code(),
				"reason":         req.Reason,
			}, nullableUUID(req.GrantedBy),
		); err != nil {
			return fmt.Errorf("credit: insert grant: %w", err)
		}

		if _, _, err := ledger.Post(ctx, tx, ledger.Posting{
			Kind:        ledger.KindAdjustment,
			Reference:   RefCreditGrant,
			ReferenceID: noteID,
			CustomerID:  req.CustomerID,
			Currency:    cur,
			Debits: []ledger.Entry{{
				Account: ledger.AccountRevenue,
				Amount:  money.New(req.AmountMinor, cur),
				Memo:    "goodwill credit " + allocated,
			}},
			Credits: []ledger.Entry{{
				Account: ledger.AccountCredit,
				Amount:  money.New(req.AmountMinor, cur),
				Memo:    req.Reason,
			}},
		}); err != nil {
			return fmt.Errorf("credit: post grant: %w", err)
		}

		balance, err := creditBalance(ctx, tx, req.CustomerID, cur)
		if err != nil {
			return err
		}
		out = Grant{
			ID: noteID, Number: allocated, CustomerID: req.CustomerID,
			Currency: cur.Code(), AmountMinor: req.AmountMinor, Reason: req.Reason,
			CreatedAt: s.depositNow(), BalanceMinor: balance,
		}

		return s.recordAudit(ctx, tx, "credit.grant", "credit_note", noteID, req.GrantedBy,
			map[string]any{
				"customer_id": req.CustomerID,
				"number":      allocated,
				"amount":      req.AmountMinor,
				"currency":    cur.Code(),
				"reason":      req.Reason,
				"balance":     balance,
			})
	})
	if err != nil {
		return Grant{}, err
	}
	return out, nil
}

// allocateCreditNumber consumes the next number in the credit-note series.
//
// The UPDATE ... RETURNING is the allocation: concurrent callers get different numbers
// because the row lock serialises them, and a rolled-back transaction returns the number
// to the sequence rather than leaving a gap.
func allocateCreditNumber(ctx context.Context, tx pgx.Tx) (string, error) {
	var next int64
	err := tx.QueryRow(ctx, `
		INSERT INTO invoice_number_sequences (scope, next_value)
		VALUES ($1, 2)
		ON CONFLICT (scope) DO UPDATE
			SET next_value = invoice_number_sequences.next_value + 1
		RETURNING next_value - 1`, CreditSeries.Scope).Scan(&next)
	if err != nil {
		return "", fmt.Errorf("credit: allocate credit note number: %w", err)
	}
	digits := strconv.FormatInt(next, 10)
	for len(digits) < CreditSeries.Width {
		digits = "0" + digits
	}
	return CreditSeries.Prefix + digits, nil
}

func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
