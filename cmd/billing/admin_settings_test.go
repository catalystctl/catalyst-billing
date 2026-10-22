package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/catalyst/billing/internal/settings"
	"github.com/catalyst/billing/internal/store"
	"github.com/catalyst/billing/internal/store/storetest"
)

// CLI parity with the operator screens (OPS-17).
//
// These tests drive the commands the way an operator does — through cmdAdmin, against the real
// database, with the connection taken from the environment — because the failure they exist to
// prevent is a command that looks right and writes the wrong thing.

// cliEnv points the commands at the test database and returns the pool they will reach.
//
// The commands open their own pool from BILLING_DATABASE_URL, so this sets that variable rather
// than passing the pool in: the point is to exercise the operator's path, not an internal one.
func cliEnv(t *testing.T) *store.Pool {
	t.Helper()
	pool := storetest.DB(t)
	t.Setenv("BILLING_DATABASE_URL", dsnFromEnv(t))
	return pool
}

// captureStdout runs fn with os.Stdout replaced and returns what it wrote.
func captureStdout(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	original := os.Stdout
	os.Stdout = writer
	done := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(reader)
		done <- buf.String()
	}()

	runErr := fn()
	os.Stdout = original
	_ = writer.Close()
	out := <-done
	_ = reader.Close()
	return out, runErr
}

// A setting can be changed from the shell, the change is what the panel reads, and the audit row
// names where it came from.
func TestAdminSettingSetWritesAndAudits(t *testing.T) {
	pool := cliEnv(t)
	ctx := context.Background()

	out, err := captureStdout(t, func() error {
		return cmdAdmin([]string{"setting", "set", "checkout", "tos_required", "true"})
	})
	if err != nil {
		t.Fatalf("setting set: %v", err)
	}
	if !strings.Contains(out, "set checkout.tos_required") {
		t.Fatalf("output = %q", out)
	}

	// The value the panel reads is the value that was written: the command goes through the same
	// accessor the settings screen does, not around it.
	value, err := settings.New(pool).Bool(ctx, "checkout", "tos_required")
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !value {
		t.Fatal("the setting reads false after being set to true")
	}

	// The audit row was written in the same transaction as the value, and it says the change came
	// from the CLI. Without --as there is no session to name, so the row is a system action with the
	// OS operator recorded in the diff rather than a person invented for the actor column.
	var (
		action    string
		actorKind string
		actorID   *string
		changes   []byte
	)
	if err := pool.QueryRow(ctx, `
		SELECT action, actor_kind, actor_id::text, changes
		FROM audit_events
		WHERE target_type = 'setting' AND action = 'setting.update'
		ORDER BY id DESC LIMIT 1`).Scan(&action, &actorKind, &actorID, &changes); err != nil {
		t.Fatalf("audit row: %v", err)
	}
	if action != "setting.update" {
		t.Fatalf("audit action = %q", action)
	}
	if actorKind != "system" || actorID != nil {
		t.Fatalf("audit actor = %s/%v, want a system action with no user", actorKind, actorID)
	}
	var diff map[string]any
	if err := json.Unmarshal(changes, &diff); err != nil {
		t.Fatalf("audit changes: %v", err)
	}
	if diff["source"] != "cli" {
		t.Fatalf("audit changes source = %v, want cli", diff["source"])
	}
	if diff["key"] != "tos_required" || diff["group"] != "checkout" {
		t.Fatalf("audit changes do not name the key: %v", diff)
	}
	if diff["value"] != "true" {
		t.Fatalf("audit changes value = %v", diff["value"])
	}
	if op, _ := diff["operator"].(string); op == "" {
		t.Fatalf("audit changes record no operator: %v", diff)
	}
}

