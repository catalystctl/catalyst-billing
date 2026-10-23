// Service metadata (SVC-02, SVC-03, SVC-04, SVC-07, SVC-08).
//
// What a service *is*, in words. The row already carried enough to provision and to bill; it
// carried nothing a human could read. This file is the read and write path for the three
// things that fix that — a label the customer chose, a description of what the box actually
// is, and the operator-defined properties a support agent needs — plus the one function a
// document is allowed to call to print them.
//
// Five ideas shape it, and each is recorded because the next person will otherwise "simplify"
// it back into the defect it prevents.
//
//  1. **The row records what a human wrote; the read path derives the rest.** `label` and
//     `description` are empty until somebody types one, and empty is not a display value: a
//     label falls back to the product's name, and a description is derived from the product,
//     the plan and the selected config options. A service is therefore never blank, and
//     "the customer cleared their name" stays distinguishable from "they never set one".
//
//  2. **A property is a definition plus a value.** The value lives on the service (one JSON
//     object, keyed by the definition's key); the label, the ordering and — the decision that
//     matters — whether it may be printed on a document live in `service_property_definitions`.
//     A value with no definition is refused by the writer *and* by a database trigger, because
//     a value nobody can render is a value nobody can explain.
//
//  3. **`show_on_invoice` on the definition is the mechanism.** A property reaches a document
//     exactly when its definition says so and the service carries a value. It is deliberately
//     not one global setting: a rack number belongs on the document, an internal asset tag does
//     not, and an operator is the only person who knows which is which.
//
//  4. **A document never receives a raw string.** Every value that can reach a document —
//     property values here, and the customer-authored label and description through
//     SanitizeDocumentText — is sanitised on the way out, not only validated on the way in.
//     A row written before a rule existed is exactly the row that would otherwise print a
//     control character into a PDF. InvoiceProperties is the only supported way to read
//     property values for a document.
//
//  5. **A customer edits their own service and nothing else.** Label and description are
//     theirs (service:write:self); properties are the operator's record of the installation
//     (service:write:all). Every customer-scoped statement carries the ownership predicate in
//     SQL, so another customer's service id is indistinguishable from one that does not exist.
package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/catalyst/billing/internal/calendar"
	"github.com/catalyst/billing/internal/money"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Bounds. They match the CHECK constraints in 0029_service_metadata.sql: a value that passes
// here and fails there would be a 500 instead of a 400, so the two must agree.
const (
	// MaxServiceLabel bounds a label. It is printed in a page heading and a table cell.
	MaxServiceLabel = 120
	// MaxServiceDescription bounds a description. A description is a paragraph, not a document.
	MaxServiceDescription = 2000
	// MaxPropertyValue bounds one property value.
	MaxPropertyValue = 200
	// MaxServiceProperties bounds how many properties one service may carry. The column is
	// capped at 8 KiB as well; the count is the limit a person can actually read.
	MaxServiceProperties = 25
	// MaxPropertyLabel bounds a definition's label.
	MaxPropertyLabel = 120
	// PropertyKeyPattern is the shape of a property key: a JSON key, a form field name and a
	// column in an export, so it is lowercase and boring on purpose.
	PropertyKeyPattern = `^[a-z][a-z0-9_]{0,63}$`
)

// Metadata errors callers branch on. They are values rather than strings so a message change
// cannot alter control flow, and each maps to one stable problem code in the HTTP layer.
var (
	// ErrLabelInvalid is returned for a label that is too long.
	ErrLabelInvalid = errors.New("service: the label is not acceptable")
	// ErrDescriptionInvalid is returned for a description that is too long or carries a
	// control character other than a newline or a tab.
	ErrDescriptionInvalid = errors.New("service: the description is not acceptable")
	// ErrUnknownProperty is returned for a value whose key has no definition. Storing it would
	// create a value nobody can label or print.
	ErrUnknownProperty = errors.New("service: no such property is defined")
	// ErrPropertyValueInvalid is returned for a value that is too long.
	ErrPropertyValueInvalid = errors.New("service: the property value is not acceptable")
	// ErrTooManyProperties is returned when a service would exceed MaxServiceProperties.
	ErrTooManyProperties = errors.New("service: this service carries too many properties")
	// ErrPropertyInUse is returned when a definition is deleted while a service still carries
	// its value. The values are cleared first, or the definition is renamed.
	ErrPropertyInUse = errors.New("service: a service still carries this property")
)

