// The notification surface: a customer's inbox and preferences, and the staff template
// editor.
//
// Two audiences, two rules:
//
//   - /api/v1/portal/notifications** is the caller's own inbox. The user id comes from the
//     session and goes into the WHERE clause of every statement, so another user's
//     notification id matches no row and answers 404 — the same answer as an id that never
//     existed, which is what stops the endpoint being an oracle for "does this id exist".
//     It is deliberately *not* AccessUser: the permission-coverage contract requires every
//     /api/v1/portal route to declare a permission, and customer:read:self /
//     customer:write:self are held by every authenticated account as a baseline, so the
//     check costs nothing and still refuses an unauthenticated caller before the handler.
//
//   - /api/v1/staff/notification-templates is the operator's wording. It requires
//     notification:write, which support does not hold: support answers "why did I get this",
//     it does not decide what the mail says. Every write is audited, because changing the
//     text of a payment-failed notice is a change to what the business tells its customers.

package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/catalyst/billing/internal/audit"
	"github.com/catalyst/billing/internal/authn"
	"github.com/catalyst/billing/internal/notify"
	"github.com/catalyst/billing/internal/paginate"
	"github.com/jackc/pgx/v5"
)

func init() {
	RegisterRoutes("50-notifications", func(r *router) { r.s.registerNotificationRoutes(r) })
}

func (s *Server) registerNotificationRoutes(r *router) {
	// The customer's own inbox.
	r.get("/api/v1/portal/notifications", authn.PermCustomerReadSelf, s.handleListNotifications)
	r.get("/api/v1/portal/notifications/unread-count", authn.PermCustomerReadSelf, s.handleUnreadCount)
	r.mutation(http.MethodPost, "/api/v1/portal/notifications/{id}/read",
		authn.PermCustomerWriteSelf, s.handleMarkNotificationRead)
	r.mutation(http.MethodPost, "/api/v1/portal/notifications/read-all",
		authn.PermCustomerWriteSelf, s.handleMarkAllNotificationsRead)

	// Preferences are read and written separately: reading is a GET so the screen can render
	// without a token, writing is a PUT because it replaces the whole set in one request.
	r.get("/api/v1/portal/notifications/preferences", authn.PermCustomerReadSelf,
		s.handleGetNotificationPreferences)
	r.mutation(http.MethodPut, "/api/v1/portal/notifications/preferences",
		authn.PermCustomerWriteSelf, s.handlePutNotificationPreferences)

	// The staff template editor.
	r.staffGet("/api/v1/staff/notification-templates", authn.PermNotificationWrite,
		s.handleListTemplates)
	r.staffGet("/api/v1/staff/notification-templates/{event_key}", authn.PermNotificationWrite,
		s.handleGetTemplate)
	r.staffMutation(http.MethodPut, "/api/v1/staff/notification-templates/{event_key}",
		authn.PermNotificationWrite, s.handlePutTemplate)
	// Reset one template to the shipped default. It is a mutation, not a delete: the event
	// always has wording, and "deleted" would mean an event that cannot be sent at all.
	r.staffMutation(http.MethodPost, "/api/v1/staff/notification-templates/{event_key}/reset",
		authn.PermNotificationWrite, s.handleResetTemplate)

	// The test mail (NTF-09). It is a mutation because it sends something, and it takes an
	// Idempotency-Key for the same reason a payment does: a retried request must not produce a
	// second mail in the operator's inbox.
	r.staffMutation(http.MethodPost, "/api/v1/staff/notifications/test-email",
		authn.PermNotificationWrite, s.handleSendTestEmail)
}

// ------------------------------------------------------------- customer read --

// notificationJSON is the wire shape of one inbox row.
func notificationJSON(n notify.Notification) map[string]any {
	out := map[string]any{
		"id":         n.ID,
		"event_key":  n.EventKey,
		"title":      n.Title,
		"body":       n.Body,
		"url":        n.URL,
		"unread":     n.Unread(),
		"created_at": n.CreatedAt.UTC(),
	}
	if n.ReadAt != nil {
		out["read_at"] = n.ReadAt.UTC()
	}
	return out
}

