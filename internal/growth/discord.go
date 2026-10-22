package growth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/catalyst/billing/internal/store"
	"github.com/jackc/pgx/v5"
)

// Discord notifications (GRW-04, NTF-13).
//
// A webhook is an outbound POST of a "rich embed" — a title, a colour, a set of fields and
// a footer — to a URL the operator pasted into settings. Four decisions matter:
//
//   - The URL is a credential. It is stored with `is_secret = true` (migration 0024), it
//     is read only here, and PublicConfig is the only shape a response may carry: it says
//     whether a webhook is configured, never what it is.
//
//   - Notify is a *best-effort side effect* of a committed change. It is deliberately not
//     called inside a transaction: a slow or failing Discord must never roll back a
//     payment. The caller invokes it after commit, and a failure is logged, not surfaced.
//
//   - The event selection is a set in the `events` setting, so an operator chooses which
//     events reach a channel. An event not in the set posts nothing, and a disabled config
//     posts nothing — the two are separate so "off" cannot be defeated by a leftover list.
//
//   - No retries. A webhook is a notification, not a delivery guarantee; retrying a
//     failed post turns a Discord outage into a queue that outlives it. The durable
//     channel is internal/notify's outbox, and Discord is the convenience mirror.

// Discord setting keys, in the 'discord' group.
const (
	settingDiscordEnabled    = "enabled"
	settingDiscordWebhookURL = "webhook_url"
	settingDiscordEvents     = "events"
)

// Event keys a caller may pass to Notify. The set is closed so a typo in a call site is a
// value that can never be selected rather than a silent post.
const (
	EventInvoicePaid     = "invoice_paid"
	EventInvoiceIssued   = "invoice_issued"
	EventOrderPaid       = "order_paid"
	EventServiceCreated  = "service_created"
	EventServiceSuspended = "service_suspended"
	EventServiceTerminated = "service_terminated"
	EventTicketOpened    = "ticket_opened"
	EventRefundIssued    = "refund_issued"
	EventCustomerCreated = "customer_created"
	EventAffiliateCommission = "affiliate_commission"
)

// KnownEvents is every event key Notify understands, in the order the settings screen
// should offer them.
var KnownEvents = []string{
	EventInvoicePaid,
	EventInvoiceIssued,
	EventOrderPaid,
	EventServiceCreated,
	EventServiceSuspended,
	EventServiceTerminated,
	EventTicketOpened,
	EventRefundIssued,
	EventCustomerCreated,
	EventAffiliateCommission,
}

// knownEvent reports whether key is one of KnownEvents.
func knownEvent(key string) bool {
	for _, k := range KnownEvents {
		if k == key {
			return true
		}
	}
	return false
}

// DiscordConfig is the operator's Discord configuration, with the secret withheld.
//
// It is the only shape a settings read may return. WebhookConfigured is a boolean rather
// than the URL: a UI needs to show "configured" and cannot be trusted (or allowed) to
// hold the credential.
type DiscordConfig struct {
	Enabled bool `json:"enabled"`
	// Events is the selection. An empty slice means "notify for nothing".
	Events []string `json:"events"`
	// WebhookConfigured reports whether a URL is stored, never the URL itself.
	WebhookConfigured bool `json:"webhook_configured"`
}

// discordSettings is the internal read, including the secret. It is unexported so no
// handler can accidentally serialise it.
type discordSettings struct {
	enabled    bool
	webhookURL string
	events     map[string]struct{}
}

// readDiscordSettings loads the config. A missing row is the zero value, which is
// disabled: a fresh installation posts nothing until an operator turns it on.
func readDiscordSettings(ctx context.Context, q Querier) (discordSettings, error) {
	var out discordSettings
	rows, err := q.Query(ctx, `
		SELECT key, value FROM settings WHERE group_key = 'discord'`)
	if err != nil {
		return out, fmt.Errorf("growth: read discord settings: %w", err)
	}
	defer rows.Close()

	events := map[string]struct{}{}
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return out, fmt.Errorf("growth: scan discord setting: %w", err)
		}
		switch key {
		case settingDiscordEnabled:
			out.enabled = strings.EqualFold(strings.TrimSpace(value), "true")
		case settingDiscordWebhookURL:
			out.webhookURL = strings.TrimSpace(value)
		case settingDiscordEvents:
			// A malformed array is a configuration error, and the safe reading of a broken
			// selection is "nothing selected" rather than "everything selected".
			var list []string
			if err := json.Unmarshal([]byte(value), &list); err != nil {
				continue
			}
			for _, e := range list {
				events[strings.TrimSpace(e)] = struct{}{}
			}
		}
	}
	if err := rows.Err(); err != nil {
		return out, fmt.Errorf("growth: read discord settings: %w", err)
	}
	out.events = events
	return out, nil
}

