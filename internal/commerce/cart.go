// Cart: a draft order, not a second pricing engine.
//
// The migration says it plainly: `carts`/`cart_items` hold a *selection* and its currency,
// and there is no money column anywhere in them. That is the whole design, and it is what
// makes the following true:
//
//   - A price change between "add to cart" and "checkout" re-prices the cart, because the
//     cart never stored a price to hold the shop to.
//   - A coupon cannot go stale: `Totals` validates it against the catalog's *current*
//     price every time it is asked.
//   - The cart's currency is fixed once it holds an item. Mixing currencies in one order is
//     meaningless without an exchange rate, and this system deliberately has none.
//
// The three rules the code enforces:
//
//  1. **Get-or-create per customer.** `carts.customer_id` is unique, so a second "add to
//     cart" joins the first rather than silently creating a parallel cart.
//  2. **The price version must exist and be sellable.** An add validates the version and
//     the product's `allow_quantity` policy, because a `disabled` product with quantity 3
//     is a request the shop cannot fulfil.
//  3. **Every total re-prices from the catalog.** `Totals` reads the current amount and
//     setup fee for each line's price version, applies the coupon if one is attached, and
//     returns the money the checkout will charge.
//
// Everything here is scoped by `customer_id`, and the HTTP layer derives that from the
// session — never from the body. A cart id is not a capability.

package commerce

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/catalyst/billing/internal/billing"
	"github.com/catalyst/billing/internal/money"
	"github.com/jackc/pgx/v5"
)

// Quantity policies from `products.allow_quantity`.
const (
	QuantityDisabled  = "disabled"
	QuantitySeparated = "separated"
	QuantityCombined  = "combined"
)

// MaxCartQuantity bounds one line, matching the table's CHECK. A typo of "100000" should be
// a validation error, not an order for a hundred thousand servers.
const MaxCartQuantity = 1000

// PurchaseUnits translates a requested quantity into what the shop actually creates, from
// the product's `allow_quantity` policy:
//
//   - disabled: exactly one unit; a larger quantity is refused.
//   - combined: one unit carrying the requested quantity (`services.quantity` is N).
//   - separated: N independent units of one each, so each gets its own order line,
//     subscription and service.
//
// It lives beside the cart because the cart and the checkout must agree on the rule: a cart
// that accepted a quantity the checkout then refuses is the defect this spells once. The
// returned numbers are also what bounds the expansion — a separated quantity of 1000 is 1000
// units, and the caller decides whether that is a document it can write.
func PurchaseUnits(policy string, quantity int) (units, perUnit int, err error) {
	if quantity < 1 {
		return 0, 0, invalid("quantity must be positive")
	}
	if quantity > MaxCartQuantity {
		return 0, 0, invalid("quantity must not exceed %d", MaxCartQuantity)
	}
	switch policy {
	case QuantityDisabled:
		if quantity > 1 {
			return 0, 0, fmt.Errorf("%w: this product does not allow a quantity", ErrNotApplicable)
		}
		return 1, 1, nil
	case QuantityCombined:
		return 1, quantity, nil
	case QuantitySeparated:
		return quantity, 1, nil
	default:
		return 0, 0, fmt.Errorf("%w: product has an unknown quantity policy %q",
			ErrNotApplicable, policy)
	}
}

