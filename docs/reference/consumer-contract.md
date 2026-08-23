# The consumer contract

`Kind: Consumer` test cases let `keploy record -c "./worker"` turn each message a
message-queue worker consumes into a test case, and `keploy test -c "./worker"`
replay it **with no broker**: the recorded delivery is handed back to the worker
and what the worker produced is asserted with per-field diffs.

This document is the reference for the **contract** — the types, the delivery
gate, the projector SPI and the judge. It is what a protocol parser implements
against, and what a person reading a green consumer suite needs in order to know
what that green actually claims.

---

## 1. What this repository ships, and what it does not

**It ships the contract and the judge, and no consumer protocol parser.**

> **Open decision, and this file's own tag gate.** Whether that split is the
> one keploy ships is **not settled** — it contradicts the strategy document's
> "at least the Kafka self-replay path must be donated to OSS", it is a
> one-way door once tagged, and it is a founder call, not an implementation
> detail. See **§9**, which states the contradiction and both answers. This
> section is what §9's decision edits.

| Piece | Where | Status |
|---|---|---|
| `models.CONSUMER`, `EffectView`, `ConsumerSpec`, `ConsumerArm`, `ConsumerResult`, the failure categories | `pkg/models/consumer.go` | here |
| The default-closed delivery gate, the projector registry, the unit recorder | `pkg/agent/proxy/integrations/consumer/` | here |
| The judge (`CompareEffects`) and the report rows it writes | `pkg/service/replay/consumer.go` | here |
| The agent routes, the agent client, the arm-and-await hook | `pkg/agent/routes`, `pkg/platform/http`, `pkg/service/replay` | here |
| A Kafka / Pulsar / SQS parser: wire codecs, role metadata, a `Projector`, a `Deliverer` | — | **not here** |

`pkg/agent/proxy/integrations/` in this repository contains `async`, `generic`,
`http` and `mysql`. There is no Kafka, Pulsar, Redis or Mongo parser in it, and
this contract does not add one. Nothing here can therefore record or replay a
consumer test **on its own**: with no parser stamping `role` metadata, the
recorder is never called, and with no `Deliverer` registered, an armed window has
nobody to deliver through.

That is deliberate, and it is stated here rather than left to be discovered:

- The judge, the contract and the report surface are here **because they are
  what a user must be able to read before trusting a green consumer suite**. A
  verdict nobody can inspect is not a verdict.
- The interfaces are designed so that a parser can be added — in this repository
  or another one — **without a second release of this contract**. Registration is
  runtime (`consumer.RegisterProjector`, `Gate.RegisterDeliverer`), the protocol
  is an opaque string, and protocol coordinates live in a map this package never
  interprets. Nothing here has to change to light it up.

---

## 2. One generic Kind, protocol in the spec

There is exactly one new `models.Kind`: `CONSUMER`. There is no `KAFKA` test
kind, no `PULSAR` kind and there will not be one.

A per-protocol Kind costs an arm in every `switch tc.Kind` in the tree — the
test-case encoder and decoder, the replay loop's window/judge/result arms, the
slug builder, the timestamp reader, the hook — roughly fifteen sites, per
protocol, forever, in every repository that consumes this module. One generic
Kind pays that once.

The protocol is a free-form string on the spec (`spec.protocol`) and on every
view. **This package never switches on it.** It is used to group, to render, and
to pick a registered projector or deliverer.

Protocol coordinates — partition, offset, broker timestamp, sequence number,
subscription, receipt handle — live in `EffectView.Coords`, an opaque
`map[string]string`. They are **noise by default and are never asserted**:
asserting an offset would redden every suite on the next re-record. The only
thing this package does with them is read the single key the recording itself
names in `spec.orderBy`, to decide which effects are ordered against each other.

---

## 3. The model

`EffectView` is the protocol-neutral view of one observable message-level
action. **The trigger and every effect are the same type**, which makes a
pipeline hop — stage A's asserted effect is stage B's recorded trigger — a join
over materialised YAML rather than new runtime.

