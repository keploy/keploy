# chaos-broken-parser — e2e regression guard

This test proves invariants **I1–I3** from `PLAN.md` under chaos
conditions:

| Invariant | Statement |
|-----------|-----------|
| **I1**    | The parser supervisor is a panic firewall: a parser that panics mid-stream never crashes the keploy process. |
| **I2**    | On panic, the dispatcher falls through to `globalPassThrough` (raw byte relay) via `FallthroughToPassthrough`. |
| **I3**    | The application's database connection keeps working: queries issued after the panic continue to succeed end-to-end. |

The matching unit-level proof is
`pkg/agent/proxy/v2_integration_test.go::TestV2_PanicDoesNotBlockTraffic`,
which uses `net.Pipe()`. This directory adds the end-to-end case with
a real Postgres 16 server in a Docker container, to catch regressions
that unit-level `net.Pipe()` can't.

## What the test does

1. `docker compose up -d` — starts `postgres` (with seeded probe data
   from `init.sql`) and a `psql-client` sidecar the harness exec's
   into.
2. (Under the `chaos_broken_parser` build tag) the harness stands up
   an in-process keploy proxy with a **synthetic Postgres parser that
   panics on its first chunk read**. The parser satisfies
   `integrations.IntegrationsV2` with `IsV2() == true` so the
   dispatcher routes it through the supervisor, and is registered at
   a priority strictly greater than `integrations.POSTGRES_V2` so it
   wins the priority race in `p.integrationsPriority`.
3. The harness fires 100 `SELECT N;` queries with 50 ms spacing
   through the proxy port.
4. On exit the harness evaluates:
   * I3: `successes >= 99` (we tolerate at most 1 loss — the one
     carrying the deliberately-panicking parse attempt),
   * I2: the log contains the canonical message
     `parser supervisor triggered passthrough fallback`,
   * I1: the log contains NO `panic:` token (any escaped panic is a
     regression).

## Running the test

### Via `go test` (preferred — respects CI's opt-in gate)

```sh
# Default: the test SKIPs unless both the e2e gate and Docker are present.
go test ./tests/e2e/chaos-broken-parser/...

# Opt in via env var...
KEPLOY_E2E=1 go test -v ./tests/e2e/chaos-broken-parser/...

# ...or via build tag. Either works.
go test -v -tags e2e ./tests/e2e/chaos-broken-parser/...
```

### Directly via the harness binary

```sh
# Dry run — just verifies the harness compiles+parses its flags.
go run ./tests/e2e/chaos-broken-parser/harness/ --dry-run

# Full run (requires docker + docker compose plugin on PATH).
go run ./tests/e2e/chaos-broken-parser/harness/

# Chaos run with the in-process broken parser (needs the V2
# supervisor packages to exist — see "Status on this branch" below).
go run -tags chaos_broken_parser ./tests/e2e/chaos-broken-parser/harness/
```

### Expected log lines on a passing run

```
bringing up compose stack (project=chaos-broken-parser)
broken-parser: chaos parser registered; proxy listening on 127.0.0.1:<port>
...
parser supervisor triggered passthrough fallback    <-- I2 evidence
...
query results: successes=100 failures=0 (min required=99, of 100)
log scan: fallback-marker=true panic-escaped=false
PASS: parser panic did not break DB connection; passthrough fallback observed
```

## Status on this branch (`feat/proxy-v2-foundation`)

The V2 supervisor + relay + fakeconn packages referenced by the
`broken_parser.go` file **do not exist yet** on this branch. The
spec names:

* `pkg/agent/proxy/fakeconn/` — parsers read `Chunks` from here.
* `pkg/agent/proxy/supervisor/` — the panic firewall.
* `pkg/agent/proxy/relay/` — sole socket writer; hosts `TLSUpgradeFn`.
* `pkg/agent/proxy/proxy_v2.go::recordViaSupervisor` — dispatcher.
* `pkg/agent/proxy/integrations/integrations.go::IntegrationsV2` —
  parsers opt in via `IsV2() bool`.
* `pkg/agent/proxy/v2_integration_test.go` — the matching unit test.

…but only `globalPassThrough` exists on `feat/proxy-v2-foundation` as
of the time this scaffolding landed. The task spec was explicit that,
in that state, the right move is to ship the test scaffolding with a
clearly-marked stub so a follow-up can finish it without a second
round of spec archaeology.

**What compiles cleanly today:**

