package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/catalyst/billing/internal/authn"
	"github.com/catalyst/billing/internal/credit"
	"github.com/catalyst/billing/internal/events"
	"github.com/catalyst/billing/internal/ledger"
	"github.com/catalyst/billing/internal/money"
	"github.com/catalyst/billing/internal/paginate"
	"github.com/catalyst/billing/internal/portal"
)

// The staff credit surface (CRD-07, E5.2).
//
// The grant itself already exists — `POST /api/v1/staff/customers/{id}/credit`, guarded by
// credit:write, which issues a numbered credit note and posts it to the subledger. What was
// missing was the *reading* half: an operator could not find out who holds credit, or look
// at a grant after making it, without SQL.
//
// So the first half of this file is three reads:
//
//   - who holds credit, and how much, per currency — the list an operator starts from;
//   - the grants that have been made, newest first — the audit trail as a screen;
//   - one grant with its credit note, its audit rows and the balance it left behind.
//
// The second half is the direction that did not exist at all: taking credit **away**. A debit
// is a journal entry of its own kind (`credit_debit`, migration 0051) with a mandatory reason,
// refusing to take the balance below zero, idempotent on its Idempotency-Key, and audited in
// the same transaction. It needs credit:write for the same reason a grant does — it is a money
// movement — while reading the list of deductions needs only credit:read:all.
//
// Four decisions shape the queries:
//
//  1. **The balance is derived, never stored.** `customer_credit` is a liability account and
//     its value is the sum of its journal entries, exactly as the customer's own portal reads
//     it. A second source for the number would eventually disagree with the first, and the
//     one an operator quotes to a customer would be the wrong one.
//  2. **Per currency, never summed.** A customer can hold USD and EUR credit at once. There is
//     no exchange rate in this product, so a total would be a number no ledger contains.
//  3. **credit:read:all, not credit:write.** Seeing what a customer is owed is an audit
//     question; granting credit is a money movement. Support holds the first and not the
//     second, which is why reading here cannot be a side effect of the write permission.
//  4. **A debit is read from the journal, not from a table of debits.** The balance is the sum
//     of the `customer_credit` entries, so the list of deductions is read from the same rows. A
//     second table describing the same deduction could disagree with the ledger, and the ledger
//     would be right.

func init() {
	RegisterRoutes("36-credit-staff", registerStaffCreditReads)
	RegisterRoutes("37-credit-debit", registerStaffCreditDebit)
}

func registerStaffCreditReads(r *router) {
	r.staffGet("/api/v1/staff/credit/customers", authn.PermCreditReadAll,
		r.s.handleStaffCreditCustomers)
	r.staffGet("/api/v1/staff/credit/grants", authn.PermCreditReadAll,
		r.s.handleStaffCreditGrants)
	r.staffGet("/api/v1/staff/credit/grants/{id}", authn.PermCreditReadAll,
		r.s.handleStaffCreditGrant)
	r.staffGet("/api/v1/staff/credit/debits", authn.PermCreditReadAll,
		r.s.handleStaffCreditDebits)
}

// registerStaffCreditDebit registers the deduction. It is a mutation: CSRF-protected and with a
// required Idempotency-Key, because "press it again" must not take the credit twice.
func registerStaffCreditDebit(r *router) {
	r.staffMutation(http.MethodPost, "/api/v1/staff/customers/{id}/credit/debit",
		authn.PermCreditWrite, r.s.handleStaffDebitCredit)
}

