// The service-property catalogue: the definitions, their order, and how many services carry
// each one.
//
// The definitions themselves already have routes (service_metadata.go: list, save on the key,
// delete). What the screen could not do is the two things an operator actually needs:
//
//   - **Reorder.** `sort_order` is part of the definition and the list is ordered by it, but
//     the only way to change it was to edit one row at a time and guess at the numbers in
//     between. A reorder is one request naming the whole order, because that is what the
//     operator did on screen — and because two single-row edits that each renumber are two
//     chances to end up with two properties claiming the same place.
//   - **See what is in use.** Deleting a definition that a service still carries is refused by
//     the database (a trigger, migration 0029) and reported as 409 `property_in_use`. That is
//     the correct refusal, but it arrives *after* the click. The usage read makes it visible
//     before: "3 services carry this key" turns a failed delete into a decision.
//
// A note on what a label edit costs, because it is the one thing about this catalogue that is
// not reversible: an invoice's property lines are rendered from the definitions at print time
// (InvoiceProperties), so renaming `rack` changes what a re-generated historical invoice says.
// The value on the document is the operator's; the words around it move when the catalogue
// moves. That is worth knowing before the click, so the screen says it.
package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/catalyst/billing/internal/audit"
	"github.com/catalyst/billing/internal/authn"
	"github.com/catalyst/billing/internal/service"
	"github.com/jackc/pgx/v5"
)

func init() {
	RegisterRoutes("64-service-property-catalogue", registerServicePropertyCatalogue)
}

func registerServicePropertyCatalogue(r *router) {
	// Reading usage is a read of the installation's configuration, so it shares the permission
	// with the definition list rather than with the writes: support can see why a delete will be
	// refused without being able to delete anything.
	r.staffGet("/api/v1/staff/service-properties/usage", authn.PermServiceReadAll,
		r.s.handlePropertyUsage)
	// The literal path sorts before the {key} routes in the mux, and the method differs anyway,
	// so a definition that happened to be called "reorder" could not shadow this.
	r.staffMutation(http.MethodPost, "/api/v1/staff/service-properties/reorder",
		authn.PermServiceWriteAll, r.s.handleReorderServiceProperties)
}

// propertyUsageJSON is one definition's usage.
type propertyUsageJSON struct {
	Key string `json:"key"`
	// Services is how many services carry the key at all. It counts presence, not a non-empty
	// value, because that is what the database's delete guard tests: a service holding
	// `{"rack": ""}` still blocks the delete, and a count that ignored it would tell the
	// operator a delete would succeed and then refuse it.
	Services int `json:"services"`
}

