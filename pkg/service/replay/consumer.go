package replay

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// The consumer judge.
//
// CompareEffects is to a Kind: Consumer test what CompareHTTPResp is to an
// HTTP one: it takes what the recording says the worker produced while
// handling one message, takes what the worker actually produced during this
// test's delivery window, and decides. It writes its findings as
// models.Result.DepResult rows — the same field the sync path's dependency
// writer uses — so the CLI renderer, the JUnit writer and the `--format json`
// NDJSON projection all pick them up with no change of their own.
//
// # IT SHARES NO CODE WITH THE MOCK MATCHER, AND THAT IS THE POINT
//
// keploy's mock matcher decides which recorded mock answers a live call. It is
// deliberately lenient, and it has to be: it scores a candidate and accepts it
// above a threshold (a Kafka request is accepted at 0.65 with ZERO body
// agreement, and a test in that package pins that floor as a SAFETY property),
// and before scoring it strips fields by bare NAME at every nesting depth —
// timestamp, host, sequence, epoch, createTime — because those are the fields
// that legitimately drift between a recording and a replay of the same call.
//
// Every one of those properties is correct for choosing a mock and
// catastrophic for judging a payload. A judge built on the matcher would
// accept a produced message with a completely different body as "matching",
// and could not see a diff in any field a real message happens to call
// `timestamp` or `host` — which is most event envelopes ever written. The
// consequence is not a missed diff, it is a GREEN TEST for a broken worker.
//
// So this file imports nothing from pkg/matcher and reimplements the small
// amount of JSON walking it needs (flattenJSON below, ~40 lines). That
// duplication is deliberate and is the cheapest possible price for the
// property. TestTheJudgeSeesThroughTheMatchersNoiseNames pins it with a
// payload made entirely of matcher-filtered field names.
//
// # WHAT IS ASSERTED
//
// Per recorded effect: protocol, op, target and key exactly; the body field by
// field when it is JSON and the projector decoded it confidently. Coordinates
// (partition, offset, broker timestamp, receipt handle …) are NOISE BY
// DEFAULT and are never asserted — asserting an offset would redden every
// suite on the next re-record. Order is asserted WITHIN a lane and not across
// lanes; see ConsumerSpec.OrderBy for what a lane is and why OSS never names a
// protocol coordinate to find one.
//
// # WHAT MAKES A PASS
//
// A pass needs three things, and the third is the one that is easy to forget:
// every recorded effect matched, no effect arrived that the recording does not
// have, AND the window closed for the right reason. A test whose effects all
// matched but whose completion backstop fired has NOT been fully observed —
// more effects may have been in flight — so it is FAILED with a named reason
// rather than passed on whatever happened to arrive first. Anything this
// version cannot honestly judge is refused BY NAME (models.FailureCategory)
// with a FAILED verdict; there is no silent pass and no status enum invented
// to carry it.
//
// See keploy-consumer-design-v2.md §0.3, §5 and §9.

// consumerVerdict is the complete outcome of judging one consumer test:
// whether it passed, the rows that explain it, and the failure categories a
// machine keys remedies on.
type consumerVerdict struct {
	Pass bool
	// Rows are models.Result.DepResult entries, all carrying the
	// models.EffectRowPrefix name prefix that identifies this producer.
	Rows []models.DepResult
	// Categories are the named failures, most specific first. Empty on a pass.
	Categories []models.FailureCategory
	// Summary is a one-line human explanation used for the replay log line.
	Summary string
}

// consumerDepAssertion is what the replay loop knows about the OTHER half of a
// consumer test's claim: the sync path's per-test dependency presence
// assertion (the deps[i] rows built from mappings.yaml).
//
// THE JUDGE HAS TO BE TOLD, BECAUSE ONE SHAPE OF TEST HAS NO OTHER CLAIM. A
// consume-and-write-to-a-database worker records `effects: []` with
// `expectEffects: 0` and a bare `sideEffects: N` count. That count is a
// RECORD-TIME FACT and nothing on the replay path ever turns it into an
// assertion: the whole claim is carried by the sync path's presence rows, and
// those rows are computed only when depAssertionValid holds AND this test's
// mapping holds a mock the recording attributed to the worker. When it does
// not (a revoked mapping, a mock reaped by the seven-second cutoff, a
// partially regenerated --update-test-mapping, or — the case OSS is in until a
// parser tags what the worker produced — a mapping whose only cross-family
// entry is untagged ambient traffic), the sync path asserts nothing about the
// worker, the judge has nothing of its own to assert either, and without this
// the test is reported PASSED with zero assertions executed — design §5's
// false-pass row 0 on one of the two most common consumer shapes.
//
// So the judge refuses such a test BY NAME instead. See the vacuity guard in
// compareEffects.
type consumerDepAssertion struct {
	// HasMapping is whether mappings.yaml carries an entry for this test.
	HasMapping bool
	// Ran is whether the presence assertion was actually computed for this
	// test AND had at least one mapped mock POSITIVELY ATTRIBUTED to the
	// worker's production (role=effect). A mapping made entirely of DNS and
	// reusable-tier mocks is filtered to nothing and asserts nothing; a
	// mapping made only of the test's own trigger asserts only that keploy
	// delivered its own message; a mapping carrying untagged cross-family
	// traffic asserts only that some call of some other protocol happened
	// again, which is what the ambient egress of any process does. All three
	// are indistinguishable from no mapping at all as far as this test's claim
	// about the WORKER is concerned. See newConsumerDepAssertion.
	Ran bool
}

// newConsumerDepAssertion builds the judge's view of the sync path's per-test
// dependency assertion. It is a pure function, and it is a function at all
// because the expression it replaces lived on one line inside RunTestSet where
// nothing could reach it: a reviewer rewrote it as `... || true` with every
// identifier preserved and the whole package stayed green, which reopens the
// vacuity guard it feeds.
//
// THE TRIGGER IS NOT EVIDENCE ABOUT THE WORKER. `len(expected) > 0` is not the
// predicate: expected still contains this test's own role=trigger mock, and
// every mapped consumer test carries one (the recorder mints exactly "its
// trigger and its effect"). A mapping degraded to trigger-only — the write
// mock revoked, reaped by the stale cutoff, or landed in the previous window —
// would then report Ran=true while the only thing asserted is that keploy's
// own trigger mock was consumed, and a consume-and-write test would pass with
// zero assertions about its writes. hasUnconsumedEffectMock already agrees the
// trigger is not evidence: it skips role=trigger outright.
//
// SO THE PREDICATE IS POSITIVE ATTRIBUTION, AND NOTHING WEAKER: a mapped mock
// the recording TAGGED role=effect. "Of a different protocol family than the
// trigger" was tried and is not evidence at all — it is satisfied by exactly
// the traffic the claim is made of. Recorder.onOther counts every mock of
// another Kind that lands inside the unit's window, which is the process's
// whole ambient egress (a /health handler's database ping, a metrics push, a
// background cron INSERT), and mappings.yaml is built from that SAME window
// authority, so the ambient mock is in this test's mapping next to its
// trigger. A worker that took the message and silently dropped it then minted
// `sideEffects: 1`, mapped an unrelated Postgres call, and the different-Kind
// arm reported Ran=true — the delegate vouching for the claim using the very
// number it was supposed to check. Design §5's false-pass row 3 (the flagship
// CONSUMER_NO_OBSERVABLE_EFFECT row), one layer removed rather than closed.
//
// THIS DELIBERATELY DIVERGES FROM hasUnconsumedEffectMock, AND THE DIVERGENCE
// IS THE SAFETY PROPERTY. The two predicates answer opposite questions, so
// they must fail in opposite directions:
//
//	hasUnconsumedEffectMock gates a RED (may a PASSED test be promoted to
//	FAILED?). It fails CLOSED — anything not positively identified as
//	same-family coordination traffic vetoes — because its wrong answer is a
//	missed regression.
//
//	mappingCanCarryAnEffectClaim gates a GREEN (may a test with no assertable
//	effect pass on a delegated claim?). It fails closed the OTHER way — only
//	positive attribution counts — because its wrong answer is a silent pass,
//	which this whole slice exists to make impossible.
//
// One vocabulary (role tags read off the recording) and two readers with
// opposite defaults; they were briefly one predicate and it could not be right
// for both. TestTheTwoEffectPredicatesFailInOppositeDirections pins the pair.
//
// THE COST, STATED PLAINLY: until a parser stamps role=effect on the mocks a
// worker produced, a consume-and-write recording whose only claim is
// `sideEffects: N` is REFUSED BY NAME at replay rather than passed. That is
// rule 7 — anything v1 cannot handle gets a named refusal and a FAILED
// verdict, never a silent pass — and OSS ships no such parser, so nothing in
// this repository regresses. Slice 6 arms it by tagging what it produced.
func newConsumerDepAssertion(hasMapping, depAssertionValid bool, expected []string, lookup map[string]mockDisplayInfo) consumerDepAssertion {
	return consumerDepAssertion{
		HasMapping: hasMapping,
		Ran:        depAssertionValid && mappingCanCarryAnEffectClaim(expected, lookup),
	}
}

