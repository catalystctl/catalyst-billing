package service

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// Stock and per-customer limits (SF-07…SF-09).
//
// These functions are exported for the *checkout* path, which this package deliberately does
// not own. They are here because they are service-lifecycle rules — what happens to a
// product's availability when a service is sold and when it is terminated — and because the
// row-locking discipline that makes them correct belongs next to the rest of the lifecycle
// code rather than being re-derived by each caller.
//
// The signatures the integrator wires:
//
//	DecrementStock(ctx context.Context, tx pgx.Tx, productID string, n int) error
//	RestoreStock(ctx context.Context, tx pgx.Tx, productID string, n int) error
//	CheckPerUserLimit(ctx context.Context, tx pgx.Tx, productID, customerID string, n int) error
//	CheckPurchaseAllowed(ctx context.Context, tx pgx.Tx, productID, customerID string, n int) error
//
// All four take a pgx.Tx rather than a pool: a stock decrement that is not in the same
// transaction as the order it belongs to is a stock decrement that can succeed while the
// order rolls back, which sells a unit twice. Taking the transaction makes that impossible
// rather than merely discouraged.

// DecrementStock removes n units from a product's stock.
//
// The decrement happens in one statement with the guard in the WHERE clause, so two
// concurrent checkouts of the last unit cannot both succeed: the second UPDATE matches no row
// and the caller is told the product is out of stock. That is a stronger guarantee than
// SELECT ... FOR UPDATE followed by UPDATE, because it does not depend on the caller having
// remembered to lock first.
//
// A product with a NULL stock is unlimited and is left alone; that is the difference between
// "no row" and "zero" that a customer would notice.
//
// A non-positive n is a programming error rather than a no-op: a checkout that asks to take
// zero units is a bug upstream, and silently accepting it would hide the bug until an
// inventory report disagreed with the orders.
func DecrementStock(ctx context.Context, tx pgx.Tx, productID string, n int) error {
	if n <= 0 {
		return fmt.Errorf("%w: a stock decrement must be positive, got %d", ErrInvalidInput, n)
	}

	var (
		stock    *int
		affected int
	)
	err := tx.QueryRow(ctx, `
		UPDATE products
		SET stock = stock - $2
		WHERE id = $1 AND stock IS NOT NULL AND stock >= $2
		RETURNING stock`, productID, n).Scan(&stock)
	if err == nil {
		return nil
	}
	if err != pgx.ErrNoRows {
		return fmt.Errorf("service: decrement stock: %w", err)
	}

	// No row was updated. Either the product does not exist, it has unlimited stock, or it
	// has too few units left. Telling those apart is what lets the caller answer "sold out"
	// rather than "no such product", and what stops an unlimited product being reported as
	// having nothing left.
	var remaining *int
	if err := tx.QueryRow(ctx, `SELECT stock FROM products WHERE id = $1`, productID).
		Scan(&remaining); err != nil {
		if err == pgx.ErrNoRows {
			return ErrNotFound
		}
		return fmt.Errorf("service: read stock: %w", err)
	}
	if remaining == nil {
		// Unlimited: the decrement is a no-op by design, not a refusal.
		return nil
	}
	if *remaining < 0 {
		// A negative stock means a previous restore over-credited; refuse to make it worse.
		return fmt.Errorf("%w: the product's stock is corrupt (%d)", ErrOutOfStock, *remaining)
	}
	affected = *remaining
	return fmt.Errorf("%w: %d remaining, %d requested", ErrOutOfStock, affected, n)
}

