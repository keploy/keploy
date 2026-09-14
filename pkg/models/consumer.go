package models

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Consumer testing contract — the protocol-neutral model.
//
// This file is the ONLY place a consumer test case, its arm and its verdict
// are described, and it is deliberately empty of protocol knowledge. There is
// nothing Kafka-shaped here, nothing Pulsar-shaped and nothing SQS-shaped:
// every protocol coordinate lives in EffectView.Coords, an opaque
// map[string]string that OSS records, persists, renders and NEVER interprets.
//
// WHY ONE GENERIC KIND. A per-protocol test-case Kind costs an arm in four
// `default:` rejects in pkg/platform/yaml/testdb/util.go, the Kind switches in
// pkg/service/replay (request/response timestamps, the judge, the result
// struct), hooks.go's SimulateRequest, testdb/naming.go's BuildTestCaseSlug,
// replay/utils.go's recordReqResTimestamps and the k8s-proxy gates — ~15 arms
// per protocol, forever, across three repositories, versus 15 once. See
// keploy-consumer-design-v2.md §0.7.
//
// SCOPE OF THIS FILE. Types and constants only. The delivery gate, the
// projector registry and the unit recorder live under
// pkg/agent/proxy/integrations/consumer/; the judge lives in
// pkg/service/replay. Nothing in OSS can produce a CONSUMER test case today:
// OSS ships no Kafka/Pulsar/SQS parser, so this contract is inert until an
// enterprise parser drives it (design §6, §7 slice 5).

// CONSUMER is the test-case Kind for a message-consumer unit: one recorded
// poll/push delivery (the trigger) plus everything the worker produced while
// handling it (the effects).
//
// WHY IT IS DECLARED HERE AND NOT IN mock.go's Kind BLOCK. Every constant in
// that block is a MOCK kind — a wire protocol some parser records and replays
// — and models.DepTypeForKind maps exactly that set onto the DepResult `type`
// protocol family, pinned by TestDepTypeForKind_CoversEveryKind, which parses
// mock.go. CONSUMER is a TEST-CASE kind only: no parser ever stamps it on a
// Mock, and a consumer test's own mocks carry the underlying protocol's kind
// (Kafka, …), not this one. Putting it in the mock block would assert it is a
// dependency protocol family, which is false. TestConsumerKindIsNotAMockKind
// pins the placement so a later move forces the DepTypeForKind table to be
// updated deliberately rather than by accident.
const CONSUMER Kind = "Consumer"

// DecodeConfidence records whether a payload was decoded well enough to be
// asserted on. It is the refuse-don't-guess seam: a projector that cannot
// model a wire version stamps DecodedOpaque, and an opaque effect can never
// produce a PASS (design §5, false-pass row 5) — comparing a misparse against
// the same misparse is a silent green, which is worse than a refusal.
type DecodeConfidence string

const (
	// DecodedConfident: the projector fully modelled this payload and its
	// Body is a faithful decode. Only a confident effect is field-diffed.
	DecodedConfident DecodeConfidence = "confident"

	// DecodedOpaque: the projector could not model the payload (an
	// un-modelled flexible version, an unknown compression, a body it
	// declined to guess at). The verdict must be FAILED with
	// CategoryConsumerOpaqueEffectBody, never PASSED.
	DecodedOpaque DecodeConfidence = "opaque"

	// DecodedPresence: NOTHING WAS DECODED, AND NOTHING WAS MEANT TO BE. The
	// view is a stand-in recording that something the worker did was
	// observed — the canonical case is a mapped database write consumed
	// inside the test's window, which has no projector in v1 and whose claim
	// is carried by the sync path's `deps[i]` presence row instead.
	//
	// WHY IT IS NOT THE SAME AS OPAQUE. Opaque means "a decoder tried, could
	// not model this, and refuses to guess" — a FAILURE, because comparing a
	// misparse against the same misparse is a silent pass. Presence means "no
	// decoder was ever asked". Folding the two together would either turn
	// every consume-and-write-to-a-database worker permanently red (one of
	// the two most common consumer shapes), or, in the other direction, make
	// a real un-modelled wire version look like a routine database write.
	//
	// A PRESENCE VIEW IS NOT COUNTED BY THE COMPLETION RULE, and this is the
	// one rule about it that is easy to get wrong. ConsumerCompletion's
	// ExpectEffects is written by the recorder from PRODUCED RECORDS ONLY —
	// it deliberately excludes mapped writes, because nothing in the replay
	// path calls ObserveEffect for a database write and counting them would
	// make every consume-and-write worker wait out its whole timeout. If the
	// gate then counted presence views on the observed side the two ends
	// would disagree by construction: a healthy test with one produce and one
	// write would report 2 observed against 1 expected and go red, and worse,
	// a presence view arriving early would satisfy the count and let the
	// window close before the produce landed. So ALL THREE ENDS SKIP THEM,
	// and they have to agree or the contract contradicts itself: the
	// recorder skips them when it writes ExpectEffects
	// (Recorder.onEffect), the gate skips them in its record count
	// (Gate.pendingLocked), and the judge skips them in its pairing on BOTH
	// the expected and the observed side (compareEffectLists). They are
	// still recorded, rendered and used to extend the grace anchor, because
	// the worker was demonstrably still working.
	//
	// The judge NEVER field-diffs a presence view, never reports it as an
	// unexpected effect, and never renders it as an `effects[i]` row. That is
	// the same visual separation design §2 asked for when it put database
	// writes in their own `spec.writes` list, expressed on the view rather
	// than in a second list that could disagree with the mapping.
	DecodedPresence DecodeConfidence = "presence"
)

// EffectAssert selects how strongly an effect row is asserted.
//
// v1 has exactly ONE mode. `assert: presence` was deliberately deleted from
// the design (§2): a green tick that asserts nothing, rendered next to one
// that does, is exactly the false confidence this work exists to remove.
// Presence-only claims are carried by the slice-4 sync-path `deps[i]` rows
// instead, which are visually and structurally distinct. The field is kept —
// and validated — because `assert` is part of a published on-disk schema and
// a future mode (headers-only, key-only) must be addable without a
// schema-version bump.
type EffectAssert string

// AssertFull is the only assert mode v1 accepts: protocol/op/target/key exact,
// body field-diffed when BodyType is JSON. The empty string means AssertFull
// so a hand-written or older spec keeps working.
const AssertFull EffectAssert = "full"

