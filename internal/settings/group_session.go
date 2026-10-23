package settings

// Session binding (IDN-16).
//
// Paymenter can lock a session to the address and the user agent it was signed in from, per
// class of user ("admin", "client", "both"). Catalyst already records both values on the
// session row for the operator's device list but never validated them, and it had no
// security group to turn the check on with.
//
// This is deliberately a setting and not a fixed behaviour. Binding a session to an IP is a
// genuine control — a stolen cookie is useless from another network — and a genuine
// usability problem: a mobile handset changes address when it switches between wifi and
// cellular, and a browser updates its user agent without asking. An installation that turns
// this on and finds itself signing people out hourly will turn it off again, so the operator
// chooses the mode *and* which sessions it applies to, and the default applies to nobody.
//
// Two keys rather than one, because a common deployment wants the strict rule for operators
// (who have one machine and access to money) and no rule for customers (who have three
// devices and no support number):
//
//	session.binding        off | user_agent | ip_and_user_agent
//	session.binding_scope  all | staff | customers
//
// The mode a session was created under is recorded on the session itself, not read back from
// these keys on every request. That is what keeps a settings change from retroactively
// re-judging sessions that are already open; `internal/authn/sessions.go` enforces what the
// row recorded.

import (
	"context"
	"strings"
)

func init() {
	Register(
		// The mode. `off` is the default because a control that signs people out on
		// upgrade is a control the operator disables before the first support call.
		Definition{
			Group: "session", Key: "binding", Kind: KindString, Default: "off",
			Choices: []string{BindOff, BindUserAgent, BindIPAndUserAgent},
		},
		// Which sessions the mode applies to. `all` is the default so that turning the
		// mode on does something visible; an installation that wants it only for staff
		// narrows it here rather than running two policies.
		Definition{
			Group: "session", Key: "binding_scope", Kind: KindString, Default: "all",
			Choices: []string{BindScopeAll, BindScopeStaff, BindScopeCustomers},
		},
	)
}

// The closed sets the two keys accept. They are exported so the authentication path can
// compare against the same strings the settings page offers, instead of repeating literals
// in two packages that would then have to be kept in step by hand.
const (
	// BindOff validates nothing: the recorded device is information, not a constraint.
	BindOff = "off"
	// BindUserAgent compares the browser's user agent. It survives a network change and
	// does not survive a browser update, which is the usual trade an operator accepts.
	BindUserAgent = "user_agent"
	// BindIPAndUserAgent compares both. It is the strict mode: an address change ends the
	// session's authority even though the cookie is still valid.
	BindIPAndUserAgent = "ip_and_user_agent"

	// BindScopeAll applies the mode to every session.
	BindScopeAll = "all"
	// BindScopeStaff applies it only to accounts holding a staff role.
	BindScopeStaff = "staff"
	// BindScopeCustomers applies it only to customer-only accounts.
	BindScopeCustomers = "customers"
)

// SessionBinding reads the binding policy (IDN-16).
//
// Both values fall back to their declarations independently: an operator who sets the mode
// and leaves the scope alone gets `all`, and a hand-edited value outside the closed set is
// treated as `off` rather than trusted. That direction matters — an unparseable value must
// not become a stricter rule than the one written, because a typo would then sign out an
// installation.
func SessionBinding(ctx context.Context, q RowQuerier) (mode, scope string) {
	return choiceOr(ctx, q, "session", "binding", BindOff),
		choiceOr(ctx, q, "session", "binding_scope", BindScopeAll)
}

// choiceOr reads one closed-set setting and falls back to the declared default.
func choiceOr(ctx context.Context, q RowQuerier, group, key, fallback string) string {
	def, ok := Lookup(group, key)
	if ok {
		fallback = def.Default
	}
	raw, ok := storedValue(ctx, q, group, key)
	if !ok {
		return fallback
	}
	raw = strings.TrimSpace(raw)
	if !allowedChoice(group, key, raw) {
		return fallback
	}
	return raw
}

// allowedChoice reports whether value is one of the definition's declared choices.
func allowedChoice(group, key, value string) bool {
	def, ok := Lookup(group, key)
	if !ok {
		return false
	}
	for _, c := range def.Choices {
		if c == value {
			return true
		}
	}
	return false
}
