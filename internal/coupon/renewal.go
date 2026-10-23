// Package coupon applies a promotion stored on a subscription to the invoice that renews it.
//
// A coupon has two lives, and only the first one was ever implemented. At checkout it reduces the
// first invoice, and `subscriptions.coupon_id` / `coupon_cycles_applied` record what the customer
// bought: which promotion, and how many billing cycles it has already discounted. The renewal
// engine, meanwhile, re-prices from `price_versions` alone, so a "first three cycles" coupon
// discounted exactly one invoice and then quietly stopped — while the customer's order confirmation
// still said three.
//
// This package is the missing half, and it is deliberately shaped as two steps:
//
//   - **`RenewalDiscount` decides.** It reads the coupon and the subscription's counter and answers
//     one question: does this coupon discount this renewal, and by how much. It writes nothing, so
//     the answer can be tested, replayed and explained.
//   - **`ApplyRenewalDiscount` writes.** It turns that answer into a discount line on a *draft*
//     invoice, re-derives the tax on the discounted value, records the redemption and advances the
//     counter — all inside the caller's transaction, so an invoice can never exist with a counter
//     that disagrees with it.
//
// # Why the discount is a line and not a lower price
//
// The same reason checkout gives: the charge line must stay the catalog amount, or the customer is
// shown "Hosting 80.00" when the price list says 100 and has no way to check the number. The line
// kind is `discount`, its net is negative, it carries no tax of its own, and each charge line's tax
// is recomputed on the *discounted* value — which is what makes the reduction appear exactly once.
//
// # What a renewal is not
//
// A renewal has no setup fee, so a coupon restricted to `applies_to = 'setup'` does not apply to it.
// That is not an oversight: charging a setup discount every period would discount work that is not
// being done.
package coupon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/catalyst/billing/internal/billing"
	"github.com/catalyst/billing/internal/money"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Querier is the read side both functions need. `RenewalDiscount` accepts it so a caller can decide
// against a pool or inside a transaction; `ApplyRenewalDiscount` requires a pgx.Tx, because the
// invoice and its counter must commit together.
type Querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// Errors callers branch on.
var (
	// ErrInvoiceNotDraft is a discount applied to a document that is already issued. An issued
	// invoice's numbers are evidence; they are not editable, and a promotion that arrived late is a
	// credit note, not a rewrite.
	ErrInvoiceNotDraft = errors.New("coupon: the invoice is not a draft")
	// ErrNoDiscount is a renewal where the coupon grants nothing. Apply refuses rather than writing
	// a zero-value discount line, which the schema refuses too.
	ErrNoDiscount = errors.New("coupon: the coupon grants no discount on this renewal")
	// ErrAlreadyApplied is a replay: this invoice already carries this coupon's redemption. The
	// caller's transaction rolls the discount line back, so a retry cannot discount twice.
	ErrAlreadyApplied = errors.New("coupon: the invoice was already discounted by this coupon")
)

// Discount is the decision for one renewal invoice.
//
// It is a value, not a pointer: the caller holds the answer it applied and can record it. `Reason`
// is prose for a human; `Applies` is what code branches on.
type Discount struct {
	Applies bool
	// CouponID and Code identify the promotion. Code is what the discount line prints, so a
	// customer can match the line to the code they typed.
	CouponID string
	Code     string
	// AmountMinor is the discount in the invoice's currency, in minor units. Zero when !Applies.
	AmountMinor int64
	// Cycle is the 1-based cycle this discount would be: the first renewal after checkout is
	// cycle 2. It is recorded on the snapshot so "which cycle was this" is answerable.
	Cycle int
	// CyclesTotal is the coupon's `recurring_cycles`; nil means every cycle.
	CyclesTotal *int
	// AppliesTo is the coupon's scope ("price", "setup", "all"). A renewal only ever reduces the
	// recurring price.
	AppliesTo string
	// Reason explains a coupon that exists but does not apply to this invoice.
	Reason string
}

