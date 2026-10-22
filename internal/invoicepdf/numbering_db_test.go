package invoicepdf_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/catalyst/billing/internal/billing"
	"github.com/catalyst/billing/internal/invoicepdf"
	"github.com/catalyst/billing/internal/money"
	"github.com/catalyst/billing/internal/store/storetest"
	"github.com/jackc/pgx/v5"
)

// LoadNumbering reads the settings table, so it is tested against the real one:
// the shape of a `settings` row, and the seeding migration, are part of what it
// depends on.
func TestLoadNumberingReadsSeededDefaults(t *testing.T) {
	pool := storetest.DB(t)
	ctx := context.Background()

	got, err := invoicepdf.LoadNumbering(ctx, pool)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	want := invoicepdf.DefaultNumbering()
	if got != want {
		t.Fatalf("seeded numbering = %+v, want the code-side defaults %+v", got, want)
	}
}

// A configured value must win, and a malformed padding must fall back rather than
// take the issue path down.
func TestLoadNumberingReadsConfiguredValues(t *testing.T) {
	pool := storetest.DB(t)
	ctx := context.Background()

	if _, err := pool.Exec(ctx, `
		UPDATE settings SET value = 'BILL-{year}-'
		WHERE group_key = 'invoices' AND key = 'invoice_number_prefix'`); err != nil {
		t.Fatalf("set prefix: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE settings SET value = '4'
		WHERE group_key = 'invoices' AND key = 'invoice_number_padding'`); err != nil {
		t.Fatalf("set padding: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE settings SET value = 'CN'
		WHERE group_key = 'invoices' AND key = 'credit_note_prefix'`); err != nil {
		t.Fatalf("set credit prefix: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE settings SET value = 'true'
		WHERE group_key = 'invoices' AND key = 'proforma'`); err != nil {
		t.Fatalf("set proforma: %v", err)
	}

	got, err := invoicepdf.LoadNumbering(ctx, pool)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.InvoicePrefix != "BILL-{year}-" {
		t.Fatalf("invoice prefix = %q", got.InvoicePrefix)
	}
	if got.InvoicePadding != 4 {
		t.Fatalf("invoice padding = %d, want 4", got.InvoicePadding)
	}
	if got.CreditNotePrefix != "CN" {
		t.Fatalf("credit note prefix = %q", got.CreditNotePrefix)
	}
	if !got.Proforma {
		t.Fatal("proforma was not read as enabled")
	}

	// A malformed padding keeps the default: a typo in a settings field must not
	// stop invoices being issued.
	if _, err := pool.Exec(ctx, `
		UPDATE settings SET value = 'six'
		WHERE group_key = 'invoices' AND key = 'invoice_number_padding'`); err != nil {
		t.Fatalf("set bad padding: %v", err)
	}
	got, err = invoicepdf.LoadNumbering(ctx, pool)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.InvoicePadding != invoicepdf.DefaultInvoicePadding {
		t.Fatalf("padding = %d after a malformed value, want the default %d",
			got.InvoicePadding, invoicepdf.DefaultInvoicePadding)
	}
}

// A missing settings row must not be an error: an installation upgraded from
// before the seed still issues invoices.
func TestLoadNumberingWithoutRows(t *testing.T) {
	pool := storetest.DB(t)
	ctx := context.Background()

	if _, err := pool.Exec(ctx, `DELETE FROM settings WHERE group_key = 'invoices'`); err != nil {
		t.Fatalf("clear settings: %v", err)
	}
	got, err := invoicepdf.LoadNumbering(ctx, pool)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got != invoicepdf.DefaultNumbering() {
		t.Fatalf("numbering = %+v, want the defaults", got)
	}
}

// The wiring the integrator must perform: read the numbering inside the issuing
// transaction, pass the series to the issuer, and the allocated number carries the
// configured format. This test is the contract, so the wiring cannot be guessed
// wrong.
func TestConfiguredSeriesReachesAnIssuedInvoice(t *testing.T) {
	pool := storetest.DB(t)
	ctx := context.Background()

	if _, err := pool.Exec(ctx, `
		UPDATE settings SET value = 'BILL-{year}-'
		WHERE group_key = 'invoices' AND key = 'invoice_number_prefix'`); err != nil {
		t.Fatalf("set prefix: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE settings SET value = '4'
		WHERE group_key = 'invoices' AND key = 'invoice_number_padding'`); err != nil {
		t.Fatalf("set padding: %v", err)
	}

	var number string
	err := pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		numbering, err := invoicepdf.LoadNumbering(ctx, tx)
		if err != nil {
			return err
		}
		series, err := numbering.InvoiceSeries(time.Now())
		if err != nil {
			return err
		}

		var customerID string
		if err := tx.QueryRow(ctx,
			`INSERT INTO customers (email, name) VALUES ('series@example.test', 'Series') RETURNING id`).
			Scan(&customerID); err != nil {
			return err
		}
		issuer := billing.NewIssuer(billing.FixedTaxPricer{
			Rate: money.MustParseRatio("0%"), Rounding: money.RoundHalfUp, Policy: "none",
		})
		cur := money.MustLookupCurrency("USD")
		id, _, _, err := issuer.CreateDraft(ctx, tx, billing.IssueRequest{
			CustomerID: customerID,
			Lines: []billing.Line{{
				Description: "Hosting", QuantityMilli: 1000, UnitPrice: money.New(1000, cur),
			}},
			Customer: billing.CustomerDetails{ID: customerID, Name: "Series"},
		})
		if err != nil {
			return err
		}
		issued, err := issuer.Issue(ctx, tx, id, series)
		if err != nil {
			return err
		}
		number = issued.Number
		return nil
	})
	if err != nil {
		t.Fatalf("issue with configured series: %v", err)
	}

	want := fmt.Sprintf("BILL-%d-0001", time.Now().UTC().Year())
	if number != want {
		t.Fatalf("issued number = %q, want %q", number, want)
	}
}
