// Updates, version reporting and telemetry (OPS-12, OPS-13, OPS-14, ADM-12).
//
// Three things live here and they are deliberately separate:
//
//   - **The version the build reports.** It comes from `Config.Version`, which cmd/billing
//     stamps at link time (`-ldflags "-X main.version=1.2.3"`). The default stamp is "dev",
//     and the panel says so rather than inventing a number.
//
//   - **The update check.** `updates.check_url` is fetched with a bounded timeout and a
//     bounded body, and the answer is compared against the running version. The check is
//     cached in the database, because a check on every page load is a denial of service
//     against the update server and against us. `POST /updates/check` is the on-demand path
//     and is the only route that ignores the cache.
//
//   - **The approval.** `POST /updates/approval` records which version an operator approved.
//     It does not apply anything: the deployment is a plain binary that this process cannot
//     replace, and the response says so in `apply.steps` rather than offering a button that
//     downloads an executable.
//
// Telemetry is off unless `updates.telemetry` is true, and the payload is built by
// internal/updates from a fixed set of counts and versions. The preview route exists so an
// operator can read exactly what would be sent before turning it on, and `telemetry_sends`
// keeps what was sent afterwards.
//
// Every route here is staff-only and requires `update:manage`. Every mutation takes an
// Idempotency-Key through `staffMutation`, and every state change is audited inside the
// transaction that makes it.
package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/catalyst/billing/internal/audit"
	"github.com/catalyst/billing/internal/authn"
	"github.com/catalyst/billing/internal/operations"
	"github.com/catalyst/billing/internal/settings"
	"github.com/catalyst/billing/internal/updates"
	"github.com/jackc/pgx/v5"
)

func init() {
	RegisterRoutes("46-updates", registerUpdates)
}

func registerUpdates(r *router) {
	// The screen: running version, cached check, approval, telemetry state, apply plan.
	r.staffGet("/api/v1/staff/updates", authn.PermUpdateManage, r.s.handleUpdateState)

	// The on-demand check. It is a mutation because it writes the cache and because it
	// causes an outbound request; a GET that did either would be a GET with side effects and
	// would be fetched by every link preview.
	r.staffMutation(http.MethodPost, "/api/v1/staff/updates/check",
		authn.PermUpdateManage, r.s.handleUpdateCheck)

	// The approval is a decision, recorded and audited. Applying it is the deploy.
	r.staffMutation(http.MethodPost, "/api/v1/staff/updates/approval",
		authn.PermUpdateManage, r.s.handleUpdateApproval)
	r.staffMutation(http.MethodDelete, "/api/v1/staff/updates/approval",
		authn.PermUpdateManage, r.s.handleClearUpdateApproval)

	// The settings this screen owns: the check URL, the channel and the telemetry switch.
	// They are written through internal/settings so the audit row, the validation and the
	// defaults are the ones every other settings write already uses.
	r.staffMutation(http.MethodPut, "/api/v1/staff/updates/settings",
		authn.PermUpdateManage, r.s.handleUpdateSettings)

	// The telemetry preview and the delivery. The preview is a read: it builds the payload
	// and returns it without sending, which is what makes "nothing identifying" checkable by
	// an operator rather than by trust.
	r.staffGet("/api/v1/staff/updates/telemetry", authn.PermUpdateManage, r.s.handleTelemetryPreview)
	r.staffMutation(http.MethodPost, "/api/v1/staff/updates/telemetry/send",
		authn.PermUpdateManage, r.s.handleTelemetrySend)
}

// updateStore returns the updates store for this request.
//
// It is constructed per call rather than cached on Server: it is a pool handle and a clock,
// and this file deliberately adds no field to Server, which is the integrator's file.
func (s *Server) updateStore() *updates.Store { return updates.New(s.pool) }

// updateSettings reads the three settings this screen needs.
func (s *Server) updateSettings(ctx context.Context) (checkURL, channel string, telemetry bool, err error) {
	store := s.settingsStore()
	checkURL, err = store.String(ctx, "updates", "check_url")
	if err != nil {
		return "", "", false, err
	}
	channel, err = store.String(ctx, "updates", "channel")
	if err != nil {
		return "", "", false, err
	}
	telemetry, err = store.Bool(ctx, "updates", "telemetry")
	if err != nil {
		return "", "", false, err
	}
	return checkURL, channel, telemetry, nil
}

