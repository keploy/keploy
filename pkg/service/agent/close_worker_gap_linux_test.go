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

// gapAgent is a record (workerAgent) or a replay (instanceAgent) with two
// test workers claimed, "a" and "b", whose proxy reports owned losses (src),
// and the proxy's own settle hooks (wp).
func gapAgent(t *testing.T, mode models.Mode) (a *Agent, src *ownedSource, wp *wireProxy, w1, w2 int) {
	t.Helper()
	src = &ownedSource{}
	if mode == models.MODE_TEST {
		var iw *instanceWire
		a, iw, _ = instanceAgent(t, mode)
		wp = iw.wireProxy
		a.Proxy = &ownedInstanceWire{instanceWire: iw, ownedSource: src}
		a.proven.pool = map[string][]string{"a": {"pa"}, "b": {"pb"}}
	} else {
		a, _ = workerAgent(t, mode, 4)
		wp = a.Proxy.(*wireProxy)
		a.Proxy = &ownedWire{wireProxy: wp, ownedSource: src}
	}
	a.proven.losses = a.markCaptureLosses(context.Background())
	c1, c2 := child(t), child(t)
	claimWorker(t, a, c1.Process.Pid, "a", 2)
	claimWorker(t, a, c2.Process.Pid, "b", 2)
	return a, src, wp, c1.Process.Pid, c2.Process.Pid
}

// A close of a worker's connection counted in that worker's gap between
// tests, after one test's end and before its next test's begin, is the
// run's: reported, charged to no test. The next test did not exist when it
// was counted, and never used the connection, so it is untouched, though no
// end takes the loss before its own (another worker's test stays open
// throughout); the same close counted once the next test is open is that
// test's. In a record and a replay, for a close keploy cannot account for
// and for bytes keploy saw lost.
func TestACloseInAWorkersGapSparesItsNextTest(t *testing.T) {
	for _, mode := range []models.Mode{models.MODE_RECORD, models.MODE_TEST} {
		for _, kind := range []models.CaptureLossKind{models.CaptureLossAppBytesUntold, models.CaptureLossAppBytesLost} {
			for _, inGap := range []bool{true, false} {
				where := "counted in the gap"
				if !inGap {
					where = "counted in the next test"
				}
				t.Run(string(mode)+"/"+string(kind)+"/"+where, func(t *testing.T) {
					a, src, _, w1, w2 := gapAgent(t, mode)
					_, serr := a.ProvenBegin(as(w1), begin("TestOne"))
					require.Nil(t, serr)
					_, serr = a.ProvenBegin(as(w2), begin("TestTwo"))
					require.Nil(t, serr)
					inst1 := a.scopeInstance("a::TestOne")
					require.NotZero(t, inst1, "premise: TestOne is a worker's test")
					_, serr = a.ProvenEnd(as(w1), models.ScopeReq{Name: "TestOne", Outcome: models.ScopeOutcomePassed})
					require.Nil(t, serr)
					closed := coreAgent.OwnedLoss{Worker: inst1, Loss: models.CaptureLoss{Kind: kind, Count: 1, What: "from 127.0.0.1:62766"}}
					if inGap {
						// Worker 1 has no test open: its connection closes now.
						src.put(closed)
					}
					_, serr = a.ProvenBegin(as(w1), begin("TestThree"))
					require.Nil(t, serr)
					require.Equal(t, inst1, a.scopeInstance("a::TestThree"), "premise: TestThree is worker 1's")
					if !inGap {
						src.put(closed)
					}
					_, serr = a.ProvenEnd(as(w1), models.ScopeReq{Name: "TestThree", Outcome: models.ScopeOutcomePassed})
					require.Nil(t, serr)
					_, serr = a.ProvenEnd(as(w2), models.ScopeReq{Name: "TestTwo", Outcome: models.ScopeOutcomePassed})
					require.Nil(t, serr)
					for _, id := range []string{"a::TestOne", "b::TestTwo"} {
						j, nj := kindOf(windowOf(t, a, id), kind)
						require.Zero(t, j+nj, "%s, which never used the connection, is untouched", id)
					}
					three := windowOf(t, a, "a::TestThree")
					j, nj := kindOf(three, kind)
					run, err := a.UnchargedCaptureLosses(context.Background())
					require.NoError(t, err)
					if inGap {
						if j != 0 || nj != 0 {
							t.Errorf("worker 1's TestThree, which began after the close was counted and never used the connection, is charged %d judged and %d unjudged %s; its outcome %s", j, nj, kind, three.Outcome)
						}
						require.NotEqual(t, models.ScopeOutcomeFailed, three.Outcome)
						require.EqualValues(t, 1, kindCount(run, kind), "the close counted while worker 1 had no test open is the run's: %+v", run)
						return
					}
					if kind == models.CaptureLossAppBytesUntold {
						require.EqualValues(t, 1, nj, "TestThree, open on worker 1 at the close, is not judged")
						require.Zero(t, j)
					} else {
						require.EqualValues(t, 1, j, "TestThree, open on worker 1 at the close, is charged the loss")
						if mode == models.MODE_TEST {
							require.Equal(t, models.ScopeOutcomeFailed, three.Outcome, "a loss keploy saw fails TestThree in a replay")
						}
					}
					require.Zero(t, kindCount(run, kind), "TestThree's close is not the run's too: %+v", run)
				})
			}
		}
	}
}

