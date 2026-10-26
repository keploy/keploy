package replay

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/utils"
)

// The compose bring-up retry (pkg/client/app) replaces the injected keploy-agent
// whenever a dependency crash forces the stack down and up again, and the
// replacement boots empty — taking the session's stored mocks with it and
// leaving a zero-test run that reports success (#4614).
// ensureAgentHoldsStoredMocks is the guard: before the first test fires it reads
// the agent's non-draining loaded count, re-registers the session when the agent
// no longer holds the corpus, and fails the test set loudly when the
// re-registration cannot be confirmed.
//
// These tests drive the helper against a fake Instrumentation, pinning the
// predicate and the re-registration sequence; the call site (one call in
// RunTestSet's compose branch, after the app-ready wait) is driven end to end
// through the compose harness (composeHarness) by the capture-loss test of an
// agent replaced under a compose app that ran.

// rearmInstr is the fake the guard talks to. It serves the loaded count the
// scenario wants /mock/stats to report and records every repair call, so the
// tests can assert the re-registration sequence was walked in full.
type rearmInstr struct {
	Instrumentation // embedded: nil, so any unexpected call panics loudly

	mu sync.Mutex

	// loaded is what /mock/stats reports. storeTakesEffect simulates the store
	// landing on the agent it was sent to; with it false the count never moves,
	// standing in for a re-registration that cannot be confirmed.
	loaded           int
	storeTakesEffect bool

	// statsErrCalls is how many leading GetMockStats calls fail with a
	// transport error; later calls answer normally.
	statsErrCalls int

	// statsUnsupported makes every GetMockStats answer
	// models.ErrMockStatsUnsupported — an agent older than /mock/stats, or one
	// whose service has no reader. Distinct from a transport error: the agent
	// is reachable and simply cannot answer this question, ever.
	statsUnsupported bool

	// afterFirstStats runs once, after the first GetMockStats answer, so a test
	// can change how later reads behave.
	afterFirstStats func()

	// lastParams is what the most recent UpdateMockParams carried, so a test can
	// assert what the repair actually sent.
	lastParams models.MockFilterParams

	// statsBlock / outgoingBlock, when non-nil, block that call until closed or
	// until the call's own context expires — a WEDGED agent, not a dead one.
	// outgoingBlock matters because bounding only the probe would move the hang
	// into the repair, where every call shares the same timeout-less client.
	statsBlock    chan struct{}
	outgoingBlock chan struct{}

	// onOutgoing runs inside MockOutgoing, so a test can observe state as it was
	// at that moment rather than after the repair finished.
	onOutgoing func()

	// outgoingErr, when set, is what MockOutgoing answers: an agent that is
	// gone refuses the repair's first call.
	outgoingErr error

	// outgoingOpts is what the last MockOutgoing was sent.
	outgoingOpts models.OutgoingOptions

	statsCalls        int
	mockOutgoingCalls int
	storeMocksCalls   int
	updateParamsCalls int
	readyCalls        int
	// readyErr is what MakeAgentReadyForDockerCompose answers.
	readyErr error
}

func (f *rearmInstr) GetMockStats(ctx context.Context) (models.MockStats, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statsCalls++
	block := f.statsBlock
	if block != nil {
		f.mu.Unlock()
		select {
		case <-block:
		case <-ctx.Done():
			f.mu.Lock()
			return models.MockStats{}, ctx.Err()
		}
		f.mu.Lock()
	}
	if f.statsUnsupported {
		return models.MockStats{}, fmt.Errorf("%w: agent returned 501", models.ErrMockStatsUnsupported)
	}
	if f.statsErrCalls > 0 {
		f.statsErrCalls--
		return models.MockStats{}, errors.New("connection refused")
	}
	out := models.MockStats{Loaded: f.loaded}
	if f.afterFirstStats != nil {
		fn := f.afterFirstStats
		f.afterFirstStats = nil
		fn()
	}
	return out, nil
}

func (f *rearmInstr) MockOutgoing(ctx context.Context, opts models.OutgoingOptions) error {
	f.mu.Lock()
	block := f.outgoingBlock
	f.outgoingOpts = opts
	f.mu.Unlock()
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if f.onOutgoing != nil {
		f.onOutgoing()
	}
	return f.mockOutgoing()
}

func (f *rearmInstr) mockOutgoing() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mockOutgoingCalls++
	return f.outgoingErr
}

func (f *rearmInstr) StoreMocks(_ context.Context, filtered []*models.Mock, unfiltered []*models.Mock) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.storeMocksCalls++
	if f.storeTakesEffect {
		f.loaded = len(filtered) + len(unfiltered)
	}
	return nil
}

func (f *rearmInstr) UpdateMockParams(_ context.Context, p models.MockFilterParams) error {
	f.mu.Lock()
	f.lastParams = p
	f.mu.Unlock()
	return f.updateMockParams()
}

func (f *rearmInstr) updateMockParams() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updateParamsCalls++
	return nil
}

func (f *rearmInstr) MakeAgentReadyForDockerCompose(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.readyCalls++
	return f.readyErr
}

// counts returns stats / MockOutgoing / StoreMocks / UpdateMockParams / ready
// call counts.
func (f *rearmInstr) counts() (int, int, int, int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.statsCalls, f.mockOutgoingCalls, f.storeMocksCalls, f.updateParamsCalls, f.readyCalls
}

// rearmHooks counts BeforeTestSetCompose so a test can prove the repair
// re-runs it. The embedded nil TestHooks makes any other call panic loudly.
type rearmHooks struct {
	TestHooks
	mu        sync.Mutex
	before    int
	lastRun   string
	lastSet   string
	lastFirst bool
}

func (h *rearmHooks) BeforeTestSetCompose(_ context.Context, testRunID, testSetID string, firstRun bool) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.before++
	h.lastRun, h.lastSet, h.lastFirst = testRunID, testSetID, firstRun
	return nil
}

func (h *rearmHooks) lastArgs() (string, string, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.lastRun, h.lastSet, h.lastFirst
}

func (h *rearmHooks) beforeCalls() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.before
}

func newRearmReplayer(f *rearmInstr) *Replayer {
	r, _ := newRearmReplayerWithHooks(f)
	return r
}

func newRearmReplayerWithHooks(f *rearmInstr) (*Replayer, *rearmHooks) {
	h := &rearmHooks{}
	return &Replayer{logger: zap.NewNop(), config: &config.Config{}, instrument: true, instrumentation: f, hookImpl: h}, h
}

// rearmSessionArgs is a two-mock corpus with one test case (the arguments the
// compose branch passes through from its setup).
func rearmSessionArgs() ([]*models.Mock, []*models.Mock, map[string]models.MockState, []*models.TestCase) {
	return []*models.Mock{{Name: "mock-a"}, {Name: "mock-b"}}, []*models.Mock{},
		map[string]models.MockState{}, []*models.TestCase{{}}
}

func TestAgentHoldsStoredCorpus(t *testing.T) {
	cases := []struct {
		name           string
		stored, loaded int
		want           bool
	}{
		{"nothing stored is never evidence of a lost agent", 0, 0, true},
		{"an agent holding anything while nothing was stored is fine", 0, 5, true},
		{"zero loaded while a corpus was stored is the replacement signature", 3, 0, false},
		{"the stored count itself proves the store landed", 3, 3, true},
		// Deliberately loose: the count is only ever written by our own store,
		// so any non-zero value means it landed on THIS agent process. A count
		// below the stored total can never trigger a needless re-registration.
		{"a non-zero count below the stored total still proves the store landed", 3, 1, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := agentHoldsStoredCorpus(c.stored, c.loaded); got != c.want {
				t.Fatalf("agentHoldsStoredCorpus(stored=%d, loaded=%d) = %v, want %v", c.stored, c.loaded, got, c.want)
			}
		})
	}
}

