// IMAP polling (E3.8 / TKT-11): the second ingest path.
//
// A support desk that only accepts a webhook needs a provider in front of the mailbox. A
// panel that speaks IMAP itself can be pointed at any mailbox, which is why this file
// implements the protocol rather than adding a mail library: what the panel needs is a
// fetch-and-mark-read loop over implicit TLS, and that is a few hundred lines of
// net/textproto against a dependency nobody else in the tree would use.
//
// What it does, in order, once per poll:
//
//  1. Connect (implicit TLS, port 993), LOGIN, SELECT the mailbox.
//  2. UID SEARCH UNSEEN, bounded to MaxMessages, so one poll cannot drain a mailbox.
//  3. For each uid: ask for RFC822.SIZE. A message over MaxMessageBytes is *refused* — its
//     headers are fetched so the refusal can be logged with a real Message-ID, and it is
//     marked seen so it is not refetched forever. Truncating a support request is worse than
//     a recorded refusal an agent can act on.
//  4. Otherwise fetch the body with a partial fetch (`BODY.PEEK[]<0.N>`), parse it, and hand
//     it to the same Ingest the webhook uses. BODY.PEEK, not BODY: the \Seen flag is set only
//     *after* the reply commits, so a crash between the two means a refetch that dedupes on
//     the Message-ID instead of a message that was marked read and never imported.
//  5. Mark the message seen.
//
// Attachments are imported (TKT-11): a screenshot pasted into a reply is most of a support
// ticket, and refusing it means the agent has to ask for it in a second message. The file is
// run through exactly the same checks the portal upload applies — the size cap, the
// content-type allowlist, the base-name normalisation, the digest — so the pipe is not a way
// around them. A part that fails a check is refused and counted, never stored, and the count
// reaches the mail log so "my attachment did not arrive" is answerable.
//
// Known gaps, stated rather than hidden: STARTTLS (port 143) is not implemented, so the
// poller requires implicit TLS; only the INBOX is polled; and a body that is neither UTF-8
// nor ASCII is stored as the bytes the transfer encoding produced. None of these are silent —
// the port check refuses loudly and the rest is documented here and in the report.

package tickets

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"html"
	"io"
	"log/slog"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/textproto"
	"strconv"
	"strings"
	"time"

	"github.com/catalyst/billing/internal/store"
)

// IMAP bounds and defaults.
const (
	// DefaultIMAPPort is implicit TLS. Port 143 would need STARTTLS, which this poller
	// refuses rather than silently talking plaintext.
	DefaultIMAPPort = 993
	// DefaultIMAPMailbox is where a support address's mail lands.
	DefaultIMAPMailbox = "INBOX"
	// DefaultIMAPBatch bounds one poll. A backlog drains over several ticks instead of in one
	// long connection that holds the mailbox.
	DefaultIMAPBatch = 20
	// MaxIMAPBatch caps an operator's configuration. One poll fetching a thousand messages is
	// a poll that outlives its timeout.
	MaxIMAPBatch = 100
	// DefaultIMAPTimeout bounds the whole conversation.
	DefaultIMAPTimeout = 30 * time.Second
	// maxHeaderBytes bounds a refused message's header fetch.
	maxHeaderBytes = 8 << 10
	// maxMIMEDepth bounds nested multipart parsing. Real mail is at most three levels; a
	// message claiming more is trying to make the parser recurse.
	maxMIMEDepth = 4
)

// IMAPConfig is the poller's configuration, read from the mail settings group.
type IMAPConfig struct {
	Host     string
	Port     int
	Username string
	Password string
	// Mailbox defaults to INBOX.
	Mailbox string
	// MaxMessages bounds one poll. Zero means DefaultIMAPBatch.
	MaxMessages int
	// MaxMessageBytes bounds one message. Zero means MaxMessageBytes.
	MaxMessageBytes int
	// Timeout bounds the whole conversation. Zero means DefaultIMAPTimeout.
	Timeout time.Duration
	// TLS is the client configuration. Nil verifies the server against Host.
	TLS *tls.Config
	// Dial overrides the transport. It exists so a test can point the poller at an in-process
	// TLS server without weakening verification anywhere else.
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
}

