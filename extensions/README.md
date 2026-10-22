# Extension fixtures

The fixtures below live here. They are not examples: they are the conformance
fixtures the whole product is tested against, and the reason the extension client
can be tested for crashes, hangs, lost replies and oversized frames without a live
payment provider.

| Fixture or adapter | Capabilities | Protocol |
| --- | --- | --- |
| `fakegateway` | `gateway.*` | NDJSON JSON-RPC 2.0 on stdin/stdout |
| `fakeprov` | `provisioner.*` | NDJSON JSON-RPC 2.0 on stdin/stdout |
| `cpanel` | `provisioner.*` | NDJSON JSON-RPC 2.0 on stdin/stdout; calls WHM's `json-api` over HTTPS |

Both are deterministic: ids come from a counter, nothing is random, and the same
input always produces the same output. Both keep state in memory only, including
the idempotency map keyed by `operation_key`.

## Building

```sh
go build ./extensions/...
```

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

`fixture.echo` and `fixture.env` are not part of the protocol. They let tests
assert on the exact bytes the client puts on the wire and on the fact that the
core's environment does not leak into the extension.

## Idempotency

Every mutating call carries an `operation_key`. Replaying a key returns the
original payment or resource instead of creating another one. `duplicate` is the
negative fixture: it deliberately ignores the key so the idempotency tests have a
case that must fail.

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
