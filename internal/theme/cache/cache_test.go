package cache_test

// The theme snapshot cache.
//
// # What is worth testing here
//
// Not "the cache caches" — that is a map lookup. What matters is the three answers it must always
// give, because the caller has no failure branch to get wrong:
//
//  1. With no store at all, it answers with the built-in default rather than nil. That is the API-only
//     deployment and every unit test.
//  2. With a store that cannot be read, it answers with the default *and* records the error, so the
//     studio's health card can say why rather than showing a blank gallery.
//  3. A snapshot's revision segment is the digest without its prefix, because that segment is a URL
//     and a cache key and a `Cache-Control: immutable` promise.
//
// The fourth property — that a publish is visible immediately and everywhere else within the TTL —
// needs a database, so it is covered by the store suite plus the browser test rather than pretended
// at here.

import (
	"context"
	"testing"
	"time"

	"github.com/catalyst/billing/internal/theme"
	"github.com/catalyst/billing/internal/theme/cache"
	"github.com/catalyst/billing/internal/theme/css"
)

// With no store, every scope serves the built-in default.
//
// This is the shape of an API-only server and of every unit test, and it must render: a nil snapshot
// would make the shell's title blank and its stylesheet absent, which is a worse failure than a
// colour that is one publish out of date.
func TestWithoutAStoreEveryScopeServesTheDefault(t *testing.T) {
	c := cache.New(nil)

	for _, scope := range cache.ScopeOrder {
		snap := c.Snapshot(context.Background(), scope)
		if snap == nil {
			t.Fatalf("%s: no snapshot", scope)
		}
		if snap.Published {
			t.Errorf("%s: a cache with no store reports a published theme", scope)
		}
		if snap.Revision != "default" {
			t.Errorf("%s: revision %q, want \"default\"", scope, snap.Revision)
		}
		if len(snap.CSS) == 0 {
			t.Errorf("%s: no stylesheet", scope)
		}
		if len(snap.Boot) == 0 {
			t.Errorf("%s: no boot script", scope)
		}
		if snap.Scope != scope {
			t.Errorf("%s: the snapshot is labelled %q", scope, snap.Scope)
		}
	}
}

// The default snapshot's stylesheet is the built-in document's, byte for byte.
//
// The cache is not a second generator: if it ever produced something different from css.Generate on
// theme.Default(), a page would render differently depending on whether a theme was published, and
// the difference would be a mystery.
func TestTheDefaultSnapshotIsTheGeneratedDefault(t *testing.T) {
	c := cache.New(nil)
	snap := c.Snapshot(context.Background(), "panel")

	want, err := css.Generate(theme.Default())
	if err != nil {
		t.Fatalf("generate the default: %v", err)
	}
	if string(snap.CSS) != string(want) {
		t.Fatal("the cached default stylesheet differs from the generated default")
	}
	digest, err := theme.Digest(theme.Default())
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	if snap.Digest != digest {
		t.Errorf("digest %q, want %q", snap.Digest, digest)
	}
}

// A revision segment is the digest without its scheme prefix, and a lookup finds it.
func TestRevisionSegmentsAreAddressable(t *testing.T) {
	c := cache.New(nil)
	snap := c.Snapshot(context.Background(), "storefront")

	if got := cache.RevisionSegment("sha256:abc123"); got != "abc123" {
		t.Fatalf("RevisionSegment = %q, want abc123", got)
	}
	if got := cache.RevisionSegment("abc123"); got != "abc123" {
		t.Fatalf("RevisionSegment on a bare digest = %q, want abc123", got)
	}

	found := c.LookupRevision(context.Background(), snap.Revision)
	if found == nil {
		t.Fatal("LookupRevision did not find the current snapshot")
	}
	if found.Scope != "storefront" {
		t.Errorf("LookupRevision found %q, want storefront", found.Scope)
	}
	if got := c.LookupRevision(context.Background(), "not-a-revision"); got != nil {
		t.Errorf("LookupRevision found something for a fabricated segment: %+v", got)
	}
	if got := c.LookupRevision(context.Background(), ""); got != nil {
		t.Error("LookupRevision found something for an empty segment")
	}
}

// A stale snapshot is rebuilt rather than served forever.
//
// The TTL is the fallback for the case the publish-time invalidation does not cover: another instance
// published and this one never heard about it. A cache without it would serve one instance's view of
// the world indefinitely.
func TestAStaleSnapshotIsReloaded(t *testing.T) {
	c := cache.New(nil).WithTTL(time.Nanosecond)
	first := c.Snapshot(context.Background(), "panel")
	// The nanosecond TTL has certainly elapsed by the time this line runs, so the second call must
	// rebuild. Without the TTL the two would be the same pointer.
	time.Sleep(time.Millisecond)
	second := c.Snapshot(context.Background(), "panel")
	if first == second {
		t.Fatal("a snapshot past its TTL was served from the cache")
	}
}

// Invalidate drops a scope and leaves the others.
func TestInvalidateDropsOneScope(t *testing.T) {
	c := cache.New(nil)
	panel := c.Snapshot(context.Background(), "panel")
	storefront := c.Snapshot(context.Background(), "storefront")

	c.Invalidate("panel")

	again := c.Snapshot(context.Background(), "panel")
	if again == panel {
		t.Fatal("Invalidate did not drop the panel snapshot")
	}
	if got := c.Snapshot(context.Background(), "storefront"); got != storefront {
		t.Fatal("Invalidate dropped a scope it was not asked about")
	}

	c.InvalidateAll()
	if got := c.Snapshot(context.Background(), "storefront"); got == storefront {
		t.Fatal("InvalidateAll did not drop the storefront snapshot")
	}
}

// The ETag is derived from the digest, so two instances serving the same revision agree.
func TestETagFollowsTheDigest(t *testing.T) {
	c := cache.New(nil)
	snap := c.Snapshot(context.Background(), "panel")
	want := `W/"` + snap.Digest + `"`
	if got := snap.ETag(); got != want {
		t.Fatalf("ETag = %q, want %q", got, want)
	}
}
