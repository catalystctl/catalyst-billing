package webpush_test

// The tests that make RFC 8291 correctness a fact rather than a hope.
//
// The central one is TestEncryptDecryptRoundTrip: it generates a receiver key pair the way a
// browser does, encrypts to it, then decrypts with the receiver's *private* key and asserts the
// payload comes back byte for byte. Nothing weaker would do. The push service cannot tell a
// correctly encrypted body from an incorrectly encrypted one — it accepts both and the
// notification silently never appears — so a test that only asserted "the request was sent"
// would pass with the derivation backwards, the keys in the wrong order, or the delimiter
// missing.
//
// The second is TestSendDeliversAnEncryptedPayload, which puts the encryption behind the real
// HTTP path (VAPID header, TTL, Urgency, content coding) and decrypts what arrived on the other
// side. Together they cover the two halves that can drift: the crypto and the wire.

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/catalyst/billing/internal/notify/webpush"
)

// b64 is the encoding every browser-facing key uses.
func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// receiver is a device: a P-256 key pair and a 16-byte auth secret, exactly what a browser
// generates when it subscribes.
type receiver struct {
	private *ecdh.PrivateKey
	sub     webpush.Subscription
	auth    string
}

func newReceiver(t *testing.T) receiver {
	t.Helper()
	key, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate receiver key: %v", err)
	}
	auth := make([]byte, 16)
	if _, err := rand.Read(auth); err != nil {
		t.Fatalf("auth secret: %v", err)
	}
	return receiver{
		private: key,
		auth:    b64(auth),
		sub: webpush.Subscription{
			Endpoint: "https://push.example.test/send/abc123",
			P256DH:   b64(key.PublicKey().Bytes()),
			Auth:     b64(auth),
		},
	}
}

// --------------------------------------------------------------- encryption --

// A payload survives the trip: encrypted to the device's public key, decrypted with the
// device's private key, identical.
func TestEncryptDecryptRoundTrip(t *testing.T) {
	r := newReceiver(t)

	// Deliberately awkward text: multi-byte UTF-8, an emoji outside the BMP, and a NUL. A
	// derivation that treated the payload as text, or a padding step that trimmed bytes, would
	// fail on one of these rather than on "hello".
	payload := []byte(`{"title":"Invoice 000042 is ready","body":"Amount due: R$ 1.234,56 — €99,00 🧾\u0000end"}`)

	body, err := webpush.Encrypt(r.sub, payload)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}

	// The frame: salt(16) + rs(4) + idlen(1) + keyid(65) + ciphertext + tag(16).
	if want := 16 + 4 + 1 + 65 + len(payload) + 1 + 16; len(body) != want {
		t.Fatalf("body is %d bytes, want %d", len(body), want)
	}
	if rs := int(body[16])<<24 | int(body[17])<<16 | int(body[18])<<8 | int(body[19]); rs != 4096 {
		t.Fatalf("record size in the header is %d, want 4096", rs)
	}
	if body[20] != 65 {
		t.Fatalf("key id length is %d, want 65", body[20])
	}
	if body[21] != 0x04 {
		t.Fatalf("key id does not start with an uncompressed point marker: 0x%02x", body[21])
	}

	got, err := webpush.Decrypt(r.private, r.auth, body)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("payload did not round-trip:\n got %q\nwant %q", got, payload)
	}
}

// A wrong auth secret must not decrypt. This is the check that the auth secret is actually
// mixed into the derivation rather than being carried along for show: a receiver that ignored
// it would still round-trip above, and would accept a body encrypted for someone else.
func TestDecryptRejectsAWrongAuthSecret(t *testing.T) {
	r := newReceiver(t)
	body, err := webpush.Encrypt(r.sub, []byte("secret"))
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	other := make([]byte, 16)
	if _, err := rand.Read(other); err != nil {
		t.Fatalf("auth secret: %v", err)
	}
	if _, err := webpush.Decrypt(r.private, b64(other), body); err == nil {
		t.Fatal("a body decrypted under the wrong auth secret")
	}
}

// A wrong key must not decrypt, and a tampered ciphertext must not authenticate.
func TestDecryptRejectsATamperedBody(t *testing.T) {
	r := newReceiver(t)
	body, err := webpush.Encrypt(r.sub, []byte("the amount is 10.00"))
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	// Flip one bit of the ciphertext. GCM authenticates, so this must fail rather than
	// decrypt to a different amount — which is the whole reason the money sentence is safe.
	tampered := append([]byte(nil), body...)
	tampered[len(tampered)-20] ^= 0x01
	if _, err := webpush.Decrypt(r.private, r.auth, tampered); err == nil {
		t.Fatal("a tampered body decrypted")
	}

	other, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	if _, err := webpush.Decrypt(other, r.auth, body); err == nil {
		t.Fatal("a body decrypted with the wrong private key")
	}
}

