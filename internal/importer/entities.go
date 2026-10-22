package importer

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/catalyst/billing/internal/money"
	"github.com/jackc/pgx/v5"
)

// The entity rules.
//
// Each rule reads one row, decides what it becomes, and records the outcome. The shape is
// the same for all five on purpose:
//
//  1. the natural key, claimed for this run (so a duplicate inside the dump is one
//     rejection rather than a failed transaction);
//  2. the parent, resolved through the mappings (so an invoice whose client was refused
//     says `parent_rejected` instead of `unknown_client`);
//  3. the values, parsed exactly (a number that would need rounding is refused);
//  4. the existing mapping, if any, which decides create versus update versus unchanged;
//  5. the write, followed by the mapping that makes the next run a no-op.
//
// Nothing here reads a value it will not use, and nothing here computes a number the dump
// did not state — except the two documented derivations: an invoice's subtotal when the
// dump only carries tax and total (exact subtraction, no rounding), and an invoice's
// amount_paid when its status is paid (the status *is* the statement that it was paid in
// full).

// ---------------------------------------------------------------- customers --

// applyClient imports one client row.
func (r *run) applyClient(row *csvRow) error {
	const file = FileClients
	const entity = EntityCustomer

	legacy := row.get(file, "id")
	if legacy == "" {
		r.reject(row, entity, "", CodeInvalidID, "the row has no client id")
		return nil
	}
	if !r.claim(row, entity, legacy) {
		return nil
	}

	email := strings.ToLower(strings.TrimSpace(row.get(file, "email")))
	if !validEmail(email) {
		r.reject(row, entity, legacy, CodeInvalidEmail,
			fmt.Sprintf("email %s is not an address", sanitizeID(email)))
		return nil
	}
	name := clientName(row)
	created, ok := r.date(row, file, "created_at", false)
	if !ok {
		return nil
	}

	target, outcome, err := r.customerTarget(row, legacy, email, name, created)
	if err != nil || target == "" {
		return err
	}
	r.written(entity, outcome)
	return nil
}

// clientName joins the parts the dump offers. An empty name stays empty: the panel shows the
// email instead of a name it invented.
func clientName(row *csvRow) string {
	first := row.get(FileClients, "first_name")
	last := row.get(FileClients, "last_name")
	if name := strings.TrimSpace(first + " " + last); name != "" {
		return name
	}
	if company := row.get(FileClients, "company"); company != "" {
		return company
	}
	return ""
}

// customerTarget resolves what a client row becomes: the row already mapped to this legacy
// id, an existing customer adopted by email, or a new customer.
func (r *run) customerTarget(row *csvRow, legacy, email, name string, created *time.Time) (string, string, error) {
	const entity = EntityCustomer
	key := mappingKey{entity, legacy}

	if target, ok := r.mappings[key]; ok {
		var (
			curEmail string
			curName  string
		)
		err := r.tx.QueryRow(r.ctx,
			`SELECT lower(email), name FROM customers WHERE id = $1`, target).Scan(&curEmail, &curName)
		if errors.Is(err, pgx.ErrNoRows) {
			r.reject(row, entity, legacy, CodeConflict, fmt.Sprintf(
				"legacy id %s was imported earlier, but the customer it points at no longer exists",
				sanitizeID(legacy)))
			return "", "", nil
		}
		if err != nil {
			return "", "", fmt.Errorf("importer: read customer: %w", err)
		}
		if curEmail == email && curName == name {
			return target, OutcomeUnchanged, nil
		}
		if curEmail != email {
			var other string
			err := r.tx.QueryRow(r.ctx,
				`SELECT id::text FROM customers WHERE lower(email) = $1 AND id <> $2`,
				email, target).Scan(&other)
			switch {
			case err == nil:
				r.reject(row, entity, legacy, CodeEmailTaken, fmt.Sprintf(
					"email %s already belongs to another customer", sanitizeID(email)))
				return "", "", nil
			case !errors.Is(err, pgx.ErrNoRows):
				return "", "", fmt.Errorf("importer: check email: %w", err)
			}
		}
		if _, err := r.tx.Exec(r.ctx,
			`UPDATE customers SET email = $2, name = $3 WHERE id = $1`, target, email, name); err != nil {
			return "", "", fmt.Errorf("importer: update customer: %w", err)
		}
		return target, OutcomeUpdated, nil
	}

	// No mapping. A customer with this email may already exist — created by hand, or by an
	// earlier dump of the same source under a different legacy id. Adopting it is the
	// behaviour an operator expects ("the customer is already here"), and it is reported as
	// updated-or-unchanged rather than as a new customer.
	var existing string
	err := r.tx.QueryRow(r.ctx,
		`SELECT id::text FROM customers WHERE lower(email) = $1`, email).Scan(&existing)
	switch {
	case err == nil:
		if other, ok := r.byTarget[mappingKey{entity, existing}]; ok && other != legacy {
			r.reject(row, entity, legacy, CodeEmailTaken, fmt.Sprintf(
				"email %s already belongs to the customer imported as legacy id %s",
				sanitizeID(email), sanitizeID(other)))
			return "", "", nil
		}
		var curName string
		if err := r.tx.QueryRow(r.ctx, `SELECT name FROM customers WHERE id = $1`, existing).Scan(&curName); err != nil {
			return "", "", fmt.Errorf("importer: read customer name: %w", err)
		}
		outcome := OutcomeUnchanged
		if curName != name {
			if _, err := r.tx.Exec(r.ctx,
				`UPDATE customers SET name = $2 WHERE id = $1`, existing, name); err != nil {
				return "", "", fmt.Errorf("importer: update customer: %w", err)
			}
			outcome = OutcomeUpdated
		}
		if err := r.recordMapping(entity, legacy, existing); err != nil {
			return "", "", err
		}
		r.warn(row.t.file, row.row, legacy, WarnCustomerMatched, fmt.Sprintf(
			"client %s matched the existing customer %s by email; the legacy id is now linked "+
				"to that customer rather than creating a second one",
			sanitizeID(legacy), sanitizeID(email)))
		return existing, outcome, nil
	case errors.Is(err, pgx.ErrNoRows):
		var id string
		if err := r.tx.QueryRow(r.ctx, `
			INSERT INTO customers (email, name, created_at)
			VALUES ($1, $2, COALESCE($3::timestamptz, now()))
			RETURNING id::text`, email, name, created).Scan(&id); err != nil {
			return "", "", fmt.Errorf("importer: create customer: %w", err)
		}
		if err := r.recordMapping(entity, legacy, id); err != nil {
			return "", "", err
		}
		return id, OutcomeCreated, nil
	default:
		return "", "", fmt.Errorf("importer: look up customer: %w", err)
	}
}

