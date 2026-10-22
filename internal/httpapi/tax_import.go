// Staff tax-rate import: one configured source, a preview, and the import itself.
//
// The rate table's CRUD lives in commerce_tax.go and is untouched by this file. What is here is
// the *origin* of a rate: the source an installation fetches from, what that document would
// change, and the import that applies it.
//
// Two shape decisions are worth stating because they are visible in the responses:
//
//   - **The preview is a POST.** It is a read of the panel's own state, but it performs a fetch
//     against a remote document, and it takes a parameter (whether to overwrite a manual rate)
//     that changes what it reports. A GET with a body would be worse. It is registered as a staff
//     mutation — with a required Idempotency-Key — for the reason the importer's dry run is: a
//     retried request replays the same report instead of fetching again, which is what a client
//     retrying a dropped response wants.
//
//   - **The import returns the run and the report.** Not a bare count: the operator who pressed
//     Import needs to see, row by row, what was created, what was replaced, what was left alone
//     because a human typed it, and what was refused and why. The run id is the handle to the same
//     report later.
//
// Both the preview and the import re-read the source. The import does not apply a preview a client
// sent back — that would be a client telling the server what to write — and the report it returns
// is what was actually applied.

package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/catalyst/billing/internal/audit"
	"github.com/catalyst/billing/internal/authn"
	"github.com/catalyst/billing/internal/tax"
	"github.com/jackc/pgx/v5"
)

// registerTaxImportRoutes wires the source, the preview, the import and its history.
//
// The path prefix is `tax-import` rather than `tax-rates/...` because `tax-rates/{id}` already
// matches a single segment, and a literal that happens to collide with an id is a route whose
// meaning depends on the router's precedence rules. A separate prefix cannot be misread.
func registerTaxImportRoutes(r *router, s *Server) {
	r.staffGet("/api/v1/staff/tax-import/source", authn.PermTaxWrite, s.handleTaxSource)
	r.staffMutation(http.MethodPut, "/api/v1/staff/tax-import/source", authn.PermTaxWrite,
		s.handleSaveTaxSource)
	r.staffMutation(http.MethodPost, "/api/v1/staff/tax-import/preview", authn.PermTaxWrite,
		s.handleTaxPreview)
	r.staffMutation(http.MethodPost, "/api/v1/staff/tax-import/import", authn.PermTaxWrite,
		s.handleTaxImport)
	r.staffGet("/api/v1/staff/tax-import/runs", authn.PermTaxWrite, s.handleTaxImportRuns)
	r.staffGet("/api/v1/staff/tax-import/provenance", authn.PermTaxWrite, s.handleTaxProvenance)
}

// taxImporter builds the importer for this request. It holds no per-request state, so a fresh one
// is cheap; the HTTP client inside is shared, so connections are reused.
func (s *Server) taxImporter() *tax.Importer {
	return tax.New(s.pool)
}

// taxSourceResponse is the source screen's payload.
type taxSourceResponse struct {
	// Configured distinguishes "no source yet" from "a source that is switched off", which are
	// different prompts: one is a setup step, the other is an operator's decision.
	Configured bool        `json:"configured"`
	Source     *tax.Source `json:"source"`
	LastRun    *tax.Run    `json:"last_run,omitempty"`
}

// handleTaxSource reads the configured source and the most recent run.
func (s *Server) handleTaxSource(w http.ResponseWriter, r *http.Request) {
	importer := s.taxImporter()
	src, err := importer.Source(r.Context())
	if err != nil && !errors.Is(err, tax.ErrNoSource) {
		s.staffError(w, r, err)
		return
	}
	out := taxSourceResponse{}
	if err == nil {
		out.Configured = true
		out.Source = &src
	}
	runs, runErr := importer.Runs(r.Context(), 1)
	if runErr != nil {
		s.staffError(w, r, runErr)
		return
	}
	if len(runs) > 0 {
		out.LastRun = &runs[0]
	}
	writeJSON(w, http.StatusOK, out)
}

// handleSaveTaxSource stores the source definition.
//
// It is audited with the old and new URL, because repointing the source changes which document
// every future rate comes from: an operator reading the audit log a year later needs to know when
// the installation started trusting a different feed.
func (s *Server) handleSaveTaxSource(w http.ResponseWriter, r *http.Request) {
	var in tax.SourceInput
	if !s.decodeJSON(w, r, &in, 16<<10) {
		return
	}
	actorID := ""
	if user, ok := userFrom(r.Context()); ok {
		actorID = user.ID
	}
	importer := s.taxImporter()
	before, beforeErr := importer.Source(r.Context())
	var saved tax.Source
	err := s.pool.InTx(r.Context(), func(ctx context.Context, tx pgx.Tx) error {
		var err error
		saved, err = importer.SaveSourceTx(ctx, tx, in, actorID)
		if err != nil {
			return err
		}
		changes := map[string]any{
			"url": saved.URL, "format": saved.Format, "enabled": saved.Enabled,
			"refresh_interval_hours": saved.RefreshIntervalHours,
		}
		if beforeErr == nil {
			changes["previous_url"] = before.URL
			changes["previous_format"] = before.Format
		}
		// No target id: the source is a singleton with no uuid of its own, and inventing one would
		// point the audit row at nothing. The URL in `changes` is the identity that matters.
		return audit.Record(ctx, tx, s.auditEvent(r, "tax_import.source", "tax_import_source",
			"", audit.OutcomeSuccess).WithChanges(changes))
	})
	if err != nil {
		s.taxError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, taxSourceResponse{Configured: true, Source: &saved})
}

