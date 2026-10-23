package httpapi_test

// The mail-branding, problem-code and shipped-locale surfaces over HTTP.
//
// Four properties are pinned here, and each is one a client depends on:
//
//   - an unknown placeholder is still a 422 `unknown_placeholder` at save time, and the refusal now
//     carries a field list so a client can point at the textarea and translate the sentence;
//   - the preview returns the branded HTML the recipient will get, alongside the plain text;
//   - the test mail is queued through the outbox and is visible in the email log, because a test
//     that bypasses the delivery path proves nothing about the path;
//   - a shipped language can be installed in one call, and a refusal names the field it is about.

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// putSettings writes one settings group through the API the screen uses.
func (e *authEnv) putSettings(t *testing.T, cookie *http.Cookie, csrf, group string,
	values map[string]string) *httptestResponse {

	t.Helper()
	rec := e.doJSON(t, http.MethodPut, "/api/v1/staff/settings", cookie, csrf, map[string]any{
		"group": group, "values": values,
	})
	return &httptestResponse{Code: rec.Code, Body: rec.Body.Bytes()}
}

// httptestResponse is the small slice of a recorder these tests read.
type httptestResponse struct {
	Code int
	Body []byte
}

// fieldProblemBody is a problem document with the machine-readable field list.
type fieldProblemBody struct {
	Type   string `json:"type"`
	Title  string `json:"title"`
	Status int    `json:"status"`
	Detail string `json:"detail"`
	Code   string `json:"code"`
	Fields []struct {
		Field string `json:"field"`
		Code  string `json:"code"`
		Value string `json:"value"`
	} `json:"fields"`
}

func decodeFieldProblem(t *testing.T, body []byte) fieldProblemBody {
	t.Helper()
	var out fieldProblemBody
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("problem body is not JSON: %v (%s)", err, body)
	}
	return out
}

// The 422 survives, and it now names the field and the placeholder.
//
// The behaviour this protects is the one that must never become a 500 at send time: a template
// whose vocabulary the event does not supply is refused where it is stored.
func TestUnknownPlaceholderIs422WithAFieldList(t *testing.T) {
	env := newStaffEnv(t)
	env.seedTemplates(t)

	rec := env.doJSON(t, http.MethodPut, "/api/v1/staff/notification-templates/invoice_paid",
		env.billing, env.billingCSRF, map[string]any{
			"subject":        "Paid {{invoice_number}}",
			"body":           "Total {{total_amount}}.",
			"in_app_title":   "Paid",
			"in_app_body":    "Thanks",
			"in_app_url":     "{{invoice_url}}",
			"enabled":        true,
			"mail_enabled":   true,
			"in_app_enabled": true,
		})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body %s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/problem+json") {
		t.Fatalf("Content-Type = %q, want application/problem+json", ct)
	}
	body := decodeFieldProblem(t, rec.Body.Bytes())
	if body.Code != "unknown_placeholder" {
		t.Fatalf("code = %q, want unknown_placeholder", body.Code)
	}
	if len(body.Fields) != 1 {
		t.Fatalf("fields = %+v, want one", body.Fields)
	}
	if body.Fields[0].Field != "body" || body.Fields[0].Value != "total_amount" {
		t.Fatalf("field = %+v, want body/total_amount", body.Fields[0])
	}
	// The sentence is still there: it is the developer-facing explanation and the client's
	// fallback when the catalogue has no translation for the code.
	if !strings.Contains(body.Detail, "total_amount") {
		t.Fatalf("detail does not name the placeholder: %q", body.Detail)
	}
}

