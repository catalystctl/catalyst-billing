package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/catalyst/billing/internal/audit"
	"github.com/catalyst/billing/internal/authn"
	"github.com/catalyst/billing/internal/settings"
	"github.com/catalyst/billing/internal/store"
	"github.com/jackc/pgx/v5"
)

// CLI parity with the panel's operator screens (OPS-17).
//
// `billing admin` could create a user, create a customer, issue an invoice and purge test data,
// but an operator with a shell and no browser could not change a setting or read a log — the two
// things actually needed at 02:00, when the panel is the thing that is broken.
//
// Both commands are thin wrappers over the code the HTTP path uses, and that is the point:
//
//   - `setting set` goes through internal/settings, so the declared key, its type, its choices and
//     its secret handling are the same ones the settings screen enforces. A CLI that re-implemented
//     validation would accept a value the panel rejects, and the panel would then fail to render
//     the group it cannot parse.
//   - `logs tail` reads `notification_deliveries`, `error_logs` and `http_logs` — the three tables
//     the /logs screen reads — so an operator does not have to learn a second, parallel story about
//     what happened.
//
// Two deliberate differences from the HTTP path, both because a CLI is not a session:
//
//   - **Who did it.** A settings write is audited naming the operator. An HTTP request has a
//     session to name; a shell does not, so the identity is resolved from `--as <email or id>`
//     (or BILLING_ADMIN_ACTOR). When it names a user, that user's permissions are checked: the CLI
//     must not become a way to do something the API would refuse with 403. When it names nobody,
//     the row is a `system` action with the OS user and host recorded in `changes.operator` — a
//     break-glass path that says plainly what it is rather than inventing a person.
//   - **Secrets on a command line.** An argument is visible in `ps` and lands in shell history, so
//     a value of `-` reads the setting from stdin. That is the same reason `create-user` prefers
//     BILLING_ADMIN_PASSWORD or a pipe over `--password`.

// ------------------------------------------------------------------ setting --