// mappingCanCarryAnEffectClaim reports whether any mock in this test's
// filtered per-test mapping is POSITIVELY ATTRIBUTED to something the worker
// produced: the recording tagged it role=effect.
//
// Nothing else qualifies, and the exclusions are the point rather than an
// omission — see newConsumerDepAssertion for why "a different protocol family
// than the trigger" is satisfied by the ambient traffic the claim is made of,
// and why this predicate must fail in the opposite direction from
// hasUnconsumedEffectMock. A mock the registry could not describe, an untagged
// cross-family write and the test's own trigger all carry nothing here.
func mappingCanCarryAnEffectClaim(expected []string, lookup map[string]mockDisplayInfo) bool {
	for _, name := range expected {
		if lookup[name].role == models.RoleEffect {
			return true
		}
	}
	return false
}

// CompareEffects judges tc against the delivery window's result and returns
// the (pass, result) pair the replay loop's per-test block expects, mirroring
// CompareHTTPResp / CompareGRPCResp.
//
// dep carries what the caller knows about the sync path's per-test dependency
// assertion; see consumerDepAssertion for why the judge cannot decide one
// shape of test without it.
//
// It is a method on *Replayer only, deliberately NOT a new method on the
// replay.Service interface: Service is implemented outside this repository,
// and Go has no optional interface methods, so widening it would break every
// implementor at compile time for a capability none of them can use yet.
func (r *Replayer) CompareEffects(tc *models.TestCase, res *models.ConsumerResult, testSetID string, emitFailureLogs bool, dep consumerDepAssertion) (bool, *models.Result) {
	verdict := compareEffects(tc, res, dep)

	result := &models.Result{
		// A consumer test has no status code. Marking it Normal keeps the
		// report's status-diff renderer — which prints whenever Normal is
		// false — from emitting a meaningless "expected 0, got 0" line under
		// every failed consumer test.
		StatusCode: models.IntResult{Normal: true},
		DepResult:  verdict.Rows,
	}
	if !verdict.Pass {
		result.FailureInfo.Risk = models.High
		for _, c := range verdict.Categories {
			result.FailureInfo.Category = appendCategoryUnique(result.FailureInfo.Category, c)
		}
	}

	if verdict.Pass {
		r.logger.Debug("consumer effects matched",
			zap.String("testcase", tc.Name),
			zap.String("testset", testSetID),
			zap.String("summary", verdict.Summary))
		return true, result
	}

	// A CONSUMER FAILURE IS ALWAYS EXPLAINED. emitFailureLogs is true for this
	// Kind at every call site: RunTestSet computes it as
	// shouldEmitFailureLogs(..., neverDemotable) and neverDemotableKind is
	// keyed on Kind alone, so the historical mock-drift suppression can never
	// apply here; the streaming/late path passes a literal true. The parameter
	// is kept so the signature matches the other two comparators and so a
	// future caller cannot silently reintroduce the suppression — pinned by
	// TestFailureLogsAreNeverSuppressedForANonDemotableKind, which covers the
	// case that used to slip through: a mock set that diverged only on
	// coordination traffic, where the NARROW predicate is false.
	if emitFailureLogs {
		r.logger.Error("consumer effect assertion failed",
			zap.String("testcase", tc.Name),
			zap.String("testset", testSetID),
			zap.Strings("categories", categoryStrings(verdict.Categories)),
			zap.String("summary", verdict.Summary),
			zap.Strings("findings", models.MissingDepNames(verdict.Rows)),
			zap.String("next_step", "each finding is an effects[i] row in the report; paths of the form effects.<i>.body.<field> paste straight into the test's spec.assertions.noise if the field is genuinely non-deterministic"))
	}
	return false, result
}

func categoryStrings(cats []models.FailureCategory) []string {
	out := make([]string, 0, len(cats))
	for _, c := range cats {
		out = append(out, string(c))
	}
	return out
}

