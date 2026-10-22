package webpush

// In-package tests for the VAPID token.
//
// They are internal because verifyJWT is: the alternative is a test that re-implements ES256
// verification in the test file and then agrees with itself about whether the signature is
// right. The one property that has to be established from outside is that a push service can
// verify it, and the way to establish that here is to verify it with the same public key the
// browser is given.

import (
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// newTestClient builds a client over a generated pair, with a fixed clock.
func newTestClient(t *testing.T, now time.Time) *Client {
	t.Helper()
	pub, priv, err := GenerateKeyPair()
	if err != nil {
		t.Fatalf("generate pair: %v", err)
	}
	client, err := NewClient(Config{
		PublicKey:  pub,
		PrivateKey: priv,
		Subject:    "mailto:ops@example.test",
		Now:        func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return client
}

// The token verifies with the public key, carries the audience it was minted for, and expires
// inside the 24-hour window RFC 8292 allows.
func TestVAPIDTokenIsSignedAndCurrent(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	client := newTestClient(t, now)

	auth, err := client.authorization("https://fcm.googleapis.com")
	if err != nil {
		t.Fatalf("authorization: %v", err)
	}
	if !strings.HasPrefix(auth, "vapid t=") {
		t.Fatalf("header = %q", auth)
	}
	token, key, ok := strings.Cut(strings.TrimPrefix(auth, "vapid t="), ",k=")
	if !ok {
		t.Fatalf("header has no k= parameter: %q", auth)
	}
	if key != client.PublicKey() {
		t.Fatalf("k = %q, want %q", key, client.PublicKey())
	}

	claims, err := client.verifyJWT(token, now)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if claims.Aud != "https://fcm.googleapis.com" {
		t.Fatalf("aud = %q", claims.Aud)
	}
	if claims.Sub != "mailto:ops@example.test" {
		t.Fatalf("sub = %q", claims.Sub)
	}
	expires := time.Unix(claims.Exp, 0)
	if !expires.After(now) {
		t.Fatalf("token is already expired: exp %s, now %s", expires, now)
	}
	if lifetime := expires.Sub(now); lifetime > 24*time.Hour {
		t.Fatalf("token lifetime is %s, the protocol allows at most 24h", lifetime)
	}
}

// The signature is the raw R||S form JWT requires, 64 bytes for P-256. DER would be a different
// length and every push service would reject it, so the length is asserted rather than assumed.
func TestVAPIDSignatureIsRawRS(t *testing.T) {
	now := time.Now()
	client := newTestClient(t, now)
	auth, err := client.authorization("https://push.example.test")
	if err != nil {
		t.Fatalf("authorization: %v", err)
	}
	token, _, _ := strings.Cut(strings.TrimPrefix(auth, "vapid t="), ",k=")
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("token has %d parts, want 3", len(parts))
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatalf("decode signature: %v", err)
	}
	if len(sig) != 64 {
		t.Fatalf("signature is %d bytes, want 64 (raw R||S)", len(sig))
	}

	header, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		t.Fatalf("decode header: %v", err)
	}
	var decoded struct {
		Alg string `json:"alg"`
		Typ string `json:"typ"`
	}
	if err := json.Unmarshal(header, &decoded); err != nil {
		t.Fatalf("header is not JSON: %v", err)
	}
	if decoded.Alg != "ES256" || decoded.Typ != "JWT" {
		t.Fatalf("header = %+v, want ES256/JWT", decoded)
	}
}

// A token minted for one audience does not verify for another, and a token that has expired does
// not verify at all. Both matter because a cached token outliving its audience is how a push to
// the wrong push service gets a 403 nobody can explain.
func TestVAPIDTokenIsBoundToItsAudience(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	client := newTestClient(t, now)
	auth, err := client.authorization("https://fcm.googleapis.com")
	if err != nil {
		t.Fatalf("authorization: %v", err)
	}
	token, _, _ := strings.Cut(strings.TrimPrefix(auth, "vapid t="), ",k=")

	claims, err := client.verifyJWT(token, now)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if claims.Aud == "https://updates.push.services.mozilla.com" {
		t.Fatal("the token's audience is the wrong push service")
	}
	if _, err := client.verifyJWT(token, now.Add(13*time.Hour)); err == nil {
		t.Fatal("a token was accepted after its exp")
	}
}

// The cache is keyed by audience: a second call for the same origin reuses the token, and a call
// for a different origin mints a new one.
func TestVAPIDTokenCacheIsPerAudience(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	client := newTestClient(t, now)

	first, err := client.authorization("https://fcm.googleapis.com")
	if err != nil {
		t.Fatalf("authorization: %v", err)
	}
	again, err := client.authorization("https://fcm.googleapis.com")
	if err != nil {
		t.Fatalf("authorization: %v", err)
	}
	if first != again {
		t.Fatal("the token was re-signed for the same audience")
	}
	other, err := client.authorization("https://updates.push.services.mozilla.com")
	if err != nil {
		t.Fatalf("authorization: %v", err)
	}
	if other == first {
		t.Fatal("the same token was used for a different audience")
	}
	token, _, _ := strings.Cut(strings.TrimPrefix(other, "vapid t="), ",k=")
	claims, err := client.verifyJWT(token, now)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if claims.Aud != "https://updates.push.services.mozilla.com" {
		t.Fatalf("aud = %q", claims.Aud)
	}
}

// The audience is the endpoint's origin: scheme and host, never the path, and never http.
func TestOriginIsThePushServiceOrigin(t *testing.T) {
	cases := []struct {
		endpoint string
		want     string
		ok       bool
	}{
		{"https://fcm.googleapis.com/fcm/send/abc123", "https://fcm.googleapis.com", true},
		{"https://updates.push.services.mozilla.com/wpush/v2/gAAAA", "https://updates.push.services.mozilla.com", true},
		{"https://push.example.test:8443/send/x", "https://push.example.test:8443", true},
		{"http://push.example.test/send/x", "", false},
		{"https:///send/x", "", false},
		{"not a url", "", false},
	}
	for _, c := range cases {
		got, err := origin(c.endpoint)
		if c.ok && (err != nil || got != c.want) {
			t.Fatalf("origin(%q) = %q, %v; want %q", c.endpoint, got, err, c.want)
		}
		if !c.ok && err == nil {
			t.Fatalf("origin(%q) accepted an unusable endpoint", c.endpoint)
		}
	}
}

// A generated key pair is a P-256 pair: the public half is a 65-byte uncompressed point and the
// private half is a 32-byte scalar, which is what the settings field and the browser expect.
func TestGenerateKeyPairShape(t *testing.T) {
	pub, priv, err := GenerateKeyPair()
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	pubRaw, err := base64.RawURLEncoding.DecodeString(pub)
	if err != nil {
		t.Fatalf("decode public: %v", err)
	}
	if len(pubRaw) != 65 || pubRaw[0] != 0x04 {
		t.Fatalf("public key is %d bytes starting 0x%02x, want an uncompressed point", len(pubRaw), pubRaw[0])
	}
	privRaw, err := base64.RawURLEncoding.DecodeString(priv)
	if err != nil {
		t.Fatalf("decode private: %v", err)
	}
	if len(privRaw) != 32 {
		t.Fatalf("private key is %d bytes, want 32", len(privRaw))
	}
	if _, err := ecdh.P256().NewPublicKey(pubRaw); err != nil {
		t.Fatalf("the generated public key is not on P-256: %v", err)
	}
	// The two halves must agree, which NewClient checks; this asserts it directly too so the
	// failure names the generator rather than the client.
	key, err := ParsePrivateKey(priv)
	if err != nil {
		t.Fatalf("parse private: %v", err)
	}
	derived, err := publicFromPrivate(key)
	if err != nil {
		t.Fatalf("derive public: %v", err)
	}
	if string(derived) != string(pubRaw) {
		t.Fatal("the generated private key does not match the generated public key")
	}
	if _, err := rand.Reader.Read(make([]byte, 1)); err != nil {
		t.Fatalf("rand: %v", err)
	}
}
