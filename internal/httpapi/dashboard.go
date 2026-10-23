// The operator dashboard (ADM-01, E5.6).
//
// It complements `internal/ops` rather than repeating it. The ops overview answers "what
// needs a decision right now" — exceptions with a recovery path. This answers "how is the
// business doing over a period" — revenue, invoice volume, customers, tickets, recurring
// revenue. The dashboard screen renders both, and neither is a superset of the other.
//
// The one rule that shapes every number here: **money never leaves its currency**. A shop
// that sells in USD and EUR has two revenue figures, not one, because adding them would be
// arithmetic on unrelated quantities and there is no exchange rate anywhere in this system.
// So revenue is a list per currency, and the response has no `total`.
//
// Revenue is read from the *ledger*, not from invoice totals. That is the difference
// between a number that reflects the books and a number that reflects paperwork: a voided
// invoice was never revenue, a credit note reduces revenue by posting a debit to the same
// account, and a proforma has not been issued at all. `journal_entries` on the revenue
// account is where all three are already recorded, so summing it is the only way to get a
// figure that agrees with the trial balance.

package httpapi

import (
	"net/http"
	"strings"
	"time"

	"github.com/catalyst/billing/internal/authn"
	"github.com/catalyst/billing/internal/calendar"
	"github.com/catalyst/billing/internal/ledger"
	"github.com/catalyst/billing/internal/ops"
	"github.com/catalyst/billing/internal/staff"
)

func init() {
	RegisterRoutes("41-operator-dashboard", registerOperatorDashboard)
}

func registerOperatorDashboard(r *router) {
	// report:read is the permission that already gates the ops overview. The dashboard is
	// the same class of information — money and customer counts — so it must not need a
	// second permission that would let a role see one and not the other.
	r.staffGet("/api/v1/staff/dashboard", authn.PermReportRead, r.s.handleDashboard)
}

// dashboardPeriods maps the accepted `period` values to a duration.
//
// A closed set rather than an arbitrary `from`/`to`: the dashboard's cards are designed
// around three windows, and an open range would let a client ask for ten years of ledger
// on every page load.
var dashboardPeriods = map[string]time.Duration{
	"7d":  7 * 24 * time.Hour,
	"30d": 30 * 24 * time.Hour,
	"90d": 90 * 24 * time.Hour,
}

// dashboardRevenue is one currency's revenue over the period.
type dashboardRevenue struct {
	Currency string `json:"currency"`
	Scale    int    `json:"scale"`
	Minor    int64  `json:"minor"`
	Display  string `json:"display"`
}

// dashboardInvoiceCount is one status's count.
type dashboardInvoiceCount struct {
	Status string `json:"status"`
	Count  int    `json:"count"`
}

// dashboardRecurring is the MRR-ish figure, per currency.
//
// It is "MRR-ish" on purpose: it normalises each active subscription's price to a monthly
// amount using its interval, so a quarterly subscription contributes a third of its price
// per month. It is not an accounting figure — the ledger is — but it is the number an
// operator uses to see whether recurring revenue is growing.
type dashboardRecurring struct {
	Currency string `json:"currency"`
	Scale    int    `json:"scale"`
	Minor    int64  `json:"minor"`
	Display  string `json:"display"`
	// Subscriptions is how many active subscriptions contribute to this figure.
	Subscriptions int `json:"subscriptions"`
}

// dashboardBody is the response.
type dashboardBody struct {
	Period string    `json:"period"`
	From   time.Time `json:"from"`
	To     time.Time `json:"to"`
	// Revenue is per currency. There is deliberately no cross-currency total.
	Revenue []dashboardRevenue `json:"revenue"`
	// Invoices counts by status over the period, for invoices *issued* in it.
	Invoices []dashboardInvoiceCount `json:"invoices"`
	// ActiveCustomers is how many customers exist with at least one live service or an
	// invoice in the period. Counting every row in `customers` would count a deleted
	// prospect as active.
	ActiveCustomers int `json:"active_customers"`
	// OpenTickets is the support queue depth right now, not over the period: a queue is
	// a current state.
	OpenTickets int `json:"open_tickets"`
	// Recurring is the normalised monthly recurring revenue, per currency.
	Recurring []dashboardRecurring `json:"recurring"`
	// Exceptions is the recent exception count from internal/ops, so the dashboard can
	// show "N things need attention" next to the revenue chart without a second request.
	Exceptions int `json:"exceptions"`
	// ExceptionCritical is how many of those are critical.
	ExceptionCritical int `json:"exception_critical"`
	// Available is false when a figure could not be read. Reporting zero instead would
	// make a broken database look like an empty business, which is the most misleading
	// thing a dashboard can do.
	Available bool `json:"available"`
	// GeneratedAt is when the figures were read.
	GeneratedAt time.Time `json:"generated_at"`
}