// A replacement agent reports zero loaded mocks; the guard must walk the whole
// re-registration sequence and confirm it.
func TestEnsureAgentHoldsStoredMocks_ReRegistersReplacementAgent(t *testing.T) {
	f := &rearmInstr{storeTakesEffect: true}
	r := newRearmReplayer(f)
	filtered, unfiltered, consumed, cases := rearmSessionArgs()

	if err := r.ensureAgentHoldsStoredMocks(context.Background(), "run-1", "test-set-0", models.OutgoingOptions{}, filtered, unfiltered, consumed, false, cases); err != nil {
		t.Fatalf("re-registration against the replacement agent should succeed, got: %v", err)
	}
	// A re-registration of the set, not a set boundary: what the app
	// consumed as it started is the set's own, and the agent keeps it.
	f.mu.Lock()
	starts := f.outgoingOpts.StartsTestSet
	f.mu.Unlock()
	if starts {
		t.Fatal("the repair's MockOutgoing said it starts a test set; the agent would drop the set's own bootstrap consumes")
	}

	stats, outgoing, stores, updates, ready := f.counts()
	if outgoing != 1 || stores != 1 || updates != 1 || ready != 1 {
		t.Fatalf("expected the full sequence once (MockOutgoing, StoreMocks, UpdateMockParams, ready), got outgoing=%d stores=%d updates=%d ready=%d",
			outgoing, stores, updates, ready)
	}
	if stats != 2 {
		t.Fatalf("expected the probe plus the confirmation read (2 stats calls), got %d", stats)
	}
}

// rearmLossInstr is rearmInstr whose capture-loss reads name the agent
// process that answered them (the keploy agent client's do): the replacement,
// agent, holding losses.
type rearmLossInstr struct {
	*rearmInstr
	agent  string
	losses models.CaptureLosses
	// beforeApp is the agent process the compose bring-up's retry started
	// in place of the one before while the app had never started
	// (AgentStartedBeforeApp); "" for none.
	beforeApp string
}

func (f *rearmLossInstr) AgentStartedBeforeApp(instance string) bool {
	return instance != "" && instance == f.beforeApp
}

func (f *rearmLossInstr) GetCaptureLosses(ctx context.Context) (models.CaptureLosses, error) {
	l, _, err := f.GetCaptureLossesAndAgent(ctx)
	return l, err
}

func (f *rearmLossInstr) GetCaptureLossesAndAgent(context.Context) (models.CaptureLosses, string, error) {
	return f.losses, f.agent, nil
}

// Under the proven switch, the capture-loss counts of a replacement agent the
// compose bring-up's retry started while the application had never started
// are all the set's: its tests are charged what it lost from its start, the
// app's own startup included, and judged on it, though the set's mark, and
// the application's and the run's bases, were read from the agent it
// replaced. That one had counted as many of each kind as the replacement
// holds, so a base kept from it would count nothing of the replacement's: a
// kind lost on the way, for each base (the set's mark, the application's
// start, the run's start, and what keploy cannot capture at all). So whether
// the retry's own setup took (the stored-mocks check finds the mocks held)
// or not (it stores them again). Any other replacement (one docker started
// again under an application that had started) took what it counted with
// it, wherever it was noticed: the bases stay its, and the test is charged
// the agent started again, which leaves it without a verdict.
func TestAgentStartedBeforeApp_CountsTheRetrysReplacementsLossesFromItsStart(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	// One of each base's kinds: the set's mark (a parser stall), the
	// application's start (a refused process of the application's), the
	// run's start (one that predates it) and what keploy cannot capture at
	// all (a Unix socket).
	lost := models.CaptureLosses{
		{Kind: models.CaptureLossParserStall, Count: 1, What: "to 10.0.0.9:5432"},
		{Kind: models.CaptureLossPIDMap, Count: 1, What: "pid 41"},
		{Kind: models.CaptureLossPIDMapExcluded, Count: 1, What: "pid 12"},
		{Kind: models.CaptureLossUnixSocket, Count: 1, What: "/run/app.sock"},
	}
	// What the agent replaced had counted at each of those bases: as many
	// of each kind as the replacement holds.
	a1 := models.CaptureLosses{
		{Kind: models.CaptureLossParserStall, Count: 1, What: "to 10.0.0.9:5432"},
		{Kind: models.CaptureLossPIDMap, Count: 1, What: "pid 7"},
		{Kind: models.CaptureLossPIDMapExcluded, Count: 1, What: "pid 3"},
		{Kind: models.CaptureLossUnixSocket, Count: 1, What: "/run/app.sock"},
	}
	startedAgain := append(models.CaptureLosses{agentStartedAgain(models.AgentReplacement{First: "a1", Next: "b2"})}, lost...)
	for _, tc := range []struct {
		name      string
		beforeApp string
		held      bool
		want      models.CaptureLosses
	}{
		{"the retry's, set up again by it", "b2", true, lost},
		{"the retry's, its setup not taken", "b2", false, lost},
		{"not the retry's, found holding the mocks", "", true, startedAgain},
		{"not the retry's, found holding none", "", false, startedAgain},
		{"another the retry started", "c3", false, startedAgain},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &rearmLossInstr{rearmInstr: &rearmInstr{storeTakesEffect: true}, agent: "b2", losses: lost, beforeApp: tc.beforeApp}
			filtered, unfiltered, consumed, cases := rearmSessionArgs()
			if tc.held {
				f.loaded = len(filtered) + len(unfiltered)
			}
			r := newRearmReplayer(f.rearmInstr)
			r.instrumentation = f
			r.lossMark, r.lossMarked = captureLossMark{at: a1, known: true, agent: "a1"}, true
			r.lossAppBase, r.lossRunStart, r.lossRunBase = a1, a1, a1
			r.lossAgent, r.lossBasesAgent = "a1", "a1"
			if err := r.ensureAgentHoldsStoredMocks(context.Background(), "run-1", "test-set-0", models.OutgoingOptions{}, filtered, unfiltered, consumed, false, cases); err != nil {
				t.Fatal(err)
			}
			r.agentStartedBeforeApp(context.Background(), nil)
			charged, err := r.testCaptureLosses(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			sort.Slice(tc.want, func(i, j int) bool { return tc.want[i].Kind < tc.want[j].Kind })
			if !reflect.DeepEqual(charged, tc.want) {
				t.Fatalf("the first test is charged %+v; want %+v", charged, tc.want)
			}
		})
	}
}

