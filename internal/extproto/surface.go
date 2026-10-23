package extproto

// The rest of the hook surface (GRW-05).
//
// Paymenter extensions are in-process PHP classes: they can register an admin resource (a CRUD
// screen over their own table), a page, a checkout field, a navigation entry, a permission and
// an event listener. Catalyst's extensions are separate processes that speak JSON-RPC, which is
// the deliberate security choice in hooks.go — so parity here means *declaring* those surfaces
// as data, not loading the extension's code into the panel.
//
// This file adds the three declarations hooks.go did not have: admin resources, pages and
// checkout configuration. They are declarations only. An admin resource says "I have a record
// type with these columns, and this is what it is called and who may see it" — it does not
// carry SQL, a template, or a URL to fetch. A page says "list me under this path, for callers
// holding this permission". A checkout config block says "ask the customer these fields when
// they buy this product, and send me the answers".
//
// Why the fields are so narrow, and what is deliberately absent:
//
//   - No markup, no URL, no inline script, no style. The panel renders a table from the column
//     descriptions and a form from the field descriptions, with its own components. This is the
//     same rule as hooks.go and it is the reason a hook cannot become a phishing page inside the
//     operator's session.
//   - No SQL and no table name. An admin resource names a *record type*; whatever serves it is
//     the extension's own method, called through the ordinary hook call path with the declared
//     permission checked. A resource that shipped a query would be a way to read any table.
//   - No client-side behaviour. A column has a value type, not a renderer.
//
// The declarations are validated with the same rigour as hooks.go: bounded counts, bounded and
// character-restricted ids, in-panel paths only, and every permission must be one the core
// itself declares. An invalid declaration refuses the whole set, because a half-installed hook
// set is worse than none — the operator cannot tell which half is live.

import (
	"fmt"
	"regexp"
	"strings"
)

const (
	// MaxAdminResources, MaxPages and MaxCheckoutFields bound one extension's declarations.
	MaxAdminResources   = 8
	MaxPages            = 8
	MaxCheckoutFields   = 16
	maxResourceColumn   = 24
	maxCheckoutChoices  = 16
	maxColumnTypeBytes  = 40
	maxCheckoutValueLen = 200
)

// Column types an admin resource may declare. The set is closed because the panel renders a
// cell by type: a type it does not know is a cell it cannot draw, and a free-form string would
// let a declaration pick a renderer that does not exist.
const (
	ColumnText     = "text"
	ColumnNumber   = "number"
	ColumnMoney    = "money"
	ColumnDate     = "date"
	ColumnDatetime = "datetime"
	ColumnBool     = "bool"
	ColumnStatus   = "status"
)

// checkoutKeyPattern allows the dotted form ("server.hostname") a checkout answer may use, which
// the stricter hook id pattern does not.
var checkoutKeyPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

var columnTypes = map[string]struct{}{
	ColumnText: {}, ColumnNumber: {}, ColumnMoney: {}, ColumnDate: {},
	ColumnDatetime: {}, ColumnBool: {}, ColumnStatus: {},
}

// AdminResource is one record type an extension contributes to the operator surface.
//
// It is a *description* of a screen, not the screen: the panel decides how a table looks, the
// extension decides what a record is. `Method` is the extension method the panel calls to read
// the records, and it must be a capability advertised in the handshake — the same rule
// ServiceAction.Method follows, and for the same reason: the panel must never ask an extension
// to run something it never claimed to have.
type AdminResource struct {
	// ID is stable within the extension.
	ID string `json:"id"`
	// Label is the singular name, e.g. "Server".
	Label string `json:"label"`
	// Plural is the collection name, e.g. "Servers". It is what a menu entry would say.
	Plural string `json:"plural"`
	// Path is the in-panel path the resource is served under.
	Path string `json:"path"`
	// Icon is a name from the panel's own icon set.
	Icon string `json:"icon"`
	// Permission is the capability required to see the resource.
	Permission string `json:"permission"`
	// Method is the extension method that answers a list call for this resource.
	Method string `json:"method"`
	// Columns are the fields a record has, in display order.
	Columns []AdminColumn `json:"columns"`
}

