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
	"sort"
	"strconv"
	"strings"

	"github.com/tk103331/jacocogo/core/data"
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

func CalTypescriptCoverage(logger *zap.Logger) (map[string]string, error) {
	coverageFilePath, err := getCoverageFilePathTypescript(filepath.Join(".", ".nyc_output", "processinfo"))
	if err != nil {
		utils.LogError(logger, err, "failed to get the coverage data file")
		return nil, err
	}
	coverageData, err := os.ReadFile(coverageFilePath)
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
			if isStatementCovered.(float64) > 0 {
				totalLinesCovered++
			}
		}
		coveragePerFile[filename] = strconv.FormatFloat(float64(totalLinesCovered*100)/float64(len(file.S)), 'f', 2, 64) + "%"
	}
	return coveragePerFile, nil
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

func getCoverageFilePathTypescript(path string) (string, error) {
	files := utils.ByTime{}
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
				coverageFileInfo, err := os.Lstat(processInfo.CoverageFilename)
				if err != nil {
					return err
				}
				files = append(files, utils.File{Info: coverageFileInfo, Path: processInfo.CoverageFilename})
			}
		}
		return nil
	}
	err := filepath.Walk(path, walkfn)
	if err != nil {
		return "", err
	}
	sort.Sort(files)
	if len(files) == 0 {
		return "", err
	}
	return files[0].Path, nil
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
			classes = append(classes, p)
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
			coveragePerFile[className] = strconv.FormatFloat(float64(count)/float64(total), 'f', 2, 64) + "%"
		}
	}
	return coveragePerFile, nil
}
