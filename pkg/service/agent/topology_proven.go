package agent

import (
	"time"

	proxyPkg "go.keploy.io/server/v3/pkg/agent/proxy"
	"go.keploy.io/server/v3/pkg/models"
)

// topologySinkSetter is implemented by a proxy that tells where the run's
// calls to local listeners went (proxy.Proxy).
type topologySinkSetter interface {
	SetTopologySink(proxyPkg.TopologySink)
}

var _ proxyPkg.TopologySink = (*Agent)(nil)

// Where the run's calls to local listeners went, under the proven switch
// (design v14 §3.1; release1-cut §5.1 items 3 and 8): the proxy relays a call
// to a listener the run's own process tree owns (LIVE) and records or mocks
// a call to any other local listener (mocked-local), and tells the wire
// (proxy.TopologySink), which names the test open at that moment on each row.
// The rows reach the CLI with the run's windows, each as a window of its own
// (models.ScopeWindow.Live, .Mocked), and its receipts carry them to keploy's
// summary.

// topologyRows are a session's rows, each once, in the order they were
// first seen. Guarded by Agent.provenMu, as part of provenWire.
type topologyRows struct {
	live    map[models.LiveRow]struct{}
	mocked  map[models.MockedRow]struct{}
	windows []models.ScopeWindow
}

// NoteLive records a LIVE call for the test open now (proxy.TopologySink).
// It is called for each read of a LIVE caller's bytes, so it only looks the
// row up when it has been seen.
func (a *Agent) NoteLive(row models.LiveRow) {
	a.provenMu.Lock()
	defer a.provenMu.Unlock()
	if a.proven.closed {
		// The run's windows were read at its end: nothing more is.
		return
	}
	row.Test = a.proven.open
	t := &a.proven.topology
	if _, seen := t.live[row]; seen {
		return
	}
	if t.live == nil {
		t.live = map[models.LiveRow]struct{}{}
	}
	t.live[row] = struct{}{}
	now := time.Now()
	t.windows = append(t.windows, models.ScopeWindow{Start: now, End: now, Live: &row})
}

// NoteMocked records a call to a local listener outside the run's tree for
// the test open now (proxy.TopologySink).
func (a *Agent) NoteMocked(row models.MockedRow) {
	a.provenMu.Lock()
	defer a.provenMu.Unlock()
	if a.proven.closed {
		return
	}
	row.Test = a.proven.open
	t := &a.proven.topology
	if _, seen := t.mocked[row]; seen {
		return
	}
	if t.mocked == nil {
		t.mocked = map[models.MockedRow]struct{}{}
	}
	t.mocked[row] = struct{}{}
	now := time.Now()
	t.windows = append(t.windows, models.ScopeWindow{Start: now, End: now, Mocked: &row})
}
