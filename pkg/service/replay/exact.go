package replay

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// Ownership and exact consumption for keploy test, under the proven switch
// (design v14 §5.2): the replay hands the agent what each test case
// owns, and the agent serves each test case exactly its own mocks, each at
// most once and in recorded order, through the proxy's exact serving.
//
//   - A test case owns the mocks its mapping lists (the agent keeps the
//     data-plane ones). A test case the mapping omits, or lists with none,
//     owns none, and so does every test case of an all-empty mapping: a call
//     it makes is a miss. disableMapping does not apply.
//   - A set with no mappings.yaml (a recording made without the switch), or
//     one keploy test wrote from what a replay consumed rather than keploy
//     record (models.MappingSourceKeployTest), owns what its test windows
//     held: each test case the test mocks recorded inside its own window,
//     derived as keploy record derives mappings.yaml under the switch
//     (windowOwnership, models.DeriveWindowMappings), documents this keploy
//     cannot read included.
//   - A mapping that cannot be read refuses the set.
//   - The mocks the mapping's worker scope lists (models.Mapping.Worker; with
//     no mappings.yaml, the test mocks recorded outside every window) are the
//     set's own, owned by no test case: each answers from its place in the
//     set's run on (models.MockEntry.Place), whether a test case is open or
//     not, after the open test case's own, at most one request in the set's
//     staging (D1, D2).
//   - Every other mock no test case lists is the set's pool. It answers
//     between test cases, beside the protocol-phase mocks, with reuse or
//     cursor-saturate as recorded.
//   - The set is staged on the agent once; a test case's call opens it, and
//     the call after its consumption is read serves the pool again.
//   - The replay never writes mappings.yaml: only record does.

// exactPlan is the set's exact serving: what each test case owns, and the
// set's pool.
type exactPlan struct {
	// learnEnded: the set's learn step has ended (endLearning), and every
	// filter the plan sends says so (models.MockFilterParams.LearnEnded).
	learnEnded bool
	owned      map[string][]string
	// repeat is, by test, its recordings its mappings entry marks
	// `repeat: any` (models.MockEntry.Repeat): the call that opens it says
	// them (models.ExactServing.Repeat).
	repeat map[string][]string
	pool   []string
	// worker is the set's worker scope (models.Mapping.Worker): the mocks of
	// it the agent holds, in recorded order, each with its place. The
	// staging call hands it to the agent, which serves each from its place
	// on.
	worker []models.MockEntry
	// idle names the worker-scope mocks whose recorded answer their parser
	// declares idle (models.IsIdleAnswer): the set's end does not require
	// them answered (D4).
	idle map[string]bool
	// begun names the test cases of the set's run that have begun: the
	// worker-scope mocks placed after one of them are due by the set's end.
	begun map[string]bool
	// unheld names the worker-scope mocks the agent does not hold: required
	// all the same, never asked of the agent (staged).
	unheld map[string]bool
	// bootSeen: keploy started the set's application for this set, so the
	// worker-scope mocks placed at the set's start (its boot, and what the
	// application did before the first test) are due by the set's end. An
	// application kept alive from an earlier set (--keep-app-alive), or one
	// keploy did not start, booted before this set's run: those are served
	// if made, never required.
	bootSeen bool
	// ownsTestMocks: some test case owns a test mock (a data-plane mock the
	// agent holds).
	ownsTestMocks bool
	// ahead is, by test case, the mocks it owns that were in flight when it
	// began: recorded in the gap before its window, answered once it had
	// begun (models.WindowSchedule.InFlightInto, which gave it them). The
	// replay serves them with the pool just before the test case opens
	// (models.ExactServing.Ahead): the replayed app may ask in the gap, as
	// it did at record.
	ahead map[string][]string
	// answerPlaced names the held mocks placed at their answer
	// (models.IsAnswerPlaced): their place is their recorded answer.
	answerPlaced map[string]bool
	// aheadNamed is the test case the latest pool call named the calls in
	// flight of (serveExactPool's next), "" for none.
	aheadNamed string
	// testView is the test case whose own view the agent may still serve:
	// set as serveExactTest asks for it (a call that failed may have reached
	// the agent all the same), "" once a pool call succeeds. The next test
	// case begins only over the pool (poolBeforeBegin).
	testView string
	// share is the set's shares (concurrent_shares.go): nil for a set whose
	// mapping states no concurrency.
	share *sharePlan
	// background says background serving serves the set (s12Serves): its
	// staging says so to the agent, and its end does not require its worker
	// scope answered (unansweredWorker).
	background bool
	// returned is, by mock, the owner a drift move's recording was given
	// back to (returnStaleMoves): its quarantine no longer applies.
	returned map[string]string
}

// noteAhead fills p.ahead from what each test case of cases owns: its mapped
// mocks the agent holds placed before the case's recorded window, which a
// mapping gives a test only for a call in flight when it began
// (models.DeriveWindowMappings). A mock's place is its recorded request, or
// its answer for one placed at its answer (models.PlaceOf): a parser holds
// such a call until a window brings its recording, so one answered in the
// window is not in flight, and is not served in the gap.
func (p *exactPlan) noteAhead(mappings map[string][]models.MockEntry, cases []*models.TestCase) {
	if p == nil {
		return
	}
	for _, tc := range cases {
		start := models.RecordedWindow(tc).Start
		if start.IsZero() {
			continue
		}
		owned := make(map[string]bool, len(p.owned[tc.Name]))
		for _, n := range p.owned[tc.Name] {
			owned[n] = true
		}
		for _, e := range mappings[tc.Name] {
			at := e.ReqTimestampMock
			if p.answerPlaced[e.Name] {
				at = e.ResTimestampMock
			}
			place, err := models.ParseMockTimestamp(at)
			if err != nil || place.IsZero() || !place.Before(start) || !owned[e.Name] {
				continue
			}
			if p.ahead == nil {
				p.ahead = map[string][]string{}
			}
			p.ahead[tc.Name] = append(p.ahead[tc.Name], e.Name)
		}
	}
}

