package audit_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/catalyst/billing/internal/audit"
	"github.com/catalyst/billing/internal/store"
	"github.com/catalyst/billing/internal/store/storetest"
	"github.com/jackc/pgx/v5"
)

// env is a pool plus one user, since actor_id is a foreign key.
type env struct {
	pool   *store.Pool
	userID string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	pool := storetest.DB(t)
	var id string
	if err := pool.QueryRow(context.Background(), `
		INSERT INTO users (email, password_hash, email_verified_at)
		VALUES ('actor@example.test', 'x', now()) RETURNING id`).Scan(&id); err != nil {
		t.Fatalf("create user: %v", err)
	}
	return &env{pool: pool, userID: id}
}

// record writes one event in a transaction.
func (e *env) record(t *testing.T, ev audit.Event) {
	t.Helper()
	err := e.pool.InTx(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return audit.Record(ctx, tx, ev)
	})
	if err != nil {
		t.Fatalf("record: %v", err)
	}
}

func TestRecordAndReadBack(t *testing.T) {
	e := newEnv(t)
	e.record(t, audit.Event{
		Actor:         audit.Actor{ID: e.userID, Kind: audit.ActorUser},
		Action:        "invoice.void",
		TargetType:    "invoice",
		TargetID:      "0f8fad5b-d9cb-469f-a165-70867728950e",
		Outcome:       audit.OutcomeSuccess,
		Changes:       map[string]any{"reason": "billed twice", "status": "void"},
		CorrelationID: "abc123",
		IP:            "203.0.113.7",
	})

	res, err := audit.List(context.Background(), e.pool, audit.Filter{Action: "invoice.void"}, 10, "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(res.Rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(res.Rows))
	}
	row := res.Rows[0]
	if row.ActorID != e.userID || row.ActorKind != audit.ActorUser {
		t.Fatalf("actor = %s/%s", row.ActorID, row.ActorKind)
	}
	if row.TargetType != "invoice" || row.Outcome != audit.OutcomeSuccess {
		t.Fatalf("row = %+v", row)
	}
	if row.Changes["reason"] != "billed twice" {
		t.Fatalf("changes = %v", row.Changes)
	}
	if row.CorrelationID != "abc123" {
		t.Fatalf("correlation = %q", row.CorrelationID)
	}
	if row.IP != "203.0.113.7" {
		t.Fatalf("ip = %q", row.IP)
	}
}

// A secret must never be stored, even when a caller puts one in the changes map. This is
// the property that makes the table safe to read during an incident.
func TestSecretsAreRedacted(t *testing.T) {
	e := newEnv(t)
	e.record(t, audit.Event{
		Actor:      audit.Actor{ID: e.userID, Kind: audit.ActorUser},
		Action:     "user.password_change",
		TargetType: "user",
		TargetID:   e.userID,
		Outcome:    audit.OutcomeSuccess,
		Changes: map[string]any{
			"password":      "hunter2",
			"new_password":  "correct horse",
			"api_key":       "bk_abcdefghijklmnop",
			"csrf_token":    "t0ken",
			"reason":        "user asked",
			"nested":        map[string]any{"authorization": "Bearer abc", "kept": "yes"},
			"recovery_code": "1234-5678",
		},
	})

	res, err := audit.List(context.Background(), e.pool, audit.Filter{Action: "user.password_change"}, 10, "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(res.Rows) != 1 {
		t.Fatalf("rows = %d", len(res.Rows))
	}
	changes := res.Rows[0].Changes
	for _, key := range []string{"password", "new_password", "api_key", "csrf_token", "recovery_code"} {
		if changes[key] != audit.RedactedMarker {
			t.Fatalf("%s = %v, want the redaction marker", key, changes[key])
		}
	}
	if changes["reason"] != "user asked" {
		t.Fatalf("an innocent field was redacted: %v", changes["reason"])
	}
	nested, ok := changes["nested"].(map[string]any)
	if !ok {
		t.Fatalf("nested = %T", changes["nested"])
	}
	if nested["authorization"] != audit.RedactedMarker || nested["kept"] != "yes" {
		t.Fatalf("nested redaction = %v", nested)
	}
}

