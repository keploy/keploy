# Mock matcher contract

**Audience**: authors of record-mode parsers in keploy, keploy/integrations,
and keploy/enterprise. This is the spec your parser's output is compared
against.

## Purpose

During replay, keploy's mock matcher selects one recorded mock per
outgoing call the application makes. It uses a small, stable set of
fields on each `*models.Mock` for this selection. Any parser that
produces mocks outside these expectations will cause wrong-mock
selection at replay time — usually silent, sometimes fatal for the
test.

This document pins the contract so every parser knows exactly what
the matcher reads and what the invariants are.

## The contract (what the matcher consumes)

For each mock, the matcher reads:

| Field | Type | Meaning | Invariant |
|-------|------|---------|-----------|
| `Spec.ReqTimestampMock` | `time.Time` (wallclock) | Instant the recorded request **first arrived** at the proxy on the real client socket. | Non-zero. `ReqTimestampMock <= ResTimestampMock`. Monotonic within a connection: if mock M2 was emitted after M1 on the same connection, `M2.ReqTimestampMock >= M1.ReqTimestampMock`. |
| `Spec.ResTimestampMock` | `time.Time` (wallclock) | Instant the last byte of the recorded response was **written to the real client** by the proxy. | Non-zero. `>= ReqTimestampMock`. |
| `Kind` | protocol-specific constant (`HTTP`, `MySQL`, etc.) | Protocol tag. | Must match the parser's `IntegrationType`. |
| `ConnectionID` | string | Stable identifier for the real TCP connection the traffic rode on. In the V2 architecture this is `supervisor.Session.ClientConnID`. | Non-empty. Consistent across all mocks emitted during the same session. |
| `Spec` (other fields) | protocol-specific (`HTTPReq/HTTPResp`, `MySQLRequests/Responses`, etc.) | The actual wire content the matcher keys against. | Byte-equivalent to what the application would have seen or sent. |
| `Metadata` | `map[string]string` | Protocol-specific annotations; parsers may add custom keys but must not conflict with reserved ones. | Reserved keys listed below. |

### Reserved `Metadata` keys

| Key | Value | Written by |
|-----|-------|------------|
| `type` | protocol name (e.g. `"mysql"`) | Every parser. |
| `connID` | same as `ConnectionID` | Every parser. |
| `destAddr` | `host:port` of real destination | Every parser where known. |
| `tls_stage` | `"prelude"` or `"post-upgrade"` | Mid-stream-TLS parsers (Postgres v2, MySQL, SMTP STARTTLS in the future). |
| `requestOperation` / `responseOperation` | protocol opcode names | Protocols with structured operations (MySQL, Postgres, Mongo). |

Custom keys are fine as long as they don't collide with the reserved
set. Replay code MUST NOT depend on custom keys for selection — they
are strictly informational.

## Timestamp rule (I5)

`ReqTimestampMock` and `ResTimestampMock` must come from the
**real-socket boundary**, not from inside the parser. In the V2
architecture, the relay stamps these:

- `Chunk.ReadAt` = `time.Now()` captured **immediately after** `Read()`
  returns on the producing real socket.
- `Chunk.WrittenAt` = `time.Now()` captured **immediately after**
  `Write()` returns on the opposite real socket.

A parser emitting a mock sets:

```go
mock.Spec.ReqTimestampMock = firstClientChunk.ReadAt
mock.Spec.ResTimestampMock = lastDestChunk.WrittenAt // or last client-facing write
```

Why this matters:

- The matcher uses `ReqTimestampMock` to order mocks within a connection
  and to pick the earliest unused mock when multiple candidates match.
  Wrong timestamps → wrong mock selected → test fails in confusing ways.
- Using `time.Now()` inside the parser captures decoder buffering,
  scheduler jitter, and mock-channel back-pressure — effectively random
  noise from the matcher's perspective.
- The boundary stamp reflects the latency the application's client
  actually observed, which is what the outer test harness's time
  window is comparing against.

