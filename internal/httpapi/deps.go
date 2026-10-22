package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"

	"github.com/catalyst/billing/internal/approvals"
	"github.com/catalyst/billing/internal/audit"
	"github.com/catalyst/billing/internal/events"
	"github.com/catalyst/billing/internal/extproto"
	"github.com/catalyst/billing/internal/notify"
	"github.com/jackc/pgx/v5"
)

// Optional dependencies.
//
// Each is attached with a With... method and may be nil. A nil dependency makes its
// endpoints refuse with a specific code rather than panic or fall open, which is what
// lets the panel run without a gateway or a mail server configured.

// CheckoutGateway is the payment side needed to start a checkout session.
//
// It is declared here rather than imported from internal/checkout so the HTTP layer
// depends on the shape it uses, not on the whole service.
type CheckoutGateway interface {
	Name() string
	CreateCheckout(ctx context.Context, p extproto.CreateCheckoutParams) (extproto.CreateCheckoutResult, error)
}

// WithGateway attaches the payment gateway used by checkout and refunds.
func (s *Server) WithGateway(g CheckoutGateway) *Server {
	s.gateway = g
	return s
}

// WithCheckoutURLs sets where the gateway returns the customer after a payment attempt.
//
// They are configuration rather than constants because a deployment behind a proxy has a
// different public origin from the one the process binds to, and a hard-coded redirect
// would send the customer to an unreachable address after paying.
func (s *Server) WithCheckoutURLs(success, cancel string) *Server {
	s.checkoutSuccessURL = success
	s.checkoutCancelURL = cancel
	return s
}

// WithEvents attaches the event broker used by the SSE endpoint.
//
// A Server without a broker still serves every other endpoint; the events endpoint
// then reports that streaming is not configured instead of holding a connection open
// that will never carry anything.
func (s *Server) WithEvents(b *events.Broker) *Server {
	s.events = b
	return s
}

// WithNotify attaches the durable notification outbox.
func (s *Server) WithNotify(q *notify.Queue) *Server {
	s.notify = q
	return s
}

// WithApprovals attaches the dual-approval service.
func (s *Server) WithApprovals(a *approvals.Service) *Server {
	s.approvals = a
	return s
}

// correlationKey is the context key for the request correlation id.
type correlationKey struct{}

// withCorrelationID stores the id.
func withCorrelationID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, correlationKey{}, id)
}

// correlationID returns the request's correlation id, or the empty string.
func correlationID(ctx context.Context) string {
	id, _ := ctx.Value(correlationKey{}).(string)
	return id
}

// newCorrelationID returns a random 128-bit hex id.
func newCorrelationID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// A correlation id is diagnostics, not security. An empty id is worse than a
		// predictable one, but neither is worth failing a request over.
		return ""
	}
	return hex.EncodeToString(b[:])
}

// requestAuditor is the audit.Auditor implementation bound to one request.
//
// Every domain service that can be driven from HTTP takes one of these, which is what makes the
// audit row for a refund or a credit note carry the same correlation id and source address as the
// request that caused it — without those services knowing anything about HTTP.
type requestAuditor struct {
	server  *Server
	request *http.Request
	reason  string
	// actor overrides the session identity. It is used by the credential flows, where the acting
	// principal is the account being changed rather than a signed-in operator.
	actor *audit.Actor
}

// RecordAction implements audit.Auditor.
func (a requestAuditor) RecordAction(ctx context.Context, tx pgx.Tx, action, targetType, targetID string,
	changes map[string]any) error {
	ev := a.server.auditEvent(a.request, action, targetType, targetID, audit.OutcomeSuccess)
	if a.actor != nil {
		ev.Actor = *a.actor
	}
	if a.reason != "" {
		if changes == nil {
			changes = map[string]any{}
		}
		if _, ok := changes["reason"]; !ok {
			changes["reason"] = a.reason
		}
	}
	return audit.Record(ctx, tx, ev.WithChanges(changes))
}

// auditorFor returns the auditor for a request, with an optional reason.
func (s *Server) auditorFor(r *http.Request, reason string) requestAuditor {
	return requestAuditor{server: s, request: r, reason: reason}
}

// actorFor builds the audit actor for the caller.
//
// The actor is taken from the session or the API key, never from the request body: an
// audit row naming an actor the caller chose is worse than no audit row, because it is
// believable.
func (s *Server) actorFor(r *http.Request) audit.Actor {
	if key, ok := apiKeyFrom(r.Context()); ok {
		return audit.Actor{ID: key.UserID, Kind: audit.ActorAPIKey}
	}
	if user, ok := userFrom(r.Context()); ok {
		actor := audit.Actor{ID: user.ID, Kind: audit.ActorUser}
		// Carry the operator behind an impersonated session, so an action taken while acting as a
		// customer is attributable to the human who took it as well as to the account it was taken on.
		if sess, ok := sessionFrom(r.Context()); ok {
			actor.ImpersonatedBy = sess.ImpersonatedBy
		}
		return actor
	}
	return audit.Actor{Kind: audit.ActorSystem}
}

// auditEvent builds an event with the request's actor, correlation id and address.
func (s *Server) auditEvent(r *http.Request, action, targetType, targetID, outcome string) audit.Event {
	return audit.Event{
		Actor:         s.actorFor(r),
		Action:        action,
		TargetType:    targetType,
		TargetID:      targetID,
		Outcome:       outcome,
		CorrelationID: correlationID(r.Context()),
		IP:            s.clientIP(r),
	}
}

// publish sends an event to the broker, if one is attached.
//
// Callers invoke this *after* the transaction has committed. Publishing inside the
// transaction would tell a client about a change that can still roll back, which is
// exactly the class of bug the SSE contract forbids.
func (s *Server) publish(ev events.Event) {
	if s.events == nil {
		return
	}
	s.events.Publish(ev)
}
