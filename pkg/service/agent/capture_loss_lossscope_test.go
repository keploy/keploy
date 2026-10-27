//go:build linux

package agent

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"go.keploy.io/server/v3/pkg/models"
)

// What the agent could not capture that no test's end was charged with is
// what was lost after the last test ended: a record's judging fails the run
// on it (design v14 §7.5, evidence charged to no test). A loss a test was
// charged with is not in it; one after the last test is, by its kind; and
// a new session forgets the last one's charges.
func TestUnchargedCaptureLossesAreWhatNoTestWasChargedWith(t *testing.T) {
	ctx := context.Background()
	a, _ := wireAgent(t, models.MODE_RECORD)
	claimSelf(t, a)
	require.Empty(t, endPassed(t, a, "TestA").Action)
	un, err := a.UnchargedCaptureLosses(ctx)
	require.NoError(t, err)
	require.Empty(t, un, "nothing lost")

	// Lost while TestB ran: TestB is charged with it.
	_, serr := a.ProvenBegin(ctx, begin("TestB"))
	require.Nil(t, serr)
	a.Hooks = countingHooks{refusals: models.PIDMapRefusals{Target: 2}}
	_, serr = a.ProvenEnd(ctx, models.ScopeReq{Name: "TestB", Outcome: "passed"})
	require.Nil(t, serr)
	un, err = a.UnchargedCaptureLosses(ctx)
	require.NoError(t, err)
	require.Empty(t, un, "a loss charged to a test is no run's loss")

	// Lost after the last test ended: no test is charged with it.
	a.Hooks = countingHooks{refusals: models.PIDMapRefusals{Target: 5}}
	un, err = a.UnchargedCaptureLosses(ctx)
	require.NoError(t, err)
	require.Len(t, un, 1)
	require.Equal(t, models.CaptureLossPIDMap, un[0].Kind)
	require.Equal(t, uint64(3), un[0].Count, "the 3 lost after the last test, not the 2 TestB was charged with")

	// A new session counts from its own start, and charges nothing yet.
	require.NoError(t, a.resetSession(ctx))
	un, err = a.UnchargedCaptureLosses(ctx)
	require.NoError(t, err)
	require.Empty(t, un)
}
