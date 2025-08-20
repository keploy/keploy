//go:build linux || darwin

package utils

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"

	"go.uber.org/zap"
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
	username := os.Getenv("SUDO_USER")

	// Get the current hard limit for the number of open file descriptors
	var rlimit syscall.Rlimit
	err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &rlimit)
	if err != nil {
		logger.Error("Failed to get RLIMIT_NOFILE", zap.Error(err))
	}

	// Get the current hard limit value
	hardLimit := rlimit.Max

	fmt.Println(hardLimit)

	userCmd = fmt.Sprintf("ulimit -S -n %d && %s", hardLimit, userCmd)

	cmd := exec.CommandContext(ctx, "sh", "-c", userCmd)
	if username != "" {
		// print all environment variables
		logger.Debug("env inherited from the cmd", zap.Any("env", os.Environ()))
		// Run the command as the user who invoked sudo to preserve the user environment variables and PATH
		cmd = exec.CommandContext(ctx, "sudo", "-E", "-u", os.Getenv("SUDO_USER"), "env", "PATH="+os.Getenv("PATH"), "sh", "-c", userCmd)
	}

	// Set the cancel function for the command
	cmd.Cancel = cancel(cmd)

	// wait after sending the interrupt signal, before sending the kill signal
	cmd.WaitDelay = waitDelay

	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
	}

	// Set the output of the command
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	logger.Debug("", zap.Any("executing cli", cmd.String()))

	err = cmd.Start()
	if err != nil {
		return CmdError{Type: Init, Err: err}
	}

	err = cmd.Wait()
	if err != nil {
		return CmdError{Type: Runtime, Err: err}
	}

	return CmdError{}
}
