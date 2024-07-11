package java

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

func DownloadAndExtractJaCoCoCli(logger *zap.Logger) error {
	jacocoPath := filepath.Join(os.TempDir(), "jacoco")
	err := os.MkdirAll(jacocoPath, 0777)
	if err != nil {
		logger.Debug("failed to create jacoco directory", zap.Error(err))
		return err
	}

	cliPath := filepath.Join(jacocoPath, "jacococli.jar")

	downloadURL := fmt.Sprintf("https://github.com/jacoco/jacoco/releases/download/v%s/jacoco-%s.zip", "0.8.12", "0.8.12")

	_, err = os.Stat(cliPath)
	if err == nil {
		return nil
	}

	resp, err := http.Get(downloadURL)
	if err != nil {
		return err
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			utils.LogError(logger, err, "failed to close response body")
		}
	}()

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
			defer func() {
				if err := cliFile.Close(); err != nil {
					utils.LogError(logger, err, "failed to close jacoco cli jar file")
				}
			}()

			outFile, err := os.Create(cliPath)
			if err != nil {
				return err
			}
			defer func() {
				if err := outFile.Close(); err != nil {
					utils.LogError(logger, err, "failed to close the output file for jacoco cli jar")
				}
			}()

			_, err = io.Copy(outFile, cliFile)
			if err != nil {
				return err
			}
		}
	}
	_, err = os.Stat(cliPath)

	if err != nil && os.IsNotExist(err) {
		return fmt.Errorf("failed to find JaCoCo binaries in the distribution")
	}

	return nil
}

func MergeAndGenerateJacocoReport(ctx context.Context, logger *zap.Logger) error {
	jacocoPath := filepath.Join(os.TempDir(), "jacoco")
	jacocoCliPath := filepath.Join(jacocoPath, "jacococli.jar")
	err := MergeJacocoCoverageFiles(ctx, jacocoCliPath)
	if err == nil {
		err = generateJacocoReport(ctx, jacocoCliPath)
		if err != nil {
			logger.Debug("failed to generate jacoco report", zap.Error(err))
		}
	} else {
		logger.Debug("failed to merge jacoco coverage data", zap.Error(err))
	}
	if err != nil {
		return err
	}
	return nil
}

func MergeJacocoCoverageFiles(ctx context.Context, jacocoCliPath string) error {
	// Find all .exec files in the target directory
	sourceFiles, err := filepath.Glob("target/*.exec")
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

	args = append(args, sourceFiles...)

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
	if err := os.MkdirAll(reportDir, 0777); err != nil {
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
