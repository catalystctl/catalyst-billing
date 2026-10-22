package marketplace

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Install: download, verify, then store. The order is the whole point.
//
// The digest is checked before anything is written anywhere it could be executed or picked
// up by the panel, and before a row is written claiming the extension is installed. A
// mismatch aborts with nothing persisted except an audit row — the panel must not show
// "installed" for an artifact that failed verification, and it must not leave a partial file
// behind that a later step could mistake for a complete one.
//
// # Why the index digest is not enough on its own
//
// The index publishes the digest and serves the download URL. If the index is compromised,
// both can be replaced together and the check passes. This package therefore supports a
// *pinned* digest: the operator supplies the digest out of band and the install refuses
// unless the index digest and the downloaded bytes both equal the pin. It also stores the
// verified bytes so the artifact that was checked is the artifact that gets deployed.
//
// Without a pin, the digest catches a corrupted transfer and a tampered mirror, not a
// compromised index. `InstallResult.Pinned` reports which case an install was, and the API's
// `trust` field states it in words rather than leaving the operator to assume.

// Failure codes.
const (
	CodeArtifactUnreachable = "marketplace_artifact_unreachable"
	CodeArtifactTooLarge    = "marketplace_artifact_too_large"
	CodeArtifactEmpty       = "marketplace_artifact_empty"
	CodeDigestMismatch      = "marketplace_digest_mismatch"
	CodeDigestInvalid       = "marketplace_digest_invalid"
	CodeExtensionUnknown    = "marketplace_extension_unknown"
	CodeAlreadyInstalled    = "marketplace_already_installed"
	CodeVersionConflict     = "marketplace_version_conflict"
	CodeStagingFailed       = "marketplace_staging_failed"
	CodeNotConfigured       = "marketplace_not_configured"
)

// VerifyError is a refused install with a stable code.
type VerifyError struct {
	Code    string
	Message string
	err     error
}

func (e *VerifyError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return e.Code
}

func (e *VerifyError) Unwrap() error { return e.err }

// Download fetches an artifact with a bounded size and returns its bytes and digest.
//
// The limit is enforced while reading rather than from Content-Length, because
// Content-Length is the server's claim: a server that lies about it would otherwise be able
// to make the panel allocate an unbounded buffer.
func Download(ctx context.Context, client *http.Client, rawURL string, limit int64) ([]byte, string, error) {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, "", &VerifyError{
			Code:    CodeArtifactUnreachable,
			Message: "the extension's download URL is not an http(s) URL",
			err:     err,
		}
	}
	if limit <= 0 {
		limit = MaxArtifactBytes
	}

	ctx, cancel := context.WithTimeout(ctx, ArtifactTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return nil, "", &VerifyError{Code: CodeArtifactUnreachable, Message: "the artifact could not be requested", err: err}
	}
	if client == nil {
		client = &http.Client{Timeout: ArtifactTimeout}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", &VerifyError{
			Code:    CodeArtifactUnreachable,
			Message: "the artifact could not be downloaded",
			err:     err,
		}
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		return nil, "", &VerifyError{
			Code:    CodeArtifactUnreachable,
			Message: fmt.Sprintf("the artifact server answered %d", resp.StatusCode),
		}
	}

	// One byte over the limit is enough to know it is too large, and it means the whole body
	// never has to be read.
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, "", &VerifyError{
			Code:    CodeArtifactUnreachable,
			Message: "the artifact could not be read",
			err:     err,
		}
	}
	if int64(len(body)) > limit {
		return nil, "", &VerifyError{
			Code:    CodeArtifactTooLarge,
			Message: fmt.Sprintf("the artifact is larger than %d bytes", limit),
		}
	}
	if len(body) == 0 {
		return nil, "", &VerifyError{
			Code:    CodeArtifactEmpty,
			Message: "the artifact is empty",
		}
	}

	sum := sha256.Sum256(body)
	return body, hexEncode(sum[:]), nil
}

// InstallRequest is one install attempt.
type InstallRequest struct {
	// Name selects the index entry.
	Name string
	// Version selects a specific version; empty means the highest listed.
	Version string
	// PinnedSHA256 is the digest the operator supplied out of band. When set, the install
	// requires the index digest *and* the downloaded bytes to match it.
	PinnedSHA256 string
	// ActorID is the operator, for the audit row and the installed_by column.
	ActorID string
}

