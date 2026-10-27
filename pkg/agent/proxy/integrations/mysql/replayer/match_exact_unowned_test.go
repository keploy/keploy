package replayer_test

import (
	"fmt"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/replayer"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.uber.org/zap"
)

// unownedExecs are recordings of poolQuery on connection c1 that no test
// owns, of lifetime lt, one per id in ids, in that order: each answers with
// its name, prefix-<id>.
func unownedExecs(prefix string, lt models.Lifetime, ids ...string) []*models.Mock {
	var out []*models.Mock
	for i, id := range ids {
		at := diffBase.Add(time.Duration(i+1) * time.Millisecond)
		out = append(out, diffMock(prefix+"-"+id, "c1", []mysql.Request{executeReq(1, id)}, at, lt, "mocks"))
	}
	return out
}

func mockNamesOf(ms []*models.Mock) []string {
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = m.Name
	}
	return out
}

// expectAnswers runs, on connection c1, an EXECUTE of statement 1 with each
// of ids in turn, and requires each to be answered with want(id).
func expectAnswers(t *testing.T, mm *proxy.MockManager, want func(id string) string, ids ...string) {
	t.Helper()
	ctx, dctx := poolConn("c1")
	for _, id := range ids {
		resp, ok, miss, err := replayer.MatchForTest(ctx, zap.NewNop(), executeReq(1, id), mm, dctx)
		if err != nil || !ok {
			t.Fatalf("EXECUTE id=%s: ok=%v err=%v miss=%s", id, ok, err, miss)
		}
		if w := want(id); resp.Payload != w {
			t.Fatalf("EXECUTE id=%s was answered with %s; want %s", id, resp.Payload, w)
		}
	}
}

// Under exact serving with no test open (the pool view: start-up, and between
// tests), a prepared statement run with each parameter it recorded there is
// answered by that parameter's recording, in either order, of either
// lifetime. No recording there is a test's own, and the first recording of
// the statement with other parameters (the FIFO candidate) outranked the one
// recorded with the command's own: id 1 was answered with id 2's rows, and id
// 2 with id 1's. Every Go app whose start-up reads one statement with two
// arguments (go-sql-driver sends a query with arguments as PREPARE and
// EXECUTE) read each other's rows.
func TestAPreparedStatementRunOutsideEveryTestIsAnsweredByItsParametersRecording(t *testing.T) {
	for _, lt := range []models.Lifetime{models.LifetimePerTest, models.LifetimeSession} {
		for _, phase := range []string{"start-up", "between tests"} {
			for _, order := range [][]string{{"1", "2"}, {"2", "1"}} {
				t.Run(fmt.Sprintf("lifetime=%v/%s/order=%v", lt, phase, order), func(t *testing.T) {
					prep := diffPrepareMock("prep-c1", "c1", poolQuery, 1, diffBase.Add(-time.Millisecond))
					pool := unownedExecs("pool-exec", lt, "1", "2")
					own := diffMock("t-exec-3", "c1", []mysql.Request{executeReq(1, "3")}, diffBase.Add(5*time.Millisecond), models.LifetimePerTest, "mocks")
					mm := proxy.NewMockManager(nil, nil, zap.NewNop())
					t.Cleanup(mm.Close)
					mm.SetMocksWithWindow(nil, append([]*models.Mock{prep, own}, pool...), models.BaseTime, time.Now().Add(time.Hour))
					if phase == "between tests" {
						mm.ServeExactTest("T", []*models.Mock{own})
						expectAnswers(t, mm, func(string) string { return "t-exec-3" }, "3")
					}
					mm.ServeExactPool("pool", append([]string{prep.Name}, mockNamesOf(pool)...))
					if !mm.ExactServing() {
						t.Fatal("precondition: the pool view is not served exactly")
					}
					expectAnswers(t, mm, func(id string) string { return "pool-exec-" + id }, order...)
				})
			}
		}
	}
}

// Under exact serving with a test open, the open test's own recording of a
// prepared statement ranks first, above a recording no test owns that the
// view shows too (the worker scope's), even one recorded with the command's
// own parameters; of two recordings no test owns, the one recorded with the
// command's own parameters ranks above the first one with other parameters.
// The first row pins the ownership read: were the worker scope's recordings
// counted as the test's own, its recording of id 1 would answer.
func TestUnderExactServingTheOpenTestsOwnRecordingRanksFirstThenTheParametersOwn(t *testing.T) {
	t.Run("the test's own with other parameters above the worker scope's with the command's", func(t *testing.T) {
		prep := diffPrepareMock("prep-c1", "c1", poolQuery, 1, diffBase.Add(-time.Millisecond))
		worker := unownedExecs("worker-exec", models.LifetimePerTest, "1")
		own := diffMock("t-exec-2", "c1", []mysql.Request{executeReq(1, "2")}, diffBase.Add(5*time.Millisecond), models.LifetimePerTest, "mocks")
		mm := proxy.NewMockManager(nil, nil, zap.NewNop())
		t.Cleanup(mm.Close)
		mm.SetMocksWithWindow(nil, append([]*models.Mock{prep, own}, worker...), models.BaseTime, time.Now().Add(time.Hour))
		mm.ServeExactWorker(mockNamesOf(worker))
		mm.ServeExactTest("T", []*models.Mock{own})
		expectAnswers(t, mm, func(string) string { return "t-exec-2" }, "1")
	})
	for _, order := range [][]string{{"1", "2"}, {"2", "1"}} {
		t.Run(fmt.Sprintf("of the worker scope's, the command's parameters' own/order=%v", order), func(t *testing.T) {
			prep := diffPrepareMock("prep-c1", "c1", poolQuery, 1, diffBase.Add(-time.Millisecond))
			worker := unownedExecs("worker-exec", models.LifetimePerTest, "1", "2")
			own := diffMock("t-query", "c1", []mysql.Request{queryReq(poolTextQuery)}, diffBase.Add(5*time.Millisecond), models.LifetimePerTest, "mocks")
			mm := proxy.NewMockManager(nil, nil, zap.NewNop())
			t.Cleanup(mm.Close)
			mm.SetMocksWithWindow(nil, append([]*models.Mock{prep, own}, worker...), models.BaseTime, time.Now().Add(time.Hour))
			mm.ServeExactWorker(mockNamesOf(worker))
			mm.ServeExactTest("T", []*models.Mock{own})
			expectAnswers(t, mm, func(id string) string { return "worker-exec-" + id }, order...)
		})
	}
}
