// The tenant's brand overlay over HTTP: read it, write it, upload its logo, and serve the
// resolved brand to the panel (E6.10).
//
// # What this file is, and what it is deliberately not
//
// It is the transport for one brand: a display name, a logo and an accent colour per colour
// mode, layered on top of the two themes the panel already has. It is not a theme registry —
// there is no route here that lists themes, selects one per user, or stores a token set.
// That is a much larger feature (a schema per token, a preview surface, a migration story
// for every token added later) and it is not what a tenant needs in order to look like
// themselves. The boundary is stated here because this is where a reader would expect the
// registry's routes to live.
//
// # The five rules that shape the handlers
//
//  1. **The resolved brand is public and cacheable.** The panel needs the brand before the
//     first screen renders, and the storefront needs it with no session at all, so
//     `GET /api/v1/branding` is unauthenticated. It exposes a display name, a colour and a
//     media URL — the same facts a visitor sees on the storefront's own page. It never
//     exposes a setting it was not asked for: the shape is built from four named keys, not
//     from a settings group dump.
//
//  2. **The accent is resolved here, not in the browser.** The panel decides which theme it
//     is in before it paints, and a brand whose accent depends on a fetch would flash the
//     built-in colour first. So the response carries both accents and the panel picks. It
//     also carries the *derived* ink (the colour text takes on top of the accent) and the
//     contrast verdict, because the same maths is used by the invoice and the two must not
//     disagree about whether a colour is legible.
//
//  3. **A write is a group write.** Name, colours and logo go through
//     `settings.Update`/`UpdateTx`, which validates the whole group before storing any of it
//     and writes one audit row per changed key inside the same transaction. A half-saved
//     brand — a new name with the old logo — is not a state that can exist.
//
//  4. **The logo is an image, and the image is checked.** It is stored through the catalog's
//     image store (digest-deduped, 256 KiB cap, closed content-type set) and referenced by
//     id. An SVG is refused here even though the catalog accepts one for product logos: the
//     invoice PDF cannot draw an SVG, so accepting it would create a brand that renders in
//     the panel and silently disappears from every document. A tenant who uploads one is
//     told why rather than left to notice.
//
//  5. **An empty value means "not configured".** Clearing a field restores the built-in
//     appearance. The handler therefore accepts "" for every key and never substitutes a
//     default — a default that repeated the built-in colour would make "unset"
//     indistinguishable from "set to the default", and the byte-identical fallback could not
//     be tested.
package httpapi

import (
	"bytes"
	"context"
	"errors"
	"image"
	// The two formats the invoice renderer can draw. They are imported for their decoders so
	// that an upload is refused here rather than discovered to be unprintable later.
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"strings"

	"github.com/catalyst/billing/internal/authn"
	"github.com/catalyst/billing/internal/brand"
	"github.com/catalyst/billing/internal/catalog"
	"github.com/catalyst/billing/internal/events"
	"github.com/catalyst/billing/internal/invoicepdf"
	"github.com/catalyst/billing/internal/portal"
	"github.com/catalyst/billing/internal/settings"
	"github.com/jackc/pgx/v5"
)

func init() {
	RegisterRoutes("46-branding", registerBranding)
}

func registerBranding(r *router) {
	// The resolved brand. Public because the storefront is public: a visitor deciding
	// whether to buy sees the tenant's name and colour on the same page, and the panel
	// needs it before it renders a screen. It is a read of four presentation values.
	r.publicGet("/api/v1/branding", r.s.handlePublicBranding)

	// The branding group, for the settings screen. Reading is gated with writing on the same
	// reasoning as the rest of the settings surface: a settings page is one screen.
	r.staffGet("/api/v1/staff/branding", authn.PermSettingsWrite, r.s.handleGetBranding)
	r.staffMutation(http.MethodPut, "/api/v1/staff/branding", authn.PermSettingsWrite,
		r.s.handlePutBranding)

	// The logo. A raw-body upload with the content type in the header, matching the catalog's
	// image upload: one fewer parser in the request path, and the panel has a File in hand.
	r.staffMutation(http.MethodPost, "/api/v1/staff/branding/logo", authn.PermSettingsWrite,
		r.s.handleUploadBrandLogo)
	// Removing the logo is its own mutation rather than "save the form with an empty field",
	// because the two are different intentions: clearing a text field is often a slip, and a
	// logo that vanishes on a slipped save is worse than one that takes a click to remove.
	r.staffMutation(http.MethodDelete, "/api/v1/staff/branding/logo", authn.PermSettingsWrite,
		r.s.handleDeleteBrandLogo)
}