// begin notes that test case name of the set's run has begun.
func (p *exactPlan) begin(name string) {
	if p == nil {
		return
	}
	if p.begun == nil {
		p.begun = map[string]bool{}
	}
	p.begun[name] = true
	if p.share != nil && p.share.sharing {
		p.share.noteRun(name, len(p.share.cands[name]) > 0)
	}
}

// staged is the worker scope the agent stages: the entries of the mocks it
// holds, in order.
func (p *exactPlan) staged() []models.MockEntry {
	if p == nil {
		return nil
	}
	var out []models.MockEntry
	for _, e := range p.worker {
		if !p.unheld[e.Name] {
			out = append(out, e)
		}
	}
	return out
}

// isWorker reports whether name is one of the set's worker-scope mocks.
func (p *exactPlan) isWorker(name string) bool {
	if p == nil {
		return false
	}
	for _, e := range p.worker {
		if e.Name == name {
			return true
		}
	}
	return false
}

// takeWorker moves, of the mocks consumed before the set's first test that
// partitionInitialConsumed would re-arm (rearm), the worker-scope ones into
// counted (the startup consumption, counted as consumed): the agent spent
// each for the staging, so none is served again, and the set's end counts it
// answered. With no plan both are returned as they are.
func (p *exactPlan) takeWorker(counted, rearm []models.MockState) ([]models.MockState, []models.MockState) {
	if p == nil || len(p.worker) == 0 {
		return counted, rearm
	}
	var keep []models.MockState
	for _, m := range rearm {
		if p.isWorker(m.Name) {
			counted = append(counted, m)
			continue
		}
		keep = append(keep, m)
	}
	return counted, keep
}

// unansweredWorker is, in recorded order, the worker-scope mocks the set's
// end requires answered and consumed does not hold: those whose place the
// run reached (a test case that has begun, or the set's start when keploy
// started its application: bootSeen), but for the ones whose answer is idle
// (D3, D4). One the recording made as keploy record stopped the application
// (MockEntry.AtStop) is required only once keploy has stopped this set's
// application and read what it consumed as it stopped (stopped).
func (p *exactPlan) unansweredWorker(consumed map[string]models.MockState, stopped bool) []string {
	// Under the background rules the set's end requires none: what a
	// background call answered is judged by record's judging,
	// and a background call that stops happening is a limit the run says
	// (unansweredBackground) and NEXT names.
	if p == nil || p.background {
		return nil
	}
	return p.unansweredWorkerOf(consumed, stopped)
}

// unansweredWorkerOf is the worker-scope recordings today's rule requires by
// the set's end that consumed holds none of (unansweredWorker), whatever rules
// serve the set.
func (p *exactPlan) unansweredWorkerOf(consumed map[string]models.MockState, stopped bool) []string {
	if p == nil {
		return nil
	}
	var out []string
	for _, e := range p.worker {
		switch {
		case p.idle[e.Name], e.AtStop && !stopped:
			continue
		case e.Place == "" && !p.bootSeen, e.Place != "" && !p.begun[e.Place]:
			continue
		}
		if _, ok := consumed[e.Name]; !ok {
			out = append(out, e.Name)
		}
	}
	return out
}

// exactServing reports whether this replay serves each test case exactly
// its own mocks: under the proven switch, with an agent to serve them.
func (r *Replayer) exactServing() bool {
	return provenswitch.On() && r.instrument && !r.noAgent
}

// provenMappings reads what each test case of testSetID owns, under the
// proven switch, and an error for a mapping that cannot be read. It gives
// none, and why, for a set whose ownership its test windows then give
// (windowOwnership): one with no mapping store or no mappings.yaml, and one
// whose mappings.yaml keploy test wrote (models.MappingSourceKeployTest),
// which lists what a replay consumed, not what each test recorded. A
// mappings.yaml with no source is trusted as recorded: one keploy test wrote
// before it said so cannot be told from keploy record's.
func (r *Replayer) provenMappings(ctx context.Context, testSetID string) (map[string][]models.MockEntry, []models.MockEntry, string, error) {
	mappings, worker, _, _, why, err := r.provenMappingsOf(ctx, testSetID)
	return mappings, worker, why, err
}

// provenMappingsOf is provenMappings, the set's concurrency as its
// mappings.yaml states it (models.Mapping.Groups, Shared: nil for none),
// and what it says keploy record recorded the set with
// (models.Mapping.RecordedWith): none for a set with no such mark, and for
// one whose ownership its test windows give.
func (r *Replayer) provenMappingsOf(ctx context.Context, testSetID string) (map[string][]models.MockEntry, []models.MockEntry, *setConcurrency, []string, string, error) {
	const absent = "the test set has no mappings.yaml"
	if r.mappingDB == nil {
		return map[string][]models.MockEntry{}, nil, nil, nil, absent, nil
	}
	if mr, ok := r.mappingDB.(mappingReader); ok {
		m, err := mr.GetMapping(ctx, testSetID)
		if err != nil {
			return nil, nil, nil, nil, "", fmt.Errorf("keploy cannot judge each test case of %s: its mappings.yaml cannot be read: %w; record the set again", testSetID, err)
		}
		if m != nil && m.Source == models.MappingSourceKeployTest {
			return map[string][]models.MockEntry{}, nil, nil, nil, "the test set's mappings.yaml was written by keploy test, from what a replay consumed, not by keploy record", nil
		}
		mappings := map[string][]models.MockEntry{}
		var worker []models.MockEntry
		var conc *setConcurrency
		var recordedWith []string
		if m != nil {
			for _, tc := range m.TestCases {
				mappings[tc.ID] = tc.Mocks
			}
			worker = m.Worker
			conc = concurrencyOf(m.Groups, mappings, m.Shared)
			recordedWith = m.RecordedWith
		}
		return mappings, worker, conc, recordedWith, absent, nil
	}
	mappings, _, err := r.mappingDB.Get(ctx, testSetID)
	if err != nil {
		return nil, nil, nil, nil, "", fmt.Errorf("keploy cannot judge each test case of %s: its mappings.yaml cannot be read: %w; record the set again", testSetID, err)
	}
	if mappings == nil {
		mappings = map[string][]models.MockEntry{}
	}
	return mappings, nil, nil, nil, absent, nil
}