// PropertyDefinition is one operator-defined property.
//
// It is installation configuration, not per-product: "rack" describes the installation, and a
// per-product definition would make the same rack a different property on every product.
type PropertyDefinition struct {
	Key string
	// Label is what a human reads. Never the key: `asset_tag` is not a label.
	Label       string
	Description string
	// ShowOnInvoice is decision 3 above: print the value on an invoice or credit note.
	ShowOnInvoice bool
	// CustomerVisible says whether the customer sees the value on their own service page. An
	// internal asset tag is a real property the customer should not read.
	CustomerVisible bool
	SortOrder       int
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// PropertyDefinitionInput is a create or an edit of a definition.
type PropertyDefinitionInput struct {
	Key           string
	Label         string
	Description   string
	ShowOnInvoice bool
	// CustomerVisible is a pointer so an edit that does not mention it keeps the current value,
	// and a create that does not mention it gets the friendly default: visible. A property an
	// operator forgot to classify is shown to the customer rather than silently hidden.
	CustomerVisible *bool
	SortOrder       int
	// ActorID is the staff user who made the change, for the audit row.
	ActorID string
}

// PropertyValue is one definition joined with the value a service carries for it.
type PropertyValue struct {
	Key             string
	Label           string
	Description     string
	Value           string
	ShowOnInvoice   bool
	CustomerVisible bool
	SortOrder       int
}

// PropertyLine is one label/value pair, sanitised, ready to print on a document.
//
// It carries no key and no flag: a document prints words, and by the time a line exists the
// decision "does this print" has already been made.
type PropertyLine struct {
	Label string
	Value string
}

// ServiceMetadata is everything needed to answer "what is this service?".
type ServiceMetadata struct {
	ServiceID string
	Status    string

	// Customer and product identity. The customer's email is the staff view's link target.
	CustomerID    string
	CustomerEmail string
	ProductID     string
	ProductName   string

	// SVC-07: the plan, the quantity and the price a support agent needs. The plan and the
	// price come through the subscription's price version, which is the immutable catalog row
	// the service renews at.
	PriceVersionID string
	PlanName       string
	Interval       string
	Currency       string
	AmountMinor    int64
	SetupMinor     int64
	// OrderID is the order the service was sold on. Nil for a service provisioned from a
	// manual invoice, which has no order.
	OrderID  *string
	Quantity int

	// What a human wrote. Empty means nobody wrote one; see EffectiveLabel.
	Label       string
	Description string
	// DerivedDescription is always populated from the product, the plan and the config
	// options, so the UI can offer it as a starting point for an edit.
	DerivedDescription string
	// EffectiveLabel and EffectiveDescription are what to display: the written text when there
	// is one, the derived text otherwise.
	EffectiveLabel       string
	EffectiveDescription string

	// CustomProperties is the stored object, filtered to the properties the audience may see.
	CustomProperties map[string]string
	// Properties is the same data with the definition's label and ordering attached, in
	// definition order.
	Properties []PropertyValue
	// Config is the stored config-option selection, which is what the description is derived
	// from and what a support agent reads to know the box's size.
	Config []ServiceConfig

	UpdatedAt time.Time
}

// PriceAmount is the plan's price as money, so the wire form can carry integer minor units and
// an explicit currency rather than a float.
func (m ServiceMetadata) PriceAmount() (money.Amount, bool) {
	cur, err := currencyOf(m.Currency)
	if err != nil {
		return money.Amount{}, false
	}
	return money.New(m.AmountMinor, cur), true
}

// SetupAmount is the plan's setup fee, when it has one.
func (m ServiceMetadata) SetupAmount() (money.Amount, bool) {
	if m.SetupMinor == 0 {
		return money.Amount{}, false
	}
	return m.PriceAmount()
}

// MetadataUpdate asks to change a service's metadata.
//
// The fields are pointers so "absent" and "empty" stay different: a customer renaming their
// service must not blank the description an operator wrote, and clearing a property is a
// deliberate act (`custom_properties: {"rack": ""}`), not the default.
type MetadataUpdate struct {
	ServiceID string
	// UserID is the actor. It is the audit row's actor and, when Staff is false, the owner the
	// statement is scoped to.
	UserID string
	// Staff says the caller holds service:write:all and may therefore edit the properties as
	// well as the label and description.
	Staff bool
	// Label replaces the label. An empty string clears it, which restores the product's name:
	// the column records what a human wrote, and clearing is a legitimate thing to do.
	Label       *string
	Description *string
	// CustomProperties applies a partial edit to the stored object. A nil map means "leave it
	// alone"; an empty string unsets one property. Staff only.
	CustomProperties map[string]string
}

// ---------------------------------------------------------------- definitions --

// PropertyDefinitions lists the defined properties in display order.
func (s *Store) PropertyDefinitions(ctx context.Context) ([]PropertyDefinition, error) {
	return propertyDefinitions(ctx, s.pool)
}

// propertyDefinitions reads the definitions through the given querier, so a write can validate
// a property key against the same snapshot it is about to insert into.
func propertyDefinitions(ctx context.Context, q Querier) ([]PropertyDefinition, error) {
	rows, err := q.Query(ctx, `
		SELECT key, label, description, show_on_invoice, customer_visible, sort_order,
		       created_at, updated_at
		FROM service_property_definitions
		ORDER BY sort_order, key`)
	if err != nil {
		return nil, fmt.Errorf("service: list property definitions: %w", err)
	}
	defer rows.Close()

	out := []PropertyDefinition{}
	for rows.Next() {
		var d PropertyDefinition
		if err := rows.Scan(&d.Key, &d.Label, &d.Description, &d.ShowOnInvoice,
			&d.CustomerVisible, &d.SortOrder, &d.CreatedAt, &d.UpdatedAt); err != nil {
			return nil, fmt.Errorf("service: scan property definition: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// SavePropertyDefinition creates or edits one definition.
//
// It is an upsert on the key because a definition has no identity beyond its key: the key is
// what every service's `custom_properties` object is written against, so re-keying a
// definition would silently orphan every value. Renaming the *label* is the supported edit.
func (s *Store) SavePropertyDefinition(ctx context.Context, in PropertyDefinitionInput) (PropertyDefinition, error) {
	key := strings.TrimSpace(in.Key)
	if !validPropertyKey(key) {
		return PropertyDefinition{}, fmt.Errorf("%w: a key is lowercase letters, digits and "+
			"underscores, starting with a letter", ErrInvalidInput)
	}
	label := singleLine(in.Label)
	if label == "" {
		return PropertyDefinition{}, fmt.Errorf("%w: a property needs a label", ErrLabelInvalid)
	}
	if len([]rune(label)) > MaxPropertyLabel {
		return PropertyDefinition{}, fmt.Errorf("%w: the label is longer than %d characters",
			ErrLabelInvalid, MaxPropertyLabel)
	}
	description := strings.TrimSpace(in.Description)
	if len([]rune(description)) > 500 {
		return PropertyDefinition{}, fmt.Errorf("%w: the description is longer than 500 characters",
			ErrInvalidInput)
	}
	if hasControl(description) {
		return PropertyDefinition{}, fmt.Errorf("%w: the description carries a control character",
			ErrInvalidInput)
	}

	var out PropertyDefinition
	err := s.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `
			INSERT INTO service_property_definitions
				(key, label, description, show_on_invoice, customer_visible, sort_order)
			VALUES ($1, $2, $3, $4, COALESCE($5::boolean, true), $6)
			ON CONFLICT (key) DO UPDATE
				SET label = EXCLUDED.label,
				    description = EXCLUDED.description,
				    show_on_invoice = EXCLUDED.show_on_invoice,
				    customer_visible = COALESCE($5::boolean,
				                                service_property_definitions.customer_visible),
				    sort_order = EXCLUDED.sort_order,
				    updated_at = now()
			RETURNING key, label, description, show_on_invoice, customer_visible, sort_order,
			          created_at, updated_at`,
			key, label, description, in.ShowOnInvoice, in.CustomerVisible, in.SortOrder).
			Scan(&out.Key, &out.Label, &out.Description, &out.ShowOnInvoice, &out.CustomerVisible,
				&out.SortOrder, &out.CreatedAt, &out.UpdatedAt); err != nil {
			return fmt.Errorf("service: save property definition: %w", err)
		}

		// The target is the definition, whose identity is a key rather than a uuid, so the audit
		// row names it in `changes` and leaves target_id empty rather than writing a string into
		// a uuid column.
		return s.audit(ctx, tx, "service.property_definition_saved", "service_property", "",
			in.ActorID, map[string]any{
				"key":              key,
				"label":            label,
				"show_on_invoice":  in.ShowOnInvoice,
				"customer_visible": out.CustomerVisible,
				"sort_order":       in.SortOrder,
			})
	})
	if err != nil {
		return PropertyDefinition{}, err
	}
	return out, nil
}

// DeletePropertyDefinition removes a definition no service carries a value for.
//
// The database trigger refuses the delete when a service still holds the key; that refusal is
// translated into ErrPropertyInUse so the handler can say which condition failed instead of
// reporting a constraint name.
func (s *Store) DeletePropertyDefinition(ctx context.Context, key, actorID string) error {
	key = strings.TrimSpace(key)
	if !validPropertyKey(key) {
		return ErrNotFound
	}
	return s.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `DELETE FROM service_property_definitions WHERE key = $1`, key); err != nil {
			if isForeignKeyViolation(err) {
				return fmt.Errorf("%w: %s", ErrPropertyInUse, key)
			}
			return fmt.Errorf("service: delete property definition: %w", err)
		}
		return s.audit(ctx, tx, "service.property_definition_deleted", "service_property", "",
			actorID, map[string]any{"key": key})
	})
}

