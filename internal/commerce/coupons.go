// Coupons: a price modifier with a life.
//
// The table in 0020_commerce.sql explains why a coupon is not a discount column. The
// consequence for this file is that it has three distinct jobs, and mixing them is how a
// promotion system starts lying to customers:
//
//  1. **Configuration** — create/update/list/get. There is no delete: a retired coupon is
//     `enabled = false`, because the invoices it discounted reference it and an invoice
//     whose discount cannot be explained is not a valid document.
//  2. **Validation** — "would this code reduce this amount for this customer?" It returns
//     an exact discount or a typed refusal (`ErrExpired`, `ErrExhausted`,
//     `ErrNotApplicable`), never a boolean plus a guess.
//  3. **Redemption** — recording a use. This is the authoritative check, and it runs under
//     a row lock on the coupon so two concurrent checkouts cannot both take the last use.
//
// Percentages are exact rationals. `money.ParseRatio("12.5%")` is 1/8, and the discount is
// `money.ApplyRate` with an explicit rounding mode, so a 12.5% discount of 10.00 is exactly
// 1.25 and never 1.2499999.

package commerce

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

// Coupon kinds.
const (
	CouponPercentage = "percentage"
	CouponFixed      = "fixed"
)

// Coupon scopes: which part of a price the discount reduces.
const (
	AppliesToPrice = "price"
	AppliesToSetup = "setup"
	AppliesToAll   = "all"
)

// couponCodePattern is deliberately permissive but anchored: a code is a token a customer
// types, so spaces and punctuation are refused rather than silently trimmed.
var couponCodePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{1,63}$`)

// Coupon is one promotion.
type Coupon struct {
	ID          string `json:"id"`
	Code        string `json:"code"`
	Description string `json:"description"`
	Kind        string `json:"kind"`
	// Percentage is the exact rational percentage string ("10", "12.5") for a percentage
	// coupon, and empty for a fixed one.
	Percentage string `json:"percentage,omitempty"`
	// AmountMinor is the fixed discount in minor units of Currency.
	AmountMinor *int64 `json:"amount_minor,omitempty"`
	Currency    string `json:"currency,omitempty"`
	AppliesTo   string `json:"applies_to"`
	MaxUses     *int   `json:"max_uses,omitempty"`
	// MaxUsesPerUser is enforced by counting redemptions per customer under the same lock.
	MaxUsesPerUser *int       `json:"max_uses_per_user,omitempty"`
	StartsAt       *time.Time `json:"starts_at,omitempty"`
	ExpiresAt      *time.Time `json:"expires_at,omitempty"`
	// RecurringCycles is the "first N cycles" rule: nil means every cycle, 1 means the
	// first invoice only. The renewal engine reads it together with the subscription's
	// applied-cycle counter.
	RecurringCycles *int `json:"recurring_cycles,omitempty"`
	// Enabled false is "retired": it keeps its history and stops accepting new uses.
	Enabled    bool      `json:"enabled"`
	ProductIDs []string  `json:"product_ids"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// CouponInput is the wire shape for creating or updating a coupon.
//
// On update, an absent field means "leave it alone": a `nil` limit keeps the stored one, and
// an empty string keeps the stored text. That is deliberately not the same as "clear it",
// because a Go decoder cannot tell `null` from an omitted key without a second pass, and
// guessing wrong would silently drop a usage cap. Clearing a limit is therefore expressed by
// retiring the coupon and creating its replacement — which is also the auditable way to change
// a promotion's terms after it has been redeemed.
type CouponInput struct {
	Code            string     `json:"code"`
	Description     string     `json:"description"`
	Kind            string     `json:"kind"`
	Percentage      string     `json:"percentage"`
	AmountMinor     *int64     `json:"amount_minor"`
	Currency        string     `json:"currency"`
	AppliesTo       string     `json:"applies_to"`
	MaxUses         *int       `json:"max_uses"`
	MaxUsesPerUser  *int       `json:"max_uses_per_user"`
	StartsAt        *time.Time `json:"starts_at"`
	ExpiresAt       *time.Time `json:"expires_at"`
	RecurringCycles *int       `json:"recurring_cycles"`
	Enabled         *bool      `json:"enabled"`
	// ProductIDs replaces the restriction set. nil means "leave it alone" on update and
	// "no restriction" on create; an empty non-nil slice clears it.
	ProductIDs *[]string `json:"product_ids"`
}

