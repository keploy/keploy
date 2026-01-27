//go:build linux || darwin

package utils

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	"go.uber.org/zap"
	"golang.org/x/term"
)

func SendSignal(logger *zap.Logger, pid int, sig syscall.Signal) error {
	err := syscall.Kill(pid, sig)
	if err != nil {
		// ignore the ESRCH error as it means the process is already dead
		if errno, ok := err.(syscall.Errno); ok && errno == syscall.ESRCH {
			return nil
		}
		logger.Error("failed to send signal to process", zap.Int("pid", pid), zap.Error(err))
		return err
	}
	logger.Debug("signal sent to process successfully", zap.Int("pid", pid), zap.String("signal", sig.String()))

	return nil
}

func ExecuteCommand(ctx context.Context, logger *zap.Logger, userCmd string, cancel func(cmd *exec.Cmd) func() error, waitDelay time.Duration) CmdError {
	// Run the app as the user who invoked sudo

	cmd := exec.CommandContext(ctx, "sh", "-c", userCmd)

	// Set the cancel function for the command
	cmd.Cancel = cancel(cmd)

	// wait after sending the interrupt signal, before sending the kill signal
	cmd.WaitDelay = waitDelay

	// Check if the command is docker-compose related and output is a TTY
	cmdType := FindDockerCmd(userCmd)
	isTTY := term.IsTerminal(int(os.Stdout.Fd()))
	stdoutWriter := io.Writer(os.Stdout)
	stderrWriter := io.Writer(os.Stderr)
	if IsMCPStdio() {
		stdoutWriter = os.Stderr
	}

	// Use PTY for Docker Compose when running in a TTY to avoid SIGTTOU/SIGTTIN issues
	// Docker Compose needs to read terminal size for progress bars, but Setpgid: true
	// puts it in a background process group which causes the OS to pause it.
	// A PTY gives Docker Compose its own terminal to work with.
	if cmdType == DockerCompose && isTTY {
		// If running in MCP mode, we cannot use PTY effectively because io.Copy to stdout
		// interferes with the JSON-RPC protocol. Instead, we pipe logs to a file.
		if IsMCPStdio() {
			// Create log file for docker-compose output in OS-specific temp directory
			timestamp := time.Now().Unix()
			logFileName := fmt.Sprintf("docker-compose-tmp-keploy-%d.logs", timestamp)
			logFilePath := filepath.Join(os.TempDir(), logFileName)

			logFile, err := os.Create(logFilePath)
			if err != nil {
				logger.Error("failed to create log file for docker-compose output", zap.Error(err))
				return CmdError{Type: Init, Err: err}
			}
			// This will close when ExecuteCommand returns, which is execution finishes
			defer logFile.Close()

			// Set command output to the log file
			cmd.Stdout = logFile
			cmd.Stderr = logFile

			// Use Setpgid for process group management (like non-PTY)
			cmd.SysProcAttr = &syscall.SysProcAttr{
				Setpgid: true,
			}

			logger.Debug("Output is a TTY (Docker Compose -> Logs to file)")
			logger.Info("Docker compose logs are being written to file", zap.String("path", logFilePath))
			logger.Info("You can view live logs using tail -f", zap.String("command", "tail -f "+logFilePath))
		} else {
			// For PTY, we use Setsid to create a new session instead of Setpgid
			// This allows the PTY to become the controlling terminal
			cmd.SysProcAttr = &syscall.SysProcAttr{
				Setsid: true,
			}
			logger.Debug("Output is a TTY (Docker Compose -> PTY)")
			return executeWithPTY(ctx, logger, cmd)
		}
	} else {
		// For non-PTY execution, use Setpgid for process group management
		cmd.SysProcAttr = &syscall.SysProcAttr{
			Setpgid: true,
		}

		if cmdType == DockerCompose {
			logger.Debug("Output is NOT a TTY (Docker Compose -> Stdout/Stderr)")
		}
		// Set the output of the command to stdout/stderr
		cmd.Stdout = stdoutWriter
		cmd.Stderr = stderrWriter
	}

	logger.Info("Starting Application :", zap.String("executing_cmd", cmd.String()))
	err := cmd.Start()
	if err != nil {
		return CmdError{Type: Init, Err: err}
	}

	err = cmd.Wait()
	if err != nil {
		return CmdError{Type: Runtime, Err: err}
	}

	return CmdError{}
}

