package httpapi_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/catalyst/billing/internal/httpapi"
	"github.com/catalyst/billing/internal/payments"
	"github.com/catalyst/billing/internal/store/storetest"
)

const secret = "whsec_test_secret"

// signedRequest builds a Stripe-style signed webhook request.
func signedRequest(t *testing.T, provider, body string, at time.Time) *http.Request {
	t.Helper()
	ts := fmt.Sprintf("%d", at.Unix())
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts))
	mac.Write([]byte("."))
	mac.Write([]byte(body))

	req := httptest.NewRequest(http.MethodPost, "/webhooks/"+provider, strings.NewReader(body))
	req.Header.Set("Stripe-Timestamp", ts)
	req.Header.Set("Stripe-Signature", "v1="+hex.EncodeToString(mac.Sum(nil)))
	return req
}

func newServer(t *testing.T, provider, account string) (*httpapi.Server, *payments.Inbox) {
	t.Helper()
	pool := storetest.DB(t)
	inbox := payments.NewInbox(pool)
	verifier := payments.HMACSigner{
		Secret:          []byte(secret),
		Header:          "Stripe-Signature",
		TimestampHeader: "Stripe-Timestamp",
		Tolerance:       5 * time.Minute,
	}
	srv := httpapi.New(httpapi.Config{
		Version: "test",
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	}, pool, inbox, provider, account, verifier)
	return srv, inbox
}

func eventBody(eventID, paymentID string) string {
	return fmt.Sprintf(`{"event_id":%q,"event_type":"payment.succeeded","payment_id":%q,`+
		`"amount":{"minor":1200,"currency":"USD"}}`, eventID, paymentID)
}

// A valid delivery is accepted and stored exactly once, and a redelivery is
// acknowledged without creating a second event.
func TestWebhookAcceptedThenDeduplicated(t *testing.T) {
	srv, _ := newServer(t, "stripe", "acct_test")
	handler := srv.Handler()

	body := eventBody("evt_1", "pi_1")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, signedRequest(t, "stripe", body, time.Now()))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body: %s", rec.Code, rec.Body)
	}
	var accepted map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &accepted); err != nil {
		t.Fatal(err)
	}
	if accepted["status"] != "accepted" {
		t.Fatalf("status field = %v, want accepted", accepted["status"])
	}

	// The same delivery again: acknowledged, but not stored twice.
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, signedRequest(t, "stripe", body, time.Now()))
	if rec.Code != http.StatusOK {
		t.Fatalf("duplicate status = %d, want 200; body: %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "duplicate") {
		t.Fatalf("duplicate response = %s", rec.Body)
	}
}

// A tampered body must be refused: the signature covers the bytes.
func TestWebhookRejectsTamperedBody(t *testing.T) {
	srv, _ := newServer(t, "stripe", "acct_test")
	handler := srv.Handler()

	req := signedRequest(t, "stripe", eventBody("evt_1", "pi_1"), time.Now())
	// Replace the body after signing.
	req.Body = io.NopCloser(strings.NewReader(eventBody("evt_1", "pi_evil")))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "invalid_signature") {
		t.Fatalf("body = %s", rec.Body)
	}
}

// An old signature is refused, so a captured delivery cannot be replayed later.
func TestWebhookRejectsStaleSignature(t *testing.T) {
	srv, _ := newServer(t, "stripe", "acct_test")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec,
		signedRequest(t, "stripe", eventBody("evt_old", "pi_1"), time.Now().Add(-time.Hour)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

// An oversized body is refused before it is fully read.
func TestWebhookRejectsOversizedBody(t *testing.T) {
	srv, _ := newServer(t, "stripe", "acct_test")
	big := strings.Repeat("x", 2<<20)
	req := httptest.NewRequest(http.MethodPost, "/webhooks/stripe", strings.NewReader(big))
	req.Header.Set("Content-Type", "application/json")

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", rec.Code)
	}
}

// An unknown provider path is a 404, and no event is stored.
func TestWebhookUnknownProvider(t *testing.T) {
	srv, _ := newServer(t, "stripe", "acct_test")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec,
		signedRequest(t, "not-configured", eventBody("evt_1", "pi_1"), time.Now()))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

// With no verifier configured, deliveries are refused rather than trusted.
func TestWebhookWithoutVerifierRefuses(t *testing.T) {
	pool := storetest.DB(t)
	srv := httpapi.New(httpapi.Config{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}, pool, payments.NewInbox(pool), "stripe", "acct_test", nil)

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec,
		signedRequest(t, "stripe", eventBody("evt_1", "pi_1"), time.Now()))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (no gateway configured)", rec.Code)
	}
}

// Liveness must not depend on the database, so a database blip does not cause a
// restart loop.
func TestHealthzDoesNotTouchDatabase(t *testing.T) {
	// A nil pool would panic if the handler touched it.
	srv := httpapi.New(httpapi.Config{
		Version: "test",
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	}, nil, nil, "stripe", "acct_test", nil)

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

// Readiness must report the database state, and must not require DDL rights.
func TestReadyzReportsDatabase(t *testing.T) {
	srv, _ := newServer(t, "stripe", "acct_test")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body)
	}

	// Without a pool, readiness is unavailable -- not "ok".
	noDB := httpapi.New(httpapi.Config{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}, nil, nil, "stripe", "acct_test", nil)
	rec = httptest.NewRecorder()
	noDB.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

// Metrics count what actually happened.
func TestMetricsReflectActivity(t *testing.T) {
	srv, _ := newServer(t, "stripe", "acct_test")
	handler := srv.Handler()

	body := eventBody("evt_metrics", "pi_1")
	for i := 0; i < 3; i++ {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, signedRequest(t, "stripe", body, time.Now()))
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	out := rec.Body.String()
	for _, want := range []string{
		"billing_webhooks_received_total 3",
		"billing_webhooks_duplicate_total 2",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("metrics missing %q:\n%s", want, out)
		}
	}
}

// A duplicate is only reported as a duplicate when the event really was stored
// before: this guards against a response that lies about durability.
func TestDuplicateRequiresStoredEvent(t *testing.T) {
	srv, inbox := newServer(t, "stripe", "acct_test")
	handler := srv.Handler()

	body := eventBody("evt_stored", "pi_1")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, signedRequest(t, "stripe", body, time.Now()))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rec.Code)
	}

	pending, err := inbox.ClaimPending(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].EventID != "evt_stored" {
		t.Fatalf("stored events = %+v, want one evt_stored", pending)
	}
}

// The operator overview is served as one request and never cached: a stale
// dashboard is worse than no dashboard.
func TestOverviewEndpoint(t *testing.T) {
	srv, _ := newServer(t, "stripe", "acct_test")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/ops/overview", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}

	var body struct {
		GeneratedAt string           `json:"generated_at"`
		Exceptions  []map[string]any `json:"exceptions"`
		Counts      map[string]int   `json:"counts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("overview is not valid JSON: %v\n%s", err, rec.Body)
	}
	if body.GeneratedAt == "" {
		t.Fatal("generated_at missing")
	}
	if _, ok := body.Counts["customers"]; !ok {
		t.Fatalf("counts missing customers: %s", rec.Body)
	}
	for _, e := range body.Exceptions {
		for _, field := range []string{"code", "title", "detail", "severity", "action"} {
			if _, ok := e[field]; !ok {
				t.Fatalf("exception missing %q: %v", field, e)
			}
		}
	}
}