// adminSettingGet prints one setting's effective value.
//
// Usage: billing admin setting get <group> <key>
//
// It exists for scripts, and for one script in particular: `scripts/dev-config.sh` points the panel at
// the fixture provisioner, and it must be able to ask whether an operator has already pointed the
// installation somewhere real. Without a read verb the only way to do that is psql against the settings
// table — a second, unvalidated path to the same data, which is how the fixtures came to overwrite a live
// integration and make every service report "The server panel does not recognise this service".
//
// A secret is printed as the redaction marker, never its value: a CLI that can read a credential back is a
// CLI that can leak one into a shell history. `--exit-code` makes an unset key exit 1, which is what a
// script wants when it is deciding whether to write a default.
func adminSettingGet(args []string) error {
	flags, positional, err := splitFlags(args, nil, []string{"exit-code"})
	if err != nil {
		return err
	}
	if len(positional) != 2 {
		return errors.New("usage: billing admin setting get <group> <key> [--exit-code]")
	}
	group, key := positional[0], positional[1]

	def, ok := settings.Lookup(group, key)
	if !ok {
		return unknownSettingError(group, key)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool, err := store.Open(ctx, store.ConfigFromEnv())
	if err != nil {
		return err
	}
	defer pool.Close()

	// The group is read in one statement through the same merge the settings screen uses, so the value
	// printed here is the one the panel would render — default included, and a secret redacted on the same
	// path. A per-key query would be a second definition of "the effective value".
	values, err := settings.New(pool).Group(ctx, group)
	if err != nil {
		return fmt.Errorf("read %s: %w", group, err)
	}
	for _, v := range values {
		if v.Key != key {
			continue
		}
		shown := v.Value
		if def.Secret && v.IsSet {
			shown = settings.RedactedMarker
		}
		fmt.Println(shown)
		if flags["exit-code"] == "true" && !v.IsSet {
			// The default is printed (so a caller can see what applies), but the exit status separates
			// "you set this" from "this is the fallback" — the difference a seeding script needs.
			os.Exit(1)
		}
		return nil
	}
	return fmt.Errorf("%s.%s is declared but was not returned by the store", group, key)
}

// adminSettingSet changes one setting from the command line.
//
// Usage: billing admin setting set <group> <key> <value|-> [--as <email or id>]
func adminSettingSet(args []string) error {
	flags, positional, err := splitFlags(args, []string{"as"}, nil)
	if err != nil {
		return err
	}
	if len(positional) != 3 {
		return errors.New("usage: billing admin setting set <group> <key> <value|-> " +
			"[--as <operator email or id>]")
	}
	group, key, raw := positional[0], positional[1], positional[2]

	// `-` means stdin. It is the only way to set a credential without putting it in an argv.
	value := raw
	if raw == "-" {
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return fmt.Errorf("read the value from stdin: %w", err)
		}
		value = strings.TrimRight(line, "\r\n")
	}

	// The key is checked against the declared set before a connection is opened, so a typo costs
	// nothing and the refusal can list what does exist. The value is then checked with the same
	// Definition.Validate the settings store calls inside its transaction: the rule is not
	// re-implemented here, it is the same code, and the write below re-runs it as the authority.
	def, ok := settings.Lookup(group, key)
	if !ok {
		return unknownSettingError(group, key)
	}
	if err := def.Validate(value); err != nil {
		return fmt.Errorf("%w (nothing was written)", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool, err := store.Open(ctx, store.ConfigFromEnv())
	if err != nil {
		return err
	}
	defer pool.Close()

	operator, err := resolveCLIOperator(ctx, pool, flags["as"], authn.PermSettingsWrite)
	if err != nil {
		return err
	}

	stored, err := settings.New(pool).Set(ctx, group, key, value, operator.userID,
		cliAuditor{actor: operator.actor, operator: operator.label})
	if err != nil {
		return settingWriteError(group, err)
	}

	// The printed value is the read-back from the same transaction that wrote it, which is what
	// makes "it said it set it" mean the row is really there. A secret is redacted by the read
	// path, so the confirmation cannot leak what was just typed.
	shown := stored.Value
	if stored.IsSecret {
		shown = settings.RedactedMarker + " (secret)"
	}
	if !stored.IsSet {
		shown = "(default) " + shown
	}
	fmt.Printf("set %s.%s = %s\n", group, key, shown)
	if operator.actor.Kind == audit.ActorSystem {
		fmt.Fprintf(os.Stderr,
			"note: no --as was given, so the audit row is a system action by %s\n"+
				"      pass --as <email> to attribute the change to an operator\n", operator.label)
	} else {
		fmt.Printf("audited as %s\n", operator.label)
	}
	return nil
}

// unknownSettingError names the mistake and lists what is available.
//
// "unknown key" on its own sends an operator to the source; the list is three lines and answers the
// question they actually have.
func unknownSettingError(group, key string) error {
	if defs := settingsForGroup(group); len(defs) == 0 {
		return fmt.Errorf("no settings group %q; groups: %s",
			group, strings.Join(settings.Groups(), ", "))
	}
	names := make([]string, 0, 8)
	for _, def := range settingsForGroup(group) {
		names = append(names, def.Key)
	}
	return fmt.Errorf("%q is not a key in group %q; keys: %s",
		key, group, strings.Join(names, ", "))
}

// settingsForGroup returns the declared keys of one group.
func settingsForGroup(group string) []settings.Definition {
	var out []settings.Definition
	for _, def := range settings.Definitions() {
		if def.Group == group {
			out = append(out, def)
		}
	}
	return out
}

// settingWriteError turns a settings error into a sentence an operator can act on.
//
// The settings package's messages are already specific ("settings: invalid value: checkout.tos_required
// must be true or false"), so they are passed through; what is added is the hint that nothing was
// written, because a refused write that leaves the operator unsure whether it half-applied is worse
// than the refusal.
func settingWriteError(group string, err error) error {
	switch {
	case errors.Is(err, settings.ErrUnknownGroup):
		return fmt.Errorf("%w (nothing was written); groups: %s",
			err, strings.Join(settings.Groups(), ", "))
	case errors.Is(err, settings.ErrUnknownKey):
		return fmt.Errorf("%w (nothing was written); keys in %q: %s",
			err, group, strings.Join(keysOf(group), ", "))
	case errors.Is(err, settings.ErrInvalidValue):
		return fmt.Errorf("%w (nothing was written)", err)
	default:
		return fmt.Errorf("the setting was not written: %w", err)
	}
}

// keysOf lists the declared key names of a group.
func keysOf(group string) []string {
	defs := settingsForGroup(group)
	out := make([]string, 0, len(defs))
	for _, def := range defs {
		out = append(out, def.Key)
	}
	return out
}

// cliOperator is the identity a CLI write is attributed to.
type cliOperator struct {
	actor audit.Actor
	// userID is the operator's user id, or empty for a system action. It is passed to the settings
	// store so the row's `updated_by` names the same principal the audit row does.
	userID string
	// label is what the command prints and what the audit row records in `changes.operator`.
	label string
}

// resolveCLIOperator decides who the command is acting as.
//
// With `who` empty the action is a system action attributed to the OS user, which is the break-glass
// path: an operator with the database credentials and no browser. With `who` set it is that user's
// action, and their permission is checked first — a CLI that skipped the check would be a way to do
// what the API refuses with 403, which is exactly the shortcut `cmdAdmin`'s comment promises not to
// take.
func resolveCLIOperator(ctx context.Context, pool *store.Pool, who string,
	required authn.Permission) (cliOperator, error) {
	who = strings.TrimSpace(who)
	if who == "" {
		if v := strings.TrimSpace(os.Getenv("BILLING_ADMIN_ACTOR")); v != "" {
			who = v
		}
	}
	if who == "" {
		return cliOperator{
			actor: audit.Actor{Kind: audit.ActorSystem},
			label: osOperatorLabel(),
		}, nil
	}

	var (
		id    string
		email string
	)
	// A uuid is an id; anything else is an address. Lower-cased because the users table's
	// uniqueness is case-insensitive and an operator types their address however they type it.
	if validUUID(who) {
		err := pool.QueryRow(ctx, `SELECT id, email FROM users WHERE id = $1`, who).Scan(&id, &email)
		if errors.Is(err, pgx.ErrNoRows) {
			return cliOperator{}, fmt.Errorf("no user has id %q", who)
		}
		if err != nil {
			return cliOperator{}, fmt.Errorf("look up operator: %w", err)
		}
	} else {
		err := pool.QueryRow(ctx, `SELECT id, email FROM users WHERE lower(email) = lower($1)`,
			who).Scan(&id, &email)
		if errors.Is(err, pgx.ErrNoRows) {
			return cliOperator{}, fmt.Errorf("no user has the address %q", who)
		}
		if err != nil {
			return cliOperator{}, fmt.Errorf("look up operator: %w", err)
		}
	}

	// The permission check runs through authn, which is the same resolution the request path uses:
	// the customer baseline plus every role held, system roles from the compiled map and custom
	// roles from the table.
	hasher, err := authn.NewHasher(authn.DefaultArgon2Params())
	if err != nil {
		return cliOperator{}, err
	}
	svc, err := authn.New(pool, hasher, authn.DefaultConfig())
	if err != nil {
		return cliOperator{}, err
	}
	perms, err := svc.PermissionsForUser(ctx, id)
	if err != nil {
		return cliOperator{}, fmt.Errorf("read permissions for %s: %w", email, err)
	}
	if _, ok := perms[required]; !ok {
		return cliOperator{}, fmt.Errorf("%s does not hold %s, so the panel would refuse this change",
			email, required)
	}

	return cliOperator{
		actor:  audit.Actor{ID: id, Kind: audit.ActorUser},
		userID: id,
		label:  email,
	}, nil
}

// osOperatorLabel describes the OS user and host behind a system action.
//
// It is not an identity — it is a hint. Recording "who ran the command" as the shell's $USER is
// better than recording nobody, and it is recorded as `changes.operator` rather than as the actor,
// so the audit log never claims a person acted when no login did.
func osOperatorLabel() string {
	user := strings.TrimSpace(os.Getenv("USER"))
	if user == "" {
		user = strings.TrimSpace(os.Getenv("LOGNAME"))
	}
	if user == "" {
		user = "unknown-user"
	}
	host, err := os.Hostname()
	if err != nil || strings.TrimSpace(host) == "" {
		host = "unknown-host"
	}
	return user + "@" + host
}

// cliAuditor writes the audit row for a CLI settings change.
//
// It implements audit.Auditor, which is what internal/settings takes: the row is written inside the
// transaction that stores the value, so a rolled-back change cannot leave an audit row claiming it
// happened. What it adds to the diff is `source: cli` and the operator label, so a reader can tell a
// shell change from a screen change without opening the correlation log.
type cliAuditor struct {
	actor    audit.Actor
	operator string
}

func (a cliAuditor) RecordAction(ctx context.Context, tx pgx.Tx, action, targetType, targetID string,
	changes map[string]any) error {
	// The map belongs to the caller, so it is copied rather than mutated: a caller that reused it
	// would otherwise find fields it never set.
	merged := make(map[string]any, len(changes)+2)
	for k, v := range changes {
		merged[k] = v
	}
	merged["source"] = "cli"
	if a.operator != "" {
		merged["operator"] = a.operator
	}
	return audit.Record(ctx, tx, audit.Event{
		Actor:      a.actor,
		Action:     action,
		TargetType: targetType,
		TargetID:   targetID,
		Outcome:    audit.OutcomeSuccess,
		Changes:    merged,
	})
}

// validUUID reports whether s is a canonical UUID, so `--as` can accept either an id or an address.
func validUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, r := range s {
		switch i {
		case 8, 13, 18, 23:
			if r != '-' {
				return false
			}
		default:
			ok := r >= '0' && r <= '9' || r >= 'a' && r <= 'f' || r >= 'A' && r <= 'F'
			if !ok {
				return false
			}
		}
	}
	return true
}

// --------------------------------------------------------------- logs tail --

// adminLogsTail prints the operator logs, newest last, like tail(1).
//
// Usage: billing admin logs tail [--kind email|error|http|all] [--limit 20] [--follow]
// [--interval 2s]
func adminLogsTail(args []string) error {
	flags, positional, err := splitFlags(args,
		[]string{"kind", "limit", "interval"}, []string{"follow"})
	if err != nil {
		return err
	}
	if len(positional) != 0 {
		return errors.New("usage: billing admin logs tail " +
			"[--kind email|error|http|all] [--limit 20] [--follow] [--interval 2s]")
	}

	kinds, err := logKinds(flags["kind"])
	if err != nil {
		return err
	}
	limit := 20
	if raw := strings.TrimSpace(flags["limit"]); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > 1000 {
			return fmt.Errorf("--limit must be a whole number between 1 and 1000, got %q", raw)
		}
		limit = parsed
	}
	interval := 2 * time.Second
	if raw := strings.TrimSpace(flags["interval"]); raw != "" {
		parsed, err := time.ParseDuration(raw)
		if err != nil || parsed < 100*time.Millisecond {
			return fmt.Errorf("--interval must be a duration of at least 100ms, got %q", raw)
		}
		interval = parsed
	}
	follow := flags["follow"] == "true"

	ctx, cancel := signalContext()
	defer cancel()

	pool, err := store.Open(ctx, store.ConfigFromEnv())
	if err != nil {
		return err
	}
	defer pool.Close()

	tailer := &logTailer{pool: pool, kinds: kinds, out: os.Stdout}

	// The first read is the tail itself: the newest `limit` rows across the selected tables,
	// printed oldest first so the last line on screen is the most recent one.
	rows, err := tailer.recent(ctx, limit)
	if err != nil {
		return err
	}
	tailer.print(rows)
	if !follow {
		return nil
	}

	// Following starts from the newest row already printed. Rows sharing its timestamp are caught
	// by the seen-set rather than by a strictly-greater comparison, so a burst of rows written in
	// the same microsecond is printed once each and never twice.
	tailer.watermark = rows[len(rows)-1].when
	fmt.Fprintf(os.Stderr, "following %s (Ctrl-C to stop)\n", strings.Join(kinds, ", "))

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			next, err := tailer.since(ctx, tailer.watermark)
			if err != nil {
				// A failed poll is reported and retried: a tail that exits on one unreachable
				// moment is a tail an operator has to babysit during exactly the incident it is
				// for.
				fmt.Fprintf(os.Stderr, "poll failed: %v\n", err)
				continue
			}
			tailer.print(next)
		}
	}
}

