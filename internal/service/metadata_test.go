package service_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/catalyst/billing/internal/service"
)

// Service metadata (SVC-02, SVC-03, SVC-04, SVC-07, SVC-08).
//
// These run against the real database because the rules under test are mostly database rules:
// a value with no definition is refused by a trigger, a control character is refused by a CHECK,
// and a customer-scoped statement is scoped by its WHERE clause. An in-memory fake would prove
// only that the Go code agrees with itself.

// boolPtr is how a definition input says "false" rather than "not mentioned".
func boolPtr(v bool) *bool { return &v }

// defineProperties creates the definitions a test needs and returns nothing: the store is the
// thing under test, not the definition CRUD.
func defineProperties(t *testing.T, f *fixture, definitions ...service.PropertyDefinitionInput) {
	t.Helper()
	for _, d := range definitions {
		if _, err := f.store.SavePropertyDefinition(context.Background(), d); err != nil {
			t.Fatalf("define %s: %v", d.Key, err)
		}
	}
}

// TestServiceIsNeverBlank is SVC-04: a service nobody has described still says what it is.
//
// The fixture has no label and no description, so both displayed values have to come from the
// product, the plan and the billing cycle. This is the property that stops a support screen from
// showing an empty row where the answer to "what is this?" should be.
func TestServiceIsNeverBlank(t *testing.T) {
	f := newFixture(t, 1200, "monthly")

	m, err := f.store.ServiceMetadata(context.Background(), f.serviceID)
	if err != nil {
		t.Fatalf("metadata: %v", err)
	}
	if m.Label != "" || m.Description != "" {
		t.Fatalf("a fresh service has a written label or description: %q / %q", m.Label, m.Description)
	}
	if m.EffectiveLabel != "VPS Small" {
		t.Fatalf("effective label = %q, want the product's name", m.EffectiveLabel)
	}
	if !strings.Contains(m.DerivedDescription, "VPS Small") ||
		!strings.Contains(m.DerivedDescription, "Small") ||
		!strings.Contains(m.DerivedDescription, "billed monthly") {
		t.Fatalf("derived description = %q, want the product, the plan and the cycle",
			m.DerivedDescription)
	}
	if m.EffectiveDescription != m.DerivedDescription {
		t.Fatalf("effective description = %q, want the derived one", m.EffectiveDescription)
	}
	// SVC-07: the plan, the price and the quantity travel with the answer.
	if m.PlanName != "Small" || m.Currency != "USD" || m.AmountMinor != 1200 || m.Quantity != 1 {
		t.Fatalf("plan facts = %+v", m)
	}
	if amount, ok := m.PriceAmount(); !ok || amount.Minor() != 1200 || amount.Currency().Code() != "USD" {
		t.Fatalf("price amount = %v %v", amount, ok)
	}
}

