package proxy

import (
	"context"
	"net/http"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// keploy mock record's full round, with the recorded test windows keploy mock
// replay sends, run out of its recorded order: test-2 (recorded after test-1)
// runs before test-1, either first (tests run in another order) or in a run
// of test-1 begun again (go test -count=2, gotestsum --rerun-fails). A late
// call of test-2, exactly its recording mock-own2, lands in test-1. The
// windows say mock-own2 was recorded after test-1's window, so the learn step
// does not read it as another place of the call, and would pair the call
// with test-1's own mock-own1 and learn what it differs in as noise onto it:
// a false green. The agent ends the round's learn step at test-1's begin out
// of order, before its window opens (pkg/service/agent keepRecordedOrder,
// Proxy.SetLearnEnded), and the call then misses, with nothing learned.
func TestAnOutOfOrderBeginEndsLearningSoALaterRecordedTestsLateCallMisses(t *testing.T) {
	for _, rerun := range []bool{false, true} {
		name := map[bool]string{false: "test-2, then test-1", true: "test-1, test-2, then test-1 again"}[rerun]
		t.Run(name, func(t *testing.T) {
			t.Setenv(provenswitch.Env, "1")
			p := startedDrainProxyLogging(t, 100, zap.NewNop())
			p.learnTracker = learn.NewTracker()
			now := time.Now()
			pool := r3Mock("mock-pool", now.Add(100*time.Millisecond), "POST", "/orders", `{"item":"apple"}`, `{"order":0}`)
			own1 := r3Mock("mock-own1", now.Add(700*time.Millisecond), "POST", "/orders", `{"item":"pear"}`, `{"order":1}`)
			own2 := r3Mock("mock-own2", now.Add(1500*time.Millisecond), "POST", "/orders", `{"item":"plum"}`, `{"order":2}`)
			mm := NewMockManager(nil, nil, zap.NewNop())
			// As keploy mock replay derives them from the set's mappings:
			// test-1 began at +0.5s (mock-own1 at +0.7s), test-2 at +1.0s
			// (mock-own2 at +1.5s).
			mm.SeedRecordedWindows([]models.TestWindow{
				{TestCase: "test-1", Start: now.Add(500 * time.Millisecond), End: own1.Spec.ResTimestampMock},
				{TestCase: "test-2", Start: now.Add(time.Second), End: own2.Spec.ResTimestampMock},
			})
			mm.SetMocksWithWindow([]*models.Mock{pool, own1, own2}, nil, models.BaseTime, now.Add(time.Hour))
			p.setMockManager(mm)
			t.Cleanup(func() { p.setMockManager(nil) })
			mm.SetPoolNames([]string{"mock-pool"})
			mm.ServeExactPool(models.PoolKeyNone, []string{"mock-pool"})
			end := func() {
				mm.ServeExactPool(models.PoolKeyNone, []string{"mock-pool"})
				if _, err := p.GetMockErrors(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			if rerun {
				// In recorded order the learn step runs: test-1's own call
				// is answered from its own recording.
				mm.ServeExactTest("test-1", []*models.Mock{roundMock(mm, "mock-own1")})
				p.ContinueTestErrorCapture()
				if code, got := r3Send(t, p, mm, models.OutgoingOptions{Learn: true, LearnPool: true}, roundPost(`{"item":"pear"}`)); code != http.StatusOK || got != `{"order":1}` {
					t.Fatalf("precondition: test-1's own call got %d %s", code, got)
				}
				if mm.LearnEnded() {
					t.Fatal("precondition: in recorded order the learn step runs")
				}
				end()
			}
			// test-2 runs, and ends with its call still in flight.
			mm.ServeExactTest("test-2", []*models.Mock{roundMock(mm, "mock-own2")})
			p.ContinueTestErrorCapture()
			end()
			// test-1 begins out of its recorded order: the agent ends the
			// learn step before its window opens.
			if err := p.SetLearnEnded(true); err != nil {
				t.Fatal(err)
			}
			mm.ServeExactTest("test-1", []*models.Mock{roundMock(mm, "mock-own1")})
			p.ContinueTestErrorCapture()
			code, got, learned, misses := elsewhereSent(t, p, mm, `{"item":"plum"}`)
			if _, ok := learned["mock-own1"]; ok || got == `{"order":1}` {
				t.Fatalf("FALSE GREEN: test-2's late call (exactly mock-own2) was paired with test-1's mock-own1 and its item learned onto it: answered %d %s, learned %v", code, got, learned)
			}
			if code == http.StatusOK {
				t.Fatalf("answered %d %s; want a miss", code, got)
			}
			if len(misses) == 0 {
				t.Fatal("the late call left no miss")
			}
			if len(learned) > 0 {
				t.Fatalf("learned %v once the learn step ended", learned)
			}
			// test-1's own call is still answered from its recording.
			if code, got := r3Send(t, p, mm, models.OutgoingOptions{Learn: true, LearnPool: true}, roundPost(`{"item":"pear"}`)); code != http.StatusOK || got != `{"order":1}` {
				t.Fatalf("test-1's own call got %d %s", code, got)
			}
		})
	}
}
