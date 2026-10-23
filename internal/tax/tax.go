// Package tax answers two questions a rate table cannot answer on its own.
//
// # Where a rate comes from
//
// The rate table itself is `internal/commerce`'s (a dated row per jurisdiction, resolved by the
// pricer, never rewritten once it has taken effect). The importer below is the answer to *where
// does the row come from*: it configures one source, fetches it over HTTP, parses it, shows what
// would change, and writes the changes — each with provenance.
//
// # Whether a rate applies at all
//
// `jurisdiction.go` is the other half, and it is the one that matters legally. A rate table says
// "19% for Germany"; it cannot say "this German business supplied a validated VAT ID and the supply
// is cross-border, so no VAT is charged", and it cannot say "this California customer is in a state
// where the installation has no nexus, so no sales tax is collected". Those are rules about facts —
// where the seller is established, what a validation service answered about an identifier, where
// nexus has been declared — so they are evaluated before a rate is applied, and the engine refuses
// with a stable problem code rather than guessing when it cannot decide. A wrong tax rate is a
// legal problem; a refusal is a support ticket.
//
// `pricer.go` is the bridge: a `billing.Pricer` built for one customer's supply, which is what the
// cart and (once wired) the invoice issuer should use instead of a fixed rate.
//
// # The rules, and why each one is not negotiable
//
//   - **A preview and an import are the same code.** `Preview` fetches, parses and diffs without
//     writing; `Apply` fetches, parses and diffs again inside the transaction that writes. They
//     are not two implementations, so the preview cannot be a nicer story than the import. The
//     consequence an operator sees is honest: pressing Import re-reads the source, and the report
//     it returns is what was actually applied, not what was previewed ten minutes ago.
//
//   - **A malformed row is refused on its own.** A real source always contains one jurisdiction
//     whose rate is "n/a", and refusing the whole document over it would make the importer
//     useless. Every row is validated before any write, and every write runs under its own
//     savepoint, so an unexpected database refusal is one rejected row and not a lost import.
//
//   - **A manual rate is never silently overwritten.** A row an operator typed has
//     `origin = 'manual'`. An import may only replace it when the caller passes
//     `OverwriteManual`, and the scheduled refresh never does: a cron job cannot ask, so it
//     cannot be the thing that replaces a human's number. The alternative — "the last writer
//     wins" — means a rate an operator deliberately pinned is undone overnight by a source that
//     is stale.
//
//   - **A rate's origin is a column, not a guess.** `origin`, `source` and `fetched_at` are
//     written on every imported row, so "which document said 19%, and when did we read it" is a
//     query a year later. A source that is later repointed does not rewrite the provenance of
//     rows it did not produce.
//
//   - **A rate is appended, not rewritten.** The rate table's invariant is that a rate which has
//     taken effect is evidence an issued invoice points at. An import that changes a live rate
//     therefore closes the old row at the new rate's start and inserts a new row, which is the
//     same operation the staff screen offers. Only a row that has *not* yet taken effect is ever
//     rewritten in place.
//
// # What this package deliberately does not do
//
// It does not claim to speak any vendor's tax API. There is no documented, stable, public schema
// for a "tax rate list" that this repository could verify offline, and guessing one would produce
// an importer that silently imports zero rows — a failure that looks exactly like success. So the
// supported formats are a **generic** JSON array and a **generic** CSV, both with the same column
// names as the rate table itself (see `Parse`). An operator with a vendor export maps it to those
// seven columns with `jq` or a spreadsheet, and the mapping is visible in the source URL they
// configured rather than hidden inside this package.
//
// It also does not validate that a rate is *correct*. It checks that a rate is well formed —
// a country code, an exact rational, a date — because a wrong-but-well-formed rate is the
// source's business, and pretending otherwise would give an operator false confidence.
package tax

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Errors callers branch on.
var (
	// ErrNoSource is "nothing is configured". It is the normal state of a fresh installation,
	// not a failure: the screen renders it as a prompt to configure one.
	ErrNoSource = errors.New("tax: no import source is configured")
	// ErrSourceDisabled is "configured, but switched off". Fetching anyway would make the switch
	// a lie, and the scheduled refresh would keep calling a source an operator silenced.
	ErrSourceDisabled = errors.New("tax: the import source is disabled")
	// ErrInvalidSource is a source definition the panel will not store (a URL that is not http,
	// an unknown format). It is a client error, not a fetch failure.
	ErrInvalidSource = errors.New("tax: invalid import source")
	// ErrBadDocument is a document that cannot be read at all: invalid JSON, no CSV header, no
	// required column. It is distinct from a rejected *row*, because there is nothing to report
	// row by row.
	ErrBadDocument = errors.New("tax: the source document cannot be read")
	// ErrFetch is a transport or HTTP failure. The detail carries the status or the error.
	ErrFetch = errors.New("tax: the source could not be fetched")
)

// Supported document formats.
const (
	FormatJSON = "json"
	FormatCSV  = "csv"
)

// Formats is the closed set the source configuration accepts.
var Formats = []string{FormatJSON, FormatCSV}

