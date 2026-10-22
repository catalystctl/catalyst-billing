// Package credit implements customer credit as a *deposit* and as an *instrument*.
//
// Credit is not a balance column. It is the `customer_credit` subledger account, and its
// value is derived from journal entries. That decision was made before this package
// existed and it is the reason this package is small: every operation here is a posting
// rule plus a document, never a number somebody has to remember to keep in step.
//
// Three operations live here:
//
//   - A deposit (CRD-02, CRD-05): the customer tops up through the configured gateway. The
//     money arrives as a payment; the credit is the liability side of it.
//   - A grant (CRD-07): an operator credits a customer without money arriving. That is a
//     credit note, numbered and immutable, exactly like any other credit document.
//   - An application (CRD-03, CRD-04, PAY-17): credit is spent on an invoice, either
//     automatically when the invoice is issued or because the customer asked.
//
// # Why a deposit is an invoice
//
// The gateway path the panel already has is `checkout.Service.Start(invoiceID)`: it reads
// the amount from the invoice under a row lock, records the attempt, and calls the
// gateway. `payment_attempts.invoice_id` is `NOT NULL REFERENCES invoices(id)`, and the
// webhook processor verifies the settled amount against that attempt. A deposit therefore
// needs an invoice behind it, and the alternative — making `invoice_id` nullable — would
// weaken the one constraint that makes amount verification possible.
//
// So a deposit is:
//
//	orders (no items)  ->  invoice "Account credit deposit"  ->  payment_attempts
//	                   ->  payments + allocation             ->  customer_credit
//
// The *order with no items* is what marks the invoice as a deposit. The existing purchase
// path always writes at least one order item, so "an order with no items" cannot be
// produced by a sale and needs no new column, no snapshot hack, and no constraint change.
//
// The deposit invoice is priced with a zero tax rate on purpose: a deposit is not a
// taxable supply. Issuing it posts receivable/revenue like any document; the grant posted
// when it settles debits that revenue back and credits `customer_credit`, so the net
// effect of a completed deposit is debit cash_clearing, credit customer_credit. Revenue is
// recognised later, when the credit is spent on an invoice that really is a supply.
package credit

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/catalyst/billing/internal/audit"
	"github.com/catalyst/billing/internal/ledger"
	"github.com/catalyst/billing/internal/money"
	"github.com/catalyst/billing/internal/settings"
	"github.com/catalyst/billing/internal/store"
	"github.com/jackc/pgx/v5"
)

var (
	// ErrDisabled is returned when an operator has turned customer credit off.
	ErrDisabled = errors.New("credit: customer credit is disabled")
	// ErrBelowMinimum is returned for a deposit smaller than credits.minimum_deposit_minor.
	ErrBelowMinimum = errors.New("credit: the deposit is below the minimum")
	// ErrAboveMaximum is returned for a deposit larger than credits.maximum_deposit_minor.
	ErrAboveMaximum = errors.New("credit: the deposit is above the maximum")
	// ErrMaxBalance is returned when the deposit would push the balance past
	// credits.maximum_credit_minor. It is refused before anything is created, so an
	// over-limit top-up leaves no abandoned invoice behind.
	ErrMaxBalance = errors.New("credit: the deposit would exceed the maximum credit balance")
	// ErrNotFound is returned for an unknown invoice, customer, deposit or payment.
	ErrNotFound = errors.New("credit: not found")
	// ErrNotDeposit is returned when a settlement does not belong to a deposit invoice.
	ErrNotDeposit = errors.New("credit: not a credit deposit")
	// ErrAmountMismatch is returned when a provider settled an amount that is not the
	// deposit that was asked for. It is fatal rather than absorbed, for the same reason the
	// payment processor treats it as fatal: a mismatch is an anomaly, not a partial payment.
	ErrAmountMismatch = errors.New("credit: the settled amount does not match the deposit")
	// ErrInsufficientCredit is returned when the customer does not have enough credit.
	ErrInsufficientCredit = errors.New("credit: insufficient customer credit")
	// ErrNoGateway is returned when a deposit is started without a configured gateway.
	ErrNoGateway = errors.New("credit: no payment gateway is configured")
	// ErrInvalidRequest is returned for a malformed request.
	ErrInvalidRequest = errors.New("credit: invalid request")
)

