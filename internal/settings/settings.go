// Package settings is the typed accessor over the `settings` table.
//
// The table is one row per (group, key) with a text value, and that shape is deliberate:
// a settings table with a typed column per setting is a migration per setting, which is
// how a settings page becomes a schema. The type therefore lives here, in code, next to
// the default that a missing row falls back to.
//
// Four rules shape the package, and each one exists because the alternative is a defect:
//
//  1. **A missing row is a default, not an error.** `Lookup` carries a code-side default
//     per key, so a fresh installation with no rows still renders a working storefront and
//     a new key needs no migration. An unknown key *is* an error, because a typo that
//     silently reads "" is a bug that shows up as a blank company name in production.
//
//  2. **A secret is never returned by a read.** `Value`, `Group` and `All` replace the
//     stored text with `RedactedMarker` and report `IsSet` instead, because a settings page
//     that echoes a gateway or SMTP credential back is a credential leak with a login
//     around it. The typed getters (`String`, `Int`, `Bool`, `JSON`) do return the real
//     value: they are the in-process path a mailer or a pricer uses, not an HTTP response.
//
//  3. **A write is a fact.** Every write records `updated_by` and `updated_at`, and writes
//     an audit row in the same transaction through an injected `audit.Auditor`, so "who
//     changed the tax mode" is answerable and a rolled-back change cannot leave an audit
//     row claiming otherwise.
//
//  4. **A group read is one snapshot.** The whole group is read in one statement and merged
//     with the defaults, so the settings page cannot render half of an operator's save.
package settings

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/catalyst/billing/internal/audit"
	"github.com/catalyst/billing/internal/brand"
	"github.com/catalyst/billing/internal/portal"
	"github.com/catalyst/billing/internal/store"
	"github.com/jackc/pgx/v5"
)

// RedactedMarker replaces a secret's value in every read path. It is the same marker the
// audit log uses, so a UI has one string to recognise.
const RedactedMarker = audit.RedactedMarker

// Kind is how a value is parsed and validated.
type Kind string

const (
	KindString Kind = "string"
	KindInt    Kind = "int"
	KindBool   Kind = "bool"
	// KindJSON is a structured value (a department list, a logo reference). It is stored
	// as text and must be valid JSON.
	KindJSON Kind = "json"
)

// Errors callers branch on.
var (
	// ErrUnknownKey is a (group, key) that is not declared below. It is an error rather
	// than a default because an undeclared key is always a typo.
	ErrUnknownKey = errors.New("settings: unknown key")
	// ErrInvalidValue is a value that does not parse as its declared kind, or a string
	// outside a closed set of choices.
	ErrInvalidValue = errors.New("settings: invalid value")
	// ErrUnknownGroup is a group with no declared keys.
	ErrUnknownGroup = errors.New("settings: unknown group")
)

// maxValueBytes bounds one stored value. A settings value is configuration, not a blob:
// the terms of service are text, not a 10 MB upload, and the CSP means an image is a
// reference rather than an inline data URI.
const maxValueBytes = 64 << 10

// Definition is one setting's code-side contract: its type, its default, and whether it is
// a secret.
type Definition struct {
	Group string
	Key   string
	Kind  Kind
	// Default is the value a missing row reads as. It must parse as Kind.
	Default string
	// Secret marks a value that is redacted from every read path. The column is also
	// persisted, so a row written before the key became secret is still redacted.
	Secret bool
	// Choices, when non-empty, closes the set of accepted strings. An enum in code is
	// cheaper than a lookup table and cannot drift from the parser that reads it.
	Choices []string
	// Check is an extra predicate for a key whose values are open-ended but not arbitrary:
	// a colour, an identifier. It runs after the kind check, so a value that is not even
	// of the right type is refused with the type's own message.
	//
	// It exists because `Choices` cannot express "any hex colour" and a bare string would
	// let a hand-written value through to a renderer that has no way to draw it. The check
	// is declared here, next to the default, so there is one place that says what a key
	// accepts; the SQL migration carries the same rule for writes made outside this code.
	Check func(d Definition, value string) error
}

// Name renders "group.key", which is what an error message and a CLI argument use.
func (d Definition) Name() string { return d.Group + "." + d.Key }

