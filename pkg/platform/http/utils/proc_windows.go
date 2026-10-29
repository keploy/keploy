//go:build windows
// +build windows

package utils

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"

	keployutils "go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// NewAgentCommand on Windows returns a plain command (no sudo).
// If the agent needs admin, run the parent process with Administrator rights.
// The useCachedCreds parameter is ignored on Windows.
func NewAgentCommand(bin string, args []string, useCachedCreds bool) *exec.Cmd {
	cmd := exec.Command(bin, args...)
	// Run in a separate process group so Ctrl+C in the parent console doesn't hit the agent.
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP,
	}
	return cmd
}

// NewAgentCommandForPTY on Windows is the same as NewAgentCommand since PTY is not supported.
func NewAgentCommandForPTY(bin string, args []string) *exec.Cmd {
	return NewAgentCommand(bin, args, false)
}

// NeedsPTY on Windows always returns false since PTY is not supported.
func NeedsPTY() bool {
	return false
}

func StartCommand(cmd *exec.Cmd) error {
	return cmd.Start()
}

// PTYHandle represents a PTY session (stub for Windows)
type PTYHandle struct {
	cmd     *exec.Cmd
	console *keployutils.ChildConsole
	logger  *zap.Logger
}

// StartCommandWithPTY on Windows falls back to regular command execution since PTY is not supported.
func StartCommandWithPTY(cmd *exec.Cmd, logger *zap.Logger) (*PTYHandle, error) {
	// Its output reaches keploy's console cut of the values the log redactor
	// holds, when it holds any (ChildConsole).
	console, err := keployutils.SetChildConsole(cmd)
	if err != nil {
		return nil, err
	}
	cmd.Stdin = os.Stdin

	err = cmd.Start()
	console.Started()
	if err != nil {
		return nil, err
	}

	return &PTYHandle{
		cmd:     cmd,
		console: console,
		logger:  logger,
	}, nil
}

// Wait waits for the command to finish, and writes out all it printed.
func (h *PTYHandle) Wait() error {
	err := h.cmd.Wait()
	h.console.Exited()
	return err
}

// GetProcess returns the underlying process for signal handling
func (h *PTYHandle) GetProcess() *os.Process {
	if h.cmd != nil {
		return h.cmd.Process
	}
	return nil
}

// TerminateCommand stops cmd's process tree. Windows has no SIGTERM to send
// and keploy attaches no uprobes there, so it is StopCommand.
func TerminateCommand(cmd *exec.Cmd, logger *zap.Logger) error {
	return StopCommand(cmd, logger)
}

// KillAgentCommand ends cmd's process tree at once (StopCommand), now or not.
func KillAgentCommand(cmd *exec.Cmd, logger *zap.Logger, _ bool) error {
	return StopCommand(cmd, logger)
}

// KillAgentReach says what KillAgentCommand does to the agent cmd started:
// it ends it at once, now or not.
func KillAgentReach(*exec.Cmd) (firstAtOnce, noAtOnce bool) { return true, false }

// KillPTYReach is KillAgentReach: Windows has no PTY.
func KillPTYReach(*PTYHandle) (firstAtOnce, noAtOnce bool) { return true, false }

// KillPTYCommand is KillAgentCommand: Windows has no PTY.
func KillPTYCommand(handle *PTYHandle, logger *zap.Logger, now bool) error {
	if handle == nil || handle.cmd == nil {
		return nil
	}
	return KillAgentCommand(handle.cmd, logger, now)
}

// StdinOff is always false on Windows, which has no PTY.
func StdinOff(*PTYHandle) bool { return false }

// RestoreTerminal has no terminal state to restore on Windows.
func (h *PTYHandle) RestoreTerminal() {}

// TerminatePTYCommand on Windows is TerminateCommand: PTY is not supported.
func TerminatePTYCommand(handle *PTYHandle, logger *zap.Logger) error {
	if handle == nil || handle.cmd == nil {
		return nil
	}
	return TerminateCommand(handle.cmd, logger)
}

// ConfigureCommandForPTY configures the command's SysProcAttr for PTY execution.
// On Windows, this is a no-op since PTY is not supported in the same way.
func ConfigureCommandForPTY(cmd *exec.Cmd) {
	// On Windows, PTY is not supported in the same way as Unix.
	// The command will run with stdin/stdout/stderr connected directly.
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
		logger.Debug("graceful taskkill failed; attempting force", zap.Int("pid", pid), zap.String("output", strings.TrimSpace(string(out))), zap.Error(err))
	} else {
		logger.Debug("graceful taskkill succeeded", zap.Int("pid", pid), zap.String("output", strings.TrimSpace(string(out))))
		return nil
	}

	// Force if still alive
	force := exec.Command("taskkill", "/PID", strconv.Itoa(pid), "/T", "/F")
	out, err = force.CombinedOutput()
	if err != nil {
		logger.Debug("forced taskkill failed; checking if process already exited", zap.Int("pid", pid), zap.String("output", strings.TrimSpace(string(out))), zap.Error(err))

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
				logger.Debug("forced taskkill failed; Process.Kill() failed", zap.Int("pid", pid), zap.Error(err2))
				return err2
			}
			return nil
		}
		return err
	}
	return nil
}

// SetOwnProcessGroup starts cmd in a process group of its own
// (CREATE_NEW_PROCESS_GROUP), which disables a console's Ctrl+C for it.
func SetOwnProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= syscall.CREATE_NEW_PROCESS_GROUP
}
