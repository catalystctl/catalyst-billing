// Command vapidkeys prints a fresh VAPID key pair for web push.
//
// The values go into the panel's settings as `push.vapid_public_key` and `push.vapid_private_key`,
// with `push.enabled` set to true. Without them the portal hides the "enable notifications"
// control, because a browser can only subscribe with a public key and the server can only sign a
// push with the matching private key.
//
// It is a command rather than a settings screen on purpose. A key pair is generated once per
// installation, and an endpoint that mints a signing secret and returns it over HTTP is a
// credential leak with a login around it. This prints to a terminal, where the operator decides
// where it goes.
//
// The private half is a 32-byte scalar in base64url and the public half is the matching
// uncompressed P-256 point, which is what the settings parser and the browser both accept. Both
// are printed so nobody has to derive one from the other by hand: a mismatched pair is not a typo,
// it is every delivery failing with a 403 that reads like a configuration error — and is one.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/catalyst/billing/internal/notify/webpush"
)

func main() {
	subject := flag.String("subject", "mailto:no-reply@localhost",
		"the VAPID subject: a mailto: or https: URL identifying whoever sends the pushes")
	flag.Parse()

	public, private, err := webpush.GenerateKeyPair()
	if err != nil {
		fmt.Fprintln(os.Stderr, "vapidkeys:", err)
		os.Exit(1)
	}
	// Build a client from the pair before printing it. It is the same validation the server runs
	// when push is switched on, so a pair this command prints is a pair that will work, and a bad
	// subject is reported here rather than as a 403 from a push service later.
	if _, err := webpush.NewClient(webpush.Config{
		PublicKey: public, PrivateKey: private, Subject: *subject,
	}); err != nil {
		fmt.Fprintln(os.Stderr, "vapidkeys:", err)
		os.Exit(1)
	}

	fmt.Println("push.enabled            = true")
	fmt.Println("push.subject            = " + *subject)
	fmt.Println("push.vapid_public_key   = " + public)
	fmt.Println("push.vapid_private_key  = " + private)
}
