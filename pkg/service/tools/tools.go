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
	"sort"
	"strconv"
	"strings"

	"github.com/charmbracelet/glamour"
	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/pkg/models"
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
	currentVersion := "v" + utils.Version
	isKeployInDocker := len(os.Getenv("KEPLOY_INDOCKER")) > 0
	if isKeployInDocker {
		return errors.New("As you are using docker version of keploy, please pull the latest Docker image of keploy to update keploy")
	}
	if strings.HasSuffix(currentVersion, "-dev") {
		return errors.New("you are using a development version of Keploy. Skipping update check")
	}

	releaseInfo, err := utils.GetLatestGitHubRelease(ctx, t.logger)
	if err != nil {
		if errors.Is(err, ErrGitHubAPIUnresponsive) {
			utils.LogError(t.logger, err, "GitHub API is unresponsive. Update process cannot continue.")
			return errors.New("gitHub API is unresponsive. Update process cannot continue")
		}
		utils.LogError(t.logger, err, "failed to fetch latest GitHub release version")
		return err
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
	var node yaml.Node
	var data []byte
	var err error

	if configData != "" {
		data = []byte(configData)
	} else {
		configData, err = config.Merge(config.InternalConfig, config.DefaultConfig)
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

// Normalise initiates the normalise process for normalising the test cases.
func (t *Tools) Normalise(_ context.Context, path string, testSet string, testCases string) error {
	t.logger.Info("Test cases and Mock Path", zap.String("path", path))
	testReportPath := filepath.Join(path, "testReports")

	// Get a list of directories in the testReportPath
	dirs, err := getDirectories(testReportPath)
	if err != nil {
		utils.LogError(t.logger, err, "Failed to get TestReports")
		return err
	}

	// Find the last-run folder
	sort.Strings(dirs)
	var lastRunFolder string
	maxFolderNumber := -1
	for i := len(dirs) - 1; i >= 0; i-- {
		if strings.HasPrefix(dirs[i], "test-run-") {
			folderNumberStr := strings.TrimPrefix(dirs[i], "test-run-")
			folderNumber, err := strconv.Atoi(folderNumberStr)
			if err != nil {
				utils.LogError(t.logger, err, "Failed to parse folder number")
				continue
			}
			if folderNumber > maxFolderNumber {
				maxFolderNumber = folderNumber
				lastRunFolder = dirs[i]
			}
		}
	}
	lastRunFolderPath := filepath.Join(testReportPath, lastRunFolder)
	t.logger.Info("Test Run Folder", zap.String("folder", lastRunFolderPath))

	// Get list of YAML files in the last run folder
	files, err := fs.ReadDir(os.DirFS(lastRunFolderPath), ".")
	if err != nil {
		utils.LogError(t.logger, err, "Failed to read directory")
		return err
	}

	// Iterate over each YAML file
	for _, file := range files {
		if strings.HasSuffix(file.Name(), ".yaml") {
			filePath := filepath.Join(lastRunFolderPath, file.Name())

			// Read the YAML file
			yamlData, err := os.ReadFile(filePath)
			if err != nil {
				utils.LogError(t.logger, err, "Failed to read YAML file")
				continue
			}

			// Unmarshal YAML into TestReport
			var testReport models.TestReport
			err = yaml.Unmarshal(yamlData, &testReport)
			if err != nil {
				utils.LogError(t.logger, err, "Failed to unmarshal YAML")
				continue
			}
			testCasesArr := strings.Split(testCases, " ")
			// Iterate over tests in the TestReport
			for _, test := range testReport.Tests {
				testCasePath := filepath.Join(path, testSet)
				if test.Status == models.TestStatusFailed && test.TestCasePath == testCasePath && contains(testCasesArr, test.TestCaseID) {

					// Read the contents of the testcase file
					testCaseFilePath := filepath.Join(test.TestCasePath, "tests", test.TestCaseID+".yaml")
					t.logger.Info("Updating testcase file", zap.String("filePath", testCaseFilePath))
					testCaseContent, err := os.ReadFile(testCaseFilePath)
					if err != nil {
						utils.LogError(t.logger, err, "Failed to read testcase file")
						continue
					}

					// Unmarshal YAML into TestCase
					var testCase TestCaseFile
					err = yaml.Unmarshal(testCaseContent, &testCase)
					if err != nil {
						utils.LogError(t.logger, err, "Failed to unmarshal YAML")
						continue
					}
					t.logger.Info("Updating Response body from :" + testCase.Spec.Resp.Body + " to :" + test.Result.BodyResult[0].Actual)
					testCase.Spec.Resp.Body = test.Result.BodyResult[0].Actual

					// Marshal TestCase back to YAML
					updatedYAML, err := yaml.Marshal(&testCase)
					if err != nil {
						utils.LogError(t.logger, err, "Failed to marshal YAML", zap.Error(err))
						continue
					}

					// Write the updated YAML content back to the file
					err = os.WriteFile(testCaseFilePath, updatedYAML, 0644)
					if err != nil {
						utils.LogError(t.logger, err, "Failed to write updated YAML to file", zap.Error(err))
						continue
					}

					t.logger.Info("Updated testcase file successfully", zap.String("testCaseFilePath", testCaseFilePath))
				}
			}
		}
	}
	return nil
}
