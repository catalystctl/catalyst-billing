package notify

// Web push: a third channel beside mail and the in-app inbox.
//
// # Where it sits
//
// Push is deliberately *not* a parallel notification system. It reads the same template
// catalogue, obeys the same four-valued preference (force / choice_on / choice_off / never)
// through the same Decide function, and dedupes on the same event keys. What is different is
// only the transport: instead of one address there is a list of devices, and instead of SMTP
// there is an encrypted POST to a push service.
//
// The preference is shared rather than separate because that is the model a customer already
// understands: "I do not want to hear about failed payments" is one decision, not one per
// channel. If push had its own switch, the preferences screen would need a matrix and every
// customer would have to answer the same question three times.
//
// # Why delivery is not inside the caller's transaction
//
// Every other channel writes to the outbox inside the transaction that made the change, and a
// worker drains it afterwards. Push cannot follow that path here: the outbox has no channel
// column and adding one is a worker change, which belongs to the integration rather than to
// this package. So DeliverPush is called *after* commit, in the same way the cancellation
// notice and the order-placed notice already are, and it takes a Querier rather than a pgx.Tx
// so it can never be called inside one by accident.
//
// # A dead device is deleted, not retried
//
// A push service answering 404 or 410 is telling us the subscription no longer exists — the
// user cleared site data, uninstalled the browser, or the service expired it. Retrying cannot
// succeed and keeping the row means every future send pays for a doomed request. The row is
// deleted, the deletion is counted, and the caller sees it in PushResult.Removed so the portal
// can say "that device was removed" rather than silently showing a device that is gone.
//
// # The two secrets, and what is not stored
//
// A subscription is an endpoint, a P-256 public key and a 16-byte auth secret. The private key
// never leaves the browser, so what the database holds is useless on its own: it can encrypt a
// message to the device and cannot read anything. There is no token to revoke and no password
// to hash — the push service revokes the endpoint, and that is the lifecycle.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/catalyst/billing/internal/notify/webpush"
	"github.com/jackc/pgx/v5"
)

// MaxPushDevices bounds how many devices one account may register.
//
// The bound exists because every send walks the list: an account with two hundred subscriptions
// turns one invoice into two hundred outbound requests, and the person who created them is not
// the person who pays for the CPU. Twenty is generous for a person with a laptop, a phone, a
// tablet and a couple of old browsers they have not cleared.
const MaxPushDevices = 20

// Limits on what is stored. They are truncations rather than refusals for the user agent (a
// label nobody parses) and refusals for the secrets (a truncated key is a device that can never
// receive anything).
const (
	maxPushEndpointLen  = 2048
	maxPushUserAgentLen = 300
)

// Errors a caller branches on. Each maps to a stable problem code at the HTTP layer.
var (
	// ErrInvalidPushSubscription is a subscription that cannot work: a non-https endpoint, a
	// key that is not a P-256 point, an auth secret that is not 16 bytes.
	ErrInvalidPushSubscription = errors.New("notify: invalid push subscription")
	// ErrPushDeviceLimit is returned when an account already has MaxPushDevices registered and
	// is subscribing another device rather than replacing one.
	ErrPushDeviceLimit = errors.New("notify: too many push devices on this account")
	// ErrPushSubscriptionNotFound is returned for an id that does not belong to the caller. It
	// is the same error as "no such id", which is what stops the endpoint being an oracle.
	ErrPushSubscriptionNotFound = errors.New("notify: no such push subscription")
	// ErrPushNotConfigured means the installation has no VAPID key pair, so nothing can be
	// signed and no device can be offered the feature.
	ErrPushNotConfigured = errors.New("notify: web push is not configured")
)

// PushSubscription is one device's registration.
type PushSubscription struct {
	ID string
	// UserID is the account that will receive notifications on this device. It is not part of
	// the key: the endpoint identifies the device, and re-subscribing replaces the row and its
	// owner (see migration 0034).
	UserID string
	// Endpoint is the push service URL. https only.
	Endpoint string
	// P256DH is the device's public key, base64url, uncompressed P-256 point.
	P256DH string
	// Auth is the device's 16-byte auth secret, base64url.
	Auth string
	// UserAgent is what the browser said it was, truncated. A label, never parsed.
	UserAgent string
	CreatedAt time.Time
	// LastUsedAt is when a push was last handed to the push service. Nil until then, which is
	// how the portal tells a device that has never received anything from one that has.
	LastUsedAt *time.Time
}

