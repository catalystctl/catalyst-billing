// Currencies: configuration for rendering, never for arithmetic.
//
// `money.Amount` is integer minor units plus a currency code and scale. The one thing it
// cannot express is how a shop *displays* that number: "$12.34", "12,34 €", "¥1,234". That
// is what this table supplies, and it is all it supplies. No exchange rate is stored,
// because a price is explicit per currency and `money` refuses cross-currency arithmetic.
//
// Two rules are enforced here rather than left to the UI:
//
//   - **Exactly one default.** The migration's partial unique index makes "two defaults"
//     impossible; this file makes "no default" impossible, by promoting a replacement in
//     the same transaction that demotes the old one.
//   - **A currency with history is disabled, not deleted.** Price rows and invoices
//     reference a currency by code, so deleting one would orphan them. `enabled = false`
//     keeps the history readable and removes it from the switcher.

package commerce

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/catalyst/billing/internal/money"
	"github.com/jackc/pgx/v5"
)

// Currency is one configured currency.
type Currency struct {
	Code string `json:"code"`
	Name string `json:"name"`
	// Scale is the ISO 4217 minor-unit exponent. It is part of the row because a JPY amount
	// must never be rendered with two decimal places.
	Scale uint8 `json:"scale"`
	// Symbol is the prefix or suffix: "$", "€", "kr".
	Symbol string `json:"symbol"`
	// SymbolPosition is "prefix" ("$12.34") or "suffix" ("12.34 €").
	SymbolPosition string `json:"symbol_position"`
	// GroupSeparator and DecimalMark are literal strings. An empty group separator means no
	// grouping.
	GroupSeparator string `json:"group_separator"`
	DecimalMark    string `json:"decimal_mark"`
	// GroupSize is the digit count between separators; 0 disables grouping.
	GroupSize int       `json:"group_size"`
	IsDefault bool      `json:"is_default"`
	Enabled   bool      `json:"enabled"`
	SortOrder int       `json:"sort_order"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// CurrencyInput is the wire shape for creating or editing a currency.
//
// The code is only read on create: it is the primary key and is referenced by every price
// row, so renaming it is a data migration, not an edit.
type CurrencyInput struct {
	Code           string  `json:"code"`
	Name           string  `json:"name"`
	Scale          *uint8  `json:"scale"`
	Symbol         string  `json:"symbol"`
	SymbolPosition string  `json:"symbol_position"`
	GroupSeparator *string `json:"group_separator"`
	DecimalMark    *string `json:"decimal_mark"`
	GroupSize      *int    `json:"group_size"`
	IsDefault      *bool   `json:"is_default"`
	Enabled        *bool   `json:"enabled"`
	SortOrder      *int    `json:"sort_order"`
}

const currencyColumns = `
	code, name, scale, symbol, symbol_position, group_separator, decimal_mark, group_size,
	is_default, enabled, sort_order, created_at, updated_at`

func scanCurrency(row pgx.Row) (Currency, error) {
	var c Currency
	var scale int
	if err := row.Scan(&c.Code, &c.Name, &scale, &c.Symbol, &c.SymbolPosition,
		&c.GroupSeparator, &c.DecimalMark, &c.GroupSize, &c.IsDefault, &c.Enabled,
		&c.SortOrder, &c.CreatedAt, &c.UpdatedAt); err != nil {
		return Currency{}, err
	}
	c.Scale = uint8(scale)
	return c, nil
}

// Currencies lists every configured currency, default first then by sort order. Staff read.
func (s *Store) Currencies(ctx context.Context) ([]Currency, error) {
	return s.currenciesWith(ctx, s.pool, false)
}

// EnabledCurrencies lists the currencies offered at checkout, for the public switcher.
func (s *Store) EnabledCurrencies(ctx context.Context) ([]Currency, error) {
	return s.currenciesWith(ctx, s.pool, true)
}

func (s *Store) currenciesWith(ctx context.Context, q querier, enabledOnly bool) ([]Currency, error) {
	stmt := `SELECT ` + currencyColumns + ` FROM currencies`
	if enabledOnly {
		stmt += ` WHERE enabled`
	}
	stmt += ` ORDER BY is_default DESC, sort_order, code`
	rows, err := q.Query(ctx, stmt)
	if err != nil {
		return nil, fmt.Errorf("commerce: list currencies: %w", err)
	}
	defer rows.Close()
	out := make([]Currency, 0, 8)
	for rows.Next() {
		c, err := scanCurrency(rows)
		if err != nil {
			return nil, fmt.Errorf("commerce: scan currency: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// Currency reads one currency by code.
func (s *Store) Currency(ctx context.Context, code string) (Currency, error) {
	return s.currencyWith(ctx, s.pool, code)
}

func (s *Store) currencyWith(ctx context.Context, q querier, code string) (Currency, error) {
	c, err := scanCurrency(q.QueryRow(ctx,
		`SELECT `+currencyColumns+` FROM currencies WHERE code = $1`,
		strings.ToUpper(strings.TrimSpace(code))))
	if errors.Is(err, pgx.ErrNoRows) {
		return Currency{}, ErrNotFound
	}
	if err != nil {
		return Currency{}, fmt.Errorf("commerce: read currency: %w", err)
	}
	return c, nil
}

// DefaultCurrency returns the configured default, which is what a new price is created in.
func (s *Store) DefaultCurrency(ctx context.Context) (Currency, error) {
	c, err := scanCurrency(s.pool.QueryRow(ctx,
		`SELECT `+currencyColumns+` FROM currencies WHERE is_default`))
	if errors.Is(err, pgx.ErrNoRows) {
		// No default is a configuration state, not a missing record: the migration seeds
		// USD as the default, so this only happens after an operator cleared it.
		return Currency{}, ErrNotFound
	}
	if err != nil {
		return Currency{}, fmt.Errorf("commerce: read default currency: %w", err)
	}
	return c, nil
}

// Format renders a minor-unit amount in this currency's configured style.
//
// The rendering is string insertion over the exact integer, exactly like
// `money.Amount.String`: there is no float and no `number_format`. It returns the bare
// number when no symbol is configured, so a currency row that only sets a scale is still
// useful.
func (c Currency) Format(minor int64) string {
	scale := int(c.Scale)
	negative := minor < 0
	var magnitude uint64
	if negative {
		magnitude = uint64(-(minor + 1)) + 1
	} else {
		magnitude = uint64(minor)
	}
	digits := strconv.FormatUint(magnitude, 10)
	if scale > 0 {
		for len(digits) <= scale {
			digits = "0" + digits
		}
		digits = digits[:len(digits)-scale] + c.decimalMarkOrDot() + digits[len(digits)-scale:]
	}
	group := c.GroupSeparator
	size := c.GroupSize
	if group != "" && size > 0 {
		// Group the integer part only. The decimal mark may be a comma, so split on it
		// rather than on a fixed offset.
		mark := c.decimalMarkOrDot()
		intPart, frac := digits, ""
		if i := strings.Index(digits, mark); i >= 0 {
			intPart, frac = digits[:i], digits[i:]
		}
		var b strings.Builder
		for i, r := range intPart {
			if i > 0 && (len(intPart)-i)%size == 0 {
				b.WriteString(group)
			}
			b.WriteRune(r)
		}
		digits = b.String() + frac
	}
	if c.Symbol == "" {
		if negative {
			return "-" + digits
		}
		return digits
	}
	if c.SymbolPosition == "suffix" {
		if negative {
			return "-" + digits + " " + c.Symbol
		}
		return digits + " " + c.Symbol
	}
	if negative {
		// The sign belongs outside the symbol: "-$1,234.56", not "$-1,234.56".
		return "-" + c.Symbol + digits
	}
	return c.Symbol + digits
}

func (c Currency) decimalMarkOrDot() string {
	if c.DecimalMark == "" {
		return "."
	}
	return c.DecimalMark
}

// Money returns the money.Currency this row describes, for arithmetic.
func (c Currency) Money() (money.Currency, error) {
	return money.NewCurrency(c.Code, c.Scale)
}

// Format renders an amount using the configured style, falling back to the money package's
// own rendering when the code is not configured.
func (s *Store) Format(ctx context.Context, code string, minor int64) (string, error) {
	c, err := s.Currency(ctx, code)
	if err != nil {
		return "", err
	}
	return c.Format(minor), nil
}

// CreateCurrency adds a currency.
func (s *Store) CreateCurrency(ctx context.Context, in CurrencyInput) (Currency, error) {
	var out Currency
	err := s.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = s.CreateCurrencyTx(ctx, tx, in)
		return err
	})
	return out, err
}

// CreateCurrencyTx adds a currency inside a caller's transaction.
func (s *Store) CreateCurrencyTx(ctx context.Context, q querier, in CurrencyInput) (Currency, error) {
	code := strings.ToUpper(strings.TrimSpace(in.Code))
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return Currency{}, invalid("name is required")
	}
	// The scale is validated by money, which is the single definition of a legal exponent.
	scale := currencyScale(code)
	if in.Scale != nil {
		scale = *in.Scale
	}
	cur, err := money.NewCurrency(code, scale)
	if err != nil {
		return Currency{}, invalid("%v", err)
	}
	position := strings.TrimSpace(in.SymbolPosition)
	if position == "" {
		position = "prefix"
	}
	if position != "prefix" && position != "suffix" {
		return Currency{}, invalid("symbol_position must be prefix or suffix")
	}
	groupSep := ","
	if in.GroupSeparator != nil {
		groupSep = *in.GroupSeparator
	}
	decimalMark := "."
	if in.DecimalMark != nil {
		decimalMark = *in.DecimalMark
	}
	if groupSep == decimalMark {
		return Currency{}, invalid("the group separator and the decimal mark must differ")
	}
	groupSize := 3
	if in.GroupSize != nil {
		groupSize = *in.GroupSize
	}
	if groupSize < 0 || groupSize > 4 {
		return Currency{}, invalid("group_size must be between 0 and 4")
	}
	sortOrder := 0
	if in.SortOrder != nil {
		sortOrder = *in.SortOrder
	}
	enabled := true
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	isDefault := in.IsDefault != nil && *in.IsDefault

	var exists bool
	err = q.QueryRow(ctx, `SELECT true FROM currencies WHERE code = $1`, cur.Code()).Scan(&exists)
	if err == nil {
		return Currency{}, ErrCodeTaken
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Currency{}, fmt.Errorf("commerce: check currency: %w", err)
	}

	if isDefault {
		// Demote the incumbent in the same transaction: the partial unique index would
		// otherwise reject the insert, and "no default" must never be observable.
		if _, err := q.Exec(ctx, `UPDATE currencies SET is_default = false WHERE is_default`); err != nil {
			return Currency{}, fmt.Errorf("commerce: clear default currency: %w", err)
		}
	}
	if _, err := q.Exec(ctx, `
		INSERT INTO currencies (code, name, scale, symbol, symbol_position, group_separator,
			decimal_mark, group_size, is_default, enabled, sort_order)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
		cur.Code(), name, scale, strings.TrimSpace(in.Symbol), position, groupSep,
		decimalMark, groupSize, isDefault, enabled, sortOrder); err != nil {
		if isUniqueViolation(err) {
			return Currency{}, ErrCodeTaken
		}
		if isCheckViolation(err) {
			return Currency{}, invalid("the currency violates a constraint")
		}
		return Currency{}, fmt.Errorf("commerce: create currency: %w", err)
	}
	return s.currencyWith(ctx, q, cur.Code())
}