// DiscordConfigFor returns the public config. It never returns the webhook URL.
func DiscordConfigFor(ctx context.Context, pool *store.Pool) (DiscordConfig, error) {
	cfg, err := readDiscordSettings(ctx, pool)
	if err != nil {
		return DiscordConfig{}, err
	}
	return cfg.public(), nil
}

// public projects the internal config onto the safe shape.
func (d discordSettings) public() DiscordConfig {
	events := make([]string, 0, len(d.events))
	for _, k := range KnownEvents {
		if _, ok := d.events[k]; ok {
			events = append(events, k)
		}
	}
	return DiscordConfig{
		Enabled:           d.enabled,
		Events:            events,
		WebhookConfigured: d.webhookURL != "",
	}
}

// DiscordNotifier posts rich embeds to the configured webhook.
//
// It is a struct with an injectable HTTP client so a test can assert exactly what would
// be posted (and assert that a disabled config posts nothing at all).
type DiscordNotifier struct {
	pool *store.Pool
	// Client is the HTTP client. A nil Client uses a bounded default; the timeout is
	// deliberately short because this call is on a request path and Discord being slow
	// must not make the panel slow.
	Client *http.Client
	// Now is injectable for the embed timestamp.
	Now func() time.Time
	// Logf receives one line per failed post. A failure here is not an error the caller
	// can act on, so it is logged rather than returned.
	Logf func(format string, args ...any)
}

// NewDiscordNotifier constructs a notifier.
func NewDiscordNotifier(pool *store.Pool) *DiscordNotifier {
	return &DiscordNotifier{
		pool: pool,
		Client: &http.Client{
			Timeout: 5 * time.Second,
			// A webhook is an absolute URL; following a redirect from it would let a
			// compromised config point at an internal address. Discord does not redirect.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		Now:  func() time.Time { return time.Now().UTC() },
		Logf: func(string, ...any) {},
	}
}

// Embed is the rich embed a caller supplies. It maps directly onto Discord's embed
// object, minus the fields this product never sets (images, video, author icon).
type Embed struct {
	Title       string
	Description string
	// URL makes the title clickable.
	URL string
	// Colour is a 24-bit integer. Zero means Discord's default.
	Colour int
	Fields []EmbedField
	// Footer is the small text at the bottom, typically the panel's name.
	Footer string
}

// EmbedField is one name/value pair inside an embed.
type EmbedField struct {
	Name   string
	Value  string
	Inline bool
}

// Payload is what a caller passes to Notify.
type Payload struct {
	// Title is the notification headline ("Invoice INV-000123 paid").
	Title string
	// Description is the body.
	Description string
	// URL deep-links to the record in the panel.
	URL string
	// Fields are the facts worth scanning: customer, amount, currency, order.
	Fields []EmbedField
	// Colour overrides the event's default colour when non-zero.
	Colour int
}

// eventColour is the default embed colour per event, so a channel is readable at a
// glance: green for money in, red for money out, orange for lifecycle.
func eventColour(event string) int {
	switch event {
	case EventInvoicePaid, EventOrderPaid, EventAffiliateCommission:
		return 0x2ECC71 // green
	case EventRefundIssued:
		return 0xE74C3C // red
	case EventServiceSuspended, EventServiceTerminated:
		return 0xE67E22 // orange
	case EventTicketOpened:
		return 0x3498DB // blue
	default:
		return 0x95A5A6 // grey
	}
}

// discordMessage is the webhook body.
type discordMessage struct {
	Username string        `json:"username,omitempty"`
	Embeds   []discordEmbed `json:"embeds"`
}

type discordEmbed struct {
	Title       string              `json:"title,omitempty"`
	Description string              `json:"description,omitempty"`
	URL         string              `json:"url,omitempty"`
	Colour      int                 `json:"color,omitempty"`
	Fields      []discordEmbedField `json:"fields,omitempty"`
	Footer      *discordFooter      `json:"footer,omitempty"`
	Timestamp   string              `json:"timestamp,omitempty"`
}

type discordEmbedField struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Inline bool   `json:"inline,omitempty"`
}

type discordFooter struct {
	Text string `json:"text"`
}

// ErrDiscordDisabled is returned by Notify when the config is off, the webhook is empty,
// or the event is not selected. Callers treat it as success (nothing to do).
var ErrDiscordDisabled = errors.New("growth: discord notifications are not enabled for this event")