// s12Serves reports, under the proven switch (the caller's), whether background
// serving serves testSetID (s12_serving.go): its mappings.yaml carries a
// proven-routing record's mark (recordedWith holds models.RecordedWithS12Routing) and a
// registered func enables it (S12Serving). A set without the mark, and a
// marked set this build does not enable it for, has every recorded
// background call answered exactly and required by its end; for a set
// without the mark that is said once per set (the caller asks once, as the
// set's mocks load), with what it means under the switch.
func (r *Replayer) s12Serves(testSetID string, recordedWith []string) bool {
	if !(&models.Mapping{RecordedWith: recordedWith}).WasRecordedWith(models.RecordedWithS12Routing) {
		r.sayRecordedWithoutRouting(testSetID, recordedWith)
		return false
	}
	if S12Serving() {
		return true
	}
	r.logger.Info("this test set was recorded under proven routing (mappings.yaml recordedWith: "+models.RecordedWithS12Routing+"), which this keploy does not enable: its worker scope and pool are served as a set recorded without proven routing, every recorded background call answered exactly and required by the set's end ("+provenswitch.Env+"=1)",
		zap.String("testSetID", testSetID),
		zap.String("next_step", "replay it with the keploy build that recorded it (keploy enterprise), whose judging proven routing rests on"))
	return false
}

// sayRecordedWithoutRouting says, in one line, that testSetID, which no
// proven-routing record marked (models.RecordedWithS12Routing: a set recorded
// before proven routing, with --sync or mapping off, or by a keploy build with
// no judging), is matched exactly under the proven switch, as every set is,
// with every recorded background call required by the set's end; that its
// fields that vary between runs were never learned, since only a
// proven-routing record learns them, so a call whose value of one differs
// from its recording is refused; and how to correct it: record the set again
// so that they are.
func (r *Replayer) sayRecordedWithoutRouting(testSetID string, recordedWith []string) {
	fix := "record the test set again with " + provenswitch.Env + "=1"
	next := fix + ", without --sync and with mapping on: that recording learns its varying fields, routes its background calls and marks its mappings.yaml recordedWith: " + models.RecordedWithS12Routing
	if !S12Serving() {
		fix += " using keploy enterprise"
		next = fix + ", whose judging learns a set's varying fields and routes its background calls: this keploy build marks no set it records"
	}
	r.logger.Info("this test set was recorded without proven routing (its mappings.yaml does not carry recordedWith: "+models.RecordedWithS12Routing+"): under "+provenswitch.Env+"=1 its calls are matched exactly, every recorded background call required by the set's end, and its varying fields (those that vary between runs) were never learned, so a call whose value of one differs from its recording is refused; to correct it, "+fix+" to learn them",
		zap.String("testSetID", testSetID),
		zap.Strings("recordedWith", recordedWith),
		zap.String("next_step", next))
}

// confirmBackground keeps background serving for the staged set only when the agent
// says, answering its staging, that it serves the set by those rules
// (pkg.BackgroundRulesReader, models.BackgroundRulesHeader): an agent that
// predates them, or that runs without the proven switch, serves by today's,
// and the replay then keeps today's checks (its worker scope required by its
// end), never looser than how the set was served. Said once, as a warning.
func (r *Replayer) confirmBackground(asked string) {
	if r.exact == nil || !r.exact.background {
		return
	}
	served := ""
	if br, ok := r.instrumentation.(pkg.BackgroundRulesReader); ok {
		served = br.AgentServesBackground()
	}
	if served == asked {
		return
	}
	r.exact.background = false
	r.logger.Warn("keploy's agent does not serve this test set by the proven routing it was recorded under: the set is judged as one recorded without proven routing, every recorded background call answered exactly and required by its end ("+provenswitch.Env+"=1)",
		zap.String("asked", asked),
		zap.String("served", served),
		zap.String("next_step", "run the keploy agent of this keploy build, with "+provenswitch.Env+"=1"))
}

