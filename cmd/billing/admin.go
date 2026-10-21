package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/catalyst/billing/internal/authn"
	"github.com/catalyst/billing/internal/billing"
	"github.com/catalyst/billing/internal/money"
	"github.com/catalyst/billing/internal/store"
	"github.com/jackc/pgx/v5"
)

// cmdAdmin holds operator tasks that must exist before there is a UI: creating
// the first customer, and issuing an invoice by hand.
//
// Every subcommand is a thin wrapper over the same domain code the API uses, so
// an admin action cannot take a shortcut that the API would refuse.
func cmdAdmin(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: billing admin " +
			"<create-user|create-customer|issue-invoice|purge-test-data>")
	}
	switch args[0] {
	case "create-user":
		return adminCreateUser(args[1:])
	case "create-customer":
		return adminCreateCustomer(args[1:])
	case "issue-invoice":
		return adminIssueInvoice(args[1:])
	case "purge-test-data":
		return adminPurgeTestData(args[1:])
	default:
		return fmt.Errorf("unknown admin command %q", args[0])
	}
}

func adminCreateCustomer(args []string) error {
	fs := flag.NewFlagSet("admin create-customer", flag.ExitOnError)
	email := fs.String("email", "", "customer email (required)")
	name := fs.String("name", "", "customer name")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *email == "" {
		return errors.New("--email is required")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool, err := store.Open(ctx, store.ConfigFromEnv())
	if err != nil {
		return err
	}
	defer pool.Close()

	var id string
	err = pool.QueryRow(ctx,
		`INSERT INTO customers (email, name) VALUES ($1, $2) RETURNING id`, *email, *name).Scan(&id)
	if err != nil {
		if store.IsUniqueViolation(err) {
			return fmt.Errorf("a customer with email %q already exists", *email)
		}
		return err
	}
	fmt.Printf("created customer %s\n", id)
	return nil
}

func adminIssueInvoice(args []string) error {
	fs := flag.NewFlagSet("admin issue-invoice", flag.ExitOnError)
	customerID := fs.String("customer", "", "customer id (required)")
	description := fs.String("description", "Service", "line description")
	amount := fs.String("amount", "", "unit price, e.g. 12.00 (required)")
	currency := fs.String("currency", "USD", "currency code")
	taxRate := fs.String("tax-rate", "0%", "exact tax rate, e.g. 20%")
	merchant := fs.String("merchant", "", "merchant legal name")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *customerID == "" || *amount == "" {
		return errors.New("--customer and --amount are required")
	}

	cur, ok := money.LookupCurrency(*currency)
	if !ok {
		var err error
		if cur, err = money.NewCurrency(*currency, 2); err != nil {
			return err
		}
	}
	price, err := money.Parse(*amount, cur)
	if err != nil {
		return err
	}
	rate, err := money.ParseRatio(*taxRate)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool, err := store.Open(ctx, store.ConfigFromEnv())
	if err != nil {
		return err
	}
	defer pool.Close()

	issuer := billing.NewIssuer(billing.FixedTaxPricer{
		Rate:     rate,
		Rounding: money.RoundHalfUp,
		Policy:   "manual",
	})

	var issued billing.Issued
	err = pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		id, _, _, err := issuer.CreateDraft(ctx, tx, billing.IssueRequest{
			CustomerID: *customerID,
			Lines: []billing.Line{{
				Description:   *description,
				QuantityMilli: 1000,
				UnitPrice:     price,
			}},
			Merchant: billing.Merchant{Name: *merchant},
			Customer: billing.CustomerDetails{ID: *customerID},
			DueAt:    time.Now().AddDate(0, 0, 7),
		})
		if err != nil {
			return err
		}
		issued, err = issuer.Issue(ctx, tx, id, billing.DefaultSeries)
		return err
	})
	if err != nil {
		return err
	}
	fmt.Printf("issued %s: %s\n", issued.Number, issued.Totals.Total.Format())
	return nil
}

// adminCreateUser creates a login. Without this, a fresh installation has no way
// to obtain the first staff account, and a panel nobody can sign in to is not
// deployable.
func adminCreateUser(args []string) error {
	fs := flag.NewFlagSet("admin create-user", flag.ExitOnError)
	email := fs.String("email", "", "email address (required)")
	// The password is deliberately not a documented flag: an argument is visible in
	// `ps` and lands in shell history. Reading it from the environment or stdin keeps
	// it out of both.
	password := fs.String("password", "", "password (prefer BILLING_ADMIN_PASSWORD or stdin: a flag is visible in ps)")
	role := fs.String("role", "", "staff role: administrator, billing, support, infrastructure")
	name := fs.String("name", "", "display name")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *email == "" {
		return errors.New("--email is required")
	}
	if *password == "" {
		*password = os.Getenv("BILLING_ADMIN_PASSWORD")
	}
	if *password == "" && stdinIsPipe() {
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil && err != io.EOF {
			return fmt.Errorf("read password from stdin: %w", err)
		}
		*password = strings.TrimRight(line, "\r\n")
	}
	if *password == "" {
		return errors.New("set BILLING_ADMIN_PASSWORD, pipe the password on stdin, or pass --password")
	}
	if *role != "" && !authn.IsStaffRole(*role) {
		return fmt.Errorf("unknown role %q; one of: administrator, billing, support, infrastructure", *role)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool, err := store.Open(ctx, store.ConfigFromEnv())
	if err != nil {
		return err
	}
	defer pool.Close()

	hasher, err := authn.NewHasher(authn.DefaultArgon2Params())
	if err != nil {
		return err
	}
	hash, err := hasher.Hash(*password)
	if err != nil {
		return err
	}

	var userID string
	err = pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `
			INSERT INTO users (email, password_hash, display_name, email_verified_at)
			VALUES ($1, $2, $3, now()) RETURNING id`, *email, hash, *name).Scan(&userID); err != nil {
			return err
		}
		if *role == "" {
			return nil
		}
		_, err := tx.Exec(ctx, `INSERT INTO staff_roles (user_id, role) VALUES ($1, $2)`, userID, *role)
		return err
	})
	if err != nil {
		if store.IsUniqueViolation(err) {
			return fmt.Errorf("a user with email %q already exists", *email)
		}
		return err
	}
	if *role == "" {
		fmt.Printf("created user %s (customer, no staff role)\n", userID)
	} else {
		fmt.Printf("created user %s with staff role %s\n", userID, *role)
	}
	return nil
}

// stdinIsPipe reports whether stdin is a pipe or file rather than a terminal, which
// is how the password is read without echoing it into the terminal or the process
// list.
func stdinIsPipe() bool {
	info, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice == 0
}
