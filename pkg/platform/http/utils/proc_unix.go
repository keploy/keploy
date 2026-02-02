//go:build linux || darwin
// +build linux darwin

package utils

import (
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	"go.uber.org/zap"
	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

// NewAgentCommand returns a command that runs elevated on Unix.
// - If already root, we run the binary directly.
// - Otherwise we prefix with "sudo".
// - If useCachedCreds is true, uses "sudo -n" (non-interactive) which relies on cached credentials.
// - We put the process in its own group so we can signal the whole group.
func NewAgentCommand(bin string, args []string, useCachedCreds bool) *exec.Cmd {
	var cmd *exec.Cmd
	if os.Geteuid() == 0 {
		cmd = exec.Command(bin, args...)
	} else {
		if useCachedCreds {
			// sudo -n <bin> <args...> - non-interactive, uses cached credentials
			all := append([]string{"-n", bin}, args...)
			cmd = exec.Command("sudo", all...)
		} else {
			// sudo <bin> <args...> - may prompt for password
			all := append([]string{bin}, args...)
			cmd = exec.Command("sudo", all...)
		}
	}

	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true, // new process group: pgid == leader pid
	}
	return cmd
}

// NewAgentCommandForPTY returns a command configured for PTY execution.
// Uses Setsid instead of Setpgid to allow PTY to become the controlling terminal.
func NewAgentCommandForPTY(bin string, args []string) *exec.Cmd {
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
		Setsid: true, // create new session for PTY
	}
	return cmd
}

