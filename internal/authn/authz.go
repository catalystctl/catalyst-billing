package authn

import (
	"errors"
	"fmt"
	"sort"
	"sync"
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
	// PermServiceUpgradeSelf lets a customer start an upgrade or downgrade of their own
	// service. It is deliberately distinct from the staff service:upgrade: the customer
	// path is scoped to their own record in SQL and issues an invoice they must pay, while
	// the staff path can move any service.
	PermServiceUpgradeSelf Permission = "service:upgrade:self"
	PermTicketReadSelf     Permission = "ticket:read:self"
	PermTicketWriteSelf    Permission = "ticket:write:self"
	// PermOrderCreateSelf lets a customer place an order for themselves. It is separate from
	// invoice:pay:self because placing an order is not paying one: a zero-total order never
	// produces an invoice, so borrowing the pay permission made a free product require a
	// capability it never exercises.
	PermOrderCreateSelf Permission = "order:create:self"
	// PermServiceWriteSelf lets a customer label and describe their own service. It is
	// separate from service:read:self because a rename is a write that an operator may want
	// to withhold while still letting the customer see what they bought.
	PermServiceWriteSelf Permission = "service:write:self"
	// PermPaymentMethodReadSelf and PermPaymentMethodWriteSelf cover a saved payment method:
	// the stored instrument a renewal charges off-session. A customer may see and replace
	// their own; the provider token is never returned to them.
	PermPaymentMethodReadSelf  Permission = "paymentmethod:read:self"
	PermPaymentMethodWriteSelf Permission = "paymentmethod:write:self"

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

	// --- commerce configuration -------------------------------------------------
	//
	// These are split by *what an operator decides*, not by which screen they appear on,
	// for the same reason catalog:write is split from invoice:write:all: the person who
	// runs a promotion is not necessarily the person who files tax, and neither of them is
	// necessarily allowed to move money. A single "settings:write" for all of it would make
	// every one of those decisions available to whoever needs any of them.

	// PermCouponReadAll reads promotions, including expired and exhausted ones.
	PermCouponReadAll Permission = "coupon:read:all"
	// PermCouponWrite creates, edits and retires promotions. Retiring is an update (the
	// `enabled` flag), never a delete: a used coupon is referenced by the invoices it
	// discounted, and the invoice snapshot must stay explainable.
	PermCouponWrite Permission = "coupon:write"
	// PermTaxWrite edits the jurisdiction rate table. Tax rates are dated rows, so a change
	// appends a new rate effective from a date rather than rewriting a filed period.
	PermTaxWrite Permission = "tax:write"
	// PermCurrencyWrite configures which currencies are offered, their display format and
	// which one is the default. It does not create exchange rates: prices are explicit.
	PermCurrencyWrite Permission = "currency:write"
	// PermConfigWrite edits configurable product options (RAM, CPU, add-ons) and the price
	// of each choice. It changes what a *subscription* renews at, which is why it is not
	// folded into catalog:write.
	PermConfigWrite Permission = "config:write"

	// --- support ---------------------------------------------------------------
	//
	// A support agent reads every ticket and writes replies. They cannot read invoices
	// beyond what they already could, so a ticket is not a way to see a customer's money.

	PermTicketReadAll  Permission = "ticket:read:all"
	PermTicketWriteAll Permission = "ticket:write:all"

	// --- communication and credit ----------------------------------------------

	// PermNotificationWrite edits message templates and their channel toggles. A template
	// is the text a customer receives, so editing one is a customer-facing change.
	PermNotificationWrite Permission = "notification:write"
	// PermCreditWrite grants customer credit without an invoice behind it. It is separate
	// from payment:refund because a grant is not a refund of money the provider took.
	PermCreditWrite Permission = "credit:write"

	// --- service lifecycle -----------------------------------------------------

	// PermServiceUpgrade moves a service to a different product or option selection and
	// issues the prorated invoice. It is separate from service:provision because it changes
	// what the customer is billed, not just what is running.
	PermServiceUpgrade Permission = "service:upgrade"

	// --- platform integrations -------------------------------------------------

	// PermGatewayWrite configures a payment gateway: its credentials, its enabled state and
	// whether it is offered at checkout. It is separate from extension:manage because it
	// carries provider secrets.
	PermGatewayWrite Permission = "gateway:write"
	// PermServerWrite configures a provisioning server (Pterodactyl, cPanel, …) and tests
	// its connection. It carries infrastructure credentials.
	PermServerWrite Permission = "server:write"

	// --- observability ---------------------------------------------------------

	// PermLogRead reads the email, error and HTTP log viewers and cron run history. Those
	// logs contain customer addresses and request paths, so they are not public to every
	// staff member by default.
	PermLogRead Permission = "log:read"

	// --- growth ----------------------------------------------------------------

	// PermAnnouncementWrite publishes storefront and dashboard announcements.
	PermAnnouncementWrite Permission = "announcement:write"

	// --- operator breadth ------------------------------------------------------

	// PermCreditReadAll reads the customer credit ledger across accounts. It is separate from
	// credit:write because seeing what a customer is owed is an audit question while granting
	// credit is a money movement.
	PermCreditReadAll Permission = "credit:read:all"
	// PermServiceWriteAll edits a service's operator-facing metadata: its label, description
	// and custom properties. It is not service:upgrade, which changes what is billed.
	PermServiceWriteAll Permission = "service:write:all"
	// PermI18nWrite edits locales and message translations.
	PermI18nWrite Permission = "i18n:write"
	// PermImportWrite runs an importer: it writes customers, orders and invoices in bulk,
	// which is a bigger hammer than customer:write:all and deserves its own grant.
	PermImportWrite Permission = "import:write"
	// PermOAuthManage registers OAuth2 clients and their scopes. A client acts on behalf of
	// a user, so registering one is an access decision, not a settings edit.
	PermOAuthManage Permission = "oauth:manage"
	// PermUpdateManage checks for and applies panel updates. It is the one permission that can
	// replace the running binary.
	PermUpdateManage Permission = "update:manage"
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
		PermServiceHold, PermServiceUpgrade,
		PermExtensionManage, PermSettingsWrite, PermStaffManage,
		PermAuditRead, PermReportRead, PermAPIKeyManage,
		PermCatalogWrite,
		PermCouponReadAll, PermCouponWrite, PermTaxWrite, PermCurrencyWrite, PermConfigWrite,
		PermTicketReadAll, PermTicketWriteAll,
		PermNotificationWrite, PermCreditWrite,
		PermGatewayWrite, PermServerWrite,
		PermLogRead, PermAnnouncementWrite,
		PermCreditReadAll, PermServiceWriteAll, PermI18nWrite, PermImportWrite,
		PermOAuthManage, PermUpdateManage,
	},
	RoleBilling: {
		PermCustomerReadAll, PermCustomerWriteAll,
		PermInvoiceReadAll, PermInvoiceWriteAll, PermInvoiceVoid,
		PermPaymentRefund, PermPaymentReconcile,
		PermServiceReadAll, PermServiceUpgrade,
		PermReportRead,
		// A billing clerk runs the commercial side: promotions, tax, currencies and the
		// credit a customer is owed. They do not configure a gateway's credentials — that is
		// an administrator's job — and they do not read infrastructure logs.
		PermCouponReadAll, PermCouponWrite, PermTaxWrite, PermCurrencyWrite, PermConfigWrite,
		PermCreditWrite, PermNotificationWrite,
		PermTicketReadAll, PermTicketWriteAll,
		// A clerk answers "what is this customer owed", corrects the label a customer sees, and
		// runs a customer import. They do not manage OAuth clients or replace the binary.
		PermCreditReadAll, PermServiceWriteAll, PermI18nWrite, PermImportWrite,
	},
	// Support can place and clear a hold, which is exactly the "stop, do not touch this"
	// instruction a first-line agent needs while a dispute is open. Support cannot suspend,
	// refund or terminate.
	RoleSupport: {
		PermCustomerReadAll,
		PermInvoiceReadAll,
		PermServiceReadAll, PermServiceHold,
		PermTicketReadSelf, PermTicketWriteSelf,
		PermTicketReadAll, PermTicketWriteAll,
		PermLogRead,
		// Support reads a balance to answer "why is my invoice like this" and corrects the
		// label on a service so the customer's portal says what the box actually is.
		PermCreditReadAll, PermServiceWriteAll,
		// A support agent may mint a read-only key for their own tooling. The key can only carry
		// scopes the agent already holds, which is why this is safe to grant here.
		PermAPIKeyManage,
	},
	RoleInfrastructure: {
		PermServiceReadAll, PermServiceSuspend, PermServiceTerminate, PermServiceProvision,
		PermServiceUpgrade,
		PermCustomerReadAll,
		PermServerWrite,
		PermAuditRead,
		PermServiceWriteAll,
		// The infrastructure role is the one that has to take a security patch at 02:00.
		PermUpdateManage,
	},
}

