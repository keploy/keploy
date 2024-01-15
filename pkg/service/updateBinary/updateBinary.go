package updateBinary

import (
	"errors"
	"fmt"
	"os"
	"os/exec"

	"github.com/charmbracelet/glamour"
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
		u.logger.Info("No updates available. Current Version " + currentVersion + " " + latestVersion + " is the latest.")
		return
	}

	u.logger.Info("Updating to Version: " + latestVersion)
	// Execute the curl command to download keploy.sh and run it with bash
	curlCommand := `curl -s -O https://raw.githubusercontent.com/keploy/keploy/main/keploy.sh && bash keploy.sh`

	// Execute the combined curl command to download and execute keploy.sh with bash
	cmd := exec.Command("sh", "-c", curlCommand)

	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			u.logger.Error(fmt.Sprintf("Failed to update binary file. Exit status: %v", exitErr.ExitCode()))
			if status, ok := exitErr.Sys().(interface{ ExitStatus() int }); ok {
				if exitStatus := status.ExitStatus(); exitStatus != 0 {
					u.logger.Error(fmt.Sprintf("Command exited with status: %v", exitStatus))
				}
			}
		} else {
			u.logger.Error(fmt.Sprintf("Failed to update binary file: %v", err))
		}

		// If there was an error during the update, return here.
		return
	}
	u.logger.Info("Updated to version " + latestVersion)

	changelog = "\n" + string(changelog)
	var renderer *glamour.TermRenderer

	var termRendererOpts []glamour.TermRendererOption
	termRendererOpts = append(termRendererOpts, glamour.WithAutoStyle(), glamour.WithWordWrap(0))

	renderer, err = glamour.NewTermRenderer(termRendererOpts...)
	if err != nil {
		u.logger.Error("Failed to initialize renderer", zap.Error(err))
		return
	}
	changelog, err = renderer.Render(changelog)
	if err != nil {
		u.logger.Error("Failed to render release notes", zap.Error(err))
		return
	}	
	fmt.Println(changelog)

}
