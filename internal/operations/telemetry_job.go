package operations

import (
	"context"
	"errors"
	"fmt"

	"github.com/catalyst/billing/internal/settings"
	"github.com/catalyst/billing/internal/store"
	"github.com/catalyst/billing/internal/updates"
)

// The daily telemetry delivery (OPS-13).
//
// `updates.telemetry` is an opt-in switch whose label says the panel reports anonymous counts
// daily. Until this job existed the only thing that ever sent a payload was an operator pressing
// "send now", so the switch promised a schedule nothing kept, and `telemetry_sends` — the history
// that is supposed to prove what was sent — stayed empty on every installation that never pressed
// the button.
//
// # What it sends, and what it refuses to send
//
// The payload is internal/updates' fixed document (seven integer counts, versions and the
// installation's random id) and this file adds nothing to it: no field, no header and no query
// parameter carries anything else. The three refusals are all states rather than errors, because a
// fresh installation with telemetry off is the normal case and a red `cron_runs` row for it would
// teach an operator to ignore red rows:
//
//   - telemetry is off (`updates.telemetry = false`, the default);
//   - no endpoint is stored;
//   - `BuildPayload` returns no payload, which is the same as the first refusal stated by the
//     builder rather than by this caller.
//
// # Why the sender is a parameter
//
// The delivery is an outbound POST to an operator-chosen host. A test must be able to point it at
// a local server and read the bytes that arrive, which is also how "nothing identifying is sent"
// is proved rather than promised.

// TelemetryRun is what one delivery attempt did.
type TelemetryRun struct {
	// Reason is set when nothing was sent, and names why.
	Reason string
	// Result is the recorded delivery, when one happened.
	Result *updates.SendResult
}

// SendTelemetry delivers the payload when the operator has opted in and an endpoint is stored.
func SendTelemetry(ctx context.Context, pool *store.Pool, opts Options) (TelemetryRun, error) {
	opts = opts.withDefaults()
	if disabled(TelemetryKillSwitch) {
		return TelemetryRun{Reason: ReasonKillSwitch}, nil
	}

	values := settings.New(pool)
	enabled, err := values.Bool(ctx, "updates", "telemetry")
	if err != nil {
		return TelemetryRun{}, fmt.Errorf("operations: read updates.telemetry: %w", err)
	}
	if !enabled {
		return TelemetryRun{Reason: ReasonDisabled}, nil
	}
	channel, err := values.String(ctx, "updates", "channel")
	if err != nil {
		return TelemetryRun{}, fmt.Errorf("operations: read updates.channel: %w", err)
	}

	updateStore := updates.New(pool)
	cfg, err := updateStore.Config(ctx)
	if err != nil {
		return TelemetryRun{}, fmt.Errorf("operations: read the update configuration: %w", err)
	}
	if cfg.TelemetryEndpoint == "" {
		return TelemetryRun{Reason: ReasonNotConfigured}, nil
	}

	payload, err := updateStore.BuildPayload(ctx, opts.Version, channel, true)
	if err != nil {
		return TelemetryRun{}, fmt.Errorf("operations: build the telemetry payload: %w", err)
	}
	if payload == nil {
		// Unreachable while the switch is on, and kept because a nil payload reaching Send would
		// be an empty POST recorded as a delivery.
		return TelemetryRun{Reason: ReasonDisabled}, nil
	}

	// No actor: this is the installation reporting about itself, not a person acting. A send
	// made by an operator through the screen records their id, and the two are distinguishable in
	// `telemetry_sends.sent_by`, which is exactly what the column is for.
	result, err := updateStore.Send(ctx, cfg.TelemetryEndpoint, *payload, "", opts.Client)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return TelemetryRun{}, err
		}
		return TelemetryRun{}, fmt.Errorf("operations: send telemetry: %w", err)
	}
	return TelemetryRun{Reason: ReasonSent, Result: &result}, nil
}
