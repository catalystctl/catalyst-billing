package httpapi

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/catalyst/billing/internal/authn"
	"github.com/catalyst/billing/internal/paginate"
)

// The holds register (ADM hold screen).
//
// A manual hold is the one instruction in this system that says "do not act on this service".
// It outranks dunning, suspension and termination: an operator who holds a service because a
// customer is in a dispute must not have the nightly run act on it anyway. The rows already
// existed and could be placed and cleared from a service screen; what did not exist was the
// cross-service view, which is the only way to answer "why is this customer not being
// invoiced" or "who told us to leave this box alone".
//
// Two facts this endpoint states rather than hides:
//
//   - The billing schedule keeps running. A hold stops *automation*, not the calendar, so the
//     row carries `next_billing_at` and `subscription_status` beside the hold. An operator who
//     assumes a hold freezes invoicing would be wrong, and the screen says so in as many words.
//   - A hold changes no money. Placing one and lifting one issue no invoice and no credit;
//     lifting a hold that has been in place for a month does not retroactively bill that month.
//
// The placement and clearing mutations are the existing routes
// (`POST /api/v1/staff/services/{id}/holds`, `DELETE /api/v1/staff/holds/{hold_id}`), which
// already audit inside their transaction and take the actor from the session. This file only
// adds the read that makes them discoverable, so there is one implementation of "place a hold"
// and not two that can drift.

func init() {
	RegisterRoutes("51-holds-admin", registerHoldsAdmin)
}

func registerHoldsAdmin(r *router) {
	r.staffGet("/api/v1/staff/holds", authn.PermServiceReadAll, r.s.handleActiveHolds)
}

// holdRegisterRow is one hold joined to the things an operator needs beside it.
type holdRegisterRow struct {
	ID            string
	ServiceID     string
	ServiceStatus string
	Generation    int64
	ProductName   string
	ProductSlug   string
	CustomerID    string
	CustomerName  string
	CustomerEmail string
	Reason        string
	CreatedAt     time.Time
	ExpiresAt     *time.Time
	// CreatedBy is the raw service_holds column. It is text, not a foreign key, so a hold
	// placed by an importer or a fixture names a string rather than a user.
	CreatedBy      string
	PlacedByEmail  string
	PlacedByName   string
	SubStatus      string
	NextBillingAt  *time.Time
	OpenInvoices   int
	LastInvoiceNum string
}

