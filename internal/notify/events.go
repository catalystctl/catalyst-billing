package notify

// The event catalogue, and one function per event.
//
// An event function is the whole of "tell the customer this happened": it renders the
// editable template, decides which channels the user allows, enqueues the mail through the
// existing outbox, and writes the inbox row. It takes a pgx.Tx, so all of it runs inside the
// caller's transaction: a message exists if and only if the change it describes committed.
//
// # Why one function per event rather than a generic Send(ctx, tx, event, vars)
//
// A generic call site would be shorter, and wrong. The variables a template may use are part
// of the template's contract, and a caller that passes the wrong map produces a message with
// a visible {{placeholder}} in it — or, worse, a plausible message with the wrong invoice
// number in it. Naming the parameters in Go means the compiler checks that the caller has an
// invoice number to give, and the signature is the documentation of what the template can
// say.
//
// Every function has the same shape:
//
//	func InvoicePaid(ctx context.Context, tx pgx.Tx, queue *Queue, in InvoicePaidInput) (Outcome, error)
//
// `tx` is the caller's transaction and `queue` is the outbox. They are separate arguments
// rather than one object because Queue also drives the drain loop, which runs outside any
// transaction; keeping them apart means an event can only ever be enqueued into the
// transaction it was given.
//
// # Errors
//
// A missing template is an error (ErrTemplateNotFound) and the caller should let it fail the
// transaction: an event that cannot be described is a bug, and a silent skip is how a
// customer ends up not being told their server was terminated. A disabled template or a
// preference that says "never" is *not* an error — it is a decision, and Result records it.
//
// # Dedupe
//
// Each function takes a dedupe key component naming the record the event is about, and
// derives the outbox key from the event and that component, e.g. "invoice_paid:<invoice id>".
// Replaying the same event therefore writes one delivery row and one inbox row, which is what
// makes a retried payment webhook safe.

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Event keys. These are the values in notification_templates.event_key, the outbox `kind`
// and the inbox `event_key` — one string per event across all three tables, so a support
// question ("why did this customer get this?") is answered by one join.
const (
	// The two account-security events reuse the outbox kinds that already exist, so nothing
	// that is wired today changes behaviour.
	EventPasswordReset     = KindPasswordReset
	EventEmailVerification = KindEmailVerification

	EventNewLoginDetected            = "new_login_detected"
	EventNewInvoiceCreated           = "new_invoice_created"
	EventInvoicePaid                 = "invoice_paid"
	EventInvoicePaymentFailed        = "invoice_payment_failed"
	EventNewOrderCreated             = "new_order_created"
	EventNewServerCreated            = "new_server_created"
	EventServerSuspended             = "server_suspended"
	EventServerTerminated            = "server_terminated"
	EventServiceCancellationReceived = "service_cancellation_received"
	EventNewTicketMessage            = "new_ticket_message"
)

// EventInfo describes one event for the preferences screen.
type EventInfo struct {
	Key  string
	Name string
	// Force is true when the user cannot opt out. A password reset and an email verification
	// are force events because they are the only way to complete an action the user
	// themselves started: opting out of the reset mail does not protect anyone, it just makes
	// the account unrecoverable.
	Force bool
}

// Catalogue is every event the product emits, in the order a preferences screen shows them:
// account security first, then money, then service, then support.
func Catalogue() []EventInfo {
	return []EventInfo{
		{Key: EventPasswordReset, Name: "Password reset", Force: true},
		{Key: EventEmailVerification, Name: "Email verification", Force: true},
		{Key: EventNewLoginDetected, Name: "Sign-in from a new device"},
		{Key: EventNewInvoiceCreated, Name: "New invoice"},
		{Key: EventInvoicePaid, Name: "Invoice paid"},
		{Key: EventInvoicePaymentFailed, Name: "Invoice payment failed"},
		{Key: EventNewOrderCreated, Name: "New order"},
		{Key: EventNewServerCreated, Name: "Server created"},
		{Key: EventServerSuspended, Name: "Server suspended"},
		{Key: EventServerTerminated, Name: "Server terminated"},
		{Key: EventServiceCancellationReceived, Name: "Cancellation received"},
		{Key: EventNewTicketMessage, Name: "New ticket reply"},
	}
}

