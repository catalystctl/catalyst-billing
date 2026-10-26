// Package presets is the embedded catalogue of starting points for a theme.
//
// # A preset is a starting point, not a mode
//
// Applying a preset creates a *draft revision* that the operator then edits and publishes
// (docs/theming-system.md §3.2). Nothing in this package publishes, and no preset is a value a
// document can point at when it is being rendered: `extends` is resolved at draft time, so a
// later change to a preset cannot mutate a theme somebody already published.
//
// # Why JSON files in the binary
//
// The same reason the frontend is embedded: a preset that arrived from a file at runtime would be
// a second deployment artifact that could drift from the binary that reads it. `go:embed` means
// `billing serve` works with no extra directory, and the export envelope (§4.3) can round-trip a
// preset an operator saved.
//
// # Why they are validated at startup
//
// A preset is compiled-in data, so a broken one is a build error, not a runtime condition. The
// decode below is a `sync.Once` rather than an `init` panic because a panic at init makes the
// whole binary unusable for a problem that only affects one screen — but `presets_test.go`
// validates every file, so the failure is caught by `go test` rather than by an operator.
package presets

import (
	"embed"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/catalyst/billing/internal/theme"
)

//go:embed *.json
var files embed.FS

// catalogueOrder is the order the gallery shows presets in.
//
// It is a list rather than alphabetical because the order is a curation decision: the two
// Catalyst presets first (they are what most operators want), then the structurally different
// designs from the least to the most unfamiliar.
var catalogueOrder = []string{
	"catalyst-console",
	"catalyst-billing",
	"catalyst-bronze",
	"brutalist",
	"editorial-paper",
	"neon-terminal",
	"glass-aurora",
	"warm-saas",
	"monochrome-studio",
	"midnight-luxe",
}

// Meta is the gallery's description of a preset.
//
// It is derived from the document where it can be (name, description, mode, radius, density) and
// declared here where it cannot (the one-line "character" the gallery shows on a card).
type Meta struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	// Character is the short label under the card: what makes this preset structurally different
	// from the others, not what colour it is.
	Character string `json:"character"`
	// Mode is the preset's default colour mode.
	Mode string `json:"mode"`
	// Radius and Density are the values that change the *shape* rather than the hue, so the
	// gallery can show that a preset is more than a recolour.
	Radius  string `json:"radius"`
	Density string `json:"density"`
	// Nav is the shell variant.
	Nav string `json:"nav"`
	// Swatch is a small set of colours the card renders: ground, card, text, accent.
	Swatch []string `json:"swatch"`
}

// character is the per-preset label, kept out of the JSON document because it is gallery copy
// rather than a theme token.
var character = map[string]string{
	"catalyst-console":  "Dense tables, flat cards, thin borders",
	"catalyst-billing":  "Warm and dense: the operator default",
	"catalyst-bronze":   "Warm bronze on ink, teal secondary",
	"brutalist":         "No radius, no blur, hard offset shadows",
	"editorial-paper":   "Serif headlines, rules instead of cards",
	"neon-terminal":     "Monospace everywhere, glowing focus",
	"glass-aurora":      "Frosted panels over a gradient mesh",
	"warm-saas":         "Rounded pills, pastel, gentle elevation",
	"monochrome-studio": "Grayscale, one signal colour, strong type",
	"midnight-luxe":     "Navy and gold, serif, spacious",
}

// Preset is one catalogue entry.
type Preset struct {
	Meta
	// Document is the preset as authored: a *layer*, to be resolved over theme.Default().
	Document theme.Document
}

var (
	loadOnce sync.Once
	loaded   []Preset
	loadErr  error
)

// All returns the catalogue in gallery order.
func All() []Preset {
	loadOnce.Do(load)
	return loaded
}

// Get returns a preset by id.
func Get(id string) (Preset, bool) {
	for _, p := range All() {
		if p.ID == id {
			return p, true
		}
	}
	return Preset{}, false
}

