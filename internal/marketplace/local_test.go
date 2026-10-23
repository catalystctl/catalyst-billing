package marketplace_test

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/catalyst/billing/internal/audit"
	"github.com/catalyst/billing/internal/marketplace"
	"github.com/catalyst/billing/internal/store/storetest"
	"github.com/jackc/pgx/v5"
)

// The local half of the marketplace client: upload, enable/disable and the scaffold (ADM-12,
// OPS-14).
//
// The properties asserted here are the ones that make an offline install as trustworthy as an
// indexed one: the digest is computed from the bytes that were stored, the same version cannot be
// replaced with different bytes, an enable/disable change is audited, and the scaffold is a
// runnable stub whose manifest names what the operator asked for.

// auditSpy records the audit rows a domain service writes.
//
// The real writer is internal/audit and it needs a request; what this test asserts is that the
// *row is offered inside the transaction*, which is the property an operator relies on when they
// ask who installed an extension.
type auditSpy struct {
	actions  []string
	changes  []map[string]any
	failWith error
}

func (s *auditSpy) RecordAction(_ context.Context, _ pgx.Tx, action, _, _ string,
	changes map[string]any) error {
	if s.failWith != nil {
		return s.failWith
	}
	s.actions = append(s.actions, action)
	s.changes = append(s.changes, changes)
	return nil
}

var _ audit.Auditor = (*auditSpy)(nil)

// uploadBody is a fake extension artifact.
var uploadBody = []byte("#!/bin/sh\necho uploaded extension\n")

// The upload stores the row, the bytes and the audit row, and it records the digest it computed
// rather than one it was told.
func TestUploadStoresTheArtifactAndItsDigest(t *testing.T) {
	pool := storetest.DB(t)
	ctx := context.Background()
	spy := &auditSpy{}

	result, err := marketplace.New(pool).Upload(ctx, marketplace.UploadRequest{
		Name:        "offlinegw",
		Version:     "2.0.0",
		Publisher:   "Internal",
		Description: "A gateway for a system with no network.",
		Kind:        marketplace.KindGateway,
		Body:        uploadBody,
		ActorID:     "",
		Auditor:     spy,
	})
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	if result.SHA256 != digestOf(uploadBody) {
		t.Fatalf("digest = %s, want %s", result.SHA256, digestOf(uploadBody))
	}
	if result.Status != "verified" {
		t.Fatalf("status = %q, want verified", result.Status)
	}
	if len(spy.actions) != 1 || spy.actions[0] != "extension.upload" {
		t.Fatalf("audit actions = %v, want extension.upload", spy.actions)
	}

	// The bytes are the bytes that were verified: the artifact read path is how an operator
	// deploys exactly what was checked.
	stored, digest, err := marketplace.New(pool).Artifact(ctx, "offlinegw", "2.0.0")
	if err != nil {
		t.Fatalf("read artifact: %v", err)
	}
	if !bytes.Equal(stored, uploadBody) || digest != result.SHA256 {
		t.Fatalf("stored artifact does not match the uploaded bytes (digest %s)", digest)
	}

	// And it is listed as installed, enabled by default: an artifact that was installed is one
	// somebody intends to use.
	installed, err := marketplace.New(pool).ListInstalled(ctx)
	if err != nil {
		t.Fatalf("list installed: %v", err)
	}
	if len(installed) != 1 || installed[0].Name != "offlinegw" || !installed[0].Enabled {
		t.Fatalf("installed = %+v, want one enabled offlinegw", installed)
	}
}

// The same version with different bytes is refused, exactly as the indexed install refuses it.
func TestUploadRefusesToReplaceAVerifiedVersion(t *testing.T) {
	pool := storetest.DB(t)
	ctx := context.Background()
	store := marketplace.New(pool)
	if _, err := store.Upload(ctx, marketplace.UploadRequest{
		Name: "offlinegw", Version: "2.0.0", Kind: marketplace.KindGateway, Body: uploadBody,
	}); err != nil {
		t.Fatalf("first upload: %v", err)
	}

	_, err := store.Upload(ctx, marketplace.UploadRequest{
		Name: "offlinegw", Version: "2.0.0", Kind: marketplace.KindGateway,
		Body: []byte("#!/bin/sh\necho different bytes\n"),
	})
	if err == nil {
		t.Fatal("a second upload replaced a verified version's bytes")
	}
	var verifyErr *marketplace.VerifyError
	if !asVerifyErr(err, &verifyErr) || verifyErr.Code != marketplace.CodeVersionConflict {
		t.Fatalf("error = %v, want marketplace_version_conflict", err)
	}

	// The original bytes are still the ones on record.
	stored, _, err := store.Artifact(ctx, "offlinegw", "2.0.0")
	if err != nil {
		t.Fatalf("read artifact: %v", err)
	}
	if !bytes.Equal(stored, uploadBody) {
		t.Fatal("the refused upload changed the stored artifact")
	}
}

// A supplied digest that does not match the bytes refuses the upload and stores nothing.
func TestUploadRefusesAMismatchedDigest(t *testing.T) {
	pool := storetest.DB(t)
	ctx := context.Background()

	_, err := marketplace.New(pool).Upload(ctx, marketplace.UploadRequest{
		Name: "offlinegw", Version: "3.0.0", Kind: marketplace.KindGateway, Body: uploadBody,
		PinnedSHA256: digestOf([]byte("something else")),
	})
	if err == nil {
		t.Fatal("an upload whose digest did not match its bytes was accepted")
	}
	var verifyErr *marketplace.VerifyError
	if !asVerifyErr(err, &verifyErr) || verifyErr.Code != marketplace.CodeDigestMismatch {
		t.Fatalf("error = %v, want marketplace_digest_mismatch", err)
	}
	if installed, _ := marketplace.New(pool).ListInstalled(ctx); len(installed) != 0 {
		t.Fatalf("a refused upload left %d row(s) behind", len(installed))
	}
}

