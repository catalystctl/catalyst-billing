package tax

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/catalyst/billing/internal/commerce"
	"github.com/jackc/pgx/v5"
)

// Applying a document: one transaction, one savepoint per row, one recorded run.
//
// The shape is deliberate.
//
//   - **The fetch happens outside the transaction.** Holding a database transaction open across a
//     network call is how a slow source becomes a pool exhaustion, and the fetch has nothing to do
//     with the write.
//   - **The diff is recomputed inside the transaction.** The preview an operator saw may be
//     minutes old; the decision that matters is the one taken against the rows as they are now.
//   - **Every write is its own savepoint.** A constraint this package did not predict — a race with
//     the staff form, a check the rate table enforces — becomes one rejected row with a reason,
//     instead of rolling back the two hundred rows that were fine.
//   - **The run row is written in the same transaction as the rates.** A run that claims to have
//     imported twenty rates must be a run whose twenty rates are committed.

// Apply fetches the configured source and writes the changes it states.
//
// It returns the run record and the report of what was actually applied. When the fetch or the
// document fails outright, the failure is recorded as a `failed` run and returned: "the refresh did
// nothing" and "the refresh could not read the source" are different facts, and an operator
// debugging a stale rate table needs to tell them apart.
func (i *Importer) Apply(ctx context.Context, opts Options) (*Result, error) {
	if opts.Trigger == "" {
		opts.Trigger = TriggerManual
	}
	src, err := i.readSource(ctx)
	if err != nil {
		return nil, err
	}
	payload, err := i.Fetch(ctx, src)
	if err != nil {
		i.recordFailure(ctx, src, fetchPayload{}, opts, err)
		return nil, err
	}
	rows, rejections, err := Parse(payload.Format, payload.Body)
	if err != nil {
		i.recordFailure(ctx, src, payload, opts, err)
		return nil, err
	}

	var result Result
	err = i.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		startedAt := i.now()
		runID, err := insertRun(ctx, tx, payload, opts, startedAt)
		if err != nil {
			return err
		}

		changes, err := i.diff(ctx, tx, rows, payload.FetchedAt, opts.OverwriteManual)
		if err != nil {
			return err
		}
		applied := make([]Change, 0, len(changes))
		for _, change := range changes {
			if change.Outcome != OutcomeCreate && change.Outcome != OutcomeUpdate {
				applied = append(applied, change)
				continue
			}
			if err := i.writeChange(ctx, tx, change, payload); err != nil {
				// The savepoint was rolled back, so the rate table is exactly as it was for this
				// jurisdiction; the row becomes a rejection with the database's reason.
				change.Outcome = OutcomeRejected
				change.Code = CodeWriteFailed
				change.Reason = err.Error()
			}
			applied = append(applied, change)
		}

		preview := buildPreview(payload, applied, rejections)
		if err := finishRun(ctx, tx, runID, OutcomeRunOK, "", preview.counts(), preview); err != nil {
			return err
		}
		result = Result{
			Run: Run{
				ID:        runID,
				SourceURL: payload.URL,
				Format:    payload.Format,
				Trigger:   opts.Trigger,
				ActorID:   opts.ActorID,
				FetchedAt: payload.FetchedAt,
				StartedAt: startedAt,
				Outcome:   OutcomeRunOK,
				Counts:    preview.counts(),
				Preview:   &preview,
			},
			Preview: preview,
		}
		if opts.Audit != nil {
			// Inside the transaction on purpose: an audit row that can be missing for a
			// committed import is not an audit trail.
			if err := opts.Audit(ctx, tx, result); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		i.recordFailure(ctx, src, payload, opts, err)
		return nil, err
	}
	return &result, nil
}

// writeChange applies one row under its own savepoint.
func (i *Importer) writeChange(ctx context.Context, tx pgx.Tx, change Change, payload fetchPayload) error {
	// A nested Begin is a SAVEPOINT in pgx. Rolling it back undoes only this row.
	savepoint, err := tx.Begin(ctx)
	if err != nil {
		return fmt.Errorf("tax: begin savepoint: %w", err)
	}
	if err := i.writeChangeTx(ctx, savepoint, change, payload); err != nil {
		if rbErr := savepoint.Rollback(ctx); rbErr != nil && !errors.Is(rbErr, pgx.ErrTxClosed) {
			return fmt.Errorf("%w (and the rollback failed: %v)", err, rbErr)
		}
		return err
	}
	if err := savepoint.Commit(ctx); err != nil {
		return fmt.Errorf("tax: release savepoint: %w", err)
	}
	return nil
}

