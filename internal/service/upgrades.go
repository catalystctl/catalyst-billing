package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/catalyst/billing/internal/billing"
	"github.com/catalyst/billing/internal/credit"
	"github.com/catalyst/billing/internal/money"
	"github.com/jackc/pgx/v5"
)

// Upgrades and downgrades (UPG-01…UPG-06, CAT-09, SVC-10).
//
// The economic shape of a mid-cycle change is one question: what is the difference between
// what the customer is on and what they are moving to, for the part of the period they have
// not consumed yet. Everything else — the invoice, the credit, the server call, the next
// renewal — follows from that number.
//
// So the arithmetic is a first-class value here rather than a step inside a handler:
//
//	Proration{Ratio: remaining/period, ...}  x  (to - from)  ->  one rounded Amount
//
// The ratio is a big.Rat, the subtraction is exact minor units, and the rounding happens
// exactly once with a mode named at the call site. There is no float, and there is no
// "30 days per month": the period length comes from internal/calendar, so a period that
// spans a month end or a daylight-saving change is measured by its real length.
//
// An upgrade issues an invoice and the service keeps its old price until that invoice is
// paid (`ApplyPendingUpgrades`). A downgrade is a credit; the credit document is issued by
// the credits work package, so the row is recorded as pending with the amount owed and the
// integrator posts the credit from it. Recording the obligation is the part that must not be
// lost, and it is not.

// Proration is the exact fraction of a billing period that remains.
//
// Remaining and Total are elapsed-time durations, and their ratio is exact. Using durations
// rather than day counts is what makes the answer the same across a DST transition and a
// 28-day February: the customer paid for an interval, and what is left of it is a fraction of
// that interval.
type Proration struct {
	PeriodStart time.Time
	PeriodEnd   time.Time
	// AsOf is the instant the change takes effect. It is clamped into the period: a change
	// requested before the period starts is worth the whole period, and one requested after
	// it ends is worth nothing.
	AsOf      time.Time
	Remaining time.Duration
	Total     time.Duration
	Ratio     money.Ratio
}

// ComputeProration returns the fraction of [start, end) that remains at asOf.
//
// A zero-length period is an error rather than a division by zero: a subscription with
// start == end is a corrupt row, and silently returning 0 would under-charge by the whole
// period.
func ComputeProration(start, end, asOf time.Time) (Proration, error) {
	if !end.After(start) {
		return Proration{}, fmt.Errorf("%w: the billing period %s..%s is empty",
			ErrInvalidInput, start, end)
	}
	total := end.Sub(start)
	remaining := end.Sub(asOf)
	if remaining > total {
		remaining = total
	}
	if remaining < 0 {
		remaining = 0
	}
	ratio, err := money.NewRatio(int64(remaining), int64(total))
	if err != nil {
		return Proration{}, fmt.Errorf("service: proration ratio: %w", err)
	}
	return Proration{
		PeriodStart: start,
		PeriodEnd:   end,
		AsOf:        asOf,
		Remaining:   remaining,
		Total:       total,
		Ratio:       ratio,
	}, nil
}

// ProratedDifference applies the proration to the difference between two prices.
//
// mode is explicit at every call site, which is what makes a re-computation reproducible.
// The default here is RoundHalfUp: a half minor unit rounds away from zero, so an upgrade
// charge rounds up and a downgrade credit also rounds up (in the customer's favour).
func ProratedDifference(from, to money.Amount, p Proration, mode money.Rounding) (money.Amount, error) {
	delta, err := to.Sub(from)
	if err != nil {
		return money.Amount{}, err
	}
	return money.ApplyRate(delta, p.Ratio, mode)
}

// UpgradePrice is one target price a service can move to.
type UpgradePrice struct {
	PriceVersionID string
	Version        int
	Name           string
	Amount         money.Amount
	Setup          money.Amount
	Interval       string
	// Prorated is what the customer would be charged (positive) or credited (negative)
	// for the rest of the current period, if they chose this price now.
	Prorated money.Amount
}

// UpgradeOption is one product reachable from a service's product.
type UpgradeOption struct {
	ProductID        string
	ProductName      string
	ProductSlug      string
	ShortDescription string
	Prices           []UpgradePrice
}

