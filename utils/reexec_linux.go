//go:build linux

package utils

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"

	"go.uber.org/zap"
)

// ReexecWithSudo re-executes the current keploy command with sudo.
// This is used when a Docker/Docker Compose command is detected and keploy is not running as root.
// Uses syscall.Exec to replace the current process entirely - no parent process remains.
// This function never returns on success.
func ReexecWithSudo(logger *zap.Logger) {
	// Get the current PATH to preserve it
	currentPath := os.Getenv("PATH")

	// Find sudo binary
	sudoPath, err := exec.LookPath("sudo")
	if err != nil {
		logger.Error("sudo not found in PATH, cannot re-execute with elevated privileges", zap.Error(err))
		fmt.Println("Error: sudo is required to run Docker commands. Please install sudo or run as root.")
		os.Exit(1)
	}

	// Find keploy binary - use the current executable
	keployPath, err := os.Executable()
	if err != nil {
		logger.Error("failed to get current executable path", zap.Error(err))
		os.Exit(1)
	}

	// Build the command: sudo -E env PATH="$PATH" keploy <original-args>
	// -E preserves the environment
	// env PATH="$PATH" ensures PATH is explicitly set (some sudo configs reset PATH)
	args := []string{
		"sudo",
		"-E",
		"env",
		fmt.Sprintf("PATH=%s", currentPath),
		keployPath,
	}
	// Append original arguments (skip the first one which is the program name)
	args = append(args, os.Args[1:]...)

	logger.Info("Re-executing with sudo for elevated privileges...")
	logger.Debug("Re-exec command", zap.Strings("args", args))

	// Use syscall.Exec to replace the current process
	// This means no parent process remains - clean handoff
	err = syscall.Exec(sudoPath, args, os.Environ())
	if err != nil {
		// syscall.Exec only returns on error
		logger.Error("failed to re-execute with sudo", zap.Error(err))
		fmt.Printf("Error: failed to re-execute with sudo: %v\n", err)
		os.Exit(1)
	}
}

// ShouldReexecWithSudo checks if keploy should re-execute itself with sudo.
// Returns true if:
// 1. A Docker/Docker Compose command is detected in the -c/--command flag, OR
// 2. The "cloud replay" subcommand is being used (it generates a docker-compose internally)
// AND Keploy is NOT currently running as root.
func ShouldReexecWithSudo() bool {
	// Already running as root - no need to re-exec
	if os.Geteuid() == 0 {
		return false
	}

	// Check if this is a "cloud replay" command, which always needs root
	// because it generates and runs a docker-compose file internally.
	if isCloudReplayCmd(os.Args) {
		return true
	}

	// Extract the command from arguments
	cmd := ExtractCommandFromArgs(os.Args)
	if cmd == "" {
		return false
	}

	// Check if it's a Docker command
	cmdType := FindDockerCmd(cmd)
	return IsDockerCmd(cmdType)
}

// isCloudReplayCmd checks if the args represent the "keploy cloud replay" subcommand.
func isCloudReplayCmd(args []string) bool {
	// It is tolerant of flags that may appear before or between subcommands,
	// e.g. `keploy --debug cloud replay` or `keploy cloud --debug replay`.
	if len(args) < 3 {
		return false
	}

	// Skip args[0] (program name) and search for "cloud" followed by "replay".
	for i := 1; i < len(args); i++ {
		if args[i] != "cloud" {
			continue
		}

		// Look ahead for "replay", allowing only flag-like args (starting with "-")
		// between "cloud" and "replay".
		for j := i + 1; j < len(args); j++ {
			if args[j] == "replay" {
				return true
			}

			// If we encounter a non-flag argument that isn't "replay", this is not
			// the "cloud replay" sequence; continue searching for another "cloud".
			if len(args[j]) == 0 || args[j][0] != '-' {
				break
			}
		}
	}

	return false
}
