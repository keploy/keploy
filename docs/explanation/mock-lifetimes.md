# Mock lifetimes — config, per-test, and connection mocks

> Status: in-progress — tracked under the mock-lifetime unification
> plan. This page documents the user-visible model; internals are in
> `pkg/models/lifetime.go`.

Every keploy mock has a **lifetime** that tells the replay-side
matcher how long that mock is useful and whether it should be consumed
on match. Three lifetimes exist:

| Lifetime | YAML tag (`spec.metadata.type`) | Consumed on match? | Window-filtered? | Examples |
|---|---|---|---|---|
| Session | `config` | No — reusable across tests | No | HTTP auth (AWS SigV4, OAuth refresh), MySQL handshake, Postgres `SET`/`SHOW`, Mongo `hello`/`isMaster`/`ping`, Redis `HELLO`/`AUTH`, gRPC reflection, Kafka `ApiVersions`/coordination |
| Connection | `connection` | No — reusable within one connID | No | Postgres `Parse` (prepared-statement setup), MySQL `COM_STMT_PREPARE` |
| Per-test | anything else (common: `mocks` for MySQL/Postgres data queries, `HTTP_CLIENT` for HTTP, or empty for legacy recordings) | Yes — deleted from pool on match | Yes, under strict-window mode | Everything else: HTTP app requests, MySQL queries, Postgres `Bind`/`Execute`, Kafka `Produce`/`Fetch`, Redis data commands, Mongo CRUD |

Only `config` and `connection` are special tag values. Every other
tag (including an absent one) is treated as per-test — recorders pick
human-readable labels like `mocks` or `HTTP_CLIENT` to aid debugging;
the routing only cares whether the tag matches the two reserved
values.

