# Async testcases — general egress engine — design

> Status: draft for review — 2026-07-15
> Supersedes the HTTP-specific approach in
> `2026-07-14-async-http-longpoll-testcases-design.md`. That spec solved
> HTTP long-poll as a self-contained, transport-specific feature (config
> lanes + a record-stop post-pass + side-effect assertion). This spec
> reframes the same problem as a **transport-agnostic engine** that
> parsers plug into, marks async **inline at record** on the authority of
> user config, and **drops the side-effect graph**. HTTP is the first
> participant; MongoDB, Kafka, and other stream parsers plug in later
> through the same capability interface with **no engine changes**.

## 1. Problem

Keploy anchors everything to a synchronous request/response spine: an
ingress request is the *test*; egress calls inside its
`[req_ts, resp_ts]` window are *mocks*. Long-poll / background-consumer
traffic does not fit — the app polls an upstream from a background
thread and receives data at non-deterministic times, with **no ingress
anchor** and **no wire header** marking a request as async.

The prior spec addressed this for HTTP only, and coupled three things
that make it hard to generalize:

1. **Transport coupling.** Lanes were HTTP host/path; segmentation read
   HTTP timestamps and `HTTPClient` types directly. MongoDB/Kafka could
   not participate without rewriting the core.
2. **Position-as-detection.** It leaned on "egress fired outside a test
   window" to classify async traffic. Under real load, async polls
   interleave with heavy concurrent egress and frequently land *inside*
   test windows — the async traffic gets **masked** by volume, so a
   position heuristic misclassifies it.
3. **Side-effect assertion.** Each delivery carried an asserted
   downstream side-effect graph, attributed by a fragile time-window
   heuristic that misattributes genuinely-async side-effects that
   coincide with a sync test window.

This spec removes all three couplings.

## 2. Locked decisions

Settled during brainstorming; fixed for this slice.

| Question | Decision |
|---|---|
| Architecture | **Transport-agnostic engine** + an **`AsyncParser` capability interface** parsers opt into (mirrors the existing `IntegrationsV2` opt-in pattern) |
| First participant | **HTTP** (in-tree `http` integration); Mongo/Kafka plug in later, no engine change |
| What marks a mock async | **User-declared lane config is authoritative.** A mock is async **iff it matches a declared lane** — lane match is the *sole* discriminator. Position/timing/heuristics never decide async-ness |
| Detection heuristics | **Suggestion-only telemetry.** May recommend lanes in the record summary; never mark |
| When marking happens | **Inline at record** (on `RecordHooks.AfterMockInsert`), not a post-pass |
| Positioning | Each async mock is **anchored to its effective testcase** (the last test started at/before the async completion; its effect arms only from the *next* test) — used only for ordered/gated serving, never for detection |
| Verdict | **Serve always; assert request shape** (flag drift, serve the recorded response anyway). Count/timing **lenient** |
| Side-effects | **Dropped.** An async call is just an egress mock the app needs fulfilled in the right order; if unfulfilled the app fails or merely logs — nothing to assert as a graph |
| Replay availability | **Gated by anchor.** A mock anchored after `Tn` is armed once the run reaches position `n`; earlier requests get a parser-supplied keep-alive. Armed mocks stay armed across later gaps until consumed |
| Concurrency | **Free.** No reaction windows ⇒ no single-active-window constraint; each lane serves independently |
| Zero-impact | No lanes configured ⇒ engine is a no-op; record & replay byte-identical to today |

## 3. Core concept

### 3.1 Transport-agnostic engine

"Transport" = the wire protocol an egress speaks (HTTP, Mongo wire,
Kafka, MySQL…). The async **engine contains no protocol-specific code**.
It only ever needs three abstract answers, each delegated to the parser
that owns the lane:

| Engine asks (abstract) | HTTP | Mongo | Kafka |
|---|---|---|---|
| Does this egress belong to lane L? | host/path glob | namespace + op | topic |
| Does the live request match the recorded one? | method/path/query | command doc | fetch topic/offset |
| What is a "no data yet" response for L? | `204`/empty body | empty getMore batch | empty fetch response |

The engine owns only transport-agnostic concerns: **anchoring, arming by
position, ordered serving, the shape verdict, keep-alive fallback.**

### 3.2 Async is metadata on ordinary mocks