// customerPermissions is what any authenticated customer may do to their own
// resources. It is a role in the same sense: the check is uniform.
var customerPermissions = []Permission{
	PermCustomerReadSelf, PermCustomerWriteSelf,
	PermInvoiceReadSelf, PermInvoicePaySelf,
	PermOrderCreateSelf,
	PermServiceReadSelf, PermServiceCancelSelf, PermServiceUpgradeSelf, PermServiceWriteSelf,
	PermTicketReadSelf, PermTicketWriteSelf,
	PermPaymentMethodReadSelf, PermPaymentMethodWriteSelf,
}

// Authorizer answers authorization questions.
type Authorizer struct {
	// table holds the permission set of an operator-defined role, keyed by role name. The four
	// system roles are compiled in (`rolePermissions`) because the code's separation of duties
	// depends on them and they must not be editable; a custom role is a row, so its permissions
	// come from here.
	//
	// It is a map rather than a callback on purpose: an Authorizer is read on every guarded
	// request, and a database round trip per request would put the permission check on the hot
	// path of a page load. The map is refreshed when a role changes, which is rare and already
	// an audited action.
	table map[Role][]Permission
	// mu guards table against a concurrent refresh while a request is reading it.
	mu sync.RWMutex
}

// NewAuthorizer constructs an Authorizer.
func NewAuthorizer() *Authorizer { return &Authorizer{} }