// Cart is one customer's open cart.
type Cart struct {
	ID         string     `json:"id"`
	CustomerID string     `json:"customer_id"`
	Currency   string     `json:"currency"`
	CouponID   *string    `json:"coupon_id,omitempty"`
	Items      []CartItem `json:"items"`
	// TosAcceptedAt records terms acceptance against the cart, so checkout can refuse a
	// cart whose acceptance predates a change to the terms.
	TosAcceptedAt *time.Time `json:"tos_accepted_at,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
	// Created reports that the read that produced this value also created the cart. It is
	// carried out of the store because only the store can tell "the first line in a new cart"
	// from "another line in an existing one", and the caller needs that to name the event it
	// publishes. It is not part of the wire shape.
	Created bool `json:"-"`
}

// CartItem is one line of a cart: what was chosen, never what it costs.
type CartItem struct {
	ID             string            `json:"id"`
	PriceVersionID string            `json:"price_version_id"`
	ProductID      string            `json:"product_id"`
	ProductName    string            `json:"product_name"`
	PriceName      string            `json:"price_name"`
	Quantity       int               `json:"quantity"`
	Config         map[string]string `json:"config"`
	Label          string            `json:"label"`
	CreatedAt      time.Time         `json:"created_at"`
}

// AddItemRequest is one "add to cart".
type AddItemRequest struct {
	PriceVersionID string            `json:"price_version_id"`
	Quantity       int               `json:"quantity"`
	Config         map[string]string `json:"config"`
	Label          string            `json:"label"`
}

// LineTotal is one priced cart line.
type LineTotal struct {
	ItemID         string `json:"item_id"`
	PriceVersionID string `json:"price_version_id"`
	ProductID      string `json:"product_id"`
	Description    string `json:"description"`
	Quantity       int    `json:"quantity"`
	// Unit and Setup are the catalog's current amounts for the version.
	Unit  money.Amount `json:"-"`
	Setup money.Amount `json:"-"`
	// Line is unit*quantity; SetupLine is setup*quantity; Discount is the coupon's share of
	// the two. Net is Line+SetupLine-Discount, before tax.
	Line        money.Amount     `json:"-"`
	SetupLine   money.Amount     `json:"-"`
	Discount    money.Amount     `json:"-"`
	Net         money.Amount     `json:"-"`
	Tax         money.Amount     `json:"-"`
	Total       money.Amount     `json:"-"`
	TaxRate     string           `json:"tax_rate"`
	TaxEvidence *TaxLineEvidence `json:"tax_evidence,omitempty"`
}

// TaxLineEvidence is the per-line tax explanation, so a cart can say "incl. 19% VAT".
type TaxLineEvidence struct {
	Jurisdiction    string `json:"jurisdiction,omitempty"`
	Rate            string `json:"rate"`
	Inclusive       bool   `json:"inclusive"`
	ExemptionReason string `json:"exemption_reason,omitempty"`
}

// Totals is the priced cart.
type Totals struct {
	Currency string      `json:"currency"`
	Lines    []LineTotal `json:"lines"`
	// Subtotal is net of discount, before tax. Tax is the sum of the line taxes; Total is
	// what the checkout charges.
	Subtotal money.Amount `json:"-"`
	Discount money.Amount `json:"-"`
	Tax      money.Amount `json:"-"`
	Total    money.Amount `json:"-"`
	// Coupon is the applied coupon, when one is attached and valid.
	Coupon *Discount `json:"coupon,omitempty"`
	// CouponProblem explains why an attached coupon is not reducing anything. A cart with
	// an expired coupon shows the refusal rather than silently dropping the discount.
	CouponProblem string `json:"coupon_problem,omitempty"`
}

// ------------------------------------------------------------- get/create --

// Cart returns the customer's open cart, creating an empty one when none exists.
//
// `currency` is only used on creation: the cart's currency is fixed once it has an item, so
// a later request with a different currency does not silently re-denominate a selection.
func (s *Store) Cart(ctx context.Context, customerID, currency string) (Cart, error) {
	var out Cart
	err := s.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = s.CartTx(ctx, tx, customerID, currency)
		return err
	})
	return out, err
}

// CartTx is Cart inside a caller's transaction.
func (s *Store) CartTx(ctx context.Context, q querier, customerID, currency string) (Cart, error) {
	if strings.TrimSpace(customerID) == "" {
		return Cart{}, invalid("a cart needs a customer")
	}
	currency = strings.ToUpper(strings.TrimSpace(currency))
	if currency == "" {
		// No currency named: use the configured default, which is what the storefront
		// starts on.
		var code string
		err := q.QueryRow(ctx, `SELECT code FROM currencies WHERE is_default`).Scan(&code)
		if errors.Is(err, pgx.ErrNoRows) {
			return Cart{}, fmt.Errorf("%w: no default currency is configured", ErrInvalidInput)
		}
		if err != nil {
			return Cart{}, fmt.Errorf("commerce: read default currency: %w", err)
		}
		currency = code
	}
	if _, err := money.NewCurrency(currency, currencyScale(currency)); err != nil {
		return Cart{}, invalid("currency %q is not valid", currency)
	}

	var (
		id string
		// created is non-nil exactly when the insert did not conflict, which is the only
		// moment "this cart came into existence" is knowable. xmax = 0 is PostgreSQL's own way
		// of saying "this row version was inserted by this statement"; a conflicting update
		// returns a row whose xmax is set. Counting the lines instead would be wrong: adding a
		// second unit of the same product is an update, not a creation.
		created *bool
	)
	// The insert is a no-op when a cart already exists, so two concurrent "open my cart"
	// requests cannot race into two rows.
	err := q.QueryRow(ctx, `
		INSERT INTO carts (customer_id, currency)
		VALUES ($1, $2)
		ON CONFLICT (customer_id) DO UPDATE SET updated_at = now()
		RETURNING id, (xmax = 0)`, customerID, currency).Scan(&id, &created)
	if err != nil {
		if isForeignKeyViolation(err) {
			return Cart{}, fmt.Errorf("%w: customer %s does not exist", ErrNotFound, customerID)
		}
		if isCheckViolation(err) {
			return Cart{}, invalid("currency %q is not valid", currency)
		}
		return Cart{}, fmt.Errorf("commerce: open cart: %w", err)
	}
	out, err := s.cartWith(ctx, q, id, customerID)
	if err != nil {
		return Cart{}, err
	}
	out.Created = created != nil && *created
	return out, nil
}

// CartForCustomer reads the customer's cart without creating one.
func (s *Store) CartForCustomer(ctx context.Context, customerID string) (Cart, error) {
	return s.cartForCustomerWith(ctx, s.pool, customerID)
}

// cartForCustomerWith reads a cart on a querier, so a checkout inside a transaction prices the
// cart it can see in that transaction rather than a snapshot from outside it.
func (s *Store) cartForCustomerWith(ctx context.Context, q querier, customerID string) (Cart, error) {
	var id string
	err := q.QueryRow(ctx, `SELECT id FROM carts WHERE customer_id = $1`, customerID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return Cart{}, ErrNotFound
	}
	if err != nil {
		return Cart{}, fmt.Errorf("commerce: read cart: %w", err)
	}
	return s.cartWith(ctx, q, id, customerID)
}

// cartWith reads one cart scoped to its owner. The customer predicate is the security
// boundary: a cart id is not a capability, so a mismatched owner is a 404.
func (s *Store) cartWith(ctx context.Context, q querier, id, customerID string) (Cart, error) {
	var c Cart
	err := q.QueryRow(ctx, `
		SELECT id, customer_id, currency, coupon_id, tos_accepted_at, created_at, updated_at
		  FROM carts
		 WHERE id = $1 AND customer_id = $2`, id, customerID).
		Scan(&c.ID, &c.CustomerID, &c.Currency, &c.CouponID, &c.TosAcceptedAt,
			&c.CreatedAt, &c.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Cart{}, ErrNotFound
	}
	if err != nil {
		return Cart{}, fmt.Errorf("commerce: read cart: %w", err)
	}
	items, err := s.cartItems(ctx, q, c.ID)
	if err != nil {
		return Cart{}, err
	}
	c.Items = items
	return c, nil
}

func (s *Store) cartItems(ctx context.Context, q querier, cartID string) ([]CartItem, error) {
	rows, err := q.Query(ctx, `
		SELECT ci.id, ci.price_version_id, pv.product_id, p.name, pv.name, ci.quantity,
		       ci.config, ci.label, ci.created_at
		  FROM cart_items ci
		  JOIN price_versions pv ON pv.id = ci.price_version_id
		  JOIN products p ON p.id = pv.product_id
		 WHERE ci.cart_id = $1
		 ORDER BY ci.created_at, ci.id`, cartID)
	if err != nil {
		return nil, fmt.Errorf("commerce: list cart items: %w", err)
	}
	defer rows.Close()
	out := make([]CartItem, 0, 4)
	for rows.Next() {
		var item CartItem
		var raw []byte
		if err := rows.Scan(&item.ID, &item.PriceVersionID, &item.ProductID, &item.ProductName,
			&item.PriceName, &item.Quantity, &raw, &item.Label, &item.CreatedAt); err != nil {
			return nil, fmt.Errorf("commerce: scan cart item: %w", err)
		}
		item.Config = decodeConfig(raw)
		out = append(out, item)
	}
	return out, rows.Err()
}

func decodeConfig(raw []byte) map[string]string {
	out := map[string]string{}
	if len(raw) == 0 {
		return out
	}
	// The column is jsonb, so a malformed value cannot exist; an error here would mean the
	// driver returned something unexpected, and an empty selection is the safe reading.
	_ = json.Unmarshal(raw, &out)
	if out == nil {
		out = map[string]string{}
	}
	return out
}

// ------------------------------------------------------------------ add --

// AddItem adds a selection to the cart, or increments an existing identical line.
//
// "Identical" means the same price version and the same config, which is exactly the
// table's unique key. A cart is a selection, not the receipt: it stores the quantity the
// customer asked for, and the checkout turns that into the purchase shape the product's
// policy names — one line of N for `combined`, N independent lines of one for `separated`.
// That is why the cart rows do not have to be split here: the mode is a catalog rule, and
// the checkout is where it becomes order lines, subscriptions and services.
func (s *Store) AddItem(ctx context.Context, customerID, currency string, req AddItemRequest) (Cart, error) {
	var out Cart
	err := s.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = s.AddItemTx(ctx, tx, customerID, currency, req)
		return err
	})
	return out, err
}

// AddItemTx adds an item inside a caller's transaction.
func (s *Store) AddItemTx(ctx context.Context, q querier, customerID, currency string, req AddItemRequest) (Cart, error) {
	cart, err := s.CartTx(ctx, q, customerID, currency)
	if err != nil {
		return Cart{}, err
	}
	price, err := s.loadPrice(ctx, q, req.PriceVersionID)
	if err != nil {
		return Cart{}, err
	}
	// Validate the selection against the catalog now, so a cart cannot hold an option the
	// product does not offer or a choice it does not have. Required options are not enforced
	// here: a cart is a draft, and the checkout is where an incomplete selection is refused.
	// The point of validating at all is that Totals prices from the same rule, so an invalid
	// selection would otherwise turn into a cart that cannot be priced.
	if _, err := LoadConfigSelections(ctx, q, price.ProductID, price.ID, req.Config, false); err != nil {
		return Cart{}, err
	}
	// The cart's currency is fixed once it holds an item. A price in another currency
	// cannot join, because there is no exchange rate to convert it with.
	if !strings.EqualFold(price.Currency, cart.Currency) && len(cart.Items) > 0 {
		return Cart{}, fmt.Errorf("%w: the cart is in %s and this price is in %s",
			ErrNotApplicable, cart.Currency, price.Currency)
	}
	if !strings.EqualFold(price.Currency, cart.Currency) {
		// The cart is empty: adopt the price's currency rather than refusing. This is the
		// "switch currency, then add" flow, and it is unambiguous because nothing is lost.
		if _, err := q.Exec(ctx, `UPDATE carts SET currency = $2, updated_at = now(), abandoned_at = NULL WHERE id = $1`,
			cart.ID, price.Currency); err != nil {
			return Cart{}, fmt.Errorf("commerce: set cart currency: %w", err)
		}
		cart.Currency = price.Currency
	}

	quantity := req.Quantity
	if quantity == 0 {
		quantity = 1
	}
	// Validate the quantity against the product's policy with the same rule the checkout
	// expands with, so a cart cannot hold a request the checkout will refuse.
	if _, _, err := PurchaseUnits(price.AllowQuantity, quantity); err != nil {
		return Cart{}, err
	}
	// ceiling is how many of this line the cart may hold. A product that is sold one at a time
	// has a ceiling of one, which is what turns a second add into a refusal instead of a merge.
	ceiling := MaxCartQuantity
	if price.AllowQuantity == QuantityDisabled {
		ceiling = 1
	}

	config, err := encodeConfig(req.Config)
	if err != nil {
		return Cart{}, err
	}
	// An identical line merges; a different one is a new line. The upsert is on the same key
	// the table's unique constraint uses, so the two cannot disagree.
	//
	// The merge adds the quantities and refuses a total above `ceiling`. Silently capping at the
	// limit would accept a request the shop cannot fulfil and then show a different number, which
	// is worse than refusing it. The `WHERE` on the update is what makes the refusal atomic: a
	// check-then-write would let two concurrent adds both pass it, and the row the customer would
	// then hold is one the checkout refuses (`quantity_not_allowed`).
	var itemID string
	err = q.QueryRow(ctx, `
		INSERT INTO cart_items (cart_id, price_version_id, quantity, config, label)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (cart_id, price_version_id, config) DO UPDATE
			SET quantity = cart_items.quantity + EXCLUDED.quantity,
			    label = CASE WHEN EXCLUDED.label <> '' THEN EXCLUDED.label ELSE cart_items.label END
			WHERE cart_items.quantity + EXCLUDED.quantity <= $6
		RETURNING id`,
		cart.ID, price.ID, quantity, config, strings.TrimSpace(req.Label), ceiling).Scan(&itemID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// The line is already there and cannot grow. Which refusal that is depends on why:
			// a one-at-a-time product is "already in your cart", and a quantity product is
			// "that is more than one line may hold".
			if ceiling == 1 {
				return Cart{}, fmt.Errorf("%w: this product is sold one at a time and is already in your cart",
					ErrAlreadyInCart)
			}
			return Cart{}, invalid("quantity must be between 1 and %d", MaxCartQuantity)
		}
		if isForeignKeyViolation(err) {
			return Cart{}, fmt.Errorf("%w: no such price", ErrNotFound)
		}
		if isCheckViolation(err) {
			return Cart{}, invalid("quantity must be between 1 and %d", MaxCartQuantity)
		}
		return Cart{}, fmt.Errorf("commerce: add cart item: %w", err)
	}
	if _, err := q.Exec(ctx, `UPDATE carts SET updated_at = now(), abandoned_at = NULL WHERE id = $1`, cart.ID); err != nil {
		return Cart{}, fmt.Errorf("commerce: touch cart: %w", err)
	}
	out, err := s.cartWith(ctx, q, cart.ID, customerID)
	if err != nil {
		return Cart{}, err
	}
	// The pre-add read is the only one that knows whether the cart existed, so its answer is
	// carried onto the post-add value the caller receives.
	out.Created = cart.Created
	return out, nil
}

// UpdateQuantity sets a line's quantity. Zero is not a delete: removing a line is its own
// operation, so a client that sends 0 by mistake gets a validation error instead of a
// silently emptied cart.
func (s *Store) UpdateQuantity(ctx context.Context, customerID, itemID string, quantity int) (Cart, error) {
	var out Cart
	err := s.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = s.UpdateQuantityTx(ctx, tx, customerID, itemID, quantity)
		return err
	})
	return out, err
}

// UpdateQuantityTx sets a line's quantity inside a caller's transaction.
func (s *Store) UpdateQuantityTx(ctx context.Context, q querier, customerID, itemID string, quantity int) (Cart, error) {
	cart, err := s.cartOwningItem(ctx, q, customerID, itemID)
	if err != nil {
		return Cart{}, err
	}
	if quantity < 1 || quantity > MaxCartQuantity {
		return Cart{}, invalid("quantity must be between 1 and %d", MaxCartQuantity)
	}
	price, err := s.priceForItem(ctx, q, itemID)
	if err != nil {
		return Cart{}, err
	}
	if _, _, err := PurchaseUnits(price.AllowQuantity, quantity); err != nil {
		return Cart{}, err
	}
	tag, err := q.Exec(ctx, `UPDATE cart_items SET quantity = $3 WHERE id = $1 AND cart_id = $2`,
		itemID, cart.ID, quantity)
	if err != nil {
		if isCheckViolation(err) {
			return Cart{}, invalid("quantity must be between 1 and %d", MaxCartQuantity)
		}
		return Cart{}, fmt.Errorf("commerce: update cart quantity: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return Cart{}, ErrNotFound
	}
	if _, err := q.Exec(ctx, `UPDATE carts SET updated_at = now(), abandoned_at = NULL WHERE id = $1`, cart.ID); err != nil {
		return Cart{}, fmt.Errorf("commerce: touch cart: %w", err)
	}
	return s.cartWith(ctx, q, cart.ID, customerID)
}

// RemoveItem deletes one line.
func (s *Store) RemoveItem(ctx context.Context, customerID, itemID string) (Cart, error) {
	var out Cart
	err := s.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = s.RemoveItemTx(ctx, tx, customerID, itemID)
		return err
	})
	return out, err
}

// RemoveItemTx deletes one line inside a caller's transaction.
func (s *Store) RemoveItemTx(ctx context.Context, q querier, customerID, itemID string) (Cart, error) {
	cart, err := s.cartOwningItem(ctx, q, customerID, itemID)
	if err != nil {
		return Cart{}, err
	}
	tag, err := q.Exec(ctx, `DELETE FROM cart_items WHERE id = $1 AND cart_id = $2`, itemID, cart.ID)
	if err != nil {
		return Cart{}, fmt.Errorf("commerce: remove cart item: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return Cart{}, ErrNotFound
	}
	if _, err := q.Exec(ctx, `UPDATE carts SET updated_at = now(), abandoned_at = NULL WHERE id = $1`, cart.ID); err != nil {
		return Cart{}, fmt.Errorf("commerce: touch cart: %w", err)
	}
	return s.cartWith(ctx, q, cart.ID, customerID)
}

// Clear empties the cart but keeps it (and its currency), because "clear" is not "delete my
// cart": the next add should not have to re-pick a currency.
func (s *Store) Clear(ctx context.Context, customerID string) (Cart, error) {
	var out Cart
	err := s.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = s.ClearTx(ctx, tx, customerID)
		return err
	})
	return out, err
}

// ClearTx empties the cart inside a caller's transaction.
func (s *Store) ClearTx(ctx context.Context, q querier, customerID string) (Cart, error) {
	cart, err := s.CartTx(ctx, q, customerID, "")
	if err != nil {
		return Cart{}, err
	}
	if _, err := q.Exec(ctx, `DELETE FROM cart_items WHERE cart_id = $1`, cart.ID); err != nil {
		return Cart{}, fmt.Errorf("commerce: clear cart: %w", err)
	}
	// Clearing the selection clears the coupon too: a discount on nothing is a state the
	// checkout would have to explain, and "add a product, the coupon comes back" is not
	// what a customer expects.
	if _, err := q.Exec(ctx,
		`UPDATE carts SET coupon_id = NULL, tos_accepted_at = NULL, updated_at = now(), abandoned_at = NULL WHERE id = $1`,
		cart.ID); err != nil {
		return Cart{}, fmt.Errorf("commerce: reset cart: %w", err)
	}
	return s.cartWith(ctx, q, cart.ID, customerID)
}

// cartOwningItem resolves the caller's cart and refuses a line that is not in it. The join
// is the scoping: an item id from another customer's cart is a 404, not a 403, because the
// difference would confirm that the id exists.
func (s *Store) cartOwningItem(ctx context.Context, q querier, customerID, itemID string) (Cart, error) {
	var cartID string
	err := q.QueryRow(ctx, `
		SELECT c.id
		  FROM carts c
		  JOIN cart_items ci ON ci.cart_id = c.id
		 WHERE ci.id = $1 AND c.customer_id = $2`, itemID, customerID).Scan(&cartID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Cart{}, ErrNotFound
	}
	if err != nil {
		return Cart{}, fmt.Errorf("commerce: resolve cart item: %w", err)
	}
	return s.cartWith(ctx, q, cartID, customerID)
}

func (s *Store) priceForItem(ctx context.Context, q querier, itemID string) (priceRow, error) {
	var priceVersionID string
	if err := q.QueryRow(ctx, `SELECT price_version_id FROM cart_items WHERE id = $1`, itemID).
		Scan(&priceVersionID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return priceRow{}, ErrNotFound
		}
		return priceRow{}, fmt.Errorf("commerce: read cart item price: %w", err)
	}
	return s.loadPrice(ctx, q, priceVersionID)
}

// ----------------------------------------------------------------- coupon --

// ApplyCoupon attaches a coupon to the cart after validating it against the cart's own
// contents, so "apply" cannot succeed on a coupon that the totals will then refuse.
func (s *Store) ApplyCoupon(ctx context.Context, customerID, code string) (Cart, error) {
	var out Cart
	err := s.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = s.ApplyCouponTx(ctx, tx, customerID, code)
		return err
	})
	return out, err
}

// ApplyCouponTx attaches a coupon inside a caller's transaction.
//
// The validation is deliberately "does this code apply to *anything* in this cart?", not "to
// the first line". A coupon restricted to product B must be attachable to a cart holding A and
// B, and validating against line 1 would refuse it for the wrong reason. The per-line
// allocation happens in Totals, which is where the actual money is decided.
func (s *Store) ApplyCouponTx(ctx context.Context, q querier, customerID, code string) (Cart, error) {
	cart, err := s.CartTx(ctx, q, customerID, "")
	if err != nil {
		return Cart{}, err
	}
	if len(cart.Items) == 0 {
		return Cart{}, fmt.Errorf("%w: the cart is empty", ErrNotApplicable)
	}
	// Resolve the coupon once, so a missing or retired code is a 404 before any product
	// matching is attempted.
	coupon, err := s.couponByCodeWith(ctx, q, code)
	if err != nil {
		return Cart{}, err
	}
	// Find the first line the coupon covers and validate against it. An unrestricted coupon
	// covers every line, so this is line 1.
	var (
		eligible *cartLine
		refusal  error
	)
	for _, item := range cart.Items {
		price, err := s.loadPrice(ctx, q, item.PriceVersionID)
		if err != nil {
			return Cart{}, err
		}
		if len(coupon.ProductIDs) > 0 && !contains(coupon.ProductIDs, price.ProductID) {
			refusal = notApplicable("it does not apply to anything in this cart")
			continue
		}
		// The coupon is validated against the price the checkout will charge, so the option
		// contribution is part of the base it discounts.
		sel, err := LoadConfigSelections(ctx, q, price.ProductID, price.ID, item.Config, false)
		if err != nil {
			return Cart{}, err
		}
		amounts, err := lineAmountsWithConfig(price, item.Quantity, ConfigAmount(sel), ConfigSetup(sel))
		if err != nil {
			return Cart{}, err
		}
		eligible = &cartLine{item: item, price: price, line: amounts.line, setup: amounts.setup}
		break
	}
	if eligible == nil {
		if refusal != nil {
			return Cart{}, refusal
		}
		return Cart{}, notApplicable("it does not apply to anything in this cart")
	}
	if _, err := s.validateCouponWith(ctx, q, DiscountRequest{
		Code:       code,
		CustomerID: customerID,
		ProductID:  eligible.price.ProductID,
		Amount:     eligible.line,
		Setup:      eligible.setup,
	}); err != nil {
		return Cart{}, err
	}
	if _, err := q.Exec(ctx,
		`UPDATE carts SET coupon_id = $2, updated_at = now(), abandoned_at = NULL WHERE id = $1`,
		cart.ID, coupon.ID); err != nil {
		return Cart{}, fmt.Errorf("commerce: apply coupon: %w", err)
	}
	return s.cartWith(ctx, q, cart.ID, customerID)
}

// RemoveCoupon detaches the coupon. It is idempotent: removing a coupon from a cart that has
// none is the state the caller asked for.
func (s *Store) RemoveCoupon(ctx context.Context, customerID string) (Cart, error) {
	var out Cart
	err := s.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = s.RemoveCouponTx(ctx, tx, customerID)
		return err
	})
	return out, err
}

// RemoveCouponTx detaches the coupon inside a caller's transaction.
func (s *Store) RemoveCouponTx(ctx context.Context, q querier, customerID string) (Cart, error) {
	cart, err := s.CartTx(ctx, q, customerID, "")
	if err != nil {
		return Cart{}, err
	}
	if _, err := q.Exec(ctx,
		`UPDATE carts SET coupon_id = NULL, updated_at = now(), abandoned_at = NULL WHERE id = $1`, cart.ID); err != nil {
		return Cart{}, fmt.Errorf("commerce: remove coupon: %w", err)
	}
	return s.cartWith(ctx, q, cart.ID, customerID)
}

// ---------------------------------------------------------------- pricing --

// Totals re-prices the cart from the catalog.
//
// Every number here is read from `price_versions` at the moment of the call. Nothing is
// cached on the cart, which is what makes "the price changed while it sat in the cart" a
// non-event: the customer is shown the current price and the checkout charges it.
//
// `pricer` is optional. When it is nil the totals are net and `Total == Subtotal`; the HTTP
// layer supplies a TaxPricer built from the customer's billing country when tax is enabled.
func (s *Store) Totals(ctx context.Context, customerID string, pricer billing.Pricer) (Totals, error) {
	cart, err := s.CartForCustomer(ctx, customerID)
	if err != nil {
		return Totals{}, err
	}
	return s.totalsFor(ctx, s.pool, cart, pricer)
}

// TotalsTx re-prices the cart inside a caller's transaction, which is what checkout uses so
// the price it charges is the price it read under the same transaction.
func (s *Store) TotalsTx(ctx context.Context, q querier, customerID string, pricer billing.Pricer) (Totals, error) {
	cart, err := s.cartForCustomerWith(ctx, q, customerID)
	if err != nil {
		return Totals{}, err
	}
	return s.totalsFor(ctx, q, cart, pricer)
}

// cartLine is one cart row together with the catalog values it re-priced to.
type cartLine struct {
	item  CartItem
	price priceRow
	// line is unit*quantity; setup is setup*quantity; both before any discount. Unit and setup
	// already include the selected config options, so the cart total is the invoice total.
	line  money.Amount
	setup money.Amount
	// configAmount and configSetup are the option contributions folded into the two values
	// above, kept so the line can report its effective unit price without re-reading the
	// catalog.
	configAmount int64
	configSetup  int64
	desc         string
}

func (s *Store) totalsFor(ctx context.Context, q querier, cart Cart, pricer billing.Pricer) (Totals, error) {
	cur, err := money.NewCurrency(cart.Currency, currencyScale(cart.Currency))
	if err != nil {
		return Totals{}, fmt.Errorf("commerce: cart currency %q: %w", cart.Currency, err)
	}
	out := Totals{
		Currency: cart.Currency,
		Lines:    make([]LineTotal, 0, len(cart.Items)),
		Subtotal: money.Zero(cur),
		Discount: money.Zero(cur),
		Tax:      money.Zero(cur),
		Total:    money.Zero(cur),
	}
	if len(cart.Items) == 0 {
		return out, nil
	}

	// Price each line from the catalog first: a fixed coupon is allocated across lines, so
	// its value depends on every line's current amount.
	rows := make([]cartLine, 0, len(cart.Items))
	gross := money.Zero(cur)
	for _, item := range cart.Items {
		price, err := s.loadPrice(ctx, q, item.PriceVersionID)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				// A price version that disappeared (a deleted product) makes the cart
				// unpriceable. Saying so is better than silently dropping the line.
				return Totals{}, fmt.Errorf(
					"%w: cart line %s refers to a price that no longer exists", ErrNotFound, item.ID)
			}
			return Totals{}, err
		}
		if !strings.EqualFold(price.Currency, cart.Currency) {
			return Totals{}, fmt.Errorf("%w: the cart is in %s and price %s is in %s",
				ErrNotApplicable, cart.Currency, price.ID, price.Currency)
		}
		// Price the selected config options the same way the checkout does. The rule is
		// shared (LoadConfigSelections), so the cart total and the invoice cannot drift.
		sel, err := LoadConfigSelections(ctx, q, price.ProductID, price.ID, item.Config, false)
		if err != nil {
			return Totals{}, err
		}
		configAmount, configSetup := ConfigAmount(sel), ConfigSetup(sel)
		amounts, err := lineAmountsWithConfig(price, item.Quantity, configAmount, configSetup)
		if err != nil {
			return Totals{}, err
		}
		if gross, err = gross.Add(amounts.line); err != nil {
			return Totals{}, err
		}
		if gross, err = gross.Add(amounts.setup); err != nil {
			return Totals{}, err
		}
		desc := price.Description()
		if summary := ConfigSummary(sel); summary != "" {
			desc += " (" + summary + ")"
		}
		rows = append(rows, cartLine{
			item: item, price: price, line: amounts.line, setup: amounts.setup,
			configAmount: configAmount, configSetup: configSetup,
			desc: desc,
		})
	}

	// Resolve the coupon once, against the whole cart. A product-restricted coupon applies
	// only to the lines it covers, which is what a customer expects from "20% off VPS".
	var (
		applied       *Discount
		couponProblem string
	)
	// Every slot starts as a zero amount in the cart's currency rather than as the zero
	// value: an uninitialised Amount carries no currency, and "is this line discounted?"
	// would then be answered by an invalid comparison instead of by zero.
	perLine := make([]money.Amount, len(rows))
	for i := range perLine {
		perLine[i] = money.Zero(cur)
	}
	if cart.CouponID != nil {
		coupon, err := s.couponWith(ctx, q, *cart.CouponID)
		switch {
		case errors.Is(err, ErrNotFound):
			// The coupon was deleted out from under the cart. Clear the reference so the
			// cart is not permanently unpriceable.
			if _, uerr := q.Exec(ctx,
				// Deliberately does NOT clear abandoned_at: this is a read path clearing a
				// dead reference, and a merchant opening the cart is not the customer coming
				// back. Only a customer's own mutation revives an abandoned cart.
				`UPDATE carts SET coupon_id = NULL, updated_at = now() WHERE id = $1`, cart.ID); uerr != nil {
				return Totals{}, fmt.Errorf("commerce: clear missing coupon: %w", uerr)
			}
			couponProblem = "the promotion is no longer available"
		case err != nil:
			return Totals{}, err
		default:
			d, err := s.discountForCart(ctx, q, coupon, rows, cart.CustomerID)
			if err != nil {
				if !isCouponRefusal(err) {
					return Totals{}, err
				}
				// A refusal is shown on the cart, not raised: the customer can remove the
				// code and still check out.
				couponProblem = err.Error()
				break
			}
			applied = &d.Discount
			perLine = d.perLine
		}
	}

	// Build the billing lines. A cart line becomes at most two invoice lines: the recurring
	// price and the one-off setup fee, each with the discount already taken off. Handing the
	// pricer net lines is what keeps tax computed on what is actually charged.
	//
	// `lineOwner` records which cart row each billing line came from, so the priced results
	// can be attributed back without matching on descriptions. The pricer returns one result
	// per input line in input order; relying on that order is only safe if the mapping is
	// recorded here rather than inferred later.
	lines := make([]billing.Line, 0, len(rows)*2)
	lineOwner := make([]int, 0, len(rows)*2)
	for i, row := range rows {
		pricePart, setupPart := row.line, row.setup
		if !perLine[i].IsZero() {
			discount := perLine[i]
			// The discount reduces the recurring price first, then the setup fee. That is
			// the order a customer reads on the invoice, and it means a coupon smaller than
			// the price never touches the setup line.
			fromPrice := minAmount(discount, pricePart)
			if pricePart, err = pricePart.Sub(fromPrice); err != nil {
				return Totals{}, err
			}
			rest, err := discount.Sub(fromPrice)
			if err != nil {
				return Totals{}, err
			}
			if !rest.IsZero() {
				if setupPart, err = setupPart.Sub(minAmount(rest, setupPart)); err != nil {
					return Totals{}, err
				}
			}
			if out.Discount, err = out.Discount.Add(discount); err != nil {
				return Totals{}, err
			}
		}
		// A line reduced to zero is still emitted when there was no setup fee: the invoice
		// must show the product the customer selected, even at 0.00, rather than silently
		// dropping it from a fully discounted cart.
		if !pricePart.IsZero() || row.setup.IsZero() {
			lines = append(lines, billing.Line{
				Description:    row.desc,
				QuantityMilli:  int64(row.item.Quantity) * 1000,
				UnitPrice:      pricePart,
				ProductID:      ptr(row.price.ProductID),
				PriceVersionID: ptr(row.price.ID),
			})
			lineOwner = append(lineOwner, i)
		}
		if !setupPart.IsZero() {
			lines = append(lines, billing.Line{
				Description:    row.desc + " — setup",
				QuantityMilli:  int64(row.item.Quantity) * 1000,
				UnitPrice:      setupPart,
				ProductID:      ptr(row.price.ProductID),
				PriceVersionID: ptr(row.price.ID),
			})
			lineOwner = append(lineOwner, i)
		}
	}

	// Start every cart line at its undiscounted value, then replace it with the priced
	// result. A line with no tax pricer keeps the net value, which is the honest default for
	// an installation that has not configured tax.
	for i := range rows {
		lt := baseLineTotal(rows[i], cur)
		lt.Discount = perLine[i]
		lt.TaxRate = "0"
		lt.Net = grossLine(rows[i])
		if !lt.Discount.IsZero() {
			if lt.Net, err = lt.Net.Sub(lt.Discount); err != nil {
				return Totals{}, err
			}
		}
		lt.Tax = money.Zero(cur)
		lt.Total = lt.Net
		out.Lines = append(out.Lines, lt)
	}

	if pricer != nil && len(lines) > 0 {
		results, totals, ev, err := pricer.Price(ctx, lines, cur)
		if err != nil {
			return Totals{}, fmt.Errorf("commerce: price cart tax: %w", err)
		}
		out.Subtotal = totals.Subtotal
		out.Tax = totals.Tax
		out.Total = totals.Total
		if len(results) != len(lines) {
			return Totals{}, fmt.Errorf(
				"commerce: the tax pricer returned %d results for %d lines", len(results), len(lines))
		}
		// Reset the per-line money before summing the priced results: `Net` above was the
		// pre-tax estimate, and the pricer's net may differ from it under inclusive tax
		// (where the quoted price is the gross). Summing onto the estimate would double-count.
		for i := range out.Lines {
			out.Lines[i].Net = money.Zero(cur)
			out.Lines[i].Tax = money.Zero(cur)
			out.Lines[i].Total = money.Zero(cur)
		}
		for k, r := range results {
			lt := &out.Lines[lineOwner[k]]
			if lt.Net, err = lt.Net.Add(r.Net); err != nil {
				return Totals{}, err
			}
			if lt.Tax, err = lt.Tax.Add(r.Tax); err != nil {
				return Totals{}, err
			}
			if lt.Total, err = lt.Total.Add(r.Total); err != nil {
				return Totals{}, err
			}
		}
		// The evidence is per invoice, so one mode and one rate describe the whole cart. A
		// cart that spans two tax classes needs a per-line pricer, which is a checkout
		// decision rather than something a totals call should guess.
		for i := range out.Lines {
			out.Lines[i].TaxRate = ev.Rate
			out.Lines[i].TaxEvidence = &TaxLineEvidence{
				Jurisdiction:    ev.Jurisdiction,
				Rate:            ev.Rate,
				Inclusive:       ev.Mode == "inclusive",
				ExemptionReason: ev.ExemptionReason,
			}
		}
	} else {
		out.Subtotal = gross
		total := gross
		if !out.Discount.IsZero() {
			if total, err = total.Sub(out.Discount); err != nil {
				return Totals{}, err
			}
		}
		out.Total = total
	}

	out.Coupon = applied
	out.CouponProblem = couponProblem
	return out, nil
}

// baseLineTotal is the per-line shape before any pricing is attached.
func baseLineTotal(row cartLine, cur money.Currency) LineTotal {
	return LineTotal{
		ItemID:         row.item.ID,
		PriceVersionID: row.price.ID,
		ProductID:      row.price.ProductID,
		Description:    row.desc,
		Quantity:       row.item.Quantity,
		Unit:           money.New(row.price.Amount+row.configAmount, cur),
		Setup:          money.New(row.price.Setup+row.configSetup, cur),
		Line:           row.line,
		SetupLine:      row.setup,
		Net:            money.Zero(cur),
		Tax:            money.Zero(cur),
		Total:          money.Zero(cur),
	}
}

// grossLine is the undiscounted value of a line.
func grossLine(row cartLine) money.Amount {
	sum, err := row.line.Add(row.setup)
	if err != nil {
		return row.line
	}
	return sum
}

// discountForCart computes the coupon's value for a whole cart and allocates it per line.
//
// A coupon restricted to products applies only to the lines that sell those products. The
// fixed-amount case is allocated across the eligible lines by their net value using the
// money package's largest-remainder allocation, so the parts sum exactly to the discount.
func (s *Store) discountForCart(ctx context.Context, q querier, coupon Coupon,
	rows []cartLine, customerID string) (cartDiscount, error) {
	cur := rows[0].line.Currency()
	eligible := make([]int, 0, len(rows))
	for i, row := range rows {
		if len(coupon.ProductIDs) == 0 || contains(coupon.ProductIDs, row.price.ProductID) {
			eligible = append(eligible, i)
		}
	}
	if len(eligible) == 0 {
		return cartDiscount{}, notApplicable("it does not apply to anything in this cart")
	}

	at := s.now()
	if !coupon.Enabled {
		return cartDiscount{}, fmt.Errorf("%w: the promotion is no longer available", ErrNotApplicable)
	}
	if coupon.StartsAt != nil && at.Before(*coupon.StartsAt) {
		return cartDiscount{}, fmt.Errorf("%w: the promotion has not started yet", ErrExpired)
	}
	if coupon.ExpiresAt != nil && !at.Before(*coupon.ExpiresAt) {
		return cartDiscount{}, fmt.Errorf("%w: the promotion has expired", ErrExpired)
	}
	uses, err := s.redemptionCount(ctx, q, coupon.ID, "")
	if err != nil {
		return cartDiscount{}, err
	}
	if coupon.MaxUses != nil && uses >= int64(*coupon.MaxUses) {
		return cartDiscount{}, fmt.Errorf("%w: the promotion has been fully redeemed", ErrExhausted)
	}
	if customerID != "" && coupon.MaxUsesPerUser != nil {
		perUser, err := s.redemptionCount(ctx, q, coupon.ID, customerID)
		if err != nil {
			return cartDiscount{}, err
		}
		if perUser >= int64(*coupon.MaxUsesPerUser) {
			return cartDiscount{}, fmt.Errorf("%w: you have already used this promotion", ErrExhausted)
		}
	}

	perLine := make([]money.Amount, len(rows))
	for i := range rows {
		perLine[i] = money.Zero(cur)
	}

	switch coupon.Kind {
	case CouponPercentage:
		ratio, err := money.ParseRatio(strings.TrimSpace(coupon.Percentage) + "%")
		if err != nil {
			return cartDiscount{}, fmt.Errorf("commerce: coupon %s has an unreadable percentage: %w",
				coupon.Code, err)
		}
		for _, i := range eligible {
			base, err := couponBase(coupon, rows[i], cur)
			if err != nil {
				return cartDiscount{}, err
			}
			if base.IsZero() {
				continue
			}
			part, err := money.ApplyRate(base, ratio, money.RoundHalfUp)
			if err != nil {
				return cartDiscount{}, fmt.Errorf("commerce: apply percentage: %w", err)
			}
			perLine[i] = part
		}
	case CouponFixed:
		if coupon.AmountMinor == nil {
			return cartDiscount{}, fmt.Errorf("commerce: coupon %s has no amount", coupon.Code)
		}
		if !strings.EqualFold(coupon.Currency, cur.Code()) {
			return cartDiscount{}, notApplicable("it is issued in %s, not %s", coupon.Currency, cur.Code())
		}
		// The discount cannot exceed the eligible value; a larger fixed coupon makes those
		// lines free rather than producing a negative line.
		cap := money.Zero(cur)
		weights := make([]int64, 0, len(eligible))
		for _, i := range eligible {
			base, err := couponBase(coupon, rows[i], cur)
			if err != nil {
				return cartDiscount{}, err
			}
			weights = append(weights, base.Minor())
			if cap, err = cap.Add(base); err != nil {
				return cartDiscount{}, err
			}
		}
		wanted := money.New(*coupon.AmountMinor, cur)
		if cmp, err := wanted.Cmp(cap); err == nil && cmp > 0 {
			wanted = cap
		}
		if wanted.IsZero() || cap.IsZero() {
			break
		}
		parts, err := money.Allocate(wanted, weights)
		if err != nil {
			return cartDiscount{}, fmt.Errorf("commerce: allocate coupon: %w", err)
		}
		for k, i := range eligible {
			perLine[i] = parts[k]
		}
	default:
		return cartDiscount{}, fmt.Errorf("commerce: coupon %s has an unknown kind %q", coupon.Code, coupon.Kind)
	}

	total := money.Zero(cur)
	for _, part := range perLine {
		if total, err = total.Add(part); err != nil {
			return cartDiscount{}, err
		}
	}
	return cartDiscount{
		Discount: Discount{
			CouponID:        coupon.ID,
			Code:            coupon.Code,
			Price:           total,
			Setup:           money.Zero(cur),
			Total:           total,
			Currency:        cur.Code(),
			AppliesTo:       coupon.AppliesTo,
			RecurringCycles: coupon.RecurringCycles,
		},
		perLine: perLine,
	}, nil
}

// couponBase is the part of a line a coupon of this scope may reduce.
func couponBase(coupon Coupon, row cartLine, cur money.Currency) (money.Amount, error) {
	base := money.Zero(cur)
	if coupon.AppliesTo == AppliesToPrice || coupon.AppliesTo == AppliesToAll {
		base = row.line
	}
	if coupon.AppliesTo == AppliesToSetup || coupon.AppliesTo == AppliesToAll {
		sum, err := base.Add(row.setup)
		if err != nil {
			return money.Amount{}, err
		}
		base = sum
	}
	return base, nil
}

// cartDiscount carries the allocated per-line parts alongside the summary.
type cartDiscount struct {
	Discount
	perLine []money.Amount
}

// isCouponRefusal reports whether an error is a *reason the coupon does not apply* rather
// than a failure. A refusal is shown to the customer on the cart; a failure is a 500.
func isCouponRefusal(err error) bool {
	return errors.Is(err, ErrExpired) || errors.Is(err, ErrExhausted) ||
		errors.Is(err, ErrNotApplicable) || errors.Is(err, ErrNotFound)
}

// ------------------------------------------------------------------ price --

// priceRow is the catalog data a cart line prices from.
type priceRow struct {
	ID            string
	ProductID     string
	Product       string
	Name          string
	Currency      string
	Amount        int64
	Setup         int64
	AllowQuantity string
}

func (p priceRow) Description() string {
	if p.Name != "" {
		return p.Product + " — " + p.Name
	}
	return p.Product
}

// lineAmountsWithConfig adds the selected config options' per-unit recurring and setup amounts
// before multiplying by quantity. A config option is priced per unit, exactly as the checkout's
// order item unit_minor is, so a cart line of three with a paid option charges it three times.
func lineAmountsWithConfig(price priceRow, quantity int, configAmount, configSetup int64) (struct {
	line  money.Amount
	setup money.Amount
}, error) {
	var out struct {
		line  money.Amount
		setup money.Amount
	}
	cur, err := money.NewCurrency(price.Currency, currencyScale(price.Currency))
	if err != nil {
		return out, fmt.Errorf("commerce: price currency %q: %w", price.Currency, err)
	}
	if quantity <= 0 {
		return out, invalid("quantity must be positive")
	}
	unit, err := money.New(price.Amount+configAmount, cur).MulInt(int64(quantity))
	if err != nil {
		return out, fmt.Errorf("commerce: line value: %w", err)
	}
	setup, err := money.New(price.Setup+configSetup, cur).MulInt(int64(quantity))
	if err != nil {
		return out, fmt.Errorf("commerce: setup value: %w", err)
	}
	out.line = unit
	out.setup = setup
	return out, nil
}

// loadPrice reads the price version a cart line refers to, and the product policy it
// carries.
func (s *Store) loadPrice(ctx context.Context, q querier, id string) (priceRow, error) {
	var p priceRow
	err := q.QueryRow(ctx, `
		SELECT pv.id, pv.product_id, p.name, pv.name, pv.currency, pv.amount_minor,
		       pv.setup_minor, p.allow_quantity
		  FROM price_versions pv
		  JOIN products p ON p.id = pv.product_id
		 WHERE pv.id = $1`, id).
		Scan(&p.ID, &p.ProductID, &p.Product, &p.Name, &p.Currency, &p.Amount,
			&p.Setup, &p.AllowQuantity)
	if errors.Is(err, pgx.ErrNoRows) {
		return priceRow{}, fmt.Errorf("%w: no such price", ErrNotFound)
	}
	if err != nil {
		return priceRow{}, fmt.Errorf("commerce: load price: %w", err)
	}
	return p, nil
}

// encodeConfig renders a selection as the jsonb the table stores. An empty selection is
// `{}` rather than NULL, because the unique key includes the column.
func encodeConfig(config map[string]string) ([]byte, error) {
	if len(config) == 0 {
		return []byte(`{}`), nil
	}
	encoded, err := json.Marshal(config)
	if err != nil {
		return nil, invalid("config is not encodable: %v", err)
	}
	return encoded, nil
}

func ptr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
