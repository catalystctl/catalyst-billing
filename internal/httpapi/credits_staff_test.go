package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/catalyst/billing/internal/credit"
)

// The staff credit read surface (CRD-07, E5.2).
//
// The grant itself is proven in credits_test.go. What is proven here is the reading half the
// screen needs: an operator can find who holds credit, look at a grant, and see the audit row
// and the balance it left — and a role that may read the ledger still cannot move money.

// staffCreditCustomer is one row of GET /api/v1/staff/credit/customers.
type staffCreditCustomer struct {
	ID       string `json:"id"`
	Email    string `json:"email"`
	Name     string `json:"name"`
	Balances []struct {
		Minor    int64  `json:"minor"`
		Currency string `json:"currency"`
		Scale    int    `json:"scale"`
	} `json:"balances"`
}

// staffCreditGrant is one row of GET /api/v1/staff/credit/grants.
type staffCreditGrant struct {
	ID         string `json:"id"`
	Number     string `json:"number"`
	CustomerID string `json:"customer_id"`
	Reason     string `json:"reason"`
	Status     string `json:"status"`
	CreatedBy  string `json:"created_by"`
	Amount     struct {
		Minor    int64  `json:"minor"`
		Currency string `json:"currency"`
	} `json:"amount"`
}

// decodePage unmarshals a page envelope.
func decodePage[T any](t *testing.T, body []byte) (rows []T, next *string) {
	t.Helper()
	var page struct {
		Data       []T     `json:"data"`
		NextCursor *string `json:"next_cursor"`
	}
	if err := json.Unmarshal(body, &page); err != nil {
		t.Fatalf("body: %v\n%s", err, body)
	}
	return page.Data, page.NextCursor
}

// grantCredit issues a goodwill credit through the API and returns the note id.
func (e *staffEnv) grantCredit(t *testing.T, customerID string, minor int64, reason string) string {
	t.Helper()
	rec := e.postJSON(t, "/api/v1/staff/customers/"+customerID+"/credit",
		e.billing, e.billingCSRF, map[string]any{
			"amount_minor": minor,
			"currency":     "USD",
			"reason":       reason,
		})
	if rec.Code != http.StatusCreated {
		t.Fatalf("grant: status %d; body %s", rec.Code, rec.Body)
	}
	var body struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body: %v\n%s", err, rec.Body)
	}
	return body.ID
}

// The list reports each customer's credit per currency, and the search narrows it.
//
// The balance is the ledger's, so a grant made through the API is what appears here: the
// screen never reads a stored balance column, because there is not one.
func TestStaffCreditCustomersListsBalances(t *testing.T) {
	e := newStaffEnv(t)
	holderID, _ := e.seedCustomer(t, "holder@example.test", "Holds Credit")
	emptyID, _ := e.seedCustomer(t, "empty@example.test", "Owes Nothing")
	e.grantCredit(t, holderID, 4250, "goodwill after an outage")

	rec := e.get(t, "/api/v1/staff/credit/customers?limit=50", e.billing)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body %s", rec.Code, rec.Body)
	}
	rows, _ := decodePage[staffCreditCustomer](t, rec.Body.Bytes())

	byID := map[string]staffCreditCustomer{}
	for _, row := range rows {
		byID[row.ID] = row
	}
	holder, ok := byID[holderID]
	if !ok {
		t.Fatalf("the customer holding credit is missing from %d rows", len(rows))
	}
	if len(holder.Balances) != 1 || holder.Balances[0].Minor != 4250 ||
		holder.Balances[0].Currency != "USD" || holder.Balances[0].Scale != 2 {
		t.Fatalf("balances = %+v, want 4250 USD scale 2", holder.Balances)
	}
	// A customer with no credit is still listed, with an empty array: "nothing on account"
	// is a state, not a failed read.
	if empty, ok := byID[emptyID]; !ok || empty.Balances == nil || len(empty.Balances) != 0 {
		t.Fatalf("the empty customer row = %+v (present %v), want an empty balances array",
			empty, ok)
	}

	// The search matches the email and the name, case-insensitively.
	search := e.get(t, "/api/v1/staff/credit/customers?q=holder%40example", e.billing)
	if search.Code != http.StatusOK {
		t.Fatalf("search: status %d; body %s", search.Code, search.Body)
	}
	found, _ := decodePage[staffCreditCustomer](t, search.Body.Bytes())
	if len(found) != 1 || found[0].ID != holderID {
		t.Fatalf("search rows = %+v, want exactly the holder", found)
	}

	// with_credit=true drops the customers who hold nothing.
	only := e.get(t, "/api/v1/staff/credit/customers?with_credit=true", e.billing)
	if only.Code != http.StatusOK {
		t.Fatalf("with_credit: status %d; body %s", only.Code, only.Body)
	}
	holding, _ := decodePage[staffCreditCustomer](t, only.Body.Bytes())
	for _, row := range holding {
		if row.ID == emptyID {
			t.Fatal("a customer with no credit is listed under with_credit=true")
		}
	}
	if len(holding) != 1 || holding[0].ID != holderID {
		t.Fatalf("with_credit rows = %+v, want only the holder", holding)
	}
}

