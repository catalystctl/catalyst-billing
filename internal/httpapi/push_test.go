package httpapi_test

// Web push over HTTP: the config the portal reads, subscribing a device, unsubscribing it, and
// the test notification.
//
// The property that matters most here is the one a mock cannot establish: the test push is a
// real delivery. The push service is a local TLS server, the body that arrives is decrypted with
// the device's own private key, and the payload is compared with what the server said it sent.
// If the RFC 8291 derivation, the VAPID header or the content coding were wrong, this test fails
// and a stub would not.

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/catalyst/billing/internal/notify/webpush"
)

// pushEnv is an authenticated customer with push configured.
type pushEnv struct {
	*authEnv
	userID string
	cookie *http.Cookie
	csrf   string
	pub    string
}

// newPushEnv creates a customer and turns web push on with a fresh VAPID pair.
func newPushEnv(t *testing.T) *pushEnv {
	t.Helper()
	env := &pushEnv{authEnv: newAuthEnv(t)}
	env.createUser(t, "push@example.test", "push password 1", "")
	env.cookie, env.csrf = env.login(t, "push@example.test", "push password 1")

	var id string
	if err := env.pool.QueryRow(context.Background(),
		`SELECT id FROM users WHERE email = 'push@example.test'`).Scan(&id); err != nil {
		t.Fatalf("read user id: %v", err)
	}
	env.userID = id

	pub, priv, err := webpush.GenerateKeyPair()
	if err != nil {
		t.Fatalf("generate VAPID pair: %v", err)
	}
	env.pub = pub
	env.setPush(t, map[string]string{
		"enabled":           "true",
		"vapid_public_key":  pub,
		"vapid_private_key": priv,
		"subject":           "mailto:ops@example.test",
	})
	return env
}

// setPush writes push settings and removes them when the test ends.
//
// `settings` is reference data, so the harness does not truncate it: a row written by one test
// would otherwise change the behaviour of every later test in the package.
func (e *pushEnv) setPush(t *testing.T, values map[string]string) {
	t.Helper()
	for key, value := range values {
		if _, err := e.pool.Exec(context.Background(), `
			INSERT INTO settings (group_key, key, value) VALUES ('push', $1, $2)
			ON CONFLICT (group_key, key) DO UPDATE SET value = EXCLUDED.value`,
			key, value); err != nil {
			t.Fatalf("set push.%s: %v", key, err)
		}
	}
	t.Cleanup(func() {
		_, _ = e.pool.Exec(context.Background(),
			`DELETE FROM settings WHERE group_key = 'push'`)
	})
}

// clearPush removes every push setting, which is the state of a fresh installation.
func (e *pushEnv) clearPush(t *testing.T) {
	t.Helper()
	if _, err := e.pool.Exec(context.Background(),
		`DELETE FROM settings WHERE group_key = 'push'`); err != nil {
		t.Fatalf("clear push settings: %v", err)
	}
}

// withPushHTTP points delivery at a local push service and restores the default afterwards.
//
// The transport is process-wide, so a test that leaves it set would hand the next test a client
// whose server has been closed.
func (e *pushEnv) withPushHTTP(t *testing.T, client *http.Client) {
	t.Helper()
	e.srv.WithPushHTTP(client)
	t.Cleanup(func() { e.srv.WithPushHTTP(nil) })
}

// do sends a request as the signed-in customer.
func (e *pushEnv) do(t *testing.T, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		reader = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, path, reader)
	req.AddCookie(e.cookie)
	req.Header.Set("X-CSRF-Token", e.csrf)
	req.Header.Set("Content-Type", "application/json")
	return e.doRequest(t, req)
}

// device is a browser: a key pair, an auth secret and the subscription JSON it would send.
type pushDevice struct {
	private  *ecdh.PrivateKey
	auth     string
	endpoint string
	body     map[string]any
}

func newPushDevice(t *testing.T, endpoint string) pushDevice {
	t.Helper()
	key, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate device key: %v", err)
	}
	auth := make([]byte, 16)
	if _, err := rand.Read(auth); err != nil {
		t.Fatalf("auth secret: %v", err)
	}
	enc := base64.RawURLEncoding.EncodeToString
	return pushDevice{
		private:  key,
		auth:     enc(auth),
		endpoint: endpoint,
		body: map[string]any{
			"endpoint": endpoint,
			"keys": map[string]string{
				"p256dh": enc(key.PublicKey().Bytes()),
				"auth":   enc(auth),
			},
		},
	}
}

// ----------------------------------------------------------------- config --

