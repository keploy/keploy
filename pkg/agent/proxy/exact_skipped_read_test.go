package proxy

import (
	"slices"
	"strings"
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
	// It counts every /x the test made: one it recorded, and one the
	// skipped test's read answered, named.
	if miss.Recorded != 1 || !slices.Equal(miss.SkippedReads, []string{skipped.Name}) {
		t.Fatalf("the third GET /x says recorded %d and skipped reads %v; want 1 and [%s]", miss.Recorded, miss.SkippedReads, skipped.Name)
	}

	mm.ServeExactPool(models.PoolKeyNone, nil)
	if got, miss := httpMissCall(t, mm, "/x"); got != "worker" && got != "skipped" {
		t.Fatalf("the GET /x between tests was answered %q (miss %+v); want the worker scope's", got, miss)
	}
}

// An extra call names only the skipped tests' reads of its own call that
// answered in its test's window: a skipped read of another call the test
// made is no call of this one, so the count of /x never takes it, and a
// worker: recording that answered the test is no skipped test's read.
func TestAnExtraCallNamesOnlyTheSkippedReadsOfItsOwnCall(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	skippedX := workerProbe("mock-skipped-x", time.Second, "/x", "skipped-x")
	skippedY := workerProbe("mock-skipped-y", 2*time.Second, "/y", "skipped-y")
	workerZ := workerProbe("mock-worker-z", 2500*time.Millisecond, "/z", "worker-z")
	own := workerProbe("mock-own-x", 4*time.Second, "/x", "own")
	mm := stagedManager(t, nil, []*models.Mock{skippedX, skippedY, workerZ, own})
	mm.SeedRecordedWindows([]models.TestWindow{{TestCase: "test-a", Start: exactT0.Add(3 * time.Second), End: exactT0.Add(5 * time.Second)}})
	reads := []string{skippedX.Name, skippedY.Name}
	mm.ServeExactWorkerSkipped(append([]string{workerZ.Name}, reads...), reads)
	mm.ServeExactPool(models.PoolKeyNone, nil)
	mm.ServeExactTest("test-a", []*models.Mock{roundMock(mm, own.Name)})
	if !mm.ExactServing() || integrations.S12Background(mm) {
		t.Fatal("precondition: test-a is not served exactly, by today's rules")
	}

	for _, c := range []struct{ path, want string }{{"/y", "skipped-y"}, {"/z", "worker-z"}, {"/x", "own"}, {"/x", "skipped-x"}} {
		if got, miss := httpMissCall(t, mm, c.path); got != c.want {
			t.Fatalf("test-a's GET %s was answered %q (miss %+v); want %q", c.path, got, miss, c.want)
		}
	}
	// The window's account, in the order they answered, read from the
	// manager or through a connection's view of it.
	for _, r := range []integrations.ExactSkippedReadsReader{mm, &scopedMockDb{MockMemDb: mm}} {
		if got := exactNames(r.SkippedReadsAnswered()); !slices.Equal(got, []string{skippedY.Name, skippedX.Name}) {
			t.Fatalf("%T says skipped reads %v answered in test-a's window; want [%s %s]", r, got, skippedY.Name, skippedX.Name)
		}
	}
	got, miss := httpMissCall(t, mm, "/x")
	if got != "" || miss == nil || miss.MissKind != models.MissExtraCall {
		t.Fatalf("test-a's third GET /x was answered %q (miss %+v); want an extra call", got, miss)
	}
	if !slices.Equal(miss.SkippedReads, []string{skippedX.Name}) {
		t.Fatalf("the extra GET /x names skipped reads %v; want only /x's, [%s]", miss.SkippedReads, skippedX.Name)
	}
	// The next test's window starts with none.
	mm.ServeExactPool(models.PoolKeyNone, nil)
	mm.ServeExactTest("test-b", nil)
	if got := mm.SkippedReadsAnswered(); len(got) != 0 {
		t.Fatalf("test-b's window starts with skipped reads %v answered; want none", got)
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

// The window's account of the skipped reads that answered (an extra call's
// count) holds only reads its view spent: a consume the staging refuses
// answered nothing (a copy of the read from another staging, or one another
// connection spent first), and a call of the test in flight as it ended,
// settled once no test is open, counts in no window's account.
func TestASkippedReadsAccountHoldsOnlyWhatTheOpenWindowSpent(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	skippedX := workerProbe("mock-skipped-x", time.Second, "/x", "skipped-x")
	own := workerProbe("mock-own-x", 4*time.Second, "/x", "own")
	mm := stagedManager(t, nil, []*models.Mock{skippedX, own})
	mm.SeedRecordedWindows([]models.TestWindow{{TestCase: "test-a", Start: exactT0.Add(3 * time.Second), End: exactT0.Add(5 * time.Second)}})
	mm.ServeExactWorkerSkipped([]string{skippedX.Name}, []string{skippedX.Name})
	mm.ServeExactPool(models.PoolKeyNone, nil)
	mm.ServeExactTest("test-a", []*models.Mock{roundMock(mm, own.Name)})

	stale := roundMock(mm, skippedX.Name).ShallowCopy()
	stale.Spec.ReqTimestampMock = stale.Spec.ReqTimestampMock.Add(time.Hour)
	if v := mm.exactConsume(stale, true); v != exactRefused {
		t.Fatalf("a copy of the skipped read the staging does not hold was %v; want refused", v)
	}
	if got := exactNames(mm.SkippedReadsAnswered()); len(got) != 0 {
		t.Fatalf("after a refused consume, skipped reads %v answered in test-a's window; want none", got)
	}

	inTest := mm.exactView()
	mm.ServeExactPool(models.PoolKeyNone, nil)
	if v := mm.exactConsumeAt(roundMock(mm, skippedX.Name), true, &roundGuard{free: true, readIn: inTest}, nil, nil); v != exactServed {
		t.Fatalf("test-a's call in flight as it ended was %v; want the skipped read served for the windows", v)
	}
	if got := exactNames(mm.SkippedReadsAnswered()); len(got) != 0 {
		t.Fatalf("with no test open, skipped reads %v answered; want none", got)
	}
}

// A skipped test's read answers at most as many calls as that test recorded
// it (founder, the bound): a call one past it is the miss of the test that
// makes it, in every shape, and names the read it is past as the skipped
// test's (models.RepeatNote.SkippedRead), never as a recording of the
// background. Before, its spend was noted as the worker scope's, so the
// call past the bound was told a background repeat: charged to no test and
// printed as the set's background call, while a test that held its own
// recording of the read failed on the same call as an extra one.
func TestACallPastASkippedTestsReadIsTheCallingTestsMissInEveryShape(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	for _, c := range []struct {
		name  string
		own   bool
		tests [][]string // the /x calls of test-a, then test-b
		want  []string   // each call's answer; "" for the miss
	}{
		{"the test that makes the read makes it twice, with no own recording", false, [][]string{{"/x", "/x"}}, []string{"skipped", ""}},
		{"the next test makes it, with no own recording", false, [][]string{{"/x"}, {"/x"}}, []string{"skipped", ""}},
		{"the first test's own, then the read, then the next test", true, [][]string{{"/x", "/x"}, {"/x"}}, []string{"own", "skipped", ""}},
	} {
		t.Run(c.name, func(t *testing.T) {
			skipped := workerProbe("mock-skipped-x", time.Second, "/x", "skipped")
			staged := []*models.Mock{skipped}
			var own *models.Mock
			if c.own {
				own = workerProbe("mock-own-x", 4*time.Second, "/x", "own")
				staged = append(staged, own)
			}
			mm := stagedManager(t, nil, staged)
			mm.SeedRecordedWindows([]models.TestWindow{
				{TestCase: "test-a", Start: exactT0.Add(3 * time.Second), End: exactT0.Add(5 * time.Second)},
				{TestCase: "test-b", Start: exactT0.Add(6 * time.Second), End: exactT0.Add(7 * time.Second)},
			})
			p := &Proxy{}
			p.setMockManager(mm)
			var ws agent.ExactWorkerSkippedServer = p
			if err := ws.ServeExactWorkerSkipped([]string{skipped.Name}, []string{skipped.Name}); err != nil {
				t.Fatal(err)
			}
			var miss *models.MockMismatchReport
			i := 0
			for n, calls := range c.tests {
				mm.ServeExactPool(models.PoolKeyNone, nil)
				var mine []*models.Mock
				if n == 0 && own != nil {
					mine = []*models.Mock{roundMock(mm, own.Name)}
				}
				mm.ServeExactTest([]string{"test-a", "test-b"}[n], mine)
				if !mm.ExactServing() || integrations.S12Background(mm) {
					t.Fatal("precondition: the test is not served exactly, by today's rules")
				}
				for _, path := range calls {
					got, m := httpMissCall(t, mm, path)
					if got != c.want[i] {
						t.Fatalf("call %d (GET %s) was answered %q (miss %+v); want %q", i+1, path, got, m, c.want[i])
					}
					if got == "" {
						miss = m
					}
					i++
				}
			}
			if miss == nil {
				t.Fatal("no call missed")
			}
			if miss.MissKind == models.MissExtraCall {
				t.Fatalf("the call past the bound missed as %q; the test that made it recorded no such call", miss.MissKind)
			}
			if miss.Repeat == nil || miss.Repeat.Mock != skipped.Name || miss.ClosestMock != skipped.Name {
				t.Fatalf("the call past the bound missed with closest %q and note %+v; want both naming the skipped test's read %s", miss.ClosestMock, miss.Repeat, skipped.Name)
			}
			if miss.Repeat.Background != "" {
				t.Fatalf("the note calls the skipped test's read a recording of the background %q: the call would be charged to no test", miss.Repeat.Background)
			}
			if !miss.Repeat.SkippedRead {
				t.Fatalf("the note does not say %s is a skipped test's read: %+v", skipped.Name, miss.Repeat)
			}
			if !strings.Contains(miss.NextSteps, "the read of a test this replay skips") || strings.Contains(miss.NextSteps, "worker scope (mappings.yaml worker:), which answers at most one call") {
				t.Fatalf("the miss's next step does not name the skipped test's read, or calls it the worker scope's: %s", miss.NextSteps)
			}
			if !strings.Contains(miss.NextSteps, "keploy does not answer a call past what the skipped test recorded") || strings.Contains(miss.NextSteps, "on its own schedule") {
				t.Fatalf("the miss's next step does not say the call is past the skipped test's read, or calls it the application's background work: %s", miss.NextSteps)
			}
		})
	}
}
