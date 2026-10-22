package notify

// Per-user channel preferences.
//
// Paymenter's model, which this reproduces because it is the one operators already
// understand, is a four-valued preference per (user, event):
//
//	force      the user cannot opt out
//	choice_on  the user has this event on
//	choice_off the user has this event off
//	never      this event is never sent, whatever anyone chooses
//
// The precedence is the whole of this file, and it is here rather than in each caller
// because the interesting cases are exactly the ones a caller would get wrong:
//
//  1. `never` wins over everything, including `force`. It is the one value an operator sets
//     deliberately to stop a mail that would otherwise be unavoidable, so a user preference
//     cannot override it. A *user* cannot store `never` or `choice_off` against a force
//     event at all — SetPreference refuses it — so the footgun of an account that can never
//     reset its password is closed at the write path, where it belongs.
//  2. A force event ignores `choice_off`. This is the point of `force`: opting out of the
//     password-reset mail does not protect anyone, it just makes the account
//     unrecoverable.
//  3. Otherwise the user's own choice applies.
//  4. With no row at all, the event is on. Notifications are opt-out, not opt-in: a customer
//     who has never opened the preferences screen must still be told their server was
//     terminated.
//
// A template's own channel toggles are applied *after* this, by the dispatcher: the
// preference says whether the user wants it, the template says whether the channel exists.
// Keeping them separate is what lets an operator turn off the inbox for an event without
// touching every customer's preference.

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// Preference values, matching the CHECK on notification_preferences.preference.
const (
	PreferenceForce     = "force"
	PreferenceChoiceOn  = "choice_on"
	PreferenceChoiceOff = "choice_off"
	PreferenceNever     = "never"
)

// ErrForceEvent is returned when a user tries to opt out of an event they cannot opt out
// of. The HTTP layer turns it into 422 with a code the UI can explain.
var ErrForceEvent = errors.New("notify: this notification cannot be turned off")

// ErrUnknownEvent is returned for an event key that is not in the catalogue.
var ErrUnknownEvent = errors.New("notify: unknown event")

// Decision is the answer to "which channels may this user be reached on for this event".
//
// Both channels share one preference, because that is the model: a user says "I do not want
// new-invoice mail", not "I want it in the inbox but not by mail". The per-template toggles
// are what express channel-level policy, and they are the operator's, not the user's.
type Decision struct {
	Mail  bool
	InApp bool
	// Preference is the effective preference: "force", "choice_on", "choice_off" or
	// "never". It is the stored value, or the default when no row exists, so a caller can
	// report why without re-deriving it.
	Preference string
	// Force is true when the event is one the user cannot opt out of.
	Force bool
	// MailReason and InAppReason explain a false, using the stable strings the dispatcher
	// copies into Result.Reason.
	MailReason  string
	InAppReason string
}

// Allowed reports whether anything at all may be sent.
func (d Decision) Allowed() bool { return d.Mail || d.InApp }

// ForceEvent reports whether an event cannot be turned off by the user.
//
// It is derived from the catalogue rather than from the template row, because "can the user
// opt out" is a property of the event's meaning, not of an editable string. A template row
// that could flip a password reset to optional would be a way to lock every customer out of
// their own account with one PUT.
func ForceEvent(eventKey string) bool {
	for _, e := range Catalogue() {
		if e.Key == eventKey {
			return e.Force
		}
	}
	return false
}

// StoredPreference reads the raw preference row, if any.
//
// The second return value is false when the user has never expressed a preference, which is
// different from having expressed "on": the preferences screen shows the default as
// unset rather than as a choice the user made.
func StoredPreference(ctx context.Context, q Querier, userID, eventKey string) (string, bool, error) {
	if userID == "" {
		return "", false, nil
	}
	var value string
	err := q.QueryRow(ctx, `
		SELECT preference FROM notification_preferences
		WHERE user_id = $1 AND event_key = $2`, userID, eventKey).Scan(&value)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("notify: read preference: %w", err)
	}
	return value, true, nil
}

// Decide resolves the channels for one user and one event.
//
// This is the function a caller should use instead of reimplementing the precedence. It
// never fails because of a *missing* preference — that is the common case — only because of
// a storage error.
func Decide(ctx context.Context, q Querier, userID, eventKey string) (Decision, error) {
	force := ForceEvent(eventKey)
	d := Decision{Force: force}

	stored, has, err := StoredPreference(ctx, q, userID, eventKey)
	if err != nil {
		return d, err
	}

	effective := stored
	switch {
	case has && stored == PreferenceNever:
		// `never` is an operator's decision and outranks the event's own force flag.
		effective = PreferenceNever
	case force:
		// A force event ignores choice_off. Note that a *stored* force is also honoured, but
		// only for a non-force event, below.
		effective = PreferenceForce
	case has:
		effective = stored
	default:
		effective = PreferenceChoiceOn
	}

	d.Preference = effective
	switch effective {
	case PreferenceNever:
		d.MailReason = "preference_never"
		d.InAppReason = "preference_never"
	case PreferenceChoiceOff:
		d.MailReason = "preference_off"
		d.InAppReason = "preference_off"
	default: // force, choice_on
		d.Mail = true
		d.InApp = true
	}
	return d, nil
}

