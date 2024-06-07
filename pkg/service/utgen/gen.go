// Package utgen is a service that generates unit tests for a given source code file.
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

type Coverage struct {
	Path    string
	Format  string
	Desired float64
	Current float64
	Content string
}

type Cursor struct {
	Line        int
	Indentation int
}

type UnitTestGenerator struct {
	srcPath       string
	testPath      string
	cmd           string
	dir           string
	cov           *Coverage
	lang          string
	cur           *Cursor
	failedTests   []*models.FailedUT
	prompt        *Prompt
	ai            *AIClient
	logger        *zap.Logger
	promptBuilder *PromptBuilder
	maxIterations int
	Files         []string
	tel           Telemetry
}

func NewUnitTestGenerator(srcPath, testPath, reportPath, cmd, dir, coverageFormat string, desiredCoverage float64, maxIterations int, model string, apiBaseURL string, _ *config.Config, tel Telemetry, logger *zap.Logger) (*UnitTestGenerator, error) {
	generator := &UnitTestGenerator{
		srcPath:       srcPath,
		testPath:      testPath,
		cmd:           cmd,
		dir:           dir,
		maxIterations: maxIterations,
		logger:        logger,
		tel:           tel,
		ai:            NewAIClient(model, apiBaseURL),
		cov: &Coverage{
			Path:    reportPath,
			Format:  coverageFormat,
			Desired: desiredCoverage,
		},
		cur: &Cursor{},
	}
	return generator, nil
}

