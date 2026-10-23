package importer

// The extra WHMCS documents: currencies, admins, products and tickets (OPS-10).
//
// # What this file is honest about
//
// There is no WHMCS installation to test against in this repository and there is no sample dump, so
// the column contract here is *stated by this package* rather than discovered from a live schema.
// Two consequences, and they shape every rule below:
//
//   - **The canonical column names are this importer's own** (`code`, `email`, `name`, `price`,
//     `subject`, …) and the WHMCS table-derived spellings are accepted as aliases only where the
//     published WHMCS schema is unambiguous (tblcurrencies.code, tbladmins.email,
//     tblproducts.name, tbltickets.title). An export whose columns match neither is refused row by
//     row with `missing_column` or `invalid_value`, and the report says which column it wanted —
//     it never imports zero rows and reports success.
//   - **A row that cannot be read exactly is refused, not approximated.** The same rule the five
//     original entities follow. A product with no price is imported *as a product with no price*
//     and a warning, because inventing a price is how a catalogue silently starts billing the
//     wrong amount.
//
// # What is deliberately not imported, and why
//
//   - **Orders.** A WHMCS order is a header whose substance (items, price versions, the paid state)
//     lives in `tblhosting`/`tblinvoices`, which are already imported as services and invoices. An
//     order row imported without its items would show a customer an order they can "pay" that buys
//     nothing, and the invoice and service already carry the commitment. This is the same decision
//     the package header records for subscriptions.
//   - **Cancellations.** A WHMCS cancellation request is a pending intent against a service
//     (`tblcancelrequests.relid`). The services document already carries the *outcome*
//     (`domainstatus = Cancelled`), and the service rule imports it as a terminated service with
//     the `service_cancelled_imported_as_terminated` warning. Importing the request as well would
//     create a second, pending cancellation for a service that is already ended.
//   - **Config options.** A service's chosen options are carried inside the services document (as
//     `config_options`, parsed by the service rule). The product-level option definitions
//     (`tblproductconfigoptions` plus its values and pricing) cannot be mapped without guessing
//     which value priced which service, and a wrong mapping re-prices a live service on its next
//     renewal.
//   - **Ticket replies.** The ticket *header* is imported; the thread is a separate document whose
//     shape (author attribution, staff flag, attachments) has no fixture here. Every imported
//     ticket carries the `ticket_thread_not_imported` warning so an operator reads the subject and
//     knows to look in the old panel for the conversation.
//
// The four rules below are one shape: claim the natural key, resolve the parent through the
// mappings, parse exactly, then create/update/unchanged. They run inside the same single
// transaction as everything else, so a dry run reports exactly what a commit would do.

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/catalyst/billing/internal/money"
	"github.com/jackc/pgx/v5"
)

// The optional documents. They are optional because a dump from an old installation may not carry
// them, and "I forgot to export tickets" is not a reason to refuse the invoices: the report names
// the document it did not see, so the omission is visible rather than silent.
const (
	FileCurrencies = "currencies"
	FileAdmins     = "admins"
	FileProducts   = "products"
	FileTickets    = "tickets"
)

// OptionalFiles is the processing order of the documents a dump may carry.
//
// Products come before tickets because a ticket may name a service, and services are already in by
// the time these run (they are in the required set).
var OptionalFiles = []string{FileCurrencies, FileAdmins, FileProducts, FileTickets}

// The extra entities.
const (
	EntityCurrency = "currency"
	EntityAdmin    = "admin"
	EntityProduct  = "product"
	EntityTicket   = "ticket"
)

// Rejection codes this file adds.
const (
	CodeInvalidRole     = "invalid_role"
	CodeInvalidPriority = "invalid_priority"
)

// Warning codes this file adds.
const (
	// WarnAdminMustResetPassword is the one warning an imported administrator always gets: the old
	// panel's password hash cannot be reused, so the account exists and nobody can sign in to it
	// until an operator sets a password (`billing admin password-reset`).
	WarnAdminMustResetPassword = "admin_password_must_be_reset"
	// WarnProductWithoutPrice says the product row exists and cannot be sold yet.
	WarnProductWithoutPrice = "product_imported_without_price"
	// WarnTicketThreadNotImported says the conversation stayed behind.
	WarnTicketThreadNotImported = "ticket_thread_not_imported"
	// WarnCurrencyExistingNotChanged says an ISO currency this panel already knows was left as it
	// was: the panel's rendering of USD outranks the old installation's.
	WarnCurrencyExistingNotChanged = "currency_already_defined"
)

