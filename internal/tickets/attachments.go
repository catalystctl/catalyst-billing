package tickets

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"mime"
	"path/filepath"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Attachments.
//
// Three decisions shape this file:
//
//  1. **The cap is enforced before the body is read.** A handler that reads first and
//     checks afterwards has already allocated the memory an attacker asked it to. The
//     HTTP layer reads at most MaxAttachmentBytes+1 through an io.LimitReader, and this
//     package checks again, because a second caller (email piping) will not go through
//     that handler.
//  2. **Bytes live in the row, deduped by digest.** The same file attached to two
//     messages is stored once. A container restart must not lose an attachment, which
//     rules out a local directory, and the CSP rules out a third-party bucket.
//  3. **A download is served by us, with a safe type.** The stored content type is
//     normalised on the way in, and the endpoint sends `X-Content-Type-Options: nosniff`
//     with `Content-Disposition: attachment` so a browser never renders an uploaded file
//     as a document on our origin.

// AttachmentInput is one uploaded file.
type AttachmentInput struct {
	Filename string
	// ContentType is the type the client claimed. It is normalised and, for types that
	// cannot be served safely, replaced with application/octet-stream.
	ContentType string
	Bytes       []byte
}

// AllowedAttachmentTypes is the closed set of file types an upload may carry.
//
// It is an allowlist, not a blocklist, because a blocklist was the bug: "downgrade the types a
// browser renders" leaves every type nobody thought of accepted as itself. The set below is what
// a support desk actually receives — screenshots, logs, PDFs, spreadsheets and archives — plus
// application/octet-stream for a file whose type is genuinely unknown, which is stored and served
// as an opaque download.
//
// What is deliberately absent: text/html, image/svg+xml, application/javascript, text/xml and
// their relatives. They are refused rather than downgraded, because a ticket is not a place to
// hand a support agent a document that executes. The refusal names the accepted types, so a
// customer with a .html file knows to zip it instead of guessing.
var AllowedAttachmentTypes = []string{
	"image/png",
	"image/jpeg",
	"image/gif",
	"image/webp",
	"application/pdf",
	"text/plain",
	"text/csv",
	"text/markdown",
	"application/json",
	"application/zip",
	"application/gzip",
	"application/x-tar",
	"application/x-7z-compressed",
	"application/x-rar-compressed",
	"application/msword",
	"application/vnd.ms-excel",
	"application/vnd.ms-powerpoint",
	"application/vnd.openxmlformats-officedocument.wordprocessingml.document",
	"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
	"application/vnd.openxmlformats-officedocument.presentationml.presentation",
	"application/vnd.oasis.opendocument.text",
	"application/vnd.oasis.opendocument.spreadsheet",
	// The honest answer for a file with no type: stored and served as opaque bytes, never as
	// something a browser would render on our origin.
	"application/octet-stream",
}

// allowedAttachmentTypeSet is AllowedAttachmentTypes as a lookup, built once.
var allowedAttachmentTypeSet = func() map[string]struct{} {
	out := make(map[string]struct{}, len(AllowedAttachmentTypes))
	for _, t := range AllowedAttachmentTypes {
		out[t] = struct{}{}
	}
	return out
}()

// IsAllowedAttachmentType reports whether the normalised type is on the allowlist.
func IsAllowedAttachmentType(contentType string) bool {
	_, ok := allowedAttachmentTypeSet[strings.TrimSpace(strings.ToLower(contentType))]
	return ok
}

// AttachmentTypeList renders the allowlist for an error message.
func AttachmentTypeList() string { return strings.Join(AllowedAttachmentTypes, ", ") }

