package updates

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// State is everything the updates screen needs, in one read.
//
// It is assembled from the settings, the cached check, the approval and the telemetry
// configuration so that the screen cannot render a mix of two different moments: the panel
// says "1.5.0 is available and approved" only when both facts came from the same read.
type State struct {
	// RunningVersion is this build's version, as stamped at build time.
	RunningVersion string `json:"running_version"`
	// Channel is `updates.channel` ("stable" or "beta").
	Channel string `json:"channel"`
	// Check is the update check, cached or fresh.
	Check Result `json:"check"`
	// Approval is the version an operator approved, or null.
	Approval *Approval `json:"approval"`
	// ApprovalMatchesCheck is true when an approval exists, a newer version is available,
	// and they are the same version. It is what "an update is available and approved"
	// means precisely; anything else is reported as the two separate facts it is.
	ApprovalMatchesCheck bool `json:"approval_matches_check"`
	// Telemetry describes the telemetry configuration without revealing the endpoint.
	Telemetry TelemetryState `json:"telemetry"`
	// Apply is the honest answer to "can you apply it": a procedure, not a button.
	Apply ApplyPlan `json:"apply"`
	// ServerTime is when this state was assembled, so a client can show cache age without
	// trusting its own clock.
	ServerTime time.Time `json:"server_time"`
}

// TelemetryState is the telemetry configuration as a read path sees it.
type TelemetryState struct {
	// Enabled is `updates.telemetry`. Off unless an operator turned it on.
	Enabled bool `json:"enabled"`
	// EndpointConfigured says whether a delivery endpoint is set. The endpoint itself is
	// never returned: it commonly carries an ingest token.
	EndpointConfigured bool `json:"endpoint_configured"`
	// EndpointHost is the host of the endpoint, for the operator's benefit.
	EndpointHost string `json:"endpoint_host"`
	// InstanceID is the random identifier a payload carries.
	InstanceID string `json:"instance_id"`
	// LastSend is the most recent delivery attempt, or null.
	LastSend *LastSend `json:"last_send"`
	// Schedule says when the worker will send next, and is null only when nothing schedules a
	// delivery. The screen reads it instead of promising a frequency of its own.
	Schedule *TelemetrySchedule `json:"schedule"`
}

// TelemetrySchedule is the job that keeps the "reported daily" promise.
//
// It is filled by the caller that also registers the job, so the interval shown to an operator is
// the interval the worker uses. A number typed into a template would be a second copy of the
// schedule, and the second copy is always the one that goes stale.
type TelemetrySchedule struct {
	// Job is the name the job is registered under, the same name cron_runs records.
	Job string `json:"job"`
	// IntervalHours is how often it runs.
	IntervalHours int64 `json:"interval_hours"`
	// KillSwitch is the environment variable an operator sets to stop it.
	KillSwitch string `json:"kill_switch"`
}

// ApplyPlan is how an approved update is actually applied.
//
// Mode is always "operator": there is no automated mode. The field exists so a client does
// not have to infer "no button" from a missing field, and so that adding a safe automated
// path later is a visible change to a documented contract rather than a hidden one.
type ApplyPlan struct {
	Mode  string   `json:"mode"`
	Steps []string `json:"steps"`
	Note  string   `json:"note"`
}

// applyPlan is the deployment procedure, taken from docs/operations.md and compose.yaml.
//
// It is spelled out here rather than only in the documentation because the screen shows it:
// an operator who clicks "approve" is told what approving commits them to.
func applyPlan(version string) ApplyPlan {
	if version == "" {
		version = "the approved version"
	}
	return ApplyPlan{
		Mode: "operator",
		Steps: []string{
			"Fetch the " + version + " release artifact and check its published digest before you run it.",
			"Stop `billing worker`, then `billing serve`.",
			"Run `billing migrate` with the owner database role; the runtime role cannot change the schema.",
			"Start `billing serve` and `billing worker` from the new binary.",
			"Run `billing doctor`; it exits non-zero on any problem.",
		},
		Note: "The panel does not replace its own binary. There is no signed release for it to verify and no " +
			"supervisor for it to ask for a restart, so a self-update would be an unauthenticated download " +
			"written over the running executable. Approving a version records the decision; applying it is " +
			"the deploy above.",
	}
}

// State assembles the updates state.
//
// `enabled` is `updates.telemetry`; `checkURL` is `updates.check_url`. The check is not
// forced here: reading the screen uses the cache, and the on-demand route is a separate,
// explicit action.
func (s *Store) State(ctx context.Context, running, channel, checkURL string, enabled bool, fetcher *Fetcher) (State, error) {
	check, err := s.Check(ctx, running, channel, checkURL, false, fetcher)
	if err != nil {
		return State{}, err
	}

	cfg, err := s.Config(ctx)
	if err != nil {
		return State{}, err
	}
	lastSend, err := s.LastTelemetrySend(ctx)
	if err != nil {
		return State{}, err
	}

	approval, err := s.Approval(ctx)
	if err != nil && !errors.Is(err, ErrNoApproval) {
		return State{}, err
	}

	out := State{
		RunningVersion: running,
		Channel:        channel,
		Check:          check,
		Approval:       approval,
		Telemetry: TelemetryState{
			Enabled:            enabled,
			EndpointConfigured: cfg.TelemetryEndpoint != "",
			EndpointHost:       cfg.EndpointHost(),
			InstanceID:         cfg.InstanceID,
			LastSend:           lastSend,
		},
		ServerTime: s.Now(),
	}
	if approval != nil {
		out.ApprovalMatchesCheck = check.Outcome == OutcomeAvailable &&
			normaliseVersion(approval.Version) == normaliseVersion(check.LatestVersion)
		out.Apply = applyPlan(approval.Version)
	} else {
		out.Apply = applyPlan("")
	}
	return out, nil
}

// normaliseVersion strips a leading "v" and surrounding space so "v1.5.0" and "1.5.0" are
// the same version when an approval is compared with a check.
func normaliseVersion(s string) string {
	return stripV(s)
}

// ValidateApproval checks a version before it is recorded.
//
// Two rules, both of which are about not recording a decision that cannot mean anything:
//
//   - the version must parse, so "latest" is refused rather than stored as a string nobody
//     can compare;
//   - it must be newer than the running build. Approving the version you are already running
//     is a no-op that would still show as "approved" on the screen, which is exactly the
//     kind of dishonest state this package exists to avoid. A running build with no
//     comparable stamp ("dev") is exempt from the second rule, because there is nothing to
//     compare it with — and a development build approving a release is the normal case.
func ValidateApproval(running, version string) (Version, error) {
	approved, err := ParseVersion(version)
	if err != nil {
		return Version{}, err
	}
	current, err := ParseVersion(running)
	if err != nil {
		// No comparable running version: accept the approval, it is the only sensible act
		// for a development build.
		return approved, nil
	}
	if approved.Compare(current) <= 0 {
		return Version{}, fmt.Errorf("%w: %s is not newer than the running version %s",
			ErrInvalidVersion, approved, current)
	}
	return approved, nil
}

// stripV removes a leading "v" or "V" and trims space.
func stripV(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 0 && (s[0] == 'v' || s[0] == 'V') {
		return s[1:]
	}
	return s
}
