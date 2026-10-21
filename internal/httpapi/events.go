package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/catalyst/billing/internal/events"
)

// Server-sent events.
//
// Why SSE and not WebSocket: the flow is one-directional (the server reports committed
// state changes), it works through every proxy that handles plain HTTP, and it degrades
// to a stream of text lines that a shell can read with curl. A WebSocket would add a
// protocol upgrade, a framing layer and a second authentication path for no gain.
//
// The rules the handler enforces:
//
//   - Authentication is the session, exactly like every other route. There is no token in
//     the query string, because a URL ends up in logs and referrers.
//   - Filtering happens here, per event, against the caller's own identity: a customer
//     only ever sees events carrying their own customer id, and staff only see events
//     whose permission they hold. A subscriber does not choose what it receives.
//   - `Last-Event-ID` resumes after the given id. Nothing older is replayed; the client is
//     expected to re-read state on reconnect, which is cheaper and cannot go stale.
//   - A heartbeat comment every 20 seconds keeps an idle proxy from closing the
//     connection, which is the most common way an SSE feed "randomly stops".

// sseHeartbeat is the interval between keep-alive comments.
const sseHeartbeat = 20 * time.Second

// handleEvents streams committed state changes to the caller.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		// Only possible behind a wrapper that does not implement Flusher. Saying so beats
		// buffering the whole stream and delivering it at the end, which is not a stream.
		writeProblem(w, http.StatusInternalServerError, "streaming_unsupported",
			"this server cannot stream responses")
		return
	}
	if s.events == nil {
		writeProblem(w, http.StatusServiceUnavailable, "events_unavailable",
			"event streaming is not configured")
		return
	}

	user, _ := userFrom(r.Context())

	// What may this caller see? Resolve it once, before the first byte: doing it per event
	// would mean a permission change mid-stream silently changes the filter.
	customerID := ""
	if s.portal != nil {
		if id, err := s.portal.CustomerIDFor(r.Context(), user.ID); err == nil {
			customerID = id
		}
	}
	staff := user.IsStaff()
	var perms map[string]struct{}
	if staff && s.auth != nil && s.auth.Authz != nil {
		perms = make(map[string]struct{})
		for p := range s.auth.Authz.Permissions(user) {
			perms[string(p)] = struct{}{}
		}
	}

	lastID := int64(0)
	if raw := strings.TrimSpace(r.Header.Get("Last-Event-ID")); raw != "" {
		if n, err := strconv.ParseInt(raw, 10, 64); err == nil && n > 0 {
			lastID = n
		}
	}

	sub := s.events.Subscribe(64)
	defer sub.Close()

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	// Defeats buffering by a reverse proxy that would otherwise hold the stream.
	h.Set("X-Accel-Buffering", "no")
	h.Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	// Tell the client how long to wait before reconnecting, and that we are alive.
	fmt.Fprintf(w, "retry: 3000\n\n")
	fmt.Fprintf(w, ": connected\n\n")
	flusher.Flush()

	heartbeat := time.NewTicker(sseHeartbeat)
	defer heartbeat.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			if _, err := fmt.Fprintf(w, ": heartbeat %d\n\n", time.Now().UTC().Unix()); err != nil {
				return
			}
			flusher.Flush()
		case ev, ok := <-sub.Events():
			if !ok {
				// The broker dropped this subscriber (slow consumer). Ending the stream
				// makes the client reconnect, which is the correct recovery.
				return
			}
			if ev.ID <= lastID {
				continue
			}
			if !visibleTo(ev, customerID, staff, perms) {
				continue
			}
			if err := writeSSE(w, ev); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// visibleTo applies the per-caller filter.
func visibleTo(ev events.Event, customerID string, staff bool, perms map[string]struct{}) bool {
	if ev.CustomerID != "" {
		// A customer-scoped event is visible to that customer, and to staff holding the
		// permission for it (an operator watching a service update).
		if customerID != "" && ev.CustomerID == customerID {
			return true
		}
		if !staff {
			return false
		}
		if ev.Permission == "" {
			return true
		}
		_, ok := perms[ev.Permission]
		return ok
	}
	// Staff-only event.
	if !staff {
		return false
	}
	if ev.Permission == "" {
		return true
	}
	_, ok := perms[ev.Permission]
	return ok
}

// writeSSE renders one event frame.
func writeSSE(w http.ResponseWriter, ev events.Event) error {
	payload := ev.Data
	if payload == nil {
		payload = map[string]any{}
	}
	// The envelope fields travel with the payload so a client does not have to correlate
	// the SSE id with the body.
	payload["event_id"] = ev.ID
	payload["kind"] = ev.Kind
	if !ev.At.IsZero() {
		payload["at"] = ev.At.UTC()
	}
	body, err := json.Marshal(payload)
	if err != nil {
		// An unserialisable payload is a programming error, not a client error. Skipping
		// the event is better than closing the stream for everyone.
		return nil
	}
	if _, err := fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", ev.ID, ev.Kind, body); err != nil {
		return err
	}
	return nil
}
