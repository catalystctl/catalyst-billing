package main

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Account names.
//
// cPanel's rules for an account username are narrow and non-negotiable: lower
// case, letters and digits only, it must start with a letter, and it is at most
// 16 characters. WHM answers a violation with metadata.result 0 and a sentence
// about the username, which is a useless thing to put in a billing queue: the core
// would retry it forever. So every name is validated here, before a request is
// sent, and the message says what is wrong with it.

// maxUsernameLen is cPanel's limit on an account username.
const maxUsernameLen = 16

var usernamePattern = regexp.MustCompile(`^[a-z][a-z0-9]{0,15}$`)

// reservedUsernames are names cPanel refuses or that already belong to something
// on the server (system users and cPanel's own service accounts). Asking WHM about
// them produces a refusal on the merits, so they are refused locally instead.
var reservedUsernames = map[string]bool{
	"root": true, "bin": true, "daemon": true, "adm": true, "lp": true,
	"sync": true, "shutdown": true, "halt": true, "mail": true, "news": true,
	"uucp": true, "operator": true, "games": true, "gopher": true, "ftp": true,
	"nobody": true, "mysql": true, "postgres": true, "apache": true, "httpd": true,
	"web": true, "www": true, "cpanel": true, "whm": true, "cphulkd": true,
	"exim": true, "dovecot": true, "cpbackup": true, "named": true, "system": true,
	"admin": true, "administrator": true, "test": true, "guest": true,
}

// usernameForKey derives the account name for an operation key.
//
// This is the whole answer to "how does a lookup survive a restart when the
// provider has no external_id field". The name is a pure function of the key, so
// the adapter can recompute it from the key alone — no local state, no side
// channel, nothing to lose. "c" + base36 of the first 64 bits of the SHA-256 is
// 14 characters at most, starts with a letter by construction, and is lower case
// and alphanumeric throughout.
//
// 64 bits of hash is not a security boundary here: an operation key that collides
// with another would have to be *chosen* to collide by someone who can already
// write the core's operation keys, and the failure mode is that createacct refuses
// the second account because the username is taken.
func usernameForKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	n := binary.BigEndian.Uint64(sum[:8])
	return "c" + strconv.FormatUint(n, 36)
}

// validateUsername checks a name against cPanel's rules and returns a message that
// names the problem, or nil.
func validateUsername(name string) error {
	switch {
	case name == "":
		return fmt.Errorf("a cPanel username is required")
	case len(name) > maxUsernameLen:
		return fmt.Errorf("%q is not a valid cPanel username: it is %d characters and cPanel allows at most %d",
			name, len(name), maxUsernameLen)
	case !usernamePattern.MatchString(name):
		return fmt.Errorf("%q is not a valid cPanel username: cPanel allows lower-case letters and digits only, starting with a letter (so no upper case, spaces, dots, hyphens or underscores)", name)
	case reservedUsernames[name]:
		return fmt.Errorf("%q is not a valid cPanel username: the name belongs to a system or cPanel service account", name)
	}
	return nil
}

// pinnedUsername reports the operator's requested name, if any.
//
// NOT PTERODACTYL-SHAPED: on Pterodactyl the customer-visible name is a free-text
// label and the identifier is separate, so nothing is lost by letting a product
// config option choose it. On cPanel the username *is* the identifier and WHM
// offers no second field to key on, so a name the adapter cannot recompute from
// the operation key would make provisioner.lookup non-authoritative — it could not
// find an account this process did not create, which is exactly the duplicate this
// protocol exists to prevent. The request is therefore refused with that
// explanation rather than accepted and quietly made unsafe.
func pinnedUsername(metadata map[string]string) (string, error) {
	raw, ok := metadata["cpanel_username"]
	if !ok {
		return "", nil
	}
	name := strings.TrimSpace(raw)
	if name == "" {
		return "", fmt.Errorf("metadata[cpanel_username] is empty; remove it to let the adapter derive the account name from the operation key")
	}
	if err := validateUsername(name); err != nil {
		return "", err
	}
	return name, nil
}

// accountName resolves the account name for one create: derived from the operation
// key, and refused if the request asked for a name that is not that.
func accountName(operationKey string, metadata map[string]string) (string, error) {
	derived := usernameForKey(operationKey)
	pinned, err := pinnedUsername(metadata)
	if err != nil {
		return "", err
	}
	if pinned != "" && pinned != derived {
		return "", fmt.Errorf("metadata[cpanel_username]=%q is not usable: cPanel has no external id field, so the account name must be derived from the operation key (%q) or provisioner.lookup could not find the account after a restart and would provision a duplicate",
			pinned, derived)
	}
	if err := validateUsername(derived); err != nil {
		// Unreachable by construction; kept so a future change to the derivation
		// cannot silently send an invalid name to WHM.
		return "", err
	}
	return derived, nil
}

// domainFor picks the hosted domain for a new account.
//
// A cPanel account is a domain plus a user; the protocol has no field for it, so
// it arrives as metadata from the product's config options. A missing domain is
// refused locally, because WHM's answer ("(XID xxxxx) The domain ... is not a valid
// domain") is a refusal the core would retry rather than a message an operator can
// act on.
func domainFor(metadata map[string]string) (string, error) {
	raw := firstNonEmpty(metadata["cpanel_domain"], metadata["domain"])
	if raw == "" {
		return "", fmt.Errorf("a cPanel account needs a domain; set metadata[cpanel_domain] from the product's config options")
	}
	domain := strings.ToLower(strings.TrimSpace(raw))
	switch {
	case strings.ContainsAny(domain, " /\\") || strings.Contains(domain, "://"):
		return "", fmt.Errorf("%q is not a domain: give a bare hostname such as example.com, without a scheme or a path", raw)
	case !strings.Contains(domain, "."):
		return "", fmt.Errorf("%q is not a domain: cPanel needs a fully qualified hostname such as example.com", raw)
	case len(domain) > 253:
		return "", fmt.Errorf("%q is not a domain: it is longer than 253 characters", raw)
	}
	return domain, nil
}

func contactEmailFor(metadata map[string]string) string {
	return strings.TrimSpace(firstNonEmpty(metadata["cpanel_contact_email"], metadata["email"]))
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
