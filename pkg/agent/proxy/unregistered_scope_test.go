package proxy

import (
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/agent/starts"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// These pin how a call from a process tree no scope registered is served: a
// helper the CI starts beside the test binary rather than under it
// (`./keploy-agent ... &`), which the test hands work to. This test process
// plays the helper: no process on its /proc chain begins a scope. The test
// binary is a pid with no /proc entry, registered only by its scope calls.

const (
	testBinary  = uint32(3999901)
	otherWorker = uint32(3999902)
)

// unregisteredPool is a pool of two tests' recordings and a shared one, with
// the replay's scope table installed and the starts registry clean.
func unregisteredPool(t *testing.T) (*Proxy, *MockManager) {
	t.Helper()
	starts.Default.Reset()
	t.Cleanup(starts.Default.Reset)
	mgr := NewMockManager(NewTreeDb(customComparator), NewTreeDb(customComparator), zap.NewNop())
	t.Cleanup(mgr.Close)
	at := time.Now().Add(-time.Hour)
	mgr.SetMocksWithWindow(nil, []*models.Mock{
		newMockForTest("a1", at, models.LifetimeSession),
		newMockForTest("b1", at, models.LifetimeSession),
		newMockForTest("shared", at, models.LifetimeSession),
	}, models.BaseTime, time.Now())
	p := &Proxy{}
	p.SetMappedUniverse([]string{"a1", "b1"})
	return p, mgr
}

func sessionNames(t *testing.T, v integrations.MockMemDb) []string {
	t.Helper()
	got, err := v.GetSessionMocks()
	require.NoError(t, err)
	return scopedNames(got)
}

// begin and end are what HandleScopeBegin/End and the agent service do for a
// worker's test with a mapping: the starts registry first, then the worker's
// scope (SetWorkerScope / ClearWorkerScope).
func begin(p *Proxy, pid uint32, test string, allow ...string) {
	starts.Default.Begin(pid, test, "", false, time.Now())
	p.SetWorkerScope(pid, allow)
}

func end(p *Proxy, pid uint32, test string) {
	starts.Default.End(pid, test, false, time.Now())
	p.ClearWorkerScope(pid)
}

// (a) While one test is open, the helper's connection is served as that
// test's worker's, and as it outlives the test it follows the run: the whole
// pool between tests, the next test's recordings in the next test, with a new
// revision at each change, whether the change was to a worker's scope or only
// to which tests are open.
func TestUnregisteredProcess_ServedAsTheOneOpenTest(t *testing.T) {
	p, mgr := unregisteredPool(t)
	self := uint32(os.Getpid())
	starts.Default.Begin(testBinary, "suite", "", true, time.Now())
	begin(p, testBinary, "TestA", "a1")

	conn, ok := p.scopedFor(self, mgr).(*scopedMockDb)
	require.True(t, ok, "a call of no registered process during the one open test is scoped")
	require.ElementsMatch(t, []string{"a1", "shared"}, sessionNames(t, conn), "served TestA's recordings, not TestB's")
	require.True(t, conn.OwnedByOpenTest(&models.Mock{Name: "a1"}), "TestA's recording is the open test's own")
	require.Equal(t, p.workerViewFor([]uint32{testBinary}).scope, conn.current().scope, "its scope is the test's")
	rev := conn.Revision()

	end(p, testBinary, "TestA")
	require.ElementsMatch(t, []string{"a1", "b1", "shared"}, sessionNames(t, conn), "between tests: the whole pool")
	between := conn.Revision()
	require.Greater(t, between, rev)

	begin(p, testBinary, "TestB", "b1")
	require.ElementsMatch(t, []string{"b1", "shared"}, sessionNames(t, conn), "the next test's recordings")
	inB := conn.Revision()
	require.Greater(t, inB, between)

	// Another worker begins a test with no mapping of its own: no worker's
	// scope changes, but two tests are now open, so the helper is served the
	// whole pool, under a new revision.
	starts.Default.Begin(otherWorker, "TestC", "", false, time.Now())
	require.ElementsMatch(t, []string{"a1", "b1", "shared"}, sessionNames(t, conn), "two tests open: the whole pool")
	require.Greater(t, conn.Revision(), inB, "a change to which tests are open is a new revision")
	require.Equal(t, conn.Revision(), conn.Revision())
}

// (b) With no test open — the suite open before the first, or between two —
// the helper's call is served as before: the bare manager, the whole pool,
// even while a worker's scope is still registered.
func TestUnregisteredProcess_NoTestOpenServedAsBefore(t *testing.T) {
	p, mgr := unregisteredPool(t)
	self := uint32(os.Getpid())
	starts.Default.Begin(testBinary, "suite", "", true, time.Now())
	p.SetWorkerScope(testBinary, []string{"a1"}) // a scope the registry has no open test for
	require.Equal(t, integrations.MockMemDb(mgr), p.scopedFor(self, mgr), "before the first test: the whole pool")

	starts.Default.Begin(testBinary, "TestA", "", false, time.Now())
	starts.Default.End(testBinary, "TestA", false, time.Now())
	require.Equal(t, integrations.MockMemDb(mgr), p.scopedFor(self, mgr), "between tests: the whole pool")
}

// (c) With two tests open at once — on two workers, or two parallel tests of
// one process — which test the helper's call is of would be a guess: it is
// served as before, the whole pool.
func TestUnregisteredProcess_TestsOpenInParallelServedAsBefore(t *testing.T) {
	p, mgr := unregisteredPool(t)
	self := uint32(os.Getpid())
	begin(p, testBinary, "TestA", "a1")
	begin(p, otherWorker, "TestB", "b1")
	require.Equal(t, integrations.MockMemDb(mgr), p.scopedFor(self, mgr), "two workers' tests open: the whole pool")

	p, mgr = unregisteredPool(t)
	begin(p, testBinary, "TestA", "a1")
	starts.Default.Begin(testBinary, "TestB", "", false, time.Now()) // t.Parallel in one process
	require.Equal(t, integrations.MockMemDb(mgr), p.scopedFor(self, mgr), "two parallel tests of one process open: the whole pool")

	// A subtest open inside its test is one test running.
	p, mgr = unregisteredPool(t)
	begin(p, testBinary, "TestA", "a1")
	starts.Default.Begin(testBinary, "TestA/case", "", false, time.Now())
	conn, ok := p.scopedFor(self, mgr).(*scopedMockDb)
	require.True(t, ok, "a subtest open in its test is the one open test")
	require.ElementsMatch(t, []string{"a1", "shared"}, sessionNames(t, conn))
}

// A process a scope registered is served by its own scope, as before: one
// the starts registry knows (it began a scope with no mapping), and one whose
// worker scope ended, see the whole pool while another worker's test is open.
func TestUnregisteredProcess_RegisteredProcessesAreUnaffected(t *testing.T) {
	p, mgr := unregisteredPool(t)
	self := uint32(os.Getpid())
	starts.Default.Begin(self, "suite", "", true, time.Now())
	begin(p, testBinary, "TestA", "a1")
	require.Equal(t, integrations.MockMemDb(mgr), p.scopedFor(self, mgr), "a registered worker with no test open keeps the whole pool")

	p, mgr = unregisteredPool(t)
	p.SetWorkerScope(self, []string{"b1"})
	conn := p.scopedFor(self, mgr).(*scopedMockDb)
	p.ClearWorkerScope(self)
	begin(p, testBinary, "TestA", "a1")
	require.ElementsMatch(t, []string{"a1", "b1", "shared"}, sessionNames(t, conn), "a worker whose test ended keeps the whole pool")
}

// (a), per-start table (keploy mock replay of a set whose mapping lists its
// suites): a connection of no registered process is served the one open
// test's recordings, its stateful cursor walking them from the first, as the
// test binary's own connection would be.
func TestUnregisteredProcess_LiveServedAsTheOneOpenTest(t *testing.T) {
	mgr := livePool(t)
	self := uint32(os.Getpid())
	p := &Proxy{}
	starts.Default.Begin(testBinary, "suite", "", true, time.Now())
	starts.Default.Begin(testBinary, "t1", "", false, time.Now())
	starts.Default.End(testBinary, "t1", false, time.Now())
	starts.Default.Begin(testBinary, "t2", "", false, time.Now())
	if got, want := readCounter(t, p.scopedFor(self, mgr), 3), "b1,b2,b2"; got != want {
		t.Fatalf("during t2 the helper was served %s, want t2's %s", got, want)
	}
}

// (b)/(c), per-start table: with no test open, or two, the helper's
// connection has no view: every recording is served, t1's first.
func TestUnregisteredProcess_LiveNoTestOrTwoServedAsBefore(t *testing.T) {
	mgr := livePool(t)
	self := uint32(os.Getpid())
	p := &Proxy{}
	starts.Default.Begin(testBinary, "suite", "", true, time.Now())
	if got, want := readCounter(t, p.scopedFor(self, mgr), 3), "a1,a2,b1"; got != want {
		t.Fatalf("before the first test the helper was served %s, want the pool's %s", got, want)
	}

	mgr = livePool(t)
	starts.Default.Begin(testBinary, "t2", "", false, time.Now())
	starts.Default.Begin(otherWorker, "t1", "", false, time.Now())
	if got, want := readCounter(t, p.scopedFor(self, mgr), 3), "a1,a2,b1"; got != want {
		t.Fatalf("with two tests open the helper was served %s, want the pool's %s", got, want)
	}
}
