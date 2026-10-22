package importer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/catalyst/billing/internal/paginate"
	"github.com/jackc/pgx/v5"
)

// Querier is the read surface the history needs. Both the pool and a transaction satisfy it,
// so the same code reads a run inside the import transaction and from the API.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// RunRecord is one recorded import.
type RunRecord struct {
	ID      string
	Source  string
	ActorID string
	// Counts are the headline numbers, duplicated from the report as columns so a query can
	// answer "how much has ever been imported" without unpacking JSON.
	Created   int
	Updated   int
	Unchanged int
	Rejected  int
	// Report is the stored report verbatim, including every rejection and warning.
	Report    Report
	CreatedAt time.Time
}

// RunList is one page of runs.
type RunList struct {
	Runs       []RunRecord
	NextCursor string
}

// ListRuns returns committed runs newest first.
//
// Dry runs are absent by construction: they write nothing, so there is nothing to list. An
// operator who wants the report of a dry run has it in the response of the request that ran
// it.
func ListRuns(ctx context.Context, q Querier, limit int, cursor string) (RunList, error) {
	limit = paginate.Clamp(limit)
	args := []any{}
	where := []string{"true"}

	sortKey, id, err := paginate.Decode(cursor)
	if err != nil {
		return RunList{}, err
	}
	if cursor != "" {
		when, err := time.Parse(time.RFC3339Nano, sortKey)
		if err != nil {
			return RunList{}, fmt.Errorf("%w: bad timestamp", paginate.ErrInvalidCursor)
		}
		args = append(args, when, id)
		where = append(where, fmt.Sprintf("(created_at, id) < ($%d, $%d)", len(args)-1, len(args)))
	}
	args = append(args, limit+1)

	rows, err := q.Query(ctx, `
		SELECT id::text, source, COALESCE(actor_id::text, ''), created, updated, unchanged,
		       rejected, report, created_at
		FROM import_runs
		WHERE `+strings.Join(where, " AND ")+`
		ORDER BY created_at DESC, id DESC
		LIMIT $`+strconv.Itoa(len(args)), args...)
	if err != nil {
		return RunList{}, fmt.Errorf("importer: list runs: %w", err)
	}
	defer rows.Close()

	out := make([]RunRecord, 0, limit)
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return RunList{}, err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return RunList{}, fmt.Errorf("importer: read runs: %w", err)
	}

	res := RunList{}
	if len(out) > limit {
		out = out[:limit]
		last := out[len(out)-1]
		res.NextCursor = paginate.Encode(last.CreatedAt.UTC().Format(time.RFC3339Nano), last.ID)
	}
	res.Runs = out
	return res, nil
}

// GetRun returns one recorded run.
func GetRun(ctx context.Context, q Querier, id string) (RunRecord, error) {
	row := q.QueryRow(ctx, `
		SELECT id::text, source, COALESCE(actor_id::text, ''), created, updated, unchanged,
		       rejected, report, created_at
		FROM import_runs WHERE id = $1`, id)
	r, err := scanRun(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return RunRecord{}, ErrNotFound
	}
	return r, err
}

// scanner is the shared shape of a row and a rows cursor.
type scanner interface {
	Scan(dest ...any) error
}

func scanRun(s scanner) (RunRecord, error) {
	var (
		r      RunRecord
		report []byte
	)
	if err := s.Scan(&r.ID, &r.Source, &r.ActorID, &r.Created, &r.Updated, &r.Unchanged,
		&r.Rejected, &report, &r.CreatedAt); err != nil {
		return RunRecord{}, err
	}
	if len(report) > 0 {
		if err := json.Unmarshal(report, &r.Report); err != nil {
			return RunRecord{}, fmt.Errorf("importer: decode run report: %w", err)
		}
	}
	return r, nil
}
