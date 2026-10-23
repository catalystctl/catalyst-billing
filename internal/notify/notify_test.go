package notify_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/catalyst/billing/internal/notify"
	"github.com/catalyst/billing/internal/store"
	"github.com/catalyst/billing/internal/store/storetest"
	"github.com/jackc/pgx/v5"
)

// fakeSender records what would have been sent and can be told to fail.
type fakeSender struct {
	mu       sync.Mutex
	sent     []string
	failures int
	failWith error
}

func (f *fakeSender) Send(_ context.Context, to, subject, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failures > 0 {
		f.failures--
		return f.failWith
	}
	f.sent = append(f.sent, to+"|"+subject)
	return nil
}

func (f *fakeSender) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sent)
}

func newQueue(t *testing.T) (*notify.Queue, *store.Pool) {
	t.Helper()
	pool := storetest.DB(t)
	return notify.NewQueue(pool), pool
}

func enqueue(t *testing.T, q *notify.Queue, pool *store.Pool, m notify.Message) (string, bool) {
	t.Helper()
	var (
		id      string
		created bool
	)
	err := pool.InTx(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		var err error
		id, created, err = q.Enqueue(ctx, tx, m)
		return err
	})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	return id, created
}

func message() notify.Message {
	return notify.Message{
		Kind:      notify.KindInvoiceIssued,
		To:        "a@example.test",
		Subject:   "Invoice INV-000001",
		Body:      "Please pay.",
		DedupeKey: "invoice_issued:inv_1",
	}
}