// The description is derived from the selected config options, not only from the product: "what
// is this box" is mostly "how big is it".
func TestDerivedDescriptionUsesTheSelectedOptions(t *testing.T) {
	f := newFixture(t, 1200, "monthly")
	ctx := context.Background()

	var optionID string
	if err := f.pool.QueryRow(ctx, `
		INSERT INTO config_options (product_id, name, option_type, choices, sort_order)
		VALUES ($1, 'Memory', 'dropdown', '["4 GB","8 GB"]'::jsonb, 1) RETURNING id`,
		f.productID).Scan(&optionID); err != nil {
		t.Fatalf("option: %v", err)
	}
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO service_configs (service_id, config_option_id, value, amount_minor)
		VALUES ($1, $2, '8 GB', 400)`, f.serviceID, optionID); err != nil {
		t.Fatalf("selection: %v", err)
	}

	m, err := f.store.ServiceMetadata(ctx, f.serviceID)
	if err != nil {
		t.Fatalf("metadata: %v", err)
	}
	if !strings.Contains(m.DerivedDescription, "Memory: 8 GB") {
		t.Fatalf("derived description = %q, want the selected option in it", m.DerivedDescription)
	}
	if len(m.Config) != 1 || m.Config[0].Value != "8 GB" {
		t.Fatalf("config = %+v", m.Config)
	}
}

// A customer renames and describes their own service, the audit row is written inside the same
// transaction, and an edit that names one field leaves the other alone.
func TestCustomerRenamesTheirOwnService(t *testing.T) {
	f := newFixture(t, 1200, "monthly")
	ctx := context.Background()

	label := "  web-01\n "
	description := "The box that serves example.test."
	m, err := f.store.UpdateServiceMetadata(ctx, service.MetadataUpdate{
		ServiceID:   f.serviceID,
		UserID:      f.userID,
		Label:       &label,
		Description: &description,
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	// The label is collapsed to one line on write, so what is stored is what would be printed.
	if m.Label != "web-01" {
		t.Fatalf("stored label = %q, want the collapsed value", m.Label)
	}
	if m.EffectiveLabel != "web-01" || m.EffectiveDescription != description {
		t.Fatalf("effective = %q / %q", m.EffectiveLabel, m.EffectiveDescription)
	}

	var (
		action   string
		actorID  string
		changes  []byte
		rowLabel string
	)
	if err := f.pool.QueryRow(ctx, `
		SELECT action, actor_id::text, changes FROM audit_events
		WHERE target_type = 'service' AND target_id = $1 ORDER BY id DESC LIMIT 1`,
		f.serviceID).Scan(&action, &actorID, &changes); err != nil {
		t.Fatalf("audit row: %v", err)
	}
	if action != "service.metadata_updated" || actorID != f.userID {
		t.Fatalf("audit = %s by %s", action, actorID)
	}
	var decoded map[string]any
	if err := json.Unmarshal(changes, &decoded); err != nil {
		t.Fatalf("changes: %v", err)
	}
	if decoded["label"] != "web-01" || decoded["description"] != description {
		t.Fatalf("audit changes = %v", decoded)
	}

	// Renaming again must not blank the description: an absent field means "leave it alone".
	newLabel := "web-02"
	m, err = f.store.UpdateServiceMetadata(ctx, service.MetadataUpdate{
		ServiceID: f.serviceID,
		UserID:    f.userID,
		Label:     &newLabel,
	})
	if err != nil {
		t.Fatalf("second update: %v", err)
	}
	if m.Label != "web-02" || m.Description != description {
		t.Fatalf("after a rename: %q / %q", m.Label, m.Description)
	}
	if err := f.pool.QueryRow(ctx, `SELECT label FROM services WHERE id = $1`, f.serviceID).
		Scan(&rowLabel); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if rowLabel != "web-02" {
		t.Fatalf("row label = %q", rowLabel)
	}
}

// A no-op edit writes no audit row. The log is evidence, and a row that says nothing changed is
// noise in the one place that has to stay readable.
func TestAnUnchangedEditWritesNoAuditRow(t *testing.T) {
	f := newFixture(t, 1200, "monthly")
	ctx := context.Background()

	same := "web-01"
	if _, err := f.store.UpdateServiceMetadata(ctx, service.MetadataUpdate{
		ServiceID: f.serviceID, UserID: f.userID, Label: &same,
	}); err != nil {
		t.Fatalf("first update: %v", err)
	}
	before := countRows(t, f.pool, "audit_events")
	if _, err := f.store.UpdateServiceMetadata(ctx, service.MetadataUpdate{
		ServiceID: f.serviceID, UserID: f.userID, Label: &same,
	}); err != nil {
		t.Fatalf("second update: %v", err)
	}
	if after := countRows(t, f.pool, "audit_events"); after != before {
		t.Fatalf("audit rows went from %d to %d for an edit that changed nothing", before, after)
	}
}

// Another customer's service id is the same answer as an id that does not exist, and nothing is
// written. This is the never-regress rule that makes identifier substitution useless.
func TestMetadataEditIsScopedToTheCustomer(t *testing.T) {
	f := newFixture(t, 1200, "monthly")
	ctx := context.Background()

	var otherUser string
	if err := f.pool.QueryRow(ctx, `
		INSERT INTO users (email, password_hash, email_verified_at)
		VALUES ('other-'||gen_random_uuid()||'@example.test', 'x', now()) RETURNING id`).
		Scan(&otherUser); err != nil {
		t.Fatalf("other user: %v", err)
	}
	var otherCustomer string
	if err := f.pool.QueryRow(ctx, `
		INSERT INTO customers (email, name)
		VALUES ('other-'||gen_random_uuid()||'@example.test', 'Other') RETURNING id`).
		Scan(&otherCustomer); err != nil {
		t.Fatalf("other customer: %v", err)
	}
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO customer_accounts (user_id, customer_id) VALUES ($1, $2)`,
		otherUser, otherCustomer); err != nil {
		t.Fatalf("link: %v", err)
	}

	label := "stolen"
	_, err := f.store.UpdateServiceMetadata(ctx, service.MetadataUpdate{
		ServiceID: f.serviceID, UserID: otherUser, Label: &label,
	})
	if !errors.Is(err, service.ErrNotFound) {
		t.Fatalf("update by another customer: %v, want ErrNotFound", err)
	}
	if _, err := f.store.ServiceMetadataOwnedBy(ctx, f.serviceID, otherUser); !errors.Is(err, service.ErrNotFound) {
		t.Fatalf("read by another customer: %v, want ErrNotFound", err)
	}
	var stored string
	if err := f.pool.QueryRow(ctx, `SELECT label FROM services WHERE id = $1`, f.serviceID).
		Scan(&stored); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if stored != "" {
		t.Fatalf("label was written by a refused request: %q", stored)
	}
}

