package notify_test

// Push subscription storage and the push delivery channel.
//
// These run against the real database because the properties that matter are database
// properties: the uniqueness that makes a re-subscribe replace rather than duplicate, the
// ownership column that follows the endpoint rather than the account, the device limit that is
// enforced in one statement, and the scoping that makes another account's id unfindable.
//
// The delivery tests use a real webpush.Client against a local TLS server, so the payload that
// arrives is decrypted with the receiver's own private key and compared with what was sent. A
// stub sender would prove that DeliverPush calls something; only the round trip proves that what
// it calls produces a notification.

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/catalyst/billing/internal/notify"
	"github.com/catalyst/billing/internal/notify/webpush"
	"github.com/catalyst/billing/internal/store"
	"github.com/catalyst/billing/internal/store/storetest"
)

// device is a browser: a P-256 key pair and a 16-byte auth secret, plus the subscription that
// carries them.
type device struct {
	private *ecdh.PrivateKey
	auth    string
	sub     notify.PushSubscription
}

func newDevice(t *testing.T, userID, endpoint string) device {
	t.Helper()
	key, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate device key: %v", err)
	}
	auth := make([]byte, 16)
	if _, err := rand.Read(auth); err != nil {
		t.Fatalf("auth secret: %v", err)
	}
	return device{
		private: key,
		auth:    b64(auth),
		sub: notify.PushSubscription{
			UserID:    userID,
			Endpoint:  endpoint,
			P256DH:    b64(key.PublicKey().Bytes()),
			Auth:      b64(auth),
			UserAgent: "Mozilla/5.0 (test) Firefox/140.0",
		},
	}
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// countingSender records what would have been sent and answers with a fixed error.
type countingSender struct {
	sent     int
	payloads [][]byte
	err      error
}

func (s *countingSender) Send(_ context.Context, _ webpush.Subscription, payload []byte) error {
	if s.err != nil {
		return s.err
	}
	s.sent++
	s.payloads = append(s.payloads, payload)
	return nil
}

// ------------------------------------------------------------- storage --

// A device is stored, listed, and re-subscribing the same endpoint replaces it rather than
// adding a second row.
func TestPushSubscriptionIsStoredPerDevice(t *testing.T) {
	pool := storetestPool(t)
	ctx := context.Background()
	user := seedUser(t, pool, "push-store@example.test")

	d := newDevice(t, user, "https://push.example.test/send/first")
	saved, err := notify.SavePushSubscription(ctx, pool, d.sub)
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if saved.ID == "" || saved.UserAgent != d.sub.UserAgent {
		t.Fatalf("saved = %+v", saved)
	}
	if saved.LastUsedAt == nil {
		t.Fatal("a fresh subscription has no last_used_at; subscribing is a use")
	}

	subs, err := notify.ListPushSubscriptions(ctx, pool, user)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(subs) != 1 {
		t.Fatalf("listed %d devices, want 1", len(subs))
	}

	// Re-subscribe the same endpoint with a rotated key. It is still one device.
	rotated := newDevice(t, user, d.sub.Endpoint)
	again, err := notify.SavePushSubscription(ctx, pool, rotated.sub)
	if err != nil {
		t.Fatalf("re-save: %v", err)
	}
	if again.ID != saved.ID {
		t.Fatalf("re-subscribing created a new row (%s, was %s)", again.ID, saved.ID)
	}
	if again.P256DH != rotated.sub.P256DH {
		t.Fatal("re-subscribing did not replace the device key")
	}
	subs, err = notify.ListPushSubscriptions(ctx, pool, user)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(subs) != 1 {
		t.Fatalf("re-subscribing left %d devices, want 1", len(subs))
	}
}