// ValidateAttachment checks a file before it is stored.
func ValidateAttachment(in AttachmentInput) (AttachmentInput, error) {
	filename := strings.TrimSpace(filepath.Base(strings.ReplaceAll(in.Filename, `\`, `/`)))
	if filename == "" || filename == "." || filename == ".." {
		return in, fmt.Errorf("%w: the attachment needs a filename", ErrInvalidInput)
	}
	if len(filename) > MaxAttachmentNameLen {
		return in, fmt.Errorf("%w: the filename is too long", ErrInvalidInput)
	}
	if len(in.Bytes) == 0 {
		return in, fmt.Errorf("%w: the attachment is empty", ErrInvalidInput)
	}
	if len(in.Bytes) > MaxAttachmentBytes {
		return in, fmt.Errorf("%w: %d bytes exceeds %d", ErrAttachmentTooLarge,
			len(in.Bytes), MaxAttachmentBytes)
	}
	in.Filename = filename
	// The allowlist is applied to the type the file would be *stored* as, so an extension that
	// implies a refused type (a .svg with no Content-Type) is refused too.
	normalized := normalizedContentType(filename, in.ContentType)
	if !IsAllowedAttachmentType(normalized) {
		return in, fmt.Errorf("%w: %q is not an accepted file type (accepted: %s)",
			ErrAttachmentType, normalized, AttachmentTypeList())
	}
	in.ContentType = SafeContentType(filename, normalized)
	return in, nil
}

// SafeContentType normalises an uploaded file's type.
//
// The stored type is what the download endpoint serves, so this is a security
// decision rather than a formatting one: `text/html`, `image/svg+xml` and anything
// else a browser would treat as a document is served as an opaque download instead.
// The original name is still in the filename, so the customer's file is not
// mislabelled to them.
//
// AllowedAttachmentTypes already refuses those types, so this is the second line of
// defence: a type that reaches storage by a path that skipped ValidateAttachment still
// cannot be served as a document.
func SafeContentType(filename, claimed string) string {
	normalized := normalizedContentType(filename, claimed)
	if normalized == "" {
		return "application/octet-stream"
	}
	if _, _, err := mime.ParseMediaType(normalized); err != nil {
		return "application/octet-stream"
	}
	// Anything a browser renders with our origin's privileges is downgraded. The
	// download is an attachment, but a user who saves it and opens it locally should
	// not have been handed a page that was labelled harmless.
	switch {
	case strings.HasPrefix(normalized, "text/html"),
		strings.HasPrefix(normalized, "application/xhtml"),
		strings.Contains(normalized, "svg"),
		strings.HasPrefix(normalized, "application/javascript"),
		strings.HasPrefix(normalized, "text/javascript"),
		strings.HasPrefix(normalized, "application/xml"),
		strings.HasPrefix(normalized, "text/xml"):
		return "application/octet-stream"
	}
	return normalized
}

// normalizedContentType is the lower-cased type without parameters: what the client claimed, or
// what the extension implies when the client claimed nothing. It is the input to both the
// allowlist and the downgrade above, so the two cannot disagree about what a file is.
func normalizedContentType(filename, claimed string) string {
	normalized := strings.TrimSpace(strings.ToLower(strings.Split(claimed, ";")[0]))
	if normalized == "" {
		if byExt := mime.TypeByExtension(strings.ToLower(filepath.Ext(filename))); byExt != "" {
			normalized = strings.TrimSpace(strings.ToLower(strings.Split(byExt, ";")[0]))
		}
	}
	return normalized
}

// DigestOf is the dedupe key: sha256 of the exact bytes.
func DigestOf(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// insertAttachment stores one file against a message, collapsing identical bytes to
// one row.
func insertAttachment(ctx context.Context, q querier, messageID string, in AttachmentInput) (Attachment, error) {
	validated, err := ValidateAttachment(in)
	if err != nil {
		return Attachment{}, err
	}
	digest := DigestOf(validated.Bytes)
	now := time.Now().UTC()

	var out Attachment
	// Dedupe first: the same file attached twice to one message is one row, so a
	// retried upload does not double the thread. The schema has no unique index for
	// this — an attachment is scoped to its message, and a retry is serialised by the
	// message insert — so the lookup is the check.
	err = q.QueryRow(ctx, `
		SELECT id, message_id, filename, content_type, byte_size, digest, created_at
		FROM ticket_attachments WHERE message_id = $1 AND digest = $2`,
		messageID, digest,
	).Scan(&out.ID, &out.MessageID, &out.Filename, &out.ContentType, &out.ByteSize,
		&out.Digest, &out.CreatedAt)
	if err == nil {
		return out, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Attachment{}, fmt.Errorf("tickets: read attachment: %w", err)
	}

	err = q.QueryRow(ctx, `
		INSERT INTO ticket_attachments (message_id, filename, content_type, bytes, byte_size,
			digest, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING id, message_id, filename, content_type, byte_size, digest, created_at`,
		messageID, validated.Filename, validated.ContentType, validated.Bytes,
		len(validated.Bytes), digest, now,
	).Scan(&out.ID, &out.MessageID, &out.Filename, &out.ContentType, &out.ByteSize,
		&out.Digest, &out.CreatedAt)
	if err != nil {
		return Attachment{}, fmt.Errorf("tickets: store attachment: %w", err)
	}
	return out, nil
}

// attachmentsForTicket lists the metadata of every attachment on a thread. It does
// not read the bytes: a thread view needs names and sizes, not a 10 MB response.
func attachmentsForTicket(ctx context.Context, q querier, ticketID string) ([]Attachment, error) {
	rows, err := q.Query(ctx, `
		SELECT a.id, a.message_id, a.filename, a.content_type, a.byte_size, a.digest, a.created_at
		FROM ticket_attachments a
		JOIN ticket_messages m ON m.id = a.message_id
		WHERE m.ticket_id = $1
		ORDER BY a.created_at, a.id`, ticketID)
	if err != nil {
		return nil, fmt.Errorf("tickets: list attachments: %w", err)
	}
	defer rows.Close()

	out := make([]Attachment, 0, 4)
	for rows.Next() {
		var a Attachment
		if err := rows.Scan(&a.ID, &a.MessageID, &a.Filename, &a.ContentType, &a.ByteSize,
			&a.Digest, &a.CreatedAt); err != nil {
			return nil, fmt.Errorf("tickets: scan attachment: %w", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("tickets: read attachments: %w", err)
	}
	return out, nil
}

// AttachmentForCustomer reads one attachment for download, scoped to the caller.
//
// The customer id is in the WHERE clause, so another customer's attachment id is
// ErrNotFound rather than a 403 that confirms the id exists.
func (s *Store) AttachmentForCustomer(ctx context.Context, customerID, attachmentID string) (Attachment, error) {
	if !validUUID(attachmentID) || !validUUID(customerID) {
		return Attachment{}, ErrNotFound
	}
	return s.attachment(ctx, `
		SELECT a.id, a.message_id, a.filename, a.content_type, a.bytes, a.byte_size,
		       a.digest, a.created_at
		FROM ticket_attachments a
		JOIN ticket_messages m ON m.id = a.message_id
		JOIN tickets t ON t.id = m.ticket_id
		WHERE a.id = $1 AND t.customer_id = $2`, attachmentID, customerID)
}

// AttachmentForStaff reads one attachment with no customer scope. The caller holds
// ticket:read:all, which the router enforces.
func (s *Store) AttachmentForStaff(ctx context.Context, attachmentID string) (Attachment, error) {
	if !validUUID(attachmentID) {
		return Attachment{}, ErrNotFound
	}
	return s.attachment(ctx, `
		SELECT a.id, a.message_id, a.filename, a.content_type, a.bytes, a.byte_size,
		       a.digest, a.created_at
		FROM ticket_attachments a
		WHERE a.id = $1`, attachmentID)
}

func (s *Store) attachment(ctx context.Context, stmt string, args ...any) (Attachment, error) {
	var a Attachment
	err := s.pool.QueryRow(ctx, stmt, args...).Scan(&a.ID, &a.MessageID, &a.Filename,
		&a.ContentType, &a.Bytes, &a.ByteSize, &a.Digest, &a.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Attachment{}, ErrNotFound
	}
	if err != nil {
		return Attachment{}, fmt.Errorf("tickets: read attachment: %w", err)
	}
	return a, nil
}

// ContentDisposition renders the header a download sends.
//
// The ASCII fallback is quoted and stripped of control characters, and the RFC 5987
// `filename*` carries the real name so a non-ASCII filename survives. A filename
// containing a quote or a newline would otherwise be a response-splitting attempt.
func ContentDisposition(filename string) string {
	safe := strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || r == '"' || r == '\\' {
			return '_'
		}
		return r
	}, filename)
	ascii := strings.Map(func(r rune) rune {
		if r > 0x7e {
			return '_'
		}
		return r
	}, safe)
	if ascii == "" {
		ascii = "attachment"
	}
	return fmt.Sprintf("attachment; filename=%q; filename*=UTF-8''%s", ascii, encodeRFC5987(safe))
}

// encodeRFC5987 percent-encodes a filename for the `filename*` parameter.
func encodeRFC5987(s string) string {
	const hexDigits = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') || c == '-' || c == '.' || c == '_' || c == '~' {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hexDigits[c>>4])
		b.WriteByte(hexDigits[c&0x0f])
	}
	return b.String()
}

// AddAttachmentAsCustomer attaches a file to a message on the caller's own ticket.
//
// The message id is optional: an empty value means "the latest message on this
// ticket", which is what the portal's reply box does when the customer attaches a
// screenshot to the reply they just sent. Both ids are checked against the customer
// in the same query, so a substituted ticket *or* message id is ErrNotFound.
func (s *Store) AddAttachmentAsCustomer(ctx context.Context, customerID, ticketID, messageID string, in AttachmentInput) (Attachment, error) {
	var out Attachment
	err := s.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = s.AddAttachmentAsCustomerTx(ctx, tx, customerID, ticketID, messageID, in)
		return err
	})
	return out, err
}

// AddAttachmentAsCustomerTx attaches a file inside a caller's transaction.
func (s *Store) AddAttachmentAsCustomerTx(ctx context.Context, tx pgx.Tx, customerID, ticketID, messageID string, in AttachmentInput) (Attachment, error) {
	settings, err := settingsFrom(ctx, tx)
	if err != nil {
		return Attachment{}, err
	}
	if settings.Disabled {
		return Attachment{}, ErrDisabled
	}
	// Refuse an oversized file before the caller stores it. The HTTP layer has already
	// bounded the read; this is the second check, for the importer that does not go
	// through that handler.
	if len(in.Bytes) > MaxAttachmentBytes {
		return Attachment{}, fmt.Errorf("%w: %d bytes exceeds %d", ErrAttachmentTooLarge,
			len(in.Bytes), MaxAttachmentBytes)
	}
	if !validUUID(ticketID) || !validUUID(customerID) {
		return Attachment{}, ErrNotFound
	}

	message, err := messageOnTicketForCustomer(ctx, tx, customerID, ticketID, messageID)
	if err != nil {
		return Attachment{}, err
	}
	return insertAttachment(ctx, tx, message, in)
}

// AddAttachmentAsStaff attaches a file to any ticket.
func (s *Store) AddAttachmentAsStaff(ctx context.Context, ticketID, messageID string, in AttachmentInput) (Attachment, error) {
	var out Attachment
	err := s.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = s.AddAttachmentAsStaffTx(ctx, tx, ticketID, messageID, in)
		return err
	})
	return out, err
}

// AddAttachmentAsStaffTx attaches a file inside a caller's transaction.
func (s *Store) AddAttachmentAsStaffTx(ctx context.Context, tx pgx.Tx, ticketID, messageID string, in AttachmentInput) (Attachment, error) {
	if len(in.Bytes) > MaxAttachmentBytes {
		return Attachment{}, fmt.Errorf("%w: %d bytes exceeds %d", ErrAttachmentTooLarge,
			len(in.Bytes), MaxAttachmentBytes)
	}
	if !validUUID(ticketID) {
		return Attachment{}, ErrNotFound
	}
	message, err := messageOnTicket(ctx, tx, ticketID, messageID)
	if err != nil {
		return Attachment{}, err
	}
	return insertAttachment(ctx, tx, message, in)
}

// messageOnTicketForCustomer resolves the target message, scoped to the customer.
func messageOnTicketForCustomer(ctx context.Context, q querier, customerID, ticketID, messageID string) (string, error) {
	if messageID != "" {
		if !validUUID(messageID) {
			return "", ErrNotFound
		}
		var id string
		err := q.QueryRow(ctx, `
			SELECT m.id
			FROM ticket_messages m
			JOIN tickets t ON t.id = m.ticket_id
			WHERE m.id = $1 AND t.id = $2 AND t.customer_id = $3`,
			messageID, ticketID, customerID).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			return "", ErrNotFound
		}
		if err != nil {
			return "", fmt.Errorf("tickets: resolve message: %w", err)
		}
		return id, nil
	}
	return latestMessageOnTicket(ctx, q, ticketID, customerID)
}

// messageOnTicket resolves the target message without a customer scope.
func messageOnTicket(ctx context.Context, q querier, ticketID, messageID string) (string, error) {
	if messageID != "" {
		if !validUUID(messageID) {
			return "", ErrNotFound
		}
		var id string
		err := q.QueryRow(ctx,
			`SELECT id FROM ticket_messages WHERE id = $1 AND ticket_id = $2`,
			messageID, ticketID).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			return "", ErrNotFound
		}
		if err != nil {
			return "", fmt.Errorf("tickets: resolve message: %w", err)
		}
		return id, nil
	}
	return latestMessageOnTicket(ctx, q, ticketID, "")
}

// latestMessageOnTicket is the default target: the newest message in the thread.
func latestMessageOnTicket(ctx context.Context, q querier, ticketID, customerID string) (string, error) {
	args := []any{ticketID}
	where := "m.ticket_id = $1"
	if customerID != "" {
		args = append(args, customerID)
		where += " AND t.customer_id = $2"
	}
	var id string
	err := q.QueryRow(ctx, `
		SELECT m.id
		FROM ticket_messages m
		JOIN tickets t ON t.id = m.ticket_id
		WHERE `+where+`
		ORDER BY m.created_at DESC, m.id DESC
		LIMIT 1`, args...).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("tickets: resolve latest message: %w", err)
	}
	return id, nil
}
