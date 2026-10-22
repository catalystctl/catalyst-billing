// The extension marketplace (ADM-23).
//
// An operator points the panel at a JSON index, sees what is published, and installs one.
// The install is the part that has to be right:
//
//   - the artifact is downloaded with a bounded size, and the size is enforced while reading
//     rather than from Content-Length, which is the server's claim;
//   - the SHA-256 is compared with the index *before* anything is written to a place anything
//     would execute and before a row is written claiming the extension is installed;
//   - a mismatch refuses with `marketplace_digest_mismatch` and leaves nothing behind;
//   - the verified bytes are stored and can be downloaded back, so the artifact that was
//     checked is the artifact that gets deployed;
//   - an operator can pin the digest out of band, and the install then requires both the
//     index digest and the downloaded bytes to match the pin.
//
// What the panel does not do is start the extension. The core loads extensions as separate
// processes from paths in its own environment (`BILLING_GATEWAY_EXTENSION`,
// `BILLING_PROVISIONER_EXTENSION`), and there is no runtime registration. So the API reports
// `status: "verified"` and an `activation` instruction naming the environment variable the
// deployment actually reads — never `status: "running"`, which would be the fake progress
// this feature must not show.
//
// # The trust statement
//
// The index publishes both the digest and the download URL, so a compromised index can
// replace both. An unpinned install therefore proves the transfer was not corrupted, not that
// the publisher is who the index says. Every install response carries `trust` saying exactly
// which case it was, and the screen shows it: an operator who assumes a green tick means
// "signed" would be wrong, and it is the panel's job to say so.
//
// Every route is staff-only and requires `extension:manage`.
package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/catalyst/billing/internal/audit"
	"github.com/catalyst/billing/internal/authn"
	"github.com/catalyst/billing/internal/marketplace"
	"github.com/jackc/pgx/v5"
)

func init() {
	RegisterRoutes("47-marketplace", registerMarketplace)
}

func registerMarketplace(r *router) {
	// The screen: configuration, the cached index and what is installed.
	r.staffGet("/api/v1/staff/marketplace", authn.PermExtensionManage, r.s.handleMarketplaceState)

	// The configuration. It is a mutation because it changes where the panel fetches from,
	// which is an outbound-request decision.
	r.staffMutation(http.MethodPut, "/api/v1/staff/marketplace/settings",
		authn.PermExtensionManage, r.s.handleMarketplaceSettings)

	// The on-demand index fetch, which is the only route that ignores the cache.
	r.staffMutation(http.MethodPost, "/api/v1/staff/marketplace/refresh",
		authn.PermExtensionManage, r.s.handleMarketplaceRefresh)

	// The install. It downloads, verifies and records; it never executes.
	r.staffMutation(http.MethodPost, "/api/v1/staff/marketplace/install",
		authn.PermExtensionManage, r.s.handleMarketplaceInstall)

	// The verified artifact, so the operator can deploy exactly the bytes that were checked.
	// It is an export by nature: it returns a binary body.
	r.staffExport("/api/v1/staff/marketplace/installed/{name}/{version}/artifact",
		authn.PermExtensionManage, r.s.handleMarketplaceArtifact)
}

// marketplaceStore returns the marketplace store for this request.
func (s *Server) marketplaceStore() *marketplace.Store { return marketplace.New(s.pool) }

// marketplaceError maps a domain error to a stable problem code.
//
// The codes come from internal/marketplace, so a client branches on
// `marketplace_digest_mismatch` rather than on prose. A digest mismatch is a 422: the request
// was well formed and the server understood it, and the artifact is the thing that was wrong.
func (s *Server) marketplaceError(w http.ResponseWriter, r *http.Request, err error) {
	var (
		verifyErr *marketplace.VerifyError
		indexErr  *marketplace.IndexError
	)
	switch {
	case errors.As(err, &verifyErr):
		writeProblem(w, statusForVerifyCode(verifyErr.Code), verifyErr.Code, verifyErr.Message)
	case errors.As(err, &indexErr):
		writeProblem(w, http.StatusBadGateway, indexErr.Code, indexErr.Message)
	default:
		s.cfg.Logger.ErrorContext(r.Context(), "marketplace operation failed",
			"path", r.URL.Path, "error", err)
		writeProblem(w, http.StatusInternalServerError, "internal_error",
			"the request could not be completed")
	}
}

