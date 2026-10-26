package main

// The CatalystCTL panel API, as much of it as a provisioner needs.
//
// # What is deliberately absent
//
// A Catalyst server object embeds its node, and the node object includes the agent's own
// `secret`. That field must never reach the billing panel, which stores customer-facing data and
// renders it in a browser. So the node is modelled here as three fields — id, name, public
// address — and the rest of the payload is dropped at decode time. A struct that carried the
// whole node "for completeness" would put a cluster credential one careless `json.Marshal` away
// from a customer's screen.
//
// # Why every call is bounded and every error is typed
//
// The panel is a remote system across a network. A read that hangs is a page that never loads, so
// every request carries the configured timeout through the shared client. And an error is not a
// string: the panel answers `{"error": "...", "code": "SERVER_NOT_FOUND"}`, and the protocol needs
// to distinguish "that server does not exist" (-32004, which the core treats as authoritative)
// from "the panel is unwell" (-32000, which is retryable). Flattening the two into one error is
// how an adapter lies about a resource having been deleted.

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// panelClient is the adapter's whole view of the panel.
type panelClient struct {
	// base is the panel origin without a trailing slash, e.g. https://panel.example.com.
	base string
	key  string
	http *http.Client
	// pageLimit bounds a list scan. Lookup falls back to scanning because the panel has no
	// filter by external key, and a bounded scan that gives up honestly is better than an
	// unbounded one that walks a shared panel forever.
	pageLimit int
}

func newPanelClient(cfg config) *panelClient {
	return &panelClient{
		base:      normalisePanelURL(cfg.panelURL),
		key:       cfg.apiKey,
		http:      &http.Client{Timeout: cfg.timeout, Transport: transportFor(cfg)},
		pageLimit: 50,
	}
}

// transportFor builds the HTTP transport, honouring the TLS verification setting.
//
// The setting exists because a self-hosted panel on a LAN often has a self-signed certificate, and
// refusing to start is worse than starting with a loud warning. It defaults to verifying, and the
// only way to turn it off is an explicit CCTL_VERIFY_TLS=off, which main.go announces on stderr at
// startup — so an operator reading logs cannot miss that they did it.
func transportFor(cfg config) *http.Transport {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if !cfg.verifyTLS {
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // opt-in, warned at startup
	}
	return transport
}

// normalisePanelURL trims what an operator habitually pastes into a base URL.
//
// The panel's own documentation and its API explorer both show paths beginning `/api`, so `…/api` is what
// people paste. Joined with the adapter's own `/api` prefix that becomes `/api/api/servers/…`, which the panel
// answers with a bare `404 {"error":"Not Found"}` — indistinguishable, before this change, from "that server
// does not exist". One character of configuration turned into a customer-facing "the server panel does not
// recognise this service".
//
// Stripping it here is better than documenting it: the value is unambiguous, and a deployment that keeps the
// old value keeps working.
func normalisePanelURL(raw string) string {
	u := strings.TrimSpace(raw)
	for {
		trimmed := strings.TrimRight(u, "/")
		// `/api` and a versioned `/api/v1` are both paths into the same API root.
		for _, suffix := range []string{"/api", "/api/v1", "/api/v2"} {
			if strings.HasSuffix(strings.ToLower(trimmed), suffix) {
				trimmed = trimmed[:len(trimmed)-len(suffix)]
				break
			}
		}
		if trimmed == u {
			return u
		}
		u = trimmed
	}
}

// panelError is a refusal or failure from the panel.
type panelError struct {
	status  int
	code    string
	message string
	// resourceMissing is set once a 404 has been shown to be a real lookup rather than a wrong API root.
	resourceMissing bool
}

func (e *panelError) Error() string {
	switch {
	case e == nil:
		return "catalystctl: unknown error"
	case e.code != "" && e.message != "":
		return fmt.Sprintf("catalystctl: %s (HTTP %d): %s", e.code, e.status, e.message)
	case e.message != "":
		return fmt.Sprintf("catalystctl: HTTP %d: %s", e.status, e.message)
	default:
		return fmt.Sprintf("catalystctl: HTTP %d", e.status)
	}
}