// Configured reports whether the poller has somewhere to connect.
func (c IMAPConfig) Configured() bool {
	return strings.TrimSpace(c.Host) != "" && strings.TrimSpace(c.Username) != ""
}

func (c IMAPConfig) port() int {
	if c.Port > 0 {
		return c.Port
	}
	return DefaultIMAPPort
}

func (c IMAPConfig) mailbox() string {
	if strings.TrimSpace(c.Mailbox) != "" {
		return strings.TrimSpace(c.Mailbox)
	}
	return DefaultIMAPMailbox
}

func (c IMAPConfig) batch() int {
	switch {
	case c.MaxMessages <= 0:
		return DefaultIMAPBatch
	case c.MaxMessages > MaxIMAPBatch:
		return MaxIMAPBatch
	default:
		return c.MaxMessages
	}
}

func (c IMAPConfig) maxBytes() int {
	if c.MaxMessageBytes <= 0 {
		return MaxMessageBytes
	}
	return c.MaxMessageBytes
}

func (c IMAPConfig) timeout() time.Duration {
	if c.Timeout > 0 {
		return c.Timeout
	}
	return DefaultIMAPTimeout
}

// settingsReader is the read surface of the settings store this package uses. It is an
// interface rather than a *settings.Store so a test can drive the parser without a database.
type settingsReader interface {
	String(ctx context.Context, group, key string) (string, error)
	Int(ctx context.Context, group, key string) (int64, error)
}

// loadIMAPConfig reads the IMAP settings through a settings reader.
func loadIMAPConfig(ctx context.Context, reader settingsReader) (IMAPConfig, error) {
	var out IMAPConfig
	var err error
	if out.Host, err = reader.String(ctx, "mail", "imap_host"); err != nil {
		return IMAPConfig{}, fmt.Errorf("tickets: read imap host: %w", err)
	}
	if out.Username, err = reader.String(ctx, "mail", "imap_username"); err != nil {
		return IMAPConfig{}, fmt.Errorf("tickets: read imap username: %w", err)
	}
	if out.Password, err = reader.String(ctx, "mail", "imap_password"); err != nil {
		return IMAPConfig{}, fmt.Errorf("tickets: read imap password: %w", err)
	}
	port, err := reader.Int(ctx, "mail", "imap_port")
	if err != nil {
		return IMAPConfig{}, fmt.Errorf("tickets: read imap port: %w", err)
	}
	if port > 0 && port <= 65535 {
		out.Port = int(port)
	}
	out.Host = strings.TrimSpace(out.Host)
	out.Username = strings.TrimSpace(out.Username)
	out.MaxMessageBytes = MaxMessageBytes
	return out, nil
}

// PollResult summarises one poll. The counts are what the worker logs, and they are the
// operator's answer to "is piping working".
type PollResult struct {
	// Fetched is how many messages the poll examined.
	Fetched int
	// Attached, Created, Duplicates, Ignored and Unattributed count the outcomes.
	Attached     int
	Created      int
	Duplicates   int
	Ignored      int
	Unattributed int
	// TooLarge is messages refused for size. They are also counted in Ignored.
	TooLarge int
	// Failed is messages whose ingest returned an error. They are not marked seen, so the
	// next poll retries them.
	Failed int
}

// Poller fetches unseen mail and hands it to the store's ingest core.
type Poller struct {
	store  *Store
	cfg    IMAPConfig
	logger *slog.Logger
}

// NewPoller constructs a Poller.
func NewPoller(pool *store.Pool, cfg IMAPConfig) *Poller {
	return &Poller{store: New(pool), cfg: cfg}
}

// WithLogger attaches a logger.
func (p *Poller) WithLogger(logger *slog.Logger) *Poller {
	p.logger = logger
	return p
}

func (p *Poller) log() *slog.Logger {
	if p.logger != nil {
		return p.logger
	}
	return slog.Default()
}

