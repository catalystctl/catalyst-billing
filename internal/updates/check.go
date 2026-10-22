package updates

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// The update check.
//
// The endpoint is `updates.check_url`, an operator setting, and the answer is a small JSON
// document:
//
//	{
//	  "version": "1.5.0",
//	  "download_url": "https://example.test/billing-1.5.0",
//	  "notes": "Fixes the renewal catch-up guard.",
//	  "published_at": "2026-02-01T10:00:00Z"
//	}
//
// Only `version` is required. The document is fetched with a bounded timeout and a bounded
// body because the update server is not us: a slow one must not hold a request open, and a
// compromised one must not be able to make the panel allocate an unbounded buffer. The
// response status is checked before the body is read, and the body is never echoed in an
// error — a proxy in front of the update server may put anything in it.
//
// Nothing fetched here is executed, written to disk or trusted as a version to install. It
// is a string that gets compared.

const (
	// DefaultTimeout bounds the whole request, connection included.
	DefaultTimeout = 5 * time.Second
	// MaxManifestBytes bounds the response body. A version manifest is a few hundred
	// bytes; a megabyte of it is an attack or a misconfiguration.
	MaxManifestBytes = 64 << 10
	// DefaultCacheTTL is how long an answer is believed. One hour: long enough that a page
	// load never causes a fetch, short enough that a release is noticed the same working
	// day. The on-demand check ignores it.
	DefaultCacheTTL = time.Hour
	// FailureCacheTTL is how long a failure is believed. It is shorter than the success
	// TTL because a fixed update server should be noticed quickly, and non-zero because a
	// broken one must not be retried on every request either.
	FailureCacheTTL = 5 * time.Minute
)

// Failure codes. They are stable identifiers the HTTP layer returns as a problem code and
// the UI can branch on, and they are what the cache stores in `failure_code`.
const (
	CodeURLInvalid      = "update_check_url_invalid"
	CodeUnreachable     = "update_check_unreachable"
	CodeTooLarge        = "update_check_too_large"
	CodeInvalidManifest = "update_check_invalid_manifest"
)

// Manifest is what an update server publishes.
type Manifest struct {
	// Version is the published version, e.g. "1.5.0" or "v1.5.0". Required.
	Version string `json:"version"`
	// DownloadURL is where a human can get the release. It is a link, not something this
	// package fetches: the panel does not download binaries.
	DownloadURL string `json:"download_url"`
	// Notes is a short human description of the release.
	Notes string `json:"notes"`
	// PublishedAt is when the release was published, when the server says.
	PublishedAt *time.Time `json:"published_at"`
}

// CheckError is a failed check with a stable code.
type CheckError struct {
	Code string
	// Message is safe to show an operator: it never contains the response body.
	Message string
	err     error
}

func (e *CheckError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return e.Code
}

func (e *CheckError) Unwrap() error { return e.err }

// Fetcher fetches and validates an update manifest.
//
// It holds an HTTP client and a clock, nothing else, so a test can point it at an
// httptest server and control the time without touching the package's behaviour.
type Fetcher struct {
	Client *http.Client
}

// NewFetcher returns a Fetcher with a bounded client.
//
// The client has its own timeout in addition to the request context, because a caller that
// forgets a deadline must still not be able to hang a handler.
func NewFetcher() *Fetcher {
	return &Fetcher{Client: &http.Client{Timeout: DefaultTimeout}}
}

