// The operator log viewers (ADM-17, ADM-19, ADM-20, OPS-15).
//
// Three tables, three questions:
//
//   - `notification_deliveries` is the email log: what was sent, to whom, and whether it
//     worked. It already exists as the durable outbox, so this is a viewer over it rather
//     than a second log that could disagree with the outbox.
//   - `error_logs` is what failed in the process.
//   - `http_logs` is what the process served, and how long it took.
//
// The email viewer deliberately returns no message body. A password-reset mail's body
// carries a working token, so a log viewer that rendered it would be a way to take over any
// account whose reset mail an operator can find — and "an operator" includes a support
// agent holding log:read.
//
// They are tables rather than only stdout because an operator without a log aggregator
// still has to answer "what failed last night" from the panel. That is also why the writers
// below are exported functions rather than something the request middleware calls inline:
// the middleware and the worker live in packages that must be able to record a row without
// depending on the HTTP handler set, and a failed write must never fail the request it is
// describing.
//
// Redaction happens on the way in first, because a stored credential is stored in a backup,
// a replica and a `pg_dump` as well. `redactLogText` is the one rule, applied by both
// writers; the readers apply it again as defence in depth, so a row that reached the table
// another way (an older build, a hand-run INSERT) still cannot leak through the viewer.
//
// Every list is keyset-paginated with internal/paginate: the tables grow on every request,
// so an OFFSET page would repeat or skip rows while an operator scrolls — and the row it
// skips is the one they were looking for.

package httpapi

import (
	"context"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/catalyst/billing/internal/authn"
	"github.com/catalyst/billing/internal/paginate"
	"github.com/catalyst/billing/internal/store"
)

func init() {
	RegisterRoutes("42-operator-logs", registerOperatorLogs)
}

func registerOperatorLogs(r *router) {
	// log:read covers all three: they are one screen in the panel and one class of
	// information (what the system did). Support holds it because "why did my email not
	// arrive" is a support question; the infrastructure role does not, because reading
	// customer addresses is not part of running a server.
	r.staffGet("/api/v1/staff/logs/email", authn.PermLogRead, r.s.handleEmailLog)
	r.staffGet("/api/v1/staff/logs/errors", authn.PermLogRead, r.s.handleErrorLog)
	r.staffGet("/api/v1/staff/logs/http", authn.PermLogRead, r.s.handleHTTPLog)
}

// logPageParams parses the pagination query for a log list.
func (s *Server) logPageParams(w http.ResponseWriter, r *http.Request) (limit int, cursor string, ok bool) {
	limit, cursor, err := ParsePage(r)
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_cursor",
			"the limit or cursor is not valid")
		return 0, "", false
	}
	return limit, cursor, true
}

// decodeLogCursor parses a (timestamp, id) cursor.
//
// The sort key is RFC3339Nano text because that is what paginate.Encode carries, and
// parsing it here means a forged cursor is a 400 rather than a database error.
func decodeLogCursor(cursor string) (time.Time, string, error) {
	sortKey, id, err := paginate.Decode(cursor)
	if err != nil {
		return time.Time{}, "", err
	}
	if sortKey == "" {
		return time.Time{}, id, nil
	}
	when, err := time.Parse(time.RFC3339Nano, sortKey)
	if err != nil {
		return time.Time{}, "", paginate.ErrInvalidCursor
	}
	return when, id, nil
}

// logTimeCursor renders the cursor for the last row of a page.
func logTimeCursor(when time.Time, id int64) string {
	return paginate.Encode(when.UTC().Format(time.RFC3339Nano), strconv.FormatInt(id, 10))
}

// ------------------------------------------------------------------- email --

// emailLogRow is one delivery in the email log.
type emailLogRow struct {
	ID        string     `json:"id"`
	Kind      string     `json:"kind"`
	To        string     `json:"to"`
	Subject   string     `json:"subject"`
	Status    string     `json:"status"`
	Attempts  int        `json:"attempts"`
	LastError string     `json:"last_error"`
	CreatedAt time.Time  `json:"created_at"`
	SentAt    *time.Time `json:"sent_at"`
}

