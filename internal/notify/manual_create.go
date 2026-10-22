package notify

// The account-created message.
//
// A customer an operator creates has no password: the account exists, the login row carries the
// hash of 32 bytes of randomness nobody kept, and the only way in is a link that lets them choose
// a password. This is the message that carries that link, and three decisions shape it.
//
//  1. **It is its own event, not a reuse of `password_reset`.** The wording is different and it has
//     to be: a reset mail says "if you did not ask for this, ignore it, the password is unchanged",
//     which for a brand-new account is actively false — ignoring it leaves an account nobody can
//     reach. An operator also needs to be able to word the invitation separately from the reset.
//
//  2. **The template lives in the code, and is inserted on first use.** `SeedTemplates` inserts the
//     shipped default for every event in `DefaultTemplates()`, and that list is not extended here
//     (this file adds the event, `events.go` owns the catalogue). So this function ensures its own
//     row exists, `ON CONFLICT DO NOTHING`, inside the caller's transaction: an operator's edited
//     wording is never overwritten, and an installation that never creates a customer by hand
//     never gains a template for a message it will never send. That is the same principle
//     `SeedTemplates` follows — seed data belongs to the code that knows the event — applied to an
//     event the catalogue does not list yet.
//
//  3. **The dedupe component is the token prefix, never the token.** The outbox must not become a
//     store of working reset links; two genuine invitations to two different accounts are two
//     messages, while a retried request for the same token is one.
//
// The link is built by the caller, so a link assembled from the request's Host header can never
// reach a customer through here.

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// EventAccountCreated is the event key for "an operator created this account".
//
// The kind shape check in `0023_notification_events.sql` accepts it: lowercase snake_case, and the
// database deliberately does not enumerate the set.
const EventAccountCreated = "account_created"

// AccountCreatedInput is what AccountCreated needs.
type AccountCreatedInput struct {
	UserID string
	Email  string
	// Link is the absolute reset URL the customer follows to choose a password. It is built by the
	// caller because only the caller knows the public origin.
	Link string
	// ExpiresIn is the lifetime of the link, so the sentence and the token cannot disagree.
	ExpiresIn time.Duration
	// Token is the dedupe component: a prefix of the token, never the token itself.
	Token       string
	CompanyName string
}

// AccountCreated queues the invitation and writes the matching inbox row.
func AccountCreated(ctx context.Context, tx pgx.Tx, queue *Queue, in AccountCreatedInput) (Outcome, error) {
	// Make sure the event has a template. It is INSERT ... ON CONFLICT DO NOTHING, so it is safe
	// on every call, safe when an operator has already edited the wording, and safe when two
	// operators create two customers at once.
	if _, err := tx.Exec(ctx, `
		INSERT INTO notification_templates
			(event_key, name, subject, body, in_app_title, in_app_body, in_app_url,
			 enabled, mail_enabled, in_app_enabled)
		VALUES ($1, $2, $3, $4, $5, $6, $7, true, true, true)
		ON CONFLICT (event_key) DO NOTHING`,
		EventAccountCreated, defaultAccountCreatedName, defaultAccountCreatedSubject,
		defaultAccountCreatedBody, defaultAccountCreatedInAppTitle,
		defaultAccountCreatedInAppBody, defaultAccountCreatedInAppURL); err != nil {
		return Outcome{EventKey: EventAccountCreated}, err
	}

	vars := map[string]string{
		"email":        in.Email,
		"link":         in.Link,
		"expires_in":   humanDuration(in.ExpiresIn),
		"company_name": orDefault(in.CompanyName, "Catalyst Billing"),
	}
	return dispatch(ctx, tx, queue, EventAccountCreated, Target{
		UserID:    in.UserID,
		Email:     in.Email,
		DedupeKey: orDefault(in.Token, in.UserID),
		Vars:      vars,
	})
}

// The shipped wording. It says what happened, what to do, and how long they have — and it does not
// ask for a password in the mail, because a message that arrives unasked must never train someone
// to type a password into a form it links to.
const (
	defaultAccountCreatedName    = "Account created"
	defaultAccountCreatedSubject = "Your {{company_name}} account is ready"

	defaultAccountCreatedBody = "An account has been created for {{email}}.\n" +
		"\n" +
		"Open this link to choose your password:\n" +
		"{{link}}\n" +
		"\n" +
		"The link expires in {{expires_in}} and can be used once.\n" +
		"If you were not expecting an account, reply to this message before using the link.\n" +
		"\n" +
		"{{company_name}}"

	defaultAccountCreatedInAppTitle = "Choose your password"
	defaultAccountCreatedInAppBody  = "An account was created for this address. Set a password to sign in."
	defaultAccountCreatedInAppURL   = "/reset"
)