If you're reading this because of a phantom replay failure, skip to
["Troubleshooting"](#troubleshooting) below.

## Why three lifetimes?

Replay has two failure modes that only show up at scale:

1. **Cross-test bleed** — test B accidentally matches a data-plane
   mock that test A recorded. Subtle: tests pass individually but fail
   when run as a batch in a different order. Fixed by per-test mocks
   being consumed on match and window-filtered.
2. **Prepared-statement evaporation** — test A's connection runs
   `PREPARE ... AS ...`. Test B's execute on the SAME connection
   references that statement by id. A strict per-test window would
   drop the prepare from test A's pool → execute fails. Connection
   lifetime solves this without reintroducing bleed: prepares are
   reusable only within the connID that owns them.

Session and per-test capture the common cases; connection handles the
long tail of stateful-protocol features that don't fit either.

## How keploy classifies at record time

The recorder inspects protocol-level signals and writes one of the
three YAML tags (or no tag for per-test). You do not need to
configure this.

| Protocol | Session (`type: config`) | Connection (`type: connection`) |
|---|---|---|
| HTTP / HTTP/2 | AWS SigV4 SQS discovery/admin (`GetQueueUrl`, `GetQueueAttributes`, `ListQueues`), SigV4-signed auth refreshes | — |
| MySQL | Handshake, auth | `COM_STMT_PREPARE` *(Phase 2.5)* |
| Postgres v2 | `StartupMessage`, `PasswordMessage`, `SASLInitialResponse`, `SASLResponse`, `GSSResponse`, `Authentication`; session queries `SET`/`SHOW`/`DEALLOCATE`/`DISCARD`/`RESET`/`UNLISTEN`/`select version()`/`select current_schema()`; empty-query `Parse` (driver PS-cache probe) | Non-empty `Parse` (prepared-statement registration) |
| Mongo v2 | `hello`, `isMaster`, `ping`, SCRAM (`saslStart`/`saslContinue`), `buildInfo`, `getLog`, `hostInfo`, `listDatabases` | — |
| Redis | `HELLO`, `AUTH`, `SELECT`, `CLIENT *`, `INFO`, `CONFIG`, `COMMAND`, `SUBSCRIBE*`, `RESET` | — |
| gRPC v2 | `/grpc.reflection.v1.*`, `/grpc.reflection.v1alpha.*` | — |
| Kafka | `ApiVersions`, `Metadata`, `SaslHandshake`, `SaslAuthenticate`, `FindCoordinator`, `JoinGroup`/`SyncGroup`/`Heartbeat`/`LeaveGroup`, `CreateTopics`/`DeleteTopics`, `DescribeConfigs`/`AlterConfigs`, `InitProducerId` | — |
| Generic / DNS | Everything | — |

If your protocol isn't listed, the mock is per-test by default. If
you believe a request should be session- or connection-scoped but
isn't being tagged, please open an issue on the keploy repo with a
minimal reproducer — it's a recorder-classification gap, fixable in
a small PR.

## Inspecting a recording

Open any test-set's `mocks.yaml` and look at `spec.metadata.type` on
each mock:

```yaml
version: api.keploy.io/v1beta1
kind: Http
name: config
spec:
  metadata:
    type: config           # ← session-scoped; reusable across tests
    operation: GetQueueUrl
  # ... the AWS SQS handshake response
---
version: api.keploy.io/v1beta1
kind: PostgresV2
name: connection
spec:
  metadata:
    type: connection       # ← connection-scoped; reusable within connID
    connID: conn-17
    requestOperation: Parse
    query: "SELECT id FROM users WHERE email = $1"
  # ... the ParseComplete response
---
version: api.keploy.io/v1beta1
kind: Http
name: mocks
spec:
  metadata:
    type: HTTP_CLIENT      # ← recorder-emitted; any non-special tag is per-test
    operation: POST
  # ... an app-issued outbound call
```

`type: config` → session (reusable across tests, never window-filtered).
`type: connection` → connection-scoped (requires a non-empty `connID`;
a `connection` mock without a `connID` falls back to session rather
than being consumed per-test, since consumption would break the
paired execute).

Any other tag — including `mocks`, `HTTP_CLIENT`, or none at all —
routes to per-test. Recorders pick human-readable labels for
debugging; only the two reserved values above change routing.

Two tag-independent overrides run BEFORE the tag switch:

1. **MySQL session-reusable commands** (`COM_PING`, `COM_STATISTICS`,
   `COM_DEBUG`, `COM_RESET_CONNECTION`) — promoted to session
   regardless of tag. Their responses are input-independent, and they
   are typically recorded at app startup (HikariCP pool warm-up, JDBC
   connection validation) BEFORE any test window begins, so leaving
   them per-test would have the strict-window pre-filter drop them.
2. **Untagged legacy-kind fallback** — recordings captured before the
   tag convention rely on a kind-based inference inside
   `DeriveLifetime` that routes untagged (empty-string `type`) HTTP /
   HTTP2 / MySQL / Redis / Postgres / PostgresV2 / Generic / DNS
   mocks to session. This fires ONLY when the tag is literally empty;
   an explicit non-canonical tag like `mocks` is honoured as per-test.
   The branch is telemetry-gated for deletion
   (`LegacyKindFallbackFires`); once it reads zero for a release
   cycle the fallback goes away and "empty tag ⇒ per-test" holds
   universally.

## Strict mode and the time window

Per-test mocks are subject to an outer-test **time window** under
strict mode. The window is the interval `[test-request-timestamp,
test-response-timestamp]` of the outer HTTP/gRPC test. A per-test
mock whose own request timestamp falls outside that window is
dropped — this is what prevents cross-test bleed.

Session and connection mocks are **never** dropped by the window
check.

Configure via `keploy.yaml`:

```yaml
test:
  strictMockWindow: true   # cross-test bleed prevention
```

Or via environment variable:

```bash
KEPLOY_STRICT_MOCK_WINDOW=1 keploy test -c "..."
```

**Default is `false`** in Phase 1. The PR that introduces
`LifetimeConnection` + strict-window infrastructure ships it as an
OPT-IN feature — many real-world apps legitimately reuse data-plane
mocks across tests (fixture-row SELECTs, long-session fuzzers), so
flipping the default would silently break those suites on upgrade.

Opt in via `test.strictMockWindow: true` in `keploy.yaml` or
`KEPLOY_STRICT_MOCK_WINDOW=1` in the environment. The env var OR-es
with the config: an enabling value forces strict on; an explicit
disabling value ("0") forces strict off regardless of the config.
Strict activation logs a one-shot Info message per agent process
naming both escape hatches, so hunts through docs are unnecessary.

The default will flip to `true` in a follow-up once every stateful-
protocol recorder classifies mocks finely enough (session vs per-test
for connection-alive commands, per-connection data mocks) that
legitimate cross-test sharing is encoded as Session/Connection
lifetime rather than implicit out-of-window reuse.

## Observability — HitCount

Session- and connection-scoped mocks have a per-mock atomic
`HitCount` exposed via `MockMemDb.SessionMockHitCounts()`. The
counter is bumped from keploy's `MarkMockAsUsed` path, which
matchers call after a successful match — per-parser coverage is
rolling out as matchers adopt the new MockMemDb surface (Phase 2
of the unification plan). Until every parser opts in, some
categories of session matches will not be reflected in the count;
a persistent zero on a session mock could mean either "never used"
OR "used, but the owning parser hasn't migrated yet."

Per-test mocks are consumed on match and their counter stays at 0
or 1 — not surfaced in telemetry.

Once every parser migrates, a persistent zero on a session mock
will be a reliable signal that the recording captured something
the app no longer issues (e.g., a stale AWS discovery call) and
the mock is safe to drop by re-recording.

## Troubleshooting

**"Strict mode breaks my prepared-statement app"**
With `LifetimeConnection` wired into both MySQL and Postgres matchers
(this PR), PREPARE/execute correlation works under strict mode as
long as your recording includes the Parse/COM_STMT_PREPARE frames with
a stable `connID`. If you're replaying an older recording that lacks
`type: "connection"` tagging, either:
- Re-record with the current keploy so Parse frames are tagged at
  capture time, or
- Opt out of strict mode: `test.strictMockWindow: false` (or
  `KEPLOY_STRICT_MOCK_WINDOW=0`).

**"My HTTP mocks are being consumed when I expected reuse"**
Check `spec.metadata.type` — if absent, they're per-test by default.
If you believe they should be session (e.g., a long-lived auth
refresh), either (a) upgrade keploy if the recorder classification
for your specific case landed in a newer release, or (b) open an
issue with the mock's request-line and headers so the recorder can
learn to tag it.

**"Replay summary says `[session mock] hit count 0`"**
A session mock was never matched across the entire test run. Usually
safe to re-record without it — the recording captured a one-off call
that your app no longer makes. If it's genuinely conditional
(matches only under certain inputs), leave it.

**"How do I know if my recording is relying on the legacy kind fallback?"**
The compat fallback is silent per-mock (a log there would swamp
debug output on large test sets) but is counted. The agent surfaces
the count as part of the replay-completion summary: a non-zero
"legacy kind fallback fires" number means at least that many mocks
in the recording did not carry a `metadata.type` tag and were
classified by the pre-tag kind-switch. Replay still works; to drop
the counter to zero, re-record with a current keploy release.
