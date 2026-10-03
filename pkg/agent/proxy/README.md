# pkg/agent/proxy — record-mode proxy architecture

This package implements keploy's record-mode proxy: the userspace
process that eBPF hooks redirect application TCP connections to. Its
job is to forward bytes transparently between the application and the
real upstream (database, API, etc.) while observing the traffic and
producing mocks for replay.

The package is split into two eras that coexist during the V2 rollout.

## V2 architecture (recommended for new parsers)

```
    real client ──┐                                          ┌── real dest
                  │                                          │
              +---v-----------------------------------+ +----v---+
              |  relay goroutines (sole writers)      | |        |
              |  Read → stamp → Write → tee           | |        |
              +---------+--------+---------------+----+ +--------+
                        |        |
              ingressChan  egressChan     (Chunk with ReadAt / WrittenAt)
                        |        |
                  +-----v--------v------+
                  |   FakeConn pair     |    Write() → ErrFakeConnNoWrite
                  +----------+----------+
                             |
                  +----------v----------+
                  |   supervisor.Run    |    panic firewall + hang watchdog + mem cap
                  +----------+----------+
                             |
                             v
                  parser.RecordOutgoing   (receives session.V2)
                             |
                             v
                    session.EmitMock
```

Split responsibilities:

- **`relay/`** — sole owner and writer of real sockets. Reads from each
  real peer, stamps `time.Now()` at the syscall boundary, writes the
  bytes to the opposite peer, and tees a copy of the
  [`Chunk`](fakeconn/chunk.go) into the parser's
  [`FakeConn`](fakeconn/fakeconn.go) via a bounded channel. Drops tees
  on memoryguard pressure, per-connection cap, or channel full — the
  real-byte path keeps flowing regardless.
- **`fakeconn/`** — read-only `net.Conn` that the parser consumes.
  `Write` always returns `ErrFakeConnNoWrite`, so a parser accidentally
  writing to the peer fails loudly rather than corrupting traffic.
- **`directive/`** — typed messages the parser sends back to the relay
  (via `session.Directives`) to request mid-stream operations: TLS
  upgrade (`KindUpgradeTLS`), abort (`KindAbortMock`), finalize
  (`KindFinalizeMock`), pause/resume. Replaces direct `*tls.Conn`
  manipulation in parsers.
