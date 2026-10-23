package marketplace

// The local half of the marketplace client: upload, enable/disable, and the scaffold (ADM-12,
// OPS-14).
//
// # Why uploading exists
//
// The index path assumes the operator can reach a listing server that publishes a digest. An
// installation with no outbound network — which is most of them, once they are in a rack — could
// therefore not install an extension it already had on disk, and the only documented route was to
// copy a binary into place by hand, where nothing records its digest. The upload path is the same
// verification with the operator as the source of the digest: the bytes are hashed here, the
// digest is recorded, and the artifact is stored in the same table the indexed installs use. What
// it does not do is claim to have checked a publisher — there is no index to check against, and
// the trust statement says exactly that.
//
// # Why enable/disable exists
//
// "Installed" and "in use" were the same word in the panel: an extension appeared in the list and
// the deployment either pointed an environment variable at it or did not. An operator who wanted
// to stop using an extension without deleting the artifact (a gateway they are migrating away
// from, a provisioner whose credentials were revoked) had no way to say so, and a support
// engineer reading the list could not tell which of four gateways was live. `enabled` is that
// missing bit, it is stored per installed row, and the deployment reads it when it resolves a
// provider.
//
// # Why a scaffold exists
//
// The protocol is small and the panel's own docs are the wrong place to start: an operator wanting
// an extension for an internal system needs a directory whose shape is already right. The scaffold
// is a zip of exactly that — a manifest, a runnable stdio stub written in the standard library
// only, and a README that names the handshake — and it is generated here rather than shipped in
// the repository because it must carry the operator's chosen name, kind and version.
//
// It deliberately produces a *runnable* stub and not a skeleton of the panel's own packages: an
// extension is a separate program that speaks JSON-RPC over stdin/stdout, and a template that
// imported internal/extproto would only compile inside this repository.

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/catalyst/billing/internal/audit"
	"github.com/catalyst/billing/internal/updates"
	"github.com/jackc/pgx/v5"
)

// Codes this file adds to the marketplace's vocabulary. They are stable strings a client branches
// on; the install codes in install.go are unchanged.
const (
	// CodeUploadInvalid means the uploaded bytes or their metadata cannot be used.
	CodeUploadInvalid = "marketplace_upload_invalid"
	// CodeScaffoldInvalid means the requested scaffold name, kind or version is not acceptable.
	CodeScaffoldInvalid = "marketplace_scaffold_invalid"
	// CodeExtensionNotFound means there is no installed row with that name and version.
	CodeExtensionNotFound = "marketplace_extension_not_found"
)

// UploadRequest is one local artifact.
type UploadRequest struct {
	// Name, Version, Publisher, Description and Kind are the operator's metadata: there is no
	// index to read them from, and the row needs them to be recognisable a year later.
	Name        string
	Version     string
	Publisher   string
	Description string
	Kind        string
	// Body is the artifact, bounded by MaxArtifactBytes.
	Body []byte
	// PinnedSHA256 is the digest the operator expects. When set, the uploaded bytes must match
	// it, which is what makes an upload from a build machine verifiable at the receiving end.
	PinnedSHA256 string
	// ActorID is the operator, for the audit row and installed_by.
	ActorID string
	// Auditor writes the audit row inside the transaction that stores the row.
	Auditor audit.Auditor
}