// handleListNotifications serves one page of the caller's inbox.
func (s *Server) handleListNotifications(w http.ResponseWriter, r *http.Request) {
	user, ok := userFrom(r.Context())
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "authentication_required", "sign in to continue")
		return
	}
	limit, cursor, err := ParsePage(r)
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_cursor",
			"the limit or cursor is not valid")
		return
	}

	filter := notify.InboxFilter{UnreadOnly: r.URL.Query().Get("unread") == "true"}
	rows, next, err := notify.List(r.Context(), s.pool, user.ID, filter, limit, cursor)
	if err != nil {
		s.writeNotifyError(w, r, err)
		return
	}

	items := make([]map[string]any, 0, len(rows))
	for _, n := range rows {
		items = append(items, notificationJSON(n))
	}
	var nextCursor *string
	if next != "" {
		nextCursor = &next
	}
	writeJSON(w, http.StatusOK, Page[map[string]any]{Data: items, NextCursor: nextCursor})
}

// handleUnreadCount serves the badge number.
//
// It is a separate endpoint rather than a field on the list because the badge is polled (or
// pushed) far more often than the list is opened, and making the shell fetch twenty
// notifications to render one integer is a waste that shows up as a slow sidebar.
func (s *Server) handleUnreadCount(w http.ResponseWriter, r *http.Request) {
	user, ok := userFrom(r.Context())
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "authentication_required", "sign in to continue")
		return
	}
	n, err := notify.UnreadCount(r.Context(), s.pool, user.ID)
	if err != nil {
		s.writeNotifyError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"unread": n})
}

// ----------------------------------------------------------- customer write --

// notificationID validates a path id before it reaches SQL.
//
// A malformed uuid would make the statement fail rather than match nothing, and the error
// would surface as a 500 for what is really "no such notification". Checking the shape first
// keeps 404 meaning 404.
func notificationID(id string) bool {
	if len(id) != 36 {
		return false
	}
	for i, r := range id {
		switch i {
		case 8, 13, 18, 23:
			if r != '-' {
				return false
			}
		default:
			if (r < '0' || r > '9') && (r < 'a' || r > 'f') && (r < 'A' || r > 'F') {
				return false
			}
		}
	}
	return true
}

// handleMarkNotificationRead marks one notification read.
func (s *Server) handleMarkNotificationRead(w http.ResponseWriter, r *http.Request) {
	user, ok := userFrom(r.Context())
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "authentication_required", "sign in to continue")
		return
	}
	id := r.PathValue("id")
	if !notificationID(id) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such notification")
		return
	}
	// The user id is the session's, so a notification belonging to somebody else updates no
	// row and is reported exactly like a missing one.
	if err := notify.MarkRead(r.Context(), s.pool, user.ID, id); err != nil {
		s.writeNotifyError(w, r, err)
		return
	}
	n, err := notify.UnreadCount(r.Context(), s.pool, user.ID)
	if err != nil {
		s.writeNotifyError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "read", "unread": n})
}

// handleMarkAllNotificationsRead marks the caller's whole inbox read.
func (s *Server) handleMarkAllNotificationsRead(w http.ResponseWriter, r *http.Request) {
	user, ok := userFrom(r.Context())
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "authentication_required", "sign in to continue")
		return
	}
	n, err := notify.MarkAllRead(r.Context(), s.pool, user.ID)
	if err != nil {
		s.writeNotifyError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "read", "marked": n, "unread": 0})
}

// ---------------------------------------------------------------- preferences --