// runExtended executes the optional documents, in order, after the required ones.
func (r *run) runExtended() error {
	for _, file := range OptionalFiles {
		if _, ok := r.in.Files[file]; !ok {
			// Not exported. Recorded as a note on the report rather than an error, and only
			// for the documents this build can read.
			r.warn(file, 0, "", "document_not_supplied",
				file+" was not supplied; nothing of that kind was imported")
			continue
		}
		if err := r.file(file); err != nil {
			return err
		}
	}
	return nil
}

// dispatchExtended routes one optional-document row to its rules.
func (r *run) dispatchExtended(file string, row *csvRow) (bool, error) {
	switch file {
	case FileCurrencies:
		return true, r.applyCurrency(row)
	case FileAdmins:
		return true, r.applyAdmin(row)
	case FileProducts:
		return true, r.applyProduct(row)
	case FileTickets:
		return true, r.applyTicket(row)
	}
	return false, nil
}

// ------------------------------------------------------------------ currencies --

// applyCurrency imports one currency definition.
//
// Only the presentation of a currency the panel does not already know is imported. An ISO code
// this panel has a definition for (USD, EUR, …) is left as the panel defines it: the panel's scale
// is the correctness rule that makes money arithmetic exact, and an old panel's `format` column is
// a rendering preference. The row is reported as unchanged with a warning naming that decision.
func (r *run) applyCurrency(row *csvRow) error {
	const file = FileCurrencies
	const entity = EntityCurrency

	code := strings.ToUpper(strings.TrimSpace(row.get(file, "code")))
	if code == "" {
		r.reject(row, entity, "", CodeInvalidID, "the row has no currency code")
		return nil
	}
	if !validCurrencyCode(code) {
		r.reject(row, entity, code, CodeInvalidCurrency,
			fmt.Sprintf("currency code %s is not 2 to 12 upper-case letters or digits", sanitizeID(code)))
		return nil
	}
	if !r.claim(row, entity, code) {
		return nil
	}

	// An ISO currency this panel already prices in is not the dump's to redefine.
	if existing, err := r.knownCurrency(code); err != nil {
		return err
	} else if existing {
		r.warn(file, row.row, code, WarnCurrencyExistingNotChanged,
			code+" is already defined by this panel and was left as it is")
		r.written(entity, OutcomeUnchanged)
		return nil
	}

	name := strings.TrimSpace(row.get(file, "name"))
	if name == "" {
		name = code
	}
	symbol := strings.TrimSpace(row.get(file, "symbol"))
	position := strings.ToLower(strings.TrimSpace(row.get(file, "symbol_position")))
	switch position {
	case "prefix", "":
		position = "prefix"
	case "suffix":
	default:
		r.reject(row, entity, code, CodeInvalidValue,
			fmt.Sprintf("symbol_position %s is not prefix or suffix", sanitizeID(position)))
		return nil
	}

	// A currency with no scale cannot be rendered, and guessing one would make every amount in it
	// wrong by a factor of a hundred. The dump's own scale is required; money.MaxScale bounds it.
	scale, ok := r.smallInt(row, entity, code, file, "scale", 0, int(money.MaxScale), true)
	if !ok {
		return nil
	}

	enabled := true
	if raw := strings.TrimSpace(row.get(file, "enabled")); raw != "" {
		switch strings.ToLower(raw) {
		case "1", "true", "yes", "enabled":
			enabled = true
		case "0", "false", "no", "disabled":
			enabled = false
		default:
			r.reject(row, entity, code, CodeInvalidValue,
				fmt.Sprintf("enabled %s is not a boolean", sanitizeID(raw)))
			return nil
		}
	}

	if _, err := r.tx.Exec(r.ctx, `
		INSERT INTO currencies (code, name, scale, symbol, symbol_position, is_default, enabled)
		VALUES ($1, $2, $3, $4, $5, false, $6)
		ON CONFLICT (code) DO UPDATE
			SET name = EXCLUDED.name, symbol = EXCLUDED.symbol,
			    symbol_position = EXCLUDED.symbol_position, enabled = EXCLUDED.enabled`,
		code, truncateText(name, 80), scale, truncateText(symbol, 8), position, enabled); err != nil {
		return fmt.Errorf("importer: write currency %s: %w", code, err)
	}
	r.written(entity, OutcomeCreated)
	return nil
}