// SetTableRoles replaces the operator-defined role permissions.
//
// A system role name passed here is ignored: the four compiled-in roles are what the rest of the
// code assumes, and letting a row redefine "administrator" would mean a database edit could
// grant a permission the deployment never intended.
func (a *Authorizer) SetTableRoles(perms map[Role][]Permission) {
	clean := make(map[Role][]Permission, len(perms))
	for role, ps := range perms {
		if _, system := rolePermissions[role]; system {
			continue
		}
		clean[role] = ps
	}
	a.mu.Lock()
	a.table = clean
	a.mu.Unlock()
}

// Permissions returns the effective permission set for a user, which is the union
// of the customer baseline and every staff role held.
func (a *Authorizer) Permissions(u User) map[Permission]struct{} {
	out := make(map[Permission]struct{}, len(customerPermissions)+16)
	for _, p := range customerPermissions {
		out[p] = struct{}{}
	}
	a.mu.RLock()
	table := a.table
	a.mu.RUnlock()
	for _, r := range u.Roles {
		// A compiled-in role wins. A custom role resolves from the table, so an operator can
		// define "support lead" without a deploy — but cannot shadow a system role.
		if ps, system := rolePermissions[Role(r)]; system {
			for _, p := range ps {
				out[p] = struct{}{}
			}
			continue
		}
		for _, p := range table[Role(r)] {
			// Only a declared permission is granted, so a stray row cannot widen the surface
			// beyond what the binary knows how to check.
			if knownPermission(p) {
				out[p] = struct{}{}
			}
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
