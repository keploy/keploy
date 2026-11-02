//go:build linux

package agent

import (
	"context"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/agent/containerkey"
	"go.keploy.io/server/v3/pkg/models"
)

// In a record, different workers' tests are open at once: each worker runs
// one test at a time, and each test's window is its own worker's.
func TestWorkersTestsOverlapAtRecord(t *testing.T) {
	a, _ := workerAgent(t, models.MODE_RECORD, 4)
	w1, w2 := child(t), child(t)
	claimWorker(t, a, w1.Process.Pid, "a", 2)
	claimWorker(t, a, w2.Process.Pid, "b", 2)
	_, serr := a.ProvenBegin(as(w1.Process.Pid), begin("TestOne"))
	require.Nil(t, serr)
	_, serr = a.ProvenBegin(as(w2.Process.Pid), begin("TestTwo"))
	require.Nil(t, serr, "another worker's test may be open")
	_, serr = a.ProvenBegin(as(w1.Process.Pid), begin("TestThree"))
	require.NotNil(t, serr, "one worker runs one test at a time")
	require.Equal(t, models.ScopeErrOneTestAtATime, serr.Code)
	require.Contains(t, serr.Message, "a::TestThree began while a::TestOne was still open")
	require.Equal(t, "a::TestOne", a.proven.openOf(1))
	require.Equal(t, "b::TestTwo", a.proven.openOf(2))

	_, serr = a.ProvenEnd(as(w1.Process.Pid), models.ScopeReq{Name: "TestOne", Outcome: "passed"})
	require.Nil(t, serr)
	require.Equal(t, "b::TestTwo", a.proven.openOf(2), "its end leaves the other worker's test open")
	require.True(t, a.proven.dropsInTest, "the drops' window that opens is the open test's, not a gap")
	_, serr = a.ProvenEnd(as(w2.Process.Pid), models.ScopeReq{Name: "TestTwo", Outcome: "failed"})
	require.Nil(t, serr)
	require.Empty(t, a.proven.open)
	require.False(t, a.proven.dropsInTest, "with no test open the window is the gap")
	one, two := windowOf(t, a, "a::TestOne"), windowOf(t, a, "b::TestTwo")
	require.Equal(t, models.ScopeOutcomePassed, one.Outcome)
	require.Equal(t, models.ScopeOutcomeFailed, two.Outcome)
	require.EqualValues(t, 1, one.Instance)
	require.EqualValues(t, 2, two.Instance)
}

// Two workers that run one test, under one key, at once are refused: the
// two runs' calls could not be told apart in its one window.
func TestOneTestOpenInTwoWorkersIsRefused(t *testing.T) {
	a, _ := workerAgent(t, models.MODE_RECORD, 4)
	w1, w2 := child(t), child(t)
	claimWorker(t, a, w1.Process.Pid, "k", 2)
	claimWorker(t, a, w2.Process.Pid, "k", 2)
	_, serr := a.ProvenBegin(as(w1.Process.Pid), begin("TestX"))
	require.Nil(t, serr)
	_, serr = a.ProvenBegin(as(w2.Process.Pid), begin("TestX"))
	require.NotNil(t, serr)
	require.Equal(t, models.ScopeErrOneTestAtATime, serr.Code)
	require.Contains(t, serr.Message, "k::TestX began in test worker 2 while test worker 1 ran the same test")
	require.Empty(t, a.proven.openOf(2))
}