// ----------------------------------------------------------------- invoices --

// applyInvoice imports one invoice row.
func (r *run) applyInvoice(row *csvRow) error {
	const file = FileInvoices
	const entity = EntityInvoice

	legacy := row.get(file, "id")
	if legacy == "" {
		r.reject(row, entity, "", CodeInvalidID, "the row has no invoice id")
		return nil
	}
	if !r.claim(row, entity, legacy) {
		return nil
	}

	// Rule 11: an imported invoice carries its historical number or it is refused. A
	// renumbered invoice is a different document to the customer holding a PDF of the old one.
	number := strings.TrimSpace(row.get(file, "number"))
	if number == "" {
		r.reject(row, entity, legacy, CodeMissingNumber,
			"the invoice has no historical number; the importer will not invent one")
		return nil
	}

	clientLegacy := row.get(file, "client_id")
	customerID, ok := r.parent(row, entity, legacy, EntityCustomer, clientLegacy, CodeUnknownClient)
	if !ok {
		return nil
	}

	status, ok := invoiceStatus(row.get(file, "status"))
	if !ok {
		r.reject(row, entity, legacy, CodeUnknownStatus, fmt.Sprintf(
			"status %s is not one this importer can represent", sanitizeID(row.get(file, "status"))))
		return nil
	}
	if status == "draft" {
		// A draft has no number in this panel's schema, so importing one would mean either
		// inventing a number or dropping the historical one. Neither is acceptable; the
		// operator re-creates a draft by hand, which is a handful of rows.
		r.reject(row, entity, legacy, CodeDraftInvoice,
			"a draft invoice carries no number in this panel, so it cannot be imported; "+
				"re-create it in the panel instead")
		return nil
	}

	cur, ok := r.currency(row, file)
	if !ok {
		return nil
	}
	amounts, ok := r.invoiceAmounts(row, cur)
	if !ok {
		return nil
	}

	issuedAt, ok := r.date(row, file, "issued_at", true)
	if !ok {
		return nil
	}
	dueAt, ok := r.date(row, file, "due_at", false)
	if !ok {
		return nil
	}

	if dup, ok := r.numbers[number]; ok {
		r.reject(row, entity, legacy, CodeDuplicateNumber, fmt.Sprintf(
			"invoice number %s is already claimed by legacy invoice %s in this dump",
			sanitizeID(number), sanitizeID(dup)))
		return nil
	}

	key := mappingKey{entity, legacy}
	if target, ok := r.mappings[key]; ok {
		outcome, err := r.existingInvoice(row, legacy, target, number, status, cur, amounts)
		if err != nil || outcome == "" {
			return err
		}
		r.numbers[number] = legacy
		r.trackInvoice(legacy, row, target, amounts, cur, false, invoiceIssue{})
		r.written(entity, outcome)
		return nil
	}

	// The number is unique in the schema, so a collision here is a real conflict with an
	// invoice this panel issued or imported earlier. Renumbering would be the one thing the
	// importer promises never to do.
	var taken bool
	err := r.tx.QueryRow(r.ctx, `SELECT true FROM invoices WHERE number = $1`, number).Scan(&taken)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("importer: check invoice number: %w", err)
	}
	if taken {
		r.reject(row, entity, legacy, CodeNumberTaken, fmt.Sprintf(
			"invoice number %s is already used by another invoice in this panel; "+
				"the row is refused rather than renumbered", sanitizeID(number)))
		return nil
	}

	snapshot, err := json.Marshal(map[string]any{
		"schema_version": 1,
		"kind":           "import",
		"source":         r.in.Source,
		"legacy_id":      legacy,
		"imported_at":    r.in.Now.UTC().Format("2006-01-02T15:04:05Z07:00"),
	})
	if err != nil {
		return fmt.Errorf("importer: encode snapshot: %w", err)
	}

	// The invoice is written as a draft and issued after its lines are in, because the
	// database refuses to add a line to an issued invoice. See run.issueInvoices.
	var id string
	if err := r.tx.QueryRow(r.ctx, `
		INSERT INTO invoices (customer_id, status, currency, subtotal_minor, tax_minor,
			total_minor, amount_paid_minor, due_at)
		VALUES ($1, 'draft', $2, $3, $4, $5, $6, $7)
		RETURNING id::text`,
		customerID, cur.Code(),
		amounts.subtotal.Minor(), amounts.tax.Minor(), amounts.total.Minor(), amounts.paid.Minor(),
		dueAt,
	).Scan(&id); err != nil {
		return fmt.Errorf("importer: create invoice: %w", err)
	}
	if err := r.recordMapping(entity, legacy, id); err != nil {
		return err
	}
	r.numbers[number] = legacy
	r.reserveSeriesNumber(number)
	r.trackInvoice(legacy, row, id, amounts, cur, true, invoiceIssue{
		number: number, status: status, issuedAt: issuedAt, dueAt: dueAt, snapshot: snapshot,
	})
	r.written(entity, OutcomeCreated)
	return nil
}

