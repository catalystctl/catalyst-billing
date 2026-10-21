package staff

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/catalyst/billing/internal/money"
	"github.com/jackc/pgx/v5"
)

// CustomerRow is one entry of the customer list.
type CustomerRow struct {
	ID        string
	Email     string
	Name      string
	CreatedAt time.Time
	// OpenInvoices is the number of invoices that are still open.
	OpenInvoices int
	// Outstanding is what the customer owes, per currency.
	Outstanding []Amount
	// Services is the number of services, in any state.
	Services int
	// HasLogin explains why a customer cannot see the portal, which is the most common
	// support question. It is true when a customer_accounts row exists.
	HasLogin bool
}

// CustomerFilter narrows the customer list.
type CustomerFilter struct {
	// Query matches the email or the name, case-insensitively.
	Query string
	// Sort is created_at, email or name. The default is created_at descending, which is
	// what an operator looking for a recent signup expects.
	Sort      string
	Direction SortDirection
}

// CustomerDetail is the joined customer view.
type CustomerDetail struct {
	Customer      CustomerRow
	Accounts      []AccountRow
	Subscriptions []SubscriptionRow
	Services      []ServiceRow
	Invoices      []InvoiceRow
	Balance       Balance
	Holds         []HoldRow
	AuthEvents    []AuthEventRow
}

// AccountRow is one login attached to a customer.
type AccountRow struct {
	UserID     string
	Email      string
	Role       string
	LastSeenAt *time.Time
}

// SubscriptionRow is one subscription.
type SubscriptionRow struct {
	ID                 string
	Status             string
	Interval           string
	NextBillingAt      time.Time
	CurrentPeriodStart time.Time
	CurrentPeriodEnd   time.Time
	CancelAtPeriodEnd  bool
	Product            string
}

// ServiceRow is one service.
type ServiceRow struct {
	ID           string
	CustomerID   string
	CustomerMail string
	Product      string
	ProductID    string
	Status       string
	ExternalID   string
	Generation   int64
	NextBillAt   *time.Time
	CreatedAt    time.Time
	OpenOps      int
	LastError    string
}

// InvoiceRow is one invoice in a list.
type InvoiceRow struct {
	ID       string
	Number   string
	Status   string
	Currency string
	Total    int64
	Paid     int64
	Due      int64
	IssuedAt *time.Time
	DueAt    *time.Time
	Overdue  bool
	// Customer is filled by the staff lists; the portal lists leave it zero.
	CustomerID    string
	CustomerEmail string
}

// Amount renders the invoice's total.
func (r InvoiceRow) TotalAmount() (Amount, error) { return Amounts(r.Total, r.Currency) }

// Balance is what the customer owes and holds, per currency.
//
// It is a list rather than a total because a customer can hold invoices in two currencies,
// and a single number would be a lie without a conversion rate the product does not have.
type Balance struct {
	Outstanding []Amount
	Credit      []Amount
}

// HoldRow is a manual hold.
type HoldRow struct {
	ID        string
	ServiceID string
	Reason    string
	ExpiresAt *time.Time
	CreatedBy string
	CreatedAt time.Time
}

// AuthEventRow is one authentication event.
type AuthEventRow struct {
	Event     string
	Detail    string
	IP        string
	CreatedAt time.Time
}

