package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/catalyst/billing/internal/events"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNoDatabaseURL is returned when no connection string is configured.
var ErrNoDatabaseURL = errors.New("store: no database URL configured (set BILLING_DATABASE_URL)")

// Config is the database configuration read from the environment.
type Config struct {
	// URL is the connection string used by the runtime.
	URL string
	// MigrationURL, when set, is used only by the migrate subcommand. It may
	// carry a role that is allowed to change the schema; the runtime role
	// should not be.
	MigrationURL string
	// MaxConns bounds the pool. Zero uses pgxpool's default, which is derived
	// from the number of CPUs; production deployments should set it explicitly
	// from the PostgreSQL connection budget.
	MaxConns int32
	// ConnectTimeout bounds the initial connection attempt.
	ConnectTimeout time.Duration
	// StatementTimeout bounds any single statement, so a pathological query
	// cannot pin a connection indefinitely.
	StatementTimeout time.Duration
}

// ConfigFromEnv reads the database configuration.
func ConfigFromEnv() Config {
	cfg := Config{
		URL:              os.Getenv("BILLING_DATABASE_URL"),
		MigrationURL:     os.Getenv("BILLING_MIGRATION_DATABASE_URL"),
		ConnectTimeout:   10 * time.Second,
		StatementTimeout: 30 * time.Second,
	}
	if v := os.Getenv("BILLING_DB_MAX_CONNS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.MaxConns = int32(n)
		}
	}
	if cfg.MigrationURL == "" {
		cfg.MigrationURL = cfg.URL
	}
	return cfg
}

// Validate checks that a connection string is present.
func (c Config) Validate() error {
	if strings.TrimSpace(c.URL) == "" {
		return ErrNoDatabaseURL
	}
	return nil
}

// Pool wraps pgxpool.Pool with the panel's transaction helper.
type Pool struct {
	*pgxpool.Pool
	statementTimeout time.Duration
}

// Open creates the runtime pool. It verifies connectivity before returning, so
// a misconfigured deployment fails at startup rather than on first request.
func Open(ctx context.Context, cfg Config) (*Pool, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	poolCfg, err := pgxpool.ParseConfig(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("store: parse database URL: %w", err)
	}
	if cfg.MaxConns > 0 {
		poolCfg.MaxConns = cfg.MaxConns
	}
	if cfg.ConnectTimeout > 0 {
		poolCfg.ConnConfig.ConnectTimeout = cfg.ConnectTimeout
	}
	// Fail fast on a broken connection instead of queueing behind it.
	poolCfg.MaxConnIdleTime = 5 * time.Minute
	poolCfg.MaxConnLifetime = time.Hour
	poolCfg.HealthCheckPeriod = 30 * time.Second

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("store: create pool: %w", err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, cfg.ConnectTimeout+5*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("store: ping database: %w", err)
	}
	return &Pool{Pool: pool, statementTimeout: cfg.StatementTimeout}, nil
}

// OpenMigrator opens a pool for schema changes.
func OpenMigrator(ctx context.Context, cfg Config) (*Pool, error) {
	cfg.URL = cfg.MigrationURL
	return Open(ctx, cfg)
}

// Migrator returns a Migrator bound to this pool.
func (p *Pool) Migrator() *Migrator { return NewMigrator(p.Pool) }

// InTx runs fn inside a transaction, committing on nil and rolling back on
// error or panic. The statement timeout is applied per transaction so a long
// batch job can raise it deliberately at its call site.
func (p *Pool) InTx(ctx context.Context, fn func(ctx context.Context, tx pgx.Tx) error) error {
	return p.inTx(ctx, p.statementTimeout, fn)
}

// InTxWithTimeout is InTx with an explicit statement timeout.
func (p *Pool) InTxWithTimeout(ctx context.Context, timeout time.Duration, fn func(ctx context.Context, tx pgx.Tx) error) error {
	return p.inTx(ctx, timeout, fn)
}

func (p *Pool) inTx(ctx context.Context, timeout time.Duration, fn func(ctx context.Context, tx pgx.Tx) error) (err error) {
	// A transaction is where a change becomes a fact, so it is also where an event has to wait
	// until the fact is true. The buffer is attached here, and announced only after Commit
	// returned nil — never in a defer, which runs on the rollback path too. A caller that does
	// not defer an event is unaffected: the buffer stays empty and flush is a loop over nothing.
	ctx, flush := events.WithPending(ctx, events.Default())

	tx, err := p.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("store: begin transaction: %w", err)
	}
	defer func() {
		if r := recover(); r != nil {
			_ = tx.Rollback(context.WithoutCancel(ctx))
			panic(r)
		}
		if err != nil {
			// Roll back with a live context: the caller's may be cancelled, and
			// a connection returned to the pool mid-transaction is a hazard.
			_ = tx.Rollback(context.WithoutCancel(ctx))
		}
	}()

	if timeout > 0 {
		if _, err = tx.Exec(ctx, fmt.Sprintf(`SET LOCAL statement_timeout = %d`, timeout.Milliseconds())); err != nil {
			return fmt.Errorf("store: set statement timeout: %w", err)
		}
	}
	if err = fn(ctx, tx); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: commit transaction: %w", err)
	}
	// After the commit, and only then: these events describe rows that now exist.
	flush()
	return nil
}

// Error classification helpers. Callers branch on these rather than on error
// strings, so a PostgreSQL message change cannot alter control flow.

// SQLState returns the PostgreSQL SQLSTATE of err, or "" if it is not a
// database error.
func SQLState(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

const (
	// StateUniqueViolation is SQLSTATE 23505.
	StateUniqueViolation = "23505"
	// StateForeignKeyViolation is SQLSTATE 23503.
	StateForeignKeyViolation = "23503"
	// StateCheckViolation is SQLSTATE 23514.
	StateCheckViolation = "23514"
	// StateRestrictViolation is SQLSTATE 23001, raised by the append-only and
	// immutability triggers.
	StateRestrictViolation = "23001"
	// StateSerializationFailure is SQLSTATE 40001, which is retryable.
	StateSerializationFailure = "40001"
	// StateDeadlockDetected is SQLSTATE 40P01, which is retryable.
	StateDeadlockDetected = "40P01"
	// StateInsufficientPrivilege is SQLSTATE 42501.
	StateInsufficientPrivilege = "42501"
	// stateUndefinedTable is SQLSTATE 42P01: the relation does not exist.
	stateUndefinedTable = "42P01"
)

// IsUniqueViolation reports whether err is a unique-constraint violation,
// optionally on a specific constraint name.
func IsUniqueViolation(err error, constraint ...string) bool {
	if SQLState(err) != StateUniqueViolation {
		return false
	}
	if len(constraint) == 0 {
		return true
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	for _, c := range constraint {
		if pgErr.ConstraintName == c {
			return true
		}
	}
	return false
}

// IsRetryable reports whether err is a transient serialization failure or
// deadlock that the caller should retry.
func IsRetryable(err error) bool {
	switch SQLState(err) {
	case StateSerializationFailure, StateDeadlockDetected:
		return true
	default:
		return false
	}
}

// IsConstraintViolation reports whether err is any check/restrict/foreign-key
// violation.
func IsConstraintViolation(err error) bool {
	switch SQLState(err) {
	case StateCheckViolation, StateRestrictViolation, StateForeignKeyViolation:
		return true
	default:
		return false
	}
}

// Rollback is a helper for tests and cleanup paths that must not fail loudly.
func Rollback(ctx context.Context, tx pgx.Tx) {
	_ = tx.Rollback(context.WithoutCancel(ctx))
}