// handleGetNotificationPreferences serves the whole catalogue resolved for the caller.
func (s *Server) handleGetNotificationPreferences(w http.ResponseWriter, r *http.Request) {
	user, ok := userFrom(r.Context())
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "authentication_required", "sign in to continue")
		return
	}
	rows, err := notify.Preferences(r.Context(), s.pool, user.ID)
	if err != nil {
		s.writeNotifyError(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, p := range rows {
		out = append(out, map[string]any{
			"event_key":  p.EventKey,
			"name":       p.Name,
			"force":      p.Force,
			"preference": p.Preference,
			"stored":     p.Stored,
			"mail":       p.Mail,
			"in_app":     p.InApp,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out})
}

// preferenceInput is one requested change.
type preferenceInput struct {
	EventKey   string `json:"event_key"`
	Preference string `json:"preference"`
}

// handlePutNotificationPreferences replaces the caller's choices.
//
// A PUT rather than a PATCH because the screen submits the whole set: the request is the
// complete answer to "what do you want", so an event omitted from the body is left at its
// default rather than silently kept at a stale value.
func (s *Server) handlePutNotificationPreferences(w http.ResponseWriter, r *http.Request) {
	user, ok := userFrom(r.Context())
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "authentication_required", "sign in to continue")
		return
	}
	var req struct {
		Preferences []preferenceInput `json:"preferences"`
	}
	if !s.decodeJSON(w, r, &req, 64<<10) {
		return
	}

	// Validate everything before writing anything: a request that turns three events off and
	// fails on the fourth should not leave the account half-configured.
	for _, p := range req.Preferences {
		if !notify.IsKnownEvent(p.EventKey) {
			writeProblem(w, http.StatusUnprocessableEntity, "unknown_event",
				"there is no notification event called "+p.EventKey)
			return
		}
		if notify.ForceEvent(p.EventKey) &&
			(p.Preference == notify.PreferenceChoiceOff || p.Preference == notify.PreferenceNever) {
			writeProblem(w, http.StatusUnprocessableEntity, "force_event",
				"this notification is required and cannot be turned off: "+p.EventKey)
			return
		}
		switch p.Preference {
		case notify.PreferenceChoiceOn, notify.PreferenceChoiceOff, notify.PreferenceNever:
		default:
			writeProblem(w, http.StatusUnprocessableEntity, "invalid_preference",
				"preference must be choice_on, choice_off or never")
			return
		}
	}

	err := s.pool.InTx(r.Context(), func(ctx context.Context, tx pgx.Tx) error {
		for _, p := range req.Preferences {
			if err := notify.SetPreference(ctx, tx, user.ID, p.EventKey, p.Preference); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		s.writeNotifyError(w, r, err)
		return
	}

	rows, err := notify.Preferences(r.Context(), s.pool, user.ID)
	if err != nil {
		s.writeNotifyError(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, p := range rows {
		out = append(out, map[string]any{
			"event_key":  p.EventKey,
			"name":       p.Name,
			"force":      p.Force,
			"preference": p.Preference,
			"stored":     p.Stored,
			"mail":       p.Mail,
			"in_app":     p.InApp,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out})
}

// ------------------------------------------------------------- staff templates --

// templateJSON is the wire shape of one template.
func templateJSON(t notify.Template) map[string]any {
	return map[string]any{
		"event_key":      t.EventKey,
		"name":           t.Name,
		"subject":        t.Subject,
		"body":           t.Body,
		"in_app_title":   t.InAppTitle,
		"in_app_body":    t.InAppBody,
		"in_app_url":     t.InAppURL,
		"enabled":        t.Enabled,
		"mail_enabled":   t.MailEnabled,
		"in_app_enabled": t.InAppEnabled,
		"cc_addresses":   t.CC,
		"bcc_addresses":  t.BCC,
		"updated_at":     t.UpdatedAt.UTC(),
	}
}

// handleListTemplates lists every template, plus the catalogue entry for each so the screen
// can show an event that has no row yet.
func (s *Server) handleListTemplates(w http.ResponseWriter, r *http.Request) {
	templates, err := notify.Templates(r.Context(), s.pool)
	if err != nil {
		s.writeNotifyError(w, r, err)
		return
	}
	byKey := make(map[string]notify.Template, len(templates))
	for _, t := range templates {
		byKey[t.EventKey] = t
	}
	// Ordered by the catalogue rather than by the table, so the screen reads in the order an
	// operator thinks: security, money, service, support.
	out := make([]map[string]any, 0, len(templates))
	for _, e := range notify.Catalogue() {
		t, ok := byKey[e.Key]
		if !ok {
			// No row: the seed has not run in this installation. Report the event anyway, with
			// its catalogue name and a null body, so the omission is visible instead of the
			// event silently missing from the screen.
			out = append(out, map[string]any{
				"event_key": e.Key, "name": e.Name, "force": e.Force, "seeded": false,
			})
			continue
		}
		row := templateJSON(t)
		row["force"] = e.Force
		row["seeded"] = true
		out = append(out, row)
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out})
}

// handleGetTemplate serves one template.
func (s *Server) handleGetTemplate(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("event_key")
	if !notify.IsKnownEvent(key) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such notification event")
		return
	}
	t, err := notify.TemplateFor(r.Context(), s.pool, key)
	if err != nil {
		s.writeNotifyError(w, r, err)
		return
	}
	row := templateJSON(t)
	row["force"] = notify.ForceEvent(key)
	writeJSON(w, http.StatusOK, row)
}

// templateBody is the editable part of a template.
//
// The event key is not in the body: it is the identity of the template and lives in the path,
// so a body cannot rename a template to another event and leave the old one missing.
type templateBody struct {
	Name         string `json:"name"`
	Subject      string `json:"subject"`
	Body         string `json:"body"`
	InAppTitle   string `json:"in_app_title"`
	InAppBody    string `json:"in_app_body"`
	InAppURL     string `json:"in_app_url"`
	Enabled      *bool  `json:"enabled"`
	MailEnabled  *bool  `json:"mail_enabled"`
	InAppEnabled *bool  `json:"in_app_enabled"`
	CC           string `json:"cc_addresses"`
	BCC          string `json:"bcc_addresses"`
}

// handlePutTemplate replaces one template's wording and channel toggles.
func (s *Server) handlePutTemplate(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("event_key")
	if !notify.IsKnownEvent(key) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such notification event")
		return
	}
	var in templateBody
	if !s.decodeJSON(w, r, &in, 256<<10) {
		return
	}

	// Start from the stored row (or the shipped default when there is none) and apply the
	// submitted fields. That way a client that only edits the subject does not blank the
	// in-app title, which is the classic "PATCH implemented as PUT" data loss.
	current, err := notify.TemplateFor(r.Context(), s.pool, key)
	if errors.Is(err, notify.ErrTemplateNotFound) {
		current = defaultTemplateFor(key)
	} else if err != nil {
		s.writeNotifyError(w, r, err)
		return
	}

	current.Name = orString(in.Name, current.Name)
	current.Subject = in.Subject
	current.Body = in.Body
	current.InAppTitle = in.InAppTitle
	current.InAppBody = in.InAppBody
	current.InAppURL = in.InAppURL
	if in.Enabled != nil {
		current.Enabled = *in.Enabled
	}
	if in.MailEnabled != nil {
		current.MailEnabled = *in.MailEnabled
	}
	if in.InAppEnabled != nil {
		current.InAppEnabled = *in.InAppEnabled
	}
	if in.CC != "" {
		current.CC = in.CC
	}
	if in.BCC != "" {
		current.BCC = in.BCC
	}

	var saved notify.Template
	err = s.pool.InTx(r.Context(), func(ctx context.Context, tx pgx.Tx) error {
		var err error
		saved, err = notify.SaveTemplate(ctx, tx, current)
		if err != nil {
			return err
		}
		// The event key is not a uuid, so it goes in `changes` rather than `target_id`: the
		// column is typed, and a text key smuggled into it would fail the insert and roll back
		// the edit.
		ev := s.auditEvent(r, "notification.template.update", "notification_template", "",
			audit.OutcomeSuccess).WithChanges(map[string]any{
			"event_key":      key,
			"subject":        saved.Subject,
			"enabled":        saved.Enabled,
			"mail_enabled":   saved.MailEnabled,
			"in_app_enabled": saved.InAppEnabled,
			// The body itself is not recorded: it is long, and the useful audit fact is that
			// the wording changed and who changed it.
			"body_changed": true,
		})
		return audit.Record(ctx, tx, ev)
	})
	if err != nil {
		s.writeNotifyError(w, r, err)
		return
	}
	row := templateJSON(saved)
	row["force"] = notify.ForceEvent(key)
	writeJSON(w, http.StatusOK, row)
}

// handleResetTemplate restores the shipped wording for one event.
func (s *Server) handleResetTemplate(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("event_key")
	if !notify.IsKnownEvent(key) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such notification event")
		return
	}
	def := defaultTemplateFor(key)
	if def.EventKey == "" {
		// The event is in the catalogue but this build ships no default for it. That is a
		// programming error, and 404 is the honest answer rather than blanking the row.
		writeProblem(w, http.StatusNotFound, "not_found", "no such notification event")
		return
	}

	var saved notify.Template
	err := s.pool.InTx(r.Context(), func(ctx context.Context, tx pgx.Tx) error {
		var err error
		saved, err = notify.SaveTemplate(ctx, tx, def)
		if err != nil {
			return err
		}
		ev := s.auditEvent(r, "notification.template.reset", "notification_template", "",
			audit.OutcomeSuccess).WithChanges(map[string]any{"event_key": key})
		return audit.Record(ctx, tx, ev)
	})
	if err != nil {
		s.writeNotifyError(w, r, err)
		return
	}
	row := templateJSON(saved)
	row["force"] = notify.ForceEvent(key)
	writeJSON(w, http.StatusOK, row)
}

// defaultTemplateFor returns the shipped default for one event.
func defaultTemplateFor(key string) notify.Template {
	for _, t := range notify.DefaultTemplates() {
		if t.EventKey == key {
			return t
		}
	}
	return notify.Template{}
}

// orString returns v, or fallback when v is blank.
func orString(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return v
}

// ---------------------------------------------------------------- errors --

// writeNotifyError maps the package's errors to stable codes.
//
// A client branches on the code, so the mapping lives in one place rather than in each
// handler. Nothing here echoes a database message: the detail is written here, not taken
// from the error.
func (s *Server) writeNotifyError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, notify.ErrTemplateNotFound):
		// A missing template is an operator problem, not a client one: the event exists and
		// the seed has not run. 503 with a code the panel can surface as "run the seed".
		s.cfg.Logger.ErrorContext(r.Context(), "notification template is missing",
			"path", r.URL.Path, "error", err)
		writeProblem(w, http.StatusServiceUnavailable, "template_missing",
			"this notification has no template configured")
	case errors.Is(err, notify.ErrNoNotification):
		writeProblem(w, http.StatusNotFound, "not_found", "no such notification")
	case errors.Is(err, notify.ErrForceEvent):
		writeProblem(w, http.StatusUnprocessableEntity, "force_event",
			"this notification is required and cannot be turned off")
	case errors.Is(err, notify.ErrUnknownEvent):
		writeProblem(w, http.StatusUnprocessableEntity, "unknown_event",
			"there is no such notification event")
	case errors.Is(err, notify.ErrUnknownPlaceholder):
		// A template that uses a word its event does not supply would reach a customer with the
		// plumbing showing, so the save is refused rather than stored. The detail names the
		// placeholders: the operator's next action is to fix the spelling. The field list is what
		// makes the refusal renderable in another language, and what lets the editor point at the
		// textarea the bad word is in.
		fields := make([]fieldProblem, 0, 4)
		for _, f := range notify.UnknownPlaceholderFields(err) {
			fields = append(fields, fieldProblem{Field: f.Field, Code: "unknown_placeholder", Value: f.Name})
		}
		writeFieldProblem(w, http.StatusUnprocessableEntity, "unknown_placeholder", err.Error(), fields)
	case errors.Is(err, paginate.ErrInvalidCursor):
		writeProblem(w, http.StatusBadRequest, "invalid_cursor", "the cursor is invalid")
	default:
		s.cfg.Logger.ErrorContext(r.Context(), "notification request failed",
			"path", r.URL.Path, "error", err)
		writeProblem(w, http.StatusInternalServerError, "internal_error",
			"the request could not be completed")
	}
}

