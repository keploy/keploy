package agent

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"golang.org/x/sys/unix"
)

// A test whose worker exited without its end (os.Exit, a -timeout kill) is
// recorded failed when another worker's test begins, which takes the clock;
// its worker's view of the pool is given back.
func TestATestWhoseWorkerExitedLosesTheClock(t *testing.T) {
	if _, err := os.Stat("/proc/self/stat"); err != nil {
		t.Skip("needs /proc")
	}
	t.Setenv(provenswitch.Env, "1")
	h := &clockHooks{}
	useHooks(t, h)
	a := newReplayAgent()
	pool := &workerPool{}
	a.Proxy = pool
	ctx := context.Background()
	worker := exec.Command("cat")
	in, err := worker.StdinPipe()
	require.NoError(t, err)
	require.NoError(t, worker.Start())
	gone := worker.Process.Pid
	require.NoError(t, a.SetScopeTable(ctx, map[string][]string{"pkg::TestA": {"m-a"}}))
	require.NoError(t, a.BeginScopeAt(ctx, "pkg::TestA", gone, time.Now()))
	require.ErrorIs(t, a.BeginScopeAt(ctx, "pkg::TestB", os.Getpid(), time.Now()), ErrOneTestAtATime, "the holder's worker lives")

	require.NoError(t, in.Close())
	require.NoError(t, worker.Wait())
	require.NoError(t, a.BeginScopeAt(ctx, "pkg::TestC", os.Getpid(), time.Now()))
	require.Equal(t, []string{"begin pkg::TestA 0001-01-01T00:00:00Z", "begin pkg::TestC 0001-01-01T00:00:00Z"}, h.moves)
	require.Equal(t, []string{models.ScopeOutcomeFailed}, outcomesOf(t, a, "pkg::TestA"))
	require.Equal(t, []uint32{uint32(gone)}, pool.cleared)
	require.NoError(t, a.EndScopeAt(ctx, "pkg::TestC", os.Getpid(), time.Now()))
	require.Equal(t, "end pkg::TestC", h.moves[len(h.moves)-1])
}

// exitedWorker starts a process, begins nothing, and returns its pid and a
// func that makes it exit and waits until it is a zombie (not reaped), then
// one that reaps it.
func exitedWorker(t *testing.T) (pid int, exit func(), reap func()) {
	t.Helper()
	if _, err := os.Stat("/proc/self/stat"); err != nil {
		t.Skip("needs /proc")
	}
	w := exec.Command("cat")
	in, err := w.StdinPipe()
	require.NoError(t, err)
	require.NoError(t, w.Start())
	reaped := false
	t.Cleanup(func() {
		if !reaped {
			_ = in.Close()
			_ = w.Wait()
		}
	})
	pid = w.Process.Pid
	exit = func() {
		require.NoError(t, in.Close())
		// Returns once the worker has exited, without reaping it.
		var info unix.Siginfo
		require.NoError(t, unix.Waitid(unix.P_PID, pid, &info, unix.WEXITED|unix.WNOWAIT, nil))
	}
	reap = func() {
		reaped = true
		require.NoError(t, w.Wait())
	}
	return pid, exit, reap
}

// A worker that exited but is not reaped yet (a zombie) is gone: its test
// loses the clock to the next.
func TestAZombieWorkersTestLosesTheClock(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := &clockHooks{}
	useHooks(t, h)
	a := newReplayAgent()
	a.Proxy = &workerPool{}
	ctx := context.Background()
	pid, exit, _ := exitedWorker(t)
	require.NoError(t, a.BeginScopeAt(ctx, "pkg::TestA", pid, time.Now()))
	exit()
	require.NoError(t, a.BeginScopeAt(ctx, "pkg::TestC", os.Getpid(), time.Now()))
	require.Equal(t, []string{models.ScopeOutcomeFailed}, outcomesOf(t, a, "pkg::TestA"))
}

// A lost worker's other open tests (a subtest) are recorded failed with it,
// and nothing of the worker stays open.
func TestALostWorkersOpenTestsAllFail(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	useHooks(t, &clockHooks{})
	a := newReplayAgent()
	a.Proxy = &workerPool{}
	ctx := context.Background()
	pid, exit, reap := exitedWorker(t)
	require.NoError(t, a.BeginScopeAt(ctx, "pkg::TestA", pid, time.Now()))
	require.NoError(t, a.BeginScopeAt(ctx, "pkg::TestA/case", pid, time.Now()))
	exit()
	reap()
	require.NoError(t, a.BeginScopeAt(ctx, "pkg::TestC", os.Getpid(), time.Now()))
	require.Equal(t, []string{models.ScopeOutcomeFailed}, outcomesOf(t, a, "pkg::TestA"))
	require.Equal(t, []string{models.ScopeOutcomeFailed}, outcomesOf(t, a, "pkg::TestA/case"))
	for k := range a.workerOpen {
		require.NotEqual(t, uint32(pid), k.pid, "the lost worker's %s stayed open", k.name)
	}
}

