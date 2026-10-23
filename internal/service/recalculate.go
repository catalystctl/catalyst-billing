package service

// "Recalculate price" (SVC-13).
//
// The action answers a question an operator asks when the catalog and a customer's bill appear to
// disagree: *what would this service cost at today's prices, and what would change?*
//
// It is not an upgrade. An upgrade is a priced change the customer (or staff) chooses, with a
// prorated invoice and an upgrade row; a recalculation is adrift repair — the catalog was edited,
// an option price was corrected, and the subscription is still pointed at a price version that no
// longer represents what is being sold. So this recomputes from the catalog and reports the
// difference, and it moves the subscription only when the operator asked it to.
//
// # Why the selection is repriced too
//
// A stored selection carries the amount it contributed *at selection time* (that is what makes a
// later option-price change safe for a live service). Applying a new price version without
// repricing the selection would produce a service billed as "new price + old option amounts",
// which is a number that matches neither the catalog nor what the customer chose.
//
// # Why money is summed as integers
//
// Every amount read here is a whole number of minor units, and adding whole units cannot
// introduce a fraction. There is no multiplication and no division, so there is no rounding to
// get wrong and no float anywhere in the path.

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// PriceSnapshot is one side of a recalculation: the plan, the selection, and their exact sum.
type PriceSnapshot struct {
	PriceVersionID string
	Version        int
	Name           string
	Interval       string
	Currency       string
	// PlanMinor is the plan's own recurring amount.
	PlanMinor int64
	// OptionsMinor is the sum of the stored selection's amounts.
	OptionsMinor int64
	// SetupMinor is the sum of the stored selection's setup fees. It is reported because a
	// recalculation that changed it would charge a customer for setting something up twice.
	SetupMinor int64
	// TotalMinor is PlanMinor + OptionsMinor. It is the number the renewal engine will charge
	// at the next period.
	TotalMinor int64
	// PlanID and Region are the provisioner target, for the caller's benefit.
	PlanID string
	Region string
}

// PriceRecalculation is what a recalculation found.
type PriceRecalculation struct {
	ServiceID      string
	SubscriptionID string
	From           PriceSnapshot
	To             PriceSnapshot
	// Changed is true when the subscription was moved to another price version or a stored
	// option amount moved. A no-op recalculation is a successful request that says so.
	Changed bool
	// RepricedOptions is how many stored option amounts were rewritten.
	RepricedOptions int
	// OperationID is the resize queued because the plan changed, empty when none was.
	OperationID string
	// PlanChanged reports whether the provisioner target itself moved, which is the fact that
	// makes a resize necessary rather than merely a reprice.
	PlanChanged bool
}

