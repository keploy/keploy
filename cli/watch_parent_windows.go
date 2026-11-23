//go:build windows

package cli

import (
	"context"
	"errors"
	"time"

	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"golang.org/x/sys/windows"
)

// watchParentInterval is how often the watchdog looks at the client between
// its checks of ctx: each look waits on the client's process for up to this
// long, so its exit is seen at once.
const watchParentInterval = time.Second

// The watchdog's reach into Windows, replaced by tests.
var (
	// openClient opens the client's process for waiting on it and reading
	// its start time, which the signed-in user may do for a process of its
	// own: no administrator needed.
	openClient = func(pid uint32) (windows.Handle, error) {
		return windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	}
	// processStarted is when the process h names started, in nanoseconds
	// since 1601 (GetProcessTimes).
	processStarted = func(h windows.Handle) (int64, error) {
		var created, exited, kernel, user windows.Filetime
		if err := windows.GetProcessTimes(h, &created, &exited, &kernel, &user); err != nil {
			return 0, err
		}
		return created.Nanoseconds(), nil
	}
	waitClient = windows.WaitForSingleObject
)

// watchParentProcess ends this agent when the keploy client that started it
// (clientPID, its --client-pid) exits, however it exits: Task Manager's End
// task, `taskkill /F`, a crash, or the VS Code extension killing it.
//
// A native Windows agent outlived its client: it kept keploy's proxy and DNS
// ports, so the next run could not take them, and it held the run's job
// object, whose processes the kernel ends only with its last handle
// (enterprise's run job: JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE), so the
// application keploy started kept running too.
//
// The client's process handle is opened once and waited on: the handle names
// that one process even after its pid is reused. A pid already reused when
// the agent opens it names a process that started after the agent did, which
// the client, having started the agent, cannot be: the client is gone. A
// process that started in the same tick of the clock the start times are cut
// to (about 15.6 ms) as the agent is taken for the client.
//
// Windows has no signal for a process to send itself, so the agent stops as
// its stop over its API (POST /agent/stop) stops it: utils.Stop, which cancels
// its root context, and its ordinary teardown runs. What a stop already
// under way holds for (utils.SetSignalHold) ends with that cancel too.
func watchParentProcess(ctx context.Context, logger *zap.Logger, clientPID int) {
	if logger == nil {
		logger = zap.NewNop()
	}
	logger.Debug("arming parent-death watchdog", zap.Int("clientPID", clientPID))
	if clientPID <= 0 {
		return
	}
	client, err := openClient(uint32(clientPID))
	if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
		// No process has the pid: the client has exited already.
		logger.Info("the keploy client that started this agent has exited; stopping the agent", zap.Int("clientPID", clientPID))
		endForGoneClient(logger)
		return
	}
	if err != nil {
		logger.Warn("cannot watch the keploy client that started this agent: if that client is killed, stop this agent yourself, or it and the application it runs keep running",
			zap.Int("clientPID", clientPID), zap.Error(err))
		return
	}
	if startedAfterAgent(client) {
		_ = windows.CloseHandle(client)
		logger.Info("the keploy client that started this agent has exited (its pid now names a process that started after this agent); stopping the agent",
			zap.Int("clientPID", clientPID))
		endForGoneClient(logger)
		return
	}
	go func() {
		defer func() { _ = windows.CloseHandle(client) }()
		for ctx.Err() == nil {
			ev, err := waitClient(client, uint32(watchParentInterval/time.Millisecond))
			if err != nil {
				logger.Warn("stopped watching the keploy client that started this agent: if that client is killed, stop this agent yourself, or it and the application it runs keep running",
					zap.Int("clientPID", clientPID), zap.Error(err))
				return
			}
			if ev == windows.WAIT_OBJECT_0 {
				logger.Info("the keploy client that started this agent has exited; stopping the agent to release its ports and end the application it runs",
					zap.Int("clientPID", clientPID))
				endForGoneClient(logger)
				return
			}
		}
	}()
}

// startedAfterAgent reports whether the process client names started after
// this agent did. A start time it cannot read says no: the process is then
// watched as the client, which ends the agent no later than its own exit.
func startedAfterAgent(client windows.Handle) bool {
	own, err := processStarted(windows.CurrentProcess())
	if err != nil {
		return false
	}
	started, err := processStarted(client)
	if err != nil {
		return false
	}
	return started > own
}

// endForGoneClient stops this agent once its client is gone, as its stop
// over its API does (utils.Stop).
var endForGoneClient = func(logger *zap.Logger) {
	_ = utils.Stop(logger, "the keploy client that started this agent has exited")
}
