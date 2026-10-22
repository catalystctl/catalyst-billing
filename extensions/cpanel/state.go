package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// ledger is the adapter's durable record of what it has done.
//
// It is *not* the authority for provisioner.lookup — WHM is, because the account
// name is derived from the operation key (see names.go). Losing this file costs
// history, not correctness: a lookup can still be answered, and a create replay
// still returns the account that exists. What it is genuinely needed for is the
// billing panel's own plan id, which WHM has no field for: listaccts knows quota
// and bandwidth limits, not the id of the plan that was sold.
//
// Two properties make it safe to depend on:
//
//   - It is written *before* createacct is called (write-ahead), so a crash
//     between the request and the reply still leaves a record of intent rather
//     than an account nobody can attribute to an operation.
//   - It is written by rename over a temporary file, so a crash mid-write leaves
//     the previous contents intact instead of a truncated JSON file.
//
// A create whose ledger write fails is refused before any request is sent. An
// adapter that cannot write down what it is about to do must not do it.
type ledger struct {
	path string

	mu     sync.Mutex
	loaded bool
	// loadErr is sticky: once the file cannot be understood, every subsequent
	// call reports it rather than pretending the ledger is empty. An empty ledger
	// would silently make inspect under-report and a create lose its plan id.
	loadErr error
	file    ledgerFile
}

// operationRecord is what the adapter remembers about one operation key.
type operationRecord struct {
	OperationKey string `json:"operation_key"`
	// Username is the cPanel account name, which is also the external id.
	Username string `json:"username"`
	Domain   string `json:"domain"`
	// PlanID is the billing panel's plan id. WHM has nowhere to store it.
	PlanID string `json:"plan_id"`
	// State is "created" or "terminated". A record is never deleted: an operator
	// asking "what happened to this key" deserves an answer after termination too.
	State     string    `json:"state"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Ledger record states.
const (
	// ledgerPending is written before createacct is called, so a crash between the
	// request and the reply leaves a record of intent rather than an orphan account.
	ledgerPending    = "pending"
	ledgerCreated    = "created"
	ledgerTerminated = "terminated"
)

type ledgerFile struct {
	Version int                        `json:"version"`
	Records map[string]operationRecord `json:"records"`
}

func newLedger(path string) *ledger {
	return &ledger{path: path, file: ledgerFile{Version: 1, Records: map[string]operationRecord{}}}
}

// ensureLoaded reads the file once. A missing file is an empty ledger; an
// unreadable or unparsable one is an error that sticks.
func (l *ledger) ensureLoaded() error {
	if l.loaded {
		return l.loadErr
	}
	l.loaded = true
	raw, err := os.ReadFile(l.path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		l.loadErr = fmt.Errorf("cPanel operation ledger %s: %w", l.path, err)
		return l.loadErr
	}
	var parsed ledgerFile
	if err := json.Unmarshal(raw, &parsed); err != nil {
		// Deliberately not "start fresh": a corrupt ledger means we do not know
		// what this adapter has already done, and answering as if we had done
		// nothing is how a duplicate account gets created.
		l.loadErr = fmt.Errorf("cPanel operation ledger %s is not readable JSON: %w; repair or remove it deliberately", l.path, err)
		return l.loadErr
	}
	if parsed.Records == nil {
		parsed.Records = map[string]operationRecord{}
	}
	parsed.Version = 1
	l.file = parsed
	return nil
}

func (l *ledger) get(key string) (operationRecord, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.ensureLoaded(); err != nil {
		return operationRecord{}, false, err
	}
	rec, ok := l.file.Records[key]
	return rec, ok, nil
}

// byUsername finds the record for an account name. It is a scan: a ledger holds
// one record per service, which is thousands at the very most, and a scan cannot
// disagree with itself the way a second index can.
func (l *ledger) byUsername(username string) (operationRecord, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.ensureLoaded(); err != nil {
		return operationRecord{}, false, err
	}
	for _, rec := range l.file.Records {
		if rec.Username == username {
			return rec, true, nil
		}
	}
	return operationRecord{}, false, nil
}

func (l *ledger) put(rec operationRecord) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.ensureLoaded(); err != nil {
		return err
	}
	now := time.Now().UTC()
	if rec.CreatedAt.IsZero() {
		rec.CreatedAt = now
	}
	rec.UpdatedAt = now
	l.file.Records[rec.OperationKey] = rec
	return l.saveLocked()
}

// saveLocked writes the whole file. A partial write is impossible because the
// rename is atomic on every filesystem this runs on.
func (l *ledger) saveLocked() error {
	dir := filepath.Dir(l.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("cPanel operation ledger %s: create directory: %w", l.path, err)
	}
	raw, err := json.MarshalIndent(l.file, "", "  ")
	if err != nil {
		return fmt.Errorf("cPanel operation ledger %s: encode: %w", l.path, err)
	}
	tmp, err := os.CreateTemp(dir, ".operations-*.tmp")
	if err != nil {
		return fmt.Errorf("cPanel operation ledger %s: create temporary file: %w", l.path, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename
	if _, err := tmp.Write(append(raw, '\n')); err != nil {
		tmp.Close()
		return fmt.Errorf("cPanel operation ledger %s: write: %w", l.path, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("cPanel operation ledger %s: sync: %w", l.path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("cPanel operation ledger %s: close: %w", l.path, err)
	}
	if err := os.Rename(tmpName, l.path); err != nil {
		return fmt.Errorf("cPanel operation ledger %s: rename: %w", l.path, err)
	}
	return nil
}
