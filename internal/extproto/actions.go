package extproto

// The three calls a service needs and the protocol did not have (SVC-16, SVC-14, UPG-12).
//
// They live in their own file rather than in spec.go because they are one work package's
// surface and because spec.go is the shared, heavily-read half of this protocol: adding an
// action there would make every future protocol diff a conflict.
//
// Each of the three is a different kind of call, and the difference is the point:
//
//	provisioner.upgrade   mutating, idempotent by operation key. The resource must end up at
//	                      the requested size. A replay returns the state it already reached,
//	                      which is what makes a retried queue row safe.
//	provisioner.fields    a pure read. It is how an adaptor says which checkout inputs it
//	                      needs ("a panel user id", "a domain name"), so the purchase form is
//	                      the adapter's knowledge rather than a list hardcoded in the core.
//	gateway.cancel_subscription
//	                      mutating, idempotent by operation key. Ends a provider-side
//	                      subscription or billing agreement. Cancelling one that is already
//	                      cancelled is success with Canceled=false, because the requested end
//	                      state is what the caller asked for.

// Method names. As everywhere in this protocol a method name doubles as its capability
// name, so an extension promises exactly the calls it lists in its handshake.
const (
	MethodProvisionerUpgrade = "provisioner.upgrade"
	// MethodProvisionerFields answers "which inputs do you need at purchase time?".
	MethodProvisionerFields = "provisioner.fields"
	// MethodGatewayCancelSubscription ends a subscription the *provider* holds the record
	// of. It is a gateway call, not a provisioner call: a hosting panel has no idea what a
	// card subscription is.
	MethodGatewayCancelSubscription = "gateway.cancel_subscription"
)

// Capability names, for extensions that want to spell them out.
const (
	CapProvisionerUpgrade        = MethodProvisionerUpgrade
	CapProvisionerFields         = MethodProvisionerFields
	CapGatewayCancelSubscription = MethodGatewayCancelSubscription
)

// ProvisionUpgradeParams asks a provisioner to move a resource to another plan.
//
// PlanID is the target, never the current one: the core knows what the service was sold as
// and what it is being moved to, and an adapter that had to diff the two would need the
// catalogue. Region is carried because the target price version may name one, and a move
// within one node is not the same request as a move to another.
//
// Metadata carries the same panel bookkeeping a create carries — the panel user id, the
// config options as `env.` prefixed entries — because a resize that rebuilt the resource
// without it would silently drop the customer's chosen options (UPG-10).
type ProvisionUpgradeParams struct {
	MutatingMeta
	ExternalID string            `json:"external_id"`
	PlanID     string            `json:"plan_id"`
	Region     string            `json:"region,omitempty"`
	Quantity   int32             `json:"quantity"`
	Metadata   map[string]string `json:"metadata,omitempty"`
}

// ProvisionUpgradeResult reports the state the resource is in after the call.
type ProvisionUpgradeResult struct {
	ExternalID string         `json:"external_id"`
	Status     ResourceStatus `json:"status"`
	PlanID     string         `json:"plan_id"`
	Region     string         `json:"region,omitempty"`
	// Changed is false when the resource was already at the requested plan, which is what a
	// replay of a completed upgrade reports.
	Changed bool `json:"changed"`
}

// FieldType is the narrow set of input shapes a checkout form can render without the core
// learning a provider's vocabulary.
type FieldType string

// The input shapes. Anything else is refused by Validate, because a form that cannot render
// a field is a purchase that cannot be completed.
const (
	FieldText     FieldType = "text"
	FieldNumber   FieldType = "number"
	FieldDropdown FieldType = "dropdown"
	FieldCheckbox FieldType = "checkbox"
	FieldPassword FieldType = "password"
)

// ProvisionField is one input the adapter needs before it can provision.
//
// Key is also the metadata key the value is sent back under in ProvisionCreateParams and
// ProvisionUpgradeParams. That is deliberate and is the whole contract: an adapter declares
// "I need a value called `domain`", the purchase stores it, and the create call receives
// metadata["domain"]. There is no second mapping to drift.
type ProvisionField struct {
	Key   string    `json:"key"`
	Label string    `json:"label"`
	Type  FieldType `json:"type"`
	// Choices closes the set for a dropdown. Ignored for every other type.
	Choices []string `json:"choices,omitempty"`
	// Required is enforced by the checkout, not advisory: a create that would be refused
	// for a missing domain must not accept the purchase and fail afterwards.
	Required bool `json:"required"`
	// Secret marks a value the panel stores write-only and never renders back.
	Secret bool   `json:"secret,omitempty"`
	Hint   string `json:"hint,omitempty"`
}

// Valid reports whether the field can be rendered and its value stored.
func (f ProvisionField) Valid() bool {
	if f.Key == "" || f.Label == "" {
		return false
	}
	switch f.Type {
	case FieldText, FieldNumber, FieldDropdown, FieldCheckbox, FieldPassword:
	case "":
		return false
	default:
		return false
	}
	if f.Type == FieldDropdown && len(f.Choices) == 0 {
		return false
	}
	return true
}

// ProvisionFieldsParams asks a provisioner which inputs it needs.
type ProvisionFieldsParams struct {
	CallMeta
}

// ProvisionFieldsResult is the adapter's declared inputs.
//
// A field that is not Valid is dropped by the core rather than rendered: a broken declaration
// must not break the purchase form for every product on the installation.
type ProvisionFieldsResult struct {
	Fields []ProvisionField `json:"fields"`
}

// GatewayCancelSubscriptionParams ends a provider-side subscription.
//
// SubscriptionID is the provider's identifier, which the panel stores as
// services.external_subscription_id. It is required: a call with no id would have to guess
// which record to destroy, and cancelling the wrong subscription is not recoverable by
// retrying.
type GatewayCancelSubscriptionParams struct {
	MutatingMeta
	SubscriptionID string `json:"subscription_id"`
	Reason         string `json:"reason,omitempty"`
}

// GatewayCancelSubscriptionResult reports what the provider says now.
type GatewayCancelSubscriptionResult struct {
	SubscriptionID string `json:"subscription_id"`
	// Status is the provider's own state name after the call ("canceled", "active", …),
	// passed through rather than translated: a core that renamed it would hide the one fact
	// the operator needs when the provider refuses.
	Status string `json:"status,omitempty"`
	// Canceled is true when this call ended it, false when it was already ended.
	Canceled bool `json:"canceled"`
	// AlreadyGone is true when the provider does not know the id at all. It is distinct
	// from Canceled=false because the panel's own record then refers to nothing, which is a
	// discrepancy an operator should see rather than a success.
	AlreadyGone bool `json:"already_gone,omitempty"`
}
