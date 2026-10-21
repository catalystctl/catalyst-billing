// Package store owns database access: connection setup, the migration runner,
// and the repositories that implement the domain's ports.
//
// The design rules that matter:
//
//   - SQL is explicit. There is no ORM and no generic repository abstraction:
//     a reviewer must be able to read the exact statement that moves money.
//   - Migrations are embedded in the binary and applied in lexical order inside
//     one transaction each, recorded in schema_migrations with a checksum. An
//     edited migration is a startup error, not a silent divergence between
//     environments.
//   - Two credentials are supported. The runtime pool should use a role without
//     DDL rights; only the migrate subcommand needs the owner role.
package store

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// ErrMigrationDrift is returned when a migration file that was already applied
// has changed. Deployed schemas must be reproducible from the file set.
var ErrMigrationDrift = errors.New("store: applied migration file has changed")

// Migration is one embedded schema change.
type Migration struct {
	// Version is the numeric prefix of the file name, e.g. 1 for 0001_core.sql.
	Version int
	Name    string
	SQL     string
	// Checksum is the SHA-256 of the SQL body, recorded at apply time.
	Checksum string
}

// LoadMigrations reads the embedded migration set, sorted by version.
func LoadMigrations() ([]Migration, error) {
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		return nil, fmt.Errorf("store: read migrations: %w", err)
	}
	var out []Migration
	seen := make(map[int]string, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		name := e.Name()
		var version int
		if _, err := fmt.Sscanf(name, "%04d_", &version); err != nil {
			return nil, fmt.Errorf("store: migration %q must start with a 4-digit version", name)
		}
		if version <= 0 {
			return nil, fmt.Errorf("store: migration %q has a non-positive version", name)
		}
		if other, dup := seen[version]; dup {
			return nil, fmt.Errorf("store: duplicate migration version %d (%q and %q)", version, other, name)
		}
		seen[version] = name

		body, err := migrationFS.ReadFile("migrations/" + name)
		if err != nil {
			return nil, fmt.Errorf("store: read %s: %w", name, err)
		}
		sum := sha256.Sum256(body)
		out = append(out, Migration{
			Version:  version,
			Name:     name,
			SQL:      string(body),
			Checksum: hex.EncodeToString(sum[:]),
		})
	}
	if len(out) == 0 {
		return nil, errors.New("store: no migrations embedded")
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	for i := 1; i < len(out); i++ {
		if out[i].Version == out[i-1].Version+1 {
			continue
		}
		// Gaps are allowed but suspicious; keep them visible in the error only
		// when the ordering is genuinely broken.
		if out[i].Version < out[i-1].Version {
			return nil, fmt.Errorf("store: migrations out of order at %s", out[i].Name)
		}
	}
	return out, nil
}

// Migrator applies migrations to one database.
type Migrator struct {
	pool *pgxpool.Pool
	// LockTimeout bounds how long to wait for the advisory migration lock. Two
	// processes starting at once must not interleave DDL.
	LockTimeout time.Duration
	// Logger receives one line per applied migration. Optional.
	Logger func(format string, args ...any)
}

// NewMigrator constructs a Migrator.
func NewMigrator(pool *pgxpool.Pool) *Migrator {
	return &Migrator{pool: pool, LockTimeout: 60 * time.Second}
}

func (m *Migrator) logf(format string, args ...any) {
	if m.Logger != nil {
		m.Logger(format, args...)
	}
}

// migrationLockID is an arbitrary but fixed advisory lock key. It is unique to
// this application so it cannot collide with another tool's lock.
const migrationLockID int64 = 8_421_196_317_004_211

// Applied returns the versions already recorded, keyed by version.
//
// It is strictly read-only: a runtime role is not allowed to create tables, and a
// readiness check that needs DDL rights would report a healthy database as broken.
// A missing schema_migrations table therefore means "nothing applied yet", not an
// error.
func (m *Migrator) Applied(ctx context.Context) (map[int]string, error) {
	rows, err := m.pool.Query(ctx, `SELECT version, checksum FROM schema_migrations`)
	if err != nil {
		if SQLState(err) == stateUndefinedTable {
			return map[int]string{}, nil
		}
		return nil, fmt.Errorf("store: read schema_migrations: %w", err)
	}
	defer rows.Close()
	out := make(map[int]string)
	for rows.Next() {
		var v int
		var sum string
		if err := rows.Scan(&v, &sum); err != nil {
			return nil, fmt.Errorf("store: scan schema_migrations: %w", err)
		}
		out[v] = sum
	}
	return out, rows.Err()
}

