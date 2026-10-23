package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/catalyst/billing/internal/audit"
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
//
// The subcommands split into two groups: the ones that bootstrap a fresh installation
// (create-user, create-customer, migrate) and the ones that reach the panel's operator screens
// from a shell (setting set, logs tail, issue-invoice, purge-test-data). The second group exists
// because an operator without a browser — at 02:00, with the panel being the thing that is broken
// — must still be able to change a setting and read a log. See admin_settings.go.
func cmdAdmin(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: billing admin " +
			"<create-user|password-reset|create-customer|issue-invoice|import|purge-test-data|setting|logs>")
	}
	switch args[0] {
	case "create-user":
		return adminCreateUser(args[1:])
	case "password-reset":
		return adminPasswordReset(args[1:])
	case "create-customer":
		return adminCreateCustomer(args[1:])
	case "issue-invoice":
		return adminIssueInvoice(args[1:])
	case "import":
		return adminImport(args[1:])
	case "purge-test-data":
		return adminPurgeTestData(args[1:])
	case "setting":
		// The verb is explicit ("set") rather than implied by the argument count: `setting` will
		// grow a `get` and a `list`, and a command that guessed its verb from how many arguments
		// it received could not grow them.
		if len(args) < 2 {
			return errors.New("usage: billing admin setting <set> <group> <key> <value|-> [--as <email>]")
		}
		switch args[1] {
		case "set":
			return adminSettingSet(args[2:])
		default:
			return fmt.Errorf("unknown setting command %q; try `billing admin setting set`", args[1])
		}
	case "logs":
		if len(args) < 2 {
			return errors.New("usage: billing admin logs <tail> [--kind email|error|http|all] [--follow]")
		}
		switch args[1] {
		case "tail":
			return adminLogsTail(args[2:])
		default:
			return fmt.Errorf("unknown logs command %q; try `billing admin logs tail`", args[1])
		}
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
		if *role != "" {
			if _, err := tx.Exec(ctx,
				`INSERT INTO staff_roles (user_id, role) VALUES ($1, $2)`, userID, *role); err != nil {
				return err
			}
		}
		// A login is also a customer of the business: it gets its billing identity here, by the
		// same rule the screen applies, so the bootstrap administrator can buy and hold a
		// service without anybody linking two rows by hand afterwards. This writes a
		// `customer_accounts` link to the account's *own* billing record and grants nothing
		// else — permissions still come from the role.
		_, err := authn.EnsureBillingIdentity(ctx, tx, userID, *email, *name)
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

// adminPasswordReset sets a password for an existing account from the command line.
//
// It exists because the panel's own reset flow needs a working mail transport and a browser, and
// the situation an operator is in when they need this is usually "the account is locked out and
// the panel is the thing that is broken". `admin create-user` could make a *new* account but
// nothing could repair an old one, so the documented break-glass procedure was a psql UPDATE —
// which sets a password without revoking the sessions that may be the reason for the reset, and
// without an audit row.
//
// It is therefore the same operation the staff screen performs, not a parallel implementation:
// `authn.Service.SetPasswordByOperator` writes the hash, bumps `credential_version`, revokes every
// live session of the target and records both the security event and the audit row — inside one
// transaction, so a half-done reset cannot happen.
//
// Usage: billing admin password-reset --email <address|id> [--password ...] [--generate] [--as <operator>]
//
// The password comes from BILLING_ADMIN_PASSWORD or a pipe by default, for the same reason
// create-user does: an argument is visible in `ps` and lands in shell history. `--generate` is for
// the operator who has nobody to tell a password to yet; the generated value is printed once and
// never stored anywhere else.
func adminPasswordReset(args []string) error {
	flags, positional, err := splitFlags(args, []string{"email", "password", "as"}, []string{"generate"})
	if err != nil {
		return err
	}
	if len(positional) != 0 {
		return errors.New("usage: billing admin password-reset --email <address|id> " +
			"[--password <value>|--generate] [--as <operator email or id>]")
	}

	target := strings.TrimSpace(flags["email"])
	if target == "" {
		return errors.New("--email is required (an address, or a user id)")
	}

	generated := flags["generate"] == "true"
	password := flags["password"]
	if generated && password != "" {
		// Both would mean one of them is ignored, and a caller cannot tell which.
		return errors.New("--generate and --password are alternatives; pass one")
	}
	if generated {
		password, err = newCLIPassword()
		if err != nil {
			return err
		}
	}
	if password == "" {
		password = os.Getenv("BILLING_ADMIN_PASSWORD")
	}
	if password == "" && stdinIsPipe() {
		line, rerr := bufio.NewReader(os.Stdin).ReadString('\n')
		if rerr != nil && !errors.Is(rerr, io.EOF) {
			return fmt.Errorf("read password from stdin: %w", rerr)
		}
		password = strings.TrimRight(line, "\r\n")
	}
	if password == "" {
		return errors.New("set BILLING_ADMIN_PASSWORD, pipe the password on stdin, " +
			"or pass --password or --generate")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool, err := store.Open(ctx, store.ConfigFromEnv())
	if err != nil {
		return err
	}
	defer pool.Close()

	var (
		userID string
		email  string
	)
	if validUUID(target) {
		err = pool.QueryRow(ctx, `SELECT id, email FROM users WHERE id = $1`, target).
			Scan(&userID, &email)
	} else {
		err = pool.QueryRow(ctx, `SELECT id, email FROM users WHERE lower(email) = lower($1)`, target).
			Scan(&userID, &email)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		// No "did you mean": the caller is an operator, and echoing account details back to an
		// unknown address is the enumeration behaviour the login endpoint refuses to have.
		return fmt.Errorf("no user matches %q", target)
	}
	if err != nil {
		return fmt.Errorf("look up the account: %w", err)
	}

	// The operator is resolved the same way `setting set` resolves one: when `--as` names a user
	// their permission is checked, because a shell must not become a way to do something the API
	// would answer 403 to. Without `--as` the reset is a system action that records the OS user
	// and host instead of inventing a person.
	operator, err := resolveCLIOperator(ctx, pool, flags["as"], authn.PermStaffManage)
	if err != nil {
		return err
	}

	hasher, err := authn.NewHasher(authn.DefaultArgon2Params())
	if err != nil {
		return err
	}
	svc, err := authn.New(pool, hasher, authn.DefaultConfig())
	if err != nil {
		return err
	}

	revoked, err := svc.SetPasswordByOperator(ctx, userID, password, operator.userID,
		cliAuditor{actor: operator.actor, operator: operator.label})
	if err != nil {
		if errors.Is(err, authn.ErrWeakPassword) {
			return fmt.Errorf("%w (nothing was changed)", err)
		}
		return fmt.Errorf("reset the password: %w", err)
	}

	fmt.Printf("reset the password for %s\n", email)
	fmt.Printf("revoked %s\n", pluralCount(revoked, "session"))
	if generated {
		// Printed once, on stdout, because there is nothing else to give the operator: the
		// point of the command is to hand somebody their first password.
		fmt.Printf("password: %s\n", password)
	}
	if operator.actor.Kind == audit.ActorSystem {
		fmt.Fprintf(os.Stderr,
			"recorded as a system action (%s); pass --as <operator> to name the person\n", operator.label)
	}
	return nil
}

// newCLIPassword returns a random password for `password-reset --generate`.
//
// 24 characters from a base64url alphabet is 144 bits of entropy: long enough that the value the
// operator reads off the terminal survives being typed once, and made of characters that survive
// a shell and a chat window.
func newCLIPassword() (string, error) {
	raw := make([]byte, 18)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate a password: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// pluralCount renders "1 session" / "0 sessions" for the operator-facing summary.
func pluralCount(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
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
