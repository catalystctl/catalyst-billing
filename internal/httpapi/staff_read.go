package httpapi

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/catalyst/billing/internal/authn"
	"github.com/catalyst/billing/internal/money"
	"github.com/catalyst/billing/internal/paginate"
	"github.com/catalyst/billing/internal/provisioning"
	"github.com/catalyst/billing/internal/staff"
)

// The staff read surface (Gate A).
//
// Every handler here is thin: it parses the query, calls one method on the read service, and
// renders the result. That is deliberate. The interesting logic is in internal/staff (which
// query, which aggregate, which currency) and in the database (which constraint). A handler
// that computed a total would be a second implementation of a rule the ledger already
// enforces.

func init() {
	RegisterRoutes("40-staff-read", registerStaffReads)
}

// staffService returns the read service, constructing it on first use.
func (s *Server) staffService() *staff.Service {
	s.staffMu.Lock()
	defer s.staffMu.Unlock()
	if s.staff == nil {
		s.staff = staff.New(s.pool)
	}
	return s.staff
}

func registerStaffReads(r *router) {
	// Customers.
	r.staffGet("/api/v1/staff/customers", authn.PermCustomerReadAll, r.s.handleStaffCustomers)
	r.staffGet("/api/v1/staff/customers/{id}", authn.PermCustomerReadAll, r.s.handleStaffCustomer)

	// Invoices.
	r.staffGet("/api/v1/staff/invoices", authn.PermInvoiceReadAll, r.s.handleStaffInvoices)
	r.staffGet("/api/v1/staff/invoices/{id}", authn.PermInvoiceReadAll, r.s.handleStaffInvoice)

	// Services and their eligibility.
	r.staffGet("/api/v1/staff/services", authn.PermServiceReadAll, r.s.handleStaffServices)
	r.staffGet("/api/v1/staff/services/{id}", authn.PermServiceReadAll, r.s.handleStaffService)
	r.staffGet("/api/v1/staff/services/{id}/suspension-eligibility",
		authn.PermServiceReadAll, r.s.handleStaffEligibility)

	// Orders, payments, the operation queue and the inbox.
	r.staffGet("/api/v1/staff/orders", authn.PermInvoiceReadAll, r.s.handleStaffOrders)
	r.staffGet("/api/v1/staff/orders/{id}", authn.PermInvoiceReadAll, r.s.handleStaffOrder)
	r.staffGet("/api/v1/staff/payments", authn.PermPaymentReconcile, r.s.handleStaffPayments)
	r.staffGet("/api/v1/staff/payments/{id}", authn.PermPaymentReconcile, r.s.handleStaffPayment)
	r.staffGet("/api/v1/staff/operations", authn.PermServiceReadAll, r.s.handleStaffOperations)
	r.staffGet("/api/v1/staff/webhooks", authn.PermPaymentReconcile, r.s.handleStaffWebhooks)

}

// pageParams parses the pagination query, writing a response on failure.
func (s *Server) pageParams(w http.ResponseWriter, r *http.Request) (limit int, cursor string, ok bool) {
	limit, cursor, err := ParsePage(r)
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_cursor",
			"the pagination parameters are invalid")
		return 0, "", false
	}
	return limit, cursor, true
}

// timeParam parses an RFC3339 query parameter.
func (s *Server) timeParam(w http.ResponseWriter, r *http.Request, name string) (*time.Time, bool) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return nil, true
	}
	when, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_time", name+" must be an RFC3339 timestamp")
		return nil, false
	}
	return &when, true
}

// boolParam parses a true/false query parameter.
func (s *Server) boolParam(w http.ResponseWriter, r *http.Request, name string) (*bool, bool) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return nil, true
	}
	v, err := strconv.ParseBool(raw)
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_parameter", name+" must be true or false")
		return nil, false
	}
	return &v, true
}

// staffError maps a read error to a response.
func (s *Server) staffError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, staff.ErrNotFound):
		writeProblem(w, http.StatusNotFound, "not_found", "no such record")
	case errors.Is(err, staff.ErrInvalidQuery):
		// The message is safe to return: it names a parameter the caller sent, never
		// anything from the database.
		writeProblem(w, http.StatusBadRequest, "invalid_query", err.Error())
	case errors.Is(err, paginate.ErrInvalidCursor):
		writeProblem(w, http.StatusBadRequest, "invalid_cursor", "the cursor is invalid")
	default:
		s.cfg.Logger.ErrorContext(r.Context(), "staff read failed",
			"path", r.URL.Path, "error", err)
		writeProblem(w, http.StatusInternalServerError, "internal_error",
			"the request could not be completed")
	}
}

