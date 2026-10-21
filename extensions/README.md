# Extension fixtures

Two real extension executables live here. They are not examples: they are the
conformance fixtures the whole product is tested against, and the reason the
extension client can be tested for crashes, hangs, lost replies and oversized
frames without a live payment provider.

| Fixture | Capabilities | Protocol |
| --- | --- | --- |
| `fakegateway` | `gateway.*` | NDJSON JSON-RPC 2.0 on stdin/stdout |
| `fakeprov` | `provisioner.*` | NDJSON JSON-RPC 2.0 on stdin/stdout |

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