* The harness itself (`go run ./tests/e2e/chaos-broken-parser/harness/`)
  — brings up compose, drives queries, tails logs, evaluates the
  pass/fail predicate. Queries flow **directly** to the postgres
  sidecar, not through a keploy proxy. This currently FAILS the
  supervisor-fallback assertion (correctly — no supervisor, no
  fallback) so it reports as a controlled failure rather than a
  false-positive pass.
* The chaos-tagged parser stub
  (`go run -tags chaos_broken_parser ./tests/e2e/chaos-broken-parser/harness/`)
  — same as above but also returns `errChaosNotYetWired` from
  `startBrokenParserProxyIfEnabled` so the failure message mentions
  the missing V2 packages explicitly.
* The `go test` wrapper (`chaos_test.go`) — SKIPs by default,
  respects `KEPLOY_E2E=1` / `-tags e2e`, SKIPs again when docker is
  unavailable. Currently fails (not skips) once docker + e2e tag are
  present, for the same "supervisor isn't wired" reason above.

**Follow-up checklist** to land the real test. Each item is a
single-line edit to `harness/broken_parser.go` once the relevant
package exists:

1. Replace the body of `startBrokenParserProxyIfEnabled` with:
   1. Construct a parser implementing
      `integrations.IntegrationsV2` whose `IsV2()` returns true and
      whose `MatchType` sniffs the Postgres startup packet
      (length prefix followed by protocol version `0x00 0x03 0x00 0x00`
      at bytes 4..8).
   2. Have the V2 chunk reader panic on first invocation — this is
      the "broken parser".
   3. Call `integrations.Register("chaos_pg", &Parsers{Initializer:
      initChaosPG, Priority: 10000})` so the synthetic parser
      outranks `POSTGRES_V2` in `p.integrationsPriority`.
   4. `proxy.New(...)` on an ephemeral TCP port, wired to a zap
      logger whose `WriteSyncer` is the harness' `logSink`.
   5. Swap the `-h postgres` argument in `driveQueries` for
      `-h 127.0.0.1 -p <proxyPort>` so queries flow through the
      fakeconn pipeline.
2. Delete the `STATUS ON THIS BRANCH` block and
   `errChaosNotYetWired` from `broken_parser.go`.
3. Delete the "Status on this branch" section in this README.
4. Verify: `go test -v -tags e2e ./tests/e2e/chaos-broken-parser/...`
   prints the expected log lines above.

## Known limitations

* **Docker-only.** The compose stack uses Linux-only images and bind
  mounts; macOS and Windows developers should rely on the Linux CI
  path. The `go test` wrapper skips on non-Linux GOOS.
* **Not wired into CI yet.** Per the task spec, CI integration is a
  follow-up to avoid accidentally gating every PR on docker
  availability before this test can produce a signal. The wrapper
  already has the right skip semantics; adding it to
  `.github/workflows/*.yml` is a one-line `go test -tags e2e
  ./tests/e2e/chaos-broken-parser/...` step.
* **No eBPF.** The harness stands keploy's proxy up **in-process**
  rather than booting the full agent with its eBPF hooks. This is
  deliberate: the invariants under test (panic firewall, passthrough
  fallback, connection liveness) are properties of the
  supervisor/relay/dispatcher tier and don't exercise the eBPF
  traffic-redirection path. Running the full agent would require
  `CAP_SYS_ADMIN` / privileged mode, which is orthogonal.
* **`go.mod` untouched.** The harness doesn't use a Postgres client
  driver (no `lib/pq`, no `pgx`); queries go via `docker compose
  exec psql-client psql …`. This keeps the test self-contained
  without widening the module graph.

## File map

* `docker-compose.yml` — compose stack (postgres + psql-client).
* `init.sql` — seed data for the probe table.
* `harness/main.go` — orchestrates compose, drives queries,
  evaluates assertions.
* `harness/broken_parser.go` — `//go:build chaos_broken_parser` — the
  in-process V2 wiring (stub today; see "Status on this branch").
* `harness/broken_parser_stub.go` — `//go:build !chaos_broken_parser`
  — no-op for the default build.
* `chaos_test.go` — `go test` wrapper; SKIPs unless opted in AND
  docker is available (the file is a regular `_test.go` file in
  package `chaos_test`).
* `e2e_tag_enabled_test.go` / `e2e_tag_disabled_test.go` — flip
  `e2eTagEnabled` based on the `e2e` build tag.
