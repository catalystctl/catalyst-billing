package httpapi

import (
	"errors"
	"net/http"
	"net/netip"
	"sort"
	"strings"

	"github.com/catalyst/billing/internal/authn"
)

// API-key IP allowlists (IDN-12).
//
// A key is a bearer credential that lives in a CI runner, a cron host or a reconciler. The
// useful control is not a shorter expiry — that breaks the integration — but a restriction
// on where it can be used from: a key minted for one machine should stop working when it is
// copied to another.
//
// The allowlist is stored as `cidr[]` on the key itself (migration 0023), because it is a
// property of the credential: a global policy would apply the same list to a browser session
// and to a robot, and the two have nothing in common.
//
// Empty means "no restriction", which is what every key minted before this feature has. That
// default is deliberate: a migration that silently narrowed every existing integration would
// break production on deploy.
//
// The address is taken from `clientIP`, which already honours trusted proxies and already
// refuses a forged X-Forwarded-For. Using the raw peer here would make the allowlist
// unenforceable behind a load balancer; using an unvalidated header would make it a
// suggestion.

// maxAPIKeyAllowlist bounds the list. A key with a thousand prefixes is a key whose operator
// meant "no restriction" and expressed it the hard way.
const maxAPIKeyAllowlist = 32

var (
	// errAllowlistInvalid is returned for an entry that is not a CIDR.
	errAllowlistInvalid = errors.New("not a CIDR")
)

// parseAllowlist validates and normalises submitted CIDRs.
//
// Host bits are masked off, so "10.0.0.7/24" is stored as "10.0.0.0/24" rather than as a
// value PostgreSQL would reject or silently reinterpret. Duplicates are dropped.
func parseAllowlist(entries []string) ([]string, error) {
	if len(entries) > maxAPIKeyAllowlist {
		return nil, errors.New("the allowlist is too long")
	}
	seen := map[string]struct{}{}
	out := make([]string, 0, len(entries))
	for _, raw := range entries {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		prefix, err := netip.ParsePrefix(raw)
		if err != nil {
			return nil, errors.New(errAllowlistInvalid.Error() + ": " + raw)
		}
		if prefix.Addr().Is4In6() {
			// A v4-mapped address compares unequal to the v4 form, so normalising here is
			// what makes "::ffff:10.0.0.0/104" and "10.0.0.0/8" the same rule.
			prefix = netip.PrefixFrom(prefix.Addr().Unmap(), prefix.Bits()-96)
		}
		normalised := prefix.Masked().String()
		if _, dup := seen[normalised]; dup {
			continue
		}
		seen[normalised] = struct{}{}
		out = append(out, normalised)
	}
	sort.Strings(out)
	return out, nil
}