// A shared browser that signs into another account must move the subscription with it. Keeping
// the old owner would deliver the previous account's notifications to the new one's screen.
func TestPushSubscriptionFollowsTheEndpointToItsNewOwner(t *testing.T) {
	pool := storetestPool(t)
	ctx := context.Background()
	first := seedUser(t, pool, "push-first@example.test")
	second := seedUser(t, pool, "push-second@example.test")

	endpoint := "https://push.example.test/send/shared-laptop"
	if _, err := notify.SavePushSubscription(ctx, pool, newDevice(t, first, endpoint).sub); err != nil {
		t.Fatalf("save for first: %v", err)
	}
	saved, err := notify.SavePushSubscription(ctx, pool, newDevice(t, second, endpoint).sub)
	if err != nil {
		t.Fatalf("save for second: %v", err)
	}
	if saved.UserID != second {
		t.Fatalf("the row still belongs to %s, want %s", saved.UserID, second)
	}

	// The first account no longer has a device: it must not keep receiving notifications on a
	// browser it has signed out of.
	subs, err := notify.ListPushSubscriptions(ctx, pool, first)
	if err != nil {
		t.Fatalf("list first: %v", err)
	}
	if len(subs) != 0 {
		t.Fatalf("the previous owner still has %d devices", len(subs))
	}
}

// Another account's subscription id matches no row: the delete is scoped in SQL, so the answer is
// "no such device" rather than "not yours", which would be an oracle for existing ids.
func TestPushSubscriptionDeleteIsScopedToItsOwner(t *testing.T) {
	pool := storetestPool(t)
	ctx := context.Background()
	owner := seedUser(t, pool, "push-owner@example.test")
	other := seedUser(t, pool, "push-other@example.test")

	saved, err := notify.SavePushSubscription(ctx, pool, newDevice(t, owner, "https://push.example.test/send/owned").sub)
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	err = notify.DeletePushSubscription(ctx, pool, other, saved.ID)
	if !errors.Is(err, notify.ErrPushSubscriptionNotFound) {
		t.Fatalf("another account's delete: got %v, want ErrPushSubscriptionNotFound", err)
	}
	subs, err := notify.ListPushSubscriptions(ctx, pool, owner)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(subs) != 1 {
		t.Fatal("another account's delete removed the row")
	}

	// The owner can, and a second delete is not found rather than an error.
	if err := notify.DeletePushSubscription(ctx, pool, owner, saved.ID); err != nil {
		t.Fatalf("owner delete: %v", err)
	}
	if err := notify.DeletePushSubscription(ctx, pool, owner, saved.ID); !errors.Is(err, notify.ErrPushSubscriptionNotFound) {
		t.Fatalf("second delete: got %v, want ErrPushSubscriptionNotFound", err)
	}
}

// The endpoint is the device, so the row can be removed by endpoint as well as by id. That is the
// path a service worker's own unsubscribe takes: the browser knows its endpoint and not our id.
func TestPushSubscriptionDeleteByEndpoint(t *testing.T) {
	pool := storetestPool(t)
	ctx := context.Background()
	owner := seedUser(t, pool, "push-endpoint@example.test")
	other := seedUser(t, pool, "push-endpoint-other@example.test")

	endpoint := "https://push.example.test/send/by-endpoint"
	if _, err := notify.SavePushSubscription(ctx, pool, newDevice(t, owner, endpoint).sub); err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := notify.DeletePushSubscriptionByEndpoint(ctx, pool, other, endpoint); !errors.Is(err, notify.ErrPushSubscriptionNotFound) {
		t.Fatalf("another account's delete: got %v, want ErrPushSubscriptionNotFound", err)
	}
	if err := notify.DeletePushSubscriptionByEndpoint(ctx, pool, owner, endpoint); err != nil {
		t.Fatalf("owner delete: %v", err)
	}
	subs, err := notify.ListPushSubscriptions(ctx, pool, owner)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(subs) != 0 {
		t.Fatalf("%d devices left", len(subs))
	}
}

