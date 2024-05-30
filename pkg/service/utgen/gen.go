package utgen

import (
	"fmt"
	"io/ioutil"
	"log"
	"math"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"go.keploy.io/server/v2/pkg/service/utgen/settings"
	"go.uber.org/zap"
	"gopkg.in/yaml.v2"
)

type TestResult struct {
	Status   string `yaml:"status"`
	Reason   string `yaml:"reason"`
	ExitCode int    `yaml:"exit_code"`
	Stderr   string `yaml:"stderr"`
	Stdout   string `yaml:"stdout"`
	Test     string `yaml:"test"`
}

type TestsDetails struct {
	Language                       string  `yaml:"language"`
	ExistingTestsFunctionSignature string  `yaml:"existing_test_function_signature"`
	NewTests                       []Tests `yaml:"new_tests"`
}

type Tests struct {
	TestBehavior   string `yaml:"test_behavior"`
	TestName       string `yaml:"test_name"`
	TestCode       string `yaml:"test_code"`
	NewImportsCode string `yaml:"new_imports_code"`
	TestsTags      string `yaml:"tests_tags"`
}

type TestHeader struct {
	Language               string `yaml:"language"`
	TestingFramework       string `yaml:"testing_framework"`
	NumberOfTests          int    `yaml:"number_of_tests"`
	TestHeadersIndentation int    `yaml:"test_headers_indentation"`
}

type TestLine struct {
	Language                        string `yaml:"language"`
	TestingFramework                string `yaml:"testing_framework"`
	NumberOfTests                   int    `yaml:"number_of_tests"`
	RelevantLineNumberToInsertAfter int    `yaml:"relevant_line_number_to_insert_after"`
}

type UnitTestGenerator struct {
	sourceFilePath         string
	testFilePath           string
	codeCoverageReportPath string
	testCommand            string
	testCommandDir         string
	coverageType           string
	desiredCoverage        float64
	language               string
	currentCoverage        float64
	codeCoverageReport     string
	relevantLineNumber     int
	testHeadersIndentation int
	failedTestRuns         []map[string]interface{}
	prompt                 *Prompt
	aiCaller               *AICaller
	logger                 *zap.Logger
	promptBuilder          *PromptBuilder
	maxIterations          int
}

func NewUnitTestGenerator(sourceFilePath, testFilePath, codeCoverageReportPath, testCommand, testCommandDir, coverageType string, desiredCoverage float64, maxIterations int, logger *zap.Logger) (*UnitTestGenerator, error) {
	generator := &UnitTestGenerator{
		sourceFilePath:         sourceFilePath,
		testFilePath:           testFilePath,
		codeCoverageReportPath: codeCoverageReportPath,
		testCommand:            testCommand,
		testCommandDir:         testCommandDir,
		coverageType:           coverageType,
		desiredCoverage:        desiredCoverage,
		maxIterations:          maxIterations,
		language:               GetCodeLanguage(sourceFilePath),
		aiCaller:               NewAICaller("gpt-4o", "http://localhost:11434"),
		logger:                 logger,
		failedTestRuns:         []map[string]interface{}{},
	}

	if err := generator.runCoverage(); err != nil {
		return nil, fmt.Errorf("failed to run coverage: %w", err)
	}

	prompt, err := generator.buildPrompt()
	if err != nil {
		return nil, fmt.Errorf("failed to build prompt: %w", err)
	}
	generator.prompt = prompt

	return generator, nil
}