// handleEmailLog lists notification deliveries, newest first.
//
// Query: status (queued|sending|sent|failed|suppressed), kind, to (an address fragment),
// from, to (time), limit, cursor.
func (s *Server) handleEmailLog(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "database_not_configured",
			"no database is configured")
		return
	}
	limit, cursor, ok := s.logPageParams(w, r)
	if !ok {
		return
	}

	q := r.URL.Query()
	args := []any{}
	where := []string{"true"}
	add := func(clause string, value any) {
		args = append(args, value)
		where = append(where, strings.ReplaceAll(clause, "?", "$"+strconv.Itoa(len(args))))
	}
	if status := strings.TrimSpace(q.Get("status")); status != "" {
		add("status = ?", status)
	}
	if kind := strings.TrimSpace(q.Get("kind")); kind != "" {
		add("kind = ?", kind)
	}
	if to := strings.TrimSpace(q.Get("to")); to != "" {
		// A case-insensitive fragment: an operator pastes part of an address.
		add("to_address ILIKE ?", "%"+to+"%")
	}
	from, ok := s.logTimeParam(w, r, "from")
	if !ok {
		return
	}
	until, ok := s.logTimeParam(w, r, "to_time")
	if !ok {
		return
	}
	if from != nil {
		add("created_at >= ?", *from)
	}
	if until != nil {
		add("created_at <= ?", *until)
	}

	if cursor != "" {
		when, id, err := decodeLogCursor(cursor)
		if err != nil {
			writeProblem(w, http.StatusBadRequest, "invalid_cursor", "the cursor is invalid")
			return
		}
		args = append(args, when, id)
		where = append(where, "(created_at, id) < ($"+strconv.Itoa(len(args)-1)+", $"+strconv.Itoa(len(args))+")")
	}
	args = append(args, limit+1)

	// `id` is a uuid on this table, so the cursor's id component is the uuid text and the
	// tie-break is a plain comparison. The sort key is created_at, which is indexed by the
	// retention policy's age index.
	stmt := `
		SELECT id, kind, to_address, subject, status, attempts, COALESCE(last_error, ''),
		       created_at, sent_at
		FROM notification_deliveries
		WHERE ` + strings.Join(where, " AND ") + `
		ORDER BY created_at DESC, id DESC
		LIMIT $` + strconv.Itoa(len(args))

	rows, err := s.pool.Query(r.Context(), stmt, args...)
	if err != nil {
		s.cfg.Logger.ErrorContext(r.Context(), "email log failed", "error", err)
		writeProblem(w, http.StatusInternalServerError, "internal_error",
			"the request could not be completed")
		return
	}
	defer rows.Close()

	out := make([]emailLogRow, 0, limit)
	for rows.Next() {
		var row emailLogRow
		if err := rows.Scan(&row.ID, &row.Kind, &row.To, &row.Subject, &row.Status,
			&row.Attempts, &row.LastError, &row.CreatedAt, &row.SentAt); err != nil {
			s.cfg.Logger.ErrorContext(r.Context(), "scan email log failed", "error", err)
			writeProblem(w, http.StatusInternalServerError, "internal_error",
				"the request could not be completed")
			return
		}
		// A delivery error is free text from an SMTP server. It is redacted on read as
		// well as on write, because a row written by an older build (or by hand) must not
		// become a credential leak through this viewer. The body is deliberately not
		// returned at all: a password-reset mail's body carries a working token.
		row.LastError = redactLogText(row.LastError)
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		writeProblem(w, http.StatusInternalServerError, "internal_error",
			"the request could not be completed")
		return
	}

	var next *string
	if len(out) > limit {
		out = out[:limit]
		last := out[len(out)-1]
		c := paginate.Encode(last.CreatedAt.UTC().Format(time.RFC3339Nano), last.ID)
		next = &c
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, Page[emailLogRow]{Data: out, NextCursor: next})
}

// logTimeParam parses an RFC3339 query parameter, writing a problem on failure.
func (s *Server) logTimeParam(w http.ResponseWriter, r *http.Request, name string) (*time.Time, bool) {
	raw := strings.TrimSpace(r.URL.Query().Get(name))
	if raw == "" {
		return nil, true
	}
	when, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_time", name+" must be an RFC3339 timestamp")
		return nil, false
	}
	return &when, true
}

// ------------------------------------------------------------------ errors --

// errorLogRow is one error row.
type errorLogRow struct {
	ID        int64     `json:"id"`
	Level     string    `json:"level"`
	Source    string    `json:"source"`
	Message   string    `json:"message"`
	Detail    string    `json:"detail"`
	CreatedAt time.Time `json:"created_at"`
}

