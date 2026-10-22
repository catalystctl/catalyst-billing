// Package orders turns a catalog price into a payable order.
//
// A purchase is four rows that must agree: the order (what was bought and for how
// much), its items (the price version each line was priced from), the subscription
// (when it bills again) and the invoice (what is owed now). They are written in one
// transaction because a partial purchase is the worst state in the system: money
// taken for a service nobody will provision, or a service provisioned with no
// invoice behind it.
//
// Like `billing`, this package works exclusively inside a caller-supplied
// transaction so the caller can add its own rows (an audit entry, an event) to the
// same commit.
//
// This is the entry point the storefront and the operator panel were missing. Until
// now `orders` only ever appeared in test fixtures: there was no way to create one,
// so the whole "choose a product, pay, get it provisioned" path was unreachable.
package orders

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/catalyst/billing/internal/billing"
	"github.com/catalyst/billing/internal/calendar"
	"github.com/catalyst/billing/internal/money"
	"github.com/jackc/pgx/v5"
)

var (
	// ErrPriceNotFound is returned when the price version does not exist.
	ErrPriceNotFound = errors.New("orders: no such price")
	// ErrCustomerNotFound is returned when the billed customer does not exist.
	ErrCustomerNotFound = errors.New("orders: no such customer")
	// ErrInvalidQuantity is returned for a non-positive or absurd quantity.
	ErrInvalidQuantity = errors.New("orders: quantity must be between 1 and 1000")
	// ErrInvalidSchedule is returned when a price version cannot be turned into a
	// billing schedule (an unknown interval or timezone).
	ErrInvalidSchedule = errors.New("orders: the price has no valid billing schedule")
)

// maxQuantity bounds a single line. A typo of "100000" should be a validation error,
// not an invoice for a hundred thousand servers.
const maxQuantity = 1000

// Request is one purchase.
type Request struct {
	// CustomerID is the billing account the order belongs to. It must exist.
	CustomerID string
	// PriceVersionID selects what is bought; the price is read from the catalog, never
	// supplied by the caller, so a client cannot choose what to charge.
	PriceVersionID string
	// Quantity defaults to 1 when zero.
	Quantity int

	// Merchant, Terms and Series are the document defaults, passed in rather than
	// reached for, so this package has no opinion about configuration.
	Merchant billing.Merchant
	Terms    string
	Series   billing.NumberSeries
	// DueAt is when the resulting invoice becomes overdue.
	DueAt time.Time
	// Now is the clock, injectable for tests.
	Now time.Time
}

// Result identifies what was created.
type Result struct {
	OrderID        string
	InvoiceID      string
	InvoiceNumber  string
	SubscriptionID string
	// Status is the order status after the purchase. It is "pending_payment" until
	// the invoice settles; payment, fulfilment and provisioning advance it.
	Status string
	// Total is the invoice total, including tax, in the invoice currency.
	Total money.Amount
}

// Service creates orders.
type Service struct {
	issuer *billing.Issuer
}

// New constructs a Service.
func New(issuer *billing.Issuer) *Service { return &Service{issuer: issuer} }

// price is the catalog row a purchase is priced from.
type price struct {
	ID        string
	ProductID string
	Product   string
	Name      string
	Currency  string
	Amount    int64
	Setup     int64
	Interval  string
	AnchorDay int
	Timezone  string
}

