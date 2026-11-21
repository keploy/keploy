package agent

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/models"
)

// A miss a test's end charges is handed to the run's end read with the call
// and its next step whole for the console (models.UnmatchedCall.Charged),
// its tag beside it and no full request; the test's evidence keeps it as
// evidence keeps a miss, the path its repeat note's hint quotes in its shape.
func TestAChargedMissReachesTheRunsReadWholeForTheConsole(t *testing.T) {
	a, p := wireAgent(t, models.MODE_TEST)
	ctx := context.Background()
	hint := `declare sessionCalls: [{host: "127.0.0.1:18094", method: "GET", pathPrefix: "/echo/rates"}].`
	whole := models.UnmatchedCall{
		Protocol: "HTTP", ActualSummary: "GET /echo/rates", Destination: "127.0.0.1:18094", PathTag: "#1f2e3d4c5b6a",
		ClosestMock: "mock-0", MatchPhase: "no_mocks", Kind: models.MissInTest,
		ReceivedReq: "GET /echo/rates HTTP/1.1\r\n\r\n", NextSteps: "This request is identical to mock-0. " + hint,
		Repeat: &models.RepeatNote{Mock: "mock-0", OutsideTests: true, Hint: hint},
	}
	claimSelf(t, a)
	_, serr := a.ProvenBegin(ctx, begin("TestReload"))
	require.Nil(t, serr)
	p.misses = []models.UnmatchedCall{whole}
	_, serr = a.ProvenEnd(ctx, models.ScopeReq{Name: "TestReload", Outcome: "passed"})
	require.Nil(t, serr)

	read, err := a.GetMockErrors(ctx)
	require.NoError(t, err)
	require.Len(t, read, 1)
	got := read[0]
	require.Equal(t, &models.WholeCall{Call: "GET /echo/rates", NextStep: whole.NextSteps}, got.Whole, "the console's call is not whole")
	require.Equal(t, "#1f2e3d4c5b6a", got.PathTag, "the log's tag is lost")
	require.Equal(t, "o::TestReload", got.ChargedTo)
	require.Empty(t, got.ReceivedReq, "a charged miss keeps the full request")
	require.NotContains(t, got.NextSteps, `"/echo/rates"`, "the shaped next step names the path whole")

	ev := windowOf(t, a, "o::TestReload").Evidence
	require.NotNil(t, ev)
	require.Len(t, ev.Misses, 1)
	kept := ev.Misses[0]
	require.Nil(t, kept.Whole, "evidence keeps the whole call")
	require.Empty(t, kept.PathTag, "evidence keeps the run's tag")
	require.NotNil(t, kept.Repeat)
	require.Contains(t, kept.Repeat.Hint, `pathPrefix: "/{}/{}"`, "evidence keeps the hint's path whole")
}