// A customer edits the properties the operator chose to show them, and only those.
//
// The visibility decision is made once, on the definition, and the writer enforces it: a customer
// can set `rack` because its definition is customer_visible, and naming `asset_tag` — a defined
// property the operator kept internal — is refused with ErrPropertyNotVisible and changes nothing.
func TestCustomerEditsOnlyVisibleProperties(t *testing.T) {
	f := newFixture(t, 1200, "monthly")
	ctx := context.Background()
	defineProperties(t, f,
		service.PropertyDefinitionInput{Key: "rack", Label: "Rack"},
		service.PropertyDefinitionInput{Key: "asset_tag", Label: "Asset tag",
			CustomerVisible: boolPtr(false)},
	)

	m, err := f.store.UpdateServiceMetadata(ctx, service.MetadataUpdate{
		ServiceID:        f.serviceID,
		UserID:           f.userID,
		CustomProperties: map[string]string{"rack": "B2"},
	})
	if err != nil {
		t.Fatalf("customer property write: %v", err)
	}
	if m.CustomProperties["rack"] != "B2" {
		t.Fatalf("stored properties = %v", m.CustomProperties)
	}

	// The internal property is refused, and the refusal takes the visible key down with it: a
	// half-applied edit is a state the customer never asked for.
	_, err = f.store.UpdateServiceMetadata(ctx, service.MetadataUpdate{
		ServiceID:        f.serviceID,
		UserID:           f.userID,
		CustomProperties: map[string]string{"rack": "B3", "asset_tag": "CT-0042"},
	})
	if !errors.Is(err, service.ErrPropertyNotVisible) {
		t.Fatalf("customer wrote an internal property: %v, want ErrPropertyNotVisible", err)
	}
	m, err = f.store.ServiceMetadata(ctx, f.serviceID)
	if err != nil {
		t.Fatalf("re-read: %v", err)
	}
	if m.CustomProperties["rack"] != "B2" {
		t.Fatalf("a refused edit wrote its visible key anyway: %v", m.CustomProperties)
	}
	if _, ok := m.CustomProperties["asset_tag"]; ok {
		t.Fatalf("a refused edit stored the internal key: %v", m.CustomProperties)
	}
}

