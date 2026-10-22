// Gateway and provisioning-server configuration in the panel (E4.2 ADM-10, E4.5 ADM-11).
//
// Two screens, one shape: pick a provider, enter its credentials, switch it on, and test the
// connection. The test launches the configured extension exactly the way the worker and the
// server do — same binary, same environment — performs the protocol handshake and calls
// `gateway.describe` / `provisioner.describe`, then reports the provider's name, its
// currencies (or plans) and its methods.
//
// Three rules shape this file, and each exists because the alternative is a defect:
//
//  1. **A secret is write-only.** The read path reports `is_set` and `source`, never the
//     value. The redaction is a property of internal/gatewayconfig, not a discipline this
//     handler has to remember: a secret's text is not in an exported field, so marshalling
//     the configuration cannot leak it even if this file is edited badly.
//
//  2. **A write says what it could not do.** When the environment supplies a value, the
//     screen marks the field read-only and this handler refuses the write with
//     `env_override`. Silently storing a value the runtime ignores would be a screen that
//     lies.
//
//  3. **The test is a read.** It is a `GET`, it takes no idempotency key, it writes no
//     settings row, no audit row and no idempotency record, and it is safe to press twice.
//     A "test" that could change configuration is a button nobody dares press. A failed test
//     is a successful request whose answer is "no": 200 with `ok: false` and a stable code,
//     so the screen can render the reason inline instead of as a request error.
//
// The webhook URL is derived from BILLING_PUBLIC_URL rather than from the request's Host
// header: the address a provider must deliver to is the deployment's public origin, and a
// request can arrive through an internal name the provider cannot resolve.

package httpapi

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/catalyst/billing/internal/authn"
	"github.com/catalyst/billing/internal/events"
	"github.com/catalyst/billing/internal/extproto"
	"github.com/catalyst/billing/internal/gatewayconfig"
	"github.com/catalyst/billing/internal/settings"
)

func init() {
	RegisterRoutes("58-integrations", registerIntegrationRoutes)
}

// integrationTestTimeout bounds a connection test. It is generous for a launch plus one
// describe call, and short enough that a wedged extension cannot hold the request open: the
// operator gets an answer either way.
const integrationTestTimeout = 25 * time.Second

func registerIntegrationRoutes(r *router) {
	// Reading is gated with writing, like the settings page: a gateway's configuration is
	// the credentials' shape and the deployment's public webhook URL, which is reconnaissance
	// rather than support information.
	r.staffGet("/api/v1/staff/gateways", authn.PermGatewayWrite, r.s.handleGetGatewayConfig)
	r.staffMutation(http.MethodPut, "/api/v1/staff/gateways", authn.PermGatewayWrite,
		r.s.handlePutGatewayConfig)
	r.staffGet("/api/v1/staff/servers", authn.PermServerWrite, r.s.handleGetServerConfig)
	r.staffMutation(http.MethodPut, "/api/v1/staff/servers", authn.PermServerWrite,
		r.s.handlePutServerConfig)

	// The connection tests. They are GETs because they change nothing: no CSRF token, no
	// idempotency key, and no idempotency record — which is the difference between "this
	// cannot change anything" and "this cannot change anything except its own bookkeeping".
	// The rate class is the mutation bucket because each press starts a process, and a
	// read-sized budget would let a page loop launch a hundred of them.
	r.add(Route{
		Method: http.MethodGet, Pattern: "/api/v1/staff/gateways/test",
		Access: AccessPermission, Permission: authn.PermGatewayWrite, StaffOnly: true,
		Rate: RateMutation, Handler: r.s.handleTestGateway,
	})
	r.add(Route{
		Method: http.MethodGet, Pattern: "/api/v1/staff/servers/test",
		Access: AccessPermission, Permission: authn.PermServerWrite, StaffOnly: true,
		Rate: RateMutation, Handler: r.s.handleTestServer,
	})
}

// integrationResolver returns the resolver for this server.
//
// It is constructed per call rather than cached on Server: it is a pool handle and two
// function seams, and this package deliberately avoids adding shared mutable state to Server
// for something this cheap. It also means a test can prove the precedence by setting the
// environment, which is the same seam a container deployment uses.
func (s *Server) integrationResolver() *gatewayconfig.Resolver {
	return gatewayconfig.New(s.pool)
}

// integrationError maps a resolution or write error to a problem document.
func (s *Server) integrationError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, gatewayconfig.ErrUnknownProvider):
		writeProblem(w, http.StatusUnprocessableEntity, "unknown_provider",
			"that provider is not one this build can launch")
	case errors.Is(err, gatewayconfig.ErrEnvOverride):
		// The message names the environment variable, never a stored value, so it is safe to
		// return and it is the one fact the operator needs.
		writeProblem(w, http.StatusConflict, "env_override", err.Error())
	case errors.Is(err, gatewayconfig.ErrNotConfigured):
		writeProblem(w, http.StatusUnprocessableEntity, "provider_required", err.Error())
	case errors.Is(err, settings.ErrUnknownKey):
		writeProblem(w, http.StatusBadRequest, "unknown_setting", err.Error())
	case errors.Is(err, settings.ErrInvalidValue):
		writeProblem(w, http.StatusUnprocessableEntity, "invalid_value", err.Error())
	default:
		s.cfg.Logger.ErrorContext(r.Context(), "integration configuration failed",
			"path", r.URL.Path, "error", err)
		writeProblem(w, http.StatusInternalServerError, "internal_error",
			"the request could not be completed")
	}
}