// IsKnownEvent reports whether key is in the catalogue.
//
// The staff template screen uses it to refuse a template for an event nothing emits: a
// template for an event that does not exist looks like a working feature and is a
// permanently silent one.
func IsKnownEvent(key string) bool {
	for _, e := range Catalogue() {
		if e.Key == key {
			return true
		}
	}
	return false
}

// Outcome records what one event function did, for the caller's log and for tests.
//
// It is returned rather than only logged because "we deliberately sent nothing" and "we
// failed to send" must be distinguishable by the code that called it: the first is a
// preference, the second is an incident.
type Outcome struct {
	EventKey string
	// Mailed is true when at least one outbox row was created by this call. A replayed event
	// reports false, because the rows already existed.
	Mailed bool
	// Inboxed is true when an inbox row was created.
	Inboxed bool
	// Reason explains why a channel was skipped, and is empty when nothing was skipped. The
	// values are stable strings a caller can branch on: "template_disabled", "mail_disabled",
	// "mail_disabled_by_setting", "in_app_disabled", "preference_off", "preference_never",
	// "no_recipient", "no_user".
	Reason string
}

// Target is the input common to every event: who to tell, and which record the event is
// about.
type Target struct {
	// UserID is the recipient's account. It is required for the inbox and for the preference
	// lookup; when it is empty the mail is still sent if the caller supplied an address, and
	// the inbox row is skipped. That combination exists for a customer with no login yet.
	UserID string
	// Email is the recipient address. An empty address skips the mail channel; a caller may
	// legitimately not know it, and guessing is worse than not sending.
	Email string
	// DedupeKey names the record this event is about: an invoice id, a service id, a ticket
	// message id. It is what makes a replay a no-op. An empty value disables deduplication,
	// which is only correct for an event a human explicitly asked for.
	DedupeKey string
	// Vars are the template variables. The event functions below build these, so a caller
	// never has to know a template's vocabulary.
	Vars map[string]string
	// MailHold, when non-empty, is why the caller knows the mail channel must not be used even
	// though the recipient exists — an unverified address, a bounced domain. The mail is then
	// recorded as suppressed with this reason instead of being sent, and the inbox channel is
	// unaffected. It exists so a caller does not have to choose between "send to an address we do
	// not trust" and "drop the record of what we would have said".
	MailHold string
}

