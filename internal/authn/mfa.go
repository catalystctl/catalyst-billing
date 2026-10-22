package authn

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// TOTP multi-factor authentication.
//
// The implementation is RFC 6238 over RFC 4226 HOTP: HMAC-SHA1, six digits, a thirty-second
// step, and a one-step window in each direction. That is what every authenticator app
// implements, and matching them matters more than choosing a stronger hash: an algorithm the
// customer's app does not support is a second factor nobody can use.
//
// Three decisions are worth explaining:
//
//   - The secret is encrypted at rest with AES-256-GCM, not stored as text. A database backup
//     that leaks must not let an attacker generate valid codes for every staff account. GCM
//     rather than CBC because it authenticates: a tampered ciphertext must fail loudly, not
//     decrypt to a different secret.
//   - The key comes from the environment with an id, so a rotation can re-wrap secrets without
//     asking every user to re-enrol.
//   - A wrong code does NOT consume the login challenge. Consuming it would let an attacker who
//     knows the password lock the real user out by submitting garbage codes.
//
// Recovery codes are hashed and single use. They are the answer to "I lost my phone", and a
// recovery code that can be reused is a password with extra steps.

// MFA errors.
var (
	// ErrMFANotEnrolled is returned when an operation needs a confirmed credential and there is
	// none.
	ErrMFANotEnrolled = errors.New("authn: no confirmed second factor")
	// ErrInvalidCode is returned for a wrong TOTP or recovery code.
	ErrInvalidCode = errors.New("authn: invalid code")
	// ErrMFAAlreadyEnrolled is returned when enrolling twice.
	ErrMFAAlreadyEnrolled = errors.New("authn: a second factor is already confirmed")
	// ErrChallengeExpired is returned for an unknown, used or expired login challenge.
	ErrChallengeExpired = errors.New("authn: the login challenge is invalid or expired")
	// ErrMFAKeyMissing is returned when no encryption key is configured. It is a startup
	// problem, and failing loudly is better than storing a secret in plaintext.
	ErrMFAKeyMissing = errors.New("authn: BILLING_MFA_KEY is not set")
)

// ChallengeLifetime is how long a login challenge stays valid. It only has to survive the few
// seconds it takes to open an authenticator app; anything longer widens the window in which a
// stolen password plus a stolen challenge is useful.
const ChallengeLifetime = 5 * time.Minute

// RecoveryCodeCount is how many codes are issued at enrolment.
const RecoveryCodeCount = 10

// TOTP parameters. They are constants rather than configuration because changing them breaks
// every enrolled authenticator.
const (
	totpDigits = 6
	totpStep   = 30 * time.Second
	// totpSkew is how many steps in each direction are accepted. One step tolerates a phone
	// clock that is up to thirty seconds off, which is common; more than that starts to accept
	// codes an attacker could replay.
	totpSkew = 1
)

// MFACredential is an enrolment.
type MFACredential struct {
	UserID      string
	Kind        string
	ConfirmedAt *time.Time
}

// keyring holds the encryption key and its id.
type keyring struct {
	id  string
	key []byte
}

// mfaKeyring reads the encryption key from the environment.
//
// The key must be 32 bytes, expressed as 64 hex characters or as raw base32. Anything else is
// refused rather than padded: a short key silently weakened is worse than a startup failure.
func mfaKeyring() (keyring, error) {
	raw := strings.TrimSpace(os.Getenv("BILLING_MFA_KEY"))
	if raw == "" {
		return keyring{}, ErrMFAKeyMissing
	}
	id := strings.TrimSpace(os.Getenv("BILLING_MFA_KEY_ID"))
	if id == "" {
		id = "primary"
	}
	key, err := decodeKey(raw)
	if err != nil {
		return keyring{}, err
	}
	return keyring{id: id, key: key}, nil
}

// decodeKey parses a 32-byte key from hex or base32.
func decodeKey(raw string) ([]byte, error) {
	if len(raw) == 64 {
		key := make([]byte, 32)
		for i := 0; i < 32; i++ {
			var b byte
			if _, err := fmt.Sscanf(raw[i*2:i*2+2], "%02x", &b); err != nil {
				key = nil
				break
			}
			key[i] = b
		}
		if key != nil {
			return key, nil
		}
	}
	upper := strings.ToUpper(strings.ReplaceAll(raw, "-", ""))
	if decoded, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(upper); err == nil && len(decoded) == 32 {
		return decoded, nil
	}
	return nil, fmt.Errorf("authn: BILLING_MFA_KEY must encode exactly 32 bytes (hex or base32)")
}