// A lost holder is recorded failed once, though the begin that found it
// lost could not move the clock and the next begin does.
func TestALostHolderFailsOnceWhenTheNextMoveFails(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := &clockHooks{}
	useHooks(t, h)
	a := newReplayAgent()
	a.Proxy = &workerPool{}
	ctx := context.Background()
	pid, exit, reap := exitedWorker(t)
	require.NoError(t, a.BeginScopeAt(ctx, "pkg::TestA", pid, time.Now()))
	exit()
	reap()
	h.err = errors.New("disk full")
	require.Error(t, a.BeginScopeAt(ctx, "pkg::TestC", os.Getpid(), time.Now()))
	h.err = nil
	require.NoError(t, a.BeginScopeAt(ctx, "pkg::TestD", os.Getpid(), time.Now()))
	require.Equal(t, []string{models.ScopeOutcomeFailed}, outcomesOf(t, a, "pkg::TestA"))
}

// A test from the holder's pid that is another process now (a reused pid)
// is not the holder's worker: the holder is lost and the new test takes the
// clock.
func TestAReusedPidIsANewWorker(t *testing.T) {
	if procStartTime(os.Getpid()) == "" {
		t.Skip("needs /proc")
	}
	t.Setenv(provenswitch.Env, "1")
	for _, next := range []string{"pkg::TestA", "pkg::TestA/case", "pkg::TestB"} {
		t.Run(next, func(t *testing.T) {
			h := &clockHooks{}
			useHooks(t, h)
			a := newReplayAgent()
			a.Proxy = &workerPool{}
			ctx := context.Background()
			me := os.Getpid()
			require.NoError(t, a.BeginScopeAt(ctx, "pkg::TestA", me, time.Now()))
			a.clockMu.Lock()
			a.clockHeldStart = "1" // the holder was another process on this pid
			a.clockMu.Unlock()
			require.NoError(t, a.BeginScopeAt(ctx, next, me, time.Now()))
			require.Equal(t, []string{"begin pkg::TestA 0001-01-01T00:00:00Z", "begin " + next + " 0001-01-01T00:00:00Z"}, h.moves)
			require.Equal(t, scopeKey{pid: uint32(me), name: next}, a.clockHeld)
		})
	}
}

// A new process on a lost worker's pid keeps its own open tests: only the
// lost worker's are failed.
func TestALostWorkersSweepSparesANewProcessOnItsPid(t *testing.T) {
	if procStartTime(os.Getpid()) == "" {
		t.Skip("needs /proc")
	}
	t.Setenv(provenswitch.Env, "1")
	useHooks(t, &clockHooks{})
	a := newReplayAgent()
	a.Proxy = &workerPool{}
	ctx := context.Background()
	me := os.Getpid()
	require.NoError(t, a.BeginScopeAt(ctx, "pkg::TestA", me, time.Now()))
	a.clockMu.Lock()
	a.clockHeldStart = "1" // the holder was another process on this pid
	a.clockMu.Unlock()
	a.scopeMu.Lock()
	a.openProc[scopeKey{pid: uint32(me), name: "pkg::TestA"}] = "1"
	a.scopeMu.Unlock()
	a.NoteScope("pkgB", me, "", true)
	require.NoError(t, a.BeginScopeAt(ctx, "pkgB", me, time.Now()))
	require.NoError(t, a.BeginScopeAt(ctx, "pkgB::TestB", me, time.Now()))
	require.NoError(t, a.EndScopeAt(ctx, "pkgB::TestB", me, time.Now()))
	require.NoError(t, a.EndScopeAt(ctx, "pkgB", me, time.Now()))
	require.Equal(t, []string{models.ScopeOutcomeFailed}, outcomesOf(t, a, "pkg::TestA"))
	require.Equal(t, []string{""}, outcomesOf(t, a, "pkgB"))
}

// A lost worker's narrowed view is given back though only its subtest, not
// the test that held the clock, had a mapping.
func TestALostWorkersSubtestPoolIsGivenBack(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	useHooks(t, &clockHooks{})
	a := newReplayAgent()
	pool := &workerPool{}
	a.Proxy = pool
	ctx := context.Background()
	pid, exit, reap := exitedWorker(t)
	require.NoError(t, a.SetScopeTable(ctx, map[string][]string{"pkg::TestA/case": {"m-1"}}))
	require.NoError(t, a.BeginScopeAt(ctx, "pkg::TestA", pid, time.Now()))
	require.NoError(t, a.BeginScopeAt(ctx, "pkg::TestA/case", pid, time.Now()))
	exit()
	reap()
	require.NoError(t, a.BeginScopeAt(ctx, "pkg::TestC", os.Getpid(), time.Now()))
	require.Equal(t, []uint32{uint32(pid)}, pool.narrowed)
	require.Equal(t, []uint32{uint32(pid)}, pool.cleared)
}

