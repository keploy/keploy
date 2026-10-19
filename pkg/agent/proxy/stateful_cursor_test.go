package proxy

import (
	"os"
	"strconv"
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

// `keploy mock replay` begins its set with the whole pool, which files every
// recording in the startup AND the session tier. The first match replaces the
// session tier's copy and leaves the startup tier's, so a pointer union listed
// each matched recording twice and the cursor walked 1,1,2,2,3.
func TestStatefulCursor_APoolServesRecordOrder(t *testing.T) {
	mgr := newCursorManager(t)
	base := time.Now().Add(-time.Hour)
	mgr.BeginTestSet(time.Time{}, nil, nil, counterPool(base, "r1", "r2", "r3"))

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

// `keploy test` begins the set, then stages once per test with a fresh copy of the
// same pool; a sequence spans the whole test-set.
func TestStatefulCursor_KeployTestBootstrapThenTests(t *testing.T) {
	mgr := newCursorManager(t)
	base := time.Now().Add(-time.Hour)
	fresh := func() []*models.Mock { return counterPool(base, "r1", "r2", "r3") }
	mgr.BeginTestSet(time.Time{}, nil, nil, fresh())

	// Two bootstrap reads before the first test: the second is where the
	// pool the set began with first holds a matched recording twice.
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
	mgr.BeginTestSet(time.Time{}, nil, nil, pool)

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
	mgr.BeginTestSet(time.Time{}, nil, nil, counterPool(base, "r1", "r2", "r3"))

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
	mgr.BeginTestSet(time.Time{}, nil, nil, counterPool(base, "r1", "r2", "r3"))

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
	mgr.BeginTestSet(time.Time{}, nil, nil, append(counterPool(base, "a1", "a2"), counterPool(base.Add(time.Minute), "b1", "b2")...))
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
	mgr.BeginTestSet(time.Time{}, nil, nil, counterPool(time.Now().Add(-time.Hour), "r1", "r2", "r3"))
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
		mgr.BeginTestSet(time.Time{}, nil, nil, counterPool(time.Now().Add(-time.Hour), "r1", "r2", "r3"))
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
	mgr.BeginTestSet(time.Time{}, nil, nil, counterPool(time.Now().Add(-time.Hour), "r1", "r2", "r3"))
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

// Allow mode, one keep-alive connection: its cursors follow the worker's scope
// to the next test, which walks its own recordings from the first.
func TestStatefulCursor_AllowKeepAliveAcrossTests(t *testing.T) {
	mgr := newCursorManager(t)
	base := time.Now().Add(-time.Hour)
	mgr.BeginTestSet(time.Time{}, nil, nil, append(counterPool(base, "a1", "a2"), counterPool(base.Add(time.Minute), "b1", "b2")...))
	p := &Proxy{}
	p.SetMappedUniverse([]string{"a1", "a2", "b1", "b2"})
	w := uint32(os.Getpid())
	p.SetWorkerScope(w, []string{"a1", "a2"})
	conn := p.scopedFor(w, mgr)
	if got, want := readCounter(t, conn, 2), "a1,a2"; got != want {
		t.Fatalf("t1 served %s, want %s", got, want)
	}
	p.ClearWorkerScope(w)
	p.SetWorkerScope(w, []string{"b1", "b2"})
	if got, want := readCounter(t, conn, 3), "b1,b2,b2"; got != want {
		t.Fatalf("t2 on the connection opened in t1 served %s, want %s", got, want)
	}
}

// The commit after a peek advances the scope the peek resolved, even when the
// worker's next test begins between the two.
func TestStatefulCursor_AllowCommitReusesThePeekedScope(t *testing.T) {
	mgr := newCursorManager(t)
	self := uint32(os.Getpid())
	p := &Proxy{}
	p.SetMappedUniverse([]string{"a1"})
	conn := workerScopedOn(t, p, mgr, []string{"a1"})
	idx := conn.MockCursorIndex("k", 3)    // a t1 read peeks
	p.SetWorkerScope(self, []string{"a1"}) // t2 begins before it commits
	conn.AdvanceMockCursor("k", idx, 3)
	if got := conn.MockCursorIndex("k", 3); got != 0 {
		t.Fatalf("t2's cursor starts at %d: the commit of a t1 read advanced it", got)
	}
}

// perTestCounterPool is a stateful dependency's recordings as a recorder that
// states lifetimes writes them: per-test (models.MetadataLifetimeKey), which
// DeriveLifetime gives the record-order cursor too.
func perTestCounterPool(base time.Time, names ...string) []*models.Mock {
	out := counterPool(base, names...)
	for _, m := range out {
		m.Spec.Metadata = map[string]string{"type": "HTTP_CLIENT", models.MetadataLifetimeKey: models.MetadataLifetimePerTest}
		m.TestModeInfo = models.TestModeInfo{}
		m.DeriveLifetime()
	}
	return out
}

// readOrMiss is readCounter that writes MISS for a call no recording answers.
func readOrMiss(t *testing.T, db integrations.MockMemDb, n int) string {
	t.Helper()
	h := httpint.New(zap.NewNop())
	got := make([]string, 0, n)
	for i := 0; i < n; i++ {
		resp, body, _ := serveOne(t, h, db, "GET /counter HTTP/1.1\r\nHost: api\r\n\r\n")
		if resp == nil || resp.StatusCode != 200 {
			got = append(got, "MISS")
			continue
		}
		got = append(got, string(body))
	}
	return strings.Join(got, ",")
}

// A stateful dependency's per-test recordings take turns, and saturate on the
// last, within the pool they are served from, and never across tests: a test
// is served its own recordings in record order and then its last one again;
// the next test starts its own over; a test that recorded none of them misses
// rather than being answered with another test's. Staged once as the set
// begins (`keploy mock replay` with no test open), every test's recordings are the
// pool, in record order.
func TestStatefulCursor_PerTestRecordingsTakeTurnsWithinTheirTestOnly(t *testing.T) {
	base := time.Now().Add(-time.Hour)
	pool := func() []*models.Mock {
		return append(perTestCounterPool(base, "a1", "a2", "a3"), perTestCounterPool(base.Add(time.Minute), "b1", "b2")...)
	}
	window := func(from time.Time) (time.Time, time.Time) { return from.Add(-time.Second), from.Add(10 * time.Second) }

	t.Run("no test open", func(t *testing.T) {
		mgr := newCursorManager(t)
		mgr.BeginTestSet(time.Time{}, nil, pool(), nil)
		if got, want := readOrMiss(t, mgr, 6), "a1,a2,a3,b1,b2,b2"; got != want {
			t.Fatalf("served %s, want %s", got, want)
		}
	})
	t.Run("keploy test", func(t *testing.T) {
		mgr := newCursorManager(t)
		mgr.BeginTestSet(time.Time{}, nil, pool(), nil)
		var got []string
		for _, tc := range []struct {
			from time.Time
			n    int
		}{{base, 4}, {base.Add(time.Minute), 3}, {base.Add(5 * time.Minute), 1}, {base, 2}} {
			start, end := window(tc.from)
			mgr.SetMocksWithWindow(pool(), nil, start, end)
			got = append(got, readOrMiss(t, mgr, tc.n))
		}
		// Test A, test B, test C (which recorded no call), and A again.
		if g, want := strings.Join(got, " | "), "a1,a2,a3,a3 | b1,b2,b2 | MISS | a1,a2"; g != want {
			t.Fatalf("served %s, want %s", g, want)
		}
	})
	t.Run("scoped workers", func(t *testing.T) {
		mgr := newCursorManager(t)
		mgr.BeginTestSet(time.Time{}, nil, pool(), nil)
		p := &Proxy{}
		p.SetMappedUniverse([]string{"a1", "a2", "a3", "b1", "b2"})
		const workerA, workerB = 4101, 4202
		p.SetWorkerScope(workerA, []string{"a1", "a2", "a3"})
		p.SetWorkerScope(workerB, []string{"b1", "b2"})
		viewA, viewB := p.scopedFor(workerA, mgr), p.scopedFor(workerB, mgr)
		got := readOrMiss(t, viewA, 1) + " " + readOrMiss(t, viewB, 1) + " " + readOrMiss(t, viewA, 3) + " " + readOrMiss(t, viewB, 2)
		if want := "a1 b1 a2,a3,a3 b2,b2"; got != want {
			t.Fatalf("served %s, want %s", got, want)
		}
		// Both tests end: what each kept for its repeats is spent, so a call
		// between tests (no worker scope: the whole pool) is served none of it.
		p.ClearWorkerScope(workerA)
		p.ClearWorkerScope(workerB)
		if got := readOrMiss(t, mgr, 1); got != "MISS" {
			t.Fatalf("a call between tests was served %s, want a miss: both tests consumed their recordings", got)
		}
		// A later run of test A is served its recordings again, from the start.
		p.SetWorkerScope(workerA, []string{"a1", "a2", "a3"})
		if got, want := readOrMiss(t, p.scopedFor(workerA, mgr), 4), "a1,a2,a3,a3"; got != want {
			t.Fatalf("a new scope served %s, want %s", got, want)
		}
	})
}

// A per-test recording is kept for its test's byte-identical repeats only: a
// call whose body differs (a fresh id each time) consumes the recording it is
// matched with, so a test's two different calls are answered by its two
// recordings, not the first one twice.
func TestStatefulCursor_ADifferentCallConsumesTheRecordingItTakes(t *testing.T) {
	base := time.Now().Add(-time.Hour)
	event := func(name, id string, at time.Time) *models.Mock {
		m := counterMock(name, at)
		m.Spec.HTTPReq = &models.HTTPReq{Method: "POST", URL: "http://api/events", ProtoMajor: 1, ProtoMinor: 1,
			Header: map[string]string{"Content-Type": "application/json"}, Body: `{"id":"` + id + `"}`}
		m.Spec.Metadata = map[string]string{"type": "HTTP_CLIENT", models.MetadataLifetimeKey: models.MetadataLifetimePerTest}
		m.TestModeInfo = models.TestModeInfo{}
		m.DeriveLifetime()
		return m
	}
	mgr := newCursorManager(t)
	pool := []*models.Mock{event("e1", "aaaa1111", base), event("e2", "bbbb2222", base.Add(time.Second))}
	mgr.BeginTestSet(time.Time{}, nil, pool, nil)
	mgr.SetMocksWithWindow(pool, nil, base.Add(-time.Second), base.Add(10*time.Second))
	h := httpint.New(zap.NewNop())
	var got []string
	for _, id := range []string{"cccc3333", "dddd4444"} {
		body := `{"id":"` + id + `"}`
		raw := "POST /events HTTP/1.1\r\nHost: api\r\nContent-Type: application/json\r\nContent-Length: " + strconv.Itoa(len(body)) + "\r\n\r\n" + body
		resp, served, _ := serveOne(t, h, mgr, raw)
		if resp == nil || resp.StatusCode != 200 {
			got = append(got, "MISS")
			continue
		}
		got = append(got, string(served))
	}
	if g, want := strings.Join(got, ","), "e1,e2"; g != want {
		t.Fatalf("served %s, want %s: the second call is answered by the recording the first did not take", g, want)
	}
}

// A per-test recording of a call the app also made at startup is answered
// from the open test's own recordings first: the startup band's recording,
// which the reusable union holds, is a sequence of its own.
func TestStatefulCursor_ATestsOwnRecordingComesBeforeTheStartupBand(t *testing.T) {
	base := time.Now().Add(-time.Hour)
	boot := perTestCounterPool(base, "boot")
	own := perTestCounterPool(base.Add(time.Minute), "a1")
	pool := append(boot, own...)
	mgr := newCursorManager(t)
	mgr.BeginTestSet(time.Time{}, nil, pool, nil)
	if got := readOrMiss(t, mgr, 1); got != "boot" {
		t.Fatalf("at startup the call was served %s, want boot", got)
	}
	start := base.Add(time.Minute - time.Second)
	mgr.SetMocksWithWindow(pool, nil, start, start.Add(10*time.Second))
	if got, want := readOrMiss(t, mgr, 2), "a1,a1"; got != want {
		t.Fatalf("in test A the call was served %s, want %s: its own recording, then again", got, want)
	}
}

// searchMock is a recording of POST /search with body, as a recorder that
// states lifetimes writes it (per-test) or as one made before (stated false:
// DeriveLifetime gives an untagged HTTP mock the reusable cursor).
func searchMock(name, body string, at time.Time, stated bool) *models.Mock {
	m := counterMock(name, at)
	m.Spec.HTTPReq = &models.HTTPReq{Method: "POST", URL: "http://api/search", ProtoMajor: 1, ProtoMinor: 1,
		Header: map[string]string{"Content-Type": "application/json"}, Body: body}
	m.Spec.Metadata = map[string]string{"type": "HTTP_CLIENT"}
	if stated {
		m.Spec.Metadata[models.MetadataLifetimeKey] = models.MetadataLifetimePerTest
	}
	m.TestModeInfo = models.TestModeInfo{}
	m.DeriveLifetime()
	return m
}

// postOrMiss sends POST /search with each body through the real HTTP
// integration and returns the recording served for each, MISS for none.
func postOrMiss(t *testing.T, db integrations.MockMemDb, bodies ...string) string {
	t.Helper()
	h := httpint.New(zap.NewNop())
	got := make([]string, 0, len(bodies))
	for _, b := range bodies {
		raw := "POST /search HTTP/1.1\r\nHost: api\r\nContent-Type: application/json\r\nContent-Length: " + strconv.Itoa(len(b)) + "\r\n\r\n" + b
		resp, served, _ := serveOne(t, h, db, raw)
		if resp == nil || resp.StatusCode != 200 {
			got = append(got, "MISS")
			continue
		}
		got = append(got, string(served))
	}
	return strings.Join(got, ",")
}

// With no test open (keploy mock replay that opens none, or the app's startup
// before the first test), no recording is a test's own: a recording that
// states its lifetime is served as one made before is, whatever the order or
// number of the calls, and neither consumed by a call that differs nor kept
// from one.
func TestStatefulCursor_NoTestOpenServesEveryRecordingAsReusable(t *testing.T) {
	base := time.Now().Add(-time.Hour)
	exact, other := `{"q":"a","p":1}`, `{"q":"a","p":2}`
	for _, tc := range []struct {
		name  string
		pool  func(stated bool) []*models.Mock
		calls []string
		want  string
	}{
		{"a different call, the recorded one, and the different one again",
			func(stated bool) []*models.Mock { return []*models.Mock{searchMock("r1", exact, base, stated)} },
			[]string{other, exact, other}, "r1,r1,r1"},
		{"the recorded call, then a different one",
			func(stated bool) []*models.Mock { return []*models.Mock{searchMock("r1", exact, base, stated)} },
			[]string{exact, other}, "r1,r1"},
		{"more calls with fresh ids than recordings",
			func(stated bool) []*models.Mock {
				return []*models.Mock{searchMock("e1", `{"id":"aaaa1111"}`, base, stated), searchMock("e2", `{"id":"bbbb2222"}`, base.Add(time.Second), stated)}
			},
			[]string{`{"id":"cccc3333"}`, `{"id":"dddd4444"}`, `{"id":"eeee5555"}`, `{"id":"ffff6666"}`}, "e1,e1,e1,e1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, stated := range []bool{false, true} {
				mgr := newCursorManager(t)
				if stated {
					mgr.BeginTestSet(time.Time{}, nil, tc.pool(true), nil)
				} else {
					mgr.BeginTestSet(time.Time{}, nil, nil, tc.pool(false))
				}
				if got := postOrMiss(t, mgr, tc.calls...); got != tc.want {
					t.Errorf("stated %v: served %s, want %s", stated, got, tc.want)
				}
			}
		})
	}
	// A test's window cleared (SetCurrentTestWindow with zero values): the
	// per-test pool is every recording again, and none is a test's own.
	t.Run("a window cleared", func(t *testing.T) {
		mgr := newCursorManager(t)
		pool := []*models.Mock{searchMock("e1", `{"id":"aaaa1111"}`, base, true), searchMock("e2", `{"id":"bbbb2222"}`, base.Add(time.Second), true)}
		mgr.SetMocksWithWindow(pool, nil, base.Add(-time.Second), base.Add(10*time.Second))
		mgr.SetCurrentTestWindow(time.Time{}, time.Time{})
		if got, want := postOrMiss(t, mgr, `{"id":"cccc3333"}`, `{"id":"dddd4444"}`, `{"id":"eeee5555"}`), "e1,e1,e1"; got != want {
			t.Errorf("served %s, want %s", got, want)
		}
	})
}

// A scoped worker's test owns the recordings its mapping names: a call that
// differs consumes the one it takes, so the test's second call is answered by
// its second recording. A recording mapped to no test (the app's startup
// call) is no test's own: served to the test, it is neither consumed nor
// spent when the test ends, and the next test is served it too.
func TestStatefulCursor_AScopedTestOwnsItsMappedRecordingsOnly(t *testing.T) {
	base := time.Now().Add(-time.Hour)
	pool := []*models.Mock{
		searchMock("boot", `{"id":"0000boot"}`, base.Add(-time.Minute), true),
		searchMock("a1", `{"id":"aaaa1111"}`, base, true),
		searchMock("a2", `{"id":"aaaa2222"}`, base.Add(time.Second), true),
	}
	mgr := newCursorManager(t)
	mgr.BeginTestSet(time.Time{}, nil, pool, nil)
	p := &Proxy{}
	p.SetMappedUniverse([]string{"a1", "a2"})
	const worker = 4101
	p.SetWorkerScope(worker, []string{"a1", "a2"})
	if got, want := postOrMiss(t, p.scopedFor(worker, mgr), `{"id":"aaaa1111"}`, `{"id":"aaaa2222"}`), "a1,a2"; got != want {
		t.Fatalf("the test's own calls were served %s, want %s", got, want)
	}
	if got, want := postOrMiss(t, p.scopedFor(worker, mgr), `{"id":"0000boot"}`, `{"id":"0000boot"}`), "boot,boot"; got != want {
		t.Fatalf("the startup call in the test was served %s, want %s", got, want)
	}
	p.ClearWorkerScope(worker)
	p.SetWorkerScope(worker, []string{})
	if got, want := postOrMiss(t, p.scopedFor(worker, mgr), `{"id":"0000boot"}`), "boot"; got != want {
		t.Fatalf("the next test's startup call was served %s, want %s: no test owns the recording", got, want)
	}
}

// A scoped worker's test run again in the same agent (a retry, -count=2, a
// watch-mode rerun) is served its whole recording again, the recordings its
// first run's calls consumed included; no call between the runs is.
func TestStatefulCursor_AScopedTestRunAgainIsServedItsRecordingsAgain(t *testing.T) {
	base := time.Now().Add(-time.Hour)
	pool := []*models.Mock{
		searchMock("a1", `{"id":"aaaa1111"}`, base, true),
		searchMock("a2", `{"id":"aaaa2222"}`, base.Add(time.Second), true),
	}
	mgr := newCursorManager(t)
	mgr.BeginTestSet(time.Time{}, nil, pool, nil)
	p := &Proxy{}
	p.SetMappedUniverse([]string{"a1", "a2"})
	const worker = 4101
	calls := []string{`{"id":"cccc3333"}`, `{"id":"dddd4444"}`, `{"id":"eeee5555"}`}
	keyed := func(db integrations.MockMemDb) string {
		var names []string
		ix := &integrations.MockIndex{Keys: func(*models.Mock) []string { return []string{"search"} }}
		_ = db.(integrations.SessionKeyReader).RangeSessionMocksWithKey(ix, "search", func(mk *models.Mock) bool {
			names = append(names, mk.Name)
			return true
		})
		return strings.Join(names, ",")
	}
	for run := 1; run <= 2; run++ {
		p.SetWorkerScope(worker, []string{"a1", "a2"})
		conn := p.scopedFor(worker, mgr)
		if got, want := postOrMiss(t, conn, calls...), "a1,a2,MISS"; got != want {
			t.Fatalf("run %d was served %s, want %s", run, got, want)
		}
		// A keyed read of the test hides what it consumed too.
		if got := keyed(conn); got != "" {
			t.Fatalf("after run %d's calls a keyed read showed %q, want nothing", run, got)
		}
		p.ClearWorkerScope(worker)
		// Between the runs: on a connection the test opened, and on a new one.
		if got := postOrMiss(t, conn, calls[0]) + "," + postOrMiss(t, mgr, calls[0]); got != "MISS,MISS" {
			t.Fatalf("a call after run %d was served %s, want misses", run, got)
		}
	}
}

// In live mode (the starts registry) a test run again is a new frame, served
// its recordings afresh; within a run each is served once, and the next test
// is served none of them.
func TestStatefulCursor_ALiveTestRunAgainIsServedItsRecordingsAgain(t *testing.T) {
	base := time.Now().Add(-time.Hour)
	mgr := newCursorManager(t)
	mgr.BeginTestSet(time.Time{}, nil, []*models.Mock{
		searchMock("a1", `{"id":"aaaa1111"}`, base, true),
		searchMock("a2", `{"id":"aaaa2222"}`, base.Add(time.Second), true),
	}, nil)
	starts.Default.SetTable("", liveSets())
	t.Cleanup(starts.Default.Reset)
	pid := uint32(os.Getpid())
	conn := (&Proxy{}).scopedFor(pid, mgr)
	calls := []string{`{"id":"cccc3333"}`, `{"id":"dddd4444"}`, `{"id":"eeee5555"}`}
	for run := 1; run <= 2; run++ {
		starts.Default.Begin(pid, "t1", "", false, time.Now())
		if got, want := postOrMiss(t, conn, calls...), "a1,a2,MISS"; got != want {
			t.Fatalf("run %d of t1 was served %s, want %s", run, got, want)
		}
		starts.Default.End(pid, "t1", false, time.Now())
	}
	starts.Default.Begin(pid, "t2", "", false, time.Now())
	if got := postOrMiss(t, conn, calls[0]); got != "MISS" {
		t.Fatalf("t2 was served %s, want a miss: t1's recordings are t1's", got)
	}
}

// Inside a test, a call the app also made at startup is answered by the
// startup recording, from the reusable union, every time: it is no test's
// own, so it is not consumed.
func TestStatefulCursor_TheStartupRecordingInsideATestIsNotConsumed(t *testing.T) {
	base := time.Now().Add(-time.Hour)
	pool := []*models.Mock{
		searchMock("boot", `{"id":"0000boot"}`, base.Add(-time.Minute), true),
		searchMock("a1", `{"id":"aaaa1111"}`, base, true),
	}
	mgr := newCursorManager(t)
	mgr.BeginTestSet(time.Time{}, nil, pool, nil)
	mgr.SetMocksWithWindow(pool, nil, base.Add(-time.Second), base.Add(10*time.Second))
	if got, want := postOrMiss(t, mgr, `{"id":"0000boot"}`, `{"id":"0000boot"}`, `{"id":"1111boot"}`), "boot,boot,boot"; got != want {
		t.Fatalf("the startup call inside the test was served %s, want %s", got, want)
	}
}

// A test begun without a worker pid (MockManager.SetScopeMocks) owns the
// per-test pool staged for it, not a per-test recording staged with the
// reusable tier (one mapped to no test): served by an exact match, the
// latter is not kept from a later call that differs.
func TestStatefulCursor_APidlessTestOwnsItsStagedPoolOnly(t *testing.T) {
	base := time.Now().Add(-time.Hour)
	mgr := newCursorManager(t)
	mgr.BeginTestSet(time.Time{}, nil, nil, nil)
	mgr.SetScopeMocks([]*models.Mock{searchMock("a1", `{"id":"aaaa1111"}`, base, true)},
		[]*models.Mock{searchMock("boot", `{"id":"0000boot"}`, base.Add(-time.Minute), true)}, true)
	if got, want := postOrMiss(t, mgr, `{"id":"0000boot"}`, `{"id":"1111boot"}`), "boot,boot"; got != want {
		t.Fatalf("the unmapped call in the test was served %s, want %s", got, want)
	}
}
