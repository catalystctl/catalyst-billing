package httpapi

// The locales screen's API (I18N-01…I18N-08).
//
// Two things about this file are worth explaining before it is read.
//
// **Why a dedicated audit wrapper rather than the catalogue one.** `withCatalogAudit` is the
// shape every staff write in this package uses — the audit row is written inside the transaction
// that made the change, so "who renamed this" cannot be lost. This file follows the same shape
// for the same reason, and it takes the locale's code as the target id rather than a row id
// because the code *is* the primary key: it appears in URLs and every catalogue row references
// it, so it is the identifier an operator will recognise in the audit log.
//
// **Why deleting the default locale is a 409 and not a 400.** The request is well-formed and the
// operator may be perfectly entitled to delete a locale; what makes it impossible is the state of
// the installation. A 409 says "not while this is true", which is what an operator needs to hear
// so the next action is "make another locale the default", not "fix my request".

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/catalyst/billing/internal/audit"
	"github.com/catalyst/billing/internal/authn"
	"github.com/catalyst/billing/internal/i18n"
)

func init() {
	RegisterRoutes("60-i18n", registerI18nRoutes)
}

func registerI18nRoutes(r *router) {
	// The public half: what the storefront and the portal need to offer a language switcher and
	// to resolve a request. It carries no message text — a client fetches the merged catalogue
	// for the locale it settled on — so it is safe to serve to an anonymous visitor.
	r.publicGet("/api/v1/i18n/locales", r.s.handlePublicLocales)
	r.publicGet("/api/v1/i18n/messages", r.s.handleI18nMessages)

	// The customer's own preference. It is registered here rather than in a customer file
	// because it is the first branch of this package's resolution chain, and the chain is only
	// real when the branch is writable: for as long as nothing stored it, a customer's language
	// choice lived in one browser's localStorage and every other device — and every queued
	// email — resolved through the header instead.
	r.userGet("/api/v1/me/locale", r.s.handleGetMyLocale)
	r.userMutation(http.MethodPut, "/api/v1/me/locale", false, r.s.handleSetMyLocale)

	// The operator's half.
	r.staffGet("/api/v1/staff/locales", authn.PermI18nWrite, r.s.handleListLocales)
	r.staffMutation(http.MethodPost, "/api/v1/staff/locales", authn.PermI18nWrite, r.s.handleCreateLocale)
	r.staffMutation(http.MethodPatch, "/api/v1/staff/locales/{code}", authn.PermI18nWrite,
		r.s.handleUpdateLocale)
	r.staffMutation(http.MethodPost, "/api/v1/staff/locales/{code}/default", authn.PermI18nWrite,
		r.s.handleSetDefaultLocale)
	r.staffMutation(http.MethodDelete, "/api/v1/staff/locales/{code}", authn.PermI18nWrite,
		r.s.handleDeleteLocale)

	// The shipped languages this installation has not added yet, and the one-click install that
	// turns one of them on (I18N-01). A read and a mutation, registered here rather than as part
	// of the create form: the point of the list is that an operator picks from names this build
	// already knows instead of typing a language's English and native names by hand.
	r.staffGet("/api/v1/staff/locales/available", authn.PermI18nWrite, r.s.handleAvailableLocales)
	r.staffMutation(http.MethodPost, "/api/v1/staff/locales/{code}/install", authn.PermI18nWrite,
		r.s.handleInstallLocale)

	// Translations. The search is a read; the write is a mutation with an Idempotency-Key.
	r.staffGet("/api/v1/staff/locales/keys", authn.PermI18nWrite, r.s.handleListKeys)
	r.staffMutation(http.MethodPut, "/api/v1/staff/locales/{code}/keys/{key}", authn.PermI18nWrite,
		r.s.handlePutTranslation)
	r.staffMutation(http.MethodDelete, "/api/v1/staff/locales/{code}/keys/{key}", authn.PermI18nWrite,
		r.s.handleDeleteTranslation)
}

// i18nStore is the store every handler in this file uses.
func (s *Server) i18nStore() *i18n.Store {
	return i18n.New(s.pool)
}

// ------------------------------------------------------------------ public --

