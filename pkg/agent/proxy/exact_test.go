package proxy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/generic"
	httpint "go.keploy.io/server/v3/pkg/agent/proxy/integrations/http"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

var exactT0 = time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)

// exactMock is an untagged mock: data-plane, whatever tier its kind derives.
func exactMock(name string, at time.Duration, lifetime models.Lifetime) *models.Mock {
	return newMockForTest(name, exactT0.Add(at), lifetime)
}

// configMock is a recorder-tagged config mock: protocol-phase.
func configMock(name string, at time.Duration) *models.Mock {
	m := exactMock(name, at, models.LifetimeSession)
	m.Spec.Metadata = map[string]string{"type": "config"}
	return m
}

func exactNames(ms []*models.Mock) []string {
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		out = append(out, m.Name)
	}
	return out
}

func equalNames(t *testing.T, what string, got []*models.Mock, want ...string) {
	t.Helper()
	g := exactNames(got)
	if len(g) != len(want) {
		t.Fatalf("%s: got %v, want %v", what, g, want)
	}
	for i := range g {
		if g[i] != want[i] {
			t.Fatalf("%s: got %v, want %v", what, g, want)
		}
	}
}

// stagedManager is a manager staged as the agent stages a replay under the
// switch: every mock of the set, once, at BaseTime.
func stagedManager(t *testing.T, perTest, session []*models.Mock) *MockManager {
	t.Helper()
	mm := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	mm.SetMocksWithWindow(perTest, session, models.BaseTime, time.Now())
	return mm
}

// While a test is open every read shows its unanswered test mocks (per-test
// reads, and the union of the reusable tiers) and the protocol-phase mocks,
// never a data-plane pool mock or another test's; and a tier-routing parser
// is told a test is open whatever the recorded times say.
func TestAnOpenTestIsServedItsOwnMocksAndTheProtocolPhase(t *testing.T) {
	a := exactMock("a", 0, models.LifetimeSession)
	b := exactMock("b", time.Second, models.LifetimePerTest)
	other := exactMock("other", 2*time.Second, models.LifetimeSession)
	pool := exactMock("pool", 3*time.Second, models.LifetimeSession)
	hs := configMock("handshake", 4*time.Second)
	mm := stagedManager(t, []*models.Mock{b}, []*models.Mock{a, other, pool, hs})
	mm.ServeExactPool("k", []string{"pool"})
	mm.ServeExactTest("T", []*models.Mock{a, b})

	pt, _ := mm.GetPerTestMocksInWindow()
	equalNames(t, "per-test", pt, "a", "b")
	for _, m := range pt {
		if m.TestModeInfo.Lifetime != models.LifetimePerTest || m.TestModeInfo.Consume != models.ConsumeReuse {
			t.Fatalf("a test mock is not served as a per-test mock consumed once: %+v", m.TestModeInfo)
		}
	}
	if a.TestModeInfo.Lifetime != models.LifetimeSession {
		t.Fatal("the caller's mock was written")
	}
	sess, _ := mm.GetSessionMocks()
	equalNames(t, "session union", sess, "a", "b", "handshake")
	// The session tier shows the open test's mocks staged there (a, whose
	// kind derives session lifetime) and the protocol-phase mocks: a parser
	// that reads that tier alone finds them.
	un, _ := mm.GetUnFilteredMocks()
	equalNames(t, "session tier", un, "a", "handshake")
	byKind, _ := mm.GetFilteredMocksByKind(models.HTTP)
	equalNames(t, "per-test by kind", byKind, "a", "b")
	if snap := mm.WindowSnapshot(); !snap.Active || !snap.FirstTestFired {
		t.Fatalf("an open test is not told: %+v", snap)
	}
	if s, e := mm.CurrentTestWindow(); !s.IsZero() || !e.IsZero() {
		t.Fatal("a recorded window narrows what an open test is served")
	}
}

// Before exact serving, a per-test begin re-staged at BaseTime filed the
// test's mocks into the startup tier (isInitialStaging keyed on the time
// alone), so a per-test read found none and a tier-routing parser (Postgres
// v3) served the test from the startup engine. Under exact serving the
// per-test reads hold the open test's mocks and the window is active.
func TestAPerTestReadFindsTheOpenTestsMocks(t *testing.T) {
	b := exactMock("b", time.Second, models.LifetimePerTest)
	mm := stagedManager(t, []*models.Mock{b}, nil)
	mm.SetMocksWithWindow([]*models.Mock{b.ShallowCopy()}, nil, models.BaseTime, time.Now())
	if pt, _ := mm.GetPerTestMocksInWindow(); len(pt) != 0 || mm.WindowSnapshot().Active {
		t.Fatalf("the pre-exact staging this test pins changed: per-test %v, active %v", exactNames(pt), mm.WindowSnapshot().Active)
	}
	mm.ServeExactTest("T", []*models.Mock{b})
	pt, _ := mm.GetPerTestMocksInWindow()
	equalNames(t, "per-test", pt, "b")
	if !mm.WindowSnapshot().Active {
		t.Fatal("the window is not active while a test is open")
	}
}

// Each test mock answers at most one request, through whichever door a
// parser consumes it, and is in no read once it has; a mock no read shows
// cannot be consumed.
func TestATestMockAnswersOnce(t *testing.T) {
	doors := map[string]func(mm *MockManager, m *models.Mock) bool{
		"DeleteFilteredMock":   func(mm *MockManager, m *models.Mock) bool { return mm.DeleteFilteredMock(*m) },
		"DeleteUnFilteredMock": func(mm *MockManager, m *models.Mock) bool { return mm.DeleteUnFilteredMock(*m) },
		"DeleteStartupMock":    func(mm *MockManager, m *models.Mock) bool { return mm.DeleteStartupMock(*m) },
		"MarkMockAsUsed":       func(mm *MockManager, m *models.Mock) bool { return mm.MarkMockAsUsed(*m) },
		"UpdateUnFilteredMock": func(mm *MockManager, m *models.Mock) bool {
			c := *m
			return mm.UpdateUnFilteredMock(m, &c)
		},
	}
	for door, consume := range doors {
		t.Run(door, func(t *testing.T) {
			a := exactMock("a", 0, models.LifetimeSession)
			other := exactMock("other", time.Second, models.LifetimeSession)
			mm := stagedManager(t, nil, []*models.Mock{a, other})
			mm.ServeExactPool("-", nil)
			mm.ServeExactTest("T", []*models.Mock{a})
			rev := mm.RevisionByKind(models.HTTP)
			pt, _ := mm.GetPerTestMocksInWindow()
			if !consume(mm, pt[0]) {
				t.Fatal("the first answer was refused")
			}
			if consume(mm, pt[0]) {
				t.Fatal("a test mock answered twice")
			}
			if consume(mm, other) {
				t.Fatal("another test's mock was consumed")
			}
			if mm.RevisionByKind(models.HTTP) == rev {
				t.Fatal("a parser keeping an index is not told the test mock is gone")
			}
			if pt, _ := mm.GetPerTestMocksInWindow(); len(pt) != 0 {
				t.Fatalf("an answered test mock is still read: %v", exactNames(pt))
			}
			test, consumed, unconsumed, ok := mm.ExactTestOutcome()
			if !ok || test != "T" || len(consumed) != 1 || consumed[0] != "a" || len(unconsumed) != 0 {
				t.Fatalf("outcome %q %v %v %v", test, consumed, unconsumed, ok)
			}
			equalNames(t, "answered", mm.ConsumedTestMocks(), "a")
			if mm.ServedCounts()["a"] != 1 {
				t.Fatalf("served %d times", mm.ServedCounts()["a"])
			}
		})
	}
}

// Two requests that read the same snapshot cannot both be answered by one
// test mock: exactly one consume of it succeeds.
func TestConcurrentConsumesOfATestMockServeOnce(t *testing.T) {
	a := exactMock("a", 0, models.LifetimeSession)
	mm := stagedManager(t, nil, []*models.Mock{a})
	mm.ServeExactTest("T", []*models.Mock{a})
	pt, _ := mm.GetPerTestMocksInWindow()
	var won atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			var ok bool
			if i%2 == 0 {
				ok = mm.DeleteFilteredMock(*pt[0])
			} else {
				ok = mm.MarkMockAsUsed(*pt[0])
			}
			if ok {
				won.Add(1)
			}
		}(i)
	}
	wg.Wait()
	if won.Load() != 1 {
		t.Fatalf("%d consumes of one test mock succeeded", won.Load())
	}
}

// A test begun again starts with every test mock unanswered.
func TestABeginAgainStartsAfresh(t *testing.T) {
	a := exactMock("a", 0, models.LifetimeSession)
	mm := stagedManager(t, nil, []*models.Mock{a})
	mm.ServeExactTest("T", []*models.Mock{a})
	pt, _ := mm.GetPerTestMocksInWindow()
	mm.DeleteFilteredMock(*pt[0])
	mm.ServeExactTest("T", []*models.Mock{a})
	pt, _ = mm.GetPerTestMocksInWindow()
	equalNames(t, "after the second begin", pt, "a")
	if !mm.DeleteFilteredMock(*pt[0]) {
		t.Fatal("the retry's test mock was refused")
	}
}

// Between tests the pool slice answers beside the protocol-phase mocks, as
// often as asked; a test's mock does not, and the window is inactive.
func TestBetweenTestsThePoolSliceAnswers(t *testing.T) {
	a := exactMock("a", 0, models.LifetimeSession)
	pool := exactMock("pool", time.Second, models.LifetimeSession)
	otherKey := exactMock("other-key", 2*time.Second, models.LifetimeSession)
	hs := configMock("handshake", 3*time.Second)
	mm := stagedManager(t, nil, []*models.Mock{a, pool, otherKey, hs})
	mm.ServeExactTest("T", []*models.Mock{a})
	mm.ServeExactPool("k", []string{"pool"})
	sess, _ := mm.GetSessionMocks()
	equalNames(t, "between tests", sess, "pool", "handshake")
	if pt, _ := mm.GetPerTestMocksInWindow(); len(pt) != 0 {
		t.Fatalf("a test mock is read between tests: %v", exactNames(pt))
	}
	for i := 0; i < 2; i++ {
		c := *pool
		if !mm.UpdateUnFilteredMock(pool, &c) {
			t.Fatal("the pool slice was refused")
		}
		pool = &c
	}
	if mm.MarkMockAsUsed(*a) || mm.MarkMockAsUsed(*otherKey) {
		t.Fatal("a mock outside the slice answered between tests")
	}
	if snap := mm.WindowSnapshot(); snap.Active || !snap.FirstTestFired {
		t.Fatalf("between tests: %+v", snap)
	}
}

// The pool slice's stateful cursors are its key's own, and none is used in
// a test.
func TestPoolCursorsAreEachKeysOwn(t *testing.T) {
	mm := stagedManager(t, nil, nil)
	mm.ServeExactPool("a", nil)
	mm.AdvanceMockCursor("GET /n", 0, 3)
	if got := mm.MockCursorIndex("GET /n", 3); got != 1 {
		t.Fatalf("key a's cursor: %d", got)
	}
	mm.ServeExactPool("b", nil)
	if got := mm.MockCursorIndex("GET /n", 3); got != 0 {
		t.Fatalf("key b reads key a's cursor: %d", got)
	}
	mm.ServeExactTest("T", nil)
	mm.AdvanceMockCursor("GET /n", 1, 3)
	if got := mm.MockCursorIndex("GET /n", 3); got != 0 {
		t.Fatalf("a cursor moved in a test: %d", got)
	}
	mm.ServeExactPool("a", nil)
	if got := mm.MockCursorIndex("GET /n", 3); got != 1 {
		t.Fatalf("key a's cursor after the test: %d", got)
	}
}