// handleActiveHolds lists every hold, with the service, the customer, when it was placed, why
// and who placed it.
//
// Query:
//
//	status  active (default) | expired | all
//	q       matches the customer's email or name, or the product's name
//	limit   default 25, max 100
//	cursor  keyset cursor from a previous page
//
// "expired" is a real state worth reading: a hold with an expiry that nobody renewed has
// lapsed, which means automation is free to act again — the opposite of what the operator who
// placed it probably still believes.
func (s *Server) handleActiveHolds(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "database_not_configured",
			"no database is configured")
		return
	}

	query := r.URL.Query()
	status := strings.TrimSpace(query.Get("status"))
	switch status {
	case "":
		status = "active"
	case "active", "expired", "all":
	default:
		writeProblem(w, http.StatusBadRequest, "invalid_status",
			"status must be active, expired or all")
		return
	}

	limit, cursor, ok := s.pageParams(w, r)
	if !ok {
		return
	}

	args := []any{}
	where := []string{}
	switch status {
	case "active":
		where = append(where, `(h.expires_at IS NULL OR h.expires_at > now())`)
	case "expired":
		where = append(where, `(h.expires_at IS NOT NULL AND h.expires_at <= now())`)
	}
	if q := strings.TrimSpace(query.Get("q")); q != "" {
		args = append(args, "%"+q+"%")
		where = append(where, fmt.Sprintf(
			`(c.email ILIKE $%d OR c.name ILIKE $%d OR p.name ILIKE $%d)`,
			len(args), len(args), len(args)))
	}
	if cursor != "" {
		sortKey, id, err := paginate.Decode(cursor)
		if err != nil {
			writeProblem(w, http.StatusBadRequest, "invalid_cursor", "the cursor is invalid")
			return
		}
		when, err := time.Parse(time.RFC3339Nano, sortKey)
		if err != nil {
			writeProblem(w, http.StatusBadRequest, "invalid_cursor", "the cursor is invalid")
			return
		}
		if !validUUID(id) {
			writeProblem(w, http.StatusBadRequest, "invalid_cursor", "the cursor is invalid")
			return
		}
		args = append(args, when, id)
		where = append(where, fmt.Sprintf(
			`(h.created_at, h.id) < ($%d::timestamptz, $%d::uuid)`, len(args)-1, len(args)))
	}
	args = append(args, limit+1)

	clause := ""
	if len(where) > 0 {
		clause = " WHERE " + strings.Join(where, " AND ")
	}

	rows, err := s.pool.Query(r.Context(), `
		SELECT h.id, h.service_id, svc.status, svc.generation,
		       p.name, p.slug, c.id, c.name, c.email,
		       h.reason, h.created_at, h.expires_at, h.created_by,
		       COALESCE(u.email, ''), COALESCE(u.display_name, ''),
		       COALESCE(sub.status, ''), sub.next_billing_at,
		       COALESCE(inv.open_count, 0), COALESCE(inv.last_number, '')
		FROM service_holds h
		JOIN services svc ON svc.id = h.service_id
		JOIN customers c ON c.id = svc.customer_id
		JOIN products p ON p.id = svc.product_id
		-- created_by is text rather than a foreign key, so the join is on the rendered id and
		-- a hold written by a fixture simply has no matching user instead of breaking the list.
		LEFT JOIN users u ON u.id::text = h.created_by
		LEFT JOIN subscriptions sub ON sub.id = svc.subscription_id
		LEFT JOIN LATERAL (
			SELECT count(*) FILTER (WHERE i.status IN ('open', 'draft')) AS open_count,
			       (SELECT i2.number FROM invoices i2
			        WHERE i2.subscription_id = svc.subscription_id
			        ORDER BY i2.created_at DESC LIMIT 1) AS last_number
			FROM invoices i
			WHERE i.subscription_id = svc.subscription_id
		) inv ON true`+clause+`
		ORDER BY h.created_at DESC, h.id DESC
		LIMIT $`+fmt.Sprint(len(args)), args...)
	if err != nil {
		s.cfg.Logger.ErrorContext(r.Context(), "list holds failed", "error", err)
		writeProblem(w, http.StatusInternalServerError, "internal_error",
			"the request could not be completed")
		return
	}
	defer rows.Close()

	// Read limit+1 so "is there another page" is answered by the data rather than by a
	// count query that could disagree with it.
	scanned := []holdRegisterRow{}
	for rows.Next() {
		var row holdRegisterRow
		if err := rows.Scan(&row.ID, &row.ServiceID, &row.ServiceStatus, &row.Generation,
			&row.ProductName, &row.ProductSlug, &row.CustomerID, &row.CustomerName,
			&row.CustomerEmail, &row.Reason, &row.CreatedAt, &row.ExpiresAt, &row.CreatedBy,
			&row.PlacedByEmail, &row.PlacedByName, &row.SubStatus, &row.NextBillingAt,
			&row.OpenInvoices, &row.LastInvoiceNum); err != nil {
			s.cfg.Logger.ErrorContext(r.Context(), "scan hold failed", "error", err)
			writeProblem(w, http.StatusInternalServerError, "internal_error",
				"the request could not be completed")
			return
		}
		scanned = append(scanned, row)
	}
	if err := rows.Err(); err != nil {
		s.cfg.Logger.ErrorContext(r.Context(), "read holds failed", "error", err)
		writeProblem(w, http.StatusInternalServerError, "internal_error",
			"the request could not be completed")
		return
	}

	now := holdClock()
	var next *string
	if len(scanned) > limit {
		scanned = scanned[:limit]
		last := scanned[len(scanned)-1]
		cursor := paginate.Encode(last.CreatedAt.UTC().Format(time.RFC3339Nano), last.ID)
		next = &cursor
	}
	out := make([]map[string]any, 0, len(scanned))
	for _, row := range scanned {
		out = append(out, holdRegisterJSON(row, now))
	}

	counts, err := s.holdCounts(r)
	if err != nil {
		s.cfg.Logger.ErrorContext(r.Context(), "count holds failed", "error", err)
		writeProblem(w, http.StatusInternalServerError, "internal_error",
			"the request could not be completed")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"data":        out,
		"next_cursor": next,
		"counts":      counts,
		// The screen renders this so the sentence "the billing schedule keeps running" is a
		// fact from the database rather than a copywriter's assumption.
		"as_of": now.UTC(),
	})
}