// What keploy keeps for the run as one refuses an overlap, with its reason:
// mock.real's uses, and drops an agent counts as no capture loss.
func TestAnOverlapIsRefusedWhereKeployKeepsTheRunAsOne(t *testing.T) {
	for _, c := range []struct {
		name, why string
		set       func(*Agent, *instanceHooks)
	}{
		{"mock.real", "this run's mock.real destinations are tracked one test at a time",
			func(a *Agent, _ *instanceHooks) { a.realDepsSet.Store(true) }},
		{"no capture-loss count", "keploy's hooks here count no capture losses",
			func(_ *Agent, h *instanceHooks) { h.countingHooks.err = models.ErrPIDMapRefusalsUnsupported }},
	} {
		t.Run(c.name, func(t *testing.T) {
			a, h := workerAgent(t, models.MODE_RECORD, 4)
			w1, w2 := child(t), child(t)
			claimWorker(t, a, w1.Process.Pid, "a", 2)
			claimWorker(t, a, w2.Process.Pid, "b", 2)
			c.set(a, h)
			_, serr := a.ProvenBegin(as(w2.Process.Pid), begin("TestAlone"))
			require.Nil(t, serr, "with no other test open it begins")
			_, serr = a.ProvenEnd(as(w2.Process.Pid), models.ScopeReq{Name: "TestAlone", Outcome: "passed"})
			require.Nil(t, serr)
			_, serr = a.ProvenBegin(as(w1.Process.Pid), begin("TestOne"))
			require.Nil(t, serr)
			_, serr = a.ProvenBegin(as(w2.Process.Pid), begin("TestTwo"))
			require.NotNil(t, serr)
			require.Equal(t, models.ScopeErrOneTestAtATime, serr.Code)
			require.Contains(t, serr.Message, "b::TestTwo began in test worker 2 while test worker 1's test a::TestOne was still open, but "+c.why)
			require.Empty(t, a.proven.openOf(2))
		})
	}
}

// A test's own kind of capture loss (a failed TLS handshake) made while two
// workers' tests are open is charged to both; one made while one test is
// open is that test's alone, whichever test ends first.
func TestALossDuringAnOverlapIsEveryOpenTestsOwn(t *testing.T) {
	a, _ := workerAgent(t, models.MODE_RECORD, 4)
	lp := lossProxy{wireProxy: a.Proxy.(*wireProxy)}
	a.Proxy = lp
	lost := func(n uint64) {
		lp.losses = models.CaptureLosses{{Kind: models.CaptureLossTLSAbort, Count: n, What: "to 10.0.0.1:443"}}
		a.Proxy = lp
	}
	a.proven.losses = a.markCaptureLosses(context.Background())
	w1, w2 := child(t), child(t)
	claimWorker(t, a, w1.Process.Pid, "a", 2)
	claimWorker(t, a, w2.Process.Pid, "b", 2)
	_, serr := a.ProvenBegin(as(w1.Process.Pid), begin("TestFirst"))
	require.Nil(t, serr)
	lost(1) // TestFirst's alone
	_, serr = a.ProvenBegin(as(w2.Process.Pid), begin("TestSecond"))
	require.Nil(t, serr)
	lost(2) // both open
	_, serr = a.ProvenEnd(as(w2.Process.Pid), models.ScopeReq{Name: "TestSecond", Outcome: "passed"})
	require.Nil(t, serr)
	lost(3) // TestFirst's alone
	_, serr = a.ProvenEnd(as(w1.Process.Pid), models.ScopeReq{Name: "TestFirst", Outcome: "passed"})
	require.Nil(t, serr)
	_, serr = a.ProvenBegin(as(w2.Process.Pid), begin("TestAfter"))
	require.Nil(t, serr)
	_, serr = a.ProvenEnd(as(w2.Process.Pid), models.ScopeReq{Name: "TestAfter", Outcome: "passed"})
	require.Nil(t, serr)

	tls := func(n uint64) models.CaptureLosses {
		return models.CaptureLosses{{Kind: models.CaptureLossTLSAbort, Count: n, What: "to 10.0.0.1:443"}}
	}
	require.Equal(t, tls(3), windowOf(t, a, "a::TestFirst").Evidence.CaptureIncomplete, "all three: it was open across each")
	require.Equal(t, tls(1), windowOf(t, a, "b::TestSecond").Evidence.CaptureIncomplete, "the one made while it was open")
	require.Nil(t, windowOf(t, a, "b::TestAfter").Evidence, "nothing came after the last test that left none open")
}