// The database refuses a subscription the Go write path would refuse, so a future writer that
// bypasses the store cannot put a plaintext endpoint in the table. The CHECK is the last line,
// and a test that inserts directly is the only way to prove it is there.
func TestPushSubscriptionDatabaseRefusesAnInsecureEndpoint(t *testing.T) {
	pool := storetestPool(t)
	ctx := context.Background()
	user := seedUser(t, pool, "push-check@example.test")
	d := newDevice(t, user, "https://push.example.test/send/check")

	_, err := pool.Exec(ctx, `
		INSERT INTO push_subscriptions (user_id, endpoint, p256dh, auth)
		VALUES ($1, 'http://push.example.test/send/plain', $2, $3)`,
		user, d.sub.P256DH, d.sub.Auth)
	if err == nil {
		t.Fatal("the database accepted an http endpoint")
	}
	if !store.IsConstraintViolation(err) {
		t.Fatalf("got %v, want a constraint violation", err)
	}

	// And the endpoint is unique: two rows for one device would double every notification.
	if _, err := notify.SavePushSubscription(ctx, pool, d.sub); err != nil {
		t.Fatalf("save: %v", err)
	}
	_, err = pool.Exec(ctx, `
		INSERT INTO push_subscriptions (user_id, endpoint, p256dh, auth)
		VALUES ($1, $2, $3, $4)`, user, d.sub.Endpoint, d.sub.P256DH, d.sub.Auth)
	if err == nil {
		t.Fatal("the database accepted a duplicate endpoint")
	}
	if got := store.SQLState(err); got != store.StateUniqueViolation {
		t.Fatalf("got %v (state %q), want a unique violation", err, got)
	}
}

// The device limit is enforced in the insert itself, and re-subscribing an existing device is
// never refused by it.
func TestPushSubscriptionDeviceLimit(t *testing.T) {
	pool := storetestPool(t)
	ctx := context.Background()
	user := seedUser(t, pool, "push-many@example.test")

	for i := 0; i < notify.MaxPushDevices; i++ {
		endpoint := "https://push.example.test/send/device-" + string(rune('a'+i))
		if _, err := notify.SavePushSubscription(ctx, pool, newDevice(t, user, endpoint).sub); err != nil {
			t.Fatalf("save device %d: %v", i, err)
		}
	}
	_, err := notify.SavePushSubscription(ctx, pool, newDevice(t, user, "https://push.example.test/send/one-too-many").sub)
	if !errors.Is(err, notify.ErrPushDeviceLimit) {
		t.Fatalf("the %dth device: got %v, want ErrPushDeviceLimit", notify.MaxPushDevices+1, err)
	}

	// Re-subscribing a device the account already has is allowed: it is the same device, not a
	// new one.
	if _, err := notify.SavePushSubscription(ctx, pool, newDevice(t, user, "https://push.example.test/send/device-a").sub); err != nil {
		t.Fatalf("re-subscribing an existing device was refused: %v", err)
	}
}

// A subscription that could never receive anything is refused at the write path, not at send
// time once per device forever.
func TestPushSubscriptionValidation(t *testing.T) {
	pool := storetestPool(t)
	ctx := context.Background()
	user := seedUser(t, pool, "push-invalid@example.test")
	good := newDevice(t, user, "https://push.example.test/send/x")

	cases := map[string]func(*notify.PushSubscription){
		"http endpoint": func(s *notify.PushSubscription) { s.Endpoint = "http://push.example.test/send/x" },
		"no host":       func(s *notify.PushSubscription) { s.Endpoint = "https:///send/x" },
		"off-curve key": func(s *notify.PushSubscription) { s.P256DH = b64(bytes.Repeat([]byte{0x04}, 65)) },
		"short auth":    func(s *notify.PushSubscription) { s.Auth = b64([]byte("nope")) },
		"no owner":      func(s *notify.PushSubscription) { s.UserID = "" },
	}
	for name, mutate := range cases {
		sub := good.sub
		mutate(&sub)
		if _, err := notify.SavePushSubscription(ctx, pool, sub); !errors.Is(err, notify.ErrInvalidPushSubscription) {
			t.Fatalf("%s: got %v, want ErrInvalidPushSubscription", name, err)
		}
	}

	// A very long user agent is a label, so it is truncated rather than refused.
	sub := good.sub
	sub.UserAgent = strings.Repeat("x", 1000)
	saved, err := notify.SavePushSubscription(ctx, pool, sub)
	if err != nil {
		t.Fatalf("save with a long user agent: %v", err)
	}
	if len(saved.UserAgent) != 300 {
		t.Fatalf("user agent stored at %d bytes, want it truncated to 300", len(saved.UserAgent))
	}
}

// ----------------------------------------------------------- preference --

