//go:build linux || darwin

package utils

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"go.uber.org/zap"
)

// CheckKeployFolderPermissions checks if the keploy folder and its contents are readable
// and writable by the current user. Returns a list of paths with permission issues.
func CheckKeployFolderPermissions(logger *zap.Logger, keployPath string) ([]PermissionError, error) {
	var permissionErrors []PermissionError
	currentUID := uint32(os.Getuid())

	// Check if keploy folder exists
	info, err := os.Stat(keployPath)
	if os.IsNotExist(err) {
		// Folder doesn't exist yet - no permission issues
		return nil, nil
	} else if err != nil {
		// Can't even stat the folder - this is a permission issue
		return []PermissionError{{Path: keployPath, OwnerUID: 0, IsRead: true}}, nil
	}

	// Folder exists, check if it's a directory
	if !info.IsDir() {
		return nil, fmt.Errorf("keploy path %s exists but is not a directory", keployPath)
	}

	// Walk the directory tree and check permissions
	err = filepath.WalkDir(keployPath, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			// Access error - this indicates a permission issue
			logger.Debug("cannot access path", zap.String("path", path), zap.Error(err))
			ownerUID := uint32(0)
			if fileInfo, statErr := os.Lstat(path); statErr == nil {
				if stat, ok := fileInfo.Sys().(*syscall.Stat_t); ok {
					ownerUID = stat.Uid
				}
			}
			permissionErrors = append(permissionErrors, PermissionError{Path: path, OwnerUID: ownerUID, IsRead: true})
			return filepath.SkipDir
		}

		// Get file info to check ownership
		fileInfo, infoErr := d.Info()
		if infoErr != nil {
			return nil
		}

		// Check if file is owned by a different user (likely root)
		if stat, ok := fileInfo.Sys().(*syscall.Stat_t); ok {
			if stat.Uid != currentUID {
				// File is owned by someone else - potential permission issue
				// Verify by actually trying to open for read/write
				hasIssue := false

				if d.IsDir() {
					// For directories, check if we can read and write
					_, readErr := os.ReadDir(path)
					if readErr != nil {
						hasIssue = true
					}
					// Also check write permission by checking if we can create a temp file
					// We use access() syscall equivalent - try to open with write flag
					testFile := filepath.Join(path, ".keploy_perm_test")
					f, writeErr := os.OpenFile(testFile, os.O_CREATE|os.O_WRONLY, 0644)
					if writeErr != nil {
						if os.IsPermission(writeErr) {
							hasIssue = true
						}
					} else {
						f.Close()
						os.Remove(testFile)
					}
				} else {
					// For files, try to open for reading and writing
					f, readErr := os.OpenFile(path, os.O_RDONLY, 0)
					if readErr != nil {
						hasIssue = true
					} else {
						f.Close()
					}

					// Check write permission
					f, writeErr := os.OpenFile(path, os.O_WRONLY, 0)
					if writeErr != nil {
						if os.IsPermission(writeErr) {
							hasIssue = true
						}
					} else {
						f.Close()
					}
				}

				if hasIssue {
					logger.Debug("permission issue detected",
						zap.String("path", path),
						zap.Uint32("ownerUID", stat.Uid),
						zap.Uint32("currentUID", currentUID))
					permissionErrors = append(permissionErrors, PermissionError{
						Path:     path,
						OwnerUID: stat.Uid,
						IsRead:   false, // Could be read or write
					})
				}
			}
		}

		return nil
	})

	if err != nil {
		return nil, fmt.Errorf("error walking keploy directory: %w", err)
	}

	return permissionErrors, nil
}

// GetPermissionFixCommand returns the sudo command to fix keploy folder permissions.
// This is used to prepend the fix command to docker commands so they run in the same PTY session.
// Returns empty string if no fix is needed.
func GetPermissionFixCommand(keployPath string) string {
	currentUser, err := user.Current()
	if err != nil {
		return ""
	}

	return fmt.Sprintf("sudo chown -R %s %s", currentUser.Username, keployPath)
}

