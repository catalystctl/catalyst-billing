package importer

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/catalyst/billing/internal/audit"
	"github.com/catalyst/billing/internal/billing"
	"github.com/catalyst/billing/internal/money"
	"github.com/jackc/pgx/v5"
)

// run is one execution of the importer: a transaction, the report being built, and the
// state the entity rules need.
//
// The state is small on purpose. Everything the importer needs to know about the database it
// reads once at the start (the mappings) or looks up when it needs it (a product, a
// payment's provider id). Nothing is cached that could disagree with the transaction it is
// reading from: the whole run is one transaction, so a lookup always sees the rows this run
// has already written.
type run struct {
	ctx  context.Context
	tx   pgx.Tx
	in   Input
	prof *profile
	rep  Report
	// runID names the row that will be inserted at the end. It is generated up front
	// because the mappings written during the run have to name it.
	runID string

	// mappings is (entity, legacy id) -> target id, loaded once for the source.
	mappings map[mappingKey]string
	// byTarget is the reverse, per entity: target id -> legacy id. It is what stops an
	// adopted customer from acquiring a second legacy id.
	byTarget map[mappingKey]string
	// refused remembers the legacy ids this run rejected, so a child row can say
	// `parent_rejected` instead of the misleading `unknown_client`.
	refused map[mappingKey]string
	// claimed tracks the natural keys this run has already written. Without it, two rows
	// with the same legacy id in one file would both take the create path and the second
	// would hit the unique index — which aborts the transaction, not the row.
	claimed map[mappingKey]bool
	// numbers maps an invoice number to the legacy id that claimed it in this run.
	numbers map[string]string
	// products caches a slug -> id lookup.
	products map[string]string
	// invoices holds the per-invoice facts the end-of-run reconciliation needs.
	invoices map[string]*invoiceState
	// seriesMax is the largest number in the panel's own invoice series that this dump
	// printed. The series is advanced past it so the next invoice issued here cannot reuse a
	// number the old system already put on a document.
	seriesMax int64
	// services is how many services were written, for the "these will not be billed" warning.
	services int
	// taxedInvoices is how many invoices carried tax, for the line-tax warning.
	taxedInvoices int
	// lines is how many invoice lines were written.
	lines int
}

type mappingKey struct {
	entity string
	legacy string
}

// invoiceState is what the reconciliation warnings and the issue step need about one invoice
// row.
type invoiceState struct {
	legacyID string
	file     string
	row      int
	targetID string
	// created marks an invoice this run wrote. It is the only kind still in draft, and a
	// draft is the only invoice whose lines may be written.
	created bool
	// number, status, issuedAt, dueAt and snapshot are what the invoice is issued with once
	// its lines are in.
	number   string
	status   string
	issuedAt *time.Time
	dueAt    *time.Time
	snapshot []byte
	// total is the invoice's own total, in minor units. It is the ceiling a payment may
	// allocate against.
	total int64
	// paid is what the invoice says it was paid, in minor units.
	paid int64
	// allocated is the sum of the payments in this dump that name this invoice.
	allocated int64
	// subtotal is the invoice's own subtotal, in minor units.
	subtotal int64
	// lineTotal is the sum of the invoice lines imported for it.
	lineTotal int64
	hasLines  bool
	tax       int64
	// currency is the invoice's currency code, so a line does not have to ask again.
	currency string
}

func newRun(ctx context.Context, tx pgx.Tx, in Input, prof *profile) (*run, error) {
	r := &run{
		ctx:      ctx,
		tx:       tx,
		in:       in,
		prof:     prof,
		rep:      emptyReport(in, prof),
		mappings: map[mappingKey]string{},
		byTarget: map[mappingKey]string{},
		refused:  map[mappingKey]string{},
		claimed:  map[mappingKey]bool{},
		numbers:  map[string]string{},
		products: map[string]string{},
		invoices: map[string]*invoiceState{},
	}
	// The id exists even for a dry run, because the mappings written during the run name it
	// and the dry run writes those mappings before rolling them back. It is only *reported*
	// for a commit: a dry run has no run row to point at.
	r.runID = newUUID()
	if err := r.loadMappings(); err != nil {
		return nil, err
	}
	return r, nil
}