// handlePublicLocales answers the enabled locales and the default.
//
// It is public because the storefront is public: a visitor who has not signed in still sees the
// language switcher, and the header that resolves their request is read before any account
// exists. The response is deliberately small — codes and names, no catalogue — so an anonymous
// request cannot pull the whole message table.
func (s *Server) handlePublicLocales(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "storage_unavailable", "locales need a database")
		return
	}
	store := s.i18nStore()
	enabled, err := store.Enabled(r.Context())
	if err != nil {
		s.i18nError(w, r, err)
		return
	}
	resolved := i18n.NewResolver(store.Default(r.Context()), localeCodes(enabled)).
		Resolve(s.sessionLocale(r), r.Header.Get("Accept-Language"))

	out := make([]map[string]any, 0, len(enabled))
	for _, l := range enabled {
		out = append(out, map[string]any{
			"code":        l.Code,
			"name":        l.Name,
			"native_name": l.NativeName,
			"is_default":  l.IsDefault,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"locales": out,
		"default": store.Default(r.Context()),
		// What this request resolved to, and why. The source is the useful half: it tells the
		// storefront whether to show the switcher pre-set to the visitor's own choice or to their
		// browser's guess, which are different things to display.
		"resolved": map[string]any{
			"locale": resolved.Locale,
			"source": resolved.Source,
		},
	})
}

// handleI18nMessages serves the merged message catalogue for one locale.
//
// "Merged" is the whole point: the response contains the locale's own text plus the default
// locale's text for every key it lacks. A client that received only the locale's rows would have
// to reimplement the fallback chain, and would get it subtly different — which is how a blank
// label reaches a screen.
func (s *Server) handleI18nMessages(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "storage_unavailable", "the catalogue needs a database")
		return
	}
	store := s.i18nStore()
	locale := strings.TrimSpace(r.URL.Query().Get("locale"))
	if locale == "" {
		locale = i18n.NewResolver(store.Default(r.Context()), nil).Resolve(
			s.sessionLocale(r), r.Header.Get("Accept-Language")).Locale
	}
	merged, err := store.CatalogFor(r.Context(), locale)
	if err != nil {
		s.i18nError(w, r, err)
		return
	}
	// A catalogue is public configuration, not state: it may be cached briefly, but the cache
	// must be revalidated, because an operator editing a translation expects the change to appear
	// without a deploy.
	w.Header().Set("Cache-Control", "public, max-age=60, must-revalidate")
	writeJSON(w, http.StatusOK, map[string]any{"locale": locale, "messages": merged})
}

// sessionLocale returns the signed-in user's stored preference, or "" for a visitor.
//
// It reads the session only when there is one: the storefront resolves for anonymous visitors
// constantly, and a locale lookup that ran a query for every one of them would put a database
// round trip on the hottest public path for no gain.
func (s *Server) sessionLocale(r *http.Request) string {
	sess, ok := sessionFrom(r.Context())
	if !ok {
		return ""
	}
	locale, err := s.i18nStore().UserLocale(r.Context(), sess.UserID)
	if err != nil {
		return ""
	}
	return locale
}

func localeCodes(locales []i18n.Locale) []string {
	out := make([]string, 0, len(locales))
	for _, l := range locales {
		out = append(out, l.Code)
	}
	return out
}

// -------------------------------------------------------------- preference --

// handleGetMyLocale answers the language this account chose, or "" when it never chose one.
//
// Empty is an answer rather than an absence: it is what tells a client to keep following the
// browser's header for this person instead of pinning a language they never picked. The default
// is included so a client can say "no preference, so English" without a second request.
func (s *Server) handleGetMyLocale(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "storage_unavailable",
			"language preferences need a database")
		return
	}
	user, ok := userFrom(r.Context())
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "authentication_required", "sign in to read your language")
		return
	}
	store := s.i18nStore()
	locale, err := store.UserLocale(r.Context(), user.ID)
	if err != nil {
		s.i18nError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"locale":  locale,
		"default": store.Default(r.Context()),
	})
}

