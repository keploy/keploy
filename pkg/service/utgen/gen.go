// Package utgen is a service that generates unit tests for a given source code file.
package utgen

import (
	"bufio"
	"context"
	"fmt"
	"io/fs"
	"math"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"

	"github.com/google/uuid"
	"github.com/k0kubun/pp/v3"
	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/pkg/service"
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
	maxIterations    int
	Files            []string
	tel              Telemetry
	additionalPrompt string
	totalTestCase    int
	testCasePassed   int
	testCaseFailed   int
	noCoverageTest   int
}

func NewUnitTestGenerator(
	cfg *config.Config,
	tel Telemetry,
	auth service.Auth,
	logger *zap.Logger,
) (*UnitTestGenerator, error) {
	genConfig := cfg.Gen

	generator := &UnitTestGenerator{
		srcPath:       genConfig.SourceFilePath,
		testPath:      genConfig.TestFilePath,
		cmd:           genConfig.TestCommand,
		dir:           genConfig.TestDir,
		maxIterations: genConfig.MaxIterations,
		logger:        logger,
		tel:           tel,
		ai:            NewAIClient(genConfig.Model, genConfig.APIBaseURL, genConfig.APIVersion, "", cfg.APIServerURL, auth, uuid.NewString(), logger),
		cov: &Coverage{
			Path:    genConfig.CoverageReportPath,
			Format:  genConfig.CoverageFormat,
			Desired: genConfig.DesiredCoverage,
		},
		additionalPrompt: genConfig.AdditionalPrompt,
		cur:              &Cursor{},
	}
	return generator, nil
}

func updateJavaScriptImports(importedContent string, newImports []string) (string, int, error) {
	importRegex := regexp.MustCompile(`(?m)^(import\s+.*?from\s+['"].*?['"];?|const\s+.*?=\s+require\(['"].*?['"]\);?)`)
	existingImportsSet := make(map[string]bool)

	existingImports := importRegex.FindAllString(importedContent, -1)
	for _, imp := range existingImports {
		if imp != "\"\"" && len(imp) > 0 {
			existingImportsSet[imp] = true
		}
	}

	for _, imp := range newImports {
		imp = strings.TrimSpace(imp)
		if imp != "\"\"" && len(imp) > 0 {
			existingImportsSet[imp] = true
		}
	}

	allImports := make([]string, 0, len(existingImportsSet))
	for imp := range existingImportsSet {
		allImports = append(allImports, imp)
	}

	importSection := strings.Join(allImports, "\n")

	updatedContent := importRegex.ReplaceAllString(importedContent, "")
	updatedContent = strings.Trim(updatedContent, "\n")
	lines := strings.Split(updatedContent, "\n")
	cleanedLines := []string{}
	for _, line := range lines {
		trimmedLine := strings.TrimSpace(line)
		if trimmedLine != "" {
			cleanedLines = append(cleanedLines, line)
		}
	}
	updatedContent = strings.Join(cleanedLines, "\n")
	updatedContent = importSection + "\n" + updatedContent

	importLength := len(strings.Split(updatedContent, "\n")) - len(strings.Split(importedContent, "\n"))
	if importLength < 0 {
		importLength = 0
	}
	return updatedContent, importLength, nil
}

func updateImports(filePath string, language string, imports string) (int, error) {
	newImports := strings.Split(imports, "\n")
	for i, imp := range newImports {
		newImports[i] = strings.TrimSpace(imp)
	}
	contentBytes, err := os.ReadFile(filePath)
	if err != nil {
		return 0, err
	}
	content := string(contentBytes)

	var updatedContent string
	var importLength int
	switch strings.ToLower(language) {
	case "go":
		updatedContent, importLength, err = updateGoImports(content, newImports)
	case "java":
		updatedContent, err = updateJavaImports(content, newImports)
	case "python":
		updatedContent, err = updatePythonImports(content, newImports)
	case "typescript":
		updatedContent, importLength, err = updateTypeScriptImports(content, newImports)
	case "javascript":
		updatedContent, importLength, err = updateJavaScriptImports(content, newImports)
	default:
		return 0, fmt.Errorf("unsupported language: %s", language)
	}
	if err != nil {
		return 0, err
	}
	err = os.WriteFile(filePath, []byte(updatedContent), fs.ModePerm)

	if err != nil {
		return 0, err
	}

	return importLength, nil
}

