//go:build linux || darwin
// +build linux darwin

package utils

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"

	"github.com/creack/pty"
	keployutils "go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

// NewAgentCommand returns a command that runs the native agent, elevated on the
// platforms that need it.
// - If the platform does not need root at all (macOS), we run the binary directly.
// - If already root, we run the binary directly.
// - Otherwise we prefix with "sudo".
// - If useCachedCreds is true, uses "sudo -n" (non-interactive) which relies on cached credentials.
// - We put the process in its own group so we can signal the whole group.
func NewAgentCommand(bin string, args []string, useCachedCreds bool) *exec.Cmd {
	var cmd *exec.Cmd
	if !AgentNeedsElevation(runtime.GOOS) || os.Geteuid() == 0 {
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

// NewAgentCommandForPTY returns a command configured for PTY execution,
// elevated on the platforms that need it (see NewAgentCommand).
// Uses Setsid instead of Setpgid to allow PTY to become the controlling terminal.
func NewAgentCommandForPTY(bin string, args []string) *exec.Cmd {
	var cmd *exec.Cmd
	if !AgentNeedsElevation(runtime.GOOS) || os.Geteuid() == 0 {
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

// TerminateCommand asks cmd to stop with SIGTERM -- cmd alone: under sudo cmd
// is sudo, which the invoking user may signal (it keeps the user's real uid)
// and which hands the signal to the agent; without sudo cmd is the agent --
// and returns without waiting and without any later SIGKILL. It
// is how the CLI stops a running keploy agent it could not ask over the
// agent's API: the caller then waits for the agent's own exit
// (AgentClient.terminateAgent), and must not call it for a cmd it has already
// reaped, whose pid may belong to another process by then. A process that has
// already exited is not an error.
func TerminateCommand(cmd *exec.Cmd, logger *zap.Logger) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	pid := cmd.Process.Pid
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
		logger.Debug("failed to send SIGTERM to process", zap.Int("pid", pid), zap.Error(err))
		return err
	}
	return nil
}

// KillAgentCommand ends the agent cmd started, for a stop that has to be
// forced (the CLI's stopWatch in pkg/platform/http). The agent is sent
// keployutils.EscalationSignal, on which it kills itself once no teardown work
// is in progress (utils.DieOnEscalation): the CLI judged from a report a
// moment old. now -- the user pressed Ctrl+C again -- sends SIGKILL instead,
// where it can: to an agent started directly. One started through sudo runs
// as root, which the user who ran keploy may not signal; sudo keeps that
// user's uid and hands the escalation signal on to the agent. Through
// cmd.Process, so an agent already reaped is never signalled. A process that
// has already exited is not an error.
func KillAgentCommand(cmd *exec.Cmd, logger *zap.Logger, now bool) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	sig := os.Signal(keployutils.EscalationSignal)
	if now && !isSudo(cmd) {
		sig = syscall.SIGKILL
	}
	if err := cmd.Process.Signal(sig); err != nil && !errors.Is(err, os.ErrProcessDone) {
		logger.Debug("failed to kill the agent", zap.Int("pid", cmd.Process.Pid), zap.Stringer("signal", sig), zap.Error(err))
		return err
	}
	return nil
}

// KillPTYCommand is KillAgentCommand for a command started on a PTY.
func KillPTYCommand(handle *PTYHandle, logger *zap.Logger, now bool) error {
	if handle == nil {
		return nil
	}
	return KillAgentCommand(handle.cmd, logger, now)
}

// isSudo reports whether cmd runs sudo, which runs the agent as another user.
func isSudo(cmd *exec.Cmd) bool {
	return filepath.Base(cmd.Path) == "sudo"
}

// TerminatePTYCommand is TerminateCommand for a command started on a PTY: it
// sends SIGTERM to the command (sudo, which hands it to the agent) and leaves
// the PTY open. Closing the PTY hangs the agent up, and a hung-up agent dies
// where it stands instead of finishing its shutdown; the PTY is closed once
// the command has exited (PTYHandle.Wait).
func TerminatePTYCommand(handle *PTYHandle, logger *zap.Logger) error {
	if handle == nil || handle.cmd == nil || handle.cmd.Process == nil {
		return nil
	}
	if err := handle.cmd.Process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
		logger.Debug("failed to send SIGTERM to PTY process", zap.Int("pid", handle.cmd.Process.Pid), zap.Error(err))
		return err
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
	termMu       sync.Mutex  // Guards oldTermState (RestoreTerminal)
	stdinOff     atomic.Bool // set by RestoreTerminal: keystrokes stay the user's
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
		_, copyErr := io.Copy(stdinGate{handle}, os.Stdin)
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
	h.RestoreTerminal()

	// stopOnce: the cleanup happens once
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

// stdinGate forwards the user's keystrokes to the PTY until RestoreTerminal
// has given the terminal back. From then on it refuses the next one, which
// ends the copy loop, as a closed PTY did: that one keystroke is dropped, not
// fed to an agent that is only shutting down, and the loop does not live on
// to take every keystroke after it.
type stdinGate struct{ h *PTYHandle }

func (g stdinGate) Write(p []byte) (int, error) {
	if g.h.stdinOff.Load() {
		return 0, io.ErrClosedPipe
	}
	return g.h.ptmx.Write(p)
}

// StdinOff reports whether RestoreTerminal has given h's terminal back and
// stopped forwarding keystrokes; for tests of the stop paths.
func StdinOff(h *PTYHandle) bool { return h.stdinOff.Load() }

// RestoreTerminal gives the user's terminal back the state StartCommandWithPTY
// found it in, and stops forwarding keystrokes to the PTY. Wait does it once the command has exited; a stop that waits for
// a running agent to finish its shutdown does it at once, so the terminal is
// the user's again even if keploy exits before the agent does. Idempotent.
func (h *PTYHandle) RestoreTerminal() {
	if h == nil {
		return
	}
	h.stdinOff.Store(true)
	h.termMu.Lock()
	defer h.termMu.Unlock()
	if h.oldTermState == nil {
		return
	}
	if err := term.Restore(int(os.Stdin.Fd()), h.oldTermState); err != nil && h.logger != nil {
		h.logger.Debug("failed to restore terminal state", zap.Error(err))
	}
	h.oldTermState = nil
}

// GetProcess returns the underlying process for signal handling
func (h *PTYHandle) GetProcess() *os.Process {
	if h.cmd != nil {
		return h.cmd.Process
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
	return err == io.EOF || errors.Is(err, io.ErrClosedPipe) ||
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

// SetOwnProcessGroup starts cmd in a process group of its own, so a signal to
// the caller's group -- a terminal's Ctrl+C -- does not reach it.
func SetOwnProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}
