package replay

import (
	"context"
	"fmt"
	"strings"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// Ownership and exact consumption for keploy test, under the proven switch
// (design v14 §5.2, S12): the replay hands the agent what each test case
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
	owned map[string][]string
	pool  []string
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
}

// unansweredWorker is, in recorded order, the worker-scope mocks the set's
// end requires answered and consumed does not hold: those whose place the
// run reached (the set's start, or a test case that has begun), but for the
// ones whose answer is idle (D3, D4).
func (p *exactPlan) unansweredWorker(consumed map[string]models.MockState) []string {
	if p == nil {
		return nil
	}
	var out []string
	for _, e := range p.worker {
		if (e.Place != "" && !p.begun[e.Place]) || p.idle[e.Name] {
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
	const absent = "the test set has no mappings.yaml"
	if r.mappingDB == nil {
		return map[string][]models.MockEntry{}, nil, absent, nil
	}
	if mr, ok := r.mappingDB.(mappingReader); ok {
		m, err := mr.GetMapping(ctx, testSetID)
		if err != nil {
			return nil, nil, "", fmt.Errorf("keploy cannot judge each test case of %s: its mappings.yaml cannot be read: %w; record the set again", testSetID, err)
		}
		if m != nil && m.Source == models.MappingSourceKeployTest {
			return map[string][]models.MockEntry{}, nil, "the test set's mappings.yaml was written by keploy test, from what a replay consumed, not by keploy record", nil
		}
		mappings := map[string][]models.MockEntry{}
		var worker []models.MockEntry
		if m != nil {
			for _, tc := range m.TestCases {
				mappings[tc.ID] = tc.Mocks
			}
			worker = m.Worker
		}
		return mappings, worker, absent, nil
	}
	mappings, _, err := r.mappingDB.Get(ctx, testSetID)
	if err != nil {
		return nil, nil, "", fmt.Errorf("keploy cannot judge each test case of %s: its mappings.yaml cannot be read: %w; record the set again", testSetID, err)
	}
	if mappings == nil {
		mappings = map[string][]models.MockEntry{}
	}
	return mappings, nil, absent, nil
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
	return out, derived.Worker
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
	p := &exactPlan{owned: make(map[string][]string, len(mappings))}
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
		if e.Name == "" || mapped[e.Name] || !held[e.Name] {
			continue
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

// serveExactTest opens test case name on the agent, served exactly its own
// mocks; vars are the test variables its request carries.
func (r *Replayer) serveExactTest(ctx context.Context, plan *exactPlan, name string, vars ...models.VariableBinding) error {
	plan.begin(name)
	err := r.instrumentation.UpdateMockParams(ctx, models.MockFilterParams{
		Variables: vars,
		Exact:     &models.ExactServing{Test: name, Owned: append([]string(nil), plan.owned[name]...)},
	})
	if err != nil {
		utils.LogError(r.logger, err, "failed to serve the test case its own mocks", zap.String("testCase", name))
	}
	return err
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
	if plan == nil {
		return nil
	}
	// Named only once the agent has them: a test case whose naming failed
	// is named again at its loop head (serveExactAhead).
	plan.aheadNamed = ""
	err := r.instrumentation.UpdateMockParams(ctx, models.MockFilterParams{Exact: &models.ExactServing{Ahead: append([]string(nil), plan.ahead[next]...)}})
	if err != nil {
		utils.LogError(r.logger, err, "failed to serve the test set's pool between test cases")
		return err
	}
	plan.aheadNamed = next
	return nil
}

// noteMappingAlwaysUsed says once per run that disableMapping does not apply
// under the proven switch.
func (r *Replayer) noteMappingAlwaysUsed() {
	r.noteIgnored("disableMapping (--disable-mapping)", "each test case is served exactly the mocks its mapping lists")
}

// The settings exact serving deletes (design v14 §10, U18 rows of S12):
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
// stopped, until every worker-scope mock the set's end requires answered has
// answered (exactPlan.unansweredWorker; D3): a worker makes its calls in its
// own time, after the last test as well. It reads what the agent consumed
// meanwhile into consumed (through add), as the set's end reads it. It
// returns those still unanswered when the run stops waiting: the set's own
// context ends (its application exited, or the user stopped it), a read
// fails, or none of them answers across workerStallLimit wake-ups in a row.
// No clock decides it.
func (r *Replayer) awaitWorkerScope(ctx context.Context, plan *exactPlan, consumed map[string]models.MockState, add func([]models.MockState)) []string {
	missing := plan.unansweredWorker(consumed)
	if len(missing) == 0 {
		return nil
	}
	r.logger.Info("waiting for the test set's worker scope: the calls the application makes on its own that no test owns, which its recording made by the set's end ("+provenswitch.Env+"=1)",
		zap.Int("calls", len(missing)), zap.Strings("mocks", missing[:min(len(missing), 20)]))
	wake, stop := workerWake(ctx)
	defer stop()
	stalls := 0
	for {
		select {
		case <-ctx.Done():
			return missing
		case <-wake:
		}
		read, err := r.hookImpl.GetConsumedMocks(ctx)
		if err != nil {
			r.logger.Warn("keploy could not read what the test set's worker scope answered; its calls still unanswered are reported as not made",
				zap.Error(err))
			return missing
		}
		add(read)
		now := plan.unansweredWorker(consumed)
		if len(now) == 0 {
			return nil
		}
		if len(now) < len(missing) {
			stalls = 0
		} else {
			stalls++
		}
		missing = now
		if stalls >= workerStallLimit {
			return missing
		}
	}
}

// workerWhy says what a set's end found of its worker scope: the calls its
// recording made that the application never did.
func workerWhy(missing []string) string {
	return fmt.Sprintf("the application never made %d of the calls the test set's worker scope recorded (the calls no test owns: %s)", len(missing), strings.Join(missing[:min(len(missing), 20)], ", "))
}
