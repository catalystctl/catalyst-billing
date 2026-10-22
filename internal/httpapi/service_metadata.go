// Service metadata over HTTP (SVC-02, SVC-03, SVC-04, SVC-07, SVC-08).
//
// Two audiences, and the split is the permission:
//
//   - A customer renames and describes their own service with service:write:self. Every
//     statement behind that route carries the ownership predicate in SQL, so another
//     customer's service id answers 404 — the same answer as an id that does not exist.
//   - Staff read the whole record — the label, the description, the plan, the quantity, the
//     price and the properties — with service:read:all, and edit the metadata with
//     service:write:all. The property *definitions* are installation configuration, so they
//     live behind the same permission: whether a property prints on an invoice is an operator
//     decision, not a support one.
//
// The metadata response is deliberately one object rather than four: "what is this service?"
// is a question, and answering it with a label, a plan, a price and a property list that a
// client has to join is how two screens end up disagreeing about the same service.
//
// Three wire rules:
//
//   - Money is integer minor units plus an explicit currency (`amountJSON`), never a float.
//   - An absent field means "leave it alone"; an empty string clears it. A customer renaming
//     their service must not blank the description an operator wrote.
//   - The `label` and `description` a customer reads are the *effective* ones: the written text
//     when there is one, the text derived from the product and the selected options otherwise.
//     Both are returned, so a screen can show what will be shown and offer the derived text as
//     a starting point for an edit.
package httpapi

import (
	"errors"
	"net/http"

	"github.com/catalyst/billing/internal/authn"
	"github.com/catalyst/billing/internal/portal"
	"github.com/catalyst/billing/internal/service"
)

func init() {
	RegisterRoutes("63-service-metadata", registerServiceMetadata)
}

func registerServiceMetadata(r *router) {
	// Customer: the metadata of their own service, and renaming it.
	r.get("/api/v1/portal/services/{id}/metadata", authn.PermServiceReadSelf,
		r.s.handlePortalServiceMetadata)
	r.mutation(http.MethodPut, "/api/v1/portal/services/{id}/metadata",
		authn.PermServiceWriteSelf, r.s.handleUpdatePortalServiceMetadata)

	// Staff: the whole record, and the properties.
	r.staffGet("/api/v1/staff/services/{id}/metadata", authn.PermServiceReadAll,
		r.s.handleStaffServiceMetadata)
	r.staffMutation(http.MethodPut, "/api/v1/staff/services/{id}/metadata",
		authn.PermServiceWriteAll, r.s.handleUpdateStaffServiceMetadata)

	r.staffGet("/api/v1/staff/service-properties", authn.PermServiceReadAll,
		r.s.handleListServiceProperties)
	r.staffMutation(http.MethodPut, "/api/v1/staff/service-properties/{key}",
		authn.PermServiceWriteAll, r.s.handleSaveServiceProperty)
	r.staffMutation(http.MethodDelete, "/api/v1/staff/service-properties/{key}",
		authn.PermServiceWriteAll, r.s.handleDeleteServiceProperty)
}

// serviceMetadataRequest is the body of a metadata edit.
//
// The two text fields are pointers so "not mentioned" and "set to empty" stay different. A
// customer's route rejects `custom_properties` explicitly rather than ignoring it: a client that
// believes it set a property must be told it did not.
type serviceMetadataRequest struct {
	Label            *string            `json:"label"`
	Description      *string            `json:"description"`
	CustomProperties *map[string]string `json:"custom_properties"`
}

// update turns the body into a store request.
func (req serviceMetadataRequest) update(serviceID, userID string, staff bool) service.MetadataUpdate {
	out := service.MetadataUpdate{
		ServiceID:   serviceID,
		UserID:      userID,
		Staff:       staff,
		Label:       req.Label,
		Description: req.Description,
	}
	if req.CustomProperties != nil {
		out.CustomProperties = *req.CustomProperties
	}
	return out
}

// propertyValueJSON is one property with its definition attached.
func propertyValueJSON(p service.PropertyValue) map[string]any {
	return map[string]any{
		"key":              p.Key,
		"label":            p.Label,
		"description":      p.Description,
		"value":            p.Value,
		"show_on_invoice":  p.ShowOnInvoice,
		"customer_visible": p.CustomerVisible,
		"sort_order":       p.SortOrder,
	}
}