// returnStaleMoves gives each drift move whose owner this run runs back to
// that owner (the compatibility audit's B4): record's judging
// moved the recording to the worker scope on a drifted pairing
// (models.MockEntry.Drift) and quarantined its owner (MockEntry.Owner) as
// keploy/unseparable_write, keploy being unable to tell the owner's own write
// from it. An owner the run runs (runs) is not quarantined now: its test was
// edited since, so its entry no longer applies, or verdicts.yaml lost it. Run
// beside the moved write, its own write could be answered unchecked, so the
// recording is the owner's own again, in recorded order: today's rule, failing
// closed. A call the recording answered elsewhere then misses, and the miss
// names the owner (markRecordedIn). An exact move, and one whose owner the run
// skips or the set no longer has (cases), stays where judging put it.
//
// It returns mappings and worker as they are when nothing is given back, else
// copies with each returned recording moved, and, by recording, the owner it
// went back to. Under the proven switch, whatever serves the set: it only
// tightens.
func returnStaleMoves(mappings map[string][]models.MockEntry, worker []models.MockEntry, cases []*models.TestCase, runs func(name string) bool) (map[string][]models.MockEntry, []models.MockEntry, map[string]string) {
	has := make(map[string]bool, len(cases))
	for _, tc := range cases {
		if tc != nil {
			has[tc.Name] = true
		}
	}
	var returned map[string]string
	for _, e := range worker {
		if e.Drift && e.Owner != "" && has[e.Owner] && runs(e.Owner) {
			if returned == nil {
				returned = map[string]string{}
			}
			returned[e.Name] = e.Owner
		}
	}
	if len(returned) == 0 {
		return mappings, worker, nil
	}
	outMappings := make(map[string][]models.MockEntry, len(mappings)+1)
	for id, entries := range mappings {
		outMappings[id] = entries
	}
	outWorker := make([]models.MockEntry, 0, len(worker))
	for _, e := range worker {
		owner, back := returned[e.Name]
		if !back {
			outWorker = append(outWorker, e)
			continue
		}
		if slices.ContainsFunc(outMappings[owner], func(o models.MockEntry) bool { return o.Name == e.Name }) {
			continue
		}
		// Back as recorded: no place, no line, no move.
		e.Place, e.Reason, e.Owner, e.Drift = "", "", "", false
		outMappings[owner] = insertByRecordedTime(outMappings[owner], e)
	}
	return outMappings, outWorker, returned
}

// insertByRecordedTime is entries with e inserted before the first entry
// recorded after it (its request time), entries left as they are: a copy.
func insertByRecordedTime(entries []models.MockEntry, e models.MockEntry) []models.MockEntry {
	at := e.RecordedAt()
	i := len(entries)
	for j, o := range entries {
		if o.RecordedAt().After(at) {
			i = j
			break
		}
	}
	out := make([]models.MockEntry, 0, len(entries)+1)
	out = append(out, entries[:i]...)
	out = append(out, e)
	return append(out, entries[i:]...)
}

// returnedWhy says, for the calls of calls whose closest recording
// (models.UnmatchedCall.ClosestMock) returnStaleMoves gave back to its owner,
// why that recording answered none of them: "" for none. A set's failure
// reason ends with it, so the miss names the owner and the remedy.
func (p *exactPlan) returnedWhy(calls []models.UnmatchedCall) string {
	if p == nil || len(p.returned) == 0 {
		return ""
	}
	var parts []string
	seen := map[string]bool{}
	for _, c := range calls {
		owner, ok := p.returned[c.ClosestMock]
		if !ok || seen[c.ClosestMock] {
			continue
		}
		seen[c.ClosestMock] = true
		parts = append(parts, fmt.Sprintf("%s is %s's own again: record's judging moved it to the background on %s's quarantine (keploy/unseparable_write), which no longer applies, as this run runs %s", c.ClosestMock, owner, owner, owner))
	}
	if len(parts) == 0 {
		return ""
	}
	return "; " + strings.Join(parts, "; ") + "; record the test set again"
}

// returnedMissed names, for each test of results with a call no mock answered
// whose closest recording returnStaleMoves gave back to another test, the call
// and that owner: "test: call (mock, given back to owner)".
func (p *exactPlan) returnedMissed(results []models.TestResult) []string {
	if p == nil || len(p.returned) == 0 {
		return nil
	}
	var out []string
	for _, r := range lastResultOfEachTest(results) {
		for _, c := range r.FailureInfo.UnmatchedCalls {
			if owner, ok := p.returned[c.ClosestMock]; ok && owner != r.TestCaseID {
				out = append(out, fmt.Sprintf("%s: %s (%s, given back to %s)", r.TestCaseID, c.ActualSummary, c.ClosestMock, owner))
			}
		}
	}
	return out
}

// lastResultOfEachTest is results with only each test's last row (a test
// retried in another cycle has one row per cycle), in the order each test
// first appears.
func lastResultOfEachTest(results []models.TestResult) []models.TestResult {
	at := make(map[string]int, len(results))
	out := make([]models.TestResult, 0, len(results))
	for _, r := range results {
		if i, seen := at[r.TestCaseID]; seen {
			out[i] = r
			continue
		}
		at[r.TestCaseID] = len(out)
		out = append(out, r)
	}
	return out
}

// logReturnedMoves says, at WARN, each recording returnStaleMoves gave back to
// its owner, and what follows.
func (r *Replayer) logReturnedMoves(testSetID string, returned map[string]string) {
	names := make([]string, 0, len(returned))
	for n := range returned {
		names = append(names, n)
	}
	sort.Strings(names)
	// A build that registers no judging (S12Serving: keploy enterprise
	// registers it) reads no verdicts.yaml: it runs every test, a
	// quarantined owner too.
	judged := S12Serving()
	for _, n := range names {
		owner := returned[n]
		cause := owner + "'s test was edited since the judging, or verdicts.yaml lost its entry"
		next := "record the test set again: a recording re-derives what each test owns, and its judging what moves"
		if !judged {
			cause = "this keploy build reads no verdicts.yaml (it has no judging: keploy enterprise's does), so it runs every test, " + owner + " among them"
			next = "replay the test set with the keploy build that recorded and judged it, which skips " + owner + " while its quarantine applies; or record it again"
		}
		r.logger.Warn("a recording record's judging moved to the test set's background is its test's own again: the move rested on "+owner+"'s quarantine (keploy/unseparable_write), and this run runs "+owner+", so that quarantine no longer applies ("+provenswitch.Env+"=1)",
			zap.String("testSetID", testSetID),
			zap.String("mock", n),
			zap.String("owner", owner),
			zap.String("why", cause+"; run beside the moved write, its own write could be answered unchecked, so "+n+" answers only as "+owner+"'s own, as recorded, and a call it answered elsewhere misses"),
			zap.String("next_step", next))
	}
}

