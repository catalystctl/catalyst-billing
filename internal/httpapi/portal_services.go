// What a customer may change about their own service, and what each change costs (UPG-07…UPG-11).
//
// Three routes already exist for the three mutations a customer can make to a service — an
// upgrade, a config-option change and a metadata edit — and until this file they had no screen
// and one missing read. The missing read is the point of this file:
//
//	GET /api/v1/portal/services/{id}/config-options
//
// `GET .../config` answers "what is selected on this service", which is the right question for a
// support agent and the wrong one for a form: it names the current value and not the price of any
// alternative, so a customer changing "2 GB" to "4 GB" would learn what it costs only after
// committing. This route answers the form's question instead — every option the customer may
// change, every choice it offers, and the amount each choice carries on *this service's* price
// version — so the price is on screen before the click.
//
// Four rules shape it, and none of them is new arithmetic:
//
//  1. **The prices are the rows the mutation reads.** The amount of a choice comes from
//     `config_option_prices` for the subscription's price version, which is exactly the row
//     `service.Store.ChangeServiceConfig` charges from and `service.Store.ResolveSelection` prices
//     a checkout with. There is no second calculation here, and no exchange rate: a price in
//     another currency is not a price this customer can move to.
//  2. **A hidden option is not offered.** `hidden` exists so an operator can attach an option to
//     a plan without selling it; the resolver skips hidden required-checks for the same reason.
//  3. **A non-upgradable option is shown but not offered as a control.** `upgradable: false` is a
//     deliberate operator decision, and a customer reading "you are on 2 GB, and that is fixed"
//     is a better answer than an option that silently disappears from the page.
//  4. **The service is resolved through the caller's customer account in SQL**, so another
//     customer's identifier is a 404 — the same answer as an id that does not exist. The pricing
//     query never sees a foreign id.
package httpapi

import (
	"net/http"

	"github.com/catalyst/billing/internal/authn"
	"github.com/catalyst/billing/internal/portal"
	"github.com/catalyst/billing/internal/service"
)

func init() {
	RegisterRoutes("64-portal-service-options", registerPortalServiceOptions)
}

func registerPortalServiceOptions(r *router) {
	// A customer reading what they may change on their own service.
	r.get("/api/v1/portal/services/{id}/config-options", authn.PermServiceReadSelf,
		r.s.handlePortalConfigOptions)

	// A plan change, priced as the plan *and* the options the customer chose on it (UPG-02).
	//
	// It is a separate route from `POST .../upgrade` rather than a field on it because the two
	// answer different questions. The upgrade route says "move my base price"; this one says "move
	// my base price and my options, and tell me what the whole change costs". A body field would
	// have made the older clients' omission mean "drop my options", which prices as a downgrade.
	r.get("/api/v1/portal/services/{id}/plan-options", authn.PermServiceUpgradeSelf,
		r.s.handlePortalPlanOptions)
	r.mutation(http.MethodPost, "/api/v1/portal/services/{id}/change-plan",
		authn.PermServiceUpgradeSelf, r.s.handlePortalChangePlan)
}

// portalConfigChoiceJSON is one choice of an option with its amount on this service's plan.
//
// The amount is integer minor units with the response's `currency` alongside, like every other
// money field this API emits: a float here would round a price, and a price that is off by a
// hundredth is a support ticket.
type portalConfigChoiceJSON struct {
	Choice      string `json:"choice"`
	AmountMinor int64  `json:"amount_minor"`
	SetupMinor  int64  `json:"setup_minor"`
}

// portalConfigOptionJSON is one option a customer may change, with its current value.
type portalConfigOptionJSON struct {
	ConfigOptionID string                   `json:"config_option_id"`
	Name           string                   `json:"name"`
	Description    string                   `json:"description"`
	Type           string                   `json:"type"`
	EnvVariable    string                   `json:"env_variable"`
	Required       bool                     `json:"required"`
	Upgradable     bool                     `json:"upgradable"`
	Value          string                   `json:"value"`
	Choices        []portalConfigChoiceJSON `json:"choices"`
}