// Validate reports whether value is acceptable for this definition.
func (d Definition) Validate(value string) error {
	if len(value) > maxValueBytes {
		return fmt.Errorf("%w: %s is longer than %d bytes", ErrInvalidValue, d.Name(), maxValueBytes)
	}
	if err := d.validateKind(value); err != nil {
		return err
	}
	if d.Check != nil {
		return d.Check(d, value)
	}
	return nil
}

// validateKind applies the rule implied by Kind (and by Choices, when a closed set exists).
func (d Definition) validateKind(value string) error {
	switch d.Kind {
	case KindInt:
		if _, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64); err != nil {
			return fmt.Errorf("%w: %s must be an integer", ErrInvalidValue, d.Name())
		}
	case KindBool:
		if _, err := strconv.ParseBool(strings.TrimSpace(value)); err != nil {
			return fmt.Errorf("%w: %s must be true or false", ErrInvalidValue, d.Name())
		}
	case KindJSON:
		if !json.Valid([]byte(value)) {
			return fmt.Errorf("%w: %s must be valid JSON", ErrInvalidValue, d.Name())
		}
	case KindString:
		if len(d.Choices) > 0 {
			for _, c := range d.Choices {
				if value == c {
					return nil
				}
			}
			return fmt.Errorf("%w: %s must be one of %s", ErrInvalidValue, d.Name(), strings.Join(d.Choices, ", "))
		}
	default:
		return fmt.Errorf("%w: %s has no kind", ErrInvalidValue, d.Name())
	}
	return nil
}

