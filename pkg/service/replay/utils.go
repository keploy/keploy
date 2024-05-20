package replay

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"errors"

	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/pkg"
	"go.keploy.io/server/v2/pkg/models"
	"go.uber.org/zap"
)

type TestReportVerdict struct {
	total  int
	passed int
	failed int
	status bool
}

func LeftJoinNoise(globalNoise config.GlobalNoise, tsNoise config.GlobalNoise) config.GlobalNoise {
	noise := globalNoise
	for field, regexArr := range tsNoise["body"] {
		noise["body"][field] = regexArr
	}
	for field, regexArr := range tsNoise["header"] {
		noise["header"][field] = regexArr
	}
	return noise
}

func replaceHostToIP(currentURL string, ipAddress string) (string, error) {
	// Parse the current URL
	parsedURL, err := url.Parse(currentURL)

	if err != nil {
		// Return the original URL if parsing fails
		return currentURL, err
	}

	if ipAddress == "" {
		return currentURL, fmt.Errorf("failed to replace url in case of docker env")
	}

	// Replace hostname with the IP address
	parsedURL.Host = strings.Replace(parsedURL.Host, parsedURL.Hostname(), ipAddress, 1)
	// Return the modified URL
	return parsedURL.String(), nil
}

type testUtils struct {
	logger     *zap.Logger
	apiTimeout uint64
}

func NewTestUtils(apiTimeout uint64, logger *zap.Logger) RequestEmulator {
	return &testUtils{
		logger:     logger,
		apiTimeout: apiTimeout,
	}
}

func (t *testUtils) SimulateRequest(ctx context.Context, _ uint64, tc *models.TestCase, testSetID string) (*models.HTTPResp, error) {
	switch tc.Kind {
	case models.HTTP:
		t.logger.Debug("Before simulating the request", zap.Any("Test case", tc))
		t.logger.Debug(fmt.Sprintf("the url of the testcase: %v", tc.HTTPReq.URL))
		resp, err := pkg.SimulateHTTP(ctx, *tc, testSetID, t.logger, t.apiTimeout)
		t.logger.Debug("After simulating the request", zap.Any("test case id", tc.Name))
		t.logger.Debug("After GetResp of the request", zap.Any("test case id", tc.Name))
		return resp, err
	}
	return nil, nil
}

func downloadAndExtractJaCoCoBinaries(version, dir string) error {
	cliPath := filepath.Join(dir, "jacococli.jar")

	downloadURL := fmt.Sprintf("https://github.com/jacoco/jacoco/releases/download/v%s/jacoco-%s.zip", version, version)

	_, err := os.Stat(cliPath)
	if err == nil {
		return nil
	}

	resp, err := http.Get(downloadURL)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	zipReader, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		return err
	}

	for _, file := range zipReader.File {
		if strings.HasSuffix(file.Name, "jacococli.jar") {
			cliFile, err := file.Open()
			if err != nil {
				return err
			}
			defer cliFile.Close()

			outFile, err := os.Create(cliPath)
			if err != nil {
				return err
			}
			defer outFile.Close()

			_, err = io.Copy(outFile, cliFile)
			if err != nil {
				return err
			}
		}
	}

	cliStat, err := os.Stat(cliPath)

	if os.IsNotExist(err) || cliStat != nil {
		return fmt.Errorf("failed to find JaCoCo binaries in the distribution")
	}

	return nil
}

func mergeJacocoCoverageFiles(ctx context.Context, jacocoCliPath string) error {
	// Find all .exec files starting with "test-set" in the target directory
	sourceFiles, err := filepath.Glob("target/test-set*.exec")
	if err != nil {
		return fmt.Errorf("error finding coverage files: %w", err)
	}
	if len(sourceFiles) == 0 {
		return errors.New("no coverage files found")
	}

	// Construct the command arguments
	args := []string{
		"java",
		"-jar",
		jacocoCliPath,
		"merge",
	}

	// Append each source file to the command
	for _, file := range sourceFiles {
		args = append(args, file)
	}

	// Specify the output file
	args = append(args, "--destfile", "target/keploy-e2e.exec")

	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to merge coverage files: %w", err)
	}

	return nil
}

func generateJacocoReport(ctx context.Context, jacocoCliPath string) error {
	reportDir := "target/site/keployE2E"

	// Ensure the report directory exists
	if err := os.MkdirAll(reportDir, 0755); err != nil {
		return fmt.Errorf("failed to create report directory: %w", err)
	}

	command := []string{
		"java",
		"-jar",
		jacocoCliPath,
		"report",
		"target/keploy-e2e.exec",
		"--classfiles",
		"target/classes",
		"--csv",
		reportDir + "/e2e.csv",
		"--html",
		reportDir,
	}

	cmd := exec.CommandContext(ctx, command[0], command[1:]...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to generate report: %w", err)
	}

	return nil
}