// AdminColumn is one field of an admin resource's record.
type AdminColumn struct {
	// Key is the field name in the record the extension returns.
	Key string `json:"key"`
	// Label is the column header.
	Label string `json:"label"`
	// Type is one of the Column* constants. It decides how a cell is rendered and sorted.
	Type string `json:"type"`
	// Primary marks the field a table links on. At most one per resource.
	Primary bool `json:"primary,omitempty"`
}

// Page is one operator page an extension contributes.
//
// A page is a place in the panel with a title and a permission, and nothing else: what it shows
// comes from the extension's own methods, called through the hook path. A page that carried
// content would be an extension writing HTML into the panel, which the CSP would refuse and the
// security model would forbid.
type Page struct {
	// ID is stable within the extension.
	ID string `json:"id"`
	// Label is the page title.
	Label string `json:"label"`
	// Path is the in-panel path the page is served under.
	Path string `json:"path"`
	// Icon is a name from the panel's own icon set.
	Icon string `json:"icon"`
	// Permission is the capability required to see the page.
	Permission string `json:"permission"`
	// Method is the extension method that answers "what does this page show".
	Method string `json:"method"`
}

// CheckoutField is one input an extension asks for when its product is bought.
//
// It is the checkout-configuration half of the row. The answers travel to the extension as part
// of the order, so an extension that provisions something with a hostname can ask for the
// hostname at checkout instead of guessing one. The field is a declaration — a label, a type and
// a closed set of choices — never a form definition with behaviour.
type CheckoutField struct {
	// Key is the name the answer is stored under.
	Key string `json:"key"`
	// Label is what the customer reads.
	Label string `json:"label"`
	// Type is "text", "number", "bool" or "select".
	Type string `json:"type"`
	// Required refuses checkout without an answer. An optional field is the default because a
	// field that cannot be skipped is a reason to abandon a purchase.
	Required bool `json:"required,omitempty"`
	// Choices closes the accepted answers for a select.
	Choices []string `json:"choices,omitempty"`
	// Help is one sentence shown under the input. Plain text.
	Help string `json:"help,omitempty"`
}

// Declarations is the whole of what an extension asks the panel to show.
//
// It is returned by Validate, so a caller that wants the validated set does not have to trust
// the raw result it was handed.
type Declarations struct {
	NavItems       []NavItem
	ServiceActions []ServiceAction
	AdminResources []AdminResource
	Pages          []Page
	CheckoutFields []CheckoutField
}

