package httpapi

import (
	"context"
	"errors"
	"net/http"

	"github.com/catalyst/billing/internal/audit"
	"github.com/catalyst/billing/internal/authn"
	"github.com/catalyst/billing/internal/billing"
	"github.com/catalyst/billing/internal/portal"
	"github.com/jackc/pgx/v5"
)

// Billing profiles (Gate B4).
//
// The same data, two doors: a customer edits their own, and staff edit any. They share the
// validation and the storage path (billing.ValidateProfile / billing.SaveProfile) so the two
// cannot diverge — a customer and an operator entering the same address must produce the same
// record, or the next invoice depends on who typed it.
//
// The property that matters: saving a profile does NOT change an issued invoice. The issuer
// copies these details into the document's snapshot, and the database trigger freezes that
// snapshot. Correcting an address today therefore cannot change what a tax authority already
// received; it changes the next document.

func init() {
	RegisterRoutes("80-billing-profile", registerBillingProfiles)
}

func registerBillingProfiles(r *router) {
	// A customer manages their own.
	r.get("/api/v1/portal/billing-profile", authn.PermCustomerReadSelf, r.s.handlePortalProfile)
	r.mutation(http.MethodPut, "/api/v1/portal/billing-profile",
		authn.PermCustomerWriteSelf, r.s.handlePortalProfileSave)

	// Staff manage any.
	r.staffGet("/api/v1/staff/customers/{id}/billing-profile",
		authn.PermCustomerReadAll, r.s.handleStaffProfile)
	r.staffMutation(http.MethodPut, "/api/v1/staff/customers/{id}/billing-profile",
		authn.PermCustomerWriteAll, r.s.handleStaffProfileSave)
}

// profileBody is the wire shape of a profile.
//
// It is a separate type from billing.Profile so the JSON field names are a deliberate choice
// rather than a reflection of a struct that also feeds the invoice snapshot.
type profileBody struct {
	LegalName    string `json:"legal_name"`
	AddressLine1 string `json:"address_line1"`
	AddressLine2 string `json:"address_line2"`
	City         string `json:"city"`
	PostalCode   string `json:"postal_code"`
	Region       string `json:"region"`
	CountryCode  string `json:"country_code"`
	TaxID        string `json:"tax_id"`
}

func (b profileBody) profile(customerID string) billing.Profile {
	return billing.Profile{
		CustomerID:   customerID,
		LegalName:    b.LegalName,
		AddressLine1: b.AddressLine1,
		AddressLine2: b.AddressLine2,
		City:         b.City,
		PostalCode:   b.PostalCode,
		Region:       b.Region,
		CountryCode:  b.CountryCode,
		TaxID:        b.TaxID,
	}
}

func profileResponse(p billing.Profile) map[string]any {
	return map[string]any{
		"customer_id":   p.CustomerID,
		"legal_name":    p.LegalName,
		"address_line1": p.AddressLine1,
		"address_line2": p.AddressLine2,
		"city":          p.City,
		"postal_code":   p.PostalCode,
		"region":        p.Region,
		"country_code":  p.CountryCode,
		"tax_id":        p.TaxID,
	}
}

// handlePortalProfile returns the caller's own billing details.
func (s *Server) handlePortalProfile(w http.ResponseWriter, r *http.Request) {
	if s.portal == nil {
		writeProblem(w, http.StatusServiceUnavailable, "portal_unavailable", "the portal is not configured")
		return
	}
	user, _ := userFrom(r.Context())
	customerID, err := s.portal.CustomerIDFor(r.Context(), user.ID)
	if err != nil {
		s.writePortalError(w, r, err)
		return
	}

	profile, err := billing.LoadProfile(r.Context(), s.pool, customerID)
	if err != nil {
		s.staffError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, profileResponse(profile))
}

// handlePortalProfileSave stores the caller's own billing details.
func (s *Server) handlePortalProfileSave(w http.ResponseWriter, r *http.Request) {
	if s.portal == nil {
		writeProblem(w, http.StatusServiceUnavailable, "portal_unavailable", "the portal is not configured")
		return
	}
	user, _ := userFrom(r.Context())
	customerID, err := s.portal.CustomerIDFor(r.Context(), user.ID)
	if err != nil {
		s.writePortalError(w, r, err)
		return
	}
	s.saveProfile(w, r, customerID)
}

// handleStaffProfile returns any customer's billing details.
func (s *Server) handleStaffProfile(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !portal.ValidID(id) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such record")
		return
	}
	profile, err := billing.LoadProfile(r.Context(), s.pool, id)
	if err != nil {
		if errors.Is(err, billing.ErrInvalidProfile) {
			writeProblem(w, http.StatusNotFound, "not_found", "no such record")
			return
		}
		s.staffError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, profileResponse(profile))
}

// handleStaffProfileSave stores any customer's billing details.
func (s *Server) handleStaffProfileSave(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !portal.ValidID(id) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such record")
		return
	}
	s.saveProfile(w, r, id)
}

// saveProfile validates, stores and audits, in one transaction.
func (s *Server) saveProfile(w http.ResponseWriter, r *http.Request, customerID string) {
	var body profileBody
	if !s.decodeJSON(w, r, &body, 16<<10) {
		return
	}
	profile := body.profile(customerID)

	var saved billing.Profile
	err := s.pool.InTx(r.Context(), func(ctx context.Context, tx pgx.Tx) error {
		// Load the previous version so the audit row can show what changed. An audit entry that
		// only says "updated" is not a diff.
		previous, err := billing.LoadProfile(ctx, tx, customerID)
		if err != nil {
			return err
		}
		if err := billing.SaveProfile(ctx, tx, profile); err != nil {
			return err
		}
		saved = profile

		return audit.Record(ctx, tx, s.auditEvent(r, "customer.billing_profile_update",
			"customer", customerID, audit.OutcomeSuccess).WithChanges(map[string]any{
			"before": map[string]any{
				"legal_name":   previous.LegalName,
				"country_code": previous.CountryCode,
				"tax_id":       previous.TaxID,
				"city":         previous.City,
			},
			"after": map[string]any{
				"legal_name":   profile.LegalName,
				"country_code": profile.CountryCode,
				"tax_id":       profile.TaxID,
				"city":         profile.City,
			},
		}))
	})
	switch {
	case err == nil:
	case errors.Is(err, billing.ErrInvalidProfile):
		// The message names the field the caller got wrong, which is the only way a form can
		// show an error next to the right input.
		writeProblem(w, http.StatusBadRequest, "invalid_profile", err.Error())
		return
	default:
		s.staffError(w, r, err)
		return
	}

	writeJSON(w, http.StatusOK, profileResponse(saved))
}