// The audit row of a property edit names the keys the request touched and nothing else.
//
// The stored object also holds properties the actor may not see, and a log line that repeated them
// would hand an internal value to everyone who can read the log — which is every support agent.
func TestAPropertyEditAuditsOnlyTheEditedKeys(t *testing.T) {
	f := newFixture(t, 1200, "monthly")
	ctx := context.Background()
	defineProperties(t, f,
		service.PropertyDefinitionInput{Key: "rack", Label: "Rack"},
		service.PropertyDefinitionInput{Key: "asset_tag", Label: "Asset tag",
			CustomerVisible: boolPtr(false)},
	)
	if _, err := f.store.UpdateServiceMetadata(ctx, service.MetadataUpdate{
		ServiceID: f.serviceID, UserID: f.userID, Staff: true,
		CustomProperties: map[string]string{"asset_tag": "CT-0042"},
	}); err != nil {
		t.Fatalf("staff write: %v", err)
	}

	if _, err := f.store.UpdateServiceMetadata(ctx, service.MetadataUpdate{
		ServiceID: f.serviceID, UserID: f.userID,
		CustomProperties: map[string]string{"rack": "B2"},
	}); err != nil {
		t.Fatalf("customer write: %v", err)
	}

	var changes []byte
	if err := f.pool.QueryRow(ctx, `
		SELECT changes FROM audit_events
		WHERE action = 'service.metadata_updated' AND target_id = $1
		ORDER BY created_at DESC, id DESC LIMIT 1`, f.serviceID).Scan(&changes); err != nil {
		t.Fatalf("audit row: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(changes, &decoded); err != nil {
		t.Fatalf("decode changes: %v", err)
	}
	properties, ok := decoded["custom_properties"].(map[string]any)
	if !ok {
		t.Fatalf("the audit row does not name the properties: %v", decoded)
	}
	if properties["rack"] != "B2" {
		t.Fatalf("the audit row does not record the edit: %v", properties)
	}
	if _, leaked := properties["asset_tag"]; leaked {
		t.Fatalf("the audit row repeated an internal property: %v", properties)
	}
}

// Staff record the properties. A key with no definition is refused before anything is written, so
// a request that mixes a good key and a bad one changes nothing.
func TestCustomPropertiesRequireADefinition(t *testing.T) {
	f := newFixture(t, 1200, "monthly")
	ctx := context.Background()
	defineProperties(t, f,
		service.PropertyDefinitionInput{Key: "rack", Label: "Rack", ShowOnInvoice: true},
		service.PropertyDefinitionInput{Key: "asset_tag", Label: "Asset tag", CustomerVisible: boolPtr(false)},
	)

	m, err := f.store.UpdateServiceMetadata(ctx, service.MetadataUpdate{
		ServiceID: f.serviceID, UserID: f.userID, Staff: true,
		CustomProperties: map[string]string{"rack": "B2", "asset_tag": "CT-0042"},
	})
	if err != nil {
		t.Fatalf("staff update: %v", err)
	}
	if m.CustomProperties["rack"] != "B2" || m.CustomProperties["asset_tag"] != "CT-0042" {
		t.Fatalf("stored properties = %v", m.CustomProperties)
	}
	if len(m.Properties) != 2 || m.Properties[0].Key != "asset_tag" && m.Properties[1].Key != "asset_tag" {
		t.Fatalf("properties = %+v", m.Properties)
	}

	// A property the operator marked customer-invisible is not in the customer's view.
	own, err := f.store.ServiceMetadataOwnedBy(ctx, f.serviceID, f.userID)
	if err != nil {
		t.Fatalf("customer read: %v", err)
	}
	if _, ok := own.CustomProperties["asset_tag"]; ok {
		t.Fatalf("the customer saw an internal property: %v", own.CustomProperties)
	}
	if own.CustomProperties["rack"] != "B2" {
		t.Fatalf("the customer did not see the visible property: %v", own.CustomProperties)
	}

	// An unknown key refuses the whole edit.
	_, err = f.store.UpdateServiceMetadata(ctx, service.MetadataUpdate{
		ServiceID: f.serviceID, UserID: f.userID, Staff: true,
		CustomProperties: map[string]string{"rack": "B3", "serial": "nope"},
	})
	if !errors.Is(err, service.ErrUnknownProperty) {
		t.Fatalf("unknown key: %v, want ErrUnknownProperty", err)
	}
	m, err = f.store.ServiceMetadata(ctx, f.serviceID)
	if err != nil {
		t.Fatalf("re-read: %v", err)
	}
	if m.CustomProperties["rack"] != "B2" {
		t.Fatalf("a refused edit wrote its good key anyway: %v", m.CustomProperties)
	}

	// An empty value unsets the property rather than storing an empty string.
	m, err = f.store.UpdateServiceMetadata(ctx, service.MetadataUpdate{
		ServiceID: f.serviceID, UserID: f.userID, Staff: true,
		CustomProperties: map[string]string{"rack": ""},
	})
	if err != nil {
		t.Fatalf("unset: %v", err)
	}
	if _, ok := m.CustomProperties["rack"]; ok {
		t.Fatalf("the property was not unset: %v", m.CustomProperties)
	}
}

// The database refuses a value whose key has no definition, so a hand-written UPDATE cannot create
// the orphan the API refuses.
func TestTheDatabaseRefusesAnUndefinedProperty(t *testing.T) {
	f := newFixture(t, 1200, "monthly")
	_, err := f.pool.Exec(context.Background(), `
		UPDATE services SET custom_properties = '{"serial":"nope"}'::jsonb WHERE id = $1`, f.serviceID)
	if err == nil {
		t.Fatal("the database accepted a property with no definition")
	}
	if !strings.Contains(err.Error(), "has no definition") {
		t.Fatalf("error = %v, want the trigger's refusal", err)
	}
}

// The database refuses a control character in a value: the sanitiser is the second line of
// defence, not the only one.
func TestTheDatabaseRefusesAControlCharacter(t *testing.T) {
	f := newFixture(t, 1200, "monthly")
	defineProperties(t, f, service.PropertyDefinitionInput{Key: "rack", Label: "Rack"})

	_, err := f.pool.Exec(context.Background(), `
		UPDATE services SET custom_properties = jsonb_build_object('rack', 'B' || chr(7) || '2')
		WHERE id = $1`, f.serviceID)
	if err == nil {
		t.Fatal("the database accepted a control character in a property value")
	}
	if !strings.Contains(err.Error(), "services_custom_properties_no_control") {
		t.Fatalf("error = %v, want the constraint", err)
	}
}

// A definition in use cannot be deleted: the value would be left with no label and no way to
// print it. Clearing the value first is the supported route.
func TestDefinitionInUseCannotBeDeleted(t *testing.T) {
	f := newFixture(t, 1200, "monthly")
	ctx := context.Background()
	defineProperties(t, f, service.PropertyDefinitionInput{Key: "rack", Label: "Rack"})

	if _, err := f.store.UpdateServiceMetadata(ctx, service.MetadataUpdate{
		ServiceID: f.serviceID, UserID: f.userID, Staff: true,
		CustomProperties: map[string]string{"rack": "B2"},
	}); err != nil {
		t.Fatalf("set: %v", err)
	}
	if err := f.store.DeletePropertyDefinition(ctx, "rack", f.userID); !errors.Is(err, service.ErrPropertyInUse) {
		t.Fatalf("delete while in use: %v, want ErrPropertyInUse", err)
	}

	if _, err := f.store.UpdateServiceMetadata(ctx, service.MetadataUpdate{
		ServiceID: f.serviceID, UserID: f.userID, Staff: true,
		CustomProperties: map[string]string{"rack": ""},
	}); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if err := f.store.DeletePropertyDefinition(ctx, "rack", f.userID); err != nil {
		t.Fatalf("delete after clearing: %v", err)
	}
	if definitions, err := f.store.PropertyDefinitions(ctx); err != nil || len(definitions) != 0 {
		t.Fatalf("definitions after delete = %+v, %v", definitions, err)
	}
}

// A definition's key is its identity, so it has to be a key: lowercase, boring, and safe as a JSON
// key and a form field name.
func TestPropertyDefinitionKeyIsValidated(t *testing.T) {
	f := newFixture(t, 1200, "monthly")
	for _, key := range []string{"Rack", "2rack", "rack-2", "", "rack tag"} {
		_, err := f.store.SavePropertyDefinition(context.Background(), service.PropertyDefinitionInput{
			Key: key, Label: "Rack",
		})
		if !errors.Is(err, service.ErrInvalidInput) {
			t.Fatalf("key %q: %v, want ErrInvalidInput", key, err)
		}
	}
	// A label is required: a property whose name is its key is not a name.
	if _, err := f.store.SavePropertyDefinition(context.Background(), service.PropertyDefinitionInput{
		Key: "rack",
	}); !errors.Is(err, service.ErrLabelInvalid) {
		t.Fatalf("missing label: %v, want ErrLabelInvalid", err)
	}
}

// SVC-08: the properties are printed on the invoice when the definition says so — and only then.
//
// Three properties are set, with three different combinations of the two flags. Exactly one of
// them reaches the document: `show_on_invoice` decides, and `customer_visible` is a different
// question with a different answer.
func TestInvoicePropertiesPrintOnlyWhatTheDefinitionAllows(t *testing.T) {
	f := newFixture(t, 1200, "monthly")
	ctx := context.Background()

	defineProperties(t, f,
		service.PropertyDefinitionInput{Key: "rack", Label: "Rack", ShowOnInvoice: true, SortOrder: 2},
		service.PropertyDefinitionInput{Key: "ip_address", Label: "IP address", ShowOnInvoice: true, SortOrder: 1},
		service.PropertyDefinitionInput{Key: "asset_tag", Label: "Asset tag", CustomerVisible: boolPtr(true)},
		service.PropertyDefinitionInput{Key: "internal_note", Label: "Internal note", CustomerVisible: boolPtr(false)},
	)
	if _, err := f.store.UpdateServiceMetadata(ctx, service.MetadataUpdate{
		ServiceID: f.serviceID, UserID: f.userID, Staff: true,
		CustomProperties: map[string]string{
			"rack":          "B2",
			"ip_address":    "203.0.113.7",
			"asset_tag":     "CT-0042",
			"internal_note": "watch this one",
		},
	}); err != nil {
		t.Fatalf("set: %v", err)
	}

	var invoiceID string
	if err := f.pool.QueryRow(ctx, `
		INSERT INTO invoices (customer_id, subscription_id, status, currency, subtotal_minor,
			total_minor, snapshot)
		VALUES ($1, $2, 'draft', 'USD', 1200, 1200, '{"schema_version":1}'::jsonb)
		RETURNING id`, f.customerID, f.subID).Scan(&invoiceID); err != nil {
		t.Fatalf("invoice: %v", err)
	}

	lines, err := f.store.InvoiceProperties(ctx, invoiceID)
	if err != nil {
		t.Fatalf("invoice properties: %v", err)
	}
	if len(lines) != 2 {
		t.Fatalf("lines = %+v, want the two show_on_invoice properties", lines)
	}
	// Ordered by the definition's sort order, not by insertion.
	if lines[0].Label != "IP address" || lines[0].Value != "203.0.113.7" {
		t.Fatalf("first line = %+v", lines[0])
	}
	if lines[1].Label != "Rack" || lines[1].Value != "B2" {
		t.Fatalf("second line = %+v", lines[1])
	}
	for _, line := range lines {
		if line.Label == "Asset tag" || line.Label == "Internal note" {
			t.Fatalf("%s reached the document without show_on_invoice", line.Label)
		}
	}

	// An invoice for a different subscription prints nothing, and does not error.
	var otherInvoice string
	if err := f.pool.QueryRow(ctx, `
		INSERT INTO invoices (customer_id, status, currency, subtotal_minor, total_minor, snapshot)
		VALUES ($1, 'draft', 'USD', 0, 0, '{"schema_version":1}'::jsonb) RETURNING id`,
		f.customerID).Scan(&otherInvoice); err != nil {
		t.Fatalf("other invoice: %v", err)
	}
	lines, err = f.store.InvoiceProperties(ctx, otherInvoice)
	if err != nil {
		t.Fatalf("unrelated invoice: %v", err)
	}
	if len(lines) != 0 {
		t.Fatalf("an unrelated invoice printed %+v", lines)
	}
}

// A property value is collapsed to one line on write, so what is stored is already what a
// document prints.
func TestPropertyValuesAreStoredAsOneLine(t *testing.T) {
	f := newFixture(t, 1200, "monthly")
	ctx := context.Background()
	defineProperties(t, f, service.PropertyDefinitionInput{Key: "rack", Label: "Rack", ShowOnInvoice: true})

	m, err := f.store.UpdateServiceMetadata(ctx, service.MetadataUpdate{
		ServiceID: f.serviceID, UserID: f.userID, Staff: true,
		CustomProperties: map[string]string{"rack": "  B2\n\trow 4  "},
	})
	if err != nil {
		t.Fatalf("set: %v", err)
	}
	if m.CustomProperties["rack"] != "B2 row 4" {
		t.Fatalf("stored value = %q, want one collapsed line", m.CustomProperties["rack"])
	}
}

// The sanitiser is what stands between an old row and a document: control and format characters
// go, whitespace collapses, and the value is capped.
func TestSanitizeDocumentText(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain", "Rack B2", "Rack B2"},
		{"newlines collapse", "Rack\n\nB2", "Rack B2"},
		{"leading and trailing space", "  Rack B2  ", "Rack B2"},
		{"control characters go", "Rack\x00\x07B2", "RackB2"},
		{"bidi override goes", "Rack\u202eB2", "RackB2"},
		{"zero width space goes", "Rack\u200bB2", "RackB2"},
		{"byte order mark goes", "\ufeffRack", "Rack"},
		{"capped", strings.Repeat("x", 200), strings.Repeat("x", service.MaxServiceLabel)},
	}
	for _, c := range cases {
		if got := service.SanitizeDocumentText(c.in); got != c.want {
			t.Fatalf("%s: SanitizeDocumentText(%q) = %q, want %q", c.name, c.in, got, c.want)
		}
	}
}