// ValidateSurface checks the declarations this file adds, and returns them normalised.
//
// It is separate from UIDescribeResult.Validate so the two can be read on their own: hooks.go
// answers "may this button exist", this answers "may this screen exist". Both are called from
// the same place, and both refuse the whole set rather than a part of it.
func (r UIDescribeResult) ValidateSurface(advertised func(method string) bool, known func(permission string) bool) (Declarations, error) {
	out := Declarations{NavItems: r.NavItems, ServiceActions: r.ServiceActions}

	if len(r.AdminResources) > MaxAdminResources {
		return out, fmt.Errorf("%w: %d admin resources, the limit is %d",
			ErrInvalidParams, len(r.AdminResources), MaxAdminResources)
	}
	if len(r.Pages) > MaxPages {
		return out, fmt.Errorf("%w: %d pages, the limit is %d", ErrInvalidParams, len(r.Pages), MaxPages)
	}
	if len(r.CheckoutFields) > MaxCheckoutFields {
		return out, fmt.Errorf("%w: %d checkout fields, the limit is %d",
			ErrInvalidParams, len(r.CheckoutFields), MaxCheckoutFields)
	}

	// One id space across every surface, and one path space across the two that have a path: a
	// page and a resource sharing a path would make the router pick one and silently drop the
	// other, and an operator would report "the extension's screen is missing" with no way to
	// see why.
	ids := make(map[string]struct{}, len(r.AdminResources)+len(r.Pages))
	paths := make(map[string]struct{}, len(r.AdminResources)+len(r.Pages))

	for i, res := range r.AdminResources {
		if err := res.validate(advertised, known); err != nil {
			return out, fmt.Errorf("%w: admin_resources[%d]: %w", ErrInvalidParams, i, err)
		}
		key := "resource:" + res.ID
		if _, dup := ids[key]; dup {
			return out, fmt.Errorf("%w: admin_resources[%d]: duplicate id %q", ErrInvalidParams, i, res.ID)
		}
		ids[key] = struct{}{}
		if _, dup := paths[res.Path]; dup {
			return out, fmt.Errorf("%w: admin_resources[%d]: path %q is already declared",
				ErrInvalidParams, i, res.Path)
		}
		paths[res.Path] = struct{}{}
	}

	for i, page := range r.Pages {
		if err := page.validate(advertised, known); err != nil {
			return out, fmt.Errorf("%w: pages[%d]: %w", ErrInvalidParams, i, err)
		}
		key := "page:" + page.ID
		if _, dup := ids[key]; dup {
			return out, fmt.Errorf("%w: pages[%d]: duplicate id %q", ErrInvalidParams, i, page.ID)
		}
		ids[key] = struct{}{}
		if _, dup := paths[page.Path]; dup {
			return out, fmt.Errorf("%w: pages[%d]: path %q is already declared", ErrInvalidParams, i, page.Path)
		}
		paths[page.Path] = struct{}{}
	}

	keys := make(map[string]struct{}, len(r.CheckoutFields))
	for i, field := range r.CheckoutFields {
		if err := field.validate(); err != nil {
			return out, fmt.Errorf("%w: checkout_config.fields[%d]: %w", ErrInvalidParams, i, err)
		}
		if _, dup := keys[field.Key]; dup {
			return out, fmt.Errorf("%w: checkout_config.fields[%d]: duplicate key %q",
				ErrInvalidParams, i, field.Key)
		}
		keys[field.Key] = struct{}{}
	}

	out.AdminResources = r.AdminResources
	out.Pages = r.Pages
	out.CheckoutFields = r.CheckoutFields
	return out, nil
}

func (r AdminResource) validate(advertised func(string) bool, known func(string) bool) error {
	if err := validateHookID(r.ID); err != nil {
		return fmt.Errorf("id: %w", err)
	}
	if err := validateHookText("label", r.Label, maxHookLabel); err != nil {
		return err
	}
	if err := validateHookText("plural", r.Plural, maxHookLabel); err != nil {
		return err
	}
	if err := validateHookPath(r.Path); err != nil {
		return err
	}
	if err := validateHookIcon(r.Icon); err != nil {
		return err
	}
	if err := validateHookPermission(r.Permission, known); err != nil {
		return err
	}
	if err := validateAdvertisedMethod(r.Method, advertised); err != nil {
		return err
	}
	if len(r.Columns) == 0 {
		return fmt.Errorf("%w: a resource with no columns cannot be drawn", ErrInvalidParams)
	}
	if len(r.Columns) > maxResourceColumn {
		return fmt.Errorf("%w: %d columns, the limit is %d", ErrInvalidParams, len(r.Columns), maxResourceColumn)
	}
	seen := make(map[string]struct{}, len(r.Columns))
	primaries := 0
	for i, col := range r.Columns {
		if err := col.validate(); err != nil {
			return fmt.Errorf("columns[%d]: %w", i, err)
		}
		if _, dup := seen[col.Key]; dup {
			return fmt.Errorf("%w: columns[%d]: duplicate key %q", ErrInvalidParams, i, col.Key)
		}
		seen[col.Key] = struct{}{}
		if col.Primary {
			primaries++
		}
	}
	if primaries > 1 {
		return fmt.Errorf("%w: %d columns are marked primary, at most one may be",
			ErrInvalidParams, primaries)
	}
	return nil
}

func (c AdminColumn) validate() error {
	if err := validateHookID(c.Key); err != nil {
		return fmt.Errorf("key: %w", err)
	}
	if err := validateHookText("label", c.Label, maxHookLabel); err != nil {
		return err
	}
	if len(c.Type) > maxColumnTypeBytes {
		return fmt.Errorf("%w: type longer than %d bytes", ErrInvalidParams, maxColumnTypeBytes)
	}
	if _, ok := columnTypes[c.Type]; !ok {
		return fmt.Errorf("%w: column %q has type %q, which this panel cannot render",
			ErrInvalidParams, c.Key, c.Type)
	}
	return nil
}

