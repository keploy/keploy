package replay

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// A test case owns what its mapping lists; every stored mock no mapping
// lists is the set's pool, once.
func TestNewExactPlan(t *testing.T) {
	m := func(names ...string) []*models.Mock {
		var out []*models.Mock
		for _, n := range names {
			out = append(out, &models.Mock{Name: n})
		}
		return out
	}
	p := newExactPlan(map[string][]models.MockEntry{"test-1": {{Name: "mock-1"}}, "test-2": {}}, nil, m("mock-0", "mock-1"), m("mock-2", "mock-0"))
	if want := map[string][]string{"test-1": {"mock-1"}, "test-2": {}}; !reflect.DeepEqual(p.owned, want) {
		t.Fatalf("owned %v, want %v", p.owned, want)
	}
	if want := []string{"mock-0", "mock-2"}; !reflect.DeepEqual(p.pool, want) {
		t.Fatalf("pool %v, want %v", p.pool, want)
	}
}

// The set's worker scope is the mocks its section lists that no test case's
// mapping lists, in the section's order with their places, and none of them
// is the pool's. One the agent does not hold is in it, required, but not
// staged on the agent.
func TestNewExactPlanKeepsTheWorkerScopeOutOfThePool(t *testing.T) {
	stored := []*models.Mock{{Name: "own"}, {Name: "w-boot"}, {Name: "w-late"}, {Name: "pool"}, {Name: "also-owned"}}
	worker := []models.MockEntry{{Name: "w-late", Place: "test-1"}, {Name: "w-boot"}, {Name: "also-owned"}, {Name: "not-held"}}
	p := newExactPlan(map[string][]models.MockEntry{"test-1": {{Name: "own"}, {Name: "also-owned"}}}, worker, stored)
	if want := []models.MockEntry{{Name: "w-late", Place: "test-1"}, {Name: "w-boot"}, {Name: "not-held"}}; !reflect.DeepEqual(p.worker, want) {
		t.Fatalf("worker scope %+v, want %+v", p.worker, want)
	}
	if want := []models.MockEntry{{Name: "w-late", Place: "test-1"}, {Name: "w-boot"}}; !reflect.DeepEqual(p.staged(), want) {
		t.Fatalf("staged worker scope %+v, want %+v", p.staged(), want)
	}
	if want := []string{"pool"}; !reflect.DeepEqual(p.pool, want) {
		t.Fatalf("pool %v, want %v: a worker-scope mock is not the pool's", p.pool, want)
	}
}

// A test case owns the stored mocks its mapping lists; one the agent does not
// hold (a connection failure, which keploy never hands it, or one the store
// pruned) is not asked of the agent.
func TestAMockTheAgentDoesNotHoldIsNotAskedOfIt(t *testing.T) {
	p := newExactPlan(map[string][]models.MockEntry{"test-1": {{Name: "mock-1"}, {Name: "mock-cf"}, {Name: "mock-gone"}}}, nil, []*models.Mock{{Name: "mock-1", Kind: models.HTTP}})
	if want := map[string][]string{"test-1": {"mock-1"}}; !reflect.DeepEqual(p.owned, want) || !p.ownsTestMocks {
		t.Fatalf("owned %v (test mocks %v), want %v", p.owned, p.ownsTestMocks, want)
	}
	// A mapping of handshakes and DNS lookups alone owns no test mock.
	dns := &models.Mock{Name: "mock-2", Kind: models.DNS}
	if p := newExactPlan(map[string][]models.MockEntry{"test-1": {{Name: "mock-2"}}}, nil, []*models.Mock{dns}); p.ownsTestMocks {
		t.Fatal("a DNS lookup counted as a test mock")
	}
}