// Customers lists customers with the aggregates the operator screen shows.
//
// The aggregates are computed with lateral subqueries rather than joins: a join against
// invoices and services at once multiplies rows, and the count would be wrong for exactly
// the customers with the most activity.
func (s *Service) Customers(ctx context.Context, f CustomerFilter, limit int, cursor string) ([]CustomerRow, string, error) {
	limit = pageLimit(limit)

	sortColumn := "c.created_at"
	switch f.Sort {
	case "", "created_at":
		sortColumn = "c.created_at"
	case "email":
		sortColumn = "lower(c.email)"
	case "name":
		sortColumn = "lower(c.name)"
	default:
		return nil, "", fmt.Errorf("%w: unknown sort %q", ErrInvalidQuery, f.Sort)
	}
	direction := "DESC"
	if f.Direction == SortAsc {
		direction = "ASC"
	}

	var b queryBuilder
	if q := strings.TrimSpace(f.Query); q != "" {
		if len(q) < 2 {
			// A one-character search matches nearly everything and is a scan. Refusing it
			// is better than a request that looks broken because it is slow.
			return nil, "", fmt.Errorf("%w: the search needs at least two characters", ErrInvalidQuery)
		}
		pattern := "%" + escapeLike(q) + "%"
		b.args = append(b.args, pattern)
		b.where = append(b.where, fmt.Sprintf(
			"(lower(c.email) LIKE $%d ESCAPE '\\' OR lower(c.name) LIKE $%d ESCAPE '\\')",
			len(b.args), len(b.args)))
	}

	if cursor != "" {
		sortKey, id, err := decodeCursorValue(cursor)
		if err != nil {
			return nil, "", err
		}
		op := "<"
		if direction == "ASC" {
			op = ">"
		}
		b.args = append(b.args, sortKey, id)
		b.where = append(b.where, fmt.Sprintf("(%s, c.id) %s ($%d, $%d)",
			sortColumn, op, len(b.args)-1, len(b.args)))
	}

	b.args = append(b.args, limit+1)
	stmt := `
		SELECT c.id, c.email, c.name, c.created_at,
		       COALESCE(inv.open_count, 0),
		       COALESCE(inv.outstanding, '[]'::jsonb),
		       COALESCE(svc.service_count, 0),
		       EXISTS (SELECT 1 FROM customer_accounts ca WHERE ca.customer_id = c.id)
		FROM customers c
		LEFT JOIN LATERAL (
			-- One row per currency, then folded into a JSON array. Summing across currencies
			-- is deliberately impossible here: the aggregate keeps each currency separate so
			-- no caller can accidentally add USD to EUR.
			SELECT COALESCE(SUM(open_count), 0) AS open_count,
			       COALESCE(jsonb_agg(jsonb_build_object('currency', currency, 'minor', outstanding)
			                          ORDER BY currency)
			                FILTER (WHERE outstanding > 0), '[]'::jsonb) AS outstanding
			FROM (
				SELECT currency, count(*) AS open_count,
				       SUM(total_minor - amount_paid_minor) AS outstanding
				FROM invoices
				WHERE customer_id = c.id AND status = 'open'
				GROUP BY currency
			) per_currency
		) inv ON true
		LEFT JOIN LATERAL (
			SELECT count(*) AS service_count FROM services WHERE customer_id = c.id
		) svc ON true
		WHERE ` + b.whereClause() + `
		ORDER BY ` + sortColumn + ` ` + direction + `, c.id ` + direction + `
		LIMIT $` + fmt.Sprint(len(b.args))

	rows, err := s.pool.Query(ctx, stmt, b.args...)
	if err != nil {
		return nil, "", fmt.Errorf("staff: list customers: %w", err)
	}
	defer rows.Close()

	out := make([]CustomerRow, 0, limit)
	var sortKeys []string
	for rows.Next() {
		var (
			row         CustomerRow
			outstanding []byte
			createdAt   time.Time
		)
		if err := rows.Scan(&row.ID, &row.Email, &row.Name, &createdAt, &row.OpenInvoices,
			&outstanding, &row.Services, &row.HasLogin); err != nil {
			return nil, "", fmt.Errorf("staff: scan customer: %w", err)
		}
		row.CreatedAt = createdAt
		parsed, err := decodeAmounts(outstanding)
		if err != nil {
			return nil, "", err
		}
		row.Outstanding = parsed
		out = append(out, row)
		// The cursor's sort key depends on which column the list was ordered by, so it is
		// derived from the row rather than from the query.
		switch sortColumn {
		case "lower(c.email)":
			sortKeys = append(sortKeys, strings.ToLower(row.Email))
		case "lower(c.name)":
			sortKeys = append(sortKeys, strings.ToLower(row.Name))
		default:
			sortKeys = append(sortKeys, createdAt.UTC().Format(time.RFC3339Nano))
		}
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("staff: read customers: %w", err)
	}

	next := ""
	if len(out) > limit {
		out = out[:limit]
		last := out[len(out)-1]
		next = encodeCursorValue(sortKeys[len(out)-1], last.ID)
	}
	return out, next, nil
}