- **`supervisor/`** — wraps each parser call with:
  - `defer recover()` → panic is caught, no goroutine leak, no socket
    close.
  - Activity-based watchdog → if no bytes forwarded and no mocks emitted
    for `HangBudget` (default 60 s) while pending work is outstanding,
    the parser is declared hung. Activity-based (not absolute) so
    30-second LLM responses and `pg_sleep(45)` don't falsely trip it.
  - Memory cap — per-connection buffered-to-parser bytes are tracked;
    breach triggers abort.
  - Goroutine accounting — parser-spawned helpers register for cancel
    on abort.
  - Incomplete-mock gate — if a chunk is dropped at the tee (or a
    parser cannot decode an exchange cleanly), `MarkMockIncomplete` sets
    the session's flag. The next mock `EmitMock` is handed, or that a
    parser drops through `LeaveOutIfIncomplete`, takes the flag and is
    left out, so a partial mock never reaches storage. It is reported,
    not silent (`ReportLeftOut`): `RecordOrphanWindow` over its
    `[ReqTimestampMock, ResTimestampMock]`, a count in the recording's
    summary (`mocks_left_out`), and a WARN with the reason,
    rate-limited per cause (the reason up to its first colon). The span
    leaves out the test cases over the exchange that are not saved yet
    when the parser gets to it. The parser runs behind the traffic (a
    full capture buffer behind, for `per_conn_cap`), and proxy mode
    checks each test case as it streams it, so one streamed before then
    is saved without the mock and fails replay with `no_mocks`; the WARN
    says so. The flag leaves out the next mock emitted, which, behind a
    full buffer, can be an exchange from before the lost chunk. A parser
    that takes the flag itself (`TakeMockIncomplete`) calls
    `ReportLeftOut` for each exchange it leaves out; one that knows
    which exchange it cannot record calls `ReportLeftOut` for it instead
    of setting the flag, as the MySQL recorder does for a command that
    does not decode or that the replayer cannot serve. The mock after it
    is recorded as usual. A parser that returns on an exchange it cannot
    record, instead of going on to the next, reports that exchange with
    `ReportStoppedOn` before it returns, as the HTTP recorder does for a
    request or response that does not decode, and the MySQL recorder for
    the command it stops recording a connection in (lost framing of the
    client's stream, or a response it cannot frame and cannot take the
    connection up again after, as from a client that pipelines its
    commands; a response it cannot frame otherwise costs that exchange
    alone, reported with `ReportLeftOut`, and the recording goes on from
    the client's next command). That is one rule for
    every parser: the exchange is counted in `mocks_left_out` and said at
    WARN like any mock left out; its span runs from its request to the
    stop, since what the parser had not read yet is lost with it; and a
    mark on the flag is taken with it and names the cause, since no
    later `EmitMock` takes it. A mark set instead reports nothing. The
    connection is left out from the stop on, from the moment the stop is
    stamped: `Session.StoppedAt`, the first of the parser's stop, its
    death or retirement, and a capture hole, read from the clock once,
    tells `Session.OnStop`, and the dispatcher opens that span there,
    before the supervisor or the dispatcher logs the stop. That span
    does not cover the exchange the parser, behind the traffic, stopped
    on; the exchange's span ends at the same instant, so they meet. A
    parser that stops without reporting the exchange (it panics, hangs,
    or returns an error without `ReportStoppedOn`) loses what it had
    captured and not recorded, the exchange it was in and every exchange
    queued behind it, with no span and no count. The stop is logged
    apart from it (the dispatcher's `parser retired` WARN, or the
    parser's own when its error wraps `ErrReported`), at WARN only while
    the recording runs: as the recording itself stops
    (`Session.RecordingStopping`), the connection ends with it, no span
    opens after the stop, and neither WARN goes out. A request the
    server's stream ends without answering is not such an exchange, and
    the HTTP recorder reports nothing for it, mark or not: with no
    response there is no mock to lose. It is most often the keep-alive
    idle-close race, which the app's HTTP client retries on a new
    connection, where it is recorded. Response bytes the capture lost
    stopped the connection's capture, which leaves out the test cases it
    carries from the loss on itself, and a request the relay could not
    write to the server (`write_error`) is that same race.
    `MarkMockComplete` is never called after `EmitMock`: it would clear
    a mark set while `EmitMock` delivered, and the mock that mark was
    for would be recorded partial.
- **`proxy_v2.go::recordViaSupervisor`** — dispatcher entry for V2
  parsers. Builds the relay + supervisor + session, invokes the
  parser, and on `FallthroughToPassthrough` drops the parser while
  **keeping the existing relay forwarding raw bytes on the real
  sockets until peer close** — it deliberately does not call
  `globalPassThrough`, which would introduce a read-loop gap
  (exactly the stall V2 was designed to eliminate). User traffic
  continues regardless of parser state.

## Safety invariants

Numbered to match `PLAN.md` at the repo root.

| # | Invariant | Enforced by |
|---|-----------|-------------|
| I1 | Transparent forwarding: every byte reaches its peer in order, or the connection is torn down by the peer's own timeout — never by keploy. | Split ownership. Relay is sole writer. FakeConn.Write is a runtime error. |
| I2 | Parser failures are local: panics / hangs / OOM in a parser never affect other connections, and never affect the faulting connection's byte path. | Supervisor panic firewall + activity watchdog + memory cap. |
| I3 | Fallback always available: any connection can drop to raw passthrough at any instant. | On `FallthroughToPassthrough`, `recordViaSupervisor` drops the parser but keeps the existing relay forwarding raw bytes end-to-end until peer close — no handoff gap, no replacement read loop. |
| I4 | Partial mocks are left out, and each is reported. | `MarkMockIncomplete` + the `EmitMock` / `LeaveOutIfIncomplete` gate, which reports the mock it leaves out (`ReportLeftOut`: `RecordOrphanWindow`, a count, a rate-limited WARN). Chunk-drop in the tee sets the flag. A parser that knows which exchange it cannot record reports it with `ReportLeftOut` itself, and one that returns on one with `ReportStoppedOn`: every mock a parser leaves out is counted, the exchange it stopped on included; what a connection carried after its recording stopped is in the orphan spans. The span leaves out only the test cases not saved by the time the parser reports it; one saved before lacks the mock, and the WARN says so. |
| I5 | Timestamp monotonicity per connection. | `Chunk.ReadAt` / `Chunk.WrittenAt` stamped at the syscall boundary in the relay; parsers must read these, never call `time.Now()` themselves. Lint rule at `tools/lint/no_timestamp_in_parser/` enforces. |
| I6 | Bounded resources per connection. | `Config.PerConnCap` on the relay tee. Supervisor tracks parser-owned bytes. |
| I7 | Kill-switchable. | `util.DefaultKillSwitch` — env `KEPLOY_DISABLE_PARSING=1`, `SIGUSR1`, or `Trip()`. Consulted per new connection in `handleConnection`. |
| I8 | eBPF hook and userspace proxy are coupled: proxy down → hook stops redirecting. | **Partial** — coordinated shutdown sequence clears proxy-state maps before listener close. A kernel-side `proxy_ready` gate requires BPF source changes (the source lives outside this repo) and is deferred. |

## Parsers: how to migrate to V2

1. Add `IsV2() bool` returning `true` on your parser type. This
   implements the `integrations.IntegrationsV2` capability interface
   and opts the parser into the new dispatch path.
2. Split your `RecordOutgoing` into a dispatcher:

   ```go
   func (p *MyParser) RecordOutgoing(ctx context.Context, s *integrations.RecordSession) error {
       if s.V2 != nil {
           return p.recordV2(ctx, s.V2)
       }
       return p.recordLegacy(ctx, s)   // original body, preserved
   }
   ```

3. Implement `recordV2(ctx, sess *supervisor.Session) error`:
   - Read via `sess.ClientStream.ReadChunk()` and
     `sess.DestStream.ReadChunk()` — do NOT access real sockets.
   - Use `chunk.ReadAt` for `ReqTimestampMock` and `chunk.WrittenAt`
     for `ResTimestampMock`. Never call `time.Now()` for mock
     timestamps (lint-enforced).
   - For mid-stream TLS, send
     `directive.UpgradeTLS(destCfg, clientCfg, reason)` on
     `sess.Directives` and wait for an `Ack` on `sess.Acks`.
     On `!ack.OK`, return the error — the supervisor aborts and the
     dispatcher falls through to passthrough, which leaves out the rest
     of the connection and says so. Do not mark the incomplete-mock flag
     first: no mock is emitted after the return to take the mark.
   - Emit mocks via `sess.EmitMock`. The gate and post-record hook
     chain run automatically.

4. The legacy branch still exists in the dispatcher for a parser that
   does not implement `IntegrationsV2`, but nothing in tree is in that
   state on the record path. There is no env knob that forces it: the
   `KEPLOY_NEW_RELAY` rollback switch has been removed.

Reference implementations: `integrations/http/recordv2.go`,
`integrations/mysql/recorder/record_v2.go`,
`integrations/generic/encode_v2.go`. A detailed walkthrough lives at
`docs/contributing/parser-migration-guide.md`.

## Tests

- Foundation unit tests: `fakeconn/` 14, `directive/` 3 groups,
  `supervisor/` 20, `relay/` 22, `util/kill_switch*` 9+.
- End-to-end integration: `v2_integration_test.go` drives real bytes
  through a Relay + Supervisor with happy / panic / hang parsers and
  asserts the I1–I5 / I7 invariants.
- Chaos e2e: `tests/e2e/chaos-broken-parser/` — docker-compose'd
  Postgres + a broken-parser build tag; asserts the app's queries
  keep succeeding through a parser panic via supervisor fallback.

All unit tests pass under `-race`.

## Rollout knobs

- `KEPLOY_NEW_RELAY` — **REMOVED.** It forced V2-capable parsers onto
  the legacy path as a global rollback. Every parser on the record path
  is now V2, and the legacy path it selected hangs EOF-delimited peers
  (it waits for both copy directions instead of tearing the second
  down), so the rollback traded one half-close defect for another. A
  value left in the environment is ignored and logged once at startup.
- `KEPLOY_DISABLE_PARSING=1` / `SIGUSR1` / admin endpoint — disable
  parsing entirely for new connections; existing connections drain.
  Routes everything to raw `globalPassThrough`. Kill switch for
  incidents where the parser layer is the suspect.

## Legacy path

Parsers without `IsV2()`, or with `IsV2()` returning false, run
unchanged through the original dispatcher branch in `handleConnection`.
They still use `util.Recover(logger, client, dest)` which closes the
SafeConn wrappers (no-op on the real sockets but parsers' own
goroutines see the closes). Bugs in legacy parsers are not protected
by the supervisor. The long-term plan is for every parser to migrate
to V2 and the legacy path to be deleted.

---

## Legacy documentation

The text below describes the pre-V2 proxy package. It is retained for
reference until all parsers have migrated to V2.

This package includes modules that the `hooks` package utilizes to
redirect the outgoing calls of the user API. This redirection is
done with the aim to record or stub the outputs of dependency calls.