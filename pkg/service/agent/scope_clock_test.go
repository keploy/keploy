package agent

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	coreAgent "go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// clockHooks is a build's AgentHooks that freezes the run's clock: it notes
// each move it is asked to make.
type clockHooks struct {
	AgentHook
	moves  []string
	err    error
	endErr error // EndTestClock's alone, when set
	file   string
}

func (h *clockHooks) BeginTestClock(_ context.Context, test string, at time.Time) error {
	h.moves = append(h.moves, "begin "+test+" "+at.UTC().Format(time.RFC3339))
	return h.err
}

func (h *clockHooks) EndTestClock(_ context.Context, test string) error {
	h.moves = append(h.moves, "end "+test)
	if h.endErr != nil {
		return h.endErr
	}
	return h.err
}

func (h *clockHooks) TimeFile() string { return h.file }

// newClockAgent is a replay's agent with a proxy that takes each narrowing:
// under the switch a begin with no mapping is served the reusable tier only
// (the proven wire's rule), so it narrows the pool too.
func newClockAgent(t *testing.T) *Agent {
	t.Helper()
	a := newReplayAgent()
	a.Proxy = &workerPool{}
	a.clientMocks.Store(uint64(0), &ClientMockStorage{})
	return a
}

func useHooks(t *testing.T, h AgentHooks) {
	t.Helper()
	prev := ActiveHooks
	RegisterHooks(h)
	t.Cleanup(func() { RegisterHooks(prev) })
}

// Under the proven switch, a replayed test's begin moves the run's clock to
// when it began in its recording, and its end moves it back; a test the
// recording kept no begin for keeps the start time (a zero instant).
func TestReplayMovesTheClockToEachTestsRecordedBegin(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := &clockHooks{}
	useHooks(t, h)
	a := newClockAgent(t)
	ctx := context.Background()
	b := time.Date(2026, 10, 6, 9, 1, 0, 0, time.UTC)
	require.NoError(t, a.SetTestBegins(ctx, map[string]time.Time{"pkg::TestA": b}, nil))

	require.NoError(t, a.BeginScopeAt(ctx, "pkg::TestA", 0, time.Now()))
	require.NoError(t, a.EndScopeAt(ctx, "pkg::TestA", 0, time.Now()))
	require.NoError(t, a.BeginScopeAt(ctx, "pkg::TestNew", 0, time.Now()))
	require.NoError(t, a.EndScopeAt(ctx, "pkg::TestNew", 0, time.Now()))
	require.Equal(t, []string{
		"begin pkg::TestA 2026-10-06T09:01:00Z", "end pkg::TestA",
		"begin pkg::TestNew 0001-01-01T00:00:00Z", "end pkg::TestNew",
	}, h.moves)
}

// The run has one clock: a test that begins while another holds it fails,
// and the holder keeps it; a suite scope never moves it.
func TestOneTestHoldsTheRunsClockAtATime(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := &clockHooks{}
	useHooks(t, h)
	a := newClockAgent(t)
	ctx := context.Background()

	a.NoteScope("pkg", 0, "", true)
	require.NoError(t, a.BeginScopeAt(ctx, "pkg", 0, time.Now()))
	require.NoError(t, a.BeginScopeAt(ctx, "pkg::TestA", 0, time.Now()))
	require.NoError(t, a.BeginScopeAt(ctx, "pkg::TestA/case", 0, time.Now()), "a subtest runs on its parent's clock")
	require.NoError(t, a.EndScopeAt(ctx, "pkg::TestA/case", 0, time.Now()))
	require.ErrorContains(t, a.BeginScopeAt(ctx, "pkg::TestB", 7, time.Now()), "pkg::TestB began while pkg::TestA was open")
	a.NoteScopeOutcome("pkg::TestB", 7, models.ScopeOutcomePassed) // a harness that ran it anyway
	require.NoError(t, a.EndScopeAt(ctx, "pkg::TestB", 7, time.Now()))
	require.NoError(t, a.EndScopeAt(ctx, "pkg::TestA", 0, time.Now()))
	require.NoError(t, a.EndScopeAt(ctx, "pkg", 0, time.Now()))
	require.Equal(t, []string{"begin pkg::TestA 0001-01-01T00:00:00Z", "end pkg::TestA"}, h.moves)
	// The refused test is recorded failed, whatever its harness did.
	ws, err := a.GetScopeWindows(ctx)
	require.NoError(t, err)
	var refused []string
	for _, w := range ws {
		if w.Name == "pkg::TestB" {
			refused = append(refused, w.Outcome)
		}
	}
	require.Equal(t, []string{models.ScopeOutcomeFailed}, refused)
}

// A begin that fails after it moved the clock (here: the pool cannot be
// narrowed, no mocks being stored) moves it back, since its harness sends no
// end: the next test begins on its own clock.
func TestABeginThatFailsGivesTheClockBack(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := &clockHooks{}
	useHooks(t, h)
	a := newClockAgent(t)
	a.Proxy = &workerPool{}
	a.clientMocks.Delete(uint64(0)) // the narrowing fails
	ctx := context.Background()
	require.NoError(t, a.SetScopeTable(ctx, map[string][]string{"pkg::TestA": {"mock-1"}}))
	require.ErrorContains(t, a.BeginScopeAt(ctx, "pkg::TestA", 0, time.Now()), "no mocks are loaded")
	a.clientMocks.Store(uint64(0), &ClientMockStorage{})
	require.NoError(t, a.BeginScopeAt(ctx, "pkg::TestB", 0, time.Now()))
	require.Equal(t, []string{
		"begin pkg::TestA 0001-01-01T00:00:00Z", "end pkg::TestA",
		"begin pkg::TestB 0001-01-01T00:00:00Z",
	}, h.moves)
}

