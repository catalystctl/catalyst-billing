// Currency configuration and the public switcher.
//
// The table supplies presentation only — symbol, position, grouping — because `money` keeps
// integer minor units and refuses cross-currency arithmetic. So this surface cannot change
// what a price *is*, only how it renders, which is why it is safe for the storefront to read.
//
// The public read is deliberately narrow: enabled currencies, default first, and nothing
// about which prices exist. A visitor needs to know what they can switch to, not what the
// shop's margins are.

package httpapi

import (
	"context"
	"net/http"
	"strings"

	"github.com/catalyst/billing/internal/authn"
	"github.com/catalyst/billing/internal/commerce"
	"github.com/jackc/pgx/v5"
)

// registerCurrencyRoutes is the staff currency surface plus the public switcher.
func registerCurrencyRoutes(r *router, s *Server) {
	// Public: the storefront renders a currency switcher before anyone signs in.
	r.publicGet("/api/v1/currencies", s.handlePublicCurrencies)

	r.staffGet("/api/v1/staff/currencies", authn.PermCurrencyWrite, s.handleListCurrencies)
	r.staffMutation(http.MethodPost, "/api/v1/staff/currencies", authn.PermCurrencyWrite, s.handleCreateCurrency)
	r.staffMutation(http.MethodPut, "/api/v1/staff/currencies/{code}", authn.PermCurrencyWrite, s.handleUpdateCurrency)
	r.staffMutation(http.MethodDelete, "/api/v1/staff/currencies/{code}", authn.PermCurrencyWrite, s.handleDeleteCurrency)
}

// currencyJSON is the wire shape. `example` renders 123456 minor units in the configured
// style, which is what the settings screen previews and what makes a misconfigured grouping
// visible before a customer sees it.
func currencyJSON(c commerce.Currency) map[string]any {
	return map[string]any{
		"code":            c.Code,
		"name":            c.Name,
		"scale":           int(c.Scale),
		"symbol":          c.Symbol,
		"symbol_position": c.SymbolPosition,
		"group_separator": c.GroupSeparator,
		"decimal_mark":    c.DecimalMark,
		"group_size":      c.GroupSize,
		"is_default":      c.IsDefault,
		"enabled":         c.Enabled,
		"sort_order":      c.SortOrder,
		"example":         c.Format(123456),
		"created_at":      c.CreatedAt.UTC(),
		"updated_at":      c.UpdatedAt.UTC(),
	}
}

// handlePublicCurrencies serves the switcher.
func (s *Server) handlePublicCurrencies(w http.ResponseWriter, r *http.Request) {
	currencies, err := s.commerceStore().EnabledCurrencies(r.Context())
	if err != nil {
		s.commerceError(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(currencies))
	for _, c := range currencies {
		// The public shape omits the sort order and the timestamps: they are operational
		// detail, and the switcher only needs to render a list.
		out = append(out, map[string]any{
			"code":            c.Code,
			"name":            c.Name,
			"scale":           int(c.Scale),
			"symbol":          c.Symbol,
			"symbol_position": c.SymbolPosition,
			"is_default":      c.IsDefault,
			"example":         c.Format(123456),
		})
	}
	// A short cache: the list changes when an operator edits it, and a minute of staleness
	// is a better trade than every anonymous visitor hitting the database.
	w.Header().Set("Cache-Control", "public, max-age=60")
	writeJSON(w, http.StatusOK, map[string]any{"data": out})
}

// handleListCurrencies lists every currency, disabled ones included.
func (s *Server) handleListCurrencies(w http.ResponseWriter, r *http.Request) {
	currencies, err := s.commerceStore().Currencies(r.Context())
	if err != nil {
		s.commerceError(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(currencies))
	for _, c := range currencies {
		out = append(out, currencyJSON(c))
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out})
}

// handleCreateCurrency adds a currency.
func (s *Server) handleCreateCurrency(w http.ResponseWriter, r *http.Request) {
	var in commerce.CurrencyInput
	if !s.decodeJSON(w, r, &in, 64<<10) {
		return
	}
	var created commerce.Currency
	// The audit target is a uuid column, and a currency's primary key is its code, so the
	// row records the code in `changes` and leaves `target_id` empty. Writing a non-uuid into
	// a uuid column would fail the whole transaction, which is how this was caught.
	err := s.withCommerceAudit(r, "currency.create", "currency", "",
		map[string]any{"code": strings.ToUpper(in.Code)},
		func(ctx context.Context, tx pgx.Tx) error {
			var err error
			created, err = s.commerceStore().CreateCurrencyTx(ctx, tx, in)
			return err
		})
	if err != nil {
		s.commerceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, currencyJSON(created))
}

// handleUpdateCurrency edits a currency's presentation or its default state.
func (s *Server) handleUpdateCurrency(w http.ResponseWriter, r *http.Request) {
	code := r.PathValue("code")
	var in commerce.CurrencyInput
	if !s.decodeJSON(w, r, &in, 64<<10) {
		return
	}
	var updated commerce.Currency
	err := s.withCommerceAudit(r, "currency.update", "currency", "",
		map[string]any{"code": strings.ToUpper(code)},
		func(ctx context.Context, tx pgx.Tx) error {
			var err error
			updated, err = s.commerceStore().UpdateCurrencyTx(ctx, tx, code, in)
			return err
		})
	if err != nil {
		s.commerceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, currencyJSON(updated))
}

// handleDeleteCurrency removes a currency that has never been used.
func (s *Server) handleDeleteCurrency(w http.ResponseWriter, r *http.Request) {
	code := r.PathValue("code")
	err := s.withCommerceAudit(r, "currency.delete", "currency", "",
		map[string]any{"code": strings.ToUpper(code)},
		func(ctx context.Context, tx pgx.Tx) error {
			return s.commerceStore().DeleteCurrencyTx(ctx, tx, code)
		})
	if err != nil {
		s.commerceError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