// handleDashboard serves the dashboard figures.
func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "database_not_configured",
			"no database is configured")
		return
	}

	period := strings.TrimSpace(r.URL.Query().Get("period"))
	if period == "" {
		period = "30d"
	}
	window, ok := dashboardPeriods[period]
	if !ok {
		writeProblem(w, http.StatusBadRequest, "invalid_period",
			"period must be one of 7d, 30d, 90d")
		return
	}

	now := time.Now().UTC()
	from := now.Add(-window)
	body := dashboardBody{
		Period:      period,
		From:        from,
		To:          now,
		Revenue:     []dashboardRevenue{},
		Invoices:    []dashboardInvoiceCount{},
		Recurring:   []dashboardRecurring{},
		GeneratedAt: now,
		Available:   true,
	}

	// Each query is independent, and a failure marks the set unavailable rather than
	// blanking the page. The dashboard is the first screen an operator opens, so it has to
	// render even when one figure cannot be read.
	if err := s.dashboardRevenue(r, from, now, &body); err != nil {
		body.Available = false
		s.cfg.Logger.ErrorContext(r.Context(), "dashboard revenue failed", "error", err)
	}
	if err := s.dashboardInvoiceCounts(r, from, now, &body); err != nil {
		body.Available = false
		s.cfg.Logger.ErrorContext(r.Context(), "dashboard invoice counts failed", "error", err)
	}
	if err := s.dashboardActiveCustomers(r, from, now, &body); err != nil {
		body.Available = false
		s.cfg.Logger.ErrorContext(r.Context(), "dashboard customers failed", "error", err)
	}
	if err := s.dashboardOpenTickets(r, &body); err != nil {
		body.Available = false
		s.cfg.Logger.ErrorContext(r.Context(), "dashboard tickets failed", "error", err)
	}
	if err := s.dashboardRecurring(r, &body); err != nil {
		body.Available = false
		s.cfg.Logger.ErrorContext(r.Context(), "dashboard recurring failed", "error", err)
	}

	// The exception count comes from the ops package, so the dashboard and the overview
	// cannot disagree about what needs attention.
	overview := ops.New(s.pool).Overview(r.Context())
	body.Exceptions = len(overview.Exceptions)
	for _, exc := range overview.Exceptions {
		if exc.Severity == ops.SeverityCritical {
			body.ExceptionCritical++
		}
	}
	if !overview.Counts.Available {
		body.Available = false
	}

	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, body)
}

// dashboardRevenue sums the revenue account over the period, per currency.
//
// Revenue is credits minus debits: an invoice credits revenue, and a credit note debits
// it. Summing only credits would report gross revenue that ignores every refund, and
// summing invoice totals would count a void invoice as revenue that was never earned.
func (s *Server) dashboardRevenue(r *http.Request, from, to time.Time, body *dashboardBody) error {
	rows, err := s.pool.Query(r.Context(), `
		SELECT currency,
		       COALESCE(SUM(credit_minor), 0) - COALESCE(SUM(debit_minor), 0) AS net_minor
		FROM journal_entries
		WHERE account_code = $1
		  AND created_at >= $2
		  AND created_at <= $3
		GROUP BY currency
		ORDER BY currency`,
		ledger.AccountRevenue, from, to)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var (
			code  string
			minor int64
		)
		if err := rows.Scan(&code, &minor); err != nil {
			return err
		}
		body.Revenue = append(body.Revenue, revenueOf(code, minor))
	}
	return rows.Err()
}

// revenueOf renders one per-currency revenue figure.
func revenueOf(code string, minor int64) dashboardRevenue {
	cur, err := staff.CurrencyOf(code)
	if err != nil {
		// An unreadable currency code is a stored value this system wrote, so falling back
		// to scale 2 is better than dropping the row: a revenue figure that disappears is
		// worse than one rendered at the wrong scale.
		return dashboardRevenue{Currency: code, Scale: 2, Minor: minor, Display: formatMinor(minor, 2)}
	}
	scale := int(cur.Scale())
	return dashboardRevenue{
		Currency: cur.Code(),
		Scale:    scale,
		Minor:    minor,
		Display:  formatMinor(minor, scale),
	}
}

// dashboardInvoiceCounts counts invoices issued in the period, by status.
func (s *Server) dashboardInvoiceCounts(r *http.Request, from, to time.Time, body *dashboardBody) error {
	rows, err := s.pool.Query(r.Context(), `
		SELECT status, count(*)
		FROM invoices
		WHERE COALESCE(issued_at, created_at) >= $1
		  AND COALESCE(issued_at, created_at) <= $2
		GROUP BY status
		ORDER BY status`, from, to)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var (
			status string
			count  int
		)
		if err := rows.Scan(&status, &count); err != nil {
			return err
		}
		body.Invoices = append(body.Invoices, dashboardInvoiceCount{Status: status, Count: count})
	}
	return rows.Err()
}

