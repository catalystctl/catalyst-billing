package httpapi

import "context"

// API key identity.
//
// A request authenticated with `Authorization: Bearer bk_...` resolves to the key's
// owning user. The key narrows what that user may do; it never widens it. That is why
// the resolved identity is a *user* plus a set of scopes, rather than a separate
// principal type with its own permission table: a key that could outlive a revoked role
// is a permanent backdoor, and this shape makes that impossible by construction.

// APIKeyIdentity is the authenticated key.
type APIKeyIdentity struct {
	// KeyID is the api_keys row.
	KeyID string
	// UserID is the owner. Every permission check still runs against this user.
	UserID string
	// Scopes narrows the owner's permissions. Empty means "no narrowing".
	Scopes []string
}

// Allows reports whether the key's scopes permit a permission.
//
// An empty scope set means the key inherits everything the owner holds, which is what an
// operator creating a key for their own automation expects. A non-empty set intersects.
func (k APIKeyIdentity) Allows(permission string) bool {
	if len(k.Scopes) == 0 {
		return true
	}
	for _, s := range k.Scopes {
		if s == permission {
			return true
		}
	}
	return false
}

// apiKeyCtxKey is the context key for the key identity.
type apiKeyCtxKey struct{}

// withAPIKey attaches a key identity to a request context.
func withAPIKey(ctx context.Context, id APIKeyIdentity) context.Context {
	return context.WithValue(ctx, apiKeyCtxKey{}, id)
}

// apiKeyFrom returns the key identity, if the request used one.
func apiKeyFrom(ctx context.Context) (APIKeyIdentity, bool) {
	id, ok := ctx.Value(apiKeyCtxKey{}).(APIKeyIdentity)
	return id, ok
}
