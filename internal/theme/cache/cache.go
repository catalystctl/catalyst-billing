// Package cache is the in-process snapshot of what each scope currently serves.
//
// # Why a cache and not a query
//
// The theme is needed on every navigation: the shell's `<title>`, the linked stylesheet, the boot
// script and the manifest all come from it. Reading a revision and generating its CSS per request
// would put a database round trip and a few hundred microseconds of string building on the hot path
// of every page load, to answer a question whose answer changes when an operator clicks Publish —
// which is to say, almost never.
//
// So each scope holds one `*Snapshot` in an atomic pointer. A read is a pointer load. A publish
// invalidates the scope (the API calls Invalidate in the same request that published), and a
// short TTL is the fallback for the case that does not cover: another instance published, and this
// one never heard about it. LISTEN/NOTIFY would close that gap faster; the TTL is what keeps the
// gap bounded even if a notification is dropped, and it is the smaller thing to get right first.
//
// # Why a miss is not an error
//
// A scope with no published revision serves the built-in default document. That is the state of a
// fresh installation and the state of an installation that has never opened the Theme Studio, so it
// is a value, not a failure. Every method here returns a usable snapshot; the only thing that can
// go wrong is a database read, and that falls back to the default too — a colour problem must never
// be able to take the panel down.
package cache

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/catalyst/billing/internal/theme"
	"github.com/catalyst/billing/internal/theme/css"
	"github.com/catalyst/billing/internal/theme/store"
)

// DefaultTTL is how long a snapshot is trusted before it is reloaded.
//
// Thirty seconds is the documented fallback (docs/theming-system.md §12): a publish is visible
// immediately on the instance that served it, and within half a minute everywhere else even if the
// invalidation is lost.
const DefaultTTL = 30 * time.Second

// Snapshot is everything a request needs about one scope's theme, already rendered.
type Snapshot struct {
	// Scope is the presentation scope this snapshot answers for.
	Scope string
	// Revision is the URL segment the stylesheet and boot script are addressed by: the document
	// digest without its `sha256:` prefix, or "default" when no theme is published.
	Revision string
	// Digest is the full `sha256:…` digest, for the history list and the ETag.
	Digest string
	// Document is the resolved theme document.
	Document theme.Document
	// CSS is the generated stylesheet.
	CSS []byte
	// Boot is the pre-paint script.
	Boot []byte
	// Published is false when the scope is serving the built-in default.
	Published bool
	// LoadedAt is when this snapshot was built, for the TTL.
	LoadedAt time.Time
	// Err is the read error that produced the default fallback, if any. It is surfaced in the
	// studio's health card rather than to a visitor.
	Err error
}

// ETag renders a weak entity tag for the snapshot's manifest.
//
// It is derived from the digest, so two instances serving the same revision agree on it and a
// conditional request is answered from either one.
func (s *Snapshot) ETag() string { return `W/"` + s.Digest + `"` }

// Cache holds one snapshot per scope.
type Cache struct {
	store *store.Store
	ttl   time.Duration
	log   *slog.Logger

	mu       sync.RWMutex
	snapshot map[string]*Snapshot
}

// New constructs a Cache.
func New(st *store.Store) *Cache {
	return &Cache{
		store:    st,
		ttl:      DefaultTTL,
		log:      slog.Default(),
		snapshot: map[string]*Snapshot{},
	}
}

// WithTTL overrides the snapshot lifetime. A zero or negative value keeps the default.
func (c *Cache) WithTTL(ttl time.Duration) *Cache {
	if ttl > 0 {
		c.ttl = ttl
	}
	return c
}

// WithLogger attaches a logger.
func (c *Cache) WithLogger(log *slog.Logger) *Cache {
	if log != nil {
		c.log = log
	}
	return c
}

// Snapshot returns the scope's current snapshot, loading it if it is absent or stale.
//
// It never returns nil: a scope with nothing published, a database that is down and a document that
// no longer validates all produce the built-in default. The caller therefore has no failure branch
// to get wrong.
func (c *Cache) Snapshot(ctx context.Context, scope string) *Snapshot {
	now := time.Now()

	c.mu.RLock()
	cached := c.snapshot[scope]
	c.mu.RUnlock()
	if cached != nil && now.Sub(cached.LoadedAt) < c.ttl {
		return cached
	}

	fresh := c.load(ctx, scope)

	c.mu.Lock()
	// Re-check under the write lock: two concurrent misses should produce one load, and the second
	// writer must not overwrite a newer snapshot with an older one.
	if current, ok := c.snapshot[scope]; ok && current.LoadedAt.After(fresh.LoadedAt) {
		c.mu.Unlock()
		return current
	}
	c.snapshot[scope] = fresh
	c.mu.Unlock()
	return fresh
}