// invoiceIssue is what turns the draft into the document the dump describes.
type invoiceIssue struct {
	number   string
	status   string
	issuedAt *time.Time
	dueAt    *time.Time
	snapshot []byte
}

// invoiceAmounts reads subtotal, tax, total and amount_paid, and enforces the arithmetic the
// schema enforces so the failure is a rejection with a row number rather than a database
// error.
func (r *run) invoiceAmounts(row *csvRow, cur money.Currency) (invoiceAmounts, bool) {
	const file = FileInvoices
	const entity = EntityInvoice
	legacy := row.get(file, "id")

	total, ok := r.amount(row, file, "total", cur)
	if !ok {
		return invoiceAmounts{}, false
	}
	tax, ok := r.optionalAmount(row, file, "tax", cur)
	if !ok {
		return invoiceAmounts{}, false
	}
	subtotal, ok := r.optionalAmount(row, file, "subtotal", cur)
	if !ok {
		return invoiceAmounts{}, false
	}
	if row.get(file, "subtotal") == "" {
		// Exact subtraction, not a rounding: the schema requires total = subtotal + tax, and
		// the dump stated two of the three. A negative result means the dump disagrees with
		// itself and is refused below.
		subtotal, _ = total.Sub(tax)
	}

	if total.IsNegative() || tax.IsNegative() || subtotal.IsNegative() {
		r.reject(row, entity, legacy, CodeNegativeAmount,
			"the invoice has a negative subtotal, tax or total, which this panel's schema cannot store")
		return invoiceAmounts{}, false
	}
	sum, _ := subtotal.Add(tax)
	if !sum.Equal(total) {
		r.reject(row, entity, legacy, CodeTotalMismatch, fmt.Sprintf(
			"subtotal %d plus tax %d is %d %s, but the invoice says %d; the row is refused rather "+
				"than adjusted", subtotal.Minor(), tax.Minor(), sum.Minor(), cur.Code(), total.Minor()))
		return invoiceAmounts{}, false
	}

	paid, ok := r.optionalAmount(row, file, "amount_paid", cur)
	if !ok {
		return invoiceAmounts{}, false
	}
	status, _ := invoiceStatus(row.get(file, "status"))
	if row.get(file, "amount_paid") == "" && (status == "paid" || status == "refunded") {
		// "Paid" is the dump's own statement that the invoice was settled in full. The amount
		// is the invoice total, which the dump also states: no new number is introduced.
		paid = total
	}
	if paid.Minor() > total.Minor() {
		r.reject(row, entity, legacy, CodeOverpayment, fmt.Sprintf(
			"amount paid %d %s exceeds the invoice total %d", paid.Minor(), cur.Code(), total.Minor()))
		return invoiceAmounts{}, false
	}
	if paid.IsNegative() {
		r.reject(row, entity, legacy, CodeNegativeAmount, "amount paid is negative")
		return invoiceAmounts{}, false
	}
	return invoiceAmounts{subtotal: subtotal, tax: tax, total: total, paid: paid}, true
}

// invoiceAmounts is the money one invoice row carries.
type invoiceAmounts struct {
	subtotal money.Amount
	tax      money.Amount
	total    money.Amount
	paid     money.Amount
}