// Under the switch keploy test serves each test case exactly its own mocks:
// the set's staging call stages it and hands the agent the pool, each test
// case's call opens it with its mapping, and the call after its consumption
// is read serves the pool again. No call narrows the pool by a recorded
// window. A test case the mapping omits owns none. With the switch off no
// call asks for exact serving.
func TestUnderTheSwitchEachTestCaseIsServedExactly(t *testing.T) {
	for _, on := range []bool{true, false} {
		t.Run(switchName(on), func(t *testing.T) {
			setSwitch(t, on)
			h := newPartialRunHarness(t, 2, 0)
			pinWindows(h)
			h.mocks.filtered = []*models.Mock{{Name: "mock-a", Kind: models.HTTP}, {Name: "mock-boot", Kind: models.HTTP}}
			mappingFile(t, h, map[string][]models.MockEntry{"test-1": mongoEntries("mock-a")})
			h.replayer.hookImpl = prHooks{consumedFor: consumedByWindow(h, map[string][]models.MockState{"test-1": {mongoAt("mock-a", 1010)}})}
			if status := h.run(t); status != models.TestSetStatusPassed {
				t.Fatalf("status %q", status)
			}
			h.instr.mu.Lock()
			params := append([]models.MockFilterParams(nil), h.instr.allParams...)
			h.instr.mu.Unlock()
			if !on {
				for _, p := range params {
					if p.Exact != nil {
						t.Fatalf("switch off: a call asked for exact serving: %+v", p)
					}
				}
				return
			}
			var got []models.ExactServing
			for _, p := range params {
				if p.Exact == nil {
					t.Fatalf("under the switch a call was not exact serving's: %+v", p)
				}
				got = append(got, *p.Exact)
			}
			// The set carries no recordedWith: s12-routing, so its staging
			// says the record lacks proven routing (models.ExactServing.Unrouted).
			want := []models.ExactServing{
				{Stage: true, Pool: []string{"mock-boot"}, Unrouted: models.UnroutedUnmarked},
				{Test: "test-1", Owned: []string{"mock-a"}}, {},
				{Test: "test-2"}, {},
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("calls %+v, want %+v", got, want)
			}
		})
	}
}

// Under the switch a mappings.yaml that cannot be read refuses the set:
// nothing is replayed from a whole pool instead. disableMapping does not
// apply: the mapping is read all the same.
func TestUnderTheSwitchAnUnreadableMappingRefusesTheSet(t *testing.T) {
	setSwitch(t, true)
	h := newPartialRunHarness(t, 2, 0)
	h.replayer.config.DisableMapping = true
	mappingFile(t, h, nil)
	dir := filepath.Join(h.replayer.config.Path, "test-set-0")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "mappings.yaml"), []byte("tests: [unclosed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	status, err := h.replayer.RunTestSet(context.Background(), "test-set-0", "test-run-0", false)
	if err == nil || status != models.TestSetStatusFailed {
		t.Fatalf("status %q, err %v; want the set refused", status, err)
	}
	h.instr.mu.Lock()
	defer h.instr.mu.Unlock()
	for _, p := range h.instr.allParams {
		if p.Exact != nil && p.Exact.Test != "" {
			t.Fatalf("a test case was served from an unreadable mapping: %+v", p)
		}
	}
}

