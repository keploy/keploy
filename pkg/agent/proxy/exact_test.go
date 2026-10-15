package proxy

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
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
	mm := stagedManager(t, nil, []*models.Mock{a, cfg})
	mm.ServeExactPool("-", []string{"a"})
	mm.ServeExactTest("T", []*models.Mock{a})
	mm.ResetForReplaySession()
	if test, _, _, ok := mm.ExactTestOutcome(); !ok || test != "" {
		t.Fatalf("the previous session's test is still read: %q %v", test, ok)
	}
	sess, _ := mm.GetSessionMocks()
	equalNames(t, "after the reset", sess, "cfg")
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

// A consume that read a view before another replaced it lands on the view it
// read: it answers nothing there, so what the replaced view says it
// consumed is final.
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
	mm.exact.view.Store(v)
	verdict := mm.exactConsume(&mk, true)
	if verdict != exactRefused {
		t.Fatalf("a consume in a replaced view: %v", verdict)
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
	if got := mm.FindStagedNotOwned(models.GENERIC, func(m *models.Mock) bool { return m.Name == "other" }); got != "other" {
		t.Fatalf("found %q, want another test's staged exchange", got)
	}
	if got := mm.FindStagedNotOwned(models.GENERIC, func(m *models.Mock) bool { return m.Name == "own" }); got != "" {
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
