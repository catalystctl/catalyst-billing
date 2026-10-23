// The storefront settings group.
//
// It lives in its own file, registered from an init, so the storefront's presentation rules do not
// have to be added to the one eight-hundred-line definitions table — that table is a merge hotspot
// and an adjacency conflict in it is resolved by hand, which is how a setting quietly disappears.
//
// Four keys, and each one is a decision an operator can plausibly want to make differently:
//
//   - `home_categories` is SF-01's rule. Paymenter's home page lists the *top-level* categories
//     that have children or visible products; a shop whose catalogue is flat and whose operator
//     wants every category in the strip can say `all` instead.
//   - `show_stock` and `low_stock_threshold` are SF-06's display half. A shop that does not want
//     to publish "3 left" (it tells a competitor how thin the stock is) can turn it off without
//     turning off the out-of-stock refusal, which is a correctness rule rather than a display
//     one.
//   - `currency_switcher` is SF-18's. The switcher only makes sense when an operator prices in
//     more than one currency; until then it is a control that changes nothing.
//
// The defaults are the behaviour the storefront already had, so an installation that never opens
// the settings page is unchanged: top-level categories, stock shown with a threshold of five, and
// the switcher available.
package settings

import (
	"fmt"
	"strconv"
	"strings"
)

func init() {
	Register(
		Definition{
			Group: "storefront", Key: "home_categories", Kind: KindString,
			Default: "top_level",
			// top_level: the Paymenter rule. all: every visible category, in sort order.
			Choices: []string{"top_level", "all"},
		},
		Definition{
			Group: "storefront", Key: "show_stock", Kind: KindBool, Default: "true",
		},
		Definition{
			Group: "storefront", Key: "low_stock_threshold", Kind: KindInt, Default: "5",
			// A threshold of zero would mean "never warn", which is what `show_stock = false`
			// already says. Keeping it positive means the two settings cannot contradict.
			Check: positiveIntOrZero,
		},
		Definition{
			Group: "storefront", Key: "currency_switcher", Kind: KindBool, Default: "true",
		},
	)
}

// positiveIntOrZero refuses a negative threshold. A negative "low stock" number is not a
// preference, it is a typo, and a comparison against it would report every product as low.
func positiveIntOrZero(d Definition, value string) error {
	n, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil {
		// The kind check already ran, so this can only be an overflow.
		return fmt.Errorf("%w: %s must be a whole number", ErrInvalidValue, d.Name())
	}
	if n < 0 {
		return fmt.Errorf("%w: %s cannot be negative", ErrInvalidValue, d.Name())
	}
	return nil
}