// logKinds validates --kind and returns the tables to read.
func logKinds(raw string) ([]string, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "all":
		return []string{"email", "error", "http"}, nil
	case "email":
		return []string{"email"}, nil
	case "error":
		return []string{"error"}, nil
	case "http":
		return []string{"http"}, nil
	default:
		return nil, fmt.Errorf("--kind must be email, error, http or all, got %q", raw)
	}
}

// cliLogRow is one line from one of the three tables.
type cliLogRow struct {
	kind string
	when time.Time
	// id is the row's primary key as text. It is the dedupe key: `error_logs` and `http_logs` use a
	// bigserial and `notification_deliveries` a uuid, and the difference must not leak into the
	// polling logic.
	id   string
	line string
}

// logTailer reads the three log tables and remembers what it has printed.
type logTailer struct {
	pool  *store.Pool
	kinds []string
	out   io.Writer
	// watermark is the newest timestamp printed, and the lower bound of the next poll.
	watermark time.Time
	// seen holds the ids printed since the watermark passed their timestamp. It is bounded: a
	// long-running tail must not grow a map forever, and an id that falls out of the window can
	// only be re-printed if it is older than everything else in the window, which cannot happen
	// because the watermark only moves forward.
	seen      map[string]struct{}
	seenOrder []string
}

