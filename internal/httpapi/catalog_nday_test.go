package httpapi_test

// The API surface of a day-count plan, exercised the way the catalog screen calls it.
//
// The calendar tests prove the arithmetic and the renewal tests prove the billing. This file
// proves the third thing an operator depends on: that a plan sold as "every 45 days" can be
// created through the endpoint the product screen uses, comes back with a label a human can
// read, and is refused when the day count is one the calendar cannot schedule.

import (
	"context"
	"net/http"
	"testing"
)

func TestCreateDayCountPriceThroughTheAPI(t *testing.T) {
	e := newStaffEnv(t)
	_, productID := e.seedCatalog(t, "nday-api", false)

	rec := e.doJSON(t, http.MethodPost, "/api/v1/staff/catalog/products/"+productID+"/prices",
		e.admin, e.adminCSRF, map[string]any{
			"name":            "Every 45 days",
			"currency":        "USD",
			"amount_minor":    4500,
			"setup_minor":     0,
			"interval":        "every_45_days",
			"anchor_day":      20,
			"anchor_timezone": "Europe/Berlin",
		})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create day-count price = %d: %s", rec.Code, rec.Body)
	}

	// The create response is the raw price row (amount_minor, no label) — the same shape it has
	// always returned for every interval. What matters here is that the row was stored as a
	// day-count period with the anchor it was given; the screen renders from the product read
	// below, which is the shape that carries the label.
	type createdPrice struct {
		ID        string `json:"id"`
		Interval  string `json:"interval"`
		AnchorDay int    `json:"anchor_day"`
	}
	created := decodeBody[createdPrice](t, rec)
	if created.Interval != "every_45_days" {
		t.Fatalf("interval = %q, want every_45_days", created.Interval)
	}
	if created.AnchorDay != 20 {
		t.Fatalf("anchor_day = %d, want 20", created.AnchorDay)
	}

	// The product read the screen performs returns the plan with a label a human can read and
	// the recurring flag, so a client with no interval table of its own still renders it
	// correctly.
	read := e.get(t, "/api/v1/staff/catalog/products/"+productID, e.admin)
	if read.Code != http.StatusOK {
		t.Fatalf("read product = %d: %s", read.Code, read.Body)
	}
	body := decodeBody[struct {
		Prices []struct {
			Interval      string `json:"interval"`
			IntervalLabel string `json:"interval_label"`
			Recurring     bool   `json:"recurring"`
		} `json:"prices"`
	}](t, read)
	var found bool
	for _, p := range body.Prices {
		if p.Interval == "every_45_days" {
			found = true
			if p.IntervalLabel != "every 45 days" {
				t.Fatalf("read-back label = %q, want \"every 45 days\"", p.IntervalLabel)
			}
			if !p.Recurring {
				t.Fatal("read-back recurring = false; a day-count plan renews")
			}
		}
	}
	if !found {
		t.Fatalf("the created plan is missing from the product read: %s", read.Body)
	}
}

// A day count the calendar cannot schedule is refused with a validation error, not stored and
// then discovered by the renewal engine.
func TestCreateDayCountPriceRefusesAnImpossiblePeriod(t *testing.T) {
	e := newStaffEnv(t)
	_, productID := e.seedCatalog(t, "nday-bad", false)

	for _, bad := range []string{"every_0_days", "every_367_days", "every_045_days", "every_days", "weekly"} {
		rec := e.doJSON(t, http.MethodPost, "/api/v1/staff/catalog/products/"+productID+"/prices",
			e.admin, e.adminCSRF, map[string]any{
				"name":            "Bad period",
				"currency":        "USD",
				"amount_minor":    1000,
				"interval":        bad,
				"anchor_day":      1,
				"anchor_timezone": "UTC",
			})
		if rec.Code != http.StatusBadRequest && rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("interval %q = %d, want a 4xx validation error: %s", bad, rec.Code, rec.Body)
		}
	}

	// Nothing was stored.
	var n int
	if err := e.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM price_versions WHERE product_id = $1 AND interval LIKE 'every%'`,
		productID).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("%d day-count prices were stored from invalid input, want 0", n)
	}
}
