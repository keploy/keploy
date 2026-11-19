//go:build linux

package agent

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	coreAgent "go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/models"
)

// switchSource is a proxy's close accounting as Proxy.chargeOpen does it: a
// close of a connection of no worker's (instance 0) is the run's count while
// no run of one test process holds the session (PairedBytesOneProcess), and
// an owned loss of its worker, stamped with that worker's epoch, otherwise.
type switchSource struct {
	*ownedSource
	cmu     sync.Mutex
	on      bool
	counted models.CaptureLosses
}

// closeOf is Proxy.chargeOpen for a connection of instance inst.
func (s *switchSource) closeOf(inst uint32, kind models.CaptureLossKind) {
	l := models.CaptureLoss{Kind: kind, Count: 1, What: "from 127.0.0.1:62999"}
	s.cmu.Lock()
	if inst == 0 && !s.on {
		s.counted = s.counted.Plus(models.CaptureLosses{l})
		s.cmu.Unlock()
		return
	}
	s.cmu.Unlock()
	s.put(coreAgent.OwnedLoss{Worker: inst, Open: true, Loss: l})
}

func (s *switchSource) PairedBytesOneProcess(on bool) {
	s.cmu.Lock()
	s.on = on
	s.cmu.Unlock()
	s.ownedSource.PairedBytesOneProcess(on)
}

func (s *switchSource) CaptureLosses() models.CaptureLosses {
	s.ownedSource.CaptureLosses()
	s.cmu.Lock()
	defer s.cmu.Unlock()
	return append(models.CaptureLosses(nil), s.counted...)
}

func (s *switchSource) isOn() bool {
	s.cmu.Lock()
	defer s.cmu.Unlock()
	return s.on
}

// switchWire and switchInstanceWire are the record's and the replay's
// proxies with that accounting.
type switchWire struct {
	*wireProxy
	*switchSource
}

type switchInstanceWire struct {
	*instanceWire
	*switchSource
}

func (p *switchWire) CaptureLosses() models.CaptureLosses { return p.switchSource.CaptureLosses() }
func (p *switchInstanceWire) CaptureLosses() models.CaptureLosses {
	return p.switchSource.CaptureLosses()
}

var (
	_ coreAgent.PairedBytesBeginner = (*switchWire)(nil)
	_ coreAgent.OwnedLossTaker      = (*switchWire)(nil)
	_ coreAgent.CaptureLossReporter = (*switchWire)(nil)
	_ coreAgent.PairedBytesBeginner = (*switchInstanceWire)(nil)
	_ coreAgent.OwnedLossTaker      = (*switchInstanceWire)(nil)
	_ coreAgent.CaptureLossReporter = (*switchInstanceWire)(nil)
)

// switchAgent is a record or a replay whose proxy accounts closes as
// switchSource does, with room for workers, marked as a session's reset
// marks it.
func switchAgent(t *testing.T, mode models.Mode) (*Agent, *switchSource) {
	t.Helper()
	sw := &switchSource{ownedSource: &ownedSource{}}
	var a *Agent
	if mode == models.MODE_TEST {
		var iw *instanceWire
		a, iw, _ = instanceAgent(t, mode)
		a.Proxy = &switchInstanceWire{instanceWire: iw, switchSource: sw}
		a.proven.pool = map[string][]string{"a": {"pa"}, "b": {"pb"}}
	} else {
		a, _ = workerAgent(t, mode, 4)
		a.Proxy = &switchWire{wireProxy: a.Proxy.(*wireProxy), switchSource: sw}
	}
	a.proven.losses = a.markCaptureLosses(context.Background())
	return a, sw
}

// lossesIn says where a loss of kind is in the run's own verdict (what the
// agent counts since base, as mock replay and mock record read it after the
// uncharged read): judged (it fails a replay) or not (it leaves the run not
// judged).
func lossesIn(during models.CaptureLosses, kind models.CaptureLossKind) (judged, notJudged uint64) {
	j, nj := during.Split()
	return kindCount(j, kind), kindCount(nj, kind)
}

