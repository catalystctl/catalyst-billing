package growth_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/catalyst/billing/internal/growth"
	"github.com/catalyst/billing/internal/store"
	"github.com/catalyst/billing/internal/store/storetest"
	"github.com/jackc/pgx/v5"
)

// discordEnv is a migrated database plus a notifier pointed at a fake webhook.
type discordEnv struct {
	*testing.T
	ctx      context.Context
	pool     *store.Pool
	notifier *growth.DiscordNotifier
	// posts counts how many times the fake webhook was called.
	posts *int
	// lastBody is the last webhook body received.
	lastBody *string
	// status is the status the fake webhook answers with.
	status int
	// server is the fake Discord endpoint.
	server *httptest.Server
}

// newDiscordEnv builds a notifier whose HTTP client is the fake endpoint.
func newDiscordEnv(t *testing.T) *discordEnv {
	t.Helper()
	pool := storetest.DB(t)
	env := &discordEnv{
		T:        t,
		ctx:      context.Background(),
		pool:     pool,
		posts:    new(int),
		lastBody: new(string),
		status:   http.StatusNoContent,
	}

	var mu sync.Mutex
	env.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 64<<10))
		mu.Lock()
		*env.posts++
		*env.lastBody = string(body)
		mu.Unlock()
		w.WriteHeader(env.status)
	}))
	t.Cleanup(env.server.Close)

	env.notifier = growth.NewDiscordNotifier(pool)
	env.notifier.Client = env.server.Client()
	return env
}

// configure writes the settings rows directly, which is what the settings surface does.
func (e *discordEnv) configure(t *testing.T, enabled bool, url string, events ...string) {
	t.Helper()
	err := e.pool.InTx(e.ctx, func(ctx context.Context, tx pgx.Tx) error {
		return growth.SetDiscordConfig(ctx, tx, enabled, url, events)
	})
	if err != nil {
		t.Fatalf("set discord config: %v", err)
	}
}

// A disabled configuration posts nothing at all: not a request, not a connection.
func TestDiscordDisabledPostsNothing(t *testing.T) {
	env := newDiscordEnv(t)
	env.configure(t, false, env.server.URL, growth.EventInvoicePaid)

	err := env.notifier.Notify(env.ctx, growth.EventInvoicePaid, growth.Payload{
		Title: "Invoice INV-1 paid",
	})
	if !errors.Is(err, growth.ErrDiscordDisabled) {
		t.Fatalf("err = %v, want ErrDiscordDisabled", err)
	}
	if *env.posts != 0 {
		t.Fatalf("%d posts, want 0 for a disabled config", *env.posts)
	}
}

// Enabled but with no webhook URL posts nothing: "on" without a destination is not a
// destination.
func TestDiscordEnabledWithoutURLPostsNothing(t *testing.T) {
	env := newDiscordEnv(t)
	env.configure(t, true, "", growth.EventInvoicePaid)

	if err := env.notifier.Notify(env.ctx, growth.EventInvoicePaid, growth.Payload{
		Title: "Invoice INV-1 paid",
	}); !errors.Is(err, growth.ErrDiscordDisabled) {
		t.Fatalf("err = %v, want ErrDiscordDisabled", err)
	}
	if *env.posts != 0 {
		t.Fatalf("%d posts, want 0 without a URL", *env.posts)
	}
}

// An event that is not selected posts nothing, even when the config is enabled. This is
// the "selectable events" half of GRW-04.
func TestDiscordUnselectedEventPostsNothing(t *testing.T) {
	env := newDiscordEnv(t)
	env.configure(t, true, env.server.URL, growth.EventInvoicePaid)

	if err := env.notifier.Notify(env.ctx, growth.EventTicketOpened, growth.Payload{
		Title: "Ticket opened",
	}); !errors.Is(err, growth.ErrDiscordDisabled) {
		t.Fatalf("err = %v, want ErrDiscordDisabled", err)
	}
	if *env.posts != 0 {
		t.Fatalf("%d posts, want 0 for an unselected event", *env.posts)
	}
}

// A selected event with an enabled config posts one rich embed carrying the title, the
// fields and the event as footer text.
func TestDiscordPostsRichEmbedForSelectedEvent(t *testing.T) {
	env := newDiscordEnv(t)
	env.configure(t, true, env.server.URL, growth.EventInvoicePaid, growth.EventTicketOpened)

	err := env.notifier.Notify(env.ctx, growth.EventInvoicePaid, growth.Payload{
		Title:       "Invoice INV-000123 paid",
		Description: "The customer paid in full.",
		URL:         "https://panel.example.test/invoices/abc",
		Fields: []growth.EmbedField{
			{Name: "Customer", Value: "Ada Lovelace", Inline: true},
			{Name: "Amount", Value: "123.45 USD", Inline: true},
			// An empty field must be dropped: Discord rejects the whole embed otherwise.
			{Name: "Empty", Value: ""},
		},
	})
	if err != nil {
		t.Fatalf("notify: %v", err)
	}
	if *env.posts != 1 {
		t.Fatalf("%d posts, want 1", *env.posts)
	}

	var msg struct {
		Username string `json:"username"`
		Embeds   []struct {
			Title       string `json:"title"`
			Description string `json:"description"`
			URL         string `json:"url"`
			Color       int    `json:"color"`
			Fields      []struct {
				Name   string `json:"name"`
				Value  string `json:"value"`
				Inline bool   `json:"inline"`
			} `json:"fields"`
			Footer struct {
				Text string `json:"text"`
			} `json:"footer"`
			Timestamp string `json:"timestamp"`
		} `json:"embeds"`
	}
	if err := json.Unmarshal([]byte(*env.lastBody), &msg); err != nil {
		t.Fatalf("decode body %q: %v", *env.lastBody, err)
	}
	if len(msg.Embeds) != 1 {
		t.Fatalf("%d embeds, want 1", len(msg.Embeds))
	}
	embed := msg.Embeds[0]
	if embed.Title != "Invoice INV-000123 paid" {
		t.Fatalf("title = %q", embed.Title)
	}
	if embed.URL != "https://panel.example.test/invoices/abc" {
		t.Fatalf("url = %q", embed.URL)
	}
	if len(embed.Fields) != 2 {
		t.Fatalf("%d fields, want 2 (the empty one is dropped): %+v", len(embed.Fields), embed.Fields)
	}
	if embed.Footer.Text != growth.EventInvoicePaid {
		t.Fatalf("footer = %q, want the event key", embed.Footer.Text)
	}
	if embed.Timestamp == "" {
		t.Fatal("the embed has no timestamp")
	}
}