// handleErrorLog lists error_logs, newest first.
//
// Query: level, source, q (a fragment of the message), limit, cursor.
func (s *Server) handleErrorLog(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "database_not_configured",
			"no database is configured")
		return
	}
	limit, cursor, ok := s.logPageParams(w, r)
	if !ok {
		return
	}

	q := r.URL.Query()
	args := []any{}
	where := []string{"true"}
	add := func(clause string, value any) {
		args = append(args, value)
		where = append(where, strings.ReplaceAll(clause, "?", "$"+strconv.Itoa(len(args))))
	}
	if level := strings.TrimSpace(q.Get("level")); level != "" {
		add("level = ?", level)
	}
	if source := strings.TrimSpace(q.Get("source")); source != "" {
		add("source = ?", source)
	}
	if needle := strings.TrimSpace(q.Get("q")); needle != "" {
		add("message ILIKE ?", "%"+needle+"%")
	}
	if cursor != "" {
		when, id, err := decodeLogCursor(cursor)
		if err != nil {
			writeProblem(w, http.StatusBadRequest, "invalid_cursor", "the cursor is invalid")
			return
		}
		seq, perr := strconv.ParseInt(id, 10, 64)
		if perr != nil {
			writeProblem(w, http.StatusBadRequest, "invalid_cursor", "the cursor is invalid")
			return
		}
		args = append(args, when, seq)
		where = append(where, "(created_at, id) < ($"+strconv.Itoa(len(args)-1)+", $"+strconv.Itoa(len(args))+")")
	}
	args = append(args, limit+1)

	stmt := `
		SELECT id, level, source, message, detail, created_at
		FROM error_logs
		WHERE ` + strings.Join(where, " AND ") + `
		ORDER BY created_at DESC, id DESC
		LIMIT $` + strconv.Itoa(len(args))

	rows, err := s.pool.Query(r.Context(), stmt, args...)
	if err != nil {
		s.cfg.Logger.ErrorContext(r.Context(), "error log failed", "error", err)
		writeProblem(w, http.StatusInternalServerError, "internal_error",
			"the request could not be completed")
		return
	}
	defer rows.Close()

	out := make([]errorLogRow, 0, limit)
	for rows.Next() {
		var row errorLogRow
		if err := rows.Scan(&row.ID, &row.Level, &row.Source, &row.Message, &row.Detail,
			&row.CreatedAt); err != nil {
			writeProblem(w, http.StatusInternalServerError, "internal_error",
				"the request could not be completed")
			return
		}
		// Defence in depth: the writer already redacts, but a row that reached the table
		// another way must not leak through the viewer either.
		row.Message = redactLogText(row.Message)
		row.Detail = redactLogText(row.Detail)
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		writeProblem(w, http.StatusInternalServerError, "internal_error",
			"the request could not be completed")
		return
	}

	var next *string
	if len(out) > limit {
		out = out[:limit]
		last := out[len(out)-1]
		c := logTimeCursor(last.CreatedAt, last.ID)
		next = &c
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, Page[errorLogRow]{Data: out, NextCursor: next})
}

// -------------------------------------------------------------------- http --

// httpLogRow is one served request.
type httpLogRow struct {
	ID         int64     `json:"id"`
	Method     string    `json:"method"`
	Path       string    `json:"path"`
	Status     int       `json:"status"`
	DurationMS int       `json:"duration_ms"`
	ActorID    string    `json:"actor_id"`
	IP         string    `json:"ip"`
	CreatedAt  time.Time `json:"created_at"`
}

