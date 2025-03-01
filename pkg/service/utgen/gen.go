// Package utgen is a service that generates unit tests for a given source code file.
package utgen

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/k0kubun/pp/v3"
	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/pkg/service"
	"go.keploy.io/server/v2/pkg/service/embedding"
	"go.keploy.io/server/v2/pkg/service/vectorstore"
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
	srcPath          string
	testPath         string
	cmd              string
	dir              string
	cov              *Coverage
	lang             string
	cur              *Cursor
	failedTests      []*models.FailedUT
	prompt           *Prompt
	ai               *AIClient
	logger           *zap.Logger
	promptBuilder    *PromptBuilder
	injector         *Injector
	maxIterations    int
	Files            []string
	tel              Telemetry
	additionalPrompt string
	totalTestCase    int
	testCasePassed   int
	testCaseFailed   int
	noCoverageTest   int
	flakiness        bool
	vectorStore      *vectorstore.MilvusStore
	embeddingService *embedding.EmbeddingService
	fileWatcher      *vectorstore.FileWatcher
}

var discardedTestsFilename = "discardedTests.txt"

func NewUnitTestGenerator(
	cfg *config.Config,
	tel Telemetry,
	auth service.Auth,
	logger *zap.Logger,
) (*UnitTestGenerator, error) {
	genConfig := cfg.Gen

	// Initialize vector store if enabled
	var milvusStore *vectorstore.MilvusStore
	var embeddingService *embedding.EmbeddingService
	var fileWatcher *vectorstore.FileWatcher

	if cfg.VectorStore.Enabled {
		// Initialize embedding service
		embeddingService = embedding.NewEmbeddingService(logger, cfg.Embedding.ApiKey)

		// Initialize vector store
		milvusConfig := &vectorstore.MilvusConfig{
			Host:           cfg.VectorStore.Host,
			Port:           cfg.VectorStore.Port,
			CollectionName: cfg.VectorStore.CollectionName,
			Dimension:      embeddingService.GetDimension(),
			IndexType:      cfg.VectorStore.IndexType,
			MetricType:     cfg.VectorStore.MetricType,
		}

		var err error
		milvusStore, err = vectorstore.NewMilvusStore(context.Background(), logger, milvusConfig)
		if err != nil {
			utils.LogError(logger, err, "failed to initialize vector store")
			// Continue without vector store
		} else {
			// Initialize file watcher if vector store is enabled
			ignorePatterns := strings.Split(cfg.VectorStore.IgnorePatterns, ",")
			fileWatcher, err = vectorstore.NewFileWatcher(logger, milvusStore, embeddingService, ignorePatterns)
			if err != nil {
				utils.LogError(logger, err, "failed to initialize file watcher")
			} else {
				// Start watching the current directory
				err = fileWatcher.WatchDirectory(context.Background(), ".")
				if err != nil {
					utils.LogError(logger, err, "failed to start file watcher")
				}
			}
		}
	}

	generator := &UnitTestGenerator{
		srcPath:       genConfig.SourceFilePath,
		testPath:      genConfig.TestFilePath,
		cmd:           genConfig.TestCommand,
		dir:           genConfig.TestDir,
		maxIterations: genConfig.MaxIterations,
		logger:        logger,
		tel:           tel,
		ai:            NewAIClient(genConfig, "", cfg.APIServerURL, auth, uuid.NewString(), logger),
		cov: &Coverage{
			Path:    genConfig.CoverageReportPath,
			Format:  genConfig.CoverageFormat,
			Desired: genConfig.DesiredCoverage,
		},
		additionalPrompt: genConfig.AdditionalPrompt,
		cur:              &Cursor{},
		flakiness:        genConfig.Flakiness,
		vectorStore:      milvusStore,
		embeddingService: embeddingService,
		fileWatcher:      fileWatcher,
	}
	return generator, nil
}

