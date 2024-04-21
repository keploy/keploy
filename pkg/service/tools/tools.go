package tools

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"github.com/charmbracelet/glamour"
	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
	"gopkg.in/yaml.v3"
)

func NewTools(logger *zap.Logger, telemetry teleDB) Service {
	return &Tools{
		logger:    logger,
		telemetry: telemetry,
	}
}

type Tools struct {
	logger    *zap.Logger
	telemetry teleDB
}

var ErrGitHubAPIUnresponsive = errors.New("GitHub API is unresponsive")

// Update initiates the tools process for the Keploy binary file.
func (t *Tools) Update(ctx context.Context) error {
	currentVersion := utils.Version
	isKeployInDocker := len(os.Getenv("KEPLOY_INDOCKER")) > 0
	if isKeployInDocker {
		fmt.Println("As you are using docker version of keploy, please pull the latest Docker image of keploy to update keploy")
		return nil
	}
	if strings.HasSuffix(currentVersion, "-dev") {
		fmt.Println("You are using a development version of Keploy. Skipping update")
		return nil
	}

	releaseInfo, err := utils.GetLatestGitHubRelease(ctx, t.logger)
	if err != nil {
		if errors.Is(err, ErrGitHubAPIUnresponsive) {
			return errors.New("GitHub API is unresponsive. Update process cannot continue")
		}
		return fmt.Errorf("Failed to fetch latest GitHub release version: %v", err)
	}

	latestVersion := releaseInfo.TagName
	fmt.Println("Current Version: " + currentVersion)
	fmt.Println("Latest Version: " + latestVersion)
	changelog := releaseInfo.Body

	if currentVersion == latestVersion {
		fmt.Println("✅You are already on the latest version of Keploy: " + latestVersion)
		return nil
	}

	t.logger.Info("Updating to Version: " + latestVersion)

	downloadURL := ""
	if runtime.GOARCH == "amd64" {
		downloadURL = "https://github.com/keploy/keploy/releases/latest/download/keploy_linux_amd64.tar.gz"
	} else {
		downloadURL = "https://github.com/keploy/keploy/releases/latest/download/keploy_linux_arm64.tar.gz"
	}
	err = t.downloadAndUpdate(ctx, t.logger, downloadURL)
	if err != nil {
		return err
	}

	t.logger.Info("Update Successful!")

	changelog = "\n" + string(changelog)
	var renderer *glamour.TermRenderer

	var termRendererOpts []glamour.TermRendererOption
	termRendererOpts = append(termRendererOpts, glamour.WithAutoStyle(), glamour.WithWordWrap(0))

	renderer, err = glamour.NewTermRenderer(termRendererOpts...)
	if err != nil {
		utils.LogError(t.logger, err, "failed to initialize renderer")
		return err
	}
	changelog, err = renderer.Render(changelog)
	if err != nil {
		utils.LogError(t.logger, err, "failed to render release notes")
		return err
	}
	fmt.Println(changelog)
	return nil
}

func (t *Tools) downloadAndUpdate(_ context.Context, _ *zap.Logger, downloadURL string) error {
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

	downloadCmd := exec.Command(curlPath, "--silent", "--location", downloadURL)
	untarCmd := exec.Command("tar", "xz", "-C", "/tmp")

	// Pipe the output of the first command to the second command
	untarCmd.Stdin, _ = downloadCmd.StdoutPipe()

	if err := downloadCmd.Start(); err != nil {
		return fmt.Errorf("Failed to start download command: %v", err)
	}
	if err := untarCmd.Start(); err != nil {
		return fmt.Errorf("Failed to start untar command: %v", err)
	}

	if err := downloadCmd.Wait(); err != nil {
		return fmt.Errorf("Failed to wait download command: %v", err)

	}
	if err := untarCmd.Wait(); err != nil {
		return fmt.Errorf("Failed to wait untar command: %v", err)

	}

	moveCmd := exec.Command("sudo", "mv", "/tmp/keploy", aliasPath)
	if err := moveCmd.Run(); err != nil {
		return fmt.Errorf("Failed to move keploy binary to %s: %v", aliasPath, err)

	}

	return nil
}

func (t *Tools) CreateConfig(_ context.Context, filePath string, configData string) error {
	var node yaml.Node
	var data []byte
	var err error

	if configData != "" {
		data = []byte(configData)
	} else {
		configData, err = config.Merge(config.InternalConfig, config.GetDefaultConfig())
		if err != nil {
			utils.LogError(t.logger, err, "failed to create default config string")
			return nil
		}
		data = []byte(configData)
	}

	if err := yaml.Unmarshal(data, &node); err != nil {
		utils.LogError(t.logger, err, "failed to unmarshal the config")
		return nil
	}
	results, err := yaml.Marshal(node.Content[0])
	if err != nil {
		utils.LogError(t.logger, err, "failed to marshal the config")
		return nil
	}

	finalOutput := append(results, []byte(utils.ConfigGuide)...)

	err = os.WriteFile(filePath, finalOutput, fs.ModePerm)
	if err != nil {
		utils.LogError(t.logger, err, "failed to write config file")
		return nil
	}

	err = os.Chmod(filePath, 0777) // Set permissions to 777
	if err != nil {
		utils.LogError(t.logger, err, "failed to set the permission of config file")
		return nil
	}

	t.logger.Info("Config file generated successfully")
	return nil
}