There is **no new artifact kind**. An async mock is an ordinary egress
mock with extra metadata stamped at record time. Existing storage,
matching, and lifetime machinery apply unchanged; the engine layers a
gated **async view** over the pool keyed by `(lane → seq-ordered list)`.

### 3.3 The reframe vs. the prior spec

```
 PRIOR:  delivery = {poll req (assert), data (serve), side-effects (serve+assert)}
         detected by config lane + "outside test window", segmented in a post-pass.

 THIS:   async mock = an ordinary egress mock + {lane, anchorAfter, asyncSeq}
         detected by config lane ALONE, stamped inline at record.
         replay: serve in seq order, gated by anchor; assert request shape only.
```

## 4. Configuration — async lanes

Transport-agnostic outer shape; the `match` block is **opaque to the
engine** and interpreted by the owning parser.

```yaml
async:
  lanes:
    - name: notifications
      type: http                    # which parser owns this lane
      match:                        # opaque to engine; parser-interpreted
        host: "notify.internal.svc"
        path: "/v1/poll*"
      volatileParams: ["cursor"]    # optional; passed to the shape matcher as noise
      notExercised: skip            # optional; skip|fail for undrained mocks (default skip)
    # future, no engine change:
    # - name: order-events
    #   type: kafka
    #   match: { topic: "orders", groupId: "billing" }
```

## 5. The plugin contract — `AsyncParser`

```go
// AsyncParser is the optional capability interface a parser implements to
// participate in async egress handling. The engine type-asserts against it,
// exactly like the existing IntegrationsV2 opt-in. Parsers that do not
// implement it are simply never async.
type AsyncParser interface {
    integrations.Integrations

    // MatchesLane reports whether an egress belongs to the lane. At record
    // the argument is the just-recorded mock; at replay it is the live
    // request wrapped as a mock. The parser interprets lane.Match in its own
    // protocol terms. This is the SOLE async discriminator.
    MatchesLane(m *models.Mock, lane AsyncLane) bool

    // MatchRequestShape compares a live request against a recorded async
    // mock's request, treating lane.VolatileParams as noise. ok=false with a
    // human-readable detail on drift. Reuses the parser's existing matcher.
    MatchRequestShape(live, recorded *models.Mock, lane AsyncLane) (ok bool, detail string)

    // EmptyResponse returns the parser's "no data yet" keep-alive payload for
    // the lane, served when no async mock is armed. Always synthesizable from
    // the protocol — never depends on a recorded empty exchange.
    EmptyResponse(lane AsyncLane) ([]byte, error)
}
```

```go
type AsyncLane struct {
    Name           string                       `yaml:"name"`
    Type           integrations.IntegrationType `yaml:"type"`  // owning parser
    Match          map[string]string            `yaml:"match"` // parser-interpreted
    VolatileParams []string                     `yaml:"volatileParams,omitempty"`
    NotExercised   string                       `yaml:"notExercised,omitempty"` // skip|fail
}

type AsyncConfig struct {
    Lanes []AsyncLane `yaml:"lanes"`
}
```

## 6. Data model — async metadata

Stamped on the mock's protocol-specific `Spec.Metadata`
(`map[string]string`, alongside the existing `connID` / `type` tags).

| Key | Meaning | Set when |
|---|---|---|
| `async` | `"true"` | record, on lane match |
| `lane` | lane name | record |
| `anchorAfter` | the *effective testcase*: last testcase started at/before the async completion, or `startup` (readability) | record |
| `anchorPos` | 1-based index of that effective testcase (0 = startup) — the value the arming check uses | record |
| `asyncSeq` | per-lane order counter (decimal string) | record |

```go
const (
    MetaAsync       = "async"       // "true"
    MetaAsyncLane   = "lane"        // lane name
    MetaAnchorAfter = "anchorAfter" // effective testcase Name, or "startup"
    MetaAnchorPos   = "anchorPos"   // 1-based index of effective testcase (0=startup)
    MetaAsyncSeq    = "asyncSeq"    // per-lane order, strconv.Itoa
)
```

## 7. Record-side — inline marking

Marking runs from the existing `RecordHooks`. The async recorder embeds
the no-op `BaseRecordHooks` and overrides two hooks: `AfterTestCaseInsert`
(track completed ingress testcases in order) and `AfterMockInsert` (stamp
async metadata). **Lane match is the sole discriminator; position never
decides async-ness — it only computes the anchor.**

