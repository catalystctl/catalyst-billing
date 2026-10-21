package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/catalyst/billing/internal/billing"
	"github.com/catalyst/billing/internal/money"
	"github.com/catalyst/billing/internal/portal"
	"github.com/jackc/pgx/v5"
)

var usdPortal = money.MustLookupCurrency("USD")

// tenantFixture creates a customer with a login, service and invoice.
type tenantFixture struct {
	userID     string
	customerID string
	serviceID  string
	invoiceID  string
}

func (e *authEnv) createTenant(t *testing.T, email, amount string) tenantFixture {
	t.Helper()
	ctx := context.Background()
	var f tenantFixture

	hash, err := e.hasher.Hash("a long enough password")
	if err != nil {
		t.Fatal(err)
	}

	err = e.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `
			INSERT INTO users (email, password_hash, display_name, email_verified_at)
			VALUES ($1, $2, 'Tenant', now()) RETURNING id`, email, hash).Scan(&f.userID); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx,
			`INSERT INTO customers (email, name) VALUES ($1, 'Tenant') RETURNING id`, email).
			Scan(&f.customerID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO customer_accounts (user_id, customer_id, role) VALUES ($1, $2, 'owner')`,
			f.userID, f.customerID); err != nil {
			return err
		}

		var productID, priceID, subID string
		if err := tx.QueryRow(ctx, `
			INSERT INTO products (slug, name) VALUES ($1, 'VPS Small') RETURNING id`,
			"vps-"+f.customerID[:8]).Scan(&productID); err != nil {
			return err
		}
		price := money.MustParse(amount, usdPortal)
		if err := tx.QueryRow(ctx, `
			INSERT INTO price_versions (product_id, version, currency, amount_minor, interval, anchor_day, anchor_timezone)
			VALUES ($1, 1, 'USD', $2, 'monthly', 1, 'UTC') RETURNING id`, productID, price.Minor()).
			Scan(&priceID); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `
			INSERT INTO subscriptions (customer_id, price_version_id, status, interval, anchor_day,
				timezone, current_period_start, current_period_end, next_billing_at)
			VALUES ($1, $2, 'active', 'monthly', 1, 'UTC', now(), now() + interval '30 days', now() + interval '30 days')
			RETURNING id`, f.customerID, priceID).Scan(&subID); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `
			INSERT INTO services (customer_id, subscription_id, product_id, status, generation)
			VALUES ($1, $2, $3, 'active', 1) RETURNING id`,
			f.customerID, subID, productID).Scan(&f.serviceID); err != nil {
			return err
		}

		issuer := billing.NewIssuer(billing.FixedTaxPricer{
			Rate: money.MustParseRatio("0%"), Rounding: money.RoundHalfUp, Policy: "none",
		})
		id, _, _, err := issuer.CreateDraft(ctx, tx, billing.IssueRequest{
			CustomerID:     f.customerID,
			SubscriptionID: subID,
			PeriodKey:      "p1",
			Lines: []billing.Line{{
				Description: "VPS Small monthly", QuantityMilli: 1000, UnitPrice: price,
			}},
			Customer: billing.CustomerDetails{ID: f.customerID},
		})
		if err != nil {
			return err
		}
		issued, err := issuer.Issue(ctx, tx, id, billing.DefaultSeries)
		if err != nil {
			return err
		}
		f.invoiceID = issued.InvoiceID
		return nil
	})
	if err != nil {
		t.Fatalf("create tenant %s: %v", email, err)
	}
	return f
}

func (e *authEnv) do(t *testing.T, method, path string, cookie *http.Cookie, csrf string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	if cookie != nil {
		req.AddCookie(cookie)
	}
	if csrf != "" {
		req.Header.Set("X-CSRF-Token", csrf)
	}
	rec := httptest.NewRecorder()
	e.srv.Handler().ServeHTTP(rec, req)
	return rec
}

// Acceptance scenario 10 over HTTP: customer A cannot reach customer B's records by
// substituting identifiers in the URL.
func TestPortalCrossTenantHTTP(t *testing.T) {
	env := newAuthEnv(t)
	env.srv.WithPortal(portal.New(env.pool))

	alice := env.createTenant(t, "alice@example.test", "12.00")
	bob := env.createTenant(t, "bob@example.test", "25.00")

	aliceCookie, aliceCSRF := env.login(t, "alice@example.test", "a long enough password")

	// Her own records work.
	if rec := env.do(t, http.MethodGet, "/api/v1/portal/invoices/"+alice.invoiceID, aliceCookie, ""); rec.Code != http.StatusOK {
		t.Fatalf("alice's own invoice: %d %s", rec.Code, rec.Body)
	}
	if rec := env.do(t, http.MethodGet, "/api/v1/portal/services/"+alice.serviceID, aliceCookie, ""); rec.Code != http.StatusOK {
		t.Fatalf("alice's own service: %d %s", rec.Code, rec.Body)
	}

	// Bob's must be indistinguishable from a nonexistent id.
	rec := env.do(t, http.MethodGet, "/api/v1/portal/invoices/"+bob.invoiceID, aliceCookie, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("alice reading bob's invoice: %d, want 404", rec.Code)
	}
	bobBody := rec.Body.String()

	rec = env.do(t, http.MethodGet, "/api/v1/portal/invoices/00000000-0000-0000-0000-000000000000", aliceCookie, "")
	if rec.Body.String() != bobBody {
		t.Fatalf("the response distinguishes a foreign record from a missing one:\n%s\nvs\n%s",
			bobBody, rec.Body.String())
	}
	if rec := env.do(t, http.MethodGet, "/api/v1/portal/services/"+bob.serviceID, aliceCookie, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("alice reading bob's service: %d, want 404", rec.Code)
	}

	// Alice's list contains only her own invoice.
	rec = env.do(t, http.MethodGet, "/api/v1/portal/invoices", aliceCookie, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list: %d", rec.Code)
	}
	var body struct {
		Invoices []struct {
			ID string `json:"id"`
		} `json:"invoices"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Invoices) != 1 || body.Invoices[0].ID != alice.invoiceID {
		t.Fatalf("alice sees %d invoices: %+v", len(body.Invoices), body.Invoices)
	}

	// And cancelling Bob's service is refused without touching it. The CSRF token
	// must be the one bound to this session: a token from a different login is a
	// different secret.
	if rec := env.do(t, http.MethodPost, "/api/v1/portal/services/"+bob.serviceID+"/cancel", aliceCookie, aliceCSRF); rec.Code != http.StatusNotFound {
		t.Fatalf("alice cancelling bob's service: %d, want 404; body %s", rec.Code, rec.Body)
	}
}