// Two encryptions of the same payload differ: the ephemeral key and the salt are fresh each
// time, so a push service (or an observer) cannot tell that two notifications are identical.
func TestEncryptIsNotDeterministic(t *testing.T) {
	r := newReceiver(t)
	first, err := webpush.Encrypt(r.sub, []byte("same text"))
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	second, err := webpush.Encrypt(r.sub, []byte("same text"))
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if bytes.Equal(first, second) {
		t.Fatal("two encryptions of the same payload are identical")
	}
	for _, body := range [][]byte{first, second} {
		got, err := webpush.Decrypt(r.private, r.auth, body)
		if err != nil {
			t.Fatalf("decrypt: %v", err)
		}
		if string(got) != "same text" {
			t.Fatalf("payload = %q", got)
		}
	}
}

// A payload larger than a push message can carry is refused here rather than truncated by a
// push service the caller cannot see.
func TestEncryptRefusesAnOversizePayload(t *testing.T) {
	r := newReceiver(t)
	if _, err := webpush.Encrypt(r.sub, bytes.Repeat([]byte("x"), webpush.MaxPayload)); err != nil {
		t.Fatalf("a payload at the limit was refused: %v", err)
	}
	if _, err := webpush.Encrypt(r.sub, bytes.Repeat([]byte("x"), webpush.MaxPayload+1)); !errors.Is(err, webpush.ErrTooLarge) {
		t.Fatalf("oversize payload: got %v, want ErrTooLarge", err)
	}
}

// A malformed subscription is refused with a clear error rather than producing a body the push
// service will accept and the device will drop.
func TestEncryptRefusesAMalformedSubscription(t *testing.T) {
	r := newReceiver(t)

	bad := r.sub
	bad.P256DH = b64(bytes.Repeat([]byte{0x04}, 65)) // right length, not a point
	if _, err := webpush.Encrypt(bad, []byte("x")); !errors.Is(err, webpush.ErrInvalidKey) {
		t.Fatalf("off-curve key: got %v, want ErrInvalidKey", err)
	}

	bad = r.sub
	bad.Auth = b64([]byte("too short"))
	if _, err := webpush.Encrypt(bad, []byte("x")); !errors.Is(err, webpush.ErrInvalidAuthSecret) {
		t.Fatalf("short auth secret: got %v, want ErrInvalidAuthSecret", err)
	}
}

// ---------------------------------------------------------------- VAPID keys --

func TestParsePrivateKeyAcceptsEveryShippedEncoding(t *testing.T) {
	pubB64, privB64, err := webpush.GenerateKeyPair()
	if err != nil {
		t.Fatalf("generate pair: %v", err)
	}
	// The generated pair must be usable and must agree with itself.
	client, err := webpush.NewClient(webpush.Config{
		PublicKey: pubB64, PrivateKey: privB64, Subject: "mailto:ops@example.test",
	})
	if err != nil {
		t.Fatalf("NewClient with a generated pair: %v", err)
	}
	if client.PublicKey() != pubB64 {
		t.Fatalf("PublicKey() = %q, want %q", client.PublicKey(), pubB64)
	}

	// PEM forms, which is what openssl and a certificate pipeline produce.
	raw, err := base64.RawURLEncoding.DecodeString(privB64)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	der, err := x509.MarshalECPrivateKey(mustECDSA(t, raw))
	if err != nil {
		t.Fatalf("marshal SEC1: %v", err)
	}
	sec1 := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})
	if _, err := webpush.NewClient(webpush.Config{
		PublicKey: pubB64, PrivateKey: string(sec1), Subject: "mailto:ops@example.test",
	}); err != nil {
		t.Fatalf("NewClient with a SEC1 PEM private key: %v", err)
	}
	pkcs8, err := x509.MarshalPKCS8PrivateKey(mustECDSA(t, raw))
	if err != nil {
		t.Fatalf("marshal PKCS8: %v", err)
	}
	block := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8})
	if _, err := webpush.NewClient(webpush.Config{
		PublicKey: pubB64, PrivateKey: string(block), Subject: "mailto:ops@example.test",
	}); err != nil {
		t.Fatalf("NewClient with a PKCS8 PEM private key: %v", err)
	}
}