// handleSetMyLocale stores the caller's language — the durable half of the switcher.
//
// PUT, not POST: an account has exactly one preference, so sending it twice is sending the same
// value rather than adding a second one. An empty `locale` clears it, which is how a customer
// goes back to following whichever browser they are sitting in front of.
func (s *Server) handleSetMyLocale(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "storage_unavailable",
			"language preferences need a database")
		return
	}
	user, ok := userFrom(r.Context())
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "authentication_required", "sign in to choose a language")
		return
	}
	var in struct {
		Locale string `json:"locale"`
	}
	if !s.decodeJSON(w, r, &in, 1<<10) {
		return
	}
	// The store is what refuses a language the catalogue cannot render, so a locale the switcher
	// would never offer cannot be written by hand either.
	store := s.i18nStore()
	stored, err := store.SetUserLocale(r.Context(), user.ID, in.Locale)
	if err != nil {
		s.i18nError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"locale":  stored,
		"default": store.Default(r.Context()),
	})
}

// ------------------------------------------------------------------- staff --

// handleListLocales serves every locale, enabled or not, plus the coverage each one has.
//
// Coverage is included because it is the question the screen exists to answer: an operator adds a
// locale and immediately wants to know how much is still untranslated. Making it a second request
// would be a second chance to render "0%" for a moment.
func (s *Server) handleListLocales(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "storage_unavailable", "locales need a database")
		return
	}
	store := s.i18nStore()
	locales, err := store.Locales(r.Context())
	if err != nil {
		s.i18nError(w, r, err)
		return
	}
	coverage, err := store.Coverage(r.Context())
	if err != nil {
		s.i18nError(w, r, err)
		return
	}
	// The total is the default locale's catalogue size, which is what "how many keys exist"
	// means: a key with no row anywhere is not a key, it is a typo in a translation.
	defaultCatalog, err := store.CatalogFor(r.Context(), store.Default(r.Context()))
	if err != nil {
		s.i18nError(w, r, err)
		return
	}
	total := len(defaultCatalog)

	out := make([]map[string]any, 0, len(locales))
	for _, l := range locales {
		out = append(out, localeJSON(l, coverage[l.Code], total))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"data":        out,
		"total_keys":  total,
		"default":     store.Default(r.Context()),
		"server_time": time.Now().UTC().Format("2006-01-02T15:04:05.000Z07:00"),
	})
}

func localeJSON(l i18n.Locale, translated, total int) map[string]any {
	return map[string]any{
		"code":        l.Code,
		"name":        l.Name,
		"native_name": l.NativeName,
		"enabled":     l.Enabled,
		"is_default":  l.IsDefault,
		"sort_order":  l.SortOrder,
		"translated":  translated,
		"total_keys":  total,
		// A percentage is what an operator reads. It is computed here rather than in the client so
		// every surface agrees on the rounding.
		"coverage":   percentOf(translated, total),
		"created_at": l.CreatedAt.UTC().Format("2006-01-02T15:04:05.000Z07:00"),
		"updated_at": l.UpdatedAt.UTC().Format("2006-01-02T15:04:05.000Z07:00"),
	}
}

func percentOf(part, whole int) int {
	if whole <= 0 {
		return 0
	}
	return (part*100 + whole/2) / whole
}

// handleAvailableLocales lists the shipped languages that are not configured yet.
//
// It answers a question the create form could not: "what else can this installation speak without
// someone typing a language's names and tag correctly". The list is code, not data — see
// internal/i18n/shipped.go for why it is not seeded into the locales table — and it shrinks as
// languages are installed, so the screen can show an empty state rather than a row that fails on
// submit.
func (s *Server) handleAvailableLocales(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "storage_unavailable", "locales need a database")
		return
	}
	store := s.i18nStore()
	locales, err := store.Locales(r.Context())
	if err != nil {
		s.i18nError(w, r, err)
		return
	}
	available := i18n.AvailableShipped(locales)
	out := make([]map[string]any, 0, len(available))
	for _, l := range available {
		out = append(out, map[string]any{
			"code":        l.Code,
			"name":        l.Name,
			"native_name": l.NativeName,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"data": out,
		// The total is the size of the shipped menu, which is what "how many more are there"
		// means. It is not a promise about translations: an installed language starts at 0%.
		"shipped": len(i18n.ShippedLocales()),
	})
}

