package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// whmClient is the adapter's view of WHM's API1 "json-api" endpoint.
//
// Six calls: listaccts, createacct, modifyacct, suspendacct, unsuspendacct, removeacct. That
// is the whole surface a hosting-account provisioner needs, and it is *four* calls
// fewer than Pterodactyl needed only because cPanel has no concept of an
// allocation, a nest or an egg to discover.
//
// NOT PTERODACTYL-SHAPED: WHM is a query-string API. There are no paths, no
// request bodies and no pagination; a parameter that is not in the query string
// does not exist, and an unknown parameter is ignored rather than rejected. That
// last part is worth knowing: a typo in `bwlimit` is silently a default-bandwidth
// account, not an error. The fake in this package is therefore stricter than WHM
// on purpose, because the real API will not tell us when we get it wrong.
type whmClient struct {
	// base is the WHM root URL without a trailing slash, e.g.
	// https://whm.example.com:2087.
	base  string
	token string
	http  *http.Client
}

func newWHMClient(cfg config) *whmClient {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if !cfg.verifyTLS {
		// CPANEL_VERIFY_TLS=0 is for a WHM box with a self-signed certificate. It
		// is deliberately noisy at startup (see configFromEnv) because it turns
		// the API token into something a network attacker can harvest.
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // operator opt-in, warned at startup
	}
	return &whmClient{
		base:  cfg.whmURL,
		token: cfg.apiToken,
		http:  &http.Client{Timeout: cfg.timeout, Transport: transport},
	}
}

// whmError is a refusal or failure from WHM.
//
// The distinction this type carries is the one the core branches on: a *refusal*
// (metadata.result == 0) is a business answer and maps to -32005; everything else
// — a transport failure, a 5xx, a body that is not the json-api envelope — is a
// failure to answer, maps to -32000, and must never be reported as not-found.
type whmError struct {
	// httpStatus is the HTTP status, when there was one.
	httpStatus int
	// command is the WHM function that failed, e.g. "createacct".
	command string
	// reason is WHM's metadata.reason, its own words for the refusal.
	reason string
	// refused is true when WHM answered with a well-formed envelope whose
	// metadata.result was 0: it understood the request and said no.
	refused bool
	// detail is extra context for a failure that is not a refusal.
	detail string
}

func (e *whmError) Error() string {
	switch {
	case e == nil:
		return "cpanel: unknown error"
	case e.refused && e.reason != "":
		return fmt.Sprintf("cPanel refused %s: %s", e.command, e.reason)
	case e.refused:
		return fmt.Sprintf("cPanel refused %s without giving a reason", e.command)
	case e.detail != "":
		return e.detail
	case e.httpStatus != 0:
		return fmt.Sprintf("cPanel %s returned HTTP %d", e.command, e.httpStatus)
	default:
		return fmt.Sprintf("cPanel %s failed", e.command)
	}
}

// data is the JSON-RPC error data. It never carries the API token or the query
// string, both of which would end up in the core's logs.
func (e *whmError) data() map[string]any {
	out := map[string]any{}
	if e.httpStatus != 0 {
		out["http_status"] = e.httpStatus
	}
	if e.command != "" {
		out["command"] = e.command
	}
	if e.refused {
		out["refused"] = true
		out["reason"] = e.reason
	}
	return out
}

// whmEnvelope is the reply shape every json-api call shares:
//
//	{"metadata":{"result":1,"reason":"...","version":1,"command":"createacct"},
//	 "data":{...}}
//
// NOT PTERODACTYL-SHAPED: Pterodactyl reports success and failure with the HTTP
// status. WHM answers HTTP 200 with metadata.result == 0 when it refuses, so the
// status code alone says nothing about whether the operation happened.
type whmEnvelope struct {
	Metadata struct {
		Result  flexInt `json:"result"`
		Reason  string  `json:"reason"`
		Command string  `json:"command"`
		Version int     `json:"version"`
	} `json:"metadata"`
	Data json.RawMessage `json:"data"`
}

// whmAccount is the part of a listaccts row this adapter reads.
type whmAccount struct {
	User      string   `json:"user"`
	Domain    string   `json:"domain"`
	Plan      string   `json:"plan"`
	Suspended flexBool `json:"suspended"`
	IP        string   `json:"ip"`
	Email     string   `json:"email"`
	// StartDate is "YYYY-MM-DD", the day the account was created. WHM reports a
	// date, not a timestamp, so it is only used when the ledger has nothing
	// better.
	StartDate string `json:"startdate"`
}

