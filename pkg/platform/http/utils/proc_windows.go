//go:build windows
// +build windows

package utils

import (
	"os/exec"
	"strconv"

	"go.uber.org/zap"
)

// NewAgentCommand on Windows returns a plain command (no sudo).
// If the agent needs admin, run the parent process with Administrator rights.
func NewAgentCommand(bin string, args []string) *exec.Cmd {
	return exec.Command(bin, args...)
}

func StartCommand(cmd *exec.Cmd) error {
	return cmd.Start()
}

// StopCommand uses "taskkill" to terminate the process tree:
// 1) try graceful (without /F), 2) fallback to force (/F).
func StopCommand(cmd *exec.Cmd, logger *zap.Logger) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	pid := cmd.Process.Pid

	// Try graceful: sends CTRL_CLOSE to console apps and WM_CLOSE to windows where possible.
	grace := exec.Command("taskkill", "/PID", strconv.Itoa(pid), "/T")
	if err := grace.Run(); err != nil {
		logger.Warn("graceful taskkill failed; forcing", zap.Int("pid", pid), zap.Error(err))
	}

	// Force if still alive
	force := exec.Command("taskkill", "/PID", strconv.Itoa(pid), "/T", "/F")
	if err := force.Run(); err != nil {
		logger.Warn("forced taskkill failed; falling back to Process.Kill()", zap.Int("pid", pid), zap.Error(err))
		return cmd.Process.Kill()
	}
	return nil
}
