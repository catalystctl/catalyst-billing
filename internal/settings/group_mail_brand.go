// The mail-branding settings group.
//
// The brand overlay already reaches the panel chrome and the invoice PDF; mail was the surface
// it did not reach, so a customer could receive a branded invoice and a plain-text email from
// the same shop. These keys are the mail half of that overlay, and they live in their own file
// (registered from an init) for the same reason the storefront group does: the definitions table
// is a merge hotspot, and an adjacency conflict resolved by hand is how a setting disappears.
//
// Four decisions are deliberate:
//
//   - **The HTML alternative is on by default, the plain-text part is always present.** A
//     message whose only part is HTML is a message a text-only client renders as nothing, so
//     the renderer emits both and the plain part is byte-identical to what this product sent
//     before the feature existed. `brand_html = false` therefore turns off the decorative part,
//     never the message.
//
//   - **The header and footer default to empty, not to text.** Empty means "use the company
//     name", so an installation that never opens this screen still gets a correct, branded
//     shell — and an operator who clears a footer gets the default back, not a footer with a
//     company name hard-coded in this repository.
//
//   - **The accent is optional.** It falls back to `branding.accent_light`, so one brand means
//     one colour rather than two that drift. The override exists because an email is read on
//     white far more often than a panel is, and the light-mode accent can be too pale for a
//     narrow rule in a mail client.
//
//   - **The logo is a URL, not an upload.** A mail client fetches the image from the recipient's
//     machine, so an id served from the panel's own host would render broken for a customer
//     whose mailbox cannot reach the panel. A URL also keeps this group free of the media store
//     the panel logo uses.
package settings

import (
	"fmt"
	"net/url"
	"strings"
)

func init() {
	Register(
		Definition{
			Group: "mail", Key: "brand_html", Kind: KindBool, Default: "true",
		},
		Definition{
			Group: "mail", Key: "brand_header", Kind: KindString, Default: "",
			// Bounded by the same rule as the panel name: a header line longer than this wraps
			// every mail the shop sends, which is a worse outcome than being told it is long.
			Check: checkBrandName,
		},
		Definition{
			Group: "mail", Key: "brand_footer", Kind: KindString, Default: "",
		},
		Definition{
			Group: "mail", Key: "brand_accent", Kind: KindString, Default: "",
			Check: checkHexColourOrEmpty,
		},
		Definition{
			Group: "mail", Key: "brand_logo_url", Kind: KindString, Default: "",
			Check: checkHTTPSOrEmpty,
		},
	)
}

// checkHTTPSOrEmpty refuses a logo reference that is not an absolute http(s) URL.
//
// A relative path cannot be resolved by a mail client — the message is not being read inside the
// panel — and any other scheme (`javascript:`, `data:`) is either inert or a URL a mail client
// may refuse to render at all. Refusing at the settings boundary means the render step never has
// to sanitise a value it should not have stored.
func checkHTTPSOrEmpty(d Definition, value string) error {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return nil
	}
	u, err := url.Parse(trimmed)
	if err != nil {
		return fmt.Errorf("%w: %s must be an absolute http(s) URL, or empty", ErrInvalidValue, d.Name())
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("%w: %s must be an absolute http(s) URL, or empty", ErrInvalidValue, d.Name())
	}
	if u.Host == "" {
		return fmt.Errorf("%w: %s must name a host", ErrInvalidValue, d.Name())
	}
	return nil
}