// isSuspended reports the account's lifecycle state. cPanel has exactly two
// states for an account that exists, so there is no "provisioning" or
// "terminated" to map.
func (a whmAccount) isSuspended() bool { return bool(a.Suspended) }

func (a whmAccount) createdAt() time.Time {
	for _, layout := range []string{"2006-01-02", "2006-01-02 15:04:05", time.RFC3339} {
		if t, err := time.Parse(layout, a.StartDate); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

// call performs one json-api call and decodes its data into out (which may be
// nil).
//
// The API token goes in an `Authorization: whm root:TOKEN` header. It is *not* a
// query parameter: a token in a URL ends up in the WHM access log and in every
// proxy log between here and there.
func (c *whmClient) call(ctx context.Context, command string, params url.Values, out any) error {
	if params == nil {
		params = url.Values{}
	}
	// Every json-api call must ask for the versioned API; without api.version the
	// reply shape is the old flat one and metadata does not exist.
	params.Set("api.version", "1")
	endpoint := c.base + "/json-api/" + command + "?" + params.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return &whmError{command: command, detail: fmt.Sprintf("cPanel %s: build request: %v", command, err)}
	}
	req.Header.Set("Authorization", "whm root:"+c.token)
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		// A transport failure is "I do not know", never "no such account".
		return &whmError{command: command, detail: fmt.Sprintf("cPanel %s: %v", command, err)}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return &whmError{httpStatus: resp.StatusCode, command: command,
			detail: fmt.Sprintf("cPanel %s: read reply: %v", command, err)}
	}

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		// A bad token is a configuration mistake, not a business refusal and not
		// "not found". Saying so once at the top of the error saves an operator a
		// day of reading queue retries.
		return &whmError{httpStatus: resp.StatusCode, command: command,
			detail: fmt.Sprintf("WHM rejected the API token (HTTP %d); check CPANEL_API_TOKEN and that the token has the root-level privileges createacct needs", resp.StatusCode)}
	}

	var env whmEnvelope
	decodeErr := json.Unmarshal(raw, &env)
	if decodeErr != nil || (resp.StatusCode >= 400 && env.Metadata.Command == "") {
		// A 5xx from a proxy in front of WHM is an HTML error page, and a 404 is
		// the wrong CPANEL_WHM_URL. Neither is an answer.
		return &whmError{httpStatus: resp.StatusCode, command: command,
			detail: fmt.Sprintf("cPanel %s returned HTTP %d and a body that is not the json-api envelope: %s",
				command, resp.StatusCode, excerpt(raw))}
	}
	if env.Metadata.Result != 1 {
		return &whmError{httpStatus: resp.StatusCode, command: command,
			refused: true, reason: strings.TrimSpace(env.Metadata.Reason)}
	}
	if out != nil && len(env.Data) > 0 && string(env.Data) != "null" {
		if err := json.Unmarshal(env.Data, out); err != nil {
			return &whmError{httpStatus: resp.StatusCode, command: command,
				detail: fmt.Sprintf("cPanel %s: decode data: %v", command, err)}
		}
	}
	return nil
}

func excerpt(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 200 {
		return s[:200] + "…"
	}
	if s == "" {
		return "(empty body)"
	}
	return s
}

// listAccounts reads accounts, optionally filtered by username.
//
// The filter is `searchtype=user&search=<name>`, which WHM documents as a search
// rather than an exact lookup, so the caller must still compare the returned
// `user` field exactly. Treating a substring hit as a match would hand the core
// somebody else's account as the result of this operation.
func (c *whmClient) listAccounts(ctx context.Context, user string) ([]whmAccount, error) {
	params := url.Values{}
	params.Set("want", "user,domain,plan,suspended,ip,email,startdate")
	if user != "" {
		params.Set("searchtype", "user")
		params.Set("search", user)
	}
	var data struct {
		Acct []whmAccount `json:"acct"`
	}
	if err := c.call(ctx, "listaccts", params, &data); err != nil {
		return nil, err
	}
	return data.Acct, nil
}

// findAccount answers "does this account exist?".
//
// A nil result means WHM answered and it does not exist. An error means WHM could
// not answer, which the caller must not confuse with absence: that confusion is
// how a reconciler provisions a second account.
func (c *whmClient) findAccount(ctx context.Context, user string) (*whmAccount, error) {
	if user == "" {
		return nil, nil
	}
	accounts, err := c.listAccounts(ctx, user)
	if err != nil {
		return nil, err
	}
	for i := range accounts {
		if accounts[i].User == user {
			return &accounts[i], nil
		}
	}
	return nil, nil
}