// holdCounts summarises the register, because the header question is "how many services are
// we deliberately not touching" and a paginated list cannot answer it.
func (s *Server) holdCounts(r *http.Request) (map[string]any, error) {
	var active, expiring, lapsed int
	err := s.pool.QueryRow(r.Context(), `
		SELECT count(*) FILTER (WHERE h.expires_at IS NULL OR h.expires_at > now()),
		       count(*) FILTER (WHERE h.expires_at IS NOT NULL AND h.expires_at > now()
		                         AND h.expires_at <= now() + interval '7 days'),
		       count(*) FILTER (WHERE h.expires_at IS NOT NULL AND h.expires_at <= now())
		FROM service_holds h`).Scan(&active, &expiring, &lapsed)
	if err != nil {
		return nil, fmt.Errorf("holds: count: %w", err)
	}
	return map[string]any{
		"active":        active,
		"expiring_7d":   expiring,
		"lapsed":        lapsed,
		"never_expires": active - expiring,
	}, nil
}

// holdRegisterJSON is the wire shape of one hold.
//
// It carries no credential and nothing about the customer that the holds screen does not need
// to answer its question: which service, whose, since when, why, and who said so.
func holdRegisterJSON(row holdRegisterRow, now time.Time) map[string]any {
	out := map[string]any{
		"id":             row.ID,
		"service_id":     row.ServiceID,
		"service_status": row.ServiceStatus,
		"generation":     row.Generation,
		"product_name":   row.ProductName,
		"product_slug":   row.ProductSlug,
		"customer": map[string]any{
			"id":    row.CustomerID,
			"name":  row.CustomerName,
			"email": row.CustomerEmail,
		},
		"reason":     row.Reason,
		"created_at": row.CreatedAt.UTC(),
		"expires_at": nil,
		// expired is computed here rather than left to the client's clock: a hold that lapsed
		// is the difference between "we are not touching this" and "we are, again".
		"expired":         row.ExpiresAt != nil && !row.ExpiresAt.After(now),
		"placed_by":       nil,
		"subscription":    nil,
		"open_invoices":   row.OpenInvoices,
		"last_invoice_no": row.LastInvoiceNum,
	}
	if row.ExpiresAt != nil {
		out["expires_at"] = row.ExpiresAt.UTC()
	}
	switch {
	case row.PlacedByEmail != "" || row.PlacedByName != "":
		out["placed_by"] = map[string]any{
			"user_id":      row.CreatedBy,
			"email":        row.PlacedByEmail,
			"display_name": row.PlacedByName,
		}
	case row.CreatedBy != "":
		// A hold written by something that is not a user (an importer, a fixture, a
		// migration). The value is shown rather than hidden, because "who placed this" must
		// never be blank.
		out["placed_by"] = map[string]any{
			"user_id":      "",
			"email":        "",
			"display_name": row.CreatedBy,
		}
	}
	if row.SubStatus != "" || row.NextBillingAt != nil {
		sub := map[string]any{"status": row.SubStatus, "next_billing_at": nil}
		if row.NextBillingAt != nil {
			sub["next_billing_at"] = row.NextBillingAt.UTC()
		}
		out["subscription"] = sub
	}
	return out
}

// holdClock is the clock this file reads, kept in one place so a response and its rows agree
// about whether a hold has lapsed.
func holdClock() time.Time { return time.Now().UTC() }
