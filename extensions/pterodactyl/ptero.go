package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// pteroClient is the adapter's view of Pterodactyl's application API.
//
// Six calls: find by external id, create, get, suspend, unsuspend, delete, plus
// one to find a free allocation. That is the whole surface a provisioner needs,
// and keeping it to seven makes the HTTP layer small enough to reason about.
type pteroClient struct {
	base string
	key  string
	http *http.Client
	// pageLimit bounds the allocation scan, so a node with thousands of free
	// ports does not make the adapter read thousands of pages.
	pageLimit int
}

func newPteroClient(cfg config) *pteroClient {
	return &pteroClient{
		base:      cfg.panelURL + "/api/application",
		key:       cfg.apiKey,
		http:      &http.Client{Timeout: cfg.timeout},
		pageLimit: 5,
	}
}

// apiError is a refusal or failure from Pterodactyl.
type apiError struct {
	httpStatus int
	// Errors are Pterodactyl's own field-keyed messages, e.g.
	// {"allocation": ["The allocation id field is required."]}.
	Errors  map[string][]string `json:"errors"`
	Message string              `json:"-"`
}

func (e *apiError) Error() string {
	if e == nil {
		return "pterodactyl: unknown error"
	}
	if len(e.Errors) > 0 {
		var parts []string
		for field, msgs := range e.Errors {
			parts = append(parts, field+": "+strings.Join(msgs, "; "))
		}
		return fmt.Sprintf("pterodactyl refused the request (HTTP %d): %s",
			e.httpStatus, strings.Join(parts, ", "))
	}
	if e.Message != "" {
		return e.Message
	}
	return fmt.Sprintf("pterodactyl returned HTTP %d", e.httpStatus)
}

func (e *apiError) data() map[string]any {
	out := map[string]any{"http_status": e.httpStatus}
	if len(e.Errors) > 0 {
		out["errors"] = e.Errors
	}
	return out
}

// pteroServer is the part of a Pterodactyl server the adapter reads. It is named
// apart from `server`, which is this process's JSON-RPC state.
type pteroServer struct {
	ID         int64   `json:"id"`
	ExternalID *string `json:"external_id"`
	UUID       string  `json:"uuid"`
	Identifier string  `json:"identifier"`
	Name       string  `json:"name"`
	Status     *string `json:"status"`
	Suspended  bool    `json:"suspended"`
	NodeID     int64   `json:"node"`
	CreatedAt  string  `json:"created_at"`
	// Limits is what the panel currently enforces for the server. It is read so a replay of
	// a resize can answer "already at the requested plan" instead of claiming a second change.
	Limits pteroLimits `json:"limits"`
	// Container carries the egg's startup environment. Pterodactyl replaces that map wholesale
	// on a startup PATCH, so it is read back before writing: a resize whose only difference is
	// a config option must still be sent, and a replayed one must not be.
	Container struct {
		Environment map[string]string `json:"environment"`
	} `json:"container"`
}

// pteroLimits is the part of a server's build configuration a plan controls.
type pteroLimits struct {
	Memory int64 `json:"memory"`
	Disk   int64 `json:"disk"`
	CPU    int64 `json:"cpu"`
}

// atPlan reports whether the server already runs at the plan's size.
//
// A server built before this adapter existed reports zero limits, which does not match any
// plan and therefore resizes once — the safe direction: an unnecessary PATCH writes the same
// values it found, while a skipped one would leave the wrong size in place.
func (s pteroServer) atPlan(p plan) bool {
	return s.Limits.Memory == p.MemoryMB && s.Limits.Disk == p.DiskMB && s.Limits.CPU == p.CPUPercent
}

// hasEnvironment reports whether the server already runs with these variables.
//
// A nil map means the panel did not report one (an older version), which is answered as "no":
// writing the same values again is harmless, while assuming they match would silently drop the
// customer's options.
func (s pteroServer) hasEnvironment(env map[string]string) bool {
	if len(env) == 0 {
		return true
	}
	if s.Container.Environment == nil {
		return false
	}
	for name, value := range env {
		if s.Container.Environment[name] != value {
			return false
		}
	}
	return true
}