// Customer returns the joined detail view.
func (s *Service) Customer(ctx context.Context, id string) (CustomerDetail, error) {
	var out CustomerDetail

	var (
		createdAt   time.Time
		outstanding []byte
	)
	err := s.pool.QueryRow(ctx, `
		SELECT c.id, c.email, c.name, c.created_at,
		       COALESCE(inv.open_count, 0),
		       COALESCE(inv.outstanding, '[]'::jsonb),
		       COALESCE(svc.service_count, 0),
		       EXISTS (SELECT 1 FROM customer_accounts ca WHERE ca.customer_id = c.id)
		FROM customers c
		LEFT JOIN LATERAL (
			-- One row per currency, then folded into a JSON array. Summing across currencies
			-- is deliberately impossible here: the aggregate keeps each currency separate so
			-- no caller can accidentally add USD to EUR.
			SELECT COALESCE(SUM(open_count), 0) AS open_count,
			       COALESCE(jsonb_agg(jsonb_build_object('currency', currency, 'minor', outstanding)
			                          ORDER BY currency)
			                FILTER (WHERE outstanding > 0), '[]'::jsonb) AS outstanding
			FROM (
				SELECT currency, count(*) AS open_count,
				       SUM(total_minor - amount_paid_minor) AS outstanding
				FROM invoices
				WHERE customer_id = c.id AND status = 'open'
				GROUP BY currency
			) per_currency
		) inv ON true
		LEFT JOIN LATERAL (
			SELECT count(*) AS service_count FROM services WHERE customer_id = c.id
		) svc ON true
		WHERE c.id = $1`, id).
		Scan(&out.Customer.ID, &out.Customer.Email, &out.Customer.Name, &createdAt,
			&out.Customer.OpenInvoices, &outstanding, &out.Customer.Services, &out.Customer.HasLogin)
	if errors.Is(err, pgx.ErrNoRows) {
		return CustomerDetail{}, ErrNotFound
	}
	if err != nil {
		return CustomerDetail{}, fmt.Errorf("staff: load customer: %w", err)
	}
	out.Customer.CreatedAt = createdAt
	if out.Customer.Outstanding, err = decodeAmounts(outstanding); err != nil {
		return CustomerDetail{}, err
	}

	if out.Accounts, err = s.accounts(ctx, id); err != nil {
		return CustomerDetail{}, err
	}
	if out.Subscriptions, err = s.subscriptions(ctx, id); err != nil {
		return CustomerDetail{}, err
	}
	if out.Services, err = s.servicesFor(ctx, id); err != nil {
		return CustomerDetail{}, err
	}
	if out.Invoices, err = s.invoicesFor(ctx, id, 20); err != nil {
		return CustomerDetail{}, err
	}
	if out.Balance, err = s.balance(ctx, id); err != nil {
		return CustomerDetail{}, err
	}
	if out.Holds, err = s.holdsForCustomer(ctx, id); err != nil {
		return CustomerDetail{}, err
	}
	if out.AuthEvents, err = s.authEvents(ctx, id, 10); err != nil {
		return CustomerDetail{}, err
	}
	return out, nil
}