// definitions is the registry. It is exactly the keys the 0020 migration seeds, plus the
// SMTP keys the backlog names for the mail settings group (NTF-10) — no key is invented
// here. Adding one is a code change and a review, which is the point: a settings page that
// accepts arbitrary keys is a schema nobody designed.
//
// Order matters: it is the order the settings page renders and the order a group read
// returns, so a diff of the response is stable.
var definitions = []Definition{
	// ------------------------------------------------------------------ general --
	{Group: "general", Key: "company_name", Kind: KindString, Default: "Catalyst Hosting"},
	{Group: "general", Key: "system_email_address", Kind: KindString, Default: "no-reply@localhost"},
	{Group: "general", Key: "tos", Kind: KindString, Default: ""},
	{Group: "general", Key: "registration_disabled", Kind: KindBool, Default: "false"},
	{Group: "general", Key: "pagination", Kind: KindInt, Default: "25"},

	// ----------------------------------------------------------------- invoices --
	{Group: "invoices", Key: "bill_to_text", Kind: KindString, Default: ""},
	{Group: "invoices", Key: "notes_client_visible", Kind: KindBool, Default: "true"},
	{Group: "invoices", Key: "invoice_number_prefix", Kind: KindString, Default: "INV-"},
	{Group: "invoices", Key: "invoice_number_padding", Kind: KindInt, Default: "6"},
	{Group: "invoices", Key: "credit_note_prefix", Kind: KindString, Default: "CN-"},
	{Group: "invoices", Key: "credit_note_padding", Kind: KindInt, Default: "6"},
	{Group: "invoices", Key: "due_days", Kind: KindInt, Default: "7"},
	{Group: "invoices", Key: "proforma", Kind: KindBool, Default: "false"},

	// ------------------------------------------------------------------ credits --
	{Group: "credits", Key: "enabled", Kind: KindBool, Default: "true"},
	{Group: "credits", Key: "minimum_deposit_minor", Kind: KindInt, Default: "500"},
	{Group: "credits", Key: "maximum_deposit_minor", Kind: KindInt, Default: "100000"},
	{Group: "credits", Key: "maximum_credit_minor", Kind: KindInt, Default: "1000000"},
	{Group: "credits", Key: "auto_use", Kind: KindBool, Default: "true"},

	// ---------------------------------------------------------------------- tax --
	{Group: "tax", Key: "enabled", Kind: KindBool, Default: "false"},
	// Inclusive means the rate is contained in the price; exclusive means it is added.
	{Group: "tax", Key: "default_mode", Kind: KindString, Default: "exclusive",
		Choices: []string{"exclusive", "inclusive"}},

	// ------------------------------------------------------------------ tickets --
	{Group: "tickets", Key: "disabled", Kind: KindBool, Default: "false"},
	{Group: "tickets", Key: "client_closing_disabled", Kind: KindBool, Default: "false"},
	{Group: "tickets", Key: "departments", Kind: KindJSON, Default: `["general","billing","technical"]`},
	{Group: "tickets", Key: "autoclose_days", Kind: KindInt, Default: "14"},

	// ------------------------------------------------------------------ cronjob --
	{Group: "cronjob", Key: "order_cancel_days", Kind: KindInt, Default: "14"},
	{Group: "cronjob", Key: "terminate_days", Kind: KindInt, Default: "30"},

	// --------------------------------------------------------------------- mail --
	{Group: "mail", Key: "disable", Kind: KindBool, Default: "false"},
	{Group: "mail", Key: "must_verify", Kind: KindBool, Default: "false"},
	// The SMTP keys NTF-10 names. They are not in the 0020 seed, so they read as these
	// defaults until an operator saves one; the password is the only secret in the set.
	{Group: "mail", Key: "smtp_host", Kind: KindString, Default: ""},
	{Group: "mail", Key: "smtp_port", Kind: KindInt, Default: "587"},
	{Group: "mail", Key: "smtp_username", Kind: KindString, Default: ""},
	{Group: "mail", Key: "smtp_password", Kind: KindString, Default: "", Secret: true},
	{Group: "mail", Key: "smtp_encryption", Kind: KindString, Default: "starttls",
		Choices: []string{"none", "starttls", "tls"}},
	{Group: "mail", Key: "smtp_from", Kind: KindString, Default: ""},
	// Inbound piping (TKT-11). A reply to a ticket notification is imported either by an
	// IMAP poller or by the provider posting to the piping endpoint; both need the same
	// address so a reply can be attributed to a ticket, and the webhook needs a shared
	// secret because the endpoint is public.
	{Group: "mail", Key: "piping_address", Kind: KindString, Default: ""},
	{Group: "mail", Key: "piping_secret", Kind: KindString, Default: "", Secret: true},
	{Group: "mail", Key: "imap_host", Kind: KindString, Default: ""},
	{Group: "mail", Key: "imap_port", Kind: KindInt, Default: "993"},
	{Group: "mail", Key: "imap_username", Kind: KindString, Default: ""},
	{Group: "mail", Key: "imap_password", Kind: KindString, Default: "", Secret: true},

	// ------------------------------------------------------------ announcements --
	{Group: "announcements", Key: "enabled", Kind: KindBool, Default: "true"},

	// ----------------------------------------------------------------- checkout --
	// TOS acceptance at checkout. `general.tos` holds the text the customer is shown; these
	// decide whether the checkbox is required and whether the storefront offers a currency
	// switcher at all.
	{Group: "checkout", Key: "tos_required", Kind: KindBool, Default: "false"},
	{Group: "checkout", Key: "currency_switcher", Kind: KindBool, Default: "true"},

	// -------------------------------------------------------------- affiliates --
	// A commission rate is an exact rational string, never a float: 10% is `10%`, and
	// `money.ParseRatio` is what reads it. `recurring_cycles` bounds how many renewal
	// invoices earn a commission (0 = every renewal, which is a deliberate choice, not a
	// missing value).
	{Group: "affiliates", Key: "enabled", Kind: KindBool, Default: "false"},
	{Group: "affiliates", Key: "rate", Kind: KindString, Default: "10%"},
	{Group: "affiliates", Key: "recurring", Kind: KindBool, Default: "false"},
	{Group: "affiliates", Key: "recurring_cycles", Kind: KindInt, Default: "0"},

	// ------------------------------------------------------------------ gateway --
	// One configured gateway at a time. The provider name selects the extension; the two
	// secrets are the only secrets in the group and are redacted on every read path.
	{Group: "gateway", Key: "provider", Kind: KindString, Default: ""},
	{Group: "gateway", Key: "enabled", Kind: KindBool, Default: "false"},
	{Group: "gateway", Key: "publishable_key", Kind: KindString, Default: ""},
	{Group: "gateway", Key: "secret_key", Kind: KindString, Default: "", Secret: true},
	{Group: "gateway", Key: "webhook_secret", Kind: KindString, Default: "", Secret: true},
	// off_session_enabled lets a renewal charge a saved payment method without the customer
	// present. It is off by default because it is the one setting that moves money with no
	// human in the loop.
	{Group: "gateway", Key: "off_session_enabled", Kind: KindBool, Default: "false"},

	// ------------------------------------------------------------------ servers --
	{Group: "servers", Key: "provider", Kind: KindString, Default: ""},
	{Group: "servers", Key: "panel_url", Kind: KindString, Default: ""},
	{Group: "servers", Key: "api_key", Kind: KindString, Default: "", Secret: true},

	// --------------------------------------------------------------------- push --
	// Web push needs a VAPID key pair: the public half is served to the browser, the private
	// half signs the push. Without both, the portal hides the "enable notifications" button
	// rather than offering one that cannot work.
	{Group: "push", Key: "enabled", Kind: KindBool, Default: "false"},
	{Group: "push", Key: "vapid_public_key", Kind: KindString, Default: ""},
	{Group: "push", Key: "vapid_private_key", Kind: KindString, Default: "", Secret: true},
	{Group: "push", Key: "subject", Kind: KindString, Default: "mailto:no-reply@localhost"},

	// ------------------------------------------------------------------ captcha --
	{Group: "captcha", Key: "provider", Kind: KindString, Default: "none",
		Choices: []string{"none", "turnstile", "recaptcha", "hcaptcha"}},
	{Group: "captcha", Key: "site_key", Kind: KindString, Default: ""},
	{Group: "captcha", Key: "secret_key", Kind: KindString, Default: "", Secret: true},

	// --------------------------------------------------------------------- i18n --
	{Group: "i18n", Key: "default_locale", Kind: KindString, Default: "en"},
	{Group: "i18n", Key: "enabled_locales", Kind: KindJSON, Default: `["en"]`},

	// ------------------------------------------------------------------ updates --
	{Group: "updates", Key: "channel", Kind: KindString, Default: "stable",
		Choices: []string{"stable", "beta"}},
	{Group: "updates", Key: "check_url", Kind: KindString, Default: ""},
	{Group: "updates", Key: "telemetry", Kind: KindBool, Default: "false"},

	// ------------------------------------------------------------------ branding --
	//
	// The tenant's brand overlay: a display name, a logo and one accent per colour mode.
	// Every key defaults to the empty string, and empty means "not configured" — the panel
	// keeps its built-in name, mark and accent. That is what makes an installation which
	// never opened the branding screen byte-identical to one built before it existed, and
	// it is why the defaults here are deliberately not the current appearance: a default
	// that repeated the built-in colour would make "unset" indistinguishable from "set to
	// the default", and the fallback could never be tested.
	//
	// Two accents rather than one because the two themes need different luminances for the
	// same hue: a mid-tone blue that reads on white is too dark on near-black, and the pale
	// version of it is invisible on white. A single stored accent would be legible in one
	// theme and washed out in the other.
	{Group: "branding", Key: "name", Kind: KindString, Default: "", Check: checkBrandName},
	{Group: "branding", Key: "logo_id", Kind: KindString, Default: "", Check: checkUUIDOrEmpty},
	{Group: "branding", Key: "accent_light", Kind: KindString, Default: "", Check: checkHexColourOrEmpty},
	{Group: "branding", Key: "accent_dark", Kind: KindString, Default: "", Check: checkHexColourOrEmpty},
}

