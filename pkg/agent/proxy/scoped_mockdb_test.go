package proxy

import (
	"os"
	"runtime"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
)

// fakeMockDb is a minimal MockMemDb: the per-test read methods return a fixed
// set; every other method is the promoted (nil) interface and must not be called
// by these tests.
type fakeMockDb struct {
	integrations.MockMemDb
	mocks []*models.Mock
}

func (f *fakeMockDb) GetFilteredMocks() ([]*models.Mock, error)         { return f.mocks, nil }
func (f *fakeMockDb) GetFilteredMocksInWindow() ([]*models.Mock, error) { return f.mocks, nil }
func (f *fakeMockDb) GetPerTestMocksInWindow() ([]*models.Mock, error)  { return f.mocks, nil }
func (f *fakeMockDb) GetSessionMocks() ([]*models.Mock, error)          { return f.mocks, nil }
func (f *fakeMockDb) GetUnFilteredMocks() ([]*models.Mock, error)       { return f.mocks, nil }
func (f *fakeMockDb) GetSessionScopedMocks() ([]*models.Mock, error)    { return f.mocks, nil }

func scopedNames(ms []*models.Mock) []string {
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		out = append(out, m.Name)
	}
	return out
}

// workerScoped is the view a call from this process gets once it registered as
// a worker whose test may see allow, under the given mapped universe.
func workerScoped(t *testing.T, db integrations.MockMemDb, allow, universe []string) *scopedMockDb {
	t.Helper()
	p := &Proxy{}
	p.SetMappedUniverse(universe)
	return workerScopedOn(t, p, db, allow)
}

// workerScopedOn registers this process as a worker of p allowed allow, and
// returns the view its calls get.
func workerScopedOn(t *testing.T, p *Proxy, db integrations.MockMemDb, allow []string) *scopedMockDb {
	t.Helper()
	p.SetWorkerScope(uint32(os.Getpid()), allow)
	v, ok := p.scopedFor(uint32(os.Getpid()), db).(*scopedMockDb)
	require.True(t, ok, "a registered worker's call is scoped")
	return v
}

func TestScopedMockDbFiltersReads(t *testing.T) {
	// m1,m2,m3 belong to tests (in the universe); "shared" belongs to no test.
	db := &fakeMockDb{mocks: []*models.Mock{{Name: "m1"}, {Name: "m2"}, {Name: "m3"}, {Name: "shared"}}}
	universe := []string{"m1", "m2", "m3"}

	// Worker allowed m1,m3: it sees m1,m3 (its test) + "shared" (unmapped), but
	// NOT m2 (another test's mock). Applies to per-test AND session tiers.
	s := workerScoped(t, db, []string{"m1", "m3"}, universe)
	for _, get := range []func() ([]*models.Mock, error){
		s.GetPerTestMocksInWindow, s.GetFilteredMocks, s.GetFilteredMocksInWindow,
		s.GetSessionMocks, s.GetUnFilteredMocks, s.GetSessionScopedMocks,
	} {
		got, err := get()
		require.NoError(t, err)
		require.ElementsMatch(t, []string{"m1", "m3", "shared"}, scopedNames(got),
			"worker sees its own test's mocks plus unmapped shared mocks, never another test's")
	}

	// A nil universe (no mappings pushed) is a passthrough — never hide anything.
	pass := workerScoped(t, db, []string{"m1"}, nil)
	got, err := pass.GetSessionMocks()
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"m1", "m2", "m3", "shared"}, scopedNames(got))
}

