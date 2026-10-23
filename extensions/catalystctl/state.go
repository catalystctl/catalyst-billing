package main

// The ledger: what this adapter has done, on disk.
//
// # Why a provisioner needs one here
//
// The protocol's hardest requirement is `provisioner.lookup`: "does a resource already exist for
// this operation key?". It exists because a create that times out may or may not have created
// anything, and the core's reconciliation re-asks rather than re-does. Pterodactyl can answer it
// from its own database, because it stores an `external_id` on every server. Catalyst does not:
// a server has a name, a description and an id, and nothing that a create request can set as a
// foreign key.
//
// So this adapter answers the question two ways, in this order:
//
//  1. **The ledger.** The operation key is written to disk *before* the create request is sent,
//     carrying the server id as soon as one exists. This is the fast, exact answer and it survives
//     the process restarting, which is the whole point.
//  2. **The description marker.** A created server carries `billing:<operation_key>` in its
//     description, so a scan of the panel can re-establish the association after the ledger is
//     lost. The scan is bounded, and running out of pages is an *error*, never "not found": the
//     core treats "not found" as authoritative and would provision a second server.
//
// # What this file is not
//
// Not a cache. A ledger entry is a record of intent and outcome, and it is never deleted: an
// operator asking "what happened to this key" deserves an answer after the resource was
// terminated. Only the panel has the final word on whether the server still exists, and every
// lookup that has a recorded id re-reads the panel to confirm it before claiming found.

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

// Ledger record states, in the order they happen.
const (
	// recordCreating means the intent is written and the create request may or may not have
	// reached the panel. A lookup that finds this state must scan, because the id is unknown.
	recordCreating = "creating"
	// recordCreated means the panel returned a server for this key.
	recordCreated = "created"
	// recordTerminated means the resource was destroyed. The record stays so the key remains
	// answerable, but a lookup reports found=false with the id attached for the audit trail.
	recordTerminated = "terminated"
)

// operationRecord is what the adapter remembers about one operation key.
type operationRecord struct {
	OperationKey string `json:"operation_key"`
	// ExternalID is the panel's server id. Empty while the create is in flight.
	ExternalID string `json:"external_id,omitempty"`
	// Name is the server name that was requested, which is also the second fallback for a
	// post-restart reconciliation when the description was edited by a customer.
	Name string `json:"name,omitempty"`
	// PlanID is the billing panel's plan id. The panel has nowhere to store it.
	PlanID string `json:"plan_id,omitempty"`
	Region string `json:"region,omitempty"`
	// State is one of the three above.
	State     string    `json:"state"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// ledgerFile is the on-disk shape. It is versioned so a future change can migrate rather than
// guess.
type ledgerFile struct {
	Version    int                        `json:"version"`
	Operations map[string]operationRecord `json:"operations"`
}

const ledgerVersion = 1

// ledger is the durable map from operation key to what happened.
type ledger struct {
	path string

	mu sync.Mutex
	// loadErr is sticky: once the file cannot be understood, every call reports it rather than
	// pretending the ledger is empty. An empty ledger silently turns "I already created this"
	// into "create another one".
	loadErr error
	loaded  bool
	file    ledgerFile
}

func newLedger(path string) *ledger {
	return &ledger{path: path, file: ledgerFile{Version: ledgerVersion, Operations: map[string]operationRecord{}}}
}

// load reads the file once. A missing file is an empty ledger, not an error: that is the state of
// a fresh installation.
func (l *ledger) load() error {
	if l.loaded {
		return l.loadErr
	}
	l.loaded = true
	raw, err := os.ReadFile(l.path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		l.loadErr = fmt.Errorf("catalystctl: read the operation ledger %s: %w", l.path, err)
		return l.loadErr
	}
	var parsed ledgerFile
	if err := json.Unmarshal(raw, &parsed); err != nil {
		l.loadErr = fmt.Errorf("catalystctl: the operation ledger %s is not valid JSON: %w", l.path, err)
		return l.loadErr
	}
	if parsed.Operations == nil {
		parsed.Operations = map[string]operationRecord{}
	}
	if parsed.Version == 0 {
		parsed.Version = ledgerVersion
	}
	l.file = parsed
	return nil
}

// get returns the record for a key.
func (l *ledger) get(key string) (operationRecord, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.load(); err != nil {
		return operationRecord{}, false, err
	}
	rec, ok := l.file.Operations[key]
	return rec, ok, nil
}

// byExternalID finds the newest record that names a panel server id.
//
// The ledger is keyed by operation key, but inspect and terminate arrive with only the server id,
// so the reverse direction is needed to answer "what plan was this sold as". A linear scan is fine:
// the map holds one entry per create this installation ever made, which is thousands at worst, and
// this is called once per operator or customer action, not per event.
func (l *ledger) byExternalID(externalID string) (operationRecord, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.load(); err != nil {
		return operationRecord{}, false, err
	}
	var best operationRecord
	found := false
	for _, rec := range l.file.Operations {
		if rec.ExternalID != externalID || externalID == "" {
			continue
		}
		if !found || rec.UpdatedAt.After(best.UpdatedAt) {
			best, found = rec, true
		}
	}
	return best, found, nil
}

// put writes or replaces one record.
//
// The write is atomic (temporary file plus rename) and it happens *before* the caller acts, which
// is what makes the record an intent rather than a report. A create whose ledger write fails is
// refused before any request is sent: an adapter that cannot write down what it is about to do
// must not do it.
func (l *ledger) put(rec operationRecord) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.load(); err != nil {
		return err
	}
	if prior, ok := l.file.Operations[rec.OperationKey]; ok {
		rec.CreatedAt = prior.CreatedAt
	}
	if rec.CreatedAt.IsZero() {
		rec.CreatedAt = time.Now().UTC()
	}
	rec.UpdatedAt = time.Now().UTC()
	l.file.Operations[rec.OperationKey] = rec
	l.file.Version = ledgerVersion
	return l.flushLocked()
}

// flushLocked writes the file by rename, so a crash mid-write leaves the previous contents intact
// rather than a truncated file that a later load would refuse.
func (l *ledger) flushLocked() error {
	dir := filepath.Dir(l.path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("catalystctl: create the ledger directory %s: %w", dir, err)
	}
	raw, err := json.MarshalIndent(l.file, "", "  ")
	if err != nil {
		return fmt.Errorf("catalystctl: encode the operation ledger: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".ledger-*")
	if err != nil {
		return fmt.Errorf("catalystctl: write the operation ledger: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(append(raw, '\n')); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("catalystctl: write the operation ledger: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("catalystctl: flush the operation ledger: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("catalystctl: close the operation ledger: %w", err)
	}
	if err := os.Rename(tmpName, l.path); err != nil {
		return fmt.Errorf("catalystctl: install the operation ledger: %w", err)
	}
	return nil
}

// markerEnvPrefix is the description prefix that ties a server back to an operation key.
//
// It is a visible marker on a customer-visible field, which is a deliberate trade: the customer
// can read it and can delete it, and the ledger is the primary record precisely so that deleting
// the marker degrades reconciliation to "scan or refuse" rather than "create a duplicate".
const markerEnvPrefix = "billing:"

func markerFor(key string) string { return markerEnvPrefix + key }

// keyInDescription reports whether a description carries the marker for key.
func keyInDescription(desc, key string) bool {
	return key != "" && desc == markerFor(key)
}
