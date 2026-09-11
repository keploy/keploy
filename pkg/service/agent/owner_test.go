// resolveOwner is the record-time half of a mock's (owner, n) identity: it names
// the per-test scope that owned a capture, at the instant the capture leaves the
// agent. These tests pin the rule it implements -- innermost window wins, an
// open window has no end, a same-worker window beats another worker's -- and,
// most importantly, that a run with no scopes at all resolves to "", which is
// what keeps `keploy record` on its original naming path.
package agent

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

func at(sec int) time.Time { return time.Unix(1_700_000_000+int64(sec), 0).UTC() }

// `keploy record` (integration testing) never opens a scope. Every capture must
// therefore resolve to the unowned namespace, which routes it to the unchanged
// mock-N mint.
func TestResolveOwnerNoScopesIsUnowned(t *testing.T) {
	a := &Agent{logger: zap.NewNop()}
	require.Equal(t, "", a.resolveOwner(0, at(10)))
	require.Equal(t, "", a.resolveOwner(4242, at(10)))
}

// A capture with no request timestamp cannot be placed in any window.
func TestResolveOwnerZeroTimestampIsUnowned(t *testing.T) {
	a := &Agent{logger: zap.NewNop()}
	a.scopeWindows = []models.ScopeWindow{{Name: "t", Start: at(0), End: at(100)}}
	require.Equal(t, "", a.resolveOwner(0, time.Time{}))
}

// The common case: the mock is emitted while its own test is still running, so
// the window has a start and no end yet. correlateScopes cannot see this at all
// -- it only ever runs over closed windows.
func TestResolveOwnerOpenWindowHasNoEnd(t *testing.T) {
	a := &Agent{logger: zap.NewNop()}
	a.workerOpen = map[scopeKey]time.Time{{pid: 0, name: "running"}: at(5)}
	require.Equal(t, "running", a.resolveOwner(0, at(9999)),
		"an open window extends to +infinity")
	require.Equal(t, "", a.resolveOwner(0, at(4)),
		"a capture made before the window opened is not owned by it")
}

// The Playwright fixture opens a worker-scoped __suite__ window that encloses
// every per-test window. A test's own call must land on the test, not on the
// enclosing suite scope.
func TestResolveOwnerInnermostWins(t *testing.T) {
	a := &Agent{logger: zap.NewNop()}
	a.workerOpen = map[scopeKey]time.Time{
		{pid: 0, name: "__suite__"}: at(0),
		{pid: 0, name: "test-b"}:    at(20),
	}
	a.scopeWindows = []models.ScopeWindow{{Name: "test-a", Start: at(5), End: at(15)}}

	require.Equal(t, "test-a", a.resolveOwner(0, at(10)), "closed inner window wins over the open suite")
	require.Equal(t, "test-b", a.resolveOwner(0, at(25)), "open inner window wins over the open suite")
	require.Equal(t, "__suite__", a.resolveOwner(0, at(17)),
		"between the two tests only the suite window contains the capture")
}

// Overlapping windows from parallel workers must not steal each other's mocks.
func TestResolveOwnerPrefersSameWorker(t *testing.T) {
	a := &Agent{logger: zap.NewNop()}
	a.scopeWindows = []models.ScopeWindow{
		{Name: "worker-100", Start: at(0), End: at(100), PID: 100},
		{Name: "worker-200", Start: at(10), End: at(100), PID: 200},
	}
	// By start time alone worker-200's window is innermost and would win.
	require.Equal(t, "worker-100", a.resolveOwner(100, at(50)))
	require.Equal(t, "worker-200", a.resolveOwner(200, at(50)))
	// A PID-less capture falls back to the pure timestamp scan.
	require.Equal(t, "worker-200", a.resolveOwner(0, at(50)))
	// A PID that matches no window falls back too, rather than going unowned.
	require.Equal(t, "worker-200", a.resolveOwner(999, at(50)))
}

// The window is inclusive at both ends, matching correlateScopes.
func TestResolveOwnerBoundsAreInclusive(t *testing.T) {
	a := &Agent{logger: zap.NewNop()}
	a.scopeWindows = []models.ScopeWindow{{Name: "t", Start: at(10), End: at(20)}}
	require.Equal(t, "t", a.resolveOwner(0, at(10)))
	require.Equal(t, "t", a.resolveOwner(0, at(20)))
	require.Equal(t, "", a.resolveOwner(0, at(21)))
}

// End to end through the public scope API, which is how a runner drives this.
func TestResolveOwnerThroughBeginEndScope(t *testing.T) {
	a := newRecordAgent()
	ctx := t.Context()

	_, err := a.BeginScope(ctx, "suite", 0)
	require.NoError(t, err)
	duringSuite := time.Now()

	_, err = a.BeginScope(ctx, "test-1", 0)
	require.NoError(t, err)
	duringTest := time.Now()
	require.Equal(t, "test-1", a.resolveOwner(0, duringTest),
		"while test-1 is open its own capture belongs to it, not the suite")
	require.NoError(t, a.EndScope(ctx, "test-1", 0))

	require.Equal(t, "suite", a.resolveOwner(0, duringSuite))
	require.Equal(t, "test-1", a.resolveOwner(0, duringTest),
		"the answer must not change once the window closes")
}
