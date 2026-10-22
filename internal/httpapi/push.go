// Web push from the customer's side: the browser subscribes, the browser unsubscribes, and the
// customer can ask for a notification they can see arrive.
//
// # Why these routes are AccessUser and not under /api/v1/portal
//
// A push subscription is the caller's own device. There is no record to be a staff member for,
// no other account's data to scope out, and no permission that could meaningfully be granted:
// "may subscribe this browser to their own notifications" is not a privilege, it is what a
// signed-in person is. AccessUser is exactly that statement — a session is required and no
// permission is — and the router now enforces it (see the note in routes.go), so a route
// declared this way cannot be reached anonymously.
//
// The paths are not under /api/v1/portal because every route there is required by the
// permission-coverage contract to declare a permission, and declaring one for this would mean
// inventing a permission whose only purpose is to satisfy a test. The routes are scoped to the
// caller in SQL instead: every statement carries `user_id = $session_user`, so another account's
// subscription id matches no row and answers 404 — the same answer as an id that never existed.
//
// # What is not here
//
// There is no route to generate a VAPID key pair. Generating a secret and returning it over
// HTTP is a credential leak with a login around it; the key pair is configuration, set once, and
// `webpush.GenerateKeyPair` exists for the tooling that does it. And there is no route to send a
// push to *another* user: that would be a way to make the panel a spam relay, and no feature
// needs it.

package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/catalyst/billing/internal/notify"
	"github.com/catalyst/billing/internal/notify/webpush"
)

func init() {
	// The push subscription table is classified in internal/store/storetest/push_tables.go: the
	// harness refuses to run against a table nobody has classified, and the registration has to
	// live in the package every database test binary links, which is the harness itself.
	RegisterRoutes("55-push", func(r *router) { r.s.registerPushRoutes(r) })
}

func (s *Server) registerPushRoutes(r *router) {
	// What the portal needs to decide whether to offer the feature at all. It is a separate
	// route from the subscription list because it is the one thing the screen must know before
	// it renders a button: with no VAPID key pair there is nothing to subscribe with, and a
	// button that cannot work is worse than no button.
	r.userGet("/api/v1/push/config", s.handlePushConfig)

	r.userGet("/api/v1/push/subscriptions", s.handleListPushSubscriptions)
	// Subscribing is idempotent by construction — the endpoint is the key — so a replayed
	// request returns the stored response instead of registering the device twice.
	r.userMutation(http.MethodPost, "/api/v1/push/subscriptions", true, s.handleSubscribePush)
	r.userMutation(http.MethodDelete, "/api/v1/push/subscriptions/{id}", true, s.handleUnsubscribePush)
	// A test push is a real delivery to every device on the account. It is a mutation because
	// it changes last_used_at and can delete a device the push service has expired.
	r.userMutation(http.MethodPost, "/api/v1/push/test", true, s.handleTestPush)
}

// ------------------------------------------------------------------ config --

// pushConfigJSON is the wire shape of GET /api/v1/push/config.
type pushConfigJSON struct {
	// Enabled is true only when push is switched on *and* the key pair is present and usable.
	// The portal hides its "enable notifications" control when this is false.
	Enabled bool `json:"enabled"`
	// PublicKey is the base64url VAPID public key a browser subscribes with. It is empty when
	// Enabled is false: serving a key the server cannot sign with would invite exactly the
	// subscription that can never be delivered to.
	PublicKey string `json:"vapid_public_key"`
	// Reason is a stable code for why push is unavailable, empty when it is available:
	// "disabled", "keys_missing", "keys_invalid".
	Reason string `json:"reason,omitempty"`
	// MaxDevices is the per-account bound, so the screen can say "3 of 20 devices" without
	// hard-coding a number the server owns.
	MaxDevices int `json:"max_devices"`
}

// pushClientCache memoises the VAPID client by key material.
//
// A client is cheap to build but holds a signed-token cache, and rebuilding it per request
// would sign a new token per push. The key is the key material itself, so rotating the keys in
// settings produces a new client on the next request with no invalidation to remember.
var pushClientCache = struct {
	mu      sync.Mutex
	entries map[string]*webpush.Client
}{entries: map[string]*webpush.Client{}}

