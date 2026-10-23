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

// A mocked-local row is taken back (a record's connection whose parser then
// declined it, or was retired before it recorded anything, listed live
// instead) only when every connection that listed it has taken its listing
// back: then its window goes too. Each take-back lists the connection's LIVE
// row for the test the mocked row named, even when another test is open by
// then. Taking one back twice counts once; one of an earlier session, or one
// taken back after the run's windows were read, changes nothing; a zero LIVE
// row lists nothing.
func TestAMockedRowIsTakenBackWithItsLastListing(t *testing.T) {
	db := models.MockedRow{Addr: "127.0.0.1:5432", Port: 5432}
	cache := models.MockedRow{Addr: "127.0.0.1:11211", Port: 11211}
	dbLive := models.LiveRow{Exe: "/usr/bin/postgres", Port: 5432, Relayed: "parser_retired"}

	a, _ := wireAgent(t, models.MODE_RECORD)
	first, second := a.NoteMocked(db), a.NoteMocked(db)
	a.NoteMocked(cache)
	first(models.LiveRow{})
	first(dbLive)
	live, mocked := topologyOf(t, a)
	require.Equal(t, []models.MockedRow{db, cache}, mocked, "another connection still lists the row")
	require.Empty(t, live, "a take-back with no LIVE row lists none, and a second take-back counts nothing")

	a, _ = wireAgent(t, models.MODE_RECORD)
	first, second = a.NoteMocked(db), a.NoteMocked(db)
	a.NoteMocked(cache)
	first(dbLive)
	second(dbLive)
	live, mocked = topologyOf(t, a)
	require.Equal(t, []models.MockedRow{cache}, mocked, "every listing of the row was taken back")
	require.Equal(t, []models.LiveRow{dbLive}, live, "the take-backs list the LIVE row, once for the test")

	// The LIVE row goes to the test the mocked row named, not to the test
	// open when it is taken back.
	a, _ = wireAgent(t, models.MODE_RECORD)
	ctx := context.Background()
	claimSelf(t, a)
	_, serr := a.ProvenBegin(ctx, begin("TestA"))
	require.Nil(t, serr)
	inA := a.NoteMocked(db)
	_, serr = a.ProvenEnd(ctx, models.ScopeReq{Name: "TestA", Outcome: models.ScopeOutcomePassed})
	require.Nil(t, serr)
	_, serr = a.ProvenBegin(ctx, begin("TestB"))
	require.Nil(t, serr)
	inA(dbLive)
	_, serr = a.ProvenEnd(ctx, models.ScopeReq{Name: "TestB", Outcome: models.ScopeOutcomePassed})
	require.Nil(t, serr)
	live, mocked = topologyOf(t, a)
	require.Empty(t, mocked)
	want := dbLive
	want.Test = "o::TestA"
	require.Equal(t, []models.LiveRow{want}, live, "the LIVE row names the test the mocked row named")

	// A listing made before the run's windows were read and taken back after
	// it: the agent's later read says what the first said.
	a, _ = wireAgent(t, models.MODE_RECORD)
	late := a.NoteMocked(db)
	_, mocked = topologyOf(t, a)
	require.Equal(t, []models.MockedRow{db}, mocked)
	late(dbLive)
	live, mocked = topologyOf(t, a)
	require.Equal(t, []models.MockedRow{db}, mocked, "a listing taken back once the windows were read changed what they say")
	require.Empty(t, live)

	a, _ = wireAgent(t, models.MODE_RECORD)
	stale := a.NoteMocked(db)
	require.NoError(t, a.resetSession(context.Background()))
	a.NoteMocked(db)
	stale(dbLive)
	live, mocked = topologyOf(t, a)
	require.Equal(t, []models.MockedRow{db}, mocked, "a listing of the session before is not this session's")
	require.Empty(t, live)
	_ = second
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
