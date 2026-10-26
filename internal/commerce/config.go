// Config option pricing: one rule, shared by the cart and the checkout.
//
// A product can carry a tree of config options (RAM, backup, region), each of which adds a
// recurring amount and a one-off setup fee to the price. The *price of a selection* is not
// the option's price: it is the price of the chosen **choice**, looked up per price version
// in `config_option_prices`. A choice with no row there is free, which is the migration's
// documented meaning and what the renewal engine and the service upgrade path already
// implement.
//
// That rule used to live only inside `register.loadConfig`, and it read the option's price
// without matching the chosen `choice`. Two consequences, both fixed here:
//
//   - The cart never read `config_option_prices` at all, so a cart total was lower than the
//     invoice the customer was then asked to pay (CT-03).
//   - The checkout priced an option by whichever `config_option_prices` row happened to be
//     returned last, not by the choice the customer picked. A dropdown with a free and a paid
//     tier could charge either.
//
// `LoadConfigSelections` is the single implementation both paths now call, so "the cart
// shows what the invoice charges" is a property of shared code rather than of two queries
// that have to be kept in step by hand.

package commerce

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
)

// ConfigQuerier is the read surface LoadConfigSelections needs. It is declared here rather than
// reusing the store's internal querier so the helper is usable from a caller's transaction
// without exposing the whole store interface. A *pgx.Tx and a pool both satisfy it.
type ConfigQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// ConfigSelection is one validated option selection and its catalog price for the price
// version the line is being bought on.
type ConfigSelection struct {
	OptionID string `json:"option_id"`
	Name     string `json:"name"`
	Value    string `json:"value"`
	// Amount and Setup are the choice's recurring and one-off amounts in the price version's
	// currency, read from `config_option_prices`. Both are zero when the choice is free.
	Amount int64 `json:"amount_minor"`
	Setup  int64 `json:"setup_minor"`
}

// LoadConfigSelections reads and validates the chosen config options for one product on one
// price version.
//
// The validation is the checkout's, unchanged: an unknown or hidden option is refused, a value
// that is not one of a dropdown/radio option's choices is refused, and a required option that
// was not selected is refused. A blank value means "not chosen" and contributes nothing.
//
// The price is read per (option, price version, choice). A missing price row is free rather
// than an error, because that is what the migration documents; a paid selection that has lost
// its price row is the renewal engine's concern, not a new cart's.
func LoadConfigSelections(ctx context.Context, q ConfigQuerier, productID, priceVersionID string,
	selected map[string]string, enforceRequired bool) ([]ConfigSelection, error) {
	rows, err := q.Query(ctx, `
		SELECT id, name, option_type, choices, required, hidden
		FROM config_options
		WHERE product_id = $1
		ORDER BY sort_order, name`, productID)
	if err != nil {
		return nil, fmt.Errorf("commerce: load config options: %w", err)
	}
	defer rows.Close()

	type option struct {
		id       string
		name     string
		kind     string
		choices  []string
		required bool
		hidden   bool
	}
	var options []option
	for rows.Next() {
		var (
			o       option
			rawJSON []byte
		)
		if err := rows.Scan(&o.id, &o.name, &o.kind, &rawJSON, &o.required, &o.hidden); err != nil {
			return nil, fmt.Errorf("commerce: scan config option: %w", err)
		}
		if len(rawJSON) > 0 {
			if err := json.Unmarshal(rawJSON, &o.choices); err != nil {
				return nil, fmt.Errorf("commerce: decode config choices: %w", err)
			}
		}
		options = append(options, o)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("commerce: read config options: %w", err)
	}
	// Close before the per-choice price lookups so a caller's transaction is not blocked by an
	// open cursor on the same connection.
	rows.Close()

	known := make(map[string]option, len(options))
	for _, o := range options {
		known[o.id] = o
	}

	// Deterministic order: the caller builds invoice descriptions from this slice, and a map
	// iteration would make the same selection render differently on two requests.
	ids := make([]string, 0, len(selected))
	for id := range selected {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	out := make([]ConfigSelection, 0, len(selected))
	for _, id := range ids {
		value := selected[id]
		o, ok := known[id]
		if !ok || o.hidden {
			return nil, fmt.Errorf("%w: unknown option %s", ErrInvalidInput, id)
		}
		if strings.TrimSpace(value) == "" {
			continue
		}
		if len(o.choices) > 0 && !containsString(o.choices, value) {
			return nil, fmt.Errorf("%w: %q is not an available choice for %s",
				ErrInvalidInput, value, o.name)
		}
		amount, setup, err := configChoicePrice(ctx, q, o.id, priceVersionID, value)
		if err != nil {
			return nil, err
		}
		out = append(out, ConfigSelection{
			OptionID: o.id, Name: o.name, Value: value,
			Amount: amount, Setup: setup,
		})
	}
	for _, o := range options {
		if !enforceRequired || !o.required || o.hidden {
			continue
		}
		if _, ok := selected[o.id]; !ok {
			return nil, fmt.Errorf("%w: %s must be chosen", ErrInvalidInput, o.name)
		}
	}
	return out, nil
}

// configChoicePrice reads one choice's amounts for a price version. No row means free.
func configChoicePrice(ctx context.Context, q ConfigQuerier, optionID, priceVersionID, choice string) (int64, int64, error) {
	var amount, setup int64
	err := q.QueryRow(ctx, `
		SELECT amount_minor, setup_minor
		FROM config_option_prices
		WHERE config_option_id = $1 AND price_version_id = $2 AND choice = $3`,
		optionID, priceVersionID, choice).Scan(&amount, &setup)
	if err != nil {
		if err == pgx.ErrNoRows {
			return 0, 0, nil
		}
		return 0, 0, fmt.Errorf("commerce: read option price: %w", err)
	}
	return amount, setup, nil
}

// ConfigAmount sums the recurring amounts of a selection.
func ConfigAmount(sel []ConfigSelection) int64 {
	var total int64
	for _, s := range sel {
		total += s.Amount
	}
	return total
}

// ConfigSetup sums the one-off setup amounts of a selection.
func ConfigSetup(sel []ConfigSelection) int64 {
	var total int64
	for _, s := range sel {
		total += s.Setup
	}
	return total
}

// ConfigSummary renders a selection for a line description, matching the checkout's format so
// the cart line and the invoice line read the same. The order is sorted by the rendered text,
// so the same selection always produces the same string.
func ConfigSummary(sel []ConfigSelection) string {
	if len(sel) == 0 {
		return ""
	}
	parts := make([]string, 0, len(sel))
	for _, s := range sel {
		parts = append(parts, s.Name+": "+s.Value)
	}
	sort.Strings(parts)
	return strings.Join(parts, ", ")
}

func containsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
