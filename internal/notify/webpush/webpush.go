// Package webpush implements Web Push with the Go standard library and nothing else.
//
// There is no module dependency here, and that is a decision rather than an accident. The
// protocol is three well-specified pieces of public-key cryptography — a VAPID JWT
// (RFC 8292), a message encrypted to the device's key (RFC 8291) and a plain HTTPS POST — and
// each one is short enough to read in a sitting. A dependency would have brought a second
// HTTP client, a second JSON encoder and a transitive tree into a package that ships inside a
// billing binary whose whole point is that its dependency surface is auditable.
//
// # What the two halves are, and why they are separate
//
//   - **Encryption** (encrypt.go) turns a payload and a subscription into the bytes a push
//     service will accept. It is pure: no clock, no network, no database. That is what lets
//     the test in webpush_test.go generate a receiver key pair, encrypt, decrypt with the
//     receiver's private key and assert the payload comes back byte for byte. Correctness of
//     RFC 8291 is then a fact that a test establishes, not a claim in a comment.
//   - **Delivery** (webpush.go) signs the VAPID token, sets the headers the push service
//     requires and POSTs. It is where the protocol's failure modes live, and they are not all
//     errors: a 404 or a 410 means the device is gone (the user cleared site data, the browser
//     was uninstalled, the service expired the subscription) and the right response is to
//     delete the row, not to retry. The caller learns that through errors.Is(err, ErrGone)
//     rather than by reading a status code out of a string.
//
// # The security shape
//
// The endpoint is supplied by the browser and this package makes an outbound request to it.
// That is an SSRF surface by construction, so the constraints are explicit: only https, no
// redirects followed, a bounded response body and a short timeout. The payload is encrypted to
// a key only the device holds, so the push service — a third party that is not ours — cannot
// read it; it sees an opaque record and the VAPID public key. The auth secret and the device
// key are the only secrets, and they never travel: they are inputs to a key derivation.
package webpush

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Subscription is one device's push registration, as the browser reports it through
// PushSubscription.toJSON(). The field names are the browser's, so a caller can hand the
// JSON straight over without a translation layer that could reorder two secrets.
type Subscription struct {
	// Endpoint is the push service URL for this subscription. https only.
	Endpoint string
	// P256DH is the device's P-256 public key, base64url, uncompressed point.
	P256DH string
	// Auth is the 16-byte shared auth secret, base64url.
	Auth string
}

// Delivery errors. A caller branches on these with errors.Is; the status code and the push
// service's own error text are available through *StatusError for a log line.
var (
	// ErrGone means the push service says the subscription no longer exists (404 or 410).
	// The subscription must be deleted: there is no retry that can succeed.
	ErrGone = errors.New("webpush: the subscription no longer exists")
	// ErrUnauthorized means the VAPID credentials were refused (401 or 403). That is a
	// configuration fault — a wrong subject, a key pair that does not match, or a clock far
	// enough out that the token's exp is in the past — not a dead device.
	ErrUnauthorized = errors.New("webpush: the push service rejected the VAPID credentials")
	// ErrTooLarge means the encrypted payload is larger than a push message may be (413), or
	// that the plaintext is larger than this package will encrypt. Push services cap a message
	// at 4096 bytes *including* the encryption overhead.
	ErrTooLarge = errors.New("webpush: the payload is too large for a push message")
	// ErrRateLimited means the push service is throttling us (429). The device is fine.
	ErrRateLimited = errors.New("webpush: the push service is rate limiting")
	// ErrRejected is any other refusal.
	ErrRejected = errors.New("webpush: the push service refused the message")
)

// StatusError is a non-success response from a push service.
type StatusError struct {
	Status int
	// Code is the push service's own error code, e.g. "BadJwtToken". It is logged, never
	// matched on: only the status code is specified.
	Code string
	Body string
}

// Error implements error.
func (e *StatusError) Error() string {
	msg := fmt.Sprintf("webpush: push service answered %d", e.Status)
	if e.Code != "" {
		msg += " (" + e.Code + ")"
	}
	return msg
}

// Unwrap maps a status onto the sentinel a caller branches on.
func (e *StatusError) Unwrap() error {
	switch e.Status {
	case http.StatusNotFound, http.StatusGone:
		return ErrGone
	case http.StatusUnauthorized, http.StatusForbidden:
		return ErrUnauthorized
	case http.StatusRequestEntityTooLarge:
		return ErrTooLarge
	case http.StatusTooManyRequests:
		return ErrRateLimited
	default:
		return ErrRejected
	}
}