// knownCurrency reports whether this panel already defines the code.
func (r *run) knownCurrency(code string) (bool, error) {
	if _, ok := money.LookupCurrency(code); ok {
		return true, nil
	}
	var exists bool
	if err := r.tx.QueryRow(r.ctx,
		`SELECT true FROM currencies WHERE code = $1`, code).Scan(&exists); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("importer: check currency: %w", err)
	}
	return exists, nil
}

// ---------------------------------------------------------------------- admins --

// adminPasswordMarker is what an imported administrator's password hash is set to.
//
// It is not a hash — it is a value the verifier can never accept — because the old panel's hash
// cannot be reused and writing a *working* password is worse than writing none. The account exists,
// owns its history, and nobody can sign in until an operator runs
// `billing admin password-reset --email <address>`; the warning on the row says so.
const adminPasswordMarker = "!imported-no-password"

// applyAdmin imports one staff account.
func (r *run) applyAdmin(row *csvRow) error {
	const file = FileAdmins
	const entity = EntityAdmin

	legacy := row.get(file, "id")
	email := strings.ToLower(strings.TrimSpace(row.get(file, "email")))
	if email == "" {
		r.reject(row, entity, legacy, CodeInvalidEmail, "the row has no email address")
		return nil
	}
	if !validEmail(email) {
		r.reject(row, entity, legacy, CodeInvalidEmail,
			fmt.Sprintf("email %s is not an address", sanitizeID(email)))
		return nil
	}
	if legacy == "" {
		legacy = email
	}
	if !r.claim(row, entity, legacy) {
		return nil
	}

	name := strings.TrimSpace(row.get(file, "first_name") + " " + row.get(file, "last_name"))
	if name == "" {
		name = strings.TrimSpace(row.get(file, "name"))
	}

	// The role must be one this panel has. Unknown role → rejected: importing an administrator as
	// an ordinary user would silently remove their access, and importing as administrator would
	// silently grant it.
	role := strings.ToLower(strings.TrimSpace(row.get(file, "role")))
	switch role {
	case "administrator", "billing", "support", "infrastructure":
	case "":
		r.reject(row, entity, legacy, CodeInvalidRole,
			"the row has no role; this panel has administrator, billing, support and infrastructure")
		return nil
	default:
		r.reject(row, entity, legacy, CodeInvalidRole,
			fmt.Sprintf("role %s is not one of administrator, billing, support, infrastructure",
				sanitizeID(role)))
		return nil
	}

	if target, ok := r.mappings[mappingKey{entity, legacy}]; ok {
		// The account was imported earlier. Its role may have been changed on purpose since, so only
		// the name is refreshed and the row is reported unchanged.
		if _, err := r.tx.Exec(r.ctx, `UPDATE users SET display_name = $2 WHERE id = $1`, target, name); err != nil {
			return fmt.Errorf("importer: update admin: %w", err)
		}
		r.written(entity, OutcomeUnchanged)
		return nil
	}

	var existing string
	err := r.tx.QueryRow(r.ctx,
		`SELECT id::text FROM users WHERE lower(email) = $1`, email).Scan(&existing)
	switch {
	case err == nil:
		// An account with this address already exists (very often the operator's own). Adopt it,
		// grant the role if it does not hold one, and say nothing has been overwritten.
		if err := r.recordMapping(entity, legacy, existing); err != nil {
			return err
		}
		if err := r.grantRole(existing, role); err != nil {
			return err
		}
		r.warn(file, row.row, legacy, WarnAdminMustResetPassword,
			email+" already existed; its role was set to "+role+" and its password was left untouched")
		r.written(entity, OutcomeUnchanged)
		return nil
	case !errors.Is(err, pgx.ErrNoRows):
		return fmt.Errorf("importer: check admin: %w", err)
	}

	var userID string
	err = r.tx.QueryRow(r.ctx, `
		INSERT INTO users (email, password_hash, display_name, email_verified_at)
		VALUES ($1, $2, $3, now()) RETURNING id`, email, adminPasswordMarker, name).Scan(&userID)
	if err != nil {
		if isUniqueViolation(err) {
			r.reject(row, entity, legacy, CodeEmailTaken,
				fmt.Sprintf("email %s is already in use", sanitizeID(email)))
			return nil
		}
		return fmt.Errorf("importer: create admin: %w", err)
	}
	if err := r.grantRole(userID, role); err != nil {
		return err
	}
	if err := r.recordMapping(entity, legacy, userID); err != nil {
		return err
	}
	r.warn(file, row.row, legacy, WarnAdminMustResetPassword,
		"set a password with `billing admin password-reset --email "+email+"` before this person signs in")
	r.written(entity, OutcomeCreated)
	return nil
}

