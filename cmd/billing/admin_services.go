package main

// `billing admin services check-remote`: which services does the configured provisioner actually know?
//
// # Why this exists
//
// Changing the installation's provider — or seeding demo data onto a real panel — leaves service rows
// pointing at resources nothing can find. From the customer side that is a single sentence, "The server
// panel does not recognise this service", which is true and useless: it does not say whether one server was
// deleted by hand or whether nine rows were fixtures from the start.
//
// Answering that by hand means a per-service curl with the panel's key, cross-referenced against the
// billing database. That is exactly the kind of thing an operator should not be doing at 02:00, and doing it
// by hand is how a *data* problem gets mistaken for a *provisioner* problem twice in a row.
//
// # What it reports
//
// One line per service, classified by what the provider said:
//
//	ok          the provider knows the resource
//	missing     the provider authoritatively does not (404 / -32004)
//	error       the provider could not be asked (auth, timeout, unreachable)
//	no-id       the service has no external id; nothing was ever provisioned
//
// The three failures are deliberately distinct. "missing" is a data repair — the row needs its resource
// re-created or its external id cleared. "error" is a deployment problem shared by every row, and treating it
// as "missing" would have an operator delete real services.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/catalyst/billing/internal/extproto"
	"github.com/catalyst/billing/internal/gatewayconfig"
	"github.com/catalyst/billing/internal/provisioning"
	"github.com/catalyst/billing/internal/store"
)

// remoteCheck is one service's verdict.
type remoteCheck struct {
	ServiceID string
	Customer  string
	Status    string
	External  string
	Verdict   string
	Detail    string
}

// adminServicesCheckRemote cross-references every provisioned service against the configured provider.
//
// Usage: billing admin services check-remote [--timeout 20s] [--status active]
func adminServicesCheckRemote(args []string) error {
	flags, positional, err := splitFlags(args, []string{"timeout", "status"}, nil)
	if err != nil {
		return err
	}
	if len(positional) != 0 {
		return errors.New("usage: billing admin services check-remote [--timeout 20s] [--status active]")
	}
	timeout := 20 * time.Second
	if raw := flags["timeout"]; raw != "" {
		parsed, err := time.ParseDuration(raw)
		if err != nil || parsed <= 0 {
			return fmt.Errorf("--timeout %q is not a positive duration", raw)
		}
		timeout = parsed
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	pool, err := store.Open(ctx, store.ConfigFromEnv())
	if err != nil {
		return err
	}
	defer pool.Close()

	cfg, err := gatewayconfig.New(pool).Load(ctx, gatewayconfig.ServerKind)
	if err != nil {
		return fmt.Errorf("read the servers configuration: %w", err)
	}
	if !cfg.Configured() {
		return errors.New("no server provider is configured; set one on the Servers screen first")
	}

	// A fixture provider is checked too, but the report says so: a "missing" verdict against `fake` means the
	// fixture was restarted and forgot, not that anything is wrong.
	fmt.Printf("provider: %s (extension: %s)\n", cfg.Provider, cfg.ExtensionPath)
	if cfg.Provider == "fake" {
		fmt.Println("note: `fake` is the conformance fixture — it holds its state in memory, so every resource")
		fmt.Println("      it ever created reports missing after a restart. Nothing below should be deleted.")
	}

	// Only services that exist as far as the provider could know are interesting; a terminally terminated
	// service has already been destroyed and reporting it as missing is noise.
	query := `SELECT s.id, COALESCE(s.external_id, ''), s.status, COALESCE(c.email, '')
	          FROM services s LEFT JOIN customers c ON c.id = s.customer_id`
	var conds []string
	var argv []any
	if status := strings.TrimSpace(flags["status"]); status != "" {
		argv = append(argv, status)
		conds = append(conds, fmt.Sprintf("s.status = $%d", len(argv)))
	}
	if len(conds) > 0 {
		query += " WHERE " + strings.Join(conds, " AND ")
	}
	query += " ORDER BY s.created_at DESC"

	rows, err := pool.Query(ctx, query, argv...)
	if err != nil {
		return fmt.Errorf("read services: %w", err)
	}
	var checks []remoteCheck
	for rows.Next() {
		var c remoteCheck
		if err := rows.Scan(&c.ServiceID, &c.External, &c.Status, &c.Customer); err != nil {
			rows.Close()
			return fmt.Errorf("scan service: %w", err)
		}
		checks = append(checks, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read services: %w", err)
	}
	if len(checks) == 0 {
		fmt.Println("no services to check")
		return nil
	}

	for i := range checks {
		checks[i].Verdict, checks[i].Detail = classifyRemote(ctx, pool, timeout, checks[i])
	}

	// Grouped by verdict so the answer to "is this one server or all of them?" is the first thing read.
	sort.SliceStable(checks, func(i, j int) bool {
		if checks[i].Verdict != checks[j].Verdict {
			return verdictRank(checks[i].Verdict) < verdictRank(checks[j].Verdict)
		}
		return checks[i].ServiceID < checks[j].ServiceID
	})

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "VERDICT\tSERVICE\tSTATUS\tEXTERNAL ID\tCUSTOMER")
	for _, c := range checks {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", c.Verdict, c.ServiceID, c.Status,
			orDash(c.External), orDash(c.Customer))
	}
	_ = tw.Flush()

	counts := map[string]int{}
	for _, c := range checks {
		counts[c.Verdict]++
	}
	fmt.Printf("\n%d service(s): %d ok, %d missing, %d error, %d without an id\n",
		len(checks), counts["ok"], counts["missing"], counts["error"], counts["no-id"])

	printRemedy(os.Stdout, checks, cfg.Provider)

	// A non-zero exit is what makes this usable from monitoring: "missing" rows need a human, "error" rows
	// need the integration fixed.
	if counts["missing"] > 0 || counts["error"] > 0 {
		return errRemoteCheckFailed
	}
	return nil
}