// handlePortalConfigOptions lists the options on the caller's own service, priced on the plan the
// service renews at, and says whether the service can be re-priced at all.
//
// The service's own selection is joined in as `value`, so the form opens on what the customer has
// rather than on the first choice of the list — a default that is not the current value is a
// change the customer did not ask for, one click away from being submitted.
//
// `can_change` and `change_blocked_reason` travel with this response rather than on the service
// object because they are the same fact as the options: a page asks "what may I change?" and gets
// one answer. They come from `portal.Manager`, which computes them from the rules the upgrade and
// config handlers enforce, so a form is only offered where the server would accept it.
func (s *Server) handlePortalConfigOptions(w http.ResponseWriter, r *http.Request) {
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

	// One ownership-scoped read, and it is the only way this handler learns the product, the
	// price version and the current selection: three separate queries would be three chances to
	// forget the scope.
	m, err := s.serviceStore(nil).ServiceMetadataOwnedBy(r.Context(), id, user.ID)
	if err != nil {
		s.writeServiceMetadataError(w, r, err)
		return
	}

	out := map[string]any{
		"service_id":       m.ServiceID,
		"price_version_id": m.PriceVersionID,
		"currency":         m.Currency,
		"options":          []portalConfigOptionJSON{},
		"types":            service.ConfigOptionTypes,
		// A service with no billing schedule has nothing to re-price, and one whose
		// subscription has ended has no period left. Both are stated here so the page can
		// say so instead of offering a form that would come back 409.
		"can_change":            false,
		"change_blocked_reason": "",
	}
	// The manager is a thin wrapper over the pool, so building one when the server has none
	// attached is the same read rather than a second rule: `serviceStore(nil)` above is
	// constructed the same way.
	mgr := s.portalManager()
	if mgr == nil {
		mgr = portal.New(s.pool)
	}
	if svc, err := mgr.Service(r.Context(), user.ID, id); err == nil {
		out["can_change"] = svc.CanChange
		out["change_blocked_reason"] = svc.ChangeBlockedReason
	}

	options, err := s.serviceStore(nil).ConfigOptions(r.Context(), m.ProductID)
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}

	selected := make(map[string]string, len(m.Config))
	for _, c := range m.Config {
		selected[c.ConfigOptionID] = c.Value
	}

	listed := make([]portalConfigOptionJSON, 0, len(options))
	for _, o := range options {
		if o.Hidden {
			// An operator attached this option to the plan without selling it. Offering it
			// would be offering a choice the renewal engine does not price.
			continue
		}
		choices := make([]portalConfigChoiceJSON, 0, len(o.Prices))
		for _, p := range o.Prices {
			if p.PriceVersionID != m.PriceVersionID {
				// Another version's price is not this service's price. An empty
				// price_version_id (a service with no schedule) matches nothing, which is
				// the honest answer: there is no plan to price against.
				continue
			}
			choices = append(choices, portalConfigChoiceJSON{
				Choice:      p.Choice,
				AmountMinor: p.AmountMinor,
				SetupMinor:  p.SetupMinor,
			})
		}
		listed = append(listed, portalConfigOptionJSON{
			ConfigOptionID: o.ID,
			Name:           o.Name,
			Description:    o.Description,
			Type:           o.Type,
			EnvVariable:    o.EnvVariable,
			Required:       o.Required,
			Upgradable:     o.Upgradable,
			Value:          selected[o.ID],
			Choices:        choices,
		})
	}
	out["options"] = listed

	writeJSON(w, http.StatusOK, out)
}

// ------------------------------------------------- plan change with options --

// planConfigChoice is one option value the customer chose on the target plan.
type planConfigChoice struct {
	ConfigOptionID string `json:"config_option_id"`
	Value          string `json:"value"`
}

// planChangeRequest is the body of a plan change.
//
// `config_options` is a pointer because absent and empty are different statements: absent means
// "leave my options alone" (the old upgrade route's behaviour), and `[]` means "the plan I am
// moving to has no options, take mine away". The second one is a real change — dropping a paid
// option returns money — so it cannot be spelled the same way as "I did not send the field".
type planChangeRequest struct {
	PriceVersionID string              `json:"price_version_id"`
	ConfigOptions  *[]planConfigChoice `json:"config_options"`
	Reason         string              `json:"reason"`
}

