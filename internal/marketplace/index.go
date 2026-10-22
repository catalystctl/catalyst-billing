// Package marketplace lists extensions from a configured index and installs one, but only
// after its digest has been verified.
//
// # What an install means here
//
// The core loads an extension as a separate process from a path in its own environment
// (`BILLING_GATEWAY_EXTENSION`, `BILLING_PROVISIONER_EXTENSION`, see cmd/billing). It has no
// runtime registration: a process cannot start an extension it was not configured to start.
// So an install is: download the artifact with a bounded size, hash it, compare the hash with
// the index *before* anything is written to a place anything would execute, store the exact
// verified bytes, and record the digest. When a staging directory is configured, the verified
// artifact is also written there atomically, and the API reports that path. Nothing is
// executed, and the API says "verified" rather than "running".
//
// # The digest and the trust problem
//
// A digest published by the same index that serves the download only protects against a
// corrupted transfer, not against a compromised index: an attacker who can rewrite the
// index can rewrite the digest. The honest mitigations are:
//
//   - an operator can *pin* the digest out of band (release notes, a signed announcement),
//     and the install then refuses unless the index digest and the downloaded bytes both
//     match the pin;
//   - the verified bytes are stored and handed back, so the artifact that was checked is the
//     artifact that gets deployed;
//   - every install is audited with the digest it verified.
//
// What this package cannot do is make an unpinned index trustworthy, and it does not claim
// to. `InstallResult.Pinned` says which case an install was.
package marketplace

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/catalyst/billing/internal/updates"
)

const (
	// IndexSchemaVersion is the version of the index document this package understands. A
	// newer one is refused rather than half-read.
	IndexSchemaVersion = 1

	// DefaultIndexTimeout bounds the index fetch. The index is small and the panel renders
	// from the cache; a slow index must not make the marketplace screen slow.
	DefaultIndexTimeout = 10 * time.Second
	// MaxIndexBytes bounds the index body. An index of a few hundred extensions is tens of
	// kilobytes; a megabyte of it is an attack or a mistake.
	MaxIndexBytes = 256 << 10
	// IndexCacheTTL is how long a fetched index is believed. Ten minutes: the listing changes
	// when a publisher publishes, which is not a hot path, and fetching on every page load
	// is a denial of service against the index host.
	IndexCacheTTL = 10 * time.Minute
	// IndexFailureTTL is how long a failed fetch is believed. Short enough that a fixed index
	// is noticed, non-zero so a broken one is not hammered.
	IndexFailureTTL = 2 * time.Minute

	// MaxArtifactBytes bounds a downloaded extension. The panel is not a package registry: an
	// extension is a small adapter binary, and 8 MiB is generous for one.
	MaxArtifactBytes = 8 << 20
	// ArtifactTimeout bounds the download, connection included.
	ArtifactTimeout = 30 * time.Second

	// MaxDescriptionBytes bounds the prose an index can put on the screen.
	MaxDescriptionBytes = 2000
	// MaxPublisherBytes bounds the publisher string.
	MaxPublisherBytes = 200
)

// namePattern is what an extension name may be. It is restrictive on purpose: the name is
// used as a filename when an artifact is staged, so a name that can contain a path separator
// or start with a dot is a path traversal waiting to be written.
var namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// sha256Pattern is a lower-case hex SHA-256.
var sha256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Failure codes. They are stable identifiers the HTTP layer returns as a problem code.
const (
	CodeIndexURLInvalid  = "marketplace_index_url_invalid"
	CodeIndexUnreachable = "marketplace_index_unreachable"
	CodeIndexTooLarge    = "marketplace_index_too_large"
	CodeIndexInvalid     = "marketplace_index_invalid"
)

// Extension is one entry in the index.
type Extension struct {
	Name        string `json:"name"`
	Version     string `json:"version"`
	Publisher   string `json:"publisher"`
	Description string `json:"description"`
	// Kind says what the extension plugs into. It decides the activation instruction the
	// API returns; it is not a claim that the core will load it.
	Kind        string `json:"kind"`
	DownloadURL string `json:"download_url"`
	// SHA256 is the artifact's digest, lower-case hex. The install refuses to proceed unless
	// the downloaded bytes hash to exactly this.
	SHA256 string `json:"sha256"`
	// Homepage is an optional link for the operator.
	Homepage string `json:"homepage"`
}

