package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"time"

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
		return errors.New("usage: billing admin <create-customer|issue-invoice>")
	}
	switch args[0] {
	case "create-customer":
		return adminCreateCustomer(args[1:])
	case "issue-invoice":
		return adminIssueInvoice(args[1:])
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