// A queued message is durable before it is sent, and a duplicate dedupe key does not
// queue a second one: replaying an invoice must not send the mail twice.
func TestEnqueueIsDurableAndDeduplicated(t *testing.T) {
	q, pool := newQueue(t)
	if _, created := enqueue(t, q, pool, message()); !created {
		t.Fatal("the first enqueue reported no row")
	}
	if _, created := enqueue(t, q, pool, message()); created {
		t.Fatal("a duplicate dedupe key created a second row")
	}

	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM notification_deliveries`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("rows = %d, want 1", n)
	}
}

func TestRunOnceSendsAndMarksSent(t *testing.T) {
	q, pool := newQueue(t)
	id, _ := enqueue(t, q, pool, message())

	sender := &fakeSender{}
	res, err := q.RunOnce(context.Background(), sender, 10)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if res.Sent != 1 || res.Failed != 0 {
		t.Fatalf("result = %+v", res)
	}
	if sender.count() != 1 {
		t.Fatalf("sender saw %d messages", sender.count())
	}

	var (
		status string
		sentAt *time.Time
	)
	if err := pool.QueryRow(context.Background(), `
		SELECT status, sent_at FROM notification_deliveries WHERE id = $1`, id).
		Scan(&status, &sentAt); err != nil {
		t.Fatal(err)
	}
	if status != "sent" || sentAt == nil {
		t.Fatalf("status = %q, sent_at = %v", status, sentAt)
	}

	// A second pass must not send it again.
	if res, err := q.RunOnce(context.Background(), sender, 10); err != nil || res.Sent != 0 {
		t.Fatalf("second pass = %+v, %v", res, err)
	}
}

// A failed delivery is recorded with its cause and retried later, not lost.
func TestFailureIsRecordedAndRetried(t *testing.T) {
	q, pool := newQueue(t)
	id, _ := enqueue(t, q, pool, message())

	sender := &fakeSender{failures: 1, failWith: errors.New("smtp is down")}
	if _, err := q.RunOnce(context.Background(), sender, 10); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	var (
		status   string
		attempts int
		lastErr  *string
		nextAt   time.Time
	)
	if err := pool.QueryRow(context.Background(), `
		SELECT status, attempts, last_error, next_attempt_at
		FROM notification_deliveries WHERE id = $1`, id).
		Scan(&status, &attempts, &lastErr, &nextAt); err != nil {
		t.Fatal(err)
	}
	if status != "failed" || attempts != 1 {
		t.Fatalf("status = %q, attempts = %d", status, attempts)
	}
	if lastErr == nil || *lastErr == "" {
		t.Fatal("the failure cause was not recorded")
	}
	if !nextAt.After(time.Now()) {
		t.Fatalf("no backoff was applied: next attempt at %v", nextAt)
	}

	// Once the backoff has passed, the retry succeeds.
	if _, err := pool.Exec(context.Background(),
		`UPDATE notification_deliveries SET next_attempt_at = now() - interval '1 second' WHERE id = $1`,
		id); err != nil {
		t.Fatal(err)
	}
	res, err := q.RunOnce(context.Background(), sender, 10)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if res.Sent != 1 {
		t.Fatalf("retry sent %d", res.Sent)
	}
}

// Retries stop at MaxAttempts: a permanently failing address must not occupy the queue
// forever.
func TestRetriesAreBounded(t *testing.T) {
	q, pool := newQueue(t)
	q.MaxAttempts = 2
	enqueue(t, q, pool, message())

	sender := &fakeSender{failures: 10, failWith: errors.New("permanent")}
	for i := 0; i < 4; i++ {
		if _, err := pool.Exec(context.Background(),
			`UPDATE notification_deliveries SET next_attempt_at = now() - interval '1 second'`); err != nil {
			t.Fatal(err)
		}
		if _, err := q.RunOnce(context.Background(), sender, 10); err != nil {
			t.Fatal(err)
		}
	}

	var attempts int
	if err := pool.QueryRow(context.Background(),
		`SELECT attempts FROM notification_deliveries`).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	if attempts > q.MaxAttempts {
		t.Fatalf("attempts = %d, want at most %d", attempts, q.MaxAttempts)
	}
}

// A message that was deliberately not sent is recorded as suppressed, so "we never sent
// it" has an answer that is not a bug report.
func TestSuppressRecordsTheDecision(t *testing.T) {
	q, pool := newQueue(t)
	m := notify.Message{Kind: notify.KindInvoiceIssued, To: "unverified@example.test",
		Subject: "Invoice", Body: "body", DedupeKey: "invoice_issued:inv_2"}
	err := pool.InTx(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return q.Suppress(ctx, tx, m, "address is not verified")
	})
	if err != nil {
		t.Fatalf("Suppress: %v", err)
	}

	var status, reason string
	if err := pool.QueryRow(context.Background(), `
		SELECT status, COALESCE(last_error, '') FROM notification_deliveries`).Scan(&status, &reason); err != nil {
		t.Fatal(err)
	}
	if status != "suppressed" || reason == "" {
		t.Fatalf("status = %q, reason = %q", status, reason)
	}

	// A suppressed row is never picked up by the sender.
	sender := &fakeSender{}
	if res, err := q.RunOnce(context.Background(), sender, 10); err != nil || res.Sent != 0 {
		t.Fatalf("a suppressed row was sent: %+v, %v", res, err)
	}
}

func TestIncompleteMessageRefused(t *testing.T) {
	q, pool := newQueue(t)
	for name, m := range map[string]notify.Message{
		"no recipient": {Kind: notify.KindInvoiceIssued, Subject: "s", Body: "b"},
		"no kind":      {To: "a@example.test", Subject: "s", Body: "b"},
		"no subject":   {Kind: notify.KindInvoiceIssued, To: "a@example.test", Body: "b"},
		"no body":      {Kind: notify.KindInvoiceIssued, To: "a@example.test", Subject: "s"},
	} {
		err := pool.InTx(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
			_, _, err := q.Enqueue(ctx, tx, m)
			return err
		})
		if err == nil {
			t.Fatalf("%s was accepted", name)
		}
	}
}

// Templates are plain text: an attacker-controlled description must not become markup.
func TestTemplatesArePlainText(t *testing.T) {
	m := notify.PasswordResetMessage("a@example.test", "https://panel.example/reset?t=abc", 30*time.Minute)
	if m.Kind != notify.KindPasswordReset || m.To != "a@example.test" {
		t.Fatalf("message = %+v", m)
	}
	if !contains(m.Body, "https://panel.example/reset?t=abc") {
		t.Fatalf("body has no link: %s", m.Body)
	}
	if contains(m.Body, "<") {
		t.Fatalf("body contains markup: %s", m.Body)
	}

	// Invoice mail has no hardcoded constructor to assert on any more: it is rendered from the
	// editable `new_invoice_created` template, and the dedupe key that makes a retried issue
	// idempotent is asserted in events_test.go against the path that actually builds it.

	verify := notify.EmailVerificationMessage("a@example.test", "https://panel.example/v?t=x", time.Hour)
	if verify.Kind != notify.KindEmailVerification {
		t.Fatalf("kind = %q", verify.Kind)
	}
}

// A missing sender is a configuration error, not a silent no-op.
func TestRunOnceWithoutSenderFails(t *testing.T) {
	q, _ := newQueue(t)
	if _, err := q.RunOnce(context.Background(), nil, 10); err == nil {
		t.Fatal("RunOnce succeeded with no sender")
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (func() bool {
		for i := 0; i+len(needle) <= len(haystack); i++ {
			if haystack[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	})()
}
