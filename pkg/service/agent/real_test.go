//go:build linux

package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/agent/realdeps"
	"go.keploy.io/server/v3/pkg/models"
)

// realProxy is the wire's proxy with mock.real's bookkeeping.
type realProxy struct {
	*wireProxy
	tracker realdeps.Tracker
	set     []string
}

func (p *realProxy) RealTracker() *realdeps.Tracker { return &p.tracker }

func (p *realProxy) SetRealDeps(_ context.Context, real []string) error {
	p.set = real
	p.tracker.Reset()
	return nil
}

func (p *realProxy) RealRun() models.RealRun { return p.tracker.Run() }

func realAgent(t *testing.T, mode models.Mode) (*Agent, *realProxy) {
	a, w := wireAgent(t, mode)
	p := &realProxy{wireProxy: w}
	a.Proxy = p
	return a, p
}

// A replayed test's REAL rows come from the bytes its connections sent while
// it was open, and a connection keploy could not capture fails it.
func TestAReplayedTestCarriesItsRealRowsAndFailsOnWhatKeployCouldNotCapture(t *testing.T) {
	a, p := realAgent(t, models.MODE_TEST)
	ctx := context.Background()
	claimSelf(t, a)
	_, serr := a.ProvenBegin(ctx, begin("TestA"))
	require.Nil(t, serr)
	p.tracker.Dialed(1, "db.example.com:5432", true)
	p.tracker.Sent(1, "db.example.com:5432")
	p.tracker.Incomplete("10.0.0.7:443", "keploy could not read which name it was opened for")
	e, serr := a.ProvenEnd(ctx, models.ScopeReq{Name: "TestA", Outcome: "passed"})
	require.Nil(t, serr)
	require.Equal(t, models.ScopeActionFail, e.Action)
	require.Contains(t, e.Reason, "could not capture a connection to 10.0.0.7:443")
	require.False(t, strings.Contains(e.Reason, "re-record"), "nothing the test did needs re-recording: %s", e.Reason)

	w := windowOf(t, a, "o::TestA")
	require.Equal(t, models.ScopeOutcomeFailed, w.Outcome)
	require.Equal(t, []models.RealUse{{Dest: "db.example.com:5432", Conns: 1}}, w.Real)
	require.Len(t, w.Evidence.Incomplete, 1)
	require.Equal(t, models.CaptureIncompleteRealName, w.Evidence.Incomplete[0].Class)
}

// A record never fails a test: what keploy could not capture is kept for
// judging, and the test keeps its own outcome.
func TestARecordedTestKeepsItsOutcomeAndItsUncapturedEvidence(t *testing.T) {
	a, p := realAgent(t, models.MODE_RECORD)
	ctx := context.Background()
	claimSelf(t, a)
	_, serr := a.ProvenBegin(ctx, begin("TestA"))
	require.Nil(t, serr)
	p.tracker.Dialed(1, "auth.example.com:443", false)
	p.tracker.Incomplete("10.0.0.7:443", "unreadable")
	e, serr := a.ProvenEnd(ctx, models.ScopeReq{Name: "TestA", Outcome: "passed"})
	require.Nil(t, serr)
	require.Empty(t, e.Action)

	w := windowOf(t, a, "o::TestA")
	require.Equal(t, models.ScopeOutcomePassed, w.Outcome)
	require.Equal(t, []models.RealUse{{Dest: "auth.example.com:443", Failed: 1}}, w.Real)
	require.NotNil(t, w.Evidence)
	require.Len(t, w.Evidence.Incomplete, 1)
}

// What keploy could not capture before a test process claimed the run is the
// run's, not its first test's.
func TestUncapturedBeforeAClaimIsTheRuns(t *testing.T) {
	a, p := realAgent(t, models.MODE_TEST)
	ctx := context.Background()
	p.tracker.Incomplete("10.0.0.7:443", "a setup step")
	claimSelf(t, a)
	_, serr := a.ProvenBegin(ctx, begin("TestA"))
	require.Nil(t, serr)
	e, serr := a.ProvenEnd(ctx, models.ScopeReq{Name: "TestA", Outcome: "passed"})
	require.Nil(t, serr)
	require.Empty(t, e.Action)
	run, err := a.RealRun(ctx)
	require.NoError(t, err)
	require.Len(t, run.Incomplete, 1)
	require.Equal(t, "a setup step", run.Incomplete[0].Why)
}

