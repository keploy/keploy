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

func CalPythonCoverage(ctx context.Context, logger *zap.Logger) (map[string]string, error) {
	covfile, err := utils.GetRecentFile(".", ".coverage.keploy")
	if err != nil {
		utils.LogError(logger, err, "failed to get the coverage data file")
		return nil, err
	}
	generateCovJSONCmd := exec.CommandContext(ctx, "coverage", "json", "--data-file="+covfile)
	_, err = generateCovJSONCmd.Output()
	if err != nil {
		utils.LogError(logger, err, "failed to create a json report of coverage", zap.Any("cmd", generateCovJSONCmd.String()))
		return nil, err
	}
	coverageData, err := os.ReadFile("coverage.json")
	if err != nil {
		utils.LogError(logger, err, "failed to read the coverage.json file")
		return nil, err
	}
	var cov pyCoverage
	err = json.Unmarshal(coverageData, &cov)
	if err != nil {
		utils.LogError(logger, err, "failed to unmarshal the coverage data")
		return nil, err
	}
	coveragePerFile := make(map[string]string)
	for filename, file := range cov.Files {
		coveragePerFile[filename] = file.Summary.PercentCoveredDisplay
	}
	return coveragePerFile, nil
}

type Start struct {
	Line   int `json:"line"`
	Column int `json:"column"`
}

type End struct {
	Line   int `json:"line"`
	Column int `json:"column"`
}

type Loc struct {
	Start `json:"start"`
	End   `json:"end"`
}

type TypescriptCoverage map[string]struct {
	Path         string `json:"path"`
	StatementMap map[string]struct {
		Start `json:"start"`
		End   `json:"end"`
	} `json:"statementMap"`
	FnMap map[string]struct {
		Name string `json:"name"`
		Decl struct {
			Start `json:"start"`
			End   `json:"end"`
		} `json:"decl"`
		Loc `json:"loc"`
		Line int `json:"line"`
	} `json:"fnMap"`
	BranchMap map[string]struct {
		Loc `json:"loc"`
		Type      string `json:"type"`
		Locations []struct {
			Start `json:"start"`
			End   `json:"end"`
		} `json:"locations"`
		Line int `json:"line"`
	} `json:"branchMap"`
	S              map[string]int `json:"s"`
	F              map[string]int `json:"f"`
	B              map[string]int `json:"b"`
	CoverageSchema string         `json:"_coverageSchema"`
	Hash           string         `json:"hash"`
	ContentHash    string         `json:"contentHash"`
}

func CalTypescriptCoverage(ctx context.Context, logger *zap.Logger) (map[string]string, error) {
	covfile, err := utils.GetLargestFile(".nyc_output")
	if err != nil {
		utils.LogError(logger, err, "failed to get the coverage data file")
		return nil, err
	}
	coverageData, err := os.ReadFile(covfile)
	if err != nil {
		utils.LogError(logger, err, "failed to read the coverage data file")
		return nil, err
	}
	var cov TypescriptCoverage
	err = json.Unmarshal(coverageData, &cov)
	if err != nil {
		utils.LogError(logger, err, "failed to unmarshal the coverage data")
		return nil, err
	}
	coveragePerFile := make(map[string]string)
	for filename, file := range cov {
		// coverage is calculated as: (no of statements covered / total no of statements) * 100
		// no of statements covered is the no of entries in S which has a value greater than 0
		// Total no of statements is len of S
		var totalLinesCovered int
		for _, isStatementCovered := range file.S {
			if isStatementCovered > 0 {
				totalLinesCovered++
			}
		} 
		coveragePerFile[filename] = strconv.FormatFloat(float64(totalLinesCovered*100)/float64(len(file.S)), 'f', 2, 64) + "%"
	}
	return coveragePerFile, nil
}