// EffectView is the protocol-neutral view of one observable message-level
// action: a delivery to the worker, or something the worker produced.
//
// The TRIGGER and every EFFECT are the same type on purpose. It makes a
// pipeline hop — stage A's asserted effect is stage B's recorded trigger — a
// join over materialised YAML rather than new runtime, and it means the
// record path, the replay path and the report all speak one vocabulary.
//
// Coords is the ONLY place protocol coordinates appear (partition, offset,
// broker timestamp, sequence number, subscription, receipt handle, …). OSS
// never reads a key out of it: it is copied, persisted, rendered and compared
// as an opaque map. Coords are NOISE BY DEFAULT and are not asserted —
// asserting an offset would redden every suite on the next re-record (§5).
type EffectView struct {
	// Protocol is the wire family that produced this view ("kafka",
	// "pulsar", "sqs", …). Free-form: OSS never switches on it, it only
	// groups and renders by it.
	Protocol string `json:"protocol" yaml:"protocol"`

	// Op is the protocol operation ("fetch", "produce", "message",
	// "send", …). Compared for exact equality by the judge.
	Op string `json:"op" yaml:"op"`

	// Target is the destination the operation addressed: a topic, a queue,
	// a subscription. Compared for exact equality by the judge.
	Target string `json:"target" yaml:"target"`

	// Key is the message key / group key when the protocol has one.
	// Compared for exact equality by the judge.
	Key string `json:"key,omitempty" yaml:"key,omitempty"`

	// Headers are the message headers, if the protocol carries any.
	//
	// ASSERTED, name by name, on every effect. They are routing, tenancy and
	// tracing metadata — dropping a `tenant` header delivers a byte-identical
	// payload to the wrong customer — so leaving a field that is projected,
	// persisted and rendered out of the comparison would be exactly the
	// silent pass this contract exists to remove. A disagreement is reported
	// under effects.<i>.headers.<name> and carries
	// CategoryEffectHeadersChanged, separate from a body diff because the
	// remedy is. A header that legitimately differs every run (a fresh
	// traceparent) is silenced by pasting its reported path into that test's
	// spec.assertions.noise — per test, explicit, and visible in a diff.
	//
	// The TRIGGER's headers are descriptive only: nothing compares them,
	// because the trigger is what keploy delivers rather than something the
	// worker produced.
	Headers map[string]string `json:"headers,omitempty" yaml:"headers,omitempty"`

	// Body is the decoded payload. Field-diffed when BodyType is JSON and
	// Decoded is DecodedConfident; otherwise compared whole.
	Body string `json:"body,omitempty" yaml:"body,omitempty"`

	// BodyType tells the judge how to compare Body. JSON gets per-field
	// diffs; everything else is compared as an opaque string.
	BodyType BodyType `json:"bodyType,omitempty" yaml:"bodyType,omitempty"`

	// Decoded is the projector's own confidence in Body. See
	// DecodeConfidence: opaque can never PASS.
	Decoded DecodeConfidence `json:"decoded,omitempty" yaml:"decoded,omitempty"`

	// Coords are opaque protocol coordinates. NOISE BY DEFAULT: never
	// asserted, only rendered and used to GROUP ordered comparisons (the
	// judge orders within a (protocol, target, coords[<partition-ish>])
	// group and is unordered across groups). OSS never interprets a key.
	Coords map[string]string `json:"coords,omitempty" yaml:"coords,omitempty"`

	// Assert selects the assertion strength. Empty means AssertFull.
	Assert EffectAssert `json:"assert,omitempty" yaml:"assert,omitempty"`

	// Records is how many protocol records this one view covers. >1 means
	// the recording batched, and the test is LABELLED as a batch test
	// rather than refused — an honest fact about the recorded frame, not a
	// hedge about the verdict. The seed convention keeps it 1 in dev.
	Records int `json:"records,omitempty" yaml:"records,omitempty"`
}

// AssertMode returns the effective assert mode, treating the empty string as
// AssertFull so older and hand-written specs keep working.
func (e EffectView) AssertMode() EffectAssert {
	if e.Assert == "" {
		return AssertFull
	}
	return e.Assert
}

// IsPresenceOnly reports whether this view is a presence stand-in rather than
// a decoded effect: nothing was decoded and nothing was meant to be. Such a
// view is NOT counted by the completion rule (see DecodedPresence for why
// counting it would redden a healthy test), and is never field-diffed, never
// reported as unexpected and never rendered as an effects[i] row.
func (e EffectView) IsPresenceOnly() bool {
	return e.Decoded == DecodedPresence
}

// IsOpaque reports whether this view's payload was NOT confidently decoded.
// An opaque expected-or-observed effect must fail the test rather than be
// compared against another opaque view (design §5, false-pass row 5).
func (e EffectView) IsOpaque() bool {
	return e.Decoded == DecodedOpaque
}

// RecordCount returns Records, treating the zero value as one record. A
// projector that does not count records still describes one observable
// action, and completion arithmetic must never see zero for a real effect.
func (e EffectView) RecordCount() int {
	if e.Records <= 0 {
		return 1
	}
	return e.Records
}

// ConsumerCompletion is the rule that decides when a consumer test is DONE.
//
// complete: observedEffects >= ExpectEffects AND there is positive evidence
// the application ran AND GraceMs has elapsed since the last observed effect.
// timeout: FAILED, EndReason ConsumerEndReasonTimeout.
//
// The grace drain is MANDATORY and never skipped. Without it an N+1
// over-production arriving 20ms after the count is satisfied is attributed to
// the NEXT test and this one passes — the over-produce regression would be
// uncatchable. Completion is never an idle timer alone: an idle timer cannot
// tell "done" from "slow" and cannot catch an extra at all.
//
// THE POSITIVE-EVIDENCE TERM EXISTS BECAUSE ExpectEffects CAN BE ZERO. See the
// field below: a consume-and-write-to-a-database worker records no protocol
// effect at all, so the count alone is satisfied before the application has
// done anything. Evidence is either an observed effect (which the worker
// cannot produce without the message) or ConsumerResult.TriggerAccepted, the
// parser's positive delivery check. With neither, the window falls through to
// the timeout backstop and reports ConsumerEndReasonTriggerNotDelivered.
type ConsumerCompletion struct {
	// ExpectEffects counts produced RECORDS (not produce REQUESTS: batching
	// makes requests-per-record nondeterministic between runs, so a
	// request-count rule flakes by construction). Presence stand-ins are
	// NOT counted — see DecodedPresence.
	//
	// IT CAN LEGITIMATELY BE ZERO, and the design sentence "a persisted test
	// always has ExpectEffects >= 1, so 0 of 0 can never read as green" is
	// not implementable: nothing on the replay path calls ObserveEffect for
	// a database write, so a consume-and-write worker — one of the two most
	// common consumer shapes — would wait out its whole timeout if its
	// writes were counted here. Mint lets such a unit through on
	// ConsumerSpec.SideEffects instead. What stops "0 of 0" reading green is
	// the positive-evidence term of the completion rule above, plus the
	// judge's own copy of the same check.
	ExpectEffects int `json:"expectEffects" yaml:"expectEffects"`

	// GraceMs is the drain window after the last observed effect, derived
	// at record time from p99(trigger -> last effect) x 3, clamped to
	// [ConsumerGraceMinMs, ConsumerGraceMaxMs].
	GraceMs int64 `json:"graceMs" yaml:"graceMs"`

	// TimeoutMs is the backstop. Hitting it is a FAILED test with a named
	// EndReason, never a silent pass and never an infinite wait.
	TimeoutMs int64 `json:"timeoutMs" yaml:"timeoutMs"`
}

// Bounds for the record-time grace derivation and the timeout backstop.
const (
	ConsumerGraceMinMs       int64 = 200
	ConsumerGraceMaxMs       int64 = 2000
	ConsumerDefaultTimeoutMs int64 = 5000
)

// Grace returns the completion grace window, falling back to
// ConsumerGraceMinMs when the spec carries none (an older or hand-written
// file). Never returns zero: a zero grace silently disables over-production
// detection, which is the one thing the drain exists for.
func (c ConsumerCompletion) Grace() time.Duration {
	ms := c.GraceMs
	if ms <= 0 {
		ms = ConsumerGraceMinMs
	}
	if ms > ConsumerGraceMaxMs {
		ms = ConsumerGraceMaxMs
	}
	return time.Duration(ms) * time.Millisecond
}