// A run of workers that claims a session a run of one test process claimed
// before (its process gone) turns the one-process accounting off: a close of
// a connection no worker made (instance 0), counted while worker 1's TestOne
// is open, is the run's count again and charges the open test, as in a run
// of workers alone (the control). Never worker 0's, whose tests no worker's
// claim begins, so it would charge no test.
func TestAWorkersRunAfterAOneProcessClaimCountsAnUnstampedClose(t *testing.T) {
	for _, mode := range []models.Mode{models.MODE_RECORD, models.MODE_TEST} {
		for _, kind := range []models.CaptureLossKind{models.CaptureLossAppBytesUntold, models.CaptureLossAppBytesLost} {
			for _, single := range []bool{false, true} {
				name := "workers only"
				if single {
					name = "after a one-process claim, its process gone"
				}
				t.Run(string(mode)+"/"+string(kind)+"/"+name, func(t *testing.T) {
					a, sw := switchAgent(t, mode)
					if single {
						s := child(t)
						claimWorker(t, a, s.Process.Pid, "s", 0)
						require.True(t, sw.isOn(), "premise: the one-process claim turned its accounting on")
						kill(t, s)
					}
					w1, w2 := child(t), child(t)
					claimWorker(t, a, w1.Process.Pid, "a", 2)
					require.False(t, sw.isOn(), "a run of workers holds the session: the one-process accounting is off")
					claimWorker(t, a, w2.Process.Pid, "b", 2)
					_, serr := a.ProvenBegin(as(w1.Process.Pid), begin("TestOne"))
					require.Nil(t, serr)
					require.NotZero(t, a.scopeInstance("a::TestOne"), "premise: TestOne is a worker's test")
					sw.closeOf(0, kind)
					_, serr = a.ProvenEnd(as(w1.Process.Pid), models.ScopeReq{Name: "TestOne", Outcome: models.ScopeOutcomePassed})
					require.Nil(t, serr)
					one := windowOf(t, a, "a::TestOne")
					j, nj := kindOf(one, kind)
					if kind == models.CaptureLossAppBytesUntold {
						require.EqualValues(t, 1, nj, "TestOne, open at the close of a connection no worker made, is not judged: %+v", one.Evidence)
						require.Zero(t, j)
					} else {
						require.EqualValues(t, 1, j, "TestOne, open at the close of a connection no worker made, is charged its lost bytes: %+v", one.Evidence)
						if mode == models.MODE_TEST {
							require.Equal(t, models.ScopeOutcomeFailed, one.Outcome, "a loss keploy saw fails TestOne in a replay")
						}
					}
				})
			}
		}
	}
}

// A close counted in a test process's gap between tests charges no test,
// and it is in the run's own verdict all the same: what mock replay and mock
// record read after the command ran (the uncharged read, then the counts
// since the run's base). A gap's lost bytes fail the replay there, and a
// gap's close keploy cannot account for leaves the run not judged. A run of
// one test process (worker 0) and a run of workers (worker 1's gap), in a
// record and a replay. Counted once: the run's own count holds it once, and
// a read of it again holds it no more.
func TestAGapsCloseIsInTheRunsOwnVerdict(t *testing.T) {
	for _, mode := range []models.Mode{models.MODE_RECORD, models.MODE_TEST} {
		for _, kind := range []models.CaptureLossKind{models.CaptureLossAppBytesUntold, models.CaptureLossAppBytesLost} {
			for _, workers := range []bool{false, true} {
				name := "one test process"
				if workers {
					name = "a worker"
				}
				t.Run(string(mode)+"/"+string(kind)+"/"+name, func(t *testing.T) {
					ctx := context.Background()
					a, sw := switchAgent(t, mode)
					// mock replay's and mock record's base, before the command.
					base, err := a.CaptureLosses(ctx)
					require.NoError(t, err)
					var ids []string
					if workers {
						w1, w2 := child(t), child(t)
						claimWorker(t, a, w1.Process.Pid, "a", 2)
						claimWorker(t, a, w2.Process.Pid, "b", 2)
						_, serr := a.ProvenBegin(as(w2.Process.Pid), begin("TestTwo"))
						require.Nil(t, serr)
						_, serr = a.ProvenBegin(as(w1.Process.Pid), begin("TestOne"))
						require.Nil(t, serr)
						inst1 := a.scopeInstance("a::TestOne")
						require.NotZero(t, inst1, "premise: TestOne is a worker's test")
						_, serr = a.ProvenEnd(as(w1.Process.Pid), models.ScopeReq{Name: "TestOne", Outcome: models.ScopeOutcomePassed})
						require.Nil(t, serr)
						// Worker 1's connection closes in its gap.
						sw.closeOf(inst1, kind)
						_, serr = a.ProvenBegin(as(w1.Process.Pid), begin("TestThree"))
						require.Nil(t, serr)
						_, serr = a.ProvenEnd(as(w1.Process.Pid), models.ScopeReq{Name: "TestThree", Outcome: models.ScopeOutcomePassed})
						require.Nil(t, serr)
						_, serr = a.ProvenEnd(as(w2.Process.Pid), models.ScopeReq{Name: "TestTwo", Outcome: models.ScopeOutcomePassed})
						require.Nil(t, serr)
						ids = []string{"a::TestOne", "b::TestTwo", "a::TestThree"}
					} else {
						claimSelf(t, a)
						endPassed(t, a, "TestOne")
						// The one process's connection closes in its gap.
						sw.closeOf(0, kind)
						endPassed(t, a, "TestTwo")
						ids = []string{"o::TestOne", "o::TestTwo"}
					}
					for _, id := range ids {
						j, nj := kindOf(windowOf(t, a, id), kind)
						require.Zero(t, j+nj, "%s never used the connection that closed in the gap", id)
					}
					run, err := a.UnchargedCaptureLosses(ctx)
					require.NoError(t, err)
					require.EqualValues(t, 1, kindCount(run, kind), "the gap's close is the run's: %+v", run)
					now, err := a.CaptureLosses(ctx)
					require.NoError(t, err)
					j, nj := lossesIn(now.Since(base), kind)
					if kind == models.CaptureLossAppBytesLost {
						require.EqualValues(t, 1, j, "the gap's lost bytes are in the run's own verdict, judged: they fail the replay")
						require.Zero(t, nj)
					} else {
						require.EqualValues(t, 1, nj, "the gap's close keploy cannot account for is in the run's own verdict, not judged: the run is not judged")
						require.Zero(t, j)
					}
					// Read again (mock record reads the uncharged losses twice):
					// counted once.
					_, err = a.UnchargedCaptureLosses(ctx)
					require.NoError(t, err)
					again, err := a.CaptureLosses(ctx)
					require.NoError(t, err)
					require.Equal(t, kindCount(now, kind), kindCount(again, kind), "a second read counts the gap's close no more")
				})
			}
		}
	}
}