// A gone worker's open test fails; another worker's open test goes on.
func TestAGoneWorkerFailsOnlyItsOwnOpenTest(t *testing.T) {
	a, _ := workerAgent(t, models.MODE_RECORD, 4)
	gone, stays := child(t), child(t)
	claimWorker(t, a, gone.Process.Pid, "a", 2)
	claimWorker(t, a, stays.Process.Pid, "b", 2)
	_, serr := a.ProvenBegin(as(gone.Process.Pid), begin("TestCrash"))
	require.Nil(t, serr)
	_, serr = a.ProvenBegin(as(stays.Process.Pid), begin("TestKeep"))
	require.Nil(t, serr)
	kill(t, gone)
	claimWorker(t, a, child(t).Process.Pid, "c", 2)
	require.Empty(t, a.proven.openOf(1))
	_, serr = a.ProvenEnd(as(stays.Process.Pid), models.ScopeReq{Name: "TestKeep", Outcome: "passed"})
	require.Nil(t, serr, "the live worker's test is still open")
	require.Equal(t, models.ScopeOutcomeFailed, windowOf(t, a, "a::TestCrash").Outcome)
	require.Equal(t, models.ScopeOutcomePassed, windowOf(t, a, "b::TestKeep").Outcome)
}

// The run's final read fails every test still open, each with its own
// worker's process.
func TestTheFinalReadFailsEveryOpenTest(t *testing.T) {
	a, _ := workerAgent(t, models.MODE_RECORD, 4)
	w1, w2 := child(t), child(t)
	claimWorker(t, a, w1.Process.Pid, "a", 2)
	claimWorker(t, a, w2.Process.Pid, "b", 2)
	_, serr := a.ProvenBegin(as(w1.Process.Pid), begin("TestOne"))
	require.Nil(t, serr)
	_, serr = a.ProvenBegin(as(w2.Process.Pid), begin("TestTwo"))
	require.Nil(t, serr)
	for id, pid := range map[string]int{"a::TestOne": w1.Process.Pid, "b::TestTwo": w2.Process.Pid} {
		w := windowOf(t, a, id)
		require.Equal(t, models.ScopeOutcomeFailed, w.Outcome, id)
		require.Contains(t, w.Evidence.Reason, "its process "+strconv.Itoa(pid)+" outlived the test command", id)
	}
}

// A call to a local listener is listed for the open test of the worker
// whose process made it; a call from no worker is no test's.
func TestATopologyRowIsItsWorkersOpenTests(t *testing.T) {
	a, _ := workerAgent(t, models.MODE_RECORD, 4)
	w1, w2 := child(t), child(t)
	claimWorker(t, a, w1.Process.Pid, "a", 2)
	claimWorker(t, a, w2.Process.Pid, "b", 2)
	_, serr := a.ProvenBegin(as(w1.Process.Pid), begin("TestOne"))
	require.Nil(t, serr)
	_, serr = a.ProvenBegin(as(w2.Process.Pid), begin("TestTwo"))
	require.Nil(t, serr)
	a.NoteLive(models.LiveRow{Exe: "/srv", Port: 8080, Instance: 2})
	a.NoteLive(models.LiveRow{Exe: "/srv", Port: 8081})
	a.NoteMocked(models.MockedRow{Addr: "10.0.0.7:443", Port: 443, Instance: 1})
	require.Contains(t, a.proven.topology.live, models.LiveRow{Test: "b::TestTwo", Exe: "/srv", Port: 8080})
	require.Contains(t, a.proven.topology.live, models.LiveRow{Exe: "/srv", Port: 8081})
	require.Contains(t, a.proven.topology.mocked, models.MockedRow{Test: "a::TestOne", Addr: "10.0.0.7:443", Port: 443})
}

// A mock whose container's read the daemon did not answer, stamped while
// two workers' tests were open, is charged to both; one stamped once one
// has closed, to the other alone.
func TestAnUnreadMockOfAnOverlapIsChargedToEveryOpenTest(t *testing.T) {
	a, _ := workerAgent(t, models.MODE_RECORD, 4)
	w1, w2 := child(t), child(t)
	claimWorker(t, a, w1.Process.Pid, "a", 2)
	claimWorker(t, a, w2.Process.Pid, "b", 2)
	_, serr := a.ProvenBegin(as(w1.Process.Pid), begin("TestOne"))
	require.Nil(t, serr)
	_, serr = a.ProvenBegin(as(w2.Process.Pid), begin("TestTwo"))
	require.Nil(t, serr)
	a.unreadMock(containerkey.Default.Scope(), "both")
	_, serr = a.ProvenEnd(as(w1.Process.Pid), models.ScopeReq{Name: "TestOne", Outcome: "passed"})
	require.Nil(t, serr)
	a.unreadMock(containerkey.Default.Scope(), "two")
	_, serr = a.ProvenEnd(as(w2.Process.Pid), models.ScopeReq{Name: "TestTwo", Outcome: "passed"})
	require.Nil(t, serr)
	unread := func(id string) uint64 {
		w := windowOf(t, a, id)
		require.NotNil(t, w.Evidence, id)
		n := uint64(0)
		for _, c := range w.Evidence.CannotCapture {
			if c.Kind == models.CaptureLossContainerUnread {
				n += c.Count
			}
		}
		return n
	}
	require.EqualValues(t, 1, unread("a::TestOne"))
	require.EqualValues(t, 2, unread("b::TestTwo"))
}

