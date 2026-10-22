// Support ticket attachments.
//
// The upload and download endpoints follow the pattern internal/httpapi/catalog.go
// established for product images, and for the same reasons:
//
//   - **The body is bounded before it is read.** A Content-Length over the cap is
//     refused without touching the body, and a chunked upload is read through an
//     io.LimitReader one byte over the cap so "exactly at the limit" and "over it"
//     are distinguishable. Reading first and checking afterwards has already
//     allocated what the caller asked for.
//   - **The upload is not wrapped in the idempotency store.** That wrapper buffers
//     the whole body to digest it, with a 1 MB limit, so a 9 MB file would be
//     refused by the wrapper with the wrong code. The attachment insert is
//     idempotent by (message, digest) instead: the same bytes attached twice to one
//     message is one row.
//   - **A download is served by us with a checked type.** The stored content type is
//     normalised on the way in (anything a browser would treat as a document becomes
//     application/octet-stream), the response carries nosniff, and it is always an
//     attachment — never inline, so an uploaded page cannot run on our origin.
package httpapi

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/catalyst/billing/internal/audit"
	"github.com/catalyst/billing/internal/authn"
	"github.com/catalyst/billing/internal/tickets"
	"github.com/jackc/pgx/v5"
)

func init() {
	RegisterRoutes("57-ticket-attachments", registerTicketAttachmentRoutes)
}

func registerTicketAttachmentRoutes(r *router) {
	// The upload is a permissioned, CSRF-protected mutation that deliberately opts
	// out of the idempotency wrapper (see the package comment). It is still rate
	// limited as a mutation: an upload loop is a write loop.
	r.add(Route{
		Method:     http.MethodPost,
		Pattern:    "/api/v1/portal/tickets/{id}/attachments",
		Access:     AccessPermission,
		Permission: authn.PermTicketWriteSelf,
		CSRF:       true,
		Rate:       RateMutation,
		Handler:    r.s.handleTicketAttachmentUpload,
	})
	r.add(Route{
		Method:     http.MethodPost,
		Pattern:    "/api/v1/staff/tickets/{id}/attachments",
		Access:     AccessPermission,
		Permission: authn.PermTicketWriteAll,
		StaffOnly:  true,
		CSRF:       true,
		Rate:       RateMutation,
		Handler:    r.s.handleStaffTicketAttachmentUpload,
	})

	r.get("/api/v1/portal/tickets/attachments/{id}",
		authn.PermTicketReadSelf, r.s.handleTicketAttachmentDownload)
	r.staffGet("/api/v1/staff/tickets/attachments/{id}",
		authn.PermTicketReadAll, r.s.handleStaffTicketAttachmentDownload)
}

// readTicketUpload reads one attachment from the request.
//
// The filename comes from `?filename=` rather than from a multipart part, because
// the body is the file and nothing else: one fewer parser in the path, and the
// panel already has a File object whose name it knows. It is trimmed to its base
// name by tickets.ValidateAttachment, so a caller cannot choose a path.
func (s *Server) readTicketUpload(w http.ResponseWriter, r *http.Request) (tickets.AttachmentInput, bool) {
	filename := strings.TrimSpace(r.URL.Query().Get("filename"))
	if filename == "" {
		writeProblem(w, http.StatusBadRequest, "invalid_body",
			"a filename query parameter is required")
		return tickets.AttachmentInput{}, false
	}
	// Refuse on the declared length before reading anything. A lying length is caught
	// by the LimitReader below.
	if r.ContentLength > tickets.MaxAttachmentBytes {
		writeProblem(w, http.StatusRequestEntityTooLarge, "attachment_too_large",
			"the attachment exceeds 10 MB")
		return tickets.AttachmentInput{}, false
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, tickets.MaxAttachmentBytes+1))
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_body", "the upload could not be read")
		return tickets.AttachmentInput{}, false
	}
	if len(body) > tickets.MaxAttachmentBytes {
		writeProblem(w, http.StatusRequestEntityTooLarge, "attachment_too_large",
			"the attachment exceeds 10 MB")
		return tickets.AttachmentInput{}, false
	}
	if len(body) == 0 {
		writeProblem(w, http.StatusBadRequest, "invalid_body", "the attachment is empty")
		return tickets.AttachmentInput{}, false
	}
	contentType := strings.TrimSpace(strings.Split(r.Header.Get("Content-Type"), ";")[0])
	return tickets.AttachmentInput{Filename: filename, ContentType: contentType, Bytes: body}, true
}