// Exact serving belongs to one replay session. A reset ends its test and
// pool: until the agent serves the next session, no data-plane mock of the
// previous one answers (an application kept alive across sessions), the
// protocol-phase mocks do, no test has fired, and no outcome is left. A
// manager that never served exactly is untouched by it.
func TestASessionResetEndsTheSessionsExactServing(t *testing.T) {
	a := exactMock("a", 0, models.LifetimeSession)
	cfg := exactMock("cfg", time.Second, models.LifetimeSession)
	cfg.Spec.Metadata = map[string]string{"type": "config"}
	w := exactMock("w", 2*time.Second, models.LifetimeSession)
	mm := stagedManager(t, nil, []*models.Mock{a, cfg, w})
	mm.ServeExactWorker([]string{"w"})
	mm.ServeExactPool("-", []string{"a"})
	mm.ServeExactTest("T", []*models.Mock{a})
	mm.ResetForReplaySession()
	if test, _, _, ok := mm.ExactTestOutcome(); !ok || test != "" {
		t.Fatalf("the previous session's test is still read: %q %v", test, ok)
	}
	// Its worker scope goes too, from this view and every one after it.
	sess, _ := mm.GetSessionMocks()
	equalNames(t, "after the reset", sess, "cfg")
	mm.ServeExactPool("-", nil)
	sess, _ = mm.GetSessionMocks()
	equalNames(t, "a view installed after the reset", sess, "cfg")
	if mm.WindowSnapshot().FirstTestFired || mm.WindowSnapshot().Active {
		t.Fatal("the next session starts with a test fired or open")
	}
	if mm.DeleteUnFilteredMock(*a) {
		t.Fatal("a mock of the previous session answered after the reset")
	}

	plain := stagedManager(t, nil, []*models.Mock{a})
	plain.ResetForReplaySession()
	if _, _, _, ok := plain.ExactTestOutcome(); ok {
		t.Fatal("a reset turned exact serving on")
	}
	sess, _ = plain.GetSessionMocks()
	equalNames(t, "without exact serving", sess, "a")
}

// A consume that read a view before another replaced it settles nothing in
// the view it read: that view is closed, and what it says it consumed is
// final. It is settled against the view in force instead (closedWait), here
// the pool's, which does not show the test's mock: refused. A view waited on
// that comes back in force (none replaces it) refuses at once.
func TestAConsumeOfAReplacedViewIsRefused(t *testing.T) {
	a := exactMock("a", 0, models.LifetimePerTest)
	mm := stagedManager(t, []*models.Mock{a}, nil)
	mm.ServeExactPool("-", nil)
	mm.ServeExactTest("T", []*models.Mock{a})
	v := mm.exactView()
	held, _ := mm.GetPerTestMocksInWindow()
	mm.ServeExactPool("-", nil)
	// The race: a consume that loaded v before the swap.
	mk := *held[0]
	verdict, wait, _ := mm.exactConsumeIn(v, &mk, true)
	if verdict != exactRefused || wait == nil {
		t.Fatalf("a consume in a replaced view: %v, wait %v; want refused, to be settled in the view in force", verdict, wait)
	}
	select {
	case <-wait:
	default:
		t.Fatal("the replaced view's replacement is in force, yet a consume of it still waits")
	}
	if verdict := mm.exactConsume(&mk, true); verdict != exactRefused {
		t.Fatalf("a consume of the ended test's mock in the pool's view: %v", verdict)
	}
	if _, consumed, _, _ := mm.ExactTestOutcome(); len(consumed) != 0 {
		t.Fatalf("the replaced view's outcome changed: %v", consumed)
	}
	mm.exact.view.Store(v)
	if verdict := mm.exactConsume(&mk, true); verdict != exactRefused {
		t.Fatalf("a consume in a closed view back in force: %v", verdict)
	}
	if _, consumed, _, _ := mm.ExactTestOutcome(); len(consumed) != 0 {
		t.Fatalf("the replaced view's outcome changed: %v", consumed)
	}
}

// A miss says where it was made, only under exact serving: in the open test,
// as one call more than recorded when its matcher says so, or outside every
// test.
func TestAMissIsFiledByWhereItWasMade(t *testing.T) {
	report := func(kind string, recorded int) *models.MockMismatchReport {
		return &models.MockMismatchReport{Protocol: "HTTP", ActualSummary: "GET /n", MissKind: kind, Recorded: recorded}
	}
	cases := []struct {
		name         string
		arm          func(mm *MockManager)
		report       *models.MockMismatchReport
		wantKind     string
		wantRecorded int
	}{
		{"off", func(*MockManager) {}, report("", 0), "", 0},
		{"in test", func(mm *MockManager) { mm.ServeExactTest("T", nil) }, report("", 0), models.MissInTest, 0},
		{"extra call", func(mm *MockManager) { mm.ServeExactTest("T", nil) }, report(models.MissExtraCall, 2), models.MissExtraCall, 2},
		{"outside", func(mm *MockManager) { mm.ServeExactPool("-", nil) }, report("", 0), models.MissOutsideTest, 0},
		// The matcher found it one more than the test recorded while the
		// test was open; it is filed once the test's end has served the pool.
		{"extra call filed as its test ended", func(mm *MockManager) {
			mm.ServeExactTest("T", nil)
			mm.ServeExactPool("-", nil)
		}, report(models.MissExtraCall, 2), models.MissExtraCall, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &Proxy{logger: zap.NewNop(), errChannel: make(chan error, 4)}
			mm := NewMockManager(nil, nil, zap.NewNop())
			defer mm.Close()
			p.setMockManager(mm)
			tc.arm(mm)
			p.BeginTestErrorCapture()
			p.sendMockNotFoundError(models.NewMockMismatchError(models.ErrNoMockMatched, tc.report))
			got, err := p.GetMockErrors(context.Background())
			if err != nil || len(got) != 1 {
				t.Fatalf("want the one miss, got %+v (%v)", got, err)
			}
			if got[0].Kind != tc.wantKind || got[0].Recorded != tc.wantRecorded {
				t.Fatalf("filed %q/%d, want %q/%d", got[0].Kind, got[0].Recorded, tc.wantKind, tc.wantRecorded)
			}
		})
	}
}

// A miss made while the replay stages a set's mocks, from the staging's
// opening (StageExactBackground) to its settling (ServeExactBackground), is
// told so first in its next steps (stagingMissNote), whatever rules the set
// was recorded under, and whether or not its parser built a report. A miss
// once the staging is settled, one in a test's view, and an answered call
// are not.
func TestAMissMadeWhileASetIsStagedSaysSo(t *testing.T) {
	provenswitch.Pin(true)
	t.Cleanup(provenswitch.Unpin)
	window := func(rules string) func(mm *MockManager) {
		return func(mm *MockManager) {
			mm.StageExactBackground(rules)
			mm.ServeExactPool(models.PoolKeyNone, nil)
		}
	}
	settled := func(rules string) func(mm *MockManager) {
		return func(mm *MockManager) {
			window(rules)(mm)
			mm.ServeExactBackground(rules, nil, nil)
			mm.ServeExactPool(models.PoolKeyNone, nil)
		}
	}
	miss := func() error {
		return models.NewMockMismatchError(models.ErrNoMockMatched, &models.MockMismatchReport{Protocol: "HTTP", ActualSummary: "GET /n", NextSteps: "check the recording."})
	}
	bare := func() error { return fmt.Errorf("no recording: %w", models.ErrNoMockMatched) }
	answered := func() error {
		return models.NewMockMismatchError(models.ErrAnswered, &models.MockMismatchReport{Protocol: "HTTP", ActualSummary: "GET /n", Answer: models.AnswerByKind})
	}
	for _, tc := range []struct {
		name string
		arm  func(mm *MockManager)
		err  func() error
		told bool
	}{
		{"a marked set's staging", window(models.RecordedWithS12Routing), miss, true},
		{"an unmarked set's staging", window(""), miss, true},
		{"a staging, a miss with no report", window(models.RecordedWithS12Routing), bare, true},
		{"a marked set settled", settled(models.RecordedWithS12Routing), miss, false},
		{"an unmarked set settled", settled(""), miss, false},
		{"a test's view during a staging", func(mm *MockManager) {
			mm.StageExactBackground(models.RecordedWithS12Routing)
			mm.ServeExactTest("T", nil)
		}, miss, false},
		{"an answered call in a staging", window(models.RecordedWithS12Routing), answered, false},
		{"no staging", func(mm *MockManager) { mm.ServeExactPool(models.PoolKeyNone, nil) }, miss, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &Proxy{logger: zap.NewNop(), errChannel: make(chan error, 4)}
			mm := NewMockManager(nil, nil, zap.NewNop())
			defer mm.Close()
			p.setMockManager(mm)
			tc.arm(mm)
			p.BeginTestErrorCapture()
			p.sendMockNotFoundError(tc.err())
			got, err := p.GetMockErrors(context.Background())
			if err != nil || len(got) != 1 {
				t.Fatalf("want the one call, got %+v (%v)", got, err)
			}
			if told := strings.HasPrefix(got[0].NextSteps, stagingMissNote); told != tc.told {
				t.Fatalf("the call's next steps %q tell the staging %v; want %v", got[0].NextSteps, told, tc.told)
			}
			if tc.told && !strings.Contains(got[0].NextSteps, "refused while the test set's mocks were being staged") {
				t.Fatalf("the staging note %q does not say it plainly", got[0].NextSteps)
			}
		})
	}
}

// A pool slice's per-test-lifetime mock (a Postgres query a second test
// process's TestMain made) is staged in the startup tree only. Between tests
// every reusable read shows it, whichever tier a parser reads: after a test
// has fired, Postgres v3 routes such a query to its session engine, which
// reads GetSessionScopedMocks.
func TestBetweenTestsEveryReusableReadShowsThePoolSlice(t *testing.T) {
	pg := exactMock("pg-pool", 0, models.LifetimePerTest)
	pg.Kind = models.PostgresV3
	pg.Spec.Metadata = map[string]string{"type": "mocks"}
	owned := exactMock("pg-owned", time.Second, models.LifetimePerTest)
	owned.Kind = models.PostgresV3
	owned.Spec.Metadata = map[string]string{"type": "mocks"}
	mm := stagedManager(t, []*models.Mock{pg, owned}, nil)
	mm.ServeExactPool("pkg/a", nil)
	mm.ServeExactTest("T1", []*models.Mock{owned})
	mm.ServeExactPool("pkg/b", []string{"pg-pool"})
	for name, read := range map[string]func() ([]*models.Mock, error){
		"GetSessionScopedMocks": mm.GetSessionScopedMocks,
		"GetUnFilteredMocks":    mm.GetUnFilteredMocks,
		"GetStartupMocks":       mm.GetStartupMocks,
		"GetSessionMocks":       mm.GetSessionMocks,
		"GetUnFilteredMocksByKind": func() ([]*models.Mock, error) {
			return mm.GetUnFilteredMocksByKind(models.PostgresV3)
		},
	} {
		got, _ := read()
		equalNames(t, name, got, "pg-pool")
	}
}

// A parser that reads one reusable tier alone (enterprise SQS reads the
// session tier) finds the open test's mocks staged there, and each answers
// once.
func TestASessionTierReaderSeesTheOpenTestsMocks(t *testing.T) {
	send := exactMock("sqs-send", 0, models.LifetimeSession)
	perTest := exactMock("pt", time.Second, models.LifetimePerTest)
	mm := stagedManager(t, []*models.Mock{perTest}, []*models.Mock{send})
	mm.ServeExactPool("k", nil)
	mm.ServeExactTest("T", []*models.Mock{send, perTest})
	ss, _ := mm.GetSessionScopedMocks()
	equalNames(t, "session tier", ss, "sqs-send")
	uk, _ := mm.GetUnFilteredMocksByKind(models.HTTP)
	equalNames(t, "session tier by kind", uk, "sqs-send")
	c := *ss[0]
	if !mm.UpdateUnFilteredMock(ss[0], &c) {
		t.Fatal("the test's own mock was refused")
	}
	if ss, _ = mm.GetSessionScopedMocks(); len(ss) != 0 {
		t.Fatalf("an answered test mock is still read: %v", exactNames(ss))
	}
}

// httpCall is a recorded GET of path, at: an HTTP recording whose method, URL
// and body are its sequence's identity outside every test.
func httpCall(name string, at time.Duration, path string) *models.Mock {
	m := exactMock(name, at, models.LifetimeSession)
	m.Spec.HTTPReq = &models.HTTPReq{Method: "GET", URL: path}
	return m
}