// A test whose begin fails after it took the clock is recorded failed, though
// its harness ignores the refusal, runs it and reports it passed: it ran on
// the start time's clock and the whole pool, not its own.
func TestABeginThatFailsAfterTheClockMovedFailsTheTest(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	useHooks(t, &clockHooks{})
	a := newClockAgent(t)
	a.Proxy = &workerPool{}
	a.clientMocks.Delete(uint64(0)) // the narrowing fails
	ctx := context.Background()
	require.NoError(t, a.SetScopeTable(ctx, map[string][]string{"pkg::TestA": {"mock-1"}}))
	require.ErrorContains(t, a.BeginScopeAt(ctx, "pkg::TestA", 0, time.Now()), "no mocks are loaded")
	a.NoteScopeOutcome("pkg::TestA", 0, models.ScopeOutcomePassed)
	// Its end tries to give the pool back, which fails as the narrowing did
	// (no mocks stored here); the end records nothing either way.
	_ = a.EndScopeAt(ctx, "pkg::TestA", 0, time.Now())
	ws, err := a.GetScopeWindows(ctx)
	require.NoError(t, err)
	var outcomes []string
	for _, w := range ws {
		if w.Name == "pkg::TestA" {
			outcomes = append(outcomes, w.Outcome)
		}
	}
	require.Equal(t, []string{models.ScopeOutcomeFailed}, outcomes)
}

// A test's end that cannot move the clock back frees the clock for the next
// test, whose begin moves it (a move is absolute), but the move stays in
// force until one back succeeds: a new session tries again, and fails while
// it cannot.
func TestAClockThatCouldNotMoveBackIsMovedBackLater(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := &clockHooks{}
	useHooks(t, h)
	a := newClockAgent(t)
	a.Proxy = &workerPool{}
	ctx := context.Background()

	require.NoError(t, a.BeginScopeAt(ctx, "pkg::TestA", 0, time.Now()))
	h.err = errors.New("disk full")
	require.ErrorContains(t, a.EndScopeAt(ctx, "pkg::TestA", 0, time.Now()), "disk full")
	require.ErrorContains(t, a.MockOutgoing(ctx, models.OutgoingOptions{}), "disk full", "the session would start on the test's clock")
	require.ErrorContains(t, a.MockOutgoing(ctx, models.OutgoingOptions{}), "disk full", "a retry tries the move again")
	h.err = nil
	require.NoError(t, a.MockOutgoing(ctx, models.OutgoingOptions{}))
	require.NoError(t, a.MockOutgoing(ctx, models.OutgoingOptions{}), "the clock is back: nothing to move")
	require.Equal(t, []string{
		"begin pkg::TestA 0001-01-01T00:00:00Z",
		"end pkg::TestA", "end pkg::TestA", "end pkg::TestA", "end pkg::TestA",
	}, h.moves)

	// The next test can begin after a failed end; its own end, once it
	// succeeds, leaves nothing to move back.
	h.moves = nil
	require.NoError(t, a.BeginScopeAt(ctx, "pkg::TestB", 0, time.Now()))
	h.err = errors.New("disk full")
	require.Error(t, a.EndScopeAt(ctx, "pkg::TestB", 0, time.Now()))
	h.err = nil
	require.NoError(t, a.BeginScopeAt(ctx, "pkg::TestC", 0, time.Now()), "a failed end does not hold the clock")
	require.NoError(t, a.EndScopeAt(ctx, "pkg::TestC", 0, time.Now()))
	require.NoError(t, a.MockOutgoing(ctx, models.OutgoingOptions{}))
	require.Equal(t, []string{
		"begin pkg::TestB 0001-01-01T00:00:00Z", "end pkg::TestB",
		"begin pkg::TestC 0001-01-01T00:00:00Z", "end pkg::TestC",
	}, h.moves)
}

// A clock that cannot be moved fails the begin and the end: the test would
// otherwise run on another clock than its recording's. The end still gives
// the run back its whole pool first.
func TestAClockThatCannotMoveFailsTheTest(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := &clockHooks{err: errors.New("disk full")}
	useHooks(t, h)
	a := newClockAgent(t)
	ctx := context.Background()
	require.ErrorContains(t, a.BeginScopeAt(ctx, "pkg::TestA", 0, time.Now()), "disk full")
	require.Zero(t, a.clockHeld, "a begin that could not move the clock holds nothing")

	h.err = nil
	pool := &workerPool{}
	a.Proxy = pool
	require.NoError(t, a.SetScopeTable(ctx, map[string][]string{"pkg::TestB": {"mock-1"}}))
	require.NoError(t, a.BeginScopeAt(ctx, "pkg::TestB", 5, time.Now()))
	require.Equal(t, []uint32{5}, pool.narrowed)
	h.err = errors.New("disk full")
	require.ErrorContains(t, a.EndScopeAt(ctx, "pkg::TestB", 5, time.Now()), "disk full")
	require.Equal(t, []uint32{5}, pool.cleared, "the worker gets the whole pool back even when the clock cannot move back")
}

