package credit

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/catalyst/billing/internal/billing"
	"github.com/catalyst/billing/internal/checkout"
	"github.com/catalyst/billing/internal/extproto"
	"github.com/catalyst/billing/internal/ledger"
	"github.com/catalyst/billing/internal/money"
	"github.com/catalyst/billing/internal/payments"
	"github.com/catalyst/billing/internal/store"
	"github.com/jackc/pgx/v5"
)

// Deposits: a customer tops up credit through the gateway (CRD-02, CRD-05).
//
// # The design, and why it does not weaken a constraint
//
// The panel's only payment path is `checkout.Service.Start(invoiceID)`: it reads the
// amount from an invoice under a row lock, records the attempt, and calls the gateway.
// `payment_attempts.invoice_id` is `NOT NULL REFERENCES invoices(id)`, and the webhook
// processor verifies the settled amount against that attempt — that check is what stops a
// gateway (or a forged webhook) from settling an amount nobody asked for. Making
// `invoice_id` nullable to model "a payment with no invoice behind it" would delete that
// check for exactly the payments that carry the most risk (money that becomes a balance
// the customer can spend anywhere).
//
// So a deposit *is* an invoice, marked in its own frozen snapshot:
//
//	invoice "Account credit deposit" (no order, snapshot.catalyst_deposit = "credit")
//	    ->  payment_attempts (via checkout.Service, unchanged)
//	    ->  payments + allocation (billing.ApplyPayment, unchanged)
//	    ->  customer_credit (the grant posted here)
//
// # Why the marker is the snapshot, and why there is no order
//
// Three candidate markers were considered and two were rejected on consequences rather than
// taste:
//
//   - A nullable `payment_attempts.invoice_id` ("a payment with no invoice behind it")
//     deletes the amount verification for exactly the payments that carry the most risk:
//     money that becomes a balance spendable anywhere. Rejected.
//   - "An order with no items" reads well until the operator dashboard is considered: its
//     paid-not-provisioned check is an INNER JOIN on orders, and counts a paid invoice whose
//     order has no active service. Every deposit would raise a permanent critical alert.
//     Rejected.
//   - The invoice snapshot. It is the document's own statement of what it is, it is frozen
//     by the trigger the moment the invoice is issued, and nothing else writes that key.
//     `invoices.order_id` stays NULL, so the dashboard's join excludes a deposit naturally
//     and a deposit is never mistaken for a purchase that failed to deliver.
//
// A staff-created manual invoice also has no order, which is why "no order" alone is not the
// marker: it would route a bank transfer for a real invoice into a credit grant. The
// snapshot key is what separates the two, and it is written only by this package.
//
// Nothing is weakened: no column is made nullable, no constraint is relaxed, and the gateway
// path, its recorded attempt and its amount verification are the existing ones unchanged.
// Fulfilment provisions from order items and from invoice lines that name a product; a
// deposit has neither, so it provisions nothing without needing to be special-cased.
//
// The deposit invoice is priced at a zero tax rate on purpose: a deposit is not a taxable
// supply. Issuing it posts receivable/revenue; settling it posts the money and then debits
// that revenue back and credits `customer_credit`, so the net effect of a completed
// deposit is
//
//	debit  cash_clearing
//	credit customer_credit
//
// and revenue is recognised later, when the credit is spent on an invoice that really is
// a supply.

// Gateway is the checkout side of the payment extension.
type Gateway interface {
	Name() string
	CreateCheckout(ctx context.Context, p extproto.CreateCheckoutParams) (extproto.CreateCheckoutResult, error)
}

// Reference types this package adds to the ledger.
const (
	// RefCreditDeposit is the grant of credit for a settled deposit. It is separate from
	// RefCreditApplication because depositing and spending are two economic events: a
	// replay of one must never be mistaken for the other.
	RefCreditDeposit ledger.ReferenceType = "credit_deposit"
	// RefCreditGrant is an operator's goodwill grant, which has no money behind it.
	RefCreditGrant ledger.ReferenceType = "credit_grant"
)

// depositDescription is what the customer sees on the deposit invoice.
const depositDescription = "Account credit deposit"

// DepositKey is the snapshot key that marks an invoice as a credit deposit.
const DepositKey = "catalyst_deposit"

// DepositKind is the value of that key. It is a string rather than a boolean so the marker
// can carry a reason later without changing the shape of what is already frozen.
const DepositKind = "credit"