// compareEffects is the whole judge, as a pure function of the recorded test
// case and the gate's result. Keeping it free of the Replayer, the logger and
// the config is what makes the comparator table a plain unit test.
func compareEffects(tc *models.TestCase, res *models.ConsumerResult, dep consumerDepAssertion) consumerVerdict {
	if tc == nil || tc.ConsumerSpec == nil {
		return refusal(models.CategoryConsumerUnsupportedSpec, "",
			"this test case is Kind: Consumer but carries no consumer spec, so there is nothing to assert",
			"a judgeable test case")
	}
	spec := tc.ConsumerSpec

	if res == nil {
		return refusal(models.CategoryConsumerUnsupportedAgent, spec.Protocol,
			"the agent returned no consumer result for this test; the running agent does not implement the consumer instrumentation, or it failed before the delivery window opened",
			"a delivery window result")
	}
	if res.TestID != "" && tc.Name != "" && res.TestID != tc.Name {
		return refusal(models.CategoryConsumerUnsupportedAgent, spec.Protocol,
			fmt.Sprintf("the agent returned the delivery window of %q while judging %q; one test's effects must never be judged against another test's spec", res.TestID, tc.Name),
			"the delivery window of this test")
	}

	// A REFUSAL FROM THE GATE OR THE PARSER WINS OUTRIGHT. It was raised
	// closest to the cause, and everything downstream of it is a consequence:
	// reporting "3 effects missing" under "the client discarded the trigger"
	// blames the worker for something keploy did.
	if res.Refused() {
		return refusal(res.Refusal, spec.Protocol, refusalDetail(res), "a judgeable delivery window")
	}

	// The two end reasons that make the effect comparison meaningless, for
	// the same reason: the worker was never given the message, so nothing it
	// did or did not produce is evidence about the worker.
	switch res.EndReason {
	case models.ConsumerEndReasonTriggerNotDelivered:
		return refusal(models.CategoryConsumerTriggerNotDelivered, spec.Protocol,
			"the delivery window closed and the application never took the trigger — check that the worker subscribes to "+triggerTarget(spec)+" on start-up, that it did not crash while booting, and that it joined the recorded consumer group",
			"the application to take the trigger")
	case models.ConsumerEndReasonTriggerDiscarded:
		return refusal(models.CategoryConsumerTriggerDiscarded, spec.Protocol,
			"keploy wrote the trigger and the client threw it away — "+refusalDetail(res),
			"the client to accept the trigger")
	}

	// A test in which NOTHING observable happened can only ever pass. Mint
	// refuses to record one; the judge refuses to grade one, because a spec is
	// a file and files get edited.
	//
	// THE GUARD IS COMPUTED OVER WHAT THIS FILE CAN ACTUALLY ASSERT, WHICH IS
	// NOT len(spec.Effects). compareEffectLists filters IsPresenceOnly() views
	// out of BOTH sides before pairing — nothing decoded them, so there is
	// nothing to compare — so a spec whose effects are ALL presence stand-ins
	// has a non-empty Effects slice and asserts exactly as much as an empty
	// one: nothing. That shape is not hypothetical; Recorder.closeUnit
	// deliberately mints it for the database-write projector shape design §2
	// describes. Keying the guard on the raw slice length skipped it outright
	// for the one spelling the recorder actually produces.
	//
	// SIDE EFFECTS AND PRESENCE VIEWS ARE THE SAME DELEGATED CLAIM. A
	// consume-and-write-to-a-database worker produces no assertable protocol
	// effect at all: its spec is `effects: []` or `effects: [presence…]` with
	// `expectEffects: 0`, plus a `sideEffects: N` count. Both spellings stand
	// for "the worker made those calls again", and this file never turns
	// either into an assertion — the claim is carried ENTIRELY by the sync
	// path's deps[i] presence rows, which exist only when this test has a
	// per-test mock mapping holding a mock POSITIVELY ATTRIBUTED to the
	// worker's production (see consumerDepAssertion). When it does not,
	// accepting either would return Pass with zero rows, zero categories and
	// zero assertions executed: the flagship "the worker stopped writing"
	// regression reported as verified_green. So a test whose ONLY claim is
	// delegated is refused by name whenever the delegate could not check it.
	//
	// "COULD NOT CHECK IT" IS NOT "HAD NOTHING IN THE MAPPING". The delegate
	// is defeated just as completely by a mapping full of traffic that is not
	// attributable to the worker: spec.SideEffects counts every cross-family
	// mock that landed inside the unit's window, mappings.yaml is built from
	// that same window, so a process that also serves /health maps its ambient
	// database ping next to the trigger. Letting that satisfy the delegate is
	// the claim vouching for itself. See newConsumerDepAssertion.
	//
	// The guard is deliberately narrow. A test that records an ASSERTABLE
	// effect still asserts that effect, and refusing it because its delegated
	// half happened to be uncheckable would be a false RED on a partially
	// regenerated mapping. Only the shape whose whole claim is unverifiable is
	// refused.
	assertable := 0
	for _, e := range spec.Effects {
		if !e.IsPresenceOnly() {
			assertable++
		}
	}
	if assertable == 0 && spec.Completion.ExpectEffects <= 0 {
		delegated := spec.SideEffects + (len(spec.Effects) - assertable)
		switch {
		case delegated <= 0:
			return refusal(models.CategoryConsumerNoObservableEffect, spec.Protocol,
				"this test records no effect, expects none and observed no other call while handling its message, so it asserts nothing and can only ever pass; re-record it with the worker producing, or delete it",
				"at least one observable effect")
		case dep.Ran:
			// The claim is carried, and checked, by the sync path — and by a
			// mapped mock the RECORDING attributed to the worker, not by
			// whatever else happened to be in flight. Both halves matter: see
			// newConsumerDepAssertion.
		case !dep.HasMapping:
			return refusal(models.CategoryConsumerMappingsRequired, spec.Protocol,
				fmt.Sprintf("this test's only claim is that the worker made %d call(s) this build cannot field-diff while handling its message, and that claim is asserted from this test's entry in mappings.yaml — which this test set has none for, so nothing would have been checked", delegated),
				"a per-test mock mapping to assert this test's side effects against")
		default:
			return refusal(models.CategoryConsumerNoObservableEffect, spec.Protocol,
				fmt.Sprintf("this test's only claim is that the worker made %d call(s) this build cannot field-diff while handling its message, and no mock in its mapping is tagged as one the worker produced (%s=%s), so nothing there can be told apart from traffic that merely happened at the same time — an unrelated handler's database call inside the same window is counted and mapped identically — and the test would pass without asserting anything about the worker", delegated, models.MetaKeyRole, models.RoleEffect),
				"at least one mock attributed to the worker's production")
		}
	}

	// THE AGENT'S COUNT IS CROSS-CHECKED AGAINST THE FILE. The count
	// assertion below compares two numbers the agent supplied, which on its
	// own would let a spec declaring three expected effects be graded green by
	// an agent that reported 0 of 0 — no pairs, no rows, no count row, and
	// end_reason count_reached. The arm carries this spec's Completion
	// verbatim, so a divergence means the agent did not honour it, which is
	// the same class of mistrust the TestID check above exists to remove.
	if res.ExpectEffects != spec.Completion.ExpectEffects {
		return refusal(models.CategoryConsumerUnsupportedAgent, spec.Protocol,
			fmt.Sprintf("the agent reported %d expected effects for a test whose spec declares %d; the delivery window was not opened with this test's completion rule, so nothing observed inside it can be graded against this spec", res.ExpectEffects, spec.Completion.ExpectEffects),
			fmt.Sprintf("a delivery window expecting %d effect(s)", spec.Completion.ExpectEffects))
	}

	// THE AGENT'S COUNT IS ALSO CROSS-CHECKED AGAINST THE AGENT'S OWN
	// CONTENTS, and this is the third number, not a restatement of the
	// second. res.ObservedEffects is what the count assertion below grades;
	// res.Effects is what compareEffectLists pairs. Nothing else in this
	// function relates them, so an agent that reports "2 observed" and ships
	// no views (or one) is graded 2-against-2 by the count assertion, pairs
	// nothing, and returns Pass with ZERO per-effect comparisons — a green
	// test in which nothing was compared. Probed against this judge:
	// {Effects: nil, ObservedEffects: 2, ExpectEffects: 2} returned
	// pass=true, 0 rows.
	//
	// It is unreachable through THIS repository's gate — Gate.finish derives
	// both from one call to pendingLocked, so they cannot disagree — and that
	// is exactly why it is checked here rather than assumed: models.
	// ConsumerResult is the agent boundary, ConsumerInstrumentation is
	// implemented outside this repository, and slice 6 is the first
	// implementation that is not this one. Same premise as the two checks
	// above: everything in the result is supplied by the agent.
	//
	// PRESENCE STAND-INS ARE EXCLUDED ON BOTH SIDES, mirroring pendingLocked
	// exactly: they are shipped as views and are deliberately not counted
	// into ObservedEffects, so counting them here would refuse every healthy
	// consume-and-write window.
	shippedRecords := 0
	for _, o := range res.Effects {
		if o.IsPresenceOnly() {
			continue
		}
		shippedRecords += o.RecordCount()
	}
	if shippedRecords != res.ObservedEffects {
		return refusal(models.CategoryConsumerUnsupportedAgent, spec.Protocol,
			fmt.Sprintf("the agent reported %d observed effect record(s) but shipped views covering %d; the window's own count and its contents disagree, so nothing in it can be graded", res.ObservedEffects, shippedRecords),
			fmt.Sprintf("a delivery window whose views cover all %d observed record(s)", res.ObservedEffects))
	}

	// AND THE SAME CHECK ON THE FILE, for the same stated reason the vacuity
	// guard gives: a spec is a file and files get edited. Recorder.mint
	// writes Completion.ExpectEffects as the record count of exactly these
	// views (an overflow REFUSES the unit rather than dropping a view, so the
	// equality holds by construction for everything the recorder can
	// produce), which makes any divergence an edit, a bad merge or a
	// hand-written spec — and a spec whose declared count does not match its
	// own contents grades its worker against a rule neither half states.
	specRecords := 0
	for _, e := range spec.Effects {
		if e.IsPresenceOnly() {
			continue
		}
		specRecords += e.RecordCount()
	}
	if specRecords != spec.Completion.ExpectEffects {
		return refusal(models.CategoryConsumerUnsupportedSpec, spec.Protocol,
			fmt.Sprintf("this test's spec declares %d expected effect record(s) but its effects list covers %d; the completion rule and the effects it is supposed to count disagree, so the window would be graded against a number the recording does not support", spec.Completion.ExpectEffects, specRecords),
			fmt.Sprintf("a spec whose completion.expectEffects matches its own %d effect record(s)", specRecords))
	}

	v := consumerVerdict{Pass: true}
	v.Rows, v.Categories = compareEffectLists(tc, spec, res)

	// THE PRESENCE-COUNT ASSERTION, AND EXACTLY WHAT IT CLAIMS.
	//
	// Presence stand-ins are invisible to everything else in this function.
	// compareEffectLists filters them out of both lanes, Recorder.onEffect
	// keeps them out of ExpectEffects and Gate.pendingLocked keeps them out of
	// ObservedEffects, so the count assertion below cannot see them on either
	// side. That left "the worker now writes the row twice" — a recorded
	// presence view against two observed ones — as a pass with no row, no
	// category and no named refusal, which rule 7 forbids. The count
	// assertion's own comment used to claim it covered this; it never did.
	//
	// OVER-PRODUCTION ONLY, AND THE ASYMMETRY IS THE POINT. An EXTRA presence
	// effect has no mock mapping to be missing from and no counterpart
	// anywhere else in the contract, so this is the only place it can ever be
	// seen. A MISSING one is the opposite: v1 has no projector for a database
	// write, so a healthy replay legitimately observes ZERO presence views
	// while the recording holds several, and asserting equality would redden
	// every consume-and-write test on a worker doing exactly what it recorded.
	// That half of the claim is carried by the sync path — an unconsumed
	// role=effect mock is non-demotable (missingEffectMockPromotes) — which is
	// the same delegation the vacuity guard above refuses to accept unchecked.
	expPresence := len(spec.Effects) - assertable
	obsPresence := 0
	for _, o := range res.Effects {
		if o.IsPresenceOnly() {
			obsPresence++
		}
	}
	if obsPresence > expPresence {
		key := models.EffectKeyPrefix + ".presence_count"
		v.Rows = append(v.Rows, models.DepResult{
			Name: models.EffectUnexpectedRowName(models.EffectView{Protocol: spec.Protocol, Op: "presence_count"}),
			Type: spec.Protocol,
			Meta: []models.DepMetaResult{{
				Normal:   false,
				Key:      key,
				Expected: strconv.Itoa(expPresence),
				Actual:   strconv.Itoa(obsPresence),
			}},
		})
		v.Categories = appendCategoryUnique(v.Categories, models.CategoryEffectUnexpected)
	}

	// THE COUNT ASSERTION. The per-effect rows above cover everything the
	// recording named by an assertable view; this covers everything it did
	// not. A worker that produces a fourth message where the recording has
	// three yields no effects[i] row of its own once the lanes are paired, and
	// without this line that over-production would pass. It is silent whenever
	// the counts agree, which is every healthy test: the gate's own completion
	// rule is the same arithmetic, so a window that closed with count_reached
	// closed BECAUSE they agreed.
	//
	// It is blind to presence stand-ins on both sides by construction — see
	// the presence-count assertion above, which is where that shape is judged.
	if res.ObservedEffects != res.ExpectEffects {
		key := models.EffectKeyPrefix + ".count"
		v.Rows = append(v.Rows, models.DepResult{
			Name: models.EffectUnexpectedRowName(models.EffectView{Protocol: spec.Protocol, Op: "count"}),
			Type: spec.Protocol,
			Meta: []models.DepMetaResult{{
				Normal:   false,
				Key:      key,
				Expected: strconv.Itoa(res.ExpectEffects),
				Actual:   strconv.Itoa(res.ObservedEffects),
			}},
		})
		if res.ObservedEffects > res.ExpectEffects {
			v.Categories = appendCategoryUnique(v.Categories, models.CategoryEffectUnexpected)
		} else {
			v.Categories = appendCategoryUnique(v.Categories, models.CategoryEffectMissing)
		}
	}

	// THE WINDOW MUST HAVE CLOSED FOR THE RIGHT REASON. Everything matching
	// is necessary and not sufficient: a backstop that fired means the
	// observation is INCOMPLETE, and passing on it would be passing on
	// whatever happened to arrive before we stopped looking.
	switch res.EndReason {
	case models.ConsumerEndReasonCountReached:
		// COUNT_REACHED NEEDS POSITIVE EVIDENCE THE APPLICATION RAN, and for
		// one shape of test it is the only thing that can supply it. A
		// consume-and-write-to-a-database worker records `effects: []` with
		// `expectEffects: 0`, so "observed >= expected" is true before the
		// application has done anything at all: a worker that crashed at boot,
		// joined the wrong group or never subscribed closes its window on the
		// count with nothing having happened, and every assertion above it is
		// vacuously satisfied. That is design §5's false-pass row 2.
		//
		// The gate applies the same rule from the other end (Gate.Complete
		// refuses to close on the count without evidence, so the backstop
		// fires and names trigger_not_delivered). This is the judge's own
		// check rather than trust in that: everything in the result is
		// supplied by the agent, and the two cross-checks above exist for
		// exactly the same reason.
		if !res.TriggerAccepted && res.ObservedEffects == 0 {
			v.Rows = append(v.Rows, endReasonRow(spec.Protocol, res,
				"the window closed on the completion count without the application having produced anything or taken the trigger, so there is no evidence it ever received the message"))
			v.Categories = appendCategoryUnique(v.Categories, models.CategoryConsumerTriggerNotDelivered)
		}
	case models.ConsumerEndReasonTimeout:
		v.Rows = append(v.Rows, endReasonRow(spec.Protocol, res,
			"the completion backstop fired before this test's effects were fully observed"))
		v.Categories = appendCategoryUnique(v.Categories, models.CategoryConsumerCompletionTimeout)
	case models.ConsumerEndReasonInternalError:
		v.Rows = append(v.Rows, endReasonRow(spec.Protocol, res,
			"the delivery window ended on an internal error: "+refusalDetail(res)))
		v.Categories = appendCategoryUnique(v.Categories, models.CategoryConsumerRunCancelled)
	default:
		v.Rows = append(v.Rows, endReasonRow(spec.Protocol, res,
			"the delivery window reported no end reason, so there is no evidence it ever closed cleanly"))
		v.Categories = appendCategoryUnique(v.Categories, models.CategoryConsumerUnsupportedAgent)
	}

	v.Pass = len(v.Categories) == 0
	v.Summary = summarise(res, v)
	return v
}