func (g *UnitTestGenerator) Start() error {
	iterationCount := 0
	var testResultsList []TestResult

	// Initial analysis of the test suite
	if err := g.InitialTestSuiteAnalysis(); err != nil {
		g.logger.Error(fmt.Sprintf("Error during initial test suite analysis: %s", err))
		return err
	}

	// Run continuously until desired coverage has been met or we've reached the maximum iteration count
	for g.currentCoverage < (g.desiredCoverage/100) && iterationCount < g.maxIterations {
		g.logger.Info(fmt.Sprintf("Current Coverage: %.2f%%", math.Round(g.currentCoverage*100)))
		g.logger.Info(fmt.Sprintf("Desired Coverage: %.2f%%", g.desiredCoverage))

		// Generate tests by making a call to the LLM
		testsDetails, err := g.GenerateTests()
		if err != nil {
			g.logger.Error(fmt.Sprintf("Error generating tests: %s", err))
			return err
		}

		// Validate each test and append the results to the test results list
		for _, generatedTest := range testsDetails.NewTests {
			testResult, err := g.ValidateTest(generatedTest, testsDetails)
			if err != nil {
				g.logger.Error(fmt.Sprintf("Error validating test: %s", err))
				return err
			}
			testResultsList = append(testResultsList, testResult)
		}

		// Increment the iteration counter
		iterationCount++

		// Updating the coverage after each iteration
		if g.currentCoverage < (g.desiredCoverage / 100) {
			if err := g.runCoverage(); err != nil {
				g.logger.Error(fmt.Sprintf("Error running coverage: %s", err))
				return err
			}
		}
	}

	if g.currentCoverage >= (g.desiredCoverage / 100) {
		g.logger.Info(fmt.Sprintf("Reached above target coverage of %.2f%% (Current Coverage: %.2f%%) in %d iterations.", g.desiredCoverage, math.Round(g.currentCoverage*100), iterationCount))
	} else if iterationCount == g.maxIterations {
		g.logger.Info(fmt.Sprintf("Reached maximum iteration limit without achieving desired coverage. Current Coverage: %.2f%%", math.Round(g.currentCoverage*100)))
	}

	GenerateReport(testResultsList, "test_results.html")
	return nil
}

func GetCodeLanguage(sourceFilePath string) string {
	// Retrieve the mapping of languages to their file extensions from settings
	// Create a map to hold the language extensions
	languageExtensionMapOrg := make(map[string][]string)

	setting := settings.GetSettings()

	// Unmarshal the language_extension_map_org section into the map
	if err := setting.UnmarshalKey("language_extension_map_org", &languageExtensionMapOrg); err != nil {
		log.Fatalf("Error unmarshaling language extension map, %s", err)
	}

	// Initialize a dictionary to map file extensions to their corresponding languages
	extensionToLanguage := make(map[string]string)

	// Populate the extensionToLanguage dictionary
	for language, extensions := range languageExtensionMapOrg {
		for _, ext := range extensions {
			extensionToLanguage[ext] = language
		}
	}

	// Extract the file extension from the source file path
	parts := strings.Split(sourceFilePath, ".")
	extensionS := "." + parts[len(parts)-1]
	// Initialize the default language name as 'unknown'
	languageName := "unknown"

	// Check if the extracted file extension is in the dictionary
	if val, ok := extensionToLanguage[extensionS]; ok {
		languageName = val
	}

	// Return the language name in lowercase
	return strings.ToLower(languageName)
}

func (g *UnitTestGenerator) runCoverage() error {
	// Perform an initial build/test command to generate coverage report and get a baseline
	g.logger.Info(fmt.Sprintf("Running build/test command to generate coverage report: '%s'", g.testCommand))
	stdout, stderr, exitCode, timeOfTestCommand, err := RunCommand(g.testCommand, g.testCommandDir)
	if err != nil {
		return fmt.Errorf("error running command '%s': %w, output: %s, error: %s", g.testCommand, err, stdout, stderr)
	}
	if exitCode != 0 {
		return fmt.Errorf("error running test command '%s'. exit code %d. output: %s, error: %s", g.testCommand, exitCode, stdout, stderr)
	}
	coverageProcessor := NewCoverageProcessor(g.codeCoverageReportPath, getFilename(g.sourceFilePath), g.coverageType)
	linesCovered, linesMissed, percentageCovered, err := coverageProcessor.ProcessCoverageReport(timeOfTestCommand)
	if err != nil {
		g.logger.Error(fmt.Sprintf("Error in coverage processing: %s", err))
		return fmt.Errorf("error in coverage processing: %w", err)
	}
	g.currentCoverage = percentageCovered
	g.codeCoverageReport = fmt.Sprintf("Lines covered: %v\nLines missed: %v\nPercentage covered: %.2f", linesCovered, linesMissed, percentageCovered*100)
	return nil
}