// Without keys the feature is unavailable, and the portal is told why so it can hide the control
// instead of offering one that cannot work.
func TestPushConfigHidesTheFeatureWithoutKeys(t *testing.T) {
	env := newPushEnv(t)
	env.clearPush(t)

	rec := env.do(t, http.MethodGet, "/api/v1/push/config", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("config: status %d, body %s", rec.Code, rec.Body)
	}
	var body struct {
		Enabled    bool   `json:"enabled"`
		PublicKey  string `json:"vapid_public_key"`
		Reason     string `json:"reason"`
		MaxDevices int    `json:"max_devices"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("config body: %v (%s)", err, rec.Body)
	}
	if body.Enabled {
		t.Fatal("push is enabled with no keys configured")
	}
	if body.PublicKey != "" {
		t.Fatalf("a public key was served with push disabled: %q", body.PublicKey)
	}
	if body.Reason != "disabled" {
		t.Fatalf("reason = %q, want disabled", body.Reason)
	}

	// Enabled but with only half the pair: still unavailable, and the reason says which half is
	// missing rather than blaming the operator's browser.
	env.setPush(t, map[string]string{"enabled": "true", "vapid_public_key": env.pub})
	rec = env.do(t, http.MethodGet, "/api/v1/push/config", nil)
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body.Enabled || body.Reason != "keys_missing" {
		t.Fatalf("enabled=%v reason=%q, want unavailable/keys_missing", body.Enabled, body.Reason)
	}
}

// A key pair that is present but unusable is reported as unavailable, not as a working feature
// that fails on every send.
func TestPushConfigRefusesAnUnusableKeyPair(t *testing.T) {
	env := newPushEnv(t)
	otherPub, _, err := webpush.GenerateKeyPair()
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	env.setPush(t, map[string]string{"vapid_public_key": otherPub})

	rec := env.do(t, http.MethodGet, "/api/v1/push/config", nil)
	var body struct {
		Enabled bool   `json:"enabled"`
		Reason  string `json:"reason"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("config body: %v", err)
	}
	if body.Enabled || body.Reason != "keys_invalid" {
		t.Fatalf("enabled=%v reason=%q, want unavailable/keys_invalid", body.Enabled, body.Reason)
	}
}