// A non-2xx from Discord is an error the caller logs, and it is not ErrDiscordDisabled:
// the difference between "did nothing" and "tried and failed" matters to an operator.
func TestDiscordFailureIsReported(t *testing.T) {
	env := newDiscordEnv(t)
	env.status = http.StatusInternalServerError
	env.configure(t, true, env.server.URL, growth.EventInvoicePaid)

	err := env.notifier.Notify(env.ctx, growth.EventInvoicePaid, growth.Payload{Title: "x"})
	if err == nil {
		t.Fatal("a 500 from the webhook was reported as success")
	}
	if errors.Is(err, growth.ErrDiscordDisabled) {
		t.Fatalf("a failed post was reported as disabled: %v", err)
	}
	if *env.posts != 1 {
		t.Fatalf("%d posts, want 1 attempt", *env.posts)
	}
}

// An unknown event key is never posted: it would leak a value the operator did not select.
func TestDiscordUnknownEventNeverPosts(t *testing.T) {
	env := newDiscordEnv(t)
	env.configure(t, true, env.server.URL, growth.EventInvoicePaid)

	if err := env.notifier.Notify(env.ctx, "made_up_event", growth.Payload{Title: "x"}); err == nil {
		t.Fatal("an unknown event key was accepted")
	}
	if *env.posts != 0 {
		t.Fatalf("%d posts, want 0 for an unknown event", *env.posts)
	}
}

// The public config never carries the webhook URL: it reports only whether one is stored.
func TestDiscordConfigRedactsWebhook(t *testing.T) {
	env := newDiscordEnv(t)
	env.configure(t, true, "https://discord.example.test/api/webhooks/123/secret-token",
		growth.EventInvoicePaid, growth.EventOrderPaid)

	cfg, err := growth.DiscordConfigFor(env.ctx, env.pool)
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	if !cfg.Enabled {
		t.Fatal("enabled = false, want true")
	}
	if !cfg.WebhookConfigured {
		t.Fatal("webhook_configured = false, want true")
	}
	if len(cfg.Events) != 2 {
		t.Fatalf("events = %v, want 2", cfg.Events)
	}

	// Marshal it and prove the secret is not in the JSON: a struct without the field is
	// the guarantee, and this asserts the guarantee rather than trusting the struct.
	encoded, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(encoded), "secret-token") ||
		strings.Contains(string(encoded), "discord.example.test") {
		t.Fatalf("the public config leaked the webhook: %s", encoded)
	}

	// The row is marked secret in the database, so a generic settings read that honours the
	// flag redacts it too.
	var secret bool
	if err := env.pool.QueryRow(env.ctx, `
		SELECT is_secret FROM settings WHERE group_key = 'discord' AND key = 'webhook_url'`,
	).Scan(&secret); err != nil {
		t.Fatalf("read is_secret: %v", err)
	}
	if !secret {
		t.Fatal("the webhook_url setting is not marked secret")
	}
}

// A malformed events array reads as "nothing selected", not "everything": a broken
// selection must not widen what gets posted.
func TestDiscordMalformedEventsSelectsNothing(t *testing.T) {
	env := newDiscordEnv(t)
	if _, err := env.pool.Exec(env.ctx, `
		UPDATE settings SET value = 'not json' WHERE group_key = 'discord' AND key = 'events'`,
	); err != nil {
		t.Fatalf("corrupt events: %v", err)
	}
	env.configure(t, true, env.server.URL, growth.EventInvoicePaid)
	// configure rewrote a valid array, so corrupt it again after.
	if _, err := env.pool.Exec(env.ctx, `
		UPDATE settings SET value = 'not json' WHERE group_key = 'discord' AND key = 'events'`,
	); err != nil {
		t.Fatalf("corrupt events: %v", err)
	}

	if err := env.notifier.Notify(env.ctx, growth.EventInvoicePaid, growth.Payload{Title: "x"}); !errors.Is(err, growth.ErrDiscordDisabled) {
		t.Fatalf("err = %v, want ErrDiscordDisabled for a malformed selection", err)
	}
	if *env.posts != 0 {
		t.Fatalf("%d posts, want 0", *env.posts)
	}
}
