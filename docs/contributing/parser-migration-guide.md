# Migrating a parser to the V2 architecture

**Audience**: engineers porting an existing record-mode parser (in
keploy, keploy/integrations, or keploy/enterprise) from the legacy
`net.Conn`-based path to the V2 FakeConn + supervisor architecture.

## Why

The V2 architecture (see `pkg/agent/proxy/README.md` and `PLAN.md`) gives
every migrated parser three guarantees for free:

1. **Parser panics never break the app's TCP connection.** The
   supervisor catches the panic, drops the partial mock, and the
   dispatcher hands the real sockets to `globalPassThrough`.
2. **Parser hangs are detected.** An activity-based watchdog declares
   the parser hung after 60 s of no progress with pending work, and
   forces the same fallback.
3. **Timestamp correctness.** Chunk timestamps are stamped at the
   real-socket boundary in the relay; the parser cannot accidentally
   skew them by buffering or async-decoding.

Migration is **additive and opt-in**. The legacy code path stays
intact; the parser routes between old and new based on whether the
dispatcher populated `RecordSession.V2`.

## The pattern, in full

Every parser migration follows the same shape. Apply it mechanically
unless the parser has mid-stream TLS (Postgres v2, MySQL) — those
need the directive-channel extension documented below.

### Step 1 — Add the capability marker

In your parser's type declaration file (often `myproto.go`):

```go
// IsV2 opts this parser into the V2 FakeConn-based record path.
// The dispatcher in handleConnection performs a type assertion
// against integrations.IntegrationsV2; when IsV2 returns true,
// RecordOutgoing is invoked inside supervisor.Run.
func (p *MyProto) IsV2() bool { return true }
```

The marker is per-type. Return `true` only once the parser's
`recordV2` is implemented and tested.

### Step 2 — Split `RecordOutgoing` into a dispatcher

Rename the current body to `recordLegacy` and put a tiny router
in the interface-method slot:

```go
func (p *MyProto) RecordOutgoing(ctx context.Context, s *integrations.RecordSession) error {
    if s.V2 != nil {
        return p.recordV2(ctx, s.V2)
    }
    return p.recordLegacy(ctx, s)
}

// recordLegacy is the exact body that was previously RecordOutgoing.
// Do NOT refactor it during migration. Keep the legacy path bit-for-bit
// stable so parity tests can compare V2 against it.
func (p *MyProto) recordLegacy(ctx context.Context, s *integrations.RecordSession) error {
    // ... existing code, unchanged ...
}
```

### Step 3 — Implement `recordV2`

The V2 session (`*supervisor.Session`) exposes:

| Field / method | Purpose |
|-----------------|---------|
| `sess.ClientStream *fakeconn.FakeConn` | Bytes the real client sent. Read via `Read` or `ReadChunk`. |
| `sess.DestStream *fakeconn.FakeConn` | Bytes the real destination sent. |
| `sess.Directives chan<- directive.Directive` | Send control messages (TLS upgrade, abort, finalize). |
| `sess.Acks <-chan directive.Ack` | Receive acks for directives. |
| `sess.Mocks chan<- *models.Mock` | Low-level mock channel (prefer `EmitMock`). |
| `sess.EmitMock(m) error` | Emit a mock (runs hook chain). If the incomplete-mock flag is set, it takes the flag, leaves the mock out and reports it. |
| `sess.MarkMockIncomplete(reason)` | Set the incomplete-mock flag: the next mock emitted is left out and reported. If you know which exchange you cannot record, report it with `ReportLeftOut` instead: the flag leaves out the next mock, which may be a healthy one. On a path that returns, report it with `ReportStoppedOn`: no mock is emitted after it to take the mark, and the exchange would go unreported. A request whose response never starts, because the server's stream ended first, is not an exchange you cannot record: there is no mock to lose, so report nothing (see below). |
| `sess.IsMockIncomplete() bool` | Query before expensive work. A mock you then drop goes through `LeaveOutIfIncomplete`. |
| `sess.LeaveOutIfIncomplete(m) bool` | Drop a mock without emitting it: takes the flag and reports the mock (its kind and times are enough). |
| `sess.TakeMockIncomplete() (string, bool)` | Take the flag yourself, to scope a mark to the exchanges it hit. You must then report each exchange you leave out with `ReportLeftOut`. |
| `sess.ReportLeftOut(m, reason)` | Report an exchange you leave out (its kind and times are enough): after `TakeMockIncomplete`, once per exchange the mark hit, with the reason you took; or, without the flag, for an exchange you know you cannot record, with your own reason. Reports its span (`[ReqTimestampMock, ResTimestampMock]`; none for a mock with no request time, which is still counted), counts it, logs the rate-limited WARN and clears pending work; does not touch the flag. `RecordOrphanWindow` alone counts and logs nothing. |
| `sess.ReportStoppedOn(kind, reqTs, reason)` | Report the exchange you stop on, just before you return with an error on it (a request or response that does not decode, framing you lost in it). One rule for every parser: reported as `ReportLeftOut` reports one, spanned from its request to the stop (`StoppedAt`: the first of your stop, your death or retirement and a capture hole, where the span of what the connection carries after it starts, open from the moment the stop is stamped), and under the relay's mark when one is on the flag, which it takes. Without it, the exchange you were in and every exchange queued behind it are lost with no span and no count. Not for the end of a stream or a recording's stop, which leave nothing out. |
| `sess.MarkMockComplete()` | Clear the flag after an exchange you record nothing for. Never after `EmitMock`, which takes the flag itself: a mark set while it delivered is the next mock's. |
| `sess.AddPostRecordHook(h)` | Front-of-chain wrapper hook. |
| `sess.Logger *zap.Logger` | Pre-scoped with connection fields. |
| `sess.Ctx context.Context` | Supervisor-managed lifetime. Respect it. |
| `sess.RecordingStopping() bool` | Whether the recording itself is stopping. A stop of your connection then costs the recording nothing: it ends with the recording, and no span opens after it. If your error wraps `supervisor.ErrReported`, your own log of the stop takes the place of the dispatcher's `parser retired` WARN: log it at WARN only while this is false, and at Debug once it is true, as the dispatcher does. |
| `sess.Opts models.OutgoingOptions` | Config (bypass rules, passwords, TLS configs, noise). |

