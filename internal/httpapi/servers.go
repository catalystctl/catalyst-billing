// What the configured provisioning server needs from a purchase (SVC-19, UPG-12).
//
// Two different things are called "server fields", and keeping them apart is the point of this
// file:
//
//   - the *credentials* the panel stores to launch the extension (a panel URL, an API key), which
//     live on the servers screen and are described by internal/gatewayconfig. They are
//     installation configuration: one set per installation, write-only when secret.
//   - the *inputs* an adapter needs for each purchase (which panel user owns the server, which
//     domain the account is for), which are declared by the extension through
//     `provisioner.fields` and stored per service. They are data, not configuration: a panel user
//     id differs per customer and must never be an installation setting.
//
// The core cannot hardcode the second kind. It does not know that cPanel needs a domain or that
// Pterodactyl needs a panel user; an adapter that did not need what the core hardcoded would be a
// form full of fields the provisioner ignores, which is the defect the parity inventory records as
// UPG-12 ("checkout config fields supplied by the server extension").
//
// So the extension declares, the panel stores, and the worker sends it back under the same key in
// `metadata`. One key end to end, no second mapping to drift.

package httpapi

import (
	"errors"
	"net/http"

	"github.com/catalyst/billing/internal/authn"
	"github.com/catalyst/billing/internal/gatewayconfig"
	"github.com/catalyst/billing/internal/provisioning"
)

func init() {
	RegisterRoutes("71-server-fields", registerServerFields)
}

func registerServerFields(r *router) {
	// It launches the configured extension, so it costs a process start and is budgeted like the
	// connection test rather than like a read.
	r.add(Route{
		Method: http.MethodGet, Pattern: "/api/v1/staff/servers/fields",
		Access: AccessPermission, Permission: authn.PermServerWrite, StaffOnly: true,
		Rate: RateMutation, Handler: r.s.handleServerFields,
	})
}

// handleServerFields reports the inputs the configured provisioner declares.
//
// It is a read: no settings row, no audit row, no queue row, and safe to press twice. A failure is
// reported inline with a stable code (like the connection test) rather than as a request error,
// because "the panel cannot answer right now" is information the screen must render next to the
// form rather than instead of it.
func (s *Server) handleServerFields(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "database_not_configured",
			"no database is configured")
		return
	}
	cfg, err := gatewayconfig.New(s.pool).Load(r.Context(), gatewayconfig.ServerKind)
	if err != nil {
		s.integrationError(w, r, err)
		return
	}
	out := map[string]any{
		"provider":      cfg.Provider,
		"provider_name": cfg.ProviderName,
		"configured":    cfg.Configured(),
		"supported":     false,
		"fields":        []map[string]any{},
		"notes":         nonNilStrings(cfg.Notes),
	}
	if !cfg.Configured() {
		// Not a failure: an installation that has not chosen a provisioner has no fields to
		// declare, and rendering a prompt is more useful than a 4xx.
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, out)
		return
	}

	fields, err := provisioning.ReadProvisionerFields(r.Context(), s.pool, integrationTestTimeout)
	if err != nil {
		if errors.Is(err, provisioning.ErrNotConfigured) {
			w.Header().Set("Cache-Control", "no-store")
			writeJSON(w, http.StatusOK, out)
			return
		}
		code, detail := testFailure(err)
		out["ok"] = false
		out["code"] = code
		out["detail"] = detail
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, out)
		return
	}
	out["ok"] = true
	out["extension"] = fields.Extension
	out["supported"] = fields.Supported
	out["capabilities"] = nonNilStrings(fields.Capabilities)
	out["fields"] = provisioningFieldJSON(fields.Fields)
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, out)
}