// DiscountRequest asks what a code is worth for one purchase line.
type DiscountRequest struct {
	Code       string
	CustomerID string
	ProductID  string
	// Amount is the recurring price the coupon may reduce.
	Amount money.Amount
	// Setup is the one-off setup fee the coupon may reduce. A zero value (no currency) is
	// treated as "no setup fee".
	Setup money.Amount
	// At is the instant the coupon is judged against. Zero means the store's clock.
	At time.Time
}

// Discount is the exact outcome of a successful validation.
type Discount struct {
	CouponID string
	Code     string
	// Price and Setup are the reductions applied to each component. Total is their sum and
	// is what the cart shows.
	Price    money.Amount
	Setup    money.Amount
	Total    money.Amount
	Currency string
	// AppliesTo and RecurringCycles are carried so the caller can explain the discount on
	// the invoice and decide whether a later renewal still gets it.
	AppliesTo       string
	RecurringCycles *int
}

// ------------------------------------------------------------------ read --

// couponColumns is the projection every read shares, so a new column cannot be added to
// one query and forgotten in another.
const couponColumns = `
	id, code, description, kind, COALESCE(percentage, ''), amount_minor, COALESCE(currency, ''),
	applies_to, max_uses, max_uses_per_user, starts_at, expires_at, recurring_cycles,
	enabled, created_at, updated_at`

func scanCoupon(row pgx.Row) (Coupon, error) {
	var c Coupon
	if err := row.Scan(&c.ID, &c.Code, &c.Description, &c.Kind, &c.Percentage,
		&c.AmountMinor, &c.Currency, &c.AppliesTo, &c.MaxUses, &c.MaxUsesPerUser,
		&c.StartsAt, &c.ExpiresAt, &c.RecurringCycles, &c.Enabled,
		&c.CreatedAt, &c.UpdatedAt); err != nil {
		return Coupon{}, err
	}
	return c, nil
}

// Coupon reads one coupon by id.
func (s *Store) Coupon(ctx context.Context, id string) (Coupon, error) {
	return s.couponWith(ctx, s.pool, id)
}