// The run's last read (the uncharged read, mock replay's and mock record's)
// turns the one-process accounting off: a close of the process's connection
// counted after it, in the session's teardown, is in the counts before the
// next session's mark, and charges no later session -- neither the next
// session's run, nor its own verdict, nor the same-named test of the process
// that claims it. The earlier session's tests had all ended and been judged.
// A loss an earlier session stamped and never read (its command gone before
// its last read, a test still open) is the next session's run's
// (resetSession's ownedLeft), but never in that session's own verdict: it was
// counted in the earlier one.
func TestATeardownCloseOfTheOneProcessChargesNoLaterSession(t *testing.T) {
	for _, mode := range []models.Mode{models.MODE_RECORD, models.MODE_TEST} {
		for _, kind := range []models.CaptureLossKind{models.CaptureLossAppBytesUntold, models.CaptureLossAppBytesLost} {
			for _, c := range []struct {
				lastRead, nextTest bool
				name               string
			}{
				{true, true, "after the session's last read"},
				{false, true, "a session with no last read"},
				{false, false, "a session with no last read, then a run with no test"},
			} {
				lastRead := c.lastRead
				t.Run(string(mode)+"/"+string(kind)+"/"+c.name, func(t *testing.T) {
					ctx := context.Background()
					a, sw := switchAgent(t, mode)
					claimSelf(t, a)
					if lastRead {
						endPassed(t, a, "TestOne")
						sw.toldOf()
						_, err := a.UnchargedCaptureLosses(ctx)
						require.NoError(t, err)
						require.Contains(t, sw.toldOf(), "off", "the session's last read turns the one-process accounting off")
					} else {
						_, serr := a.ProvenBegin(ctx, begin("TestOne"))
						require.Nil(t, serr)
					}
					// Its connection closes in the teardown.
					sw.closeOf(0, kind)
					require.NoError(t, a.resetSession(ctx))
					base, err := a.CaptureLosses(ctx)
					require.NoError(t, err)
					if c.nextTest {
						claimSelf(t, a)
						endPassed(t, a, "TestOne")
						j, nj := kindOf(windowOf(t, a, "o::TestOne"), kind)
						require.Zero(t, j+nj, "the next session's TestOne never used the earlier session's connection")
					}
					run, err := a.UnchargedCaptureLosses(ctx)
					require.NoError(t, err)
					if lastRead {
						require.Zero(t, kindCount(run, kind), "the teardown's close is not the next session's run's: %+v", run)
					}
					now, err := a.CaptureLosses(ctx)
					require.NoError(t, err)
					require.Zero(t, kindCount(now.Since(base), kind), "nor in the next session's own verdict")
				})
			}
		}
	}
}