// grantRole attaches a staff role, doing nothing when the account already holds it.
func (r *run) grantRole(userID, role string) error {
	if _, err := r.tx.Exec(r.ctx, `
		INSERT INTO staff_roles (user_id, role) VALUES ($1, $2)
		ON CONFLICT (user_id) DO UPDATE SET role = EXCLUDED.role`, userID, role); err != nil {
		return fmt.Errorf("importer: grant role: %w", err)
	}
	return nil
}

// -------------------------------------------------------------------- products --

// applyProduct imports one catalogue product and, when the dump carries a price, one price version.
func (r *run) applyProduct(row *csvRow) error {
	const file = FileProducts
	const entity = EntityProduct

	legacy := row.get(file, "id")
	name := strings.TrimSpace(row.get(file, "name"))
	slug := strings.ToLower(strings.TrimSpace(row.get(file, "slug")))
	if slug == "" {
		// A slug is derived from the name only when the name yields one; a name that is all
		// punctuation is refused rather than given a random slug.
		slug = slugify(name)
	}
	if name == "" || slug == "" {
		r.reject(row, entity, legacy, CodeInvalidValue,
			"the row needs a name (and a slug, or a name a slug can be derived from)")
		return nil
	}
	if legacy == "" {
		legacy = slug
	}
	if !r.claim(row, entity, legacy) {
		return nil
	}

	var productID string
	err := r.tx.QueryRow(r.ctx,
		`SELECT id::text FROM products WHERE slug = $1`, slug).Scan(&productID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		if err := r.tx.QueryRow(r.ctx, `
			INSERT INTO products (slug, name) VALUES ($1, $2) RETURNING id::text`,
			slug, truncateText(name, 200)).Scan(&productID); err != nil {
			return fmt.Errorf("importer: create product: %w", err)
		}
		r.written(entity, OutcomeCreated)
	case err != nil:
		return fmt.Errorf("importer: read product: %w", err)
	default:
		r.written(entity, OutcomeUnchanged)
	}
	if target, ok := r.mappings[mappingKey{entity, legacy}]; !ok || target != productID {
		if err := r.recordMapping(entity, legacy, productID); err != nil {
			return err
		}
	}

	// The price is optional and is never invented. Without one the product exists in the catalogue
	// and cannot be sold until an operator prices it, which the warning says.
	raw := strings.TrimSpace(row.get(file, "price"))
	if raw == "" {
		r.warn(file, row.row, legacy, WarnProductWithoutPrice,
			name+" was imported without a price; it cannot be sold until one is added")
		return nil
	}
	cur, ok := r.currency(row, file)
	if !ok {
		return nil
	}
	amount, ok := r.amount(row, file, "price", cur)
	if !ok {
		return nil
	}
	interval := strings.ToLower(strings.TrimSpace(row.get(file, "interval")))
	switch interval {
	case "monthly", "quarterly", "semiannual", "annual":
	case "annually", "yearly":
		// The two spellings every other panel uses.
		interval = "annual"
	case "":
		interval = "monthly"
	default:
		r.reject(row, entity, legacy, CodeInvalidValue,
			fmt.Sprintf("interval %s is not monthly, quarterly, semiannual or annual", sanitizeID(interval)))
		return nil
	}

	// A price version is numbered, and the importer never rewrites one: a version that already
	// exists with the same amount is the same price, and one that differs would rewrite history.
	var (
		versionID string
		existing  int64
	)
	err = r.tx.QueryRow(r.ctx, `
		SELECT id::text, amount_minor FROM price_versions
		WHERE product_id = $1 AND currency = $2 AND interval = $3
		ORDER BY version DESC LIMIT 1`, productID, cur.Code(), interval).Scan(&versionID, &existing)
	switch {
	case err == nil:
		if existing == amount.Minor() {
			return nil
		}
		r.reject(row, entity, legacy, CodeConflict, fmt.Sprintf(
			"the product already has a %s price of %s; this panel never edits a price version in place",
			interval, moneyText(existing, cur)))
		return nil
	case !errors.Is(err, pgx.ErrNoRows):
		return fmt.Errorf("importer: read price version: %w", err)
	}

	if _, err := r.tx.Exec(r.ctx, `
		INSERT INTO price_versions
		    (product_id, version, currency, amount_minor, interval, anchor_day, anchor_timezone)
		VALUES ($1, 1, $2, $3, $4, 1, 'UTC')`,
		productID, cur.Code(), amount.Minor(), interval); err != nil {
		return fmt.Errorf("importer: create price version: %w", err)
	}
	return nil
}

