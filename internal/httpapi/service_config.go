// Configurable product options over HTTP (UPG-07…UPG-11, SVC-07).
//
// Two audiences again:
//
//   - Staff define an option and price each choice per price version. This is catalog
//     configuration, so it needs config:write rather than catalog:write: changing an option
//     changes what every subscription on that product renews at.
//   - A customer reads the selection on their own service and may change an *upgradable*
//     option, which issues a prorated charge exactly like an upgrade.
//
// The priced selection is returned as a breakdown rather than a total, because a total a
// customer cannot check is a total they have to trust. The resolver that produces it is the
// same one the renewal engine will call, so the quote and the renewal cannot disagree.
package httpapi

import (
	"net/http"

	"github.com/catalyst/billing/internal/authn"
	"github.com/catalyst/billing/internal/portal"
	"github.com/catalyst/billing/internal/service"
)

func init() {
	RegisterRoutes("62-service-config", registerServiceConfig)
}

func registerServiceConfig(r *router) {
	// Staff: the option CRUD and its per-version pricing.
	r.staffGet("/api/v1/staff/config-options", authn.PermInvoiceReadAll, r.s.handleListConfigOptions)
	r.staffMutation(http.MethodPost, "/api/v1/staff/config-options",
		authn.PermConfigWrite, r.s.handleCreateConfigOption)
	r.staffMutation(http.MethodPut, "/api/v1/staff/config-options/{id}",
		authn.PermConfigWrite, r.s.handleUpdateConfigOption)
	r.staffMutation(http.MethodDelete, "/api/v1/staff/config-options/{id}",
		authn.PermConfigWrite, r.s.handleDeleteConfigOption)
	r.staffMutation(http.MethodPut, "/api/v1/staff/config-options/{id}/prices",
		authn.PermConfigWrite, r.s.handleSetConfigOptionPrice)
	r.staffMutation(http.MethodDelete, "/api/v1/staff/config-options/{id}/prices",
		authn.PermConfigWrite, r.s.handleDeleteConfigOptionPrice)
	// A resolver probe: what would this selection cost on this price version. It is a read
	// that runs the same function checkout and renewal run, so a support agent can answer
	// "why is this service 42.00" without reading the database.
	r.staffGet("/api/v1/staff/config-options/resolve", authn.PermInvoiceReadAll,
		r.s.handleResolveConfigOptions)

	// Customer: the selection on their own service, and changing an upgradable option.
	r.get("/api/v1/portal/services/{id}/config", authn.PermServiceReadSelf,
		r.s.handlePortalServiceConfig)
	r.mutation(http.MethodPut, "/api/v1/portal/services/{id}/config",
		authn.PermServiceUpgradeSelf, r.s.handleChangeServiceConfig)
}

