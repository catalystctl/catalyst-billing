package i18n

// Locales: the configured languages, which one is the default, and the one a customer chose.
//
// A locale row is configuration, exactly like a currency row: it decides how a page reads and
// nothing about what it says arithmetically. The two invariants that matter are enforced where
// they cannot be bypassed — in the schema — and this file only has to keep them true from the
// application side:
//
//   - **Exactly one default.** The partial unique index makes two impossible; `SetDefaultTx`
//     makes zero impossible by demoting the incumbent before promoting the replacement, in one
//     transaction. A moment with no default would resolve a request to whatever the code
//     happened to fall back to.
//   - **A default is always enabled.** A `CHECK` refuses `is_default AND NOT enabled`. Disabling
//     the default is therefore refused here with a sentence an operator can act on, rather than
//     surfacing as a constraint violation.
//
// A locale is never deleted while it is the default, and deleting one cascades its translations
// (the catalog's foreign key) and clears any customer preference that pointed at it
// (`ON DELETE SET NULL`), so removing a language cannot leave a dangling reference.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/catalyst/billing/internal/store"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// DefaultCode is the locale an installation resolves to before anything is configured.
//
// It matches the `i18n.default_locale` setting's default and the row migration 0037 seeds, so
// the three agree on a fresh install. It is a fallback for an empty table, not a policy: the
// row with `is_default` wins whenever one exists.
const DefaultCode = "en"

// Errors the HTTP layer maps to stable codes.
var (
	// ErrNotFound is returned when the locale does not exist.
	ErrNotFound = errors.New("i18n: no such locale")
	// ErrCodeTaken is returned when the code is already configured.
	ErrCodeTaken = errors.New("i18n: locale already exists")
	// ErrInvalidInput is returned for a request the domain refuses to interpret.
	ErrInvalidInput = errors.New("i18n: invalid input")
	// ErrDefaultLocale is returned when a change would disable or remove the default.
	ErrDefaultLocale = errors.New("i18n: the default locale cannot be disabled or removed")
	// ErrKeyTaken is returned when a translation key already has a row for the locale.
	ErrKeyTaken = errors.New("i18n: that key is already translated")
)

// querier is the subset of pgx that both a pool and a transaction satisfy.
type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// Store is the localization data access.
type Store struct {
	pool *store.Pool
	// settings supplies the `i18n.*` fallbacks for an installation whose locales table is
	// empty (a test database truncated by the harness, a migration run before the seed).
	// Nil means "use DefaultCode".
	settings SettingsReader
}

// SettingsReader is the settings access this package needs.
//
// It is an interface rather than an import of `internal/settings` so the fallback is a
// declared dependency a test can supply, and so a Store constructed with only a pool still
// works — which is what every caller that has a database and no settings store does.
type SettingsReader interface {
	String(ctx context.Context, group, key string) (string, error)
	JSON(ctx context.Context, group, key string, dst any) error
}

// New constructs a Store.
func New(pool *store.Pool) *Store { return &Store{pool: pool} }

// WithSettings attaches the settings fallbacks. It returns the store for chaining.
func (s *Store) WithSettings(reader SettingsReader) *Store {
	s.settings = reader
	return s
}

// Pool exposes the underlying pool for callers that need their own transaction.
func (s *Store) Pool() *store.Pool { return s.pool }

