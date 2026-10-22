package authn

import (
	"errors"
	"fmt"
	"sort"
)

// Permission is a named capability. The set is closed and explicit: a permission
// that is not declared here cannot be granted, so a typo fails closed.
type Permission string

const (
	// Customer-facing permissions.
	PermCustomerReadSelf  Permission = "customer:read:self"
	PermCustomerWriteSelf Permission = "customer:write:self"
	PermInvoiceReadSelf   Permission = "invoice:read:self"
	PermInvoicePaySelf    Permission = "invoice:pay:self"
	PermServiceReadSelf   Permission = "service:read:self"
	PermServiceCancelSelf Permission = "service:cancel:self"
	PermTicketReadSelf    Permission = "ticket:read:self"
	PermTicketWriteSelf   Permission = "ticket:write:self"

	// Staff permissions, split by the role that needs them.
	PermCustomerReadAll  Permission = "customer:read:all"
	PermCustomerWriteAll Permission = "customer:write:all"
	PermInvoiceReadAll   Permission = "invoice:read:all"
	PermInvoiceWriteAll  Permission = "invoice:write:all"
	// PermInvoiceVoid is separate from invoice:write:all because voiding is irreversible in
	// the legal sense: the document was issued with a number, and the number is burned.
	PermInvoiceVoid      Permission = "invoice:void"
	PermPaymentRefund    Permission = "payment:refund"
	PermPaymentReconcile Permission = "payment:reconcile"
	PermServiceReadAll   Permission = "service:read:all"
	PermServiceSuspend   Permission = "service:suspend"
	// PermServiceHold gates placing and clearing a manual hold. It is separate from
	// service:suspend because a hold is the *opposite* instruction: suspend asks automation
	// to act, hold asks it to stop. Support staff need the second and should not have the
	// first.
	PermServiceHold      Permission = "service:hold"
	PermServiceTerminate Permission = "service:terminate"
	PermServiceProvision Permission = "service:provision"
	PermExtensionManage  Permission = "extension:manage"
	PermSettingsWrite    Permission = "settings:write"
	PermStaffManage      Permission = "staff:manage"
	PermAuditRead        Permission = "audit:read"
	PermReportRead       Permission = "report:read"
	PermAPIKeyManage     Permission = "apikey:manage"
	// PermCatalogWrite gates the catalog: categories, products, their metadata, logos and
	// new price versions. It is separate from invoice:write:all because the two are different
	// jobs: a billing clerk issues documents, a catalog manager decides what is for sale.
	// Reading the catalog for invoicing is still invoice:read:all — pricing an invoice must
	// not require the right to change a price.
	PermCatalogWrite Permission = "catalog:write"
)

// Role is a named bundle of permissions.
type Role string

const (
	RoleAdministrator  Role = "administrator"
	RoleBilling        Role = "billing"
	RoleSupport        Role = "support"
	RoleInfrastructure Role = "infrastructure"
)

// rolePermissions is the authorization map. It is data, reviewed in code review,
// rather than rows an operator can edit: widening a role is a decision that should
// leave a trace in a diff.
//
// Separation of duties is deliberate. Billing staff can refund but cannot
// terminate infrastructure; infrastructure staff can terminate but cannot refund.
// An administrator holds both, which is why the plan calls for dual approval on
// high-value actions rather than assuming one role should not exist.
var rolePermissions = map[Role][]Permission{
	RoleAdministrator: {
		PermCustomerReadAll, PermCustomerWriteAll,
		PermInvoiceReadAll, PermInvoiceWriteAll, PermInvoiceVoid,
		PermPaymentRefund, PermPaymentReconcile,
		PermServiceReadAll, PermServiceSuspend, PermServiceTerminate, PermServiceProvision,
		PermServiceHold,
		PermExtensionManage, PermSettingsWrite, PermStaffManage,
		PermAuditRead, PermReportRead, PermAPIKeyManage,
		PermCatalogWrite,
	},
	RoleBilling: {
		PermCustomerReadAll, PermCustomerWriteAll,
		PermInvoiceReadAll, PermInvoiceWriteAll, PermInvoiceVoid,
		PermPaymentRefund, PermPaymentReconcile,
		PermServiceReadAll,
		PermReportRead,
	},
	// Support can place and clear a hold, which is exactly the "stop, do not touch this"
	// instruction a first-line agent needs while a dispute is open. Support cannot suspend,
	// refund or terminate.
	RoleSupport: {
		PermCustomerReadAll,
		PermInvoiceReadAll,
		PermServiceReadAll, PermServiceHold,
		PermTicketReadSelf, PermTicketWriteSelf,
		// A support agent may mint a read-only key for their own tooling. The key can only carry
		// scopes the agent already holds, which is why this is safe to grant here.
		PermAPIKeyManage,
	},
	RoleInfrastructure: {
		PermServiceReadAll, PermServiceSuspend, PermServiceTerminate, PermServiceProvision,
		PermCustomerReadAll,
		PermAuditRead,
	},
}