func TestScopedForResolvesWorkerAndFallsBack(t *testing.T) {
	db := &fakeMockDb{mocks: []*models.Mock{{Name: "only-this"}, {Name: "other"}, {Name: "shared"}}}
	self := uint32(os.Getpid())
	sees := func(v integrations.MockMemDb) []string {
		got, err := v.GetSessionMocks()
		require.NoError(t, err)
		return scopedNames(got)
	}

	// No worker scoped: fast path, no /proc walk, the bare manager.
	p := &Proxy{}
	require.Equal(t, integrations.MockMemDb(db), p.scopedFor(self, db), "no scopes ⇒ whole pool")

	// The replay installed its scope table, but no worker registered: still
	// the bare manager. A wrap is a new store identity, which parsers that
	// key state on the store would split per connection.
	p.SetMappedUniverse([]string{"only-this", "other"})
	require.Equal(t, integrations.MockMemDb(db), p.scopedFor(self, db), "a table alone does not wrap")

	// A registered worker PID: the call is scoped to its allowlist.
	p.SetWorkerScope(self, []string{"only-this"})
	view := p.scopedFor(self, db)
	require.ElementsMatch(t, []string{"only-this", "shared"}, sees(view), "own PID resolves to its test's mocks")

	// PID 0 (unknown origin) and an unregistered/dead PID ⇒ the bare manager.
	require.Equal(t, integrations.MockMemDb(db), p.scopedFor(0, db))
	require.Equal(t, integrations.MockMemDb(db), p.scopedFor(4000000000, db))

	// Cleared ⇒ the same, still-open view is back to the whole pool.
	p.ClearWorkerScope(self)
	require.ElementsMatch(t, []string{"only-this", "other", "shared"}, sees(view))
}

// A connection outlives a test: a keep-alive client a worker opened during one
// test and reuses in the next sees the next test's mocks, and revision-gated
// parsers on it are told the view changed — while another worker's test does
// not touch it.
func TestScopedViewFollowsItsWorkerAcrossTests(t *testing.T) {
	mgr := NewMockManager(NewTreeDb(customComparator), NewTreeDb(customComparator), zap.NewNop())
	defer mgr.Close()
	at := time.Now().Add(-time.Hour)
	mgr.SetMocksWithWindow(nil, []*models.Mock{
		newMockForTest("a1", at, models.LifetimeSession),
		newMockForTest("b1", at, models.LifetimeSession),
		newMockForTest("shared", at, models.LifetimeSession),
	}, models.BaseTime, time.Now())
	self := uint32(os.Getpid())
	sees := func(v integrations.MockMemDb) []string {
		got, err := v.GetSessionMocks()
		require.NoError(t, err)
		return scopedNames(got)
	}
	p := &Proxy{}
	p.SetMappedUniverse([]string{"a1", "b1"})
	p.SetWorkerScope(self, []string{"a1"})

	conn := p.scopedFor(self, mgr).(*scopedMockDb) // opened during t1
	require.ElementsMatch(t, []string{"a1", "shared"}, sees(conn))
	rev, byKind := conn.Revision(), conn.RevisionByKind(models.HTTP)
	require.Equal(t, rev, conn.Revision(), "an unchanged view keeps its revision")

	p.SetWorkerScope(4000000001, []string{"b1"}) // another worker's test
	require.Equal(t, rev, conn.Revision(), "another worker's test does not touch this view")

	p.ClearWorkerScope(self)
	p.SetWorkerScope(self, []string{"b1"})
	require.ElementsMatch(t, []string{"b1", "shared"}, sees(conn), "a keep-alive connection follows the worker to its next test")
	next := conn.Revision()
	require.Greater(t, next, rev, "a parser that caches what it read must read again")
	require.Greater(t, conn.RevisionByKind(models.HTTP), byKind)
	require.Equal(t, next, conn.Revision())
}