// statusForVerifyCode picks the HTTP status for a refusal.
//
// A digest mismatch and a version conflict are 422: the request was understood and the
// artifact or the index entry is what the panel refuses. A missing entry is 404. Everything
// else is a 502, because the failure was upstream.
func statusForVerifyCode(code string) int {
	switch code {
	case marketplace.CodeExtensionUnknown:
		return http.StatusNotFound
	case marketplace.CodeDigestMismatch, marketplace.CodeDigestInvalid,
		marketplace.CodeVersionConflict, marketplace.CodeArtifactEmpty:
		return http.StatusUnprocessableEntity
	case marketplace.CodeArtifactTooLarge:
		return http.StatusRequestEntityTooLarge
	case marketplace.CodeNotConfigured:
		return http.StatusUnprocessableEntity
	default:
		return http.StatusBadGateway
	}
}

// marketplaceState is the screen's whole payload.
type marketplaceState struct {
	// Configured is false when no index URL is set. The screen renders that as a state, not
	// as an empty grid: "the marketplace is not configured" and "nothing is published" are
	// different facts.
	Configured bool `json:"configured"`
	// IndexURL is the configured listing URL.
	IndexURL string `json:"index_url"`
	// InstallDir is where a verified artifact is staged; empty means "stored in the panel
	// only", which is reported rather than hidden.
	InstallDir string `json:"install_dir"`
	// FetchedAt is when the cached listing was fetched; nil when it never has been.
	FetchedAt *string `json:"fetched_at"`
	// Cached says the listing came from the cache rather than a fetch in this request.
	Cached bool `json:"cached"`
	// Index is the listing. Empty when it could not be read.
	Index marketplace.Index `json:"index"`
	// FailureCode is set when the last fetch failed, so a broken index is visible instead of
	// looking like an empty marketplace.
	FailureCode string `json:"failure_code"`
	// Installed is what has been verified and stored.
	Installed []marketplace.Installed `json:"installed"`
	// Trust explains what a digest check proves, so the screen can state it once rather than
	// leaving an operator to assume the tick means "signed".
	Trust string `json:"trust"`
}

// marketplaceTrust is the standing explanation shown on the screen.
const marketplaceTrust = "A digest from the index proves the download was not corrupted or swapped in transit. " +
	"It does not prove the index is trustworthy: whoever controls the index controls the digest too. " +
	"Pin the digest from the release announcement when you need more than that, and the install will then " +
	"require the index and the artifact to match your pin."

// handleMarketplaceState returns the marketplace screen's payload.
func (s *Server) handleMarketplaceState(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "database_not_configured",
			"no database is configured")
		return
	}
	store := s.marketplaceStore()
	cfg, err := store.Config(r.Context())
	if err != nil {
		s.marketplaceError(w, r, err)
		return
	}
	installed, err := store.ListInstalled(r.Context())
	if err != nil {
		s.marketplaceError(w, r, err)
		return
	}

	out := marketplaceState{
		Configured: cfg.IndexURL != "",
		IndexURL:   cfg.IndexURL,
		InstallDir: cfg.InstallDir,
		Index:      marketplace.Index{SchemaVersion: marketplace.IndexSchemaVersion, Extensions: []marketplace.Extension{}},
		Installed:  installed,
		Trust:      marketplaceTrust,
	}

	if cfg.IndexURL != "" {
		loaded, err := store.LoadIndex(r.Context(), cfg.IndexURL, false, nil)
		if err != nil {
			s.marketplaceError(w, r, err)
			return
		}
		fetched := loaded.FetchedAt.UTC().Format("2006-01-02T15:04:05Z07:00")
		out.FetchedAt = &fetched
		out.Cached = loaded.Cached
		out.FailureCode = loaded.FailureCode
		if loaded.FailureCode == "" {
			out.Index = loaded.Index
		}
	}

	// No-store: the payload embeds a cache age and the installed set, and a proxy serving a
	// stale copy would tell an operator their install vanished.
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, out)
}