// encrypt seals a secret with AES-256-GCM.
func (k keyring) encrypt(plaintext string) ([]byte, error) {
	block, err := aes.NewCipher(k.key)
	if err != nil {
		return nil, fmt.Errorf("authn: cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("authn: gcm: %w", err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("authn: nonce: %w", err)
	}
	// The nonce is prepended: GCM needs it to decrypt, and it is not secret.
	return gcm.Seal(nonce, nonce, []byte(plaintext), []byte(k.id)), nil
}

// decrypt opens a sealed secret.
func (k keyring) decrypt(sealed []byte, keyID string) (string, error) {
	block, err := aes.NewCipher(k.key)
	if err != nil {
		return "", fmt.Errorf("authn: cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("authn: gcm: %w", err)
	}
	if len(sealed) < gcm.NonceSize() {
		return "", errors.New("authn: ciphertext is too short")
	}
	nonce, body := sealed[:gcm.NonceSize()], sealed[gcm.NonceSize():]
	plain, err := gcm.Open(nil, nonce, body, []byte(keyID))
	if err != nil {
		// A failure here means the ciphertext was tampered with or the key changed. Both are
		// incidents, not user errors.
		return "", fmt.Errorf("authn: decrypt secret: %w", err)
	}
	return string(plain), nil
}

// EnrolMFA creates an unconfirmed TOTP credential and returns the secret once.
//
// The credential is unconfirmed until a code is presented: enrolling without proving the app
// works is how a user locks themselves out.
func (s *Service) EnrolMFA(ctx context.Context, userID string) (secret, otpauthURL string, err error) {
	keys, err := mfaKeyring()
	if err != nil {
		return "", "", err
	}

	var existingConfirmed bool
	err = s.pool.QueryRow(ctx, `
		SELECT confirmed_at IS NOT NULL FROM mfa_credentials WHERE user_id = $1 AND kind = 'totp'`,
		userID).Scan(&existingConfirmed)
	if err == nil && existingConfirmed {
		return "", "", ErrMFAAlreadyEnrolled
	}
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return "", "", fmt.Errorf("authn: read mfa credential: %w", err)
	}

	secret, err = randomSecret()
	if err != nil {
		return "", "", err
	}
	sealed, err := keys.encrypt(secret)
	if err != nil {
		return "", "", err
	}

	var email string
	if err := s.pool.QueryRow(ctx, `SELECT email FROM users WHERE id = $1`, userID).Scan(&email); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", "", ErrUnknownAccount
		}
		return "", "", fmt.Errorf("authn: load account for enrolment: %w", err)
	}

	// Re-enrolling replaces an unconfirmed credential, so a user who abandoned an enrolment can
	// start again without an operator.
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO mfa_credentials (user_id, kind, secret_enc, key_id)
		VALUES ($1, 'totp', $2, $3)
		ON CONFLICT (user_id, kind) DO UPDATE
			SET secret_enc = EXCLUDED.secret_enc, key_id = EXCLUDED.key_id,
			    confirmed_at = NULL, created_at = now()
		WHERE mfa_credentials.confirmed_at IS NULL`,
		userID, sealed, keys.id); err != nil {
		return "", "", fmt.Errorf("authn: store mfa credential: %w", err)
	}

	return secret, buildOtpauthURL(email, secret), nil
}

// otpauthURL renders the provisioning URI an authenticator app scans.
func buildOtpauthURL(email, secret string) string {
	label := url.PathEscape("Catalyst Billing:" + email)
	params := url.Values{}
	params.Set("secret", secret)
	params.Set("issuer", "Catalyst Billing")
	params.Set("algorithm", "SHA1")
	params.Set("digits", fmt.Sprint(totpDigits))
	params.Set("period", fmt.Sprint(int(totpStep.Seconds())))
	return "otpauth://totp/" + label + "?" + params.Encode()
}

// ConfirmMFA validates a code against the pending secret and returns recovery codes.
//
// The codes are returned once and stored hashed. There is no endpoint that shows them again: a
// recovery code that can be re-read is not a recovery code, it is a second password.
func (s *Service) ConfirmMFA(ctx context.Context, userID, code string) ([]string, error) {
	keys, err := mfaKeyring()
	if err != nil {
		return nil, err
	}

	var (
		sealed    []byte
		keyID     string
		confirmed *time.Time
	)
	err = s.pool.QueryRow(ctx, `
		SELECT secret_enc, key_id, confirmed_at FROM mfa_credentials
		WHERE user_id = $1 AND kind = 'totp'`, userID).Scan(&sealed, &keyID, &confirmed)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrMFANotEnrolled
	}
	if err != nil {
		return nil, fmt.Errorf("authn: read mfa credential: %w", err)
	}
	if confirmed != nil {
		return nil, ErrMFAAlreadyEnrolled
	}

	secret, err := keys.decrypt(sealed, keyID)
	if err != nil {
		return nil, err
	}
	if !verifyTOTP(secret, code, s.cfg.Now()) {
		return nil, ErrInvalidCode
	}

	codes := make([]string, 0, RecoveryCodeCount)
	hashed := make([][]byte, 0, RecoveryCodeCount)
	for i := 0; i < RecoveryCodeCount; i++ {
		c, err := randomCode(10)
		if err != nil {
			return nil, err
		}
		codes = append(codes, c)
		hashed = append(hashed, HashCode(c))
	}

	err = s.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			UPDATE mfa_credentials SET confirmed_at = now()
			WHERE user_id = $1 AND kind = 'totp'`, userID); err != nil {
			return fmt.Errorf("authn: confirm mfa credential: %w", err)
		}
		// A confirmed enrolment answers an operator's reset demand. The three columns are
		// cleared together: the demand is what the screens read, and the timestamp and actor are
		// only meaningful while the demand is outstanding (the audit row keeps the history).
		if _, err := tx.Exec(ctx, `
			UPDATE users
			SET mfa_enrolment_required = false, mfa_reset_at = NULL, mfa_reset_by = NULL,
			    updated_at = now()
			WHERE id = $1 AND mfa_enrolment_required`, userID); err != nil {
			return fmt.Errorf("authn: clear mfa enrolment demand: %w", err)
		}
		// Old codes are discarded: two sets would both work, and the older one is the one an
		// attacker is more likely to have.
		if _, err := tx.Exec(ctx, `DELETE FROM mfa_recovery_codes WHERE user_id = $1`, userID); err != nil {
			return fmt.Errorf("authn: clear recovery codes: %w", err)
		}
		for _, h := range hashed {
			if _, err := tx.Exec(ctx, `
				INSERT INTO mfa_recovery_codes (user_id, code_hash) VALUES ($1, $2)`,
				userID, h); err != nil {
				return fmt.Errorf("authn: store recovery code: %w", err)
			}
		}
		return s.recordEvent(ctx, tx, userID, "mfa_enrolled", "", "")
	})
	if err != nil {
		return nil, err
	}
	return codes, nil
}

