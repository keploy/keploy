package proxy

import (
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	httpint "go.keploy.io/server/v3/pkg/agent/proxy/integrations/http"
	"go.keploy.io/server/v3/pkg/agent/starts"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// counterMock is one recording of a stateful dependency (GET /counter), as
// DeriveLifetime classifies an untagged HTTP mock: a session mock served by the
// record-order cursor.
func counterMock(name string, at time.Time) *models.Mock {
	return &models.Mock{
		Version: "api.keploy.io/v1beta1",
		Name:    name,
		Kind:    models.Kind(models.HTTP),
		Spec: models.MockSpec{
			HTTPReq: &models.HTTPReq{Method: "GET", URL: "http://api/counter", ProtoMajor: 1, ProtoMinor: 1},
			// As recorded: the serializer recomputes a recorded Content-Length.
			HTTPResp:         &models.HTTPResp{StatusCode: 200, Body: name, Header: map[string]string{"Content-Length": "0"}},
			ReqTimestampMock: at,
			ResTimestampMock: at.Add(time.Millisecond),
		},
		TestModeInfo: models.TestModeInfo{
			Lifetime:        models.LifetimeSession,
			Consume:         models.ConsumeCursorSaturate,
			LifetimeDerived: true,
		},
	}
}

func counterPool(base time.Time, names ...string) []*models.Mock {
	out := make([]*models.Mock, 0, len(names))
	for i, n := range names {
		out = append(out, counterMock(n, base.Add(time.Duration(i)*time.Second)))
	}
	return out
}

// readCounter sends GET /counter n times through the real HTTP integration and
// returns the recording served for each.
func readCounter(t *testing.T, db integrations.MockMemDb, n int) string {
	t.Helper()
	h := httpint.New(zap.NewNop())
	got := make([]string, 0, n)
	for i := 0; i < n; i++ {
		resp, body, err := serveOne(t, h, db, "GET /counter HTTP/1.1\r\nHost: api\r\n\r\n")
		if resp == nil {
			t.Fatalf("call %d: no response; the integration returned %v", i+1, err)
		}
		got = append(got, string(body))
	}
	return strings.Join(got, ",")
}

func newCursorManager(t *testing.T) *MockManager {
	t.Helper()
	mgr := NewMockManager(NewTreeDb(customComparator), NewTreeDb(customComparator), zap.NewNop())
	t.Cleanup(mgr.Close)
	return mgr
}

// `keploy mock replay` stages its pool once, at BaseTime, which files every
// recording in the startup AND the session tier. The first match replaces the
// session tier's copy and leaves the startup tier's, so a pointer union listed
// each matched recording twice and the cursor walked 1,1,2,2,3.
func TestStatefulCursor_BaseTimeStagingServesRecordOrder(t *testing.T) {
	mgr := newCursorManager(t)
	base := time.Now().Add(-time.Hour)
	mgr.SetMocksWithWindow(nil, counterPool(base, "r1", "r2", "r3"), models.BaseTime, time.Now())

	if got, want := readCounter(t, mgr, 6), "r1,r2,r3,r3,r3,r3"; got != want {
		t.Fatalf("served %s, want %s", got, want)
	}
	sess, err := mgr.GetSessionMocks()
	if err != nil {
		t.Fatal(err)
	}
	if len(sess) != 3 {
		t.Fatalf("GetSessionMocks lists %d mocks after matching, want the 3 recordings once each", len(sess))
	}
}

// `keploy test` stages at BaseTime, then once per test with a fresh copy of the
// same pool; a sequence spans the whole test-set.
func TestStatefulCursor_KeployTestBootstrapThenTests(t *testing.T) {
	mgr := newCursorManager(t)
	base := time.Now().Add(-time.Hour)
	fresh := func() []*models.Mock { return counterPool(base, "r1", "r2", "r3") }
	mgr.SetMocksWithWindow(nil, fresh(), models.BaseTime, time.Now())

	// Two bootstrap reads before the first test: the second is where the
	// BaseTime-staged pool first holds a matched recording twice.
	got := []string{readCounter(t, mgr, 2)}
	for test := 0; test < 2; test++ {
		start := base.Add(time.Duration(10+10*test) * time.Second)
		mgr.SetMocksWithWindow(nil, fresh(), start, start.Add(5*time.Second))
		got = append(got, readCounter(t, mgr, 2))
	}
	if g, want := strings.Join(got, ","), "r1,r2,r3,r3,r3,r3"; g != want {
		t.Fatalf("served %s, want %s", g, want)
	}
}

// A scoped worker keeps the cursor (the wrap used to erase it, so every read
// replayed the first recording) and has its own: two workers reading the same
// request each walk their own test's recordings, and a worker's next scope
// starts its sequences over.
func TestStatefulCursor_ScopedWorkersEachWalkTheirOwnRecordings(t *testing.T) {
	mgr := newCursorManager(t)
	base := time.Now().Add(-time.Hour)
	pool := append(counterPool(base, "a1", "a2"), counterPool(base.Add(time.Minute), "b1", "b2")...)
	mgr.SetMocksWithWindow(nil, pool, models.BaseTime, time.Now())

	p := &Proxy{}
	p.SetMappedUniverse([]string{"a1", "a2", "b1", "b2"})
	const workerA, workerB = 4101, 4202
	p.SetWorkerScope(workerA, []string{"a1", "a2"})
	p.SetWorkerScope(workerB, []string{"b1", "b2"})
	viewA, viewB := p.scopedFor(workerA, mgr), p.scopedFor(workerB, mgr)

	steps := []struct {
		db   integrations.MockMemDb
		want string
	}{{viewA, "a1"}, {viewB, "b1"}, {viewA, "a2"}, {viewB, "b2"}, {viewA, "a2"}}
	for i, st := range steps {
		if got := readCounter(t, st.db, 1); got != st.want {
			t.Fatalf("read %d served %s, want %s", i+1, got, st.want)
		}
	}

	// The worker's next scope — here a re-run of the same test — starts over.
	p.SetWorkerScope(workerA, []string{"a1", "a2"})
	if got, want := readCounter(t, p.scopedFor(workerA, mgr), 3), "a1,a2,a2"; got != want {
		t.Fatalf("a new scope served %s, want %s", got, want)
	}
}

// Every reader of the startup∪session union — whole, windowed, keyed — lists
// a recording once after a match replaced its session-tier copy, and lists the
// replacement (the up-to-date copy) where the shared pointer used to be.
func TestSessionUnionListsARecordingOnceAfterAMatchReplacesIt(t *testing.T) {
	mgr := newCursorManager(t)
	base := time.Now().Add(-time.Hour)
	mgr.SetMocksWithWindow(nil, counterPool(base, "r1", "r2", "r3"), models.BaseTime, time.Now())

	before, err := mgr.GetSessionMocks()
	if err != nil || len(before) != 3 {
		t.Fatalf("staged pool: %d mocks, err %v; want 3", len(before), err)
	}
	old := before[0]
	upd := *old
	upd.TestModeInfo.SortOrder = old.TestModeInfo.SortOrder + 100
	if !mgr.UpdateUnFilteredMock(old, &upd) {
		t.Fatal("UpdateUnFilteredMock did not find the staged mock")
	}

	check := func(what string, got []*models.Mock) {
		t.Helper()
		if len(got) != 3 {
			t.Fatalf("%s lists %d mocks, want each of the 3 recordings once", what, len(got))
		}
		if got[0].Name != "r1" || got[0].TestModeInfo.SortOrder != upd.TestModeInfo.SortOrder {
			t.Fatalf("%s lists %s (sort order %d) first, want the updated copy of r1", what, got[0].Name, got[0].TestModeInfo.SortOrder)
		}
	}
	all, err := mgr.GetSessionMocks()
	if err != nil {
		t.Fatal(err)
	}
	check("GetSessionMocks", all)

	inWin, err := mgr.GetSessionMocksInWindow(base.Add(-time.Minute), base.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	check("GetSessionMocksInWindow", inWin)

	var keyed []*models.Mock
	ix := &integrations.MockIndex{Keys: func(*models.Mock) []string { return []string{"counter"} }}
	if err := mgr.RangeSessionMocksWithKey(ix, "counter", func(mk *models.Mock) bool {
		keyed = append(keyed, mk)
		return true
	}); err != nil {
		t.Fatal(err)
	}
	check("RangeSessionMocksWithKey", keyed)
}

// The startup tier itself follows a change made through the session tier: a
// reader of GetStartupMocks (a parser serving the bootstrap phase) sees the
// updated copy, and a recording deleted from the session tier is gone from the
// startup tier — and so from the union — too.
func TestStartupTierFollowsSessionUpdatesAndDeletes(t *testing.T) {
	mgr := newCursorManager(t)
	base := time.Now().Add(-time.Hour)
	mgr.SetMocksWithWindow(nil, counterPool(base, "r1", "r2", "r3"), models.BaseTime, time.Now())

	sess, _ := mgr.GetSessionMocks()
	old := sess[0]
	upd := *old
	upd.TestModeInfo.SortOrder = old.TestModeInfo.SortOrder + 100
	if !mgr.UpdateUnFilteredMock(old, &upd) {
		t.Fatal("UpdateUnFilteredMock did not find the staged mock")
	}
	startup, _ := mgr.GetStartupMocks()
	found := false
	for _, mk := range startup {
		if mk == old {
			t.Fatal("the startup tier still holds the pre-update copy")
		}
		found = found || mk == &upd
	}
	if !found {
		t.Fatal("the startup tier does not hold the updated copy")
	}

	gone := sess[1]
	if !mgr.DeleteUnFilteredMock(*gone) {
		t.Fatal("DeleteUnFilteredMock did not find the staged mock")
	}
	for _, read := range []func() ([]*models.Mock, error){mgr.GetStartupMocks, mgr.GetSessionMocks} {
		ms, _ := read()
		for _, mk := range ms {
			if mk.Name == gone.Name {
				t.Fatalf("deleted recording %s is still listed", gone.Name)
			}
		}
	}
}

// liveSets is the app-start table `keploy mock replay` installs in live mode:
// t1 recorded a1,a2 and t2 b1,b2 of the same counter.
func liveSets() map[string]models.SetTable {
	return map[string]models.SetTable{"": {Tests: map[string][]models.Owned{
		"t1": {{Name: "a1"}, {Name: "a2"}},
		"t2": {{Name: "b1"}, {Name: "b2"}},
	}}}
}

func livePool(t *testing.T) *MockManager {
	t.Helper()
	mgr := newCursorManager(t)
	base := time.Now().Add(-time.Hour)
	mgr.SetMocksWithWindow(nil, append(counterPool(base, "a1", "a2"), counterPool(base.Add(time.Minute), "b1", "b2")...), models.BaseTime, time.Now())
	starts.Default.SetTable("", liveSets())
	t.Cleanup(starts.Default.Reset)
	return mgr
}

// Live mode resolves what a call sees on every read, so its cursors are
// resolved on every read too: one keep-alive connection opened in t1 and still
// open in t2 walks t2's recordings from the first.
func TestStatefulCursor_LiveKeepAliveAcrossTests(t *testing.T) {
	mgr := livePool(t)
	pid := uint32(os.Getpid())
	starts.Default.Begin(pid, "t1", "", false, time.Now())
	conn := (&Proxy{}).scopedFor(pid, mgr)
	if got, want := readCounter(t, conn, 2), "a1,a2"; got != want {
		t.Fatalf("t1 served %s, want %s", got, want)
	}
	starts.Default.End(pid, "t1", false, time.Now())
	starts.Default.Begin(pid, "t2", "", false, time.Now())
	if got, want := readCounter(t, conn, 3), "b1,b2,b2"; got != want {
		t.Fatalf("t2 on the connection opened in t1 served %s, want %s", got, want)
	}
}

// A process with no test of its own (an app started once for the suite) sees
// the one test open on another worker, and its cursors follow that test: t2's
// sequence starts at its first recording, not where t1's left off.
func TestStatefulCursor_LiveSuiteAppFollowsTheOpenTest(t *testing.T) {
	mgr := livePool(t)
	app := uint32(os.Getpid())
	const runner = 3999999
	starts.Default.Begin(app, "suite", "", true, time.Now())
	p := &Proxy{}

	starts.Default.Begin(runner, "t1", "", false, time.Now())
	if got, want := readCounter(t, p.scopedFor(app, mgr), 2), "a1,a2"; got != want {
		t.Fatalf("during t1 served %s, want %s", got, want)
	}
	starts.Default.End(runner, "t1", false, time.Now())
	starts.Default.Begin(runner, "t2", "", false, time.Now())
	if got, want := readCounter(t, p.scopedFor(app, mgr), 2), "b1,b2"; got != want {
		t.Fatalf("during t2 served %s, want %s", got, want)
	}
}

// Two matchers can pick the same recording from one snapshot and both update
// it; the second update replaces the FIRST one's copy (the tree finds the
// recording by ID), so the startup tier must follow the copy the session tier
// actually held, not the caller's stale pointer.
func TestStartupTierFollowsTheCopyAnUpdateReplaced(t *testing.T) {
	mgr := newCursorManager(t)
	mgr.SetMocksWithWindow(nil, counterPool(time.Now().Add(-time.Hour), "r1", "r2", "r3"), models.BaseTime, time.Now())
	snap, _ := mgr.GetSessionMocks()
	p := snap[0]
	a, b := *p, *p
	a.TestModeInfo.SortOrder += 100
	b.TestModeInfo.SortOrder += 200
	if !mgr.UpdateUnFilteredMock(p, &a) || !mgr.UpdateUnFilteredMock(p, &b) {
		t.Fatal("both updates of the recording must land")
	}
	after, _ := mgr.GetSessionMocks()
	if len(after) != 3 || after[0] != &b {
		t.Fatalf("union lists %d mocks with %p first, want the 3 recordings with the last copy (%p) first", len(after), after[0], &b)
	}
}

// Concurrent matches of one recording leave both tiers holding the same copy.
func TestStartupTierStaysInStepUnderConcurrentUpdates(t *testing.T) {
	for round := 0; round < 200; round++ {
		mgr := newCursorManager(t)
		mgr.SetMocksWithWindow(nil, counterPool(time.Now().Add(-time.Hour), "r1", "r2", "r3"), models.BaseTime, time.Now())
		snap, _ := mgr.GetSessionMocks()
		p := snap[0]
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				c := *p
				c.TestModeInfo.SortOrder += int64(i + 1)
				<-start
				mgr.UpdateUnFilteredMock(p, &c)
			}(i)
		}
		close(start)
		wg.Wait()
		if after, _ := mgr.GetSessionMocks(); len(after) != 3 {
			t.Fatalf("round %d: union lists %d mocks after concurrent updates of one recording, want 3", round, len(after))
		}
	}
}

