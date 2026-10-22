package customer

// Creating a service for an existing customer, by hand.
//
// The dangerous half of this package, and the decisions that keep it from being a second billing
// path are argued in the migration header (`0046_manual_create.sql`) and in the package comment.
// The short version, because it is what a reader of this file needs first:
//
//   - a recurring price gets a real `subscriptions` row, because that is what the renewal engine
//     scans; the period is derived from the price's own anchor and timezone by internal/calendar,
//     exactly as `orders.periodFor` and `register.periodFor` do;
//   - a comped service gets no subscription at all, which is the existing idiom for "nothing may
//     ever invoice this";
//   - nothing here writes an invoice or an order. Billing for a service is a different action with
//     a different document, and the panel already has one path for it.
//
// Everything that decides whether a product may be sold is called, not re-implemented:
// `service.CheckPurchaseAllowed` for the quantity policy, the per-customer limit and the stock
// check, `service.DecrementStock` for the decrement, and `service.Store.ResolveSelectionTx` for
// validating and pricing the config options. The rows this file writes are the same rows
// `register.activateZeroTotal` writes for a free purchase — the shape is mirrored deliberately and
// narrowly, because a service created by an operator and one created by a checkout must be
// indistinguishable to dunning, the portal, the provisioner and the renewal engine.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/catalyst/billing/internal/audit"
	"github.com/catalyst/billing/internal/calendar"
	"github.com/catalyst/billing/internal/catalog"
	"github.com/catalyst/billing/internal/service"
	"github.com/jackc/pgx/v5"
)

// Errors the HTTP layer maps to distinct codes. Each one is a different thing for the operator to
// do, which is why they are separate values rather than one "cannot create that".
var (
	// ErrCustomerNotFound is returned for a customer id that does not exist. It is a 404, never a
	// 500 from a foreign-key violation.
	ErrCustomerNotFound = errors.New("customer: no such customer")
	// ErrPriceNotFound is returned for a price version that does not exist.
	ErrPriceNotFound = errors.New("customer: no such price version")
	// ErrReasonRequired is returned when the operator did not say why the service was added.
	// A service with no order behind it is exactly what an auditor asks about.
	ErrReasonRequired = errors.New("customer: a manual service needs a reason")
	// ErrInvalidQuantity is returned for a quantity outside 1..1000.
	ErrInvalidQuantity = errors.New("customer: quantity must be between 1 and 1000")
	// ErrOutOfStock is returned when the product cannot supply the quantity.
	ErrOutOfStock = errors.New("customer: the product is out of stock")
	// ErrPerUserLimit is returned when the customer already holds the maximum of this product.
	ErrPerUserLimit = errors.New("customer: the customer already holds the maximum of this product")
	// ErrQuantityNotAllowed is returned when the product sells one per order.
	ErrQuantityNotAllowed = errors.New("customer: this product cannot be given in a quantity")
	// ErrInvalidConfig is returned for a config selection the product does not offer.
	ErrInvalidConfig = errors.New("customer: the selected options are not available for this price")
	// ErrInvalidSchedule is returned when a recurring price cannot be turned into a period.
	ErrInvalidSchedule = errors.New("customer: this price has no valid billing schedule")
)

// maxQuantity matches the order and cart checks, so a typo of "100000" is a validation error
// rather than a hundred thousand services.
const maxQuantity = 1000

// ServiceInput is one hand-made service.
type ServiceInput struct {
	CustomerID     string
	PriceVersionID string
	// Quantity defaults to 1.
	Quantity int
	// Config is the chosen config options as {config_option_id: value}. It is validated and priced
	// from the catalog: an unknown or unavailable choice is refused rather than ignored.
	Config map[string]string
	// Reason is why the service was added. It is required, stored on the service and written into
	// the audit row.
	Reason string
	// Comp asks for a service that nothing will ever invoice: no subscription, so the renewal
	// engine has no row to find.
	Comp bool
	// ActorID is the operator, for the audit row. It comes from the session, never from a body.
	ActorID       string
	CorrelationID string
	IP            string
}

