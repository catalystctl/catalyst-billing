package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/catalyst/billing/internal/growth"
	"github.com/catalyst/billing/web"
	"github.com/jackc/pgx/v5"
)

// Referral attribution, end to end.
//
// The affiliate programme's arithmetic is already proven in internal/growth: exact rates,
// idempotent commissions, payout reconciliation. What these tests prove is that a signup
// ever *reaches* it. Before them growth.AttributeSignup had no caller and ?ref= was
// dropped on the floor, so every one of those properties was true of a system nobody
// could enter: the storefront took the code, the register form never heard of it, and no
// affiliate was ever paid for a customer they brought.

// storefront attaches the embedded SPA so the document route — the request a referral link
// actually points at — exists. A server without it answers 404 at "/" by design.
func storefront(t *testing.T, e *authEnv) *authEnv {
	t.Helper()
	h, err := web.New(false)
	if err != nil {
		t.Fatalf("web handler: %v", err)
	}
	e.srv.WithWeb(h)
	return e
}

// arrive is one click on a referral link: the storefront document with the code on the
// query string. It returns the cookies the browser is left holding afterwards, which is
// the whole point — the code has to survive the browsing that precedes registration.
func arrive(t *testing.T, e *authEnv, target string) []*http.Cookie {
	t.Helper()
	rec := httptest.NewRecorder()
	e.srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("storefront %s: status %d, body %s", target, rec.Code, rec.Body)
	}
	return rec.Result().Cookies()
}

// registerWith posts a registration carrying the cookies the browser accumulated.
func (e *authEnv) registerWith(t *testing.T, body map[string]any, cookies []*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/register", bytes.NewReader(encoded))
	req.Header.Set("Content-Type", "application/json")
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	e.srv.Handler().ServeHTTP(rec, req)
	return rec
}

// referralCookie returns the attribution cookie in a response, if any.
func referralCookie(cookies []*http.Cookie) *http.Cookie {
	for _, c := range cookies {
		if c.Name == "billing_ref" {
			return c
		}
	}
	return nil
}

// referralCookieValue builds the value the storefront would have stored for a code, so a
// test can present a cookie without walking the landing page that sets it. A capture time
// is part of the value because the server enforces the referral window itself.
func referralCookieValue(code string) string {
	return fmt.Sprintf("%d:%s", time.Now().UTC().Unix(), code)
}

// staleReferralCookieValue is the same value captured beyond the attribution window.
func staleReferralCookieValue(code string) string {
	captured := time.Now().UTC().Add(-(31 * 24 * time.Hour))
	return fmt.Sprintf("%d:%s", captured.Unix(), code)
}

// attribution is the referral row written for a customer, if any.
type attribution struct {
	affiliateID string
	orderID     string
	status      string
}

// attributionOf reads the signup attribution for an address.
func (e *authEnv) attributionOf(t *testing.T, email string) (attribution, bool) {
	t.Helper()
	var a attribution
	err := e.pool.QueryRow(context.Background(), `
		SELECT r.affiliate_id, coalesce(r.order_id::text, ''), r.status
		FROM affiliate_referrals r
		JOIN customers c ON c.id = r.customer_id
		WHERE lower(c.email) = $1 AND r.order_id IS NULL`, strings.ToLower(email),
	).Scan(&a.affiliateID, &a.orderID, &a.status)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return attribution{}, false
		}
		t.Fatalf("read attribution: %v", err)
	}
	return a, true
}

// customerID returns the billing account for an address.
func (e *authEnv) customerID(t *testing.T, email string) string {
	t.Helper()
	var id string
	if err := e.pool.QueryRow(context.Background(),
		`SELECT id FROM customers WHERE lower(email) = $1`, strings.ToLower(email)).Scan(&id); err != nil {
		t.Fatalf("customer for %s: %v", email, err)
	}
	return id
}

// payAnOrder gives the customer a paid order and its paid invoice, then lets the payment
// path record the commission. This is the money the affiliate is owed, arrived at the way
// production arrives at it.
func (e *authEnv) payAnOrder(t *testing.T, customerID string, totalMinor int64) growth.Referral {
	t.Helper()
	ctx := context.Background()
	var (
		orderID   string
		invoiceID string
	)
	if err := e.pool.QueryRow(ctx, `
		INSERT INTO orders (customer_id, status, currency, total_minor)
		VALUES ($1, 'paid', 'USD', $2) RETURNING id`, customerID, totalMinor).Scan(&orderID); err != nil {
		t.Fatalf("order: %v", err)
	}
	if err := e.pool.QueryRow(ctx, `
		INSERT INTO invoices (number, customer_id, order_id, status, currency,
			subtotal_minor, tax_minor, total_minor, amount_paid_minor, issued_at, snapshot)
		VALUES ('INV-REFERRAL', $1, $2, 'paid', 'USD', $3, 0, $3, $3, now(),
		        '{"schema_version":1}'::jsonb)
		RETURNING id`, customerID, orderID, totalMinor).Scan(&invoiceID); err != nil {
		t.Fatalf("invoice: %v", err)
	}

	var referral growth.Referral
	if err := e.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		referral, err = growth.RecordCommissionForPaidInvoice(ctx, tx, invoiceID)
		return err
	}); err != nil {
		t.Fatalf("record commission: %v", err)
	}
	return referral
}

