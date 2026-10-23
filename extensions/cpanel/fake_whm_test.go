package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// testToken is the API token the fake accepts.
const testToken = "ABCDEF0123456789"

// fakeWHM is a stand-in for WHM's json-api, strict enough to catch a wrong
// request.
//
// It is deliberately *stricter* than the real API. WHM ignores a parameter it does
// not recognise and answers a missing one with a default, so a typo in `bwlimit`
// silently becomes an account with the server's default bandwidth. The fake
// refuses the request instead, which is the only way a test can notice.
//
// It enforces: api.version=1, the `whm root:TOKEN` authorization header, cPanel's
// username rules, a domain that looks like a domain, a positive quota and
// bandwidth, and the real state machine — a second suspend of a suspended account
// is metadata.result 0, not a no-op.
type fakeWHM struct {
	t      *testing.T
	server *httptest.Server

	mu       sync.Mutex
	accounts map[string]*fakeAccount
	requests []whmRequest
	failNext *failMode
	// refuse maps a command to a reason, so the next call to *that* command
	// answers a well-formed envelope with metadata.result 0: WHM understood the
	// request and said no.
	refuse map[string]string
	// raceOnCreate makes createacct lose a race: it records the account and then
	// refuses because the name is taken, which is what happens when a second
	// worker creates the same operation key at the same moment.
	raceOnCreate bool
}

type whmRequest struct {
	Method  string
	Command string
	Query   url.Values
	Auth    string
}

type failMode struct {
	status int
	body   string
}

type fakeAccount struct {
	User      string
	Domain    string
	Quota     int64
	Bandwidth int64
	Suspended bool
	Email     string
	StartDate string
}

func newFakeWHM(t *testing.T) *fakeWHM {
	t.Helper()
	f := &fakeWHM{t: t, accounts: map[string]*fakeAccount{}, refuse: map[string]string{}}
	f.server = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.server.Close)
	return f
}

// config is a configuration pointed at the fake, with a ledger in a fresh
// temporary directory so tests cannot see each other's history.
func (f *fakeWHM) config() config {
	plans := []plan{
		{ID: "small", QuotaMB: 1024, BandwidthMB: 10240},
		{ID: "large", QuotaMB: 10240, BandwidthMB: 102400},
	}
	parsed, err := url.Parse(f.server.URL)
	if err != nil {
		f.t.Fatalf("fake url: %v", err)
	}
	return config{
		whmURL:      f.server.URL,
		apiToken:    testToken,
		timeout:     5 * time.Second,
		verifyTLS:   true,
		statePath:   filepath.Join(f.t.TempDir(), "operations.json"),
		plans:       plans,
		plansByID:   map[string]plan{"small": plans[0], "large": plans[1]},
		defaultPlan: "small",
		host:        parsed.Hostname(),
	}
}

func (f *fakeWHM) lastRequest() whmRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) == 0 {
		f.t.Fatal("no request recorded")
	}
	return f.requests[len(f.requests)-1]
}

func (f *fakeWHM) requestCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

func (f *fakeWHM) commands() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.requests))
	for _, r := range f.requests {
		out = append(out, r.Command)
	}
	return out
}

// failWith makes the next call answer this HTTP status and body, before any
// authentication or routing.
func (f *fakeWHM) failWith(status int, body string) {
	f.mu.Lock()
	f.failNext = &failMode{status: status, body: body}
	f.mu.Unlock()
}

// refuseWith makes the next call to command answer metadata.result 0 with this
// reason.
func (f *fakeWHM) refuseWith(command, reason string) {
	f.mu.Lock()
	f.refuse[command] = reason
	f.mu.Unlock()
}

// requestFor returns the first recorded request for a command.
func (f *fakeWHM) requestFor(command string) whmRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.requests {
		if r.Command == command {
			return r
		}
	}
	f.t.Fatalf("no %s request recorded (recorded: %v)", command, commandNames(f.requests))
	return whmRequest{}
}

// countFor counts recorded requests for a command.
func (f *fakeWHM) countFor(command string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, r := range f.requests {
		if r.Command == command {
			n++
		}
	}
	return n
}