// Push obeys the same force / choice / never precedence as mail and the inbox, because it is the
// same Decision. The three cases below are the ones a second, parallel implementation would get
// wrong.
func TestDeliverPushObeysThePreference(t *testing.T) {
	pool := storetestPool(t)
	ctx := context.Background()
	user := seedUser(t, pool, "push-pref@example.test")
	if _, err := notify.SavePushSubscription(ctx, pool, newDevice(t, user, "https://push.example.test/send/pref").sub); err != nil {
		t.Fatalf("save: %v", err)
	}

	// No preference row: notifications are opt-out, so the default is on.
	sender := &countingSender{}
	res, err := notify.DeliverPush(ctx, pool, sender, user, notify.EventInvoicePaid,
		notify.PushMessage{Title: "Invoice paid"})
	if err != nil {
		t.Fatalf("deliver: %v", err)
	}
	if res.Sent != 1 || sender.sent != 1 {
		t.Fatalf("default preference: sent %d (sender %d), want 1", res.Sent, sender.sent)
	}

	// choice_off: nothing is sent, and the reason is the stable string the other channels use.
	if err := notify.SetPreference(ctx, pool, user, notify.EventInvoicePaid, notify.PreferenceChoiceOff); err != nil {
		t.Fatalf("set preference: %v", err)
	}
	sender = &countingSender{}
	res, err = notify.DeliverPush(ctx, pool, sender, user, notify.EventInvoicePaid,
		notify.PushMessage{Title: "Invoice paid"})
	if err != nil {
		t.Fatalf("deliver: %v", err)
	}
	if res.Sent != 0 || sender.sent != 0 {
		t.Fatal("a push was sent for an event the user turned off")
	}
	if res.Skipped != "preference_off" {
		t.Fatalf("Skipped = %q, want preference_off", res.Skipped)
	}

	// `never` is an operator decision and outranks everything. It is written directly because a
	// user is not allowed to store it — SetPreference refuses, which is the other half of the
	// rule and is tested in preferences' own tests.
	if _, err := pool.Exec(ctx, `
		INSERT INTO notification_preferences (user_id, event_key, preference, updated_at)
		VALUES ($1, $2, 'never', now())
		ON CONFLICT (user_id, event_key) DO UPDATE SET preference = 'never'`,
		user, notify.EventInvoicePaid); err != nil {
		t.Fatalf("store never: %v", err)
	}
	sender = &countingSender{}
	res, err = notify.DeliverPush(ctx, pool, sender, user, notify.EventInvoicePaid,
		notify.PushMessage{Title: "Invoice paid"})
	if err != nil {
		t.Fatalf("deliver: %v", err)
	}
	if res.Sent != 0 || res.Skipped != "preference_never" {
		t.Fatalf("never: sent %d, skipped %q", res.Sent, res.Skipped)
	}
}