// RecalculatePrice recomputes a service's price from the catalog and applies the difference.
//
// `apply` is the operator's decision, not a default: with apply=false the caller gets the
// comparison and nothing in the database changes. A staff screen can therefore show the
// consequence before anyone presses the consequential button.
func (s *Store) RecalculatePrice(ctx context.Context, serviceID, actorID string,
	apply bool) (PriceRecalculation, error) {
	var out PriceRecalculation

	err := s.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var (
			productID  string
			subID      string
			status     string
			externalID string
			generation int64
		)
		err := tx.QueryRow(ctx, `
			SELECT product_id, COALESCE(subscription_id::text, ''), status,
			       COALESCE(external_id, ''), generation
			FROM services WHERE id = $1 FOR UPDATE`, serviceID).
			Scan(&productID, &subID, &status, &externalID, &generation)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("service: lock service: %w", err)
		}
		if subID == "" {
			return fmt.Errorf("%w: this service has no billing schedule to recalculate", ErrNotUpgradable)
		}

		sub, err := scanSubscription(tx.QueryRow(ctx, `
			SELECT `+subscriptionColumns+` FROM subscriptions WHERE id = $1 FOR UPDATE`, subID))
		if err != nil {
			return err
		}
		current, err := loadPriceVersion(ctx, tx, sub.PriceVersionID)
		if err != nil {
			return err
		}

		// The catalog's current price is its newest version for the same interval and currency.
		// Restricting both is what stops a recalculation from silently moving a monthly service
		// to an annual price, which would be a different product rather than a corrected one.
		target, err := currentPriceVersion(ctx, tx, productID, current.Interval, current.Currency)
		if err != nil {
			return err
		}

		from, err := snapshotSelection(ctx, tx, serviceID, current)
		if err != nil {
			return err
		}
		to, err := snapshotSelection(ctx, tx, serviceID, target)
		if err != nil {
			return err
		}

		out = PriceRecalculation{
			ServiceID:      serviceID,
			SubscriptionID: subID,
			From:           from,
			To:             to,
			PlanChanged:    from.PlanID != to.PlanID || from.Region != to.Region,
		}
		out.Changed = from.PriceVersionID != to.PriceVersionID || from.OptionsMinor != to.OptionsMinor ||
			from.SetupMinor != to.SetupMinor

		if !apply || !out.Changed {
			return nil
		}

		// The stored selection is written before the subscription moves, in the same
		// transaction: a service cannot be left priced against the old options for even one
		// renewal, and a rollback puts both halves back.
		repriced, err := applySelectionPrices(ctx, tx, serviceID, target)
		if err != nil {
			return err
		}
		out.RepricedOptions = repriced

		if target.ID != current.ID {
			tag, err := tx.Exec(ctx, `
				UPDATE subscriptions SET price_version_id = $2, version = version + 1
				WHERE id = $1`, subID, target.ID)
			if err != nil {
				return fmt.Errorf("service: reprice subscription: %w", err)
			}
			if tag.RowsAffected() != 1 {
				return ErrNotFound
			}
		}

		// Only a change of *plan* needs the provisioner. A corrected amount on the same plan is
		// a billing fact, and queueing a resize for it would restart a customer's server to
		// tell it nothing.
		if out.PlanChanged && externalID != "" {
			opID, err := enqueueResize(ctx, tx, serviceID, target, generation)
			if err != nil {
				return err
			}
			out.OperationID = opID
		}

		return s.audit(ctx, tx, "service.price_recalculated", "service", serviceID, actorID,
			map[string]any{
				"from_price_version_id": from.PriceVersionID,
				"to_price_version_id":   to.PriceVersionID,
				"from_total_minor":      from.TotalMinor,
				"to_total_minor":        to.TotalMinor,
				"from_options_minor":    from.OptionsMinor,
				"to_options_minor":      to.OptionsMinor,
				"currency":              to.Currency,
				"repriced_options":      repriced,
				"plan_changed":          out.PlanChanged,
				"operation_id":          out.OperationID,
				// The status is reported rather than changed: a recalculation is not a
				// suspension or a resume, and an operator using it to fix a price must not
				// discover it also restarted something.
				"status": status,
			})
	})
	if err != nil {
		return PriceRecalculation{}, err
	}
	return out, nil
}

// currentPriceVersion reads the newest version of a product's price for one interval and currency.
func currentPriceVersion(ctx context.Context, q Querier, productID, interval, currency string) (priceVersion, error) {
	var id string
	err := q.QueryRow(ctx, `
		SELECT id FROM price_versions
		WHERE product_id = $1 AND interval = $2 AND currency = $3
		ORDER BY version DESC
		LIMIT 1`, productID, interval, currency).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return priceVersion{}, fmt.Errorf(
			"%w: the catalog has no %s price in %s for this product", ErrNotFound, interval, currency)
	}
	if err != nil {
		return priceVersion{}, fmt.Errorf("service: read catalog price: %w", err)
	}
	return loadPriceVersion(ctx, q, id)
}