// Without the switch, and in a record, nothing moves the clock.
func TestTheClockStaysWithoutTheSwitchAndInRecord(t *testing.T) {
	h := &clockHooks{}
	useHooks(t, h)
	ctx := context.Background()

	t.Setenv(provenswitch.Env, "")
	a := newClockAgent(t)
	require.NoError(t, a.BeginScopeAt(ctx, "pkg::TestA", 0, time.Now()))
	require.NoError(t, a.EndScopeAt(ctx, "pkg::TestA", 0, time.Now()))

	t.Setenv(provenswitch.Env, "1")
	r := newRecordAgent()
	require.NoError(t, r.BeginScopeAt(ctx, "pkg::TestA", 0, time.Now()))
	require.NoError(t, r.EndScopeAt(ctx, "pkg::TestA", 0, time.Now()))
	require.Empty(t, h.moves)
	require.Equal(t, models.MODE_TEST, a.config.Agent.Mode)
}

// The begins belong to the replay that installed them: a new session forgets
// them.
func TestANewSessionForgetsTheBegins(t *testing.T) {
	a := newClockAgent(t)
	require.NoError(t, a.SetTestBegins(context.Background(), map[string]time.Time{"pkg::TestA": time.Now()}, nil))
	a.resetScopeState()
	require.Nil(t, a.testBegins)
}

// A new mock session (MockOutgoing) gives back, through the build, the clock
// a test of the previous session still held; one whose clock cannot move back
// fails.
func TestMockOutgoingGivesTheHeldClockBack(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := &clockHooks{}
	useHooks(t, h)
	a := newClockAgent(t)
	a.Proxy = &workerPool{}
	ctx := context.Background()
	require.NoError(t, a.BeginScopeAt(ctx, "pkg::TestA", 0, time.Now()))
	require.NoError(t, a.MockOutgoing(ctx, models.OutgoingOptions{}))
	require.Equal(t, []string{"begin pkg::TestA 0001-01-01T00:00:00Z", "end pkg::TestA"}, h.moves)
	require.NoError(t, a.BeginScopeAt(ctx, "pkg::TestB", 0, time.Now()))
	h.err = errors.New("disk full")
	require.ErrorContains(t, a.MockOutgoing(ctx, models.OutgoingOptions{}), "disk full")
}

// TimeFile is the build's clock record, and "" for a build that does not
// freeze the run.
func TestTimeFileIsTheBuildsClockRecord(t *testing.T) {
	useHooks(t, &AgentHook{})
	require.Equal(t, "", TimeFile())
	useHooks(t, &clockHooks{file: "/tmp/kclk1/t"})
	require.Equal(t, "/tmp/kclk1/t", TimeFile())
}

// workerPool is a proxy that notes which workers' served views are narrowed
// and given back.
type workerPool struct {
	coreAgent.Proxy
	settledProxy
	narrowed, cleared []uint32
	mu                sync.Mutex
	exactTest         string
}

func (p *workerPool) Mock(context.Context, models.OutgoingOptions) error { return nil }
func (p *workerPool) Record(context.Context, chan<- *models.Mock, models.OutgoingOptions) error {
	return nil
}
func (p *workerPool) SetMappedUniverse([]string) {}
func (p *workerPool) SetMocks(context.Context, []*models.Mock, []*models.Mock) error {
	return nil
}
func (p *workerPool) SetWorkerScope(pid uint32, _ []string) { p.narrowed = append(p.narrowed, pid) }
func (p *workerPool) ClearWorkerScope(pid uint32)           { p.cleared = append(p.cleared, pid) }

// The proven wire serves its single worker exactly (exact_proven.go).
func (p *workerPool) ServeExactTest(test string, _ []*models.Mock) error {
	p.mu.Lock()
	p.exactTest = test
	p.mu.Unlock()
	return nil
}
func (p *workerPool) ServeExactPool(string, []string) error { return nil }

// ExactTestOutcome is the latest test opened, open or ended.
func (p *workerPool) ExactTestOutcome() (string, []string, []string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.exactTest, nil, nil, true
}

// syncClockHooks notes the moves it is asked to make from many goroutines,
// and whether a begin ever came while another test's begin was unanswered by
// its end.
type syncClockHooks struct {
	AgentHook
	mu      sync.Mutex
	open    string
	crossed []string
	moves   int
}

func (h *syncClockHooks) BeginTestClock(_ context.Context, test string, _ time.Time) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.open != "" {
		h.crossed = append(h.crossed, "begin "+test+" while "+h.open+" held the clock")
	}
	h.open = test
	h.moves++
	return nil
}

