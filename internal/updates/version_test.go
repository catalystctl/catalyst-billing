package updates_test

import (
	"testing"
	"time"

	"github.com/catalyst/billing/internal/updates"
)

// Version parsing and ordering.
//
// These are pure and run without a database, because they are the part of the update check
// that decides whether an operator is told "up to date" or "1.5.0 available". Getting the
// comparison wrong is how a panel tells someone they are current when they are not.

func TestParseVersion(t *testing.T) {
	cases := []struct {
		in        string
		wantMajor int
		wantMinor int
		wantPatch int
		wantPre   []string
		wantErr   bool
	}{
		{in: "1.2.3", wantMajor: 1, wantMinor: 2, wantPatch: 3},
		{in: "v1.2.3", wantMajor: 1, wantMinor: 2, wantPatch: 3},
		{in: " 2.0.0 ", wantMajor: 2},
		{in: "1.5.0-rc.1", wantMajor: 1, wantMinor: 5, wantPre: []string{"rc", "1"}},
		{in: "1.5.0+build.7", wantMajor: 1, wantMinor: 5},
		{in: "1.5.0-rc.1+sha.abc", wantMajor: 1, wantMinor: 5, wantPre: []string{"rc", "1"}},
		{in: "dev", wantErr: true},
		{in: "", wantErr: true},
		{in: "1.2", wantErr: true},
		{in: "1.2.3.4", wantErr: true},
		{in: "01.2.3", wantErr: true},
		{in: "1.2.x", wantErr: true},
		{in: "1.2.3-", wantErr: true},
		{in: "-1.2.3", wantErr: true},
	}
	for _, tc := range cases {
		got, err := updates.ParseVersion(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("ParseVersion(%q) = %v, want an error", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseVersion(%q): %v", tc.in, err)
			continue
		}
		if got.Major != tc.wantMajor || got.Minor != tc.wantMinor || got.Patch != tc.wantPatch {
			t.Errorf("ParseVersion(%q) = %d.%d.%d, want %d.%d.%d",
				tc.in, got.Major, got.Minor, got.Patch, tc.wantMajor, tc.wantMinor, tc.wantPatch)
		}
		if len(got.Pre) != len(tc.wantPre) {
			t.Errorf("ParseVersion(%q) pre = %v, want %v", tc.in, got.Pre, tc.wantPre)
		}
	}
}

// The ordering that decides the answer. A pre-release is older than its release, which is
// what keeps a beta build from being reported as newer than the release it leads to.
func TestVersionCompare(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"1.0.0", "1.0.0", 0},
		{"v1.0.0", "1.0.0", 0},
		{"1.0.0+build.1", "1.0.0+build.2", 0},
		{"1.0.0", "1.0.1", -1},
		{"1.0.1", "1.0.0", 1},
		{"1.9.0", "1.10.0", -1},
		{"2.0.0", "1.99.99", 1},
		{"1.5.0-rc.1", "1.5.0", -1},
		{"1.5.0", "1.5.0-rc.1", 1},
		{"1.5.0-rc.1", "1.5.0-rc.2", -1},
		{"1.5.0-alpha", "1.5.0-beta", -1},
		{"1.5.0-alpha", "1.5.0-alpha.1", -1},
		// A numeric identifier sorts below an alphanumeric one, per semantic versioning.
		{"1.5.0-1", "1.5.0-alpha", -1},
	}
	for _, tc := range cases {
		a, err := updates.ParseVersion(tc.a)
		if err != nil {
			t.Fatalf("parse %q: %v", tc.a, err)
		}
		b, err := updates.ParseVersion(tc.b)
		if err != nil {
			t.Fatalf("parse %q: %v", tc.b, err)
		}
		if got := a.Compare(b); got != tc.want {
			t.Errorf("%s.Compare(%s) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
		// The ordering must be antisymmetric, or "a newer than b" and "b newer than a"
		// could both be reported.
		if got := b.Compare(a); got != -tc.want {
			t.Errorf("%s.Compare(%s) = %d, want %d", tc.b, tc.a, got, -tc.want)
		}
	}
}

// The outcome is the sentence the panel shows, so each case is asserted by name.
func TestBuildResultOutcomes(t *testing.T) {
	manifest := func(v string) updates.Manifest {
		return updates.Manifest{Version: v, DownloadURL: "https://example.test/billing-" + v}
	}
	cases := []struct {
		running string
		latest  string
		want    updates.Outcome
	}{
		{"1.4.0", "1.5.0", updates.OutcomeAvailable},
		{"1.5.0", "1.5.0", updates.OutcomeUpToDate},
		{"1.6.0", "1.5.0", updates.OutcomeAhead},
		// A development build has no comparable version, and claiming "up to date" would be
		// a lie rather than a convenience.
		{"dev", "1.5.0", updates.OutcomeUnknown},
		{"test", "1.5.0", updates.OutcomeUnknown},
	}
	for _, tc := range cases {
		got := updates.BuildResult(tc.running, "stable", "https://example.test/updates.json",
			manifest(tc.latest), time.Now().UTC())
		if got.Outcome != tc.want {
			t.Errorf("running %q, latest %q: outcome %q, want %q", tc.running, tc.latest, got.Outcome, tc.want)
		}
		if got.RunningVersion != tc.running {
			t.Errorf("running version reported as %q, want %q", got.RunningVersion, tc.running)
		}
		if got.LatestVersion != tc.latest {
			t.Errorf("latest version reported as %q, want %q", got.LatestVersion, tc.latest)
		}
	}
}

// A manifest version with a leading v is reported without it, so the screen and the approval
// compare the same strings.
func TestBuildResultStripsLeadingV(t *testing.T) {
	got := updates.BuildResult("1.4.0", "stable", "https://example.test/u.json",
		updates.Manifest{Version: "v1.5.0"}, time.Now().UTC())
	if got.LatestVersion != "1.5.0" {
		t.Fatalf("latest version = %q, want 1.5.0", got.LatestVersion)
	}
	if got.Outcome != updates.OutcomeAvailable {
		t.Fatalf("outcome = %q, want available", got.Outcome)
	}
}

// An approval must be a version that parses and is newer than the running build, so the screen
// cannot show "approved" for something that means nothing.
func TestValidateApproval(t *testing.T) {
	if _, err := updates.ValidateApproval("1.4.0", "1.5.0"); err != nil {
		t.Fatalf("approving a newer version: %v", err)
	}
	if _, err := updates.ValidateApproval("1.4.0", "v1.5.0"); err != nil {
		t.Fatalf("approving a newer version with a v prefix: %v", err)
	}
	if _, err := updates.ValidateApproval("1.4.0", "1.4.0"); err == nil {
		t.Fatal("approving the running version should be refused: it is not an update")
	}
	if _, err := updates.ValidateApproval("1.4.0", "1.3.0"); err == nil {
		t.Fatal("approving an older version should be refused")
	}
	if _, err := updates.ValidateApproval("1.4.0", "latest"); err == nil {
		t.Fatal("approving a non-version should be refused")
	}
	// A development build has nothing to compare with, so approving a release is the normal
	// case rather than a refusal.
	if _, err := updates.ValidateApproval("dev", "1.5.0"); err != nil {
		t.Fatalf("approving from a development build: %v", err)
	}
}
