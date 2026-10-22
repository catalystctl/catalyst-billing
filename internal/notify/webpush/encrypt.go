package webpush

// RFC 8291 message encryption, over the `aes128gcm` content coding of RFC 8188.
//
// # The shape of the thing
//
// The push service is a third party. It is not ours, it is not trusted with the message, and it
// must still be able to route the message to one device. That is why web push encrypts to the
// *device* and not to the service: the payload is sealed with a key derived from a P-256 key
// agreement between a throwaway key generated here and the device's public key, mixed with a
// 16-byte auth secret the device and this server share. The push service sees an opaque record
// and a public key it cannot use.
//
// The derivation is three steps, and every one of them is load-bearing:
//
//	ecdh_secret = ECDH(as_private, ua_public)
//	prk_key     = HKDF-Extract(salt = auth_secret, ikm = ecdh_secret)
//	ikm         = HKDF-Expand(prk_key, "WebPush: info" || 0x00 || ua_public || as_public, 32)
//	prk         = HKDF-Extract(salt = salt, ikm = ikm)
//	CEK         = HKDF-Expand(prk, "Content-Encoding: aes128gcm" || 0x00, 16)
//	NONCE       = HKDF-Expand(prk, "Content-Encoding: nonce" || 0x00, 12)
//
// The auth secret is used as the *salt* of the first extract rather than as the IKM, which is
// the step an implementer gets backwards; and the two public keys are concatenated into the
// info string in the order (device, ephemeral), which is the other one. Both mistakes produce a
// message the device cannot decrypt, and neither produces an error anywhere — the push service
// accepts the body and the notification simply never appears. That is precisely why the test in
// webpush_test.go decrypts: a round trip through the receiver's own private key is the only
// thing that distinguishes "correct" from "looks correct".
//
// # One record, and why the padding byte matters
//
// RFC 8188 frames the plaintext as a sequence of records, each ending in a delimiter byte:
// 0x01 for "another record follows", 0x02 for the last one. A push message is one record, so
// the plaintext is `payload || 0x02`. Omitting the delimiter is a body the device rejects as
// truncated, and using 0x01 makes it wait for a record that never arrives.
//
// # The size limit is not this package's invention
//
// Push services cap the *encrypted* body at 4096 bytes. The header is 86 bytes, the GCM tag is
// 16 and the delimiter is 1, so a plaintext larger than 3993 bytes cannot be sent at all. The
// caller is told (ErrTooLarge) rather than being sent a truncated message: a notification with
// half a sentence is worse than a short one, and the notification layer truncates its own text
// before it gets here.

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
)

const (
	// recordSize is the record size advertised in the header. One record holds the whole
	// payload, so this is the maximum, not a chunk size.
	recordSize = 4096
	// MaxPayload is the largest plaintext a push message can carry: the 4096-byte ceiling
	// minus the 86-byte header, the 16-byte GCM tag and the 1-byte delimiter.
	MaxPayload = recordSize - headerLen - 16 - 1
	// headerLen is salt(16) + rs(4) + idlen(1) + keyid(65).
	headerLen = 16 + 4 + 1 + 65
	// authSecretLen is fixed by the protocol: the browser sends 16 bytes.
	authSecretLen = 16
)

// ErrInvalidAuthSecret is returned for an auth secret that is not 16 bytes.
var ErrInvalidAuthSecret = errors.New("webpush: the subscription's auth secret must be 16 bytes")

// ValidateAuthSecret checks a subscription's auth secret without encrypting anything.
//
// It exists because the store validates a subscription at the write path, where there is no
// message to encrypt yet: a subscription whose auth secret is the wrong length is one that can
// never receive a notification, and the time to say so is when the browser registers it.
func ValidateAuthSecret(s string) error {
	raw, err := decodeBase64(s)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidAuthSecret, err)
	}
	if len(raw) != authSecretLen {
		return fmt.Errorf("%w: got %d", ErrInvalidAuthSecret, len(raw))
	}
	return nil
}

// Encrypt seals plaintext to one subscription, returning the complete body to POST.
//
// The result is the RFC 8188 header followed by the single encrypted record: what a push
// service accepts with `Content-Encoding: aes128gcm` and what the device's own decryption
// expects.
func Encrypt(sub Subscription, plaintext []byte) ([]byte, error) {
	return encrypt(rand.Reader, sub, plaintext)
}

// encrypt is Encrypt with the randomness injected, so a test can be deterministic about the
// ephemeral key while still exercising the real derivation.
func encrypt(random io.Reader, sub Subscription, plaintext []byte) ([]byte, error) {
	if len(plaintext) > MaxPayload {
		return nil, fmt.Errorf("%w: %d bytes, the limit is %d",
			ErrTooLarge, len(plaintext), MaxPayload)
	}

	uaPublic, err := ParsePublicKey(sub.P256DH)
	if err != nil {
		return nil, err
	}
	authSecret, err := decodeBase64(sub.Auth)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidAuthSecret, err)
	}
	if len(authSecret) != authSecretLen {
		return nil, fmt.Errorf("%w: got %d", ErrInvalidAuthSecret, len(authSecret))
	}

	ephemeral, err := ecdh.P256().GenerateKey(random)
	if err != nil {
		return nil, fmt.Errorf("webpush: generate the ephemeral key: %w", err)
	}
	shared, err := ephemeral.ECDH(uaPublic)
	if err != nil {
		return nil, fmt.Errorf("webpush: key agreement: %w", err)
	}

	salt := make([]byte, 16)
	if _, err := io.ReadFull(random, salt); err != nil {
		return nil, fmt.Errorf("webpush: read a salt: %w", err)
	}

	cek, nonce, err := deriveKeys(salt, authSecret, shared, uaPublic.Bytes(), ephemeral.PublicKey().Bytes())
	if err != nil {
		return nil, err
	}

	block, err := aes.NewCipher(cek)
	if err != nil {
		return nil, fmt.Errorf("webpush: cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("webpush: gcm: %w", err)
	}

	// Copy before appending: appending to the caller's slice would write into its spare
	// capacity, and the caller's payload is usually a shared buffer.
	record := make([]byte, len(plaintext)+1)
	copy(record, plaintext)
	record[len(plaintext)] = 0x02

	body := make([]byte, 0, headerLen+len(record)+gcm.Overhead())
	body = append(body, salt...)
	// rs is a big-endian uint32.
	body = append(body, byte(recordSize>>24), byte(recordSize>>16), byte(recordSize>>8), byte(recordSize&0xff))
	// idlen, then the key id: the ephemeral public key, which is what the device needs to
	// redo the agreement.
	body = append(body, byte(len(ephemeral.PublicKey().Bytes())))
	body = append(body, ephemeral.PublicKey().Bytes()...)
	body = gcm.Seal(body, nonce, record, nil)
	return body, nil
}