func (g *UnitTestGenerator) Start(ctx context.Context) error {
	g.tel.GenerateUT()

	// Check for context cancellation before proceeding
	select {
	case <-ctx.Done():
		return fmt.Errorf("process cancelled by user")
	default:
		// Continue if no cancellation
	}

	// To find the source files if the source path is not provided
	if g.srcPath == "" {
		if err := g.runCoverage(); err != nil {
			return err
		}
		if len(g.Files) == 0 {
			return fmt.Errorf("couldn't identify the source files. Please mention source file and test file using flags")
		}
	}
	const paddingHeight = 1
	columnWidths3 := []int{29, 29, 29}
	columnWidths2 := []int{40, 40}

	for i := 0; i < len(g.Files)+1; i++ {
		newTestFile := false
		var err error

		// Respect context cancellation in each iteration
		select {
		case <-ctx.Done():
			return fmt.Errorf("process cancelled by user")
		default:
		}

		// If the source file path is not provided, iterate over all the source files and test files
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
		isEmpty, err := utils.IsFileEmpty(g.testPath)
		if err != nil {
			g.logger.Error("Error checking if test file is empty", zap.Error(err))
			return err
		}
		if isEmpty {
			newTestFile = true
		}
		if !newTestFile {
			if err = g.runCoverage(); err != nil {
				return err
			}
		} else {
			g.cov.Current = 0
		}

		iterationCount := 1
		g.lang = GetCodeLanguage(g.srcPath)
		g.promptBuilder, err = NewPromptBuilder(g.srcPath, g.testPath, g.cov.Content, "", "", g.lang, g.additionalPrompt, g.ai.FunctionUnderTest, g.logger)
		g.injector = NewInjectorBuilder(g.logger, g.lang)

		if err != nil {
			utils.LogError(g.logger, err, "Error creating prompt builder")
			return err
		}
		if !isEmpty {
			if err := g.setCursor(ctx); err != nil {
				utils.LogError(g.logger, err, "Error during initial test suite analysis")
				return err
			}
		}
		initialCoverage := g.cov.Current
		// Respect context cancellation in the inner loop
		for g.cov.Current < (g.cov.Desired/100) && iterationCount <= g.maxIterations {
			passedTests, noCoverageTest, failedBuild, totalTest := 0, 0, 0, 0
			select {
			case <-ctx.Done():
				return fmt.Errorf("process cancelled by user")
			default:
			}

			pp.SetColorScheme(models.GetPassingColorScheme())
			if _, err := pp.Printf("Current Coverage: %s%% for file %s\n", math.Round(g.cov.Current*100), g.srcPath); err != nil {
				utils.LogError(g.logger, err, "failed to print coverage")
			}
			if _, err := pp.Printf("Desired Coverage: %s%% for file %s\n", g.cov.Desired, g.srcPath); err != nil {
				utils.LogError(g.logger, err, "failed to print coverage")
			}

			// Check for failed tests:
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

			g.promptBuilder.InstalledPackages, err = g.injector.libraryInstalled()
			g.promptBuilder.ImportDetails = g.injector.getModelDetails(g.srcPath)
			g.promptBuilder.ModuleName, _ = g.injector.GetModuleName(g.srcPath)
			if err != nil {
				utils.LogError(g.logger, err, "Error getting installed packages")
			}
			g.prompt, err = g.promptBuilder.BuildPrompt("test_generation", failedTestRunsValue)
			if err != nil {
				utils.LogError(g.logger, err, "Error building prompt")
				return err
			}
			g.failedTests = []*models.FailedUT{}
			testsDetails, err := g.GenerateTests(ctx, iterationCount)
			if err != nil {
				utils.LogError(g.logger, err, "Error generating tests")
				return err
			}
			if testsDetails == nil {
				g.logger.Info("No tests generated")
				continue
			}

			var originalSrcCode string
			var codeModified bool

			// Check if code refactoring is needed
			if len(testsDetails.RefactoredSourceCode) != 0 {
				// Read the original source code
				originalSrcCode, err = readFile(g.srcPath)
				if err != nil {
					return fmt.Errorf("failed to read the source code: %w", err)
				}

				// modify the source code for refactoring.
				if !(strings.Contains(testsDetails.RefactoredSourceCode, "blank output don't refactor code") || strings.Contains(testsDetails.RefactoredSourceCode, "no refactoring")) {
					if err := os.WriteFile(g.srcPath, []byte(testsDetails.RefactoredSourceCode), 0644); err != nil {
						return fmt.Errorf("failed to refactor source code:%w", err)
					}
					codeModified = true
				}
			}

			var overallCovInc = false

			g.logger.Info("Validating new generated tests one by one")
			g.totalTestCase += len(testsDetails.NewTests)
			totalTest = len(testsDetails.NewTests)

			for _, generatedTest := range testsDetails.NewTests {
				installedPackages, err := g.injector.libraryInstalled()
				if err != nil {
					g.logger.Warn("Error getting installed packages", zap.Error(err))
				}
				select {
				case <-ctx.Done():
					return fmt.Errorf("process cancelled by user")
				default:
				}
				coverageInc, err := g.ValidateTest(generatedTest, &passedTests, &noCoverageTest, &failedBuild, installedPackages)
				if err != nil {
					utils.LogError(g.logger, err, "Error validating test")

					rerr := revertSourceCode(g.srcPath, originalSrcCode, codeModified)
					if rerr != nil {
						utils.LogError(g.logger, rerr, "Error reverting source code")
					}

					return err
				}

				// if any test increases the coverage, set the flag to true
				overallCovInc = overallCovInc || coverageInc
			}
			// if any of the test couldn't increase the coverage, revert the source code
			if !overallCovInc {
				err := revertSourceCode(g.srcPath, originalSrcCode, codeModified)
				if err != nil {
					utils.LogError(g.logger, err, "Error reverting source code")
				}
			} else {
				g.promptBuilder.Src.Code = testsDetails.RefactoredSourceCode
			}
			if g.cov.Current < (g.cov.Desired/100) && g.cov.Current > 0 {
				if err := g.runCoverage(); err != nil {
					utils.LogError(g.logger, err, "Error running coverage")
					return err
				}
			}

			if len(g.failedTests) > 0 {
				err := g.saveFailedTestCasesToFile()
				if err != nil {
					utils.LogError(g.logger, err, "Error adding failed test cases to file")
				}
			}

			fmt.Printf("\n<=========================================>\n")
			fmt.Printf(("Tests generated in Session") + "\n")
			fmt.Printf("+-------------------------------+-------------------------------+-------------------------------+\n")
			fmt.Printf("| %s | %s | %s |\n",
				centerAlignText("Total Test Cases", 29),
				centerAlignText("Test Cases Passed", 29),
				centerAlignText("Test Cases Failed", 29))
			fmt.Printf("+-------------------------------+-------------------------------+-------------------------------+\n")
			fmt.Print(addHeightPadding(paddingHeight, 3, columnWidths3))
			fmt.Printf("| \033[33m%s\033[0m | \033[32m%s\033[0m | \033[33m%s\033[0m |\n",
				centerAlignText(fmt.Sprintf("%d", totalTest), 29),
				centerAlignText(fmt.Sprintf("%d", passedTests), 29),
				centerAlignText(fmt.Sprintf("%d", failedBuild+noCoverageTest), 29))
			fmt.Print(addHeightPadding(paddingHeight, 3, columnWidths3))
			fmt.Printf("+-------------------------------+-------------------------------+-------------------------------+\n")
			fmt.Printf(("Discarded tests in session") + "\n")
			fmt.Printf("+------------------------------------------+------------------------------------------+\n")
			fmt.Printf("| %s | %s |\n",
				centerAlignText("Build failures", 40),
				centerAlignText("No Coverage output", 40))
			fmt.Printf("+------------------------------------------+------------------------------------------+\n")
			fmt.Print(addHeightPadding(paddingHeight, 2, columnWidths2))
			fmt.Printf("| \033[35m%s\033[0m | \033[92m%s\033[0m |\n",
				centerAlignText(fmt.Sprintf("%d", failedBuild), 40),
				centerAlignText(fmt.Sprintf("%d", noCoverageTest), 40))
			fmt.Print(addHeightPadding(paddingHeight, 2, columnWidths2))
			fmt.Printf("+------------------------------------------+------------------------------------------+\n")
			fmt.Printf("<=========================================>\n")
			err = g.ai.SendCoverageUpdate(ctx, g.ai.SessionID, initialCoverage, g.cov.Current, iterationCount)
			if err != nil {
				utils.LogError(g.logger, err, "Error sending coverage update")
			}

			iterationCount++
		}

		if g.cov.Current == 0 && newTestFile {
			err := os.Remove(g.testPath)
			if err != nil {
				g.logger.Error("Error removing test file", zap.Error(err))
			}
		}

		pp.SetColorScheme(models.GetPassingColorScheme())
		if g.cov.Current >= (g.cov.Desired / 100) {
			if _, err := pp.Printf("For File %s Reached above target coverage of %s%% (Current Coverage: %s%%) in %s iterations.\n", g.srcPath, g.cov.Desired, math.Round(g.cov.Current*100), iterationCount); err != nil {
				utils.LogError(g.logger, err, "failed to print coverage")
			}
		} else if iterationCount > g.maxIterations {
			if _, err := pp.Printf("For File %s Reached maximum iteration limit without achieving desired coverage. Current Coverage: %s%%\n", g.srcPath, math.Round(g.cov.Current*100)); err != nil {
				utils.LogError(g.logger, err, "failed to print coverage")
			}
		}
	}
	fmt.Printf("\n<=========================================>\n")
	fmt.Printf(("COMPLETE TEST GENERATE SUMMARY") + "\n")
	fmt.Printf(("Total Test Summary") + "\n")

	fmt.Printf("+-------------------------------+-------------------------------+-------------------------------+\n")
	fmt.Printf("| %s | %s | %s |\n",
		centerAlignText("Total Test Cases", 29),
		centerAlignText("Test Cases Passed", 29),
		centerAlignText("Test Cases Failed", 29))

	fmt.Printf("+-------------------------------+-------------------------------+-------------------------------+\n")
	fmt.Print(addHeightPadding(paddingHeight, 3, columnWidths3))
	fmt.Printf("| \033[33m%s\033[0m | \033[32m%s\033[0m | \033[33m%s\033[0m |\n",
		centerAlignText(fmt.Sprintf("%d", g.totalTestCase), 29),
		centerAlignText(fmt.Sprintf("%d", g.testCasePassed), 29),
		centerAlignText(fmt.Sprintf("%d", g.testCaseFailed+g.noCoverageTest), 29))
	fmt.Print(addHeightPadding(paddingHeight, 3, columnWidths3))
	fmt.Printf("+-------------------------------+-------------------------------+-------------------------------+\n")

	fmt.Printf(("Discarded Cases Summary") + "\n")
	fmt.Printf("+------------------------------------------+------------------------------------------+\n")
	fmt.Printf("| %s | %s |\n",
		centerAlignText("Build failures", 40),
		centerAlignText("No Coverage output", 40))

	fmt.Printf("+------------------------------------------+------------------------------------------+\n")
	fmt.Print(addHeightPadding(paddingHeight, 2, columnWidths2))
	fmt.Printf("| \033[35m%s\033[0m | \033[92m%s\033[0m |\n",
		centerAlignText(fmt.Sprintf("%d", g.testCaseFailed), 40),
		centerAlignText(fmt.Sprintf("%d", g.noCoverageTest), 40))
	fmt.Print(addHeightPadding(paddingHeight, 2, columnWidths2))
	fmt.Printf("+------------------------------------------+------------------------------------------+\n")

	fmt.Printf("<=========================================>\n")
	return nil
}