// marketplaceSettingsRequest is the configuration body. Pointers distinguish "absent" from
// "cleared".
type marketplaceSettingsRequest struct {
	IndexURL   *string `json:"index_url"`
	InstallDir *string `json:"install_dir"`
}

// handleMarketplaceSettings writes the marketplace configuration.
func (s *Server) handleMarketplaceSettings(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "database_not_configured",
			"no database is configured")
		return
	}
	var in marketplaceSettingsRequest
	if !s.decodeJSON(w, r, &in, 16<<10) {
		return
	}
	if in.IndexURL == nil && in.InstallDir == nil {
		writeProblem(w, http.StatusBadRequest, "nothing_to_change",
			"the request carries no setting to write")
		return
	}

	store := s.marketplaceStore()
	cfg, err := store.Config(r.Context())
	if err != nil {
		s.marketplaceError(w, r, err)
		return
	}
	indexURL := cfg.IndexURL
	if in.IndexURL != nil {
		indexURL = strings.TrimSpace(*in.IndexURL)
		if indexURL != "" {
			if code, message := validHTTPURL(indexURL); code != "" {
				writeProblem(w, http.StatusUnprocessableEntity, "invalid_url", message)
				return
			}
		}
	}
	installDir := cfg.InstallDir
	if in.InstallDir != nil {
		installDir = strings.TrimSpace(*in.InstallDir)
	}

	user, _ := userFrom(r.Context())
	if err := s.pool.InTx(r.Context(), func(ctx context.Context, tx pgx.Tx) error {
		if err := store.SetConfig(ctx, tx, indexURL, installDir, user.ID); err != nil {
			return err
		}
		return audit.Record(ctx, tx, s.auditEvent(r, "marketplace.configure", "marketplace", "", audit.OutcomeSuccess).
			WithChanges(map[string]any{
				"index_url":   indexURL,
				"install_dir": installDir,
			}))
	}); err != nil {
		s.marketplaceError(w, r, err)
		return
	}

	s.handleMarketplaceState(w, r)
}

// handleMarketplaceRefresh forces an index fetch.
func (s *Server) handleMarketplaceRefresh(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "database_not_configured",
			"no database is configured")
		return
	}
	store := s.marketplaceStore()
	cfg, err := store.Config(r.Context())
	if err != nil {
		s.marketplaceError(w, r, err)
		return
	}
	if cfg.IndexURL == "" {
		writeProblem(w, http.StatusUnprocessableEntity, marketplace.CodeNotConfigured,
			"set the marketplace index URL before refreshing")
		return
	}

	loaded, err := store.LoadIndex(r.Context(), cfg.IndexURL, true, nil)
	if err != nil {
		s.marketplaceError(w, r, err)
		return
	}
	if loaded.FailureCode != "" {
		writeProblem(w, http.StatusBadGateway, loaded.FailureCode,
			"the marketplace index could not be read")
		return
	}

	if err := s.pool.InTx(r.Context(), func(ctx context.Context, tx pgx.Tx) error {
		return audit.Record(ctx, tx, s.auditEvent(r, "marketplace.refresh", "marketplace", "", audit.OutcomeSuccess).
			WithChanges(map[string]any{
				"index_url":  cfg.IndexURL,
				"extensions": len(loaded.Index.Extensions),
			}))
	}); err != nil {
		s.marketplaceError(w, r, err)
		return
	}

	s.handleMarketplaceState(w, r)
}

// marketplaceInstallRequest is the install body.
type marketplaceInstallRequest struct {
	Name string `json:"name"`
	// Version selects a specific version; empty means the highest listed.
	Version string `json:"version"`
	// PinnedSHA256 is the digest the operator supplied out of band. When set, both the index
	// entry and the downloaded bytes must match it.
	PinnedSHA256 string `json:"pinned_sha256"`
}