// pushHTTPClient is the transport web push delivery uses.
//
// It is a package variable rather than a Server field because Server's fields are declared in
// server.go, which belongs to the integration; the With... method below is the same shape as
// the other optional dependencies (WithGateway, WithNotify) and is called during wiring, before
// the server serves anything. A test uses it to point delivery at a local TLS server whose
// certificate it trusts, which is what makes the delivery path testable without reaching a real
// push service.
var pushHTTPClient struct {
	mu     sync.RWMutex
	client *http.Client
}

// WithPushHTTP attaches the HTTP client used to reach push services.
//
// Nil means "use the package's bounded default": a ten-second timeout and no redirects
// followed. An installation behind a proxy or with a private CA replaces it here.
func (s *Server) WithPushHTTP(c *http.Client) *Server {
	pushHTTPClient.mu.Lock()
	pushHTTPClient.client = c
	pushHTTPClient.mu.Unlock()
	// Drop the memoised clients: they were built with the previous transport.
	pushClientCache.mu.Lock()
	pushClientCache.entries = map[string]*webpush.Client{}
	pushClientCache.mu.Unlock()
	return s
}

// currentPushHTTP returns the configured transport, or nil for the default.
func currentPushHTTP() *http.Client {
	pushHTTPClient.mu.RLock()
	defer pushHTTPClient.mu.RUnlock()
	return pushHTTPClient.client
}

// pushClient returns the configured VAPID client, or the reason there is not one.
//
// The reason is a stable string rather than an error because "push is switched off" is not a
// failure: it is the default state of a fresh installation, and the portal is expected to hide
// the feature rather than report a problem.
func (s *Server) pushClient(ctx context.Context) (*webpush.Client, string, error) {
	st := s.settingsStore()
	enabled, err := st.Bool(ctx, "push", "enabled")
	if err != nil {
		return nil, "", err
	}
	if !enabled {
		return nil, "disabled", nil
	}
	public, err := st.String(ctx, "push", "vapid_public_key")
	if err != nil {
		return nil, "", err
	}
	private, err := st.String(ctx, "push", "vapid_private_key")
	if err != nil {
		return nil, "", err
	}
	subject, err := st.String(ctx, "push", "subject")
	if err != nil {
		return nil, "", err
	}
	if public == "" || private == "" {
		return nil, "keys_missing", nil
	}

	sum := sha256.Sum256([]byte(public + "\x00" + private + "\x00" + subject))
	httpClient := currentPushHTTP()
	key := hex.EncodeToString(sum[:])
	if httpClient != nil {
		// The transport is part of the identity: a test (or an operator) that swaps it must not
		// be handed a client built with the previous one.
		key += fmt.Sprintf(":%p", httpClient)
	}

	pushClientCache.mu.Lock()
	cached, ok := pushClientCache.entries[key]
	pushClientCache.mu.Unlock()
	if ok {
		return cached, "", nil
	}

	client, err := webpush.NewClient(webpush.Config{
		PublicKey:  public,
		PrivateKey: private,
		Subject:    subject,
		HTTP:       httpClient,
		// A day: a notification about an invoice is worth reading tomorrow, and a push service
		// that holds it that long is doing what the header asks.
		TTL: 24 * time.Hour,
	})
	if err != nil {
		// The key pair is stored but unusable — a half-copied value, a key on the wrong curve,
		// two halves from different pairs. That is an operator problem the portal must not turn
		// into a button, so it is reported as unavailable and logged once per request.
		s.cfg.Logger.WarnContext(ctx, "web push is enabled but its VAPID keys are unusable",
			"error", err)
		return nil, "keys_invalid", nil
	}

	pushClientCache.mu.Lock()
	// Bound the cache: an operator rotating keys a few times is normal, a hundred entries is a
	// leak. Only the current key set is ever read.
	if len(pushClientCache.entries) > 8 {
		pushClientCache.entries = map[string]*webpush.Client{}
	}
	pushClientCache.entries[key] = client
	pushClientCache.mu.Unlock()
	return client, "", nil
}

// handlePushConfig serves what the portal needs to render the feature.
func (s *Server) handlePushConfig(w http.ResponseWriter, r *http.Request) {
	client, reason, err := s.pushClient(r.Context())
	if err != nil {
		s.writePushError(w, r, err)
		return
	}
	out := pushConfigJSON{MaxDevices: notify.MaxPushDevices, Reason: reason}
	if client != nil {
		out.Enabled = true
		out.PublicKey = client.PublicKey()
	}
	writeJSON(w, http.StatusOK, out)
}