// realKeeper is a wireProxy that keeps a run's mock.real across pushes.
type realKeeper struct{ *wireProxy }

func (realKeeper) KeepRealDeps(context.Context, []string) error { return nil }

// The overlap check sees the run's mock.real as each push installs it.
func TestARunsMockRealIsSeenByTheOverlapCheck(t *testing.T) {
	a, p := wireAgent(t, models.MODE_RECORD)
	ctx := context.Background()
	// This proxy relays no real destination: the list is refused, and the
	// run still counts as one that names some.
	_ = a.SetRealDeps(ctx, []string{"api.example.com:443"})
	require.True(t, a.realDepsSet.Load())
	require.NoError(t, a.SetRealDeps(ctx, nil))
	require.False(t, a.realDepsSet.Load())
	a.Proxy = realKeeper{p}
	require.NoError(t, a.KeepRealDeps(ctx, []string{"api.example.com:443"}))
	require.True(t, a.realDepsSet.Load())
	require.NoError(t, a.KeepRealDeps(ctx, nil))
	require.False(t, a.realDepsSet.Load())
}

// A given is the open test's of the worker whose process gave it, while
// another worker's test is open too.
func TestAGivenIsItsOwnWorkersOpenTests(t *testing.T) {
	a, _ := workerAgent(t, models.MODE_RECORD, 4)
	w1, w2 := child(t), child(t)
	claimWorker(t, a, w1.Process.Pid, "a", 2)
	claimWorker(t, a, w2.Process.Pid, "b", 2)
	_, serr := a.ProvenBegin(as(w1.Process.Pid), begin("TestOne"))
	require.Nil(t, serr)
	_, serr = a.ProvenBegin(as(w2.Process.Pid), begin("TestTwo"))
	require.Nil(t, serr)
	resp, serr := a.ProvenGive(as(w2.Process.Pid), give("app", "app-2", "b_test.go:7"))
	require.Nil(t, serr)
	require.Equal(t, "given:app", resp.Variable)
	resp, serr = a.ProvenGive(as(w1.Process.Pid), give("app", "app-1", "a_test.go:7"))
	require.Nil(t, serr)
	require.Equal(t, "given:app", resp.Variable, "each test numbers its own givens")
	_, serr = a.ProvenEnd(as(w2.Process.Pid), models.ScopeReq{Name: "TestTwo", Outcome: "passed"})
	require.Nil(t, serr)
	_, serr = a.ProvenGive(as(w2.Process.Pid), give("app", "app-3", ""))
	require.NotNil(t, serr, "its worker has no test open")
	require.Equal(t, models.ScopeErrNotOpen, serr.Code)
	_, serr = a.ProvenEnd(as(w1.Process.Pid), models.ScopeReq{Name: "TestOne", Outcome: "passed"})
	require.Nil(t, serr)
	require.Equal(t, []models.GivenValue{{Name: "given:app", Value: "app-1", Source: "a_test.go:7"}}, windowOf(t, a, "a::TestOne").Given)
	require.Equal(t, []models.GivenValue{{Name: "given:app", Value: "app-2", Source: "b_test.go:7"}}, windowOf(t, a, "b::TestTwo").Given)
}

// raise raises the proxy listener's drops by listener and its network
// namespace's by net.
func raise(p *wireProxy, listener, net uint64) {
	p.smu.Lock()
	p.drops, p.netDrops = p.drops+listener, p.netDrops+net
	p.smu.Unlock()
}