// DisableMFA removes the second factor.
//
// It requires the password AND a current code. A stolen session alone must not be able to
// remove the second factor, and a stolen password alone must not either.
func (s *Service) DisableMFA(ctx context.Context, userID, password, code string) error {
	keys, err := mfaKeyring()
	if err != nil {
		return err
	}

	var (
		passwordHash string
		sealed       []byte
		keyID        string
		confirmed    *time.Time
	)
	err = s.pool.QueryRow(ctx, `
		SELECT u.password_hash, m.secret_enc, m.key_id, m.confirmed_at
		FROM users u
		JOIN mfa_credentials m ON m.user_id = u.id AND m.kind = 'totp'
		WHERE u.id = $1`, userID).Scan(&passwordHash, &sealed, &keyID, &confirmed)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrMFANotEnrolled
	}
	if err != nil {
		return fmt.Errorf("authn: load mfa credential: %w", err)
	}
	if confirmed == nil {
		return ErrMFANotEnrolled
	}

	if _, err := s.hasher.Verify(password, passwordHash); err != nil {
		return ErrInvalidCredentials
	}
	secret, err := keys.decrypt(sealed, keyID)
	if err != nil {
		return err
	}
	if !verifyTOTP(secret, code, s.cfg.Now()) {
		return ErrInvalidCode
	}

	return s.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`DELETE FROM mfa_credentials WHERE user_id = $1`, userID); err != nil {
			return fmt.Errorf("authn: delete mfa credential: %w", err)
		}
		if _, err := tx.Exec(ctx,
			`DELETE FROM mfa_recovery_codes WHERE user_id = $1`, userID); err != nil {
			return fmt.Errorf("authn: delete recovery codes: %w", err)
		}
		if _, err := tx.Exec(ctx,
			`DELETE FROM mfa_challenges WHERE user_id = $1`, userID); err != nil {
			return fmt.Errorf("authn: delete challenges: %w", err)
		}
		return s.recordEvent(ctx, tx, userID, "mfa_disabled", "", "")
	})
}

