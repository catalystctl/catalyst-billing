package notify

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/smtp"
	"strings"
	"time"
)

// SMTPSender delivers through a relay.
//
// It is deliberately small. The product needs "deliver a short plain-text message to
// one address"; a full MIME library would add parsing surface for messages it never
// sends. What it does handle is the part that goes wrong in production: a bounded
// dial/read timeout (an unreachable relay must not hold a worker forever), STARTTLS
// when the relay offers it, and plain authentication only over TLS.
type SMTPSender struct {
	// Host and Port of the relay.
	Host string
	Port int
	// Username and Password for PLAIN authentication. Empty means no auth.
	Username string
	Password string
	// From is the envelope sender, e.g. "billing@example.test".
	From string
	// Timeout bounds the whole conversation.
	Timeout time.Duration
	// InsecureSkipVerify is only for a test relay with a self-signed certificate. It
	// is never set from a general "insecure" switch: TLS verification is the only thing
	// stopping a relay impersonator from reading reset tokens.
	InsecureSkipVerify bool
}

// Send implements Sender: a text/plain message, exactly as this product sent before mail
// branding existed.
func (s SMTPSender) Send(ctx context.Context, to, subject, body string) error {
	return s.deliver(ctx, to, buildMessage(s.From, to, subject, body, ""))
}

// SendHTML implements HTMLSender: one multipart/alternative message carrying both the plain
// text and its branded HTML representation.
//
// Both parts are always present. A message whose only part is HTML is unreadable in a
// text-only client, and the plain part here is the same text Send would have delivered, so a
// client that prefers text is unaffected by the feature.
func (s SMTPSender) SendHTML(ctx context.Context, to, subject, text, htmlPart string) error {
	if strings.TrimSpace(htmlPart) == "" {
		return s.Send(ctx, to, subject, text)
	}
	return s.deliver(ctx, to, buildMessage(s.From, to, subject, text, htmlPart))
}

// deliver performs the SMTP conversation for one already-built message.
func (s SMTPSender) deliver(ctx context.Context, to string, message []byte) error {
	if s.Host == "" || s.From == "" {
		return errors.New("notify: smtp sender is not configured")
	}
	if strings.ContainsAny(to, "\r\n") || strings.ContainsAny(s.From, "\r\n") {
		// Header injection: a newline in an address is how a message becomes two.
		return errors.New("notify: address contains a newline")
	}
	port := s.Port
	if port == 0 {
		port = 587
	}
	timeout := s.Timeout
	if timeout <= 0 {
		timeout = 20 * time.Second
	}

	addr := net.JoinHostPort(s.Host, fmt.Sprint(port))
	dialer := &net.Dialer{Timeout: timeout}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("notify: dial %s: %w", addr, err)
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	} else {
		_ = conn.SetDeadline(time.Now().Add(timeout))
	}

	client, err := smtp.NewClient(conn, s.Host)
	if err != nil {
		conn.Close()
		return fmt.Errorf("notify: smtp handshake: %w", err)
	}
	defer client.Close()

	if ok, _ := client.Extension("STARTTLS"); ok {
		if err := client.StartTLS(&tls.Config{
			ServerName:         s.Host,
			MinVersion:         tls.VersionTLS12,
			InsecureSkipVerify: s.InsecureSkipVerify,
		}); err != nil {
			return fmt.Errorf("notify: starttls: %w", err)
		}
	}
	if s.Username != "" {
		if ok, _ := client.Extension("AUTH"); ok {
			if err := client.Auth(smtp.PlainAuth("", s.Username, s.Password, s.Host)); err != nil {
				return fmt.Errorf("notify: smtp auth: %w", err)
			}
		}
	}
	if err := client.Mail(s.From); err != nil {
		return fmt.Errorf("notify: MAIL FROM: %w", err)
	}
	if err := client.Rcpt(to); err != nil {
		return fmt.Errorf("notify: RCPT TO: %w", err)
	}
	wc, err := client.Data()
	if err != nil {
		return fmt.Errorf("notify: DATA: %w", err)
	}
	if _, err := wc.Write(message); err != nil {
		wc.Close()
		return fmt.Errorf("notify: write body: %w", err)
	}
	if err := wc.Close(); err != nil {
		return fmt.Errorf("notify: finish body: %w", err)
	}
	return client.Quit()
}

