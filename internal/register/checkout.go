// Checkout: the customer purchase path.
//
// This is the piece the storefront was missing. Before it, an order could only be
// created by staff (`POST /api/v1/staff/orders`), so the "Buy" button was a login
// link and a visitor who registered had nothing to buy. The handler in
// internal/httpapi/checkout_customer.go is thin on purpose; everything that can be
// wrong about money is here, where a test can prove it without an HTTP round trip.
//
// # What is reused, and what is mirrored
//
// The pricing engine is `billing.Issuer`: it takes catalog lines and returns exact
// net, tax and total from the configured `billing.Pricer`, allocates the invoice
// number, freezes the snapshot and posts the ledger. Nothing here computes a tax
// rate, rounds a total or allocates a number from scratch — re-deriving any of those
// would be a second money path, which is exactly how a ledger stops balancing.
//
// `orders.Service.Create` does the same thing for exactly one price version. A cart
// is N lines, so this file writes the order row, its items and one subscription per
// line itself and then hands the whole line set to `Issuer.CreateDraft`. Those row
// shapes are mirrored from internal/orders/orders.go, deliberately and narrowly: the
// columns, the 'pending' subscription status and the "order total is the invoice
// total" rule are the same, because a customer order and a staff order must be
// indistinguishable to fulfilment, dunning and the operator screens.
//
// # The rules that are load-bearing
//
//   - **customer_id comes from the session, never from the body.** The endpoint
//     cannot be told whose order to create, so identifier substitution is not a
//     reachable attack.
//   - **The price is read from the catalog under a row lock.** A line names a price
//     version; it never carries an amount, so a client cannot choose what to pay.
//   - **Stock and per-user limits are enforced under a lock on the product row.** The
//     lock is taken in a deterministic order (sorted product ids) so two carts that
//     share two products cannot deadlock, and the decrement is a conditional UPDATE
//     so the last unit cannot be sold twice even if the lock were bypassed.
//   - **A coupon is a line on the invoice, not a lower unit price.** The charge lines
//     keep the catalog net so the document explains itself; the discount is a
//     `line_kind='discount'` line with a negative net. Its tax is zero and each charge
//     line's tax is re-derived from the discounted net, because carrying negative tax
//     on the discount line *and* re-pricing the charges would count the reduction
//     twice and the invoice would under-collect.
//   - **A zero total skips the invoice.** Nothing is owed, so there is nothing to
//     issue; the subscriptions are activated and the services are created and queued
//     for provisioning in the same transaction, which is what a settled payment would
//     have produced.
package register

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/catalyst/billing/internal/audit"
	"github.com/catalyst/billing/internal/billing"
	"github.com/catalyst/billing/internal/calendar"
	"github.com/catalyst/billing/internal/credit"
	"github.com/catalyst/billing/internal/money"
	"github.com/catalyst/billing/internal/notify"
	"github.com/catalyst/billing/internal/store"
	"github.com/jackc/pgx/v5"
)

// Checkout errors. Each one maps to a distinct client-visible code, because a
// customer who is told "that did not work" cannot act on it.
var (
	// ErrNoLines is returned for an empty cart.
	ErrNoLines = errors.New("checkout: at least one line is required")
	// ErrTooManyLines bounds one order.
	ErrTooManyLines = errors.New("checkout: at most 50 lines are allowed")
	// ErrNoCustomer is returned when the signed-in account has no billing account.
	ErrNoCustomer = errors.New("checkout: this account has no billing account")
	// ErrPriceNotFound is returned when a price version does not exist.
	ErrPriceNotFound = errors.New("checkout: no such price")
	// ErrInvalidQuantity is returned for a quantity outside 1..1000.
	ErrInvalidQuantity = errors.New("checkout: quantity must be between 1 and 1000")
	// ErrQuantityNotAllowed is returned when the product sells one per order.
	ErrQuantityNotAllowed = errors.New("checkout: this product cannot be bought in a quantity")
	// ErrMixedCurrency is returned when lines do not share one currency.
	ErrMixedCurrency = errors.New("checkout: every line of an order must use the same currency")
	// ErrOutOfStock is returned when the product cannot supply the quantity.
	ErrOutOfStock = errors.New("checkout: not enough stock")
	// ErrUserLimitReached is returned when the per-user limit would be exceeded.
	ErrUserLimitReached = errors.New("checkout: you already hold the maximum of this product")
	// ErrInvalidSchedule is returned when a price cannot be turned into a period.
	ErrInvalidSchedule = errors.New("checkout: this price has no valid billing schedule")
	// ErrTOSRequired is returned when terms are configured and were not accepted.
	ErrTOSRequired = errors.New("checkout: the terms of service must be accepted")
	// ErrEmailVerificationRequired is returned when the installation requires a
	// verified address before a purchase.
	ErrEmailVerificationRequired = errors.New("checkout: confirm your email address before buying")
	// ErrInvalidConfig is returned for a config selection that is not offered.
	ErrInvalidConfig = errors.New("checkout: the selected options are not available for this price")

	// Coupon failures. They are separate values because the UI shows a different
	// sentence for each, and because a support agent needs to know which one it was.
	ErrCouponNotFound    = errors.New("checkout: no such coupon")
	ErrCouponDisabled    = errors.New("checkout: this coupon is no longer available")
	ErrCouponNotStarted  = errors.New("checkout: this coupon is not active yet")
	ErrCouponExpired     = errors.New("checkout: this coupon has expired")
	ErrCouponExhausted   = errors.New("checkout: this coupon has been fully redeemed")
	ErrCouponUsed        = errors.New("checkout: you have already used this coupon")
	ErrCouponNotForYou   = errors.New("checkout: this coupon does not apply to these products")
	ErrCouponWrongMoney  = errors.New("checkout: this coupon is issued in a different currency")
	ErrCouponNoDiscount  = errors.New("checkout: this coupon gives no discount on this order")
	ErrCouponNotEligible = errors.New("checkout: this coupon does not apply to what you are buying")
)

// maxLines bounds one order. A cart is a handful of products; a hundred-line order
// is a script, and the invoice document has to stay printable.
const maxLines = 50

// maxQuantity matches the order_items and cart_items check.
const maxQuantity = 1000

// CheckoutLine is one requested purchase.
type CheckoutLine struct {
	// PriceVersionID names what is bought. The price is read from the catalog.
	PriceVersionID string
	// Quantity defaults to 1.
	Quantity int
	// Config is the chosen config options as {config_option_id: value}. It is
	// re-validated against the product's options and priced from the catalog; an
	// unknown or unavailable choice is refused rather than ignored.
	Config map[string]string
}

// CheckoutRequest is one purchase.
type CheckoutRequest struct {
	// UserID is the session's user. The billing account is derived from it.
	UserID string
	// CustomerID is derived by ResolveCustomer; a caller may supply it only when it
	// came from ResolveCustomer, never from a request body.
	CustomerID string
	Lines      []CheckoutLine
	// CouponCode is optional and matched case-insensitively.
	CouponCode string
	// AcceptedTOS is the checkout checkbox.
	AcceptedTOS bool

	// Audit attribution.
	CorrelationID string
	IP            string
}

// CheckoutLineResult is one priced line of the response.
type CheckoutLineResult struct {
	Description string
	Quantity    int
	Unit        money.Amount
	Net         money.Amount
	Tax         money.Amount
	Total       money.Amount
}

// CheckoutResult is what was created.
type CheckoutResult struct {
	OrderID string
	// CustomerID is the billing account the order belongs to. It is echoed so the
	// caller can scope the live event without a second query.
	CustomerID string
	// Status is "pending_payment" when an invoice was issued, or "paid" when the
	// order was free.
	Status   string
	Currency string
	Subtotal money.Amount
	Tax      money.Amount
	Discount money.Amount
	Total    money.Amount
	// InvoiceID and InvoiceNumber are empty for a zero-total order: there is no
	// document, because there is nothing to pay.
	InvoiceID     string
	InvoiceNumber string
	// SubscriptionIDs has one entry per purchased line.
	SubscriptionIDs []string
	// ServiceIDs is populated only on the zero-total path, where the service is
	// created immediately instead of waiting for a payment.
	ServiceIDs []string
	// Activated reports that the zero-total path ran.
	Activated bool
	// CouponCode echoes the coupon that was applied, empty when none was.
	CouponCode string
	Lines      []CheckoutLineResult
}

