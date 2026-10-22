package tickets_test

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/catalyst/billing/internal/tickets"
)

// The IMAP poller is tested against a scripted TLS server rather than a mock of our own
// client: what can actually break is the protocol conversation — the literal, the tagged
// completion, the \Seen flag that must only be set after the reply commits — and a mock would
// assume all three are right.

// testCertificate generates a self-signed certificate for localhost, so the test exercises
// real certificate verification instead of an InsecureSkipVerify flag that production would
// then be tempted to copy.
func testCertificate(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "localhost"},
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	cert, err := tls.X509KeyPair(
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: mustMarshalKey(t, key)}))
	if err != nil {
		t.Fatalf("key pair: %v", err)
	}
	pool := x509.NewCertPool()
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse certificate: %v", err)
	}
	pool.AddCert(parsed)
	return cert, pool
}

// fakeIMAP is a scripted IMAP4rev1 server: it speaks the subset the poller uses and records
// what it was asked to do.
type fakeIMAP struct {
	listener net.Listener
	cert     tls.Certificate
	raw      []byte
	// headers is what a HEADER-only fetch returns.
	headers []byte
	uid     int

	mu     sync.Mutex
	seen   bool
	stores []string
}

func newFakeIMAP(t *testing.T, raw []byte, uid int) (*fakeIMAP, *x509.CertPool) {
	t.Helper()
	cert, pool := testCertificate(t)
	listener, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	headers := raw
	if idx := strings.Index(string(raw), "\r\n\r\n"); idx >= 0 {
		headers = raw[:idx+4]
	}
	server := &fakeIMAP{listener: listener, cert: cert, raw: raw, headers: headers, uid: uid}
	go server.serve()
	t.Cleanup(func() { listener.Close() })
	return server, pool
}

func (s *fakeIMAP) addr() string { return s.listener.Addr().String() }

func (s *fakeIMAP) markedSeen() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.seen
}

func (s *fakeIMAP) storeCommands() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.stores...)
}

func (s *fakeIMAP) serve() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		go s.handle(conn)
	}
}

func (s *fakeIMAP) handle(conn net.Conn) {
	defer conn.Close()
	reader := bufio.NewReader(conn)
	writer := bufio.NewWriter(conn)

	reply := func(text string) {
		writer.WriteString(text + "\r\n")
		writer.Flush()
	}
	reply("* OK [CAPABILITY IMAP4rev1] fake server ready")

	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, " ", 2)
		tag := parts[0]
		command := ""
		if len(parts) > 1 {
			command = parts[1]
		}
		upper := strings.ToUpper(command)

		switch {
		case strings.HasPrefix(upper, "LOGIN"):
			reply(tag + " OK LOGIN completed")
		case strings.HasPrefix(upper, "SELECT"):
			reply("* 1 EXISTS")
			reply(tag + " OK [READ-WRITE] SELECT completed")
		case strings.HasPrefix(upper, "UID SEARCH"):
			s.mu.Lock()
			seen := s.seen
			s.mu.Unlock()
			if seen {
				reply("* SEARCH")
			} else {
				reply(fmt.Sprintf("* SEARCH %d", s.uid))
			}
			reply(tag + " OK SEARCH completed")
		case strings.HasPrefix(upper, "UID FETCH"):
			switch {
			case strings.Contains(upper, "BODY.PEEK[HEADER]"):
				reply(fmt.Sprintf("* 1 FETCH (UID %d BODY[HEADER] {%d}", s.uid, len(s.headers)))
				writer.Write(s.headers)
				writer.WriteString(")")
				writer.Flush()
				reply("")
				reply(tag + " OK FETCH completed")
			case strings.Contains(upper, "BODY.PEEK[]"):
				reply(fmt.Sprintf("* 1 FETCH (UID %d BODY[]<0> {%d}", s.uid, len(s.raw)))
				writer.Write(s.raw)
				writer.WriteString(")")
				writer.Flush()
				reply("")
				reply(tag + " OK FETCH completed")
			default:
				reply(fmt.Sprintf("* 1 FETCH (UID %d RFC822.SIZE %d)", s.uid, len(s.raw)))
				reply(tag + " OK FETCH completed")
			}
		case strings.HasPrefix(upper, "UID STORE"):
			s.mu.Lock()
			s.seen = true
			s.stores = append(s.stores, command)
			s.mu.Unlock()
			reply(fmt.Sprintf("* 1 FETCH (UID %d FLAGS (\\Seen))", s.uid))
			reply(tag + " OK STORE completed")
		case strings.HasPrefix(upper, "LOGOUT"):
			reply("* BYE")
			reply(tag + " OK LOGOUT completed")
			return
		default:
			reply(tag + " BAD unsupported command")
		}
	}
}