// creditCustomer is one row of the credit list.
type creditCustomer struct {
	ID        string    `json:"id"`
	Email     string    `json:"email"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
	// Balances is one entry per currency with a positive balance. An empty array means
	// "nothing on account", never "could not read it".
	Balances []money.Amount `json:"-"`
}

// creditCustomerJSON renders a row for the wire.
func creditCustomerJSON(row creditCustomer) map[string]any {
	return map[string]any{
		"id":         row.ID,
		"email":      row.Email,
		"name":       row.Name,
		"created_at": row.CreatedAt.UTC(),
		"balances":   creditAmounts(row.Balances),
	}
}

// handleStaffCreditCustomers lists customers with their credit balances.
//
// Query: q (a name or email fragment, two characters minimum), sort (created_at, email, name),
// direction (asc, desc), with_credit (true to list only customers holding a balance), limit,
// cursor.
//
// The search refuses a one-character term for the same reason the customer list does: it
// matches nearly every row, so the answer is a scan that looks like a hang rather than a
// search.
func (s *Server) handleStaffCreditCustomers(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "storage_unavailable", "no database configured")
		return
	}
	limit, cursor, ok := s.pageParams(w, r)
	if !ok {
		return
	}

	q := r.URL.Query()
	sortColumn, direction, ok := creditCustomerSort(w, r)
	if !ok {
		return
	}

	// $1 is the ledger account the balances are summed from, so it is bound once and shared by
	// the lateral subquery and the with_credit filter.
	args := []any{ledger.AccountCredit}
	where := []string{"true"}

	if term := strings.TrimSpace(q.Get("q")); term != "" {
		if len(term) < 2 {
			writeProblem(w, http.StatusBadRequest, "invalid_query",
				"the search needs at least two characters")
			return
		}
		pattern := "%" + escapeLikePattern(strings.ToLower(term)) + "%"
		args = append(args, pattern, pattern)
		where = append(where, fmt.Sprintf(
			"(lower(c.email) LIKE $%d ESCAPE '\\' OR lower(c.name) LIKE $%d ESCAPE '\\')",
			len(args)-1, len(args)))
	}
	if raw := strings.TrimSpace(q.Get("with_credit")); raw != "" {
		withCredit, err := strconv.ParseBool(raw)
		if err != nil {
			writeProblem(w, http.StatusBadRequest, "invalid_query", "with_credit must be true or false")
			return
		}
		if withCredit {
			where = append(where, `EXISTS (
				SELECT 1 FROM journal_entries je
				WHERE je.customer_id = c.id AND je.account_code = $1
				GROUP BY je.customer_id
				HAVING SUM(je.credit_minor - je.debit_minor) > 0)`)
		}
	}

	op := "<"
	if direction == "ASC" {
		op = ">"
	}
	if cursor != "" {
		sortKey, id, err := paginate.Decode(cursor)
		if err != nil || id == "" {
			writeProblem(w, http.StatusBadRequest, "invalid_cursor", "the cursor is invalid")
			return
		}
		args = append(args, sortKey, id)
		where = append(where, fmt.Sprintf("(%s, c.id) %s ($%d, $%d)",
			sortColumn, op, len(args)-1, len(args)))
	}

	args = append(args, limit+1)
	stmt := `
		SELECT c.id, c.email, c.name, c.created_at, COALESCE(bal.balances, '[]'::jsonb)
		FROM customers c
		LEFT JOIN LATERAL (
			-- One row per currency, folded into a JSON array. Summing across currencies is
			-- deliberately impossible here: there is no exchange rate, so a single number
			-- would be arithmetic on unrelated quantities.
			SELECT jsonb_agg(jsonb_build_object('currency', currency, 'minor', balance)
			                 ORDER BY currency) AS balances
			FROM (
				SELECT currency, SUM(credit_minor - debit_minor) AS balance
				FROM journal_entries
				WHERE customer_id = c.id AND account_code = $1
				GROUP BY currency
				HAVING SUM(credit_minor - debit_minor) > 0
			) per_currency
		) bal ON true
		WHERE ` + strings.Join(where, " AND ") + `
		ORDER BY ` + sortColumn + ` ` + direction + `, c.id ` + direction + `
		LIMIT $` + strconv.Itoa(len(args))

	rows, err := s.pool.Query(r.Context(), stmt, args...)
	if err != nil {
		s.cfg.Logger.ErrorContext(r.Context(), "credit customer list failed", "error", err)
		writeProblem(w, http.StatusInternalServerError, "internal_error",
			"the request could not be completed")
		return
	}
	defer rows.Close()

	out := make([]creditCustomer, 0, limit)
	keys := make([]string, 0, limit)
	for rows.Next() {
		var (
			row      creditCustomer
			balances []byte
		)
		if err := rows.Scan(&row.ID, &row.Email, &row.Name, &row.CreatedAt, &balances); err != nil {
			s.cfg.Logger.ErrorContext(r.Context(), "scan credit customer failed", "error", err)
			writeProblem(w, http.StatusInternalServerError, "internal_error",
				"the request could not be completed")
			return
		}
		parsed, err := parseCreditAmounts(balances)
		if err != nil {
			s.cfg.Logger.ErrorContext(r.Context(), "decode credit balances failed", "error", err)
			writeProblem(w, http.StatusInternalServerError, "internal_error",
				"the request could not be completed")
			return
		}
		row.Balances = parsed
		out = append(out, row)
		// The cursor's sort key follows the column the list was ordered by, so it is derived
		// from the row rather than from the query.
		switch sortColumn {
		case "lower(c.email)":
			keys = append(keys, strings.ToLower(row.Email))
		case "lower(c.name)":
			keys = append(keys, strings.ToLower(row.Name))
		default:
			keys = append(keys, row.CreatedAt.UTC().Format(time.RFC3339Nano))
		}
	}
	if err := rows.Err(); err != nil {
		writeProblem(w, http.StatusInternalServerError, "internal_error",
			"the request could not be completed")
		return
	}

	var next *string
	if len(out) > limit {
		out = out[:limit]
		c := paginate.Encode(keys[len(out)-1], out[len(out)-1].ID)
		next = &c
	}

	page := make([]map[string]any, 0, len(out))
	for _, row := range out {
		page = append(page, creditCustomerJSON(row))
	}
	writeJSON(w, http.StatusOK, Page[map[string]any]{Data: page, NextCursor: next})
}

// creditCustomerSort validates the sort parameters and returns the SQL column and direction.
//
// The column is chosen from a fixed set rather than interpolated from the request: the sort
// key is the one place in a list endpoint where a query string could otherwise reach SQL.
func creditCustomerSort(w http.ResponseWriter, r *http.Request) (column, direction string, ok bool) {
	q := r.URL.Query()
	direction = "DESC"
	if strings.EqualFold(strings.TrimSpace(q.Get("direction")), "asc") {
		direction = "ASC"
	}
	switch strings.TrimSpace(q.Get("sort")) {
	case "", "created_at":
		return "c.created_at", direction, true
	case "email":
		return "lower(c.email)", direction, true
	case "name":
		return "lower(c.name)", direction, true
	default:
		writeProblem(w, http.StatusBadRequest, "invalid_query",
			"sort must be created_at, email or name")
		return "", "", false
	}
}

// creditGrantRow is one grant in the list.
type creditGrantRow struct {
	ID            string
	Number        string
	CustomerID    string
	CustomerEmail string
	Currency      string
	AmountMinor   int64
	Reason        string
	Status        string
	CreatedAt     time.Time
	CreatedBy     string
	CreatedByMail string
}

// handleStaffCreditGrants lists goodwill credit grants, newest first.
//
// Query: customer_id, limit, cursor.
//
// "Grant" is read from the note's frozen snapshot (`snapshot->>'kind' = 'grant'`) rather than
// from a second table: the snapshot is what the document was issued with, so it cannot drift
// from the note it describes. A credit note that corrects an invoice has a different kind and
// belongs on the invoice, not here.
func (s *Server) handleStaffCreditGrants(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "storage_unavailable", "no database configured")
		return
	}
	limit, cursor, ok := s.pageParams(w, r)
	if !ok {
		return
	}

	args := []any{}
	where := []string{"cn.snapshot->>'kind' = 'grant'"}
	if customerID := strings.TrimSpace(r.URL.Query().Get("customer_id")); customerID != "" {
		if !portal.ValidID(customerID) {
			writeProblem(w, http.StatusNotFound, "not_found", "no such customer")
			return
		}
		args = append(args, customerID)
		where = append(where, "$"+strconv.Itoa(len(args))+" = cn.customer_id")
	}
	if cursor != "" {
		when, id, err := decodeCreditCursor(cursor)
		if err != nil {
			writeProblem(w, http.StatusBadRequest, "invalid_cursor", "the cursor is invalid")
			return
		}
		args = append(args, when, id)
		where = append(where, fmt.Sprintf("(cn.created_at, cn.id) < ($%d, $%d)",
			len(args)-1, len(args)))
	}
	args = append(args, limit+1)

	rows, err := s.pool.Query(r.Context(), `
		SELECT cn.id, COALESCE(cn.number, ''), cn.customer_id, c.email, cn.currency,
		       cn.total_minor, cn.reason, cn.status, cn.created_at,
		       COALESCE(cn.created_by::text, ''), COALESCE(u.email, '')
		FROM credit_notes cn
		JOIN customers c ON c.id = cn.customer_id
		LEFT JOIN users u ON u.id = cn.created_by
		WHERE `+strings.Join(where, " AND ")+`
		ORDER BY cn.created_at DESC, cn.id DESC
		LIMIT $`+strconv.Itoa(len(args)), args...)
	if err != nil {
		s.cfg.Logger.ErrorContext(r.Context(), "credit grant list failed", "error", err)
		writeProblem(w, http.StatusInternalServerError, "internal_error",
			"the request could not be completed")
		return
	}
	defer rows.Close()

	out := make([]creditGrantRow, 0, limit)
	for rows.Next() {
		var row creditGrantRow
		if err := rows.Scan(&row.ID, &row.Number, &row.CustomerID, &row.CustomerEmail,
			&row.Currency, &row.AmountMinor, &row.Reason, &row.Status, &row.CreatedAt,
			&row.CreatedBy, &row.CreatedByMail); err != nil {
			writeProblem(w, http.StatusInternalServerError, "internal_error",
				"the request could not be completed")
			return
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		writeProblem(w, http.StatusInternalServerError, "internal_error",
			"the request could not be completed")
		return
	}

	var next *string
	if len(out) > limit {
		out = out[:limit]
		last := out[len(out)-1]
		c := encodeCreditCursor(last.CreatedAt, last.ID)
		next = &c
	}
	page := make([]map[string]any, 0, len(out))
	for _, row := range out {
		page = append(page, creditGrantJSON(row))
	}
	writeJSON(w, http.StatusOK, Page[map[string]any]{Data: page, NextCursor: next})
}

// creditGrantJSON renders one grant for the wire.
func creditGrantJSON(row creditGrantRow) map[string]any {
	return map[string]any{
		"id":               row.ID,
		"number":           row.Number,
		"customer_id":      row.CustomerID,
		"customer_email":   row.CustomerEmail,
		"amount":           amountJSON(money.New(row.AmountMinor, moneyFor(row.Currency))),
		"reason":           row.Reason,
		"status":           row.Status,
		"created_at":       row.CreatedAt.UTC(),
		"created_by":       row.CreatedBy,
		"created_by_email": row.CreatedByMail,
	}
}

// handleStaffCreditGrant serves one credit note with its audit rows and the balance it left.
//
// This is the "what did that grant do" screen: the note as issued, every audit row written
// against it, and the customer's balance per currency now. The balance is the current one,
// not a replay of the balance at issue time — the ledger's balance at an instant is derivable
// but not stored, and a screen that reconstructed it would be inventing a number.
func (s *Server) handleStaffCreditGrant(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "storage_unavailable", "no database configured")
		return
	}
	id := r.PathValue("id")
	if !validUUID(id) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such credit note")
		return
	}

	var (
		row       creditGrantRow
		invoiceID *string
		kind      string
	)
	err := s.pool.QueryRow(r.Context(), `
		SELECT cn.id, COALESCE(cn.number, ''), cn.customer_id, c.email, cn.currency,
		       cn.total_minor, cn.reason, cn.status, cn.created_at,
		       COALESCE(cn.created_by::text, ''), COALESCE(u.email, ''),
		       cn.invoice_id::text, COALESCE(cn.snapshot->>'kind', '')
		FROM credit_notes cn
		JOIN customers c ON c.id = cn.customer_id
		LEFT JOIN users u ON u.id = cn.created_by
		WHERE cn.id = $1`, id).
		Scan(&row.ID, &row.Number, &row.CustomerID, &row.CustomerEmail, &row.Currency,
			&row.AmountMinor, &row.Reason, &row.Status, &row.CreatedAt,
			&row.CreatedBy, &row.CreatedByMail, &invoiceID, &kind)
	if err != nil {
		if isNoRows(err) {
			writeProblem(w, http.StatusNotFound, "not_found", "no such credit note")
			return
		}
		s.cfg.Logger.ErrorContext(r.Context(), "credit grant read failed", "error", err)
		writeProblem(w, http.StatusInternalServerError, "internal_error",
			"the request could not be completed")
		return
	}

	auditRows, err := s.creditNoteAudit(r.Context(), row.ID)
	if err != nil {
		s.cfg.Logger.ErrorContext(r.Context(), "credit grant audit read failed", "error", err)
		writeProblem(w, http.StatusInternalServerError, "internal_error",
			"the request could not be completed")
		return
	}

	balances, err := credit.BalanceFor(r.Context(), s.pool, row.CustomerID)
	if err != nil {
		s.cfg.Logger.ErrorContext(r.Context(), "credit balance read failed", "error", err)
		writeProblem(w, http.StatusInternalServerError, "internal_error",
			"the request could not be completed")
		return
	}
	var forCurrency *money.Amount
	for _, balance := range balances {
		if balance.Currency().Code() == row.Currency {
			b := balance
			forCurrency = &b
			break
		}
	}
	if forCurrency == nil {
		// Nothing left in this currency: the grant was spent. Reporting a zero balance is the
		// honest answer, and the alternative — omitting the field — reads as a failed read.
		zero := money.New(0, moneyFor(row.Currency))
		forCurrency = &zero
	}

	grant := creditGrantJSON(row)
	grant["kind"] = kind
	grant["invoice_id"] = invoiceID

	writeJSON(w, http.StatusOK, map[string]any{
		"grant":    grant,
		"audit":    auditRows,
		"balance":  amountJSON(*forCurrency),
		"balances": creditAmounts(balances),
		"customer": map[string]any{"id": row.CustomerID, "email": row.CustomerEmail},
	})
}

// creditNoteAudit reads the audit rows written against one credit note.
//
// The rows are read, never assembled: the audit log is append-only and written inside the
// transaction that made the change, so what it says about a note is the evidence rather than a
// description of it. `changes` is already redacted at write time.
func (s *Server) creditNoteAudit(ctx context.Context, noteID string) ([]map[string]any, error) {
	return s.creditAudit(ctx, "credit_note", noteID)
}

// creditAudit reads the audit rows written against one target of any type.
//
// The target type is a parameter because a debit is not a credit note: it has no document, and
// its evidence is the audit row written with the posting (target type `credit_debit`). Reading
// both through one function keeps the row shape identical, which is what lets a client render a
// grant's evidence and a debit's evidence with the same component.
func (s *Server) creditAudit(ctx context.Context, targetType, targetID string) ([]map[string]any, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT a.id, a.action, a.outcome, COALESCE(a.actor_id::text, ''),
		       COALESCE(u.email, ''), a.created_at, a.changes
		FROM audit_events a
		LEFT JOIN users u ON u.id = a.actor_id
		WHERE a.target_type = $1 AND a.target_id = $2
		ORDER BY a.id`, targetType, targetID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []map[string]any{}
	for rows.Next() {
		var (
			id        int64
			action    string
			outcome   string
			actorID   string
			actorMail string
			createdAt time.Time
			changes   []byte
		)
		if err := rows.Scan(&id, &action, &outcome, &actorID, &actorMail, &createdAt,
			&changes); err != nil {
			return nil, err
		}
		decoded := map[string]any{}
		if len(changes) > 0 {
			_ = json.Unmarshal(changes, &decoded)
		}
		out = append(out, map[string]any{
			"id":          id,
			"action":      action,
			"outcome":     outcome,
			"actor_id":    actorID,
			"actor_email": actorMail,
			"created_at":  createdAt.UTC(),
			"changes":     decoded,
		})
	}
	return out, rows.Err()
}