// subscriptionColumns is the one place the column list is written, so the scan order and the
// INSERT/SELECT cannot drift apart.
const subscriptionColumns = `id, user_id, endpoint, p256dh, auth, user_agent, created_at, last_used_at`

// scanSubscription reads one row.
func scanSubscription(row interface{ Scan(...any) error }) (PushSubscription, error) {
	var s PushSubscription
	err := row.Scan(&s.ID, &s.UserID, &s.Endpoint, &s.P256DH, &s.Auth, &s.UserAgent,
		&s.CreatedAt, &s.LastUsedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return PushSubscription{}, ErrPushSubscriptionNotFound
	}
	if err != nil {
		return PushSubscription{}, fmt.Errorf("notify: read push subscription: %w", err)
	}
	return s, nil
}

// ValidatePushSubscription checks that a subscription could ever receive a message.
//
// The endpoint and the keys are validated by decoding them, not by matching a pattern: a
// base64url string of the right length that is not a point on P-256 is exactly the value that
// gets past a regex and then fails at send time, once per device, forever.
func ValidatePushSubscription(s PushSubscription) error {
	if s.UserID == "" {
		return fmt.Errorf("%w: the subscription has no owner", ErrInvalidPushSubscription)
	}
	endpoint := strings.TrimSpace(s.Endpoint)
	if endpoint == "" {
		return fmt.Errorf("%w: the endpoint is empty", ErrInvalidPushSubscription)
	}
	if len(endpoint) > maxPushEndpointLen {
		return fmt.Errorf("%w: the endpoint is longer than %d bytes",
			ErrInvalidPushSubscription, maxPushEndpointLen)
	}
	// Parsed rather than prefix-matched: "https:///send/x" has the prefix and no host, and a
	// host is what makes it an endpoint the server can POST to. The scheme check is the one
	// that matters — an http endpoint would send the VAPID token in the clear and let a
	// subscription point the server at a plaintext host of the caller's choosing.
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return fmt.Errorf("%w: the endpoint is not a URL: %v", ErrInvalidPushSubscription, err)
	}
	if parsed.Scheme != "https" {
		return fmt.Errorf("%w: the endpoint must be https", ErrInvalidPushSubscription)
	}
	if parsed.Host == "" {
		return fmt.Errorf("%w: the endpoint has no host", ErrInvalidPushSubscription)
	}
	if _, err := webpush.ParsePublicKey(s.P256DH); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidPushSubscription, err)
	}
	if err := webpush.ValidateAuthSecret(s.Auth); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidPushSubscription, err)
	}
	return nil
}

// SavePushSubscription registers a device, replacing the row for the same endpoint.
//
// "Replacing" includes the owner. A browser's subscription belongs to the browser profile, not
// to an account, so a laptop that signs out of one account and into another re-subscribes the
// same endpoint: the row must follow, or the first account would keep receiving notifications
// on a device it no longer owns. That is a cross-account leak, and it is why the uniqueness is
// on the endpoint rather than on (user, endpoint).
func SavePushSubscription(ctx context.Context, q Querier, s PushSubscription) (PushSubscription, error) {
	s.Endpoint = strings.TrimSpace(s.Endpoint)
	if err := ValidatePushSubscription(s); err != nil {
		return PushSubscription{}, err
	}
	agent := truncateRunes(s.UserAgent, maxPushUserAgentLen)

	// One statement, because two would race: the count and the insert must agree, and a client
	// that subscribes two devices concurrently must not slip past the limit between them.
	row := q.QueryRow(ctx, `
		INSERT INTO push_subscriptions (user_id, endpoint, p256dh, auth, user_agent, last_used_at)
		SELECT $1, $2, $3, $4, $5, now()
		WHERE (SELECT count(*) FROM push_subscriptions WHERE user_id = $1) < $6
		   OR EXISTS (SELECT 1 FROM push_subscriptions WHERE endpoint = $2)
		ON CONFLICT (endpoint) DO UPDATE
			SET user_id = EXCLUDED.user_id,
			    p256dh = EXCLUDED.p256dh,
			    auth = EXCLUDED.auth,
			    user_agent = EXCLUDED.user_agent,
			    last_used_at = now()
		RETURNING `+subscriptionColumns,
		s.UserID, s.Endpoint, s.P256DH, s.Auth, agent, MaxPushDevices)

	saved, err := scanSubscription(row)
	if errors.Is(err, ErrPushSubscriptionNotFound) {
		// The SELECT produced no row, which means the device limit was hit and this endpoint is
		// new. That is the only way the statement returns nothing.
		return PushSubscription{}, ErrPushDeviceLimit
	}
	if err != nil {
		return PushSubscription{}, err
	}
	return saved, nil
}

