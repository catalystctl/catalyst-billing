// Plan intervals at checkout: which lines get a billing schedule, and which do not.
//
// This is one file rather than a few lines inside checkout.go because it is the piece of the
// purchase path that `one_time` and `free` prices added, and keeping it separate is what lets the
// rest of checkout.go stay exactly as it was for a recurring product.
//
// # The rule
//
// A price version either recurs or it does not (catalog.Recurring). A recurring line creates a
// subscription, as it always has. A one-time or free line creates **no subscription at all** —
// not a placeholder, not a subscription with a zero next-bill date, not one that the renewal
// engine is expected to skip. The absence is the enforcement: the renewal scanner reads
// `subscriptions`, dunning joins through `subscriptions`, and the portal reads its next-bill date
// from `subscriptions`, so a row that does not exist cannot be renewed, cannot justify a
// suspension for an invoice that cannot recur, and cannot make the portal promise a date that
// will never arrive. `services.subscription_id` has been nullable since 0001, so the service is
// written with NULL and nothing had to be relaxed.
//
// # Why the times are returned rather than computed here
//
// `periodFor` is unchanged. It still derives the first period from the price's anchor in the
// price's timezone, and it still returns the same two instants for monthly, quarterly, semiannual
// and annual as it did before this file existed. A one-time or free line never reaches it.
package register

import (
	"time"

	"github.com/catalyst/billing/internal/catalog"
)

// planSchedule decides what a cart line's billing schedule is.
//
// It returns `recurring == false` for a one-time or free price, in which case the times are zero
// and the caller must not write a subscription. For a recurring price it returns exactly what
// `periodFor` returns today, so no existing behaviour moves.
func planSchedule(cl catalogLine, now time.Time) (start, end time.Time, recurring bool, err error) {
	if !catalog.Recurring(cl.interval) {
		return time.Time{}, time.Time{}, false, nil
	}
	start, end, err = periodFor(cl, now)
	if err != nil {
		return time.Time{}, time.Time{}, false, err
	}
	return start, end, true, nil
}

// firstID returns the first subscription an order created, or "" when it created none.
//
// The invoice's `subscription_id` and a coupon redemption's `subscription_id` both name the
// schedule an order is billed on. An order of one-time lines has no such schedule, so both are
// written as NULL (the columns are nullable for this case); a mixed cart links the invoice to the
// first schedule it did create, which is what dunning and the renewal engine need in order to
// find the recurring part of the order.
func firstID(ids []string) string {
	for _, id := range ids {
		if id != "" {
			return id
		}
	}
	return ""
}

// invoicePeriodKey is the invoice's period key, or "" when the invoice is not part of a billing
// series.
//
// `invoices_one_per_period` is unique on (subscription_id, period_key) where both are non-null, so
// an invoice with no subscription is outside that index entirely. Writing a period key on it
// anyway would be a claim that this document recurs, which is exactly what a one-time purchase is
// not.
func invoicePeriodKey(subscriptionID string, now time.Time) string {
	if subscriptionID == "" {
		return ""
	}
	return now.UTC().Format("2006-01-02")
}

// compactIDs drops the empty entries a non-recurring line leaves in a per-line id slice, so a
// statement that takes an array of ids never receives one that names nothing.
func compactIDs(ids []string) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if id != "" {
			out = append(out, id)
		}
	}
	return out
}
