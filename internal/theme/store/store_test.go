package store_test

// The theme store against a real database.
//
// # Why these are database tests and not mocks
//
// Every invariant this package is responsible for is a *database* invariant: at most one open draft
// per theme, at most one published revision, an assignment that points at a published revision, an
// asset that cannot be deleted while a document references it. A mock would assert that the code
// calls the SQL the test expects — which is a tautology — while the two failures that actually
// happened during development were both schema-level:
//
//   - keying themes on (scope, slug) left two open drafts in one scope the moment an operator tried a
//     second preset, so "Save" had no single target;
//   - the composite foreign key from theme_assignments into (theme_id, id) is what proves the
//     assignment's revision belongs to the assignment's theme, and nothing but a real constraint can
//     demonstrate that.
//
// They skip without a database (storetest.DB) and are required in CI (BILLING_REQUIRE_DB=1).

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/catalyst/billing/internal/store/storetest"
	"github.com/catalyst/billing/internal/theme"
	"github.com/catalyst/billing/internal/theme/store"
)

// newStore returns a Store over a freshly truncated database.
func newStore(t *testing.T) (*store.Store, context.Context) {
	t.Helper()
	pool := storetest.DB(t)
	return store.New(pool), context.Background()
}

// draftDoc returns a valid resolved document with a distinguishing id.
func draftDoc(t *testing.T, id, name string) theme.Document {
	t.Helper()
	doc := theme.Default()
	doc.ID = id
	doc.Name = name
	if err := theme.ValidateResolved(doc); err != nil {
		t.Fatalf("the fixture document is not valid: %v", err)
	}
	return doc
}

// The empty state: nothing published, nothing drafted.
//
// A fresh installation is this state, and it must be a value rather than an error, because every
// caller would otherwise need a "no theme yet" branch — and the branch that gets forgotten is the one
// that takes a page down.
func TestAFreshInstallationHasNothingPublished(t *testing.T) {
	st, ctx := newStore(t)

	published, err := st.Published(ctx, "panel")
	if err != nil {
		t.Fatalf("Published: %v", err)
	}
	if published != nil {
		t.Fatalf("a fresh installation has a published revision: %+v", published)
	}
	drafts, err := st.Drafts(ctx, "panel")
	if err != nil {
		t.Fatalf("Drafts: %v", err)
	}
	if len(drafts) != 0 {
		t.Fatalf("%d drafts on a fresh installation", len(drafts))
	}
	history, err := st.History(ctx, "panel", 10)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(history) != 0 {
		t.Fatalf("%d publications on a fresh installation", len(history))
	}
}

// One open draft per scope, whatever the document calls itself.
//
// The document's `id` is a label that follows the current revision; it is not the key. Keying the
// theme row on it left two drafts open in one scope, which is the bug this test pins.
func TestOnlyOneDraftIsOpenPerScope(t *testing.T) {
	st, ctx := newStore(t)

	first, err := st.CreateDraft(ctx, store.DraftInput{
		Scope: "panel", Source: draftDoc(t, "brutalist", "Brutalist"), Document: draftDoc(t, "brutalist", "Brutalist"),
	})
	if err != nil {
		t.Fatalf("first draft: %v", err)
	}
	second, err := st.CreateDraft(ctx, store.DraftInput{
		Scope: "panel", Source: draftDoc(t, "warm-saas", "Warm SaaS"), Document: draftDoc(t, "warm-saas", "Warm SaaS"),
	})
	if err != nil {
		t.Fatalf("second draft: %v", err)
	}
	if second.Number != first.Number+1 {
		t.Fatalf("the second draft is revision %d, want %d", second.Number, first.Number+1)
	}

	drafts, err := st.Drafts(ctx, "panel")
	if err != nil {
		t.Fatalf("Drafts: %v", err)
	}
	if len(drafts) != 1 {
		t.Fatalf("%d open drafts, want 1", len(drafts))
	}
	if drafts[0].ID != second.ID {
		t.Fatalf("the open draft is %s, want the newest (%s)", drafts[0].ID, second.ID)
	}

	// The first is archived, not deleted: the history is the point of a revision.
	old, err := st.Revision(ctx, first.ID)
	if err != nil {
		t.Fatalf("read the archived revision: %v", err)
	}
	if old.Status != store.StatusArchived {
		t.Fatalf("the replaced draft has status %q, want archived", old.Status)
	}
	// And the theme row's label followed the newest document, so the history list says what the theme
	// currently is.
	if second.Slug != "warm-saas" || second.Name != "Warm SaaS" {
		t.Fatalf("the theme row did not follow the document: slug %q name %q", second.Slug, second.Name)
	}
}