// Provenance of a rate row.
const (
	// OriginManual is a rate an operator typed (or that predates the importer). It is protected.
	OriginManual = "manual"
	// OriginImport is a rate the importer wrote. It may be replaced by a later import.
	OriginImport = "import"
)

// What one row of the source would become. These strings are part of the API: the screen
// branches on them, so a copy edit cannot change behaviour.
const (
	// OutcomeCreate is a jurisdiction with no open rate.
	OutcomeCreate = "create"
	// OutcomeUpdate is a jurisdiction whose open rate differs and may be replaced.
	OutcomeUpdate = "update"
	// OutcomeUnchanged is a jurisdiction whose open rate already says this.
	OutcomeUnchanged = "unchanged"
	// OutcomeProtected is a jurisdiction the import refuses to touch: a manual rate, or one an
	// operator disabled. It is not an error — it is the protection working — but it is reported
	// so an operator can see the source disagrees with them.
	OutcomeProtected = "protected"
	// OutcomeRejected is a malformed row.
	OutcomeRejected = "rejected"
)

// Rejection codes. Stable identifiers, never the detail sentence.
const (
	CodeMissingCountry    = "missing_country"
	CodeInvalidCountry    = "invalid_country"
	CodeRegionOnWildcard  = "region_on_wildcard"
	CodeMissingRate       = "missing_rate"
	CodeInvalidRate       = "invalid_rate"
	CodeInvalidDate       = "invalid_effective_from"
	CodeInvalidInclusive  = "invalid_inclusive"
	CodeInvalidClass      = "invalid_tax_class"
	CodeInvalidRegion     = "invalid_region"
	CodeFieldCount        = "field_count"
	CodeDuplicateInSource = "duplicate_in_source"
	// CodeWriteFailed is a row the database refused. It exists so a constraint this package did
	// not predict is one rejected row with a reason, not a rolled-back import.
	CodeWriteFailed = "write_failed"
)

// Triggers. Only a manual import may be told to overwrite a manual rate.
const (
	TriggerManual    = "manual"
	TriggerScheduled = "scheduled"
)

// Run outcomes.
const (
	OutcomeRunRunning = "running"
	OutcomeRunOK      = "ok"
	OutcomeRunFailed  = "failed"
)

// Source is the configured document.
type Source struct {
	URL    string `json:"url"`
	Format string `json:"format"`
	// Enabled is the operator's switch. A disabled source is never fetched, by a person or by
	// the worker.
	Enabled bool `json:"enabled"`
	// RefreshInterval is how often the scheduled refresh should re-fetch. It is stored as whole
	// hours, which is the resolution a rate actually changes at.
	RefreshInterval time.Duration `json:"-"`
	// RefreshIntervalHours is the wire form of RefreshInterval.
	RefreshIntervalHours int       `json:"refresh_interval_hours"`
	UpdatedAt            time.Time `json:"updated_at"`
}

// SourceInput is the wire shape for configuring a source.
type SourceInput struct {
	URL                  string `json:"url"`
	Format               string `json:"format"`
	Enabled              *bool  `json:"enabled"`
	RefreshIntervalHours *int   `json:"refresh_interval_hours"`
}

// Row is one rate as the source states it, after parsing and validation.
//
// Rate is kept exactly as the document spelled it ("0.19", "19%", "1/5"). Canonicalisation is
// `internal/commerce`'s job — the same function the staff form goes through — so there is one
// implementation of "what a rate is", and comparison here is numeric rather than textual.
type Row struct {
	// Line is the row's position in the document (1-based; the CSV header is line 1, so the
	// first data row is 2). It is what an operator needs to fix the export.
	Line        int    `json:"line"`
	Name        string `json:"name"`
	CountryCode string `json:"country_code"`
	RegionCode  string `json:"region_code"`
	TaxClass    string `json:"tax_class"`
	Rate        string `json:"rate"`
	Inclusive   bool   `json:"inclusive"`
	// EffectiveFrom is when the rate starts. Zero means "when the import runs".
	EffectiveFrom time.Time `json:"effective_from"`
}

// Jurisdiction renders the row's key the way the rate table does: `DE/standard`,
// `US-CA/standard`, `all/standard`.
func (r Row) Jurisdiction() string {
	return jurisdiction(r.CountryCode, r.RegionCode, r.TaxClass)
}

// Rejection is one row the importer refused.
type Rejection struct {
	Line   int    `json:"line"`
	Key    string `json:"key"`
	Code   string `json:"code"`
	Detail string `json:"detail"`
}

// Current is the rate the table holds now for a jurisdiction, as the preview needs it.
type Current struct {
	ID            string     `json:"id"`
	Rate          string     `json:"rate"`
	Inclusive     bool       `json:"inclusive"`
	EffectiveFrom time.Time  `json:"effective_from"`
	Origin        string     `json:"origin"`
	Source        string     `json:"source,omitempty"`
	FetchedAt     *time.Time `json:"fetched_at,omitempty"`
}