// UpgradeOptions lists the products reachable from a service's product, with their prices in
// the service's currency.
//
// Only prices in the subscription's currency are offered: this system deliberately has no
// exchange rate, so a price in another currency is not a price the customer can move to. A
// product with no price in that currency is omitted rather than shown as unavailable, because
// an option the customer cannot choose is noise.
func (s *Store) UpgradeOptions(ctx context.Context, serviceID string) ([]UpgradeOption, error) {
	var (
		productID string
		subID     string
	)
	if err := s.pool.QueryRow(ctx, `
		SELECT product_id, COALESCE(subscription_id::text, '')
		FROM services WHERE id = $1`, serviceID).Scan(&productID, &subID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("service: load service: %w", err)
	}
	if subID == "" {
		return nil, fmt.Errorf("%w: this service has no billing schedule to upgrade", ErrNotUpgradable)
	}
	sub, err := scanSubscription(s.pool.QueryRow(ctx, `
		SELECT `+subscriptionColumns+` FROM subscriptions WHERE id = $1`, subID))
	if err != nil {
		return nil, err
	}

	current, err := loadPriceVersion(ctx, s.pool, sub.PriceVersionID)
	if err != nil {
		return nil, err
	}
	currency := current.Currency

	now := s.clock()
	start, end := billingPeriod(sub, now)
	proration, err := ComputeProration(start, end, now)
	if err != nil {
		return nil, err
	}
	cur, err := currencyOf(currency)
	if err != nil {
		return nil, err
	}
	fromAmount := money.New(current.AmountMinor, cur)

	rows, err := s.pool.Query(ctx, `
		SELECT p.id, p.name, p.slug, p.short_description,
		       pv.id, pv.version, pv.name, pv.amount_minor, pv.setup_minor, pv.interval
		FROM product_upgrades pu
		JOIN products p ON p.id = pu.upgrade_to_id
		JOIN price_versions pv ON pv.product_id = p.id
		WHERE pu.product_id = $1
		  AND pv.currency = $2
		  AND pv.interval = $3
		  AND p.hidden = false
		ORDER BY p.sort_order, p.name, pv.version DESC`, productID, currency, current.Interval)
	if err != nil {
		return nil, fmt.Errorf("service: list upgrade options: %w", err)
	}
	defer rows.Close()

	var out []UpgradeOption
	index := map[string]int{}
	for rows.Next() {
		var (
			opt       UpgradeOption
			priceID   string
			version   int
			name      string
			amount    int64
			setup     int64
			interval  string
			shortDesc string
		)
		if err := rows.Scan(&opt.ProductID, &opt.ProductName, &opt.ProductSlug, &shortDesc,
			&priceID, &version, &name, &amount, &setup, &interval); err != nil {
			return nil, fmt.Errorf("service: scan upgrade option: %w", err)
		}
		opt.ShortDescription = shortDesc

		prorated, err := ProratedDifference(fromAmount, money.New(amount, cur), proration, money.RoundHalfUp)
		if err != nil {
			return nil, err
		}

		idx, ok := index[opt.ProductID]
		if !ok {
			out = append(out, opt)
			idx = len(out) - 1
			index[opt.ProductID] = idx
		}
		out[idx].Prices = append(out[idx].Prices, UpgradePrice{
			PriceVersionID: priceID,
			Version:        version,
			Name:           name,
			Amount:         money.New(amount, cur),
			Setup:          money.New(setup, cur),
			Interval:       interval,
			Prorated:       prorated,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("service: read upgrade options: %w", err)
	}
	return out, nil
}

// UpgradeRequest asks to move a service to a different price version.
type UpgradeRequest struct {
	ServiceID string
	// PriceVersionID is the target. The product it belongs to must be reachable from the
	// service's product through product_upgrades.
	PriceVersionID string
	// Selection is the config-option selection on the *target* plan, keyed by option id. A nil
	// map means "this change does not touch the options": the stored selection is left alone and
	// the prorated difference covers the base prices only, which is the behaviour every existing
	// caller has. A non-nil map is the whole target selection, priced on the target version and
	// applied with the plan — the two halves of one price cannot move a period apart.
	Selection map[string]string
	// UserID is the actor. On the customer path it is also the scope.
	UserID string
	Reason string
	// Staff bypasses the customer scoping and the reachability check's "is this an upgrade
	// in the customer's favour" question: staff may move a service to any price version of a
	// reachable product, including a more expensive one the customer did not ask for.
	Staff bool
}

// UpgradeResult is what an upgrade or downgrade did.
type UpgradeResult struct {
	UpgradeID          string
	ServiceID          string
	FromPriceVersionID string
	ToPriceVersionID   string
	// Direction is "upgrade", "downgrade" or "none" (the same price).
	Direction string
	Currency  string
	// ProratedMinor is the signed prorated difference: positive is a charge, negative a
	// credit.
	ProratedMinor int64
	// InvoiceID and InvoiceNumber are set for an upgrade.
	InvoiceID     string
	InvoiceNumber string
	// CreditMinor is the credit the customer is owed for a downgrade, and CreditPending says
	// the credit has not been posted yet. The integrator posts it from the service_upgrades
	// row; until then the row stays pending and the service keeps its current price.
	CreditMinor   int64
	CreditPending bool
	Status        string
	Proration     Proration
}

// Upgrade moves a service to a target price version, issuing the prorated invoice (an
// upgrade) or recording the prorated credit (a downgrade).
func (s *Store) Upgrade(ctx context.Context, req UpgradeRequest) (UpgradeResult, error) {
	if strings.TrimSpace(req.ServiceID) == "" || strings.TrimSpace(req.PriceVersionID) == "" {
		return UpgradeResult{}, fmt.Errorf("%w: a service and a target price are required", ErrInvalidInput)
	}
	return s.upgrade(ctx, req)
}

// UpgradeAsStaff moves any service, on a customer's behalf.
func (s *Store) UpgradeAsStaff(ctx context.Context, req UpgradeRequest) (UpgradeResult, error) {
	if strings.TrimSpace(req.ServiceID) == "" || strings.TrimSpace(req.PriceVersionID) == "" {
		return UpgradeResult{}, fmt.Errorf("%w: a service and a target price are required", ErrInvalidInput)
	}
	req.Staff = true
	return s.upgrade(ctx, req)
}

func (s *Store) upgrade(ctx context.Context, req UpgradeRequest) (UpgradeResult, error) {
	var out UpgradeResult
	now := s.clock()

	err := s.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var (
			productID  string
			subID      string
			customerID string
			status     string
		)
		var err error
		if req.Staff {
			err = tx.QueryRow(ctx, `
				SELECT product_id, COALESCE(subscription_id::text, ''), customer_id, status
				FROM services WHERE id = $1 FOR UPDATE`, req.ServiceID).
				Scan(&productID, &subID, &customerID, &status)
		} else {
			err = tx.QueryRow(ctx, `
				SELECT s.product_id, COALESCE(s.subscription_id::text, ''), s.customer_id, s.status
				FROM services s
				JOIN customer_accounts ca ON ca.customer_id = s.customer_id
				WHERE s.id = $1 AND ca.user_id = $2
				FOR UPDATE OF s`, req.ServiceID, req.UserID).
				Scan(&productID, &subID, &customerID, &status)
		}
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("service: lock service: %w", err)
		}
		if status == "terminated" || status == "terminating" {
			return fmt.Errorf("%w: a %s service cannot be upgraded", ErrNotUpgradable, status)
		}
		if subID == "" {
			return fmt.Errorf("%w: this service has no billing schedule", ErrNotUpgradable)
		}

		sub, err := scanSubscription(tx.QueryRow(ctx, `
			SELECT `+subscriptionColumns+` FROM subscriptions WHERE id = $1 FOR UPDATE`, subID))
		if err != nil {
			return err
		}
		if sub.Status != "active" && sub.Status != "past_due" {
			return fmt.Errorf("%w: the subscription is %s", ErrNotUpgradable, sub.Status)
		}

		target, err := loadPriceVersion(ctx, tx, req.PriceVersionID)
		if err != nil {
			return err
		}
		current, err := loadPriceVersion(ctx, tx, sub.PriceVersionID)
		if err != nil {
			return err
		}

		// Reachability. A staff member may also move within the graph's reverse direction,
		// but never to an unrelated product: the graph is the promise the storefront made
		// about which products are comparable.
		if !req.Staff || target.ProductID != productID {
			if err := reachable(ctx, tx, productID, target.ProductID); err != nil {
				return err
			}
		}
		if target.Currency != current.Currency {
			return fmt.Errorf("%w: %s to %s", ErrCurrencyMismatch, current.Currency, target.Currency)
		}
		if target.Interval != current.Interval {
			return fmt.Errorf("%w: %s to %s", ErrIntervalMismatch, current.Interval, target.Interval)
		}

		cur, err := currencyOf(current.Currency)
		if err != nil {
			return err
		}

		start, end := billingPeriod(sub, now)
		proration, err := ComputeProration(start, end, now)
		if err != nil {
			return err
		}

		// A plan change can carry the target plan's config options (UPG-02). They are part of the
		// same prorated difference because they are part of the same price: the renewal engine
		// bills the base price *plus* one line per chosen option, so a change that moved the plan
		// and left the options to a second request would charge the wrong number for the rest of
		// this period and for every period after it. The arithmetic is unchanged — one exact
		// ratio, one RoundHalfUp — it is only fed the whole price on each side.
		fromAmount := money.New(current.AmountMinor, cur)
		toAmount := money.New(target.AmountMinor, cur)
		var resolved ResolvedSelection
		if req.Selection != nil {
			stored, err := s.storedSelectionTotal(ctx, tx, req.ServiceID, current, cur)
			if err != nil {
				return err
			}
			if fromAmount, err = fromAmount.Add(stored); err != nil {
				return err
			}
			resolved, err = s.ResolveSelectionTx(ctx, tx, target.ProductID, target.ID, req.Selection)
			if err != nil {
				return err
			}
			if toAmount, err = toAmount.Add(resolved.Total); err != nil {
				return err
			}
		}
		prorated, err := ProratedDifference(fromAmount, toAmount, proration, money.RoundHalfUp)
		if err != nil {
			return err
		}

		out = UpgradeResult{
			ServiceID:          req.ServiceID,
			FromPriceVersionID: current.ID,
			ToPriceVersionID:   target.ID,
			Currency:           current.Currency,
			ProratedMinor:      prorated.Minor(),
			Proration:          proration,
			Status:             UpgradePending,
		}
		switch {
		case prorated.IsPositive():
			out.Direction = "upgrade"
		case prorated.IsNegative():
			out.Direction = "downgrade"
		default:
			out.Direction = "none"
		}

		var invoiceID any
		var number string
		if prorated.IsPositive() {
			if s.Issuer == nil {
				return fmt.Errorf("%w: invoice issuance is not configured", ErrInvalidInput)
			}
			id, num, err := s.issueProratedInvoice(ctx, tx, proratedInvoice{
				CustomerID:     customerID,
				SubscriptionID: sub.ID,
				PriceVersionID: target.ID,
				Amount:         prorated,
				Description: fmt.Sprintf("Prorated upgrade to %s (%s of %s days remaining)",
					priceLabel(target), days(proration.Remaining), days(proration.Total)),
			})
			if err != nil {
				return err
			}
			invoiceID, number = id, num
			out.InvoiceID, out.InvoiceNumber = id, num
			// `credits.auto_use` promises that a balance covering an invoice settles it without the
			// customer asking (CRD-03). The setting used to be honoured only on the checkout path,
			// so a customer with credit on account who upgraded was invoiced in full and the
			// balance sat untouched. It runs on the invoice's own id, so a replayed upgrade
			// applies once, and in this transaction, so a rolled-back upgrade spends nothing.
			if _, err := credit.ApplyCreditToInvoice(ctx, tx, id); err != nil {
				return err
			}
		} else if prorated.IsNegative() {
			// The credit is posted by the credits work package from this row. The obligation
			// is recorded here so it cannot be lost between the two packages.
			out.CreditMinor = -prorated.Minor()
			out.CreditPending = true
		}

		var upgradeID string
		if err := tx.QueryRow(ctx, `
			INSERT INTO service_upgrades
				(service_id, from_price_version_id, to_price_version_id, prorated_minor,
				 currency, invoice_id, status)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
			RETURNING id`,
			req.ServiceID, current.ID, target.ID, prorated.Minor(), current.Currency,
			invoiceID, UpgradePending).Scan(&upgradeID); err != nil {
			return fmt.Errorf("service: record upgrade: %w", err)
		}
		out.UpgradeID = upgradeID

		// The chosen options wait here until the change is applied. They cannot go straight into
		// `service_configs`: that table is what the *current* subscription renews from, and this
		// change is still pending payment, so writing them now would price the target plan's
		// options against the old price version on the next renewal.
		optionIDs := make([]string, 0, len(resolved.Options))
		if req.Selection != nil {
			for _, contribution := range resolved.Options {
				if _, err := tx.Exec(ctx, `
					INSERT INTO service_upgrade_configs
						(upgrade_id, config_option_id, value, amount_minor, setup_minor)
					VALUES ($1, $2, $3, $4, $5)
					ON CONFLICT (upgrade_id, config_option_id) DO UPDATE
					SET value = EXCLUDED.value,
					    amount_minor = EXCLUDED.amount_minor,
					    setup_minor = EXCLUDED.setup_minor`,
					upgradeID, contribution.ConfigOptionID, contribution.Value,
					contribution.Amount.Minor(), contribution.Setup.Minor()); err != nil {
					return fmt.Errorf("service: record chosen option: %w", err)
				}
				optionIDs = append(optionIDs, contribution.ConfigOptionID)
			}
		}

		return s.audit(ctx, tx, "service.upgrade", "service", req.ServiceID, req.UserID,
			map[string]any{
				"upgrade_id":         upgradeID,
				"from_price_version": current.ID,
				"to_price_version":   target.ID,
				"prorated_minor":     prorated.Minor(),
				"currency":           current.Currency,
				"invoice_id":         out.InvoiceID,
				"direction":          out.Direction,
				"reason":             req.Reason,
				"number":             number,
				"from_minor":         fromAmount.Minor(),
				"to_minor":           toAmount.Minor(),
				"config_options":     optionIDs,
			})
	})
	if err != nil {
		return UpgradeResult{}, err
	}
	return out, nil
}