// The table is append-only, which is what makes it evidence.
func TestAuditEventsCannotBeMutated(t *testing.T) {
	e := newEnv(t)
	e.record(t, audit.Event{
		Actor:      audit.Actor{ID: e.userID, Kind: audit.ActorUser},
		Action:     "invoice.issue",
		TargetType: "invoice",
		Outcome:    audit.OutcomeSuccess,
	})

	ctx := context.Background()
	if _, err := e.pool.Exec(ctx, `UPDATE audit_events SET outcome = 'failure'`); err == nil {
		t.Fatal("an audit event was updated")
	}
	if _, err := e.pool.Exec(ctx, `DELETE FROM audit_events`); err == nil {
		t.Fatal("an audit event was deleted")
	}
}

// A system action is attributable to a kind without naming a person.
func TestSystemActor(t *testing.T) {
	e := newEnv(t)
	e.record(t, audit.Event{
		Actor:      audit.Actor{Kind: audit.ActorSystem},
		Action:     "renewal.run",
		TargetType: "subscription",
		Outcome:    audit.OutcomeSuccess,
	})
	res, err := audit.List(context.Background(), e.pool, audit.Filter{Action: "renewal.run"}, 10, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rows) != 1 || res.Rows[0].ActorKind != audit.ActorSystem || res.Rows[0].ActorID != "" {
		t.Fatalf("system row = %+v", res.Rows)
	}
}

// Invalid events must be refused while the caller can still fix them, rather than
// written as unfilterable noise.
func TestValidation(t *testing.T) {
	e := newEnv(t)
	cases := map[string]audit.Event{
		"no action": {
			Actor: audit.Actor{ID: e.userID, Kind: audit.ActorUser}, TargetType: "invoice",
			Outcome: audit.OutcomeSuccess,
		},
		"no target type": {
			Actor: audit.Actor{ID: e.userID, Kind: audit.ActorUser}, Action: "invoice.void",
			Outcome: audit.OutcomeSuccess,
		},
		"bad outcome": {
			Actor: audit.Actor{ID: e.userID, Kind: audit.ActorUser}, Action: "a", TargetType: "b",
			Outcome: "maybe",
		},
		"bad actor kind": {
			Actor: audit.Actor{ID: e.userID, Kind: "robot"}, Action: "a", TargetType: "b",
			Outcome: audit.OutcomeSuccess,
		},
		"system actor with an id": {
			Actor: audit.Actor{ID: e.userID, Kind: audit.ActorSystem}, Action: "a",
			TargetType: "b", Outcome: audit.OutcomeSuccess,
		},
	}
	for name, ev := range cases {
		err := e.pool.InTx(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
			return audit.Record(ctx, tx, ev)
		})
		if err == nil {
			t.Fatalf("%s was accepted", name)
		}
	}
}

// Pagination must not repeat or skip rows when a new event is inserted between pages.
func TestListPaginationIsStable(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		e.record(t, audit.Event{
			Actor:      audit.Actor{ID: e.userID, Kind: audit.ActorUser},
			Action:     "invoice.issue",
			TargetType: "invoice",
			Outcome:    audit.OutcomeSuccess,
			Changes:    map[string]any{"n": i},
		})
	}

	first, err := audit.List(ctx, e.pool, audit.Filter{Action: "invoice.issue"}, 2, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Rows) != 2 || first.NextCursor == "" {
		t.Fatalf("first page = %d rows, cursor %q", len(first.Rows), first.NextCursor)
	}

	// Insert a newer row between the two pages: it must not shift the window.
	e.record(t, audit.Event{
		Actor:      audit.Actor{ID: e.userID, Kind: audit.ActorUser},
		Action:     "invoice.issue",
		TargetType: "invoice",
		Outcome:    audit.OutcomeSuccess,
		Changes:    map[string]any{"n": "between"},
	})

	second, err := audit.List(ctx, e.pool, audit.Filter{Action: "invoice.issue"}, 2, first.NextCursor)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[any]bool{}
	for _, r := range first.Rows {
		seen[r.Changes["n"]] = true
	}
	for _, r := range second.Rows {
		if seen[r.Changes["n"]] {
			t.Fatalf("page two repeated row %v", r.Changes["n"])
		}
	}
	if len(second.Rows) != 2 {
		t.Fatalf("second page = %d rows, want 2", len(second.Rows))
	}
}