// dispatch is the one implementation every event function calls.
//
// Keeping the policy here rather than in twelve functions is what makes "a force event
// ignores an opt-out" true for every force event, including ones added later.
func dispatch(ctx context.Context, tx pgx.Tx, queue *Queue, eventKey string, d Target) (Outcome, error) {
	res := Outcome{EventKey: eventKey}

	tmpl, err := TemplateFor(ctx, tx, eventKey)
	if err != nil {
		return res, err
	}
	if !tmpl.Enabled {
		res.Reason = "template_disabled"
		return res, nil
	}

	decision, err := Decide(ctx, tx, d.UserID, eventKey)
	if err != nil {
		return res, err
	}

	// A template's links are panel paths. The two channels need different forms of the same
	// value — a mail must carry an absolute URL or the customer clicks nothing, while an inbox
	// row is rendered inside the panel and an absolute URL there would break a deployment
	// behind a proxy — so the variables are rendered twice rather than once with a compromise.
	vars := make(map[string]string, len(d.Vars)+1)
	for k, v := range d.Vars {
		vars[k] = v
	}
	// Every template may use {{company_name}}; supplying it once here rather than in twelve
	// callers is what keeps a new event from rendering a visible placeholder. It is read from
	// settings so an operator's branding change reaches the mail without a deploy, and a
	// missing setting falls back rather than blanking the sentence.
	if _, ok := vars["company_name"]; !ok {
		vars["company_name"] = companyName(ctx, tx)
	}
	rendered := Render(tmpl, vars)
	// The mail gets the absolute form of the same links.
	mailRendered := Render(tmpl, absoluteLinks(vars))
	// The branded HTML shell is rendered here, inside the same transaction, because the drain
	// runs later in another process: what it claims must be what this change produced, with the
	// brand as it was when the change committed. `mail.brand_html = false` yields "", which is a
	// plain-text mail, and the text body below is never affected either way.
	mailHTML := MailBrandFor(ctx, tx).HTMLPart(mailRendered, publicURL())

	// ---- mail ----
	//
	// Three switches decide the mail channel, and they are checked in order of authority: the
	// installation-wide kill switch, the per-template channel toggle, and the user's own
	// preference. The kill switch comes first because it is the operator's emergency brake
	// (a misconfigured SMTP relay must stop every message, not only the one they remembered to
	// disable), and because it is the one switch a customer cannot override.
	if !decision.Mail {
		res.Reason = decision.MailReason
	} else if !tmpl.MailEnabled {
		res.Reason = "mail_disabled"
	} else if mailDisabledBySetting(ctx, tx) {
		res.Reason = "mail_disabled_by_setting"
		// Record the decision rather than only skipping it. "We did not send it" and "we failed
		// to send it" are different answers to a support ticket, and only one of them is a bug.
		// The row is written with status 'suppressed', so the drain loop never claims it.
		if queue != nil {
			if err := queue.Suppress(ctx, tx, Message{
				Kind:      eventKey,
				To:        d.Email,
				Subject:   mailRendered.Subject,
				Body:      mailRendered.Body,
				HTML:      mailHTML,
				DedupeKey: dedupeKey(eventKey, d.DedupeKey),
			}, "mail.disable is set"); err != nil {
				return res, err
			}
		}
	} else if strings.TrimSpace(d.MailHold) != "" {
		res.Reason = "mail_held"
		if queue != nil {
			if err := queue.Suppress(ctx, tx, Message{
				Kind:      eventKey,
				To:        d.Email,
				Subject:   mailRendered.Subject,
				Body:      mailRendered.Body,
				HTML:      mailHTML,
				DedupeKey: dedupeKey(eventKey, d.DedupeKey),
			}, d.MailHold); err != nil {
				return res, err
			}
		}
	} else if strings.TrimSpace(d.Email) == "" {
		res.Reason = "no_recipient"
	} else if queue == nil {
		return res, fmt.Errorf("notify: %s: no queue configured", eventKey)
	} else {
		created, err := enqueueMail(ctx, tx, queue, eventKey, d, mailRendered, mailHTML)
		if err != nil {
			return res, err
		}
		res.Mailed = created
	}

	// ---- in-app ----
	if !decision.InApp {
		if res.Reason == "" {
			res.Reason = decision.InAppReason
		}
		return res, nil
	}
	if !tmpl.InAppEnabled {
		if res.Reason == "" {
			res.Reason = "in_app_disabled"
		}
		return res, nil
	}
	if d.UserID == "" {
		if res.Reason == "" {
			res.Reason = "no_user"
		}
		return res, nil
	}
	created, err := Create(ctx, tx, Notification{
		UserID:    d.UserID,
		EventKey:  eventKey,
		Title:     rendered.InAppTitle,
		Body:      rendered.InAppBody,
		URL:       rendered.InAppURL,
		DedupeKey: dedupeKey(eventKey, d.DedupeKey),
	})
	if err != nil {
		return res, err
	}
	res.Inboxed = created
	return res, nil
}

// absoluteLinks returns a copy of the template variables with panel-relative links made
// absolute.
//
// The copy matters: the caller's map is the inbox's, and mutating it would make the inbox
// store an absolute URL that a deployment behind a proxy cannot resolve.
func absoluteLinks(vars map[string]string) map[string]string {
	out := make(map[string]string, len(vars))
	for k, v := range vars {
		out[k] = v
	}
	base := publicURL()
	for _, key := range []string{"invoice_url", "order_url", "service_url", "ticket_url", "security_url", "link"} {
		if link, ok := out[key]; ok && strings.HasPrefix(link, "/") {
			out[key] = base + link
		}
	}
	return out
}

