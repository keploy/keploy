package tools

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/tk103331/jacocogo/core/data"
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

func CalGoCoverage() (models.TestCoverage, error) {
	testCov := models.TestCoverage{
		FileCov:  make(map[string]string),
		TotalCov: "",
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

func CalPythonCoverage(ctx context.Context) (models.TestCoverage, error) {
	testCov := models.TestCoverage{
		FileCov:  make(map[string]string),
		TotalCov: "",
	}

	covFileName := os.Getenv("COVERAGE_FILE")
	if covFileName == "" {
		covFileName = ".coverage"
	}
	generateCovJSONCmd := exec.CommandContext(ctx, "coverage", "json", "--data-file="+covFileName)
	_, err := generateCovJSONCmd.Output()
	if err != nil {
		return testCov, err
	}
	coverageData, err := os.ReadFile("coverage.json")
	if err != nil {
		return testCov, err
	}
	var cov pyCoverage
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
		Loc  `json:"loc"`
		Line int `json:"line"`
	} `json:"fnMap"`
	BranchMap map[string]struct {
		Loc       `json:"loc"`
		Type      string `json:"type"`
		Locations []struct {
			Start `json:"start"`
			End   `json:"end"`
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

func CalTypescriptCoverage(ctx context.Context) (models.TestCoverage, error) {
	testCov := models.TestCoverage{
		FileCov:  make(map[string]string),
		TotalCov: "",
	}

	coverageFilePaths, err := getCoverageFilePathsTypescript(filepath.Join(".", ".nyc_output", "processinfo"))
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
		var cov TypescriptCoverage
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
				if isStatementCovered.(float64) > 0 {
					linesCoveredPerFile[filename][line] = true
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

type ProcessInfo struct {
	Parent           string   `json:"parent"`
	Pid              int      `json:"pid"`
	Argv             []string `json:"argv"`
	ExecArgv         []string `json:"execArgv"`
	Cwd              string   `json:"cwd"`
	Time             int      `json:"time"`
	Ppid             int      `json:"ppid"`
	CoverageFilename string   `json:"coverageFilename"`
	ExternalID       string   `json:"externalId"`
	UUID             string   `json:"uuid"`
	Files            []string `json:"files"`
}

func getCoverageFilePathsTypescript(path string) ([]string, error) {
	filePaths := []string{}
	walkfn := func(path string, info os.FileInfo, err error) error {
		if !info.IsDir() && !strings.HasSuffix(path, "index.json") {
			fileData, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			var processInfo ProcessInfo
			err = json.Unmarshal(fileData, &processInfo)
			if err != nil {
				return err
			}
			if len(processInfo.Files) > 0 {
				filePaths = append(filePaths, processInfo.CoverageFilename)
			}
		}
		return nil
	}
	err := filepath.Walk(path, walkfn)
	if err != nil {
		return nil, err
	}
	return filePaths, nil
}

type sessionVisitor struct {
}

func (sessionVisitor) VisitSessionInfo(_ data.SessionInfo) error {
	return nil
}

type executionVisitor struct {
}

func (executionVisitor) VisitExecutionData(data data.ExecutionData) error {
	count := 0
	for _, p := range data.Probes {
		if p {
			count++
		}
	}

	file, err := os.OpenFile("testSetCoverage.txt", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0777)
	if err != nil {
		return err
	}

	defer func() {
		err := file.Close()
		if err != nil {
			utils.LogError(nil, err, "failed to close the file")
		}
	}()

	w := bufio.NewWriter(file)

	fmt.Fprintf(w, "%3d %3d %s\n", count, len(data.Probes), data.Name)

	err = w.Flush()
	if err != nil {
		return err
	}

	return nil
}

func CalJavaCoverage(logger *zap.Logger, testSetID string) (map[string]string, error) {
	covExecFile, err := os.Open(filepath.Join("target", testSetID+".exec"))
	if err != nil {
		utils.LogError(logger, err, "failed to open the coverage exec file")
		return nil, err
	}
	defer func() {
		err := covExecFile.Close()
		if err != nil {
			utils.LogError(logger, err, "failed to close the coverage exec file")
		}
	}()

	// parse the exec file and write the coverage data to a file
	reader := data.NewReader(covExecFile)
	reader.SetSessionVisitor(sessionVisitor{})
	reader.SetExecutionVisitor(executionVisitor{})
	_, err = reader.Read()
	if err != nil {
		utils.LogError(logger, err, "failed to read the coverage exec file")
		return nil, err
	}

	// fetch all the classes in the target folder
	classFolder := filepath.Join(".", "target", "classes")
	classes := []string{}
	walkfn := func(path string, info os.FileInfo, err error) error {
		if !info.IsDir() && filepath.Ext(path) == ".class" {
			p, err := filepath.Rel(classFolder, path)
			if err != nil {
				return err
			}
			classes = append(classes, strings.TrimSuffix(p, filepath.Ext(p)))
		}
		return nil
	}
	err = filepath.Walk(classFolder, walkfn)
	if err != nil {
		utils.LogError(logger, err, "failed to walk the classes directory in target folder")
		return nil, err
	}

	defer func() {
		err := os.Remove("testSetCoverage.txt")
		if err != nil {
			utils.LogError(logger, err, "failed to remove the coverage file")
		}
	}()

	covdata, err := os.ReadFile("testSetCoverage.txt")
	if err != nil {
		utils.LogError(logger, err, "failed to read the coverage file")
		return nil, err
	}
	coveragePerFile := make(map[string]string)
	malformedErrMsg := "java coverage file is malformed"
	for _, line := range strings.Split(string(covdata), "\n") {
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 3 {
			utils.LogError(logger, err, malformedErrMsg)
			return nil, err
		}
		countStr := fields[0]
		count, err := strconv.Atoi(countStr)
		if err != nil {
			utils.LogError(logger, err, malformedErrMsg)
			return nil, err
		}
		totalStr := fields[1]
		total, err := strconv.Atoi(totalStr)
		if err != nil {
			utils.LogError(logger, err, malformedErrMsg)
			return nil, err
		}
		className := fields[2]
		if slices.Contains(classes, className) {
			coveragePerFile[className] = strconv.FormatFloat(float64(count*100)/float64(total), 'f', 2, 64) + "%"
		}
	}
	return coveragePerFile, nil
}