// MaxBrandName bounds the display name. It is the width of the panel's brand line, not an
// arbitrary number: a name longer than this wraps the sidebar and pushes the navigation off
// the screen, which is a worse outcome than being told the name is too long.
const MaxBrandName = 80

// checkBrandName bounds the display name and refuses control characters.
//
// A name is printed in the panel and on every invoice, so a newline or a NUL in it is not a
// cosmetic problem: the PDF writer replaces control bytes with spaces, and the panel would
// render a name that is half invisible with no explanation.
func checkBrandName(d Definition, value string) error {
	if len([]rune(value)) > MaxBrandName {
		return fmt.Errorf("%w: %s is longer than %d characters", ErrInvalidValue, d.Name(), MaxBrandName)
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("%w: %s contains a control character", ErrInvalidValue, d.Name())
		}
	}
	return nil
}

// checkHexColourOrEmpty accepts a hex colour or the empty string ("not configured").
//
// The empty string is accepted because clearing a brand value is how an operator removes a
// brand, and a rule that refused "" would make the only way back to the built-in look a
// database write.
func checkHexColourOrEmpty(d Definition, value string) error {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	if _, err := brand.ParseHex(value); err != nil {
		return fmt.Errorf("%w: %s must be a hex colour such as #3b5bdb, or empty", ErrInvalidValue, d.Name())
	}
	return nil
}

