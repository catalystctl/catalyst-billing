// Imports (OPS-10, OPS-11).
//
// Three routes, one permission, and one decision about what a request looks like.
//
//   - **The permission is import:write for the reads as well as the writes.** An import log
//     says how many customers, invoices and payments came from another panel; that is not a
//     report a support agent needs, and it is not information the panel exposes to anyone who
//     can read a ticket. Whoever may run an import may read what the last one did — the two
//     are the same operational job.
//
//   - **The dump travels as a JSON envelope of five CSV documents, not as a multipart
//     upload.** A multipart parser is a second parser in the request path, and the browser
//     already has the files as text. The envelope also makes the request hashable: the
//     idempotency layer digests the body, so a retried upload replays the stored report
//     instead of running the import twice. The cost is a 1 MiB body cap (the idempotency
//     layer's), which is roughly ten thousand rows; a larger history is imported in several
//     runs, and that is safe because the legacy-id mappings persist between them — clients
//     first, then invoices, then payments, each run resuming where the last one stopped
//     without creating anything twice.
//
//   - **`commit` defaults to false.** A caller that omits it gets a dry run and a report. The
//     only way to write is to ask for it, which is the right way round for a request that can
//     create a customer database.
package httpapi

import (
	"errors"
	"net/http"
	"strings"

	"github.com/catalyst/billing/internal/authn"
	"github.com/catalyst/billing/internal/importer"
)

func init() {
	RegisterRoutes("78-imports", func(r *router) { r.s.registerImportRoutes(r) })
}

// maxImportBody bounds the request. It matches the idempotency layer's buffer so the two
// limits cannot disagree: a body the wrapper accepts is a body this handler can read.
const maxImportBody = 1 << 20

func (s *Server) registerImportRoutes(r *router) {
	// Reads: what did the last import do, and what exactly did it refuse?
	r.staffGet("/api/v1/staff/imports", authn.PermImportWrite, s.handleListImports)
	r.staffGet("/api/v1/staff/imports/{id}", authn.PermImportWrite, s.handleGetImport)

	// The run itself. It is a staff mutation with a required Idempotency-Key even when it is
	// only a dry run: the key is what lets a client retry a dropped response and get the same
	// report back rather than a second (or, worse, a first) write.
	r.staffMutation(http.MethodPost, "/api/v1/staff/imports", authn.PermImportWrite,
		s.handleRunImport)
}

// importRequest is the request body.
type importRequest struct {
	// Source selects the dump dialect: whmcs or paymenter.
	Source string `json:"source"`
	// Commit turns the dry run into a write. Omitted means false, which means a dry run.
	Commit bool `json:"commit"`
	// DefaultCurrency is used for rows that carry no currency of their own.
	DefaultCurrency string `json:"default_currency"`
	// Files maps a document name to its CSV text.
	Files map[string]string `json:"files"`
}

// handleRunImport runs a dry run or a commit and answers with the report.
func (s *Server) handleRunImport(w http.ResponseWriter, r *http.Request) {
	var req importRequest
	if !s.decodeJSON(w, r, &req, maxImportBody) {
		return
	}
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "storage_unavailable",
			"the importer needs a database")
		return
	}

	rep, err := importer.Run(r.Context(), s.pool, importer.Input{
		Source:          req.Source,
		Commit:          req.Commit,
		DefaultCurrency: req.DefaultCurrency,
		ActorID:         principalID(r),
		CorrelationID:   correlationID(r.Context()),
		IP:              s.clientIP(r),
		Auditor:         s.auditorFor(r, ""),
		Files:           req.Files,
	})
	if err != nil {
		s.importError(w, r, err)
		return
	}
	// A dry run is 200 with `dry_run: true`, not 202 or 204: the answer *is* the body, and a
	// client that sees a 2xx with no report would have to guess.
	writeJSON(w, http.StatusOK, rep)
}

// handleListImports serves the run history, newest first.
func (s *Server) handleListImports(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "storage_unavailable",
			"the importer needs a database")
		return
	}
	limit, cursor, err := ParsePage(r)
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_cursor", "the limit or cursor is not valid")
		return
	}
	list, err := importer.ListRuns(r.Context(), s.pool, limit, cursor)
	if err != nil {
		s.importError(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(list.Runs))
	for _, run := range list.Runs {
		out = append(out, importRunJSON(run))
	}
	page := Page[map[string]any]{Data: out}
	if list.NextCursor != "" {
		next := list.NextCursor
		page.NextCursor = &next
	}
	writePage(w, page)
}

// handleGetImport serves one recorded run with its full report.
func (s *Server) handleGetImport(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "storage_unavailable",
			"the importer needs a database")
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if !looksLikeUUID(id) {
		// A malformed id is the caller's mistake, and answering 404 would hide it.
		writeProblem(w, http.StatusBadRequest, "invalid_id", "the run id is not a uuid")
		return
	}
	run, err := importer.GetRun(r.Context(), s.pool, id)
	if err != nil {
		s.importError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, importRunJSON(run))
}

// importRunJSON is the one description of a recorded run.
//
// The report is included in the list as well as the detail: a run is a small document, and a
// history that required a second request per row would make "show me what the last import
// refused" a fan-out.
func importRunJSON(run importer.RunRecord) map[string]any {
	return map[string]any{
		"id":         run.ID,
		"source":     run.Source,
		"actor_id":   nullableString(run.ActorID),
		"created":    run.Created,
		"updated":    run.Updated,
		"unchanged":  run.Unchanged,
		"rejected":   run.Rejected,
		"created_at": run.CreatedAt.UTC().Format("2006-01-02T15:04:05.000Z07:00"),
		"report":     run.Report,
	}
}

// importError maps the importer's sentinels to stable problem codes.
func (s *Server) importError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, importer.ErrNotFound):
		writeProblem(w, http.StatusNotFound, "not_found", "no such import run")
	case errors.Is(err, importer.ErrUnavailable):
		writeProblem(w, http.StatusServiceUnavailable, "storage_unavailable",
			"the importer needs a database")
	case errors.Is(err, importer.ErrUnknownSource):
		writeProblem(w, http.StatusUnprocessableEntity, "unknown_source",
			"the dump source is not one this importer reads (whmcs, paymenter)")
	case errors.Is(err, importer.ErrMissingFile):
		writeProblem(w, http.StatusUnprocessableEntity, "missing_file", err.Error())
	case errors.Is(err, importer.ErrInvalidInput):
		writeProblem(w, http.StatusUnprocessableEntity, "invalid_input", err.Error())
	case errors.Is(err, importer.ErrConflict):
		// The transaction rolled back, so nothing was written. A retry is the right response,
		// which is what the code says.
		writeProblem(w, http.StatusConflict, "import_conflict",
			"another import wrote the same record at the same time; nothing was changed, retry")
	default:
		s.cfg.Logger.ErrorContext(r.Context(), "import failed",
			"path", r.URL.Path, "error", err)
		writeProblem(w, http.StatusInternalServerError, "internal_error",
			"the import could not be completed and nothing was written")
	}
}

// looksLikeUUID reports whether s has the shape of a UUID.
//
// It is a shape check, not a parse: the value is passed to a query as a parameter, so the only
// thing this has to prevent is the database raising a type error on a value that is obviously
// not an id — which would be a 500 for what is a client mistake.
func looksLikeUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
				return false
			}
		}
	}
	return true
}