// amountMap renders one per-currency amount.
func amountMap(a staff.Amount) map[string]any {
	return map[string]any{
		"minor":    a.Minor,
		"currency": a.Currency.Code(),
		"scale":    int(a.Currency.Scale()),
		"display":  a.Currency.Code() + " " + formatMinor(a.Minor, int(a.Currency.Scale())),
	}
}

// formatMinor renders minor units with an explicit scale, without floating point.
func formatMinor(minor int64, scale int) string {
	negative := minor < 0
	if negative {
		minor = -minor
	}
	digits := strconv.FormatInt(minor, 10)
	if scale > 0 {
		for len(digits) <= scale {
			digits = "0" + digits
		}
		digits = digits[:len(digits)-scale] + "." + digits[len(digits)-scale:]
	}
	if negative {
		return "-" + digits
	}
	return digits
}

func amountsMap(list []staff.Amount) []map[string]any {
	out := make([]map[string]any, 0, len(list))
	for _, a := range list {
		out = append(out, amountMap(a))
	}
	return out
}

// timeOrNil renders a nullable timestamp.
func timeOrNil(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UTC()
}

// nextCursor renders a cursor field, which is null on the last page.
func nextCursor(cursor string) *string {
	if cursor == "" {
		return nil
	}
	return &cursor
}

// ------------------------------------------------------------------ customers --

func (s *Server) handleStaffCustomers(w http.ResponseWriter, r *http.Request) {
	limit, cursor, ok := s.pageParams(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()

	filter := staff.CustomerFilter{
		Query: q.Get("q"),
		Sort:  q.Get("sort"),
	}
	if q.Get("direction") == "asc" {
		filter.Direction = staff.SortAsc
	}

	rows, next, err := s.staffService().Customers(r.Context(), filter, limit, cursor)
	if err != nil {
		s.staffError(w, r, err)
		return
	}

	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		out = append(out, map[string]any{
			"id":            row.ID,
			"email":         row.Email,
			"name":          row.Name,
			"created_at":    row.CreatedAt.UTC(),
			"open_invoices": row.OpenInvoices,
			"outstanding":   amountsMap(row.Outstanding),
			"services":      row.Services,
			"has_login":     row.HasLogin,
		})
	}
	writePage(w, Page[map[string]any]{Data: out, NextCursor: nextCursor(next)})
}

func (s *Server) handleStaffCustomer(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validUUID(id) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such record")
		return
	}

	detail, err := s.staffService().Customer(r.Context(), id)
	if err != nil {
		s.staffError(w, r, err)
		return
	}

	accounts := make([]map[string]any, 0, len(detail.Accounts))
	for _, a := range detail.Accounts {
		accounts = append(accounts, map[string]any{
			"user_id":      a.UserID,
			"email":        a.Email,
			"role":         a.Role,
			"last_seen_at": timeOrNil(a.LastSeenAt),
		})
	}

	subscriptions := make([]map[string]any, 0, len(detail.Subscriptions))
	for _, sub := range detail.Subscriptions {
		subscriptions = append(subscriptions, subscriptionMap(sub))
	}

	services := make([]map[string]any, 0, len(detail.Services))
	for _, svc := range detail.Services {
		services = append(services, serviceMap(svc))
	}

	invoices := make([]map[string]any, 0, len(detail.Invoices))
	for _, inv := range detail.Invoices {
		invoices = append(invoices, invoiceMap(inv))
	}

	holds := make([]map[string]any, 0, len(detail.Holds))
	for _, h := range detail.Holds {
		holds = append(holds, holdMap(h))
	}

	events := make([]map[string]any, 0, len(detail.AuthEvents))
	for _, e := range detail.AuthEvents {
		events = append(events, map[string]any{
			"event":      e.Event,
			"detail":     e.Detail,
			"ip":         e.IP,
			"created_at": e.CreatedAt.UTC(),
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"customer": map[string]any{
			"id":            detail.Customer.ID,
			"email":         detail.Customer.Email,
			"name":          detail.Customer.Name,
			"created_at":    detail.Customer.CreatedAt.UTC(),
			"open_invoices": detail.Customer.OpenInvoices,
			"services":      detail.Customer.Services,
			"has_login":     detail.Customer.HasLogin,
		},
		"accounts":      accounts,
		"subscriptions": subscriptions,
		"services":      services,
		"invoices":      invoices,
		"balance": map[string]any{
			"outstanding": amountsMap(detail.Balance.Outstanding),
			"credit":      amountsMap(detail.Balance.Credit),
		},
		"holds":              holds,
		"recent_auth_events": events,
	})
}