// ------------------------------------------------------------------- read --

// ServiceMetadata returns a service's metadata without a customer scope. It is the staff read;
// a caller that serves a customer must use ServiceMetadataOwnedBy.
func (s *Store) ServiceMetadata(ctx context.Context, serviceID string) (ServiceMetadata, error) {
	return s.serviceMetadata(ctx, serviceID, "")
}

// ServiceMetadataOwnedBy returns the metadata of a service the user's customer account holds.
//
// The ownership predicate is part of the query, so a service that belongs to somebody else is
// reported as missing — the same answer as an id that does not exist, which is what makes
// identifier substitution useless rather than merely detected.
func (s *Store) ServiceMetadataOwnedBy(ctx context.Context, serviceID, userID string) (ServiceMetadata, error) {
	if strings.TrimSpace(userID) == "" {
		return ServiceMetadata{}, ErrNotFound
	}
	return s.serviceMetadata(ctx, serviceID, userID)
}

// serviceMetadata loads the row, the plan, the selection and the properties.
func (s *Store) serviceMetadata(ctx context.Context, serviceID, ownerUserID string) (ServiceMetadata, error) {
	var (
		out            ServiceMetadata
		planName       *string
		priceVersionID *string
		interval       *string
		currency       *string
		amountMinor    *int64
		setupMinor     *int64
		rawProperties  []byte
	)
	query := `
		SELECT s.id, s.status, s.customer_id, c.email, s.product_id, p.name,
		       pv.id, pv.name, pv.interval, pv.currency, pv.amount_minor, pv.setup_minor,
		       s.order_id, s.quantity, s.label, s.description, s.custom_properties, s.updated_at
		FROM services s
		JOIN customers c ON c.id = s.customer_id
		JOIN products p ON p.id = s.product_id
		LEFT JOIN subscriptions sub ON sub.id = s.subscription_id
		LEFT JOIN price_versions pv ON pv.id = sub.price_version_id`
	args := []any{serviceID}
	if ownerUserID != "" {
		query += `
		JOIN customer_accounts ca ON ca.customer_id = s.customer_id`
	}
	query += `
		WHERE s.id = $1`
	if ownerUserID != "" {
		query += ` AND ca.user_id = $2`
		args = append(args, ownerUserID)
	}

	err := s.pool.QueryRow(ctx, query, args...).Scan(
		&out.ServiceID, &out.Status, &out.CustomerID, &out.CustomerEmail, &out.ProductID,
		&out.ProductName, &priceVersionID, &planName, &interval, &currency, &amountMinor,
		&setupMinor, &out.OrderID, &out.Quantity, &out.Label, &out.Description,
		&rawProperties, &out.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return ServiceMetadata{}, ErrNotFound
	}
	if err != nil {
		return ServiceMetadata{}, fmt.Errorf("service: load service metadata: %w", err)
	}
	if priceVersionID != nil {
		out.PriceVersionID = *priceVersionID
	}
	if planName != nil {
		out.PlanName = *planName
	}
	if interval != nil {
		out.Interval = *interval
	}
	if currency != nil {
		out.Currency = *currency
	}
	if amountMinor != nil {
		out.AmountMinor = *amountMinor
	}
	if setupMinor != nil {
		out.SetupMinor = *setupMinor
	}

	configs, err := s.ServiceConfigs(ctx, serviceID)
	if err != nil {
		return ServiceMetadata{}, err
	}
	out.Config = configs

	stored, err := decodeProperties(rawProperties)
	if err != nil {
		return ServiceMetadata{}, err
	}

	definitions, err := s.PropertyDefinitions(ctx)
	if err != nil {
		return ServiceMetadata{}, err
	}

	out.Properties = make([]PropertyValue, 0, len(definitions))
	out.CustomProperties = map[string]string{}
	for _, d := range definitions {
		value, ok := stored[d.Key]
		if !ok {
			continue
		}
		if ownerUserID != "" && !d.CustomerVisible {
			// The customer's own page shows what the operator chose to show them. The value is
			// omitted rather than rendered blank, so the page cannot reveal that a property
			// exists by showing an empty row for it.
			continue
		}
		out.Properties = append(out.Properties, PropertyValue{
			Key: d.Key, Label: d.Label, Description: d.Description, Value: value,
			ShowOnInvoice: d.ShowOnInvoice, CustomerVisible: d.CustomerVisible,
			SortOrder: d.SortOrder,
		})
		out.CustomProperties[d.Key] = value
	}

	out.DerivedDescription = deriveDescription(out.ProductName, out.PlanName, out.Interval, configs)
	out.EffectiveLabel = effectiveLabel(out.Label, out.ProductName, out.Quantity)
	out.EffectiveDescription = strings.TrimSpace(out.Description)
	if out.EffectiveDescription == "" {
		out.EffectiveDescription = out.DerivedDescription
	}
	return out, nil
}

