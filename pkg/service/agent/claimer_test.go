//go:build linux || darwin

package agent

import (
	"io"
	"os"
	"os/exec"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
)

// holdStdinEnv makes this test binary, run again as a child with it set,
// wait until its stdin closes and exit, before any test runs.
const holdStdinEnv = "KEPLOY_TEST_AGENT_HOLD_STDIN"

func init() {
	if os.Getenv(holdStdinEnv) == "" {
		return
	}
	_, _ = io.Copy(io.Discard, os.Stdin)
	os.Exit(0)
}

// ProcStart and ProcAlive are the agent's own reads of a process's start
// time and liveness: a live process is alive as the one that started then
// and not with another start time, and once it has exited and been reaped
// neither read finds it.
func TestProcStartAndProcAliveReadAProcessAsTheAgentDoes(t *testing.T) {
	me, ok := ProcStart(os.Getpid())
	require.True(t, ok, "this process has a start time")
	require.NotZero(t, me)
	require.True(t, ProcAlive(os.Getpid(), me))
	require.False(t, ProcAlive(os.Getpid(), me+1), "another start time is another process on the pid")

	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), holdStdinEnv+"=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	in, err := cmd.StdinPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			_ = cmd.Wait()
		}
	})
	pid := cmd.Process.Pid
	start, ok := ProcStart(pid)
	require.True(t, ok, "a running child has a start time")
	require.True(t, ProcAlive(pid, start), "a running child is alive")

	require.NoError(t, in.Close())
	require.NoError(t, cmd.Wait())
	// The kernel may give pid to a new process by now: only one with the
	// child's own start time would be the child still found.
	if got, ok := ProcStart(pid); ok {
		require.NotEqual(t, start, got, "a child that exited and was reaped has no start time")
	}
	require.False(t, ProcAlive(pid, start), "a child that exited is not alive")
}
