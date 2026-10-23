// Package importer migrates a billing history from another panel into this one.
//
// The input is a *set* of CSV documents — clients, invoices, invoice items, services and
// transactions, plus optional currencies, admins, products and tickets — exported from the old
// panel, and the output is a report saying, row by row, what was created, what was updated, what
// was already there and what was refused and why. Which of the optional documents this build reads,
// and what it deliberately leaves behind (orders, cancellations, config options, ticket threads),
// is stated at the top of whmcs_extended.go.
//
// The rules that shape it, and the reason each one is not negotiable:
//
//   - **A dry run is the default, and it writes nothing.** The dry run is not a separate
//     implementation of the rules: it runs the same code inside a transaction that is
//     always rolled back, so the counts it prints are the counts a commit produces. Two
//     implementations would drift, and the one that drifts is always the one the operator
//     trusted.
//
//   - **A commit is one transaction.** See the header of migration 0039 for why a resumable
//     step list was rejected. The short version: the only outcomes are "the dump is in" and
//     "the dump is not in", and the dry run has already said which.
//
//   - **Idempotency is a row, not a check.** Every entity carries the legacy id it had in
//     the old system, and `import_records` holds the mapping from that natural key to the
//     row it became. Re-running the same dump finds the mapping and takes the
//     update-or-unchanged path, so it creates nothing and reports exactly that.
//
//   - **The importer never invents a number.** Every imported invoice either carries its
//     historical number or is refused (`missing_invoice_number`); a number already used in
//     this panel is a conflict (`number_taken`), never a renumbering. The same rule extends
//     to amounts, dates and statuses: a row that cannot be read exactly is refused with a
//     reason and a row number rather than rounded into something plausible. An imported
//     invoice that would collide with the panel's own numbering series advances the series
//     instead, so the next invoice this panel issues cannot take a number the old system
//     already printed.
//
//   - **Rejections are not fatal.** A commit writes the rows it can and records the ones it
//     cannot, because a real dump from a real install always contains a refunded invoice, a
//     negative line or a client with a malformed email, and refusing the whole migration
//     over three rows would make the importer useless. Fix the export, run it again: the
//     valid rows are unchanged, and only the previously refused ones are attempted.
//
//   - **Nothing is posted to the ledger.** An imported invoice and an imported payment are
//     records of what the old system did; posting a three-year-old invoice today would move
//     this period's revenue and tax liability. An operator who wants opening balances posts
//     one adjustment, deliberately, with a reason.
//
// What it deliberately does not do: create subscriptions from imported services (the next
// renewal date in a dump is a fact about the old system, and turning it into a subscription
// bills the customer again on cut-over day), import ticket threads, or guess a product from
// a numeric package id (a service names its product by slug, or it is refused — a wrong
// product would price the next renewal wrong).
package importer

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/catalyst/billing/internal/audit"
	"github.com/catalyst/billing/internal/store"
	"github.com/jackc/pgx/v5"
)

// The five documents a dump carries, in the order they are processed.
//
// The order is a dependency order, not a preference: an invoice needs its client, a line
// needs its invoice, a payment needs its invoice. Processing in this order means a row
// whose parent is refused in the same run is reported as `parent_rejected` instead of
// failing with a database error halfway through the transaction.
const (
	FileClients      = "clients"
	FileInvoices     = "invoices"
	FileInvoiceItems = "invoice_items"
	FileServices     = "services"
	FileTransactions = "transactions"
)

// Files is the processing order.
var Files = []string{FileClients, FileInvoices, FileInvoiceItems, FileServices, FileTransactions}

// Entity names. They are the `entity` column of import_records and the key of a table
// report, so they are stable identifiers rather than prose.
const (
	EntityCustomer    = "customer"
	EntityInvoice     = "invoice"
	EntityInvoiceLine = "invoice_line"
	EntityService     = "service"
	EntityPayment     = "payment"
)

// EntityOrder is the order a report lists its tables in.
var EntityOrder = []string{
	EntityCustomer, EntityInvoice, EntityInvoiceLine, EntityService, EntityPayment,
}