```yaml
kind: Consumer
name: test-7
spec:
  protocol: kafka
  trigger:                       # an EffectView
    protocol: kafka
    op: fetch
    target: orders
    key: o-4c1
    headers: {traceparent: 00-4bf9a1d2e3f4-01}
    body: '{"orderId":"o-4c1"}'
    bodyType: JSON
    decoded: confident           # confident | opaque | presence
    coords: {partition: "0", offset: "1840"}
    records: 1                   # >1 ⇒ a batched unit, labelled honestly
  effects:
    - {protocol: kafka, op: produce, target: order-events, key: o-4c1,
       assert: full, decoded: confident, bodyType: JSON,
       body: '{"orderId":"o-4c1","status":"CONFIRMED"}'}
  sideEffects: 1                 # calls of another family inside the window
  orderBy: partition             # which Coords key names an ordered lane
  completion:
    expectEffects: 1
    graceMs: 250
    timeoutMs: 5000
  assertions:
    noise:
      - effects.0.body.processedAt
```

### What is asserted

Per recorded effect: `protocol`, `op`, `target` and `key` **exactly**; the
**headers** name by name; the **body** field by field when it is JSON and the
projector decoded it confidently, and whole otherwise. Order is asserted
**within** a lane and not across lanes.

`coords` are never asserted. `records` is a fact about the recorded frame, not
an assertion.

A field that legitimately differs every run — a fresh `traceparent`, a
`processedAt` stamp — is silenced by pasting **the path the report printed**
into that test's `spec.assertions.noise`. Per test, explicit, and visible in a
diff. There is deliberately no set-wide switch: widening a silence to every test
in a set is how one flaky field hides a real regression everywhere.

### `sideEffects` is a count, and it is not this file's assertion

A consume-and-write-to-a-database worker produces no protocol effects at all.
`sideEffects` records how many calls of a **different protocol family than the
trigger** fell inside the message's recorded window, which is what tells "the
worker did nothing" apart from "the worker only did things this spec does not
carry".

The count is taken at the **mock choke point** — `SyncMockManager.AddMock`,
which every mock in the process passes, whichever parser emitted it — and not
from the consumer parser's own announcements. It has to be: the write is a
`postgres` (or `http`, or `generic`) mock, and those parsers are not
consumer-aware and never will be. Counted from the consumer parser alone the
number would be structurally zero and every healthy consume-and-write recording
would be refused at mint as having no observable effect.

Its claim — that those calls happened again — is asserted by the **dependency
presence rows** built from the test's own entry in `mappings.yaml`, not here.
When a test has no usable mapping entry, that claim cannot be checked at all,
and a test whose *only* claim is delegated is then **failed by name**
(`CONSUMER_MAPPINGS_REQUIRED` / `CONSUMER_NO_OBSERVABLE_EFFECT`) rather than
passed with nothing having been asserted. The same guard covers the other
spelling of the same shape — a spec whose effects are all **presence
stand-ins**, which the judge filters out of both lanes before pairing and so
asserts exactly as much as an empty list.

"A usable mapping entry" means one that can *carry* the claim, and the bar is
**positive attribution**: at least one mapped mock the recording tagged
`role=effect`. Nothing weaker qualifies.

A mapping degraded to the test's **own trigger** does not count — the trigger
is keploy's own message, and vouching for the worker with it would let the
whole shape pass while asserting nothing about the writes.

Neither does an **untagged mock of a different protocol family**, which was the
first version of this rule and is not evidence at all. `sideEffects` counts
every cross-family mock that landed inside the unit's window, and
`mappings.yaml` is built from that *same* window authority — so a worker that
took the message and silently dropped it still minted `sideEffects: 1` from its
process's ambient `/health` database ping, and that same ambient mock then sat
in the test's mapping next to the trigger and vouched for the claim. The count
was vouching for itself, and the flagship "the worker stopped writing"
regression passed green with zero assertions executed.

The count cannot tell a call the message handler made from an unrelated call
the process made at the same time — a `/health` endpoint's database ping is
enough — and neither can anything derived from it. That is why it is never
allowed to carry a verdict on its own: a test whose only claim is this count is
graded **only** when its mapping holds a mock attributed to the worker, and is
named and failed otherwise.