// RenewalDiscount decides what the coupon stored on a subscription does to one renewal.
//
// `baseMinor` is the recurring value being discounted (the base price plus every paid option) in
// `cur`. `at` is the renewal instant, which is what the coupon's own window is judged against: a
// promotion that expired last month must not discount this month's invoice.
//
// It reads but never writes, so the renewal engine can call it on every pass without side effects,
// including for a subscription it later routes to review. Call it inside the transaction that holds
// the subscription lock: the counter it reads is the one a concurrent renewal must not also spend.
func RenewalDiscount(ctx context.Context, q Querier, subscriptionID string, baseMinor int64,
	cur money.Currency, at time.Time) (Discount, error) {

	var (
		couponID   *string
		cyclesDone int
	)
	if err := q.QueryRow(ctx, `
		SELECT coupon_id, coupon_cycles_applied FROM subscriptions WHERE id = $1`,
		subscriptionID).Scan(&couponID, &cyclesDone); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Discount{}, fmt.Errorf("coupon: subscription %s not found", subscriptionID)
		}
		return Discount{}, fmt.Errorf("coupon: read subscription coupon: %w", err)
	}
	if couponID == nil || *couponID == "" {
		return Discount{}, nil
	}

	var (
		code        string
		kind        string
		percentage  *string
		amountMinor *int64
		couponCur   *string
		appliesTo   string
		maxUses     *int
		maxPerUser  *int
		startsAt    *time.Time
		expiresAt   *time.Time
		recurring   *int
		enabled     bool
	)
	err := q.QueryRow(ctx, `
		SELECT code, kind, percentage, amount_minor, currency, applies_to,
		       max_uses, max_uses_per_user, starts_at, expires_at, recurring_cycles, enabled
		  FROM coupons WHERE id = $1`, *couponID).
		Scan(&code, &kind, &percentage, &amountMinor, &couponCur, &appliesTo,
			&maxUses, &maxPerUser, &startsAt, &expiresAt, &recurring, &enabled)
	if err != nil {
		return Discount{}, fmt.Errorf("coupon: read coupon %s: %w", *couponID, err)
	}

	out := Discount{
		CouponID:    *couponID,
		Code:        strings.ToUpper(code),
		AppliesTo:   appliesTo,
		Cycle:       cyclesDone + 1,
		CyclesTotal: recurring,
	}
	decline := func(reason string) (Discount, error) {
		out.Reason = reason
		return out, nil
	}

	if !enabled {
		return decline("the coupon has been retired")
	}
	if startsAt != nil && at.Before(*startsAt) {
		return decline("the coupon has not started yet")
	}
	if expiresAt != nil && !at.Before(*expiresAt) {
		return decline("the coupon expired on " + expiresAt.UTC().Format("2006-01-02"))
	}
	// The `setup`-only scope has nothing to discount on a renewal: the fee it reduces is charged
	// once, on the invoice that created the service.
	if appliesTo == "setup" {
		return decline("the coupon applies to setup fees only, and a renewal has none")
	}
	// "The first N cycles" is the coupon's whole point, and the counter is what makes it a fact
	// rather than a guess. A lifetime coupon (NULL) never exhausts.
	if recurring != nil && cyclesDone >= *recurring {
		return decline(fmt.Sprintf("the coupon applies to the first %d cycle(s) and has used them", *recurring))
	}
	// A use cap is a promise about total uses. A renewal is a use, so it counts — and it is counted
	// from the redemption rows, the same way checkout counts it, for the same reason: a counter can
	// be raced, a count cannot.
	if maxUses != nil {
		var used int
		if err := q.QueryRow(ctx,
			`SELECT count(*) FROM coupon_redemptions WHERE coupon_id = $1`, *couponID).Scan(&used); err != nil {
			return Discount{}, fmt.Errorf("coupon: count coupon uses: %w", err)
		}
		if used >= *maxUses {
			return decline(fmt.Sprintf("the coupon has reached its limit of %d uses", *maxUses))
		}
	}
	if maxPerUser != nil {
		var used int
		if err := q.QueryRow(ctx,
			`SELECT count(*) FROM coupon_redemptions WHERE coupon_id = $1 AND customer_id = $2`,
			*couponID, customerOf(ctx, q, subscriptionID)).Scan(&used); err != nil {
			return Discount{}, fmt.Errorf("coupon: count customer coupon uses: %w", err)
		}
		if used >= *maxPerUser {
			return decline(fmt.Sprintf("the customer has reached the limit of %d uses", *maxPerUser))
		}
	}

	if baseMinor <= 0 {
		return decline("there is nothing on this invoice to discount")
	}

	var discount int64
	switch kind {
	case "percentage":
		if percentage == nil {
			return decline("the coupon has no percentage")
		}
		// The column stores a percent ("10", "12.5"), not a fraction, exactly as checkout reads it.
		ratio, err := money.ParseRatio(*percentage)
		if err != nil {
			return Discount{}, fmt.Errorf("coupon: coupon percentage %q: %w", *percentage, err)
		}
		percent, err := money.NewRatio(1, 100)
		if err != nil {
			return Discount{}, err
		}
		applied, err := money.ApplyRate(money.New(baseMinor, cur), ratio.Mul(percent), money.RoundHalfUp)
		if err != nil {
			return Discount{}, err
		}
		discount = applied.Minor()
	case "fixed":
		if amountMinor == nil {
			return decline("the coupon has no amount")
		}
		if couponCur == nil || !strings.EqualFold(*couponCur, cur.Code()) {
			// A fixed amount is money in one currency. Converting it would need an exchange rate
			// this product does not have, and pretending 10 EUR is 10 USD is worse than not
			// discounting.
			return decline(fmt.Sprintf("the coupon is issued in %s and this invoice is in %s",
				valueOr(couponCur, "another currency"), cur.Code()))
		}
		discount = *amountMinor
	default:
		return decline("the coupon has an unknown kind")
	}

	// A discount larger than the invoice is clamped: the customer cannot be paid to renew, and the
	// invoice's subtotal must not go negative.
	if discount > baseMinor {
		discount = baseMinor
	}
	if discount <= 0 {
		return decline("the coupon gives no discount on this invoice")
	}
	out.Applies = true
	out.AmountMinor = discount
	return out, nil
}