// mappingReader is the part of a mapping store that reads a set's whole
// mappings.yaml, which says which keploy command wrote its test entries
// (models.Mapping.Source): nil for no file.
type mappingReader interface {
	GetMapping(ctx context.Context, testSetID string) (*models.Mapping, error)
}

// windowOwnership is what each test case of testSetID owns when no
// mappings.yaml keploy record wrote says so (why, from provenMappings): the
// test mocks recorded inside its own window, the earliest-opened one where
// two were open (models.DeriveWindowMappings), as keploy record derives a
// recording's mappings.yaml under the switch (S3). A recording made without
// the switch has none; replayed under it, a test case owns what its window
// held, never nothing, and never another test's call. cases are every test
// case of the set, run or not, so a mock of one not selected is no other's.
// stored is every mock read, both pools; skipped the documents the decoders
// could not read (models.TestSetMocks.SkippedWindowed): a test case owns a
// recorded call this keploy cannot serve, and is judged for it. The replay
// never writes what it derives.
func (r *Replayer) windowOwnership(testSetID string, cases []*models.TestCase, why string, skipped []models.WindowedMock, stored ...[]*models.Mock) (map[string][]models.MockEntry, []models.MockEntry) {
	out, worker, _ := r.windowOwnershipOf(testSetID, cases, why, skipped, stored...)
	return out, worker
}

// windowOwnershipOf is windowOwnership, and the concurrency its windows
// derive (models.WindowMappings.Groups): which tests were open together while
// each call was recorded.
func (r *Replayer) windowOwnershipOf(testSetID string, cases []*models.TestCase, why string, skipped []models.WindowedMock, stored ...[]*models.Mock) (map[string][]models.MockEntry, []models.MockEntry, *setConcurrency) {
	windows := make([]models.TestWindow, 0, len(cases))
	for _, tc := range cases {
		windows = append(windows, models.RecordedWindow(tc))
	}
	var mocks []models.WindowedMock
	seen := map[string]bool{}
	for _, list := range stored {
		for _, mk := range list {
			if mk == nil || mk.Name == "" || seen[mk.Name] {
				continue
			}
			seen[mk.Name] = true
			mocks = append(mocks, models.NewWindowedMock(mk))
		}
	}
	for _, w := range skipped {
		if w.Name == "" || seen[w.Name] {
			continue
		}
		seen[w.Name] = true
		mocks = append(mocks, w)
	}
	derived := models.DeriveWindowMappings(windows, mocks)
	out := make(map[string][]models.MockEntry, len(derived.Tests))
	mapped := 0
	for _, tc := range derived.Tests {
		out[tc.ID] = tc.Mocks
		mapped += len(tc.Mocks)
	}
	r.logger.Info("under KEPLOY_PROVEN each test case of this test set owns the calls recorded inside its own window, as keploy record maps a recording made under the switch",
		zap.String("testSetID", testSetID),
		zap.String("reason", why),
		zap.Int("numTests", len(out)),
		zap.Int("ownedMocks", mapped),
		zap.Int("workerMocks", len(derived.Worker)),
		zap.String("next_step", "record the test set again under KEPLOY_PROVEN for a mappings.yaml keploy record writes"))
	if n := len(derived.Concurrent); n > 0 {
		r.logger.Warn("some tests of this test set were recorded while another test's request was still open: a call recorded while two tests were open is owned by the test whose request came first, which may not be the test that made it",
			zap.String("testSetID", testSetID),
			zap.Int("count", n),
			zap.Strings("tests", derived.Concurrent[:min(n, 20)]),
			zap.String("next_step", "if one of these tests fails with a call it did not make or a call no recorded mock answered, record the test set again under KEPLOY_PROVEN, one request at a time"))
	}
	return out, derived.Worker, concurrencyOf(derived.Groups, out, nil)
}

// newExactPlan is the plan for a set whose test cases own mappings, whose
// worker scope is worker, and whose stored mocks are stored (what the agent
// holds): every stored mock no mapping lists is the pool's. A test case owns
// the stored mocks its mapping lists. One it lists that the agent does not
// hold (a connection failure, which keploy never hands it, or one the store
// pruned) answers nothing: the agent is not asked for it, and the test case
// is judged for it all the same, as a recorded call it did not make
// (unconsumedTestMocks reads the whole mapping). The worker scope is the
// stored data-plane mocks it lists that no test case's mapping lists, in its
// order; it is not the pool's.
func newExactPlan(mappings map[string][]models.MockEntry, worker []models.MockEntry, stored ...[]*models.Mock) *exactPlan {
	p := &exactPlan{owned: make(map[string][]string, len(mappings)), repeat: models.RepeatMarks(mappings)}
	// held: the stored mocks by name, true for a test mock (data-plane).
	held := map[string]bool{}
	for _, list := range stored {
		for _, mk := range list {
			if mk != nil && mk.Name != "" {
				held[mk.Name] = !mk.IsProtocolPhase()
				if models.IsAnswerPlaced(mk) {
					if p.answerPlaced == nil {
						p.answerPlaced = map[string]bool{}
					}
					p.answerPlaced[mk.Name] = true
				}
			}
		}
	}
	mapped := map[string]bool{}
	for test, entries := range mappings {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			if e.Name == "" {
				continue
			}
			mapped[e.Name] = true
			dataPlane, ok := held[e.Name]
			if !ok {
				continue
			}
			names = append(names, e.Name)
			p.ownsTestMocks = p.ownsTestMocks || dataPlane
		}
		p.owned[test] = names
	}
	idle := map[string]bool{}
	for _, list := range stored {
		for _, mk := range list {
			if mk != nil && mk.Name != "" && models.IsIdleAnswer(mk) {
				idle[mk.Name] = true
			}
		}
	}
	for _, e := range worker {
		if e.Name == "" || mapped[e.Name] {
			continue
		}
		// A protocol-phase mock is never required. One the agent does not
		// hold is required all the same, as a test case's own is: a
		// recorded call the application never makes fails the set, and one
		// it makes misses. A kind no assertion covers (DNS, a connection
		// failure) is not.
		dataPlane, stored := held[e.Name]
		if (stored && !dataPlane) || (!stored && isUnassertedMockEntry(e, nil)) {
			continue
		}
		if !stored {
			if p.unheld == nil {
				p.unheld = map[string]bool{}
			}
			p.unheld[e.Name] = true
		}
		mapped[e.Name] = true
		p.worker = append(p.worker, e)
		if idle[e.Name] {
			if p.idle == nil {
				p.idle = map[string]bool{}
			}
			p.idle[e.Name] = true
		}
	}
	seen := map[string]bool{}
	for _, list := range stored {
		for _, mk := range list {
			if mk == nil || mk.Name == "" || mapped[mk.Name] || seen[mk.Name] {
				continue
			}
			seen[mk.Name] = true
			p.pool = append(p.pool, mk.Name)
		}
	}
	return p
}