// updateError maps a domain error to a stable problem code.
//
// The check codes are stable strings from internal/updates, so a client can branch on
// `update_check_unreachable` without parsing prose. A broken update server must not make the
// screen fail: `GET /updates` reports it in `check.outcome` and `check.failure_code`, and only
// an explicit on-demand check answers non-2xx.
func (s *Server) updateError(w http.ResponseWriter, r *http.Request, err error) {
	var (
		verifyErr *updates.CheckError
		invalid   = errors.Is(err, updates.ErrInvalidVersion)
	)
	switch {
	case errors.As(err, &verifyErr):
		writeProblem(w, http.StatusBadGateway, verifyErr.Code, verifyErr.Message)
	case invalid:
		writeProblem(w, http.StatusUnprocessableEntity, "invalid_version", err.Error())
	default:
		s.cfg.Logger.ErrorContext(r.Context(), "update operation failed",
			"path", r.URL.Path, "error", err)
		writeProblem(w, http.StatusInternalServerError, "internal_error",
			"the request could not be completed")
	}
}

// handleUpdateState returns everything the updates screen renders.
func (s *Server) handleUpdateState(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "database_not_configured",
			"no database is configured")
		return
	}
	checkURL, channel, telemetry, err := s.updateSettings(r.Context())
	if err != nil {
		s.settingsError(w, r, err)
		return
	}
	state, err := s.updateStore().State(r.Context(), s.cfg.Version, channel, checkURL, telemetry, nil)
	if err != nil {
		s.updateError(w, r, err)
		return
	}
	// A state read must not be cached: it embeds a cache age and an approval, and a proxy
	// serving a stale one would tell an operator their approval was lost.
	w.Header().Set("Cache-Control", "no-store")
	// The schedule is the *job's* interval, not a number typed into the screen. The screen used to
	// say "daily" because that is what the setting's label promised; the promise is now kept by a job
	// in internal/operations, so the sentence is generated from the constant that registers it
	// (OPS-13). Stamped here rather than built in internal/updates because that package cannot import
	// operations: operations imports it.
	state.Telemetry.Schedule = &updates.TelemetrySchedule{
		Job:           operations.TelemetryJob,
		IntervalHours: int64(operations.TelemetryInterval / time.Hour),
		KillSwitch:    operations.TelemetryKillSwitch,
	}
	writeJSON(w, http.StatusOK, state)
}

// handleUpdateCheck forces a check, ignoring the cache.
func (s *Server) handleUpdateCheck(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "database_not_configured",
			"no database is configured")
		return
	}
	checkURL, channel, telemetry, err := s.updateSettings(r.Context())
	if err != nil {
		s.settingsError(w, r, err)
		return
	}
	if strings.TrimSpace(checkURL) == "" {
		writeProblem(w, http.StatusUnprocessableEntity, "update_check_not_configured",
			"set updates.check_url before checking for updates")
		return
	}

	store := s.updateStore()
	result, err := store.Check(r.Context(), s.cfg.Version, channel, checkURL, true, nil)
	if err != nil {
		s.updateError(w, r, err)
		return
	}
	if result.FailureCode != "" {
		// The failure is recorded in the cache and visible on the screen; the on-demand
		// check is the one place where asking for an answer and not getting one is an error.
		writeProblem(w, http.StatusBadGateway, result.FailureCode, result.Detail)
		return
	}

	// A successful check is worth an audit row: it is an outbound request made on the
	// installation's behalf, and "who asked the update server" is answerable.
	if err := s.pool.InTx(r.Context(), func(ctx context.Context, tx pgx.Tx) error {
		return audit.Record(ctx, tx, s.auditEvent(r, "update.check", "update", "", audit.OutcomeSuccess).
			WithChanges(map[string]any{
				"running_version": s.cfg.Version,
				"latest_version":  result.LatestVersion,
				"outcome":         string(result.Outcome),
			}))
	}); err != nil {
		s.updateError(w, r, err)
		return
	}
	_ = telemetry

	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, result)
}