// Under the switch a test case whose mapping names a mock the set does not
// have is run, never served that mock, and failed for it as a recorded call
// it did not make; the rest of the set runs.
func TestUnderTheSwitchAMappedMockTheSetLacksFailsItsTestCase(t *testing.T) {
	setSwitch(t, true)
	h := newPartialRunHarness(t, 2, 0)
	pinWindows(h)
	h.mocks.filtered = []*models.Mock{{Name: "mock-a", Kind: models.HTTP}}
	mappingFile(t, h, map[string][]models.MockEntry{"test-1": mongoEntries("mock-a", "mock-9")})
	h.replayer.hookImpl = prHooks{consumedFor: consumedByWindow(h, map[string][]models.MockState{"test-1": {mongoAt("mock-a", 1010)}})}
	if status := h.run(t); status != models.TestSetStatusFailed {
		t.Fatalf("status %q, want the set failed", status)
	}
	h.instr.mu.Lock()
	var opened []string
	for _, p := range h.instr.allParams {
		if p.Exact != nil && p.Exact.Test != "" {
			opened = append(opened, p.Exact.Test+":"+strings.Join(p.Exact.Owned, ","))
		}
	}
	h.instr.mu.Unlock()
	if want := []string{"test-1:mock-a", "test-2:"}; !reflect.DeepEqual(opened, want) {
		t.Fatalf("opened %v, want %v", opened, want)
	}
	h.report.mu.Lock()
	defer h.report.mu.Unlock()
	got := map[string]models.TestStatus{}
	for _, r := range h.report.results {
		got[r.TestCaseID] = r.Status
	}
	if got["test-1"] != models.TestStatusFailed || got["test-2"] != models.TestStatusPassed {
		t.Fatalf("results %v; want test-1 failed for mock-9 and test-2 passed", got)
	}
}

// The settings exact serving deletes are ignored under the switch, and
// work as before without it: removeUnusedMocks prunes nothing (a passing
// test leaves none of its mocks unconsumed, and the pool's are other runs'),
// and disableStatefulMocks does not turn the pool's cursors off.
func TestUnderTheSwitchTheDeletedSettingsAreIgnored(t *testing.T) {
	for _, on := range []bool{false, true} {
		t.Run(switchName(on), func(t *testing.T) {
			setSwitch(t, on)
			h := newPartialRunHarness(t, 2, 0)
			h.replayer.config.Test.RemoveUnusedMocks = true
			h.replayer.config.Test.DisableStatefulMocks = true
			if status := h.run(t); status != models.TestSetStatusPassed {
				t.Fatalf("status %q", status)
			}
			want := 1
			if on {
				want = 0
			}
			if got := h.mocks.pruneCalls(); got != want {
				t.Fatalf("pruned %d times, want %d", got, want)
			}
			if got := h.replayer.statefulMocksDisabled(); got == on {
				t.Fatalf("disableStatefulMocks read %v", got)
			}
		})
	}
}

// Under the switch a set whose test cases own nothing says so once, with the
// fix: every call a test case makes to a dependency will miss.
func TestUnderTheSwitchASetThatOwnsNothingSaysSo(t *testing.T) {
	setSwitch(t, true)
	h := newPartialRunHarness(t, 2, 0)
	pinWindows(h)
	core, logs := observer.New(zap.WarnLevel)
	h.replayer.logger = zap.New(core)
	mappingFile(t, h, nil)
	_ = h.run(t)
	said := logs.FilterMessageSnippet("no test case of this test set owns a recorded call").All()
	if len(said) != 1 || !strings.Contains(fmt.Sprint(said[0].ContextMap()["next_step"]), "keploy record") {
		t.Fatalf("want one warning with the fix, got %+v", said)
	}
}

