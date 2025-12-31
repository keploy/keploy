//go:build windows && amd64

package windows

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"golang.org/x/sys/windows"
)

const pidFileName = "keploy_redirector.pid"

// watchdogState tracks the state of the watchdog and cleanup mechanisms
type watchdogState struct {
	stopped      atomic.Bool
	cleanupOnce  sync.Once
	cleanupMutex sync.Mutex
	pidFilePath  string
}

var watchdog = &watchdogState{}

// getPIDFilePath returns the path to the PID file in the temp directory
func getPIDFilePath() string {
	return filepath.Join(os.TempDir(), pidFileName)
}

// initWatchdog initializes the watchdog system:
// 1. Creates new PID file for current session
// 2. Registers console control handler for graceful shutdown scenarios
// NOTE: Stale PID cleanup is done separately in load() BEFORE StartRedirector
func (h *Hooks) initWatchdog() error {
	watchdog.pidFilePath = getPIDFilePath()
	watchdog.stopped.Store(false)

	// Write current PID to file
	currentPID := os.Getpid()
	if err := os.WriteFile(watchdog.pidFilePath, []byte(strconv.Itoa(currentPID)), 0644); err != nil {
		h.logger.Error("failed to write PID file")
		return fmt.Errorf("failed to write PID file: %w", err)
	}

	h.logger.Info("watchdog: PID file created")

	// Register console control handler for console close events
	// This catches Ctrl+C, Ctrl+Break, and window close - but NOT Task Manager kills
	if err := h.registerConsoleHandler(); err != nil {
		h.logger.Warn("failed to register console handler")
	}

	return nil
}

// cleanupStalePIDFile checks if a PID file exists from a previous session.
// If the process in the PID file is no longer running, it means Keploy
// was killed abruptly and we should cleanup WinDivert.
func (h *Hooks) cleanupStalePIDFile() error {
	pidFilePath := getPIDFilePath()

	data, err := os.ReadFile(pidFilePath)
	if os.IsNotExist(err) {
		// No stale PID file - clean start
		return nil
	}
	if err != nil {
		return fmt.Errorf("failed to read PID file: %w", err)
	}

	pidStr := strings.TrimSpace(string(data))
	pid, err := strconv.Atoi(pidStr)
	if err != nil {
		// Invalid PID file, just remove it
		os.Remove(pidFilePath)
		return nil
	}

	// Check if the process with this PID is still running
	if isProcessRunning(uint32(pid)) {
		// Process is still running - either another instance or PID was reused
		// We should not cleanup in this case
		h.logger.Warn("another Keploy instance may be running")
		return nil
	}

	// Process is not running - this was a crash! Cleanup WinDivert
	h.logger.Info("watchdog: detected stale PID file from crashed session, cleaning up WinDivert")

	if err := StopRedirector(); err != nil {
		// This is expected if redirector wasn't actually running
		h.logger.Debug("watchdog: cleanup attempt returned error (may be normal)")
	} else {
		h.logger.Info("watchdog: successfully cleaned up WinDivert from previous crash")
	}

	// Remove the stale PID file
	os.Remove(pidFilePath)

	return nil
}

// isProcessRunning checks if a process with the given PID is still running
func isProcessRunning(pid uint32) bool {
	// Try to open the process with minimal access rights
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		// Process doesn't exist or we can't access it
		return false
	}
	defer windows.CloseHandle(handle)

	// Check if process has exited
	var exitCode uint32
	if err := windows.GetExitCodeProcess(handle, &exitCode); err != nil {
		return false
	}

	// STILL_ACTIVE means process is running
	return exitCode == 259 // STILL_ACTIVE constant
}

// stopWatchdog signals the watchdog to stop and removes the PID file
// Called during normal graceful cleanup
func (h *Hooks) stopWatchdog() {
	watchdog.stopped.Store(true)

	// Remove PID file since we're shutting down cleanly
	if watchdog.pidFilePath != "" {
		if err := os.Remove(watchdog.pidFilePath); err != nil && !os.IsNotExist(err) {
			h.logger.Warn("failed to remove PID file during cleanup")
		} else {
			h.logger.Debug("watchdog: PID file removed successfully")
		}
	}
}

// registerConsoleHandler registers a Windows console control handler
// that captures console events like Ctrl+C, Ctrl+Break, and console close.
// Note: Task Manager kills bypass this handler - that's why we have the PID file approach.
func (h *Hooks) registerConsoleHandler() error {
	// Load kernel32.dll and get SetConsoleCtrlHandler
	kernel32 := windows.NewLazySystemDLL("kernel32.dll")
	setConsoleCtrlHandler := kernel32.NewProc("SetConsoleCtrlHandler")

	// Create the handler callback
	// Windows calls this from a different thread, so we use the global watchdog state
	handlerCallback := windows.NewCallback(func(ctrlType uint32) uintptr {
		switch ctrlType {
		case windows.CTRL_C_EVENT, windows.CTRL_BREAK_EVENT, windows.CTRL_CLOSE_EVENT,
			windows.CTRL_LOGOFF_EVENT, windows.CTRL_SHUTDOWN_EVENT:
			// Perform emergency cleanup
			h.emergencyCleanup()
			return 1 // Handled
		}
		return 0 // Not handled
	})

	// Register the handler
	ret, _, err := setConsoleCtrlHandler.Call(handlerCallback, 1)
	if ret == 0 {
		return fmt.Errorf("SetConsoleCtrlHandler failed: %w", err)
	}

	return nil
}

// emergencyCleanup performs cleanup when abnormal termination is detected
func (h *Hooks) emergencyCleanup() {
	watchdog.cleanupOnce.Do(func() {
		watchdog.cleanupMutex.Lock()
		defer watchdog.cleanupMutex.Unlock()

		h.logger.Info("watchdog: performing emergency WinDivert cleanup")
		if err := StopRedirector(); err != nil {
			h.logger.Error("watchdog: failed to stop redirector during emergency cleanup")
		} else {
			h.logger.Info("watchdog: WinDivert cleanup successful")
		}

		// Remove PID file
		if watchdog.pidFilePath != "" {
			os.Remove(watchdog.pidFilePath)
		}
	})
}