// Under the proven switch, a set whose mark and bases were read from the
// compose bring-up retry's replacement already (its stack came up after the
// retry, and was marked once it was up) is counted from that mark: what the
// replacement counted before it, before the application started, is not the
// set's tests', and the bases stay as they are. One of each base's kinds,
// each counted before the mark.
func TestAgentStartedBeforeApp_LeavesBasesReadFromTheRetrysReplacementAsTheyAre(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	before := models.CaptureLosses{
		{Kind: models.CaptureLossParserStall, Count: 1, What: "to 10.0.0.9:5432"},
		{Kind: models.CaptureLossPIDMap, Count: 1, What: "pid 41"},
		{Kind: models.CaptureLossPIDMapExcluded, Count: 1, What: "pid 12"},
		{Kind: models.CaptureLossUnixSocket, Count: 1, What: "/run/app.sock"},
	}
	f := &rearmLossInstr{rearmInstr: &rearmInstr{storeTakesEffect: true}, agent: "b2", losses: before, beforeApp: "b2"}
	r := newRearmReplayer(f.rearmInstr)
	r.instrumentation = f
	r.lossMark, r.lossMarked = captureLossMark{at: before, known: true, agent: "b2"}, true
	r.lossAppBase, r.lossRunStart, r.lossRunBase = before, before, before
	r.lossAgent, r.lossBasesAgent = "b2", "b2"
	r.agentStartedBeforeApp(context.Background(), nil)
	if !reflect.DeepEqual(r.lossBases(), models.LossBases{Set: before, App: before, Run: before, Capture: before}) {
		t.Fatalf("the bases became %+v; want them left at the mark", r.lossBases())
	}
	charged, err := r.testCaptureLosses(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(charged) != 0 {
		t.Fatalf("the first test is charged %+v of what the agent counted before the set's mark; want nothing", charged)
	}
}

// Under the proven switch, the log that says the compose bring-up retry's
// replacement is counted from its start names the agent it replaced from the
// set's bases when the set's mark does not name it: the mark's read failed
// (it names no agent), or the mark was read from the replacement while the
// application's and the run's bases were read from the agent before it. The
// set is counted from the replacement's start either way.
func TestAgentStartedBeforeApp_NamesTheAgentReplacedFromTheBasesWhenTheMarkDoesNot(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	lost := models.CaptureLosses{{Kind: models.CaptureLossPIDMap, Count: 1, What: "pid 41"}}
	a1 := models.CaptureLosses{{Kind: models.CaptureLossPIDMap, Count: 1, What: "pid 7"}}
	// lossAgent is what a real run holds by then: a mark read from b2 notes b2
	// (markCaptureLosses calls noteLossAgent), and a failed read notes nothing.
	for _, tc := range []struct {
		name      string
		mark      captureLossMark
		lossAgent string
	}{
		{"the set's mark read failed", captureLossMark{err: errors.New("the agent did not answer")}, "a1"},
		{"the set's mark read from the replacement", captureLossMark{at: lost, known: true, agent: "b2"}, "b2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &rearmLossInstr{rearmInstr: &rearmInstr{storeTakesEffect: true}, agent: "b2", losses: lost, beforeApp: "b2"}
			r := newRearmReplayer(f.rearmInstr)
			r.instrumentation = f
			core, logs := observer.New(zapcore.InfoLevel)
			r.logger = zap.New(core)
			r.lossMark, r.lossMarked = tc.mark, true
			r.lossAppBase, r.lossRunStart, r.lossRunBase = a1, a1, a1
			r.lossAgent, r.lossBasesAgent = tc.lossAgent, "a1"
			r.agentStartedBeforeApp(context.Background(), nil)
			said := logs.FilterMessageSnippet("bring-up retry before the application ever ran").
				FilterField(zap.String("agent replaced", "a1")).FilterField(zap.String("agent", "b2"))
			if said.Len() != 1 {
				t.Fatalf("%d of %d logs say the retry's agent b2 counts from its start, naming a1 as the agent it replaced; want 1: %+v",
					said.Len(), logs.Len(), logs.AllUntimed())
			}
			charged, err := r.testCaptureLosses(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(charged, lost) {
				t.Fatalf("the first test is charged %+v; want %+v, all b2 lost from its start", charged, lost)
			}
		})
	}
}

// replacedAtCheckInstr is a compose stack kept across sets (composeInstr)
// whose agent names itself in its capture-loss reads, as the keploy agent
// client's do. Its counts become lost from the read numbered lossAt (from 1)
// on. At the first stored-mocks check after replaceAtCheck is set, the agent
// is found replaced by b2 (docker starting again an agent container that
// crashed): b2 holds no mocks, and counts nothing of a1's. With byRetry, b2
// is one the compose bring-up's retry started while the app had never
// started, whose setup there did not take (AgentStartedBeforeApp).
type replacedAtCheckInstr struct {
	*composeInstr
	mu             sync.Mutex
	agent          string
	reads, lossAt  int
	lost, counts   models.CaptureLosses
	loaded         int
	replaceAtCheck bool
	replaced       bool
	byRetry        bool
}

func (f *replacedAtCheckInstr) AgentStartedBeforeApp(instance string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.byRetry && f.replaced && instance == "b2"
}

func (f *replacedAtCheckInstr) GetCaptureLosses(ctx context.Context) (models.CaptureLosses, error) {
	l, _, err := f.GetCaptureLossesAndAgent(ctx)
	return l, err
}

func (f *replacedAtCheckInstr) GetCaptureLossesAndAgent(context.Context) (models.CaptureLosses, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	if !f.replaced && f.lossAt > 0 && f.reads >= f.lossAt {
		f.counts = f.lost
	}
	return f.counts, f.agent, nil
}

func (f *replacedAtCheckInstr) GetMockStats(context.Context) (models.MockStats, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.replaceAtCheck {
		f.replaceAtCheck, f.replaced = false, true
		f.agent, f.counts, f.loaded = "b2", nil, 0
	}
	return models.MockStats{Loaded: f.loaded}, nil
}

func (f *replacedAtCheckInstr) StoreMocks(ctx context.Context, filtered, unfiltered []*models.Mock) error {
	f.mu.Lock()
	f.loaded = len(filtered) + len(unfiltered)
	f.mu.Unlock()
	return f.composeInstr.StoreMocks(ctx, filtered, unfiltered)
}

// Under the proven switch, keploy's agent found replaced as a set of a compose
// stack kept across sets brings it up, once the stack's app has run, is no
// replacement compose's bring-up retry made (that runs only while the app's
// container was never started): the app ran against the agent replaced, and
// may run on with a process that agent could not place. Here set 1's stop of
// the app fails, so the app runs on into set 2 with a process a1 counted
// (pid_map_refused), and set 2's start of it is no start anew. Set 2 is not
// judged when b2 is found in a1's place: each of its tests is charged the
// agent started again, rather than passing on b2's empty counts. Set 3,
// whose app keploy starts anew under b2, is judged on b2's counts. Where the
// bring-up's retry replaced the agent in the stack's first bring-up, before
// its app ever ran, the replacement's counts are all the set's, and the set
// is judged.
func TestUnderTheSwitchAnAgentReplacedUnderAComposeAppThatRanLeavesTheSetUnjudged(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	lost := models.CaptureLosses{{Kind: models.CaptureLossPIDMap, Count: 1, What: "pid 41"}}
	harness := func(t *testing.T, stopErr error) (*prRun, *composeInstr, *replacedAtCheckInstr) {
		c := &composeInstr{stopErr: stopErr}
		h := composeHarness(t, c)
		h.mocks.filtered = []*models.Mock{{Name: "mock-a", Kind: models.HTTP}}
		// Reads: set 1's mark, then one as each of its tests ends.
		f := &replacedAtCheckInstr{composeInstr: c, agent: "a1", lossAt: 3, lost: lost}
		h.replayer.instrumentation = f
		h.replayer.config.Test.Mocking = true
		return h, c, f
	}
	unjudged := func(h *prRun) int {
		n := 0
		for _, r := range h.report.report.Tests {
			for _, cc := range r.FailureInfo.CannotCapture {
				if cc.Kind == models.CaptureLossAgentStartedAgain && cc.What == "agent process a1, then b2" {
					n++
				}
			}
		}
		return n
	}
	for _, replaced := range []bool{false, true} {
		name := "the agent is not replaced"
		if replaced {
			name = "the agent is replaced as set 2 brings the stack up"
		}
		t.Run(name, func(t *testing.T) {
			h, c, f := harness(t, errors.New("keploy could not interrupt the application's container: context deadline exceeded"))
			if s, n := h.runServing(t), chargedWith(h, models.CaptureLossPIDMap); s != models.TestSetStatusInternalErr || n == 0 {
				t.Fatalf("set 1: %s, %d tests charged pid_map_refused; want INTERNAL_ERR (its stop failed), charged", s, n)
			}
			c.stopErr = nil
			f.mu.Lock()
			f.replaceAtCheck = replaced
			f.mu.Unlock()
			resetResults(h)
			s := h.runServing(t)
			if replaced && !f.replaced {
				t.Fatal("premise: the agent was not found replaced at set 2's stored-mocks check")
			}
			if !replaced {
				if n := chargedWith(h, models.CaptureLossPIDMap); s != models.TestSetStatusFailed || n != 2 {
					t.Fatalf("set 2: %s, %d of 2 tests charged pid_map_refused; want FAILED, both: the app ran on from set 1", s, n)
				}
				return
			}
			if n := unjudged(h); s != models.TestSetStatusInternalErr || n != 2 || !h.replayer.notJudged("test-set-0") {
				t.Fatalf("set 2: %s, %d of 2 tests charged the agent started again; want INTERNAL_ERR, not judged, both: the app ran on with what a1 counted, and a1 is gone", s, n)
			}
			if reason := h.report.report.FailureReason; !strings.Contains(reason, "agent process a1, then b2") {
				t.Fatalf("set 2's reason %q; want it to name both agent processes", reason)
			}
			resetResults(h)
			if s, n := h.runServing(t), unjudged(h); s != models.TestSetStatusPassed || n != 0 {
				t.Fatalf("set 3: %s, %d tests charged the agent started again; want PASSED: set 2's stop ended the app, and set 3 started it anew under b2", s, n)
			}
		})
	}
	t.Run("the retry replaced the agent in the stack's first bring-up", func(t *testing.T) {
		h, _, f := harness(t, nil)
		f.replaceAtCheck, f.byRetry = true, true
		if s, n := h.runServing(t), unjudged(h); s != models.TestSetStatusPassed || n != 0 || !f.replaced {
			t.Fatalf("set 1: %s, %d tests charged the agent started again, replaced %v; want PASSED, judged on b2's counts: the app never ran against a1", s, n, f.replaced)
		}
	})
}

// Through Start, an agent the bring-up's retry started in the first set's
// bring-up, before the run's app ran, holds all the set's evidence: the set
// is judged on the replacement's counts.
func TestUnderTheSwitchThroughStartTheRetrysAgentInTheFirstBringUpIsJudged(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	c := &composeInstr{}
	h := composeHarness(t, c)
	h.mocks.filtered = []*models.Mock{{Name: "mock-a", Kind: models.HTTP}}
	f := &replacedAtCheckInstr{composeInstr: c, agent: "a1", replaceAtCheck: true, byRetry: true}
	h.replayer.instrumentation = f
	h.replayer.config.Test.Mocking = true
	h.replayer.config.Command = "docker compose up"
	// What config/default.go sets, which the harness's bare config does not:
	// Start runs each test set this many times at most.
	h.replayer.config.Test.MaxFlakyChecks = 1
	// Start waits for the compose agent's /health before its first set.
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer agent.Close()
	h.replayer.config.Agent.AgentURI = agent.URL
	saved := utils.ErrCode
	t.Cleanup(func() { utils.ErrCode = saved })
	utils.ErrCode = 0
	ctx, cancel := context.WithTimeout(context.Background(), prTimeout)
	defer cancel()
	if err := h.replayer.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !f.replaced {
		t.Fatal("premise: the agent was not found replaced at the first set's stored-mocks check")
	}
	if rep := h.report.report; rep == nil || rep.Status != string(models.TestSetStatusPassed) {
		t.Fatalf("report %+v; want PASSED, judged on b2's counts: the run's app never ran against a1", rep)
	}
}

// When even the re-store does not land on the agent the tests would fire
// against, the guard must fail the test set loudly — naming the cause — rather
// than let the run proceed mockless.
func TestEnsureAgentHoldsStoredMocks_FailsLoudlyWhenReRegistrationUnconfirmed(t *testing.T) {
	f := &rearmInstr{storeTakesEffect: false}
	r := newRearmReplayer(f)
	filtered, unfiltered, consumed, cases := rearmSessionArgs()

	err := r.ensureAgentHoldsStoredMocks(context.Background(), "run-1", "test-set-0", models.OutgoingOptions{}, filtered, unfiltered, consumed, false, cases)
	if err == nil {
		t.Fatal("an unconfirmed re-registration must fail the test set instead of firing tests mockless")
	}
	if !strings.Contains(err.Error(), "replaced") || !strings.Contains(err.Error(), "mockless") {
		t.Fatalf("the failure must name the cause and the refusal, got: %v", err)
	}

	stats, outgoing, stores, _, _ := f.counts()
	if outgoing != 1 || stores != 1 {
		t.Fatalf("the repair sequence should still have been attempted once, got outgoing=%d stores=%d", outgoing, stores)
	}
	if stats != 2 {
		t.Fatalf("expected the probe plus one (failed) confirmation read, got %d", stats)
	}
}

// The healthy path: the agent still holds the corpus, so nothing is touched
// beyond the single probe.
func TestEnsureAgentHoldsStoredMocks_NoOpWhenAgentHoldsCorpus(t *testing.T) {
	f := &rearmInstr{loaded: 2, storeTakesEffect: true}
	r := newRearmReplayer(f)
	filtered, unfiltered, consumed, cases := rearmSessionArgs()

	if err := r.ensureAgentHoldsStoredMocks(context.Background(), "run-1", "test-set-0", models.OutgoingOptions{}, filtered, unfiltered, consumed, false, cases); err != nil {
		t.Fatalf("an agent holding the corpus must pass the guard, got: %v", err)
	}

	stats, outgoing, stores, updates, ready := f.counts()
	if stats != 1 || outgoing != 0 || stores != 0 || updates != 0 || ready != 0 {
		t.Fatalf("the healthy path must not re-register anything, got stats=%d outgoing=%d stores=%d updates=%d ready=%d",
			stats, outgoing, stores, updates, ready)
	}
}

// Nothing was stored, so there is nothing to lose: no probe, no repair.
func TestEnsureAgentHoldsStoredMocks_SkipsProbeForEmptyCorpus(t *testing.T) {
	f := &rearmInstr{}
	r := newRearmReplayer(f)

	if err := r.ensureAgentHoldsStoredMocks(context.Background(), "run-1", "test-set-0", models.OutgoingOptions{}, nil, nil, map[string]models.MockState{}, false, []*models.TestCase{{}}); err != nil {
		t.Fatalf("an empty corpus must be a no-op, got: %v", err)
	}

	stats, outgoing, stores, updates, ready := f.counts()
	if stats != 0 || outgoing != 0 || stores != 0 || updates != 0 || ready != 0 {
		t.Fatalf("an empty corpus must not probe or repair, got stats=%d outgoing=%d stores=%d updates=%d ready=%d",
			stats, outgoing, stores, updates, ready)
	}
}

// A probe that cannot reach the agent at all must not be read as "armed":
// after the one retry it falls through to the re-registration, which then
// fails loudly if the agent is genuinely gone.
func TestEnsureAgentHoldsStoredMocks_ProbeFailureFallsThroughToReRegistration(t *testing.T) {
	f := &rearmInstr{storeTakesEffect: true, statsErrCalls: 2}
	r := newRearmReplayer(f)
	filtered, unfiltered, consumed, cases := rearmSessionArgs()

	if err := r.ensureAgentHoldsStoredMocks(context.Background(), "run-1", "test-set-0", models.OutgoingOptions{}, filtered, unfiltered, consumed, false, cases); err != nil {
		t.Fatalf("re-registration should succeed once the agent answers again, got: %v", err)
	}

	stats, outgoing, stores, _, _ := f.counts()
	if stores != 1 {
		t.Fatalf("a failing probe must fall through to the repair sequence, got stores=%d", stores)
	}
	if stats != 3 {
		t.Fatalf("expected two failing probe reads plus the confirmation read (3 stats calls), got %d", stats)
	}
	if outgoing != 1 {
		t.Fatalf("expected MockOutgoing once as part of the repair, got %d", outgoing)
	}
}

// An agent that CANNOT answer /mock/stats is not an agent reporting "nothing
// stored". Version skew — a keploy CLI newer than the agent image it is driving,
// or an agent whose service has no stats reader — must leave the run exactly as
// it was before this guard existed.
//
// Without this, the guard reads silence as a replacement, re-registers, asks the
// same unanswerable question to confirm, and fails EVERY docker-compose test set
// against such an agent.
func TestEnsureAgentHoldsStoredMocks_SkipsWhenAgentCannotReportStats(t *testing.T) {
	f := &rearmInstr{statsUnsupported: true}
	r := newRearmReplayer(f)
	filtered, unfiltered, consumed, cases := rearmSessionArgs()

	if err := r.ensureAgentHoldsStoredMocks(context.Background(), "run-1", "test-set-0", models.OutgoingOptions{}, filtered, unfiltered, consumed, false, cases); err != nil {
		t.Fatalf("an agent that cannot report stats must not fail the test set, got: %v", err)
	}

	_, outgoing, stores, updates, ready := f.counts()
	if outgoing != 0 || stores != 0 || updates != 0 || ready != 0 {
		t.Fatalf("nothing should be re-registered on an unanswerable probe, got outgoing=%d stores=%d updates=%d ready=%d",
			outgoing, stores, updates, ready)
	}
}

// The same condition discovered at the CONFIRMATION read: the repair ran, and
// this agent simply cannot report whether it took. The session is no worse off
// than before the guard existed, so it must not fail the set on an answer nobody
// can give.
func TestEnsureAgentHoldsStoredMocks_AcceptsUnconfirmableReRegistration(t *testing.T) {
	// Answers normally once (a real replacement: loaded 0 against a stored
	// corpus), then can no longer report at all.
	f := &rearmInstr{loaded: 0}
	r := newRearmReplayer(f)
	filtered, unfiltered, consumed, cases := rearmSessionArgs()
	f.afterFirstStats = func() { f.statsUnsupported = true }

	if err := r.ensureAgentHoldsStoredMocks(context.Background(), "run-1", "test-set-0", models.OutgoingOptions{}, filtered, unfiltered, consumed, false, cases); err != nil {
		t.Fatalf("an unconfirmable re-registration must not fail the test set, got: %v", err)
	}

	_, outgoing, stores, updates, _ := f.counts()
	if outgoing != 1 || stores != 1 || updates != 1 {
		t.Fatalf("the repair must still have run in full, got outgoing=%d stores=%d updates=%d", outgoing, stores, updates)
	}
}

// Cancelling the run must abort the probe. probeMockStats derives its deadline
// from the caller's context, and deriving from context.Background() instead
// would leave Ctrl+C unable to interrupt a slow agent.
func TestEnsureAgentHoldsStoredMocks_ProbeHonoursCallerCancellation(t *testing.T) {
	prev := mockStatsProbeTimeout
	mockStatsProbeTimeout = time.Hour // long enough that only cancellation can end it
	t.Cleanup(func() { mockStatsProbeTimeout = prev })

	f := &rearmInstr{statsBlock: make(chan struct{})}
	defer close(f.statsBlock)
	r := newRearmReplayer(f)
	filtered, unfiltered, consumed, cases := rearmSessionArgs()

	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(100 * time.Millisecond); cancel() }()

	done := make(chan struct{})
	go func() {
		_ = r.ensureAgentHoldsStoredMocks(ctx, "run-1", "test-set-0", models.OutgoingOptions{},
			filtered, unfiltered, consumed, false, cases)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("cancelling the run did not abort the probe; the deadline must derive from the caller's context")
	}
}