// Upload verifies and stores an artifact the operator supplied.
//
// The order is the same as Install's and for the same reason: every guard runs before anything is
// written, so a refused upload leaves nothing behind that a later step could mistake for a
// verified extension.
func (s *Store) Upload(ctx context.Context, req UploadRequest) (InstallResult, error) {
	name := strings.TrimSpace(req.Name)
	version := strings.TrimSpace(req.Version)
	kind := strings.TrimSpace(req.Kind)
	if kind == "" {
		kind = KindOther
	}

	switch {
	case !namePattern.MatchString(name):
		return InstallResult{}, &VerifyError{Code: CodeUploadInvalid,
			Message: "the extension name must match [a-z0-9][a-z0-9._-]{0,63}"}
	case len(version) == 0 || len(version) > 64:
		return InstallResult{}, &VerifyError{Code: CodeUploadInvalid,
			Message: "a version of 1 to 64 characters is required"}
	}
	if _, err := updates.ParseVersion(version); err != nil {
		return InstallResult{}, &VerifyError{Code: CodeUploadInvalid,
			Message: "the version must be a semantic version, e.g. 1.2.0"}
	}
	switch kind {
	case KindGateway, KindProvisioner, KindOther:
	default:
		return InstallResult{}, &VerifyError{Code: CodeUploadInvalid,
			Message: fmt.Sprintf("unknown extension kind %q; one of gateway, provisioner, other", kind)}
	}
	if len(req.Body) == 0 {
		return InstallResult{}, &VerifyError{Code: CodeArtifactEmpty, Message: "the artifact is empty"}
	}
	if int64(len(req.Body)) > MaxArtifactBytes {
		return InstallResult{}, &VerifyError{Code: CodeArtifactTooLarge,
			Message: fmt.Sprintf("an artifact may be at most %d bytes", MaxArtifactBytes)}
	}

	sum := sha256.Sum256(req.Body)
	digest := hexEncode(sum[:])
	pin := NormaliseDigest(req.PinnedSHA256)
	if pin != "" && !ValidDigest(pin) {
		return InstallResult{}, &VerifyError{Code: CodeDigestInvalid,
			Message: "the expected digest must be 64 hex characters"}
	}
	if pin != "" {
		if err := VerifyDigest(pin, digest); err != nil {
			return InstallResult{}, err
		}
	}

	// The same rule the index path enforces, for the same reason: replacing a verified artifact
	// with different bytes under one version is how "verified" stops meaning anything.
	if existing, err := s.installedDigest(ctx, name, version); err != nil {
		return InstallResult{}, err
	} else if existing != "" && existing != digest {
		return InstallResult{}, &VerifyError{
			Code: CodeVersionConflict,
			Message: fmt.Sprintf("%s %s is already installed with a different digest (%s); "+
				"upload it as a new version instead of replacing a verified artifact",
				name, version, existing),
		}
	}

	// An upload has no URL to remember, so the source is a scheme rather than a lie: `upload:`
	// says the artifact came from the operator's machine and cannot be re-fetched by anyone.
	ext := Extension{
		Name:        name,
		Version:     version,
		Publisher:   strings.TrimSpace(req.Publisher),
		Description: strings.TrimSpace(req.Description),
		Kind:        kind,
		SHA256:      digest,
		DownloadURL: "upload:" + name,
	}
	if len(ext.Publisher) > MaxPublisherBytes {
		return InstallResult{}, &VerifyError{Code: CodeUploadInvalid,
			Message: fmt.Sprintf("a publisher may be at most %d bytes", MaxPublisherBytes)}
	}
	if len(ext.Description) > MaxDescriptionBytes {
		return InstallResult{}, &VerifyError{Code: CodeUploadInvalid,
			Message: fmt.Sprintf("a description may be at most %d bytes", MaxDescriptionBytes)}
	}

	result := InstallResult{
		Name:        ext.Name,
		Version:     ext.Version,
		Publisher:   ext.Publisher,
		Description: ext.Description,
		Kind:        ext.Kind,
		SHA256:      digest,
		Pinned:      pin != "",
		VerifiedAt:  s.Now(),
		SizeBytes:   int64(len(req.Body)),
		Status:      "verified",
		Activation:  activationFor(ext, ""),
		Trust: "The artifact's sha256 was computed from the bytes you uploaded and recorded with it. " +
			"That proves the bytes stored are the bytes you have; it does not prove where they came from, " +
			"and there is no index to check a publisher against. Pin a digest from the release announcement " +
			"when you need the upload to match a published build.",
	}

	if err := s.recordUpload(ctx, ext, result, req); err != nil {
		return InstallResult{}, err
	}
	return result, nil
}