// The portal requires a session: identity is never taken from the URL.
func TestPortalRequiresAuthentication(t *testing.T) {
	env := newAuthEnv(t)
	env.srv.WithPortal(portal.New(env.pool))

	for _, path := range []string{
		"/api/v1/portal/overview",
		"/api/v1/portal/invoices",
		"/api/v1/portal/services",
	} {
		if rec := env.do(t, http.MethodGet, path, nil, ""); rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s anonymous: %d, want 401", path, rec.Code)
		}
	}
}

// Cancelling requires a CSRF token, and the response states the consequence.
func TestPortalCancelRequiresCSRFAndStatesConsequence(t *testing.T) {
	env := newAuthEnv(t)
	env.srv.WithPortal(portal.New(env.pool))
	tenant := env.createTenant(t, "alice@example.test", "12.00")
	cookie, csrf := env.login(t, "alice@example.test", "a long enough password")

	path := "/api/v1/portal/services/" + tenant.serviceID + "/cancel"

	if rec := env.do(t, http.MethodPost, path, cookie, ""); rec.Code != http.StatusForbidden {
		t.Fatalf("cancel without CSRF: %d, want 403", rec.Code)
	}

	rec := env.do(t, http.MethodPost, path, cookie, csrf)
	if rec.Code != http.StatusOK {
		t.Fatalf("cancel: %d %s", rec.Code, rec.Body)
	}
	var body struct {
		Status  string `json:"status"`
		EndsAt  string `json:"ends_at"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Status != "cancellation_scheduled" {
		t.Fatalf("status = %q", body.Status)
	}
	if body.EndsAt == "" {
		t.Fatal("the response does not say when the service ends")
	}
	if body.Message == "" {
		t.Fatal("the response does not state that the service stays active until the period ends")
	}
}

// The overview reports money as integer minor units, never as a float.
func TestPortalAmountsAreIntegerMinorUnits(t *testing.T) {
	env := newAuthEnv(t)
	env.srv.WithPortal(portal.New(env.pool))
	env.createTenant(t, "alice@example.test", "12.34")
	cookie, _ := env.login(t, "alice@example.test", "a long enough password")

	rec := env.do(t, http.MethodGet, "/api/v1/portal/overview", cookie, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("overview: %d %s", rec.Code, rec.Body)
	}
	var body struct {
		Balance struct {
			Outstanding []struct {
				Minor    int64  `json:"minor"`
				Currency string `json:"currency"`
				Display  string `json:"display"`
			} `json:"outstanding"`
		} `json:"balance"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Balance.Outstanding) != 1 {
		t.Fatalf("outstanding = %+v", body.Balance.Outstanding)
	}
	if got := body.Balance.Outstanding[0].Minor; got != 1234 {
		t.Fatalf("minor = %d, want 1234", got)
	}
	if got := body.Balance.Outstanding[0].Display; got != "12.34 USD" {
		t.Fatalf("display = %q", got)
	}
}