func (m *Migrator) ensureTable(ctx context.Context) error {
	_, err := m.pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version     int         PRIMARY KEY,
			name        text        NOT NULL,
			checksum    text        NOT NULL,
			applied_at  timestamptz NOT NULL DEFAULT now()
		)`)
	if err != nil {
		return fmt.Errorf("store: create schema_migrations: %w", err)
	}
	return nil
}

// Up applies every pending migration. It is safe to run concurrently: a
// session-level advisory lock serialises runners, and the applied set is
// re-read under that lock.
func (m *Migrator) Up(ctx context.Context) (applied []Migration, err error) {
	migrations, err := LoadMigrations()
	if err != nil {
		return nil, err
	}

	// The advisory lock needs a single dedicated connection.
	conn, err := m.pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: acquire migration connection: %w", err)
	}
	defer conn.Release()

	lockCtx := ctx
	if m.LockTimeout > 0 {
		var cancel context.CancelFunc
		lockCtx, cancel = context.WithTimeout(ctx, m.LockTimeout)
		defer cancel()
	}
	if _, err := conn.Exec(lockCtx, `SELECT pg_advisory_lock($1)`, migrationLockID); err != nil {
		return nil, fmt.Errorf("store: acquire migration lock: %w", err)
	}
	defer func() {
		// Use a fresh context: the caller's may already be cancelled, and a
		// leaked session lock would block every later deploy.
		unlockCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if _, uerr := conn.Exec(unlockCtx, `SELECT pg_advisory_unlock($1)`, migrationLockID); uerr != nil && err == nil {
			err = fmt.Errorf("store: release migration lock: %w", uerr)
		}
	}()

	if err := m.ensureTable(ctx); err != nil {
		return nil, err
	}
	done, err := m.appliedOn(ctx, conn.Conn())
	if err != nil {
		return nil, err
	}

	for _, mig := range migrations {
		if checksum, ok := done[mig.Version]; ok {
			if checksum != mig.Checksum {
				return applied, fmt.Errorf("%w: %s (recorded %s, file %s)",
					ErrMigrationDrift, mig.Name, short(checksum), short(mig.Checksum))
			}
			continue
		}
		if err := m.apply(ctx, conn.Conn(), mig); err != nil {
			return applied, err
		}
		applied = append(applied, mig)
		m.logf("applied migration %s", mig.Name)
	}
	return applied, nil
}

func (m *Migrator) appliedOn(ctx context.Context, conn *pgx.Conn) (map[int]string, error) {
	rows, err := conn.Query(ctx, `SELECT version, checksum FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("store: read schema_migrations: %w", err)
	}
	defer rows.Close()
	out := make(map[int]string)
	for rows.Next() {
		var v int
		var sum string
		if err := rows.Scan(&v, &sum); err != nil {
			return nil, fmt.Errorf("store: scan schema_migrations: %w", err)
		}
		out[v] = sum
	}
	return out, rows.Err()
}

// apply runs one migration in its own transaction. PostgreSQL has transactional
// DDL, so a failing migration leaves no partial schema behind.
func (m *Migrator) apply(ctx context.Context, conn *pgx.Conn, mig Migration) error {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: begin %s: %w", mig.Name, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, mig.SQL); err != nil {
		return fmt.Errorf("store: apply %s: %w", mig.Name, err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO schema_migrations (version, name, checksum) VALUES ($1, $2, $3)`,
		mig.Version, mig.Name, mig.Checksum,
	); err != nil {
		return fmt.Errorf("store: record %s: %w", mig.Name, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: commit %s: %w", mig.Name, err)
	}
	return nil
}

// Pending returns the migrations that would be applied by Up.
func (m *Migrator) Pending(ctx context.Context) ([]Migration, error) {
	done, err := m.Applied(ctx)
	if err != nil {
		return nil, err
	}
	migrations, err := LoadMigrations()
	if err != nil {
		return nil, err
	}
	var out []Migration
	for _, mig := range migrations {
		if _, ok := done[mig.Version]; !ok {
			out = append(out, mig)
		}
	}
	return out, nil
}

func short(sum string) string {
	if len(sum) <= 12 {
		return sum
	}
	return sum[:12]
}
