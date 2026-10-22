package marketplace_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/catalyst/billing/internal/marketplace"
	"github.com/catalyst/billing/internal/store/storetest"
	"github.com/jackc/pgx/v5"
)

// The marketplace index and the install path.
//
// The properties that matter are the ones a fake install would skip: the artifact is bounded,
// the digest is checked *before* anything is recorded, a mismatch refuses and leaves nothing
// behind, a pinned digest has to agree with both the index and the bytes, and the same version
// cannot be replaced with different bytes.

// artifact is a fake extension binary.
var artifact = []byte("#!/bin/sh\necho fake extension\n")

// digestOf returns the lower-case hex SHA-256 of body.
func digestOf(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// artifactServer serves the fake artifact and counts downloads.
func artifactServer(t *testing.T, body []byte) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// indexJSON renders an index with one extension.
func indexJSON(downloadURL, digest string) string {
	body, _ := json.Marshal(map[string]any{
		"schema_version": 1,
		"extensions": []map[string]any{{
			"name":         "fakegateway",
			"version":      "1.2.0",
			"publisher":    "Catalyst",
			"description":  "A gateway that takes pretend money.",
			"kind":         "gateway",
			"download_url": downloadURL,
			"sha256":       digest,
		}},
	})
	return string(body)
}

// ---------------------------------------------------------------- parsing --

func TestParseIndexRejectsBadEntries(t *testing.T) {
	good := marketplace.Extension{
		Name: "fakegateway", Version: "1.2.0", Publisher: "Catalyst",
		Kind: marketplace.KindGateway, DownloadURL: "https://example.test/a",
		SHA256: strings.Repeat("a", 64),
	}
	render := func(ext marketplace.Extension, schemaVersion int) string {
		body, _ := json.Marshal(map[string]any{
			"schema_version": schemaVersion,
			"extensions":     []marketplace.Extension{ext},
		})
		return string(body)
	}

	if _, err := marketplace.ParseIndex([]byte(render(good, 1))); err != nil {
		t.Fatalf("a valid index was refused: %v", err)
	}

	cases := map[string]string{
		"path traversal in the name": render(func() marketplace.Extension {
			ext := good
			ext.Name = "../../etc/passwd"
			return ext
		}(), 1),
		"uppercase name": render(func() marketplace.Extension {
			ext := good
			ext.Name = "FakeGateway"
			return ext
		}(), 1),
		"version that is not a version": render(func() marketplace.Extension {
			ext := good
			ext.Version = "latest"
			return ext
		}(), 1),
		"short digest": render(func() marketplace.Extension {
			ext := good
			ext.SHA256 = "abc"
			return ext
		}(), 1),
		"file download url": render(func() marketplace.Extension {
			ext := good
			ext.DownloadURL = "file:///etc/passwd"
			return ext
		}(), 1),
		"unknown kind": render(func() marketplace.Extension {
			ext := good
			ext.Kind = "kernel-module"
			return ext
		}(), 1),
		"newer schema version": render(good, 2),
		"not json":             `<html>maintenance</html>`,
		"unknown field": `{"schema_version":1,"extensions":[{"name":"fakegateway","version":"1.2.0",
			"download_url":"https://example.test/a","sha256":"` + strings.Repeat("a", 64) + `",
			"post_install":"rm -rf /"}]}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := marketplace.ParseIndex([]byte(body)); err == nil {
				t.Fatalf("the index was accepted: %s", body)
			}
		})
	}
}

// A digest written in upper case is the same digest, so it is normalised rather than refused:
// refusing it would reject an index a publisher wrote in a different case for no benefit.
func TestParseIndexNormalisesAnUppercaseDigest(t *testing.T) {
	body := `{"schema_version":1,"extensions":[{"name":"fakegateway","version":"1.2.0",
		"download_url":"https://example.test/a","sha256":"` + strings.Repeat("A", 64) + `"}]}`
	index, err := marketplace.ParseIndex([]byte(body))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if index.Extensions[0].SHA256 != strings.Repeat("a", 64) {
		t.Fatalf("digest = %q, want it lower-cased", index.Extensions[0].SHA256)
	}
	if index.Extensions[0].Kind != marketplace.KindOther {
		t.Fatalf("kind = %q, want %q when the index omits it", index.Extensions[0].Kind, marketplace.KindOther)
	}
}

// A duplicate name and version is a broken index rather than a last-one-wins.
func TestParseIndexRejectsDuplicates(t *testing.T) {
	entry := map[string]any{
		"name": "fakegateway", "version": "1.2.0",
		"download_url": "https://example.test/a", "sha256": strings.Repeat("a", 64),
	}
	body, _ := json.Marshal(map[string]any{
		"schema_version": 1,
		"extensions":     []map[string]any{entry, entry},
	})
	if _, err := marketplace.ParseIndex(body); err == nil {
		t.Fatal("an index listing the same name and version twice was accepted")
	}
}

// Find prefers the highest version, so "install fakegateway" does not install an old release
// just because it is listed first.
func TestFindPrefersTheHighestVersion(t *testing.T) {
	body := `{"schema_version":1,"extensions":[
		{"name":"fakegateway","version":"1.2.0","download_url":"https://example.test/a","sha256":"` +
		strings.Repeat("a", 64) + `"},
		{"name":"fakegateway","version":"1.10.0","download_url":"https://example.test/b","sha256":"` +
		strings.Repeat("b", 64) + `"},
		{"name":"fakegateway","version":"1.9.9","download_url":"https://example.test/c","sha256":"` +
		strings.Repeat("c", 64) + `"}]}`
	index, err := marketplace.ParseIndex([]byte(body))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	ext, ok := index.Find("fakegateway")
	if !ok {
		t.Fatal("Find did not find the extension")
	}
	if ext.Version != "1.10.0" {
		t.Fatalf("Find returned version %q, want 1.10.0", ext.Version)
	}
}

// ---------------------------------------------------------------- install --

// installEnv is a configured store plus a working artifact server.
type installEnv struct {
	store    *marketplace.Store
	index    marketplace.Index
	server   *httptest.Server
	hits     *atomic.Int64
	artifact []byte
}

func newInstallEnv(t *testing.T) *installEnv {
	t.Helper()
	pool := storetest.DB(t)
	srv, hits := artifactServer(t, artifact)
	index, err := marketplace.ParseIndex([]byte(indexJSON(srv.URL+"/fakegateway-1.2.0", digestOf(artifact))))
	if err != nil {
		t.Fatalf("parse index: %v", err)
	}
	store := marketplace.New(pool)
	// The configuration row is recreated on read when a truncate removed it, so an install
	// in this test does not depend on a previous test having left one behind.
	if _, err := store.Config(context.Background()); err != nil {
		t.Fatalf("config: %v", err)
	}
	return &installEnv{store: store, index: index, server: srv, hits: hits, artifact: artifact}
}

// The happy path: the digest matches, the bytes are stored, and the row records the digest
// that was verified.
func TestInstallStoresAVerifiedArtifact(t *testing.T) {
	env := newInstallEnv(t)
	ctx := context.Background()

	result, err := env.store.Install(ctx, env.index, marketplace.InstallRequest{
		Name: "fakegateway",
	}, nil)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if result.SHA256 != digestOf(env.artifact) {
		t.Fatalf("recorded digest = %q, want %q", result.SHA256, digestOf(env.artifact))
	}
	if result.Status != "verified" {
		// Never "running": the core cannot start an extension it was not configured to start.
		t.Fatalf("status = %q, want verified", result.Status)
	}
	if result.Pinned {
		t.Fatal("an unpinned install reported itself as pinned")
	}
	if !strings.Contains(result.Trust, "does not prove the index") {
		t.Fatalf("the trust statement does not explain the limit of an unpinned digest: %q", result.Trust)
	}
	if !strings.Contains(result.Activation, "BILLING_GATEWAY_EXTENSION") {
		t.Fatalf("the activation instruction does not name the variable the deployment reads: %q", result.Activation)
	}

	// The stored artifact is the exact bytes that were verified, so the operator can deploy
	// what was checked.
	body, digest, err := env.store.Artifact(ctx, "fakegateway", "1.2.0")
	if err != nil {
		t.Fatalf("read artifact: %v", err)
	}
	if string(body) != string(env.artifact) {
		t.Fatalf("the stored artifact differs from the verified bytes")
	}
	if digest != result.SHA256 {
		t.Fatalf("stored digest = %q, want %q", digest, result.SHA256)
	}

	installed, err := env.store.ListInstalled(ctx)
	if err != nil {
		t.Fatalf("list installed: %v", err)
	}
	if len(installed) != 1 {
		t.Fatalf("installed count = %d, want 1", len(installed))
	}
	if installed[0].Name != "fakegateway" || installed[0].Version != "1.2.0" {
		t.Fatalf("installed row = %+v", installed[0])
	}
	if installed[0].Status != "verified" {
		t.Fatalf("installed status = %q, want verified", installed[0].Status)
	}
}

// The acceptance criterion: a mismatched digest is refused, and nothing is recorded.
func TestInstallRefusesADigestMismatch(t *testing.T) {
	env := newInstallEnv(t)
	ctx := context.Background()

	// The index promises a digest the artifact does not have, which is what a tampered
	// mirror or a corrupted transfer looks like.
	wrong := strings.Repeat("0", 64)
	index, err := marketplace.ParseIndex([]byte(indexJSON(env.server.URL+"/fakegateway-1.2.0", wrong)))
	if err != nil {
		t.Fatalf("parse index: %v", err)
	}

	_, err = env.store.Install(ctx, index, marketplace.InstallRequest{Name: "fakegateway"}, nil)
	if err == nil {
		t.Fatal("an install with a mismatched digest succeeded")
	}
	var verifyErr *marketplace.VerifyError
	if !asVerify(err, &verifyErr) {
		t.Fatalf("error = %v, want a VerifyError", err)
	}
	if verifyErr.Code != marketplace.CodeDigestMismatch {
		t.Fatalf("code = %q, want %q", verifyErr.Code, marketplace.CodeDigestMismatch)
	}

	// Nothing was recorded and nothing was stored: "installed" for an artifact that failed
	// verification is exactly the fake state this feature must not show.
	installed, err := env.store.ListInstalled(ctx)
	if err != nil {
		t.Fatalf("list installed: %v", err)
	}
	if len(installed) != 0 {
		t.Fatalf("a refused install left %d installed rows", len(installed))
	}
	if body, _, _ := env.store.Artifact(ctx, "fakegateway", "1.2.0"); body != nil {
		t.Fatal("a refused install stored an artifact")
	}
}

// A pinned digest has to agree with the index as well as with the bytes: one of the two is
// wrong, and the operator must find out before anything is stored.
func TestInstallRefusesAPinThatDisagreesWithTheIndex(t *testing.T) {
	env := newInstallEnv(t)
	_, err := env.store.Install(context.Background(), env.index, marketplace.InstallRequest{
		Name:         "fakegateway",
		PinnedSHA256: strings.Repeat("f", 64),
	}, nil)
	if err == nil {
		t.Fatal("an install with a pin that disagrees with the index succeeded")
	}
	var verifyErr *marketplace.VerifyError
	if !asVerify(err, &verifyErr) || verifyErr.Code != marketplace.CodeDigestMismatch {
		t.Fatalf("error = %v, want a digest mismatch", err)
	}
}

// A pin that matches both the index and the bytes is recorded as pinned, and the trust
// statement says what that proved.
func TestInstallWithAMatchingPinIsRecordedAsPinned(t *testing.T) {
	env := newInstallEnv(t)
	result, err := env.store.Install(context.Background(), env.index, marketplace.InstallRequest{
		Name:         "fakegateway",
		PinnedSHA256: strings.ToUpper(digestOf(env.artifact)),
	}, nil)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if !result.Pinned {
		t.Fatal("an install with a pin did not report itself as pinned")
	}
	if !strings.Contains(result.Trust, "out of band") {
		t.Fatalf("the trust statement does not mention the pin: %q", result.Trust)
	}

	installed, err := env.store.ListInstalled(context.Background())
	if err != nil {
		t.Fatalf("list installed: %v", err)
	}
	if len(installed) != 1 || !installed[0].Pinned {
		t.Fatalf("the installed row does not record the pin: %+v", installed)
	}
}

// A malformed pin is refused rather than ignored: silently treating "abc" as "no pin" would
// install without the check the operator asked for.
func TestInstallRefusesAMalformedPin(t *testing.T) {
	env := newInstallEnv(t)
	_, err := env.store.Install(context.Background(), env.index, marketplace.InstallRequest{
		Name:         "fakegateway",
		PinnedSHA256: "not-a-digest",
	}, nil)
	if err == nil {
		t.Fatal("an install with a malformed pin succeeded")
	}
	var verifyErr *marketplace.VerifyError
	if !asVerify(err, &verifyErr) || verifyErr.Code != marketplace.CodeDigestInvalid {
		t.Fatalf("error = %v, want a digest invalid error", err)
	}
}

// The same version with different bytes is refused rather than replaced: replacing a verified
// artifact under the same version is how "verified" stops meaning anything.
func TestInstallRefusesToReplaceAVersionWithDifferentBytes(t *testing.T) {
	env := newInstallEnv(t)
	ctx := context.Background()
	if _, err := env.store.Install(ctx, env.index, marketplace.InstallRequest{Name: "fakegateway"}, nil); err != nil {
		t.Fatalf("first install: %v", err)
	}

	other := []byte("#!/bin/sh\necho a different binary\n")
	otherSrv, _ := artifactServer(t, other)
	index, err := marketplace.ParseIndex([]byte(indexJSON(otherSrv.URL+"/fakegateway-1.2.0", digestOf(other))))
	if err != nil {
		t.Fatalf("parse index: %v", err)
	}
	_, err = env.store.Install(ctx, index, marketplace.InstallRequest{Name: "fakegateway"}, nil)
	if err == nil {
		t.Fatal("a second install of the same version with different bytes succeeded")
	}
	var verifyErr *marketplace.VerifyError
	if !asVerify(err, &verifyErr) || verifyErr.Code != marketplace.CodeVersionConflict {
		t.Fatalf("error = %v, want a version conflict", err)
	}

	// The original artifact is untouched.
	body, digest, err := env.store.Artifact(ctx, "fakegateway", "1.2.0")
	if err != nil {
		t.Fatalf("read artifact: %v", err)
	}
	if digest != digestOf(env.artifact) || string(body) != string(env.artifact) {
		t.Fatal("a refused replacement overwrote the verified artifact")
	}
}

// Re-installing the same bytes is idempotent, which is what a retried install should be.
func TestReinstallOfTheSameBytesIsIdempotent(t *testing.T) {
	env := newInstallEnv(t)
	ctx := context.Background()
	first, err := env.store.Install(ctx, env.index, marketplace.InstallRequest{Name: "fakegateway"}, nil)
	if err != nil {
		t.Fatalf("first install: %v", err)
	}
	second, err := env.store.Install(ctx, env.index, marketplace.InstallRequest{Name: "fakegateway"}, nil)
	if err != nil {
		t.Fatalf("second install: %v", err)
	}
	if first.SHA256 != second.SHA256 {
		t.Fatalf("digests differ: %q and %q", first.SHA256, second.SHA256)
	}
	installed, err := env.store.ListInstalled(ctx)
	if err != nil {
		t.Fatalf("list installed: %v", err)
	}
	if len(installed) != 1 {
		t.Fatalf("installed count = %d, want 1", len(installed))
	}
}

// The download is bounded. An artifact server that streams forever must not fill the panel's
// memory, and the bound is enforced while reading rather than from Content-Length.
func TestInstallRefusesAnOversizedArtifact(t *testing.T) {
	pool := storetest.DB(t)
	big := make([]byte, marketplace.MaxArtifactBytes+1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(big)
	}))
	t.Cleanup(srv.Close)

	index, err := marketplace.ParseIndex([]byte(indexJSON(srv.URL+"/big", digestOf(big))))
	if err != nil {
		t.Fatalf("parse index: %v", err)
	}
	store := marketplace.New(pool)
	if _, err := store.Config(context.Background()); err != nil {
		t.Fatalf("config: %v", err)
	}
	_, err = store.Install(context.Background(), index, marketplace.InstallRequest{Name: "fakegateway"}, nil)
	if err == nil {
		t.Fatal("an oversized artifact was accepted")
	}
	var verifyErr *marketplace.VerifyError
	if !asVerify(err, &verifyErr) || verifyErr.Code != marketplace.CodeArtifactTooLarge {
		t.Fatalf("error = %v, want an oversized artifact error", err)
	}
}

// An unknown name is a 404-shaped refusal, not an empty install.
func TestInstallUnknownExtension(t *testing.T) {
	env := newInstallEnv(t)
	_, err := env.store.Install(context.Background(), env.index,
		marketplace.InstallRequest{Name: "not-in-the-index"}, nil)
	if err == nil {
		t.Fatal("installing an unknown extension succeeded")
	}
	var verifyErr *marketplace.VerifyError
	if !asVerify(err, &verifyErr) || verifyErr.Code != marketplace.CodeExtensionUnknown {
		t.Fatalf("error = %v, want an unknown extension error", err)
	}
}

// With a staging directory configured, the verified artifact is written there atomically, and
// the path is reported. Staging happens only after the digest matched.
func TestInstallStagesTheVerifiedArtifact(t *testing.T) {
	pool := storetest.DB(t)
	dir := t.TempDir()
	srv, _ := artifactServer(t, artifact)
	index, err := marketplace.ParseIndex([]byte(indexJSON(srv.URL+"/fakegateway-1.2.0", digestOf(artifact))))
	if err != nil {
		t.Fatalf("parse index: %v", err)
	}

	store := marketplace.New(pool)
	if err := pool.InTx(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return store.SetConfig(ctx, tx, "https://example.test/index.json", dir, "")
	}); err != nil {
		t.Fatalf("set config: %v", err)
	}
	t.Cleanup(func() {
		_ = pool.InTx(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
			return store.SetConfig(ctx, tx, "", "", "")
		})
	})

	result, err := store.Install(context.Background(), index, marketplace.InstallRequest{Name: "fakegateway"}, nil)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if result.StagedPath == "" {
		t.Fatal("a staging directory is configured but nothing was staged")
	}
	if filepath.Dir(result.StagedPath) != dir {
		t.Fatalf("staged path %q is not inside %q", result.StagedPath, dir)
	}
	staged, err := os.ReadFile(result.StagedPath)
	if err != nil {
		t.Fatalf("read staged artifact: %v", err)
	}
	if string(staged) != string(artifact) {
		t.Fatal("the staged artifact differs from the verified bytes")
	}
	// Executable, because it is the extension binary the operator will point the deployment
	// at. Nothing executes it from here.
	info, err := os.Stat(result.StagedPath)
	if err != nil {
		t.Fatalf("stat staged artifact: %v", err)
	}
	if info.Mode().Perm()&0o100 == 0 {
		t.Fatalf("the staged artifact is not executable: %v", info.Mode())
	}
	// No temporary file is left behind: a half-written artifact a later step could pick up is
	// worse than none.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read staging dir: %v", err)
	}
	if len(entries) != 1 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("the staging directory has %d entries (%v), want 1", len(entries), names)
	}
}

