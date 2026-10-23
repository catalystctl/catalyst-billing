package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The customer's language preference, through the real handlers.
//
// The resolution chain has three branches — the account's choice, the Accept-Language header,
// then the default — and the first one is only reachable if something can write it. These tests
// pin the write, the read-back, and the refusal, because the failure they guard against is
// invisible from the UI: a switcher that only wrote localStorage looked correct in the browser
// that made the choice while every other device, and every queued email, resolved through the
// header.

// getWithHeader is `env.get` with an Accept-Language value, which is the input the resolution
// chain exists to read.
func (e *authEnv) getWithHeader(t *testing.T, path, acceptLanguage string, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if acceptLanguage != "" {
		req.Header.Set("Accept-Language", acceptLanguage)
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	e.srv.Handler().ServeHTTP(rec, req)
	return rec
}

// resolvedLocale reads the `resolved` object of GET /api/v1/i18n/locales.
func resolvedLocale(t *testing.T, rec *httptest.ResponseRecorder) (locale, source string) {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("locales: status %d; body %s", rec.Code, rec.Body)
	}
	var body struct {
		Resolved struct {
			Locale string `json:"locale"`
			Source string `json:"source"`
		} `json:"resolved"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("locales body: %v\n%s", err, rec.Body)
	}
	return body.Resolved.Locale, body.Resolved.Source
}

// enableLocale turns a seeded locale on, which is what an operator does in /locales.
func (e *authEnv) enableLocale(t *testing.T, code string) {
	t.Helper()
	if _, err := e.pool.Exec(context.Background(),
		`UPDATE locales SET enabled = true, updated_at = now() WHERE code = $1`, code); err != nil {
		t.Fatalf("enable %s: %v", code, err)
	}
}

// Setting a locale stores it, and the next request resolves to it even when the browser asks for
// something else. That is the whole defect: without the write, the header was the first branch
// that could ever answer.
func TestUserLocalePreferenceRoundTripsAndBeatsTheHeader(t *testing.T) {
	env := newAuthEnv(t)
	env.enableLocale(t, "de")
	userID := env.createUser(t, "locale@example.test", "locale password 1", "")
	cookie, csrf := env.login(t, "locale@example.test", "locale password 1")

	// Before choosing, this browser's header decides: French is not offered, so the default.
	if locale, source := resolvedLocale(t, env.getWithHeader(t, "/api/v1/i18n/locales", "fr-FR,fr;q=0.9", cookie)); locale != "en" || source != "default" {
		t.Fatalf("before choosing: resolved %q from %q, want the default", locale, source)
	}

	// The code is sent in the case a browser would send it; the store normalises it.
	rec := env.putJSON(t, "/api/v1/me/locale", cookie, csrf, map[string]any{"locale": "DE"})
	if rec.Code != http.StatusOK {
		t.Fatalf("store: status %d; body %s", rec.Code, rec.Body)
	}
	var stored struct {
		Locale string `json:"locale"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &stored); err != nil {
		t.Fatalf("store body: %v\n%s", err, rec.Body)
	}
	if stored.Locale != "de" {
		t.Fatalf("stored locale = %q, want the normalised code", stored.Locale)
	}

	// The durable record, read straight from the column the mail path uses.
	var column *string
	if err := env.pool.QueryRow(context.Background(),
		`SELECT locale FROM users WHERE id = $1`, userID).Scan(&column); err != nil {
		t.Fatalf("read users.locale: %v", err)
	}
	if column == nil || *column != "de" {
		t.Fatalf("users.locale = %v, want de", column)
	}

	// The read-back route agrees with the column.
	got := env.get(t, "/api/v1/me/locale", cookie)
	if got.Code != http.StatusOK {
		t.Fatalf("read preference: status %d; body %s", got.Code, got.Body)
	}
	var read struct {
		Locale string `json:"locale"`
	}
	if err := json.Unmarshal(got.Body.Bytes(), &read); err != nil {
		t.Fatalf("read body: %v\n%s", err, got.Body)
	}
	if read.Locale != "de" {
		t.Fatalf("GET /me/locale = %q, want de", read.Locale)
	}

	// A later request in a browser that asks for French resolves to the stored German: the
	// preference outranks the header, which is the branch that used to be unreachable.
	if locale, source := resolvedLocale(t, env.getWithHeader(t, "/api/v1/i18n/locales", "fr-FR,fr;q=0.9", cookie)); locale != "de" || source != "preference" {
		t.Fatalf("after choosing: resolved %q from %q, want de from preference", locale, source)
	}

	// An anonymous request with the same header still gets the default: the preference belongs
	// to an account, not to the installation.
	if locale, source := resolvedLocale(t, env.getWithHeader(t, "/api/v1/i18n/locales", "fr-FR", nil)); locale != "en" || source != "default" {
		t.Fatalf("anonymous: resolved %q from %q, want the default", locale, source)
	}

	// Clearing the preference returns the customer to following their browser, which is what a
	// visitor who never chose gets.
	rec = env.putJSON(t, "/api/v1/me/locale", cookie, csrf, map[string]any{"locale": ""})
	if rec.Code != http.StatusOK {
		t.Fatalf("clear: status %d; body %s", rec.Code, rec.Body)
	}
	if locale, source := resolvedLocale(t, env.getWithHeader(t, "/api/v1/i18n/locales", "de-AT,de;q=0.9", cookie)); locale != "de" || source != "header" {
		t.Fatalf("after clearing: resolved %q from %q, want de from the header", locale, source)
	}

	// Clearing is not the same request as choosing the default. "No opinion" and "I want English"
	// resolve to the same page today and are different facts tomorrow: the first follows the
	// browser, the second ignores it. A store that collapsed them would silently pin every
	// customer who cleared their preference to whatever the default was that day.
	rec = env.putJSON(t, "/api/v1/me/locale", cookie, csrf, map[string]any{"locale": "en"})
	if rec.Code != http.StatusOK {
		t.Fatalf("choose the default: status %d; body %s", rec.Code, rec.Body)
	}
	if err := env.pool.QueryRow(context.Background(),
		`SELECT locale FROM users WHERE id = $1`, userID).Scan(&column); err != nil {
		t.Fatalf("read users.locale: %v", err)
	}
	if column == nil || *column != "en" {
		t.Fatalf("users.locale = %v after choosing the default, want en rather than NULL", column)
	}
	if locale, source := resolvedLocale(t, env.getWithHeader(t, "/api/v1/i18n/locales", "de-AT,de;q=0.9", cookie)); locale != "en" || source != "preference" {
		t.Fatalf("after choosing the default: resolved %q from %q, want en from the preference", locale, source)
	}
}

