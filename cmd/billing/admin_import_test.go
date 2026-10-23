package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// `billing admin import` (OPS-10).
//
// The command is the batch entry point for a migration, and the two things it must not do are the
// two things these tests assert: write anything without `--commit`, and silently ignore a document
// it did not recognise. The rest of the importer is tested in internal/importer against the same
// database; what lives here is the directory and the flag handling.

// writeDump writes a minimal dump directory and returns its path.
func writeDump(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return dir
}

// minDump is the five required documents with headers only.
func minDump() map[string]string {
	return map[string]string{
		"clients.csv":       "userid,email,datecreated\n1,cli@example.test,2021-01-01\n",
		"invoices.csv":      "invoiceid,userid,invoicenum,status,total,currency,date\n",
		"invoice_items.csv": "id,invoiceid,description,amount\n",
		"services.csv":      "id,userid,package,domainstatus\n",
		"transactions.csv":  "id,transid,gateway,amount\n",
	}
}

// A dry run reads the directory, reports the row, and writes nothing.
func TestAdminImportDryRunWritesNothing(t *testing.T) {
	pool := cliEnv(t)
	dir := writeDump(t, minDump())

	out, err := captureStdout(t, func() error {
		return cmdAdmin([]string{"import", "--source", "whmcs", "--dir", dir})
	})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if !strings.Contains(out, "dry run") {
		t.Fatalf("output = %q, want it to say nothing was written", out)
	}
	if !strings.Contains(out, "1 row(s)") {
		t.Fatalf("output = %q, want the row count", out)
	}

	var customers int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM customers`).Scan(&customers); err != nil {
		t.Fatalf("count customers: %v", err)
	}
	if customers != 0 {
		t.Fatalf("%d customer(s) were written by a dry run", customers)
	}
}

// `--commit` writes, and the run is recorded with the report.
func TestAdminImportCommitWrites(t *testing.T) {
	pool := cliEnv(t)
	dir := writeDump(t, minDump())

	out, err := captureStdout(t, func() error {
		return cmdAdmin([]string{"import", "--source", "whmcs", "--dir", dir, "--commit"})
	})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if strings.Contains(out, "dry run") {
		t.Fatalf("output = %q, want a committed run", out)
	}
	if !strings.Contains(out, "run ") || !strings.Contains(out, "recorded") {
		t.Fatalf("output = %q, want the run id", out)
	}

	var (
		customers int
		runs      int
	)
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM customers`).Scan(&customers); err != nil {
		t.Fatal(err)
	}
	if customers != 1 {
		t.Fatalf("%d customers, want 1", customers)
	}
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM import_runs`).Scan(&runs); err != nil {
		t.Fatal(err)
	}
	if runs != 1 {
		t.Fatalf("%d import runs recorded, want 1", runs)
	}
}

// The directory may carry optional documents under their own names, and a file that is not a
// document is ignored with a notice rather than silently or fatally.
func TestAdminImportReadsOptionalDocumentsAndIgnoresNotes(t *testing.T) {
	cliEnv(t)
	files := minDump()
	files["currencies.csv"] = "code,name,symbol,scale\nZZC,Charlie,!,2\n"
	files["README.txt"] = "export notes, not a document\n"
	dir := writeDump(t, files)

	out, err := captureStdout(t, func() error {
		return cmdAdmin([]string{"import", "--source", "whmcs", "--dir", dir})
	})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if !strings.Contains(out, "currencies") {
		t.Fatalf("output = %q, want the currencies document in the report", out)
	}
}

// A missing required document is refused before anything is read.
func TestAdminImportRequiresEveryDocument(t *testing.T) {
	cliEnv(t)
	files := minDump()
	delete(files, "transactions.csv")
	dir := writeDump(t, files)

	_, err := captureStdout(t, func() error {
		return cmdAdmin([]string{"import", "--source", "whmcs", "--dir", dir})
	})
	if err == nil {
		t.Fatal("a dump without transactions was accepted")
	}
	if !strings.Contains(err.Error(), "transactions") {
		t.Fatalf("error = %v, want it to name the missing document", err)
	}
}