// A WEDGED agent — reachable, accepting the connection, never answering — must
// not hold this guard open. The shared AgentClient has no timeout of its own, so
// without a per-call deadline the gate blocks for as long as the test-set
// context lives and the run ends with no verdict at all: worse than the
// zero-test run it exists to prevent.
func TestEnsureAgentHoldsStoredMocks_BoundsTheWholeGuardAgainstAWedgedAgent(t *testing.T) {
	prev := mockStatsProbeTimeout
	mockStatsProbeTimeout = 50 * time.Millisecond
	t.Cleanup(func() { mockStatsProbeTimeout = prev })

	prevRepair := agentRepairTimeout
	agentRepairTimeout = 50 * time.Millisecond
	t.Cleanup(func() { agentRepairTimeout = prevRepair })

	// Blocks BOTH the probe and the first repair call. Bounding only the probe
	// moves the hang to MockOutgoing, which uses the same timeout-less client —
	// so a fake whose MockOutgoing returns instantly would pass while the real
	// guard still hung.
	f := &rearmInstr{statsBlock: make(chan struct{}), outgoingBlock: make(chan struct{})}
	defer func() { close(f.statsBlock); close(f.outgoingBlock) }()
	r := newRearmReplayer(f)
	filtered, unfiltered, consumed, cases := rearmSessionArgs()

	done := make(chan error, 1)
	go func() {
		done <- r.ensureAgentHoldsStoredMocks(context.Background(), "run-1", "test-set-0",
			models.OutgoingOptions{}, filtered, unfiltered, consumed, false, cases)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the guard hung on an agent that never answers; the probe AND the repair must be bounded — " +
			"they share one timeout-less client, so bounding the probe alone just moves the hang")
	}
}

