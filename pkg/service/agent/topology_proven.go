package agent

import (
	"sync"
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
// to a listener the run's own process tree owns (LIVE), relays one to a local
// dependency it does not record (LIVE, models.LiveRow.Relayed: a Redis the
// open-source keploy skips, a record's parsing kill switch, a record's
// parser that declined the connection or was retired before it recorded
// anything of it, whose mocked row is taken back), and
// records or mocks a call to any other local listener (mocked-local), and
// tells the wire (proxy.TopologySink), which names the test open at that
// moment on each row.
// The rows reach the CLI with the run's windows, each as a window of its own
// (models.ScopeWindow.Live, .Mocked), and its receipts carry them to keploy's
// summary.

// topologyRows are a session's rows, each once, in the order they were
// first seen. Guarded by Agent.provenMu, as part of provenWire.
type topologyRows struct {
	live    map[models.LiveRow]struct{}
	mocked  map[models.MockedRow]*mockedListing
	windows []models.ScopeWindow
}

// mockedListing is how many connections listed a mocked-local row: the row
// is taken back (unnoteMocked) only when every one of them has taken back
// its listing.
type mockedListing struct{ n int }

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
	a.noteLiveLocked(row)
}

// noteLiveLocked records row, its Test set, once. a.provenMu held, the wire
// not closed.
func (a *Agent) noteLiveLocked(row models.LiveRow) {
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
// the test open now (proxy.TopologySink), and returns how to take this
// listing back: a record's connection whose parser then declined it, or was
// retired before it recorded anything of it, recorded nothing, and is listed
// live instead. Taking it back lists asLive, the connection's LIVE row, for
// the test the mocked row names, so that test, whose calls on the
// connection went live, keeps a row however much later the take-back comes.
// The mocked row stays while another connection's listing of it stands; the
// last one taken back removes it, with its window. A listing of an earlier
// session (the wire was reset since), or one taken back once the run's
// windows were read, changes nothing. A zero asLive lists nothing.
func (a *Agent) NoteMocked(row models.MockedRow) (unnote func(asLive models.LiveRow)) {
	a.provenMu.Lock()
	defer a.provenMu.Unlock()
	if a.proven.closed {
		return func(models.LiveRow) {}
	}
	row.Test = a.proven.open
	t := &a.proven.topology
	l := t.mocked[row]
	if l == nil {
		if t.mocked == nil {
			t.mocked = map[models.MockedRow]*mockedListing{}
		}
		l = &mockedListing{}
		t.mocked[row] = l
		now := time.Now()
		t.windows = append(t.windows, models.ScopeWindow{Start: now, End: now, Mocked: &row})
	}
	l.n++
	var once sync.Once
	return func(asLive models.LiveRow) { once.Do(func() { a.unnoteMocked(row, l, asLive) }) }
}

// unnoteMocked takes back one listing l of row, and lists asLive for row's
// test (NoteMocked).
func (a *Agent) unnoteMocked(row models.MockedRow, l *mockedListing, asLive models.LiveRow) {
	a.provenMu.Lock()
	defer a.provenMu.Unlock()
	t := &a.proven.topology
	if a.proven.closed || t.mocked[row] != l {
		// Read already, or another session's rows.
		return
	}
	if asLive != (models.LiveRow{}) {
		asLive.Test = row.Test
		a.noteLiveLocked(asLive)
	}
	if l.n--; l.n > 0 {
		return
	}
	delete(t.mocked, row)
	for i := len(t.windows) - 1; i >= 0; i-- {
		if w := t.windows[i]; w.Mocked != nil && *w.Mocked == row {
			t.windows = append(t.windows[:i:i], t.windows[i+1:]...)
			break
		}
	}
}