// FixKeployFolderPermissions attempts to fix permission issues on the keploy folder
// by changing ownership to the current user using sudo chown.
// It shows which files have issues and provides a timeout for the sudo password prompt.
// If timeout is reached, it continues without fixing (with a warning).
func FixKeployFolderPermissions(ctx context.Context, logger *zap.Logger, keployPath string, permErrors []PermissionError) error {
	// Get current user
	currentUser, err := user.Current()
	if err != nil {
		return fmt.Errorf("failed to get current user: %w", err)
	}

	// Log the files with permission issues
	fmt.Println()
	logger.Warn("The keploy folder contains files not accessible by current user (likely from running an older version with sudo).")

	// Show the problematic files (up to 5, then "and X more...")
	if len(permErrors) > 0 {
		logger.Info("Files with permission issues:")
		maxShow := 5
		for i, pe := range permErrors {
			if i >= maxShow {
				logger.Info(fmt.Sprintf("  ... and %d more", len(permErrors)-maxShow))
				break
			}
			logger.Info(fmt.Sprintf("  - %s (owner UID: %d)", pe.Path, pe.OwnerUID))
		}
	}

	fmt.Println()
	logger.Info("To fix this, we need to change ownership of the keploy folder to your user.")
	fmt.Println()

	// First, cache sudo credentials using sudo -v (this prompts for password)
	cmd := exec.CommandContext(ctx, "sudo", "-v")
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		// Check if context was cancelled (user pressed Ctrl+C)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// Check if the process was killed by a signal (e.g., SIGINT from Ctrl+C)
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			// Exit code -1 or signal termination indicates interrupt
			if exitErr.ExitCode() == -1 || strings.Contains(err.Error(), "signal") {
				return context.Canceled
			}
		}
		logger.Warn(fmt.Sprintf("Failed to authenticate sudo: %v", err))
		logger.Warn(fmt.Sprintf("To fix manually, run: sudo chown -R %s %s", currentUser.Username, keployPath))
		return fmt.Errorf("sudo authentication failed")
	}

	// Now run chown with cached credentials (should not prompt again)
	logger.Info(fmt.Sprintf("Running: sudo chown -R %s %s", currentUser.Username, keployPath))
	if err := runSudoChownNonInteractive(ctx, currentUser.Username, true, keployPath); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		logger.Warn(fmt.Sprintf("Failed to fix permissions: %v", err))
		logger.Warn(fmt.Sprintf("To fix manually, run: sudo chown -R %s %s", currentUser.Username, keployPath))
		return fmt.Errorf("failed to fix permissions")
	}

	logger.Info("Successfully fixed keploy folder permissions")
	return nil
}

// AreSudoCredentialsCached checks if sudo credentials are currently cached.
// Returns true if already root or if sudo -n -v succeeds (credentials cached).
func AreSudoCredentialsCached() bool {
	// Already root - no sudo needed
	if os.Geteuid() == 0 {
		return true
	}

	// Check if credentials are cached using sudo -n -v
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	checkCmd := exec.CommandContext(ctx, "sudo", "-n", "-v")
	return checkCmd.Run() == nil
}

// CacheSudoCredentials prompts for sudo password and caches the credentials.
// This uses sudo -v which validates and caches credentials without running a command.
// The cached credentials will be used by subsequent sudo calls (including the agent).
// If already root or credentials are already cached, this is a no-op.
func CacheSudoCredentials(ctx context.Context, logger *zap.Logger) error {
	// No need to cache if already root
	if os.Geteuid() == 0 {
		return nil
	}

	// Check if credentials are already cached using sudo -n -v
	checkCmd := exec.CommandContext(ctx, "sudo", "-n", "-v")
	if err := checkCmd.Run(); err == nil {
		// Credentials already cached
		logger.Debug("Sudo credentials already cached")
		return nil
	}

	logger.Info("Enter sudo password (will be cached for subsequent operations).")

	// Run sudo -v to validate and cache credentials
	cmd := exec.CommandContext(ctx, "sudo", "-v")
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	return cmd.Run()
}

