package i18n

// The message catalog: one row per (locale, key), and the fallback that makes a missing row a
// smaller problem than a blank one.
//
// Three rules, and the third is the one that costs money if it is dropped:
//
//  1. **A key is dotted and lower-case** — `storefront.hero.title`, `mail.password_reset.subject`
//     — so a screen's keys sort together and a prefix can be selected without a second table.
//     The shape is checked in Go as well as in SQL so a bad key is a 422 with a sentence rather
//     than a constraint violation.
//
//  2. **Fallback is resolved on read.** `Catalog.Translate` answers in this order: the requested
//     locale, the default locale, the key itself. It never returns "". A blank label renders as a
//     button with nothing in it, which reads as a deliberate design choice and hides the bug.
//     Because the fallback is computed rather than stored, "untranslated" stays knowable: the
//     coverage count is the number of keys with a row in that locale.
//
//  3. **A value substituted into a message is not a translation.** A mail body contains a reset
//     link and an invoice number; those are data. So a catalog row holds prose with `{{name}}`
//     placeholders, and substitution is a single pass that never re-examines what it wrote — a
//     customer who can type into a field cannot make a message pull in another value.

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	// maxKeyLength bounds a key. It matches the CHECK in 0037_i18n.sql.
	maxKeyLength = 200
	// maxTextLength bounds one translation. A storefront paragraph is prose, not a document.
	maxTextLength = 20 << 10
)