// A begin while another worker's test is open closes that test's drops'
// window, not a gap: what the window was carried from the gap and could not
// pair pairs with nothing later, as at any test's end. A drop whose two
// rises the gap after the tests sees is the next test's.
func TestAnOverlappingBeginClosesATestsDropsWindow(t *testing.T) {
	a, _ := workerAgent(t, models.MODE_RECORD, 4)
	p := a.Proxy.(*wireProxy)
	w1, w2 := child(t), child(t)
	claimWorker(t, a, w1.Process.Pid, "a", 2)
	claimWorker(t, a, w2.Process.Pid, "b", 2)
	_, serr := a.ProvenBegin(as(w1.Process.Pid), begin("TestZero"))
	require.Nil(t, serr)
	_, serr = a.ProvenEnd(as(w1.Process.Pid), models.ScopeReq{Name: "TestZero", Outcome: "passed"})
	require.Nil(t, serr)
	raise(p, 1, 0) // in the gap, unpaired: carried into TestOne's window
	_, serr = a.ProvenBegin(as(w1.Process.Pid), begin("TestOne"))
	require.Nil(t, serr)
	_, serr = a.ProvenBegin(as(w2.Process.Pid), begin("TestTwo"))
	require.Nil(t, serr)
	raise(p, 0, 1) // pairs with nothing: the rise it would pair with was carried, unpaired, through a test's window
	_, serr = a.ProvenEnd(as(w2.Process.Pid), models.ScopeReq{Name: "TestTwo", Outcome: "passed"})
	require.Nil(t, serr)
	_, serr = a.ProvenEnd(as(w1.Process.Pid), models.ScopeReq{Name: "TestOne", Outcome: "passed"})
	require.Nil(t, serr)
	raise(p, 1, 1) // in the gap: TestThree's
	_, serr = a.ProvenBegin(as(w2.Process.Pid), begin("TestThree"))
	require.Nil(t, serr)
	_, serr = a.ProvenEnd(as(w2.Process.Pid), models.ScopeReq{Name: "TestThree", Outcome: "passed"})
	require.Nil(t, serr)

	for _, id := range []string{"a::TestZero", "a::TestOne", "b::TestTwo"} {
		require.Nil(t, windowOf(t, a, id).Evidence, "%s is charged nothing", id)
	}
	w := windowOf(t, a, "b::TestThree")
	require.NotNil(t, w.Evidence)
	require.Equal(t, models.CaptureLosses{{Kind: models.CaptureLossListenerDropped, Count: 1}}, w.Evidence.CannotCapture)
}

// An end that gives up on its test's calls while another worker's test is
// open leaves the drops' window that test's: the run's read of what no test
// was charged does not close it, and the open test's end charges it.
func TestAGivenUpEndLeavesTheDropsWindowTheOpenTests(t *testing.T) {
	a, h := workerAgent(t, models.MODE_RECORD, 4)
	p := a.Proxy.(*wireProxy)
	ctx := context.Background()
	w1, w2 := child(t), child(t)
	claimWorker(t, a, w1.Process.Pid, "a", 2)
	claimWorker(t, a, w2.Process.Pid, "b", 2)
	_, serr := a.ProvenBegin(as(w1.Process.Pid), begin("TestOne"))
	require.Nil(t, serr)
	_, serr = a.ProvenBegin(as(w2.Process.Pid), begin("TestTwo"))
	require.Nil(t, serr)
	fault := func(err error) {
		h.mu.Lock()
		h.countHooks.err = err
		h.mu.Unlock()
	}
	fault(models.ErrProxyConnsUnsupported)
	_, serr = a.ProvenEnd(as(w2.Process.Pid), models.ScopeReq{Name: "TestTwo", Outcome: "passed"})
	require.NotNil(t, serr, "keploy cannot count the calls still waiting for its proxy")
	fault(nil)
	raise(p, 1, 1) // in TestOne's window
	un, err := a.UnchargedCaptureLosses(ctx)
	require.NoError(t, err)
	require.Empty(t, un, "the drop is the open test's")
	_, serr = a.ProvenEnd(as(w1.Process.Pid), models.ScopeReq{Name: "TestOne", Outcome: "passed"})
	require.Nil(t, serr)
	w := windowOf(t, a, "a::TestOne")
	require.NotNil(t, w.Evidence)
	require.Equal(t, models.CaptureLosses{{Kind: models.CaptureLossListenerDropped, Count: 1}}, w.Evidence.CannotCapture)
}
