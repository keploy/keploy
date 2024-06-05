package utgen

import (
	"context"
	"fmt"
	"math"
	"os"
	"strings"

	"github.com/k0kubun/pp/v3"
	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

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
	failedTestRuns         []*models.FailedUnitTest
	prompt                 *Prompt
	aiCaller               *AICaller
	logger                 *zap.Logger
	promptBuilder          *PromptBuilder
	maxIterations          int
	Files                  []string
}

func NewUnitTestGenerator(sourceFilePath, testFilePath, codeCoverageReportPath, testCommand, testCommandDir, coverageType string, desiredCoverage float64, maxIterations int, model string, apiBaseURL string, config *config.Config, logger *zap.Logger) (*UnitTestGenerator, error) {
	generator := &UnitTestGenerator{
		sourceFilePath:         sourceFilePath,
		testFilePath:           testFilePath,
		codeCoverageReportPath: codeCoverageReportPath,
		testCommand:            testCommand,
		testCommandDir:         testCommandDir,
		coverageType:           coverageType,
		desiredCoverage:        desiredCoverage,
		maxIterations:          maxIterations,
		aiCaller:               NewAICaller(model, apiBaseURL, config.UtGen.Litellm),
		logger:                 logger,
	}
	return generator, nil
}

func (g *UnitTestGenerator) Start(ctx context.Context) error {

	if g.sourceFilePath == "" {
		if err := g.runCoverage(); err != nil {
			return err
		}
		if len(g.Files) == 0 {
			return fmt.Errorf("couldn't identify the source files Please mention source file and test file using flags")
		}
	}

	for i := 0; i < len(g.Files)+1; i++ {

		newTestFile := false

		var err error

		if i < len(g.Files) {
			g.sourceFilePath = g.Files[i]
			g.testFilePath, err = getTestFilePath(g.sourceFilePath, g.testCommandDir, GetCodeLanguage(g.sourceFilePath))
			if err != nil || g.testFilePath == "" {
				g.logger.Error("Error getting test file path", zap.Error(err))
				continue
			}

			isCreated, err := createTestFile(g.testFilePath, g.sourceFilePath)
			if err != nil {
				g.logger.Error("Error creating test file", zap.Error(err))
				continue
			}
			newTestFile = isCreated
		}

		g.logger.Info(fmt.Sprintf("Generating tests for file: %s", g.sourceFilePath))

		if !newTestFile {
			if err = g.runCoverage(); err != nil {
				return err
			}
		} else {
			g.currentCoverage = 0
		}

		// Run the initial coverage to get the base line
		iterationCount := 0
		g.language = GetCodeLanguage(g.sourceFilePath)
		g.promptBuilder = NewPromptBuilder(g.sourceFilePath, g.testFilePath, g.codeCoverageReport, "", "", "", g.language)

		if err := g.InitialUnitTestAnalysis(ctx); err != nil {
			utils.LogError(g.logger, err, "Error during initial test suite analysis")
			return err
		}

		for g.currentCoverage < (g.desiredCoverage/100) && iterationCount < g.maxIterations {

			pp.SetColorScheme(models.PassingColorScheme)
			if _, err := pp.Printf("\nCurrent Coverage: %s%% for file %s\n", math.Round(g.currentCoverage*100), g.sourceFilePath); err != nil {
				utils.LogError(g.logger, err, "failed to print coverage")
			}
			if _, err := pp.Printf("Desired Coverage: %s%% for file %s\n", g.desiredCoverage, g.sourceFilePath); err != nil {
				utils.LogError(g.logger, err, "failed to print coverage")
			}

			// Check for existence of failed tests:
			failedTestRunsValue := ""
			if g.failedTestRuns != nil && len(g.failedTestRuns) > 0 {
				for _, failedTest := range g.failedTestRuns {
					code := failedTest.TestCode
					errorMessage := failedTest.ErrorMsg
					failedTestRunsValue += fmt.Sprintf("Failed Test:\n\n%s\n\n", code)
					if errorMessage != "" {
						failedTestRunsValue += fmt.Sprintf("Error message for test above:\n%s\n\n\n", errorMessage)
					} else {
						failedTestRunsValue += "\n\n"
					}
				}
			}

			g.prompt = g.promptBuilder.BuildPrompt(failedTestRunsValue)
			g.failedTestRuns = []*models.FailedUnitTest{}

			testsDetails, err := g.GenerateTests(ctx)
			if err != nil {
				utils.LogError(g.logger, err, "Error generating tests")
				return err
			}

			for _, generatedTest := range testsDetails.NewTests {
				err := g.ValidateTest(generatedTest)
				if err != nil {
					utils.LogError(g.logger, err, "Error validating test")
					return err
				}
			}
			iterationCount++

			if g.currentCoverage < (g.desiredCoverage/100) && g.currentCoverage > 0 {
				if err := g.runCoverage(); err != nil {
					utils.LogError(g.logger, err, "Error running coverage")
					return err
				}
			}
		}

		if g.currentCoverage == 0 && newTestFile {
			err := os.Remove(g.testFilePath)
			if err != nil {
				g.logger.Error("Error removing test file", zap.Error(err))
			}
		}

		pp.SetColorScheme(models.PassingColorScheme)
		if g.currentCoverage >= (g.desiredCoverage / 100) {
			if _, err := pp.Printf("\nFor File %s Reached above target coverage of %s%% (Current Coverage: %s%%) in %s%% iterations.", g.sourceFilePath, g.desiredCoverage, math.Round(g.currentCoverage*100), iterationCount); err != nil {
				utils.LogError(g.logger, err, "failed to print coverage")
			}
		} else if iterationCount == g.maxIterations {
			if _, err := pp.Printf("\nFor File %s Reached maximum iteration limit without achieving desired coverage. Current Coverage: %s%%", g.sourceFilePath, math.Round(g.currentCoverage*100)); err != nil {
				utils.LogError(g.logger, err, "failed to print coverage")
			}
		}
	}
	return nil
}