**What that costs in this version, stated plainly:** no parser in this
repository tags what a worker produced, so a consume-and-write recording taken
with this build is **failed by name at replay**
(`CONSUMER_NO_OBSERVABLE_EFFECT`) rather than passed. The recorder says so at
record time — `Recorder.Close` warns with a `units_without_attributed_effects`
count and names the fix — instead of leaving it to be discovered a CI run
later. See §7.

---

## 4. The delivery gate

`consumer.Gate` is a **state machine, not a view over the mock pool**, with
three phases: `boot`, `armed(testID)`, `draining`.

**It is default-closed.** Narrowing the resident mock pool to one test's mocks
is necessary for injection and is *not* sufficient: the whole pool is resident
while the application boots, and a consumer joins its group and polls
immediately, so a pool-is-the-gate design would let the worker drain every
recorded message in the set before test-1 existed. Delivery is refused in `boot`
and in `draining`, and the pool swap opens nothing.

**Arm adopts; it never clears.** Arming carries a monotonic observation epoch
and adopts everything observed since the previous test completed. A prefetching
client can be answered between the pool swap and the arm, and clearing the
buffer would either lose those effects (a false red by timeout) or count them
twice (a false red by extra).

### `Deliverer` — pull and push

```go
type Deliverer interface {
    Deliver(ctx context.Context, m *models.Mock) error
}
```

`Arm` is an active push, not a pull, and that shape is deliberate:

- A **pull** protocol (Kafka `Fetch`, SQS `ReceiveMessage`) implements `Deliver`
  by stashing the payload and answering the next poll from the stash. Such a
  parser **must** consult `Gate.ArmedTest` before answering that poll and drop
  the stash when the gate is no longer armed for it: the gate cannot un-write
  bytes the parser already holds.
- A **push** protocol (Pulsar) writes the message immediately, unprompted. A
  pulsar-client-go consumer sends **one** flow-control frame at subscribe
  carrying its whole receiver queue; under a pull-shaped SPI test-1 would
  consume that single prompt and tests 2..N would never be handed anything.
  `consumerfake.PushDeliverer` is a compiling sketch of that parser.

A `Deliverer` **must also call `Gate.MarkTriggerAccepted`** once it has positive
evidence the client took the message — not when the bytes were written. For a
test that expects zero protocol effects that is the only evidence the
application ran at all; without it such a test times out with
`trigger_not_delivered`. What counts as evidence is protocol knowledge: for
Kafka it is the client **not** re-fetching the offset it was just served.

### Completion is count plus a grace drain, never an idle timer

```
complete ⟺ observed ≥ expected
           ∧ there is evidence the application ran
           ∧ grace has elapsed since max(arm, last observation)
timeout  ⟹ FAILED, with a named end reason
```

The grace drain is mandatory. Without it an N+1 over-production arriving twenty
milliseconds after the count is satisfied would be attributed to the next test
and this one would pass — and catching that is half of what the gate is for. An
idle timer alone cannot tell "done" from "slow" and cannot see an extra at all.

The backstop yields to a drain that is already running (an effect landing late
must not have its drain cut short and be reported as a timeout), bounded by one
extra grace window so a continuously producing worker cannot hold a window open
for ever.

---

## 5. The projector SPI

```go
type Projector interface {
    Project(m *models.Mock) ([]models.EffectView, error)
}

func RegisterProjector(protocol string, p Projector) func()
```

A parser registers one from its `init()`. `Project` decodes a mock's payload
into the neutral view and stamps its own confidence:

- `confident` — decoded, and safe to field-diff.
- `opaque` — the parser met something it does not model. **An opaque payload can
  never PASS.** A misparse compared against the same misparse agrees for the
  wrong reason, and that is the one false pass no amount of field diffing can
  catch, so it is refused by name on either side.
- `presence` — a stand-in for something that happened but has no decoded
  payload, such as a database write. It is never field-diffed, never counted by
  the completion rule and never rendered as an `effects[i]` row.

Every call is wrapped in `utils.Recover`: a projector panic scores the test
`CONSUMER_PROJECTOR_FAILED` and never kills the agent.

### Role metadata

Three metadata keys on a mock, whose constants live in this repository so
parsers cannot drift the spelling:

| Key | Values | Meaning |
|---|---|---|
| `role` | `trigger`, `effect`, absent | a delivery, something the worker produced, or an ordinary mock |
| `op` | free-form | the protocol operation |
| `target` | free-form | topic / queue / subscription |

**An absent `role` means nothing changes.** That is what makes the whole
contract additive: an untagged mock behaves exactly as it did before this
existed.

---

## 6. The judge, and why it shares no code with the mock matcher

`CompareEffects` imports nothing from the mock matcher, and that is a
correctness property rather than a style choice.

The matcher decides which recorded mock answers a live call. It is deliberately
lenient — it accepts a candidate above a score threshold, and before scoring it
strips fields **by bare name at every nesting depth**: `timestamp`, `host`,
`sequence`, `epoch`, `createTime`. Every one of those properties is right for
choosing a mock and catastrophic for judging a payload: those names are ordinary
business fields inside a message envelope, and a judge that inherited the filter
could not see a diff in any of them. The consequence would not be a missed diff,
it would be a **green test for a broken worker**. A unit test pins the property
with a payload made entirely of those names.

### Failure categories

Every refusal is a `models.FailureCategory` on a **FAILED** test — never a
silent pass, and never a new status enum:

`CONSUMER_NO_OBSERVABLE_EFFECT` · `CONSUMER_OPAQUE_EFFECT_BODY` ·
`CONSUMER_UNSUPPORTED_WIRE_VERSION` · `CONSUMER_TRIGGER_DISCARDED` ·
`CONSUMER_TRIGGER_NOT_DELIVERED` · `CONSUMER_EFFECT_STRADDLES_UNIT` ·
`CONSUMER_MULTI_CONNECTION_RECORDING` · `CONSUMER_EFFECT_CACHE_OVERFLOW` ·
`CONSUMER_UNITS_LOST` · `CONSUMER_MAPPINGS_REQUIRED` ·
`CONSUMER_REPEAT_PASS_UNSUPPORTED` · `CONSUMER_UNSUPPORTED_AGENT` ·
`CONSUMER_UNSUPPORTED_SPEC` · `CONSUMER_PROJECTOR_FAILED` ·
`CONSUMER_COMPLETION_TIMEOUT` · `CONSUMER_RUN_CANCELLED`

and the verdicts `EFFECT_MISSING`, `EFFECT_UNEXPECTED`, `EFFECT_BODY_CHANGED`,
`EFFECT_HEADERS_CHANGED`, `EFFECT_TARGET_CHANGED`.

### A missing effect is never demotable

An expected mock that went unconsumed normally demotes a test to `OBSOLETE`,
which does **not** fail the test set and does **not** change the exit code. That
reading is correct for a mock pool that drifted away from its recording, and
exactly inverted for a consumer: an unconsumed **effect** mock means the worker
did not produce the message the recording says it produces.

So for `Kind: Consumer` the demotion is vetoed — but only by an unconsumed mock
that could actually be an effect (`role=effect`, or a mapped call of a
different family than the trigger, which is the database-write case). An
unconsumed per-test **coordination** mock, which a client may legitimately skip
on a given run, keeps the ordinary demotion; failing a clean test on it would
be a red suite that is a lie.

**This predicate is deliberately wider than the one above it, and the asymmetry
is the safety property.** They answer opposite questions, so they fail in
opposite directions:

| | question | wrong answer is | so it fails |
|---|---|---|---|
| `hasUnconsumedEffectMock` | may a PASSED test be promoted to FAILED? | a missed regression | closed — anything not positively identified as same-family coordination traffic vetoes |
| `mappingCanCarryAnEffectClaim` | may a test with no assertable effect pass on a delegated claim? | a **silent pass** | closed the other way — only `role=effect` counts |

They were briefly one predicate with one shared vocabulary, and it could not be
right for both: the shared version let ambient cross-family traffic vouch for a
worker that produced nothing.

---

## 7. What this version deliberately does not do

- **One test per delivery, not per record.** K records share one wire response
  and one checksum; splitting them needs a batch re-encoder that would destroy
  the verbatim-bytes path the trust rests on. `records: N` is written honestly.