Enforcement: `tools/lint/no_timestamp_in_parser/` rejects `time.Now()`,
`time.Since()`, `time.Until()` calls inside files under
`**/recorder/*.go` and `**/encode*.go`. Log-line and telemetry use
can opt out with `// allow:time.Now` on the preceding line. Tests are
exempt.

## Serialization and monotonic time

`time.Time` values carry both wallclock and monotonic readings. The
YAML serializer used for mockdb **strips the monotonic reading on
write**. This does not affect the matcher, which compares wallclock
only. Do not try to preserve monotonic across serialization boundaries.

Do not call `.UTC()` on stamped timestamps before emitting — keep them
in the local process zone for consistency with the test harness's
`time.Now()` window. The matcher uses zone-aware comparison.

## Ordering guarantee

The supervisor enforces per-session monotonicity for `ReqTimestampMock`
inside `Session.EmitMock`. The implementation holds a short
`sync.Mutex` around a `lastReqTimestamp` field and on every emit:

- If the mock's `ReqTimestampMock` is zero, pass through (parsers or
  test mocks without a populated timestamp bypass the clamp).
- If it is strictly earlier than `lastReqTimestamp`, clamp it up to
  `lastReqTimestamp + 1ns`. The matcher's ordering invariant holds
  either way; the clamp only corrects parser-internal reordering
  harmlessly.
- When `supervisor.SetDebugMonotonic(true)` is called (typically by
  test binaries), a regression panics instead of clamps so parser
  bugs surface immediately. Production leaves it off.

Parsers should not rely on the clamp to hide ordering bugs — it is a
correctness backstop, not a substitute for emitting ordered mocks.

## Connection tagging

`ConnectionID` must be stable across all mocks emitted by a single
`Session`. In the V2 path, `Session.ClientConnID` is the canonical
source; `EmitMock` propagates it onto `mock.ConnectionID` when the
parser has not already populated the field. Parsers that need to
override (e.g. a wrapper parser tagging a composite ID) can still
assign `m.ConnectionID` explicitly before calling `EmitMock`.

The matcher uses connection tagging for protocols where state is
connection-scoped (e.g. MySQL prepared statements, Postgres extended
query protocol).

## Post-record hook chain

Wrapper parsers can annotate mocks produced by a shared parser without
teaching that parser about downstream protocols. The chain is invoked
by `EmitMock` before the send to the mocks channel. Hooks run in
LIFO order (front-of-chain wrapper first).

```go
// Example: wrapper parser layers a custom metadata key on top of
// the shared HTTP parser's output.
sess.AddPostRecordHook(func(m *models.Mock) {
    m.Metadata["outer-protocol"] = "sqs"
})
```

`AddPostRecordHook` is nil-receiver and nil-hook safe. Chain
ordering is the responsibility of the wrapper — use the helper,
do not assign to `OnMockRecorded` directly (direct assignment
replaces any existing chain).

## Invariants summary

1. `ReqTimestampMock` ≤ `ResTimestampMock` — always.
2. `ReqTimestampMock` per-session-monotonic — guaranteed by supervisor
   clamp, but parsers should emit in order.
3. `Kind` matches the parser's `IntegrationType` — the dispatcher
   relies on this mapping at replay time.
4. `ConnectionID` consistent within a session — carried from
   `Session.ClientConnID`.
5. Partial mocks are never emitted. If `Session.IsMockIncomplete()`
   returns true, `EmitMock` silently drops. Parsers that do their own
   batching should consult `IsMockIncomplete` before expensive
   mock-construction work.

## Violating the contract

- **Wrong timestamp**: mock selected at wrong point in test → flaky
  replay. Most common real-world failure mode.
- **Wrong Kind**: dispatcher routes to wrong replay decoder →
  decode error surfaced to the test.
- **Missing ConnectionID**: connection-scoped state (prepared stmts)
  unmatchable → replay produces wrong results silently.
- **Partial mock emitted despite `IsMockIncomplete`**: replay matches
  against truncated payload → byte-level diff failure.

When in doubt, diff your V2 output byte-for-byte against the legacy
parser's output for the same input traffic. Parity tests are the
authoritative check.

## Change log

- 2026-04-24 — initial version, written alongside the V2 architecture
  landing (PR #4113).