func subscriptionMap(sub staff.SubscriptionRow) map[string]any {
	return map[string]any{
		"id":                   sub.ID,
		"status":               sub.Status,
		"interval":             sub.Interval,
		"product":              sub.Product,
		"next_billing_at":      sub.NextBillingAt.UTC(),
		"current_period_start": sub.CurrentPeriodStart.UTC(),
		"current_period_end":   sub.CurrentPeriodEnd.UTC(),
		"cancel_at_period_end": sub.CancelAtPeriodEnd,
	}
}

func serviceMap(svc staff.ServiceRow) map[string]any {
	return map[string]any{
		"id":              svc.ID,
		"customer":        map[string]any{"id": svc.CustomerID, "email": svc.CustomerMail},
		"product":         map[string]any{"id": svc.ProductID, "name": svc.Product},
		"status":          svc.Status,
		"external_id":     svc.ExternalID,
		"generation":      svc.Generation,
		"next_bill_at":    timeOrNil(svc.NextBillAt),
		"created_at":      svc.CreatedAt.UTC(),
		"open_operations": svc.OpenOps,
		"last_error":      nullableString(svc.LastError),
	}
}

func invoiceMap(inv staff.InvoiceRow) map[string]any {
	out := map[string]any{
		"id":        inv.ID,
		"number":    inv.Number,
		"status":    inv.Status,
		"currency":  inv.Currency,
		"total":     amountMap(staff.Amount{Minor: inv.Total, Currency: mustCurrency(inv.Currency)}),
		"paid":      amountMap(staff.Amount{Minor: inv.Paid, Currency: mustCurrency(inv.Currency)}),
		"due":       amountMap(staff.Amount{Minor: inv.Due, Currency: mustCurrency(inv.Currency)}),
		"overdue":   inv.Overdue,
		"issued_at": timeOrNil(inv.IssuedAt),
		"due_at":    timeOrNil(inv.DueAt),
	}
	if inv.CustomerID != "" {
		out["customer"] = map[string]any{"id": inv.CustomerID, "email": inv.CustomerEmail}
	}
	return out
}

func holdMap(h staff.HoldRow) map[string]any {
	return map[string]any{
		"id":         h.ID,
		"service_id": h.ServiceID,
		"reason":     h.Reason,
		"expires_at": timeOrNil(h.ExpiresAt),
		"created_by": h.CreatedBy,
		"created_at": h.CreatedAt.UTC(),
	}
}

// nullableString renders an empty string as JSON null, which is what the UI branches on for
// "no error" versus "an error with an empty message".
func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// optionalString renders an optional database text column. A nil column is JSON null — which is
// what the payment list uses to say "this is not an offline payment" — and an empty value is
// treated the same way, because an empty method is not a method.
func optionalString(s *string) any {
	if s == nil || *s == "" {
		return nil
	}
	return *s
}

// mustCurrency resolves a currency code that came from a database CHECK constraint, so it is
// always well formed; the error path exists only to keep the renderer total.
func mustCurrency(code string) money.Currency {
	cur, err := staff.CurrencyOf(code)
	if err != nil {
		return money.Currency{}
	}
	return cur
}

// ------------------------------------------------------------------- invoices --