// recent reads the newest limit rows across the selected tables, oldest first.
func (t *logTailer) recent(ctx context.Context, limit int) ([]cliLogRow, error) {
	var all []cliLogRow
	for _, kind := range t.kinds {
		rows, err := t.read(ctx, kind, nil, limit)
		if err != nil {
			return nil, err
		}
		all = append(all, rows...)
	}
	sortLogRows(all)
	if len(all) > limit {
		all = all[len(all)-limit:]
	}
	return all, nil
}

// since reads rows newer than the watermark, oldest first.
func (t *logTailer) since(ctx context.Context, watermark time.Time) ([]cliLogRow, error) {
	var all []cliLogRow
	for _, kind := range t.kinds {
		rows, err := t.read(ctx, kind, &watermark, 200)
		if err != nil {
			return nil, err
		}
		all = append(all, rows...)
	}
	sortLogRows(all)
	return all, nil
}

// read queries one table.
//
// The same statement serves the initial tail and the follow poll: `since` is nil for the first
// page, in which case the newest rows are read with a DESC limit and reversed, which is what tail(1)
// means.
func (t *logTailer) read(ctx context.Context, kind string, since *time.Time, limit int) ([]cliLogRow, error) {
	var (
		stmt  string
		args  []any
		build func(pgx.Rows) (cliLogRow, error)
	)
	switch kind {
	case "error":
		// The message is redacted and flattened on the way out for the same reason the /logs
		// screen does it: a detail is free text and may carry a credential.
		build = func(rows pgx.Rows) (cliLogRow, error) {
			var (
				row     cliLogRow
				id      string
				when    time.Time
				level   string
				source  string
				message string
			)
			if err := rows.Scan(&id, &when, &level, &source, &message); err != nil {
				return cliLogRow{}, err
			}
			row = cliLogRow{kind: kind, when: when, id: id,
				line: fmt.Sprintf("%s %-5s [%s] %s", stamp(when), level, source, flatten(message))}
			return row, nil
		}
	case "http":
		build = func(rows pgx.Rows) (cliLogRow, error) {
			var (
				row        cliLogRow
				id         string
				when       time.Time
				method     string
				path       string
				status     int
				durationMS int
				ip         string
			)
			if err := rows.Scan(&id, &when, &method, &path, &status, &durationMS, &ip); err != nil {
				return cliLogRow{}, err
			}
			row = cliLogRow{kind: kind, when: when, id: id,
				line: fmt.Sprintf("%s %-5s %3d %s %s %dms %s", stamp(when), "http", status,
					method, flatten(redactCLILogText(path)), durationMS, ip)}
			return row, nil
		}
	case "email":
		build = func(rows pgx.Rows) (cliLogRow, error) {
			var (
				row      cliLogRow
				id       string
				when     time.Time
				mailKind string
				to       string
				subject  string
				status   string
				attempts int
			)
			if err := rows.Scan(&id, &when, &mailKind, &to, &subject, &status, &attempts); err != nil {
				return cliLogRow{}, err
			}
			// The body is deliberately absent, exactly as in the viewer: a password-reset mail's
			// body carries a working token.
			row = cliLogRow{kind: kind, when: when, id: id,
				line: fmt.Sprintf("%s %-5s %s %s %q attempts=%d", stamp(when), "email", status,
					to, flatten(redactCLILogText(subject)), attempts)}
			return row, nil
		}
	default:
		return nil, fmt.Errorf("unknown log kind %q", kind)
	}

	switch kind {
	case "error":
		stmt = `SELECT id::text, created_at, level, source, message FROM error_logs`
	case "http":
		stmt = `SELECT id::text, created_at, method, path, status, duration_ms,
		               COALESCE(host(ip), '') FROM http_logs`
	case "email":
		stmt = `SELECT id::text, created_at, kind, to_address, subject, status, attempts
		        FROM notification_deliveries`
	}

	order := "created_at DESC, id DESC"
	if since != nil {
		args = append(args, *since)
		stmt += " WHERE created_at >= $1"
		order = "created_at ASC, id ASC"
	}
	args = append(args, limit)
	stmt += " ORDER BY " + order + " LIMIT $" + strconv.Itoa(len(args))

	rows, err := t.pool.Query(ctx, stmt, args...)
	if err != nil {
		return nil, fmt.Errorf("read the %s log: %w", kind, err)
	}
	defer rows.Close()

	out := make([]cliLogRow, 0, limit)
	for rows.Next() {
		row, err := build(rows)
		if err != nil {
			return nil, fmt.Errorf("scan the %s log: %w", kind, err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the %s log: %w", kind, err)
	}
	if since == nil {
		// The newest-first read is reversed so the caller can print it top to bottom.
		for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
			out[i], out[j] = out[j], out[i]
		}
	}
	return out, nil
}

// print writes rows it has not written before, and moves the watermark.
func (t *logTailer) print(rows []cliLogRow) {
	if t.seen == nil {
		t.seen = make(map[string]struct{}, 256)
	}
	for _, row := range rows {
		key := row.kind + ":" + row.id
		if _, done := t.seen[key]; done {
			continue
		}
		fmt.Fprintln(t.out, row.line)
		t.remember(key)
		if row.when.After(t.watermark) {
			t.watermark = row.when
		}
	}
}

// remember adds a key to the seen set and keeps the set bounded.
func (t *logTailer) remember(key string) {
	const maxSeen = 4096
	t.seen[key] = struct{}{}
	t.seenOrder = append(t.seenOrder, key)
	if len(t.seenOrder) <= maxSeen {
		return
	}
	drop := t.seenOrder[0]
	t.seenOrder = t.seenOrder[1:]
	delete(t.seen, drop)
}

// sortLogRows orders rows by timestamp, then by kind and id so the output is deterministic when
// several tables write in the same instant.
func sortLogRows(rows []cliLogRow) {
	sort.Slice(rows, func(i, j int) bool {
		if !rows[i].when.Equal(rows[j].when) {
			return rows[i].when.Before(rows[j].when)
		}
		if rows[i].kind != rows[j].kind {
			return rows[i].kind < rows[j].kind
		}
		return rows[i].id < rows[j].id
	})
}

// stamp renders a timestamp the way a log line is read: to the second, in UTC.
func stamp(when time.Time) string {
	return when.UTC().Format("2006-01-02T15:04:05Z")
}

// flatten collapses a multi-line value into one line so a log tail stays a tail.
func flatten(s string) string {
	const maxLen = 1000
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > maxLen {
		return s[:maxLen] + "…"
	}
	return s
}

// logSecretMarkers are the substrings that make a piece of log text a credential.
//
// They mirror internal/httpapi/logs.go, which owns the rule for the /logs screen. The list is
// duplicated rather than shared because that file belongs to the operator-surface package and this
// one must not import its HTTP handlers; the two are asserted to agree by a test, so a marker added
// there without adding it here fails the build rather than quietly leaking through the CLI.
var logSecretMarkers = []string{
	"authorization", "password", "passwd", "secret", "token", "api_key", "apikey",
	"private_key", "cookie", "credential", "signature", "card_number", "cvv", "cvc",
}

// redactCLILogText removes credential-looking key/value pairs from free text.
//
// Same rule as the viewer's: a marker followed by `:` or `=` loses its value, because a log line that
// reached the table from an older build (or from a hand-run INSERT) must not become a credential on
// an operator's terminal either.
func redactCLILogText(s string) string {
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
		b.WriteString(rest[:pos])
		end := pos + len(marker)
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
		b.WriteString(rest[pos : sep+1])
		b.WriteString(" [redacted]")
		rest = rest[sep+1:]
		// Skip the value: to the next whitespace, or to the end.
		for len(rest) > 0 && rest[0] != ' ' && rest[0] != '\t' {
			rest = rest[1:]
		}
	}
	return b.String()
}