func updateGoImports(codeBlock string, newImports []string) (string, int, error) {
	importRegex := regexp.MustCompile(`(?ms)import\s*(\([\s\S]*?\)|"[^"]+")`)
	existingImportsSet := make(map[string]bool)

	matches := importRegex.FindStringSubmatch(codeBlock)
	if matches != nil {
		importBlock := matches[0]
		importLines := strings.Split(importBlock, "\n")
		existingImports := extractGoImports(importLines)
		for _, imp := range existingImports {
			existingImportsSet[imp] = true
		}
		newImports = extractGoImports(newImports)
		for _, imp := range newImports {
			imp = strings.TrimSpace(imp)
			if imp != "\"\"" && len(imp) > 0 {
				existingImportsSet[imp] = true
			}
		}
		allImports := make([]string, 0, len(existingImportsSet))
		for imp := range existingImportsSet {
			allImports = append(allImports, imp)
		}
		importBlockNew := createGoImportBlock(allImports)
		updatedContent := importRegex.ReplaceAllString(codeBlock, importBlockNew)
		return updatedContent, len(strings.Split(importBlockNew, "\n")) - len(importLines), nil
	}
	packageRegex := regexp.MustCompile(`package\s+\w+`)

	pkgMatch := packageRegex.FindStringIndex(codeBlock)
	if pkgMatch == nil {
		return "", 0, fmt.Errorf("could not find package declaration")
	}
	newImports = extractGoImports(newImports)
	importBlock := createGoImportBlock(newImports)

	insertPos := pkgMatch[1]
	updatedContent := codeBlock[:insertPos] + "\n\n" + importBlock + "\n" + codeBlock[insertPos:]
	return updatedContent, len(strings.Split(importBlock, "\n")) + 1, nil

}

func extractGoImports(importLines []string) []string {
	imports := []string{}
	for _, line := range importLines {
		line = strings.TrimSpace(line)
		if (line == "import (" || line == ")" || line == "") || len(line) == 0 {
			continue
		}
		line = strings.TrimPrefix(line, "import ")
		line = strings.Trim(line, `"`)
		imports = append(imports, line)
	}
	return imports
}

func createGoImportBlock(imports []string) string {
	importBlock := "import (\n"
	for _, imp := range imports {
		imp = strings.TrimSpace(imp)
		imp = strings.Trim(imp, `"`)
		importBlock += fmt.Sprintf(`    "%s"`+"\n", imp)
	}
	importBlock += ")"
	return importBlock
}

func updateJavaImports(content string, newImports []string) (string, error) {
	importRegex := regexp.MustCompile(`(?m)^import\s+.*?;`)
	existingImportsSet := make(map[string]bool)

	existingImports := importRegex.FindAllString(content, -1)
	for _, imp := range existingImports {
		existingImportsSet[imp] = true
	}

	for _, imp := range newImports {
		imp = strings.TrimSpace(imp)
		if imp != "\"\"" && len(imp) > 0 {
			importStatement := fmt.Sprintf("import %s;", imp)
			existingImportsSet[importStatement] = true
		}
	}

	allImports := make([]string, 0, len(existingImportsSet))
	for imp := range existingImportsSet {
		allImports = append(allImports, imp)
	}
	importSection := strings.Join(allImports, "\n")

	updatedContent := importRegex.ReplaceAllString(content, "")
	packageRegex := regexp.MustCompile(`(?m)^package\s+.*?;`)
	pkgMatch := packageRegex.FindStringIndex(updatedContent)
	insertPos := 0
	if pkgMatch != nil {
		insertPos = pkgMatch[1]
	}

	updatedContent = updatedContent[:insertPos] + "\n\n" + importSection + "\n" + updatedContent[insertPos:]
	return updatedContent, nil
}