// SetPreference stores a user's choice.
//
// It refuses the two combinations that would produce an unreachable account: `never` or
// `choice_off` against a force event. Refusing here rather than silently upgrading the value
// means the API can tell the user why, and the stored row never disagrees with the rule that
// is applied when a message is sent.
//
// Passing PreferenceForce for a non-force event is refused for the same reason in the other
// direction: `force` is a property of the event, so a user must not be able to give
// themselves a notification they cannot then turn off.
func SetPreference(ctx context.Context, q Querier, userID, eventKey, preference string) error {
	if !IsKnownEvent(eventKey) {
		return fmt.Errorf("%w: %s", ErrUnknownEvent, eventKey)
	}
	switch preference {
	case PreferenceForce, PreferenceChoiceOn, PreferenceChoiceOff, PreferenceNever:
	default:
		return fmt.Errorf("notify: unknown preference %q", preference)
	}
	force := ForceEvent(eventKey)
	if force && (preference == PreferenceChoiceOff || preference == PreferenceNever) {
		return fmt.Errorf("%w: %s", ErrForceEvent, eventKey)
	}
	if !force && preference == PreferenceForce {
		return fmt.Errorf("notify: only an account-security event can be forced: %s", eventKey)
	}

	_, err := q.Exec(ctx, `
		INSERT INTO notification_preferences (user_id, event_key, preference, updated_at)
		VALUES ($1, $2, $3, now())
		ON CONFLICT (user_id, event_key) DO UPDATE
			SET preference = EXCLUDED.preference, updated_at = now()`,
		userID, eventKey, preference)
	if err != nil {
		return fmt.Errorf("notify: save preference: %w", err)
	}
	return nil
}

// ClearPreference removes a user's choice, returning the event to its default.
//
// It exists because "on" and "not set" are different on the preferences screen: a customer
// who wants the default back should not have to guess which of the two values the default is.
func ClearPreference(ctx context.Context, q Querier, userID, eventKey string) error {
	_, err := q.Exec(ctx,
		`DELETE FROM notification_preferences WHERE user_id = $1 AND event_key = $2`,
		userID, eventKey)
	if err != nil {
		return fmt.Errorf("notify: clear preference: %w", err)
	}
	return nil
}

// PreferenceRow is one line of the preferences screen: the event, what it is, what the
// effective setting is, and whether the user is allowed to change it.
type PreferenceRow struct {
	EventKey   string
	Name       string
	Force      bool
	Preference string
	// Stored is the user's own choice, empty when they have not made one.
	Stored string
	Mail   bool
	InApp  bool
}

// Preferences returns the whole catalogue resolved for one user, in display order.
//
// It resolves rather than listing raw rows so the screen shows what will actually happen,
// including the defaults for events the user has never touched. One query for the stored
// rows, not one per event: a preferences screen that issues twelve queries is twelve
// chances for a partial render.
func Preferences(ctx context.Context, q Querier, userID string) ([]PreferenceRow, error) {
	stored := map[string]string{}
	if userID != "" {
		rows, err := q.Query(ctx,
			`SELECT event_key, preference FROM notification_preferences WHERE user_id = $1`, userID)
		if err != nil {
			return nil, fmt.Errorf("notify: read preferences: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var key, value string
			if err := rows.Scan(&key, &value); err != nil {
				return nil, fmt.Errorf("notify: scan preference: %w", err)
			}
			stored[key] = value
		}
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("notify: read preferences: %w", err)
		}
	}

	catalogue := Catalogue()
	out := make([]PreferenceRow, 0, len(catalogue))
	for _, e := range catalogue {
		d := Decision{Force: e.Force}
		// Same precedence as Decide, applied to the value already in memory.
		effective := stored[e.Key]
		switch {
		case effective == PreferenceNever:
		case e.Force:
			effective = PreferenceForce
		case effective == "":
			effective = PreferenceChoiceOn
		}
		d.Preference = effective
		d.Mail = effective == PreferenceForce || effective == PreferenceChoiceOn
		d.InApp = d.Mail
		out = append(out, PreferenceRow{
			EventKey:   e.Key,
			Name:       e.Name,
			Force:      e.Force,
			Preference: effective,
			Stored:     stored[e.Key],
			Mail:       d.Mail,
			InApp:      d.InApp,
		})
	}
	return out, nil
}