// procStartTime sees a live process of the agent's pid namespace, and
// nothing for no pid, pid 1, or a kernel thread.
func TestProcStartTime(t *testing.T) {
	if _, err := os.Stat("/proc/self/stat"); err != nil {
		t.Skip("needs /proc")
	}
	require.NotEmpty(t, procStartTime(os.Getpid()))
	require.Empty(t, procStartTime(0))
	require.Empty(t, procStartTime(1))
	if b, err := os.ReadFile("/proc/2/stat"); err == nil && strings.Contains(string(b), "(kthreadd)") {
		require.Empty(t, procStartTime(2), "a kernel thread")
	}
}

// Under the proven switch the single-worker wire opens and closes each test
// (ProvenBegin, ProvenEnd), with no pid: its begin moves the run's clock to
// the test's recorded begin, and its end moves it back. A test still open
// when the run's windows are read (its process exited, or outlived the test
// command) is failed with the clock moved back. A wire begin that cannot move
// the clock, or whose pool cannot narrow once it moved, opens nothing, leaves
// the clock where it was, and records the test failed.
func TestTheWireMovesTheClockPerTest(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := &clockHooks{}
	useHooks(t, h)
	a, _ := wireAgent(models.MODE_TEST)
	storeMocks(a)
	ctx := context.Background()
	h.file = "/tmp/kclk1/t"
	resp, serr := a.ClaimInstance(ctx, models.InstanceReq{Key: "o"}, os.Getpid())
	require.Nil(t, serr)
	require.Equal(t, "/tmp/kclk1/t", resp.TimeFile, "the claim names the clock record the run's processes read")
	b := time.Date(2026, 10, 6, 9, 1, 0, 0, time.UTC)
	require.NoError(t, a.SetTestBegins(ctx, map[string]time.Time{"o::TestA": b}, nil))

	_, serr = a.ProvenBegin(ctx, begin("TestA"))
	require.Nil(t, serr)
	_, serr = a.ProvenEnd(ctx, models.ScopeReq{Name: "TestA", Outcome: models.ScopeOutcomePassed})
	require.Nil(t, serr)
	require.Equal(t, []string{models.ScopeOutcomePassed}, outcomesOf(t, a, "o::TestA"))

	h.err = errors.New("disk full")
	_, serr = a.ProvenBegin(ctx, begin("TestB"))
	require.NotNil(t, serr, "a test that cannot run on its recording's clock is refused")
	h.err = nil
	require.Zero(t, a.clockHeld)

	a.clientMocks.Delete(uint64(0)) // its pool cannot narrow, after its clock moved
	_, serr = a.ProvenBegin(ctx, begin("TestD"))
	require.NotNil(t, serr, "a test whose pool cannot narrow is refused, with the clock moved back")
	storeMocks(a)
	require.Zero(t, a.clockHeld)
	require.Zero(t, a.clockMoved)

	_, serr = a.ProvenBegin(ctx, begin("TestC"))
	require.Nil(t, serr)
	require.NoError(t, a.reapOpenTest(ctx))
	require.Equal(t, []string{models.ScopeOutcomeFailed}, outcomesOf(t, a, "o::TestB"), "a test refused its recording's clock is recorded failed")
	require.Equal(t, []string{models.ScopeOutcomeFailed}, outcomesOf(t, a, "o::TestD"))
	require.Equal(t, models.ScopeOutcomeFailed, windowOf(t, a, "o::TestC").Outcome)
	require.Zero(t, a.clockHeld)
	require.Zero(t, a.clockMoved)
	require.Equal(t, []string{
		"begin o::TestA 2026-10-06T09:01:00Z", "end o::TestA",
		"begin o::TestB 0001-01-01T00:00:00Z",
		"begin o::TestD 0001-01-01T00:00:00Z", "end o::TestD",
		"begin o::TestC 0001-01-01T00:00:00Z", "end o::TestC",
	}, h.moves)
}

// A begin of the previous session that lands after a new session released
// the clock, and before it reset the wire, takes the clock; the reset gives
// it back, under the wire, or every begin of the new session would be
// refused while that forgotten test held it.
func TestASessionResetGivesBackAClockTakenAfterTheRelease(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := &clockHooks{}
	useHooks(t, h)
	a, _ := wireAgent(models.MODE_TEST)
	storeMocks(a)
	ctx := context.Background()
	claimSelf(t, a)
	require.NoError(t, a.releaseTestClock(ctx))
	_, serr := a.ProvenBegin(ctx, begin("TestOld"))
	require.Nil(t, serr)
	require.NoError(t, a.resetSession(ctx))
	require.Zero(t, a.clockHeld)
	require.Zero(t, a.clockMoved)
	require.Equal(t, []string{"begin o::TestOld 0001-01-01T00:00:00Z", "end o::TestOld"}, h.moves)

	claimSelf(t, a)
	_, serr = a.ProvenBegin(ctx, begin("TestNew"))
	require.Nil(t, serr, "the new session's test begins")
}