// depositInvoicePredicate identifies a deposit invoice by its own frozen snapshot.
//
// A deposit has no order and a manual invoice has no order either, so the snapshot key is
// the only thing that separates them. It is written inside the same transaction that issues
// the invoice, and the issue trigger freezes it, so it cannot drift from the document.
const depositInvoicePredicate = `i.snapshot->>'` + DepositKey + `' = '` + DepositKind + `'`

// WithGateway attaches the payment gateway a deposit is started through.
//
// A service without one can still grant, apply and read credit; only StartDeposit needs
// the gateway, and it reports ErrNoGateway rather than failing half way.
func (s *Service) WithGateway(g Gateway, account, successURL, cancelURL string) *Service {
	clone := *s
	clone.gateway = g
	clone.account = account
	clone.successURL = successURL
	clone.cancelURL = cancelURL
	return &clone
}

// DepositRequest asks to top up credit.
type DepositRequest struct {
	CustomerID  string
	AmountMinor int64
	Currency    string
	// RequestedBy is the customer's user id, used for the audit row.
	RequestedBy string
}

// Deposit is a started top-up: an issued invoice plus the gateway session to pay it.
type Deposit struct {
	InvoiceID string
	// Number is the deposit invoice's document number.
	Number      string
	AttemptID   string
	RedirectURL string
	Amount      money.Amount
	// Status is the checkout attempt's state, normally "pending".
	Status string
}