// updateApprovalRequest is the approval body.
type updateApprovalRequest struct {
	// Version is the release being approved, e.g. "1.5.0".
	Version string `json:"version"`
	// Note is the operator's reason, which is what an incident review reads later.
	Note string `json:"note"`
}

// handleUpdateApproval records an approved target version.
//
// It refuses a version that is not newer than the running one, and it refuses a version that
// does not parse. Both refusals exist so the screen cannot show "approved" for something that
// means nothing: an approval of the version already running is a no-op that still looks like
// progress.
func (s *Server) handleUpdateApproval(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "database_not_configured",
			"no database is configured")
		return
	}
	var in updateApprovalRequest
	if !s.decodeJSON(w, r, &in, 8<<10) {
		return
	}
	approved, err := updates.ValidateApproval(s.cfg.Version, in.Version)
	if err != nil {
		s.updateError(w, r, err)
		return
	}

	user, _ := userFrom(r.Context())
	store := s.updateStore()
	if err := s.pool.InTx(r.Context(), func(ctx context.Context, tx pgx.Tx) error {
		if err := store.SetApproval(ctx, tx, approved.String(), in.Note, user.ID); err != nil {
			return err
		}
		return audit.Record(ctx, tx, s.auditEvent(r, "update.approve", "update", "", audit.OutcomeSuccess).
			WithChanges(map[string]any{
				"version":         approved.String(),
				"running_version": s.cfg.Version,
				"note":            in.Note,
			}))
	}); err != nil {
		s.updateError(w, r, err)
		return
	}

	checkURL, channel, telemetry, err := s.updateSettings(r.Context())
	if err != nil {
		s.settingsError(w, r, err)
		return
	}
	state, err := store.State(r.Context(), s.cfg.Version, channel, checkURL, telemetry, nil)
	if err != nil {
		s.updateError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, state)
}

// handleClearUpdateApproval withdraws the approval.
func (s *Server) handleClearUpdateApproval(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "database_not_configured",
			"no database is configured")
		return
	}
	store := s.updateStore()
	if err := s.pool.InTx(r.Context(), func(ctx context.Context, tx pgx.Tx) error {
		if err := store.ClearApproval(ctx, tx); err != nil {
			return err
		}
		return audit.Record(ctx, tx, s.auditEvent(r, "update.approval_cleared", "update", "", audit.OutcomeSuccess))
	}); err != nil {
		s.updateError(w, r, err)
		return
	}

	checkURL, channel, telemetry, err := s.updateSettings(r.Context())
	if err != nil {
		s.settingsError(w, r, err)
		return
	}
	state, err := store.State(r.Context(), s.cfg.Version, channel, checkURL, telemetry, nil)
	if err != nil {
		s.updateError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, state)
}

// updateSettingsRequest is the settings body.
//
// Every field is a pointer so "absent" and "set to the zero value" are distinguishable: a
// client that only changes the telemetry switch must not clear the check URL by omitting it.
type updateSettingsRequest struct {
	CheckURL  *string `json:"check_url"`
	Channel   *string `json:"channel"`
	Telemetry *bool   `json:"telemetry"`
	// TelemetryEndpoint is written to update_config rather than to `settings`, because it
	// commonly carries an ingest token and the settings table's read paths are shared with a
	// generic settings page.
	TelemetryEndpoint *string `json:"telemetry_endpoint"`
}