// Locale is one configured language.
type Locale struct {
	// Code is a BCP 47 tag in canonical case: "en", "pt-BR", "zh-Hans".
	Code string `json:"code"`
	// Name is the English name, for an operator's list: "German".
	Name string `json:"name"`
	// NativeName is the name in the language itself, for the customer's switcher: "Deutsch".
	NativeName string `json:"native_name"`
	// Enabled says whether the locale is offered and can resolve.
	Enabled bool `json:"enabled"`
	// IsDefault is the locale a request with no usable preference resolves to.
	IsDefault bool      `json:"is_default"`
	SortOrder int       `json:"sort_order"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// LocaleInput is the wire shape for creating or editing a locale.
//
// The code is only read on create: it is the primary key, it appears in URLs and it is
// referenced by every catalog row and every customer preference, so renaming it is a data
// migration rather than an edit.
type LocaleInput struct {
	Code       string `json:"code"`
	Name       string `json:"name"`
	NativeName string `json:"native_name"`
	Enabled    *bool  `json:"enabled"`
	IsDefault  *bool  `json:"is_default"`
	SortOrder  *int   `json:"sort_order"`
}

const localeColumns = `code, name, native_name, enabled, is_default, sort_order, created_at, updated_at`

func scanLocale(row pgx.Row) (Locale, error) {
	var l Locale
	if err := row.Scan(&l.Code, &l.Name, &l.NativeName, &l.Enabled, &l.IsDefault,
		&l.SortOrder, &l.CreatedAt, &l.UpdatedAt); err != nil {
		return Locale{}, err
	}
	return l, nil
}

// Locales lists every configured locale, the default first then by sort order.
func (s *Store) Locales(ctx context.Context) ([]Locale, error) {
	return s.localesWith(ctx, s.pool)
}

// Enabled lists the locales a switcher may offer, the default first.
func (s *Store) Enabled(ctx context.Context) ([]Locale, error) {
	all, err := s.localesWith(ctx, s.pool)
	if err != nil {
		return nil, err
	}
	out := make([]Locale, 0, len(all))
	for _, l := range all {
		if l.Enabled {
			out = append(out, l)
		}
	}
	return out, nil
}

func (s *Store) localesWith(ctx context.Context, q querier) ([]Locale, error) {
	rows, err := q.Query(ctx,
		`SELECT `+localeColumns+` FROM locales ORDER BY is_default DESC, sort_order, code`)
	if err != nil {
		return nil, fmt.Errorf("i18n: list locales: %w", err)
	}
	defer rows.Close()
	out := make([]Locale, 0, 8)
	for rows.Next() {
		l, err := scanLocale(rows)
		if err != nil {
			return nil, fmt.Errorf("i18n: scan locale: %w", err)
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// Locale reads one locale by code, normalising the case first so `pt-br` finds `pt-BR`.
func (s *Store) Locale(ctx context.Context, code string) (Locale, error) {
	normalized, err := NormalizeCode(code)
	if err != nil {
		return Locale{}, err
	}
	return s.localeWith(ctx, s.pool, normalized)
}

func (s *Store) localeWith(ctx context.Context, q querier, code string) (Locale, error) {
	l, err := scanLocale(q.QueryRow(ctx,
		`SELECT `+localeColumns+` FROM locales WHERE code = $1`, code))
	if errors.Is(err, pgx.ErrNoRows) {
		return Locale{}, ErrNotFound
	}
	if err != nil {
		return Locale{}, fmt.Errorf("i18n: read locale: %w", err)
	}
	return l, nil
}

// Default returns the code of the default locale.
//
// The row is authoritative. When no row is flagged — a table truncated by the test harness, an
// installation whose migration ran but whose seed was removed — the `i18n.default_locale`
// setting answers, and then `DefaultCode`. Resolution must never fail because configuration is
// missing: a panel that cannot decide what language to speak cannot render a page at all.
func (s *Store) Default(ctx context.Context) string {
	var code string
	err := s.pool.QueryRow(ctx, `SELECT code FROM locales WHERE is_default`).Scan(&code)
	if err == nil && code != "" {
		return code
	}
	if s.settings != nil {
		if value, serr := s.settings.String(ctx, "i18n", "default_locale"); serr == nil {
			if normalized, nerr := NormalizeCode(value); nerr == nil {
				return normalized
			}
		}
	}
	return DefaultCode
}

// enabledCodes returns the codes a request may resolve to.
//
// A table with no enabled row falls back to the `i18n.enabled_locales` setting and then to the
// default, for the same reason `Default` does.
func (s *Store) enabledCodes(ctx context.Context, all []Locale) []string {
	out := make([]string, 0, len(all))
	for _, l := range all {
		if l.Enabled {
			out = append(out, l.Code)
		}
	}
	if len(out) > 0 {
		return out
	}
	if s.settings != nil {
		var codes []string
		if err := s.settings.JSON(ctx, "i18n", "enabled_locales", &codes); err == nil {
			cleaned := make([]string, 0, len(codes))
			for _, code := range codes {
				if normalized, err := NormalizeCode(code); err == nil {
					cleaned = append(cleaned, normalized)
				}
			}
			if len(cleaned) > 0 {
				return cleaned
			}
		}
	}
	return []string{s.Default(ctx)}
}

// CreateTx adds a locale inside the caller's transaction.
//
// An empty `enabled` means "offer it": adding a language is a deliberate act, and a locale that
// has to be enabled in a second step is one an operator will think they added and cannot use.
func (s *Store) CreateTx(ctx context.Context, q querier, in LocaleInput) (Locale, error) {
	code, err := NormalizeCode(in.Code)
	if err != nil {
		return Locale{}, err
	}
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return Locale{}, invalid("an English name is required, so an operator can find it in the list")
	}
	native := strings.TrimSpace(in.NativeName)
	if native == "" {
		// A switcher shows the language in its own words. Falling back to the English name
		// keeps a half-filled row usable instead of refusing it.
		native = name
	}
	enabled := true
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	isDefault := in.IsDefault != nil && *in.IsDefault
	sortOrder := 0
	if in.SortOrder != nil {
		sortOrder = *in.SortOrder
	}
	if isDefault && !enabled {
		return Locale{}, invalid("the default locale must be enabled: a request with no preference resolves to it")
	}

	var exists bool
	err = q.QueryRow(ctx, `SELECT true FROM locales WHERE code = $1`, code).Scan(&exists)
	if err == nil {
		return Locale{}, ErrCodeTaken
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Locale{}, fmt.Errorf("i18n: check locale: %w", err)
	}

	if isDefault {
		// Demote the incumbent first: the partial unique index would otherwise reject the
		// insert, and a moment with no default must never be observable.
		if _, err := q.Exec(ctx,
			`UPDATE locales SET is_default = false, updated_at = now() WHERE is_default`); err != nil {
			return Locale{}, fmt.Errorf("i18n: clear default locale: %w", err)
		}
	}
	if _, err := q.Exec(ctx, `
		INSERT INTO locales (code, name, native_name, enabled, is_default, sort_order)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		code, name, native, enabled, isDefault, sortOrder); err != nil {
		if isUniqueViolation(err) {
			return Locale{}, ErrCodeTaken
		}
		if isCheckViolation(err) {
			return Locale{}, invalid("the locale violates a constraint (code shape, or a default that is not enabled)")
		}
		return Locale{}, fmt.Errorf("i18n: create locale: %w", err)
	}
	return s.localeWith(ctx, q, code)
}