// notFound reports whether the panel said the thing does not exist.
//
// Only the panel's own code counts. A 404 without it is ambiguous — it is what a wrong API root answers, and
// treating it as "gone" is how a misconfigured panel URL reports every customer's server as missing. The one
// exception is a 404 the caller has *proven* to be a real lookup, by checking that the API root itself answers
// (see probeRoot); that proof is recorded on the error as resourceMissing.
func (e *panelError) notFound() bool {
	if e == nil {
		return false
	}
	if e.code == "SERVER_NOT_FOUND" {
		return true
	}
	return e.status == http.StatusNotFound && e.resourceMissing
}

// locked reports whether the resource is suspended or in another state the panel refuses to act
// on. The panel also uses 423 for "suspended", which is not a failure to report as broken.
func (e *panelError) locked() bool { return e != nil && e.status == http.StatusLocked }

// flexInt64 decodes a number the panel sometimes writes as a JSON string.
//
// The stats endpoint reports `"networkRxBytes": "270748827193"` — a string, because the value can
// exceed what a JavaScript number holds exactly — while other numeric fields are plain numbers.
// One type that accepts both is what stops that inconsistency from being a decode failure on the
// busiest screen in the feature.
type flexInt64 int64

func (f *flexInt64) UnmarshalJSON(raw []byte) error {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		*f = 0
		return nil
	}
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return err
		}
		s = strings.TrimSpace(s)
		if s == "" {
			*f = 0
			return nil
		}
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			// A float string (some panels emit "1.2e3") is not a byte count worth failing a page
			// over: the measurement is advisory, so an unparseable one becomes absent.
			if fl, ferr := strconv.ParseFloat(s, 64); ferr == nil {
				*f = flexInt64(fl)
				return nil
			}
			*f = 0
			return nil
		}
		*f = flexInt64(n)
		return nil
	}
	var n int64
	if err := json.Unmarshal(raw, &n); err != nil {
		var fl float64
		if ferr := json.Unmarshal(raw, &fl); ferr == nil {
			*f = flexInt64(fl)
			return nil
		}
		return err
	}
	*f = flexInt64(n)
	return nil
}

func (f flexInt64) Int64() int64 { return int64(f) }

// panelNode is the sliver of a node the billing panel may see.
type panelNode struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	LocationID    string `json:"locationId"`
	PublicAddress string `json:"publicAddress"`
	IsOnline      bool   `json:"isOnline"`
	MaxMemoryMb   int64  `json:"maxMemoryMb"`
	MaxCpuCores   int64  `json:"maxCpuCores"`
}

// panelLocation is one datacenter.
type panelLocation struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// panelTemplate is one sellable game/service definition.
type panelTemplate struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// panelNodeStats is what a node has and has not allocated.
type panelNodeStats struct {
	NodeID    string `json:"nodeId"`
	IsOnline  bool   `json:"isOnline"`
	Resources struct {
		MaxMemoryMb       int64 `json:"maxMemoryMb"`
		MaxCpuCores       int64 `json:"maxCpuCores"`
		AllocatedMemoryMb int64 `json:"allocatedMemoryMb"`
		AllocatedCpuCores int64 `json:"allocatedCpuCores"`
	} `json:"resources"`
}

// panelConnection is where a player connects.
type panelConnection struct {
	AssignedIP    string `json:"assignedIp"`
	NodeIP        string `json:"nodeIp"`
	HostNetworkIP string `json:"hostNetworkIp"`
	Host          string `json:"host"`
	Port          int32  `json:"port"`
}