func (h *syncClockHooks) EndTestClock(_ context.Context, test string) error {
	// Yield before the end lands, so a begin the agent let through too early
	// would get here first.
	for i := 0; i < 20; i++ {
		runtime.Gosched()
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.open != test {
		h.crossed = append(h.crossed, "end "+test+" while "+h.open+" held the clock")
	}
	h.open = ""
	h.moves++
	return nil
}

// Begins and ends that race from many goroutines reach the build strictly
// paired: a begin only once the previous holder's end is through, and every
// refusal names the test that held the clock then.
func TestTheRunsClockMovesInPairsUnderRacingTests(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := &syncClockHooks{}
	useHooks(t, h)
	a := newClockAgent(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				name := fmt.Sprintf("pkg::Test%d_%d", w, i)
				if err := a.BeginScopeAt(ctx, name, 0, time.Now()); err != nil {
					assert.Contains(t, err.Error(), name+" began while ")
					continue
				}
				assert.NoError(t, a.EndScopeAt(ctx, name, 0, time.Now()))
			}
		}(w)
	}
	wg.Wait()
	h.mu.Lock()
	defer h.mu.Unlock()
	require.Empty(t, h.crossed)
	require.NotZero(t, h.moves)
	require.Empty(t, h.open, "every test that held the clock gave it back")
}

// A new session gives back the clock a test of the previous one still held.
func TestANewSessionGivesTheClockBack(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := &clockHooks{}
	useHooks(t, h)
	a := newClockAgent(t)
	ctx := context.Background()
	require.NoError(t, a.BeginScopeAt(ctx, "pkg::TestA", 0, time.Now()))
	require.NoError(t, a.releaseTestClock(ctx))
	require.Equal(t, []string{"begin pkg::TestA 0001-01-01T00:00:00Z", "end pkg::TestA"}, h.moves)
	require.NoError(t, a.BeginScopeAt(ctx, "pkg::TestB", 0, time.Now()))
}

// fileOnlyHooks is a build that freezes the run's clock (it names the record)
// but cannot move it per test.
type fileOnlyHooks struct{ AgentHook }

func (fileOnlyHooks) TimeFile() string { return "/tmp/kclk1/t" }

// Under the switch, a build that freezes the clock but cannot move it per test
// refuses the begins, so the replay fails rather than run every test at the
// start time; a build that does not freeze the clock, or the switch off, takes
// them.
func TestSetTestBeginsRefusesABuildThatCannotMoveItsClock(t *testing.T) {
	ctx := context.Background()
	begins := map[string]time.Time{"pkg::TestA": time.Now()}
	t.Setenv(provenswitch.Env, "1")
	useHooks(t, &fileOnlyHooks{})
	require.ErrorIs(t, newClockAgent(t).SetTestBegins(ctx, begins, nil), ErrNoTestClock)
	useHooks(t, &clockHooks{file: "/tmp/kclk1/t"})
	require.NoError(t, newClockAgent(t).SetTestBegins(ctx, begins, nil))
	useHooks(t, &AgentHook{})
	require.NoError(t, newClockAgent(t).SetTestBegins(ctx, begins, nil))
	t.Setenv(provenswitch.Env, "")
	useHooks(t, &fileOnlyHooks{})
	require.NoError(t, newClockAgent(t).SetTestBegins(ctx, begins, nil))
}

// A test refused the clock while another holds it undoes nothing at its end:
// the holder's worker keeps its narrowed view, and the clock stays.
func TestARefusedTestsEndLeavesTheHoldersPool(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := &clockHooks{}
	useHooks(t, h)
	a := newClockAgent(t)
	pool := &workerPool{}
	a.Proxy = pool
	ctx := context.Background()
	me := os.Getpid()
	require.NoError(t, a.SetScopeTable(ctx, map[string][]string{"pkg::TestA": {"m-a"}, "pkg::TestB": {"m-b"}}))
	require.NoError(t, a.BeginScopeAt(ctx, "pkg::TestA", me, time.Now()))
	require.ErrorIs(t, a.BeginScopeAt(ctx, "pkg::TestB", me, time.Now()), ErrOneTestAtATime)
	require.NoError(t, a.EndScopeAt(ctx, "pkg::TestB", me, time.Now()))
	require.NoError(t, a.EndScopeAt(ctx, "pkg::TestB", me, time.Now()), "an end sent twice")
	require.Empty(t, pool.cleared, "the refused test's end gave the holder's worker the whole pool")
	require.Equal(t, []string{"begin pkg::TestA 0001-01-01T00:00:00Z"}, h.moves)
	require.NoError(t, a.EndScopeAt(ctx, "pkg::TestA", me, time.Now()))
	require.Equal(t, []uint32{uint32(me)}, pool.cleared)

	// A refused test that later begins on its own runs as any other.
	require.NoError(t, a.BeginScopeAt(ctx, "pkg::TestB", me, time.Now()))
	require.NoError(t, a.EndScopeAt(ctx, "pkg::TestB", me, time.Now()))
	require.Equal(t, []uint32{uint32(me), uint32(me)}, pool.cleared)
}

// A harness that retries the end of a test whose move back failed moves the
// clock back then.
func TestARetriedEndMovesTheClockBack(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := &clockHooks{}
	useHooks(t, h)
	a := newClockAgent(t)
	ctx := context.Background()
	require.NoError(t, a.BeginScopeAt(ctx, "pkg::TestA", 0, time.Now()))
	h.err = errors.New("disk full")
	require.Error(t, a.EndScopeAt(ctx, "pkg::TestA", 0, time.Now()))
	h.err = nil
	require.NoError(t, a.EndScopeAt(ctx, "pkg::TestA", 0, time.Now()))
	require.Zero(t, a.clockMoved)
	require.Equal(t, []string{"begin pkg::TestA 0001-01-01T00:00:00Z", "end pkg::TestA", "end pkg::TestA"}, h.moves)
}

