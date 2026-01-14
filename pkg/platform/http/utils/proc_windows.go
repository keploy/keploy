//go:build windows
// +build windows

package utils

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"syscall"

	"go.uber.org/zap"
)

// NewAgentCommand on Windows returns a plain command (no sudo).
// If the agent needs admin, run the parent process with Administrator rights.
func NewAgentCommand(bin string, args []string) *exec.Cmd {
	cmd := exec.Command(bin, args...)
	// Run in a separate process group so Ctrl+C in the parent console doesn't hit the agent.
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP,
	}
	return cmd
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
	out, err := grace.CombinedOutput()
	if err != nil {
		logger.Warn("graceful taskkill failed; attempting force", zap.Int("pid", pid), zap.String("output", strings.TrimSpace(string(out))), zap.Error(err))
	} else {
		logger.Debug("graceful taskkill succeeded", zap.Int("pid", pid), zap.String("output", strings.TrimSpace(string(out))))
		return nil
	}

	// Force if still alive
	force := exec.Command("taskkill", "/PID", strconv.Itoa(pid), "/T", "/F")
	out, err = force.CombinedOutput()
	if err != nil {
		logger.Warn("forced taskkill failed; checking if process already exited", zap.Int("pid", pid), zap.String("output", strings.TrimSpace(string(out))), zap.Error(err))

		// Check if process exists using exit code instead of parsing locale-dependent output.
		// tasklist returns exit code 0 if process found, non-zero if not found.
		tlCmd := exec.Command("tasklist", "/FI", fmt.Sprintf("PID eq %d", pid), "/NH")
		if tlErr := tlCmd.Run(); tlErr != nil {
			// Non-zero exit code means process not found - already gone, which is what we want
			if exitError, ok := tlErr.(*exec.ExitError); ok && exitError.ExitCode() != 0 {
				return nil
			}
		} else {
			// Exit code 0 means process still exists, but taskkill failed - fall through to Process.Kill()
		}

		// Try Process.Kill() as a last resort; tolerate "invalid argument" which indicates the process already exited.
		if cmd.Process != nil {
			if err2 := cmd.Process.Kill(); err2 != nil {
				if strings.Contains(err2.Error(), "invalid argument") {
					// Process already exited, treat as success.
					return nil
				}
				logger.Warn("forced taskkill failed; Process.Kill() failed", zap.Int("pid", pid), zap.Error(err2))
				return err2
			}
			return nil
		}
		return err
	}
	return nil
}