// dashboardActiveCustomers counts customers with a live service or an invoice in the
// period.
//
// A customer with a terminated service and no invoice in the window is not active, and
// counting the whole table would report a business that only ever grows.
func (s *Server) dashboardActiveCustomers(r *http.Request, from, to time.Time, body *dashboardBody) error {
	return s.pool.QueryRow(r.Context(), `
		SELECT count(*) FROM customers c
		WHERE EXISTS (SELECT 1 FROM services s
		              WHERE s.customer_id = c.id AND s.status IN ('active', 'suspended', 'provisioning'))
		   OR EXISTS (SELECT 1 FROM invoices i
		              WHERE i.customer_id = c.id
		                AND COALESCE(i.issued_at, i.created_at) >= $1
		                AND COALESCE(i.issued_at, i.created_at) <= $2)`,
		from, to).Scan(&body.ActiveCustomers)
}

// dashboardOpenTickets counts the support queue.
func (s *Server) dashboardOpenTickets(r *http.Request, body *dashboardBody) error {
	return s.pool.QueryRow(r.Context(),
		`SELECT count(*) FROM tickets WHERE status <> 'closed'`).Scan(&body.OpenTickets)
}

// dashboardRecurring computes normalised monthly recurring revenue per currency.
//
// Each active subscription's price is divided by its interval, so a quarterly price contributes
// a third and an annual price a twelfth. A day-count interval ("every 45 days") has no month to
// divide by, so it is normalised over a 365-day year: a 45-day price contributes 365/(45*12) of
// itself per month. That is a *dashboard* figure, not a billable amount — the only place in the
// system where a year is assumed to be 365 days is here, and it is stated rather than implied.
//
// The arithmetic is integer throughout. A rounding error of one minor unit per subscription is
// acceptable for a headline number, and a float would be unacceptable anywhere in this system.
func (s *Server) dashboardRecurring(r *http.Request, body *dashboardBody) error {
	rows, err := s.pool.Query(r.Context(), `
		SELECT pv.currency, sub.interval, pv.amount_minor
		FROM subscriptions sub
		JOIN price_versions pv ON pv.id = sub.price_version_id
		WHERE sub.status IN ('active', 'past_due')`)
	if err != nil {
		return err
	}
	defer rows.Close()

	type acc struct {
		minor int64
		count int
	}
	byCurrency := map[string]*acc{}
	order := []string{}
	for rows.Next() {
		var (
			code     string
			interval string
			amount   int64
		)
		if err := rows.Scan(&code, &interval, &amount); err != nil {
			return err
		}
		num, den, ok := monthlyShare(interval)
		if !ok {
			continue
		}
		a := byCurrency[code]
		if a == nil {
			a = &acc{}
			byCurrency[code] = a
			order = append(order, code)
		}
		a.minor += mulDiv(amount, num, den)
		a.count++
	}
	if err := rows.Err(); err != nil {
		return err
	}

	for _, code := range order {
		a := byCurrency[code]
		cur, err := staff.CurrencyOf(code)
		scale := 2
		rendered := code
		if err == nil {
			scale = int(cur.Scale())
			rendered = cur.Code()
		}
		body.Recurring = append(body.Recurring, dashboardRecurring{
			Currency:      rendered,
			Scale:         scale,
			Minor:         a.minor,
			Display:       formatMinor(a.minor, scale),
			Subscriptions: a.count,
		})
	}
	return nil
}

// monthlyShare returns the fraction of a price that one month represents, as a numerator and
// a denominator.
//
// A calendar interval is one month divided by its length in months. A day-count interval has no
// length in months, so it is normalised over a 365-day year: every N days is N/365 of a year,
// which is 365/(N*12) of a year per month. The denominator is never zero for a value the
// database accepts, and an unknown interval reports false rather than dividing by zero.
func monthlyShare(interval string) (num, den int64, ok bool) {
	if days, dayCount := calendar.ParseDayInterval(interval); dayCount {
		return 365, int64(days) * 12, true
	}
	if months := intervalMonths(interval); months > 0 {
		return 1, int64(months), true
	}
	return 0, 0, false
}

// mulDiv computes v*num/den in integer arithmetic without overflowing on a large amount: the
// quotient and the remainder are scaled separately, so an amount near the int64 limit cannot
// wrap.
func mulDiv(v, num, den int64) int64 {
	if den == 0 {
		return 0
	}
	return v/den*num + v%den*num/den
}

// intervalMonths maps a calendar subscription interval to its length in months.
func intervalMonths(interval string) int {
	switch interval {
	case "monthly":
		return 1
	case "quarterly":
		return 3
	case "semiannual":
		return 6
	case "annual":
		return 12
	default:
		return 0
	}
}