// endLearning ends the set's learn step on the agent from the plan's next
// filter on (models.MockFilterParams.LearnEnded): a retry cycle of record's
// full round runs the passing test cases again out of their recorded order,
// so it is judged and learns nothing. It stays ended for the rest of the set,
// the deferred streaming pass included, which goes back to an earlier window
// too: a set's learning is its first cycle's, and a streaming test case of a
// set that ran retry cycles, whose one run is that pass, learns nothing (one
// whose pass needs drift learned fails there). A set with no retry cycle ends
// it before that pass when a streaming test case was recorded before a test
// case that ran ahead of it: the pass then runs out of recorded order too.
// The retry cycles, and a streaming pass so run out of recorded order, are
// judged with what the round keeps of the first cycle's learning
// (serveFirstCycleLearned). A test whose consume a parser learned
// on all the same once it ended (MockState.LearnedAfterEnd: a parser that
// does not read the end) has no verdict, and its row says why
// (markLearnedAfterEnd): nothing keeps that learning. A no-op on a nil plan
// (no exact serving: no agent learns).
func (p *exactPlan) endLearning() {
	if p != nil {
		p.learnEnded = true
	}
}

// endFirstCycleLearning ends the set's learn step on the agent before its
// first retry cycle (endLearning), with the pool served again ahead of next,
// the cycle's first test case: its filter ends the learn step before the
// agent settles the calls made since the first cycle's last test, so what the
// learn step learned of them is decided by then. It returns what they
// consumed, read now: the first cycle's, kept with what it learned. Without
// exact serving or instrumentation it ends the plan's learn step only, and
// reads nothing.
func (r *Replayer) endFirstCycleLearning(ctx context.Context, plan *exactPlan, next string) ([]models.MockState, error) {
	plan.endLearning()
	if plan == nil || !r.instrument {
		return nil, nil
	}
	if err := r.serveExactPool(ctx, plan, next); err != nil {
		return nil, err
	}
	return r.hookImpl.GetConsumedMocks(ctx)
}

// serveFirstCycleLearned has the agent serve the set's retry cycles what the
// replay keeps of its first cycle's learning (kept, by mock name), or the
// deferred streaming pass of a set with none, run out of recorded order,
// what the set's other test cases learned: the pool served again ahead of
// next, the cycle's (or the pass's) first test case, its filter
// carrying, with the learn step ended, every state of kept that carries
// learning, in name order (models.MockFilterParams.Learned). A test that
// passed in the first cycle only through drift its learn step learned is so
// judged with it in the retry cycles, as the next keploy test is once it is
// persisted. A no-op without exact serving or instrumentation, or when kept
// carries no learning.
func (r *Replayer) serveFirstCycleLearned(ctx context.Context, plan *exactPlan, next string, kept map[string]models.MockState) error {
	if plan == nil || !r.instrument {
		return nil
	}
	learned := carriesLearning(kept)
	if len(learned) == 0 {
		return nil
	}
	return r.serveExactPoolWith(ctx, plan, next, learned)
}

