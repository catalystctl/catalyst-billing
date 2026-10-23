package oauth_test

import (
	"context"
	"testing"
)

// A provider row with no claim mapping must still be readable.
//
// `social_providers.email_verified_claim` is nullable, and the read scans it into a string. A row
// whose value is NULL — written by hand, or by a build that predates the field — used to fail the
// scan, and because the staff list is one query over every row, one such row took the whole
// sign-in provider screen down with a 500. The empty string is the value the rest of the code
// already reads as "this provider cannot prove an address", so the read normalises to it.
func TestProviderWithNullVerifiedClaimIsReadable(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()

	var providerID string
	if err := env.pool.QueryRow(ctx, `
		INSERT INTO social_providers (slug, name, issuer_url, authorization_endpoint,
			token_endpoint, userinfo_endpoint, client_id, client_secret, enabled,
			email_verified_claim)
		VALUES ('null-claim', 'Null Claim', 'https://provider.example.test',
			'https://provider.example.test/authorize', 'https://provider.example.test/token',
			'https://provider.example.test/userinfo', 'client-id', 'client-secret', true, NULL)
		RETURNING id`).Scan(&providerID); err != nil {
		t.Fatalf("insert provider: %v", err)
	}

	providers, err := env.svc.ListProviders(ctx, false)
	if err != nil {
		t.Fatalf("list providers: %v", err)
	}
	found := false
	for _, p := range providers {
		if p.ID != providerID {
			continue
		}
		found = true
		if p.EmailVerifiedClaim != "" {
			t.Fatalf("email_verified_claim = %q, want the empty string", p.EmailVerifiedClaim)
		}
	}
	if !found {
		t.Fatal("the provider with a NULL claim is missing from the list")
	}

	// The single-provider read goes through the same columns, so it is asserted too: a screen
	// that opens one provider must not fail where the list succeeded.
	one, err := env.svc.ProviderByID(ctx, providerID)
	if err != nil {
		t.Fatalf("read provider: %v", err)
	}
	if one.EmailVerifiedClaim != "" {
		t.Fatalf("email_verified_claim = %q", one.EmailVerifiedClaim)
	}
}