// SVC-07: the quantity and the order the row now carries are reported, and the derived label says
// how many of them there are.
func TestQuantityAndOrderAreReported(t *testing.T) {
	f := newFixture(t, 1200, "monthly")
	ctx := context.Background()

	var orderID string
	if err := f.pool.QueryRow(ctx, `
		INSERT INTO orders (customer_id, status, currency, total_minor)
		VALUES ($1, 'paid', 'USD', 3600) RETURNING id`,
		f.customerID).Scan(&orderID); err != nil {
		t.Fatalf("order: %v", err)
	}
	if _, err := f.pool.Exec(ctx, `
		UPDATE services SET quantity = 3, order_id = $2 WHERE id = $1`,
		f.serviceID, orderID); err != nil {
		t.Fatalf("update: %v", err)
	}

	m, err := f.store.ServiceMetadata(ctx, f.serviceID)
	if err != nil {
		t.Fatalf("metadata: %v", err)
	}
	if m.Quantity != 3 {
		t.Fatalf("quantity = %d, want 3", m.Quantity)
	}
	if m.OrderID == nil || *m.OrderID != orderID {
		t.Fatalf("order = %v, want %s", m.OrderID, orderID)
	}
	if m.EffectiveLabel != "VPS Small ×3" {
		t.Fatalf("effective label = %q, want the quantity in it", m.EffectiveLabel)
	}
}