// ServiceResult is what was created.
type ServiceResult struct {
	ServiceID      string
	CustomerID     string
	ProductID      string
	PriceVersionID string
	// SubscriptionID is empty for a comped service and for a one-time or free price: the absence
	// of the row is what stops anything renewing it.
	SubscriptionID string
	// Status is the service's status after creation. It is 'pending' until the provisioning
	// operation succeeds, which is the same handover a purchase goes through.
	Status string
	// Quantity and Options describe what was selected.
	Quantity int
	Options  []service.SelectionContribution
	// Interval, PeriodStart, PeriodEnd and NextBillAt describe the schedule the renewal engine
	// will act on. They are zero for a service with no subscription.
	Interval    string
	PeriodStart time.Time
	PeriodEnd   time.Time
	NextBillAt  time.Time
	// Comped reports that no subscription was created, so nothing will ever invoice this service.
	Comped bool
	// OriginReason is the reason the operator gave, echoed so the client can show what was
	// recorded rather than what it sent.
	OriginReason string
	// CreatedAt is the timestamp the row was written with, read back rather than assumed, so the
	// response describes the record instead of the request.
	CreatedAt time.Time
}

// CreateService adds a service to an existing customer, in the caller's transaction.
//
// The order of the writes is the order of the guards: the customer must exist, the price must
// exist, the product must be sellable to this customer, and only then does anything get written.
// The stock decrement is inside the same transaction as the service insert, so a failure after it
// returns the unit to the shelf.
func (s *Store) CreateService(ctx context.Context, tx pgx.Tx, in ServiceInput) (ServiceResult, error) {
	reason := strings.TrimSpace(in.Reason)
	if reason == "" {
		return ServiceResult{}, ErrReasonRequired
	}
	quantity := in.Quantity
	if quantity == 0 {
		quantity = 1
	}
	if quantity < 1 || quantity > maxQuantity {
		return ServiceResult{}, ErrInvalidQuantity
	}
	if !validID(in.CustomerID) {
		return ServiceResult{}, ErrCustomerNotFound
	}
	if !validID(in.PriceVersionID) {
		return ServiceResult{}, ErrPriceNotFound
	}

	// The customer must exist before anything is written. Checking here turns a foreign-key
	// violation into a 404 the handler can explain, rather than a 500 from the driver.
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT true FROM customers WHERE id = $1`, in.CustomerID).
		Scan(&exists); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ServiceResult{}, ErrCustomerNotFound
		}
		return ServiceResult{}, fmt.Errorf("customer: load customer: %w", err)
	}

	price, err := loadPriceVersion(ctx, tx, in.PriceVersionID)
	if err != nil {
		return ServiceResult{}, err
	}

	// The purchase guards, in the order that reports the more useful refusal first: the
	// per-customer limit is a policy about this customer, the stock is a fact about the product.
	if err := service.CheckPurchaseAllowed(ctx, tx, price.ProductID, in.CustomerID, quantity); err != nil {
		return ServiceResult{}, mapPurchaseError(err)
	}

	// Price the config selection against this price version. It refuses an option the product does
	// not offer, a value the option does not offer, and a missing required option — the same
	// function the customer portal uses to change a selection on a live service.
	resolved, err := s.services().ResolveSelectionTx(ctx, tx, price.ProductID, price.PriceVersionID, in.Config)
	if err != nil {
		if errors.Is(err, service.ErrOptionNotAvailable) {
			return ServiceResult{}, fmt.Errorf("%w: %s", ErrInvalidConfig, optionDetail(err))
		}
		return ServiceResult{}, err
	}

	// Take the stock now, inside this transaction. A manual creation that could put a sold-out
	// product into a customer's hands is a bug an operator finds in production.
	if err := service.DecrementStock(ctx, tx, price.ProductID, quantity); err != nil {
		return ServiceResult{}, mapPurchaseError(err)
	}

	out := ServiceResult{
		CustomerID:     in.CustomerID,
		ProductID:      price.ProductID,
		PriceVersionID: price.PriceVersionID,
		Quantity:       quantity,
		Status:         "pending",
		Options:        resolved.Options,
		OriginReason:   reason,
	}

	// A schedule is only written for a recurring price that is not comped. The two refusals are
	// different statements: a comped service is deliberately never billed, and a one-time or free
	// price is not a schedule at all (0028: the absence of a subscription is what makes "nothing may
	// renew this" true in the database rather than in a code path).
	switch {
	case !catalog.Recurring(price.Interval):
		// Nothing to renew, and not "comped": the price itself says it is charged once (or not at
		// all). The service is written with no subscription below.
	case in.Comp:
		out.Comped = true
	default:
		start, end, err := periodFor(price, s.clock())
		if err != nil {
			return ServiceResult{}, err
		}
		// 'active', not 'pending': the customer is on the plan from now, and the first renewal is
		// the end of this period. A 'pending' subscription would never renew — that status exists
		// for an order waiting for a payment that, here, will never come.
		if err := tx.QueryRow(ctx, `
			INSERT INTO subscriptions (customer_id, price_version_id, status, interval,
				anchor_day, timezone, current_period_start, current_period_end, next_billing_at)
			VALUES ($1, $2, 'active', $3, $4, $5, $6, $7, $7)
			RETURNING id`,
			in.CustomerID, price.PriceVersionID, price.Interval, price.AnchorDay, price.Timezone,
			start, end).Scan(&out.SubscriptionID); err != nil {
			return ServiceResult{}, fmt.Errorf("customer: create subscription: %w", err)
		}
		out.Interval, out.PeriodStart, out.PeriodEnd, out.NextBillAt = price.Interval, start, end, end
	}

	// The adapter's target is captured on the service, so a later catalog edit cannot change what
	// an in-flight provisioning call asks for. This is the same two keys
	// `register.activateZeroTotal` writes.
	config := map[string]any{}
	if price.ProvisionPlan != "" {
		config["plan_id"] = price.ProvisionPlan
	}
	if price.ProvisionRegion != "" {
		config["region"] = price.ProvisionRegion
	}

	if err := tx.QueryRow(ctx, `
		INSERT INTO services (customer_id, subscription_id, product_id, status, generation, config,
			origin, origin_reason, origin_actor)
		VALUES ($1, $2, $3, 'pending', 1, $4, 'manual', $5, $6)
		RETURNING id, created_at`,
		in.CustomerID, nullableID(out.SubscriptionID), price.ProductID, config, reason,
		nullableID(in.ActorID)).Scan(&out.ServiceID, &out.CreatedAt); err != nil {
		return ServiceResult{}, fmt.Errorf("customer: create service: %w", err)
	}

	// The option selection is stored on the service, because the renewal engine prices from it and
	// the provisioner reads it to build the environment. The amount is captured at selection time
	// so a later option-price change does not silently re-price an existing service.
	for _, sel := range resolved.Options {
		if _, err := tx.Exec(ctx, `
			INSERT INTO service_configs (service_id, config_option_id, value, amount_minor, setup_minor)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (service_id, config_option_id) DO NOTHING`,
			out.ServiceID, sel.ConfigOptionID, sel.Value, sel.Amount.Minor(), sel.Setup.Minor()); err != nil {
			return ServiceResult{}, fmt.Errorf("customer: store service config: %w", err)
		}
	}

	// Queue the delivery. The key is derived from the service id, so it is unique by construction
	// and cannot collide with a purchase's `create:<order item id>` key: a replay of this request
	// (the idempotency layer answers it) or a second create operation for the same service finds
	// the existing row rather than provisioning twice.
	if _, err := tx.Exec(ctx, `
		INSERT INTO service_operations (service_id, kind, operation_key, generation, status)
		VALUES ($1, 'create', $2, 1, 'queued')`,
		out.ServiceID, "create:manual:"+out.ServiceID); err != nil {
		return ServiceResult{}, fmt.Errorf("customer: enqueue provisioning: %w", err)
	}

	// The audit row, inside the transaction, naming the operator and the reason. It also records
	// that no invoice was raised: that absence is the decision an auditor is checking, and stating
	// it is cheaper than making them infer it from the absence of an invoice.
	if err := audit.Record(ctx, tx, audit.Event{
		Actor:         audit.Actor{ID: in.ActorID, Kind: audit.ActorUser},
		Action:        "service.create_manual",
		TargetType:    "service",
		TargetID:      out.ServiceID,
		Outcome:       audit.OutcomeSuccess,
		CorrelationID: in.CorrelationID,
		IP:            in.IP,
		Changes: map[string]any{
			"reason":           reason,
			"customer_id":      in.CustomerID,
			"product_id":       price.ProductID,
			"price_version_id": price.PriceVersionID,
			"quantity":         quantity,
			"subscription_id":  out.SubscriptionID,
			"comped":           out.Comped,
			"invoiced":         false,
			"interval":         price.Interval,
			"options":          len(resolved.Options),
			"option_total":     resolved.Total.Minor(),
			"option_setup":     resolved.SetupTotal.Minor(),
		},
	}); err != nil {
		return ServiceResult{}, err
	}
	return out, nil
}

// services returns the service-lifecycle store used to validate and price a config selection.
//
// It is constructed per call rather than held on the Store: `service.New` takes only the pool, and
// the value it returns carries an injectable clock that this package must not override. Nothing
// here uses its auditor, because the audit row for a manual creation is written above with the
// request's actor.
func (s *Store) services() *service.Store { return service.New(s.pool) }

// priceVersion is the immutable catalog row a manual service is built from.
type priceVersion struct {
	PriceVersionID  string
	ProductID       string
	Currency        string
	AmountMinor     int64
	SetupMinor      int64
	Interval        string
	AnchorDay       int
	Timezone        string
	ProvisionPlan   string
	ProvisionRegion string
}

// loadPriceVersion reads the price version and the product it belongs to.
func loadPriceVersion(ctx context.Context, q interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}, id string) (priceVersion, error) {
	var p priceVersion
	err := q.QueryRow(ctx, `
		SELECT pv.id, pv.product_id, pv.currency, pv.amount_minor, pv.setup_minor, pv.interval,
		       pv.anchor_day, pv.anchor_timezone,
		       COALESCE(pv.provision_plan, ''), COALESCE(pv.provision_region, '')
		FROM price_versions pv
		WHERE pv.id = $1`, id).
		Scan(&p.PriceVersionID, &p.ProductID, &p.Currency, &p.AmountMinor, &p.SetupMinor,
			&p.Interval, &p.AnchorDay, &p.Timezone, &p.ProvisionPlan, &p.ProvisionRegion)
	if errors.Is(err, pgx.ErrNoRows) {
		return priceVersion{}, ErrPriceNotFound
	}
	if err != nil {
		return priceVersion{}, fmt.Errorf("customer: load price version: %w", err)
	}
	return p, nil
}

// periodFor derives the first billing period from the price's own schedule.
//
// It is the same derivation `orders.periodFor` and `register.periodFor` perform, and it is here for
// the same reason they are there: the alternative — sharing one of those, which are unexported in
// packages this one does not own — would either widen those packages or duplicate the *rule*.
// Deriving a period from the anchor rather than adding a fixed number of days is what makes
// month-end and leap-day behaviour correct, and that rule lives in internal/calendar, which is what
// is called below.
func periodFor(p priceVersion, now time.Time) (time.Time, time.Time, error) {
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

// mapPurchaseError translates the lifecycle package's refusals into this package's.
//
// The two vocabularies are kept separate so the HTTP layer maps one set of values to one set of
// codes: a `service.ErrOutOfStock` reaching a handler that only knows `customer.ErrOutOfStock`
// would be a 500 for something the operator can fix in one click.
func mapPurchaseError(err error) error {
	switch {
	case errors.Is(err, service.ErrOutOfStock):
		return fmt.Errorf("%w: %s", ErrOutOfStock, err.Error())
	case errors.Is(err, service.ErrPerUserLimit):
		return fmt.Errorf("%w: %s", ErrPerUserLimit, err.Error())
	case errors.Is(err, service.ErrInvalidInput):
		// The only invalid input CheckPurchaseAllowed raises on its own is a product whose
		// `allow_quantity` is disabled being asked for more than one.
		return fmt.Errorf("%w: %s", ErrQuantityNotAllowed, err.Error())
	case errors.Is(err, service.ErrNotFound):
		return ErrPriceNotFound
	default:
		return err
	}
}

// optionDetail strips the wrapped sentinel from a selection error, so the client is shown the
// sentence that names the option rather than "customer: the selected options are not available for
// this price: service: the option selection is not available for this product: 8 GB is required".
func optionDetail(err error) string {
	return strings.TrimPrefix(err.Error(), service.ErrOptionNotAvailable.Error()+": ")
}

// nullableID renders an empty id as SQL NULL.
func nullableID(id string) any {
	if id == "" {
		return nil
	}
	return id
}