// pollFirst answers a call as a parser that takes the first recording in read
// order and consumes it through a delete door does (enterprise SQS), reading
// again when the consume is refused; "a miss" when no recording is read.
func pollFirst(t *testing.T, mm *MockManager, prefix string) string {
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
		if mm.DeleteUnFilteredMock(*hit) {
			return hit.Name
		}
	}
	t.Fatalf("%s: every consume was refused", prefix)
	return ""
}

// pollEarliest answers a call as a parser that ranks the recordings by
// recorded time, the earliest first, does (Mongo v2's matchTieKey).
func pollEarliest(t *testing.T, mm *MockManager, prefix string) string {
	t.Helper()
	for i := 0; i < 10; i++ {
		mocks, _ := mm.GetSessionScopedMocks()
		var hit *models.Mock
		for _, mk := range mocks {
			if strings.HasPrefix(mk.Name, prefix) && (hit == nil || mk.Spec.ReqTimestampMock.Before(hit.Spec.ReqTimestampMock)) {
				hit = mk
			}
		}
		if hit == nil {
			return "a miss"
		}
		if mm.DeleteUnFilteredMock(*hit) {
			return hit.Name
		}
	}
	t.Fatalf("%s: every consume was refused", prefix)
	return ""
}

// A data-plane recording a parser consumes through a delete door outside
// every test is consumed for the staging (founder decision 31): no begin or
// end brings it back, and once each of a call's recordings answered, the call
// misses (the manager names no last answer: a parser that knows the call's
// sequence saturates itself). Before, every begin and end brought the first
// recording back: a consumer polled after each test was answered its
// seeded messages again (enterprise sqs-localstack). A parser that picks by
// recorded time (Mongo v2's boot drain) is answered alike.
func TestOutsideEveryTestADataPlaneRecordingAnswersOncePerStaging(t *testing.T) {
	for _, tc := range []struct {
		name string
		poll func(*testing.T, *MockManager, string) string
	}{{"first in read order", pollFirst}, {"earliest recorded", pollEarliest}} {
		t.Run(tc.name, func(t *testing.T) {
			mm := stagedManager(t, nil, []*models.Mock{httpCall("r1", 0, "/receive"), httpCall("r2", time.Second, "/receive"), httpCall("r3", 2*time.Second, "/receive")})
			pool := []string{"r1", "r2", "r3"}
			mm.ServeExactPool("k", pool)
			got := []string{tc.poll(t, mm, "r"), tc.poll(t, mm, "r")}
			mm.ServeExactTest("T1", nil)
			mm.ServeExactPool("k", pool)
			got = append(got, tc.poll(t, mm, "r"), tc.poll(t, mm, "r"))
			mm.ServeExactTest("T2", nil)
			mm.ServeExactPool("k", pool)
			got = append(got, tc.poll(t, mm, "r"))
			if strings.Join(got, ",") != "r1,r2,r3,a miss,a miss" {
				t.Fatalf("answered %v; want r1, r2, r3 once each across tests, then a miss", got)
			}
		})
	}
}

// A protocol-phase recording stays reusable as before: one a parser consumes
// through a delete door outside every test is gone until the view changes,
// so the next connection in the gap claims the next recording (ZooKeeper's
// connect: two connections never share a session), and each test, and each
// gap, has them all again (an SQS config call the gap before a test made is
// answered in the test).
func TestOutsideEveryTestAProtocolPhaseRecordingStaysReusable(t *testing.T) {
	c1, c2 := configMock("c1", 0), configMock("c2", time.Second)
	t1, t2 := httpCall("t1", 2*time.Second, "/token"), httpCall("t2", 3*time.Second, "/token")
	t1.Spec.Metadata = map[string]string{"type": "config"}
	t2.Spec.Metadata = map[string]string{"type": "config"}
	mm := stagedManager(t, nil, []*models.Mock{c1, c2, t1, t2})
	mm.ServeExactPool("k", nil)
	if got := []string{pollEarliest(t, mm, "c"), pollEarliest(t, mm, "c"), pollEarliest(t, mm, "t")}; strings.Join(got, ",") != "c1,c2,t1" {
		t.Fatalf("in the gap: %v; want c1, c2 (one per connection), t1", got)
	}
	mm.ServeExactTest("T1", nil)
	if got := []string{pollEarliest(t, mm, "t"), pollEarliest(t, mm, "t"), pollEarliest(t, mm, "c")}; strings.Join(got, ",") != "t1,t2,c1" {
		t.Fatalf("in the test: %v; want t1, t2 and c1 again", got)
	}
	mm.ServeExactPool("k", nil)
	if got := pollEarliest(t, mm, "c"); got != "c1" {
		t.Fatalf("the next gap: %s; want c1 again", got)
	}
}

// A consume outside every test is accounted as used, as on every path: the
// consumed list (and so the served counts, and the noise a replay learns)
// has it.
func TestOutsideEveryTestAConsumeIsAccounted(t *testing.T) {
	mm := stagedManager(t, nil, []*models.Mock{httpCall("r1", 0, "/x")})
	mm.ServeExactPool("k", []string{"r1"})
	mm.GetConsumedMocks()
	if got := pollFirst(t, mm, "r"); got != "r1" {
		t.Fatalf("answered %s", got)
	}
	if c := mm.GetConsumedMocks(); len(c) != 1 || c[0].Name != "r1" {
		t.Fatalf("consumed %v; want r1", c)
	}
}

// The staged recordings a consume outside every test is checked against are
// read under the staging lock: a consume while a staging lands never caches
// a half-staged or previous set, which would refuse the staging's own
// recordings for the whole staging (and a parser that reads again on a
// refusal would spin).
func TestOutsideEveryTestAStagingIsNeverHalfRead(t *testing.T) {
	const n = 5000
	set := func(round int) []*models.Mock {
		out := make([]*models.Mock, 0, n)
		for i := 0; i < n; i++ {
			out = append(out, httpCall(fmt.Sprintf("m%d", i), time.Duration(round)*time.Hour+time.Duration(i)*time.Millisecond, fmt.Sprintf("/p%d", i)))
		}
		return out
	}
	mm := stagedManager(t, nil, set(0))
	for round := 1; round <= 5; round++ {
		mm.ServeExactPool("k", []string{"m1"})
		read, _ := mm.GetSessionScopedMocks()
		if len(read) != 1 {
			t.Fatalf("round %d read %v", round, exactNames(read))
		}
		poller := *read[0]
		stop := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					mm.DeleteUnFilteredMock(poller) // a consume while the staging lands
				}
			}
		}()
		mm.SetMocksWithWindow(nil, set(round), models.BaseTime, time.Now())
		close(stop)
		wg.Wait()
		mm.ServeExactPool("k", []string{"m0"})
		got, _ := mm.GetSessionScopedMocks()
		if len(got) != 1 || !mm.DeleteUnFilteredMock(*got[0]) {
			t.Fatalf("round %d: the staging refused its own recording m0", round)
		}
	}
}

// Two calls that read the same recordings before either consumed one: the
// first to consume a recording answers with it, the other is refused, reads
// again, and answers with the next. Neither is answered the same recording.
func TestOutsideEveryTestTwoCallsNeverShareARecording(t *testing.T) {
	mm := stagedManager(t, nil, []*models.Mock{httpCall("r1", 0, "/receive"), httpCall("r2", time.Second, "/receive")})
	mm.ServeExactPool("k", []string{"r1", "r2"})
	a, _ := mm.GetSessionScopedMocks()
	b, _ := mm.GetSessionScopedMocks()
	if !mm.DeleteUnFilteredMock(*a[0]) {
		t.Fatal("the first call was refused")
	}
	if mm.DeleteUnFilteredMock(*b[0]) {
		t.Fatal("the second call was answered the recording the first consumed")
	}
	// Nor through a door that does not evict, as a parser's fallback is.
	upd := *b[0]
	if mm.UpdateUnFilteredMock(b[0], &upd) || mm.MarkMockAsUsed(*b[0]) {
		t.Fatal("the second call's fallback door answered the recording the first consumed")
	}
	b, _ = mm.GetSessionScopedMocks()
	if len(b) != 1 || b[0].Name != "r2" || !mm.DeleteUnFilteredMock(*b[0]) {
		t.Fatalf("read again %v; want r2, answering", exactNames(b))
	}
}

// Calls that consume one recording at once, outside every test, each
// having read it unconsumed: exactly one is answered with it.
func TestOutsideEveryTestConcurrentConsumesOfARecordingServeOnce(t *testing.T) {
	for round := 0; round < 100; round++ {
		mm := stagedManager(t, nil, []*models.Mock{httpCall("r1", 0, "/receive")})
		mm.ServeExactPool("k", []string{"r1"})
		read, _ := mm.GetSessionScopedMocks()
		mk := *read[0]
		var wins atomic.Int32
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := 0; i < 16; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				if mm.DeleteUnFilteredMock(mk) {
					wins.Add(1)
				}
			}()
		}
		close(start)
		wg.Wait()
		if w := wins.Load(); w != 1 {
			t.Fatalf("round %d: %d calls answered with one recording", round, w)
		}
	}
}

// A key started afresh (RestartExactKey: its test process claimed it again)
// answers again what its calls outside every test consumed, and its stateful
// cursors start over; another key's consumption and cursors are its own.
func TestRestartingAKeyLeavesTheOtherKeysAlone(t *testing.T) {
	mm := stagedManager(t, nil, []*models.Mock{httpCall("a1", 0, "/a"), httpCall("b1", time.Second, "/b")})
	consume := func(key, name string) {
		t.Helper()
		mm.ServeExactPool(key, []string{name})
		if got := pollFirst(t, mm, name); got != name {
			t.Fatalf("%s answered %s", key, got)
		}
		mm.AdvanceMockCursor(name+"-seq", 0, 3)
	}
	consume("k1", "a1")
	consume("k2", "b1")
	mm.RestartExactKey("k1", []string{"a1"})
	mm.ServeExactPool("k1", []string{"a1"})
	if got := pollFirst(t, mm, "a1"); got != "a1" {
		t.Fatalf("k1 started afresh answered %s; want a1 again", got)
	}
	if idx := mm.MockCursorIndex("a1-seq", 3); idx != 0 {
		t.Fatalf("k1's cursor is at %d; want it started over", idx)
	}
	mm.ServeExactPool("k2", []string{"b1"})
	if got := pollFirst(t, mm, "b1"); got != "a miss" {
		t.Fatalf("k2 answered %s; its consumption is its own", got)
	}
	if idx := mm.MockCursorIndex("b1-seq", 3); idx != 1 {
		t.Fatalf("k2's cursor is at %d; want it where k2 left it", idx)
	}
}

// What a call outside every test consumed is the staging's: the next staging
// answers it afresh, and a recording read from the previous staging, under a
// name the next one stages too, is refused when consumed after it landed.
func TestOutsideEveryTestTheNextStagingAnswersAfresh(t *testing.T) {
	old := exactMock("m1", 0, models.LifetimeSession)
	mm := stagedManager(t, nil, []*models.Mock{old, exactMock("m2", time.Second, models.LifetimeSession)})
	mm.ServeExactPool("k", []string{"m1", "m2"})
	if got := pollFirst(t, mm, "m1"); got != "m1" {
		t.Fatalf("answered %s", got)
	}
	read, _ := mm.GetSessionScopedMocks()
	if len(read) != 1 || read[0].Name != "m2" {
		t.Fatalf("read %v; want m2 alone", exactNames(read))
	}
	stale := *read[0]
	next := exactMock("m2", time.Hour, models.LifetimeSession) // another set's m2
	mm.SetMocksWithWindow(nil, []*models.Mock{exactMock("m1", time.Hour-time.Second, models.LifetimeSession), next}, models.BaseTime, time.Now())
	mm.ServeExactPool("k", []string{"m1", "m2"})
	if mm.DeleteUnFilteredMock(stale) {
		t.Fatal("a recording read from the previous staging was consumed in the next")
	}
	if got := pollFirst(t, mm, "m"); got != "m1" {
		t.Fatalf("the next staging answered %s; want m1 afresh", got)
	}
}