// CheckoutService turns a cart into an order.
type CheckoutService struct {
	pool   *store.Pool
	issuer *billing.Issuer
	queue  *notify.Queue
	// merchant, terms and series are the document defaults, supplied by the caller so
	// this package has no opinion about configuration.
	merchant billing.Merchant
	terms    string
	series   billing.NumberSeries
	now      func() time.Time
}

// NewCheckoutService constructs a checkout service.
func NewCheckoutService(pool *store.Pool, issuer *billing.Issuer, merchant billing.Merchant,
	terms string, series billing.NumberSeries) *CheckoutService {
	if series.Scope == "" {
		series = billing.DefaultSeries
	}
	return &CheckoutService{
		pool:     pool,
		issuer:   issuer,
		merchant: merchant,
		terms:    terms,
		series:   series,
		now:      func() time.Time { return time.Now().UTC() },
	}
}

// WithNotify attaches the outbox, so the issued invoice produces the same
// "your invoice is ready" message the staff path sends.
func (s *CheckoutService) WithNotify(q *notify.Queue) *CheckoutService {
	s.queue = q
	return s
}

// WithClock overrides the clock. Tests use it to pin the billing period.
func (s *CheckoutService) WithClock(now func() time.Time) *CheckoutService {
	if now != nil {
		s.now = now
	}
	return s
}

// ResolveCustomer returns the billing account for a signed-in user.
//
// The link in `customer_accounts` is the authoritative answer, because registration
// writes it in the same transaction as the user. The staff fallback exists for the
// same reason it does in internal/portal: an operator is already authenticated as
// themselves, and requiring a second identity row before they can be billed asks
// them to link an account to an account. A *customer* never gets the email fallback,
// because an email match is not proof of ownership.
func (s *CheckoutService) ResolveCustomer(ctx context.Context, userID string) (string, error) {
	if userID == "" {
		return "", ErrNoCustomer
	}
	var id string
	err := s.pool.QueryRow(ctx, `
		SELECT customer_id FROM customer_accounts WHERE user_id = $1
		ORDER BY created_at LIMIT 1`, userID).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("checkout: resolve customer: %w", err)
	}
	err = s.pool.QueryRow(ctx, `
		SELECT c.id
		FROM users u
		JOIN customers c ON lower(c.email) = lower(u.email)
		WHERE u.id = $1
		  AND EXISTS (SELECT 1 FROM staff_roles sr WHERE sr.user_id = u.id)
		ORDER BY c.created_at
		LIMIT 1`, userID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNoCustomer
	}
	if err != nil {
		return "", fmt.Errorf("checkout: resolve customer: %w", err)
	}
	return id, nil
}

// Checkout prices the request from the catalog and writes the order.
func (s *CheckoutService) Checkout(ctx context.Context, req CheckoutRequest) (CheckoutResult, error) {
	if s == nil || s.issuer == nil || s.pool == nil {
		return CheckoutResult{}, fmt.Errorf("checkout: ordering is not configured")
	}
	if len(req.Lines) == 0 {
		return CheckoutResult{}, ErrNoLines
	}
	if len(req.Lines) > maxLines {
		return CheckoutResult{}, ErrTooManyLines
	}
	customerID := req.CustomerID
	if customerID == "" {
		var err error
		if customerID, err = s.ResolveCustomer(ctx, req.UserID); err != nil {
			return CheckoutResult{}, err
		}
	}

	// The two settings that gate a purchase are read before the transaction: a refusal
	// should not take a lock, and the answers cannot change mid-request in a way that
	// matters.
	policy, err := ReadPolicy(ctx, s.pool)
	if err != nil {
		return CheckoutResult{}, err
	}
	if policy.TOSRequired && !req.AcceptedTOS {
		return CheckoutResult{}, ErrTOSRequired
	}
	if policy.MustVerifyEmail {
		verified, err := emailVerified(ctx, s.pool, req.UserID)
		if err != nil {
			return CheckoutResult{}, err
		}
		if !verified {
			return CheckoutResult{}, ErrEmailVerificationRequired
		}
	}

	now := s.now()
	var out CheckoutResult
	err = s.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		res, err := s.checkoutTx(ctx, tx, req, customerID, now)
		if err != nil {
			return err
		}
		out = res
		return audit.Record(ctx, tx, audit.Event{
			Actor:         audit.Actor{ID: req.UserID, Kind: audit.ActorUser},
			Action:        "order.create",
			TargetType:    "order",
			TargetID:      res.OrderID,
			Outcome:       audit.OutcomeSuccess,
			CorrelationID: req.CorrelationID,
			IP:            req.IP,
			Changes: map[string]any{
				"customer_id":      customerID,
				"lines":            len(req.Lines),
				"coupon_code":      res.CouponCode,
				"currency":         res.Currency,
				"subtotal_minor":   res.Subtotal.Minor(),
				"discount_minor":   res.Discount.Minor(),
				"tax_minor":        res.Tax.Minor(),
				"total_minor":      res.Total.Minor(),
				"invoice_id":       res.InvoiceID,
				"invoice_number":   res.InvoiceNumber,
				"subscription_ids": res.SubscriptionIDs,
				"activated":        res.Activated,
			},
		})
	})
	if err != nil {
		return CheckoutResult{}, err
	}
	return out, nil
}

// emailVerified reports whether an account has confirmed its address.
func emailVerified(ctx context.Context, q interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}, userID string) (bool, error) {
	var at *time.Time
	if err := q.QueryRow(ctx,
		`SELECT email_verified_at FROM users WHERE id = $1`, userID).Scan(&at); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, ErrNoCustomer
		}
		return false, fmt.Errorf("checkout: read verification state: %w", err)
	}
	return at != nil, nil
}

// catalogLine is one price version plus the product rules that constrain it.
type catalogLine struct {
	priceVersionID string
	productID      string
	productName    string
	priceName      string
	currency       string
	amount         int64
	setup          int64
	interval       string
	anchorDay      int
	timezone       string

	stock        *int
	perUserLimit *int
	allowQty     string
}

// description is the line text on the order and the invoice, matching the staff path.
func (c catalogLine) description() string {
	if c.priceName != "" {
		return c.productName + " — " + c.priceName
	}
	return c.productName
}

// configSelection is one validated option selection and its catalog price.
type configSelection struct {
	optionID string
	name     string
	value    string
	amount   int64
	setup    int64
}

// charge is one invoice line the order asks for, before pricing.
type charge struct {
	line    billing.Line
	kind    string // "price" or "setup", for the coupon's applies_to rule
	product string
}