// Entry is one catalog row.
type Entry struct {
	Locale string `json:"locale"`
	Key    string `json:"key"`
	Text   string `json:"text"`
	// UpdatedBy is the operator who last edited the wording, or "" when it was seeded.
	UpdatedBy string    `json:"updated_by"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Key is one translatable key across every locale.
//
// It is the shape the editor's search returns: an operator searching "hero" wants the key, its
// default text, and which languages still lack it — not one row per language.
type Key struct {
	Key string `json:"key"`
	// Default is the default locale's text, or "" when the key has no row there. An empty
	// Default is a real state: a key only a translation mentions, which the editor shows so it
	// can be corrected rather than hidden.
	Default string `json:"default"`
	// Translations maps a locale code to its text.
	Translations map[string]string `json:"translations"`
	// Missing lists the enabled locales with no row for this key, in sort order. It is what
	// makes "still to translate" a list instead of an arithmetic exercise.
	Missing []string `json:"missing"`
}

// Catalog is the translations this package has loaded, plus which locale is the default.
//
// It is a plain value so a caller can copy it, hand it to a renderer and drop it. The maps are
// never written after LoadCatalog returns.
type Catalog struct {
	// Default is the code a missing key falls back to.
	Default string
	// Entries is locale -> key -> text.
	Entries map[string]map[string]string
}

// Translate returns the text for a key in a locale, falling back to the default locale and then
// to the key itself.
//
// It never returns "". That is the whole contract: every caller may render the result directly.
func (c Catalog) Translate(locale, key string) string {
	if text := c.Entries[locale][key]; text != "" {
		return text
	}
	def := c.Default
	if def == "" {
		def = DefaultCode
	}
	if text := c.Entries[def][key]; text != "" {
		return text
	}
	return key
}

// Has reports whether a locale has its own row for a key. It is what the coverage count is built
// from, and what the editor's "still to translate" filter reads.
func (c Catalog) Has(locale, key string) bool {
	return c.Entries[locale][key] != ""
}

// Keys returns every key in the catalog, sorted, across all locales.
func (c Catalog) Keys() []string {
	seen := make(map[string]struct{}, len(c.Entries))
	for _, entries := range c.Entries {
		for key := range entries {
			seen[key] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for key := range seen {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

// Merged flattens the fallback chain for one locale into a single map.
//
// The result is keyed by key only and its values are already resolved, so a template or a JSON
// response can be handed it directly. The default's text is the base and the locale's own rows
// overwrite it, which is exactly "the locale, then the default". A key present in neither is
// absent, and the client's helper falls back to the key — the same rule, one layer out.
func (c Catalog) Merged(locale string) map[string]string {
	def := c.Default
	if def == "" {
		def = DefaultCode
	}
	target := locale
	if _, err := NormalizeCode(locale); err != nil {
		target = def
	}
	flat := make(map[string]string, len(c.Entries[def])+len(c.Entries[target]))
	for key, text := range c.Entries[def] {
		flat[key] = text
	}
	for key, text := range c.Entries[target] {
		flat[key] = text
	}
	return flat
}

// LoadCatalog reads every translation for the given locales, plus the default locale.
//
// The default is always included, even when the caller did not ask for it, because the fallback
// needs it: a page rendering in German with one missing key must be able to answer from English
// without a second round trip.
func (s *Store) LoadCatalog(ctx context.Context, locales ...string) (Catalog, error) {
	def := s.Default(ctx)
	wanted := make(map[string]struct{}, len(locales)+1)
	wanted[def] = struct{}{}
	for _, code := range locales {
		normalized, err := NormalizeCode(code)
		if err != nil {
			continue
		}
		wanted[normalized] = struct{}{}
	}
	codes := make([]string, 0, len(wanted))
	for code := range wanted {
		codes = append(codes, code)
	}
	sort.Strings(codes)

	rows, err := s.pool.Query(ctx,
		`SELECT locale_code, key, text FROM message_catalog WHERE locale_code = ANY($1)`, codes)
	if err != nil {
		return Catalog{}, fmt.Errorf("i18n: load catalog: %w", err)
	}
	defer rows.Close()

	catalog := Catalog{Default: def, Entries: make(map[string]map[string]string, len(codes))}
	for _, code := range codes {
		catalog.Entries[code] = map[string]string{}
	}
	for rows.Next() {
		var locale, key, text string
		if err := rows.Scan(&locale, &key, &text); err != nil {
			return Catalog{}, fmt.Errorf("i18n: scan catalog row: %w", err)
		}
		if catalog.Entries[locale] == nil {
			catalog.Entries[locale] = map[string]string{}
		}
		catalog.Entries[locale][key] = text
	}
	if err := rows.Err(); err != nil {
		return Catalog{}, fmt.Errorf("i18n: read catalog: %w", err)
	}
	return catalog, nil
}

// CatalogFor reads one locale's messages with the default locale merged underneath them.
//
// This is the read a page and a mail use: one query, one flat map, so a render never pays for a
// fallback lookup.
func (s *Store) CatalogFor(ctx context.Context, locale string) (map[string]string, error) {
	catalog, err := s.LoadCatalog(ctx, locale)
	if err != nil {
		return nil, err
	}
	return catalog.Merged(locale), nil
}

// Keys searches the catalog for the editor.
//
// `query` matches the key or any locale's text, case-insensitively, because an operator looks for
// "hero" or for "Servers that come with" and does not know which they typed. `prefix` narrows to a
// namespace ("mail."). The result carries every locale's text for each matching key, so the
// editor renders one row per key with a column per language.
func (s *Store) Keys(ctx context.Context, query, prefix string, limit int) ([]Key, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	def := s.Default(ctx)
	locales, err := s.Enabled(ctx)
	if err != nil {
		return nil, err
	}
	codes := make([]string, 0, len(locales)+1)
	codes = append(codes, def)
	for _, l := range locales {
		if l.Code != def {
			codes = append(codes, l.Code)
		}
	}

	// The filter is in SQL so a search does not read the whole catalog into memory. It matches
	// the key or any locale's text, which is what makes "find the sentence I want to fix"
	// possible in a language the operator does not read.
	needle := "%" + strings.ToLower(strings.TrimSpace(query)) + "%"
	keyPrefix := strings.TrimSpace(prefix) + "%"
	rows, err := s.pool.Query(ctx, `
		SELECT key FROM message_catalog
		WHERE key LIKE $2
		  AND ($1 = '%%'
		       OR lower(key) LIKE $1
		       OR EXISTS (SELECT 1 FROM message_catalog m2
		                  WHERE m2.key = message_catalog.key AND lower(m2.text) LIKE $1))
		GROUP BY key
		ORDER BY key
		LIMIT $3`, needle, keyPrefix, limit)
	if err != nil {
		return nil, fmt.Errorf("i18n: search keys: %w", err)
	}
	keys := make([]string, 0, limit)
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			rows.Close()
			return nil, fmt.Errorf("i18n: scan key: %w", err)
		}
		keys = append(keys, key)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("i18n: read keys: %w", err)
	}
	rows.Close()
	if len(keys) == 0 {
		return []Key{}, nil
	}

	textRows, err := s.pool.Query(ctx,
		`SELECT key, locale_code, text FROM message_catalog WHERE key = ANY($1)`, keys)
	if err != nil {
		return nil, fmt.Errorf("i18n: read translations: %w", err)
	}
	defer textRows.Close()

	byKey := make(map[string]map[string]string, len(keys))
	for textRows.Next() {
		var key, locale, text string
		if err := textRows.Scan(&key, &locale, &text); err != nil {
			return nil, fmt.Errorf("i18n: scan translation: %w", err)
		}
		if byKey[key] == nil {
			byKey[key] = map[string]string{}
		}
		byKey[key][locale] = text
	}
	if err := textRows.Err(); err != nil {
		return nil, fmt.Errorf("i18n: read translations: %w", err)
	}

	out := make([]Key, 0, len(keys))
	for _, key := range keys {
		entry := Key{Key: key, Translations: byKey[key], Missing: []string{}}
		if entry.Translations == nil {
			entry.Translations = map[string]string{}
		}
		entry.Default = entry.Translations[def]
		for _, code := range codes {
			if _, ok := entry.Translations[code]; !ok {
				entry.Missing = append(entry.Missing, code)
			}
		}
		out = append(out, entry)
	}
	return out, nil
}

// PutTx writes or replaces one translation inside the caller's transaction.
//
// An empty text is refused here as well as in the schema: "clear this translation" is a delete,
// and a blank row would be indistinguishable from a translation that renders as nothing.
func (s *Store) PutTx(ctx context.Context, q querier, locale, key, text, actorID string) (Entry, error) {
	normalized, err := NormalizeCode(locale)
	if err != nil {
		return Entry{}, err
	}
	key = strings.TrimSpace(key)
	if err := ValidateKey(key); err != nil {
		return Entry{}, err
	}
	body := strings.TrimSpace(text)
	if body == "" {
		return Entry{}, invalidField("text", FieldRequired,
			"a translation cannot be empty: delete the row to fall back to the default locale")
	}
	if len(body) > maxTextLength {
		return Entry{}, invalidField("text", FieldInvalid,
			"the translation is too long (limit %d bytes)", maxTextLength)
	}
	if _, err := s.localeWith(ctx, q, normalized); err != nil {
		return Entry{}, err
	}
	var actor any
	if strings.TrimSpace(actorID) != "" {
		actor = actorID
	}
	if _, err := q.Exec(ctx, `
		INSERT INTO message_catalog (locale_code, key, text, updated_by, updated_at)
		VALUES ($1, $2, $3, $4, now())
		ON CONFLICT (locale_code, key) DO UPDATE
			SET text = EXCLUDED.text, updated_by = EXCLUDED.updated_by, updated_at = now()`,
		normalized, key, body, actor); err != nil {
		return Entry{}, fmt.Errorf("i18n: write translation: %w", err)
	}
	return s.entry(ctx, q, normalized, key)
}

// Put writes one translation and commits it.
func (s *Store) Put(ctx context.Context, locale, key, text, actorID string) (Entry, error) {
	var out Entry
	err := s.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = s.PutTx(ctx, tx, locale, key, text, actorID)
		return err
	})
	return out, err
}

// DeleteTranslationTx removes one translation, which is how a locale falls back to the default
// for a key.
//
// It is named apart from the locale-level Delete on purpose: this package has two things a
// caller can delete, a locale and a translation, and a method called Delete on a Store that
// owns both is a method whose meaning has to be looked up.
//
// The default locale's own row is protected: deleting it would leave the key with no text
// anywhere, and the fallback would then be the key itself — a visible regression for every
// locale at once.
func (s *Store) DeleteTranslationTx(ctx context.Context, q querier, locale, key string) error {
	normalized, err := NormalizeCode(locale)
	if err != nil {
		return err
	}
	if err := ValidateKey(strings.TrimSpace(key)); err != nil {
		return err
	}
	if normalized == s.Default(ctx) {
		return ErrDefaultLocale
	}
	tag, err := q.Exec(ctx,
		`DELETE FROM message_catalog WHERE locale_code = $1 AND key = $2`, normalized, key)
	if err != nil {
		return fmt.Errorf("i18n: delete translation: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteTranslation removes one translation and commits it.
func (s *Store) DeleteTranslation(ctx context.Context, locale, key string) error {
	return s.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return s.DeleteTranslationTx(ctx, tx, locale, key)
	})
}

// entry reads one row back after a write, so the response is the committed state.
func (s *Store) entry(ctx context.Context, q querier, locale, key string) (Entry, error) {
	var e Entry
	var updatedBy *string
	err := q.QueryRow(ctx, `
		SELECT locale_code, key, text, updated_by::text, updated_at
		FROM message_catalog WHERE locale_code = $1 AND key = $2`, locale, key).
		Scan(&e.Locale, &e.Key, &e.Text, &updatedBy, &e.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Entry{}, ErrNotFound
	}
	if err != nil {
		return Entry{}, fmt.Errorf("i18n: read translation: %w", err)
	}
	if updatedBy != nil {
		e.UpdatedBy = *updatedBy
	}
	return e, nil
}

// Coverage reports how many keys each locale has translated, against the number of keys in the
// default locale.
//
// It is the number the locales screen shows, and the reason the catalog does not copy English text
// into other locales: a copy would report 100% for a language nobody translated.
func (s *Store) Coverage(ctx context.Context) (map[string]int, error) {
	def := s.Default(ctx)
	var total int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM message_catalog WHERE locale_code = $1`, def).Scan(&total); err != nil {
		return nil, fmt.Errorf("i18n: count default keys: %w", err)
	}
	rows, err := s.pool.Query(ctx,
		`SELECT locale_code, count(*) FROM message_catalog GROUP BY locale_code`)
	if err != nil {
		return nil, fmt.Errorf("i18n: count coverage: %w", err)
	}
	defer rows.Close()
	out := make(map[string]int, 8)
	for rows.Next() {
		var code string
		var n int
		if err := rows.Scan(&code, &n); err != nil {
			return nil, fmt.Errorf("i18n: scan coverage: %w", err)
		}
		out[code] = n
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("i18n: read coverage: %w", err)
	}
	// The default's own count is its total, and a locale with no rows is zero rather than
	// absent, so a caller can render "0 of N" without a special case.
	out[def] = total
	return out, nil
}