// refusalDetail returns the gate's own words for a refusal, or a neutral
// stand-in so a row never renders an empty Actual.
func refusalDetail(res *models.ConsumerResult) string {
	if res != nil && strings.TrimSpace(res.RefusalDetail) != "" {
		return res.RefusalDetail
	}
	return "no further detail was reported"
}

func triggerTarget(spec *models.ConsumerSpec) string {
	if t := strings.TrimSpace(spec.Trigger.Target); t != "" {
		return t
	}
	return "the recorded source"
}

// refusal builds the single-row verdict for something this version declines to
// judge. Rule 7: named, FAILED, never silent.
func refusal(category models.FailureCategory, protocol, detail, expected string) consumerVerdict {
	return consumerVerdict{
		Pass: false,
		Rows: []models.DepResult{{
			Name: models.EffectUnexpectedRowName(models.EffectView{Protocol: protocol, Op: "refused"}),
			Type: protocol,
			Meta: []models.DepMetaResult{{
				Normal:   false,
				Key:      models.EffectKeyPrefix + ".refusal",
				Expected: expected,
				Actual:   detail,
			}},
		}},
		Categories: []models.FailureCategory{category},
		Summary:    string(category) + ": " + detail,
	}
}

func endReasonRow(protocol string, res *models.ConsumerResult, detail string) models.DepResult {
	return models.DepResult{
		Name: models.EffectUnexpectedRowName(models.EffectView{Protocol: protocol, Op: "window"}),
		Type: protocol,
		Meta: []models.DepMetaResult{{
			Normal:   false,
			Key:      models.EffectKeyPrefix + ".end_reason",
			Expected: string(models.ConsumerEndReasonCountReached),
			Actual:   string(res.EndReason) + " (" + detail + ")",
		}},
	}
}

