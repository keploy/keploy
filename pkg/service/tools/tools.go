package tools

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"

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
	// if strings.HasSuffix(currentVersion, "-dev") {
	// 	fmt.Println("You are using a development version of Keploy. Skipping update")
	// 	return nil
	// }

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

	err = t.downloadAndUpdate(ctx, t.logger)
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

func (t *Tools) downloadAndUpdate(_ context.Context, _ *zap.Logger) error {
	curlPath, err := exec.LookPath("curl")
	if err != nil {
		return errors.New("curl command not found on the system")
	}

	cmd := exec.Command(curlPath, "--silent", "-L", "-o", "install.sh", "https://keploy.io/install.sh")
	err = cmd.Run()
	if err != nil {
		return errors.New("failed to download the latest version of keploy")
	}

	cmd = exec.Command("bash", "install.sh")
	err = cmd.Run()
	if err != nil {
		return errors.New("failed to install the latest version of keploy")
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