// Create adds a locale and commits it.
func (s *Store) Create(ctx context.Context, in LocaleInput) (Locale, error) {
	var out Locale
	err := s.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = s.CreateTx(ctx, tx, in)
		return err
	})
	return out, err
}

// UpdateTx edits a locale's names, whether it is offered, and whether it is the default.
//
// A partial update: a nil field is left as stored. That is what lets the list screen's "Show"
// and "Set default" buttons send one field instead of a whole row they would have to keep in
// sync with the server.
func (s *Store) UpdateTx(ctx context.Context, q querier, code string, in LocaleInput) (Locale, error) {
	normalized, err := NormalizeCode(code)
	if err != nil {
		return Locale{}, err
	}
	current, err := s.localeWith(ctx, q, normalized)
	if err != nil {
		return Locale{}, err
	}
	name, native, enabled, isDefault, sortOrder := current.Name, current.NativeName,
		current.Enabled, current.IsDefault, current.SortOrder
	if trimmed := strings.TrimSpace(in.Name); trimmed != "" {
		name = trimmed
	}
	if trimmed := strings.TrimSpace(in.NativeName); trimmed != "" {
		native = trimmed
	}
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	if in.IsDefault != nil {
		isDefault = *in.IsDefault
	}
	if in.SortOrder != nil {
		sortOrder = *in.SortOrder
	}

	if current.IsDefault && (!enabled || !isDefault) {
		return Locale{}, ErrDefaultLocale
	}
	if isDefault && !enabled {
		return Locale{}, invalid("the default locale must be enabled: a request with no preference resolves to it")
	}
	if isDefault && !current.IsDefault {
		if _, err := q.Exec(ctx,
			`UPDATE locales SET is_default = false, updated_at = now() WHERE is_default`); err != nil {
			return Locale{}, fmt.Errorf("i18n: clear default locale: %w", err)
		}
	}
	if _, err := q.Exec(ctx, `
		UPDATE locales
		SET name = $2, native_name = $3, enabled = $4, is_default = $5, sort_order = $6,
		    updated_at = now()
		WHERE code = $1`,
		normalized, name, native, enabled, isDefault, sortOrder); err != nil {
		if isCheckViolation(err) {
			return Locale{}, invalid("the locale violates a constraint (a default that is not enabled)")
		}
		return Locale{}, fmt.Errorf("i18n: update locale: %w", err)
	}
	return s.localeWith(ctx, q, normalized)
}