// The repair must re-run BeforeTestSetCompose. It rotates the agent's
// per-test-set debug sink and runs any non-default AgentHooks — both lived in
// the process that went away, and the rotation is exactly the artifact someone
// wants when asking why the agent was replaced.
func TestEnsureAgentHoldsStoredMocks_ReRunsBeforeTestSetComposeOnRepair(t *testing.T) {
	f := &rearmInstr{loaded: 0, storeTakesEffect: true}
	r, h := newRearmReplayerWithHooks(f)
	filtered, unfiltered, consumed, cases := rearmSessionArgs()

	if err := r.ensureAgentHoldsStoredMocks(context.Background(), "run-1", "test-set-0",
		models.OutgoingOptions{}, filtered, unfiltered, consumed, false, cases); err != nil {
		t.Fatalf("repair failed: %v", err)
	}
	if got := h.beforeCalls(); got != 1 {
		t.Fatalf("BeforeTestSetCompose ran %d times on the repair path; want 1", got)
	}
	// The testRunID parameter exists only to be threaded here; without this the
	// signature change is unasserted and passing "" would go unnoticed.
	run, set, first := h.lastArgs()
	if run != "run-1" || set != "test-set-0" {
		t.Fatalf("hook got testRunID=%q testSetID=%q; want run-1 / test-set-0", run, set)
	}
	if first {
		t.Fatal("the repair passed firstRun=true; a repair is never the first run and must not " +
			"re-trigger first-run cleanup")
	}
}

// ...and must NOT run it when nothing needed repairing.
func TestEnsureAgentHoldsStoredMocks_NoHookWhenAgentIsHealthy(t *testing.T) {
	f := &rearmInstr{loaded: 2, storeTakesEffect: true}
	r, h := newRearmReplayerWithHooks(f)
	filtered, unfiltered, consumed, cases := rearmSessionArgs()

	if err := r.ensureAgentHoldsStoredMocks(context.Background(), "run-1", "test-set-0",
		models.OutgoingOptions{}, filtered, unfiltered, consumed, false, cases); err != nil {
		t.Fatalf("healthy path failed: %v", err)
	}
	if got := h.beforeCalls(); got != 0 {
		t.Fatalf("BeforeTestSetCompose ran %d times with nothing to repair; want 0", got)
	}
}

