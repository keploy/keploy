//go:build linux

package agent

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	coreAgent "go.keploy.io/server/v3/pkg/agent"
	proxyPkg "go.keploy.io/server/v3/pkg/agent/proxy"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// topologyOf is the LIVE and mocked-local rows among a run's final windows.
// Each is a window of its own, which names no test and no claim.
func topologyOf(t *testing.T, a *Agent) ([]models.LiveRow, []models.MockedRow) {
	t.Helper()
	ws, err := a.FinalScopeWindows(context.Background())
	require.NoError(t, err)
	var live []models.LiveRow
	var mocked []models.MockedRow
	for _, w := range ws {
		switch {
		case w.Live != nil:
			require.Empty(t, w.Name, "a row's window is no test's")
			require.Empty(t, w.Claim, "a row's window is no claim")
			require.Nil(t, w.Mocked, "a row's window carries one row")
			live = append(live, *w.Live)
		case w.Mocked != nil:
			require.Empty(t, w.Name, "a row's window is no test's")
			require.Empty(t, w.Claim, "a row's window is no claim")
			mocked = append(mocked, *w.Mocked)
		}
	}
	return live, mocked
}

// Each row names the test open when the call was made, or none outside every
// test; a row seen again for the same test is listed once, and again for
// another test.
func TestARowNamesTheTestOpenWhenTheCallWasMade(t *testing.T) {
	for _, mode := range []models.Mode{models.MODE_RECORD, models.MODE_TEST} {
		t.Run(string(mode), func(t *testing.T) {
			a, _ := wireAgent(t, mode)
			ctx := context.Background()
			app := models.LiveRow{Exe: "/usr/bin/node", Port: 3000}
			db := models.MockedRow{Addr: "127.0.0.1:5432", PID: 42, Exe: "/usr/lib/postgresql/16/bin/postgres", Port: 5432}

			a.NoteLive(app) // before any claim: outside every test
			claimSelf(t, a)
			_, serr := a.ProvenBegin(ctx, begin("TestA"))
			require.Nil(t, serr)
			for range 3 {
				a.NoteLive(app)
			}
			a.NoteMocked(db)
			_, serr = a.ProvenEnd(ctx, models.ScopeReq{Name: "TestA", Outcome: models.ScopeOutcomePassed})
			require.Nil(t, serr)
			a.NoteLive(app) // between tests: seen outside every test already
			_, serr = a.ProvenBegin(ctx, begin("TestB"))
			require.Nil(t, serr)
			a.NoteLive(app)
			_, serr = a.ProvenEnd(ctx, models.ScopeReq{Name: "TestB", Outcome: models.ScopeOutcomePassed})
			require.Nil(t, serr)

			live, mocked := topologyOf(t, a)
			require.Equal(t, []models.LiveRow{
				{Exe: "/usr/bin/node", Port: 3000},
				{Test: "o::TestA", Exe: "/usr/bin/node", Port: 3000},
				{Test: "o::TestB", Exe: "/usr/bin/node", Port: 3000},
			}, live)
			require.Equal(t, []models.MockedRow{
				{Test: "o::TestA", Addr: "127.0.0.1:5432", PID: 42, Exe: "/usr/lib/postgresql/16/bin/postgres", Port: 5432},
			}, mocked)
		})
	}
}

// The run's windows are read once, at its end: a row seen after that would
// never be read, and is not kept.
func TestARowAfterTheFinalReadIsNotKept(t *testing.T) {
	a, _ := wireAgent(t, models.MODE_TEST)
	live, _ := topologyOf(t, a)
	require.Empty(t, live)
	a.NoteLive(models.LiveRow{Exe: "/app", Port: 8080})
	a.NoteMocked(models.MockedRow{Addr: "127.0.0.1:6379", Port: 6379})
	a.provenMu.Lock()
	require.Empty(t, a.proven.topology.windows)
	a.provenMu.Unlock()
}

// The rows belong to the session that saw them.
func TestRowsResetWithTheSession(t *testing.T) {
	a, _ := wireAgent(t, models.MODE_TEST)
	a.NoteLive(models.LiveRow{Exe: "/app", Port: 8080})
	a.NoteMocked(models.MockedRow{Addr: "127.0.0.1:6379", Port: 6379})
	require.NoError(t, a.resetSession(context.Background()))
	live, mocked := topologyOf(t, a)
	require.Empty(t, live)
	require.Empty(t, mocked)
}

// sinkProxy is a proxy that takes a topology sink.
type sinkProxy struct {
	coreAgent.Proxy
	sink proxyPkg.TopologySink
}

func (p *sinkProxy) SetTopologySink(s proxyPkg.TopologySink) { p.sink = s }

// The agent is the sink of its proxy's rows.
func TestTheAgentIsItsProxysTopologySink(t *testing.T) {
	prev := coreAgent.ActiveIncomingProxy
	t.Cleanup(func() { coreAgent.ActiveIncomingProxy = prev })
	p := &sinkProxy{}
	a := New(zap.NewNop(), nil, p, nil, nil, nil)
	require.Same(t, a, p.sink)
}
