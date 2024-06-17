//go:build linux

package replay

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
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
	failed int ``
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

// ReplaceBaseURL replaces the baseUrl of the old URL with the new URL's.
func ReplaceBaseURL(newURL, oldURL string) (string, error) {
	parsedOldURL, err := url.Parse(oldURL)
	if err != nil {
		return "", fmt.Errorf("failed to parse the old URL: %v", err)
	}

	parsedNewURL, err := url.Parse(newURL)
	if err != nil {
		return "", fmt.Errorf("failed to parse the new URL: %v", err)
	}
	// if scheme is empty, then add the scheme from the old URL in order to parse it correctly
	if parsedNewURL.Scheme == "" {
		parsedNewURL.Scheme = parsedOldURL.Scheme
		parsedNewURL, err = url.Parse(parsedNewURL.String())
		if err != nil {
			return "", fmt.Errorf("failed to parse the scheme added new URL: %v", err)
		}
	}

	parsedOldURL.Scheme = parsedNewURL.Scheme
	parsedOldURL.Host = parsedNewURL.Host
	path, err := url.JoinPath(parsedNewURL.Path, parsedOldURL.Path)
	if err != nil {
		return "", fmt.Errorf("failed to join '%v' and '%v' paths: %v", parsedNewURL.Path, parsedOldURL.Path, err)
	}
	parsedOldURL.Path = path

	replacedURL := parsedOldURL.String()
	return replacedURL, nil
}

type requestMockUtil struct {
	logger     *zap.Logger
	path       string
	mockName   string
	apiTimeout uint64
	basePath   string
}

func NewRequestMockUtil(logger *zap.Logger, path, mockName string, apiTimeout uint64, basePath string) RequestMockHandler {
	return &requestMockUtil{
		path:       path,
		logger:     logger,
		mockName:   mockName,
		apiTimeout: apiTimeout,
		basePath:   basePath,
	}
}
func (t *requestMockUtil) SimulateRequest(ctx context.Context, _ uint64, tc *models.TestCase, testSetID string) (*models.HTTPResp, error) {
	switch tc.Kind {
	case models.HTTP:
		t.logger.Debug("Before simulating the request", zap.Any("Test case", tc))
		resp, err := pkg.SimulateHTTP(ctx, *tc, testSetID, t.logger, t.apiTimeout)
		t.logger.Debug("After simulating the request", zap.Any("test case id", tc.Name))
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
		coverageData, err = CalGoCoverage(ctx)
	case models.Python:
		coverageData, err = CalPythonCoverage(ctx)
	case models.Node:
		coverageData, err = CalTypescriptCoverage()
	case models.Java:
		coverageData, err = CalJavaCoverage(logger)
	}
	if err != nil {
		utils.LogError(logger, err, "failed to calculate coverage for the test run")
	} else {
		logger.Sugar().Infoln(models.HighlightPassingString("Total Coverage Percentage: ", coverageData.TotalCov))
		err = reportDB.InsertCoverageReport(ctx, testRunID, &coverageData)
		if err != nil {
			utils.LogError(logger, err, "failed to update report with the coverage data")
		}
	}
}

func mergeAndGenerateJacocoReport(ctx context.Context, logger *zap.Logger) error {
	jacocoPath := filepath.Join(os.TempDir(), "jacoco")
	jacocoCliPath := filepath.Join(jacocoPath, "jacococli.jar")
	err := mergeJacocoCoverageFiles(ctx, jacocoCliPath)
	if err == nil {
		err = generateJacocoReport(ctx, jacocoCliPath)
		if err != nil {
			logger.Debug("failed to generate jacoco report", zap.Error(err))
		}
	} else {
		logger.Debug("failed to merge jacoco coverage data", zap.Error(err))
	}
	if err != nil {
		return err
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

	args = append(args, sourceFiles...)

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
	if err := os.MkdirAll(reportDir, 0777); err != nil {
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
		reportDir,
	}

	cmd := exec.CommandContext(ctx, command[0], command[1:]...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to generate report: %w", err)
	}

	return nil
}

func CalGoCoverage(ctx context.Context) (models.TestCoverage, error) {
	testCov := models.TestCoverage{
		FileCov:  make(map[string]string),
		TotalCov: "",
	}

	generateCovTxtCmd := exec.CommandContext(ctx, "go", "tool", "covdata", "textfmt", "-i="+os.Getenv("GOCOVERDIR"), "-o="+os.Getenv("GOCOVERDIR")+"/total-coverage.txt")
	_, err := generateCovTxtCmd.Output()
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

type TypescriptCoverage map[string]struct {
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

func CalTypescriptCoverage() (models.TestCoverage, error) {
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

func CalJavaCoverage(logger *zap.Logger) (models.TestCoverage, error) {
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
	defer func() {
		if err := file.Close(); err != nil {
			utils.LogError(logger, err, "Error closing coverage csv file")
		}
	}()

	reader := csv.NewReader(file)
	records, err := reader.ReadAll()
	if err != nil {
		return testCov, fmt.Errorf("failed to read CSV file: %w", err)
	}

	var totalInstructions, coveredInstructions int

	// Skip header row and process each record
	for i, record := range records {
		if i == 0 {
			continue // Skip header
		}

		// Parse instructions coverage data
		instructionsMissed, err := strconv.Atoi(record[3])
		if err != nil {
			return testCov, err
		}
		instructionsCovered, err := strconv.Atoi(record[4])
		if err != nil {
			return testCov, err
		}

		// Calculate total instructions and covered instructions
		totalInstructions += instructionsMissed + instructionsCovered
		coveredInstructions += instructionsCovered

		// Calculate coverage percentage for each class
		if instructionsCovered > 0 {
			coverage := float64(instructionsCovered) / float64(instructionsCovered+instructionsMissed) * 100
			classPath := strings.ReplaceAll(record[1], ".", string(os.PathSeparator))              // Replace dots with path separator
			testCov.FileCov[filepath.Join(classPath, record[2])] = fmt.Sprintf("%.2f%%", coverage) // Use class path as key
		}
	}
	if totalInstructions > 0 {
		totalCoverage := float64(coveredInstructions) / float64(totalInstructions) * 100
		testCov.TotalCov = fmt.Sprintf("%.2f%%", totalCoverage)
	}

	return testCov, nil
}

func (t *requestMockUtil) AfterTestHook(_ context.Context, testRunID, testSetID string, tsCnt int) (*models.TestReport, error) {
	t.logger.Debug("AfterTestHook", zap.Any("testRunID", testRunID), zap.Any("testSetID", testSetID), zap.Any("totalTestSetCount", tsCnt))
	return nil, nil
}

func (t *requestMockUtil) ProcessTestRunStatus(_ context.Context, status bool, testSetID string) {
	if status {
		t.logger.Debug("Test case passed for", zap.String("testSetID", testSetID))
	} else {
		t.logger.Debug("Test case failed for", zap.String("testSetID", testSetID))
	}
}

func (t *requestMockUtil) FetchMockName() string {
	return t.mockName
}

func (t *requestMockUtil) ProcessMockFile(_ context.Context, testSetID string) {
	if t.basePath != "" {
		t.logger.Debug("Mocking is disabled when basePath is given", zap.String("testSetID", testSetID), zap.String("basePath", t.basePath))
		return
	}
	t.logger.Debug("Mock file for test set", zap.String("testSetID", testSetID))
}