// A locale the installation does not offer is refused, not stored. A stored preference for a
// language the catalogue cannot render is a customer whose page falls through to bare keys.
func TestUserLocaleRefusesALanguageTheInstallationDoesNotOffer(t *testing.T) {
	env := newAuthEnv(t)
	// `de` ships disabled with a full translation set, so it exists and is still not offered.
	userID := env.createUser(t, "locale@example.test", "locale password 1", "")
	cookie, csrf := env.login(t, "locale@example.test", "locale password 1")

	for _, code := range []string{"de", "fr"} {
		rec := env.putJSON(t, "/api/v1/me/locale", cookie, csrf, map[string]any{"locale": code})
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("%s: status %d, want 422; body %s", code, rec.Code, rec.Body)
		}
		if got := problemCode(t, rec); got != "locale_not_enabled" {
			t.Fatalf("%s: problem code = %q, want locale_not_enabled", code, got)
		}
	}

	var column *string
	if err := env.pool.QueryRow(context.Background(),
		`SELECT locale FROM users WHERE id = $1`, userID).Scan(&column); err != nil {
		t.Fatalf("read users.locale: %v", err)
	}
	if column != nil {
		t.Fatalf("users.locale = %q after a refused write, want NULL", *column)
	}

	// Enabling it is what makes the same request succeed, so the refusal is about the
	// installation's state rather than the request's shape.
	env.enableLocale(t, "de")
	if rec := env.putJSON(t, "/api/v1/me/locale", cookie, csrf, map[string]any{"locale": "de"}); rec.Code != http.StatusOK {
		t.Fatalf("after enabling de: status %d; body %s", rec.Code, rec.Body)
	}
}

// The preference belongs to an account, so it cannot be read or written anonymously.
func TestUserLocalePreferenceNeedsASession(t *testing.T) {
	env := newAuthEnv(t)

	if rec := env.get(t, "/api/v1/me/locale", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous read: status %d, want 401", rec.Code)
	}

	req := httptest.NewRequest(http.MethodPut, "/api/v1/me/locale",
		bytes.NewReader([]byte(`{"locale":"de"}`)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	env.srv.Handler().ServeHTTP(rec, req)
	// The CSRF guard is the outermost wrapper and fails closed without a session, so an
	// anonymous mutation is refused before the handler runs at all. Either refusal would be
	// correct; what matters is that the preference cannot be written by someone with no account.
	if rec.Code != http.StatusForbidden && rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous write: status %d, want 401 or 403; body %s", rec.Code, rec.Body)
	}
}