// settleThenClose is the one-process proxy whose end's settle, once armed,
// is done (moving worker 0's epoch past the ending test's, as the proxy's
// settledEnd does) and then counts a close of the process's connection,
// before the end takes its losses: stamped with the gap's epoch.
type settleThenClose struct {
	*ownedWire
	kind  models.CaptureLossKind
	armed bool
	put   bool
}

func (p *settleThenClose) PairedBytesSettled(worker uint32) {
	p.ownedSource.PairedBytesSettled(worker)
	if p.armed && !p.put {
		p.put = true
		p.ownedSource.put(coreAgent.OwnedLoss{Worker: worker, Open: true, Loss: models.CaptureLoss{Kind: p.kind, Count: 1, What: "from 127.0.0.1:62766"}})
	}
}

// A close counted after an end's settle is done, but before that end takes
// its losses, is the gap's: the run's, never the ending test's, which the
// settle had answered. Only one counted while the settle ran is the ending
// test's (TestACloseCountedInAnEndsSettleIsTheEndingTests).
func TestACloseCountedAfterAnEndsSettleIsTheRuns(t *testing.T) {
	for _, mode := range []models.Mode{models.MODE_RECORD, models.MODE_TEST} {
		for _, kind := range []models.CaptureLossKind{models.CaptureLossAppBytesUntold, models.CaptureLossAppBytesLost} {
			t.Run(string(mode)+"/"+string(kind), func(t *testing.T) {
				ctx := context.Background()
				a, wp := wireAgent(t, mode)
				p := &settleThenClose{ownedWire: &ownedWire{wireProxy: wp, ownedSource: &ownedSource{}}, kind: kind}
				a.Proxy = p
				a.proven.losses = a.markCaptureLosses(ctx)
				claimSelf(t, a)
				_, serr := a.ProvenBegin(ctx, begin("TestOne"))
				require.Nil(t, serr)
				p.armed = true
				_, serr = a.ProvenEnd(ctx, models.ScopeReq{Name: "TestOne", Outcome: models.ScopeOutcomePassed})
				require.Nil(t, serr)
				require.True(t, p.put, "premise: TestOne's end told the proxy its settle was done")
				one := windowOf(t, a, "o::TestOne")
				j, nj := kindOf(one, kind)
				require.Zero(t, j+nj, "a close counted after TestOne's settle was done is not TestOne's: %+v", one.Evidence)
				require.Equal(t, models.ScopeOutcomePassed, one.Outcome)
				run, err := a.UnchargedCaptureLosses(ctx)
				require.NoError(t, err)
				require.EqualValues(t, 1, kindCount(run, kind), "it is the run's: %+v", run)
			})
		}
	}
}

// The run's own count adds each loss the wire made the run's to the count of
// its kind, one count per kind, so a count since a base reads the sum; a
// kind no count has is its own.
func TestTheRunsOwnLossesAddToTheirKindsCount(t *testing.T) {
	counts := models.CaptureLosses{
		{Kind: models.CaptureLossAppBytesLost, Count: 2, What: "from 127.0.0.1:1"},
		{Kind: models.CaptureLossTLSAbort, Count: 1, What: "tls"},
	}
	run := models.CaptureLosses{
		{Kind: models.CaptureLossAppBytesLost, Count: 1, What: "from 127.0.0.1:2"},
		{Kind: models.CaptureLossAppBytesUntold, Count: 3, What: "from 127.0.0.1:3"},
		{Kind: models.CaptureLossTLSAbort, Count: 1},
	}
	got := withCounted(counts, run)
	require.EqualValues(t, 3, kindCount(got, models.CaptureLossAppBytesLost))
	require.EqualValues(t, 2, kindCount(got, models.CaptureLossTLSAbort))
	require.EqualValues(t, 3, kindCount(got, models.CaptureLossAppBytesUntold))
	require.Len(t, got, 3, "one count per kind: %+v", got)
	for _, l := range got {
		switch l.Kind {
		case models.CaptureLossAppBytesLost:
			require.Equal(t, "from 127.0.0.1:2", l.What, "the run's latest loss names what was lost")
		case models.CaptureLossTLSAbort:
			require.Equal(t, "tls", l.What, "a loss that names nothing keeps the count's words")
		}
	}
	require.EqualValues(t, 2, counts[0].Count, "the counts read are not changed")
	require.Equal(t, counts, withCounted(counts, nil))
}