// Config is what a Client needs.
type Config struct {
	// PublicKey and PrivateKey are the VAPID key pair, in any of the encodings
	// ParsePublicKey and ParsePrivateKey accept. The public half is also what the browser
	// subscribes with, so a mismatch between the two is a subscription that can never be
	// delivered to — NewClient refuses it rather than letting every push fail with a 403.
	PublicKey  string
	PrivateKey string
	// Subject is the VAPID `sub` claim: a mailto: or https: URL identifying the sender, so a
	// push service operator can contact whoever is sending traffic it does not like.
	Subject string
	// TTL is how long the push service may hold the message for an offline device. Zero is
	// valid and means "deliver now or drop it".
	TTL time.Duration
	// Urgency is the RFC 8030 urgency: very-low, low, normal or high. Empty means normal.
	Urgency string
	// Topic makes a later message replace an earlier undelivered one with the same topic,
	// which is what keeps ten queued invoice reminders from arriving at once. Empty means
	// every message is independent.
	Topic string
	// HTTP is the client used for delivery. Optional; a bounded default is used when nil.
	HTTP *http.Client
	// Now is the clock, injectable so a test can prove the token's exp.
	Now func() time.Time
	// Rand is the randomness source. Optional.
	Rand io.Reader
}

// Client sends encrypted pushes to one device at a time.
//
// It is safe for concurrent use: the VAPID token cache is mutex-guarded, and nothing else is
// mutated after construction.
type Client struct {
	private *ecdsa.PrivateKey
	public  *ecdh.PublicKey
	// publicB64 is the `k` parameter of the Authorization header: the uncompressed public
	// point in base64url, which is the same string the browser subscribed with.
	publicB64 string
	subject   string
	ttl       int
	urgency   string
	topic     string
	http      *http.Client
	now       func() time.Time
	rand      io.Reader

	// tokens caches the signed JWT per audience. A VAPID token is valid for up to 24 hours and
	// the audience is the push service's origin, so an installation that sends to Chrome and
	// Firefox signs two tokens a day rather than one per message.
	mu     sync.Mutex
	tokens map[string]cachedToken
}

type cachedToken struct {
	value   string
	expires time.Time
}

// maxResponseBytes bounds what is read from a push service. Its error bodies are short; a
// response larger than this is either a proxy in the way or an attack.
const maxResponseBytes = 4 << 10

// defaultTimeout bounds one delivery attempt. A push service that has not answered in ten
// seconds is not going to, and the caller is holding a request open.
const defaultTimeout = 10 * time.Second

