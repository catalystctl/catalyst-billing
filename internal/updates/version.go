// Package updates reports the running build's version, checks a configured endpoint for a
// newer one, and records the version an operator has approved.
//
// # What this package deliberately does not do
//
// It does not replace the running binary. The deployment is a plain executable started by
// whatever supervises it, with the frontend embedded and the schema applied by a separate
// `billing migrate` run under a different database role (see compose.yaml, the Makefile and
// docs/operations.md). Nothing in this repository signs a release, and nothing here could
// restart the process. A "self-update" would therefore be: fetch bytes over the network
// with no signature to check them against, overwrite the executable, and trust that a
// restart happens. That is a remote code execution feature wearing an update button, and it
// would be the single most dangerous route in the panel.
//
// So an update is two facts and a procedure: the running version, the version that is
// available, and the version an operator has *approved*. Applying it is the deployment's
// job — pull the artifact, `billing migrate`, restart `serve` and `worker` — and the API
// says exactly that instead of pretending otherwise.
//
// # What it does do
//
//   - Parses and compares semantic versions without a dependency, including the
//     pre-release ordering that makes 1.5.0-rc1 older than 1.5.0.
//   - Fetches `updates.check_url` with a bounded timeout and a bounded body, because a
//     compromised or broken update server must not be able to exhaust the panel's memory or
//     hold a request open.
//   - Caches the answer in the database (`update_check`), because a check on every page
//     load is a denial of service against the update server and against us. The cache is
//     shared across processes and survives a restart, unlike an in-memory one.
//   - Caches a *failure* briefly, so a broken update server is not hammered either.
//   - Records an approval (`update_approval`) and reports the honest state, including that
//     a development build ("dev") cannot be compared with a release at all.
package updates

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// ErrInvalidVersion is a string that is not a semantic version this package understands.
var ErrInvalidVersion = errors.New("updates: invalid version")

// buildVersion is the running binary's version, stamped by cmd/billing at startup.
//
// It exists because the version is a property of the *process*, and two things in it need to
// read it without being handed it: the update check the API serves (that path is given the
// version in its Config) and the daily telemetry job, which runs in the worker — a `package main`
// that no library can import. Before this the telemetry payload could only be built by an HTTP
// handler, which is why the daily delivery did not exist.
//
// The default is "dev", the same value the linker stamp defaults to, so a build that forgets to
// call SetBuildVersion reports honestly rather than claiming a release.
var buildVersion = "dev"

// SetBuildVersion records the running binary's version. It is called once, from main.
func SetBuildVersion(v string) {
	if v = strings.TrimSpace(v); v != "" {
		buildVersion = v
	}
}

// BuildVersion returns the running binary's version.
func BuildVersion() string { return buildVersion }

// Version is a parsed semantic version.
//
// It is a small, deliberately strict parser: `MAJOR.MINOR.PATCH` with an optional
// `-PRERELEASE` and an optional `+BUILD`. A leading `v` is accepted because release tags
// almost always carry one, and it is stripped rather than stored so that "v1.2.3" and
// "1.2.3" cannot compare as different versions of the same release.
type Version struct {
	Major int
	Minor int
	Patch int
	// Pre is the dot-separated pre-release identifiers, e.g. ["rc", "1"] for 1.5.0-rc.1.
	// Empty for a release version.
	Pre []string
	// Raw is the string the caller supplied, without the leading "v". It is what the API
	// reports back, so the panel shows the version the release actually calls itself.
	Raw string
}

