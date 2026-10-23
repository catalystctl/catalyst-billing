package main

// `billing admin import` (OPS-10).
//
// The importer has had an API for a while and no command-line entry point, which left the one
// migration an operator is most likely to run without a browser — a fresh installation taking over
// from WHMCS — dependent on the panel being up. It also left the *batch* shape unserved: the HTTP
// route caps its body at 1 MiB, which is roughly ten thousand rows, so a large history has to be
// split. A directory does not have that limit.
//
// # The rules this command keeps
//
//   - **A dry run is the default.** `--commit` is required to write anything, exactly as the API's
//     `commit` field defaults to false. A command that writes because somebody forgot a flag is a
//     command that creates a customer database by accident.
//   - **The report is printed, rejections included.** Not a count: the file, the row, the legacy id
//     and the reason, because the operator's next step is to fix the export and run it again.
//   - **It goes through the same package the API uses.** `importer.Run` is the whole
//     implementation; this file collects documents into an Input and prints the result. A CLI that
//     re-implemented the rules would be a second importer, and the one that drifts is the one
//     nobody tested.
//   - **Who ran it.** The actor is resolved the same way `setting set` resolves one: `--as` names a
//     user, whose `import:write` permission is checked because the panel would refuse the request
//     without it; without `--as` the run is recorded as a system action naming the OS user and host.
//
// Usage:
//
//	billing admin import --source whmcs --dir ./dump [--commit] \
//	    [--default-currency USD] [--as <operator email or id>]

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/catalyst/billing/internal/authn"
	"github.com/catalyst/billing/internal/importer"
	"github.com/catalyst/billing/internal/store"
)

func adminImport(args []string) error {
	flags, positional, err := splitFlags(args,
		[]string{"source", "dir", "default-currency", "as"}, []string{"commit"})
	if err != nil {
		return err
	}
	if len(positional) != 0 {
		return errors.New("usage: billing admin import --source whmcs|paymenter --dir <path> " +
			"[--commit] [--default-currency USD] [--as <operator email or id>]")
	}

	source := strings.ToLower(strings.TrimSpace(flags["source"]))
	if source == "" {
		return errors.New("--source is required (one of: " + strings.Join(importer.Sources(), ", ") + ")")
	}
	dir := strings.TrimSpace(flags["dir"])
	if dir == "" {
		return errors.New("--dir is required: the directory holding one CSV per document")
	}

	files, err := readDumpDir(dir)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return fmt.Errorf("no documents found in %s: expected files named %s (.csv is optional)",
			dir, strings.Join(append(append([]string{}, importer.Files...), importer.OptionalFiles...), ", "))
	}

	// The documents a run needs are checked by the importer itself, which is where the rule lives:
	// a missing required file is `ErrMissingFile` and an unknown name is `ErrInvalidInput`, both
	// before a connection is opened.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	pool, err := store.Open(ctx, store.ConfigFromEnv())
	if err != nil {
		return err
	}
	defer pool.Close()

	in := importer.Input{
		Source:          source,
		Commit:          flags["commit"] == "true",
		DefaultCurrency: strings.ToUpper(strings.TrimSpace(flags["default-currency"])),
		Files:           files,
	}
	if !in.Commit {
		// Said before the run rather than after it: an operator who reads the report and thinks it
		// was written would re-run with --commit and import everything twice (the mappings make
		// that safe, but they should not have to find out that way).
		fmt.Println("dry run: nothing will be written (pass --commit to import)")
	}

	// The operator is resolved the way every other CLI write resolves one: `--as` names a user and
	// their permission is checked, because the panel would answer 403; without `--as` the run is a
	// system action recording the OS user and host.
	{
		operator, err := resolveCLIOperator(ctx, pool, flags["as"], authn.PermImportWrite)
		if err != nil {
			return err
		}
		in.ActorID = operator.userID
		in.Auditor = cliAuditor{actor: operator.actor, operator: operator.label}
	}

	report, err := importer.Run(ctx, pool, in)
	// A run that failed still has a report worth printing: it says which file it stopped on.
	printImportReport(report)
	if err != nil {
		return fmt.Errorf("import failed: %w", err)
	}
	if !in.Commit {
		fmt.Println("nothing was written; run the same command with --commit to apply it")
	} else {
		fmt.Printf("run %s recorded\n", report.RunID)
	}
	return nil
}

// readDumpDir loads one CSV per document name from dir.
//
// The file may be named `<document>.csv` or `<document>`: exports come in both shapes and the name
// is the only thing that connects a file to a rule. A file whose base name is not a document this
// build reads is ignored *with a notice*, because a directory of exports often contains notes and
// nothing is gained by refusing it — unlike the API, where an unknown document is a caller error.
func readDumpDir(dir string) (map[string]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", dir, err)
	}
	known := map[string]bool{}
	for _, name := range append(append([]string{}, importer.Files...), importer.OptionalFiles...) {
		known[name] = true
	}

	files := map[string]string{}
	var ignored []string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		base := entry.Name()
		if strings.HasPrefix(base, ".") {
			continue
		}
		doc := strings.TrimSuffix(strings.TrimSuffix(base, ".csv"), ".CSV")
		doc = strings.ToLower(doc)
		if !known[doc] {
			ignored = append(ignored, base)
			continue
		}
		body, err := os.ReadFile(filepath.Join(dir, base))
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", base, err)
		}
		if _, dup := files[doc]; dup {
			return nil, fmt.Errorf("%s appears twice (%s.csv and %s); keep one", doc, doc, base)
		}
		files[doc] = string(body)
	}
	if len(ignored) > 0 {
		sort.Strings(ignored)
		fmt.Fprintf(os.Stderr, "ignoring %d file(s) that are not documents this importer reads: %s\n",
			len(ignored), strings.Join(ignored, ", "))
	}
	return files, nil
}

// printImportReport writes the report the way an operator reads it: the totals, then what was
// refused and what needs attention, each with the file and row that produced it.
func printImportReport(report importer.Report) {
	label := ""
	if report.DryRun {
		label = " (dry run)"
	}
	fmt.Printf("%s import%s\n", report.Source, label)
	fmt.Printf("  read %d row(s): %d created, %d updated, %d unchanged, %d rejected (in %dms)\n",
		report.Totals.RowsRead, report.Totals.Created, report.Totals.Updated,
		report.Totals.Unchanged, report.Totals.Rejected, report.DurationMS)
	for _, file := range report.Files {
		if file.RowsRead == 0 && file.Rejected == 0 && file.Error == "" {
			continue
		}
		fmt.Printf("  %-14s %4d row(s), %d rejected", file.File, file.RowsRead, file.Rejected)
		if file.Error != "" {
			fmt.Printf(", unreadable: %s", file.Error)
		}
		fmt.Println()
	}
	if len(report.Rejections) > 0 {
		fmt.Println("rejected rows:")
		for _, r := range report.Rejections {
			fmt.Printf("  %s row %d (line %d) %s: %s\n", r.File, r.Row, r.Line, r.Code, r.Detail)
		}
	}
	if len(report.Warnings) > 0 {
		fmt.Println("warnings:")
		for _, w := range report.Warnings {
			where := w.File
			if w.Row > 0 {
				where = fmt.Sprintf("%s row %d", w.File, w.Row)
			}
			fmt.Printf("  %s: %s\n", where, w.Detail)
		}
	}
}