// ------------------------------------------------------------------- reads --

// handleGetGatewayConfig returns the gateway's configuration as it currently reads.
func (s *Server) handleGetGatewayConfig(w http.ResponseWriter, r *http.Request) {
	s.readIntegration(w, r, gatewayconfig.GatewayKind)
}

// handleGetServerConfig returns the provisioner's configuration as it currently reads.
func (s *Server) handleGetServerConfig(w http.ResponseWriter, r *http.Request) {
	s.readIntegration(w, r, gatewayconfig.ServerKind)
}

// readIntegration is the shared read path for both screens.
func (s *Server) readIntegration(w http.ResponseWriter, r *http.Request, kind gatewayconfig.Kind) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "database_not_configured",
			"no database is configured")
		return
	}
	cfg, err := s.integrationResolver().Load(r.Context(), kind)
	if err != nil {
		s.integrationError(w, r, err)
		return
	}
	// A configuration read must never be served from a cache: it carries the deployment's
	// current precedence decision, and a proxy that cached it would show one operator another
	// operator's last save.
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, cfg)
}

// --------------------------------------------------------------- writes --

// integrationPutBody is the write shape, shared by both screens.
//
// Credentials travel under `values` rather than as named top-level fields, for the same
// reason the settings page writes a map: a provider's field set is declared in
// internal/gatewayconfig, and a handler with one JSON field per credential would need
// editing (and a migration of every client) each time a provider gains one.
type integrationPutBody struct {
	// Provider selects the extension. Absent leaves the stored provider unchanged.
	Provider *string `json:"provider"`
	// Enabled is the gateway switch. It has no meaning for a provisioner, which is on
	// whenever a provider is stored, and sending it there is an unknown setting.
	Enabled *bool `json:"enabled"`
	// OffSessionEnabled lets a renewal charge a saved method with no customer present.
	OffSessionEnabled *bool `json:"off_session_enabled"`
	// Values is key -> text. A secret sent as "[redacted]" is skipped, so loading the page
	// and saving it unchanged cannot overwrite a credential with the marker.
	Values map[string]string `json:"values"`
}

// handlePutGatewayConfig writes the gateway's configuration.
func (s *Server) handlePutGatewayConfig(w http.ResponseWriter, r *http.Request) {
	s.writeIntegration(w, r, gatewayconfig.GatewayKind)
}

// handlePutServerConfig writes the provisioner's configuration.
func (s *Server) handlePutServerConfig(w http.ResponseWriter, r *http.Request) {
	s.writeIntegration(w, r, gatewayconfig.ServerKind)
}

// writeIntegration is the shared write path for both screens.
func (s *Server) writeIntegration(w http.ResponseWriter, r *http.Request, kind gatewayconfig.Kind) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "database_not_configured",
			"no database is configured")
		return
	}

	var body integrationPutBody
	if !s.decodeJSON(w, r, &body, 64<<10) {
		return
	}
	values := make(map[string]string, len(body.Values)+3)
	for key, value := range body.Values {
		values[key] = value
	}
	if body.Provider != nil {
		values["provider"] = strings.TrimSpace(*body.Provider)
	}
	if body.Enabled != nil {
		values["enabled"] = boolText(*body.Enabled)
	}
	if body.OffSessionEnabled != nil {
		values["off_session_enabled"] = boolText(*body.OffSessionEnabled)
	}
	if len(values) == 0 {
		writeProblem(w, http.StatusBadRequest, "nothing_to_update",
			"the request carries no configuration to write")
		return
	}

	user, _ := userFrom(r.Context())
	// The audit row is written by the settings store inside the same transaction that stores
	// the value, relabelled by the resolver to gateway.config.update / server.config.update.
	// So there is nothing to record here, and a rolled-back save cannot leave a row claiming
	// otherwise.
	cfg, err := s.integrationResolver().Save(r.Context(), kind, values, user.ID, s.auditorFor(r, ""))
	if err != nil {
		s.integrationError(w, r, err)
		return
	}

	kindName := "gateway"
	if kind == gatewayconfig.ServerKind {
		kindName = "server"
	}
	permission := string(authn.PermGatewayWrite)
	if kind == gatewayconfig.ServerKind {
		permission = string(authn.PermServerWrite)
	}
	s.publish(events.Event{
		Kind:       kindName + ".configured",
		Permission: permission,
		Data:       map[string]any{"provider": cfg.Provider, "enabled": cfg.Enabled},
	})

	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, cfg)
}