// A test case owns, of its mapping, a call in flight when it began only as
// one recorded before its window (models.DeriveWindowMappings gives a test
// nothing else from before it): noteAhead lists those, of the mocks the
// agent holds, for the pool call before it to name.
func TestNoteAheadListsATestCasesMappedCallsInFlight(t *testing.T) {
	at := func(ms int) time.Time { return attrAt(ms) }
	entry := func(name string, ms int) models.MockEntry {
		return models.MockEntry{Name: name, Kind: string(models.HTTP), ReqTimestampMock: models.FormatMockTimestamp(at(ms))}
	}
	mappings := map[string][]models.MockEntry{
		"test-1": {entry("own-1", 1050)},
		"test-2": {entry("fetch", 1500), entry("own-2", 2050), entry("gone", 1600), {Name: "untimed", Kind: string(models.HTTP)}},
	}
	held := []*models.Mock{{Name: "own-1", Kind: models.HTTP}, {Name: "fetch", Kind: models.HTTP}, {Name: "own-2", Kind: models.HTTP}, {Name: "untimed", Kind: models.HTTP}}
	cases := []*models.TestCase{
		{Name: "test-1", Kind: models.HTTP, HTTPReq: models.HTTPReq{Timestamp: at(1000)}, HTTPResp: models.HTTPResp{Timestamp: at(1100)}},
		{Name: "test-2", Kind: models.HTTP, HTTPReq: models.HTTPReq{Timestamp: at(2000)}, HTTPResp: models.HTTPResp{Timestamp: at(2100)}},
	}
	p := newExactPlan(mappings, nil, held)
	p.noteAhead(mappings, cases)
	if got := p.ahead["test-2"]; len(got) != 1 || got[0] != "fetch" {
		t.Fatalf("test-2's calls in flight = %v; want [fetch] (own-2 is in its window, gone is not held, untimed has no time)", got)
	}
	if got := p.ahead["test-1"]; len(got) != 0 {
		t.Fatalf("test-1's calls in flight = %v; want none", got)
	}
}

// A call a parser holds until its answer (models.RegisterAnswerPlaced) is
// placed at its answer (models.PlaceOf): one sent in the gap before a test
// and answered in its window is in the window, not in flight when it began,
// and is not served in the gap: its parser holds it until the test's window
// brings its recording, as the recording's answer came in the test. A call
// not so placed, sent in the gap, is still in flight.
func TestNoteAheadLeavesOutACallPlacedAtItsAnswerInTheWindow(t *testing.T) {
	t.Cleanup(models.RegisterAnswerPlaced(models.HTTP, func(m *models.Mock) bool { return m.Spec.Metadata["poll"] == "yes" }))
	at := func(ms int) time.Time { return attrAt(ms) }
	entry := func(name string, req, res int) models.MockEntry {
		return models.MockEntry{Name: name, Kind: string(models.HTTP), ReqTimestampMock: models.FormatMockTimestamp(at(req)), ResTimestampMock: models.FormatMockTimestamp(at(res))}
	}
	held := func(name string, req, res int, poll bool) *models.Mock {
		m := &models.Mock{Name: name, Kind: models.HTTP, Spec: models.MockSpec{
			Metadata:         map[string]string{"type": "mocks", models.MetadataLifetimeKey: models.MetadataLifetimePerTest},
			ReqTimestampMock: at(req), ResTimestampMock: at(res),
		}}
		if poll {
			m.Spec.Metadata["poll"] = "yes"
		}
		return m
	}
	mappings := map[string][]models.MockEntry{"test-2": {entry("poll", 1500, 2050), entry("fetch", 1600, 2060)}}
	cases := []*models.TestCase{
		{Name: "test-1", Kind: models.HTTP, HTTPReq: models.HTTPReq{Timestamp: at(1000)}, HTTPResp: models.HTTPResp{Timestamp: at(1100)}},
		{Name: "test-2", Kind: models.HTTP, HTTPReq: models.HTTPReq{Timestamp: at(2000)}, HTTPResp: models.HTTPResp{Timestamp: at(2100)}},
	}
	p := newExactPlan(mappings, nil, []*models.Mock{held("poll", 1500, 2050, true), held("fetch", 1600, 2060, false)})
	p.noteAhead(mappings, cases)
	if got := p.ahead["test-2"]; len(got) != 1 || got[0] != "fetch" {
		t.Fatalf("test-2's calls in flight = %v; want [fetch]: the poll is placed at its answer, in the window", got)
	}
}