// A staged data-plane mock a parser consumes through a delete door while no
// test is open is consumed for the staging (decision 31): no begin or end
// brings it back. A protocol-phase mock it evicts answers once until the
// view changes, and again in the next gap or test, as before.
func TestAnEvictedStagedMockIsTheStagingsOrTheViews(t *testing.T) {
	p := exactMock("p", 0, models.LifetimePerTest)
	p.Kind = models.MySQL
	p.Spec.Metadata = map[string]string{"type": "mocks"}
	hs := configMock("handshake", time.Second)
	mm := stagedManager(t, []*models.Mock{p}, []*models.Mock{hs})
	mm.ServeExactPool("k", []string{"p"})
	sess, _ := mm.GetSessionMocks()
	equalNames(t, "gap 1", sess, "p", "handshake")
	if !mm.DeleteFilteredMock(*sess[0]) || !mm.DeleteUnFilteredMock(*sess[1]) {
		t.Fatal("a staged mock the view shows was refused")
	}
	if mm.DeleteFilteredMock(*sess[0]) || mm.DeleteUnFilteredMock(*sess[1]) {
		t.Fatal("an evicted mock answered twice")
	}
	if sess, _ = mm.GetSessionMocks(); len(sess) != 0 {
		t.Fatalf("evicted mocks are still read: %v", exactNames(sess))
	}
	mm.ServeExactTest("T1", nil)
	sess, _ = mm.GetSessionMocks()
	equalNames(t, "in a test", sess, "handshake")
	mm.ServeExactPool("k", []string{"p"})
	sess, _ = mm.GetSessionMocks()
	equalNames(t, "gap 2: the handshake is back, the data-plane mock is the staging's", sess, "handshake")
	mm.SetMocksWithWindow([]*models.Mock{p}, []*models.Mock{hs}, models.BaseTime, time.Now())
	mm.ServeExactPool("k", []string{"p"})
	sess, _ = mm.GetSessionMocks()
	equalNames(t, "the next staging", sess, "p", "handshake")
}

// A copy a previous attempt of a test read (a call in flight across the
// test's end and its retry's begin) answers for nothing in the retry: the
// retry's own copy of that mock still answers, once.
func TestAStaleCopyFromThePreviousAttemptIsRefused(t *testing.T) {
	a := exactMock("a", 0, models.LifetimeSession)
	mm := stagedManager(t, nil, []*models.Mock{a})
	mm.ServeExactTest("T", []*models.Mock{a})
	stale, _ := mm.GetPerTestMocksInWindow()
	mm.ServeExactPool("-", nil)
	mm.ServeExactTest("T", []*models.Mock{a}) // the retry
	if mm.DeleteFilteredMock(*stale[0]) {
		_, consumed, unconsumed, _ := mm.ExactTestOutcome()
		t.Fatalf("attempt 1's copy answered for the retry: consumed=%v unconsumed=%v", consumed, unconsumed)
	}
	own, _ := mm.GetPerTestMocksInWindow()
	if len(own) != 1 || !mm.DeleteFilteredMock(*own[0]) {
		t.Fatalf("the retry's own copy did not answer: %v", exactNames(own))
	}
	// A mock a parser builds by name carries no view: it is the open test's.
	mm.ServeExactTest("T", []*models.Mock{a})
	if !mm.MarkMockAsUsed(models.Mock{Name: "a", Kind: models.HTTP}) {
		t.Fatal("a consume by name was refused")
	}
}

// The outcome is the latest test's, read after it stopped being served: a
// consume that lands after its end changes nothing, and the end's read is
// final.
func TestTheOutcomeIsTheEndedTestsAndFinal(t *testing.T) {
	a := exactMock("a", 0, models.LifetimePerTest)
	b := exactMock("b", time.Second, models.LifetimePerTest)
	mm := stagedManager(t, []*models.Mock{a, b}, nil)
	mm.ServeExactPool("-", nil)
	if test, _, _, ok := mm.ExactTestOutcome(); !ok || test != "" {
		t.Fatalf("before any test: %q %v", test, ok)
	}
	mm.ServeExactTest("T", []*models.Mock{a, b})
	held, _ := mm.GetPerTestMocksInWindow()
	if !mm.DeleteFilteredMock(*held[0]) {
		t.Fatal("a did not answer")
	}
	mm.ServeExactPool("-", nil)
	if mm.DeleteFilteredMock(*held[1]) {
		t.Fatal("a test mock answered after its test ended")
	}
	test, consumed, unconsumed, ok := mm.ExactTestOutcome()
	if !ok || test != "T" || strings.Join(consumed, ",") != "a" || strings.Join(unconsumed, ",") != "b" {
		t.Fatalf("outcome %q consumed=%v unconsumed=%v ok=%v", test, consumed, unconsumed, ok)
	}
}

// A push parser paces server push by the recorded windows (RecordedWindows,
// CurrentTestWindow). Under exact serving what the view shows is due: the
// open test's push is its own mocks, and no recorded window holds it back.
func TestUnderExactServingWhatTheViewShowsIsDue(t *testing.T) {
	defer models.RegisterCarryOver(models.Kind("ExactPush"), func(*models.Mock) bool { return false })()
	w1 := models.TestWindow{Start: exactT0, End: exactT0.Add(time.Second)}
	w2 := models.TestWindow{Start: exactT0.Add(10 * time.Second), End: exactT0.Add(11 * time.Second)}
	msg := exactMock("push", 10*time.Second+500*time.Millisecond, models.LifetimePerTest)

	mm := NewMockManager(nil, nil, zap.NewNop())
	defer mm.Close()
	mm.ResetForReplaySession()
	mm.SeedRecordedWindows([]models.TestWindow{w1, w2})
	mm.SetMocksWithWindow([]*models.Mock{msg}, nil, models.BaseTime, time.Now())
	if mm.RecordedWindows().Len() != 2 {
		t.Fatal("without exact serving the seeded windows pace push")
	}
	mm.ServeExactPool("-", nil)
	mm.ServeExactTest("test-2", []*models.Mock{msg})
	start, _ := mm.CurrentTestWindow()
	if !mm.RecordedWindows().Released(msg.Spec.ReqTimestampMock, start) {
		t.Fatal("a push the open test owns is held under exact serving")
	}
}

// A protocol-phase mock a parser evicts through a delete door is gone from
// every read until the next begin or end, the DNS and connection reads too.
func TestAnEvictedMockIsGoneFromTheStatelessAndConnectionReads(t *testing.T) {
	conn := exactMock("conn", 0, models.LifetimeConnection)
	conn.Spec.Metadata = map[string]string{"type": "connection", "connID": "c1"}
	mm := stagedManager(t, nil, []*models.Mock{conn})
	mm.ServeExactPool("-", nil)
	got, _ := mm.GetConnectionMocks("c1")
	equalNames(t, "before", got, "conn")
	if !mm.DeleteUnFilteredMock(*conn) {
		t.Fatal("the connection mock was not evicted")
	}
	got, _ = mm.GetConnectionMocks("c1")
	equalNames(t, "evicted", got)
	mm.ServeExactPool("-", nil)
	got, _ = mm.GetConnectionMocks("c1")
	equalNames(t, "after the next end", got, "conn")
}

// Under exact serving a mock is the open test's own when the test's mapping
// names it (ServeExactTest), whatever else the test holds; with no test open
// none is.
func TestOwnedByOpenTestUnderExactServingIsTheTestsMapping(t *testing.T) {
	a := exactMock("a", 0, models.LifetimePerTest)
	b := exactMock("b", time.Second, models.LifetimePerTest)
	other := exactMock("other", 2*time.Second, models.LifetimePerTest)
	mm := stagedManager(t, []*models.Mock{a, b, other}, nil)
	mm.ServeExactPool("k", nil)
	for _, m := range []*models.Mock{a, b, other} {
		if mm.OwnedByOpenTest(m) {
			t.Fatalf("with no test open %s is owned", m.Name)
		}
	}
	for _, tc := range []struct {
		owned []*models.Mock
		want  map[string]bool
	}{
		{[]*models.Mock{a, b}, map[string]bool{"a": true, "b": true, "other": false}},
		{[]*models.Mock{a}, map[string]bool{"a": true, "b": false, "other": false}},
	} {
		mm.ServeExactTest("T", tc.owned)
		for _, m := range []*models.Mock{a, b, other} {
			if got := mm.OwnedByOpenTest(m); got != tc.want[m.Name] {
				t.Errorf("test owning %d mocks: OwnedByOpenTest(%s) = %v, want %v", len(tc.owned), m.Name, got, tc.want[m.Name])
			}
		}
	}
}

// Under exact serving a test's Generic exchange stated per-test (type
// "config") answers once through the Generic parser's door, and is reported a
// test mock (MockState.DataPlane), so the replay counts it as the test's. The
// staged mocks (StagedMocks) include another test's, which no read shows.
func TestAGenericTestMockIsReportedAsATestMock(t *testing.T) {
	generic := func(name string, at time.Duration, stated string) *models.Mock {
		return &models.Mock{Name: name, Kind: models.GENERIC, Spec: models.MockSpec{
			Metadata:         map[string]string{"type": "config", models.MetadataLifetimeKey: stated},
			ReqTimestampMock: exactT0.Add(at), ResTimestampMock: exactT0.Add(at + time.Millisecond),
		}, TestModeInfo: models.TestModeInfo{Lifetime: models.LifetimeSession, LifetimeDerived: true}}
	}
	own := generic("own", 0, models.MetadataLifetimePerTest)
	other := generic("other", time.Second, models.MetadataLifetimePerTest)
	hello := generic("hello", 2*time.Second, models.MetadataLifetimeSession)
	mm := stagedManager(t, nil, []*models.Mock{own, other, hello})
	mm.ServeExactPool("-", []string{"hello"})
	mm.ServeExactTest("T", []*models.Mock{own})
	// A miss's diagnosis finds a staged mock the open test does not own, by
	// name: another test's, which no read shows; never the test's own.
	if got := stagedNotOwnedNow(mm, models.GENERIC, func(m *models.Mock) bool { return m.Name == "other" }); got != "other" {
		t.Fatalf("found %q, want another test's staged exchange", got)
	}
	if got := stagedNotOwnedNow(mm, models.GENERIC, func(m *models.Mock) bool { return m.Name == "own" }); got != "" {
		t.Fatalf("found the test's own exchange %q", got)
	}
	var served *models.Mock
	session, _ := mm.GetSessionMocks()
	for _, m := range session {
		if m.Name == "own" {
			served = m
		}
		if m.Name == "other" {
			t.Fatal("another test's exchange is read")
		}
	}
	if served == nil {
		t.Fatal("the test's exchange is not read where the Generic parser reads")
	}
	c := *served
	if !mm.UpdateUnFilteredMock(served, &c) {
		t.Fatal("the answer was refused")
	}
	var state *models.MockState
	for _, s := range mm.GetConsumedMocks() {
		if s.Name == "own" {
			state = &s
		}
	}
	if state == nil || !state.DataPlane || state.Type != "config" {
		t.Fatalf("reported %+v, want a test mock of type config", state)
	}
}