// StartDeposit validates a top-up, creates the deposit invoice and opens a gateway session.
//
// The order of operations is deliberate:
//
//  1. Validate against the settings and the balance limit *before* creating anything, so a
//     refused top-up leaves no open invoice behind.
//  2. Create the invoice, mark it as a deposit and issue it in one transaction.
//  3. Ask the gateway for a session. If that fails the deposit invoice is voided rather than
//     left open: an open deposit reserves headroom under the maximum balance, and nothing
//     can ever be collected against it. Voiding keeps the document as evidence and lets the
//     customer retry immediately.
func (s *Service) StartDeposit(ctx context.Context, req DepositRequest) (Deposit, error) {
	if s.pool == nil {
		return Deposit{}, ErrInvalidRequest
	}
	if s.gateway == nil {
		// Checked before anything is written: an unconfigured gateway is a deployment
		// state, and creating an invoice we cannot collect would leave a phantom balance
		// reservation behind.
		return Deposit{}, ErrNoGateway
	}
	if req.CustomerID == "" {
		return Deposit{}, fmt.Errorf("%w: a customer is required", ErrInvalidRequest)
	}
	if req.AmountMinor <= 0 {
		return Deposit{}, fmt.Errorf("%w: the amount must be positive", ErrInvalidRequest)
	}
	cur, err := resolveCurrency(req.Currency)
	if err != nil {
		return Deposit{}, err
	}

	var (
		invoiceID, number string
	)
	err = s.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		settings, err := LoadSettings(ctx, tx)
		if err != nil {
			return err
		}
		if !settings.Enabled {
			return ErrDisabled
		}
		if req.AmountMinor < settings.MinimumDepositMinor {
			return fmt.Errorf("%w: the minimum is %s",
				ErrBelowMinimum, money.New(settings.MinimumDepositMinor, cur).Format())
		}
		if req.AmountMinor > settings.MaximumDepositMinor {
			return fmt.Errorf("%w: the maximum is %s",
				ErrAboveMaximum, money.New(settings.MaximumDepositMinor, cur).Format())
		}

		// Lock the customer row: two concurrent top-ups must not each be told the whole
		// headroom is available, which would take the balance past the maximum.
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT true FROM customers WHERE id = $1 FOR UPDATE`,
			req.CustomerID).Scan(&exists); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return fmt.Errorf("credit: lock customer: %w", err)
		}

		balance, err := creditBalance(ctx, tx, req.CustomerID, cur)
		if err != nil {
			return err
		}
		// Deposits that are issued but not yet settled are a reservation: their money is
		// on its way and will become credit, so counting only the settled balance would
		// let a customer start two top-ups that each pass the limit.
		var pending int64
		if err := tx.QueryRow(ctx, `
			SELECT COALESCE(SUM(i.total_minor - i.amount_paid_minor), 0)
			FROM invoices i
			WHERE i.customer_id = $1 AND i.currency = $2 AND i.status = 'open'
			  AND `+depositInvoicePredicate, req.CustomerID, cur.Code()).Scan(&pending); err != nil {
			return fmt.Errorf("credit: sum pending deposits: %w", err)
		}
		if balance+pending+req.AmountMinor > settings.MaximumCreditMinor {
			return fmt.Errorf("%w: %s held, %s pending, %s maximum",
				ErrMaxBalance,
				money.New(balance, cur).Format(),
				money.New(pending, cur).Format(),
				money.New(settings.MaximumCreditMinor, cur).Format())
		}

		customer := billing.CustomerDetails{ID: req.CustomerID}
		if err := billing.FillCustomer(ctx, tx, &customer); err != nil {
			return err
		}
		issuer := billing.NewIssuer(billing.FixedTaxPricer{
			Rate: money.MustParseRatio("0%"), Rounding: money.RoundHalfUp,
			Policy: "credit-deposit", Jurisdiction: "none",
		})
		dueAt := s.depositNow().AddDate(0, 0, 7)
		draftID, _, _, err := issuer.CreateDraft(ctx, tx, billing.IssueRequest{
			CustomerID: req.CustomerID,
			Lines: []billing.Line{{
				Description:   depositDescription,
				QuantityMilli: 1000,
				UnitPrice:     money.New(req.AmountMinor, cur),
			}},
			Customer: customer,
			Terms:    "Credit added to your account when this payment settles.",
			DueAt:    dueAt,
		})
		if err != nil {
			return err
		}
		// The marker is written while the invoice is still a draft, which is the only window
		// in which the document may change. Issue then freezes it into the snapshot, so the
		// settlement path reads the marker from the same evidence a reprint would.
		if _, err := tx.Exec(ctx, `
			UPDATE invoices
			SET snapshot = jsonb_set(snapshot, ARRAY[$2], to_jsonb($3::text), true)
			WHERE id = $1 AND status = 'draft'`,
			draftID, DepositKey, DepositKind); err != nil {
			return fmt.Errorf("credit: mark deposit invoice: %w", err)
		}

		issued, err := issuer.Issue(ctx, tx, draftID, billing.DefaultSeries)
		if err != nil {
			return err
		}
		invoiceID, number = issued.InvoiceID, issued.Number

		return s.recordAudit(ctx, tx, "credit.deposit.start", "invoice", invoiceID, req.RequestedBy,
			map[string]any{
				"customer_id": req.CustomerID,
				"amount":      req.AmountMinor,
				"currency":    cur.Code(),
				"number":      number,
			})
	})
	if err != nil {
		return Deposit{}, err
	}

	// The gateway session is created outside the transaction on purpose: it is a remote
	// call, and holding a row lock across it would serialise every other write to the
	// customer for as long as the provider takes to answer.
	session, err := checkout.New(s.pool, s.gateway, s.account, s.successURL, s.cancelURL).
		Start(ctx, invoiceID)
	if err != nil {
		// The session was not created, so nothing can be collected against this invoice.
		// Voiding it (rather than leaving it open) is what stops a failed attempt from
		// reserving headroom under the maximum balance forever, and it keeps the document
		// as evidence of what was attempted. A void invoice has no payments, so the
		// trigger in 0008 permits open -> void.
		if verr := s.voidDepositInvoice(ctx, invoiceID); verr != nil {
			// Reporting the original failure matters more than the cleanup: the customer
			// can retry either way, and the operator log carries both.
			return Deposit{}, fmt.Errorf("credit: start deposit checkout: %w (cleanup: %v)", err, verr)
		}
		return Deposit{}, fmt.Errorf("credit: start deposit checkout: %w", err)
	}

	return Deposit{
		InvoiceID:   invoiceID,
		Number:      number,
		AttemptID:   session.AttemptID,
		RedirectURL: session.URL,
		Amount:      money.New(req.AmountMinor, cur),
		Status:      "pending",
	}, nil
}

// DepositSettlement is a provider-confirmed payment for a deposit.
type DepositSettlement struct {
	Provider          string
	ProviderAccount   string
	ProviderPaymentID string
	InvoiceID         string
	AmountMinor       int64
	Currency          string
}

// DepositResult reports what settling a deposit did.
type DepositResult struct {
	InvoiceID string
	PaymentID string
	// Granted is the credit added to the customer.
	Granted money.Amount
	// Duplicate is true when this provider payment had already been granted.
	Duplicate bool
	// BalanceMinor is the customer's credit in this currency after the grant.
	BalanceMinor int64
	// OverLimit is true when the grant took the balance past credits.maximum_credit_minor.
	// The grant still happens: the provider already took the money, and refusing to credit
	// it would leave the customer paid up with no balance. The flag is what makes the
	// overshoot visible instead of silent.
	OverLimit bool
}

// SettleDeposit turns a settled deposit payment into customer credit.
//
// It is idempotent on the provider payment id: the payment row, its allocation and the
// grant posting are all keyed by it, so a replayed webhook adds the credit once. The
// invoice's own posting is reused through billing.ApplyPayment, which is the same function
// every other payment path uses — a deposit is not a special kind of money.
func (s *Service) SettleDeposit(ctx context.Context, settle DepositSettlement) (DepositResult, error) {
	if s.pool == nil {
		return DepositResult{}, ErrInvalidRequest
	}
	if settle.InvoiceID == "" || settle.ProviderPaymentID == "" {
		return DepositResult{}, fmt.Errorf("%w: a deposit needs an invoice and a payment", ErrInvalidRequest)
	}

	var out DepositResult
	err := s.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		out = DepositResult{}
		var err error
		out, err = SettleDepositTx(ctx, tx, settle, s.auditor)
		return err
	})
	if err != nil {
		return DepositResult{}, err
	}
	return out, nil
}

// SettleDepositTx is SettleDeposit inside a caller's transaction. The webhook applier uses
// this form so the settlement commits with the rest of the payment handling.
func SettleDepositTx(ctx context.Context, tx pgx.Tx, settle DepositSettlement,
	auditor auditRecorder) (DepositResult, error) {
	var (
		customerID, currency string
		totalMinor           int64
	)
	err := tx.QueryRow(ctx, `
		SELECT i.customer_id, i.currency, i.total_minor
		FROM invoices i
		WHERE i.id = $1 AND `+depositInvoicePredicate+`
		FOR UPDATE`, settle.InvoiceID).Scan(&customerID, &currency, &totalMinor)
	if errors.Is(err, pgx.ErrNoRows) {
		return DepositResult{}, ErrNotDeposit
	}
	if err != nil {
		return DepositResult{}, fmt.Errorf("credit: load deposit invoice: %w", err)
	}

	cur, err := resolveCurrency(currency)
	if err != nil {
		return DepositResult{}, err
	}
	if !strings.EqualFold(settle.Currency, cur.Code()) {
		return DepositResult{}, fmt.Errorf("%w: settled %s, deposit is %s",
			ErrAmountMismatch, settle.Currency, cur.Code())
	}
	if settle.AmountMinor != totalMinor {
		return DepositResult{}, fmt.Errorf("%w: settled %d, deposit is %d",
			ErrAmountMismatch, settle.AmountMinor, totalMinor)
	}

	// The invoice posting, the allocation and the invoice transition are the existing
	// payment path, not a copy of it.
	applied, err := billing.ApplyPayment(ctx, tx, billing.AllocationRequest{
		Provider:          settle.Provider,
		ProviderAccount:   settle.ProviderAccount,
		ProviderPaymentID: settle.ProviderPaymentID,
		InvoiceID:         settle.InvoiceID,
		Amount:            money.New(settle.AmountMinor, cur),
	})
	if err != nil {
		return DepositResult{}, err
	}

	// The grant: debit the revenue the deposit invoice recognised, credit the customer's
	// balance. Keyed by the provider payment id, so a replay finds the existing posting
	// instead of granting twice.
	if _, _, err := ledger.Post(ctx, tx, ledger.Posting{
		Kind:        ledger.KindAdjustment,
		Reference:   RefCreditDeposit,
		ReferenceID: applied.PaymentID,
		CustomerID:  customerID,
		Currency:    cur,
		Debits: []ledger.Entry{{
			Account: ledger.AccountRevenue,
			Amount:  money.New(settle.AmountMinor, cur),
			Memo:    "deposit credited to balance",
		}},
		Credits: []ledger.Entry{{
			Account: ledger.AccountCredit,
			Amount:  money.New(settle.AmountMinor, cur),
			Memo:    "deposit " + settle.ProviderPaymentID,
		}},
	}); err != nil {
		return DepositResult{}, fmt.Errorf("credit: post deposit grant: %w", err)
	}

	balance, err := creditBalance(ctx, tx, customerID, cur)
	if err != nil {
		return DepositResult{}, err
	}
	settings, err := LoadSettings(ctx, tx)
	if err != nil {
		return DepositResult{}, err
	}
	overLimit := balance > settings.MaximumCreditMinor

	out := DepositResult{
		InvoiceID:    settle.InvoiceID,
		PaymentID:    applied.PaymentID,
		Granted:      money.New(settle.AmountMinor, cur),
		Duplicate:    applied.Duplicate,
		BalanceMinor: balance,
		OverLimit:    overLimit,
	}

	changes := map[string]any{
		"invoice_id":  settle.InvoiceID,
		"payment_id":  applied.PaymentID,
		"amount":      settle.AmountMinor,
		"currency":    cur.Code(),
		"balance":     balance,
		"over_limit":  overLimit,
		"provider_id": settle.ProviderPaymentID,
	}
	if auditor != nil {
		if err := auditor.RecordAction(ctx, tx, "credit.deposit.settle", "invoice",
			settle.InvoiceID, changes); err != nil {
			return DepositResult{}, err
		}
	}
	return out, nil
}

// auditRecorder is the audit hook a settlement takes. It is an interface here so the
// transactional helper does not need the whole service.
type auditRecorder interface {
	RecordAction(ctx context.Context, tx pgx.Tx, action, targetType, targetID string,
		changes map[string]any) error
}

// DepositApplier implements payments.Applier for installations that sell credit.
//
// The payments processor has exactly one applier, so this wraps the ordinary one and
// routes: a deposit invoice becomes customer credit, and everything else is handed to the
// wrapped applier unchanged (fulfilment, which allocates and provisions). Routing in the
// applier rather than in the processor keeps internal/payments free of any knowledge of
// credit, which is the property that makes it reusable.
type DepositApplier struct {
	Pool *store.Pool
	// Next handles every settlement that is not a deposit.
	Next payments.Applier
}

// NewDepositApplier constructs the router.
func NewDepositApplier(pool *store.Pool, next payments.Applier) *DepositApplier {
	return &DepositApplier{Pool: pool, Next: next}
}

// ApplySettledPayment implements payments.Applier.
func (a *DepositApplier) ApplySettledPayment(ctx context.Context, tx pgx.Tx, p payments.SettledPayment) (payments.ApplyResult, error) {
	isDeposit, err := IsDepositInvoice(ctx, tx, p.InvoiceID)
	if err != nil {
		return payments.ApplyResult{}, err
	}
	if !isDeposit {
		if a.Next == nil {
			return payments.ApplyResult{}, fmt.Errorf("%w: no applier is configured", ErrInvalidRequest)
		}
		return a.Next.ApplySettledPayment(ctx, tx, p)
	}

	res, err := SettleDepositTx(ctx, tx, DepositSettlement{
		Provider:          p.Provider,
		ProviderAccount:   p.ProviderAccount,
		ProviderPaymentID: p.ProviderPaymentID,
		InvoiceID:         p.InvoiceID,
		AmountMinor:       p.AmountMinor,
		Currency:          p.Currency,
	}, nil)
	if err != nil {
		return payments.ApplyResult{}, err
	}
	return payments.ApplyResult{
		Duplicate:   res.Duplicate,
		InvoicePaid: true,
		PaymentID:   res.PaymentID,
	}, nil
}

// IsDepositInvoice reports whether an invoice is a credit deposit.
func IsDepositInvoice(ctx context.Context, q interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}, invoiceID string) (bool, error) {
	var found bool
	err := q.QueryRow(ctx, `
		SELECT true FROM invoices i WHERE i.id = $1 AND `+depositInvoicePredicate,
		invoiceID).Scan(&found)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("credit: check deposit invoice: %w", err)
	}
	return found, nil
}

// DepositInvoice returns the deposit invoice behind a checkout attempt, or ErrNotDeposit.
func (s *Service) DepositInvoice(ctx context.Context, invoiceID string) error {
	if s.pool == nil {
		return ErrInvalidRequest
	}
	ok, err := IsDepositInvoice(ctx, s.pool, invoiceID)
	if err != nil {
		return err
	}
	if !ok {
		return ErrNotDeposit
	}
	return nil
}

// voidDepositInvoice abandons a deposit whose gateway session could not be created.
func (s *Service) voidDepositInvoice(ctx context.Context, invoiceID string) error {
	return s.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			UPDATE invoices SET status = 'void' WHERE id = $1 AND status = 'open'`,
			invoiceID); err != nil {
			return fmt.Errorf("credit: void abandoned deposit: %w", err)
		}
		return nil
	})
}

// depositNow is a small helper so the deposit code has one clock.
func (s *Service) depositNow() time.Time {
	if s.Now == nil {
		return time.Now().UTC()
	}
	return s.Now()
}
