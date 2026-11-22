//go:build darwin

package http

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/client/app"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// On macOS keploy adopts no daemon, so the run waits on the application
// before it stops it (beforeStop) only while appRunsOn reads a process the
// command started running on in the command's own process group: a native
// app whose command exited and left a sleep in its group runs on, and once
// the sleep ends it does not.
func TestAppRunsOnOffLinuxReadsTheCommandsGroup(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "sleep.pid")
	ap := app.NewApp(zap.NewNop(), "sleep 60 & echo $! > '"+pidFile+"'; exit 0", nil,
		models.SetupOptions{CommandType: string(utils.Native)})
	require.False(t, appRunsOn(zap.NewNop(), ap), "an app not run yet runs on")

	// Registered before the run, so a require that fails anywhere below
	// still kills the sleep the command leaves.
	t.Cleanup(func() { killLeftSleep(pidFile) })

	appErr := ap.Run(context.Background())
	require.Equal(t, models.ErrAppStopped, appErr.AppErrorType, "%v", appErr.Err)
	raw, err := os.ReadFile(pidFile)
	require.NoError(t, err, "the command wrote no sleep pid")
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	require.NoError(t, err)
	pgid, err := syscall.Getpgid(pid)
	require.NoError(t, err, "the sleep the command left is gone")
	require.True(t, pgid > 1 && pgid != syscall.Getpgrp(),
		"the sleep runs in group %d, this test's own or none: the native command was not given a group of its own", pgid)

	require.True(t, appRunsOn(zap.NewNop(), ap),
		"the command exited and left a sleep in its group: the run would stop the app without waiting")
	ran := false
	beforeStop(context.Background(), func(_ context.Context, running func() bool) {
		ran = true
		require.True(t, running(), "the wait was handed a running that says nothing runs")
	}, func() bool { return appRunsOn(zap.NewNop(), ap) })
	require.True(t, ran, "the run did not wait on an app that runs on")

	require.NoError(t, syscall.Kill(-pgid, syscall.SIGKILL))
	require.Eventually(t, func() bool { return !appRunsOn(zap.NewNop(), ap) }, 10*time.Second, 20*time.Millisecond,
		"the app runs on after its group was killed")
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
