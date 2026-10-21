// Command billing is the single binary for the billing panel.
//
// One artifact, several roles:
//
//	billing serve      HTTP API and UI
//	billing worker     background jobs (payments, provisioning, renewals)
//	billing migrate    apply schema migrations and exit
//	billing doctor     check configuration and connectivity, and exit non-zero on a problem
//	billing admin      operator tasks (create a customer, issue a token)
//
// A small installation runs `serve` and `worker` in one process with
// --embedded-worker; a larger one runs the same binary in separate units. Both
// paths execute identical code, so behaviour cannot diverge between them.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/catalyst/billing/internal/extproto"
	"github.com/catalyst/billing/internal/store"
)

// version is stamped at build time: -ldflags "-X main.version=1.2.3".
var version = "dev"

func main() {
	if err := run(os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(0)
		}
		fmt.Fprintf(os.Stderr, "billing: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		usage(os.Stderr)
		return errors.New("no subcommand")
	}

	switch args[0] {
	case "serve":
		return cmdServe(args[1:])
	case "worker":
		return cmdWorker(args[1:])
	case "migrate":
		return cmdMigrate(args[1:])
	case "doctor":
		return cmdDoctor(args[1:])
	case "admin":
		return cmdAdmin(args[1:])
	case "version", "--version", "-v":
		fmt.Println(version)
		return nil
	case "help", "--help", "-h":
		usage(os.Stdout)
		return nil
	default:
		usage(os.Stderr)
		return fmt.Errorf("unknown subcommand %q", args[0])
	}
}

// extensionChecks launches each configured extension and verifies the handshake.
//
// A handshake failure is reported with the extension's stderr, because "the
// extension did not start" is never actionable on its own.
func extensionChecks(ctx context.Context) []check {
	var out []check

	for _, spec := range []struct {
		name   string
		envKey string
		method string
	}{
		{"gateway_extension", "BILLING_GATEWAY_EXTENSION", "gateway.describe"},
		{"provisioner_extension", "BILLING_PROVISIONER_EXTENSION", "provisioner.describe"},
	} {
		path := strings.TrimSpace(os.Getenv(spec.envKey))
		if path == "" {
			out = append(out, check{
				Name:   spec.name,
				OK:     false,
				Detail: spec.envKey + " is not set",
			})
			continue
		}

		launchCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		client, err := extproto.Launch(launchCtx, extproto.Spec{
			Path:        path,
			CoreVersion: version,
			CallTimeout: 5 * time.Second,
		})
		if err != nil {
			cancel()
			// Launch failed, so there is no client to read stderr from. The error
			// already carries the reason the handshake or the exec failed.
			out = append(out, check{Name: spec.name, OK: false, Detail: err.Error()})
			continue
		}
		// The handshake already happened in Launch; describe confirms the method
		// set the core depends on is actually implemented.
		var desc map[string]any
		err = client.Call(launchCtx, spec.method, struct{}{}, &desc)
		caps := client.Capabilities()
		cancel()
		_ = client.Close()

		if err != nil {
			out = append(out, check{Name: spec.name, OK: false,
				Detail: fmt.Sprintf("%s failed: %v", spec.method, err)})
			continue
		}
		out = append(out, check{Name: spec.name, OK: true,
			Detail: fmt.Sprintf("%s, %d capabilities", client.Info().Name, len(caps))})
	}
	return out
}

func usage(w *os.File) {
	fmt.Fprint(w, `billing — self-hosted billing and service automation

Usage:
  billing <subcommand> [flags]

Subcommands:
  serve     run the HTTP API and web UI
  worker    run background jobs
  migrate   apply pending database migrations
  doctor    verify configuration, database and extensions
  admin     operator tasks
  version   print the version

Run "billing <subcommand> -h" for the flags of a subcommand.
`)
}

// signalContext returns a context cancelled by SIGINT/SIGTERM, so every
// long-running command shuts down the same way.
func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
}

// ------------------------------------------------------------------ migrate --

func cmdMigrate(args []string) error {
	fs := flag.NewFlagSet("migrate", flag.ExitOnError)
	status := fs.Bool("status", false, "report pending migrations without applying them")
	timeout := fs.Duration("timeout", 5*time.Minute, "overall timeout")
	if err := fs.Parse(args); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	cfg := store.ConfigFromEnv()
	pool, err := store.OpenMigrator(ctx, cfg)
	if err != nil {
		return err
	}
	defer pool.Close()

	migrator := pool.Migrator()
	migrator.Logger = func(format string, a ...any) {
		fmt.Printf(format+"\n", a...)
	}

	if *status {
		pending, err := migrator.Pending(ctx)
		if err != nil {
			return err
		}
		if len(pending) == 0 {
			fmt.Println("schema is up to date")
			return nil
		}
		fmt.Printf("%d pending migration(s):\n", len(pending))
		for _, m := range pending {
			fmt.Printf("  %s\n", m.Name)
		}
		return nil
	}

	applied, err := migrator.Up(ctx)
	if err != nil {
		return err
	}
	if len(applied) == 0 {
		fmt.Println("schema is up to date")
		return nil
	}
	fmt.Printf("applied %d migration(s)\n", len(applied))
	return nil
}