- **Per-record attribution across a unit boundary.** One produced frame can
  carry records for two units. Both units are refused by name
  (`CONSUMER_EFFECT_STRADDLES_UNIT`) rather than one silently dropped.
- **Field-level database assertions.** Writes are asserted at **presence** only.
- **A write the recording does not have is only caught when it is OBSERVED.**
  Presence stand-ins are outside the completion arithmetic on both sides, so
  they get their own assertion: **more** presence effects observed than recorded
  is `EFFECT_UNEXPECTED` (the "the worker now writes the row twice" regression,
  which nothing else in the contract can see — the mock-set comparison only
  fires on expected-not-consumed, never on an extra). **Fewer** is deliberately
  not the judge's finding: v1 has no projector for a database write, so a
  healthy replay legitimately observes zero of them, and that half of the claim
  is carried by the non-demotable unconsumed-effect-mock rule instead.
- **Editing `spec.trigger.body` does not change what the application receives.**
  The mock's recorded bytes supply the payload; the spec describes it.
- **Ordering across lanes is not asserted**, and neither are `coords`.
- **Repeat passes** (`--retryPassing`, `--must-pass`) are refused by name over a
  consumer set: a consumer's fetch position and producer sequence do not rewind
  between attempts inside one process, so the second attempt does not measure
  what the first one did.
- **Attribution of a side-effect call to the message handler.** This is the
  named residual of this version, and everything else in this bullet follows
  from it.

  `sideEffects` is binned on the unit's **window** — the same authority
  `mappings.yaml` uses — because that is the only authority OSS has. A mock
  carries a connection id and nothing that ties one connection's work to
  another's, so a `/health` handler's database ping, a cron `INSERT` or a
  metrics push that lands inside the window is counted, and mapped, exactly as
  a real write would be. Design §0.6 makes this point with a single `/health`
  endpoint being enough to interleave.

  **Consequences, both ends:**

  - *Record.* The mint guard (`len(effects) == 0 && sideEffects == 0`) can
    still be satisfied by ambient traffic, so a unit that produced nothing
    observable is persisted rather than refused. It is not silent:
    `Recorder.Close` warns with `units_without_attributed_effects` and names
    what would make it gradeable.
  - *Replay.* The judge does **not** accept that count, or any mock merely
    co-resident with it, as evidence. A test whose only claim is `sideEffects`
    is **failed by name** with `CONSUMER_NO_OBSERVABLE_EFFECT` unless its
    mapping holds a mock tagged `role=effect`. Rule 7: a named refusal and a
    FAILED verdict, never a silent pass.

  So in this version a consume-and-write recording is red, not green, and it is
  red for a stated reason at both ends.

  **Slice-6 acceptance criterion.** The parser that supplies connection/TGID
  attribution must feed it to `Recorder.onOther` so the count means what its
  name says, and must tag the mocks it attributes `role=effect` so the judge
  can grade them. Until then this shape does not produce a passing test, by
  design.

---

## 8. Divergences from the design document, and what they cost

The design (`keploy-consumer-design-v2.md`) is the argument; this is what
shipped. Four things differ, and they are written here rather than left to be
found by whoever next reads both.

### `spec.writes` became `spec.sideEffects: <int>`

Design §2's test-case model carries `writes:` as a list of
`{protocol, op, target}` presence entries, and §5 scopes the missing-effect
claim to "any mapped `role=effect` mock **or `spec.writes` entry**". The
on-disk format ships a bare count instead
(`pkg/platform/yaml/testdb/testdata/consumer-testcase.yaml`).

Nothing is lost from the **assertion**: the presence claim is carried by the
`deps[i]` rows built from the test's own entry in `mappings.yaml`, and for a
consumer test an unconsumed effect mock is non-demotable. What is lost is
**legibility** — the spec file no longer names which writes the recording saw,
so a human reading one test case cannot tell what is expected without opening
`mappings.yaml` beside it. This is a hand-editable on-disk format that a parser
in another repository will write, so the divergence is stated rather than
quietly carried: **the design doc should be amended to match, not the file.**

### gRPC-only test sets now anchor TLS certificates on the recording

