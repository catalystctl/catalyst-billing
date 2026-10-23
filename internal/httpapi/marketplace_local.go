// The local half of the marketplace screen: upload, enable/disable and the scaffold (ADM-12,
// OPS-14).
//
// The indexed install lives in marketplace.go and is unchanged. These three routes cover what an
// offline installation needs, and each is a separate route for a separate reason:
//
//   - `POST /marketplace/upload` takes the artifact as the request body. It is a raw `application/
//     zip` (or any octet-stream) rather than a JSON document with a base64 field: base64 costs a
//     third more bytes and, more importantly, a JSON body cannot be streamed, so an 8 MiB artifact
//     would be buffered twice.
//   - `PATCH /marketplace/installed/{name}/{version}` is the enable/disable switch. It is a PATCH
//     rather than a POST because it changes one field of an existing record, and the body says
//     which value rather than being the value.
//   - `GET /marketplace/scaffold` returns a generated zip. It is a GET because it writes nothing;
//     the query names the extension, and the operator unpacks the result.
//
// Every mutation requires `extension:manage` (the permission the rest of this screen uses), an
// idempotency key through `staffMutation`, and writes its audit row inside the transaction that
// changes the row.
//
// The scaffold is a GET that produces a file, which means it is reachable from a link. That is
// deliberate: "download the skeleton" is what an operator does with it, and a download needs a
// plain link more than it needs a fetch wrapper.
package httpapi

import (
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/catalyst/billing/internal/authn"
	"github.com/catalyst/billing/internal/marketplace"
)

func init() {
	RegisterRoutes("47-marketplace-local", registerMarketplaceLocal)
}

func registerMarketplaceLocal(r *router) {
	// The upload. The metadata travels in headers rather than the query string so it does not end
	// up in an access log: a publisher name is not a secret, but a rule that says "identifiers go
	// in headers, bytes go in the body" is easier to keep than a per-endpoint decision.
	r.staffMutation(http.MethodPost, "/api/v1/staff/marketplace/upload",
		authn.PermExtensionManage, r.s.handleMarketplaceUpload)

	// The switch. It is not idempotency-keyed in the panel's sense (setting the same value twice
	// is the same state), but it goes through staffMutation like every other mutation so it keeps
	// the CSRF check, the rate class and the audit expectations of the surface.
	r.staffMutation(http.MethodPatch, "/api/v1/staff/marketplace/installed/{name}/{version}",
		authn.PermExtensionManage, r.s.handleMarketplaceSetEnabled)

	// The scaffold. A read: nothing is stored.
	r.staffGet("/api/v1/staff/marketplace/scaffold",
		authn.PermExtensionManage, r.s.handleMarketplaceScaffold)
}

// marketplaceUploadMaxBytes bounds the upload.
//
// It is marketplace.MaxArtifactBytes plus a small allowance for the headers, so a body that is
// exactly at the documented limit is accepted and the refusal for a larger one is the domain's
// `marketplace_artifact_too_large` rather than the reader's opaque error.
const marketplaceUploadMaxBytes = marketplace.MaxArtifactBytes + 4096

// marketplaceUploadMeta is the operator's metadata, read from headers.
//
// Headers rather than a JSON envelope because the body is binary: a multipart form would add a
// parser, a boundary and its own size quirks for one file and five short strings.
type marketplaceUploadMeta struct {
	Name        string
	Version     string
	Publisher   string
	Description string
	Kind        string
	ExpectedSHA string
}

// decodeMarketplaceUpload reads the metadata headers of an upload.
func decodeMarketplaceUpload(r *http.Request) marketplaceUploadMeta {
	get := func(name string) string { return strings.TrimSpace(r.Header.Get(name)) }
	return marketplaceUploadMeta{
		Name:        get("X-Extension-Name"),
		Version:     get("X-Extension-Version"),
		Publisher:   get("X-Extension-Publisher"),
		Description: get("X-Extension-Description"),
		Kind:        get("X-Extension-Kind"),
		ExpectedSHA: get("X-Extension-Sha256"),
	}
}