// UpdateCurrency edits a currency's presentation and default state.
//
// A currency that is the default cannot be disabled: a storefront with no default has no
// currency to create a price in. Disabling the default promotes another enabled currency
// instead, in the same transaction, so the invariant "exactly one default" holds at every
// commit boundary.
func (s *Store) UpdateCurrency(ctx context.Context, code string, in CurrencyInput) (Currency, error) {
	var out Currency
	err := s.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = s.UpdateCurrencyTx(ctx, tx, code, in)
		return err
	})
	return out, err
}

// UpdateCurrencyTx edits a currency inside a caller's transaction.
func (s *Store) UpdateCurrencyTx(ctx context.Context, q querier, code string, in CurrencyInput) (Currency, error) {
	current, err := s.currencyWith(ctx, q, code)
	if err != nil {
		return Currency{}, err
	}
	name := current.Name
	if strings.TrimSpace(in.Name) != "" {
		name = strings.TrimSpace(in.Name)
	}
	// An absent symbol means "keep it", not "clear it": a PUT that only changes the grouping
	// must not blank the symbol as a side effect. Clearing is not expressible, and a currency
	// with no symbol is a configuration mistake rather than a state anyone wants.
	symbol := current.Symbol
	if strings.TrimSpace(in.Symbol) != "" {
		symbol = strings.TrimSpace(in.Symbol)
	}
	scale := current.Scale
	if in.Scale != nil {
		scale = *in.Scale
	}
	if scale != current.Scale {
		// Changing the exponent reinterprets every stored minor amount: 1000 USD is 10.00,
		// 1000 JPY is 1000. That is a data migration, not an edit.
		return Currency{}, fmt.Errorf("%w: changing the scale would reinterpret stored amounts", ErrInUse)
	}
	position := current.SymbolPosition
	if strings.TrimSpace(in.SymbolPosition) != "" {
		position = strings.TrimSpace(in.SymbolPosition)
	}
	if position != "prefix" && position != "suffix" {
		return Currency{}, invalid("symbol_position must be prefix or suffix")
	}
	groupSep := current.GroupSeparator
	if in.GroupSeparator != nil {
		groupSep = *in.GroupSeparator
	}
	decimalMark := current.DecimalMark
	if in.DecimalMark != nil {
		decimalMark = *in.DecimalMark
	}
	if groupSep == decimalMark {
		return Currency{}, invalid("the group separator and the decimal mark must differ")
	}
	groupSize := current.GroupSize
	if in.GroupSize != nil {
		groupSize = *in.GroupSize
	}
	if groupSize < 0 || groupSize > 4 {
		return Currency{}, invalid("group_size must be between 0 and 4")
	}
	sortOrder := current.SortOrder
	if in.SortOrder != nil {
		sortOrder = *in.SortOrder
	}
	enabled := current.Enabled
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	isDefault := current.IsDefault
	if in.IsDefault != nil {
		isDefault = *in.IsDefault
	}
	if !enabled && isDefault {
		return Currency{}, fmt.Errorf("%w: the default currency cannot be disabled; make another currency the default first", ErrInUse)
	}
	if !enabled && !isDefault {
		// Fine: a disabled currency keeps its history and leaves the switcher.
	}
	if isDefault && !current.IsDefault {
		if _, err := q.Exec(ctx, `UPDATE currencies SET is_default = false WHERE is_default`); err != nil {
			return Currency{}, fmt.Errorf("commerce: clear default currency: %w", err)
		}
	}
	if !isDefault && current.IsDefault {
		// Demoting the default would leave none. Promote the next enabled currency instead,
		// which is what an operator means by "USD is not the default any more". The demotion
		// happens first because the partial unique index allows exactly one default row at a
		// time, so promoting before demoting is a unique violation.
		var replacement string
		err := q.QueryRow(ctx, `
			SELECT code FROM currencies
			 WHERE code <> $1 AND enabled
			 ORDER BY sort_order, code LIMIT 1`, current.Code).Scan(&replacement)
		if errors.Is(err, pgx.ErrNoRows) {
			return Currency{}, fmt.Errorf("%w: another enabled currency is required before the default can change", ErrInUse)
		}
		if err != nil {
			return Currency{}, fmt.Errorf("commerce: choose replacement default: %w", err)
		}
		if _, err := q.Exec(ctx,
			`UPDATE currencies SET is_default = false WHERE code = $1`, current.Code); err != nil {
			return Currency{}, fmt.Errorf("commerce: demote default currency: %w", err)
		}
		if _, err := q.Exec(ctx,
			`UPDATE currencies SET is_default = true WHERE code = $1`, replacement); err != nil {
			return Currency{}, fmt.Errorf("commerce: promote default currency: %w", err)
		}
	}
	if _, err := q.Exec(ctx, `
		UPDATE currencies
		   SET name = $2, symbol = $3, symbol_position = $4, group_separator = $5,
		       decimal_mark = $6, group_size = $7, is_default = $8, enabled = $9,
		       sort_order = $10, updated_at = now()
		 WHERE code = $1`,
		current.Code, name, symbol, position, groupSep, decimalMark,
		groupSize, isDefault, enabled, sortOrder); err != nil {
		if isCheckViolation(err) {
			return Currency{}, invalid("the currency violates a constraint")
		}
		return Currency{}, fmt.Errorf("commerce: update currency: %w", err)
	}
	return s.currencyWith(ctx, q, current.Code)
}