// Change is one row's fate.
type Change struct {
	Line         int    `json:"line"`
	Jurisdiction string `json:"jurisdiction"`
	Outcome      string `json:"outcome"`
	// Reason explains a protected or rejected row in one sentence. It is prose; the code is what
	// a client branches on.
	Reason string `json:"reason,omitempty"`
	// Code is set for a rejected row and empty otherwise.
	Code string `json:"code,omitempty"`
	// Current is the open rate this change would replace, when there is one.
	Current *Current `json:"current,omitempty"`
	// Proposed is what the source says. For an update, `effective_from` is the instant the new
	// row would start, which is not always the document's date (see resolveEffectiveFrom).
	Proposed Row `json:"proposed"`
}

// Preview is what an import would do, computed without writing anything.
type Preview struct {
	SourceURL string    `json:"source_url"`
	Format    string    `json:"format"`
	FetchedAt time.Time `json:"fetched_at"`
	// Rows is every row the document stated, including the ones that were refused.
	Rows       int         `json:"rows"`
	Creates    int         `json:"creates"`
	Updates    int         `json:"updates"`
	Unchanged  int         `json:"unchanged"`
	Protected  int         `json:"protected"`
	Rejected   int         `json:"rejected"`
	Changes    []Change    `json:"changes"`
	Rejections []Rejection `json:"rejections"`
}

// Counts is the headline of a run.
type Counts struct {
	Rows      int `json:"rows"`
	Created   int `json:"created"`
	Updated   int `json:"updated"`
	Unchanged int `json:"unchanged"`
	Protected int `json:"protected"`
	Rejected  int `json:"rejected"`
}

// Run is one recorded import.
type Run struct {
	ID         string     `json:"id"`
	SourceURL  string     `json:"source_url"`
	Format     string     `json:"format"`
	Trigger    string     `json:"trigger"`
	ActorID    string     `json:"actor_id,omitempty"`
	FetchedAt  time.Time  `json:"fetched_at"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	Outcome    string     `json:"outcome"`
	Detail     string     `json:"detail,omitempty"`
	Counts     Counts     `json:"counts"`
	// Preview is the report verbatim: what every row became, and why each refusal was refused.
	Preview *Preview `json:"report,omitempty"`
}

// Result is what an import did. The run is the record; the preview is the same report the
// preview endpoint would have shown for the same fetch.
type Result struct {
	Run     Run     `json:"run"`
	Preview Preview `json:"preview"`
}

// Provenance is one rate's origin, for the rate screen.
type Provenance struct {
	RateID    string     `json:"rate_id"`
	Origin    string     `json:"origin"`
	Source    string     `json:"source"`
	FetchedAt *time.Time `json:"fetched_at,omitempty"`
}

// Options configures an apply.
type Options struct {
	// OverwriteManual is the operator's explicit permission to replace a rate a human typed. The
	// scheduled refresh never sets it: a cron job cannot ask, so it cannot be the thing that
	// replaces a human's number.
	OverwriteManual bool
	// Trigger is TriggerManual or TriggerScheduled.
	Trigger string
	// ActorID is the staff user, empty for the worker.
	ActorID string
	// Audit, when set, is called inside the import transaction with the report. It exists so the
	// audit row and the rates commit together: an audit trail that can be missing for a committed
	// import is not a trail.
	Audit func(ctx context.Context, tx pgx.Tx, result Result) error
}

// ---------------------------------------------------------------- helpers --

// jurisdiction renders a rate's key.
func jurisdiction(country, region, class string) string {
	base := country
	if region != "" {
		base = country + "-" + region
	}
	return base + "/" + class
}

// validCountry reports whether a country code is the wildcard or two upper-case letters.
func validCountry(code string) bool {
	if code == WildcardCountry {
		return true
	}
	if len(code) != 2 {
		return false
	}
	for _, r := range code {
		if r < 'A' || r > 'Z' {
			return false
		}
	}
	return true
}

// WildcardCountry is the literal the rate table uses for "everything not otherwise matched". It
// is duplicated from `commerce` rather than imported because this file is about the *document*,
// and the constant is part of the schema, not of the resolution.
const WildcardCountry = "all"

// DefaultTaxClass is what a row with no class means.
const DefaultTaxClass = "standard"

// fetchPayload is one successful fetch: the bytes, and when they arrived.
type fetchPayload struct {
	URL       string
	Format    string
	Body      []byte
	FetchedAt time.Time
}

// querier is the subset of pgx that a pool and a transaction both satisfy, so the same code can
// read the table inside the import transaction and from a plain read path.
type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// describeErr turns a wrapped error into the sentence a run records.
func describeErr(err error) string {
	if err == nil {
		return ""
	}
	var de *documentError
	if errors.As(err, &de) {
		return de.Error()
	}
	return err.Error()
}

// documentError is a bad document with the line that made it bad, when there is one.
type documentError struct {
	Line   int
	Reason string
}

func (e *documentError) Error() string {
	if e.Line > 0 {
		return fmt.Sprintf("%s (line %d)", e.Reason, e.Line)
	}
	return e.Reason
}

func (e *documentError) Unwrap() error { return ErrBadDocument }

func badDocument(line int, message string, args ...any) error {
	return &documentError{Line: line, Reason: fmt.Sprintf(message, args...)}
}