// ------------------------------------------------------------------ write --

// UpdateServiceMetadata applies a label, a description and (for staff) the properties.
//
// One transaction, and the audit row is written inside it: a metadata edit that is visible
// without the record of who made it is exactly the kind of quiet change the append-only log
// exists to prevent.
func (s *Store) UpdateServiceMetadata(ctx context.Context, in MetadataUpdate) (ServiceMetadata, error) {
	label, description, err := prepareText(in)
	if err != nil {
		return ServiceMetadata{}, err
	}

	err = s.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		// Lock the row first: the property write below is a read-modify-write of a JSON object,
		// and two concurrent editors must not merge into one another.
		var (
			currentLabel       string
			currentDescription string
			rawProperties      []byte
		)
		lockQuery := `
			SELECT s.label, s.description, s.custom_properties
			FROM services s`
		lockArgs := []any{in.ServiceID}
		if !in.Staff {
			lockQuery += `
			JOIN customer_accounts ca ON ca.customer_id = s.customer_id`
		}
		lockQuery += `
			WHERE s.id = $1`
		if !in.Staff {
			lockQuery += ` AND ca.user_id = $2`
			lockArgs = append(lockArgs, in.UserID)
		}
		lockQuery += ` FOR UPDATE OF s`

		err := tx.QueryRow(ctx, lockQuery, lockArgs...).
			Scan(&currentLabel, &currentDescription, &rawProperties)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("service: lock service metadata: %w", err)
		}

		newLabel := currentLabel
		if label != nil {
			newLabel = *label
		}
		newDescription := currentDescription
		if description != nil {
			newDescription = *description
		}

		properties := rawProperties
		changes := map[string]any{}
		if in.CustomProperties != nil {
			if !in.Staff {
				return fmt.Errorf("%w: custom properties are edited by staff", ErrInvalidInput)
			}
			stored, err := decodeProperties(rawProperties)
			if err != nil {
				return err
			}
			merged, err := mergeProperties(ctx, tx, stored, in.CustomProperties)
			if err != nil {
				return err
			}
			encoded, err := encodeProperties(merged)
			if err != nil {
				return err
			}
			properties = encoded
			changes["custom_properties"] = merged
		}

		if label != nil && *label != currentLabel {
			changes["label"] = *label
		}
		if description != nil && *description != currentDescription {
			changes["description"] = *description
		}
		if len(changes) == 0 {
			// Nothing to write and nothing to record. An audit row for a no-op edit is noise in
			// the one log that has to stay readable.
			return nil
		}

		if _, err := tx.Exec(ctx, `
			UPDATE services
			SET label = $2, description = $3, custom_properties = $4, updated_at = now()
			WHERE id = $1`, in.ServiceID, newLabel, newDescription, string(properties)); err != nil {
			if isForeignKeyViolation(err) {
				// The only foreign key this statement can trip is the property-key trigger.
				return ErrUnknownProperty
			}
			return fmt.Errorf("service: update service metadata: %w", err)
		}

		return s.audit(ctx, tx, "service.metadata_updated", "service", in.ServiceID,
			in.UserID, changes)
	})
	if err != nil {
		return ServiceMetadata{}, err
	}
	if in.Staff {
		return s.ServiceMetadata(ctx, in.ServiceID)
	}
	return s.ServiceMetadataOwnedBy(ctx, in.ServiceID, in.UserID)
}

