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
	if utils.CmdType(g.commandType) == utils.DockerRun {
		index := strings.Index(g.cmd, "docker run")
		return g.cmd[:index+len("docker run")] +
			" -v " + os.Getenv("PWD") + ":" + os.Getenv("PWD") +
			" -e GOCOVERDIR=" + goCovPath + " " +
			g.cmd[index+len("docker run"):], nil
	}
	if utils.CmdType(g.commandType) != utils.Native {
		return g.cmd, nil
	}
	if !checkForCoverFlag(g.logger, g.cmd) {
		g.logger.Warn("go binary was not built with -cover flag")
		return g.cmd, errors.New("binary not coverable")
	}
	return g.cmd, nil
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

	generateCovTxtCmd := exec.CommandContext(g.ctx, "go", "tool", "covdata", "textfmt", "-i="+coverageDir, "-o="+coverageDir+"/total-coverage.txt")
	_, err = generateCovTxtCmd.Output()
	if err != nil {
		return testCov, err
	}

	coveragePerFileTmp := make(map[string][]int) // filename -> [noOfLines, coveredLines]
	covdata, err := os.ReadFile(coverageDir + "/total-coverage.txt")
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
	testCov.Loc = models.Loc{
		Total:   totalLines,
		Covered: totalCoveredLines,
	}
	return testCov, nil
}

func (g *Golang) AppendCoverage(coverage *models.TestCoverage, testRunID string) error {
	return g.reportDB.UpdateReport(g.ctx, testRunID, coverage)
}