// handleUpdateSettings writes the update configuration.
//
// The three settings keys go through internal/settings, so validation, defaults, redaction
// and the audit row are the same ones every other settings write gets. The telemetry endpoint
// goes to update_config, and its audit row records the endpoint's host only — an ingest token
// in the path is a credential and must not be copied into the audit log.
func (s *Server) handleUpdateSettings(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "database_not_configured",
			"no database is configured")
		return
	}
	var in updateSettingsRequest
	if !s.decodeJSON(w, r, &in, 16<<10) {
		return
	}

	values := map[string]string{}
	if in.CheckURL != nil {
		url := strings.TrimSpace(*in.CheckURL)
		if url != "" {
			if code, message := validHTTPURL(url); code != "" {
				writeProblem(w, http.StatusUnprocessableEntity, code, message)
				return
			}
		}
		values["check_url"] = url
	}
	if in.Channel != nil {
		values["channel"] = strings.TrimSpace(*in.Channel)
	}
	if in.Telemetry != nil {
		values["telemetry"] = boolText(*in.Telemetry)
	}
	if len(values) == 0 && in.TelemetryEndpoint == nil {
		writeProblem(w, http.StatusBadRequest, "nothing_to_change",
			"the request carries no setting to write")
		return
	}

	user, _ := userFrom(r.Context())
	store := s.updateStore()

	if in.TelemetryEndpoint != nil {
		endpoint := strings.TrimSpace(*in.TelemetryEndpoint)
		if endpoint != "" {
			if code, message := validHTTPURL(endpoint); code != "" {
				writeProblem(w, http.StatusUnprocessableEntity, code, message)
				return
			}
		}
		if err := s.pool.InTx(r.Context(), func(ctx context.Context, tx pgx.Tx) error {
			if err := store.SetTelemetryEndpoint(ctx, tx, endpoint, user.ID); err != nil {
				return err
			}
			// The host, never the URL: an ingest token in the path is a credential, and the
			// audit log is read by more people than the settings page is.
			return audit.Record(ctx, tx, s.auditEvent(r, "update.telemetry_endpoint", "update", "", audit.OutcomeSuccess).
				WithChanges(map[string]any{
					"endpoint_host": updates.Config{TelemetryEndpoint: endpoint}.EndpointHost(),
					"configured":    endpoint != "",
				}))
		}); err != nil {
			s.updateError(w, r, err)
			return
		}
	}

	if len(values) > 0 {
		if _, err := s.settingsStore().Update(r.Context(), "updates", values, user.ID, s.auditorFor(r, "")); err != nil {
			s.settingsError(w, r, err)
			return
		}
	}

	checkURL, channel, telemetry, err := s.updateSettings(r.Context())
	if err != nil {
		s.settingsError(w, r, err)
		return
	}
	state, err := store.State(r.Context(), s.cfg.Version, channel, checkURL, telemetry, nil)
	if err != nil {
		s.updateError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, state)
}

// telemetryPreview is what an operator reads before turning telemetry on.
type telemetryPreview struct {
	// Enabled is `updates.telemetry`. When it is false, `payload` is null and `payload_json`
	// is empty: "empty when off" is a property of this response, not of a comment.
	Enabled bool `json:"enabled"`
	// Payload is the document that would be sent, or null when telemetry is off.
	Payload *updates.Payload `json:"payload"`
	// PayloadJSON is the exact bytes that would be sent, so an operator can read them
	// without re-serialising the object.
	PayloadJSON string `json:"payload_json"`
	// EndpointHost is where it would go. The endpoint itself is never returned.
	EndpointHost string `json:"endpoint_host"`
	// DeliveryPossible is false when no endpoint is configured, which is a state the screen
	// must show rather than discovering it when the operator clicks send.
	DeliveryPossible bool `json:"delivery_possible"`
	// Fields lists the top-level keys of the payload. It is documentation rendered from the
	// payload itself, so it cannot drift from it.
	Fields []string `json:"fields"`
	// LastSend is the most recent delivery attempt, or null.
	LastSend *updates.LastSend `json:"last_send"`
}

// handleTelemetryPreview builds the payload without sending it.
func (s *Server) handleTelemetryPreview(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "database_not_configured",
			"no database is configured")
		return
	}
	_, channel, telemetry, err := s.updateSettings(r.Context())
	if err != nil {
		s.settingsError(w, r, err)
		return
	}
	store := s.updateStore()
	cfg, err := store.Config(r.Context())
	if err != nil {
		s.updateError(w, r, err)
		return
	}
	lastSend, err := store.LastTelemetrySend(r.Context())
	if err != nil {
		s.updateError(w, r, err)
		return
	}

	out := telemetryPreview{
		Enabled:          telemetry,
		EndpointHost:     cfg.EndpointHost(),
		DeliveryPossible: cfg.TelemetryEndpoint != "",
		LastSend:         lastSend,
		Fields:           telemetryFields(),
	}
	if telemetry {
		payload, err := store.BuildPayload(r.Context(), s.cfg.Version, channel, true)
		if err != nil {
			s.updateError(w, r, err)
			return
		}
		if payload != nil {
			encoded, err := updates.EncodePayload(*payload)
			if err != nil {
				s.updateError(w, r, err)
				return
			}
			out.Payload = payload
			out.PayloadJSON = string(encoded)
		}
	}

	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, out)
}