// A refused install does not stage anything either: the verification happens first.
func TestRefusedInstallStagesNothing(t *testing.T) {
	pool := storetest.DB(t)
	dir := t.TempDir()
	srv, _ := artifactServer(t, artifact)
	index, err := marketplace.ParseIndex([]byte(indexJSON(srv.URL+"/fakegateway-1.2.0", strings.Repeat("0", 64))))
	if err != nil {
		t.Fatalf("parse index: %v", err)
	}
	store := marketplace.New(pool)
	if err := pool.InTx(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return store.SetConfig(ctx, tx, "https://example.test/index.json", dir, "")
	}); err != nil {
		t.Fatalf("set config: %v", err)
	}
	t.Cleanup(func() {
		_ = pool.InTx(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
			return store.SetConfig(ctx, tx, "", "", "")
		})
	})

	if _, err := store.Install(context.Background(), index,
		marketplace.InstallRequest{Name: "fakegateway"}, nil); err == nil {
		t.Fatal("an install with a mismatched digest succeeded")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read staging dir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("a refused install staged %d files", len(entries))
	}
}

// The index is cached, so the marketplace screen does not fetch on every load.
func TestIndexIsCached(t *testing.T) {
	pool := storetest.DB(t)
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(indexJSON("https://example.test/a", strings.Repeat("a", 64))))
	}))
	t.Cleanup(srv.Close)

	store := marketplace.New(pool)
	if _, err := store.LoadIndex(context.Background(), srv.URL, false, nil); err != nil {
		t.Fatalf("first load: %v", err)
	}
	second, err := store.LoadIndex(context.Background(), srv.URL, false, nil)
	if err != nil {
		t.Fatalf("second load: %v", err)
	}
	if !second.Cached {
		t.Fatal("the second load did not come from the cache")
	}
	if len(second.Index.Extensions) != 1 {
		t.Fatalf("cached index has %d extensions, want 1", len(second.Index.Extensions))
	}
	if hits.Load() != 1 {
		t.Fatalf("the index server was asked %d times, want 1", hits.Load())
	}

	// A forced refresh is the only path that ignores the cache.
	if _, err := store.LoadIndex(context.Background(), srv.URL, true, nil); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if hits.Load() != 2 {
		t.Fatalf("a refresh did not ask the index server (%d hits)", hits.Load())
	}
}

// A failed fetch is reported as a failure rather than as an empty marketplace, so the screen
// can say "the index could not be read" instead of "nothing is published".
func TestFailedIndexFetchIsReported(t *testing.T) {
	pool := storetest.DB(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)

	store := marketplace.New(pool)
	loaded, err := store.LoadIndex(context.Background(), srv.URL, false, nil)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if loaded.FailureCode == "" {
		t.Fatal("a failed fetch reported no failure code")
	}
	if len(loaded.Index.Extensions) != 0 {
		t.Fatal("a failed fetch produced extensions")
	}
}

// asVerify is errors.As for the marketplace's VerifyError, kept local so the test file does
// not import errors for one call.
func asVerify(err error, target **marketplace.VerifyError) bool {
	for err != nil {
		if v, ok := err.(*marketplace.VerifyError); ok {
			*target = v
			return true
		}
		unwrapper, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = unwrapper.Unwrap()
	}
	return false
}