func summarise(res *models.ConsumerResult, v consumerVerdict) string {
	return fmt.Sprintf("%d of %d expected effects observed, ended %s, %d finding(s)",
		res.ObservedEffects, res.ExpectEffects, res.EndReason, len(models.MissingDepNames(v.Rows)))
}

// ---------------------------------------------------------------------------
// The comparison itself.
// ---------------------------------------------------------------------------

// laneKey identifies an independently-ordered stream of effects. Two effects
// in the same lane are compared IN ORDER; two effects in different lanes are
// not ordered against each other at all.
//
// The lane coordinate is read out of EffectView.Coords under the key the
// RECORDING names (ConsumerSpec.OrderBy) — never a key this package chooses.
// That is what keeps a Kafka partition, a Pulsar subscription and an SQS
// message group out of OSS while still letting each of them order correctly:
// the value is compared for string equality and is otherwise meaningless here.
type laneKey struct {
	protocol string
	target   string
	lane     string
}

func laneOf(v models.EffectView, orderBy string) laneKey {
	k := laneKey{protocol: v.Protocol, target: v.Target}
	if orderBy != "" {
		k.lane = v.Coords[orderBy]
	}
	return k
}

// compareEffectLists pairs the recorded effects with the observed ones and
// turns every disagreement into a row.
//
// PAIRING, IN TWO PASSES.
//
// Pass 1 is positional within a lane: recorded effect i of a lane is compared
// with observed effect i of the same lane. That is what makes "the worker
// still produces the right things, but in the wrong order" a finding rather
// than an invisible reshuffle.
//
// Pass 2 reconciles what pass 1 could not place. A recorded effect with no
// observed counterpart and an observed effect with no recorded counterpart
// are, on their own, "missing" and "unexpected" — two rows, two categories,
// and a human left to notice that they are the same message. When their
// payloads are identical they are almost certainly ONE event that went
// somewhere else: a routing regression, whose remedy is different from both
// of the other two. Pass 2 pairs those and reports them as an identity change.
// It only ever merges two failures into one better-named failure; it can never
// turn a failure into a pass.
func compareEffectLists(tc *models.TestCase, spec *models.ConsumerSpec, res *models.ConsumerResult) ([]models.DepResult, []models.FailureCategory) {
	// Presence stand-ins are NOT counted by the completion rule and are never
	// compared: nothing decoded them, so there is nothing to compare. Their
	// claim ("this write happened") belongs to the sync path's deps[i] rows,
	// which the same test also carries.
	//
	// THEY ARE FILTERED FROM BOTH SIDES, and the symmetry is the whole point.
	// Filtering only the observed side would leave every recorded presence
	// view with no candidate in its lane, so it would fall to `missing` on a
	// perfectly healthy worker — and worse, it would shift the positional
	// pairing of every real effect behind it in that lane, manufacturing a
	// spurious identity change too. Nothing on the replay path calls
	// ObserveEffect for a database write in v1, so a recorded presence view
	// could not be paired even in principle. The record side agrees:
	// Recorder.onEffect excludes presence views from ExpectEffects, and
	// Gate.pendingLocked excludes them from the observed count. All three
	// doc comments say the same sentence deliberately, so a future divergence
	// shows up in a diff.
	observed := make([]models.EffectView, 0, len(res.Effects))
	for _, o := range res.Effects {
		if o.IsPresenceOnly() {
			continue
		}
		observed = append(observed, o)
	}
	expected := make([]int, 0, len(spec.Effects))
	for i, e := range spec.Effects {
		if e.IsPresenceOnly() {
			continue
		}
		expected = append(expected, i)
	}

	expLanes := map[laneKey][]int{}
	var expOrder []laneKey
	for _, i := range expected {
		e := spec.Effects[i]
		k := laneOf(e, spec.OrderBy)
		if _, seen := expLanes[k]; !seen {
			expOrder = append(expOrder, k)
		}
		expLanes[k] = append(expLanes[k], i)
	}
	obsLanes := map[laneKey][]int{}
	for i, o := range observed {
		k := laneOf(o, spec.OrderBy)
		obsLanes[k] = append(obsLanes[k], i)
	}

	type pairing struct{ exp, obs int }
	var pairs []pairing
	var missing []int
	obsClaimed := make([]bool, len(observed))

	// Pass 1 — positional within each lane, in recorded lane order so the
	// output is deterministic.
	for _, k := range expOrder {
		e, o := expLanes[k], obsLanes[k]
		n := min(len(e), len(o))
		for i := 0; i < n; i++ {
			pairs = append(pairs, pairing{exp: e[i], obs: o[i]})
			obsClaimed[o[i]] = true
		}
		missing = append(missing, e[n:]...)
	}
	var extra []int
	for i := range observed {
		if !obsClaimed[i] {
			extra = append(extra, i)
		}
	}
	sort.Ints(missing)

	// Pass 2 — a missing effect and an extra effect carrying the SAME payload
	// are one event that was routed elsewhere.
	if len(missing) > 0 && len(extra) > 0 {
		stillMissing := missing[:0:0]
		used := map[int]bool{}
		for _, mi := range missing {
			matchedAt := -1
			for _, xi := range extra {
				if used[xi] {
					continue
				}
				if sameEffectPayload(spec.Effects[mi], observed[xi]) {
					matchedAt = xi
					break
				}
			}
			if matchedAt < 0 {
				stillMissing = append(stillMissing, mi)
				continue
			}
			used[matchedAt] = true
			pairs = append(pairs, pairing{exp: mi, obs: matchedAt})
		}
		missing = stillMissing
		stillExtra := extra[:0:0]
		for _, xi := range extra {
			if !used[xi] {
				stillExtra = append(stillExtra, xi)
			}
		}
		extra = stillExtra
	}

	sort.Slice(pairs, func(i, j int) bool { return pairs[i].exp < pairs[j].exp })

	var rows []models.DepResult
	var cats []models.FailureCategory

	for _, p := range pairs {
		row, found := comparePair(tc, p.exp, spec.Effects[p.exp], observed[p.obs])
		if len(found) == 0 {
			continue
		}
		rows = append(rows, row)
		for _, c := range found {
			cats = appendCategoryUnique(cats, c)
		}
	}
	for _, mi := range missing {
		e := spec.Effects[mi]
		rows = append(rows, models.DepResult{
			Name: models.EffectRowNameFor(mi, e),
			Type: e.Protocol,
			Meta: []models.DepMetaResult{{
				Normal:   false,
				Key:      models.EffectKey(mi, models.EffectKeyPresence),
				Expected: models.EffectPresenceObserved,
				Actual:   models.EffectPresenceMissing,
			}},
		})
		cats = appendCategoryUnique(cats, models.CategoryEffectMissing)
	}
	for _, xi := range extra {
		o := observed[xi]
		rows = append(rows, models.DepResult{
			Name: models.EffectUnexpectedRowName(o),
			Type: o.Protocol,
			Meta: []models.DepMetaResult{{
				Normal:   false,
				Key:      models.EffectKeyPrefix + "." + models.EffectUnexpectedIndex + "." + models.EffectKeyUnexpected,
				Expected: models.EffectPresenceMissing,
				Actual:   models.EffectPresenceObserved,
			}},
		})
		cats = appendCategoryUnique(cats, models.CategoryEffectUnexpected)
	}
	return rows, cats
}