// A REAL row and a capture loss compose (design v14 §3.2, §4): a replayed
// test that used a mock.real destination while the agent lost a call keeps
// its REAL row, and fails with the loss first, then the connections keploy
// could not capture of its own. A later test that only used REAL is tainted
// by the loss and keeps its row too.
func TestAReplayedTestCarriesItsRealRowsAndACaptureLoss(t *testing.T) {
	a, p := realAgent(t, models.MODE_TEST)
	ctx := context.Background()
	claimSelf(t, a)
	_, serr := a.ProvenBegin(ctx, begin("TestA"))
	require.Nil(t, serr)
	p.tracker.Dialed(1, "db.example.com:5432", true)
	p.tracker.Sent(1, "db.example.com:5432")
	p.tracker.Incomplete("10.0.0.7:443", "keploy could not read which name it was opened for")
	a.Hooks = countingHooks{refusals: models.PIDMapRefusals{Target: 2, LastTargetErr: -7}}
	e, serr := a.ProvenEnd(ctx, models.ScopeReq{Name: "TestA", Outcome: "passed"})
	require.Nil(t, serr)
	require.Equal(t, models.ScopeActionFail, e.Action)
	lost := models.CaptureLosses{{Kind: models.CaptureLossPIDMap, Count: 2, What: "target_namespace_pids, E2BIG: the map had no room for it, usually because it is full"}}
	// The loss first, then the connection, in one sentence; nothing to
	// re-record or quarantine: both are keploy's own.
	require.Equal(t, "keploy: "+lost.Text()+"; keploy could not capture a connection to 10.0.0.7:443 (keploy could not read which name it was opened for).", e.Reason)
	require.Contains(t, e.Reason, "capture incomplete: the kernel refused 2 insert(s) into keploy's PID map target_namespace_pids")

	_, serr = a.ProvenBegin(ctx, begin("TestB"))
	require.Nil(t, serr)
	p.tracker.Dialed(2, "db.example.com:5432", true)
	p.tracker.Sent(2, "db.example.com:5432")
	e, serr = a.ProvenEnd(ctx, models.ScopeReq{Name: "TestB", Outcome: "passed"})
	require.Nil(t, serr)
	require.Equal(t, models.ScopeActionFail, e.Action, "the loss taints every later test")
	require.Equal(t, "keploy: "+lost.Text()+".", e.Reason, "TestA's connection is not TestB's")

	// The windows last: reading them ends the run.
	w := windowOf(t, a, "o::TestA")
	require.Equal(t, models.ScopeOutcomeFailed, w.Outcome)
	require.Equal(t, []models.RealUse{{Dest: "db.example.com:5432", Conns: 1}}, w.Real)
	require.Equal(t, lost, w.Evidence.CaptureIncomplete)
	require.Len(t, w.Evidence.Incomplete, 1)
	w = windowOf(t, a, "o::TestB")
	require.Equal(t, models.ScopeOutcomeFailed, w.Outcome)
	require.Equal(t, []models.RealUse{{Dest: "db.example.com:5432", Conns: 1}}, w.Real)
	require.Equal(t, lost, w.Evidence.CaptureIncomplete)
	require.Empty(t, w.Evidence.Incomplete)
}

// In a record, a REAL row and a capture loss compose on the window for
// judging, and the test keeps its own outcome.
func TestARecordedTestKeepsItsRealRowsAndACaptureLoss(t *testing.T) {
	a, p := realAgent(t, models.MODE_RECORD)
	ctx := context.Background()
	claimSelf(t, a)
	_, serr := a.ProvenBegin(ctx, begin("TestA"))
	require.Nil(t, serr)
	p.tracker.Dialed(1, "auth.example.com:443", true)
	p.tracker.Sent(1, "auth.example.com:443")
	p.tracker.Incomplete("10.0.0.7:443", "unreadable")
	a.Hooks = countingHooks{refusals: models.PIDMapRefusals{Target: 1}}
	e, serr := a.ProvenEnd(ctx, models.ScopeReq{Name: "TestA", Outcome: "passed"})
	require.Nil(t, serr)
	require.Empty(t, e.Action, "a record never fails a test")

	w := windowOf(t, a, "o::TestA")
	require.Equal(t, models.ScopeOutcomePassed, w.Outcome)
	require.Equal(t, []models.RealUse{{Dest: "auth.example.com:443", Conns: 1}}, w.Real)
	require.Len(t, w.Evidence.CaptureIncomplete, 1)
	require.Len(t, w.Evidence.Incomplete, 1)
	// The same text as a replay's.
	require.Equal(t, "keploy: "+w.Evidence.CaptureIncomplete.Text()+"; keploy could not capture a connection to 10.0.0.7:443 (unreadable).", w.Evidence.Reason)
}

