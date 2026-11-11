//go:build darwin

package app

import (
	"context"
	"os/exec"
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
	a := &App{logger: zap.NewNop(), docker: &noContainers{}, kind: utils.Native, cmd: "sleep 60 & exit 0"}
	var before bool
	assert.NotPanics(t, func() { before = a.GroupRunsOn() })
	assert.False(t, before, "an app not run yet runs on")
	appErr := a.run(context.Background())
	require.Equal(t, models.ErrAppStopped, appErr.AppErrorType, "%v", appErr.Err)
	cmd := a.native.Load()
	require.NotNil(t, cmd, "the run kept no command")
	pgid := cmd.Process.Pid
	t.Cleanup(func() { _ = syscall.Kill(-pgid, syscall.SIGKILL) })
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