// A call in flight when a test began (recorded in the gap before its window,
// answered once it had begun) is that test's, and the replayed app may ask for
// it in the gap, as it did at record. The pool view just before the test
// answers it then (ServeExactPool's ahead), once, as the test's own: when the
// test opens it counts as answered, first, and the test is served it no
// more. Not answered in the gap, the test is served it as its own. A pool
// view without ahead shows it nowhere, as before.
func TestTheGapBeforeATestAnswersItsCallInFlight(t *testing.T) {
	fetch := exactMock("fetch", 0, models.LifetimePerTest)
	own := exactMock("own-2", time.Second, models.LifetimePerTest)
	pool := exactMock("pool", 2*time.Second, models.LifetimeSession)

	t.Run("answered in the gap", func(t *testing.T) {
		mm := stagedManager(t, []*models.Mock{fetch, own}, []*models.Mock{pool})
		mm.ServeExactPool("-", []string{"pool"}, fetch)
		pt, _ := mm.GetPerTestMocksInWindow()
		equalNames(t, "per-test in the gap", pt, "fetch")
		if !mm.DeleteFilteredMock(*pt[0]) {
			t.Fatal("the call in flight was refused in the gap")
		}
		if mm.DeleteFilteredMock(*pt[0]) {
			t.Fatal("the call in flight answered twice in the gap")
		}
		if mm.OwnedByOpenTest(pt[0]) || mm.InFlightIntoOpenTest(pt[0]) {
			t.Fatal("no test is open in the gap")
		}
		// A tier-routing parser reads only the reusable tiers with no test
		// open: they show the call in flight too, until it answers.
		mm2 := stagedManager(t, []*models.Mock{fetch, own}, []*models.Mock{pool})
		mm2.ServeExactPool("-", []string{"pool"}, fetch)
		for name, read := range map[string]func() ([]*models.Mock, error){
			"GetSessionMocks": mm2.GetSessionMocks, "GetSessionScopedMocks": mm2.GetSessionScopedMocks, "GetStartupMocks": mm2.GetStartupMocks,
		} {
			if rs, _ := read(); !strings.Contains(strings.Join(exactNames(rs), ","), "fetch") {
				t.Fatalf("%s in the gap shows %v; want the call in flight", name, exactNames(rs))
			}
		}
		// The replay drains the test that ended after the pool call: the
		// gap's answer is not in it.
		if names := consumedNames(mm.GetConsumedMocks()); len(names) != 0 {
			t.Fatalf("the ended test's drain read %v; the gap's answer is the next test's", names)
		}
		mm.ServeExactTest("test-2", []*models.Mock{fetch, own})
		if names := consumedNames(mm.GetConsumedMocks()); strings.Join(names, ",") != "fetch" {
			t.Fatalf("test-2's drain read %v; want the gap's answer, flagged as test-2 opened", names)
		}
		if !mm.InFlightIntoOpenTest(fetch) || mm.InFlightIntoOpenTest(own) {
			t.Fatal("test-2's call in flight is its own in flight, and only it")
		}
		pt, _ = mm.GetPerTestMocksInWindow()
		equalNames(t, "per-test in test-2", pt, "own-2")
		if !mm.DeleteFilteredMock(*pt[0]) {
			t.Fatal("test-2's own mock was refused")
		}
		mm.ServeExactPool("-", []string{"pool"})
		test, consumed, unconsumed, ok := mm.ExactTestOutcome()
		if !ok || test != "test-2" || strings.Join(consumed, ",") != "fetch,own-2" || len(unconsumed) != 0 {
			t.Fatalf("outcome %q consumed %v unconsumed %v", test, consumed, unconsumed)
		}
	})

	t.Run("not answered in the gap", func(t *testing.T) {
		mm := stagedManager(t, []*models.Mock{fetch, own}, []*models.Mock{pool})
		mm.ServeExactPool("-", []string{"pool"}, fetch)
		mm.ServeExactTest("test-2", []*models.Mock{fetch, own})
		pt, _ := mm.GetPerTestMocksInWindow()
		equalNames(t, "per-test in test-2", pt, "fetch", "own-2")
		if !mm.DeleteFilteredMock(*pt[0]) {
			t.Fatal("the call in flight was refused in its test")
		}
		mm.ServeExactPool("-", []string{"pool"})
		if _, consumed, unconsumed, _ := mm.ExactTestOutcome(); strings.Join(consumed, ",") != "fetch" || strings.Join(unconsumed, ",") != "own-2" {
			t.Fatalf("consumed %v unconsumed %v", consumed, unconsumed)
		}
	})

	t.Run("a pool view without ahead", func(t *testing.T) {
		mm := stagedManager(t, []*models.Mock{fetch, own}, []*models.Mock{pool})
		mm.ServeExactPool("-", []string{"pool"})
		if pt, _ := mm.GetPerTestMocksInWindow(); len(pt) != 0 {
			t.Fatalf("the gap serves %v; want none", exactNames(pt))
		}
		if mm.DeleteFilteredMock(*fetch) {
			t.Fatal("a call no view shows was answered")
		}
	})

	t.Run("a pool view named again keeps what the gap answered", func(t *testing.T) {
		mm := stagedManager(t, []*models.Mock{fetch, own}, []*models.Mock{pool})
		mm.ServeExactPool("-", []string{"pool"}, fetch)
		pt, _ := mm.GetPerTestMocksInWindow()
		if !mm.DeleteFilteredMock(*pt[0]) {
			t.Fatal("the call in flight was refused in the gap")
		}
		mm.AheadVariables().Bind(models.VariableBinding{Name: "id", Value: "live-7", Mock: "fetch"})
		mm.ServeExactPool("-", []string{"pool"}, fetch)
		if pt, _ := mm.GetPerTestMocksInWindow(); len(pt) != 0 {
			t.Fatalf("the gap, named again, serves %v; the call in flight answered once", exactNames(pt))
		}
		mm.ServeExactTest("test-2", []*models.Mock{fetch, own})
		pt, _ = mm.GetPerTestMocksInWindow()
		equalNames(t, "per-test in test-2", pt, "own-2")
		if names := consumedNames(mm.GetConsumedMocks()); strings.Join(names, ",") != "fetch" {
			t.Fatalf("test-2's drain read %v; want the gap's answer once", names)
		}
		mm.StartVariables(nil)
		if got := boundNames(mm.BoundVariables()); got != "id" {
			t.Fatalf("test-2 begins with %q bound; want what its call in flight bound before the gap was named again", got)
		}
	})

	t.Run("a gap answer for a test that did not open is no consume of the one that did", func(t *testing.T) {
		for _, next := range []string{"another pool view", "another test"} {
			mm := stagedManager(t, []*models.Mock{fetch, own}, []*models.Mock{pool})
			mm.ServeExactPool("-", []string{"pool"}, fetch)
			pt, _ := mm.GetPerTestMocksInWindow()
			if !mm.DeleteFilteredMock(*pt[0]) {
				t.Fatal("the call in flight was refused in the gap")
			}
			if next == "another pool view" {
				mm.ServeExactPool("-", []string{"pool"})
			}
			mm.ServeExactTest("test-3", []*models.Mock{own})
			if names := consumedNames(mm.GetConsumedMocks()); len(names) != 0 {
				t.Fatalf("%s: test-3's drain read %v; the gap answered test-2's call", next, names)
			}
		}
	})

	t.Run("what the gap's answers bound is the next test's", func(t *testing.T) {
		mm := stagedManager(t, []*models.Mock{fetch, own}, []*models.Mock{pool})
		mm.StartVariables(nil)
		mm.Variables().Bind(models.VariableBinding{Name: "t1", Value: "v1", Mock: "own-1"})
		mm.ServeExactPool("-", []string{"pool"}, fetch)
		// The next test's calls in flight bind in its scope, begun early
		// (AheadVariables, which the matcher picks for them: InAheadScope).
		if !mm.InAheadScope(fetch) || mm.InAheadScope(own) || mm.InAheadScope(pool) {
			t.Fatal("only the next test's call in flight binds in its scope in the gap")
		}
		mm.AheadVariables().Bind(models.VariableBinding{Name: "id", Value: "live-7", Recorded: "rec-7", Mock: "fetch"})
		mm.AheadVariables().Bind(models.VariableBinding{Name: "stray", Value: "s", Mock: "unanswered"})
		if got := boundNames(mm.BoundVariables()); got != "t1" {
			t.Fatalf("the ended test reads %s; the next test's calls bind in a scope of their own", got)
		}
		pt, _ := mm.GetPerTestMocksInWindow()
		if !mm.DeleteFilteredMock(*pt[0]) {
			t.Fatal("the call in flight was refused in the gap")
		}
		mm.ServeExactTest("test-2", []*models.Mock{fetch, own})
		mm.StartVariables([]models.VariableBinding{{Name: "g", Value: "given"}, {Name: "id", Value: "from-the-request"}})
		got := mm.BoundVariables()
		if boundNames(got) != "g,id" {
			t.Fatalf("test-2 begins with %s; want its given values and what its answered call in flight bound", boundNames(got))
		}
		for _, b := range got {
			if b.Name == "id" && (b.Value != "from-the-request" || !b.Given) {
				t.Fatalf("a given value wins over a carried one: %+v", b)
			}
		}
		// Not given, it is carried as bound.
		mm2 := stagedManager(t, []*models.Mock{fetch, own}, []*models.Mock{pool})
		mm2.ServeExactPool("-", []string{"pool"}, fetch)
		mm2.AheadVariables().Bind(models.VariableBinding{Name: "id", Value: "live-7", Recorded: "rec-7", Mock: "fetch"})
		pt, _ = mm2.GetPerTestMocksInWindow()
		mm2.DeleteFilteredMock(*pt[0])
		mm2.ServeExactTest("test-2", []*models.Mock{fetch, own})
		mm2.StartVariables(nil)
		if got := mm2.BoundVariables(); len(got) != 1 || got[0].Value != "live-7" || got[0].Given {
			t.Fatalf("test-2 begins with %+v; want id bound to live-7, as made", got)
		}
	})

	t.Run("several answered in the gap count first, in the order they answered", func(t *testing.T) {
		poll := exactMock("poll", 500*time.Millisecond, models.LifetimePerTest)
		mm := stagedManager(t, []*models.Mock{fetch, poll, own}, []*models.Mock{pool})
		mm.ServeExactPool("-", []string{"pool"}, fetch, poll)
		pt, _ := mm.GetPerTestMocksInWindow()
		byName := map[string]*models.Mock{}
		for _, mk := range pt {
			byName[mk.Name] = mk
		}
		// Answered in the other order than recorded.
		for _, name := range []string{"poll", "fetch"} {
			if !mm.DeleteFilteredMock(*byName[name]) {
				t.Fatalf("%s was refused in the gap", name)
			}
		}
		mm.ServeExactTest("test-2", []*models.Mock{fetch, poll, own})
		if names := consumedNames(mm.GetConsumedMocks()); strings.Join(names, ",") != "poll,fetch" {
			t.Fatalf("test-2's drain read %v; want the gap's answers in the order they answered", names)
		}
		pt, _ = mm.GetPerTestMocksInWindow()
		if !mm.DeleteFilteredMock(*pt[0]) {
			t.Fatal("test-2's own mock was refused")
		}
		mm.ServeExactPool("-", []string{"pool"})
		if _, consumed, _, _ := mm.ExactTestOutcome(); strings.Join(consumed, ",") != "poll,fetch,own-2" {
			t.Fatalf("test-2 consumed %v; want the gap's answers first, in their order", consumed)
		}
	})

	t.Run("the consume flagged as the test opens is the copy the gap served", func(t *testing.T) {
		mm := stagedManager(t, []*models.Mock{fetch, own}, []*models.Mock{pool})
		mm.ServeExactPool("-", []string{"pool"}, fetch)
		pt, _ := mm.GetPerTestMocksInWindow()
		served := *pt[0]
		served.Spec.ReqBodyNoise = map[string][]string{"body.at": {}}
		if !mm.DeleteFilteredMock(served) {
			t.Fatal("the call in flight was refused in the gap")
		}
		mm.ServeExactTest("test-2", []*models.Mock{fetch, own})
		states := mm.GetConsumedMocks()
		if len(states) != 1 || states[0].Name != "fetch" {
			t.Fatalf("test-2's drain read %v", consumedNames(states))
		}
		if _, ok := states[0].ReqBodyNoise["body.at"]; !ok {
			t.Fatalf("the consume carries %v; want what the served copy detected (body.at)", states[0].ReqBodyNoise)
		}
	})

	t.Run("a copy the gap served answers nothing in the test", func(t *testing.T) {
		mm := stagedManager(t, []*models.Mock{fetch, own}, []*models.Mock{pool})
		mm.ServeExactPool("-", []string{"pool"}, fetch)
		pt, _ := mm.GetPerTestMocksInWindow()
		mm.ServeExactTest("test-2", []*models.Mock{fetch, own})
		if mm.DeleteFilteredMock(*pt[0]) {
			t.Fatal("a copy read in the gap, unanswered there, answered in the test: it was served in another view")
		}
	})
}

func consumedNames(states []models.MockState) []string {
	out := make([]string, 0, len(states))
	for _, st := range states {
		out = append(out, st.Name)
	}
	return out
}

func boundNames(bs []models.VariableBinding) string {
	out := make([]string, 0, len(bs))
	for _, b := range bs {
		out = append(out, b.Name)
	}
	return strings.Join(out, ",")
}

// varMock is an HTTP recording of POST path with body {"key":"k-1"}, the
// variable key at body.key (bound there, or used there when use: bound by an
// earlier call), its answer carrying it at body.echo.
func varMock(name, path string, use bool, lifetime models.Lifetime) *models.Mock {
	return varMockOf(name, path, "k-1", use, lifetime)
}