// allowedFrom reports whether an address satisfies an allowlist.
//
// An empty list allows everything. A non-empty list requires a match: there is no implicit
// "and localhost", because an operator who forgot to add their own address should notice
// immediately rather than discover later that the restriction was partly decorative.
func allowedFrom(allowlist []string, addr netip.Addr) bool {
	if len(allowlist) == 0 {
		return true
	}
	if !addr.IsValid() {
		// No address, no match. This is the fail-closed direction: a request whose address
		// cannot be determined must not pass a location check.
		return false
	}
	addr = addr.Unmap()
	for _, raw := range allowlist {
		prefix, err := netip.ParsePrefix(raw)
		if err != nil {
			continue
		}
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

// apiKeyAddressAllowed is the pure decision, with no database access.
//
// It exists so the authentication path can enforce an allowlist it already has in hand. If
// authn.APIKey grows an IPAllowlist field (one extra column in its SELECT), auth.go calls
// this with no extra round trip; until then apiKeyAllowedFromRequest reads the column.
func apiKeyAddressAllowed(r *http.Request, clientIP string, allowlist []string) bool {
	if len(allowlist) == 0 {
		return true
	}
	addr, err := netip.ParseAddr(clientIP)
	if err != nil {
		// An address that cannot be parsed must not satisfy a location check.
		return false
	}
	return allowedFrom(allowlist, addr)
}

// apiKeyAllowedFromRequest applies a key's stored allowlist to a request.
//
// It is the function the authentication path calls. It returns true when the key may be used
// from this address, and false when it may not — the caller answers 401, not 403, because a
// key used from the wrong place is indistinguishable from a key that does not work there.
//
// An error is also "not allowed": a key whose allowlist cannot be read must not be treated
// as unrestricted, because that is precisely the case an attacker would try to induce.
func (s *Server) apiKeyAllowedFromRequest(r *http.Request, keyID string) bool {
	if s.pool == nil {
		return true
	}
	var allowlist []string
	if err := s.pool.QueryRow(r.Context(),
		`SELECT ip_allowlist::text[] FROM api_keys WHERE id = $1`, keyID).Scan(&allowlist); err != nil {
		s.cfg.Logger.ErrorContext(r.Context(),
			"could not read an api key's allowlist; refusing the key", "error", err)
		return false
	}
	return apiKeyAddressAllowed(r, s.clientIP(r), allowlist)
}

// RegisterRoutes wires the allowlist administration.
//
// Creation and listing stay in apikeys.go, which this feature may not edit: the create body
// is decoded with DisallowUnknownFields, so accepting `ip_allowlist` at creation needs a field
// there (reported to the integrator). Until that lands, an operator sets the allowlist on the
// key immediately after creating it, and the key is unrestricted only for the seconds between
// the two calls.
//
// ENFORCEMENT is a three-line change in internal/httpapi/auth.go's apiKeyIdentity, which is a
// shared file. It is reported, not applied:
//
//	key, err := s.auth.Service.AuthenticateAPIKey(r.Context(), presented)
//	if err != nil {
//		return APIKeyIdentity{}, authn.User{}, false
//	}
//	if !s.apiKeyAllowedFromRequest(r, key.ID) {          // <-- add these three lines
//		return APIKeyIdentity{}, authn.User{}, false      //     (fail-closed: a 401)
//	}
//
// A false result produces the existing 401 invalid_api_key, which is the correct answer: a
// key used from the wrong network must be indistinguishable from a key that does not exist.
func init() {
	RegisterRoutes("91-api-key-allowlist", registerAPIKeyAllowlist)
}

func registerAPIKeyAllowlist(r *router) {
	r.staffGet("/api/v1/staff/api-keys/{id}/ip-allowlist",
		authn.PermAPIKeyManage, r.s.handleAPIKeyAllowlistGet)
	r.staffMutation(http.MethodPut, "/api/v1/staff/api-keys/{id}/ip-allowlist",
		authn.PermAPIKeyManage, r.s.handleAPIKeyAllowlistSet)
}

func (s *Server) handleAPIKeyAllowlistGet(w http.ResponseWriter, r *http.Request) {
	if s.auth == nil || s.auth.Service == nil {
		writeProblem(w, http.StatusServiceUnavailable, "auth_not_configured", "authentication is not configured")
		return
	}
	user, _ := userFrom(r.Context())
	id := r.PathValue("id")
	if !validUUID(id) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such key")
		return
	}

	var allowlist []string
	err := s.pool.QueryRow(r.Context(), `
		SELECT ip_allowlist::text[] FROM api_keys WHERE id = $1 AND user_id = $2`,
		id, user.ID).Scan(&allowlist)
	if err != nil {
		// Someone else's key is indistinguishable from a nonexistent one.
		writeProblem(w, http.StatusNotFound, "not_found", "no such key")
		return
	}
	if allowlist == nil {
		allowlist = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":           id,
		"ip_allowlist": allowlist,
		"unrestricted": len(allowlist) == 0,
	})
}

func (s *Server) handleAPIKeyAllowlistSet(w http.ResponseWriter, r *http.Request) {
	if s.auth == nil || s.auth.Service == nil {
		writeProblem(w, http.StatusServiceUnavailable, "auth_not_configured", "authentication is not configured")
		return
	}
	user, _ := userFrom(r.Context())
	id := r.PathValue("id")
	if !validUUID(id) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such key")
		return
	}

	var req struct {
		// IPAllowlist is a list of CIDR strings. An empty list removes the restriction.
		IPAllowlist []string `json:"ip_allowlist"`
	}
	if !s.decodeJSON(w, r, &req, 8<<10) {
		return
	}
	allowlist, err := parseAllowlist(req.IPAllowlist)
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_allowlist", err.Error())
		return
	}

	// Ownership is in the WHERE clause: another user's key affects zero rows, which is
	// reported exactly like an unknown id.
	tag, err := s.pool.Exec(r.Context(), `
		UPDATE api_keys SET ip_allowlist = $3::cidr[]
		WHERE id = $1 AND user_id = $2 AND revoked_at IS NULL`,
		id, user.ID, allowlist)
	if err != nil {
		s.cfg.Logger.ErrorContext(r.Context(), "api key allowlist update failed", "error", err)
		writeProblem(w, http.StatusInternalServerError, "internal_error",
			"the request could not be completed")
		return
	}
	if tag.RowsAffected() == 0 {
		writeProblem(w, http.StatusNotFound, "not_found", "no such key")
		return
	}

	s.auditKeyEvent(r, "user.api_key_allowlist_set", id, map[string]any{
		"ip_allowlist": allowlist,
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"id":           id,
		"ip_allowlist": allowlist,
		"unrestricted": len(allowlist) == 0,
	})
}
