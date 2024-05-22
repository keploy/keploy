package replay

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/pkg"
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

type TestReportVerdict struct {
	total  int
	passed int
	failed int
	status bool
}

func LeftJoinNoise(globalNoise config.GlobalNoise, tsNoise config.GlobalNoise) config.GlobalNoise {
	noise := globalNoise

	if _, ok := noise["body"]; !ok {
		noise["body"] = make(map[string][]string)
	}
	if tsNoiseBody, ok := tsNoise["body"]; ok {
		for field, regexArr := range tsNoiseBody {
			noise["body"][field] = regexArr
		}
	}

	if _, ok := noise["header"]; !ok {
		noise["header"] = make(map[string][]string)
	}
	if tsNoiseHeader, ok := tsNoise["header"]; ok {
		for field, regexArr := range tsNoiseHeader {
			noise["header"][field] = regexArr
		}
	}

	return noise
}

type testUtils struct {
	logger     *zap.Logger
	apiTimeout uint64
}

func NewTestUtils(apiTimeout uint64, logger *zap.Logger) RequestEmulator {
	return &testUtils{
		logger:     logger,
		apiTimeout: apiTimeout,
	}
}

func (t *testUtils) SimulateRequest(ctx context.Context, _ uint64, tc *models.TestCase, testSetID string) (*models.HTTPResp, error) {
	switch tc.Kind {
	case models.HTTP:
		t.logger.Debug("Before simulating the request", zap.Any("Test case", tc))
		t.logger.Debug(fmt.Sprintf("the url of the testcase: %v", tc.HTTPReq.URL))
		resp, err := pkg.SimulateHTTP(ctx, *tc, testSetID, t.logger, t.apiTimeout)
		t.logger.Debug("After simulating the request", zap.Any("test case id", tc.Name))
		t.logger.Debug("After GetResp of the request", zap.Any("test case id", tc.Name))
		return resp, err
	}
	return nil, nil
}

func CalculateAndInsertTestCoverage(ctx context.Context, logger *zap.Logger, reportDB ReportDB, testRunID string, language config.Language) {
	var coverageData models.TestCoverage
	logger.Info("calculating coverage for the test run and inserting it into the report")
	var err error
	switch language {
	case models.Go:
		coverageData, err = CalGoCoverage()
	case models.Python:
		coverageData, err = CalPythonCoverage(ctx)
	case models.Node:
		coverageData, err = CalTypescriptCoverage(ctx)
	case models.Java:
		coverageData, err = CalJavaCoverage(ctx)
	}
	if err != nil {
		utils.LogError(logger, err, "failed to calculate coverage for the test run")
	} else {
		err = reportDB.InsertCoverageReport(ctx, testRunID, &coverageData)
		if err != nil {
			utils.LogError(logger, err, "failed to update report with the coverage data")
		}
	}
}

func downloadAndExtractJaCoCoBinaries(version, dir string) error {
	cliPath := filepath.Join(dir, "jacococli.jar")

	downloadURL := fmt.Sprintf("https://github.com/jacoco/jacoco/releases/download/v%s/jacoco-%s.zip", version, version)

	_, err := os.Stat(cliPath)
	if err == nil {
		return nil
	}

	resp, err := http.Get(downloadURL)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	zipReader, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		return err
	}

	for _, file := range zipReader.File {
		if strings.HasSuffix(file.Name, "jacococli.jar") {
			cliFile, err := file.Open()
			if err != nil {
				return err
			}
			defer cliFile.Close()

			outFile, err := os.Create(cliPath)
			if err != nil {
				return err
			}
			defer outFile.Close()

			_, err = io.Copy(outFile, cliFile)
			if err != nil {
				return err
			}
		}
	}

	cliStat, err := os.Stat(cliPath)

	if os.IsNotExist(err) || cliStat != nil {
		return fmt.Errorf("failed to find JaCoCo binaries in the distribution")
	}

	return nil
}

func mergeJacocoCoverageFiles(ctx context.Context, jacocoCliPath string) error {
	// Find all .exec files starting with "test-set" in the target directory
	sourceFiles, err := filepath.Glob("target/test-set*.exec")
	if err != nil {
		return fmt.Errorf("error finding coverage files: %w", err)
	}
	if len(sourceFiles) == 0 {
		return errors.New("no coverage files found")
	}

	// Construct the command arguments
	args := []string{
		"java",
		"-jar",
		jacocoCliPath,
		"merge",
	}

	// Append each source file to the command
	for _, file := range sourceFiles {
		args = append(args, file)
	}

	// Specify the output file
	args = append(args, "--destfile", "target/keploy-e2e.exec")

	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to merge coverage files: %w", err)
	}

	return nil
}

func generateJacocoReport(ctx context.Context, jacocoCliPath string) error {
	reportDir := "target/site/keployE2E"

	// Ensure the report directory exists
	if err := os.MkdirAll(reportDir+"/html", 0755); err != nil {
		return fmt.Errorf("failed to create report directory: %w", err)
	}

	command := []string{
		"java",
		"-jar",
		jacocoCliPath,
		"report",
		"target/keploy-e2e.exec",
		"--classfiles",
		"target/classes",
		"--csv",
		reportDir + "/e2e.csv",
		"--html",
		reportDir + "/html",
	}

	cmd := exec.CommandContext(ctx, command[0], command[1:]...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to generate report: %w", err)
	}

	return nil
}

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
		covFileName = ".coverage.keploy"
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

func CalJavaCoverage(ctx context.Context) (models.TestCoverage, error) {
	testCov := models.TestCoverage{
		FileCov:  make(map[string]string),
		TotalCov: "",
	}

	// Define the path to the CSV file
	csvPath := filepath.Join("target", "site", "keployE2E", "e2e.csv")

	file, err := os.Open(csvPath)
	if err != nil {
		return testCov, fmt.Errorf("failed to open CSV file: %w", err)
	}
	defer file.Close()

	reader := csv.NewReader(file)
	records, err := reader.ReadAll()
	if err != nil {
		return testCov, fmt.Errorf("failed to read CSV file: %w", err)
	}

	var totalLines, coveredLines int

	// Skip header row and process each record
	for i, record := range records {
		if i == 0 {
			continue // Skip header
		}

		// Parse line coverage data
		lineMissed, err := strconv.Atoi(record[7])
		if err != nil {
			return testCov, err
		}
		lineCovered, err := strconv.Atoi(record[8])
		if err != nil {
			return testCov, err
		}

		// Calculate total lines and covered lines
		totalLines += lineMissed + lineCovered
		coveredLines += lineCovered

		// Calculate coverage percentage for each class
		if total := lineMissed + lineCovered; total > 0 {
			coverage := float64(lineCovered) / float64(total) * 100
			classPath := strings.ReplaceAll(record[1], ".", string(os.PathSeparator))              // Replace dots with path separator
			testCov.FileCov[filepath.Join(classPath, record[2])] = fmt.Sprintf("%.2f%%", coverage) // Use class path as key
		}
	}
	if totalLines > 0 {
		totalCoverage := float64(coveredLines) / float64(totalLines) * 100
		testCov.TotalCov = fmt.Sprintf("%.2f%%", totalCoverage)
	}

	return testCov, nil
}
