package renewal

// Renewal surcharges: the recurring half of a configurable product.
//
// Checkout folds the chosen config options into the *first* invoice's base line (see
// internal/register/checkout.go, loadConfig/checkoutTx) and writes the selection to
// `service_configs`, one row per chosen option. Before this file existed the renewal engine
// billed `price_versions.amount_minor` and nothing else, so every paid option — extra RAM,
// a bigger backup target, a control-panel add-on — was charged once and then given away for
// the rest of the subscription's life while the provisioned resource kept running. The leak
// was unbounded, silent, and hit every subscription with a paid option.
//
// The engine now bills the base price plus one line per chosen option that carries a
// recurring amount.

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/catalyst/billing/internal/billing"
	"github.com/catalyst/billing/internal/money"
	"github.com/jackc/pgx/v5"
)

// ErrUnpricedSurcharge is returned when a selection that was sold as paid has no price row
// at the subscription's price version. The engine routes it to review instead of billing.
var ErrUnpricedSurcharge = errors.New("renewal: a paid config option has no price at the subscription's price version")

// loadSurchargeLines turns the config options chosen on the subscription's service into
// recurring invoice lines for the period being renewed.
//
// # Which price
//
// An option is priced from `config_option_prices`, keyed by
// (config_option_id, price_version_id, choice) — the same key and the same "a choice with
// no row is free" rule that internal/service/config.go uses when it prorates a mid-cycle
// change, and the same resolution checkout used to write `service_configs` in the first
// place. The `price_version_id` is the *subscription's*, i.e. the one the base line is read
// from, so the option and its base can never drift onto different versions of the catalogue.
//
// We deliberately do not price from `service_configs.amount_minor`, even though that column
// exists: it is the amount captured at selection time, and using it would silently keep
// charging a price the catalogue has since retired. A price version is immutable, so
// resolving against it is stable, and the two agree for every service checkout created.
//
// # setup_minor
//
// Setup is charged once, on the invoice that creates the service. It is not read here at
// all: a renewal that repeated it would charge the customer again every period for work that
// happened once. TestSurchargeSetupDoesNotRecur pins that.
//
// # A missing price row
//
// The catalogue can fail to price a selection in two very different ways, and `service_configs`
// is what tells them apart:
//
//   - the selection recorded no amount at selection time: the option was free (a checkbox,
//     a text or number option, a free dropdown choice). It contributes no line. Treating this
//     as an error would wedge the renewal of every service that has a free-form option, and
//     the migration is explicit that "a choice with no row here is free".
//   - the selection recorded a non-zero amount: the customer is paying for it today and the
//     catalogue no longer prices that choice at this version. Renewing at zero would hand the
//     resource over for free — the same revenue leak this file exists to close — and renewing
//     at the recorded amount would break the rule that a period is priced from one price
//     version. So the renewal is refused as needs_review, the same way an anchor divergence
//     between the catalogue and the subscription is: a human decides, and nothing is billed
//     on a guess. The subscription is not advanced, so the run keeps reporting it until the
//     catalogue or the service is fixed.
func loadSurchargeLines(ctx context.Context, tx pgx.Tx, sub Subscription, cur money.Currency) ([]billing.Line, error) {
	// One row per stored selection. The LEFT JOIN is the price lookup: a NULL
	// `priced_minor` is the "the catalogue has no row for this choice at this version" case
	// that the comment above turns into either "free" or "needs review".
	rows, err := tx.Query(ctx, `
		SELECT co.name, sc.value, sc.amount_minor, cop.amount_minor
		FROM services s
		JOIN service_configs sc ON sc.service_id = s.id
		JOIN config_options co ON co.id = sc.config_option_id
		LEFT JOIN config_option_prices cop
		       ON cop.config_option_id = sc.config_option_id
		      AND cop.price_version_id = $2
		      AND cop.choice = sc.value
		WHERE s.subscription_id = $1
		ORDER BY co.sort_order, co.name, co.id`,
		sub.ID, sub.PriceVersionID)
	if err != nil {
		return nil, fmt.Errorf("renewal: read service configs: %w", err)
	}
	defer rows.Close()

	type selection struct {
		name     string
		value    string
		recorded int64
		priced   *int64
	}
	var selections []selection
	for rows.Next() {
		var s selection
		if err := rows.Scan(&s.name, &s.value, &s.recorded, &s.priced); err != nil {
			return nil, fmt.Errorf("renewal: scan service config: %w", err)
		}
		selections = append(selections, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("renewal: read service configs: %w", err)
	}
	// Close before returning so a caller's next statement on this transaction is not blocked
	// by a still-open cursor.
	rows.Close()

	lines := make([]billing.Line, 0, len(selections))
	var unpriced []string
	for _, s := range selections {
		switch {
		case s.priced != nil && *s.priced > 0:
			// The description matches the text checkout uses for the same selection on the
			// first invoice ("RAM: 32 GB"), so the customer can recognise the option, and it
			// is one line per option rather than a lump, so the document shows what is being
			// paid for.
			lines = append(lines, billing.Line{
				Description:   s.name + ": " + s.value,
				QuantityMilli: 1000,
				UnitPrice:     money.New(*s.priced, cur),
			})
		case s.priced != nil:
			// A zero-priced choice is free. No line at all: a zero-amount line is noise on a
			// document the customer keeps.
		case s.recorded > 0:
			unpriced = append(unpriced, s.name+": "+s.value)
		}
		// s.recorded > 0 with no price row is collected above; a free selection with no price
		// row is simply nothing to bill.
	}
	if len(unpriced) > 0 {
		return nil, fmt.Errorf("%w: %s", ErrUnpricedSurcharge, strings.Join(unpriced, ", "))
	}
	return lines, nil
}