// Kinds an extension may declare.
const (
	KindGateway     = "gateway"
	KindProvisioner = "provisioner"
	KindOther       = "other"
)

// Index is the index document.
type Index struct {
	SchemaVersion int         `json:"schema_version"`
	Extensions    []Extension `json:"extensions"`
}

// IndexError is a failed index fetch with a stable code.
type IndexError struct {
	Code    string
	Message string
	err     error
}

func (e *IndexError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return e.Code
}

func (e *IndexError) Unwrap() error { return e.err }

// Fetcher fetches and validates an index.
type Fetcher struct {
	Client *http.Client
}

// NewFetcher returns a Fetcher with a bounded client.
func NewFetcher() *Fetcher {
	return &Fetcher{Client: &http.Client{Timeout: DefaultIndexTimeout}}
}

// Fetch retrieves and validates the index at rawURL.
func (f *Fetcher) Fetch(ctx context.Context, rawURL string) (Index, error) {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return Index{}, &IndexError{
			Code:    CodeIndexURLInvalid,
			Message: "the marketplace index URL must be an http or https URL",
		}
	}

	ctx, cancel := context.WithTimeout(ctx, DefaultIndexTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return Index{}, &IndexError{Code: CodeIndexURLInvalid, Message: "the marketplace index could not be requested", err: err}
	}
	req.Header.Set("Accept", "application/json")

	client := f.Client
	if client == nil {
		client = NewFetcher().Client
	}
	resp, err := client.Do(req)
	if err != nil {
		return Index{}, &IndexError{
			Code:    CodeIndexUnreachable,
			Message: "the marketplace index could not be reached",
			err:     err,
		}
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		return Index{}, &IndexError{
			Code:    CodeIndexUnreachable,
			Message: fmt.Sprintf("the marketplace index answered %d", resp.StatusCode),
		}
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxIndexBytes+1))
	if err != nil {
		return Index{}, &IndexError{
			Code:    CodeIndexUnreachable,
			Message: "the marketplace index could not be read",
			err:     err,
		}
	}
	if len(body) > MaxIndexBytes {
		return Index{}, &IndexError{
			Code:    CodeIndexTooLarge,
			Message: fmt.Sprintf("the marketplace index is larger than %d bytes", MaxIndexBytes),
		}
	}

	index, err := ParseIndex(body)
	if err != nil {
		return Index{}, err
	}
	return index, nil
}

// ParseIndex validates an index document.
//
// Validation is strict and complete before anything is shown: an index with one bad entry is
// refused as a whole rather than rendered with the bad entry silently dropped, because a
// dropped entry is an extension an operator cannot install and cannot see why.
func ParseIndex(body []byte) (Index, error) {
	var index Index
	dec := json.NewDecoder(strings.NewReader(string(body)))
	// Unknown fields are refused: a newer index that carries a field this build would ignore
	// is a newer index this build does not understand.
	dec.DisallowUnknownFields()
	if err := dec.Decode(&index); err != nil {
		return Index{}, &IndexError{Code: CodeIndexInvalid, Message: "the marketplace index is not valid JSON", err: err}
	}
	if index.SchemaVersion != IndexSchemaVersion {
		return Index{}, &IndexError{
			Code:    CodeIndexInvalid,
			Message: fmt.Sprintf("the marketplace index schema_version is %d; this panel understands %d", index.SchemaVersion, IndexSchemaVersion),
		}
	}
	if index.Extensions == nil {
		return Index{}, &IndexError{Code: CodeIndexInvalid, Message: "the marketplace index has no extensions array"}
	}

	seen := make(map[string]struct{}, len(index.Extensions))
	for i := range index.Extensions {
		ext := &index.Extensions[i]
		if err := validateExtension(ext); err != nil {
			return Index{}, err
		}
		key := ext.Name + "@" + ext.Version
		if _, dup := seen[key]; dup {
			return Index{}, &IndexError{
				Code:    CodeIndexInvalid,
				Message: fmt.Sprintf("the marketplace index lists %s twice", key),
			}
		}
		seen[key] = struct{}{}
	}

	// A stable order, so the screen and the API response do not reshuffle between fetches.
	sort.SliceStable(index.Extensions, func(i, j int) bool {
		if index.Extensions[i].Name != index.Extensions[j].Name {
			return index.Extensions[i].Name < index.Extensions[j].Name
		}
		return index.Extensions[i].Version < index.Extensions[j].Version
	})
	return index, nil
}

