package httpapi_test

import (
	"os"
	"strings"
	"testing"
)

// The customer dashboard's ticket widget (TKT-14).
//
// The frontend is not compiled by `go test`, so this reads the view source the way the export
// guard does: it is a check that the widget still exists and still asks the customer-scoped
// ticket endpoint, not a substitute for the browser harness. It fails if the card is dropped in
// a refactor, which is exactly the gap the parity row recorded — a customer could reach tickets
// from the navigation and nowhere on the page that answers "what do I owe".
func TestCustomerDashboardShowsATicketWidget(t *testing.T) {
	source, err := os.ReadFile("../../web/src/views/portal.ts")
	if err != nil {
		t.Fatalf("read the portal view: %v", err)
	}
	view := string(source)

	if !strings.Contains(view, `"/api/v1/portal/tickets?limit=`) {
		t.Fatal("the customer dashboard never reads /api/v1/portal/tickets, so the ticket widget cannot render")
	}
	// The card must link into the thread and into the new-ticket form; a widget that only lists
	// subjects is a dead end.
	if !strings.Contains(view, "ticketHref(") {
		t.Fatal("the ticket widget does not link to a ticket")
	}
	if !strings.Contains(view, `"/tickets/new"`) {
		t.Fatal("the ticket widget offers no way to open a new ticket")
	}
	// A staff reply moves a ticket to `replied`, so that is what the widget must surface first.
	if !strings.Contains(view, `row.status === "replied"`) {
		t.Fatal("the ticket widget does not surface the tickets waiting for the customer")
	}
}