// panelServer is one server, as the list and detail endpoints return it.
type panelServer struct {
	ID                string                     `json:"id"`
	UUID              string                     `json:"uuid"`
	Name              string                     `json:"name"`
	Description       *string                    `json:"description"`
	Status            string                     `json:"status"`
	OwnerID           string                     `json:"ownerId"`
	NodeID            string                     `json:"nodeId"`
	LocationID        string                     `json:"locationId"`
	TemplateID        string                     `json:"templateId"`
	NetworkMode       string                     `json:"networkMode"`
	AllocatedMemoryMb int64                      `json:"allocatedMemoryMb"`
	AllocatedCpuCores int64                      `json:"allocatedCpuCores"`
	AllocatedDiskMb   int64                      `json:"allocatedDiskMb"`
	PrimaryPort       int32                      `json:"primaryPort"`
	PrimaryIP         *string                    `json:"primaryIp"`
	SuspendedAt       *string                    `json:"suspendedAt"`
	SuspensionReason  *string                    `json:"suspensionReason"`
	CreatedAt         time.Time                  `json:"createdAt"`
	Node              *panelNode                 `json:"node"`
	Template          *struct{ ID, Name string } `json:"template"`
	Connection        *panelConnection           `json:"connection"`
}

// descriptionText is the marker-bearing description, or empty.
func (s panelServer) descriptionText() string {
	if s.Description == nil {
		return ""
	}
	return *s.Description
}

// address returns the host a player connects to and the port.
//
// Three sources, in decreasing order of correctness: the computed connection block (which knows
// about bridge ports and host-network addresses), the primary ip/port pair, and finally the node's
// public address. The last is a fallback rather than an equal: a node address is right for a
// host-network server and wrong for a bridged one.
func (s panelServer) address() (string, int32) {
	if s.Connection != nil {
		for _, host := range []string{s.Connection.Host, s.Connection.HostNetworkIP, s.Connection.AssignedIP, s.Connection.NodeIP} {
			if strings.TrimSpace(host) != "" {
				port := s.Connection.Port
				if port == 0 {
					port = s.PrimaryPort
				}
				return strings.TrimSpace(host), port
			}
		}
	}
	if s.PrimaryIP != nil && strings.TrimSpace(*s.PrimaryIP) != "" {
		return strings.TrimSpace(*s.PrimaryIP), s.PrimaryPort
	}
	if s.Node != nil && strings.TrimSpace(s.Node.PublicAddress) != "" {
		return strings.TrimSpace(s.Node.PublicAddress), s.PrimaryPort
	}
	return "", s.PrimaryPort
}

// panelAllocation is a port the panel can assign to a server.
type panelAllocation struct {
	ID       string  `json:"id"`
	NodeID   string  `json:"nodeId"`
	ServerID *string `json:"serverId"`
	IP       string  `json:"ip"`
	Port     int32   `json:"port"`
	Alias    *string `json:"alias"`
}

// free reports whether the allocation is unassigned.
func (a panelAllocation) free() bool { return a.ServerID == nil || *a.ServerID == "" }

// envelope is the panel's reply wrapper for reads.
type envelope[T any] struct {
	Success bool   `json:"success"`
	Data    T      `json:"data"`
	Error   string `json:"error"`
	Code    string `json:"code"`
}

