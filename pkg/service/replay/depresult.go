package replay

import (
	"net/url"
	"slices"
	"sort"
	"strings"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// mockDisplayInfo carries the human-facing identity of a loaded mock, built
// once per test set from the mock registry and keyed by mock name. Hoisted out
// of RunTestSet (where it used to be a function-local `mockInfo`) so the
// DepResult writer below can be unit-tested without a full replay.
//
// Every field is best-effort: r.mockDB may be nil and GetUnFilteredMocks
// errors are swallowed at the build site, so a lookup miss is normal and
// consumers must degrade rather than assume a hit.
type mockDisplayInfo struct {
	summary  string
	protocol string
	target   string
	// kind is the mock's models.Kind, kept as a typed value alongside the
	// display-oriented `protocol` string so the non-demotion rule can compare
	// two mocks' families without re-parsing a rendered label.
	kind models.Kind
	// role is the consumer contract's models.MetaKeyRole value: trigger,
	// effect, or empty for an ordinary mock. It is what lets the CONSUMER
	// non-demotion apply to the mocks it is ABOUT (the effects) rather than to
	// every per-test mock the test happened to map. Empty for every mock in
	// this repository today: nothing in OSS stamps role metadata.
	role string
}

// mockTargetFromSpec derives a human-meaningful destination for a mock.
//
// Neither models.MockEntry (the mapping / expected side) nor models.MockState
// (the consumed side) records a destination, so the loaded *models.Mock is the
// only place one exists. Priority order, MOST DISCRIMINATING first — the row
// name has to tell five outgoing calls to the same service apart:
//
//  1. HTTP: method + host + path. A bare host collapses every call to one
//     service into indistinguishable rows; `GET api.internal:8080/orders` is
//     the string a human acts on and an agent keys off.
//  2. Metadata["destAddr"] ("host:port", written by the MySQL and Postgres
//     recorders) PLUS the mock's operation token when it has one, so the two
//     protocols that route every call through a single address still produce
//     distinct rows ("mysql:3306 COM_QUERY").
//  3. The protocol-generic summary (models.MockSummaryFromSpec), which is what
//     the report UI already shows for MatchedCalls. It beats the bare
//     `operation` token below because for Mongo/MySQL/Postgres it is
//     "<protocol> <operation>" rather than a naked verb.
//  4. The operation token on its own — the only identifying thing some v1
//     mocks carry.
//
// Returns "" when none is available; DepRowName renders correctly without it.
func mockTargetFromSpec(mock *models.Mock) string {
	if mock == nil {
		return ""
	}
	if t := httpTargetFromReq(mock.Spec.HTTPReq); t != "" {
		return t
	}
	md := mock.Spec.Metadata
	op := mockOperationToken(md)
	if v := strings.TrimSpace(md["destAddr"]); v != "" {
		if op != "" {
			return v + " " + op
		}
		return v
	}
	// MockSummaryFromSpec falls back to the bare Kind when a mock carries
	// nothing identifying. That is already the row's `type` token, so using it
	// as the target too would render "deps[0] redis Redis (presence)".
	if s := strings.TrimSpace(models.MockSummaryFromSpec(mock)); s != "" && s != string(mock.Kind) {
		return s
	}
	return op
}

// mockOperationToken returns the most specific operation verb a mock's
// metadata carries.
//
// "operation" is the key every v1 recorder writes and the only one
// models.MockSummaryFromSpec reads. The MySQL v2 recorder writes
// "requestOperation"/"responseOperation" instead (see
// integrations/mysql/recorder/record_v2.go), which is why MockSummaryFromSpec
// returns a bare "MySQL" for those mocks — and why every MySQL dependency row
// would otherwise collapse to the shared "mysql:3306" address with only the
// index to tell them apart. The response side is deliberately not consulted:
// it names the server's reply status ("OK"), not the call.
func mockOperationToken(md map[string]string) string {
	for _, key := range []string{"operation", "requestOperation"} {
		if v := strings.TrimSpace(md[key]); v != "" {
			return v
		}
	}
	return ""
}

// httpTargetFromReq renders an outgoing HTTP call as "<METHOD> <host><path>".
// The scheme is dropped (it is implied by the port and adds noise to a name
// that is already long) but the path is not: the path is the only thing that
// distinguishes two calls to the same service.
func httpTargetFromReq(req *models.HTTPReq) string {
	if req == nil {
		return ""
	}
	method := strings.TrimSpace(string(req.Method))
	raw := strings.TrimSpace(req.URL)
	target := raw
	if u, err := url.Parse(raw); err == nil && u.Host != "" {
		target = u.Host + u.EscapedPath()
	}
	switch {
	case method == "" && target == "":
		return ""
	case method == "":
		return target
	case target == "":
		return method
	}
	return method + " " + target
}

// eligibleExpectedEntries is THE SINGLE DEFINITION of "which of the
// dependencies the recording mapped to this test may the per-test assertion
// look at". Both readers of that question call it, and they must, because the
// two of them now decide different halves of one user-visible verdict:
//
//   - RunTestSet derives filteredExpectedNames from it (the list isMockSubset
//     compares against, i.e. the mockSetMismatch verdict) AND noEligibleDeps
//     from it (the latched WARN that tells the user WHY nothing was asserted);
//   - buildDepResults iterates it, deciding the persisted deps_checked bit,
//     deps_consumed and the MISSING rows.
//
// Before this was one function the predicate was written out twice, once at
// each site. Nothing pinned that the two copies agreed, and widening one of
// them alone produces exactly the silent-honesty regressions this slice exists
// to close: widen it here only and the writer reports NOT-CHECKED while the
// warner stays silent, so the user gets `dependencies_checked: false` with no
// explanation; widen it at the call site only and the warning fires for test
// sets whose assertion did run, training users to ignore the line. Both keep
// the whole suite green, because each half is pinned separately and each half
// is individually correct. Sharing the function makes
// `noEligibleDeps == (len(expected) > 0 && len(eligible) == 0)` true BY
// CONSTRUCTION rather than by two people remembering.
//
// WHAT IS EXCLUDED, and why it is not the bug:
//
//   - DNS: resolution order is non-deterministic.
//   - Reusable tiers (session / connection / config): recorded once at app
//     boot and shared across every test, so they are not deterministically
//     attributed to one test's window. Asserting their presence per test would
//     go "missing" at random and fail healthy tests.
//
// The exclusion is deliberate and is NOT relaxed to make the assertion cover
// more recordings — that would trade a false green for a false red. What the
// caller owes the user is an honest report that the assertion had nothing to
// run over, which is what the eligibility guard in buildDepResults and the
// warning keyed off noEligibleDeps between them provide.
//
// Returns a slice that aliases nothing in expected (entries are copied by
// value), so callers may retain it.
func eligibleExpectedEntries(
	expected []models.MockEntry,
	mockKindByName map[string]models.Kind,
	reusableMockNames map[string]bool,
) []models.MockEntry {
	eligible := make([]models.MockEntry, 0, len(expected))
	for _, m := range expected {
		if isDNSMockEntry(m, mockKindByName) || reusableMockNames[m.Name] {
			continue
		}
		eligible = append(eligible, m)
	}
	return eligible
}

// buildDepResults is the first writer of models.Result.DepResult
// (keploy-consumer-design-v2.md §2 "Report — first ever writer of
// models.Result.DepResult", §7 slice 4).
//
// It emits one presence row per per-test dependency the recording says this
// test exercised that was NOT observed during the test's window — i.e. per
// mapped mock that survives the SAME filters the existing consumed-vs-expected
// assertion applies:
//
//   - DNS is excluded: resolution order is non-deterministic.
//   - Reusable tiers (session / connection / config) are excluded: they are
//     recorded once at app boot and shared across every test, so they are not
//     deterministically attributed to a single test's window. Including them
//     would emit rows that go "missing" at random.
//
// The dependencies that WERE observed are NOT persisted individually, in any
// mode: they collapse into depAssertion.Consumed, a plain int on the result.
// One row per consumed dependency costs ~190 bytes of YAML in a report that is
// written per test-set, re-read by `keploy report` and uploaded to the fleet
// report store; a Postgres-chatty suite consumes 50-200 mocks per test, so
// that is 3-11 MB per test-set report of `consumed/consumed` boilerplate no
// consumer reads, and the identities are already persisted by
// TestResult.MatchedCalls.
//
// depAssertion.Checked IS THE ONE BIT THAT MAKES `dep_result: []`
// UNAMBIGUOUS, and it is set when `valid` holds AND AT LEAST ONE MAPPED
// DEPENDENCY SURVIVED THE FILTER ABOVE — even if nothing was missing and
// nothing was consumed. Without the bit a test that lost nothing and a test
// whose assertion never executed (a --base-path run, --disable-mapping, a test
// set with no mappings.yaml, a failed consumed-mock fetch) are byte-identical
// on disk, and an agent reading the NDJSON contract cannot tell "no dependency
// regressions" from "this question was never asked" — which would re-create,
// in the machine-readable surface, exactly the false-green this slice exists
// to close.
//
// The eligibility half of that conjunction is not a detail: `valid` is
// computed from the RUN's shape (instrument mode, a live mapping, a successful
// consumed fetch) and says nothing about whether this test had a dependency
// the assertion is allowed to look at. For an ordinary recording it usually
// does not — see the eligibility guard at the bottom of this function — and
// setting Checked from `valid` alone reported "the assertion ran and found
// nothing missing" for tests where it never ran over a single dependency.
//
// An earlier revision of this slice encoded that same bit as an
// unconditionally-written aggregate DepResult row, which cost +224 bytes of
// nested YAML per test on EVERY report, forever, including the ones where
// nothing is missing and the verdict knob is off. Two scalars with omitempty
// carry the same information for ~40 bytes, and leave a clean test's
// `dep_result: []` byte-identical to a pre-slice-4 report.
//
// The consumed side applies the same DNS + reusable-tier filter that
// filteredMockNames does at the call site, so "this function produced a row
// with Normal=false" and "isMockSubset(filteredMockNames, filteredExpected)
// returned false" are the same signal computed from the same inputs. That
// matters because the --assert-dependencies verdict keys off mockSetMismatch
// while the rendered explanation comes from these rows: if the two used
// different filters a user could get a FAILED test with nothing to look at.
//
// valid gates the whole thing (it becomes depAssertion.Checked). It must be
// the SAME predicate that gates
// mockSetMismatch (see depAssertionValid at the call site): an expected-vs-
// consumed comparison is only a valid per-test assertion when the per-test
// mock mapping was actually armed on the agent, which happens only in
// instrument mode with mapping-based strategy and a successful per-test
// consumed fetch. Returning the zero depAssertion (rather than a set of false
// rows) keeps the report byte-identical to today for every other mode — the
// same policy MockMismatches and MatchedCalls already use.
//
// This does NOT change how mocks are matched or served; it is a read-only
// projection of bookkeeping the replay loop already computes.
func buildDepResults(
	expected []models.MockEntry,
	consumed []models.MockState,
	valid bool,
	reusableMockNames map[string]bool,
	mockKindByName map[string]models.Kind,
	lookup map[string]mockDisplayInfo,
) depAssertion {
	if !valid || len(expected) == 0 {
		return depAssertion{}
	}

	consumedNames := make(map[string]bool, len(consumed))
	for _, m := range consumed {
		if m.Kind == models.DNS || isReusableTierState(m) {
			continue
		}
		consumedNames[m.Name] = true
	}

	var out []models.DepResult
	consumedCount := 0
	// missingOverflow counts the missing dependencies past depMissingRowCap.
	// They collapse into one models.DepMissingOverflowRow instead of being
	// written out individually — see that function for the sizing measurement
	// and for why the collapsed row still carries Normal=false.
	missingOverflow := 0
	// depIndex counts positions in the FILTERED EXPECTED list, not in the
	// emitted slice. The row name is documented as a stable identifier that
	// agents correlate across runs (models.DepRowName), and indexing by output
	// position made it depend on which OTHER dependencies happened to be
	// missing in that particular run: with expectations [a b c], a run that
	// lost only `a` and a run that lost only `c` both emitted "deps[0]",
	// naming two different dependencies. Indexing by the recording makes the
	// index a property of the mapping, so the same dependency keeps the same
	// name whatever else happened.
	//
	// HONESTLY, though, it is a property of the mapping AND of the mock data
	// this run managed to load: the skip below reads mockKindByName and
	// reusableMockNames, both filled from the mocks RunTestSet fetched, and
	// the registry fetch that backs `lookup` swallows its error (r.mockDB may
	// also be nil). A run that classifies fewer entries as DNS or
	// reusable-tier skips fewer of them and shifts every index after the
	// first one it failed to classify. So the name is stable across runs that
	// loaded the same mock set — which is the normal case, since rows are only
	// built when depAssertionValid holds and that requires instrument mode —
	// not stable unconditionally. An agent correlating rows across runs should
	// treat the name as an identifier, not as a promise.
	//
	// eligibleExpectedEntries is the SHARED definition of the filter, the same
	// call RunTestSet uses to build filteredExpectedNames and noEligibleDeps.
	// It is not inlined here: the persisted Checked bit and the user-facing
	// "why nothing was asserted" warning are then two computations of one
	// question, and nothing would pin that they agree.
	eligible := eligibleExpectedEntries(expected, mockKindByName, reusableMockNames)
	depIndex := 0
	for _, m := range eligible {
		i := depIndex
		depIndex++

		if consumedNames[m.Name] {
			consumedCount++
			continue
		}

		if len(out) >= depMissingRowCap {
			// Past the cap. The dependencies are visited in RECORDED order
			// (depIndex above), so which ones make the cut is a property of
			// the mapping and is stable across runs — a report diffed between
			// two runs does not shuffle.
			missingOverflow++
			continue
		}

		depType := depTypeFor(m, mockKindByName, lookup)
		out = append(out, models.DepResult{
			Name: models.DepRowName(i, depType, depTargetFor(m, lookup)),
			Type: depType,
			Meta: []models.DepMetaResult{{
				Normal:   false,
				Key:      models.DepKeyPresence,
				Expected: models.DepPresenceConsumed,
				Actual:   models.DepPresenceMissing,
			}},
		})
	}

	// THE ELIGIBILITY GUARD, and the reason `valid` alone is not enough.
	//
	// `eligible` holds the entries that SURVIVED the shared filter. Zero means
	// the recording mapped dependencies to this test and every one of them was
	// excluded — DNS, or the reusable session/connection tier — so the
	// assertion never had anything to run over. It is the same slice
	// RunTestSet measures for noEligibleDeps, so the bit persisted here and the
	// reason the user is shown cannot disagree.
	//
	// That is not a corner case, it is the DEFAULT for an ordinary recording.
	// models.Mock.DeriveLifetime's kind fallback (pkg/models/lifetime.go)
	// classifies untagged HTTP / HTTP2 / Postgres / MySQL / Generic egress as
	// LifetimeSession, so isReusableTierMock is true for every mock a plain
	// recording of an HTTP app produces and reusableMockNames skips all of
	// them. Returning Checked=true here persisted `deps_checked: true,
	// deps_consumed: 0, dep_result: []` — which models.Result.DepsChecked and
	// the NDJSON `dependencies_checked` both document as "the assertion ran and
	// found nothing missing" — for a test where nothing was ever ELIGIBLE to be
	// checked. That is the false green this slice exists to close, re-created
	// inside the surface built to close it.
	//
	// The fix is NOT to start asserting the excluded mocks. They are excluded
	// deliberately (see the filter note in this function's doc comment):
	// reusable-tier mocks are recorded once at app boot and shared across every
	// test, so a per-test presence assertion over them goes "missing" at random
	// and produces false REDs. The dishonest bit is the bug; the exclusion is
	// the design.
	//
	// Consumed and Rows stay consistent by construction: with nothing eligible
	// the loop above incremented consumedCount zero times and appended no rows,
	// so the zero depAssertion loses no information.
	//
	// The user learns WHY from the per-test-set warning — see
	// depInertNoEligibleDepsReason, which names this exact state.
	if len(eligible) == 0 {
		return depAssertion{}
	}

	if missingOverflow > 0 {
		out = append(out, models.DepMissingOverflowRow(missingOverflow))
	}

	return depAssertion{Checked: true, Consumed: consumedCount, Rows: out}
}

// depAssertion is the complete per-test dependency verdict buildDepResults
// produces: whether the assertion RAN, how many recorded dependencies were
// exercised, and one row per dependency that was NOT.
//
// Checked and Consumed are scalars rather than a synthesised DepResult row on
// purpose — see the sizing note on buildDepResults. The zero value is exactly
// "the assertion did not run", which is what every mode that cannot arm the
// per-test mock mapping returns.
type depAssertion struct {
	// Checked is the persisted proof that the assertion ran OVER AT LEAST ONE
	// ELIGIBLE DEPENDENCY. True even when Consumed is 0 and Rows is empty;
	// that combination is a real answer ("checked, nothing exercised, nothing
	// missing"), not a missing one.
	//
	// It is FALSE when the mapping's every entry was filtered out as DNS or
	// reusable-tier, which is the normal shape for a recording whose mocks
	// carry no per-test tier tag. "Nothing was eligible" is not "nothing was
	// missing", and reporting it as the latter is a false green.
	Checked bool
	// Consumed counts the recorded per-test dependencies that WERE observed.
	Consumed int
	// Rows carries one presence row per dependency that went missing, plus at
	// most one models.DepMissingOverflowRow. Nil when nothing went missing.
	Rows []models.DepResult
}

// depMissingRowCap bounds the number of INDIVIDUAL missing-dependency rows
// persisted per test; the rest collapse into models.DepMissingOverflowRow.
//
// 50 is chosen so the cap is invisible for the shape this signal is actually
// for — an app that stopped making one or a handful of recorded calls, which
// is what a real regression looks like — while bounding the pathological shape
// the slice's own flagship scenario produces: a downstream service removed, or
// a mock pool whose names drifted wholesale, makes every mapped dependency of
// every test unconsumed at once. Measured through reportdb.InsertReport at 100
// tests x 200 mapped dependencies, the uncapped writer produced a 5.4 MB
// test-set report against 115 KB pre-slice-4; with the cap that report is
// bounded at ~1.4 MB and the FIRST 50 offenders (in recorded order) are still
// named individually, which is more than a human or an agent acts on.
//
// Measured end to end through reportdb.InsertReport, 100 tests x 200 mapped
// dependencies with NOTHING consumed (the all-missing shape):
//
//	pre-slice-4 (no dep_result at all) :    37,997 B
//	uncapped                           : 4,829,097 B   (127x)
//	capped at 50 + overflow row        : 1,272,797 B   (33x, 3.8x smaller)
//
// TestBuildDepResults_AllMissingStaysBounded pins the ratio on the real writer
// output so the cap cannot be quietly removed.
//
// A user who wants every row has the mapping itself (mappings.yaml) and
// TestResult.MockMismatches, both of which carry the full expected set.
const depMissingRowCap = 50

// depTypeFor resolves the normalised protocol family of a mapping entry,
// preferring the entry's own Kind and falling back to the loaded-mock lookup
// for entries recorded without one.
func depTypeFor(m models.MockEntry, mockKindByName map[string]models.Kind, lookup map[string]mockDisplayInfo) string {
	kind := m.Kind
	if kind == "" {
		if k, ok := mockKindByName[m.Name]; ok {
			kind = string(k)
		}
	}
	if kind == "" {
		kind = lookup[m.Name].protocol
	}
	if kind == "" {
		return ""
	}
	return models.DepTypeForKind(models.Kind(kind))
}

// depTargetFor resolves the human-meaningful destination of a mapping entry.
// Empty is a legitimate answer (r.mockDB may be nil, or the mock carries
// nothing identifying); DepRowName renders correctly without it.
func depTargetFor(m models.MockEntry, lookup map[string]mockDisplayInfo) string {
	return lookup[m.Name].target
}

// isDNSMockEntry reports whether a mapping entry refers to a DNS mock, using
// the entry's own Kind first and the loaded-mock lookup as a fallback for
// entries recorded without one. Extracted so buildDepResults and the inline
// filteredExpectedNames loop in RunTestSet cannot drift apart.
func isDNSMockEntry(m models.MockEntry, mockKindByName map[string]models.Kind) bool {
	if strings.EqualFold(m.Kind, string(models.DNS)) {
		return true
	}
	if kind, ok := mockKindByName[m.Name]; ok && kind == models.DNS {
		return true
	}
	return false
}

// shouldEmitFailureLogs reports whether the response-diff logs are printed for
// a test whose per-test mock set diverged.
//
// Historically a divergence meant "re-record", so the diff was suppressed as
// noise and the test was quietly demoted to OBSOLETE. Under
// --assert-dependencies that same test is about to be marked FAILED, and
// suppressing its diffs would hand the user a red test with no explanation.
// Knob off (the default) is byte-identical to before.
//
// neverDemotable forces the diffs out for a Kind whose verdict can never be
// demoted (see neverDemotableKind). It takes the BY-KIND bit, not the narrower
// "an effect mock went unconsumed" one, and the difference is the whole
// property: a consumer test whose mock set diverged only on coordination
// traffic still has a verdict that came from the judge, and suppressing its
// rows would leave a FAILED consumer test with no categories, no summary and
// no findings list — the diff hidden on precisely the flagship regression this
// contract exists to catch. For this Kind the judge's rows ARE the finding,
// never mapping drift, so the historical suppression has no meaning here.
func shouldEmitFailureLogs(mockSetMismatch, assertDependencies, neverDemotable bool) bool {
	return !mockSetMismatch || assertDependencies || neverDemotable
}

// dependencyAssertionRejects reports whether the opt-in dependency assertion
// (config.Test.AssertDependencies / --assert-dependencies) turns a diverged
// per-test mock set into a real test failure.
//
// A diverged set means an expected per-test mock was not observed during the
// test's window — the app stopped making an outgoing call the recording says
// it makes. Today that is demoted to OBSOLETE and the run still exits 0
// (keploy-consumer-design-v2.md §5, false-pass row 0). Default false keeps
// that behaviour for every existing user; opting in fails the test, the test
// set, and the process exit code.
func dependencyAssertionRejects(mockSetMismatch, assertDependencies bool) bool {
	return mockSetMismatch && assertDependencies
}

// demoteToObsolete reports whether a non-passing test whose mock set diverged
// keeps the historical silent demotion to OBSOLETE (which does NOT fail the
// test set and does NOT affect the exit code) rather than being marked FAILED.
//
// Three independent opt-ins veto the demotion: --schema-noise-strict
// (strictMockReject), --assert-dependencies (depAssertFail) and
// --strict-failure. All default false, so the default verdict is unchanged.
// neverDemotable is the fourth veto and is not a knob at all: see
// neverDemotableKind.
func demoteToObsolete(mockSetMismatch, strictMockReject, depAssertFail, strictFailure, neverDemotable bool) bool {
	return mockSetMismatch && !strictMockReject && !depAssertFail && !strictFailure && !neverDemotable
}

// neverDemotableKind reports whether a test case Kind's verdict may NEVER be
// demoted to OBSOLETE, whatever the knobs say and whatever mock happened to go
// unconsumed.
//
// THIS IS THE FIRST LINE OF THE CONSUMER CONTRACT, not a refinement of it.
// The OBSOLETE demotion exists for a real condition: a mock pool that drifted
// away from the recording, where "an expected mock was not consumed" says
// nothing about the application. For a CONSUMER test that reading is exactly
// inverted. Its effect mocks ARE its assertions — an unconsumed effect mock
// means the worker did not produce the message the recording says it
// produces — so the demotion would route the flagship regression ("the worker
// stopped producing") to OBSOLETE, which does not fail the test set and does
// not change the exit code. The run would report verified_green for a broken
// worker.
//
// IT IS KEYED ON Kind ALONE, AND THAT IS DELIBERATE. This predicate answers
// only one question: may a test the judge ALREADY FAILED be graded OBSOLETE?
// For a consumer test the answer is no in every configuration, whatever mock
// happened to go unconsumed — the verdict came from the judge's own rows
// (EFFECT_BODY_CHANGED, CONSUMER_COMPLETION_TIMEOUT, a named refusal), and
// demoting it would drop a real failure to a green run with the mismatch as
// its only explanation. Narrowing this to "an unconsumed effect mock is
// present" reopens design §5's false-pass row 0 for the single most likely
// slice-6 integration mistake: a parser that does not run its served trigger
// through the DeleteFilteredMock / GetConsumedMocks bookkeeping makes the
// mock set diverge on EVERY consumer test, and the unconsumed mock is a
// trigger, which hasUnconsumedEffectMock deliberately skips.
//
// The OTHER question — may a test the judge PASSED be promoted to FAILED? —
// is missingEffectMockPromotes, and that one MUST be narrow, because a
// promotion on coordination traffic is a false RED. Two questions, two
// predicates; they were one boolean and it could not be right for both.
func neverDemotableKind(kind models.Kind) bool {
	return kind == models.CONSUMER
}

// missingEffectMockPromotes reports whether a test whose response/verdict
// PASSED must be FAILED anyway because a mock carrying one of its effect
// claims was never consumed.
//
// This is the narrow half of the pair, and the narrowness is a false-RED
// guard rather than a concession. mockSetMismatch fires for ANY unconsumed
// per-test mock, and a consumer test legitimately maps per-test coordination
// mocks it does not have to replay identically every run (design §4 P4
// deliberately keeps OffsetFetch per-test: a client that had a cached
// position, or that did not rejoin, skips it). Promoting a clean test on
// those would fail it and hand the reader a message about the worker's
// production that has nothing to do with what happened. Design §5 scopes the
// missing-effect claim to "any mapped role=effect mock or spec.writes entry",
// and this is that scope.
func missingEffectMockPromotes(kind models.Kind, expected, consumed []string, lookup map[string]mockDisplayInfo) bool {
	return kind == models.CONSUMER && hasUnconsumedEffectMock(expected, consumed, lookup)
}

// hasUnconsumedEffectMock reports whether any expected per-test mock that went
// UNCONSUMED could be carrying one of this test's effect claims: a mock the
// parser tagged role=effect, or a mapped call of a different protocol family
// than the trigger (the spec.writes / consume-and-write case, which no parser
// tags because the mock belongs to another protocol's parser entirely).
//
// IT IS WIDER THAN replay.mappingCanCarryAnEffectClaim ON PURPOSE, and the two
// must not be re-unified. That one decides whether a test may PASS on a
// delegated claim, so it accepts only positive attribution (role=effect):
// untagged cross-family traffic is exactly what the sideEffects count is made
// of, and letting it vouch is the count vouching for itself. This one decides
// whether a PASSED test must be FAILED, where the same untagged mock is a
// candidate regression and excusing it would be the silent green. Opposite
// questions, opposite defaults, both landing on red when in doubt.
//
// IT FAILS CLOSED, AND THE DIRECTION IS THE WHOLE POINT. There is exactly one
// shape of unconsumed mock this may excuse: same-family coordination traffic —
// a fetch position, a heartbeat, a commit — which is the case the OBSOLETE
// demotion exists for. Excusing it needs POSITIVE identification, all four of:
// no role tag, a known Kind on the mock, a known Kind on the trigger, and the
// two Kinds equal. Anything else — no entry in the lookup at all, an entry
// with no Kind, no role=trigger mock to compare against — is treated as a
// possible effect and vetoes.
//
// That matters because the lookup is BEST-EFFORT: RunTestSet builds it from
// r.mockDB.GetUnFilteredMocks and both a nil mockDB and a fetch error leave it
// empty (the error is logged, not swallowed, but it still cannot be repaired
// here). Excusing a miss would mean a consume-and-write test whose write mock
// went unconsumed — the flagship "the worker stopped writing" regression — is
// reported PASSED because a side lookup did not load. A contract whose premise
// is "no silent pass" must degrade to red, not to green.
//
// An unconsumed TRIGGER is the one unconditional exclusion, and it is not a
// degradation: it is keploy failing to deliver, not the worker failing to
// produce. The gate names that by itself (trigger_not_delivered), and routing
// it through the worker-blaming promotion would point the reader at the
// application for keploy's own miss. It does NOT rescue the verdict either
// way — neverDemotableKind keeps a judge-failed consumer test FAILED whatever
// this returns.
//
// Both name slices are the already-filtered per-test sets (DNS and
// reusable-tier mocks removed) that mockSetMismatch itself is computed over,
// so this cannot veto a demotion the mismatch signal did not raise.
func hasUnconsumedEffectMock(expected, consumed []string, lookup map[string]mockDisplayInfo) bool {
	consumedSet := make(map[string]bool, len(consumed))
	for _, name := range consumed {
		consumedSet[name] = true
	}

	// The trigger's family, read off the expectation rather than assumed.
	var triggerKind models.Kind
	for _, name := range expected {
		if lookup[name].role == models.RoleTrigger {
			triggerKind = lookup[name].kind
			break
		}
	}

	for _, name := range expected {
		if consumedSet[name] {
			continue
		}
		info := lookup[name]
		if info.role == models.RoleTrigger {
			continue
		}
		if isSameFamilyCoordination(info, triggerKind) {
			continue
		}
		return true
	}
	return false
}

// isSameFamilyCoordination reports whether an unconsumed mock is POSITIVELY
// identified as coordination traffic of the trigger's own protocol family, the
// one shape hasUnconsumedEffectMock excuses. Every conjunct is a thing that
// must be KNOWN, not merely absent: an empty lookup entry satisfies none of
// them, so a degraded registry vetoes instead of excusing.
func isSameFamilyCoordination(info mockDisplayInfo, triggerKind models.Kind) bool {
	return info.role == "" && info.kind != "" && triggerKind != "" && info.kind == triggerKind
}

// resolveTestStatus turns an ALREADY-RESOLVED response outcome plus the three
// mock-set veto flags into a persisted models.TestStatus and into whether the
// test set itself goes red (which is what drives the process exit code).
//
// It is deliberately NOT the entry point: `responseMatched` here means "the
// response matched AND no veto promoted this test", i.e. it is downstream of
// the promotions resolveTestOutcome performs. Call resolveTestOutcome — it
// owns the promotions and is the seam the wiring test pins. This function
// stays separate only because the demotion algebra is worth reading, and
// asserting, on its own.
//
// failsSet is deliberately separate from "status == FAILED": OBSOLETE does not
// fail the test set, and that asymmetry IS the silent-green hole
// (keploy-consumer-design-v2.md §5, false-pass row 0).
func resolveTestStatus(responseMatched, mockSetMismatch, strictMockReject, depAssertFail, strictFailure, neverDemotable bool) (status models.TestStatus, failsSet bool) {
	if responseMatched {
		return models.TestStatusPassed, false
	}
	if demoteToObsolete(mockSetMismatch, strictMockReject, depAssertFail, strictFailure, neverDemotable) {
		return models.TestStatusObsolete, false
	}
	return models.TestStatusFailed, true
}

// mismatchLog names the log line RunTestSet emits for a diverged per-test mock
// set. It exists so the CHOICE of line is a value this package can test,
// rather than a branch buried in a 3000-line function where a reviewer proved
// it could be neutered with the whole suite staying green.
type mismatchLog int

const (
	// mismatchLogNone: the mock set matched, nothing to say.
	mismatchLogNone mismatchLog = iota
	// mismatchLogSchemaNoiseReject: the response matched but
	// --schema-noise-strict rejected the mock (non-noise request-body drift).
	mismatchLogSchemaNoiseReject
	// mismatchLogDependencyReject: the response matched but
	// --assert-dependencies promotes the unexercised dependency to a failure.
	mismatchLogDependencyReject
	// mismatchLogIgnoredResponseMatched: the response matched and no knob is
	// on — the historical Debug line, and the silent-green case itself.
	mismatchLogIgnoredResponseMatched
	// mismatchLogObsolete: the response failed and the test is demoted.
	mismatchLogObsolete
	// mismatchLogVetoedFailure: the response failed and a veto flag keeps it
	// FAILED instead of demoting it, so the line must name the flag rather
	// than telling the user to re-record.
	//
	// CONSUMER-VISIBLE STRING CHANGE, for --strict-failure users only. This
	// arm previously emitted the same line as mismatchLogObsolete ("mock
	// mapping mismatch detected; marking testcase as obsolete. Re-record the
	// test case or run with --update-test-mapping..."), which contradicted the
	// verdict it was announcing and pointed the user at the wrong fix. It now
	// says "marking testcase as FAILED" and carries a `reason` field naming
	// the flag. The VERDICT is unchanged in every combination. Nothing in this
	// repo greps the old literal (checked: .github/workflows and the e2e
	// scripts have no match for "marking testcase"), but anyone counting
	// demotions by scraping it externally loses those hits — call it out in
	// the release notes.
	mismatchLogVetoedFailure
	// mismatchLogNonDemotableReject: the response matched and the test is
	// FAILED anyway because its Kind's verdict can never be demoted — a
	// consumer test whose effect mock went unconsumed. It needs its own line
	// because the other two promotion lines name a FLAG the user could turn
	// off, and this one does not: the remedy is to look at the worker, not at
	// the invocation.
	mismatchLogNonDemotableReject
)

// testOutcome is the complete per-test decision for a diverged (or clean)
// mock set: the persisted status, whether the test set goes red, whether the
// mismatch is recorded for the end-of-run report, and which log line explains
// it.
type testOutcome struct {
	Status         models.TestStatus
	FailsTestSet   bool
	RecordMismatch bool
	Log            mismatchLog
	// VetoFlags names the flag(s) that kept a demotable test FAILED, for the
	// mismatchLogVetoedFailure line. Empty otherwise.
	VetoFlags string
	// DepAssertFail is the resolved --assert-dependencies rejection, exposed
	// because the caller logs it and because resolveTestStatus consumes it.
	DepAssertFail bool
}

// resolveTestOutcome is THE per-test verdict: the single place that turns the
// raw response outcome plus the three opt-in knobs into everything RunTestSet
// does about it.
//
// It takes the PRE-PROMOTION response result. That is the whole point: the
// flagship promotion this slice adds — the response MATCHED but a recorded
// outgoing call was not observed, and --assert-dependencies is on — happens by
// flipping the response result to false, so a verdict function that only saw
// the post-flip value could never assert it. It used to live as
// `case testPass && depAssertFail:` inside RunTestSet, where a reviewer
// replaced the condition with `testPass && false` and the entire five-package
// suite stayed green. Every branch below is now a table row in
// TestResolveTestOutcome.
//
// Nothing here is reachable unless mockSetMismatch is true, which is itself
// gated on depAssertionValid at the call site.
func resolveTestOutcome(responseMatched, mockSetMismatch, schemaNoiseStrict, assertDependencies, strictFailure, neverDemotable, effectMockMissing bool) testOutcome {
	depAssertFail := dependencyAssertionRejects(mockSetMismatch, assertDependencies)
	out := testOutcome{DepAssertFail: depAssertFail}

	if !mockSetMismatch {
		out.Status, out.FailsTestSet = resolveTestStatus(responseMatched, false, false, false, strictFailure, neverDemotable)
		return out
	}

	strictMockReject := false
	switch {
	case responseMatched && schemaNoiseStrict:
		// Strict schema-noise: the expected mock was REJECTED because a
		// non-noise request-body field drifted. The response can still match
		// (a deterministic dependency), so the response check alone cannot
		// catch it.
		responseMatched = false
		strictMockReject = true
		out.Log = mismatchLogSchemaNoiseReject
		out.RecordMismatch = true
	case responseMatched && effectMockMissing:
		// A CONSUMER test whose effects all compared clean but whose expected
		// effect mock was never consumed. For this Kind the two statements
		// cannot both be true: an effect mock that nothing consumed is an
		// effect the worker did not produce. Unconditional — there is no knob,
		// because there is no configuration in which grading that green is
		// correct.
		//
		// THIS ARM TAKES THE NARROW BIT. A promotion turns a passing test red,
		// so it must fire only on a mock that could actually be carrying an
		// effect claim — never on the coordination traffic a healthy consumer
		// legitimately skips. The BY-KIND bit belongs to the two arms below,
		// where the test has already failed and the only question is whether
		// that failure may be graded away.
		responseMatched = false
		out.Log = mismatchLogNonDemotableReject
		out.RecordMismatch = true
	case responseMatched && depAssertFail:
		// The slice's flagship promotion: response matched, recorded outgoing
		// call not observed, knob on -> a real FAILED test and a red run.
		responseMatched = false
		out.Log = mismatchLogDependencyReject
		out.RecordMismatch = true
	case responseMatched:
		// The silent-green case with the knob off. The verdict does not
		// change — visibility comes from the DepResult rows, which are written
		// and rendered either way.
		out.Log = mismatchLogIgnoredResponseMatched
	case depAssertFail || strictFailure || neverDemotable:
		// The response failed AND a veto keeps the test FAILED. The historical
		// "marking testcase as obsolete / re-record" wording would contradict
		// the verdict and point the user at the wrong fix, so name the reason.
		//
		// neverDemotable, not effectMockMissing. The judge already said this
		// consumer test failed; which mock happened to go unconsumed cannot
		// make that verdict obsolete, and letting it would drop a named
		// refusal or a real effect diff to a green run.
		out.Log = mismatchLogVetoedFailure
		out.VetoFlags = vetoFlagName(depAssertFail, strictFailure, neverDemotable)
		out.RecordMismatch = true
	default:
		out.Log = mismatchLogObsolete
		out.RecordMismatch = true
	}

	out.Status, out.FailsTestSet = resolveTestStatus(responseMatched, mockSetMismatch, strictMockReject, depAssertFail, strictFailure, neverDemotable)
	return out
}

// depLogLevel says how loudly a missing dependency is reported per test.
type depLogLevel int

const (
	// depLogNone: nothing missing, nothing to say.
	depLogNone depLogLevel = iota
	// depLogDebug: reported-only mode (the knob is off). An expected-but-
	// unobserved mock is exactly the condition the OBSOLETE demotion exists to
	// tolerate, so on a suite with a drifting mock pool this fires on every
	// test of every run. Per-test it stays at Debug; RunTestSet emits ONE Warn
	// per test set summarising the count, so the signal is not lost and the
	// CLI does not grow hundreds of new Warn lines for users who never asked
	// for the assertion.
	depLogDebug
	// depLogError: --assert-dependencies is on, the run is going red anyway.
	depLogError
)

// attachDepResults writes the dependency rows onto a persisted test result and
// applies the two side effects that go with them: the DEPENDENCY_MISSING
// failure category and the per-test log level.
//
// Extracted from RunTestSet for the same reason resolveTestOutcome is: it is
// the primary deliverable of this slice and it lived on an untested line
// inside a 3000-line function, where deleting it left every test green.
//
// Returns the missing row names (for the test-set-level summary log) and the
// level the caller should log them at.
func attachDepResults(
	tcResult *models.TestResult,
	status models.TestStatus,
	dep depAssertion,
	assertDependencies bool,
) (missingNames []string, level depLogLevel) {
	if tcResult == nil {
		return nil, depLogNone
	}
	// APPEND, NEVER OVERWRITE. models.Result.DepResult has TWO producers: the
	// sync path's presence rows (`deps[i]`, built here) and the consumer
	// judge's per-effect rows (`effects[i]`, already on tcResult.Result
	// because the comparator wrote them into the *models.Result the replay
	// loop copied in). Assigning here would silently delete the judge's rows —
	// which are the entire verdict of a consumer test — while leaving every
	// call, argument and enclosing condition in RunTestSet untouched.
	//
	// For every other Kind nothing else writes the field, so the append is
	// byte-identical to the assignment it replaces. The row-name prefixes stay
	// disjoint (models.IsEffectRow), so a reader can still tell the producers
	// apart.
	tcResult.Result.DepResult = append(existingEffectRows(tcResult.Result.DepResult), dep.Rows...)
	// The two scalars are what make `dep_result: []` unambiguous. They are
	// written even when nothing is missing — that is the whole point of
	// DepsChecked — and left at their zero values when the assertion did not
	// run, which omitempty then keeps off the wire entirely.
	tcResult.Result.DepsChecked = dep.Checked
	tcResult.Result.DepsConsumed = dep.Consumed

	missingNames = models.MissingDepNames(dep.Rows)
	if len(missingNames) == 0 {
		return nil, depLogNone
	}

	// Only label FAILED/OBSOLETE tests: a PASSED test must never grow a
	// failure category (printSummary and the k8s-proxy fleet report both read
	// Category as a failure taxonomy). The ROWS stay on a passing test either
	// way — that is what makes the silent-green case visible without the knob.
	//
	// slices.Clone breaks the aliasing with testResult.FailureInfo.Category,
	// which the matcher owns and which is also shared with the persisted
	// Result copy; appendCategoryUnique keeps a second writer of this category
	// (slice 5's consumer projector is planned to add one) from duplicating it.
	if status == models.TestStatusFailed || status == models.TestStatusObsolete {
		tcResult.FailureInfo.Category = appendCategoryUnique(
			slices.Clone(tcResult.FailureInfo.Category), models.DependencyMissing)
	}

	if assertDependencies {
		return missingNames, depLogError
	}
	return missingNames, depLogDebug
}

// existingEffectRows returns the rows on a result that were written by the
// CONSUMER judge rather than by this file, so attachDepResults can add the
// sync-path rows without destroying them.
//
// It filters rather than simply keeping whatever is there, because
// attachDepResults runs once per test per --retry-passing CYCLE against a
// freshly built result: anything it does not recognise as another producer's
// row would otherwise accumulate.
func existingEffectRows(rows []models.DepResult) []models.DepResult {
	if len(rows) == 0 {
		return nil
	}
	out := make([]models.DepResult, 0, len(rows))
	for _, r := range rows {
		if models.IsEffectRow(r) {
			out = append(out, r)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// recordUnexercised is the SINGLE writer of the per-test-set set that
// warnUnexercisedDependencies reports on.
//
// Only the reported-only level is collected: under --assert-dependencies the
// per-test Error already fired and the run is red, so a summary Warn would be
// noise on top of a failure. Extracted because emptying this map is otherwise
// an invisible mutation — the wiring test pins the CALL to
// warnUnexercisedDependencies, but a summary over an always-empty map is a
// silent no-op, which is how the per-test Warn regression's replacement could
// be deleted with the suite staying green.
//
// The status is kept, not just the name: the summary distinguishes the tests
// whose response still matched (the silent-green population) from the ones
// that were demoted, which are two different things to go and look at.
//
// AUTHORITATIVE FOR THE CURRENT CYCLE, not cumulative. --retry-passing re-runs
// the passing tests up to five times and each cycle overwrites
// finalTestCaseResults[name], so the PERSISTED rows are always the LAST
// cycle's. A set that only ever inserted would keep a test that lost a
// dependency in cycle 1 and consumed it in cycle 2, and the end-of-test-set
// Warn would then name a test whose own report carries no missing row and
// count it in responseStillMatched — a WARN contradicting every other surface,
// in exactly the mock-consistency-flake scenario --retry-passing exists to
// smooth over. Deleting on a clean cycle makes the summary agree with the
// report by construction.
func recordUnexercised(set map[string]models.TestStatus, name string, status models.TestStatus, level depLogLevel) {
	if set == nil {
		return
	}
	if level != depLogDebug {
		delete(set, name)
		return
	}
	set[name] = status
}

// vetoFlagName names the flag that turned a mock-set mismatch into a FAILED
// verdict, so the log tells the user which knob to look at instead of pointing
// them at re-recording.
func vetoFlagName(depAssertFail, strictFailure, neverDemotable bool) string {
	var parts []string
	if depAssertFail {
		parts = append(parts, "--assert-dependencies")
	}
	if strictFailure {
		parts = append(parts, "--strict-failure")
	}
	if neverDemotable {
		// Not a flag, and the wording says so: a reader who sees this must
		// not go looking for an invocation to change.
		parts = append(parts, "this test case Kind is never demoted to obsolete")
	}
	return strings.Join(parts, ", ")
}

// dependencyAssertionInertReason explains why --assert-dependencies cannot run
// over a WHOLE TEST SET (the first three cases), over the test cases whose
// per-test consumed-mock fetch failed (the fourth), over the test cases whose
// every recorded dependency is ineligible (the fifth), or over the streaming
// subset of one (the sixth), or "" when it can run everywhere.
//
// The knob keys off mockSetMismatch, which needs an ARMED per-test mock
// mapping. Three test-set-level preconditions can silently remove it, and a
// user who puts --assert-dependencies in CI against a test set that fails any
// of them gets a green run with no indication the assertion never executed — the same
// silent-green class the slice exists to close.
//
// consumedFetchFailed is the FOURTH case. It is ranked above the two below it
// because it is the only one of the three the operator can act on — see
// depInertConsumedFetchReason for why absorbing it into the tier reason
// mis-sent people to re-tag a recording that was fine.
//
// noEligibleDeps is the FIFTH case and the one an ordinary recording actually
// hits. See depInertNoEligibleDepsReason: every mock the mapping records for a
// test can be excluded from the assertion as DNS or reusable (session /
// connection) tier, which is exactly what models.Mock.DeriveLifetime's kind
// fallback makes of untagged HTTP / Postgres / MySQL egress. The assertion is
// then armed, has a mapping, and still has nothing to look at. buildDepResults
// reports that honestly as NOT-CHECKED; without this reason the user sees a
// report full of `dependencies_checked: false` and no statement of why.
//
// deferredStreaming is the SIXTH case (it is checked last) and, with the two
// above it, one of the three that are not test-set wide. RunTestSet splits
// SSE/chunked test cases out of the main replay loop into a Phase-2 pass
// (pkg.IsHTTPStreamingTestCase), and that pass deliberately writes no
// DepResult and never calls resolveTestOutcome — see the NOTE on
// models.Result.DepResult in the Phase-2 body — so --assert-dependencies
// cannot promote anything there no matter how healthy the mapping is. Without
// this case a CI run of --assert-dependencies over a set of streaming tests is
// green for an assertion that never executed, which is the same silent green
// as the other three.
//
// ORDER MATTERS: the three set-wide preconditions are checked first because
// each of them already disables the assertion for the streaming tests too, so
// naming one of them is strictly more informative than naming the deferral.
// consumedFetchFailed comes next because it is actionable and the states below
// it are not. noEligibleDeps outranks the streaming deferral for the same reason — the
// tier classification comes from the RECORDING, so it applies to the deferred
// streaming test cases as well, while the deferral says nothing about the
// tests the main loop just asserted. In practice the two are never both true
// at a call site: the main replay loop passes deferredStreaming=false (it only
// ever sees the non-streaming bucket) and the Phase-2 pass has no filtered
// expectation list to compute noEligibleDeps from, so it passes false.
//
// hasExpectedMocks is deliberately NOT one of them: it is a per-TEST condition
// and "this test case makes no outgoing calls" is completely normal, so
// warning on it would fire on every healthy suite.
//
// The cost of that choice, stated plainly because an earlier version of this
// comment got it wrong: hasExpectedMocks is a conjunct of depAssertionValid at
// the call site, so for such a test buildDepResults returns the ZERO
// depAssertion at its `!valid` guard — Checked is false. `dependencies_checked`
// is then false, byte-identical to a --base-path run or a failed consumed-mock
// fetch. A test that makes no outgoing calls is therefore reported as UNCHECKED
// rather than as "checked, nothing missing", and is indistinguishable on the
// wire from the other unchecked modes. That is the conservative reading of the
// documented rule ("read DepsChecked, never len(DepResult)",
// models.Result.DependenciesChecked) and never a false green, but it does mean
// `dependencies_checked` is false for every test in a suite that makes no
// outgoing calls at all.
//
// It is not fixable by setting Checked here anyway: hasExpectedMocks
// also gates mockSetMismatch, where dropping it would make isMockSubset
// compare a non-empty consumed set against an empty expectation and demote
// healthy tests to OBSOLETE. The two must keep using one predicate.
//
// noEligibleDeps is the NEIGHBOURING case and is treated differently on
// purpose: there the recording DOES map dependencies to the test and every one
// of them was filtered out, which a user reading a mapping full of entries has
// no way to guess. Both report UNCHECKED; only that one is worth a line.
func dependencyAssertionInertReason(instrument, useMappingBased, isMappingEnabled, consumedFetchFailed, deferredStreaming, noEligibleDeps bool) string {
	switch {
	case !instrument:
		return "not instrument mode: the per-test mock mapping is never armed for --base-path / remote-agent runs, so which mock a request consumed is not attributable to a single test"
	case !isMappingEnabled:
		return "mapping is disabled (test.disableMapping / --disable-mapping): there is no per-test expectation to assert against"
	case !useMappingBased:
		return "this test set has no usable mappings.yaml (recorded before test-mock mapping existed, or the mapping database is unavailable): re-record it, or run once with --update-test-mapping"
	case consumedFetchFailed:
		return depInertConsumedFetchReason
	case noEligibleDeps:
		return depInertNoEligibleDepsReason
	case deferredStreaming:
		return depInertStreamingReason
	}
	return ""
}

// depInertStreamingReason is compared by identity in
// dependencyAssertionInertMessage to pick the scope-accurate message, so it
// lives as a constant rather than an inline string.
const depInertStreamingReason = "this test set contains streaming (SSE/chunked) test cases, which are replayed in a deferred pass that writes no dependency rows and runs no dependency assertion: those test cases cannot be failed by the flag, however healthy the mapping is"

// depInertConsumedFetchReason names the state where the assertion is fully
// armed — instrument mode, mapping enabled, a usable mappings.yaml — and the
// run still could not ask the question, because fetching the mocks THIS test
// consumed failed. RunTestSet nils consumedMocks on that error and drops
// instrumentConsumedFetchErr into depAssertionValid, so the writer returns the
// zero depAssertion and the test is reported dependencies_checked=false.
//
// It OUTRANKS depInertNoEligibleDepsReason on purpose. Both states report the
// same honest NOT-CHECKED, but only one of them is the user's to fix: a
// transport failure talking to the agent is actionable, while the tier
// classification is a property of the recording. When a test set hits both at
// once (the fetch failed AND its mapping happened to be all-reusable) the
// earlier wording blamed the tier and sent the operator to re-tag a recording
// that was never the problem.
//
// It also closes a gap of its own. Before this arm existed a fetch failure on
// a test set with perfectly eligible per-test dependencies produced NO warning
// at all: the reason function returned "" and the run reported
// dependencies_checked=false for those tests with nothing said. That is the
// same silent class as the other five — an assertion the user asked for, which
// did not execute, with no line saying so.
const depInertConsumedFetchReason = "the per-test consumed-mock fetch failed for one or more test cases in this test set: the agent could not report which mocks the test actually consumed, so there is no observed set to compare the recording's mapping against. Their dependency verdict is dependencies_checked=false (NOT CHECKED), not clean — this is a transport/agent error, not a statement about the recording"

// depInertNoEligibleDepsReason names the state where everything the assertion
// needs is armed — instrument mode, mapping enabled, a usable mappings.yaml —
// and there is still nothing to assert, because every dependency the recording
// maps to these test cases is excluded from the per-test assertion.
//
// This is the DEFAULT for a recording whose mocks carry no per-test tier tag.
// models.Mock.DeriveLifetime classifies untagged HTTP / HTTP2 / Postgres /
// MySQL / Generic egress as session-tier (its documented legacy kind
// fallback), and session/connection-tier mocks are deliberately excluded: they
// are recorded once at app boot and shared across every test, so asserting
// their presence per test would go "missing" at random and fail healthy tests.
// DNS entries are excluded for the same class of reason (resolution order is
// non-deterministic).
//
// The message has to say what the report will show, because the honest report
// for this state (`dependencies_checked: false`) is indistinguishable on the
// wire from the other not-run modes: this line is the only place the user
// learns which one they are in.
const depInertNoEligibleDepsReason = "every dependency the recording maps to these test cases is excluded from the per-test assertion: session/connection-tier mocks (which is how untagged HTTP/Postgres/MySQL egress is classified, so this is the norm for recordings whose mocks carry no per-test tier tag) are shared across every test and are not attributable to one test's window, and DNS is non-deterministic. Nothing is left to assert, so these tests are reported as dependencies_checked=false (NOT CHECKED) rather than as checked-and-clean"

// dependencyAssertionInertMessage picks the headline for a reason.
//
// The three set-wide preconditions really do mean no dependency can fail ANY
// test in this test set, and get the blanket sentence. The other two are
// narrower claims and must not borrow it:
//
//   - the streaming deferral leaves a mixed test set's non-streaming tests
//     fully asserted, so a set-wide sentence would be a false alarm about them;
//   - no-eligible-dependencies is decided per test from that test's own mapped
//     entries, and says specifically that the verdict for those tests is
//     NOT-CHECKED rather than clean — which is the sentence that stops a
//     reader treating an empty `dep_result` as a green dependency result.
func dependencyAssertionInertMessage(reason string) string {
	switch reason {
	case depInertStreamingReason:
		return "--assert-dependencies does not run for this test set's streaming test cases; no dependency can fail one of them"
	case depInertNoEligibleDepsReason:
		return "--assert-dependencies has no eligible dependency to assert for one or more test cases in this test set; their dependency verdict is NOT CHECKED, not clean"
	case depInertConsumedFetchReason:
		return "--assert-dependencies could not read the consumed mocks for one or more test cases in this test set; their dependency verdict is NOT CHECKED, not clean"
	}
	return "--assert-dependencies is inert for this test set; no dependency can fail a test here"
}

// warnDependencyAssertionInert emits ONE warning per test set when the user
// asked for --assert-dependencies but the signal it keys off cannot be
// computed, or has nothing to run over. warned is the per-test-set latch,
// shared by the main replay loop and the deferred streaming pass: whichever
// reaches it first wins, and a test set never emits this warning twice.
//
// The main replay loop calls this once per TEST CASE (with that test's own
// consumedFetchFailed and noEligibleDeps), which is what lets the fourth and
// fifth reasons fire at all — they are properties of a single test's fetch and
// mapped entries, not of the test set. The latch keeps that one line per test
// set regardless.
//
// The message is chosen from the reason (dependencyAssertionInertMessage)
// because the scopes are not the same claim.
func (r *Replayer) warnDependencyAssertionInert(testSetID string, useMappingBased, isMappingEnabled, consumedFetchFailed, deferredStreaming, noEligibleDeps bool, warned *bool) {
	if !r.config.Test.AssertDependencies || warned == nil || *warned {
		return
	}
	reason := dependencyAssertionInertReason(r.instrument, useMappingBased, isMappingEnabled, consumedFetchFailed, deferredStreaming, noEligibleDeps)
	if reason == "" {
		return
	}
	*warned = true
	r.logger.Warn(dependencyAssertionInertMessage(reason),
		zap.String("testset", testSetID),
		zap.String("reason", reason))
}

// unexercisedSummary splits the collected tests into a sorted name list, a
// capped sample and the count whose RESPONSE still matched.
//
// The passed count is the one that matters and the one the wording has to be
// careful about: for a PASSED test the response really did match, which is the
// silent-green case; for an OBSOLETE test it did not — that is why it was
// demoted — so a blanket "the response still matched" would be wrong for
// exactly the subset a user is most likely to investigate.
func unexercisedSummary(tests map[string]models.TestStatus) (names, sample []string, passed int) {
	names = make([]string, 0, len(tests))
	for name, status := range tests {
		names = append(names, name)
		if status == models.TestStatusPassed {
			passed++
		}
	}
	sort.Strings(names)
	sample = names
	if len(sample) > depWarnSampleSize {
		sample = sample[:depWarnSampleSize]
	}
	return names, sample, passed
}

// warnUnexercisedDependencies emits the single per-test-set summary for
// dependencies that were recorded but not observed while the knob is off.
// Under --assert-dependencies the per-test Error already fired and the run is
// red, so this stays quiet.
//
// "not observed during the test's own window" is deliberate wording, not
// hedging: GetConsumedMocks is drained immediately after the response comes
// back, so an outgoing call the app makes AFTER writing its response (an audit
// write, an analytics POST, a cache set) is attributed to the next test, not
// this one. The data supports "keploy did not see this call inside this test's
// window"; it does not support "the app never made this call".
func (r *Replayer) warnUnexercisedDependencies(testSetID string, tests map[string]models.TestStatus) {
	if len(tests) == 0 {
		return
	}
	names, sample, passed := unexercisedSummary(tests)
	r.logger.Warn("some tests did not exercise a dependency their recording says they use, and were not failed for it (run with --assert-dependencies to fail on this). Note that an outgoing call made AFTER the response is written is not attributable to this test's window",
		zap.String("testset", testSetID),
		zap.Int("tests", len(names)),
		zap.Int("responseStillMatched", passed),
		zap.Strings("sample", sample))
}

// depWarnSampleSize caps the per-test-set sample so a fully-drifted suite
// cannot emit a thousand-name log line.
const depWarnSampleSize = 5