// Timeout returns the completion backstop, falling back to
// ConsumerDefaultTimeoutMs. Never returns zero: a zero timeout would let a
// worker that never produces block the run forever instead of failing it.
func (c ConsumerCompletion) Timeout() time.Duration {
	ms := c.TimeoutMs
	if ms <= 0 {
		ms = ConsumerDefaultTimeoutMs
	}
	return time.Duration(ms) * time.Millisecond
}

// ConsumerSpec is the persisted `spec:` of a CONSUMER test case.
//
// DELIBERATELY ABSENT: `spec.writes`. Design §2 listed a presence-only writes
// list; it is not built here and its claim is not lost. A mapped write-mock
// that goes unconsumed during the test's window is ALREADY reported, by the
// slice-4 sync-path dependency writer, as a `deps[i] <type> <target>
// (presence)` row — and once the CONSUMER verdict is non-demotable that row
// fails the test. Materialising the same fact a second time in the spec would
// need cross-goroutine buffering the record path does not have (the test case
// is inserted concurrently with, not after, its mapping resolving), and would
// give two independent producers of one claim that could disagree. See the
// implementation notes on this slice.
//
// DELIBERATELY ABSENT: a `mock:` reference on trigger or effects. It cannot
// be written: a mock's name is rewritten twice after the parser assigns it
// (once when the record window resolves, once when it is persisted), so any
// name captured in the spec would be stale by construction — and a
// hand-editable field that silently contradicts the mock pool is worse than
// no field. Identity is role/op/target metadata plus the recorder's in-memory
// unit id.
type ConsumerSpec struct {
	// Metadata mirrors HTTPSchema.Metadata / GrpcSpec.Metadata: free-form
	// document annotations (currently only "description").
	Metadata map[string]string `json:"metadata,omitempty" yaml:"metadata,omitempty"`

	// Protocol is the consumer's wire family. Denormalised from
	// Trigger.Protocol so a reader can route a whole test without decoding
	// the trigger.
	Protocol string `json:"protocol" yaml:"protocol"`

	// Trigger is the recorded delivery that starts the unit. On replay the
	// recorded MOCK supplies the bytes actually handed to the client;
	// editing Trigger.Body here changes what is REPORTED, not what the app
	// receives (design §8). The gate refuses to run a test whose trigger
	// cannot be delivered rather than reporting "0 effects" as an app bug.
	Trigger EffectView `json:"trigger" yaml:"trigger"`

	// Effects are what the worker produced while handling the trigger,
	// ordered WITHIN a (protocol, target, partition-ish coord) group and
	// unordered ACROSS groups. Partition order is the observable semantics
	// of a log, so a swap there is a real regression; two goroutines
	// producing to different partitions interleave nondeterministically and
	// that is not.
	Effects []EffectView `json:"effects,omitempty" yaml:"effects,omitempty"`

	// OrderBy names the ONE key in EffectView.Coords whose value splits a
	// (protocol, target) pair into independently-ordered lanes. It is how the
	// judge can honour "ordered within a lane, unordered across lanes"
	// WITHOUT OSS EVER NAMING A PROTOCOL COORDINATE.
	//
	// A Kafka recording writes `orderBy: partition`, and the judge then reads
	// Coords["partition"] as an opaque discriminator: it compares those values
	// for string equality and attaches no meaning to them whatever. Pulsar and
	// SQS leave it empty. The alternative — hard-coding "partition" in the
	// comparator — would put a Kafka concept in the one package this design
	// says must contain nothing Kafka-shaped, and would silently do nothing
	// for every protocol that spells its lane differently.
	//
	// EMPTY MEANS ONE LANE PER (protocol, target): every effect to a target is
	// ordered against every other effect to that target. That is the stricter
	// reading, and it is the correct default — a recording that does not say
	// its effects are independently ordered has not earned the weaker
	// assertion. A protocol whose producer genuinely interleaves must say so
	// by naming a lane coordinate.
	OrderBy string `json:"orderBy,omitempty" yaml:"orderBy,omitempty"`

	// SideEffects is how many calls of a DIFFERENT protocol family than the
	// trigger fell inside this message's recorded window — the
	// consume-and-write-to-a-database shape: an INSERT, an outgoing HTTP
	// call, a cache set.
	//
	// IT IS COUNTED AT THE MOCK CHOKE POINT, NOT BY THE CONSUMER PARSER. The
	// calls it counts belong to OTHER parsers — postgres, http, generic —
	// which are not consumer-aware and never will be, so nothing calls the
	// consumer recorder for them on any code path. The ingest is registered on
	// SyncMockManager.AddMock, which every mock in the process passes
	// (consumer.Recorder.OnEgress). Counted from the consumer parser's own
	// announcements this number would be structurally zero and Recorder.
	// closeUnit would refuse every healthy consume-and-write recording.
	//
	// "IN THE WINDOW", NOT "BY THE HANDLER". The recorder bins on the unit's
	// own window because that is the same authority the test-mock mapping
	// uses, so this number agrees with what mappings.yaml will carry. It
	// cannot tell a call the message handler made from an unrelated call the
	// process made at the same time — a /health endpoint's database ping is
	// enough — because nothing in a mock ties one connection's work to
	// another's.
	//
	// THAT LIMIT IS WHY THIS NUMBER CANNOT CARRY A VERDICT ON ITS OWN, and
	// why the judge will not accept a mapping that merely LOOKS like it might
	// carry one either. A test whose ONLY claim is this count is refused by
	// name at replay unless the sync path's presence assertion actually ran
	// for it AND its mapping holds a mock the recording ATTRIBUTED to the
	// worker (role=effect). Neither the test's own trigger nor an untagged
	// cross-family mock counts: mappings.yaml is built from the same window
	// this count is binned on, so the ambient /health ping is mapped next to
	// the trigger, and accepting it would be this number vouching for itself
	// (replay.consumerDepAssertion, replay.mappingCanCarryAnEffectClaim).
	//
	// The practical consequence, stated where the field is defined: until a
	// parser tags what the worker produced, a recording whose only claim is
	// this count is FAILED with CONSUMER_NO_OBSERVABLE_EFFECT at replay
	// rather than passed. Rule 7 — a named refusal, never a silent pass.
	//
	// IT IS A COUNT, NOT A LIST, AND IT IS NOT AN ASSERTION. What each of
	// those calls was, and whether it happened again on replay, is asserted
	// by the slice-4 sync-path dependency rows (`deps[i] … (presence)`),
	// which are built from the test's own mock mapping and are the single
	// persisted statement of that claim. Materialising the same fact a second
	// time here as a list would give two independent producers of one claim
	// that can disagree, and would need cross-goroutine buffering the record
	// path does not have (a test case is handed to persistence concurrently
	// with, not after, its mapping resolving). Design §2 called that list
	// `spec.writes`; this is the honest subset of it that the record path can
	// actually write.
	//
	// WHAT IT IS FOR: telling "the worker did nothing observable" apart from
	// "the worker only did things this spec does not carry". Without it the
	// two are both `effects: []` + `expectEffects: 0`, and the judge's
	// vacuity guard — which refuses a test that can only ever pass — would
	// refuse every consume-to-a-database test the recorder deliberately mints
	// (see Recorder.closeUnit, which lets such a unit through precisely
	// because it made side-effect calls). That contradiction is a guaranteed
	// red on one of the two most common consumer shapes.
	SideEffects int `json:"sideEffects,omitempty" yaml:"sideEffects,omitempty"`

	// Completion is the count+grace rule for this test.
	Completion ConsumerCompletion `json:"completion" yaml:"completion"`

	// Created mirrors HTTPSchema.Created: unix seconds, coarse.
	Created int64 `json:"created,omitempty" yaml:"created,omitempty"`

	// ReqTimestampMock / ResTimestampMock are the recorded unit's WINDOW —
	// the trigger's request and response times. Same field names as
	// HTTPSchema/GrpcSpec so the vocabulary does not fork.
	//
	// The window is cut at the trigger's RESPONSE time, not its request:
	// every mainstream client issues the next poll from INSIDE the current
	// one, before the app has processed the record it is holding, so
	// cutting at the request would put the next trigger inside this
	// window. These two fields are what TestCase.RecordWindow() returns for
	// a CONSUMER test, and they are why a consumer test does not fall back
	// to Created's one-second granularity (which would corrupt mapping
	// regeneration against 300ms seed spacing).
	ReqTimestampMock time.Time `json:"reqTimestampMock,omitempty" yaml:"reqTimestampMock,omitempty"`
	ResTimestampMock time.Time `json:"resTimestampMock,omitempty" yaml:"resTimestampMock,omitempty"`

	// AppPort mirrors HTTPSchema.AppPort.
	AppPort uint16 `json:"app_port,omitempty" yaml:"app_port,omitempty"`

	// Assertions carries per-test noise under NoiseAssertion, exactly as
	// HTTPSchema and GrpcSpec do, so `keploy` reads consumer noise through
	// the same path it already reads HTTP noise. Paths are the report's own
	// diff vocabulary (effects.<i>.body.<dotted.path>), so a reported path
	// pastes straight back in.
	Assertions map[AssertionType]interface{} `json:"assertions,omitempty" yaml:"assertions,omitempty"`
}

