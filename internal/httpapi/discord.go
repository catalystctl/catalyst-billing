// Discord notification HTTP surface (GRW-04, NTF-13).
//
// The webhook URL is a credential, so this file has exactly one rule that shapes it: the
// read returns whether a webhook is configured, never the URL. That is why the response is
// growth.DiscordConfig (which has no URL field) rather than the settings rows.
//
// The write reuses settings:write: Discord configuration *is* system settings, and asking
// for a new permission for it would mean a role could not configure the channel it already
// administers.
//
// Notify itself is not a route. It is exported from internal/growth for the event paths to
// call after their transaction commits (see the package report for the call sites).
package httpapi

import (
	"context"
	"net/http"
	"strings"

	"github.com/catalyst/billing/internal/audit"
	"github.com/catalyst/billing/internal/authn"
	"github.com/catalyst/billing/internal/growth"
	"github.com/jackc/pgx/v5"
)

func init() {
	RegisterRoutes("76-growth-discord", func(r *router) { r.s.registerDiscordRoutes(r) })
}

func (s *Server) registerDiscordRoutes(r *router) {
	r.staffGet("/api/v1/staff/discord", authn.PermReportRead, s.handleDiscordConfig)
	r.staffMutation(http.MethodPut, "/api/v1/staff/discord",
		authn.PermSettingsWrite, s.handleUpdateDiscordConfig)
}

// handleDiscordConfig returns the selection and whether a webhook is stored.
//
// The URL is deliberately absent: a settings page that echoes a credential back is a
// credential leak in every proxy log and browser cache between here and the operator.
func (s *Server) handleDiscordConfig(w http.ResponseWriter, r *http.Request) {
	cfg, err := growth.DiscordConfigFor(r.Context(), s.pool)
	if err != nil {
		s.cfg.Logger.ErrorContext(r.Context(), "read discord config failed", "error", err)
		writeProblem(w, http.StatusInternalServerError, "internal_error",
			"the request could not be completed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled":            cfg.Enabled,
		"events":             cfg.Events,
		"webhook_configured": cfg.WebhookConfigured,
		"available_events":   growth.KnownEvents,
	})
}

// updateDiscordRequest is the body of a configuration change.
//
// WebhookURL is write-only. Omitting it keeps the stored credential, because the operator
// cannot see the old value to send it back; an explicit empty string with enabled=false is
// how the webhook is removed.
type updateDiscordRequest struct {
	Enabled    bool     `json:"enabled"`
	WebhookURL string   `json:"webhook_url"`
	Events     []string `json:"events"`
	// ClearWebhook removes the stored URL. It is a separate flag rather than "empty means
	// clear" so that saving the event selection from a screen that does not hold the secret
	// cannot accidentally delete it.
	ClearWebhook bool `json:"clear_webhook"`
}

func (s *Server) handleUpdateDiscordConfig(w http.ResponseWriter, r *http.Request) {
	var in updateDiscordRequest
	if !s.decodeJSON(w, r, &in, 16<<10) {
		return
	}
	webhook := strings.TrimSpace(in.WebhookURL)
	if in.ClearWebhook {
		webhook = ""
	}

	var cfg growth.DiscordConfig
	err := s.pool.InTx(r.Context(), func(ctx context.Context, tx pgx.Tx) error {
		if err := growth.SetDiscordConfig(ctx, tx, in.Enabled, webhook, in.Events); err != nil {
			return err
		}
		if in.ClearWebhook {
			// SetDiscordConfig treats an empty URL as "keep the existing one"; clearing is
			// an explicit statement, so it is written here.
			if _, err := tx.Exec(ctx, `
				UPDATE settings SET value = '', updated_at = now()
				WHERE group_key = 'discord' AND key = 'webhook_url'`); err != nil {
				return err
			}
		}
		ev := s.auditEvent(r, "settings.discord.update", "settings", "",
			audit.OutcomeSuccess).WithChanges(map[string]any{
			"enabled":       in.Enabled,
			"events":        in.Events,
			"webhook_clear": in.ClearWebhook,
			// The URL itself is never recorded: the audit redactor matches on the field
			// name, but not writing it at all is stronger than relying on the redactor.
			"webhook_set": webhook != "",
		})
		return audit.Record(ctx, tx, ev)
	})
	if err != nil {
		s.cfg.Logger.ErrorContext(r.Context(), "update discord config failed", "error", err)
		writeProblem(w, http.StatusUnprocessableEntity, "invalid_input",
			"the discord configuration could not be saved")
		return
	}

	cfg, err = growth.DiscordConfigFor(r.Context(), s.pool)
	if err != nil {
		s.cfg.Logger.ErrorContext(r.Context(), "read discord config failed", "error", err)
		writeProblem(w, http.StatusInternalServerError, "internal_error",
			"the request could not be completed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled":            cfg.Enabled,
		"events":             cfg.Events,
		"webhook_configured": cfg.WebhookConfigured,
		"available_events":   growth.KnownEvents,
	})
}