// A publish points the scope at the revision, and a second publish replaces it.
func TestPublishPointsTheScopeAtOneRevision(t *testing.T) {
	st, ctx := newStore(t)

	draft, err := st.CreateDraft(ctx, store.DraftInput{
		Scope: "storefront", Source: draftDoc(t, "one", "One"), Document: draftDoc(t, "one", "One"),
	})
	if err != nil {
		t.Fatalf("draft: %v", err)
	}
	result, err := st.Publish(ctx, draft.ID, "", "first")
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if result.Revision.Status != store.StatusPublished {
		t.Fatalf("status %q, want published", result.Revision.Status)
	}
	if result.Revision.PublishedAt == nil {
		t.Fatal("a published revision has no published_at")
	}
	if result.Publication.PreviousRevisionID != nil {
		t.Fatal("the first publication claims to have replaced something")
	}

	published, err := st.Published(ctx, "storefront")
	if err != nil || published == nil {
		t.Fatalf("Published: %v (%v)", err, published)
	}
	if published.ID != draft.ID {
		t.Fatalf("the scope serves %s, want %s", published.ID, draft.ID)
	}

	// A second revision publishes over the first.
	second, err := st.CreateDraft(ctx, store.DraftInput{
		Scope: "storefront", Source: draftDoc(t, "two", "Two"), Document: draftDoc(t, "two", "Two"),
	})
	if err != nil {
		t.Fatalf("second draft: %v", err)
	}
	again, err := st.Publish(ctx, second.ID, "", "second")
	if err != nil {
		t.Fatalf("publish 2: %v", err)
	}
	if again.Publication.PreviousRevisionID == nil || *again.Publication.PreviousRevisionID != draft.ID {
		t.Fatalf("the second publication replaced %v, want %s", again.Publication.PreviousRevisionID, draft.ID)
	}

	// The first is archived, so "the published revision" is one row rather than a query convention.
	first, err := st.Revision(ctx, draft.ID)
	if err != nil {
		t.Fatalf("read the first: %v", err)
	}
	if first.Status != store.StatusArchived {
		t.Fatalf("the replaced revision has status %q, want archived", first.Status)
	}

	// The history lists both, newest first.
	history, err := st.History(ctx, "storefront", 10)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(history) != 2 {
		t.Fatalf("%d publications, want 2", len(history))
	}
	if history[0].RevisionID != second.ID {
		t.Fatalf("the newest publication is %s, want %s", history[0].RevisionID, second.ID)
	}
}

// A published revision cannot be edited, and an archived one cannot be published directly.
func TestOnlyADraftCanBeEditedOrPublished(t *testing.T) {
	st, ctx := newStore(t)

	draft, err := st.CreateDraft(ctx, store.DraftInput{
		Scope: "panel", Source: draftDoc(t, "one", "One"), Document: draftDoc(t, "one", "One"),
	})
	if err != nil {
		t.Fatalf("draft: %v", err)
	}
	if _, err := st.Publish(ctx, draft.ID, "", ""); err != nil {
		t.Fatalf("publish: %v", err)
	}

	// Editing the published revision is refused, and the error says which case it is.
	_, err = st.UpdateDraft(ctx, draft.ID, store.DraftInput{
		Source: draftDoc(t, "edited", "Edited"), Document: draftDoc(t, "edited", "Edited"),
	})
	if !errors.Is(err, store.ErrNotDraft) {
		t.Fatalf("editing a published revision: %v, want ErrNotDraft", err)
	}

	// Publishing it again is allowed — that is what "roll back to this revision" is — and it writes
	// another publication row rather than silently succeeding.
	again, err := st.Publish(ctx, draft.ID, "", "rollback")
	if err != nil {
		t.Fatalf("republishing: %v", err)
	}
	if again.Revision.Status != store.StatusPublished {
		t.Fatalf("status %q after a rollback, want published", again.Revision.Status)
	}

	// An archived revision must be restored first: publishing history in place would make the
	// history list stop describing what happened.
	other, err := st.CreateDraft(ctx, store.DraftInput{
		Scope: "panel", Source: draftDoc(t, "two", "Two"), Document: draftDoc(t, "two", "Two"),
	})
	if err != nil {
		t.Fatalf("second draft: %v", err)
	}
	if _, err := st.Publish(ctx, other.ID, "", ""); err != nil {
		t.Fatalf("publish the second: %v", err)
	}
	if _, err := st.Publish(ctx, draft.ID, "", ""); err == nil {
		t.Fatal("an archived revision was published directly")
	}
}

