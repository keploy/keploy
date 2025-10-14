//go:build windows

package utils

import (
	"context"
	"os"
	"os/exec"
	"syscall"
	"time"

	"go.uber.org/zap"
	"golang.org/x/sys/windows"
)

func SendSignal(logger *zap.Logger, pid int, sig syscall.Signal) error {
	handle, err := syscall.OpenProcess(syscall.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		if errno, ok := err.(syscall.Errno); ok && errno == windows.ERROR_INVALID_PARAMETER {
			// ERROR_INVALID_PARAMETER means the process does not exist
			return nil
		}
		logger.Error("failed to open process", zap.Int("pid", pid), zap.Error(err))
		return err
	}
	defer syscall.CloseHandle(handle)

	var retVal int32
	if sig == syscall.SIGKILL || sig == syscall.SIGTERM {
		retVal = 1 // Default exit code for termination
	}

	if err := syscall.TerminateProcess(handle, uint32(retVal)); err != nil {
		logger.Error("failed to terminate process", zap.Int("pid", pid), zap.Error(err))
		return err
	}

	logger.Debug("signal sent to process successfully", zap.Int("pid", pid), zap.String("signal", sig.String()))
	return nil
}

//	func ExecuteCommand(ctx context.Context, logger *zap.Logger, userCmd string, cancel func(cmd *exec.Cmd) func() error, waitDelay time.Duration) CmdError {
//		return CmdError{Type: Init, Err: errors.New("not implemented")}
//	}
func ExecuteCommand(ctx context.Context, logger *zap.Logger, userCmd string, cancel func(cmd *exec.Cmd) func() error, waitDelay time.Duration) CmdError {
	// On Windows, commands are typically executed via 'cmd /C' or 'powershell -Command'
	// to handle complex shell-like logic in 'userCmd'. 'cmd /C' is the most robust default.
	cmd := exec.CommandContext(ctx, "cmd", "/C", userCmd)

	// Set the custom cancel function for the command
	cmd.Cancel = cancel(cmd)

	// cmd.WaitDelay is ignored on Windows, but we set it for consistency with the signature.
	cmd.WaitDelay = waitDelay

	// Use Windows-specific system call attributes.
	// CREATE_NEW_PROCESS_GROUP helps ensure that when the process is terminated
	// (usually via a job object managed by the Go runtime on context cancellation),
	// the entire process tree started by 'cmd /C' is terminated, mimicking the
	// effect of Setpgid on Linux.
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP,
	}

	// Set the output of the command
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	logger.Info("Starting Application (Windows):", zap.String("executing_cli", cmd.String()))

	// Start the command
	err := cmd.Start()
	if err != nil {
		return CmdError{Type: Init, Err: err}
	}

	// Wait for the command to finish
	err = cmd.Wait()
	if err != nil {
		return CmdError{Type: Runtime, Err: err}
	}

	return CmdError{}
}