// do performs one request and decodes the reply into out (which may be nil).
func (c *panelClient) do(ctx context.Context, method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("catalystctl: encode request: %w", err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+"/api"+path, reader)
	if err != nil {
		return fmt.Errorf("catalystctl: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("catalystctl: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return fmt.Errorf("catalystctl: read reply: %w", err)
	}

	if resp.StatusCode >= 400 {
		pe := &panelError{status: resp.StatusCode}
		var parsed struct {
			Error string `json:"error"`
			Code  string `json:"code"`
		}
		if json.Unmarshal(raw, &parsed) == nil {
			pe.code, pe.message = parsed.Code, parsed.Error
		}
		if pe.message == "" {
			pe.message = excerpt(raw)
		}
		return pe
	}

	if out == nil || resp.StatusCode == http.StatusNoContent || len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	return json.Unmarshal(raw, out)
}

// probeRoot reports whether the configured base is actually the panel's API.
//
// One cheap call, only on a 404 that did not carry the panel's own "server not found" code. It is what makes
// the difference between "the resource is gone" and "we are talking to something that is not the panel"
// observable rather than guessed.
func (c *panelClient) probeRoot(ctx context.Context) bool {
	var out envelope[[]panelLocation]
	return c.do(ctx, http.MethodGet, "/locations", nil, &out) == nil
}

// getServer reads one server. A missing server is a *panelError whose notFound() is true.
func (c *panelClient) getServer(ctx context.Context, id string) (panelServer, error) {
	var out envelope[panelServer]
	if err := c.do(ctx, http.MethodGet, "/servers/"+url.PathEscape(id), nil, &out); err != nil {
		var pe *panelError
		if errors.As(err, &pe) && pe.status == http.StatusNotFound && pe.code != "SERVER_NOT_FOUND" {
			// A 404 with no code: either the panel does not know this server, or this is not the panel.
			if c.probeRoot(ctx) {
				pe.resourceMissing = true
				return panelServer{}, pe
			}
			return panelServer{}, &panelError{
				status: http.StatusNotFound,
				code:   "PANEL_API_NOT_FOUND",
				message: fmt.Sprintf("%s does not answer as a Catalyst panel API; "+
					"check the panel URL (it should be the origin, without /api)", c.base),
			}
		}
		return panelServer{}, err
	}
	if out.Data.ID == "" {
		return panelServer{}, &panelError{status: http.StatusNotFound, code: "SERVER_NOT_FOUND"}
	}
	return out.Data, nil
}

// listServers walks the panel's server list, one page at a time.
//
// The panel has no filter by an external key, and its `search` parameter is accepted but ignored
// (verified against the live panel: a nonsense term returns every server). So a lookup has to
// read the list and match locally, and the page bound is what stops a shared panel with ten
// thousand servers from turning one reconciliation into a denial of service.
func (c *panelClient) listServers(ctx context.Context, each func(panelServer) bool) error {
	const pageSize = 100
	for page := 0; page < c.pageLimit; page++ {
		var out envelope[[]panelServer]
		path := fmt.Sprintf("/servers?limit=%d&offset=%d", pageSize, page*pageSize)
		if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
			return err
		}
		for _, srv := range out.Data {
			if each(srv) {
				return nil
			}
		}
		// An *empty* page is the only reliable end-of-list signal. Comparing against the requested
		// page size is not: a panel that caps its own page size below what was asked for would look
		// like the end of the list, and concluding that is how a lookup reports "not found" for a
		// server that exists on page two.
		if len(out.Data) == 0 {
			return nil
		}
	}
	// Ran out of pages. Saying so is the honest answer: "I scanned as far as I am allowed and did
	// not find it" must not be reported as "it does not exist", because the core treats that as
	// authoritative and would provision a duplicate.
	return fmt.Errorf("%w: scanned %d pages without finding the operation key", errScanExhausted, c.pageLimit)
}

// serverStats is the panel's metrics answer for one server.
type serverStats struct {
	CPUPercent        float64   `json:"cpuPercent"`
	MemoryUsageMb     flexInt64 `json:"memoryUsageMb"`
	MemoryAllocatedMb flexInt64 `json:"memoryAllocatedMb"`
	MemoryPercentage  float64   `json:"memoryPercentage"`
	DiskUsageMb       flexInt64 `json:"diskUsageMb"`
	DiskIoMb          flexInt64 `json:"diskIoMb"`
	NetworkRxBytes    flexInt64 `json:"networkRxBytes"`
	NetworkTxBytes    flexInt64 `json:"networkTxBytes"`
	Timestamp         *string   `json:"timestamp"`
	// Message is set instead of the numbers when the panel has no sample yet ("No metrics
	// available yet"). Its presence is how the adapter tells "idle" from "unknown".
	Message string `json:"message"`
}