// A public key that is not the public half of the private key is refused at construction: the
// browser would subscribe with one key and the token would be signed with the other, and every
// delivery would fail with a 403 that looks like a configuration error because it is one.
func TestNewClientRefusesAMismatchedKeyPair(t *testing.T) {
	pubA, _, err := webpush.GenerateKeyPair()
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	_, privB, err := webpush.GenerateKeyPair()
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	_, err = webpush.NewClient(webpush.Config{
		PublicKey: pubA, PrivateKey: privB, Subject: "mailto:ops@example.test",
	})
	if err == nil {
		t.Fatal("a mismatched VAPID key pair was accepted")
	}
	if !strings.Contains(err.Error(), "not the public half") {
		t.Fatalf("error does not explain the problem: %v", err)
	}
}

func TestNewClientRequiresAProperSubject(t *testing.T) {
	pub, priv, err := webpush.GenerateKeyPair()
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	for _, subject := range []string{"", "ops@example.test", "ftp://example.test"} {
		if _, err := webpush.NewClient(webpush.Config{
			PublicKey: pub, PrivateKey: priv, Subject: subject,
		}); err == nil {
			t.Fatalf("subject %q was accepted", subject)
		}
	}
	for _, subject := range []string{"mailto:ops@example.test", "https://example.test/ops"} {
		if _, err := webpush.NewClient(webpush.Config{
			PublicKey: pub, PrivateKey: priv, Subject: subject,
		}); err != nil {
			t.Fatalf("subject %q was refused: %v", subject, err)
		}
	}
}

func mustECDSA(t *testing.T, rawScalar []byte) *ecdsa.PrivateKey {
	t.Helper()
	key, err := webpush.ParsePrivateKey(b64(rawScalar))
	if err != nil {
		t.Fatalf("parse raw scalar: %v", err)
	}
	return key
}

// ---------------------------------------------------------------- delivery --