func commandNames(requests []whmRequest) []string {
	out := make([]string, 0, len(requests))
	for _, r := range requests {
		out = append(out, r.Command)
	}
	return out
}

// addAccount creates an account out of band, as staff using WHM would.
func (f *fakeWHM) addAccount(user, domain string, suspended bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.accounts[user] = &fakeAccount{
		User: user, Domain: domain, Quota: 1024, Bandwidth: 10240,
		Suspended: suspended, StartDate: "2024-01-02",
	}
}

func (f *fakeWHM) hasAccount(user string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.accounts[user]
	return ok
}

func (f *fakeWHM) accountCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.accounts)
}

func (f *fakeWHM) handle(w http.ResponseWriter, r *http.Request) {
	command := strings.TrimPrefix(r.URL.Path, "/json-api/")
	rec := whmRequest{
		Method:  r.Method,
		Command: command,
		Query:   r.URL.Query(),
		Auth:    r.Header.Get("Authorization"),
	}

	f.mu.Lock()
	f.requests = append(f.requests, rec)
	fail := f.failNext
	f.failNext = nil
	refuseReason, refuse := f.refuse[command]
	delete(f.refuse, command)
	f.mu.Unlock()

	if fail != nil {
		w.WriteHeader(fail.status)
		fmt.Fprint(w, fail.body)
		return
	}

	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		fmt.Fprint(w, `{"metadata":{"result":0,"reason":"json-api is GET only"}}`)
		return
	}
	// A real WHM answers 403 for a bad token, before it looks at the function.
	if rec.Auth != "whm root:"+testToken {
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, "<html><body>Access denied</body></html>")
		return
	}
	if rec.Query.Get("api.version") != "1" {
		// Without api.version WHM answers the legacy flat shape, which has no
		// metadata and therefore cannot report a refusal.
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `[{"status":1,"statusmsg":"ok"}]`)
		return
	}
	if refuse {
		f.writeRefused(w, command, refuseReason)
		return
	}

	switch command {
	case "listaccts":
		f.listAccts(w, rec.Query)
	case "createacct":
		f.createAcct(w, rec.Query)
	case "modifyacct":
		f.modifyAcct(w, rec.Query)
	case "suspendacct":
		f.suspendAcct(w, rec.Query, true)
	case "unsuspendacct":
		f.suspendAcct(w, rec.Query, false)
	case "removeacct":
		f.removeAcct(w, rec.Query)
	default:
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, "<html><body>Not Found</body></html>")
	}
}

func (f *fakeWHM) writeOK(w http.ResponseWriter, command string, data any) {
	w.Header().Set("Content-Type", "application/json")
	body := map[string]any{
		"metadata": map[string]any{"result": 1, "reason": "OK", "version": 1, "command": command},
	}
	if data != nil {
		body["data"] = data
	}
	_ = json.NewEncoder(w).Encode(body)
}

func (f *fakeWHM) writeRefused(w http.ResponseWriter, command, reason string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"metadata": map[string]any{"result": 0, "reason": reason, "version": 1, "command": command},
	})
}

func (f *fakeWHM) listAccts(w http.ResponseWriter, query url.Values) {
	search := query.Get("search")
	if search != "" && query.Get("searchtype") != "user" {
		f.writeRefused(w, "listaccts", "searchtype must be user for a username search")
		return
	}
	f.mu.Lock()
	rows := make([]map[string]any, 0, len(f.accounts))
	for _, acct := range f.accounts {
		// WHM's `search` is a substring match, not an exact lookup. The adapter
		// has to compare the user field itself; emulating that here is what makes
		// the comparison testable.
		if search != "" && !strings.Contains(acct.User, search) {
			continue
		}
		suspended := 0
		if acct.Suspended {
			suspended = 1
		}
		rows = append(rows, map[string]any{
			"user":      acct.User,
			"domain":    acct.Domain,
			"plan":      "",
			"suspended": suspended,
			"ip":        "192.0.2.10",
			"email":     acct.Email,
			"startdate": acct.StartDate,
		})
	}
	f.mu.Unlock()
	f.writeOK(w, "listaccts", map[string]any{"acct": rows})
}

