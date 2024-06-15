//go:build windows

package utils

import (
	"syscall"

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