// The clock is held by a test of one worker: the same name from another
// worker is another test, refused while the first holds it, and its end moves
// nothing.
func TestTheClockIsHeldByOneWorkersTest(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := &clockHooks{}
	useHooks(t, h)
	a := newClockAgent(t)
	a.Proxy = &workerPool{}
	ctx := context.Background()
	me, other := os.Getpid(), os.Getpid()+1 // the holder's worker lives
	require.NoError(t, a.BeginScopeAt(ctx, "pkg::TestA", me, time.Now()))
	require.ErrorContains(t, a.BeginScopeAt(ctx, "pkg::TestA", other, time.Now()), "began while pkg::TestA was open")
	require.ErrorContains(t, a.BeginScopeAt(ctx, "pkg::TestA/case", other, time.Now()), "began while pkg::TestA was open")
	require.NoError(t, a.EndScopeAt(ctx, "pkg::TestA", other, time.Now()))
	require.NoError(t, a.EndScopeAt(ctx, "pkg::TestA", other, time.Now()))
	require.Equal(t, []string{"begin pkg::TestA 0001-01-01T00:00:00Z"}, h.moves, "another worker's end moved the holder's clock")
	require.NoError(t, a.EndScopeAt(ctx, "pkg::TestA", me, time.Now()))
	require.Equal(t, []string{"begin pkg::TestA 0001-01-01T00:00:00Z", "end pkg::TestA"}, h.moves)
}

// Without the switch, a test whose pool cannot narrow keeps its window and
// its harness's verdict, as before the clock.
func TestWithoutTheSwitchAFailedNarrowingKeepsTheVerdict(t *testing.T) {
	t.Setenv(provenswitch.Env, "")
	useHooks(t, &clockHooks{})
	a := newClockAgent(t)
	a.Proxy = &workerPool{}
	a.clientMocks.Delete(uint64(0)) // the narrowing fails
	ctx := context.Background()
	require.NoError(t, a.SetScopeTable(ctx, map[string][]string{"pkg::TestA": {"mock-1"}}))
	require.ErrorContains(t, a.BeginScopeAt(ctx, "pkg::TestA", 0, time.Now()), "no mocks stored")
	a.NoteScopeOutcome("pkg::TestA", 0, models.ScopeOutcomePassed)
	_ = a.EndScopeAt(ctx, "pkg::TestA", 0, time.Now())
	require.Equal(t, []string{models.ScopeOutcomePassed}, outcomesOf(t, a, "pkg::TestA"))
}

// A subtest riding its parent's clock whose pool cannot narrow keeps its
// window, and moves no clock: it took none.
func TestASubtestWhoseNarrowingFailsMovesNoClock(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := &clockHooks{}
	useHooks(t, h)
	a := newClockAgent(t)
	a.Proxy = &workerPool{}
	ctx := context.Background()
	require.NoError(t, a.BeginScopeAt(ctx, "pkg::TestA", 0, time.Now()))
	require.NoError(t, a.SetScopeTable(ctx, map[string][]string{"pkg::TestA/case": {"mock-1"}}))
	a.clientMocks.Delete(uint64(0)) // the narrowing fails
	require.ErrorContains(t, a.BeginScopeAt(ctx, "pkg::TestA/case", 0, time.Now()), "no mocks are loaded")
	a.NoteScopeOutcome("pkg::TestA/case", 0, models.ScopeOutcomePassed)
	_ = a.EndScopeAt(ctx, "pkg::TestA/case", 0, time.Now())
	require.Equal(t, []string{models.ScopeOutcomePassed}, outcomesOf(t, a, "pkg::TestA/case"))
	require.Equal(t, []string{"begin pkg::TestA 0001-01-01T00:00:00Z"}, h.moves)
}

// A begin whose move fails after an end that could not move the clock back
// leaves that move marked: the next session still moves it back.
func TestAFailedBeginKeepsAnEarlierMoveMarked(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := &clockHooks{}
	useHooks(t, h)
	a := newClockAgent(t)
	a.Proxy = &workerPool{}
	ctx := context.Background()
	require.NoError(t, a.BeginScopeAt(ctx, "pkg::TestA", 0, time.Now()))
	h.err = errors.New("disk full")
	require.Error(t, a.EndScopeAt(ctx, "pkg::TestA", 0, time.Now()))
	require.Error(t, a.BeginScopeAt(ctx, "pkg::TestB", 0, time.Now()))
	h.err = nil
	require.NoError(t, a.MockOutgoing(ctx, models.OutgoingOptions{}))
	require.Equal(t, []string{
		"begin pkg::TestA 0001-01-01T00:00:00Z", "end pkg::TestA",
		"begin pkg::TestB 0001-01-01T00:00:00Z", "end pkg::TestA",
	}, h.moves)
}

// outcomesOf is the verdicts of name's windows.
func outcomesOf(t *testing.T, a *Agent, name string) []string {
	t.Helper()
	ws, err := a.GetScopeWindows(context.Background())
	require.NoError(t, err)
	var out []string
	for _, w := range ws {
		if w.Name == name {
			out = append(out, w.Outcome)
		}
	}
	return out
}