// configOptionRequest is the body of a create or edit.
type configOptionRequest struct {
	ProductID   string   `json:"product_id"`
	ParentID    *string  `json:"parent_id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Type        string   `json:"type"`
	Choices     []string `json:"choices"`
	EnvVariable string   `json:"env_variable"`
	Required    bool     `json:"required"`
	Hidden      bool     `json:"hidden"`
	// Upgradable is a pointer so an edit that omits it preserves the current value.
	Upgradable *bool `json:"upgradable"`
	SortOrder  int   `json:"sort_order"`
}

func (req configOptionRequest) input() service.ConfigOptionInput {
	return service.ConfigOptionInput{
		ProductID:   req.ProductID,
		ParentID:    req.ParentID,
		Name:        req.Name,
		Description: req.Description,
		Type:        req.Type,
		Choices:     req.Choices,
		EnvVariable: req.EnvVariable,
		Required:    req.Required,
		Hidden:      req.Hidden,
		Upgradable:  req.Upgradable,
		SortOrder:   req.SortOrder,
	}
}

// configOptionJSON is the wire form of an option.
func configOptionJSON(o service.ConfigOption) map[string]any {
	out := map[string]any{
		"id":           o.ID,
		"product_id":   o.ProductID,
		"name":         o.Name,
		"description":  o.Description,
		"type":         o.Type,
		"choices":      o.Choices,
		"env_variable": o.EnvVariable,
		"required":     o.Required,
		"hidden":       o.Hidden,
		"upgradable":   o.Upgradable,
		"sort_order":   o.SortOrder,
		"created_at":   o.CreatedAt.UTC(),
		"updated_at":   o.UpdatedAt.UTC(),
	}
	if o.ParentID != nil {
		out["parent_id"] = *o.ParentID
	}
	prices := make([]map[string]any, 0, len(o.Prices))
	for _, p := range o.Prices {
		prices = append(prices, map[string]any{
			"id":               p.ID,
			"price_version_id": p.PriceVersionID,
			"choice":           p.Choice,
			"amount_minor":     p.AmountMinor,
			"setup_minor":      p.SetupMinor,
		})
	}
	out["prices"] = prices
	return out
}

// handleListConfigOptions lists a product's options with their prices.
//
// Query: product_id (required).
func (s *Server) handleListConfigOptions(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "database_not_configured",
			"no database is configured")
		return
	}
	productID := r.URL.Query().Get("product_id")
	if !portal.ValidID(productID) {
		writeProblem(w, http.StatusBadRequest, "invalid_product",
			"product_id is required and must be a product id")
		return
	}
	options, err := s.serviceStore(nil).ConfigOptions(r.Context(), productID)
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(options))
	for _, o := range options {
		out = append(out, configOptionJSON(o))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"options": out,
		"types":   service.ConfigOptionTypes,
	})
}

// handleCreateConfigOption creates an option.
func (s *Server) handleCreateConfigOption(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "database_not_configured",
			"no database is configured")
		return
	}
	var req configOptionRequest
	if !s.decodeJSON(w, r, &req, 32<<10) {
		return
	}
	out, err := s.serviceStore(r).CreateConfigOption(r.Context(), req.input())
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, configOptionJSON(out))
}

// handleUpdateConfigOption edits an option.
func (s *Server) handleUpdateConfigOption(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "database_not_configured",
			"no database is configured")
		return
	}
	id := r.PathValue("id")
	if !portal.ValidID(id) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such record")
		return
	}
	var req configOptionRequest
	if !s.decodeJSON(w, r, &req, 32<<10) {
		return
	}
	out, err := s.serviceStore(r).UpdateConfigOption(r.Context(), id, req.input())
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, configOptionJSON(out))
}

// handleDeleteConfigOption removes an option no live service has selected.
func (s *Server) handleDeleteConfigOption(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "database_not_configured",
			"no database is configured")
		return
	}
	id := r.PathValue("id")
	if !portal.ValidID(id) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such record")
		return
	}
	if err := s.serviceStore(r).DeleteConfigOption(r.Context(), id); err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "deleted": true})
}

// configOptionPriceRequest sets one choice's amount on one price version.
type configOptionPriceRequest struct {
	PriceVersionID string `json:"price_version_id"`
	Choice         string `json:"choice"`
	AmountMinor    int64  `json:"amount_minor"`
	SetupMinor     int64  `json:"setup_minor"`
}

// handleSetConfigOptionPrice sets a choice's amount and setup fee.
func (s *Server) handleSetConfigOptionPrice(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "database_not_configured",
			"no database is configured")
		return
	}
	id := r.PathValue("id")
	if !portal.ValidID(id) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such record")
		return
	}
	var req configOptionPriceRequest
	if !s.decodeJSON(w, r, &req, 16<<10) {
		return
	}
	if !portal.ValidID(req.PriceVersionID) {
		writeProblem(w, http.StatusBadRequest, "invalid_price_version",
			"price_version_id must be a price version id")
		return
	}
	out, err := s.serviceStore(r).SetConfigOptionPrice(r.Context(), id, req.PriceVersionID,
		req.Choice, req.AmountMinor, req.SetupMinor)
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":               out.ID,
		"config_option_id": out.ConfigOptionID,
		"price_version_id": out.PriceVersionID,
		"choice":           out.Choice,
		"amount_minor":     out.AmountMinor,
		"setup_minor":      out.SetupMinor,
	})
}

// handleDeleteConfigOptionPrice removes one choice's price.
func (s *Server) handleDeleteConfigOptionPrice(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "database_not_configured",
			"no database is configured")
		return
	}
	id := r.PathValue("id")
	if !portal.ValidID(id) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such record")
		return
	}
	var req configOptionPriceRequest
	if !s.decodeJSON(w, r, &req, 16<<10) {
		return
	}
	if !portal.ValidID(req.PriceVersionID) {
		writeProblem(w, http.StatusBadRequest, "invalid_price_version",
			"price_version_id must be a price version id")
		return
	}
	if err := s.serviceStore(r).DeleteConfigOptionPrice(r.Context(), id, req.PriceVersionID, req.Choice); err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
}

// handleResolveConfigOptions prices a selection.
//
// Query: product_id, price_version_id, and one `option.<id>=<value>` pair per selection. The
// option prefix keeps the selection distinguishable from the two required parameters, so a
// caller cannot smuggle a price version in as an option.
func (s *Server) handleResolveConfigOptions(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "database_not_configured",
			"no database is configured")
		return
	}
	query := r.URL.Query()
	productID := query.Get("product_id")
	priceVersionID := query.Get("price_version_id")
	if !portal.ValidID(productID) || !portal.ValidID(priceVersionID) {
		writeProblem(w, http.StatusBadRequest, "invalid_request",
			"product_id and price_version_id are required")
		return
	}
	selection := map[string]string{}
	for key, values := range query {
		if len(values) == 0 {
			continue
		}
		const prefix = "option."
		if len(key) <= len(prefix) || key[:len(prefix)] != prefix {
			continue
		}
		selection[key[len(prefix):]] = values[0]
	}

	resolved, err := s.serviceStore(nil).ResolveSelection(r.Context(), productID, priceVersionID, selection)
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, resolvedSelectionJSON(resolved))
}

// resolvedSelectionJSON is the priced selection.
func resolvedSelectionJSON(r service.ResolvedSelection) map[string]any {
	options := make([]map[string]any, 0, len(r.Options))
	for _, o := range r.Options {
		options = append(options, map[string]any{
			"config_option_id": o.ConfigOptionID,
			"name":             o.Name,
			"env_variable":     o.EnvVariable,
			"value":            o.Value,
			"amount":           amountJSON(o.Amount),
			"setup":            amountJSON(o.Setup),
		})
	}
	return map[string]any{
		"price_version_id": r.PriceVersionID,
		"currency":         r.Currency,
		"options":          options,
		"total":            amountJSON(r.Total),
		"setup_total":      amountJSON(r.SetupTotal),
		"env":              r.Env,
	}
}

// serviceConfigJSON is one stored selection.
func serviceConfigJSON(c service.ServiceConfig) map[string]any {
	return map[string]any{
		"id":               c.ID,
		"config_option_id": c.ConfigOptionID,
		"name":             c.Name,
		"type":             c.Type,
		"description":      c.Description,
		"env_variable":     c.EnvVariable,
		"choices":          c.Choices,
		"value":            c.Value,
		"amount_minor":     c.AmountMinor,
		"setup_minor":      c.SetupMinor,
		"upgradable":       c.Upgradable,
		"created_at":       c.CreatedAt.UTC(),
	}
}

// handlePortalServiceConfig returns the selection stored on the caller's service.
func (s *Server) handlePortalServiceConfig(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "database_not_configured",
			"no database is configured")
		return
	}
	id := r.PathValue("id")
	if !portal.ValidID(id) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such record")
		return
	}
	user, _ := userFrom(r.Context())
	if err := s.serviceStore(nil).ServiceOwnedBy(r.Context(), id, user.ID); err != nil {
		s.writeServiceError(w, r, err)
		return
	}

	configs, err := s.serviceStore(nil).ServiceConfigs(r.Context(), id)
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(configs))
	for _, c := range configs {
		out = append(out, serviceConfigJSON(c))
	}
	writeJSON(w, http.StatusOK, map[string]any{"config": out})
}

// serviceConfigChangeRequest changes one option on a live service.
type serviceConfigChangeRequest struct {
	ConfigOptionID string `json:"config_option_id"`
	Value          string `json:"value"`
	Reason         string `json:"reason"`
}

// handleChangeServiceConfig changes an upgradable option and charges the prorated delta.
func (s *Server) handleChangeServiceConfig(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "database_not_configured",
			"no database is configured")
		return
	}
	id := r.PathValue("id")
	if !portal.ValidID(id) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such record")
		return
	}
	var req serviceConfigChangeRequest
	if !s.decodeJSON(w, r, &req, 16<<10) {
		return
	}
	if !portal.ValidID(req.ConfigOptionID) {
		writeProblem(w, http.StatusBadRequest, "invalid_config_option",
			"config_option_id must be a config option id")
		return
	}
	user, _ := userFrom(r.Context())

	res, err := s.serviceStore(r).ChangeServiceConfig(r.Context(), service.ConfigChangeRequest{
		ServiceID:      id,
		ConfigOptionID: req.ConfigOptionID,
		Value:          req.Value,
		UserID:         user.ID,
		Reason:         req.Reason,
	})
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	s.publishServiceEvent(r, id, "service.config_changed", map[string]any{
		"service_id": id, "upgrade_id": res.UpgradeID,
		"config_option_id": req.ConfigOptionID, "value": req.Value,
	})
	writeJSON(w, http.StatusCreated, upgradeResultJSON(res))
}