// comparePair judges one recorded effect against the effect that took its
// place. It returns no categories when they agree.
//
// It can return MORE THAN ONE: headers and body are independent assertions
// with different remedies, and a worker that dropped a routing header while
// also changing a payload field has two regressions, not one. Collapsing them
// would make the report name whichever one this function happened to check
// first.
func comparePair(tc *models.TestCase, index int, exp, obs models.EffectView) (models.DepResult, []models.FailureCategory) {
	row := models.DepResult{Name: models.EffectRowNameFor(index, exp), Type: exp.Protocol}

	if !models.SameEffectIdentity(exp, obs) {
		row.Meta = []models.DepMetaResult{{
			Normal:   false,
			Key:      models.EffectKey(index, models.EffectKeyIdentity),
			Expected: models.EffectIdentity(exp),
			Actual:   models.EffectIdentity(obs),
		}}
		return row, []models.FailureCategory{models.CategoryEffectTargetChanged}
	}

	// AN UNIMPLEMENTED ASSERT MODE IS REFUSED, NOT DEFAULTED. `assert` is part
	// of a published on-disk schema and v1 implements exactly one mode; a
	// spec written by a newer build must not be silently graded under the
	// only rule this build happens to know.
	if mode := exp.AssertMode(); mode != models.AssertFull {
		row.Meta = []models.DepMetaResult{{
			Normal:   false,
			Key:      models.EffectKey(index, "assert"),
			Expected: string(models.AssertFull),
			Actual:   string(mode) + " (this build implements no such assert mode)",
		}}
		return row, []models.FailureCategory{models.CategoryConsumerUnsupportedSpec}
	}

	// There is deliberately no presence-only branch here. compareEffectLists
	// removes presence stand-ins from BOTH the expected and the observed lane
	// before pairing, so neither side of this comparison can be one, and a
	// branch that cannot be reached is a claim about behaviour that nothing
	// enforces.
	//
	// OPAQUE NEVER PASSES. A payload the projector declined to model, compared
	// against another payload the projector declined to model, is a misparse
	// compared with the same misparse: it agrees for the wrong reason. That is
	// the one shape of false pass that no amount of field diffing can catch,
	// so it is refused on either side.
	if exp.IsOpaque() || obs.IsOpaque() {
		actual := string(obs.Decoded)
		if obs.IsOpaque() && exp.IsOpaque() {
			actual = "opaque on both the recorded and the observed side"
		} else if exp.IsOpaque() {
			actual = "opaque in the recording"
		}
		row.Meta = []models.DepMetaResult{{
			Normal:   false,
			Key:      models.EffectKey(index, models.EffectKeyOpaque),
			Expected: models.EffectDecodedConfidentValue,
			Actual:   actual,
		}}
		return row, []models.FailureCategory{models.CategoryConsumerOpaqueEffectBody}
	}

	var cats []models.FailureCategory
	// HEADERS ARE ASSERTED. EffectView.Headers is written by the projector,
	// persisted in the spec, shown in the design's own on-disk example and
	// rendered in the report — and until this existed nothing compared it, so
	// a dropped tenant/routing/trace header was a green test with nothing in
	// the report, the JUnit output or --format json to hint at it. That is the
	// silent pass rule 7 forbids. A header that genuinely differs every run (a
	// fresh traceparent) is silenced the way a payload field is: by pasting
	// the reported effects.<i>.headers.<name> path into that test's
	// spec.assertions.noise.
	headerMetas := diffEffectHeaders(tc, index, exp, obs)
	if len(headerMetas) > 0 {
		cats = append(cats, models.CategoryEffectHeadersChanged)
	}
	bodyMetas := diffEffectBody(tc, index, exp, obs)
	if len(bodyMetas) > 0 {
		cats = append(cats, models.CategoryEffectBodyChanged)
	}
	if len(cats) == 0 {
		return row, nil
	}
	row.Meta = append(headerMetas, bodyMetas...)
	return row, cats
}