// DeleteCurrency removes a currency that has never been used.
//
// A currency referenced by a price version or an invoice is refused, because deleting it
// would orphan a document that says "12.00 XYZ". The supported way to stop offering one is
// `enabled = false`.
func (s *Store) DeleteCurrency(ctx context.Context, code string) error {
	return s.DeleteCurrencyTx(ctx, s.pool, code)
}

// DeleteCurrencyTx removes a currency inside a caller's transaction.
func (s *Store) DeleteCurrencyTx(ctx context.Context, q querier, code string) error {
	current, err := s.currencyWith(ctx, q, code)
	if err != nil {
		return err
	}
	if current.IsDefault {
		return fmt.Errorf("%w: the default currency cannot be deleted", ErrInUse)
	}
	var prices, invoices int
	if err := q.QueryRow(ctx,
		`SELECT count(*) FROM price_versions WHERE currency = $1`, current.Code).Scan(&prices); err != nil {
		return fmt.Errorf("commerce: count prices: %w", err)
	}
	if err := q.QueryRow(ctx,
		`SELECT count(*) FROM invoices WHERE currency = $1`, current.Code).Scan(&invoices); err != nil {
		return fmt.Errorf("commerce: count invoices: %w", err)
	}
	if prices > 0 || invoices > 0 {
		return fmt.Errorf("%w: %d price(s) and %d invoice(s) use it", ErrInUse, prices, invoices)
	}
	tag, err := q.Exec(ctx, `DELETE FROM currencies WHERE code = $1`, current.Code)
	if err != nil {
		return fmt.Errorf("commerce: delete currency: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
