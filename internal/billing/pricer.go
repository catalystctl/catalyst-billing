package billing

import (
	"context"
	"fmt"

	"github.com/catalyst/billing/internal/money"
)

// FixedTaxPricer is a deliberately simple Pricer: one explicit rate applied to
// every line, with an explicit inclusive/exclusive mode.
//
// v1 ships an explicitly configured, reviewed launch-market policy rather than
// guessing a jurisdiction. This type is that policy. It refuses configurations
// it cannot compute instead of approximating them.
type FixedTaxPricer struct {
	// Rate is the exact tax rate, e.g. money.MustParseRatio("20%").
	Rate money.Ratio
	// Inclusive means the quoted price already contains tax, so tax is
	// extracted from it rather than added to it.
	Inclusive bool
	// Rounding is applied once per line.
	Rounding money.Rounding
	// Policy names the configuration; it is recorded as tax evidence.
	Policy string
	// Jurisdiction is recorded as evidence for audit.
	Jurisdiction string
}

// Price implements Pricer.
func (p FixedTaxPricer) Price(_ context.Context, lines []Line, cur money.Currency) ([]LineResult, Totals, TaxEvidence, error) {
	if !p.Rounding.Valid() {
		return nil, Totals{}, TaxEvidence{}, fmt.Errorf("%w: invalid rounding mode", ErrInvalidInvoice)
	}
	if !cur.IsValid() {
		return nil, Totals{}, TaxEvidence{}, fmt.Errorf("%w: no currency", ErrInvalidInvoice)
	}
	if len(lines) == 0 {
		return nil, Totals{}, TaxEvidence{}, fmt.Errorf("%w: no lines", ErrInvalidInvoice)
	}

	mode := "exclusive"
	if p.Inclusive {
		mode = "inclusive"
	}
	evidence := TaxEvidence{
		Mode:         mode,
		Policy:       p.Policy,
		Rate:         p.Rate.String(),
		Jurisdiction: p.Jurisdiction,
	}

	results := make([]LineResult, 0, len(lines))
	subtotal := money.Zero(cur)
	taxTotal := money.Zero(cur)
	grandTotal := money.Zero(cur)

	for i, l := range lines {
		quoted, err := lineValue(l, cur)
		if err != nil {
			return nil, Totals{}, TaxEvidence{}, fmt.Errorf("line %d: %w", i, err)
		}

		var net, tax, total money.Amount
		if p.Inclusive {
			// The quoted value is the gross. Tax is the exact share
			// gross * r / (1 + r), rounded once; the net is then whatever is
			// left, so net + tax == gross holds exactly and the customer is
			// never charged a different total than the one quoted.
			ratio, err := p.shareOfGross()
			if err != nil {
				return nil, Totals{}, TaxEvidence{}, err
			}
			tax, err = money.ApplyRate(quoted, ratio, p.Rounding)
			if err != nil {
				return nil, Totals{}, TaxEvidence{}, err
			}
			if net, err = quoted.Sub(tax); err != nil {
				return nil, Totals{}, TaxEvidence{}, err
			}
			total = quoted
		} else {
			net = quoted
			if tax, err = money.ApplyRate(net, p.Rate, p.Rounding); err != nil {
				return nil, Totals{}, TaxEvidence{}, err
			}
			if total, err = net.Add(tax); err != nil {
				return nil, Totals{}, TaxEvidence{}, err
			}
		}

		results = append(results, LineResult{
			Description:   l.Description,
			QuantityMilli: l.QuantityMilli,
			UnitMinor:     l.UnitPrice.Minor(),
			Net:           net,
			Tax:           tax,
			Total:         total,
			TaxRate:       p.Rate.String(),
		})

		if subtotal, err = subtotal.Add(net); err != nil {
			return nil, Totals{}, TaxEvidence{}, err
		}
		if taxTotal, err = taxTotal.Add(tax); err != nil {
			return nil, Totals{}, TaxEvidence{}, err
		}
		if grandTotal, err = grandTotal.Add(total); err != nil {
			return nil, Totals{}, TaxEvidence{}, err
		}
	}

	// The invoice-level identity must hold, or the schema will reject the
	// invoice later with a much less helpful error.
	sum, err := subtotal.Add(taxTotal)
	if err != nil {
		return nil, Totals{}, TaxEvidence{}, err
	}
	if cmp, err := sum.Cmp(grandTotal); err != nil {
		return nil, Totals{}, TaxEvidence{}, err
	} else if cmp != 0 {
		return nil, Totals{}, TaxEvidence{}, fmt.Errorf(
			"%w: subtotal %s + tax %s does not equal total %s",
			ErrInvalidInvoice, subtotal, taxTotal, grandTotal)
	}

	return results, Totals{Subtotal: subtotal, Tax: taxTotal, Total: grandTotal}, evidence, nil
}

// shareOfGross returns r/(1+r): the fraction of a tax-inclusive price that is
// tax.
func (p FixedTaxPricer) shareOfGross() (money.Ratio, error) {
	one := money.One()
	onePlusRate, err := money.RatioFromRat(new(money.Rat).Add(p.Rate.Rat(), one.Rat()))
	if err != nil {
		return money.Ratio{}, err
	}
	return p.Rate.Divide(onePlusRate)
}

// lineValue is the quoted value of a line: unit price times quantity, rounded
// once.
//
// A quantity of exactly 1.000 is the common case and bypasses the
// multiplication entirely, so a monthly plan's price can never drift by a minor
// unit because of a rounding step that should not have existed.
func lineValue(l Line, cur money.Currency) (money.Amount, error) {
	if l.QuantityMilli <= 0 {
		return money.Amount{}, fmt.Errorf("%w: quantity must be positive", ErrInvalidInvoice)
	}
	if !l.UnitPrice.Currency().Equal(cur) {
		return money.Amount{}, fmt.Errorf("%w: %s vs %s", ErrCurrencyMismatch, l.UnitPrice.Currency(), cur)
	}
	if l.QuantityMilli == 1000 {
		return l.UnitPrice, nil
	}
	ratio, err := money.NewRatio(l.QuantityMilli, 1000)
	if err != nil {
		return money.Amount{}, err
	}
	return money.ApplyRate(l.UnitPrice, ratio, money.RoundHalfUp)
}
