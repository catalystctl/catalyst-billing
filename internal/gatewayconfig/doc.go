// Package gatewayconfig is the single answer to "which gateway and which provisioner is
// this installation using, and what does its extension need to start?".
//
// Two real adapters exist — extensions/stripe and extensions/pterodactyl — and each reads
// its whole configuration from its own environment. Before this package the only way to
// configure them was to set those variables on the *core* process and hope the extension
// host passed them through. That made ADM-10 and ADM-11 impossible: a settings screen could
// have saved a Stripe key into a table that nothing ever read.
//
// # The decision: the environment wins, per value
//
// For every value a provider needs, the effective value is resolved in this order:
//
//  1. the environment variable the extension itself reads (STRIPE_SECRET_KEY,
//     PTERODACTYL_PANEL_URL, …) when it is set and non-empty;
//  2. the value saved in `settings` by the panel (`gateway.secret_key`,
//     `servers.panel_url`, …);
//  3. nothing — the value is absent, and the extension's own validation decides whether
//     that is fatal.
//
// The environment wins because a container deployment declares its credentials in its
// environment, and that deployment must keep working across an upgrade that introduces a
// settings screen. It is also the value an operator can see and rotate without a database
// write, and a saved row silently shadowing a deployment's intent is the failure that is
// hardest to diagnose: the panel would show one key and the process would use another.
//
// The provider name follows the same rule, with one addition: `BILLING_GATEWAY_PROVIDER`
// naming a provider also *enables* the gateway, because a deployment that names a provider
// is asking for payments to work. There is no environment equivalent for the provisioner's
// provider name, so `servers.provider` is the only source — a deployment identifies its
// provisioner by the binary `BILLING_PROVISIONER_EXTENSION` points at.
//
// The extension binary follows the same rule: `BILLING_GATEWAY_EXTENSION` /
// `BILLING_PROVISIONER_EXTENSION` when set, otherwise `bin/<extension>` — the layout
// `make build` and the README produce (`go build -o bin/fakegateway ./extensions/fakegateway`).
//
// # What is read, and what is never returned
//
// The read path reports, per field, whether a value is set and where it came from
// (`environment`, `panel`, `default`, `unset`). It never reports a secret's text: a secret's
// `Value` is always empty and only `IsSet` and `Source` travel. The real text is used in one
// place — `Config.Env`, which is what the extension host hands to the child process — and it
// lives in an unexported field so it cannot be marshalled into a response by accident.
//
// A field whose value comes from the environment is marked `Editable: false`, because a
// panel form that accepts a value the runtime will ignore is worse than no form: the
// operator saves it, the screen says "saved", and nothing changes.
//
// # What the connection test does
//
// `TestGateway` and `TestServer` launch the configured extension exactly the way the worker
// and the server do — same binary resolution, same environment — perform the protocol
// handshake and call `gateway.describe` / `provisioner.describe`. They write nothing: no
// settings row, no audit row, no queue entry. A test that could change configuration would
// be a test nobody dares press.
package gatewayconfig
