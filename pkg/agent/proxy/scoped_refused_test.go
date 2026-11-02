package proxy

import (
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	httpint "go.keploy.io/server/v3/pkg/agent/proxy/integrations/http"
	"go.keploy.io/server/v3/pkg/agent/starts"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// A worker whose test keploy refused at its begin (a pid begin without the
// marks) is refused every read, naming why, until its test ends: a harness
// that ignores the refusal and runs the test is never served the whole
// pool. The nearest worker on a connection's chain decides, so a refused
// worker is refused even when an ancestor has a test open; a view made in
// the worker's earlier test follows it into the refusal.
func TestAReadOfARefusedWorkerIsRefusedUntilItsTestEnds(t *testing.T) {
	starts.Default.Reset()
	t.Cleanup(starts.Default.Reset)
	mgr := newCursorManager(t)
	base := time.Now().Add(-time.Hour)
	mgr.SetMocksWithWindow(nil, append(counterPool(base, "a1", "a2"), counterPool(base.Add(time.Minute), "b1", "b2")...), models.BaseTime, time.Now())
	p := &Proxy{}
	p.SetMappedUniverse([]string{"a1", "a2", "b1", "b2"})
	self, parent := uint32(os.Getpid()), uint32(os.Getppid())
	p.SetWorkerScope(parent, []string{"b1", "b2"})
	p.SetWorkerScope(self, []string{"a1", "a2"})
	earlier := p.scopedFor(self, mgr)
	const why = "t1 began with the pid 42: keploy narrows a test by its pid only with the marks"
	p.RefuseWorker(self, why)
	opened := p.scopedFor(self, mgr)
	if opened == integrations.MockMemDb(mgr) {
		t.Fatal("a connection of the refused worker got the bare manager, which serves every test's mocks")
	}
	for name, conn := range map[string]integrations.MockMemDb{"made in its earlier test": earlier, "opened while refused": opened} {
		for read, err := range map[string]error{
			"GetPerTestMocksInWindow":   errOfRead(conn.GetPerTestMocksInWindow()),
			"GetSessionScopedMocks":     errOfRead(conn.GetSessionScopedMocks()),
			"GetStartupMocks":           errOfRead(conn.GetStartupMocks()),
			"RangePerTestMocksInWindow": conn.(integrations.PerTestRanger).RangePerTestMocksInWindow(func(*models.Mock) bool { return true }),
			"RangeSessionMocksWithKey":  liveKeyedErr(conn),
		} {
			if err == nil {
				t.Fatalf("a view %s: %s served the refused worker", name, read)
			}
			if !strings.Contains(err.Error(), why) || !strings.Contains(err.Error(), "refused at its begin") {
				t.Fatalf("a view %s: %s refused it with %q, which does not say why", name, read, err)
			}
		}
		if resp, body, _ := serveOne(t, httpint.New(zap.NewNop()), conn, "GET /counter HTTP/1.1\r\nHost: api\r\n\r\n"); resp != nil {
			t.Fatalf("a view %s: the refused worker's call was served %s", name, body)
		}
	}
	// A refused worker is refused as the first worker a proxy registers.
	first := &Proxy{}
	first.RefuseWorker(self, why)
	if err := errOfRead(first.scopedFor(self, mgr).GetStartupMocks()); err == nil || !strings.Contains(err.Error(), why) {
		t.Fatalf("the first worker registered, refused, was read with %v", err)
	}
	// Its end gives the reads back: the nearest registered worker is now the
	// ancestor, whose test is open.
	p.ClearWorkerScope(self)
	if got, want := readCounter(t, opened, 2), "b1,b2"; got != want {
		t.Fatalf("after the refused test ended the connection served %s, want %s", got, want)
	}
}

// With the marks, a keyed walk of the session tier (MySQL's lookups by
// statement) with no test open, at boot or between two tests, shows what the
// connection's read of that tier shows there.
func TestALiveKeyedSessionWalkWithNoTestOpenShowsWhatItsReadsShow(t *testing.T) {
	mgr := livePool(t)
	pid := uint32(os.Getpid())
	conn := (&Proxy{}).scopedFor(pid, mgr)
	read := func() string {
		// The tier the keyed walk indexes.
		mocks, err := conn.GetSessionMocks() //nolint:staticcheck // SA1019: the session tier is what RangeSessionMocksWithKey walks
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, mk := range mocks {
			names = append(names, mk.Name)
		}
		return sortedNames(strings.Join(names, ","))
	}
	if got, want := sortedNames(liveKeyedNames(t, conn)), read(); got != want || want != "a1,a2,b1,b2" {
		t.Fatalf("at boot a keyed walk showed %s, and the session tier %s; want a1,a2,b1,b2 both", got, want)
	}
	starts.Default.Begin(pid, "t2", "", false, time.Now())
	starts.Default.End(pid, "t2", false, time.Now())
	if got, want := sortedNames(liveKeyedNames(t, conn)), read(); got != want {
		t.Fatalf("between tests a keyed walk showed %s, and the session tier %s", got, want)
	}
}

// With the marks, a walk of the per-test tier (MySQL's per-test lookup)
// shows a connection the open test's recordings only, as its other reads do,
// not every test's.
func TestALivePerTestWalkShowsOnlyTheOpenTestsMocks(t *testing.T) {
	mgr := newCursorManager(t)
	base := time.Now().Add(-time.Hour)
	mgr.SetScopeMocks(append(counterPool(base, "a1", "a2"), counterPool(base.Add(time.Minute), "b1", "b2")...), nil, true)
	starts.Default.SetTable("", liveSets())
	t.Cleanup(starts.Default.Reset)
	pid := uint32(os.Getpid())
	conn := (&Proxy{}).scopedFor(pid, mgr)
	starts.Default.Begin(pid, "t2", "", false, time.Now())
	var names []string
	if err := conn.(integrations.PerTestRanger).RangePerTestMocksInWindow(func(mk *models.Mock) bool {
		names = append(names, mk.Name)
		return true
	}); err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(names, ","), "b1,b2"; got != want {
		t.Fatalf("during t2 a per-test walk showed %s, want %s", got, want)
	}
}

func sortedNames(joined string) string {
	names := strings.Split(joined, ",")
	slices.Sort(names)
	return strings.Join(names, ",")
}
