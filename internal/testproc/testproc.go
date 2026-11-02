//go:build unix

// Package testproc starts a process a test runs (an app, a helper, a
// stand-in) so that it does not outlive the test: it runs in a process group
// of its own, which the test's cleanup kills, and on Linux the kernel kills
// it too when the test's process ends with no cleanup run, as a timeout's
// panic, a crash or a kill at a job's end leave them.
//
// A process it starts in turn is killed with its group by the cleanup, but
// not by the kernel when the test's process dies: it ends on its own, as a
// shell's short sleep does, or looks for its owner, as starvetest's spins
// do. On other systems there is no such kill, and the started process
// outlives a test's process that ends with no cleanup run.
package testproc

import (
	"os/exec"
	"runtime"
	"syscall"
	"testing"
)

// Start starts cmd in a process group of its own and returns a channel
// closed once cmd has exited and been reaped; cmd.ProcessState is set by
// then, and the caller does not Wait. It sets cmd.SysProcAttr. When t ends,
// its cleanup kills the group and waits for the reap, so a test that fails
// or ends early leaves none of the group running.
//
// On Linux the kernel kills cmd as the thread that started it ends
// (PR_SET_PDEATHSIG), and so with the test's process, however that ends.
// That thread is one a goroutine keeps, locked, until cmd is reaped: a
// thread the runtime shares could be taken by a goroutine that locks it and
// exits, which ends the thread, and cmd with it, while the test runs.
func Start(t testing.TB, cmd *exec.Cmd) <-chan struct{} {
	t.Helper()
	cmd.SysProcAttr = procAttr()
	started := make(chan error, 1)
	reaped := make(chan struct{})
	go func() {
		// Never unlocked: the runtime ends the thread with this goroutine,
		// once cmd is reaped or failed to start.
		runtime.LockOSThread()
		err := cmd.Start()
		started <- err
		if err != nil {
			return
		}
		_ = cmd.Wait()
		close(reaped)
	}()
	if err := <-started; err != nil {
		t.Fatalf("start %s: %v", cmd.Path, err)
	}
	pgid := cmd.Process.Pid
	t.Cleanup(func() {
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
		<-reaped
	})
	return reaped
}
