package Update

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"github.com/charmbracelet/glamour"
	"go.keploy.io/server/utils"
	"go.uber.org/zap"
)

// NewUpdater initializes a new updater instance.
func NewUpdater(logger *zap.Logger) Updater {
	return &updater{
		logger: logger,
	}
}

// updater manages the updating process of Keploy .
type updater struct {
	logger *zap.Logger
}

var ErrGitHubAPIUnresponsive = errors.New("GitHub API is unresponsive")

// Update initiates the update process for the Keploy binary file.
func (u *updater) Update(ctx context.Context) error {
	currentVersion := utils.Version

	isDockerCmd := len(os.Getenv("IS_DOCKER_CMD")) > 0
	if isDockerCmd {
		return errors.New("please Pull the latest Docker image of keploy")
	}
	if strings.HasSuffix(currentVersion, "-dev") {
		return errors.New("you are using a development version of Keploy. Skipping update check")
	}

	releaseInfo, err := utils.GetLatestGitHubRelease()
	if err != nil {
		if errors.Is(err, ErrGitHubAPIUnresponsive) {
			u.logger.Error("GitHub API is unresponsive. Update process cannot continue.")
			return errors.New("gitHub API is unresponsive. Update process cannot continue")
		}
		u.logger.Error("failed to fetch latest GitHub release version")
		return err
	}

	latestVersion := releaseInfo.TagName
	changelog := releaseInfo.Body

	if currentVersion == latestVersion {
		u.logger.Info("You are on the latest version of Keploy: " + latestVersion)
		return
	}

	u.logger.Info("Updating to Version: " + latestVersion)

	downloadUrl := ""
	if runtime.GOARCH == "amd64" {
		downloadUrl = "https://github.com/keploy/keploy/releases/latest/download/keploy_linux_amd64.tar.gz"
	} else {
		downloadUrl = "https://github.com/keploy/keploy/releases/latest/download/keploy_linux_arm64.tar.gz"
	}
	err = u.downloadAndUpdate(downloadUrl)
	if err != nil {
		return err
	}

	u.logger.Info("Update Successful!")

	changelog = "\n" + string(changelog)
	var renderer *glamour.TermRenderer

	var termRendererOpts []glamour.TermRendererOption
	termRendererOpts = append(termRendererOpts, glamour.WithAutoStyle(), glamour.WithWordWrap(0))

	renderer, err = glamour.NewTermRenderer(termRendererOpts...)
	if err != nil {
		u.logger.Error("failed to initialize renderer", zap.Error(err))
		return err
	}
	changelog, err = renderer.Render(changelog)
	if err != nil {
		u.logger.Error("failed to render release notes", zap.Error(err))
		return err
	}
	fmt.Println(changelog)
}

func (u *updater) downloadAndUpdate(downloadUrl string) error {
	curlPath, err := exec.LookPath("curl")
	if err != nil {
		return errors.New("curl command not found on the system")
	}

	// Determine the path based on the alias "keploy"
	aliasPath := "/usr/local/bin/keploybin" // Default path
	aliasCmd := exec.Command("which", "keploy")
	aliasOutput, err := aliasCmd.Output()
	if err == nil && len(aliasOutput) > 0 {
		aliasPath = strings.TrimSpace(string(aliasOutput))
	}
	// Check if the aliasPath is a valid path
	_, err = os.Stat(aliasPath)
	if os.IsNotExist(err) {
		return fmt.Errorf("alias path %s does not exist", aliasPath)
	}

	// Check if the aliasPath is a directory
	if fileInfo, err := os.Stat(aliasPath); err == nil && fileInfo.IsDir() {
		return fmt.Errorf("alias path %s is a directory, not a file", aliasPath)
	}

	downloadCmd := exec.Command(curlPath, "--silent", "--location", downloadUrl)
	untarCmd := exec.Command("tar", "xz", "-C", "/tmp")

	// Pipe the output of the first command to the second command
	untarCmd.Stdin, _ = downloadCmd.StdoutPipe()

	if err := downloadCmd.Start(); err != nil {
		return fmt.Errorf("failed to start download command: %v", err)
	}
	if err := untarCmd.Start(); err != nil {
		return fmt.Errorf("failed to start untar command: %v", err)
	}

	if err := downloadCmd.Wait(); err != nil {
		return fmt.Errorf("failed to wait download command: %v", err)

	}
	if err := untarCmd.Wait(); err != nil {
		return fmt.Errorf("failed to wait untar command: %v", err)

	}

	moveCmd := exec.Command("sudo", "mv", "/tmp/keploy", aliasPath)
	if err := moveCmd.Run(); err != nil {
		return fmt.Errorf("failed to move keploy binary to %s: %v", aliasPath, err)

	}

	return nil
}