// Outcomes of one row. A row that was already imported and is identical is `unchanged`, not
// `updated`: the difference is what tells an operator whether a second run of the same dump
// did anything at all.
const (
	OutcomeCreated   = "created"
	OutcomeUpdated   = "updated"
	OutcomeUnchanged = "unchanged"
	OutcomeRejected  = "rejected"
)

// Rejection codes. They are part of the API: a client branches on the code, never on the
// detail sentence, so a copy edit cannot change behaviour.
const (
	CodeInvalidID          = "invalid_id"
	CodeDuplicateID        = "duplicate_id_in_dump"
	CodeMissingColumn      = "missing_column"
	CodeInvalidCSV         = "invalid_csv"
	CodeFieldCount         = "field_count"
	CodeInvalidEmail       = "invalid_email"
	CodeInvalidAmount      = "invalid_amount"
	CodeNegativeAmount     = "negative_amount"
	CodeInvalidDate        = "invalid_date"
	CodeMissingDate        = "missing_date"
	CodeMissingCurrency    = "missing_currency"
	CodeInvalidCurrency    = "invalid_currency"
	CodeUnknownStatus      = "unknown_status"
	CodeUnknownClient      = "unknown_client"
	CodeUnknownInvoice     = "unknown_invoice"
	CodeParentRejected     = "parent_rejected"
	CodeMissingNumber      = "missing_invoice_number"
	CodeDuplicateNumber    = "duplicate_invoice_number"
	CodeNumberTaken        = "number_taken"
	CodeInvoiceImmutable   = "invoice_immutable"
	CodeDraftInvoice       = "draft_invoice"
	CodeTotalMismatch      = "total_mismatch"
	CodeEmailTaken         = "email_taken"
	CodeUnknownProduct     = "unknown_product"
	CodeNonPositivePayment = "non_positive_payment"
	CodePaymentImmutable   = "payment_immutable"
	CodeDuplicatePayment   = "duplicate_payment"
	CodeOverpayment        = "overpayment"
	CodeUnknownSource      = "unknown_source"
	CodeMissingFile        = "missing_file"
	CodeInvalidValue       = "invalid_value"
	CodeConflict           = "conflict"
)

// Warning codes: things that were imported but that the operator should know about. They
// never change what was written.
const (
	WarnPaidWithoutPayments = "paid_invoice_without_payments"
	WarnPaymentAllocations  = "invoice_paid_disagrees_with_payments"
	WarnSubtotalMismatch    = "invoice_subtotal_disagrees_with_lines"
	WarnUnallocatedPayment  = "payment_without_invoice"
	WarnServicesNotBilled   = "services_imported_without_subscription"
	WarnTaxNotReconstructed = "line_tax_not_reconstructed"
	WarnCustomerMatched     = "customer_matched_by_email"
	WarnServiceCancelled    = "service_cancelled_imported_as_terminated"
)

var (
	// ErrUnknownSource is returned for a dump dialect this build does not read.
	ErrUnknownSource = errors.New("importer: unknown dump source")
	// ErrMissingFile is returned when a dump omits a document the source declares. Every
	// file is required even if it is only a header, so "I forgot to export transactions"
	// is a refusal rather than a silently paymentless import.
	ErrMissingFile = errors.New("importer: dump is missing a file")
	// ErrInvalidInput is returned for a request the importer cannot act on.
	ErrInvalidInput = errors.New("importer: invalid input")
	// ErrNotFound is returned when a run does not exist.
	ErrNotFound = errors.New("importer: run not found")
	// ErrConflict is returned when a concurrent run won a race the importer could not
	// resolve by checking first. It is retryable and the caller should say so.
	ErrConflict = errors.New("importer: another import wrote the same record")
	// ErrUnavailable is returned when the importer is asked to run without a database.
	ErrUnavailable = errors.New("importer: no database configured")
)

// Input is one import request.
type Input struct {
	// Source selects the dump dialect: "whmcs" or "paymenter".
	Source string
	// Commit distinguishes a dry run from a real one. The zero value is a dry run: a caller
	// that forgets to say which it wants does not get to write.
	Commit bool
	// DefaultCurrency is used for a row that carries no currency of its own. Empty means
	// "there is no default", and such a row is refused with `missing_currency`.
	DefaultCurrency string
	// ActorID is the signed-in user who asked for the run. It is stored on the run row and
	// on the audit event.
	ActorID string
	// CorrelationID and IP travel into the audit event, so an import is attributable in the
	// same way a refund is.
	CorrelationID string
	IP            string
	// Auditor is the request-scoped audit writer. When it is nil the importer writes the
	// audit event itself.
	Auditor audit.Auditor
	// Files maps a document name (clients, invoices, ...) to its CSV text.
	Files map[string]string
	// Now is the clock. Zero means time.Now.
	Now time.Time
}

