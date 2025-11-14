//go:build windows

package utils

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"

	"go.uber.org/zap"
	"golang.org/x/sys/windows"
)

func SendSignal(logger *zap.Logger, pid int, sig syscall.Signal) error {
	if sig != syscall.SIGINT {
		// For others you can fall back to TerminateProcess if you want
		return fmt.Errorf("only SIGINT supported on Windows for console ctrl events")
	}

	// Negative PID: treat as process group in your abstraction
	if pid < 0 {
		pid = -pid
	}

	// This sends CTRL_BREAK_EVENT (less dangerous than CTRL_C_EVENT)
	if err := windows.GenerateConsoleCtrlEvent(windows.CTRL_BREAK_EVENT, uint32(pid)); err != nil {
		logger.Error("GenerateConsoleCtrlEvent failed", zap.Int("pid", pid), zap.Error(err))
		return err
	}

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
