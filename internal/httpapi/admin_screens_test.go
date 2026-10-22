package httpapi_test

// The three operator screens that had an API but no route to a human: the notification
// template editor, the automation list, and the service-property catalogue.
//
// These tests assert the properties the screens exist for, not the happy path:
//
//   - a template edit round-trips, and the preview renders the *saved* wording rather than
//     something the client guessed;
//   - a placeholder the renderer does not know is refused, so a customer never reads
//     "{{invoice_nuber}}";
//   - the automation list matches the jobs the worker actually registers — compared against
//     lifecycle.Jobs() and against the registration in cmd/billing/jobs.go, so a new job
//     cannot be invisible on the screen;
//   - a property definition round-trips with both visibility flags, a reorder round-trips, and
//     a definition a service carries cannot be deleted.

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/catalyst/billing/internal/lifecycle"
	"github.com/catalyst/billing/internal/notify"
)

// --------------------------------------------------------------- templates --

// templatePreview is the part of the preview response these tests read.
type templatePreview struct {
	EventKey string `json:"event_key"`
	Force    bool   `json:"force"`
	Mail     struct {
		Subject string `json:"subject"`
		Body    string `json:"body"`
	} `json:"mail"`
	InApp struct {
		Title string `json:"title"`
		Body  string `json:"body"`
		URL   string `json:"url"`
	} `json:"in_app"`
	Variables []struct {
		Name   string `json:"name"`
		Sample string `json:"sample"`
	} `json:"variables"`
}

func decodePreview(t *testing.T, body []byte) templatePreview {
	t.Helper()
	var out templatePreview
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("preview body: %v (%s)", err, body)
	}
	return out
}

// A template edit round-trips, and the preview of the saved wording is the saved wording
// rendered with the event's sample data.
func TestNotificationTemplateEditRoundTripsAndPreviews(t *testing.T) {
	env := newStaffEnv(t)

	edit := map[string]any{
		"subject":        "Payment received for {{invoice_number}}",
		"body":           "Thank you. Invoice {{invoice_number}} is settled.\n\nSee {{invoice_url}}.",
		"in_app_title":   "Paid {{invoice_number}}",
		"in_app_body":    "{{total}} received.",
		"in_app_url":     "{{invoice_url}}",
		"enabled":        true,
		"mail_enabled":   true,
		"in_app_enabled": true,
	}
	rec := env.doJSON(t, http.MethodPut, "/api/v1/staff/notification-templates/invoice_paid",
		env.billing, env.billingCSRF, edit)
	if rec.Code != http.StatusOK {
		t.Fatalf("save: status %d; body %s", rec.Code, rec.Body)
	}
	var saved struct {
		Subject string `json:"subject"`
		Body    string `json:"body"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &saved); err != nil {
		t.Fatalf("save body: %v", err)
	}
	if saved.Subject != edit["subject"] || saved.Body != edit["body"] {
		t.Fatalf("the edit did not round-trip: %+v", saved)
	}

	// Read it back through the list, which is what the screen renders.
	rec = env.get(t, "/api/v1/staff/notification-templates", env.billing)
	if rec.Code != http.StatusOK {
		t.Fatalf("list: status %d; body %s", rec.Code, rec.Body)
	}
	var list struct {
		Data []struct {
			EventKey string `json:"event_key"`
			Subject  string `json:"subject"`
			Force    bool   `json:"force"`
			Seeded   bool   `json:"seeded"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("list body: %v", err)
	}
	var found bool
	for _, row := range list.Data {
		if row.EventKey != "invoice_paid" {
			continue
		}
		found = true
		if row.Subject != edit["subject"] {
			t.Fatalf("list subject = %q, want the saved one", row.Subject)
		}
		// invoice_paid is an optional event: a customer may turn it off. The screen uses this
		// flag to say so, because it decides whether an edit is safe.
		if row.Force {
			t.Fatal("invoice_paid is reported as transactional")
		}
	}
	if !found {
		t.Fatal("invoice_paid is missing from the template list")
	}

	// Preview the draft that is now stored. The rendered subject must be the stored subject with
	// the sample data substituted — that is the whole point of the preview.
	samples, err := notify.SampleVariables("invoice_paid")
	if err != nil {
		t.Fatalf("samples: %v", err)
	}
	rec = env.doJSON(t, http.MethodPost,
		"/api/v1/staff/notification-templates/invoice_paid/preview",
		env.billing, env.billingCSRF, edit)
	if rec.Code != http.StatusOK {
		t.Fatalf("preview: status %d; body %s", rec.Code, rec.Body)
	}
	preview := decodePreview(t, rec.Body.Bytes())
	wantSubject := "Payment received for " + samples["invoice_number"]
	if preview.Mail.Subject != wantSubject {
		t.Fatalf("preview subject = %q, want %q", preview.Mail.Subject, wantSubject)
	}
	// The mail gets the ABSOLUTE form of the panel path; the inbox row keeps the relative one.
	//
	// Asserted as a property rather than against a literal base URL. The base comes from
	// BILLING_PUBLIC_URL, so hardcoding one makes the test pass or fail on the deployment's
	// configuration rather than on the behaviour — and asserting `http://` specifically is wrong
	// outright, because a mail that links to an https origin is more correct than one that does
	// not. What matters is that the link is absolute at all: a relative path in an email is
	// unclickable, which is the defect this is protecting against.
	if !strings.Contains(preview.Mail.Body, "://") || !strings.Contains(preview.Mail.Body, samples["invoice_url"]) {
		t.Fatalf("the mail preview has no absolute link: %q", preview.Mail.Body)
	}
	if preview.InApp.URL != samples["invoice_url"] {
		t.Fatalf("inbox url = %q, want %q", preview.InApp.URL, samples["invoice_url"])
	}
	if len(preview.Variables) == 0 {
		t.Fatal("the preview carries no vocabulary, so the editor cannot offer any placeholders")
	}
	if preview.Force {
		t.Fatal("the preview reports invoice_paid as transactional")
	}
}