// existingInvoice compares an already-imported invoice with the row being replayed.
//
// An issued invoice is immutable in the database, so "the dump changed" cannot be applied:
// the honest answer is a rejection naming the difference, not a silent no-op that hides a
// divergence between the two systems.
func (r *run) existingInvoice(row *csvRow, legacy, target, number, status string,
	cur money.Currency, amounts invoiceAmounts) (string, error) {
	const entity = EntityInvoice

	var (
		curNumber, curStatus, curCurrency      string
		curSubtotal, curTax, curTotal, curPaid int64
	)
	err := r.tx.QueryRow(r.ctx, `
		SELECT COALESCE(number, ''), status, currency, subtotal_minor, tax_minor, total_minor,
		       amount_paid_minor
		FROM invoices WHERE id = $1`, target).
		Scan(&curNumber, &curStatus, &curCurrency, &curSubtotal, &curTax, &curTotal, &curPaid)
	if errors.Is(err, pgx.ErrNoRows) {
		r.reject(row, entity, legacy, CodeConflict, fmt.Sprintf(
			"legacy id %s was imported earlier, but the invoice it points at no longer exists",
			sanitizeID(legacy)))
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("importer: read invoice: %w", err)
	}
	switch {
	case curNumber != number:
		r.reject(row, entity, legacy, CodeInvoiceImmutable, fmt.Sprintf(
			"the invoice was imported as number %s and the dump now says %s; an issued invoice "+
				"is immutable, so the row is refused", sanitizeID(curNumber), sanitizeID(number)))
		return "", nil
	case curStatus != status:
		r.reject(row, entity, legacy, CodeInvoiceImmutable, fmt.Sprintf(
			"the invoice was imported as %s and the dump now says %s", curStatus, status))
		return "", nil
	case curCurrency != cur.Code(), curSubtotal != amounts.subtotal.Minor(),
		curTax != amounts.tax.Minor(), curTotal != amounts.total.Minor(),
		curPaid != amounts.paid.Minor():
		r.reject(row, entity, legacy, CodeInvoiceImmutable, fmt.Sprintf(
			"the invoice was imported as %d %s (%d + %d tax, %d paid) and the dump now says "+
				"%d %s (%d + %d tax, %d paid); an issued invoice is immutable, so the row is refused",
			curTotal, curCurrency, curSubtotal, curTax, curPaid,
			amounts.total.Minor(), cur.Code(), amounts.subtotal.Minor(), amounts.tax.Minor(),
			amounts.paid.Minor()))
		return "", nil
	}
	return OutcomeUnchanged, nil
}

// trackInvoice records what the issue step and the reconciliation need to know about an
// invoice row.
func (r *run) trackInvoice(legacy string, row *csvRow, target string, amounts invoiceAmounts,
	cur money.Currency, created bool, issue invoiceIssue) {
	st := r.invoices[legacy]
	if st == nil {
		st = &invoiceState{legacyID: legacy, file: row.t.file, row: row.row}
		r.invoices[legacy] = st
	}
	st.targetID = target
	st.paid = amounts.paid.Minor()
	st.total = amounts.total.Minor()
	st.subtotal = amounts.subtotal.Minor()
	st.tax = amounts.tax.Minor()
	st.currency = cur.Code()
	st.created = created
	if created {
		st.number = issue.number
		st.status = issue.status
		st.issuedAt = issue.issuedAt
		st.dueAt = issue.dueAt
		st.snapshot = issue.snapshot
	}
	if amounts.tax.Minor() > 0 {
		r.taxedInvoices++
	}
}

// -------------------------------------------------------------- invoice lines --