```go
type AsyncRecorder struct {
    record.BaseRecordHooks
    cfg     AsyncConfig
    parsers map[integrations.IntegrationType]AsyncParser
    mu      sync.Mutex
    tests   []testWindow      // ingress windows (by start timestamp)
    seq     map[string]int    // per-lane counter
}

type testWindow struct {
    id        string
    startedAt time.Time // ingress request timestamp = window START
}

// AfterTestCaseInsert records each ingress testcase's window START.
func (r *AsyncRecorder) AfterTestCaseInsert(_ context.Context, info *record.TestCaseContext) error {
    r.mu.Lock()
    defer r.mu.Unlock()
    r.tests = append(r.tests, testWindow{
        id:        info.TestCase.Name,
        startedAt: info.TestCase.HTTPReq.Timestamp, // window start
    })
    return nil
}

// BeforeMockInsert stamps async metadata on any egress mock that matches a
// declared lane. No-op when no lane is configured (zero-impact guarantee).
// MUST be the Before hook: mockDB.InsertMock persists the mock, so a stamp
// applied in AfterMockInsert never reaches the recorded YAML (verified by the
// e2e). Testcase windows are tracked in AfterTestCaseInsert because
// TestCase.Name is only assigned by InsertTestCase (anchorPos is
// timestamp-derived and correct either way; the After hook makes the
// human-readable anchorAfter name correct too).
func (r *AsyncRecorder) BeforeMockInsert(_ context.Context, info *record.MockContext) error {
    if len(r.cfg.Lanes) == 0 {
        return nil
    }
    m := info.Mock
    for _, lane := range r.cfg.Lanes {
        p, ok := r.parsers[lane.Type]
        if !ok || !p.MatchesLane(m, lane) {
            continue
        }
        r.mu.Lock()
        r.seq[lane.Name]++
        seq := r.seq[lane.Name]
        // Anchor by the async COMPLETION time (response timestamp).
        anchorID, anchorPos := r.effectiveTestcase(m.Spec.ResTimestampMock)
        r.mu.Unlock()

        meta := m.Spec.Metadata // flat map on MockSpec; init if nil in real code
        meta[MetaAsync] = "true"
        meta[MetaAsyncLane] = lane.Name
        meta[MetaAnchorAfter] = anchorID
        meta[MetaAnchorPos] = strconv.Itoa(anchorPos)
        meta[MetaAsyncSeq] = strconv.Itoa(seq)
        return nil // matched a lane; done
    }
    return nil // no lane matched -> ordinary mock, untouched
}

// effectiveTestcase returns the last ingress test STARTED at or before ts (the
// async completion time) and its 1-based index. This is the "effective
// testcase" — the last test that ran on the OLD state before this delivery
// landed. A delivery arriving mid-test-2 anchors to test-2, so its effect is
// armed only once test-2 completes (visible from test-3) and never
// retroactively alters test-2. Returns ("startup", 0) if no test had started.
func (r *AsyncRecorder) effectiveTestcase(ts time.Time) (string, int) {
    id, pos := "startup", 0
    var best time.Time
    for _, w := range r.tests {
        if !w.startedAt.After(ts) { // startedAt <= ts
            pos++
            if id == "startup" || !w.startedAt.Before(best) {
                best = w.startedAt
                id = w.id
            }
        }
    }
    return id, pos
}
```

Consequences (documented constraints):

- **A lane endpoint is async everywhere.** Declaring a lane makes *all*
  its traffic async; the same endpoint cannot be sync in one place and
  async in another. (A background poller is a poller.)
- **Robust under load.** Because detection is the user's declaration, not
  a traffic-derived signal, no volume of concurrent egress can mask it.

## 8. Replay-side — the engine

The app boots once; pollers run throughout. The engine tracks
**position** = number of sync tests completed (the replay runner calls
`OnTestComplete` as each finishes). Each lane has an ordered stream and a
cursor.

