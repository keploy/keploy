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
	// still kills the sleep the command leaves in its group.
	pgid := 0
	t.Cleanup(func() {
		if pgid == 0 {
			pgid = groupOfPidFile(pidFile)
		}
		if pgid > 1 {
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
		}
	})

	appErr := ap.Run(context.Background())
	require.Equal(t, models.ErrAppStopped, appErr.AppErrorType, "%v", appErr.Err)
	raw, err := os.ReadFile(pidFile)
	require.NoError(t, err, "the command wrote no sleep pid")
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	require.NoError(t, err)
	pgid, err = syscall.Getpgid(pid)
	require.NoError(t, err, "the sleep the command left is gone")

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

// groupOfPidFile reads the process group of the pid a command wrote to
// path, or 0 when there is no such file, pid or process.
func groupOfPidFile(path string) int {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pid <= 1 {
		return 0
	}
	pgid, err := syscall.Getpgid(pid)
	if err != nil {
		return 0
	}
	return pgid
}
