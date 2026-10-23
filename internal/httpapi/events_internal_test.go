package httpapi

// Regression tests for a process-killing defect in the SSE writer.
//
// The broker fans one events.Event value out to every subscriber, and the Data map inside
// it is a reference: all subscribers of an event — and the publisher — share one map.
// writeSSE used to add the envelope fields (event_id, kind, at) straight into that map, so
// every subscriber's goroutine wrote to the same map while another subscriber's
// json.Marshal iterated it. The runtime does not recover from that:
//
//	fatal error: concurrent map iteration and map write
//	encoding/json.mapEncoder.encode -> httpapi.writeSSE (events.go)
//
// and the whole process died, taking unrelated requests with it. It needed only two open
// streams, which is why it showed up in a browser and never in the suite.
//
// The tests below pin both halves of the fix: the shared payload is never written to, and
// concurrent frames of one event are independent.

import (
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/catalyst/billing/internal/authn"
	"github.com/catalyst/billing/internal/events"
)

// writeSSE renders the envelope into its own copy, so the event's payload is untouched.
func TestWriteSSEDoesNotMutateTheSharedPayload(t *testing.T) {
	data := map[string]any{"invoice_id": "inv_1", "total_minor": 1200}
	ev := events.Event{ID: 7, Kind: "invoice.paid", Data: data, At: time.Now().UTC()}

	rec := httptest.NewRecorder()
	if err := writeSSE(rec, ev); err != nil {
		t.Fatalf("writeSSE: %v", err)
	}

	body := rec.Body.String()
	for _, want := range []string{`"event_id":7`, `"kind":"invoice.paid"`, `"invoice_id":"inv_1"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("frame %q is missing %s", body, want)
		}
	}
	if len(data) != 2 {
		t.Fatalf("writeSSE wrote the envelope into the shared payload: %v", data)
	}
	if _, leaked := data["event_id"]; leaked {
		t.Fatalf("the event's payload now carries event_id: %v", data)
	}

	// A second frame from the same event must be identical. Before the fix the envelope from
	// the first frame was already in the map, so this is also the "stale envelope" check.
	second := httptest.NewRecorder()
	if err := writeSSE(second, ev); err != nil {
		t.Fatalf("writeSSE (second frame): %v", err)
	}
	if got := strings.Count(second.Body.String(), `"event_id":`); got != 1 {
		t.Fatalf("second frame carries the envelope %d times: %s", got, second.Body.String())
	}
}

// Concurrent frames of one event are rendered independently. Run with -race this is a
// detected race rather than a crash before the fix; without -race it aborts the process.
func TestConcurrentFramesOfOneEventDoNotRace(t *testing.T) {
	data := map[string]any{"invoice_id": "inv_shared", "total_minor": 1200}
	ev := events.Event{ID: 11, Kind: "invoice.paid", Data: data, At: time.Now().UTC()}

	const streams, frames = 8, 50
	bodies := make([]string, streams)
	var wg sync.WaitGroup
	for i := 0; i < streams; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			var b strings.Builder
			for j := 0; j < frames; j++ {
				rec := httptest.NewRecorder()
				if err := writeSSE(rec, ev); err != nil {
					t.Errorf("writeSSE: %v", err)
					return
				}
				b.WriteString(rec.Body.String())
			}
			bodies[i] = b.String()
		}(i)
	}
	wg.Wait()

	for i, body := range bodies {
		if got := strings.Count(body, `"event_id":11`); got != frames {
			t.Fatalf("stream %d rendered the envelope %d times, want %d", i, got, frames)
		}
		if got := strings.Count(body, `"invoice_id":"inv_shared"`); got != frames {
			t.Fatalf("stream %d lost the payload: %d of %d frames", i, got, frames)
		}
	}
	if len(data) != 2 {
		t.Fatalf("the shared payload was modified: %v", data)
	}
}

// Two real streams through the broker: the event reaches both, each frame is well formed,
// and the map the publisher handed over is still exactly what it published.
func TestTwoStreamsShareOneEventWithoutCorruptingIt(t *testing.T) {
	s := testServer(t)
	first, cancelFirst := startStream(t, s, adminUser(), "")
	defer cancelFirst()
	second, cancelSecond := startStream(t, s, adminUser(), "")
	defer cancelSecond()

	data := map[string]any{"invoice_id": "inv_shared"}
	s.publish(events.Event{
		Kind:       "invoice.paid",
		Permission: string(authn.PermInvoiceReadAll),
		Data:       data,
	})

	first.waitFor(t, `"invoice_id":"inv_shared"`)
	second.waitFor(t, `"invoice_id":"inv_shared"`)

	for i, rec := range []*sseRecorder{first, second} {
		if got := strings.Count(rec.String(), `"event_id":`); got != 1 {
			t.Fatalf("stream %d rendered the envelope %d times:\n%s", i, got, rec.String())
		}
	}
	if len(data) != 1 {
		t.Fatalf("publishing mutated the payload the caller handed over: %v", data)
	}
}
