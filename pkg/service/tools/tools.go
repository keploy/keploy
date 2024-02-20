package tools

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/charmbracelet/glamour"
	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
	"gopkg.in/yaml.v3"
)

func NewTools(logger *zap.Logger) Service {
	return &Tools{
		logger: logger,
	}
}

type Tools struct {
	logger *zap.Logger
}

var ErrGitHubAPIUnresponsive = errors.New("GitHub API is unresponsive")

// Update initiates the tools process for the Keploy binary file.
func (t *Tools) Update(ctx context.Context) error {
	currentVersion := utils.Version

	isDockerCmd := len(os.Getenv("IS_DOCKER_CMD")) > 0
	if isDockerCmd {
		return errors.New("please Pull the latest Docker image of keploy")
	}
	if strings.HasSuffix(currentVersion, "-dev") {
		return errors.New("you are using a development version of Keploy. Skipping tools check")
	}

	releaseInfo, err := utils.GetLatestGitHubRelease()
	if err != nil {
		if errors.Is(err, ErrGitHubAPIUnresponsive) {
			t.logger.Error("GitHub API is unresponsive. Update process cannot continue.")
			return errors.New("gitHub API is unresponsive. Update process cannot continue")
		}
		t.logger.Error("failed to fetch latest GitHub release version")
		return err
	}

	latestVersion := releaseInfo.TagName
	changelog := releaseInfo.Body

	if currentVersion == latestVersion {
		t.logger.Info("You are on the latest version of Keploy: " + latestVersion)
		return nil
	}

	t.logger.Info("Updating to Version: " + latestVersion)

	downloadUrl := ""
	if runtime.GOARCH == "amd64" {
		downloadUrl = "https://github.com/keploy/keploy/releases/latest/download/keploy_linux_amd64.tar.gz"
	} else {
		downloadUrl = "https://github.com/keploy/keploy/releases/latest/download/keploy_linux_arm64.tar.gz"
	}
	err = t.downloadAndUpdate(downloadUrl)
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
		t.logger.Error("failed to initialize renderer", zap.Error(err))
		return err
	}
	changelog, err = renderer.Render(changelog)
	if err != nil {
		t.logger.Error("failed to render release notes", zap.Error(err))
		return err
	}
	fmt.Println(changelog)
	return nil
}

func (t *Tools) downloadAndUpdate(downloadUrl string) error {
	// Download the file
	resp, err := http.Get(downloadUrl)
	if err != nil {
		return fmt.Errorf("failed to download file: %v", err)
	}
	defer resp.Body.Close()

	// Create a temporary file to store the downloaded tar.gz
	tmpFile, err := os.CreateTemp("", "keploy-download-*.tar.gz")
	if err != nil {
		return fmt.Errorf("failed to create temporary file: %v", err)
	}
	defer tmpFile.Close()
	defer os.Remove(tmpFile.Name())

	// Write the downloaded content to the temporary file
	_, err = io.Copy(tmpFile, resp.Body)
	if err != nil {
		return fmt.Errorf("failed to write to temporary file: %v", err)
	}

	// Extract the tar.gz file
	if err := extractTarGz(tmpFile.Name(), "/tmp"); err != nil {
		return fmt.Errorf("failed to extract tar.gz file: %v", err)
	}

	// Determine the path based on the alias "keploy"
	aliasPath := "/usr/local/bin/keploy" // Default path

	// Check if the aliasPath is a valid path
	_, err = os.Stat(aliasPath)
	if os.IsNotExist(err) {
		return fmt.Errorf("alias path %s does not exist", aliasPath)
	}

	// Check if the aliasPath is a directory
	if fileInfo, err := os.Stat(aliasPath); err == nil && fileInfo.IsDir() {
		return fmt.Errorf("alias path %s is a directory, not a file", aliasPath)
	}

	// Move the extracted binary to the alias path
	if err := os.Rename("/tmp/keploy", aliasPath); err != nil {
		return fmt.Errorf("failed to move keploy binary to %s: %v", aliasPath, err)
	}

	return nil
}

func extractTarGz(gzipPath, destDir string) error {
	file, err := os.Open(gzipPath)
	if err != nil {
		return err
	}
	defer file.Close()

	gzipReader, err := gzip.NewReader(file)
	if err != nil {
		return err
	}
	defer gzipReader.Close()

	tarReader := tar.NewReader(gzipReader)

	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}

		target := filepath.Join(destDir, header.Name)

		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0755); err != nil {
				return err
			}
		case tar.TypeReg:
			outFile, err := os.Create(target)
			if err != nil {
				return err
			}
			if _, err := io.Copy(outFile, tarReader); err != nil {
				outFile.Close()
				return err
			}
			outFile.Close()
		}
	}

	return nil
}

func (t *Tools) CreateConfig(ctx context.Context, filePath string, configData string) error {
	var node yaml.Node
	var data []byte
	var err error

	if configData != "" {
		data = []byte(configData)
	} else {
		configData, err = config.Merge(config.InternalConfig, config.DefaultConfig)
		if err != nil {
			t.logger.Fatal("Error while creating default config string", zap.Error(err))
		}
		data = []byte(configData)
	}

	if err := yaml.Unmarshal(data, &node); err != nil {
		t.logger.Fatal("Unmarshalling failed", zap.Error(err))
	}
	results, err := yaml.Marshal(node.Content[0])
	if err != nil {
		t.logger.Fatal("Failed to marshal the config", zap.Error(err))
	}

	finalOutput := append(results, []byte(utils.ConfigGuide)...)

	err = os.WriteFile(filePath, finalOutput, os.ModePerm)
	if err != nil {
		t.logger.Fatal("Failed to write config file", zap.Error(err))
	}

	err = os.Chmod(filePath, 0777) // Set permissions to 777
	if err != nil {
		t.logger.Error("failed to set the permission of config file", zap.Error(err))
		return nil
	}

	t.logger.Info("Config file generated successfully")
	return nil
}
