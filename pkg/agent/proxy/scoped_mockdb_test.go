package proxy

import (
	"os"
	"runtime"
	"testing"

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

func TestScopedMockDbFiltersReads(t *testing.T) {
	// m1,m2,m3 belong to tests (in the universe); "shared" belongs to no test.
	db := &fakeMockDb{mocks: []*models.Mock{{Name: "m1"}, {Name: "m2"}, {Name: "m3"}, {Name: "shared"}}}
	universe := map[string]struct{}{"m1": {}, "m2": {}, "m3": {}}

	// Worker allowed m1,m3: it sees m1,m3 (its test) + "shared" (unmapped), but
	// NOT m2 (another test's mock). Applies to per-test AND session tiers.
	s := &scopedMockDb{MockMemDb: db, allow: map[string]struct{}{"m1": {}, "m3": {}}, universe: universe}
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
	pass := &scopedMockDb{MockMemDb: db, allow: map[string]struct{}{"m1": {}}, universe: nil}
	got, err := pass.GetSessionMocks()
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"m1", "m2", "m3", "shared"}, scopedNames(got))
}

func TestScopedForResolvesWorkerAndFallsBack(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("scopedFor's process-tree walk reads /proc (linux only)")
	}
	db := &fakeMockDb{}
	self := uint32(os.Getpid())

	// Empty registry: fast path, no /proc walk, returns the bare manager.
	p := &Proxy{}
	require.Equal(t, integrations.MockMemDb(db), p.scopedFor(self, db), "no scopes ⇒ whole pool")

	// A registered worker PID: the call is scoped to its allowlist.
	p.SetWorkerScope(self, []string{"only-this"})
	require.IsType(t, &scopedMockDb{}, p.scopedFor(self, db), "own PID resolves to a scoped view")

	// PID 0 (unknown origin) and an unregistered/dead PID ⇒ whole pool.
	require.Equal(t, integrations.MockMemDb(db), p.scopedFor(0, db))
	require.Equal(t, integrations.MockMemDb(db), p.scopedFor(4000000000, db))

	// Cleared ⇒ back to the whole pool.
	p.ClearWorkerScope(self)
	require.Equal(t, integrations.MockMemDb(db), p.scopedFor(self, db))
}

func TestScopedForWalksUpProcessTree(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("process-tree walk is linux only")
	}
	db := &fakeMockDb{}
	// Register the PARENT of this process; this process's call must resolve up to
	// it (models a worker whose child/grandchild opened the socket).
	p := &Proxy{}
	p.SetWorkerScope(uint32(os.Getppid()), []string{"m"})
	require.IsType(t, &scopedMockDb{}, p.scopedFor(uint32(os.Getpid()), db),
		"a call from a descendant resolves up to the registered worker")

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