func (s *Server) handleStaffInvoices(w http.ResponseWriter, r *http.Request) {
	limit, cursor, ok := s.pageParams(w, r)
	if !ok {
		return
	}
	from, ok := s.timeParam(w, r, "from")
	if !ok {
		return
	}
	to, ok := s.timeParam(w, r, "to")
	if !ok {
		return
	}
	overdue, ok := s.boolParam(w, r, "overdue")
	if !ok {
		return
	}

	q := r.URL.Query()
	filter := staff.InvoiceFilter{
		Status:     q.Get("status"),
		CustomerID: q.Get("customer_id"),
		Currency:   q.Get("currency"),
		From:       from,
		To:         to,
	}
	if overdue != nil {
		filter.Overdue = *overdue
	}

	rows, next, err := s.staffService().Invoices(r.Context(), filter, limit, cursor)
	if err != nil {
		s.staffError(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		out = append(out, invoiceMap(row))
	}
	writePage(w, Page[map[string]any]{Data: out, NextCursor: nextCursor(next)})
}

func (s *Server) handleStaffInvoice(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validUUID(id) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such record")
		return
	}

	detail, err := s.staffService().Invoice(r.Context(), id)
	if err != nil {
		s.staffError(w, r, err)
		return
	}

	lines := make([]map[string]any, 0, len(detail.Lines))
	for _, l := range detail.Lines {
		lines = append(lines, map[string]any{
			"position":    l.Position,
			"description": l.Description,
			"quantity":    l.Quantity,
			"unit_minor":  l.UnitMinor,
			"net_minor":   l.NetMinor,
			"tax_rate":    l.TaxRate,
			"tax_minor":   l.TaxMinor,
			"total_minor": l.TotalMinor,
		})
	}

	payments := make([]map[string]any, 0, len(detail.Payments))
	for _, p := range detail.Payments {
		payments = append(payments, map[string]any{
			"id":                  p.ID,
			"provider":            p.Provider,
			"provider_account":    p.ProviderAccount,
			"provider_payment_id": p.ProviderPaymentID,
			"amount":              amountMap(staff.Amount{Minor: p.Amount, Currency: mustCurrency(p.Currency)}),
			"status":              p.Status,
			"received_at":         p.ReceivedAt.UTC(),
			"method":              optionalString(p.Method),
			"note":                optionalString(p.Note),
		})
	}

	allocations := make([]map[string]any, 0, len(detail.Allocations))
	for _, a := range detail.Allocations {
		allocations = append(allocations, map[string]any{
			"id":         a.ID,
			"payment_id": a.PaymentID,
			"invoice_id": a.InvoiceID,
			"amount":     amountMap(staff.Amount{Minor: a.Amount, Currency: mustCurrency(a.Currency)}),
			"created_at": a.CreatedAt.UTC(),
		})
	}

	refunds := make([]map[string]any, 0, len(detail.Refunds))
	for _, rf := range detail.Refunds {
		refunds = append(refunds, refundMap(rf))
	}

	creditNotes := make([]map[string]any, 0, len(detail.CreditNotes))
	for _, cn := range detail.CreditNotes {
		creditNotes = append(creditNotes, creditNoteMap(cn))
	}

	// The document's own lifecycle columns: whether it is provisional, the note an operator
	// wrote for the customer, and why it was voided. They are printed on the PDF and were
	// absent here, so the screen could not show the note it had just written or say that a
	// document is a proforma rather than a numbered invoice.
	doc, err := s.loadInvoiceDocumentState(r.Context(), id)
	if err != nil {
		if errors.Is(err, errDocumentNotFound) {
			writeProblem(w, http.StatusNotFound, "not_found", "no such record")
			return
		}
		s.staffError(w, r, err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"invoice": map[string]any{
			"id":          detail.Invoice.ID,
			"number":      detail.Invoice.Number,
			"status":      detail.Invoice.Status,
			"currency":    detail.Invoice.Currency,
			"total":       amountMap(staff.Amount{Minor: detail.Invoice.Total, Currency: mustCurrency(detail.Invoice.Currency)}),
			"paid":        amountMap(staff.Amount{Minor: detail.Invoice.Paid, Currency: mustCurrency(detail.Invoice.Currency)}),
			"due":         amountMap(staff.Amount{Minor: detail.Invoice.Due, Currency: mustCurrency(detail.Invoice.Currency)}),
			"overdue":     detail.Invoice.Overdue,
			"issued_at":   timeOrNil(detail.Invoice.IssuedAt),
			"due_at":      timeOrNil(detail.Invoice.DueAt),
			"proforma":    doc.Proforma,
			"client_note": doc.ClientNote,
			"void_reason": doc.VoidReason,
			// The setting that decides whether a customer reads the note. The screen shows
			// the note either way and says when it is not printed, because a note an operator
			// wrote and cannot see on the customer's copy is a support call.
			"notes_client_visible": s.clientNotesVisible(r.Context()),
			"customer":             map[string]any{"id": detail.Customer.ID, "email": detail.Customer.Email, "name": detail.Customer.Name},
			"snapshot":             detail.Snapshot,
			"lines":                lines,
		},
		"payments":     payments,
		"allocations":  allocations,
		"refunds":      refunds,
		"credit_notes": creditNotes,
		"journal":      journalMap(detail.Journal),
	})
}