// A parser that caches by revision samples the revision, then reads, and
// keeps what it read under the revision it sampled. A view between two tests
// shows the whole pool both times, but it must not report the same revision:
// a test that began after the sample and ended before the next one would
// leave that test's filtered read cached as the whole pool.
func TestScopedViewNeverReturnsToAnEarlierRevision(t *testing.T) {
	db := &fakeMockDb{mocks: []*models.Mock{{Name: "a1"}, {Name: "b1"}, {Name: "shared"}}}
	self := uint32(os.Getpid())
	p := &Proxy{}
	p.SetMappedUniverse([]string{"a1", "b1"})
	conn := workerScopedOn(t, p, db, []string{"a1"})
	p.ClearWorkerScope(self) // t1 ended: the connection is between tests

	sampled := conn.Revision()
	p.SetWorkerScope(self, []string{"b1"}) // t2 begins after the sample
	cached, err := conn.GetSessionMocks()  // read under the sampled revision
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"b1", "shared"}, scopedNames(cached))
	p.ClearWorkerScope(self) // t2 ends before the next sample

	require.Greater(t, conn.Revision(), sampled, "the view is the whole pool again, but what was read under the old revision is not")

	// And a test's end is a change of its own: a parser that sampled during
	// t3 must read again once t3 has ended and the view is the whole pool.
	p.SetWorkerScope(self, []string{"a1"})
	during := conn.Revision()
	p.ClearWorkerScope(self)
	require.Greater(t, conn.Revision(), during, "the test ended: the view changed")
}

// Replacing the mapped universe changes what every view filters with.
func TestScopedViewRevisionFollowsTheMappedUniverse(t *testing.T) {
	db := &fakeMockDb{mocks: []*models.Mock{{Name: "a1"}, {Name: "b1"}, {Name: "shared"}}}
	p := &Proxy{}
	p.SetMappedUniverse([]string{"a1", "b1"})
	conn := workerScopedOn(t, p, db, []string{"a1"})
	rev := conn.Revision()
	p.SetMappedUniverse([]string{"a1", "b1", "shared"})
	got, err := conn.GetSessionMocks()
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"a1"}, scopedNames(got), "shared is now another test's")
	require.Greater(t, conn.Revision(), rev)
}

// A view made later reports a revision above anything an earlier view
// reported, so a parser that keys its cache on the store's address cannot
// take a new view at a recycled address for the one it knew.
func TestScopedViewStartsAboveEveryEarlierView(t *testing.T) {
	db := &fakeMockDb{}
	self, other := uint32(os.Getpid()), uint32(4000000001) // no /proc entry: its chain is itself
	p := &Proxy{}
	p.SetMappedUniverse([]string{"a1"})
	p.SetWorkerScope(other, []string{"a1"}) // before everything the old view goes through
	old := workerScopedOn(t, p, db, []string{"a1"})
	p.ClearWorkerScope(self)
	p.SetWorkerScope(self, []string{"a1"})
	last := old.Revision() // the old connection's last word, then it closes

	fresh, ok := p.scopedFor(other, db).(*scopedMockDb)
	require.True(t, ok)
	require.Greater(t, fresh.Revision(), last, "nothing of the new view changed since the old one's last revision, but it is a different view")
}

// Every registered worker on a connection's process chain counts, not only the
// nearest: when a worker nested under another ends its test, the view falls to
// the outer worker's test, and when that one ends too, to the whole pool. Each
// is a change a caching parser must hear of.
func TestScopedViewFollowsEveryWorkerOnItsChain(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the parent is found by walking /proc (linux only)")
	}
	db := &fakeMockDb{mocks: []*models.Mock{{Name: "a"}, {Name: "b"}, {Name: "shared"}}}
	self, parent := uint32(os.Getpid()), uint32(os.Getppid())
	sees := func(v integrations.MockMemDb) []string {
		got, err := v.GetSessionMocks()
		require.NoError(t, err)
		return scopedNames(got)
	}
	p := &Proxy{}
	p.SetMappedUniverse([]string{"a", "b"})
	p.SetWorkerScope(parent, []string{"b"})
	conn := workerScopedOn(t, p, db, []string{"a"})
	require.ElementsMatch(t, []string{"a", "shared"}, sees(conn))
	rev := conn.Revision()

	p.ClearWorkerScope(self)
	require.ElementsMatch(t, []string{"b", "shared"}, sees(conn), "the inner worker ended: the outer one's test")
	inner := conn.Revision()
	require.Greater(t, inner, rev)

	p.ClearWorkerScope(parent)
	require.ElementsMatch(t, []string{"a", "b", "shared"}, sees(conn), "the outer worker ended: the whole pool")
	outer := conn.Revision()
	require.Greater(t, outer, inner)

	p.SetWorkerScope(parent, []string{"a"})
	require.ElementsMatch(t, []string{"a", "shared"}, sees(conn))
	require.Greater(t, conn.Revision(), outer, "the outer worker began again")
}