// enqueueMail writes the delivery rows for one rendered message.
//
// CC and BCC are a fan-out, not a header, and that is a consequence of the Sender contract:
// Send(ctx, to, subject, body) delivers to one address. Rather than widen that interface —
// which every implementation and every test would have to absorb — each extra address gets
// its own outbox row with its own dedupe key. The invariant that one row is one recipient
// survives, a retry still cannot double-send, and an operator who CCs their own billing
// address still receives the message.
//
// The copy is byte-identical, so the CC recipient sees the same body the customer did. It is
// not marked as a copy in the text; a deployment that needs "this was a copy" in the message
// needs a header, which means a wider Sender, which is the integrator's call.
func enqueueMail(ctx context.Context, tx pgx.Tx, queue *Queue, eventKey string,
	d Target, r Rendered, htmlPart string) (bool, error) {

	base := dedupeKey(eventKey, d.DedupeKey)
	recipients := make([]struct {
		to   string
		role string
	}, 0, 1+len(r.CC)+len(r.BCC))
	recipients = append(recipients, struct {
		to   string
		role string
	}{d.Email, ""})
	for _, addr := range r.CC {
		recipients = append(recipients, struct {
			to   string
			role string
		}{addr, "cc"})
	}
	for _, addr := range r.BCC {
		recipients = append(recipients, struct {
			to   string
			role string
		}{addr, "bcc"})
	}

	var any bool
	for _, rcpt := range recipients {
		if strings.TrimSpace(rcpt.to) == "" {
			continue
		}
		msg := Message{
			Kind:      eventKey,
			To:        rcpt.to,
			Subject:   r.Subject,
			Body:      r.Body,
			HTML:      htmlPart,
			DedupeKey: base,
		}
		if rcpt.role != "" && base != "" {
			// A distinct key per copy, so the primary and the copies are independent: a
			// retried event finds all of them and writes none.
			msg.DedupeKey = base + ":" + rcpt.role + ":" + rcpt.to
		}
		_, created, err := queue.Enqueue(ctx, tx, msg)
		if err != nil {
			return any, err
		}
		any = any || created
	}
	return any, nil
}

// dedupeKey builds the stable key for an event about a record.
func dedupeKey(eventKey, subject string) string {
	if strings.TrimSpace(subject) == "" {
		return ""
	}
	return eventKey + ":" + subject
}

// ---------------------------------------------------------------- events --

// A note on the default `*_url` values below.
//
// A notification is read by a *customer*, and the panel only lets a customer reach /portal,
// /account, /cart, /checkout and /tickets (see CUSTOMER_PREFIXES in web/src/routes.ts). A
// default of "/invoices/<id>" would therefore deep-link a customer into the staff invoice
// screen and bounce them out of their own area — a broken link in the one place a customer is
// most likely to click it.
//
// So each default points at the customer-visible destination, and where no customer view
// does not exist yet (orders) it points at the customer's landing page rather than at a staff
// route. The caller should pass the exact path — it knows the record and the current route shape
// — and the default is only the fallback for a caller that does not.

// PasswordReset sends the reset mail.
//
// The wording comes from the editable password_reset template, and an inbox row is written
// alongside the mail so a customer who reads the panel first still sees that a reset was
// requested — the notice is a security signal, and a mailbox the customer may no longer control
// is the wrong place for the only copy of it.
//
// The dedupe key must include the token, not just the user: two genuine reset requests must
// both arrive, while a retried request for the same token must not. The caller passes the
// token (or a prefix of its hash) as Token.
//
// The token is data, and this function does not care what it is: the link is built by the caller,
// so a link built from the request Host header cannot reach a customer through here.
func PasswordReset(ctx context.Context, tx pgx.Tx, queue *Queue, in PasswordResetInput) (Outcome, error) {
	vars := map[string]string{
		"email":        in.Email,
		"link":         in.Link,
		"expires_in":   humanDuration(in.ExpiresIn),
		"company_name": orDefault(in.CompanyName, "Catalyst Billing"),
	}
	return dispatch(ctx, tx, queue, EventPasswordReset, Target{
		UserID: in.UserID, Email: in.Email,
		DedupeKey: orDefault(in.Token, in.UserID), Vars: vars,
	})
}

// PasswordResetInput is what PasswordReset needs.
type PasswordResetInput struct {
	UserID string
	Email  string
	// Link is the absolute reset URL. It is built by the caller because only the caller knows
	// the public origin, and a link built from the request Host is a link an attacker chooses.
	Link      string
	ExpiresIn time.Duration
	// Token is the dedupe component: a token prefix, never the token itself, so the outbox
	// does not become a store of working reset links.
	Token       string
	CompanyName string
}

