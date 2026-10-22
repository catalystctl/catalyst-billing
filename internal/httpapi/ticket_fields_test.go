package httpapi_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/catalyst/billing/internal/portal"
)

// The ticket-field HTTP surface, and the two features that need it: a ticket the team opens for a
// customer, and the two custom-field requirements.
//
// What is asserted here rather than in internal/tickets is the part a client sees: the status, the
// stable code, and the fact that the refusal *names the field*. A frontend that shows "a required
// field is missing" on a form with nine fields is a puzzle, so the sentence is part of the
// contract and is tested as one.

// newFieldEnv is a staff env with the portal wired (the customer routes resolve a customer from
// the session) and one customer to open tickets for.
func newFieldEnv(t *testing.T) (*staffEnv, string, string) {
	t.Helper()
	env := newStaffEnv(t)
	env.srv.WithPortal(portal.New(env.pool))
	customerID, _ := env.seedCustomer(t, "alice@example.test", "Alice")
	return env, customerID, "alice@example.test"
}

// createField posts a field definition as support and returns its id.
func (e *staffEnv) createField(t *testing.T, body map[string]any) string {
	t.Helper()
	rec := e.postJSON(t, "/api/v1/staff/ticket-fields", e.support, e.supportCSRF, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create field %v = %d: %s", body["key"], rec.Code, rec.Body)
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode field: %v (%s)", err, rec.Body)
	}
	return created.ID
}

// deleteJSON sends a DELETE with a CSRF token and a fresh idempotency key, which postJSON does not
// cover.
func (e *authEnv) deleteJSON(t *testing.T, path string, cookie *http.Cookie, csrf string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodDelete, path, nil)
	req.AddCookie(cookie)
	if csrf != "" {
		req.Header.Set("X-CSRF-Token", csrf)
	}
	req.Header.Set("Idempotency-Key", "field-delete-"+t.Name()+"-"+time.Now().Format("150405.000000000"))
	rec := httptest.NewRecorder()
	e.srv.Handler().ServeHTTP(rec, req)
	return rec
}

// ------------------------------------------------------- staff creates a ticket --

