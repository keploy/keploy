package replay

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

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

	statsCalls        int
	mockOutgoingCalls int
	storeMocksCalls   int
	updateParamsCalls int
	readyCalls        int
}

func (f *rearmInstr) GetMockStats(context.Context) (models.MockStats, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statsCalls++
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

func (f *rearmInstr) MockOutgoing(context.Context, models.OutgoingOptions) error {
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

func (f *rearmInstr) UpdateMockParams(context.Context, models.MockFilterParams) error {
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

func newRearmReplayer(f *rearmInstr) *Replayer {
	return &Replayer{logger: zap.NewNop(), config: &config.Config{}, instrument: true, instrumentation: f}
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

	if err := r.ensureAgentHoldsStoredMocks(context.Background(), "test-set-0", models.OutgoingOptions{}, filtered, unfiltered, consumed, false, cases); err != nil {
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

	err := r.ensureAgentHoldsStoredMocks(context.Background(), "test-set-0", models.OutgoingOptions{}, filtered, unfiltered, consumed, false, cases)
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

	if err := r.ensureAgentHoldsStoredMocks(context.Background(), "test-set-0", models.OutgoingOptions{}, filtered, unfiltered, consumed, false, cases); err != nil {
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

	if err := r.ensureAgentHoldsStoredMocks(context.Background(), "test-set-0", models.OutgoingOptions{}, nil, nil, map[string]models.MockState{}, false, []*models.TestCase{{}}); err != nil {
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

	if err := r.ensureAgentHoldsStoredMocks(context.Background(), "test-set-0", models.OutgoingOptions{}, filtered, unfiltered, consumed, false, cases); err != nil {
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

	if err := r.ensureAgentHoldsStoredMocks(context.Background(), "test-set-0", models.OutgoingOptions{}, filtered, unfiltered, consumed, false, cases); err != nil {
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

	if err := r.ensureAgentHoldsStoredMocks(context.Background(), "test-set-0", models.OutgoingOptions{}, filtered, unfiltered, consumed, false, cases); err != nil {
		t.Fatalf("an unconfirmable re-registration must not fail the test set, got: %v", err)
	}

	_, outgoing, stores, updates, _ := f.counts()
	if outgoing != 1 || stores != 1 || updates != 1 {
		t.Fatalf("the repair must still have run in full, got outgoing=%d stores=%d updates=%d", outgoing, stores, updates)
	}
}