// HasMFA reports whether a user has a confirmed second factor.
func (s *Service) HasMFA(ctx context.Context, userID string) (bool, error) {
	var confirmed bool
	err := s.pool.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM mfa_credentials
		                WHERE user_id = $1 AND kind = 'totp' AND confirmed_at IS NOT NULL)`,
		userID).Scan(&confirmed)
	if err != nil {
		return false, fmt.Errorf("authn: check mfa: %w", err)
	}
	return confirmed, nil
}

// StartMFAChallenge creates a login challenge after a correct password.
//
// The challenge carries no authority beyond "the password was right, now prove the second
// factor". It is single use and expires in minutes.
func (s *Service) StartMFAChallenge(ctx context.Context, userID string) (string, time.Time, error) {
	token, err := newToken(32)
	if err != nil {
		return "", time.Time{}, err
	}
	expires := s.cfg.Now().Add(ChallengeLifetime)

	local, err := s.pool.Begin(ctx)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("authn: begin challenge: %w", err)
	}
	defer local.Rollback(ctx)

	// An older challenge is invalidated: two live challenges double the guessing surface.
	if _, err := local.Exec(ctx,
		`UPDATE mfa_challenges SET used_at = now() WHERE user_id = $1 AND used_at IS NULL`,
		userID); err != nil {
		return "", time.Time{}, fmt.Errorf("authn: invalidate previous challenges: %w", err)
	}
	if _, err := local.Exec(ctx, `
		INSERT INTO mfa_challenges (user_id, token_hash, expires_at) VALUES ($1, $2, $3)`,
		userID, HashToken(token), expires); err != nil {
		return "", time.Time{}, fmt.Errorf("authn: create challenge: %w", err)
	}
	if err := local.Commit(ctx); err != nil {
		return "", time.Time{}, fmt.Errorf("authn: commit challenge: %w", err)
	}
	return token, expires, nil
}

// CompleteMFAChallenge validates a challenge plus a code, and reports the user.
//
// A wrong code does not consume the challenge: consuming it would let an attacker who has the
// password deny the real user access by submitting garbage. The challenge is consumed only when
// the code is right, and the per-attempt throttle in the HTTP layer bounds the guessing.
func (s *Service) CompleteMFAChallenge(ctx context.Context, token, code string) (string, error) {
	keys, err := mfaKeyring()
	if err != nil {
		return "", err
	}

	var (
		challengeID string
		userID      string
	)
	err = s.pool.QueryRow(ctx, `
		SELECT id, user_id FROM mfa_challenges
		WHERE token_hash = $1 AND used_at IS NULL AND expires_at > now()`,
		HashToken(token)).Scan(&challengeID, &userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrChallengeExpired
	}
	if err != nil {
		return "", fmt.Errorf("authn: read challenge: %w", err)
	}

	// A recovery code is accepted in place of a TOTP code: "I lost my phone" must have an
	// answer, and the answer must not be "ask an operator to disable your second factor".
	if ok, err := s.consumeRecoveryCode(ctx, userID, code); err != nil {
		return "", err
	} else if ok {
		return s.finishChallenge(ctx, challengeID, userID)
	}

	var (
		sealed    []byte
		keyID     string
		confirmed *time.Time
	)
	err = s.pool.QueryRow(ctx, `
		SELECT secret_enc, key_id, confirmed_at FROM mfa_credentials
		WHERE user_id = $1 AND kind = 'totp'`, userID).Scan(&sealed, &keyID, &confirmed)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && confirmed == nil) {
		return "", ErrMFANotEnrolled
	}
	if err != nil {
		return "", fmt.Errorf("authn: read mfa credential: %w", err)
	}

	secret, err := keys.decrypt(sealed, keyID)
	if err != nil {
		return "", err
	}
	if !verifyTOTP(secret, code, s.cfg.Now()) {
		return "", ErrInvalidCode
	}
	return s.finishChallenge(ctx, challengeID, userID)
}

// finishChallenge consumes the challenge and reports the user.
func (s *Service) finishChallenge(ctx context.Context, challengeID, userID string) (string, error) {
	err := s.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`UPDATE mfa_challenges SET used_at = now() WHERE id = $1`, challengeID); err != nil {
			return fmt.Errorf("authn: consume challenge: %w", err)
		}
		return s.recordEvent(ctx, tx, userID, "mfa_challenge_completed", "", "")
	})
	if err != nil {
		return "", err
	}
	return userID, nil
}

// consumeRecoveryCode spends a recovery code, if the input is one.
func (s *Service) consumeRecoveryCode(ctx context.Context, userID, code string) (bool, error) {
	if strings.TrimSpace(code) == "" {
		return false, nil
	}
	var spent bool
	err := s.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		// The UPDATE is the check: `used_at IS NULL` means a second use of the same code loses
		// the race rather than succeeding.
		err := tx.QueryRow(ctx, `
			UPDATE mfa_recovery_codes SET used_at = now()
			WHERE user_id = $1 AND code_hash = $2 AND used_at IS NULL
			RETURNING true`, userID, HashCode(code)).Scan(&spent)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("authn: consume recovery code: %w", err)
		}
		return s.recordEvent(ctx, tx, userID, "mfa_recovery_code_used", "", "")
	})
	return spent, err
}

// verifyTOTP checks a code against a secret at a moment in time.
//
// The comparison is constant time. A six-digit code has only a million possibilities, and a
// timing side channel that reveals a matching prefix would cut that down dramatically.
func verifyTOTP(secret, code string, now time.Time) bool {
	candidate := strings.ReplaceAll(strings.TrimSpace(code), " ", "")
	if len(candidate) != totpDigits {
		return false
	}
	for _, c := range candidate {
		if c < '0' || c > '9' {
			return false
		}
	}

	// The secret may be entered with spaces or in lower case; an authenticator app shows it in
	// groups, and a user typing it must not have to know that.
	normalised := strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(secret), " ", ""))
	key, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(normalised)
	if err != nil {
		return false
	}

	counter := now.Unix() / int64(totpStep.Seconds())
	for offset := -int64(totpSkew); offset <= int64(totpSkew); offset++ {
		expected := hotp(key, counter+offset)
		if subtle.ConstantTimeCompare([]byte(expected), []byte(candidate)) == 1 {
			return true
		}
	}
	return false
}

// hotp computes the RFC 4226 value for a counter.
func hotp(key []byte, counter int64) string {
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], uint64(counter))

	mac := hmac.New(sha1.New, key)
	mac.Write(buf[:])
	sum := mac.Sum(nil)

	// Dynamic truncation, RFC 4226 section 5.3.
	offset := sum[len(sum)-1] & 0x0f
	binaryCode := (uint32(sum[offset])&0x7f)<<24 |
		uint32(sum[offset+1])<<16 |
		uint32(sum[offset+2])<<8 |
		uint32(sum[offset+3])
	return fmt.Sprintf("%0*d", totpDigits, binaryCode%1_000_000)
}

// TOTPCodeForTest computes the code for a secret at a time.
//
// It is exported so the HTTP tests can drive a real enrolment end to end without reimplementing
// the algorithm; it is not used by production code.
func TOTPCodeForTest(secret string, at time.Time) (string, error) {
	normalised := strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(secret), " ", ""))
	key, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(normalised)
	if err != nil {
		return "", fmt.Errorf("authn: decode secret: %w", err)
	}
	return hotp(key, at.Unix()/int64(totpStep.Seconds())), nil
}
