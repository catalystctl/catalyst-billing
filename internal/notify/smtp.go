package notify

import (
	"context"
	"crypto/tls"
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

// Send implements Sender.
func (s SMTPSender) Send(ctx context.Context, to, subject, body string) error {
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
	if _, err := wc.Write(buildMessage(s.From, to, subject, body)); err != nil {
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
// The body is plain text and is not encoded: the templates in this package are ASCII by
// construction. A message that needs non-ASCII is a message that needs a MIME encoder,
// and pretending otherwise would produce mojibake.
func buildMessage(from, to, subject, body string) []byte {
	var b strings.Builder
	b.WriteString("From: " + from + "\r\n")
	b.WriteString("To: " + to + "\r\n")
	b.WriteString("Subject: " + sanitiseHeader(subject) + "\r\n")
	b.WriteString("Date: " + time.Now().UTC().Format(time.RFC1123Z) + "\r\n")
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=us-ascii\r\n")
	b.WriteString("\r\n")
	// Normalise line endings: a bare LF is not a valid line ending in SMTP data.
	b.WriteString(strings.ReplaceAll(strings.ReplaceAll(body, "\r\n", "\n"), "\n", "\r\n"))
	b.WriteString("\r\n")
	return []byte(b.String())
}

// sanitiseHeader removes characters that would break out of the header.
func sanitiseHeader(s string) string {
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	return s
}