// ---------------------------------------------------------------- the read --

// brandingSettingsGroup is the settings group the brand lives in.
const brandingSettingsGroup = "branding"

// brandResolved is the wire shape of a resolved brand.
//
// Every field is present whether or not it is configured, because a client that has to
// distinguish "absent" from "empty" will get it wrong once and paint a default name. `name`
// is empty when unset, and the client's own fallback is what renders the built-in wordmark —
// which is what makes the fallback testable in one place rather than two.
type brandResolved struct {
	// Name is the tenant's display name, or "" when not configured.
	Name string `json:"name"`
	// AccentLight and AccentDark are the stored accents ("#rrggbb") or "".
	AccentLight string `json:"accent_light"`
	AccentDark  string `json:"accent_dark"`
	// LogoID is the stored image id, or "". LogoURL is derived from it.
	LogoID  string `json:"logo_id"`
	LogoURL string `json:"logo_url"`
	// Resolved carries the derived values a renderer would otherwise have to compute: the
	// ink to use on each accent and whether the accent is legible on its own theme's
	// background. Sending them means the panel and the invoice cannot disagree.
	Resolved brandDerived `json:"resolved"`
	// UpdatedAt is when the group was last written, or nil. It is what a settings screen
	// shows as "last changed", and it lets a cached copy be reasoned about.
	UpdatedAt *string `json:"updated_at"`
}

// brandDerived is the derived colour information, per mode.
type brandDerived struct {
	Light brandModeDerived `json:"light"`
	Dark  brandModeDerived `json:"dark"`
}

// brandModeDerived is one mode's derived colour facts.
type brandModeDerived struct {
	// Ink is the colour text takes on top of the accent ("#rrggbb"). When no accent is
	// configured it is the built-in theme's ink, so a client can use it unconditionally.
	Ink string `json:"ink"`
	// Text is the accent adjusted until it reads on this mode's background. It is what a
	// link or a small label should use when the raw accent would be too faint.
	Text string `json:"text"`
	// Contrast is the raw accent's WCAG ratio against this mode's background.
	Contrast float64 `json:"contrast"`
	// Legible reports whether that ratio reaches the AA threshold for body text.
	Legible bool `json:"legible"`
	// Set reports whether an accent is configured for this mode. A client uses this to
	// decide whether to emit an override at all — emitting the built-in colour would make
	// "unset" indistinguishable from "set to the default".
	Set bool `json:"set"`
}

// The panel's own background and ink per mode, from web/src/styles/tokens.css. They are
// repeated here because the derivation has to be done somewhere the CSS cannot: the invoice
// prints on white paper and the panel paints on --bg, and a single threshold for both would
// be wrong for one of them.
var (
	panelLightBackground = brand.Colour{R: 0xf5, G: 0xf6, B: 0xf8} // --bg, light
	panelDarkBackground  = brand.Colour{R: 0x0a, G: 0x0d, B: 0x13} // --bg, dark
	panelLightInk        = brand.Colour{R: 0xff, G: 0xff, B: 0xff} // --accent-ink, light
	panelDarkInk         = brand.Colour{R: 0x0a, G: 0x0d, B: 0x13} // --accent-ink, dark
)