// varMockOf is varMock with key recorded as recorded.
func varMockOf(name, path, recorded string, use bool, lifetime models.Lifetime) *models.Mock {
	v := models.MockVariable{Recorded: recorded, Answer: []string{"body.echo"}}
	if use {
		v.Use = []string{"body.key"}
	} else {
		v.Bind = "body.key"
	}
	resp := `{"echo":"` + recorded + `"}`
	m := &models.Mock{Version: "api.keploy.io/v1beta1", Name: name, Kind: models.HTTP, Spec: models.MockSpec{
		Metadata: map[string]string{"type": models.HTTPClient},
		HTTPReq: &models.HTTPReq{Method: "POST", URL: "http://deps" + path,
			Header: map[string]string{"Host": "deps", "Content-Type": "application/json"}, Body: `{"key":"` + recorded + `"}`},
		HTTPResp:         &models.HTTPResp{StatusCode: 200, Header: map[string]string{"Content-Type": "application/json", "Content-Length": strconv.Itoa(len(resp))}, Body: resp},
		Variables:        models.MockVariables{"key": v},
		ReqTimestampMock: exactT0, ResTimestampMock: exactT0.Add(time.Millisecond),
	}}
	m.TestModeInfo.Lifetime, m.TestModeInfo.LifetimeDerived = lifetime, true
	return m
}

// postKey sends POST path with body {"key":key} through the real HTTP
// integration and returns the status and body answered.
func postKey(t *testing.T, mm *MockManager, path, key string) string {
	t.Helper()
	body := `{"key":"` + key + `"}`
	resp, got, _ := serveOne(t, httpint.New(zap.NewNop()), mm, "POST "+path+" HTTP/1.1\r\nHost: deps\r\nContent-Type: application/json\r\nContent-Length: "+strconv.Itoa(len(body))+"\r\n\r\n"+body)
	if resp == nil {
		return "no answer"
	}
	return strconv.Itoa(resp.StatusCode) + " " + string(got)
}

// A call of the gap that is not the next test's in flight reads the variables
// the test that ended bound, as it does with no call named in flight: a pool
// recording that uses one is answered with its value.
func TestACallOfTheGapReadsTheEndedTestsVariables(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	for _, named := range []bool{false, true} {
		own := varMock("own-1", "/charge", false, models.LifetimePerTest)
		beat := varMock("beat", "/beat", true, models.LifetimeSession)
		fetch := exactMock("fetch", time.Second, models.LifetimePerTest)
		mm := stagedManager(t, []*models.Mock{own, fetch}, []*models.Mock{beat})
		mm.ServeExactTest("test-1", []*models.Mock{own})
		mm.StartVariables(nil)
		if got := postKey(t, mm, "/charge", "x-1"); got != `200 {"echo":"x-1"}` {
			t.Fatalf("test-1's call was answered %s", got)
		}
		var ahead []*models.Mock
		if named {
			ahead = []*models.Mock{fetch}
		}
		mm.ServeExactPool("-", []string{"beat"}, ahead...)
		if got := postKey(t, mm, "/beat", "x-1"); got != `200 {"echo":"x-1"}` {
			t.Fatalf("named %v: the pool's call in the gap was answered %s; want key read from test-1's binding", named, got)
		}
		if b, ok := mm.Variables().Lookup("key"); !ok || b.Value != "x-1" {
			t.Fatalf("named %v: the gap reads key as %+v, %v", named, b, ok)
		}
		if got := boundNames(mm.BoundVariables()); got != "key" {
			t.Fatalf("named %v: test-1's variables read %q after the pool call", named, got)
		}
	}
}

// The next test's call in flight, answered in the gap before it, binds in that
// test's scope, begun early, not the ended test's: it is answered whatever the
// ended test bound, and what it binds is the next test's, exactly as when it
// is answered inside that test. A later call of the next test that uses the
// variable is answered with the call in flight's value.
func TestTheNextTestsCallInFlightBindsInItsScopeInTheGap(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	for _, c := range []struct {
		name           string
		live, recorded string // the call in flight's key: live, and as recorded
	}{
		{"another value of the variable", "x-2", "k-2"},
		{"the same value recorded as another", "x-1", "k-2"},
		{"the same value as recorded alike", "x-1", "k-1"},
	} {
		for _, inGap := range []bool{true, false} {
			own1 := varMock("own-1", "/charge", false, models.LifetimePerTest)
			fetch := varMockOf("fetch", "/poll", c.recorded, false, models.LifetimePerTest)
			own2 := varMockOf("own-2", "/use", c.recorded, true, models.LifetimePerTest)
			mm := stagedManager(t, []*models.Mock{own1, fetch, own2}, nil)
			mm.ServeExactTest("test-1", []*models.Mock{own1})
			mm.StartVariables(nil)
			if got := postKey(t, mm, "/charge", "x-1"); got != `200 {"echo":"x-1"}` {
				t.Fatalf("%s: test-1's call was answered %s", c.name, got)
			}
			want := `200 {"echo":"` + c.live + `"}`
			if inGap {
				mm.ServeExactPool("-", nil, fetch)
				if got := postKey(t, mm, "/poll", c.live); got != want {
					t.Fatalf("%s: the call in flight was answered %s in the gap; want %s", c.name, got, want)
				}
				mm.ServeExactTest("test-2", []*models.Mock{fetch, own2})
				mm.StartVariables(nil)
			} else {
				mm.ServeExactPool("-", nil)
				mm.ServeExactTest("test-2", []*models.Mock{fetch, own2})
				mm.StartVariables(nil)
				if got := postKey(t, mm, "/poll", c.live); got != want {
					t.Fatalf("%s (control): the call in flight was answered %s in test-2; want %s", c.name, got, want)
				}
			}
			if got := boundNames(mm.BoundVariables()); got != "key" {
				t.Fatalf("%s, in the gap %v: test-2 holds %q bound; want key, the call in flight's", c.name, inGap, got)
			}
			if got := postKey(t, mm, "/use", c.live); got != want {
				t.Fatalf("%s, in the gap %v: test-2's call that uses key was answered %s; want %s", c.name, inGap, got, want)
			}
		}
	}
}

// The set's worker scope (ServeExactWorker): a worker-scope mock the run has
// reached the place of answers whether a test is open or not, after the open
// test's own, and at most one request in the staging, through whichever door
// a parser consumes it: a consume in a test spends it for the gap after and
// the next test too. One the run has not reached the place of answers
// nothing, in a test or out of one, until a later ServeExactWorker names it.
func TestTheWorkerScopeAnswersFromItsPlaceOnInAndOutOfTests(t *testing.T) {
	own := httpCall("own", 0, "/orders")
	boot := httpCall("w-boot", time.Second, "/receive")
	later := httpCall("w-later", 2*time.Second, "/receive")
	pool := httpCall("pool", 3*time.Second, "/pool")
	mm := stagedManager(t, nil, []*models.Mock{own, boot, later, pool})

	// The set's start: only the boot one is reached.
	mm.ServeExactWorker([]string{"w-boot"})
	mm.ServeExactPool("k", []string{"pool"})
	sess, _ := mm.GetSessionScopedMocks()
	equalNames(t, "before the first test", sess, "w-boot", "pool")

	// A test opens: its own mock first, then the reached worker mock; the
	// pool is not shown in a test, nor the worker mock not reached yet.
	mm.ServeExactWorker([]string{"w-boot"})
	mm.ServeExactTest("T1", []*models.Mock{own})
	if pt, _ := mm.GetPerTestMocksInWindow(); len(pt) == 0 || pt[0].Name != "own" {
		t.Fatalf("in a test the test's own mock is not read first: %v", exactNames(pt))
	}
	sess, _ = mm.GetSessionScopedMocks()
	if names := strings.Join(exactNames(sess), ","); strings.Contains(names, "pool") || strings.Contains(names, "w-later") || !strings.Contains(names, "w-boot") {
		t.Fatalf("in a test the reusable read shows %v; want w-boot, and neither the pool nor w-later", names)
	}
	if got := pollFirst(t, mm, "w-boot"); got != "w-boot" {
		t.Fatalf("the reached worker mock did not answer in a test: %s", got)
	}
	if got := pollFirst(t, mm, "w-boot"); got != "a miss" {
		t.Fatalf("a worker mock answered twice in a test: %s", got)
	}
	if got := pollFirst(t, mm, "w-later"); got != "a miss" {
		t.Fatalf("a worker mock whose place the run has not reached answered: %s", got)
	}

	// T1 ends: w-later's place (after T1) is reached. w-boot, spent in T1,
	// stays spent in the gap and the next test.
	mm.ServeExactWorker([]string{"w-boot", "w-later"})
	mm.ServeExactPool("k", []string{"pool"})
	if got := pollFirst(t, mm, "w-boot"); got != "a miss" {
		t.Fatalf("a worker mock spent in a test answered again in the gap after: %s", got)
	}
	mm.ServeExactWorker([]string{"w-boot", "w-later"})
	mm.ServeExactTest("T2", nil)
	if got := pollFirst(t, mm, "w-later"); got != "w-later" {
		t.Fatalf("a worker mock placed after T1 did not answer in T2: %s", got)
	}
	mm.ServeExactPool("k", []string{"pool"})
	if got := pollFirst(t, mm, "w-later"); got != "a miss" {
		t.Fatalf("a worker mock answered again after its one request: %s", got)
	}
	// The next staging answers afresh.
	mm.SetMocksWithWindow(nil, []*models.Mock{own, boot, later, pool}, models.BaseTime, time.Now())
	mm.ServeExactWorker([]string{"w-boot"})
	mm.ServeExactPool("k", nil)
	if got := pollFirst(t, mm, "w-boot"); got != "w-boot" {
		t.Fatalf("the next staging's worker mock did not answer: %s", got)
	}
}

// stagedNotOwnedNow is mm's lookup of a recording made elsewhere for a call
// whose first read is now (MarkCallRound).
func stagedNotOwnedNow(mm *MockManager, kind models.Kind, match func(*models.Mock) bool) string {
	name, _ := mm.FindStagedNotOwned(kind, mm.MarkCallRound(), match)
	return name
}

// stagedNotOwnedByKeysNow is stagedNotOwnedNow, by keys.
func stagedNotOwnedByKeysNow(mm *MockManager, kind models.Kind, keys []string, match func(*models.Mock) bool) string {
	name, _ := mm.FindStagedNotOwnedByKeys(kind, mm.MarkCallRound(), keys, match)
	return name
}

