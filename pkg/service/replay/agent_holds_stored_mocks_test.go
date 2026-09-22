package replay

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
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
// predicate and the re-registration sequence. The call site itself (one call in
// RunTestSet's compose branch, after the app-ready wait) is reviewed by eye —
// driving RunTestSet end to end needs a live app, an agent and a report DB.

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

	statsCalls        int
	mockOutgoingCalls int
	storeMocksCalls   int
	updateParamsCalls int
	readyCalls        int
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

func (f *rearmInstr) MockOutgoing(ctx context.Context, _ models.OutgoingOptions) error {
	f.mu.Lock()
	block := f.outgoingBlock
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
	return nil
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
	return nil
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

	stats, outgoing, stores, updates, ready := f.counts()
	if outgoing != 1 || stores != 1 || updates != 1 || ready != 1 {
		t.Fatalf("expected the full sequence once (MockOutgoing, StoreMocks, UpdateMockParams, ready), got outgoing=%d stores=%d updates=%d ready=%d",
			outgoing, stores, updates, ready)
	}
	if stats != 2 {
		t.Fatalf("expected the probe plus the confirmation read (2 stats calls), got %d", stats)
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
		consumed, false, time.Time{}); err != nil {
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
		consumed, false, time.Time{}); err != nil {
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
