// Dashboard widgets over a selectable period (ADM-01, ADM-36, ADM-37, E5.6).
//
// This is the companion of internal/httpapi/dashboard.go, not a replacement for it. That
// file answers "how is the business doing this month" with a fixed 7/30/90-day window; this
// one answers the same question over a window the operator chooses, and adds the two figures
// the dashboard was missing — new customers and the paid-not-provisioned queue — so the
// screen can show revenue, the customer counts, the support queue and the exception list
// together instead of across three screens.
//
// Four rules shape it:
//
//  1. **Revenue comes from the ledger, not from invoice totals.** It calls the same
//     `dashboardRevenue` the existing dashboard uses, so the two screens cannot disagree
//     about what the month earned: a voided invoice was never revenue, and a credit note
//     reduces revenue by posting a debit to the same account.
//
//  2. **Every figure is one SQL aggregate.** Nothing here loads rows and sums them in Go. A
//     dashboard that adds up a page of results is a dashboard that changes its answer when
//     the page size changes, and it cannot be fixed by adding an index.
//
//  3. **The period is a parameter.** 7d/30d/90d, or `custom` with `from` and `to`. A custom
//     range is bounded: an unbounded one would let a client ask for ten years of ledger on
//     every page load, which is a denial of service with a login around it.
//
//  4. **A widget degrades honestly.** An empty period shows zero, because zero is true. A
//     figure that could not be read is `null` and is named in `unavailable`, because
//     reporting zero for a broken query would make an outage look like a quiet month. The
//     response is still 200: the dashboard is the first screen an operator opens and it has
//     to render when one number cannot be read.
package httpapi

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/catalyst/billing/internal/authn"
	"github.com/catalyst/billing/internal/ledger"
	"github.com/catalyst/billing/internal/money"
	"github.com/catalyst/billing/internal/staff"
)

func init() {
	RegisterRoutes("42-dashboard-widgets", func(r *router) { r.s.registerDashboardWidgetRoutes(r) })
}

func (s *Server) registerDashboardWidgetRoutes(r *router) {
	// The same permission as the dashboard itself. A widget is the same class of
	// information — money and customer counts — so a second permission would let a role see
	// one screen's revenue and not the other's, which is not a boundary anyone can explain.
	r.staffGet("/api/v1/staff/dashboard/widgets", authn.PermReportRead, s.handleDashboardWidgets)
}

// widgetMaxSpan bounds a custom range. A quarter is what the fixed periods cover; a year is
// generous enough for a year-end review and small enough that the ledger query stays an index
// range rather than a scan.
const widgetMaxSpan = 366 * 24 * time.Hour

// widgetQueueLimit bounds the rows of an exception queue the dashboard renders. The count
// covers every affected record; the list is what fits on the screen.
const widgetQueueLimit = 25

