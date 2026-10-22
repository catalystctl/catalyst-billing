package notify_test

// The template vocabulary and the save-time check.
//
// The properties that matter here are the ones a customer would notice:
//
//   - every placeholder an event supplies is in the catalogue, so a valid template can be saved
//     (a missing entry would make an operator's correct edit impossible to store);
//   - no placeholder outside the catalogue can be stored, so a customer never reads
//     "{{invoice_nuber}}";
//   - the preview renders the draft with the same rules the send path uses, including the
//     absolute links a mail needs and the panel-relative links an inbox row needs.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/catalyst/billing/internal/notify"
	"github.com/catalyst/billing/internal/store/storetest"
)

// The placeholder scanner agrees with the renderer on the awkward inputs.
//
// A validator that disagreed with the renderer in either direction would be a bug an operator
// could not work around: refusing text the renderer would have left alone makes a legitimate
// template unsaveable, and missing a placeholder the renderer expands lets a typo through.
func TestPlaceholdersMatchTheRenderer(t *testing.T) {
	cases := []struct {
		name   string
		tmpl   string
		expect []string
	}{
		{"plain", "no placeholders here", nil},
		{"one", "Invoice {{invoice_number}}", []string{"invoice_number"}},
		{"two", "{{a}} and {{b}}", []string{"a", "b"}},
		{"spaces inside", "{{ spaced }}", []string{"spaced"}},
		{"repeated", "{{a}} {{a}}", []string{"a", "a"}},
		{"unclosed", "costs {{ 5", nil},
		{"empty", "a {{}} b", nil},
		{"nested braces", "{{ a {{b}} c }}", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := notify.Placeholders(tc.tmpl)
			if len(got) != len(tc.expect) {
				t.Fatalf("Placeholders(%q) = %v, want %v", tc.tmpl, got, tc.expect)
			}
			for i := range got {
				if got[i] != tc.expect[i] {
					t.Fatalf("Placeholders(%q) = %v, want %v", tc.tmpl, got, tc.expect)
				}
			}
			// Every name the scanner reports is one the renderer would have expanded: rendered
			// with no data at all, the placeholder must still be visible in the output.
			rendered := notify.Render(notify.Template{EventKey: notify.EventInvoicePaid, Body: tc.tmpl}, nil)
			for _, name := range got {
				if !strings.Contains(rendered.Body, "{{"+name+"}}") {
					t.Fatalf("the renderer did not treat %q as a placeholder: %q", name, rendered.Body)
				}
			}
			// And text the scanner does not report is left untouched by the renderer.
			for _, literal := range []string{"{{ 5", "{{}}", "{{ a {{b}} c }}"} {
				if strings.Contains(tc.tmpl, literal) &&
					notify.Render(notify.Template{Body: literal}, nil).Body != literal {
					t.Fatalf("the renderer rewrote literal text %q", literal)
				}
			}
		})
	}
}

// Every event's sample data fills every placeholder the event documents, so the preview never
// shows a hole and the editor's "insert placeholder" chips are all usable.
func TestEveryEventSampleDataFillsItsVocabulary(t *testing.T) {
	for _, event := range notify.Catalogue() {
		t.Run(event.Key, func(t *testing.T) {
			variables, err := notify.TemplateVariables(event.Key)
			if err != nil {
				t.Fatalf("variables: %v", err)
			}
			if len(variables) == 0 {
				t.Fatalf("event %q has no vocabulary", event.Key)
			}
			var body strings.Builder
			for _, v := range variables {
				if v.Sample == "" {
					t.Fatalf("variable %q has no sample value", v.Name)
				}
				body.WriteString("[" + "{{" + v.Name + "}}" + "] ")
			}
			samples, err := notify.SampleVariables(event.Key)
			if err != nil {
				t.Fatalf("samples: %v", err)
			}
			rendered := notify.Render(notify.Template{
				EventKey: event.Key, Subject: "subject", Body: body.String(),
			}, samples)
			if strings.Contains(rendered.Body, "{{") {
				t.Fatalf("sample data left a placeholder unrendered: %q", rendered.Body)
			}
		})
	}
}