// handleMarketplaceInstall downloads, verifies and records one extension.
func (s *Server) handleMarketplaceInstall(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "database_not_configured",
			"no database is configured")
		return
	}
	var in marketplaceInstallRequest
	if !s.decodeJSON(w, r, &in, 8<<10) {
		return
	}

	store := s.marketplaceStore()
	cfg, err := store.Config(r.Context())
	if err != nil {
		s.marketplaceError(w, r, err)
		return
	}
	if cfg.IndexURL == "" {
		writeProblem(w, http.StatusUnprocessableEntity, marketplace.CodeNotConfigured,
			"set the marketplace index URL before installing")
		return
	}
	loaded, err := store.LoadIndex(r.Context(), cfg.IndexURL, false, nil)
	if err != nil {
		s.marketplaceError(w, r, err)
		return
	}
	if loaded.FailureCode != "" {
		writeProblem(w, http.StatusBadGateway, loaded.FailureCode,
			"the marketplace index could not be read")
		return
	}

	user, _ := userFrom(r.Context())
	result, err := store.Install(r.Context(), loaded.Index, marketplace.InstallRequest{
		Name:         in.Name,
		Version:      in.Version,
		PinnedSHA256: in.PinnedSHA256,
		ActorID:      user.ID,
	}, nil)
	if err != nil {
		// A refused install is audited as a failure, with the code, so "someone tried to
		// install an artifact whose digest did not match" is answerable later. The audit row
		// is written even though nothing was installed, because the attempt is the event.
		s.auditInstallRefusal(r, in, err)
		s.marketplaceError(w, r, err)
		return
	}

	if err := s.pool.InTx(r.Context(), func(ctx context.Context, tx pgx.Tx) error {
		// The target id is empty: audit_events.target_id is a uuid, and an extension is
		// identified by its name and version, which are in the diff. Writing a name into a
		// uuid column would be a 500, and writing a synthetic uuid would be a lie.
		return audit.Record(ctx, tx, s.auditEvent(r, "marketplace.install", "extension", "", audit.OutcomeSuccess).
			WithChanges(map[string]any{
				"name":       result.Name,
				"version":    result.Version,
				"sha256":     result.SHA256,
				"pinned":     result.Pinned,
				"size_bytes": result.SizeBytes,
				"staged":     result.StagedPath != "",
			}))
	}); err != nil {
		s.marketplaceError(w, r, err)
		return
	}

	writeJSON(w, http.StatusCreated, result)
}

// auditInstallRefusal records a refused install.
//
// It runs on its own transaction because the install's transaction rolled back (or never
// began). A failure to record is logged rather than returned: the caller already has the
// refusal, and turning it into a 500 would hide the real reason.
func (s *Server) auditInstallRefusal(r *http.Request, in marketplaceInstallRequest, cause error) {
	code := "marketplace_install_refused"
	var verifyErr *marketplace.VerifyError
	if errors.As(cause, &verifyErr) {
		code = verifyErr.Code
	}
	if err := s.pool.InTx(r.Context(), func(ctx context.Context, tx pgx.Tx) error {
		return audit.Record(ctx, tx, s.auditEvent(r, "marketplace.install", "extension", "", audit.OutcomeFailure).
			WithChanges(map[string]any{
				"name":          in.Name,
				"version":       in.Version,
				"pinned_sha256": in.PinnedSHA256,
				"refused_code":  code,
			}))
	}); err != nil {
		s.cfg.Logger.ErrorContext(r.Context(), "could not record a refused extension install",
			"name", in.Name, "error", err)
	}
}

// handleMarketplaceArtifact returns the verified bytes.
//
// The digest is returned in a header so a client can check what it received against what was
// verified, and the body is served with a content type that cannot be rendered: an extension
// artifact is a binary, and a browser must download it rather than display it.
func (s *Server) handleMarketplaceArtifact(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "database_not_configured",
			"no database is configured")
		return
	}
	name := strings.TrimSpace(r.PathValue("name"))
	version := strings.TrimSpace(r.PathValue("version"))
	if name == "" || version == "" {
		writeProblem(w, http.StatusNotFound, "not_found", "no such installed extension")
		return
	}

	body, digest, err := s.marketplaceStore().Artifact(r.Context(), name, version)
	if err != nil {
		s.marketplaceError(w, r, err)
		return
	}
	if body == nil {
		writeProblem(w, http.StatusNotFound, "not_found", "no such installed extension")
		return
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition",
		`attachment; filename="`+name+"-"+version+`"`)
	w.Header().Set("X-Content-SHA256", digest)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}