func revertSourceCode(srcPath, originalSrcCode string, codeModified bool) error {
	if !codeModified {
		return nil
	}

	if err := os.WriteFile(srcPath, []byte(originalSrcCode), 0644); err != nil {
		return fmt.Errorf("failed to revert source code to the original state:%w", err)
	}
	return nil
}

func centerAlignText(text string, width int) string {
	text = strings.Trim(text, "\"")

	textLen := len(text)
	if textLen >= width {
		return text
	}

	leftPadding := (width - textLen) / 2
	rightPadding := width - textLen - leftPadding

	return fmt.Sprintf("%s%s%s", strings.Repeat(" ", leftPadding), text, strings.Repeat(" ", rightPadding))
}

func addHeightPadding(rows int, columns int, columnWidths []int) string {
	padding := ""
	for i := 0; i < rows; i++ {
		for j := 0; j < columns; j++ {
			if j == columns-1 {
				padding += fmt.Sprintf("| %-*s |\n", columnWidths[j], "")
			} else {
				padding += fmt.Sprintf("| %-*s ", columnWidths[j], "")
			}
		}
	}
	return padding
}

func statusUpdater(stop <-chan bool) {
	messages := []string{
		"Running tests... Please wait.",
		"Still running tests... Hang tight!",
		"Tests are still executing... Almost there!",
	}
	i := 0
	for {
		select {
		case <-stop:
			fmt.Printf("\r\033[K")
			return
		default:
			fmt.Printf("\r\033[K%s", messages[i%len(messages)])
			time.Sleep(5 * time.Second)
			i++
		}
	}
}