func refundMap(rf staff.RefundRow) map[string]any {
	return map[string]any{
		"id":                 rf.ID,
		"payment_id":         rf.PaymentID,
		"provider_refund_id": nullableString(rf.ProviderRefundID),
		"operation_key":      rf.OperationKey,
		"amount":             amountMap(staff.Amount{Minor: rf.Amount, Currency: mustCurrency(rf.Currency)}),
		"status":             rf.Status,
		"reason":             rf.Reason,
		"created_at":         rf.CreatedAt.UTC(),
		"finished_at":        timeOrNil(rf.FinishedAt),
	}
}

func creditNoteMap(cn staff.CreditNoteRow) map[string]any {
	out := map[string]any{
		"id":       cn.ID,
		"number":   cn.Number,
		"kind":     cn.Kind,
		"currency": cn.Currency,
		"total":    amountMap(staff.Amount{Minor: cn.TotalMinor, Currency: mustCurrency(cn.Currency)}),
		"tax":      amountMap(staff.Amount{Minor: cn.TaxMinor, Currency: mustCurrency(cn.Currency)}),
		"reason":   cn.Reason,
		"status":   cn.Status,
		// voidable is the screen's affordance: only an issued note that has not been spent can
		// be cancelled, and the server re-checks that under a lock.
		"voidable":    cn.Status == "issued",
		"void_reason": cn.VoidReason,
		"voided_at":   timeOrNil(cn.VoidedAt),
		"issued_at":   timeOrNil(cn.IssuedAt),
		"created_at":  cn.CreatedAt.UTC(),
	}
	if cn.InvoiceID != "" {
		out["invoice_id"] = cn.InvoiceID
	}
	return out
}

// journalMap renders postings. It is read-only by construction: there is no endpoint that
// edits a posting, because a correction is a reversal, not an update.
func journalMap(transactions []staff.JournalTransaction) []map[string]any {
	out := make([]map[string]any, 0, len(transactions))
	for _, tx := range transactions {
		entries := make([]map[string]any, 0, len(tx.Entries))
		for _, e := range tx.Entries {
			entries = append(entries, map[string]any{
				"account": e.AccountCode,
				"debit":   e.DebitMinor,
				"credit":  e.CreditMinor,
				"memo":    e.Memo,
			})
		}
		out = append(out, map[string]any{
			"id":             tx.ID,
			"kind":           tx.Kind,
			"reference_type": tx.ReferenceType,
			"reference_id":   tx.ReferenceID,
			"currency":       tx.Currency,
			"created_at":     tx.CreatedAt.UTC(),
			"entries":        entries,
		})
	}
	return out
}

// ------------------------------------------------------------------- services --

func (s *Server) handleStaffServices(w http.ResponseWriter, r *http.Request) {
	limit, cursor, ok := s.pageParams(w, r)
	if !ok {
		return
	}
	open, ok := s.boolParam(w, r, "has_open_operation")
	if !ok {
		return
	}

	q := r.URL.Query()
	filter := staff.ServiceFilter{
		Status:     q.Get("status"),
		CustomerID: q.Get("customer_id"),
		ProductID:  q.Get("product_id"),
	}
	if open != nil {
		filter.OnlyOpenOperations = *open
	}

	rows, next, err := s.staffService().Services(r.Context(), filter, limit, cursor)
	if err != nil {
		s.staffError(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		out = append(out, serviceMap(row))
	}
	writePage(w, Page[map[string]any]{Data: out, NextCursor: nextCursor(next)})
}

func (s *Server) handleStaffService(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validUUID(id) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such record")
		return
	}

	detail, err := s.staffService().Service(r.Context(), id)
	if err != nil {
		s.staffError(w, r, err)
		return
	}

	operations := make([]map[string]any, 0, len(detail.Operations))
	for _, op := range detail.Operations {
		operations = append(operations, operationMap(op))
	}
	holds := make([]map[string]any, 0, len(detail.Holds))
	for _, h := range detail.Holds {
		holds = append(holds, holdMap(h))
	}
	invoices := make([]map[string]any, 0, len(detail.Invoices))
	for _, inv := range detail.Invoices {
		invoices = append(invoices, invoiceMap(inv))
	}

	out := map[string]any{
		"service":      serviceMap(detail.Service),
		"subscription": nil,
		"operations":   operations,
		"holds":        holds,
		"invoices":     invoices,
	}
	if detail.Subscription != nil {
		out["subscription"] = subscriptionMap(*detail.Subscription)
	}
	writeJSON(w, http.StatusOK, out)
}

