package httpapi_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/catalyst/billing/internal/authn"
)

// Staff user administration over HTTP.
//
// These run against the real router and a real database, because the properties under test
// are about the composition: the permission declared on each route, the problem+json
// envelope, the idempotency key on every mutation, and the refusals that must reach the
// screen as codes rather than as a generic 500.

// userKeySeq makes each mutation's idempotency key unique.
var userKeySeq atomic.Int64

// doUser performs a request with a session cookie and CSRF token.
func (e *authEnv) doUser(t *testing.T, method, path string, cookie *http.Cookie, csrf string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		reader = bytes.NewReader(raw)
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	if cookie != nil {
		req.AddCookie(cookie)
	}
	if csrf != "" {
		req.Header.Set("X-CSRF-Token", csrf)
	}
	if method != http.MethodGet {
		req.Header.Set("Idempotency-Key", fmt.Sprintf("user-admin-test-%d", userKeySeq.Add(1)))
	}
	rec := httptest.NewRecorder()
	e.srv.Handler().ServeHTTP(rec, req)
	return rec
}

// userBody is the account shape the API returns.
type userBody struct {
	ID            string  `json:"id"`
	Email         string  `json:"email"`
	DisplayName   string  `json:"display_name"`
	Status        string  `json:"status"`
	EmailVerified bool    `json:"email_verified"`
	Role          string  `json:"role"`
	IsStaff       bool    `json:"is_staff"`
	LastSignInAt  *string `json:"last_sign_in_at"`
}

// decodeUser decodes one account.
func decodeUser(t *testing.T, rec *httptest.ResponseRecorder) userBody {
	t.Helper()
	if rec.Code != http.StatusOK && rec.Code != http.StatusCreated {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body)
	}
	var out userBody
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode account: %v; body %s", err, rec.Body)
	}
	return out
}

// An administrator creates a support agent through the API, and that agent can sign in and
// work. This is the gap the whole package exists to close: onboarding without a shell.
func TestCreateStaffUserOverHTTPAndSignIn(t *testing.T) {
	env := newAuthEnv(t)
	env.createUser(t, "root@example.test", "root password 1", "administrator")
	cookie, csrf := env.login(t, "root@example.test", "root password 1")

	rec := env.doUser(t, http.MethodPost, "/api/v1/staff/users", cookie, csrf, map[string]any{
		"email":        "new-agent@example.test",
		"display_name": "New Agent",
		"password":     "a long enough password",
		"role":         "support",
	})
	created := decodeUser(t, rec)
	if created.Role != "support" || !created.IsStaff {
		t.Fatalf("created account = %+v, want a support agent", created)
	}
	if created.Status != "active" {
		t.Fatalf("status = %q, want active", created.Status)
	}

	// The agent signs in with the password the operator chose.
	agentCookie, _ := env.login(t, "new-agent@example.test", "a long enough password")

	// …and can do their job, which needs ticket:write:all, while staff:manage is still
	// refused: creating an agent must not create an administrator.
	me := env.get(t, "/api/v1/auth/me", agentCookie)
	if me.Code != http.StatusOK {
		t.Fatalf("agent /auth/me: status %d, body %s", me.Code, me.Body)
	}
	if rec := env.get(t, "/api/v1/staff/users", agentCookie); rec.Code != http.StatusForbidden {
		t.Fatalf("a support agent listed users: status %d, want 403", rec.Code)
	}
}

// Creating a customer is the same endpoint with no role: a support agent onboarding somebody
// who rang up is the same screen.
func TestCreateCustomerUserOverHTTP(t *testing.T) {
	env := newAuthEnv(t)
	env.createUser(t, "root@example.test", "root password 1", "administrator")
	cookie, csrf := env.login(t, "root@example.test", "root password 1")

	rec := env.doUser(t, http.MethodPost, "/api/v1/staff/users", cookie, csrf, map[string]any{
		"email":        "customer@example.test",
		"display_name": "Phone Customer",
		"password":     "a long enough password",
	})
	created := decodeUser(t, rec)
	if created.IsStaff || created.Role != "" {
		t.Fatalf("created account = %+v, want a customer with no staff role", created)
	}
	env.login(t, "customer@example.test", "a long enough password")
}