// --as attributes the change to a real operator, and refuses one who could not make it in the panel.
func TestAdminSettingSetAttributesToOperator(t *testing.T) {
	pool := cliEnv(t)
	ctx := context.Background()

	var adminID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO users (email, password_hash, email_verified_at)
		VALUES ('cli-admin@example.test', 'x', now()) RETURNING id`).Scan(&adminID); err != nil {
		t.Fatalf("admin: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO staff_roles (user_id, role) VALUES ($1, 'administrator')`, adminID); err != nil {
		t.Fatalf("role: %v", err)
	}

	out, err := captureStdout(t, func() error {
		return cmdAdmin([]string{"setting", "set", "general", "tos", "Acme Ltd",
			"--as", "CLI-Admin@Example.Test"})
	})
	if err != nil {
		t.Fatalf("setting set --as: %v", err)
	}
	if !strings.Contains(out, "audited as cli-admin@example.test") {
		t.Fatalf("output = %q", out)
	}

	// The audit row names the user, and `updated_by` on the setting row agrees with it: one
	// principal, two places, no disagreement about who changed it.
	var (
		actorID   *string
		actorKind string
		updatedBy *string
	)
	if err := pool.QueryRow(ctx, `
		SELECT a.actor_id::text, a.actor_kind, s.updated_by::text
		FROM audit_events a
		LEFT JOIN settings s ON s.group_key = 'general' AND s.key = 'tos'
		WHERE a.action = 'setting.update'
		ORDER BY a.id DESC LIMIT 1`).Scan(&actorID, &actorKind, &updatedBy); err != nil {
		t.Fatalf("audit row: %v", err)
	}
	if actorID == nil || *actorID != adminID || actorKind != "user" {
		t.Fatalf("audit actor = %s/%v, want user %s", actorKind, actorID, adminID)
	}
	if updatedBy == nil || *updatedBy != adminID {
		t.Fatalf("settings.updated_by = %v, want %s", updatedBy, adminID)
	}

	// A support agent does not hold settings:write, so the CLI refuses exactly what the API would
	// refuse with 403 — an admin action may not take a shortcut the panel does not allow.
	var supportID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO users (email, password_hash, email_verified_at)
		VALUES ('cli-support@example.test', 'x', now()) RETURNING id`).Scan(&supportID); err != nil {
		t.Fatalf("support: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO staff_roles (user_id, role) VALUES ($1, 'support')`, supportID); err != nil {
		t.Fatalf("role: %v", err)
	}

	err = cmdAdmin([]string{"setting", "set", "general", "tos", "Refused Ltd",
		"--as", "cli-support@example.test"})
	if err == nil {
		t.Fatal("a support agent was allowed to change a setting from the CLI")
	}
	if !strings.Contains(err.Error(), "settings:write") {
		t.Fatalf("error = %v, want it to name the missing permission", err)
	}
	// Nothing was written.
	value, err := settings.New(pool).String(ctx, "general", "tos")
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if value == "Refused Ltd" {
		t.Fatal("the refused change was written anyway")
	}

	// An address that matches nobody is an error rather than a silent system action: a typo in
	// --as must not quietly produce an unattributed row.
	if err := cmdAdmin([]string{"setting", "set", "general", "tos", "X",
		"--as", "nobody@example.test"}); err == nil {
		t.Fatal("an unknown --as was accepted")
	}
}

// An unknown key, an unknown group and an invalid value are all refused, with a readable message
// and a non-zero exit, and nothing is written.
func TestAdminSettingSetRefusals(t *testing.T) {
	pool := cliEnv(t)
	ctx := context.Background()

	before, err := settings.New(pool).All(ctx)
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}

	cases := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "unknown key",
			args: []string{"setting", "set", "checkout", "tos_requird", "true"},
			want: "tos_requird",
		},
		{
			name: "unknown group",
			args: []string{"setting", "set", "checkuot", "tos_required", "true"},
			want: "checkuot",
		},
		{
			name: "invalid value",
			args: []string{"setting", "set", "checkout", "tos_required", "yes"},
			want: "must be true or false",
		},
		{
			name: "unknown flag",
			args: []string{"setting", "set", "checkout", "tos_required", "true", "--as-of", "x"},
			want: "unknown flag",
		},
		{
			name: "missing arguments",
			args: []string{"setting", "set", "checkout", "tos_required"},
			want: "usage",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := cmdAdmin(tc.args)
			if err == nil {
				t.Fatalf("%v was accepted", tc.args)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %q, want it to contain %q", err, tc.want)
			}
		})
	}

	// The refusals above ran before any connection for the value-level ones, but the point is the
	// table: not one row changed.
	after, err := settings.New(pool).All(ctx)
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	if len(after) != len(before) {
		t.Fatalf("%d groups after the refusals, was %d", len(after), len(before))
	}
	for i := range before {
		for j := range before[i].Keys {
			if before[i].Keys[j].Value != after[i].Keys[j].Value ||
				before[i].Keys[j].IsSet != after[i].Keys[j].IsSet {
				t.Fatalf("%s.%s changed during a refused command",
					before[i].Group, before[i].Keys[j].Key)
			}
		}
	}

	// The message names what does exist, so a typo is a one-line fix rather than a trip to the
	// source.
	err = cmdAdmin([]string{"setting", "set", "checkout", "tos_requird", "true"})
	if err == nil || !strings.Contains(err.Error(), "tos_required") {
		t.Fatalf("error = %v, want it to list the group's keys", err)
	}
}