// ConsumerEndReason names why a consumer test stopped waiting. It is
// persisted on the report and is what an agent keys remedies on, so the set is
// a one-way door.
type ConsumerEndReason string

const (
	// ConsumerEndReasonCountReached: the expected effect count was reached
	// and the grace window drained with nothing more arriving. The only
	// end reason a PASS can have.
	ConsumerEndReasonCountReached ConsumerEndReason = "count_reached"

	// ConsumerEndReasonTimeout: the backstop fired before the count was
	// reached. Always a FAILED test.
	ConsumerEndReasonTimeout ConsumerEndReason = "timeout"

	// ConsumerEndReasonTriggerNotDelivered: the app never took the trigger
	// (it never polled, never subscribed, crashed at boot, joined the wrong
	// group). Distinguishes "the app is broken" from "keploy never
	// delivered", which a bare "0 effects" cannot.
	ConsumerEndReasonTriggerNotDelivered ConsumerEndReason = "trigger_not_delivered"

	// ConsumerEndReasonTriggerDiscarded: keploy wrote the trigger and the
	// CLIENT threw it away — a session id it did not recognise, a batch
	// below its fetch position. Bytes on the wire are not delivery, and
	// without this the case reads as "the worker stopped producing".
	ConsumerEndReasonTriggerDiscarded ConsumerEndReason = "trigger_discarded"

	// ConsumerEndReasonInternalError: the gate itself failed (a projector
	// panicked, the deliverer returned an error). Reported rather than
	// folded into "timeout", because the two need completely different
	// remedies and a wrong remedy costs an agent a whole loop. Not one of
	// the four end reasons design §2 tabulated — added so a KNOWN cause is
	// never reported as an unknown one.
	ConsumerEndReasonInternalError ConsumerEndReason = "internal_error"
)

// ConsumerArm is the request that opens one test's delivery window.
//
// Arm is the ACTIVE event, not a pull. A pull-shaped "take the trigger if you
// have one" cannot express "push this now", which push protocols require: a
// Pulsar client sends one FLOW carrying its whole receiver queue at subscribe
// time, so under a pull model test-1 would consume that FLOW and tests 2..N
// would never receive anything. Arm + Deliverer costs the same code and closes
// that door before the interface is published.
//
// Arming is NECESSARY BUT NOT SUFFICIENT for injection. The whole mock pool is
// resident while the app boots, and a consumer polls immediately, so a
// pool-narrowing-is-arming design would let the app drain every trigger before
// test-1 exists. The gate is default-closed: it refuses to deliver outside an
// armed window and answers every poll in boot/draining with a synthesized
// empty response.
type ConsumerArm struct {
	// TestID is the test case this window belongs to. Every effect
	// observed inside the window is attributed to it.
	TestID string `json:"test_id" yaml:"test_id"`

	// TestSetID scopes TestID; the gate is reset per test set so
	// --keep-app-alive cannot leak an armed trigger across sets.
	TestSetID string `json:"test_set_id" yaml:"test_set_id"`

	// Protocol selects which registered Deliverer is asked to deliver.
	Protocol string `json:"protocol" yaml:"protocol"`

	// Trigger is the RECORDED view of the delivery. It identifies and
	// describes the trigger; it is not the bytes. The mock pool supplies
	// the bytes.
	Trigger EffectView `json:"trigger" yaml:"trigger"`

	// Completion is this test's count+grace rule.
	Completion ConsumerCompletion `json:"completion" yaml:"completion"`
}

// ConsumerRecordingReport is the record-side reconciliation design §3 R6
// requires a recording to end with: how many consumer units were observed, how
// many became test cases, how many were refused by name, and what — if
// anything — makes the recording untrustworthy.
//
// IT EXISTS SO A DEGRADED RECORDING CAN FAIL, NOT ONLY BE LOGGED. A user who
// seeded ten messages and got eight files has no way to tell from the file
// count alone: a unit refused by name and a unit that vanished produce the
// same number of files, and an agent loop keying on the exit code would go
// straight on to replay a suite that is silently short. Problems is what turns
// that into a non-zero exit; the counters are what a human reads to see how
// bad it is.
type ConsumerRecordingReport struct {
	UnitsObserved  int `json:"units_observed" yaml:"units_observed"`
	UnitsPersisted int `json:"units_persisted" yaml:"units_persisted"`
	UnitsRefused   int `json:"units_refused" yaml:"units_refused"`
	// UnitsLost is the units that were neither persisted nor refused by
	// name. Anything here vanished silently, which is the one outcome
	// design §3 R6 forbids.
	UnitsLost int `json:"units_lost" yaml:"units_lost"`
	// OrphanEffects counts effect records the worker produced while no unit
	// was open. They belong to no test, so replay has nothing to compare
	// them against.
	OrphanEffects int `json:"orphan_effect_records" yaml:"orphan_effect_records"`
	// Problems names every reason this recording must not be replayed
	// as-is, already carrying its FailureCategory. Empty means the
	// recording reconciled and the run may exit 0.
	Problems []string `json:"problems,omitempty" yaml:"problems,omitempty"`
}

// Degraded reports whether the recording must not be replayed as-is.
func (r ConsumerRecordingReport) Degraded() bool { return len(r.Problems) > 0 }

// Observed reports whether any consumer unit was seen at all. Every HTTP-only
// recording — which today is every recording OSS can make, since no OSS parser
// stamps role metadata — returns false, and callers stay silent on it.
func (r ConsumerRecordingReport) Observed() bool {
	return r.UnitsObserved > 0 || r.UnitsRefused > 0 || r.OrphanEffects > 0
}

