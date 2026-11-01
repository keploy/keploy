package proxy

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
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
	"go.keploy.io/server/v3/pkg/matcher/learn"
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
	return stagedManagerLogged(t, zap.NewNop(), perTest, session)
}

// stagedManagerLogged is stagedManager, logging to lg.
func stagedManagerLogged(t *testing.T, lg *zap.Logger, perTest, session []*models.Mock) *MockManager {
	t.Helper()
	mm := NewMockManager(nil, nil, lg)
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

// exactMongo is an untagged Mongo recording of lifetime lt.
func exactMongo(name string, at time.Duration, lt models.Lifetime) *models.Mock {
	m := exactMock(name, at, lt)
	m.Kind = models.Mongo
	return m
}

// perTestReads checks every per-test read of mm (GetFilteredMocks, by kind,
// in window, and the round snapshot's) shows want, of kind Mongo, and that
// another kind's read shows nothing.
func perTestReads(t *testing.T, mm *MockManager, what string, want ...string) {
	t.Helper()
	pt, _ := mm.GetFilteredMocks()
	equalNames(t, what+": GetFilteredMocks", pt, want...)
	byKind, _ := mm.GetFilteredMocksByKind(models.Mongo)
	equalNames(t, what+": GetFilteredMocksByKind", byKind, want...)
	inWindow, _ := mm.GetFilteredMocksInWindow()
	equalNames(t, what+": GetFilteredMocksInWindow", inWindow, want...)
	other, _ := mm.GetFilteredMocksByKind(models.HTTP)
	equalNames(t, what+": another kind's read", other)
	snap, ok := mm.RoundSnapshot(integrations.RoundQuery{Kind: models.Mongo})
	if !ok {
		t.Fatalf("%s: no round snapshot under exact serving", what)
	}
	equalNames(t, what+": the round snapshot's per-test read", snap.PerTest, want...)
}

// exactWindows seeds mm's recorded windows of the set (the staging call's
// RecordedWindows): T over [0.5s, 1.5s] and T2 over [1.7s, 2.5s] of exactT0.
func exactWindows(mm *MockManager) {
	mm.SeedRecordedWindows([]models.TestWindow{
		{TestCase: "T", Start: exactT0.Add(500 * time.Millisecond), End: exactT0.Add(1500 * time.Millisecond)},
		{TestCase: "T2", Start: exactT0.Add(1700 * time.Millisecond), End: exactT0.Add(2500 * time.Millisecond)},
	})
}

// A per-test recording the pool slice holds that was sent after the set's
// last recorded test (a Go mongo-driver's endSessions at client.Disconnect)
// is filed by the staging in the startup tree alone. Once the first test
// fired a parser reads only the per-test tier for it (Mongo's index), so
// every per-test read shows it while no test is open after a test, and the
// round snapshot shows the same. Before the first test the startup read is
// the parsers' own and nothing is added, and in a test nothing changes. It
// stays fail-closed: the slice's per-test recordings sent before the first
// test, in the gap between two tests, or in the last test's window (answered
// after it) are never shown in a per-test read
// (a late call paired with one, made before another test's own recording,
// would learn what that test's answer gave the app), nor is a teardown
// recording outside the slice, and once the teardown recording answered
// through Mongo's delete door, a second call finds nothing through any door.
func TestAfterTheLastTestTheSetsTeardownRecordingAnswersAPerTestRead(t *testing.T) {
	boot := exactMongo("boot", 200*time.Millisecond, models.LifetimePerTest)
	owned := exactMongo("owned", time.Second, models.LifetimePerTest)
	gap := exactMongo("gap", 1600*time.Millisecond, models.LifetimePerTest)
	owned2 := exactMongo("owned-2", 2*time.Second, models.LifetimePerTest)
	// Sent in T2's window and answered after it: in flight as the last
	// test ended, not sent after it.
	straddle := exactMongo("straddle", 2400*time.Millisecond, models.LifetimePerTest)
	straddle.Spec.ResTimestampMock = exactT0.Add(2600 * time.Millisecond)
	end := exactMongo("end-sessions", 3*time.Second, models.LifetimePerTest)
	unlisted := exactMongo("unlisted", 4*time.Second, models.LifetimePerTest)
	// A session-tier recording of the slice is a reusable read's (the
	// staging files it in the session tree too), never a per-test read's.
	session := exactMongo("session-tier", 5*time.Second, models.LifetimeSession)
	mm := stagedManager(t, []*models.Mock{boot, owned, gap, owned2, straddle, end, unlisted}, []*models.Mock{session})
	exactWindows(mm)
	slice := []string{"boot", "gap", "straddle", "end-sessions", "session-tier"}
	mm.ServeExactPool("k", slice)
	perTestReads(t, mm, "before the first test")
	mm.ServeExactTest("T", []*models.Mock{owned})
	perTestReads(t, mm, "in the first test", "owned")
	if !mm.DeleteFilteredMock(*owned) {
		t.Fatal("precondition: the test's own recording did not answer it")
	}
	mm.ServeExactPool("k", slice)
	perTestReads(t, mm, "between two tests", "end-sessions")
	mm.ServeExactTest("T2", []*models.Mock{owned2})
	perTestReads(t, mm, "in the second test", "owned-2")
	if !mm.DeleteFilteredMock(*owned2) {
		t.Fatal("precondition: the second test's own recording did not answer it")
	}
	mm.ServeExactPool("k", slice)
	perTestReads(t, mm, "after the last test", "end-sessions")
	if sess, _ := mm.GetSessionMocks(); !hasName(sess, "session-tier") {
		t.Fatalf("after the last test the reusable read lost the slice's session-tier recording: %v", exactNames(sess))
	}
	if !mm.DeleteFilteredMock(*end) {
		t.Fatal("the pool's recording of the call after the last test did not answer it")
	}
	perTestReads(t, mm, "after it answered")
	c := *end
	if mm.DeleteFilteredMock(*end) || mm.DeleteStartupMock(*end) || mm.UpdateUnFilteredMock(end, &c) || mm.MarkMockAsUsed(*end) {
		t.Fatal("the call after the last test, made once more, was answered by the recording that already answered it")
	}
	if mm.DeleteFilteredMock(*unlisted) {
		t.Fatal("a call after the last test was answered by a recording the pool slice does not hold")
	}
}

// keploy mock replay sends no recorded windows but stages the whole
// recording and names the pool's recordings (SetPoolNames): there the set's
// teardown is what was sent after every recording a test owns, consumed or
// not, so after the tests a per-test read shows the pool's recording made
// after the last test's, never one made before the first test or between two
// tests. With neither windows nor pool names, or with pool names but no
// recording a test owns, nothing tells the teardown, and a per-test read
// after the test shows none.
func TestWithNoRecordedWindowsTheTeardownIsAfterEveryRecordingATestOwns(t *testing.T) {
	boot := exactMongo("boot", 200*time.Millisecond, models.LifetimePerTest)
	owned := exactMongo("owned", time.Second, models.LifetimePerTest)
	gap := exactMongo("gap", 1600*time.Millisecond, models.LifetimePerTest)
	owned2 := exactMongo("owned-2", 2*time.Second, models.LifetimePerTest)
	// A session-tier recording a test owns, answered later than its request.
	reused := exactMongo("reused", 2100*time.Millisecond, models.LifetimeSession)
	reused.Spec.ResTimestampMock = exactT0.Add(2900 * time.Millisecond)
	early := exactMongo("before-answer", 2800*time.Millisecond, models.LifetimePerTest)
	end := exactMongo("end-sessions", 3*time.Second, models.LifetimePerTest)
	// A protocol-phase recording made later is no test's.
	hs := configMock("handshake", 3500*time.Millisecond)
	slice := []string{"boot", "gap", "before-answer", "end-sessions"}
	mm := stagedManager(t, []*models.Mock{boot, owned, gap, owned2, early, end}, []*models.Mock{reused, hs})
	mm.SetPoolNames(slice)
	mm.ServeExactPool("k", slice)
	perTestReads(t, mm, "before the first test")
	mm.ServeExactTest("T", []*models.Mock{owned})
	if !mm.DeleteFilteredMock(*owned) {
		t.Fatal("precondition: the test's own recording did not answer it")
	}
	mm.ServeExactPool("k", slice)
	perTestReads(t, mm, "between two tests", "end-sessions")
	mm.ServeExactTest("T2", []*models.Mock{owned2, reused})
	if !mm.DeleteFilteredMock(*owned2) {
		t.Fatal("precondition: the second test's own recording did not answer it")
	}
	mm.ServeExactPool("k", slice)
	perTestReads(t, mm, "after the last test", "end-sessions")

	bare := stagedManager(t, []*models.Mock{owned, end}, nil)
	bare.ServeExactPool("k", []string{"end-sessions"})
	bare.ServeExactTest("T", []*models.Mock{owned})
	bare.ServeExactPool("k", []string{"end-sessions"})
	perTestReads(t, bare, "after the test, no windows and no pool names")

	// Pool names, but no test owns a recording: nothing tells where the
	// tests were, so none is added either.
	pooled := stagedManager(t, []*models.Mock{boot, end}, nil)
	pooled.SetPoolNames([]string{"boot", "end-sessions"})
	pooled.ServeExactPool("k", []string{"boot", "end-sessions"})
	pooled.ServeExactTest("T", nil)
	pooled.ServeExactPool("k", []string{"boot", "end-sessions"})
	perTestReads(t, pooled, "after a test, no recording a test owns")
}

// keploy mock replay of a suite of several test processes run one after
// another (go test ./..., one test binary per package): each binary sends
// its own calls after its own last test (a Go mongo-driver's endSessions at
// client.Disconnect), recorded in its key's pool slice. Each process's
// teardown is bounded by its own tests' recordings alone (SetPoolOwners): a
// recording only another key's tests list is no bound of it. So after a's
// last test a per-test read shows a's teardown, though b's tests were
// recorded later, and b's after b's; neither before its first test (b's is
// shown in b's start-up, once a test fired: a stated limit), nor another
// key's. It stays fail-closed: with no owners (an agent that sent none), a
// recording whose owner key is not known (its test's ID names its test file
// rather than a key, or tests of both keys list it), or a second process of
// key a after b, a's teardown is bounded by those tests too, and no per-test
// read shows it after a's last test.
func TestEachTestProcessesTeardownIsBoundedByItsOwnTests(t *testing.T) {
	aBoot := exactMongo("a-boot", 100*time.Millisecond, models.LifetimePerTest)
	a1 := exactMongo("a1", time.Second, models.LifetimePerTest)
	a2 := exactMongo("a2", 1500*time.Millisecond, models.LifetimePerTest)
	aEnd := exactMongo("a-end", 2*time.Second, models.LifetimePerTest)
	bBoot := exactMongo("b-boot", 2500*time.Millisecond, models.LifetimePerTest)
	b1 := exactMongo("b1", 3*time.Second, models.LifetimePerTest)
	bEnd := exactMongo("b-end", 3500*time.Millisecond, models.LifetimePerTest)
	aSlice, bSlice := []string{"a-boot", "a-end"}, []string{"b-boot", "b-end"}
	all := []*models.Mock{aBoot, a1, a2, aEnd, bBoot, b1, bEnd}
	owners := map[string]string{"a1": "a", "a2": "a", "b1": "b"}
	// As the agent hands them (agent.PoolNamesSetter): the owners with the
	// pool names, through the proxy.
	setup := func(owners map[string]string) *MockManager {
		mm := stagedManager(t, all, nil)
		if err := (&Proxy{mockManager: mm}).SetPoolNames(append(append([]string{}, aSlice...), bSlice...), owners); err != nil {
			t.Fatalf("SetPoolNames: %v", err)
		}
		return mm
	}
	runA := func(mm *MockManager) {
		mm.ServeExactPool("a", aSlice)
		perTestReads(t, mm, "a's start-up")
		mm.ServeExactTest("a::TestA1", []*models.Mock{a1})
		if !mm.DeleteFilteredMock(*a1) {
			t.Fatal("precondition: a1 did not answer TestA1")
		}
		mm.ServeExactPool("a", aSlice)
		mm.ServeExactTest("a::TestA2", []*models.Mock{a2})
		if !mm.DeleteFilteredMock(*a2) {
			t.Fatal("precondition: a2 did not answer TestA2")
		}
		mm.ServeExactPool("a", aSlice)
	}

	// One staging generation, bounds read by key in turn: each key's own (the cache
	// is one key's), and outside every claim (PoolKeyNone) every test's.
	mm := setup(owners)
	at := aEnd.Spec.ReqTimestampMock
	for i, tc := range []struct {
		key  string
		want bool
	}{{"a", true}, {"a", true}, {"b", false}, {"a", true}, {models.PoolKeyNone, false}} {
		if after := mm.teardownAfter(tc.key, ""); after == nil || after(at) != tc.want {
			t.Errorf("read %d, key %q: a-end is the teardown = %v, want %v", i, tc.key, after != nil && after(at), tc.want)
		}
	}
	if after := mm.teardownAfter("b", ""); after == nil || !after(bEnd.Spec.ReqTimestampMock) {
		t.Errorf("key b: b-end is not b's teardown")
	}
	// New owners in the same generation move a's bound: b1 now a's too.
	mm.teardownAfter("a", "")
	mm.SetPoolOwners(map[string]string{"a1": "a", "a2": "a", "b1": "a"})
	if after := mm.teardownAfter("a", ""); after == nil || after(at) {
		t.Errorf("FALSE GREEN? new owners naming b1 a's: a-end still a's teardown (a stale bound)")
	}
	mm.SetPoolOwners(owners)
	runA(mm)
	perTestReads(t, mm, "after a's last test", "a-end")
	if !mm.DeleteFilteredMock(*aEnd) {
		t.Fatal("a's call after its last test was not answered by its own recording of it")
	}
	perTestReads(t, mm, "after a's teardown answered")
	mm.ServeExactPool("b", bSlice)
	perTestReads(t, mm, "b's start-up, a test fired", "b-end")
	mm.ServeExactTest("b::TestB", []*models.Mock{b1})
	if !mm.DeleteFilteredMock(*b1) {
		t.Fatal("precondition: b1 did not answer TestB")
	}
	mm.ServeExactPool("b", bSlice)
	perTestReads(t, mm, "after b's last test", "b-end")
	if !mm.DeleteFilteredMock(*bEnd) {
		t.Fatal("b's call after its last test was not answered by its own recording of it")
	}

	for _, tc := range []struct {
		name   string
		owners map[string]string
	}{
		{"no owners sent", nil},
		{"b's recording of no known key", map[string]string{"a1": "a", "a2": "a", "b1": ""}},
		{"a second process of key a after b", map[string]string{"a1": "a", "a2": "a", "b1": "a"}},
	} {
		mm := setup(tc.owners)
		runA(mm)
		perTestReads(t, mm, "FALSE GREEN? after a's last test, "+tc.name)
	}
}

// keploy mock replay's teardown bound is read once per staging generation
// of the trees: the per-test reads of one burst (GetFilteredMocks, its
// by-kind read, ...) reuse it while the generation and the scope table
// stand, not every staged recording at every read. An exact serve moves
// Revision but not the staging generation (exact serving hides what it
// serves rather than deleting it from the trees, and the bound reads the
// trees alone), so the next burst reuses it too: main read the trees again
// after each served call. A new staging (a recording a test owns, made
// later) moves it, so the pool's recording made before that one is no
// teardown any more; a new scope table moves it too.
func TestTheTeardownBoundIsReadOncePerTreeRevision(t *testing.T) {
	owned := exactMongo("owned", time.Second, models.LifetimePerTest)
	end := exactMongo("end-sessions", 3*time.Second, models.LifetimePerTest)
	mm := stagedManager(t, []*models.Mock{owned, end}, nil)
	mm.SetPoolNames([]string{"end-sessions"})
	after := mm.teardownAfter("k", "")
	b := mm.exact.teardownLast.Load()
	if after == nil || b == nil || !after(end.Spec.ReqTimestampMock) || after(owned.Spec.ReqTimestampMock) {
		t.Fatalf("precondition: end-sessions is not the teardown alone (bound %+v)", b)
	}
	if mm.teardownAfter("k", ""); mm.exact.teardownLast.Load() != b {
		t.Errorf("a second read in one revision read the trees again")
	}
	// Three bursts of three reads, an exact serve after the second: one scan
	// of the trees, where every read scanned them (nine), and one per
	// revision two.
	slice := []string{"end-sessions"}
	mm.ServeExactPool("k", slice)
	mm.ServeExactTest("T", []*models.Mock{owned})
	if !mm.DeleteFilteredMock(*owned) {
		t.Fatalf("precondition: owned did not answer its test")
	}
	mm.ServeExactPool("k", slice)
	scans, last := 0, mm.exact.teardownLast.Load()
	burst := func() {
		for _, read := range []func(){
			func() { _, _ = mm.GetFilteredMocks() },
			func() { _, _ = mm.GetFilteredMocksByKind(models.Mongo) },
			func() { mm.teardownAfter("k", "") },
		} {
			read()
			if c := mm.exact.teardownLast.Load(); c != last {
				scans, last = scans+1, c
			}
		}
	}
	burst()
	burst()
	rev := mm.Revision()
	if !mm.DeleteFilteredMock(*end) {
		t.Fatalf("precondition: end-sessions, the teardown, did not answer from the pool")
	}
	if mm.Revision() == rev {
		t.Fatalf("precondition: an exact serve did not move the revision")
	}
	burst()
	if scans != 1 || last == nil || last.last != b.last {
		t.Errorf("three bursts, an exact serve between: %d scans (bound %+v), want 1 to the same bound %s", scans, last, b.last)
	}
	mm.ServeExactPool("k", nil)
	late := exactMongo("owned-late", 4*time.Second, models.LifetimePerTest)
	mm.SetMocksWithWindow([]*models.Mock{owned, end, late}, nil, models.BaseTime, time.Now())
	if after := mm.teardownAfter("k", ""); after == nil || after(end.Spec.ReqTimestampMock) || !after(late.Spec.ResTimestampMock.Add(time.Millisecond)) {
		t.Errorf("FALSE GREEN? a new staging, a test's recording made after end-sessions: end-sessions still the teardown (a stale bound)")
	}
	mm.SetPoolNames([]string{"end-sessions", "owned-late"})
	if after := mm.teardownAfter("k", ""); after == nil || !after(end.Spec.ReqTimestampMock) {
		t.Errorf("a new scope table naming owned-late the pool's: end-sessions is not the teardown (a stale bound)")
	}
}

// The reached worker scope is shown whether a test is open or not, so after
// a test a per-test read shows its per-test recordings of the set's teardown,
// as it shows the pool slice's, and never one made before the set's last test
// ended. In a test a per-test read is unchanged: it shows the test's own
// alone (the worker scope's per-test recordings there are a stated limit,
// read through no per-test read).
func TestAfterTheLastTestAPerTestReadShowsTheReachedWorkerScopesTeardown(t *testing.T) {
	early := exactMongo("worker-early", 200*time.Millisecond, models.LifetimePerTest)
	owned := exactMongo("owned", time.Second, models.LifetimePerTest)
	w := exactMongo("worker", 3*time.Second, models.LifetimePerTest)
	mm := stagedManager(t, []*models.Mock{early, owned, w}, nil)
	exactWindows(mm)
	mm.ServeExactWorker([]string{"worker-early", "worker"})
	mm.ServeExactPool("k", nil)
	perTestReads(t, mm, "before the first test")
	mm.ServeExactTest("T", []*models.Mock{owned})
	perTestReads(t, mm, "in the test", "owned")
	mm.ServeExactPool("k", nil)
	perTestReads(t, mm, "after the test", "worker")
}

// Under the background rules (a set recorded with proven routing) the call
// after the last test is answered from the pool's recording too, once.
func TestUnderTheBackgroundRulesTheCallAfterTheLastTestAnswersOnce(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	owned := exactMongo("owned", time.Second, models.LifetimePerTest)
	end := exactMongo("end-sessions", 3*time.Second, models.LifetimePerTest)
	mm := stagedManager(t, []*models.Mock{owned, end}, nil)
	exactWindows(mm)
	mm.ServeExactBackground(models.RecordedWithS12Routing, []string{"end-sessions"}, nil)
	mm.ServeExactPool(models.PoolKeyNone, []string{"end-sessions"})
	perTestReads(t, mm, "before the first test")
	mm.ServeExactTest("T", []*models.Mock{owned})
	if !mm.DeleteFilteredMock(*owned) {
		t.Fatal("precondition: the test's own recording did not answer it")
	}
	mm.ServeExactPool(models.PoolKeyNone, []string{"end-sessions"})
	perTestReads(t, mm, "after the last test", "end-sessions")
	if !mm.DeleteFilteredMock(*end) || mm.DeleteFilteredMock(*end) {
		t.Fatal("under the background rules the call after the last test was not answered exactly once")
	}
}

// The pool slice's stateful cursors are its key's own, and a test's are its
// own: a test does not read the pool's, and a cursor moved in a test leaves
// the pool's where it was.
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
	if got := mm.MockCursorIndex("GET /n", 3); got != 0 {
		t.Fatalf("a test reads a pool's cursor: %d", got)
	}
	mm.AdvanceMockCursor("GET /n", 1, 3)
	if got := mm.MockCursorIndex("GET /n", 3); got != 2 {
		t.Fatalf("the test's cursor: %d", got)
	}
	mm.ServeExactPool("a", nil)
	if got := mm.MockCursorIndex("GET /n", 3); got != 1 {
		t.Fatalf("key a's cursor after the test: %d", got)
	}
}

