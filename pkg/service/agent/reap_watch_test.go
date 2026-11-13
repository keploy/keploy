//go:build linux

package agent

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/models"
)

// liveWindow is test id's window as the run's windows read now, without the
// final read that fails every test still open (FinalScopeWindows).
func liveWindow(a *Agent, id string) (models.ScopeWindow, bool) {
	ws, _ := a.GetScopeWindows(context.Background())
	for _, w := range ws {
		if w.Name == id {
			return w, true
		}
	}
	return models.ScopeWindow{}, false
}

// failedSoon waits for test id to be failed with why, with no claim and no
// final read to fail it: the watch does.
func failedSoon(t *testing.T, a *Agent, id, why string) {
	t.Helper()
	require.Eventually(t, func() bool {
		w, ok := liveWindow(a, id)
		return ok && w.Outcome == models.ScopeOutcomeFailed && w.Evidence != nil
	}, 10*time.Second, 10*time.Millisecond, "%s is failed as soon as its process is seen gone", id)
	w, _ := liveWindow(a, id)
	require.Contains(t, w.Evidence.Reason, why)
}

func watches(a *Agent) int {
	a.provenMu.Lock()
	defer a.provenMu.Unlock()
	return a.proven.watches
}

// The test process exits with a test open (a go test binary killed by its
// own -timeout): the test fails then, not at the next claim or the run's
// final read, so what the processes it left running do after it is no call
// of its. The run's final read still lists it failed, once.
func TestATestWhoseProcessExitsFailsWithoutAClaimOrARead(t *testing.T) {
	a, _ := wireAgent(t, models.MODE_TEST)
	ctx := context.Background()
	proc := child(t)
	_, serr := a.ClaimInstance(ctx, models.InstanceReq{Key: "k"}, proc.Process.Pid)
	require.Nil(t, serr)
	_, serr = a.ProvenBegin(ctx, begin("TestDies"))
	require.Nil(t, serr)
	kill(t, proc)

	failedSoon(t, a, "k::TestDies", "the test process exited while the test was open")
	a.provenMu.Lock()
	open := a.proven.openOf(0)
	a.provenMu.Unlock()
	require.Empty(t, open, "its window is closed: a call from here is charged to no test")
	require.Eventually(t, func() bool { return watches(a) == 0 }, 10*time.Second, 10*time.Millisecond, "with no test open the watch ends")
	time.Sleep(5 * goneWatchEvery)
	require.Equal(t, 0, watches(a), "an ended watch looks no more")

	ws, err := a.FinalScopeWindows(ctx)
	require.NoError(t, err)
	n := 0
	for _, w := range ws {
		if w.Name == "k::TestDies" {
			n++
			require.Equal(t, models.ScopeOutcomeFailed, w.Outcome)
		}
	}
	require.Equal(t, 1, n, "failed once: the final read finds it closed")
}

// A test whose process runs on stays open across the watch's looks; once it
// ends, the watch ends, and the next test opened is watched again.
func TestALiveTestStaysOpenAndTheNextTestIsWatchedToo(t *testing.T) {
	a, _ := wireAgent(t, models.MODE_RECORD)
	ctx := context.Background()
	proc := child(t)
	_, serr := a.ClaimInstance(ctx, models.InstanceReq{Key: "k"}, proc.Process.Pid)
	require.Nil(t, serr)
	_, serr = a.ProvenBegin(ctx, begin("TestLives"))
	require.Nil(t, serr)
	require.Equal(t, 1, watches(a))
	// A second start while it runs starts no second watch.
	a.provenMu.Lock()
	a.watchGoneLocked()
	a.provenMu.Unlock()
	require.Equal(t, 1, watches(a), "one watch at a time")
	time.Sleep(10 * goneWatchEvery)
	a.provenMu.Lock()
	open := a.proven.openOf(0)
	a.provenMu.Unlock()
	require.Equal(t, "k::TestLives", open, "a test whose process runs is not failed by the watch")

	_, serr = a.ProvenEnd(ctx, models.ScopeReq{Name: "TestLives", Outcome: "passed"})
	require.Nil(t, serr)
	require.Eventually(t, func() bool { return watches(a) == 0 }, 10*time.Second, 10*time.Millisecond, "with no test open the watch ends")
	w, ok := liveWindow(a, "k::TestLives")
	require.True(t, ok)
	require.Equal(t, models.ScopeOutcomePassed, w.Outcome)

	_, serr = a.ProvenBegin(ctx, begin("TestDiesNext"))
	require.Nil(t, serr)
	kill(t, proc)
	failedSoon(t, a, "k::TestDiesNext", "the test process exited while the test was open")
}