func (s *Service) accounts(ctx context.Context, customerID string) ([]AccountRow, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT u.id, u.email, ca.role,
		       (SELECT max(last_seen_at) FROM sessions s
		         WHERE s.user_id = u.id AND s.revoked_at IS NULL)
		FROM customer_accounts ca
		JOIN users u ON u.id = ca.user_id
		WHERE ca.customer_id = $1
		ORDER BY ca.created_at`, customerID)
	if err != nil {
		return nil, fmt.Errorf("staff: list accounts: %w", err)
	}
	defer rows.Close()

	out := []AccountRow{}
	for rows.Next() {
		var (
			row    AccountRow
			seenAt *time.Time
		)
		if err := rows.Scan(&row.UserID, &row.Email, &row.Role, &seenAt); err != nil {
			return nil, fmt.Errorf("staff: scan account: %w", err)
		}
		row.LastSeenAt = seenAt
		out = append(out, row)
	}
	return out, rows.Err()
}

func (s *Service) subscriptions(ctx context.Context, customerID string) ([]SubscriptionRow, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT sub.id, sub.status, sub.interval, sub.next_billing_at,
		       sub.current_period_start, sub.current_period_end,
		       sub.cancel_at_period_end, p.name
		FROM subscriptions sub
		JOIN price_versions pv ON pv.id = sub.price_version_id
		JOIN products p ON p.id = pv.product_id
		WHERE sub.customer_id = $1
		ORDER BY sub.created_at DESC`, customerID)
	if err != nil {
		return nil, fmt.Errorf("staff: list subscriptions: %w", err)
	}
	defer rows.Close()

	out := []SubscriptionRow{}
	for rows.Next() {
		var row SubscriptionRow
		if err := rows.Scan(&row.ID, &row.Status, &row.Interval, &row.NextBillingAt,
			&row.CurrentPeriodStart, &row.CurrentPeriodEnd, &row.CancelAtPeriodEnd,
			&row.Product); err != nil {
			return nil, fmt.Errorf("staff: scan subscription: %w", err)
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// ServiceFilter narrows the service list.
type ServiceFilter struct {
	Status string
	// CustomerID scopes the list to one customer.
	CustomerID string
	ProductID  string
	// OnlyOpenOperations keeps services with a queued, running, retrying, unknown or
	// unreviewed operation, which is the "stuck" view.
	OnlyOpenOperations bool
}

func (s *Service) servicesFor(ctx context.Context, customerID string) ([]ServiceRow, error) {
	rows, _, err := s.Services(ctx, ServiceFilter{CustomerID: customerID}, 50, "")
	return rows, err
}

// Services lists services with the operation state an operator needs.
//
// last_error is the newest non-null failure, so a stuck service is explainable without a
// second request: "which service is broken and why" is the question the screen answers.
func (s *Service) Services(ctx context.Context, f ServiceFilter, limit int, cursor string) ([]ServiceRow, string, error) {
	limit = pageLimit(limit)

	var b queryBuilder
	if f.Status != "" {
		b.add("s.status = $%d", f.Status)
	}
	if f.CustomerID != "" {
		b.add("s.customer_id = $%d", f.CustomerID)
	}
	if f.ProductID != "" {
		b.add("s.product_id = $%d", f.ProductID)
	}
	if f.OnlyOpenOperations {
		b.addRaw(`EXISTS (SELECT 1 FROM service_operations so2
		                  WHERE so2.service_id = s.id
		                    AND so2.status IN ('queued','running','retry_wait','unknown','needs_review'))`)
	}
	if cursor != "" {
		when, id, err := decodeTimeCursor(cursor)
		if err != nil {
			return nil, "", err
		}
		b.args = append(b.args, when, id)
		b.where = append(b.where, fmt.Sprintf("(s.created_at, s.id) < ($%d, $%d)",
			len(b.args)-1, len(b.args)))
	}
	b.args = append(b.args, limit+1)

	stmt := `
		SELECT s.id, s.customer_id, c.email, p.name, p.id, s.status,
		       COALESCE(s.external_id, ''), s.generation, s.created_at,
		       sub.next_billing_at,
		       COALESCE(ops.open_count, 0),
		       COALESCE(ops.last_error, '')
		FROM services s
		JOIN customers c ON c.id = s.customer_id
		JOIN products p ON p.id = s.product_id
		LEFT JOIN subscriptions sub ON sub.id = s.subscription_id
		LEFT JOIN LATERAL (
			SELECT count(*) FILTER (WHERE status IN
			           ('queued','running','retry_wait','unknown','needs_review')) AS open_count,
			       (SELECT last_error FROM service_operations
			         WHERE service_id = s.id AND last_error IS NOT NULL
			         ORDER BY created_at DESC LIMIT 1) AS last_error
			FROM service_operations
			WHERE service_id = s.id
		) ops ON true
		WHERE ` + b.whereClause() + `
		ORDER BY s.created_at DESC, s.id DESC
		LIMIT $` + fmt.Sprint(len(b.args))

	rows, err := s.pool.Query(ctx, stmt, b.args...)
	if err != nil {
		return nil, "", fmt.Errorf("staff: list services: %w", err)
	}
	defer rows.Close()

	out := make([]ServiceRow, 0, limit)
	for rows.Next() {
		var (
			row       ServiceRow
			createdAt time.Time
			nextBill  *time.Time
		)
		if err := rows.Scan(&row.ID, &row.CustomerID, &row.CustomerMail, &row.Product,
			&row.ProductID, &row.Status, &row.ExternalID, &row.Generation, &createdAt,
			&nextBill, &row.OpenOps, &row.LastError); err != nil {
			return nil, "", fmt.Errorf("staff: scan service: %w", err)
		}
		row.CreatedAt = createdAt
		row.NextBillAt = nextBill
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("staff: read services: %w", err)
	}

	next := ""
	if len(out) > limit {
		out = out[:limit]
		last := out[len(out)-1]
		next = CursorOf(last.CreatedAt, last.ID)
	}
	return out, next, nil
}

// OperationRow is one provisioning operation.
type OperationRow struct {
	ID           string
	ServiceID    string
	CustomerID   string
	CustomerMail string
	Kind         string
	OperationKey string
	Generation   int64
	Status       string
	Attempts     int
	LastError    string
	CreatedAt    time.Time
	StartedAt    *time.Time
	FinishedAt   *time.Time
}

// OperationFilter narrows the operation queue.
type OperationFilter struct {
	Status    string
	Kind      string
	ServiceID string
}

// Operations lists operations, newest first.
func (s *Service) Operations(ctx context.Context, f OperationFilter, limit int, cursor string) ([]OperationRow, string, error) {
	limit = pageLimit(limit)

	var b queryBuilder
	if f.Status != "" {
		b.add("so.status = $%d", f.Status)
	}
	if f.Kind != "" {
		b.add("so.kind = $%d", f.Kind)
	}
	if f.ServiceID != "" {
		b.add("so.service_id = $%d", f.ServiceID)
	}
	if cursor != "" {
		when, id, err := decodeTimeCursor(cursor)
		if err != nil {
			return nil, "", err
		}
		b.args = append(b.args, when, id)
		b.where = append(b.where, fmt.Sprintf("(so.created_at, so.id) < ($%d, $%d)",
			len(b.args)-1, len(b.args)))
	}
	b.args = append(b.args, limit+1)

	stmt := `
		SELECT so.id, so.service_id, s.customer_id, c.email, so.kind, so.operation_key,
		       so.generation, so.status, so.attempts, COALESCE(so.last_error, ''),
		       so.created_at, so.started_at, so.finished_at
		FROM service_operations so
		JOIN services s ON s.id = so.service_id
		JOIN customers c ON c.id = s.customer_id
		WHERE ` + b.whereClause() + `
		ORDER BY so.created_at DESC, so.id DESC
		LIMIT $` + fmt.Sprint(len(b.args))

	rows, err := s.pool.Query(ctx, stmt, b.args...)
	if err != nil {
		return nil, "", fmt.Errorf("staff: list operations: %w", err)
	}
	defer rows.Close()

	out := make([]OperationRow, 0, limit)
	for rows.Next() {
		var row OperationRow
		if err := rows.Scan(&row.ID, &row.ServiceID, &row.CustomerID, &row.CustomerMail,
			&row.Kind, &row.OperationKey, &row.Generation, &row.Status, &row.Attempts,
			&row.LastError, &row.CreatedAt, &row.StartedAt, &row.FinishedAt); err != nil {
			return nil, "", fmt.Errorf("staff: scan operation: %w", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("staff: read operations: %w", err)
	}

	next := ""
	if len(out) > limit {
		out = out[:limit]
		last := out[len(out)-1]
		next = CursorOf(last.CreatedAt, last.ID)
	}
	return out, next, nil
}

// Operation returns one operation.
func (s *Service) Operation(ctx context.Context, id string) (OperationRow, error) {
	var row OperationRow
	err := s.pool.QueryRow(ctx, `
		SELECT so.id, so.service_id, s.customer_id, c.email, so.kind, so.operation_key,
		       so.generation, so.status, so.attempts, COALESCE(so.last_error, ''),
		       so.created_at, so.started_at, so.finished_at
		FROM service_operations so
		JOIN services s ON s.id = so.service_id
		JOIN customers c ON c.id = s.customer_id
		WHERE so.id = $1`, id).
		Scan(&row.ID, &row.ServiceID, &row.CustomerID, &row.CustomerMail, &row.Kind,
			&row.OperationKey, &row.Generation, &row.Status, &row.Attempts, &row.LastError,
			&row.CreatedAt, &row.StartedAt, &row.FinishedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return OperationRow{}, ErrNotFound
	}
	if err != nil {
		return OperationRow{}, fmt.Errorf("staff: load operation: %w", err)
	}
	return row, nil
}

// WebhookRow is one inbox event.
type WebhookRow struct {
	ID              string
	Provider        string
	ProviderAccount string
	EventID         string
	EventType       string
	ReceivedAt      time.Time
	ProcessedAt     *time.Time
	Attempts        int
	LastError       string
}

// WebhookFilter narrows the inbox view.
type WebhookFilter struct {
	// Processed selects processed (true), unprocessed (false) or either (nil).
	Processed *bool
	Provider  string
}

// Webhooks lists inbox events, newest first.
func (s *Service) Webhooks(ctx context.Context, f WebhookFilter, limit int, cursor string) ([]WebhookRow, string, error) {
	limit = pageLimit(limit)

	var b queryBuilder
	if f.Processed != nil {
		if *f.Processed {
			b.addRaw("processed_at IS NOT NULL")
		} else {
			b.addRaw("processed_at IS NULL")
		}
	}
	if f.Provider != "" {
		b.add("provider = $%d", f.Provider)
	}
	if cursor != "" {
		when, id, err := decodeTimeCursor(cursor)
		if err != nil {
			return nil, "", err
		}
		b.args = append(b.args, when, id)
		b.where = append(b.where, fmt.Sprintf("(received_at, id) < ($%d, $%d)",
			len(b.args)-1, len(b.args)))
	}
	b.args = append(b.args, limit+1)

	stmt := `
		SELECT id, provider, provider_account, event_id, event_type, received_at,
		       processed_at, attempts, COALESCE(last_error, '')
		FROM gateway_events
		WHERE ` + b.whereClause() + `
		ORDER BY received_at DESC, id DESC
		LIMIT $` + fmt.Sprint(len(b.args))

	rows, err := s.pool.Query(ctx, stmt, b.args...)
	if err != nil {
		return nil, "", fmt.Errorf("staff: list webhooks: %w", err)
	}
	defer rows.Close()

	out := make([]WebhookRow, 0, limit)
	for rows.Next() {
		var row WebhookRow
		if err := rows.Scan(&row.ID, &row.Provider, &row.ProviderAccount, &row.EventID,
			&row.EventType, &row.ReceivedAt, &row.ProcessedAt, &row.Attempts,
			&row.LastError); err != nil {
			return nil, "", fmt.Errorf("staff: scan webhook: %w", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("staff: read webhooks: %w", err)
	}

	next := ""
	if len(out) > limit {
		out = out[:limit]
		last := out[len(out)-1]
		next = CursorOf(last.ReceivedAt, last.ID)
	}
	return out, next, nil
}

// decodeAmounts parses the per-currency aggregate the customer queries build.
func decodeAmounts(raw []byte) ([]Amount, error) {
	if len(raw) == 0 {
		return []Amount{}, nil
	}
	type pair struct {
		Currency string `json:"currency"`
		Minor    int64  `json:"minor"`
	}
	var pairs []pair
	if err := jsonUnmarshal(raw, &pairs); err != nil {
		return nil, fmt.Errorf("staff: decode amounts: %w", err)
	}
	out := make([]Amount, 0, len(pairs))
	for _, p := range pairs {
		amt, err := Amounts(p.Minor, p.Currency)
		if err != nil {
			return nil, err
		}
		out = append(out, amt)
	}
	return out, nil
}

// escapeLike escapes the LIKE wildcards in a user-supplied search term, so searching for
// "100%" finds the literal string rather than everything.
func escapeLike(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '%', '_', '\\':
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// currencyFor resolves the currency of an amount list entry.
func currencyFor(a Amount) money.Currency { return a.Currency }
