# Extension fixtures

Four real extension executables live here. They are not examples: they are the
conformance fixtures the whole product is tested against, and the reason the
extension client can be tested for crashes, hangs, lost replies and oversized
frames without a live payment provider.

| Fixture or adapter | Capabilities | Protocol |
| --- | --- | --- |
| `fakegateway` | `gateway.*` | NDJSON JSON-RPC 2.0 on stdin/stdout, version 1 |
| `fakeprov` | `provisioner.*` | NDJSON JSON-RPC 2.0 on stdin/stdout, version 1 |
| `cpanel` | `provisioner.*` | NDJSON JSON-RPC 2.0 on stdin/stdout; calls WHM's `json-api` over HTTPS |
| `example-hooks` | `ui.describe`, `events.subscribe`, `service.reboot` | NDJSON JSON-RPC 2.0 on stdin/stdout, version 2 (version 1 on request) |

All four are deterministic: ids come from a counter, nothing is random, and the
same input always produces the same output. All four keep state in memory only,
including the idempotency map keyed by `operation_key`.

`fakegateway` and `fakeprov` speak protocol version 1 only, which is the point:
they are the compatibility case. A v1 extension is still accepted, still works
exactly as it did, and is never sent an event.

## Building

```sh
go build ./extensions/...
```

## Protocol version 2

Version 2 adds two things to version 1 and changes nothing that version 1
defined. The envelope, the deadline stamping, the error codes and the
idempotency rules are identical, so an extension that only speaks version 1 is
unaffected: it selects version 1 in the handshake and the core asks it nothing
new.

### `ui.describe` — extension-contributed UI as data

The core calls `ui.describe` once, after the handshake, and stores the answer
against the installed extension:

```json
{
  "nav_items": [
    {"id": "example-hooks", "label": "Example hooks",
     "path": "/staff/extensions/example-hooks", "icon": "plug",
     "permission": "extension:manage"}
  ],
  "service_actions": [
    {"id": "reboot", "label": "Reboot service", "method": "service.reboot",
     "confirm": "Reboot this service? It will be unavailable for about a minute.",
     "permission": "service:write:self"}
  ]
}
```

This is data, not code. The panel renders a label, an icon *name* from its own
set and a link to a path it already routes; the extension never supplies markup,
script or a URL to load, so contributing UI never means running its code in an
operator's session.

What the core refuses at registration, in full:

| Rule | Why |
| --- | --- |
| `permission` must be in `authn.AllPermissions()` | a hook cannot invent a capability |
| `method` must be a capability from the handshake | the surface is "call this hook", not "run this code" |
| `method` must not be a core method (`initialize`, `health`, `gateway.*`, `provisioner.*`, `ui.describe`, `events.*`) | a hook must not be able to route into another surface |
| `path` must be absolute, in-panel, and contain no scheme, `..` or backslash | it is rendered as a link |
| `icon` must be a lower-case icon name (`a-z0-9-`) | it is an icon name, not SVG |
| `id`, `label`, `confirm` are bounded, printable, and ids are unique | a page render must not be able to be flooded |

A declaration that breaks any rule is refused as a whole: a half-installed hook
set is worse than none, because the operator cannot tell which half is live.

### `events.subscribe` / `events.deliver` — lifecycle events

After the handshake the core offers its closed list of kinds and the extension
answers with the subset it wants. Nothing is assumed: an extension that does not
advertise `events.subscribe` is not asked, and one that accepts nothing receives
nothing.

| Kind | Emitted by |
| --- | --- |
| `cart.created`, `cart.updated`, `cart.abandoned`, `cart.checked_out` | the storefront cart (declared ahead of the cart publishing them) |
| `order.created`, `order.paid`, `order.cancelled` | checkout and order placement |
| `ticket.created`, `ticket.replied`, `ticket.closed`, `ticket.assigned`, `ticket.updated` | the support ticket surface |

A kind is only delivered if the core actually publishes it. Today that is
`order.created` and the ticket kinds; a declared cart kind is a subscription an
extension can write now and receive when the cart starts publishing.

Delivery is a JSON-RPC **notification** (`events.deliver`, no id, no reply). It
happens after the change committed, and never on the request path: the core
fans out through a bounded per-extension queue that drops and counts rather than
blocking. A dead extension therefore cannot slow a checkout — it loses events,
and the loss is visible in the registry's `Dropped` counter rather than silent.

## Programming `example-hooks`

`example-hooks` is a small real extension: one navigation item and one service
action, which is the whole of the version-2 surface in a form a test can drive.

| Variable | Meaning |
| --- | --- |
| `EXAMPLE_HOOKS_MODE=ok` (default) | answers normally |
| `EXAMPLE_HOOKS_MODE=reject` | the service action answers `-32005`, a business rejection |
| `EXAMPLE_HOOKS_MODE=stall` | stops reading stdin after registration: the core's writes fill the pipe and its queue, which is how "a dead extension cannot delay a checkout" is tested |
| `EXAMPLE_HOOKS_MODE=exit` | exits after registration, so the next notification fails immediately |
| `EXAMPLE_HOOKS_VERSIONS=2,1` | versions to offer, newest first. Set `1` for a v1-only build |
| `EXAMPLE_HOOKS_NAV_PERMISSION` | override the nav item's declared permission (default `extension:manage`) |
| `EXAMPLE_HOOKS_ACTION_PERMISSION` | override the action's declared permission (default `service:write:self`) |
| `EXAMPLE_HOOKS_ACTION_METHOD` | override the method the action calls (default `service.reboot`) |
| `EXAMPLE_HOOKS_EVENT_KINDS` | comma-separated kinds to accept, or `*` for all offered (default) |
| `EXAMPLE_HOOKS_PIDFILE` | write the process id at startup |