// InstallResult is what an install did.
type InstallResult struct {
	Name        string `json:"name"`
	Version     string `json:"version"`
	Publisher   string `json:"publisher"`
	Description string `json:"description"`
	Kind        string `json:"kind"`
	// SHA256 is the digest that was verified against the downloaded bytes.
	SHA256 string `json:"sha256"`
	// Pinned says the operator supplied the digest out of band.
	Pinned bool `json:"pinned"`
	// VerifiedAt is when the digest matched.
	VerifiedAt time.Time `json:"verified_at"`
	// SizeBytes is the artifact size that was verified.
	SizeBytes int64 `json:"size_bytes"`
	// StagedPath is where a copy was written, when a staging directory is configured.
	// Empty means the verified bytes live in the database only.
	StagedPath string `json:"staged_path"`
	// Status is "verified": the artifact is checked and stored. It is never "running": the
	// core cannot start an extension it was not configured to start.
	Status string `json:"status"`
	// Activation says what an operator must do to make the core use it.
	Activation string `json:"activation"`
	// Trust states, in words, what the digest check did and did not prove.
	Trust string `json:"trust"`
}

// activationFor renders the honest instruction for a verified extension.
//
// It names the environment variable the deployment actually reads (see cmd/billing and
// .env.example), because "installed" with no next step is the fake progress this feature is
// required to avoid.
func activationFor(ext Extension, stagedPath string) string {
	if stagedPath == "" {
		return "The verified artifact is stored in the panel. Download it from this page, place it where the " +
			"deployment expects it, and set " + envVarFor(ext.Kind) + " to its path. The panel does not start " +
			"extensions it was not configured to start."
	}
	return "Place the verified artifact at " + stagedPath + " (or wherever the deployment keeps it) and set " +
		envVarFor(ext.Kind) + " to that path, then restart `billing worker`. The panel does not start extensions " +
		"it was not configured to start."
}

// envVarFor names the environment variable the deployment reads for a kind.
func envVarFor(kind string) string {
	switch kind {
	case KindGateway:
		return "BILLING_GATEWAY_EXTENSION"
	case KindProvisioner:
		return "BILLING_PROVISIONER_EXTENSION"
	default:
		return "the extension path variable for its kind"
	}
}

// trustStatement explains what the digest check proved.
func trustStatement(pinned bool) string {
	if pinned {
		return "The artifact's sha256 matches both the index and the digest you supplied out of band, so the " +
			"download and the index entry were both checked against a value the index cannot change."
	}
	return "The artifact's sha256 matches the index. That proves the download was not corrupted or swapped in " +
		"transit; it does not prove the index itself is trustworthy, because a compromised index can change " +
		"both the entry and the digest. Pin the digest from the release announcement to close that gap."
}

