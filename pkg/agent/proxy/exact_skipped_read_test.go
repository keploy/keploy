package proxy

import (
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// A read the app made once and kept (a token it looks up and caches) is
// recorded in the window of the first test that made it. Once a replay's
// gate skips that test, keploy mock replay moves the read to the worker
// scope (ServeExactWorkerSkipped), and the app makes it in the window of
// whichever test runs first. When that test holds its own recording of the
// same read, it now makes the read once more than it recorded: the skipped
// test's read answers that call, after the test's own, and only once. A
// call past it misses as one more than the test recorded, and a worker:
// recording of the same read that is no skipped test's stays hidden from
// it. Without this, the moved read was hidden from the test's second call
// like any worker: recording, the test failed ("called 2 times, recorded
// 1"), judging set it aside, and the read moved on to the next test in every
// later run until no test was kept.
func TestASkippedTestsReadAnswersOneCallMoreThanTheTestRecordedOnce(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	skipped := workerProbe("mock-skipped-x", time.Second, "/x", "skipped")
	worker := workerProbe("mock-worker-x", 2*time.Second, "/x", "worker")
	own := workerProbe("mock-own-x", 4*time.Second, "/x", "own")
	mm := stagedManager(t, nil, []*models.Mock{skipped, worker, own})
	mm.SeedRecordedWindows([]models.TestWindow{{TestCase: "test-a", Start: exactT0.Add(3 * time.Second), End: exactT0.Add(5 * time.Second)}})
	workerNames := []string{skipped.Name, worker.Name}
	p := &Proxy{}
	p.setMockManager(mm)
	var ws agent.ExactWorkerSkippedServer = p
	if err := ws.ServeExactWorkerSkipped(workerNames, []string{skipped.Name}); err != nil {
		t.Fatal(err)
	}
	mm.ServeExactPool(models.PoolKeyNone, nil)
	mm.ServeExactTest("test-a", []*models.Mock{roundMock(mm, own.Name)})
	if !mm.ExactServing() || integrations.S12Background(mm) {
		t.Fatal("precondition: test-a is not served exactly, by today's rules")
	}

	if got, miss := httpMissCall(t, mm, "/x"); got != "own" {
		t.Fatalf("test-a's GET /x was answered %q (miss %+v); want its own first", got, miss)
	}
	if got, miss := httpMissCall(t, mm, "/x"); got != "skipped" {
		t.Fatalf("test-a's second GET /x, the skipped test's read, was answered %q (miss %+v); want the skipped test's recording", got, miss)
	}
	got, miss := httpMissCall(t, mm, "/x")
	if got != "" {
		t.Fatalf("test-a's third GET /x, one past the skipped test's one recording, was answered %q", got)
	}
	if miss.MissKind != models.MissExtraCall {
		t.Fatalf("the third GET /x missed as %q; want %q", miss.MissKind, models.MissExtraCall)
	}

	mm.ServeExactPool(models.PoolKeyNone, nil)
	if got, miss := httpMissCall(t, mm, "/x"); got != "worker" && got != "skipped" {
		t.Fatalf("the GET /x between tests was answered %q (miss %+v); want the worker scope's", got, miss)
	}
}

// Only a name of the worker scope is a skipped test's read. Under the
// background rules a pool recording is hidden from a call one more than the
// open test's own recordings of it; naming it among the skipped reads while
// the worker scope does not hold it hides it still: the second PING misses
// as an extra call, and the pool's recording answers between tests.
func TestASkippedReadTheWorkerScopeDoesNotHoldStaysHidden(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	now := time.Now()
	start := now.Add(time.Second)
	pool := ownRepeatExchange("mock-pool-ping", "PING", "POOL", now)
	own := ownRepeatExchange("mock-own-ping", "PING", "OWN1", start.Add(100*time.Millisecond))
	mm := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	mm.SeedRecordedWindows([]models.TestWindow{{TestCase: "test-0", Start: start, End: start.Add(900 * time.Millisecond)}})
	mm.SetMocksWithWindow([]*models.Mock{own}, []*models.Mock{pool}, models.BaseTime, now.Add(time.Hour))
	poolNames := []string{pool.Name}
	mm.ServeExactBackground(models.RecordedWithS12Routing, poolNames, nil)
	mm.ServeExactWorkerSkipped(nil, poolNames)
	mm.ServeExactPool(models.PoolKeyNone, poolNames)
	mm.ServeExactTest("test-0", []*models.Mock{roundMock(mm, own.Name)})
	if !mm.ExactServing() {
		t.Fatal("precondition: test-0 is not served exactly")
	}

	if got, miss := genericCall(t, mm, "PING"); got != "OWN1" {
		t.Fatalf("test-0's PING was answered %q (miss %+v); want its own OWN1", got, miss)
	}
	got, miss := genericCall(t, mm, "PING")
	if got != "" {
		t.Fatalf("test-0's second PING, one more than it recorded, was answered %q by a pool recording named as a skipped read", got)
	}
	if miss.MissKind != models.MissExtraCall {
		t.Fatalf("the second PING missed as %q; want %q", miss.MissKind, models.MissExtraCall)
	}
	mm.ServeExactPool(models.PoolKeyNone, poolNames)
	if got, miss := genericCall(t, mm, "PING"); got != "POOL" {
		t.Fatalf("the PING between tests was answered %q (miss %+v); want the pool's POOL", got, miss)
	}
}