## Programming a fixture

Failure modes are selected with an environment variable. The mode applies to
every method except `initialize`, so the handshake always succeeds and the
failure happens on the call under test.

| `FAKE_GATEWAY_MODE` / `FAKE_PROV_MODE` | Behaviour |
| --- | --- |
| `ok` (default) | answers normally |
| `fail` | answers with a JSON-RPC error object |
| `hang` | never answers, keeps running |
| `oversized` | answers with a 2 MiB frame (over the 1 MiB default limit) |
| `crash` | exits mid-call, before answering |
| `slow` | answers after `FAKE_*_SLOW_MS` milliseconds (default 500) |
| `create_then_timeout` (provisioner) | provisions the resource, then swallows the reply |
| `duplicate` (provisioner) | ignores `operation_key`, so a retry provisions a second resource |
| `garbage` (gateway) | answers with a line that is not JSON |
| `badrpc` (gateway) | answers with JSON that is not JSON-RPC 2.0 |

Other knobs:

| Variable | Meaning |
| --- | --- |
| `FAKE_*_SLOW_MS` | delay for `slow` mode |
| `FAKE_*_VERSION` | answer `initialize` with this protocol version instead of 1 |
| `FAKE_*_NEGOTIATE=fail` | refuse version negotiation with `-32001` |
| `FAKE_*_PIDFILE` | write the process id at startup (used to assert the core kills it) |
| `FAKE_*_STDERR` | write this text to stderr at startup |

## Fixture-only methods

| Method | Purpose |
| --- | --- |
| `health` | liveness probe; the only call besides `initialize` allowed before the handshake |
| `fixture.echo` | returns the raw params it received, verbatim |
| `fixture.env` | returns the child's environment as `{"vars": {...}}` |
| `fixture.received` | what `example-hooks` has received: the delivered events and the action calls, in order |

`fixture.echo`, `fixture.env` and `fixture.received` are not part of the
protocol. They let tests assert on the exact bytes the client puts on the wire,
on the fact that the core's environment does not leak into the extension, and on
the fact that an event actually arrived at the extension rather than at a
counter inside the core.

## Idempotency

Every mutating call carries an `operation_key`. Replaying a key returns the
original payment or resource instead of creating another one. `duplicate` is the
negative fixture: it deliberately ignores the key so the idempotency tests have a
case that must fail.

A service action is a mutating call too: the panel sends its `Idempotency-Key`
as the `operation_key`, so a retried click returns the first result rather than
rebooting twice.

## Real adapters

`cpanel` is not a fixture. It is an adapter for a live WHM server, and its purpose
is the same as the fixtures': keeping the protocol honest. A second provisioner
that does not fit the first one's shape is the only evidence that the protocol is
not shaped like the first one's provider.

It manages hosting accounts on one WHM server and reads:

| Variable | Meaning |
| --- | --- |
| `CPANEL_WHM_URL` | required; e.g. `https://whm.example.com:2087` |
| `CPANEL_API_TOKEN` | required; a WHM API token, sent as `Authorization: whm root:TOKEN` |
| `CPANEL_PLANS` | optional; `id:quota_mb:bandwidth_mb`,… (default one plan `default:1024:10240`) |
| `CPANEL_DEFAULT_PLAN` | optional; the plan a call that names none gets |
| `CPANEL_TIMEOUT_MS` | optional; per-call HTTP timeout, default 30000 |
| `CPANEL_VERIFY_TLS` | optional; default on. Off prints a warning at startup |
| `CPANEL_STATE_FILE` | optional; the operation ledger, default `/var/lib/catalyst/cpanel/operations.json` |

Its failure modes are the ones a provider can actually produce, and each maps to a
different JSON-RPC code because the core reacts differently to each:

| Situation | Answer |
| --- | --- |
| WHM unreachable, answering 5xx, or answering something that is not the `json-api` envelope | `-32000`, never "not found" — the core waits instead of provisioning a duplicate |
| WHM rejects the API token | `-32000`, naming `CPANEL_API_TOKEN` |
| `metadata.result: 0`: a refusal on the merits | `-32005` carrying WHM's own reason |
| The account is not in `listaccts` | `found:false` from `lookup`; `-32004` from `inspect`/`suspend`/`resume`; `terminate` succeeds with `changed:false` |
| The account is already in the requested state | success with `changed:false`, and WHM is not called at all |
| A name or domain cPanel would refuse | `-32005` before any request is sent |

`metadata[cpanel_domain]` supplies the hosted domain; a create without one is
refused locally. `metadata[cpanel_username]` is refused unless it equals the name
derived from the operation key: WHM has no external id field, so a name the adapter
cannot recompute from the key would leave `provisioner.lookup` unable to find an
account it did not create, which is the duplicate the method exists to prevent.
