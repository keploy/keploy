package proxy

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// callViewRun is a store served exactly whose test-0 owns own, a PING its
// first call answered: test-0's view lists own as answered.
func callViewRun(t *testing.T) (*MockManager, *models.Mock) {
	t.Helper()
	t.Setenv(provenswitch.Env, "1")
	now := time.Now()
	start := now.Add(time.Second)
	own := ownRepeatExchange("mock-own-ping", "PING sk9LIVEsecret77", "OWN1", start.Add(100*time.Millisecond))
	mm := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	mm.SeedRecordedWindows([]models.TestWindow{{TestCase: "test-0", Start: start, End: start.Add(900 * time.Millisecond)}})
	mm.SetMocksWithWindow([]*models.Mock{own}, nil, models.BaseTime, now.Add(time.Hour))
	mm.ServeExactTest("test-0", []*models.Mock{roundMock(mm, own.Name)})
	if !mm.ExactServing() {
		t.Fatal("precondition: test-0 is not served exactly")
	}
	if got, miss := genericCall(t, mm, "PING sk9LIVEsecret77"); got != "OWN1" {
		t.Fatalf("precondition: test-0's PING was answered %q (miss %+v); want its own OWN1", got, miss)
	}
	return mm, own
}

func answeredNames(ms []*models.Mock) []string {
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		out = append(out, m.Name)
	}
	return out
}

// A call held to its round is told the test mocks that answered in the view
// its reads are made in (heldScope.ConsumedTestMocks), as its other reads
// are: a call read in test-0's view is told test-0's answered recording after
// test-0 ended, the pool view in force listing none; through a connection's
// scoped view too. Such a call never reports its view moved
// (CallViewMoved). A call that reads the view in force (decided in the test
// the store opens next: TrackerAhead) is told the view in force's, and
// reports its view moved once a test's begin or end installed another view
// since it was held, and not before. Before, every held call was told the
// view in force's answered recordings: none once test-0 ended, so a miss
// report built then found no recording that redacted what the call sent.
func TestAHeldCallIsToldTheAnsweredRecordingsOfTheViewItReadsIn(t *testing.T) {
	for _, scoped := range []bool{false, true} {
		for _, ahead := range []bool{false, true} {
			t.Run(map[bool]string{false: "the manager", true: "a scoped view"}[scoped]+"/"+map[bool]string{false: "read in its own view", true: "reading the view in force"}[ahead], func(t *testing.T) {
				mm, own := callViewRun(t)
				var store integrations.MockMemDb = mm
				if scoped {
					store = &scopedMockDb{MockMemDb: mm}
				}
				call := integrations.MarkCallRound(store)
				if ahead {
					call = call.WithTrackerAhead(true)
				}
				held, _ := integrations.HoldToRound(store, call)
				er, ok := held.(integrations.ExactConsumedReader)
				if !ok {
					t.Fatalf("the held store %T tells no answered recordings", held)
				}
				if got := answeredNames(er.ConsumedTestMocks()); len(got) != 1 || got[0] != own.Name || integrations.CallViewMoved(held) {
					t.Fatalf("in test-0 the held call is told %v answered, moved %v; want [%s], not moved", got, integrations.CallViewMoved(held), own.Name)
				}
				mm.ServeExactPool(models.PoolKeyNone, nil)
				got, moved := answeredNames(er.ConsumedTestMocks()), integrations.CallViewMoved(held)
				if ahead {
					if len(got) != 0 || !moved {
						t.Fatalf("after test-0 ended a call reading the view in force is told %v answered, moved %v; want none, moved", got, moved)
					}
					return
				}
				if len(got) != 1 || got[0] != own.Name || moved {
					t.Fatalf("after test-0 ended the call read in test-0's view is told %v answered, moved %v; want [%s], not moved", got, moved, own.Name)
				}
			})
		}
	}
}

// markEnds ends mm's open test (installs the pool view) right after the next
// call's round is marked, once.
func markEnds(t *testing.T, mm *MockManager) *bool {
	ended := new(bool)
	mm.exact.onMarked = func() {
		mm.exact.onMarked = nil
		if mm.ExactTestServed() != "test-0" {
			t.Errorf("precondition: test-0 is not open as the call is marked: %q", mm.ExactTestServed())
		}
		mm.ServeExactPool(models.PoolKeyNone, nil)
		*ended = true
	}
	return ended
}

// Under the proven switch, an opaque TCP exchange identical to one test-0's
// own recording answered, marked in test-0's view, which the replay ends
// right after the mark, is read in test-0's view and told its answered
// recording there: it misses as one exchange more than the test recorded,
// recorded once, and its report carries no byte the call sent. Before, its
// miss read the pool view's answered recordings, none, and was reported as an
// exchange the test never recorded.
func TestAGenericExchangeMarkedInATestIsToldItsAnsweredRecordingsAfterTheTestEnds(t *testing.T) {
	mm, _ := callViewRun(t)
	ended := markEnds(t, mm)
	got, miss := genericCall(t, mm, "PING sk9LIVEsecret77")
	if !*ended || mm.ExactTestServed() != "" {
		t.Fatalf("precondition: test-0 did not end right after the call's mark (ended %v, open %q)", *ended, mm.ExactTestServed())
	}
	if got != "" {
		t.Fatalf("the second PING, one more than test-0 recorded, was answered %q", got)
	}
	if miss.MissKind != models.MissExtraCall || miss.Recorded != 1 {
		t.Fatalf("the second PING missed as %q, recorded %d; want %q, recorded 1", miss.MissKind, miss.Recorded, models.MissExtraCall)
	}
	if r := fmt.Sprintf("%+v", *miss); strings.Contains(r, "sk9LIVE") {
		t.Fatalf("the miss's report carries the bytes the call sent: %s", r)
	}
}