// A miss's diagnosis (FindStagedNotOwned) names a staged recording the open
// view does not show and no call of the set consumed: another test's, in a
// test or between tests, and a worker-scope one whose place the run has not
// reached. It never names one the view shows (the pool between tests, a
// reached worker mock), the open test's own, or one a call already consumed.
// Each recording is named for one miss at most, as it answers one call at
// most: two identical calls made elsewhere name its two recordings, in
// recorded order, and a third names none. The next staging starts afresh.
func TestFindStagedNotOwnedNamesARecordingTheViewDoesNotShow(t *testing.T) {
	own := httpCall("own", 0, "/orders")
	other := httpCall("other", time.Second, "/delete")
	inT1 := httpCall("other-in-t1", 1500*time.Millisecond, "/archive")
	consumed := httpCall("consumed", 1700*time.Millisecond, "/notify")
	boot := httpCall("w-boot", 2*time.Second, "/receive")
	later := httpCall("w-later", 3*time.Second, "/receive")
	pool := httpCall("pool", 4*time.Second, "/pool")
	pollA := httpCall("poll-a", 5*time.Second, "/jobs")
	pollB := httpCall("poll-b", 6*time.Second, "/jobs")
	all := []*models.Mock{own, other, inT1, consumed, boot, later, pool, pollA, pollB}
	mm := stagedManager(t, nil, all)
	find := func(name string) string {
		return stagedNotOwnedNow(mm, models.HTTP, func(m *models.Mock) bool { return m.Name == name })
	}
	jobs := func() string {
		return stagedNotOwnedNow(mm, models.HTTP, func(m *models.Mock) bool { return m.Spec.HTTPReq.URL == "/jobs" })
	}

	// The set's start, no test open: any test's recording is named, and the
	// worker mock not reached; the pool and the reached worker mock are shown.
	mm.ServeExactWorker([]string{"w-boot"})
	mm.ServeExactPool("k", []string{"pool"})
	for name, want := range map[string]string{"other": "other", "w-later": "w-later", "w-boot": "", "pool": ""} {
		if got := find(name); got != want {
			t.Errorf("before the first test, %s: found %q, want %q", name, got, want)
		}
	}
	if got := find("other"); got != "" {
		t.Errorf("a recording already named was named for a second miss: %q", got)
	}
	if a, b, none := jobs(), jobs(), jobs(); a != "poll-a" || b != "poll-b" || none != "" {
		t.Errorf("three identical calls named %q, %q, %q; want poll-a, poll-b, then none", a, b, none)
	}

	// A test open: never its own, still another test's.
	mm.ServeExactWorker([]string{"w-boot"})
	mm.ServeExactTest("T1", []*models.Mock{own})
	if got := find("own"); got != "" {
		t.Errorf("in T1 its own recording was named: %q", got)
	}
	if got := find("other-in-t1"); got != "other-in-t1" {
		t.Errorf("in T1 another test's recording was not named: %q", got)
	}

	// Its owner is not a test that has begun until it begins.
	if mm.StagedOwnerBegun("consumed") || !mm.StagedOwnerBegun("own") {
		t.Errorf("owners begun: consumed %v, own %v; want only own's test begun", mm.StagedOwnerBegun("consumed"), mm.StagedOwnerBegun("own"))
	}

	// Another test consumes its recording: no later miss names it.
	mm.ServeExactWorker([]string{"w-boot"})
	mm.ServeExactTest("T2", []*models.Mock{consumed})
	if !mm.StagedOwnerBegun("consumed") {
		t.Error("consumed's test began, and its owner is not read as begun")
	}
	if !mm.DeleteFilteredMock(*consumed) {
		t.Fatal("T2 could not consume its own recording")
	}
	mm.ServeExactWorker([]string{"w-boot"})
	mm.ServeExactPool("k", []string{"pool"})
	if got := find("consumed"); got != "" {
		t.Errorf("a recording its test consumed was named after: %q", got)
	}
	// Nor one the worker scope spent.
	if got := pollFirst(t, mm, "w-boot"); got != "w-boot" {
		t.Fatalf("the reached worker mock did not answer: %s", got)
	}
	if got := find("w-boot"); got != "" {
		t.Errorf("a spent worker mock was named: %q", got)
	}
	// The next staging begins afresh: no test of it has begun, and a
	// recording named in the previous one is named again.
	mm.SetMocksWithWindow(nil, all, models.BaseTime, time.Now())
	mm.ServeExactPool("k", nil)
	if mm.StagedOwnerBegun("own") || mm.StagedOwnerBegun("consumed") {
		t.Error("a test begun in the previous staging is read as begun in the next")
	}
	if got := find("other"); got != "other" {
		t.Errorf("the next staging did not name a recording the previous one named: %q", got)
	}
}

// A recording of a test not begun yet, which the agent keeps on disk and the
// trees do not hold, is named from the recording source by its request key
// (FindStagedNotOwnedByKeys), read on each such miss and kept by nobody. The
// source's mocks take the same filters as the trees': never the open test's
// own, each named once; and one passed over by name (named already, the open
// test's) is never decoded. Without keys only the trees are looked in, and a
// session reset drops the source.
func TestFindStagedNotOwnedByKeysReadsTheRecordingSource(t *testing.T) {
	pool := httpCall("pool", 0, "/pool")
	onDisk := httpCall("t2-delete", time.Second, "/delete")
	own := httpCall("t1-own", 2*time.Second, "/delete")
	mm := stagedManager(t, nil, []*models.Mock{pool})
	var asked, decoded []string
	mm.SetRecordingSource(func(kind models.Kind, keys []string, visit func(string, func() (*models.Mock, error)) bool) error {
		asked = append(asked, string(kind)+" "+strings.Join(keys, ","))
		if kind != models.HTTP || len(keys) != 1 || keys[0] != "DELETE /delete" {
			return nil
		}
		for _, m := range []*models.Mock{onDisk, own} {
			m := m
			if !visit(m.Name, func() (*models.Mock, error) { decoded = append(decoded, m.Name); return m, nil }) {
				break
			}
		}
		return nil
	})
	find := func(key, name string) string {
		var keys []string
		if key != "" {
			keys = []string{key}
		}
		return stagedNotOwnedByKeysNow(mm, models.HTTP, keys, func(m *models.Mock) bool { return m.Name == name })
	}
	mm.ServeExactPool("k", []string{"pool"})
	if got := find("", "t2-delete"); got != "" || len(asked) != 0 {
		t.Fatalf("with no key the source was read (%v), naming %q", asked, got)
	}
	if got := find("DELETE /delete", "t2-delete"); got != "t2-delete" {
		t.Fatalf("a recording kept on disk was not named before its test: %q", got)
	}
	if got := find("DELETE /delete", "t2-delete"); got != "" {
		t.Fatalf("a recording kept on disk was named twice: %q", got)
	}
	mm.ServeExactTest("T1", []*models.Mock{own})
	if got := find("DELETE /delete", "t1-own"); got != "" {
		t.Fatalf("the open test's own recording was named from the source: %q", got)
	}
	if len(asked) != 3 {
		t.Fatalf("the source was read %v; want once per keyed miss", asked)
	}
	// t2-delete was decoded once, as the first miss named it: the second
	// miss passed it over by name (and decoded t1-own, which is not that
	// call), and the third passed over both, named and the open test's own.
	if want := []string{"t2-delete", "t1-own"}; !reflect.DeepEqual(decoded, want) {
		t.Fatalf("decoded %v; want %v: a recording named already, or the open test's own, is passed over by name", decoded, want)
	}
	mm.ResetForReplaySession()
	mm.ServeExactPool("k", nil)
	if got := find("DELETE /delete", "t2-delete"); got != "" || len(asked) != 3 {
		t.Fatalf("after a session reset the source was still read (%v), naming %q", asked, got)
	}
}

// A pool view's ahead (the next test's call in flight, ServeExactPool) is the
// view's own: answered in the gap as that test's, it is flagged consumed only
// once that test opens. An identical second call in the gap is an extra call,
// and never names it as a recording made elsewhere, answered or not.
func TestFindStagedNotOwnedSkipsTheNextTestsCallsInFlight(t *testing.T) {
	ahead := httpCall("t2-fetch", time.Second, "/fetch")
	mm := stagedManager(t, nil, []*models.Mock{ahead})
	mm.ServeExactPool("k", nil, ahead)
	named := func() string {
		return stagedNotOwnedNow(mm, models.HTTP, func(m *models.Mock) bool { return m.Name == "t2-fetch" })
	}
	if got := named(); got != "" {
		t.Fatalf("before it answered, the next test's call in flight was named as made elsewhere: %q", got)
	}
	if got := pollFirst(t, mm, "t2-fetch"); got != "t2-fetch" {
		t.Fatalf("the call in flight did not answer in the gap: %s", got)
	}
	if got := named(); got != "" {
		t.Fatalf("an identical second call in the gap named the next test's call in flight %q, which answered here already", got)
	}
}

// The recordings before an instant are the trees' and the agent's on-disk
// ones, by request time, each once (a tree's copy of an on-disk recording is
// visited from the tree), until the visitor stops; a reader that fails says
// so, and with none the trees' alone are visited. The worker's scoped view
// forwards them, and is an error over a store that cannot read them; a new
// replay session forgets the last set's reader.
func TestRangeRecordedBeforeVisitsTheTreesAndTheDisk(t *testing.T) {
	mm := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	pool := exactMock("pool-1", time.Second, models.LifetimeSession)
	own := exactMock("own-2", 2*time.Second, models.LifetimePerTest)
	later := exactMock("pool-9", 9*time.Second, models.LifetimeSession)
	mm.SetMocksWithWindow([]*models.Mock{own}, []*models.Mock{pool, later}, models.BaseTime, time.Now())
	at := exactT0.Add(5 * time.Second)
	visit := func(db integrations.RecordedBeforeReader) (string, []*models.Mock, error) {
		var names []string
		var got []*models.Mock
		err := db.RangeRecordedBefore(at, func(m *models.Mock) bool {
			names = append(names, m.Name)
			got = append(got, m)
			return true
		})
		sort.Strings(names)
		return strings.Join(names, ","), got, err
	}
	if names, _, err := visit(mm); err != nil || names != "own-2,pool-1" {
		t.Fatalf("with no disk: %s %v; want own-2,pool-1", names, err)
	}
	onDisk := exactMock("other-test-3", 3*time.Second, models.LifetimePerTest)
	ownCopy := exactMock("own-2", 2*time.Second, models.LifetimePerTest)
	mm.SetRecordedBefore(func(t2 time.Time, v func(*models.Mock) bool) error {
		if !t2.Equal(at) {
			t.Fatalf("the disk read before %v", t2)
		}
		for _, m := range []*models.Mock{ownCopy, onDisk} {
			if !v(m) {
				return nil
			}
		}
		return nil
	})
	names, got, err := visit(mm)
	treeCopy := false
	for _, m := range got {
		treeCopy = treeCopy || m == own
	}
	if err != nil || names != "other-test-3,own-2,pool-1" || !treeCopy {
		t.Fatalf("with the disk: %s %v (own-2 the tree's: %v); want other-test-3, own-2 (the tree's), pool-1", names, err, treeCopy)
	}
	if names, _, err := visit(&scopedMockDb{MockMemDb: mm}); err != nil || names != "other-test-3,own-2,pool-1" {
		t.Fatalf("through the scoped view: %s %v", names, err)
	}
	type noReader struct{ integrations.MockMemDb }
	if names, _, err := visit(&scopedMockDb{MockMemDb: noReader{mm}}); err == nil {
		t.Fatalf("a scoped view over a store that cannot read its recordings visited %q as all of them", names)
	}
	stops := 0
	_ = mm.RangeRecordedBefore(at, func(*models.Mock) bool { stops++; return false })
	if stops != 1 {
		t.Fatalf("a visitor that stops was called %d times", stops)
	}
	mm.SetRecordedBefore(func(time.Time, func(*models.Mock) bool) error { return fmt.Errorf("closed") })
	if _, _, err := visit(mm); err == nil {
		t.Fatal("a failing disk read was not told")
	}
	mm.ResetForReplaySession()
	if names, _, err := visit(mm); err != nil || strings.Contains(names, "other-test-3") {
		t.Fatalf("after a new replay session: %s %v; want the last set's reader forgotten", names, err)
	}
}

// The pool names the agent hands over (the replay's scope table's pool
// slices) say which mocks are the pool's, through a worker's view too; none is
// known before a replay sent them, and an empty table says no mock is.
func TestThePoolNamesSayWhatIsThePools(t *testing.T) {
	mm := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	a, b := &models.Mock{Name: "boot-0"}, &models.Mock{Name: "test-0"}
	if _, known := mm.InPool(a); known {
		t.Fatal("the pool is known before any replay sent its table")
	}
	mm.SetPoolNames([]string{"boot-0", ""})
	for _, db := range []integrations.MockMemDb{mm, &scopedMockDb{MockMemDb: mm}} {
		if pool, known := integrations.InPool(db, a); !pool || !known {
			t.Fatalf("%T: boot-0 pool=%v known=%v; want the pool's", db, pool, known)
		}
		if pool, known := integrations.InPool(db, b); pool || !known {
			t.Fatalf("%T: test-0 pool=%v known=%v; want known, not the pool's", db, pool, known)
		}
	}
	mm.SetPoolNames(nil)
	if pool, known := mm.InPool(a); pool || !known {
		t.Fatalf("an empty table: boot-0 pool=%v known=%v; want known, not the pool's", pool, known)
	}
	mm.SetPoolNames([]string{"boot-0"})
	mm.ResetForReplaySession()
	if _, known := mm.InPool(a); known {
		t.Fatal("a new replay session reads the last session's pool names")
	}
}