// handlePropertyUsage reports, per definition, how many services carry it.
//
// One query with a left join rather than a count per definition: the screen renders every row,
// and N+1 round trips to draw a table is how a settings page takes two seconds.
func (s *Server) handlePropertyUsage(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "database_not_configured",
			"no database is configured")
		return
	}
	rows, err := s.pool.Query(r.Context(), `
		SELECT d.key, count(s.id)
		FROM service_property_definitions d
		LEFT JOIN services s ON s.custom_properties ? d.key
		GROUP BY d.key
		ORDER BY d.key`)
	if err != nil {
		s.cfg.Logger.ErrorContext(r.Context(), "property usage failed", "error", err)
		writeProblem(w, http.StatusInternalServerError, "internal_error",
			"the request could not be completed")
		return
	}
	defer rows.Close()

	out := make([]propertyUsageJSON, 0, 16)
	for rows.Next() {
		var row propertyUsageJSON
		if err := rows.Scan(&row.Key, &row.Services); err != nil {
			s.cfg.Logger.ErrorContext(r.Context(), "property usage scan failed", "error", err)
			writeProblem(w, http.StatusInternalServerError, "internal_error",
				"the request could not be completed")
			return
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		s.cfg.Logger.ErrorContext(r.Context(), "property usage rows failed", "error", err)
		writeProblem(w, http.StatusInternalServerError, "internal_error",
			"the request could not be completed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out})
}

// propertyReorderRequest is the body of a reorder.
type propertyReorderRequest struct {
	// Keys is the whole order, most significant first. A definition omitted from the list is not
	// deleted: it keeps its relative place after the named ones, which is what makes the request
	// safe when another operator adds a definition between the render and the save.
	Keys []string `json:"keys"`
}

// handleReorderServiceProperties renumbers the definitions in the order given.
//
// The numbers are multiples of ten so a later single-row edit has room to slot something in
// between without renumbering everything, which is the reason the original rows were seeded
// that way.
func (s *Server) handleReorderServiceProperties(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "database_not_configured",
			"no database is configured")
		return
	}
	var req propertyReorderRequest
	if !s.decodeJSON(w, r, &req, 32<<10) {
		return
	}

	// Validate the shape before touching the database: a duplicate key would make the UPDATE
	// assign two positions to one row and silently lose the other, which is the failure mode a
	// reorder must not have.
	named := make([]string, 0, len(req.Keys))
	seen := make(map[string]struct{}, len(req.Keys))
	for _, raw := range req.Keys {
		key := strings.TrimSpace(raw)
		if key == "" {
			writeProblem(w, http.StatusUnprocessableEntity, "invalid_order",
				"the order contains an empty key")
			return
		}
		if _, dup := seen[key]; dup {
			writeProblem(w, http.StatusUnprocessableEntity, "invalid_order",
				"the order names "+key+" twice")
			return
		}
		seen[key] = struct{}{}
		named = append(named, key)
	}
	if len(named) == 0 {
		writeProblem(w, http.StatusUnprocessableEntity, "invalid_order",
			"send the property keys in the order you want them")
		return
	}

	var order []string
	err := s.pool.InTx(r.Context(), func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT key FROM service_property_definitions ORDER BY sort_order, key`)
		if err != nil {
			return err
		}
		existing := make([]string, 0, 16)
		present := make(map[string]struct{}, 16)
		for rows.Next() {
			var key string
			if err := rows.Scan(&key); err != nil {
				rows.Close()
				return err
			}
			existing = append(existing, key)
			present[key] = struct{}{}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}

		for _, key := range named {
			if _, ok := present[key]; !ok {
				// The same sentinel the metadata editor uses for an unknown key, so the screen
				// branches on one code (400 unknown_property) whether the key was typed into a
				// service's properties or into an order.
				return fmt.Errorf("%w: %s", service.ErrUnknownProperty, key)
			}
		}

		// The named keys first, in the order given; everything else after them in the order it
		// already had. Nothing is dropped, so a concurrent create cannot be lost by a reorder
		// that never saw it.
		order = make([]string, 0, len(existing))
		order = append(order, named...)
		for _, key := range existing {
			if _, ok := seen[key]; ok {
				continue
			}
			order = append(order, key)
		}

		if _, err := tx.Exec(ctx, `
			UPDATE service_property_definitions d
			SET sort_order = v.ord * 10
			FROM (
				SELECT unnest($1::text[]) AS key,
				       generate_subscripts($1::text[], 1) - 1 AS ord
			) v
			WHERE d.key = v.key
			  AND d.sort_order <> v.ord * 10`, order); err != nil {
			return err
		}

		// The audit row is written inside the transaction that made the change, so a reorder and
		// the record of who made it cannot disagree.
		ev := s.auditEvent(r, "service.property_definition_reordered", "service_property", "",
			audit.OutcomeSuccess).WithChanges(map[string]any{"order": order})
		return audit.Record(ctx, tx, ev)
	})
	if err != nil {
		if errors.Is(err, service.ErrUnknownProperty) {
			writeProblem(w, http.StatusBadRequest, "unknown_property", err.Error())
			return
		}
		s.cfg.Logger.ErrorContext(r.Context(), "property reorder failed", "error", err)
		writeProblem(w, http.StatusInternalServerError, "internal_error",
			"the order could not be saved")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"keys": order})
}
