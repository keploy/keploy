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
		// Graceful
		err = cmd.Process.Signal(syscall.SIGTERM)
		if err != nil {
			logger.Error("failed to send SIGTERM to process; falling back to kill", zap.Int("pid", pid), zap.Error(err))
		}
		time.Sleep(3 * time.Second)
		// Force
		return cmd.Process.Kill()
	}

	// Graceful: SIGTERM group
	if err := syscall.Kill(-pgid, syscall.SIGTERM); err != nil {
		logger.Warn("failed to send SIGTERM to process group", zap.Int("pgid", pgid), zap.Error(err))
	}

	return nil
}
