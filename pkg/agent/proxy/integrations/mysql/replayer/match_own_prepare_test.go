package replayer_test

import (
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/replayer"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// Under the proven switch, on a set served by the background rules, a
// connection's PREPARE the open test does not own answers the test's PREPARE
// of its statement, though the test owns a PREPARE of another statement.
// Every recording no mapping lists is the pool's (newExactPlan), so the
// connection's PREPARE is named in the pool, and its consume asks whether the
// call is one of the test's own (ownRepeats). The dry match that answers
// reads the connection's prepared statements as the matcher does, and finds
// the connection's recording of the statement: no recording of the test's,
// so it is passed over (own_repeat.go). Counted as the test's own, the
// PREPARE was taken for one more than the test recorded, and the
// connection's recording was refused to it.
//
// keploy maps no connection-lifetime mock to a test (syncMock
// attributedReusable, keploy mock record's dataPlane), so no test of a
// recording keploy writes owns a PREPARE. The replay still owns every mapped
// recording it holds, protocol phase included (newExactPlan), so a mapping
// that lists a PREPARE under a test, as one written by hand may, reaches it.
func TestAConnectionsPrepareIsNotOneOfTheTestsOwnPrepares(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	s1, _ := diffWindow(1)
	conn := diffPrepareMock("conn-prep-a", "c1", diffQueryA, 7, diffBase.Add(-time.Millisecond))
	own := diffPrepareMock("t1-prep-b", "c1", diffQueryB, 11, s1.Add(time.Millisecond))
	mm := proxy.NewMockManager(nil, nil, zap.NewNop())
	defer mm.Close()
	mm.SetMocksWithWindow(nil, []*models.Mock{conn, own}, models.BaseTime, time.Now().Add(time.Hour))
	seedWindows(mm, 2)
	mm.ServeExactBackground(models.RecordedWithS12Routing, []string{conn.Name}, nil)
	mm.ServeExactTest("T1", []*models.Mock{own})
	if !mm.ExactServing() {
		t.Fatal("precondition: test T1 is not served exactly")
	}
	ctx, dctx := betweenTestsConn()
	resp, ok, miss, err := replayer.MatchForTest(ctx, zap.NewNop(), prepareReq(diffQueryA), mm, dctx)
	if err != nil || !ok || resp == nil {
		t.Fatalf("T1's PREPARE of statement A: ok=%v err=%v miss=%s", ok, err, miss)
	}
	if resp.Payload != conn.Name {
		t.Fatalf("T1's PREPARE of statement A, which it does not own, was answered with %q; want the connection's %s", resp.Payload, conn.Name)
	}
}