// handleHTTPLog lists http_logs, newest first.
//
// Query: method, status (an exact code), min_status, path (a prefix), limit, cursor.
func (s *Server) handleHTTPLog(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "database_not_configured",
			"no database is configured")
		return
	}
	limit, cursor, ok := s.logPageParams(w, r)
	if !ok {
		return
	}

	q := r.URL.Query()
	args := []any{}
	where := []string{"true"}
	add := func(clause string, value any) {
		args = append(args, value)
		where = append(where, strings.ReplaceAll(clause, "?", "$"+strconv.Itoa(len(args))))
	}
	if method := strings.TrimSpace(q.Get("method")); method != "" {
		add("method = ?", strings.ToUpper(method))
	}
	if status := strings.TrimSpace(q.Get("status")); status != "" {
		code, err := strconv.Atoi(status)
		if err != nil || code < 100 || code > 599 {
			writeProblem(w, http.StatusBadRequest, "invalid_status", "status must be an HTTP status code")
			return
		}
		add("status = ?", code)
	}
	if minStatus := strings.TrimSpace(q.Get("min_status")); minStatus != "" {
		code, err := strconv.Atoi(minStatus)
		if err != nil || code < 100 || code > 599 {
			writeProblem(w, http.StatusBadRequest, "invalid_status",
				"min_status must be an HTTP status code")
			return
		}
		add("status >= ?", code)
	}
	if path := strings.TrimSpace(q.Get("path")); path != "" {
		add("path LIKE ?", path+"%")
	}
	if cursor != "" {
		when, id, err := decodeLogCursor(cursor)
		if err != nil {
			writeProblem(w, http.StatusBadRequest, "invalid_cursor", "the cursor is invalid")
			return
		}
		seq, perr := strconv.ParseInt(id, 10, 64)
		if perr != nil {
			writeProblem(w, http.StatusBadRequest, "invalid_cursor", "the cursor is invalid")
			return
		}
		args = append(args, when, seq)
		where = append(where, "(created_at, id) < ($"+strconv.Itoa(len(args)-1)+", $"+strconv.Itoa(len(args))+")")
	}
	args = append(args, limit+1)

	stmt := `
		SELECT id, method, path, status, duration_ms, COALESCE(actor_id::text, ''),
		       COALESCE(host(ip), ''), created_at
		FROM http_logs
		WHERE ` + strings.Join(where, " AND ") + `
		ORDER BY created_at DESC, id DESC
		LIMIT $` + strconv.Itoa(len(args))

	rows, err := s.pool.Query(r.Context(), stmt, args...)
	if err != nil {
		s.cfg.Logger.ErrorContext(r.Context(), "http log failed", "error", err)
		writeProblem(w, http.StatusInternalServerError, "internal_error",
			"the request could not be completed")
		return
	}
	defer rows.Close()

	out := make([]httpLogRow, 0, limit)
	for rows.Next() {
		var row httpLogRow
		if err := rows.Scan(&row.ID, &row.Method, &row.Path, &row.Status, &row.DurationMS,
			&row.ActorID, &row.IP, &row.CreatedAt); err != nil {
			writeProblem(w, http.StatusInternalServerError, "internal_error",
				"the request could not be completed")
			return
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		writeProblem(w, http.StatusInternalServerError, "internal_error",
			"the request could not be completed")
		return
	}

	var next *string
	if len(out) > limit {
		out = out[:limit]
		last := out[len(out)-1]
		c := logTimeCursor(last.CreatedAt, last.ID)
		next = &c
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, Page[httpLogRow]{Data: out, NextCursor: next})
}

// ------------------------------------------------------------------ writers --

// ErrorEntry is one row for WriteError.
type ErrorEntry struct {
	// Level is "error" unless the caller knows better ("warn", "critical"). It is a free
	// string in the schema, not an enum, because a new severity must not need a migration.
	Level string
	// Source is the component that failed, e.g. "worker.renewals" or "http.webhook".
	Source string
	// Message is the short, stable description. It must not contain customer data.
	Message string
	// Detail is the diagnostic text (an error string, a stack). It is redacted before it
	// is stored.
	Detail string
}

// WriteError records one error-log row.
//
// The signature is deliberately narrow: the middleware and the worker both have a
// context, a component name and an error, and nothing else they should be able to store.
// The write is best-effort by contract — it returns an error so a caller can log it, but a
// caller must not fail the request or the job it is describing because the log write
// failed. Losing a log line is recoverable; failing a paid request because the log table
// was full is not.
//
//	func WriteError(ctx context.Context, pool *store.Pool, e ErrorEntry) error
//
// A nil pool is a no-op, so an API-only deployment with no database still runs.
func WriteError(ctx context.Context, pool *store.Pool, e ErrorEntry) error {
	if pool == nil {
		return nil
	}
	level := strings.TrimSpace(e.Level)
	if level == "" {
		level = "error"
	}
	source := truncate(strings.TrimSpace(e.Source), 120)
	message := truncate(redactLogText(strings.TrimSpace(e.Message)), 2000)
	if message == "" {
		// A row with no message cannot be acted on, and the schema requires one. The
		// source alone is more useful than nothing.
		message = "unspecified error"
	}
	detail := truncate(redactLogText(e.Detail), 16<<10)

	_, err := pool.Exec(ctx, `
		INSERT INTO error_logs (level, source, message, detail)
		VALUES ($1, $2, $3, $4)`, level, source, message, detail)
	return err
}

// HTTPEntry is one row for WriteHTTP.
//
// The struct deliberately has no headers field. The table has none either: a header map is
// where an Authorization token or a session cookie would be copied into a backup, and the
// value of storing it is far below the cost of leaking it. Method, path, status, duration
// and the actor answer every question the viewer is for.
type HTTPEntry struct {
	Method     string
	Path       string
	Status     int
	DurationMS int
	// ActorID is the authenticated principal, when there is one. It is a uuid, or empty.
	ActorID string
	// IP is the caller's address. It is stored as inet, so a malformed value is dropped
	// rather than failing the insert.
	IP string
}

