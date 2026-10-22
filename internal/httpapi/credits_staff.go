package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/catalyst/billing/internal/authn"
	"github.com/catalyst/billing/internal/credit"
	"github.com/catalyst/billing/internal/ledger"
	"github.com/catalyst/billing/internal/money"
	"github.com/catalyst/billing/internal/paginate"
	"github.com/catalyst/billing/internal/portal"
)

// The staff credit read surface (CRD-07, E5.2).
//
// The grant itself already exists — `POST /api/v1/staff/customers/{id}/credit`, guarded by
// credit:write, which issues a numbered credit note and posts it to the subledger. What was
// missing was the *reading* half: an operator could not find out who holds credit, or look
// at a grant after making it, without SQL.
//
// So this file is three reads and nothing else:
//
//   - who holds credit, and how much, per currency — the list an operator starts from;
//   - the grants that have been made, newest first — the audit trail as a screen;
//   - one grant with its credit note, its audit rows and the balance it left behind.
//
// Three decisions shape the queries:
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

func init() {
	RegisterRoutes("36-credit-staff", registerStaffCreditReads)
}

func registerStaffCreditReads(r *router) {
	r.staffGet("/api/v1/staff/credit/customers", authn.PermCreditReadAll,
		r.s.handleStaffCreditCustomers)
	r.staffGet("/api/v1/staff/credit/grants", authn.PermCreditReadAll,
		r.s.handleStaffCreditGrants)
	r.staffGet("/api/v1/staff/credit/grants/{id}", authn.PermCreditReadAll,
		r.s.handleStaffCreditGrant)
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
	rows, err := s.pool.Query(ctx, `
		SELECT a.id, a.action, a.outcome, COALESCE(a.actor_id::text, ''),
		       COALESCE(u.email, ''), a.created_at, a.changes
		FROM audit_events a
		LEFT JOIN users u ON u.id = a.actor_id
		WHERE a.target_type = 'credit_note' AND a.target_id = $1
		ORDER BY a.id`, noteID)
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
	rows, err := s.creditNoteAudit(ctx, noteID)
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
