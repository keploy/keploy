package agent

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/models"
)

// casesProxy is a wireProxy that takes the requests each client test sent in
// its recording (coreAgent.ClientCasesTaker), and says in which order it was
// given them and opened the test's capture window.
type casesProxy struct {
	*wireProxy
	order []string
	tests []string
	taken [][]models.ClientCase
}

func (p *casesProxy) TakeClientCases(test string, cases []models.ClientCase) {
	p.order, p.tests, p.taken = append(p.order, "cases"), append(p.tests, test), append(p.taken, cases)
}

func (p *casesProxy) ServeExactTest(test string, owned []*models.Mock) error {
	p.order = append(p.order, "view")
	return p.wireProxy.ServeExactTest(test, owned)
}

func (p *casesProxy) ContinueTestErrorCapture() {
	p.order = append(p.order, "open")
	p.wireProxy.ContinueTestErrorCapture()
}

// A replayed test's begin gives the proxy's learn step the requests its
// client test sent in its recording, once the test's capture window opened
// (the tracker begins the test, which forgets the last test's), and before
// the test's view opens (the proxy notes the variables bound at them as the
// test's scope begins there): a test whose recording kept none gives none,
// and a new session forgets them.
func TestABeginGivesTheLearnStepTheRequestsItsTestSent(t *testing.T) {
	a, wp := wireAgent(t, models.MODE_TEST)
	p := &casesProxy{wireProxy: wp}
	a.Proxy = p
	ctx := context.Background()
	claimSelf(t, a)
	sent := []models.ClientCase{{Name: "post-apps-1", Req: models.HTTPReq{Method: "POST", URL: "http://api/apps", Body: `{"name":"app-1"}`}}}
	a.SetTestCases(ctx, map[string][]models.ClientCase{"o::TestA": sent, "o::TestEmpty": {}, "o::TestC": sent})
	require.NotContains(t, a.testCases, "o::TestEmpty", "a test that sent nothing is kept")
	require.Len(t, a.testCases, 2)
	sent[0].Name = "changed"
	_, serr := a.ProvenBegin(ctx, begin("TestA"))
	require.Nil(t, serr)
	_, serr = a.ProvenEnd(ctx, models.ScopeReq{Name: "TestA", Outcome: models.ScopeOutcomePassed})
	require.Nil(t, serr)
	_, serr = a.ProvenBegin(ctx, begin("TestB"))
	require.Nil(t, serr)
	require.Equal(t, []string{"open", "cases", "view", "open", "cases", "view"}, p.order, "the proxy notes the variables bound at a test's cases as the test's view opens, after its cases")
	require.Equal(t, []string{"o::TestA", "o::TestB"}, p.tests, "each test's requests are given as its own, so a variable bound at one binds in that test")
	require.Len(t, p.taken, 2)
	require.Len(t, p.taken[0], 1)
	require.Equal(t, "post-apps-1", p.taken[0][0].Name, "the agent keeps its own copy of the requests")
	require.Nil(t, p.taken[1], "a test whose recording kept no request")
	a.SetTestCases(ctx, nil)
	require.Nil(t, a.testCases)
}

// The ingress cannot tell which test a request is of, so while another
// test worker's test is open no request pairs: a test begun beside one gives
// the learn step none, and a test begun in another worker stops the run's
// window's test pairing. A proxy that reads no client test's request is
// given none.
func TestATestBesideAnotherWorkersGivesTheLearnStepNoRequests(t *testing.T) {
	a, wp := wireAgent(t, models.MODE_TEST)
	p := &casesProxy{wireProxy: wp}
	a.Proxy = p
	sent := []models.ClientCase{{Name: "post-apps-1", Req: models.HTTPReq{Method: "POST", URL: "http://api/apps"}}}
	a.SetTestCases(context.Background(), map[string][]models.ClientCase{"o::TestA": sent})
	a.giveClientCases("o::TestA", 0, false)
	a.giveClientCases("o::TestA", 0, true)
	a.giveClientCases("o::TestA", 2, false)
	require.Len(t, p.taken, 3)
	require.Len(t, p.taken[0], 1, "the run's window's test alone")
	require.Nil(t, p.taken[1], "begun beside another worker's test")
	require.Nil(t, p.taken[2], "begun in another worker")
	a.Proxy = wp
	a.giveClientCases("o::TestA", 0, false)
}