// The refusals the screen depends on, each with its own stable code.
func TestUserAdministrationRefusals(t *testing.T) {
	env := newAuthEnv(t)
	adminID := env.createUser(t, "root@example.test", "root password 1", "administrator")
	cookie, csrf := env.login(t, "root@example.test", "root password 1")

	t.Run("duplicate email", func(t *testing.T) {
		env.createUser(t, "taken@example.test", "a long enough password", "")
		rec := env.doUser(t, http.MethodPost, "/api/v1/staff/users", cookie, csrf, map[string]any{
			"email":        "TAKEN@example.test",
			"display_name": "Second",
			"password":     "a long enough password",
		})
		if rec.Code != http.StatusConflict {
			t.Fatalf("status %d, want 409; body %s", rec.Code, rec.Body)
		}
		if code := decodeProblemCode(t, rec); code != "email_taken" {
			t.Fatalf("code = %q, want email_taken", code)
		}
	})

	t.Run("weak password", func(t *testing.T) {
		rec := env.doUser(t, http.MethodPost, "/api/v1/staff/users", cookie, csrf, map[string]any{
			"email":        "weak@example.test",
			"display_name": "Weak",
			"password":     "short",
		})
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status %d, want 422; body %s", rec.Code, rec.Body)
		}
		if code := decodeProblemCode(t, rec); code != "weak_password" {
			t.Fatalf("code = %q, want weak_password", code)
		}
	})

	t.Run("unknown role", func(t *testing.T) {
		rec := env.doUser(t, http.MethodPost, "/api/v1/staff/users", cookie, csrf, map[string]any{
			"email":        "norole@example.test",
			"display_name": "No Role",
			"password":     "a long enough password",
			"role":         "superuser",
		})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status %d, want 400; body %s", rec.Code, rec.Body)
		}
		if code := decodeProblemCode(t, rec); code != "unknown_role" {
			t.Fatalf("code = %q, want unknown_role", code)
		}
	})

	t.Run("disabling yourself", func(t *testing.T) {
		rec := env.doUser(t, http.MethodPatch, "/api/v1/staff/users/"+adminID, cookie, csrf,
			map[string]any{"status": "disabled"})
		if rec.Code != http.StatusConflict {
			t.Fatalf("status %d, want 409; body %s", rec.Code, rec.Body)
		}
		if code := decodeProblemCode(t, rec); code != "self_lockout" {
			t.Fatalf("code = %q, want self_lockout", code)
		}
	})

	t.Run("demoting the last administrator", func(t *testing.T) {
		rec := env.doUser(t, http.MethodPatch, "/api/v1/staff/users/"+adminID, cookie, csrf,
			map[string]any{"role": "support"})
		if rec.Code != http.StatusConflict {
			t.Fatalf("status %d, want 409; body %s", rec.Code, rec.Body)
		}
		if code := decodeProblemCode(t, rec); code != "last_administrator" {
			t.Fatalf("code = %q, want last_administrator", code)
		}
	})

	t.Run("deleting an account with history", func(t *testing.T) {
		target := env.createUser(t, "historic@example.test", "a long enough password", "support")
		// One audit row naming them as the actor is enough: the audit trail is append-only.
		if _, err := env.pool.Exec(t.Context(), `
			INSERT INTO audit_events (actor_id, actor_kind, action, target_type, outcome)
			VALUES ($1, 'user', 'invoice.void', 'invoice', 'success')`, target); err != nil {
			t.Fatalf("write audit row: %v", err)
		}
		rec := env.doUser(t, http.MethodDelete, "/api/v1/staff/users/"+target, cookie, csrf, nil)
		if rec.Code != http.StatusConflict {
			t.Fatalf("status %d, want 409; body %s", rec.Code, rec.Body)
		}
		if code := decodeProblemCode(t, rec); code != "user_has_history" {
			t.Fatalf("code = %q, want user_has_history", code)
		}
	})
}