```go
type laneStream struct {
    lane   AsyncLane
    mocks  []*models.Mock // sorted by asyncSeq
    cursor int            // next unconsumed index
}

// Engine is transport-agnostic: no HTTP/Mongo/Kafka knowledge.
type Engine struct {
    parsers  map[integrations.IntegrationType]AsyncParser
    streams  map[string]*laneStream    // by lane name
    order    map[string]int            // testcase id -> run index (for anchors)
    position int
    mu       sync.Mutex
    report   *Report
}

func (e *Engine) OnTestComplete() {
    e.mu.Lock()
    e.position++
    e.mu.Unlock()
}

// Decide is called by a parser's MockOutgoing once it has detected (via its
// own MatchesLane) that a live request routes to lane L. It returns the
// recorded mock to serve, or nil to signal "serve the parser's keep-alive".
// The engine touches no wire bytes — the parser encodes the response.
func (e *Engine) Decide(lane AsyncLane, live *models.Mock) (*models.Mock, error) {
    e.mu.Lock()
    defer e.mu.Unlock()

    s := e.streams[lane.Name]
    if s != nil && s.cursor < len(s.mocks) && e.isArmed(s.mocks[s.cursor]) {
        recorded := s.mocks[s.cursor]
        p := e.parsers[lane.Type]
        if ok, detail := p.MatchRequestShape(live, recorded, lane); ok {
            e.report.Pass(lane.Name, recorded)
        } else {
            e.report.Flag(lane.Name, recorded, detail) // serve anyway
        }
        s.cursor++
        return recorded, nil
    }
    return nil, nil // nothing armed -> caller serves EmptyResponse
}

// isArmed reports whether a mock's anchor has been reached at the current
// position. "startup" is armed from the very beginning.
func (e *Engine) isArmed(m *models.Mock) bool {
    a := specMetadata(m)[MetaAnchorAfter]
    if a == "startup" {
        return true
    }
    return e.position >= e.order[a]+1 // armed once the anchor test has completed
}
```

The parser side (HTTP shown; the async branch is identical for any
participant):

```go
func (h *HTTP) MockOutgoing(ctx context.Context, src net.Conn,
    dstCfg *models.ConditionalDstCfg, mockDb integrations.MockMemDb,
    opts models.OutgoingOptions) error {

    req := h.readRequest(src)
    if lane, ok := h.laneFor(req); ok { // uses h.MatchesLane over configured lanes
        recorded, err := h.engine.Decide(lane, wrapAsMock(req))
        if err != nil {
            return err
        }
        if recorded != nil {
            return h.writeRecordedResponse(src, recorded) // serve recorded resp
        }
        empty, err := h.EmptyResponse(lane)               // keep-alive
        if err != nil {
            return err
        }
        return h.writeRaw(src, empty)
    }
    return h.matchAndServe(ctx, src, mockDb, opts) // normal, non-async path
}
```

**Invariant that makes the cursor sound:** `asyncSeq` is assigned in
record order, and `anchorAfter` (= last completed test) is monotonically
non-decreasing over record time. So within a lane a later `asyncSeq` can
never carry an *earlier* anchor — cursor order and arm order agree, and a
single forward-only cursor + `isArmed` check is sufficient (no need to
scan the stream for an armed-but-out-of-order mock).

Behavior falls out of the above:

- **Gated availability** — `isArmed` refuses a mock until its anchor test
  has completed; earlier polls get `EmptyResponse`.
- **Ordered serving** — `cursor` advances through `asyncSeq` order.
- **Armed stays armed** — a slow poller that reaches an armed mock in a
  later gap still gets it; arming never revokes.