// WriteHTTP records one served-request row.
//
//	func WriteHTTP(ctx context.Context, pool *store.Pool, e HTTPEntry) error
//
// The query string is stripped from the path before it is stored: a reset token or an
// email address in a query string is exactly the kind of thing a log viewer would display
// to the next operator, and the path is enough to find the route.
func WriteHTTP(ctx context.Context, pool *store.Pool, e HTTPEntry) error {
	if pool == nil {
		return nil
	}
	method := strings.ToUpper(strings.TrimSpace(e.Method))
	if method == "" {
		method = "GET"
	}
	path := stripQuery(e.Path)
	if path == "" {
		path = "/"
	}
	// An actor id that is not a uuid would fail the insert; a non-uuid principal is not
	// one this schema can name, so it is dropped rather than guessed at.
	var actor any
	if validUUID(e.ActorID) {
		actor = e.ActorID
	}
	var ip any
	if addr := strings.TrimSpace(e.IP); addr != "" {
		// The column is `inet`, so a value the server cannot parse would fail the insert.
		// A malformed address is dropped rather than allowed to lose the whole row: the
		// method, path, status and duration are the useful part.
		if net.ParseIP(addr) != nil {
			ip = addr
		}
	}
	duration := e.DurationMS
	if duration < 0 {
		duration = 0
	}

	_, err := pool.Exec(ctx, `
		INSERT INTO http_logs (method, path, status, duration_ms, actor_id, ip)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		truncate(method, 12), truncate(redactLogText(path), 2048), e.Status, duration, actor, ip)
	return err
}

// logSecretMarkers are the substrings that make a piece of text a credential.
//
// They are matched against a `key=value` or `key: value` pair inside the text, because a
// blanket substring match on the value would redact an innocent word. The list mirrors
// audit.secretMarkers, which is the rule the audit log already applies to field names.
var logSecretMarkers = []string{
	"authorization", "password", "passwd", "secret", "token", "api_key", "apikey",
	"private_key", "cookie", "credential", "signature", "card_number", "cvv", "cvc",
}

// redactLogText removes credential-looking key/value pairs from free text.
//
// A log detail is a string, not a map, so the redaction is textual: "Authorization: Bearer
// abc" becomes "Authorization: [redacted]". It is deliberately conservative — it looks for
// a marker followed by a separator — because over-redacting a log line is a support
// inconvenience while under-redacting it is a credential in a backup.
//
// The scan is written as an explicit walk rather than a regular expression so the
// delimiters and the replacement are visible: a regex that was subtly wrong would fail
// open, which is the one failure mode this function exists to prevent.
func redactLogText(s string) string {
	if s == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(len(s))
	rest := s
	for len(rest) > 0 {
		lower := strings.ToLower(rest)
		marker, pos := "", -1
		for _, m := range logSecretMarkers {
			if p := strings.Index(lower, m); p >= 0 && (pos < 0 || p < pos) {
				marker, pos = m, p
			}
		}
		if pos < 0 {
			b.WriteString(rest)
			break
		}
		// Everything before the marker is safe.
		b.WriteString(rest[:pos])
		end := pos + len(marker)

		// The marker must be followed by a separator within a short window; otherwise it
		// is a word in a sentence ("the token bucket") rather than a key/value pair.
		sep := -1
		for i := end; i < len(rest) && i < end+3; i++ {
			if rest[i] == ':' || rest[i] == '=' {
				sep = i
				break
			}
		}
		if sep < 0 {
			b.WriteString(rest[pos:end])
			rest = rest[end:]
			continue
		}

		// Write the marker and its separator, then the redaction in place of the value.
		b.WriteString(rest[pos : sep+1])
		b.WriteString(" [redacted]")
		// The value runs to the next delimiter.
		valueEnd := len(rest)
		for i := sep + 1; i < len(rest); i++ {
			switch rest[i] {
			case ',', ';', '&', '\n', '\r', '"', '\'':
				valueEnd = i
			}
			if valueEnd != len(rest) {
				break
			}
		}
		rest = rest[valueEnd:]
	}
	return b.String()
}

// stripQuery removes a query string and fragment from a path.
func stripQuery(path string) string {
	if path == "" {
		return ""
	}
	if u, err := url.Parse(path); err == nil && u.Path != "" {
		return u.Path
	}
	if i := strings.IndexAny(path, "?#"); i >= 0 {
		return path[:i]
	}
	return path
}

// truncate bounds a string without splitting a UTF-8 sequence in the middle.
func truncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	// Trim to the limit, then drop a partial rune at the end.
	cut := limit
	for cut > 0 && (s[cut]&0xC0) == 0x80 {
		cut--
	}
	return s[:cut]
}
