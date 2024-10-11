package tools

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"go.keploy.io/server/v2/pkg/service/export"

	"github.com/charmbracelet/glamour"
	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/pkg/service"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
	yamlLib "gopkg.in/yaml.v3"
)

func NewTools(logger *zap.Logger, telemetry teleDB, auth service.Auth) Service {
	return &Tools{
		logger:    logger,
		telemetry: telemetry,
		auth:      auth,
	}
}

type Tools struct {
	logger    *zap.Logger
	telemetry teleDB
	auth      service.Auth
}

var ErrGitHubAPIUnresponsive = errors.New("GitHub API is unresponsive")

func (t *Tools) SendTelemetry(event string, output ...map[string]interface{}) {
	t.telemetry.SendTelemetry(event, output...)
}

func (t *Tools) Export(ctx context.Context) error {
	return export.Export(ctx, t.logger)
}

// Update initiates the tools process for the Keploy binary file.
func (t *Tools) Update(ctx context.Context) error {
	currentVersion := "v" + utils.Version
	isKeployInDocker := len(os.Getenv("KEPLOY_INDOCKER")) > 0
	if isKeployInDocker {
		fmt.Println("As you are using docker version of keploy, please pull the latest Docker image of keploy to update keploy")
		return nil
	}
	if strings.HasSuffix(currentVersion, "-dev") {
		fmt.Println("you are using a development version of Keploy. Skipping update")
		return nil
	}

	releaseInfo, err := utils.GetLatestGitHubRelease(ctx, t.logger)
	if err != nil {
		if errors.Is(err, ErrGitHubAPIUnresponsive) {
			return errors.New("gitHub API is unresponsive. Update process cannot continue")
		}
		return fmt.Errorf("failed to fetch latest GitHub release version: %v", err)
	}

	latestVersion := releaseInfo.TagName
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

func (t *Tools) downloadAndUpdate(ctx context.Context, logger *zap.Logger, downloadURL string) error {
	// Create a new request with context
	req, err := http.NewRequestWithContext(ctx, "GET", downloadURL, nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %v", err)
	}

	// Create a HTTP client and execute the request
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to download file: %v", err)
	}
	defer func() {
		if cerr := resp.Body.Close(); cerr != nil {
			utils.LogError(logger, cerr, "failed to close response body")
		}
	}()

	// Create a temporary file to store the downloaded tar.gz
	tmpFile, err := os.CreateTemp("", "keploy-download-*.tar.gz")
	if err != nil {
		return fmt.Errorf("failed to create temporary file: %v", err)
	}
	defer func() {
		if err := tmpFile.Close(); err != nil {
			utils.LogError(logger, err, "failed to close temporary file")
		}
		if err := os.Remove(tmpFile.Name()); err != nil {
			utils.LogError(logger, err, "failed to remove temporary file")
		}
	}()

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

	keployPath, err := exec.LookPath("keploy")
	if err == nil && keployPath != "" {
		aliasPath = keployPath
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

	// Move the extracted binary to the alias path
	if err := os.Rename("/tmp/keploy", aliasPath); err != nil {
		return fmt.Errorf("failed to move keploy binary to %s: %v", aliasPath, err)
	}

	if err := os.Chmod(aliasPath, 0777); err != nil {
		return fmt.Errorf("failed to set execute permission on %s: %v", aliasPath, err)
	}

	return nil
}

func extractTarGz(gzipPath, destDir string) error {
	file, err := os.Open(gzipPath)
	if err != nil {
		return err
	}

	defer func() {
		if err := file.Close(); err != nil {
			utils.LogError(nil, err, "failed to close file")
		}
	}()

	gzipReader, err := gzip.NewReader(file)
	if err != nil {
		return err
	}

	defer func() {
		if err := gzipReader.Close(); err != nil {
			utils.LogError(nil, err, "failed to close gzip reader")
		}
	}()

	tarReader := tar.NewReader(gzipReader)

	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}

		fileName := filepath.Clean(header.Name)
		if strings.Contains(fileName, "..") {
			return fmt.Errorf("invalid file path: %s", fileName)
		}

		target := filepath.Join(destDir, header.Name)

		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0777); err != nil {
				return err
			}
		case tar.TypeReg:
			outFile, err := os.Create(target)
			if err != nil {
				return err
			}
			if _, err := io.Copy(outFile, tarReader); err != nil {
				if err := outFile.Close(); err != nil {
					return err
				}
				return err
			}
			if err := outFile.Close(); err != nil {
				return err
			}
		}
	}
	return nil
}

func (t *Tools) CreateConfig(_ context.Context, filePath string, configData string) error {
	var node yamlLib.Node
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

	if err := yamlLib.Unmarshal(data, &node); err != nil {
		utils.LogError(t.logger, err, "failed to unmarshal the config")
		return nil
	}
	results, err := yamlLib.Marshal(node.Content[0])
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

	return nil
}

func (t *Tools) IgnoreTests(_ context.Context, _ string, _ []string) error {
	return nil
}

func (t *Tools) IgnoreTestSet(_ context.Context, _ string) error {
	return nil
}

func (t *Tools) Login(ctx context.Context) bool {
	return t.auth.Login(ctx)
}