// dashboardWidgetsBody is the response.
//
// The pointers are deliberate. `null` says "this figure could not be read", `0` says "there
// was nothing to count", and a dashboard that cannot tell those apart is worse than one that
// shows nothing.
type dashboardWidgetsBody struct {
	// Period is what was asked for: 7d, 30d, 90d or custom.
	Period string    `json:"period"`
	From   time.Time `json:"from"`
	To     time.Time `json:"to"`
	// Days is the length of the window, so a client can label a chart without date maths.
	Days int `json:"days"`
	// GeneratedAt is when the figures were read.
	GeneratedAt time.Time `json:"generated_at"`
	// Available is false when at least one figure could not be read.
	Available bool `json:"available"`
	// Unavailable names the widgets that failed, by their JSON key, so a screen can mark the
	// card it could not fill instead of leaving an unexplained dash.
	Unavailable []string `json:"unavailable"`

	// Revenue is per currency, from the ledger. There is deliberately no cross-currency
	// total: adding USD to EUR is arithmetic on unrelated quantities.
	Revenue []dashboardRevenue `json:"revenue"`
	// ActiveCustomers is how many customers had a live service, an invoice issued, or a
	// payment received in the period.
	ActiveCustomers *int `json:"active_customers"`
	// NewCustomers is how many customer records were created in the period.
	NewCustomers *int `json:"new_customers"`
	// OpenTickets is the support queue depth *now*. A queue is a current state, not a period
	// total, so it does not move when the period changes.
	OpenTickets *int `json:"open_tickets"`
	// TicketsOpened is how many tickets were opened in the period.
	TicketsOpened *int `json:"tickets_opened"`
	// OverdueInvoices is the overdue position *now*, with the money at stake per currency.
	OverdueInvoices *dashboardOverdue `json:"overdue_invoices"`
	// PaidNotProvisioned is the exception queue the ops overview alerts on: invoices that are
	// settled while the thing they bought is not running.
	PaidNotProvisioned *dashboardExceptionQueue `json:"paid_not_provisioned"`

	// Series is the revenue chart: one point per bucket per currency, in the *same* ledger the
	// Revenue figure comes from. It is a chart rather than a second figure because a total hides
	// the shape: an operator deciding whether the month is fine needs to see the week of zero
	// that a quiet total would average away.
	Series []dashboardSeries `json:"revenue_series"`
	// Previous is the same window immediately before this one, with the deltas. It is what turns
	// "€4,120" into a number an operator can act on — the same figure was €9,800 last month, and
	// the three-count "new customers" was nine.
	Previous *dashboardPrevious `json:"previous_period"`
	// SupportTickets is the support queue itself, not a count of it. A count tells an operator
	// there is work; a table tells them which work, so the dashboard can link to the ticket that
	// has been open for nine days.
	SupportTickets []dashboardTicket `json:"support_tickets"`
}

// dashboardSeries is one currency's revenue over the period, bucketed.
type dashboardSeries struct {
	Currency string `json:"currency"`
	// Points are oldest first and every bucket in the window is present, including the empty
	// ones: a chart that skips a day with no revenue draws a line that says the shop earned
	// money on that day.
	Points []dashboardPoint `json:"points"`
}

// dashboardPoint is one bucket of the chart.
type dashboardPoint struct {
	// At is the bucket's start, in UTC, midnight-aligned for a day bucket and Monday-aligned for
	// a week bucket.
	At    time.Time `json:"at"`
	Minor int64     `json:"minor"`
}

// dashboardPrevious is the comparison window and its figures.
type dashboardPrevious struct {
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
	// Revenue is per currency, like the current period. A currency with no revenue in the previous
	// period is absent rather than zero-valued, so the client can say "new" instead of "+100%".
	Revenue         []dashboardRevenue `json:"revenue"`
	ActiveCustomers *int               `json:"active_customers"`
	NewCustomers    *int               `json:"new_customers"`
	TicketsOpened   *int               `json:"tickets_opened"`
}

// dashboardTicket is one row of the support queue.
type dashboardTicket struct {
	ID         string    `json:"id"`
	Number     string    `json:"number"`
	Subject    string    `json:"subject"`
	Department string    `json:"department"`
	Priority   string    `json:"priority"`
	Status     string    `json:"status"`
	CustomerID string    `json:"customer_id"`
	Email      string    `json:"email"`
	AssignedTo string    `json:"assigned_to"`
	CreatedAt  time.Time `json:"created_at"`
	// WaitingSince is the last activity: a ticket nobody has touched for a week is the one that
	// needs attention, and `created_at` cannot say that.
	WaitingSince time.Time `json:"waiting_since"`
}

// dashboardOverdue is the overdue-invoice widget.
type dashboardOverdue struct {
	Count int `json:"count"`
	// OldestDueAt is when the oldest unpaid invoice fell due, so an operator can tell a fresh
	// blip from a long-running leak.
	OldestDueAt *time.Time `json:"oldest_due_at"`
	// Money is the outstanding amount per currency.
	Money []dashboardRevenue `json:"money"`
}