// checkUUIDOrEmpty accepts a uuid or the empty string.
//
// The logo is a reference to a stored image, so the value is an identifier: a typo would
// otherwise read as "no logo" and the operator would be told their upload vanished.
func checkUUIDOrEmpty(d Definition, value string) error {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return nil
	}
	if !portal.ValidID(trimmed) {
		return fmt.Errorf("%w: %s must be a stored image id, or empty", ErrInvalidValue, d.Name())
	}
	return nil
}

// Register adds settings definitions contributed by another file in this package.
//
// It exists so a group does not have to be declared in the one eight-hundred-line slice above.
// That slice is a merge hotspot: five unrelated features each needing one group means five edits to
// the same region, and an adjacency conflict in a settings table is resolved by hand, which is how a
// setting quietly disappears.
//
// A contributing file calls Register from its own init() and the group appears everywhere the others
// do. Register panics on a duplicate name, at startup, because a settings key that exists twice is a
// key whose value depends on which declaration won.
func Register(ds ...Definition) {
	// Validate the whole batch BEFORE mutating anything.
	//
	// A registration that panics halfway through would leave half a group in the table, and a
	// half-registered group is worse than a refused one: some keys present, others silently
	// falling through to a default. Checking first means a bad batch changes nothing.
	tables()
	pending := make(map[string]struct{}, len(ds))
	for _, d := range ds {
		if _, dup := index[d.Name()]; dup {
			panic("settings: duplicate definition for " + d.Name())
		}
		if _, dup := pending[d.Name()]; dup {
			panic("settings: duplicate definition for " + d.Name() + " in one batch")
		}
		pending[d.Name()] = struct{}{}
	}
	definitions = append(definitions, ds...)
	// The tables are built lazily, on first use, so they see everything registered by every init().
	// Building them in a package-level initialiser would race the registration order.
	rebuild()
}

var (
	tablesOnce sync.Once
	index      map[string]Definition
	groupOrder []string
)

// rebuild discards the derived tables so the next lookup rebuilds them.
func rebuild() {
	tablesOnce = sync.Once{}
	index = nil
	groupOrder = nil
}

func buildTables() {
	index = make(map[string]Definition, len(definitions))
	groupOrder = make([]string, 0, 16)
	seen := make(map[string]struct{}, 16)
	for _, d := range definitions {
		if _, dup := index[d.Name()]; dup {
			panic("settings: duplicate definition for " + d.Name())
		}
		index[d.Name()] = d
		if _, ok := seen[d.Group]; ok {
			continue
		}
		seen[d.Group] = struct{}{}
		groupOrder = append(groupOrder, d.Group)
	}
}

func tables() {
	tablesOnce.Do(buildTables)
}

// Definitions returns every declared setting, in render order.
func Definitions() []Definition {
	out := make([]Definition, len(definitions))
	copy(out, definitions)
	return out
}

// Groups returns the declared group names, in render order.
func Groups() []string {
	tables()
	out := make([]string, len(groupOrder))
	copy(out, groupOrder)
	return out
}

// Lookup returns the definition for a (group, key).
func Lookup(group, key string) (Definition, bool) {
	tables()
	d, ok := index[group+"."+key]
	return d, ok
}

// groupDefinitions returns the definitions of one group, in declaration order.
func groupDefinitions(group string) []Definition {
	var out []Definition
	for _, d := range definitions {
		if d.Group == group {
			out = append(out, d)
		}
	}
	return out
}