// sameEffectPayload reports whether two views carry the same payload, used
// only to recognise a routed-elsewhere effect in pass 2. Bytes-equal is
// deliberately the whole test: pass 2 exists to give a BETTER NAME to two
// failures that are really one, and a fuzzy notion of "same payload" here
// would start merging genuinely unrelated events under a single row.
func sameEffectPayload(a, b models.EffectView) bool {
	// An opaque payload is a payload nothing decoded, so "the same payload"
	// is not a claim that can be made about it. Presence stand-ins never
	// reach here at all: compareEffectLists filters them out of both lanes.
	if a.IsOpaque() || b.IsOpaque() {
		return false
	}
	if strings.TrimSpace(a.Body) == "" || strings.TrimSpace(b.Body) == "" {
		return false
	}
	if a.Body == b.Body {
		return true
	}
	// Two JSON encodings of the same document (key order, whitespace) are the
	// same payload. Anything that does not parse falls back to bytes.
	fa, oka := flattenBody(a.Body)
	fb, okb := flattenBody(b.Body)
	if !oka || !okb || len(fa) != len(fb) {
		return false
	}
	for k, v := range fa {
		if fb[k] != v {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// The payload differ.
//
// This is the part that MUST NOT be shared with the mock matcher, so it is
// written here from scratch and kept as small as it can be. It has exactly two
// jobs: turn a JSON document into dotted paths, and report the paths on which
// two documents disagree.
//
// NOTHING IS FILTERED BY FIELD NAME. The matcher drops keys called timestamp,
// host, sequence, epoch and createTime at every nesting depth before it
// compares, because those are the fields that legitimately drift between a
// recording and a replay of the same OUTGOING CALL. Inside a message payload
// those are ordinary business fields — `event.timestamp` is very often the
// only thing that distinguishes two events — and silencing them by name is
// exactly how a real regression becomes a green test. The only thing that
// silences a path here is the user writing it into that test's
// spec.assertions.noise, which is per-test, explicit, and reviewable in a diff.
// ---------------------------------------------------------------------------

// diffEffectBody compares two decoded payloads and returns one failed meta per
// disagreeing path. An empty return means they agree (or every disagreement
// was declared noise).
func diffEffectBody(tc *models.TestCase, index int, exp, obs models.EffectView) []models.DepMetaResult {
	noise := noiseKeys(tc)

	// Field-level comparison needs BOTH sides to be JSON. A body the
	// recording declared JSON that no longer parses is itself the finding, so
	// it falls through to the whole-body comparison below rather than being
	// silently skipped.
	if exp.BodyType == models.JSON || obs.BodyType == models.JSON {
		expFlat, expOK := flattenBody(exp.Body)
		obsFlat, obsOK := flattenBody(obs.Body)
		if expOK && obsOK {
			return diffFlat(models.EffectBodyKeyPrefix(index), expFlat, obsFlat, noise)
		}
	}

	if exp.Body == obs.Body {
		return nil
	}
	key := models.EffectKey(index, models.EffectKeyBody)
	if noise[key] {
		return nil
	}
	return []models.DepMetaResult{{Normal: false, Key: key, Expected: exp.Body, Actual: obs.Body}}
}

// diffEffectHeaders compares two views' message headers and returns one failed
// meta per disagreeing header name.
//
// Header names are compared EXACTLY, and a header present on only one side
// renders as <absent> — dropping a header and blanking it are different
// regressions. Nothing is filtered by name here for the same reason nothing is
// filtered by name in the body differ: a `host` or `timestamp` header is an
// ordinary header, and silencing it by name is how a real regression becomes a
// green test.
func diffEffectHeaders(tc *models.TestCase, index int, exp, obs models.EffectView) []models.DepMetaResult {
	if len(exp.Headers) == 0 && len(obs.Headers) == 0 {
		return nil
	}
	return diffFlat(models.EffectHeaderKeyPrefix(index), exp.Headers, obs.Headers, noiseKeys(tc))
}

// diffFlat reports the paths on which two flattened documents disagree, in
// sorted path order so a report diffed between two runs does not shuffle.
// prefix is prepended to every reported path, which is what lets one differ
// serve both the body (effects.<i>.body.) and the headers
// (effects.<i>.headers.) namespaces.
func diffFlat(prefix string, exp, obs map[string]string, noise map[string]bool) []models.DepMetaResult {
	paths := make([]string, 0, len(exp)+len(obs))
	seen := make(map[string]bool, len(exp)+len(obs))
	for p := range exp {
		if !seen[p] {
			seen[p] = true
			paths = append(paths, p)
		}
	}
	for p := range obs {
		if !seen[p] {
			seen[p] = true
			paths = append(paths, p)
		}
	}
	sort.Strings(paths)

	var out []models.DepMetaResult
	for _, p := range paths {
		e, hasE := exp[p]
		o, hasO := obs[p]
		if hasE && hasO && e == o {
			continue
		}
		key := prefix + p
		if noise[key] {
			continue
		}
		if !hasE {
			e = absentValue
		}
		if !hasO {
			o = absentValue
		}
		out = append(out, models.DepMetaResult{Normal: false, Key: key, Expected: e, Actual: o})
	}
	return out
}

// absentValue is the rendered stand-in for a path that exists on only one
// side. It is a distinct token rather than an empty string so that "the field
// is gone" and "the field is now empty" — two different regressions — do not
// render identically.
const absentValue = "<absent>"

// flattenBody parses a JSON document into dotted paths. It reports false for
// anything that is not valid JSON, which the caller turns into a whole-body
// comparison rather than a silent skip.
func flattenBody(body string) (map[string]string, bool) {
	trimmed := strings.TrimSpace(body)
	if trimmed == "" {
		return nil, false
	}
	var doc any
	if err := json.Unmarshal([]byte(trimmed), &doc); err != nil {
		return nil, false
	}
	out := map[string]string{}
	flattenJSON("", doc, out)
	return out, true
}

// flattenJSON walks a decoded JSON value into path -> rendered-scalar pairs.
//
// Arrays are indexed positionally (items.0.sku). That makes element ORDER part
// of the assertion, which is correct for a message payload: `[a, b]` and
// `[b, a]` are different messages, and a set-wise comparison here would hide a
// reordering regression in a list of line items.
//
// An empty object or array records its own path with an empty-container token
// rather than vanishing, so "the list became empty" is a diff instead of an
// absence that only shows up as the disappearance of its children.
func flattenJSON(path string, v any, out map[string]string) {
	switch t := v.(type) {
	case map[string]any:
		if len(t) == 0 {
			out[path] = "{}"
			return
		}
		for k, child := range t {
			flattenJSON(joinPath(path, k), child, out)
		}
	case []any:
		if len(t) == 0 {
			out[path] = "[]"
			return
		}
		for i, child := range t {
			flattenJSON(joinPath(path, strconv.Itoa(i)), child, out)
		}
	case nil:
		out[path] = "null"
	case bool:
		out[path] = strconv.FormatBool(t)
	case float64:
		out[path] = strconv.FormatFloat(t, 'f', -1, 64)
	case string:
		out[path] = t
	default:
		out[path] = fmt.Sprint(t)
	}
}

func joinPath(prefix, key string) string {
	if prefix == "" {
		return key
	}
	return prefix + "." + key
}

// noiseKeys is the set of assertion paths the user declared non-deterministic
// for THIS test.
//
// PER-TEST, NEVER PER-TEST-SET. The set-wide NoiseConfig would widen a
// silence to every test in the set, which is precisely the "one flaky field
// silences a real regression everywhere" failure this contract exists to
// remove. Paths are matched EXACTLY and are the same strings the report
// prints, so a reported path pastes straight back into
// spec.assertions.noise with no translation.
//
// The VALUES of models.Noise (regex / value-prefix filters) are deliberately
// ignored here: a consumer payload field is either asserted or it is not, and
// a half-silenced field is a claim nobody can read off the file.
func noiseKeys(tc *models.TestCase) map[string]bool {
	if tc == nil || len(tc.Noise) == 0 {
		return nil
	}
	out := make(map[string]bool, len(tc.Noise))
	for k := range tc.Noise {
		out[strings.TrimSpace(k)] = true
	}
	return out
}

// ---------------------------------------------------------------------------
// Replay-loop helpers that are Kind-aware but not part of the judge.
// ---------------------------------------------------------------------------

// backdateFor returns the timestamp that anchors generated TLS certificates
// for a test set (models.OutgoingOptions.Backdate).
//
// WHAT A ZERO BACKDATE ACTUALLY DOES, because the obvious story is wrong and
// design §4 P8 states the wrong one. It does NOT produce a 1970 certificate:
// pkg/agent/proxy/tls/ca.go's CertForClient already substitutes time.Now() for
// a zero backdate before subtracting a year, so zero has always meant "valid
// from a year ago" and has always been safe. The §9 companion requirement to
// "refuse to generate a certificate from a zero Backdate" is deliberately NOT
// implemented for that reason: it would turn a working default into a hard
// failure for every recording whose first test case has no timestamp.
//
// WHAT THIS FUNCTION IS ACTUALLY FOR. The call sites read
// `testCases[0].HTTPReq.Timestamp` literally, which is the zero time for any
// test case that is not HTTP — so a consumer set anchored its certificates on
// wall-clock-now rather than on the recording, and a certificate generated
// today for traffic recorded weeks ago has no relationship to the recorded
// exchange it is standing in for. EarliestTimestamp is Kind-aware and falls
// back through the test case's own window to its creation time, so a consumer
// set anchors on the trigger that actually happened.
//
// THIS IS THE ONE PLACE AN EXISTING KIND'S BEHAVIOUR CHANGES, and the change
// is deliberate: an HTTP set whose FIRST test case has a zero HTTPReq.Timestamp
// (an imported test, a curl-generated one, an older recording) used to yield
// the zero time and now yields that case's Created — or, failing that, the
// first later test case that has a usable timestamp. Both are strictly closer
// to the recording than "now minus a year". A set with no usable timestamp
// anywhere still returns the zero time, and ca.go still handles it exactly as
// it always has. TestBackdateFor pins every one of these rows.
func backdateFor(testCases []*models.TestCase) time.Time {
	for _, tc := range testCases {
		if at := tc.EarliestTimestamp(); !at.IsZero() {
			return at
		}
	}
	return time.Time{}
}

// containsConsumerTest reports whether a test set contains at least one
// Kind: Consumer test case.
func containsConsumerTest(testCases []*models.TestCase) bool {
	for _, tc := range testCases {
		if tc != nil && tc.Kind == models.CONSUMER {
			return true
		}
	}
	return false
}

// refuseUnmappedConsumerSet refuses a test set that contains a consumer test
// but is not replaying through per-test mock mappings that are actually ARMED
// AT THE AGENT.
//
// WHY IT IS A REFUSAL AND NOT A DOWNGRADE. Without an armed mapping the agent
// filters mocks by TIMESTAMP WINDOW, which cannot arm exactly one trigger: the
// whole window's worth of recorded poll responses is resident at once and the
// worker — which polls continuously — drains whichever it is handed first.
// Every test then asserts a message that belongs to some other test, and the
// resulting failures point at the worker rather than at the missing mapping.
// Running anyway would produce a red suite that is a lie, or (worse) a green
// one for the same reason.
//
// THE PREDICATE IS THE SAME THREE CONJUNCTS depAssertionValid USES, and it has
// to be, because "a mappings.yaml was loaded" is not "the per-test mapping is
// armed":
//
//	useMappingBased   determineMockingStrategy found a usable mappings.yaml.
//	isMappingEnabled  test.disableMapping is not set.
//	r.instrument      SendMockFilterParamsToAgent RETURNS EARLY when this is
//	                  false, so nothing is ever armed and the agent serves from
//	                  the whole pool — exactly the state this refusal exists to
//	                  prevent, reached with a mappings.yaml sitting on disk.
//
// r.instrument is `config.Command != ""`, and today the CLI happens to reject
// the combination that would reach it (an empty Command is accepted only for
// `test` with a base path, which forces CommandType to Empty). That is a
// validation two layers away in another package, and NewReplayer is called
// directly by embedders where none of it runs, so the conjunct is checked
// here.
//
// The set fails with models.CategoryConsumerMappingsRequired named in the
// error so the exit code, the report and an agent reading the log all agree.
func (r *Replayer) refuseUnmappedConsumerSet(testSetID string, testCases []*models.TestCase, useMappingBased, isMappingEnabled bool) error {
	if !containsConsumerTest(testCases) {
		return nil
	}
	armed := useMappingBased && isMappingEnabled && r.instrument
	if armed {
		return nil
	}
	why := "without mappings the agent filters mocks by timestamp window, which cannot arm exactly one trigger: a continuously polling worker drains whichever recorded message it is handed first, so every test would assert another test's message"
	nextStep := "re-record the test set, or regenerate its mappings.yaml with --update-test-mapping; if test.disableMapping is set in keploy.yml, unset it for this set"
	detail := "has no usable per-test mock mappings"
	switch {
	case !r.instrument:
		detail = "cannot arm its per-test mock mappings: this run has no application command, so the agent is never sent a mock filter and serves every test from the whole recorded pool"
		why = "SendMockFilterParamsToAgent returns early without an application command, so the per-test mapping is never armed however complete mappings.yaml is; a continuously polling worker then drains whichever recorded message it is handed first"
		nextStep = "run the consumer test set with the application command keploy should start (`keploy test -c \"<command>\"`), so the per-test mock mapping can be armed at the agent"
	case !isMappingEnabled:
		detail = "has per-test mock mappings that are disabled by configuration"
		nextStep = "unset test.disableMapping in keploy.yml for this test set; consumer tests cannot be replayed with timestamp-window filtering"
	}
	err := fmt.Errorf("%s: test set %s contains consumer test cases but %s", models.CategoryConsumerMappingsRequired, testSetID, detail)
	r.logger.Error("refusing to replay a consumer test set without armed mock mappings",
		zap.String("testset", testSetID),
		zap.String("category", string(models.CategoryConsumerMappingsRequired)),
		zap.Bool("mappings_loaded", useMappingBased),
		zap.Bool("mapping_enabled", isMappingEnabled),
		zap.Bool("instrumented", r.instrument),
		zap.String("why", why),
		zap.String("next_step", nextStep))
	return err
}

// resetConsumerGate returns the consumer delivery gate to its default-closed
// boot phase at the START of a test set.
//
// It is what keeps --keep-app-alive from leaking one set's state into the
// next: one application process spans every set, so a gate left armed by an
// interrupted set, or an effect adopted across the boundary, would land on the
// next set's first test.
//
// GATED ON THE SET ACTUALLY CONTAINING A CONSUMER TEST. This used to live in
// Hooks.BeforeTestSetReplay, which has no test cases in scope, so every
// `keploy test` run — a pure HTTP suite with no consumer test anywhere
// included — issued an extra POST to the agent per test set. Harmless, but an
// unnecessary new call on a path this slice is supposed to leave untouched,
// and impossible to write a test for from the hook. Here containsConsumerTest
// can short-circuit it exactly the way drainConsumerGate does.
//
// It is quiet by design. A trailing count from the PREVIOUS set is a set too
// late to blame anyone for, and drainConsumerGate has already reported it at
// the end of that set; a failed call is reported by the arm that follows,
// where it can actually fail a test.
func (r *Replayer) resetConsumerGate(ctx context.Context, testSetID string, testCases []*models.TestCase) {
	if !containsConsumerTest(testCases) {
		return
	}
	ci, ok := r.instrumentation.(ConsumerInstrumentation)
	if !ok {
		return
	}
	if _, err := ci.ResetConsumerGate(ctx, testSetID); err != nil {
		r.logger.Debug("failed to reset the consumer delivery gate for this test set",
			zap.String("testset", testSetID), zap.Error(err))
	}
}

// drainConsumerGate closes the consumer delivery gate at the END of a test set
// and reports whether anything was left unattributed. It returns true when the
// set must be failed.
//
// WHY THE END AND NOT ONLY THE START. The gate is also reset at the start of a
// set (resetConsumerGate). That reset can never report the LAST set's trailing
// effects, because nothing runs after it. Within a set an effect that lands
// after the grace fails the NEXT test as an extra — loud, and correct at suite
// level even though the blame is one window out — but the last test of the
// last set has no next test, so the one regression the mandatory grace drain
// exists to catch (the worker emitting an N+1 message) would produce no row,
// no log line and no non-zero exit at all.
//
// A NON-ZERO TRAILING COUNT FAILS THE SET; A FAILED CALL DOES NOT. They are
// different things and used to be the same return value, which meant a 501
// from an agent that predates the route, a 500, or a dropped connection was
// reported to the user as "the worker produced after the last test of this
// set" with a remedy telling them to go and look at their worker's emissions.
// That is the exact confusion Gate.abortArm's own comment says to avoid:
// keploy's infrastructure failure dressed up as the application's regression.
//
// A failed set here is a FAILED SET STATUS, which is what turns the run
// non-zero. It deliberately does not synthesise a test result: there is no
// test to attribute the effect to — that is the entire finding.
func (r *Replayer) drainConsumerGate(ctx context.Context, testSetID string, testCases []*models.TestCase) bool {
	if !containsConsumerTest(testCases) {
		return false
	}
	ci, ok := r.instrumentation.(ConsumerInstrumentation)
	if !ok {
		return false
	}
	trailing, err := ci.ResetConsumerGate(ctx, testSetID)
	if err != nil {
		r.logger.Error("could not drain the consumer delivery gate at the end of this test set",
			zap.String("testset", testSetID),
			zap.Error(err),
			zap.String("why", "the gate is what would report an effect produced after the last test of the set closed its window; with this call failing, such an over-production goes unreported for this set"),
			zap.String("next_step", "this is a keploy-side failure, not a finding about the worker: check that the agent is reachable and new enough to serve /agent/consumer/reset"))
		return false
	}
	if trailing == 0 {
		return false
	}
	r.logger.Error("the worker produced after the last test of this set closed its window",
		zap.String("testset", testSetID),
		zap.Int("trailing_effect_records", trailing),
		zap.String("why", "an effect that arrives after a test's grace drain is attributed to the NEXT test as an extra, and the last test of a set has no next test; reporting it here is the only place an over-production at the very end of a run can be seen"),
		zap.String("next_step", "check whether the worker emits more messages than the recording says it does — an N+1 emission is the regression the grace drain exists to catch"))
	return true
}

// refuseRepeatPassOverConsumerSet refuses --retryPassing and --must-pass over
// a test set containing consumer tests.
//
// A REPEAT PASS IS NOT A REPEAT. Both flags re-run tests inside the SAME
// application process, and a consumer's state does not rewind between
// attempts: its fetch position has moved past the message, its idempotent
// producer sequence has advanced, and the recorded acknowledgement it would be
// served carries the first attempt's offsets. The second attempt therefore
// measures something different from the first, and the flags exist precisely
// to decide whether two measurements of the SAME thing agree. A "flake"
// reported by such a comparison is an artefact of the comparison.
//
// Both flags are refused in one place, deliberately: they are two spellings of
// the same request (--must-pass sets MaxFlakyChecks), and refusing one while
// silently honouring the other is how this rule would rot.
func (r *Replayer) refuseRepeatPassOverConsumerSet(testSetID string, testCases []*models.TestCase) error {
	if !containsConsumerTest(testCases) {
		return nil
	}
	var flags []string
	if r.config.RetryPassing {
		flags = append(flags, "--retryPassing")
	}
	if r.config.Test.MaxFlakyChecks > 1 {
		flags = append(flags, fmt.Sprintf("--must-pass (maxFlakyChecks=%d)", r.config.Test.MaxFlakyChecks))
	}
	if len(flags) == 0 {
		return nil
	}
	err := fmt.Errorf("%s: %s cannot be used over test set %s, which contains consumer test cases", models.CategoryConsumerRepeatPassUnsupported, strings.Join(flags, " and "), testSetID)
	r.logger.Error("refusing a repeat pass over a consumer test set",
		zap.String("testset", testSetID),
		zap.String("category", string(models.CategoryConsumerRepeatPassUnsupported)),
		zap.Strings("flags", flags),
		zap.String("why", "these flags re-run tests inside the same application process, and a consumer's fetch position and producer sequence do not rewind between attempts, so the second attempt does not measure what the first one did"),
		zap.String("next_step", "drop the flag for this set; to test for genuine flakiness, run keploy test again from a fresh application process"))
	return err
}