// carriesLearning is, in name order, every state of states that carries
// learned noise, variables, binds or clock reads.
func carriesLearning(states map[string]models.MockState) []models.MockState {
	var out []models.MockState
	for _, st := range states {
		if len(st.ReqBodyNoise) > 0 || len(st.Variables) > 0 || len(st.Binds) > 0 || len(st.Clocks) > 0 {
			out = append(out, st)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// serveExactTest opens test case name on the agent, served exactly its own
// mocks, with what its opening carries (testOpening).
func (r *Replayer) serveExactTest(ctx context.Context, plan *exactPlan, name string, open testOpening) error {
	plan.begin(name)
	plan.testView = name
	ex := &models.ExactServing{Test: name, Owned: append([]string(nil), plan.owned[name]...), Repeat: append([]string(nil), plan.repeat[name]...)}
	// keploy record's full round: the recordings it shares with the tests
	// that ran beside it, after its own.
	ex.Concurrent = plan.concurrentServing(name)
	params := models.MockFilterParams{
		Variables:   open.vars,
		TestRequest: open.request,
		Exact:       ex,
		LearnEnded:  plan.learnEnded,
	}
	err := r.sendFilter(ctx, params)
	if err != nil {
		utils.LogError(r.logger, err, "failed to serve the test case its own mocks", zap.String("testCase", name))
		return err
	}
	return nil
}

// nextExactTest is the test case a replay loop opens after the one that just
// ended: the first of rest it serves (a test selected, when a selection is
// set, neither ignored nor refused). "" when none of rest is: what opens next
// is then not known yet (another retry cycle, or the deferred streaming
// pass), and serveExactAhead names it at its loop head.
func nextExactTest[V any](rest []*models.TestCase, selected, ignored map[string]bool, refused map[string][]V) string {
	for _, tc := range rest {
		if _, ok := selected[tc.Name]; !ok && len(selected) != 0 {
			continue
		}
		if ignored[tc.Name] {
			continue
		}
		if len(refused[tc.Name]) > 0 {
			continue
		}
		return tc.Name
	}
	return ""
}

// serveExactAhead names test case name's calls in flight when it began, at the
// head of the loop that opens it, when the pool call before it did not name
// them: what opens next was not known then (a retry cycle's first test case,
// the streaming pass's), or the one it named did not open (a base path that
// failed). The pool answers them until it opens. A no-op when the latest pool
// call named them, or name has none.
func (r *Replayer) serveExactAhead(ctx context.Context, plan *exactPlan, name string) {
	if plan == nil || plan.aheadNamed == name || len(plan.ahead[name]) == 0 {
		return
	}
	if err := r.serveExactPool(ctx, plan, name); err != nil {
		r.logger.Debug("could not name the next test case's calls in flight when it begins; they answer once it opens",
			zap.String("testCase", name), zap.Error(err))
	}
}

// drainAnsweredInGap reads, as test case name opens, what the gap before it
// answered of its calls in flight: ServeExactTest flags them consumed as it
// opens, and they are read here, before its request is sent, so that a drain
// after the request holds what the request consumed alone. Nothing is read
// when the pool call before it named none of its calls (plan.aheadNamed): no
// answer is flagged at its open.
func (r *Replayer) drainAnsweredInGap(ctx context.Context, plan *exactPlan, name string) ([]models.MockState, error) {
	if plan == nil || plan.aheadNamed != name || len(plan.ahead[name]) == 0 {
		return nil, nil
	}
	return r.hookImpl.GetConsumedMocks(ctx)
}

// serveExactPool serves the set's pool again, once a test case's
// consumption is read: a call the application makes between test cases is
// the pool's, but for one of next's mocks in flight when it begins
// (exactPlan.ahead), which answers as next's. next is the test case the
// replay opens next, "" for none.
func (r *Replayer) serveExactPool(ctx context.Context, plan *exactPlan, next string) error {
	return r.serveExactPoolWith(ctx, plan, next, nil)
}

// serveExactPoolWith is serveExactPool, its filter carrying learned
// (models.MockFilterParams.Learned) when it is not empty.
func (r *Replayer) serveExactPoolWith(ctx context.Context, plan *exactPlan, next string, learned []models.MockState) error {
	if plan == nil {
		return nil
	}
	// Named only once the agent has them: a test case whose naming failed
	// is named again at its loop head (serveExactAhead).
	plan.aheadNamed = ""
	err := r.instrumentation.UpdateMockParams(ctx, models.MockFilterParams{Exact: &models.ExactServing{Ahead: append([]string(nil), plan.ahead[next]...)}, LearnEnded: plan.learnEnded, Learned: learned})
	if err != nil {
		utils.LogError(r.logger, err, "failed to serve the test set's pool between test cases")
		return err
	}
	plan.aheadNamed = next
	plan.testView = ""
	return nil
}

// endStreamOnFailure logs the pool call a streamed test case that failed
// makes as it ends (endStream), when the agent did not take it: the test
// case has failed already, and the next one's loop head serves the pool
// again before it begins (poolBeforeBegin), or stops the set.
func endStreamOnFailure(logger *zap.Logger, name string, err error) {
	if err != nil {
		logger.Warn("keploy could not serve the test set's pool as a failed streaming test case ended; it serves it again before the next test case begins",
			zap.String("testCase", name), zap.Error(err))
	}
}

// poolBeforeBegin serves the pool before test case name begins, when the
// agent may still serve the last test case's own view: a test case that
// ended on an exit that served no pool, or whose pool call the agent did not
// take. Beginning name opens its capture window, and with it the learn
// step's running test on the agent (Proxy.ContinueTestErrorCapture): over
// the last test case's view, the agent's tracker files what that test case
// served as served before name, while the store still answers name's calls
// with the last one's recordings, so a value the last test case's recording
// held read as noise onto one of its own. An error when the pool cannot be
// served: name must not begin then. A no-op for no plan, or when the latest
// pool call succeeded.
func (r *Replayer) poolBeforeBegin(ctx context.Context, plan *exactPlan, name string) error {
	if plan == nil || plan.testView == "" {
		return nil
	}
	last := plan.testView
	if err := r.serveExactPool(ctx, plan, name); err != nil {
		return fmt.Errorf("keploy could not serve the test set's pool after test case %s, so it did not begin test case %s over %s's mocks: %w", last, name, last, err)
	}
	return nil
}

// logStoppedBeforeTest says, where it happens, that keploy stopped a set
// because it did not begin a test case on its agent under exact serving
// (poolBeforeBegin, openTestErrorCapture), and why. It says nothing of the
// set's verdict, which the set's end gives (logNotBegun): an agent that
// refuses while the app runs on is said even when the set ends with the
// app's status, or a failed post-script ends it before its end.
func (r *Replayer) logStoppedBeforeTest(testSetID string, err error) {
	r.logger.Warn("keploy did not begin a test case on its agent, so it stopped the test-set before that test case",
		zap.String("test-set", testSetID),
		zap.String("reason", err.Error()))
}

// logNotBegun says, at the end of a set keploy stopped because it did not
// begin a test case on its agent under exact serving (poolBeforeBegin,
// openTestErrorCapture), that the set is not judged, why, and what to do.
func (r *Replayer) logNotBegun(testSetID string, err error) {
	// Not utils.LogError, which drops an error that wraps context.Canceled:
	// a set marked not judged is always said.
	r.logger.Error("test-set is not judged: keploy did not begin one of its test cases on its agent, so it stopped the set before that test case; the run goes on to the next test-set",
		zap.String("test-set", testSetID),
		zap.String("reason", err.Error()),
		zap.String("next_step", "check the agent's log for why it did not answer and whether it is still running, then run the test-set again"))
}

// noteMappingAlwaysUsed says once per run that disableMapping does not apply
// under the proven switch.
func (r *Replayer) noteMappingAlwaysUsed() {
	r.noteIgnored("disableMapping (--disable-mapping)", "each test case is served exactly the mocks its mapping lists")
}

// The settings exact serving deletes (design v14 §10, U18 rows):
// under the proven switch each is ignored, said once per run, and never
// weakens a verdict. The flip deletes them.

// statefulMocksDisabled is test.disableStatefulMocks, ignored under the
// switch: a test's own mocks answer once each in recorded order, and the
// pool keeps its cursors.
func (r *Replayer) statefulMocksDisabled() bool {
	if !r.config.Test.DisableStatefulMocks {
		return false
	}
	if provenswitch.On() {
		r.noteIgnored("disableStatefulMocks", "a test's own mocks answer once each, in recorded order")
		return false
	}
	return true
}

// removeUnusedMocks is test.removeUnusedMocks, ignored under the switch: a
// passing test leaves no test mock unconsumed, so pruning would only delete
// pool mocks another run may need (preserveFailedMocks goes with it).
func (r *Replayer) removeUnusedMocks() bool {
	if !r.config.Test.RemoveUnusedMocks {
		return false
	}
	if provenswitch.On() {
		r.noteIgnored("removeUnusedMocks (--remove-unused-mocks)", "a passing test leaves none of its mocks unconsumed, and the pool's are other runs' too")
		return false
	}
	return true
}

// noteIgnored says once per run that setting does not apply under the switch.
func (r *Replayer) noteIgnored(setting, why string) {
	if _, said := r.ignoredSaid.LoadOrStore(setting, true); said {
		return
	}
	r.logger.Warn(setting+" is ignored under KEPLOY_PROVEN: "+why,
		zap.String("next_step", "remove it from the configuration or the command line"))
}

// noteOwnsNothing says once per set, under the switch, that no test case of
// testSetID owns a test mock: every call a test case makes to a dependency
// misses.
func (r *Replayer) noteOwnsNothing(testSetID string) {
	if r.exact == nil || r.exact.ownsTestMocks {
		return
	}
	r.logger.Warn("under KEPLOY_PROVEN no test case of this test set owns a recorded call: its mappings.yaml, or with none its test windows, list no data-plane mock the agent holds, so every call a test case makes to a dependency misses",
		zap.String("testSetID", testSetID),
		zap.String("next_step", "record the test set again with this keploy (keploy record writes mappings.yaml)"))
}

// workerStallLimit is the count of wake-ups in a row, with none of the set's
// worker-scope mocks answered, after which the set's end stops waiting for
// them (awaitWorkerScope): the end count's watchdog's (design v14's lead
// decision, 600). It counts wake-ups, not seconds: a starved replay starves
// its watchdog equally, so load alone cannot trip it.
var workerStallLimit = 600

// workerWake is how the wait for the worker scope wakes: once a second. A
// test replaces it.
var workerWake = func(ctx context.Context) (<-chan time.Time, func()) {
	t := time.NewTicker(time.Second)
	return t.C, t.Stop
}

// awaitWorkerScope waits, at the set's end and before its application is
// stopped, until every worker-scope mock the set's end requires answered by
// then has answered (exactPlan.unansweredWorker, those the recording made
// before its stop; D3): a worker makes its calls in its own time, after the
// last test as well. It reads what the agent consumed meanwhile into
// consumed (through add), as the set's end reads it. It returns those still
// unanswered when the run stops waiting: the set's own context ends (its
// application exited, or the user stopped it), or none of them answers
// across workerStallLimit wake-ups in a row. The wake-ups count the
// application's progress on the calls, not keploy's: a call the application
// no longer makes holds the set's end for that many. A read that fails ends
// the wait with its error: what the worker scope answered is then not known,
// and the set is not judged.
func (r *Replayer) awaitWorkerScope(ctx context.Context, plan *exactPlan, consumed map[string]models.MockState, add func([]models.MockState)) ([]string, error) {
	missing := plan.unansweredWorker(consumed, false)
	if len(missing) == 0 {
		return nil, nil
	}
	r.logger.Info("waiting for the test set's worker scope: the calls the application makes on its own that no test owns, which its recording made by the set's end ("+provenswitch.Env+"=1)",
		zap.Int("calls", len(missing)), zap.Strings("mocks", missing[:min(len(missing), 20)]))
	wake, stop := workerWake(ctx)
	defer stop()
	stalls := 0
	for {
		select {
		case <-ctx.Done():
			return missing, nil
		case <-wake:
		}
		read, err := r.hookImpl.GetConsumedMocks(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return missing, nil
			}
			return missing, err
		}
		add(read)
		now := plan.unansweredWorker(consumed, false)
		if len(now) == 0 {
			return nil, nil
		}
		if len(now) < len(missing) {
			stalls = 0
		} else {
			stalls++
		}
		missing = now
		if stalls >= workerStallLimit {
			return missing, nil
		}
	}
}

// workerWhy says what a set's end found of its worker scope: the calls its
// recording made that the application never did.
func workerWhy(missing []string) string {
	return fmt.Sprintf("the application never made %d of the calls the test set's worker scope recorded (the calls no test owns: %s)", len(missing), strings.Join(missing[:min(len(missing), 20)], ", "))
}
