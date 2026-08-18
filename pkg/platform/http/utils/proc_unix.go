//go:build linux || darwin
// +build linux darwin

package utils

import (
	"errors"
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
	} else {
		// sudo <bin> <args...>
		all := append([]string{bin}, args...)
		cmd = exec.Command("sudo", all...)
	}

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

// gracefulStopTimeout bounds how long StopCommand waits for a SIGTERM'd process
// group to exit before escalating to SIGKILL. StopCommand returns as soon as the
// group is actually gone, so this is only the ceiling for a process that ignores
// SIGTERM — the common case (a process that exits on SIGTERM) returns in well
// under a second.
const gracefulStopTimeout = 8 * time.Second

// forceKillTimeout bounds the post-SIGKILL wait for the group to disappear.
const forceKillTimeout = 3 * time.Second

// groupGone reports whether no process remains in process group pgid. kill(-pgid, 0)
// returns ESRCH once every member has exited and been reaped (keploy's app-watch
// goroutine reaps the leader, so a graceful exit clears the group promptly).
func groupGone(pgid int) bool {
	return errors.Is(syscall.Kill(-pgid, 0), syscall.ESRCH)
}

// waitGroupGone polls until process group pgid is gone or timeout elapses.
func waitGroupGone(pgid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if groupGone(pgid) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// waitPIDGone polls until process pid is gone or timeout elapses (fallback path
// where the process is not in its own group).
func waitPIDGone(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// StopCommand gracefully stops the app's process group: SIGTERM, then WAIT for the
// group to actually exit so its resources (listening ports, sockets) are released,
// then SIGKILL if it ignored SIGTERM. The wait is load-bearing: returning before
// the group has exited races the next app start against a still-bound port and
// surfaces as "listen tcp :PORT: bind: address already in use" (the
// go-docker-timefreeze flake). The previous implementation SIGTERM'd the group and
// returned immediately, doing neither the wait nor the documented SIGKILL fallback.
func StopCommand(cmd *exec.Cmd, logger *zap.Logger) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	pid := cmd.Process.Pid

	// Determine pgid (with Setpgid, leader's pgid == pid)
	pgid, err := syscall.Getpgid(pid)
	if err != nil {
		logger.Debug("failed to get pgid; falling back to direct kill", zap.Int("pid", pid), zap.Error(err))
		// Graceful SIGTERM to the leader only.
		if sigErr := cmd.Process.Signal(syscall.SIGTERM); sigErr != nil {
			// Process already finished is expected during graceful shutdown, not an error
			if sigErr.Error() == "os: process already finished" {
				logger.Debug("process already finished during graceful shutdown", zap.Int("pid", pid))
				return nil
			}
			logger.Debug("failed to send SIGTERM to process; falling back to kill", zap.Int("pid", pid), zap.Error(sigErr))
		}
		// Wait (bounded) for exit so the port is released, then force-kill.
		if waitPIDGone(pid, gracefulStopTimeout) {
			return nil
		}
		return cmd.Process.Kill()
	}

	// Graceful: SIGTERM the whole group.
	if err := syscall.Kill(-pgid, syscall.SIGTERM); err != nil {
		logger.Debug("failed to send SIGTERM to process group", zap.Int("pgid", pgid), zap.Error(err))
	}

	// Wait (bounded) for the group to fully exit so its port/sockets are released
	// before we return — otherwise the next app start races a still-bound port.
	if waitGroupGone(pgid, gracefulStopTimeout) {
		return nil
	}

	// Ignored SIGTERM: force-kill the whole group and confirm it is gone.
	logger.Debug("process group did not exit after SIGTERM; escalating to SIGKILL", zap.Int("pgid", pgid))
	if err := syscall.Kill(-pgid, syscall.SIGKILL); err != nil {
		logger.Debug("failed to send SIGKILL to process group", zap.Int("pgid", pgid), zap.Error(err))
	}
	waitGroupGone(pgid, forceKillTimeout)
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
	closeOnce    sync.Once   // Ensure PTY is closed only once
	stopOnce     sync.Once   // Ensure resize channel cleanup happens only once
	ptmxMu       sync.Mutex  // Protects access to ptmx during resize and close
	closing      bool        // Flag to indicate we're shutting down
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

	// Set terminal to a mode suitable for PTY interaction
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
			// Use mutex to prevent race with ptmx.Close()
			handle.ptmxMu.Lock()
			if handle.closing {
				handle.ptmxMu.Unlock()
				continue
			}
			if resizeErr := pty.InheritSize(os.Stdin, ptmx); resizeErr != nil {
				if !isClosedPTYError(resizeErr) {
					logger.Debug("failed to resize PTY", zap.Error(resizeErr))
				}
			}
			handle.ptmxMu.Unlock()
		}
	}()
	// Trigger initial resize
	handle.resizeCh <- syscall.SIGWINCH

	// Copy PTY output to real stdout (for agent logs, prompts, etc.)
	// We add to WG so we can wait for this to finish (or error out)
	handle.wg.Add(1)
	go func() {
		defer handle.wg.Done()
		_, copyErr := io.Copy(os.Stdout, ptmx)
		if copyErr != nil && !isClosedPTYError(copyErr) {
			logger.Debug("error copying PTY output to stdout", zap.Error(copyErr))
		}
	}()

	// Copy stdin to PTY (for password input, etc.)
	// NOTE: We do NOT add this to WaitGroup because io.Copy blocks on os.Stdin.Read()
	// which cannot be interrupted by closing ptmx. This goroutine will exit when
	// the process terminates or when a write to closed ptmx fails.
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

	// Use stopOnce to ensure cleanup only happens once (Wait and StopPTYCommand may both be called)
	h.stopOnce.Do(func() {
		// Stop listening for resize signals
		signal.Stop(h.resizeCh)
		// Drain any pending signals
		select {
		case <-h.resizeCh:
		default:
		}
		close(h.resizeCh)

		// Set closing flag and close PTY under mutex to prevent race with resize handler
		h.ptmxMu.Lock()
		h.closing = true
		h.closeOnce.Do(func() {
			if closeErr := h.ptmx.Close(); closeErr != nil {
				h.logger.Debug("failed to close PTY", zap.Error(closeErr))
			}
		})
		h.ptmxMu.Unlock()
	})

	// Wait for background goroutines (resize, stdout copy, stdin copy) to finish
	h.wg.Wait()

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

	// Use stopOnce to ensure cleanup only happens once (Wait and StopPTYCommand may both be called)
	handle.stopOnce.Do(func() {
		// Stop resize signal handling BEFORE closing the PTY
		signal.Stop(handle.resizeCh)
		// Drain any pending signals
		select {
		case <-handle.resizeCh:
		default:
		}
		close(handle.resizeCh)

		// Close PTY FIRST - this causes sudo to see EOF and exit immediately
		// This is more effective than SIGTERM when sudo is waiting for password input
		handle.ptmxMu.Lock()
		handle.closing = true
		handle.closeOnce.Do(func() {
			if handle.ptmx != nil {
				if err := handle.ptmx.Close(); err != nil && !isClosedPTYError(err) {
					logger.Debug("failed to close PTY in StopPTYCommand", zap.Error(err))
				}
			}
		})
		handle.ptmxMu.Unlock()
	})

	// Send SIGTERM as a fallback in case closing PTY didn't terminate the process
	if err := handle.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		if err.Error() != "os: process already finished" {
			logger.Debug("failed to send SIGTERM to PTY process", zap.Int("pid", pid), zap.Error(err))
		}
	}

	// Wait for all goroutines (resize, stdout copy) to finish
	handle.wg.Wait()

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