// boolText renders a switch the way the settings store reads it.
func boolText(v bool) string {
	if v {
		return "true"
	}
	return "false"
}

// ---------------------------------------------------------------- testing --

// integrationTestResult is the answer to a connection test.
//
// A failed test is not a failed request: the request succeeded and the answer is "no". So
// the status is always 200 and the failure travels in `ok`/`code`/`detail`, which is what
// lets the screen show the reason next to the button that caused it.
type integrationTestResult struct {
	OK bool `json:"ok"`
	// Code is a stable identifier for the failure, empty on success.
	Code string `json:"code,omitempty"`
	// Detail is one sentence for the operator.
	Detail string `json:"detail,omitempty"`
	// Gateway and Server are the description of whichever surface was tested; exactly one is
	// present on success.
	Gateway *gatewayconfig.GatewayDescription `json:"gateway,omitempty"`
	Server  *gatewayconfig.ServerDescription  `json:"server,omitempty"`
	// Notes repeat the configuration's own remarks, so a failure explains itself without a
	// second request.
	Notes []string `json:"notes,omitempty"`
	// TestedAt is when the test finished.
	TestedAt time.Time `json:"tested_at"`
}

// handleTestGateway launches the configured gateway and reports what it says.
func (s *Server) handleTestGateway(w http.ResponseWriter, r *http.Request) {
	s.testIntegration(w, r, gatewayconfig.GatewayKind)
}

// handleTestServer launches the configured provisioner and reports what it says.
func (s *Server) handleTestServer(w http.ResponseWriter, r *http.Request) {
	s.testIntegration(w, r, gatewayconfig.ServerKind)
}

// testIntegration is the shared test path.
//
// It tests the *saved* configuration, not the form on screen. That is a deliberate limit:
// the alternative is accepting credentials in a GET, which would put a secret in a URL, in
// the access log and in the browser history. The screen says so and offers "save and test".
func (s *Server) testIntegration(w http.ResponseWriter, r *http.Request, kind gatewayconfig.Kind) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "database_not_configured",
			"no database is configured")
		return
	}
	resolver := s.integrationResolver()
	cfg, err := resolver.Load(r.Context(), kind)
	if err != nil {
		s.integrationError(w, r, err)
		return
	}

	result := integrationTestResult{Notes: cfg.Notes, TestedAt: time.Now().UTC()}
	fail := func(code, detail string) {
		result.OK = false
		result.Code = code
		result.Detail = detail
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, result)
	}

	if !cfg.Configured() {
		fail("not_configured", "No provider is configured yet. Choose one and save its credentials first.")
		return
	}
	if cfg.ExtensionPath == "" {
		fail("extension_missing", "No extension binary is configured for this provider.")
		return
	}
	if _, statErr := os.Stat(cfg.ExtensionPath); statErr != nil {
		fail("extension_missing", "The extension binary "+cfg.ExtensionPath+" was not found.")
		return
	}

	// The test gets its own context: the request's context is cancelled when the client goes
	// away, and a half-killed child process is a worse diagnostic than a timeout.
	ctx, cancel := context.WithTimeout(r.Context(), integrationTestTimeout)
	defer cancel()

	if kind == gatewayconfig.ServerKind {
		desc, err := resolver.TestServer(ctx, cfg, integrationTestTimeout)
		if err != nil {
			code, detail := testFailure(err)
			fail(code, detail)
			return
		}
		result.OK = true
		result.Server = &desc
	} else {
		desc, err := resolver.TestGateway(ctx, cfg, integrationTestTimeout)
		if err != nil {
			code, detail := testFailure(err)
			fail(code, detail)
			return
		}
		result.OK = true
		result.Gateway = &desc
	}

	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, result)
}

// testFailure turns an extension failure into a stable code and a sentence.
//
// The codes are the four things that actually go wrong, and they are distinct because the
// remedy is different for each: a missing binary is a deployment problem, a handshake
// failure is a version mismatch, a protocol violation is a broken adapter, and a remote
// error is the provider refusing the credentials.
func testFailure(err error) (string, string) {
	switch {
	case errors.Is(err, gatewayconfig.ErrNotConfigured):
		return "not_configured", "No provider is configured yet."
	case errors.Is(err, extproto.ErrCrashed):
		return "extension_crashed",
			"The extension could not be started, or it exited before answering. Check the binary path and its configuration."
	case errors.Is(err, extproto.ErrVersionMismatch):
		return "protocol_version_mismatch",
			"The extension does not speak a protocol version this panel supports."
	case errors.Is(err, extproto.ErrProtocol):
		return "protocol_error",
			"The extension answered in a way the protocol does not allow. It is not a compatible adapter."
	case errors.Is(err, extproto.ErrTimeout), errors.Is(err, context.DeadlineExceeded):
		return "timeout",
			"The extension did not answer in time. The provider may be unreachable, or the credentials may be wrong."
	case errors.Is(err, extproto.ErrRemote), errors.Is(err, extproto.ErrRejected):
		return "provider_refused", err.Error()
	default:
		return "test_failed", err.Error()
	}
}