// A visitor who arrives through ?ref= is linked to the affiliate, and the affiliate's
// commission base includes the order that customer goes on to pay.
func TestSignupThroughAReferralLinkEarnsTheAffiliate(t *testing.T) {
	lowerRegisterCost(t)
	env := storefront(t, newAuthEnv(t))
	affiliateID, _ := env.seedAffiliate(t, "PARTNER10", "1/10")

	cookies := arrive(t, env, "/?ref=partner10")
	rec := env.registerWith(t, map[string]any{
		"email":        "referred@example.test",
		"password":     "correct horse battery",
		"display_name": "Referred Customer",
	}, cookies)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("register: status %d, body %s", rec.Code, rec.Body)
	}

	// The link between the signup and the affiliate is a row, written with the account.
	got, ok := env.attributionOf(t, "referred@example.test")
	if !ok {
		t.Fatal("registering with ?ref=PARTNER10 created no attribution")
	}
	if got.affiliateID != affiliateID {
		t.Fatalf("attributed to affiliate %s, want %s", got.affiliateID, affiliateID)
	}
	if got.status != "pending" {
		t.Fatalf("attribution status = %q, want pending", got.status)
	}

	// And the commission base includes it: 10% of 12345 is 1234.5, rounded half-up once,
	// on the exact product.
	commission := env.payAnOrder(t, env.customerID(t, "referred@example.test"), 12345)
	if commission.Commission.Minor() != 1235 {
		t.Fatalf("commission = %d, want 1235 (10%% of 12345, half-up)", commission.Commission.Minor())
	}

	affiliate, err := growth.New(env.pool).Get(context.Background(), affiliateID)
	if err != nil {
		t.Fatalf("read affiliate: %v", err)
	}
	if len(affiliate.Balances) != 1 || affiliate.Balances[0].Owed.Minor() != 1235 {
		t.Fatalf("affiliate balances = %+v, want 1235 USD owed", affiliate.Balances)
	}
	if affiliate.ReferredCount != 1 {
		t.Fatalf("referred count = %d, want 1", affiliate.ReferredCount)
	}

	// The code has been spent: the browser must not carry it into a second signup.
	if cleared := referralCookie(rec.Result().Cookies()); cleared == nil || cleared.MaxAge >= 0 {
		t.Fatalf("referral cookie after attribution = %+v, want it expired", cleared)
	}
}

