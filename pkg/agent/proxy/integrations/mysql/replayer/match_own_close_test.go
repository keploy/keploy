package replayer_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/replayer"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire"
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
// Which mapping lists a CLOSE under a test depends on who writes it. An agent
// that maps the recording itself (syncMock: keploy record --sync, or
// enterprise's agent-side mapping, which the background rules serve) maps a
// CLOSE to the test whose window holds it, and a PREPARE to no test. keploy
// record's own window mappings (no --sync) list neither. keploy mock record's
// app-start path maps both by window. The replay owns whatever a mapping
// lists (newExactPlan), and this row stages such a mapping: the test's CLOSE
// listed under it, and one made between tests, which no mapping lists, the
// pool's. Both are connection recordings, so the dry match that counts the
// test's own (ownRepeats) reads the pool's CLOSE too (GetConnectionMocks). It
// finds the test's own CLOSE first, then the pool's, which ends the count
// (own_repeat.go). Counted as the test's own, the miss said the test recorded
// the CLOSE twice.
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
	if !strings.Contains(miss, "extraCall:1 extraOf:t1-close-a}") {
		t.Fatalf("the second CLOSE of statement A missed with %s; want an extra call, recorded once, repeating the test's own CLOSE t1-close-a", miss)
	}
}

// Under the proven switch, on a set served by the background rules, a test
// that owns a CLOSE of one statement closes another one, whose CLOSE the pool
// holds: the pool's CLOSE answers it, as it does when the test owns no
// CLOSE. Any recorded CLOSE matches a live one by its header alone, so the
// test's CLOSE of statement A matches the live CLOSE of statement B too, at a
// lower score than the pool's CLOSE of B. The own-call check (ownRepeats)
// passed the pool's CLOSE over and found the test's, so it took the CLOSE for
// one more than the test recorded: the background was hidden from it, it
// missed as an extra call, and the miss closed the app's connection.
func TestACloseOfAStatementThePoolClosedIsThePoolsThoughTheTestOwnsAnotherClose(t *testing.T) {
	for _, ownsCloseA := range []bool{true, false} {
		t.Run(fmt.Sprintf("test owns a CLOSE of A: %v", ownsCloseA), func(t *testing.T) {
			t.Setenv(provenswitch.Env, "1")
			s1, _ := diffWindow(1)
			prepA := diffPrepareMock("conn-prep-a", "c1", diffQueryA, 7, diffBase.Add(-2*time.Millisecond))
			prepB := diffPrepareMock("conn-prep-b", "c1", diffQueryB, 8, diffBase.Add(-time.Millisecond))
			closeOf := func(name string, id uint32, at time.Time) *models.Mock {
				mk := diffMock(name, "c1", []mysql.Request{closeReq(id)}, at, models.LifetimeConnection, "mocks")
				mk.Spec.Metadata[models.MetadataLifetimeKey] = models.MetadataLifetimeConnection
				return mk
			}
			ownA := closeOf("t1-close-a", 7, s1.Add(time.Millisecond))
			poolB := closeOf("pool-close-b", 8, s1.Add(7*time.Millisecond))
			all, own := []*models.Mock{prepA, prepB, poolB}, []*models.Mock(nil)
			if ownsCloseA {
				all, own = append(all, ownA), []*models.Mock{ownA}
			}
			mm := proxy.NewMockManager(nil, nil, zap.NewNop())
			defer mm.Close()
			mm.SetMocksWithWindow(nil, all, models.BaseTime, time.Now().Add(time.Hour))
			seedWindows(mm, 3)
			mm.ServeExactBackground(models.RecordedWithS12Routing, []string{prepA.Name, prepB.Name, poolB.Name}, nil)
			mm.ServeExactTest("T1", own)
			if !mm.ExactServing() {
				t.Fatal("precondition: test T1 is not served exactly")
			}
			ctx := context.WithValue(context.Background(), models.ClientConnectionIDKey, "c1")
			dctx := &wire.DecodeContext{PreparedStatements: map[uint32]*mysql.StmtPrepareOkPacket{}, StmtIDToQuery: btStatements, NextStmtID: 100}
			if ownsCloseA {
				if _, ok, miss, err := replayer.MatchForTest(ctx, zap.NewNop(), closeReq(7), mm, dctx); !ok || err != nil {
					t.Fatalf("T1's CLOSE of statement A, which it recorded: ok=%v err=%v miss=%s", ok, err, miss)
				}
			}
			if _, ok, miss, err := replayer.MatchForTest(ctx, zap.NewNop(), closeReq(8), mm, dctx); !ok || err != nil {
				t.Fatalf("T1's CLOSE of statement B, which the pool holds and T1 never recorded, missed (ok=%v err=%v): %s", ok, err, miss)
			}
			if n := mm.ServedCounts()[poolB.Name]; n != 1 {
				t.Fatalf("T1's CLOSE of statement B was answered, but the pool's CLOSE of B answered %d calls; want 1 (served: %v)", n, mm.ServedCounts())
			}
		})
	}
}