// Settings is the credits.* group of the settings table, with code-side defaults.
//
// Every value has a default because the row may be absent: a fresh installation, or one
// migrated from before the settings table existed, must still take a deposit. A missing
// row is configuration that has not been written yet, not a broken installation. The
// defaults themselves live in internal/settings, which is the one place that declares what
// a setting is called, what type it has and what it reads as when absent.
type Settings struct {
	Enabled             bool
	MinimumDepositMinor int64
	MaximumDepositMinor int64
	MaximumCreditMinor  int64
	AutoUse             bool
}

// DefaultSettings is the configuration a fresh installation behaves as if it had.
//
// The numbers come from internal/settings, which is the single registry of every
// setting's type and default. They are not repeated here: a second copy would drift the
// first time an operator changed a default, and the drift would show up as a deposit limit
// that disagrees with the settings page that displays it.
func DefaultSettings() Settings {
	return Settings{
		Enabled:             settingDefaultBool("enabled"),
		MinimumDepositMinor: settingDefaultInt("minimum_deposit_minor"),
		MaximumDepositMinor: settingDefaultInt("maximum_deposit_minor"),
		MaximumCreditMinor:  settingDefaultInt("maximum_credit_minor"),
		AutoUse:             settingDefaultBool("auto_use"),
	}
}

// settingDefaultBool reads one credits default from the settings registry.
func settingDefaultBool(key string) bool {
	def, ok := settings.Lookup("credits", key)
	if !ok {
		// An undeclared key is a programming error, and the safe reading of "the limit I
		// cannot find" is the restrictive one.
		return false
	}
	parsed, err := strconv.ParseBool(strings.TrimSpace(def.Default))
	if err != nil {
		return false
	}
	return parsed
}

// settingDefaultInt reads one credits default from the settings registry.
func settingDefaultInt(key string) int64 {
	def, ok := settings.Lookup("credits", key)
	if !ok {
		return 0
	}
	parsed, err := strconv.ParseInt(strings.TrimSpace(def.Default), 10, 64)
	if err != nil || parsed < 0 {
		return 0
	}
	return parsed
}

// Querier is the read surface a settings load needs. A pool, a transaction and a
// connection all satisfy it.
type Querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// LoadSettings reads the credits settings, falling back to the code-side default for any
// key that is absent or unparseable.
//
// It takes a querier rather than using settings.Store because every caller reads these
// inside the transaction that will act on them: the maximum-balance check and the deposit it
// authorises must see one snapshot, and a read through the pool could see a limit that an
// operator changed between the check and the write. The *defaults* still come from the
// settings registry, so there is one declaration of what each key means.
//
// An unparseable value keeps the default rather than failing: an operator who typed
// "1O0" into a numeric setting should not lose the ability to take a deposit, and the
// audit trail already records who changed the setting.
func LoadSettings(ctx context.Context, q Querier) (Settings, error) {
	out := DefaultSettings()
	rows, err := q.Query(ctx, `SELECT key, value FROM settings WHERE group_key = 'credits'`)
	if err != nil {
		return out, fmt.Errorf("credit: read settings: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return out, fmt.Errorf("credit: scan setting: %w", err)
		}
		switch key {
		case "enabled":
			out.Enabled = parseBool(value, out.Enabled)
		case "minimum_deposit_minor":
			out.MinimumDepositMinor = parseInt(value, out.MinimumDepositMinor)
		case "maximum_deposit_minor":
			out.MaximumDepositMinor = parseInt(value, out.MaximumDepositMinor)
		case "maximum_credit_minor":
			out.MaximumCreditMinor = parseInt(value, out.MaximumCreditMinor)
		case "auto_use":
			out.AutoUse = parseBool(value, out.AutoUse)
		}
	}
	if err := rows.Err(); err != nil {
		return out, fmt.Errorf("credit: read settings: %w", err)
	}
	return out, nil
}

