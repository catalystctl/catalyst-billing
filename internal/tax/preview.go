package tax

import (
	"context"
	"fmt"
	"time"

	"github.com/catalyst/billing/internal/money"
)

// The diff: what the document says, against what the table holds.
//
// The comparison is numeric, not textual. "0.19" and "19/100" and "19%" are the same rate, and a
// preview that called that an update would report a change to the customer's price on every
// refresh — which is how an operator learns to ignore the preview.
//
// The "current" row for a jurisdiction is the open one (`effective_to is null`). There can be at
// most one *enabled* open row per key (the rate table's unique index), plus possibly one the
// operator disabled, which is why the lookup prefers the enabled one.

// openRate is the rate the table holds now for a jurisdiction.
type openRate struct {
	ID            string
	CountryCode   string
	RegionCode    string
	TaxClass      string
	Rate          string
	Inclusive     bool
	EffectiveFrom time.Time
	Enabled       bool
	Origin        string
	Source        string
	FetchedAt     *time.Time
}

func (o openRate) key() string {
	return jurisdiction(o.CountryCode, o.RegionCode, o.TaxClass)
}

// Preview fetches the configured source and reports what an import would do, writing nothing.
//
// It is not a dry run of a different implementation: `Apply` calls the same `diff` inside the
// transaction that writes, so the preview cannot be a nicer story than the import.
func (i *Importer) Preview(ctx context.Context, opts Options) (Preview, error) {
	src, err := i.readSource(ctx)
	if err != nil {
		return Preview{}, err
	}
	payload, err := i.Fetch(ctx, src)
	if err != nil {
		return Preview{}, err
	}
	rows, rejections, err := Parse(payload.Format, payload.Body)
	if err != nil {
		return Preview{}, err
	}
	changes, err := i.diff(ctx, i.pool, rows, payload.FetchedAt, opts.OverwriteManual)
	if err != nil {
		return Preview{}, err
	}
	return buildPreview(payload, changes, rejections), nil
}

// diff decides each row's fate. It reads the open rows and returns one Change per parsed row.
func (i *Importer) diff(ctx context.Context, q querier, rows []Row, fetchedAt time.Time, overwriteManual bool) ([]Change, error) {
	open, err := openRates(ctx, q)
	if err != nil {
		return nil, err
	}
	byKey := make(map[string][]openRate, len(open))
	for _, rate := range open {
		byKey[rate.key()] = append(byKey[rate.key()], rate)
	}

	now := fetchedAt
	if now.IsZero() {
		now = i.now()
	}
	changes := make([]Change, 0, len(rows))
	for _, row := range rows {
		current := currentOpen(byKey[row.Jurisdiction()])
		row.EffectiveFrom = resolveEffectiveFrom(row.EffectiveFrom, current, now)
		change := Change{
			Line:         row.Line,
			Jurisdiction: row.Jurisdiction(),
			Proposed:     row,
		}
		if current != nil {
			change.Current = &Current{
				ID:            current.ID,
				Rate:          current.Rate,
				Inclusive:     current.Inclusive,
				EffectiveFrom: current.EffectiveFrom,
				Origin:        current.Origin,
				Source:        current.Source,
				FetchedAt:     current.FetchedAt,
			}
		}

		switch {
		case current == nil:
			change.Outcome = OutcomeCreate
		case !current.Enabled:
			// The operator switched this jurisdiction off. An import that re-enabled it would undo
			// a decision nobody asked it to revisit.
			change.Outcome = OutcomeProtected
			change.Reason = "this jurisdiction's open rate is disabled; an import does not re-enable a rate an operator switched off"
		case sameRate(*current, row):
			change.Outcome = OutcomeUnchanged
		case current.Origin == OriginManual && !overwriteManual:
			change.Outcome = OutcomeProtected
			change.Reason = "this jurisdiction's open rate was entered by an operator; an import replaces it only when overwrite_manual is set"
		default:
			change.Outcome = OutcomeUpdate
		}
		changes = append(changes, change)
	}
	return changes, nil
}

