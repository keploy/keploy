//go:build windows

package utils

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"go.uber.org/zap"
	"golang.org/x/sys/windows"
)

// The console calls golang.org/x/sys/windows does not wrap.
var (
	procAttachConsole = windows.NewLazySystemDLL("kernel32.dll").NewProc("AttachConsole")
	procFreeConsole   = windows.NewLazySystemDLL("kernel32.dll").NewProc("FreeConsole")
)

// consoleSends holds each CTRL_BREAK send, so that one send's attach to an
// application's console (ctrlBreakFromItsConsole) is never another send's
// console: a process is on one console at most, and a send made while keploy
// is on another application's console goes to that console, not its own.
var consoleSends sync.Mutex

// whileOnTheAppsConsole, when a row sets it, runs while keploy is on an
// application's console, before the CTRL_BREAK is sent from there.
var whileOnTheAppsConsole func()

func SendSignal(logger *zap.Logger, pid int, sig syscall.Signal) error {
	if sig != syscall.SIGINT {
		// For others you can fall back to TerminateProcess if you want
		return fmt.Errorf("only SIGINT supported on Windows for console ctrl events")
	}

	// Negative PID: treat as process group in your abstraction
	if pid < 0 {
		pid = -pid
	}

	consoleSends.Lock()
	defer consoleSends.Unlock()
	// This sends CTRL_BREAK_EVENT (less dangerous than CTRL_C_EVENT). It
	// reaches only a group on keploy's own console.
	err := windows.GenerateConsoleCtrlEvent(windows.CTRL_BREAK_EVENT, uint32(pid))
	if err == nil {
		return nil
	}
	// A keploy with no console (started detached, as a service, or by a
	// program that has none) is on no console, and the application it
	// started was given a console of its own: the CTRL_BREAK is sent from
	// there.
	if aerr := ctrlBreakFromItsConsole(uint32(pid)); aerr != nil {
		err = fmt.Errorf("%w; and from the application's own console: %v", err, aerr)
		logger.Error("GenerateConsoleCtrlEvent failed", zap.Int("pid", pid), zap.Error(err))
		return err
	}
	logger.Debug("sent the application its CTRL_BREAK from its own console: keploy is on none", zap.Int("pid", pid))
	return nil
}

// ctrlBreakFromItsConsole sends a CTRL_BREAK to the process group pid from
// the console of the group's first process (pid): keploy is on that console
// only for the send. A process keploy starts meanwhile would be on it too, so
// it is held for the send alone. The caller holds consoleSends.
func ctrlBreakFromItsConsole(pid uint32) error {
	if r, _, err := procAttachConsole.Call(uintptr(pid)); r == 0 {
		return fmt.Errorf("AttachConsole: %w", err)
	}
	defer func() { _, _, _ = procFreeConsole.Call() }()
	if whileOnTheAppsConsole != nil {
		whileOnTheAppsConsole()
	}
	return windows.GenerateConsoleCtrlEvent(windows.CTRL_BREAK_EVENT, pid)
}

//	func ExecuteCommand(ctx context.Context, logger *zap.Logger, userCmd string, cancel func(cmd *exec.Cmd) func() error, waitDelay time.Duration) CmdError {
//		return CmdError{Type: Init, Err: errors.New("not implemented")}
//	}

func ExecuteCommand(ctx context.Context, logger *zap.Logger, userCmd string, kind CmdType, cancel func(cmd *exec.Cmd) func() error, waitDelay time.Duration, stdin []byte, extraEnv []string, opts ...ExecOption) CmdError {
	o := execOptionsOf(opts)
	// On Windows, commands are typically executed via 'cmd /C' or 'powershell -Command'
	// to handle complex shell-like logic in 'userCmd'. 'cmd /C' is the most robust default.
	cmd := exec.CommandContext(ctx, "cmd", "/C", userCmd)

	// Set the custom cancel function for the command
	cmd.Cancel = cancel(cmd)

	// extraEnv is added to what the command inherits from keploy, for this
	// command alone — a secret that one child needs is not exported to every
	// other process keploy starts.
	if len(extraEnv) > 0 {
		cmd.Env = append(os.Environ(), extraEnv...)
	}

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

	// When in-memory compose content is provided, pipe it via stdin.
	if len(stdin) > 0 {
		cmd.Stdin = bytes.NewReader(stdin)
	}

	// Set the output of the command, cut of the values the redactor holds
	// when it holds any (ChildConsole).
	console, err := SetChildConsoleTee(cmd, o.outputTee)
	if err != nil {
		return CmdError{Type: Init, Err: err}
	}

	logger.Info("Starting Application (Windows):", zap.String("executing_cli", RedactCmdArgs(cmd.Args)))

	// Start the command. On a native run this goes through the registered
	// starter, which creates the process suspended, loads the interception shim
	// into it and then resumes it — Windows has no DYLD_INSERT_LIBRARIES, so
	// that has to happen at process creation. With no starter registered (a
	// docker command, or interception disabled) this is exactly the cmd.Start()
	// it always was.
	err = startApplication(cmd, kind)
	console.Started()
	if err != nil {
		return CmdError{Type: Init, Err: err}
	}

	// Wait for the command to finish
	err = cmd.Wait()
	console.Exited()
	if err != nil {
		return CmdError{Type: Runtime, Err: err}
	}

	return CmdError{}
}