// checkoutTx is the whole purchase, inside the caller's transaction.
func (s *CheckoutService) checkoutTx(ctx context.Context, tx pgx.Tx, req CheckoutRequest,
	customerID string, now time.Time) (CheckoutResult, error) {

	// Load every line's catalog row first, without locking: a malformed id or a
	// mixed-currency cart must be refused before any lock is taken.
	lines := make([]catalogLine, 0, len(req.Lines))
	selections := make([][]configSelection, 0, len(req.Lines))
	quantities := make([]int, 0, len(req.Lines))
	for _, l := range req.Lines {
		if !validID(l.PriceVersionID) {
			return CheckoutResult{}, ErrPriceNotFound
		}
		qty := l.Quantity
		if qty == 0 {
			qty = 1
		}
		if qty < 1 || qty > maxQuantity {
			return CheckoutResult{}, ErrInvalidQuantity
		}
		cl, err := loadCatalogLine(ctx, tx, l.PriceVersionID)
		if err != nil {
			return CheckoutResult{}, err
		}
		if cl.allowQty == "disabled" && qty != 1 {
			return CheckoutResult{}, ErrQuantityNotAllowed
		}
		sel, err := loadConfig(ctx, tx, cl, l.Config)
		if err != nil {
			return CheckoutResult{}, err
		}
		lines = append(lines, cl)
		selections = append(selections, sel)
		quantities = append(quantities, qty)
	}

	currency := lines[0].currency
	for _, cl := range lines {
		if cl.currency != currency {
			return CheckoutResult{}, ErrMixedCurrency
		}
	}
	cur, err := currencyOf(currency)
	if err != nil {
		return CheckoutResult{}, err
	}

	// Lock the products in a deterministic order. Two carts that share two products
	// and lock them in opposite orders deadlock; sorting makes that impossible.
	wanted := make(map[string]int)
	for i, cl := range lines {
		wanted[cl.productID] += quantities[i]
	}
	productIDs := make([]string, 0, len(wanted))
	for id := range wanted {
		productIDs = append(productIDs, id)
	}
	sort.Strings(productIDs)
	for _, id := range productIDs {
		var stock *int
		if err := tx.QueryRow(ctx,
			`SELECT stock FROM products WHERE id = $1 FOR UPDATE`, id).Scan(&stock); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return CheckoutResult{}, ErrPriceNotFound
			}
			return CheckoutResult{}, fmt.Errorf("checkout: lock product: %w", err)
		}
		if stock != nil && *stock < wanted[id] {
			return CheckoutResult{}, ErrOutOfStock
		}
	}

	// Per-user limits are counted against what the customer already holds. The count
	// is inside the same transaction as the insert, and the product lock above
	// serialises concurrent checkouts of the same product, so two carts cannot both
	// see "one held" and both add a second.
	for _, cl := range lines {
		if cl.perUserLimit == nil {
			continue
		}
		// "Held" is a service the customer has *or* a subscription that will produce
		// one. Counting only services would let a customer place ten unpaid orders for
		// the same product and take delivery of ten services the moment they paid, which
		// is exactly what the limit exists to prevent.
		var held int
		if err := tx.QueryRow(ctx, `
			SELECT
			  (SELECT count(*) FROM subscriptions s
			    JOIN price_versions pv ON pv.id = s.price_version_id
			    WHERE s.customer_id = $1 AND pv.product_id = $2
			      AND s.status IN ('pending', 'active', 'past_due'))
			+ (SELECT count(*) FROM services sv
			    WHERE sv.customer_id = $1 AND sv.product_id = $2
			      AND sv.subscription_id IS NULL AND sv.status <> 'terminated')`,
			customerID, cl.productID).Scan(&held); err != nil {
			return CheckoutResult{}, fmt.Errorf("checkout: count held services: %w", err)
		}
		if held+wanted[cl.productID] > *cl.perUserLimit {
			return CheckoutResult{}, ErrUserLimitReached
		}
	}

	// Decrement stock. The conditional UPDATE is defence in depth: the row is locked
	// above, so zero rows affected means the lock was not held and refusing is the
	// only safe answer.
	for _, id := range productIDs {
		var hasStock bool
		if err := tx.QueryRow(ctx,
			`SELECT stock IS NOT NULL FROM products WHERE id = $1`, id).Scan(&hasStock); err != nil {
			return CheckoutResult{}, fmt.Errorf("checkout: read stock flag: %w", err)
		}
		if !hasStock {
			continue
		}
		tag, err := tx.Exec(ctx, `
			UPDATE products SET stock = stock - $2
			WHERE id = $1 AND stock IS NOT NULL AND stock >= $2`, id, wanted[id])
		if err != nil {
			return CheckoutResult{}, fmt.Errorf("checkout: decrement stock: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return CheckoutResult{}, ErrOutOfStock
		}
	}

	// Build the invoice lines. One charge line per cart line, plus a setup line when
	// the price has a setup fee — the same split the staff path produces, so a
	// customer invoice and a staff invoice of the same product look the same.
	charges := make([]charge, 0, len(lines)*2)
	descriptions := make([]string, 0, len(lines))
	for i, cl := range lines {
		desc := cl.description()
		if summary := configSummary(selections[i]); summary != "" {
			desc += " (" + summary + ")"
		}
		descriptions = append(descriptions, desc)

		unit := cl.amount + configAmount(selections[i])
		charges = append(charges, charge{
			line: billing.Line{
				Description:    desc,
				QuantityMilli:  int64(quantities[i]) * 1000,
				UnitPrice:      money.New(unit, cur),
				ProductID:      ptr(cl.productID),
				PriceVersionID: ptr(cl.priceVersionID),
			},
			kind:    "price",
			product: cl.productID,
		})
		setup := cl.setup + configSetup(selections[i])
		if setup > 0 {
			charges = append(charges, charge{
				line: billing.Line{
					Description:    desc + " — setup",
					QuantityMilli:  int64(quantities[i]) * 1000,
					UnitPrice:      money.New(setup, cur),
					ProductID:      ptr(cl.productID),
					PriceVersionID: ptr(cl.priceVersionID),
				},
				kind:    "setup",
				product: cl.productID,
			})
		}
	}

	// Resolve the coupon before writing anything: an invalid code must not leave a
	// half-built order behind, and its discount decides whether there is an invoice at
	// all.
	plan, err := s.resolveCoupon(ctx, tx, req.CouponCode, customerID, currency, charges, cur)
	if err != nil {
		return CheckoutResult{}, err
	}

	// Would anything be owed? The quoted value is what the customer pays before tax;
	// a discount that takes it to zero means there is nothing to invoice. Tax on a
	// zero net is zero, so this decision does not need the pricer.
	quoted := int64(0)
	for _, c := range charges {
		quoted += c.line.UnitPrice.Minor() * (c.line.QuantityMilli / 1000)
	}
	payable := quoted - plan.amount

	// The order row. total_minor is provisional until the invoice prices the lines;
	// for a zero-total order it stays zero, which is the truth.
	var orderID string
	if err := tx.QueryRow(ctx, `
		INSERT INTO orders (customer_id, status, currency, total_minor)
		VALUES ($1, 'pending_payment', $2, 0)
		RETURNING id`, customerID, currency).Scan(&orderID); err != nil {
		return CheckoutResult{}, fmt.Errorf("checkout: insert order: %w", err)
	}

	// One order item per cart line. Its id is the anchor fulfilment uses to enqueue a
	// provisioning operation, so it must exist before anything is provisioned.
	orderItemIDs := make([]string, 0, len(lines))
	for i, cl := range lines {
		var itemID string
		unit := cl.amount + configAmount(selections[i])
		setup := (cl.setup + configSetup(selections[i])) * int64(quantities[i])
		if err := tx.QueryRow(ctx, `
			INSERT INTO order_items (order_id, price_version_id, description, quantity,
				unit_minor, line_minor, setup_minor, position)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
			RETURNING id`,
			orderID, cl.priceVersionID, descriptions[i], quantities[i], unit,
			unit*int64(quantities[i]), setup, i+1).Scan(&itemID); err != nil {
			return CheckoutResult{}, fmt.Errorf("checkout: insert order item: %w", err)
		}
		orderItemIDs = append(orderItemIDs, itemID)
	}

	// One subscription per *recurring* line, starting 'pending' so an unpaid order can never
	// renew. The payment applier activates it; the zero-total path activates it here. A one-time
	// or free line gets no subscription at all — see plan_interval.go for why the absence, rather
	// than a placeholder row, is what stops it being renewed.
	subscriptionIDs := make([]string, len(lines))
	for i, cl := range lines {
		start, end, recurring, err := planSchedule(cl, now)
		if err != nil {
			return CheckoutResult{}, err
		}
		if !recurring {
			continue
		}
		var subID string
		if err := tx.QueryRow(ctx, `
			INSERT INTO subscriptions (customer_id, price_version_id, status, interval,
				anchor_day, timezone, current_period_start, current_period_end, next_billing_at,
				coupon_id, coupon_cycles_applied)
			VALUES ($1, $2, 'pending', $3, $4, $5, $6, $7, $7, $8, $9)
			RETURNING id`,
			customerID, cl.priceVersionID, cl.interval, cl.anchorDay, cl.timezone,
			start, end, nullableString(plan.couponID), plan.cyclesApplied).Scan(&subID); err != nil {
			return CheckoutResult{}, fmt.Errorf("checkout: insert subscription: %w", err)
		}
		subscriptionIDs[i] = subID
	}
	// The first subscription that exists, for the invoice and the coupon redemption. An order of
	// one-time lines has none, and both columns are nullable for exactly that case.
	firstSubscriptionID := firstID(subscriptionIDs)

	out := CheckoutResult{
		OrderID:         orderID,
		CustomerID:      customerID,
		Currency:        currency,
		SubscriptionIDs: compactIDs(subscriptionIDs),
		CouponCode:      plan.code,
		Discount:        money.New(plan.amount, cur),
	}

	// A zero total has nothing to pay, so no document is issued: CT-08. The
	// subscriptions are activated and the services are created and queued now, which
	// is exactly what a settled payment would have done.
	if payable == 0 {
		serviceIDs, err := s.activateZeroTotal(ctx, tx, customerID, lines, selections,
			orderItemIDs, subscriptionIDs)
		if err != nil {
			return CheckoutResult{}, err
		}
		if plan.amount > 0 {
			// The redemption is recorded even though there is no invoice: `max_uses`
			// counts uses, and a free order is still a use. The invoice id is null,
			// which the unique index tolerates.
			if err := insertRedemption(ctx, tx, plan, customerID, "", firstSubscriptionID, currency); err != nil {
				return CheckoutResult{}, err
			}
		}
		if _, err := tx.Exec(ctx, `
			UPDATE orders SET status = 'paid', total_minor = 0, updated_at = now(),
			                  version = version + 1
			WHERE id = $1`, orderID); err != nil {
			return CheckoutResult{}, fmt.Errorf("checkout: mark the free order paid: %w", err)
		}
		out.Status = "paid"
		out.Activated = true
		out.ServiceIDs = serviceIDs
		out.Subtotal = money.New(0, cur)
		out.Tax = money.New(0, cur)
		out.Total = money.New(0, cur)
		out.Lines = requestedLines(charges, cur)
		return out, nil
	}

	// Price the lines. Issuer.CreateDraft is the single pricing path: it applies the
	// configured pricer, writes the draft invoice, its lines and the snapshot.
	chargeLines := make([]billing.Line, 0, len(charges))
	for _, c := range charges {
		chargeLines = append(chargeLines, c.line)
	}
	customer := billing.CustomerDetails{ID: customerID}
	if err := billing.FillCustomer(ctx, tx, &customer); err != nil {
		if errors.Is(err, billing.ErrInvalidProfile) {
			return CheckoutResult{}, ErrNoCustomer
		}
		return CheckoutResult{}, err
	}

	invoiceID, _, snap, err := s.issuer.CreateDraft(ctx, tx, billing.IssueRequest{
		CustomerID:      customerID,
		OrderID:         orderID,
		SubscriptionID:  firstSubscriptionID,
		PeriodKey:       invoicePeriodKey(firstSubscriptionID, now),
		Lines:           chargeLines,
		Merchant:        s.merchant,
		Customer:        customer,
		Terms:           s.terms,
		DueAt:           now.AddDate(0, 0, 30),
		Series:          s.series,
		PriceVersionIDs: priceVersionIDs(lines),
	})
	if err != nil {
		return CheckoutResult{}, err
	}

	// The discount is applied to the draft, where the document is still editable.
	if plan.amount > 0 {
		adjusted, err := applyDiscount(ctx, tx, invoiceID, snap, plan, charges, cur)
		if err != nil {
			return CheckoutResult{}, err
		}
		snap = adjusted
		if err := insertRedemption(ctx, tx, plan, customerID, invoiceID, firstSubscriptionID, currency); err != nil {
			return CheckoutResult{}, err
		}
	}

	issued, err := s.issuer.Issue(ctx, tx, invoiceID, s.series)
	if err != nil {
		return CheckoutResult{}, err
	}
	// The order total is the invoice total, tax and discount included: a customer
	// comparing the two must see the same number.
	if _, err := tx.Exec(ctx, `
		UPDATE orders SET total_minor = $2, updated_at = now(), version = version + 1
		WHERE id = $1`, orderID, issued.Totals.Total.Minor()); err != nil {
		return CheckoutResult{}, fmt.Errorf("checkout: set order total: %w", err)
	}
	if err := s.queueInvoice(ctx, tx, invoiceID, customerID, issued.Number,
		issued.Totals.Total, now.AddDate(0, 0, 30)); err != nil {
		return CheckoutResult{}, err
	}

	// Spend any credit the customer already holds, in the same transaction that issued the
	// invoice. Applying it here rather than in a later job is what makes "I had $20 of credit and
	// the invoice was $18" resolve to a paid order immediately instead of an invoice that sits
	// open until a sweep finds it.
	//
	// A failure is fatal rather than best-effort: the credit application is money, and an order
	// that committed while its credit silently did not would leave the customer charged twice.
	// A customer with no credit, or with auto-use turned off, is a no-op.
	if _, err := credit.ApplyCreditToInvoice(ctx, tx, invoiceID); err != nil {
		return CheckoutResult{}, fmt.Errorf("checkout: apply credit: %w", err)
	}

	priced, err := invoiceLines(ctx, tx, invoiceID, cur)
	if err != nil {
		return CheckoutResult{}, err
	}

	out.Status = "pending_payment"
	out.InvoiceID = issued.InvoiceID
	out.InvoiceNumber = issued.Number
	out.Subtotal = issued.Totals.Subtotal
	out.Tax = issued.Totals.Tax
	out.Total = issued.Totals.Total
	out.Lines = priced
	return out, nil
}