// TestParseInboundMultipart checks the parser the poller relies on: headers, threading,
// quoted-printable text, and an attachment that is counted and refused rather than stored.
func TestParseInboundMultipart(t *testing.T) {
	raw := strings.Join([]string{
		`From: "Alice" <alice@example.test>`,
		`To: support+ticket-0f8fad5b-d9cb-469f-a165-70867728950e@panel.test`,
		`Subject: =?utf-8?Q?Re=3A_printer_on_fire?=`,
		`Message-ID: <m1@example.test>`,
		`In-Reply-To: <staff-1@panel.test>`,
		`References: <staff-1@panel.test> <m0@example.test>`,
		`Auto-Submitted: no`,
		`Content-Type: multipart/mixed; boundary="b1"`,
		``,
		`--b1`,
		`Content-Type: text/plain; charset=utf-8`,
		`Content-Transfer-Encoding: quoted-printable`,
		``,
		`yes, now it is worse=0Aand the printer is on fire`,
		`--b1`,
		`Content-Type: application/pdf`,
		`Content-Disposition: attachment; filename="invoice.pdf"`,
		`Content-Transfer-Encoding: base64`,
		``,
		`JVBERi0xLjQK`,
		`--b1--`,
		``,
	}, "\r\n")

	msg, err := tickets.ParseInbound([]byte(raw))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if msg.MessageID != "<m1@example.test>" {
		t.Fatalf("message id = %q", msg.MessageID)
	}
	if msg.Subject != "Re: printer on fire" {
		t.Fatalf("subject = %q, want the decoded header", msg.Subject)
	}
	if !strings.Contains(msg.Body, "now it is worse\nand the printer is on fire") {
		t.Fatalf("body = %q, want the quoted-printable text decoded", msg.Body)
	}
	if msg.AttachmentsDropped != 1 {
		t.Fatalf("attachments dropped = %d, want 1", msg.AttachmentsDropped)
	}
	if len(msg.References) != 2 || msg.InReplyTo != "<staff-1@panel.test>" {
		t.Fatalf("threading = %q / %v", msg.InReplyTo, msg.References)
	}
	if len(msg.To) != 1 || !strings.Contains(msg.To[0], "0f8fad5b-d9cb-469f-a165-70867728950e") {
		t.Fatalf("to = %v", msg.To)
	}
}

