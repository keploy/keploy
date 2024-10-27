package csharp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"go.uber.org/zap"
)

func downloadDotnetCoverage(ctx context.Context) error {
	// Consruct arguments for command to check if dotnet-coverage is already installed or not
	check_args := []string{
		"dotnet",
		"tool",
		"list",
		"-g",
	}

	checkCmd := exec.CommandContext(ctx, check_args[0], check_args[1:]...)
	checkCmd.Stdout = os.Stdout
	checkCmd.Stderr = os.Stderr

	if err := checkCmd.Run(); err != nil {
		return fmt.Errorf("failed to check for existing dotnet-coverage: %w", err)
	} else {
		if strings.Contains(checkCmd.String(), "dotnet-coverage") {
			fmt.Println("dotnet-coverage is already installed")
			return nil
		}
	}

	fmt.Println("dotnet-coverage not found. Installing...")

	// Construct the command arguments to install dotnet-coverage
	installArgs := []string{
		"dotnet",
		"tool",
		"install",
		"--global",
		"dotnet-coverage",
	}

	installCmd := exec.CommandContext(ctx, installArgs[0], installArgs[1:]...)
	installCmd.Stdout = os.Stdout
	installCmd.Stderr = os.Stderr

	if err := installCmd.Run(); err != nil {
		return fmt.Errorf("failed to install dotnet-coverage. Ensure .NET SDK is installed and try again: %w", err)
	}

	return nil
}

func MergeAndGenerateDotnetReport(ctx context.Context, logger *zap.Logger) error {
	err := MergeCoverageFiles(ctx)
	if err == nil {
		err = GenerateCoverageReport(ctx)

		if err != nil {
			logger.Debug("failed to generate dotnet-coverage report: %w", zap.Error(err))
		}
	} else {
		logger.Debug("failed to merge dotnet-coverage data: %w", zap.Error(err))
	}

	if err != nil {
		return err
	}

	return nil
}

func MergeCoverageFiles(ctx context.Context) error {
	// Find all .cobertura files in the target directory
	sourceFiles, err := filepath.Glob("target/*.cobertura")
	if err != nil {
		return fmt.Errorf("error finding coverage files: %w", err)
	}

	if len(sourceFiles) == 0 {
		return errors.New("no coverage files found")
	}

	// Construct command arguments to merge coverage files
	args := []string{
		"dotnet-coverage",
		"merge",
		"--output",
		"--output-format",
		"cobertura",
		"target/*.cobertura",
	}

	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Stderr = os.Stderr
	cmd.Stdout = os.Stdout

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to merge coverage files: %w", err)
	}

	return nil
}

func GenerateCoverageReport(ctx context.Context) error {
	reportDir := "target/site/keployE2E"

	// Ensure that the report dir exists
	if err := os.MkdirAll(reportDir, 0777); err != nil {
		return fmt.Errorf("failed to create report directory: %w", err)
	}

	// Consturct command arguments to generate the coverage report
	args := []string{
		"dotnet-coverage",
		"collect",
		"--output",
		"target/keploy-e2e.cobertura",
		"output.coverage",
		"--output-format",
		"cobertura",
		"--",
		"dotnet",
		"test",
	}

	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to generate report: %w", err)
	}

	return nil
}