// Wiping every scope at once (a new replay session) changes every open view.
func TestScopedViewRevisionFollowsAWipe(t *testing.T) {
	db := &fakeMockDb{mocks: []*models.Mock{{Name: "a"}, {Name: "b"}, {Name: "shared"}}}
	p := &Proxy{}
	p.SetMappedUniverse([]string{"a", "b"})
	conn := workerScopedOn(t, p, db, []string{"a"})
	rev := conn.Revision()
	p.ClearAllWorkerScopes()
	got, err := conn.GetSessionMocks()
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"a", "b", "shared"}, scopedNames(got))
	require.Greater(t, conn.Revision(), rev)
}

func TestScopedForWalksUpProcessTree(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("process-tree walk is linux only")
	}
	db := &fakeMockDb{mocks: []*models.Mock{{Name: "parents"}, {Name: "others"}, {Name: "shared"}}}
	// Register the PARENT of this process; this process's call must resolve up to
	// it (models a worker whose child/grandchild opened the socket).
	p := &Proxy{}
	p.SetMappedUniverse([]string{"parents", "others"})
	p.SetWorkerScope(uint32(os.Getppid()), []string{"parents"})
	view := p.scopedFor(uint32(os.Getpid()), db)
	got, err := view.GetSessionMocks()
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"parents", "shared"}, scopedNames(got),
		"a call from a descendant sees the registered ancestor's test")

	p.ClearAllWorkerScopes()
	require.Equal(t, integrations.MockMemDb(db), p.scopedFor(uint32(os.Getpid()), db))
}

func TestPpidFromStat(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("/proc only on linux")
	}
	ppid, ok := ppidFromStat(uint32(os.Getpid()))
	require.True(t, ok)
	require.Equal(t, uint32(os.Getppid()), ppid)

	_, ok = ppidFromStat(4000000000) // no such pid
	require.False(t, ok)
}

// SetWorkerScope with an empty name list clears the entry (no-mapping ⇒ suite).
func TestSetWorkerScopeEmptyClears(t *testing.T) {
	p := &Proxy{}
	p.SetWorkerScope(42, []string{"a"})
	p.SetWorkerScope(42, nil)
	p.workerScopeMu.RLock()
	_, present := p.workerScope[42]
	p.workerScopeMu.RUnlock()
	require.False(t, present, "empty allowlist clears the worker's scope")
}

// The wrapper embeds the MockMemDb INTERFACE, so anything a parser reaches for
// by type assertion but that is not in that interface is silently erased by the
// wrap. Every kind-aware parser then falls to a legacy branch — and in
// `keploy mock replay`, the only mode where worker scoping exists, the startup
// tier is the whole pool, so that branch reads nothing.
func TestScopedMockDb_DoesNotEraseTheManagersCapabilities(t *testing.T) {
	mgr := NewMockManager(NewTreeDb(customComparator), NewTreeDb(customComparator), zap.NewNop())
	defer mgr.Close()

	type revSrc interface{ Revision() uint64 }
	type revKind interface{ RevisionByKind(models.Kind) uint64 }
	type kindAware interface {
		GetFilteredMocksByKind(models.Kind) ([]*models.Mock, error)
		GetUnFilteredMocksByKind(models.Kind) ([]*models.Mock, error)
	}

	var scoped interface{} = &scopedMockDb{MockMemDb: mgr}
	if _, ok := scoped.(revSrc); !ok {
		t.Error("Revision erased by the wrap: consumers fall back to rebuilding on every call")
	}
	if _, ok := scoped.(revKind); !ok {
		t.Error("RevisionByKind erased by the wrap")
	}
	if _, ok := scoped.(kindAware); !ok {
		t.Error("the by-kind readers are erased by the wrap: kind-aware parsers take their " +
			"legacy branch, which cannot read the startup tier")
	}
	c, ok := scoped.(integrations.MockCursor)
	if !ok {
		t.Fatal("the stateful cursor is erased by the wrap: a scoped worker replays every " +
			"repeated request as its first recording")
	}
	// The wrap must reach the manager's cursor, not a private copy.
	c.AdvanceMockCursor("k", 0, 3)
	if got := mgr.MockCursorIndex("k", 3); got != 1 {
		t.Fatalf("cursor advanced through the wrap must be visible on the manager: got %d, want 1", got)
	}
}