func operationMap(op staff.OperationRow) map[string]any {
	return map[string]any{
		"id":            op.ID,
		"service":       map[string]any{"id": op.ServiceID, "customer": map[string]any{"id": op.CustomerID, "email": op.CustomerMail}},
		"kind":          op.Kind,
		"operation_key": op.OperationKey,
		"generation":    op.Generation,
		"status":        op.Status,
		"attempts":      op.Attempts,
		"last_error":    nullableString(op.LastError),
		"created_at":    op.CreatedAt.UTC(),
		"started_at":    timeOrNil(op.StartedAt),
		"finished_at":   timeOrNil(op.FinishedAt),
	}
}

// handleStaffEligibility evaluates suspension eligibility.
//
// It is a separate endpoint from the service detail because it *writes*: every evaluation
// records a suspension_decisions row, which is what makes "why was this customer suspended"
// answerable. A read endpoint that writes on every page load would fill the audit trail with
// noise, so the operator asks for it deliberately.
func (s *Server) handleStaffEligibility(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validUUID(id) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such record")
		return
	}
	if s.dunning == nil {
		writeProblem(w, http.StatusServiceUnavailable, "dunning_unavailable",
			"the dunning engine is not configured")
		return
	}

	elig, err := s.dunning.Evaluate(r.Context(), id)
	if err != nil {
		s.staffError(w, r, err)
		return
	}

	out := map[string]any{
		"service_id": elig.ServiceID,
		"eligible":   elig.Eligible,
		"reason":     elig.Reason,
		"generation": elig.Generation,
	}
	if elig.InvoiceID != "" {
		out["invoice_id"] = elig.InvoiceID
	}
	if elig.Outstanding != nil {
		out["outstanding"] = *elig.Outstanding
	}
	writeJSON(w, http.StatusOK, out)
}

// --------------------------------------------------------------------- orders --

