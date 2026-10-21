# Extension host

An extension is an executable that the core starts and talks to over newline-
delimited JSON-RPC 2.0 on stdin and stdout. This document states what the core
actually enforces, because a security property that is only described in prose is
not a security property.

## What the core enforces

| Property | How | Verified by |
|---|---|---|
| No credential inheritance | The child receives `PATH` plus only the `Env` entries the caller passed. Nothing else from the core's environment. | `TestParentSecretsNotInherited` |
| Bounded frames | A frame larger than `MaxFrame` (default 1 MiB) is a protocol violation that kills the connection; the call fails rather than buffering unbounded input. | `internal/extproto/client_test.go` |
| Deadlines | Every call carries a deadline; an extension that does not answer within it fails the call with a distinct timeout error. | `internal/extproto/client_test.go` |
| Cancellation | Cancelling the caller's context sends `$/cancelRequest` and returns immediately; a late reply is discarded. | `internal/extproto/client_test.go` |
| Crash isolation | A reader detects EOF or a parse failure, fails every pending call with a crash error, and later calls fail fast instead of hanging. | `internal/extproto/client_test.go` |
| Process-group kill | The child is started in its own process group, and closing the client kills the whole group, so a forked grandchild does not survive. | `internal/extproto/process_unix_test.go` |
| Version negotiation | The handshake must agree on a protocol version; a mismatch is a distinct error and the child is killed. | `internal/extproto/client_test.go` |
| No database access | Extensions are not given a connection string, a database handle, or a core API to change financial state. They return observations. | by construction |

## What the core does NOT enforce

**Process separation is not a sandbox.** A same-user executable can read the host
filesystem, open network connections, and inspect other processes. The RPC protocol
constrains what the extension is *asked* to do; it does not constrain what the
extension *can* do.

Therefore:

- Extensions installed today are **fully trusted operator code**, equivalent in
  authority to the panel binary itself.
- Running untrusted third-party extensions requires a runner that the core does not
  yet provide: a separate restricted OS account or container, a read-only root
  filesystem, resource limits (CPU, memory, file descriptors), and enforced egress
  policy at the network layer.
- A permission manifest is a declaration of intent for the operator and for that
  future runner. It cannot enforce anything on its own, and this project does not
  pretend otherwise.

## Why not native Go plugins

Go's `plugin` package documents portability, build-compatibility and runtime
drawbacks severe enough that a plugin cannot be treated as a stable product
interface. A supervised subprocess with a versioned protocol gives an upgrade path,
crash isolation, and a language-independent contract.

## Reconciliation and idempotency

The core assumes an adapter may lose in-process state: a restart, a crash, a
redeploy. Two protocol obligations make that survivable.

1. **`operation_key`** — every mutating call carries a stable key. An adapter that
   sees the same key twice must return the same resource, not create a second one.
2. **`provisioner.lookup`** — an adapter must answer, authoritatively, whether a
   resource exists for an operation key. The core uses this to resolve an unknown
   outcome and never replays `create` to find out, because a replay provisions a
   duplicate whenever the adapter no longer remembers the key.

An adapter that cannot answer `lookup` must return an error, not `found: false`. The
core treats an error as "still unknown" and routes the operation to review; it
treats `found: false` as proof that nothing was created and retries safely.

This distinction is the difference between "we do not know" and "it does not
exist", and conflating the two is how a reconciliation loop provisions a second
server for a customer who paid once.

## Payload conventions

- Money crosses the wire as `{"minor": <int64>, "currency": "USD"}`. Never a float,
  never a decimal string. The core owns rounding.
- Every mutating call carries `operation_key` and a deadline.
- The core validates all configuration and action inputs, calculates totals, records
  postings and controls state transitions. An extension returns typed observations;
  it cannot mark an invoice paid, modify a balance, or receive database credentials.

## Writing one

```go
package main

import (
	"context"

	"github.com/catalyst/billing/internal/extproto"
)

// The adapter must honour the operation key and answer lookup authoritatively.
func main() {
	extproto.Serve(extproto.Handlers{
		Create: func(ctx context.Context, p extproto.ProvisionCreateParams) (extproto.ProvisionCreateResult, error) {
			// Idempotent on p.OperationKey.
			return extproto.ProvisionCreateResult{ExternalID: "srv-1", Status: "running"}, nil
		},
		Lookup: func(ctx context.Context, p extproto.ProvisionLookupParams) (extproto.ProvisionLookupResult, error) {
			// Must search the backend, not an in-memory cache.
			return extproto.ProvisionLookupResult{Found: false}, nil
		},
	})
}
```

The two conformance fixtures in `extensions/` are working examples and are what the
end-to-end tests drive, including the failure modes (`create_then_timeout`,
`duplicate`, `crash`, `hang`, `oversized`) that are impossible to trigger on demand
against a real provider.
