package tax

// The resolving pricer: what the invoice issuer and the cart should use instead of a fixed rate.
//
// A `billing.Pricer` is asked to price lines it did not choose, which is the correct shape: the
// arithmetic stays in `billing.FixedTaxPricer` and the *decision* about which rate applies stays
// here. This type is the bridge, and it exists so there is exactly one place where "what rate does
// this supply carry" is answered.
//
// # Why a pricer is built per customer
//
// The rate depends on facts about the customer (billing country, VAT ID), and `Pricer.Price` is not
// given a customer. Rather than push those facts through a widening interface — which would touch
// every implementor — the pricer is constructed *for* one supply with the facts already resolved.
// The cart path does this per request; an invoice issuer should do the same (see the report on
// TAX-02: `internal/httpapi/server.go` still builds the issuer with a no-tax pricer).
//
// # What "no tax" means here
//
// Three different absences are deliberately not collapsed:
//
//   - tax is switched off  → the caller passes no pricer at all and prices net;
//   - the rate table is empty → this pricer charges nothing through the same `no-tax` policy the
//     product shipped with, so an installation that has never configured a rate is unchanged;
//   - a rate exists but the rules cannot decide → the price fails with a *Refusal, and nothing is
//     charged. That is the whole point of the rules layer.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/catalyst/billing/internal/billing"
	"github.com/catalyst/billing/internal/commerce"
	"github.com/catalyst/billing/internal/money"
	"github.com/jackc/pgx/v5"
)

// RulesPricer prices one supply using a decision the rules already made.
type RulesPricer struct {
	// Decision is what the rules concluded. It is resolved once, at construction: a cart is
	// re-priced from the same facts, and an invoice must not change its mind between lines.
	Decision Decision
	// Store resolves the rate for a charge. It is the same store the rules used.
	Store *commerce.Store
	// Rounding defaults to half-up, the product's rounding rule.
	Rounding money.Rounding
}

// Price implements billing.Pricer.
func (p RulesPricer) Price(ctx context.Context, lines []billing.Line, cur money.Currency) ([]billing.LineResult, billing.Totals, billing.TaxEvidence, error) {
	switch p.Decision.Kind {
	case DecisionRate:
		// The decision already resolved the exact rate inside the issuing transaction (see
		// ResolvePricer), so price from that resolved value rather than looking the rate up a
		// second time: a second lookup could see a different row and make the evidence describe
		// a rate the document was not priced from. The arithmetic is still FixedTaxPricer's, the
		// one implementation of rounding.
		ratio, err := money.ParseRatio(p.Decision.Rate)
		if err != nil {
			return nil, billing.Totals{}, billing.TaxEvidence{}, fmt.Errorf("tax: decision rate %q is not a rational: %w", p.Decision.Rate, err)
		}
		rounding := p.Rounding
		if !rounding.Valid() {
			rounding = money.RoundHalfUp
		}
		mode := "exclusive"
		if p.Decision.Inclusive {
			mode = "inclusive"
		}
		delegate := billing.FixedTaxPricer{
			Rate:         ratio,
			Inclusive:    p.Decision.Inclusive,
			Rounding:     rounding,
			Policy:       p.Decision.Policy,
			Jurisdiction: p.Decision.Jurisdiction,
		}
		results, totals, evidence, err := delegate.Price(ctx, lines, cur)
		if err != nil {
			return nil, billing.Totals{}, billing.TaxEvidence{}, err
		}
		evidence.Mode = mode
		return results, totals, evidence, nil

	case DecisionNoTax:
		// The shipped behaviour: an installation with no rates configured charges nothing. It is
		// not a zero-rate decision and does not pretend to be one.
		return p.zero(ctx, lines, cur, billing.TaxEvidence{
			Mode:         "exclusive",
			Policy:       PolicyNoTax,
			Rate:         "0",
			Jurisdiction: "none",
		})

	default:
		// Reverse charge, no nexus, staff exemption and an unconfigured destination all charge
		// nothing, and each records its own policy and reason. The reason is what makes a zero on
		// a document defensible a year later.
		return p.zero(ctx, lines, cur, billing.TaxEvidence{
			Mode:            "exclusive",
			Policy:          p.Decision.Policy,
			Rate:            "0",
			Jurisdiction:    p.Decision.Jurisdiction,
			ExemptionReason: p.Decision.Reason,
		})
	}
}

// zero prices every line with no tax, preserving the line shape so nothing downstream needs a
// special case for "free of tax".
func (p RulesPricer) zero(ctx context.Context, lines []billing.Line, cur money.Currency, evidence billing.TaxEvidence) ([]billing.LineResult, billing.Totals, billing.TaxEvidence, error) {
	rounding := p.Rounding
	if !rounding.Valid() {
		rounding = money.RoundHalfUp
	}
	delegate := billing.FixedTaxPricer{
		Rate:         money.ZeroRatio(),
		Inclusive:    false,
		Rounding:     rounding,
		Policy:       evidence.Policy,
		Jurisdiction: evidence.Jurisdiction,
	}
	results, totals, _, err := delegate.Price(ctx, lines, cur)
	if err != nil {
		return nil, billing.Totals{}, billing.TaxEvidence{}, err
	}
	return results, totals, evidence, nil
}