func (g *UnitTestGenerator) runCoverage() error {
	// Perform an initial build/test command to generate coverage report and get a baseline
	if g.sourceFilePath != "" {
		g.logger.Info(fmt.Sprintf("Running build/test command to generate coverage report: '%s'", g.testCommand))
	}
	_, _, exitCode, timeOfTestCommand, err := RunCommand(g.testCommand, g.testCommandDir)
	if err != nil {
		utils.LogError(g.logger, err, "Error running test command")
	}
	if exitCode != 0 {
		utils.LogError(g.logger, err, "Error running test command")
	}
	coverageProcessor := NewCoverageProcessor(g.codeCoverageReportPath, getFilename(g.sourceFilePath), g.coverageType)
	_, _, percentageCovered, files, err := coverageProcessor.ProcessCoverageReport(timeOfTestCommand)
	if err != nil {
		utils.LogError(g.logger, err, "Error in coverage processing")
		return fmt.Errorf("error in coverage processing: %w", err)
	}
	g.currentCoverage = percentageCovered
	if g.sourceFilePath == "" {
		g.Files = files
	}
	return nil
}

func (g *UnitTestGenerator) GenerateTests(ctx context.Context) (*models.UnitTestsDetails, error) {
	response, promptTokenCount, responseTokenCount, err := g.aiCaller.CallModel(ctx, g.prompt, 4096)
	if err != nil {
		utils.LogError(g.logger, err, "Error calling AI model")
		return &models.UnitTestsDetails{}, err
	}
	g.logger.Info(fmt.Sprintf("Total token used count for LLM model %s: %d", g.aiCaller.Model, promptTokenCount+responseTokenCount))
	testsDetails := unmarshalYamlTestDetails(response)
	return testsDetails, nil
}

func (g *UnitTestGenerator) InitialUnitTestAnalysis(ctx context.Context) error {
	testHeadersIndentation, err := g.analyzeTestHeadersIndentation(ctx)
	if err != nil {
		return fmt.Errorf("failed to analyze test headers indentation: %w", err)
	}
	relevantLineNumberToInsertAfter, err := g.analyzeRelevantLineNumberToInsertAfter(ctx)
	if err != nil {
		return fmt.Errorf("failed to analyze relevant line number to insert new tests: %w", err)
	}
	g.testHeadersIndentation = testHeadersIndentation
	g.relevantLineNumber = relevantLineNumberToInsertAfter
	return nil
}