// invoiceLines reads back the issued document's lines.
//
// The response reports the document, not the request: a client that shows per-line tax
// must show the tax that was actually charged, and the discount line has to appear so
// the cart total reconciles with what the customer is asked to pay.
func invoiceLines(ctx context.Context, tx pgx.Tx, invoiceID string, cur money.Currency) ([]CheckoutLineResult, error) {
	rows, err := tx.Query(ctx, `
		SELECT description, quantity::text, unit_minor, net_minor, tax_minor, total_minor
		FROM invoice_lines WHERE invoice_id = $1 ORDER BY position`, invoiceID)
	if err != nil {
		return nil, fmt.Errorf("checkout: read invoice lines: %w", err)
	}
	defer rows.Close()

	out := make([]CheckoutLineResult, 0, 4)
	for rows.Next() {
		var (
			desc     string
			qty      string
			unit     int64
			net, tax int64
			total    int64
		)
		if err := rows.Scan(&desc, &qty, &unit, &net, &tax, &total); err != nil {
			return nil, fmt.Errorf("checkout: scan invoice line: %w", err)
		}
		milli := quantityMilli(qty)
		// A fractional quantity is reported in whole units for display; the money is
		// taken from the document, never recomputed from it.
		out = append(out, CheckoutLineResult{
			Description: desc,
			Quantity:    int((milli + 999) / 1000),
			Unit:        money.New(unit, cur),
			Net:         money.New(net, cur),
			Tax:         money.New(tax, cur),
			Total:       money.New(total, cur),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("checkout: read invoice lines: %w", err)
	}
	return out, nil
}

// requestedLines renders the requested lines for the response.
//
// These are the catalog amounts the request asked for, before tax and discount. The
// invoice is the authority on the priced document, and the response carries its id so
// a client that needs per-line tax reads it there.
func requestedLines(charges []charge, cur money.Currency) []CheckoutLineResult {
	out := make([]CheckoutLineResult, 0, len(charges))
	for _, c := range charges {
		qty := int(c.line.QuantityMilli / 1000)
		net, err := c.line.UnitPrice.MulInt(int64(qty))
		if err != nil {
			net = money.Zero(cur)
		}
		out = append(out, CheckoutLineResult{
			Description: c.line.Description,
			Quantity:    qty,
			Unit:        c.line.UnitPrice,
			Net:         net,
			Tax:         money.Zero(cur),
			Total:       net,
		})
	}
	return out
}

// couponPlan is a resolved coupon and the discount it grants.
type couponPlan struct {
	couponID string
	code     string
	// amount is the discount in minor units on the quoted price, > 0 when applied.
	amount int64
	// appliesTo is "price", "setup" or "all".
	appliesTo string
	// eligible marks which charge lines the discount reduces, by index.
	eligible []int
	// cyclesApplied is 1 when the coupon has a recurring window: this is the first
	// invoice it has discounted. It is 0 for a lifetime coupon.
	cyclesApplied int
}

// resolveCoupon validates a code and computes the discount under a row lock.
//
// Every refusal is a distinct error so the UI can say something true. The lock on the
// coupon row is what makes `max_uses` safe: two checkouts racing for the last
// redemption are serialised, and the count they both read is the committed one.
func (s *CheckoutService) resolveCoupon(ctx context.Context, tx pgx.Tx, code, customerID,
	currency string, charges []charge, cur money.Currency) (couponPlan, error) {

	code = strings.TrimSpace(code)
	if code == "" {
		return couponPlan{}, nil
	}

	var (
		couponID    string
		enabled     bool
		kind        string
		percentage  *string
		amountMinor *int64
		couponCur   *string
		appliesTo   string
		maxUses     *int
		maxPerUser  *int
		startsAt    *time.Time
		expiresAt   *time.Time
		recurring   *int
	)
	err := tx.QueryRow(ctx, `
		SELECT id, enabled, kind, percentage, amount_minor, currency, applies_to,
		       max_uses, max_uses_per_user, starts_at, expires_at, recurring_cycles
		FROM coupons WHERE upper(code) = upper($1)
		FOR UPDATE`, code).
		Scan(&couponID, &enabled, &kind, &percentage, &amountMinor, &couponCur, &appliesTo,
			&maxUses, &maxPerUser, &startsAt, &expiresAt, &recurring)
	if errors.Is(err, pgx.ErrNoRows) {
		return couponPlan{}, ErrCouponNotFound
	}
	if err != nil {
		return couponPlan{}, fmt.Errorf("checkout: load coupon: %w", err)
	}
	if !enabled {
		return couponPlan{}, ErrCouponDisabled
	}
	now := s.now()
	if startsAt != nil && now.Before(*startsAt) {
		return couponPlan{}, ErrCouponNotStarted
	}
	if expiresAt != nil && !now.Before(*expiresAt) {
		return couponPlan{}, ErrCouponExpired
	}

	// Global and per-customer use counts come from the redemption rows, not from a
	// mutable counter: a counter is a race, a count is a fact.
	if maxUses != nil {
		var used int
		if err := tx.QueryRow(ctx,
			`SELECT count(*) FROM coupon_redemptions WHERE coupon_id = $1`, couponID).Scan(&used); err != nil {
			return couponPlan{}, fmt.Errorf("checkout: count coupon uses: %w", err)
		}
		if used >= *maxUses {
			return couponPlan{}, ErrCouponExhausted
		}
	}
	if maxPerUser != nil {
		var used int
		if err := tx.QueryRow(ctx, `
			SELECT count(*) FROM coupon_redemptions
			WHERE coupon_id = $1 AND customer_id = $2`, couponID, customerID).Scan(&used); err != nil {
			return couponPlan{}, fmt.Errorf("checkout: count customer coupon uses: %w", err)
		}
		if used >= *maxPerUser {
			return couponPlan{}, ErrCouponUsed
		}
	}

	// Product restrictions. No rows means "every product".
	restricted := false
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM coupon_products WHERE coupon_id = $1)`, couponID).
		Scan(&restricted); err != nil {
		return couponPlan{}, fmt.Errorf("checkout: read coupon products: %w", err)
	}
	if restricted {
		for _, c := range charges {
			var ok bool
			if err := tx.QueryRow(ctx, `
				SELECT EXISTS (SELECT 1 FROM coupon_products
				               WHERE coupon_id = $1 AND product_id = $2)`,
				couponID, c.product).Scan(&ok); err != nil {
				return couponPlan{}, fmt.Errorf("checkout: check coupon product: %w", err)
			}
			if !ok {
				return couponPlan{}, ErrCouponNotForYou
			}
		}
	}

	// Which lines does it reduce?
	eligible := make([]int, 0, len(charges))
	for i, c := range charges {
		switch appliesTo {
		case "price":
			if c.kind == "price" {
				eligible = append(eligible, i)
			}
		case "setup":
			if c.kind == "setup" {
				eligible = append(eligible, i)
			}
		default: // "all"
			eligible = append(eligible, i)
		}
	}
	if len(eligible) == 0 {
		return couponPlan{}, ErrCouponNotEligible
	}

	// The discountable base is the eligible lines' quoted value, computed from the
	// same catalog amounts the invoice will use, so the discount cannot disagree with
	// the document.
	base := int64(0)
	for _, i := range eligible {
		base += charges[i].line.UnitPrice.Minor() * (charges[i].line.QuantityMilli / 1000)
	}
	if base <= 0 {
		return couponPlan{}, ErrCouponNoDiscount
	}

	var discount int64
	switch kind {
	case "percentage":
		if percentage == nil {
			return couponPlan{}, ErrCouponDisabled
		}
		// The column stores a percent ("10", "12.5"), not a fraction: the schema's CHECK
		// accepts only digits and a decimal point. Dividing by 100 here is what makes
		// "10" mean ten per cent rather than ten times the price.
		ratio, err := money.ParseRatio(*percentage)
		if err != nil {
			return couponPlan{}, fmt.Errorf("checkout: coupon percentage: %w", err)
		}
		percent, err := money.NewRatio(1, 100)
		if err != nil {
			return couponPlan{}, err
		}
		applied, err := money.ApplyRate(money.New(base, cur), ratio.Mul(percent), money.RoundHalfUp)
		if err != nil {
			return couponPlan{}, err
		}
		discount = applied.Minor()
	case "fixed":
		if amountMinor == nil {
			return couponPlan{}, ErrCouponDisabled
		}
		if couponCur == nil || !strings.EqualFold(*couponCur, currency) {
			// A fixed coupon is an amount of money in one currency. Applying it to an
			// invoice in another would need an exchange rate this product does not have.
			return couponPlan{}, ErrCouponWrongMoney
		}
		discount = *amountMinor
	default:
		return couponPlan{}, ErrCouponDisabled
	}

	// A discount larger than the eligible value is clamped: the customer cannot be
	// paid to buy, and the invoice cannot carry a negative subtotal.
	if discount > base {
		discount = base
	}
	if discount <= 0 {
		return couponPlan{}, ErrCouponNoDiscount
	}

	plan := couponPlan{
		couponID:  couponID,
		code:      strings.ToUpper(code),
		amount:    discount,
		appliesTo: appliesTo,
		eligible:  eligible,
	}
	// A coupon with a recurring window has now discounted its first cycle. A lifetime
	// coupon (NULL) records no counter, because "unlimited" is not a cycle count.
	if recurring != nil {
		plan.cyclesApplied = 1
	}
	return plan, nil
}

// applyDiscount turns a priced draft into a discounted one, returning the snapshot it
// wrote.
//
// The mechanism, and why it is this one:
//
//   - The charge lines keep the catalog *net*. That is what makes the document
//     checkable: the customer can compare the line to the price list, and a support
//     agent can answer "why was I charged this" from the page.
//   - The discount is a separate `line_kind='discount'` line whose net is the total
//     net reduction, negative, with zero tax.
//   - Each charge line's tax is re-derived from its *discounted* net at the exact rate
//     the pricer recorded. Carrying negative tax on the discount line as well would
//     count the reduction twice and the invoice would under-collect.
//
// The discount is split across the eligible lines by largest remainder, so the parts
// sum exactly to the discount and no minor unit is invented or lost.
//
// Tax mode matters. Exclusive: tax is added to the discounted net. Inclusive: the
// quoted price contains the tax, so the discounted *gross* is reduced first and the
// tax is its share. Using the wrong one would change the total by the tax on the tax.
func applyDiscount(ctx context.Context, tx pgx.Tx, invoiceID string, snap billing.Snapshot,
	plan couponPlan, charges []charge, cur money.Currency) (billing.Snapshot, error) {

	// Read back the lines the pricer wrote. Their net and rate are the authority, not
	// the request: a pricer is free to compute net differently from unit × quantity.
	type storedLine struct {
		id          string
		position    int
		description string
		quantity    string
		unitMinor   int64
		netMinor    int64
		taxRate     string
		taxMinor    int64
	}
	rows, err := tx.Query(ctx, `
		SELECT id, position, description, quantity::text, unit_minor, net_minor,
		       tax_rate, tax_minor
		FROM invoice_lines WHERE invoice_id = $1 ORDER BY position`, invoiceID)
	if err != nil {
		return snap, fmt.Errorf("checkout: read draft lines: %w", err)
	}
	var stored []storedLine
	for rows.Next() {
		var l storedLine
		if err := rows.Scan(&l.id, &l.position, &l.description, &l.quantity, &l.unitMinor,
			&l.netMinor, &l.taxRate, &l.taxMinor); err != nil {
			rows.Close()
			return snap, fmt.Errorf("checkout: scan draft line: %w", err)
		}
		stored = append(stored, l)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return snap, fmt.Errorf("checkout: read draft lines: %w", err)
	}
	rows.Close()

	inclusive := strings.EqualFold(snap.TaxEvidence.Mode, "inclusive")

	// Weights are the eligible lines' quoted value: the net in exclusive mode, the
	// net plus tax (the gross) in inclusive mode. A zero-value line cannot absorb a
	// discount.
	weights := make([]int64, 0, len(plan.eligible))
	indexes := make([]int, 0, len(plan.eligible))
	for _, i := range plan.eligible {
		if i >= len(stored) {
			continue
		}
		w := stored[i].netMinor
		if inclusive {
			w += stored[i].taxMinor
		}
		if w <= 0 {
			continue
		}
		weights = append(weights, w)
		indexes = append(indexes, i)
	}
	if len(weights) == 0 {
		return snap, ErrCouponNoDiscount
	}
	parts, err := money.Allocate(money.New(plan.amount, cur), weights)
	if err != nil {
		return snap, fmt.Errorf("checkout: allocate discount: %w", err)
	}

	results := make([]billing.LineResult, 0, len(stored)+1)
	newTaxTotal := int64(0)
	netReduction := int64(0)

	for i, l := range stored {
		// The charge line's NET never changes. It stays the catalog amount, which is
		// what makes the document checkable: the customer can compare the line to the
		// price list. The discount is expressed by the discount line, and the tax is
		// re-derived from the *discounted* value so it is reduced exactly once.
		net, tax := l.netMinor, l.taxMinor
		if idx := indexOf(indexes, i); idx >= 0 {
			share := parts[idx].Minor()
			if inclusive {
				// The quoted price contains the tax, so the discount reduces the gross.
				// Tax is its share of the reduced gross and the net is what is left, so
				// net + tax is exactly the reduced gross; the net *reduction* is what the
				// discount line records.
				gross := l.netMinor + l.taxMinor - share
				tax, err = shareOfGross(gross, l.taxRate, cur)
				if err != nil {
					return snap, err
				}
				netReduction += l.netMinor - (gross - tax)
			} else {
				// Tax is added to the discounted net.
				tax, err = taxOnNet(l.netMinor-share, l.taxRate, cur)
				if err != nil {
					return snap, err
				}
				netReduction += share
			}
		}
		if _, err := tx.Exec(ctx, `
			UPDATE invoice_lines
			SET tax_minor = $2::bigint, total_minor = net_minor + $2::bigint
			WHERE id = $1`, l.id, tax); err != nil {
			return snap, fmt.Errorf("checkout: reprice line: %w", err)
		}
		newTaxTotal += tax
		results = append(results, billing.LineResult{
			Description:    l.description,
			QuantityMilli:  quantityMilli(l.quantity),
			UnitMinor:      l.unitMinor,
			Net:            money.New(net, cur),
			Tax:            money.New(tax, cur),
			Total:          money.New(net+tax, cur),
			TaxRate:        l.taxRate,
			ProductID:      chargeProvenance(charges, i).product,
			PriceVersionID: chargeProvenance(charges, i).version,
		})
	}

	// The discount line itself: the total net reduction, negative, and no tax.
	position := 0
	if len(stored) > 0 {
		position = stored[len(stored)-1].position
	}
	discountDesc := "Discount — coupon " + plan.code
	if netReduction <= 0 {
		return snap, ErrCouponNoDiscount
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO invoice_lines (invoice_id, position, description, quantity, unit_minor,
			net_minor, tax_rate, tax_minor, total_minor, line_kind)
		VALUES ($1, $2, $3, 1, $4::bigint, $4::bigint, '0', 0, $4::bigint, 'discount')`,
		invoiceID, position+1, discountDesc, -netReduction); err != nil {
		return snap, fmt.Errorf("checkout: insert discount line: %w", err)
	}
	results = append(results, billing.LineResult{
		Description:   discountDesc,
		QuantityMilli: 1000,
		UnitMinor:     -netReduction,
		Net:           money.New(-netReduction, cur),
		Tax:           money.Zero(cur),
		Total:         money.New(-netReduction, cur),
		TaxRate:       "0",
	})

	// subtotal is the sum of every line's net, the discount line included.
	newSubtotal := int64(0)
	for _, r := range results {
		newSubtotal += r.Net.Minor()
	}
	totals := billing.Totals{
		Subtotal: money.New(newSubtotal, cur),
		Tax:      money.New(newTaxTotal, cur),
		Total:    money.New(newSubtotal+newTaxTotal, cur),
	}
	if totals.Subtotal.Minor() < 0 || totals.Total.Minor() < 0 {
		// The database refuses this too, but a negative total must never reach the
		// ledger: it would be an invoice that pays the customer.
		return snap, ErrCouponNoDiscount
	}

	snap.Lines = results
	snap.Totals = totals
	snap.Currency = cur.Code()
	if _, err := tx.Exec(ctx, `
		UPDATE invoices SET subtotal_minor = $2, tax_minor = $3, total_minor = $4
		WHERE id = $1 AND status = 'draft'`,
		invoiceID, totals.Subtotal.Minor(), totals.Tax.Minor(), totals.Total.Minor()); err != nil {
		return snap, fmt.Errorf("checkout: update invoice totals: %w", err)
	}
	if err := writeSnapshot(ctx, tx, invoiceID, snap, plan, cur); err != nil {
		return snap, err
	}
	return snap, nil
}