// ----------------------------------------------------------- subscriptions --

// pushSubscriptionJSON is one device on the wire. It never carries the auth secret back to the
// client: the device already has it, and echoing a secret to a page is a habit that ends in a
// leak the first time a response is logged.
type pushSubscriptionJSON struct {
	ID        string `json:"id"`
	Endpoint  string `json:"endpoint"`
	UserAgent string `json:"user_agent"`
	CreatedAt any    `json:"created_at"`
	// LastUsedAt is null until a push has been handed to the push service.
	LastUsedAt any `json:"last_used_at"`
}

func subscriptionJSON(sub notify.PushSubscription) pushSubscriptionJSON {
	out := pushSubscriptionJSON{
		ID:        sub.ID,
		Endpoint:  sub.Endpoint,
		UserAgent: sub.UserAgent,
		CreatedAt: sub.CreatedAt.UTC(),
	}
	if sub.LastUsedAt != nil {
		out.LastUsedAt = sub.LastUsedAt.UTC()
	}
	return out
}

// handleListPushSubscriptions lists the caller's devices.
func (s *Server) handleListPushSubscriptions(w http.ResponseWriter, r *http.Request) {
	user, ok := userFrom(r.Context())
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "authentication_required", "sign in to continue")
		return
	}
	subs, err := notify.ListPushSubscriptions(r.Context(), s.pool, user.ID)
	if err != nil {
		s.writePushError(w, r, err)
		return
	}
	items := make([]pushSubscriptionJSON, 0, len(subs))
	for _, sub := range subs {
		items = append(items, subscriptionJSON(sub))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"data":        items,
		"max_devices": notify.MaxPushDevices,
	})
}

// pushSubscribeRequest is the browser's PushSubscription.toJSON().
//
// The nested `keys` object is the browser's own shape, and it is kept rather than flattened so
// the client can pass the object straight through: a translation layer between the browser and
// the server is one more place for a secret to be dropped.
type pushSubscribeRequest struct {
	Endpoint string `json:"endpoint"`
	Keys     struct {
		P256DH string `json:"p256dh"`
		Auth   string `json:"auth"`
	} `json:"keys"`
}

// handleSubscribePush registers a device, replacing the row for the same endpoint.
func (s *Server) handleSubscribePush(w http.ResponseWriter, r *http.Request) {
	user, ok := userFrom(r.Context())
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "authentication_required", "sign in to continue")
		return
	}
	if _, reason, err := s.pushClient(r.Context()); err != nil {
		s.writePushError(w, r, err)
		return
	} else if reason != "" {
		// Refusing here is what stops the database filling with subscriptions that could never
		// be delivered to. The browser has already asked the user for permission, so the reason
		// is spelled out rather than left as a generic failure.
		writeProblem(w, http.StatusServiceUnavailable, "push_not_configured",
			"this installation has no web push keys configured")
		return
	}

	var req pushSubscribeRequest
	if !s.decodeJSON(w, r, &req, 16<<10) {
		return
	}

	saved, err := notify.SavePushSubscription(r.Context(), s.pool, notify.PushSubscription{
		UserID:   user.ID,
		Endpoint: req.Endpoint,
		P256DH:   req.Keys.P256DH,
		Auth:     req.Keys.Auth,
		// The header, not the body: a browser cannot set User-Agent from a fetch, so the header
		// is the real one and a body field would be a value the caller chose.
		UserAgent: r.UserAgent(),
	})
	if err != nil {
		s.writePushError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, subscriptionJSON(saved))
}

// handleUnsubscribePush removes one of the caller's devices.
func (s *Server) handleUnsubscribePush(w http.ResponseWriter, r *http.Request) {
	user, ok := userFrom(r.Context())
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "authentication_required", "sign in to continue")
		return
	}
	id := r.PathValue("id")
	if err := notify.DeletePushSubscription(r.Context(), s.pool, user.ID, id); err != nil {
		s.writePushError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "deleted", "id": id})
}

// ------------------------------------------------------------------- test --