// The shipped default for every event is storable, which is the other half of the same
// property: a default that used a placeholder outside the catalogue would be a template the
// installation could send but not save.
func TestEveryDefaultTemplatePassesValidation(t *testing.T) {
	for _, tmpl := range notify.DefaultTemplates() {
		if err := notify.ValidateTemplate(tmpl); err != nil {
			t.Fatalf("default for %q is invalid: %v", tmpl.EventKey, err)
		}
	}
}

// A typo is refused at save time, in the store, so every write path is covered — not only the
// screen that remembered to ask.
func TestSaveTemplateRefusesAnUnknownPlaceholder(t *testing.T) {
	pool := storetest.DB(t)
	ctx := context.Background()
	seedTemplates(t, pool)

	before, err := notify.TemplateFor(ctx, pool, notify.EventInvoicePaid)
	if err != nil {
		t.Fatalf("read template: %v", err)
	}

	_, err = notify.SaveTemplate(ctx, pool, notify.Template{
		EventKey:     notify.EventInvoicePaid,
		Name:         before.Name,
		Subject:      "Invoice {{invoice_nuber}}",
		Body:         before.Body,
		InAppTitle:   before.InAppTitle,
		InAppBody:    before.InAppBody,
		InAppURL:     before.InAppURL,
		Enabled:      true,
		MailEnabled:  true,
		InAppEnabled: true,
	})
	if !errors.Is(err, notify.ErrUnknownPlaceholder) {
		t.Fatalf("save error = %v, want ErrUnknownPlaceholder", err)
	}
	// The refusal names the placeholder, the field it was written in, and the vocabulary the
	// event does supply — an operator who typed a plausible word needs the real ones in front of
	// them, not a rule to go and look up.
	for _, want := range []string{"{{invoice_nuber}}", "(subject)", "{{invoice_number}}", "{{total}}"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the refusal does not contain %q: %v", want, err)
		}
	}

	// Nothing was written: the stored row still renders.
	after, err := notify.TemplateFor(ctx, pool, notify.EventInvoicePaid)
	if err != nil {
		t.Fatalf("re-read template: %v", err)
	}
	if after.Subject != before.Subject {
		t.Fatalf("a refused save changed the stored subject: %q", after.Subject)
	}
	if strings.Contains(after.Subject, "invoice_nuber") {
		t.Fatalf("the typo reached the stored template: %q", after.Subject)
	}
}

// A placeholder in the *subject* is the same bug as one in the body, and it is the easier one
// to miss: the subject is one line, and it is the line a customer reads before opening anything.
func TestSaveTemplateRefusesAnUnknownPlaceholderInTheSubject(t *testing.T) {
	pool := storetest.DB(t)
	ctx := context.Background()
	seedTemplates(t, pool)

	before, err := notify.TemplateFor(ctx, pool, notify.EventPasswordReset)
	if err != nil {
		t.Fatalf("read template: %v", err)
	}

	// The body is valid; only the subject carries a word the event does not supply.
	_, err = notify.SaveTemplate(ctx, pool, notify.Template{
		EventKey:     notify.EventPasswordReset,
		Name:         before.Name,
		Subject:      "Reset the password for {{account_email}}",
		Body:         before.Body,
		InAppTitle:   before.InAppTitle,
		InAppBody:    before.InAppBody,
		InAppURL:     before.InAppURL,
		Enabled:      true,
		MailEnabled:  true,
		InAppEnabled: true,
	})
	if !errors.Is(err, notify.ErrUnknownPlaceholder) {
		t.Fatalf("save error = %v, want ErrUnknownPlaceholder", err)
	}
	if !strings.Contains(err.Error(), "(subject)") {
		t.Fatalf("the refusal does not say which field: %v", err)
	}
	if !strings.Contains(err.Error(), "{{email}}") {
		t.Fatalf("the refusal does not list the placeholders the event supplies: %v", err)
	}

	after, err := notify.TemplateFor(ctx, pool, notify.EventPasswordReset)
	if err != nil {
		t.Fatalf("re-read template: %v", err)
	}
	if after.Subject != before.Subject {
		t.Fatalf("a refused save changed the stored subject: %q", after.Subject)
	}
}

