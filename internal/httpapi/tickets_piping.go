// Email piping over HTTP (E3.8 / TKT-11).
//
// Two routes live here and nothing else:
//
//  1. `POST /api/v1/webhooks/mail/piping` is the provider-facing ingest endpoint. It is
//     public — a mail provider has no cookie — and authenticated by `mail.piping_secret`
//     with a constant-time comparison. An installation that has not set the secret refuses
//     *every* request, including one that presents the empty string: an unauthenticated
//     ingest endpoint is an open door, and "we forgot to configure it" must fail closed.
//
//  2. `GET /api/v1/staff/tickets/piping` is the support view of what arrived: every inbound
//     message with its outcome, and — for the ones that were not attached — the reason. A
//     reply that could not be attributed is stored and surfaced here, never silently
//     dropped, because a customer who answered and got silence is a customer who leaves.
//
// The endpoint does not use the idempotency wrapper. A provider retry carries the same
// Message-ID, which is a stronger key than an Idempotency-Key the provider never sends, and
// the ingest core enforces it in the database.
package httpapi

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/catalyst/billing/internal/authn"
	"github.com/catalyst/billing/internal/events"
	"github.com/catalyst/billing/internal/paginate"
	"github.com/catalyst/billing/internal/tickets"
)

func init() {
	RegisterRoutes("57-ticket-piping", registerTicketPipingRoutes)
}

// maxPipingBody bounds the JSON envelope a provider posts. The message body itself is
// capped at tickets.MaxMessageBytes by the ingest core; the envelope is larger because JSON
// escaping and a References list cost bytes, and a body that exceeds the cap is refused with
// a recorded reason rather than a truncated reply.
const maxPipingBody = 512 << 10

func registerTicketPipingRoutes(r *router) {
	// The provider-facing ingest. No CSRF and no session: the secret is the authentication,
	// and there is no cookie to bind a token to.
	r.publicPost("/api/v1/webhooks/mail/piping", r.s.handleMailPiping)
	// The operator-facing view of what arrived.
	r.staffGet("/api/v1/staff/tickets/piping", authn.PermTicketReadAll, r.s.handlePipingMail)
}

// pipingMessageRequest is the JSON a provider posts for one parsed message.
//
// The fields mirror the headers the attribution core reads, so a provider with an HTTP API
// can hand over what it parsed without the panel having to parse MIME again. `headers` is
// where the loop check finds the panel's own marker and `Auto-Submitted`.
//
// `attachments` is accepted only to be counted as refused, and that is a deliberate
// asymmetry with the IMAP path (which imports files, TKT-11): this envelope carries a
// provider's *parsed* view of a message, and the bytes of a 10 MB screenshot base64'd into a
// JSON body is a contract this endpoint has not promised and cannot bound the same way the
// poller bounds a partial fetch. A deployment that wants attachments pipes the mailbox, which
// is what the poller is for. The count still reaches the mail log, so the refusal is visible
// rather than silent.
type pipingMessageRequest struct {
	MessageID string   `json:"message_id"`
	From      string   `json:"from"`
	FromName  string   `json:"from_name"`
	To        []string `json:"to"`
	Cc        []string `json:"cc"`
	Subject   string   `json:"subject"`
	InReplyTo string   `json:"in_reply_to"`
	// References may be a list or a single header value; both are accepted because providers
	// differ and neither is authoritative.
	References    []string          `json:"references"`
	Body          string            `json:"body"`
	AutoSubmitted string            `json:"auto_submitted"`
	Headers       map[string]string `json:"headers"`
	Attachments   []json.RawMessage `json:"attachments"`
	ReceivedAt    *time.Time        `json:"received_at"`
}

