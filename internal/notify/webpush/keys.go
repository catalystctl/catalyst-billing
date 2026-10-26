package webpush

// VAPID keys: the pair an installation signs its pushes with, and the one the browser
// subscribes with.
//
// # Why the parser accepts three encodings
//
// The canonical form of a VAPID key is the one the browser wants: the uncompressed P-256 point
// in base64url, 87 characters, no padding. That is what the portal serves and what an operator
// pastes into settings. But the tools that generate these keys do not agree on a format —
// `openssl ecparam -genkey` writes PEM, Node's web-push writes the raw scalar, and a key that
// came out of a certificate is PKCS#8 — and a settings field that accepts exactly one of them
// turns a working key into "invalid key" with no explanation. So the parser accepts all of
// them and the *validation* is what stays strict: it must be a P-256 key, the point must be on
// the curve, and the two halves must match.

import (
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"
)

// ErrInvalidKey is returned for a key that cannot be read as a P-256 key.
var ErrInvalidKey = errors.New("webpush: invalid VAPID key")

// ParsePrivateKey reads the private half of a VAPID key pair.
//
// Accepted: base64url (padded or not) of the 32-byte scalar, PEM "EC PRIVATE KEY" (SEC1) and
// PEM "PRIVATE KEY" (PKCS#8). The curve must be P-256: VAPID is defined over it and a P-384
// key would produce a token no push service accepts.
func ParsePrivateKey(s string) (*ecdsa.PrivateKey, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, fmt.Errorf("%w: the private key is empty", ErrInvalidKey)
	}

	if strings.Contains(s, "-----BEGIN") {
		block, _ := pem.Decode([]byte(s))
		if block == nil {
			return nil, fmt.Errorf("%w: the private key looks like PEM but does not decode", ErrInvalidKey)
		}
		switch block.Type {
		case "EC PRIVATE KEY":
			key, err := x509.ParseECPrivateKey(block.Bytes)
			if err != nil {
				return nil, fmt.Errorf("%w: %v", ErrInvalidKey, err)
			}
			return checkPrivateCurve(key)
		case "PRIVATE KEY":
			parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
			if err != nil {
				return nil, fmt.Errorf("%w: %v", ErrInvalidKey, err)
			}
			key, ok := parsed.(*ecdsa.PrivateKey)
			if !ok {
				return nil, fmt.Errorf("%w: the PKCS#8 key is not an ECDSA key", ErrInvalidKey)
			}
			return checkPrivateCurve(key)
		default:
			return nil, fmt.Errorf("%w: unexpected PEM block %q", ErrInvalidKey, block.Type)
		}
	}

	raw, err := decodeBase64(s)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidKey, err)
	}
	if len(raw) != 32 {
		return nil, fmt.Errorf("%w: a raw P-256 private key is 32 bytes, got %d", ErrInvalidKey, len(raw))
	}
	// ParseRawPrivateKey refuses a scalar of zero or one at or above the group order, and it
	// derives the public half: a raw scalar carries no point, and a signing key needs both.
	key, err := ecdsa.ParseRawPrivateKey(elliptic.P256(), raw)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidKey, err)
	}
	return key, nil
}

// checkPrivateCurve refuses a key on any curve other than P-256.
func checkPrivateCurve(key *ecdsa.PrivateKey) (*ecdsa.PrivateKey, error) {
	if key.Curve != elliptic.P256() {
		return nil, fmt.Errorf("%w: the key is on %s, VAPID requires P-256",
			ErrInvalidKey, key.Curve.Params().Name)
	}
	return key, nil
}

// ParsePublicKey reads the public half of a VAPID key pair.
//
// Accepted: base64url of the uncompressed point (the form the browser takes as
// applicationServerKey) and PEM "PUBLIC KEY" (PKIX). A point that is not on the curve is
// refused here rather than producing a subscription nothing can ever be encrypted to.
func ParsePublicKey(s string) (*ecdh.PublicKey, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, fmt.Errorf("%w: the public key is empty", ErrInvalidKey)
	}

	var raw []byte
	if strings.Contains(s, "-----BEGIN") {
		block, _ := pem.Decode([]byte(s))
		if block == nil {
			return nil, fmt.Errorf("%w: the public key looks like PEM but does not decode", ErrInvalidKey)
		}
		parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrInvalidKey, err)
		}
		key, ok := parsed.(*ecdsa.PublicKey)
		if !ok {
			return nil, fmt.Errorf("%w: the PEM key is not an ECDSA key", ErrInvalidKey)
		}
		encoded, err := key.ECDH()
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrInvalidKey, err)
		}
		return encoded, nil
	}

	raw, err := decodeBase64(s)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidKey, err)
	}
	// ecdh.PublicKey rejects an uncompressed point that is not on the curve, which is the
	// check that matters: an off-curve point would be an invalid-curve attack in the making.
	pub, err := ecdh.P256().NewPublicKey(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidKey, err)
	}
	return pub, nil
}

// publicFromPrivate returns the uncompressed public point for a private key.
func publicFromPrivate(key *ecdsa.PrivateKey) ([]byte, error) {
	encoded, err := key.PublicKey.ECDH()
	if err != nil {
		return nil, fmt.Errorf("webpush: derive the public key: %w", err)
	}
	return encoded.Bytes(), nil
}

// GenerateKeyPair makes a fresh VAPID pair, in the encoding settings store.
//
// It exists so an operator can create keys without a second toolchain: the alternative is
// pasting a key from a Node package whose format this parser may or may not accept, which is a
// support ticket rather than a feature.
func GenerateKeyPair() (publicKey, privateKey string, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", "", fmt.Errorf("webpush: generate VAPID key: %w", err)
	}
	pub, err := publicFromPrivate(key)
	if err != nil {
		return "", "", err
	}
	// Bytes() is the raw 32-byte scalar, which is exactly what ParsePrivateKey accepts.
	scalar, err := key.Bytes()
	if err != nil {
		return "", "", fmt.Errorf("webpush: encode VAPID private key: %w", err)
	}
	return base64URL(pub), base64URL(scalar), nil
}

// decodeBase64 accepts the base64url forms browsers and tools emit, and standard base64 as a
// courtesy: a key copied out of an openssl pipeline is often std-encoded.
func decodeBase64(s string) ([]byte, error) {
	for _, enc := range []*base64.Encoding{
		base64.RawURLEncoding, base64.URLEncoding,
		base64.RawStdEncoding, base64.StdEncoding,
	} {
		if raw, err := enc.DecodeString(s); err == nil {
			return raw, nil
		}
	}
	return nil, errors.New("not valid base64")
}

// base64URL is the encoding every web-facing key uses: url-safe, unpadded.
func base64URL(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
