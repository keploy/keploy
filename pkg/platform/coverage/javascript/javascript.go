// Package javascript implements the methods for javascript coverage services.
package javascript

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"

	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/pkg/platform/coverage"
	"go.uber.org/zap"
)

type Javascript struct {
	ctx            context.Context
	logger         *zap.Logger
	cfg            *config.Config
	testSetCounter int
}

func New(ctx context.Context, logger *zap.Logger, cfg *config.Config) coverage.Service {
	return &Javascript{
		ctx:    ctx,
		logger: logger,
		cfg:    cfg,
	}
}

func (j *Javascript) PreProcess(appCmd string, _ string) (string, error) {
	cmd := exec.Command("nyc", "--version")
	err := cmd.Run()
	if err != nil {
		j.logger.Warn("coverage tool not found, skipping coverage calculation. please install coverage tool using 'npm install -g nyc'")
		return appCmd, err
	}
	var nycCmd string
	if j.testSetCounter == 0 {
		nycCmd = "nyc --clean=true "
	} else {
		j.testSetCounter++
		nycCmd = "nyc --clean=false "
	}
	return nycCmd + appCmd, nil
}

type StartTy struct {
	Line   int `json:"line"`
	Column int `json:"column"`
}

type EndTy struct {
	Line   int `json:"line"`
	Column int `json:"column"`
}

type Loc struct {
	StartTy `json:"start"`
	EndTy   `json:"end"`
}

type Coverage map[string]struct {
	Path         string `json:"path"`
	StatementMap map[string]struct {
		StartTy `json:"start"`
		EndTy   `json:"end"`
	} `json:"statementMap"`
	FnMap map[string]struct {
		Name string `json:"name"`
		Decl struct {
			StartTy `json:"start"`
			EndTy   `json:"end"`
		} `json:"decl"`
		Loc  `json:"loc"`
		Line int `json:"line"`
	} `json:"fnMap"`
	BranchMap map[string]struct {
		Loc       `json:"loc"`
		Type      string `json:"type"`
		Locations []struct {
			StartTy `json:"start"`
			EndTy   `json:"end"`
		} `json:"locations"`
		Line int `json:"line"`
	} `json:"branchMap"`
	S              map[string]interface{} `json:"s"`
	F              map[string]interface{} `json:"f"`
	B              map[string]interface{} `json:"b"`
	CoverageSchema string                 `json:"_coverageSchema"`
	Hash           string                 `json:"hash"`
	ContentHash    string                 `json:"contentHash"`
}

func (j *Javascript) GetCoverage() (models.TestCoverage, error) {
	testCov := models.TestCoverage{
		FileCov:  make(map[string]string),
		TotalCov: "",
	}

	coverageFilePaths, err := getCoverageFilePathsJavascript(filepath.Join(".", ".nyc_output", "processinfo"))
	if err != nil {
		return testCov, err
	}
	if len(coverageFilePaths) == 0 {
		return testCov, fmt.Errorf("no coverage files found")
	}

	// coverage is calculated as: (no of statements covered / total no of statements) * 100
	// no of statements covered is the no of entries in S which has a value greater than 0
	// Total no of statements is len of S

	linesCoveredPerFile := make(map[string]map[string]bool) // filename -> line -> covered/not covered

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
			if _, ok := linesCoveredPerFile[filename]; !ok {
				linesCoveredPerFile[filename] = make(map[string]bool)
			}
			for line, isStatementCovered := range file.S {
				if _, ok := linesCoveredPerFile[filename][line]; !ok {
					linesCoveredPerFile[filename][line] = false
				}

				switch isExecSegmentCov := isStatementCovered.(type) {
				case float64:
					if (isExecSegmentCov) > 0 {
						linesCoveredPerFile[filename][line] = true
					}
				default:
					linesCoveredPerFile[filename][line] = false
				}
			}
		}
	}

	totalLines := 0
	totalCoveredLines := 0
	coveredLinesPerFile := make(map[string]int) // filename -> no of covered lines
	for filename, lines := range linesCoveredPerFile {
		for _, isCovered := range lines {
			totalLines++
			if isCovered {
				totalCoveredLines++
				coveredLinesPerFile[filename]++
			}
		}
	}

	for filename, lines := range linesCoveredPerFile {
		testCov.FileCov[filename] = strconv.FormatFloat(float64(coveredLinesPerFile[filename]*100)/float64(len(lines)), 'f', 2, 64) + "%"
	}
	testCov.TotalCov = strconv.FormatFloat(float64(totalCoveredLines*100)/float64(totalLines), 'f', 2, 64) + "%"
	return testCov, nil
}
