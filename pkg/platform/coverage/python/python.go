// Package python implements the methods for python coverage services.
package python

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strings"

	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/pkg/platform/coverage"
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

func (p *Python) PreProcess(disableLineCoverage bool) (string, error) {
	cmd := exec.Command("coverage")
	err := cmd.Run()
	if err != nil {
		p.logger.Warn("coverage tool not found, skipping coverage caluclation. Please install coverage tool using 'pip install coverage'")
		return p.cmd, err
	}
	createPyCoverageConfig(p.logger)
	return strings.Replace(p.cmd, p.executable, "coverage run $APPEND --data-file=.coverage.keploy", 1), nil
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
	generateCovJSONCmd := exec.CommandContext(p.ctx, "python3", "-m", "coverage", "json", "--data-file="+covFileName)
	generateCovJSONCmd.Stdout = os.Stdout
	generateCovJSONCmd.Stderr = os.Stderr
	err := generateCovJSONCmd.Run()
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
	return testCov, nil
}

func (p *Python) AppendCoverage(coverage *models.TestCoverage, testRunID string) error {
	return p.reportDB.UpdateReport(p.ctx, testRunID, coverage)
}