- **Lenient count** — extra polls get keep-alive; unconsumed mocks remain
  (see §9 #5).
- **Free concurrency** — independent `laneStream`s, no reaction windows.

## 9. Edge cases & error handling

| # | Case | Behavior |
|---|---|---|
| 1 | Nothing armed for a lane | Serve `EmptyResponse(lane)`. Parser-synthesized, so it always exists — closes the prior spec's "empty-template absence" gap. If a parser cannot synthesize one, it holds/delays the connection (natural long-poll timeout). |
| 2 | App polls a lane, nothing armed now or ever | Keep-alive indefinitely. Not a hang, not a failure. |
| 3 | Request-shape mismatch | Serve the recorded response anyway; record a FLAG (async-tagged) with the parser's detail. `volatileParams` excluded from the compare. |
| 4 | App polls *more* than recorded | Extra polls get keep-alive. Tolerated. |
| 5 | App polls *fewer* / never drains a lane | Unconsumed armed mocks remain; end-of-run `not-exercised` note per lane. Default `skip` (informational); `fail` opt-in via `notExercised`. |
| 6 | Multiple lanes concurrently | Independent arm pointers, no reaction windows → served independently. |
| 7 | Multiple live connections on one lane at replay | Engine keys by **lane, not connID** (fresh connIDs at replay); connections on a lane share the ordered stream FIFO. Per-connection fidelity deferred. |
| 8 | `anchorAfter=startup` | Armed from the start — bootstrap pollers work before the first test. |
| 9 | Lane endpoint called inside a test window at record | Still async (lane is authoritative); anchored to the last *completed* test. |
| 10 | Zero lanes configured | Engine no-op; record & replay byte-identical. |

## 10a. E2E findings (real `keploy record`/`test` against a long-poll sample)

Verified end-to-end with a sample app (ingress `/step` + background long-poll
consumer) under `sudo keploy record`/`test`:

- **Record ✅** — poll egress correctly stamped `async/lane/anchorAfter/
  anchorPos/asyncSeq`; effective-testcase anchor observed (startup→0,
  `get-step-2`→2, …). Bug caught + fixed: stamping must run in
  `BeforeMockInsert` (see §7).
- **Replay ✅ (partial)** — startup-anchored delivery served; unarmed polls got
  the `204` keep-alive; all sync tests pass; zero-impact holds.
- **Finding A — replay timing:** keploy replays the sync tests in ~ms, so a
  periodic background poller rarely polls into the sub-ms inter-test gaps.
  Mid-sequence deliveries therefore stay unarmed → keep-alive → not-exercised
  (skip=pass). Only startup / `--delay`-window deliveries are reliably served.
  A faithful demonstration of mid-sequence ordered serving needs either a
  fast/aligned poller or a keploy-paced delivery mode (future work).
- **Finding B — verdict visibility:** the engine computes the shape-flag and
  not-exercised counts in `Report()`, but nothing surfaces them in keploy's
  test summary/logs yet. Wiring `Engine.Report()` into the replay summary is
  required for the verdict (and any shape drift) to be visible.

## 10. Known limitations / future work

- **Per-connection stream fidelity** — multiple connections on one lane
  are merged FIFO; per-connection streams are not preserved.
- **Non-HTTP participants** — Mongo/Kafka `AsyncParser` implementations
  are follow-up slices (engine is ready for them).
- **Absolute-timing fidelity** — idle is collapsed; faithful wall-clock
  pacing is out of scope.
- **Cross-lane record-time ordering** — lanes are independent at replay;
  a global cross-lane order is not reconstructed (not needed without
  side-effect windows).

## 11. Testing strategy

- **Engine unit tests (transport-agnostic), driven by a *fake*
  `AsyncParser`** — anchoring & arm-pointer by position, gated
  availability (not served before anchor), armed-stays-armed across gaps,
  ordered serving by `asyncSeq`, shape pass/flag, keep-alive fallback,
  unconsumed → not-exercised. *This suite proves the engine has zero
  protocol code — it runs green with no real transport.*
- **Pluggability proof** — a second trivial fake parser implementing
  `AsyncParser`, asserting the engine accepts a non-HTTP transport with no
  engine changes. Guards against accidental HTTP coupling.
- **Record-side marking (unit)** — lane match → async metadata stamped;
  anchor correct incl. during-window and `startup`; non-lane egress
  untouched; zero lanes → nothing stamped. Mirrors
  `pkg/agent/proxy/integrations/http/recordv2_test.go` style.
- **HTTP `AsyncParser` (unit)** — `MatchesLane` globs + `volatileParams`,
  `MatchRequestShape` reusing the HTTP matcher, `EmptyResponse` incl. the
  synthesize-when-absent path.
- **E2E** — sample app: a couple of ingress endpoints + a background HTTP
  long-poll consumer. Record → verify async markers + anchors → replay
  serves in order (keep-alive before armed) → mutate a request shape →
  verify the flag fires. Follows the existing e2e harness.
- **Regression** — no lanes → record/replay byte-identical.

## 12. Open questions

- `not-exercised` default: confirm `skip` with a prominent summary count
  (vs. `fail`).
- Should `MatchRequestShape` FLAGs be rate-limited per lane on a fast
  poller (one line per gap + a final count)?
- Where the engine lives in the dispatch path (proxy dispatcher vs. a
  thin shim inside each parser's `MockOutgoing`) — §8 sketches the shim;
  a shared dispatcher hook may read cleaner. Decide during planning.
- Keep-alive cadence when a parser must hold the connection (case #1) —
  fixed delay vs. mirror the recorded inter-arrival.