// ConsumerResetResult is what the gate reports back when it is returned to its
// default-closed boot phase at a test-set boundary.
//
// IT IS A RESULT AND NOT AN ERROR, and that distinction is the whole reason
// the type exists. A non-zero TrailingEffects is an APPLICATION regression —
// the worker produced after the last test of the set closed its window, which
// is the one place an N+1 emission at the very end of a run can be seen at
// all. A failed reset CALL is a keploy problem: an unreachable agent, an agent
// that predates the route. Carrying both as "the reset returned an error" made
// every infrastructure failure read to the user as "your worker emits more
// messages than the recording says" and sent them to debug the wrong system.
type ConsumerResetResult struct {
	// TrailingEffects is how many effect RECORDS were observed after the
	// last completed test's window and therefore belong to no test.
	TrailingEffects int `json:"trailing_effects" yaml:"trailing_effects"`
}

// ConsumerResult is what the gate reports back when a window closes. It is the
// judge's input on the replay side and the source of TestResult.Consumer.
type ConsumerResult struct {
	// TestID echoes ConsumerArm.TestID. A result whose TestID does not
	// match the arm is a bug, not a verdict: the caller must refuse it
	// rather than judge one test's effects against another's spec.
	TestID string `json:"test_id" yaml:"test_id"`

	// TriggerAccepted is a POSITIVE delivery check — the client took the
	// trigger and did not ask for the same position again. It is
	// deliberately not called "delivered": that only ever meant "bytes were
	// written", which is exactly the false signal design §5 row 1 describes.
	TriggerAccepted bool `json:"trigger_accepted" yaml:"trigger_accepted"`

	// ExpectEffects / ObservedEffects are record counts, not request
	// counts.
	ExpectEffects   int `json:"expected_effects" yaml:"expected_effects"`
	ObservedEffects int `json:"observed_effects" yaml:"observed_effects"`

	// EndReason names why the window closed.
	EndReason ConsumerEndReason `json:"end_reason,omitempty" yaml:"end_reason,omitempty"`

	// Effects are the LIVE views observed inside the window, in observation
	// order. The gate records them; it never judges them.
	Effects []EffectView `json:"effects,omitempty" yaml:"effects,omitempty"`

	// Refusal, when set, is the named reason this run cannot be judged
	// normally. It always produces TestStatusFailed plus this category —
	// never a silent pass, and never a status enum that does not exist.
	Refusal FailureCategory `json:"refusal,omitempty" yaml:"refusal,omitempty"`

	// RefusalDetail is the human-readable specifics behind Refusal.
	RefusalDetail string `json:"refusal_detail,omitempty" yaml:"refusal_detail,omitempty"`
}

// Refused reports whether the gate declined to produce a judgeable run.
func (r *ConsumerResult) Refused() bool {
	return r != nil && r.Refusal != ""
}

// Info projects a gate result onto the persisted, report-facing shape.
// Returns nil for a nil result so a non-consumer test keeps serializing
// exactly as it did before this field existed.
func (r *ConsumerResult) Info() *ConsumerResultInfo {
	if r == nil {
		return nil
	}
	return &ConsumerResultInfo{
		TriggerAccepted: r.TriggerAccepted,
		ExpectedEffects: r.ExpectEffects,
		ObservedEffects: r.ObservedEffects,
		EndReason:       r.EndReason,
	}
}

// ConsumerResultInfo is the persisted projection of a consumer run, carried on
// TestResult.Consumer.
//
// It is a POINTER on TestResult with omitempty so that every HTTP and gRPC
// report — every report already on disk, and every report a non-consumer run
// writes — serializes byte-identically to a pre-consumer build. That is pinned
// by the report golden tests.
//
// It carries the four scalars a reader needs to tell the shapes of failure
// apart without parsing prose: did the app take the trigger, how many effects
// were expected, how many arrived, and why we stopped waiting. The per-field
// diffs live in Result.DepResult, as they do for the sync path.
type ConsumerResultInfo struct {
	TriggerAccepted bool              `json:"trigger_accepted" yaml:"trigger_accepted"`
	ExpectedEffects int               `json:"expected_effects" yaml:"expected_effects"`
	ObservedEffects int               `json:"observed_effects" yaml:"observed_effects"`
	EndReason       ConsumerEndReason `json:"end_reason,omitempty" yaml:"end_reason,omitempty"`
}

// FormatConsumerRun renders the one-line consumer window summary that follows
// a test's effect rows, e.g.
//
//	window: 2 of 2 effects observed, trigger accepted, ended count_reached
//
// It is one line and not a block because everything a reader ACTS on is
// already in the effects[i] rows; this says what the window itself did, which
// is what tells "the worker produced the wrong thing" apart from "we stopped
// looking too early". Returns "" for a nil receiver so a non-consumer test
// renders byte-identically to a build without this field.
func (r *ConsumerResultInfo) FormatConsumerRun() string {
	if r == nil {
		return ""
	}
	accepted := "trigger NOT accepted"
	if r.TriggerAccepted {
		accepted = "trigger accepted"
	}
	reason := string(r.EndReason)
	if reason == "" {
		reason = "no end reason reported"
	}
	return fmt.Sprintf("  window: %d of %d effects observed, %s, ended %s\n",
		r.ObservedEffects, r.ExpectedEffects, accepted, reason)
}

// Mock Spec.Metadata keys stamped by a protocol recorder so the OSS consumer
// recorder can bin mocks into units without knowing the protocol.
//
// They live in OSS precisely so a parser in another repository cannot drift
// the spelling: a typo would not fail to compile, it would silently produce a
// consumer test with no trigger and no effects, which mints as "no observable
// effect" or replays as a vacuous pass.
const (
	// MetaKeyRole is "trigger", "effect", or absent. ABSENT IS THE DEFAULT
	// AND MEANS ORDINARY MOCK: an untagged mock behaves exactly as it does
	// today, which is what makes this contract additive.
	MetaKeyRole = "role"

	// MetaKeyOp is the protocol operation the mock carries, copied into
	// EffectView.Op.
	MetaKeyOp = "op"

	// MetaKeyTarget is the topic/queue/subscription, copied into
	// EffectView.Target.
	MetaKeyTarget = "target"

	// MetaKeyUnit is the recorder's in-memory consumer-unit id. It is the
	// TRIGGER IDENTITY EXEMPTION: a mock tagged role=trigger with unit u-i
	// belongs to unit i, never to whichever timestamp window it lands in.
	MetaKeyUnit = "unit"

	// MetaKeyRecords is how many protocol records the mock's payload
	// carries, as a decimal string (Spec.Metadata is map[string]string).
	MetaKeyRecords = "records"

	// MetaKeyOrderBy names the ONE Coords key that splits a (protocol,
	// target) pair into independently-ordered lanes, and is copied verbatim
	// into ConsumerSpec.OrderBy at mint time. A Kafka recorder stamps
	// `orderBy: partition` on its trigger mock; Pulsar and SQS stamp nothing.
	//
	// IT IS STAMPED BY THE PROTOCOL RECORDER, NOT CHOSEN BY OSS, and that is
	// the whole point: naming "partition" anywhere in this repository would
	// put a Kafka concept in the one package that must contain nothing
	// Kafka-shaped, and would silently do nothing for a protocol that spells
	// its lane differently. Without a producer, ConsumerSpec.OrderBy is
	// always empty and every recording gets the STRICTER reading (one lane
	// per (protocol, target)), which turns two goroutines producing to
	// different partitions of one topic into a spurious
	// EFFECT_TARGET_CHANGED — the exact false red design §5 says is handled.
	MetaKeyOrderBy = "orderBy"

	// MetaKeyUnitTest names the test case a trigger mock belongs to. It is
	// the machine-readable half of the trigger identity exemption: the
	// record-side window resolver bins a mock carrying it into THAT test,
	// whatever window its timestamp falls in.
	//
	// WHY IT IS NEEDED. A consumer unit's window runs from the trigger's
	// RESPONSE time to the NEXT trigger's response time, because every
	// mainstream client issues the following poll from inside the current
	// one, before the application has processed the record it is holding —
	// so a window cut at the request would swallow the next trigger. But
	// that leaves each trigger's OWN mock outside its own window (its
	// request time precedes its response time) and inside the PREVIOUS
	// one. Without this key a trigger is attributed to the previous test
	// for the first few tests of a recording and dropped by the stale
	// buffer cutoff after that — which is to say replay would have no
	// bytes to deliver.
	//
	// UNLIKE THE OTHER KEYS THIS ONE IS IN-FLIGHT ONLY, exactly like the
	// raw response bytes a parser attaches while a mock is in memory. It
	// is stamped by the OSS consumer recorder (never by a parser, so it
	// cannot drift), consumed by the window resolver, and REMOVED before
	// the mock is written — mocks.yaml keeps the shape it has today, and
	// the mapping stays the single persisted statement of which mocks
	// belong to which test.
	MetaKeyUnitTest = "unitTest"
)