// A reusable request recorded more than once answers in record order inside
// a test: a parser that walks its recordings with the store's cursor (an HTTP
// served mark, a Postgres session sequence) is not answered the first
// recording at every ask, another ask's answer passed for this one's. The
// test's cursor is its own: the next test, and the same test opened again (a
// retry), start the sequence over, and an ended test's cursors are kept
// for its own calls only until the next staging.
func TestARequestRecordedTwiceInATestAnswersInRecordOrder(t *testing.T) {
	mm := stagedManager(t, nil, nil)
	const seq = "postgres-v3/session-sequence\x00probe"
	// ask is one call a parser answers: it peeks at the sequence's position
	// and moves it past the recording it served.
	ask := func(key string) int {
		i := mm.MockCursorIndex(key, 2)
		mm.AdvanceMockCursor(key, i, 2)
		return i
	}
	asks := func(key string, n int) []int {
		var got []int
		for ; n > 0; n-- {
			got = append(got, ask(key))
		}
		return got
	}
	mm.ServeExactPool("-", nil)
	if got := asks(seq, 1); !reflect.DeepEqual(got, []int{0}) {
		t.Fatalf("between tests: %v", got)
	}
	mm.ServeExactTest("T", nil)
	if got := asks(seq, 3); !reflect.DeepEqual(got, []int{0, 1, 1}) {
		t.Fatalf("in a test the recordings answered %v; want 0, 1 and then the last again", got)
	}
	if got := asks("perTest\x00GET /n", 2); !reflect.DeepEqual(got, []int{0, 1}) {
		t.Fatalf("a per-test sequence in a test answered %v; want 0 1, the test's own", got)
	}
	mm.ServeExactTest("T", nil)
	if got := asks(seq, 2); !reflect.DeepEqual(got, []int{0, 1}) {
		t.Fatalf("the test opened again answered %v; want its sequence started over", got)
	}
	// The next test, opened right after: a call of its answered as its view
	// goes in, before the ended test's cursors are closed, keeps its place.
	var early []int
	mm.exact.beforeBump = func(*exactView) { early = append(early, ask(seq)) }
	mm.ServeExactTest("U", nil)
	mm.exact.beforeBump = nil
	if got := append(early, asks(seq, 1)...); !reflect.DeepEqual(got, []int{0, 1}) {
		t.Fatalf("the next test answered %v; want its own sequence, in record order", got)
	}
	mm.ServeExactPool("-", nil)
	if got := asks(seq, 1); !reflect.DeepEqual(got, []int{1}) {
		t.Fatalf("between tests after the tests: %v; want the pool's own cursor, moved once", got)
	}
	// An ended test's cursors are closed with its view and kept for the
	// calls read in it, and the next staging drops them: nothing of an
	// ended test is kept past its set.
	mm.cursorMu.Lock()
	if len(mm.testCursors) == 0 || len(mm.closedTests) != len(mm.testCursors) {
		t.Fatalf("the ended tests' cursors %v, closed %v; want each kept, closed, until the next staging", mm.testCursors, mm.closedTests)
	}
	mm.cursorMu.Unlock()
	mm.ResetStatefulCursors()
	mm.cursorMu.Lock()
	defer mm.cursorMu.Unlock()
	if len(mm.testCursors) != 0 || len(mm.closedTests) != 0 {
		t.Fatalf("an ended test's cursors are kept past the staging: %v %v", mm.testCursors, mm.closedTests)
	}
}

