//go:build linux

package agent

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	coreAgent "go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/models"
)

// keptAgent is a record with two test workers, TestOne (worker "a") and
// TestTwo (worker "b") both open, whose proxy reports owned losses (src).
// It returns TestOne's worker's instance.
func keptAgent(t *testing.T) (*Agent, *ownedSource, uint32, func(), func()) {
	t.Helper()
	a, _ := workerAgent(t, models.MODE_RECORD, 4)
	src := &ownedSource{}
	a.Proxy = &ownedWire{wireProxy: a.Proxy.(*wireProxy), ownedSource: src}
	a.proven.losses = a.markCaptureLosses(context.Background())
	w1, w2 := child(t), child(t)
	claimWorker(t, a, w1.Process.Pid, "a", 2)
	claimWorker(t, a, w2.Process.Pid, "b", 2)
	_, serr := a.ProvenBegin(as(w1.Process.Pid), begin("TestOne"))
	require.Nil(t, serr)
	_, serr = a.ProvenBegin(as(w2.Process.Pid), begin("TestTwo"))
	require.Nil(t, serr)
	inst1 := a.scopeInstance("a::TestOne")
	require.NotZero(t, inst1, "premise: TestOne is a worker's test")
	require.NotEqual(t, inst1, a.scopeInstance("b::TestTwo"), "premise: the tests are two workers'")
	endOne := func() {
		_, _ = a.ProvenEnd(as(w1.Process.Pid), models.ScopeReq{Name: "TestOne", Outcome: models.ScopeOutcomePassed})
	}
	endTwo := func() {
		_, _ = a.ProvenEnd(as(w2.Process.Pid), models.ScopeReq{Name: "TestTwo", Outcome: models.ScopeOutcomePassed})
	}
	return a, src, inst1, endOne, endTwo
}

func untoldFrom(port string) models.CaptureLoss {
	return models.CaptureLoss{Kind: models.CaptureLossAppBytesUntold, Count: 1, What: "from 127.0.0.1:" + port}
}

// A loss of a worker's open test (coreAgent.OwnedLoss's Worker) that another
// worker's test's end took is kept for that test, and is never dropped or
// charged to a test that never used the connection: a loss that names its
// test is that test's whatever worker it names; one kept for a test still
// open when the run's uncharged losses are read is the run's, and that
// test's later end is not charged it again; one an end whose count read
// failed took, kept or its own, is the run's.
func TestALossKeptForAWorkersOpenTestIsNeverDropped(t *testing.T) {
	untold := models.CaptureLossAppBytesUntold
	t.Run("a loss that names its test is that test's, whatever worker it names", func(t *testing.T) {
		a, src, inst1, endOne, endTwo := keptAgent(t)
		src.put(coreAgent.OwnedLoss{Test: "b::TestTwo", Worker: inst1, Loss: untoldFrom("62766")})
		endTwo()
		endOne()
		_, two := kindOf(windowOf(t, a, "b::TestTwo"), untold)
		require.EqualValues(t, 1, two, "TestTwo, the test the loss names, is not judged")
		judged, one := kindOf(windowOf(t, a, "a::TestOne"), untold)
		require.Zero(t, judged+one, "TestOne, of the worker the loss names but not its test, is untouched")
	})
	t.Run("kept for a test still open when the run's losses are read: the run's, once", func(t *testing.T) {
		a, src, inst1, endOne, endTwo := keptAgent(t)
		// mock record's base, before the command.
		base, err := a.CaptureLosses(context.Background())
		require.NoError(t, err)
		src.put(coreAgent.OwnedLoss{Worker: inst1, Open: true, Loss: untoldFrom("62766")})
		endTwo()
		run, err := a.UnchargedCaptureLosses(context.Background())
		require.NoError(t, err)
		require.EqualValues(t, 1, kindCount(run, untold), "a loss kept for a test still open is the run's as the run's losses are read: %+v", run)
		// And in the run's own verdict, the counts since the base mock
		// replay and mock record read next: not judged.
		now, err := a.CaptureLosses(context.Background())
		require.NoError(t, err)
		judged, notJudged := lossesIn(now.Since(base), untold)
		require.Zero(t, judged)
		require.EqualValues(t, 1, notJudged, "the loss kept for TestOne, open at the run's last read, is in the run's own verdict: %+v", now.Since(base))
		endOne()
		judged, one := kindOf(windowOf(t, a, "a::TestOne"), untold)
		require.Zero(t, judged+one, "TestOne is not charged a loss the run already reported")
	})
	t.Run("taken by ends whose count read failed: the run's", func(t *testing.T) {
		a, src, inst1, endOne, endTwo := keptAgent(t)
		ctx := context.Background()
		// mock record's base, before the command.
		base, err := a.CaptureLosses(ctx)
		require.NoError(t, err)
		// The counts can no longer be read (no mark of the run's start).
		a.provenMu.Lock()
		a.proven.losses = captureLossMark{}
		a.provenMu.Unlock()
		// Taken at TestTwo's end and kept for TestOne, then one taken at
		// TestOne's own end.
		src.put(coreAgent.OwnedLoss{Worker: inst1, Open: true, Loss: untoldFrom("62766")})
		endTwo()
		src.put(coreAgent.OwnedLoss{Worker: inst1, Open: true, Loss: untoldFrom("62767")})
		endOne()
		a.provenMu.Lock()
		require.EqualValues(t, 2, kindCount(a.proven.ownedRun, untold), "both are the run's: %+v", a.proven.ownedRun)
		require.Empty(t, a.proven.ownedOpen)
		a.provenMu.Unlock()
		// And both are in the run's own verdict, the counts since the base
		// that mock record reads after the command: the run is not judged.
		now, err := a.CaptureLosses(ctx)
		require.NoError(t, err)
		judged, notJudged := lossesIn(now.Since(base), untold)
		require.EqualValues(t, 2, notJudged, "the kept loss and TestOne's own are in the run's own verdict: %+v", now)
		require.Zero(t, judged)
	})
}
