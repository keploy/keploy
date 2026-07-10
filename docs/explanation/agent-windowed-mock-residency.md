# Agent-side windowed, disk-backed mock residency

Status: implemented (Layer 1). Branch: `fix/agent-windowed-mock-residency`.

## Problem

In Kubernetes auto-replay, k8s-proxy spins an ephemeral `rpl-` pod (app + keploy
agent sidecar) and streams the recorded mock pool to the agent via `/storemocks`.
The agent was OOM-killed at its ~1 GiB limit, restarted, orphaned the replay
session, and wedged the whole auto-replay pipeline.

The already-shipped streaming `StoreMocks` (v3.5.85) removed the *decode
transient* (the encoded copy that briefly coexisted with the decoded pool) but
not the root cause: the agent holds the **entire delivered pool resident** for
the whole replay and uses the per-test time window only as a **matching** filter,
never as a **residency** filter.

Two facts make this fatal:

- `ClientMockStorage{filtered, unfiltered}` (`pkg/service/agent/agent.go`) is
  published once and never trimmed during a replay. `UpdateMockParams` copies the
  full pool per test and hands a window subset to
  `MockManager.SetMocksWithWindow`; the out-of-window mocks stay resident.
- The new-release "historical smart-set" regression downloads the whole per-app
  mock blob and replays it through the same path — the heaviest pool.

## Design (Layer 1)

Promote the time window from a *matching* filter to a *residency* filter, on the
agent side only (replay loop, `StoreMocks` send path, and k8s-proxy unchanged):

- At ingest (`StoreMocksStream`), **window-eligible per-test mocks are kept on disk to
  an agent-local temp file** and indexed by request timestamp; the resident
  reference is dropped. Reusable mocks (config/session/connection) and ineligible
  per-test mocks (missing/invalid timestamps) stay resident.
- At `UpdateMockParams`, the agent **loads only the mocks the existing filter
  would keep for this call** from disk, then runs the unchanged filter +
  `SetMocksWithWindow`.

Resident RAM becomes `O(config/startup + one test's window)` for every mock kind,
independent of pool size — fixing both the per-tick and the historical OOM.

### Disk eligibility

A mock is kept on disk iff (after `DeriveLifetime`): `Lifetime == LifetimePerTest`
**and** it has valid, non-zero request and response timestamps with
`res >= req`. Everything else stays resident, because the filter routes it
without a window check:

- reusable (`LifetimeSession`/`LifetimeConnection`, i.e. `Metadata["type"]` is
  `config`/`connection`) — matched across the whole session, never windowed;
- missing/inverted-timestamp per-test mocks — the filter keeps/drops them by
  rules that don't consult the window.

### Mode-aware load (correctness by construction)

`UpdateMockParams` has three modes; the on-disk load mirrors each exactly so the
filter's output is byte-for-byte identical to the old full-resident path:

| mode | load | why |
|---|---|---|
| mapping-based (`UseMappingBased`) | `LoadByNames(params.MockMapping)` | mapping selects by name, ignoring the window |
| effective-strict window | `LoadWindow(after,before)` ∪ `LoadBefore(firstWindowStart)` | strict keeps in-window + startup band; the stale band and `req>end` are simply not loaded — exactly what strict drops |
| lax (default) | `LoadAll()` | lax **promotes** out-of-window per-test mocks to the session tier; loading only the window would lose shared-fixture reuse. Lax is not the OOM path, so correctness wins over the RAM bound here |

"Effective-strict" mirrors the filter's own decision:
`pkg.IsStrictMockWindow(agentStrict)` where
`agentStrict = params.StrictMockWindow && !isWindowedProxy`. On the production
`WindowedProxy` path `agentStrict` is forced false (strict is enforced by
`SetMocksWithWindow`), so the effective agent-filter strictness — and therefore
the RAM bound — is driven by the **`KEPLOY_STRICT_MOCK_WINDOW`** env override.

> **Operational requirement:** to get the RAM bound on the `WindowedProxy` path,
> run the replay agent with `KEPLOY_STRICT_MOCK_WINDOW=1`. `StoreMocksStream`
> gates on `pkg.IsStrictMockWindow(false)` and **only builds the on-disk store
> when strict is in effect** — so under lax mode the store never engages and
> per-test mocks stay fully resident, **byte-for-byte the prior behavior (no
> regression, no bound)**. This is deliberate: lax promotes out-of-window
> per-test mocks to the session tier, so it needs the whole per-test set every
> test; keeping them resident is strictly better than reloading the pool from
> disk each test. `strictMockWindow` defaults to `true` (`config/config.go`);
> `KEPLOY_STRICT_MOCK_WINDOW=0` (env) or `test.strictMockWindow: false`
> (keploy.yaml) opt out — env wins over config.

