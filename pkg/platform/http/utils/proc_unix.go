//go:build linux || darwin
// +build linux darwin

package utils

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"

	"go.uber.org/zap"
)

// EnsureSudoAccess checks if we need sudo and pre-authenticates if necessary.
// This should be called before NewAgentCommand to ensure sudo credentials are cached.
// Returns nil if running as root or if sudo auth succeeded, error otherwise.
func EnsureSudoAccess(logger *zap.Logger) error {
	// Already root, no sudo needed
	if os.Geteuid() == 0 {
		return nil
	}

	// Check if sudo credentials are already cached (non-interactive check)
	checkCmd := exec.Command("sudo", "-n", "true")
	if err := checkCmd.Run(); err == nil {
		// Credentials already cached
		return nil
	}

	// Need to prompt for password - ensure we have a terminal
	logger.Info("Root privileges required. Please enter your password.")

	// Use sudo -v to validate/cache credentials with proper terminal handling
	authCmd := exec.Command("sudo", "-v")
	authCmd.Stdin = os.Stdin
	authCmd.Stdout = os.Stdout
	authCmd.Stderr = os.Stderr

	if err := authCmd.Run(); err != nil {
		return fmt.Errorf("failed to obtain sudo privileges: %w. Please run with 'sudo' or ensure you have sudo access", err)
	}

	logger.Info("Sudo credentials cached successfully")
	return nil
}

// NewAgentCommand returns a command that runs elevated on Unix.
// - If already root, we run the binary directly.
// - Otherwise we prefix with "sudo -n" (non-interactive, using cached credentials).
// - Call EnsureSudoAccess() before this to ensure credentials are cached.
// - We put the process in its own group so we can signal the whole group.
func NewAgentCommand(bin string, args []string) *exec.Cmd {
	var cmd *exec.Cmd
	if os.Geteuid() == 0 {
		cmd = exec.Command(bin, args...)
	} else {
		// sudo -n (non-interactive) <bin> <args...>
		// -n flag uses cached credentials without prompting
		all := append([]string{"-n", bin}, args...)
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
			// Process already finished is expected during graceful shutdown, not an error
			if err.Error() == "os: process already finished" {
				logger.Debug("process already finished during graceful shutdown", zap.Int("pid", pid))
				return nil
			}
			logger.Warn("failed to send SIGTERM to process; falling back to kill", zap.Int("pid", pid), zap.Error(err))
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