// A pid-0 test withdrawn at its begin whose clock could not move back has it
// moved back by its end, and its end gives back no pool it never narrowed.
func TestAWithdrawnTestsEndMovesItsClockBack(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := &clockHooks{endErr: errors.New("disk full")}
	useHooks(t, h)
	a := newClockAgent(t)
	a.Proxy = &workerPool{}
	a.clientMocks.Delete(uint64(0)) // the narrowing fails, after the clock moved
	ctx := context.Background()
	require.NoError(t, a.SetScopeTable(ctx, map[string][]string{"pkg::TestA": {"mock-1"}}))
	require.ErrorContains(t, a.BeginScopeAt(ctx, "pkg::TestA", 0, time.Now()), "disk full")
	h.endErr = nil
	require.NoError(t, a.EndScopeAt(ctx, "pkg::TestA", 0, time.Now()), "the end restored a pool the begin never narrowed")
	require.Zero(t, a.clockMoved)
	require.Equal(t, []string{"begin pkg::TestA 0001-01-01T00:00:00Z", "end pkg::TestA", "end pkg::TestA"}, h.moves)
}

// A new session whose clock cannot move back changes nothing of the previous
// session: its windows and begins stay, for the retry.
func TestAFailedReleaseKeepsThePreviousSession(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := &clockHooks{}
	useHooks(t, h)
	a := newClockAgent(t)
	a.Proxy = &workerPool{}
	ctx := context.Background()
	require.NoError(t, a.SetTestBegins(ctx, map[string]time.Time{"pkg::TestA": time.Now()}, nil))
	require.NoError(t, a.BeginScopeAt(ctx, "pkg::TestA", 0, time.Now()))
	h.err = errors.New("disk full")
	require.Error(t, a.MockOutgoing(ctx, models.OutgoingOptions{}))
	_, err := a.GetOutgoing(ctx, models.OutgoingOptions{})
	require.Error(t, err)
	require.NotNil(t, a.testBegins)
	require.Contains(t, a.workerOpen, scopeKey{name: "pkg::TestA"})
}

// A record session (GetOutgoing) gives back the clock a test still held.
func TestGetOutgoingGivesTheHeldClockBack(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := &clockHooks{}
	useHooks(t, h)
	a := newClockAgent(t)
	a.Proxy = &workerPool{}
	ctx := context.Background()
	require.NoError(t, a.BeginScopeAt(ctx, "pkg::TestA", 0, time.Now()))
	_, err := a.GetOutgoing(ctx, models.OutgoingOptions{})
	require.NoError(t, err)
	require.Equal(t, []string{"begin pkg::TestA 0001-01-01T00:00:00Z", "end pkg::TestA"}, h.moves)
}

// A holder's worker is gone only when its stat is no longer there or names
// another process (a zombie, a reused pid); a stat the agent cannot read for
// another reason, as when it is out of file descriptors under load, never
// judges a worker that may be alive lost.
func TestWorkerGoneOnlyWhenTheProcessIsNotThere(t *testing.T) {
	stat := func(state, start string) []byte {
		f := make([]string, 20)
		for i := range f {
			f[i] = "0"
		}
		f[0], f[19] = state, start
		return []byte("42 (worker) " + strings.Join(f, " "))
	}
	for _, c := range []struct {
		name string
		read func(int) ([]byte, error)
		gone bool
	}{
		{"alive, same start", func(int) ([]byte, error) { return stat("S", "100"), nil }, false},
		{"another process on the pid", func(int) ([]byte, error) { return stat("S", "200"), nil }, true},
		{"a zombie", func(int) ([]byte, error) { return stat("Z", "100"), nil }, true},
		{"exited", func(int) ([]byte, error) {
			return nil, &fs.PathError{Op: "open", Path: "/proc/42/stat", Err: syscall.ENOENT}
		}, true},
		{"exited mid-read", func(int) ([]byte, error) {
			return nil, &fs.PathError{Op: "read", Path: "/proc/42/stat", Err: syscall.ESRCH}
		}, true},
		{"out of file descriptors", func(int) ([]byte, error) {
			return nil, &fs.PathError{Op: "open", Path: "/proc/42/stat", Err: syscall.EMFILE}
		}, false},
		{"a denied read", func(int) ([]byte, error) {
			return nil, &fs.PathError{Op: "open", Path: "/proc/42/stat", Err: syscall.EACCES}
		}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			prev := readProcStat
			readProcStat = c.read
			t.Cleanup(func() { readProcStat = prev })
			if got := workerGone(42, "100"); got != c.gone {
				t.Fatalf("workerGone = %v, want %v", got, c.gone)
			}
		})
	}
}

// realClockHooks is a build that freezes the run's clock and can put it on
// real time for a test (RealTestClock).
type realClockHooks struct{ clockHooks }

func (h *realClockHooks) BeginRealTestClock(_ context.Context, test string) error {
	h.moves = append(h.moves, "real "+test)
	return h.err
}

// A test whose recording lists a mock.real destination runs on the real
// clock (F3); the others at their recorded begins. Each end moves the clock
// back to the run's start time.
func TestATestThatUsesMockRealRunsOnTheRealClock(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := &realClockHooks{clockHooks{file: "/tmp/kclk1/t"}}
	useHooks(t, h)
	a := newClockAgent(t)
	ctx := context.Background()
	b := time.Date(2026, 10, 6, 9, 1, 0, 0, time.UTC)
	require.NoError(t, a.SetTestBegins(ctx, map[string]time.Time{"pkg::TestA": b, "pkg::TestB": b},
		map[string][]string{"pkg::TestB": {"db.example.com:5432"}, "pkg::TestC": nil}))
	for _, id := range []string{"pkg::TestA", "pkg::TestB"} {
		require.NoError(t, a.BeginScopeAt(ctx, id, 0, time.Now()))
		require.NoError(t, a.EndScopeAt(ctx, id, 0, time.Now()))
	}
	require.Equal(t, []string{
		"begin pkg::TestA 2026-10-06T09:01:00Z", "end pkg::TestA",
		"real pkg::TestB", "end pkg::TestB",
	}, h.moves)
}