func (g *UnitTestGenerator) buildPrompt() (*Prompt, error) {
	// Check for existence of failed tests:
	failedTestRunsValue := ""
	if len(g.failedTestRuns) > 0 {
		for _, failedTest := range g.failedTestRuns {
			code, ok := failedTest["test_code"].(string)
			if !ok {
				g.logger.Error("Error processing failed test runs: test_code is not a string")
				return nil, fmt.Errorf("error processing failed test runs: test_code is not a string")
			}
			errorMessage, ok := failedTest["error_message"].(string)
			if !ok {
				errorMessage = ""
			}
			failedTestRunsValue += fmt.Sprintf("Failed Test:\n\n%s\n\n", code)
			if errorMessage != "" {
				failedTestRunsValue += fmt.Sprintf("Error message for test above:\n%s\n\n\n", errorMessage)
			} else {
				failedTestRunsValue += "\n\n"
			}
		}
		g.failedTestRuns = nil // Reset the failed test runs
	}

	g.promptBuilder = NewPromptBuilder(g.sourceFilePath, g.testFilePath, g.codeCoverageReport, "", "", failedTestRunsValue, g.language)
	return g.promptBuilder.BuildPrompt(), nil
}

func (g *UnitTestGenerator) InitialTestSuiteAnalysis() error {
	testHeadersIndentation, err := analyzeTestHeadersIndentation(g.promptBuilder, g.aiCaller)
	if err != nil {
		return fmt.Errorf("failed to analyze test headers indentation: %w", err)
	}
	relevantLineNumberToInsertAfter, err := analyzeRelevantLineNumberToInsertAfter(g.promptBuilder, g.aiCaller)
	if err != nil {
		return fmt.Errorf("failed to analyze relevant line number to insert new tests: %w", err)
	}
	g.testHeadersIndentation = testHeadersIndentation
	g.relevantLineNumber = relevantLineNumberToInsertAfter
	return nil
}

func analyzeTestHeadersIndentation(promptBuilder *PromptBuilder, aiCaller *AICaller) (int, error) {
	testHeadersIndentation := -1
	allowedAttempts := 3
	counterAttempts := 0
	for testHeadersIndentation == -1 && counterAttempts < allowedAttempts {
		prompt := promptBuilder.BuildPromptCustom("analyze_suite_test_headers_indentation")
		response, _, _, err := aiCaller.CallModel(prompt, 4096)
		if err != nil {
			return 0, fmt.Errorf("error calling AI model: %w", err)
		}
		testsDetails := unmarshalYamlTestHeaders(response)
		if testsDetails.TestHeadersIndentation != 0 {
			testHeadersIndentation, err = convertToInt(testsDetails.TestHeadersIndentation)
			if err != nil {
				return 0, fmt.Errorf("error converting test_headers_indentation to int: %w", err)
			}
		}
		counterAttempts++
	}

	if testHeadersIndentation == -1 {
		return 0, fmt.Errorf("failed to analyze the test headers indentation")
	}

	return testHeadersIndentation, nil
}