// executeWithPTY runs the command inside a dedicated PTY (Pseudo-Terminal).
// This is necessary for Docker Compose when Setpgid is true, as Docker Compose
// tries to read terminal size for rendering progress bars. Without a PTY,
// the OS would pause the background process with SIGTTOU/SIGTTIN.
func executeWithPTY(_ context.Context, logger *zap.Logger, cmd *exec.Cmd) CmdError {
	// Start the command with a PTY
	// pty.Start creates a PTY pair, assigns the slave PTY to cmd's stdin/stdout/stderr,
	// and starts the command
	ptmx, err := pty.Start(cmd)
	if err != nil {
		logger.Error("failed to start command with PTY", zap.Error(err))
		return CmdError{Type: Init, Err: err}
	}

	logger.Info("Starting Application:", zap.String("executing_cmd", cmd.String()))

	// Handle terminal resize - propagate size changes from real terminal to PTY
	resizeCh := make(chan os.Signal, 1)
	signal.Notify(resizeCh, syscall.SIGWINCH)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range resizeCh {
			if resizeErr := pty.InheritSize(os.Stdin, ptmx); resizeErr != nil {
				// We might get an error if the PTY is closed while we try to resize it.
				// This is expected during shutdown.
				if !isClosedPTYError(resizeErr) {
					logger.Debug("failed to resize PTY", zap.Error(resizeErr))
				}
			}
		}
	}()
	// Trigger initial resize
	resizeCh <- syscall.SIGWINCH

	// Copy PTY output to real stdout
	// This goroutine will exit when ptmx is closed after cmd.Wait() returns
	outputDone := make(chan struct{})
	var copyErr error
	go func() {
		_, copyErr = io.Copy(os.Stdout, ptmx)
		close(outputDone)
	}()

	// Wait for the command to finish
	cmdErr := cmd.Wait()

	// Stop listening for resize signals first, then drain any pending signals
	// before closing the channel to avoid potential write to closed channel
	signal.Stop(resizeCh)
	// Drain any pending signals
	select {
	case <-resizeCh:
	default:
	}
	close(resizeCh)

	// Wait for the resize goroutine to finish to ensure it's done using the PTY
	wg.Wait()

	// Close PTY - this will unblock the io.Copy goroutine reading from ptmx
	if closeErr := ptmx.Close(); closeErr != nil {
		logger.Debug("failed to close PTY", zap.Error(closeErr))
	}

	// Wait for output copy to finish to ensure all output is flushed
	<-outputDone

	// Log copy error if it's not due to PTY being closed (which is expected)
	if copyErr != nil && !isClosedPTYError(copyErr) {
		logger.Debug("error copying PTY output to stdout", zap.Error(copyErr))
	}

	if cmdErr != nil {
		return CmdError{Type: Runtime, Err: cmdErr}
	}

	return CmdError{}
}

// isClosedPTYError checks if the error is due to the PTY being closed,
// which is expected when the command finishes.
func isClosedPTYError(err error) bool {
	if err == nil {
		return false
	}
	errStr := err.Error()
	// When PTY is closed, io.Copy returns with read errors like:
	// - "read /dev/ptmx: input/output error" (Linux)
	// - "read /dev/ptmx: file already closed"
	// - EOF is also expected
	return err == io.EOF ||
		strings.Contains(errStr, "input/output error") ||
		strings.Contains(errStr, "file already closed") ||
		strings.Contains(errStr, "bad file descriptor")
}