// The preview renders the branded HTML part the customer will receive.
func TestTemplatePreviewCarriesTheBrandedHTML(t *testing.T) {
	env := newStaffEnv(t)
	env.seedTemplates(t)

	if rec := env.putSettings(t, env.admin, env.adminCSRF, "mail", map[string]string{
		"brand_header":   "Northwind",
		"brand_footer":   "Northwind Hosting · 12 Harbour Road",
		"brand_accent":   "#0f766e",
		"brand_logo_url": "https://cdn.example.test/logo.png",
	}); rec.Code != http.StatusOK {
		t.Fatalf("save mail branding: status %d; body %s", rec.Code, rec.Body)
	}

	rec := env.doJSON(t, http.MethodPost,
		"/api/v1/staff/notification-templates/invoice_paid/preview",
		env.billing, env.billingCSRF, map[string]any{
			"subject":      "Paid {{invoice_number}}",
			"body":         "Thank you. Invoice {{invoice_number}} is settled.",
			"in_app_title": "Paid",
			"in_app_body":  "Thanks",
			"in_app_url":   "{{invoice_url}}",
		})
	if rec.Code != http.StatusOK {
		t.Fatalf("preview: status %d; body %s", rec.Code, rec.Body)
	}
	var out struct {
		Mail struct {
			Subject string `json:"subject"`
			Body    string `json:"body"`
			HTML    string `json:"html"`
			Brand   struct {
				HTML    bool   `json:"html"`
				Header  string `json:"header"`
				Footer  string `json:"footer"`
				Accent  string `json:"accent"`
				LogoURL string `json:"logo_url"`
			} `json:"brand"`
		} `json:"mail"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("preview body: %v (%s)", err, rec.Body)
	}
	if !out.Mail.Brand.HTML {
		t.Fatal("the preview says no branded part would be sent")
	}
	if out.Mail.Brand.Header != "Northwind" || out.Mail.Brand.Accent != "#0f766e" {
		t.Fatalf("preview brand = %+v", out.Mail.Brand)
	}
	if out.Mail.HTML == "" {
		t.Fatal("the preview has no HTML part")
	}
	for _, want := range []string{"Northwind", "12 Harbour Road", "#0f766e", "https://cdn.example.test/logo.png"} {
		if !strings.Contains(out.Mail.HTML, want) {
			t.Fatalf("the preview's HTML is missing %q:\n%s", want, out.Mail.HTML)
		}
	}
	// The plain-text preview is the other channel and stays markup-free.
	if strings.Contains(out.Mail.Body, "<") {
		t.Fatalf("the plain-text preview contains markup:\n%s", out.Mail.Body)
	}
}

// The test mail is queued through the outbox and audited.
func TestTestEmailIsQueuedThroughTheOutbox(t *testing.T) {
	env := newStaffEnv(t)

	rec := env.doJSON(t, http.MethodPost, "/api/v1/staff/notifications/test-email",
		env.billing, env.billingCSRF, map[string]any{"to": "ops@example.test"})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body %s", rec.Code, rec.Body)
	}
	var out struct {
		ID     string `json:"id"`
		To     string `json:"to"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("body: %v (%s)", err, rec.Body)
	}
	if out.ID == "" || out.To != "ops@example.test" || out.Status != "queued" {
		t.Fatalf("response = %+v", out)
	}

	var kind, body, htmlPart string
	if err := env.pool.QueryRow(context.Background(), `
		SELECT kind, body, body_html FROM notification_deliveries WHERE id = $1`, out.ID).
		Scan(&kind, &body, &htmlPart); err != nil {
		t.Fatalf("read delivery: %v", err)
	}
	if kind != "test_email" {
		t.Fatalf("kind = %q, want test_email", kind)
	}
	if !strings.Contains(body, "test message") {
		t.Fatalf("the queued body is not the test sentence:\n%s", body)
	}
	if strings.Contains(body, "<") {
		t.Fatalf("the plain part contains markup:\n%s", body)
	}
	if htmlPart == "" {
		t.Fatal("the test mail has no branded HTML part")
	}

	// The request is a mutation, so it is in the audit log with the recipient it used.
	var audited int
	if err := env.pool.QueryRow(context.Background(), `
		SELECT count(*) FROM audit_events WHERE action = 'notification.test_email'`).
		Scan(&audited); err != nil {
		t.Fatalf("count audit: %v", err)
	}
	if audited != 1 {
		t.Fatalf("audit rows = %d, want 1", audited)
	}
}

// A bodyless request mails the caller, which is what the screen's button offers.
func TestTestEmailWithNoBodyMailsTheCaller(t *testing.T) {
	env := newStaffEnv(t)
	rec := env.doJSON(t, http.MethodPost, "/api/v1/staff/notifications/test-email",
		env.billing, env.billingCSRF, nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body %s", rec.Code, rec.Body)
	}
	var out struct {
		To string `json:"to"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("body: %v (%s)", err, rec.Body)
	}
	if out.To != "billing@example.test" {
		t.Fatalf("recipient = %q, want the caller's own address", out.To)
	}
}

// An address that cannot go in a header is a field refusal, not a failed delivery later.
func TestTestEmailRefusesAnUnusableAddress(t *testing.T) {
	env := newStaffEnv(t)
	rec := env.doJSON(t, http.MethodPost, "/api/v1/staff/notifications/test-email",
		env.billing, env.billingCSRF, map[string]any{"to": "not an address"})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body %s", rec.Code, rec.Body)
	}
	body := decodeFieldProblem(t, rec.Body.Bytes())
	if body.Code != "invalid_input" {
		t.Fatalf("code = %q, want invalid_input", body.Code)
	}
	if len(body.Fields) != 1 || body.Fields[0].Field != "to" || body.Fields[0].Code != "invalid" {
		t.Fatalf("fields = %+v, want to/invalid", body.Fields)
	}
}

// The mail-branding keys reach the settings API, so an operator can edit them without a deploy.
func TestMailBrandingKeysAreEditableThroughTheSettingsAPI(t *testing.T) {
	env := newStaffEnv(t)
	rec := env.get(t, "/api/v1/staff/settings?group=mail", env.admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("get settings: status %d; body %s", rec.Code, rec.Body)
	}
	var out struct {
		Group string `json:"group"`
		Keys  []struct {
			Key string `json:"key"`
		} `json:"keys"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("settings body: %v (%s)", err, rec.Body)
	}
	have := map[string]bool{}
	for _, k := range out.Keys {
		have[k.Key] = true
	}
	for _, key := range []string{"brand_html", "brand_header", "brand_footer", "brand_accent", "brand_logo_url"} {
		if !have[key] {
			t.Fatalf("the mail group does not expose %q: %v", key, have)
		}
	}

	// A logo reference that is not an absolute http(s) URL is refused by the validator, not
	// stored and drawn into a mail.
	bad := env.putSettings(t, env.admin, env.adminCSRF, "mail",
		map[string]string{"brand_logo_url": "javascript:alert(1)"})
	if bad.Code != http.StatusUnprocessableEntity {
		t.Fatalf("bad logo: status %d, want 422; body %s", bad.Code, bad.Body)
	}
}