// latestCreditGrantAudit returns the audit row for a grant, for the grant response itself.
//
// It is read after the transaction commits rather than threaded out of the domain service:
// the credit package owns its own audit write, and widening its return type so the HTTP layer
// could echo an audit id would couple the two for a display field. A read that fails is
// reported as absent, because the grant is already committed and the client must not be told
// the grant failed when it did not.
func (s *Server) latestCreditGrantAudit(ctx context.Context, noteID string) map[string]any {
	return s.latestCreditAudit(ctx, "credit_note", noteID)
}

// latestCreditAudit returns the newest audit row for a target, or nil when there is none.
func (s *Server) latestCreditAudit(ctx context.Context, targetType, targetID string) map[string]any {
	rows, err := s.creditAudit(ctx, targetType, targetID)
	if err != nil || len(rows) == 0 {
		return nil
	}
	return rows[len(rows)-1]
}

// parseCreditAmounts decodes the per-currency balance array the list query builds.
func parseCreditAmounts(raw []byte) ([]money.Amount, error) {
	if len(raw) == 0 {
		return []money.Amount{}, nil
	}
	var rows []struct {
		Currency string `json:"currency"`
		Minor    int64  `json:"minor"`
	}
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, err
	}
	out := make([]money.Amount, 0, len(rows))
	for _, row := range rows {
		out = append(out, money.New(row.Minor, moneyFor(row.Currency)))
	}
	return out, nil
}