// The pool call after a test case names the next one the loop opens: not one
// the selection leaves out, nor an ignored or a refused one; after the loop's
// last, the first deferred streaming test case.
func TestNextExactTestIsTheNextOneTheLoopOpens(t *testing.T) {
	tc := func(name string) *models.TestCase { return &models.TestCase{Name: name} }
	rest := []*models.TestCase{tc("a"), tc("b"), tc("c"), tc("d")}
	selected := map[string]bool{"b": true, "c": true, "d": true}
	ignored := map[string]bool{"b": true}
	refused := map[string][]string{"c": {"a placeholder refused"}}
	if got := nextExactTest(rest, selected, ignored, refused); got != "d" {
		t.Fatalf("next = %q; want d", got)
	}
	// After the loop's last served test case what opens next is not known
	// yet (a retry cycle's first, the streaming pass's): serveExactAhead
	// names it once it is.
	if got := nextExactTest(rest[:3], selected, ignored, refused); got != "" {
		t.Fatalf("next after the loop's last = %q; want none", got)
	}
	if got := nextExactTest(nil, nil, nil, refused); got != "" {
		t.Fatalf("next of nothing = %q", got)
	}
}

// consumedReads hands out, read by read, what the agent consumed meanwhile.
type consumedReads struct {
	prHooks
	reads chan []models.MockState
	// err, when set, is every read's error.
	err error
}

func (c consumedReads) GetConsumedMocks(context.Context) ([]models.MockState, error) {
	if c.err != nil {
		return nil, c.err
	}
	select {
	case r := <-c.reads:
		return r, nil
	default:
		return nil, nil
	}
}

// The set's end requires answered every worker-scope mock its run reached the
// place of (D3): one placed at the set's start when keploy started the set's
// application (bootSeen), and one placed after a test case that began. One
// placed after a test case that never began, one whose answer its parser
// declares idle (D4), and one consumed already are not required; nor is one
// placed at the set's start of an application keploy did not start for the
// set. One the recording made as keploy record stopped the application
// (AtStop) is required only once keploy stopped this set's.
func TestTheSetsEndRequiresTheWorkerScopeItsRunReached(t *testing.T) {
	poll := &models.Mock{Name: "w-empty-poll", Kind: models.HTTP}
	unregister := models.RegisterIdleAnswer(models.HTTP, func(m *models.Mock) bool { return m.Name == "w-empty-poll" })
	t.Cleanup(unregister)
	stored := []*models.Mock{{Name: "w-boot"}, {Name: "w-after-1"}, {Name: "w-after-2"}, poll, {Name: "w-done"}, {Name: "w-shutdown"}}
	p := newExactPlan(nil, []models.MockEntry{
		{Name: "w-boot"}, {Name: "w-after-1", Place: "test-1"}, {Name: "w-after-2", Place: "test-2"},
		{Name: "w-empty-poll"}, {Name: "w-done"}, {Name: "w-shutdown", Place: "test-1", AtStop: true},
	}, stored)
	consumed := map[string]models.MockState{"w-done": {Name: "w-done"}}
	if got := p.unansweredWorker(consumed, false); got != nil {
		t.Fatalf("before any test of an application keploy did not start: %v; want none", got)
	}
	p.bootSeen = true
	if got := p.unansweredWorker(consumed, false); !reflect.DeepEqual(got, []string{"w-boot"}) {
		t.Fatalf("before any test: %v; want [w-boot]", got)
	}
	p.begin("test-1")
	if got := p.unansweredWorker(consumed, false); !reflect.DeepEqual(got, []string{"w-boot", "w-after-1"}) {
		t.Fatalf("after test-1 began: %v; want [w-boot w-after-1]", got)
	}
	if got := p.unansweredWorker(consumed, true); !reflect.DeepEqual(got, []string{"w-boot", "w-after-1", "w-shutdown"}) {
		t.Fatalf("once the application was stopped: %v; want [w-boot w-after-1 w-shutdown]", got)
	}
}