// applyInvoiceItem imports one invoice line.
func (r *run) applyInvoiceItem(row *csvRow) error {
	const file = FileInvoiceItems
	const entity = EntityInvoiceLine

	legacy := row.get(file, "id")
	if legacy == "" {
		r.reject(row, entity, "", CodeInvalidID, "the row has no invoice item id")
		return nil
	}
	if !r.claim(row, entity, legacy) {
		return nil
	}

	invoiceLegacy := row.get(file, "invoice_id")
	invoiceID, ok := r.parent(row, entity, legacy, EntityInvoice, invoiceLegacy, CodeUnknownInvoice)
	if !ok {
		return nil
	}

	description := row.get(file, "description")
	if description == "" {
		r.reject(row, entity, legacy, CodeInvalidValue, "the line has no description")
		return nil
	}

	cur, err := r.invoiceCurrency(invoiceLegacy, invoiceID)
	if err != nil {
		return err
	}
	net, ok := r.amount(row, file, "amount", cur)
	if !ok {
		return nil
	}
	if net.IsNegative() {
		// A discount or a correction is a negative amount the schema cannot store as a line.
		// Refusing is the honest answer: dropping the row would make the invoice's lines add
		// up to more than the invoice.
		r.reject(row, entity, legacy, CodeNegativeAmount, fmt.Sprintf(
			"line amount %d is negative; this panel stores discounts as their own line type, "+
				"and the importer will not turn a negative charge into a positive one", net.Minor()))
		return nil
	}

	if target, ok := r.mappings[mappingKey{entity, legacy}]; ok {
		var (
			curDescription string
			curNet         int64
		)
		err := r.tx.QueryRow(r.ctx,
			`SELECT description, net_minor FROM invoice_lines WHERE id = $1`, target).
			Scan(&curDescription, &curNet)
		if errors.Is(err, pgx.ErrNoRows) {
			r.reject(row, entity, legacy, CodeConflict, fmt.Sprintf(
				"legacy id %s was imported earlier, but the line it points at no longer exists",
				sanitizeID(legacy)))
			return nil
		}
		if err != nil {
			return fmt.Errorf("importer: read invoice line: %w", err)
		}
		if curDescription != description || curNet != net.Minor() {
			r.reject(row, entity, legacy, CodeInvoiceImmutable, fmt.Sprintf(
				"the line belongs to an issued invoice and was imported as %s (%d); the dump now "+
					"says %s (%d)", sanitizeID(curDescription), curNet,
				sanitizeID(description), net.Minor()))
			return nil
		}
		r.noteLine(invoiceLegacy, net.Minor())
		r.written(entity, OutcomeUnchanged)
		return nil
	}

	// A line can only be written while its invoice is still a draft, which is the case only for
	// an invoice this run created. Adding a line to an invoice imported earlier would be a
	// rewrite of an issued document, and the database refuses it — so the row is refused here,
	// with a reason, instead of aborting the transaction.
	if st := r.invoices[invoiceLegacy]; st == nil || !st.created {
		r.reject(row, entity, legacy, CodeInvoiceImmutable, fmt.Sprintf(
			"invoice %s is already issued and its lines are frozen; a line can only be imported "+
				"together with the invoice it belongs to", sanitizeID(invoiceLegacy)))
		return nil
	}

	// Position is the next free one for this invoice, which keeps the reprint in the order the
	// old panel printed. MAX() inside the transaction sees the lines this run has written.
	var position int
	if err := r.tx.QueryRow(r.ctx,
		`SELECT COALESCE(MAX(position), 0) + 1 FROM invoice_lines WHERE invoice_id = $1`,
		invoiceID).Scan(&position); err != nil {
		return fmt.Errorf("importer: next line position: %w", err)
	}
	var id string
	if err := r.tx.QueryRow(r.ctx, `
		INSERT INTO invoice_lines (invoice_id, position, description, quantity, unit_minor,
			net_minor, tax_rate, tax_minor, total_minor)
		VALUES ($1, $2, $3, 1, $4, $4, '0', 0, $4)
		RETURNING id::text`,
		invoiceID, position, description, net.Minor()).Scan(&id); err != nil {
		return fmt.Errorf("importer: create invoice line: %w", err)
	}
	if err := r.recordMapping(entity, legacy, id); err != nil {
		return err
	}
	r.noteLine(invoiceLegacy, net.Minor())
	r.written(entity, OutcomeCreated)
	return nil
}

// noteLine adds a line to its invoice's reconciliation total.
func (r *run) noteLine(invoiceLegacy string, net int64) {
	r.lines++
	if st := r.invoices[invoiceLegacy]; st != nil {
		st.lineTotal += net
		st.hasLines = true
	}
}

// invoiceCurrency resolves the currency of an invoice the line belongs to: from this run's
// state when the invoice was imported here, and from the database otherwise.
func (r *run) invoiceCurrency(invoiceLegacy, invoiceID string) (money.Currency, error) {
	if st := r.invoices[invoiceLegacy]; st != nil && st.currency != "" {
		return currencyFor(st.currency)
	}
	var code string
	if err := r.tx.QueryRow(r.ctx, `SELECT currency FROM invoices WHERE id = $1`, invoiceID).Scan(&code); err != nil {
		return money.Currency{}, fmt.Errorf("importer: read invoice currency: %w", err)
	}
	return currencyFor(code)
}

// ---------------------------------------------------------------- services --