// The real HTTP path: the body that arrives at the push service decrypts to the payload that
// was sent, and the headers are the ones the protocol requires.
func TestSendDeliversAnEncryptedPayload(t *testing.T) {
	r := newReceiver(t)

	var (
		gotAuth     string
		gotEncoding string
		gotTTL      string
		gotUrgency  string
		gotBody     []byte
	)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		gotAuth = req.Header.Get("Authorization")
		gotEncoding = req.Header.Get("Content-Encoding")
		gotTTL = req.Header.Get("TTL")
		gotUrgency = req.Header.Get("Urgency")
		gotBody, _ = io.ReadAll(req.Body)
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	pub, priv, err := webpush.GenerateKeyPair()
	if err != nil {
		t.Fatalf("generate pair: %v", err)
	}
	client, err := webpush.NewClient(webpush.Config{
		PublicKey: pub, PrivateKey: priv, Subject: "mailto:ops@example.test",
		TTL: 2 * time.Hour, Urgency: "high", HTTP: srv.Client(),
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	sub := r.sub
	sub.Endpoint = srv.URL + "/send/abc123"
	payload := []byte(`{"title":"Test notification","body":"Push is working."}`)
	if err := client.Send(context.Background(), sub, payload); err != nil {
		t.Fatalf("send: %v", err)
	}

	if gotEncoding != "aes128gcm" {
		t.Fatalf("Content-Encoding = %q, want aes128gcm", gotEncoding)
	}
	if gotTTL != "7200" {
		t.Fatalf("TTL = %q, want 7200", gotTTL)
	}
	if gotUrgency != "high" {
		t.Fatalf("Urgency = %q, want high", gotUrgency)
	}

	// Authorization: vapid t=<jwt>,k=<public key>
	const prefix = "vapid t="
	if !strings.HasPrefix(gotAuth, prefix) {
		t.Fatalf("Authorization = %q, want a vapid scheme", gotAuth)
	}
	token, key, ok := strings.Cut(strings.TrimPrefix(gotAuth, prefix), ",k=")
	if !ok {
		t.Fatalf("Authorization has no k= parameter: %q", gotAuth)
	}
	if key != pub {
		t.Fatalf("Authorization k = %q, want the configured public key %q", key, pub)
	}
	claims := decodeClaims(t, token)
	if want := strings.TrimSuffix(srv.URL, "/"); claims["aud"] != want {
		t.Fatalf("token aud = %v, want the push service origin %q", claims["aud"], want)
	}
	if claims["sub"] != "mailto:ops@example.test" {
		t.Fatalf("token sub = %v", claims["sub"])
	}

	// The proof: the device's own private key opens what the server sent.
	plain, err := webpush.Decrypt(r.private, r.auth, gotBody)
	if err != nil {
		t.Fatalf("the delivered body does not decrypt: %v", err)
	}
	if !bytes.Equal(plain, payload) {
		t.Fatalf("delivered payload = %q, want %q", plain, payload)
	}
}

// decodeClaims reads a JWT's claim set without verifying it. The signature is verified in
// vapid_internal_test.go; here the question is what the token says.
func decodeClaims(t *testing.T, token string) map[string]any {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("token has %d parts, want 3", len(parts))
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decode claims: %v", err)
	}
	var claims map[string]any
	if err := json.Unmarshal(raw, &claims); err != nil {
		t.Fatalf("unmarshal claims: %v", err)
	}
	return claims
}

// 404 and 410 mean the device is gone. The caller must be able to branch on that without
// parsing a string, because the right response is to delete the subscription rather than retry.
func TestSendReportsADeadSubscription(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusGone} {
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"code":"Gone","message":"subscription expired"}`))
		}))
		r := newReceiver(t)
		sub := r.sub
		sub.Endpoint = srv.URL + "/send/abc123"
		client := testClient(t, srv.Client())

		err := client.Send(context.Background(), sub, []byte("x"))
		if !errors.Is(err, webpush.ErrGone) {
			t.Fatalf("status %d: got %v, want ErrGone", status, err)
		}
		var statusErr *webpush.StatusError
		if !errors.As(err, &statusErr) || statusErr.Status != status {
			t.Fatalf("status %d: the error does not carry the status: %v", status, err)
		}
		if statusErr.Code != "Gone" {
			t.Fatalf("status %d: code = %q, want the push service's own code", status, statusErr.Code)
		}
		srv.Close()
	}
}

// The other statuses map to their own sentinels: a configuration fault and a throttle are not a
// dead device, and deleting the subscription over one would silently unsubscribe a working
// phone.
func TestSendMapsStatusesToSentinels(t *testing.T) {
	cases := []struct {
		status int
		want   error
	}{
		{http.StatusUnauthorized, webpush.ErrUnauthorized},
		{http.StatusForbidden, webpush.ErrUnauthorized},
		{http.StatusRequestEntityTooLarge, webpush.ErrTooLarge},
		{http.StatusTooManyRequests, webpush.ErrRateLimited},
		{http.StatusInternalServerError, webpush.ErrRejected},
	}
	for _, c := range cases {
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(c.status)
		}))
		r := newReceiver(t)
		sub := r.sub
		sub.Endpoint = srv.URL + "/send/abc123"
		client := testClient(t, srv.Client())
		err := client.Send(context.Background(), sub, []byte("x"))
		if !errors.Is(err, c.want) {
			t.Fatalf("status %d: got %v, want %v", c.status, err, c.want)
		}
		if errors.Is(err, webpush.ErrGone) {
			t.Fatalf("status %d was reported as a dead subscription", c.status)
		}
		srv.Close()
	}
}

// An http:// endpoint is refused before a request is made. The endpoint is user-supplied and
// this server POSTs to it, so the scheme is the one constraint that can be enforced locally.
func TestSendRefusesANonTLS(t *testing.T) {
	r := newReceiver(t)
	client := testClient(t, http.DefaultClient)
	sub := r.sub
	sub.Endpoint = "http://push.example.test/send/abc"
	if err := client.Send(context.Background(), sub, []byte("x")); err == nil {
		t.Fatal("an http endpoint was accepted")
	}
}

// The token is cached per audience: an installation that sends to two push services signs two
// tokens, not one per message.
func TestVAPIDTokenIsCachedPerAudience(t *testing.T) {
	var tokens []string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		auth := req.Header.Get("Authorization")
		token, _, _ := strings.Cut(strings.TrimPrefix(auth, "vapid t="), ",k=")
		tokens = append(tokens, token)
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	r := newReceiver(t)
	sub := r.sub
	sub.Endpoint = srv.URL + "/send/abc"
	client := testClient(t, srv.Client())

	for i := 0; i < 3; i++ {
		if err := client.Send(context.Background(), sub, []byte("x")); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
	}
	if len(tokens) != 3 {
		t.Fatalf("the push service saw %d requests, want 3", len(tokens))
	}
	for i, token := range tokens {
		if token != tokens[0] {
			t.Fatalf("request %d carried a different token; the cache is not working", i)
		}
	}
}

// testClient builds a client whose clock and keys are fixed enough for a test.
func testClient(t *testing.T, httpClient *http.Client) *webpush.Client {
	t.Helper()
	pub, priv, err := webpush.GenerateKeyPair()
	if err != nil {
		t.Fatalf("generate pair: %v", err)
	}
	client, err := webpush.NewClient(webpush.Config{
		PublicKey: pub, PrivateKey: priv, Subject: "mailto:ops@example.test",
		HTTP: httpClient,
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return client
}
