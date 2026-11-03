//go:build linux

package agent

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
)

// mainThreadExitsEnv makes this test binary, started with it set to a
// directory, a process that runs until it is killed and, once a file named
// exit appears in that directory, exits its main thread alone (exit, not
// exit_group). Its stat then reads Z, as a Go program's does while it execs
// from a thread other than its main one, and it runs on.
const mainThreadExitsEnv = "KEPLOY_TEST_AGENT_MAIN_THREAD_EXITS"

func init() {
	dir := os.Getenv(mainThreadExitsEnv)
	if dir == "" {
		return
	}
	// init runs on the main thread: the goroutine stays on it.
	runtime.LockOSThread()
	for {
		if _, err := os.Stat(filepath.Join(dir, "exit")); err == nil {
			break
		}
		time.Sleep(time.Millisecond)
	}
	_, _, _ = syscall.RawSyscall(syscall.SYS_EXIT, 0, 0, 0)
}

// startMainThreadExits starts this test binary as a process that runs until
// the test ends (it is then killed and reaped), and returns it with a func
// that exits its main thread alone and returns once its stat reads Z with
// another thread of it there.
func startMainThreadExits(t *testing.T) (*exec.Cmd, func()) {
	t.Helper()
	dir := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	// No collection (a stop of the world would wait for ever on the main
	// thread, which holds a P it never gives back), and Ps to spare.
	cmd.Env = append(os.Environ(), mainThreadExitsEnv+"="+dir, "GOGC=off", "GOMAXPROCS=4")
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return cmd, func() {
		t.Helper()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "exit"), nil, 0o644))
		waitStat(t, cmd.Process.Pid, "its main thread exited while another thread of it is there",
			func(state byte, threads int) bool { return state == 'Z' && threads > 1 })
	}
}

// exitAll kills process c, every thread of it, and returns once only its
// exited main thread is left, not reaped: a zombie its parent has not
// waited for.
func exitAll(t *testing.T, c *exec.Cmd) {
	t.Helper()
	require.NoError(t, c.Process.Signal(syscall.SIGKILL))
	waitStat(t, c.Process.Pid, "every thread of it exited, and it is not reaped",
		func(state byte, threads int) bool { return state == 'Z' && threads == 1 })
}

// waitStat waits until process pid's stat line has a state and a thread
// count (field 20, num_threads: every thread not yet released) that ok
// takes; a minute turns a hang into a failure.
func waitStat(t *testing.T, pid int, what string, ok func(state byte, threads int) bool) {
	t.Helper()
	deadline := time.Now().Add(time.Minute)
	for {
		b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
		require.NoError(t, err)
		f := strings.Fields(string(b[strings.LastIndexByte(string(b), ')')+1:]))
		threads, err := strconv.Atoi(f[17])
		require.NoError(t, err)
		if ok(f[0][0], threads) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("process %d: waited a minute for %s; its state is %s with %d threads", pid, what, f[0], threads)
		}
		time.Sleep(time.Millisecond)
	}
}

// A Go program that execs from a thread other than its main one (a test
// launcher that claims, then execs the test binary) reads Z in its stat
// from the moment its main thread exits until the exec takes that thread's
// place, and runs throughout. A worker in that state is live: another
// worker's claim leaves its instance alone, its begin is its own, and a
// process in that state can claim. Once every thread of it has exited it is
// gone, reaped or not: its instance ends, and its open test fails.
func TestAWorkerIsLiveUntilEveryThreadOfItHasExited(t *testing.T) {
	a, h := workerAgent(t, models.MODE_RECORD, 4)
	execs, exitMain := startMainThreadExits(t)
	require.Equal(t, "1", claimWorker(t, a, execs.Process.Pid, "a", 3))
	exitMain()
	require.Equal(t, "2", claimWorker(t, a, child(t).Process.Pid, "b", 3))
	require.Empty(t, h.endedIDs(), "a worker whose main thread exited while another thread of it runs is live: a claim leaves its instance alone")

	exited, exitMain := startMainThreadExits(t)
	exitMain()
	require.Equal(t, "3", claimWorker(t, a, exited.Process.Pid, "c", 3), "a process whose main thread exited while another thread of it runs can claim")
	for _, w := range []*exec.Cmd{execs, exited} {
		_, serr := a.ProvenBegin(as(w.Process.Pid), begin("TestRuns"))
		require.Nil(t, serr, "process %d's begin is its own", w.Process.Pid)
	}
	require.Empty(t, h.endedIDs())

	exitAll(t, execs)
	exitAll(t, exited)
	require.Equal(t, "1", claimWorker(t, a, os.Getpid(), "d", 3), "a worker every thread of which has exited is gone, reaped or not")
	require.Equal(t, []uint32{1, 3}, h.endedIDs())
	for _, id := range []string{"a::TestRuns", "c::TestRuns"} {
		w := windowOf(t, a, id)
		require.Equal(t, models.ScopeOutcomeFailed, w.Outcome, id)
		require.Contains(t, w.Evidence.Reason, "the test worker's process exited while the test was open", id)
	}
}

// A process is the one that started then only with that start time: a live
// one read with another is another process on a reused pid. One the kernel
// shows dead (X, being released) or does not show has exited.
func TestAProcessIsAliveOnlyAsTheOneThatStartedThen(t *testing.T) {
	me, ok := procStart(os.Getpid())
	require.True(t, ok)
	require.True(t, procAlive(os.Getpid(), me))
	require.False(t, procAlive(os.Getpid(), me+1), "another start time is another process on the pid")

	was := statOf
	t.Cleanup(func() { statOf = was })
	statOf = func(int) (utils.ProcStat, bool) { return utils.ProcStat{State: 'X', StartTime: me}, true }
	_, ok = procStart(os.Getpid())
	require.False(t, ok, "a process being released has exited")
	require.False(t, procAlive(os.Getpid(), me))

	statOf = func(int) (utils.ProcStat, bool) { return utils.ProcStat{}, false }
	_, ok = procStart(os.Getpid())
	require.False(t, ok, "a process whose stat cannot be read has exited")
	require.False(t, procAlive(os.Getpid(), 0), "a process not there is no process, whatever start time was asked for")
}