// ------------------------------------------------------------- problem fields --

// fieldProblem is one machine-readable field failure inside a problem document.
//
// It is deliberately not part of the generic `problem` struct in server.go: a field list belongs
// only to the refusals that have fields, and adding a nullable array to every error response in
// the product would make every client handle a shape that is almost always empty.
type fieldProblem struct {
	// Field is the input the failure is about, named the way the editor labels it.
	Field string `json:"field"`
	// Code is a stable identifier the client translates: required, invalid, duplicate, unknown,
	// unknown_placeholder.
	Code string `json:"code"`
	// Value is the offending value when there is one worth naming (a placeholder name), never a
	// secret.
	Value string `json:"value,omitempty"`
}

// writeFieldProblem writes a problem document that also carries a field list.
//
// The document is the same RFC 9457 shape as every other refusal — same `type`, `title`, `status`
// and `code` — so a client that already branches on problems needs no special case; `fields` is
// the addition. Splitting the envelope would mean two error parsers in the client, which is
// exactly the kind of duplication that lets one of them fall behind.
func writeFieldProblem(w http.ResponseWriter, status int, code, detail string, fields []fieldProblem) {
	body := map[string]any{
		"type":   "about:blank",
		"title":  http.StatusText(status),
		"status": status,
		"code":   code,
	}
	if detail != "" {
		body["detail"] = detail
	}
	// An empty list is omitted rather than sent as []: "this refusal has no fields" and "this
	// refusal is about no particular field" are the same fact, and a client that has to tell an
	// empty array from a missing key will get it wrong once.
	if len(fields) > 0 {
		body["fields"] = fields
	}
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
	// After the response, never before: a log write must not be able to change the answer.
	recordProblem(w, status, code, detail)
}