// identifier is what the core records as the external id. Pterodactyl's short
// `identifier` is stable and is what its own URLs use; the numeric id is an
// internal key that a database migration can change.
func (s pteroServer) identifier() string {
	if s.Identifier != "" {
		return s.Identifier
	}
	if s.UUID != "" {
		return s.UUID
	}
	return fmt.Sprintf("%d", s.ID)
}

func (s pteroServer) isSuspended() bool { return s.Suspended }

// protocolStatus maps Pterodactyl's state onto the protocol's resource states.
//
// Pterodactyl reports a *process* status (running/starting/stopping/offline) and a
// separate suspended flag. The protocol's states are about the resource's
// lifecycle, so the flag wins: a suspended server is suspended whether or not its
// process is somehow up.
func (s pteroServer) protocolStatus() string {
	if s.Suspended {
		return "suspended"
	}
	return "active"
}

func (s pteroServer) createdAt() time.Time {
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05.000000Z", "2006-01-02 15:04:05"} {
		if t, err := time.Parse(layout, s.CreatedAt); err == nil {
			return t.UTC()
		}
	}
	return time.Now().UTC()
}

// envelope is Pterodactyl's single-object response shape.
type envelope struct {
	Object     string          `json:"object"`
	Attributes json.RawMessage `json:"attributes"`
}

// listEnvelope is its list shape.
type listEnvelope struct {
	Object string `json:"object"`
	Data   []struct {
		Object     string          `json:"object"`
		Attributes json.RawMessage `json:"attributes"`
	} `json:"data"`
	Meta struct {
		Pagination struct {
			TotalPages  int `json:"total_pages"`
			CurrentPage int `json:"current_page"`
		} `json:"pagination"`
	} `json:"meta"`
}

// do performs one request and decodes the reply into out (which may be nil).
func (c *pteroClient) do(ctx context.Context, method, path string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("pterodactyl: encode request: %w", err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, reader)
	if err != nil {
		return fmt.Errorf("pterodactyl: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("pterodactyl: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return fmt.Errorf("pterodactyl: read reply: %w", err)
	}

	if resp.StatusCode >= 400 {
		ae := &apiError{httpStatus: resp.StatusCode}
		// Pterodactyl answers 204 for a delete, and its error shape is
		// {"errors": {"field": ["message"]}}.
		var parsed apiError
		if json.Unmarshal(raw, &parsed) == nil {
			ae.Errors = parsed.Errors
		}
		if len(ae.Errors) == 0 && len(bytes.TrimSpace(raw)) > 0 {
			ae.Message = fmt.Sprintf("pterodactyl returned HTTP %d: %s", resp.StatusCode, excerpt(raw))
		}
		return ae
	}

	if out == nil || resp.StatusCode == http.StatusNoContent || len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("pterodactyl: decode reply: %w", err)
	}
	return nil
}

func excerpt(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 200 {
		return s[:200] + "…"
	}
	return s
}

