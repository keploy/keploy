package updateBinary

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"

	"go.keploy.io/server/utils"
	"go.uber.org/zap"
)

// GitHubRelease holds information about the GitHub release.
type GitHubRelease struct {
	TagName string `json:"tag_name"`
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
	UpdateBinary(binaryFilePath string)
}

// UpdateBinary initiates the update process for the Keploy binary file.
func (u *updater) UpdateBinary(binaryFilePath string) {
	currentVersion := utils.KeployVersion

	// Fetch the latest version from GitHub releases
	latestVersion, err := getLatestGitHubRelease()
	fmt.Println("latestVersion is: ", latestVersion)
	if err != nil {
		u.logger.Error("Failed to fetch latest GitHub release version", zap.Error(err))
		return
	}

	if currentVersion == latestVersion {
		u.logger.Info("No updates available. Version " + latestVersion + " is the latest.")
		return
	}

	// Execute the curl command to download keploy.sh and run it with bash
	curlCommand := `curl -O https://raw.githubusercontent.com/keploy/keploy/main/keploy.sh && bash keploy.sh`

	// Create the command
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

// getLatestGitHubRelease fetches the latest version from GitHub releases.

func getLatestGitHubRelease() (string, error) {
	// GitHub repository details
	repoOwner := "keploy"
	repoName := "keploy"

	// GitHub API URL for latest release
	apiURL := fmt.Sprintf("https://api.github.com/repos/%s/%s/releases/latest", repoOwner, repoName)

	// Create an HTTP client
	client := http.Client{}

	// Create a GET request to the GitHub API
	req, err := http.NewRequest("GET", apiURL, nil)
	if err != nil {
		return "", err
	}

	// Send the request
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	// Decode the response JSON
	var release GitHubRelease
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return "", err
	}

	return release.TagName, nil
}
