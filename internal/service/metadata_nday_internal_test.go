package service

// intervalPhrase is unexported, so its boundary behaviour is asserted from inside the package.
// The DB-backed tests next door prove the end-to-end sentence; this proves the function cannot
// be handed something it should refuse and invent a cycle out of it.

import "testing"

func TestIntervalPhrase(t *testing.T) {
	for _, tc := range []struct {
		interval string
		want     string
	}{
		// The four calendar periods, unchanged.
		{"monthly", "billed monthly"},
		{"quarterly", "billed quarterly"},
		{"semiannual", "billed every six months"},
		{"annual", "billed annually"},
		// Day-count periods: the whole 1..366 range is a real cycle.
		{"every_1_days", "billed every 1 days"},
		{"every_7_days", "billed every 7 days"},
		{"every_45_days", "billed every 45 days"},
		{"every_366_days", "billed every 366 days"},
		// Not a cycle: the two non-recurring kinds, an absent interval, and everything the
		// calendar refuses. None of these may produce a sentence, because a description that
		// names a billing cycle for a one-time purchase is a lie on a support screen.
		{"one_time", ""},
		{"free", ""},
		{"", ""},
		{"every_0_days", ""},
		{"every_367_days", ""},
		{"every_045_days", ""},
		{"every_days", ""},
		{"every_45_day", ""},
		{"weekly", ""},
		{"45 days", ""},
	} {
		if got := intervalPhrase(tc.interval); got != tc.want {
			t.Fatalf("intervalPhrase(%q) = %q, want %q", tc.interval, got, tc.want)
		}
	}
}