`TestCase.EarliestTimestamp` reads the record window for every Kind, so a
gRPC-only set's certificate backdate moved from `HTTPReq.Timestamp` — the zero
time, for which `tls.CertForClient` substituted wall-clock now — to
`GrpcReq.Timestamp`, the recording instant.

`CertForClient` sets `NotBefore = backdate - 1y` and `NotAfter = now + 1y`
unconditionally, so an earlier backdate only **widens** the validity window and
can never expire a certificate. It is nonetheless a user-visible change to an
existing Kind and belongs in the release notes as one. `TestBackdateFor` carries
a gRPC-only row and a mixed HTTP+gRPC row that pin it.

### An invalid simulation response now ends its test's iteration

Pre-existing, on the HTTP and gRPC paths, and changed here because the consumer
arm sits alongside them: the "invalid response type for X test case" arm used
`break`, which breaks the **switch**, not the loop. The test then fell through
the rest of the loop body with `testResult` still nil and was counted into the
failure total **twice** — once in that arm and again in the status switch below.
All three arms now `continue`.

The verdict for such a test is unchanged (it was FAILED, it is FAILED) and it
still reddens the set. What changes for an existing HTTP/gRPC run is the
**failure count** printed for a set containing one, and that the rest of the
loop body no longer executes for it. Pinned by
`TestGivingUpOnATestCaseContinuesTheLoopRatherThanTheSwitch`. It belongs in the
release notes beside the gRPC backdate change.

### The Pulsar `Deliver` sketch is a compiling fake, not a parser

Design §4 P1 requires a push-shaped `Deliverer` to exist before this SPI tags,
because the pull/push split is the one genuine one-way door in the interface.
`consumerfake.PushDeliverer` is that sketch: it spends flow-control credits the
way a `pulsar-client-go` consumer's `receiverQueueSize` does, and its doc
comment states what a real Pulsar parser does with `GrantFlow`, `Deliver` and
`MarkTriggerAccepted`. It is a fake and says so. It is not a Pulsar parser and
this repository does not ship one.

---

## 9. The OSS-Kafka question — DECISION REQUIRED BEFORE THIS TAGS

Design §6 names this as "the one thing here that is not reversible later
without a public retraction", and it is unresolved in code. It is recorded here
so it cannot tag unnoticed.

**The contradiction.** This repository ships the contract, the gate, the
recorder and the judge, and **no consumer protocol parser**. So an OSS-only user
can read every rule a green consumer suite obeys and cannot produce a single
`Kind: Consumer` test case. `keploy-async-roadmap-winner.md` says "correctness is
a floor, never a paywall" and, specifically, "at least the Kafka self-replay
path must be donated to OSS". Those two facts cannot both stand.

**Why the split that shipped is defensible on its own terms.** The judge and the
contract are here *because a verdict nobody can inspect is not a verdict* — a
user must be able to read what green claims before trusting it. Nothing about
that argument requires the parser to be here too, and the registration seams
(`RegisterProjector`, `RegisterDeliverer`, protocol as an opaque string) are
built so a parser can be added from any module **without a second release of
this contract**.

**Why that is not sufficient.** "You may inspect the rules but may not run them"
is not the floor the strategy document promises. And the asymmetry is
self-reinforcing: with no OSS parser there is no OSS user of this SPI, so the
first real consumer of it is the enterprise Kafka parser, and the interface will
drift toward whatever that one caller needs.

**The two answers, and neither is free:**

1. **Donate a minimal OSS Kafka consumer path** — the wire fidelity work, the
   KIP-482 flexibility predicate, role stamping, a Produce/Fetch projector and a
   pull `Deliverer`. Honours the strategy document. Costs the parser work in OSS
   and gives up the Kafka path as a differentiator.
2. **Amend the strategy document** — strike "at least the Kafka self-replay path
   must be donated to OSS" and state plainly that OSS ships the contract and the
   verdict, and paid ships the protocols.

**Status: not decided.** Whichever is chosen must be reflected in the strategy
document *and* in §1 of this file **before `v3.6.23` is tagged**, because after
the tag option 2 is a public retraction rather than an edit.