// deriveMode computes the derived facts for one mode.
func deriveMode(stored string, background, builtinInk brand.Colour) brandModeDerived {
	accent, ok := parseStoredAccent(stored)
	if !ok {
		return brandModeDerived{
			Ink:      builtinInk.Hex(),
			Text:     "",
			Contrast: 0,
			Legible:  true,
			Set:      false,
		}
	}
	ratio := brand.Contrast(accent, background)
	return brandModeDerived{
		Ink:      brand.Ink(accent).Hex(),
		Text:     brand.Readable(accent, background).Hex(),
		Contrast: round2(ratio),
		Legible:  ratio >= brand.MinTextContrast,
		Set:      true,
	}
}

// round2 rounds to two decimals so the JSON is stable and readable.
func round2(v float64) float64 {
	return float64(int64(v*100+0.5)) / 100
}

// parseStoredAccent reads a stored accent, reporting false for "" and for anything malformed.
func parseStoredAccent(raw string) (brand.Colour, bool) {
	if strings.TrimSpace(raw) == "" {
		return brand.Colour{}, false
	}
	c, err := brand.ParseHex(raw)
	if err != nil {
		return brand.Colour{}, false
	}
	return c, true
}

// brandFromSettings builds the resolved shape from a settings group read.
func brandFromSettings(values []settings.Value) brandResolved {
	out := brandResolved{}
	for _, v := range values {
		switch v.Key {
		case "name":
			out.Name = v.Value
		case "accent_light":
			out.AccentLight = v.Value
		case "accent_dark":
			out.AccentDark = v.Value
		case "logo_id":
			out.LogoID = v.Value
		}
		if v.UpdatedAt != nil {
			ts := v.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z07:00")
			// The newest write in the group is the group's timestamp: one brand is one fact.
			if out.UpdatedAt == nil || ts > *out.UpdatedAt {
				out.UpdatedAt = &ts
			}
		}
	}
	if out.LogoID != "" {
		out.LogoURL = "/api/v1/media/" + out.LogoID
	}
	out.Resolved = brandDerived{
		Light: deriveMode(out.AccentLight, panelLightBackground, panelLightInk),
		Dark:  deriveMode(out.AccentDark, panelDarkBackground, panelDarkInk),
	}
	return out
}

// loadBrand reads the brand group.
//
// A missing database is not an error a caller has to handle: an installation with no
// database has no brand, and the panel falls back to its built-in appearance. That is the
// same answer a fresh installation gives.
func (s *Server) loadBrand(ctx context.Context) (brandResolved, error) {
	if s.pool == nil {
		return brandFromSettings(nil), nil
	}
	values, err := settings.New(s.pool).Group(ctx, brandingSettingsGroup)
	if err != nil {
		return brandResolved{}, err
	}
	return brandFromSettings(values), nil
}

// handlePublicBranding serves the resolved brand.
//
// The cache is short and public. A brand change should be visible within a minute, and the
// response carries nothing an unauthenticated visitor cannot already read off the storefront
// page it is standing on.
func (s *Server) handlePublicBranding(w http.ResponseWriter, r *http.Request) {
	out, err := s.loadBrand(r.Context())
	if err != nil {
		s.cfg.Logger.ErrorContext(r.Context(), "read branding", "error", err)
		writeProblem(w, http.StatusInternalServerError, "internal_error",
			"the request could not be completed")
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=60")
	writeJSON(w, http.StatusOK, out)
}

// handleGetBranding serves the same shape to the settings screen.
//
// It is the public shape plus the group as the settings API would render it, so the screen
// does not need two requests and cannot show a form whose values disagree with the resolved
// brand beside it.
func (s *Server) handleGetBranding(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "database_not_configured",
			"no database is configured")
		return
	}
	values, err := settings.New(s.pool).Group(r.Context(), brandingSettingsGroup)
	if err != nil {
		s.settingsError(w, r, err)
		return
	}
	// `no-store`: a settings screen that showed a cached value would show one operator
	// another operator's last save.
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{
		"brand":  brandFromSettings(values),
		"group":  settingsGroupJSON(settings.GroupValues{Group: brandingSettingsGroup, Keys: values}),
		"limits": brandLimits(),
	})
}

