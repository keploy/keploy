//go:build linux

package agent

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/models"
)

// A timer's run grows as the run reaches the places of its recordings: a
// page's 5 s poll, recorded once in TestA and once in TestB, all of one
// answer (one run). TestA polls once more than it recorded: poll-1, the
// run's last reached, answers again. TestB begins, poll-2 is reached and
// answers, and TestB polls once more too: poll-2 is now the run's last and
// answers again. poll-1, which answered again while it was the last, is no
// later recording of the run that holds poll-2 back.
func TestKeployMockReplayAnswersARunsLastAgainOnceTheRunGrew(t *testing.T) {
	mocks := []*models.Mock{
		pingMock("a-own", "/a", "a", t0.Add(time.Second)),
		pingMock("poll-1", "/jobs", "none", t0.Add(2*time.Second)),
		pingMock("b-own", "/b", "b", t0.Add(11*time.Second)),
		pingMock("poll-2", "/jobs", "none", t0.Add(12*time.Second)),
	}
	a, _ := wireAgent(t, models.MODE_TEST)
	p := newServingProxy()
	a.Proxy = p
	ctx := context.Background()
	require.NoError(t, a.StoreMocks(ctx, nil, mocks))
	require.NoError(t, a.SetScopeTable(ctx, map[string][]string{"o::TestA": {"a-own"}, "o::TestB": {"b-own"}}))
	require.NoError(t, a.SetScopeWorker(ctx, []models.MockEntry{
		{Name: "poll-1", Kind: "Http", Place: "o::TestA", Cause: models.CauseTimer, Stretch: &models.Stretch{Run: "r1"}},
		{Name: "poll-2", Kind: "Http", Place: "o::TestB", Cause: models.CauseTimer, Stretch: &models.Stretch{Run: "r1"}},
	}))
	require.NoError(t, a.SetScopePool(ctx, map[string][]string{}))
	claimSelf(t, a)

	got, e := runTest(t, a, p, "TestA", "/a", "/jobs", "/jobs")
	require.Equal(t, []string{"200 a", "200 none", "200 none"}, got, "TestA: the run's last reached recording answers again")
	require.Empty(t, e.Action, "TestA failed: %s", e.Reason)

	got, e = runTest(t, a, p, "TestB", "/b", "/jobs", "/jobs")
	require.Equal(t, []string{"200 b", "200 none", "200 none"}, got, "TestB: once poll-2 is reached and answered, it answers again")
	require.Empty(t, e.Action, "TestB failed: %s", e.Reason)
}
