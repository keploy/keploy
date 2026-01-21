//go:build linux || darwin

package utils

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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

	cmd := exec.CommandContext(ctx, "sh", "-c", userCmd)

	// Set the cancel function for the command
	cmd.Cancel = cancel(cmd)

	// wait after sending the interrupt signal, before sending the kill signal
	cmd.WaitDelay = waitDelay

	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
	}

	// // Check if the command is docker-compose related and output is a TTY
	// cmdType := FindDockerCmd(userCmd)
	// isTTY := term.IsTerminal(int(os.Stdout.Fd()))

	// if cmdType == DockerCompose && isTTY {
	// Create log file for docker-compose output in OS-specific temp directory
	timestamp := time.Now().Unix()
	logFileName := fmt.Sprintf("docker-compose-tmp-keploy-%d.logs", timestamp)
	logFilePath := filepath.Join(os.TempDir(), logFileName)

	logFile, err := os.Create(logFilePath)
	if err != nil {
		logger.Error("failed to create log file for docker-compose output", zap.Error(err))
		return CmdError{Type: Init, Err: err}
	}
	defer logFile.Close()

	// Set command output to the log file
	cmd.Stdout = logFile
	cmd.Stderr = logFile

	logger.Debug("Output is a TTY (Docker Compose -> Logs to file)")
	logger.Info("Docker compose logs are being written to file", zap.String("path", logFilePath))
	logger.Info("You can view live logs using tail -f", zap.String("command", "tail -f "+logFilePath))
	// // } else {
	// 	if cmdType == DockerCompose {
	// 		logger.Debug("Output is NOT a TTY (Docker Compose -> Stdout/Stderr)")
	// 	}
	// 	// Set the output of the command to stdout/stderr
	// 	cmd.Stdout = os.Stdout
	// 	cmd.Stderr = os.Stderr
	// // }

	logger.Info("Starting Application :", zap.String("executing_cli", cmd.String()))
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
