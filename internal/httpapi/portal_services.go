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