func (g *UnitTestGenerator) runCoverage() error {
	// Perform an initial build/test command to generate coverage report and get a baseline
	if g.srcPath != "" {
		g.logger.Info(fmt.Sprintf("Running test command to generate coverage report: '%s'", g.cmd))
	}

	stopStatus := make(chan bool)
	go statusUpdater(stopStatus)

	startTime := time.Now()

	_, _, exitCode, lastUpdatedTime, err := RunCommand(g.cmd, g.dir, g.logger)
	duration := time.Since(startTime)
	stopStatus <- true
	g.logger.Info(fmt.Sprintf("Test command completed in %v", formatDuration(duration)))

	if err != nil {
		g.logger.Warn("Test command failed. Ensure no tests are failing, and rerun the command.")
		return fmt.Errorf("error running test command: %s", g.cmd)
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

func (g *UnitTestGenerator) GenerateTests(ctx context.Context, iterationCount int) (*models.UTDetails, error) {
	fmt.Println("Generating Tests...")

	select {
	case <-ctx.Done():
		err := ctx.Err()
		return &models.UTDetails{}, err
	default:
	}

	requestPurpose := TestForFile
	if len(g.ai.FunctionUnderTest) > 0 {
		requestPurpose = TestForFunction
	}

	updatedTestContent, err := readFile(g.testPath)
	if err != nil {
		g.logger.Error("Error reading updated test file content", zap.Error(err))
		return &models.UTDetails{}, err
	}
	g.promptBuilder.Test.Code = updatedTestContent
	g.promptBuilder.CovReportContent = g.cov.Content
	g.prompt, err = g.promptBuilder.BuildPrompt("test_generation", "")
	if err != nil {
		utils.LogError(g.logger, err, "Error building prompt")
		return &models.UTDetails{}, err
	}

	aiRequest := AIRequest{
		MaxTokens:      4096,
		Prompt:         *g.prompt,
		SessionID:      g.ai.SessionID,
		Iteration:      iterationCount,
		RequestPurpose: requestPurpose,
	}

	response, err := g.ai.Call(ctx, CompletionParams{}, aiRequest, false)
	if err != nil {
		return &models.UTDetails{}, err
	}

	testsDetails, err := unmarshalYamlTestDetails(response)
	if err != nil {
		utils.LogError(g.logger, err, "Error unmarshalling test details")
		return &models.UTDetails{}, err
	}

	return testsDetails, nil
}

func (g *UnitTestGenerator) setCursor(ctx context.Context) error {
	fmt.Println("Getting indentation for new Tests...")
	indentation, err := g.getIndentation(ctx)
	if err != nil {
		return fmt.Errorf("failed to analyze test headers indentation: %w", err)
	}
	fmt.Println("Getting Line number for new Tests...")
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

		aiRequest := AIRequest{
			MaxTokens: 4096,
			Prompt:    *prompt,
			SessionID: g.ai.SessionID,
		}
		response, err := g.ai.Call(ctx, CompletionParams{}, aiRequest, false)
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

		aiRequest := AIRequest{
			MaxTokens: 4096,
			Prompt:    *prompt,
			SessionID: g.ai.SessionID,
		}
		response, err := g.ai.Call(ctx, CompletionParams{}, aiRequest, false)
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

func (g *UnitTestGenerator) ValidateTest(
	generatedTest models.UT,
	passedTests,
	noCoverageTest,
	failedBuild *int,
	installedPackages []string,
) (bool, error) {
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
	testCodeIndented = "\n" + g.injector.addCommentToTest(strings.TrimSpace(testCodeIndented)) + "\n"
	originalContent, err := readFile(g.testPath)
	if err != nil {
		return false, fmt.Errorf("failed to read test file: %w", err)
	}
	originalContentLines := strings.Split(originalContent, "\n")
	testCodeLines := strings.Split(testCodeIndented, "\n")
	if InsertAfter > len(originalContentLines) {
		InsertAfter = len(originalContentLines)
	}
	processedTestLines := append(originalContentLines[:InsertAfter], testCodeLines...)
	processedTestLines = append(processedTestLines, originalContentLines[InsertAfter:]...)
	processedTest := strings.Join(processedTestLines, "\n")
	if err := os.WriteFile(g.testPath, []byte(processedTest), 0644); err != nil {
		return false, fmt.Errorf("failed to write test file: %w", err)
	}
	importLen, err := g.injector.updateImports(g.testPath, generatedTest.NewImportsCode)
	if err != nil {
		g.logger.Warn("Error updating imports", zap.Error(err))
	}
	newInstalledPackages, err := g.injector.installLibraries(generatedTest.LibraryInstallationCode, installedPackages)
	if err != nil {
		g.logger.Debug("Error installing libraries", zap.Error(err))
	}

	g.logger.Info(fmt.Sprintf("Running Test with command: '%s'", g.cmd))
	stdout, stderr, exitCode, timeOfTestCommand, _ := RunCommand(g.cmd, g.dir, g.logger)
	if exitCode != 0 {
		g.logger.Info("Test Run Failed")
		if err := os.WriteFile(g.testPath, []byte(originalContent), 0644); err != nil {
			return false, fmt.Errorf("failed to revert test file: %w", err)
		}
		err = g.injector.uninstallLibraries(newInstalledPackages)
		if err != nil {
			g.logger.Warn("Error uninstalling libraries", zap.Error(err))
		}
		// Mark test as failed
		g.failedTests = append(g.failedTests, &models.FailedUT{
			TestCode:                generatedTest.TestCode,
			ErrorMsg:                extractErrorMessage(stdout, stderr, g.lang),
			NewImportsCode:          generatedTest.NewImportsCode,
			LibraryInstallationCode: generatedTest.LibraryInstallationCode,
		})
		g.testCaseFailed++
		*failedBuild++
		return false, nil
	}

	coverageProcessor := NewCoverageProcessor(g.cov.Path, getFilename(g.srcPath), g.cov.Format)
	coverageResult, err := coverageProcessor.ProcessCoverageReport(timeOfTestCommand)
	if err != nil {
		utils.LogError(g.logger, err, "Error in coverage processing")
		return false, fmt.Errorf("error in coverage processing: %w", err)
	}
	initialCoverage := g.cov.Current
	g.cov.Current = coverageResult.Coverage
	g.cov.Content = coverageResult.ReportContent
	if g.srcPath == "" {
		g.Files = coverageResult.Files
	}

	coverageIncreased := g.cov.Current > initialCoverage
	if !coverageIncreased {
		g.logger.Info("No coverage increase detected after initial test run.")
		// Revert test file to original content
		if err := os.WriteFile(g.testPath, []byte(originalContent), 0644); err != nil {
			return false, fmt.Errorf("failed to revert test file: %w", err)
		}
		// Uninstall any installed libraries
		err = g.injector.uninstallLibraries(newInstalledPackages)
		if err != nil {
			g.logger.Warn("Error uninstalling libraries", zap.Error(err))
		}
		// Mark test as ineffective
		g.noCoverageTest++
		*noCoverageTest++
		return false, nil
	}

	if g.flakiness {
		// Run the Test Five Times to Check for Flakiness
		g.logger.Info("Coverage increased. Running additional test iterations to check for flakiness.")
		for i := 0; i < 5; i++ {
			g.logger.Info(fmt.Sprintf("Flakiness Check - Iteration no: %d", i+1))
			stdout, stderr, exitCode, _, _ := RunCommand(g.cmd, g.dir, g.logger)
			if exitCode != 0 {
				g.logger.Info(fmt.Sprintf("Flaky test detected on iteration %d: %s", i+1, stderr))
				// Revert test file to original content
				if err := os.WriteFile(g.testPath, []byte(originalContent), 0644); err != nil {
					return false, fmt.Errorf("failed to revert test file: %w", err)
				}
				// Uninstall any installed libraries
				err = g.injector.uninstallLibraries(newInstalledPackages)
				if err != nil {
					g.logger.Warn("Error uninstalling libraries", zap.Error(err))
				}
				g.failedTests = append(g.failedTests, &models.FailedUT{
					TestCode:                generatedTest.TestCode,
					ErrorMsg:                extractErrorMessage(stdout, stderr, g.lang),
					NewImportsCode:          generatedTest.NewImportsCode,
					LibraryInstallationCode: generatedTest.LibraryInstallationCode,
				})
				g.testCaseFailed++
				*failedBuild++
				return false, nil
			}
		}
	}
	g.testCasePassed++
	*passedTests++
	g.cov.Current = coverageResult.Coverage
	g.logger.Info("Generated test passed and increased coverage")
	g.cur.Line = g.cur.Line + len(testCodeLines) + importLen
	return true, nil
}

func (g *UnitTestGenerator) saveFailedTestCasesToFile() error {
	dir, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("error getting current directory: %w", err)
	}

	newFilePath := filepath.Join(dir, discardedTestsFilename)

	fileHandle, err := os.OpenFile(newFilePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("error opening discarded tests file: %w", err)
	}

	defer func() {
		err := fileHandle.Close()
		if err != nil {
			g.logger.Error("Error closing file handle", zap.Error(err))
		}
	}()

	var builder strings.Builder

	// Writing Test Cases To File
	for _, failedTest := range g.failedTests {
		builder.WriteString("\n" + strings.Repeat("-", 20) + "Test Case" + strings.Repeat("-", 20) + "\n")
		if len(failedTest.NewImportsCode) > 0 {
			builder.WriteString(fmt.Sprintf("Import Statements:\n%s\n", failedTest.NewImportsCode))
		}
		if len(failedTest.LibraryInstallationCode) > 0 {
			builder.WriteString(fmt.Sprintf("Required Library Installation\n%s\n", failedTest.LibraryInstallationCode))
		}
		builder.WriteString(fmt.Sprintf("Test Implementation:\n%s\n\n", failedTest.TestCode))
		if len(failedTest.ErrorMsg) > 0 {
			builder.WriteString(fmt.Sprintf("Error Message:\n%s\n", failedTest.ErrorMsg))
		}
		builder.WriteString(strings.Repeat("-", 49) + "\n")
	}

	_, err = fileHandle.WriteString(fmt.Sprintf("%s\n", builder.String()))
	if err != nil {
		return fmt.Errorf("error writing to discarded tests file: %w", err)
	}
	return nil
}

func (g *UnitTestGenerator) FindSimilarCode(ctx context.Context, codeSnippet string, topK int) ([]vectorstore.CodeSearchResult, error) {
	if g.vectorStore == nil || g.embeddingService == nil {
		return nil, fmt.Errorf("vector store or embedding service not initialized")
	}

	// Generate embedding for the code snippet
	embedding, err := g.embeddingService.GenerateEmbedding(ctx, codeSnippet)
	if err != nil {
		utils.LogError(g.logger, err, "failed to generate embedding for code snippet")
		return nil, err
	}

	// Search in vector store
	results, err := g.vectorStore.SearchSimilarCode(ctx, embedding, topK)
	if err != nil {
		utils.LogError(g.logger, err, "failed to search for similar code")
		return nil, err
	}

	return results, nil
}

func (g *UnitTestGenerator) Cleanup() {
	if g.fileWatcher != nil {
		g.fileWatcher.Stop()
	}
}