// dashboardExceptionQueue is one exception from internal/ops, with the records behind it.
type dashboardExceptionQueue struct {
	Code     string `json:"code"`
	Title    string `json:"title"`
	Detail   string `json:"detail"`
	Action   string `json:"action"`
	Severity string `json:"severity"`
	// Count is every affected record, not just the rows below.
	Count    int        `json:"count"`
	OldestAt *time.Time `json:"oldest_at"`
	// Money is the amount at stake per currency.
	Money []dashboardRevenue `json:"money"`
	// Items is the queue itself, oldest first, so the card can link to the invoice.
	Items []dashboardQueueItem `json:"items"`
	// Truncated is true when Count exceeds the rows returned.
	Truncated bool `json:"truncated"`
}

// dashboardQueueItem is one paid-not-provisioned invoice.
type dashboardQueueItem struct {
	InvoiceID  string         `json:"invoice_id"`
	Number     string         `json:"number"`
	CustomerID string         `json:"customer_id"`
	Email      string         `json:"email"`
	Total      map[string]any `json:"total"`
	IssuedAt   *time.Time     `json:"issued_at"`
	OrderID    string         `json:"order_id"`
}

// handleDashboardWidgets serves the period-scoped widget set.
func (s *Server) handleDashboardWidgets(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "database_not_configured",
			"no database is configured")
		return
	}
	from, to, period, err := widgetWindow(r)
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_period", err.Error())
		return
	}

	now := time.Now().UTC()
	body := dashboardWidgetsBody{
		Period:      period,
		From:        from,
		To:          to,
		Days:        int(to.Sub(from).Hours() / 24),
		GeneratedAt: now,
		Available:   true,
		Unavailable: []string{},
		Revenue:     []dashboardRevenue{},
	}

	// Each widget is independent and each failure is named. One broken figure marks the set
	// unavailable rather than blanking the page.
	fail := func(name string, err error) {
		body.Available = false
		body.Unavailable = append(body.Unavailable, name)
		s.cfg.Logger.ErrorContext(r.Context(), "dashboard widget failed",
			"widget", name, "error", err)
	}

	// Revenue: the ledger, through the same function the fixed-period dashboard uses, so the
	// two screens cannot disagree about what a period earned.
	revenue := dashboardBody{Revenue: []dashboardRevenue{}}
	if err := s.dashboardRevenue(r, from, to, &revenue); err != nil {
		fail("revenue", err)
	} else {
		body.Revenue = revenue.Revenue
	}

	if n, err := s.widgetActiveCustomers(r.Context(), from, to); err != nil {
		fail("active_customers", err)
	} else {
		body.ActiveCustomers = &n
	}
	if n, err := s.widgetCount(r.Context(),
		`SELECT count(*) FROM customers WHERE created_at >= $1 AND created_at <= $2`,
		from, to); err != nil {
		fail("new_customers", err)
	} else {
		body.NewCustomers = &n
	}
	if n, err := s.widgetCount(r.Context(),
		`SELECT count(*) FROM tickets WHERE status <> 'closed'`); err != nil {
		fail("open_tickets", err)
	} else {
		body.OpenTickets = &n
	}
	if n, err := s.widgetCount(r.Context(),
		`SELECT count(*) FROM tickets WHERE created_at >= $1 AND created_at <= $2`,
		from, to); err != nil {
		fail("tickets_opened", err)
	} else {
		body.TicketsOpened = &n
	}
	if overdue, err := s.widgetOverdue(r.Context()); err != nil {
		fail("overdue_invoices", err)
	} else {
		body.OverdueInvoices = overdue
	}
	if queue, err := s.widgetPaidNotProvisioned(r.Context()); err != nil {
		fail("paid_not_provisioned", err)
	} else {
		body.PaidNotProvisioned = queue
	}
	if series, err := s.widgetRevenueSeries(r.Context(), from, to); err != nil {
		fail("revenue_series", err)
	} else {
		body.Series = series
	}
	if prev, err := s.widgetPrevious(r.Context(), from, to); err != nil {
		fail("previous_period", err)
	} else {
		body.Previous = prev
	}
	if tickets, err := s.widgetSupportQueue(r.Context()); err != nil {
		fail("support_tickets", err)
	} else {
		body.SupportTickets = tickets
	}

	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, body)
}

