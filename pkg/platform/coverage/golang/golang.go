// Package golang implements the methods for golang coverage services.
package golang

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"

	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/pkg/platform/coverage"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

type Golang struct {
	ctx                context.Context
	logger             *zap.Logger
	reportDB           coverage.ReportDB
	cmd                string
	coverageReportPath string
	commandType        string
}

func New(ctx context.Context, logger *zap.Logger, reportDB coverage.ReportDB, cmd, coverageReportPath, commandType string) *Golang {
	return &Golang{
		ctx:                ctx,
		logger:             logger,
		reportDB:           reportDB,
		cmd:                cmd,
		coverageReportPath: coverageReportPath,
		commandType:        commandType,
	}
}

func (g *Golang) PreProcess(_ bool) (string, error) {
	if !checkForCoverFlag(g.logger, g.cmd) {
		return g.cmd, errors.New("binary not coverable")
	}
	if utils.CmdType(g.commandType) == utils.Native {
		goCovPath, err := utils.SetCoveragePath(g.logger, g.coverageReportPath)
		if err != nil {
			g.logger.Warn("failed to set go coverage path", zap.Error(err))
			return g.cmd, err
		}
		err = os.Setenv("GOCOVERDIR", goCovPath)
		if err != nil {
			g.logger.Warn("failed to set GOCOVERDIR", zap.Error(err))
			return g.cmd, err
		}
	}
	return g.cmd, nil
}

func (g *Golang) GetCoverage() (models.TestCoverage, error) {
	testCov := models.TestCoverage{
		FileCov:  make(map[string]models.CoverageElement),
		TotalCov: models.CoverageElement{},
	}

	coverageDir := os.Getenv("GOCOVERDIR")

	f, err := os.Open(coverageDir)
	if err != nil {
		utils.LogError(g.logger, err, "failed to open coverage directory, skipping coverage calculation")
		return testCov, err
	}
	defer func() {
		if err := f.Close(); err != nil {
			utils.LogError(g.logger, err, "Error closing coverage directory, skipping coverage calculation")
		}
	}()

	_, err = f.Readdirnames(1) // Or f.Readdir(1)
	if err == io.EOF {
		utils.LogError(g.logger, err, fmt.Sprintf("no coverage files found in %s, skipping coverage calculation", coverageDir))
		return testCov, err
	}

	generateCovTxtCmd := exec.CommandContext(g.ctx, "go", "tool", "covdata", "textfmt", "-i="+coverageDir, "-o="+coverageDir+"/total-coverage.txt")
	_, err = generateCovTxtCmd.Output()
	if err != nil {
		return testCov, err
	}

	generateCovPercentCmd := exec.CommandContext(g.ctx, "go", "tool", "covdata", "func", "-i="+coverageDir)
	funcCoverageOutput, err := generateCovPercentCmd.Output()
	if err != nil {
		return testCov, err
	}

	coveragePerFileTmp, err := ParseTextFmtFile()
	if err != nil {
		return testCov, err
	}

	funcCoveragePerFile, err := ParseFuncFmtFile(string(funcCoverageOutput))
	if err != nil {
		return testCov, err
	}

	totalLines := 0
	totalCoveredLines := 0
	totalFunctions := 0
	totalCoveredFunctions := 0

	for filename, lines := range coveragePerFileTmp {
		totalLines += lines[0]
		totalCoveredLines += lines[1]
		totalFunctions += funcCoveragePerFile[filename][0]
		totalCoveredFunctions += funcCoveragePerFile[filename][1]
		testCov.FileCov[filename] = models.CoverageElement{
			LineCov: coverage.Percentage(lines[1], lines[0]),
			FuncCov: coverage.Percentage(funcCoveragePerFile[filename][1], funcCoveragePerFile[filename][0]),
		}
	}

	testCov.TotalCov = models.CoverageElement{
		LineCov: coverage.Percentage(totalCoveredLines, totalLines),
		FuncCov: coverage.Percentage(totalCoveredFunctions, totalFunctions),
	}
	return testCov, nil
}

func (g *Golang) AppendCoverage(coverage *models.TestCoverage, testRunID string) error {
	return g.reportDB.UpdateReport(g.ctx, testRunID, coverage)
}
