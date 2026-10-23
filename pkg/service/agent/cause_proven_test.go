package agent

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/models"
)

// In a record, the calls the open test's harness saw a repeating timer make
// are kept, in order, with the test's window, for keploy mock record to give
// their recordings to the set's worker scope. One reported while no test is
// open is no test's, and is taken unkept; a report that does not name a
// timer's call is refused.
func TestARecordKeepsTheTimersCallsWithTheTestsWindow(t *testing.T) {
	a, _ := wireAgent(t, models.MODE_RECORD)
	ctx := context.Background()
	claimSelf(t, a)
	poll := models.CauseReq{Method: "POST", Host: "localhost:8083", URL: "/query", Body: "abc", N: 1, Cause: models.CauseTimer}
	require.Nil(t, a.Cause(ctx, poll), "a tick between tests")

	_, serr := a.ProvenBegin(ctx, begin("TestJobs"))
	require.Nil(t, serr)
	require.Nil(t, a.Cause(ctx, poll))
	ping := models.CauseReq{Method: "GET", Host: "localhost:8083", URL: "/session", N: 0, Cause: models.CauseTimer}
	require.Nil(t, a.Cause(ctx, ping))
	serr = a.Cause(ctx, models.CauseReq{Method: "GET", URL: "/x", Cause: "click"})
	require.NotNil(t, serr)
	require.Equal(t, http.StatusBadRequest, serr.Status)
	_, serr = a.ProvenEnd(ctx, models.ScopeReq{Name: "TestJobs", Outcome: "passed"})
	require.Nil(t, serr)
	require.Equal(t, []models.TimerCall{
		{Method: "POST", Host: "localhost:8083", URL: "/query", Body: "abc", N: 1},
		{Method: "GET", Host: "localhost:8083", URL: "/session", N: 0},
	}, windowOf(t, a, "o::TestJobs").Timers)
}

// A replay takes a timer's call unkept: its recording is in the worker scope
// already.
func TestAReplayTakesTheTimersCallsUnkept(t *testing.T) {
	a, _ := wireAgent(t, models.MODE_TEST)
	ctx := context.Background()
	claimSelf(t, a)
	_, serr := a.ProvenBegin(ctx, begin("TestJobs"))
	require.Nil(t, serr)
	require.Nil(t, a.Cause(ctx, models.CauseReq{Method: "GET", URL: "/jobs", Cause: models.CauseTimer}))
	_, serr = a.ProvenEnd(ctx, models.ScopeReq{Name: "TestJobs", CanSkip: true, Outcome: "passed"})
	require.Nil(t, serr)
	require.Empty(t, windowOf(t, a, "o::TestJobs").Timers)
}
