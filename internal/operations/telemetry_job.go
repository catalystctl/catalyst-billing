package operations

import (
	"context"
	"errors"
	"fmt"
	"time"

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
// # What leaves the installation
//
// Exactly the document `updates.Payload` marshals, and nothing else: the payload schema version, a
// random per-installation id, the panel version and channel, the Go version, OS and architecture,
// the highest applied migration number, a timestamp, and seven integer counts (customers, active
// and suspended services, open invoices, invoices paid in the last 30 days, open tickets, installed
// extensions). No customer name, address, email, invoice number, amount or ticket subject is in it,
// and there is no "send more" tier to opt into: the anonymous document is the ceiling, which is the
// safest shape for a thing that leaves the building by default-off.
//
// # Why the endpoint is configuration and not a constant
//
// There is no vendor endpoint compiled in. `update_config.telemetry_endpoint` is an operator-set
// URL, treated as a credential on every read path, and this build cannot verify what a third
// party's collector expects — so it makes no assumption beyond "POST this JSON, accept a 2xx". A
// deployment that points it at a sink it controls sees exactly the bytes documented above, which is
// also how the test proves the payload is anonymous: it reads them off a local server.
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

// TelemetryMinimumGap is how close two recorded deliveries may be.
//
// Twenty hours rather than twenty-four: a daily tick that drifts by a restart should still send, and
// the cost of an occasional early delivery is nothing next to a job that sends on every restart.
const TelemetryMinimumGap = 20 * time.Hour

// deliveredWithin reports whether a successful delivery was recorded inside the window.
func deliveredWithin(ctx context.Context, pool *store.Pool, now time.Time, window time.Duration) (bool, error) {
	var count int
	err := pool.QueryRow(ctx, `
		SELECT count(*) FROM telemetry_sends
		WHERE status = 'sent' AND sent_at > $1`, now.Add(-window)).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("operations: read the telemetry history: %w", err)
	}
	return count > 0, nil
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

	// A restart must not become a second delivery. The worker ticks a job once on start and then
	// every interval, so a deployment that restarts hourly would otherwise send hourly — the
	// schedule is a promise about frequency, and a promise that depends on uptime is not one.
	// The guard is a stored row, not an in-memory timestamp: two workers restarting together see
	// the same history.
	sentRecently, err := deliveredWithin(ctx, pool, opts.Now(), TelemetryMinimumGap)
	if err != nil {
		return TelemetryRun{}, err
	}
	if sentRecently {
		return TelemetryRun{Reason: ReasonRecentlySent}, nil
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