// The poller reads an unseen message, ingests it as a reply, and only then marks it seen.
func TestIMAPPollerIngestsAndMarksSeen(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	thread := f.open(t, f.alice, "piped from imap")
	f.addStaffReply(t, thread.Ticket.ID, "<staff-imap@panel.test>", "what is wrong")

	raw := strings.Join([]string{
		`From: alice@example.test`,
		`To: support@panel.test`,
		`Subject: Re: piped from imap`,
		`Message-ID: <imap-1@example.test>`,
		`In-Reply-To: <staff-imap@panel.test>`,
		`Content-Type: text/plain; charset=utf-8`,
		``,
		`it is still broken`,
		``,
	}, "\r\n")

	server, pool := newFakeIMAP(t, []byte(raw), 7)

	poller := tickets.NewPoller(f.pool, tickets.IMAPConfig{
		Host:     "localhost",
		Port:     993,
		Username: "support@panel.test",
		Password: "secret",
		TLS:      &tls.Config{RootCAs: pool, ServerName: "localhost", MinVersion: tls.VersionTLS12},
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, network, server.addr())
		},
	})

	result, err := poller.Poll(ctx)
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if result.Fetched != 1 || result.Attached != 1 {
		t.Fatalf("result = %+v, want one fetched and attached", result)
	}
	if !server.markedSeen() {
		t.Fatal("the message was not marked seen after it was ingested")
	}
	if len(server.storeCommands()) != 1 {
		t.Fatalf("store commands = %v, want one", server.storeCommands())
	}

	got, err := f.svc.ThreadForCustomer(ctx, f.alice, thread.Ticket.ID)
	if err != nil {
		t.Fatalf("read thread: %v", err)
	}
	found := false
	for _, m := range got.Messages {
		if strings.Contains(m.Body, "it is still broken") {
			found = true
		}
	}
	if !found {
		t.Fatalf("the polled reply is not in the thread: %+v", got.Messages)
	}

	// A second poll finds nothing: the flag was set, and a refetch would dedupe anyway.
	again, err := poller.Poll(ctx)
	if err != nil {
		t.Fatalf("second poll: %v", err)
	}
	if again.Fetched != 0 {
		t.Fatalf("second poll fetched %d, want 0", again.Fetched)
	}
}

// An oversized message is refused and recorded, not truncated into a reply.
func TestIMAPPollerRefusesAnOversizedMessage(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	raw := strings.Join([]string{
		`From: alice@example.test`,
		`Subject: enormous`,
		`Message-ID: <huge-1@example.test>`,
		`Content-Type: text/plain`,
		``,
		strings.Repeat("x", 400),
		``,
	}, "\r\n")

	server, pool := newFakeIMAP(t, []byte(raw), 9)

	poller := tickets.NewPoller(f.pool, tickets.IMAPConfig{
		Host:            "localhost",
		Username:        "support@panel.test",
		Password:        "secret",
		MaxMessageBytes: 100,
		TLS:             &tls.Config{RootCAs: pool, ServerName: "localhost", MinVersion: tls.VersionTLS12},
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, network, server.addr())
		},
	})

	result, err := poller.Poll(ctx)
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if result.TooLarge != 1 || result.Ignored != 1 {
		t.Fatalf("result = %+v, want one refused as too large", result)
	}
	rows, _, err := f.svc.ListInboundMail(ctx, tickets.MailLogFilter{Status: "ignored"}, 50, "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 || rows[0].Reason != tickets.ReasonTooLarge {
		t.Fatalf("rows = %+v, want one too_large refusal", rows)
	}
	if rows[0].MessageID != "huge-1@example.test" {
		t.Fatalf("logged message id = %q, want the header's", rows[0].MessageID)
	}
	if !server.markedSeen() {
		t.Fatal("a refused message must be marked seen, or it is refetched forever")
	}
}

// A plaintext port is refused rather than silently used.
func TestIMAPPollerRefusesPort143(t *testing.T) {
	f := newFixture(t)
	poller := tickets.NewPoller(f.pool, tickets.IMAPConfig{
		Host: "localhost", Port: 143, Username: "u", Password: "p",
	})
	if _, err := poller.Poll(context.Background()); err == nil {
		t.Fatal("port 143 was accepted; STARTTLS is not implemented, so it must be refused")
	}
}

// An installation with no mailbox is a no-op, not an error: the job runs unconditionally.
func TestIMAPPollerWithoutConfigurationDoesNothing(t *testing.T) {
	f := newFixture(t)
	result, err := tickets.NewPoller(f.pool, tickets.IMAPConfig{}).Poll(context.Background())
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if result.Fetched != 0 {
		t.Fatalf("result = %+v, want nothing", result)
	}
}

func mustMarshalKey(t *testing.T, key *ecdsa.PrivateKey) []byte {
	t.Helper()
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	return der
}
