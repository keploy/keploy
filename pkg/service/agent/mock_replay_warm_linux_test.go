//go:build linux

package agent

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/models"
)

// warmWireProxy is the wire's proxy (wireProxy) that can read a staged set
// for the learn step (coreAgent.LearnSetWarmer), as keploy's proxy does: it
// counts the agent's calls, and the stagings (SetMocks) made before each.
type warmWireProxy struct {
	*wireProxy
	warms []int
}

func (p *warmWireProxy) WarmLearnSet() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.warms = append(p.warms, p.staged)
	return nil
}

// keploy mock's replay of record's full round (mock record's auto-replay, a session with
// Learn on) stages its set as the CLI hands the agent its pool slices
// (SetScopePool: servePoolExactly's first staging, stageForExactServing),
// before the test command runs, not by keploy test's stage filter. That
// staging has the proxy read the set's recordings for the learn step
// (WarmLearnSet) once for the set, after its recordings are staged; the
// set's later serves of its pool (another push of it, a test's begin and
// its end) stage nothing again and read nothing again. The proxy reads
// them only while the set's learn step is on, every recording at the
// staging (TestAStagedSetIsReadForTheLearnStepInRecordsFullRoundAlone), and
// a staged set's first drifted call then reads only where its drift stands
// (TestASetsCallsAfterItsStagingReadOnlyWhatTheirDriftTouches). Before,
// nothing on this path read the set: its first drifted calls read every
// recording before them while their client waited (on api-server's round, a
// test's POST /query timed out at 30 s with five handlers in
// DriftCache.warm, and the per-set line was in no log).
func TestAMockReplaySetIsReadForTheLearnStepAsItIsStaged(t *testing.T) {
	a, wp := wireAgent(t, models.MODE_TEST)
	p := &warmWireProxy{wireProxy: wp}
	a.Proxy = p
	ctx := context.Background()
	warms := func() []int {
		p.mu.Lock()
		defer p.mu.Unlock()
		return append([]int(nil), p.warms...)
	}

	// No recording stored yet: nothing is staged, nor read.
	require.NoError(t, a.SetScopePool(ctx, map[string][]string{"o": {"mock-0"}}))
	require.Empty(t, warms(), "with no recording stored there is no set to read")

	var mocks []*models.Mock
	for i := 0; i < 8; i++ {
		m := httpMock(fmt.Sprintf("mock-%d", i), "GET", fmt.Sprintf("http://h/rows/%d", i))
		m.Spec.ReqTimestampMock = t0.Add(time.Duration(i) * time.Second)
		mocks = append(mocks, m)
	}
	storeMocks(a, mocks...)
	a.scopeTable = map[string][]string{"o::TestA": {"mock-1", "mock-2"}}
	require.NoError(t, a.SetScopePool(ctx, map[string][]string{"o": {"mock-0"}}))
	require.Equal(t, []int{1}, warms(), "the set is read for the learn step as it is staged, after its recordings are in")

	// The set's later serves of its pool, a test's begin and its end,
	// stage nothing again, nor read the set again.
	require.NoError(t, a.SetScopePool(ctx, map[string][]string{"o": {"mock-0"}}))
	claimSelf(t, a)
	_, serr := a.ProvenBegin(ctx, begin("TestA"))
	require.Nil(t, serr)
	_, serr = a.ProvenEnd(ctx, models.ScopeReq{Name: "TestA", Outcome: "passed"})
	require.Nil(t, serr)
	require.Equal(t, []int{1}, warms(), "a set is read for the learn step once, as it is staged")

	// A proxy that cannot read a set for the learn step is staged as before.
	b, bp := wireAgent(t, models.MODE_TEST)
	storeMocks(b, mocks...)
	require.NoError(t, b.SetScopePool(ctx, map[string][]string{"o": {"mock-0"}}))
	bp.mu.Lock()
	require.Equal(t, 1, bp.staged)
	bp.mu.Unlock()
}