// A new session drops a test the last one left open; its evidence is the
// run's.
func TestANewSessionDropsAnOpenTestsRealUse(t *testing.T) {
	a, p := realAgent(t, models.MODE_TEST)
	ctx := context.Background()
	claimSelf(t, a)
	_, serr := a.ProvenBegin(ctx, begin("TestA"))
	require.Nil(t, serr)
	p.tracker.Incomplete("10.0.0.7:443", "left open")
	require.NoError(t, a.resetSession(ctx))
	run, err := a.RealRun(ctx)
	require.NoError(t, err)
	require.Len(t, run.Incomplete, 1)
}

func TestSetRealDepsNeedsAProxyThatRelaysThem(t *testing.T) {
	a, _ := wireAgent(t, models.MODE_TEST)
	ctx := context.Background()
	require.NoError(t, a.SetRealDeps(ctx, nil), "an empty list asks nothing")
	require.ErrorIs(t, a.SetRealDeps(ctx, []string{"localhost:5432"}), models.ErrNoRealRelay)
	run, err := a.RealRun(ctx)
	require.NoError(t, err)
	require.Empty(t, run)

	a, p := realAgent(t, models.MODE_TEST)
	require.NoError(t, a.SetRealDeps(ctx, []string{"localhost:5432"}))
	require.Equal(t, []string{"localhost:5432"}, p.set)
}

// keploy test's test case windows (Begin/ContinueTestErrorCapture, then
// /agent/real/test) carry each case's REAL rows and the evidence charged to
// it: what keploy could not capture while the case ran. What came while no
// case ran (before the first, between two) touched no case, and is the
// run's.
func TestKeployTestCasesCarryTheirRealRows(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "1")
	a, p := realAgent(t, models.MODE_TEST)
	ctx := context.Background()
	p.tracker.Incomplete("10.0.0.6:443", "the app's start-up") // before the first case: the run's

	require.NoError(t, a.BeginTestErrorCapture(ctx))
	p.tracker.Dialed(1, "db:5432", true)
	p.tracker.Sent(1, "db:5432")
	got, err := a.RealTest(ctx)
	require.NoError(t, err)
	require.Equal(t, []models.RealUse{{Dest: "db:5432", Conns: 1}}, got.Real)
	require.Empty(t, got.Incomplete, "what came before the set's first case is the run's")

	p.tracker.Incomplete("10.0.0.7:443", "between cases") // touched no case: the run's
	require.NoError(t, a.ContinueTestErrorCapture(ctx))
	p.tracker.Incomplete("10.0.0.8:443", "in the case")
	got, err = a.RealTest(ctx)
	require.NoError(t, err)
	require.Empty(t, got.Real)
	require.Len(t, got.Incomplete, 1)
	require.Equal(t, "10.0.0.8:443", got.Incomplete[0].Destination)

	run := p.tracker.Run()
	require.Len(t, run.Incomplete, 2)
	require.Equal(t, "10.0.0.6:443", run.Incomplete[0].Destination)
	require.Equal(t, "10.0.0.7:443", run.Incomplete[1].Destination)

	// An agent without a relaying proxy has nothing to report.
	plain, _ := wireAgent(t, models.MODE_TEST)
	got, err = plain.RealTest(ctx)
	require.NoError(t, err)
	require.Equal(t, models.RealTest{}, got)
}

// keploy record names a test case's REAL destinations by its window.
func TestRealUsedDuring(t *testing.T) {
	a, p := realAgent(t, models.MODE_RECORD)
	p.tracker.KeepUses(true) // keploy record's incoming stream keeps them
	from := time.Now()
	p.tracker.Dialed(1, "db:5432", true)
	p.tracker.Sent(1, "db:5432")
	require.Equal(t, []string{"db:5432"}, a.RealUsedDuring(from, time.Now()))
	plain, _ := wireAgent(t, models.MODE_RECORD)
	require.Nil(t, plain.RealUsedDuring(from, time.Now()))
}