func (in Input) withDefaults() Input {
	in.Source = strings.ToLower(strings.TrimSpace(in.Source))
	in.DefaultCurrency = strings.ToUpper(strings.TrimSpace(in.DefaultCurrency))
	if in.Now.IsZero() {
		in.Now = time.Now()
	}
	return in
}

// Totals is the headline count of one report.
type Totals struct {
	RowsRead  int `json:"rows_read"`
	Created   int `json:"created"`
	Updated   int `json:"updated"`
	Unchanged int `json:"unchanged"`
	Rejected  int `json:"rejected"`
}

// FileReport is what happened to one document.
type FileReport struct {
	File string `json:"file"`
	// RowsRead counts data rows, not lines: the header is not a row, and a blank line is
	// not one either.
	RowsRead int `json:"rows_read"`
	Rejected int `json:"rejected"`
	// Error is set when the document as a whole could not be read — a missing required
	// column, or a quoting error. Rows then carry no individual rejection: there was nothing
	// to attribute one to.
	Error string `json:"error,omitempty"`
}

// TableReport is what happened to one entity.
type TableReport struct {
	Entity    string `json:"entity"`
	Created   int    `json:"created"`
	Updated   int    `json:"updated"`
	Unchanged int    `json:"unchanged"`
	Rejected  int    `json:"rejected"`
}

// Rejection is one refused row.
//
// Row and Line are both reported because they answer different questions: Row is the n-th
// data row, which is what a person counts in a spreadsheet below the header, and Line is
// the physical line in the file, which is what an editor jumps to when the file contains
// embedded newlines inside quoted fields.
type Rejection struct {
	File     string `json:"file"`
	Row      int    `json:"row"`
	Line     int    `json:"line"`
	Entity   string `json:"entity"`
	LegacyID string `json:"legacy_id,omitempty"`
	Code     string `json:"code"`
	Detail   string `json:"detail"`
}

// Warning is something imported that the operator should look at. It never changes what was
// written, which is why it is a separate list rather than a rejection with a severity.
type Warning struct {
	File     string `json:"file,omitempty"`
	Row      int    `json:"row,omitempty"`
	LegacyID string `json:"legacy_id,omitempty"`
	Code     string `json:"code"`
	Detail   string `json:"detail"`
}

// Report is the answer to "what would happen" (dry run) or "what happened" (commit).
type Report struct {
	Source string `json:"source"`
	// DryRun is true when nothing was written. It is repeated at the top level, rather than
	// inferred from a missing RunID, because it is the single most important fact in the
	// document.
	DryRun     bool          `json:"dry_run"`
	RunID      string        `json:"run_id,omitempty"`
	StartedAt  time.Time     `json:"started_at"`
	DurationMS int64         `json:"duration_ms"`
	Totals     Totals        `json:"totals"`
	Files      []FileReport  `json:"files"`
	Tables     []TableReport `json:"tables"`
	Rejections []Rejection   `json:"rejections"`
	Warnings   []Warning     `json:"warnings"`
}

// emptyReport builds a report with every file and every table present, so a client renders
// the same shape whether or not anything was rejected.
func emptyReport(in Input, prof *profile) Report {
	files := make([]FileReport, 0, len(Files))
	for _, f := range Files {
		files = append(files, FileReport{File: f})
	}
	tables := make([]TableReport, 0, len(EntityOrder))
	for _, e := range EntityOrder {
		tables = append(tables, TableReport{Entity: e})
	}
	return Report{
		Source:     prof.source,
		DryRun:     !in.Commit,
		StartedAt:  in.Now.UTC(),
		Files:      files,
		Tables:     tables,
		Rejections: []Rejection{},
		Warnings:   []Warning{},
	}
}