// A placeholder the event does not supply is refused, both at save time and in the preview, and
// nothing is written.
func TestNotificationTemplateRefusesAnUnknownPlaceholder(t *testing.T) {
	env := newStaffEnv(t)

	rec := env.get(t, "/api/v1/staff/notification-templates/password_reset", env.billing)
	if rec.Code != http.StatusOK {
		t.Fatalf("read: status %d; body %s", rec.Code, rec.Body)
	}
	var before struct {
		Subject string `json:"subject"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &before); err != nil {
		t.Fatalf("read body: %v", err)
	}

	bad := map[string]any{
		"subject": "Reset the password for {{email}}",
		"body":    "Use this link: {{reset_link}}",
	}
	rec = env.doJSON(t, http.MethodPut, "/api/v1/staff/notification-templates/password_reset",
		env.billing, env.billingCSRF, bad)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("save with an unknown placeholder: status %d, want 422; body %s", rec.Code, rec.Body)
	}
	if p := decodeProblem(t, rec); p.Code != "unknown_placeholder" {
		t.Fatalf("code = %q, want unknown_placeholder", p.Code)
	}
	// The refusal names the placeholder, the field it was written in, and the vocabulary the
	// event does supply. "Invalid placeholder" alone would make the operator hunt for the
	// mistake and then guess at the right spelling.
	for _, want := range []string{"{{reset_link}}", "(body)", "{{email}}", "{{link}}"} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("the refusal does not contain %q: %s", want, rec.Body)
		}
	}

	// A placeholder in the subject is the same bug and the easier one to miss: it is one line,
	// and it is the line a customer reads before opening the message.
	rec = env.doJSON(t, http.MethodPut, "/api/v1/staff/notification-templates/password_reset",
		env.billing, env.billingCSRF, map[string]any{
			"subject": "Reset the password for {{account_email}}",
			"body":    "Use this link: {{link}}",
		})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("save with an unknown placeholder in the subject: status %d, want 422; body %s",
			rec.Code, rec.Body)
	}
	if p := decodeProblem(t, rec); p.Code != "unknown_placeholder" {
		t.Fatalf("subject code = %q, want unknown_placeholder", p.Code)
	}
	if !strings.Contains(rec.Body.String(), "(subject)") {
		t.Fatalf("the refusal does not say the subject is where the mistake is: %s", rec.Body)
	}

	// Nothing was stored: the row still has its original wording.
	rec = env.get(t, "/api/v1/staff/notification-templates/password_reset", env.billing)
	if rec.Code != http.StatusOK {
		t.Fatalf("re-read: status %d; body %s", rec.Code, rec.Body)
	}
	var after struct {
		Subject string `json:"subject"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &after); err != nil {
		t.Fatalf("re-read body: %v", err)
	}
	if after.Subject != before.Subject {
		t.Fatalf("a refused save changed the template: %q -> %q", before.Subject, after.Subject)
	}

	// The preview refuses the same draft, so the editor can say why before the operator clicks
	// save rather than after.
	rec = env.doJSON(t, http.MethodPost,
		"/api/v1/staff/notification-templates/password_reset/preview",
		env.billing, env.billingCSRF, bad)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("preview with an unknown placeholder: status %d, want 422; body %s", rec.Code, rec.Body)
	}
	if p := decodeProblem(t, rec); p.Code != "unknown_placeholder" {
		t.Fatalf("preview code = %q, want unknown_placeholder", p.Code)
	}
}