// --------------------------------------------------------------- test email --

// testEmailBody is the request shape for a test mail.
type testEmailBody struct {
	// To is optional: an empty value mails the caller, which is the case the button offers.
	To string `json:"to"`
}

// handleSendTestEmail queues a test message through the real delivery path (NTF-09).
//
// It answers 202 rather than 200 because nothing has been delivered yet: the message is in the
// outbox and the worker will claim it. That is the honest status — and it is also the useful one,
// because "queued" means an operator can watch the email log go from queued to sent, which is what
// proves the relay works. A synchronous 200 would prove the SMTP credentials and say nothing about
// the path every invoice takes.
func (s *Server) handleSendTestEmail(w http.ResponseWriter, r *http.Request) {
	// An empty body is "mail me", which is the case the button offers: the recipient field is the
	// only input and it defaults to the caller. Requiring `{}` would make the simplest request the
	// one a client has to know a detail to make.
	var in testEmailBody
	if r.ContentLength != 0 {
		if !s.decodeJSON(w, r, &in, 8<<10) {
			return
		}
	}
	user, ok := userFrom(r.Context())
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "authentication_required", "sign in to continue")
		return
	}
	if s.notify == nil {
		writeProblem(w, http.StatusServiceUnavailable, "notifications_unavailable",
			"the notification queue is not configured")
		return
	}

	to := strings.TrimSpace(in.To)
	if to == "" {
		to = user.Email
	}
	var fields []fieldProblem
	if to == "" {
		fields = append(fields, fieldProblem{Field: "to", Code: "required"})
	} else if !strings.Contains(to, "@") || strings.ContainsAny(to, " \t\r\n") {
		// The address lands in a mail header. A space or a newline in it is not a formatting
		// preference, it is how one message becomes two, and the SMTP sender would refuse the
		// whole delivery later — where the operator would see a failed row and no reason.
		fields = append(fields, fieldProblem{Field: "to", Code: "invalid"})
	}
	if len(fields) > 0 {
		writeFieldProblem(w, http.StatusUnprocessableEntity, "invalid_input",
			"the recipient address is not usable", fields)
		return
	}

	var id string
	err := s.pool.InTx(r.Context(), func(ctx context.Context, tx pgx.Tx) error {
		var err error
		id, err = notify.SendTestEmail(ctx, tx, s.notify, notify.TestEmailInput{
			To: to, RequestedBy: user.Email,
		})
		if err != nil {
			return err
		}
		ev := s.auditEvent(r, "notification.test_email", "notification", "",
			audit.OutcomeSuccess).WithChanges(map[string]any{"to": to})
		return audit.Record(ctx, tx, ev)
	})
	if err != nil {
		s.writeNotifyError(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"id":     id,
		"to":     to,
		"status": "queued",
	})
}
