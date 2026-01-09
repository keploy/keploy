//go:build linux || darwin
// +build linux darwin

package utils

import (
	"os"
	"os/exec"
	"syscall"
	"time"

	"go.uber.org/zap"
)

// NewAgentCommand returns a command that runs elevated on Unix.
// - If already root, we run the binary directly.
// - Otherwise we prefix with "sudo".
// - We put the process in its own group so we can signal the whole group.
func NewAgentCommand(bin string, args []string) *exec.Cmd {
	var cmd *exec.Cmd
	if os.Geteuid() == 0 {
		cmd = exec.Command(bin, args...)
	} else {
		// sudo <bin> <args...>
		all := append([]string{bin}, args...)
		cmd = exec.Command("sudo", all...)
	}

	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true, // new process group: pgid == leader pid
	}
	return cmd
}

// StartCommand simply starts the process; group set via SysProcAttr above.
func StartCommand(cmd *exec.Cmd) error {
	return cmd.Start()
}

// StopCommand tries graceful SIGTERM to the process group, then SIGKILL fallback.
func StopCommand(cmd *exec.Cmd, logger *zap.Logger) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	pid := cmd.Process.Pid

	// Determine pgid (with Setpgid, leader's pgid == pid)
	pgid, err := syscall.Getpgid(pid)
	if err != nil {
		logger.Warn("failed to get pgid; falling back to direct kill", zap.Int("pid", pid), zap.Error(err))
		return stopProcessDirect(cmd, logger)
	}

	// Graceful: SIGTERM group
	if err := syscall.Kill(-pgid, syscall.SIGTERM); err != nil {
		logger.Warn("failed to send SIGTERM to process group", zap.Int("pgid", pgid), zap.Error(err))
	}

	// Wait for process to exit gracefully with timeout
	timeout := 5 * time.Second
	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		// Check if process still exists (signal 0 test)
		if err := syscall.Kill(pid, 0); err != nil {
			// Process is gone (signal 0 test failed)
			logger.Debug("process exited gracefully", zap.Int("pid", pid))
			return nil
		}
		time.Sleep(100 * time.Millisecond) // Poll every 100ms
	}

	// Timeout reached, force kill
	logger.Warn("process did not exit gracefully, forcing kill", zap.Int("pgid", pgid))
	if err := syscall.Kill(-pgid, syscall.SIGKILL); err != nil {
		logger.Error("failed to send SIGKILL to process group", zap.Int("pgid", pgid), zap.Error(err))
		return err
	}

	return nil
}

// stopProcessDirect handles termination of a single process (not a group)
func stopProcessDirect(cmd *exec.Cmd, logger *zap.Logger) error {
	pid := cmd.Process.Pid

	// Graceful: SIGTERM
	err := cmd.Process.Signal(syscall.SIGTERM)
	if err != nil {
		// Process already finished is expected during graceful shutdown, not an error
		if err.Error() == "os: process already finished" {
			logger.Debug("process already finished during graceful shutdown", zap.Int("pid", pid))
			return nil
		}
		logger.Warn("failed to send SIGTERM to process", zap.Int("pid", pid), zap.Error(err))
	}

	// Wait for process to exit gracefully with timeout
	timeout := 5 * time.Second
	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		// Check if process still exists
		if err := syscall.Kill(pid, 0); err != nil {
			// Process is gone
			logger.Debug("process exited gracefully", zap.Int("pid", pid))
			return nil
		}
		time.Sleep(100 * time.Millisecond) // Poll every 100ms
	}

	// Timeout reached, force kill
	logger.Warn("process did not exit gracefully, forcing kill", zap.Int("pid", pid))
	return cmd.Process.Kill()
}