// Value is one setting as a read path sees it.
type Value struct {
	Group string `json:"group"`
	Key   string `json:"key"`
	Kind  Kind   `json:"kind"`
	// Value is the stored text, or RedactedMarker when IsSecret. A secret's value is
	// never placed in this field, whether or not a row exists.
	Value string `json:"value"`
	// IsSecret says the field was redacted. A client renders it as a password input.
	IsSecret bool `json:"is_secret"`
	// IsSet says a row exists. It is how a client tells "unchanged default" from
	// "explicitly set", and the only thing a secret reveals.
	IsSet bool `json:"is_set"`
	// UpdatedAt is when the row was last written; nil when the value is the default.
	UpdatedAt *time.Time `json:"updated_at"`
}

// GroupValues is one group's settings, for the settings page.
type GroupValues struct {
	Group string  `json:"group"`
	Keys  []Value `json:"keys"`
}

// Store reads and writes settings.
//
// It holds a pool and nothing else: the clock is the database's (`now()`), so a row's
// updated_at and the audit row's created_at cannot disagree with each other or with a
// worker on another host.
type Store struct {
	pool *store.Pool
}

// New constructs a Store.
func New(pool *store.Pool) *Store { return &Store{pool: pool} }

// querier is the read surface a pool and a transaction share.
type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// storedRow is one persisted row.
type storedRow struct {
	value     string
	secret    bool
	updatedAt time.Time
}

// ---------------------------------------------------------------- typed reads --

// String returns a setting's stored text, or its default when no row exists.
//
// It returns the real value even for a secret: this is the in-process path a mailer reads
// its SMTP password through. It is not, and must not become, an HTTP response.
func (s *Store) String(ctx context.Context, group, key string) (string, error) {
	value, _, err := s.raw(ctx, group, key)
	return value, err
}

// Int returns a setting as an integer.
func (s *Store) Int(ctx context.Context, group, key string) (int64, error) {
	value, err := s.String(ctx, group, key)
	if err != nil {
		return 0, err
	}
	n, perr := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if perr != nil {
		return 0, fmt.Errorf("%w: %s.%s is not an integer", ErrInvalidValue, group, key)
	}
	return n, nil
}

// Bool returns a setting as a boolean.
func (s *Store) Bool(ctx context.Context, group, key string) (bool, error) {
	value, err := s.String(ctx, group, key)
	if err != nil {
		return false, err
	}
	b, perr := strconv.ParseBool(strings.TrimSpace(value))
	if perr != nil {
		return false, fmt.Errorf("%w: %s.%s is not a boolean", ErrInvalidValue, group, key)
	}
	return b, nil
}

// JSON decodes a structured setting into dst.
func (s *Store) JSON(ctx context.Context, group, key string, dst any) error {
	value, err := s.String(ctx, group, key)
	if err != nil {
		return err
	}
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("%w: %s.%s is empty", ErrInvalidValue, group, key)
	}
	if jerr := json.Unmarshal([]byte(value), dst); jerr != nil {
		return fmt.Errorf("%w: %s.%s is not valid JSON", ErrInvalidValue, group, key)
	}
	return nil
}

// raw reads one value and reports whether a row existed.
func (s *Store) raw(ctx context.Context, group, key string) (string, bool, error) {
	def, ok := Lookup(group, key)
	if !ok {
		return "", false, fmt.Errorf("%w: %s.%s", ErrUnknownKey, group, key)
	}
	var value string
	err := s.pool.QueryRow(ctx,
		`SELECT value FROM settings WHERE group_key = $1 AND key = $2`, group, key).Scan(&value)
	if errors.Is(err, pgx.ErrNoRows) {
		return def.Default, false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("settings: read %s: %w", def.Name(), err)
	}
	return value, true, nil
}

// ------------------------------------------------------------------ group reads --

// Group returns every setting of one group, merged with its defaults.
//
// The whole group is read in one statement, so the page cannot render half of a save: a
// second statement per key would let a concurrent update land between them and show a mix
// of old and new values.
func (s *Store) Group(ctx context.Context, group string) ([]Value, error) {
	defs := groupDefinitions(group)
	if len(defs) == 0 {
		return nil, fmt.Errorf("%w: %s", ErrUnknownGroup, group)
	}
	rows, err := s.readGroup(ctx, s.pool, group)
	if err != nil {
		return nil, err
	}
	return mergeValues(defs, rows), nil
}