// handleInstallLocale adds one shipped language, enabled, with its names filled in.
//
// It is a separate endpoint from create because it is a separate act: create is "a language this
// build has never heard of, defined by hand"; install is "one of ours, switched on". The audit
// action differs too, so the log distinguishes an operator picking from a menu from an operator
// authoring a language row.
func (s *Server) handleInstallLocale(w http.ResponseWriter, r *http.Request) {
	code := strings.TrimSpace(r.PathValue("code"))
	var out i18n.Locale
	err := s.withLocaleAudit(r, "i18n.locale.install", code, nil,
		func(ctx context.Context, tx pgx.Tx) error {
			var err error
			out, err = s.i18nStore().InstallShippedTx(ctx, tx, code)
			return err
		})
	if err != nil {
		s.i18nError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, localeJSON(out, 0, 0))
}

func (s *Server) handleCreateLocale(w http.ResponseWriter, r *http.Request) {
	var in i18n.LocaleInput
	if !s.decodeJSON(w, r, &in, 8<<10) {
		return
	}
	var out i18n.Locale
	err := s.withLocaleAudit(r, "i18n.locale.create", in.Code, nil,
		func(ctx context.Context, tx pgx.Tx) error {
			var err error
			out, err = s.i18nStore().CreateTx(ctx, tx, in)
			return err
		})
	if err != nil {
		s.i18nError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, localeJSON(out, 0, 0))
}

func (s *Server) handleUpdateLocale(w http.ResponseWriter, r *http.Request) {
	var in i18n.LocaleInput
	if !s.decodeJSON(w, r, &in, 8<<10) {
		return
	}
	code := strings.TrimSpace(r.PathValue("code"))
	var out i18n.Locale
	err := s.withLocaleAudit(r, "i18n.locale.update", code, localeChanges(in),
		func(ctx context.Context, tx pgx.Tx) error {
			var err error
			out, err = s.i18nStore().UpdateTx(ctx, tx, code, in)
			return err
		})
	if err != nil {
		s.i18nError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, localeJSON(out, 0, 0))
}

// handleSetDefaultLocale moves the default.
//
// It is its own endpoint rather than a field on update because it is the one locale change that
// moves every request in the installation at once, and an operator should be able to make it
// without also sending the rest of the locale's fields — a PATCH that carries the old name would
// silently revert a rename made a moment earlier in another tab.
func (s *Server) handleSetDefaultLocale(w http.ResponseWriter, r *http.Request) {
	code := strings.TrimSpace(r.PathValue("code"))
	var out i18n.Locale
	err := s.withLocaleAudit(r, "i18n.locale.set_default", code, nil,
		func(ctx context.Context, tx pgx.Tx) error {
			var err error
			out, err = s.i18nStore().SetDefaultTx(ctx, tx, code)
			return err
		})
	if err != nil {
		s.i18nError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, localeJSON(out, 0, 0))
}

func (s *Server) handleDeleteLocale(w http.ResponseWriter, r *http.Request) {
	code := strings.TrimSpace(r.PathValue("code"))
	err := s.withLocaleAudit(r, "i18n.locale.delete", code, nil,
		func(ctx context.Context, tx pgx.Tx) error {
			return s.i18nStore().DeleteTx(ctx, tx, code)
		})
	if err != nil {
		s.i18nError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleListKeys searches the catalogue by key.
func (s *Server) handleListKeys(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "storage_unavailable", "the catalogue needs a database")
		return
	}
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	prefix := strings.TrimSpace(r.URL.Query().Get("prefix"))
	limit := 100
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 && n <= 500 {
			limit = n
		}
	}
	keys, err := s.i18nStore().Keys(r.Context(), query, prefix, limit)
	if err != nil {
		s.i18nError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": keys, "limit": limit})
}

func (s *Server) handlePutTranslation(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Text string `json:"text"`
	}
	if !s.decodeJSON(w, r, &in, 64<<10) {
		return
	}
	code := strings.TrimSpace(r.PathValue("code"))
	key := strings.TrimSpace(r.PathValue("key"))

	var out i18n.Entry
	err := s.withLocaleAudit(r, "i18n.translation.put", code+"/"+key,
		map[string]any{"text_bytes": len(in.Text)},
		func(ctx context.Context, tx pgx.Tx) error {
			var err error
			out, err = s.i18nStore().PutTx(ctx, tx, code, key, in.Text, s.actorFor(r).ID)
			return err
		})
	if err != nil {
		s.i18nError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"locale":     out.Locale,
		"key":        out.Key,
		"text":       out.Text,
		"updated_by": out.UpdatedBy,
		"updated_at": out.UpdatedAt.UTC().Format("2006-01-02T15:04:05.000Z07:00"),
	})
}