// A lookup of a recording made elsewhere is asked of the round the call was
// made in (integrations.CallRound, marked as its first read): asked of no
// read it names nothing; a recording the call's first read's view held as
// its own is never named once the replay ended that test under the call,
// nor spent; and none is named once the replay staged another set, whether
// it staged before the lookup or right before a name is spent, and the
// next set's recording keeps its one miss for a call of its own.
func TestALookupOfARecordingMadeElsewhereIsAskedOfTheCallsRound(t *testing.T) {
	own := httpCall("own", 0, "/orders")
	other := httpCall("other", time.Second, "/delete")
	mm := stagedManager(t, nil, []*models.Mock{own, other})
	find := func(call integrations.CallRound, name string) (string, bool) {
		return mm.FindStagedNotOwned(models.HTTP, call, func(m *models.Mock) bool { return m.Name == name })
	}
	mm.ServeExactTest("T1", []*models.Mock{own})
	if got, moved := find(integrations.CallRound{}, "other"); got != "" || moved {
		t.Fatalf("asked of no read: named %q (restaged %v)", got, moved)
	}

	// A call T1's view read, decided after T1 ended under it.
	call := mm.MarkCallRound()
	mm.ServeExactTest("T2", []*models.Mock{other})
	if got, moved := find(call, "own"); got != "" || moved {
		t.Fatalf("a call made in T1 named T1's own once T1 ended under it: %q (restaged %v)", got, moved)
	}
	if got, _ := find(mm.MarkCallRound(), "own"); got != "own" {
		t.Fatalf("a call made in T2 did not name T1's own (%q): the lookup that refused it spent it", got)
	}

	// The next set staged after the call's first read: none of its
	// recordings is even offered to the parser's comparison.
	call = mm.MarkCallRound()
	next := func(name string) *models.Mock { return httpCall(name, 2*time.Second, "/next") }
	mm.SetMocksWithWindow(nil, []*models.Mock{next("next")}, models.BaseTime, time.Now())
	mm.ServeExactTest("S2-T1", nil)
	offered := 0
	got, moved := mm.FindStagedNotOwned(models.HTTP, call, func(*models.Mock) bool { offered++; return true })
	if got != "" || !moved || offered != 0 {
		t.Fatalf("a call of the set before named %q of the next set (restaged %v, %d offered), want none, restaged, none offered", got, moved, offered)
	}
	if got, _ := find(mm.MarkCallRound(), "next"); got != "next" {
		t.Fatalf("the next set's recording lost its one miss to a call of the set before: %q", got)
	}

	// The next set staged right before the lookup spends a name, and a
	// call of that set named its recording at once: the lookup of the set
	// before spends nothing, in neither set, so the recording is named
	// for that one call only.
	mm.SetMocksWithWindow(nil, []*models.Mock{next("again")}, models.BaseTime, time.Now())
	mm.ServeExactTest("S3-T1", nil)
	call = mm.MarkCallRound()
	staged, inForce := false, ""
	mm.exact.onNaming = func() {
		if !staged {
			staged = true
			mm.SetMocksWithWindow(nil, []*models.Mock{next("again")}, models.BaseTime, time.Now())
			mm.ServeExactTest("S4-T1", nil)
			inForce, _ = find(mm.MarkCallRound(), "again")
		}
	}
	got, moved = find(call, "again")
	mm.exact.onNaming = nil
	if !staged || got != "" || !moved {
		t.Fatalf("staged right before the name was spent (%v): named %q (restaged %v), want none, restaged", staged, got, moved)
	}
	if inForce != "again" {
		t.Fatalf("the set staged in force's own call did not name its recording: %q", inForce)
	}
	if got, _ := find(mm.MarkCallRound(), "again"); got != "" {
		t.Fatalf("the set in force's recording was named for a second call (%q): the lookup of the set before emptied what that set spent", got)
	}

	// The next set staged right after the lookup spent a name in the
	// call's staging: the name is not told, and the lookup says the set
	// moved.
	mm.SetMocksWithWindow(nil, []*models.Mock{next("late")}, models.BaseTime, time.Now())
	mm.ServeExactTest("S5-T1", nil)
	call = mm.MarkCallRound()
	mm.exact.onNamed = func() {
		mm.exact.onNamed = nil
		mm.SetMocksWithWindow(nil, []*models.Mock{next("late")}, models.BaseTime, time.Now())
		mm.ServeExactTest("S6-T1", nil)
	}
	if got, moved := find(call, "late"); got != "" || !moved {
		t.Fatalf("staged right after a name was spent: named %q (restaged %v), want none, restaged", got, moved)
	}
	if got, _ := find(mm.MarkCallRound(), "late"); got != "late" {
		t.Fatalf("the set in force's recording lost its one miss to a call of the set before: %q", got)
	}
}

// genericExchange is a Generic exchange the recorder stated per-test,
// answering OK.
func genericExchange(name string, at time.Duration, req string) *models.Mock {
	m := exactMock(name, at, models.LifetimeSession)
	m.Kind = models.GENERIC
	m.Spec.Metadata = map[string]string{"type": "config", models.MetadataLifetimeKey: models.MetadataLifetimePerTest}
	m.Spec.GenericRequests = []models.Payload{{Message: []models.OutputBinary{{Type: models.String, Data: req}}}}
	m.Spec.GenericResponses = []models.Payload{{Message: []models.OutputBinary{{Type: models.String, Data: "OK"}}}}
	return m
}

// genericMiss is the miss report the Generic parser files for req, sent on
// a connection served from mm. A request the parser answers fails the test.
func genericMiss(t *testing.T, mm *MockManager, req string) *models.MockMismatchReport {
	t.Helper()
	r := genericServe(t, mm, req)
	if r == nil {
		t.Fatalf("%q: no miss with a report: the parser answered it", req)
	}
	return r
}

// genericServe is the miss report the Generic parser files for req, sent on
// a connection served from mm, or nil when the parser answered it: the
// application reads the answer and closes the connection, which ends the
// parser with no miss.
func genericServe(t *testing.T, mm *MockManager, req string) *models.MockMismatchReport {
	t.Helper()
	answered := make(chan bool, 1)
	app, proxySide := net.Pipe()
	t.Cleanup(func() { app.Close(); proxySide.Close() })
	go func() {
		if _, err := app.Write([]byte(req)); err != nil {
			return
		}
		n, _ := app.Read(make([]byte, 64))
		answered <- n > 0
		if n > 0 {
			app.Close()
		}
	}()
	ctx := context.WithValue(context.Background(), models.ClientConnectionIDKey, "1")
	ctx = context.WithValue(ctx, models.DestConnectionIDKey, "2")
	err := generic.New(zap.NewNop()).MockOutgoing(ctx, proxySide, nil, mm, models.OutgoingOptions{})
	var r interface {
		MismatchReport() *models.MockMismatchReport
	}
	if errors.As(err, &r) && r.MismatchReport() != nil {
		return r.MismatchReport()
	}
	// No miss: the parser answered the request, or failed it with no report.
	proxySide.Close()
	if !<-answered {
		t.Fatalf("%q: neither answered nor a miss with a report: %v", req, err)
	}
	return nil
}

// The Generic parser asks its lookup of an exchange made elsewhere of the
// round the request's first read found it made in (MarkCallRound, before
// its matcher reads the store): another test's exchange is named; one the
// call's test owned when the call was read is not once the replay ended
// that test under the call; and none is once the replay staged the next set
// under it.
func TestAGenericMissIsAskedOfTheRoundItsRequestWasReadIn(t *testing.T) {
	mine := genericExchange("ex-mine", 0, "AUTH step2")
	theirs := genericExchange("ex-theirs", time.Second, "AUTH step3")
	mm := stagedManager(t, nil, []*models.Mock{mine, theirs})
	mm.ServeExactTest("T1", []*models.Mock{mine})
	if r := genericMiss(t, mm, "AUTH step3"); !r.NotOwned || r.ClosestMock != "ex-theirs" {
		t.Fatalf("another test's exchange was not named: %+v", r)
	}

	// T1 ends between the request's first read and the matcher's.
	mm.exact.onMarked = func() {
		mm.exact.onMarked = nil
		mm.ServeExactTest("T2", nil)
	}
	if r := genericMiss(t, mm, "AUTH step2"); r.NotOwned {
		t.Fatalf("the exchange the call's test owned when its request was read was named as made elsewhere: %+v", r)
	}

	// The next set is staged between the request's first read and the
	// matcher's.
	mm.ServeExactTest("T3", nil)
	mm.exact.onMarked = func() {
		mm.exact.onMarked = nil
		mm.SetMocksWithWindow(nil, []*models.Mock{genericExchange("s2-ex", 2*time.Second, "AUTH step4")}, models.BaseTime, time.Now())
		mm.ServeExactTest("S2-T1", nil)
	}
	if r := genericMiss(t, mm, "AUTH step4"); r.NotOwned {
		t.Fatalf("a request of the set before was named as the next set's exchange: %+v", r)
	}
	if r := genericMiss(t, mm, "AUTH step4"); !r.NotOwned || r.ClosestMock != "s2-ex" {
		t.Fatalf("the next set's exchange lost its one miss to a request of the set before: %+v", r)
	}
}

// A Generic request held after its first read of the round while the
// replay stages the next set, whose open test owns an exchange that is the
// request byte for byte, is never answered with that exchange, nor
// diagnosed against it: the matcher and the miss read the store held to the
// request's first read (integrations.HoldToRound), which finds nothing once
// the staging moved. The request misses, saying the set was staged again
// under it, and the exchange, never spent, answers the next set's own
// request.
func TestAGenericRequestHeldWhileTheNextSetIsStagedIsNeverAnsweredFromIt(t *testing.T) {
	mine := genericExchange("ex-mine", 0, "AUTH step2")
	next := genericExchange("s2-ex", 2*time.Second, "AUTH step4")
	mm := stagedManager(t, nil, []*models.Mock{mine})
	mm.ServeExactTest("T1", []*models.Mock{mine})
	staged := false
	mm.exact.onMarked = func() {
		mm.exact.onMarked = nil
		mm.SetMocksWithWindow(nil, []*models.Mock{next}, models.BaseTime, time.Now())
		mm.ServeExactTest("S2-T1", []*models.Mock{next})
		staged = true
	}
	r := genericServe(t, mm, "AUTH step4")
	if !staged {
		t.Fatal("precondition: the replay never staged the next set under the request")
	}
	if r == nil {
		t.Fatalf("a request of the set before was answered with the next set's %s", next.Name)
	}
	if r.NotOwned || r.ClosestMock == next.Name {
		t.Fatalf("a request of the set before was diagnosed against the next set's %s: %+v", next.Name, r)
	}
	if !strings.Contains(r.NextSteps, "staged a test set again while it decided this request, so keploy answered it with no recorded exchange") {
		t.Fatalf("the miss does not say the set was staged again under the request: %q", r.NextSteps)
	}

	// Never spent on the request before, it answers the next set's own.
	if r := genericServe(t, mm, "AUTH step4"); r != nil {
		t.Fatalf("the next set's own request missed after a request of the set before: %+v", r)
	}
}

// BenchmarkACallsRoundIsMarked is what each request's first read of the
// round costs replay under the switch (MarkCallRound), by the set's size:
// the staging and the view in force, read in O(1), whatever the set holds.
func BenchmarkACallsRoundIsMarked(b *testing.B) {
	for _, n := range []int{100, 1000, 5000} {
		for _, par := range []bool{false, true} {
			b.Run(strconv.Itoa(n)+"/parallel="+strconv.FormatBool(par), func(b *testing.B) {
				mm := NewMockManager(nil, nil, zap.NewNop())
				b.Cleanup(mm.Close)
				ms := make([]*models.Mock, n)
				for i := range ms {
					ms[i] = httpCall(fmt.Sprintf("mock-%d", i), time.Duration(i)*time.Millisecond, "/x/"+strconv.Itoa(i))
				}
				mm.SetMocksWithWindow(nil, ms, models.BaseTime, time.Now())
				mm.ServeExactTest("test-1", ms[:1])
				if !integrations.MarkCallRound(mm).Read() {
					b.Fatal("no round marked")
				}
				b.ReportAllocs()
				b.ResetTimer()
				if par {
					b.RunParallel(func(pb *testing.PB) {
						for pb.Next() {
							integrations.MarkCallRound(mm)
						}
					})
					return
				}
				for i := 0; i < b.N; i++ {
					integrations.MarkCallRound(mm)
				}
			})
		}
	}
}