// widgetWindow resolves the period parameter into a half-open window.
func widgetWindow(r *http.Request) (time.Time, time.Time, string, error) {
	now := time.Now().UTC()
	period := strings.TrimSpace(r.URL.Query().Get("period"))
	if period == "" {
		period = "30d"
	}

	if period != "custom" {
		window, ok := dashboardPeriods[period]
		if !ok {
			return time.Time{}, time.Time{}, "", errInvalidWidgetPeriod(
				"period must be one of 7d, 30d, 90d or custom")
		}
		return now.Add(-window), now, period, nil
	}

	from, err := parseWidgetTime(r.URL.Query().Get("from"), false)
	if err != nil {
		return time.Time{}, time.Time{}, "", errInvalidWidgetPeriod(
			"a custom period needs `from` as an RFC3339 timestamp or a YYYY-MM-DD date")
	}
	to, err := parseWidgetTime(r.URL.Query().Get("to"), true)
	if err != nil {
		return time.Time{}, time.Time{}, "", errInvalidWidgetPeriod(
			"a custom period needs `to` as an RFC3339 timestamp or a YYYY-MM-DD date")
	}
	if !to.After(from) {
		return time.Time{}, time.Time{}, "", errInvalidWidgetPeriod("`to` must be after `from`")
	}
	if to.Sub(from) > widgetMaxSpan {
		return time.Time{}, time.Time{}, "", errInvalidWidgetPeriod(
			"a custom period cannot be longer than 366 days")
	}
	return from, to, period, nil
}

// errInvalidWidgetPeriod is a plain error whose message is the problem detail.
type errInvalidWidgetPeriod string

func (e errInvalidWidgetPeriod) Error() string { return string(e) }

// parseWidgetTime accepts an RFC3339 timestamp or a calendar date.
//
// A date input sends `2026-03-01`, and a date has no time of day: the start of the day is the
// honest reading of `from`, and the *end* of the day is the honest reading of `to` — a range
// that stopped at midnight would silently exclude the day the operator asked for.
func parseWidgetTime(raw string, endOfDay bool) (time.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, errInvalidWidgetPeriod("a date is required")
	}
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t.UTC(), nil
	}
	day, err := time.Parse("2006-01-02", raw)
	if err != nil {
		return time.Time{}, err
	}
	if endOfDay {
		return day.Add(24*time.Hour - time.Nanosecond).UTC(), nil
	}
	return day.UTC(), nil
}

// widgetCount runs a counting query and returns the figure.
func (s *Server) widgetCount(ctx context.Context, query string, args ...any) (int, error) {
	var n int
	if err := s.pool.QueryRow(ctx, query, args...).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// widgetActiveCustomers counts customers who were active in the period.
//
// Active is "had a live service, or was invoiced, or paid". Counting every row in `customers`
// would count a prospect who signed up and left, and a business that only ever grows is the
// most flattering possible bug.
func (s *Server) widgetActiveCustomers(ctx context.Context, from, to time.Time) (int, error) {
	return s.widgetCount(ctx, `
		SELECT count(*) FROM customers c
		WHERE EXISTS (SELECT 1 FROM services s
		              WHERE s.customer_id = c.id
		                AND s.status IN ('active', 'suspended', 'provisioning'))
		   OR EXISTS (SELECT 1 FROM invoices i
		              WHERE i.customer_id = c.id
		                AND COALESCE(i.issued_at, i.created_at) >= $1
		                AND COALESCE(i.issued_at, i.created_at) <= $2)
		   OR EXISTS (SELECT 1 FROM payments p
		              JOIN payment_allocations pa ON pa.payment_id = p.id
		              JOIN invoices i ON i.id = pa.invoice_id
		              WHERE i.customer_id = c.id
		                AND p.received_at >= $1
		                AND p.received_at <= $2)`, from, to)
}

// widgetOverdue reads the overdue position: how many, since when, and how much per currency.
//
// Overdue is a current state, not a period total, so it is not scoped to the window. The
// amount is what is still owed on each invoice, not its total: an invoice half paid is half
// overdue.
func (s *Server) widgetOverdue(ctx context.Context) (*dashboardOverdue, error) {
	out := &dashboardOverdue{Money: []dashboardRevenue{}}
	if err := s.pool.QueryRow(ctx, `
		SELECT count(*), min(due_at) FROM invoices
		WHERE status = 'open' AND due_at IS NOT NULL AND due_at < now()`,
	).Scan(&out.Count, &out.OldestDueAt); err != nil {
		return nil, err
	}

	rows, err := s.pool.Query(ctx, `
		SELECT currency, coalesce(sum(total_minor - amount_paid_minor), 0)
		FROM invoices
		WHERE status = 'open' AND due_at IS NOT NULL AND due_at < now()
		GROUP BY currency
		ORDER BY currency`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			code  string
			minor int64
		)
		if err := rows.Scan(&code, &minor); err != nil {
			return nil, err
		}
		out.Money = append(out.Money, revenueOf(code, minor))
	}
	return out, rows.Err()
}