// The vocabulary route serves every event, and support cannot reach it: support answers "why
// did I get this", it does not decide what the mail says.
func TestNotificationTemplateEditorIsForTemplateEditors(t *testing.T) {
	env := newStaffEnv(t)

	rec := env.get(t, "/api/v1/staff/notification-template-variables", env.billing)
	if rec.Code != http.StatusOK {
		t.Fatalf("variables: status %d; body %s", rec.Code, rec.Body)
	}
	var body struct {
		Data []struct {
			EventKey  string `json:"event_key"`
			Force     bool   `json:"force"`
			Variables []struct {
				Name string `json:"name"`
			} `json:"variables"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("variables body: %v", err)
	}
	if len(body.Data) != len(notify.Catalogue()) {
		t.Fatalf("%d events described, %d in the catalogue", len(body.Data), len(notify.Catalogue()))
	}
	// The transactional flag decides whether an edit is safe, so it has to be there.
	var sawForce bool
	for _, event := range body.Data {
		if event.Force {
			sawForce = true
		}
		if len(event.Variables) == 0 {
			t.Fatalf("event %q offers no placeholders", event.EventKey)
		}
	}
	if !sawForce {
		t.Fatal("no event is marked transactional; password reset cannot be opted out of")
	}

	rec = env.get(t, "/api/v1/staff/notification-template-variables", env.support)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("support reading the vocabulary: status %d, want 403", rec.Code)
	}
	problemCode(t, rec)

	rec = env.doJSON(t, http.MethodPost,
		"/api/v1/staff/notification-templates/invoice_paid/preview",
		env.support, env.supportCSRF, map[string]any{"subject": "x", "body": "y"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("support previewing a template: status %d, want 403", rec.Code)
	}
}

// -------------------------------------------------------------- automation --

// automationRow is one job as the automation screen reads it.
type automationRow struct {
	Name            string   `json:"name"`
	IntervalSeconds int64    `json:"interval_seconds"`
	Interval        string   `json:"interval"`
	Summary         string   `json:"summary"`
	Disabled        bool     `json:"disabled"`
	KillSwitch      *string  `json:"kill_switch"`
	KillSwitchSet   bool     `json:"kill_switch_set"`
	SwitchEffect    string   `json:"switch_effect"`
	Settings        []string `json:"settings"`
	Source          string   `json:"source"`
}

// automationResponse is the shape the automation screen reads.
type automationResponse struct {
	Data         []automationRow `json:"data"`
	KillSwitches struct {
		Mechanism string `json:"mechanism"`
		ReadOnly  bool   `json:"read_only"`
		Note      string `json:"note"`
	} `json:"kill_switches"`
	Worker struct {
		EmbeddedSwitch string `json:"embedded_switch"`
		Embedded       bool   `json:"embedded"`
	} `json:"worker"`
}

func fetchAutomation(t *testing.T, env *staffEnv) automationResponse {
	t.Helper()
	rec := env.get(t, "/api/v1/staff/automation", env.admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("automation: status %d; body %s", rec.Code, rec.Body)
	}
	var body automationResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("automation body: %v (%s)", err, rec.Body)
	}
	return body
}

// repoFile reads a file from the repository root, so a test can compare the screen against the
// code that registers the jobs.
func repoFile(t *testing.T, rel string) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for i := 0; i < 6; i++ {
		candidate := filepath.Join(dir, rel)
		if _, err := os.Stat(candidate); err == nil {
			content, err := os.ReadFile(candidate)
			if err != nil {
				t.Fatalf("read %s: %v", candidate, err)
			}
			return string(content)
		}
		dir = filepath.Dir(dir)
	}
	t.Fatalf("could not find %s above the package directory", rel)
	return ""
}

// jobDeclaration is one job as cmd/billing declares it.
type jobDeclaration struct {
	Name     string
	Interval time.Duration
}

var (
	jobNamePattern     = regexp.MustCompile(`name:\s+"([a-z0-9-]+)",`)
	jobIntervalPattern = regexp.MustCompile(`name:\s+"([a-z0-9-]+)",\s*\n\s*interval:\s*([^,\n]+),`)
	jobConstPattern    = regexp.MustCompile(`(?m)^\s*const\s+([A-Za-z_][A-Za-z0-9_]*)\s*=\s*([^\n]+)$`)
)

// declaredJobs reads the job list out of cmd/billing's registration.
//
// It is a source scan rather than a call because cmd/billing is package main: the worker's own
// registration cannot be imported. The scan is deliberately strict — an interval expression it
// does not understand fails the test with the expression in the message — so a new job cannot
// quietly slip past the comparison.
func declaredJobs(t *testing.T, sources ...string) []jobDeclaration {
	t.Helper()

	consts := map[string]string{}
	for _, src := range sources {
		for _, m := range jobConstPattern.FindAllStringSubmatch(src, -1) {
			consts[m[1]] = strings.TrimSpace(m[2])
		}
	}

	var out []jobDeclaration
	seen := map[string]bool{}
	for _, src := range sources {
		for _, m := range jobIntervalPattern.FindAllStringSubmatch(src, -1) {
			name := m[1]
			if seen[name] {
				continue
			}
			seen[name] = true
			out = append(out, jobDeclaration{
				Name:     name,
				Interval: durationOf(t, strings.TrimSpace(m[2]), consts),
			})
		}
	}
	if len(out) == 0 {
		t.Fatal("the job parser found no jobs; the registration has moved or changed shape")
	}
	return out
}

// durationOf evaluates the interval expressions the worker uses: `5 * time.Second`,
// `time.Minute`, or a named constant defined beside it.
func durationOf(t *testing.T, expr string, consts map[string]string) time.Duration {
	t.Helper()
	units := map[string]time.Duration{
		"time.Second": time.Second,
		"time.Minute": time.Minute,
		"time.Hour":   time.Hour,
	}
	total := time.Duration(1)
	for _, factor := range strings.Split(expr, "*") {
		factor = strings.TrimSpace(factor)
		switch {
		case units[factor] != 0:
			total *= units[factor]
		case factor == "":
			t.Fatalf("empty factor in interval expression %q", expr)
		case strings.ContainsAny(factor, "0123456789"):
			n, err := strconv.Atoi(factor)
			if err != nil {
				t.Fatalf("cannot read the multiplier in %q: %v", expr, err)
			}
			total *= time.Duration(n)
		default:
			value, ok := consts[factor]
			if !ok {
				t.Fatalf("the interval expression %q names %q, which this test cannot resolve", expr, factor)
			}
			total *= durationOf(t, value, consts)
		}
	}
	if total <= 0 {
		t.Fatalf("interval expression %q evaluated to %v", expr, total)
	}
	return total
}

// The automation list is the worker's job list: every job the worker registers is on the screen
// with the interval it is registered with, and every row names where it is registered.
func TestAutomationMatchesTheJobsTheWorkerRegisters(t *testing.T) {
	// The worker's own switches are cleared, so the comparison is against the full registration
	// rather than against whatever the developer's shell happens to export.
	for _, key := range []string{
		"BILLING_DISABLE_PAYMENTS", "BILLING_DISABLE_PROVISIONING", "BILLING_DISABLE_SUSPENSION",
		"BILLING_DISABLE_REFUND_RECONCILE", "BILLING_DISABLE_NOTIFICATIONS",
		lifecycle.OrderCancelKillSwitch, lifecycle.TerminationKillSwitch,
		"BILLING_EMBEDDED_WORKER",
	} {
		t.Setenv(key, "")
	}

	env := newStaffEnv(t)
	body := fetchAutomation(t, env)

	byName := map[string]int64{}
	sources := map[string]string{}
	for _, row := range body.Data {
		if row.Summary == "" {
			t.Fatalf("job %q has no summary, so the screen cannot say what it does", row.Name)
		}
		if row.IntervalSeconds <= 0 || row.Interval == "" {
			t.Fatalf("job %q has no interval", row.Name)
		}
		if row.Source == "" {
			t.Fatalf("job %q does not say where it is registered", row.Name)
		}
		byName[row.Name] = row.IntervalSeconds
		sources[row.Name] = row.Source
	}

	// 1. The two lifecycle rules, compared against the function the worker itself calls.
	registered := lifecycle.Jobs(env.pool, nil)
	if len(registered) == 0 {
		t.Fatal("lifecycle.Jobs() returned nothing, so this comparison proves nothing")
	}
	for _, job := range registered {
		seconds, ok := byName[job.Name]
		if !ok {
			t.Fatalf("the worker registers %q but the automation screen does not list it", job.Name)
		}
		if want := int64(job.Interval / time.Second); seconds != want {
			t.Fatalf("job %q: screen says %ds, the worker registers %ds", job.Name, seconds, want)
		}
		if sources[job.Name] != "internal/lifecycle/lifecycle.go" {
			t.Fatalf("job %q claims to be registered in %q", job.Name, sources[job.Name])
		}
	}

	// 2. Everything cmd/billing registers, read out of the registration itself.
	declared := declaredJobs(t,
		repoFile(t, "cmd/billing/jobs.go"),
		repoFile(t, "cmd/billing/piping.go"),
	)
	for _, job := range declared {
		seconds, ok := byName[job.Name]
		if !ok {
			t.Fatalf("cmd/billing registers %q but the automation screen does not list it", job.Name)
		}
		if want := int64(job.Interval / time.Second); seconds != want {
			t.Fatalf("job %q: screen says %ds, the worker registers %ds", job.Name, seconds, want)
		}
	}

	// 3. Every row is a job something registers, so the screen cannot describe a job that no
	// longer exists.
	declaredNames := map[string]bool{}
	for _, job := range declared {
		declaredNames[job.Name] = true
	}
	for _, job := range registered {
		declaredNames[job.Name] = true
	}
	for _, row := range body.Data {
		if !declaredNames[row.Name] {
			t.Fatalf("the automation screen lists %q, which nothing registers", row.Name)
		}
	}

	// 4. The switches are reported as read-only environment variables, in the response itself,
	// so the screen cannot render them as controls.
	if body.KillSwitches.Mechanism != "environment" || !body.KillSwitches.ReadOnly {
		t.Fatalf("kill switches = %+v, want a read-only environment mechanism", body.KillSwitches)
	}
	if body.KillSwitches.Note == "" {
		t.Fatal("the response does not explain why the switches cannot be changed here")
	}

	// 5. Every switch named is documented in .env.example, because a switch an operator cannot
	// find is a switch that does not exist.
	envExample := repoFile(t, ".env.example")
	for _, row := range body.Data {
		if row.KillSwitch == nil {
			continue
		}
		if !strings.Contains(envExample, *row.KillSwitch) {
			t.Fatalf("job %q names the switch %q, which .env.example does not document",
				row.Name, *row.KillSwitch)
		}
	}
}

// A job stopped by its switch is still listed, and says so. "What runs on this box" has to
// include what has been switched off, or an operator cannot tell a stopped job from a job that
// was never there.
func TestAutomationShowsAJobItsSwitchHasStopped(t *testing.T) {
	env := newStaffEnv(t)

	// Before: the job is registered and enabled.
	body := fetchAutomation(t, env)
	before := findJob(t, body, lifecycle.OrderCancelJob)
	if before.Disabled {
		t.Fatalf("%q is disabled before the switch was set", lifecycle.OrderCancelJob)
	}
	if before.KillSwitch == nil || *before.KillSwitch != lifecycle.OrderCancelKillSwitch {
		t.Fatalf("%q does not name its switch: %+v", lifecycle.OrderCancelJob, before.KillSwitch)
	}

	// The worker's registration honours the switch; the screen reads the same source.
	t.Setenv(lifecycle.OrderCancelKillSwitch, "1")
	for _, job := range lifecycle.Jobs(env.pool, nil) {
		if job.Name == lifecycle.OrderCancelJob {
			t.Fatalf("lifecycle.Jobs() still registers %q with its switch set", lifecycle.OrderCancelJob)
		}
	}

	body = fetchAutomation(t, env)
	after := findJob(t, body, lifecycle.OrderCancelJob)
	if !after.Disabled {
		t.Fatalf("%q is not reported as disabled with %s set", lifecycle.OrderCancelJob, lifecycle.OrderCancelKillSwitch)
	}
	if !after.KillSwitchSet {
		t.Fatalf("%q does not report its switch as set", lifecycle.OrderCancelJob)
	}
	// It is still on the screen: a disabled job has to be visible to be understood.
	if after.Summary == "" || after.IntervalSeconds == 0 {
		t.Fatalf("the disabled job lost its description: %+v", after)
	}
}

// findJob returns one row by name.
func findJob(t *testing.T, body automationResponse, name string) automationRow {
	t.Helper()
	for _, row := range body.Data {
		if row.Name == name {
			return row
		}
	}
	t.Fatalf("job %q is not on the automation screen", name)
	return body.Data[0]
}

// The job list needs settings:write; support holds neither, so it cannot read it.
func TestAutomationNeedsSettingsWrite(t *testing.T) {
	env := newStaffEnv(t)

	rec := env.get(t, "/api/v1/staff/automation", env.support)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("support reading automation: status %d, want 403", rec.Code)
	}
	problemCode(t, rec)
}

// ------------------------------------------------------- service properties --

// propertyRow is one definition as the screen reads it.
type propertyRow struct {
	Key             string `json:"key"`
	Label           string `json:"label"`
	Description     string `json:"description"`
	ShowOnInvoice   bool   `json:"show_on_invoice"`
	CustomerVisible bool   `json:"customer_visible"`
	SortOrder       int    `json:"sort_order"`
}

func listProperties(t *testing.T, env *staffEnv) []propertyRow {
	t.Helper()
	rec := env.get(t, "/api/v1/staff/service-properties", env.admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("list properties: status %d; body %s", rec.Code, rec.Body)
	}
	var body struct {
		Properties []propertyRow `json:"properties"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("properties body: %v", err)
	}
	return body.Properties
}

func propertyUsage(t *testing.T, env *staffEnv) map[string]int {
	t.Helper()
	rec := env.get(t, "/api/v1/staff/service-properties/usage", env.admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("usage: status %d; body %s", rec.Code, rec.Body)
	}
	var body struct {
		Data []struct {
			Key      string `json:"key"`
			Services int    `json:"services"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("usage body: %v", err)
	}
	out := map[string]int{}
	for _, row := range body.Data {
		out[row.Key] = row.Services
	}
	return out
}

// A definition round-trips with both visibility flags, and a reorder round-trips.
func TestServicePropertyDefinitionRoundTrips(t *testing.T) {
	env := newStaffEnv(t)

	// The two flags answer different questions, and the screen's whole job is to keep them
	// apart: this one prints on a document and stays out of the customer's portal.
	rec := env.doJSON(t, http.MethodPut, "/api/v1/staff/service-properties/asset_tag", env.admin,
		env.adminCSRF, map[string]any{
			"label":            "Asset tag",
			"description":      "The internal inventory number.",
			"show_on_invoice":  true,
			"customer_visible": false,
			"sort_order":       20,
		})
	if rec.Code != http.StatusOK {
		t.Fatalf("create: status %d; body %s", rec.Code, rec.Body)
	}
	var created propertyRow
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("create body: %v", err)
	}
	if !created.ShowOnInvoice || created.CustomerVisible {
		t.Fatalf("the flags did not round-trip: %+v", created)
	}

	// A second definition, created ahead of it.
	rec = env.doJSON(t, http.MethodPut, "/api/v1/staff/service-properties/rack", env.admin,
		env.adminCSRF, map[string]any{
			"label":            "Rack",
			"description":      "Which cabinet the server sits in.",
			"show_on_invoice":  false,
			"customer_visible": true,
			"sort_order":       10,
		})
	if rec.Code != http.StatusOK {
		t.Fatalf("create rack: status %d; body %s", rec.Code, rec.Body)
	}

	rows := listProperties(t, env)
	if len(rows) != 2 || rows[0].Key != "rack" || rows[1].Key != "asset_tag" {
		t.Fatalf("list order = %+v, want rack then asset_tag", rows)
	}
	if !rows[1].ShowOnInvoice || rows[1].CustomerVisible {
		t.Fatalf("the list lost the flags: %+v", rows[1])
	}
	if rows[1].Description == "" || rows[1].Label != "Asset tag" {
		t.Fatalf("the list lost the wording: %+v", rows[1])
	}

	// Reorder: asset_tag first. The response is the order after the change, which is what the
	// screen renders.
	rec = env.doJSON(t, http.MethodPost, "/api/v1/staff/service-properties/reorder", env.admin,
		env.adminCSRF, map[string]any{"keys": []string{"asset_tag", "rack"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("reorder: status %d; body %s", rec.Code, rec.Body)
	}
	var order struct {
		Keys []string `json:"keys"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &order); err != nil {
		t.Fatalf("reorder body: %v", err)
	}
	if len(order.Keys) != 2 || order.Keys[0] != "asset_tag" {
		t.Fatalf("reorder response = %v", order.Keys)
	}
	rows = listProperties(t, env)
	if rows[0].Key != "asset_tag" || rows[1].Key != "rack" {
		t.Fatalf("the order did not persist: %+v", rows)
	}

	// An empty order and a duplicated key are refused rather than half-applied.
	for _, bad := range []map[string]any{
		{"keys": []string{}},
		{"keys": []string{"rack", "rack"}},
	} {
		rec = env.doJSON(t, http.MethodPost, "/api/v1/staff/service-properties/reorder", env.admin,
			env.adminCSRF, bad)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("reorder %v: status %d, want 422; body %s", bad, rec.Code, rec.Body)
		}
		if p := decodeProblem(t, rec); p.Code != "invalid_order" {
			t.Fatalf("code = %q, want invalid_order", p.Code)
		}
	}
	// A key that has no definition is refused with the code the metadata editor already uses.
	rec = env.doJSON(t, http.MethodPost, "/api/v1/staff/service-properties/reorder", env.admin,
		env.adminCSRF, map[string]any{"keys": []string{"nonexistent"}})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("reorder with an unknown key: status %d, want 400; body %s", rec.Code, rec.Body)
	}
	if p := decodeProblem(t, rec); p.Code != "unknown_property" {
		t.Fatalf("code = %q, want unknown_property", p.Code)
	}

	// An unused definition deletes cleanly.
	rec = env.doJSON(t, http.MethodDelete, "/api/v1/staff/service-properties/rack", env.admin,
		env.adminCSRF, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete: status %d; body %s", rec.Code, rec.Body)
	}
	if rows = listProperties(t, env); len(rows) != 1 {
		t.Fatalf("after the delete: %+v", rows)
	}
}