// provenance is a line's product and price version, carried into the snapshot.
type provenance struct {
	product *string
	version *string
}

// chargeProvenance returns the catalog provenance of the requested line at index i.
//
// CreateDraft writes the lines in request order, so index i of the stored draft is
// request line i. Carrying it through billing.LineResult is what lets fulfilment find
// the product and plan without re-reading the order.
func chargeProvenance(charges []charge, i int) provenance {
	if i < 0 || i >= len(charges) {
		return provenance{}
	}
	return provenance{product: charges[i].line.ProductID, version: charges[i].line.PriceVersionID}
}

// taxOnNet returns tax as an exact share of a net amount, for exclusive mode.
func taxOnNet(net int64, rate string, cur money.Currency) (int64, error) {
	ratio, err := money.ParseRatio(rate)
	if err != nil {
		// A rate the pricer wrote that cannot be parsed is a bug in the pricer, not a
		// client error. Treating it as zero would silently drop tax.
		return 0, fmt.Errorf("checkout: unreadable tax rate %q: %w", rate, err)
	}
	if ratio.IsZero() {
		return 0, nil
	}
	tax, err := money.ApplyRate(money.New(net, cur), ratio, money.RoundHalfUp)
	if err != nil {
		return 0, err
	}
	return tax.Minor(), nil
}