// ------------------------------------------------------------------- doctor --

// check is one doctor result.
type check struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

// cmdDoctor checks the things that are cheap to check and expensive to discover
// in production: database connectivity, schema state, role privileges and the
// presence of required configuration.
func cmdDoctor(args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ExitOnError)
	jsonOut := fs.Bool("json", false, "machine-readable output")
	if err := fs.Parse(args); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var checks []check
	add := func(name string, ok bool, detail string) {
		checks = append(checks, check{Name: name, OK: ok, Detail: detail})
	}

	cfg := store.ConfigFromEnv()
	add("database_url", cfg.URL != "", "BILLING_DATABASE_URL is set")

	if cfg.URL != "" {
		pool, err := store.Open(ctx, cfg)
		if err != nil {
			add("database_connect", false, err.Error())
		} else {
			defer pool.Close()
			add("database_connect", true, "")

			pending, err := pool.Migrator().Pending(ctx)
			switch {
			case err != nil:
				add("schema", false, err.Error())
			case len(pending) > 0:
				names := make([]string, len(pending))
				for i, m := range pending {
					names[i] = m.Name
				}
				add("schema", false, "pending: "+strings.Join(names, ", "))
			default:
				add("schema", true, "up to date")
			}

			// The runtime role must not be able to change the schema.
			var canCreate bool
			if err := pool.QueryRow(ctx,
				`SELECT has_schema_privilege(current_user, 'public', 'CREATE')`).Scan(&canCreate); err != nil {
				add("runtime_role", false, err.Error())
			} else if canCreate {
				add("runtime_role", false, "the runtime role can CREATE in schema public; use a restricted role")
			} else {
				add("runtime_role", true, "cannot create schema objects")
			}

			// A cached balance that disagrees with the entries is a bug worth
			// finding before a customer does.
			rows, err := pool.Query(ctx, `
				SELECT currency, COALESCE(SUM(debit_minor),0) - COALESCE(SUM(credit_minor),0)
				FROM journal_entries GROUP BY currency`)
			if err != nil {
				add("ledger_balance", false, err.Error())
			} else {
				unbalanced := ""
				for rows.Next() {
					var cur string
					var net int64
					if err := rows.Scan(&cur, &net); err != nil {
						unbalanced = err.Error()
						break
					}
					if net != 0 {
						unbalanced = fmt.Sprintf("%s is off by %d minor units", cur, net)
					}
				}
				rows.Close()
				if unbalanced != "" {
					add("ledger_balance", false, unbalanced)
				} else {
					add("ledger_balance", true, "every currency sums to zero")
				}
			}

			// Events that have been pending a long time mean the worker is not
			// running or is stuck.
			var staleEvents int
			if err := pool.QueryRow(ctx, `
				SELECT count(*) FROM gateway_events
				WHERE processed_at IS NULL AND received_at < now() - interval '15 minutes'`).
				Scan(&staleEvents); err != nil {
				add("webhook_backlog", false, err.Error())
			} else if staleEvents > 0 {
				add("webhook_backlog", false, fmt.Sprintf("%d event(s) pending over 15 minutes", staleEvents))
			} else {
				add("webhook_backlog", true, "")
			}
		}
	}

	// Extensions: a broken executable means payments or provisioning silently
	// stop, so it is checked here rather than discovered from a stalled queue.
	checks = append(checks, extensionChecks(ctx)...)

	failed := 0
	for _, c := range checks {
		if !c.OK {
			failed++
		}
	}
	if *jsonOut {
		fmt.Print("[")
		for i, c := range checks {
			if i > 0 {
				fmt.Print(",")
			}
			fmt.Printf(`{"name":%q,"ok":%t,"detail":%q}`, c.Name, c.OK, c.Detail)
		}
		fmt.Println("]")
	} else {
		for _, c := range checks {
			mark := "ok  "
			if !c.OK {
				mark = "FAIL"
			}
			if c.Detail == "" {
				fmt.Printf("%s  %s\n", mark, c.Name)
			} else {
				fmt.Printf("%s  %s: %s\n", mark, c.Name, c.Detail)
			}
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d check(s) failed", failed)
	}
	return nil
}