// A call peeks at its sequence and commits after it answers; a view installed
// between the two (a test opening or ending) does not move the next view's
// sequence by the peeked one's position. Through one SequenceView the commit
// moves the sequence it peeked: the pool's own, or the ended test's own,
// which no other view reads.
func TestAPeekAndItsCommitMoveOneViewsSequence(t *testing.T) {
	const key = "postgres-v3/session-sequence\x00probe"
	view := func(mm *MockManager) integrations.MockCursor {
		sv, ok := mm.SequenceView()
		if !ok {
			t.Fatal("no sequence view under exact serving")
		}
		return sv.Cursor
	}
	ask := func(mm *MockManager) int {
		c := view(mm)
		i := c.MockCursorIndex(key, 3)
		c.AdvanceMockCursor(key, i, 3)
		return i
	}
	t.Run("pool to test", func(t *testing.T) {
		mm := stagedManager(t, nil, nil)
		mm.ServeExactPool("-", nil)
		ask(mm)
		ask(mm)
		c := view(mm)
		idx := c.MockCursorIndex(key, 3)
		mm.ServeExactTest("T", nil)
		c.AdvanceMockCursor(key, idx, 3)
		if got := view(mm).MockCursorIndex(key, 3); got != 0 {
			t.Fatalf("the test's first ask reads %d: moved by the pool's peek of %d", got, idx)
		}
		mm.ServeExactPool("-", nil)
		if got := view(mm).MockCursorIndex(key, 3); got != 2 {
			t.Fatalf("the pool reads %d; want 2: its own commit moved it", got)
		}
	})
	t.Run("test to test", func(t *testing.T) {
		mm := stagedManager(t, nil, nil)
		mm.ServeExactPool("-", nil)
		mm.ServeExactTest("T1", nil)
		ask(mm)
		c := view(mm)
		idx := c.MockCursorIndex(key, 3)
		mm.ServeExactTest("T2", nil)
		c.AdvanceMockCursor(key, idx, 3)
		if got := view(mm).MockCursorIndex(key, 3); got != 0 {
			t.Fatalf("T2's first ask reads %d: moved by T1's peek of %d", got, idx)
		}
	})
	t.Run("a reset in a test", func(t *testing.T) {
		mm := stagedManager(t, nil, nil)
		mm.ServeExactPool("-", nil)
		mm.ServeExactTest("T", nil)
		ask(mm)
		ask(mm)
		mm.ResetStatefulCursors()
		if got := view(mm).MockCursorIndex(key, 3); got != 0 {
			t.Fatalf("the open test reads %d after a reset of the cursors; want 0, the sequence's start", got)
		}
	})
	t.Run("test to pool", func(t *testing.T) {
		mm := stagedManager(t, nil, nil)
		mm.ServeExactPool("-", nil)
		mm.ServeExactTest("T", nil)
		ask(mm)
		ask(mm)
		c := view(mm)
		idx := c.MockCursorIndex(key, 3)
		mm.ServeExactPool("-", nil)
		c.AdvanceMockCursor(key, idx, 3)
		if got := view(mm).MockCursorIndex(key, 3); got != 0 {
			t.Fatalf("the pool's first ask reads %d: moved by the test's peek of %d", got, idx)
		}
		mm.cursorMu.Lock()
		defer mm.cursorMu.Unlock()
		if len(mm.cursors) != 0 {
			t.Fatalf("a commit after its test ended moved the pool's cursors: %v", mm.cursors)
		}
		for gen, cursors := range mm.testCursors {
			if cursors[key] != 3 {
				t.Fatalf("a commit after its test ended (test view %d): %v; want the test's own sequence moved past the 2 it peeked", gen, cursors)
			}
		}
	})
}

// A call read in a test and walked after the test ended (a session query in
// flight across the test's end, a held store's sequence view) reads the
// position the test's calls reached, so it is answered what it would have
// been answered before the end, never the test's first recording again.
// Its commit moves that test's own sequence, which no other view reads, so
// two calls read in the test and walked after it answer in record order
// wherever the end lands between them: neither answer depends on when the
// end lands. The next test's sequence starts at its own start, and the
// next staging drops the ended test's cursors.
func TestACallReadInATestAndWalkedAfterItEndedReadsWhereTheTestLeftIt(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "1")
	const key, n = "postgres-v3/session-sequence\x00probe", 5
	for _, endBeforeXCommits := range []bool{false, true} {
		t.Run(fmt.Sprintf("the end lands before the first late call commits=%v", endBeforeXCommits), func(t *testing.T) {
			mm := stagedManager(t, nil, nil)
			mm.ServeExactPool("-", nil)
			mm.ServeExactTest("T", nil)
			sv, ok := mm.SequenceView()
			if !ok {
				t.Fatal("no sequence view under exact serving")
			}
			c := sv.Cursor
			for i := 0; i < 2; i++ {
				c.AdvanceMockCursor(key, c.MockCursorIndex(key, n), n)
			}
			held, _ := mm.HoldToRound(mm.MarkCallRound())
			// X, read in T, peeks before T ends; Y, read in T too, is walked
			// after T ended.
			x := c.MockCursorIndex(key, n)
			if !endBeforeXCommits {
				c.AdvanceMockCursor(key, x, n)
			}
			mm.ServeExactPool("-", nil) // T ends
			if endBeforeXCommits {
				c.AdvanceMockCursor(key, x, n)
			}
			hv, ok := integrations.SequenceViewOf(held)
			if !ok || hv.Test != "T" {
				t.Fatalf("the held call's view is test %q (%v); want T's", hv.Test, ok)
			}
			if x != 2 {
				t.Fatalf("X, read in T after two asks, peeked %d; want 2", x)
			}
			for name, cur := range map[string]integrations.MockCursor{"the view it read": c, "the held store's view": hv.Cursor} {
				if got := cur.MockCursorIndex(key, n); got != 3 {
					t.Fatalf("%s: Y, read in T and walked after T ended, reads %d; want 3, the next of T's sequence (0 re-walks T from its start, 2 answers X's recording again)", name, got)
				}
			}
			if now, _ := mm.SequenceView(); now.Cursor.MockCursorIndex(key, n) != 0 {
				t.Fatal("a commit of a call read in T moved the pool's sequence")
			}
			mm.ServeExactTest("U", nil)
			if now, _ := mm.SequenceView(); now.Cursor.MockCursorIndex(key, n) != 0 {
				t.Fatal("the next test's sequence does not start at its own start")
			}
			mm.ResetStatefulCursors()
			if got := c.MockCursorIndex(key, n); got != 0 {
				t.Fatalf("after the next staging, T's cursor still reads %d; want it dropped", got)
			}
			hv.Cursor.AdvanceMockCursor(key, 3, n)
			if got := c.MockCursorIndex(key, n); got != 0 {
				t.Fatalf("after the next staging, a commit of a call read in T moved %d; want nothing kept", got)
			}
		})
	}
}

// openingAt is the manager whose next test opens (open, once) between a
// call's first read of the store and its first look at a cursor: a call made
// between tests still in flight as the test begins. Under the proven switch
// the call is held to its round (HoldToRound) and reads the held store from
// then on, so the test opens as it is held.
type openingAt struct {
	*MockManager
	open func()
	done bool
}

func (s *openingAt) fire() {
	if !s.done {
		s.done = true
		s.open()
	}
}

func (s *openingAt) SequenceView() (integrations.SequenceView, bool) {
	s.fire()
	return s.MockManager.SequenceView()
}

func (s *openingAt) MockCursorIndex(key string, n int) int {
	s.fire()
	return s.MockManager.MockCursorIndex(key, n)
}

func (s *openingAt) HoldToRound(call integrations.CallRound) (integrations.MockMemDb, func() bool) {
	held, restaged := s.MockManager.HoldToRound(call)
	s.fire()
	return held, restaged
}

// A call read in one view is walked in the view it was read in (its call
// round's), however late it looks at a cursor: a held store's sequence view
// is the call's view, its test, recorded window and cursor, so a test that
// opens meanwhile takes a call read between tests into neither its window nor
// its cursor, and a test that ends meanwhile leaves its call in both. Through
// the HTTP integration, a request of the set's worker scope recorded at the
// set's start, read between tests while the test opens, is answered the
// first recording, and the test's call the next, by placement: the test's
// answers never depend on goroutine timing.
func TestACallReadBeforeATestOpensIsWalkedInTheViewItWasReadIn(t *testing.T) {
	// Exact serving runs under the proven switch alone (the replay's
	// exactServing), where a call's first read marks its round.
	t.Setenv("KEPLOY_PROVEN", "1")
	window := models.TestWindow{TestCase: "A", Start: exactT0.Add(3 * time.Second), End: exactT0.Add(5 * time.Second)}
	t.Run("the held store's sequence view", func(t *testing.T) {
		const key = "probe"
		mm := stagedManager(t, nil, []*models.Mock{seqCall("g0", time.Second, "g0")})
		mm.SeedRecordedWindows([]models.TestWindow{window})
		mm.ServeExactWorker([]string{"g0"})
		mm.ServeExactPool("k", nil)
		held, _ := mm.HoldToRound(mm.MarkCallRound())
		mm.ServeExactTest("A", nil)
		sv, ok := integrations.SequenceViewOf(held)
		if !ok || sv.Test != "" || !sv.Start.IsZero() || sv.Worker != nil {
			t.Fatalf("a call read between tests, held, walks test %q from %v (worker scope told %v) once the test opened; want the pool view's: no test, no window, the worker scope one sequence with the pool's", sv.Test, sv.Start, sv.Worker != nil)
		}
		sv.Cursor.AdvanceMockCursor(key, sv.Cursor.MockCursorIndex(key, 2), 2)
		if now, _ := mm.SequenceView(); now.Cursor.MockCursorIndex(key, 2) != 0 {
			t.Fatal("the gap call's commit moved the open test's sequence")
		}
		inTest, _ := mm.HoldToRound(mm.MarkCallRound())
		mm.ServeExactPool("k", nil)
		if now, _ := mm.SequenceView(); now.Cursor.MockCursorIndex(key, 2) != 1 {
			t.Fatal("the gap call's commit did not move the pool's sequence")
		}
		if sv, _ := integrations.SequenceViewOf(inTest); sv.Test != "A" || !sv.Start.Equal(window.Start) || !sv.End.Equal(window.End) || sv.Worker == nil || !sv.Worker(seqCall("g0", time.Second, "g0")) {
			t.Fatalf("a call read in test A, held, walks test %q over [%v, %v] (worker scope told %v) once the test ended; want A's recorded window and its worker scope", sv.Test, sv.Start, sv.End, sv.Worker != nil)
		}
	})
	for _, straddle := range []bool{false, true} {
		t.Run(fmt.Sprintf("test opens before the call looks at a cursor=%v", straddle), func(t *testing.T) {
			mm := stagedManager(t, nil, []*models.Mock{seqCall("s0", time.Second, "s0"), seqCall("s1", 2*time.Second, "s1")})
			mm.SeedRecordedWindows([]models.TestWindow{window})
			mm.ServeExactWorker([]string{"s0", "s1"})
			mm.ServeExactPool("k", nil)
			opened := false
			open := func() {
				if !opened {
					opened = true
					mm.ServeExactTest("A", nil)
				}
			}
			var db integrations.MockMemDb = mm
			if straddle {
				db = &openingAt{MockManager: mm, open: open}
			}
			gap := probe(t, db)
			open()
			// The gap call walks the calls between tests' cursor, the
			// view it was read in; the windows walk their own, so test
			// A's first is s0 too. Main's row wanted s1: one cursor for
			// both, so A's answer moved with the calls between tests. A
			// gap call walked in A's view would advance A's and give s1.
			if first := probe(t, mm); gap != "s0" || first != "s0" {
				t.Fatalf("the call made between tests answered %q and test A's first %q; want s0 and s0, whenever the test opened: the windows' sequence is apart from the calls between tests'", gap, first)
			}
		})
	}
}

// A connection view made before exact serving began passes through to the
// manager (passThrough), its sequence view too: in a test, a request its
// worker scope recorded more than once at the set's start is answered by
// placement, and one the test recorded more than once its own in record
// order, as through the manager; and a pool peek committed after the test
// opened moves the pool's sequence, never the test's. Both with the proven
// switch off and on.
func TestAConnectionViewMadeBeforeExactServingWalksAsTheManagerDoes(t *testing.T) {
	for _, sw := range []string{"", "1"} {
		t.Run("switch="+sw, func(t *testing.T) {
			t.Setenv("KEPLOY_PROVEN", sw)
			win := func(name string, from, to time.Duration) models.TestWindow {
				return models.TestWindow{TestCase: name, Start: exactT0.Add(from), End: exactT0.Add(to)}
			}
			t.Run("the worker scope by placement", func(t *testing.T) {
				w := []*models.Mock{seqCall("w0", time.Second, "0"), seqCall("w1", 2*time.Second, "1"), seqCall("w2", 3*time.Second, "2")}
				mm := stagedManager(t, nil, w)
				mm.SeedRecordedWindows([]models.TestWindow{win("T", 4*time.Second, 5*time.Second)})
				mm.ServeExactWorker([]string{"w0", "w1", "w2"})
				mm.ServeExactPool("k", nil)
				mm.ServeExactTest("T", nil)
				db := &scopedMockDb{MockMemDb: mm}
				if got := []string{probe(t, db), probe(t, db), probe(t, db)}; !reflect.DeepEqual(got, []string{"0", "1", "2"}) {
					t.Fatalf("in a test, through the view, answered %v; want 0 1 2", got)
				}
			})
			t.Run("a test's own", func(t *testing.T) {
				a := []*models.Mock{seqCall("a0", time.Second, "a")}
				b := []*models.Mock{seqCall("b0", 3*time.Second, "b"), seqCall("b1", 4*time.Second, "b2")}
				mm := stagedManager(t, nil, append(append([]*models.Mock{}, a...), b...))
				mm.SeedRecordedWindows([]models.TestWindow{win("A", 500*time.Millisecond, 2*time.Second), win("B", 2500*time.Millisecond, 5*time.Second)})
				mm.ServeExactPool("k", nil)
				mm.ServeExactTest("B", b)
				db := &scopedMockDb{MockMemDb: mm}
				if got := []string{probe(t, db), probe(t, db)}; !reflect.DeepEqual(got, []string{"b", "b2"}) {
					t.Fatalf("test B, through the view, answered %v; want b b2, its own", got)
				}
			})
			t.Run("a pool peek committed in a test", func(t *testing.T) {
				const key = "postgres-v3/session-sequence\x00probe"
				mm := stagedManager(t, nil, nil)
				db := &scopedMockDb{MockMemDb: mm}
				view := func() integrations.MockCursor {
					sv, ok := integrations.SequenceViewOf(db)
					if !ok {
						t.Fatal("a view made before exact serving began gives no sequence view under it")
					}
					return sv.Cursor
				}
				mm.ServeExactPool("-", nil)
				for i := 0; i < 2; i++ {
					c := view()
					j := c.MockCursorIndex(key, 3)
					c.AdvanceMockCursor(key, j, 3)
				}
				c := view()
				idx := c.MockCursorIndex(key, 3)
				mm.ServeExactTest("T", nil)
				c.AdvanceMockCursor(key, idx, 3)
				if got := view().MockCursorIndex(key, 3); got != 0 {
					t.Fatalf("the test's first ask reads %d, moved by the pool's peek of %d", got, idx)
				}
			})
		})
	}
}