// The weak-password refusal must be the *same* code registration uses. A password rule that
// differed by screen would be a rule nobody can state.
func TestWeakPasswordCodeMatchesRegistration(t *testing.T) {
	env := newAuthEnv(t)
	env.createUser(t, "root@example.test", "root password 1", "administrator")
	cookie, csrf := env.login(t, "root@example.test", "root password 1")

	admin := env.doUser(t, http.MethodPost, "/api/v1/staff/users", cookie, csrf, map[string]any{
		"email":        "x@example.test",
		"display_name": "X",
		"password":     "short",
	})
	adminCode := decodeProblemCode(t, admin)

	body, _ := json.Marshal(map[string]any{
		"email": "y@example.test", "password": "short", "display_name": "Y", "accept_tos": true,
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/register", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	env.srv.Handler().ServeHTTP(rec, req)
	registerCode := decodeProblemCode(t, rec)

	if adminCode != registerCode {
		t.Fatalf("the admin screen refuses a weak password with %q and registration with %q",
			adminCode, registerCode)
	}
	if adminCode != "weak_password" {
		t.Fatalf("code = %q, want weak_password", adminCode)
	}
}

// The list shows staff and customers, with the fields the screen renders.
func TestUserListOverHTTP(t *testing.T) {
	env := newAuthEnv(t)
	env.createUser(t, "root@example.test", "root password 1", "administrator")
	env.createUser(t, "agent@example.test", "a long enough password", "support")
	env.createUser(t, "customer@example.test", "a long enough password", "")
	cookie, _ := env.login(t, "root@example.test", "root password 1")

	rec := env.get(t, "/api/v1/staff/users", cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body)
	}
	var page struct {
		Data []userBody `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(page.Data) != 3 {
		t.Fatalf("listed %d accounts, want 3", len(page.Data))
	}

	staff := env.get(t, "/api/v1/staff/users?staff=true", cookie)
	if err := json.Unmarshal(staff.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode staff: %v", err)
	}
	if len(page.Data) != 2 {
		t.Fatalf("staff list = %d accounts, want 2", len(page.Data))
	}

	search := env.get(t, "/api/v1/staff/users?q=agent", cookie)
	if err := json.Unmarshal(search.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode search: %v", err)
	}
	if len(page.Data) != 1 || page.Data[0].Email != "agent@example.test" {
		t.Fatalf("search returned %+v", page.Data)
	}
	if page.Data[0].Role != "support" {
		t.Fatalf("role = %q, want support", page.Data[0].Role)
	}

	// A one-character search is refused rather than scanned.
	short := env.get(t, "/api/v1/staff/users?q=a", cookie)
	if short.Code != http.StatusBadRequest {
		t.Fatalf("one-character search: status %d, want 400", short.Code)
	}
}

// Setting a password never returns one, and it ends the target's sessions.
func TestSetPasswordOverHTTP(t *testing.T) {
	env := newAuthEnv(t)
	env.createUser(t, "root@example.test", "root password 1", "administrator")
	targetID := env.createUser(t, "target@example.test", "a long enough password", "support")
	cookie, csrf := env.login(t, "root@example.test", "root password 1")
	targetCookie, _ := env.login(t, "target@example.test", "a long enough password")

	rec := env.doUser(t, http.MethodPost, "/api/v1/staff/users/"+targetID+"/password", cookie, csrf,
		map[string]any{"password": "another long password"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body)
	}
	if bytes.Contains(bytes.ToLower(rec.Body.Bytes()), []byte("another long password")) {
		t.Fatalf("the response echoed the password: %s", rec.Body)
	}
	var body struct {
		Status          string `json:"status"`
		SessionsRevoked int    `json:"sessions_revoked"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Status != "password_set" || body.SessionsRevoked < 1 {
		t.Fatalf("body = %+v, want password_set with at least one session revoked", body)
	}

	// The target's session is gone and the new password works.
	if me := env.get(t, "/api/v1/auth/me", targetCookie); me.Code != http.StatusUnauthorized {
		t.Fatalf("the old session still works: status %d", me.Code)
	}
	env.login(t, "target@example.test", "another long password")
}

// Role CRUD over HTTP: create, rename, replace permissions, delete — and the system roles
// refuse every write.
func TestRoleAdministrationOverHTTP(t *testing.T) {
	env := newAuthEnv(t)
	env.createUser(t, "root@example.test", "root password 1", "administrator")
	cookie, csrf := env.login(t, "root@example.test", "root password 1")

	create := env.doUser(t, http.MethodPost, "/api/v1/staff/roles", cookie, csrf, map[string]any{
		"name":        "support-lead",
		"description": "First line plus holds",
		"permissions": []string{"customer:read:all", "service:hold"},
	})
	if create.Code != http.StatusCreated {
		t.Fatalf("create role: status %d, body %s", create.Code, create.Body)
	}
	var role struct {
		ID          string   `json:"id"`
		Name        string   `json:"name"`
		IsSystem    bool     `json:"is_system"`
		Permissions []string `json:"permissions"`
		Holders     int      `json:"holders"`
	}
	if err := json.Unmarshal(create.Body.Bytes(), &role); err != nil {
		t.Fatalf("decode role: %v", err)
	}
	if role.Name != "support-lead" || len(role.Permissions) != 2 || role.IsSystem {
		t.Fatalf("created role = %+v", role)
	}

	rename := env.doUser(t, http.MethodPatch, "/api/v1/staff/roles/"+role.ID, cookie, csrf,
		map[string]any{"name": "support-leads", "description": "Renamed"})
	if rename.Code != http.StatusOK {
		t.Fatalf("rename: status %d, body %s", rename.Code, rename.Body)
	}
	if err := json.Unmarshal(rename.Body.Bytes(), &role); err != nil {
		t.Fatalf("decode renamed role: %v", err)
	}
	if role.Name != "support-leads" {
		t.Fatalf("name = %q, want support-leads", role.Name)
	}

	// Assign it to somebody, and check the count the screen shows.
	assigned := env.doUser(t, http.MethodPost, "/api/v1/staff/users", cookie, csrf, map[string]any{
		"email":        "lead@example.test",
		"display_name": "Lead",
		"password":     "a long enough password",
		"role":         "support-leads",
	})
	if assigned.Code != http.StatusCreated {
		t.Fatalf("assign custom role: status %d, body %s", assigned.Code, assigned.Body)
	}

	list := env.get(t, "/api/v1/staff/roles", cookie)
	var roles struct {
		Data []struct {
			ID          string   `json:"id"`
			Name        string   `json:"name"`
			IsSystem    bool     `json:"is_system"`
			Holders     int      `json:"holders"`
			Permissions []string `json:"permissions"`
		} `json:"data"`
	}
	if err := json.Unmarshal(list.Body.Bytes(), &roles); err != nil {
		t.Fatalf("decode roles: %v", err)
	}
	var custom, system *struct {
		ID          string   `json:"id"`
		Name        string   `json:"name"`
		IsSystem    bool     `json:"is_system"`
		Holders     int      `json:"holders"`
		Permissions []string `json:"permissions"`
	}
	for i := range roles.Data {
		switch roles.Data[i].Name {
		case "support-leads":
			custom = &roles.Data[i]
		case "administrator":
			system = &roles.Data[i]
		}
	}
	if custom == nil || custom.Holders != 1 {
		t.Fatalf("the custom role's holder count is wrong: %+v", custom)
	}
	if system == nil || !system.IsSystem || len(system.Permissions) == 0 {
		t.Fatalf("the administrator role is not reported with its permissions: %+v", system)
	}
	if system.Holders != 1 {
		t.Fatalf("administrator holders = %d, want 1", system.Holders)
	}

	// A role somebody holds cannot be deleted: deleting it would silently strip their access.
	inUse := env.doUser(t, http.MethodDelete, "/api/v1/staff/roles/"+role.ID, cookie, csrf, nil)
	if inUse.Code != http.StatusConflict {
		t.Fatalf("delete a held role: status %d, want 409; body %s", inUse.Code, inUse.Body)
	}
	if code := decodeProblemCode(t, inUse); code != "role_in_use" {
		t.Fatalf("code = %q, want role_in_use", code)
	}

	// A system role cannot be renamed, have its permissions replaced, or be deleted.
	for _, systemID := range []string{system.ID} {
		if rec := env.doUser(t, http.MethodPatch, "/api/v1/staff/roles/"+systemID, cookie, csrf,
			map[string]any{"name": "renamed-admin"}); rec.Code != http.StatusConflict {
			t.Fatalf("rename a system role: status %d, want 409; body %s", rec.Code, rec.Body)
		} else if code := decodeProblemCode(t, rec); code != "system_role" {
			t.Fatalf("code = %q, want system_role", code)
		}
		if rec := env.doUser(t, http.MethodPut, "/api/v1/staff/roles/"+systemID+"/permissions", cookie, csrf,
			map[string]any{"permissions": []string{"customer:read:all"}}); rec.Code != http.StatusConflict {
			t.Fatalf("replace a system role's permissions: status %d, want 409; body %s", rec.Code, rec.Body)
		}
		if rec := env.doUser(t, http.MethodDelete, "/api/v1/staff/roles/"+systemID, cookie, csrf, nil); rec.Code != http.StatusConflict {
			t.Fatalf("delete a system role: status %d, want 409; body %s", rec.Code, rec.Body)
		}
	}
}

// A disabled account's session stops working, over HTTP: the cookie that worked a moment ago
// is a 401.
func TestDisabledUserSessionStopsWorkingOverHTTP(t *testing.T) {
	env := newAuthEnv(t)
	env.createUser(t, "root@example.test", "root password 1", "administrator")
	targetID := env.createUser(t, "target@example.test", "a long enough password", "support")
	cookie, csrf := env.login(t, "root@example.test", "root password 1")
	targetCookie, _ := env.login(t, "target@example.test", "a long enough password")

	if me := env.get(t, "/api/v1/auth/me", targetCookie); me.Code != http.StatusOK {
		t.Fatalf("the target's session does not work before the change: %d", me.Code)
	}

	rec := env.doUser(t, http.MethodPatch, "/api/v1/staff/users/"+targetID, cookie, csrf,
		map[string]any{"status": "disabled"})
	if rec.Code != http.StatusOK {
		t.Fatalf("disable: status %d, body %s", rec.Code, rec.Body)
	}
	if updated := decodeUser(t, rec); updated.Status != "disabled" {
		t.Fatalf("status = %q, want disabled", updated.Status)
	}
	if me := env.get(t, "/api/v1/auth/me", targetCookie); me.Code != http.StatusUnauthorized {
		t.Fatalf("a disabled account's session still works: status %d", me.Code)
	}
}

// Every route in this package declares staff:manage, so a customer and a support agent are
// both refused. A missing declaration would be an anonymous administrative surface.
func TestUserAdministrationRequiresStaffManage(t *testing.T) {
	env := newAuthEnv(t)
	targetID := env.createUser(t, "target@example.test", "a long enough password", "")
	env.createUser(t, "agent@example.test", "a long enough password", "support")

	for _, who := range []struct{ email, password string }{
		{"target@example.test", "a long enough password"},
		{"agent@example.test", "a long enough password"},
	} {
		cookie, csrf := env.login(t, who.email, who.password)
		checks := []struct {
			method, path string
			body         any
		}{
			{http.MethodGet, "/api/v1/staff/users", nil},
			{http.MethodPost, "/api/v1/staff/users", map[string]any{
				"email": "sneaky@example.test", "display_name": "Sneaky", "password": "a long enough password",
			}},
			{http.MethodPatch, "/api/v1/staff/users/" + targetID, map[string]any{"status": "disabled"}},
			{http.MethodDelete, "/api/v1/staff/users/" + targetID, nil},
			{http.MethodPost, "/api/v1/staff/users/" + targetID + "/password",
				map[string]any{"password": "a long enough password"}},
			{http.MethodPatch, "/api/v1/staff/roles/00000000-0000-0000-0000-000000000000",
				map[string]any{"name": "nope"}},
		}
		for _, check := range checks {
			rec := env.doUser(t, check.method, check.path, cookie, csrf, check.body)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("%s %s as %s: status %d, want 403; body %s",
					check.method, check.path, who.email, rec.Code, rec.Body)
			}
			// A customer is refused by the staff-only guard before the permission is
			// even consulted; a staff agent without staff:manage is refused by the
			// permission check. Both are 403, and the distinction is deliberate.
			if code := decodeProblemCode(t, rec); code != "forbidden" && code != "staff_required" {
				t.Fatalf("%s %s as %s: code %q, want forbidden or staff_required",
					check.method, check.path, who.email, code)
			}
		}
	}
}

// The audit row for a create is written inside the transaction, so the trail says who
// onboarded whom.
func TestUserCreateIsAudited(t *testing.T) {
	env := newAuthEnv(t)
	adminID := env.createUser(t, "root@example.test", "root password 1", "administrator")
	cookie, csrf := env.login(t, "root@example.test", "root password 1")

	rec := env.doUser(t, http.MethodPost, "/api/v1/staff/users", cookie, csrf, map[string]any{
		"email":        "audited@example.test",
		"display_name": "Audited",
		"password":     "a long enough password",
		"role":         "billing",
	})
	created := decodeUser(t, rec)

	var (
		action, targetType, targetID, actorID string
	)
	if err := env.pool.QueryRow(t.Context(), `
		SELECT action, target_type, target_id::text, actor_id::text
		FROM audit_events
		WHERE action = 'user.create' AND target_id = $1`, created.ID).
		Scan(&action, &targetType, &targetID, &actorID); err != nil {
		t.Fatalf("no audit row for the create: %v", err)
	}
	if targetType != "user" || actorID != adminID {
		t.Fatalf("audit row = %s %s actor %s, want user %s actor %s",
			action, targetType, actorID, created.ID, adminID)
	}
}

// A user administration call with no Idempotency-Key is refused: every staff mutation
// requires one, and this surface is no exception.
func TestUserMutationRequiresIdempotencyKey(t *testing.T) {
	env := newAuthEnv(t)
	env.createUser(t, "root@example.test", "root password 1", "administrator")
	cookie, csrf := env.login(t, "root@example.test", "root password 1")

	body, _ := json.Marshal(map[string]any{
		"email": "nokey@example.test", "display_name": "No Key", "password": "a long enough password",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/staff/users", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", csrf)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	env.srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400; body %s", rec.Code, rec.Body)
	}
	if code := decodeProblemCode(t, rec); code != "idempotency_key_required" {
		t.Fatalf("code = %q, want idempotency_key_required", code)
	}
}

// The permission catalogue the roles screen offers must be the declared set, so the screen
// cannot offer a permission the server refuses.
func TestPermissionCatalogueIsServed(t *testing.T) {
	env := newAuthEnv(t)
	env.createUser(t, "root@example.test", "root password 1", "administrator")
	cookie, _ := env.login(t, "root@example.test", "root password 1")

	rec := env.get(t, "/api/v1/staff/permissions", cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body)
	}
	var body struct {
		Data   []string            `json:"data"`
		ByRole map[string][]string `json:"by_role"`
		System []string            `json:"system"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Data) != len(authn.AllPermissions()) {
		t.Fatalf("catalogue has %d permissions, want %d", len(body.Data), len(authn.AllPermissions()))
	}
	if len(body.System) != 4 {
		t.Fatalf("system roles = %v, want four", body.System)
	}
	if len(body.ByRole["administrator"]) == 0 {
		t.Fatal("the administrator's permission set is not reported")
	}
}