func (f *fakeWHM) createAcct(w http.ResponseWriter, query url.Values) {
	user := query.Get("username")
	domain := query.Get("domain")
	quota, quotaErr := strconv.ParseInt(query.Get("quota"), 10, 64)
	bandwidth, bandwidthErr := strconv.ParseInt(query.Get("bwlimit"), 10, 64)

	switch {
	case user == "":
		f.writeRefused(w, "createacct", "The username is required")
		return
	case domain == "" || !strings.Contains(domain, "."):
		f.writeRefused(w, "createacct", "The domain "+domain+" is not a valid domain")
		return
	case quotaErr != nil || quota <= 0:
		f.writeRefused(w, "createacct", "The quota is not a positive number of megabytes")
		return
	case bandwidthErr != nil || bandwidth <= 0:
		f.writeRefused(w, "createacct", "The bandwidth limit is not a positive number of megabytes")
		return
	}
	if err := validateUsername(user); err != nil {
		f.writeRefused(w, "createacct", "The username "+user+" is not valid")
		return
	}

	if f.raceOnCreate {
		// The other worker got there first, between our listaccts and our
		// createacct.
		f.addAccount(user, domain, false)
		f.writeRefused(w, "createacct", "Sorry, the user "+user+" already exists")
		return
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if _, exists := f.accounts[user]; exists {
		f.writeRefused(w, "createacct", "Sorry, the user "+user+" already exists")
		return
	}
	f.accounts[user] = &fakeAccount{
		User: user, Domain: domain, Quota: quota, Bandwidth: bandwidth,
		Email: query.Get("contactemail"), StartDate: time.Now().UTC().Format("2006-01-02"),
	}
	f.writeOK(w, "createacct", map[string]any{"ip": "192.0.2.10", "user": user})
}

// modifyAcct applies modifyacct the way WHM does: it sets the two limits it is given and
// leaves everything else (files, mailboxes, the domain) alone.
func (f *fakeWHM) modifyAcct(w http.ResponseWriter, query url.Values) {
	user := query.Get("user")
	quota, quotaErr := strconv.ParseInt(query.Get("quota"), 10, 64)
	bandwidth, bandwidthErr := strconv.ParseInt(query.Get("bwlimit"), 10, 64)
	switch {
	case user == "":
		f.writeRefused(w, "modifyacct", "The user is required")
		return
	case quotaErr != nil || quota <= 0:
		f.writeRefused(w, "modifyacct", "The quota is not a positive number of megabytes")
		return
	case bandwidthErr != nil || bandwidth <= 0:
		f.writeRefused(w, "modifyacct", "The bandwidth limit is not a positive number of megabytes")
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	acct, ok := f.accounts[user]
	if !ok {
		f.writeRefused(w, "modifyacct", "Sorry, the user "+user+" does not exist")
		return
	}
	acct.Quota = quota
	acct.Bandwidth = bandwidth
	f.writeOK(w, "modifyacct", map[string]any{"user": user})
}

func (f *fakeWHM) suspendAcct(w http.ResponseWriter, query url.Values, suspend bool) {
	command := "suspendacct"
	if !suspend {
		command = "unsuspendacct"
	}
	user := query.Get("user")
	f.mu.Lock()
	defer f.mu.Unlock()
	acct, ok := f.accounts[user]
	if !ok {
		f.writeRefused(w, command, "Sorry, the user "+user+" does not exist")
		return
	}
	// The real API refuses a state change that would not change anything, which is
	// why the adapter must check the state first rather than calling blindly.
	if acct.Suspended == suspend {
		if suspend {
			f.writeRefused(w, command, "The account "+user+" is already suspended")
		} else {
			f.writeRefused(w, command, "The account "+user+" is not suspended")
		}
		return
	}
	acct.Suspended = suspend
	f.writeOK(w, command, map[string]any{"user": user})
}

func (f *fakeWHM) removeAcct(w http.ResponseWriter, query url.Values) {
	user := query.Get("user")
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.accounts[user]; !ok {
		f.writeRefused(w, "removeacct", "Sorry, the user "+user+" does not exist")
		return
	}
	delete(f.accounts, user)
	f.writeOK(w, "removeacct", map[string]any{"user": user})
}