// ValidateKey checks a key's shape and length.
//
// The rule is the same as the CHECK in 0037_i18n.sql, expressed here so a bad key from the editor
// is a 422 with a sentence rather than a 500 from a constraint violation.
func ValidateKey(key string) error {
	if key == "" {
		return invalidField("key", FieldRequired, "a key is required, e.g. storefront.hero.title")
	}
	if len(key) > maxKeyLength {
		return invalidField("key", FieldInvalid, "a key is at most %d characters", maxKeyLength)
	}
	for i, r := range key {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		case r == '.' || r == '_' || r == ':' || r == '-':
			if i == 0 {
				return invalidField("key", FieldInvalid, "a key cannot start with %q", string(r))
			}
		default:
			return invalidField("key", FieldInvalid,
				"a key is lower-case letters, digits, dot, underscore, colon or dash (found %q)", string(r))
		}
	}
	return nil
}

// Substitute replaces `{{name}}` placeholders in a rendered message in a single pass.
//
// Single pass matters: a value that itself contains `{{link}}` is written literally rather than
// expanded, so a customer who can type into a field cannot make a mail pull in another variable's
// value. An unknown placeholder is left as written, which is visible in a rendered message and
// therefore fixable, rather than silently blank.
func Substitute(text string, values map[string]string) string {
	if len(values) == 0 || !strings.Contains(text, "{{") {
		return text
	}
	var b strings.Builder
	b.Grow(len(text))
	for i := 0; i < len(text); {
		if strings.HasPrefix(text[i:], "{{") {
			if end := strings.Index(text[i+2:], "}}"); end >= 0 {
				name := text[i+2 : i+2+end]
				if value, ok := values[name]; ok {
					b.WriteString(value)
					i += 2 + end + 2
					continue
				}
			}
		}
		b.WriteByte(text[i])
		i++
	}
	return b.String()
}

// CatalogSet returns the default locale's messages and one locale's messages in a single read.
//
// It exists for the mail path, which needs both: the default's text to recognise a queued message
// (the body was rendered in the default locale) and the recipient's text to re-render it. Doing it
// in one query keeps a drain of fifty messages at fifty round trips rather than a hundred.
//
// The second map is not merged: a caller that wants fallback per key must decide what "missing"
// means for it, and the mail renderer deliberately keeps the original line instead of substituting
// the default's text it already has.
func (s *Store) CatalogSet(ctx context.Context, locale string) (def map[string]string, localized map[string]string, err error) {
	catalog, err := s.LoadCatalog(ctx, locale)
	if err != nil {
		return nil, nil, err
	}
	normalized, nerr := NormalizeCode(locale)
	if nerr != nil {
		normalized = catalog.Default
	}
	def = catalog.Entries[catalog.Default]
	localized = catalog.Entries[normalized]
	if def == nil {
		def = map[string]string{}
	}
	if localized == nil {
		localized = map[string]string{}
	}
	return def, localized, nil
}