// taxPreviewRequest is the body of both the preview and the import.
type taxPreviewRequest struct {
	// OverwriteManual is the operator's explicit permission to replace a rate a human typed. It
	// is read here and passed straight through; the importer is the thing that enforces it.
	OverwriteManual bool `json:"overwrite_manual"`
}

// handleTaxPreview fetches the source and reports what an import would do.
func (s *Server) handleTaxPreview(w http.ResponseWriter, r *http.Request) {
	var in taxPreviewRequest
	if !s.decodeJSON(w, r, &in, 16<<10) {
		return
	}
	preview, err := s.taxImporter().Preview(r.Context(), tax.Options{OverwriteManual: in.OverwriteManual})
	if err != nil {
		s.taxError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"preview": preview})
}

// handleTaxImport fetches the source and applies it.
func (s *Server) handleTaxImport(w http.ResponseWriter, r *http.Request) {
	var in taxPreviewRequest
	if !s.decodeJSON(w, r, &in, 16<<10) {
		return
	}
	actorID := ""
	if user, ok := userFrom(r.Context()); ok {
		actorID = user.ID
	}
	opts := tax.Options{
		OverwriteManual: in.OverwriteManual,
		Trigger:         tax.TriggerManual,
		ActorID:         actorID,
	}
	// The audit row commits with the rates: an import that happened without a trail is worse than
	// one that failed loudly.
	opts.Audit = func(ctx context.Context, tx pgx.Tx, result tax.Result) error {
		return audit.Record(ctx, tx, s.auditEvent(r, "tax_import.apply", "tax_import_run",
			result.Run.ID, audit.OutcomeSuccess).WithChanges(map[string]any{
			"source_url":       result.Run.SourceURL,
			"overwrite_manual": in.OverwriteManual,
			"created":          result.Run.Counts.Created,
			"updated":          result.Run.Counts.Updated,
			"unchanged":        result.Run.Counts.Unchanged,
			"protected":        result.Run.Counts.Protected,
			"rejected":         result.Run.Counts.Rejected,
		}))
	}
	result, err := s.taxImporter().Apply(r.Context(), opts)
	if err != nil {
		s.taxError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// handleTaxImportRuns lists recent runs, newest first.
func (s *Server) handleTaxImportRuns(w http.ResponseWriter, r *http.Request) {
	limit := 20
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil {
			limit = parsed
		}
	}
	runs, err := s.taxImporter().Runs(r.Context(), limit)
	if err != nil {
		s.staffError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": runs})
}

// handleTaxProvenance returns every rate's origin.
//
// It is a separate read from the rate list because the rate list is commerce's projection, shared
// with the pricer and the staff form. The screen joins the two by id.
func (s *Server) handleTaxProvenance(w http.ResponseWriter, r *http.Request) {
	provenance, err := s.taxImporter().Provenance(r.Context())
	if err != nil {
		s.staffError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": provenance})
}

// taxError maps the importer's errors to stable codes.
//
// The distinction that matters to the screen: "nothing is configured" and "the source is switched
// off" are configuration states an operator fixes on this page, while "unreachable" and "unreadable"
// are failures they fix on the other end. Collapsing them into one 500 would make the screen unable
// to say which.
func (s *Server) taxError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, tax.ErrNoSource):
		writeProblem(w, http.StatusConflict, "no_source",
			"no tax-rate source is configured; save a URL first")
	case errors.Is(err, tax.ErrSourceDisabled):
		writeProblem(w, http.StatusConflict, "source_disabled",
			"the tax-rate source is switched off; enable it before fetching")
	case errors.Is(err, tax.ErrInvalidSource):
		writeProblem(w, http.StatusUnprocessableEntity, "invalid_source", err.Error())
	case errors.Is(err, tax.ErrBadDocument):
		writeProblem(w, http.StatusUnprocessableEntity, "source_document_invalid", err.Error())
	case errors.Is(err, tax.ErrFetch):
		writeProblem(w, http.StatusBadGateway, "source_unreachable", err.Error())
	default:
		s.staffError(w, r, err)
	}
}
