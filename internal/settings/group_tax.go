// The tax jurisdiction settings group.
//
// `tax.enabled` and `tax.default_mode` live in the shared definitions table; these five are the
// facts the *rules* need and a rate table cannot state. They are registered here rather than
// appended to that table for the reason group_storefront.go exists: the shared table is a merge
// hotspot, and a setting that disappears in an adjacency conflict is a setting that silently
// changes what an installation charges.
//
// Every one of them defaults to "off" or "empty", which is the behaviour the product had before
// these rules existed. That is deliberate: an installation that upgrades must not start charging
// or refunding tax because a new key appeared with a guess in it.
//
//   - `seller_country` is the installation's own place of establishment. It is required before
//     reverse charge can be decided: whether a supply is cross-border is the first question the
//     rule asks, and an empty seller country makes every answer a guess.
//   - `reverse_charge` switches the EU B2B rule on. Off by default because it changes what is
//     charged to a customer who supplies a VAT ID: zero instead of the destination rate.
//   - `vat_validation_url` is the service a VAT ID is checked against. The default is the
//     European Commission's VIES endpoint, because inventing a local answer would mean granting
//     reverse charge on an unchecked string.
//   - `vat_validation_ttl_hours` is how long a check is trusted. A cart is re-priced on every
//     view, and calling an external service per render would make the cart depend on it.
//   - `nexus_enforced` switches the subnational sales-tax rule on. Off by default for the same
//     reason as `reverse_charge`: once it is on, a country with any nexus declaration is taxed
//     only where a declaration matches.
package settings

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

func init() {
	Register(
		Definition{
			Group: "tax", Key: "seller_country", Kind: KindString, Default: "",
			Check: countryCodeOrEmpty,
		},
		Definition{
			Group: "tax", Key: "reverse_charge", Kind: KindBool, Default: "false",
		},
		Definition{
			Group: "tax", Key: "vat_validation_url", Kind: KindString,
			// The official VIES endpoint. It is a fact about the EU, not about this
			// installation, and it is the only service this code knows how to speak to.
			Default: "https://ec.europa.eu/taxation_customs/vies/services/checkVatService",
			Check:   httpURL,
		},
		Definition{
			Group: "tax", Key: "vat_validation_ttl_hours", Kind: KindInt, Default: "24",
			Check: betweenOneAnd(8760),
		},
		Definition{
			Group: "tax", Key: "nexus_enforced", Kind: KindBool, Default: "false",
		},
	)
}

// countryCodeOrEmpty accepts "" (not configured) or an ISO 3166-1 alpha-2 code in upper case.
// A lower-case code is a typo rather than a variant: every comparison against a country in this
// codebase is an equality on upper-case text.
func countryCodeOrEmpty(d Definition, value string) error {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return nil
	}
	if len(trimmed) != 2 || trimmed != strings.ToUpper(trimmed) {
		return fmt.Errorf("%w: %s must be empty or two upper-case letters, e.g. DE", ErrInvalidValue, d.Name())
	}
	for _, r := range trimmed {
		if r < 'A' || r > 'Z' {
			return fmt.Errorf("%w: %s must be two letters, e.g. DE", ErrInvalidValue, d.Name())
		}
	}
	return nil
}

// httpURL refuses anything that is not an http(s) endpoint. A validation service reached over
// another scheme is either a local file read or a typo, and neither is a service that can
// confirm a VAT ID.
func httpURL(d Definition, value string) error {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return nil
	}
	parsed, err := url.Parse(trimmed)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return fmt.Errorf("%w: %s must be an http(s) URL", ErrInvalidValue, d.Name())
	}
	return nil
}

func betweenOneAnd(max int) func(Definition, string) error {
	return func(d Definition, value string) error {
		n, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil {
			// The kind check already ran, so this can only be an overflow.
			return fmt.Errorf("%w: %s must be a whole number", ErrInvalidValue, d.Name())
		}
		if n < 1 || n > max {
			return fmt.Errorf("%w: %s must be between 1 and %d", ErrInvalidValue, d.Name(), max)
		}
		return nil
	}
}