// validateExtension checks one entry and normalises it.
func validateExtension(ext *Extension) error {
	ext.Name = strings.TrimSpace(ext.Name)
	ext.Version = strings.TrimSpace(ext.Version)
	ext.Publisher = strings.TrimSpace(ext.Publisher)
	ext.Description = strings.TrimSpace(ext.Description)
	ext.Kind = strings.TrimSpace(ext.Kind)
	ext.DownloadURL = strings.TrimSpace(ext.DownloadURL)
	ext.SHA256 = strings.ToLower(strings.TrimSpace(ext.SHA256))
	ext.Homepage = strings.TrimSpace(ext.Homepage)

	if !namePattern.MatchString(ext.Name) {
		return &IndexError{
			Code:    CodeIndexInvalid,
			Message: fmt.Sprintf("the marketplace index has an invalid extension name %q", ext.Name),
		}
	}
	if _, err := updates.ParseVersion(ext.Version); err != nil {
		return &IndexError{
			Code:    CodeIndexInvalid,
			Message: fmt.Sprintf("extension %s has a version that is not a semantic version", ext.Name),
		}
	}
	if len(ext.Publisher) > MaxPublisherBytes {
		return &IndexError{
			Code:    CodeIndexInvalid,
			Message: fmt.Sprintf("extension %s has a publisher longer than %d bytes", ext.Name, MaxPublisherBytes),
		}
	}
	if len(ext.Description) > MaxDescriptionBytes {
		return &IndexError{
			Code:    CodeIndexInvalid,
			Message: fmt.Sprintf("extension %s has a description longer than %d bytes", ext.Name, MaxDescriptionBytes),
		}
	}
	switch ext.Kind {
	case "":
		ext.Kind = KindOther
	case KindGateway, KindProvisioner, KindOther:
	default:
		return &IndexError{
			Code:    CodeIndexInvalid,
			Message: fmt.Sprintf("extension %s declares an unknown kind %q", ext.Name, ext.Kind),
		}
	}
	if !sha256Pattern.MatchString(ext.SHA256) {
		return &IndexError{
			Code:    CodeIndexInvalid,
			Message: fmt.Sprintf("extension %s has no lower-case hex sha256", ext.Name),
		}
	}
	link, err := url.Parse(ext.DownloadURL)
	if err != nil || link.Host == "" || (link.Scheme != "http" && link.Scheme != "https") {
		return &IndexError{
			Code:    CodeIndexInvalid,
			Message: fmt.Sprintf("extension %s has a download_url that is not an http(s) URL", ext.Name),
		}
	}
	if ext.Homepage != "" {
		link, err := url.Parse(ext.Homepage)
		if err != nil || link.Host == "" || (link.Scheme != "http" && link.Scheme != "https") {
			return &IndexError{
				Code:    CodeIndexInvalid,
				Message: fmt.Sprintf("extension %s has a homepage that is not an http(s) URL", ext.Name),
			}
		}
	}
	return nil
}

// Find returns the entry for a name, preferring the highest version when several are listed.
func (idx Index) Find(name string) (Extension, bool) {
	name = strings.TrimSpace(name)
	var best Extension
	found := false
	for _, ext := range idx.Extensions {
		if ext.Name != name {
			continue
		}
		if !found {
			best, found = ext, true
			continue
		}
		bestVersion, _ := updates.ParseVersion(best.Version)
		thisVersion, _ := updates.ParseVersion(ext.Version)
		if thisVersion.Compare(bestVersion) > 0 {
			best = ext
		}
	}
	return best, found
}

// VerifyDigest compares a digest with an expected one and reports the mismatch as an error
// with a stable code.
func VerifyDigest(expected, actual string) error {
	if strings.EqualFold(expected, actual) {
		return nil
	}
	return &VerifyError{
		Code: CodeDigestMismatch,
		Message: fmt.Sprintf("the downloaded artifact's sha256 is %s, not the %s the index promised",
			actual, expected),
	}
}

// ValidDigest reports whether s is a lower-case hex SHA-256.
func ValidDigest(s string) bool { return sha256Pattern.MatchString(s) }

// NormaliseDigest lower-cases and trims a digest.
func NormaliseDigest(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// hexEncode is a small helper so callers do not import encoding/hex for one call.
func hexEncode(sum []byte) string { return hex.EncodeToString(sum) }
