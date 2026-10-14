package proxy

import (
	"context"
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
	un, _ := mm.GetUnFilteredMocks()
	equalNames(t, "session tier", un, "handshake")
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

// Exact serving belongs to one replay session; without it nothing changes.
func TestASessionResetTurnsExactServingOff(t *testing.T) {
	a := exactMock("a", 0, models.LifetimeSession)
	mm := stagedManager(t, nil, []*models.Mock{a})
	mm.ServeExactTest("T", nil)
	if sess, _ := mm.GetSessionMocks(); len(sess) != 0 {
		t.Fatalf("a test that owns nothing is served %v", exactNames(sess))
	}
	mm.ResetForReplaySession()
	if _, _, _, ok := mm.ExactTestOutcome(); ok {
		t.Fatal("exact serving outlived its session")
	}
	sess, _ := mm.GetSessionMocks()
	equalNames(t, "without exact serving", sess, "a")
	if mm.WindowSnapshot().FirstTestFired {
		t.Fatal("the next session starts with a test fired")
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