// errRemoteCheckFailed signals that at least one service did not verify.
var errRemoteCheckFailed = errors.New("one or more services did not verify against the provider")

// classifyRemote asks the provider about one service.
func classifyRemote(ctx context.Context, pool *store.Pool, timeout time.Duration, c remoteCheck) (string, string) {
	if c.External == "" {
		return "no-id", "never provisioned"
	}
	_, err := provisioning.Inspect(ctx, pool, timeout, c.External)
	switch {
	case err == nil:
		return "ok", ""
	case errors.Is(err, extproto.ErrNotFound):
		return "missing", "the provider does not have this resource"
	case errors.Is(err, extproto.ErrTimeout):
		return "error", "the provider did not answer in time"
	case errors.Is(err, provisioning.ErrNotConfigured), errors.Is(err, provisioning.ErrUnsupportedOperation):
		return "error", err.Error()
	default:
		return "error", firstLine(err.Error())
	}
}

// printRemedy says what to do, because a table alone leaves the reader to invent the conclusion.
func printRemedy(w io.Writer, checks []remoteCheck, provider string) {
	var missing, errored int
	for _, c := range checks {
		switch c.Verdict {
		case "missing":
			missing++
		case "error":
			errored++
		}
	}
	switch {
	case errored > 0:
		fmt.Fprintf(w, "\nthe provider could not be asked about %d service(s): fix the integration (panel URL, key,\n"+
			"extension path) before reading anything else here — the `missing` rows may be false.\n", errored)
	case missing > 0:
		fmt.Fprintf(w, "\n%d service(s) point at resources %q does not have. Two causes, and they look identical\n"+
			"from the customer's side:\n", missing, provider)
		fmt.Fprintf(w, "  - a resource deleted in the provider's own panel: the service needs re-provisioning;\n")
		fmt.Fprintf(w, "  - rows seeded for a different provider (demo/fixture data): their external ids were never\n")
		fmt.Fprintf(w, "    real. Remove them, or re-point them at real resources.\n")
	case missing == 0 && errored == 0:
		fmt.Fprintf(w, "\nevery provisioned service is known to %q.\n", provider)
	}
}

// verdictRank orders the table worst-first.
func verdictRank(verdict string) int {
	switch verdict {
	case "error":
		return 0
	case "missing":
		return 1
	case "no-id":
		return 2
	default:
		return 3
	}
}

// orDash keeps empty columns readable.
func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}

// firstLine trims a multi-line error to its first line, for a table cell.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 120 {
		s = s[:120] + "…"
	}
	return s
}