// prepareText validates the label and the description, returning nil for a field the request
// did not mention.
//
// A label is collapsed to one line rather than refused for a pasted newline: the column records
// what will be printed, and a newline inside a name reflows a table. A description keeps its
// newlines — it is a paragraph — so it is the one field where a control character is an error.
func prepareText(in MetadataUpdate) (label, description *string, err error) {
	if in.Label != nil {
		value := singleLine(*in.Label)
		if len([]rune(value)) > MaxServiceLabel {
			return nil, nil, fmt.Errorf("%w: the label is longer than %d characters",
				ErrLabelInvalid, MaxServiceLabel)
		}
		label = &value
	}
	if in.Description != nil {
		value := strings.TrimSpace(*in.Description)
		if len([]rune(value)) > MaxServiceDescription {
			return nil, nil, fmt.Errorf("%w: the description is longer than %d characters",
				ErrDescriptionInvalid, MaxServiceDescription)
		}
		if hasControl(value) {
			return nil, nil, fmt.Errorf("%w: the description carries a control character",
				ErrDescriptionInvalid)
		}
		description = &value
	}
	return label, description, nil
}

// mergeProperties applies a partial edit to the stored object.
//
// The whole edit is validated before anything is written: a request that names one good property
// and one undefined one changes nothing, because a half-applied edit is a state the customer
// never asked for.
func mergeProperties(ctx context.Context, tx pgx.Tx, stored, edit map[string]string) (map[string]string, error) {
	definitions, err := propertyDefinitions(ctx, tx)
	if err != nil {
		return nil, err
	}
	known := make(map[string]struct{}, len(definitions))
	for _, d := range definitions {
		known[d.Key] = struct{}{}
	}

	merged := make(map[string]string, len(stored)+len(edit))
	for key, value := range stored {
		merged[key] = value
	}
	for key, value := range edit {
		key = strings.TrimSpace(key)
		if _, ok := known[key]; !ok {
			return nil, fmt.Errorf("%w: %s", ErrUnknownProperty, key)
		}
		value = singleLine(value)
		if value == "" {
			// An empty value unsets the property. Keeping a row of empty strings would make
			// "set to nothing" and "not set" two states that render identically.
			delete(merged, key)
			continue
		}
		if len([]rune(value)) > MaxPropertyValue {
			return nil, fmt.Errorf("%w: %s is longer than %d characters",
				ErrPropertyValueInvalid, key, MaxPropertyValue)
		}
		merged[key] = value
	}
	if len(merged) > MaxServiceProperties {
		return nil, fmt.Errorf("%w: the limit is %d", ErrTooManyProperties, MaxServiceProperties)
	}
	return merged, nil
}