// Run executes one import.
//
// A dry run executes the same code as a commit and then rolls the transaction back. There is
// no second implementation of the rules to drift from this one, which is what makes the
// dry-run report trustworthy.
func Run(ctx context.Context, pool *store.Pool, in Input) (Report, error) {
	if pool == nil {
		return Report{}, ErrUnavailable
	}
	in = in.withDefaults()
	prof, err := profileFor(in.Source)
	if err != nil {
		return Report{}, err
	}
	if err := prof.checkFiles(in.Files); err != nil {
		return Report{}, err
	}

	var rep Report
	if in.Commit {
		err = pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			r, err := newRun(ctx, tx, in, prof)
			if err != nil {
				return err
			}
			if err := r.execute(); err != nil {
				return err
			}
			rep = r.report()
			return r.commit()
		})
		if err != nil {
			return rep, classify(err)
		}
		return rep, nil
	}

	// Dry run: run everything, then undo it. The sentinel is how the transaction helper is
	// asked for a rollback without turning the rollback into an error.
	err = pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		r, err := newRun(ctx, tx, in, prof)
		if err != nil {
			return err
		}
		if err := r.execute(); err != nil {
			return err
		}
		rep = r.report()
		return errDryRun
	})
	if err != nil && !errors.Is(err, errDryRun) {
		return rep, classify(err)
	}
	return rep, nil
}

// errDryRun is the sentinel that rolls a dry run back. It never leaves this package.
var errDryRun = errors.New("importer: dry run")

// classify turns a driver error into the importer's own vocabulary.
//
// A unique violation here means a concurrent import inserted the same natural key between
// this run's check and its insert. The run's transaction is already doomed, so the honest
// answer is "try again", not "row 41 is bad".
func classify(err error) error {
	if errors.Is(err, errDryRun) {
		return nil
	}
	if store.IsUniqueViolation(err) {
		return fmt.Errorf("%w: %v", ErrConflict, err)
	}
	return err
}

// sortedFiles returns the report's file entries in processing order.
func (r *Report) fileIndex(name string) int {
	for i := range r.Files {
		if r.Files[i].File == name {
			return i
		}
	}
	return -1
}

// ensureFile returns the position of a document's counters, appending one for a document that is
// not in the required set.
//
// The optional documents (currencies, admins, products, tickets) are only present in a report when
// the dump carried them, so the report grows rather than showing four empty rows on every import.
func (r *Report) ensureFile(name string) int {
	if i := r.fileIndex(name); i >= 0 {
		return i
	}
	r.Files = append(r.Files, FileReport{File: name})
	return len(r.Files) - 1
}

// tableIndex returns the position of an entity's counters, appending one if the entity is
// not in the fixed list (which would be a bug, but a missing counter is worse than an
// unexpected one).
func (r *Report) tableIndex(entity string) int {
	for i := range r.Tables {
		if r.Tables[i].Entity == entity {
			return i
		}
	}
	r.Tables = append(r.Tables, TableReport{Entity: entity})
	return len(r.Tables) - 1
}

// count records one outcome, keeping the totals and the table counters in step.
func (r *Report) count(entity, outcome string) {
	i := r.tableIndex(entity)
	switch outcome {
	case OutcomeCreated:
		r.Tables[i].Created++
		r.Totals.Created++
	case OutcomeUpdated:
		r.Tables[i].Updated++
		r.Totals.Updated++
	case OutcomeUnchanged:
		r.Tables[i].Unchanged++
		r.Totals.Unchanged++
	case OutcomeRejected:
		r.Tables[i].Rejected++
		r.Totals.Rejected++
	}
}

// sortRejections orders rejections the way a person reads the dump: by file, then by row.
// Insertion order is already close, but the invoice-number conflict is detected while
// processing a later row and reported against the earlier one.
func (r *Report) sortRejections() {
	order := make(map[string]int, len(Files))
	for i, f := range Files {
		order[f] = i
	}
	sort.SliceStable(r.Rejections, func(i, j int) bool {
		a, b := r.Rejections[i], r.Rejections[j]
		if order[a.File] != order[b.File] {
			return order[a.File] < order[b.File]
		}
		if a.Row != b.Row {
			return a.Row < b.Row
		}
		return a.Code < b.Code
	})
}