func analyzeRelevantLineNumberToInsertAfter(promptBuilder *PromptBuilder, aiCaller *AICaller) (int, error) {
	relevantLineNumberToInsertAfter := -1
	allowedAttempts := 3
	counterAttempts := 0
	for relevantLineNumberToInsertAfter == -1 && counterAttempts < allowedAttempts {
		prompt := promptBuilder.BuildPromptCustom("analyze_suite_test_insert_line")
		response, _, _, err := aiCaller.CallModel(prompt, 4096)
		if err != nil {
			return 0, fmt.Errorf("error calling AI model: %w", err)
		}
		testsDetails := unmarshalYamlTestLine(response)
		if testsDetails.RelevantLineNumberToInsertAfter != 0 {
			relevantLineNumberToInsertAfter, err = convertToInt(testsDetails.RelevantLineNumberToInsertAfter)
			if err != nil {
				return 0, fmt.Errorf("error converting relevant_line_number_to_insert_after to int: %w", err)
			}
		}
		counterAttempts++
	}
	if relevantLineNumberToInsertAfter == -1 {
		return 0, fmt.Errorf("failed to analyze the relevant line number to insert new tests")
	}
	return relevantLineNumberToInsertAfter, nil
}

func (g *UnitTestGenerator) GenerateTests() (*TestsDetails, error) {
	response, promptTokenCount, responseTokenCount, err := g.aiCaller.CallModel(g.prompt, 4096)
	if err != nil {
		return &TestsDetails{}, fmt.Errorf("error calling AI model: %w", err)
	}
	g.logger.Info(fmt.Sprintf("Total token used count for LLM model %s: %d", g.aiCaller.Model, promptTokenCount+responseTokenCount))
	testsDetails := unmarshalYamlTestDetails(response)
	return testsDetails, nil
}

func (g *UnitTestGenerator) ValidateTest(generatedTest Tests, testsDetails *TestsDetails) (TestResult, error) {
	testCode := strings.TrimSpace(generatedTest.TestCode)
	relevantLineNumberToInsertAfter := g.relevantLineNumber
	neededIndent := g.testHeadersIndentation
	testCodeIndented := testCode
	if neededIndent != 0 {
		initialIndent := len(testCode) - len(strings.TrimLeft(testCode, " "))
		deltaIndent := neededIndent - initialIndent
		if deltaIndent > 0 {
			lines := strings.Split(testCode, "\n")
			for i, line := range lines {
				lines[i] = strings.Repeat(" ", deltaIndent) + line
			}
			testCodeIndented = strings.Join(lines, "\n")
		}
	}
	testCodeIndented = "\n" + strings.TrimSpace(testCodeIndented) + "\n"
	if testCodeIndented != "" && relevantLineNumberToInsertAfter != 0 {
		// Append the generated test to the relevant line in the test file
		originalContent := readFile(g.testFilePath)
		originalContentLines := strings.Split(originalContent, "\n")
		testCodeLines := strings.Split(testCodeIndented, "\n")
		processedTestLines := append(originalContentLines[:relevantLineNumberToInsertAfter], testCodeLines...)
		processedTestLines = append(processedTestLines, originalContentLines[relevantLineNumberToInsertAfter:]...)
		processedTest := strings.Join(processedTestLines, "\n")
		if err := ioutil.WriteFile(g.testFilePath, []byte(processedTest), 0644); err != nil {
			return TestResult{}, fmt.Errorf("failed to write test file: %w", err)
		}

		// Run the test using the Runner class
		g.logger.Info(fmt.Sprintf("Running test with the following command: '%s'", g.testCommand))
		stdout, stderr, exitCode, timeOfTestCommand, _ := RunCommand(g.testCommand, g.testCommandDir)

		if exitCode != 0 {
			// Test failed, roll back the test file to its original content
			if err := ioutil.WriteFile(g.testFilePath, []byte(originalContent), 0644); err != nil {
				return TestResult{}, fmt.Errorf("failed to write test file: %w", err)
			}
			g.logger.Info("Skipping a generated test that failed")
			failDetails := TestResult{
				Status:   "FAIL",
				Reason:   "Test failed",
				ExitCode: exitCode,
				Stderr:   stderr,
				Stdout:   stdout,
				Test:     generatedTest.TestCode,
			}
			g.failedTestRuns = append(g.failedTestRuns, map[string]interface{}{
				"test_code":     generatedTest.TestCode,
				"error_message": extractErrorMessagePython(stdout),
			})
			return failDetails, nil
		}

		// Check for coverage increase
		newCoverageProcessor := NewCoverageProcessor(g.codeCoverageReportPath, getFilename(g.sourceFilePath), g.coverageType)
		_, _, newPercentageCovered, err := newCoverageProcessor.ProcessCoverageReport(timeOfTestCommand)
		if err != nil {
			return TestResult{}, fmt.Errorf("error processing coverage report: %w", err)
		}
		if newPercentageCovered <= g.currentCoverage {
			// Test failed to increase coverage, roll back the test file to its original content
			if err := ioutil.WriteFile(g.testFilePath, []byte(originalContent), 0644); err != nil {
				return TestResult{}, fmt.Errorf("failed to write test file: %w", err)
			}
			g.logger.Info("Skipping a generated test that failed to increase coverage")
			failDetails := TestResult{
				Status:   "FAIL",
				Reason:   "Test failed to increase coverage",
				ExitCode: exitCode,
				Stderr:   stderr,
				Stdout:   stdout,
				Test:     generatedTest.TestCode,
			}
			g.failedTestRuns = append(g.failedTestRuns, map[string]interface{}{
				"test_code":     generatedTest.TestCode,
				"error_message": extractErrorMessagePython(stdout),
			})
			return failDetails, nil
		}
		g.currentCoverage = newPercentageCovered
		g.logger.Info("Generated test passed and increased coverage")
		return TestResult{
			Status:   "PASS",
			ExitCode: exitCode,
			Stderr:   stderr,
			Stdout:   stdout,
			Test:     generatedTest.TestCode,
		}, nil
	}
	return TestResult{}, nil
}

