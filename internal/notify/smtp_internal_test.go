package notify

// The MIME builder, tested from inside the package because that is where it is reachable.
//
// A malformed multipart message is the worst kind of bug here: the send *succeeds*, the delivery
// row says 'sent', and the customer receives either a blank mail or a wall of MIME headers. Nothing
// in the product would ever tell the operator. So the shape is asserted directly: both parts are
// present, in the order that makes text the fallback, each decodes to the text that was handed in,
// and the plain-text message is unchanged from what the product sent before branding existed.

import (
	"encoding/base64"
	"strings"
	"testing"
)

// splitParts is a deliberately small multipart reader: this test knows the exact shape the builder
// writes, and a full MIME parser would be more code than the thing under test.
func splitParts(t *testing.T, message, boundary string) map[string]string {
	t.Helper()
	parts := map[string]string{}
	for _, chunk := range strings.Split(message, "--"+boundary) {
		if strings.TrimSpace(chunk) == "" || strings.HasPrefix(strings.TrimSpace(chunk), "--") {
			continue
		}
		headerEnd := strings.Index(chunk, "\r\n\r\n")
		if headerEnd < 0 {
			t.Fatalf("a part has no header/body separator: %q", chunk)
		}
		headers, body := chunk[:headerEnd], chunk[headerEnd+4:]
		contentType := ""
		for _, line := range strings.Split(headers, "\r\n") {
			if strings.HasPrefix(strings.ToLower(line), "content-type:") {
				contentType = strings.TrimSpace(strings.SplitN(line, ":", 2)[1])
			}
		}
		if contentType == "" {
			t.Fatalf("a part has no Content-Type: %q", headers)
		}
		body = strings.TrimSuffix(body, "\r\n")
		if strings.Contains(headers, "base64") {
			decoded, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(body, "\r\n", ""))
			if err != nil {
				t.Fatalf("a base64 part does not decode: %v", err)
			}
			body = string(decoded)
		}
		parts[contentType] = body
	}
	return parts
}

// A plain message is the shape this product sent before branding existed.
func TestBuildMessageWithoutHTMLIsPlainText(t *testing.T) {
	message := string(buildMessage("billing@example.test", "ada@example.test",
		"Invoice INV-1", "Please pay.", ""))
	if !strings.Contains(message, "Content-Type: text/plain; charset=us-ascii\r\n") {
		t.Fatalf("a plain message is not text/plain: %q", message)
	}
	if strings.Contains(message, "multipart") {
		t.Fatalf("a plain message is multipart: %q", message)
	}
	if !strings.HasSuffix(message, "\r\nPlease pay.\r\n") {
		t.Fatalf("the body was altered: %q", message)
	}
}

// A branded message is multipart/alternative, text first, and each part decodes to its input.
func TestBuildMessageWithHTMLCarriesBothParts(t *testing.T) {
	text := "Pay here: https://panel.example.test/portal"
	htmlPart := "<!doctype html><p>Pay here: <a href=\"https://panel.example.test/portal\">link</a></p>"
	message := string(buildMessage("billing@example.test", "ada@example.test",
		"Invoice INV-1", text, htmlPart))

	boundaryAt := strings.Index(message, `boundary="`)
	if boundaryAt < 0 {
		t.Fatalf("no multipart boundary: %q", message)
	}
	rest := message[boundaryAt+len(`boundary="`):]
	boundary := rest[:strings.Index(rest, `"`)]

	// The preamble is optional; what matters is the header and the parts.
	if !strings.Contains(message, "Content-Type: multipart/alternative;") {
		t.Fatalf("not multipart/alternative: %q", message)
	}

	parts := splitParts(t, message, boundary)
	if got, ok := parts["text/plain; charset=us-ascii"]; !ok {
		t.Fatalf("no text/plain part: %v", parts)
	} else if got != text {
		t.Fatalf("the text part = %q, want %q", got, text)
	}
	if got, ok := parts["text/html; charset=us-ascii"]; !ok {
		t.Fatalf("no text/html part: %v", parts)
	} else if got != htmlPart {
		t.Fatalf("the HTML part = %q, want %q", got, htmlPart)
	}

	// Text comes first: that is the part a client falls back to when it cannot render HTML.
	if strings.Index(message, "text/plain") > strings.Index(message, "text/html") {
		t.Fatal("the HTML part is before the text part")
	}
}

// Non-ASCII content is base64 with an explicit utf-8 charset, not raw bytes under us-ascii.
func TestBuildMessageEncodesNonASCII(t *testing.T) {
	message := string(buildMessage("billing@example.test", "ada@example.test",
		"Café invoice", "Grüße", ""))
	if !strings.Contains(message, "Content-Transfer-Encoding: base64") {
		t.Fatalf("non-ASCII body was not encoded: %q", message)
	}
	if !strings.Contains(message, "charset=utf-8") {
		t.Fatalf("non-ASCII body claims the wrong charset: %q", message)
	}
	// The subject becomes an encoded word, because a raw non-ASCII header is not valid 5322.
	if !strings.Contains(message, "Subject: =?utf-8?b?") {
		t.Fatalf("non-ASCII subject was not encoded: %q", message)
	}
	// And it round-trips: the encoded word decodes to the subject that was handed in.
	at := strings.Index(message, "Subject: =?utf-8?b?")
	enc := message[at+len("Subject: =?utf-8?b?"):]
	enc = enc[:strings.Index(enc, "?=")]
	decoded, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		t.Fatalf("subject encoded word does not decode: %v", err)
	}
	if string(decoded) != "Café invoice" {
		t.Fatalf("subject decoded to %q", decoded)
	}
}

// A newline in a subject cannot break out of the header.
func TestBuildMessageStripsHeaderBreaks(t *testing.T) {
	message := string(buildMessage("billing@example.test", "ada@example.test",
		"Invoice\r\nBcc: evil@example.test", "body", ""))
	if strings.Contains(message, "\r\nBcc:") {
		t.Fatalf("the subject injected a header: %q", message)
	}
}

// The boundary is random, so an operator's footer text cannot contain it.
func TestMessageBoundaryIsRandom(t *testing.T) {
	first := messageBoundary()
	second := messageBoundary()
	if first == second {
		t.Fatal("two messages used the same boundary")
	}
	if !strings.HasPrefix(first, "catalyst-billing-alt-") {
		t.Fatalf("boundary = %q", first)
	}
}