// handleTelemetrySend delivers the payload now.
//
// Three refusals, each of which is a state the screen must be able to explain: telemetry is
// off, no endpoint is configured, and — implicitly — a payload that cannot be built.
func (s *Server) handleTelemetrySend(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "database_not_configured",
			"no database is configured")
		return
	}
	_, channel, telemetry, err := s.updateSettings(r.Context())
	if err != nil {
		s.settingsError(w, r, err)
		return
	}
	if !telemetry {
		writeProblem(w, http.StatusUnprocessableEntity, "telemetry_disabled",
			"telemetry is off; turn it on before sending")
		return
	}

	store := s.updateStore()
	cfg, err := store.Config(r.Context())
	if err != nil {
		s.updateError(w, r, err)
		return
	}
	if cfg.TelemetryEndpoint == "" {
		writeProblem(w, http.StatusUnprocessableEntity, "telemetry_endpoint_not_configured",
			"no telemetry endpoint is configured")
		return
	}

	payload, err := store.BuildPayload(r.Context(), s.cfg.Version, channel, true)
	if err != nil {
		s.updateError(w, r, err)
		return
	}
	if payload == nil {
		// Unreachable while `telemetry` is true, and kept because a nil payload reaching a
		// send would be an empty POST that still counted as a send.
		writeProblem(w, http.StatusUnprocessableEntity, "telemetry_disabled",
			"telemetry is off; turn it on before sending")
		return
	}

	user, _ := userFrom(r.Context())
	result, err := store.Send(r.Context(), cfg.TelemetryEndpoint, *payload, user.ID, nil)
	if err != nil {
		s.updateError(w, r, err)
		return
	}

	if err := s.pool.InTx(r.Context(), func(ctx context.Context, tx pgx.Tx) error {
		return audit.Record(ctx, tx, s.auditEvent(r, "update.telemetry_send", "update", result.ID, audit.OutcomeSuccess).
			WithChanges(map[string]any{
				"status":         result.Status,
				"http_status":    result.HTTPStatus,
				"payload_sha256": result.PayloadSHA,
				"endpoint_host":  result.EndpointHost,
				"failure_code":   result.FailureCode,
			}))
	}); err != nil {
		s.updateError(w, r, err)
		return
	}

	// A rejected delivery is reported as 502 with the stable code, because an operator who
	// clicked "send" and got a 200 would reasonably conclude it arrived.
	if result.Status != "sent" {
		writeProblem(w, http.StatusBadGateway, result.FailureCode,
			"the telemetry payload could not be delivered")
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// telemetryFields lists the payload's top-level keys, in the order the document declares
// them, so the screen's "what is sent" list cannot drift from the payload.
func telemetryFields() []string {
	return []string{
		"schema_version", "instance_id", "panel_version", "channel", "go_version",
		"os", "arch", "database_schema_version", "generated_at", "counts",
	}
}

// validHTTPURL checks a URL an operator typed, and returns a problem code and message when it
// is unusable.
//
// The check is deliberately shallow: a host and an http(s) scheme. Anything stricter would
// refuse a valid internal URL, and the fetch itself already bounds and validates what comes
// back.
func validHTTPURL(raw string) (code, message string) {
	lower := strings.ToLower(raw)
	if !strings.HasPrefix(lower, "http://") && !strings.HasPrefix(lower, "https://") {
		return "invalid_url", "the URL must start with http:// or https://"
	}
	rest := raw[strings.Index(raw, "://")+3:]
	if rest == "" || strings.HasPrefix(rest, "/") {
		return "invalid_url", "the URL must name a host"
	}
	return "", ""
}

// settingsGroupForUpdates is a compile-time assertion that the group this file writes to is
// one internal/settings declares. A typo here would be a 400 at runtime rather than a build
// failure, and the settings package cannot be imported for a const because the group name is
// a string.
var _ = func() bool {
	if _, ok := settings.Lookup("updates", "check_url"); !ok {
		panic("httpapi: the updates settings group is not declared in internal/settings")
	}
	return true
}()
