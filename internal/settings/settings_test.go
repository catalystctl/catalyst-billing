package settings_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/catalyst/billing/internal/settings"
	"github.com/catalyst/billing/internal/store/storetest"
	"github.com/jackc/pgx/v5"
)

// recordingAuditor captures the audit rows a write would have produced, so a test can
// assert what was recorded without needing the real audit table. It implements
// audit.Auditor, which is the same interface the HTTP layer passes in.
type recordingAuditor struct {
	mu    sync.Mutex
	calls []auditCall
}

type auditCall struct {
	Action     string
	TargetType string
	TargetID   string
	Changes    map[string]any
}

func (a *recordingAuditor) RecordAction(_ context.Context, _ pgx.Tx, action, targetType, targetID string,
	changes map[string]any) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls = append(a.calls, auditCall{Action: action, TargetType: targetType, TargetID: targetID, Changes: changes})
	return nil
}

// A key with no row reads its code-side default rather than failing. This is what lets a
// fresh installation render before an operator has saved anything.
func TestMissingKeyReturnsDefault(t *testing.T) {
	pool := storetest.DB(t)
	s := settings.New(pool)
	ctx := context.Background()

	// The migration seeds these, so delete them to model a fresh, unseeded row.
	if _, err := pool.Exec(ctx, `DELETE FROM settings WHERE group_key = 'general' AND key = 'company_name'`); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM settings WHERE group_key = 'cronjob' AND key = 'terminate_days'`); err != nil {
		t.Fatalf("delete: %v", err)
	}

	got, err := s.String(ctx, "general", "company_name")
	if err != nil {
		t.Fatalf("String: %v", err)
	}
	if got != "Catalyst Hosting" {
		t.Fatalf("company_name = %q, want the code-side default", got)
	}

	days, err := s.Int(ctx, "cronjob", "terminate_days")
	if err != nil {
		t.Fatalf("Int: %v", err)
	}
	if days != 30 {
		t.Fatalf("terminate_days = %d, want 30", days)
	}

	// A key that was never seeded at all still has a default: the SMTP port is declared in
	// code, not inserted by 0020.
	port, err := s.Int(ctx, "mail", "smtp_port")
	if err != nil {
		t.Fatalf("Int(smtp_port): %v", err)
	}
	if port != 587 {
		t.Fatalf("smtp_port = %d, want 587", port)
	}

	// The group read reports the default with is_set=false, which is how a client tells
	// "default" from "explicitly set to the default".
	values, err := s.Group(ctx, "general")
	if err != nil {
		t.Fatalf("Group: %v", err)
	}
	for _, v := range values {
		if v.Key != "company_name" {
			continue
		}
		if v.Value != "Catalyst Hosting" || v.IsSet || v.UpdatedAt != nil {
			t.Fatalf("default value = %+v, want the default with is_set=false", v)
		}
	}
}

// An undeclared key is an error, not a silent empty string: a typo must fail where it is
// written rather than render as a blank field in production.
func TestUnknownKeyIsAnError(t *testing.T) {
	pool := storetest.DB(t)
	s := settings.New(pool)

	if _, err := s.String(context.Background(), "general", "company_nmae"); err == nil {
		t.Fatal("a misspelled key returned a value instead of an error")
	}
}

// A secret is never returned by a read path. The stored text must not appear in the value,
// in a group read, or in a full read, and is_set must be the only thing it reveals.
func TestSecretIsNeverReturned(t *testing.T) {
	pool := storetest.DB(t)
	s := settings.New(pool)
	ctx := context.Background()

	const secret = "super-secret-smtp-password"
	if _, err := s.Update(ctx, "mail", map[string]string{"smtp_password": secret}, "", nil); err != nil {
		t.Fatalf("write secret: %v", err)
	}

	// The typed getter is the in-process path and does return it: a mailer has to.
	raw, err := s.String(ctx, "mail", "smtp_password")
	if err != nil {
		t.Fatalf("String: %v", err)
	}
	if raw != secret {
		t.Fatalf("the typed getter returned %q, want the stored secret", raw)
	}

	// Every read path a handler could serialise is redacted.
	group, err := s.Group(ctx, "mail")
	if err != nil {
		t.Fatalf("Group: %v", err)
	}
	assertRedacted(t, group, secret)

	all, err := s.All(ctx)
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	for _, g := range all {
		assertRedacted(t, g.Keys, secret)
	}

	// The encoded JSON must not contain the secret either: a redaction that a marshaller
	// can undo is not a redaction.
	encoded, err := json.Marshal(all)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(encoded), secret) {
		t.Fatalf("the secret appears in the JSON encoding: %s", encoded)
	}

	// is_set distinguishes "a password is configured" from "none", which is the only
	// useful thing a redacted read can say.
	var found bool
	for _, v := range group {
		if v.Key != "smtp_password" {
			continue
		}
		found = true
		if v.Value != settings.RedactedMarker {
			t.Fatalf("value = %q, want the redacted marker", v.Value)
		}
		if !v.IsSet {
			t.Fatal("is_set = false for a secret that was written")
		}
		if !v.IsSecret {
			t.Fatal("is_secret = false for a secret key")
		}
	}
	if !found {
		t.Fatal("smtp_password is missing from the mail group")
	}
}

func assertRedacted(t *testing.T, values []settings.Value, secret string) {
	t.Helper()
	for _, v := range values {
		if strings.Contains(v.Value, secret) {
			t.Fatalf("%s.%s leaked the secret in a read: %q", v.Group, v.Key, v.Value)
		}
	}
}

// Saving a redacted secret back (the "load the form, change one field, save" case) must
// not overwrite the credential with the marker.
func TestRedactedMarkerIsNotStored(t *testing.T) {
	pool := storetest.DB(t)
	s := settings.New(pool)
	ctx := context.Background()

	const secret = "real-password"
	if _, err := s.Update(ctx, "mail", map[string]string{"smtp_password": secret}, "", nil); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := s.Update(ctx, "mail", map[string]string{
		"smtp_password": settings.RedactedMarker,
		"smtp_host":     "smtp.example.test",
	}, "", nil); err != nil {
		t.Fatalf("second write: %v", err)
	}

	got, err := s.String(ctx, "mail", "smtp_password")
	if err != nil {
		t.Fatalf("String: %v", err)
	}
	if got != secret {
		t.Fatalf("smtp_password = %q, want the original secret preserved", got)
	}
	host, err := s.String(ctx, "mail", "smtp_host")
	if err != nil {
		t.Fatalf("String(host): %v", err)
	}
	if host != "smtp.example.test" {
		t.Fatalf("smtp_host = %q, want the new value", host)
	}
}

// A write is audited: one row per key, naming the key, and never containing a secret's
// value.
func TestWriteIsAudited(t *testing.T) {
	pool := storetest.DB(t)
	s := settings.New(pool)
	ctx := context.Background()

	actor, err := createStaffUser(ctx, pool)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	rec := &recordingAuditor{}
	if _, err := s.Update(ctx, "general", map[string]string{"company_name": "Acme Hosting"}, actor, rec); err != nil {
		t.Fatalf("Update: %v", err)
	}

	if len(rec.calls) != 1 {
		t.Fatalf("audit rows = %d, want 1", len(rec.calls))
	}
	call := rec.calls[0]
	if call.Action != "setting.update" {
		t.Fatalf("action = %q, want setting.update", call.Action)
	}
	if call.TargetType != "setting" {
		t.Fatalf("target_type = %q, want setting", call.TargetType)
	}
	if call.Changes["key"] != "company_name" || call.Changes["group"] != "general" {
		t.Fatalf("changes do not name the key: %+v", call.Changes)
	}
	if call.Changes["value"] != "Acme Hosting" {
		t.Fatalf("changes do not record the new value: %+v", call.Changes)
	}

	// updated_by is recorded on the row itself, which is what makes "who last changed
	// this" answerable without scanning the audit log.
	var updatedBy string
	if err := pool.QueryRow(ctx,
		`SELECT updated_by::text FROM settings WHERE group_key = 'general' AND key = 'company_name'`).
		Scan(&updatedBy); err != nil {
		t.Fatalf("read updated_by: %v", err)
	}
	if updatedBy != actor {
		t.Fatalf("updated_by = %q, want %q", updatedBy, actor)
	}

	// A secret's audit row records that it changed, not what it changed to.
	rec.calls = nil
	if _, err := s.Update(ctx, "mail", map[string]string{"smtp_password": "hunter2"}, actor, rec); err != nil {
		t.Fatalf("Update secret: %v", err)
	}
	if len(rec.calls) != 1 {
		t.Fatalf("secret audit rows = %d, want 1", len(rec.calls))
	}
	if rec.calls[0].Changes["value"] != settings.RedactedMarker {
		t.Fatalf("a secret's audit row recorded its value: %+v", rec.calls[0].Changes)
	}
}

// A bulk group read is consistent: it returns every declared key exactly once, in
// declaration order, with the stored value where one exists.
func TestGroupReadIsCompleteAndConsistent(t *testing.T) {
	pool := storetest.DB(t)
	s := settings.New(pool)
	ctx := context.Background()

	if _, err := s.Update(ctx, "invoices", map[string]string{
		"due_days":             "14",
		"proforma":             "true",
		"bill_to_text":         "Catalyst Hosting\n1 Main St",
		"notes_client_visible": "false",
	}, "", nil); err != nil {
		t.Fatalf("Update: %v", err)
	}

	values, err := s.Group(ctx, "invoices")
	if err != nil {
		t.Fatalf("Group: %v", err)
	}

	// Every declared key appears exactly once, in declaration order.
	defs := settings.Definitions()
	var want []string
	for _, d := range defs {
		if d.Group == "invoices" {
			want = append(want, d.Key)
		}
	}
	if len(values) != len(want) {
		t.Fatalf("group read returned %d keys, want %d", len(values), len(want))
	}
	for i, key := range want {
		if values[i].Key != key {
			t.Fatalf("key %d = %q, want %q (declaration order)", i, values[i].Key, key)
		}
	}

	// The stored values are the ones just written; the rest are defaults.
	byKey := map[string]settings.Value{}
	for _, v := range values {
		byKey[v.Key] = v
	}
	if byKey["due_days"].Value != "14" || !byKey["due_days"].IsSet {
		t.Fatalf("due_days = %+v, want 14 and is_set", byKey["due_days"])
	}
	if byKey["proforma"].Value != "true" {
		t.Fatalf("proforma = %q, want true", byKey["proforma"].Value)
	}
	if byKey["invoice_number_prefix"].Value != "INV-" {
		t.Fatalf("invoice_number_prefix = %+v, want the seeded value", byKey["invoice_number_prefix"])
	}
	// A key that 0020 does not seed reports its code-side default and is_set=false.
	if byKey["due_days"].IsSet != true {
		t.Fatalf("due_days should be marked as explicitly set: %+v", byKey["due_days"])
	}
	mail, err := s.Group(ctx, "mail")
	if err != nil {
		t.Fatalf("Group(mail): %v", err)
	}
	for _, v := range mail {
		if v.Key == "smtp_port" && (v.Value != "587" || v.IsSet) {
			t.Fatalf("smtp_port = %+v, want the code-side default with is_set=false", v)
		}
	}

	// Reading the same group twice is stable: the read is one statement over a stable
	// order, not a map iteration.
	again, err := s.Group(ctx, "invoices")
	if err != nil {
		t.Fatalf("Group again: %v", err)
	}
	if len(again) != len(values) {
		t.Fatalf("a second read returned %d keys, want %d", len(again), len(values))
	}
	for i := range values {
		if again[i].Key != values[i].Key || again[i].Value != values[i].Value {
			t.Fatalf("key %d changed between reads: %+v then %+v", i, values[i], again[i])
		}
	}
}

// Validation runs before any write: a bulk update with one bad value writes nothing.
func TestInvalidValueWritesNothing(t *testing.T) {
	pool := storetest.DB(t)
	s := settings.New(pool)
	ctx := context.Background()

	before, err := s.String(ctx, "general", "company_name")
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	_, err = s.Update(ctx, "general", map[string]string{
		"company_name":          "Changed",
		"registration_disabled": "maybe",
	}, "", nil)
	if err == nil {
		t.Fatal("an invalid boolean was accepted")
	}
	if !strings.Contains(err.Error(), "registration_disabled") {
		t.Fatalf("error does not name the offending key: %v", err)
	}

	after, err := s.String(ctx, "general", "company_name")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if after != before {
		t.Fatalf("company_name = %q after a rejected update, want %q: a partial write landed", after, before)
	}
}

// createStaffUser inserts a user so updated_by has a real reference to point at.
func createStaffUser(ctx context.Context, pool interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}) (string, error) {
	var id string
	err := pool.QueryRow(ctx, `
		INSERT INTO users (email, password_hash, email_verified_at)
		VALUES ('operator@example.test', 'x', now()) RETURNING id`).Scan(&id)
	return id, err
}

// ------------------------------------------------------------------ branding --

// The brand group is declared, so a settings page renders it and a typo is an error rather
// than a silent empty value.
func TestBrandingGroupIsDeclared(t *testing.T) {
	want := map[string]bool{"name": false, "logo_id": false, "accent_light": false, "accent_dark": false}
	found := 0
	for _, d := range settings.Definitions() {
		if d.Group != "branding" {
			continue
		}
		if _, ok := want[d.Key]; !ok {
			t.Fatalf("the branding group declares an unexpected key %q", d.Key)
		}
		want[d.Key] = true
		found++
	}
	if found != len(want) {
		t.Fatalf("the branding group declares %d keys, want %d", found, len(want))
	}
	for key, seen := range want {
		if !seen {
			t.Fatalf("branding.%s is not declared", key)
		}
	}
	// And every default is empty: a default that repeated the built-in appearance would
	// make "unset" indistinguishable from "set to the default", and the byte-identical
	// fallback could not be tested.
	for _, d := range settings.Definitions() {
		if d.Group == "branding" && d.Default != "" {
			t.Fatalf("branding.%s defaults to %q, want the empty string", d.Key, d.Default)
		}
	}
}

// A brand write is validated before anything is stored: a form that sends a valid name and
// an invalid colour must not leave the name applied.
func TestBrandingAcceptsHexColoursAndRefusesTheRest(t *testing.T) {
	pool := storetest.DB(t)
	s := settings.New(pool)
	ctx := context.Background()

	accepted := []string{"", "#3b5bdb", "3b5bdb", "#ABC", "#000000"}
	for _, value := range accepted {
		if _, err := s.Update(ctx, "branding", map[string]string{"accent_light": value}, "", nil); err != nil {
			t.Fatalf("accent_light %q was refused: %v", value, err)
		}
	}

	for _, value := range []string{"chartreuse", "#12345", "rgb(1,2,3)", "#3b5bdb; drop table settings"} {
		_, err := s.Update(ctx, "branding", map[string]string{"accent_light": value}, "", nil)
		if err == nil {
			t.Fatalf("accent_light %q was accepted", value)
		}
		if !strings.Contains(err.Error(), "branding.accent_light") {
			t.Fatalf("the error does not name the key: %v", err)
		}
	}

	// The logo is an identifier: a value that is not one is refused rather than stored as a
	// reference that names nothing.
	if _, err := s.Update(ctx, "branding", map[string]string{"logo_id": "not-a-uuid"}, "", nil); err == nil {
		t.Fatal("a non-uuid logo_id was accepted")
	}
	validID := "8c8d0f5a-0000-0000-0000-000000000001"
	if _, err := s.Update(ctx, "branding", map[string]string{"logo_id": validID}, "", nil); err != nil {
		t.Fatalf("a valid logo_id was refused: %v", err)
	}
}

// A name is printed in the sidebar and on every invoice, so it is bounded and carries no
// control characters.
func TestBrandingNameIsBounded(t *testing.T) {
	pool := storetest.DB(t)
	s := settings.New(pool)
	ctx := context.Background()

	if _, err := s.Update(ctx, "branding", map[string]string{"name": strings.Repeat("a", settings.MaxBrandName)}, "", nil); err != nil {
		t.Fatalf("a name at the limit was refused: %v", err)
	}
	if _, err := s.Update(ctx, "branding", map[string]string{"name": strings.Repeat("a", settings.MaxBrandName+1)}, "", nil); err == nil {
		t.Fatal("a name over the limit was accepted")
	}
	if _, err := s.Update(ctx, "branding", map[string]string{"name": "Acme\nHosting"}, "", nil); err == nil {
		t.Fatal("a name containing a newline was accepted")
	}
}

// The database carries the same rule as the code, so a row written by hand through psql
// cannot create a value the renderers cannot draw.
func TestBrandingConstraintsExistInTheSchema(t *testing.T) {
	pool := storetest.DB(t)
	ctx := context.Background()

	if _, err := pool.Exec(ctx, `
		INSERT INTO settings (group_key, key, value) VALUES ('branding', 'accent_light', 'chartreuse')`,
	); err == nil {
		t.Fatal("the schema accepted a colour that is not a colour")
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO settings (group_key, key, value) VALUES ('branding', 'logo_id', 'not-a-uuid')`,
	); err == nil {
		t.Fatal("the schema accepted a logo id that is not an id")
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO settings (group_key, key, value) VALUES ('branding', 'name', repeat('a', 81))`,
	); err == nil {
		t.Fatal("the schema accepted a name longer than the panel can render")
	}
	// The three-digit form is expanded and lower-cased on write, so there is exactly one
	// spelling of a colour in the database and the panel's comparison against its own token
	// cannot be fooled by case.
	if _, err := pool.Exec(ctx, `
		INSERT INTO settings (group_key, key, value) VALUES ('branding', 'accent_dark', '#ABC')
		ON CONFLICT (group_key, key) DO UPDATE SET value = EXCLUDED.value`,
	); err != nil {
		t.Fatalf("insert a short hex colour: %v", err)
	}
	var stored string
	if err := pool.QueryRow(ctx, `
		SELECT value FROM settings WHERE group_key = 'branding' AND key = 'accent_dark'`,
	).Scan(&stored); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if stored != "#aabbcc" {
		t.Fatalf("the stored accent is %q, want #aabbcc", stored)
	}
}
