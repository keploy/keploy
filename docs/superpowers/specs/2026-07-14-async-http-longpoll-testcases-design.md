# Async testcases — HTTP long-poll (egress) — design

> Status: draft for review — 2026-07-14
> Scope: slice 1 of a larger "async testcase" effort. This slice covers
> **HTTP long-poll egress consumers** only. Background producers
> (Kafka/NATS/RabbitMQ/PubSub push) and server-side (ingress) long-poll
> are explicitly deferred to later specs.

## 1. Problem

Keploy today assumes a **synchronous request-response spine**:

- **Ingress** (the app's incoming HTTP/gRPC request) → recorded as a
  **test case**: the thing that is *driven and asserted* at replay.
- **Egress** (the app's outbound calls to DB / queue / HTTP) → recorded
  as **mocks**: *inputs served back* to the app, matched inside that
  test's time window `[req_ts, resp_ts]`.
- Everything anchors to that window. The codebase already stretches this
  with "async straggle" tolerance (`GetPerTestMocksInWindow` tolerates
  response timestamps after the window end) and with mock lifetimes
  (per-test / session / connection / startup — see
  `docs/explanation/mock-lifetimes.md`).

Some real app behaviors do **not** fit a synchronous request-response
anchor. This effort tracks four async shapes at a conceptual level:

1. Asynchronous implementation, synchronous request-response behavior.
2. Asynchronous job processing (starting jobs).
3. Starting several queries and returning partial success.
4. Asynchronous streaming.

The two concrete cases motivating this work:

- **(i) Background producers** — the app opens a connection to a broker
  (Kafka/NATS/RabbitMQ/PubSub) in a background thread and *pushes* data
  on its own schedule. No incoming request triggers it, so there is no
  test window to anchor it to, and the produced payload is arguably the
  app's *output under test* — inverting the "egress = input mock"
  assumption.
- **(ii) Long polling** — a long-lived connection over which the app
  *polls* an upstream and receives data pushed at non-deterministic
  times. There is **no header** that marks a request as a long-poll, so
  it is indistinguishable at the wire level from a merely-slow ordinary
  request.

**This spec addresses case (ii), for the egress direction (the app is
the client), over HTTP.** Case (i) is a separate spec.

### Why it's hard

- **No ingress anchor.** The poll cycles fire from a background thread,
  not in response to any recorded ingress test, so there is no
  `[req_ts, resp_ts]` window to scope them.
- **No detection signal.** Long-poll requests carry no distinguishing
  header; they look like ordinary (slow) HTTP calls.
- **Full-fidelity assertion is wanted.** A single poll connection carries
  three roles simultaneously: the app→upstream **poll requests**
  (protocol, to assert), the upstream→app **data** (the stimulus, to
  serve as a mock), and the app's resulting **egress side-effects**
  (downstream behavior, to assert).

## 2. Locked decisions

These were settled during brainstorming and are treated as fixed for
slice 1:

| Question | Decision |
|---|---|
| Replay goal for async output | **Mock + assert** (full fidelity) |
| First scope slice | **Long polling** (producer case deferred) |
| Direction | **App is the client (egress)** |
| Pass/fail criterion | **Side-effects + protocol** both asserted |
| Test unit | **Per delivery/fetch** (empty/timeout polls are not testcases) |
| Replay delivery model | **App-paced, collapse idle** (compress empty polls; assert data-bearing poll *shape*, not exact poll *count*) |
| Transport (slice 1) | **Generic HTTP long-poll** (in-tree `http` integration) |
| Architecture | **First-class artifact + async-stream handling (A)** classified by **config lanes (C)**; heuristics are suggestion-only |
| Replay ordering | **Recording-order-anchored interleave** with the sync tests (not a trailing phase) |
| Intra-gap concurrency | **Serialize across lanes** (single active window; concurrent per-lane windows deferred) |

## 3. Core concept & data model

### 3.1 The reframe

A normal Keploy test is: *stimulus* = ingress request Keploy drives;
*reaction* = egress mocks consumed inside `[req_ts, resp_ts]`; *pass* =
every egress call matched a mock and every per-test mock was consumed.

An async testcase is the **same machine with a different window
source**:

```
 SYNC test:   [ ingress req ]───window──▶[ ingress resp ]
                     │  app makes egress calls, matched+consumed inside window

 ASYNC test:  [ app polls ]──▶[ upstream DELIVERS data ]───window──▶[ app's next poll ]
                                       │  stimulus served    │ app's egress side-effects
                                       │  as a mock          │ matched+consumed inside window
```

Instead of a parallel assertion engine, an async testcase **reuses
per-test mock windowing** — it defines the window as the *reaction
interval* `[delivery_ts, next_poll_ts)` on a poll connection, and the
stimulus is *serving the recorded delivery* rather than *driving an
ingress request*.

### 3.2 Artifact

New `Kind: "AsyncHttpPoll"`, one artifact per **data-bearing** delivery:

| Field | Role at replay |
|---|---|
| `Lane` | which configured lane produced this |
| `connID` | record-time per-connection identity (ordering key) |
| `Sequence` | per-lane order of this delivery within its lane's stream (cross-lane ordering within a gap uses the delivery timestamp `t_d`, not this field) |
| `AnchorAfter` | the preceding sync test's id, or `startup` — where this delivery sat in the sync-test sequence (see §6) |
| `PollRequest` (`HTTPReq`) | app→upstream poll — **asserted on shape** (protocol) |
| `Delivery` (`HTTPResp`) | upstream→app data — **served as the stimulus mock** |
| `SideEffects` (`[]Mock`) | egress the app made before its next poll — **served + asserted** via the existing per-test consume model |
| timing (`latencyMs`, `interArrivalMs`) | detection/telemetry only; **collapsed** at replay |

Empty/timeout polls are **not** testcases. The first empty exchange per
lane is captured as an **empty template** (a session-lifetime keep-alive
mock) used to keep the app's poller alive when no delivery is armed.

### 3.3 Storage

Async testcases are written to a dedicated `async/` file within the
test-set so the sync runner ignores them and they read as a distinct
artifact. The lane config and the empty template are stored alongside.

## 4. Configuration — async lanes

Config lanes are the **primary classifier** — the deliberate answer to
"no special headers." Detection is declarative, not guessed.

```yaml
async:
  lanes:
    - name: notifications
      match:
        host: "notify.internal.svc"   # host glob
        path: "/v1/poll*"             # path glob
      # optional emptiness predicate override; default = 204/304 or empty body
      empty:
        status: [204, 304]
        bodyEmpty: true
        # jsonPath: "$.messages"      # (future) treat empty array as empty
      # optional: params to treat as noise on the poll-shape assertion
      volatileParams: ["cursor", "offset", "since"]
```

A **heuristic detector** (see §5.4) may *suggest* lanes in the record
summary but never silently classifies traffic as async.

## 5. Record-side flow

Classification/segmentation runs as a **record-stop post-pass**, fed by
the existing `RecordHooks` (`AfterMockInsert` collects egress mocks;
`AfterTestCaseInsert` collects ingress test windows). A post-pass is
used (rather than inline) because segmentation needs the whole
per-connection picture and the full set of ingress windows.

Egress HTTP mocks already carry everything needed:
`Spec.Metadata["connID"]`, `Spec.ReqTimestampMock`,
`Spec.ResTimestampMock`, `Spec.Metadata["type"] == HTTPClient`
(see `pkg/agent/proxy/integrations/http/recordv2.go`).

### 5.1 Steps

1. **Lane match.** For each egress HTTP mock, test `(host, path)`
   against configured lanes. Group matches by `connID`; sort by
   `ReqTimestampMock` → a per-connection ordered exchange list.
2. **Delivery segmentation.** Within a lane connection, classify each
   exchange as *data-bearing* vs *empty* using the lane's emptiness
   predicate. Data-bearing → a delivery. The first empty exchange is
   captured as the lane's **empty template**.
3. **Side-effect attribution.** For a delivery at `t_d`, the reaction
   window is `[t_d, t_next_delivery)` (next data-bearing delivery on the
   same lane, or record-end). Collect **non-lane** egress mocks whose
   `ReqTimestampMock` falls in that window **and** outside every ingress
   test window (egress inside a sync test belongs to that test — no
   double counting) → `SideEffects`. This is the same time-window
   attribution heuristic Keploy already uses for sync tests; it is
   documented as a heuristic (see §7, edge cases).
4. **Anchor computation.** Compute `AnchorAfter` = the id of the last
   sync test whose window ended before `t_d`, or `startup` if `t_d`
   precedes the first sync test. This positions the delivery in the
   sync-test sequence for replay ordering (see §6).
5. **Emit** one `AsyncHttpPoll` testcase per delivery.

### 5.2 Detection assist (suggestion-only)

The same pass computes per-`connID` stats over *all* egress (median
`ResTs − ReqTs` latency, repeat-request ratio, ratio fired outside any
ingress window). Connections that look like long-polls but are not in
any configured lane are logged in the record summary as *"consider
adding lane …"*. They are never emitted as testcases and never silently
reclassified.

### 5.3 Zero-impact guarantee

If no `async.lanes` are configured, the post-pass is a no-op and
recording is byte-identical to today.

## 6. Replay-side flow — recording-order-anchored interleave

Replay honors the **recording-time order** between async deliveries and
the sync tests. It is a **serialized interleave**, not a trailing phase.

```
 drain gap(startup) ─▶ run T₁ ─▶ drain gap₁ ─▶ run T₂ ─▶ drain gap₂ ─▶ … ─▶ run Tₙ ─▶ drain gap(end)
                         │                       │
                    sync window            async windows (this gap's deliveries,
                                            in recorded Sequence order)
```

- The app **boots once**; its background poller runs throughout.
- **During a sync test window**, lane polls are served the **empty
  template** (keep-alive, no assertion) — a delivery is only ever served
  when its gap is armed.
- **Between sync tests** (a "gap"), the runner **arms** the async driver
  for exactly the deliveries whose `AnchorAfter` matches that gap. It
  waits (bounded per-delivery timeout) until the gap drains, asserts each
  delivery, then advances to the next sync test.

Because sync test `T_k`'s window closes before the gap's async windows
open, and those close before `T_{k+1}`, there is still **only one active
window at a time**. Recording-time order is honored **without** a
concurrent-windowing redesign of the MockManager.

**Consistency with collapse-idle:** we preserve recording-time
*order/position* relative to the tests, not absolute wall-clock delays.
Order is faithful; idle waits are compressed.

### 6.1 Async driver

Per lane, the driver holds the gap's ordered delivery list + a cursor.
On each poll the app issues on a lane connection:

1. **Assert poll shape** (protocol) — method/path/headers/query vs the
   recorded `PollRequest`, noise-filtered like existing matchers;
   `volatileParams` (cursor/offset/since) default to noise (or assert
   monotonic advance).
2. **Serve** `Delivery[cursor]` as the response (the stimulus).
3. **Open reaction window** — set the MockManager's current window to
   this delivery's bounds and load its `SideEffects` as per-test mocks.
   The window closes on the app's **next poll** on that lane (natural
   boundary) or a per-delivery timeout.
4. **On close** — assert every `SideEffect` was consumed (missing ⇒
   fail) and no unmatched egress occurred (unexpected ⇒ fail). Advance
   cursor.
5. **On exhaustion of the gap** — see §7 #1 (overflow behavior).

### 6.2 Live-connection mapping

At replay the app opens a *fresh* connection (new `connID`), so the
driver keys streams by **lane + order of appearance**, not the recorded
`connID`. Slice 1 assumes one primary connection per lane; multiple
connections on a lane map FIFO into one global `Sequence` (documented
limitation, §7 #8).

### 6.3 Multiple deliveries in one gap

- **Same lane, a burst of deliveries.** Naturally serial: an HTTP
  long-poll connection has one in-flight request at a time, so the app's
  own poll cadence drives it. Serve deliveries in `Sequence` order, one
  per poll; each **re-poll** simultaneously closes+asserts the previous
  delivery's window and fetches the next. Zero extra machinery.

  ```
  arm gap ─▶ poll ─▶ serve #1 ─▶ app side-effects ─▶ RE-poll (closes #1, serves #2) ─▶ … ─▶ empty ⇒ drained
  ```

- **Multiple lanes/connections in one gap (genuinely concurrent).**
  Slice 1 **serializes across lanes** by delivery timestamp `t_d`
  (record-time order across all lanes in the gap): arm one delivery at a
  time; a poll on a not-yet-armed lane gets the empty template and
  retries. (Within a single lane, ordering is the per-lane `Sequence`.) Keeps one active window. Cost: deliveries that
  truly overlapped at record time are linearized — fine for independent
  messages; side-effects that genuinely interleaved across lanes are
  flattened. Concurrent per-lane windows are deferred (§8).

### 6.4 Pass/fail

Each `AsyncHttpPoll` passes iff: poll shape matched **AND** all
`SideEffects` consumed **AND** no unexpected egress in the window.
Reported in the existing report structure, tagged async.

## 7. Edge cases & error handling

| # | Case | Behavior |
|---|---|---|
| 1 | App polls **more** than recorded (gap deliveries exhausted) | Serve the **first recorded delivery whose poll shape matches**, *reused* (not consumed), with an INFO log: `ordered deliveries exhausted for this gap; serving first matched delivery for this poll (reuse) — connID=<c> seq=<first-match>`. An overflow (reused) delivery opens **no reaction window and asserts no side-effects** (no recorded expectation at that position). The empty template is the fallback only when *nothing* shape-matches. |
| 2 | App polls **fewer** than recorded / never polls in an armed gap | Bounded per-delivery wait; undrained deliveries → `not-exercised` (fail/skip per config). Runner advances — replay never hangs. See §7.1 for causes. |
| 3 | **Poll-shape mismatch** (protocol fail) | Record the failure but **still serve** the delivery, so side-effect assertions still get exercised. `volatileParams` default to noise. |
| 4 | **Unexpected egress** in the reaction window | No matching `SideEffect` ⇒ fail (unexpected side-effect), same as sync no-match. |
| 5 | **Missing side-effect** | A recorded `SideEffect` unconsumed at window close ⇒ fail. |
| 6 | Poll-vs-side-effect ambiguity | Lane requests route to the async driver; only **non-lane** egress counts as a side-effect. The next poll closes the window *and* fetches the next delivery — never mis-attributed. |
| 7 | Reaction straggles past the next test | Serialization forces it to complete before `T_{k+1}` (accepted collapse-idle trade-off). |
| 8 | Multiple connections on the **same** lane at record | Merged into one global `Sequence` per lane, served FIFO over whichever live connection polls. Per-connection fidelity deferred. |
| 9 | Empty-template misclassification | Per-lane emptiness predicate configurable (status / body-empty / JSON path). Record summary lists deliveries-vs-empties per lane so the predicate can be tuned. |
| 10 | No background poller at replay | Gaps never drain → async testcases `not-exercised`, reported clearly (not a hang). |
| 11 | Strict-window interaction | The async reaction window is just another window source; session/connection/startup mocks (auth/handshake) remain available to side-effect calls. Empty template = session lifetime. |

### 7.1 Why "app polls fewer than recorded" happens

Not a corner case; real causes, all handled by the bounded wait →
`not-exercised` path, and tagged in the report where inferable:

1. **Poller cadence vs bounded wait.** The poller has its own rhythm
   (fixed interval, long-poll `max-wait`, exponential backoff). We
   collapse idle and are ready instantly, but the app may still be
   sleeping on its own timer before the next poll.
2. **State-dependent polling.** The app polls only under an internal
   condition (queue depth, feature flag, "stop after N"). That state is
   reached via mocked side-effects at replay and can diverge.
3. **Diverging side-effect response changes the loop.** A mocked
   side-effect (e.g. offset-commit) returning a "pause/stop" shape
   shortens or exits the poll loop.
4. **Processing crash.** A mocked side-effect returns a shape the app
   didn't expect → panic/exception in the consumer thread → poller dies.
5. **Shutdown timing.** End-of-run stops the app before the gap drains.
6. **Batching drift.** Record saw one fetch return an N-message batch
   (1 delivery = 1 poll); replay batching differently shifts poll counts.

## 8. Known limitations / future work

- **Concurrent per-lane reaction windows** — slice 1 serializes across
  lanes; true record-time cross-lane concurrency needs a MockManager
  multi-window change.
- **Per-connection stream fidelity** — multiple connections on one lane
  are merged FIFO; per-connection streams are not preserved.
- **Background producers (case i)** — Kafka/NATS/RabbitMQ/PubSub push is
  a separate spec.
- **Server-side (ingress) long-poll** — the app-as-server direction is a
  separate spec.
- **Keploy-paced push delivery** — for genuine server-push protocols
  (NATS/PubSub) rather than pull/long-poll.
- **Absolute-timing fidelity** — slice 1 collapses idle; a
  faithful-pacing mode is deliberately out of scope.

## 9. Testing strategy

- **Record-side classifier/segmenter (unit)** — table-driven over
  synthetic egress-mock sequences: lane glob matching, data-vs-empty
  segmentation, side-effect time-window attribution (incl. excluding
  in-sync-window egress), anchor computation. Mirrors
  `pkg/agent/proxy/integrations/http/recordv2_test.go`.
- **Async driver (unit)** — scripted poll sequence → assert correct
  delivery per poll, window open/close on re-poll, consume / missing /
  unexpected verdicts, overflow reuse + log, timeout → not-exercised,
  cross-lane serialization order.
- **Interleave scheduler (unit)** — assert drain-gap ordering and the
  **single-active-window invariant** (never two open at once).
- **E2E** — sample app with a couple of ingress endpoints + a background
  HTTP long-poll consumer that, per delivered message, does a DB write +
  an outbound HTTP call. Record → verify artifacts + anchors → replay
  passes → mutate a side-effect → verify the fail is caught. Follows the
  existing e2e harness style.
- **Regression** — apps with no async lanes configured record/replay
  byte-identically (zero impact when the feature is unused).

## 10. Open questions

- `not-exercised` default: hard **fail** or **skip**? (Proposed:
  configurable, default skip with a prominent summary count.)
- Should the overflow-reuse log (§7 #1) be rate-limited per lane to
  avoid flooding on a fast overflow poller? (Proposed: yes, one line
  per gap + a final count.)
- Bounded per-delivery wait default value and whether it should adapt to
  the observed record-time poll cadence.