// findByExternalID answers the question the core's reconciliation depends on:
// does a server already exist for this operation key?
//
// It filters on Pterodactyl's own `external_id` field, which is why the key
// survives this process restarting. A nil result means Pterodactyl answered and
// there is none; an error means it could not answer, which the caller must not
// confuse with absence.
func (c *pteroClient) findByExternalID(externalID string) (*pteroServer, error) {
	if externalID == "" {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	path := "/servers?filter%5Bexternal_id%5D=" + url.QueryEscape(externalID) + "&per_page=1"
	var list listEnvelope
	if err := c.do(ctx, http.MethodGet, path, nil, &list); err != nil {
		return nil, err
	}
	for _, entry := range list.Data {
		var srv pteroServer
		if err := json.Unmarshal(entry.Attributes, &srv); err != nil {
			return nil, fmt.Errorf("pterodactyl: decode server: %w", err)
		}
		// Belt and braces: Pterodactyl's filter is exact today, but a server whose
		// external_id merely *contains* the key must never be mistaken for a
		// match — that would return someone else's server as this operation's
		// result.
		if srv.ExternalID != nil && *srv.ExternalID == externalID {
			return &srv, nil
		}
	}
	return nil, nil
}

// getServer reads one server by its short identifier or numeric id.
func (c *pteroClient) getServer(externalID string) (*pteroServer, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// The short identifier is what the core stores, but the application API's
	// server endpoints take the numeric id, so a short id is resolved through the
	// same external_id lookup the reconciler uses. This keeps one identifier in
	// the core instead of two that can disagree.
	if !isNumeric(externalID) {
		found, err := c.findByExternalID(externalID)
		if err != nil {
			return nil, err
		}
		if found != nil {
			return found, nil
		}
		// Not our external id: it may be a Pterodactyl identifier from before this
		// adapter existed, so fall through and look it up by uuid.
		return c.getServerByIdentifier(ctx, externalID)
	}
	var env envelope
	if err := c.do(ctx, http.MethodGet, "/servers/"+externalID, nil, &env); err != nil {
		return nil, err
	}
	var srv pteroServer
	if err := json.Unmarshal(env.Attributes, &srv); err != nil {
		return nil, fmt.Errorf("pterodactyl: decode server: %w", err)
	}
	return &srv, nil
}

func (c *pteroClient) getServerByIdentifier(ctx context.Context, identifier string) (*pteroServer, error) {
	var list listEnvelope
	if err := c.do(ctx, http.MethodGet, "/servers?per_page=100", nil, &list); err != nil {
		return nil, err
	}
	for _, entry := range list.Data {
		var srv pteroServer
		if err := json.Unmarshal(entry.Attributes, &srv); err != nil {
			return nil, fmt.Errorf("pterodactyl: decode server: %w", err)
		}
		if srv.Identifier == identifier || srv.UUID == identifier {
			return &srv, nil
		}
	}
	return nil, &apiError{httpStatus: 404, Message: "no such server: " + identifier}
}

func isNumeric(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// createServerRequest is the body Pterodactyl's create endpoint expects.
type createServerRequest struct {
	Name         string
	UserID       string
	Description  string
	ExternalID   string
	NestID       string
	EggID        string
	DockerImage  string
	Startup      string
	Environment  map[string]string
	MemoryMB     int64
	DiskMB       int64
	CPUPercent   int64
	AllocationID string
}

// createServer creates one server.
//
// The request is built as a map rather than a struct so the optional fields can be
// omitted: Pterodactyl rejects an explicit empty `docker_image` but is happy with
// the field absent, and an omitted field is what makes the egg's own defaults
// apply.
func (c *pteroClient) createServer(in createServerRequest) (*pteroServer, error) {
	if in.UserID == "" {
		return nil, &apiError{
			httpStatus: 422,
			Errors: map[string][]string{
				"user": {"a server must be owned by a panel user; set metadata[pterodactyl_user_id]"},
			},
		}
	}
	body := map[string]any{
		"name":        in.Name,
		"user":        in.UserID,
		"egg":         in.EggID,
		"external_id": in.ExternalID,
		"environment": in.Environment,
		"limits": map[string]any{
			"memory": in.MemoryMB,
			"swap":   0,
			"disk":   in.DiskMB,
			"io":     500,
			"cpu":    in.CPUPercent,
		},
		"feature_limits": map[string]any{
			"databases":   0,
			"allocations": 0,
			"backups":     0,
		},
		"allocation":          map[string]any{"default": numericOrString(in.AllocationID)},
		"start_on_completion": false,
	}
	if in.Description != "" {
		body["description"] = in.Description
	}
	if in.DockerImage != "" {
		body["docker_image"] = in.DockerImage
	}
	if in.Startup != "" {
		body["startup"] = in.Startup
	}
	if in.NestID != "" {
		// Pterodactyl derives the nest from the egg, so this is informational. It
		// is not sent: an unknown field is a 422 on some versions.
		_ = in.NestID
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	var env envelope
	if err := c.do(ctx, http.MethodPost, "/servers", body, &env); err != nil {
		return nil, err
	}
	var srv pteroServer
	if err := json.Unmarshal(env.Attributes, &srv); err != nil {
		return nil, fmt.Errorf("pterodactyl: decode created server: %w", err)
	}
	return &srv, nil
}

// numericOrString sends an allocation as a number when it is one. Pterodactyl
// accepts both, and the metadata it comes from is a string map.
func numericOrString(s string) any {
	if isNumeric(s) {
		var n int64
		_, _ = fmt.Sscan(s, &n)
		return n
	}
	return s
}

// updateBuild rewrites a server's limits.
//
// Pterodactyl's build endpoint takes the full set, not a delta, so every limit the plan controls
// is sent: a partial body would leave the previous value for any field omitted.
func (c *pteroClient) updateBuild(id int64, p plan) error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	body := map[string]any{
		"memory": p.MemoryMB,
		"swap":   0,
		"disk":   p.DiskMB,
		"io":     500,
		"cpu":    p.CPUPercent,
	}
	return c.do(ctx, http.MethodPatch, fmt.Sprintf("/servers/%d/build", id), body, nil)
}

// updateStartup rewrites the server's egg environment.
//
// The environment is replaced wholesale, which is why the core sends every declared option on
// every resize: a merged body would keep the value of an option the customer removed.
func (c *pteroClient) updateStartup(id int64, environment map[string]string, startup string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	body := map[string]any{"environment": environment}
	if strings.TrimSpace(startup) != "" {
		body["startup"] = startup
	}
	return c.do(ctx, http.MethodPatch, fmt.Sprintf("/servers/%d/startup", id), body, nil)
}

func (c *pteroClient) suspendServer(id int64) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return c.do(ctx, http.MethodPost, fmt.Sprintf("/servers/%d/suspend", id), nil, nil)
}

func (c *pteroClient) unsuspendServer(id int64) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return c.do(ctx, http.MethodPost, fmt.Sprintf("/servers/%d/unsuspend", id), nil, nil)
}