// applyService imports one service.
func (r *run) applyService(row *csvRow) error {
	const file = FileServices
	const entity = EntityService

	legacy := row.get(file, "id")
	if legacy == "" {
		r.reject(row, entity, "", CodeInvalidID, "the row has no service id")
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

	slug := row.get(file, "product")
	if slug == "" {
		r.reject(row, entity, legacy, CodeUnknownProduct, "the row names no product")
		return nil
	}
	productID, ok, err := r.product(row, slug)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}

	status, cancelled, ok := serviceStatus(row.get(file, "status"))
	if !ok {
		r.reject(row, entity, legacy, CodeUnknownStatus, fmt.Sprintf(
			"status %s is not one this importer can represent", sanitizeID(row.get(file, "status"))))
		return nil
	}
	if cancelled {
		r.warn(file, row.row, legacy, WarnServiceCancelled, fmt.Sprintf(
			"service %s is marked cancelled in the dump and is imported as terminated; the old "+
				"panel's cancellation request is not reconstructed", sanitizeID(legacy)))
	}

	externalID := row.get(file, "external_id")
	config, err := r.serviceConfig(row, legacy)
	if err != nil {
		return err
	}
	createdAt, ok := r.date(row, file, "created_at", false)
	if !ok {
		return nil
	}

	key := mappingKey{entity, legacy}
	if target, ok := r.mappings[key]; ok {
		var (
			curStatus   string
			curExternal *string
			curConfig   []byte
		)
		err := r.tx.QueryRow(r.ctx,
			`SELECT status, external_id, config FROM services WHERE id = $1`, target).
			Scan(&curStatus, &curExternal, &curConfig)
		if errors.Is(err, pgx.ErrNoRows) {
			r.reject(row, entity, legacy, CodeConflict, fmt.Sprintf(
				"legacy id %s was imported earlier, but the service it points at no longer exists",
				sanitizeID(legacy)))
			return nil
		}
		if err != nil {
			return fmt.Errorf("importer: read service: %w", err)
		}
		unchanged := curStatus == status &&
			derefString(curExternal) == externalID &&
			sameJSON(curConfig, config)
		if unchanged {
			r.services++
			r.written(entity, OutcomeUnchanged)
			return nil
		}
		if _, err := r.tx.Exec(r.ctx, `
			UPDATE services SET status = $2, external_id = $3, config = $4, updated_at = now()
			WHERE id = $1`, target, status, nullableText(externalID), config); err != nil {
			return fmt.Errorf("importer: update service: %w", err)
		}
		r.services++
		r.written(entity, OutcomeUpdated)
		return nil
	}

	var id string
	if err := r.tx.QueryRow(r.ctx, `
		INSERT INTO services (customer_id, product_id, status, external_id, config, created_at,
			updated_at)
		VALUES ($1, $2, $3, $4, $5, COALESCE($6::timestamptz, now()), now())
		RETURNING id::text`,
		customerID, productID, status, nullableText(externalID), config, createdAt,
	).Scan(&id); err != nil {
		return fmt.Errorf("importer: create service: %w", err)
	}
	if err := r.recordMapping(entity, legacy, id); err != nil {
		return err
	}
	r.services++
	r.written(entity, OutcomeCreated)
	return nil
}

// serviceConfig keeps the facts a service row carries that this schema has no column for.
//
// A billing cycle and a next-due date are not decoration: they are the old panel's billing
// arrangement, and throwing them away is what makes an import unreviewable. They are stored
// under a namespaced key so nothing reads them as panel configuration by accident.
func (r *run) serviceConfig(row *csvRow, legacy string) ([]byte, error) {
	imported := map[string]any{
		"source":     r.in.Source,
		"service_id": legacy,
	}
	if cycle := row.get(FileServices, "billing_cycle"); cycle != "" {
		imported["billing_cycle"] = cycle
	}
	if due := row.get(FileServices, "next_due_at"); due != "" {
		imported["next_due_at"] = due
	}
	config, err := json.Marshal(map[string]any{"imported": imported})
	if err != nil {
		return nil, fmt.Errorf("importer: encode service config: %w", err)
	}
	return config, nil
}

// -------------------------------------------------------------- transactions --