func unmarshalYamlTestDetails(yamlStr string) *TestsDetails {
	yamlStr = strings.TrimSpace(yamlStr)
	yamlStr = strings.TrimPrefix(yamlStr, "```yaml")
	yamlStr = strings.TrimSuffix(yamlStr, "```")
	var data *TestsDetails
	err := yaml.Unmarshal([]byte(yamlStr), &data)
	if err != nil {
		fmt.Println(err)
		return nil
	}
	return data
}

func unmarshalYamlTestHeaders(yamlStr string) *TestHeader {
	yamlStr = strings.TrimSpace(yamlStr)
	yamlStr = strings.TrimPrefix(yamlStr, "```yaml")
	yamlStr = strings.TrimSuffix(yamlStr, "```")

	var data *TestHeader
	err := yaml.Unmarshal([]byte(yamlStr), &data)
	if err != nil {
		fmt.Println(err)
		return nil
	}
	return data
}

func unmarshalYamlTestLine(yamlStr string) *TestLine {
	yamlStr = strings.TrimSpace(yamlStr)
	yamlStr = strings.TrimPrefix(yamlStr, "```yaml")
	yamlStr = strings.TrimSuffix(yamlStr, "```")
	var data *TestLine
	err := yaml.Unmarshal([]byte(yamlStr), &data)
	if err != nil {
		fmt.Println(err)
		return nil
	}
	return data

}

func convertToInt(value interface{}) (int, error) {
	switch v := value.(type) {
	case int:
		return v, nil
	case float64:
		return int(v), nil
	case string:
		return strconv.Atoi(v)
	default:
		return 0, fmt.Errorf("unsupported type for conversion to int: %T", value)
	}
}

func extractErrorMessagePython(failMessage string) string {
	const MAX_LINES = 15
	pattern := `={3,} FAILURES ={3,}(.*?)(={3,}|$)`
	re := regexp.MustCompile(pattern)
	match := re.FindStringSubmatch(failMessage)
	if len(match) > 1 {
		errStr := strings.TrimSpace(match[1])
		errStrLines := strings.Split(errStr, "\n")
		if len(errStrLines) > MAX_LINES {
			errStr = "...\n" + strings.Join(errStrLines[len(errStrLines)-MAX_LINES:], "\n")
		}
		return errStr
	}
	return ""
}

func getFilename(filePath string) string {
	return filepath.Base(filePath)
}