// ----------------------------------------------------------------- documents --

// InvoiceProperties returns the sanitised properties an invoice may print.
//
// This is the seam between this package and a document. It resolves the services an invoice
// belongs to through the subscription the invoice was raised for (and through the order, for a
// service sold on one), keeps the properties whose definition says `show_on_invoice`, and
// sanitises every value on the way out. A property that is merely `customer_visible` is *not*
// printed: the two flags answer different questions, and conflating them would put an internal
// note on a customer's invoice.
//
// The result is ordered by the definition's sort order and is empty — never an error — when the
// installation has configured nothing, which is the state of every installation that never
// opened the properties screen.
func (s *Store) InvoiceProperties(ctx context.Context, invoiceID string) ([]PropertyLine, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT d.label, d.sort_order, d.key, s.custom_properties -> d.key AS value
		FROM invoices i
		JOIN services s
		  ON (i.subscription_id IS NOT NULL AND s.subscription_id = i.subscription_id)
		  OR (i.order_id IS NOT NULL AND s.order_id = i.order_id)
		JOIN service_property_definitions d
		  ON d.show_on_invoice
		 AND s.custom_properties ? d.key
		WHERE i.id = $1
		ORDER BY d.sort_order, d.key, s.id`, invoiceID)
	if err != nil {
		return nil, fmt.Errorf("service: load invoice properties: %w", err)
	}
	defer rows.Close()

	out := []PropertyLine{}
	seen := map[string]struct{}{}
	for rows.Next() {
		var (
			label     string
			sortOrder int
			key       string
			raw       []byte
		)
		if err := rows.Scan(&label, &sortOrder, &key, &raw); err != nil {
			return nil, fmt.Errorf("service: scan invoice property: %w", err)
		}
		if _, dup := seen[key]; dup {
			// Two services on one invoice can carry the same property (a customer who bought two
			// boxes in one order). The document prints one line per property, not one per
			// service: a repeated label with two different values reads as a contradiction.
			continue
		}
		value := sanitize(decodeJSONString(raw), MaxPropertyValue)
		if value == "" {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, PropertyLine{Label: SanitizeDocumentText(label), Value: value})
	}
	return out, rows.Err()
}

// SanitizeDocumentText makes a customer-authored string safe to print.
//
// It removes control and format characters (a newline inside a name reflows a table; a
// right-to-left override reverses the rest of the line), collapses runs of whitespace to one
// space, trims, and caps the length. It is exported because the label and the description are
// the other free text a document may print, and a document must not have to reimplement the
// rule.
//
// It does not escape HTML: a document is drawn as text (the PDF renderer draws strings, the web
// view sets textContent), and escaping here would print `&amp;` on an invoice.
func SanitizeDocumentText(raw string) string {
	return sanitize(raw, MaxServiceLabel)
}

// sanitize is the one implementation of the rule above.
func sanitize(raw string, limit int) string {
	var b strings.Builder
	b.Grow(len(raw))
	spaced := false
	written := 0
	for _, r := range raw {
		switch {
		case r == '\n' || r == '\r' || r == '\t' || r == ' ':
			spaced = true
			continue
		case unicode.IsControl(r), unicode.Is(unicode.Cf, r), r == '\uFEFF':
			// Zero-width and bidi characters are format characters, not text. Dropping them
			// keeps a value that looks like "12" from printing as something else entirely.
			continue
		}
		if spaced && written > 0 {
			b.WriteByte(' ')
			written++
		}
		spaced = false
		b.WriteRune(r)
		written++
		if written >= limit {
			break
		}
	}
	return strings.TrimSpace(b.String())
}

// ---------------------------------------------------------------- helpers --

// validPropertyKey reports whether a key matches PropertyKeyPattern.
//
// It is written by hand rather than compiled as a package-level regexp because the pattern is
// trivial and a regexp that is never recompiled is one more global to reason about.
func validPropertyKey(key string) bool {
	if key == "" || len(key) > 64 {
		return false
	}
	for i, r := range key {
		switch {
		case r >= 'a' && r <= 'z':
		case i > 0 && (r >= '0' && r <= '9' || r == '_'):
		default:
			return false
		}
	}
	return true
}

// singleLine trims a value to one line, which is what a label and a property value are. It is
// applied on write, so what is stored is already what a document would print.
func singleLine(raw string) string {
	return sanitize(raw, MaxServiceLabel*4)
}

// hasControl reports whether a string carries a character that has no business in a paragraph.
// A newline, a carriage return and a tab are legitimate; the database CHECK strips those before
// it applies the same test.
func hasControl(raw string) bool {
	for _, r := range raw {
		if r == '\n' || r == '\r' || r == '\t' {
			continue
		}
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return true
		}
	}
	return false
}

// effectiveLabel is what to display: the customer's own name for the service, or the product's
// name with the quantity when they have not chosen one.
func effectiveLabel(written, productName string, quantity int) string {
	if label := singleLine(written); label != "" {
		return label
	}
	name := strings.TrimSpace(productName)
	if quantity > 1 {
		return fmt.Sprintf("%s ×%d", name, quantity)
	}
	return name
}

// deriveDescription is SVC-04: a service is never blank.
//
// The description is built from what the service actually is — the product, the plan, the
// options that were selected and the billing cycle — so a support agent who has never seen the
// account can read it and know what the customer is paying for. It describes *facts*, never
// prices: the price is on the invoice, and a description that repeated it would go stale the
// first time the catalog changed.
func deriveDescription(productName, planName, interval string, configs []ServiceConfig) string {
	parts := make([]string, 0, 3+len(configs))
	if name := strings.TrimSpace(productName); name != "" {
		parts = append(parts, name)
	}
	if plan := strings.TrimSpace(planName); plan != "" && !strings.EqualFold(plan, productName) {
		parts = append(parts, plan)
	}
	options := make([]string, 0, len(configs))
	for _, c := range configs {
		value := singleLine(c.Value)
		if value == "" {
			continue
		}
		if name := strings.TrimSpace(c.Name); name != "" {
			options = append(options, name+": "+value)
		} else {
			options = append(options, value)
		}
	}
	if len(options) > 0 {
		parts = append(parts, strings.Join(options, ", "))
	}
	if phrase := intervalPhrase(interval); phrase != "" {
		parts = append(parts, phrase)
	}
	return strings.Join(parts, " · ")
}

// intervalPhrase turns a billing interval into the words a description uses.
func intervalPhrase(interval string) string {
	switch interval {
	case "monthly":
		return "billed monthly"
	case "quarterly":
		return "billed quarterly"
	case "semiannual":
		return "billed every six months"
	case "annual":
		return "billed annually"
	default:
		// A day-count period ("every_45_days") is a real billing period, so a description
		// that omitted it would describe a service as "billed" with no cycle at all. The
		// parse comes from internal/calendar, which is the package that decides what a
		// period is: a second parser here would be a second answer to the same question.
		if days, ok := calendar.ParseDayInterval(interval); ok {
			return fmt.Sprintf("billed every %d days", days)
		}
		return ""
	}
}

// decodeProperties reads the stored JSON object.
//
// A non-string value cannot be produced by the API or by the database CHECK, but a row could
// have been written before either existed, so a value that is not a string is skipped rather
// than crashing the read.
func decodeProperties(raw []byte) (map[string]string, error) {
	out := map[string]string{}
	if len(raw) == 0 {
		return out, nil
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, fmt.Errorf("service: decode custom properties: %w", err)
	}
	for key, value := range decoded {
		if text, ok := value.(string); ok {
			out[key] = text
		}
	}
	return out, nil
}

// encodeProperties writes the stored JSON object.
func encodeProperties(values map[string]string) ([]byte, error) {
	encoded, err := json.Marshal(values)
	if err != nil {
		return nil, fmt.Errorf("service: encode custom properties: %w", err)
	}
	return encoded, nil
}

// decodeJSONString reads one `custom_properties -> key` value.
func decodeJSONString(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return ""
	}
	return text
}

// isForeignKeyViolation reports whether an error is the trigger's refusal of a property key with
// no definition. The trigger raises foreign_key_violation on purpose so this test is the same
// one an FK would produce.
func isForeignKeyViolation(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "23503"
	}
	return false
}