// propertyDefinitionJSON is the wire form of a definition.
func propertyDefinitionJSON(d service.PropertyDefinition) map[string]any {
	return map[string]any{
		"key":              d.Key,
		"label":            d.Label,
		"description":      d.Description,
		"show_on_invoice":  d.ShowOnInvoice,
		"customer_visible": d.CustomerVisible,
		"sort_order":       d.SortOrder,
		"created_at":       d.CreatedAt.UTC(),
		"updated_at":       d.UpdatedAt.UTC(),
	}
}

// serviceMetadataJSON is the one description of "what is this service?".
//
// `portal` selects the customer's view of the same object. The differences are the ones the
// customer must not see — the internal order id and the properties the operator marked
// customer-invisible — rather than a second shape, so a staff screen and the customer's own
// page cannot disagree about the parts they share.
func serviceMetadataJSON(m service.ServiceMetadata, portalView bool) map[string]any {
	properties := make([]map[string]any, 0, len(m.Properties))
	for _, p := range m.Properties {
		properties = append(properties, propertyValueJSON(p))
	}
	config := make([]map[string]any, 0, len(m.Config))
	for _, c := range m.Config {
		config = append(config, map[string]any{
			"config_option_id": c.ConfigOptionID,
			"name":             c.Name,
			"type":             c.Type,
			"value":            c.Value,
			"amount_minor":     c.AmountMinor,
			"setup_minor":      c.SetupMinor,
		})
	}

	plan := map[string]any{
		"price_version_id": m.PriceVersionID,
		"name":             m.PlanName,
		"interval":         m.Interval,
		"currency":         m.Currency,
		"amount_minor":     m.AmountMinor,
		"setup_minor":      m.SetupMinor,
	}
	if amount, ok := m.PriceAmount(); ok {
		plan["amount"] = amountJSON(amount)
	}

	out := map[string]any{
		"service_id": m.ServiceID,
		"status":     m.Status,
		"customer": map[string]any{
			"id":    m.CustomerID,
			"email": m.CustomerEmail,
		},
		"product": map[string]any{
			"id":   m.ProductID,
			"name": m.ProductName,
		},
		"plan": plan,
		// SVC-07: what a support agent needs to answer "how many, and at what price".
		"quantity": m.Quantity,
		// What a human wrote, and what to display. Both, because the edit form needs the
		// first and the heading needs the second.
		"label":                 m.Label,
		"description":           m.Description,
		"effective_label":       m.EffectiveLabel,
		"effective_description": m.EffectiveDescription,
		"derived_description":   m.DerivedDescription,
		"custom_properties":     m.CustomProperties,
		"properties":            properties,
		"config":                config,
		"updated_at":            m.UpdatedAt.UTC(),
	}
	if !portalView && m.OrderID != nil {
		out["order_id"] = *m.OrderID
	}
	return out
}

// handleStaffServiceMetadata returns the whole record for a support agent.
func (s *Server) handleStaffServiceMetadata(w http.ResponseWriter, r *http.Request) {
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
	m, err := s.serviceStore(nil).ServiceMetadata(r.Context(), id)
	if err != nil {
		s.writeServiceMetadataError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, serviceMetadataJSON(m, false))
}

// handleUpdateStaffServiceMetadata edits the label, the description and the properties.
func (s *Server) handleUpdateStaffServiceMetadata(w http.ResponseWriter, r *http.Request) {
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
	var req serviceMetadataRequest
	if !s.decodeJSON(w, r, &req, 32<<10) {
		return
	}
	user, _ := userFrom(r.Context())

	m, err := s.serviceStore(r).UpdateServiceMetadata(r.Context(),
		req.update(id, user.ID, true))
	if err != nil {
		s.writeServiceMetadataError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, serviceMetadataJSON(m, false))
}

// handlePortalServiceMetadata returns the metadata of the caller's own service.
func (s *Server) handlePortalServiceMetadata(w http.ResponseWriter, r *http.Request) {
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

	m, err := s.serviceStore(nil).ServiceMetadataOwnedBy(r.Context(), id, user.ID)
	if err != nil {
		s.writeServiceMetadataError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, serviceMetadataJSON(m, true))
}