// handleTicketAttachmentUpload attaches a file to the caller's own ticket.
func (s *Server) handleTicketAttachmentUpload(w http.ResponseWriter, r *http.Request) {
	if !s.requireTickets(w) {
		return
	}
	customerID, ok := s.customerFor(w, r)
	if !ok {
		return
	}
	in, ok := s.readTicketUpload(w, r)
	if !ok {
		return
	}
	ticketID := r.PathValue("id")
	messageID := strings.TrimSpace(r.URL.Query().Get("message_id"))

	var attachment tickets.Attachment
	err := s.pool.InTx(r.Context(), func(ctx context.Context, tx pgx.Tx) error {
		var err error
		attachment, err = s.ticketStore().AddAttachmentAsCustomerTx(ctx, tx, customerID,
			ticketID, messageID, in)
		if err != nil {
			return err
		}
		return audit.Record(ctx, tx, s.auditEvent(r, "ticket.attachment", "ticket", ticketID,
			audit.OutcomeSuccess).WithChanges(map[string]any{
			"attachment_id": attachment.ID,
			"filename":      attachment.Filename,
			"byte_size":     attachment.ByteSize,
		}))
	})
	if err != nil {
		s.ticketsError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, attachmentJSON(attachment, false))
}

// handleStaffTicketAttachmentUpload attaches a file to any ticket.
func (s *Server) handleStaffTicketAttachmentUpload(w http.ResponseWriter, r *http.Request) {
	if !s.requireTickets(w) {
		return
	}
	in, ok := s.readTicketUpload(w, r)
	if !ok {
		return
	}
	ticketID := r.PathValue("id")
	messageID := strings.TrimSpace(r.URL.Query().Get("message_id"))

	var attachment tickets.Attachment
	err := s.pool.InTx(r.Context(), func(ctx context.Context, tx pgx.Tx) error {
		var err error
		attachment, err = s.ticketStore().AddAttachmentAsStaffTx(ctx, tx, ticketID, messageID, in)
		if err != nil {
			return err
		}
		return audit.Record(ctx, tx, s.auditEvent(r, "ticket.attachment", "ticket", ticketID,
			audit.OutcomeSuccess).WithChanges(map[string]any{
			"attachment_id": attachment.ID,
			"filename":      attachment.Filename,
			"byte_size":     attachment.ByteSize,
		}))
	})
	if err != nil {
		s.ticketsError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, attachmentJSON(attachment, true))
}

// handleTicketAttachmentDownload serves an attachment the caller's customer owns.
func (s *Server) handleTicketAttachmentDownload(w http.ResponseWriter, r *http.Request) {
	if !s.requireTickets(w) {
		return
	}
	customerID, ok := s.customerFor(w, r)
	if !ok {
		return
	}
	attachment, err := s.ticketStore().AttachmentForCustomer(r.Context(), customerID, r.PathValue("id"))
	if err != nil {
		s.ticketsError(w, r, err)
		return
	}
	writeAttachment(w, r, attachment)
}

// handleStaffTicketAttachmentDownload serves any attachment. The route is gated by
// ticket:read:all and staff-only, which is the boundary for a queue.
func (s *Server) handleStaffTicketAttachmentDownload(w http.ResponseWriter, r *http.Request) {
	if !s.requireTickets(w) {
		return
	}
	attachment, err := s.ticketStore().AttachmentForStaff(r.Context(), r.PathValue("id"))
	if err != nil {
		s.ticketsError(w, r, err)
		return
	}
	writeAttachment(w, r, attachment)
}

// writeAttachment renders one file with the headers that make it safe.
//
//   - Content-Type is the stored, normalised type and nosniff stops the browser
//     second-guessing it.
//   - Content-Disposition is always `attachment`, so even a type that slipped
//     through is downloaded rather than rendered on our origin.
//   - The cache is private: an attachment is one customer's file, and a shared proxy
//     must not keep it.
//   - A CSP of `default-src 'none'; sandbox` is the third line of defence for the
//     case where a user opens the URL directly in a browser that ignores nosniff.
func writeAttachment(w http.ResponseWriter, r *http.Request, a tickets.Attachment) {
	h := w.Header()
	h.Set("Content-Type", a.ContentType)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Disposition", tickets.ContentDisposition(a.Filename))
	h.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	h.Set("Cache-Control", "private, no-store")
	h.Set("Content-Length", strconv.Itoa(len(a.Bytes)))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(a.Bytes)
	}
}