// Install downloads, verifies and records one extension.
//
// Every guard runs before anything is persisted: the entry must exist, the artifact must be
// within the size bound, the digest must match (and the pin, when there is one), and only
// then is the row written. The staging copy is written *after* the digest matched, through a
// temporary file and a rename, so a reader can never observe a partially written artifact at
// the final path.
func (s *Store) Install(ctx context.Context, idx Index, req InstallRequest, client *http.Client) (InstallResult, error) {
	name := strings.TrimSpace(req.Name)
	if name == "" {
		return InstallResult{}, &VerifyError{Code: CodeExtensionUnknown, Message: "an extension name is required"}
	}

	ext, ok := idx.Find(name)
	if !ok {
		return InstallResult{}, &VerifyError{
			Code:    CodeExtensionUnknown,
			Message: fmt.Sprintf("no extension named %q is in the marketplace index", name),
		}
	}
	if wanted := strings.TrimSpace(req.Version); wanted != "" {
		found := false
		for _, candidate := range idx.Extensions {
			if candidate.Name == name && candidate.Version == wanted {
				ext, found = candidate, true
				break
			}
		}
		if !found {
			return InstallResult{}, &VerifyError{
				Code:    CodeExtensionUnknown,
				Message: fmt.Sprintf("the marketplace index has no version %q of %q", wanted, name),
			}
		}
	}

	pin := NormaliseDigest(req.PinnedSHA256)
	if pin != "" {
		if !ValidDigest(pin) {
			return InstallResult{}, &VerifyError{
				Code:    CodeDigestInvalid,
				Message: "the pinned digest must be 64 hex characters",
			}
		}
		// A pin that disagrees with the index is a refusal, not a preference: one of the two
		// is wrong and the operator must find out before anything is stored.
		if pin != ext.SHA256 {
			return InstallResult{}, &VerifyError{
				Code: CodeDigestMismatch,
				Message: fmt.Sprintf("the index lists %s for %s %s, but you pinned %s; nothing was installed",
					ext.SHA256, ext.Name, ext.Version, pin),
			}
		}
	}

	// Refuse a version that is already installed with different bytes. Replacing a verified
	// artifact with different bytes under the same version is how a "verified" extension
	// silently becomes an unverified one.
	if existing, err := s.installedDigest(ctx, ext.Name, ext.Version); err != nil {
		return InstallResult{}, err
	} else if existing != "" && existing != ext.SHA256 {
		return InstallResult{}, &VerifyError{
			Code: CodeVersionConflict,
			Message: fmt.Sprintf("%s %s is already installed with a different digest (%s); "+
				"publish a new version instead of replacing a verified artifact",
				ext.Name, ext.Version, existing),
		}
	}

	body, digest, err := Download(ctx, client, ext.DownloadURL, MaxArtifactBytes)
	if err != nil {
		return InstallResult{}, err
	}
	// The verification, and it happens before the row, the staging copy and the audit row.
	if err := VerifyDigest(ext.SHA256, digest); err != nil {
		return InstallResult{}, err
	}
	if pin != "" {
		if err := VerifyDigest(pin, digest); err != nil {
			return InstallResult{}, err
		}
	}

	// Staging happens only now, after the digest matched.
	stagedPath := ""
	cfg, err := s.Config(ctx)
	if err != nil {
		return InstallResult{}, err
	}
	if dir := strings.TrimSpace(cfg.InstallDir); dir != "" {
		stagedPath, err = stageArtifact(dir, ext, body)
		if err != nil {
			return InstallResult{}, &VerifyError{
				Code:    CodeStagingFailed,
				Message: "the artifact verified but could not be staged: " + err.Error(),
				err:     err,
			}
		}
	}

	verifiedAt := s.Now()
	result := InstallResult{
		Name:        ext.Name,
		Version:     ext.Version,
		Publisher:   ext.Publisher,
		Description: ext.Description,
		Kind:        ext.Kind,
		SHA256:      digest,
		Pinned:      pin != "",
		VerifiedAt:  verifiedAt,
		SizeBytes:   int64(len(body)),
		StagedPath:  stagedPath,
		Status:      "verified",
		Activation:  activationFor(ext, stagedPath),
		Trust:       trustStatement(pin != ""),
	}

	if err := s.recordInstall(ctx, ext, result, body, req.ActorID); err != nil {
		return InstallResult{}, err
	}
	return result, nil
}

// stageArtifact writes the verified bytes to dir atomically.
//
// The file name is derived from the validated extension name, its version and its digest, so
// two versions of one extension cannot collide and a re-install of the same bytes is the same
// file. The name is already restricted to `[a-z0-9._-]` by the index validator, so it cannot
// escape dir.
func stageArtifact(dir string, ext Extension, body []byte) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	name := fmt.Sprintf("%s-%s-%s", ext.Name, ext.Version, ext.SHA256[:12])
	final := filepath.Join(dir, name)

	tmp, err := os.CreateTemp(dir, "."+name+".*")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	defer func() {
		// A failure anywhere below removes the temporary file: a half-written artifact that a
		// later step could pick up is worse than no artifact.
		_ = os.Remove(tmpName)
	}()

	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		return "", err
	}
	// The file is executable because it is an extension binary the operator will point the
	// deployment at. Nothing executes it from here.
	if err := tmp.Chmod(0o755); err != nil {
		_ = tmp.Close()
		return "", err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(tmpName, final); err != nil {
		return "", err
	}
	return final, nil
}

// errNoPool is returned when the store was constructed without a pool.
var errNoPool = errors.New("marketplace: no database configured")