// handleUpdatePortalServiceMetadata lets a customer rename and describe their own service.
func (s *Server) handleUpdatePortalServiceMetadata(w http.ResponseWriter, r *http.Request) {
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
	var req serviceMetadataRequest
	if !s.decodeJSON(w, r, &req, 16<<10) {
		return
	}
	if req.CustomProperties != nil {
		// Named explicitly: a customer who thinks they set a property must be told that the
		// installation's record is the operator's, not silently ignored.
		writeProblem(w, http.StatusForbidden, "forbidden",
			"custom properties are recorded by staff")
		return
	}
	user, _ := userFrom(r.Context())

	m, err := s.serviceStore(r).UpdateServiceMetadata(r.Context(),
		req.update(id, user.ID, false))
	if err != nil {
		s.writeServiceMetadataError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, serviceMetadataJSON(m, true))
}

// handleListServiceProperties returns the definitions an operator has configured.
func (s *Server) handleListServiceProperties(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "database_not_configured",
			"no database is configured")
		return
	}
	definitions, err := s.serviceStore(nil).PropertyDefinitions(r.Context())
	if err != nil {
		s.writeServiceMetadataError(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(definitions))
	for _, d := range definitions {
		out = append(out, propertyDefinitionJSON(d))
	}
	writeJSON(w, http.StatusOK, map[string]any{"properties": out})
}

// servicePropertyRequest is the body of a definition create or edit.
type servicePropertyRequest struct {
	Label           string `json:"label"`
	Description     string `json:"description"`
	ShowOnInvoice   bool   `json:"show_on_invoice"`
	CustomerVisible *bool  `json:"customer_visible"`
	SortOrder       int    `json:"sort_order"`
}

// handleSaveServiceProperty creates or edits one definition.
//
// It is a PUT on the key rather than a POST plus a PATCH because the key is the identity: the
// key is what every service's value is written against, and re-keying a definition would orphan
// the values. Editing the label is the supported change.
func (s *Server) handleSaveServiceProperty(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "database_not_configured",
			"no database is configured")
		return
	}
	key := r.PathValue("key")
	var req servicePropertyRequest
	if !s.decodeJSON(w, r, &req, 16<<10) {
		return
	}
	user, _ := userFrom(r.Context())

	// customer_visible travels as a pointer: an edit that omits it keeps the current value, and a
	// create that omits it gets the friendly default, visible.
	in := service.PropertyDefinitionInput{
		Key:             key,
		Label:           req.Label,
		Description:     req.Description,
		ShowOnInvoice:   req.ShowOnInvoice,
		CustomerVisible: req.CustomerVisible,
		SortOrder:       req.SortOrder,
		ActorID:         user.ID,
	}

	out, err := s.serviceStore(r).SavePropertyDefinition(r.Context(), in)
	if err != nil {
		s.writeServiceMetadataError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, propertyDefinitionJSON(out))
}

// handleDeleteServiceProperty removes a definition no service carries a value for.
func (s *Server) handleDeleteServiceProperty(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "database_not_configured",
			"no database is configured")
		return
	}
	key := r.PathValue("key")
	user, _ := userFrom(r.Context())

	if err := s.serviceStore(r).DeletePropertyDefinition(r.Context(), key, user.ID); err != nil {
		s.writeServiceMetadataError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"key": key, "deleted": true})
}

// writeServiceMetadataError maps a metadata error to a problem document.
//
// The codes are stable because the UI branches on them: "unknown_property" points at the
// properties screen, "property_in_use" explains why a definition cannot be deleted, and
// "invalid_label" keeps the form open with the text the customer typed. Everything else is the
// lifecycle mapper's business, so it is delegated rather than duplicated.
func (s *Server) writeServiceMetadataError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, service.ErrUnknownProperty):
		writeProblem(w, http.StatusBadRequest, "unknown_property", err.Error())
	case errors.Is(err, service.ErrPropertyInUse):
		writeProblem(w, http.StatusConflict, "property_in_use", err.Error())
	case errors.Is(err, service.ErrPropertyValueInvalid):
		writeProblem(w, http.StatusBadRequest, "invalid_property_value", err.Error())
	case errors.Is(err, service.ErrTooManyProperties):
		writeProblem(w, http.StatusBadRequest, "too_many_properties", err.Error())
	case errors.Is(err, service.ErrLabelInvalid):
		writeProblem(w, http.StatusBadRequest, "invalid_label", err.Error())
	case errors.Is(err, service.ErrDescriptionInvalid):
		writeProblem(w, http.StatusBadRequest, "invalid_description", err.Error())
	default:
		s.writeServiceError(w, r, err)
	}
}