// --------------------------------------------------------------------- tickets --

// applyTicket imports one ticket header.
//
// The thread is not imported (see the file header); every row carries a warning saying so. A ticket
// whose customer is unknown is refused with `unknown_client` rather than attached to nobody: a
// ticket with no customer is invisible to the person who wrote it.
func (r *run) applyTicket(row *csvRow) error {
	const file = FileTickets
	const entity = EntityTicket

	legacy := row.get(file, "id")
	if legacy == "" {
		r.reject(row, entity, "", CodeInvalidID, "the row has no ticket id")
		return nil
	}
	if !r.claim(row, entity, legacy) {
		return nil
	}

	clientLegacy := row.get(file, "client_id")
	customerID, ok := r.parent(row, entity, legacy, EntityCustomer, clientLegacy, CodeUnknownClient)
	if !ok {
		return nil
	}

	subject := strings.TrimSpace(row.get(file, "subject"))
	if subject == "" {
		r.reject(row, entity, legacy, CodeInvalidValue, "the ticket has no subject")
		return nil
	}

	status := normalizeTicketStatus(row.get(file, "status"))
	if status == "" {
		r.reject(row, entity, legacy, CodeUnknownStatus,
			fmt.Sprintf("status %s is not open, replied or closed", sanitizeID(row.get(file, "status"))))
		return nil
	}
	priority := strings.ToLower(strings.TrimSpace(row.get(file, "priority")))
	switch priority {
	case "":
		priority = "normal"
	case "low", "normal", "high":
	case "medium":
		// Every other panel spells the middle of the set "medium".
		priority = "normal"
	case "urgent":
		priority = "high"
	default:
		r.reject(row, entity, legacy, CodeInvalidPriority,
			fmt.Sprintf("priority %s is not low, normal or high", sanitizeID(priority)))
		return nil
	}
	department := strings.ToLower(strings.TrimSpace(row.get(file, "department")))
	if department == "" {
		department = "general"
	}
	created, ok := r.date(row, file, "created_at", false)
	if !ok {
		return nil
	}

	// The ticket number is the dump's, never renumbered: a customer quoting "ticket 41221" must find
	// the same ticket here. A number already used in this panel is a conflict.
	number := strings.TrimSpace(row.get(file, "number"))
	if number != "" {
		var taken bool
		if err := r.tx.QueryRow(r.ctx,
			`SELECT true FROM tickets WHERE number = $1`, number).Scan(&taken); err != nil &&
			!errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("importer: check ticket number: %w", err)
		} else if taken {
			r.reject(row, entity, legacy, CodeNumberTaken,
				fmt.Sprintf("ticket number %s already exists in this panel", sanitizeID(number)))
			return nil
		}
	}

	if target, ok := r.mappings[mappingKey{entity, legacy}]; ok {
		if _, err := r.tx.Exec(r.ctx, `
			UPDATE tickets SET subject = $2, priority = $3, status = $4, department = $5
			WHERE id = $1`, target, truncateText(subject, 200), priority, status, department); err != nil {
			return fmt.Errorf("importer: update ticket: %w", err)
		}
		r.warn(file, row.row, legacy, WarnTicketThreadNotImported,
			"the ticket was already imported; its conversation is in the old panel")
		r.written(entity, OutcomeUnchanged)
		return nil
	}

	var ticketID string
	if err := r.tx.QueryRow(r.ctx, `
		INSERT INTO tickets (number, customer_id, subject, department, priority, status,
		                     created_at, last_reply_at)
		VALUES (NULLIF($1, ''), $2, $3, $4, $5, $6, COALESCE($7, now()), COALESCE($7, now()))
		RETURNING id::text`,
		number, customerID, truncateText(subject, 200), truncateText(department, 40), priority,
		status, created).Scan(&ticketID); err != nil {
		return fmt.Errorf("importer: create ticket: %w", err)
	}
	if err := r.recordMapping(entity, legacy, ticketID); err != nil {
		return err
	}
	r.warn(file, row.row, legacy, WarnTicketThreadNotImported,
		"the ticket header was imported; its conversation stayed in the old panel")
	r.written(entity, OutcomeCreated)
	return nil
}

