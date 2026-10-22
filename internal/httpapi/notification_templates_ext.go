// The template editor's two read-shaped helpers: the vocabulary an event supplies, and a
// preview of a draft.
//
// The list, read, save and reset of templates already exist (notifications.go). What was
// missing is the half an operator needs *before* saving: which words may a template use, and
// what the sentence looks like once those words are filled in. Without it the editor was a
// text box whose mistakes were discovered by a customer.
//
// Three decisions shape these routes:
//
//   - The vocabulary is served rather than embedded in the client. The list of placeholders
//     lives next to the events that supply them (internal/notify/template_vars.go), and a
//     client that hardcoded its own copy would offer a placeholder the server refuses — or
//     hide one the server accepts.
//   - The preview is a POST. A draft template is too long for a query string, and a GET would
//     put the operator's draft into access logs and browser history. The route stores nothing:
//     it is a read with a body, declared CSRF-guarded because every non-GET session route is.
//   - A draft that uses a placeholder its event does not supply is refused here with 422
//     `unknown_placeholder`, and again at save time inside notify.SaveTemplate. The renderer
//     leaves an unknown placeholder as literal text — correct at send time, where a visible
//     "{{invoice_number}}" tells an operator something is wrong, and wrong in an inbox, where
//     the customer is the one reading it.
package httpapi

import (
	"errors"
	"net/http"

	"github.com/catalyst/billing/internal/authn"
	"github.com/catalyst/billing/internal/notify"
)

func init() {
	RegisterRoutes("51-notification-template-editor", registerNotificationTemplateEditor)
}

func registerNotificationTemplateEditor(r *router) {
	r.staffGet("/api/v1/staff/notification-template-variables", authn.PermNotificationWrite,
		r.s.handleTemplateVariables)

	r.add(Route{
		Method:     http.MethodPost,
		Pattern:    "/api/v1/staff/notification-templates/{event_key}/preview",
		Access:     AccessPermission,
		Permission: authn.PermNotificationWrite,
		StaffOnly:  true,
		// A mutation in method only: the draft is rendered and discarded, so there is nothing
		// for an Idempotency-Key to protect and nothing for the replayer to store.
		CSRF:    true,
		Rate:    RateRead,
		Handler: r.s.handlePreviewTemplate,
	})
}

// templateVariableJSON is one placeholder an event supplies.
func templateVariableJSON(v notify.TemplateVariable) map[string]any {
	return map[string]any{
		"name":        v.Name,
		"description": v.Description,
		"sample":      v.Sample,
	}
}

// handleTemplateVariables serves every event's vocabulary, in catalogue order.
//
// One response rather than one per event: the editor renders the chips beside the template it
// is editing, and a screen that had to ask per event would either fetch on every selection or
// ship a second, drifting copy of the list.
func (s *Server) handleTemplateVariables(w http.ResponseWriter, r *http.Request) {
	catalogue := notify.Catalogue()
	out := make([]map[string]any, 0, len(catalogue))
	for _, e := range catalogue {
		variables, err := notify.TemplateVariables(e.Key)
		if err != nil {
			// The catalogue and the vocabulary are written side by side, so this is a build
			// error rather than a request error: report it instead of serving a partial list
			// that would look like an event with no placeholders.
			s.cfg.Logger.ErrorContext(r.Context(), "event has no template vocabulary",
				"event_key", e.Key, "error", err)
			writeProblem(w, http.StatusInternalServerError, "internal_error",
				"the template vocabulary could not be read")
			return
		}
		items := make([]map[string]any, 0, len(variables))
		for _, v := range variables {
			items = append(items, templateVariableJSON(v))
		}
		out = append(out, map[string]any{
			"event_key": e.Key,
			"name":      e.Name,
			// force is the transactional flag: a customer cannot opt out of these, so an edit
			// to their wording reaches everybody.
			"force":     e.Force,
			"variables": items,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out})
}

// handlePreviewTemplate renders a draft with the event's sample data.
//
// The body is the same shape the save route takes, and every text field in it replaces the
// stored one (see handlePutTemplate), so the draft is built from the body alone — no read of
// the stored row is needed to know what the save would produce. That matters beyond saving a
// query: a preview that merged differently from the save would preview a template nobody could
// store.
func (s *Server) handlePreviewTemplate(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("event_key")
	if !notify.IsKnownEvent(key) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such notification event")
		return
	}
	var in templateBody
	if !s.decodeJSON(w, r, &in, 256<<10) {
		return
	}

	draft := notify.Template{
		EventKey:   key,
		Name:       in.Name,
		Subject:    in.Subject,
		Body:       in.Body,
		InAppTitle: in.InAppTitle,
		InAppBody:  in.InAppBody,
		InAppURL:   in.InAppURL,
		CC:         in.CC,
		BCC:        in.BCC,
	}

	preview, err := notify.PreviewTemplate(r.Context(), s.pool, draft)
	if err != nil {
		s.writeTemplatePreviewError(w, r, err)
		return
	}

	variables, err := notify.TemplateVariables(key)
	if err != nil {
		s.writeTemplatePreviewError(w, r, err)
		return
	}
	items := make([]map[string]any, 0, len(variables))
	for _, v := range variables {
		items = append(items, templateVariableJSON(v))
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"event_key": key,
		"force":     notify.ForceEvent(key),
		// The mail channel as it would be sent: links made absolute, subject stripped of the
		// control characters a substituted value could carry.
		"mail": map[string]any{
			"subject":       preview.Mail.Subject,
			"body":          preview.Mail.Body,
			"cc_addresses":  preview.Mail.CC,
			"bcc_addresses": preview.Mail.BCC,
		},
		// The inbox row as it would be stored: panel-relative links, which is what the panel can
		// resolve behind a proxy.
		"in_app": map[string]any{
			"title": preview.InApp.InAppTitle,
			"body":  preview.InApp.InAppBody,
			"url":   preview.InApp.InAppURL,
		},
		"variables": items,
	})
}

// writeTemplatePreviewError maps the preview's failures to problem documents.
//
// Only two errors are possible and both are the client's: an event that does not exist, and a
// draft whose placeholders the event does not supply. Anything else is the settings read
// falling over, which notify already tolerates.
func (s *Server) writeTemplatePreviewError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, notify.ErrUnknownPlaceholder):
		// The detail names the placeholders, because the operator's next action is to fix the
		// spelling and the screen cannot know which one was wrong from the code alone.
		writeProblem(w, http.StatusUnprocessableEntity, "unknown_placeholder", err.Error())
	case errors.Is(err, notify.ErrUnknownEvent):
		writeProblem(w, http.StatusNotFound, "not_found", "no such notification event")
	default:
		s.cfg.Logger.ErrorContext(r.Context(), "template preview failed",
			"path", r.URL.Path, "error", err)
		writeProblem(w, http.StatusInternalServerError, "internal_error",
			"the preview could not be rendered")
	}
}