// Update edits a locale and commits it.
func (s *Store) Update(ctx context.Context, code string, in LocaleInput) (Locale, error) {
	var out Locale
	err := s.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = s.UpdateTx(ctx, tx, code, in)
		return err
	})
	return out, err
}

// SetDefaultTx makes one locale the default, demoting the incumbent in the same transaction.
func (s *Store) SetDefaultTx(ctx context.Context, q querier, code string) (Locale, error) {
	normalized, err := NormalizeCode(code)
	if err != nil {
		return Locale{}, err
	}
	target, err := s.localeWith(ctx, q, normalized)
	if err != nil {
		return Locale{}, err
	}
	if target.IsDefault {
		return target, nil
	}
	if !target.Enabled {
		return Locale{}, invalid("enable %s before making it the default", normalized)
	}
	if _, err := q.Exec(ctx,
		`UPDATE locales SET is_default = false, updated_at = now() WHERE is_default`); err != nil {
		return Locale{}, fmt.Errorf("i18n: clear default locale: %w", err)
	}
	if _, err := q.Exec(ctx,
		`UPDATE locales SET is_default = true, updated_at = now() WHERE code = $1`, normalized); err != nil {
		return Locale{}, fmt.Errorf("i18n: set default locale: %w", err)
	}
	return s.localeWith(ctx, q, normalized)
}

// SetDefault makes one locale the default and commits it.
func (s *Store) SetDefault(ctx context.Context, code string) (Locale, error) {
	var out Locale
	err := s.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = s.SetDefaultTx(ctx, tx, code)
		return err
	})
	return out, err
}

// DeleteTx removes a locale, its translations and every customer preference pointing at it.
//
// The default cannot be deleted: an installation with no default would resolve every request
// through the code-side fallback, which is a configuration nobody chose. An operator who wants
// rid of the current default makes another locale the default first.
func (s *Store) DeleteTx(ctx context.Context, q querier, code string) error {
	normalized, err := NormalizeCode(code)
	if err != nil {
		return err
	}
	current, err := s.localeWith(ctx, q, normalized)
	if err != nil {
		return err
	}
	if current.IsDefault {
		return ErrDefaultLocale
	}
	tag, err := q.Exec(ctx, `DELETE FROM locales WHERE code = $1`, normalized)
	if err != nil {
		return fmt.Errorf("i18n: delete locale: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// Delete removes a locale and commits it.
func (s *Store) Delete(ctx context.Context, code string) error {
	return s.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return s.DeleteTx(ctx, tx, code)
	})
}

// UserLocale returns the locale a user stored, or "" when they never chose one.
//
// Empty is not "English": it is "this account has no opinion", which is what lets the
// Accept-Language header decide for someone who has not touched the switcher.
func (s *Store) UserLocale(ctx context.Context, userID string) (string, error) {
	if strings.TrimSpace(userID) == "" {
		return "", nil
	}
	var code *string
	err := s.pool.QueryRow(ctx, `SELECT locale FROM users WHERE id = $1`, userID).Scan(&code)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("i18n: read user locale: %w", err)
	}
	if code == nil {
		return "", nil
	}
	return *code, nil
}