// stats reads the latest metrics sample.
//
// A panel with no sample answers 200 and a message rather than an error, so this reports ok=false
// instead of inventing zeros. A zeroed figure on a page is a wrong statement; a dash is not.
func (c *panelClient) stats(ctx context.Context, id string) (serverStats, bool, error) {
	var out envelope[serverStats]
	if err := c.do(ctx, http.MethodGet, "/servers/"+url.PathEscape(id)+"/stats", nil, &out); err != nil {
		// Metrics are advisory. A server whose stats cannot be read is still a server the
		// customer may start, so a metrics failure is reported as "no sample" and never as a
		// failure of the whole call.
		if pe, ok := err.(*panelError); ok && pe.notFound() {
			return serverStats{}, false, nil
		}
		return serverStats{}, false, nil
	}
	if out.Data.Message != "" {
		return serverStats{}, false, nil
	}
	return out.Data, true, nil
}

// freeAllocation returns an unassigned port on the node, or an error naming the remedy.
func (c *panelClient) freeAllocation(ctx context.Context, nodeID string, used map[int32]struct{}) (panelAllocation, error) {
	var out envelope[[]panelAllocation]
	if err := c.do(ctx, http.MethodGet, "/nodes/"+url.PathEscape(nodeID)+"/allocations", nil, &out); err != nil {
		return panelAllocation{}, err
	}
	for _, alloc := range out.Data {
		if !alloc.free() {
			continue
		}
		// An allocation with no `serverId` is not proof the port is unused. A server created before the
		// allocation existed — or one whose allocation was reset — holds its port through `primaryPort`
		// alone, and the panel refuses a create on that port with PORT_ALREADY_IN_USE. Learned against a
		// live panel, where port 25566 was listed as free while a bridge server was listening on it.
		if _, taken := used[alloc.Port]; taken {
			continue
		}
		return alloc, nil
	}
	return panelAllocation{}, fmt.Errorf(
		"no usable port allocation is left on node %s; add ports on the panel's node screen", nodeID)
}

// portsInUse collects the ports the node's existing servers listen on.
//
// It is the second half of the "is this port really free" question: the allocation table answers "was this
// port handed out", and the servers answer "is something actually on it". A create needs both.
func (c *panelClient) portsInUse(ctx context.Context, nodeID string) (map[int32]struct{}, error) {
	used := map[int32]struct{}{}
	err := c.listServers(ctx, func(srv panelServer) bool {
		if srv.NodeID == nodeID && srv.PrimaryPort > 0 {
			used[srv.PrimaryPort] = struct{}{}
		}
		return false // never stop early: every page matters
	})
	if err != nil {
		return used, err
	}
	return used, nil
}

// allocationOnPort finds the free allocation carrying a specific port.
//
// A customer who asked for a port gets that port or a clear refusal. Silently handing them a
// different one would be worse than failing: the address is what they paste into their game client
// and give to their players.
func (c *panelClient) allocationOnPort(ctx context.Context, nodeID string, port int32) (panelAllocation, error) {
	var out envelope[[]panelAllocation]
	if err := c.do(ctx, http.MethodGet, "/nodes/"+url.PathEscape(nodeID)+"/allocations", nil, &out); err != nil {
		return panelAllocation{}, err
	}
	for _, alloc := range out.Data {
		if alloc.Port == port && alloc.free() {
			return alloc, nil
		}
	}
	return panelAllocation{}, fmt.Errorf(
		"port %d is not free on node %s; leave the port empty to take any free one", port, nodeID)
}