func (p Page) validate(advertised func(string) bool, known func(string) bool) error {
	if err := validateHookID(p.ID); err != nil {
		return fmt.Errorf("id: %w", err)
	}
	if err := validateHookText("label", p.Label, maxHookLabel); err != nil {
		return err
	}
	if err := validateHookPath(p.Path); err != nil {
		return err
	}
	if err := validateHookIcon(p.Icon); err != nil {
		return err
	}
	if err := validateHookPermission(p.Permission, known); err != nil {
		return err
	}
	return validateAdvertisedMethod(p.Method, advertised)
}

func (f CheckoutField) validate() error {
	// A checkout field's key is a form name, not a hook id: it goes into the stored answers and
	// may be dotted ("server.hostname"), so it is checked as an identifier rather than through
	// validateHookID's stricter pattern.
	if !checkoutKeyPattern.MatchString(f.Key) || len(f.Key) > maxHookID {
		return fmt.Errorf("%w: key %q must be lower-case letters, digits, dot, dash or underscore",
			ErrInvalidParams, f.Key)
	}
	if err := validateHookText("label", f.Label, maxHookLabel); err != nil {
		return err
	}
	switch f.Type {
	case "text", "number", "bool", "select":
	default:
		return fmt.Errorf("%w: field %q has type %q; the accepted types are text, number, bool and select",
			ErrInvalidParams, f.Key, f.Type)
	}
	if f.Help != "" {
		if err := validateHookText("help", f.Help, maxHookConfirm); err != nil {
			return err
		}
	}
	if f.Type != "select" {
		if len(f.Choices) > 0 {
			return fmt.Errorf("%w: field %q offers choices but is not a select",
				ErrInvalidParams, f.Key)
		}
		return nil
	}
	if len(f.Choices) == 0 {
		return fmt.Errorf("%w: select %q offers no choices, so it can never be answered",
			ErrInvalidParams, f.Key)
	}
	if len(f.Choices) > maxCheckoutChoices {
		return fmt.Errorf("%w: select %q offers %d choices, the limit is %d",
			ErrInvalidParams, f.Key, len(f.Choices), maxCheckoutChoices)
	}
	for i, choice := range f.Choices {
		if strings.TrimSpace(choice) == "" || len(choice) > maxCheckoutValueLen {
			return fmt.Errorf("%w: select %q choice %d is empty or too long",
				ErrInvalidParams, f.Key, i)
		}
	}
	return nil
}

// validateHookIcon checks an icon name against the panel's own set.
func validateHookIcon(icon string) error {
	if icon == "" {
		return fmt.Errorf("%w: icon is required: the panel renders a name from its own set, not an image",
			ErrInvalidParams)
	}
	if len(icon) > maxHookIcon || !hookIconPattern.MatchString(icon) {
		return fmt.Errorf("%w: icon %q must be a lower-case icon name (a-z, 0-9, -), not a URL or markup",
			ErrInvalidParams, icon)
	}
	return nil
}

// validateAdvertisedMethod checks a method the panel would call on the extension.
//
// Same rule as a service action: it must be a capability the extension advertised, and it must
// not be one of the core's own protocol methods, so a declaration cannot route the panel into
// initialize or another extension's namespace.
func validateAdvertisedMethod(method string, advertised func(string) bool) error {
	if method == "" {
		return fmt.Errorf("%w: method is required", ErrInvalidParams)
	}
	if len(method) > maxHookMethod {
		return fmt.Errorf("%w: method longer than %d bytes", ErrInvalidParams, maxHookMethod)
	}
	if isCoreMethod(method) {
		return fmt.Errorf("%w: method %q is a core protocol method and cannot be declared as a hook",
			ErrInvalidParams, method)
	}
	if advertised == nil || !advertised(method) {
		return fmt.Errorf("%w: method %q was not advertised as a capability in the handshake",
			ErrInvalidParams, method)
	}
	return nil
}