// A build that freezes the run's clock but cannot put it on real time refuses
// a recording with a REAL test, so the test never runs at its recorded begin
// against a dependency that answers at real time. A build that freezes
// nothing has no clock to put back on real time.
func TestSetTestBeginsRefusesARealTestABuildCannotUnfreeze(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	ctx := context.Background()
	begins := map[string]time.Time{"pkg::TestA": time.Now()}
	real := map[string][]string{"pkg::TestA": {"db.example.com:5432"}}
	useHooks(t, &clockHooks{file: "/tmp/kclk1/t"})
	require.ErrorIs(t, newClockAgent(t).SetTestBegins(ctx, begins, real), ErrNoRealTestClock)
	require.NoError(t, newClockAgent(t).SetTestBegins(ctx, begins, nil))

	// A list of no destinations is no REAL test.
	require.NoError(t, newClockAgent(t).SetTestBegins(ctx, begins, map[string][]string{"pkg::TestA": nil}))

	h := &clockHooks{}
	useHooks(t, h)
	a := newClockAgent(t)
	require.NoError(t, a.SetTestBegins(ctx, begins, real))
	require.NoError(t, a.BeginScopeAt(ctx, "pkg::TestA", 0, time.Now()))
	require.Empty(t, h.moves, "a REAL test keeps the real clock it is on")
}

// A keploy test case that uses mock.real runs on the real clock (F3) through
// the build's RealTestClock; a build that freezes nothing has no clock to
// move; one that freezes and cannot is refused.
func TestBeginRealTestCase(t *testing.T) {
	ctx := context.Background()
	h := &realClockHooks{clockHooks{file: "/tmp/kclk1/t"}}
	useHooks(t, h)
	require.NoError(t, BeginRealTestCase(ctx, "test-1"))
	require.Equal(t, []string{"real test-1"}, h.moves)

	useHooks(t, &AgentHook{})
	require.NoError(t, BeginRealTestCase(ctx, "test-1"), "a build that freezes nothing has nothing to move")
	useHooks(t, &fileOnlyHooks{})
	require.ErrorIs(t, BeginRealTestCase(ctx, "test-1"), ErrNoRealTestClock)
}

// outsideClockHooks is a build that can also keep the run's clock on real
// time outside tests (RealOutsideClock).
type outsideClockHooks struct{ realClockHooks }

func (h *outsideClockHooks) OutsideTestClock(_ context.Context, real bool) error {
	if real {
		h.moves = append(h.moves, "outside real")
	} else {
		h.moves = append(h.moves, "outside start")
	}
	return h.err
}

// A test ID's path is the part before its last "::", without the project.
func TestTestPathIsTheIDsPath(t *testing.T) {
	for id, want := range map[string]string{
		"pkg::TestA":                               "pkg",
		"e2e/a.spec.ts::pays › with a card":        "e2e/a.spec.ts",
		"web::e2e/a.spec.ts::pays":                 "e2e/a.spec.ts",
		"e2e/a.spec.ts::ends with a colon:":        "e2e/a.spec.ts",
		models.TestID("", "", "pkg", "Test/[::1]"): "pkg",
		models.TestID("web", "f.ts", "k", ":a::b"): "f.ts",
		"TestBare": "",
	} {
		require.Equal(t, want, testPath(id), id)
	}
}

// A key is REAL when every test of the recording its ID carries lists a
// mock.real destination: not when one runs at its recorded begin (one the
// scope table alone lists included), nor when no recorded test carries it.
func TestARealKeyIsOneWhoseRecordedTestsAllUseMockReal(t *testing.T) {
	a := newClockAgent(t)
	at := time.Date(2026, 10, 6, 9, 1, 0, 0, time.UTC)
	stripe := []string{"api.stripe.com:443"}
	require.NoError(t, a.SetTestBegins(context.Background(),
		map[string]time.Time{"a::x": at, "a::y": at, "b::x": at, "b::y": at, "d::x": at, "c::x": at},
		map[string][]string{"a::x": stripe, "a::y": stripe, "b::x": stripe, "d::x": stripe}))
	a.scopeTable = map[string][]string{"a::x": nil, "d::old": nil}
	require.True(t, a.realKey("a"))
	require.False(t, a.realKey("b"), "a key with a test that runs at its recorded begin")
	require.False(t, a.realKey("c"), "a key with no REAL test")
	require.False(t, a.realKey("d"), "a test the scope table alone lists runs at its recorded begin")
	require.False(t, a.realKey("worker"), "a key no recorded test carries")
}