// encodeCreditCursor renders a (created_at, id) position for the grant list.
func encodeCreditCursor(when time.Time, id string) string {
	return paginate.Encode(when.UTC().Format(time.RFC3339Nano), id)
}

// decodeCreditCursor parses a (created_at, id) position.
func decodeCreditCursor(cursor string) (time.Time, string, error) {
	sortKey, id, err := paginate.Decode(cursor)
	if err != nil {
		return time.Time{}, "", err
	}
	if sortKey == "" || id == "" {
		return time.Time{}, "", paginate.ErrInvalidCursor
	}
	when, err := time.Parse(time.RFC3339Nano, sortKey)
	if err != nil {
		return time.Time{}, "", paginate.ErrInvalidCursor
	}
	return when, id, nil
}

// escapeLikePattern escapes the wildcards in a LIKE pattern so a search for "a_b" does not
// match "axb". It mirrors internal/staff's escapeLike, which is unexported there.
func escapeLikePattern(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch r {
		case '%', '_', '\\':
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// ------------------------------------------------------------ taking credit away --

// handleStaffDebitCredit takes credit away from a customer.
//
// The body is amount, currency and reason, all required. The reason is not a formality: it
// becomes the memo on the `customer_credit` entry, which is what the customer's own credit
// history renders, and it is copied into the audit row written inside the posting's transaction.
// A deduction with no explanation is indistinguishable from a bug to the person whose balance
// moved, which is the whole reason this endpoint exists rather than a note in a ticket.
//
// The refusals are deliberate and distinct:
//
//   - 400 invalid_amount / reason_required / invalid_currency — the request is incomplete, so
//     nothing was attempted and nothing was written;
//   - 409 insufficient_credit — the request is well-formed but the account cannot support it.
//     The balance is unchanged: a debit is refused rather than clamped, because taking part of
//     what was asked for would leave the operator and the customer disagreeing about what
//     happened;
//   - 404 not_found — no such customer;
//   - 409 idempotency_key_reused — the key already belongs to a different deduction. A replay of
//     the same request returns the original entry instead, which is what makes a dropped
//     connection safe to retry.
func (s *Server) handleStaffDebitCredit(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "storage_unavailable", "no database configured")
		return
	}
	customerID := r.PathValue("id")
	if !portal.ValidID(customerID) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such customer")
		return
	}

	var req struct {
		AmountMinor int64  `json:"amount_minor"`
		Currency    string `json:"currency"`
		Reason      string `json:"reason"`
	}
	if !s.decodeJSON(w, r, &req, 8<<10) {
		return
	}
	if req.AmountMinor <= 0 {
		writeProblem(w, http.StatusBadRequest, "invalid_amount", "the amount must be positive")
		return
	}
	if strings.TrimSpace(req.Reason) == "" {
		writeProblem(w, http.StatusBadRequest, "reason_required",
			"a debit needs a reason: the customer sees it on their own credit history")
		return
	}
	if strings.TrimSpace(req.Currency) == "" {
		writeProblem(w, http.StatusBadRequest, "invalid_currency", "a currency is required")
		return
	}

	// The route wrapper already required the key and replays a stored response for it. The key
	// is passed on anyway, because the domain layer derives the entry's identity from it: that
	// is what makes the debit safe even if the wrapper is bypassed, and it is what makes the
	// ledger's (kind, reference_type, reference_id) unique key the second line of defence.
	user, _ := userFrom(r.Context())
	key := strings.TrimSpace(r.Header.Get(idempotencyHeader))

	// The reason is handed to the auditor as well as to the domain: the audit row carries it even
	// if a future caller of the domain method forgets to put it in its changes, and the auditor is
	// what stamps the request's correlation id and source address on the row.
	debit, err := s.creditServiceWithReason(r, req.Reason).Debit(r.Context(), credit.DebitRequest{
		CustomerID:  customerID,
		AmountMinor: req.AmountMinor,
		Currency:    req.Currency,
		Reason:      req.Reason,
		DebitedBy:   user.ID,
		Key:         key,
	})
	switch {
	case err == nil:
	case errors.Is(err, credit.ErrNotFound):
		writeProblem(w, http.StatusNotFound, "not_found", "no such customer")
		return
	case errors.Is(err, credit.ErrInsufficientCredit):
		// 409, not 400: the request is well-formed and the customer exists; the account cannot
		// absorb it. Nothing was written, so the balance the message quotes is still the balance.
		writeProblem(w, http.StatusConflict, "insufficient_credit", err.Error())
		return
	case errors.Is(err, credit.ErrKeyReused):
		writeProblem(w, http.StatusConflict, "idempotency_key_reused",
			"that Idempotency-Key was already used for a different debit")
		return
	case errors.Is(err, credit.ErrInvalidRequest):
		writeProblem(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	default:
		s.staffError(w, r, err)
		return
	}

	if !debit.AlreadyDebited {
		// The event says a balance moved, which is what a dashboard or a webhook consumer needs
		// to know. It carries no reason: the reason is customer-visible in their own history and
		// in the audit row, and broadcasting it would send an internal note to every subscriber.
		s.publish(events.Event{
			Kind:       "credit.debited",
			Permission: string(authn.PermCustomerReadAll),
			Data: map[string]any{
				"customer_id":  debit.CustomerID,
				"debit_id":     debit.ID,
				"amount_minor": debit.AmountMinor,
				"currency":     debit.Currency,
			},
		})
	}

	cur := moneyFor(debit.Currency)
	writeJSON(w, http.StatusCreated, map[string]any{
		"id":          debit.ID,
		"customer_id": debit.CustomerID,
		"amount":      amountJSON(money.New(debit.AmountMinor, cur)),
		"balance":     amountJSON(money.New(debit.BalanceMinor, cur)),
		"reason":      debit.Reason,
		"kind":        string(ledger.KindCreditDebit),
		"replayed":    debit.AlreadyDebited,
		"audit":       s.latestCreditAudit(r.Context(), "credit_debit", debit.ID),
	})
}

