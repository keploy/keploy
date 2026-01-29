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
// - Uses Setpgid for process group management.
func NewAgentCommand(bin string, args []string, _ bool, env []string) *exec.Cmd {
	var cmd *exec.Cmd
	if os.Geteuid() == 0 {
		cmd = exec.Command(bin, args...)
		if len(env) > 0 {
			cmd.Env = append(os.Environ(), env...)
		}
	} else {
		// sudo [ENV=VAL...] <bin> <args...>
		// We prepend env vars to the command arguments for sudo
		all := make([]string, 0, len(env)+1+len(args))
		all = append(all, env...)
		all = append(all, bin)
		all = append(all, args...)
		cmd = exec.Command("sudo", all...)
	}

	// Always use Setpgid for process group management
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
	}
	return cmd
}

// StartCommand simply starts the process.
func StartCommand(cmd *exec.Cmd) error {
	return cmd.Start()
}

// StopCommand tries graceful SIGTERM to the process group, then SIGKILL fallback.
func StopCommand(cmd *exec.Cmd, logger *zap.Logger) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	pid := cmd.Process.Pid

	pgid, err := syscall.Getpgid(pid)
	if err != nil {
		logger.Warn("failed to get pgid; falling back to direct kill", zap.Int("pid", pid), zap.Error(err))
		// Graceful
		err = cmd.Process.Signal(syscall.SIGTERM)
		if err != nil {
			// Process already finished is expected during graceful shutdown, not an error
			if err.Error() == "os: process already finished" {
				logger.Debug("process already finished during graceful shutdown", zap.Int("pid", pid))
				return nil
			}
			logger.Warn("failed to send SIGTERM to process; falling back to kill", zap.Int("pid", pid), zap.Error(err))
		}
		time.Sleep(10 * time.Second)
		// Force
		return cmd.Process.Kill()
	}

	logger.Debug("sending SIGTERM to process group", zap.Int("pid", pid), zap.Int("pgid", pgid))

	// Graceful: SIGTERM group (negative pgid sends to all processes in the group)
	if err := syscall.Kill(-pgid, syscall.SIGTERM); err != nil {
		logger.Warn("failed to send SIGTERM to process group", zap.Int("pgid", pgid), zap.Error(err))
	}

	return nil
}