// parseBool reads a stored boolean, keeping the default for anything it cannot parse.
//
// strconv.ParseBool is used rather than a hand-written switch so the accepted spellings are
// the same ones the settings page validates against: a value that saved successfully must
// read back, and two parsers that disagree would make that untrue.
func parseBool(raw string, fallback bool) bool {
	parsed, err := strconv.ParseBool(strings.TrimSpace(raw))
	if err != nil {
		return fallback
	}
	return parsed
}

func parseInt(raw string, fallback int64) int64 {
	n, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || n < 0 {
		return fallback
	}
	return n
}

// Service performs credit operations.
type Service struct {
	pool *store.Pool
	// Now is injectable for tests.
	Now func() time.Time
	// auditor, when set, writes audit rows inside the transaction that made the change,
	// with the caller's actor, correlation id and source address.
	auditor audit.Auditor
	// gateway is the payment extension a deposit is started through. It is nil when no
	// gateway is configured, and only StartDeposit needs it.
	gateway Gateway
	// account scopes provider identifiers to one merchant account.
	account string
	// successURL and cancelURL are where the gateway returns the customer.
	successURL string
	cancelURL  string
}

// New constructs the service.
func New(pool *store.Pool) *Service {
	return &Service{pool: pool, Now: func() time.Time { return time.Now().UTC() }}
}

// WithAuditor returns a copy of the service that writes its audit rows through a.
func (s *Service) WithAuditor(a audit.Auditor) *Service {
	clone := *s
	clone.auditor = a
	return &clone
}

// Settings reads the credits settings through the service's pool.
func (s *Service) Settings(ctx context.Context) (Settings, error) {
	if s.pool == nil {
		return DefaultSettings(), nil
	}
	return LoadSettings(ctx, s.pool)
}

// Balance returns the customer's credit per currency, from the subledger.
//
// It is derived, never cached: the ledger is the authority and a stored balance would
// drift the first time a posting failed half way through a deploy.
func (s *Service) Balance(ctx context.Context, customerID string) ([]money.Amount, error) {
	if s.pool == nil {
		return nil, ErrInvalidRequest
	}
	return BalanceFor(ctx, s.pool, customerID)
}

// Entry is one movement on a customer's credit balance.
//
// It is read from the ledger rather than from a credit-specific table, because the ledger is what
// the balance is derived from: a transaction list assembled from anywhere else could disagree with
// the number above it, and the customer would be right to trust neither.
type Entry struct {
	ID       string
	Currency string
	// Amount is signed: positive when credit was added, negative when it was spent.
	Amount    money.Amount
	Memo      string
	CreatedAt time.Time
}

// TransactionsFor reads a customer's credit movements, newest first.
//
// `customer_credit` is a liability account, so credit is the positive side: a deposit or a grant
// credits it, spending it debits it. The sign is normalised here so a caller never has to know
// which side of the ledger it is reading — a spent credit appears as a negative movement, which is
// what a customer expects from a statement.
func TransactionsFor(ctx context.Context, q Querier, customerID string, limit int) ([]Entry, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := q.Query(ctx, `
		SELECT id::text, currency, credit_minor - debit_minor, memo, created_at
		FROM journal_entries
		WHERE customer_id = $1 AND account_code = $2
		ORDER BY created_at DESC, id DESC
		LIMIT $3`, customerID, ledger.AccountCredit, limit)
	if err != nil {
		return nil, fmt.Errorf("credit: read transactions: %w", err)
	}
	defer rows.Close()

	out := []Entry{}
	for rows.Next() {
		var (
			e     Entry
			minor int64
		)
		if err := rows.Scan(&e.ID, &e.Currency, &minor, &e.Memo, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("credit: scan transaction: %w", err)
		}
		cur, ok := money.LookupCurrency(e.Currency)
		if !ok {
			cur, _ = money.NewCurrency(e.Currency, 2)
		}
		e.Amount = money.New(minor, cur)
		out = append(out, e)
	}
	return out, rows.Err()
}