// A definition a service carries cannot be deleted, and the usage read says so before the click.
func TestServicePropertyInUseCannotBeDeleted(t *testing.T) {
	env := newStaffEnv(t)
	tenant := env.createTenant(t, "prop-in-use@example.test", "12.00")

	rec := env.doJSON(t, http.MethodPut, "/api/v1/staff/service-properties/rack", env.admin,
		env.adminCSRF, map[string]any{
			"label": "Rack", "show_on_invoice": true, "customer_visible": true, "sort_order": 10,
		})
	if rec.Code != http.StatusOK {
		t.Fatalf("define: status %d; body %s", rec.Code, rec.Body)
	}

	// Before a service carries it, the delete is allowed by the guard.
	if usage := propertyUsage(t, env); usage["rack"] != 0 {
		t.Fatalf("usage for an unused definition = %d, want 0", usage["rack"])
	}

	rec = env.doJSON(t, http.MethodPut,
		"/api/v1/staff/services/"+tenant.serviceID+"/metadata", env.admin, env.adminCSRF,
		map[string]any{"custom_properties": map[string]string{"rack": "B2"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("set the property: status %d; body %s", rec.Code, rec.Body)
	}

	if usage := propertyUsage(t, env); usage["rack"] != 1 {
		t.Fatalf("usage after the value was set = %d, want 1", usage["rack"])
	}

	rec = env.doJSON(t, http.MethodDelete, "/api/v1/staff/service-properties/rack", env.admin,
		env.adminCSRF, nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("deleting a definition in use: status %d, want 409; body %s", rec.Code, rec.Body)
	}
	if p := decodeProblem(t, rec); p.Code != "property_in_use" {
		t.Fatalf("code = %q, want property_in_use", p.Code)
	}
	// It is still there, and the service still carries its value.
	if rows := listProperties(t, env); len(rows) != 1 {
		t.Fatalf("the refused delete removed the definition: %+v", rows)
	}
}

// The two flags are honoured where they matter: customer_visible decides whether the customer's
// own page shows the property, and show_on_invoice decides whether a document prints it.
func TestServicePropertyFlagsDecideWhatTheCustomerSees(t *testing.T) {
	env := newStaffEnv(t)
	tenant := env.createTenant(t, "prop-flags@example.test", "12.00")
	cookie, _ := env.login(t, "prop-flags@example.test", "a long enough password")

	// One property the customer may read and one they may not.
	for key, visible := range map[string]bool{"rack": true, "asset_tag": false} {
		rec := env.doJSON(t, http.MethodPut, "/api/v1/staff/service-properties/"+key, env.admin,
			env.adminCSRF, map[string]any{
				"label":            strings.ToUpper(key[:1]) + key[1:],
				"show_on_invoice":  false,
				"customer_visible": visible,
			})
		if rec.Code != http.StatusOK {
			t.Fatalf("define %s: status %d; body %s", key, rec.Code, rec.Body)
		}
	}

	rec := env.doJSON(t, http.MethodPut,
		"/api/v1/staff/services/"+tenant.serviceID+"/metadata", env.admin, env.adminCSRF,
		map[string]any{"custom_properties": map[string]string{"rack": "B2", "asset_tag": "AT-9"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("set the properties: status %d; body %s", rec.Code, rec.Body)
	}

	// The staff view carries both, with their definitions attached.
	rec = env.get(t, "/api/v1/staff/services/"+tenant.serviceID+"/metadata", env.admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("staff metadata: status %d; body %s", rec.Code, rec.Body)
	}
	staffKeys := propertyKeys(t, rec.Body.Bytes())
	if !staffKeys["rack"] || !staffKeys["asset_tag"] {
		t.Fatalf("the staff view is missing a property: %v", staffKeys)
	}

	// The customer's view carries the visible one only.
	rec = env.get(t, "/api/v1/portal/services/"+tenant.serviceID+"/metadata", cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("portal metadata: status %d; body %s", rec.Code, rec.Body)
	}
	portalKeys := propertyKeys(t, rec.Body.Bytes())
	if !portalKeys["rack"] {
		t.Fatalf("the customer cannot see the property marked customer_visible: %v", portalKeys)
	}
	if portalKeys["asset_tag"] {
		t.Fatalf("the customer can see a property marked customer_visible=false: %v", portalKeys)
	}
}

// propertyKeys reads the property keys out of a metadata response.
func propertyKeys(t *testing.T, body []byte) map[string]bool {
	t.Helper()
	var decoded struct {
		Properties []struct {
			Key   string `json:"key"`
			Label string `json:"label"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("metadata body: %v (%s)", err, body)
	}
	out := map[string]bool{}
	for _, p := range decoded.Properties {
		out[p.Key] = true
		if p.Label == "" {
			t.Fatalf("property %q has no label", p.Key)
		}
	}
	return out
}

// The property catalogue needs service:write:all to change and service:read:all to read, and a
// caller without them is refused with a problem document rather than an empty list.
func TestServicePropertyCatalogueIsGuarded(t *testing.T) {
	env := newStaffEnv(t)
	env.createTenant(t, "prop-perm@example.test", "12.00")
	cookie, customerCSRF := env.login(t, "prop-perm@example.test", "a long enough password")

	rec := env.doJSON(t, http.MethodPost, "/api/v1/staff/service-properties/reorder", cookie,
		customerCSRF, map[string]any{"keys": []string{"rack"}})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("customer reordering: status %d, want 403", rec.Code)
	}
	problemCode(t, rec)

	// A mutation without a CSRF token is refused before the handler runs.
	rec = env.doJSON(t, http.MethodPost, "/api/v1/staff/service-properties/reorder", env.admin, "",
		map[string]any{"keys": []string{"rack"}})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("reorder without a CSRF token: status %d, want 403", rec.Code)
	}
	problemCode(t, rec)
}