// EmailVerification sends the confirmation mail.
func EmailVerification(ctx context.Context, tx pgx.Tx, queue *Queue, in EmailVerificationInput) (Outcome, error) {
	vars := map[string]string{
		"email":        in.Email,
		"link":         in.Link,
		"expires_in":   humanDuration(in.ExpiresIn),
		"company_name": orDefault(in.CompanyName, "Catalyst Billing"),
	}
	return dispatch(ctx, tx, queue, EventEmailVerification, Target{
		UserID: in.UserID, Email: in.Email,
		DedupeKey: orDefault(in.Token, in.UserID), Vars: vars,
	})
}

// EmailVerificationInput is what EmailVerification needs.
type EmailVerificationInput struct {
	UserID      string
	Email       string
	Link        string
	ExpiresIn   time.Duration
	Token       string
	CompanyName string
}

// InvoiceIssued tells a customer their invoice is ready.
//
// It supersedes the hardcoded InvoiceIssuedMessage: the wording now comes from the
// new_invoice_created template, and an inbox row is written alongside the mail.
func InvoiceIssued(ctx context.Context, tx pgx.Tx, queue *Queue, in InvoiceIssuedInput) (Outcome, error) {
	vars := map[string]string{
		"invoice_number": in.Number,
		"customer_name":  in.CustomerName,
		"total":          in.Total,
		"due_at":         formatWhen(in.DueAt),
		"invoice_url":    orDefault(in.InvoiceURL, "/portal/invoices/"+in.InvoiceID),
	}
	return dispatch(ctx, tx, queue, EventNewInvoiceCreated, Target{
		UserID: in.UserID, Email: in.Email, DedupeKey: in.InvoiceID, Vars: vars,
		MailHold: in.HoldMailReason,
	})
}

// InvoiceIssuedInput is what InvoiceIssued needs.
type InvoiceIssuedInput struct {
	UserID       string
	Email        string
	InvoiceID    string
	Number       string
	CustomerName string
	// Total is the already-formatted amount, e.g. "USD 12.00". Formatting money is the
	// caller's job because only it knows the currency scale.
	Total string
	DueAt time.Time
	// InvoiceURL is the panel path. A relative path is made absolute for the mail.
	InvoiceURL string
	// HoldMailReason suppresses the mail while still recording it, e.g. "recipient address is
	// not verified". The inbox row is unaffected: the invoice is still visible in the panel,
	// which is the one channel we know the customer can reach.
	HoldMailReason string
}

// InvoicePaid thanks a customer for a payment.
func InvoicePaid(ctx context.Context, tx pgx.Tx, queue *Queue, in InvoicePaidInput) (Outcome, error) {
	vars := map[string]string{
		"invoice_number": in.Number,
		"total":          in.Total,
		"paid_at":        formatWhen(in.PaidAt),
		"invoice_url":    orDefault(in.InvoiceURL, "/portal/invoices/"+in.InvoiceID),
	}
	return dispatch(ctx, tx, queue, EventInvoicePaid, Target{
		UserID: in.UserID, Email: in.Email, DedupeKey: in.InvoiceID, Vars: vars,
	})
}

// InvoicePaidInput is what InvoicePaid needs.
type InvoicePaidInput struct {
	UserID     string
	Email      string
	InvoiceID  string
	Number     string
	Total      string
	PaidAt     time.Time
	InvoiceURL string
}

// InvoicePaymentFailed tells a customer a payment attempt did not go through.
//
// The dedupe key includes the attempt time, not just the invoice: a card that fails on
// Monday and again on Friday is two facts the customer needs, while a webhook retried for
// Monday's attempt is one.
func InvoicePaymentFailed(ctx context.Context, tx pgx.Tx, queue *Queue, in InvoicePaymentFailedInput) (Outcome, error) {
	vars := map[string]string{
		"invoice_number": in.Number,
		"total":          in.Total,
		"attempted_at":   formatWhen(in.AttemptedAt),
		"failure_reason": orDefault(in.Reason, "the payment was declined"),
		"invoice_url":    orDefault(in.InvoiceURL, "/portal/invoices/"+in.InvoiceID),
	}
	return dispatch(ctx, tx, queue, EventInvoicePaymentFailed, Target{
		UserID: in.UserID, Email: in.Email, DedupeKey: withAttempt(in.InvoiceID, in.AttemptedAt),
		Vars: vars,
	})
}