// LocaleForEmail returns the locale stored for the account with this address.
//
// It is the mail path's entry point: a queued delivery knows only its recipient address, and
// "which language does this person read" is a fact about their account. An address with no
// account, or an account with no preference, returns "" — the caller then sends the default
// locale's text, which is what a message composed before this feature existed looks like.
func (s *Store) LocaleForEmail(ctx context.Context, email string) (string, error) {
	address := strings.TrimSpace(email)
	if address == "" {
		return "", nil
	}
	var code *string
	err := s.pool.QueryRow(ctx,
		`SELECT locale FROM users WHERE lower(email) = lower($1)`, address).Scan(&code)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("i18n: read recipient locale: %w", err)
	}
	if code == nil {
		return "", nil
	}
	return *code, nil
}

// SetUserLocale stores a user's language preference. An empty code clears it.
//
// A preference for a locale that is not enabled is refused rather than stored: a customer who
// picks a language the installation does not offer would otherwise be stuck reading the default
// while their account claims otherwise.
func (s *Store) SetUserLocale(ctx context.Context, userID, code string) (string, error) {
	if strings.TrimSpace(userID) == "" {
		return "", invalid("a session is required to store a language preference")
	}
	normalized := ""
	if strings.TrimSpace(code) != "" {
		var err error
		normalized, err = NormalizeCode(code)
		if err != nil {
			return "", err
		}
		locale, err := s.Locale(ctx, normalized)
		if err != nil {
			return "", err
		}
		if !locale.Enabled {
			return "", invalid("%s is not offered", normalized)
		}
	}
	var value any
	if normalized != "" {
		value = normalized
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE users SET locale = $2, updated_at = now() WHERE id = $1`, userID, value)
	if err != nil {
		return "", fmt.Errorf("i18n: store user locale: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return "", ErrNotFound
	}
	return normalized, nil
}

// NormalizeCode canonicalises a language tag and refuses a shape the schema would reject.
//
// The canonical form is the one the database stores: the language subtag lower-case, a two-letter
// region upper-case, a four-letter script title-case. Normalising on the way in means `pt-br`,
// `PT-BR` and `pt-BR` are one locale rather than three, which matters because the code appears in
// a URL, in a cookie and in an `Accept-Language` header.
func NormalizeCode(raw string) (string, error) {
	code := strings.TrimSpace(raw)
	if code == "" {
		return "", invalid("a locale code is required")
	}
	if len(code) > 35 {
		return "", invalid("a locale code is at most 35 characters")
	}
	parts := strings.Split(code, "-")
	for i, part := range parts {
		if part == "" {
			return "", invalid("a locale code has no empty subtag")
		}
		if i == 0 {
			if len(part) < 2 || len(part) > 3 || !allLetters(part) {
				return "", invalid("a locale starts with a 2 or 3 letter language, e.g. en, de or fil")
			}
			parts[i] = strings.ToLower(part)
			continue
		}
		if len(part) < 2 || len(part) > 8 || !allAlphanumeric(part) {
			return "", invalid("a locale subtag is 2 to 8 letters or digits")
		}
		switch {
		case len(part) == 2 && allLetters(part):
			// A two-letter alphabetic subtag is a region: pt-BR, en-GB.
			parts[i] = strings.ToUpper(part)
		case len(part) == 4 && allLetters(part):
			// A four-letter alphabetic subtag is a script: zh-Hans, sr-Latn.
			parts[i] = strings.ToUpper(part[:1]) + strings.ToLower(part[1:])
		default:
			parts[i] = strings.ToLower(part)
		}
	}
	return strings.Join(parts, "-"), nil
}

func allLetters(s string) bool {
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') {
			return false
		}
	}
	return true
}

func allAlphanumeric(s string) bool {
	for _, r := range s {
		if !allLetters(string(r)) && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}

// sqlState exposes the SQLSTATE of a Postgres error. Matching the code rather than the message
// keeps the mapping stable across Postgres versions and server locales.
func sqlState(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

func isUniqueViolation(err error) bool { return sqlState(err) == "23505" }
func isCheckViolation(err error) bool  { return sqlState(err) == "23514" }

// invalid wraps a message as ErrInvalidInput so a handler can map it without reading the text.
func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidInput, fmt.Sprintf(format, args...))
}