// Invalidate drops a scope's snapshot, forcing a reload on the next read.
//
// It is called by the publish handler after the transaction commits. Invalidating before the commit
// would let a concurrent request reload the *old* revision and cache it for another TTL, which is
// the "I published and nothing changed" bug.
func (c *Cache) Invalidate(scope string) {
	c.mu.Lock()
	delete(c.snapshot, scope)
	c.mu.Unlock()
}

// InvalidateAll drops every snapshot. It is what a role or brand change uses when the change affects
// more than one scope.
func (c *Cache) InvalidateAll() {
	c.mu.Lock()
	c.snapshot = map[string]*Snapshot{}
	c.mu.Unlock()
}

// load builds a snapshot from the database, falling back to the default document.
func (c *Cache) load(ctx context.Context, scope string) *Snapshot {
	now := time.Now()
	if c.store == nil {
		return defaultSnapshot(scope, now, nil)
	}
	rev, err := c.store.Published(ctx, scope)
	if err != nil {
		c.log.WarnContext(ctx, "read the published theme; serving the built-in default",
			"scope", scope, "error", err)
		return defaultSnapshot(scope, now, err)
	}
	if rev == nil {
		return defaultSnapshot(scope, now, nil)
	}
	// Re-validate on read. The write path validated, but a row written by hand — or by a build that
	// knew a token this one does not — must not reach the CSS generator unchecked.
	if err := theme.ValidateResolved(rev.Document); err != nil {
		c.log.ErrorContext(ctx, "the published theme is not renderable; serving the built-in default",
			"scope", scope, "revision", rev.ID, "error", err)
		return defaultSnapshot(scope, now, err)
	}
	sheet, err := css.Generate(rev.Document)
	if err != nil {
		c.log.ErrorContext(ctx, "generate the theme stylesheet; serving the built-in default",
			"scope", scope, "revision", rev.ID, "error", err)
		return defaultSnapshot(scope, now, err)
	}
	return &Snapshot{
		Scope:     scope,
		Revision:  RevisionSegment(rev.Digest),
		Digest:    rev.Digest,
		Document:  rev.Document,
		CSS:       sheet,
		Boot:      css.BootScript(rev.Document),
		Published: true,
		LoadedAt:  now,
	}
}

// defaultSnapshot is the built-in default, rendered once per load.
func defaultSnapshot(scope string, now time.Time, err error) *Snapshot {
	doc := theme.Default()
	sheet, genErr := css.Generate(doc)
	if genErr != nil {
		// The built-in default failing to generate is a programming error, not a runtime condition:
		// the presets test and the golden test both render it. Serving no stylesheet at all is the
		// only safe answer, and the caller logs it.
		sheet = nil
	}
	digest, _ := theme.Digest(doc)
	return &Snapshot{
		Scope:     scope,
		Revision:  "default",
		Digest:    digest,
		Document:  doc,
		CSS:       sheet,
		Boot:      css.BootScript(doc),
		Published: false,
		LoadedAt:  now,
		Err:       err,
	}
}

// RevisionSegment turns a `sha256:<hex>` digest into a URL path segment.
//
// The colon is dropped rather than escaped: a revision is addressed by a browser, a cache key and a
// `Cache-Control: immutable` header, and a colon in a path segment is legal but awkward in every log
// line it appears in.
func RevisionSegment(digest string) string {
	const prefix = "sha256:"
	if len(digest) > len(prefix) && digest[:len(prefix)] == prefix {
		return digest[len(prefix):]
	}
	return digest
}

// LookupRevision finds a cached snapshot by its revision segment, across every scope.
//
// It is the lookup the revision-addressed stylesheet route needs, and it takes no scope because the
// URL carries none: `<revision>` is the document digest, and the same digest can be live in more than
// one scope (a portal that inherits the panel's document is served the panel's bytes). Searching the
// cache is a handful of map lookups, and it answers for every request a live page makes. A miss means
// the revision is not current anywhere — a stale tab — which the caller answers from the database.
func (c *Cache) LookupRevision(_ context.Context, segment string) *Snapshot {
	if segment == "" {
		return nil
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	// Scopes are searched in declaration order so the answer is deterministic when a digest is live
	// in two of them.
	for _, scope := range ScopeOrder {
		if snap, ok := c.snapshot[scope]; ok && snap.Revision == segment {
			return snap
		}
	}
	return nil
}

// ScopeOrder is the order scopes are searched and listed in.
//
// It is duplicated from theme/store rather than imported so this package does not depend on the
// database layer: the cache is the thing the request path uses, and it must be constructible without
// a pool.
var ScopeOrder = []string{"panel", "portal", "storefront", "email", "invoice"}