func updatePythonImports(content string, newImports []string) (string, error) {
	scanner := bufio.NewScanner(strings.NewReader(content))
	existingImportsMap := make(map[string]map[string]bool)
	codeLines := []string{}
	importLines := []string{}

	ignoredPrefixes := "# checking coverage for file - do not remove"

	for scanner.Scan() {
		line := scanner.Text()
		trimmedLine := strings.TrimSpace(line)

		if trimmedLine == "" {
			continue
		}
		shouldIgnore := (strings.HasPrefix(trimmedLine, "import ") || strings.HasPrefix(trimmedLine, "from ")) && strings.Contains(trimmedLine, ignoredPrefixes)
		if shouldIgnore {
			parts := strings.Split(trimmedLine, "#")
			coreImport := strings.TrimSpace(parts[0])

			if strings.HasPrefix(coreImport, "from ") {
				fields := strings.Fields(coreImport)
				moduleName := fields[1]
				importPart := coreImport[strings.Index(coreImport, "import")+len("import "):]
				importedItems := strings.Split(importPart, ",")

				if _, exists := existingImportsMap[moduleName]; !exists {
					existingImportsMap[moduleName] = make(map[string]bool)
				}
				for _, item := range importedItems {
					cleanedItem := strings.TrimSpace(item)
					if cleanedItem != "" {
						existingImportsMap[moduleName][cleanedItem] = true
					}
				}
			}
			codeLines = append(codeLines, line)
			continue
		}

		if strings.HasPrefix(trimmedLine, "import ") || strings.HasPrefix(trimmedLine, "from ") {
			codeLines = append(codeLines, line)
		} else {
			codeLines = append(codeLines, line)
		}
	}

	for _, imp := range newImports {
		imp = strings.TrimSpace(imp)
		if imp == "\"\"" || len(imp) == 0 {
			continue
		}
		if strings.HasPrefix(imp, "from ") {
			fields := strings.Fields(imp)
			moduleName := fields[1]
			newItems := strings.Split(fields[3], ",")
			if _, exists := existingImportsMap[moduleName]; !exists {
				existingImportsMap[moduleName] = make(map[string]bool)
			}
			for _, item := range newItems {
				existingImportsMap[moduleName][strings.TrimSpace(item)] = true
			}
		} else if strings.HasPrefix(imp, "import ") {
			fields := strings.Fields(imp)
			moduleName := fields[1]
			if _, exists := existingImportsMap[moduleName]; !exists {
				existingImportsMap[moduleName] = make(map[string]bool)
			}
		}
	}
	for i, line := range codeLines {
		trimmedLine := strings.TrimSpace(line)

		if strings.HasPrefix(trimmedLine, "from ") {
			fields := strings.Fields(trimmedLine)
			moduleName := fields[1]

			if itemsMap, exists := existingImportsMap[moduleName]; exists && len(itemsMap) > 0 {
				items := mapKeysToSortedSlice(itemsMap)
				importLine := fmt.Sprintf("from %s import %s", moduleName, strings.Join(items, ", "))

				if strings.Contains(trimmedLine, ignoredPrefixes) {
					importLine += " " + ignoredPrefixes
				}
				codeLines[i] = importLine
				delete(existingImportsMap, moduleName)
			}
		}
	}

	for module, itemsMap := range existingImportsMap {
		if len(itemsMap) > 0 {
			items := mapKeysToSortedSlice(itemsMap)
			importLine := fmt.Sprintf("from %s import %s", module, strings.Join(items, ", "))
			importLine += " " + ignoredPrefixes
			importLines = append(importLines, importLine)
		}
	}
	nonEmptyCodeLines := []string{}
	for _, line := range codeLines {
		if strings.TrimSpace(line) != "" {
			nonEmptyCodeLines = append(nonEmptyCodeLines, line)
		}
	}

	nonEmptyImportLines := []string{}
	for _, line := range importLines {
		if strings.TrimSpace(line) != "" {
			nonEmptyImportLines = append(nonEmptyImportLines, line)
		}
	}

	updatedContent := strings.Join(nonEmptyImportLines, "\n") + "\n" + strings.Join(nonEmptyCodeLines, "\n")
	return updatedContent, nil
}

// Helper function to convert map keys to a sorted slice
func mapKeysToSortedSlice(itemsMap map[string]bool) []string {
	items := []string{}
	for item := range itemsMap {
		items = append(items, item)
	}
	sort.Strings(items)
	return items
}