// applyTransaction imports one payment.
func (r *run) applyTransaction(row *csvRow) error {
	const file = FileTransactions
	const entity = EntityPayment

	legacy := row.get(file, "id")
	if legacy == "" {
		r.reject(row, entity, "", CodeInvalidID, "the row has no transaction id")
		return nil
	}
	if !r.claim(row, entity, legacy) {
		return nil
	}

	providerPaymentID := row.get(file, "transaction_id")
	if providerPaymentID == "" {
		// House rule 3: ingestion is idempotent on the provider payment id. A payment without
		// one cannot be made idempotent, so it cannot be imported at all.
		r.reject(row, entity, legacy, CodeInvalidValue,
			"the transaction has no provider payment id; a payment without one cannot be ingested twice safely")
		return nil
	}
	gateway := normalizeGateway(row.get(file, "gateway"))
	if gateway == "" {
		r.reject(row, entity, legacy, CodeInvalidValue, "the transaction has no gateway")
		return nil
	}
	cur, ok := r.currency(row, file)
	if !ok {
		return nil
	}
	amount, ok := r.amount(row, file, "amount", cur)
	if !ok {
		return nil
	}
	if amount.IsNegative() || amount.IsZero() {
		// A refund or a fee adjustment is a negative row in a WHMCS export. It is not a
		// payment, and the panel's payments table has no place for it; the row is refused so
		// the operator can handle it deliberately rather than silently losing it.
		r.reject(row, entity, legacy, CodeNonPositivePayment, fmt.Sprintf(
			"amount %d is not a positive payment; refunds and adjustments are not imported",
			amount.Minor()))
		return nil
	}
	receivedAt, ok := r.date(row, file, "received_at", true)
	if !ok {
		return nil
	}

	// Resolve the invoice before writing anything: a row whose link cannot be honoured must
	// leave no payment behind.
	invoiceLegacy := row.get(file, "invoice_id")
	var invoiceID string
	if invoiceLegacy != "" {
		invoiceID, ok = r.parent(row, entity, legacy, EntityInvoice, invoiceLegacy, CodeUnknownInvoice)
		if !ok {
			return nil
		}
	}

	// The room an invoice has left is checked before anything is written: a payment that would
	// take its invoice past its total is a refused row, and a refused row leaves no payment
	// behind.
	if invoiceID != "" {
		room, err := r.allocationRoom(invoiceLegacy, invoiceID)
		if err != nil {
			return err
		}
		if amount.Minor() > room {
			r.reject(row, entity, legacy, CodeOverpayment, fmt.Sprintf(
				"payment of %d %s would take invoice %s past its total; %d is still unallocated",
				amount.Minor(), cur.Code(), sanitizeID(invoiceLegacy), room))
			return nil
		}
	}

	// The provider account is synthetic and namespaced. The payments table is unique on
	// (provider, provider_account, provider_payment_id), and filing an import under its own
	// account is what stops an imported id from colliding with a live one for the same
	// gateway — or from being mistaken for a payment this panel actually took.
	providerAccount := "import:" + r.in.Source

	outcome := OutcomeCreated
	var (
		paymentID string
		existing  string
		mappedTo  string
	)
	err := r.tx.QueryRow(r.ctx, `
		SELECT p.id::text, COALESCE(ir.legacy_id, '')
		FROM payments p
		LEFT JOIN import_records ir
		  ON ir.source = $4 AND ir.entity = 'payment' AND ir.target_id = p.id
		WHERE p.provider = $1 AND p.provider_account = $2 AND p.provider_payment_id = $3`,
		gateway, providerAccount, providerPaymentID, r.in.Source).Scan(&existing, &mappedTo)
	switch {
	case err == nil && mappedTo == legacy:
		// Already imported by an earlier run: the mapping is the identity, so this is a replay.
		r.written(entity, OutcomeUnchanged)
		return nil
	case err == nil && mappedTo != "":
		r.reject(row, entity, legacy, CodeDuplicatePayment, fmt.Sprintf(
			"provider payment %s is already imported as legacy transaction %s",
			sanitizeID(providerPaymentID), sanitizeID(mappedTo)))
		return nil
	case err == nil:
		// The payment exists but was not imported: a live webhook got there first. Adopt it,
		// which is what house rule 3 already guarantees — the provider id is the identity.
		paymentID = existing
		outcome = OutcomeUnchanged
		if err := r.recordMapping(entity, legacy, paymentID); err != nil {
			return err
		}
	case !errors.Is(err, pgx.ErrNoRows):
		return fmt.Errorf("importer: look up payment: %w", err)
	}

	if paymentID == "" {
		if mapped, ok := r.mappings[mappingKey{entity, legacy}]; ok {
			var (
				curAmount   int64
				curCurrency string
			)
			err := r.tx.QueryRow(r.ctx,
				`SELECT amount_minor, currency FROM payments WHERE id = $1`, mapped).
				Scan(&curAmount, &curCurrency)
			if errors.Is(err, pgx.ErrNoRows) {
				r.reject(row, entity, legacy, CodeConflict, fmt.Sprintf(
					"legacy id %s was imported earlier, but the payment it points at no longer exists",
					sanitizeID(legacy)))
				return nil
			}
			if err != nil {
				return fmt.Errorf("importer: read payment: %w", err)
			}
			if curAmount != amount.Minor() || curCurrency != cur.Code() {
				r.reject(row, entity, legacy, CodePaymentImmutable, fmt.Sprintf(
					"the payment was imported as %d %s and the dump now says %d %s; a recorded "+
						"receipt is not rewritten", curAmount, curCurrency, amount.Minor(), cur.Code()))
				return nil
			}
			r.written(entity, OutcomeUnchanged)
			return nil
		}

		if err := r.tx.QueryRow(r.ctx, `
			INSERT INTO payments (provider, provider_account, provider_payment_id, amount_minor,
				currency, status, received_at)
			VALUES ($1, $2, $3, $4, $5, 'succeeded', $6)
			RETURNING id::text`,
			gateway, providerAccount, providerPaymentID, amount.Minor(), cur.Code(), receivedAt,
		).Scan(&paymentID); err != nil {
			return fmt.Errorf("importer: create payment: %w", err)
		}
		if err := r.recordMapping(entity, legacy, paymentID); err != nil {
			return err
		}
	}

	if invoiceID == "" {
		r.warn(file, row.row, legacy, WarnUnallocatedPayment, fmt.Sprintf(
			"payment %s names no invoice, so it is imported unallocated: it is money received "+
				"that the old panel had not applied to a document", sanitizeID(legacy)))
		r.written(entity, outcome)
		return nil
	}

	// An adopted payment may already be allocated to this invoice by the flow that created it.
	var allocated bool
	err = r.tx.QueryRow(r.ctx, `
		SELECT true FROM payment_allocations WHERE payment_id = $1 AND invoice_id = $2`,
		paymentID, invoiceID).Scan(&allocated)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("importer: check payment allocation: %w", err)
	}
	if !allocated {
		if _, err := r.tx.Exec(r.ctx, `
			INSERT INTO payment_allocations (payment_id, invoice_id, amount_minor, currency)
			VALUES ($1, $2, $3, $4)`,
			paymentID, invoiceID, amount.Minor(), cur.Code()); err != nil {
			return fmt.Errorf("importer: allocate payment: %w", err)
		}
		if st := r.invoices[invoiceLegacy]; st != nil {
			st.allocated += amount.Minor()
		}
	}
	r.written(entity, outcome)
	return nil
}