// Fetch retrieves and validates the manifest at rawURL.
func (f *Fetcher) Fetch(ctx context.Context, rawURL string) (Manifest, error) {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || parsed.Host == "" {
		return Manifest{}, &CheckError{Code: CodeURLInvalid, Message: "the update check URL is not a valid URL"}
	}
	// Only http and https. A file:// or ftp:// URL here would make the panel read local
	// files or speak a protocol nobody reviewed, and an operator who wants a local file can
	// serve it over loopback.
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return Manifest{}, &CheckError{
			Code:    CodeURLInvalid,
			Message: "the update check URL must be http or https",
		}
	}

	ctx, cancel := context.WithTimeout(ctx, DefaultTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return Manifest{}, &CheckError{Code: CodeURLInvalid, Message: "the update check URL could not be requested", err: err}
	}
	req.Header.Set("Accept", "application/json")
	// A conditional request is not attempted: the cache is ours, and an ETag dance would
	// add a second failure mode for a document that is a few hundred bytes.

	client := f.Client
	if client == nil {
		client = NewFetcher().Client
	}
	resp, err := client.Do(req)
	if err != nil {
		return Manifest{}, &CheckError{
			Code:    CodeUnreachable,
			Message: "the update server could not be reached",
			err:     err,
		}
	}
	defer func() {
		// Drain a little so the connection can be reused, then close. Not draining at all
		// leaks connections under a polling client.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		return Manifest{}, &CheckError{
			Code:    CodeUnreachable,
			Message: fmt.Sprintf("the update server answered %d", resp.StatusCode),
		}
	}

	// One byte over the limit is enough to know the body is too large, without reading the
	// rest of it.
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxManifestBytes+1))
	if err != nil {
		return Manifest{}, &CheckError{
			Code:    CodeUnreachable,
			Message: "the update server's answer could not be read",
			err:     err,
		}
	}
	if len(body) > MaxManifestBytes {
		return Manifest{}, &CheckError{
			Code:    CodeTooLarge,
			Message: fmt.Sprintf("the update manifest is larger than %d bytes", MaxManifestBytes),
		}
	}

	var manifest Manifest
	dec := json.NewDecoder(strings.NewReader(string(body)))
	// Unknown fields are refused, exactly like the marketplace index. The manifest is a
	// closed contract: a server that starts sending an "install_command" is not something
	// this panel should read half of and act on, and a field nobody declared is a field
	// nobody reviewed.
	dec.DisallowUnknownFields()
	if err := dec.Decode(&manifest); err != nil {
		return Manifest{}, &CheckError{
			Code:    CodeInvalidManifest,
			Message: "the update manifest is not valid JSON",
			err:     err,
		}
	}
	if strings.TrimSpace(manifest.Version) == "" {
		return Manifest{}, &CheckError{
			Code:    CodeInvalidManifest,
			Message: "the update manifest has no version",
		}
	}
	if _, err := ParseVersion(manifest.Version); err != nil {
		// The published version must parse: a server answering "latest" cannot be compared
		// with anything, and guessing would be worse than reporting a broken manifest.
		return Manifest{}, &CheckError{
			Code:    CodeInvalidManifest,
			Message: "the update manifest's version is not a semantic version",
			err:     err,
		}
	}
	if manifest.DownloadURL != "" {
		link, err := url.Parse(manifest.DownloadURL)
		if err != nil || link.Host == "" || (link.Scheme != "http" && link.Scheme != "https") {
			return Manifest{}, &CheckError{
				Code:    CodeInvalidManifest,
				Message: "the update manifest's download_url is not an http(s) URL",
			}
		}
	}

	return manifest, nil
}