// InvoicePaymentFailedInput is what InvoicePaymentFailed needs.
type InvoicePaymentFailedInput struct {
	UserID      string
	Email       string
	InvoiceID   string
	Number      string
	Total       string
	AttemptedAt time.Time
	// Reason is the gateway's decline message. It is customer-facing text from a third party,
	// which is another reason the body is plain text.
	Reason     string
	InvoiceURL string
}

// OrderCreated confirms an order.
func OrderCreated(ctx context.Context, tx pgx.Tx, queue *Queue, in OrderCreatedInput) (Outcome, error) {
	vars := map[string]string{
		"order_number": in.Number,
		"total":        in.Total,
		"order_url":    orDefault(in.OrderURL, "/portal"),
	}
	return dispatch(ctx, tx, queue, EventNewOrderCreated, Target{
		UserID: in.UserID, Email: in.Email, DedupeKey: in.OrderID, Vars: vars,
	})
}

// OrderCreatedInput is what OrderCreated needs.
type OrderCreatedInput struct {
	UserID   string
	Email    string
	OrderID  string
	Number   string
	Total    string
	OrderURL string
}

// ServerCreated tells a customer their service is ready.
//
// It is also the activation message (SF-11): the per-product snippet an operator writes — how to
// log in to the panel they just bought, which port the game server listens on, where the DNS
// records go — arrives here as Instructions and is rendered as {{product_instructions}}. A
// product with no snippet gets DefaultActivationInstructions rather than a hole in the sentence,
// because an empty activation mail is the one message a customer is guaranteed to open.
//
// The snippet is data, not a template: it is substituted as a value in a single render pass, so
// a snippet that happens to contain "{{link}}" renders literally. An operator writing product
// copy must not be able to reach another variable by typing its name.
func ServerCreated(ctx context.Context, tx pgx.Tx, queue *Queue, in ServerCreatedInput) (Outcome, error) {
	vars := map[string]string{
		"service_name":         in.ServiceName,
		"server_name":          orDefault(in.ServerName, in.ServiceName),
		"product_instructions": orDefault(in.Instructions, DefaultActivationInstructions),
		"service_url":          orDefault(in.ServiceURL, "/portal/services/"+in.ServiceID),
	}
	return dispatch(ctx, tx, queue, EventNewServerCreated, Target{
		UserID: in.UserID, Email: in.Email, DedupeKey: in.ServiceID, Vars: vars,
	})
}

// DefaultActivationInstructions is what the activation message says when a product carries no
// snippet of its own.
//
// It is a sentence, not an empty string, and that is the whole point: the template is written by
// an operator and may place {{product_instructions}} mid-paragraph, where an empty value would
// read as a mistake. It also tells the customer where to go next, which is the one thing a
// generic activation mail can usefully say.
const DefaultActivationInstructions = "Sign in to your account for the access details."

// ServerCreatedInput is what ServerCreated needs.
type ServerCreatedInput struct {
	UserID      string
	Email       string
	ServiceID   string
	ServiceName string
	ServerName  string
	// Instructions is the product's activation snippet (products.email_template). Empty falls
	// back to DefaultActivationInstructions.
	Instructions string
	ServiceURL   string
}

// ServerSuspended tells a customer their service was suspended.
//
// The dedupe key includes the suspension time so a service suspended, restored and suspended
// again notifies twice, while a dunning run that re-evaluates the same suspension does not.
func ServerSuspended(ctx context.Context, tx pgx.Tx, queue *Queue, in ServerSuspendedInput) (Outcome, error) {
	vars := map[string]string{
		"service_name": in.ServiceName,
		"reason":       orDefault(in.Reason, "the service was suspended"),
		"suspended_at": formatWhen(in.SuspendedAt),
		"service_url":  orDefault(in.ServiceURL, "/portal/services/"+in.ServiceID),
	}
	return dispatch(ctx, tx, queue, EventServerSuspended, Target{
		UserID: in.UserID, Email: in.Email,
		DedupeKey: withAttempt(in.ServiceID, in.SuspendedAt), Vars: vars,
	})
}