// The startup tier is filtered like every other read tier. It sounds shared,
// but staging puts the whole per-test slice into it, and in `keploy mock replay`
// it is the entire pool — so leaving it unfiltered let one worker read, and
// consume, another worker's mocks.
func TestScopedMockDb_StartupTierIsScopedToTheWorker(t *testing.T) {
	mgr := NewMockManager(NewTreeDb(customComparator), NewTreeDb(customComparator), zap.NewNop())
	defer mgr.Close()

	a := newMockForTest("mock-A", time.Now().Add(-time.Hour), models.LifetimePerTest)
	b := newMockForTest("mock-B", time.Now().Add(-time.Hour), models.LifetimePerTest)
	mgr.SetMocksWithWindow([]*models.Mock{a, b}, nil, models.BaseTime, time.Now())

	workerA := workerScoped(t, mgr, []string{"mock-A"}, []string{"mock-A", "mock-B"})

	startup, err := workerA.GetStartupMocks()
	if err != nil {
		t.Fatalf("GetStartupMocks: %v", err)
	}
	if containsMockNamed(startup, "mock-B") {
		t.Fatal("worker A can see worker B's mock through the startup tier; it can also " +
			"consume it, deleting another worker's recording")
	}
	if !containsMockNamed(startup, "mock-A") {
		t.Fatal("worker A cannot see its own mock")
	}
}

// A scoped worker's keyed walk of the session tier sees what its
// GetSessionMocks sees, through the manager's index or, when the wrapped store
// has none, through a walk of that snapshot.
func TestScopedMockDb_KeyedSessionWalkIsScopedToTheWorker(t *testing.T) {
	base := time.Date(2026, 9, 30, 11, 0, 0, 0, time.UTC)
	var session []*models.Mock
	for i, n := range []string{"mine", "theirs", "shared", "mine-unkeyed"} {
		mk := newMockForTest(n, base.Add(time.Duration(i)*time.Millisecond), models.LifetimeSession)
		if n != "mine-unkeyed" {
			mk.Noise = []string{"k"}
		}
		session = append(session, mk)
	}
	mm := NewMockManager(nil, nil, zap.NewNop())
	defer mm.Close()
	mm.SetMocksWithWindow(nil, session, base, base.Add(time.Second))
	allow := []string{"mine", "mine-unkeyed"}
	universe := []string{"mine", "theirs", "mine-unkeyed"}

	walk := func(db integrations.SessionKeyReader) []string {
		var got []string
		require.NoError(t, db.RangeSessionMocksWithKey(keyedTestIndex, "k", func(mk *models.Mock) bool {
			got = append(got, mk.Name)
			return true
		}))
		return got
	}
	want := []string{"mine", "shared"}
	require.Equal(t, want, walk(workerScoped(t, mm, allow, universe)))
	// A wrapped store without the index: the filtered snapshot is walked.
	require.Equal(t, want, walk(workerScoped(t, &fakeMockDb{mocks: session}, allow, universe)))
}