// Values for MetaKeyRole.
const (
	// RoleTrigger marks the mock whose payload is delivered to the worker
	// to start a unit.
	RoleTrigger = "trigger"

	// RoleEffect marks a mock produced by the worker while handling a
	// trigger.
	RoleEffect = "effect"
)

// Consumer FailureCategory constants.
//
// Every one of these sets TestStatusFailed and is attached to
// TestResult.FailureInfo.Category. There is deliberately no
// TestStatusUnsupported: that enum value does not exist, StringToTestSetStatus
// errors on anything unknown, and introducing it would be a lock-step
// three-repository change whose half-done state parses as unknown and defaults
// to GREEN. A named category on a FAILED test gives a non-zero exit, a correct
// FailCount and a red status with zero on-disk format change.
//
// The first eleven are REFUSALS: v1 cannot honestly judge this test, so it
// says so by name instead of guessing. The last four are VERDICTS: the
// assertion ran and this is what it found.
const (
	// --- refusals (design §5) ---

	// CategoryConsumerNoObservableEffect: a unit with zero effects and zero
	// mapped writes. Such a test can only ever pass, so recording it would
	// manufacture a vacuous green. Refused at mint.
	CategoryConsumerNoObservableEffect FailureCategory = "CONSUMER_NO_OBSERVABLE_EFFECT"

	// CategoryConsumerOpaqueEffectBody: an expected or observed effect
	// carries DecodedOpaque. Comparing a misparse against the same misparse
	// is a silent PASS; refusing is the only safe verdict.
	CategoryConsumerOpaqueEffectBody FailureCategory = "CONSUMER_OPAQUE_EFFECT_BODY"

	// CategoryConsumerUnsupportedWireVersion: the parser met a wire version
	// it does not model and declined to best-effort parse it.
	CategoryConsumerUnsupportedWireVersion FailureCategory = "CONSUMER_UNSUPPORTED_WIRE_VERSION"

	// CategoryConsumerTriggerDiscarded: keploy wrote the trigger and the
	// client discarded it. Pairs with ConsumerEndReasonTriggerDiscarded.
	CategoryConsumerTriggerDiscarded FailureCategory = "CONSUMER_TRIGGER_DISCARDED"

	// CategoryConsumerEffectStraddlesUnit: one produced frame carried
	// records belonging to two consumer units. BOTH units are refused by
	// name rather than one being silently dropped or reported as an extra.
	CategoryConsumerEffectStraddlesUnit FailureCategory = "CONSUMER_EFFECT_STRADDLES_UNIT"

	// CategoryConsumerMultiConnectionRecording: the recording spanned more
	// than one broker connection (a reconnect or a rebalance), so recorded
	// session identity cannot be replayed coherently.
	CategoryConsumerMultiConnectionRecording FailureCategory = "CONSUMER_MULTI_CONNECTION_RECORDING"

	// CategoryConsumerEffectCacheOverflow: the recorder's bounded payload
	// cache dropped a projected effect. Dropping a view refuses its test —
	// never a silent drop, which would under-count ExpectEffects and turn a
	// real over-produce into a pass.
	CategoryConsumerEffectCacheOverflow FailureCategory = "CONSUMER_EFFECT_CACHE_OVERFLOW"

	// CategoryConsumerUnitsLost: more consumer units were observed than
	// test cases were persisted. Fails the RECORDING, out loud, instead of
	// quietly producing a smaller suite than the user watched being made.
	CategoryConsumerUnitsLost FailureCategory = "CONSUMER_UNITS_LOST"

	// CategoryConsumerMappingsRequired: a consumer test set without usable
	// mappings. Timestamp-window filtering cannot arm exactly one trigger,
	// so replaying without mappings would deliver the wrong message.
	CategoryConsumerMappingsRequired FailureCategory = "CONSUMER_MAPPINGS_REQUIRED"

	// CategoryConsumerRepeatPassUnsupported: --retryPassing / --must-pass
	// over a consumer set. A fetch position and an idempotent producer
	// sequence do not rewind between attempts, so a second pass over the
	// same app process is not a repeat of the first.
	CategoryConsumerRepeatPassUnsupported FailureCategory = "CONSUMER_REPEAT_PASS_UNSUPPORTED"

	// CategoryConsumerProjectorFailed: the projector for a payload's
	// protocol panicked, or no projector was registered for it at all.
	//
	// NOT ONE OF THE ELEVEN the design tabulated, and deliberately
	// separate from CategoryConsumerUnsupportedWireVersion. That category
	// means the decoder met a wire version it declines to model — the
	// refuse-don't-guess contract working as designed, and a user-visible
	// limitation with a user-visible remedy. A projector that crashes, or
	// one a parser forgot to register, is a KEPLOY DEFECT. Reporting the
	// two under one name would send whoever reads the recording to look
	// for the wrong thing, and folding a crash into "unsupported version"
	// is how a bug gets filed as a limitation and never fixed.
	CategoryConsumerProjectorFailed FailureCategory = "CONSUMER_PROJECTOR_FAILED"

	// CategoryConsumerUnsupportedAgent: the running agent does not
	// implement the consumer instrumentation. The set is REFUSED, not
	// degraded: there is no weak-verdict path that can still print PASSED.
	CategoryConsumerUnsupportedAgent FailureCategory = "CONSUMER_UNSUPPORTED_AGENT"

	// CategoryConsumerTriggerNotDelivered: the delivery window closed and the
	// application never took the trigger — it never polled, never subscribed,
	// crashed at boot, or joined the wrong group.
	//
	// DELIBERATELY DISTINCT FROM CategoryConsumerTriggerDiscarded, by the same
	// argument that separates CategoryConsumerProjectorFailed from
	// CategoryConsumerUnsupportedWireVersion. "The client threw our bytes
	// away" and "the client never asked for anything" have opposite remedies:
	// the first is a keploy fidelity bug (a session id, a fetch position), the
	// second is the application not running the consumer at all. Reporting
	// both under one name sends whoever reads the report to look for the wrong
	// thing. Pairs with ConsumerEndReasonTriggerNotDelivered.
	CategoryConsumerTriggerNotDelivered FailureCategory = "CONSUMER_TRIGGER_NOT_DELIVERED"

	// CategoryConsumerCompletionTimeout: the completion backstop fired. The
	// assertion is INCOMPLETE, not satisfied — more effects may still have
	// been in flight — so the verdict is FAILED with this name rather than a
	// pass on whatever happened to arrive first. Pairs with
	// ConsumerEndReasonTimeout.
	CategoryConsumerCompletionTimeout FailureCategory = "CONSUMER_COMPLETION_TIMEOUT"

	// CategoryConsumerUnsupportedSpec: the persisted spec asks for something
	// this build cannot honestly judge — no ConsumerSpec at all on a
	// Kind: Consumer test case, or an effect whose `assert` mode this version
	// does not implement.
	//
	// It exists so that the three structural refusals do not have to be
	// smuggled under a category that means something else. Reusing
	// CategoryConsumerNoObservableEffect for a spec-less test would be
	// literally true (such a test can only ever pass) and practically
	// misleading: it names a RECORDING problem with a known remedy
	// ("re-record; the worker produced nothing"), while this is a FILE
	// problem with a different one ("the test case is corrupt or was written
	// by a newer build").
	CategoryConsumerUnsupportedSpec FailureCategory = "CONSUMER_UNSUPPORTED_SPEC"

	// CategoryConsumerRunCancelled: the run was torn down (Ctrl-C, a cancelled
	// context) while a test's window was open.
	//
	// It exists because the gate previously reported this as
	// CONSUMER_UNSUPPORTED_AGENT, which is a different and much more alarming
	// claim: a cancelled run is not an agent that lacks consumer support, and
	// an agent reading that name would go looking for a missing capability
	// instead of noticing that someone stopped the run.
	CategoryConsumerRunCancelled FailureCategory = "CONSUMER_RUN_CANCELLED"

	// --- verdicts (design §1 demo, §5 "What is asserted") ---

	// CategoryEffectBodyChanged: an effect matched on
	// protocol/op/target/key and its payload differs. The per-field diffs
	// are the DepResult meta rows.
	CategoryEffectBodyChanged FailureCategory = "EFFECT_BODY_CHANGED"

	// CategoryEffectMissing: a recorded effect was not observed. THE
	// FLAGSHIP REGRESSION — the worker stopped producing. It must never be
	// demotable to OBSOLETE.
	CategoryEffectMissing FailureCategory = "EFFECT_MISSING"

	// CategoryEffectUnexpected: an effect was observed that the recording
	// does not have. The over-produce (N+1) regression; catching it is why
	// the grace drain is mandatory.
	CategoryEffectUnexpected FailureCategory = "EFFECT_UNEXPECTED"

	// CategoryEffectTargetChanged: an effect went to a different
	// target/op/key than recorded — a routing regression, distinct from a
	// payload change because the remedy is different.
	CategoryEffectTargetChanged FailureCategory = "EFFECT_TARGET_CHANGED"

	// CategoryEffectHeadersChanged: an effect matched on
	// protocol/op/target/key and its message HEADERS differ. The per-header
	// diffs are DepResult meta rows keyed effects.<i>.headers.<name>.
	//
	// SEPARATE FROM CategoryEffectBodyChanged BECAUSE THE REMEDY IS. The
	// headers a worker attaches are routing, tenancy and tracing metadata: a
	// dropped `tenant` header sends a message to the wrong customer with a
	// byte-identical payload, and reporting that as a payload change points
	// the reader at a diff that does not exist. It is the same argument that
	// keeps EFFECT_TARGET_CHANGED separate from EFFECT_BODY_CHANGED.
	CategoryEffectHeadersChanged FailureCategory = "EFFECT_HEADERS_CHANGED"
)