// normalizeTicketStatus maps a dump's status onto this panel's three.
func normalizeTicketStatus(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "open", "new", "customer-reply", "customer_reply", "in_progress", "answered":
		return "open"
	case "replied", "staff-reply", "staff_reply", "pending":
		return "replied"
	case "closed", "resolved", "solved":
		return "closed"
	}
	return ""
}

// -------------------------------------------------------------------- helpers --

// entityForFile names the entity an optional document produces.
//
// It is separate from entityFor in run.go because the required documents were the only ones that
// existed when that function was written; a rejection from an optional document still needs an
// entity name, since that is the key of the report's per-entity counters.
func entityForFile(file string) string {
	switch file {
	case FileCurrencies:
		return EntityCurrency
	case FileAdmins:
		return EntityAdmin
	case FileProducts:
		return EntityProduct
	case FileTickets:
		return EntityTicket
	}
	return file
}

// smallInt reads a bounded integer, rejecting a value outside the range.
//
// The bound is a schema rule stated once, in code, so a bad value is refused with the row number
// the operator needs rather than by a constraint name from the database.
func (r *run) smallInt(row *csvRow, entity, legacy, file, key string, low, high int, required bool) (int, bool) {
	raw := strings.TrimSpace(row.get(file, key))
	if raw == "" {
		if required {
			r.reject(row, entity, legacy, CodeInvalidValue, key+" is required")
			return 0, false
		}
		return 0, true
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < low || n > high {
		r.reject(row, entity, legacy, CodeInvalidValue,
			fmt.Sprintf("%s must be a whole number between %d and %d", key, low, high))
		return 0, false
	}
	return n, true
}

// validCurrencyCode mirrors the schema's check, so a code is refused here with a row number rather
// than by the database with a constraint name.
func validCurrencyCode(code string) bool {
	if len(code) < 2 || len(code) > 12 {
		return false
	}
	for _, r := range code {
		if r < 'A' || r > 'Z' {
			if r < '0' || r > '9' {
				return false
			}
		}
	}
	return true
}

// slugify derives a product slug from its name.
//
// It is deliberately conservative: lower-case ASCII letters and digits, joined by single hyphens.
// A name that yields nothing (all punctuation, or a script this rule does not transliterate) returns
// "" and the row is refused, because a wrong slug is a product nobody can link to.
func slugify(name string) string {
	var b strings.Builder
	lastHyphen := true
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastHyphen = false
		case r == ' ' || r == '-' || r == '_' || r == '.' || r == '/':
			if !lastHyphen {
				b.WriteRune('-')
				lastHyphen = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

// moneyText renders an amount for a rejection message.
func moneyText(minor int64, cur money.Currency) string {
	return money.New(minor, cur).Format()
}

// truncateText bounds a text column without splitting a rune.
func truncateText(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && (s[cut]&0xC0) == 0x80 {
		cut--
	}
	return s[:cut]
}

// isUniqueViolation reports whether err is a unique-index violation.
func isUniqueViolation(err error) bool {
	return strings.Contains(err.Error(), "SQLSTATE 23505")
}