// Notify posts an embed for eventKey, if the config selects it.
//
// It reads the config on every call rather than caching it: an operator who disables the
// webhook expects the next event to be silent, and a cache would keep posting until
// restart. The read is one indexed query against a table of a handful of rows.
//
// ErrDiscordDisabled means "correctly did nothing". Any other error means the post was
// attempted and failed, and the caller should log it — it still must not fail the request
// that triggered the notification.
func (n *DiscordNotifier) Notify(ctx context.Context, eventKey string, p Payload) error {
	if n == nil || n.pool == nil {
		return ErrDiscordDisabled
	}
	if !knownEvent(eventKey) {
		// An unknown key is a programming error, but posting it would be worse than
		// ignoring it: it would leak a value the operator never selected.
		return fmt.Errorf("%w: unknown event %q", ErrDiscordDisabled, eventKey)
	}
	cfg, err := readDiscordSettings(ctx, n.pool)
	if err != nil {
		return err
	}
	if !cfg.enabled || cfg.webhookURL == "" {
		return ErrDiscordDisabled
	}
	if _, ok := cfg.events[eventKey]; !ok {
		return ErrDiscordDisabled
	}

	colour := p.Colour
	if colour == 0 {
		colour = eventColour(eventKey)
	}
	msg := discordMessage{
		Username: "Catalyst Billing",
		Embeds: []discordEmbed{{
			Title:       p.Title,
			Description: p.Description,
			URL:         p.URL,
			Colour:      colour,
			Fields:      toDiscordFields(p.Fields),
			Footer:      &discordFooter{Text: eventKey},
			Timestamp:   n.Now().Format(time.RFC3339),
		}},
	}
	body, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("growth: encode discord payload: %w", err)
	}

	client := n.Client
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.webhookURL,
		bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("growth: build discord request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "CatalystBilling/1.0")

	resp, err := client.Do(req)
	if err != nil {
		n.logf("discord webhook post failed for %s: %v", eventKey, err)
		return fmt.Errorf("growth: post discord webhook: %w", err)
	}
	defer resp.Body.Close()
	// Drain a bounded amount so the connection can be reused; the body is not otherwise
	// interesting, and reading it unbounded from an untrusted endpoint is a hazard.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		n.logf("discord webhook rejected %s: status %d", eventKey, resp.StatusCode)
		return fmt.Errorf("growth: discord webhook returned status %d", resp.StatusCode)
	}
	return nil
}

func (n *DiscordNotifier) logf(format string, args ...any) {
	if n.Logf != nil {
		n.Logf(format, args...)
	}
}

// toDiscordFields maps the caller's fields, dropping empties (Discord rejects an embed
// field with an empty name or value, which would make the whole post fail).
func toDiscordFields(in []EmbedField) []discordEmbedField {
	out := make([]discordEmbedField, 0, len(in))
	for _, f := range in {
		if strings.TrimSpace(f.Name) == "" || strings.TrimSpace(f.Value) == "" {
			continue
		}
		out = append(out, discordEmbedField{Name: f.Name, Value: f.Value, Inline: f.Inline})
	}
	return out
}

// SetDiscordConfig writes the operator's configuration.
//
// It is the writer behind the settings surface. The webhook URL is marked secret here
// rather than at the call site so a second caller cannot forget: a settings row that is
// not secret is a credential that will be echoed back by the next generic settings read.
func SetDiscordConfig(ctx context.Context, tx pgx.Tx, enabled bool, webhookURL string, events []string) error {
	if tx == nil {
		return fmt.Errorf("%w: setting a config needs a transaction", ErrInvalidInput)
	}
	// Validate the selection: an unknown event key in the list is dropped rather than
	// stored, so the settings screen cannot become a list of keys Notify ignores.
	clean := make([]string, 0, len(events))
	for _, e := range events {
		e = strings.TrimSpace(e)
		if knownEvent(e) {
			clean = append(clean, e)
		}
	}
	encoded, err := json.Marshal(clean)
	if err != nil {
		return fmt.Errorf("growth: encode discord events: %w", err)
	}

	upsert := func(key, value string, secret bool) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO settings (group_key, key, value, is_secret, updated_at)
			VALUES ('discord', $1, $2, $3, now())
			ON CONFLICT (group_key, key) DO UPDATE
				SET value = EXCLUDED.value,
				    is_secret = EXCLUDED.is_secret,
				    updated_at = now()`, key, value, secret)
		if err != nil {
			return fmt.Errorf("growth: write discord setting %s: %w", key, err)
		}
		return nil
	}
	flag := "false"
	if enabled {
		flag = "true"
	}
	if err := upsert(settingDiscordEnabled, flag, false); err != nil {
		return err
	}
	if err := upsert(settingDiscordEvents, string(encoded), false); err != nil {
		return err
	}
	// Only write the URL when one was supplied: an empty value means "keep the existing
	// credential", because the settings screen cannot show the URL back to the operator
	// and therefore cannot round-trip it. Clearing the webhook is an explicit "" with
	// enabled=false, which is the documented way to remove it.
	if strings.TrimSpace(webhookURL) != "" {
		if err := upsert(settingDiscordWebhookURL, strings.TrimSpace(webhookURL), true); err != nil {
			return err
		}
	}
	return nil
}