// NewClient validates the key pair and builds a client.
//
// The check that the public and private halves belong together is not ceremony: the public key
// is what the browser subscribes with, and if it is a different key from the one that signs the
// VAPID token then every delivery is refused by the push service with a 403 that reads like a
// configuration error and is one. Refusing at construction puts that failure where it can be
// seen once, at boot, instead of at every send.
func NewClient(cfg Config) (*Client, error) {
	if strings.TrimSpace(cfg.PublicKey) == "" || strings.TrimSpace(cfg.PrivateKey) == "" {
		return nil, errors.New("webpush: both halves of the VAPID key pair are required")
	}
	if strings.TrimSpace(cfg.Subject) == "" {
		return nil, errors.New("webpush: a VAPID subject is required")
	}
	if !strings.HasPrefix(cfg.Subject, "mailto:") && !strings.HasPrefix(cfg.Subject, "https://") {
		// RFC 8292 requires one of the two. A bare address is the mistake an operator makes,
		// and a push service answers it with a 403 that does not say why.
		return nil, fmt.Errorf("webpush: the VAPID subject must be a mailto: or https: URL, got %q", cfg.Subject)
	}

	private, err := ParsePrivateKey(cfg.PrivateKey)
	if err != nil {
		return nil, err
	}
	public, err := ParsePublicKey(cfg.PublicKey)
	if err != nil {
		return nil, err
	}
	derived, err := publicFromPrivate(private)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(derived, public.Bytes()) {
		return nil, errors.New("webpush: the VAPID public key is not the public half of the " +
			"private key; the browser would subscribe with one key and the token would be " +
			"signed by the other")
	}

	urgency := cfg.Urgency
	switch urgency {
	case "":
		urgency = "normal"
	case "very-low", "low", "normal", "high":
	default:
		return nil, fmt.Errorf("webpush: unknown urgency %q", urgency)
	}
	if cfg.TTL < 0 {
		return nil, errors.New("webpush: TTL must not be negative")
	}

	c := &Client{
		private:   private,
		public:    public,
		publicB64: base64URL(public.Bytes()),
		subject:   cfg.Subject,
		ttl:       int(cfg.TTL / time.Second),
		urgency:   urgency,
		topic:     cfg.Topic,
		http:      cfg.HTTP,
		now:       cfg.Now,
		rand:      cfg.Rand,
		tokens:    map[string]cachedToken{},
	}
	if c.http == nil {
		c.http = &http.Client{
			Timeout: defaultTimeout,
			// A redirect is not part of the protocol. Following one would let a subscription
			// endpoint point this server at a host of the attacker's choosing.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	}
	if c.now == nil {
		c.now = time.Now
	}
	if c.rand == nil {
		c.rand = rand.Reader
	}
	return c, nil
}

// PublicKey returns the base64url public key a browser subscribes with.
//
// It is the same string an operator stores in settings, so the portal serves it straight from
// here rather than from a second copy that could drift.
func (c *Client) PublicKey() string { return c.publicB64 }

// Send encrypts payload to one device and hands it to the push service.
//
// The payload is the JSON a service worker's `push` event receives; this package does not
// interpret it. A 404 or 410 returns an error that satisfies errors.Is(err, ErrGone), and the
// caller must delete the subscription.
func (c *Client) Send(ctx context.Context, sub Subscription, payload []byte) error {
	body, err := encrypt(c.rand, sub, payload)
	if err != nil {
		return err
	}
	audience, err := origin(sub.Endpoint)
	if err != nil {
		return err
	}
	auth, err := c.authorization(audience)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, sub.Endpoint, bytes.NewReader(body))
	if err != nil {
		// A malformed endpoint is the browser's fault or an attack; either way it is not
		// retryable and the caller should treat it like a dead subscription.
		return fmt.Errorf("%w: %v", ErrGone, err)
	}
	req.Header.Set("Authorization", auth)
	req.Header.Set("Content-Encoding", "aes128gcm")
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("TTL", fmt.Sprint(c.ttl))
	req.Header.Set("Urgency", c.urgency)
	if c.topic != "" {
		req.Header.Set("Topic", c.topic)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("webpush: deliver: %w", err)
	}
	defer resp.Body.Close()
	// Drain rather than discard: an unread body stops the connection being reused, and a
	// notification burst would open a connection per device.
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))

	switch resp.StatusCode {
	case http.StatusCreated, http.StatusOK, http.StatusAccepted, http.StatusNoContent:
		return nil
	}

	statusErr := &StatusError{Status: resp.StatusCode, Body: strings.TrimSpace(string(raw))}
	// Push services report a machine-readable code in the body: {"code": "...", "message": "..."}.
	// It is carried into the log line because "410" alone does not distinguish an expired
	// subscription from a deleted one.
	var problem struct {
		Code string `json:"code"`
	}
	if json.Unmarshal(raw, &problem) == nil {
		statusErr.Code = problem.Code
	}
	return statusErr
}

// authorization builds the `Authorization: vapid t=…,k=…` header for one audience.
func (c *Client) authorization(audience string) (string, error) {
	c.mu.Lock()
	cached, ok := c.tokens[audience]
	c.mu.Unlock()

	now := c.now()
	if ok && now.Before(cached.expires) {
		return "vapid t=" + cached.value + ",k=" + c.publicB64, nil
	}

	// RFC 8292 caps the token's lifetime at 24 hours. Twelve leaves room for a clock that is a
	// little ahead of the push service's without ever presenting an expired token.
	const lifetime = 12 * time.Hour
	token, err := c.signJWT(audience, now, now.Add(lifetime))
	if err != nil {
		return "", err
	}
	c.mu.Lock()
	c.tokens[audience] = cachedToken{value: token, expires: now.Add(lifetime - time.Minute)}
	c.mu.Unlock()
	return "vapid t=" + token + ",k=" + c.publicB64, nil
}

// origin returns the scheme and host of an endpoint: the JWT audience is the push service's
// origin, not the full subscription URL.
func origin(endpoint string) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", fmt.Errorf("webpush: subscription endpoint is not a URL: %w", err)
	}
	if u.Scheme != "https" {
		return "", fmt.Errorf("webpush: subscription endpoint must be https, got %q", u.Scheme)
	}
	if u.Host == "" {
		return "", errors.New("webpush: subscription endpoint has no host")
	}
	return u.Scheme + "://" + u.Host, nil
}