func updateTypeScriptImports(importedContent string, newImports []string) (string, int, error) {
	importRegex := regexp.MustCompile(`(?m)^import\s+.*?;`)
	existingImportsSet := make(map[string]bool)

	existingImports := importRegex.FindAllString(importedContent, -1)
	for _, imp := range existingImports {
		existingImportsSet[imp] = true
	}

	for _, imp := range newImports {
		imp = strings.TrimSpace(imp)
		if imp != "\"\"" && len(imp) > 0 {
			existingImportsSet[imp] = true
		}
	}

	allImports := make([]string, 0, len(existingImportsSet))
	for imp := range existingImportsSet {
		allImports = append(allImports, imp)
	}
	importSection := strings.Join(allImports, "\n")

	updatedContent := importRegex.ReplaceAllString(importedContent, "")
	updatedContent = strings.Trim(updatedContent, "\n")
	lines := strings.Split(updatedContent, "\n")
	cleanedLines := []string{}
	for _, line := range lines {
		trimmedLine := strings.TrimSpace(line)
		if trimmedLine != "" {
			cleanedLines = append(cleanedLines, line)
		}
	}
	updatedContent = strings.Join(cleanedLines, "\n")
	updatedContent = importSection + "\n" + updatedContent
	importLength := len(strings.Split(updatedContent, "\n")) - len(strings.Split(importedContent, "\n"))
	if importLength < 0 {
		importLength = 0
	}
	return updatedContent, importLength, nil
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

		iterationCount := 0
		g.lang = GetCodeLanguage(g.srcPath)

		g.promptBuilder, err = NewPromptBuilder(g.srcPath, g.testPath, g.cov.Content, "", "", g.lang, g.additionalPrompt, g.logger)
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

		// Respect context cancellation in the inner loop
		for g.cov.Current < (g.cov.Desired/100) && iterationCount < g.maxIterations {
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

			g.promptBuilder.InstalledPackages, err = libraryInstalled(g.logger, g.lang)
			if err != nil {
				utils.LogError(g.logger, err, "Error getting installed packages")
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
			g.totalTestCase += len(testsDetails.NewTests)
			totalTest = len(testsDetails.NewTests)
			for _, generatedTest := range testsDetails.NewTests {
				installedPackages, err := libraryInstalled(g.logger, g.lang)
				if err != nil {
					g.logger.Warn("Error getting installed packages", zap.Error(err))
				}
				select {
				case <-ctx.Done():
					return fmt.Errorf("process cancelled by user")
				default:
				}
				err = g.ValidateTest(generatedTest, &passedTests, &noCoverageTest, &failedBuild, installedPackages)
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
		} else if iterationCount == g.maxIterations {
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

func (g *UnitTestGenerator) runCoverage() error {
	// Perform an initial build/test command to generate coverage report and get a baseline
	if g.srcPath != "" {
		g.logger.Info(fmt.Sprintf("Running test command to generate coverage report: '%s'", g.cmd))
	}
	_, _, exitCode, lastUpdatedTime, err := RunCommand(g.cmd, g.dir, g.logger)
	if err != nil {
		utils.LogError(g.logger, err, "Error running test command")
		return fmt.Errorf("error running test command: %w", err)
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
	fmt.Println("Generating Tests...")

	select {
	case <-ctx.Done():
		err := ctx.Err()
		return &models.UTDetails{}, err
	default:
	}

	response, promptTokenCount, responseTokenCount, err := g.ai.Call(ctx, g.prompt, 4096)
	if err != nil {
		return &models.UTDetails{}, err
	}

	select {
	case <-ctx.Done():
		err := ctx.Err()
		return &models.UTDetails{}, err
	default:
	}

	g.logger.Info(fmt.Sprintf("Total token used count for LLM model %s: %d", g.ai.Model, promptTokenCount+responseTokenCount))

	select {
	case <-ctx.Done():
		err := ctx.Err()
		return &models.UTDetails{}, err
	default:
	}

	testsDetails, err := unmarshalYamlTestDetails(response)
	if err != nil {
		utils.LogError(g.logger, err, "Error unmarshalling test details")
		return &models.UTDetails{}, err
	}

	select {
	case <-ctx.Done():
		err := ctx.Err()
		return &models.UTDetails{}, err
	default:
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

func (g *UnitTestGenerator) ValidateTest(generatedTest models.UT, passedTests, noCoverageTest, failedBuild *int, installedPackages []string) error {
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
	if InsertAfter > len(originalContentLines) {
		InsertAfter = len(originalContentLines)
	}
	processedTestLines := append(originalContentLines[:InsertAfter], testCodeLines...)
	processedTestLines = append(processedTestLines, originalContentLines[InsertAfter:]...)
	processedTest := strings.Join(processedTestLines, "\n")
	if err := os.WriteFile(g.testPath, []byte(processedTest), 0644); err != nil {
		return fmt.Errorf("failed to write test file: %w", err)
	}

	newInstalledPackages, err := installLibraries(generatedTest.LibraryInstallationCode, installedPackages, g.logger)
	if err != nil {
		g.logger.Debug("Error installing libraries", zap.Error(err))
	}

	// Run the test using the Runner class
	g.logger.Info(fmt.Sprintf("Running test 5 times for proper validation with the following command: '%s'", g.cmd))

	var testCommandStartTime int64
	importLen, err := updateImports(g.testPath, g.lang, generatedTest.NewImportsCode)
	if err != nil {
		g.logger.Warn("Error updating imports", zap.Error(err))
	}
	for i := 0; i < 5; i++ {

		g.logger.Info(fmt.Sprintf("Iteration no: %d", i+1))

		stdout, _, exitCode, timeOfTestCommand, _ := RunCommand(g.cmd, g.dir, g.logger)
		if exitCode != 0 {
			g.logger.Info(fmt.Sprintf("Test failed in %d iteration", i+1))
			// Test failed, roll back the test file to its original content

			if err := os.Truncate(g.testPath, 0); err != nil {
				return fmt.Errorf("failed to truncate test file: %w", err)
			}

			if err := os.WriteFile(g.testPath, []byte(originalContent), 0644); err != nil {
				return fmt.Errorf("failed to write test file: %w", err)
			}
			err = uninstallLibraries(g.lang, newInstalledPackages, g.logger)

			if err != nil {
				g.logger.Warn("Error uninstalling libraries", zap.Error(err))
			}
			g.logger.Info("Skipping a generated test that failed")
			g.failedTests = append(g.failedTests, &models.FailedUT{
				TestCode: generatedTest.TestCode,
				ErrorMsg: extractErrorMessage(stdout),
			})
			g.testCaseFailed++
			*failedBuild++
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
		g.noCoverageTest++
		*noCoverageTest++
		// Test failed to increase coverage, roll back the test file to its original content

		if err := os.Truncate(g.testPath, 0); err != nil {
			return fmt.Errorf("failed to truncate test file: %w", err)
		}

		if err := os.WriteFile(g.testPath, []byte(originalContent), 0644); err != nil {
			return fmt.Errorf("failed to write test file: %w", err)
		}

		err = uninstallLibraries(g.lang, newInstalledPackages, g.logger)

		if err != nil {
			g.logger.Warn("Error uninstalling libraries", zap.Error(err))
		}

		g.logger.Info("Skipping a generated test that failed to increase coverage")
		return nil
	}
	g.testCasePassed++
	*passedTests++
	g.cov.Current = covResult.Coverage
	g.cur.Line = g.cur.Line + len(testCodeLines) + importLen
	g.cur.Line = g.cur.Line + len(testCodeLines)
	g.logger.Info("Generated test passed and increased coverage")
	return nil
}

func libraryInstalled(logger *zap.Logger, language string) ([]string, error) {
	switch strings.ToLower(language) {
	case "go":
		out, err := exec.Command("go", "list", "-m", "all").Output()
		if err != nil {
			return nil, fmt.Errorf("failed to get Go dependencies: %w", err)
		}
		return extractDependencies(out), nil

	case "java":
		out, err := exec.Command("mvn", "dependency:list", "-DincludeScope=compile", "-Dstyle.color=never", "-B").Output()
		if err != nil {
			return nil, fmt.Errorf("failed to get Java dependencies: %w", err)
		}
		return extractJavaDependencies(out), nil

	case "python":
		out, err := exec.Command("pip", "freeze").Output()
		if err != nil {
			logger.Info("Error getting Python dependencies with `pip` command, trying `pip3` command")
			out, err = exec.Command("pip3", "freeze").Output()
			if err != nil {
				return nil, fmt.Errorf("failed to get Python dependencies: %w", err)
			}
		}

		return extractDependencies(out), nil

	case "typescript", "javascript":
		cmd := exec.Command("sh", "-c", "npm list --depth=0 --parseable | sed 's|.*/||'")
		out, err := cmd.Output()
		if err != nil {
			return nil, fmt.Errorf("failed to get JavaScript/TypeScript dependencies: %w", err)
		}
		return extractDependencies(out), nil

	default:
		return nil, fmt.Errorf("unsupported language: %s", language)
	}
}

func extractJavaDependencies(output []byte) []string {
	lines := strings.Split(string(output), "\n")
	var dependencies []string
	inDependencySection := false

	depRegex := regexp.MustCompile(`^\[INFO\]\s+[\+\|\\\-]{1,2}\s+([\w\.\-]+:[\w\.\-]+):jar:([\w\.\-]+):([\w\.\-]+)`)

	for _, line := range lines {
		cleanedLine := strings.TrimSpace(line)
		if strings.HasPrefix(cleanedLine, "[INFO]") {
			cleanedLine = "[INFO]" + strings.TrimSpace(cleanedLine[6:])
		}
		if strings.Contains(cleanedLine, "maven-dependency-plugin") && strings.Contains(cleanedLine, ":list") {
			inDependencySection = true
			continue
		}

		if inDependencySection && (strings.Contains(cleanedLine, "BUILD SUCCESS") || strings.Contains(cleanedLine, "---")) {
			inDependencySection = false
			continue
		}

		if inDependencySection && strings.HasPrefix(cleanedLine, "[INFO]") {
			matches := depRegex.FindStringSubmatch(cleanedLine)
			if len(matches) >= 4 {
				groupArtifact := matches[1]
				version := matches[2]
				dep := fmt.Sprintf("%s:%s", groupArtifact, version)
				dependencies = append(dependencies, dep)
			} else {
				cleanedLine = strings.TrimPrefix(cleanedLine, "[INFO]")
				cleanedLine = strings.TrimSpace(cleanedLine)

				cleanedLine = strings.TrimPrefix(cleanedLine, "+-")
				cleanedLine = strings.TrimPrefix(cleanedLine, "\\-")
				cleanedLine = strings.TrimPrefix(cleanedLine, "|")

				cleanedLine = strings.TrimSpace(cleanedLine)

				depParts := strings.Split(cleanedLine, ":")
				if len(depParts) >= 5 {
					dep := fmt.Sprintf("%s:%s:%s", depParts[0], depParts[1], depParts[3])
					dependencies = append(dependencies, dep)
				}
			}
		}
	}
	return dependencies
}

func extractDependencies(output []byte) []string {
	lines := strings.Split(string(output), "\n")
	var dependencies []string
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed != "" {
			dependencies = append(dependencies, trimmed)
		}
	}
	return dependencies
}

func isPackageInList(installedPackages []string, packageName string) bool {
	for _, pkg := range installedPackages {
		if pkg == packageName {
			return true
		}
	}
	return false
}

func installLibraries(libraryCommands string, installedPackages []string, logger *zap.Logger) ([]string, error) {
	var newInstalledPackages []string
	libraryCommands = strings.TrimSpace(libraryCommands)
	if libraryCommands == "" || libraryCommands == "\"\"" {
		return newInstalledPackages, nil
	}

	commands := strings.Split(libraryCommands, "\n")
	for _, command := range commands {
		packageName := extractPackageName(command)

		if isPackageInList(installedPackages, packageName) {
			continue
		}

		_, _, exitCode, _, err := RunCommand(command, "", logger)
		if exitCode != 0 || err != nil {
			return newInstalledPackages, fmt.Errorf("failed to install library: %s", command)
		}

		installedPackages = append(installedPackages, packageName)
		newInstalledPackages = append(newInstalledPackages, packageName)
	}
	return newInstalledPackages, nil
}

func extractPackageName(command string) string {
	fields := strings.Fields(command)
	if len(fields) < 3 {
		return ""
	}
	return fields[2]
}

func uninstallLibraries(language string, installedPackages []string, logger *zap.Logger) error {
	for _, command := range installedPackages {
		logger.Info(fmt.Sprintf("Uninstalling library: %s", command))

		var uninstallCommand string
		switch strings.ToLower(language) {
		case "go":
			uninstallCommand = fmt.Sprintf("go mod edit -droprequire %s && go mod tidy", command)
		case "python":
			uninstallCommand = fmt.Sprintf("pip uninstall -y %s", command)
		case "javascript":
			uninstallCommand = fmt.Sprintf("npm uninstall %s", command)
		case "java":
			uninstallCommand = fmt.Sprintf("mvn dependency:purge-local-repository -DreResolve=false -Dinclude=%s", command)
		}
		if uninstallCommand != "" {
			logger.Info(fmt.Sprintf("Uninstalling library with command: %s", uninstallCommand))
			_, _, exitCode, _, err := RunCommand(uninstallCommand, "", logger)
			if exitCode != 0 || err != nil {
				logger.Warn(fmt.Sprintf("Failed to uninstall library: %s", uninstallCommand), zap.Error(err))
			}
		}
	}
	return nil
}