// Enable and disable change the stored row and write one audit row each.
func TestSetEnabledAuditsTheSwitch(t *testing.T) {
	pool := storetest.DB(t)
	ctx := context.Background()
	store := marketplace.New(pool)
	if _, err := store.Upload(ctx, marketplace.UploadRequest{
		Name: "offlinegw", Version: "2.0.0", Kind: marketplace.KindGateway, Body: uploadBody,
	}); err != nil {
		t.Fatalf("upload: %v", err)
	}

	spy := &auditSpy{}
	row, err := store.SetEnabled(ctx, "offlinegw", "2.0.0", false, "", spy)
	if err != nil {
		t.Fatalf("disable: %v", err)
	}
	if row.Enabled {
		t.Fatal("the returned row says enabled after a disable")
	}
	if len(spy.actions) != 1 || spy.actions[0] != "extension.set_enabled" {
		t.Fatalf("audit actions = %v, want extension.set_enabled", spy.actions)
	}
	if spy.changes[0]["enabled"] != false {
		t.Fatalf("audit changes = %v, want enabled=false", spy.changes[0])
	}

	// The screen reads what was stored.
	installed, err := store.ListInstalled(ctx)
	if err != nil {
		t.Fatalf("list installed: %v", err)
	}
	if len(installed) != 1 || installed[0].Enabled {
		t.Fatalf("installed = %+v, want one disabled row", installed)
	}

	// An unknown row is a domain error with a stable code, not a silent no-op.
	_, err = store.SetEnabled(ctx, "offlinegw", "9.9.9", true, "", spy)
	if err == nil {
		t.Fatal("enabling a version that is not installed was accepted")
	}
	var verifyErr *marketplace.VerifyError
	if !asVerifyErr(err, &verifyErr) || verifyErr.Code != marketplace.CodeExtensionNotFound {
		t.Fatalf("error = %v, want marketplace_extension_not_found", err)
	}
}

// The scaffold is a zip whose files name the operator's extension, and it is byte-stable.
func TestScaffoldIsARunnableSkeleton(t *testing.T) {
	body, filename, err := marketplace.Scaffold(marketplace.ScaffoldRequest{
		Name: "internalgw", Version: "0.1.0", Kind: marketplace.KindGateway, Publisher: "Ops",
	})
	if err != nil {
		t.Fatalf("scaffold: %v", err)
	}
	if filename != "internalgw-0.1.0-scaffold.zip" {
		t.Fatalf("filename = %q", filename)
	}

	reader, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatalf("the scaffold is not a zip: %v", err)
	}
	files := map[string]string{}
	for _, file := range reader.File {
		rc, err := file.Open()
		if err != nil {
			t.Fatalf("open %s: %v", file.Name, err)
		}
		var buf bytes.Buffer
		if _, err := buf.ReadFrom(rc); err != nil {
			t.Fatalf("read %s: %v", file.Name, err)
		}
		_ = rc.Close()
		files[file.Name] = buf.String()
	}

	for _, want := range []string{
		"internalgw/manifest.json", "internalgw/main.go", "internalgw/go.mod", "internalgw/README.md",
	} {
		if _, ok := files[want]; !ok {
			t.Fatalf("the scaffold has no %s", want)
		}
	}
	manifest := files["internalgw/manifest.json"]
	for _, want := range []string{`"name": "internalgw"`, `"kind": "gateway"`, "billing.ext/1", "gateway.create_checkout"} {
		if !bytes.Contains([]byte(manifest), []byte(want)) {
			t.Fatalf("manifest does not contain %q:\n%s", want, manifest)
		}
	}
	// The stub must be a program that answers the handshake, or the scaffold is a lie.
	main := files["internalgw/main.go"]
	for _, want := range []string{"func main()", `case "initialize":`, `case "health":`} {
		if !bytes.Contains([]byte(main), []byte(want)) {
			t.Fatalf("the stub does not contain %q", want)
		}
	}

	// The same request twice is the same bytes: a scaffold with the current time in its headers
	// would make "is this the file I generated?" unanswerable.
	again, _, err := marketplace.Scaffold(marketplace.ScaffoldRequest{
		Name: "internalgw", Version: "0.1.0", Kind: marketplace.KindGateway, Publisher: "Ops",
	})
	if err != nil {
		t.Fatalf("scaffold again: %v", err)
	}
	if !bytes.Equal(body, again) {
		t.Fatal("the same scaffold request produced different bytes")
	}
}

// A scaffold for a name that cannot become a directory is refused.
func TestScaffoldRefusesBadNames(t *testing.T) {
	for _, req := range []marketplace.ScaffoldRequest{
		{Name: "../escape", Kind: marketplace.KindGateway},
		{Name: "ok", Version: "not-a-version", Kind: marketplace.KindGateway},
		{Name: "ok", Kind: "spaceship"},
	} {
		if _, _, err := marketplace.Scaffold(req); err == nil {
			t.Fatalf("scaffold %+v was accepted", req)
		}
	}
}

// asVerifyErr extracts the domain refusal, which is the code a client branches on.
func asVerifyErr(err error, target **marketplace.VerifyError) bool {
	return errors.As(err, target)
}