// A worker whose process exits with a test open: its test fails then, and
// its instance ends; another worker's open test stays open.
func TestAGoneWorkersOpenTestFailsWithoutAClaimOrARead(t *testing.T) {
	a, h := workerAgent(t, models.MODE_RECORD, 4)
	gone, stays := child(t), child(t)
	claimWorker(t, a, gone.Process.Pid, "a", 2)
	claimWorker(t, a, stays.Process.Pid, "b", 2)
	_, serr := a.ProvenBegin(as(gone.Process.Pid), begin("TestCrash"))
	require.Nil(t, serr)
	_, serr = a.ProvenBegin(as(stays.Process.Pid), begin("TestRuns"))
	require.Nil(t, serr)
	kill(t, gone)

	failedSoon(t, a, "a::TestCrash", "the test worker's process exited while the test was open")
	require.Equal(t, []uint32{1}, h.endedIDs(), "the gone worker's instance ended")
	a.provenMu.Lock()
	still := []string{}
	for _, inst := range a.proven.openIDs() {
		still = append(still, a.proven.openOf(inst))
	}
	a.provenMu.Unlock()
	require.Equal(t, []string{"b::TestRuns"}, still, "the live worker's test stays open")
	// A look at the live worker's test takes no wire either: a run of
	// workers has no single test process to look at.
	require.NoError(t, a.wire.lock(context.Background()))
	ended, done := lookNow(a, nil, 5*time.Second)
	a.wire.unlock()
	require.True(t, <-done, "the look found the test open")
	require.True(t, ended, "a look at a live worker's test does not wait for the wire")
	_, serr = a.ProvenEnd(as(stays.Process.Pid), models.ScopeReq{Name: "TestRuns", Outcome: "passed"})
	require.Nil(t, serr)
}

// A session reset while the watch runs keeps it: one watch at a time, which
// ends once the new session has no test open.
func TestTheWatchOutlivesASessionReset(t *testing.T) {
	a, _ := wireAgent(t, models.MODE_TEST)
	ctx := context.Background()
	_, serr := a.ClaimInstance(ctx, models.InstanceReq{Key: "k"}, os.Getpid())
	require.Nil(t, serr)
	_, serr = a.ProvenBegin(ctx, begin("TestOpen"))
	require.Nil(t, serr)
	require.Equal(t, 1, watches(a))
	require.NoError(t, a.resetSession(ctx))
	require.Equal(t, 1, watches(a), "the reset keeps the watch it did not end")
	require.Eventually(t, func() bool { return watches(a) == 0 }, 10*time.Second, 10*time.Millisecond, "the new session has no test open: the watch ends")
}

// lookNow runs one look of the watch (reapGone) with procs as the buffer an
// earlier look handed back. It reports whether the look ended within wait,
// and done, which gives, once the look ends, whether it found a test open.
func lookNow(a *Agent, procs []claimant, wait time.Duration) (ended bool, done <-chan bool) {
	c := make(chan bool, 1)
	finished := make(chan struct{})
	go func() {
		_, open := a.reapGone(procs)
		c <- open
		close(finished)
	}()
	select {
	case <-finished:
		return true, c
	case <-time.After(wait):
		return false, c
	}
}

// A look at a test whose process runs takes nothing a claim, a begin or an
// end waits for: with the wire held by one of them, the look ends at once and
// the test stays open. A look reads the processes of now, not those an
// earlier look read into the buffer it handed back (one gone since).
func TestALookAtALiveTestDoesNotWaitForTheWire(t *testing.T) {
	a, _ := wireAgent(t, models.MODE_TEST)
	ctx := context.Background()
	proc := child(t)
	_, serr := a.ClaimInstance(ctx, models.InstanceReq{Key: "k"}, proc.Process.Pid)
	require.Nil(t, serr)
	_, serr = a.ProvenBegin(ctx, begin("TestLives"))
	require.Nil(t, serr)
	earlier := child(t)
	start, ok := procStart(earlier.Process.Pid)
	require.True(t, ok)
	kill(t, earlier)

	require.NoError(t, a.wire.lock(ctx)) // as a claim, a begin or an end holds it
	ended, done := lookNow(a, []claimant{{pid: earlier.Process.Pid, start: start}}, 5*time.Second)
	a.wire.unlock()
	require.True(t, <-done, "the look found the test open")
	require.True(t, ended, "a look at a live test does not wait for the wire")
	a.provenMu.Lock()
	still := a.proven.openOf(0)
	a.provenMu.Unlock()
	require.Equal(t, "k::TestLives", still)

	_, serr = a.ProvenEnd(ctx, models.ScopeReq{Name: "TestLives", Outcome: "passed"})
	require.Nil(t, serr)
	require.Eventually(t, func() bool { return watches(a) == 0 }, 10*time.Second, 10*time.Millisecond, "with no test open the watch ends")
}