// A code that resolves to nobody is the visitor's bad link, not their fault. It must not
// cost them the account, and it must not quietly attribute them to anybody either.
func TestUnknownReferralCodeStillRegistersAndAttributesNobody(t *testing.T) {
	lowerRegisterCost(t)
	env := storefront(t, newAuthEnv(t))
	env.seedAffiliate(t, "PARTNER10", "1/10")

	cookies := arrive(t, env, "/?ref=PARTNR10")
	rec := env.registerWith(t, map[string]any{
		"email":        "typo@example.test",
		"password":     "correct horse battery",
		"display_name": "Typo Victim",
	}, cookies)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("register with an unknown code: status %d, body %s", rec.Code, rec.Body)
	}

	// The answer is the one a registration with no code gets: same status, same body, no
	// hint that a code was presented or that it was refused.
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body: %v\n%s", err, rec.Body)
	}
	if body["status"] != "accepted" || body["verification_required"] != false {
		t.Fatalf("body = %v, want the ordinary accepted response", body)
	}

	// The account exists.
	var users int
	if err := env.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM users WHERE lower(email) = 'typo@example.test'`).Scan(&users); err != nil {
		t.Fatalf("count users: %v", err)
	}
	if users != 1 {
		t.Fatalf("users = %d, want the account to have been created", users)
	}

	if _, ok := env.attributionOf(t, "typo@example.test"); ok {
		t.Fatal("an unknown referral code created an attribution")
	}

	// A refused code has had its answer, so the visitor stops carrying it.
	if cleared := referralCookie(rec.Result().Cookies()); cleared == nil || cleared.MaxAge >= 0 {
		t.Fatalf("referral cookie after a refused code = %+v, want it expired", cleared)
	}
}

// The storefront is the only place the code is seen before registration, so it has to keep
// it — and only when it could be one.
func TestStorefrontKeepsAReferralCode(t *testing.T) {
	env := storefront(t, newAuthEnv(t))

	kept := referralCookie(arrive(t, env, "/?ref=partner10"))
	if kept == nil {
		t.Fatal("the storefront dropped ?ref=; registration can never attribute the signup")
	}
	if !strings.HasSuffix(kept.Value, ":PARTNER10") {
		t.Fatalf("stored code = %q, want it to end in :PARTNER10 (normalised)", kept.Value)
	}
	if kept.MaxAge <= 0 {
		t.Fatalf("referral cookie MaxAge = %d, want a positive window", kept.MaxAge)
	}
	// The browser carries it to the registration POST without any script reading it, and
	// it must not ride along on a cross-site POST.
	if !kept.HttpOnly {
		t.Fatal("referral cookie is readable by script; only the server needs it")
	}
	if kept.SameSite != http.SameSiteLaxMode {
		t.Fatalf("referral cookie SameSite = %v, want Lax", kept.SameSite)
	}

	// A visitor who did not come through a link is not given one, and a value that cannot
	// be a code is not copied onto the wire.
	if c := referralCookie(arrive(t, env, "/")); c != nil {
		t.Fatalf("plain visit set a referral cookie: %+v", c)
	}
	long := "/?ref=" + strings.Repeat("a", 65)
	if c := referralCookie(arrive(t, env, long)); c != nil {
		t.Fatalf("an over-long ?ref= was stored: %+v", c)
	}
}

// A client that carries the code itself does not have to know about the cookie.
func TestRegisterAcceptsTheCodeInTheBody(t *testing.T) {
	lowerRegisterCost(t)
	env := newAuthEnv(t)
	affiliateID, _ := env.seedAffiliate(t, "PARTNER10", "1/10")

	rec := env.registerWith(t, map[string]any{
		"email":         "posted@example.test",
		"password":      "correct horse battery",
		"display_name":  "Posted Code",
		"referral_code": "PARTNER10",
	}, nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("register: status %d, body %s", rec.Code, rec.Body)
	}
	got, ok := env.attributionOf(t, "posted@example.test")
	if !ok {
		t.Fatal("referral_code in the body created no attribution")
	}
	if got.affiliateID != affiliateID {
		t.Fatalf("attributed to %s, want %s", got.affiliateID, affiliateID)
	}
}

// A referral window that is only enforced by the browser is not enforced: a client that
// keeps the cookie past its Max-Age would attribute a signup to a link clicked months ago.
// The capture time travels in the value so the server can refuse it.
func TestExpiredReferralCookieAttributesNobody(t *testing.T) {
	lowerRegisterCost(t)
	env := storefront(t, newAuthEnv(t))
	env.seedAffiliate(t, "PARTNER10", "1/10")

	rec := env.registerWith(t, map[string]any{
		"email":        "late@example.test",
		"password":     "correct horse battery",
		"display_name": "Late Arrival",
	}, []*http.Cookie{{Name: "billing_ref", Value: staleReferralCookieValue("PARTNER10")}})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("register: status %d, body %s", rec.Code, rec.Body)
	}
	if _, ok := env.attributionOf(t, "late@example.test"); ok {
		t.Fatal("a referral cookie older than the attribution window still attributed a signup")
	}
}

// A duplicate registration creates nothing, so it must not consume the code: the browser
// may still be used to register the account the link was meant for.
func TestDuplicateRegistrationKeepsTheReferralCode(t *testing.T) {
	lowerRegisterCost(t)
	env := storefront(t, newAuthEnv(t))
	env.seedAffiliate(t, "PARTNER10", "1/10")
	env.createUser(t, "existing@example.test", "correct horse battery", "")

	rec := env.registerWith(t, map[string]any{
		"email":        "existing@example.test",
		"password":     "correct horse battery",
		"display_name": "Existing",
	}, []*http.Cookie{{Name: "billing_ref", Value: referralCookieValue("PARTNER10")}})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("duplicate register: status %d, body %s", rec.Code, rec.Body)
	}
	if c := referralCookie(rec.Result().Cookies()); c != nil {
		t.Fatalf("a duplicate registration expired the referral cookie: %+v", c)
	}
	if _, ok := env.attributionOf(t, "existing@example.test"); ok {
		t.Fatal("a duplicate registration attributed an account that already existed")
	}
}