func (g *UnitTestGenerator) analyzeTestHeadersIndentation(ctx context.Context) (int, error) {
	testHeadersIndentation := -1
	allowedAttempts := 3
	counterAttempts := 0
	for testHeadersIndentation == -1 && counterAttempts < allowedAttempts {
		prompt := g.promptBuilder.BuildPromptCustom("analyze_suite_test_headers_indentation")
		response, _, _, err := g.aiCaller.CallModel(ctx, prompt, 4096)
		if err != nil {
			utils.LogError(g.logger, err, "Error calling AI model")
			return 0, err
		}
		testsDetails := unmarshalYamlTestHeaders(response)
		testHeadersIndentation, err = convertToInt(testsDetails.TestHeadersIndentation)
		if err != nil {
			return 0, fmt.Errorf("error converting test_headers_indentation to int: %w", err)
		}
		counterAttempts++
	}
	if testHeadersIndentation == -1 {
		return 0, fmt.Errorf("failed to analyze the test headers indentation")
	}
	return testHeadersIndentation, nil
}

func (g *UnitTestGenerator) analyzeRelevantLineNumberToInsertAfter(ctx context.Context) (int, error) {
	relevantLineNumberToInsertAfter := -1
	allowedAttempts := 3
	counterAttempts := 0
	for relevantLineNumberToInsertAfter == -1 && counterAttempts < allowedAttempts {
		prompt := g.promptBuilder.BuildPromptCustom("analyze_suite_test_insert_line")
		response, _, _, err := g.aiCaller.CallModel(ctx, prompt, 4096)
		if err != nil {
			utils.LogError(g.logger, err, "Error calling AI model")
			return 0, err
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

func (g *UnitTestGenerator) ValidateTest(generatedTest models.UnitTest) error {
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
	// Append the generated test to the relevant line in the test file
	originalContent := readFile(g.testFilePath)
	originalContentLines := strings.Split(originalContent, "\n")
	testCodeLines := strings.Split(testCodeIndented, "\n")
	processedTestLines := append(originalContentLines[:relevantLineNumberToInsertAfter], testCodeLines...)
	processedTestLines = append(processedTestLines, originalContentLines[relevantLineNumberToInsertAfter:]...)
	processedTest := strings.Join(processedTestLines, "\n")
	if err := os.WriteFile(g.testFilePath, []byte(processedTest), 0644); err != nil {
		return fmt.Errorf("failed to write test file: %w", err)
	}

	// Run the test using the Runner class
	g.logger.Info(fmt.Sprintf("Running test with the following command: '%s'", g.testCommand))

	var testCommandStartTime int64

	for i := 5; i > 0; i-- {
		stdout, _, exitCode, timeOfTestCommand, _ := RunCommand(g.testCommand, g.testCommandDir)
		if exitCode != 0 {
			// Test failed, roll back the test file to its original content

			if err := os.Truncate(g.testFilePath, 0); err != nil {
				return fmt.Errorf("failed to truncate test file: %w", err)
			}

			if err := os.WriteFile(g.testFilePath, []byte(originalContent), 0644); err != nil {
				return fmt.Errorf("failed to write test file: %w", err)
			}
			g.logger.Info("Skipping a generated test that failed")
			g.failedTestRuns = append(g.failedTestRuns, &models.FailedUnitTest{
				TestCode: generatedTest.TestCode,
				ErrorMsg: extractErrorMessage(stdout),
			})
			return nil
		}
		testCommandStartTime = timeOfTestCommand
	}

	// Check for coverage increase
	newCoverageProcessor := NewCoverageProcessor(g.codeCoverageReportPath, getFilename(g.sourceFilePath), g.coverageType)
	_, _, newPercentageCovered, _, err := newCoverageProcessor.ProcessCoverageReport(testCommandStartTime)
	if err != nil {
		return fmt.Errorf("error processing coverage report: %w", err)
	}
	if newPercentageCovered <= g.currentCoverage {
		// Test failed to increase coverage, roll back the test file to its original content

		if err := os.Truncate(g.testFilePath, 0); err != nil {
			return fmt.Errorf("failed to truncate test file: %w", err)
		}

		if err := os.WriteFile(g.testFilePath, []byte(originalContent), 0644); err != nil {
			return fmt.Errorf("failed to write test file: %w", err)
		}
		g.logger.Info("Skipping a generated test that failed to increase coverage")
		return nil
	}
	g.currentCoverage = newPercentageCovered
	g.relevantLineNumber = g.relevantLineNumber + len(testCodeLines)
	g.logger.Info("Generated test passed and increased coverage")
	return nil
}
