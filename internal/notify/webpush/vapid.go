package webpush

// The VAPID token (RFC 8292): a short-lived ES256 JWT that tells the push service who is
// sending, and an unauthenticated public key beside it.
//
// # The four claims, and the one that goes wrong
//
//	aud  the push service's origin — scheme and host, no path. Not the subscription URL.
//	exp  at most 24 hours after issue; this package uses 12.
//	sub  a mailto: or https: URL for the sender, so the push service operator can reach
//	     whoever is generating traffic it does not like.
//
// `aud` is the one that goes wrong, and it goes wrong quietly: a token whose audience is the
// full subscription URL, or the wrong scheme, is refused with a 403 that says nothing useful.
// So the audience is derived by origin() from the endpoint being pushed to, never configured.
//
// # Why the signature is 64 raw bytes and not DER
//
// JWT's ES256 is defined over the *raw* concatenation of r and s, 32 bytes each, not over the
// ASN.1 SEQUENCE that crypto/ecdsa produces. Encoding it the obvious way (`SignASN1`) yields a
// token every push service rejects as malformed. Both halves are therefore zero-padded to
// exactly 32 bytes: a short r is common — roughly one signature in 256 — and dropping the
// padding produces a 63-byte signature that fails intermittently, which is the worst kind of
// bug to find.

import (
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math/big"
	"strings"
	"time"
)

// jwtHeader is fixed: there is exactly one algorithm VAPID allows.
var jwtHeader = base64URL([]byte(`{"typ":"JWT","alg":"ES256"}`))

// vapidClaims is the claim set, marshalled rather than formatted.
//
// json.Marshal rather than fmt.Sprintf because `sub` is operator-supplied text: a subject
// containing a quote would otherwise produce a token with a broken claim set, and the failure
// would be a 403 from a third party with no explanation.
type vapidClaims struct {
	Aud string `json:"aud"`
	Exp int64  `json:"exp"`
	Sub string `json:"sub"`
}

// signJWT builds a VAPID token for one audience.
func (c *Client) signJWT(audience string, now, expires time.Time) (string, error) {
	claims, err := json.Marshal(vapidClaims{
		Aud: audience,
		Exp: expires.Unix(),
		Sub: c.subject,
	})
	if err != nil {
		return "", fmt.Errorf("webpush: encode VAPID claims: %w", err)
	}
	signingInput := jwtHeader + "." + base64URL(claims)
	digest := sha256.Sum256([]byte(signingInput))

	random := c.rand
	if random == nil {
		random = rand.Reader
	}
	r, s, err := ecdsa.Sign(random, c.private, digest[:])
	if err != nil {
		return "", fmt.Errorf("webpush: sign VAPID token: %w", err)
	}
	// Raw R||S, each padded to the curve size (32 bytes for P-256). See the note above.
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return signingInput + "." + base64URL(sig), nil
}

// verifyJWT checks a token this package produced. It is exported through the test file only in
// spirit: it lives here because the alternative is a test that re-implements ES256 verification
// and then agrees with itself.
func (c *Client) verifyJWT(token string, now time.Time) (vapidClaims, error) {
	var claims vapidClaims
	header, rest, ok := cut(token, ".")
	if !ok {
		return claims, fmt.Errorf("webpush: token has no header")
	}
	if header != jwtHeader {
		return claims, fmt.Errorf("webpush: token header is %q", header)
	}
	body, sig, ok := cut(rest, ".")
	if !ok {
		return claims, fmt.Errorf("webpush: token has no signature")
	}
	rawSig, err := decodeBase64(sig)
	if err != nil {
		return claims, fmt.Errorf("webpush: signature is not base64url: %w", err)
	}
	if len(rawSig) != 64 {
		return claims, fmt.Errorf("webpush: signature is %d bytes, want 64", len(rawSig))
	}
	rawClaims, err := decodeBase64(body)
	if err != nil {
		return claims, fmt.Errorf("webpush: claims are not base64url: %w", err)
	}
	if err := json.Unmarshal(rawClaims, &claims); err != nil {
		return claims, fmt.Errorf("webpush: claims are not JSON: %w", err)
	}

	digest := sha256.Sum256([]byte(header + "." + body))
	r := new(big.Int).SetBytes(rawSig[:32])
	s := new(big.Int).SetBytes(rawSig[32:])
	if !ecdsa.Verify(&c.private.PublicKey, digest[:], r, s) {
		return claims, fmt.Errorf("webpush: signature does not verify")
	}
	if claims.Exp <= now.Unix() {
		return claims, fmt.Errorf("webpush: token expired at %d", claims.Exp)
	}
	return claims, nil
}

// cut splits s at the first sep.
func cut(s, sep string) (before, after string, found bool) {
	before, after, found = strings.Cut(s, sep)
	return before, after, found
}