// seqCall is a reusable HTTP recording of GET http://deps/probe at at,
// answered body, walked by the stateful cursor (as the agent derives an
// untagged HTTP mock before staging).
func seqCall(name string, at time.Duration, body string) *models.Mock {
	m := httpCall(name, at, "http://deps/probe")
	m.Spec.Metadata = map[string]string{"type": models.HTTPClient}
	m.Spec.HTTPReq.Header = map[string]string{"Host": "deps"}
	m.Spec.HTTPResp = &models.HTTPResp{StatusCode: 200, Header: map[string]string{"Content-Length": strconv.Itoa(len(body))}, Body: body}
	m.Spec.ResTimestampMock = m.Spec.ReqTimestampMock.Add(time.Millisecond)
	m.TestModeInfo.Consume = models.ConsumeCursorSaturate
	return m
}

// probe asks GET /probe through the real HTTP integration and returns the
// body answered.
func probe(t *testing.T, db integrations.MockMemDb) string {
	t.Helper()
	resp, got, _ := serveOne(t, httpint.New(zap.NewNop()), db, "GET /probe HTTP/1.1\r\nHost: deps\r\n\r\n")
	if resp == nil {
		return "no answer"
	}
	return string(got)
}

// A request a test recorded more than once is answered, in the test, its own
// recordings in record order, each once (a test view serves them as its own,
// never through the stateful walk): a test is answered its own, never those
// of a test before it that this run did not make (a selective replay, the
// kept set), whichever tests ran before it. Once its own are spent, the set's
// worker scope answers the request: it belongs to no test, so it is walked by
// placement from the set's start on, as between tests (a test that recorded
// none of the request is answered the same). With the proven switch off, a
// test's call past its own recordings takes the worker scope's next reached
// recording. Under the switch, a call identical to one the test's own
// recordings answered is one call more than the test recorded
// (integrations.OwnRepeatRead): the worker scope never answers it, and its
// recordings stay unspent for the request between tests. Then nothing, never
// an earlier recording again. Each case runs through every pass that walks
// the sequence (workerWalkPasses).
func TestATestsOwnRecordingsAnswerInRecordOrderThenTheWorkerScope(t *testing.T) {
	win := func(name string, from, to time.Duration) models.TestWindow {
		return models.TestWindow{TestCase: name, Start: exactT0.Add(from), End: exactT0.Add(to)}
	}
	const miss = "no answer"
	for _, pass := range workerWalkPasses() {
		t.Run(pass.name, func(t *testing.T) {
			t.Setenv("KEPLOY_PROVEN", pass.sw)
			asks := func(mm *MockManager, n int) []string {
				var got []string
				for i := 0; i < n; i++ {
					got = append(got, pass.ask(t, mm))
				}
				return got
			}
			t.Run("in record order", func(t *testing.T) {
				own := []*models.Mock{pass.rec("w0", time.Second, "0"), pass.rec("w1", 2*time.Second, "1"), pass.rec("w2", 3*time.Second, "2")}
				mm := stagedManager(t, nil, own)
				mm.SeedRecordedWindows([]models.TestWindow{win("T", 500*time.Millisecond, 4*time.Second)})
				mm.ServeExactPool("k", nil)
				mm.ServeExactTest("T", own)
				if got := asks(mm, 4); !reflect.DeepEqual(got, []string{"0", "1", "2", miss}) {
					t.Fatalf("in a test answered %v; want 0 1 2, then nothing", got)
				}
			})
			t.Run("a test's own, not an earlier test's", func(t *testing.T) {
				run := func(aFirst bool) []string {
					a := []*models.Mock{pass.rec("a0", time.Second, "a")}
					b := []*models.Mock{pass.rec("b0", 3*time.Second, "b"), pass.rec("b1", 4*time.Second, "b2")}
					mm := stagedManager(t, nil, append(append([]*models.Mock{}, a...), b...))
					mm.SeedRecordedWindows([]models.TestWindow{win("A", 500*time.Millisecond, 2*time.Second), win("B", 2500*time.Millisecond, 5*time.Second)})
					mm.ServeExactPool("k", nil)
					if aFirst {
						mm.ServeExactTest("A", a)
						if got := pass.ask(t, mm); got != "a" {
							t.Fatalf("test A answered %q; want a", got)
						}
						mm.ServeExactPool("k", nil)
					}
					mm.ServeExactTest("B", b)
					return asks(mm, 3)
				}
				want := []string{"b", "b2", miss}
				if got := run(false); !reflect.DeepEqual(got, want) {
					t.Fatalf("test B, run without test A, answered %v; want %v: its own, then nothing", got, want)
				}
				if got := run(true); !reflect.DeepEqual(got, want) {
					t.Fatalf("test B, run after test A, answered %v; want %v, as without A", got, want)
				}
			})
			t.Run("the worker scope after the test's own", func(t *testing.T) {
				own := pass.rec("a0", 4*time.Second, "mine")
				mm := stagedManager(t, nil, []*models.Mock{pass.rec("s0", time.Second, "false"), pass.rec("s1", 2*time.Second, "true"), own})
				mm.SeedRecordedWindows([]models.TestWindow{win("A", 3*time.Second, 5*time.Second)})
				mm.ServeExactWorker([]string{"s0", "s1"})
				mm.ServeExactPool("k", nil)
				mm.ServeExactWorker([]string{"s0", "s1"})
				mm.ServeExactTest("A", []*models.Mock{own})
				if pass.sw == "" {
					if got := asks(mm, 4); !reflect.DeepEqual(got, []string{"mine", "false", "true", miss}) {
						t.Fatalf("test A answered %v; want its own, then the worker scope's by placement, then nothing", got)
					}
					return
				}
				if got := asks(mm, 3); !reflect.DeepEqual(got, []string{"mine", miss, miss}) {
					t.Fatalf("test A answered %v; want its own, then nothing: each call after it is one more than the test recorded", got)
				}
				mm.ServeExactPool("k", nil)
				if got := asks(mm, 3); !reflect.DeepEqual(got, []string{"false", "true", miss}) {
					t.Fatalf("between tests answered %v; want the worker scope's by placement, unspent by test A, then nothing", got)
				}
			})
			t.Run("a test that recorded none", func(t *testing.T) {
				// b0 is recorded in the gap after A, so it is reached
				// once A began.
				mm := stagedManager(t, nil, []*models.Mock{pass.rec("s0", time.Second, "false"), pass.rec("s1", 2*time.Second, "true"), pass.rec("b0", 6*time.Second, "later")})
				mm.SeedRecordedWindows([]models.TestWindow{win("A", 3*time.Second, 5*time.Second)})
				mm.ServeExactWorker([]string{"s0", "s1"})
				mm.ServeExactPool("k", nil)
				mm.ServeExactWorker([]string{"s0", "s1", "b0"})
				mm.ServeExactTest("A", nil)
				if got := asks(mm, 4); !reflect.DeepEqual(got, []string{"false", "true", "later", miss}) {
					t.Fatalf("test A, which recorded none, answered %v; want the worker scope's by placement, then nothing: never an earlier one again", got)
				}
			})
		})
	}
}

// A worker-scope request (models.Mapping.Worker) belongs to no test: record
// places each of its recordings in the gap it was made in, so no test's
// recorded window holds one. It is walked by placement on two cursors: one
// across the test windows and one across the calls between tests. Main
// walked one sequence for both, so how many polls landed between tests
// moved what a later window was answered, and with it the test's verdict,
// with load. Now neither's count moves the other's answers: polls made only
// between tests are answered as recorded, a poll made in a window is
// answered the windows' next recording by placement, and the set's end may
// find a reached recording unanswered (the worker scope is required of
// none). A recording whose place the run has not reached answers nothing, in
// a test or out of one. Each case runs through every pass that walks the sequence,
// with the proven switch off and on, and the agent's order of calls: the
// worker set named before each view goes in.
func TestAWorkerScopeRequestIsWalkedByPlacementWhereverItsCallsLand(t *testing.T) {
	const miss = "no answer"
	for _, pass := range workerWalkPasses() {
		t.Run(pass.name, func(t *testing.T) {
			t.Setenv("KEPLOY_PROVEN", pass.sw)
			// g0 is recorded at the set's start, g1 in the gap after
			// A, g2 in the gap after B: each is reachable once the test
			// before its gap began (MockEntry.Place).
			recorded := func() []*models.Mock {
				return []*models.Mock{pass.rec("g0", time.Second, "0"), pass.rec("g1", 3500*time.Millisecond, "1"), pass.rec("g2", 5500*time.Millisecond, "2")}
			}
			wins := []models.TestWindow{
				{TestCase: "A", Start: exactT0.Add(2 * time.Second), End: exactT0.Add(3 * time.Second)},
				{TestCase: "B", Start: exactT0.Add(4 * time.Second), End: exactT0.Add(5 * time.Second)},
			}
			// run replays the set with each poll made where lands says
			// (start, inA, gapA, inB or gapB), in order, and returns
			// what each was answered and the reached recordings the
			// set's end finds unanswered.
			run := func(lands ...string) ([]string, []string) {
				mm := stagedManager(t, nil, recorded())
				mm.SeedRecordedWindows(wins)
				var got []string
				next := 0
				poll := func(where string) {
					for next < len(lands) && lands[next] == where {
						got = append(got, pass.ask(t, mm))
						next++
					}
				}
				mm.ServeExactWorker([]string{"g0"})
				mm.ServeExactPool("k", nil)
				poll("start")
				mm.ServeExactWorker([]string{"g0", "g1"})
				mm.ServeExactTest("A", nil)
				poll("inA")
				mm.ServeExactWorker([]string{"g0", "g1"})
				mm.ServeExactPool("k", nil)
				poll("gapA")
				mm.ServeExactWorker([]string{"g0", "g1", "g2"})
				mm.ServeExactTest("B", nil)
				poll("inB")
				mm.ServeExactWorker([]string{"g0", "g1", "g2"})
				mm.ServeExactPool("k", nil)
				poll("gapB")
				if next != len(lands) {
					t.Fatalf("lands %v: poll %d made nowhere", lands, next)
				}
				answered := map[string]bool{}
				for _, n := range consumedNames(mm.GetConsumedMocks()) {
					answered[n] = true
				}
				var unanswered []string
				for _, n := range []string{"g0", "g1", "g2"} {
					if !answered[n] {
						unanswered = append(unanswered, n)
					}
				}
				return got, unanswered
			}
			for _, c := range []struct {
				lands      []string
				want       []string
				unanswered []string
				why        string
			}{
				{[]string{"start", "gapA", "gapB"}, []string{"0", "1", "2"}, nil,
					"made where recorded, all between tests: one sequence, as on main"},
				{[]string{"start", "inA", "gapB"}, []string{"0", "0", "1"}, []string{"g2"},
					"A's poll starts the windows' sequence at g0, whatever the gap answered; gapB goes on from the gap's g0"},
				{[]string{"start", "inB", "gapB"}, []string{"0", "0", "1"}, []string{"g2"},
					"B's poll starts the windows' sequence at g0; gapB goes on from the gap's g0"},
				{[]string{"inA", "gapA", "gapB"}, []string{"0", "0", "1"}, []string{"g2"},
					"the gaps' sequence starts at g0, whatever A's window answered"},
				{[]string{"start", "gapA", "inB"}, []string{"0", "1", "0"}, []string{"g2"},
					"B's poll starts the windows' sequence at g0, however many polls the gaps made"},
			} {
				got, unanswered := run(c.lands...)
				if !reflect.DeepEqual(got, c.want) || !reflect.DeepEqual(unanswered, c.unanswered) {
					t.Errorf("polls made %v answered %v, leaving %v unanswered at the set's end; want %v and %v: %s", c.lands, got, unanswered, c.want, c.unanswered, c.why)
				}
			}
			// Polls in one window walk the windows' sequence in placement
			// order, never an earlier recording again; and a recording
			// whose place the run has not reached answers nothing in a
			// test: three polls in A take g0 and g1 (g2's place is after
			// B began), then nothing. The gap's poll before them moves
			// none of A's answers.
			if got, _ := run("start", "inA", "inA", "inA"); !reflect.DeepEqual(got, []string{"0", "0", "1", miss}) {
				t.Errorf("a gap poll, then three polls in A, answered %v; want 0, then 0 1 and nothing: g2's place is after B began", got)
			}
		})
	}
}