// Poll reads the unseen messages and ingests them.
//
// It returns an error only when the conversation itself failed (connect, login, select) or
// when at least one message could not be ingested; a mailbox with nothing new is a success
// with a zero result.
func (p *Poller) Poll(ctx context.Context) (PollResult, error) {
	var res PollResult
	if !p.cfg.Configured() {
		// Not configured is not a failure: a fresh installation has no mailbox to poll and the
		// job must be harmless.
		return res, nil
	}

	conn, err := dialIMAP(ctx, p.cfg)
	if err != nil {
		return res, err
	}
	defer conn.close()

	if err := conn.login(p.cfg.Username, p.cfg.Password); err != nil {
		return res, err
	}
	if err := conn.selectMailbox(p.cfg.mailbox()); err != nil {
		return res, err
	}
	defer func() { _ = conn.logout() }()

	uids, err := conn.searchUnseen(p.cfg.batch())
	if err != nil {
		return res, err
	}
	var firstErr error
	for _, uid := range uids {
		res.Fetched++
		size, err := conn.size(uid)
		if err != nil {
			res.Failed++
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if size > p.cfg.maxBytes() {
			p.refuseOversized(ctx, conn, uid, size, &res)
			continue
		}
		raw, truncated, err := conn.body(uid, p.cfg.maxBytes()+1)
		if err != nil {
			res.Failed++
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if truncated || len(raw) > p.cfg.maxBytes() {
			// The server's RFC822.SIZE lied, or the message grew. Either way the body we have is
			// incomplete and must not become a support reply.
			p.refuseOversized(ctx, conn, uid, len(raw), &res)
			continue
		}
		msg, err := ParseInbound(raw)
		if err != nil {
			res.Ignored++
			if _, lerr := p.store.RecordDropped(ctx, DroppedMessage{
				Reason: ReasonUnparseable, Source: "imap",
			}); lerr != nil {
				p.log().Error("record unparseable message", "uid", uid, "error", lerr)
			}
			_ = conn.markSeen(uid)
			continue
		}
		msg.Source = "imap"

		out, err := p.store.Ingest(ctx, msg)
		if err != nil {
			// Do not mark it seen: the next poll retries, and the Message-ID dedupe makes the
			// retry safe if this one actually committed before the error surfaced.
			res.Failed++
			if firstErr == nil {
				firstErr = err
			}
			p.log().Error("ingest inbound message", "uid", uid,
				"message_id", msg.MessageID, "error", err)
			continue
		}
		_ = conn.markSeen(uid)
		switch out.Outcome {
		case IngestAttached:
			res.Attached++
		case IngestCreated:
			res.Created++
		case IngestDuplicate:
			res.Duplicates++
		case IngestIgnored:
			res.Ignored++
		case IngestUnattributed:
			res.Unattributed++
		}
	}
	if firstErr != nil {
		return res, fmt.Errorf("tickets: imap poll: %w", firstErr)
	}
	return res, nil
}

// refuseOversized records a message that is too large to become a reply.
//
// The headers are fetched so the log row carries the real Message-ID, From and Subject: an
// operator looking at the mailbox needs to know *which* message was refused, and a row with
// an empty subject is not an answer.
func (p *Poller) refuseOversized(ctx context.Context, conn *imapConn, uid, size int, res *PollResult) {
	dropped := DroppedMessage{Reason: ReasonTooLarge, Source: "imap"}
	if headers, err := conn.headers(uid, maxHeaderBytes); err == nil {
		if parsed, perr := ParseInbound(headers); perr == nil {
			dropped.MessageID = parsed.MessageID
			dropped.From = parsed.From
			dropped.Subject = parsed.Subject
		}
	}
	if _, err := p.store.RecordDropped(ctx, dropped); err != nil {
		p.log().Error("record oversized message", "uid", uid, "size", size, "error", err)
	}
	_ = conn.markSeen(uid)
	res.TooLarge++
	res.Ignored++
}

// ------------------------------------------------------------------- transport --

// imapConn is a minimal IMAP4rev1 client: enough to log in, search and fetch, and nothing
// else. Every response is read as a line plus, when announced, a literal.
type imapConn struct {
	conn net.Conn
	tp   *textproto.Reader
	w    *bufio.Writer
	tag  int
}

// dialIMAP connects and performs the TLS handshake and the greeting exchange.
func dialIMAP(ctx context.Context, cfg IMAPConfig) (*imapConn, error) {
	port := cfg.port()
	if port == 143 {
		// Refuse rather than send a password in the clear. The alternative — silently talking
		// plaintext because the operator picked the standard non-TLS port — is how credentials
		// leak.
		return nil, errors.New("imap: port 143 requires STARTTLS, which this poller does not " +
			"implement; use the implicit-TLS port (993)")
	}
	addr := net.JoinHostPort(cfg.Host, strconv.Itoa(port))
	timeout := cfg.timeout()

	dial := cfg.Dial
	if dial == nil {
		dialer := &net.Dialer{Timeout: timeout}
		dial = dialer.DialContext
	}
	raw, err := dial(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("imap: dial %s: %w", addr, err)
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = raw.SetDeadline(deadline)
	} else {
		_ = raw.SetDeadline(time.Now().Add(timeout))
	}

	tlsConfig := cfg.TLS
	if tlsConfig == nil {
		tlsConfig = &tls.Config{ServerName: cfg.Host, MinVersion: tls.VersionTLS12}
	}
	secured := tls.Client(raw, tlsConfig)
	if err := secured.HandshakeContext(ctx); err != nil {
		raw.Close()
		return nil, fmt.Errorf("imap: tls handshake: %w", err)
	}

	c := &imapConn{
		conn: secured,
		tp:   textproto.NewReader(bufio.NewReader(secured)),
		w:    bufio.NewWriter(secured),
	}
	greeting, err := c.tp.ReadLine()
	if err != nil {
		c.close()
		return nil, fmt.Errorf("imap: read greeting: %w", err)
	}
	// "* OK [CAPABILITY ...] server ready" or "* PREAUTH".
	upper := strings.ToUpper(greeting)
	if !strings.HasPrefix(upper, "* OK") && !strings.HasPrefix(upper, "* PREAUTH") {
		c.close()
		return nil, fmt.Errorf("imap: server refused the connection: %s", truncateText(greeting, 200))
	}
	return c, nil
}

func (c *imapConn) close() {
	if c.conn != nil {
		_ = c.conn.Close()
	}
}

// command sends one tagged command and reads until its completion.
func (c *imapConn) command(format string, args ...any) ([]string, error) {
	c.tag++
	tag := "p" + strconv.Itoa(c.tag)
	if _, err := fmt.Fprintf(c.w, "%s %s\r\n", tag, fmt.Sprintf(format, args...)); err != nil {
		return nil, fmt.Errorf("imap: write command: %w", err)
	}
	if err := c.w.Flush(); err != nil {
		return nil, fmt.Errorf("imap: write command: %w", err)
	}

	var lines []string
	for {
		line, err := c.tp.ReadLine()
		if err != nil {
			return nil, fmt.Errorf("imap: read response: %w", err)
		}
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, tag+" ") {
			rest := strings.TrimSpace(trimmed[len(tag)+1:])
			if !strings.HasPrefix(strings.ToUpper(rest), "OK") {
				return lines, fmt.Errorf("imap: %s", truncateText(rest, 200))
			}
			return lines, nil
		}
		lines = append(lines, line)
	}
}

func (c *imapConn) login(username, password string) error {
	quotedUser, err := imapQuote(username)
	if err != nil {
		return err
	}
	quotedPass, err := imapQuote(password)
	if err != nil {
		return err
	}
	if _, err := c.command("LOGIN %s %s", quotedUser, quotedPass); err != nil {
		return fmt.Errorf("imap: login: %w", err)
	}
	return nil
}

func (c *imapConn) selectMailbox(mailbox string) error {
	quoted, err := imapQuote(mailbox)
	if err != nil {
		return err
	}
	if _, err := c.command("SELECT %s", quoted); err != nil {
		return fmt.Errorf("imap: select %s: %w", mailbox, err)
	}
	return nil
}

// searchUnseen returns at most limit unseen uids, oldest first.
func (c *imapConn) searchUnseen(limit int) ([]int, error) {
	lines, err := c.command("UID SEARCH UNSEEN")
	if err != nil {
		return nil, fmt.Errorf("imap: search unseen: %w", err)
	}
	var uids []int
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(strings.ToUpper(trimmed), "* SEARCH") {
			continue
		}
		for _, field := range strings.Fields(trimmed)[2:] {
			uid, err := strconv.Atoi(field)
			if err != nil || uid <= 0 {
				continue
			}
			uids = append(uids, uid)
			if len(uids) >= limit {
				return uids, nil
			}
		}
	}
	return uids, nil
}