// A worker-scope entry naming a mock the agent does not hold is required all
// the same, as a test case's own entry is: a recorded call never made fails
// the set. One of a kind no assertion covers (DNS) is not, nor is a
// protocol-phase mock.
func TestTheWorkerScopeRequiresAnEntryTheAgentDoesNotHold(t *testing.T) {
	// A stored protocol-phase mock (a DNS answer) under an entry that names
	// another kind: the stored mock decides.
	resolved := &models.Mock{Name: "w-resolved", Kind: models.DNS}
	p := newExactPlan(nil, []models.MockEntry{
		{Name: "w-skipped", Kind: string(models.HTTP)}, {Name: "w-dns", Kind: string(models.DNS)}, {Name: "w-resolved", Kind: string(models.HTTP)},
	}, []*models.Mock{resolved})
	p.bootSeen = true
	if got := p.unansweredWorker(nil, false); !reflect.DeepEqual(got, []string{"w-skipped"}) {
		t.Fatalf("required %v; want [w-skipped]", got)
	}
}

// Of the mocks consumed before the set's first test that would be re-armed
// (partitionInitialConsumed), the worker-scope ones are counted: the agent
// spent each for the staging. The others are re-armed as before; with no plan
// nothing moves.
func TestAWorkerMockConsumedBeforeTheFirstTestIsCounted(t *testing.T) {
	p := newExactPlan(nil, []models.MockEntry{{Name: "w-boot"}}, []*models.Mock{{Name: "w-boot"}})
	counted, rearm := p.takeWorker([]models.MockState{{Name: "handshake"}}, []models.MockState{{Name: "w-boot"}, {Name: "probe"}})
	if len(counted) != 2 || counted[1].Name != "w-boot" || len(rearm) != 1 || rearm[0].Name != "probe" {
		t.Fatalf("counted %v, re-armed %v; want w-boot counted and probe re-armed", counted, rearm)
	}
	var none *exactPlan
	if c, r := none.takeWorker(nil, []models.MockState{{Name: "w-boot"}}); len(c) != 0 || len(r) != 1 {
		t.Fatalf("with no plan counted %v, re-armed %v", c, r)
	}
}