// A look that finds the test's process gone takes the wire, and looks again
// under it before it fails a test: here the next claim held the wire, failed
// the dead process's test itself and claimed the run for the next process,
// whose test then opened. The look fails neither: the dead test was failed
// once, with the claim's reason, and the live one stays open.
func TestALookThatFindsAProcessGoneLooksAgainUnderTheWire(t *testing.T) {
	a, _ := wireAgent(t, models.MODE_TEST)
	ctx := context.Background()
	old := child(t)
	_, serr := a.ClaimInstance(ctx, models.InstanceReq{Key: "k"}, old.Process.Pid)
	require.Nil(t, serr)
	_, serr = a.ProvenBegin(ctx, begin("TestOld"))
	require.Nil(t, serr)

	require.NoError(t, a.wire.lock(ctx)) // the next claim's
	kill(t, old)
	ended, done := lookNow(a, nil, 5*goneWatchEvery)
	require.False(t, ended, "a look that finds the test's process gone waits for the wire")
	// What the next claim does with the wire held: the dead process's test
	// fails, the run is the next process's, and its test opens.
	a.failOpen(ctx, "k::TestOld", "the next claim found its process gone")
	next := child(t)
	start, ok := procStart(next.Process.Pid)
	require.True(t, ok)
	a.provenMu.Lock()
	a.proven.claim = claimant{pid: next.Process.Pid, start: start}
	a.proven.open[0] = scopeTest{id: "k::TestNew", start: time.Now()}
	a.provenMu.Unlock()
	a.wire.unlock()
	require.True(t, <-done, "the look found a test open")
	time.Sleep(5 * goneWatchEvery) // and the watch's own looks, as many

	a.provenMu.Lock()
	open := a.proven.openOf(0)
	delete(a.proven.open, 0) // TestNew never began: no window to end
	a.provenMu.Unlock()
	require.Equal(t, "k::TestNew", open, "the live process's test is not failed for the dead one's")
	w, ok := liveWindow(a, "k::TestOld")
	require.True(t, ok)
	require.Equal(t, models.ScopeOutcomeFailed, w.Outcome)
	require.NotNil(t, w.Evidence)
	require.Contains(t, w.Evidence.Reason, "the next claim found its process gone", "failed once, by the claim")
	require.Eventually(t, func() bool { return watches(a) == 0 }, 10*time.Second, 10*time.Millisecond, "with no test open the watch ends")
}

// BenchmarkALookAtALiveTest is what the watch costs while a test is open:
// one look every goneWatchEvery, which reads the test process's stat line
// and takes no wire. cpu%/tick is one look's share of a CPU at that rate.
// With no test open no watch runs (the rows above), so it costs nothing.
func BenchmarkALookAtALiveTest(b *testing.B) {
	a, _ := wireAgent(b, models.MODE_TEST)
	ctx := context.Background()
	_, serr := a.ClaimInstance(ctx, models.InstanceReq{Key: "k"}, os.Getpid())
	require.Nil(b, serr)
	_, serr = a.ProvenBegin(ctx, begin("TestLive"))
	require.Nil(b, serr)
	var procs []claimant
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var open bool
		if procs, open = a.reapGone(procs); !open {
			b.Fatal("the test closed")
		}
	}
	b.StopTimer()
	perLook := float64(b.Elapsed().Nanoseconds()) / float64(b.N)
	b.ReportMetric(perLook/float64(goneWatchEvery.Nanoseconds())*100, "cpu%/tick")
	_, serr = a.ProvenEnd(ctx, models.ScopeReq{Name: "TestLive", Outcome: "passed"})
	require.Nil(b, serr)
}