// With keys configured the portal gets the key it must pass to pushManager.subscribe.
func TestPushConfigServesThePublicKey(t *testing.T) {
	env := newPushEnv(t)

	rec := env.do(t, http.MethodGet, "/api/v1/push/config", nil)
	var body struct {
		Enabled    bool   `json:"enabled"`
		PublicKey  string `json:"vapid_public_key"`
		MaxDevices int    `json:"max_devices"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("config body: %v", err)
	}
	if !body.Enabled {
		t.Fatal("push is not enabled")
	}
	if body.PublicKey != env.pub {
		t.Fatalf("public key = %q, want %q", body.PublicKey, env.pub)
	}
	if body.MaxDevices <= 0 {
		t.Fatalf("max_devices = %d", body.MaxDevices)
	}
}

// ----------------------------------------------------------- subscriptions --

// A device is registered once, and re-subscribing the same endpoint replaces the row.
func TestPushSubscribeReplacesTheSameDevice(t *testing.T) {
	env := newPushEnv(t)
	d := newPushDevice(t, "https://push.example.test/send/one")

	rec := env.do(t, http.MethodPost, "/api/v1/push/subscriptions", d.body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("subscribe: status %d, body %s", rec.Code, rec.Body)
	}
	var first struct {
		ID       string `json:"id"`
		Endpoint string `json:"endpoint"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &first); err != nil {
		t.Fatalf("subscribe body: %v (%s)", err, rec.Body)
	}
	if first.Endpoint != d.endpoint {
		t.Fatalf("endpoint = %q", first.Endpoint)
	}
	// The secret is never echoed back.
	if strings.Contains(rec.Body.String(), d.auth) {
		t.Fatal("the auth secret was returned to the client")
	}

	// The browser rotated its keys; the same endpoint subscribes again.
	again := newPushDevice(t, d.endpoint)
	rec = env.do(t, http.MethodPost, "/api/v1/push/subscriptions", again.body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("re-subscribe: status %d, body %s", rec.Code, rec.Body)
	}
	var second struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &second)
	if second.ID != first.ID {
		t.Fatalf("re-subscribing created a second device (%s, was %s)", second.ID, first.ID)
	}

	rec = env.do(t, http.MethodGet, "/api/v1/push/subscriptions", nil)
	var list struct {
		Data []struct {
			ID       string `json:"id"`
			Endpoint string `json:"endpoint"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("list body: %v (%s)", err, rec.Body)
	}
	if len(list.Data) != 1 {
		t.Fatalf("listed %d devices, want 1", len(list.Data))
	}
}

// A subscription that could never receive anything is refused at the write path with a code the
// screen can explain.
func TestPushSubscribeRejectsAnUnusableSubscription(t *testing.T) {
	env := newPushEnv(t)

	cases := map[string]map[string]any{
		"http endpoint": {
			"endpoint": "http://push.example.test/send/x",
			"keys":     map[string]string{"p256dh": b64url(bytes.Repeat([]byte{0x04}, 65)), "auth": b64url(bytes.Repeat([]byte{1}, 16))},
		},
		"off-curve key": {
			"endpoint": "https://push.example.test/send/x",
			"keys":     map[string]string{"p256dh": b64url(bytes.Repeat([]byte{0x04}, 65)), "auth": b64url(bytes.Repeat([]byte{1}, 16))},
		},
		"short auth secret": {
			"endpoint": "https://push.example.test/send/x",
			"keys":     map[string]string{"p256dh": newPushDevice(t, "https://push.example.test/x").body["keys"].(map[string]string)["p256dh"], "auth": b64url([]byte("short"))},
		},
	}
	for name, body := range cases {
		rec := env.do(t, http.MethodPost, "/api/v1/push/subscriptions", body)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("%s: status %d, want 422; body %s", name, rec.Code, rec.Body)
		}
		if p := decodeProblem(t, rec); p.Code != "invalid_subscription" {
			t.Fatalf("%s: code = %q, want invalid_subscription", name, p.Code)
		}
	}
}

// Subscribing is refused when push is not configured, so the database cannot fill with devices
// that can never be delivered to.
func TestPushSubscribeRequiresConfiguration(t *testing.T) {
	env := newPushEnv(t)
	env.clearPush(t)

	rec := env.do(t, http.MethodPost, "/api/v1/push/subscriptions",
		newPushDevice(t, "https://push.example.test/send/x").body)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503; body %s", rec.Code, rec.Body)
	}
	if p := decodeProblem(t, rec); p.Code != "push_not_configured" {
		t.Fatalf("code = %q, want push_not_configured", p.Code)
	}
}

// Another account's device id is a 404, not a 403: the delete is scoped in SQL, so the answer
// cannot be used to learn which ids exist.
func TestPushUnsubscribeIsScopedToTheCaller(t *testing.T) {
	env := newPushEnv(t)
	d := newPushDevice(t, "https://push.example.test/send/mine")
	rec := env.do(t, http.MethodPost, "/api/v1/push/subscriptions", d.body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("subscribe: status %d, body %s", rec.Code, rec.Body)
	}
	var saved struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &saved)

	// A second customer.
	env.createUser(t, "other-push@example.test", "other password 1", "")
	otherCookie, otherCSRF := env.login(t, "other-push@example.test", "other password 1")
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/push/subscriptions/"+saved.ID, nil)
	req.AddCookie(otherCookie)
	req.Header.Set("X-CSRF-Token", otherCSRF)
	rec = env.doRequest(t, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("another account's delete: status %d, want 404; body %s", rec.Code, rec.Body)
	}
	if p := decodeProblem(t, rec); p.Code != "not_found" {
		t.Fatalf("code = %q, want not_found", p.Code)
	}

	// The owner can.
	rec = env.do(t, http.MethodDelete, "/api/v1/push/subscriptions/"+saved.ID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("owner delete: status %d, body %s", rec.Code, rec.Body)
	}
	rec = env.do(t, http.MethodGet, "/api/v1/push/subscriptions", nil)
	var list struct {
		Data []json.RawMessage `json:"data"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &list)
	if len(list.Data) != 0 {
		t.Fatalf("%d devices left after delete", len(list.Data))
	}
}

// The push routes need a session: a subscription is the caller's own device, but "own" requires
// knowing who the caller is.
func TestPushRoutesRequireASession(t *testing.T) {
	env := newPushEnv(t)

	for _, path := range []string{"/api/v1/push/config", "/api/v1/push/subscriptions"} {
		rec := env.get(t, path, nil)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("anonymous %s: status %d, want 401", path, rec.Code)
		}
	}
	// A mutation without a session is refused before the handler runs. The code is 403 rather
	// than 401 because the CSRF wrapper is outermost and a request with no token cannot get
	// past it — the same answer every session mutation in this server gives, and the reason it
	// is not worth asserting a specific code here beyond "refused".
	req := httptest.NewRequest(http.MethodPost, "/api/v1/push/test", nil)
	rec := env.doRequest(t, req)
	if rec.Code != http.StatusUnauthorized && rec.Code != http.StatusForbidden {
		t.Fatalf("anonymous test push: status %d, want 401 or 403", rec.Code)
	}
}

// ------------------------------------------------------------------- test --

// With no device there is nothing to send to, and the answer says so rather than pretending.
func TestTestPushNeedsADevice(t *testing.T) {
	env := newPushEnv(t)

	rec := env.do(t, http.MethodPost, "/api/v1/push/test", nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status %d, want 409; body %s", rec.Code, rec.Body)
	}
	if p := decodeProblem(t, rec); p.Code != "no_push_device" {
		t.Fatalf("code = %q, want no_push_device", p.Code)
	}
}