// handlePortalPlanOptions lists the options of a plan the customer may move to, priced on that
// plan's own price version.
//
// This is what makes "choose a product and its options" one form instead of two: the amounts here
// are the rows `ChangePlan` charges from, so the price on screen and the price on the invoice are
// the same rows read by the same resolver.
//
// A price version that is not a reachable target answers 404, exactly like a service that is not
// the caller's: the list of targets is the server's answer, and asking about anything else must
// not disclose whether it exists.
func (s *Server) handlePortalPlanOptions(w http.ResponseWriter, r *http.Request) {
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
	priceVersionID := r.URL.Query().Get("price_version_id")
	if !portal.ValidID(priceVersionID) {
		writeProblem(w, http.StatusBadRequest, "invalid_price_version",
			"price_version_id must identify a plan")
		return
	}
	user, _ := userFrom(r.Context())

	if err := s.serviceStore(nil).ServiceOwnedBy(r.Context(), id, user.ID); err != nil {
		s.writeServiceError(w, r, err)
		return
	}

	targets, err := s.serviceStore(nil).UpgradeOptions(r.Context(), id)
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	var (
		productID string
		found     bool
	)
	for _, target := range targets {
		for _, price := range target.Prices {
			if price.PriceVersionID == priceVersionID {
				productID, found = target.ProductID, true
			}
		}
	}
	if !found {
		writeProblem(w, http.StatusNotFound, "not_found", "no such plan for this service")
		return
	}

	options, err := s.serviceStore(nil).ConfigOptions(r.Context(), productID)
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}

	// The customer's current values, when the option is one they already have (a version change
	// within the same product). A different product's options start from the first choice.
	current := map[string]string{}
	if configs, err := s.serviceStore(nil).ServiceConfigs(r.Context(), id); err == nil {
		for _, c := range configs {
			current[c.ConfigOptionID] = c.Value
		}
	}

	listed := make([]portalConfigOptionJSON, 0, len(options))
	for _, o := range options {
		if o.Hidden {
			continue
		}
		choices := make([]portalConfigChoiceJSON, 0, len(o.Prices))
		for _, p := range o.Prices {
			if p.PriceVersionID != priceVersionID {
				continue
			}
			choices = append(choices, portalConfigChoiceJSON{
				Choice:      p.Choice,
				AmountMinor: p.AmountMinor,
				SetupMinor:  p.SetupMinor,
			})
		}
		listed = append(listed, portalConfigOptionJSON{
			ConfigOptionID: o.ID,
			Name:           o.Name,
			Description:    o.Description,
			Type:           o.Type,
			EnvVariable:    o.EnvVariable,
			Required:       o.Required,
			Upgradable:     o.Upgradable,
			Value:          current[o.ID],
			Choices:        choices,
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"service_id":       id,
		"product_id":       productID,
		"price_version_id": priceVersionID,
		"options":          listed,
		"types":            service.ConfigOptionTypes,
	})
}

// handlePortalChangePlan moves a service to another plan and changes its options in the same
// priced change (UPG-02).
//
// The prorated amount is the whole difference for the rest of the period — base price and option
// contributions — computed by `service.Store.ChangePlan` with the same exact rational arithmetic
// an upgrade has always used, rounded once. A positive difference issues an invoice and takes
// effect when it is paid; a negative one is recorded as a credit and posted by the credits
// package. The response is the same shape as the upgrade route's, so a client renders one result
// dialog for both.
func (s *Server) handlePortalChangePlan(w http.ResponseWriter, r *http.Request) {
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
	var req planChangeRequest
	if !s.decodeJSON(w, r, &req, 32<<10) {
		return
	}
	if !portal.ValidID(req.PriceVersionID) {
		writeProblem(w, http.StatusBadRequest, "invalid_price_version",
			"price_version_id must identify a plan")
		return
	}

	var selection map[string]string
	if req.ConfigOptions != nil {
		selection = make(map[string]string, len(*req.ConfigOptions))
		for _, choice := range *req.ConfigOptions {
			if !portal.ValidID(choice.ConfigOptionID) {
				writeProblem(w, http.StatusBadRequest, "invalid_config_option",
					"config_option_id must identify an option")
				return
			}
			selection[choice.ConfigOptionID] = choice.Value
		}
	}

	user, _ := userFrom(r.Context())
	res, err := s.serviceStore(r).ChangePlan(r.Context(), service.PlanChangeRequest{
		ServiceID:      id,
		PriceVersionID: req.PriceVersionID,
		Selection:      selection,
		UserID:         user.ID,
		Reason:         req.Reason,
	})
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}

	s.publishServiceEvent(r, id, "service.upgrade_requested", map[string]any{
		"service_id": id, "upgrade_id": res.UpgradeID, "direction": res.Direction,
		"prorated_minor": res.ProratedMinor,
	})
	writeJSON(w, http.StatusCreated, upgradeResultJSON(res))
}