func (s *Store) couponWith(ctx context.Context, q querier, id string) (Coupon, error) {
	c, err := scanCoupon(q.QueryRow(ctx, `SELECT `+couponColumns+` FROM coupons WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Coupon{}, ErrNotFound
	}
	if err != nil {
		return Coupon{}, fmt.Errorf("commerce: read coupon: %w", err)
	}
	if c.ProductIDs, err = s.couponProducts(ctx, q, c.ID); err != nil {
		return Coupon{}, err
	}
	return c, nil
}

// CouponByCode reads one coupon by its code, case-insensitively, as the unique index does.
func (s *Store) CouponByCode(ctx context.Context, code string) (Coupon, error) {
	return s.couponByCodeWith(ctx, s.pool, code)
}

func (s *Store) couponByCodeWith(ctx context.Context, q querier, code string) (Coupon, error) {
	c, err := scanCoupon(q.QueryRow(ctx,
		`SELECT `+couponColumns+` FROM coupons WHERE upper(code) = upper($1)`,
		strings.TrimSpace(code)))
	if errors.Is(err, pgx.ErrNoRows) {
		return Coupon{}, ErrNotFound
	}
	if err != nil {
		return Coupon{}, fmt.Errorf("commerce: read coupon by code: %w", err)
	}
	if c.ProductIDs, err = s.couponProducts(ctx, q, c.ID); err != nil {
		return Coupon{}, err
	}
	return c, nil
}

// Coupons lists every coupon, retired ones included, newest first. Staff-only surface: a
// customer never sees a promotion that has not been applied to their cart.
func (s *Store) Coupons(ctx context.Context) ([]Coupon, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+couponColumns+` FROM coupons ORDER BY created_at DESC, code`)
	if err != nil {
		return nil, fmt.Errorf("commerce: list coupons: %w", err)
	}
	defer rows.Close()
	out := make([]Coupon, 0, 16)
	for rows.Next() {
		c, err := scanCoupon(rows)
		if err != nil {
			return nil, fmt.Errorf("commerce: scan coupon: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("commerce: read coupons: %w", err)
	}
	for i := range out {
		if out[i].ProductIDs, err = s.couponProducts(ctx, s.pool, out[i].ID); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (s *Store) couponProducts(ctx context.Context, q querier, couponID string) ([]string, error) {
	rows, err := q.Query(ctx,
		`SELECT product_id FROM coupon_products WHERE coupon_id = $1 ORDER BY product_id`, couponID)
	if err != nil {
		return nil, fmt.Errorf("commerce: list coupon products: %w", err)
	}
	defer rows.Close()
	out := make([]string, 0, 4)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("commerce: scan coupon product: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ----------------------------------------------------------------- write --

// CreateCoupon inserts a coupon and its product restrictions.
func (s *Store) CreateCoupon(ctx context.Context, in CouponInput) (Coupon, error) {
	var out Coupon
	err := s.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = s.CreateCouponTx(ctx, tx, in)
		return err
	})
	return out, err
}

// CreateCouponTx inserts a coupon inside a caller's transaction.
func (s *Store) CreateCouponTx(ctx context.Context, q querier, in CouponInput) (Coupon, error) {
	normalised, err := normaliseCoupon(in, true)
	if err != nil {
		return Coupon{}, err
	}
	enabled := true
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	var id string
	err = q.QueryRow(ctx, `
		INSERT INTO coupons (code, description, kind, percentage, amount_minor, currency,
			applies_to, max_uses, max_uses_per_user, starts_at, expires_at, recurring_cycles,
			enabled)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
		RETURNING id`,
		normalised.Code, normalised.Description, normalised.Kind, normalised.percentage,
		normalised.AmountMinor, normalised.currency, normalised.AppliesTo, in.MaxUses,
		in.MaxUsesPerUser, in.StartsAt, in.ExpiresAt, in.RecurringCycles, enabled).Scan(&id)
	if err != nil {
		if isUniqueViolation(err) {
			return Coupon{}, ErrCodeTaken
		}
		if isCheckViolation(err) {
			return Coupon{}, invalid("the coupon values violate a constraint")
		}
		return Coupon{}, fmt.Errorf("commerce: create coupon: %w", err)
	}
	if in.ProductIDs != nil {
		if err := s.replaceCouponProducts(ctx, q, id, *in.ProductIDs); err != nil {
			return Coupon{}, err
		}
	}
	return s.couponWith(ctx, q, id)
}

// UpdateCoupon edits a coupon. It is also how a coupon is retired: `enabled: false`.
//
// The code is editable, because a code is not referenced by an invoice — the redemption
// row references the coupon's id, so renaming the promotion keeps history intact.
func (s *Store) UpdateCoupon(ctx context.Context, id string, in CouponInput) (Coupon, error) {
	var out Coupon
	err := s.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = s.UpdateCouponTx(ctx, tx, id, in)
		return err
	})
	return out, err
}

// UpdateCouponTx edits a coupon inside a caller's transaction.
func (s *Store) UpdateCouponTx(ctx context.Context, q querier, id string, in CouponInput) (Coupon, error) {
	current, err := s.couponWith(ctx, q, id)
	if err != nil {
		return Coupon{}, err
	}
	// `kind` decides how `value` is interpreted, so switching it in place would reinterpret
	// every historic redemption's meaning. A promotion that changes shape is a new promotion;
	// the old one is retired so its invoices stay explainable.
	if k := strings.TrimSpace(in.Kind); k != "" && k != current.Kind {
		return Coupon{}, invalid(
			"kind cannot change from %q to %q; retire this coupon and create a replacement",
			current.Kind, k)
	}
	// A partial update must not accidentally clear fields it did not mention, so the
	// input is merged onto the stored row before it is validated.
	merged := CouponInput{
		Code:            firstNonEmpty(in.Code, current.Code),
		Description:     firstNonEmpty(in.Description, current.Description),
		Kind:            firstNonEmpty(in.Kind, current.Kind),
		Percentage:      firstNonEmpty(in.Percentage, current.Percentage),
		AmountMinor:     in.AmountMinor,
		Currency:        firstNonEmpty(in.Currency, current.Currency),
		AppliesTo:       firstNonEmpty(in.AppliesTo, current.AppliesTo),
		MaxUses:         in.MaxUses,
		MaxUsesPerUser:  in.MaxUsesPerUser,
		StartsAt:        in.StartsAt,
		ExpiresAt:       in.ExpiresAt,
		RecurringCycles: in.RecurringCycles,
		Enabled:         in.Enabled,
		ProductIDs:      in.ProductIDs,
	}
	if in.AmountMinor == nil {
		merged.AmountMinor = current.AmountMinor
	}
	if in.MaxUses == nil {
		merged.MaxUses = current.MaxUses
	}
	if in.MaxUsesPerUser == nil {
		merged.MaxUsesPerUser = current.MaxUsesPerUser
	}
	if in.StartsAt == nil {
		merged.StartsAt = current.StartsAt
	}
	if in.ExpiresAt == nil {
		merged.ExpiresAt = current.ExpiresAt
	}
	if in.RecurringCycles == nil {
		merged.RecurringCycles = current.RecurringCycles
	}
	normalised, err := normaliseCoupon(merged, false)
	if err != nil {
		return Coupon{}, err
	}
	enabled := current.Enabled
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	_, err = q.Exec(ctx, `
		UPDATE coupons
		   SET code = $2, description = $3, kind = $4, percentage = $5, amount_minor = $6,
		       currency = $7, applies_to = $8, max_uses = $9, max_uses_per_user = $10,
		       starts_at = $11, expires_at = $12, recurring_cycles = $13, enabled = $14,
		       updated_at = now()
		 WHERE id = $1`,
		id, normalised.Code, normalised.Description, normalised.Kind, normalised.percentage,
		normalised.AmountMinor, normalised.currency, normalised.AppliesTo, merged.MaxUses,
		merged.MaxUsesPerUser, merged.StartsAt, merged.ExpiresAt, merged.RecurringCycles, enabled)
	if err != nil {
		if isUniqueViolation(err) {
			return Coupon{}, ErrCodeTaken
		}
		if isCheckViolation(err) {
			return Coupon{}, invalid("the coupon values violate a constraint")
		}
		return Coupon{}, fmt.Errorf("commerce: update coupon: %w", err)
	}
	if in.ProductIDs != nil {
		if err := s.replaceCouponProducts(ctx, q, id, *in.ProductIDs); err != nil {
			return Coupon{}, err
		}
	}
	return s.couponWith(ctx, q, id)
}

func (s *Store) replaceCouponProducts(ctx context.Context, q querier, couponID string, productIDs []string) error {
	if _, err := q.Exec(ctx, `DELETE FROM coupon_products WHERE coupon_id = $1`, couponID); err != nil {
		return fmt.Errorf("commerce: clear coupon products: %w", err)
	}
	seen := make(map[string]struct{}, len(productIDs))
	for _, productID := range productIDs {
		productID = strings.TrimSpace(productID)
		if productID == "" {
			continue
		}
		if _, dup := seen[productID]; dup {
			continue
		}
		seen[productID] = struct{}{}
		if _, err := q.Exec(ctx,
			`INSERT INTO coupon_products (coupon_id, product_id) VALUES ($1, $2)`,
			couponID, productID); err != nil {
			if isForeignKeyViolation(err) {
				return invalid("product %s does not exist", productID)
			}
			return fmt.Errorf("commerce: restrict coupon to product: %w", err)
		}
	}
	return nil
}

// normalisedCoupon is the validated form of an input, before it reaches SQL.
type normalisedCoupon struct {
	Code        string
	Description string
	Kind        string
	percentage  *string
	AmountMinor *int64
	currency    *string
	AppliesTo   string
}

// normaliseCoupon validates an input and separates the percentage and fixed columns so the
// database CHECK can never be the first thing to notice a bad request.
func normaliseCoupon(in CouponInput, create bool) (normalisedCoupon, error) {
	out := normalisedCoupon{
		Code:        strings.TrimSpace(in.Code),
		Description: strings.TrimSpace(in.Description),
		Kind:        strings.TrimSpace(in.Kind),
		AppliesTo:   strings.TrimSpace(in.AppliesTo),
	}
	if !couponCodePattern.MatchString(out.Code) {
		return normalisedCoupon{}, invalid(
			"code must be 2-64 characters of letters, digits, hyphens and underscores")
	}
	if out.Kind != CouponPercentage && out.Kind != CouponFixed {
		return normalisedCoupon{}, invalid("kind must be %q or %q", CouponPercentage, CouponFixed)
	}
	if out.AppliesTo == "" {
		out.AppliesTo = AppliesToAll
	}
	switch out.AppliesTo {
	case AppliesToPrice, AppliesToSetup, AppliesToAll:
	default:
		return normalisedCoupon{}, invalid("applies_to must be price, setup or all")
	}
	switch out.Kind {
	case CouponPercentage:
		raw := strings.TrimSpace(in.Percentage)
		if raw == "" {
			return normalisedCoupon{}, invalid("a percentage coupon needs a percentage")
		}
		ratio, err := money.ParseRatio(raw + "%")
		if err != nil {
			return normalisedCoupon{}, invalid("percentage %q is not a number", raw)
		}
		if ratio.IsNegative() || ratio.IsZero() {
			return normalisedCoupon{}, invalid("percentage must be greater than zero")
		}
		// 100% is the ceiling: a larger percentage would make the shop pay the customer.
		if ratio.Float64() > 1 {
			return normalisedCoupon{}, invalid("percentage must not exceed 100")
		}
		// Store the rational in its canonical decimal form so the column stays readable
		// ("12.5", not "25/2") while remaining exact.
		out.percentage = &raw
		if in.AmountMinor != nil {
			return normalisedCoupon{}, invalid("a percentage coupon must not carry an amount")
		}
		if strings.TrimSpace(in.Currency) != "" {
			return normalisedCoupon{}, invalid("a percentage coupon is currency-independent")
		}
	case CouponFixed:
		if in.AmountMinor == nil || *in.AmountMinor <= 0 {
			return normalisedCoupon{}, invalid("a fixed coupon needs a positive amount_minor")
		}
		currency := strings.ToUpper(strings.TrimSpace(in.Currency))
		if currency == "" {
			return normalisedCoupon{}, invalid("a fixed coupon needs a currency")
		}
		if _, err := money.NewCurrency(currency, currencyScale(currency)); err != nil {
			return normalisedCoupon{}, invalid("currency %q is not valid", currency)
		}
		if strings.TrimSpace(in.Percentage) != "" {
			return normalisedCoupon{}, invalid("a fixed coupon must not carry a percentage")
		}
		out.AmountMinor = in.AmountMinor
		out.currency = &currency
	}
	if in.MaxUses != nil && *in.MaxUses <= 0 {
		return normalisedCoupon{}, invalid("max_uses must be positive when set")
	}
	if in.MaxUsesPerUser != nil && *in.MaxUsesPerUser <= 0 {
		return normalisedCoupon{}, invalid("max_uses_per_user must be positive when set")
	}
	if in.RecurringCycles != nil && *in.RecurringCycles <= 0 {
		return normalisedCoupon{}, invalid("recurring_cycles must be positive when set")
	}
	if in.StartsAt != nil && in.ExpiresAt != nil && !in.ExpiresAt.After(*in.StartsAt) {
		return normalisedCoupon{}, invalid("expires_at must be after starts_at")
	}
	if !create && out.Code == "" {
		return normalisedCoupon{}, invalid("code is required")
	}
	return out, nil
}

// currencyScale returns the ISO minor-unit exponent for a code, defaulting to 2 for a code
// the money package does not know. The currency table is the authority at runtime; this is
// only used to validate the code's shape during coupon validation.
func currencyScale(code string) uint8 {
	if cur, ok := money.LookupCurrency(code); ok {
		return cur.Scale()
	}
	return 2
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// -------------------------------------------------------------- validate --

// ValidateCoupon answers "what is this code worth here?".
//
// The order of the checks is the order a customer experiences them, and each refusal is a
// distinct typed error so the cart can say "expired" rather than "invalid code".
//
// The usage counts are read here for a live answer; the authoritative enforcement is
// Redeem, under a row lock. Reading them here is not a race that matters: the cart is
// showing a price, and the checkout re-checks.
func (s *Store) ValidateCoupon(ctx context.Context, req DiscountRequest) (Discount, error) {
	return s.validateCouponWith(ctx, s.pool, req)
}

func (s *Store) validateCouponWith(ctx context.Context, q querier, req DiscountRequest) (Discount, error) {
	if strings.TrimSpace(req.Code) == "" {
		return Discount{}, invalid("a coupon code is required")
	}
	coupon, err := s.couponByCodeWith(ctx, q, req.Code)
	if err != nil {
		return Discount{}, err
	}
	at := req.At
	if at.IsZero() {
		at = s.now()
	}
	at = at.UTC()

	if !coupon.Enabled {
		// A retired coupon is indistinguishable from an unknown one to a customer: the
		// promotion no longer exists as far as the storefront is concerned.
		return Discount{}, ErrNotFound
	}
	if coupon.StartsAt != nil && at.Before(*coupon.StartsAt) {
		return Discount{}, fmt.Errorf("%w: it starts at %s", ErrExpired,
			coupon.StartsAt.UTC().Format(time.RFC3339))
	}
	if coupon.ExpiresAt != nil && !at.Before(*coupon.ExpiresAt) {
		return Discount{}, fmt.Errorf("%w: it expired at %s", ErrExpired,
			coupon.ExpiresAt.UTC().Format(time.RFC3339))
	}
	if len(coupon.ProductIDs) > 0 && req.ProductID != "" {
		if !contains(coupon.ProductIDs, req.ProductID) {
			return Discount{}, notApplicable("it does not apply to this product")
		}
	}
	if req.ProductID == "" && len(coupon.ProductIDs) > 0 {
		return Discount{}, notApplicable("it applies to specific products only")
	}

	cur := req.Amount.Currency()
	if !cur.IsValid() {
		return Discount{}, invalid("the amount has no currency")
	}
	if coupon.Kind == CouponFixed && !strings.EqualFold(coupon.Currency, cur.Code()) {
		return Discount{}, notApplicable("it is issued in %s, not %s", coupon.Currency, cur.Code())
	}

	uses, err := s.redemptionCount(ctx, q, coupon.ID, "")
	if err != nil {
		return Discount{}, err
	}
	if coupon.MaxUses != nil && uses >= int64(*coupon.MaxUses) {
		return Discount{}, fmt.Errorf("%w: the promotion has been fully redeemed", ErrExhausted)
	}
	if req.CustomerID != "" && coupon.MaxUsesPerUser != nil {
		perUser, err := s.redemptionCount(ctx, q, coupon.ID, req.CustomerID)
		if err != nil {
			return Discount{}, err
		}
		if perUser >= int64(*coupon.MaxUsesPerUser) {
			return Discount{}, fmt.Errorf("%w: you have already used this promotion", ErrExhausted)
		}
	}

	return applyCoupon(coupon, req)
}

// applyCoupon computes the exact discount. It is pure: no database, no clock, no rounding
// choice outside the explicit one below.
func applyCoupon(coupon Coupon, req DiscountRequest) (Discount, error) {
	cur := req.Amount.Currency()
	price := req.Amount
	setup := req.Setup
	if !setup.IsValid() {
		setup = money.Zero(cur)
	}
	if !setup.Currency().Equal(cur) {
		return Discount{}, invalid("the setup fee is in %s but the price is in %s",
			setup.Currency(), cur)
	}
	if price.IsNegative() || setup.IsNegative() {
		return Discount{}, invalid("a discount cannot be computed on a negative amount")
	}

	includePrice := coupon.AppliesTo == AppliesToPrice || coupon.AppliesTo == AppliesToAll
	includeSetup := coupon.AppliesTo == AppliesToSetup || coupon.AppliesTo == AppliesToAll

	out := Discount{
		CouponID:        coupon.ID,
		Code:            coupon.Code,
		Price:           money.Zero(cur),
		Setup:           money.Zero(cur),
		Total:           money.Zero(cur),
		Currency:        cur.Code(),
		AppliesTo:       coupon.AppliesTo,
		RecurringCycles: coupon.RecurringCycles,
	}

	switch coupon.Kind {
	case CouponPercentage:
		// The stored percentage is a bare number ("12.5"); ParseRatio with a trailing
		// percent sign turns it into an exact 1/8 without a float.
		ratio, err := money.ParseRatio(strings.TrimSpace(coupon.Percentage) + "%")
		if err != nil {
			return Discount{}, fmt.Errorf("commerce: coupon %s has an unreadable percentage: %w",
				coupon.Code, err)
		}
		if includePrice {
			if out.Price, err = money.ApplyRate(price, ratio, money.RoundHalfUp); err != nil {
				return Discount{}, fmt.Errorf("commerce: apply percentage to price: %w", err)
			}
		}
		if includeSetup {
			if out.Setup, err = money.ApplyRate(setup, ratio, money.RoundHalfUp); err != nil {
				return Discount{}, fmt.Errorf("commerce: apply percentage to setup: %w", err)
			}
		}
	case CouponFixed:
		var err error
		if coupon.AmountMinor == nil {
			return Discount{}, fmt.Errorf("commerce: coupon %s has no amount", coupon.Code)
		}
		remaining := money.New(*coupon.AmountMinor, cur)
		// The price is reduced first: a "10 off" promotion is understood by a customer as
		// "10 off the product", and taking it from the setup fee first would surprise them
		// on the first invoice.
		//
		// `take` is the smaller of the remaining discount and the component, so the
		// subtraction cannot go negative; the error is still checked because a discarded
		// overflow error is exactly the kind of silent corruption this package exists to
		// avoid.
		if includePrice && remaining.IsPositive() {
			take := minAmount(remaining, price)
			out.Price = take
			if remaining, err = remaining.Sub(take); err != nil {
				return Discount{}, fmt.Errorf("commerce: fixed coupon price remainder: %w", err)
			}
		}
		if includeSetup && remaining.IsPositive() {
			take := minAmount(remaining, setup)
			out.Setup = take
			if _, err = remaining.Sub(take); err != nil {
				return Discount{}, fmt.Errorf("commerce: fixed coupon setup remainder: %w", err)
			}
		}
	default:
		return Discount{}, fmt.Errorf("commerce: coupon %s has an unknown kind %q", coupon.Code, coupon.Kind)
	}

	total, err := out.Price.Add(out.Setup)
	if err != nil {
		return Discount{}, err
	}
	out.Total = total
	// A live coupon that reduces nothing is not an error: a fixed coupon scoped to a setup fee
	// the product does not charge, or a 0-value base. The caller shows "0.00 off" rather than
	// claiming the code is invalid, because the code *is* valid.
	return out, nil
}

// minAmount returns the smaller of two amounts in the same currency.
func minAmount(a, b money.Amount) money.Amount {
	if cmp, err := a.Cmp(b); err == nil && cmp > 0 {
		return b
	}
	return a
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// ------------------------------------------------------------- redemption --

// RedemptionRequest records one use of a coupon.
type RedemptionRequest struct {
	CouponID string
	// CustomerID is required: "who used it" is what `max_uses_per_user` counts.
	CustomerID string
	// InvoiceID and SubscriptionID are optional links. When InvoiceID is set, a replay of
	// the same checkout is a no-op rather than a second redemption.
	InvoiceID      *string
	SubscriptionID *string
	DiscountMinor  int64
	Currency       string
	// At is the instant the redemption is judged against; zero means the store's clock.
	At time.Time
}

// Redemption is a recorded use.
type Redemption struct {
	ID             string    `json:"id"`
	CouponID       string    `json:"coupon_id"`
	CustomerID     string    `json:"customer_id"`
	InvoiceID      *string   `json:"invoice_id,omitempty"`
	SubscriptionID *string   `json:"subscription_id,omitempty"`
	DiscountMinor  int64     `json:"discount_minor"`
	Currency       string    `json:"currency"`
	CreatedAt      time.Time `json:"created_at"`
}

// Redeem records a use of a coupon, enforcing its limits under a row lock.
//
// This is the authoritative check. The sequence is deliberate:
//
//  1. `SELECT ... FOR UPDATE` on the coupon row. Every concurrent redemption of this coupon
//     now serialises here, so the count read in step 2 cannot be stale.
//  2. Count the redemptions — globally for `max_uses`, per customer for
//     `max_uses_per_user` — inside that lock.
//  3. Insert. The unique `(coupon_id, invoice_id)` index turns a replay of the same
//     checkout into "already redeemed", which returns the existing row instead of failing.
//
// A coupon whose window has closed is refused here as well as in Validate: a cart can sit
// open for a day, and the checkout is where "expired" must be true.
func (s *Store) Redeem(ctx context.Context, tx pgx.Tx, req RedemptionRequest) (Redemption, error) {
	if tx == nil {
		return Redemption{}, invalid("a transaction is required to redeem a coupon")
	}
	if strings.TrimSpace(req.CustomerID) == "" {
		return Redemption{}, invalid("a redemption needs a customer")
	}
	if req.DiscountMinor < 0 {
		return Redemption{}, invalid("a discount cannot be negative")
	}
	if _, err := money.NewCurrency(req.Currency, currencyScale(req.Currency)); err != nil {
		return Redemption{}, invalid("currency %q is not valid", req.Currency)
	}

	coupon, err := s.lockCoupon(ctx, tx, req.CouponID)
	if err != nil {
		return Redemption{}, err
	}
	at := req.At
	if at.IsZero() {
		at = s.now()
	}
	at = at.UTC()
	if !coupon.Enabled {
		return Redemption{}, ErrNotFound
	}
	if coupon.StartsAt != nil && at.Before(*coupon.StartsAt) {
		return Redemption{}, fmt.Errorf("%w: it has not started yet", ErrExpired)
	}
	if coupon.ExpiresAt != nil && !at.Before(*coupon.ExpiresAt) {
		return Redemption{}, fmt.Errorf("%w: it expired at %s", ErrExpired,
			coupon.ExpiresAt.UTC().Format(time.RFC3339))
	}
	uses, err := s.redemptionCount(ctx, tx, coupon.ID, "")
	if err != nil {
		return Redemption{}, err
	}
	if coupon.MaxUses != nil && uses >= int64(*coupon.MaxUses) {
		return Redemption{}, fmt.Errorf("%w: the promotion has been fully redeemed", ErrExhausted)
	}
	if coupon.MaxUsesPerUser != nil {
		perUser, err := s.redemptionCount(ctx, tx, coupon.ID, req.CustomerID)
		if err != nil {
			return Redemption{}, err
		}
		if perUser >= int64(*coupon.MaxUsesPerUser) {
			return Redemption{}, fmt.Errorf("%w: this customer has used it %d time(s)", ErrExhausted, perUser)
		}
	}

	var out Redemption
	err = tx.QueryRow(ctx, `
		INSERT INTO coupon_redemptions (coupon_id, customer_id, invoice_id, subscription_id,
			discount_minor, currency)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING id, coupon_id, customer_id, invoice_id, subscription_id, discount_minor,
		          currency, created_at`,
		coupon.ID, req.CustomerID, req.InvoiceID, req.SubscriptionID,
		req.DiscountMinor, req.Currency).
		Scan(&out.ID, &out.CouponID, &out.CustomerID, &out.InvoiceID, &out.SubscriptionID,
			&out.DiscountMinor, &out.Currency, &out.CreatedAt)
	if err != nil {
		if isUniqueViolation(err) && req.InvoiceID != nil {
			// A replay of the same checkout: the use was already recorded against this
			// invoice, so return it rather than double-counting.
			return s.redemptionForInvoice(ctx, tx, coupon.ID, *req.InvoiceID)
		}
		return Redemption{}, fmt.Errorf("commerce: record redemption: %w", err)
	}
	return out, nil
}

// lockCoupon reads a coupon FOR UPDATE so concurrent redemptions serialise.
func (s *Store) lockCoupon(ctx context.Context, q querier, id string) (Coupon, error) {
	c, err := scanCoupon(q.QueryRow(ctx,
		`SELECT `+couponColumns+` FROM coupons WHERE id = $1 FOR UPDATE`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Coupon{}, ErrNotFound
	}
	if err != nil {
		return Coupon{}, fmt.Errorf("commerce: lock coupon: %w", err)
	}
	return c, nil
}

// redemptionCount counts uses, globally or for one customer.
func (s *Store) redemptionCount(ctx context.Context, q querier, couponID, customerID string) (int64, error) {
	var n int64
	var err error
	if customerID == "" {
		err = q.QueryRow(ctx,
			`SELECT count(*) FROM coupon_redemptions WHERE coupon_id = $1`, couponID).Scan(&n)
	} else {
		err = q.QueryRow(ctx, `
			SELECT count(*) FROM coupon_redemptions
			 WHERE coupon_id = $1 AND customer_id = $2`, couponID, customerID).Scan(&n)
	}
	if err != nil {
		return 0, fmt.Errorf("commerce: count redemptions: %w", err)
	}
	return n, nil
}

func (s *Store) redemptionForInvoice(ctx context.Context, q querier, couponID, invoiceID string) (Redemption, error) {
	var out Redemption
	err := q.QueryRow(ctx, `
		SELECT id, coupon_id, customer_id, invoice_id, subscription_id, discount_minor,
		       currency, created_at
		  FROM coupon_redemptions
		 WHERE coupon_id = $1 AND invoice_id = $2`, couponID, invoiceID).
		Scan(&out.ID, &out.CouponID, &out.CustomerID, &out.InvoiceID, &out.SubscriptionID,
			&out.DiscountMinor, &out.Currency, &out.CreatedAt)
	if err != nil {
		return Redemption{}, fmt.Errorf("commerce: read redemption: %w", err)
	}
	return out, nil
}

// Redemptions lists the recorded uses of a coupon, newest first. It is the relation
// manager's read: "who took this promotion".
func (s *Store) Redemptions(ctx context.Context, couponID string) ([]Redemption, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, coupon_id, customer_id, invoice_id, subscription_id, discount_minor,
		       currency, created_at
		  FROM coupon_redemptions
		 WHERE coupon_id = $1
		 ORDER BY created_at DESC`, couponID)
	if err != nil {
		return nil, fmt.Errorf("commerce: list redemptions: %w", err)
	}
	defer rows.Close()
	out := make([]Redemption, 0, 16)
	for rows.Next() {
		var r Redemption
		if err := rows.Scan(&r.ID, &r.CouponID, &r.CustomerID, &r.InvoiceID,
			&r.SubscriptionID, &r.DiscountMinor, &r.Currency, &r.CreatedAt); err != nil {
			return nil, fmt.Errorf("commerce: scan redemption: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