// handleMarketplaceUpload stores an artifact the operator supplied.
func (s *Server) handleMarketplaceUpload(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "database_not_configured",
			"no database is configured")
		return
	}
	meta := decodeMarketplaceUpload(r)
	if meta.Name == "" || meta.Version == "" {
		writeProblem(w, http.StatusBadRequest, "marketplace_upload_invalid",
			"X-Extension-Name and X-Extension-Version are required")
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, marketplaceUploadMaxBytes))
	if err != nil {
		// The reader's error is about the limit it was given, which is the domain's limit plus the
		// header allowance; the domain's code is what a client branches on.
		writeProblem(w, http.StatusRequestEntityTooLarge, marketplace.CodeArtifactTooLarge,
			"the artifact is larger than the marketplace accepts")
		return
	}
	// An empty body is caught by the domain (artifact_empty), which is the one place that decides
	// it: a handler that also refused it would be a second answer to one question.

	user, _ := userFrom(r.Context())
	result, err := s.marketplaceStore().Upload(r.Context(), marketplace.UploadRequest{
		Name:         meta.Name,
		Version:      meta.Version,
		Publisher:    meta.Publisher,
		Description:  meta.Description,
		Kind:         meta.Kind,
		Body:         body,
		PinnedSHA256: meta.ExpectedSHA,
		ActorID:      user.ID,
		Auditor:      s.auditorFor(r, "uploaded through the marketplace screen"),
	})
	if err != nil {
		s.marketplaceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, result)
}

// enableRequest is the PATCH body.
type enableRequest struct {
	Enabled *bool `json:"enabled"`
}

// handleMarketplaceSetEnabled turns one installed extension on or off.
func (s *Server) handleMarketplaceSetEnabled(w http.ResponseWriter, r *http.Request) {
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

	var req enableRequest
	if !s.decodeJSON(w, r, &req, 4<<10) {
		return
	}
	if req.Enabled == nil {
		// No default: an omitted field that was read as "false" would switch a live gateway off
		// because a client forgot a key.
		writeProblem(w, http.StatusBadRequest, "marketplace_request_invalid",
			"`enabled` is required and must be true or false")
		return
	}

	user, _ := userFrom(r.Context())
	row, err := s.marketplaceStore().SetEnabled(r.Context(), name, version, *req.Enabled,
		user.ID, s.auditorFor(r, "changed through the marketplace screen"))
	if err != nil {
		s.marketplaceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, row)
}

// handleMarketplaceScaffold generates a starter extension.
//
// Query: name, version (default 0.1.0), kind (gateway|provisioner|other), publisher.
//
// The response is the zip itself with a content-disposition filename, because the next thing the
// operator does is unpack it. The size is small (a few KiB) and it is generated per request, so
// there is no cache header to get wrong: `no-store`, since an installation should not keep a
// skeleton it did not ask for in a proxy.
func (s *Server) handleMarketplaceScaffold(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "database_not_configured",
			"no database is configured")
		return
	}
	q := r.URL.Query()
	body, filename, err := marketplace.Scaffold(marketplace.ScaffoldRequest{
		Name:      strings.TrimSpace(q.Get("name")),
		Version:   strings.TrimSpace(q.Get("version")),
		Kind:      strings.TrimSpace(q.Get("kind")),
		Publisher: strings.TrimSpace(q.Get("publisher")),
	})
	if err != nil {
		var verifyErr *marketplace.VerifyError
		if errors.As(err, &verifyErr) {
			writeProblem(w, http.StatusUnprocessableEntity, verifyErr.Code, verifyErr.Message)
			return
		}
		writeProblem(w, http.StatusInternalServerError, "internal_error",
			"the request could not be completed")
		return
	}

	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}