// ListPushSubscriptions returns an account's devices, newest first.
//
// It is scoped by user in SQL, so a caller cannot list another account's devices even if it
// passes the wrong id — which is the rule every customer-scoped query in this codebase follows.
func ListPushSubscriptions(ctx context.Context, q Querier, userID string) ([]PushSubscription, error) {
	if userID == "" {
		return nil, nil
	}
	rows, err := q.Query(ctx, `
		SELECT `+subscriptionColumns+`
		FROM push_subscriptions
		WHERE user_id = $1
		ORDER BY created_at DESC`, userID)
	if err != nil {
		return nil, fmt.Errorf("notify: list push subscriptions: %w", err)
	}
	defer rows.Close()

	out := make([]PushSubscription, 0, 4)
	for rows.Next() {
		s, err := scanSubscription(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("notify: list push subscriptions: %w", err)
	}
	return out, nil
}

// DeletePushSubscription removes one device, scoped to its owner.
//
// The user id is in the WHERE clause rather than in a check before it: another account's id
// matches no row and returns ErrPushSubscriptionNotFound, which the HTTP layer answers 404 —
// the same answer as an id that never existed.
func DeletePushSubscription(ctx context.Context, q Querier, userID, id string) error {
	if userID == "" || id == "" {
		return ErrPushSubscriptionNotFound
	}
	tag, err := q.Exec(ctx,
		`DELETE FROM push_subscriptions WHERE id = $1 AND user_id = $2`, id, userID)
	if err != nil {
		return fmt.Errorf("notify: delete push subscription: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrPushSubscriptionNotFound
	}
	return nil
}

// DeletePushSubscriptionByEndpoint removes the device a browser just unsubscribed.
//
// The browser knows its endpoint and not our row id, so this is the path an unsubscribe from a
// page takes. It is scoped by user for the same reason the id form is.
func DeletePushSubscriptionByEndpoint(ctx context.Context, q Querier, userID, endpoint string) error {
	if userID == "" || endpoint == "" {
		return ErrPushSubscriptionNotFound
	}
	tag, err := q.Exec(ctx,
		`DELETE FROM push_subscriptions WHERE endpoint = $1 AND user_id = $2`, endpoint, userID)
	if err != nil {
		return fmt.Errorf("notify: delete push subscription: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrPushSubscriptionNotFound
	}
	return nil
}

// TouchPushSubscription records that a push was handed to the push service.
//
// It is best-effort by design: failing to update a timestamp must not fail a delivery that has
// already happened, so the error is returned for the log and never for the caller's decision.
func TouchPushSubscription(ctx context.Context, q Querier, id string) error {
	if id == "" {
		return nil
	}
	if _, err := q.Exec(ctx,
		`UPDATE push_subscriptions SET last_used_at = now() WHERE id = $1`, id); err != nil {
		return fmt.Errorf("notify: touch push subscription: %w", err)
	}
	return nil
}

// --------------------------------------------------------------- delivery --

// PushMessage is the payload a service worker receives.
//
// The field names are the service worker's, not the template's: the browser side of this
// feature is web/src/sw.ts and it reads exactly these keys. `url` is a panel path, never an
// absolute URL — a notification that can send the customer off-site is a phishing vector with
// our name on it.
type PushMessage struct {
	Title string `json:"title"`
	Body  string `json:"body"`
	URL   string `json:"url,omitempty"`
	Tag   string `json:"tag,omitempty"`
	// EventKey lets the service worker group or suppress duplicates; it is also the field an
	// operator sees in a browser's push log, which is why it is the catalogue key and not a
	// free-form string.
	EventKey string `json:"event_key,omitempty"`
}

// Payload renders the message as the JSON a push body carries.
//
// The text is truncated here rather than refused: a push service caps the encrypted body at
// 4096 bytes, and a template an operator wrote without watching the limit must not turn into a
// notification that is never delivered. Truncation is by rune, so a multi-byte character is
// never cut in half — a payload with an invalid UTF-8 tail is rejected by the browser's JSON
// parser and the whole notification is lost.
func (m PushMessage) Payload() ([]byte, error) {
	msg := PushMessage{
		Title:    truncateRunes(strings.TrimSpace(m.Title), 120),
		Body:     truncateRunes(strings.TrimSpace(m.Body), 400),
		URL:      truncateRunes(strings.TrimSpace(m.URL), 512),
		Tag:      truncateRunes(strings.TrimSpace(m.Tag), 64),
		EventKey: truncateRunes(strings.TrimSpace(m.EventKey), 64),
	}
	if msg.Title == "" {
		// A notification with no title renders as the origin's hostname, which looks like spam.
		msg.Title = "Catalyst Billing"
	}
	payload, err := json.Marshal(msg)
	if err != nil {
		return nil, fmt.Errorf("notify: encode push payload: %w", err)
	}
	if len(payload) > webpush.MaxPayload {
		// The only way to get here is a body of 400 runes of four-byte characters plus a long
		// URL; trimming the body to fit is better than dropping the notification.
		msg.Body = truncateRunes(msg.Body, 200)
		payload, err = json.Marshal(msg)
		if err != nil {
			return nil, fmt.Errorf("notify: encode push payload: %w", err)
		}
	}
	return payload, nil
}

// PushSender delivers one encrypted message to one device.
//
// It is an interface so the tests can assert what would have been sent, and so an installation
// that has no VAPID key pair can leave it nil rather than carrying a client that always fails.
// *webpush.Client is the production implementation.
type PushSender interface {
	Send(ctx context.Context, sub webpush.Subscription, payload []byte) error
}

// PushResult summarises one delivery pass, so the caller can report it.
type PushResult struct {
	// Sent is the number of devices the push service accepted the message for.
	Sent int
	// Removed is the number of devices deleted because the push service said they are gone.
	Removed int
	// Failed is the number of devices that could not be reached for another reason (a 5xx, a
	// rate limit, a network error). The subscription is kept: these are transient.
	Failed int
	// Skipped explains why nothing was sent at all, using the same stable strings the mail and
	// inbox channels use, plus "no_device" and "no_user". Empty when a send was attempted.
	Skipped string
	// Failures carries up to a few human-readable reasons, for a log line or a test push
	// response. It is bounded so one bad device cannot fill a response body.
	Failures []string
}

// DeliverPush sends one message to every device an account has, obeying the preference.
//
// `eventKey` is the catalogue key the message is about. It is used for exactly one thing: the
// preference decision, through the same Decide call the mail and inbox channels use. Passing an
// empty eventKey means "this is not an event" — a test push the user asked for — and it is
// allowed, because a customer who presses "send me a test notification" has already consented
// to receiving it.
//
// A nil sender is not an error: it means the installation has no VAPID key pair, and the result
// says so with Skipped = "push_not_configured" rather than failing the caller's request.
func DeliverPush(ctx context.Context, q Querier, sender PushSender, userID, eventKey string,
	msg PushMessage) (PushResult, error) {

	res := PushResult{}
	if sender == nil {
		res.Skipped = "push_not_configured"
		return res, nil
	}
	if userID == "" {
		res.Skipped = "no_user"
		return res, nil
	}

	if eventKey != "" {
		decision, err := Decide(ctx, q, userID, eventKey)
		if err != nil {
			return res, err
		}
		if !decision.Push {
			res.Skipped = decision.PushReason
			return res, nil
		}
	}

	subs, err := ListPushSubscriptions(ctx, q, userID)
	if err != nil {
		return res, err
	}
	if len(subs) == 0 {
		res.Skipped = "no_device"
		return res, nil
	}

	payload, err := msg.Payload()
	if err != nil {
		return res, err
	}

	for _, sub := range subs {
		err := sender.Send(ctx, webpush.Subscription{
			Endpoint: sub.Endpoint,
			P256DH:   sub.P256DH,
			Auth:     sub.Auth,
		}, payload)
		switch {
		case err == nil:
			res.Sent++
			// The timestamp is a nicety, not a fact the caller needs; a failure to write it is
			// dropped rather than turned into a delivery failure.
			_ = TouchPushSubscription(ctx, q, sub.ID)
		case errors.Is(err, webpush.ErrGone):
			// A dead device, not an error: delete the row so no future send pays for it.
			if derr := DeletePushSubscription(ctx, q, userID, sub.ID); derr != nil {
				return res, derr
			}
			res.Removed++
		default:
			res.Failed++
			if len(res.Failures) < 3 {
				res.Failures = append(res.Failures, err.Error())
			}
		}
	}
	return res, nil
}

// truncateRunes shortens s to at most n runes without splitting one.
func truncateRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	count := 0
	for i := range s {
		if count == n {
			return s[:i]
		}
		count++
	}
	return s
}
