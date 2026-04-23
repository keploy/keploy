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
  - Incomplete-mock gate — if any chunk is dropped at the tee,
    `MarkMockIncomplete` is set; subsequent `EmitMock` calls on that
    session are silently dropped so partial mocks never reach storage.
- **`proxy_v2.go::recordViaSupervisor`** — dispatcher entry for V2
  parsers. Builds the relay + supervisor + session, invokes the
  parser, and on `FallthroughToPassthrough` drops the parser and
  hands the real sockets to `globalPassThrough`. User traffic
  continues regardless of parser state.

## Safety invariants

Numbered to match `PLAN.md` at the repo root.

| # | Invariant | Enforced by |
|---|-----------|-------------|
| I1 | Transparent forwarding: every byte reaches its peer in order, or the connection is torn down by the peer's own timeout — never by keploy. | Split ownership. Relay is sole writer. FakeConn.Write is a runtime error. |
| I2 | Parser failures are local: panics / hangs / OOM in a parser never affect other connections, and never affect the faulting connection's byte path. | Supervisor panic firewall + activity watchdog + memory cap. |
| I3 | Fallback always available: any connection can drop to raw passthrough at any instant. | `recordViaSupervisor` routes `FallthroughToPassthrough` results to `globalPassThrough`. |
| I4 | Partial mocks are dropped. | `MarkMockIncomplete` + `EmitMock` gate. Chunk-drop in the tee sets the flag. |
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
     On `!ack.OK`, call `sess.MarkMockIncomplete("tls upgrade failed")`
     and return the error — the supervisor aborts and the dispatcher
     falls through to passthrough.
   - Emit mocks via `sess.EmitMock`. The gate and post-record hook
     chain run automatically.

4. Legacy path stays untouched. During rollout, `KEPLOY_NEW_RELAY=off`
   forces every parser to the legacy path.

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

- `KEPLOY_NEW_RELAY=off` / `KEPLOY_NEW_RELAY=0` — force every parser,
  including V2-capable ones, onto the legacy path. Global V2 rollback.
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