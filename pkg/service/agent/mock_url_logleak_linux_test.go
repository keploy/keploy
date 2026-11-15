package agent

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/internal/logleak"
	"go.keploy.io/server/v3/pkg/models"
)

// A recorded call a test never made is named, in the evidence its harness
// and the run's receipts get, by its mock's URL (models.MockURLShape): the
// host whole, as the console and the report name a host, the path by its
// shape. That text goes nowhere else: the agent's whole log of the test's
// begin and end names no host from it (a name is traffic), so a line that
// takes up the evidence's text fails here.
func TestTheEvidenceOfANeverMadeCallKeepsItsHostOutOfTheLog(t *testing.T) {
	const s = logleak.Canary
	a, _ := wireAgent(t, models.MODE_TEST)
	logger, logs := logleak.Observe()
	a.logger = logger
	ctx := context.Background()
	host := s + ".tenant.example"
	storeMocks(a, httpMock("mock-1", "POST", "http://"+host+":18282/audit"))
	a.scopeTable = map[string][]string{"o::TestA": {"mock-1"}}
	claimSelf(t, a)
	_, serr := a.ProvenBegin(ctx, begin("TestA"))
	require.Nil(t, serr)
	e, serr := a.ProvenEnd(ctx, models.ScopeReq{Name: "TestA", Outcome: "passed"})
	require.Nil(t, serr)
	require.Equal(t, models.ScopeActionFail, e.Action)
	require.Contains(t, e.Reason, "recorded call POST "+host+":18282/audit (mock-1) was never made", "the harness's evidence names the host whole")
	require.NotZero(t, logs.Len(), "precondition: the test's begin and end logged nothing; the test drove no line")
	logleak.Check(t, logs, s, "the agent's lines about a test that never made a recorded call")
}
