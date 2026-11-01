package proxy

import (
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// keepingDoors are the doors that keep the recording they consume in the
// trees: MarkMockAsUsed (enterprise Redis and Aerospike consume a data-plane
// recording through no other door) and UpdateUnFilteredMock (a reusable-tier
// recording's, in HTTP, MySQL and Mongo; keploy's Generic parser consumes
// every recording through it, TestAPoolExchangeTheGenericParserServesAnswersOnce).
var keepingDoors = map[string]func(mm *MockManager, m *models.Mock) bool{
	"MarkMockAsUsed": func(mm *MockManager, m *models.Mock) bool { return mm.MarkMockAsUsed(*m) },
	"UpdateUnFilteredMock": func(mm *MockManager, m *models.Mock) bool {
		c := *m
		return mm.UpdateUnFilteredMock(m, &c)
	},
}

// pollMarked answers a call as a parser that takes the first recording in
// read order and consumes it through MarkMockAsUsed, a door that never
// evicts, does, reading again when the consume is refused; "a miss" when no
// recording is read.
func pollMarked(t *testing.T, mm *MockManager, prefix string) string {
	t.Helper()
	return pollKept(t, mm, prefix, keepingDoors["MarkMockAsUsed"])
}

// pollKept is pollMarked through the keeping door consume.
func pollKept(t *testing.T, mm *MockManager, prefix string, consume func(mm *MockManager, m *models.Mock) bool) string {
	t.Helper()
	for i := 0; i < 10; i++ {
		mocks, _ := mm.GetSessionScopedMocks()
		var hit *models.Mock
		for _, mk := range mocks {
			if strings.HasPrefix(mk.Name, prefix) {
				hit = mk
				break
			}
		}
		if hit == nil {
			return "a miss"
		}
		if consume(mm, hit) {
			return hit.Name
		}
	}
	t.Fatalf("%s: every consume was refused", prefix)
	return ""
}

// Under the background rules a pool recording a test consumes through a door
// that keeps it in the trees answers one call in the staging, as one
// consumed through a delete door does
// (TestS12ShowsThePoolInATestAfterOwnAndWorker): spent, and listed spent,
// until the next staging. Before, such a door passed it on and nothing spent
// it, so one pool recording answered every identical call of every test: a
// call the recording never saw passed.
func TestAPoolRecordingAMarkingParserConsumesInATestAnswersOnce(t *testing.T) {
	for door, consume := range keepingDoors {
		t.Run(door, func(t *testing.T) { poolAnswersOnceInATest(t, consume) })
	}
}

func poolAnswersOnceInATest(t *testing.T, consume func(mm *MockManager, m *models.Mock) bool) {
	pollMarked := func(t *testing.T, mm *MockManager, prefix string) string {
		t.Helper()
		return pollKept(t, mm, prefix, consume)
	}
	mm, ms := s12Set(t)
	mm.ServeExactBackground(models.RecordedWithS12Routing, []string{"pool"}, []string{"w-boot", "w-later"})
	mm.ServeExactWorker([]string{"w-boot"})
	mm.ServeExactTest("T1", []*models.Mock{ms["own"]})
	if got := pollMarked(t, mm, "pool"); got != "pool" {
		t.Fatalf("the pool did not answer a test's call through the marking door: %s", got)
	}
	for call := 2; call <= 3; call++ {
		if got := pollMarked(t, mm, "pool"); got != "a miss" {
			t.Fatalf("call %d of the recording the pool holds once was answered by %s; want a miss", call, got)
		}
	}
	equalNames(t, "the background after the pool answered", mm.Background(models.HTTP), "w-boot")
	equalNames(t, "the spent background", mm.SpentBackground(models.HTTP), "pool")
	// The worker scope's, through the same door, as before: once.
	if got := pollMarked(t, mm, "w-boot"); got != "w-boot" {
		t.Fatalf("the reached worker scope did not answer through the marking door: %s", got)
	}
	if got := pollMarked(t, mm, "w-boot"); got != "a miss" {
		t.Fatalf("the worker scope's recording answered twice: %s", got)
	}
	// Spent for the staging, not the view: the next test of the set and the
	// gap after it find it spent.
	mm.ServeExactTest("T2", []*models.Mock{ms["other"]})
	if got := pollMarked(t, mm, "pool"); got != "a miss" {
		t.Fatalf("a pool recording T1 spent answered in T2: %s", got)
	}
	mm.ServeExactPool(models.PoolKeyNone, []string{"pool"})
	if got := pollMarked(t, mm, "pool"); got != "a miss" {
		t.Fatalf("a pool recording a test spent answered again in the gap after it: %s", got)
	}
	// The next staging answers afresh.
	mm.SetMocksWithWindow(nil, []*models.Mock{ms["own"], ms["other"], ms["w-boot"], ms["w-later"], ms["pool"]}, models.BaseTime, time.Now())
	mm.ServeExactBackground(models.RecordedWithS12Routing, []string{"pool"}, []string{"w-boot", "w-later"})
	mm.ServeExactWorker([]string{"w-boot"})
	mm.ServeExactTest("T1", []*models.Mock{ms["own"]})
	if got := pollMarked(t, mm, "pool"); got != "pool" {
		t.Fatalf("the next staging's pool did not answer: %s", got)
	}
}

// Under the background rules a pool recording consumed between tests
// through a door that keeps it in the trees is spent for the staging, as one
// consumed through a delete door is (consumeOutside): it answers no later
// call, in the gap or in a test. Before, the gap's call passed it on unspent,
// and the next test's identical call was answered by it again.
func TestAPoolRecordingAMarkingParserConsumesBetweenTestsIsSpentForTheStaging(t *testing.T) {
	for door, consume := range keepingDoors {
		t.Run(door, func(t *testing.T) { poolSpentBetweenTests(t, consume) })
	}
}

func poolSpentBetweenTests(t *testing.T, consume func(mm *MockManager, m *models.Mock) bool) {
	pollMarked := func(t *testing.T, mm *MockManager, prefix string) string {
		t.Helper()
		return pollKept(t, mm, prefix, consume)
	}
	mm, ms := s12Set(t)
	mm.ServeExactBackground(models.RecordedWithS12Routing, []string{"pool"}, []string{"w-boot", "w-later"})
	mm.ServeExactWorker(nil)
	mm.ServeExactPool(models.PoolKeyNone, []string{"pool"})
	if got := pollMarked(t, mm, "pool"); got != "pool" {
		t.Fatalf("the pool slice did not answer a call between tests: %s", got)
	}
	if got := pollMarked(t, mm, "pool"); got != "a miss" {
		t.Fatalf("a pool recording answered twice between tests: %s", got)
	}
	equalNames(t, "the spent background", mm.SpentBackground(models.HTTP), "pool")
	mm.ServeExactTest("T1", []*models.Mock{ms["own"]})
	if got := pollMarked(t, mm, "pool"); got != "a miss" {
		t.Fatalf("a pool recording the gap spent answered the next test's call: %s", got)
	}
}

// What the background rules spend through the marking door is the pool's
// data-plane recordings only. A protocol-phase recording the pool names (the
// pool is every recording no test's mapping lists: a connection's config
// exchange, say) answers every call through it, in a test and between tests,
// as before; so does a set served by today's rules, whose pool slice the
// marking door never spent.
func TestTheMarkingDoorSpendsOnlyTheBackgroundsDataPlanePool(t *testing.T) {
	t.Run("protocol-phase", func(t *testing.T) {
		mm, ms := s12Set(t)
		hello := configMock("hello", 5*time.Second)
		mm.SetMocksWithWindow(nil, []*models.Mock{ms["own"], ms["other"], ms["w-boot"], ms["w-later"], ms["pool"], hello}, models.BaseTime, time.Now())
		mm.ServeExactBackground(models.RecordedWithS12Routing, []string{"pool", "hello"}, []string{"w-boot", "w-later"})
		mm.ServeExactWorker(nil)
		mm.ServeExactPool(models.PoolKeyNone, []string{"pool", "hello"})
		for call := 1; call <= 3; call++ {
			if got := pollMarked(t, mm, "hello"); got != "hello" {
				t.Fatalf("between tests, call %d of a protocol-phase recording was answered by %s", call, got)
			}
		}
		mm.ServeExactTest("T1", []*models.Mock{ms["own"]})
		for call := 1; call <= 3; call++ {
			if got := pollMarked(t, mm, "hello"); got != "hello" {
				t.Fatalf("in a test, call %d of a protocol-phase recording was answered by %s", call, got)
			}
		}
	})
	t.Run("async lane", func(t *testing.T) {
		// An async lane's recording keeps today's rules under the background
		// rules, as in a test (TestS12LeavesAnAsyncPoolRecordingOutOfATest).
		mm, ms := s12Set(t)
		lane := httpCall("lane", 5*time.Second, "/lane")
		lane.Spec.Async = &models.AsyncMeta{}
		mm.SetMocksWithWindow(nil, []*models.Mock{ms["own"], ms["other"], ms["w-boot"], ms["w-later"], ms["pool"], lane}, models.BaseTime, time.Now())
		mm.ServeExactBackground(models.RecordedWithS12Routing, []string{"pool", "lane"}, []string{"w-boot", "w-later"})
		mm.ServeExactWorker(nil)
		mm.ServeExactPool(models.PoolKeyNone, []string{"pool", "lane"})
		for call := 1; call <= 3; call++ {
			if got := pollMarked(t, mm, "lane"); got != "lane" {
				t.Fatalf("between tests, call %d of an async lane's recording was answered by %s", call, got)
			}
		}
	})
	t.Run("today's rules", func(t *testing.T) {
		mm, _ := s12Set(t)
		mm.ServeExactBackground("", []string{"pool"}, []string{"w-boot", "w-later"})
		mm.ServeExactWorker(nil)
		mm.ServeExactPool(models.PoolKeyNone, []string{"pool"})
		for call := 1; call <= 3; call++ {
			if got := pollMarked(t, mm, "pool"); got != "pool" {
				t.Fatalf("today's rules: call %d between tests was answered by %s", call, got)
			}
		}
	})
}

// keploy's Generic parser consumes every recording, filtered or not, only
// through UpdateUnFilteredMock, a door that keeps it in the trees. Under the
// background rules one data-plane exchange in the pool (stated per-test, as
// the recorder writes a connection's later exchanges) that no test owns
// answers one identical exchange in the staging, in a test and between
// tests; the next two miss. Before, it answered all three.
func TestAPoolExchangeTheGenericParserServesAnswersOnce(t *testing.T) {
	for _, inTest := range []bool{true, false} {
		name := "between tests"
		if inTest {
			name = "in a test"
		}
		t.Run(name, func(t *testing.T) {
			t.Setenv(provenswitch.Env, "1")
			now := time.Now()
			start := now.Add(time.Second)
			perTest := func() map[string]string {
				return map[string]string{"type": "config", models.MetadataLifetimeKey: models.MetadataLifetimePerTest}
			}
			pool := ownRepeatExchange("mock-pool-info", "INFO", "INF0", now)
			pool.Spec.Metadata = perTest()
			own := ownRepeatExchange("mock-own-ping", "PING", "OWN1", start.Add(100*time.Millisecond))
			own.Spec.Metadata = perTest()
			mm := NewMockManager(nil, nil, zap.NewNop())
			t.Cleanup(mm.Close)
			mm.SeedRecordedWindows([]models.TestWindow{{TestCase: "test-0", Start: start, End: start.Add(900 * time.Millisecond)}})
			mm.SetMocksWithWindow([]*models.Mock{own}, []*models.Mock{pool}, models.BaseTime, now.Add(time.Hour))
			poolNames := []string{pool.Name}
			mm.ServeExactBackground(models.RecordedWithS12Routing, poolNames, nil)
			mm.ServeExactPool(models.PoolKeyNone, poolNames)
			if inTest {
				mm.ServeExactTest("test-0", []*models.Mock{roundMock(mm, own.Name)})
			}
			if pool.IsProtocolPhase() || !mm.ExactServing() {
				t.Fatalf("the staging is not a data-plane pool exchange served exactly: protocol-phase %v, exact %v", pool.IsProtocolPhase(), mm.ExactServing())
			}
			if got, _ := genericCall(t, mm, "INFO"); got != "INF0" {
				t.Fatalf("the pool's exchange did not answer the first INFO: %q", got)
			}
			for call := 2; call <= 3; call++ {
				if got, _ := genericCall(t, mm, "INFO"); got != "" {
					t.Fatalf("call %d of the exchange the pool holds once was answered by the pool (%q); want a miss", call, got)
				}
			}
			equalNames(t, "the spent background", mm.SpentBackground(models.GENERIC), pool.Name)
		})
	}
}
