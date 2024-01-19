package updateBinary

import (
	"errors"
	"fmt"
	"os/exec"
	"runtime"

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

	arch := runtime.GOARCH
	downloadUrl := ""
	if arch == "amd64" {
		downloadUrl = "https://github.com/keploy/keploy/releases/latest/download/keploy_linux_amd64.tar.gz"
	} else {
		downloadUrl = "https://github.com/keploy/keploy/releases/latest/download/keploy_linux_arm64.tar.gz"
	}
	err = u.downloadAndUpdate(downloadUrl)
	if err != nil {
		u.logger.Error("update failed", zap.Error(err))
		return
	}

	u.logger.Info("Update successful ")

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

func (u *updater) downloadAndUpdate(downloadUrl string) error {
	downloadCmd := exec.Command("curl", "--silent", "--location", downloadUrl)
	untarCmd := exec.Command("tar", "xz", "-C", "/tmp")

	// Pipe the output of the first command to the second command
	untarCmd.Stdin, _ = downloadCmd.StdoutPipe()

	if err := downloadCmd.Start(); err != nil {
		return err
	}
	if err := untarCmd.Start(); err != nil {
		return err
	}

	if err := downloadCmd.Wait(); err != nil {
		return err
	}
	if err := untarCmd.Wait(); err != nil {
		return err
	}

	moveCmd := exec.Command("sudo", "mv", "/tmp/keploy", "/usr/local/bin/keploybin")
	if err := moveCmd.Run(); err != nil {
		return err
	}

	return nil
}	
