package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

// Unlinking a sign-in provider.
//
// The link is the route in, so removing one is a security action, not a preference: an identity
// that is no longer controlled by the account holder must be detachable by them. The route
// existed and no screen called it, which left "link from your security page" as a one-way door.
//
// The two properties under test are the ones the SQL provides rather than a handler:
// ownership is in the WHERE clause, so another person's link is a 404 that is byte-identical to
// one that never existed, and deleting it removes exactly one row.

// linkFixture is one `social_identities` row.
type linkFixture struct {
	userID string
	linkID string
}

// newLinkedProvider creates a provider and two accounts, each linked to it.
func newLinkedProvider(t *testing.T, env *oauthEnv) (alice, bob linkFixture) {
	t.Helper()
	ctx := context.Background()

	env.createUser("admin@example.test", "correct horse battery", "administrator")
	adminCookie, adminCSRF := env.login("admin@example.test", "correct horse battery")

	provider := `{"slug":"github","name":"GitHub","issuer_url":"https://github.example.test",
		"authorization_endpoint":"https://github.example.test/authorize",
		"token_endpoint":"https://github.example.test/token",
		"userinfo_endpoint":"https://github.example.test/userinfo",
		"client_id":"id","client_secret":"secret","enabled":true}`
	if rec := env.postJSON(http.MethodPost, "/api/v1/staff/oauth-providers", provider,
		adminCookie, adminCSRF); rec.Code != http.StatusCreated {
		t.Fatalf("create provider: status %d, body %s", rec.Code, rec.Body)
	}

	var providerID string
	if err := env.pool.QueryRow(ctx,
		`SELECT id FROM social_providers WHERE slug = 'github'`).Scan(&providerID); err != nil {
		t.Fatalf("read provider: %v", err)
	}

	alice.userID = env.createUser("alice@example.test", "correct horse battery", "")
	bob.userID = env.createUser("bob@example.test", "correct horse battery", "")

	for _, link := range []*linkFixture{&alice, &bob} {
		subject := "subject-" + link.userID[:8]
		if err := env.pool.QueryRow(ctx, `
			INSERT INTO social_identities (provider_id, user_id, subject, email)
			VALUES ($1, $2, $3, $4) RETURNING id`,
			providerID, link.userID, subject, subject+"@example.test").Scan(&link.linkID); err != nil {
			t.Fatalf("create link: %v", err)
		}
	}
	return alice, bob
}

// countLinks counts every link in the installation, which is what "exactly one row" means.
func countLinks(t *testing.T, env *oauthEnv) int {
	t.Helper()
	var n int
	if err := env.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM social_identities`).Scan(&n); err != nil {
		t.Fatalf("count links: %v", err)
	}
	return n
}

// A person can unlink a provider from their own account, and only their own.
func TestSocialUnlinkRemovesOnlyTheCallersLink(t *testing.T) {
	env := newOAuthEnv(t)
	alice, bob := newLinkedProvider(t, env)
	aliceCookie, aliceCSRF := env.login("alice@example.test", "correct horse battery")

	// The list the screen renders: only the caller's own identities.
	rec := env.get("/api/v1/portal/social-links", aliceCookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("list links: %d %s", rec.Code, rec.Body)
	}
	var listed struct {
		Data []struct {
			ID           string `json:"id"`
			ProviderSlug string `json:"provider_slug"`
			ProviderName string `json:"provider_name"`
			Email        string `json:"email"`
			Subject      string `json:"subject"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode links: %v", err)
	}
	if len(listed.Data) != 1 {
		t.Fatalf("links = %+v, want only the caller's", listed.Data)
	}
	if listed.Data[0].ID != alice.linkID || listed.Data[0].ProviderName != "GitHub" {
		t.Fatalf("link = %+v, want alice's GitHub identity", listed.Data[0])
	}

	// Somebody else's link is indistinguishable from one that does not exist.
	rec = env.postJSON(http.MethodDelete, "/api/v1/portal/social-links/"+bob.linkID, "",
		aliceCookie, aliceCSRF)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unlinking another account's link: status %d, want 404; body %s", rec.Code, rec.Body)
	}
	if got := countLinks(t, env); got != 2 {
		t.Fatalf("links = %d after a refused unlink, want 2", got)
	}

	// A malformed id is the same 404, before the database is touched.
	rec = env.postJSON(http.MethodDelete, "/api/v1/portal/social-links/not-a-uuid", "",
		aliceCookie, aliceCSRF)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unlinking a malformed id: status %d, want 404", rec.Code)
	}

	// Her own link goes, and nothing else does.
	rec = env.postJSON(http.MethodDelete, "/api/v1/portal/social-links/"+alice.linkID, "",
		aliceCookie, aliceCSRF)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("unlinking own link: status %d, want 204; body %s", rec.Code, rec.Body)
	}
	if got := countLinks(t, env); got != 1 {
		t.Fatalf("links = %d after unlinking one, want 1", got)
	}

	rec = env.get("/api/v1/portal/social-links", aliceCookie)
	_ = json.Unmarshal(rec.Body.Bytes(), &listed)
	if len(listed.Data) != 0 {
		t.Fatalf("links after unlinking = %+v, want none", listed.Data)
	}

	// Unlinking the same link twice is a 404 the second time, not a second delete.
	rec = env.postJSON(http.MethodDelete, "/api/v1/portal/social-links/"+alice.linkID, "",
		aliceCookie, aliceCSRF)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unlinking twice: status %d, want 404", rec.Code)
	}
}

// Unlinking requires a session: an anonymous delete is refused before the handler runs, and
// with it the CSRF token a mutation requires.
func TestSocialUnlinkRequiresASession(t *testing.T) {
	env := newOAuthEnv(t)
	alice, _ := newLinkedProvider(t, env)

	rec := env.postJSON(http.MethodDelete, "/api/v1/portal/social-links/"+alice.linkID, "",
		nil, "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous unlink: status %d, want 401; body %s", rec.Code, rec.Body)
	}
	if got := countLinks(t, env); got != 2 {
		t.Fatalf("links = %d after an anonymous unlink, want 2", got)
	}
}