// splitFlags separates `--name value` and `--name=value` pairs from positional arguments.
//
// The standard flag package stops parsing at the first positional, so
// `setting set company name Acme --as me@example.com` would leave `--as` unparsed and the command
// would silently ignore it. An operator types the command in the order the usage line shows, so the
// parser accepts flags anywhere.
//
// values names the flags that take a value; bools names the flags that do not. A boolean flag's value
// in the returned map is "true", so `flags["follow"] == "true"` reads naturally.
func splitFlags(args, values, bools []string) (map[string]string, []string, error) {
	valueSet := make(map[string]struct{}, len(values))
	for _, name := range values {
		valueSet[name] = struct{}{}
	}
	boolSet := make(map[string]struct{}, len(bools))
	for _, name := range bools {
		boolSet[name] = struct{}{}
	}

	out := make(map[string]string, len(values)+len(bools))
	var positional []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			positional = append(positional, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(arg, "--") || arg == "--" {
			// `-` is the stdin marker, not a flag.
			positional = append(positional, arg)
			continue
		}
		name := strings.TrimPrefix(arg, "--")
		inline := ""
		hasInline := false
		if idx := strings.Index(name, "="); idx >= 0 {
			name, inline = name[:idx], name[idx+1:]
			hasInline = true
		}
		if _, ok := boolSet[name]; ok {
			if hasInline {
				return nil, nil, fmt.Errorf("--%s does not take a value", name)
			}
			out[name] = "true"
			continue
		}
		if _, ok := valueSet[name]; !ok {
			return nil, nil, fmt.Errorf("unknown flag --%s", name)
		}
		if hasInline {
			out[name] = inline
			continue
		}
		if i+1 >= len(args) {
			return nil, nil, fmt.Errorf("--%s needs a value", name)
		}
		i++
		out[name] = args[i]
	}
	return out, positional, nil
}