func (g *UnitTestGenerator) Start(ctx context.Context) error {

	g.tel.GenerateUT()

	// To find the source files if the source path is not provided
	if g.srcPath == "" {
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
		// If the source file path is not provided, then iterate over all the source files and test files
		if i < len(g.Files) {
			g.srcPath = g.Files[i]
			g.testPath, err = getTestFilePath(g.srcPath, g.dir)
			if err != nil || g.testPath == "" {
				g.logger.Error("Error getting test file path", zap.Error(err))
				continue
			}
			isCreated, err := createTestFile(g.testPath, g.srcPath)
			if err != nil {
				g.logger.Error("Error creating test file", zap.Error(err))
				continue
			}
			newTestFile = isCreated
		}
		g.logger.Info(fmt.Sprintf("Generating tests for file: %s", g.srcPath))
		if !newTestFile {
			if err = g.runCoverage(); err != nil {
				return err
			}
		} else {
			g.cov.Current = 0
		}
		// Run the initial coverage to get the base line
		iterationCount := 0
		g.lang = GetCodeLanguage(g.srcPath)
		g.promptBuilder, err = NewPromptBuilder(g.srcPath, g.testPath, g.cov.Content, "", "", g.lang, g.logger)
		if err != nil {
			utils.LogError(g.logger, err, "Error creating prompt builder")
			return err
		}
		if err := g.setCursor(ctx); err != nil {
			utils.LogError(g.logger, err, "Error during initial test suite analysis")
			return err
		}

		for g.cov.Current < (g.cov.Desired/100) && iterationCount < g.maxIterations {
			pp.SetColorScheme(models.PassingColorScheme)
			if _, err := pp.Printf("\nCurrent Coverage: %s%% for file %s\n", math.Round(g.cov.Current*100), g.srcPath); err != nil {
				utils.LogError(g.logger, err, "failed to print coverage")
			}
			if _, err := pp.Printf("Desired Coverage: %s%% for file %s\n", g.cov.Desired, g.srcPath); err != nil {
				utils.LogError(g.logger, err, "failed to print coverage")
			}
			// Check for existence of failed tests:
			failedTestRunsValue := ""
			if g.failedTests != nil && len(g.failedTests) > 0 {
				for _, failedTest := range g.failedTests {
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
			g.prompt, err = g.promptBuilder.BuildPrompt("test_generation", failedTestRunsValue)
			if err != nil {
				utils.LogError(g.logger, err, "Error building prompt")
				return err
			}
			g.failedTests = []*models.FailedUT{}
			testsDetails, err := g.GenerateTests(ctx)
			if err != nil {
				utils.LogError(g.logger, err, "Error generating tests")
				return err
			}
			g.logger.Info("Validating new generated tests one by one")
			for _, generatedTest := range testsDetails.NewTests {
				err := g.ValidateTest(generatedTest)
				if err != nil {
					utils.LogError(g.logger, err, "Error validating test")
					return err
				}
			}
			iterationCount++
			if g.cov.Current < (g.cov.Desired/100) && g.cov.Current > 0 {
				if err := g.runCoverage(); err != nil {
					utils.LogError(g.logger, err, "Error running coverage")
					return err
				}
			}
		}

		if g.cov.Current == 0 && newTestFile {
			err := os.Remove(g.testPath)
			if err != nil {
				g.logger.Error("Error removing test file", zap.Error(err))
			}
		}

		pp.SetColorScheme(models.PassingColorScheme)
		if g.cov.Current >= (g.cov.Desired / 100) {
			if _, err := pp.Printf("\nFor File %s Reached above target coverage of %s%% (Current Coverage: %s%%) in %s%% iterations.", g.srcPath, g.cov.Desired, math.Round(g.cov.Current*100), iterationCount); err != nil {
				utils.LogError(g.logger, err, "failed to print coverage")
			}
		} else if iterationCount == g.maxIterations {
			if _, err := pp.Printf("\nFor File %s Reached maximum iteration limit without achieving desired coverage. Current Coverage: %s%%", g.srcPath, math.Round(g.cov.Current*100)); err != nil {
				utils.LogError(g.logger, err, "failed to print coverage")
			}
		}
	}
	return nil
}

func (g *UnitTestGenerator) runCoverage() error {
	// Perform an initial build/test command to generate coverage report and get a baseline
	if g.srcPath != "" {
		g.logger.Info(fmt.Sprintf("Running test command to generate coverage report: '%s'", g.cmd))
	}
	_, _, exitCode, lastUpdatedTime, err := RunCommand(g.cmd, g.dir)
	if err != nil {
		utils.LogError(g.logger, err, "Error running test command")
	}
	if exitCode != 0 {
		utils.LogError(g.logger, err, "Error running test command")
	}
	coverageProcessor := NewCoverageProcessor(g.cov.Path, getFilename(g.srcPath), g.cov.Format)
	coverageResult, err := coverageProcessor.ProcessCoverageReport(lastUpdatedTime)
	if err != nil {
		utils.LogError(g.logger, err, "Error in coverage processing")
		return fmt.Errorf("error in coverage processing: %w", err)
	}
	g.cov.Current = coverageResult.Coverage
	g.cov.Content = coverageResult.ReportContent
	if g.srcPath == "" {
		g.Files = coverageResult.Files
	}
	return nil
}

func (g *UnitTestGenerator) GenerateTests(ctx context.Context) (*models.UTDetails, error) {
	response, promptTokenCount, responseTokenCount, err := g.ai.Call(ctx, g.prompt, 4096)
	if err != nil {
		utils.LogError(g.logger, err, "Error calling AI model")
		return &models.UTDetails{}, err
	}
	g.logger.Info(fmt.Sprintf("Total token used count for LLM model %s: %d", g.ai.Model, promptTokenCount+responseTokenCount))
	testsDetails, err := unmarshalYamlTestDetails(response)
	if err != nil {
		utils.LogError(g.logger, err, "Error unmarshalling test details")
		return &models.UTDetails{}, err
	}
	return testsDetails, nil
}

func (g *UnitTestGenerator) setCursor(ctx context.Context) error {
	indentation, err := g.getIndentation(ctx)
	if err != nil {
		return fmt.Errorf("failed to analyze test headers indentation: %w", err)
	}
	line, err := g.getLine(ctx)
	if err != nil {
		return fmt.Errorf("failed to analyze relevant line number to insert new tests: %w", err)
	}
	g.cur.Indentation = indentation
	g.cur.Line = line
	return nil
}

func (g *UnitTestGenerator) getIndentation(ctx context.Context) (int, error) {
	indentation := -1
	allowedAttempts := 3
	counterAttempts := 0
	for indentation == -1 && counterAttempts < allowedAttempts {
		prompt, err := g.promptBuilder.BuildPrompt("indentation", "")
		if err != nil {
			return 0, fmt.Errorf("error building prompt: %w", err)
		}
		response, _, _, err := g.ai.Call(ctx, prompt, 4096)
		if err != nil {
			utils.LogError(g.logger, err, "Error calling AI model")
			return 0, err
		}
		testsDetails, err := unmarshalYamlTestHeaders(response)
		if err != nil {
			utils.LogError(g.logger, err, "Error unmarshalling test headers")
			return 0, err
		}
		indentation, err = convertToInt(testsDetails.Indentation)
		if err != nil {
			return 0, fmt.Errorf("error converting test_headers_indentation to int: %w", err)
		}
		counterAttempts++
	}
	if indentation == -1 {
		return 0, fmt.Errorf("failed to analyze the test headers indentation")
	}
	return indentation, nil
}

func (g *UnitTestGenerator) getLine(ctx context.Context) (int, error) {
	line := -1
	allowedAttempts := 3
	counterAttempts := 0
	for line == -1 && counterAttempts < allowedAttempts {
		prompt, err := g.promptBuilder.BuildPrompt("insert_line", "")
		if err != nil {
			return 0, fmt.Errorf("error building prompt: %w", err)
		}
		response, _, _, err := g.ai.Call(ctx, prompt, 4096)
		if err != nil {
			utils.LogError(g.logger, err, "Error calling AI model")
			return 0, err
		}
		testsDetails, err := unmarshalYamlTestLine(response)
		if err != nil {
			utils.LogError(g.logger, err, "Error unmarshalling test line")
			return 0, err
		}
		line, err = convertToInt(testsDetails.Line)
		if err != nil {
			return 0, fmt.Errorf("error converting relevant_line_number_to_insert_after to int: %w", err)
		}
		counterAttempts++
	}
	if line == -1 {
		return 0, fmt.Errorf("failed to analyze the relevant line number to insert new tests")
	}
	return line, nil
}

func (g *UnitTestGenerator) ValidateTest(generatedTest models.UT) error {
	testCode := strings.TrimSpace(generatedTest.TestCode)
	InsertAfter := g.cur.Line
	Indent := g.cur.Indentation
	testCodeIndented := testCode
	if Indent != 0 {
		initialIndent := len(testCode) - len(strings.TrimLeft(testCode, " "))
		deltaIndent := Indent - initialIndent
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
	originalContent, err := readFile(g.testPath)
	if err != nil {
		return fmt.Errorf("failed to read test file: %w", err)
	}
	originalContentLines := strings.Split(originalContent, "\n")
	testCodeLines := strings.Split(testCodeIndented, "\n")
	processedTestLines := append(originalContentLines[:InsertAfter], testCodeLines...)
	processedTestLines = append(processedTestLines, originalContentLines[InsertAfter:]...)
	processedTest := strings.Join(processedTestLines, "\n")
	if err := os.WriteFile(g.testPath, []byte(processedTest), 0644); err != nil {
		return fmt.Errorf("failed to write test file: %w", err)
	}

	// Run the test using the Runner class
	g.logger.Info(fmt.Sprintf("Running test 5 times for proper validation with the following command: '%s'", g.cmd))

	var testCommandStartTime int64

	for i := 0; i < 5; i++ {

		g.logger.Info(fmt.Sprintf("Iteration no: %d", i+1))

		stdout, _, exitCode, timeOfTestCommand, _ := RunCommand(g.cmd, g.dir)
		if exitCode != 0 {
			g.logger.Info(fmt.Sprintf("Test failed in %d iteration", i+1))
			// Test failed, roll back the test file to its original content

			if err := os.Truncate(g.testPath, 0); err != nil {
				return fmt.Errorf("failed to truncate test file: %w", err)
			}

			if err := os.WriteFile(g.testPath, []byte(originalContent), 0644); err != nil {
				return fmt.Errorf("failed to write test file: %w", err)
			}
			g.logger.Info("Skipping a generated test that failed")
			g.failedTests = append(g.failedTests, &models.FailedUT{
				TestCode: generatedTest.TestCode,
				ErrorMsg: extractErrorMessage(stdout),
			})
			return nil
		}
		testCommandStartTime = timeOfTestCommand
	}

	// Check for coverage increase
	newCoverageProcessor := NewCoverageProcessor(g.cov.Path, getFilename(g.srcPath), g.cov.Format)
	covResult, err := newCoverageProcessor.ProcessCoverageReport(testCommandStartTime)
	if err != nil {
		return fmt.Errorf("error processing coverage report: %w", err)
	}
	if covResult.Coverage <= g.cov.Current {
		// Test failed to increase coverage, roll back the test file to its original content

		if err := os.Truncate(g.testPath, 0); err != nil {
			return fmt.Errorf("failed to truncate test file: %w", err)
		}

		if err := os.WriteFile(g.testPath, []byte(originalContent), 0644); err != nil {
			return fmt.Errorf("failed to write test file: %w", err)
		}
		g.logger.Info("Skipping a generated test that failed to increase coverage")
		return nil
	}
	g.cov.Current = covResult.Coverage
	g.cur.Line = g.cur.Line + len(testCodeLines)
	g.logger.Info("Generated test passed and increased coverage")
	return nil
}