// workerWalkPass is one pass of the HTTP matcher a worker-scope request is
// walked through: with the proven switch off (the exact-body pass), and on:
// the strict pass, the variable pass (recordings that declare a variable),
// the replay of learned noise (learnedMatch) and record's learn step (the
// full round).
type workerWalkPass struct {
	name string
	sw   string
	// dress makes a recording the pass's; vary sends each call its own
	// request id, which only learned noise excuses.
	dress func(*models.Mock)
	vary  bool
	opts  models.OutgoingOptions
	calls int
}

func workerWalkPasses() []*workerWalkPass {
	return []*workerWalkPass{
		{name: "exact-body pass (switch off)"},
		{name: "strict pass", sw: "1"},
		{name: "variable pass", sw: "1", dress: func(m *models.Mock) {
			m.Spec.Variables = models.MockVariables{"rid-" + m.Name: {Recorded: "rec", Bind: "header.X-Req-Id"}}
		}},
		{name: "learned noise (learnedMatch)", sw: "1", vary: true, dress: func(m *models.Mock) {
			m.Spec.ReqBodyNoise = map[string][]string{"header.x-req-id": {}}
		}},
		{name: "learn step", sw: "1", opts: models.OutgoingOptions{Learn: true, LearnPool: true}},
	}
}

// rec is a recording of GET /probe at at answered body, dressed for the pass.
func (p *workerWalkPass) rec(name string, at time.Duration, body string) *models.Mock {
	m := seqCall(name, at, body)
	m.Spec.HTTPReq.Header["X-Req-Id"] = "rec"
	if p.dress != nil {
		p.dress(m)
	}
	return m
}