// brandLimits reports the bounds the server enforces, so the screen can state them rather
// than discover them through a refused save.
func brandLimits() map[string]any {
	return map[string]any{
		"name_max":          settings.MaxBrandName,
		"logo_max_bytes":    catalog.MaxImageBytes,
		"logo_types":        brandLogoTypes(),
		"min_text_contrast": brand.MinTextContrast,
	}
}

// brandLogoTypes is the closed set a brand logo may be.
//
// PNG and JPEG only. The catalog also accepts WebP and SVG for product logos, and both are
// wrong here: the invoice renderer can draw neither, so a WebP logo would render in the
// panel and vanish from every document, and an SVG is a document that can carry a script.
// A brand that is half-applied is worse than one that is refused with a reason.
func brandLogoTypes() []string { return []string{"image/png", "image/jpeg"} }

// ---------------------------------------------------------------- the write --

// brandPutBody is the group write.
//
// Pointers would be the usual way to distinguish "clear this field" from "leave it alone",
// but the settings store is a *group* write: the form posts the group, and a missing key
// means "leave it". An explicit empty string therefore has to mean "clear", which is exactly
// what `*string` with a nil check would express — so a plain map is used and the presence of
// the key is the intent.
type brandPutBody struct {
	Values map[string]string `json:"values"`
}

// brandWritableKeys is the closed set a brand write may touch. A client that sends
// `general.company_name` is refused rather than silently ignored: the settings store already
// rejects an unknown key, and this makes the message name the field.
var brandWritableKeys = []string{"name", "accent_light", "accent_dark"}

// handlePutBranding writes the brand group.
func (s *Server) handlePutBranding(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "database_not_configured",
			"no database is configured")
		return
	}
	var body brandPutBody
	if !s.decodeJSON(w, r, &body, 64<<10) {
		return
	}
	if body.Values == nil {
		writeProblem(w, http.StatusBadRequest, "values_required",
			"the request must carry the values to write")
		return
	}
	// The logo is not writable through this route. It is set by an upload and cleared by a
	// delete, so that pointing the brand at an arbitrary image id is not a thing a form can
	// do by typing.
	for key := range body.Values {
		if key == "logo_id" {
			writeProblem(w, http.StatusBadRequest, "logo_not_writable",
				"the logo is set by uploading it, not by naming an image")
			return
		}
	}
	// Normalise before validating: the settings definition accepts "#abc" and "#ABC" so an
	// operator is not fought by a form, and the stored value is always the six-digit
	// lower-case form so the panel can compare it against its own token.
	for _, key := range brandWritableKeys {
		value, ok := body.Values[key]
		if !ok {
			continue
		}
		if key == "name" {
			body.Values[key] = strings.TrimSpace(value)
			continue
		}
		if strings.TrimSpace(value) == "" {
			body.Values[key] = ""
			continue
		}
		parsed, err := brand.ParseHex(value)
		if err != nil {
			writeProblem(w, http.StatusUnprocessableEntity, "invalid_value",
				key+" must be a hex colour such as #3b5bdb, or empty")
			return
		}
		body.Values[key] = parsed.Hex()
	}

	user, _ := userFrom(r.Context())
	values, err := settings.New(s.pool).Update(r.Context(), brandingSettingsGroup, body.Values,
		user.ID, s.auditorFor(r, ""))
	if err != nil {
		s.settingsError(w, r, err)
		return
	}

	// Publishing after commit is what lets another operator's open branding screen know it
	// is stale. The event carries no value: it is a notification, not a copy of the brand.
	s.publish(events.Event{
		Kind:       "branding.updated",
		Permission: string(authn.PermSettingsWrite),
		Data:       map[string]any{"group": brandingSettingsGroup},
	})

	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{
		"brand": brandFromSettings(values),
		"group": settingsGroupJSON(settings.GroupValues{Group: brandingSettingsGroup, Keys: values}),
	})
}