// Create writes the order, its items, the subscription and an issued invoice.
//
// The invoice is issued, not left as a draft: a purchase is an offer to pay now, and a
// draft nobody can see is not. Issuing inside the same transaction means the ledger
// posting and the order commit or roll back together.
func (s *Service) Create(ctx context.Context, tx pgx.Tx, req Request) (Result, error) {
	if req.Quantity == 0 {
		req.Quantity = 1
	}
	if req.Quantity < 1 || req.Quantity > maxQuantity {
		return Result{}, ErrInvalidQuantity
	}
	if req.Series.Scope == "" {
		req.Series = billing.DefaultSeries
	}
	if req.Now.IsZero() {
		req.Now = time.Now().UTC()
	}
	if s == nil || s.issuer == nil {
		return Result{}, fmt.Errorf("orders: invoice issuance is not configured")
	}

	p, err := loadPrice(ctx, tx, req.PriceVersionID)
	if err != nil {
		return Result{}, err
	}
	cur, ok := money.LookupCurrency(p.Currency)
	if !ok {
		cur, err = money.NewCurrency(p.Currency, 2)
		if err != nil {
			return Result{}, fmt.Errorf("orders: currency %q: %w", p.Currency, err)
		}
	}

	// The customer must exist. Checking here turns a foreign-key violation into a
	// 404 the handler can explain, rather than a 500 from the driver.
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT true FROM customers WHERE id = $1`, req.CustomerID).
		Scan(&exists); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Result{}, ErrCustomerNotFound
		}
		return Result{}, fmt.Errorf("orders: load customer: %w", err)
	}

	start, end, err := periodFor(p, req.Now)
	if err != nil {
		return Result{}, err
	}

	lineMinor := p.Amount * int64(req.Quantity)
	setupMinor := p.Setup * int64(req.Quantity)

	var orderID string
	if err := tx.QueryRow(ctx, `
		INSERT INTO orders (customer_id, status, currency, total_minor)
		VALUES ($1, 'pending_payment', $2, $3)
		RETURNING id`, req.CustomerID, p.Currency, lineMinor+setupMinor).Scan(&orderID); err != nil {
		return Result{}, fmt.Errorf("orders: insert order: %w", err)
	}

	description := p.description()
	if _, err := tx.Exec(ctx, `
		INSERT INTO order_items (order_id, price_version_id, description, quantity,
			unit_minor, line_minor, setup_minor, position)
		VALUES ($1, $2, $3, $4, $5, $6, $7, 1)`,
		orderID, p.ID, description, req.Quantity, p.Amount, lineMinor, setupMinor,
	); err != nil {
		return Result{}, fmt.Errorf("orders: insert order item: %w", err)
	}

	// The subscription starts as 'pending' and is activated by the payment applier when
	// the invoice settles. A subscription that is never paid therefore never renews: it
	// must not become 'active' on an unpaid order.
	var subscriptionID string
	if err := tx.QueryRow(ctx, `
		INSERT INTO subscriptions (customer_id, price_version_id, status, interval,
			anchor_day, timezone, current_period_start, current_period_end, next_billing_at)
		VALUES ($1, $2, 'pending', $3, $4, $5, $6, $7, $7)
		RETURNING id`,
		req.CustomerID, p.ID, p.Interval, p.AnchorDay, p.Timezone, start, end,
	).Scan(&subscriptionID); err != nil {
		return Result{}, fmt.Errorf("orders: insert subscription: %w", err)
	}

	// The invoice is priced from the catalog line, not from the order total, so the
	// configured pricer computes net, tax and total exactly as it does for every other
	// document.
	lines := []billing.Line{{
		Description:   description,
		QuantityMilli: int64(req.Quantity) * 1000,
		UnitPrice:     money.New(p.Amount, cur),
	}}
	if p.Setup > 0 {
		lines = append(lines, billing.Line{
			Description:   description + " — setup",
			QuantityMilli: int64(req.Quantity) * 1000,
			UnitPrice:     money.New(p.Setup, cur),
		})
	}

	customer := billing.CustomerDetails{ID: req.CustomerID}
	if err := billing.FillCustomer(ctx, tx, &customer); err != nil {
		if errors.Is(err, billing.ErrInvalidProfile) {
			return Result{}, ErrCustomerNotFound
		}
		return Result{}, err
	}

	period := billing.Period{
		Start: start.UTC().Format(time.RFC3339),
		End:   end.UTC().Format(time.RFC3339),
		Key:   start.UTC().Format("2006-01-02"),
	}
	invoiceID, _, _, err := s.issuer.CreateDraft(ctx, tx, billing.IssueRequest{
		CustomerID:      req.CustomerID,
		OrderID:         orderID,
		SubscriptionID:  subscriptionID,
		PeriodKey:       period.Key,
		Lines:           lines,
		Merchant:        req.Merchant,
		Customer:        customer,
		Period:          &period,
		Terms:           req.Terms,
		DueAt:           req.DueAt,
		Series:          req.Series,
		PriceVersionIDs: []string{p.ID},
	})
	if err != nil {
		return Result{}, err
	}
	issued, err := s.issuer.Issue(ctx, tx, invoiceID, req.Series)
	if err != nil {
		return Result{}, err
	}

	// The order total is the invoice total, tax included: a customer comparing the order
	// to the invoice must see the same number.
	if _, err := tx.Exec(ctx, `
		UPDATE orders SET total_minor = $2, updated_at = now(), version = version + 1
		WHERE id = $1`, orderID, issued.Totals.Total.Minor()); err != nil {
		return Result{}, fmt.Errorf("orders: set order total: %w", err)
	}

	return Result{
		OrderID:        orderID,
		InvoiceID:      issued.InvoiceID,
		InvoiceNumber:  issued.Number,
		SubscriptionID: subscriptionID,
		Status:         "pending_payment",
		Total:          issued.Totals.Total,
	}, nil
}

// querier is the read surface that loading a price needs.
type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// loadPrice reads the immutable price version and the product it belongs to.
func loadPrice(ctx context.Context, q querier, id string) (price, error) {
	var p price
	err := q.QueryRow(ctx, `
		SELECT pv.id, pv.product_id, pr.name, pv.name, pv.currency, pv.amount_minor,
		       pv.setup_minor, pv.interval, pv.anchor_day, pv.anchor_timezone
		FROM price_versions pv
		JOIN products pr ON pr.id = pv.product_id
		WHERE pv.id = $1`, id).
		Scan(&p.ID, &p.ProductID, &p.Product, &p.Name, &p.Currency, &p.Amount,
			&p.Setup, &p.Interval, &p.AnchorDay, &p.Timezone)
	if errors.Is(err, pgx.ErrNoRows) {
		return price{}, ErrPriceNotFound
	}
	if err != nil {
		return price{}, fmt.Errorf("orders: load price: %w", err)
	}
	return p, nil
}

// description is the line text on the order and the invoice.
func (p price) description() string {
	if p.Name != "" {
		return p.Product + " — " + p.Name
	}
	return p.Product
}

// periodFor derives the first billing period from the price version's own schedule.
// Deriving it from the anchor rather than adding a fixed number of days is what makes
// month-end and leap-day behaviour correct.
func periodFor(p price, now time.Time) (time.Time, time.Time, error) {
	interval, err := calendar.ParseInterval(p.Interval)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("%w: %v", ErrInvalidSchedule, err)
	}
	loc, err := time.LoadLocation(p.Timezone)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("%w: unknown timezone %q", ErrInvalidSchedule, p.Timezone)
	}
	anchor, err := calendar.NewAnchor(p.AnchorDay, 0, 0)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("%w: %v", ErrInvalidSchedule, err)
	}
	schedule, err := calendar.NewSchedule(interval, anchor, loc)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("%w: %v", ErrInvalidSchedule, err)
	}
	period := schedule.PeriodAt(now)
	return period.Start, period.End, nil
}