// handleTestPush sends a notification to every device the caller has registered.
//
// It goes through the same delivery path as a real notification — same encryption, same VAPID
// signing, same dead-device cleanup — so "the test arrived" is evidence that the feature works
// rather than evidence that a mock was called. The event key is empty on purpose: a test push is
// not an event, and a customer who presses the button has already consented to receiving it.
func (s *Server) handleTestPush(w http.ResponseWriter, r *http.Request) {
	user, ok := userFrom(r.Context())
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "authentication_required", "sign in to continue")
		return
	}
	client, reason, err := s.pushClient(r.Context())
	if err != nil {
		s.writePushError(w, r, err)
		return
	}
	if client == nil {
		writeProblem(w, http.StatusServiceUnavailable, "push_not_configured",
			"this installation has no web push keys configured ("+reason+")")
		return
	}

	res, err := notify.DeliverPush(r.Context(), s.pool, client, user.ID, "", notify.PushMessage{
		Title: "Notifications are working",
		Body: "This is a test notification from Catalyst Billing. " +
			"Nothing has changed on your account.",
		URL:      "/portal/notifications",
		Tag:      "push-test",
		EventKey: "push_test",
	})
	if err != nil {
		s.writePushError(w, r, err)
		return
	}

	switch {
	case res.Sent > 0:
		// At least one device accepted it. A partial failure is reported rather than hidden:
		// "1 of 2 devices" is a fact the customer can act on.
		writeJSON(w, http.StatusOK, pushResultJSON(res))
	case res.Removed > 0:
		// Every device we tried was gone. That is not a server error — the subscriptions have
		// been cleaned up — but nothing was delivered, so it is not a success either.
		writeProblem(w, http.StatusConflict, "no_push_device",
			"the registered devices are no longer reachable and have been removed; "+
				"enable notifications on this device again")
	case res.Skipped == "no_device":
		writeProblem(w, http.StatusConflict, "no_push_device",
			"enable notifications on a device before sending a test")
	case res.Skipped == "no_user":
		writeProblem(w, http.StatusUnauthorized, "authentication_required", "sign in to continue")
	default:
		// A failure the push service reported: the credentials were refused, it was throttling
		// us, or it returned a 5xx. 502 because the fault is downstream, with a code that says
		// which.
		s.cfg.Logger.WarnContext(r.Context(), "test push was not delivered",
			"user", user.ID, "failures", res.Failures)
		writeProblem(w, http.StatusBadGateway, "push_delivery_failed",
			"the push service did not accept the notification")
	}
}

// pushResultJSON is the delivery summary the screen shows.
func pushResultJSON(res notify.PushResult) map[string]any {
	failures := res.Failures
	if failures == nil {
		failures = []string{}
	}
	return map[string]any{
		"sent":     res.Sent,
		"removed":  res.Removed,
		"failed":   res.Failed,
		"failures": failures,
	}
}

// ---------------------------------------------------------------- errors --

// writePushError maps the package's errors to stable codes.
//
// Nothing here echoes a database message: every detail is written here, so a schema change
// cannot become a client-visible string.
func (s *Server) writePushError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, notify.ErrInvalidPushSubscription):
		writeProblem(w, http.StatusUnprocessableEntity, "invalid_subscription",
			"that subscription cannot receive notifications: the endpoint must be https "+
				"and the keys must be a P-256 point and a 16-byte auth secret")
	case errors.Is(err, notify.ErrPushDeviceLimit):
		writeProblem(w, http.StatusUnprocessableEntity, "push_device_limit",
			"this account already has the maximum number of devices; remove one first")
	case errors.Is(err, notify.ErrPushSubscriptionNotFound):
		// 404 rather than 403: another account's id must be indistinguishable from an id that
		// never existed.
		writeProblem(w, http.StatusNotFound, "not_found", "no such device")
	case errors.Is(err, notify.ErrPushNotConfigured):
		writeProblem(w, http.StatusServiceUnavailable, "push_not_configured",
			"this installation has no web push keys configured")
	default:
		s.cfg.Logger.ErrorContext(r.Context(), "push request failed",
			"path", r.URL.Path, "error", err)
		writeProblem(w, http.StatusInternalServerError, "internal_error",
			"the request could not be completed")
	}
}