// The test push is transmitted, and what arrives decrypts with the device's private key.
func TestTestPushDeliversAnEncryptedPayload(t *testing.T) {
	env := newPushEnv(t)

	var (
		received []byte
		headers  http.Header
	)
	pushService := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		headers = r.Header.Clone()
		received, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusCreated)
	}))
	defer pushService.Close()

	// The server must trust the local push service's certificate.
	env.withPushHTTP(t, pushService.Client())

	d := newPushDevice(t, pushService.URL+"/send/http-test")
	rec := env.do(t, http.MethodPost, "/api/v1/push/subscriptions", d.body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("subscribe: status %d, body %s", rec.Code, rec.Body)
	}

	rec = env.do(t, http.MethodPost, "/api/v1/push/test", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("test push: status %d, body %s", rec.Code, rec.Body)
	}
	var res struct {
		Sent     int      `json:"sent"`
		Removed  int      `json:"removed"`
		Failed   int      `json:"failed"`
		Failures []string `json:"failures"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("result body: %v (%s)", err, rec.Body)
	}
	if res.Sent != 1 || res.Failed != 0 {
		t.Fatalf("result = %+v", res)
	}

	if headers.Get("Content-Encoding") != "aes128gcm" {
		t.Fatalf("Content-Encoding = %q", headers.Get("Content-Encoding"))
	}
	if !strings.HasPrefix(headers.Get("Authorization"), "vapid t=") {
		t.Fatalf("Authorization = %q", headers.Get("Authorization"))
	}

	plain, err := webpush.Decrypt(d.private, d.auth, received)
	if err != nil {
		t.Fatalf("the delivered body does not decrypt: %v", err)
	}
	var msg struct {
		Title string `json:"title"`
		URL   string `json:"url"`
	}
	if err := json.Unmarshal(plain, &msg); err != nil {
		t.Fatalf("payload is not JSON: %v (%s)", err, plain)
	}
	if msg.Title == "" {
		t.Fatalf("payload has no title: %s", plain)
	}
	if msg.URL != "/portal/notifications" {
		t.Fatalf("payload url = %q, want the panel path", msg.URL)
	}
}

// A push service saying 404 or 410 means the device is gone: the subscription is removed and the
// caller is told there is nothing left to send to, rather than being shown an error.
func TestTestPushRemovesADeadDevice(t *testing.T) {
	env := newPushEnv(t)

	pushService := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusGone)
	}))
	defer pushService.Close()
	env.withPushHTTP(t, pushService.Client())

	d := newPushDevice(t, pushService.URL+"/send/gone")
	if rec := env.do(t, http.MethodPost, "/api/v1/push/subscriptions", d.body); rec.Code != http.StatusCreated {
		t.Fatalf("subscribe: status %d, body %s", rec.Code, rec.Body)
	}

	rec := env.do(t, http.MethodPost, "/api/v1/push/test", nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status %d, want 409; body %s", rec.Code, rec.Body)
	}
	if p := decodeProblem(t, rec); p.Code != "no_push_device" {
		t.Fatalf("code = %q, want no_push_device", p.Code)
	}

	rec = env.do(t, http.MethodGet, "/api/v1/push/subscriptions", nil)
	var list struct {
		Data []json.RawMessage `json:"data"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &list)
	if len(list.Data) != 0 {
		t.Fatal("a dead device was kept")
	}
}

// A push service that refuses the credentials is a downstream fault, reported as such: the
// subscription is kept, because a configuration problem is not a dead phone.
func TestTestPushReportsAPushServiceFailure(t *testing.T) {
	env := newPushEnv(t)

	pushService := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"code":"BadJwtToken"}`))
	}))
	defer pushService.Close()
	env.withPushHTTP(t, pushService.Client())

	d := newPushDevice(t, pushService.URL+"/send/forbidden")
	if rec := env.do(t, http.MethodPost, "/api/v1/push/subscriptions", d.body); rec.Code != http.StatusCreated {
		t.Fatalf("subscribe: status %d, body %s", rec.Code, rec.Body)
	}

	rec := env.do(t, http.MethodPost, "/api/v1/push/test", nil)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status %d, want 502; body %s", rec.Code, rec.Body)
	}
	if p := decodeProblem(t, rec); p.Code != "push_delivery_failed" {
		t.Fatalf("code = %q, want push_delivery_failed", p.Code)
	}

	rec = env.do(t, http.MethodGet, "/api/v1/push/subscriptions", nil)
	var list struct {
		Data []json.RawMessage `json:"data"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &list)
	if len(list.Data) != 1 {
		t.Fatal("a device was deleted over a transient push service failure")
	}
}

// b64url is the encoding a browser uses for its keys.
func b64url(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