// A label longer than the column allows is refused with a stable error rather than a 500 from the
// CHECK constraint.
func TestLabelAndDescriptionBounds(t *testing.T) {
	f := newFixture(t, 1200, "monthly")
	ctx := context.Background()

	long := strings.Repeat("x", service.MaxServiceLabel+1)
	if _, err := f.store.UpdateServiceMetadata(ctx, service.MetadataUpdate{
		ServiceID: f.serviceID, UserID: f.userID, Label: &long,
	}); !errors.Is(err, service.ErrLabelInvalid) {
		t.Fatalf("long label: %v, want ErrLabelInvalid", err)
	}

	longDescription := strings.Repeat("x", service.MaxServiceDescription+1)
	if _, err := f.store.UpdateServiceMetadata(ctx, service.MetadataUpdate{
		ServiceID: f.serviceID, UserID: f.userID, Description: &longDescription,
	}); !errors.Is(err, service.ErrDescriptionInvalid) {
		t.Fatalf("long description: %v, want ErrDescriptionInvalid", err)
	}

	// A description is a paragraph, so a newline is allowed; a bell is not.
	paragraph := "Line one\nLine two"
	if _, err := f.store.UpdateServiceMetadata(ctx, service.MetadataUpdate{
		ServiceID: f.serviceID, UserID: f.userID, Description: &paragraph,
	}); err != nil {
		t.Fatalf("paragraph: %v", err)
	}
	bell := "Line one\x07Line two"
	if _, err := f.store.UpdateServiceMetadata(ctx, service.MetadataUpdate{
		ServiceID: f.serviceID, UserID: f.userID, Description: &bell,
	}); !errors.Is(err, service.ErrDescriptionInvalid) {
		t.Fatalf("control character: %v, want ErrDescriptionInvalid", err)
	}
}

// Clearing the label restores the product's name rather than leaving an empty heading.
func TestClearingTheLabelRestoresTheProductName(t *testing.T) {
	f := newFixture(t, 1200, "monthly")
	ctx := context.Background()

	label := "web-01"
	if _, err := f.store.UpdateServiceMetadata(ctx, service.MetadataUpdate{
		ServiceID: f.serviceID, UserID: f.userID, Label: &label,
	}); err != nil {
		t.Fatalf("set: %v", err)
	}
	empty := ""
	m, err := f.store.UpdateServiceMetadata(ctx, service.MetadataUpdate{
		ServiceID: f.serviceID, UserID: f.userID, Label: &empty,
	})
	if err != nil {
		t.Fatalf("clear: %v", err)
	}
	if m.Label != "" || m.EffectiveLabel != "VPS Small" {
		t.Fatalf("after clearing: %q / %q", m.Label, m.EffectiveLabel)
	}
}
