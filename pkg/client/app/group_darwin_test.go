//go:build darwin

package app

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// On macOS keploy adopts no daemon, so a run's caller waits on the app
// after its command exits only while a process the command started runs on
// in the command's own process group: before the run, none; after a command
// that left a sleep in its group, that sleep; once the sleep ends, none.
func TestANativeAppRunsOnInItsCommandsGroup(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "sleep.pid")
	a := &App{logger: zap.NewNop(), docker: &noContainers{}, kind: utils.Native,
		cmd: "sleep 60 & echo $! > '" + pidFile + "'; exit 0"}
	var before bool
	assert.NotPanics(t, func() { before = a.GroupRunsOn() })
	assert.False(t, before, "an app not run yet runs on")
	// Registered before the run, so a require that fails anywhere below
	// still kills the sleep the command leaves.
	t.Cleanup(func() { killLeftSleep(pidFile) })
	appErr := a.run(context.Background())
	require.Equal(t, models.ErrAppStopped, appErr.AppErrorType, "%v", appErr.Err)
	cmd := a.native.Load()
	require.NotNil(t, cmd, "the run kept no command")
	pgid := cmd.Process.Pid
	require.True(t, a.GroupRunsOn(), "the command exited and left a sleep in its group: the app does not run on")
	require.NoError(t, syscall.Kill(-pgid, syscall.SIGKILL))
	require.Eventually(t, func() bool { return !a.GroupRunsOn() }, 10*time.Second, 20*time.Millisecond, "the app runs on after its group was killed")
}

// A command kept before it started (the run keeps it as it sets its cancel
// up, before the start) leads no group yet: the app does not run on.
func TestAnAppWhoseCommandNeverStartedDoesNotRunOn(t *testing.T) {
	a := &App{logger: zap.NewNop()}
	a.native.Store(exec.Command("sh", "-c", "exit 0"))
	var runs bool
	require.NotPanics(t, func() { runs = a.GroupRunsOn() })
	require.False(t, runs, "a command that never started runs on")
}

// killLeftSleep kills the sleep whose pid a command wrote to path, if it
// still runs: its whole process group when that group is its own, and the
// sleep alone otherwise. It never signals this test's own group: a native
// command that lost its Setpgid leaves the sleep in go test's group, where a
// group kill would kill go test.
func killLeftSleep(path string) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pid <= 1 {
		return
	}
	pgid, err := syscall.Getpgid(pid)
	if err != nil {
		return
	}
	if pgid > 1 && pgid != syscall.Getpgrp() {
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
		return
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
}