// Effect row vocabulary for models.Result.DepResult.
//
// The consumer projector is the second writer of DepResult, alongside the
// sync path's presence rows. The two are told apart by ROW-NAME PREFIX, a
// settled one-way door documented in depresult.go: `deps[i]` is the sync
// path's presence-only claim (which covers outgoing READS as well as writes,
// so calling one a "write" would be wrong), and `effects[i]` is this
// projector's, carrying field-level diffs decoded from a protocol payload.
// `writes[i]` is retired and used by neither.
//
//	effects[<i>] <protocol> <op> <target> key=<key>
//
// <i> indexes the test's RECORDED effect list, never the emitted rows, so an
// effect keeps the same name from run to run whatever else went missing.
const (
	// EffectRowPrefix is the row-name prefix that identifies a consumer
	// projector row. Matched as a prefix; the index and the rest follow.
	EffectRowPrefix = "effects["

	// EffectKeyPrefix is the DepMetaResult.Key namespace for effect
	// assertions: effects.<i>.<what>. Body field diffs extend it with
	// body.<dotted.path> so a reported key pastes straight into
	// spec.assertions.noise.
	EffectKeyPrefix = "effects"

	// EffectKeyPresence is the DepMetaResult.Key of the assertion "this
	// recorded effect was observed at all".
	EffectKeyPresence = "presence"

	// EffectKeyUnexpected is the DepMetaResult.Key of the assertion "no
	// effect beyond the recorded ones was observed".
	EffectKeyUnexpected = "unexpected"

	// EffectKeyOpaque is the DepMetaResult.Key of the assertion "this
	// effect's payload was confidently decoded".
	EffectKeyOpaque = "decoded"

	// EffectKeyIdentity is the DepMetaResult.Key of the assertion "this
	// effect went where the recording says it went": protocol, op, target and
	// key together. It is separate from the body keys because the remedy is
	// different — a routing regression is not a payload regression — and
	// because a single line naming both sides is what a human acts on.
	EffectKeyIdentity = "identity"

	// EffectKeyBody is the DepMetaResult.Key used when a payload is compared
	// WHOLE rather than field by field: a non-JSON body, or two JSON bodies
	// where one side did not parse. Field-level comparison uses
	// EffectBodyKeyPrefix instead.
	EffectKeyBody = "body"

	// EffectKeyHeaders is the DepMetaResult.Key namespace for message-header
	// assertions: effects.<i>.headers.<name>. Headers ARE asserted — a
	// dropped tenant, routing or trace header is a real worker regression, and
	// leaving a persisted, rendered, on-disk field unasserted is the silent
	// pass rule 7 forbids. Like a body path, a reported header key pastes
	// straight into spec.assertions.noise, which is how a genuinely
	// per-run header (a fresh traceparent) is silenced per test rather than
	// per test set.
	EffectKeyHeaders = "headers"

	// EffectUnexpectedIndex is the index token of a row describing an effect
	// that is NOT in the recording, mirroring DepSummaryIndex.
	//
	// A row name's index is documented as the effect's position in the
	// RECORDED list, so that a name is stable across runs. An unexpected
	// effect has no such position, and inventing one (its position among the
	// OBSERVED effects, say) would produce a name that means something
	// different in every run and collides with a real recorded effect's name.
	// The token cannot appear in a real index.
	EffectUnexpectedIndex = "*"

	// EffectPresenceObserved / EffectPresenceMissing are the Expected /
	// Actual values of a presence assertion. Literal strings because
	// DepMetaResult.Expected/Actual are strings in the persisted schema and
	// must stay that way.
	EffectPresenceObserved = "observed"
	EffectPresenceMissing  = "not observed"

	// EffectDecodedConfidentValue is the Expected side of an opaque-payload
	// row; the Actual side carries whatever the projector actually stamped.
	EffectDecodedConfidentValue = "confident"
)

