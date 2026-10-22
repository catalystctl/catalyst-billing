package notify

// The template variable catalogue, and the check that refuses a template the renderer
// cannot finish.
//
// The renderer leaves an unknown placeholder in the output as literal text (see Render).
// That is the right behaviour at *send* time — a message with a visible "{{invoice_number}}"
// tells the operator something is wrong, where an empty string would quietly produce a
// sentence with a hole in it — but it is the wrong behaviour at *save* time. A template that
// no event can complete is a message that goes to a customer with the plumbing showing, and
// the operator who typed the typo is the only person who could have been told in time.
//
// So the vocabulary each event supplies is written down here, next to the events themselves
// (events.go builds these maps), and the staff editor checks a draft against it before it is
// stored. Three properties are deliberate:
//
//   - The catalogue is *per event*, not one global set. "total" is a real variable for an
//     invoice and a meaningless one for a password reset; a global set would let an operator
//     write a reset mail that renders "{{total}}" and store it.
//   - A variable a *caller* supplies (a product's activation snippet, the operator's company
//     name) is still listed, because the template may use it and the operator cannot see
//     events.go from the panel.
//   - The sample values are shaped like the real ones — the same date layout formatWhen
//     produces, a number like a real invoice number — so the preview shows what a customer
//     will actually read rather than a row of "example".

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// ErrUnknownPlaceholder is returned when a template uses a placeholder its event does not
// supply. The staff handler turns it into 422 with a code the editor can explain.
var ErrUnknownPlaceholder = errors.New("notify: a template uses a placeholder no event supplies")

// TemplateVariable is one placeholder an event supplies to its template.
type TemplateVariable struct {
	// Name is the word between the braces: "invoice_number".
	Name string
	// Description says what the value is, for the operator reading the editor.
	Description string
	// Sample is a realistic value, used to render the preview.
	Sample string
}

// sampleCompanyName is what the preview uses before the operator's own brand name is read
// from settings. It matches the fallback companyName() applies at send time, so the preview
// and the mail cannot disagree about the default.
const sampleCompanyName = "Catalyst Billing"

// companyNameVariable is appended to every event's vocabulary, because dispatch() supplies
// it to every template.
var companyNameVariable = TemplateVariable{
	Name:        "company_name",
	Description: "The company name from the general settings.",
	Sample:      sampleCompanyName,
}