// buildMessage renders a minimal RFC 5322 message.
//
// With an empty htmlPart this is the text/plain message the product has always sent. With one it
// is multipart/alternative, text first: the order tells a client which part is the fallback, and
// a client that cannot show HTML shows the same words it would have received anyway.
//
// Non-ASCII content is base64 encoded with an explicit utf-8 charset rather than sent raw. A
// company name with an accent in it is normal, and the previous us-ascii/raw combination
// delivered that as mojibake — the branded shell is exactly where a tenant's own name appears,
// so it is exactly where this could not be left alone.
func buildMessage(from, to, subject, body, htmlPart string) []byte {
	var b strings.Builder
	b.WriteString("From: " + from + "\r\n")
	b.WriteString("To: " + to + "\r\n")
	b.WriteString("Subject: " + encodeHeaderText(subject) + "\r\n")
	b.WriteString("Date: " + time.Now().UTC().Format(time.RFC1123Z) + "\r\n")
	b.WriteString("MIME-Version: 1.0\r\n")

	if strings.TrimSpace(htmlPart) == "" {
		b.WriteString(encodeTextPart("text/plain", body))
		b.WriteString("\r\n")
		return []byte(b.String())
	}

	boundary := messageBoundary()
	b.WriteString("Content-Type: multipart/alternative; boundary=\"" + boundary + "\"\r\n")
	b.WriteString("\r\n")
	// The preamble is for a client that understands neither part; it says what the message is
	// rather than showing it the MIME plumbing.
	b.WriteString("This message is a transaction notice from the billing panel.\r\n")
	b.WriteString("--" + boundary + "\r\n")
	b.WriteString(encodeTextPart("text/plain", body))
	b.WriteString("\r\n--" + boundary + "\r\n")
	b.WriteString(encodeTextPart("text/html", htmlPart))
	b.WriteString("\r\n--" + boundary + "--\r\n")
	return []byte(b.String())
}

// encodeTextPart renders one MIME part, headers included.
//
// ASCII content is sent as-is so the plain-text part of a mail is byte-identical to what this
// product sent before branding existed. Anything else is base64 with a utf-8 charset, because
// raw non-ASCII under `charset=us-ascii` is the mojibake bug.
func encodeTextPart(contentType, content string) string {
	normalised := strings.ReplaceAll(strings.ReplaceAll(content, "\r\n", "\n"), "\n", "\r\n")
	if isASCII(normalised) {
		return "Content-Type: " + contentType + "; charset=us-ascii\r\n\r\n" + normalised
	}
	return "Content-Type: " + contentType + "; charset=utf-8\r\n" +
		"Content-Transfer-Encoding: base64\r\n\r\n" +
		base64Lines(normalised)
}

// isASCII reports whether every byte is 7-bit.
func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] > 0x7f {
			return false
		}
	}
	return true
}

// base64Lines encodes base64 in 76-character lines, which is the MIME maximum.
func base64Lines(s string) string {
	encoded := base64.StdEncoding.EncodeToString([]byte(s))
	var b strings.Builder
	for len(encoded) > 76 {
		b.WriteString(encoded[:76])
		b.WriteString("\r\n")
		encoded = encoded[76:]
	}
	b.WriteString(encoded)
	return b.String()
}

// messageBoundary returns a boundary that cannot occur in the content.
//
// It is random rather than derived from the body: a boundary that appears in an operator's
// footer text would split the message into a part the client cannot parse, and the failure would
// look like a broken mail client rather than a broken template.
func messageBoundary() string {
	var raw [12]byte
	if _, err := rand.Read(raw[:]); err != nil {
		// A boundary that is merely unlikely beats failing the send: the alternative is a
		// password reset that never leaves the panel.
		return "catalyst-billing-alt-" + time.Now().UTC().Format("20060102150405")
	}
	return "catalyst-billing-alt-" + hex.EncodeToString(raw[:])
}

// encodeHeaderText makes a header value safe and, when it contains non-ASCII, encodes it.
//
// A newline in a header is how one message becomes two, so control characters are stripped
// first and always. RFC 2047 encoded-words are used only for a value that needs them, so an
// ASCII subject stays readable in the raw message.
func encodeHeaderText(s string) string {
	s = sanitiseHeader(s)
	if isASCII(s) {
		return s
	}
	return "=?utf-8?b?" + base64.StdEncoding.EncodeToString([]byte(s)) + "?="
}

// sanitiseHeader removes characters that would break out of the header.
func sanitiseHeader(s string) string {
	s = strings.ReplaceAll(s, "\r", " ")
	return strings.ReplaceAll(s, "\n", " ")
}
