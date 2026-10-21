// Package authn implements password authentication and server-side sessions.
//
// The threat model is a self-hosted panel reachable from the internet, so the
// defaults are chosen to be safe rather than convenient:
//
//   - passwords are hashed with Argon2id, and the encoded hash carries its own
//     parameters so they can be raised later and upgraded on next login;
//   - session tokens are random, stored only as a hash, and validated against a
//     per-user credential version, so a password change revokes everything;
//   - login attempts are counted per account and per source address, and the
//     response never reveals whether an account exists;
//   - every failure path takes the same time as a success, so the endpoint cannot
//     be used as an account oracle.
package authn

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"golang.org/x/crypto/argon2"
)

var (
	// ErrInvalidCredentials is returned for a wrong password, an unknown account
	// and a disabled account alike. Distinguishing them turns the login endpoint
	// into an account enumeration oracle.
	ErrInvalidCredentials = errors.New("authn: invalid credentials")
	// ErrAccountDisabled is returned only after a correct password, so it cannot
	// be used to discover which addresses exist.
	ErrAccountDisabled = errors.New("authn: account is disabled")
	// ErrTooManyAttempts is returned when throttling rejects an attempt.
	ErrTooManyAttempts = errors.New("authn: too many attempts")
)

// Argon2Params are the hashing parameters. They are persisted inside each hash,
// so a deployment can raise them and old hashes keep working until the user next
// signs in.
type Argon2Params struct {
	Memory      uint32 // KiB
	Iterations  uint32
	Parallelism uint8
	SaltLength  uint32
	KeyLength   uint32
}

// DefaultArgon2Params targets 64 MiB and 3 passes, which is the OWASP
// recommendation for Argon2id at the time of writing. They are a starting point to
// calibrate against the deployment's hardware, not a constant to trust forever.
func DefaultArgon2Params() Argon2Params {
	return Argon2Params{
		Memory:      64 * 1024,
		Iterations:  3,
		Parallelism: 2,
		SaltLength:  16,
		KeyLength:   32,
	}
}

// Validate checks the parameters are within sane bounds. A too-low setting is a
// security defect, and a too-high one is a denial-of-service vector.
func (p Argon2Params) Validate() error {
	if p.Memory < 19*1024 || p.Memory > 1024*1024 {
		return fmt.Errorf("authn: argon2 memory %d KiB out of range 19456..1048576", p.Memory)
	}
	if p.Iterations < 1 || p.Iterations > 10 {
		return fmt.Errorf("authn: argon2 iterations %d out of range 1..10", p.Iterations)
	}
	if p.Parallelism < 1 || p.Parallelism > 8 {
		return fmt.Errorf("authn: argon2 parallelism %d out of range 1..8", p.Parallelism)
	}
	if p.SaltLength < 16 || p.SaltLength > 64 {
		return fmt.Errorf("authn: argon2 salt length %d out of range 16..64", p.SaltLength)
	}
	if p.KeyLength < 32 || p.KeyLength > 64 {
		return fmt.Errorf("authn: argon2 key length %d out of range 32..64", p.KeyLength)
	}
	return nil
}

// MinPasswordLength and MaxPasswordLength bound the input. The upper bound exists
// because Argon2 cost scales with input length: an unbounded password is a cheap
// way to exhaust the server.
const (
	MinPasswordLength = 12
	MaxPasswordLength = 1024
)

// Hasher hashes and verifies passwords.
type Hasher struct {
	params Argon2Params
}

// NewHasher constructs a Hasher, validating its parameters.
func NewHasher(p Argon2Params) (*Hasher, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return &Hasher{params: p}, nil
}

// Params returns the current parameters.
func (h *Hasher) Params() Argon2Params { return h.params }

// ErrWeakPassword is returned when a password is outside the accepted length bounds.
//
// It is a distinct error from a hashing failure because the two are answered differently: a weak
// password is the caller's mistake (422), a hashing failure is ours (500).
var ErrWeakPassword = errors.New("authn: password is too short or too long")