// Restoring copies the source, not the resolved document.
//
// The distinction is the whole inheritance story: a storefront theme restored from revision 3 should
// re-resolve against today's panel, not freeze the panel as it was then.
func TestRestoreCopiesTheSourceAndResolvesAgain(t *testing.T) {
	st, ctx := newStore(t)

	// A source that states only an accent: everything else is inherited.
	source := theme.Document{
		Schema: theme.SchemaVersion,
		ID:     "sparse",
		Name:   "Sparse",
		Color:  theme.Color{Brand: theme.BrandPalette{Primary: "#7c3aed"}},
	}
	resolved, err := theme.Resolve(theme.Default(), source)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	first, err := st.CreateDraft(ctx, store.DraftInput{
		Scope: "panel", Source: source, Document: resolved,
	})
	if err != nil {
		t.Fatalf("draft: %v", err)
	}

	restored, err := st.Restore(ctx, first.ID, "")
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	// The source came back sparse: it has the accent and not the full palette.
	if restored.Source.Color.Brand.Primary != "#7c3aed" {
		t.Errorf("the restored source lost its primary: %q", restored.Source.Color.Brand.Primary)
	}
	if restored.Source.Color.Dark.Bg != "" {
		t.Errorf("the restored source carries an inherited background (%q); it should be a layer",
			restored.Source.Color.Dark.Bg)
	}
	// The resolved document is complete, because that is what renders.
	if restored.Document.Color.Dark.Bg == "" {
		t.Error("the restored resolved document is missing the inherited background")
	}
	if restored.Status != store.StatusDraft {
		t.Errorf("status %q, want draft", restored.Status)
	}
	if restored.Number <= first.Number {
		t.Errorf("the restored revision is number %d, want more than %d", restored.Number, first.Number)
	}
}

// A revision can be found by its digest, which is what the revision-addressed stylesheet route needs.
func TestByDigestFindsAnArchivedRevision(t *testing.T) {
	st, ctx := newStore(t)

	first, err := st.CreateDraft(ctx, store.DraftInput{
		Scope: "panel", Source: draftDoc(t, "one", "One"), Document: draftDoc(t, "one", "One"),
	})
	if err != nil {
		t.Fatalf("draft: %v", err)
	}
	if _, err := st.Publish(ctx, first.ID, "", ""); err != nil {
		t.Fatalf("publish: %v", err)
	}
	second, err := st.CreateDraft(ctx, store.DraftInput{
		Scope: "panel", Source: draftDoc(t, "two", "Two"), Document: draftDoc(t, "two", "Two"),
	})
	if err != nil {
		t.Fatalf("second draft: %v", err)
	}
	if _, err := st.Publish(ctx, second.ID, "", ""); err != nil {
		t.Fatalf("publish 2: %v", err)
	}

	found, err := st.ByDigest(ctx, "panel", first.Digest)
	if err != nil {
		t.Fatalf("ByDigest: %v", err)
	}
	if found == nil || found.ID != first.ID {
		t.Fatalf("ByDigest returned %v, want the archived first revision", found)
	}
	// A digest that does not exist is (nil, nil): "not this scope's revision" is a value, not an
	// error, because the caller falls through to another scope.
	missing, err := st.ByDigest(ctx, "panel", "sha256:"+strings.Repeat("0", 64))
	if err != nil {
		t.Fatalf("ByDigest for a missing digest: %v", err)
	}
	if missing != nil {
		t.Fatalf("ByDigest found a revision for a fabricated digest: %+v", missing)
	}
}

