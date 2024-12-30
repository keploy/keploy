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

// Define Windows API function GenerateConsoleCtrlEvent
var (
	kernel32                     = syscall.NewLazyDLL("kernel32.dll")
	procGenerateConsoleCtrlEvent = kernel32.NewProc("GenerateConsoleCtrlEvent")
)

func generateConsoleCtrlEvent(ctrlEvent uint32, processGroupID uint32) error {
	ret, _, err := procGenerateConsoleCtrlEvent.Call(uintptr(ctrlEvent), uintptr(processGroupID))
	if ret == 0 {
		return fmt.Errorf("GenerateConsoleCtrlEvent failed: %v", err)
	}
	return nil
}

func SendSignal(logger *zap.Logger, pid int, sig syscall.Signal) error {
	handle, err := syscall.OpenProcess(syscall.PROCESS_TERMINATE, false, uint32(-pid))
	if err != nil {
		if errno, ok := err.(syscall.Errno); ok && errno == windows.ERROR_INVALID_PARAMETER {
			return nil
		}
		logger.Error("failed to open process", zap.Int("pid", pid), zap.Error(err))
		return err
	}
	defer syscall.CloseHandle(handle)

	err = generateConsoleCtrlEvent(syscall.CTRL_BREAK_EVENT, uint32(-pid))
	if err != nil {
		logger.Error("Error sending CTRL_BREAK_EVENT:", zap.Error(err))
		return err
	}

	logger.Debug("signal sent to process successfully", zap.Int("pid", pid), zap.String("signal", sig.String()))
	return nil
}

func ExecuteCommand(ctx context.Context, logger *zap.Logger, userCmd string, cancel func(cmd *exec.Cmd) func() error, waitDelay time.Duration) CmdError {
	// Create the command without sudo (not needed on Windows)
	cmd := exec.CommandContext(ctx, "cmd", "/C", userCmd)

	// Log environment variables for debugging
	logger.Debug("Environment variables", zap.Any("env", os.Environ()))

	// Set cancel function and delay before force-killing
	if cancel != nil {
		cmd.Cancel = cancel(cmd)
	}
	cmd.WaitDelay = waitDelay

	// Set environment variables and output streams
	cmd.Env = os.Environ()
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	logger.Debug("Executing CLI command", zap.String("command", cmd.String()))

	// Start the command
	err := cmd.Start()
	if err != nil {
		return CmdError{Type: Init, Err: err}
	}

	// Wait for the command to complete
	err = cmd.Wait()
	if err != nil {
		return CmdError{Type: Runtime, Err: err}
	}

	return CmdError{}
}
