// Package python implements the methods for python coverage services.
package python

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/coverage"
	"go.uber.org/zap"
)

type Python struct {
	ctx        context.Context
	logger     *zap.Logger
	reportDB   coverage.ReportDB
	cmd        string
	executable string
}

func New(ctx context.Context, logger *zap.Logger, reportDB coverage.ReportDB, cmd, executable string) *Python {
	return &Python{
		ctx:        ctx,
		logger:     logger,
		reportDB:   reportDB,
		cmd:        cmd,
		executable: executable,
	}
}

func (p *Python) PreProcess(_ bool) (string, error) {
	cmd := exec.Command("coverage")
	err := cmd.Run()
	if err != nil {
		p.logger.Warn("coverage tool not found, skipping coverage caluclation. Please install coverage tool using 'pip install coverage'")
		return p.cmd, err
	}
	createPyCoverageConfig(p.logger)

	// Split the command into parts to handle environment variables and other prefixes
	parts := strings.Fields(p.cmd)

	// Find the index of the executable
	executableIndex := -1
	for i, part := range parts {
		if part == p.executable {
			executableIndex = i
			break
		}
	}

	if executableIndex == -1 {
		// Fallback to original behavior if executable not found as separate part
		covCmd := fmt.Sprintf("%s -m coverage run", p.executable)
		str := strings.Replace(p.cmd, p.executable, covCmd, 1)
		p.logger.Debug("PreProcess command for Python coverage (fallback)", zap.String("command", str))
		return str, nil
	}

	// Insert coverage flags right after the executable
	newParts := make([]string, 0, len(parts)+3)               // +3 for "-m", "coverage", "run"
	newParts = append(newParts, parts[:executableIndex+1]...) // Include executable
	newParts = append(newParts, "-m", "coverage", "run")      // Add coverage flags
	newParts = append(newParts, parts[executableIndex+1:]...) // Add remaining parts

	str := strings.Join(newParts, " ")
	p.logger.Debug("PreProcess command for Python coverage", zap.String("command", str), zap.String("executable", p.executable))
	return str, nil
}

type pyCoverageFile struct {
	Meta struct {
		Version        string `json:"version"`
		Timestamp      string `json:"timestamp"`
		BranchCoverage bool   `json:"branch_coverage"`
		ShowContexts   bool   `json:"show_contexts"`
	} `json:"meta"`
	Files map[string]struct {
		ExecutedLines []int `json:"executed_lines"`
		Summary       struct {
			CoveredLines          int     `json:"covered_lines"`
			NumStatements         int     `json:"num_statements"`
			PercentCovered        float64 `json:"percent_covered"`
			PercentCoveredDisplay string  `json:"percent_covered_display"`
			MissingLines          int     `json:"missing_lines"`
			ExcludedLines         int     `json:"excluded_lines"`
		} `json:"summary"`
		MissingLines  []int `json:"missing_lines"`
		ExcludedLines []int `json:"excluded_lines"`
	} `json:"files"`
	Totals struct {
		CoveredLines          int     `json:"covered_lines"`
		NumStatements         int     `json:"num_statements"`
		PercentCovered        float64 `json:"percent_covered"`
		PercentCoveredDisplay string  `json:"percent_covered_display"`
		MissingLines          int     `json:"missing_lines"`
		ExcludedLines         int     `json:"excluded_lines"`
	} `json:"totals"`
}

func (p *Python) GetCoverage() (models.TestCoverage, error) {
	testCov := models.TestCoverage{
		FileCov:  make(map[string]string),
		TotalCov: "",
	}

	covFileName := os.Getenv("COVERAGE_FILE")
	if covFileName == "" {
		covFileName = ".coverage.keploy"
	}

	p.logger.Info("Combining coverage from child processes when present; no impact if none exist")

	matches, err := filepath.Glob(".coverage.keploy.*")
	if err != nil {
		return testCov, fmt.Errorf("glob failed for .coverage.keploy.*: %w", err)
	}
	if len(matches) == 0 {
		p.logger.Warn("no per-process .coverage files found – nothing to combine")
		return testCov, nil
	}

	args := append([]string{
		"-m",
		"coverage",
		"combine",
		"--data-file=" + covFileName, // final merged file
	}, matches...)

	combineCmd := exec.CommandContext(p.ctx, p.executable, args...)
	combineCmd.Stdout = os.Stdout
	combineCmd.Stderr = os.Stderr

	if err := combineCmd.Run(); err != nil {
		p.logger.Error("failed to combine coverage files", zap.Error(err))
		return testCov, err
	}
	generateCovJSONCmd := exec.CommandContext(p.ctx, p.executable, "-m", "coverage", "json", "--data-file="+covFileName)
	generateCovJSONCmd.Stdout = os.Stdout
	generateCovJSONCmd.Stderr = os.Stderr
	err = generateCovJSONCmd.Run()
	if err != nil {
		return testCov, err
	}
	coverageData, err := os.ReadFile("coverage.json")
	if err != nil {
		return testCov, err
	}
	var cov pyCoverageFile
	err = json.Unmarshal(coverageData, &cov)
	if err != nil {
		return testCov, err
	}
	for filename, file := range cov.Files {
		testCov.FileCov[filename] = file.Summary.PercentCoveredDisplay + "%"
	}
	testCov.TotalCov = cov.Totals.PercentCoveredDisplay + "%"
	testCov.Loc = models.Loc{
		Total:   cov.Totals.NumStatements,
		Covered: cov.Totals.CoveredLines,
	}

	return testCov, nil
}

func (p *Python) AppendCoverage(coverage *models.TestCoverage, testRunID string) error {
	return p.reportDB.UpdateReport(p.ctx, testRunID, coverage)
}