// widgetPaidNotProvisioned reads the exception queue the ops overview alerts on.
//
// The predicate is the same one ops.paidNotProvisioned uses — the invoice is settled and no
// working service exists for what its order bought — stated here as one query that returns
// the rows *and* the count, because a dashboard card needs both and two queries could be
// answered from two different commits. internal/ops stays the alerting source of truth; this
// is the same question asked for a screen, and the count in the card header is the count of
// the rows below it.
func (s *Server) widgetPaidNotProvisioned(ctx context.Context) (*dashboardExceptionQueue, error) {
	out := &dashboardExceptionQueue{
		Code:     "paid_not_provisioned",
		Title:    "Paid, but the service is not running",
		Detail:   "The invoice is settled and no active service exists for what the order bought.",
		Action:   "Inspect the service operation; if it is stuck, requeue it or provision manually and record the external id.",
		Severity: "critical",
		Money:    []dashboardRevenue{},
		Items:    []dashboardQueueItem{},
	}

	rows, err := s.pool.Query(ctx, `
		SELECT i.id::text, coalesce(i.number, ''), i.customer_id::text,
		       coalesce(c.email, ''), i.currency, i.total_minor,
		       coalesce(i.issued_at, i.created_at), o.id::text,
		       count(*) OVER () AS total
		FROM invoices i
		JOIN orders o ON o.id = i.order_id
		JOIN customers c ON c.id = i.customer_id
		WHERE i.status = 'paid'
		  AND NOT EXISTS (
		      SELECT 1
		      FROM order_items oi
		      JOIN services s ON s.product_id = (
		          SELECT pv.product_id FROM price_versions pv WHERE pv.id = oi.price_version_id
		      )
		                     AND s.customer_id = i.customer_id
		                     AND s.status IN ('active', 'suspended')
		      WHERE oi.order_id = o.id
		  )
		ORDER BY COALESCE(i.issued_at, i.created_at)
		LIMIT $1`, widgetQueueLimit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var (
			item     dashboardQueueItem
			currency string
			minor    int64
			issuedAt time.Time
		)
		if err := rows.Scan(&item.InvoiceID, &item.Number, &item.CustomerID, &item.Email,
			&currency, &minor, &issuedAt, &item.OrderID, &out.Count); err != nil {
			return nil, err
		}
		cur, err := staff.CurrencyOf(currency)
		if err != nil {
			// staff.CurrencyOf falls back to scale 2 for a code outside the ISO table, so this
			// is unreachable; returning the error beats inventing a scale.
			return nil, err
		}
		item.Total = amountJSON(money.New(minor, cur))
		at := issuedAt.UTC()
		item.IssuedAt = &at
		if out.OldestAt == nil {
			out.OldestAt = &at
		}
		out.Items = append(out.Items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out.Truncated = out.Count > len(out.Items)

	// The money at stake, per currency. One query for the whole queue rather than one per
	// row: the card says "this much is paid for nothing".
	moneyRows, err := s.pool.Query(ctx, `
		SELECT i.currency, coalesce(sum(i.total_minor), 0)
		FROM invoices i
		JOIN orders o ON o.id = i.order_id
		WHERE i.status = 'paid'
		  AND NOT EXISTS (
		      SELECT 1
		      FROM order_items oi
		      JOIN services s ON s.product_id = (
		          SELECT pv.product_id FROM price_versions pv WHERE pv.id = oi.price_version_id
		      )
		                     AND s.customer_id = i.customer_id
		                     AND s.status IN ('active', 'suspended')
		      WHERE oi.order_id = o.id
		  )
		GROUP BY i.currency
		ORDER BY i.currency`)
	if err != nil {
		return nil, err
	}
	defer moneyRows.Close()
	for moneyRows.Next() {
		var (
			code  string
			minor int64
		)
		if err := moneyRows.Scan(&code, &minor); err != nil {
			return nil, err
		}
		out.Money = append(out.Money, revenueOf(code, minor))
	}
	return out, moneyRows.Err()
}

// widgetBucket picks the chart bucket for a window.
//
// A 90-day window bucketed by day is ninety points on a card that is 300px wide — a smear, not a
// trend. Weeks for anything longer than a quarter of the dashboard's range, days otherwise, and
// the boundary is a constant so the client can label the axis without re-deriving it.
func widgetBucket(days int) (unit string, label string) {
	if days > 45 {
		return "week", "week"
	}
	return "day", "day"
}

// widgetRevenueSeries reads the revenue chart from the ledger.
//
// It is the same account and the same net-of-credits arithmetic as the Revenue figure above, in
// one query that groups by bucket, so the chart's points add up to the total it is drawn beside.
// A chart that disagreed with its own headline number is worse than no chart.
func (s *Server) widgetRevenueSeries(ctx context.Context, from, to time.Time) ([]dashboardSeries, error) {
	unit, _ := widgetBucket(int(to.Sub(from).Hours() / 24))
	// date_trunc's argument is a literal here, chosen from the two allowed values above; it is
	// formatted into the statement rather than bound because PostgreSQL does not accept a
	// parameter there. The value cannot come from a request.
	trunc := "day"
	if unit == "week" {
		trunc = "week"
	}

	rows, err := s.pool.Query(ctx, `
		SELECT currency,
		       date_trunc('`+trunc+`', created_at) AS bucket,
		       COALESCE(SUM(credit_minor), 0) - COALESCE(SUM(debit_minor), 0) AS net_minor
		FROM journal_entries
		WHERE account_code = $1
		  AND created_at >= $2
		  AND created_at <= $3
		GROUP BY currency, bucket
		ORDER BY currency, bucket`, ledger.AccountRevenue, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	// A dense series: every bucket in the window exists, even with no revenue. The fill happens
	// here rather than in the chart because "no row" and "zero" are the same number on a graph and
	// different facts in a query, and only this side knows which buckets are missing.
	byCurrency := map[string]map[time.Time]int64{}
	for rows.Next() {
		var (
			code   string
			bucket time.Time
			minor  int64
		)
		if err := rows.Scan(&code, &bucket, &minor); err != nil {
			return nil, err
		}
		if byCurrency[code] == nil {
			byCurrency[code] = map[time.Time]int64{}
		}
		byCurrency[code][bucket.UTC()] = minor
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	codes := make([]string, 0, len(byCurrency))
	for code := range byCurrency {
		codes = append(codes, code)
	}
	sort.Strings(codes)

	out := make([]dashboardSeries, 0, len(codes))
	for _, code := range codes {
		points := make([]dashboardPoint, 0, 32)
		for at := widgetBucketStart(from, unit); !at.After(to); at = widgetNextBucket(at, unit) {
			points = append(points, dashboardPoint{At: at, Minor: byCurrency[code][at]})
		}
		out = append(out, dashboardSeries{Currency: code, Points: points})
	}
	return out, nil
}

// widgetBucketStart aligns a window's start to the bucket it falls in.
//
// A day bucket starts at midnight UTC and a week bucket on Monday, which is what date_trunc
// returns; the arithmetic mirrors it so the fill lines up with the sums rather than drifting by
// the part of the day the window happens to start in.
func widgetBucketStart(from time.Time, unit string) time.Time {
	utc := from.UTC()
	start := time.Date(utc.Year(), utc.Month(), utc.Day(), 0, 0, 0, 0, time.UTC)
	if unit == "week" {
		// Go's Weekday is Sunday=0; date_trunc('week') is ISO, which starts on Monday.
		offset := (int(start.Weekday()) + 6) % 7
		start = start.AddDate(0, 0, -offset)
	}
	return start
}

// widgetNextBucket advances one bucket.
func widgetNextBucket(at time.Time, unit string) time.Time {
	if unit == "week" {
		return at.AddDate(0, 0, 7)
	}
	return at.AddDate(0, 0, 1)
}

// widgetPrevious reads the window immediately before this one.
//
// The previous window is the same length, ending where this one starts: a "month" is compared
// with the previous month rather than with "the 30 days before today", because an operator
// comparing a calendar month expects the calendar.
//
// It is a separate query rather than a second bucket in the same one because the comparison is of
// *figures* (net revenue per currency, how many customers were active, how many tickets opened),
// and mixing the two windows in one row would make every predicate a conditional.
func (s *Server) widgetPrevious(ctx context.Context, from, to time.Time) (*dashboardPrevious, error) {
	span := to.Sub(from)
	prevFrom, prevTo := from.Add(-span), from
	out := &dashboardPrevious{From: prevFrom, To: prevTo, Revenue: []dashboardRevenue{}}

	rows, err := s.pool.Query(ctx, `
		SELECT currency,
		       COALESCE(SUM(credit_minor), 0) - COALESCE(SUM(debit_minor), 0) AS net_minor
		FROM journal_entries
		WHERE account_code = $1
		  AND created_at >= $2
		  AND created_at < $3
		GROUP BY currency
		ORDER BY currency`, ledger.AccountRevenue, prevFrom, prevTo)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			code  string
			minor int64
		)
		if err := rows.Scan(&code, &minor); err != nil {
			return nil, err
		}
		out.Revenue = append(out.Revenue, revenueOf(code, minor))
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	if n, err := s.widgetActiveCustomers(ctx, prevFrom, prevTo); err != nil {
		return nil, err
	} else {
		out.ActiveCustomers = &n
	}
	if n, err := s.widgetCount(ctx,
		`SELECT count(*) FROM customers WHERE created_at >= $1 AND created_at < $2`,
		prevFrom, prevTo); err != nil {
		return nil, err
	} else {
		out.NewCustomers = &n
	}
	if n, err := s.widgetCount(ctx,
		`SELECT count(*) FROM tickets WHERE created_at >= $1 AND created_at < $2`,
		prevFrom, prevTo); err != nil {
		return nil, err
	} else {
		out.TicketsOpened = &n
	}
	return out, nil
}

// widgetSupportQueue reads the rows behind the support count.
//
// Oldest waiting first rather than newest: the count above this table says "there are nine open
// tickets", and the useful next sentence is "and one of them has been waiting nine days". A queue
// sorted by creation date would put the newest at the top, which is the opposite of what needs a
// human.
func (s *Server) widgetSupportQueue(ctx context.Context) ([]dashboardTicket, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT t.id::text, COALESCE(t.number, ''), t.subject, t.department, t.priority,
		       t.status, t.customer_id::text, COALESCE(c.email, ''),
		       COALESCE(t.assigned_to::text, ''), t.created_at,
		       COALESCE(t.last_reply_at, t.created_at)
		FROM tickets t
		JOIN customers c ON c.id = t.customer_id
		WHERE t.status <> 'closed'
		ORDER BY COALESCE(t.last_reply_at, t.created_at), t.created_at
		LIMIT $1`, widgetQueueLimit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]dashboardTicket, 0, widgetQueueLimit)
	for rows.Next() {
		var ticket dashboardTicket
		if err := rows.Scan(&ticket.ID, &ticket.Number, &ticket.Subject, &ticket.Department,
			&ticket.Priority, &ticket.Status, &ticket.CustomerID, &ticket.Email,
			&ticket.AssignedTo, &ticket.CreatedAt, &ticket.WaitingSince); err != nil {
			return nil, err
		}
		out = append(out, ticket)
	}
	return out, rows.Err()
}
