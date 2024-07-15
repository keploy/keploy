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

func (p *Python) PreProcess() (string, error) {
	cmd := exec.Command("coverage")
	err := cmd.Run()
	if err != nil {
		p.logger.Warn("coverage tool not found, skipping coverage caluclation. Please install coverage tool using 'pip install coverage'")
		return p.cmd, err
	}
	createPyCoverageConfig(p.logger)
	return strings.Replace(p.cmd, p.executable, "coverage run $APPEND --branch --data-file=.coverage.keploy", 1), nil
}

type pyCoverageFile struct {
	Files map[string]struct {
		Summary struct {
			CoveredLines          int     `json:"covered_lines"`
			NumStatements         int     `json:"num_statements"`
			PercentCovered        float64 `json:"percent_covered"`
			PercentCoveredDisplay string  `json:"percent_covered_display"`
			NumBranches           int     `json:"num_branches"`
			CoveredBranches       int     `json:"covered_branches"`
		} `json:"summary"`
	} `json:"files"`
	Totals struct {
		CoveredLines          int     `json:"covered_lines"`
		NumStatements         int     `json:"num_statements"`
		PercentCovered        float64 `json:"percent_covered"`
		PercentCoveredDisplay string  `json:"percent_covered_display"`
		NumBranches           int     `json:"num_branches"`
		CoveredBranches       int     `json:"covered_branches"`
	} `json:"totals"`
}

func (p *Python) GetCoverage() (models.TestCoverage, error) {
	testCov := models.TestCoverage{
		FileCov:  make(map[string]models.CoverageElement),
		TotalCov: models.CoverageElement{},
	}

	covFileName := os.Getenv("COVERAGE_FILE")
	if covFileName == "" {
		covFileName = ".coverage.keploy"
	}
	generateCovJSONCmd := exec.CommandContext(p.ctx, "python3", "-m", "coverage", "json", "--data-file="+covFileName)
	_, err := generateCovJSONCmd.Output()
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
		testCov.FileCov[filename] = models.CoverageElement{
			LineCov:   coverage.CalCovPercentage(file.Summary.CoveredLines, file.Summary.NumStatements),
			BranchCov: coverage.CalCovPercentage(file.Summary.CoveredBranches, file.Summary.NumBranches),
		}
	}
	testCov.TotalCov = models.CoverageElement{
		LineCov:   coverage.CalCovPercentage(cov.Totals.CoveredLines, cov.Totals.NumStatements),
		BranchCov: coverage.CalCovPercentage(cov.Totals.CoveredBranches, cov.Totals.NumBranches),
	}
	return testCov, nil
}

func (p *Python) AppendCoverage(coverage *models.TestCoverage, testRunID string) error {
	return p.reportDB.UpdateReport(p.ctx, testRunID, coverage)
}