// customerPermissions is what any authenticated customer may do to their own
// resources. It is a role in the same sense: the check is uniform.
var customerPermissions = []Permission{
	PermCustomerReadSelf, PermCustomerWriteSelf,
	PermInvoiceReadSelf, PermInvoicePaySelf,
	PermServiceReadSelf, PermServiceCancelSelf,
	PermTicketReadSelf, PermTicketWriteSelf,
}

// Authorizer answers authorization questions.
type Authorizer struct{}

// NewAuthorizer constructs an Authorizer.
func NewAuthorizer() *Authorizer { return &Authorizer{} }

// Permissions returns the effective permission set for a user, which is the union
// of the customer baseline and every staff role held.
func (a *Authorizer) Permissions(u User) map[Permission]struct{} {
	out := make(map[Permission]struct{}, len(customerPermissions)+16)
	for _, p := range customerPermissions {
		out[p] = struct{}{}
	}
	for _, r := range u.Roles {
		for _, p := range rolePermissions[Role(r)] {
			out[p] = struct{}{}
		}
	}
	return out
}

// Can reports whether the user holds a permission.
//
// Deny-by-default: an unknown permission, an unknown role, and a disabled user all
// result in false rather than an error or an allowance.
func (a *Authorizer) Can(u User, p Permission) bool {
	if u.ID == "" || u.Status != "active" {
		return false
	}
	if !knownPermission(p) {
		return false
	}
	_, ok := a.Permissions(u)[p]
	return ok
}

// CanAny reports whether the user holds at least one of the permissions.
func (a *Authorizer) CanAny(u User, ps ...Permission) bool {
	for _, p := range ps {
		if a.Can(u, p) {
			return true
		}
	}
	return false
}

// CanAll reports whether the user holds every permission.
func (a *Authorizer) CanAll(u User, ps ...Permission) bool {
	for _, p := range ps {
		if !a.Can(u, p) {
			return false
		}
	}
	return true
}

// Require returns ErrForbidden unless the user holds the permission.
func (a *Authorizer) Require(u User, p Permission) error {
	if !a.Can(u, p) {
		return fmt.Errorf("%w: %s", ErrForbidden, p)
	}
	return nil
}

// ErrForbidden is returned when an authorization check fails.
var ErrForbidden = errors.New("authn: forbidden")

// knownPermission reports whether p is in the declared set.
func knownPermission(p Permission) bool {
	for _, list := range rolePermissions {
		for _, candidate := range list {
			if candidate == p {
				return true
			}
		}
	}
	for _, candidate := range customerPermissions {
		if candidate == p {
			return true
		}
	}
	return false
}

// AllPermissions returns every declared permission, sorted. Used by the settings
// UI and by tests that assert the map has no unknown entries.
func AllPermissions() []Permission {
	set := make(map[Permission]struct{})
	for _, list := range rolePermissions {
		for _, p := range list {
			set[p] = struct{}{}
		}
	}
	for _, p := range customerPermissions {
		set[p] = struct{}{}
	}
	out := make([]Permission, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// AllRoles returns every declared staff role.
func AllRoles() []Role {
	out := make([]Role, 0, len(rolePermissions))
	for r := range rolePermissions {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// IsStaffRole reports whether r is a declared staff role.
func IsStaffRole(r string) bool {
	_, ok := rolePermissions[Role(r)]
	return ok
}