// shareOfGross returns the tax contained in a gross amount, for inclusive mode.
func shareOfGross(gross int64, rate string, cur money.Currency) (int64, error) {
	ratio, err := money.ParseRatio(rate)
	if err != nil {
		return 0, fmt.Errorf("checkout: unreadable tax rate %q: %w", rate, err)
	}
	if ratio.IsZero() {
		return 0, nil
	}
	one := money.One()
	denom, err := money.RatioFromRat(new(money.Rat).Add(ratio.Rat(), one.Rat()))
	if err != nil {
		return 0, err
	}
	share, err := ratio.Divide(denom)
	if err != nil {
		return 0, err
	}
	tax, err := money.ApplyRate(money.New(gross, cur), share, money.RoundHalfUp)
	if err != nil {
		return 0, err
	}
	return tax.Minor(), nil
}

// writeSnapshot rewrites the draft's snapshot with the discounted lines and the coupon
// evidence.
//
// The snapshot is what makes the document reprintable and its numbers justifiable, so
// the discount has to be in it: the amount, the code, the coupon id and which lines it
// reduced. It is stored as an extra top-level key rather than by widening
// billing.Snapshot, because the snapshot's own schema is owned by the billing package
// and a reader that does not know the key simply ignores it.
func writeSnapshot(ctx context.Context, tx pgx.Tx, invoiceID string, snap billing.Snapshot,
	plan couponPlan, cur money.Currency) error {

	raw, err := json.Marshal(snap)
	if err != nil {
		return fmt.Errorf("checkout: encode snapshot: %w", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("checkout: decode snapshot: %w", err)
	}
	doc["discount"] = map[string]any{
		"coupon_id":    plan.couponID,
		"code":         plan.code,
		"amount_minor": plan.amount,
		"currency":     cur.Code(),
		"applies_to":   plan.appliesTo,
		"lines":        plan.eligible,
		"tax_note": "the discount line carries no tax; each charge line's tax is " +
			"computed on its discounted net",
	}
	if _, err := tx.Exec(ctx,
		`UPDATE invoices SET snapshot = $2 WHERE id = $1 AND status = 'draft'`,
		invoiceID, doc); err != nil {
		return fmt.Errorf("checkout: update snapshot: %w", err)
	}
	return nil
}

// insertRedemption records the use against the invoice it discounted.
//
// The unique (coupon_id, invoice_id) index makes a replayed checkout a no-op instead
// of a second redemption, which is what makes `max_uses` a fact rather than a guess. A
// free order has no invoice, so its redemption carries a null invoice id, which the
// partial unique index tolerates.
func insertRedemption(ctx context.Context, tx pgx.Tx, plan couponPlan, customerID,
	invoiceID, subscriptionID, currency string) error {
	if _, err := tx.Exec(ctx, `
		INSERT INTO coupon_redemptions (coupon_id, customer_id, invoice_id, subscription_id,
			discount_minor, currency)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT DO NOTHING`,
		plan.couponID, customerID, nullableString(invoiceID), nullableString(subscriptionID),
		plan.amount, currency); err != nil {
		return fmt.Errorf("checkout: record coupon redemption: %w", err)
	}
	return nil
}

// activateZeroTotal delivers a free order.
//
// There is no payment and no invoice, so the two things a settled payment does have to
// happen here: the subscriptions become active (or they would never renew and the
// service would silently expire), and a service row plus its create operation are
// written so the provisioner delivers it. This mirrors
// fulfilment.Applier.enqueueProvisioning, which is unexported and takes an invoice.
func (s *CheckoutService) activateZeroTotal(ctx context.Context, tx pgx.Tx, customerID string,
	lines []catalogLine, selections [][]configSelection, orderItemIDs, subscriptionIDs []string) ([]string, error) {

	serviceIDs := make([]string, 0, len(lines))
	for i, cl := range lines {
		// The adapter's target is configuration captured on the service, so a later
		// catalog edit cannot change what an in-flight provisioning call asks for.
		config := map[string]any{}
		plan, region, err := provisionTarget(ctx, tx, cl.priceVersionID)
		if err != nil {
			return nil, err
		}
		if plan != "" {
			config["plan_id"] = plan
		}
		if region != "" {
			config["region"] = region
		}

		var serviceID string
		// A one-time or free line has no subscription, so the service is written with none: the
		// absence is what tells dunning, the portal and the renewal engine that this service has
		// no schedule behind it.
		if err := tx.QueryRow(ctx, `
			INSERT INTO services (customer_id, subscription_id, product_id, status, generation, config)
			VALUES ($1, $2, $3, 'pending', 1, $4)
			RETURNING id`, customerID, nullableString(subscriptionIDs[i]), cl.productID, config).Scan(&serviceID); err != nil {
			return nil, fmt.Errorf("checkout: create service: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO service_operations (service_id, kind, operation_key, generation, status)
			VALUES ($1, 'create', $2, 1, 'queued')`, serviceID, "create:"+orderItemIDs[i]); err != nil {
			return nil, fmt.Errorf("checkout: enqueue provisioning: %w", err)
		}
		// The option selection is stored on the service, because the renewal engine
		// prices from it and the provisioner reads it to build the environment.
		for _, sel := range selections[i] {
			if _, err := tx.Exec(ctx, `
				INSERT INTO service_configs (service_id, config_option_id, value, amount_minor, setup_minor)
				VALUES ($1, $2, $3, $4, $5)
				ON CONFLICT (service_id, config_option_id) DO NOTHING`,
				serviceID, sel.optionID, sel.value, sel.amount, sel.setup); err != nil {
				return nil, fmt.Errorf("checkout: store service config: %w", err)
			}
		}
		serviceIDs = append(serviceIDs, serviceID)
	}

	// A free order is paid the moment it is placed; the worker advances it to
	// 'fulfilling' and then 'completed' as the provisioning operations settle.
	if _, err := tx.Exec(ctx, `
		UPDATE subscriptions SET status = 'active', version = version + 1
		WHERE id = ANY($1::uuid[])`, compactIDs(subscriptionIDs)); err != nil {
		return nil, fmt.Errorf("checkout: activate subscriptions: %w", err)
	}
	return serviceIDs, nil
}

// provisionTarget reads the adapter plan and region a price version provisions into.
func provisionTarget(ctx context.Context, tx pgx.Tx, priceVersionID string) (plan, region string, err error) {
	var p, r *string
	if err := tx.QueryRow(ctx, `
		SELECT provision_plan, provision_region FROM price_versions WHERE id = $1`,
		priceVersionID).Scan(&p, &r); err != nil {
		return "", "", fmt.Errorf("checkout: read provision target: %w", err)
	}
	if p != nil {
		plan = *p
	}
	if r != nil {
		region = *r
	}
	return plan, region, nil
}

// queueInvoice writes the "your invoice is ready" message in the same transaction as
// the invoice, so a crash cannot lose it and a retry cannot send it twice.
func (s *CheckoutService) queueInvoice(ctx context.Context, tx pgx.Tx, invoiceID, customerID,
	number string, total money.Amount, dueAt time.Time) error {
	if s.queue == nil {
		return nil
	}
	var (
		email    string
		verified bool
		name     string
		userID   *string
	)
	if err := tx.QueryRow(ctx, `
		SELECT c.email, COALESCE(u.email_verified_at IS NOT NULL, false), c.name,
		       (SELECT ca.user_id::text FROM customer_accounts ca
		         WHERE ca.customer_id = c.id
		         ORDER BY (ca.role = 'owner') DESC, ca.created_at
		         LIMIT 1)
		FROM customers c
		LEFT JOIN users u ON lower(u.email) = lower(c.email)
		WHERE c.id = $1
		ORDER BY u.created_at
		LIMIT 1`, customerID).Scan(&email, &verified, &name, &userID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return fmt.Errorf("checkout: read invoice recipient: %w", err)
	}
	in := notify.InvoiceIssuedInput{
		Email:        email,
		InvoiceID:    invoiceID,
		Number:       number,
		CustomerName: name,
		Total:        total.Format(),
		DueAt:        dueAt,
	}
	if userID != nil {
		in.UserID = *userID
	}
	if !verified {
		// "We did not send it" and "we failed to send it" are different answers to a support
		// ticket, and only one of them is a bug. The hold records the rendered message as
		// suppressed; the inbox row is still written, because the panel is a channel we know the
		// customer can reach.
		in.HoldMailReason = "recipient address is not verified"
	}
	// The wording now comes from the editable new_invoice_created template, so what a customer
	// reads is the operator's sentence on the same branded shell as every other mail — instead of
	// a Go literal that no screen could change.
	_, err := notify.InvoiceIssued(ctx, tx, s.queue, in)
	return err
}

// loadCatalogLine reads the price version and the product rules that constrain it.
func loadCatalogLine(ctx context.Context, tx pgx.Tx, priceVersionID string) (catalogLine, error) {
	var c catalogLine
	err := tx.QueryRow(ctx, `
		SELECT pv.id, pv.product_id, pr.name, pv.name, pv.currency, pv.amount_minor,
		       pv.setup_minor, pv.interval, pv.anchor_day, pv.anchor_timezone,
		       pr.stock, pr.per_user_limit, pr.allow_quantity
		FROM price_versions pv
		JOIN products pr ON pr.id = pv.product_id
		WHERE pv.id = $1`, priceVersionID).
		Scan(&c.priceVersionID, &c.productID, &c.productName, &c.priceName, &c.currency,
			&c.amount, &c.setup, &c.interval, &c.anchorDay, &c.timezone,
			&c.stock, &c.perUserLimit, &c.allowQty)
	if errors.Is(err, pgx.ErrNoRows) {
		return catalogLine{}, ErrPriceNotFound
	}
	if err != nil {
		return catalogLine{}, fmt.Errorf("checkout: load price: %w", err)
	}
	return c, nil
}

// loadConfig validates a line's option selection and prices it from the catalog.
//
// An unknown option, a choice the option does not offer, or a missing required option
// is refused: silently dropping a selection would charge a price the customer did not
// agree to, and silently accepting one would price a service the provisioner cannot
// build.
func loadConfig(ctx context.Context, tx pgx.Tx, cl catalogLine, selected map[string]string) ([]configSelection, error) {
	rows, err := tx.Query(ctx, `
		SELECT co.id, co.name, co.option_type, co.choices, co.required, co.hidden,
		       COALESCE(cop.amount_minor, 0), COALESCE(cop.setup_minor, 0)
		FROM config_options co
		LEFT JOIN config_option_prices cop
		       ON cop.config_option_id = co.id AND cop.price_version_id = $2
		WHERE co.product_id = $1
		ORDER BY co.sort_order, co.name`, cl.productID, cl.priceVersionID)
	if err != nil {
		return nil, fmt.Errorf("checkout: load config options: %w", err)
	}
	defer rows.Close()

	type option struct {
		id       string
		name     string
		kind     string
		choices  []string
		required bool
		hidden   bool
		amount   int64
		setup    int64
	}
	var options []option
	for rows.Next() {
		var (
			o       option
			rawJSON []byte
		)
		if err := rows.Scan(&o.id, &o.name, &o.kind, &rawJSON, &o.required, &o.hidden,
			&o.amount, &o.setup); err != nil {
			return nil, fmt.Errorf("checkout: scan config option: %w", err)
		}
		if len(rawJSON) > 0 {
			if err := json.Unmarshal(rawJSON, &o.choices); err != nil {
				return nil, fmt.Errorf("checkout: decode config choices: %w", err)
			}
		}
		options = append(options, o)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("checkout: read config options: %w", err)
	}

	// Only options the product actually offers may be selected.
	known := make(map[string]option, len(options))
	for _, o := range options {
		known[o.id] = o
	}
	out := make([]configSelection, 0, len(selected))
	for id, value := range selected {
		o, ok := known[id]
		if !ok || o.hidden {
			return nil, fmt.Errorf("%w: unknown option %s", ErrInvalidConfig, id)
		}
		if strings.TrimSpace(value) == "" {
			continue
		}
		if len(o.choices) > 0 && !contains(o.choices, value) {
			return nil, fmt.Errorf("%w: %q is not an available choice for %s",
				ErrInvalidConfig, value, o.name)
		}
		out = append(out, configSelection{
			optionID: o.id, name: o.name, value: value,
			amount: o.amount, setup: o.setup,
		})
	}
	for _, o := range options {
		if !o.required || o.hidden {
			continue
		}
		if _, ok := selected[o.id]; !ok {
			return nil, fmt.Errorf("%w: %s must be chosen", ErrInvalidConfig, o.name)
		}
	}
	return out, nil
}

// periodFor derives the first billing period from the price's own schedule, the same
// way internal/orders does: deriving it from the anchor rather than adding a fixed
// number of days is what makes month-end and leap-day behaviour correct, and a
// day-count price ("every 45 days") is counted from the anchor day by the same rule.
// The rule itself lives in internal/calendar so no purchase path can invent its own.
func periodFor(c catalogLine, now time.Time) (time.Time, time.Time, error) {
	period, err := calendar.FirstPeriod(c.interval, c.anchorDay, c.timezone, now)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("%w: %v", ErrInvalidSchedule, err)
	}
	return period.Start, period.End, nil
}

// currencyOf resolves a persisted currency code.
func currencyOf(code string) (money.Currency, error) {
	if cur, ok := money.LookupCurrency(code); ok {
		return cur, nil
	}
	return money.NewCurrency(code, 2)
}

// configSummary renders the chosen options for a line description.
func configSummary(sel []configSelection) string {
	if len(sel) == 0 {
		return ""
	}
	parts := make([]string, 0, len(sel))
	for _, s := range sel {
		parts = append(parts, s.name+": "+s.value)
	}
	sort.Strings(parts)
	return strings.Join(parts, ", ")
}

func configAmount(sel []configSelection) int64 {
	var total int64
	for _, s := range sel {
		total += s.amount
	}
	return total
}

func configSetup(sel []configSelection) int64 {
	var total int64
	for _, s := range sel {
		total += s.setup
	}
	return total
}

// priceVersionIDs lists the catalog versions an order was priced from.
func priceVersionIDs(lines []catalogLine) []string {
	seen := make(map[string]struct{}, len(lines))
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		if _, ok := seen[l.priceVersionID]; ok {
			continue
		}
		seen[l.priceVersionID] = struct{}{}
		out = append(out, l.priceVersionID)
	}
	return out
}

// quantityMilli converts the numeric text Postgres returned back to thousandths.
func quantityMilli(text string) int64 {
	var whole, frac int64
	parts := strings.SplitN(text, ".", 2)
	if len(parts) > 0 {
		fmt.Sscanf(parts[0], "%d", &whole)
	}
	if len(parts) > 1 {
		f := parts[1]
		if len(f) > 3 {
			f = f[:3]
		}
		for len(f) < 3 {
			f += "0"
		}
		fmt.Sscanf(f, "%d", &frac)
	}
	return whole*1000 + frac
}

func indexOf(xs []int, v int) int {
	for i, x := range xs {
		if x == v {
			return i
		}
	}
	return -1
}

func contains(xs []string, v string) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}

func ptr(s string) *string { return &s }

func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// validID is a UUID shape check, so a malformed id is a 404 that never reaches the
// database and "not a uuid" cannot be told from "does not exist".
func validID(id string) bool {
	if len(id) != 36 {
		return false
	}
	for i, r := range id {
		switch i {
		case 8, 13, 18, 23:
			if r != '-' {
				return false
			}
		default:
			if (r < '0' || r > '9') && (r < 'a' || r > 'f') && (r < 'A' || r > 'F') {
				return false
			}
		}
	}
	return true
}