// All returns every group, in render order.
func (s *Store) All(ctx context.Context) ([]GroupValues, error) {
	rows, err := s.readAll(ctx, s.pool)
	if err != nil {
		return nil, err
	}
	out := make([]GroupValues, 0, len(groupOrder))
	for _, group := range groupOrder {
		out = append(out, GroupValues{Group: group, Keys: mergeValues(groupDefinitions(group), rows[group])})
	}
	return out, nil
}

// readGroup loads one group's rows.
func (s *Store) readGroup(ctx context.Context, q querier, group string) (map[string]storedRow, error) {
	rows, err := q.Query(ctx,
		`SELECT key, value, is_secret, updated_at FROM settings WHERE group_key = $1`, group)
	if err != nil {
		return nil, fmt.Errorf("settings: read group %s: %w", group, err)
	}
	defer rows.Close()

	out := make(map[string]storedRow, 8)
	for rows.Next() {
		var (
			key string
			row storedRow
		)
		if err := rows.Scan(&key, &row.value, &row.secret, &row.updatedAt); err != nil {
			return nil, fmt.Errorf("settings: scan group %s: %w", group, err)
		}
		out[key] = row
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("settings: read group %s: %w", group, err)
	}
	return out, nil
}

// readAll loads every row, grouped.
func (s *Store) readAll(ctx context.Context, q querier) (map[string]map[string]storedRow, error) {
	rows, err := q.Query(ctx,
		`SELECT group_key, key, value, is_secret, updated_at FROM settings`)
	if err != nil {
		return nil, fmt.Errorf("settings: read all: %w", err)
	}
	defer rows.Close()

	out := make(map[string]map[string]storedRow, len(groupOrder))
	for rows.Next() {
		var (
			group string
			key   string
			row   storedRow
		)
		if err := rows.Scan(&group, &key, &row.value, &row.secret, &row.updatedAt); err != nil {
			return nil, fmt.Errorf("settings: scan all: %w", err)
		}
		if out[group] == nil {
			out[group] = make(map[string]storedRow, 8)
		}
		out[group][key] = row
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("settings: read all: %w", err)
	}
	return out, nil
}

// mergeValues combines declared definitions with stored rows, in declaration order.
//
// A definition with no row yields its default and IsSet false. A secret never yields its
// text: the stored value is replaced before it can reach a caller, so the redaction is a
// property of this function rather than a discipline every handler has to remember.
func mergeValues(defs []Definition, rows map[string]storedRow) []Value {
	out := make([]Value, 0, len(defs))
	for _, def := range defs {
		row, stored := rows[def.Key]
		v := Value{
			Group:    def.Group,
			Key:      def.Key,
			Kind:     def.Kind,
			IsSecret: def.Secret || (stored && row.secret),
		}
		if v.IsSecret {
			// Redacted whether or not a row exists: "no password set" is IsSet=false,
			// and the marker alone reveals nothing either way.
			v.Value = RedactedMarker
		}
		if stored {
			v.IsSet = true
			when := row.updatedAt.UTC()
			v.UpdatedAt = &when
			if !v.IsSecret {
				v.Value = row.value
			}
		} else if !v.IsSecret {
			v.Value = def.Default
		}
		out = append(out, v)
	}
	return out
}

// ----------------------------------------------------------------------- writes --

// Set writes one value and returns it as a read path sees it.
func (s *Store) Set(ctx context.Context, group, key, value, actorID string, auditor audit.Auditor) (Value, error) {
	values, err := s.Update(ctx, group, map[string]string{key: value}, actorID, auditor)
	if err != nil {
		return Value{}, err
	}
	for _, v := range values {
		if v.Key == key {
			return v, nil
		}
	}
	return Value{}, fmt.Errorf("%w: %s.%s", ErrUnknownKey, group, key)
}