// The repair itself must not hand filtering back to the replacement agent.
//
// Scope note: at the real call site `totalConsumedMocks` is still EMPTY when the
// repair runs (it is allocated per test set and first written after the guard),
// so the map this particular send carries is empty in production and the agent
// omits it either way. What matters is the FLAG — and the latch it arms, which
// TestConsumedHistoryRebuildSticksAfterARepair covers with a non-empty map on
// the later per-test sends that actually carry one.
func TestEnsureAgentHoldsStoredMocks_RepairDoesNotDeferToTheReplacementsOwnHistory(t *testing.T) {
	t.Setenv("KEPLOY_AGENT_OWNS_CONSUMED", "1")

	f := &rearmInstr{loaded: 0, storeTakesEffect: true}
	r := newRearmReplayer(f)
	filtered, unfiltered, _, cases := rearmSessionArgs()
	consumed := map[string]models.MockState{"mock-a": {Name: "mock-a", Usage: models.Deleted}}

	if err := r.ensureAgentHoldsStoredMocks(context.Background(), "run-1", "test-set-0",
		models.OutgoingOptions{}, filtered, unfiltered, consumed, false, cases); err != nil {
		t.Fatalf("repair failed: %v", err)
	}

	f.mu.Lock()
	p := f.lastParams
	f.mu.Unlock()

	if p.AgentOwnsConsumed {
		t.Fatal("the repair told a replacement agent to use its own consumption history; that history is empty")
	}
	if len(p.TotalConsumedMocks) != 1 {
		t.Fatalf("the repair sent %d consumed entries; want the CLI's own history (1) so already-consumed "+
			"mocks are not re-served", len(p.TotalConsumedMocks))
	}
}

// And the ordinary per-test path must keep honouring the flag — otherwise this
// fix would quietly disable the optimisation it works around.
func TestSendMockFilterParamsKeepsAgentOwnsConsumedOnTheNormalPath(t *testing.T) {
	t.Setenv("KEPLOY_AGENT_OWNS_CONSUMED", "1")

	f := &rearmInstr{}
	r := newRearmReplayer(f)
	consumed := map[string]models.MockState{"mock-a": {Name: "mock-a", Usage: models.Deleted}}

	if err := r.SendMockFilterParamsToAgent(context.Background(), []string{}, models.BaseTime, time.Now(),
		consumed, false, recordedSetShape{}, nil); err != nil {
		t.Fatalf("send failed: %v", err)
	}

	f.mu.Lock()
	p := f.lastParams
	f.mu.Unlock()

	if !p.AgentOwnsConsumed || p.TotalConsumedMocks != nil {
		t.Fatalf("the normal path must still hand consumption to the agent; got owns=%v consumed=%d",
			p.AgentOwnsConsumed, len(p.TotalConsumedMocks))
	}
}

// The rebuild has to STICK. The agent evaluates AgentOwnsConsumed per request,
// so seeding the replacement on the repair call and then reverting on every
// later per-test call puts it straight back to filtering against an empty
// history — already-consumed mocks re-served for the rest of the run, which is
// the silently wrong pass this exists to prevent.
func TestConsumedHistoryRebuildSticksAfterARepair(t *testing.T) {
	t.Setenv("KEPLOY_AGENT_OWNS_CONSUMED", "1")

	f := &rearmInstr{loaded: 0, storeTakesEffect: true}
	r := newRearmReplayer(f)
	filtered, unfiltered, _, cases := rearmSessionArgs()
	consumed := map[string]models.MockState{"mock-a": {Name: "mock-a", Usage: models.Deleted}}

	// The repair.
	if err := r.ensureAgentHoldsStoredMocks(context.Background(), "run-1", "test-set-0",
		models.OutgoingOptions{}, filtered, unfiltered, consumed, false, cases); err != nil {
		t.Fatalf("repair failed: %v", err)
	}

	// A subsequent ORDINARY per-test send, which passes rebuildConsumed=false.
	if err := r.SendMockFilterParamsToAgent(context.Background(), []string{}, models.BaseTime, time.Now(),
		consumed, false, recordedSetShape{}, nil); err != nil {
		t.Fatalf("per-test send failed: %v", err)
	}

	f.mu.Lock()
	p := f.lastParams
	f.mu.Unlock()

	if p.AgentOwnsConsumed {
		t.Fatal("after a repair, a later per-test call handed filtering back to the replacement agent's " +
			"own history, which is missing everything consumed before it started")
	}
	if len(p.TotalConsumedMocks) != 1 {
		t.Fatalf("later calls sent %d consumed entries; want the CLI's history (1)", len(p.TotalConsumedMocks))
	}
}

// The agent clears its own per-name consumption history whenever MockOutgoing
// resets the mock manager (pkg/agent/proxy, the test-set boundary clear). The
// repair calls MockOutgoing MID-SET, so on a FALSE POSITIVE — both probes failed
// but the agent is alive and its history is real — that clear wipes live state.
//
// It is safe only because the latch is armed BEFORE that call, so every later
// send carries the CLI's map instead. Reorder those two and the wipe becomes
// silent data loss, which is what this pins.
func TestRepairLatchesBeforeItResetsTheAgentsMockManager(t *testing.T) {
	f := &rearmInstr{loaded: 0, storeTakesEffect: true}
	r := newRearmReplayer(f)

	var latchedAtOutgoing bool
	f.onOutgoing = func() { latchedAtOutgoing = r.agentHistoryIncompleteForRun.Load() }

	filtered, unfiltered, consumed, cases := rearmSessionArgs()
	if err := r.ensureAgentHoldsStoredMocks(context.Background(), "run-1", "test-set-0",
		models.OutgoingOptions{}, filtered, unfiltered, consumed, false, cases); err != nil {
		t.Fatalf("repair failed: %v", err)
	}

	if !latchedAtOutgoing {
		t.Fatal("MockOutgoing ran before the latch was armed; that call resets the agent's mock manager " +
			"and clears its consumption history, so on a false-positive repair a live agent would lose " +
			"real history with nothing sending the CLI's map in its place")
	}
}

// appExitInstr is rearmInstr behind an instrumentation that can tell whether
// the application's container still runs (AppContainerExitReader), as the
// docker compose agent client can.
type appExitInstr struct {
	*rearmInstr

	// exit is what AppContainerExit reports: nil while the app runs.
	exit error
	// logs is what GetRecentAppLogs reports: the app's last lines.
	logs string

	exitCalls int
}

func (f *appExitInstr) AppContainerExit(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.exitCalls++
	return f.exit
}

func (f *appExitInstr) GetRecentAppLogs(context.Context) string { return f.logs }

func (f *appExitInstr) appExitCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.exitCalls
}

// newAppExitReplayer is newRearmReplayer over f, logging to an observer.
func newAppExitReplayer(f *appExitInstr) (*Replayer, *observer.ObservedLogs) {
	core, logs := observer.New(zapcore.DebugLevel)
	return &Replayer{logger: zap.New(core), config: &config.Config{}, instrument: true, instrumentation: f, hookImpl: &rearmHooks{}}, logs
}