// Following an update into the startup tier keeps that tier's key index: the
// key does not change, so the entry is swapped in place rather than the index
// dropped and rebuilt on the next keyed lookup.
func TestStartupTierKeyIndexSurvivesAMirroredUpdate(t *testing.T) {
	mgr := newCursorManager(t)
	mgr.SetMocksWithWindow(nil, counterPool(time.Now().Add(-time.Hour), "r1", "r2", "r3"), models.BaseTime, time.Now())
	ix := &integrations.MockIndex{Keys: func(*models.Mock) []string { return []string{"counter"} }}
	walk := func() []*models.Mock {
		var out []*models.Mock
		_ = mgr.RangeSessionMocksWithKey(ix, "counter", func(mk *models.Mock) bool { out = append(out, mk); return true })
		return out
	}
	before := walk() // builds both tiers' key indexes
	upd := *before[0]
	upd.TestModeInfo.SortOrder += 100
	if !mgr.UpdateUnFilteredMock(before[0], &upd) {
		t.Fatal("update did not land")
	}
	mgr.treesMu.RLock()
	startup := mgr.startup
	mgr.treesMu.RUnlock()
	startup.mu.RLock()
	_, kept := startup.keyed[ix]
	startup.mu.RUnlock()
	if !kept {
		t.Fatal("the startup tier dropped its key index on a mirrored update")
	}
	if got := walk(); len(got) != 3 || got[0] != &upd {
		t.Fatalf("keyed walk lists %d mocks, first %p; want 3 with the updated copy (%p) first", len(got), got[0], &upd)
	}
}
