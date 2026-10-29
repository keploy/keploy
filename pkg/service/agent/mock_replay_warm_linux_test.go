//go:build linux

package agent

import (
	"context"
	"fmt"
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

// (mongo-stuck-handler, the learn step's cost per call; r3f.) keploy mock's
// replay of record's full round (mock record's auto-replay, a session with
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
// recording before them while their client waited (r3f: TestSeeded's POST
// /query timed out at 30 s with five handlers in DriftCache.warm, and the
// per-set line was in no log).
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