// ServerSuspendedInput is what ServerSuspended needs.
type ServerSuspendedInput struct {
	UserID      string
	Email       string
	ServiceID   string
	ServiceName string
	Reason      string
	SuspendedAt time.Time
	ServiceURL  string
}

// ServerTerminated tells a customer their service was terminated.
func ServerTerminated(ctx context.Context, tx pgx.Tx, queue *Queue, in ServerTerminatedInput) (Outcome, error) {
	vars := map[string]string{
		"service_name":  in.ServiceName,
		"terminated_at": formatWhen(in.TerminatedAt),
		"service_url":   orDefault(in.ServiceURL, "/portal/services/"+in.ServiceID),
	}
	return dispatch(ctx, tx, queue, EventServerTerminated, Target{
		UserID: in.UserID, Email: in.Email, DedupeKey: in.ServiceID, Vars: vars,
	})
}

// ServerTerminatedInput is what ServerTerminated needs.
type ServerTerminatedInput struct {
	UserID       string
	Email        string
	ServiceID    string
	ServiceName  string
	TerminatedAt time.Time
	ServiceURL   string
}

// CancellationReceived acknowledges a cancellation request.
//
// It is worth sending even though the customer just asked for it: it is the record that the
// request arrived, and the thing they will look for when the service is still running next
// month.
func CancellationReceived(ctx context.Context, tx pgx.Tx, queue *Queue, in CancellationReceivedInput) (Outcome, error) {
	vars := map[string]string{
		"service_name": in.ServiceName,
		"cancels_at":   formatWhen(in.CancelsAt),
		"service_url":  orDefault(in.ServiceURL, "/portal/services/"+in.ServiceID),
	}
	return dispatch(ctx, tx, queue, EventServiceCancellationReceived, Target{
		UserID: in.UserID, Email: in.Email, DedupeKey: in.ServiceID, Vars: vars,
	})
}

// CancellationReceivedInput is what CancellationReceived needs.
type CancellationReceivedInput struct {
	UserID      string
	Email       string
	ServiceID   string
	ServiceName string
	CancelsAt   time.Time
	ServiceURL  string
}

// TicketMessage tells one party that a ticket has a new reply.
//
// MessageID is the dedupe key, which is what makes an inbound email piped twice produce one
// notification. The excerpt is truncated here rather than in the template so an
// operator-edited template cannot accidentally mail a customer the whole thread.
func TicketMessage(ctx context.Context, tx pgx.Tx, queue *Queue, in TicketMessageInput) (Outcome, error) {
	vars := map[string]string{
		"ticket_reference": in.Reference,
		"ticket_subject":   in.Subject,
		"author":           orDefault(in.Author, "Support"),
		"ticket_excerpt":   excerpt(in.Excerpt, 400),
		"ticket_url":       orDefault(in.TicketURL, "/tickets/"+in.TicketID),
	}
	return dispatch(ctx, tx, queue, EventNewTicketMessage, Target{
		UserID: in.UserID, Email: in.Email,
		DedupeKey: orDefault(in.MessageID, in.TicketID), Vars: vars,
	})
}

// TicketMessageInput is what TicketMessage needs.
type TicketMessageInput struct {
	// UserID is the recipient — the customer on a staff reply, or the assignee on a customer
	// reply. Picking the right one is the caller's job; this package does not know the
	// ticket's participants.
	UserID    string
	Email     string
	TicketID  string
	Reference string
	Subject   string
	Author    string
	Excerpt   string
	TicketURL string
	MessageID string
}

// NewLogin tells a customer their account was used from somewhere new.
//
// It is a choice event, not a force one: a customer who only ever signs in from one machine
// and finds the notice noise may turn it off, and the reset path is still there. The dedupe
// key is the auth event id, so a replayed login does not notify twice.
func NewLogin(ctx context.Context, tx pgx.Tx, queue *Queue, in NewLoginInput) (Outcome, error) {
	vars := map[string]string{
		"email":        in.Email,
		"login_at":     formatWhen(in.At),
		"ip_address":   orDefault(in.IP, "unknown"),
		"user_agent":   orDefault(in.UserAgent, "unknown"),
		"security_url": orDefault(in.SecurityURL, "/portal/profile"),
	}
	return dispatch(ctx, tx, queue, EventNewLoginDetected, Target{
		UserID: in.UserID, Email: in.Email, DedupeKey: in.AuthEventID, Vars: vars,
	})
}