// NeedsPTY returns true if the command needs PTY for interactive input (e.g., sudo password).
func NeedsPTY() bool {
	// Need PTY when not root (sudo may prompt for password) and stdin is a TTY
	return os.Geteuid() != 0 && term.IsTerminal(int(os.Stdin.Fd()))
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

// PTYHandle represents a PTY session for interactive command execution
type PTYHandle struct {
	ptmx         *os.File
	cmd          *exec.Cmd
	logger       *zap.Logger
	resizeCh     chan os.Signal
	wg           sync.WaitGroup
	oldTermState *term.State // Store original terminal state for restoration
}

// makeRawInputOnly sets the terminal to a mode suitable for PTY interaction:
// - Disables local echo (ECHO) - let the PTY slave control echo
// - Disables canonical mode (ICANON) - send chars immediately without line buffering
// - Keeps ISIG enabled - so Ctrl+C generates SIGINT properly
// - Keeps output processing (OPOST) enabled - so \n is converted to \r\n
// This is different from term.MakeRaw() which disables ALL processing including output.
func makeRawInputOnly(fd int) (*term.State, error) {
	oldState, err := term.GetState(fd)
	if err != nil {
		return nil, err
	}

	// Get current termios
	termios, err := unix.IoctlGetTermios(fd, ioctlReadTermios)
	if err != nil {
		return nil, err
	}

	// Disable echo and canonical mode (input processing)
	// Keep ISIG enabled so Ctrl+C works for interrupt signals
	// Keep OPOST (output processing) enabled so \n -> \r\n conversion happens
	termios.Lflag &^= unix.ECHO | unix.ECHONL | unix.ICANON | unix.IEXTEN
	termios.Iflag &^= unix.IGNBRK | unix.BRKINT | unix.PARMRK | unix.ISTRIP | unix.INLCR | unix.IGNCR | unix.ICRNL | unix.IXON
	// Note: We do NOT disable OPOST in Oflag, keeping output processing enabled
	termios.Cflag &^= unix.CSIZE | unix.PARENB
	termios.Cflag |= unix.CS8
	termios.Cc[unix.VMIN] = 1
	termios.Cc[unix.VTIME] = 0

	if err := unix.IoctlSetTermios(fd, ioctlWriteTermios, termios); err != nil {
		return nil, err
	}

	return oldState, nil
}

// StartCommandWithPTY starts a command with a PTY for interactive input (e.g., sudo password).
// Returns a PTYHandle that must be used to wait for the command and clean up resources.
func StartCommandWithPTY(cmd *exec.Cmd, logger *zap.Logger) (*PTYHandle, error) {
	// Start the command with a PTY
	ptmx, err := pty.Start(cmd)
	if err != nil {
		logger.Error("failed to start command with PTY", zap.Error(err))
		return nil, err
	}

	handle := &PTYHandle{
		ptmx:     ptmx,
		cmd:      cmd,
		logger:   logger,
		resizeCh: make(chan os.Signal, 1),
	}

	// Set terminal to a mode suitable for PTY interaction:
	// - Disable local echo (let the PTY slave control echo for password prompts)
	// - Keep output processing enabled (so \n is converted to \r\n)
	if term.IsTerminal(int(os.Stdin.Fd())) {
		oldState, err := makeRawInputOnly(int(os.Stdin.Fd()))
		if err != nil {
			logger.Debug("failed to set terminal mode", zap.Error(err))
		} else {
			handle.oldTermState = oldState
		}
	}

	// Handle terminal resize - propagate size changes from real terminal to PTY
	signal.Notify(handle.resizeCh, syscall.SIGWINCH)
	handle.wg.Add(1)
	go func() {
		defer handle.wg.Done()
		for range handle.resizeCh {
			if resizeErr := pty.InheritSize(os.Stdin, ptmx); resizeErr != nil {
				if !isClosedPTYError(resizeErr) {
					logger.Debug("failed to resize PTY", zap.Error(resizeErr))
				}
			}
		}
	}()
	// Trigger initial resize
	handle.resizeCh <- syscall.SIGWINCH

	// Copy PTY output to real stdout (for agent logs, prompts, etc.)
	go func() {
		_, copyErr := io.Copy(os.Stdout, ptmx)
		if copyErr != nil && !isClosedPTYError(copyErr) {
			logger.Debug("error copying PTY output to stdout", zap.Error(copyErr))
		}
	}()

	// Copy stdin to PTY (for password input, etc.)
	go func() {
		_, copyErr := io.Copy(ptmx, os.Stdin)
		if copyErr != nil && !isClosedPTYError(copyErr) {
			logger.Debug("error copying stdin to PTY", zap.Error(copyErr))
		}
	}()

	return handle, nil
}

// Wait waits for the command to finish and cleans up PTY resources.
func (h *PTYHandle) Wait() error {
	// Wait for the command to finish
	cmdErr := h.cmd.Wait()

	// Restore terminal state before any other cleanup
	if h.oldTermState != nil {
		if restoreErr := term.Restore(int(os.Stdin.Fd()), h.oldTermState); restoreErr != nil {
			h.logger.Debug("failed to restore terminal state", zap.Error(restoreErr))
		}
	}

	// Stop listening for resize signals
	signal.Stop(h.resizeCh)
	// Drain any pending signals
	select {
	case <-h.resizeCh:
	default:
	}
	close(h.resizeCh)

	// Wait for the resize goroutine to finish
	h.wg.Wait()

	// Close PTY
	if closeErr := h.ptmx.Close(); closeErr != nil {
		h.logger.Debug("failed to close PTY", zap.Error(closeErr))
	}

	return cmdErr
}

// GetProcess returns the underlying process for signal handling
func (h *PTYHandle) GetProcess() *os.Process {
	if h.cmd != nil {
		return h.cmd.Process
	}
	return nil
}

// StopPTYCommand gracefully stops a command running with PTY
func StopPTYCommand(handle *PTYHandle, logger *zap.Logger) error {
	if handle == nil || handle.cmd == nil || handle.cmd.Process == nil {
		return nil
	}

	// Restore terminal state first
	if handle.oldTermState != nil {
		if restoreErr := term.Restore(int(os.Stdin.Fd()), handle.oldTermState); restoreErr != nil {
			logger.Debug("failed to restore terminal state", zap.Error(restoreErr))
		}
		handle.oldTermState = nil // Prevent double restore
	}

	pid := handle.cmd.Process.Pid

	// Send SIGTERM to the process
	if err := handle.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		if err.Error() == "os: process already finished" {
			logger.Debug("process already finished during graceful shutdown", zap.Int("pid", pid))
			return nil
		}
		logger.Warn("failed to send SIGTERM to PTY process", zap.Int("pid", pid), zap.Error(err))
		// Force kill
		return handle.cmd.Process.Kill()
	}

	return nil
}

// isClosedPTYError checks if the error is due to the PTY being closed,
// which is expected when the command finishes.
func isClosedPTYError(err error) bool {
	if err == nil {
		return false
	}
	errStr := err.Error()
	return err == io.EOF ||
		strings.Contains(errStr, "input/output error") ||
		strings.Contains(errStr, "file already closed") ||
		strings.Contains(errStr, "bad file descriptor")
}

// ConfigureCommandForPTY configures the command's SysProcAttr for PTY execution.
// On Unix, this sets Setsid to create a new session for the PTY.
func ConfigureCommandForPTY(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid: true, // create new session for PTY
	}
}