// The set's end waits for the worker scope as long as its calls keep coming:
// it reads what the agent consumed at each wake-up, and returns once every
// required one has answered. One that never answers ends the wait after
// workerStallLimit wake-ups in a row with nothing answered, and is reported.
// The set's own context ending (its application exited) ends the wait at
// once. No clock decides it: the test drives the wake-ups.
func TestTheSetsEndWaitsForTheWorkerScopeWhileItProgresses(t *testing.T) {
	prevLimit, prevWake := workerStallLimit, workerWake
	t.Cleanup(func() { workerStallLimit, workerWake = prevLimit, prevWake })
	workerStallLimit = 3
	wakes := make(chan time.Time)
	workerWake = func(context.Context) (<-chan time.Time, func()) { return wakes, func() {} }
	plan := func() *exactPlan {
		p := newExactPlan(nil, []models.MockEntry{{Name: "w-1"}, {Name: "w-2"}, {Name: "w-shutdown", AtStop: true}},
			[]*models.Mock{{Name: "w-1"}, {Name: "w-2"}, {Name: "w-shutdown"}})
		p.bootSeen = true
		return p
	}
	var readErr error
	run := func(r *Replayer, ctx context.Context, p *exactPlan, consumed map[string]models.MockState) <-chan []string {
		out := make(chan []string, 1)
		go func() {
			missing, err := r.awaitWorkerScope(ctx, p, consumed, func(read []models.MockState) {
				for _, m := range read {
					consumed[m.Name] = m
				}
			})
			readErr = err
			out <- missing
		}()
		return out
	}

	t.Run("every call made", func(t *testing.T) {
		reads := make(chan []models.MockState, 4)
		r := &Replayer{logger: zap.NewNop(), hookImpl: consumedReads{reads: reads}}
		done := run(r, context.Background(), plan(), map[string]models.MockState{})
		reads <- []models.MockState{{Name: "w-1"}}
		wakes <- time.Time{} // w-1 answers
		wakes <- time.Time{} // a slow worker: nothing this time, under the limit
		reads <- []models.MockState{{Name: "w-2"}}
		for {
			select {
			case wakes <- time.Time{}:
			case missing := <-done:
				if len(missing) != 0 {
					t.Fatalf("every call was made, yet %v were reported", missing)
				}
				return
			}
		}
	})
	t.Run("a call never made", func(t *testing.T) {
		reads := make(chan []models.MockState, 4)
		r := &Replayer{logger: zap.NewNop(), hookImpl: consumedReads{reads: reads}}
		done := run(r, context.Background(), plan(), map[string]models.MockState{})
		reads <- []models.MockState{{Name: "w-1"}}
		wakes <- time.Time{} // w-1 answers: progress
		for i := 0; i < 3; i++ {
			wakes <- time.Time{}
		}
		if missing := <-done; !reflect.DeepEqual(missing, []string{"w-2"}) {
			t.Fatalf("reported %v; want [w-2]", missing)
		}
	})
	t.Run("the application exits", func(t *testing.T) {
		r := &Replayer{logger: zap.NewNop(), hookImpl: consumedReads{reads: make(chan []models.MockState)}}
		ctx, cancel := context.WithCancel(context.Background())
		done := run(r, ctx, plan(), map[string]models.MockState{})
		cancel()
		if missing := <-done; !reflect.DeepEqual(missing, []string{"w-1", "w-2"}) {
			t.Fatalf("reported %v; want [w-1 w-2]", missing)
		}
	})
	t.Run("nothing required", func(t *testing.T) {
		r := &Replayer{logger: zap.NewNop(), hookImpl: consumedReads{reads: make(chan []models.MockState)}}
		consumed := map[string]models.MockState{"w-1": {}, "w-2": {}}
		// The shutdown's call is not waited for: the application makes it
		// only once it is stopped.
		if missing := <-run(r, context.Background(), plan(), consumed); missing != nil {
			t.Fatalf("reported %v with every call made", missing)
		}
	})
	t.Run("a read fails", func(t *testing.T) {
		r := &Replayer{logger: zap.NewNop(), hookImpl: consumedReads{reads: make(chan []models.MockState), err: errors.New("the agent did not answer")}}
		done := run(r, context.Background(), plan(), map[string]models.MockState{})
		wakes <- time.Time{}
		<-done
		if readErr == nil {
			t.Fatal("a failed read was not returned: the calls would be reported as never made")
		}
	})
}

// The set's retry cycles, and a streaming pass run out of recorded order,
// are sent every kept state that carries learning, in name order: learned
// noise, learned variables, or only binds, the variables other mocks bind at
// it, whose uses would lose their bind without it (a false red), or only a
// clock read, which the retry cycle checks against its call's window (sent
// none, it would answer the call with the clock read unchecked). A state that
// carries none is not sent.
func TestTheLearningServedAfterTheLearnStepEndedIsEveryStateThatCarriesSome(t *testing.T) {
	states := map[string]models.MockState{
		"e-clock": {Name: "e-clock", Clocks: map[string]models.ClockOffset{"body.at": {Res: time.Millisecond}}},
		"d-none":  {Name: "d-none"},
		"c-binds": {Name: "c-binds", Binds: map[string]models.MockVariables{"mock-0": {"id_1": {Recorded: "u-1", Bind: "body.id"}}}},
		"b-vars":  {Name: "b-vars", Variables: models.MockVariables{"v": {Recorded: "x"}}},
		"a-noise": {Name: "a-noise", ReqBodyNoise: map[string][]string{"body.at": {}}},
	}
	var got []string
	for _, st := range carriesLearning(states) {
		got = append(got, st.Name)
	}
	if want := []string{"a-noise", "b-vars", "c-binds", "e-clock"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the states sent with the learn step ended are %v; want %v (a binds-only and a clock-only state among them)", got, want)
	}
}