// Result is one update check: the running version, the published version, and the answer.
//
// It is also the cached shape. `Cached` and `ExpiresAt` are about the cache, not about the
// update server, and are set by the store rather than by the fetcher.
type Result struct {
	// Configured is false when no check URL is set.
	Configured bool `json:"configured"`
	// SourceURL is the check URL the answer came from. It is reported so an operator can
	// see which server answered.
	SourceURL string `json:"source_url"`
	// Channel is `updates.channel` at the time of the check ("stable" or "beta").
	Channel string `json:"channel"`
	// RunningVersion is the build's own version.
	RunningVersion string `json:"running_version"`
	// LatestVersion is what the server published. Empty when the check failed.
	LatestVersion string `json:"latest_version"`
	// DownloadURL is the link a human follows to get the release.
	DownloadURL string `json:"download_url"`
	// Notes is the release note from the server.
	Notes string `json:"notes"`
	// PublishedAt is when the server says the release was published.
	PublishedAt *time.Time `json:"published_at"`
	// Outcome is the comparison.
	Outcome Outcome `json:"outcome"`
	// CheckedAt is when this answer was obtained (not when it was read from the cache).
	CheckedAt time.Time `json:"checked_at"`
	// Cached says this answer came from the cache.
	Cached bool `json:"cached"`
	// ExpiresAt is when the cached answer stops being believed.
	ExpiresAt *time.Time `json:"expires_at"`
	// FailureCode is set when the check failed. It is empty for a successful check.
	FailureCode string `json:"failure_code"`
	// Detail explains a failure or an unknown outcome in one sentence.
	Detail string `json:"detail"`
}

// Fresh reports whether the cached answer may be used without a new fetch.
func (r Result) Fresh(now time.Time) bool {
	if r.ExpiresAt == nil {
		return false
	}
	return now.Before(*r.ExpiresAt)
}

// BuildResult compares a manifest against the running version.
//
// A running version that does not parse (the binary's default stamp is "dev") yields
// OutcomeUnknown rather than an error: the panel can still show what is published, and it
// must not claim "up to date" when it has no idea what it is running.
func BuildResult(running, channel, sourceURL string, manifest Manifest, now time.Time) Result {
	out := Result{
		Configured:     true,
		SourceURL:      sourceURL,
		Channel:        channel,
		RunningVersion: running,
		LatestVersion:  strings.TrimPrefix(strings.TrimSpace(manifest.Version), "v"),
		DownloadURL:    manifest.DownloadURL,
		Notes:          manifest.Notes,
		PublishedAt:    manifest.PublishedAt,
		CheckedAt:      now,
	}

	runningVersion, runErr := ParseVersion(running)
	latestVersion, latestErr := ParseVersion(manifest.Version)
	switch {
	case latestErr != nil:
		// The fetcher already refuses this, so reaching here means a caller built a
		// Manifest by hand. Reporting it as unknown is still better than a panic.
		out.Outcome = OutcomeUnknown
		out.Detail = "the published version could not be parsed"
	case runErr != nil:
		out.Outcome = OutcomeUnknown
		out.Detail = "this build has no comparable version stamp, so it cannot be checked"
	default:
		switch runningVersion.Compare(latestVersion) {
		case 0:
			out.Outcome = OutcomeUpToDate
		case -1:
			out.Outcome = OutcomeAvailable
		default:
			out.Outcome = OutcomeAhead
		}
	}
	return out
}

// NotConfiguredResult is the result when no check URL is set.
func NotConfiguredResult(running, channel string) Result {
	return Result{
		Configured:     false,
		Channel:        channel,
		RunningVersion: running,
		Outcome:        OutcomeNotConfigured,
		Detail:         "no update check URL is configured",
	}
}

// FailureResult is the result of a failed check.
func FailureResult(running, channel, sourceURL, code, detail string, now time.Time) Result {
	return Result{
		Configured:     true,
		SourceURL:      sourceURL,
		Channel:        channel,
		RunningVersion: running,
		Outcome:        OutcomeUnreachable,
		CheckedAt:      now,
		FailureCode:    code,
		Detail:         detail,
	}
}

// CacheTTL is how long a result is believed: the failure TTL for a failed check, the
// success TTL otherwise.
func (r Result) CacheTTL() time.Duration {
	if r.FailureCode != "" {
		return FailureCacheTTL
	}
	return DefaultCacheTTL
}

// asCheckError returns the stable code and message of an error from Fetch.
func asCheckError(err error) (code, message string) {
	var ce *CheckError
	if errors.As(err, &ce) {
		return ce.Code, ce.Message
	}
	return CodeUnreachable, "the update check failed"
}
