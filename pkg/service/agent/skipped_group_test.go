package agent

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/models"
)

// cleanupMiss is a call a group's afterAll makes with a value only its tests
// bind (a session cookie), which no recording answers.
func cleanupMiss() models.UnmatchedCall {
	return models.UnmatchedCall{Protocol: "HTTP", ActualSummary: "POST /query", MatchPhase: models.MatchPhaseVariable, Kind: models.MissOutsideTest}
}

// skipThenClose claims key, skips its test skipped as the verdicts gate it,
// and leaves the misses its closing hooks make in the proxy.
func skipThenClose(t *testing.T, a *Agent, p *wireProxy, key, skipped string, misses ...models.UnmatchedCall) {
	t.Helper()
	ctx := context.Background()
	_, serr := a.ClaimInstance(ctx, models.InstanceReq{Key: key}, os.Getpid())
	require.Nil(t, serr)
	require.NoError(t, a.SetScopeSkips(ctx, map[string]string{key + "::" + skipped: "flaky: flipped"}))
	b, serr := a.ProvenBegin(ctx, begin(skipped))
	require.Nil(t, serr)
	require.Equal(t, models.ScopeActionSkip, b.Action)
	p.mu.Lock()
	p.misses = append(p.misses, misses...)
	p.mu.Unlock()
}

// run claims key (unless it is "") and runs its test name to a passed end.
func run(t *testing.T, a *Agent, key, name string) models.ScopeEndResp {
	t.Helper()
	ctx := context.Background()
	if key != "" {
		_, serr := a.ClaimInstance(ctx, models.InstanceReq{Key: key}, os.Getpid())
		require.Nil(t, serr)
	}
	_, serr := a.ProvenBegin(ctx, begin(name))
	require.Nil(t, serr)
	e, serr := a.ProvenEnd(ctx, models.ScopeReq{Name: name, Outcome: "passed"})
	require.Nil(t, serr)
	return e
}

// The closing hooks of a group whose test keploy skipped send what that test
// would have bound: their misses, made under the group's key before the next
// group claims, are charged to no test. The next group's test passes, and the
// run's misses still hold them, saying whose they are.
func TestTheClosingHooksOfAGroupWithASkippedTestFailNoTest(t *testing.T) {
	a, p := wireAgent(t, models.MODE_TEST)
	skipThenClose(t, a, p, "spec/Sharing", "TestShare", cleanupMiss())

	e := run(t, a, "spec/Checkbox", "TestToggle")
	require.Empty(t, e.Action, "the skipped group's cleanup failed the next group's test")
	require.Equal(t, models.ScopeOutcomePassed, windowOf(t, a, "spec/Checkbox::TestToggle").Outcome)

	all, err := a.GetMockErrors(context.Background())
	require.NoError(t, err)
	require.Len(t, all, 1, "the miss stays in the run's misses")
	require.Equal(t, "spec/Sharing", all[0].SkippedGroup)
	require.False(t, all[0].CountsAgainstTest())
	require.Contains(t, all[0].NextSteps, "closing hooks of spec/Sharing after keploy skipped spec/Sharing::TestShare")
}

// With no test of the group skipped, a miss made between two tests is the
// next test's, as ever (design v14 §5.1).
func TestAGroupWithNoSkippedTestStillChargesTheNextTest(t *testing.T) {
	a, p := wireAgent(t, models.MODE_TEST)
	ctx := context.Background()
	run(t, a, "spec/Sharing", "TestShare")
	p.mu.Lock()
	p.misses = append(p.misses, cleanupMiss())
	p.mu.Unlock()

	e := run(t, a, "spec/Checkbox", "TestToggle")
	require.Equal(t, models.ScopeActionFail, e.Action)
	all, err := a.GetMockErrors(ctx)
	require.NoError(t, err)
	require.Len(t, all, 1)
	require.Empty(t, all[0].SkippedGroup)
}

// Only what the skipped group's hooks made before the next group claimed is
// set aside: a miss of the next group's own beforeAll, made after its claim,
// is its test's.
func TestAMissMadeAfterTheNextGroupClaimsIsItsTests(t *testing.T) {
	a, p := wireAgent(t, models.MODE_TEST)
	ctx := context.Background()
	skipThenClose(t, a, p, "spec/Sharing", "TestShare", cleanupMiss())
	_, serr := a.ClaimInstance(ctx, models.InstanceReq{Key: "spec/Checkbox"}, os.Getpid())
	require.Nil(t, serr)
	p.mu.Lock()
	p.misses = append(p.misses, models.UnmatchedCall{Protocol: "HTTP", ActualSummary: "POST /query", Kind: models.MissOutsideTest})
	p.mu.Unlock()

	e := run(t, a, "", "TestToggle")
	require.Equal(t, models.ScopeActionFail, e.Action, "the next group's own miss was set aside")
	all, err := a.GetMockErrors(ctx)
	require.NoError(t, err)
	require.Len(t, all, 2)
	var aside int
	for _, m := range all {
		if m.SkippedGroup != "" {
			aside++
		}
	}
	require.Equal(t, 1, aside, "only the skipped group's cleanup is set aside")
}

// A test after the skipped one in the same group claims no other key: a miss
// made between them is that test's, as before.
func TestAMissBeforeTheSkippedGroupsNextTestIsThatTests(t *testing.T) {
	a, p := wireAgent(t, models.MODE_TEST)
	skipThenClose(t, a, p, "spec/Sharing", "TestShare", cleanupMiss())

	e := run(t, a, "", "TestAfter")
	require.Equal(t, models.ScopeActionFail, e.Action)
}

// A record skips nothing, so nothing is set aside there.
func TestARecordSetsNoMissAsideForASkippedGroup(t *testing.T) {
	a, _ := wireAgent(t, models.MODE_RECORD)
	ctx := context.Background()
	_, serr := a.ClaimInstance(ctx, models.InstanceReq{Key: "spec/Sharing"}, os.Getpid())
	require.Nil(t, serr)
	require.NoError(t, a.SetScopeSkips(ctx, map[string]string{"spec/Sharing::TestShare": "flaky: flipped"}))
	_, serr = a.ProvenBegin(ctx, begin("TestShare"))
	require.Nil(t, serr)
	_, serr = a.ProvenEnd(ctx, models.ScopeReq{Name: "TestShare", Outcome: "passed"})
	require.Nil(t, serr)
	a.provenMu.Lock()
	defer a.provenMu.Unlock()
	require.Empty(t, a.proven.skipped)
}