// handleMailPiping ingests one inbound message posted by a mail provider.
func (s *Server) handleMailPiping(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "tickets_unavailable",
			"support tickets are not configured")
		return
	}
	cfg, err := tickets.LoadPipingConfig(r.Context(), s.pool)
	if err != nil {
		s.cfg.Logger.ErrorContext(r.Context(), "read piping configuration", "error", err)
		writeProblem(w, http.StatusServiceUnavailable, "piping_unavailable",
			"the piping configuration could not be read")
		return
	}
	// Fail closed. An empty secret means the operator has not configured piping, and an
	// endpoint that accepts an empty secret is an endpoint anyone can post tickets into.
	if cfg.Secret == "" {
		s.cfg.Logger.WarnContext(r.Context(), "piping webhook refused: no secret configured")
		writeProblem(w, http.StatusServiceUnavailable, "piping_not_configured",
			"email piping is not configured on this installation")
		return
	}
	if !pipingSecretMatches(r, cfg.Secret) {
		writeProblem(w, http.StatusUnauthorized, "unauthorized",
			"the piping secret is missing or wrong")
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxPipingBody+1))
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "unreadable_body", "could not read the request body")
		return
	}
	if len(body) > maxPipingBody {
		writeProblem(w, http.StatusRequestEntityTooLarge, "payload_too_large",
			"the message envelope is too large")
		return
	}

	var req pipingMessageRequest
	// Unknown fields are tolerated here, unlike every session-facing route: the sender is a
	// mail provider whose payload grows without asking, and refusing an extra field would
	// break piping on the provider's next release.
	if err := json.Unmarshal(body, &req); err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_body", "the request body is not valid JSON")
		return
	}

	msg := tickets.InboundMessage{
		MessageID:          req.MessageID,
		From:               req.From,
		FromName:           req.FromName,
		To:                 req.To,
		Cc:                 req.Cc,
		Subject:            req.Subject,
		InReplyTo:          req.InReplyTo,
		References:         req.References,
		Body:               req.Body,
		AutoSubmitted:      req.AutoSubmitted,
		Headers:            req.Headers,
		AttachmentsDropped: len(req.Attachments),
		Source:             "webhook",
	}
	if req.ReceivedAt != nil {
		msg.ReceivedAt = req.ReceivedAt.UTC()
	}

	result, err := s.ticketStore().Ingest(r.Context(), msg)
	if err != nil {
		s.ticketsError(w, r, err)
		return
	}

	// A committed reply is worth telling the open staff screens about; a refusal is not.
	if result.TicketID != "" && result.CustomerID != "" {
		kind := "ticket.replied"
		if result.Outcome == tickets.IngestCreated {
			kind = "ticket.created"
		}
		s.publish(events.Event{Kind: kind, CustomerID: result.CustomerID,
			Permission: string(authn.PermTicketReadAll),
			Data: map[string]any{
				"ticket_id":  result.TicketID,
				"message_id": result.MessageID,
				"piped":      true,
			}})
	}

	out := map[string]any{
		"outcome":  string(result.Outcome),
		"log_id":   result.LogID,
		"subject":  result.Subject,
		"source":   "webhook",
		"piped_at": time.Now().UTC(),
	}
	if result.TicketID != "" {
		out["ticket_id"] = result.TicketID
	}
	if result.CustomerID != "" {
		out["customer_id"] = result.CustomerID
	}
	if result.MessageID != "" {
		out["message_id"] = result.MessageID
	}
	if result.AttributedBy != "" {
		out["attributed_by"] = result.AttributedBy
	}
	if result.Reason != "" {
		out["reason"] = result.Reason
	}

	switch result.Outcome {
	case tickets.IngestAttached, tickets.IngestCreated:
		// 201: a message was written. 200 for a duplicate is the same success that created
		// nothing, which is exactly what a provider retry should see.
		writeJSON(w, http.StatusCreated, out)
	case tickets.IngestDuplicate:
		writeJSON(w, http.StatusOK, out)
	default:
		// 202: accepted and recorded, deliberately not attached. The provider must not retry —
		// a retry would be refused again for the same reason — so this is not a 4xx.
		writeJSON(w, http.StatusAccepted, out)
	}
}

// pipingSecretMatches compares the presented secret with the configured one in constant
// time.
//
// Both values are hashed first so the comparison is constant-time regardless of length: a
// length-dependent early return would leak the secret's size, and comparing the raw values
// with ConstantTimeCompare alone only hides the contents.
func pipingSecretMatches(r *http.Request, want string) bool {
	got := strings.TrimSpace(r.Header.Get("X-Piping-Secret"))
	if got == "" {
		// A provider that sends a bearer token instead of a custom header is accepted, because
		// the header name is the provider's choice and the secret is the authentication.
		if header := strings.TrimSpace(r.Header.Get("Authorization")); strings.HasPrefix(strings.ToLower(header), "bearer ") {
			got = strings.TrimSpace(header[len("bearer "):])
		}
	}
	if got == "" {
		return false
	}
	gotSum := sha256.Sum256([]byte(got))
	wantSum := sha256.Sum256([]byte(want))
	return subtle.ConstantTimeCompare(gotSum[:], wantSum[:]) == 1
}

// handlePipingMail lists inbound messages, newest first.
//
// Query: status=received|ignored|all, limit, cursor. The body of a refused message is
// included: an agent has to be able to read what the customer wrote in order to answer it by
// hand, and that is the whole point of surfacing the refusal instead of dropping it.
func (s *Server) handlePipingMail(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "tickets_unavailable",
			"support tickets are not configured")
		return
	}
	limit, cursor, ok := s.pageParams(w, r)
	if !ok {
		return
	}
	filter := tickets.MailLogFilter{Status: strings.TrimSpace(r.URL.Query().Get("status"))}

	rows, next, err := s.ticketStore().ListInboundMail(r.Context(), filter, limit, cursor)
	if err != nil {
		if errors.Is(err, paginate.ErrInvalidCursor) {
			writeProblem(w, http.StatusBadRequest, "invalid_cursor", "the cursor is invalid")
			return
		}
		s.ticketsError(w, r, err)
		return
	}

	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		entry := map[string]any{
			"id":                  row.ID,
			"from":                row.From,
			"to":                  row.To,
			"subject":             row.Subject,
			"body":                row.Body,
			"status":              row.Status,
			"attachments_dropped": row.AttachmentsDropped,
			"source":              row.Source,
			"created_at":          row.CreatedAt.UTC(),
		}
		if row.MessageID != "" {
			entry["message_id"] = row.MessageID
		}
		if row.TicketID != "" {
			entry["ticket_id"] = row.TicketID
		}
		if row.CustomerID != "" {
			entry["customer_id"] = row.CustomerID
		}
		if row.AttributedBy != "" {
			entry["attributed_by"] = row.AttributedBy
		}
		if row.Reason != "" {
			entry["reason"] = row.Reason
		}
		out = append(out, entry)
	}
	body := map[string]any{"messages": out}
	if next != "" {
		body["next_cursor"] = next
	}
	writeJSON(w, http.StatusOK, body)
}
