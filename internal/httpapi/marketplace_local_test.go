package httpapi_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/catalyst/billing/internal/marketplace"
)

// The local marketplace routes (ADM-12, OPS-14).
//
// The domain rules are tested in internal/marketplace; what these assert is the *wire*: the
// artifact arrives as bytes with its metadata in headers, the enable/disable switch changes the
// row the screen reads, and the scaffold is a downloadable zip. The failure they exist to prevent
// is a route that returns 200 and stores nothing.

// patchJSON performs a PATCH with the CSRF token and an idempotency key.
func patchJSON(t *testing.T, env *staffEnv, path, key string, body any) *httptest.ResponseRecorder {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	req := httptest.NewRequest(http.MethodPatch, path, bytes.NewReader(encoded))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(env.admin)
	req.Header.Set("X-CSRF-Token", env.adminCSRF)
	req.Header.Set("Idempotency-Key", key)
	rec := httptest.NewRecorder()
	env.srv.Handler().ServeHTTP(rec, req)
	return rec
}

// uploadArtifact posts a binary artifact with its metadata headers.
func uploadArtifact(t *testing.T, env *staffEnv, name, version, kind string, body []byte,
	headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/staff/marketplace/upload", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("X-Extension-Name", name)
	req.Header.Set("X-Extension-Version", version)
	req.Header.Set("X-Extension-Kind", kind)
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	req.AddCookie(env.admin)
	req.Header.Set("X-CSRF-Token", env.adminCSRF)
	req.Header.Set("Idempotency-Key", "upload-"+name+"-"+version)
	rec := httptest.NewRecorder()
	env.srv.Handler().ServeHTTP(rec, req)
	return rec
}

// An uploaded artifact is stored, appears in the installed list, and can be switched off.
func TestMarketplaceUploadAndToggle(t *testing.T) {
	env := newStaffEnv(t)
	body := []byte("#!/bin/sh\necho an extension\n")
	sum := sha256.Sum256(body)

	rec := uploadArtifact(t, env, "uploadedgw", "1.0.0", "gateway", body,
		map[string]string{"X-Extension-Publisher": "Internal"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("upload: status %d; body %s", rec.Code, rec.Body)
	}
	var result struct {
		Name    string `json:"name"`
		Version string `json:"version"`
		SHA256  string `json:"sha256"`
		Status  string `json:"status"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode: %v\n%s", err, rec.Body)
	}
	if result.SHA256 != hex.EncodeToString(sum[:]) || result.Status != "verified" {
		t.Fatalf("upload result = %+v, want the verified digest", result)
	}

	// The screen reads it back from the state endpoint, enabled.
	state := env.get(t, "/api/v1/staff/marketplace", env.admin)
	if state.Code != http.StatusOK {
		t.Fatalf("state: %d %s", state.Code, state.Body)
	}
	var installed struct {
		Installed []struct {
			Name    string `json:"name"`
			Enabled bool   `json:"enabled"`
		} `json:"installed"`
	}
	if err := json.Unmarshal(state.Body.Bytes(), &installed); err != nil {
		t.Fatalf("decode state: %v", err)
	}
	if len(installed.Installed) != 1 || !installed.Installed[0].Enabled {
		t.Fatalf("installed = %+v, want one enabled row", installed.Installed)
	}

	// Disabling is a PATCH on the row, and the audit row names the operator's action.
	disable := patchJSON(t, env, "/api/v1/staff/marketplace/installed/uploadedgw/1.0.0",
		"disable-uploadedgw", map[string]any{"enabled": false})
	if disable.Code != http.StatusOK {
		t.Fatalf("disable: status %d; body %s", disable.Code, disable.Body)
	}
	var row struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.Unmarshal(disable.Body.Bytes(), &row); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if row.Enabled {
		t.Fatal("the response says enabled after disabling")
	}
	if err := env.pool.QueryRow(context.Background(),
		`SELECT enabled FROM installed_extensions WHERE name = 'uploadedgw'`).Scan(&row.Enabled); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if row.Enabled {
		t.Fatal("the stored row is still enabled")
	}
	var action string
	if err := env.pool.QueryRow(context.Background(), `
		SELECT action FROM audit_events WHERE action = 'extension.set_enabled'
		ORDER BY created_at DESC LIMIT 1`).Scan(&action); err != nil {
		t.Fatalf("read audit row: %v", err)
	}

	// A PATCH without the field is refused rather than read as "false".
	empty := patchJSON(t, env, "/api/v1/staff/marketplace/installed/uploadedgw/1.0.0",
		"disable-empty", map[string]any{})
	if empty.Code != http.StatusBadRequest {
		t.Fatalf("empty patch: status %d, want 400", empty.Code)
	}
}

// An upload without metadata is a 400 with the domain's code, not a 500.
func TestMarketplaceUploadRequiresMetadata(t *testing.T) {
	env := newStaffEnv(t)
	rec := uploadArtifact(t, env, "", "", "gateway", []byte("bytes"), nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body %s", rec.Code, rec.Body)
	}
	var problem struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &problem); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if problem.Code != marketplace.CodeUploadInvalid {
		t.Fatalf("code = %q, want %q", problem.Code, marketplace.CodeUploadInvalid)
	}
}

// The scaffold downloads a zip whose manifest names the requested extension.
func TestMarketplaceScaffoldDownload(t *testing.T) {
	env := newStaffEnv(t)
	rec := env.get(t, "/api/v1/staff/marketplace/scaffold?name=acmegw&version=0.2.0&kind=gateway", env.admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body %s", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/zip" {
		t.Fatalf("content-type = %q, want application/zip", got)
	}
	if got := rec.Header().Get("Content-Disposition"); !strings.Contains(got, "acmegw-0.2.0-scaffold.zip") {
		t.Fatalf("content-disposition = %q", got)
	}
	if rec.Body.Len() == 0 || !bytes.HasPrefix(rec.Body.Bytes(), []byte("PK")) {
		t.Fatalf("the body is not a zip (%d bytes)", rec.Body.Len())
	}

	// A name that cannot be a directory is refused with the domain's code.
	bad := env.get(t, "/api/v1/staff/marketplace/scaffold?name=bad%2Fname&kind=gateway", env.admin)
	if bad.Code != http.StatusUnprocessableEntity {
		t.Fatalf("bad name: status %d, want 422", bad.Code)
	}
	var problem struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(bad.Body.Bytes(), &problem); err != nil {
		t.Fatal(err)
	}
	if problem.Code != marketplace.CodeScaffoldInvalid {
		t.Fatalf("code = %q, want %q", problem.Code, marketplace.CodeScaffoldInvalid)
	}
}