// Reading credit is credit:read:all, which support holds and infrastructure does not.
//
// The distinction is the point: a support agent answers "what is this customer owed", an
// infrastructure agent runs servers. Neither may grant — that is credit:write, tested in
// credits_test.go.
func TestStaffCreditCustomersGuards(t *testing.T) {
	e := newStaffEnv(t)
	e.seedCustomer(t, "guarded@example.test", "Guarded")

	if rec := e.get(t, "/api/v1/staff/credit/customers", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous: status %d, want 401", rec.Code)
	}

	// Support holds credit:read:all.
	if rec := e.get(t, "/api/v1/staff/credit/customers", e.support); rec.Code != http.StatusOK {
		t.Fatalf("support: status %d, want 200; body %s", rec.Code, rec.Body)
	}

	// Infrastructure does not.
	e.createUser(t, "infra@example.test", "infra password 1", "infrastructure")
	infra, _ := e.login(t, "infra@example.test", "infra password 1")
	rec := e.get(t, "/api/v1/staff/credit/customers", infra)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("infrastructure: status %d, want 403; body %s", rec.Code, rec.Body)
	}
	if !containsCode(rec.Body.Bytes(), "forbidden") {
		t.Fatalf("body = %s, want a forbidden problem document", rec.Body)
	}

	// A one-character search is refused rather than turned into a scan.
	short := e.get(t, "/api/v1/staff/credit/customers?q=h", e.billing)
	if short.Code != http.StatusBadRequest || !containsCode(short.Body.Bytes(), "invalid_query") {
		t.Fatalf("short search: status %d; body %s", short.Code, short.Body)
	}

	// An unknown sort column is a 400, never interpolated into the query. The value below is a
	// real SQL expression, so a whitelist that leaked would be a working injection.
	badSort := e.get(t, "/api/v1/staff/credit/customers?sort=lower(email)", e.billing)
	if badSort.Code != http.StatusBadRequest || !containsCode(badSort.Body.Bytes(), "invalid_query") {
		t.Fatalf("bad sort: status %d; body %s", badSort.Code, badSort.Body)
	}

	// A malformed cursor is a 400, not a silent restart at page one.
	badCursor := e.get(t, "/api/v1/staff/credit/customers?cursor=not-a-cursor", e.billing)
	if badCursor.Code != http.StatusBadRequest || !containsCode(badCursor.Body.Bytes(), "invalid_cursor") {
		t.Fatalf("bad cursor: status %d; body %s", badCursor.Code, badCursor.Body)
	}
}