// eventVariables is the vocabulary of every event in the catalogue.
//
// It mirrors the maps the event functions in events.go build, and the test that drives every
// event with sample data proves the two cannot drift: a variable an event supplies but this
// table omits would make a valid template unsaveable, and one this table lists but the event
// never supplies would render a placeholder to a customer.
var eventVariables = map[string][]TemplateVariable{
	EventPasswordReset: {
		{Name: "email", Description: "The address the reset was requested for.", Sample: "ada@example.test"},
		{Name: "link", Description: "The one-time reset URL.", Sample: "/reset?token=4f9c2a1b"},
		{Name: "expires_in", Description: "How long the link stays valid.", Sample: "24 hours"},
	},
	EventEmailVerification: {
		{Name: "email", Description: "The address being confirmed.", Sample: "ada@example.test"},
		{Name: "link", Description: "The one-time confirmation URL.", Sample: "/verify?token=4f9c2a1b"},
		{Name: "expires_in", Description: "How long the link stays valid.", Sample: "24 hours"},
	},
	EventNewLoginDetected: {
		{Name: "email", Description: "The account that was signed in to.", Sample: "ada@example.test"},
		{Name: "login_at", Description: "When the sign-in happened.", Sample: "3 March 2026 14:20 UTC"},
		{Name: "ip_address", Description: "The address the sign-in came from.", Sample: "203.0.113.9"},
		{Name: "user_agent", Description: "The browser or client reported by the sign-in.", Sample: "Firefox on Linux"},
		{Name: "security_url", Description: "Where the customer changes their password.", Sample: "/portal/profile"},
	},
	EventNewInvoiceCreated: {
		{Name: "invoice_number", Description: "The invoice's number.", Sample: "INV-2026-0042"},
		{Name: "customer_name", Description: "The customer the invoice was issued to.", Sample: "Ada Lovelace"},
		{Name: "total", Description: "The amount due, already formatted.", Sample: "USD 49.00"},
		{Name: "due_at", Description: "The due date.", Sample: "17 March 2026 00:00 UTC"},
		{Name: "invoice_url", Description: "The customer's view of the invoice.", Sample: "/portal/invoices/2b1f"},
	},
	EventInvoicePaid: {
		{Name: "invoice_number", Description: "The invoice that was paid.", Sample: "INV-2026-0042"},
		{Name: "total", Description: "The amount paid, already formatted.", Sample: "USD 49.00"},
		{Name: "paid_at", Description: "When the payment was applied.", Sample: "3 March 2026 14:20 UTC"},
		{Name: "invoice_url", Description: "The customer's view of the invoice.", Sample: "/portal/invoices/2b1f"},
	},
	EventInvoicePaymentFailed: {
		{Name: "invoice_number", Description: "The invoice the attempt was for.", Sample: "INV-2026-0042"},
		{Name: "total", Description: "The amount the attempt was for.", Sample: "USD 49.00"},
		{Name: "attempted_at", Description: "When the attempt was made.", Sample: "3 March 2026 14:20 UTC"},
		{Name: "failure_reason", Description: "The gateway's reason, as text.", Sample: "the card was declined"},
		{Name: "invoice_url", Description: "Where the customer retries the payment.", Sample: "/portal/invoices/2b1f"},
	},
	EventNewOrderCreated: {
		{Name: "order_number", Description: "The order's number.", Sample: "ORD-2026-0011"},
		{Name: "total", Description: "The order total, already formatted.", Sample: "USD 49.00"},
		{Name: "order_url", Description: "Where the customer tracks the order.", Sample: "/portal"},
	},
	EventNewServerCreated: {
		{Name: "service_name", Description: "The service, as the customer knows it.", Sample: "web-01"},
		{Name: "server_name", Description: "The server's hostname, or the service name.", Sample: "web-01.example.test"},
		{Name: "product_instructions", Description: "The product's activation snippet, or a default sentence.", Sample: DefaultActivationInstructions},
		{Name: "service_url", Description: "The customer's view of the service.", Sample: "/portal/services/8c2a"},
	},
	EventServerSuspended: {
		{Name: "service_name", Description: "The service that was suspended.", Sample: "web-01"},
		{Name: "reason", Description: "Why it was suspended.", Sample: "the invoice is overdue"},
		{Name: "suspended_at", Description: "When the suspension happened.", Sample: "3 March 2026 14:20 UTC"},
		{Name: "service_url", Description: "The customer's view of the service.", Sample: "/portal/services/8c2a"},
	},
	EventServerTerminated: {
		{Name: "service_name", Description: "The service that was terminated.", Sample: "web-01"},
		{Name: "terminated_at", Description: "When the termination happened.", Sample: "3 March 2026 14:20 UTC"},
		{Name: "service_url", Description: "The customer's view of the service.", Sample: "/portal/services/8c2a"},
	},
	EventServiceCancellationReceived: {
		{Name: "service_name", Description: "The service being cancelled.", Sample: "web-01"},
		{Name: "cancels_at", Description: "When the service ends.", Sample: "1 April 2026 00:00 UTC"},
		{Name: "service_url", Description: "The customer's view of the service.", Sample: "/portal/services/8c2a"},
	},
	EventNewTicketMessage: {
		{Name: "ticket_reference", Description: "The ticket's reference.", Sample: "TIC-0042"},
		{Name: "ticket_subject", Description: "The ticket's subject.", Sample: "Invoice question"},
		{Name: "author", Description: "Who wrote the message.", Sample: "Support"},
		{Name: "ticket_excerpt", Description: "The first 400 characters of the message.", Sample: "Thanks for getting in touch — here is the breakdown."},
		{Name: "ticket_url", Description: "Where the reader replies.", Sample: "/tickets/5d7e"},
	},
}

// TemplateVariables returns the placeholders the named event supplies, in a stable order.
//
// The list includes company_name, which every template may use, and it is returned for an
// event even when the catalogue is asked before any template row exists: the vocabulary is a
// property of the event, not of the installation's data.
func TemplateVariables(eventKey string) ([]TemplateVariable, error) {
	if !IsKnownEvent(eventKey) {
		return nil, fmt.Errorf("%w: %s", ErrUnknownEvent, eventKey)
	}
	own := eventVariables[eventKey]
	out := make([]TemplateVariable, 0, len(own)+1)
	out = append(out, own...)
	out = append(out, companyNameVariable)
	return out, nil
}

