package presets_test

import (
	"testing"

	"github.com/catalyst/billing/internal/brand"
	"github.com/catalyst/billing/internal/theme"
	"github.com/catalyst/billing/internal/theme/css"
	"github.com/catalyst/billing/internal/theme/presets"
)

func TestCatalogueLoads(t *testing.T) {
	if err := presets.Err(); err != nil {
		t.Fatal(err)
	}
	if got := len(presets.All()); got != 10 {
		t.Fatalf("catalogue has %d presets, want 10", got)
	}
}

func TestEveryPresetResolvesAndRenders(t *testing.T) {
	for _, p := range presets.All() {
		doc, err := presets.Document(p.ID)
		if err != nil {
			t.Fatalf("%s: %v", p.ID, err)
		}
		if _, err := css.Generate(doc); err != nil {
			t.Fatalf("%s: generate: %v", p.ID, err)
		}
	}
}

func TestContrast(t *testing.T) {
	for _, p := range presets.All() {
		doc, err := presets.Document(p.ID)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range []struct {
			name string
			pal  theme.ModePalette
		}{{"dark", doc.Color.Dark}, {"light", doc.Color.Light}} {
			bg, err := brand.ParseHex(m.pal.Bg)
			if err != nil {
				t.Fatalf("%s/%s bg: %v", p.ID, m.name, err)
			}
			for _, c := range []struct {
				field string
				raw   string
				min   float64
			}{
				{"text", m.pal.Text, 4.5},
				{"textMuted", m.pal.TextMuted, 4.5},
				{"textFaint", m.pal.TextFaint, 3.0},
				{"borderStrong", m.pal.BorderStrong, 1.4},
				{"accent", doc.AccentFor(m.name), 3.0},
			} {
				fg, err := brand.ParseHex(c.raw)
				if err != nil {
					t.Fatalf("%s/%s %s: %v", p.ID, m.name, c.field, err)
				}
				if got := brand.Contrast(fg, bg); got < c.min {
					t.Errorf("%s/%s %s contrast %.2f < %.1f", p.ID, m.name, c.field, got, c.min)
				}
			}
		}
	}
}