// allocationRoom is how much of an invoice is still unpaid, counting both the payments this
// run has written and the ones already in the database.
func (r *run) allocationRoom(invoiceLegacy, invoiceID string) (int64, error) {
	if st := r.invoices[invoiceLegacy]; st != nil {
		return st.total - st.allocated, nil
	}
	var (
		total     int64
		allocated int64
	)
	if err := r.tx.QueryRow(r.ctx, `
		SELECT i.total_minor, COALESCE(SUM(a.amount_minor), 0)
		FROM invoices i
		LEFT JOIN payment_allocations a ON a.invoice_id = i.id
		WHERE i.id = $1
		GROUP BY i.total_minor`, invoiceID).Scan(&total, &allocated); err != nil {
		return 0, fmt.Errorf("importer: read invoice allocation room: %w", err)
	}
	return total - allocated, nil
}

// ------------------------------------------------------------------ parents --

// parent resolves a referenced legacy id, distinguishing the three ways it can be missing.
func (r *run) parent(row *csvRow, entity, legacy, parentEntity, parentLegacy, unknownCode string) (string, bool) {
	if parentLegacy == "" {
		r.reject(row, entity, legacy, unknownCode, fmt.Sprintf("the row names no %s id", parentEntity))
		return "", false
	}
	if target, ok := r.mappings[mappingKey{parentEntity, parentLegacy}]; ok {
		return target, true
	}
	if code, refused := r.refusedParent(parentEntity, parentLegacy); refused {
		r.reject(row, entity, legacy, CodeParentRejected, fmt.Sprintf(
			"the %s %s was refused in this dump (%s), so this row cannot be imported",
			parentEntity, sanitizeID(parentLegacy), code))
		return "", false
	}
	r.reject(row, entity, legacy, unknownCode, fmt.Sprintf(
		"%s %s is not in this dump and has not been imported before",
		parentEntity, sanitizeID(parentLegacy)))
	return "", false
}

// ------------------------------------------------------------------- enums --

// invoiceStatus maps a dump status onto this panel's invoice states.
//
// The mapping is explicit rather than "lower-case it and hope": `unpaid` and `open` mean the
// same thing, and a status this importer does not recognise is refused instead of being
// stored as something it is not.
func invoiceStatus(raw string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "paid":
		return "paid", true
	case "unpaid", "open":
		return "open", true
	case "cancelled", "canceled", "void":
		return "void", true
	case "refunded":
		return "refunded", true
	case "collections", "uncollectible":
		return "uncollectible", true
	case "disputed":
		return "disputed", true
	case "draft":
		return "draft", true
	}
	return "", false
}

// serviceStatus maps a dump status onto this panel's service states.
//
// The second return value marks the one mapping that loses information: a cancelled service
// in a WHMCS-style dump is not a state this schema has, and terminating it is the closest
// true thing. The caller warns, so the difference is visible rather than assumed.
func serviceStatus(raw string) (string, bool, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "active":
		return "active", false, true
	case "suspended":
		return "suspended", false, true
	case "pending":
		return "pending", false, true
	case "terminated":
		return "terminated", false, true
	case "cancelled", "canceled":
		return "terminated", true, true
	case "fraud", "error":
		return "error", false, true
	case "provisioning":
		return "provisioning", false, true
	case "terminating":
		return "terminating", false, true
	}
	return "", false, false
}

// normalizeGateway turns a dump's gateway name into the provider slug the payments table
// stores.
func normalizeGateway(raw string) string {
	var b strings.Builder
	for _, c := range strings.ToLower(strings.TrimSpace(raw)) {
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '_' || c == '-':
			b.WriteRune(c)
		case c == ' ' || c == '.':
			b.WriteByte('_')
		}
	}
	return strings.Trim(b.String(), "_")
}

// ----------------------------------------------------------------- helpers --

// currencyFor resolves a stored currency code the way the rest of the panel does.
func currencyFor(code string) (money.Currency, error) {
	if cur, ok := money.LookupCurrency(code); ok {
		return cur, nil
	}
	return money.NewCurrency(code, 2)
}

// validEmail is deliberately permissive: it rejects the shapes that are certainly not an
// address (no @, a space, nothing on either side) and leaves the rest to the panel.
func validEmail(email string) bool {
	at := strings.IndexByte(email, '@')
	if at <= 0 || at != strings.LastIndexByte(email, '@') || at == len(email)-1 {
		return false
	}
	return !strings.ContainsAny(email, " \t\r\n")
}

// nullableText maps "" to NULL, so an absent external id does not become an empty string.
func nullableText(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// sameJSON compares a stored jsonb document with the one just encoded, so an unchanged
// service is reported as unchanged rather than rewritten on every run.
func sameJSON(stored, want []byte) bool {
	var a, b any
	if err := json.Unmarshal(stored, &a); err != nil {
		return false
	}
	if err := json.Unmarshal(want, &b); err != nil {
		return false
	}
	aj, err := json.Marshal(a)
	if err != nil {
		return false
	}
	bj, err := json.Marshal(b)
	if err != nil {
		return false
	}
	return string(aj) == string(bj)
}