// BalanceFor reads a customer's credit per currency from any querier.
func BalanceFor(ctx context.Context, q Querier, customerID string) ([]money.Amount, error) {
	rows, err := q.Query(ctx, `
		SELECT currency, SUM(credit_minor - debit_minor) AS balance
		FROM journal_entries
		WHERE customer_id = $1 AND account_code = $2
		GROUP BY currency
		HAVING SUM(credit_minor - debit_minor) > 0
		ORDER BY currency`, customerID, ledger.AccountCredit)
	if err != nil {
		return nil, fmt.Errorf("credit: read balance: %w", err)
	}
	defer rows.Close()

	out := []money.Amount{}
	for rows.Next() {
		var (
			code  string
			minor int64
		)
		if err := rows.Scan(&code, &minor); err != nil {
			return nil, fmt.Errorf("credit: scan balance: %w", err)
		}
		cur, err := currencyOf(code)
		if err != nil {
			return nil, err
		}
		out = append(out, money.New(minor, cur))
	}
	return out, rows.Err()
}

// creditBalance reads one currency's credit balance. It takes a transaction because every
// caller must read it under the same lock as the write that follows.
func creditBalance(ctx context.Context, tx pgx.Tx, customerID string, cur money.Currency) (int64, error) {
	var minor int64
	err := tx.QueryRow(ctx, `
		SELECT COALESCE(SUM(credit_minor - debit_minor), 0)
		FROM journal_entries
		WHERE customer_id = $1 AND account_code = $2 AND currency = $3`,
		customerID, ledger.AccountCredit, cur.Code()).Scan(&minor)
	if err != nil {
		return 0, fmt.Errorf("credit: read credit balance: %w", err)
	}
	return minor, nil
}

// currencyOf resolves a persisted currency code.
func currencyOf(code string) (money.Currency, error) {
	if cur, ok := money.LookupCurrency(code); ok {
		return cur, nil
	}
	return money.NewCurrency(code, 2)
}

// resolveCurrency validates a currency code from a request body.
func resolveCurrency(code string) (money.Currency, error) {
	if strings.TrimSpace(code) == "" {
		return money.Currency{}, fmt.Errorf("%w: a currency is required", ErrInvalidRequest)
	}
	cur, err := currencyOf(code)
	if err != nil {
		return money.Currency{}, fmt.Errorf("%w: unknown currency %q", ErrInvalidRequest, code)
	}
	return cur, nil
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

// nullableUUID turns an empty id into SQL NULL.
func nullableUUID(id string) any {
	if id == "" {
		return nil
	}
	return id
}

// recordAudit writes the audit row through the caller's auditor when one is attached, and
// otherwise as a system action.
func (s *Service) recordAudit(ctx context.Context, tx pgx.Tx, action, targetType, targetID,
	actorID string, changes map[string]any) error {
	if s.auditor != nil {
		return s.auditor.RecordAction(ctx, tx, action, targetType, targetID, changes)
	}
	ev := audit.Event{
		Actor:      audit.Actor{ID: actorID, Kind: audit.ActorUser},
		Action:     action,
		TargetType: targetType,
		TargetID:   targetID,
		Outcome:    audit.OutcomeSuccess,
		Changes:    changes,
	}
	if actorID == "" {
		ev.Actor = audit.Actor{Kind: audit.ActorSystem}
	}
	return audit.Record(ctx, tx, ev)
}