// ParseVersion parses a semantic version.
//
// A build with no version stamp reports "dev" (see cmd/billing); that is not an error a
// caller should crash on, it is a fact the update check reports as "cannot compare". So the
// error is returned and the caller decides — `Check` treats an unparseable *running* version
// as an unknown outcome rather than a failure, and an unparseable *published* version as a
// broken manifest.
func ParseVersion(s string) (Version, error) {
	raw := strings.TrimSpace(s)
	raw = strings.TrimPrefix(raw, "v")
	raw = strings.TrimPrefix(raw, "V")
	if raw == "" {
		return Version{}, fmt.Errorf("%w: empty", ErrInvalidVersion)
	}

	// Build metadata is ignored for ordering: 1.2.3+build.5 and 1.2.3+build.6 are the same
	// release. It is dropped before parsing so a build number can never affect a comparison.
	body := raw
	if i := strings.IndexByte(body, '+'); i >= 0 {
		body = body[:i]
	}
	pre := []string(nil)
	if i := strings.IndexByte(body, '-'); i >= 0 {
		preText := body[i+1:]
		body = body[:i]
		if preText == "" {
			return Version{}, fmt.Errorf("%w: %q has an empty pre-release", ErrInvalidVersion, s)
		}
		for _, part := range strings.Split(preText, ".") {
			if part == "" {
				return Version{}, fmt.Errorf("%w: %q has an empty pre-release identifier", ErrInvalidVersion, s)
			}
			pre = append(pre, part)
		}
	}

	parts := strings.Split(body, ".")
	if len(parts) != 3 {
		return Version{}, fmt.Errorf("%w: %q must be MAJOR.MINOR.PATCH", ErrInvalidVersion, s)
	}
	nums := make([]int, 3)
	for i, part := range parts {
		// No leading zeroes: "01" is a typo or an attempt to make two strings that look
		// equal compare unequal. Rejecting it keeps the comparison unambiguous.
		if len(part) > 1 && part[0] == '0' {
			return Version{}, fmt.Errorf("%w: %q has a leading zero", ErrInvalidVersion, s)
		}
		n, err := strconv.Atoi(part)
		if err != nil || n < 0 {
			return Version{}, fmt.Errorf("%w: %q is not a number", ErrInvalidVersion, s)
		}
		nums[i] = n
	}

	return Version{
		Major: nums[0], Minor: nums[1], Patch: nums[2],
		Pre: pre, Raw: raw,
	}, nil
}

// IsZero reports whether v is the zero value, i.e. was never parsed.
func (v Version) IsZero() bool { return v.Raw == "" }

// IsPrerelease reports whether this is a pre-release build.
func (v Version) IsPrerelease() bool { return len(v.Pre) > 0 }

// String renders the version the way it was supplied, minus any leading "v".
func (v Version) String() string { return v.Raw }

// Compare orders two versions: -1 when v is older than other, 0 when they are the same
// release, +1 when v is newer.
//
// Pre-release ordering follows semantic versioning: 1.5.0-rc.1 < 1.5.0-rc.2 < 1.5.0, and a
// numeric identifier sorts below an alphanumeric one. That matters here because the panel
// offers a "beta" channel, and a beta build must not be reported as newer than the release
// it leads up to.
func (v Version) Compare(other Version) int {
	if c := compareInt(v.Major, other.Major); c != 0 {
		return c
	}
	if c := compareInt(v.Minor, other.Minor); c != 0 {
		return c
	}
	if c := compareInt(v.Patch, other.Patch); c != 0 {
		return c
	}
	switch {
	case len(v.Pre) == 0 && len(other.Pre) == 0:
		return 0
	case len(v.Pre) == 0:
		// A release is newer than any pre-release of the same number.
		return 1
	case len(other.Pre) == 0:
		return -1
	}
	for i := 0; i < len(v.Pre) && i < len(other.Pre); i++ {
		if c := compareIdentifier(v.Pre[i], other.Pre[i]); c != 0 {
			return c
		}
	}
	// All shared identifiers are equal: the one with more identifiers is newer, which is
	// what makes 1.0.0-alpha < 1.0.0-alpha.1.
	return compareInt(len(v.Pre), len(other.Pre))
}

func compareInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// compareIdentifier orders one pre-release identifier pair.
func compareIdentifier(a, b string) int {
	an, aNum := numericIdentifier(a)
	bn, bNum := numericIdentifier(b)
	switch {
	case aNum && bNum:
		return compareInt(an, bn)
	case aNum:
		// Numeric identifiers always have lower precedence than alphanumeric ones.
		return -1
	case bNum:
		return 1
	}
	return strings.Compare(a, b)
}

// numericIdentifier reports whether s is all digits, and its value.
func numericIdentifier(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, false
		}
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		// Too large to represent. Treating it as alphanumeric keeps the ordering total
		// instead of turning a silly version string into a panic.
		return 0, false
	}
	return n, true
}

// Outcome is the answer to "is there a newer version".
type Outcome string

const (
	// OutcomeNotConfigured means no check URL is set. It is not an error: a panel with no
	// update server configured is a supported state.
	OutcomeNotConfigured Outcome = "not_configured"
	// OutcomeUpToDate means the published version equals the running one.
	OutcomeUpToDate Outcome = "up_to_date"
	// OutcomeAvailable means a newer version is published.
	OutcomeAvailable Outcome = "available"
	// OutcomeAhead means the running build is newer than the published one: a development
	// build, a rollback, or a channel that has not caught up.
	OutcomeAhead Outcome = "ahead"
	// OutcomeUnknown means the two versions cannot be compared, which is the honest answer
	// for a build stamped "dev".
	OutcomeUnknown Outcome = "unknown"
	// OutcomeUnreachable means the last check failed. The failure code says why.
	OutcomeUnreachable Outcome = "unreachable"
)