// A close of a worker's connection counted while that worker's test's end
// settles (its bytes wait) is the ending test's: the settle moves its calls.
// The end tells the proxy its settle is done only after it (the deferred
// PairedBytesSettled), which moves the worker's epoch past the ending
// test's: told as the settle began, the close would be the run's.
func TestACloseCountedInAnEndsSettleIsTheEndingTests(t *testing.T) {
	for _, mode := range []models.Mode{models.MODE_RECORD, models.MODE_TEST} {
		t.Run(string(mode), func(t *testing.T) {
			a, src, wp, w1, w2 := gapAgent(t, mode)
			_, serr := a.ProvenBegin(as(w1), begin("TestOne"))
			require.Nil(t, serr)
			_, serr = a.ProvenBegin(as(w2), begin("TestTwo"))
			require.Nil(t, serr)
			inst1 := a.scopeInstance("a::TestOne")
			require.NotZero(t, inst1, "premise: TestOne is a worker's test")
			var once sync.Once
			waited := false
			wp.bytesWait = func(context.Context, uint64) error {
				once.Do(func() {
					waited = true
					src.put(coreAgent.OwnedLoss{Worker: inst1, Loss: untoldFrom("62766")})
				})
				return nil
			}
			_, serr = a.ProvenEnd(as(w1), models.ScopeReq{Name: "TestOne", Outcome: models.ScopeOutcomePassed})
			require.Nil(t, serr)
			wp.bytesWait = nil
			require.True(t, waited, "premise: TestOne's end waited for the bytes on its connections")
			_, serr = a.ProvenEnd(as(w2), models.ScopeReq{Name: "TestTwo", Outcome: models.ScopeOutcomePassed})
			require.Nil(t, serr)
			_, one := kindOf(windowOf(t, a, "a::TestOne"), models.CaptureLossAppBytesUntold)
			require.EqualValues(t, 1, one, "a close counted in TestOne's settle is TestOne's")
			j, two := kindOf(windowOf(t, a, "b::TestTwo"), models.CaptureLossAppBytesUntold)
			require.Zero(t, j+two, "TestTwo, another worker's, is untouched")
			run, err := a.UnchargedCaptureLosses(context.Background())
			require.NoError(t, err)
			require.Zero(t, kindCount(run, models.CaptureLossAppBytesUntold), "TestOne's close is not the run's: %+v", run)
		})
	}
}

// A loss kept for a worker's test still open (ownedOpen) when the session
// is reset is the next session's to report, as the run's: never dropped.
func TestASessionResetKeepsALossKeptForAnOpenTestAsTheRuns(t *testing.T) {
	a, src, inst1, _, endTwo := keptAgent(t)
	src.put(coreAgent.OwnedLoss{Worker: inst1, Loss: untoldFrom("62766")})
	endTwo()
	a.provenMu.Lock()
	kept := len(a.proven.ownedOpen["a::TestOne"])
	a.provenMu.Unlock()
	require.Equal(t, 1, kept, "premise: TestTwo's end kept the loss for TestOne, still open")
	require.NoError(t, a.resetSession(context.Background()))
	run, err := a.UnchargedCaptureLosses(context.Background())
	require.NoError(t, err)
	require.EqualValues(t, 1, kindCount(run, models.CaptureLossAppBytesUntold), "the loss kept for the earlier session's open test is the run's: %+v", run)
}