// size asks for one message's RFC822.SIZE.
func (c *imapConn) size(uid int) (int, error) {
	lines, err := c.command("UID FETCH %d (UID RFC822.SIZE)", uid)
	if err != nil {
		return 0, fmt.Errorf("imap: fetch size %d: %w", uid, err)
	}
	for _, line := range lines {
		if size, ok := fetchInt(line, "RFC822.SIZE"); ok {
			return size, nil
		}
	}
	return 0, fmt.Errorf("imap: fetch size %d: no size in the response", uid)
}

// headers fetches the message headers only, bounded by limit.
func (c *imapConn) headers(uid, limit int) ([]byte, error) {
	literal, _, err := c.fetchLiteral(uid, fmt.Sprintf("BODY.PEEK[HEADER]<0.%d>", limit))
	return literal, err
}

// body fetches the whole message, bounded by limit. truncated reports that the literal was
// exactly the requested size, which means the server may have more to give.
func (c *imapConn) body(uid, limit int) (raw []byte, truncated bool, err error) {
	literal, _, err := c.fetchLiteral(uid, fmt.Sprintf("BODY.PEEK[]<0.%d>", limit))
	if err != nil {
		return nil, false, err
	}
	return literal, len(literal) >= limit, nil
}

// fetchLiteral issues a FETCH for one literal-valued item and reads the literal.
func (c *imapConn) fetchLiteral(uid int, item string) ([]byte, int, error) {
	c.tag++
	tag := "p" + strconv.Itoa(c.tag)
	if _, err := fmt.Fprintf(c.w, "%s UID FETCH %d (UID %s)\r\n", tag, uid, item); err != nil {
		return nil, 0, fmt.Errorf("imap: write fetch: %w", err)
	}
	if err := c.w.Flush(); err != nil {
		return nil, 0, fmt.Errorf("imap: write fetch: %w", err)
	}

	var (
		literal []byte
		size    int
	)
	for {
		line, err := c.tp.ReadLine()
		if err != nil {
			return nil, 0, fmt.Errorf("imap: read fetch: %w", err)
		}
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, tag+" ") {
			rest := strings.TrimSpace(trimmed[len(tag)+1:])
			if !strings.HasPrefix(strings.ToUpper(rest), "OK") {
				return nil, 0, fmt.Errorf("imap: fetch %d: %s", uid, truncateText(rest, 200))
			}
			return literal, size, nil
		}
		if n, ok := literalSize(trimmed); ok {
			// The line ends with "{n}": the next n bytes are the literal, and they are read from
			// the same buffered reader the lines came from.
			if n > 2*MaxMessageBytes {
				return nil, 0, fmt.Errorf("imap: fetch %d: literal of %d bytes is out of bounds", uid, n)
			}
			buf := make([]byte, n)
			if _, err := io.ReadFull(c.tp.R, buf); err != nil {
				return nil, 0, fmt.Errorf("imap: read literal: %w", err)
			}
			literal = buf
			continue
		}
		if v, ok := fetchInt(trimmed, "RFC822.SIZE"); ok {
			size = v
		}
	}
}