// ApplyRequest is one discount written onto one draft invoice.
type ApplyRequest struct {
	InvoiceID      string
	SubscriptionID string
	CustomerID     string
	Discount       Discount
	Currency       money.Currency
}

// Applied is what was written, for a caller that wants to record it.
type Applied struct {
	DiscountMinor int64
	Subtotal      money.Amount
	Tax           money.Amount
	Total         money.Amount
	// LineID is the discount line, so a caller can point an audit row at it.
	LineID string
}

// ApplyRenewalDiscount writes the coupon into a draft invoice.
//
// The shape is the same one checkout uses for the first invoice, because a customer comparing two
// documents should see the same two facts arranged the same way:
//
//   - each charge line keeps its catalog **net** and its tax is recomputed on the *discounted*
//     value, so the tax reduction happens exactly once;
//   - one `line_kind = 'discount'` line carries the total reduction, negative and untaxed;
//   - the invoice totals are the sum of the line nets plus the recomputed tax;
//   - the snapshot is rewritten, with a `discount` block naming the coupon and the cycle.
//
// Inclusive tax is handled the way checkout handles it: the quoted price contains the tax, so the
// discount reduces the gross, the tax is its share of the reduced gross, and the net is what is
// left. Using the exclusive formula on an inclusive invoice would reduce the tax by the tax on the
// discount.
func ApplyRenewalDiscount(ctx context.Context, tx pgx.Tx, req ApplyRequest) (Applied, error) {
	if !req.Discount.Applies || req.Discount.AmountMinor <= 0 {
		return Applied{}, ErrNoDiscount
	}
	if req.Discount.CouponID == "" {
		return Applied{}, fmt.Errorf("%w: no coupon to apply", ErrNoDiscount)
	}
	cur := req.Currency
	if !cur.IsValid() {
		return Applied{}, fmt.Errorf("coupon: no currency")
	}

	var (
		status   string
		currency string
		snap     billing.Snapshot
	)
	if err := tx.QueryRow(ctx, `
		SELECT status, currency, snapshot FROM invoices WHERE id = $1 FOR UPDATE`,
		req.InvoiceID).Scan(&status, &currency, &snap); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Applied{}, fmt.Errorf("coupon: invoice %s not found", req.InvoiceID)
		}
		return Applied{}, fmt.Errorf("coupon: read invoice: %w", err)
	}
	if status != "draft" {
		return Applied{}, fmt.Errorf("%w: %s is %s", ErrInvoiceNotDraft, req.InvoiceID, status)
	}
	if !strings.EqualFold(currency, cur.Code()) {
		return Applied{}, fmt.Errorf("%w: invoice is %s, discount is %s",
			billing.ErrCurrencyMismatch, currency, cur.Code())
	}

	// A replay is refused before anything is written. The redemption's unique (coupon_id,
	// invoice_id) index is the fact this reads: if the row exists, this invoice already carries the
	// discount, and applying it again would write a second discount line and advance the counter
	// twice. Checking first means the refusal is a clean error rather than a constraint violation
	// from whichever write happens to collide.
	var alreadyApplied bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM coupon_redemptions WHERE coupon_id = $1 AND invoice_id = $2)`,
		req.Discount.CouponID, req.InvoiceID).Scan(&alreadyApplied); err != nil {
		return Applied{}, fmt.Errorf("coupon: check existing redemption: %w", err)
	}
	if alreadyApplied {
		return Applied{}, fmt.Errorf("%w: invoice %s", ErrAlreadyApplied, req.InvoiceID)
	}

	// The lines the pricer wrote are the authority on net and rate, not the request: a pricer is
	// free to compute net differently from unit × quantity, and the discount must reduce what is
	// actually on the document.
	type storedLine struct {
		id          string
		position    int
		description string
		quantity    string
		unitMinor   int64
		netMinor    int64
		taxRate     string
		taxMinor    int64
		productID   *string
		versionID   *string
	}
	rows, err := tx.Query(ctx, `
		SELECT id, position, description, quantity::text, unit_minor, net_minor,
		       tax_rate, tax_minor, product_id, price_version_id
		  FROM invoice_lines
		 WHERE invoice_id = $1 AND line_kind = 'charge'
		 ORDER BY position`, req.InvoiceID)
	if err != nil {
		return Applied{}, fmt.Errorf("coupon: read draft lines: %w", err)
	}
	var stored []storedLine
	for rows.Next() {
		var l storedLine
		if err := rows.Scan(&l.id, &l.position, &l.description, &l.quantity, &l.unitMinor,
			&l.netMinor, &l.taxRate, &l.taxMinor, &l.productID, &l.versionID); err != nil {
			rows.Close()
			return Applied{}, fmt.Errorf("coupon: scan draft line: %w", err)
		}
		stored = append(stored, l)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return Applied{}, fmt.Errorf("coupon: read draft lines: %w", err)
	}
	rows.Close()
	if len(stored) == 0 {
		return Applied{}, fmt.Errorf("%w: the invoice has no charge lines", ErrNoDiscount)
	}

	inclusive := strings.EqualFold(snap.TaxEvidence.Mode, "inclusive")

	// The split is proportional to each line's value, by largest remainder, so the parts sum exactly
	// to the discount and no minor unit is invented or lost. In inclusive mode the proportion is of
	// the gross, because that is the amount the discount is reducing.
	weights := make([]int64, 0, len(stored))
	for _, l := range stored {
		w := l.netMinor
		if inclusive {
			w += l.taxMinor
		}
		if w <= 0 {
			continue
		}
		weights = append(weights, w)
	}
	if len(weights) == 0 {
		return Applied{}, ErrNoDiscount
	}
	if int64(len(weights)) != int64(len(stored)) {
		// A zero-value charge line cannot absorb a discount; refusing is better than silently
		// spreading the discount over the lines that can, which would not match the amount.
		return Applied{}, fmt.Errorf("%w: the invoice has a line with no value", ErrNoDiscount)
	}
	parts, err := money.Allocate(money.New(req.Discount.AmountMinor, cur), weights)
	if err != nil {
		return Applied{}, fmt.Errorf("coupon: allocate discount: %w", err)
	}

	results := make([]billing.LineResult, 0, len(stored)+1)
	newTaxTotal := int64(0)
	netReduction := int64(0)
	for i, l := range stored {
		share := parts[i].Minor()
		var tax int64
		if inclusive {
			// Reduce the gross first; the tax is its share of the reduced gross and the net is the
			// remainder, so net + tax is exactly the reduced gross.
			gross := l.netMinor + l.taxMinor - share
			tax, err = shareOfGross(gross, l.taxRate, cur)
			if err != nil {
				return Applied{}, err
			}
			netReduction += l.netMinor - (gross - tax)
		} else {
			tax, err = taxOnNet(l.netMinor-share, l.taxRate, cur)
			if err != nil {
				return Applied{}, err
			}
			netReduction += share
		}
		if _, err := tx.Exec(ctx, `
			UPDATE invoice_lines
			   SET tax_minor = $2::bigint, total_minor = net_minor + $2::bigint
			 WHERE id = $1`, l.id, tax); err != nil {
			return Applied{}, fmt.Errorf("coupon: reprice line: %w", err)
		}
		newTaxTotal += tax
		results = append(results, billing.LineResult{
			Description:    l.description,
			QuantityMilli:  quantityMilli(l.quantity),
			UnitMinor:      l.unitMinor,
			Net:            money.New(l.netMinor, cur),
			Tax:            money.New(tax, cur),
			Total:          money.New(l.netMinor+tax, cur),
			TaxRate:        l.taxRate,
			ProductID:      l.productID,
			PriceVersionID: l.versionID,
		})
	}
	if netReduction <= 0 {
		return Applied{}, ErrNoDiscount
	}

	description := "Discount — coupon " + req.Discount.Code
	position := stored[len(stored)-1].position
	var lineID string
	if err := tx.QueryRow(ctx, `
		INSERT INTO invoice_lines (invoice_id, position, description, quantity, unit_minor,
			net_minor, tax_rate, tax_minor, total_minor, line_kind)
		VALUES ($1, $2, $3, 1, $4::bigint, $4::bigint, '0', 0, $4::bigint, 'discount')
		RETURNING id`,
		req.InvoiceID, position+1, description, -netReduction).Scan(&lineID); err != nil {
		return Applied{}, fmt.Errorf("coupon: insert discount line: %w", err)
	}
	results = append(results, billing.LineResult{
		Description:   description,
		QuantityMilli: 1000,
		UnitMinor:     -netReduction,
		Net:           money.New(-netReduction, cur),
		Tax:           money.Zero(cur),
		Total:         money.New(-netReduction, cur),
		TaxRate:       "0",
	})

	// Subtotal is the sum of every line's net, the discount line included.
	newSubtotal := int64(0)
	for _, r := range results {
		newSubtotal += r.Net.Minor()
	}
	totals := billing.Totals{
		Subtotal: money.New(newSubtotal, cur),
		Tax:      money.New(newTaxTotal, cur),
		Total:    money.New(newSubtotal+newTaxTotal, cur),
	}
	if totals.Subtotal.Minor() < 0 || totals.Total.Minor() < 0 {
		return Applied{}, ErrNoDiscount
	}
	snap.Lines = results
	snap.Totals = totals
	snap.Currency = cur.Code()

	if _, err := tx.Exec(ctx, `
		UPDATE invoices SET subtotal_minor = $2, tax_minor = $3, total_minor = $4
		 WHERE id = $1 AND status = 'draft'`,
		req.InvoiceID, totals.Subtotal.Minor(), totals.Tax.Minor(), totals.Total.Minor()); err != nil {
		return Applied{}, fmt.Errorf("coupon: update invoice totals: %w", err)
	}
	if err := writeSnapshot(ctx, tx, req.InvoiceID, snap, req.Discount, netReduction, cur); err != nil {
		return Applied{}, err
	}

	// The redemption is recorded against the invoice it discounted, so `max_uses` stays a count of
	// facts. The unique (coupon_id, invoice_id) index is what makes a replay visible: if the row
	// already exists, this invoice was discounted before, and continuing would discount it twice
	// and advance the counter twice. Refusing is the only safe answer, and the caller's transaction
	// rolls the discount line back.
	tag, err := tx.Exec(ctx, `
		INSERT INTO coupon_redemptions (coupon_id, customer_id, invoice_id, subscription_id,
			discount_minor, currency)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT DO NOTHING`,
		req.Discount.CouponID, req.CustomerID, req.InvoiceID, req.SubscriptionID,
		netReduction, cur.Code())
	if err != nil {
		return Applied{}, fmt.Errorf("coupon: record redemption: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return Applied{}, fmt.Errorf("%w: invoice %s was already discounted by this coupon",
			ErrAlreadyApplied, req.InvoiceID)
	}

	// The counter advances in the same transaction as the invoice it describes. If this row cannot
	// be updated, the invoice must not exist either: a renewal discounted as cycle 2 while the
	// subscription still believes it is on cycle 1 would discount cycle 2 for ever.
	tag, err = tx.Exec(ctx, `
		UPDATE subscriptions
		   SET coupon_cycles_applied = coupon_cycles_applied + 1
		 WHERE id = $1`, req.SubscriptionID)
	if err != nil {
		return Applied{}, fmt.Errorf("coupon: advance coupon cycle: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return Applied{}, fmt.Errorf("coupon: subscription %s disappeared", req.SubscriptionID)
	}

	return Applied{
		DiscountMinor: netReduction,
		Subtotal:      totals.Subtotal,
		Tax:           totals.Tax,
		Total:         totals.Total,
		LineID:        lineID,
	}, nil
}

// writeSnapshot rewrites the draft's snapshot with the discounted lines and the coupon facts.
//
// The snapshot is what makes the document reprintable and its numbers justifiable, so the discount
// must be in it. It is stored as an extra top-level key rather than by widening billing.Snapshot,
// the same way checkout stores its discount block: the snapshot's own schema belongs to the billing
// package, and a reader that does not know the key ignores it.
func writeSnapshot(ctx context.Context, tx pgx.Tx, invoiceID string, snap billing.Snapshot,
	d Discount, netReduction int64, cur money.Currency) error {

	raw, err := json.Marshal(snap)
	if err != nil {
		return fmt.Errorf("coupon: encode snapshot: %w", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("coupon: decode snapshot: %w", err)
	}
	block := map[string]any{
		"coupon_id":    d.CouponID,
		"code":         d.Code,
		"amount_minor": netReduction,
		"currency":     cur.Code(),
		"applies_to":   d.AppliesTo,
		"cycle":        d.Cycle,
		"tax_note": "the discount line carries no tax; each charge line's tax is " +
			"computed on its discounted net",
	}
	if d.CyclesTotal != nil {
		block["cycles_total"] = *d.CyclesTotal
	}
	doc["discount"] = block
	if _, err := tx.Exec(ctx,
		`UPDATE invoices SET snapshot = $2 WHERE id = $1 AND status = 'draft'`,
		invoiceID, doc); err != nil {
		return fmt.Errorf("coupon: update snapshot: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------- helpers --

// taxOnNet returns tax as an exact share of a net amount, for exclusive mode. A rate the pricer
// wrote that cannot be parsed is a bug in the pricer, so it fails rather than being treated as zero.
func taxOnNet(net int64, rate string, cur money.Currency) (int64, error) {
	ratio, err := money.ParseRatio(rate)
	if err != nil {
		return 0, fmt.Errorf("coupon: unreadable tax rate %q: %w", rate, err)
	}
	if ratio.IsZero() {
		return 0, nil
	}
	tax, err := money.ApplyRate(money.New(net, cur), ratio, money.RoundHalfUp)
	if err != nil {
		return 0, err
	}
	return tax.Minor(), nil
}

// shareOfGross returns the tax contained in a gross amount, for inclusive mode.
func shareOfGross(gross int64, rate string, cur money.Currency) (int64, error) {
	ratio, err := money.ParseRatio(rate)
	if err != nil {
		return 0, fmt.Errorf("coupon: unreadable tax rate %q: %w", rate, err)
	}
	if ratio.IsZero() {
		return 0, nil
	}
	one := money.One()
	denom, err := money.RatioFromRat(new(money.Rat).Add(ratio.Rat(), one.Rat()))
	if err != nil {
		return 0, err
	}
	share, err := ratio.Divide(denom)
	if err != nil {
		return 0, err
	}
	tax, err := money.ApplyRate(money.New(gross, cur), share, money.RoundHalfUp)
	if err != nil {
		return 0, err
	}
	return tax.Minor(), nil
}

// quantityMilli parses the stored quantity ("2", "1.5") back into thousandths.
func quantityMilli(stored string) int64 {
	whole, fraction, hasFraction := strings.Cut(strings.TrimSpace(stored), ".")
	value := int64(0)
	for _, r := range whole {
		if r < '0' || r > '9' {
			return 1000
		}
		value = value*10 + int64(r-'0')
	}
	value *= 1000
	if !hasFraction {
		return value
	}
	scale := int64(100)
	for _, r := range fraction {
		if r < '0' || r > '9' || scale == 0 {
			break
		}
		value += int64(r-'0') * scale
		scale /= 10
	}
	return value
}

func valueOr(v *string, fallback string) string {
	if v == nil || *v == "" {
		return fallback
	}
	return *v
}

// customerOf reads the subscription's customer, for the per-customer use count.
func customerOf(ctx context.Context, q Querier, subscriptionID string) string {
	var id string
	if err := q.QueryRow(ctx,
		`SELECT customer_id FROM subscriptions WHERE id = $1`, subscriptionID).Scan(&id); err != nil {
		return ""
	}
	return id
}

// SubscriptionCoupon is one subscription carrying a coupon, as the promotion screen shows it.
//
// The screen needs this because "the coupon discounted the first invoice" and "the coupon will keep
// discounting this subscription" are different states of the same row, and the difference is the
// counter compared to the coupon's window. Reading it from the subscription is what makes the
// promise visible instead of inferred.
type SubscriptionCoupon struct {
	SubscriptionID string `json:"subscription_id"`
	ServiceID      string `json:"service_id,omitempty"`
	CustomerID     string `json:"customer_id"`
	CustomerEmail  string `json:"customer_email"`
	ProductName    string `json:"product_name"`
	Status         string `json:"status"`
	CyclesApplied  int    `json:"cycles_applied"`
	// CyclesTotal is the coupon's `recurring_cycles`; nil means every cycle.
	CyclesTotal *int `json:"cycles_total"`
	// Remaining is CyclesTotal - CyclesApplied, nil for a lifetime coupon. It is computed here so
	// the screen cannot invent a different answer from the same two numbers.
	Remaining *int      `json:"remaining"`
	CreatedAt time.Time `json:"created_at"`
}

// SubscriptionsCarrying lists the subscriptions a coupon is attached to, newest first.
func SubscriptionsCarrying(ctx context.Context, q Querier, couponID string) ([]SubscriptionCoupon, error) {
	rows, err := q.Query(ctx, `
		SELECT s.id, COALESCE(sv.id::text, ''), s.customer_id, cu.email, p.name, s.status,
		       s.coupon_cycles_applied, cp.recurring_cycles, s.created_at
		  FROM subscriptions s
		  JOIN customers cu ON cu.id = s.customer_id
		  JOIN price_versions pv ON pv.id = s.price_version_id
		  JOIN products p ON p.id = pv.product_id
		  LEFT JOIN services sv ON sv.subscription_id = s.id
		  LEFT JOIN coupons cp ON cp.id = s.coupon_id
		 WHERE s.coupon_id = $1
		 ORDER BY s.created_at DESC`, couponID)
	if err != nil {
		return nil, fmt.Errorf("coupon: list subscriptions carrying coupon: %w", err)
	}
	defer rows.Close()
	out := make([]SubscriptionCoupon, 0, 8)
	for rows.Next() {
		var c SubscriptionCoupon
		if err := rows.Scan(&c.SubscriptionID, &c.ServiceID, &c.CustomerID, &c.CustomerEmail,
			&c.ProductName, &c.Status, &c.CyclesApplied, &c.CyclesTotal, &c.CreatedAt); err != nil {
			return nil, fmt.Errorf("coupon: scan subscription carrying coupon: %w", err)
		}
		if c.CyclesTotal != nil {
			remaining := *c.CyclesTotal - c.CyclesApplied
			if remaining < 0 {
				remaining = 0
			}
			c.Remaining = &remaining
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