// Under docker compose the agent is a service in the app's project, and
// compose stops it the moment the app's container exits. An agent that went
// with the app is not a replaced one: reporting "replaced" there sent the
// reader after the agent, when the app had died at boot (dsmon F23). The
// failure must name the app's exit and carry its last log lines -- and the
// run must do exactly what it did before: the re-registration is still tried.
func TestEnsureAgentHoldsStoredMocks_ReportsTheAppsExitWhenTheAgentWentWithIt(t *testing.T) {
	exit := errors.New("the application container k8s-proxy exited with code 1")
	f := &appExitInstr{
		rearmInstr: &rearmInstr{statsErrCalls: 2, outgoingErr: errors.New("dial tcp [::1]:61885: connect: connection refused")},
		exit:       exit,
		logs:       "FATAL shared token: no Kubernetes client",
	}
	r, logs := newAppExitReplayer(f)
	filtered, unfiltered, consumed, cases := rearmSessionArgs()

	err := r.ensureAgentHoldsStoredMocks(context.Background(), "run-1", "test-set-0", models.OutgoingOptions{}, filtered, unfiltered, consumed, false, cases)
	if err == nil {
		t.Fatal("an agent that went with the app must still fail the test set")
	}
	if !errors.Is(err, exit) || !strings.Contains(err.Error(), "exited with code 1") {
		t.Fatalf("the failure must name the app's exit and its code, got: %v", err)
	}
	if strings.Contains(err.Error(), "replaced") {
		t.Fatalf("the app exited; the failure must not say the agent was replaced, got: %v", err)
	}
	if !strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("the agent's own error must still be carried, got: %v", err)
	}
	for _, e := range logs.All() {
		if strings.Contains(e.Message, "replaced") {
			t.Fatalf("logged %q although the app's container had exited", e.Message)
		}
	}
	var sawLogs bool
	for _, e := range logs.FilterLevelExact(zapcore.ErrorLevel).All() {
		if e.ContextMap()["appLogs"] == f.logs {
			sawLogs = true
		}
	}
	if !sawLogs {
		t.Fatal("the app's last log lines were not logged with its exit")
	}

	// Diagnosis only: the repair is still attempted, and armed, as before.
	if _, outgoing, _, _, _ := f.counts(); outgoing != 1 {
		t.Fatalf("the re-registration must still be attempted, got MockOutgoing=%d", outgoing)
	}
	if !r.agentHistoryIncompleteForRun.Load() {
		t.Fatal("the consumption-history latch must still be armed, as before")
	}
}

// The app still runs and the agent is gone: that is the replaced agent the
// guard exists for, and its failure stays as it was.
func TestEnsureAgentHoldsStoredMocks_KeepsReplacedWhenTheAppStillRuns(t *testing.T) {
	f := &appExitInstr{
		rearmInstr: &rearmInstr{statsErrCalls: 2, outgoingErr: errors.New("dial tcp [::1]:61885: connect: connection refused")},
	}
	r, _ := newAppExitReplayer(f)
	filtered, unfiltered, consumed, cases := rearmSessionArgs()

	err := r.ensureAgentHoldsStoredMocks(context.Background(), "run-1", "test-set-0", models.OutgoingOptions{}, filtered, unfiltered, consumed, false, cases)
	if err == nil {
		t.Fatal("a gone agent must fail the test set")
	}
	if !strings.Contains(err.Error(), "keploy-agent was replaced during the docker compose bring-up") || !strings.Contains(err.Error(), "MockOutgoing") {
		t.Fatalf("with the app running the failure must stay the replaced-agent one, got: %v", err)
	}
	if f.appExitCalls() == 0 {
		t.Fatal("the app's container was never looked at before the agent was called replaced")
	}
}

// An agent that holds the corpus, or one that answered and took the
// re-registration, never needs the app's container looked at.
func TestEnsureAgentHoldsStoredMocks_LooksAtTheAppOnlyWhenTheAgentFails(t *testing.T) {
	for _, tc := range []struct {
		name string
		f    *rearmInstr
	}{
		{"healthy agent", &rearmInstr{loaded: 2, storeTakesEffect: true}},
		{"replaced agent re-registered", &rearmInstr{storeTakesEffect: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &appExitInstr{rearmInstr: tc.f, exit: errors.New("the application container app exited with code 1")}
			r, _ := newAppExitReplayer(f)
			filtered, unfiltered, consumed, cases := rearmSessionArgs()

			if err := r.ensureAgentHoldsStoredMocks(context.Background(), "run-1", "test-set-0", models.OutgoingOptions{}, filtered, unfiltered, consumed, false, cases); err != nil {
				t.Fatalf("got: %v", err)
			}
			if n := f.appExitCalls(); n != 0 {
				t.Fatalf("the app's container was inspected %d times with nothing failing", n)
			}
		})
	}
}

// acceptsWatch is a set's watch of its agent's process that counts Accepts;
// rep is the replacement it says, where replaced.
type acceptsWatch struct {
	rep      models.AgentReplacement
	replaced bool
	accepts  int
}

func (w *acceptsWatch) Replacement() (models.AgentReplacement, bool) {
	return w.rep, w.replaced
}
func (w *acceptsWatch) Accept() { w.accepts++ }
func (w *acceptsWatch) Stop()   {}

// The replacement compose's bring-up retry started while the app had never
// started holds all the set's watch keeps to (the app never ran against the
// one it replaced): where the watch says the agent was replaced by it, the
// watch keeps to it, whether the retry's own setup took or the stored-mocks
// check stored the mocks again. A replacement the retry did not start (one
// docker started again under an app that ran), wherever it was noticed, does
// not move the watch, which says so; nor does a watch that says no
// replacement, or a run with the switch off.
func TestAgentStartedBeforeApp_TheWatchKeepsToTheRetrysReplacement(t *testing.T) {
	ab := models.AgentReplacement{First: "a1", Next: "b2"}
	for _, tc := range []struct {
		name      string
		off       bool
		beforeApp string
		replaced  bool
		accepts   int
	}{
		{"replaced by the retry's", false, "b2", true, 1},
		{"replaced by one the retry did not start", false, "", true, 0},
		{"replaced by another than the retry's", false, "c3", true, 0},
		{"not replaced", false, "b2", false, 0},
		{"the switch off", true, "b2", true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !tc.off {
				t.Setenv(provenswitch.Env, "1")
			}
			f := &rearmLossInstr{rearmInstr: &rearmInstr{storeTakesEffect: true}, agent: "b2", beforeApp: tc.beforeApp}
			r := newRearmReplayer(f.rearmInstr)
			r.instrumentation = f
			w := &acceptsWatch{rep: ab, replaced: tc.replaced}
			r.agentStartedBeforeApp(context.Background(), w)
			if w.accepts != tc.accepts {
				t.Fatalf("%d Accept(s), want %d", w.accepts, tc.accepts)
			}
		})
	}
}

// retryInstr is a compose stack (composeInstr) whose agent names itself in
// its capture-loss reads, as the keploy agent client's do.
//
//   - retryAtReady: from the end of the set's MakeAgentReadyForDockerCompose
//     on, the agent that answers is b2, holding the set's mocks: compose's
//     bring-up retry started it in place of a1 while the app's container
//     had never started, and the client set it up again (stored the mocks,
//     accepted the watches) and noted it (AgentStartedBeforeApp), so the
//     stored-mocks check finds the mocks held.
//   - lossAfterReady: a1 counts lost from the end of MakeAgentReady on (the
//     app's startup lost a process under a1).
//   - replaceAtCheck: at the first stored-mocks check the agent is b2, which
//     holds no mocks and counts nothing: docker started the agent container
//     again during the app's readiness wait, after the app started under a1.
type retryInstr struct {
	*composeInstr
	mu             sync.Mutex
	agent          string
	counts, lost   models.CaptureLosses
	loaded         int
	retryAtReady   bool
	lossAfterReady bool
	replaceAtCheck bool
	replaced       bool
	byRetry        bool
}

func (f *retryInstr) GetCaptureLosses(ctx context.Context) (models.CaptureLosses, error) {
	l, _, err := f.GetCaptureLossesAndAgent(ctx)
	return l, err
}

func (f *retryInstr) GetCaptureLossesAndAgent(context.Context) (models.CaptureLosses, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.counts, f.agent, nil
}

func (f *retryInstr) AgentStartedBeforeApp(instance string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.byRetry && instance == "b2"
}