// createAccountRequest is what the adapter knows that WHM needs.
type createAccountRequest struct {
	Username string
	Domain   string
	// QuotaMB is the disk quota in megabytes; 0 would mean unlimited, which is
	// why plans reject a zero quota rather than offering it by accident.
	QuotaMB int64
	// BandwidthMB is the monthly transfer limit in megabytes.
	BandwidthMB  int64
	ContactEmail string
}

// createAccount creates one hosting account.
//
// NOT PTERODACTYL-SHAPED: there is no package/plan object to select. WHM's
// "packages" are server-side presets an operator maintains; a billing plan here is
// a quota/bandwidth pair the panel owns, so createacct is called with `quota` and
// `bwlimit` and no `plan`. That means a plan the billing panel sells does not have
// to exist in WHM first, and it also means WHM cannot tell us which plan an
// account was sold on — the ledger in this package is the only place that survives
// (see state.go).
func (c *whmClient) createAccount(ctx context.Context, in createAccountRequest) error {
	params := url.Values{}
	params.Set("username", in.Username)
	params.Set("domain", in.Domain)
	params.Set("quota", fmt.Sprint(in.QuotaMB))
	params.Set("bwlimit", fmt.Sprint(in.BandwidthMB))
	// A cPanel account is a shell-less, jailed user by default; hasshell=0 makes
	// that explicit rather than depending on the server's default.
	params.Set("hasshell", "0")
	if in.ContactEmail != "" {
		params.Set("contactemail", in.ContactEmail)
	}
	// No request body, no idempotency key, no external id: WHM has nowhere to put
	// one. Uniqueness of `username` is the only thing that stops a duplicate, which
	// is exactly why the username is derived from the operation key.
	return c.call(ctx, "createacct", params, nil)
}

// suspendAccount stops an account without deleting it. WHM keeps the files and
// the databases, which is what a billing suspension means.
// modifyAccount changes an account's quota and bandwidth in place.
//
// NOT PTERODACTYL-SHAPED: there is no build to rebuild and no startup environment to resend. An
// account's "size" here is two WHM limits, and modifyacct sets them without touching a file,
// a database or a mailbox.
func (c *whmClient) modifyAccount(ctx context.Context, user string, p plan) error {
	params := url.Values{}
	params.Set("user", user)
	params.Set("quota", fmt.Sprint(p.QuotaMB))
	params.Set("bwlimit", fmt.Sprint(p.BandwidthMB))
	return c.call(ctx, "modifyacct", params, nil)
}

func (c *whmClient) suspendAccount(ctx context.Context, user, reason string) error {
	params := url.Values{}
	params.Set("user", user)
	if strings.TrimSpace(reason) != "" {
		// WHM truncates the reason at 255 characters; cutting it here keeps the
		// request identical to what the operator will see in WHM.
		params.Set("reason", truncate(reason, 255))
	}
	return c.call(ctx, "suspendacct", params, nil)
}

func (c *whmClient) unsuspendAccount(ctx context.Context, user string) error {
	params := url.Values{}
	params.Set("user", user)
	return c.call(ctx, "unsuspendacct", params, nil)
}

// removeAccount destroys an account and its data.
//
// NOT PTERODACTYL-SHAPED: there is no `force` flag and no separate "keep the
// files" option. removeacct is final; the DNS zone is kept unless keepdns is
// cleared, which is why it is cleared here — a terminated service should not leave
// its zone behind, and the customer's domain is not ours to keep pointing.
func (c *whmClient) removeAccount(ctx context.Context, user string) error {
	params := url.Values{}
	params.Set("user", user)
	params.Set("keepdns", "0")
	return c.call(ctx, "removeacct", params, nil)
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}

// flexInt decodes WHM's habit of answering a number as a number, as a string, or
// as a bool depending on the function and the version. metadata.result is 1 or 0
// and must never be read as anything else.
type flexInt int

func (f *flexInt) UnmarshalJSON(b []byte) error {
	raw := strings.TrimSpace(string(b))
	raw = strings.Trim(raw, `"`)
	switch raw {
	case "1", "true":
		*f = 1
	case "0", "false", "":
		*f = 0
	default:
		return fmt.Errorf("cpanel: %q is not a WHM result flag", raw)
	}
	return nil
}

// flexBool is the same tolerance applied to listaccts' `suspended` field, which
// is 0/1 on the versions this adapter was written against and true/false on
// others.
type flexBool bool

func (f *flexBool) UnmarshalJSON(b []byte) error {
	var i flexInt
	if err := i.UnmarshalJSON(b); err != nil {
		return err
	}
	*f = flexBool(i != 0)
	return nil
}