// A shipped language is installable in one call, and the list shrinks afterwards.
func TestShippedLocalesCanBeInstalled(t *testing.T) {
	env := newStaffEnv(t)

	rec := env.get(t, "/api/v1/staff/locales/available", env.admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("available: status %d; body %s", rec.Code, rec.Body)
	}
	var available struct {
		Data []struct {
			Code       string `json:"code"`
			Name       string `json:"name"`
			NativeName string `json:"native_name"`
		} `json:"data"`
		Shipped int `json:"shipped"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &available); err != nil {
		t.Fatalf("available body: %v (%s)", err, rec.Body)
	}
	if available.Shipped != 24 {
		t.Fatalf("shipped = %d, want 24", available.Shipped)
	}
	var found bool
	for _, l := range available.Data {
		if l.Code == "pt-BR" {
			found = true
			if l.NativeName != "Português (Brasil)" {
				t.Fatalf("native name = %q", l.NativeName)
			}
		}
	}
	if !found {
		t.Fatalf("pt-BR is not offered for installation: %+v", available.Data)
	}

	installed := env.doJSON(t, http.MethodPost, "/api/v1/staff/locales/pt-BR/install",
		env.admin, env.adminCSRF, map[string]any{})
	if installed.Code != http.StatusCreated {
		t.Fatalf("install: status %d; body %s", installed.Code, installed.Body)
	}
	var locale struct {
		Code    string `json:"code"`
		Enabled bool   `json:"enabled"`
	}
	if err := json.Unmarshal(installed.Body.Bytes(), &locale); err != nil {
		t.Fatalf("install body: %v (%s)", err, installed.Body)
	}
	if locale.Code != "pt-BR" || !locale.Enabled {
		t.Fatalf("installed locale = %+v", locale)
	}

	// Installing it again is the existing duplicate conflict, and it names the field.
	again := env.doJSON(t, http.MethodPost, "/api/v1/staff/locales/pt-BR/install",
		env.admin, env.adminCSRF, map[string]any{})
	if again.Code != http.StatusConflict {
		t.Fatalf("second install: status %d, want 409; body %s", again.Code, again.Body)
	}
	body := decodeFieldProblem(t, again.Body.Bytes())
	if body.Code != "duplicate_locale" {
		t.Fatalf("code = %q, want duplicate_locale", body.Code)
	}
	if len(body.Fields) != 1 || body.Fields[0].Field != "code" || body.Fields[0].Code != "duplicate" {
		t.Fatalf("fields = %+v, want code/duplicate", body.Fields)
	}
}

// A language this build does not ship is refused with a field list rather than a 404.
func TestInstallingAnUnknownLanguageNamesTheCodeField(t *testing.T) {
	env := newStaffEnv(t)
	rec := env.doJSON(t, http.MethodPost, "/api/v1/staff/locales/zz-ZZ/install",
		env.admin, env.adminCSRF, map[string]any{})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body %s", rec.Code, rec.Body)
	}
	body := decodeFieldProblem(t, rec.Body.Bytes())
	if body.Code != "invalid_input" {
		t.Fatalf("code = %q, want invalid_input", body.Code)
	}
	if len(body.Fields) != 1 || body.Fields[0].Field != "code" || body.Fields[0].Code != "unknown" {
		t.Fatalf("fields = %+v, want code/unknown", body.Fields)
	}
}

// A malformed locale on the create route is a field refusal too.
func TestCreateLocaleNamesTheOffendingField(t *testing.T) {
	env := newStaffEnv(t)
	rec := env.doJSON(t, http.MethodPost, "/api/v1/staff/locales", env.admin, env.adminCSRF,
		map[string]any{"code": "e", "name": "Broken"})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body %s", rec.Code, rec.Body)
	}
	body := decodeFieldProblem(t, rec.Body.Bytes())
	if len(body.Fields) != 1 || body.Fields[0].Field != "code" {
		t.Fatalf("fields = %+v, want a code field", body.Fields)
	}
}