// stageOrderProxy is the wire's proxy that can read a staged set for the
// learn step and takes the set's recordings before an instant
// (coreAgent.RecordedBeforeSetter), as keploy's proxy does. It logs, in
// order, the stagings ("stage"), that hand-off ("before") and the reads for
// the learn step ("warm").
type stageOrderProxy struct {
	*wireProxy
	evMu   sync.Mutex
	events []string
}

func (p *stageOrderProxy) event(e string) {
	p.evMu.Lock()
	defer p.evMu.Unlock()
	p.events = append(p.events, e)
}

func (p *stageOrderProxy) log() []string {
	p.evMu.Lock()
	defer p.evMu.Unlock()
	return append([]string(nil), p.events...)
}

func (p *stageOrderProxy) SetMocks(ctx context.Context, filtered, unfiltered []*models.Mock) error {
	p.event("stage")
	return p.wireProxy.SetMocks(ctx, filtered, unfiltered)
}

func (p *stageOrderProxy) SetRecordedBefore(func(t time.Time, visit func(*models.Mock) bool) error) error {
	p.event("before")
	return nil
}

func (p *stageOrderProxy) WarmLearnSet() error {
	p.event("warm")
	return nil
}

// keploy test's stage filter (serveKeployTestExactly's Stage branch), the
// path keploy record's auto-replay and keploy test take, has the proxy read
// the set's recordings for the learn step (WarmLearnSet) once per set: after
// the set's mocks are staged and the proxy holds the set's recordings before
// an instant, so the read sees the set it serves; and not again on a test's
// begin or end. The next set's stage filter reads that set once. The twin of
// TestAMockReplaySetIsReadForTheLearnStepAsItIsStaged, for this path: with
// the call gone, a set's first drifted calls read every recording before
// them on their answer path while their client waits.
func TestAKeployTestSetIsReadForTheLearnStepAsItIsStaged(t *testing.T) {
	a, wp := wireAgent(t, models.MODE_TEST)
	p := &stageOrderProxy{wireProxy: wp}
	a.Proxy = p
	ctx := context.Background()
	var mocks []*models.Mock
	for i := 0; i < 8; i++ {
		m := httpMock(fmt.Sprintf("mock-%d", i), "GET", fmt.Sprintf("http://h/rows/%d", i))
		m.Spec.ReqTimestampMock = t0.Add(time.Duration(i) * time.Second)
		mocks = append(mocks, m)
	}
	stage := func() {
		t.Helper()
		require.NoError(t, a.StoreMocks(ctx, nil, mocks))
		require.NoError(t, a.UpdateMockParams(ctx, models.MockFilterParams{AfterTime: models.BaseTime, BeforeTime: time.Now(),
			Exact: &models.ExactServing{Stage: true, Pool: []string{"mock-0"}}}))
	}
	// readOnce checks the log holds one read since from, after a staging
	// and the recordings-before hand-off, both since from.
	readOnce := func(from int) {
		t.Helper()
		ev := p.log()[from:]
		warm, staged, before := -1, -1, -1
		for i, e := range ev {
			switch e {
			case "warm":
				require.Equal(t, -1, warm, "the set is read for the learn step twice: %v", ev)
				warm = i
			case "stage":
				staged = i
			case "before":
				before = i
			}
		}
		require.NotEqual(t, -1, warm, "the set is never read for the learn step: %v", ev)
		require.True(t, staged != -1 && staged < warm, "the set is read before its mocks are staged: %v", ev)
		require.True(t, before != -1 && before < warm, "the set is read before the proxy holds its recordings before an instant: %v", ev)
	}

	stage()
	readOnce(0)
	// A test's begin and its end read nothing again.
	require.NoError(t, a.UpdateMockParams(ctx, models.MockFilterParams{Exact: &models.ExactServing{Test: "TestA", Owned: []string{"mock-1", "mock-2"}}}))
	require.NoError(t, a.UpdateMockParams(ctx, models.MockFilterParams{Exact: &models.ExactServing{}}))
	readOnce(0)

	// The next set's stage filter reads that set, once.
	next := len(p.log())
	stage()
	readOnce(next)
}