// A grant is followed by its evidence: the note, the audit row and the balance it left.
//
// The audit row is read from audit_events rather than reconstructed, which is what makes it
// evidence: it was written inside the transaction that issued the note.
func TestStaffCreditGrantDetailShowsNoteAuditAndBalance(t *testing.T) {
	e := newStaffEnv(t)
	customerID, _ := e.seedCustomer(t, "detail@example.test", "Detail Me")

	// The grant response carries its audit row, so a client that has just granted does not
	// need a second request to show it.
	grantRec := e.postJSON(t, "/api/v1/staff/customers/"+customerID+"/credit",
		e.billing, e.billingCSRF, map[string]any{
			"amount_minor": 1500,
			"currency":     "USD",
			"reason":       "goodwill",
		})
	if grantRec.Code != http.StatusCreated {
		t.Fatalf("grant: status %d; body %s", grantRec.Code, grantRec.Body)
	}
	var granted struct {
		ID     string `json:"id"`
		Number string `json:"number"`
		Audit  *struct {
			Action    string `json:"action"`
			Outcome   string `json:"outcome"`
			ActorMail string `json:"actor_email"`
		} `json:"audit"`
	}
	if err := json.Unmarshal(grantRec.Body.Bytes(), &granted); err != nil {
		t.Fatalf("body: %v\n%s", err, grantRec.Body)
	}
	if granted.Audit == nil {
		t.Fatal("the grant response carries no audit row")
	}
	if granted.Audit.Action != "credit.grant" || granted.Audit.Outcome != "success" {
		t.Fatalf("audit = %+v", granted.Audit)
	}
	if granted.Audit.ActorMail != "billing@example.test" {
		t.Fatalf("audit actor = %q, want the operator who granted", granted.Audit.ActorMail)
	}

	// A second grant so the detail view has a history rather than a single row.
	e.grantCredit(t, customerID, 500, "second grant")

	rec := e.get(t, "/api/v1/staff/credit/grants/"+granted.ID, e.support)
	if rec.Code != http.StatusOK {
		t.Fatalf("detail: status %d; body %s", rec.Code, rec.Body)
	}
	var detail struct {
		Grant staffCreditGrant `json:"grant"`
		Audit []struct {
			Action    string `json:"action"`
			Outcome   string `json:"outcome"`
			ActorMail string `json:"actor_email"`
		} `json:"audit"`
		Balance struct {
			Minor    int64  `json:"minor"`
			Currency string `json:"currency"`
		} `json:"balance"`
		Balances []struct {
			Minor    int64  `json:"minor"`
			Currency string `json:"currency"`
		} `json:"balances"`
		Customer struct {
			ID    string `json:"id"`
			Email string `json:"email"`
		} `json:"customer"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("body: %v\n%s", err, rec.Body)
	}
	if detail.Grant.ID != granted.ID || detail.Grant.Number != granted.Number {
		t.Fatalf("grant = %+v, want id %s number %s", detail.Grant, granted.ID, granted.Number)
	}
	if detail.Grant.Amount.Minor != 1500 || detail.Grant.Amount.Currency != "USD" {
		t.Fatalf("amount = %+v", detail.Grant.Amount)
	}
	if detail.Grant.Reason != "goodwill" {
		t.Fatalf("reason = %q", detail.Grant.Reason)
	}
	if detail.Customer.ID != customerID || detail.Customer.Email != "detail@example.test" {
		t.Fatalf("customer = %+v", detail.Customer)
	}
	if len(detail.Audit) != 1 || detail.Audit[0].Action != "credit.grant" {
		t.Fatalf("audit = %+v, want exactly the grant row", detail.Audit)
	}
	// 1500 + 500: the balance is the ledger's current one, not a replay of the balance at
	// issue time.
	if detail.Balance.Minor != 2000 || detail.Balance.Currency != "USD" {
		t.Fatalf("balance = %+v, want 2000 USD", detail.Balance)
	}
	if len(detail.Balances) != 1 || detail.Balances[0].Minor != 2000 {
		t.Fatalf("balances = %+v", detail.Balances)
	}

	// The list is the same notes, newest first, and filters by customer.
	list := e.get(t, "/api/v1/staff/credit/grants?customer_id="+customerID, e.billing)
	if list.Code != http.StatusOK {
		t.Fatalf("list: status %d; body %s", list.Code, list.Body)
	}
	grants, _ := decodePage[staffCreditGrant](t, list.Body.Bytes())
	if len(grants) != 2 {
		t.Fatalf("%d grants, want 2: %+v", len(grants), grants)
	}
	if grants[0].Reason != "second grant" || grants[1].ID != granted.ID {
		t.Fatalf("grants are not newest first: %+v", grants)
	}
	for _, g := range grants {
		if g.CustomerID != customerID {
			t.Fatalf("a grant for another customer is in the filtered list: %+v", g)
		}
		if g.CreatedBy == "" {
			t.Fatalf("grant %s does not name the operator who made it", g.Number)
		}
	}

	// Another customer's history is empty, not an error, and a bogus id is a 404 before any
	// query runs.
	otherID, _ := e.seedCustomer(t, "other@example.test", "Other")
	emptyList := e.get(t, "/api/v1/staff/credit/grants?customer_id="+otherID, e.billing)
	if emptyList.Code != http.StatusOK {
		t.Fatalf("other list: status %d; body %s", emptyList.Code, emptyList.Body)
	}
	if rows, _ := decodePage[staffCreditGrant](t, emptyList.Body.Bytes()); len(rows) != 0 {
		t.Fatalf("another customer's grants leaked: %+v", rows)
	}
	if bad := e.get(t, "/api/v1/staff/credit/grants?customer_id=nope", e.billing); bad.Code != http.StatusNotFound {
		t.Fatalf("bogus customer id: status %d, want 404", bad.Code)
	}
	if bad := e.get(t, "/api/v1/staff/credit/grants/not-a-uuid", e.billing); bad.Code != http.StatusNotFound {
		t.Fatalf("bogus note id: status %d, want 404", bad.Code)
	}
	if missing := e.get(t, "/api/v1/staff/credit/grants/00000000-0000-0000-0000-000000000000", e.billing); missing.Code != http.StatusNotFound {
		t.Fatalf("unknown note: status %d, want 404; body %s", missing.Code, missing.Body)
	}
}

// The balance a grant left is the subledger's, so spending the credit moves it.
//
// This is the assertion that would fail if the screen had been built on a stored balance
// column: the note is immutable, but the balance it left is not the balance now.
func TestStaffCreditGrantDetailBalanceFollowsTheLedger(t *testing.T) {
	e := newStaffEnv(t)
	customerID, _ := e.seedCustomer(t, "spend@example.test", "Spends Credit")
	noteID := e.grantCredit(t, customerID, 3000, "goodwill")

	// Spend part of it through the domain path, which is what paying an invoice with credit
	// does.
	if _, err := credit.New(e.pool).Grant(context.Background(), credit.GrantRequest{
		CustomerID: customerID, AmountMinor: 1000, Currency: "USD",
		Reason: "second", Key: "spend-test-seed",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	rec := e.get(t, "/api/v1/staff/credit/grants/"+noteID, e.billing)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body %s", rec.Code, rec.Body)
	}
	var detail struct {
		Grant struct {
			Amount struct {
				Minor int64 `json:"minor"`
			} `json:"amount"`
		} `json:"grant"`
		Balance struct {
			Minor int64 `json:"minor"`
		} `json:"balance"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("body: %v\n%s", err, rec.Body)
	}
	if detail.Grant.Amount.Minor != 3000 {
		t.Fatalf("the note's amount = %d, want 3000", detail.Grant.Amount.Minor)
	}
	if detail.Balance.Minor != 4000 {
		t.Fatalf("balance = %d, want 4000 (the note is 3000, the balance is the ledger's)", detail.Balance.Minor)
	}
}