// handleDeleteTranslation removes one translation, which is how a locale falls back for that key.
func (s *Server) handleDeleteTranslation(w http.ResponseWriter, r *http.Request) {
	code := strings.TrimSpace(r.PathValue("code"))
	key := strings.TrimSpace(r.PathValue("key"))
	err := s.withLocaleAudit(r, "i18n.translation.delete", code+"/"+key, nil,
		func(ctx context.Context, tx pgx.Tx) error {
			return s.i18nStore().DeleteTranslationTx(ctx, tx, code, key)
		})
	if err != nil {
		s.i18nError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ----------------------------------------------------------------- plumbing --

// withLocaleAudit runs a locale write and records it in the same transaction.
//
// The locale code goes in `changes`, not in target_id: that column is a uuid, and a locale's
// primary key is a BCP 47 tag ("pt-BR"). Passing the code as the id type-checks in Go and fails in
// Postgres, which rolls the write back and reports a 500 — the audit trail silently made every
// locale write impossible.
func (s *Server) withLocaleAudit(r *http.Request, action, code string, changes map[string]any,
	fn func(ctx context.Context, tx pgx.Tx) error) error {
	if changes == nil {
		changes = map[string]any{}
	}
	changes["code"] = code
	return s.pool.InTx(r.Context(), func(ctx context.Context, tx pgx.Tx) error {
		if err := fn(ctx, tx); err != nil {
			return err
		}
		ev := s.auditEvent(r, action, "locale", "", audit.OutcomeSuccess).WithChanges(changes)
		return audit.Record(ctx, tx, ev)
	})
}

// localeChanges describes what an update asked to change, without the text of anything.
func localeChanges(in i18n.LocaleInput) map[string]any {
	out := map[string]any{}
	if in.Name != "" {
		out["name"] = in.Name
	}
	if in.NativeName != "" {
		out["native_name"] = in.NativeName
	}
	if in.Enabled != nil {
		out["enabled"] = *in.Enabled
	}
	if in.SortOrder != nil {
		out["sort_order"] = *in.SortOrder
	}
	return out
}

// i18nError maps the locale package's sentinels to stable problem codes.
func (s *Server) i18nError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, i18n.ErrNotFound):
		writeProblem(w, http.StatusNotFound, "not_found", "no such locale or translation")
	case errors.Is(err, i18n.ErrCodeTaken):
		// A duplicate is about one field and the client can say so without parsing a sentence.
		writeFieldProblem(w, http.StatusConflict, "duplicate_locale", err.Error(),
			[]fieldProblem{{Field: "code", Code: "duplicate"}})
	case errors.Is(err, i18n.ErrLocaleNotEnabled):
		// A stable code rather than a validation sentence: the switcher branches on it, and the
		// honest reading is "pick another language", not "your request was malformed".
		writeProblem(w, http.StatusUnprocessableEntity, "locale_not_enabled", err.Error())
	case errors.Is(err, i18n.ErrDefaultLocale):
		// Well-formed request, impossible state. The operator's next action is to make another
		// locale the default, so the message says that rather than restating the rule.
		writeProblem(w, http.StatusConflict, "default_locale",
			"the default locale cannot be removed or left untranslated: make another locale the default first")
	case errors.Is(err, i18n.ErrInvalidInput):
		// The package names the field when it can (see internal/i18n/problems.go). The field list
		// is the translatable half of the refusal: the sentence is English by construction, the
		// field and its code are not.
		fields := make([]fieldProblem, 0, 2)
		for _, p := range i18n.FieldProblems(err) {
			fields = append(fields, fieldProblem{Field: p.Field, Code: p.Code})
		}
		writeFieldProblem(w, http.StatusUnprocessableEntity, "invalid_input", err.Error(), fields)
	default:
		s.cfg.Logger.ErrorContext(r.Context(), "locale operation failed",
			"path", r.URL.Path, "error", err)
		writeProblem(w, http.StatusInternalServerError, "internal_error", "the operation could not be completed")
	}
}
