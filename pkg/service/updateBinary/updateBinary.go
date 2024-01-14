package updateBinary

import (
  "errors"
	"fmt"
	"os"
	"os/exec"
  "encoding/json"
	"net/http"
	"go.keploy.io/server/utils"
	"go.uber.org/zap"
)

// GitHubRelease holds information about the GitHub release.
type GitHubRelease struct {
	TagName string `json:"tag_name"`
	Body    string `json:"body"`
}

// updater manages the updating process of the Keploy binary.
type updater struct {
	logger *zap.Logger
}

// NewUpdater initializes a new updater instance.
func NewUpdater(logger *zap.Logger) Updater {
	return &updater{
		logger: logger,
	}
}

// Updater defines the contract for updating the Keploy binary.
type Updater interface {
	UpdateBinary()
}

var ErrGitHubAPIUnresponsive = errors.New("GitHub API is unresponsive")

// UpdateBinary initiates the update process for the Keploy binary file.
func (u *updater) UpdateBinary() {
	currentVersion := utils.KeployVersion

	// Fetch the latest version and release body from GitHub releases with a timeout
	releaseInfo, err := utils.GetLatestGitHubRelease()
	latestVersion := releaseInfo.TagName
	changelog := releaseInfo.Body

	if err != nil {
		if err == ErrGitHubAPIUnresponsive {
			u.logger.Error("GitHub API is unresponsive. Update process cannot continue.")
		} else {
			u.logger.Error("Failed to fetch latest GitHub release version", zap.Error(err))
		}
		return
	}

	if currentVersion == latestVersion {
		u.logger.Info("No updates available. Version " + latestVersion + " is the latest.")
		return
	}

	u.logger.Info("Updating to Version: " + latestVersion)
	// Execute the curl command to download keploy.sh and run it with bash
	curlCommand := `curl -O https://raw.githubusercontent.com/keploy/keploy/main/keploy.sh && bash keploy.sh`

	// Execute the combined curl command to download and execute keploy.sh with bash
	cmd := exec.Command("sh", "-c", curlCommand)

	// Set up input for the command
	cmd.Stdin = os.Stdin

	// Set output and error
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	// Start the command
	if err := cmd.Start(); err != nil {
		u.logger.Error("Failed to start command", zap.Error(err))
		return
	}

	// Wait for command to finish
	if err := cmd.Wait(); err != nil {
		// Handle non-zero exit status here if required
		if exitErr, ok := err.(*exec.ExitError); ok {
			u.logger.Error(fmt.Sprintf("Failed to update binary file: %v", exitErr))
		} else {
			u.logger.Error(fmt.Sprintf("Failed to wait for command: %v", err))
		}
		return
	}

	u.logger.Info("Updated Keploy binary to version " + latestVersion)
}