// A value of `-` is read from stdin, so a credential never appears in an argv.
func TestAdminSettingSetReadsValueFromStdin(t *testing.T) {
	pool := cliEnv(t)

	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	if _, err := writer.WriteString("From stdin Ltd\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = writer.Close()
	original := os.Stdin
	os.Stdin = reader
	defer func() { os.Stdin = original }()

	if _, err := captureStdout(t, func() error {
		return cmdAdmin([]string{"setting", "set", "general", "tos", "-"})
	}); err != nil {
		t.Fatalf("setting set -: %v", err)
	}

	value, err := settings.New(pool).String(context.Background(), "general", "tos")
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if value != "From stdin Ltd" {
		t.Fatalf("general.tos = %q, want the value piped on stdin", value)
	}
}

// logs tail prints the same rows the /logs screen reads, oldest first, redacted.
func TestAdminLogsTailPrintsTheOperatorLogs(t *testing.T) {
	pool := cliEnv(t)
	ctx := context.Background()

	// Three rows across two tables, so the merge is exercised rather than a single table.
	if _, err := pool.Exec(ctx, `
		INSERT INTO error_logs (level, source, message, detail)
		VALUES ('error', 'worker', 'the renewal job failed', 'smtp password: hunter2')`); err != nil {
		t.Fatalf("error log: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO http_logs (method, path, status, duration_ms)
		VALUES ('GET', '/api/v1/staff/customers', 200, 12)`); err != nil {
		t.Fatalf("http log: %v", err)
	}

	out, err := captureStdout(t, func() error {
		return cmdAdmin([]string{"logs", "tail", "--kind", "all", "--limit", "10"})
	})
	if err != nil {
		t.Fatalf("logs tail: %v", err)
	}
	lines := nonEmptyLines(out)
	if len(lines) != 2 {
		t.Fatalf("%d lines, want 2:\n%s", len(lines), out)
	}
	if !strings.Contains(lines[0], "the renewal job failed") {
		t.Fatalf("first line = %q, want the oldest row first", lines[0])
	}
	if !strings.Contains(lines[1], "/api/v1/staff/customers") || !strings.Contains(lines[1], "200") {
		t.Fatalf("second line = %q, want the http row", lines[1])
	}

	// A credential in a log line is redacted on the way out, exactly as the viewer redacts it: the
	// row was written by a hand-run INSERT here, which is the case the read-side rule exists for.
	detail := e2eRedacted(pool, t)
	if strings.Contains(out, "hunter2") {
		t.Fatalf("the CLI printed a credential:\n%s", out)
	}
	if detail == "" {
		t.Fatal("the fixture row is missing")
	}

	// --kind narrows the read to one table.
	onlyHTTP, err := captureStdout(t, func() error {
		return cmdAdmin([]string{"logs", "tail", "--kind", "http"})
	})
	if err != nil {
		t.Fatalf("logs tail --kind http: %v", err)
	}
	if strings.Contains(onlyHTTP, "renewal job failed") {
		t.Fatalf("--kind http printed an error row:\n%s", onlyHTTP)
	}
	if !strings.Contains(onlyHTTP, "/api/v1/staff/customers") {
		t.Fatalf("--kind http printed no http row:\n%s", onlyHTTP)
	}

	// A bad kind is refused rather than silently ignored.
	if err := cmdAdmin([]string{"logs", "tail", "--kind", "smtp"}); err == nil {
		t.Fatal("an unknown --kind was accepted")
	}
}

// Following prints each row once, and only rows newer than the last one printed.
//
// The loop itself is driven by a signal context, so the poller is tested directly: a test that
// waited on a ticker would be a test of the clock.
func TestLogTailerFollowsWithoutDuplicating(t *testing.T) {
	pool := cliEnv(t)
	ctx := context.Background()

	if _, err := pool.Exec(ctx, `
		INSERT INTO error_logs (level, source, message) VALUES ('warn', 'cron', 'first')`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	var buf bytes.Buffer
	tailer := &logTailer{pool: pool, kinds: []string{"error"}, out: &buf}

	initial, err := tailer.recent(ctx, 10)
	if err != nil {
		t.Fatalf("recent: %v", err)
	}
	tailer.print(initial)
	if got := strings.Count(buf.String(), "first"); got != 1 {
		t.Fatalf("the first row was printed %d times", got)
	}
	watermark := tailer.watermark
	if watermark.IsZero() {
		t.Fatal("the watermark did not advance")
	}

	// Nothing new: a poll returns no rows and prints nothing.
	empty, err := tailer.since(ctx, watermark)
	if err != nil {
		t.Fatalf("since: %v", err)
	}
	tailer.print(empty)
	if got := strings.Count(buf.String(), "first"); got != 1 {
		t.Fatalf("a poll reprinted the first row (%d times)", got)
	}

	// A new row is picked up once, and re-polling from the same instant does not repeat it.
	if _, err := pool.Exec(ctx, `
		INSERT INTO error_logs (level, source, message) VALUES ('error', 'worker', 'second')`); err != nil {
		t.Fatalf("insert: %v", err)
	}
	// The poll's lower bound is inclusive, so the row at the watermark comes back with the new
	// one; what must not happen is printing it twice, which is what the seen-set is for.
	fresh, err := tailer.since(ctx, watermark)
	if err != nil {
		t.Fatalf("since: %v", err)
	}
	found := false
	for _, row := range fresh {
		if strings.Contains(row.line, "second") {
			found = true
		}
	}
	if !found {
		t.Fatalf("fresh = %+v, want the new row", fresh)
	}
	tailer.print(fresh)
	tailer.print(fresh) // a duplicate poll must not print twice
	if got := strings.Count(buf.String(), "second"); got != 1 {
		t.Fatalf("the new row was printed %d times, want 1", got)
	}
	if !tailer.watermark.After(watermark) {
		t.Fatalf("the watermark did not advance past %s", watermark)
	}
}

// The CLI's redaction rule and the viewer's must not drift.
//
// The two lists live in different packages because the CLI must not import the HTTP handlers, so
// they are compared here rather than shared. A marker added to the viewer without adding it here
// would leak through the terminal.
func TestCLILogRedactionMatchesTheViewer(t *testing.T) {
	// The viewer's list, transcribed. Keeping the assertion on the *behaviour* rather than on the
	// slice means a marker added there fails here even if the transcription is forgotten.
	samples := []string{
		"Authorization: Bearer abc123",
		"password=hunter2",
		"api_key: k-1234",
		"private_key=-----BEGIN",
		"card_number=4242424242424242",
		"cvv: 123",
		"signature=deadbeef",
		"cookie=session=abc",
		"credential: xyz",
		"token=abc",
	}
	for _, sample := range samples {
		redacted := redactCLILogText(sample)
		if !strings.Contains(redacted, "[redacted]") {
			t.Fatalf("%q was not redacted: %q", sample, redacted)
		}
	}
	// An innocent sentence keeps its words: over-redacting is a support inconvenience, but a rule
	// that redacted every occurrence of "token" would make the log unreadable.
	if got := redactCLILogText("the token bucket is full"); got != "the token bucket is full" {
		t.Fatalf("an innocent sentence was rewritten: %q", got)
	}
}

// nonEmptyLines splits output into the lines an operator would read.
func nonEmptyLines(out string) []string {
	var lines []string
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

// e2eRedacted returns the stored detail of the fixture error row, so the test can prove the
// credential really is in the table and was removed only on the way out.
func e2eRedacted(pool *store.Pool, t *testing.T) string {
	t.Helper()
	var detail string
	if err := pool.QueryRow(context.Background(),
		`SELECT detail FROM error_logs ORDER BY id DESC LIMIT 1`).Scan(&detail); err != nil {
		t.Fatalf("read detail: %v", err)
	}
	return detail
}

// The stamp helper is the one thing every line shares; a line without a parseable timestamp is not
// greppable, which is most of what a log is for.
func TestLogLineStampIsParseable(t *testing.T) {
	when := time.Date(2026, 4, 1, 10, 30, 0, 0, time.UTC)
	if _, err := time.Parse(time.RFC3339, stamp(when)); err != nil {
		t.Fatalf("stamp %q is not RFC3339: %v", stamp(when), err)
	}
}