// snapshotSelection sums a service's stored selection at one price version's prices.
//
// The stored amount is *not* used: it is the amount the customer was charged when they chose,
// and the whole question a recalculation asks is what the same selection costs now.
func snapshotSelection(ctx context.Context, q Querier, serviceID string,
	price priceVersion) (PriceSnapshot, error) {
	out := PriceSnapshot{
		PriceVersionID: price.ID,
		Version:        price.Version,
		Name:           price.Name,
		Interval:       price.Interval,
		Currency:       price.Currency,
		PlanMinor:      price.AmountMinor,
		PlanID:         price.ProvisionPlan,
		Region:         price.ProvisionRegion,
	}
	rows, err := q.Query(ctx, `
		SELECT sc.config_option_id, sc.value
		FROM service_configs sc
		WHERE sc.service_id = $1`, serviceID)
	if err != nil {
		return PriceSnapshot{}, fmt.Errorf("service: read selection: %w", err)
	}
	type chosen struct{ optionID, value string }
	var chosenRows []chosen
	for rows.Next() {
		var c chosen
		if err := rows.Scan(&c.optionID, &c.value); err != nil {
			rows.Close()
			return PriceSnapshot{}, fmt.Errorf("service: scan selection: %w", err)
		}
		chosenRows = append(chosenRows, c)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return PriceSnapshot{}, fmt.Errorf("service: read selection: %w", err)
	}
	rows.Close()

	for _, c := range chosenRows {
		var amountMinor, setupMinor int64
		err := q.QueryRow(ctx, `
			SELECT amount_minor, setup_minor FROM config_option_prices
			WHERE config_option_id = $1 AND price_version_id = $2 AND choice = $3`,
			c.optionID, price.ID, c.value).Scan(&amountMinor, &setupMinor)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return PriceSnapshot{}, fmt.Errorf("service: read option price: %w", err)
		}
		// A choice with no price row on this version is free, which is what the migration
		// documents; it is not a missing price to guess at.
		out.OptionsMinor += amountMinor
		out.SetupMinor += setupMinor
	}
	out.TotalMinor = out.PlanMinor + out.OptionsMinor
	return out, nil
}

// applySelectionPrices rewrites the stored option amounts to one price version's prices.
//
// It returns how many rows actually moved, which is what distinguishes "the catalog changed" from
// "the subscription was on a stale version with identical amounts".
func applySelectionPrices(ctx context.Context, tx pgx.Tx, serviceID string, price priceVersion) (int, error) {
	rows, err := tx.Query(ctx, `
		SELECT sc.config_option_id, sc.value
		FROM service_configs sc
		WHERE sc.service_id = $1
		FOR UPDATE`, serviceID)
	if err != nil {
		return 0, fmt.Errorf("service: lock selection: %w", err)
	}
	type chosen struct{ optionID, value string }
	var chosenRows []chosen
	for rows.Next() {
		var c chosen
		if err := rows.Scan(&c.optionID, &c.value); err != nil {
			rows.Close()
			return 0, fmt.Errorf("service: scan selection: %w", err)
		}
		chosenRows = append(chosenRows, c)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, fmt.Errorf("service: lock selection: %w", err)
	}
	rows.Close()

	moved := 0
	for _, c := range chosenRows {
		var amountMinor, setupMinor int64
		err := tx.QueryRow(ctx, `
			SELECT amount_minor, setup_minor FROM config_option_prices
			WHERE config_option_id = $1 AND price_version_id = $2 AND choice = $3`,
			c.optionID, price.ID, c.value).Scan(&amountMinor, &setupMinor)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return 0, fmt.Errorf("service: read option price: %w", err)
		}
		tag, err := tx.Exec(ctx, `
			UPDATE service_configs
			SET amount_minor = $3, setup_minor = $4
			WHERE service_id = $1 AND config_option_id = $2
			  AND (amount_minor <> $3 OR setup_minor <> $4)`,
			serviceID, c.optionID, amountMinor, setupMinor)
		if err != nil {
			return 0, fmt.Errorf("service: reprice selection: %w", err)
		}
		moved += int(tag.RowsAffected())
	}
	return moved, nil
}