// A key may name one describe group of a test file (its path, then the group's
// titles): the group's beforeAll runs on the clock of the file's tests, as it
// did under the file's own key. A path that only starts like the file's is not
// it.
func TestAGroupsKeyIsRealAsItsFilesTests(t *testing.T) {
	a := newClockAgent(t)
	at := time.Date(2026, 10, 6, 9, 1, 0, 0, time.UTC)
	stripe := []string{"js.stripe.com:443"}
	require.NoError(t, a.SetTestBegins(context.Background(),
		map[string]time.Time{"e2e/a.spec.ts::G › x": at, "e2e/a.spec.ts::H › y": at, "e2e/b.spec.ts::G › x": at},
		map[string][]string{"e2e/a.spec.ts::G › x": stripe, "e2e/a.spec.ts::H › y": stripe}))
	require.True(t, a.realKey("e2e/a.spec.ts"))
	require.True(t, a.realKey("e2e/a.spec.ts/G"))
	require.True(t, a.realKey("e2e/a.spec.ts/G/Inner"))
	require.False(t, a.realKey("e2e/b.spec.ts/G"), "a group of a file with a test that runs at its recorded begin")
	require.False(t, a.realKey("e2e/a.spec.tsx/G"), "another file whose path starts like it")
	require.False(t, keyCoversPath("e2e/a.spec.ts", ""), "a bare test name is no key's")
}

// Under the proven switch, a claim of a key whose recorded tests all use
// mock.real keeps the run's clock on real time outside them: its tests run
// on real time, and so did all it did outside them when they were recorded.
// A claim of any other key puts it back at the start time. Only a change
// moves the clock (a run with no REAL key never calls the build), the moves
// in tests are as before, and a new session gives the start time back.
func TestAClaimOfARealKeyKeepsTheClockOutsideItsTestsOnRealTime(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := &outsideClockHooks{realClockHooks{clockHooks{file: "/tmp/kclk1/t"}}}
	useHooks(t, h)
	a := newClockAgent(t)
	ctx := context.Background()
	b := time.Date(2026, 10, 6, 9, 1, 0, 0, time.UTC)
	stripe := []string{"api.stripe.com:443"}
	require.NoError(t, a.SetTestBegins(ctx,
		map[string]time.Time{"e2e/a.spec.ts::pays": b, "e2e/b.spec.ts::pays": b, "e2e/b.spec.ts::lists": b},
		map[string][]string{"e2e/a.spec.ts::pays": stripe, "e2e/b.spec.ts::pays": stripe}))
	run := func(id string) {
		t.Helper()
		require.NoError(t, a.BeginScopeAt(ctx, id, 0, time.Now()))
		require.NoError(t, a.EndScopeAt(ctx, id, 0, time.Now()))
	}
	require.NoError(t, a.claimTestClock(ctx, "chromium"), "a worker's own claim: no recorded test carries its key")
	require.NoError(t, a.claimTestClock(ctx, "e2e/a.spec.ts"))
	run("e2e/a.spec.ts::pays")
	require.NoError(t, a.claimTestClock(ctx, "e2e/a.spec.ts"), "the same clock again moves nothing")
	require.NoError(t, a.claimTestClock(ctx, "e2e/b.spec.ts"))
	run("e2e/b.spec.ts::lists")
	require.NoError(t, a.claimTestClock(ctx, "e2e/a.spec.ts"))
	require.NoError(t, a.releaseTestClock(ctx))
	require.False(t, a.clockOutsideReal)
	require.Equal(t, []string{
		"outside real",
		"real e2e/a.spec.ts::pays", "end e2e/a.spec.ts::pays",
		"outside start",
		"begin e2e/b.spec.ts::lists 2026-10-06T09:01:00Z", "end e2e/b.spec.ts::lists",
		"outside real",
		"outside start",
	}, h.moves)

	// A build that cannot move the clock outside tests leaves it as it was,
	// and the next claim tries again.
	h.moves, h.err = nil, errors.New("disk full")
	require.ErrorContains(t, a.claimTestClock(ctx, "e2e/a.spec.ts"), "disk full")
	require.False(t, a.clockOutsideReal)
	h.err = nil
	require.NoError(t, a.claimTestClock(ctx, "e2e/a.spec.ts"))
	require.Equal(t, []string{"outside real", "outside real"}, h.moves)

	// Without the switch nothing moves.
	t.Setenv(provenswitch.Env, "")
	h.moves = nil
	require.NoError(t, a.claimTestClock(ctx, "e2e/b.spec.ts"))
	require.Empty(t, h.moves)
}

// A build that freezes the run's clock and puts REAL tests on real time but
// cannot keep it there outside them refuses the claim of a REAL key, so what
// the key does outside its tests never runs hours or days before them; a
// build that freezes nothing has no clock to move.
func TestAClaimOfARealKeyNeedsABuildThatKeepsRealTimeOutsideTests(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	ctx := context.Background()
	begins := map[string]time.Time{"pkg::TestA": time.Now()}
	real := map[string][]string{"pkg::TestA": {"api.stripe.com:443"}}
	useHooks(t, &realClockHooks{clockHooks{file: "/tmp/kclk1/t"}})
	a := newClockAgent(t)
	require.NoError(t, a.SetTestBegins(ctx, begins, real))
	require.NoError(t, a.claimTestClock(ctx, "other"))
	require.ErrorIs(t, a.claimTestClock(ctx, "pkg"), ErrNoRealOutsideClock)

	useHooks(t, &realClockHooks{})
	require.NoError(t, a.claimTestClock(ctx, "pkg"))
}
