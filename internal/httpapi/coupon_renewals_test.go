package httpapi_test

// The promotion's renewal view, through the router: the one read that tells an operator how much
// life a "first N cycles" coupon still has.

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

func TestCouponSubscriptionsShowRemainingCycles(t *testing.T) {
	env := newStaffEnv(t)
	ctx := context.Background()

	rec := env.doJSON(t, http.MethodPost, "/api/v1/staff/coupons", env.billing, env.billingCSRF,
		map[string]any{"code": "THREE", "kind": "percentage", "percentage": "10", "recurring_cycles": 3})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create coupon: %d %s", rec.Code, rec.Body)
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}

	customerID, _ := env.seedCommerceCustomer(t, "renewing-coupon@example.test")
	productID, priceID := env.seedPrice(t, "coupon-renewal-vps", 10000, 0)
	_ = productID
	var subscriptionID string
	if err := env.pool.QueryRow(ctx, `
		INSERT INTO subscriptions (customer_id, price_version_id, status, interval, anchor_day,
			timezone, current_period_start, current_period_end, next_billing_at,
			coupon_id, coupon_cycles_applied)
		VALUES ($1, $2, 'active', 'monthly', 1, 'UTC', now() - interval '1 month', now(),
		        now() + interval '1 month', $3, 1)
		RETURNING id`, customerID, priceID, created.ID).Scan(&subscriptionID); err != nil {
		t.Fatalf("create subscription: %v", err)
	}

	// Support holds no coupon:read:all, so the read is a permission, not a staff area.
	rec = env.get(t, "/api/v1/staff/coupons/"+created.ID+"/subscriptions", env.support)
	if code := commerceProblem(t, rec, http.StatusForbidden); code != "forbidden" {
		t.Fatalf("code = %q, want forbidden", code)
	}

	rec = env.get(t, "/api/v1/staff/coupons/"+created.ID+"/subscriptions", env.billing)
	if rec.Code != http.StatusOK {
		t.Fatalf("read subscriptions: %d %s", rec.Code, rec.Body)
	}
	var body struct {
		Data []struct {
			SubscriptionID string `json:"subscription_id"`
			CyclesApplied  int    `json:"cycles_applied"`
			CyclesTotal    *int   `json:"cycles_total"`
			Remaining      *int   `json:"remaining"`
			ProductName    string `json:"product_name"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Data) != 1 || body.Data[0].SubscriptionID != subscriptionID {
		t.Fatalf("data = %+v, want the one subscription carrying the coupon", body.Data)
	}
	if body.Data[0].CyclesApplied != 1 || body.Data[0].Remaining == nil || *body.Data[0].Remaining != 2 {
		t.Fatalf("row = %+v, want 1 applied and 2 remaining", body.Data[0])
	}

	// A malformed id is a 404 rather than a cast error surfacing as a 500.
	rec = env.get(t, "/api/v1/staff/coupons/not-a-uuid/subscriptions", env.billing)
	if code := commerceProblem(t, rec, http.StatusNotFound); code != "not_found" {
		t.Fatalf("code = %q, want not_found", code)
	}
}