// currentOpen picks the rate a change would act on: the enabled open row if there is one,
// otherwise a disabled open row (so the caller can report the protection rather than silently
// inserting a second open row that shadows it).
func currentOpen(rates []openRate) *openRate {
	var disabled *openRate
	for index := range rates {
		rate := rates[index]
		if rate.Enabled {
			return &rates[index]
		}
		if disabled == nil {
			disabled = &rates[index]
		}
	}
	return disabled
}

// sameRate reports whether the table already says what the document says.
func sameRate(current openRate, row Row) bool {
	if current.Inclusive != row.Inclusive {
		return false
	}
	left, err := money.ParseRatio(current.Rate)
	if err != nil {
		return false
	}
	right, err := money.ParseRatio(row.Rate)
	if err != nil {
		return false
	}
	return left.Rat().Cmp(right.Rat()) == 0
}

// resolveEffectiveFrom decides when the incoming rate starts.
//
// A document states the date a rate applies from, and the table's rows are ordered by that date.
// The two rules:
//
//   - A row with no date starts when the import runs. A document that omits the column is saying
//     "these are today's rates", which is the common case.
//   - A row cannot start before the rate it replaces. If the document's date is not after the
//     current row's start, the new row starts now — or one second after the current row, when the
//     current row is itself dated in the future. Backdating an import would rewrite the rate that
//     applied to invoices already issued, which is exactly what the dated table exists to prevent.
func resolveEffectiveFrom(from time.Time, current *openRate, now time.Time) time.Time {
	if from.IsZero() {
		from = now
	}
	if current == nil || from.After(current.EffectiveFrom) {
		return from
	}
	if now.After(current.EffectiveFrom) {
		return now
	}
	return current.EffectiveFrom.Add(time.Second)
}

// openRates reads every rate that is still open, with its provenance.
func openRates(ctx context.Context, q querier) ([]openRate, error) {
	rows, err := q.Query(ctx, `
		SELECT id, country_code, region_code, tax_class, rate, inclusive, effective_from,
		       enabled, origin, source, fetched_at
		  FROM tax_rates
		 WHERE effective_to IS NULL
		 ORDER BY effective_from DESC`)
	if err != nil {
		return nil, fmt.Errorf("tax: list open rates: %w", err)
	}
	defer rows.Close()
	out := make([]openRate, 0, 32)
	for rows.Next() {
		var rate openRate
		if err := rows.Scan(&rate.ID, &rate.CountryCode, &rate.RegionCode, &rate.TaxClass,
			&rate.Rate, &rate.Inclusive, &rate.EffectiveFrom, &rate.Enabled, &rate.Origin,
			&rate.Source, &rate.FetchedAt); err != nil {
			return nil, fmt.Errorf("tax: scan open rate: %w", err)
		}
		out = append(out, rate)
	}
	return out, rows.Err()
}

// buildPreview folds the changes and the parse-time refusals into one report.
func buildPreview(payload fetchPayload, changes []Change, rejections []Rejection) Preview {
	preview := Preview{
		SourceURL:  payload.URL,
		Format:     payload.Format,
		FetchedAt:  payload.FetchedAt,
		Changes:    changes,
		Rejections: rejections,
	}
	if preview.Changes == nil {
		preview.Changes = []Change{}
	}
	if preview.Rejections == nil {
		preview.Rejections = []Rejection{}
	}
	for _, change := range changes {
		switch change.Outcome {
		case OutcomeCreate:
			preview.Creates++
		case OutcomeUpdate:
			preview.Updates++
		case OutcomeUnchanged:
			preview.Unchanged++
		case OutcomeProtected:
			preview.Protected++
		case OutcomeRejected:
			preview.Rejected++
		}
	}
	preview.Rejected += len(rejections)
	preview.Rows = len(changes) + len(rejections)
	return preview
}

// counts is the run's headline, derived from the report rather than counted twice.
func (p Preview) counts() Counts {
	return Counts{
		Rows:      p.Rows,
		Created:   p.Creates,
		Updated:   p.Updates,
		Unchanged: p.Unchanged,
		Protected: p.Protected,
		Rejected:  p.Rejected,
	}
}