// Decrypt opens a body produced by Encrypt.
//
// It is exported for two reasons, and neither is the delivery path: it is what makes the
// round-trip test possible, so RFC 8291 correctness is established rather than assumed, and it
// is the tool an operator or a support engineer needs to answer "did we encrypt this
// correctly" without a browser. It takes the device's private key, which only the device has,
// so its existence gives nothing away.
func Decrypt(receiver *ecdh.PrivateKey, authSecretB64 string, body []byte) ([]byte, error) {
	if receiver == nil {
		return nil, errors.New("webpush: no receiver key")
	}
	if len(body) < headerLen {
		return nil, fmt.Errorf("webpush: body is %d bytes, shorter than the header", len(body))
	}
	salt := body[:16]
	rs := int(body[16])<<24 | int(body[17])<<16 | int(body[18])<<8 | int(body[19])
	if rs < 18 || rs > 1<<24 {
		return nil, fmt.Errorf("webpush: record size %d is out of range", rs)
	}
	idLen := int(body[20])
	if idLen != 65 || len(body) < headerLen {
		return nil, fmt.Errorf("webpush: key id is %d bytes, want 65", idLen)
	}
	asPublic, err := ecdh.P256().NewPublicKey(body[21:86])
	if err != nil {
		return nil, fmt.Errorf("webpush: ephemeral key in the header is not a P-256 point: %w", err)
	}

	authSecret, err := decodeBase64(authSecretB64)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidAuthSecret, err)
	}
	if len(authSecret) != authSecretLen {
		return nil, fmt.Errorf("%w: got %d", ErrInvalidAuthSecret, len(authSecret))
	}

	shared, err := receiver.ECDH(asPublic)
	if err != nil {
		return nil, fmt.Errorf("webpush: key agreement: %w", err)
	}
	cek, nonce, err := deriveKeys(salt, authSecret, shared, receiver.PublicKey().Bytes(), asPublic.Bytes())
	if err != nil {
		return nil, err
	}

	block, err := aes.NewCipher(cek)
	if err != nil {
		return nil, fmt.Errorf("webpush: cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("webpush: gcm: %w", err)
	}
	record, err := gcm.Open(nil, nonce, body[headerLen:], nil)
	if err != nil {
		// A wrong auth secret, a wrong key and a tampered body are all indistinguishable
		// here, by design: that is what authenticated encryption is for.
		return nil, fmt.Errorf("webpush: the record does not authenticate: %w", err)
	}
	if len(record) == 0 {
		return nil, errors.New("webpush: the record is empty")
	}
	if last := record[len(record)-1]; last != 0x02 {
		return nil, fmt.Errorf("webpush: the record's delimiter is 0x%02x, want 0x02 (last record)", last)
	}
	return record[:len(record)-1], nil
}

// deriveKeys is the RFC 8291 §3.3/§3.4 derivation. Both sides call it, which is what makes the
// round-trip test a real check: the receiver derives from its own private key and the sender
// from its ephemeral one, and the two only agree if every input is in the right place.
func deriveKeys(salt, authSecret, shared, uaPublic, asPublic []byte) (cek, nonce []byte, err error) {
	// HKDF-Extract with the auth secret as the salt.
	prkKey, err := hkdf.Extract(sha256.New, shared, authSecret)
	if err != nil {
		return nil, nil, fmt.Errorf("webpush: hkdf extract: %w", err)
	}
	// "WebPush: info" || 0x00 || ua_public || as_public
	info := make([]byte, 0, 14+len(uaPublic)+len(asPublic))
	info = append(info, []byte("WebPush: info")...)
	info = append(info, 0x00)
	info = append(info, uaPublic...)
	info = append(info, asPublic...)
	ikm, err := hkdf.Expand(sha256.New, prkKey, string(info), 32)
	if err != nil {
		return nil, nil, fmt.Errorf("webpush: hkdf expand: %w", err)
	}
	prk, err := hkdf.Extract(sha256.New, ikm, salt)
	if err != nil {
		return nil, nil, fmt.Errorf("webpush: hkdf extract: %w", err)
	}
	cek, err = hkdf.Expand(sha256.New, prk, "Content-Encoding: aes128gcm\x00", 16)
	if err != nil {
		return nil, nil, fmt.Errorf("webpush: hkdf expand: %w", err)
	}
	nonce, err = hkdf.Expand(sha256.New, prk, "Content-Encoding: nonce\x00", 12)
	if err != nil {
		return nil, nil, fmt.Errorf("webpush: hkdf expand: %w", err)
	}
	return cek, nonce, nil
}