// RestoreStock returns n units to a product's stock.
//
// A product with NULL stock stays NULL: "unlimited" must not become "unlimited + 3", which
// would silently turn an unlimited product into a finite one the first time an order was
// cancelled.
func RestoreStock(ctx context.Context, tx pgx.Tx, productID string, n int) error {
	if n <= 0 {
		return fmt.Errorf("%w: a stock restore must be positive, got %d", ErrInvalidInput, n)
	}

	tag, err := tx.Exec(ctx, `
		UPDATE products SET stock = stock + $2 WHERE id = $1 AND stock IS NOT NULL`,
		productID, n)
	if err != nil {
		return fmt.Errorf("service: restore stock: %w", err)
	}
	if tag.RowsAffected() == 0 {
		// Either the product does not exist or its stock is unlimited. Distinguishing them
		// keeps a cancellation of an unlimited product from reporting a missing product.
		var exists bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS (SELECT 1 FROM products WHERE id = $1)`, productID).Scan(&exists); err != nil {
			return fmt.Errorf("service: read product: %w", err)
		}
		if !exists {
			return ErrNotFound
		}
	}
	return nil
}

// CheckPerUserLimit refuses the (limit+1)th unit of a product for one customer.
//
// `products.per_user_limit` counts *live* services: a terminated service no longer occupies a
// slot, which is what makes "you may have 3 of these" a policy a customer can act on rather
// than a lifetime cap. n is how many more the caller is about to create, so a quantity of 2
// against a limit of 3 with one existing is refused rather than half-fulfilled.
func CheckPerUserLimit(ctx context.Context, tx pgx.Tx, productID, customerID string, n int) error {
	if n <= 0 {
		return fmt.Errorf("%w: a quantity must be positive, got %d", ErrInvalidInput, n)
	}

	var limit *int
	if err := tx.QueryRow(ctx, `SELECT per_user_limit FROM products WHERE id = $1`, productID).
		Scan(&limit); err != nil {
		if err == pgx.ErrNoRows {
			return ErrNotFound
		}
		return fmt.Errorf("service: read per-user limit: %w", err)
	}
	if limit == nil {
		return nil
	}

	var held int
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FROM services
		WHERE product_id = $1 AND customer_id = $2
		  AND status NOT IN ('terminated')`, productID, customerID).Scan(&held); err != nil {
		return fmt.Errorf("service: count customer services: %w", err)
	}
	if held+n > *limit {
		return fmt.Errorf("%w: %d of %d already held, %d more requested",
			ErrPerUserLimit, held, *limit, n)
	}
	return nil
}

// CheckPurchaseAllowed runs both purchase guards in the order that reports the more useful
// refusal first: the per-customer limit is a policy about *this* customer, and the stock is a
// fact about the product. Telling a customer "sold out" when the real reason is that they
// already hold their maximum sends them to support for nothing.
//
// The quantity policy (`allow_quantity`) is checked too, because it is the third rule the same
// migration added and a caller that enforces two of three will be asked about the third.
func CheckPurchaseAllowed(ctx context.Context, tx pgx.Tx, productID, customerID string, n int) error {
	if n <= 0 {
		return fmt.Errorf("%w: a quantity must be positive, got %d", ErrInvalidInput, n)
	}

	var allowQuantity string
	var stock *int
	if err := tx.QueryRow(ctx, `
		SELECT allow_quantity, stock FROM products WHERE id = $1`, productID).
		Scan(&allowQuantity, &stock); err != nil {
		if err == pgx.ErrNoRows {
			return ErrNotFound
		}
		return fmt.Errorf("service: read product rules: %w", err)
	}

	if allowQuantity == "disabled" && n > 1 {
		return fmt.Errorf("%w: this product cannot be bought in a quantity greater than one", ErrInvalidInput)
	}

	if err := CheckPerUserLimit(ctx, tx, productID, customerID, n); err != nil {
		return err
	}
	if stock != nil && n > *stock {
		return fmt.Errorf("%w: %d remaining, %d requested", ErrOutOfStock, *stock, n)
	}
	return nil
}

// ProductRules is the product's purchase policy, for a storefront that must render it.
type ProductRules struct {
	Stock         *int
	PerUserLimit  *int
	AllowQuantity string
}

// Rules reads a product's purchase policy.
func (s *Store) Rules(ctx context.Context, productID string) (ProductRules, error) {
	var r ProductRules
	err := s.pool.QueryRow(ctx, `
		SELECT stock, per_user_limit, allow_quantity FROM products WHERE id = $1`, productID).
		Scan(&r.Stock, &r.PerUserLimit, &r.AllowQuantity)
	if err != nil {
		if err == pgx.ErrNoRows {
			return ProductRules{}, ErrNotFound
		}
		return ProductRules{}, fmt.Errorf("service: read product rules: %w", err)
	}
	return r, nil
}