// EffectRowName builds the stable DepResult.Name for one recorded effect.
// index is the effect's position in the test's RECORDED effect list — never
// its position among the emitted rows, which would make the name depend on
// what else diverged in that particular run.
//
// protocol, op, target and key are all optional: a projector does not always
// carry every one, and a row must stay readable without them rather than
// degrade into a run of empty spaces.
func EffectRowName(index int, protocol, op, target, key string) string {
	return effectRowName(strconv.Itoa(index), protocol, op, target, key)
}

// effectRowName is EffectRowName over an already-rendered index token, so the
// numeric rows and the EffectUnexpectedIndex row cannot drift in shape.
func effectRowName(index, protocol, op, target, key string) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s%s]", EffectRowPrefix, index)
	for _, part := range []string{protocol, op, target} {
		if part = strings.TrimSpace(part); part != "" {
			sb.WriteString(" ")
			sb.WriteString(part)
		}
	}
	if key = strings.TrimSpace(key); key != "" {
		sb.WriteString(" key=")
		sb.WriteString(key)
	}
	return sb.String()
}

// EffectRowNameFor is EffectRowName for a whole view.
func EffectRowNameFor(index int, v EffectView) string {
	return EffectRowName(index, v.Protocol, v.Op, v.Target, v.Key)
}

// EffectUnexpectedRowName builds the DepResult.Name for an effect the worker
// produced that the recording does not contain. It carries
// EffectUnexpectedIndex rather than a number: see that constant.
func EffectUnexpectedRowName(v EffectView) string {
	return effectRowName(EffectUnexpectedIndex, v.Protocol, v.Op, v.Target, v.Key)
}

// EffectIdentity renders the comparable identity of a view — protocol, op,
// target and key — as one line, for the two sides of an identity assertion.
// It deliberately excludes Body and Coords: coords are noise by default, and
// the body has its own assertion.
func EffectIdentity(v EffectView) string {
	var sb strings.Builder
	for _, part := range []string{v.Protocol, v.Op, v.Target} {
		if part = strings.TrimSpace(part); part != "" {
			if sb.Len() > 0 {
				sb.WriteString(" ")
			}
			sb.WriteString(part)
		}
	}
	if key := strings.TrimSpace(v.Key); key != "" {
		if sb.Len() > 0 {
			sb.WriteString(" ")
		}
		sb.WriteString("key=")
		sb.WriteString(key)
	}
	return sb.String()
}

// SameEffectIdentity reports whether two views address the same destination:
// protocol, op, target and key all equal. Coords are excluded on purpose (they
// are noise by default) and so is the body (it has its own assertion).
func SameEffectIdentity(a, b EffectView) bool {
	return a.Protocol == b.Protocol && a.Op == b.Op && a.Target == b.Target && a.Key == b.Key
}

// IsEffectRow reports whether a DepResult row was written by the consumer
// projector rather than the sync-path presence writer. Renderers dispatch on
// this, never on a test's Kind: the row prefix is the documented
// discriminator, and it keeps the renderers Kind-agnostic.
func IsEffectRow(d DepResult) bool {
	return strings.HasPrefix(d.Name, EffectRowPrefix)
}

// EffectBodyKeyPrefix returns the DepMetaResult.Key prefix for one effect's
// body field diffs: "effects.<i>.body.". A differ appends its own dotted path
// to it, producing keys like effects.0.body.status that paste straight into
// spec.assertions.noise.
func EffectBodyKeyPrefix(index int) string {
	return EffectKeyPrefix + "." + strconv.Itoa(index) + ".body."
}

// EffectKey builds a non-body assertion key for one effect:
// "effects.<i>.<what>", e.g. effects.0.presence.
func EffectKey(index int, what string) string {
	return EffectKeyPrefix + "." + strconv.Itoa(index) + "." + what
}

// EffectHeaderKeyPrefix returns the DepMetaResult.Key prefix for one effect's
// header diffs: "effects.<i>.headers.". The differ appends the header's own
// name, producing keys like effects.0.headers.traceparent that paste straight
// into spec.assertions.noise.
func EffectHeaderKeyPrefix(index int) string {
	return EffectKeyPrefix + "." + strconv.Itoa(index) + "." + EffectKeyHeaders + "."
}

// RecordWindow returns the RECORD-TIME request/response window of a test case,
// whatever its Kind. Either value may be zero when the recording did not
// populate it; callers that need a non-zero anchor use EarliestTimestamp.
//
// A CONSUMER test's window is its trigger's request/response times, carried on
// the spec. Without this arm every consumer test would report the zero time
// and fall back to Created's ONE-SECOND granularity — against 300ms seed
// spacing that collapses several units into one window, corrupting mapping
// regeneration; and the zero time feeds TLS certificate generation, which
// then anchors every certificate on wall-clock now instead of on the recorded
// exchange (see EarliestTimestamp for what that does and does not cost).
func (tc *TestCase) RecordWindow() (time.Time, time.Time) {
	if tc == nil {
		return time.Time{}, time.Time{}
	}
	switch tc.Kind {
	case CONSUMER:
		if tc.ConsumerSpec == nil {
			return time.Time{}, time.Time{}
		}
		return tc.ConsumerSpec.ReqTimestampMock, tc.ConsumerSpec.ResTimestampMock
	case GRPC_EXPORT:
		return tc.GrpcReq.Timestamp, tc.GrpcResp.Timestamp
	case HTTP, HTTP2:
		return tc.HTTPReq.Timestamp, tc.HTTPResp.Timestamp
	}
	// Unknown or empty Kind (older recordings): fall back to whichever
	// payload is populated, HTTP first — the overwhelmingly common case.
	if !tc.HTTPReq.Timestamp.IsZero() || !tc.HTTPResp.Timestamp.IsZero() {
		return tc.HTTPReq.Timestamp, tc.HTTPResp.Timestamp
	}
	return tc.GrpcReq.Timestamp, tc.GrpcResp.Timestamp
}

// EarliestTimestamp returns the earliest non-zero recorded timestamp for a
// test case: the start of its window, falling back to the end of its window,
// then to Created.
//
// It exists because Backdate (which anchors generated TLS certificates) was
// read straight off HTTPReq.Timestamp, which is the ZERO TIME for any
// non-HTTP test case.
//
// WHAT A ZERO BACKDATE ACTUALLY DOES, because the obvious story is wrong and
// design §4 P8 tells the wrong one. It does NOT produce a 1970 certificate:
// tls.CertForClient substitutes time.Now() for a zero backdate before
// subtracting a year, so zero has always meant "valid from a year ago" and has
// always been safe. What it loses is the RELATIONSHIP TO THE RECORDING: a
// certificate generated today for traffic recorded weeks ago has nothing to do
// with the exchange it is standing in for. That is the defect this fixes, and
// it is why the §9 companion requirement to "refuse to generate a certificate
// from a zero Backdate" is deliberately not implemented — it would turn a
// working default into a hard failure for every recording whose test cases
// carry no timestamp.
//
// Returns the zero time only when the test case carries no usable timestamp at
// all, which callers pass straight through: ca.go has always handled it.
func (tc *TestCase) EarliestTimestamp() time.Time {
	if tc == nil {
		return time.Time{}
	}
	req, resp := tc.RecordWindow()
	if !req.IsZero() {
		return req
	}
	if !resp.IsZero() {
		return resp
	}
	if tc.Created > 0 {
		return time.Unix(tc.Created, 0)
	}
	return time.Time{}
}