// runSudoChownNonInteractive runs sudo chown using cached credentials (non-interactive).
func runSudoChownNonInteractive(ctx context.Context, username string, recursive bool, path string) error {
	args := []string{"-n", "chown"} // -n = non-interactive, fail if password needed
	if recursive {
		args = append(args, "-R")
	}
	args = append(args, username, path)

	cmd := exec.CommandContext(ctx, "sudo", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	return cmd.Run()
}

// runSudoChown runs sudo chown with proper terminal handling.
// It resets the terminal to sane mode before running sudo to ensure password input works correctly.
func runSudoChown(ctx context.Context, username string, recursive bool, path string) error {
	// Open /dev/tty for direct terminal access
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("failed to open /dev/tty: %w", err)
	}
	defer tty.Close()

	// Reset terminal to sane mode before running sudo
	// This ensures the terminal is in canonical mode with echo properly controlled
	sttyCmd := exec.CommandContext(ctx, "stty", "sane", "-F", "/dev/tty")
	_ = sttyCmd.Run() // Ignore errors, best effort

	// Build the chown command
	args := []string{"chown"}
	if recursive {
		args = append(args, "-R")
	}
	args = append(args, username, path)

	cmd := exec.CommandContext(ctx, "sudo", args...)
	cmd.Stdin = tty
	cmd.Stdout = tty
	cmd.Stderr = tty

	return cmd.Run()
}

// FixFilePermission attempts to fix permission issues on a specific file by changing ownership.
// This is called when a file operation (read or write) fails due to permission issues,
// typically because the file is owned by root from an older sudo-based keploy version.
func FixFilePermission(ctx context.Context, logger *zap.Logger, filePath string) error {
	// Get current user
	currentUser, err := user.Current()
	if err != nil {
		return fmt.Errorf("failed to get current user: %w", err)
	}

	logger.Warn("Cannot access file (likely owned by root from older keploy version)",
		zap.String("path", filePath))
	logger.Info(fmt.Sprintf("Running: sudo chown %s %s", currentUser.Username, filePath))

	if err := runSudoChown(ctx, currentUser.Username, false, filePath); err != nil {
		return fmt.Errorf("failed to change ownership of %s: %w. Please run manually: sudo chown %s %s", filePath, err, currentUser.Username, filePath)
	}

	logger.Debug("Successfully fixed file permission", zap.String("path", filePath))
	return nil
}

// EnsureKeployFolderPermissions checks and fixes permission issues (both read and write) on the keploy folder.
// This should be called once at startup before any file operations to ensure all files
// are accessible by the current user. This prevents permission errors during test execution.
// If deferForDocker is true, it stores the fix info for later use by docker PTY session instead of fixing now.
func EnsureKeployFolderPermissions(ctx context.Context, logger *zap.Logger, keployPath string, deferForDocker bool) error {
	// Check if keploy folder exists
	if _, err := os.Stat(keployPath); os.IsNotExist(err) {
		// Folder doesn't exist yet - no permission issues to fix
		return nil
	}

	permErrors, err := CheckKeployFolderPermissions(logger, keployPath)
	if err != nil {
		return fmt.Errorf("failed to check keploy folder permissions: %w", err)
	}

	if len(permErrors) == 0 {
		// No permission issues
		PendingPermissionFix.HasIssues = false
		return nil
	}

	// Log the problematic paths at debug level
	logger.Debug("Found files/directories with permission issues in keploy folder", zap.Int("count", len(permErrors)))

	if deferForDocker {
		// Store for later - docker PTY will handle this
		PendingPermissionFix.HasIssues = true
		PendingPermissionFix.KeployPath = keployPath
		PendingPermissionFix.PermErrors = permErrors

		// Log the issue info so user knows what's happening
		fmt.Println()
		logger.Warn("The keploy folder contains files not accessible by current user (likely from running an older version with sudo).")
		if len(permErrors) > 0 {
			logger.Info("Files with permission issues:")
			maxShow := 5
			for i, pe := range permErrors {
				if i >= maxShow {
					logger.Info(fmt.Sprintf("  ... and %d more", len(permErrors)-maxShow))
					break
				}
				logger.Info(fmt.Sprintf("  - %s (owner UID: %d)", pe.Path, pe.OwnerUID))
			}
		}
		fmt.Println()
		logger.Info("Permission fix will be included with the docker command (single password prompt).")
		return nil
	}

	// Fix permissions on the entire keploy folder
	// sudo -v will cache credentials, which will be used by subsequent sudo -n commands
	return FixKeployFolderPermissions(ctx, logger, keployPath, permErrors)
}