// An agent can open a ticket for a named customer, and the ticket records both sides of it.
func TestStaffOpensTicketForCustomer(t *testing.T) {
	env, customerID, _ := newFieldEnv(t)

	rec := env.postJSON(t, "/api/v1/staff/tickets", env.support, env.supportCSRF, map[string]any{
		"customer_id": customerID,
		"subject":     "called about a broken site",
		"department":  "technical",
		"priority":    "high",
		"message":     map[string]any{"body": "the customer telephoned"},
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("staff create = %d: %s", rec.Code, rec.Body)
	}
	var thread struct {
		Ticket struct {
			ID            string `json:"id"`
			Number        string `json:"number"`
			OpenedByStaff bool   `json:"opened_by_staff"`
			OpenedBy      string `json:"opened_by"`
			Customer      struct {
				ID    string `json:"id"`
				Email string `json:"email"`
			} `json:"customer"`
		} `json:"ticket"`
		Messages []struct {
			FromStaff bool   `json:"from_staff"`
			Body      string `json:"body"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &thread); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body)
	}
	if !thread.Ticket.OpenedByStaff {
		t.Fatalf("the ticket does not record that staff opened it: %s", rec.Body)
	}
	if thread.Ticket.OpenedBy == "" {
		t.Fatal("the ticket does not record which staff member opened it")
	}
	if thread.Ticket.Customer.ID != customerID || thread.Ticket.Customer.Email != "alice@example.test" {
		t.Fatalf("customer = %+v, want the named customer", thread.Ticket.Customer)
	}
	if len(thread.Messages) != 1 || !thread.Messages[0].FromStaff {
		t.Fatalf("first message = %+v, want one staff message", thread.Messages)
	}

	// The customer sees it in their own portal list, marked as staff-opened.
	cookie, _ := env.login(t, "alice@example.test", "customer password 1")
	rec = env.get(t, "/api/v1/portal/tickets", cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("customer list = %d: %s", rec.Code, rec.Body)
	}
	var list struct {
		Tickets []struct {
			ID            string `json:"id"`
			OpenedByStaff bool   `json:"opened_by_staff"`
		} `json:"tickets"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Tickets) != 1 || !list.Tickets[0].OpenedByStaff {
		t.Fatalf("customer list = %+v, want the staff-opened ticket", list.Tickets)
	}

	// The queue shows it too, with the customer attached.
	rec = env.get(t, "/api/v1/staff/tickets", env.support)
	var queue struct {
		Tickets []struct {
			ID            string `json:"id"`
			OpenedByStaff bool   `json:"opened_by_staff"`
			Customer      struct {
				ID string `json:"id"`
			} `json:"customer"`
		} `json:"tickets"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &queue); err != nil {
		t.Fatal(err)
	}
	if len(queue.Tickets) != 1 || queue.Tickets[0].Customer.ID != customerID {
		t.Fatalf("queue = %+v", queue.Tickets)
	}
}

// A staff create names a customer that must exist, and a customer session cannot reach it.
func TestStaffCreateRequiresACustomerAndPermission(t *testing.T) {
	env, _, _ := newFieldEnv(t)

	// No customer at all.
	rec := env.postJSON(t, "/api/v1/staff/tickets", env.support, env.supportCSRF, map[string]any{
		"subject": "nobody", "message": map[string]any{"body": "hi"},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("create without a customer = %d, want 400: %s", rec.Code, rec.Body)
	}
	if code := problemOf(t, rec); code != "invalid_input" {
		t.Fatalf("code = %q, want invalid_input", code)
	}

	// A customer that does not exist.
	rec = env.postJSON(t, "/api/v1/staff/tickets", env.support, env.supportCSRF, map[string]any{
		"customer_id": "0f8fad5b-d9cb-469f-a165-70867728950e",
		"subject":     "nobody",
		"message":     map[string]any{"body": "hi"},
	})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("create for an unknown customer = %d, want 404: %s", rec.Code, rec.Body)
	}
	problemOf(t, rec)

	// A customer session cannot open a ticket on anyone's behalf.
	cookie, csrf := env.login(t, "alice@example.test", "customer password 1")
	rec = env.postJSON(t, "/api/v1/staff/tickets", cookie, csrf, map[string]any{
		"customer_id": "0f8fad5b-d9cb-469f-a165-70867728950e",
		"subject":     "self",
		"message":     map[string]any{"body": "hi"},
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("customer on the staff create = %d, want 403: %s", rec.Code, rec.Body)
	}
	problemOf(t, rec)

	// Infrastructure staff hold neither ticket permission, so they cannot create either.
	env.createUser(t, "infra@example.test", "infra password 1", "infrastructure")
	infra, infraCSRF := env.login(t, "infra@example.test", "infra password 1")
	rec = env.postJSON(t, "/api/v1/staff/tickets", infra, infraCSRF, map[string]any{
		"customer_id": "0f8fad5b-d9cb-469f-a165-70867728950e",
		"subject":     "no",
		"message":     map[string]any{"body": "hi"},
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("infrastructure create = %d, want 403: %s", rec.Code, rec.Body)
	}
}

// --------------------------------------------------------------- the definitions --

// The three types can be created over HTTP, and the settings screen can read them back.
func TestTicketFieldCRUD(t *testing.T) {
	env, _, _ := newFieldEnv(t)

	textID := env.createField(t, map[string]any{
		"key": "server_name", "label": "Server name", "type": "text",
		"required_on_create": true,
	})
	selectID := env.createField(t, map[string]any{
		"key": "region", "label": "Region", "type": "select",
		"options":           []string{"eu-west", "us-east"},
		"required_to_close": true,
	})
	checkboxID := env.createField(t, map[string]any{
		"key": "confirmed", "label": "Confirmed", "type": "checkbox",
	})

	rec := env.get(t, "/api/v1/staff/ticket-fields", env.support)
	if rec.Code != http.StatusOK {
		t.Fatalf("list fields = %d: %s", rec.Code, rec.Body)
	}
	var listed struct {
		Fields []struct {
			ID               string   `json:"id"`
			Key              string   `json:"key"`
			Type             string   `json:"type"`
			Options          []string `json:"options"`
			RequiredOnCreate bool     `json:"required_on_create"`
			RequiredToClose  bool     `json:"required_to_close"`
		} `json:"fields"`
		Types []string `json:"types"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Fields) != 3 {
		t.Fatalf("fields = %+v, want 3", listed.Fields)
	}
	if len(listed.Types) != 3 {
		t.Fatalf("types = %+v, want the closed set", listed.Types)
	}
	byID := make(map[string]int, 3)
	for i, field := range listed.Fields {
		byID[field.ID] = i
	}
	if got := listed.Fields[byID[selectID]]; got.Type != "select" || len(got.Options) != 2 || !got.RequiredToClose {
		t.Fatalf("select field = %+v", got)
	}
	if got := listed.Fields[byID[textID]]; got.Type != "text" || !got.RequiredOnCreate {
		t.Fatalf("text field = %+v", got)
	}
	if got := listed.Fields[byID[checkboxID]]; got.Type != "checkbox" || got.Options == nil {
		t.Fatalf("checkbox field = %+v", got)
	}

	// A change, and then a retirement. Archiving keeps the definition readable.
	rec = env.patchTicket(t, "/api/v1/staff/ticket-fields/"+checkboxID, env.support, env.supportCSRF,
		map[string]any{"label": "Acknowledged", "required_to_close": true})
	if rec.Code != http.StatusOK {
		t.Fatalf("patch field = %d: %s", rec.Code, rec.Body)
	}
	var patched struct {
		Label           string `json:"label"`
		RequiredToClose bool   `json:"required_to_close"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &patched); err != nil {
		t.Fatal(err)
	}
	if patched.Label != "Acknowledged" || !patched.RequiredToClose {
		t.Fatalf("patched = %+v", patched)
	}

	// An invalid definition is a 400 with the reason, not a 500.
	rec = env.postJSON(t, "/api/v1/staff/ticket-fields", env.support, env.supportCSRF, map[string]any{
		"key": "Bad Key!", "label": "Bad", "type": "text",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid key = %d, want 400: %s", rec.Code, rec.Body)
	}
	problemOf(t, rec)
	rec = env.postJSON(t, "/api/v1/staff/ticket-fields", env.support, env.supportCSRF, map[string]any{
		"key": "no_options", "label": "No options", "type": "select",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("select without options = %d, want 400: %s", rec.Code, rec.Body)
	}
	problemOf(t, rec)
	rec = env.postJSON(t, "/api/v1/staff/ticket-fields", env.support, env.supportCSRF, map[string]any{
		"key": "unknown_type", "label": "Unknown", "type": "radio",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown type = %d, want 400: %s", rec.Code, rec.Body)
	}
	problemOf(t, rec)

	// A customer cannot configure the desk's fields.
	cookie, csrf := env.login(t, "alice@example.test", "customer password 1")
	rec = env.postJSON(t, "/api/v1/staff/ticket-fields", cookie, csrf, map[string]any{
		"key": "mine", "label": "Mine", "type": "text",
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("customer creating a field = %d, want 403: %s", rec.Code, rec.Body)
	}
	problemOf(t, rec)
}

// ------------------------------------------------------- required at creation --

// A required-at-creation field refuses the ticket and names the field, and the answer is accepted
// on both the customer's route and the staff route.
func TestRequiredFieldOnCreateOverHTTP(t *testing.T) {
	env, customerID, _ := newFieldEnv(t)
	env.createField(t, map[string]any{
		"key": "server_name", "label": "Server name", "type": "text",
		"required_on_create": true,
	})

	cookie, csrf := env.login(t, "alice@example.test", "customer password 1")

	rec := env.postJSON(t, "/api/v1/portal/tickets", cookie, csrf, map[string]any{
		"subject": "no answer", "message": map[string]any{"body": "help"},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("create without the field = %d, want 400: %s", rec.Code, rec.Body)
	}
	if code := problemOf(t, rec); code != "required_field_missing" {
		t.Fatalf("code = %q, want required_field_missing", code)
	}
	var problem struct {
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &problem); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(problem.Detail, "Server name") {
		t.Fatalf("detail %q does not name the field", problem.Detail)
	}

	// The staff route enforces the same rule.
	rec = env.postJSON(t, "/api/v1/staff/tickets", env.support, env.supportCSRF, map[string]any{
		"customer_id": customerID, "subject": "no answer",
		"message": map[string]any{"body": "help"},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("staff create without the field = %d, want 400: %s", rec.Code, rec.Body)
	}
	if code := problemOf(t, rec); code != "required_field_missing" {
		t.Fatalf("code = %q, want required_field_missing", code)
	}

	// With the answer, both succeed, and the thread carries the value.
	rec = env.postJSON(t, "/api/v1/portal/tickets", cookie, csrf, map[string]any{
		"subject": "answered", "message": map[string]any{"body": "help"},
		"fields": map[string]any{"server_name": "web-01"},
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create with the answer = %d: %s", rec.Code, rec.Body)
	}
	var thread struct {
		Fields []struct {
			Key      string `json:"key"`
			Answered bool   `json:"answered"`
			Value    any    `json:"value"`
		} `json:"fields"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &thread); err != nil {
		t.Fatal(err)
	}
	if len(thread.Fields) != 1 || !thread.Fields[0].Answered || thread.Fields[0].Value != "web-01" {
		t.Fatalf("thread fields = %+v", thread.Fields)
	}
}

// --------------------------------------------------------- required to close --

// Closing is refused while a close-required field is unanswered, the refusal names the field, and
// the field can be answered afterwards — which is the only way the requirement is satisfiable.
func TestRequiredToCloseOverHTTP(t *testing.T) {
	env, _, _ := newFieldEnv(t)
	env.createField(t, map[string]any{
		"key": "region", "label": "Region", "type": "select",
		"options":           []string{"eu-west", "us-east"},
		"required_to_close": true,
	})

	cookie, csrf := env.login(t, "alice@example.test", "customer password 1")
	ticketID, _ := env.openTicket(t, cookie, csrf, "closable")

	// Close is refused, and the sentence names the field.
	rec := env.postJSON(t, "/api/v1/portal/tickets/"+ticketID+"/close", cookie, csrf, nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("close without the field = %d, want 409: %s", rec.Code, rec.Body)
	}
	if code := problemOf(t, rec); code != "required_field_missing" {
		t.Fatalf("code = %q, want required_field_missing", code)
	}
	var problem struct {
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &problem); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(problem.Detail, "Region") || !strings.Contains(problem.Detail, "closed") {
		t.Fatalf("detail = %q, want it to name the field and the close", problem.Detail)
	}

	// Staff cannot bypass it either, and neither can the queue's status edit.
	rec = env.postJSON(t, "/api/v1/staff/tickets/"+ticketID+"/close", env.support, env.supportCSRF, nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("staff close without the field = %d, want 409: %s", rec.Code, rec.Body)
	}
	rec = env.patchTicket(t, "/api/v1/staff/tickets/"+ticketID, env.support, env.supportCSRF,
		map[string]any{"status": "closed"})
	if rec.Code != http.StatusConflict {
		t.Fatalf("staff PATCH to closed = %d, want 409: %s", rec.Code, rec.Body)
	}
	if code := problemOf(t, rec); code != "required_field_missing" {
		t.Fatalf("code = %q, want required_field_missing", code)
	}

	// Answer it, then close.
	rec = env.postJSONPut(t, "/api/v1/portal/tickets/"+ticketID+"/fields", cookie, csrf,
		map[string]any{"values": map[string]any{"region": "eu-west"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("answer fields = %d: %s", rec.Code, rec.Body)
	}
	var answered struct {
		Fields []struct {
			Key      string `json:"key"`
			Answered bool   `json:"answered"`
			Value    any    `json:"value"`
		} `json:"fields"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &answered); err != nil {
		t.Fatal(err)
	}
	if len(answered.Fields) != 1 || !answered.Fields[0].Answered || answered.Fields[0].Value != "eu-west" {
		t.Fatalf("answered fields = %+v", answered.Fields)
	}

	rec = env.postJSON(t, "/api/v1/portal/tickets/"+ticketID+"/close", cookie, csrf, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("close after answering = %d: %s", rec.Code, rec.Body)
	}
}

// A field answered on someone else's ticket is not found, and a value outside a select's options
// is refused with the field named.
func TestTicketFieldAnswersAreScopedAndValidated(t *testing.T) {
	env, _, _ := newFieldEnv(t)
	env.seedCustomer(t, "bob@example.test", "Bob")
	env.createField(t, map[string]any{
		"key": "region", "label": "Region", "type": "select",
		"options": []string{"eu-west", "us-east"},
	})

	cookie, csrf := env.login(t, "alice@example.test", "customer password 1")
	bobCookie, bobCSRF := env.login(t, "bob@example.test", "customer password 1")
	ticketID, _ := env.openTicket(t, cookie, csrf, "alice's")

	// Bob cannot answer Alice's ticket.
	rec := env.postJSONPut(t, "/api/v1/portal/tickets/"+ticketID+"/fields", bobCookie, bobCSRF,
		map[string]any{"values": map[string]any{"region": "eu-west"}})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("cross-customer answer = %d, want 404: %s", rec.Code, rec.Body)
	}
	problemOf(t, rec)

	// An unknown option is refused with the field named.
	rec = env.postJSONPut(t, "/api/v1/portal/tickets/"+ticketID+"/fields", cookie, csrf,
		map[string]any{"values": map[string]any{"region": "mars"}})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown option = %d, want 400: %s", rec.Code, rec.Body)
	}
	var problem struct {
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &problem); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(problem.Detail, "Region") {
		t.Fatalf("detail = %q, want the field named", problem.Detail)
	}

	// An empty body is a caller error rather than a no-op.
	rec = env.postJSONPut(t, "/api/v1/portal/tickets/"+ticketID+"/fields", cookie, csrf,
		map[string]any{"values": map[string]any{}})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty answer = %d, want 400: %s", rec.Code, rec.Body)
	}

	// Staff can answer on any ticket.
	rec = env.postJSONPut(t, "/api/v1/staff/tickets/"+ticketID+"/fields", env.support, env.supportCSRF,
		map[string]any{"values": map[string]any{"region": "us-east"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("staff answer = %d: %s", rec.Code, rec.Body)
	}
}

// ------------------------------------------------------------------ uploads --

// The upload's content-type allowlist is enforced over HTTP, and the meta endpoint advertises it so
// a form can say what is accepted before a 10 MB file is sent.
func TestAttachmentAllowlistOverHTTP(t *testing.T) {
	env, _, _ := newFieldEnv(t)
	cookie, csrf := env.login(t, "alice@example.test", "customer password 1")
	ticketID, _ := env.openTicket(t, cookie, csrf, "files")

	rec := env.uploadAttachment(t, "/api/v1/portal/tickets/"+ticketID+"/attachments?filename=page.html",
		cookie, csrf, "text/html", []byte("<script>alert(1)</script>"))
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("html upload = %d, want 415: %s", rec.Code, rec.Body)
	}
	if code := problemOf(t, rec); code != "attachment_type_not_allowed" {
		t.Fatalf("code = %q, want attachment_type_not_allowed", code)
	}
	var problem struct {
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &problem); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(problem.Detail, "application/pdf") {
		t.Fatalf("detail = %q, want it to list what is accepted", problem.Detail)
	}

	// An accepted type is stored, for the customer and for staff.
	rec = env.uploadAttachment(t, "/api/v1/portal/tickets/"+ticketID+"/attachments?filename=shot.png",
		cookie, csrf, "image/png", []byte("\x89PNG\r\n\x1a\n"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("png upload = %d: %s", rec.Code, rec.Body)
	}
	rec = env.uploadAttachment(t, "/api/v1/staff/tickets/"+ticketID+"/attachments?filename=note.txt",
		env.support, env.supportCSRF, "text/plain", []byte("a staff note"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("staff upload = %d: %s", rec.Code, rec.Body)
	}

	// The meta endpoint advertises the allowlist and the custom fields the form must draw.
	rec = env.get(t, "/api/v1/portal/tickets/meta", cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("meta = %d: %s", rec.Code, rec.Body)
	}
	var meta struct {
		AllowedAttachmentTypes []string `json:"allowed_attachment_types"`
		CustomFields           []struct {
			Key  string `json:"key"`
			Type string `json:"type"`
		} `json:"custom_fields"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &meta); err != nil {
		t.Fatal(err)
	}
	if len(meta.AllowedAttachmentTypes) == 0 {
		t.Fatal("meta does not advertise the attachment allowlist")
	}
	for _, accepted := range meta.AllowedAttachmentTypes {
		if accepted == "text/html" {
			t.Fatal("text/html is advertised as accepted")
		}
	}
}

// The staff meta endpoint carries the same configuration, so the staff form asks the same
// questions the customer's form does.
func TestStaffMetaCarriesCustomFields(t *testing.T) {
	env, _, _ := newFieldEnv(t)
	env.createField(t, map[string]any{
		"key": "callback_number", "label": "Callback number", "type": "text",
		"required_on_create": true,
	})

	rec := env.get(t, "/api/v1/staff/tickets/meta", env.support)
	if rec.Code != http.StatusOK {
		t.Fatalf("staff meta = %d: %s", rec.Code, rec.Body)
	}
	var meta struct {
		CustomFields []struct {
			Key              string `json:"key"`
			RequiredOnCreate bool   `json:"required_on_create"`
		} `json:"custom_fields"`
		AllowedAttachmentTypes []string `json:"allowed_attachment_types"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &meta); err != nil {
		t.Fatal(err)
	}
	if len(meta.CustomFields) != 1 || meta.CustomFields[0].Key != "callback_number" ||
		!meta.CustomFields[0].RequiredOnCreate {
		t.Fatalf("staff meta fields = %+v", meta.CustomFields)
	}
	if len(meta.AllowedAttachmentTypes) == 0 {
		t.Fatal("staff meta does not advertise the attachment allowlist")
	}
}

// A ticket's custom fields survive a read as staff and as the customer, and an archived field's
// answer stays readable on the ticket that gave it.
func TestArchivedFieldAnswerSurvivesOverHTTP(t *testing.T) {
	env, _, _ := newFieldEnv(t)
	fieldID := env.createField(t, map[string]any{
		"key": "resolution", "label": "Root cause", "type": "text",
		"required_to_close": true,
	})

	cookie, csrf := env.login(t, "alice@example.test", "customer password 1")
	ticketID, _ := env.openTicket(t, cookie, csrf, "answered then retired")

	rec := env.postJSONPut(t, "/api/v1/portal/tickets/"+ticketID+"/fields", cookie, csrf,
		map[string]any{"values": map[string]any{"resolution": "disk full"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("answer = %d: %s", rec.Code, rec.Body)
	}

	// Retire the field with the DELETE route.
	rec = env.deleteJSON(t, "/api/v1/staff/ticket-fields/"+fieldID, env.support, env.supportCSRF)
	if rec.Code != http.StatusOK {
		t.Fatalf("archive = %d: %s", rec.Code, rec.Body)
	}
	var archived struct {
		Archived bool `json:"archived"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &archived); err != nil {
		t.Fatal(err)
	}
	if !archived.Archived {
		t.Fatalf("archived = %+v", archived)
	}

	// The ticket still shows the answer; a retired field is not a lost record.
	rec = env.get(t, "/api/v1/portal/tickets/"+ticketID, cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("thread = %d: %s", rec.Code, rec.Body)
	}
	var thread struct {
		Fields []struct {
			Key      string `json:"key"`
			Answered bool   `json:"answered"`
			Value    any    `json:"value"`
			Archived bool   `json:"archived"`
		} `json:"fields"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &thread); err != nil {
		t.Fatal(err)
	}
	if len(thread.Fields) != 1 || !thread.Fields[0].Answered || thread.Fields[0].Value != "disk full" {
		t.Fatalf("thread fields = %+v", thread.Fields)
	}

	// And the close is no longer blocked, because an archived field is not required.
	rec = env.postJSON(t, "/api/v1/portal/tickets/"+ticketID+"/close", cookie, csrf, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("close after archiving = %d: %s", rec.Code, rec.Body)
	}

	// The settings screen can still see the definition.
	rec = env.get(t, "/api/v1/staff/ticket-fields?include_archived=true", env.support)
	var listed struct {
		Fields []struct {
			ID       string `json:"id"`
			Archived bool   `json:"archived"`
		} `json:"fields"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Fields) != 1 || !listed.Fields[0].Archived {
		t.Fatalf("archived list = %+v", listed.Fields)
	}
	rec = env.get(t, "/api/v1/staff/ticket-fields", env.support)
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Fields) != 0 {
		t.Fatalf("live list still offers the archived field: %+v", listed.Fields)
	}
}
