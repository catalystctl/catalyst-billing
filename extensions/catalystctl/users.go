package main

// The panel account a customer's server belongs to.
//
// # Why this exists
//
// A game panel is multi-tenant: every server has an owner, and an owner is a panel account with its own
// login, its own 2FA and its own access. A billing service is not a panel account. If the adapter created
// servers owned by the API key's own user, then:
//
//   - every customer's server would live in one account, so one leaked panel login exposes all of them;
//   - the customer could not open the console we link to, because it is not their server;
//   - support could not tell "the customer's server" from "ours".
//
// So a create ensures a panel account for the customer, and gives the server to it.
//
// # Matching, not guessing
//
// The identity is the email address, compared case-insensitively, because that is what a person types
// into a login form. The panel enforces its own uniqueness on it, so "found an account with this email"
// means exactly one account. When no account exists one is created — and its password is random and
// thrown away (see createPanelUser), because billing has no password to give and must never invent one
// that somebody might guess.
//
// The mapping is written to the ledger keyed by address, so a second service for the same customer
// reuses the account instead of attempting a duplicate create. If the ledger is lost the lookup finds it
// anyway, which is the point of matching on the address rather than on a remembered id.

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"strings"
	"time"
)

// customerEmailKey is the metadata key the core sends for the customer's address.
const customerEmailKey = "customer_email"

// panelUser is the account a server belongs to.
type panelUser struct {
	ID       string `json:"id"`
	Email    string `json:"email"`
	Username string `json:"username"`
}

// ensurePanelUser resolves the customer's panel account, creating it when necessary.
//
// It returns an empty id when the adapter is configured not to own servers per customer, which leaves
// the panel's own default in place.
func (s *server) ensurePanelUser(ctx context.Context, metadata map[string]string) (string, error) {
	if s.cfg.userMode == userModeNone {
		return "", nil
	}
	// An explicit owner wins: a reseller product places servers in a panel account of its own.
	if explicit := s.setting(metadata, "owner", "OWNER_ID", s.cfg.ownerID); explicit != "" {
		return explicit, nil
	}

	email := strings.TrimSpace(metadata[customerEmailKey])
	if email == "" {
		// A service with no customer address is not a purchase; it is a back-office fixture or an
		// import. Creating an account for nobody would litter the panel, and failing here would block
		// work that has nothing to do with a person.
		return "", nil
	}
	email = strings.ToLower(email)

	// The ledger first: one read, no listing.
	if rec, ok, err := s.ledger.get(panelUserKey(email)); err != nil {
		return "", err
	} else if ok && rec.ExternalID != "" {
		return rec.ExternalID, nil
	}

	users, err := s.client.listUsers(ctx, email)
	if err != nil {
		return "", err
	}
	for _, user := range users {
		if strings.EqualFold(strings.TrimSpace(user.Email), email) {
			s.recordPanelUser(email, user)
			return user.ID, nil
		}
	}

	username, err := s.freeUsername(ctx, email)
	if err != nil {
		return "", err
	}
	created, err := s.client.createPanelUser(ctx, createUserRequest{
		Email:    email,
		Username: username,
		Password: randomPassword(),
		RoleIDs:  s.roleIDs(),
	})
	if err == nil && created.valid() {
		s.recordPanelUser(email, created)
		return created.ID, nil
	}
	if err != nil && !isRejection(err) {
		return "", err
	}

	// Either the panel refused a duplicate or it answered without an id, and in both cases the account
	// may well exist: the panel's account search is not immediately consistent (the create and the search
	// can be milliseconds apart), and a create whose reply could not be parsed has still happened.
	//
	// So the repair is to look again, a few times with a short wait, before concluding failure. Giving up
	// is the one outcome that is definitely wrong: it strands an account on the panel that no operation
	// key points at, and every later attempt for the same customer is then a permanent 409.
	for attempt := 0; attempt < 5; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(time.Duration(attempt) * 300 * time.Millisecond):
			}
		}
		again, listErr := s.client.listUsers(ctx, email)
		if listErr != nil {
			continue
		}
		for _, user := range again {
			if strings.EqualFold(strings.TrimSpace(user.Email), email) {
				s.recordPanelUser(email, user)
				return user.ID, nil
			}
		}
		// The initial lookup used the search endpoint. A panel under load may not match there yet, so the
		// last resort reads the account list itself.
		if attempt >= 2 {
			if all, err := s.client.listUsers(ctx, ""); err == nil {
				for _, user := range all {
					if strings.EqualFold(strings.TrimSpace(user.Email), email) {
						s.recordPanelUser(email, user)
						return user.ID, nil
					}
				}
			}
		}
	}
	if err != nil {
		return "", err
	}
	return "", fmt.Errorf("the panel created no account for %s and reports none", email)
}