// A bad cursor is an error, never a silent reset to page one.
func TestListRejectsBadCursor(t *testing.T) {
	e := newEnv(t)
	if _, err := audit.List(context.Background(), e.pool, audit.Filter{}, 10, "!!not-a-cursor"); err == nil {
		t.Fatal("a malformed cursor was accepted")
	}
}

// Filtering by actor, target and date range narrows the result set.
func TestListFilters(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	target := "0f8fad5b-d9cb-469f-a165-70867728950e"
	e.record(t, audit.Event{
		Actor: audit.Actor{ID: e.userID, Kind: audit.ActorUser}, Action: "invoice.void",
		TargetType: "invoice", TargetID: target, Outcome: audit.OutcomeSuccess,
	})
	e.record(t, audit.Event{
		Actor: audit.Actor{Kind: audit.ActorSystem}, Action: "renewal.run",
		TargetType: "subscription", Outcome: audit.OutcomeSuccess,
	})

	byTarget, err := audit.List(ctx, e.pool, audit.Filter{TargetType: "invoice", TargetID: target}, 10, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(byTarget.Rows) != 1 || byTarget.Rows[0].Action != "invoice.void" {
		t.Fatalf("by target = %+v", byTarget.Rows)
	}

	byActor, err := audit.List(ctx, e.pool, audit.Filter{ActorID: e.userID}, 10, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(byActor.Rows) != 1 {
		t.Fatalf("by actor = %d rows", len(byActor.Rows))
	}

	future := time.Now().Add(time.Hour)
	if res, err := audit.List(ctx, e.pool, audit.Filter{From: &future}, 10, ""); err != nil {
		t.Fatal(err)
	} else if len(res.Rows) != 0 {
		t.Fatalf("future window returned %d rows", len(res.Rows))
	}
}

// A denied authorization is recorded with its own outcome, so "who tried and was
// refused" is answerable.
func TestDeniedOutcome(t *testing.T) {
	e := newEnv(t)
	e.record(t, audit.Event{
		Actor:      audit.Actor{ID: e.userID, Kind: audit.ActorUser},
		Action:     "refund.create",
		TargetType: "payment",
		Outcome:    audit.OutcomeDenied,
	})
	res, err := audit.List(context.Background(), e.pool, audit.Filter{Outcome: audit.OutcomeDenied}, 10, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rows) != 1 || res.Rows[0].Outcome != audit.OutcomeDenied {
		t.Fatalf("denied rows = %+v", res.Rows)
	}
}

// A failing insert must fail the caller's transaction: an audit row and the change it
// describes commit together or not at all.
func TestRecordFailureAbortsTransaction(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	err := e.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`INSERT INTO customers (email) VALUES ('rolled-back@example.test')`); err != nil {
			return err
		}
		// A non-existent actor violates the foreign key.
		return audit.Record(ctx, tx, audit.Event{
			Actor:      audit.Actor{ID: "11111111-1111-1111-1111-111111111111", Kind: audit.ActorUser},
			Action:     "customer.create",
			TargetType: "customer",
			Outcome:    audit.OutcomeSuccess,
		})
	})
	if err == nil {
		t.Fatal("an invalid audit row did not fail the transaction")
	}
	var n int
	if qerr := e.pool.QueryRow(ctx,
		`SELECT count(*) FROM customers WHERE email = 'rolled-back@example.test'`).Scan(&n); qerr != nil {
		t.Fatal(qerr)
	}
	if n != 0 {
		t.Fatal("the customer insert survived a failed audit write")
	}
}

// IsSecretField is the single implementation of the redaction rule; it must be stable.
func TestIsSecretField(t *testing.T) {
	for _, name := range []string{"password", "Password", "api_key", "X-CSRF-Token", "secret_enc"} {
		if !audit.IsSecretField(name) {
			t.Fatalf("IsSecretField(%q) = false", name)
		}
	}
	for _, name := range []string{"reason", "status", "amount_minor", "customer_id"} {
		if audit.IsSecretField(name) {
			t.Fatalf("IsSecretField(%q) = true", name)
		}
	}
}

var _ = errors.New