// reachable reports whether toProduct is in the upgrade graph from fromProduct.
//
// The graph is directed as written and a downgrade traverses the same edge in reverse, so a
// pair is reachable if an edge exists in either direction. That is exactly the model the
// migration describes: one table, not two.
func reachable(ctx context.Context, q Querier, fromProduct, toProduct string) error {
	if fromProduct == toProduct {
		return nil
	}
	var ok bool
	err := q.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM product_upgrades
			WHERE (product_id = $1 AND upgrade_to_id = $2)
			   OR (product_id = $2 AND upgrade_to_id = $1))`,
		fromProduct, toProduct).Scan(&ok)
	if err != nil {
		return fmt.Errorf("service: read upgrade graph: %w", err)
	}
	if !ok {
		return fmt.Errorf("%w: %s is not reachable from %s", ErrNotUpgradable, toProduct, fromProduct)
	}
	return nil
}

// proratedInvoice is the input to the upgrade invoice.
type proratedInvoice struct {
	CustomerID     string
	SubscriptionID string
	PriceVersionID string
	Amount         money.Amount
	Description    string
}

// issueProratedInvoice creates and issues a one-line invoice for the prorated amount.
//
// The line names no product: a prorated difference provisions nothing, and marking it as a
// product sale would make fulfilment try to create a second service for it.
func (s *Store) issueProratedInvoice(ctx context.Context, tx pgx.Tx, in proratedInvoice) (string, string, error) {
	now := s.clock()
	details := billing.CustomerDetails{ID: in.CustomerID}
	if err := fillCustomer(ctx, tx, &details); err != nil {
		return "", "", err
	}

	id, _, _, err := s.Issuer.CreateDraft(ctx, tx, billing.IssueRequest{
		CustomerID:     in.CustomerID,
		SubscriptionID: in.SubscriptionID,
		Lines: []billing.Line{{
			Description:   in.Description,
			QuantityMilli: 1000,
			UnitPrice:     in.Amount,
		}},
		Merchant:        s.Merchant,
		Customer:        details,
		Terms:           s.Terms,
		DueAt:           now.AddDate(0, 0, s.dueDays()),
		Series:          s.Series,
		PriceVersionIDs: []string{in.PriceVersionID},
	})
	if err != nil {
		return "", "", fmt.Errorf("service: create upgrade invoice: %w", err)
	}
	issued, err := s.Issuer.Issue(ctx, tx, id, s.Series)
	if err != nil {
		return "", "", fmt.Errorf("service: issue upgrade invoice: %w", err)
	}
	return issued.InvoiceID, issued.Number, nil
}

// fillCustomer reads the billed party for the invoice snapshot.
func fillCustomer(ctx context.Context, tx pgx.Tx, c *billing.CustomerDetails) error {
	var name, email string
	err := tx.QueryRow(ctx, `SELECT name, email FROM customers WHERE id = $1`, c.ID).
		Scan(&name, &email)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("service: load customer: %w", err)
	}
	c.Name, c.Email = name, email
	return nil
}

// PendingUpgrade is one upgrade waiting to be applied.
type PendingUpgrade struct {
	ID                 string
	ServiceID          string
	CustomerID         string
	FromPriceVersionID string
	ToPriceVersionID   string
	ProratedMinor      int64
	Currency           string
	InvoiceID          string
	InvoiceStatus      string
	Status             string
	CreatedAt          time.Time
}

// PendingUpgrades lists the upgrades the worker can act on.
//
// When paidOnly is true it lists the ones whose invoice has been paid and which are therefore
// ready to apply; when false it lists every pending row, including the downgrades whose credit
// has not been posted yet.
func (s *Store) PendingUpgrades(ctx context.Context, paidOnly bool, limit int) ([]PendingUpgrade, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx, `
		SELECT u.id, u.service_id, s.customer_id,
		       COALESCE(u.from_price_version_id::text, ''), u.to_price_version_id,
		       u.prorated_minor, u.currency, COALESCE(u.invoice_id::text, ''),
		       COALESCE(i.status, ''), u.status, u.created_at
		FROM service_upgrades u
		JOIN services s ON s.id = u.service_id
		LEFT JOIN invoices i ON i.id = u.invoice_id
		WHERE u.status = 'pending'
		  AND (NOT $1 OR (u.invoice_id IS NOT NULL AND i.status = 'paid'))
		ORDER BY u.created_at
		LIMIT $2`, paidOnly, limit)
	if err != nil {
		return nil, fmt.Errorf("service: list pending upgrades: %w", err)
	}
	defer rows.Close()

	out := []PendingUpgrade{}
	for rows.Next() {
		var u PendingUpgrade
		if err := rows.Scan(&u.ID, &u.ServiceID, &u.CustomerID, &u.FromPriceVersionID,
			&u.ToPriceVersionID, &u.ProratedMinor, &u.Currency, &u.InvoiceID,
			&u.InvoiceStatus, &u.Status, &u.CreatedAt); err != nil {
			return nil, fmt.Errorf("service: scan pending upgrade: %w", err)
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// Upgrade returns one upgrade row.
func (s *Store) UpgradeRow(ctx context.Context, id string) (PendingUpgrade, error) {
	var u PendingUpgrade
	err := s.pool.QueryRow(ctx, `
		SELECT u.id, u.service_id, s.customer_id,
		       COALESCE(u.from_price_version_id::text, ''), u.to_price_version_id,
		       u.prorated_minor, u.currency, COALESCE(u.invoice_id::text, ''),
		       COALESCE(i.status, ''), u.status, u.created_at
		FROM service_upgrades u
		JOIN services s ON s.id = u.service_id
		LEFT JOIN invoices i ON i.id = u.invoice_id
		WHERE u.id = $1`, id).
		Scan(&u.ID, &u.ServiceID, &u.CustomerID, &u.FromPriceVersionID, &u.ToPriceVersionID,
			&u.ProratedMinor, &u.Currency, &u.InvoiceID, &u.InvoiceStatus, &u.Status, &u.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return PendingUpgrade{}, ErrNotFound
	}
	if err != nil {
		return PendingUpgrade{}, fmt.Errorf("service: load upgrade: %w", err)
	}
	return u, nil
}

// ApplyPendingUpgrades applies every upgrade whose invoice has been paid.
//
// This is the second half of UPG-05: the invoice moves the money, and this moves the service.
// Splitting them is what makes a half-applied upgrade visible — the row stays pending until
// the server has been told, and `PendingUpgrades` is the queue an operator can look at.
//
// Wire it into cmd/billing/jobs.go as a jobSpec:
//
//	svc.ApplyPendingUpgrades(ctx)
//
// It is idempotent: the row is marked applied in the same transaction that moves the
// subscription, and the provisioning operation key is derived from the service, the target
// price version and the generation, so a second run enqueues nothing.
func (s *Store) ApplyPendingUpgrades(ctx context.Context) ([]string, error) {
	pending, err := s.PendingUpgrades(ctx, true, 200)
	if err != nil {
		return nil, err
	}
	var applied []string
	for _, u := range pending {
		if err := s.ApplyUpgrade(ctx, u.ID); err != nil {
			// One bad row must not wedge the batch; the next tick retries it and the row is
			// still visible as pending.
			continue
		}
		applied = append(applied, u.ID)
	}
	return applied, nil
}

// ApplyUpgrade applies one upgrade row: the subscription takes the new price and the
// provisioner is asked to resize.
//
// It refuses a row that is not pending, and it refuses an unpaid invoice, so a caller cannot
// hand a service a new price before the customer has paid for it.
//
// A *downgrade* row carries no invoice — its money is a credit, not a charge — so applying one
// here is refused: the customer would get the cheaper price without being credited. The credits
// work package calls ApplyDowngrade once it has posted the credit.
func (s *Store) ApplyUpgrade(ctx context.Context, upgradeID string) error {
	return s.apply(ctx, upgradeID, false)
}

// ApplyDowngrade applies a downgrade row after its credit has been posted.
//
// It is the integrator's hook: once the credits work package has credited the customer the
// amount on the row, calling this moves the service to the cheaper price and enqueues the
// resize. Until then the row stays pending, so an operator can see that the customer is still
// owed something.
func (s *Store) ApplyDowngrade(ctx context.Context, upgradeID string) error {
	return s.apply(ctx, upgradeID, true)
}

func (s *Store) apply(ctx context.Context, upgradeID string, allowCredit bool) error {
	now := s.clock()

	return s.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var (
			serviceID   string
			toPrice     string
			status      string
			invoiceID   *string
			invoicePaid bool
			prorated    int64
		)
		err := tx.QueryRow(ctx, `
			SELECT u.service_id, u.to_price_version_id, u.status, u.invoice_id,
			       COALESCE((SELECT status = 'paid' FROM invoices WHERE id = u.invoice_id), false),
			       u.prorated_minor
			FROM service_upgrades u WHERE u.id = $1
			FOR UPDATE OF u`, upgradeID).
			Scan(&serviceID, &toPrice, &status, &invoiceID, &invoicePaid, &prorated)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("service: lock upgrade: %w", err)
		}
		if status != UpgradePending {
			return fmt.Errorf("%w: its status is %s", ErrNotPending, status)
		}
		if invoiceID != nil && !invoicePaid {
			return fmt.Errorf("%w: the upgrade invoice is not paid", ErrNoInvoice)
		}
		if invoiceID == nil && prorated < 0 && !allowCredit {
			return fmt.Errorf("%w: a downgrade is applied once its credit is posted; "+
				"the credits package calls ApplyDowngrade", ErrCreditNotPosted)
		}

		var (
			subID      string
			generation int64
			externalID string
		)
		if err := tx.QueryRow(ctx, `
			SELECT COALESCE(subscription_id::text, ''), generation, COALESCE(external_id, '')
			FROM services WHERE id = $1 FOR UPDATE`, serviceID).
			Scan(&subID, &generation, &externalID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return fmt.Errorf("service: lock service: %w", err)
		}
		if subID == "" {
			return fmt.Errorf("%w: this service has no billing schedule", ErrNotUpgradable)
		}

		// The subscription takes the new price. The next renewal reads price_version_id, so
		// this single column is what makes the *next* invoice the new amount.
		tag, err := tx.Exec(ctx, `
			UPDATE subscriptions
			SET price_version_id = $2, version = version + 1
			WHERE id = $1`, subID, toPrice)
		if err != nil {
			return fmt.Errorf("service: reprice subscription: %w", err)
		}
		if tag.RowsAffected() != 1 {
			return ErrNotFound
		}

		// The options move with the price, in the same commit. `service_configs` is what the next
		// renewal prices from, so a plan that changed here and a selection that changed a request
		// later would bill one period at the new base price with the old options — the same class
		// of defect as a paid option that never recurs.
		if err := s.applyPendingSelection(ctx, tx, upgradeID, serviceID, toPrice); err != nil {
			return err
		}

		// The provisioner is told, but only when there is something to tell it about. A
		// service that was never provisioned has no remote resource to resize.
		if externalID != "" {
			if _, err := enqueueOperation(ctx, tx, serviceID, "resize",
				fmt.Sprintf("resize:%s:%s:%d", serviceID, toPrice, generation), generation); err != nil {
				return err
			}
		}

		if _, err := tx.Exec(ctx, `
			UPDATE service_upgrades SET status = 'applied', applied_at = $2 WHERE id = $1`,
			upgradeID, now); err != nil {
			return fmt.Errorf("service: mark upgrade applied: %w", err)
		}

		return s.audit(ctx, tx, "service.upgrade_applied", "service", serviceID, "",
			map[string]any{
				"upgrade_id":       upgradeID,
				"price_version_id": toPrice,
				"resized":          externalID != "",
			})
	})
}

// StaleUpgradeAfter is how long a pending upgrade may sit unpaid before the cron cancels it.
const StaleUpgradeAfter = 14 * 24 * time.Hour

// CancelStaleUpgrades cancels pending upgrades whose invoice was never paid (OPS-03 / UPG-06).
//
// An upgrade invoice that nobody pays leaves the customer on the old price with a live
// document that says otherwise, and leaves the row pending forever. The cron voids the
// invoice and cancels the row, so the state agrees with itself again.
//
// A paid invoice is never touched: `ApplyPendingUpgrades` owns that case, and voiding a paid
// document is forbidden by the immutability trigger anyway.
//
// Wire it into cmd/billing/jobs.go as a jobSpec:
//
//	svc.CancelStaleUpgrades(ctx)
func (s *Store) CancelStaleUpgrades(ctx context.Context) ([]string, error) {
	cutoff := s.clock().Add(-StaleUpgradeAfter)

	rows, err := s.pool.Query(ctx, `
		SELECT u.id, COALESCE(u.invoice_id::text, ''), COALESCE(i.status, '')
		FROM service_upgrades u
		LEFT JOIN invoices i ON i.id = u.invoice_id
		WHERE u.status = 'pending'
		  AND u.created_at < $1
		  AND (u.invoice_id IS NULL OR i.status IN ('open', 'draft'))
		ORDER BY u.created_at
		LIMIT 200`, cutoff)
	if err != nil {
		return nil, fmt.Errorf("service: list stale upgrades: %w", err)
	}
	type stale struct {
		id, invoiceID, invoiceStatus string
	}
	var candidates []stale
	for rows.Next() {
		var c stale
		if err := rows.Scan(&c.id, &c.invoiceID, &c.invoiceStatus); err != nil {
			rows.Close()
			return nil, fmt.Errorf("service: scan stale upgrade: %w", err)
		}
		candidates = append(candidates, c)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("service: list stale upgrades: %w", err)
	}
	rows.Close()

	var cancelled []string
	for _, c := range candidates {
		if err := s.cancelUpgrade(ctx, c.id, c.invoiceID); err != nil {
			continue
		}
		cancelled = append(cancelled, c.id)
	}
	return cancelled, nil
}

// cancelUpgrade voids the unpaid invoice and marks the row cancelled, atomically.
func (s *Store) cancelUpgrade(ctx context.Context, upgradeID, invoiceID string) error {
	return s.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var (
			serviceID string
			status    string
		)
		if err := tx.QueryRow(ctx, `
			SELECT service_id, status FROM service_upgrades WHERE id = $1 FOR UPDATE`, upgradeID).
			Scan(&serviceID, &status); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return fmt.Errorf("service: lock upgrade: %w", err)
		}
		if status != UpgradePending {
			return fmt.Errorf("%w: its status is %s", ErrNotPending, status)
		}

		if invoiceID != "" {
			// The trigger permits open -> void and refuses paid -> void, so a paid invoice
			// cannot be erased by this path even if the guard above were wrong.
			if _, err := tx.Exec(ctx, `
				UPDATE invoices SET status = 'void' WHERE id = $1 AND status = 'open'`,
				invoiceID); err != nil {
				return fmt.Errorf("service: void stale upgrade invoice: %w", err)
			}
		}

		if _, err := tx.Exec(ctx, `
			UPDATE service_upgrades SET status = 'cancelled' WHERE id = $1`, upgradeID); err != nil {
			return fmt.Errorf("service: cancel upgrade: %w", err)
		}

		// The options the customer chose for a plan they are not going to get must not be
		// applied later by anything that walks this row.
		if _, err := tx.Exec(ctx,
			`DELETE FROM service_upgrade_configs WHERE upgrade_id = $1`, upgradeID); err != nil {
			return fmt.Errorf("service: drop the cancelled change's options: %w", err)
		}

		return s.audit(ctx, tx, "service.upgrade_cancelled", "service", serviceID, "",
			map[string]any{"upgrade_id": upgradeID, "invoice_id": invoiceID, "reason": "stale"})
	})
}

// priceLabel is the human name of a price version, for an invoice line.
func priceLabel(p priceVersion) string {
	if strings.TrimSpace(p.Name) != "" {
		return p.Name
	}
	return fmt.Sprintf("v%d", p.Version)
}

// days renders a duration as a day count with one decimal place, without a float.
//
// It exists only for prose on an invoice line ("5.5 of 30 days remaining"), never for
// arithmetic: the proration itself is a rational and is never rounded to a display value.
func days(d time.Duration) string {
	tenths := int64(d) * 10 / int64(24*time.Hour)
	return fmt.Sprintf("%d.%d", tenths/10, tenths%10)
}
