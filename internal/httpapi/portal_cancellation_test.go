package httpapi_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

// A customer's cancellation request, end to end (CAN-01…CAN-04).
//
// The screen used to post an end-of-period cancellation with no reason to a route that recorded
// no row, so a customer could not ask for an immediate stop and no operator ever saw the request.
// These tests pin the three things that make the request real:
//
//   - the type is honoured: `end_of_period` schedules, `immediate` is recorded as pending;
//   - the reason travels with it and reaches the staff queue;
//   - the operators who can act on it are told, in their own inbox.

// cancellationReply is the 201 body of a cancellation request.
type cancellationReply struct {
	ID          string `json:"id"`
	ServiceID   string `json:"service_id"`
	Type        string `json:"type"`
	Status      string `json:"status"`
	Reason      string `json:"reason"`
	EffectiveAt string `json:"effective_at"`
}

// The end-of-period request keeps the service running, records the reason, and appears in the
// queue an operator works from — with the reason, because that is the only explanation they get.
func TestPortalCancellationCapturesTheReasonAndReachesTheQueue(t *testing.T) {
	env := newStaffEnv(t)
	tenant := env.createTenant(t, "cancel-reason@example.test", "10.00")
	cookie, csrf := env.login(t, "cancel-reason@example.test", "a long enough password")

	const reason = "moving to a cheaper provider next month"
	rec := env.portalMutation(t, http.MethodPost,
		"/api/v1/portal/services/"+tenant.serviceID+"/cancellation", cookie, csrf,
		freshKey("cancel"), map[string]any{
			"type":   "end_of_period",
			"reason": reason,
		})
	if rec.Code != http.StatusCreated {
		t.Fatalf("request cancellation: status %d; body %s", rec.Code, rec.Body)
	}
	var body cancellationReply
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("cancellation body: %v\n%s", err, rec.Body)
	}
	if body.Type != "end_of_period" || body.Status != "scheduled" {
		t.Fatalf("type/status = %s/%s, want end_of_period/scheduled", body.Type, body.Status)
	}
	if body.Reason != reason {
		t.Fatalf("reason = %q, want the customer's own words", body.Reason)
	}
	if body.EffectiveAt == "" {
		t.Fatal("the response does not say when the service ends")
	}
	if _, err := time.Parse(time.RFC3339, body.EffectiveAt); err != nil {
		t.Fatalf("effective_at is not a timestamp: %v", body.EffectiveAt)
	}

	// The row is the record: it names the requester, and the schedule now stops.
	var (
		recordedReason string
		requestedBy    *string
		cancelAtEnd    bool
	)
	if err := env.pool.QueryRow(t.Context(), `
		SELECT c.reason, c.requested_by::text, sub.cancel_at_period_end
		FROM service_cancellations c
		JOIN services s ON s.id = c.service_id
		JOIN subscriptions sub ON sub.id = s.subscription_id
		WHERE c.id = $1`, body.ID).Scan(&recordedReason, &requestedBy, &cancelAtEnd); err != nil {
		t.Fatalf("read cancellation: %v", err)
	}
	if recordedReason != reason {
		t.Fatalf("stored reason = %q, want %q", recordedReason, reason)
	}
	if requestedBy == nil || *requestedBy != tenant.userID {
		t.Fatalf("requested_by = %v, want the customer's user id %s", requestedBy, tenant.userID)
	}
	if !cancelAtEnd {
		t.Fatal("the subscription still renews; the request did not reach the schedule")
	}

	// The queue is where it becomes a decision the operator can see.
	rec = env.get(t, "/api/v1/staff/cancellations", env.support)
	if rec.Code != http.StatusOK {
		t.Fatalf("staff queue: status %d; body %s", rec.Code, rec.Body)
	}
	var queue struct {
		Cancellations []struct {
			ID     string `json:"id"`
			Type   string `json:"type"`
			Status string `json:"status"`
			Reason string `json:"reason"`
		} `json:"cancellations"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &queue); err != nil {
		t.Fatalf("queue body: %v\n%s", err, rec.Body)
	}
	found := false
	for _, row := range queue.Cancellations {
		if row.ID == body.ID {
			found = true
			if row.Reason != reason || row.Type != "end_of_period" || row.Status != "scheduled" {
				t.Fatalf("the queued row disagrees with the request: %+v", row)
			}
		}
	}
	if !found {
		t.Fatalf("the customer's request is not in the queue: %s", rec.Body)
	}
}

// An immediate request is a *request*: it is recorded as pending, it does not stop the service,
// and it waits for a decision.
func TestPortalImmediateCancellationIsOnlyARequest(t *testing.T) {
	env := newStaffEnv(t)
	tenant := env.createTenant(t, "cancel-immediate@example.test", "10.00")
	cookie, csrf := env.login(t, "cancel-immediate@example.test", "a long enough password")

	rec := env.portalMutation(t, http.MethodPost,
		"/api/v1/portal/services/"+tenant.serviceID+"/cancellation", cookie, csrf,
		freshKey("cancel-now"), map[string]any{
			"type":   "immediate",
			"reason": "the box is compromised, take it down",
		})
	if rec.Code != http.StatusCreated {
		t.Fatalf("immediate request: status %d; body %s", rec.Code, rec.Body)
	}
	var body cancellationReply
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body: %v\n%s", err, rec.Body)
	}
	if body.Type != "immediate" || body.Status != "pending" {
		t.Fatalf("type/status = %s/%s, want immediate/pending", body.Type, body.Status)
	}

	// Nothing was destroyed and nothing was scheduled: an immediate request is destructive, so it
	// is an operator's decision (the parity design note, and the reason the row exists at all).
	var (
		serviceStatus string
		cancelAtEnd   bool
	)
	if err := env.pool.QueryRow(t.Context(), `
		SELECT s.status, sub.cancel_at_period_end
		FROM services s
		JOIN subscriptions sub ON sub.id = s.subscription_id
		WHERE s.id = $1`, tenant.serviceID).Scan(&serviceStatus, &cancelAtEnd); err != nil {
		t.Fatalf("read service: %v", err)
	}
	if serviceStatus != "active" {
		t.Fatalf("service status = %q, want active; an immediate request terminated it", serviceStatus)
	}
	if cancelAtEnd {
		t.Fatal("an immediate request scheduled an end-of-period cancellation")
	}
}

// The operators who can act on a cancellation are told, in their own inbox. Without this the
// request is a row nobody is looking at, which is how an immediate cancellation waits until the
// customer chases it.
func TestCancellationNotifiesTheStaffWhoCanActOnIt(t *testing.T) {
	env := newStaffEnv(t)
	tenant := env.createTenant(t, "cancel-notify@example.test", "10.00")
	cookie, csrf := env.login(t, "cancel-notify@example.test", "a long enough password")

	rec := env.portalMutation(t, http.MethodPost,
		"/api/v1/portal/services/"+tenant.serviceID+"/cancellation", cookie, csrf,
		freshKey("cancel-notify"), map[string]any{
			"type":   "immediate",
			"reason": "moving abroad",
		})
	if rec.Code != http.StatusCreated {
		t.Fatalf("request: status %d; body %s", rec.Code, rec.Body)
	}

	// Support holds service:read:all, so support is a recipient; the customer is not.
	rec = env.get(t, "/api/v1/portal/notifications", env.support)
	if rec.Code != http.StatusOK {
		t.Fatalf("support inbox: status %d; body %s", rec.Code, rec.Body)
	}
	var inbox struct {
		Data []struct {
			EventKey string `json:"event_key"`
			Title    string `json:"title"`
			Body     string `json:"body"`
			URL      string `json:"url"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &inbox); err != nil {
		t.Fatalf("inbox body: %v\n%s", err, rec.Body)
	}
	if len(inbox.Data) == 0 {
		t.Fatal("support was not told about the cancellation request")
	}
	row := inbox.Data[0]
	if row.EventKey != "service_cancellation_received" {
		t.Fatalf("event key = %q, want service_cancellation_received", row.EventKey)
	}
	if row.URL == "" {
		t.Fatal("the notification has no link, so the operator has nowhere to go")
	}
	if row.Body == "" || !contains(row.Body, "moving abroad") {
		t.Fatalf("the notification does not carry the customer's reason: %q", row.Body)
	}

	// The customer's own inbox gets the acknowledgement mail's in-app row, not the staff one: a
	// customer must not be shown the queue's working note.
	rec = env.get(t, "/api/v1/portal/notifications", cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("customer inbox: status %d; body %s", rec.Code, rec.Body)
	}
	var customerInbox struct {
		Data []struct {
			Title string `json:"title"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &customerInbox); err != nil {
		t.Fatalf("customer inbox body: %v\n%s", err, rec.Body)
	}
	for _, item := range customerInbox.Data {
		if item.Title == "Cancellation requested" {
			t.Fatal("the customer received the staff queue's notification")
		}
	}
}

// contains is a substring check, kept local so the assertion reads as prose.
func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