// Catalogue returns the gallery metadata only, which is what the studio's list endpoint needs.
func Catalogue() []Meta {
	all := All()
	out := make([]Meta, 0, len(all))
	for _, p := range all {
		out = append(out, p.Meta)
	}
	return out
}

// Document returns a preset resolved over the built-in default: a complete, renderable document.
//
// This is what "apply this preset as a draft" stores. Resolving here rather than at render time is
// the rule from merge.go: a published revision must not change when something it inherited from
// changes.
func Document(id string) (theme.Document, error) {
	p, ok := Get(id)
	if !ok {
		return theme.Document{}, fmt.Errorf("theme: no preset %q", id)
	}
	doc, err := theme.Resolve(theme.Default(), p.Document)
	if err != nil {
		return theme.Document{}, err
	}
	if err := theme.ValidateResolved(doc); err != nil {
		return theme.Document{}, fmt.Errorf("theme: preset %q is not renderable: %w", id, err)
	}
	return doc, nil
}

// load decodes and validates every embedded preset.
func load() {
	entries, err := files.ReadDir(".")
	if err != nil {
		loadErr = fmt.Errorf("theme/presets: read the embedded catalogue: %w", err)
		return
	}
	byID := make(map[string]Preset, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		raw, err := files.ReadFile(entry.Name())
		if err != nil {
			loadErr = fmt.Errorf("theme/presets: read %s: %w", entry.Name(), err)
			return
		}
		doc, err := theme.Decode(raw)
		if err != nil {
			loadErr = fmt.Errorf("theme/presets: %s: %w", entry.Name(), err)
			return
		}
		if got, want := entry.Name(), doc.ID+".json"; got != want {
			loadErr = fmt.Errorf("theme/presets: %s declares id %q; the file must be named %s",
				got, doc.ID, want)
			return
		}
		byID[doc.ID] = Preset{Meta: metaFor(doc), Document: doc}
	}

	// The catalogue order is authoritative, and a preset missing from it is a preset nobody can
	// find — which is a silent omission, so it fails here instead.
	if len(byID) != len(catalogueOrder) {
		loadErr = fmt.Errorf("theme/presets: %d presets embedded but %d in the catalogue order",
			len(byID), len(catalogueOrder))
		return
	}
	for _, id := range catalogueOrder {
		p, ok := byID[id]
		if !ok {
			loadErr = fmt.Errorf("theme/presets: catalogue order names %q, which is not embedded", id)
			return
		}
		loaded = append(loaded, p)
		delete(byID, id)
	}
	if len(byID) > 0 {
		leftovers := make([]string, 0, len(byID))
		for id := range byID {
			leftovers = append(leftovers, id)
		}
		sort.Strings(leftovers)
		loadErr = fmt.Errorf("theme/presets: embedded but not in the catalogue order: %v", leftovers)
	}
}

// metaFor derives the gallery metadata from a document.
func metaFor(doc theme.Document) Meta {
	mode := doc.Color.Mode
	if mode == "" {
		mode = "dark"
	}
	palette := doc.Color.Dark
	if mode == "light" {
		palette = doc.Color.Light
	}
	accent := palette.Accent
	if accent == "" {
		accent = doc.Color.Brand.Primary
	}
	return Meta{
		ID:          doc.ID,
		Name:        doc.Name,
		Description: doc.Description,
		Character:   character[doc.ID],
		Mode:        mode,
		Radius:      doc.Shape.Radius.MD,
		Density:     doc.Space.Density,
		Nav:         doc.Layout.Shell.Nav,
		Swatch:      []string{palette.Bg, palette.Card, palette.Text, accent},
	}
}

// Err reports why the embedded catalogue could not be loaded, if it could not.
//
// It is exported so `billing doctor` and the studio's health card can report a broken build
// rather than showing an empty gallery.
func Err() error {
	All()
	return loadErr
}