// loadMappings reads every legacy id already imported from this source.
//
// One query rather than one per row: the map is what decides create-versus-update for every
// row, so it is read before the first row and never re-read. A row inserted by this run is
// added to the same map, which is what makes the in-file duplicate check work.
func (r *run) loadMappings() error {
	rows, err := r.tx.Query(r.ctx,
		`SELECT entity, legacy_id, target_id::text FROM import_records WHERE source = $1`,
		r.in.Source)
	if err != nil {
		return fmt.Errorf("importer: read existing mappings: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var entity, legacy, target string
		if err := rows.Scan(&entity, &legacy, &target); err != nil {
			return fmt.Errorf("importer: scan mapping: %w", err)
		}
		r.mappings[mappingKey{entity, legacy}] = target
		r.byTarget[mappingKey{entity, target}] = legacy
	}
	return rows.Err()
}

// execute walks the documents in dependency order and then reconciles what it wrote.
func (r *run) execute() error {
	for _, file := range Files {
		if err := r.file(file); err != nil {
			return err
		}
	}
	// The optional documents (currencies, admins, products, tickets) run after the required ones
	// because they depend on them: a ticket needs its customer and a product may be named by a
	// service. See whmcs_extended.go for what is deliberately not imported from a dump.
	if err := r.runExtended(); err != nil {
		return err
	}
	if err := r.issueInvoices(); err != nil {
		return err
	}
	if err := r.reserveInvoiceSeries(); err != nil {
		return err
	}
	r.reconcile()
	r.rep.sortRejections()
	r.rep.DurationMS = r.in.Now.Sub(r.rep.StartedAt).Milliseconds()
	if r.rep.DurationMS < 0 {
		r.rep.DurationMS = 0
	}
	return nil
}

// issueInvoices turns every invoice this run created from a draft into the issued document
// the dump describes.
//
// It goes through a draft because the database refuses to add a line to an issued invoice,
// and an import has to write the invoice and its lines. That is not a workaround: it is the
// same path the panel's own issuer takes — price, write the draft and its lines, then issue —
// and it keeps the immutability trigger meaningful instead of making the importer an
// exception to it. Everything happens inside the run's transaction, so no reader ever sees
// the draft.
func (r *run) issueInvoices() error {
	legacies := make([]string, 0, len(r.invoices))
	for legacy := range r.invoices {
		legacies = append(legacies, legacy)
	}
	sort.Strings(legacies)
	for _, legacy := range legacies {
		st := r.invoices[legacy]
		if !st.created {
			continue
		}
		tag, err := r.tx.Exec(r.ctx, `
			UPDATE invoices
			SET number = $2, status = $3, issued_at = $4, due_at = $5, snapshot = $6
			WHERE id = $1 AND status = 'draft'`,
			st.targetID, st.number, st.status, st.issuedAt, st.dueAt, st.snapshot)
		if err != nil {
			return fmt.Errorf("importer: issue invoice %s: %w", st.legacyID, err)
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("importer: issue invoice %s: the draft was not found", st.legacyID)
		}
	}
	return nil
}

// reserveInvoiceSeries advances this panel's own invoice numbering past every number the
// dump printed in that series.
//
// Without it, an installation whose old panel numbered invoices INV-000001..INV-000900 and
// whose new panel has issued none would print INV-000001 again — a second document with a
// number a customer already holds. Advancing the sequence is not inventing a number: it is
// refusing to reuse one.
func (r *run) reserveInvoiceSeries() error {
	if r.seriesMax <= 0 {
		return nil
	}
	if _, err := r.tx.Exec(r.ctx, `
		INSERT INTO invoice_number_sequences (scope, next_value) VALUES ($1, $2)
		ON CONFLICT (scope) DO UPDATE
			SET next_value = GREATEST(invoice_number_sequences.next_value, $2)`,
		billing.DefaultSeries.Scope, r.seriesMax+1); err != nil {
		return fmt.Errorf("importer: reserve invoice numbers: %w", err)
	}
	return nil
}

// file reads one document and applies every row.
//
// A document-level failure (a missing column, a quoting error) is recorded on the file and
// does not stop the run: the other four documents are still worth importing, and the report
// says exactly which file was unreadable.
func (r *run) file(name string) error {
	idx := r.rep.ensureFile(name)
	rd, err := newReader(name, r.in.Files[name], r.prof)
	if err != nil {
		r.rep.Files[idx].Error = errorDetail(err)
		return nil
	}
	for {
		row, rerr := rd.next()
		if rerr != nil && row == nil {
			if errors.Is(rerr, io.EOF) {
				break
			}
			r.rep.Files[idx].Error = errorDetail(rerr)
			break
		}
		if rerr != nil {
			var re *rowError
			if errors.As(rerr, &re) {
				r.reject(row, entityFor(name), "", re.code, re.detail)
			} else {
				return fmt.Errorf("importer: read %s: %w", name, rerr)
			}
			continue
		}
		if err := r.dispatch(name, row); err != nil {
			return err
		}
	}
	r.rep.Files[idx].RowsRead = rd.rows
	r.rep.Totals.RowsRead += rd.rows
	return nil
}

// dispatch routes one row to its entity rules.
func (r *run) dispatch(file string, row *csvRow) error {
	if handled, err := r.dispatchExtended(file, row); handled {
		return err
	}
	switch file {
	case FileClients:
		return r.applyClient(row)
	case FileInvoices:
		return r.applyInvoice(row)
	case FileInvoiceItems:
		return r.applyInvoiceItem(row)
	case FileServices:
		return r.applyService(row)
	case FileTransactions:
		return r.applyTransaction(row)
	}
	return nil
}

// entityFor maps a document to the entity it produces.
func entityFor(file string) string {
	switch file {
	case FileClients:
		return EntityCustomer
	case FileInvoices:
		return EntityInvoice
	case FileInvoiceItems:
		return EntityInvoiceLine
	case FileServices:
		return EntityService
	case FileTransactions:
		return EntityPayment
	}
	return entityForFile(file)
}

// ------------------------------------------------------------------ report --

// reject records one refused row.
func (r *run) reject(row *csvRow, entity, legacyID, code, detail string) {
	file := row.t.file
	r.rep.Rejections = append(r.rep.Rejections, Rejection{
		File: file, Row: row.row, Line: row.line, Entity: entity,
		LegacyID: legacyID, Code: code, Detail: detail,
	})
	r.rep.count(entity, OutcomeRejected)
	if i := r.rep.fileIndex(file); i >= 0 {
		r.rep.Files[i].Rejected++
	}
	if legacyID != "" {
		r.refused[mappingKey{entity, legacyID}] = code
	}
}

// warn records something imported that the operator should know about.
func (r *run) warn(file string, row int, legacyID, code, detail string) {
	r.rep.Warnings = append(r.rep.Warnings, Warning{
		File: file, Row: row, LegacyID: legacyID, Code: code, Detail: detail,
	})
}

// written records one successful row.
func (r *run) written(entity, outcome string) { r.rep.count(entity, outcome) }

// report returns the report as it stands.
func (r *run) report() Report {
	if r.in.Commit {
		r.rep.RunID = r.runID
	}
	return r.rep
}

// commit writes the run row and the audit event, then lets the transaction commit.
//
// The run row is written last because it carries the finished report, and it is written in
// the same transaction as the rows it describes: a run row for a rolled-back import would be
// a lie, exactly like an audit row for a change that did not happen.
func (r *run) commit() error {
	var actor any
	if r.in.ActorID != "" {
		actor = r.in.ActorID
	}
	if _, err := r.tx.Exec(r.ctx, `
		INSERT INTO import_runs (id, source, actor_id, created, updated, unchanged, rejected, report)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		r.runID, r.in.Source, actor,
		r.rep.Totals.Created, r.rep.Totals.Updated, r.rep.Totals.Unchanged, r.rep.Totals.Rejected,
		r.rep,
	); err != nil {
		return fmt.Errorf("importer: record run: %w", err)
	}

	changes := map[string]any{
		"source":    r.in.Source,
		"rows_read": r.rep.Totals.RowsRead,
		"created":   r.rep.Totals.Created,
		"updated":   r.rep.Totals.Updated,
		"unchanged": r.rep.Totals.Unchanged,
		"rejected":  r.rep.Totals.Rejected,
		"warnings":  len(r.rep.Warnings),
		"files":     len(r.rep.Files),
	}
	if r.in.Auditor != nil {
		return r.in.Auditor.RecordAction(r.ctx, r.tx, "import.run", "import_run", r.runID, changes)
	}
	return audit.Record(r.ctx, r.tx, audit.Event{
		Actor:         audit.Actor{ID: r.in.ActorID, Kind: audit.ActorUser},
		Action:        "import.run",
		TargetType:    "import_run",
		TargetID:      r.runID,
		Outcome:       audit.OutcomeSuccess,
		CorrelationID: r.in.CorrelationID,
		IP:            r.in.IP,
		Changes:       changes,
	})
}

// reconcile emits the warnings that can only be known once every file has been read.
//
// Each one describes a disagreement between two documents in the same dump, which is the
// class of problem an import most often hides: the invoice says it was paid, the payments
// file disagrees, and neither is wrong enough to refuse.
func (r *run) reconcile() {
	keys := make([]string, 0, len(r.invoices))
	for k := range r.invoices {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		st := r.invoices[k]
		switch {
		case st.paid > 0 && st.allocated == 0:
			r.warn(st.file, st.row, st.legacyID, WarnPaidWithoutPayments, fmt.Sprintf(
				"invoice %s is marked paid but no payment in this dump names it; it is imported "+
					"as paid with amount_paid %d %s, and the ledger will not have a matching receipt",
				sanitizeID(st.legacyID), st.paid, st.currency))
		case st.paid > 0 && st.allocated != st.paid:
			r.warn(st.file, st.row, st.legacyID, WarnPaymentAllocations, fmt.Sprintf(
				"invoice %s records %d %s paid and the payments in this dump allocate %d; "+
					"both numbers are imported as they are, so the difference stays visible",
				sanitizeID(st.legacyID), st.paid, st.currency, st.allocated))
		}
		if st.hasLines && st.lineTotal != st.subtotal {
			r.warn(st.file, st.row, st.legacyID, WarnSubtotalMismatch, fmt.Sprintf(
				"invoice %s has subtotal %d %s and its lines sum to %d; the invoice total is "+
					"imported from the invoice row, so the lines explain less than the total",
				sanitizeID(st.legacyID), st.subtotal, st.currency, st.lineTotal))
		}
	}
	if r.services > 0 {
		r.warn("", 0, "", WarnServicesNotBilled, fmt.Sprintf(
			"%d service(s) were imported without a subscription: the renewal engine will not "+
				"invoice them until an operator attaches a price, which is deliberate — a "+
				"renewal date from the old panel must not bill a customer twice on cut-over day",
			r.services))
	}
	if r.lines > 0 && r.taxedInvoices > 0 {
		r.warn("", 0, "", WarnTaxNotReconstructed,
			"invoice lines are imported with zero line tax: a WHMCS-style dump carries tax "+
				"at the invoice level, and splitting it across lines would be a number the "+
				"dump never stated. Each invoice's own tax is imported as it was")
	}
}

// ------------------------------------------------------------------ helpers --

// recordMapping writes the natural key for a row this run created or adopted.
func (r *run) recordMapping(entity, legacy, target string) error {
	if _, err := r.tx.Exec(r.ctx, `
		INSERT INTO import_records (source, entity, legacy_id, target_id, run_id)
		VALUES ($1, $2, $3, $4, $5)`,
		r.in.Source, entity, legacy, target, r.runID,
	); err != nil {
		return fmt.Errorf("importer: record %s mapping: %w", entity, err)
	}
	key := mappingKey{entity, legacy}
	r.mappings[key] = target
	r.byTarget[mappingKey{entity, target}] = legacy
	return nil
}

// claim reserves a natural key for this run and reports a duplicate within the dump.
func (r *run) claim(row *csvRow, entity, legacy string) bool {
	key := mappingKey{entity, legacy}
	if r.claimed[key] {
		r.reject(row, entity, legacy, CodeDuplicateID, fmt.Sprintf(
			"legacy id %s appears more than once in this dump; the first row wins", sanitizeID(legacy)))
		return false
	}
	r.claimed[key] = true
	return true
}

// refusedParent reports why a referenced row is not available, distinguishing "the parent
// was refused in this run" from "the parent is not in this dump at all".
func (r *run) refusedParent(entity, legacy string) (string, bool) {
	code, ok := r.refused[mappingKey{entity, legacy}]
	return code, ok
}

// currency resolves a row's currency, falling back to the request's default.
func (r *run) currency(row *csvRow, file string) (money.Currency, bool) {
	code := strings.ToUpper(row.get(file, "currency"))
	if code == "" {
		code = r.in.DefaultCurrency
	}
	if code == "" {
		r.reject(row, entityFor(file), row.get(file, "id"), CodeMissingCurrency,
			"the row has no currency and the request did not name a default one")
		return money.Currency{}, false
	}
	cur, ok := money.LookupCurrency(code)
	if !ok {
		// An unrecognised code is read at scale 2, which is the rule the rest of the panel
		// uses for a currency outside the ISO table (growth, commerce). A code that is in
		// the table keeps its own scale, so a JPY amount is not silently multiplied by 100.
		var err error
		cur, err = money.NewCurrency(code, 2)
		if err != nil {
			r.reject(row, entityFor(file), row.get(file, "id"), CodeInvalidCurrency,
				fmt.Sprintf("currency %q is not a valid code", code))
			return money.Currency{}, false
		}
	}
	return cur, true
}

// amount parses a decimal string exactly. Anything that would need rounding is refused:
// money.Parse rejects more fractional digits than the currency allows, and the importer
// never rounds a number it read.
func (r *run) amount(row *csvRow, file, key string, cur money.Currency) (money.Amount, bool) {
	text := row.get(file, key)
	if text == "" {
		r.reject(row, entityFor(file), row.get(file, "id"), CodeInvalidAmount,
			fmt.Sprintf("the %s column is empty", key))
		return money.Amount{}, false
	}
	amt, err := money.Parse(text, cur)
	if err != nil {
		r.reject(row, entityFor(file), row.get(file, "id"), CodeInvalidAmount,
			fmt.Sprintf("%s %q is not an exact amount in %s: %v", key, text, cur.Code(), err))
		return money.Amount{}, false
	}
	return amt, true
}

// date parses a timestamp. A dump carries wall-clock times with no zone; they are read as
// UTC, which is stated here rather than guessed per row.
func (r *run) date(row *csvRow, file, key string, required bool) (*time.Time, bool) {
	text := row.get(file, key)
	if text == "" {
		if required {
			r.reject(row, entityFor(file), row.get(file, "id"), CodeMissingDate,
				fmt.Sprintf("the %s column is empty", key))
			return nil, false
		}
		return nil, true
	}
	for _, layout := range []string{
		time.RFC3339Nano,
		"2006-01-02 15:04:05",
		"2006-01-02T15:04:05",
		"2006-01-02",
		"02/01/2006",
		"01/02/2006",
	} {
		if t, err := time.Parse(layout, text); err == nil {
			utc := t.UTC()
			return &utc, true
		}
	}
	r.reject(row, entityFor(file), row.get(file, "id"), CodeInvalidDate,
		fmt.Sprintf("%s %q is not a date this importer reads (expected YYYY-MM-DD or an RFC 3339 timestamp)", key, text))
	return nil, false
}

// product resolves a service's product slug to a product id.
func (r *run) product(row *csvRow, slug string) (string, bool, error) {
	if id, ok := r.products[slug]; ok {
		if id == "" {
			r.reject(row, EntityService, row.get(FileServices, "id"), CodeUnknownProduct,
				fmt.Sprintf("no product has the slug %q", slug))
			return "", false, nil
		}
		return id, true, nil
	}
	var id string
	err := r.tx.QueryRow(r.ctx, `SELECT id::text FROM products WHERE slug = $1`, slug).Scan(&id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			r.products[slug] = ""
			r.reject(row, EntityService, row.get(FileServices, "id"), CodeUnknownProduct,
				fmt.Sprintf("no product has the slug %q; the importer will not guess a product "+
					"from a package id, because a wrong product prices the next renewal wrong", slug))
			return "", false, nil
		}
		return "", false, fmt.Errorf("importer: look up product %q: %w", slug, err)
	}
	r.products[slug] = id
	return id, true, nil
}

// reserveSeriesNumber records an imported number that belongs to the panel's own invoice
// series, so the series can be advanced past it.
func (r *run) reserveSeriesNumber(number string) {
	if v, ok := seriesValue(number, billing.DefaultSeries.Prefix); ok && v > r.seriesMax {
		r.seriesMax = v
	}
}

// seriesValue parses "INV-000123" into 123. It is deliberately narrow: only a number that
// the panel's own formatter would produce can collide with the panel's own sequence.
func seriesValue(number, prefix string) (int64, bool) {
	if !strings.HasPrefix(number, prefix) {
		return 0, false
	}
	digits := number[len(prefix):]
	if digits == "" || len(digits) > 18 {
		return 0, false
	}
	var v int64
	for _, c := range digits {
		if c < '0' || c > '9' {
			return 0, false
		}
		v = v*10 + int64(c-'0')
	}
	return v, true
}

// newUUID returns a random RFC 4122 version-4 UUID.
//
// It is generated here rather than by gen_random_uuid() because the run id has to be known
// before the row it names exists: the mappings written during the run carry it, and the run
// row itself is written last.
func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// A run without an id cannot be recorded. This cannot happen on a working system,
		// and a panic here is better than a nil id reaching the database.
		panic("importer: read random bytes: " + err.Error())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	var out [36]byte
	hex.Encode(out[0:8], b[0:4])
	out[8] = '-'
	hex.Encode(out[9:13], b[4:6])
	out[13] = '-'
	hex.Encode(out[14:18], b[6:8])
	out[18] = '-'
	hex.Encode(out[19:23], b[8:10])
	out[23] = '-'
	hex.Encode(out[24:36], b[10:16])
	return string(out[:])
}

// errorDetail renders an error for a report without its sentinel prefix, so the report reads
// as prose rather than as "importer: ...".
func errorDetail(err error) string {
	var re *rowError
	if errors.As(err, &re) {
		return re.detail
	}
	return err.Error()
}