func (f *retryInstr) MakeAgentReadyForDockerCompose(ctx context.Context) error {
	err := f.composeInstr.MakeAgentReadyForDockerCompose(ctx)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.retryAtReady {
		f.retryAtReady, f.replaced, f.byRetry = false, true, true
		f.agent, f.counts = "b2", nil
	}
	if f.lossAfterReady && !f.replaced {
		f.counts = f.lost
	}
	return err
}

func (f *retryInstr) GetMockStats(context.Context) (models.MockStats, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.replaceAtCheck {
		f.replaceAtCheck, f.replaced = false, true
		f.agent, f.counts, f.loaded = "b2", nil, 0
	}
	return models.MockStats{Loaded: f.loaded}, nil
}

func (f *retryInstr) StoreMocks(ctx context.Context, filtered, unfiltered []*models.Mock) error {
	f.mu.Lock()
	f.loaded = len(filtered) + len(unfiltered)
	f.mu.Unlock()
	return f.composeInstr.StoreMocks(ctx, filtered, unfiltered)
}

func retryHarness(t *testing.T) (*prRun, *retryInstr) {
	c := &composeInstr{}
	h := composeHarness(t, c)
	h.mocks.filtered = []*models.Mock{{Name: "mock-a", Kind: models.HTTP}}
	f := &retryInstr{composeInstr: c, agent: "a1"}
	h.replayer.instrumentation = f
	h.replayer.config.Test.Mocking = true
	return h, f
}

// chargedStartedAgain is how many of the set's tests are charged
// models.CaptureLossAgentStartedAgain.
func chargedStartedAgain(h *prRun) int {
	n := 0
	for _, r := range h.report.report.Tests {
		for _, cc := range r.FailureInfo.CannotCapture {
			if cc.Kind == models.CaptureLossAgentStartedAgain {
				n++
			}
		}
	}
	return n
}

// composeStacks are the two ways a compose set gets its stack, each a run of
// the set: the stack kept across sets (serveTest), whose first bring-up these
// sets are, as keploy test runs every compose set that mocks (composeReuse);
// and one the set brings up afresh (serveTest false), which RunTestSet marks
// and rebases on its own path (keploy test takes it for compose today only
// with mocking off, which reads no losses). The bases and the agent's
// replacement must come out the same on both.
var composeStacks = []struct {
	name string
	run  func(*prRun, *testing.T) models.TestSetStatus
}{
	{"the stack kept across sets", (*prRun).runServing},
	{"a stack the set brings up afresh", (*prRun).run},
}

// Under the proven switch, compose's bring-up retry on its normal path: the
// retry starts the agent again while the app's container is only created
// (never started), and the client sets the replacement up again, mocks and
// all. The set's mark was read from a1 (the pids are marked before the agent
// is made ready), and the stored-mocks check finds the mocks held. The app
// never ran against a1, and nothing was lost: the set is judged, and passes.
func TestUnderTheSwitchTheRetrysAgentSetUpAgainLeavesTheSetJudged(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	for _, stack := range composeStacks {
		t.Run(stack.name, func(t *testing.T) {
			h, f := retryHarness(t)
			core, logs := observer.New(zapcore.InfoLevel)
			h.replayer.logger = zap.New(core)
			f.retryAtReady = true
			s := stack.run(h, t)
			if !f.replaced {
				t.Fatal("premise: the retry did not start the agent again at MakeAgentReady")
			}
			if n := chargedStartedAgain(h); s != models.TestSetStatusPassed || n != 0 {
				t.Fatalf("%s, %d of 2 tests charged the agent started again (reason %q); want PASSED, judged: the app never ran against a1",
					s, n, h.report.report.FailureReason)
			}
			// The log says so, naming both agent processes.
			said := logs.FilterMessageSnippet("bring-up retry before the application ever ran").
				FilterField(zap.String("agent replaced", "a1")).FilterField(zap.String("agent", "b2"))
			if said.Len() != 1 {
				t.Fatalf("%d logs say the retry's agent counts from its start, naming a1 and b2; want 1", said.Len())
			}
		})
	}
}

// Under the proven switch, each set bringing its compose stack up afresh: the
// second set's stack is another, with an agent process of its own (b2), and
// keploy starts the app anew under it. a1 counted a process of set 1's app it
// could not place; that app and a1 went with set 1's stack, so set 2 counts
// from b2's start and is judged on it: it passes, no test charged a1's loss
// or the agent started again. Set 1 is charged its own loss.
func TestUnderTheSwitchASetsOwnComposeStackCountsFromItsAgentsStart(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h, f := retryHarness(t)
	f.lost, f.lossAfterReady = models.CaptureLosses{{Kind: models.CaptureLossPIDMap, Count: 1, What: "pid 41"}}, true
	if s, n := h.run(t), chargedWith(h, models.CaptureLossPIDMap); s != models.TestSetStatusFailed || n != 2 {
		t.Fatalf("set 1: %s, %d of 2 tests charged pid_map_refused; want FAILED, both: its app lost a process under a1", s, n)
	}
	// Set 1's stack is down; set 2's comes up with an agent of its own.
	f.mu.Lock()
	f.agent, f.counts, f.lossAfterReady = "b2", nil, false
	f.mu.Unlock()
	resetResults(h)
	s := h.run(t)
	if lost, again := chargedWith(h, models.CaptureLossPIDMap), chargedStartedAgain(h); s != models.TestSetStatusPassed || lost != 0 || again != 0 || h.replayer.notJudged("test-set-0") {
		t.Fatalf("set 2: %s, %d tests charged pid_map_refused, %d the agent started again (reason %q); want PASSED, judged on b2's counts from its start",
			s, lost, again, h.report.report.FailureReason)
	}
}

// Under the proven switch, an agent docker started again after the app
// started under a1 (during the app's readiness wait), as a stack is brought
// up: a1 counted a process of the app's it could not place as the app
// started, which no read of keploy's saw before a1 went. The replacement
// holds no mocks, so the stored-mocks check stores them again; but it is no
// replacement the bring-up's retry started, and the app did run against a1:
// a1's count went with it, so each test is charged the agent started again,
// and the set is not judged, rather than passing on b2's empty counts.
// Control: with no replacement the set fails, both tests charged the loss.
func TestUnderTheSwitchAnAgentReplacedAfterTheAppStartedDoesNotForgetItsStartupLoss(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	lost := models.CaptureLosses{{Kind: models.CaptureLossPIDMap, Count: 1, What: "pid 41"}}
	for _, replaced := range []bool{false, true} {
		name := "not replaced"
		if replaced {
			name = "replaced during the readiness wait"
		}
		for _, stack := range composeStacks {
			t.Run(name+", "+stack.name, func(t *testing.T) {
				h, f := retryHarness(t)
				f.lost, f.lossAfterReady, f.replaceAtCheck = lost, true, replaced
				s := stack.run(h, t)
				if replaced != f.replaced {
					t.Fatalf("premise: replaced %v, want %v", f.replaced, replaced)
				}
				charged, n := chargedWith(h, models.CaptureLossPIDMap), chargedStartedAgain(h)
				if !replaced {
					if s != models.TestSetStatusFailed || charged != 2 {
						t.Fatalf("%s, %d of 2 tests charged pid_map_refused; want FAILED, both", s, charged)
					}
					return
				}
				if s != models.TestSetStatusInternalErr || n != 2 || !h.replayer.notJudged("test-set-0") {
					t.Fatalf("%s, %d of 2 tests charged the agent started again; want INTERNAL_ERR, not judged, both: the app started under a1, which is gone with what it counted", s, n)
				}
				if reason := h.report.report.FailureReason; !strings.Contains(reason, "agent process a1, then b2") {
					t.Fatalf("reason %q; want it to name both agent processes", reason)
				}
			})
		}
	}
}