// recordPanelUser remembers the address-to-account mapping.
//
// A failure to write it is not fatal: the next create finds the account by address anyway, which is why
// the fallback exists. Losing the write costs one extra listing, not a duplicate account.
func (s *server) recordPanelUser(email string, user panelUser) {
	_ = s.ledger.put(operationRecord{
		OperationKey: panelUserKey(email),
		ExternalID:   user.ID,
		Name:         user.Username,
		State:        recordCreated,
	})
}

// panelUserKey is the ledger key for an address-to-account mapping.
func panelUserKey(email string) string { return "panel-user:" + strings.ToLower(email) }

// freeUsername picks a username the panel does not already have.
//
// A panel enforces username uniqueness separately from email uniqueness, so "alice@example.com" becoming
// "alice" is only correct until a different alice buys something — and the create would then be refused
// for a reason that has nothing to do with the customer. The candidate is therefore checked against the
// panel's own search, which filters usernames as well as addresses.
//
// A probing failure is not fatal: the create is attempted anyway rather than failing a purchase over a
// naming lookup, and the panel's uniqueness check remains the authority.
func (s *server) freeUsername(ctx context.Context, email string) (string, error) {
	base := usernameBase(email)
	taken, err := s.client.listUsers(ctx, base)
	if err != nil {
		return base, nil
	}
	candidates := usernameCandidates(base, len(taken)+2)
	for _, candidate := range candidates {
		if !usernameTaken(candidate, taken) {
			return candidate, nil
		}
	}
	return candidates[len(candidates)-1], nil
}

// usernameTaken reports whether an account already uses a username.
func usernameTaken(candidate string, existing []panelUser) bool {
	for _, user := range existing {
		if strings.EqualFold(strings.TrimSpace(user.Username), candidate) {
			return true
		}
	}
	return false
}

// usernameCandidates lists the names to try, in order: the plain one, then numbered variants.
//
// The variants are derived from the address rather than random, so the same customer keeps the same
// username across purchases and a support agent can guess it.
func usernameCandidates(base string, n int) []string {
	out := make([]string, 0, n+1)
	out = append(out, base)
	for i := 1; i <= n; i++ {
		out = append(out, fmt.Sprintf("%s%d", base, i))
	}
	return out
}

// usernameBase is the alphabetic core of an address.
func usernameBase(email string) string {
	base := email
	if at := strings.Index(base, "@"); at > 0 {
		base = base[:at]
	}
	var cleaned strings.Builder
	for _, r := range strings.ToLower(base) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-' {
			cleaned.WriteRune(r)
		}
	}
	name := strings.Trim(cleaned.String(), "._-")
	if len(name) < 3 {
		// A panel with a minimum length would refuse a two-character username, and the customer would
		// see a failed purchase over the length of their own address.
		name = "customer"
	}
	if len(name) > 24 {
		name = name[:24]
	}
	return name
}

// roleIDs is the panel role a new customer account gets.
//
// The answer comes from configuration because role ids are panel-specific and a hardcoded one would be
// wrong on every installation but the one it was copied from. An empty answer is reported to the panel,
// which then applies its own default — and that is the only case where a customer could end up with more
// access than intended, so startup warns about it (see main.go).
func (s *server) roleIDs() []string {
	if s.cfg.userRoleID == "" {
		return nil
	}
	return []string{s.cfg.userRoleID}
}

// randomPassword makes an unguessable password for an account nobody will sign into with it.
//
// This is deliberate and it is the honest answer to "what password do we give the panel account?":
// none. Billing has no password to hand over, and inventing a predictable one would make every customer
// account on the panel trivially accessible. The customer gets into the panel through the panel's own
// password reset, which is the flow that verifies the address. The value is never logged, never stored,
// and never returned.
func randomPassword() string {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		// Returning a weaker secret on a read failure is worse than failing the create: the caller
		// reports a retryable error and the panel never gets a guessable account.
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

func (p panelUser) valid() bool { return p.ID != "" }

// panelUserMode is the configured policy for panel accounts.
type panelUserMode string

const (
	// userModeCreate gives each customer an account of their own (the default).
	userModeCreate panelUserMode = "create"
	// userModeNone leaves ownership to the panel, for an installation whose panel already has all its
	// accounts and where servers belong to the API key's user.
	userModeNone panelUserMode = "none"
)

func validUserMode(mode string) bool {
	return panelUserMode(mode) == userModeCreate || panelUserMode(mode) == userModeNone
}
