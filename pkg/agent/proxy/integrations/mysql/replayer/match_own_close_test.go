package replayer_test

import (
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/replayer"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// Under the proven switch, on a set served by the background rules, a test
// that closes its statement once more than it recorded misses that CLOSE as
// an extra call, recorded once: the connection's CLOSE of the statement,
// which the pool holds, is no recording of the test's.
//
// keploy record maps a CLOSE to the test whose window holds it: its recorder
// types it "mocks" and states its lifetime as its connection's, so it is
// reusable only by its statement (models.Mock.ReusableByStatementOnly), and
// syncMock attributedReusable attributes it to its window. One made between
// tests is mapped to no test, and is the pool's. Both are connection
// recordings, so the dry match that counts the test's own (ownRepeats) reads
// the pool's CLOSE too (GetConnectionMocks). It finds the test's own CLOSE
// first, then the pool's, and passes the pool's over (own_repeat.go).
// Counted as the test's own, the miss said the test recorded the CLOSE twice.
func TestACloseOneMoreThanTheTestRecordedCountsOnlyTheTestsOwnClose(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	s1, _ := diffWindow(1)
	prep := diffPrepareMock("conn-prep-a", "c1", diffQueryA, 7, diffBase.Add(-time.Millisecond))
	closeOf := func(name string, at time.Time) *models.Mock {
		mk := diffMock(name, "c1", []mysql.Request{closeReq(7)}, at, models.LifetimeConnection, "mocks")
		mk.Spec.Metadata[models.MetadataLifetimeKey] = models.MetadataLifetimeConnection
		return mk
	}
	own := closeOf("t1-close-a", s1.Add(time.Millisecond))
	pool := closeOf("pool-close-a", s1.Add(7*time.Millisecond))
	if !own.ReusableByStatementOnly() {
		t.Fatal("precondition: the CLOSE is not one keploy record maps to the test whose window holds it")
	}
	mm := proxy.NewMockManager(nil, nil, zap.NewNop())
	defer mm.Close()
	mm.SetMocksWithWindow(nil, []*models.Mock{prep, own, pool}, models.BaseTime, time.Now().Add(time.Hour))
	seedWindows(mm, 3)
	mm.ServeExactBackground(models.RecordedWithS12Routing, []string{prep.Name, pool.Name}, nil)
	mm.ServeExactTest("T1", []*models.Mock{own})
	if !mm.ExactServing() {
		t.Fatal("precondition: test T1 is not served exactly")
	}
	ctx, dctx := betweenTestsConn()

	if _, ok, miss, err := replayer.MatchForTest(ctx, zap.NewNop(), closeReq(7), mm, dctx); !ok || err != nil {
		t.Fatalf("T1's CLOSE of statement A, which it recorded: ok=%v err=%v miss=%s", ok, err, miss)
	}
	_, ok, miss, err := replayer.MatchForTest(ctx, zap.NewNop(), closeReq(7), mm, dctx)
	if ok || err != nil {
		t.Fatalf("T1's second CLOSE of statement A, one more than it recorded, was answered (ok=%v err=%v)", ok, err)
	}
	if !strings.Contains(miss, "extraCall:1}") {
		t.Fatalf("the second CLOSE of statement A missed with %s; want an extra call, recorded once", miss)
	}
}