// PricerForCustomer builds the pricer for one customer's sale at an instant.
//
// It returns (nil, nil) when tax is switched off, which is the signal the cart and the checkout
// use to price net: the off switch is a configuration, not a zero rate, and pretending otherwise
// would write evidence claiming a rate was resolved.
//
// When the rules refuse, the refusal is returned as the error. A caller that wants a price fails
// loudly with a stable code rather than charging a plausible number.
// ResolvePricer implements billing.PricerResolver. It resolves a customer's supply against the
// caller's transaction, so the billing profile, the settings, the rate row and any VAT-ID
// validation read here are the same snapshot the invoice is written in.
//
// A nil pricer with a nil error means tax is switched off; the issuer keeps its no-tax fallback.
func (r *Rules) ResolvePricer(ctx context.Context, tx pgx.Tx, customerID string, at time.Time) (billing.Pricer, error) {
	return r.WithTx(tx).PricerForCustomer(ctx, customerID, at)
}

func (r *Rules) PricerForCustomer(ctx context.Context, customerID string, at time.Time) (billing.Pricer, error) {
	enabled, err := r.taxEnabled(ctx)
	if err != nil {
		return nil, err
	}
	if !enabled {
		return nil, nil
	}
	supply, err := r.supplyFor(ctx, customerID, at)
	if err != nil {
		return nil, err
	}
	decision, err := r.Decide(ctx, supply)
	if err != nil {
		return nil, err
	}
	return RulesPricer{Decision: decision, Store: r.store}, nil
}

// PricerForSupply builds the pricer for a supply the caller already assembled — a quote for a
// visitor, a re-price of a stored invoice, a staff preview.
func (r *Rules) PricerForSupply(ctx context.Context, supply Supply) (billing.Pricer, error) {
	supply.At = supply.At.UTC()
	if supply.At.IsZero() {
		supply.At = r.now()
	}
	decision, err := r.Decide(ctx, supply)
	if err != nil {
		return nil, err
	}
	return RulesPricer{Decision: decision, Store: r.store}, nil
}

// DecideForCustomer is PricerForCustomer without the pricer, for a screen that wants to explain the
// decision rather than price anything.
func (r *Rules) DecideForCustomer(ctx context.Context, customerID string, at time.Time) (Decision, error) {
	supply, err := r.supplyFor(ctx, customerID, at)
	if err != nil {
		return Decision{}, err
	}
	return r.Decide(ctx, supply)
}

// supplyFor reads the facts the billing profile holds. A customer without a profile has no
// country, which the rules treat as "this destination is not configured" rather than as an error:
// the profile is filled in at checkout, and a cart before that is priced net with the reason
// recorded.
func (r *Rules) supplyFor(ctx context.Context, customerID string, at time.Time) (Supply, error) {
	if at.IsZero() {
		at = r.now()
	}
	supply := Supply{CustomerID: customerID, At: at}
	err := r.q.QueryRow(ctx, `
		SELECT country_code, region, tax_id, tax_exempt, tax_exemption_reason
		  FROM billing_profiles WHERE customer_id = $1`, customerID).
		Scan(&supply.Country, &supply.Region, &supply.VATID, &supply.StaffExempt, &supply.StaffExemptReason)
	if errors.Is(err, pgx.ErrNoRows) {
		// No profile: no destination. The rules record that as an unconfigured jurisdiction.
		return supply, nil
	}
	if err != nil {
		return Supply{}, fmt.Errorf("tax: read billing profile for %s: %w", customerID, err)
	}
	supply.Country = strings.ToUpper(strings.TrimSpace(supply.Country))
	supply.Region = strings.ToUpper(strings.TrimSpace(supply.Region))
	return supply, nil
}

// taxEnabled reads the master switch from the settings table directly: this package must work on
// the invoice path, which has no HTTP request and therefore no settings store with an auditor.
func (r *Rules) taxEnabled(ctx context.Context) (bool, error) {
	var value string
	err := r.q.QueryRow(ctx,
		`SELECT value FROM settings WHERE group_key = 'tax' AND key = 'enabled'`).Scan(&value)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("tax: read tax switch: %w", err)
	}
	return strings.EqualFold(strings.TrimSpace(value), "true"), nil
}

// TaxEnabled reports whether the installation charges tax at all.
//
// It is the master switch, and it is exported because a screen that explains a decision has to be
// able to say "tax is switched off" rather than "no rate resolved for this jurisdiction".
func (r *Rules) TaxEnabled(ctx context.Context) (bool, error) {
	return r.taxEnabled(ctx)
}
