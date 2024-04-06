package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

func CalGoCoverage(ctx context.Context, logger *zap.Logger, testset string) map[string]string {
	coveragePerFileTmp := make(map[string][]int) // filename -> [noOfLines, coveredLines]
	generateCovTxtCmd := exec.CommandContext(ctx, "go", "tool", "covdata", "textfmt", "-i="+os.Getenv("GOCOVERDIR"), "-o="+os.Getenv("GOCOVERDIR")+"/total-coverage.txt")
	_, err := generateCovTxtCmd.Output()
	if err != nil {
		utils.LogError(logger, err, fmt.Sprintf("failed to get the coverage for %s", testset), zap.Any("cmd", generateCovTxtCmd.String()))
		return nil
	}
	covdata, err := os.ReadFile(os.Getenv("GOCOVERDIR") + "/total-coverage.txt")
	if err != nil {
		utils.LogError(logger, err, "failed to read the coverage file", zap.String("file", os.Getenv("GOCOVERDIR")+"/total-coverage.txt"))
		return nil
	}
	// a line is of the form: <filename>:<startLineRow>.<startLineCol>,<endLineRow>.<endLineCol> <noOfLines> <coveredOrNot>
	for _, line := range strings.Split(string(covdata), "\n") {
		line = strings.TrimSpace(line)
		if strings.Split(line, ":")[0] == "mode" || line == "" {
			continue
		}
		lineFields := strings.Fields(line)
		malformedErrMsg := "go coverage file is malformed"
		if len(lineFields) == 3 {
			noOfLines, err := strconv.Atoi(lineFields[1])
			if err != nil {
				utils.LogError(logger, err, malformedErrMsg)
				return nil
			}
			coveredOrNot, err := strconv.Atoi(lineFields[2])
			if err != nil {
				utils.LogError(logger, err, malformedErrMsg)
				return nil
			}
			i := strings.Index(line, ":")
			var filename string
			if i > 0 {
				filename = line[:i]
			} else {
				utils.LogError(logger, err, malformedErrMsg)
				return nil
			}

			if _, ok := coveragePerFileTmp[filename]; !ok {
				coveragePerFileTmp[filename] = make([]int, 2)
			}

			coveragePerFileTmp[filename][0] += noOfLines
			if coveredOrNot != 0 {
				coveragePerFileTmp[filename][1] += noOfLines
			}
		} else {
			utils.LogError(logger, err, malformedErrMsg)
			return nil
		}
	}

	// calculate percentage from the coveragePerFileTmp
	coveragePerFile := make(map[string]string) // filename -> coveragePercentage
	for filename, lines := range coveragePerFileTmp {
		covPercentage := float64(lines[1]*100) / float64(lines[0])
		coveragePerFile[filename] = strconv.FormatFloat(float64(covPercentage), 'f', 2, 64) + "%"
	}
	return coveragePerFile
}

type pyCoverage struct {
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

func CalPythonCoverage(ctx context.Context, logger *zap.Logger) map[string]string {
	covfile, err := utils.GetRecentFile(".", ".coverage.keploy")
	fmt.Println(covfile)
	if err != nil {
		utils.LogError(logger, err, "failed to get the coverage data file")
		return nil
	}
	generateCovJSONCmd := exec.CommandContext(ctx, "coverage", "json", "--data-file="+covfile)
	fmt.Println(generateCovJSONCmd.String())
	_, err = generateCovJSONCmd.Output()
	if err != nil {
		utils.LogError(logger, err, "failed to create a json report of coverage", zap.Any("cmd", generateCovJSONCmd.String()))
		return nil
	}
	coverageData, err := os.ReadFile("coverage.json")
	if err != nil {
		utils.LogError(logger, err, "failed to read the coverage.json file")
		return nil
	}
	var cov pyCoverage
	err = json.Unmarshal(coverageData, &cov)
	if err != nil {
		utils.LogError(logger, err, "failed to unmarshal the coverage data")
		return nil
	}
	coveragePerFile := make(map[string]string)
	for filename, file := range cov.Files {
		coveragePerFile[filename] = file.Summary.PercentCoveredDisplay
	}
	fmt.Println(coveragePerFile)
	return coveragePerFile
}