// SampleVariables returns the preview's data for one event, keyed the way a template uses it.
func SampleVariables(eventKey string) (map[string]string, error) {
	variables, err := TemplateVariables(eventKey)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(variables))
	for _, v := range variables {
		out[v.Name] = v.Sample
	}
	return out, nil
}

// Placeholders returns the placeholder names tmpl uses, in the order they appear.
//
// It walks the template with the same rules substitute() applies when it renders — a "{{"
// with no closing "}}" is literal text, and so is "{{}}", which is how a template can talk
// about braces at all — so the validator cannot refuse text the renderer would have left
// alone, and cannot miss a placeholder it would have expanded.
func Placeholders(tmpl string) []string {
	if !strings.Contains(tmpl, "{{") {
		return nil
	}
	var out []string
	rest := tmpl
	for {
		open := strings.Index(rest, "{{")
		if open < 0 {
			return out
		}
		closeRel := strings.Index(rest[open+2:], "}}")
		if closeRel < 0 {
			return out
		}
		end := open + 2 + closeRel
		key := strings.TrimSpace(rest[open+2 : end])
		if key != "" && !strings.ContainsAny(key, "{}") {
			out = append(out, key)
		}
		rest = rest[end+2:]
	}
}

// UnknownPlaceholders returns the placeholders in t that its event does not supply, sorted
// and de-duplicated. An unknown event reports nothing here; the handler refuses the key
// before this is reached, and inventing "every placeholder is unknown" for a template whose
// event does not exist would be a misleading answer.
func UnknownPlaceholders(t Template) []string {
	known := map[string]struct{}{}
	if variables, err := TemplateVariables(t.EventKey); err == nil {
		for _, v := range variables {
			known[v.Name] = struct{}{}
		}
	} else {
		return nil
	}

	seen := map[string]struct{}{}
	var out []string
	for _, field := range []string{t.Subject, t.Body, t.InAppTitle, t.InAppBody, t.InAppURL} {
		for _, name := range Placeholders(field) {
			if _, ok := known[name]; ok {
				continue
			}
			if _, dup := seen[name]; dup {
				continue
			}
			seen[name] = struct{}{}
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// ValidateTemplate refuses a template whose placeholders the renderer does not know.
//
// It is called by SaveTemplate, so every write path — the staff editor, the reset action, a
// test — is checked, and the refusal happens where the wording is stored rather than at the
// moment a customer's payment fails to produce a mail.
func ValidateTemplate(t Template) error {
	unknown := UnknownPlaceholders(t)
	if len(unknown) == 0 {
		return nil
	}
	quoted := make([]string, 0, len(unknown))
	for _, name := range unknown {
		quoted = append(quoted, "{{"+name+"}}")
	}
	return fmt.Errorf("%w: %s does not supply %s", ErrUnknownPlaceholder, t.EventKey,
		strings.Join(quoted, ", "))
}

// Preview is a draft template rendered for the operator.
//
// The two channels are rendered separately because they are rendered separately when the
// event is actually sent: the mail body gets absolute links (a customer clicking a relative
// link in a mailbox clicks nothing), while the inbox row keeps panel-relative paths (an
// absolute URL in the panel breaks a deployment behind a proxy). A preview that showed one
// form for both would be showing a lie about one of them.
type Preview struct {
	Mail  Rendered
	InApp Rendered
}

// PreviewTemplate renders a draft template with the sample data for its event.
//
// The draft is not stored: this is what the editor calls on every keystroke that matters, so
// an operator sees the sentence before it is saved. It validates first, because rendering a
// template with a placeholder the event does not supply is exactly the state this function
// exists to keep out of the customer's inbox.
func PreviewTemplate(ctx context.Context, q Querier, t Template) (Preview, error) {
	if !IsKnownEvent(t.EventKey) {
		return Preview{}, fmt.Errorf("%w: %s", ErrUnknownEvent, t.EventKey)
	}
	if err := ValidateTemplate(t); err != nil {
		return Preview{}, err
	}
	vars, err := SampleVariables(t.EventKey)
	if err != nil {
		return Preview{}, err
	}
	// The operator's own brand name, so the preview shows their company rather than ours. A
	// settings read that fails falls back rather than failing the preview: the operator is
	// editing wording, and the wording is still worth seeing.
	if q != nil {
		vars["company_name"] = companyName(ctx, q)
	}
	return Preview{
		Mail:  Render(t, absoluteLinks(vars)),
		InApp: Render(t, vars),
	}, nil
}