// NewLoginInput is what NewLogin needs.
type NewLoginInput struct {
	UserID      string
	Email       string
	AuthEventID string
	At          time.Time
	IP          string
	UserAgent   string
	SecurityURL string
}

// ------------------------------------------------------------------ util --

// humanDuration renders an expiry the way a person reads it: "24 hours", not "24h0m0s".
func humanDuration(d time.Duration) string {
	if d <= 0 {
		return "a short time"
	}
	if d%(24*time.Hour) == 0 {
		n := int(d / (24 * time.Hour))
		if n == 1 {
			return "24 hours"
		}
		return fmt.Sprintf("%d days", n)
	}
	if d%time.Hour == 0 {
		return fmt.Sprintf("%d hours", int(d/time.Hour))
	}
	return fmt.Sprintf("%d minutes", int(d/time.Minute))
}

// withAttempt makes a dedupe subject out of a record id and the instant of the attempt, so
// that two genuine attempts are two notifications while a replay of one is one.
func withAttempt(id string, at time.Time) string {
	if at.IsZero() {
		return id
	}
	return id + ":" + at.UTC().Format(time.RFC3339)
}

// formatWhen renders a timestamp for a customer.
//
// UTC and a fixed layout, not time.Local: the worker and the web process may run in different
// zones, and a mail that says "due 3 March" in one and "due 2 March" in the other is a
// support ticket.
func formatWhen(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format("2 January 2006 15:04 MST")
}

// orDefault returns v, or fallback when v is blank.
func orDefault(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return v
}

// excerpt shortens text for a notification body.
//
// It cuts on a rune boundary, so a multi-byte character is never split into invalid UTF-8 —
// which would render as a replacement character in the customer's inbox.
func excerpt(s string, limit int) string {
	s = strings.TrimSpace(strings.Join(strings.Fields(s), " "))
	if limit <= 0 || len(s) <= limit {
		return s
	}
	cut := 0
	for i := range s {
		if i > limit {
			break
		}
		cut = i
	}
	return strings.TrimSpace(s[:cut]) + "…"
}

// companyName reads the operator's brand name, falling back when the setting is absent.
//
// A failure to read it is not an error: the message is more useful with the fallback than not
// sent at all, and a settings read cannot be the reason a customer is not told their payment
// failed.
func companyName(ctx context.Context, q Querier) string {
	var name string
	err := q.QueryRow(ctx,
		`SELECT value FROM settings WHERE group_key = 'general' AND key = 'company_name'`).Scan(&name)
	if err != nil || strings.TrimSpace(name) == "" {
		return "Catalyst Billing"
	}
	return name
}

// mailDisabledBySetting reports whether the operator has switched mail off for the whole
// installation (`mail.disable`).
//
// It is read inside the caller's transaction, next to the change it is about to describe, so a
// settings edit that commits after this read cannot half-apply to a message already queued. A
// missing row or an unparseable value is "not disabled": a settings read must never be the
// reason a customer is not told their password was reset.
func mailDisabledBySetting(ctx context.Context, q Querier) bool {
	var v string
	if err := q.QueryRow(ctx,
		`SELECT value FROM settings WHERE group_key = 'mail' AND key = 'disable'`).Scan(&v); err != nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "true", "t", "1", "yes", "on":
		return true
	}
	return false
}

// publicURL is the origin a link in a mail must point at.
//
// Read from the environment rather than from the request, for the same reason
// publicBaseURL in httpapi is: a link built from the Host header is a link an attacker
// chooses by sending a forged Host.
func publicURL() string {
	if v := strings.TrimSpace(os.Getenv("BILLING_PUBLIC_URL")); v != "" {
		return strings.TrimRight(v, "/")
	}
	return "http://127.0.0.1:8080"
}
