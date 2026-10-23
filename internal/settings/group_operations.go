// The operations settings group (OPS-09).
//
// The parity inventory's row is precise about the defect: `cronjob.order_cancel_days`,
// `cronjob.terminate_days` and `tickets.autoclose_days` were declared, rendered on the settings
// page, and read by the jobs that obey them — but the *dunning ladder* still ran the constants
// hard-coded in `dunning.DefaultPolicy()`. An operator who set "cancel after 30 days" was
// therefore configuring one rule while a second, equally consequential rule (remind every two
// days, give up after three reminders, allow suspension after a week) stayed at whatever the
// last release compiled in.
//
// The four keys here are that ladder, moved into the table an operator can read and change.
// Their defaults are exactly the numbers DefaultPolicy() returned, so an installation that never
// opens the settings page behaves the same as before — which is the only safe way to move a
// constant into configuration.
//
// Why these are in the `cronjob` group rather than a new one: the group is already "what the
// scheduled rules do and when", the settings page already renders it, and the kill switches on
// the automation screen point at jobs that read settings from it. A second group for the same
// subject would make an operator look in two places for one answer.
//
// One threshold the row names is deliberately *not* here: "invoice days" — how far before the period
// ends a renewal invoice is issued. That is already a stored per-installation value in the renewal
// policy (`invoice_lead_days`, alongside `payment_terms_days` and the catch-up limits), because it
// changes the shape of a billing run rather than the escalation of one invoice. Duplicating it into
// this group would give an operator two places to set one number, and the second would be the one
// the renewal engine does not read.
//
// A note on `dunning_enabled`: the ladder needed an off switch before its numbers became
// configurable, because "remind every 0 days" and "suspend after 0 days" are the readings that
// harm a customer, and a stored zero is a plausible typo. With an explicit switch, a zero day
// count is unambiguously "not configured" and falls back to the default; an operator who wants
// the ladder to stop says so with a boolean rather than by zeroing a threshold.
package settings

import (
	"fmt"
	"strconv"
	"strings"
)

// The operations keys. They are exported names because internal/dunning and the automation
// screen both refer to them, and a string literal repeated in two packages is a typo waiting for
// a quiet afternoon.
const (
	// GroupCronjob is the group the scheduled rules read.
	GroupCronjob = "cronjob"
	// KeyReminderDays is the gap between dunning steps, in whole days.
	KeyReminderDays = "reminder_days"
	// KeyMaxReminders is how many reminders run before the state is exhausted.
	KeyMaxReminders = "max_reminders"
	// KeySuspendDays is how long after the due date a suspension becomes possible.
	KeySuspendDays = "suspend_days"
	// KeyDunningEnabled is the ladder's own off switch.
	KeyDunningEnabled = "dunning_enabled"
	// KeyChargeRetryHours is how long a declined off-session charge waits before it is offered
	// to the provider again. It is the one threshold a ticker needs and a button does not.
	KeyChargeRetryHours = "charge_retry_hours"
)

func init() {
	Register(
		Definition{
			Group: GroupCronjob, Key: KeyDunningEnabled, Kind: KindBool, Default: "true",
		},
		Definition{
			Group: GroupCronjob, Key: KeyReminderDays, Kind: KindInt, Default: "2",
			// One day is the shortest gap that can still be called a reminder schedule: a
			// value of zero would mean "remind continuously", which is a mail flood rather
			// than a dunning rule.
			Check: atLeastOneDay,
		},
		Definition{
			Group: GroupCronjob, Key: KeyMaxReminders, Kind: KindInt, Default: "3",
			// A ladder longer than ten steps has stopped being a reminder schedule: by then
			// the invoice is a debt, and the honest next step is a human.
			Check: wholeNumberBetween(1, 10),
		},
		Definition{
			Group: GroupCronjob, Key: KeySuspendDays, Kind: KindInt, Default: "7",
			Check: atLeastOneDay,
		},
		Definition{
			Group: GroupCronjob, Key: KeyChargeRetryHours, Kind: KindInt, Default: "24",
			// A week is the ceiling: a card declined a week ago is a card the customer has not
			// replaced, and retrying it costs the merchant's reputation for no chance of payment.
			Check: wholeNumberBetween(1, 168),
		},
	)
}

// atLeastOneDay refuses a non-positive day count.
//
// The refusal is at the settings boundary rather than in the reader because a zero stored here
// would be *ambiguous* to every reader: "off" and "immediately" are both plausible readings of
// "0 days", and this package must not let an operator store a value whose meaning depends on
// which code path reads it. The ladder's off switch is `dunning_enabled`.
func atLeastOneDay(d Definition, value string) error {
	n, err := wholeInt(value)
	if err != nil {
		return err
	}
	if n < 1 {
		return fmt.Errorf("%w: %s must be at least 1 (use cronjob.dunning_enabled to switch the ladder off)",
			ErrInvalidValue, d.Name())
	}
	return nil
}

// wholeNumberBetween closes a range. It runs after the kind check, so the only failure it can
// report is the range.
func wholeNumberBetween(low, high int64) func(d Definition, value string) error {
	return func(d Definition, value string) error {
		n, err := wholeInt(value)
		if err != nil {
			return err
		}
		if n < low || n > high {
			return fmt.Errorf("%w: %s must be between %d and %d", ErrInvalidValue, d.Name(), low, high)
		}
		return nil
	}
}

// wholeInt parses the stored text.
//
// It cannot fail for a value that reached Definition.Check through Validate — the kind check
// ran first — but a Check is also reachable from a caller that skipped validation, and returning
// the parse error is better than a panic in a settings write. strconv, not Sscanf: "%d" accepts
// "12abc" as 12, which would let a typo through as a number.
func wholeInt(value string) (int64, error) {
	n, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: %q is not a whole number", ErrInvalidValue, value)
	}
	return n, nil
}