// Hash produces an encoded Argon2id hash:
//
//	$argon2id$v=19$m=65536,t=3,p=2$<salt>$<key>
//
// The encoding is self-describing, which is what makes parameter upgrades
// possible without a flag day.
func (h *Hasher) Hash(password string) (string, error) {
	if len(password) < MinPasswordLength {
		return "", fmt.Errorf("%w: at least %d characters are required",
			ErrWeakPassword, MinPasswordLength)
	}
	if len(password) > MaxPasswordLength {
		return "", fmt.Errorf("%w: at most %d bytes are allowed",
			ErrWeakPassword, MaxPasswordLength)
	}
	salt := make([]byte, h.params.SaltLength)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("authn: read salt: %w", err)
	}
	key := argon2.IDKey([]byte(password), salt,
		h.params.Iterations, h.params.Memory, h.params.Parallelism, h.params.KeyLength)

	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, h.params.Memory, h.params.Iterations, h.params.Parallelism,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key),
	), nil
}

// Verify checks a password against an encoded hash.
//
// It returns the parameters the hash was created with, so a caller can re-hash
// with stronger parameters after a successful login.
func (h *Hasher) Verify(password, encoded string) (Argon2Params, error) {
	params, salt, want, err := decodeHash(encoded)
	if err != nil {
		return Argon2Params{}, err
	}
	got := argon2.IDKey([]byte(password), salt,
		params.Iterations, params.Memory, params.Parallelism, params.KeyLength)

	// Constant-time comparison: a timing side channel here would allow a byte-wise
	// forgery of the derived key.
	if subtle.ConstantTimeCompare(got, want) != 1 {
		return params, ErrInvalidCredentials
	}
	return params, nil
}

// NeedsRehash reports whether an encoded hash was produced with weaker parameters
// than the current ones.
func (h *Hasher) NeedsRehash(encoded string) bool {
	params, _, _, err := decodeHash(encoded)
	if err != nil {
		return true
	}
	return params.Memory < h.params.Memory ||
		params.Iterations < h.params.Iterations ||
		params.Parallelism < h.params.Parallelism ||
		params.KeyLength < h.params.KeyLength
}

func decodeHash(encoded string) (Argon2Params, []byte, []byte, error) {
	parts := strings.Split(encoded, "$")
	// "", "argon2id", "v=19", "m=..,t=..,p=..", salt, key
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return Argon2Params{}, nil, nil, errors.New("authn: malformed password hash")
	}
	if parts[2] != fmt.Sprintf("v=%d", argon2.Version) {
		return Argon2Params{}, nil, nil, fmt.Errorf("authn: unsupported argon2 version %q", parts[2])
	}

	var params Argon2Params
	for _, field := range strings.Split(parts[3], ",") {
		k, v, ok := strings.Cut(field, "=")
		if !ok {
			return Argon2Params{}, nil, nil, fmt.Errorf("authn: malformed hash parameter %q", field)
		}
		n, err := strconv.ParseUint(v, 10, 32)
		if err != nil {
			return Argon2Params{}, nil, nil, fmt.Errorf("authn: malformed hash parameter %q", field)
		}
		switch k {
		case "m":
			params.Memory = uint32(n)
		case "t":
			params.Iterations = uint32(n)
		case "p":
			if n > 255 {
				return Argon2Params{}, nil, nil, errors.New("authn: parallelism out of range")
			}
			params.Parallelism = uint8(n)
		default:
			return Argon2Params{}, nil, nil, fmt.Errorf("authn: unknown hash parameter %q", k)
		}
	}

	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return Argon2Params{}, nil, nil, errors.New("authn: malformed salt")
	}
	key, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return Argon2Params{}, nil, nil, errors.New("authn: malformed key")
	}
	if len(key) == 0 {
		return Argon2Params{}, nil, nil, errors.New("authn: empty key")
	}
	// Salt and key length come from the hash itself, then the whole set is
	// validated so a hostile hash cannot request absurd work.
	params.SaltLength = uint32(len(salt))
	params.KeyLength = uint32(len(key))
	if err := params.Validate(); err != nil {
		return Argon2Params{}, nil, nil, err
	}
	return params, salt, key, nil
}

// HashToken returns the stored form of a session token.
//
// SHA-256 is correct here and Argon2 is not: the token is 256 bits of server
// randomness, so there is nothing to brute-force, and session validation happens on
// every request where a deliberately slow hash would be a denial-of-service lever.
func HashToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

// newToken returns a URL-safe random token with the given bytes of entropy.
func newToken(nBytes int) (string, error) {
	b := make([]byte, nBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("authn: read random bytes: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