// A force event ignores an opt-out: opting out of a password reset does not protect anyone, it
// just makes the account unrecoverable. The row is written directly because the write path
// refuses to create it, which is exactly what makes this a test of the read path.
func TestDeliverPushSendsAForceEventDespiteAnOptOut(t *testing.T) {
	pool := storetestPool(t)
	ctx := context.Background()
	user := seedUser(t, pool, "push-force@example.test")
	if _, err := notify.SavePushSubscription(ctx, pool, newDevice(t, user, "https://push.example.test/send/force").sub); err != nil {
		t.Fatalf("save: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO notification_preferences (user_id, event_key, preference, updated_at)
		VALUES ($1, $2, 'choice_off', now())
		ON CONFLICT (user_id, event_key) DO UPDATE SET preference = 'choice_off'`,
		user, notify.EventPasswordReset); err != nil {
		t.Fatalf("store preference: %v", err)
	}

	sender := &countingSender{}
	res, err := notify.DeliverPush(ctx, pool, sender, user, notify.EventPasswordReset,
		notify.PushMessage{Title: "Reset your password"})
	if err != nil {
		t.Fatalf("deliver: %v", err)
	}
	if res.Sent != 1 {
		t.Fatalf("a force event was suppressed by an opt-out: sent %d, skipped %q", res.Sent, res.Skipped)
	}
}

// A test push is not an event: the empty event key means "the user asked for this", so the
// preference lookup is skipped rather than consulted.
func TestDeliverPushSendsATestEvenWhenEveryEventIsOff(t *testing.T) {
	pool := storetestPool(t)
	ctx := context.Background()
	user := seedUser(t, pool, "push-test@example.test")
	if _, err := notify.SavePushSubscription(ctx, pool, newDevice(t, user, "https://push.example.test/send/test").sub); err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := notify.SetPreference(ctx, pool, user, notify.EventInvoicePaid, notify.PreferenceChoiceOff); err != nil {
		t.Fatalf("set preference: %v", err)
	}

	sender := &countingSender{}
	res, err := notify.DeliverPush(ctx, pool, sender, user, "", notify.PushMessage{Title: "Test"})
	if err != nil {
		t.Fatalf("deliver: %v", err)
	}
	if res.Sent != 1 {
		t.Fatalf("a test push was suppressed: skipped %q", res.Skipped)
	}
}

// An account with no device is not an error; the caller is told why nothing was sent.
func TestDeliverPushWithNoDevice(t *testing.T) {
	pool := storetestPool(t)
	ctx := context.Background()
	user := seedUser(t, pool, "push-nodevice@example.test")

	res, err := notify.DeliverPush(ctx, pool, &countingSender{}, user, "", notify.PushMessage{Title: "x"})
	if err != nil {
		t.Fatalf("deliver: %v", err)
	}
	if res.Sent != 0 || res.Skipped != "no_device" {
		t.Fatalf("res = %+v, want Skipped no_device", res)
	}

	// No sender configured (no VAPID key pair) is also a skip, not a failure.
	res, err = notify.DeliverPush(ctx, pool, nil, user, "", notify.PushMessage{Title: "x"})
	if err != nil {
		t.Fatalf("deliver with no sender: %v", err)
	}
	if res.Skipped != "push_not_configured" {
		t.Fatalf("Skipped = %q, want push_not_configured", res.Skipped)
	}
}

// -------------------------------------------------------------- delivery --

// A 404 or 410 from the push service deletes the subscription: the device is gone, and no retry
// can change that.
func TestDeliverPushDeletesADeadDevice(t *testing.T) {
	pool := storetestPool(t)
	ctx := context.Background()
	user := seedUser(t, pool, "push-dead@example.test")
	if _, err := notify.SavePushSubscription(ctx, pool, newDevice(t, user, "https://push.example.test/send/dead").sub); err != nil {
		t.Fatalf("save: %v", err)
	}

	res, err := notify.DeliverPush(ctx, pool, &countingSender{err: webpush.ErrGone}, user, "",
		notify.PushMessage{Title: "x"})
	if err != nil {
		t.Fatalf("deliver: %v", err)
	}
	if res.Removed != 1 || res.Failed != 0 || res.Sent != 0 {
		t.Fatalf("res = %+v, want one removed", res)
	}
	subs, err := notify.ListPushSubscriptions(ctx, pool, user)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(subs) != 0 {
		t.Fatal("a dead device was kept")
	}
}

// Any other failure keeps the subscription: a rate limit or a 5xx is transient, and deleting the
// device over one would silently unsubscribe a working phone.
func TestDeliverPushKeepsADeviceOnATransientFailure(t *testing.T) {
	pool := storetestPool(t)
	ctx := context.Background()
	user := seedUser(t, pool, "push-transient@example.test")
	if _, err := notify.SavePushSubscription(ctx, pool, newDevice(t, user, "https://push.example.test/send/flaky").sub); err != nil {
		t.Fatalf("save: %v", err)
	}

	res, err := notify.DeliverPush(ctx, pool, &countingSender{err: webpush.ErrRateLimited}, user, "",
		notify.PushMessage{Title: "x"})
	if err != nil {
		t.Fatalf("deliver: %v", err)
	}
	if res.Failed != 1 || res.Removed != 0 {
		t.Fatalf("res = %+v, want one failure and no removal", res)
	}
	if len(res.Failures) != 1 {
		t.Fatalf("Failures = %v, want the reason", res.Failures)
	}
	subs, err := notify.ListPushSubscriptions(ctx, pool, user)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(subs) != 1 {
		t.Fatal("a device was deleted over a transient failure")
	}
}

// The whole path end to end: the stored subscription, the real VAPID client, the encrypted POST
// and the receiver's own private key. The payload that arrives must be the payload that was
// sent, or the feature does not work no matter what the database says.
func TestDeliverPushDeliversAnEncryptedPayload(t *testing.T) {
	pool := storetestPool(t)
	ctx := context.Background()
	user := seedUser(t, pool, "push-e2e@example.test")

	var received []byte
	var authHeader string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeader = r.Header.Get("Authorization")
		received, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	d := newDevice(t, user, srv.URL+"/send/e2e")
	saved, err := notify.SavePushSubscription(ctx, pool, d.sub)
	if err != nil {
		t.Fatalf("save: %v", err)
	}

	pub, priv, err := webpush.GenerateKeyPair()
	if err != nil {
		t.Fatalf("generate VAPID pair: %v", err)
	}
	client, err := webpush.NewClient(webpush.Config{
		PublicKey: pub, PrivateKey: priv, Subject: "mailto:ops@example.test",
		HTTP: srv.Client(),
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	res, err := notify.DeliverPush(ctx, pool, client, user, "",
		notify.PushMessage{
			Title:    "Notifications are working",
			Body:     "This is a test notification.",
			URL:      "/portal/notifications",
			EventKey: "push_test",
		})
	if err != nil {
		t.Fatalf("deliver: %v", err)
	}
	if res.Sent != 1 {
		t.Fatalf("res = %+v, want one sent", res)
	}
	if !strings.HasPrefix(authHeader, "vapid t=") {
		t.Fatalf("Authorization = %q", authHeader)
	}

	plain, err := webpush.Decrypt(d.private, d.auth, received)
	if err != nil {
		t.Fatalf("the delivered body does not decrypt: %v", err)
	}
	var msg notify.PushMessage
	if err := json.Unmarshal(plain, &msg); err != nil {
		t.Fatalf("payload is not JSON: %v", err)
	}
	if msg.Title != "Notifications are working" || msg.URL != "/portal/notifications" {
		t.Fatalf("payload = %+v", msg)
	}

	// The device is marked used, which is what the portal shows as "last received".
	subs, err := notify.ListPushSubscriptions(ctx, pool, user)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(subs) != 1 || subs[0].ID != saved.ID {
		t.Fatalf("subscription disappeared: %+v", subs)
	}
	if subs[0].LastUsedAt == nil || subs[0].LastUsedAt.Before(subs[0].CreatedAt) {
		t.Fatalf("last_used_at was not updated: %+v", subs[0])
	}
}

// A payload is truncated by rune, so a notification an operator wrote without watching the size
// limit still arrives — and arrives as valid UTF-8, because the browser's JSON parser rejects
// anything else and the whole notification is lost.
func TestPushMessagePayloadIsBounded(t *testing.T) {
	long := notify.PushMessage{
		Title: strings.Repeat("🧾", 300),
		Body:  strings.Repeat("é", 900),
		URL:   "/portal/notifications",
	}
	payload, err := long.Payload()
	if err != nil {
		t.Fatalf("payload: %v", err)
	}
	if len(payload) > webpush.MaxPayload {
		t.Fatalf("payload is %d bytes, over the %d limit", len(payload), webpush.MaxPayload)
	}
	var msg notify.PushMessage
	if err := json.Unmarshal(payload, &msg); err != nil {
		t.Fatalf("payload is not valid JSON (a split rune?): %v", err)
	}
	if msg.Title == "" || msg.Body == "" {
		t.Fatalf("payload lost its text: %+v", msg)
	}

	// A message with no title still gets one, so the notification does not render as the
	// origin's hostname, which looks like spam.
	empty, err := (notify.PushMessage{Body: "body"}).Payload()
	if err != nil {
		t.Fatalf("payload: %v", err)
	}
	if err := json.Unmarshal(empty, &msg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if msg.Title == "" {
		t.Fatal("a titleless message produced a notification with no title")
	}
}

// storetestPool is the harness pool, with the test's own database.
func storetestPool(t *testing.T) *store.Pool {
	t.Helper()
	return storetest.DB(t)
}