func (s *Server) handleStaffOrders(w http.ResponseWriter, r *http.Request) {
	limit, cursor, ok := s.pageParams(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	filter := staff.OrderFilter{Status: q.Get("status"), CustomerID: q.Get("customer_id")}

	rows, next, err := s.staffService().Orders(r.Context(), filter, limit, cursor)
	if err != nil {
		s.staffError(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		out = append(out, map[string]any{
			"id":         row.ID,
			"customer":   map[string]any{"id": row.CustomerID, "email": row.CustomerEmail},
			"status":     row.Status,
			"currency":   row.Currency,
			"total":      amountMap(staff.Amount{Minor: row.TotalMinor, Currency: mustCurrency(row.Currency)}),
			"created_at": row.CreatedAt.UTC(),
			"invoice_id": nullableString(row.InvoiceID),
		})
	}
	writePage(w, Page[map[string]any]{Data: out, NextCursor: nextCursor(next)})
}

func (s *Server) handleStaffOrder(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validUUID(id) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such record")
		return
	}
	detail, err := s.staffService().Order(r.Context(), id)
	if err != nil {
		s.staffError(w, r, err)
		return
	}

	items := make([]map[string]any, 0, len(detail.Items))
	for _, item := range detail.Items {
		items = append(items, map[string]any{
			"id":               item.ID,
			"description":      item.Description,
			"quantity":         item.Quantity,
			"unit_minor":       item.UnitMinor,
			"line_minor":       item.LineMinor,
			"setup_minor":      item.SetupMinor,
			"price_version_id": item.PriceVersionID,
			"provision_plan":   item.ProvisionPlan,
			"provision_region": item.ProvisionRegion,
		})
	}
	invoices := make([]map[string]any, 0, len(detail.Invoices))
	for _, inv := range detail.Invoices {
		invoices = append(invoices, invoiceMap(inv))
	}
	attempts := make([]map[string]any, 0, len(detail.Attempts))
	for _, a := range detail.Attempts {
		attempts = append(attempts, map[string]any{
			"id":           a.ID,
			"gateway":      a.Gateway,
			"status":       a.Status,
			"provider_ref": nullableString(a.ProviderRef),
			"amount":       amountMap(staff.Amount{Minor: a.AmountMinor, Currency: mustCurrency(a.Currency)}),
			"created_at":   a.CreatedAt.UTC(),
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"order": map[string]any{
			"id":         detail.Order.ID,
			"customer":   map[string]any{"id": detail.Order.CustomerID, "email": detail.Order.CustomerEmail},
			"status":     detail.Order.Status,
			"currency":   detail.Order.Currency,
			"total":      amountMap(staff.Amount{Minor: detail.Order.TotalMinor, Currency: mustCurrency(detail.Order.Currency)}),
			"created_at": detail.Order.CreatedAt.UTC(),
			"updated_at": detail.Order.UpdatedAt.UTC(),
			"invoice_id": nullableString(detail.Order.InvoiceID),
		},
		"items":    items,
		"invoices": invoices,
		"attempts": attempts,
	})
}

// ------------------------------------------------------------------- payments --

func (s *Server) handleStaffPayments(w http.ResponseWriter, r *http.Request) {
	limit, cursor, ok := s.pageParams(w, r)
	if !ok {
		return
	}
	from, ok := s.timeParam(w, r, "from")
	if !ok {
		return
	}
	to, ok := s.timeParam(w, r, "to")
	if !ok {
		return
	}

	q := r.URL.Query()
	filter := staff.PaymentFilter{
		Provider:   q.Get("provider"),
		Status:     q.Get("status"),
		CustomerID: q.Get("customer_id"),
		// How an offline payment arrived. It is exact, like provider, and empty means "any".
		Method: q.Get("method"),
		From:   from,
		To:     to,
	}

	rows, invoices, next, err := s.staffService().Payments(r.Context(), filter, limit, cursor)
	if err != nil {
		s.staffError(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for i, row := range rows {
		ids := []string{}
		if invoices[i] != "" {
			ids = splitIDs(invoices[i])
		}
		out = append(out, map[string]any{
			"id":                  row.ID,
			"provider":            row.Provider,
			"provider_account":    row.ProviderAccount,
			"provider_payment_id": row.ProviderPaymentID,
			"amount":              amountMap(staff.Amount{Minor: row.Amount, Currency: mustCurrency(row.Currency)}),
			"currency":            row.Currency,
			"status":              row.Status,
			"received_at":         row.ReceivedAt.UTC(),
			"invoice_ids":         ids,
			// null for a gateway payment: how it arrived is the provider's fact, not ours.
			"method": optionalString(row.Method),
			"note":   optionalString(row.Note),
		})
	}
	writePage(w, Page[map[string]any]{Data: out, NextCursor: nextCursor(next)})
}

// splitIDs splits the comma-joined invoice ids the list query aggregates.
func splitIDs(joined string) []string {
	out := []string{}
	for _, part := range splitComma(joined) {
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func splitComma(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == ',' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return append(out, s[start:])
}

func (s *Server) handleStaffPayment(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validUUID(id) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such record")
		return
	}
	detail, err := s.staffService().Payment(r.Context(), id)
	if err != nil {
		s.staffError(w, r, err)
		return
	}

	allocations := make([]map[string]any, 0, len(detail.Allocations))
	for _, a := range detail.Allocations {
		allocations = append(allocations, map[string]any{
			"id":         a.ID,
			"payment_id": a.PaymentID,
			"invoice_id": a.InvoiceID,
			"amount":     amountMap(staff.Amount{Minor: a.Amount, Currency: mustCurrency(a.Currency)}),
			"created_at": a.CreatedAt.UTC(),
		})
	}
	refunds := make([]map[string]any, 0, len(detail.Refunds))
	for _, rf := range detail.Refunds {
		refunds = append(refunds, refundMap(rf))
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"payment": map[string]any{
			"id":                  detail.Payment.ID,
			"provider":            detail.Payment.Provider,
			"provider_account":    detail.Payment.ProviderAccount,
			"provider_payment_id": detail.Payment.ProviderPaymentID,
			"amount":              amountMap(staff.Amount{Minor: detail.Payment.Amount, Currency: mustCurrency(detail.Payment.Currency)}),
			"currency":            detail.Payment.Currency,
			"status":              detail.Payment.Status,
			"received_at":         detail.Payment.ReceivedAt.UTC(),
			"method":              optionalString(detail.Payment.Method),
			"note":                optionalString(detail.Payment.Note),
		},
		"allocations": allocations,
		"refunds":     refunds,
		"journal":     journalMap(detail.Journal),
	})
}

// ---------------------------------------------------------- operations/inbox --

func (s *Server) handleStaffOperations(w http.ResponseWriter, r *http.Request) {
	limit, cursor, ok := s.pageParams(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	filter := staff.OperationFilter{
		Status:    q.Get("status"),
		Kind:      q.Get("kind"),
		ServiceID: q.Get("service_id"),
	}

	rows, next, err := s.staffService().Operations(r.Context(), filter, limit, cursor)
	if err != nil {
		s.staffError(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		entry := operationMap(row)
		if row.StartedAt != nil {
			entry["lease_expires_at"] = row.StartedAt.Add(provisioning.DefaultLeaseDuration).UTC()
		} else {
			entry["lease_expires_at"] = nil
		}
		out = append(out, entry)
	}
	writePage(w, Page[map[string]any]{Data: out, NextCursor: nextCursor(next)})
}

func (s *Server) handleStaffWebhooks(w http.ResponseWriter, r *http.Request) {
	limit, cursor, ok := s.pageParams(w, r)
	if !ok {
		return
	}
	processed, ok := s.boolParam(w, r, "processed")
	if !ok {
		return
	}

	filter := staff.WebhookFilter{Processed: processed, Provider: r.URL.Query().Get("provider")}
	rows, next, err := s.staffService().Webhooks(r.Context(), filter, limit, cursor)
	if err != nil {
		s.staffError(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		out = append(out, map[string]any{
			"id":               row.ID,
			"provider":         row.Provider,
			"provider_account": row.ProviderAccount,
			"event_id":         row.EventID,
			"event_type":       row.EventType,
			"received_at":      row.ReceivedAt.UTC(),
			"processed_at":     timeOrNil(row.ProcessedAt),
			"attempts":         row.Attempts,
			"last_error":       nullableString(row.LastError),
		})
	}
	writePage(w, Page[map[string]any]{Data: out, NextCursor: nextCursor(next)})
}

// -------------------------------------------------------------------- catalog --

func (s *Server) handleStaffProducts(w http.ResponseWriter, r *http.Request) {
	limit, cursor, ok := s.pageParams(w, r)
	if !ok {
		return
	}
	rows, next, err := s.staffService().Products(r.Context(), limit, cursor)
	if err != nil {
		s.staffError(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		out = append(out, productMap(row))
	}
	writePage(w, Page[map[string]any]{Data: out, NextCursor: nextCursor(next)})
}

func (s *Server) handleStaffProduct(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validUUID(id) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such record")
		return
	}
	row, err := s.staffService().Product(r.Context(), id)
	if err != nil {
		s.staffError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"product":              map[string]any{"id": row.ID, "slug": row.Slug, "name": row.Name, "created_at": row.CreatedAt.UTC()},
		"price_versions":       priceVersionsMap(row.PriceVersions),
		"active_subscriptions": row.ActiveSubscriptions,
	})
}

func productMap(row staff.ProductRow) map[string]any {
	return map[string]any{
		"id":             row.ID,
		"slug":           row.Slug,
		"name":           row.Name,
		"created_at":     row.CreatedAt.UTC(),
		"price_versions": priceVersionsMap(row.PriceVersions),
	}
}

func priceVersionsMap(versions []staff.PriceVersionRow) []map[string]any {
	out := make([]map[string]any, 0, len(versions))
	for _, v := range versions {
		out = append(out, map[string]any{
			"id":               v.ID,
			"version":          v.Version,
			"currency":         v.Currency,
			"amount":           amountMap(staff.Amount{Minor: v.AmountMinor, Currency: mustCurrency(v.Currency)}),
			"setup":            amountMap(staff.Amount{Minor: v.SetupMinor, Currency: mustCurrency(v.Currency)}),
			"interval":         v.Interval,
			"anchor_day":       v.AnchorDay,
			"anchor_timezone":  v.AnchorTimezone,
			"provision_plan":   v.ProvisionPlan,
			"provision_region": v.ProvisionRegion,
			"created_at":       v.CreatedAt.UTC(),
		})
	}
	return out
}