#### Reading request/response bytes

Use `ReadChunk` for timestamps, `Read` for byte streams:

```go
chunk, err := sess.ClientStream.ReadChunk()
if err != nil {
    if errors.Is(err, io.EOF) || errors.Is(err, fakeconn.ErrClosed) {
        // Normal close or supervisor abort — return cleanly.
        return nil
    }
    return err
}
// chunk.ReadAt is the canonical request-first-byte timestamp.
```

The same holds when `DestStream` ends before any of a request's response:
return cleanly and report nothing, even with the incomplete-mock flag set.
There is no response, so no mock is lost. It is most often the keep-alive
idle-close race (the upstream closes a pooled connection as idle as the app
sends on it), and the app's client retries on a new connection, where the
call is recorded. Reporting it (`ReportStoppedOn`) would leave out every test
case in flight at that moment. Response bytes the capture lost are not yours
to report there: a parser that cannot re-align after a hole is no longer fed,
and the capture leaves out the test cases the connection carries from the
loss on itself.

#### Pairing a request with its response: `NextRequest`

A request/response parser starts every exchange with
`sess.NextRequest(kind, false)`: it waits for the client's next request
and returns its first chunk without taking it (read the request on as
you would), and it sets the server stream's floor to that chunk. Server
bytes captured before a request are never its answer, and the floor
drops them however late they reach you. On a connection whose capture
began after it was established (`Session.JoinedMidConnection`, which
the producer sets once, when it makes the session), where the server
stream starts with them, captured before the connection's first
request, they answer a request the capture does not have (one in flight
when the capture began): that run is reported once (`ReportLeftOut`).
Every other run is dropped with a Debug line, with nothing left out for
it there. It answers no request: on a connection captured from its first
byte (the relay's), nothing was sent before the capture began, so bytes
before its first request are the server's own, and a later run was sent
between two requests (an error or a timeout the server sent on its own,
such as MySQL's ERR at `wait_timeout` or an idle-close 408). Or it is
what is left of an answer you stopped reading and left out, which you
report yourself (`ReportLeftOut`) as you do: the MySQL recorder leaves
out a response it cannot frame, goes on from the client's next command,
and skips the rest of that response by capture time first
(`fakeconn.FakeConn.SkipThrough`, which cuts the stream before the
floor); the floor drops only what that skip left. The bytes captured
before the connection's first request are the server stream's start,
in the floor's run there however they leave the stream (the floor's
read or such a skip), and nothing captured after that request is in
that run, whatever floor is in force when you first read the stream:
an answer in flight is reported over its own bytes. A parser that
returns without reading the server's stream past its start (a client
that sends only requests with no answer and closes, such as a pool's
COM_QUIT, or closes in the middle of a request) never has the floor
drop it there, so `defer sess.EndExchanges()` where your parser
starts, to run however it returns: on a joined connection it reads the
server's stream on until its start is behind it, and the answer in
flight is reported all the same. A connection on which the client
sends no request at all reports nothing: with no request, nothing
orders the server's bytes against one. The order is
the chunks' connection-wide capture sequence (`fakeconn.Chunk.ConnSeq`),
which every producer numbers (`fakeconn.ConnSeqOf`): a client chunk after
every byte the server had sent the producer when the chunk was read, read
yet or not (the relay: `connseq.Upstream`); never compare timestamps to
order the two directions. A parser that goes on to a
request with an earlier answer still to read (MySQL's held PREPARE)
passes `true` for as long as that is so, and the floor stays where it
is. See the HTTP/1 and MySQL V2 recorders.

For byte-stream-oriented protocols (HTTP/1), you can pass the FakeConn
to a `bufio.Reader` — it satisfies `net.Conn`. Caveat:
`bufio.Reader.ReadBytes` can over-consume past your framing boundary
(e.g. into the next request in a pipeline). If that matters, use
`ReadChunk` directly and do your own buffering.

#### Emitting mocks

```go
mock := &models.Mock{
    Kind: models.MyProto,
    Spec: models.MockSpec{
        ReqTimestampMock: firstClientChunk.ReadAt,
        ResTimestampMock: lastDestChunk.WrittenAt,
        MyProtoReq:       parsedReq,
        MyProtoResp:      parsedResp,
        Metadata:         map[string]string{"type": "myproto"},
    },
    ConnectionID: sess.ClientConnID,
}
if err := sess.EmitMock(mock); err != nil {
    return err
}
```

See `docs/reference/mock-matcher-contract.md` for the exact shape
the replay matcher consumes. Mock output must be byte-equivalent
to the legacy path for the same input — write a parity test.

#### Timestamp rule (I5)

**Never call `time.Now()` for `ReqTimestampMock` / `ResTimestampMock`.**
Always source from `chunk.ReadAt` / `chunk.WrittenAt`. Log-line and
telemetry use of `time.Now()` is fine — prefix the call site with
`// allow:time.Now` to suppress the lint.

Enforced by `tools/lint/no_timestamp_in_parser/`. Run locally:
```
go run ./tools/lint/no_timestamp_in_parser/cmd/no_timestamp_in_parser ./...
```

### Step 4 — Mid-stream TLS (only if your protocol needs it)

Postgres's SSLRequest and MySQL's `CLIENT_SSL` are the two known
cases. SMTP's STARTTLS would be another if we add support.

Pattern:

```go
// 1. Read the pre-TLS prelude bytes from the FakeConn and emit a
//    config mock tagged with Metadata["tls_stage"] = "prelude".
// ...

// 2. Request the upgrade. destCfg is a *tls.Config; keploy acts as
//    TLS client to the real destination. clientCfg is a *tls.Config;
//    keploy acts as TLS server presenting the MITM cert to the real
//    client. The relay's injected TLSUpgradeFn owns the MITM cert
//    chain; clientCfg can be a minimal non-nil value to signal
//    "yes, upgrade the client side".
sess.Directives <- directive.UpgradeTLS(destCfg, clientCfg, "myproto tls_start")

// 3. Wait for the ack.
var ack directive.Ack
select {
case ack = <-sess.Acks:
case <-sess.Ctx.Done():
    return sess.Ctx.Err()
}
if !ack.OK {
    // The supervisor falls through to passthrough on the error, which
    // leaves out the rest of the connection and says so. No mark on the
    // incomplete-mock flag: no mock is emitted after this to take it.
    return fmt.Errorf("tls upgrade: %w", ack.Err)
}

// 4. From this point, subsequent chunks on ClientStream and
//    DestStream are post-TLS plaintext. The relay did the handshake
//    on the real sockets; you just keep reading.
```

For the TLS config builders, **use the system trust store** by
default (i.e. do not set `InsecureSkipVerify: true`). CodeQL flags
`InsecureSkipVerify`, and the supervisor's fallback-to-passthrough
handles the case where the upstream cert doesn't verify — traffic
still flows, the mock is dropped. See
`pkg/agent/proxy/integrations/mysql/recorder/record_v2.go:buildDestTLSConfigV2`
for the reference pattern.

### Step 5 — Handle protocol-specific lifecycles

- **HTTP/1 keepalive / pipelining**: loop reading request → response
  pairs, each started with `sess.NextRequest`, until it returns
  `io.EOF` or `ErrClosed`. See `pkg/agent/proxy/integrations/http/recordv2.go`.
- **MySQL / Postgres multi-phase**: explicit state machine — handshake,
  optional TLS upgrade, auth, query loop. Reuse existing wire decoders
  that accept a `net.Conn` or `io.Reader` — they work on FakeConn
  unchanged. See `pkg/agent/proxy/integrations/mysql/recorder/record_v2.go`.
- **Mongo / gRPC / HTTP/2**: per-stream state machines. Read frame
  headers from the chunk, branch on opcode, accumulate until a
  complete message is buffered.

### Step 6 — Tests

Add tests in the same directory as your parser, with a `v2_test.go`
suffix or in a separate file:

```go
// TestMyProto_RecordV2_HappyPath — drive canned bytes through a
// fakeconn pair; assert mock shape and chunk-derived timestamps.
// TestMyProto_RecordV2_Parity — feed identical bytes into legacy
// and V2; assert mocks are equivalent on non-timestamp fields.
// TestMyProto_RecordV2_TLSUpgrade — if your protocol has one.
// TestMyProto_IsV2 — guards the capability marker.
```

Use `fakeconn.New(ch, nil, nil)` to construct FakeConns directly and
push `Chunk` values on the channel with known `ReadAt`/`WrittenAt`
values (e.g. `time.Unix(1000, 0)`) to prove you're not calling
`time.Now()`.

For mid-stream TLS, plug a stub directive processor: drain
`sess.Directives`, push back an `Ack{OK:true}` (or `OK:false` for the
failure test) on `sess.Acks`.

### Step 7 — Run the gate

From the repo root:

```
go build ./...
go vet ./...
gofmt -l <your parser dir>
go test -race -count=1 -timeout 60s ./<your parser dir>/...
go run ./tools/lint/no_timestamp_in_parser/cmd/no_timestamp_in_parser ./<your parser dir>/...
```

All clean, all green. Commit with DCO sign-off:

```
git commit -s -m "feat(<proto>): native-migrate record path to V2 FakeConn architecture"
```

## Reference migrations

Three parsers are migrated on PR #4113 — use them as templates:

- **HTTP/1** — `pkg/agent/proxy/integrations/http/recordv2.go`. No TLS
  directive; simplest shape. Parity test against `parseFinalHTTP`.
- **MySQL** — `pkg/agent/proxy/integrations/mysql/recorder/record_v2.go`.
  Mid-stream TLS via `directive.UpgradeTLS`. Full handshake + auth +
  query state machine.
- **Generic** — `pkg/agent/proxy/integrations/generic/encode_v2.go`.
  Concurrent reader goroutines pair request/response.

## Things that bite people

1. **Don't call `session.Ingress.Write(...)`**. In V2 mode, `Ingress`
   and `Egress` are nil — your reference to them nil-derefs. The relay
   is the only writer. If your legacy code writes a response to the
   client, you don't need that in V2 — the relay already forwarded it.
2. **Don't close FakeConns yourself**. The supervisor closes them on
   abort via `SessionOnAbort`. Your parser just reads until EOF or
   `ErrClosed` and returns.
3. **Don't spawn helper goroutines without `sess.Ctx`**. They'll leak.
   Either use `sess.Ctx` directly, or register via
   `supervisor.RegisterGoroutine()` (available when direct supervisor
   access is wired — today use `sess.Ctx`).
4. **Don't skip the parity test**. Byte-equivalence with the legacy
   path is the only way to prove replay still works. Corner cases
   (chunked transfer, TLS renegotiation, streaming responses) are
   exactly where V2 and legacy tend to drift silently.
5. **Don't suppress `time.Now()` indiscriminately**. The lint rule's
   `// allow:time.Now` exemption is for log lines and metrics.
   `ReqTimestampMock` / `ResTimestampMock` must come from chunks.
6. **Don't remove parser-internal `memoryguard.IsRecordingPaused`
   calls from the legacy path.** The relay handles it for V2 at the
   tee; legacy parsers still need their checks because they're not
   behind the relay. Redundancy disappears naturally as parsers
   migrate.
7. **Set `IsV2() bool { return true }` last**, after `recordV2` is
   landed and tested. A parser returning true without a working
   `recordV2` will crash in the dispatcher.

## Rollout knobs

- `KEPLOY_NEW_RELAY` — **REMOVED.** It used to force every parser back
  to the legacy path. A value left in the environment is ignored (and
  logged once at startup), so it is no longer an incident lever.
- `KEPLOY_DISABLE_PARSING=1` / `SIGUSR1` disables parser dispatch
  entirely; every connection goes to raw passthrough. This is the
  incident lever.

## When in doubt

- `pkg/agent/proxy/README.md` — architecture overview.
- `docs/reference/mock-matcher-contract.md` — what the replay matcher
  actually reads.
- `PLAN.md` (repo root) — the multi-phase design.
- PR #4113 — the foundation + three reference migrations.

Ask in `#keploy-record-v2` (or equivalent) before breaking new ground;
the core V2 maintainers will usually have seen the corner case before.