// deleteServer destroys a server. `force` also removes its files and its backups,
// which is what a termination means: leaving the data behind would keep the disk
// billable and the customer's data recoverable after they asked for it gone.
func (c *pteroClient) deleteServer(id int64) error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	return c.do(ctx, http.MethodDelete, fmt.Sprintf("/servers/%d?force=true", id), nil, nil)
}

// firstFreeAllocation finds a free port on a node.
//
// Pterodactyl requires a default allocation and has no "pick any" option, so this
// is the adapter's job. Pages are bounded: a node with no free port is an
// actionable error, not a reason to read ten thousand rows.
func (c *pteroClient) firstFreeAllocation(nodeID string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for page := 1; page <= c.pageLimit; page++ {
		var list listEnvelope
		path := fmt.Sprintf("/nodes/%s/allocations?per_page=100&page=%d", url.PathEscape(nodeID), page)
		if err := c.do(ctx, http.MethodGet, path, nil, &list); err != nil {
			return "", err
		}
		for _, entry := range list.Data {
			var alloc struct {
				ID       int64 `json:"id"`
				Assigned bool  `json:"assigned"`
			}
			if err := json.Unmarshal(entry.Attributes, &alloc); err != nil {
				continue
			}
			if !alloc.Assigned {
				return fmt.Sprintf("%d", alloc.ID), nil
			}
		}
		if page >= list.Meta.Pagination.TotalPages {
			break
		}
	}
	return "", &apiError{
		httpStatus: 409,
		Errors: map[string][]string{
			"allocation": {"node " + nodeID + " has no free allocation; add ports to the node or set PTERODACTYL_ALLOCATION_ID"},
		},
	}
}
