package client_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	client "github.com/catalyst/billing/api/client"
)

// The generated client is not decoration: these tests drive it against a real HTTP server
// and assert the three things a client has to get right — path substitution, the error
// envelope, and the headers a mutation needs.

func newServer(t *testing.T, handler http.HandlerFunc) (*httptest.Server, *client.Client) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv, &client.Client{BaseURL: srv.URL, HTTP: srv.Client()}
}

// A path parameter is substituted and escaped: a value containing a slash must not be able
// to reach a different route.
func TestPathParameterIsEscaped(t *testing.T) {
	var gotPath string
	srv, c := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.EscapedPath()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x"}`))
	})
	c.BaseURL = srv.URL

	res, err := c.GetPortalInvoicesById(context.Background(), "../../admin", nil, nil)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if !res.OK() {
		t.Fatalf("status = %d", res.StatusCode)
	}
	if strings.Contains(gotPath, "../") {
		t.Fatalf("the path parameter escaped into the path: %s", gotPath)
	}
	if !strings.Contains(gotPath, "..%2F..%2Fadmin") {
		t.Fatalf("path = %s, want the value escaped", gotPath)
	}
}

// A failing response becomes a typed Problem, so a caller branches on `code` rather than on
// a status number or a string.
func TestProblemIsDecoded(t *testing.T) {
	_, c := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"type":"about:blank","title":"Forbidden","status":403,` +
			`"detail":"you do not have access to this action","code":"forbidden"}`))
	})

	res, err := c.GetOpsOverview(context.Background(), nil, nil)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if res.OK() {
		t.Fatal("a 403 was reported as success")
	}
	problem, ok := res.Err().(*client.Problem)
	if !ok {
		t.Fatalf("error type = %T, want *client.Problem", res.Err())
	}
	if problem.Code != "forbidden" || problem.Status != 403 {
		t.Fatalf("problem = %+v", problem)
	}
	if !strings.Contains(problem.Error(), "forbidden") {
		t.Fatalf("Error() = %q", problem.Error())
	}
}

// Decode refuses on a failure rather than silently leaving the destination zero.
func TestDecodeRefusesOnFailure(t *testing.T) {
	_, c := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"code":"not_found","title":"Not Found","status":404}`))
	})
	res, err := c.GetAuthMe(context.Background(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := res.Decode(&out); err == nil {
		t.Fatal("Decode succeeded on a 404")
	}
}

// A mutation sends the session cookie, the CSRF header and the idempotency key. Without all
// three the server refuses it, which is exactly why the client must send them.
func TestMutationSendsRequiredHeaders(t *testing.T) {
	var (
		sawCookie, sawCSRF, sawKey, sawAuth string
		sawBody                             []byte
	)
	_, c := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		sawCookie = r.Header.Get("Cookie")
		sawCSRF = r.Header.Get("X-CSRF-Token")
		sawKey = r.Header.Get("Idempotency-Key")
		sawAuth = r.Header.Get("Authorization")
		sawBody = make([]byte, 64)
		n, _ := r.Body.Read(sawBody)
		sawBody = sawBody[:n]
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"status":"cancellation_scheduled","ends_at":"2024-06-01T00:00:00Z"}`))
	})
	c.SessionCookie = "session-token"
	c.CSRFToken = "csrf-token"
	c.IdempotencyKey = "key-00000001"

	res, err := c.PostPortalServicesByIdCancel(context.Background(), "svc_1", nil,
		map[string]any{"reason": "moving house"})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d", res.StatusCode)
	}
	if !strings.Contains(sawCookie, "billing_session=session-token") {
		t.Fatalf("cookie = %q", sawCookie)
	}
	if sawCSRF != "csrf-token" {
		t.Fatalf("csrf = %q", sawCSRF)
	}
	if sawKey != "key-00000001" {
		t.Fatalf("idempotency key = %q", sawKey)
	}
	if sawAuth != "" {
		t.Fatalf("a session request also sent Authorization: %q", sawAuth)
	}
	if !strings.Contains(string(sawBody), "moving house") {
		t.Fatalf("body = %s", sawBody)
	}

	var out map[string]any
	if err := res.Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out["status"] != "cancellation_scheduled" {
		t.Fatalf("body = %v", out)
	}
}

// A bearer request does not send the CSRF header: a bearer token cannot be forged by a
// browser, so the double-submit check does not apply to it.
func TestBearerRequestSkipsCSRF(t *testing.T) {
	var sawCSRF, sawAuth string
	_, c := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		sawCSRF = r.Header.Get("X-CSRF-Token")
		sawAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	})
	c.BearerToken = "bk_test"
	c.CSRFToken = "csrf-token"

	if _, err := c.PostAuthLogout(context.Background(), nil, nil); err != nil {
		t.Fatalf("call: %v", err)
	}
	if sawAuth != "Bearer bk_test" {
		t.Fatalf("Authorization = %q", sawAuth)
	}
	if sawCSRF != "" {
		t.Fatalf("a bearer request sent X-CSRF-Token: %q", sawCSRF)
	}
}

// Query parameters reach the server encoded, and a cursor round-trips unchanged.
func TestQueryIsEncoded(t *testing.T) {
	var got url.Values
	_, c := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Query()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[],"next_cursor":null}`))
	})

	q := url.Values{}
	q.Set("limit", "25")
	q.Set("cursor", "MjAyNC0wMS0wMg==")
	q.Set("q", "a b")
	res, err := c.GetPortalInvoices(context.Background(), q, nil)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if got.Get("limit") != "25" || got.Get("cursor") != "MjAyNC0wMS0wMg==" || got.Get("q") != "a b" {
		t.Fatalf("query = %v", got)
	}

	var page client.Page
	if err := res.Decode(&page); err != nil {
		t.Fatalf("decode page: %v", err)
	}
	if page.NextCursor != nil {
		t.Fatalf("next_cursor = %v, want nil", *page.NextCursor)
	}
	var data []map[string]any
	if err := json.Unmarshal(page.Data, &data); err != nil {
		t.Fatalf("page data: %v", err)
	}
}

// A non-JSON error body (a proxy error page) must not crash the client or be mistaken for a
// typed problem.
func TestNonJSONErrorIsTolerated(t *testing.T) {
	_, c := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("<html>bad gateway</html>"))
	})
	res, err := c.GetHealthz(context.Background(), nil, nil)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if res.OK() {
		t.Fatal("a 502 was reported as success")
	}
	if res.Problem != nil {
		t.Fatalf("an HTML body was parsed as a problem: %+v", res.Problem)
	}
	if res.Err() == nil {
		t.Fatal("Err() returned nil for a 502")
	}
}