// listUsers reads the panel's accounts, filtered by the panel's own search.
//
// The search is the whole reason this is safe on a large installation: listing every account and paging
// until the address turns up would either stop early (and create a duplicate for a customer who already
// has an account) or walk a shared panel forever. The panel filters by address and username, so the
// answer is one page and the caller only has to confirm an exact match — a substring search for
// "alice@example.test" also matches "notalice@example.test", so the confirmation is not optional.
func (c *panelClient) listUsers(ctx context.Context, search string) ([]panelUser, error) {
	path := "/admin/users?limit=100"
	if search != "" {
		path += "&search=" + url.QueryEscape(search)
	}
	var out struct {
		Users []panelUser `json:"users"`
		// The admin users endpoint answers a bare object rather than the {success,data} envelope.
		Data struct {
			Users []panelUser `json:"users"`
		} `json:"data"`
	}
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	if out.Users != nil {
		return out.Users, nil
	}
	return out.Data.Users, nil
}

// createUserRequest is the POST /api/admin/users payload.
type createUserRequest struct {
	Email    string   `json:"email"`
	Username string   `json:"username"`
	Password string   `json:"password"`
	RoleIDs  []string `json:"roleIds,omitempty"`
}

// createPanelUser creates one account.
func (c *panelClient) createPanelUser(ctx context.Context, req createUserRequest) (panelUser, error) {
	// The created account comes back as a BARE object — no `success`/`data` envelope, unlike most of this
	// panel's writes. Verified against the live panel: `{"id":…, "email":…, "username":…, "roles":[]}`.
	//
	// All four shapes are accepted anyway, because getting this wrong is the worst kind of bug in this
	// flow: the account IS created, the adapter reports the create as failed, and the retry hits a 409 for
	// an account nobody has the id of. That is a stuck purchase with a stranded account on the panel, and
	// it happened here before the bare shape was understood.
	var out struct {
		panelUser
		User panelUser `json:"user"`
		Data struct {
			panelUser
			User panelUser `json:"user"`
		} `json:"data"`
	}
	if err := c.do(ctx, http.MethodPost, "/admin/users", req, &out); err != nil {
		return panelUser{}, err
	}
	for _, candidate := range []panelUser{out.panelUser, out.User, out.Data.panelUser, out.Data.User} {
		if candidate.valid() {
			return candidate, nil
		}
	}
	return panelUser{}, fmt.Errorf("the panel accepted the account request but returned no user")
}

// locations lists the panel's datacenters.
func (c *panelClient) locations(ctx context.Context) ([]panelLocation, error) {
	var out envelope[[]panelLocation]
	if err := c.do(ctx, http.MethodGet, "/locations", nil, &out); err != nil {
		return nil, err
	}
	return out.Data, nil
}

// templates lists every template the panel sells, page by page.
//
// Paging matters here in a way it does not for locations: a real installation has hundreds of game
// templates (the reference panel has 251), so a single page silently hides most of the catalogue. A
// template missing from the dropdown is a product an operator cannot sell, and — worse — a purchase that
// names one by id was refused as "does not exist on this panel" while it was on page three. That is why
// id resolution also consults the panel directly (see templateByID) instead of trusting this list.
func (c *panelClient) templates(ctx context.Context) ([]panelTemplate, error) {
	const pageSize = 100
	out := make([]panelTemplate, 0, pageSize)
	for page := 0; page < c.pageLimit; page++ {
		var resp envelope[[]panelTemplate]
		if err := c.do(ctx, http.MethodGet,
			fmt.Sprintf("/templates?limit=%d&offset=%d", pageSize, page*pageSize), nil, &resp); err != nil {
			// A first-page failure is the caller's problem; a later one leaves what was already read,
			// because a partial list is more useful than none and the detail endpoint can still resolve
			// a specific id.
			if page == 0 {
				return nil, err
			}
			return out, nil
		}
		out = append(out, resp.Data...)
		if len(resp.Data) == 0 {
			break
		}
	}
	return out, nil
}