// handleStaffCreditDebits lists staff deductions, newest first.
//
// The rows come from the journal, not from a table of debits: the deduction is a posting, and a
// list built from a second table could disagree with the balance it is meant to explain. The
// operator is read from the audit row written with the posting.
func (s *Server) handleStaffCreditDebits(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "storage_unavailable", "no database configured")
		return
	}
	customerID := strings.TrimSpace(r.URL.Query().Get("customer_id"))
	if customerID != "" && !portal.ValidID(customerID) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such customer")
		return
	}
	limit := 50
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 || parsed > 200 {
			writeProblem(w, http.StatusBadRequest, "invalid_query", "limit must be between 1 and 200")
			return
		}
		limit = parsed
	}

	rows, err := credit.DebitsFor(r.Context(), s.pool, customerID, limit)
	if err != nil {
		s.cfg.Logger.ErrorContext(r.Context(), "credit debit list failed", "error", err)
		writeProblem(w, http.StatusInternalServerError, "internal_error",
			"the request could not be completed")
		return
	}

	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		out = append(out, map[string]any{
			"id":               row.ID,
			"customer_id":      row.CustomerID,
			"customer_email":   row.CustomerEmail,
			"amount":           amountJSON(money.New(row.AmountMinor, moneyFor(row.Currency))),
			"reason":           row.Reason,
			"created_at":       row.CreatedAt.UTC(),
			"created_by":       row.DebitedBy,
			"created_by_email": row.DebitedByMail,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"debits": out})
}
