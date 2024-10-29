// Package javascript implements the methods for javascript coverage services.
package javascript

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/pkg/platform/coverage"
	"go.uber.org/zap"
)

type Javascript struct {
	ctx      context.Context
	logger   *zap.Logger
	reportDB coverage.ReportDB
	cmd      string
}

func New(ctx context.Context, logger *zap.Logger, reportDB coverage.ReportDB, cmd string) *Javascript {
	return &Javascript{
		ctx:      ctx,
		logger:   logger,
		reportDB: reportDB,
		cmd:      cmd,
	}
}

func (j *Javascript) PreProcess(disableLineCoverage bool) (string, error) {
	cmd := exec.Command("nyc", "--version")
	err := cmd.Run()
	if err != nil {
		j.logger.Warn("coverage tool not found, skipping coverage caluclation. please install coverage tool using 'npm install -g nyc'")
		return j.cmd, err
	}
	nycCmd := "nyc --clean=$CLEAN "
	if disableLineCoverage {
		nycCmd += "--reporter=none "
	}
	return nycCmd + j.cmd, nil
}

type Coverage map[string]struct {
	S map[string]interface{} `json:"s"`
	F map[string]interface{} `json:"f"`
	B map[string]interface{} `json:"b"`
}

func (j *Javascript) GetCoverage() (models.TestCoverage, error) {
	testCov := models.TestCoverage{
		FileCov:  make(map[string]models.CoverageElement),
		TotalCov: models.CoverageElement{},
	}

	coverageFilePaths, err := getCoverageFilePathsJavascript(filepath.Join(".", ".nyc_output", "processinfo"))
	if err != nil {
		return testCov, err
	}
	if len(coverageFilePaths) == 0 {
		return testCov, fmt.Errorf("no coverage files found")
	}

	// map[string]map[string]bool => filename -> execSegment(line, branch or func) -> covered or not
	linesCoveredPerFile := make(map[string]map[string]bool)  
	branchCoveredPerFile := make(map[string]map[string]bool) 
	funcCoveredPerFile := make(map[string]map[string]bool)   

	for _, coverageFilePath := range coverageFilePaths {

		coverageData, err := os.ReadFile(coverageFilePath)
		if err != nil {
			return testCov, err
		}
		var cov Coverage
		err = json.Unmarshal(coverageData, &cov)
		if err != nil {
			return testCov, err
		}

		for filename, file := range cov {
			AddCovInfoPerFile(linesCoveredPerFile, file.S, filename)
			AddCovInfoPerFile(branchCoveredPerFile, file.B, filename)
			AddCovInfoPerFile(funcCoveredPerFile, file.F, filename)
		}
	}

	totalLines, totalCoveredLines, coveredLinesPerFile := CalculateCoverageMetrics(linesCoveredPerFile)
	totalBranches, totalCoveredBranches, coveredBranchesPerFile := CalculateCoverageMetrics(branchCoveredPerFile)
	totalFunctions, totalCoveredFunctions, coveredFunctionsPerFile := CalculateCoverageMetrics(funcCoveredPerFile)

	for filename, lineCoverageCounts := range linesCoveredPerFile {
		testCov.FileCov[filename] = models.CoverageElement{
			LineCov:   coverage.Percentage(coveredLinesPerFile[filename],len(lineCoverageCounts)),
			BranchCov: coverage.Percentage(coveredBranchesPerFile[filename], len(branchCoveredPerFile[filename])),
			FuncCov:   coverage.Percentage(coveredFunctionsPerFile[filename], len(funcCoveredPerFile[filename])),
		}
	}

	testCov.TotalCov = models.CoverageElement{
		LineCov:   coverage.Percentage(totalCoveredLines, totalLines),
		BranchCov: coverage.Percentage(totalCoveredBranches, totalBranches),
		FuncCov:   coverage.Percentage(totalCoveredFunctions, totalFunctions),
	}
	return testCov, nil
}

func (j *Javascript) AppendCoverage(coverage *models.TestCoverage, testRunID string) error {
	return j.reportDB.UpdateReport(j.ctx, testRunID, coverage)
}