// --------------------------------------------------------------- the logo --

// handleUploadBrandLogo stores an image and points the brand at it, in one transaction.
//
// The image and the reference are one fact: a failure between them would leave a screen
// showing a logo id that names nothing, which reads as "the upload worked and then the logo
// broke". `UpdateTx` runs the settings write inside the same transaction as the image insert.
func (s *Server) handleUploadBrandLogo(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "database_not_configured",
			"no database is configured")
		return
	}
	contentType := strings.TrimSpace(strings.Split(r.Header.Get("Content-Type"), ";")[0])
	if contentType == "" {
		writeProblem(w, http.StatusBadRequest, "invalid_body", "a Content-Type is required")
		return
	}
	if !isBrandLogoType(contentType) {
		writeProblem(w, http.StatusUnsupportedMediaType, "unsupported_image_type",
			"a brand logo must be a PNG or JPEG: those are the formats an invoice can print")
		return
	}
	// One byte over the cap so the reader can tell "exactly at the limit" from "over it".
	body, err := io.ReadAll(io.LimitReader(r.Body, catalog.MaxImageBytes+1))
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_body", "the upload could not be read")
		return
	}
	if len(body) > catalog.MaxImageBytes {
		writeProblem(w, http.StatusRequestEntityTooLarge, "image_too_large",
			"the image exceeds 256 KiB")
		return
	}
	// Decode before storing. A file with a PNG content type and no PNG inside it is a file
	// the invoice renderer will refuse, and "the upload succeeded and the logo is invisible"
	// is the worst possible outcome. Decoding here is a few kilobytes of work on a 256 KiB
	// cap and turns that into a refusal with a reason.
	if _, format, derr := image.DecodeConfig(bytes.NewReader(body)); derr != nil {
		writeProblem(w, http.StatusUnprocessableEntity, "invalid_image",
			"the upload is not an image this server can read")
		return
	} else if !isBrandLogoFormat(format) {
		writeProblem(w, http.StatusUnsupportedMediaType, "unsupported_image_type",
			"a brand logo must be a PNG or JPEG: those are the formats an invoice can print")
		return
	}

	user, _ := userFrom(r.Context())
	var values []settings.Value
	var img catalog.Image
	err = s.pool.InTx(r.Context(), func(ctx context.Context, tx pgx.Tx) error {
		var err error
		img, err = s.catalogStore().PutImageTx(ctx, tx, contentType, body)
		if err != nil {
			return err
		}
		values, err = settings.New(s.pool).UpdateTx(ctx, tx, brandingSettingsGroup,
			map[string]string{"logo_id": img.ID}, user.ID, s.auditorFor(r, ""))
		return err
	})
	if err != nil {
		s.brandError(w, r, err)
		return
	}

	s.publish(events.Event{
		Kind:       "branding.updated",
		Permission: string(authn.PermSettingsWrite),
		Data:       map[string]any{"group": brandingSettingsGroup},
	})

	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusCreated, map[string]any{
		"id":           img.ID,
		"content_type": img.ContentType,
		"byte_size":    img.ByteSize,
		"url":          "/api/v1/media/" + img.ID,
		"brand":        brandFromSettings(values),
	})
}

// handleDeleteBrandLogo clears the reference.
//
// The image row is deliberately kept. It is deduped by digest and shared with the catalog, so
// deleting it could break a product logo that happens to be the same file; and "remove the
// logo from the brand" is not "destroy the image".
func (s *Server) handleDeleteBrandLogo(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "database_not_configured",
			"no database is configured")
		return
	}
	user, _ := userFrom(r.Context())
	values, err := settings.New(s.pool).Update(r.Context(), brandingSettingsGroup,
		map[string]string{"logo_id": ""}, user.ID, s.auditorFor(r, ""))
	if err != nil {
		s.settingsError(w, r, err)
		return
	}
	s.publish(events.Event{
		Kind:       "branding.updated",
		Permission: string(authn.PermSettingsWrite),
		Data:       map[string]any{"group": brandingSettingsGroup},
	})
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{
		"brand": brandFromSettings(values),
		"group": settingsGroupJSON(settings.GroupValues{Group: brandingSettingsGroup, Keys: values}),
	})
}