// An asset cannot be deleted while a document references it.
//
// A published revision's stylesheet would otherwise start returning 404 for its own background, which
// is a silent visual regression on a live site rather than an error anybody sees.
func TestAnAssetInUseCannotBeDeleted(t *testing.T) {
	st, ctx := newStore(t)

	asset, err := st.PutAsset(ctx, store.AssetInput{
		Kind:        "background",
		ContentType: "image/png",
		Bytes:       []byte("\x89PNG\r\n\x1a\nfixture"),
		Digest:      "sha256:0000000000000000000000000000000000000000000000000000000000000001",
	})
	if err != nil {
		t.Fatalf("PutAsset: %v", err)
	}

	// Unreferenced: the delete works.
	if err := st.DeleteAsset(ctx, asset.ID); err != nil {
		t.Fatalf("deleting an unused asset: %v", err)
	}

	// Now store it again and reference it from a document.
	asset, err = st.PutAsset(ctx, store.AssetInput{
		Kind:        "background",
		ContentType: "image/png",
		Bytes:       []byte("\x89PNG\r\n\x1a\nfixture"),
		Digest:      "sha256:0000000000000000000000000000000000000000000000000000000000000001",
	})
	if err != nil {
		t.Fatalf("PutAsset (second): %v", err)
	}
	doc := draftDoc(t, "with-asset", "With asset")
	doc.Background = theme.Background{Style: "image", Image: theme.AssetRef(asset.ID)}
	if _, err := st.CreateDraft(ctx, store.DraftInput{Scope: "panel", Source: doc, Document: doc}); err != nil {
		t.Fatalf("draft: %v", err)
	}
	if err := st.DeleteAsset(ctx, asset.ID); !errors.Is(err, store.ErrAssetInUse) {
		t.Fatalf("deleting a referenced asset: %v, want ErrAssetInUse", err)
	}
}

// An upload is deduped on (digest, kind), so the same bytes in two roles are two rows.
//
// The roles matter: the same PNG used as a favicon and as a social card must survive deleting one of
// them, and a dedupe that ignored the kind would make that impossible.
func TestAssetsDedupeOnDigestAndKind(t *testing.T) {
	st, ctx := newStore(t)

	const digest = "sha256:0000000000000000000000000000000000000000000000000000000000000002"
	bytes := []byte("\x89PNG\r\n\x1a\nshared")

	first, err := st.PutAsset(ctx, store.AssetInput{Kind: "favicon", ContentType: "image/png", Bytes: bytes, Digest: digest})
	if err != nil {
		t.Fatalf("PutAsset favicon: %v", err)
	}
	again, err := st.PutAsset(ctx, store.AssetInput{Kind: "favicon", ContentType: "image/png", Bytes: bytes, Digest: digest})
	if err != nil {
		t.Fatalf("PutAsset favicon (again): %v", err)
	}
	if again.ID != first.ID {
		t.Fatalf("the same bytes in the same role made two rows: %s and %s", first.ID, again.ID)
	}

	other, err := st.PutAsset(ctx, store.AssetInput{Kind: "og", ContentType: "image/png", Bytes: bytes, Digest: digest})
	if err != nil {
		t.Fatalf("PutAsset og: %v", err)
	}
	if other.ID == first.ID {
		t.Fatal("the same bytes in two roles collapsed into one row; deleting one would break the other")
	}

	assets, err := st.Assets(ctx)
	if err != nil {
		t.Fatalf("Assets: %v", err)
	}
	if len(assets) != 2 {
		t.Fatalf("%d assets, want 2", len(assets))
	}
}

// An invalid scope is refused before anything is written.
func TestAnUnknownScopeIsRefused(t *testing.T) {
	st, ctx := newStore(t)

	_, err := st.CreateDraft(ctx, store.DraftInput{
		Scope: "invoice-pdf", Source: draftDoc(t, "x", "X"), Document: draftDoc(t, "x", "X"),
	})
	if err == nil {
		t.Fatal("CreateDraft accepted an unknown scope")
	}
}