// templateByID reads one template, which is how an id is resolved without depending on a list page.
//
// A purchase may name a template the list cannot show — because the installation has more templates
// than the adapter is willing to page through, or because the list is briefly incomplete. Asking the
// panel for that id is authoritative and cheap, and it is what makes "put this exact template on this
// product" work.
func (c *panelClient) templateByID(ctx context.Context, id string) (panelTemplate, error) {
	var out envelope[panelTemplate]
	if err := c.do(ctx, http.MethodGet, "/templates/"+url.PathEscape(id), nil, &out); err != nil {
		return panelTemplate{}, err
	}
	if out.Data.ID == "" {
		return panelTemplate{}, &panelError{status: http.StatusNotFound, code: "TEMPLATE_NOT_FOUND"}
	}
	return out.Data, nil
}

// nodes lists the panel's nodes. The endpoint is unpaginated.
func (c *panelClient) nodes(ctx context.Context) ([]panelNode, error) {
	var out envelope[[]panelNode]
	if err := c.do(ctx, http.MethodGet, "/nodes", nil, &out); err != nil {
		return nil, err
	}
	return out.Data, nil
}

// nodeStats reads a node's allocated capacity, which is what load balancing compares.
func (c *panelClient) nodeStats(ctx context.Context, nodeID string) (panelNodeStats, error) {
	var out envelope[panelNodeStats]
	if err := c.do(ctx, http.MethodGet, "/nodes/"+url.PathEscape(nodeID)+"/stats", nil, &out); err != nil {
		return panelNodeStats{}, err
	}
	return out.Data, nil
}

// createRequest is the POST /api/servers payload.
//
// Only the fields the adapter sets are modelled. Everything else is the panel's business and
// sending an empty value for it would be worse than omitting it: `networkMode: ""` is not the
// same request as no network mode at all, which is why it is a pointer.
type createRequest struct {
	Name              string            `json:"name"`
	Description       string            `json:"description,omitempty"`
	TemplateID        string            `json:"templateId"`
	NodeID            string            `json:"nodeId"`
	LocationID        string            `json:"locationId"`
	OwnerID           string            `json:"ownerId,omitempty"`
	PrimaryPort       int32             `json:"primaryPort"`
	PrimaryIP         string            `json:"primaryIp,omitempty"`
	AllocationID      string            `json:"allocationId,omitempty"`
	AllocatedMemoryMb int64             `json:"allocatedMemoryMb"`
	AllocatedCpuCores int64             `json:"allocatedCpuCores"`
	AllocatedDiskMb   int64             `json:"allocatedDiskMb"`
	NetworkMode       *string           `json:"networkMode,omitempty"`
	Environment       map[string]string `json:"environment,omitempty"`
}

func (c *panelClient) createServer(ctx context.Context, req createRequest) (panelServer, error) {
	var out envelope[panelServer]
	if err := c.do(ctx, http.MethodPost, "/servers", req, &out); err != nil {
		return panelServer{}, err
	}
	if out.Data.ID == "" {
		return panelServer{}, fmt.Errorf("the panel accepted the request but returned no server")
	}
	return out.Data, nil
}

// power issues one power action. The panel models each action as its own path.
func (c *panelClient) power(ctx context.Context, id, action string) error {
	return c.do(ctx, http.MethodPost, "/servers/"+url.PathEscape(id)+"/"+action, nil, nil)
}

func (c *panelClient) suspend(ctx context.Context, id, reason string) error {
	body := map[string]any{}
	if reason != "" {
		body["reason"] = reason
	}
	return c.do(ctx, http.MethodPost, "/servers/"+url.PathEscape(id)+"/suspend", body, nil)
}

func (c *panelClient) unsuspend(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodPost, "/servers/"+url.PathEscape(id)+"/unsuspend", nil, nil)
}

func (c *panelClient) deleteServer(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/servers/"+url.PathEscape(id), nil, nil)
}

// excerpt shortens a response body for an error message.
func excerpt(raw []byte) string {
	s := strings.TrimSpace(string(raw))
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}