// markSeen sets \Seen on one message.
func (c *imapConn) markSeen(uid int) error {
	if _, err := c.command(`UID STORE %d +FLAGS (\Seen)`, uid); err != nil {
		return fmt.Errorf("imap: mark %d seen: %w", uid, err)
	}
	return nil
}

// logout is best effort: the conversation is over either way.
func (c *imapConn) logout() error {
	_, err := c.command("LOGOUT")
	return err
}

// imapQuote renders a quoted string, refusing anything that would break out of it.
func imapQuote(value string) (string, error) {
	if strings.ContainsAny(value, "\r\n\x00") {
		return "", errors.New("imap: a credential contains a line break")
	}
	var b strings.Builder
	b.Grow(len(value) + 2)
	b.WriteByte('"')
	for _, r := range value {
		if r == '"' || r == '\\' {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	b.WriteByte('"')
	return b.String(), nil
}

// literalSize reads the "{n}" announcement at the end of a FETCH response line.
func literalSize(line string) (int, bool) {
	line = strings.TrimSpace(line)
	if !strings.HasSuffix(line, "}") {
		return 0, false
	}
	open := strings.LastIndexByte(line, '{')
	if open < 0 {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimSuffix(line[open+1:], "}"))
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

// fetchInt reads "KEY <digits>" out of a FETCH response line.
func fetchInt(line, key string) (int, bool) {
	upper := strings.ToUpper(line)
	idx := strings.Index(upper, strings.ToUpper(key)+" ")
	if idx < 0 {
		return 0, false
	}
	rest := line[idx+len(key)+1:]
	end := 0
	for end < len(rest) && rest[end] >= '0' && rest[end] <= '9' {
		end++
	}
	if end == 0 {
		return 0, false
	}
	n, err := strconv.Atoi(rest[:end])
	if err != nil {
		return 0, false
	}
	return n, true
}

// --------------------------------------------------------------------- parsing --

// ParseInbound turns one RFC 5322 message into an InboundMessage.
//
// It is exported because the webhook path does not use it (a provider sends parsed JSON) and
// a test drives it directly, but it is the same parser the poller uses: a second
// implementation for tests would prove nothing about the first.
func ParseInbound(raw []byte) (InboundMessage, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return InboundMessage{}, errors.New("tickets: empty message")
	}
	hdr, body := splitHeaderBody(raw)
	if len(hdr) == 0 {
		return InboundMessage{}, errors.New("tickets: message has no headers")
	}
	decoder := &mime.WordDecoder{}
	subject, err := decoder.DecodeHeader(hdr.Get("Subject"))
	if err != nil {
		// A malformed encoded word is not a reason to lose the message; keep it verbatim.
		subject = hdr.Get("Subject")
	}
	from := hdr.Get("From")
	if decoded, derr := decoder.DecodeHeader(from); derr == nil {
		from = decoded
	}

	msg := InboundMessage{
		MessageID:     hdr.Get("Message-Id"),
		From:          from,
		Subject:       subject,
		InReplyTo:     hdr.Get("In-Reply-To"),
		References:    strings.Fields(hdr.Get("References")),
		AutoSubmitted: hdr.Get("Auto-Submitted"),
		Headers:       headerMap(hdr),
		To:            splitAddressList(hdr.Get("To")),
		Cc:            splitAddressList(hdr.Get("Cc")),
	}
	if parsed, perr := mail.ParseAddress(from); perr == nil {
		msg.FromName = parsed.Name
	}
	if when, derr := mail.ParseDate(hdr.Get("Date")); derr == nil {
		// The message's own date, so a reply is stored at the moment it was written rather than
		// at the moment the poller happened to run.
		msg.ReceivedAt = when.UTC()
	}
	text, _, attachments, refused := extractParts(hdr, body, 0)
	msg.Body = text
	msg.Attachments = attachments
	msg.AttachmentsDropped = refused
	return msg, nil
}

// splitHeaderBody separates the header block from the body.
//
// It is hand-written rather than textproto.ReadMIMEHeader because the body's offset matters:
// ReadMIMEHeader consumes buffered bytes that the body then needs.
func splitHeaderBody(raw []byte) (textproto.MIMEHeader, []byte) {
	hdr := make(textproto.MIMEHeader, 16)
	pos := 0
	lastKey := ""
	for pos < len(raw) {
		end := bytes.IndexByte(raw[pos:], '\n')
		var line []byte
		next := len(raw)
		if end < 0 {
			line = raw[pos:]
		} else {
			line = raw[pos : pos+end]
			next = pos + end + 1
		}
		line = bytes.TrimSuffix(line, []byte("\r"))
		if len(line) == 0 {
			pos = next
			break
		}
		if line[0] == ' ' || line[0] == '\t' {
			// A folded continuation belongs to the previous header.
			if lastKey != "" && len(hdr[lastKey]) > 0 {
				hdr[lastKey][len(hdr[lastKey])-1] += " " + strings.TrimSpace(string(line))
			}
			pos = next
			continue
		}
		colon := bytes.IndexByte(line, ':')
		if colon <= 0 {
			pos = next
			continue
		}
		key := textproto.CanonicalMIMEHeaderKey(strings.TrimSpace(string(line[:colon])))
		hdr[key] = append(hdr[key], strings.TrimSpace(string(line[colon+1:])))
		lastKey = key
		pos = next
	}
	if pos > len(raw) {
		pos = len(raw)
	}
	return hdr, raw[pos:]
}

// headerMap flattens the headers the loop check reads.
func headerMap(hdr textproto.MIMEHeader) map[string]string {
	out := make(map[string]string, len(hdr))
	for key, values := range hdr {
		out[key] = strings.Join(values, ", ")
	}
	return out
}

// splitAddressList splits a To/Cc header into individual addresses, leaving display names
// attached (normaliseInbound parses them).
func splitAddressList(value string) []string {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	// mail.ParseAddressList understands commas inside quoted display names; fall back to a
	// comma split when the header is malformed.
	if list, err := mail.ParseAddressList(value); err == nil {
		out := make([]string, 0, len(list))
		for _, addr := range list {
			out = append(out, addr.Address)
		}
		return out
	}
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// extractParts pulls the reply text and the attached files out of a message.
//
// The return is (text, isHTML, attachments, refused). A multipart message contributes its
// first text/plain part, falling back to its first text/html part reduced to text; any other
// part is a file. A file that passes ValidateAttachment is returned for storage; one that
// fails it — a type a browser could execute, an oversized body, an empty file — is counted as
// refused so the agent can see that something was sent and ask for it another way.
func extractParts(hdr textproto.MIMEHeader, body []byte, depth int) (string, bool, []AttachmentInput, int) {
	if depth > maxMIMEDepth {
		return "", false, nil, 1
	}
	mediaType, params, err := mime.ParseMediaType(hdr.Get("Content-Type"))
	if err != nil || mediaType == "" {
		mediaType = "text/plain"
	}
	if strings.HasPrefix(mediaType, "multipart/") {
		boundary := params["boundary"]
		if boundary == "" {
			return decodeTransfer(hdr, body), false, nil, 0
		}
		var (
			plain, rich string
			files       []AttachmentInput
			refused     int
		)
		for _, part := range splitMultipart(body, boundary) {
			partHeader, partBody := splitHeaderBody(part)
			text, isHTML, partFiles, lost := extractParts(partHeader, partBody, depth+1)
			files = append(files, partFiles...)
			refused += lost
			if text == "" {
				continue
			}
			if isHTML {
				if rich == "" {
					rich = text
				}
				continue
			}
			if plain == "" {
				plain = text
			}
		}
		if plain != "" {
			return plain, false, files, refused
		}
		return rich, true, files, refused
	}

	// A part is a file when the sender labelled it as one, named it, or sent something that is
	// not text. A `text/plain` part with no filename stays body text: that is the reply.
	disposition, dispParams, _ := mime.ParseMediaType(hdr.Get("Content-Disposition"))
	filename := dispParams["filename"]
	if filename == "" {
		filename = params["name"]
	}
	isFile := filename != "" || disposition == "attachment" || !strings.HasPrefix(mediaType, "text/")
	if isFile {
		file, ok := inboundAttachment(filename, mediaType, hdr, body)
		if !ok {
			return "", false, nil, 1
		}
		return "", false, []AttachmentInput{file}, 0
	}

	switch {
	case mediaType == "text/plain":
		return decodeTransfer(hdr, body), false, nil, 0
	case mediaType == "text/html":
		return htmlToText(decodeTransfer(hdr, body)), true, nil, 0
	case strings.HasPrefix(mediaType, "text/"):
		return decodeTransfer(hdr, body), false, nil, 0
	default:
		return "", false, nil, 1
	}
}

// inboundAttachment turns one MIME part into an upload candidate and runs it through the same
// validation the portal upload uses.
//
// The second return is false when the part must be refused rather than stored. The checks are
// not re-implemented here: ValidateAttachment owns the size cap, the allowlist and the
// filename normalisation, and this path going around it is exactly the hole that would make
// email a way to put an executable in a support thread.
func inboundAttachment(filename, mediaType string, hdr textproto.MIMEHeader, body []byte) (AttachmentInput, bool) {
	bytes := decodeTransferBytes(hdr, body)
	file := AttachmentInput{
		Filename:    filename,
		ContentType: mediaType,
		Bytes:       bytes,
	}
	if strings.TrimSpace(file.Filename) == "" {
		file.Filename = unnamedAttachment(mediaType)
	}
	validated, err := ValidateAttachment(file)
	if err != nil {
		return AttachmentInput{}, false
	}
	return validated, true
}

// unnamedAttachment names a file the sender did not name.
//
// An attachment with no filename cannot be offered as a download with a sensible label, and
// ValidateAttachment refuses the empty name outright, so a name is derived from the type. It
// is generic on purpose: the name is the sender's claim about the file, and inventing a
// specific one ("invoice.pdf") would be a claim the panel cannot support.
func unnamedAttachment(mediaType string) string {
	if exts, err := mime.ExtensionsByType(mediaType); err == nil && len(exts) > 0 {
		return "attachment" + exts[0]
	}
	return "attachment.bin"
}

// splitMultipart cuts a multipart body into its parts on the boundary.
func splitMultipart(body []byte, boundary string) [][]byte {
	delim := []byte("--" + boundary)
	start := bytes.Index(body, delim)
	if start < 0 {
		return nil
	}
	rest := body[start:]
	var parts [][]byte
	for {
		rest = rest[len(delim):]
		if bytes.HasPrefix(rest, []byte("--")) {
			// The closing delimiter.
			break
		}
		nl := bytes.IndexByte(rest, '\n')
		if nl < 0 {
			break
		}
		rest = rest[nl+1:]
		next := bytes.Index(rest, delim)
		if next < 0 {
			parts = append(parts, rest)
			break
		}
		parts = append(parts, rest[:next])
		rest = rest[next:]
	}
	return parts
}

// decodeTransferBytes is decodeTransfer without the string round-trip, for a part whose bytes
// are stored rather than rendered.
func decodeTransferBytes(hdr textproto.MIMEHeader, body []byte) []byte {
	switch strings.ToLower(strings.TrimSpace(hdr.Get("Content-Transfer-Encoding"))) {
	case "base64":
		clean := bytes.Map(func(r rune) rune {
			switch r {
			case '\r', '\n', ' ', '\t':
				return -1
			}
			return r
		}, body)
		if decoded, err := base64.StdEncoding.DecodeString(string(clean)); err == nil {
			return decoded
		}
	case "quoted-printable":
		if decoded, err := io.ReadAll(quotedprintable.NewReader(bytes.NewReader(body))); err == nil {
			return decoded
		}
	}
	return body
}

// decodeTransfer undoes the part's content transfer encoding.
func decodeTransfer(hdr textproto.MIMEHeader, body []byte) string {
	switch strings.ToLower(strings.TrimSpace(hdr.Get("Content-Transfer-Encoding"))) {
	case "base64":
		clean := bytes.Map(func(r rune) rune {
			switch r {
			case '\r', '\n', ' ', '\t':
				return -1
			}
			return r
		}, body)
		if decoded, err := base64.StdEncoding.DecodeString(string(clean)); err == nil {
			return string(decoded)
		}
	case "quoted-printable":
		if decoded, err := io.ReadAll(quotedprintable.NewReader(bytes.NewReader(body))); err == nil {
			return string(decoded)
		}
	}
	return string(body)
}

// htmlToText reduces an HTML-only body to readable text.
//
// It is deliberately crude: a customer whose mail client sent only HTML should still have
// their words stored, and the panel renders message bodies as text, so the markup must not
// reach it. A tag stripper is the smallest thing that is honest about that.
func htmlToText(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	depth := 0
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '<':
			depth++
		case '>':
			if depth > 0 {
				depth--
			}
		default:
			if depth == 0 {
				b.WriteByte(c)
			}
		}
	}
	text := html.UnescapeString(b.String())
	// Block-level tags leave their text on one line; the newlines they imply are gone, so
	// collapse runs of whitespace and keep paragraph breaks readable.
	lines := strings.Split(text, "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.Join(strings.Fields(line), " ")
		if line == "" {
			continue
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}