// isBrandLogoType reports whether a declared content type is one a brand logo may be.
func isBrandLogoType(contentType string) bool {
	for _, allowed := range brandLogoTypes() {
		if contentType == allowed {
			return true
		}
	}
	return false
}

// isBrandLogoFormat reports whether a decoded format name is one a brand logo may be.
// `image.DecodeConfig` names the format rather than echoing the header, so this catches a
// PNG declared as a JPEG and an SVG declared as anything.
func isBrandLogoFormat(format string) bool {
	return format == "png" || format == "jpeg"
}

// brandError maps the errors an upload can produce to stable codes.
func (s *Server) brandError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, catalog.ErrImageTooLarge):
		writeProblem(w, http.StatusRequestEntityTooLarge, "image_too_large",
			"the image exceeds 256 KiB")
	case errors.Is(err, catalog.ErrBadImageType):
		writeProblem(w, http.StatusUnsupportedMediaType, "unsupported_image_type",
			"a brand logo must be a PNG or JPEG: those are the formats an invoice can print")
	case errors.Is(err, catalog.ErrInvalidInput):
		writeProblem(w, http.StatusUnprocessableEntity, "invalid_image", "the upload is empty")
	case errors.Is(err, settings.ErrInvalidValue), errors.Is(err, settings.ErrUnknownKey):
		s.settingsError(w, r, err)
	default:
		s.cfg.Logger.ErrorContext(r.Context(), "brand logo upload failed",
			"path", r.URL.Path, "error", err)
		writeProblem(w, http.StatusInternalServerError, "internal_error",
			"the request could not be completed")
	}
}

// ------------------------------------------------------ the invoice's copy --

// invoiceBrand reads the brand for a document renderer.
//
// It returns the zero value on any failure. A document is owed to a customer whether or not
// the branding row is readable, so a broken brand must produce the unbranded invoice rather
// than a 500 — the same reasoning that makes a corrupt logo print no logo.
//
// The logo is loaded as bytes here rather than in the renderer because the renderer has no
// database, and the bytes are bounded by the upload cap (256 KiB) so this is one small read
// on a path that already reads a snapshot.
func (s *Server) invoiceBrand(ctx context.Context) invoicepdf.Brand {
	if s.pool == nil {
		return invoicepdf.Brand{}
	}
	values, err := settings.New(s.pool).Group(ctx, brandingSettingsGroup)
	if err != nil {
		s.cfg.Logger.WarnContext(ctx, "read branding for a document", "error", err)
		return invoicepdf.Brand{}
	}
	resolved := brandFromSettings(values)
	out := invoicepdf.Brand{Name: resolved.Name}

	// The document is printed on white paper, so the light accent is the one that belongs on
	// it: the dark accent exists because a pale colour is invisible on a near-black panel,
	// which is not a problem paper has. If only the dark accent is configured it is used
	// anyway — the renderer darkens it until it reads, so it prints as a legible relative of
	// the tenant's colour rather than as nothing.
	if accent, ok := parseStoredAccent(resolved.AccentLight); ok {
		out.Accent = accent
	} else if accent, ok := parseStoredAccent(resolved.AccentDark); ok {
		out.Accent = accent
	}

	if resolved.LogoID != "" && portal.ValidID(resolved.LogoID) {
		img, err := s.catalogStore().Image(ctx, resolved.LogoID)
		if err != nil {
			// A dangling reference is "no logo", not a failed document. The brand still
			// applies: one broken field is not a broken brand.
			s.cfg.Logger.WarnContext(ctx, "read brand logo for a document",
				"logo_id", resolved.LogoID, "error", err)
			return out
		}
		out.Logo = &invoicepdf.Logo{ContentType: img.ContentType, Bytes: img.Bytes}
	}
	return out
}
