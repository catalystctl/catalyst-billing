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

There is no framework to import. An extension is a program with a loop that reads
one JSON object per line from stdin, dispatches on `method`, and writes one JSON
object per line to stdout. The wire format is the contract, which is why the two
fixtures in `extensions/` deliberately do **not** import the core's protocol
package: if they did, a change to the Go types could silently pass while the wire
format broke.

The skeleton, with the parts that matter for correctness:

```go
func main() {
	dec := bufio.NewReaderSize(os.Stdin, 64<<10)
	enc := json.NewEncoder(os.Stdout)
	enc.SetEscapeHTML(false)

	for {
		line, err := readLine(dec, 1<<20) // bounded, like the core's reader
		if err != nil {
			return // EOF: the core closed us, which is a normal shutdown
		}
		var req struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      *uint64         `json:"id"`
			Method  string          `json:"method"`
			Params  json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(line, &req); err != nil {
			continue // one bad frame must not kill the process
		}

		result, rpcErr := dispatch(req)
		// A notification (nil ID) gets no reply.
		if req.ID == nil {
			continue
		}
		_ = enc.Encode(map[string]any{
			"jsonrpc": "2.0", "id": *req.ID, "result": result, "error": rpcErr,
		})
	}
}
```

`dispatch` must implement `initialize` and `health`, plus whatever capabilities it
advertised in the handshake. The two obligations that make the core safe to operate:

- `provisioner.create` is idempotent on `operation_key`: the same key returns the
  same resource.
- `provisioner.lookup` searches the **backend**, not an in-memory cache, and answers
  `found: false` only when it is certain nothing was created.

The fixtures are complete working examples, and they implement the failure modes the
end-to-end tests drive — `create_then_timeout`, `duplicate`, `crash`, `hang`,
`oversized` — because those are impossible to trigger on demand against a real
provider and are exactly where an integration silently breaks.
