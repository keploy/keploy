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