The rare resident per-test mocks (ineligible) are always merged into the loaded
set (`dedupByName`, dedup by unique mock name).

## Key files

- `pkg/agent/proxy/diskmocks.go` — `DiskMocks`: gob-per-record temp file + an
  in-memory `{reqTsNano, offset, len}` index and a `name -> entry` map;
  `Add`, `Finalize` (sort by ts), `LoadWindow`/`LoadBefore`/`LoadByNames`/
  `LoadAll`, `EarliestReqTs`, `Close`; `EligibleForDisk` predicate.
- `pkg/service/agent/agent.go` — `ClientMockStorage.diskMocks`; `StoreMocksStream`
  routes eligible per-test mocks to disk; `finalizeClientMocks` closes the
  superseded generation's on-disk store and seeds the freeze anchor from the earliest
  on-disk timestamp too; `UpdateMockParams` builds the per-test input via
  `loadPerTestMocks`.

## Correctness / edge cases

- **Freeze anchor.** On-disk mocks often carry the earliest (app-boot) request
  timestamp. `finalizeClientMocks` folds `DiskMocks.EarliestReqTs()` into the
  anchor so it is not skewed later.
- **Startup band.** `SetMocksWithWindow` routes `req < firstWindowStart` to the
  startup tree; `LoadBefore(firstWindowStart)` reloads that band so app-boot DB
  calls still replay.
- **Window inclusivity.** `LoadWindow` uses `[start,end]` inclusive on both ends,
  matching `SetMocksWithWindow` (`!Before(start) && !After(end)`).
- **Pointer identity.** The filter already `DeepCopy`s each per-test mock, so
  trees never held the resident pointers; fresh-decoded pointers per window are
  consistent within a test. Reusable mocks (identity-sensitive: HitCount,
  MarkMockAsUsed) stay resident, preserving identity across tests.
- **Legacy blob `StoreMocks`.** No on-disk store (`diskMocks == nil`);
  `loadPerTestMocks` returns the full resident slice — behavior unchanged.
- **Supersede / leak.** `finalizeClientMocks` closes the previous generation's
  on-disk store; `Close` removes the temp file and is idempotent. StoreMocks is called
  once per test-set before that set's sequential `UpdateMockParams` loop, so a
  load never races a close in practice; if it ever did, `Load*` returns a clean
  error rather than mis-serving.
- **Best-effort.** If the temp store can't be created, or a single mock's disk
  write fails, that mock stays resident and ingest proceeds — never lost.
- **No other readers.** `ClientMockStorage.filtered`/`clientMocks` are read only
  by `StoreMocks`, `StoreMocksStream`, `finalizeClientMocks`, `UpdateMockParams`.

## Layer 2 (interval-scoped prune) — investigated, NOT warranted

The original idea was to prune prior-interval per-test mocks before re-streaming.
Grounding in `MockYaml.UpdateMocks` (`pkg/platform/yaml/mockdb/db.go`) shows this
is unnecessary: the auto-replay prune (`RemoveUnusedMocks=true`,
`pruneBefore = replay start`) already **drops** any per-test mock that is not
`config`, not consumed by a passing test this tick, not recorded after replay
start, and not in the startup band. So prior-interval per-test mocks that aren't
re-consumed are already pruned at each tick's end; the "accumulation" is a bounded
one-tick lag, not unbounded growth. Layer 1 bounds agent RAM regardless. Changing
`pruneBefore` to an interval floor would risk dropping mocks a still-pending
(not-yet-replayed) test in the same continuously-recording test-set needs — a real
correctness hazard for a marginal streaming saving. Deferred unless a measured
stream-volume problem justifies it.

## Verification

- Unit: `pkg/agent/proxy/diskmocks_test.go` (window inclusivity, startup-band
  exclusivity, by-name, all, earliest-ts, close-safety, eligibility);
  `pkg/service/agent/storemocks_stream_test.go::TestStoreMocksStream_PerTestMocksGoToDisk`
  (residency split + freeze-anchor-from-disk) plus the existing
  `MatchesLegacy` parity test.
- Pending: memory-behavior test (large synthetic pool → resident ≈
  O(config+window)); kind-lab integration (no OOM, identical HitCounts, historical
  path); live pprof on the replica agent `:6060` at rising RSS.

## Rollout

Land in `keploy`, re-vendor into `enterprise` and `k8s-proxy`, ship client +
agent in lockstep. Set `KEPLOY_STRICT_MOCK_WINDOW=1` on the replay agent.