// The preview renders both channels with their own link forms, and refuses the same drafts the
// save path refuses.
func TestPreviewRendersBothChannels(t *testing.T) {
	pool := storetest.DB(t)
	ctx := context.Background()

	draft := notify.Template{
		EventKey:   notify.EventInvoicePaid,
		Subject:    "Payment received for {{invoice_number}}",
		Body:       "Thank you. See {{invoice_url}}.\n\n{{company_name}}",
		InAppTitle: "Paid {{invoice_number}}",
		InAppBody:  "{{total}} received.",
		InAppURL:   "{{invoice_url}}",
	}
	preview, err := notify.PreviewTemplate(ctx, pool, draft)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if !strings.Contains(preview.Mail.Subject, "INV-") {
		t.Fatalf("the subject was not rendered: %q", preview.Mail.Subject)
	}
	// The mail carries an absolute link: a relative link in a mailbox is a link nobody can
	// click.
	if !strings.Contains(preview.Mail.Body, "http") {
		t.Fatalf("the mail body has no absolute link: %q", preview.Mail.Body)
	}
	// The inbox row keeps the panel-relative path, which is what the panel can resolve.
	if !strings.HasPrefix(preview.InApp.InAppURL, "/") {
		t.Fatalf("the inbox url = %q, want a panel path", preview.InApp.InAppURL)
	}

	// The preview is the renderer, not an approximation of it: the same draft rendered by hand
	// with the same sample data produces the same sentences.
	samples, err := notify.SampleVariables(notify.EventInvoicePaid)
	if err != nil {
		t.Fatalf("samples: %v", err)
	}
	if samples["company_name"] == "" || samples["invoice_number"] == "" {
		t.Fatal("the sample data is incomplete")
	}
	if !strings.Contains(preview.Mail.Subject, samples["invoice_number"]) {
		t.Fatalf("the subject does not carry the invoice number: %q", preview.Mail.Subject)
	}
	// The absolute form of the same link, which is what a mail must carry.
	//
	// Asserted as a property, not against a literal base URL. The base comes from
	// BILLING_PUBLIC_URL, so hardcoding one makes this pass or fail on the deployment's
	// configuration rather than on behaviour — and `http://` specifically is wrong, because a mail
	// linking to an https origin is more correct than one that does not. What matters is that the
	// link is absolute at all: a relative path in an email is unclickable, which is the defect.
	if !strings.Contains(preview.Mail.Body, "://") || !strings.Contains(preview.Mail.Body, samples["invoice_url"]) {
		t.Fatalf("the mail body does not carry an absolute invoice link: %q", preview.Mail.Body)
	}
	// The inbox row is rendered from the same data, with the relative link.
	if preview.InApp.InAppTitle != "Paid "+samples["invoice_number"] {
		t.Fatalf("inbox title = %q", preview.InApp.InAppTitle)
	}
	if preview.InApp.InAppBody != samples["total"]+" received." {
		t.Fatalf("inbox body = %q", preview.InApp.InAppBody)
	}

	if _, err := notify.PreviewTemplate(ctx, pool, notify.Template{
		EventKey: notify.EventInvoicePaid,
		Subject:  "{{nope}}",
		Body:     "body",
	}); !errors.Is(err, notify.ErrUnknownPlaceholder) {
		t.Fatalf("preview error = %v, want ErrUnknownPlaceholder", err)
	}
}