// ask is one call of GET /probe through the pass, answered body or "no
// answer".
func (p *workerWalkPass) ask(t *testing.T, mm *MockManager) string {
	t.Helper()
	p.calls++
	id := "rec"
	if p.vary {
		id = "r" + strconv.Itoa(p.calls)
	}
	resp, body := learnServe(t, httpint.New(zap.NewNop()), mm, context.Background(), p.opts, "GET /probe HTTP/1.1\r\nHost: deps\r\nX-Req-Id: "+id+"\r\n\r\n")
	if resp == nil || resp.StatusCode != http.StatusOK {
		return "no answer"
	}
	return string(body)
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

// A session reset forgets the outcome of the session's last ended test too,
// as it does the open one's: until the next session opens a test, the
// outcome names no test. The outcome is trusted by its test's name, so one
// left over would report the previous session's test as this one's.
func TestASessionResetForgetsTheLastEndedTestsOutcome(t *testing.T) {
	a := exactMock("a", 0, models.LifetimePerTest)
	mm := stagedManager(t, []*models.Mock{a}, nil)
	mm.ServeExactPool("-", nil)
	mm.ServeExactTest("T", []*models.Mock{a})
	mm.ServeExactPool("-", nil)
	if test, _, unconsumed, ok := mm.ExactTestOutcome(); !ok || test != "T" || len(unconsumed) != 1 {
		t.Fatalf("before the reset the outcome is %q %v (ok %v); want test T's, with a unconsumed", test, unconsumed, ok)
	}
	mm.ResetForReplaySession()
	if test, consumed, unconsumed, ok := mm.ExactTestOutcome(); !ok || test != "" || len(consumed)+len(unconsumed) != 0 {
		t.Fatalf("after the reset the outcome is %q %v %v (ok %v); want no test's", test, consumed, unconsumed, ok)
	}
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
	verdict, wait, _ := mm.exactConsumeIn(v, &mk, true, nil)
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

// A miss is told it was made while the replay staged a set's mocks only
// where its call was decided: where the call first read the round, as its
// parser marked it (integrations.StampMark), never from the view serving
// when the miss reaches the proxy. A call decided under a staged set's view
// whose report reaches the proxy once the next set's staging opened is not
// told so; a call decided in a staging whose report reaches the proxy once
// the set is staged is. HTTP (replay's matcher and record's full round) and
// Generic alike. A miss of a parser that marks no round is told from the
// view serving then (TestAMissMadeWhileASetIsStagedSaysSo).
func TestAMissIsToldOfAStagingOnlyWhereItsCallWasDecided(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	opening := func(mm *MockManager) {
		mm.StageExactBackground(models.RecordedWithS12Routing)
		mm.ServeExactPool(models.PoolKeyNone, nil)
	}
	settling := func(mm *MockManager) {
		mm.ServeExactBackground(models.RecordedWithS12Routing, nil, nil)
		mm.ServeExactPool(models.PoolKeyNone, nil)
	}
	settled := func(mm *MockManager) { opening(mm); settling(mm) }
	serve := map[string]func(t *testing.T, db integrations.MockMemDb) *models.MockMismatchReport{
		"HTTP replay": func(t *testing.T, db integrations.MockMemDb) *models.MockMismatchReport {
			return sendForMiss(t, db, models.OutgoingOptions{}, learn.NewTracker(), jobsCall)
		},
		"HTTP record's full round": func(t *testing.T, db integrations.MockMemDb) *models.MockMismatchReport {
			return sendForMiss(t, db, models.OutgoingOptions{Learn: true, LearnPool: true}, learn.NewTracker(), jobsCall)
		},
		"Generic": func(t *testing.T, db integrations.MockMemDb) *models.MockMismatchReport {
			return genericServeIn(t, context.Background(), db, "AUTH step4")
		},
	}
	for _, tc := range []struct {
		name          string
		before, under func(mm *MockManager)
		told          bool
	}{
		{"decided under a staged set, reported in the next staging", settled, opening, false},
		{"decided in a staging, reported once the set is staged", opening, settling, true},
	} {
		for parser, send := range serve {
			t.Run(parser+": "+tc.name, func(t *testing.T) {
				p := &Proxy{logger: zap.NewNop(), errChannel: make(chan error, 4)}
				mm := NewMockManager(nil, nil, zap.NewNop())
				t.Cleanup(mm.Close)
				p.setMockManager(mm)
				tc.before(mm)
				if mm.exactMissStaging() != tc.told {
					t.Fatalf("precondition: the call's view reads as a staging's: %v", !tc.told)
				}
				db := &heldFirstRead{MockManager: mm, moved: func() { tc.under(mm) }}
				r := send(t, db)
				if r == nil {
					t.Fatal("the call was answered")
				}
				if !db.fired.Load() {
					t.Fatal("precondition: the replay never moved under the call")
				}
				if mm.exactMissStaging() == tc.told {
					t.Fatalf("precondition: the view serving as the miss is reported reads as a staging's: %v", tc.told)
				}
				p.BeginTestErrorCapture()
				p.sendMockNotFoundError(models.NewMockMismatchError(models.ErrNoMockMatched, r))
				got, err := p.GetMockErrors(context.Background())
				if err != nil || len(got) != 1 {
					t.Fatalf("want the one call, got %+v (%v)", got, err)
				}
				if told := strings.HasPrefix(got[0].NextSteps, stagingMissNote); told != tc.told {
					t.Fatalf("WRONG REASON: the call's next steps %q tell the staging %v; want %v", got[0].NextSteps, told, tc.told)
				}
			})
		}
	}
}

// A miss is filed where its call was made, in its test or outside every
// test, as the call's first read of the round found it (its parser marked
// it: integrations.StampMark), never from the view serving when the miss
// reaches the proxy: a call made in a test whose miss is reported once the
// test ended is that test's miss, and one made between tests whose miss is
// reported once the next test opened is no miss of that test. HTTP
// (replay's matcher and record's full round) and Generic alike. A miss of a
// parser that marks no round is filed from the view serving then
// (TestAMissIsFiledByWhereItWasMade).
func TestAMissIsFiledWhereItsCallWasMade(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	staged := func(mm *MockManager) {
		mm.StageExactBackground(models.RecordedWithS12Routing)
		mm.ServeExactPool(models.PoolKeyNone, nil)
		mm.ServeExactBackground(models.RecordedWithS12Routing, nil, nil)
		mm.ServeExactPool(models.PoolKeyNone, nil)
	}
	inTest := func(mm *MockManager) { mm.ServeExactTest("T1", nil) }
	between := func(mm *MockManager) { mm.ServeExactPool(models.PoolKeyNone, nil) }
	serve := map[string]func(t *testing.T, db integrations.MockMemDb) *models.MockMismatchReport{
		"HTTP replay": func(t *testing.T, db integrations.MockMemDb) *models.MockMismatchReport {
			return sendForMiss(t, db, models.OutgoingOptions{}, learn.NewTracker(), jobsCall)
		},
		"HTTP record's full round": func(t *testing.T, db integrations.MockMemDb) *models.MockMismatchReport {
			return sendForMiss(t, db, models.OutgoingOptions{Learn: true, LearnPool: true}, learn.NewTracker(), jobsCall)
		},
		"Generic": func(t *testing.T, db integrations.MockMemDb) *models.MockMismatchReport {
			return genericServeIn(t, context.Background(), db, "AUTH step4")
		},
	}
	for _, tc := range []struct {
		name          string
		before, under func(mm *MockManager)
		want          string
	}{
		{"made in a test, reported once the test ended", inTest, between, models.MissInTest},
		{"made between tests, reported once the next test opened", between, inTest, models.MissOutsideTest},
	} {
		for parser, send := range serve {
			t.Run(parser+": "+tc.name, func(t *testing.T) {
				p := &Proxy{logger: zap.NewNop(), errChannel: make(chan error, 4)}
				mm := NewMockManager(nil, nil, zap.NewNop())
				t.Cleanup(mm.Close)
				p.setMockManager(mm)
				staged(mm)
				tc.before(mm)
				if got := mm.exactMissKind(nil); got != tc.want {
					t.Fatalf("precondition: the call's view files a miss %q; want %q", got, tc.want)
				}
				db := &heldFirstRead{MockManager: mm, moved: func() { tc.under(mm) }}
				r := send(t, db)
				if r == nil {
					t.Fatal("the call was answered")
				}
				if !db.fired.Load() {
					t.Fatal("precondition: the replay never moved under the call")
				}
				if got := mm.exactMissKind(nil); got == tc.want {
					t.Fatalf("precondition: the view serving as the miss is reported files it %q too", got)
				}
				p.BeginTestErrorCapture()
				p.sendMockNotFoundError(models.NewMockMismatchError(models.ErrNoMockMatched, r))
				got, err := p.GetMockErrors(context.Background())
				if err != nil || len(got) != 1 {
					t.Fatalf("want the one call, got %+v (%v)", got, err)
				}
				if got[0].Kind != tc.want {
					t.Fatalf("WRONG KIND: the miss is filed %q; want %q, where its call was made", got[0].Kind, tc.want)
				}
			})
		}
	}
}

// Whether a call's set was served by proven routing is read from the view
// the call's first read found (integrations.S12BackgroundIn), through the
// store's mark, a round snapshot and a scoped view alike, never from the
// view serving when a parser reports the call's miss: a call of a set
// served by proven routing whose miss is reported once the replay staged a
// set recorded without it is one of a set whose varying fields record's full
// round learned, and the reverse. Without a mark (the zero CallRound), the
// store's now.
func TestACallsProvenRoutingIsReadFromTheViewItsFirstReadFound(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	stage := func(rules string) func(mm *MockManager) {
		return func(mm *MockManager) {
			mm.StageExactBackground(rules)
			mm.ServeExactPool(models.PoolKeyNone, nil)
			mm.ServeExactBackground(rules, nil, nil)
			mm.ServeExactPool(models.PoolKeyNone, nil)
		}
	}
	marks := map[string]func(mm *MockManager) integrations.CallRound{
		"the store's mark": func(mm *MockManager) integrations.CallRound { return mm.MarkCallRound() },
		"a scoped view's mark": func(mm *MockManager) integrations.CallRound {
			return integrations.MarkCallRound(&scopedMockDb{MockMemDb: mm})
		},
		"a round snapshot": func(mm *MockManager) integrations.CallRound {
			snap, ok := mm.RoundSnapshot(integrations.RoundQuery{})
			if !ok {
				t.Fatal("no snapshot")
			}
			return snap.CallRound()
		},
	}
	for _, tc := range []struct {
		name          string
		before, under func(mm *MockManager)
		want          bool
	}{
		{"marked in a set served by proven routing, read once one recorded without it is staged", stage(models.RecordedWithS12Routing), stage(""), true},
		{"marked in a set recorded without proven routing, read once one served by it is staged", stage(""), stage(models.RecordedWithS12Routing), false},
	} {
		for how, mark := range marks {
			t.Run(how+": "+tc.name, func(t *testing.T) {
				mm := NewMockManager(nil, nil, zap.NewNop())
				t.Cleanup(mm.Close)
				tc.before(mm)
				call := mark(mm)
				if !call.Read() {
					t.Fatal("precondition: the call was not marked")
				}
				tc.under(mm)
				if integrations.S12Background(mm) == tc.want {
					t.Fatalf("precondition: the view serving now reads %v too", tc.want)
				}
				if got := integrations.S12BackgroundIn(call, mm); got != tc.want {
					t.Fatalf("WRONG SET: the call's set reads as served by proven routing: %v; want %v, as its first read found it", got, tc.want)
				}
				if got := integrations.S12BackgroundIn(integrations.CallRound{}, mm); got != integrations.S12Background(mm) {
					t.Fatalf("an unmarked call reads %v; want the store's now", got)
				}
			})
		}
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
// test's own. In a test's window it answers at most one request in the
// staging's windows, through whichever door a parser consumes it: a consume
// in a test spends it for the next test too. Outside every test it is served
// by the pool slice's rule, whatever a window spent: a delete door (this
// parser's) answers once in the staging and spends it for every view after.
// One the run has not reached the place of answers nothing, in a test or
// out of one, until a later ServeExactWorker names it.
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
	// answers the gap after once, as the pool slice's recording a delete
	// door consumes does, and is then spent for the staging: the gap's
	// second call and the next test find none.
	mm.ServeExactWorker([]string{"w-boot", "w-later"})
	mm.ServeExactPool("k", []string{"pool"})
	if got := pollFirst(t, mm, "w-boot"); got != "w-boot" {
		t.Fatalf("a worker mock spent in a test did not answer the gap after by the pool's rule: %s", got)
	}
	if got := pollFirst(t, mm, "w-boot"); got != "a miss" {
		t.Fatalf("a delete door's worker mock answered twice outside every test: %s", got)
	}
	mm.ServeExactWorker([]string{"w-boot", "w-later"})
	mm.ServeExactTest("T2", nil)
	if got := pollFirst(t, mm, "w-boot"); got != "a miss" {
		t.Fatalf("a worker mock spent for the staging between tests answered in T2: %s", got)
	}
	if got := pollFirst(t, mm, "w-later"); got != "w-later" {
		t.Fatalf("a worker mock placed after T1 did not answer in T2: %s", got)
	}
	if got := pollFirst(t, mm, "w-later"); got != "a miss" {
		t.Fatalf("a worker mock answered twice in a test: %s", got)
	}
	mm.ServeExactTest("T3", nil)
	if got := pollFirst(t, mm, "w-later"); got != "a miss" {
		t.Fatalf("a worker mock spent in T2's window answered in T3's: %s", got)
	}
	mm.ServeExactPool("k", []string{"pool"})
	if got := pollFirst(t, mm, "w-later"); got != "w-later" {
		t.Fatalf("a worker mock spent in a test did not answer outside every test: %s", got)
	}
	// The next staging answers afresh.
	mm.SetMocksWithWindow(nil, []*models.Mock{own, boot, later, pool}, models.BaseTime, time.Now())
	mm.ServeExactWorker([]string{"w-boot"})
	mm.ServeExactPool("k", nil)
	if got := pollFirst(t, mm, "w-boot"); got != "w-boot" {
		t.Fatalf("the next staging's worker mock did not answer: %s", got)
	}
}

// A worker-scope recording a parser serves through a door that keeps it
// (HTTP's MarkMockAsUsed of a reusable or cursor recording) is served
// between tests as the pool slice's is: again and again, whatever a test's
// window spent. In a window it answers once in the staging's windows. A
// poller that calls twice as often between tests when replay is slower is
// answered every time, as before its recording was listed in worker:.
func TestAWorkerRecordingAKeepingDoorServesAnswersEveryCallBetweenTests(t *testing.T) {
	poll := httpCall("w-poll", time.Second, "/poll")
	mm := stagedManager(t, nil, []*models.Mock{poll})
	mm.ServeExactWorker([]string{"w-poll"})
	mm.ServeExactPool("k", []string{"w-poll"})
	for i := 0; i < 4; i++ {
		if got := markFirst(t, mm, "w-poll"); got != "w-poll" {
			t.Fatalf("between tests, call %d of the poller: %s; want w-poll, as the pool answers it", i+1, got)
		}
	}
	mm.ServeExactTest("T1", nil)
	if got := markFirst(t, mm, "w-poll"); got != "w-poll" {
		t.Fatalf("the worker recording did not answer in a test's window: %s", got)
	}
	if got := markFirst(t, mm, "w-poll"); got != "a miss" {
		t.Fatalf("the worker recording answered twice in a test's window: %s", got)
	}
	mm.ServeExactPool("k", []string{"w-poll"})
	for i := 0; i < 4; i++ {
		if got := markFirst(t, mm, "w-poll"); got != "w-poll" {
			t.Fatalf("after a window spent it, call %d between tests: %s; want w-poll", i+1, got)
		}
	}
	mm.ServeExactTest("T2", nil)
	if got := markFirst(t, mm, "w-poll"); got != "a miss" {
		t.Fatalf("the worker recording answered in a second window of the staging: %s", got)
	}
}

// markFirst answers a call as a parser that takes the first recording in
// read order and serves it through a door that keeps it (MarkMockAsUsed, as
// HTTP does a reusable recording); "a miss" when none is read or the consume
// is refused.
func markFirst(t *testing.T, mm *MockManager, prefix string) string {
	t.Helper()
	mocks, _ := mm.GetSessionScopedMocks()
	for _, mk := range mocks {
		if strings.HasPrefix(mk.Name, prefix) {
			if mm.MarkMockAsUsed(*mk) {
				return mk.Name
			}
			return "a miss"
		}
	}
	return "a miss"
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
	// Recorded at the instant itself: a fixture's whole second may have made
	// it before the call, so it is visited (the learn step reads it as an
	// earlier place unless F served it).
	same := exactMock("pool-5", 5*time.Second, models.LifetimeSession)
	mm.SetMocksWithWindow([]*models.Mock{own}, []*models.Mock{pool, later, same}, models.BaseTime, time.Now())
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
	if names, _, err := visit(mm); err != nil || names != "own-2,pool-1,pool-5" {
		t.Fatalf("with no disk: %s %v; want own-2,pool-1,pool-5", names, err)
	}
	onDisk := exactMock("other-test-3", 3*time.Second, models.LifetimePerTest)
	ownCopy := exactMock("own-2", 2*time.Second, models.LifetimePerTest)
	onDiskSame := exactMock("other-test-5", 5*time.Second, models.LifetimePerTest)
	onDiskLater := exactMock("other-test-6", 6*time.Second, models.LifetimePerTest)
	mm.SetRecordedBefore(func(t2 time.Time, v func(*models.Mock) bool) error {
		if !t2.Equal(at) {
			t.Fatalf("the disk read before %v", t2)
		}
		for _, m := range []*models.Mock{ownCopy, onDisk, onDiskSame, onDiskLater} {
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
	if err != nil || names != "other-test-3,other-test-5,own-2,pool-1,pool-5" || !treeCopy {
		t.Fatalf("with the disk: %s %v (own-2 the tree's: %v); want other-test-3, other-test-5, own-2 (the tree's), pool-1, pool-5 (not other-test-6, a source that hands more)", names, err, treeCopy)
	}
	if names, _, err := visit(&scopedMockDb{MockMemDb: mm}); err != nil || names != "other-test-3,other-test-5,own-2,pool-1,pool-5" {
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

// A call marked where keploy's store no longer shows the view of the test
// its learn tracker still has open (the store's pool view after that test's
// end, or the next test's view after it), may be that test's late call: its
// mark (MarkCallRound, which the Generic, MySQL and Postgres parsers read)
// holds that test's recordings as its own, as a round snapshot's call round
// does, so no lookup names one as made elsewhere, and record's judging
// never bakes a test's own recording into the worker scope. The Generic
// parser's miss there names none. Once the tracker ends the test, a call
// marked then names it.
func TestACallMarkedWhereTheStoreEndedATestTheTrackerHasOpenNamesNoneOfItsOwn(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	for _, tc := range []struct {
		name  string
		after func(mm *MockManager, next *models.Mock)
	}{
		{"the store's pool view", func(mm *MockManager, next *models.Mock) { mm.ServeExactPool(models.PoolKeyNone, nil) }},
		{"the next test's view", func(mm *MockManager, next *models.Mock) {
			mm.ServeExactPool(models.PoolKeyNone, nil)
			mm.ServeExactTest("T2", []*models.Mock{next})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			own := genericExchange("ex-own", 0, "AUTH step2")
			next := genericExchange("ex-next", time.Second, "PING")
			mm := stagedManager(t, nil, []*models.Mock{own, next})
			tr := learn.NewTracker()
			mm.LearnBoundary(tr, func(t *learn.Tracker) { t.Begin() })
			mm.ServeExactTest("T1", []*models.Mock{own})
			tc.after(mm, next)
			find := func(call integrations.CallRound) string {
				name, _ := mm.FindStagedNotOwned(models.GENERIC, call, func(m *models.Mock) bool { return m.Name == own.Name })
				return name
			}
			if got := find(mm.MarkCallRound()); got != "" {
				t.Fatalf("a call marked with T1 ended on the store alone named T1's own %q as made elsewhere", got)
			}
			if snap, ok := mm.RoundSnapshot(integrations.RoundQuery{Tracker: tr}); !ok || snap.Ended != "T1" {
				t.Fatalf("precondition: the round reads %v (%s), ended %q; want T1 ended on the store alone", snap.Phase, snap.Boundary, snap.Ended)
			} else if got := find(snap.CallRound()); got != "" {
				t.Fatalf("a snapshot's call round with T1 ended on the store alone named T1's own %q as made elsewhere", got)
			}
			if r := genericMiss(t, mm, "AUTH step2"); r.NotOwned || r.ClosestMock == own.Name {
				t.Fatalf("a Generic request with T1 ended on the store alone was diagnosed as T1's own recording made elsewhere: %+v", r)
			}
			mm.LearnBoundary(tr, func(t *learn.Tracker) { t.End() })
			if got := find(mm.MarkCallRound()); got != own.Name {
				t.Fatalf("a call marked once the tracker ended T1 too named %q; want T1's own %s, made elsewhere", got, own.Name)
			}
		})
	}
}

// A call marked where keploy's learn tracker began a test whose view the
// store has not installed (integrations.RoundSnapshot.TrackerAhead) is the
// pool's on the store's pool view after the test before
// (integrations.RoundSnapshot.PoolsCall) and the tracker's test's on that
// test's view still in force, and in neither does anything tell which
// recordings are the tracker's test's own: its mark (MarkCallRound, which
// the Generic, MySQL and Postgres parsers
// read) holds every recording, as a round snapshot's call round does, so no
// lookup names the test's own recording as made elsewhere, and the Generic
// parser's miss there names none. Once the store opens the test, a call
// marked then names the test before's recording.
func TestACallMarkedWhereTheTrackerIsAheadOfTheStoreNamesNoRecordingMadeElsewhere(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	for _, tc := range []struct {
		name  string
		after func(mm *MockManager)
	}{
		{"the store's pool view after T1", func(mm *MockManager) { mm.ServeExactPool(models.PoolKeyNone, nil) }},
		{"the store still showing T1's view", func(mm *MockManager) {}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := genericExchange("ex-before", 0, "AUTH step2")
			own := genericExchange("ex-own", time.Second, "PING")
			mm := stagedManager(t, nil, []*models.Mock{before, own})
			tr := learn.NewTracker()
			mm.LearnBoundary(tr, func(t *learn.Tracker) { t.Begin() })
			mm.ServeExactTest("T1", []*models.Mock{before})
			mm.LearnBoundary(tr, func(t *learn.Tracker) { t.End() })
			tc.after(mm)
			mm.LearnBoundary(tr, func(t *learn.Tracker) { t.Begin() })
			find := func(call integrations.CallRound, name string) string {
				got, _ := mm.FindStagedNotOwned(models.GENERIC, call, func(m *models.Mock) bool { return m.Name == name })
				return got
			}
			snap, ok := mm.RoundSnapshot(integrations.RoundQuery{Tracker: tr})
			if !ok || !snap.TrackerAhead() {
				t.Fatalf("precondition: the round reads %v (%s), ended %q; want the tracker ahead of the store", snap.Phase, snap.Boundary, snap.Ended)
			}
			if got := find(mm.MarkCallRound(), own.Name); got != "" {
				t.Fatalf("a call marked with the tracker ahead of the store named %q as made elsewhere; want none", got)
			}
			if got := find(snap.CallRound(), own.Name); got != "" {
				t.Fatalf("a snapshot's call round with the tracker ahead of the store named %q as made elsewhere; want none", got)
			}
			if r := genericMiss(t, mm, "PING"); r.NotOwned || r.ClosestMock == own.Name {
				t.Fatalf("a Generic request with the tracker ahead of the store was diagnosed as a recording made elsewhere: %+v", r)
			}
			mm.ServeExactTest("T2", []*models.Mock{own})
			if got := find(mm.MarkCallRound(), before.Name); got != before.Name {
				t.Fatalf("a call marked once the store opened T2 named %q; want T1's %s, made elsewhere", got, before.Name)
			}
		})
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

// A call a parser marks (MarkCallRound) before its learn step's first read
// of the round carries the round's boundary count, so that first read tells
// whether the round moved between the two (integrations.CallRound
// .MovedSince): a test's end (the store's pool view, the learn tracker's
// end), or that end and the next test's begin, landing between them moved
// it, and so does a staging; the tracker's end alone moves it where the
// tracker's boundaries reach the store. Where nothing moved, or only a
// consume of the test's own, or a test process claimed its key again (the
// generation moves, no boundary does), the first read is of the mark's
// round. The staging epoch, all a mark carried before, tells none of a
// test's boundaries: the learn step decided such a call in the round its
// first read showed, the pool's or the next test's, its mark holding the
// ended test's recordings as its own, and learned what the call differs in
// onto the pool view's in-flight recording of the next test as noise. A
// mark read through a connection's view, and a round snapshot's call round,
// carry the count too. A boundary that lands as the mark lets the round
// lock go is after the mark, which then reads the view and the count of
// before it; and a mark in a test set the store has not staged carries the
// count as every other does.
func TestACallMarkedBeforeItsTestMovesIsMovedAtItsFirstReadOfTheRound(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	type land func(t *testing.T, mm *MockManager, step func(func(*learn.Tracker)), own, ahead, next *models.Mock)
	end := func(t *testing.T, mm *MockManager, step func(func(*learn.Tracker)), _, ahead, _ *models.Mock) {
		mm.ServeExactPool(models.PoolKeyNone, nil, ahead)
		step(func(t *learn.Tracker) { t.End() })
	}
	for _, routed := range []bool{false, true} {
		for _, tc := range []struct {
			name  string
			moved bool
			land  land
			// asMarked lands the move in the mark itself, as it lets the
			// round lock go (MockManager.exact.onMarked).
			asMarked bool
		}{
			{name: "nothing (control)", land: func(*testing.T, *MockManager, func(func(*learn.Tracker)), *models.Mock, *models.Mock, *models.Mock) {}},
			{name: "a consume of the test's own", land: func(t *testing.T, mm *MockManager, _ func(func(*learn.Tracker)), own, _, _ *models.Mock) {
				if !mm.DeleteFilteredMock(*own) {
					t.Fatal("precondition: T1's own recording was not consumed")
				}
			}},
			{name: "a test process's claim of its key again", land: func(t *testing.T, mm *MockManager, _ func(func(*learn.Tracker)), _, ahead, _ *models.Mock) {
				mm.RestartExactKey(models.PoolKeyNone, []string{ahead.Name})
			}},
			{name: "the tracker's end alone", moved: routed, land: func(t *testing.T, _ *MockManager, step func(func(*learn.Tracker)), _, _, _ *models.Mock) {
				step(func(t *learn.Tracker) { t.End() })
			}},
			{name: "the test's end", moved: true, land: end},
			{name: "the test's end, as the mark lets the round lock go", moved: true, land: end, asMarked: true},
			{name: "the test's end and the next test's begin", moved: true, land: func(t *testing.T, mm *MockManager, step func(func(*learn.Tracker)), own, ahead, next *models.Mock) {
				end(t, mm, step, own, ahead, next)
				step(func(t *learn.Tracker) { t.Begin() })
				mm.ServeExactTest("T2", []*models.Mock{ahead, next})
			}},
			{name: "a staging of the set again", moved: true, land: func(t *testing.T, mm *MockManager, _ func(func(*learn.Tracker)), own, ahead, next *models.Mock) {
				mm.SetMocksWithWindow(nil, []*models.Mock{own, ahead, next}, models.BaseTime, time.Now())
			}},
		} {
			t.Run(fmt.Sprintf("%s, routed=%v", tc.name, routed), func(t *testing.T) {
				own := genericExchange("ex-own", 0, "AUTH step2")
				ahead := genericExchange("ex-ahead", time.Second, "PING")
				next := genericExchange("ex-next", 2*time.Second, "PING")
				mm := stagedManager(t, nil, []*models.Mock{own, ahead, next})
				tr := learn.NewTracker()
				step := func(f func(*learn.Tracker)) { f(tr) }
				if routed {
					step = func(f func(*learn.Tracker)) { mm.LearnBoundary(tr, f) }
				}
				step(func(t *learn.Tracker) { t.Begin() })
				mm.ServeExactTest("T1", []*models.Mock{own})
				at, ok := mm.RoundSnapshot(integrations.RoundQuery{Tracker: tr})
				if !ok || at.Test != "T1" || at.Phase != integrations.RoundInTest && routed {
					t.Fatalf("precondition: the round reads %q's view (%v, %v); want T1's", at.Test, at.Phase, ok)
				}
				if tc.asMarked {
					mm.exact.onMarked = func() {
						mm.exact.onMarked = nil
						tc.land(t, mm, step, own, ahead, next)
					}
				}
				marks := map[string]integrations.CallRound{"the store's mark": mm.MarkCallRound()}
				if !tc.asMarked {
					marks["a scoped view's mark"] = integrations.MarkCallRound(&scopedMockDb{MockMemDb: mm})
					marks["a round snapshot's own"] = at.CallRound()
					for name, mark := range marks {
						if got, known := mark.Round(); !known || got.Gen != at.At.Gen || got.Bounds != at.At.Bounds || got.Epoch != at.At.Epoch {
							t.Fatalf("%s reads the round at %+v (known %v); want the round's %+v", name, got, known, at.At)
						}
					}
					tc.land(t, mm, step, own, ahead, next)
				}
				first, ok := mm.RoundSnapshot(integrations.RoundQuery{Tracker: tr})
				if !ok {
					t.Fatal("no first read of the round")
				}
				for name, mark := range marks {
					if got := mark.MovedSince(first.At); got != tc.moved {
						r, _ := mark.Round()
						t.Errorf("%s of T1 read again after %s: moved %v, want %v (mark at %+v, first read at %+v)", name, tc.name, got, tc.moved, r, first.At)
					}
				}
				if first.CallRound().MovedSince(first.At) {
					t.Error("a round snapshot's call round is moved by its own read")
				}
			})
		}
	}
	t.Run("a mark between tests, the tracker's next begin", func(t *testing.T) {
		own := genericExchange("ex-own", 0, "AUTH step2")
		mm := stagedManager(t, nil, []*models.Mock{own})
		tr := learn.NewTracker()
		mm.LearnBoundary(tr, func(t *learn.Tracker) { t.Begin() })
		mm.ServeExactTest("T1", []*models.Mock{own})
		mm.LearnBoundary(tr, func(t *learn.Tracker) { t.End() })
		mm.ServeExactPool(models.PoolKeyNone, nil)
		at, ok := mm.RoundSnapshot(integrations.RoundQuery{Tracker: tr})
		if !ok || at.Phase != integrations.RoundBetween {
			t.Fatalf("precondition: the round reads %v (%s); want between tests", at.Phase, at.Boundary)
		}
		mark := mm.MarkCallRound()
		mm.LearnBoundary(tr, func(t *learn.Tracker) { t.Begin() })
		first, _ := mm.RoundSnapshot(integrations.RoundQuery{Tracker: tr})
		if !first.PoolsCall() || !mark.MovedSince(first.At) {
			r, _ := mark.Round()
			t.Fatalf("a call marked between tests, read once the tracker began the next test (pool's call %v), is not moved (mark at %+v, read at %+v)", first.PoolsCall(), r, first.At)
		}
	})
	t.Run("a mark in a test set the store has not staged", func(t *testing.T) {
		own := genericExchange("ex-own", 0, "AUTH step2")
		mm := stagedManager(t, nil, []*models.Mock{own})
		tr := learn.NewTracker()
		mm.LearnBoundary(tr, func(t *learn.Tracker) { t.Begin() })
		mm.ServeExactTest("T1", []*models.Mock{own})
		mm.LearnBoundary(tr, func(t *learn.Tracker) { t.End() })
		mm.ServeExactPool(models.PoolKeyNone, nil)
		mm.LearnBoundary(tr, func(t *learn.Tracker) { t.NewSet() })
		at, ok := mm.RoundSnapshot(integrations.RoundQuery{Tracker: tr})
		if !ok || !at.SetUnstaged() {
			t.Fatalf("precondition: the round reads %v (%s); want a test set the store has not staged", at.Phase, at.Boundary)
		}
		mark := mm.MarkCallRound()
		if !mark.Staging() || mark.InTest() {
			t.Fatalf("precondition: the mark is not a staging's outside every test: staging %v, in a test %v", mark.Staging(), mark.InTest())
		}
		if mark.MovedSince(at.At) {
			r, _ := mark.Round()
			t.Fatalf("a call marked in a test set the store has not staged is moved where nothing moved (mark at %+v, read at %+v)", r, at.At)
		}
		mm.SetMocksWithWindow(nil, []*models.Mock{own}, models.BaseTime, time.Now())
		staged, _ := mm.RoundSnapshot(integrations.RoundQuery{Tracker: tr})
		if !mark.MovedSince(staged.At) {
			t.Fatal("a call marked in a test set the store had not staged is not moved by its staging")
		}
	})
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
	return genericServeIn(t, context.Background(), mm, req)
}

// genericServeIn is genericServe with the parser's ctx made from ctx.
func genericServeIn(t *testing.T, ctx context.Context, mm integrations.MockMemDb, req string) *models.MockMismatchReport {
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
	ctx = context.WithValue(ctx, models.ClientConnectionIDKey, "1")
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

// A Generic request held after its first read of the round while the
// replay stages the next set, whose open test has already answered an
// exchange, is never told as one exchange more than that test recorded
// (extraCall), nor diagnosed against that exchange (answeredExchanges):
// both read the store held to the request's first read, which shows nothing
// of the next set's test once the staging moved. The next set's own request
// is told and diagnosed against it.
func TestAGenericRequestHeldWhileTheNextSetIsStagedIsNoExtraCallOfItsTest(t *testing.T) {
	for _, tc := range []struct{ name, answered, req string }{
		{"the exchange it is", "AUTH step4", "AUTH step4"},
		{"an exchange it resembles", "AUTH step4 user=b", "AUTH step4 user=a"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mine := genericExchange("ex-mine", 0, "PING")
			next := genericExchange("s2-ex", 2*time.Second, tc.answered)
			mm := stagedManager(t, nil, []*models.Mock{mine})
			mm.ServeExactTest("T1", []*models.Mock{mine})
			answered := false
			mm.exact.onMarked = func() {
				mm.exact.onMarked = nil
				mm.SetMocksWithWindow(nil, []*models.Mock{next}, models.BaseTime, time.Now())
				mm.ServeExactTest("S2-T1", []*models.Mock{next})
				// S2-T1's own request, answered with its exchange.
				mocks, _ := mm.GetPerTestMocksInWindow()
				session, _ := mm.GetSessionMocks()
				for _, mk := range append(mocks, session...) {
					if mk.Name == next.Name && !answered {
						c := *mk
						answered = mm.DeleteFilteredMock(c) || mm.UpdateUnFilteredMock(mk, &c)
					}
				}
			}
			r := genericServe(t, mm, tc.req)
			if !answered {
				t.Fatal("precondition: the replay staged the next set under the request and its test answered its own")
			}
			if r == nil {
				t.Fatalf("a request of the set before was answered with the next set's %s", next.Name)
			}
			if r.MissKind == models.MissExtraCall {
				t.Fatalf("a request of the set before was told as one exchange more than the next set's test recorded: %+v", r)
			}
			if r.NotOwned || r.ClosestMock == next.Name {
				t.Fatalf("a request of the set before was diagnosed against the next set's %s: %+v", next.Name, r)
			}

			// The next set's own request: told, or diagnosed, against it.
			r = genericServe(t, mm, tc.req)
			if r == nil || r.ClosestMock != next.Name && r.MissKind != models.MissExtraCall {
				t.Fatalf("the next set's own request is %+v; want it told against %s", r, next.Name)
			}
		})
	}
}

// aloftHolder is a connection with one request aloft (learn.Flight): the
// parser's ctx carries it as the proxy's kept connection does.
type aloftHolder struct{ f *learn.Flight }

func (aloftHolder) Hold() func()              { return func() {} }
func (a aloftHolder) InFlight() *learn.Flight { return a.f }

// The Generic parser's consume says whose request it serves
// (integrations.ServingIn): the mock manager tells the learn step the serve
// is the request's connection's, so a request aloft on a parallel
// connection that reads no recording (a health check) never makes the serve
// one keploy cannot place. From the session tier and from an open test's own
// exchanges alike.
func TestAGenericServeIsToldInItsRequest(t *testing.T) {
	for _, exact := range []bool{false, true} {
		t.Run("exact serving "+strconv.FormatBool(exact), func(t *testing.T) {
			ex := genericExchange("ex", 0, "AUTH step2")
			mm := stagedManager(t, nil, []*models.Mock{ex})
			if exact {
				mm.ServeExactTest("T1", []*models.Mock{ex})
			}
			var told []*learn.Flight
			mm.SetServedObserver(func(m *models.Mock) {
				if m.Name == ex.Name {
					told = append(told, nil)
				}
			})
			mm.SetServedInObserver(func(m *models.Mock, f *learn.Flight) {
				if m.Name == ex.Name {
					told = append(told, f)
				}
			})
			in := learn.NewTracker().Lift()
			if r := genericServeIn(t, integrations.WithRequestHolder(context.Background(), aloftHolder{in}), mm, "AUTH step2"); r != nil {
				t.Fatalf("the request missed: %+v", r)
			}
			if len(told) != 1 || told[0] != in {
				t.Fatalf("the serve of %s was told %d time(s), in %v; want once, in the request's", ex.Name, len(told), told)
			}
		})
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

// The cost of a stateful HTTP call under exact serving: each reads every
// mock the store stages (stagedGroup) and the strict pass diffs the
// recordings of the call's method. This row measures it with 1,000 other
// stateful recordings staged against none, and checks the answers are the
// same at both sizes. It times, never bounds: load varies, so the number it
// logs (go test -v -run TestAStatefulCallsCostAtAThousandStagedMocks) is for
// the PR text, not an assertion.
func TestAStatefulCallsCostAtAThousandStagedMocks(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "1")
	const rounds = 15
	measure := func(others int) (time.Duration, []string) {
		var per []time.Duration
		var answers []string
		for r := 0; r < rounds; r++ {
			staged := []*models.Mock{seqCall("w0", time.Second, "0"), seqCall("w1", 2*time.Second, "1"), seqCall("w2", 3*time.Second, "2")}
			for i := 0; i < others; i++ {
				m := seqCall(fmt.Sprintf("o%04d", i), 4*time.Second+time.Duration(i)*time.Millisecond, "o")
				m.Spec.HTTPReq.URL = fmt.Sprintf("http://deps/other/%d", i)
				staged = append(staged, m)
			}
			mm := stagedManager(t, nil, staged)
			mm.SeedRecordedWindows([]models.TestWindow{{TestCase: "T", Start: exactT0.Add(500 * time.Millisecond), End: exactT0.Add(10 * time.Second)}})
			mm.ServeExactWorker([]string{"w0", "w1", "w2"})
			mm.ServeExactPool("k", nil)
			mm.ServeExactTest("T", nil)
			var got []string
			var took time.Duration
			for i := 0; i < 4; i++ {
				body, d := timedServe(t, mm)
				if body == "" {
					got = append(got, "no answer")
					continue // a miss's report is not the walk's cost
				}
				took += d
				got = append(got, body)
			}
			per = append(per, took/3)
			answers = got
		}
		sort.Slice(per, func(i, j int) bool { return per[i] < per[j] })
		return per[len(per)/2], answers
	}
	want := []string{"0", "1", "2", "no answer"}
	few, fewGot := measure(0)
	many, manyGot := measure(1000)
	if !reflect.DeepEqual(fewGot, want) || !reflect.DeepEqual(manyGot, want) {
		t.Fatalf("answered %v with 3 staged and %v with 1,003; want %v at both", fewGot, manyGot, want)
	}
	t.Logf("an answered stateful call under exact serving (strict pass, median of %d): %v per call with 3 mocks staged, %v with 1,003", rounds, few, many)
}

// timedServe asks GET /probe through the real HTTP integration on one
// connection and returns the body answered ("" for none) and the time from
// the request's write to its answer's last byte: the serve, without the
// connection's teardown.
func timedServe(t *testing.T, mm integrations.MockMemDb) (string, time.Duration) {
	t.Helper()
	app, agent := net.Pipe()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	served := make(chan error, 1)
	go func() {
		served <- httpint.New(zap.NewNop()).MockOutgoing(ctx, agent, &models.ConditionalDstCfg{}, mm, models.OutgoingOptions{})
		agent.Close()
	}()
	defer func() { app.Close(); <-served }()
	start := time.Now()
	if _, err := app.Write([]byte("GET /probe HTTP/1.1\r\nHost: deps\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(app), nil)
	if err != nil || resp.StatusCode != http.StatusOK {
		return "", 0
	}
	body := make([]byte, resp.ContentLength)
	if _, err := io.ReadFull(resp.Body, body); err != nil {
		return "", 0
	}
	return string(body), time.Since(start)
}

// In the variable pass, with a request id bound per call (each call sends a
// new one), a test's call is answered the test's own recording, then the
// worker scope's (by placement: it belongs to no test), then nothing, never a
// spent recording again though its variable no longer holds for the next
// call. With one id, each call after the test's own is the call its own
// recording answered, one more than the test recorded
// (integrations.OwnRepeatRead): the worker scope never answers it, and its
// recording answers the call between tests.
func TestAVariableCallWithANewIdEachCallIsAnsweredItsOwnThenTheWorkerScope(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "1")
	rec := func(name string, at time.Duration, body string) *models.Mock {
		m := seqCall(name, at, body)
		m.Spec.HTTPReq.Header["X-Req-Id"] = "rec"
		m.Spec.Variables = models.MockVariables{"rid-" + m.Name: {Recorded: "rec", Bind: "header.X-Req-Id"}}
		return m
	}
	for _, c := range []struct {
		name string
		ids  []string
		want []string
		// between is what the call is answered once test A ends.
		between string
	}{
		{"a new id each call", []string{"r1", "r2", "r3"}, []string{"mine", "start", "no answer"}, "no answer"},
		{"one id", []string{"rec", "rec", "rec"}, []string{"mine", "no answer", "no answer"}, "start"},
	} {
		own := rec("a0", 3500*time.Millisecond, "mine")
		mm := stagedManager(t, nil, []*models.Mock{rec("s1", 2*time.Second, "start"), own})
		mm.SeedRecordedWindows([]models.TestWindow{{TestCase: "A", Start: exactT0.Add(3 * time.Second), End: exactT0.Add(5 * time.Second)}})
		mm.ServeExactWorker([]string{"s1"})
		mm.ServeExactPool("k", nil)
		mm.ServeExactWorker([]string{"s1"})
		mm.ServeExactTest("A", []*models.Mock{own})
		ask := func(id string) string {
			resp, body := learnServe(t, httpint.New(zap.NewNop()), mm, context.Background(), models.OutgoingOptions{}, "GET /probe HTTP/1.1\r\nHost: deps\r\nX-Req-Id: "+id+"\r\n\r\n")
			if resp == nil || resp.StatusCode != http.StatusOK {
				return "no answer"
			}
			return string(body)
		}
		var got []string
		for _, id := range c.ids {
			got = append(got, ask(id))
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Fatalf("%s: test A answered %v; want %v", c.name, got, c.want)
		}
		mm.ServeExactPool("k", nil)
		if got := ask("r9"); got != c.between {
			t.Fatalf("%s: between tests the call was answered %q; want %q", c.name, got, c.between)
		}
	}
}

// Between tests a variable request's sequence is fixed by every recording of
// it the store stages, the ones whose variable no longer holds for this call
// too (a member through the variable fit's problem, not its hit): two pool
// recordings that each bind their own request id answered r1 then r2, so a
// call past them is answered nothing, never the first again because its id
// came back. The same for two of the worker scope's.
func TestAVariableCallPastItsRecordingsIsAnsweredNothingBetweenTests(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "1")
	rec := func(name string, at time.Duration) *models.Mock {
		m := seqCall(name, at, name)
		m.Spec.HTTPReq.Header["X-Req-Id"] = "rec"
		m.Spec.Variables = models.MockVariables{"rid-" + name: {Recorded: "rec", Bind: "header.X-Req-Id"}}
		return m
	}
	for _, worker := range []bool{false, true} {
		mm := stagedManager(t, nil, []*models.Mock{rec("p0", time.Second), rec("p1", 2*time.Second)})
		if worker {
			mm.ServeExactWorker([]string{"p0", "p1"})
			mm.ServeExactPool("k", nil)
		} else {
			mm.ServeExactPool("k", []string{"p0", "p1"})
		}
		var got []string
		for _, id := range []string{"r1", "r2", "r1", "r1"} {
			resp, body := learnServe(t, httpint.New(zap.NewNop()), mm, context.Background(), models.OutgoingOptions{}, "GET /probe HTTP/1.1\r\nHost: deps\r\nX-Req-Id: "+id+"\r\n\r\n")
			if resp == nil || resp.StatusCode != http.StatusOK {
				got = append(got, "no answer")
				continue
			}
			got = append(got, string(body))
		}
		if want := []string{"p0", "p1", "no answer", "no answer"}; !reflect.DeepEqual(got, want) {
			t.Errorf("worker scope %v: ids r1 r2 r1 r1 answered %v; want %v: past its recordings, nothing", worker, got, want)
		}
	}
}

// A test window's answers never depend on how many calls landed between
// tests before it. The windows and the calls between tests read the worker
// scope apart, each on its own spends: a call between tests that spends a
// worker: recording through a delete door (this parser's, enterprise SQS's)
// spends it for the calls outside every test only, and a window's call
// spends it for the windows only. So a poller recorded three times, polled
// 0, 1, 3 or 10 times between tests as a slower or faster replay makes it,
// is answered the same in the next test's window every time: its three
// recordings once each, in placement order, then a miss. Through a door
// that keeps the recording (MarkMockAsUsed) the same holds.
func TestAWindowsWorkerAnswersDoNotMoveWithTheCallsBetweenTests(t *testing.T) {
	for _, door := range []struct {
		name string
		ask  func(*testing.T, *MockManager, string) string
	}{{"delete door", pollFirst}, {"keeping door", markFirst}} {
		t.Run(door.name, func(t *testing.T) {
			window := func(gap int) []string {
				polls := []*models.Mock{httpCall("w-0", time.Second, "/poll"), httpCall("w-1", 2*time.Second, "/poll"), httpCall("w-2", 3*time.Second, "/poll")}
				mm := stagedManager(t, nil, polls)
				names := []string{"w-0", "w-1", "w-2"}
				mm.ServeExactWorker(names)
				mm.ServeExactTest("TestA", nil)
				mm.ServeExactWorker(names)
				mm.ServeExactPool("k", nil)
				for i := 0; i < gap; i++ {
					door.ask(t, mm, "w-")
				}
				mm.ServeExactWorker(names)
				mm.ServeExactTest("TestB", nil)
				var got []string
				for i := 0; i < 4; i++ {
					got = append(got, door.ask(t, mm, "w-"))
				}
				return got
			}
			want := []string{"w-0", "w-1", "w-2", "a miss"}
			for _, gap := range []int{0, 1, 3, 10} {
				if got := window(gap); !reflect.DeepEqual(got, want) {
					t.Errorf("after %d calls between tests, TestB's window was answered %v; want %v whatever the gap made", gap, got, want)
				}
			}
		})
	}
}

// A worker-scope consume is spent for the view its call was read in, not the
// one in force as its commit lands. A held call (HoldToRound) settles in the
// view in force while its staging holds, which a begin or an end may have
// replaced since its read, more often the slower the replay. So a call read
// between tests that commits after the next test opened spends nothing a
// window reads, and a test's call that commits after its end still spends
// its recording for the windows: whether a commit lands before or after a
// begin or an end never moves what a window is answered. Both through a
// delete door (DeleteUnFilteredMock, enterprise SQS's) and through a door
// that keeps the recording (MarkMockAsUsed, HTTP's).
func TestAWorkerSpendFollowsTheViewTheCallWasReadIn(t *testing.T) {
	names := []string{"w-0", "w-1"}
	setup := func(t *testing.T) (*MockManager, models.Mock) {
		polls := []*models.Mock{httpCall("w-0", time.Second, "/poll"), httpCall("w-1", 2*time.Second, "/poll")}
		mm := stagedManager(t, nil, polls)
		mm.ServeExactWorker(names)
		return mm, *polls[0]
	}
	for _, door := range []struct {
		name    string
		consume func(integrations.MockMemDb, models.Mock) bool
		ask     func(*testing.T, *MockManager, string) string
		evicts  bool
	}{
		{"delete door", func(db integrations.MockMemDb, mk models.Mock) bool { return db.DeleteUnFilteredMock(mk) }, pollFirst, true},
		{"keeping door", func(db integrations.MockMemDb, mk models.Mock) bool { return db.MarkMockAsUsed(mk) }, markFirst, false},
	} {
		t.Run(door.name+"/read between tests, committed after the test opened", func(t *testing.T) {
			mm, w0 := setup(t)
			mm.ServeExactPool("k", nil)
			held, _ := mm.HoldToRound(mm.MarkCallRound())
			mm.ServeExactWorker(names)
			mm.ServeExactTest("TestA", nil)
			if ok := door.consume(held, w0); door.evicts && !ok {
				t.Fatal("a call read between tests could not spend w-0 for the calls between tests once the test opened")
			}
			var got []string
			for i := 0; i < 3; i++ {
				got = append(got, door.ask(t, mm, "w-"))
			}
			// Settled as TestA's window's, the gap call would spend w-0
			// for the windows and TestA would be answered w-1, then a
			// miss: its answers would move with when the commit landed.
			if want := []string{"w-0", "w-1", "a miss"}; !reflect.DeepEqual(got, want) {
				t.Errorf("TestA's window was answered %v after a call read between tests committed in it; want %v, as when the commit lands before the begin", got, want)
			}
		})
		t.Run(door.name+"/read in a test, committed after its end", func(t *testing.T) {
			mm, w0 := setup(t)
			mm.ServeExactTest("TestA", nil)
			held, _ := mm.HoldToRound(mm.MarkCallRound())
			mm.ServeExactWorker(names)
			mm.ServeExactPool("k", nil)
			if !door.consume(held, w0) {
				t.Fatal("TestA's call could not spend w-0 for the windows once its test ended")
			}
			mm.ServeExactWorker(names)
			mm.ServeExactTest("TestB", nil)
			var got []string
			for i := 0; i < 2; i++ {
				got = append(got, door.ask(t, mm, "w-"))
			}
			// Settled as one of the calls between tests, TestA's call
			// would leave w-0 unspent for the windows and TestB would be
			// answered it again: one recording, two windows' answers.
			if want := []string{"w-1", "a miss"}; !reflect.DeepEqual(got, want) {
				t.Errorf("TestB's window was answered %v after TestA's call committed past TestA's end; want %v: w-0 answered TestA's window, as when the commit lands before the end", got, want)
			}
		})
	}
}