// Update writes several values of one group atomically, and returns the group as it now
// reads.
//
// Everything happens in one transaction: the upserts, the audit rows, and the read-back.
// That is what makes the response the committed state rather than a guess, and it is what
// stops a half-saved settings form from being visible to the next reader.
//
// A secret sent back as RedactedMarker is skipped rather than stored. A settings page that
// loads a redacted value into a field and saves the form unchanged must not overwrite the
// credential with the literal string "[redacted]" — a bug that would be found by the mail
// server refusing to authenticate weeks later.
func (s *Store) Update(ctx context.Context, group string, values map[string]string, actorID string, auditor audit.Auditor) ([]Value, error) {
	defs, keys, err := prepareUpdate(group, values)
	if err != nil {
		return nil, err
	}

	var out []Value
	err = s.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = writeValues(ctx, s, tx, group, defs, keys, values, actorID, auditor)
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// UpdateTx writes several values of one group inside a transaction the caller owns.
//
// It exists for a write that has to be one fact with something else: uploading a brand logo
// stores the image and points the `branding.logo_id` setting at it, and a failure between
// those two writes would leave a screen showing a logo id that names nothing. The validation
// is the same validation — it runs before the caller's transaction is touched, so a bad
// value cannot abort a transaction that is already doing other work.
func (s *Store) UpdateTx(ctx context.Context, tx pgx.Tx, group string, values map[string]string, actorID string, auditor audit.Auditor) ([]Value, error) {
	defs, keys, err := prepareUpdate(group, values)
	if err != nil {
		return nil, err
	}
	return writeValues(ctx, s, tx, group, defs, keys, values, actorID, auditor)
}

// prepareUpdate validates a group write and returns the definitions and the keys to write,
// in deterministic order. Nothing is written here: a bulk update that fails on its last key
// must not leave the first nine applied.
func prepareUpdate(group string, values map[string]string) (map[string]Definition, []string, error) {
	defs := groupDefinitions(group)
	if len(defs) == 0 {
		return nil, nil, fmt.Errorf("%w: %s", ErrUnknownGroup, group)
	}
	byKey := make(map[string]Definition, len(defs))
	for _, d := range defs {
		byKey[d.Key] = d
	}
	keys := make([]string, 0, len(values))
	for key, value := range values {
		def, ok := byKey[key]
		if !ok {
			return nil, nil, fmt.Errorf("%w: %s.%s", ErrUnknownKey, group, key)
		}
		if def.Secret && value == RedactedMarker {
			continue
		}
		if err := def.Validate(value); err != nil {
			return nil, nil, err
		}
		keys = append(keys, key)
	}
	// Deterministic write order keeps the audit trail readable and makes a concurrent
	// pair of updates deadlock in one predictable direction rather than randomly.
	sort.Strings(keys)
	return byKey, keys, nil
}

// writeValues performs the upserts, the audit rows and the read-back inside tx.
func writeValues(ctx context.Context, s *Store, tx pgx.Tx, group string, byKey map[string]Definition,
	keys []string, values map[string]string, actorID string, auditor audit.Auditor) ([]Value, error) {
	defs := groupDefinitions(group)
	for _, key := range keys {
		def := byKey[key]
		value := values[key]
		if _, err := tx.Exec(ctx, `
			INSERT INTO settings (group_key, key, value, is_secret, updated_by, updated_at)
			VALUES ($1, $2, $3, $4, $5, now())
			ON CONFLICT (group_key, key) DO UPDATE
				SET value = EXCLUDED.value,
				    is_secret = EXCLUDED.is_secret,
				    updated_by = EXCLUDED.updated_by,
				    updated_at = now()`,
			group, key, value, def.Secret, nullableID(actorID)); err != nil {
			return nil, fmt.Errorf("settings: write %s: %w", def.Name(), err)
		}

		if auditor == nil {
			continue
		}
		// The audit row names the key and, for a non-secret, the new value. A secret's
		// new value is never written anywhere it can be read back.
		logged := value
		if def.Secret {
			logged = RedactedMarker
		}
		changes := map[string]any{
			"group":     group,
			"key":       key,
			"value":     logged,
			"is_secret": def.Secret,
		}
		if err := auditor.RecordAction(ctx, tx, "setting.update", "setting", "", changes); err != nil {
			return nil, err
		}
	}

	rows, err := s.readGroup(ctx, tx, group)
	if err != nil {
		return nil, err
	}
	return mergeValues(defs, rows), nil
}

// nullableID renders an empty actor id as SQL NULL, because settings.updated_by is a uuid
// reference and "" is not a uuid.
func nullableID(s string) any {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return s
}
