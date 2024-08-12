// Package golang implements the methods for golang coverage services.
package golang

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/pkg/platform/coverage"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

type Golang struct {
	ctx                context.Context
	logger             *zap.Logger
	coverageReportPath string
	commandType        string
	cfg                *config.Config
}

func New(ctx context.Context, logger *zap.Logger, coverageReportPath string, cfg *config.Config) coverage.Service {
	return &Golang{
		ctx:                ctx,
		logger:             logger,
		coverageReportPath: coverageReportPath,
		cfg:                cfg,
	}
}

func (g *Golang) PreProcess(appCmd string, _ string) (string, error) {
	if !checkForCoverFlag(g.logger, appCmd) {
		return appCmd, errors.New("binary not coverable")
	}
	goCovPath, err := utils.SetCoveragePath(g.logger, g.coverageReportPath)
	if err != nil {
		g.logger.Warn("failed to set go coverage path", zap.Error(err))
		return appCmd, err
	}
	err = os.Setenv("GOCOVERDIR", goCovPath)
	if err != nil {
		g.logger.Warn("failed to set GOCOVERDIR", zap.Error(err))
		return appCmd, err
	}
	return appCmd, nil
}

func (g *Golang) GetCoverage() (models.TestCoverage, error) {
	testCov := models.TestCoverage{
		FileCov:  make(map[string]string),
		TotalCov: "",
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

	generateCovTxtCmd := exec.CommandContext(g.ctx, "go", "tool", "covdata", "textfmt", "-i="+os.Getenv("GOCOVERDIR"), "-o="+os.Getenv("GOCOVERDIR")+"/total-coverage.txt")
	_, err = generateCovTxtCmd.Output()
	if err != nil {
		return testCov, err
	}

	coveragePerFileTmp := make(map[string][]int) // filename -> [noOfLines, coveredLines]
	covdata, err := os.ReadFile(os.Getenv("GOCOVERDIR") + "/total-coverage.txt")
	if err != nil {
		return testCov, err
	}
	// a line is of the form: <filename>:<startLineRow>.<startLineCol>,<endLineRow>.<endLineCol> <noOfLines> <coveredOrNot>
	for idx, line := range strings.Split(string(covdata), "\n") {
		line = strings.TrimSpace(line)
		if strings.Split(line, ":")[0] == "mode" || line == "" {
			continue
		}
		lineFields := strings.Fields(line)
		malformedErrMsg := "go coverage file is malformed"
		if len(lineFields) == 3 {
			noOfLines, err := strconv.Atoi(lineFields[1])
			if err != nil {
				return testCov, err
			}
			coveredOrNot, err := strconv.Atoi(lineFields[2])
			if err != nil {
				return testCov, err
			}
			i := strings.Index(line, ":")
			var filename string
			if i > 0 {
				filename = line[:i]
			} else {
				return testCov, fmt.Errorf("%s at line %d", malformedErrMsg, idx)
			}

			if _, ok := coveragePerFileTmp[filename]; !ok {
				coveragePerFileTmp[filename] = make([]int, 2)
			}

			coveragePerFileTmp[filename][0] += noOfLines
			if coveredOrNot != 0 {
				coveragePerFileTmp[filename][1] += noOfLines
			}
		} else {
			return testCov, fmt.Errorf("%s at %d", malformedErrMsg, idx)
		}
	}

	totalLines := 0
	totalCoveredLines := 0
	for filename, lines := range coveragePerFileTmp {
		totalLines += lines[0]
		totalCoveredLines += lines[1]
		covPercentage := float64(lines[1]*100) / float64(lines[0])
		testCov.FileCov[filename] = strconv.FormatFloat(float64(covPercentage), 'f', 2, 64) + "%"
	}
	testCov.TotalCov = strconv.FormatFloat(float64(totalCoveredLines*100)/float64(totalLines), 'f', 2, 64) + "%"
	return testCov, nil
}
