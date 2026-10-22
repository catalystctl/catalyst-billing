package tax

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// The configured source: one row, read and written whole.
//
// The validation is deliberately strict about the URL and the format, because both failures are
// silent otherwise. A `file://` URL would let the panel read its own filesystem; a misspelled
// format would parse as neither and import nothing while reporting success.

// DefaultRefreshInterval is how often a newly configured source is refreshed before an operator
// says otherwise. A day is the cadence a tax authority publishes at; an hour would be rude to the
// source and a week would let a January change land in February.
const DefaultRefreshInterval = 24 * time.Hour

// MinRefreshInterval and MaxRefreshInterval bound the setting. A refresh every minute is a
// scraper; a refresh every year is a rate table nobody is maintaining.
const (
	MinRefreshInterval = time.Hour
	MaxRefreshInterval = 365 * 24 * time.Hour
)

// Source reads the configured source. It returns ErrNoSource when nothing has been configured,
// which the screen renders as a prompt rather than as a failure.
func (i *Importer) Source(ctx context.Context) (Source, error) {
	return i.sourceWith(ctx, i.pool)
}

func (i *Importer) sourceWith(ctx context.Context, q querier) (Source, error) {
	var (
		src     Source
		hours   int
		enabled bool
	)
	err := q.QueryRow(ctx, `
		SELECT url, format, enabled, refresh_interval_hours, updated_at
		  FROM tax_import_sources
		 WHERE scope = 'installation'`).
		Scan(&src.URL, &src.Format, &enabled, &hours, &src.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Source{}, ErrNoSource
	}
	if err != nil {
		return Source{}, fmt.Errorf("tax: read import source: %w", err)
	}
	src.Enabled = enabled
	src.RefreshIntervalHours = hours
	src.RefreshInterval = time.Duration(hours) * time.Hour
	return src, nil
}

// SaveSource stores the source definition, replacing any previous one.
//
// It is an upsert on the singleton row: an operator who reconfigures the source is correcting the
// installation's configuration, not creating a second one. Rates already imported keep the URL
// they were fetched from, so repointing the source does not rewrite their provenance.
func (i *Importer) SaveSource(ctx context.Context, in SourceInput, actorID string) (Source, error) {
	var src Source
	err := i.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		src, err = i.SaveSourceTx(ctx, tx, in, actorID)
		return err
	})
	return src, err
}

// SaveSourceTx stores the source inside a caller's transaction, so the audit row describing the
// change commits with it.
func (i *Importer) SaveSourceTx(ctx context.Context, q querier, in SourceInput, actorID string) (Source, error) {
	url := strings.TrimSpace(in.URL)
	parsed, err := parseSourceURL(url)
	if err != nil {
		return Source{}, err
	}
	format := strings.ToLower(strings.TrimSpace(in.Format))
	if format == "" {
		format = FormatJSON
	}
	if !isFormat(format) {
		return Source{}, fmt.Errorf("%w: format %q is not one of %s",
			ErrInvalidSource, in.Format, strings.Join(Formats, ", "))
	}
	enabled := true
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	hours := int(DefaultRefreshInterval / time.Hour)
	if in.RefreshIntervalHours != nil {
		hours = *in.RefreshIntervalHours
	}
	if hours < int(MinRefreshInterval/time.Hour) || hours > int(MaxRefreshInterval/time.Hour) {
		return Source{}, fmt.Errorf(
			"%w: refresh_interval_hours must be between %d and %d",
			ErrInvalidSource, int(MinRefreshInterval/time.Hour), int(MaxRefreshInterval/time.Hour))
	}

	var src Source
	if err := q.QueryRow(ctx, `
		INSERT INTO tax_import_sources (scope, url, format, enabled, refresh_interval_hours,
			updated_by, updated_at)
		VALUES ('installation', $1, $2, $3, $4, NULLIF($5, '')::uuid, now())
		ON CONFLICT (scope) DO UPDATE
		SET url = EXCLUDED.url, format = EXCLUDED.format, enabled = EXCLUDED.enabled,
		    refresh_interval_hours = EXCLUDED.refresh_interval_hours,
		    updated_by = EXCLUDED.updated_by, updated_at = now()
		RETURNING url, format, enabled, refresh_interval_hours, updated_at`,
		parsed, format, enabled, hours, actorID).
		Scan(&src.URL, &src.Format, &src.Enabled, &src.RefreshIntervalHours, &src.UpdatedAt); err != nil {
		return Source{}, fmt.Errorf("tax: save import source: %w", err)
	}
	src.RefreshInterval = time.Duration(src.RefreshIntervalHours) * time.Hour
	return src, nil
}

// parseSourceURL validates and normalises a source URL.
//
// Only http and https are accepted. The reason is not purism: the panel fetches this URL with its
// own network identity, so a `file://` source would be a local file read with an operator-facing
// form, and a `data:` source would be configuration that no audit can reproduce.
func parseSourceURL(raw string) (string, error) {
	if raw == "" {
		return "", fmt.Errorf("%w: a url is required", ErrInvalidSource)
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrInvalidSource, err)
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http", "https":
	default:
		return "", fmt.Errorf("%w: the url must be http or https, not %q", ErrInvalidSource, parsed.Scheme)
	}
	if parsed.Host == "" {
		return "", fmt.Errorf("%w: the url has no host", ErrInvalidSource)
	}
	if parsed.User != nil {
		// A credential in a URL is a credential in a log, a run report and an audit row. The
		// source is fetched anonymously on purpose; an authenticated feed needs a real secret
		// store, not a URL field.
		return "", fmt.Errorf("%w: the url must not carry credentials", ErrInvalidSource)
	}
	return parsed.String(), nil
}

func isFormat(format string) bool {
	for _, f := range Formats {
		if f == format {
			return true
		}
	}
	return false
}