// writeChangeTx closes the rate being replaced and appends the new one, then stamps provenance.
//
// The close-then-append pair is the rate table's own supported operation (see
// commerce.UpdateTaxRateTx): a rate that has taken effect is evidence an issued invoice points at,
// so it is never rewritten. The only exception is a row that has not taken effect yet — closing it
// and appending a second row would leave two rows describing a period that never happened — but
// that case cannot arise here, because a rate that has not taken effect is still the *open* row and
// resolveEffectiveFrom guarantees the incoming row starts after it.
func (i *Importer) writeChangeTx(ctx context.Context, q pgx.Tx, change Change, payload fetchPayload) error {
	from := change.Proposed.EffectiveFrom
	if from.IsZero() {
		from = i.now()
	}
	inclusive := change.Proposed.Inclusive

	if change.Outcome == OutcomeUpdate && change.Current != nil {
		if _, err := i.commerce.UpdateTaxRateTx(ctx, q, change.Current.ID,
			commerce.TaxRateInput{EffectiveTo: &from}); err != nil {
			return err
		}
	}

	created, err := i.commerce.CreateTaxRateTx(ctx, q, commerce.TaxRateInput{
		Name:          change.Proposed.Name,
		CountryCode:   change.Proposed.CountryCode,
		RegionCode:    change.Proposed.RegionCode,
		TaxClass:      change.Proposed.TaxClass,
		Rate:          change.Proposed.Rate,
		Inclusive:     &inclusive,
		EffectiveFrom: &from,
	})
	if err != nil {
		return err
	}

	// Provenance, in the same transaction as the row it describes: a rate whose source is not
	// recorded is a rate nobody can explain, and a rate that exists while its provenance does not
	// is worse than either.
	if _, err := q.Exec(ctx, `
		UPDATE tax_rates
		   SET origin = $2, source = $3, fetched_at = $4
		 WHERE id = $1`,
		created.ID, OriginImport, payload.URL, payload.FetchedAt); err != nil {
		return fmt.Errorf("tax: record rate provenance: %w", err)
	}
	return nil
}

// ------------------------------------------------------------- the run row --

func insertRun(ctx context.Context, q querier, payload fetchPayload, opts Options, startedAt time.Time) (string, error) {
	fetchedAt := payload.FetchedAt
	if fetchedAt.IsZero() {
		fetchedAt = startedAt
	}
	format := payload.Format
	if format == "" {
		format = FormatJSON
	}
	var id string
	err := q.QueryRow(ctx, `
		INSERT INTO tax_import_runs
			(source_url, format, trigger, actor_id, fetched_at, started_at, outcome)
		VALUES ($1, $2, $3, NULLIF($4, '')::uuid, $5, $6, 'running')
		RETURNING id`,
		payload.URL, format, opts.Trigger, opts.ActorID, fetchedAt, startedAt).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("tax: record import run: %w", err)
	}
	return id, nil
}

func finishRun(ctx context.Context, q querier, id, outcome, detail string, counts Counts, report Preview) error {
	reportJSON, err := marshalReport(report)
	if err != nil {
		return err
	}
	tag, err := q.Exec(ctx, `
		UPDATE tax_import_runs
		   SET outcome = $2, detail = $3, finished_at = now(),
		       rows_total = $4, created = $5, updated = $6, unchanged = $7,
		       protected = $8, rejected = $9, report = $10
		 WHERE id = $1`,
		id, outcome, detail, counts.Rows, counts.Created, counts.Updated, counts.Unchanged,
		counts.Protected, counts.Rejected, reportJSON)
	if err != nil {
		return fmt.Errorf("tax: finish import run: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("tax: import run %s disappeared", id)
	}
	return nil
}

// recordFailure writes a `failed` run for a fetch or parse that never reached the rate table.
//
// It is best effort on purpose: the caller is already returning the real error, and a failure to
// record a failure must not replace it with a confusing second one.
func (i *Importer) recordFailure(ctx context.Context, src Source, payload fetchPayload, opts Options, cause error) {
	if opts.Trigger == "" {
		opts.Trigger = TriggerManual
	}
	if payload.URL == "" {
		payload.URL = src.URL
		payload.Format = src.Format
	}
	if payload.FetchedAt.IsZero() {
		payload.FetchedAt = i.now()
	}
	startedAt := i.now()
	// The error is deliberately dropped: the caller is already returning the failure that explains
	// itself, and a failure to record a failure must not replace it with a confusing second one.
	_ = i.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		id, err := insertRun(ctx, tx, payload, opts, startedAt)
		if err != nil {
			return err
		}
		return finishRun(ctx, tx, id, OutcomeRunFailed, describeErr(cause), Counts{}, Preview{})
	})
}

func marshalReport(preview Preview) ([]byte, error) {
	if preview.Changes == nil {
		preview.Changes = []Change{}
	}
	if preview.Rejections == nil {
		preview.Rejections = []Rejection{}
	}
	encoded, err := json.Marshal(preview)
	if err != nil {
		return nil, fmt.Errorf("tax: encode import report: %w", err)
	}
	return encoded, nil
}