// recordUpload stores the row, its artifact and the audit row in one transaction.
func (s *Store) recordUpload(ctx context.Context, ext Extension, result InstallResult, req UploadRequest) error {
	return s.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var id string
		err := tx.QueryRow(ctx, `
			INSERT INTO installed_extensions
			    (name, version, publisher, description, kind, sha256, pinned_sha256,
			     source_url, size_bytes, staged_path, installed_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, '', $10)
			ON CONFLICT (name, version) DO UPDATE
				SET publisher = EXCLUDED.publisher,
				    description = EXCLUDED.description,
				    kind = EXCLUDED.kind,
				    installed_by = EXCLUDED.installed_by,
				    installed_at = now()
			RETURNING id`,
			ext.Name, ext.Version, ext.Publisher, ext.Description, ext.Kind, result.SHA256,
			pinnedValue(result), ext.DownloadURL, result.SizeBytes, nullableUUID(req.ActorID)).Scan(&id)
		if err != nil {
			return fmt.Errorf("marketplace: record upload: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO installed_extension_artifacts (extension_id, sha256, content)
			VALUES ($1, $2, $3)
			ON CONFLICT (extension_id) DO UPDATE
				SET sha256 = EXCLUDED.sha256, content = EXCLUDED.content`,
			id, result.SHA256, req.Body); err != nil {
			return fmt.Errorf("marketplace: store uploaded artifact: %w", err)
		}
		if req.Auditor != nil {
			// The audit row is written in this transaction, so a rolled-back upload cannot leave
			// a row claiming an extension was installed. The digest is in the row; the bytes are
			// not, because an audit log is not a package store.
			if err := req.Auditor.RecordAction(ctx, tx, "extension.upload", "installed_extension", id,
				map[string]any{
					"name":       ext.Name,
					"version":    ext.Version,
					"kind":       ext.Kind,
					"sha256":     result.SHA256,
					"size_bytes": result.SizeBytes,
					"pinned":     result.Pinned,
				}); err != nil {
				return err
			}
		}
		return nil
	})
}

// SetEnabled turns one installed extension on or off.
//
// The row is the unit, not the name: an operator may keep two versions installed, one enabled
// while they cut over. It returns the updated row so the screen renders what was stored rather
// than what was requested.
func (s *Store) SetEnabled(ctx context.Context, name, version string, enabled bool,
	actorID string, auditor audit.Auditor) (Installed, error) {
	if strings.TrimSpace(name) == "" || strings.TrimSpace(version) == "" {
		return Installed{}, &VerifyError{Code: CodeExtensionNotFound,
			Message: "an extension name and version are required"}
	}

	var out Installed
	err := s.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var id string
		err := tx.QueryRow(ctx, `
			UPDATE installed_extensions SET enabled = $3
			WHERE name = $1 AND version = $2
			RETURNING id::text, name, version, publisher, description, kind, sha256,
			          pinned_sha256 IS NOT NULL, source_url, size_bytes, staged_path,
			          installed_at, enabled`,
			name, version, enabled).Scan(&id, &out.Name, &out.Version, &out.Publisher,
			&out.Description, &out.Kind, &out.SHA256, &out.Pinned, &out.SourceURL,
			&out.SizeBytes, &out.StagedPath, &out.InstalledAt, &out.Enabled)
		if errors.Is(err, pgx.ErrNoRows) {
			return &VerifyError{Code: CodeExtensionNotFound,
				Message: fmt.Sprintf("no installed extension named %q at version %q", name, version)}
		}
		if err != nil {
			return fmt.Errorf("marketplace: set enabled: %w", err)
		}
		out.Status = "verified"
		if auditor != nil {
			return auditor.RecordAction(ctx, tx, "extension.set_enabled", "installed_extension", id,
				map[string]any{"name": name, "version": version, "enabled": enabled})
		}
		return nil
	})
	if err != nil {
		return Installed{}, err
	}
	out.InstalledAt = out.InstalledAt.UTC()
	return out, nil
}

// ScaffoldRequest is one generated skeleton.
type ScaffoldRequest struct {
	Name    string
	Version string
	Kind    string
	// Publisher is written into the manifest so the stub answers the handshake with it.
	Publisher string
}

// Scaffold builds a zip an operator can unpack and build.
//
// The files are deliberately small and the stub is standard-library Go: it compiles anywhere Go
// does, it speaks the protocol without importing this repository, and its `initialize` answer is
// the first thing the core checks. A template that only compiled inside the panel's own module
// would be an odd thing to ship to someone writing an extension for a different machine.
func Scaffold(req ScaffoldRequest) ([]byte, string, error) {
	name := strings.TrimSpace(req.Name)
	kind := strings.TrimSpace(req.Kind)
	if kind == "" {
		kind = KindOther
	}
	version := strings.TrimSpace(req.Version)
	if version == "" {
		version = "0.1.0"
	}
	publisher := strings.TrimSpace(req.Publisher)
	if publisher == "" {
		publisher = name
	}

	if !namePattern.MatchString(name) {
		return nil, "", &VerifyError{Code: CodeScaffoldInvalid,
			Message: "the name must match [a-z0-9][a-z0-9._-]{0,63} — it becomes the directory and the manifest entry"}
	}
	if _, err := updates.ParseVersion(version); err != nil {
		return nil, "", &VerifyError{Code: CodeScaffoldInvalid,
			Message: "the version must be a semantic version, e.g. 0.1.0"}
	}
	switch kind {
	case KindGateway, KindProvisioner, KindOther:
	default:
		return nil, "", &VerifyError{Code: CodeScaffoldInvalid,
			Message: fmt.Sprintf("unknown kind %q; one of gateway, provisioner, other", kind)}
	}

	files := []struct {
		name string
		body string
	}{
		{name + "/manifest.json", scaffoldManifest(name, version, kind, publisher)},
		{name + "/main.go", scaffoldMain(name, kind, publisher)},
		{name + "/go.mod", "module " + moduleFor(name) + "\n\ngo 1.23\n"},
		{name + "/README.md", scaffoldReadme(name, version, kind)},
	}

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, file := range files {
		// A fixed timestamp: a zip with the current time in every header produces different bytes
		// for the same scaffold, which would make "is this the file I generated?" unanswerable.
		header := &zip.FileHeader{Name: file.name, Method: zip.Deflate, Modified: time.Unix(0, 0).UTC()}
		w, err := zw.CreateHeader(header)
		if err != nil {
			return nil, "", &VerifyError{Code: CodeScaffoldInvalid, Message: "build the scaffold: " + err.Error()}
		}
		if _, err := w.Write([]byte(file.body)); err != nil {
			return nil, "", &VerifyError{Code: CodeScaffoldInvalid, Message: "build the scaffold: " + err.Error()}
		}
	}
	if err := zw.Close(); err != nil {
		return nil, "", &VerifyError{Code: CodeScaffoldInvalid, Message: "build the scaffold: " + err.Error()}
	}
	return buf.Bytes(), name + "-" + version + "-scaffold.zip", nil
}

// moduleFor turns an extension name into a Go module path that is valid and obviously local.
func moduleFor(name string) string { return "example.com/extensions/" + name }

// scaffoldManifest is the metadata file the operator fills in before publishing.
func scaffoldManifest(name, version, kind, publisher string) string {
	capabilities := `[
    "initialize",
    "health"
  ]`
	if kind == KindGateway {
		capabilities = `[
    "initialize",
    "health",
    "gateway.describe",
    "gateway.create_checkout",
    "gateway.query_payment",
    "gateway.refund",
    "gateway.verify_webhook"
  ]`
	} else if kind == KindProvisioner {
		capabilities = `[
    "initialize",
    "health",
    "provisioner.describe",
    "provisioner.validate",
    "provisioner.create",
    "provisioner.inspect",
    "provisioner.lookup",
    "provisioner.suspend",
    "provisioner.resume",
    "provisioner.terminate"
  ]`
	}
	return fmt.Sprintf(`{
  "$comment": "Fill in the empty values, then publish this artifact's sha256 wherever your operators get it from.",
  "name": %q,
  "version": %q,
  "kind": %q,
  "publisher": %q,
  "protocol": "billing.ext/1",
  "capabilities": %s,
  "env": [],
  "homepage": ""
}
`, name, version, kind, publisher, capabilities)
}

// scaffoldReadme explains the protocol in the words an operator needs.
func scaffoldReadme(name, version, kind string) string {
	return "# " + name + "\n\n" +
		"An extension for Catalyst Billing, version " + version + ", kind `" + kind + "`.\n\n" +
		"## How the panel starts it\n\n" +
		"The panel launches the extension as a child process and speaks JSON-RPC 2.0 over stdin/stdout,\n" +
		"one JSON object per line. It sends `initialize` first and expects the handshake reply within\n" +
		"the call timeout; nothing else is called until that succeeds. `health` may be called at any\n" +
		"time, including before the handshake.\n\n" +
		"```sh\ngo build -o " + name + " .\nBILLING_GATEWAY_EXTENSION=$PWD/" + name + " billing worker   # for a gateway\n```\n\n" +
		"or set `BILLING_PROVISIONER_EXTENSION` for a provisioner. Then install the binary from the\n" +
		"marketplace screen (Upload) so the panel records its digest, and enable it there.\n\n" +
		"## The handshake\n\n" +
		"Request:  `{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"initialize\",\"params\":{\"protocol\":\"billing.ext/1\",\"versions\":[1]}}`\n\n" +
		"Reply:    `{\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"version\":1,\"name\":\"...\",\"publisher\":\"...\",\"capabilities\":[\"...\"]}}`\n\n" +
		"An extension promises exactly the capabilities it lists; the panel refuses to call a method\n" +
		"that was not advertised, so a half-finished extension fails visibly rather than silently.\n\n" +
		"## Things this protocol insists on\n\n" +
		"- **Idempotency keys.** Every mutating call carries an `operation_key`. Replaying it must return\n" +
		"  the result of the first execution, never perform a second one. This is what makes the panel's\n" +
		"  retry queue safe.\n" +
		"- **Cancellation.** `$/cancelRequest` means stop working on that id; a late answer is discarded.\n" +
		"- **Secrets.** Credentials arrive in the environment, never in a payload. Do not log them.\n" +
		"- **No floating point.** Money is an exact count of minor units plus a currency code.\n"
}

// scaffoldMain is a runnable stdio stub: it implements the handshake and the health probe and
// answers every other method with a JSON-RPC error, which is the honest state of a new extension.
func scaffoldMain(name, kind, publisher string) string {
	return `// The " + name + " extension: a starting point, not an implementation.
//
// It speaks JSON-RPC 2.0 over stdin/stdout, one message per line, and it answers the two calls the
// panel makes before it trusts an extension: initialize and health. Every other method returns an
// error, so the panel reports "capability not implemented" instead of hanging.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
)

const (
	name        = "` + name + `"
	kind        = "` + kind + `"
	publisher   = "` + publisher + `"
	protocol    = "billing.ext/1"
	// capabilities must list exactly the methods this program implements.
	capabilities = []string{"initialize", "health"}
)

type request struct {
	JSONRPC string          ` + "`json:\"jsonrpc\"`" + `
	ID      *uint64         ` + "`json:\"id\"`" + `
	Method  string          ` + "`json:\"method\"`" + `
	Params  json.RawMessage ` + "`json:\"params\"`" + `
}

type response struct {
	JSONRPC string ` + "`json:\"jsonrpc\"`" + `
	ID      *uint64 ` + "`json:\"id\"`" + `
	Result  any    ` + "`json:\"result,omitempty\"`" + `
	Error   *rpcError ` + "`json:\"error,omitempty\"`" + `
}

type rpcError struct {
	Code    int    ` + "`json:\"code\"`" + `
	Message string ` + "`json:\"message\"`" + `
}

func main() {
	in := bufio.NewScanner(os.Stdin)
	// A frame may carry a whole configuration map; the panel caps its own frames at 1 MiB.
	in.Buffer(make([]byte, 0, 64<<10), 1<<20)
	out := bufio.NewWriter(os.Stdout)
	defer out.Flush()

	for in.Scan() {
		line := in.Bytes()
		if len(line) == 0 {
			continue
		}
		var req request
		if err := json.Unmarshal(line, &req); err != nil {
			write(out, response{JSONRPC: "2.0", Error: &rpcError{Code: -32700, Message: "parse error"}})
			continue
		}
		switch req.Method {
		case "initialize":
			write(out, response{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{
				"version":      1,
				"name":         name,
				"publisher":    publisher,
				"capabilities": capabilities,
			}})
		case "health":
			write(out, response{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{"ok": true}})
		case "$/cancelRequest":
			// A notification: no answer. There is nothing to cancel yet.
		default:
			write(out, response{JSONRPC: "2.0", ID: req.ID, Error: &rpcError{
				Code:    -32601,
				Message: fmt.Sprintf("%s does not implement %q yet", name, req.Method),
			}})
		}
	}
	if err := in.Err(); err != nil {
		fmt.Fprintln(os.Stderr, name+": "+err.Error())
		os.Exit(1)
	}
}

func write(out *bufio.Writer, resp response) {
	encoded, err := json.Marshal(resp)
	if err != nil {
		fmt.Fprintln(os.Stderr, name+": encode: "+err.Error())
		return
	}
	if _, err := out.Write(append(encoded, '\n')); err != nil {
		fmt.Fprintln(os.Stderr, name+": write: "+err.Error())
		return
	}
	if err := out.Flush(); err != nil {
		fmt.Fprintln(os.Stderr, name+": flush: "+err.Error())
	}
}
`
}
