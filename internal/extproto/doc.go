// Package extproto defines the wire protocol between the billing core and
// out-of-process extensions (payment gateways and provisioners), and the client
// that drives an extension process.
//
// # Why a separate process
//
// Payment gateways and provisioning back-ends are the two places where a
// billing panel must integrate with code it does not own and cannot fully
// trust. Running that code in-process would put a third-party panic, a leaked
// goroutine or an incompatible dependency directly into the invoicing path.
// Instead every extension is an ordinary executable: the core starts it, speaks
// a line protocol to it, and can kill it.
//
// # Transport
//
// Newline-delimited JSON (NDJSON) over the extension's stdin/stdout. Exactly one
// JSON object per line, no pretty printing, no embedded raw newlines
// (encoding/json escapes them). stdout carries frames and nothing else; any
// diagnostic output belongs on stderr, which the core captures separately.
//
// A frame larger than the configured limit (1 MiB by default) is a protocol
// violation: the connection is killed rather than buffered, so a misbehaving
// extension cannot exhaust the core's memory.
//
// # Wire format
//
// JSON-RPC 2.0. Requests carry a uint64 id, responses echo it, and notifications
// carry no id:
//
//	request:      {"jsonrpc":"2.0","id":1,"method":"gateway.describe","params":{...}}
//	success:      {"jsonrpc":"2.0","id":1,"result":{...}}
//	failure:      {"jsonrpc":"2.0","id":1,"error":{"code":-32000,"message":"...","data":{...}}}
//	notification: {"jsonrpc":"2.0","method":"$/cancelRequest","params":{...}}
//
// # Handshake
//
// Nothing may be called before a successful handshake except "health" and
// "initialize" itself. The core sends "initialize" with the protocol name and
// the versions it speaks; the extension answers with the version it selected,
// its identity and the capabilities it implements. If there is no common
// version the call fails with CodeVersionMismatch and the process is killed.
//
// # Money
//
// Amounts cross the wire as {"minor":<int64>,"currency":"USD"} — an exact
// integer count of minor units. Floating point never appears in a billing
// protocol: 0.10 has no exact binary representation, and a one-unit rounding
// difference is a wrong invoice.
//
// # Idempotency
//
// Every mutating call carries a caller-supplied "operation_key". The core
// generates it once, persists it with the intent, and replays the same key on
// every retry — including retries after the extension timed out, crashed, or was
// restarted. An extension must therefore treat a repeated key as "return the
// result of the first execution", which is what makes it safe for the core to
// retry a create whose reply it never saw.
//
// Every call also carries a "deadline" the extension should honour; the client
// additionally enforces min(ctx deadline, Spec.CallTimeout) locally so a hung
// extension cannot block the core indefinitely.
package extproto
