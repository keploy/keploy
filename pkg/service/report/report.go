package report

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/invopop/yaml"
	"github.com/k0kubun/pp/v3"
	"go.keploy.io/server/v2/config"
	matcherUtils "go.keploy.io/server/v2/pkg/matcher"
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/pkg/service/tools"
	"go.uber.org/zap"
)

type Report struct {
	logger   *zap.Logger
	config   *config.Config
	reportDB ReportDB
	testDB   TestDB
}

type fileReport struct {
	Tests []models.TestResult `yaml:"tests" json:"tests"`
}

const (
	ReportSuffix  = "-report"
	TestRunPrefix = "test-run-"
)

func New(logger *zap.Logger, cfg *config.Config, reportDB ReportDB, testDB TestDB) *Report {
	return &Report{
		logger:   logger,
		config:   cfg,
		reportDB: reportDB,
		testDB:   testDB,
	}
}

// GenerateReport orchestrates the entire report generation process
func (r *Report) GenerateReport(ctx context.Context) error {
	if r.config.Report.ReportPath != "" {
		return r.generateReportFromFile(r.config.Report.ReportPath)
	}

	latestRunID, err := r.getLatestTestRunID(ctx)

	if err != nil {
		return err
	}

	testSetIDs := r.extractTestSetIDs()
	if len(testSetIDs) == 0 {
		r.logger.Info("No test sets selected for report generation, Generating report for all test sets")

		var err error
		testSetIDs, err = r.testDB.GetReportTestSets(ctx, latestRunID)
		if err != nil {
			r.logger.Error("failed to get all test set ids", zap.Error(err))
			return err
		}

		if len(testSetIDs) == 0 {
			r.logger.Warn("No test sets found for report generation")
			return nil
		}
	}

	if latestRunID == "" {
		r.logger.Warn("no test runs found")
		return nil
	}

	r.logger.Debug("latest run id is", zap.String("latest_run_id", latestRunID))

	failedTests, err := r.collectFailedTests(ctx, latestRunID, testSetIDs)
	if err != nil {
		return err
	}

	if len(failedTests) == 0 {
		r.logger.Info("No failed tests found in the latest test run")
		return nil
	}

	err = r.printFailedTestReports(failedTests)
	if err != nil {
		r.logger.Error("failed to print failed test reports", zap.Error(err))
		return err
	}

	r.logger.Info(fmt.Sprintf("✂️ CLI output truncated - see the %s report file for the complete diff.", latestRunID))

	r.logger.Info("Report generation completed successfully")

	return nil
}

// generateReportFromFile loads a report from an absolute file path and prints diffs for failed tests.
func (r *Report) generateReportFromFile(reportPath string) error {
	if !filepath.IsAbs(reportPath) {
		// Should be enforced in CLI validation; keep a guard here for safety.
		return fmt.Errorf("report-path must be absolute, got %q", reportPath)
	}

	data, err := os.ReadFile(reportPath)
	if err != nil {
		r.logger.Error("failed to read report file", zap.String("report_path", reportPath), zap.Error(err))
		return err
	}
	r.logger.Info("Generating report from file", zap.String("report_path", reportPath))

	tests, err := r.parseReportTests(data)
	if err != nil {
		r.logger.Error("failed to parse report file", zap.String("report_path", reportPath), zap.Error(err))
		return err
	}

	failed := r.extractFailedTestsFromResults(tests)
	if len(failed) == 0 {
		r.logger.Info("No failed tests found in the provided report file")
		return nil
	}

	if err := r.printFailedTestReports(failed); err != nil {
		r.logger.Error("failed to print failed test reports from file", zap.Error(err))
		return err
	}

	r.logger.Info("Report generation (from file) completed successfully")
	return nil
}

// parseReportTests unmarshals the report data into a fileReport struct.
func (r *Report) parseReportTests(data []byte) ([]models.TestResult, error) {
	var fr fileReport
	if err := yaml.Unmarshal(data, &fr); err != nil {
		return nil, fmt.Errorf("failed to unmarshal report: %w", err)
	}
	if len(fr.Tests) == 0 {
		return nil, fmt.Errorf("invalid report: 'tests' is missing or empty (expected top-level 'tests' array)")
	}
	return fr.Tests, nil
}

// extractTestSetIDs extracts and cleans test set IDs from config
func (r *Report) extractTestSetIDs() []string {
	var testSetIDs []string
	for testSet := range r.config.Report.SelectedTestSets {
		testSetIDs = append(testSetIDs, strings.TrimSpace(testSet))
	}
	return testSetIDs
}

// getLatestTestRunID retrieves and determines the latest test run ID
func (r *Report) getLatestTestRunID(ctx context.Context) (string, error) {
	testRunIDs, err := r.reportDB.GetAllTestRunIDs(ctx)
	if err != nil {
		r.logger.Error("failed to get all test run ids", zap.Error(err))
		return "", err
	}

	if len(testRunIDs) == 0 {
		return "", nil
	}

	sort.Slice(testRunIDs, func(i, j int) bool {
		numi, erri := strconv.Atoi(strings.TrimPrefix(testRunIDs[i], TestRunPrefix))
		numj, errj := strconv.Atoi(strings.TrimPrefix(testRunIDs[j], TestRunPrefix))
		if erri != nil && errj != nil {
			return testRunIDs[i] < testRunIDs[j]
		}
		if erri != nil {
			return true // i is less if it can't be parsed
		}
		if errj != nil {
			return false // j is less if it can't be parsed
		}
		return numi < numj
	})

	return testRunIDs[len(testRunIDs)-1], nil
}

// collectFailedTests gathers all failed tests from the specified test sets
func (r *Report) collectFailedTests(ctx context.Context, runID string, testSetIDs []string) ([]models.TestResult, error) {
	var failedTests []models.TestResult

	for _, testSetID := range testSetIDs {
		cleanTestSetID := strings.TrimSuffix(testSetID, ReportSuffix)

		results, err := r.reportDB.GetReport(ctx, runID, cleanTestSetID)
		if err != nil {
			r.logger.Error("failed to get test case results for test set",
				zap.String("test_set_id", cleanTestSetID), zap.Error(err))
			continue
		}

		if results == nil {
			r.logger.Warn("no results found for test set", zap.String("test_set_id", cleanTestSetID))
			continue
		}

		failedTests = append(failedTests, r.extractFailedTestsFromResults(results.Tests)...)
	}

	return failedTests, nil
}

// extractFailedTestsFromResults filters out only the failed tests from results
func (r *Report) extractFailedTestsFromResults(tests []models.TestResult) []models.TestResult {
	var failedTests []models.TestResult
	for _, result := range tests {
		if result.Status == models.TestStatusFailed {
			failedTests = append(failedTests, result)
		}
	}
	return failedTests
}

// printFailedTestReports generates and prints reports for all failed tests
func (r *Report) printFailedTestReports(failedTests []models.TestResult) error {
	for _, test := range failedTests {
		if err := r.printSingleTestReport(test); err != nil {
			return err
		}
	}
	return nil
}

// printSingleTestReport generates and prints a report for a single failed test
func (r *Report) printSingleTestReport(test models.TestResult) error {
	// If full-body mode is ON, use the original pipeline (entire expected/actual bodies)
	if r.config.Report.ShowFullBody {
		logDiffs := matcherUtils.NewDiffsPrinter(test.Name)
		printer := r.createFormattedPrinter()
		logs := r.generateTestHeader(test, printer)

		if err := r.addStatusCodeDiffs(test, &logDiffs); err != nil {
			return err
		}
		if err := r.addHeaderDiffs(test, &logDiffs); err != nil {
			return err
		}
		if err := r.addBodyDiffs(test, &logDiffs); err != nil {
			return err
		}
		if err := r.printAndRenderDiffs(printer, logs, &logDiffs); err != nil {
			return err
		}
		fmt.Println("\n--------------------------------------------------------------------")
		return nil
	}

	printer := r.createFormattedPrinter()

	// Print test case header
	header := r.generateTestHeader(test, printer)
	if _, err := printer.Printf(header); err != nil {
		r.logger.Error("failed to print test header", zap.Error(err))
		return err
	}

	// Print status code and header diffs in the same table-style format
	metaDiff := GenerateStatusAndHeadersTableDiff(test)
	fmt.Println(applyCliColorsToDiff(metaDiff))
	fmt.Println()

	// Print body diffs using the new method for JSON
	for _, bodyResult := range test.Result.BodyResult {
		if !bodyResult.Normal {
			if strings.EqualFold(string(bodyResult.Type), "JSON") {
				diff, err := GenerateTableDiff(bodyResult.Expected, bodyResult.Actual)
				if err != nil {
					r.logger.Warn("failed to generate table view for JSON diff, falling back to default diff", zap.Error(err))
					if err := r.printDefaultBodyDiff(bodyResult); err != nil {
						r.logger.Error("failed to print default body diff", zap.Error(err))
					}
				} else {
					fmt.Println(applyCliColorsToDiff(diff))
				}
			} else {
				r.logger.Info("Non-JSON body mismatch found, using default diff.", zap.String("type", string(bodyResult.Type)))
				if err := r.printDefaultBodyDiff(bodyResult); err != nil {
					r.logger.Error("failed to print default body diff for non-json type", zap.Error(err))
				}
			}
		}
	}
	fmt.Println("\n--------------------------------------------------------------------")
	return nil
}

// printDefaultBodyDiff renders a generic diff for a single failed body result.
func (r *Report) printDefaultBodyDiff(bodyResult models.BodyResult) error {
	logDiffs := matcherUtils.NewDiffsPrinter("")

	actualValue, err := r.renderTemplateValue(bodyResult.Actual)
	if err != nil {
		return fmt.Errorf("failed to render actual body: %w", err)
	}

	expectedValue, err := r.renderTemplateValue(bodyResult.Expected)
	if err != nil {
		return fmt.Errorf("failed to render expected body: %w", err)
	}

	logDiffs.PushBodyDiff(fmt.Sprint(expectedValue), fmt.Sprint(actualValue), nil)

	if err := logDiffs.Render(); err != nil {
		r.logger.Error("failed to render the default body diffs", zap.Error(err))
		return err
	}
	return nil
}

// createFormattedPrinter creates a configured pretty printer
func (r *Report) createFormattedPrinter() *pp.PrettyPrinter {
	printer := pp.New()
	printer.WithLineInfo = false
	printer.SetColorScheme(models.GetFailingColorScheme())
	return printer
}

// generateTestHeader creates the test report header
func (r *Report) generateTestHeader(test models.TestResult, printer *pp.PrettyPrinter) string {
	return printer.Sprintf("Testrun failed for testcase with id: %s\n\n--------------------------------------------------------------------\n\n",
		test.TestCaseID)
}

// addStatusCodeDiffs adds status code differences to the diff printer
func (r *Report) addStatusCodeDiffs(test models.TestResult, logDiffs *matcherUtils.DiffsPrinter) error {
	if !test.Result.StatusCode.Normal {
		logDiffs.PushStatusDiff(
			fmt.Sprint(test.Result.StatusCode.Expected),
			fmt.Sprint(test.Result.StatusCode.Actual),
		)
	}
	return nil
}

// addHeaderDiffs adds header differences to the diff printer
func (r *Report) addHeaderDiffs(test models.TestResult, logDiffs *matcherUtils.DiffsPrinter) error {
	for _, headerResult := range test.Result.HeadersResult {
		if !headerResult.Normal {
			actualValue := strings.Join(headerResult.Actual.Value, ", ")
			expectedValue := strings.Join(headerResult.Expected.Value, ", ")
			logDiffs.PushHeaderDiff(expectedValue, actualValue, headerResult.Actual.Key, nil)
		}
	}
	return nil
}

// addBodyDiffs adds body differences to the diff printer
func (r *Report) addBodyDiffs(test models.TestResult, logDiffs *matcherUtils.DiffsPrinter) error {
	for _, bodyResult := range test.Result.BodyResult {
		if !bodyResult.Normal {
			actualValue, err := r.renderTemplateValue(bodyResult.Actual)
			if err != nil {
				return fmt.Errorf("failed to render actual body: %w", err)
			}

			expectedValue, err := r.renderTemplateValue(bodyResult.Expected)
			if err != nil {
				return fmt.Errorf("failed to render expected body: %w", err)
			}

			logDiffs.PushBodyDiff(fmt.Sprint(expectedValue), fmt.Sprint(actualValue), nil)
		}
	}
	return nil
}

// renderTemplateValue renders a templated value and returns the result
func (r *Report) renderTemplateValue(value interface{}) (interface{}, error) {
	_, renderedValue, err := tools.RenderIfTemplatized(value)
	if err != nil {
		r.logger.Error("failed to render template value", zap.Error(err))
		return nil, err
	}
	return renderedValue, nil
}

// printAndRenderDiffs prints the logs and renders the differences
func (r *Report) printAndRenderDiffs(printer *pp.PrettyPrinter, logs string, logDiffs *matcherUtils.DiffsPrinter) error {
	if _, err := printer.Printf(logs); err != nil {
		r.logger.Error("failed to print the logs", zap.Error(err))
		return err
	}

	if err := logDiffs.Render(); err != nil {
		r.logger.Error("failed to render the diffs", zap.Error(err))
		return err
	}

	return nil
}

// applyCliColorsToDiff adds ANSI colors to values in the JSON diff block.
// - Value after "Path:" is yellow
// - Value after "Old:" is red
// - Value after "New:" is green
func applyCliColorsToDiff(diff string) string {
	const (
		ansiReset  = "\x1b[0m"
		ansiYellow = "\x1b[33m"
		ansiRed    = "\x1b[31m"
		ansiGreen  = "\x1b[32m"
	)

	lines := strings.Split(diff, "\n")
	for i, line := range lines {
		if strings.HasPrefix(line, "Path: ") {
			// Color only the value after "Path: " in yellow
			value := strings.TrimPrefix(line, "Path: ")
			lines[i] = "Path: " + ansiYellow + value + ansiReset
			continue
		}
		if strings.HasPrefix(line, "  Old: ") {
			// Color only the value after "  Old: " in red
			value := strings.TrimPrefix(line, "  Old: ")
			lines[i] = "  Old: " + ansiRed + value + ansiReset
			continue
		}
		if strings.HasPrefix(line, "  New: ") {
			// Color only the value after "  New: " in green
			value := strings.TrimPrefix(line, "  New: ")
			lines[i] = "  New: " + ansiGreen + value + ansiReset
			continue
		}
	}
	return strings.Join(lines, "\n")
}

// GenerateStatusAndHeadersTableDiff builds a table-style diff for status code, headers,
// trailer headers, and synthetic content-length when body differs and header is absent.
func GenerateStatusAndHeadersTableDiff(test models.TestResult) string {
	var sb strings.Builder
	sb.WriteString("=== CHANGES IN STATUS AND HEADERS ===\n")

	hasDiff := false

	// Status code
	if !test.Result.StatusCode.Normal {
		hasDiff = true
		sb.WriteString("Path: status_code\n")
		sb.WriteString(fmt.Sprintf("  Old: %d\n", test.Result.StatusCode.Expected))
		sb.WriteString(fmt.Sprintf("  New: %d\n\n", test.Result.StatusCode.Actual))
	}

	// Headers
	for _, hr := range test.Result.HeadersResult {
		if hr.Normal {
			continue
		}
		hasDiff = true
		expected := strings.Join(hr.Expected.Value, ", ")
		actual := strings.Join(hr.Actual.Value, ", ")
		sb.WriteString(fmt.Sprintf("Path: header.%s\n", hr.Actual.Key))
		sb.WriteString(fmt.Sprintf("  Old: %s\n", expected))
		sb.WriteString(fmt.Sprintf("  New: %s\n\n", actual))
	}

	// Trailer headers
	for _, tr := range test.Result.TrailerResult {
		if tr.Normal {
			continue
		}
		hasDiff = true
		expected := strings.Join(tr.Expected.Value, ", ")
		actual := strings.Join(tr.Actual.Value, ", ")
		sb.WriteString(fmt.Sprintf("Path: trailer.%s\n", tr.Actual.Key))
		sb.WriteString(fmt.Sprintf("  Old: %s\n", expected))
		sb.WriteString(fmt.Sprintf("  New: %s\n\n", actual))
	}

	// Synthetic content length if body differs and content-length header wasn't already reported
	hasContentLengthHeaderChange := false
	for _, hr := range test.Result.HeadersResult {
		if strings.EqualFold(hr.Actual.Key, "Content-Length") || strings.EqualFold(hr.Expected.Key, "Content-Length") {
			hasContentLengthHeaderChange = !hr.Normal
			break
		}
	}
	if !hasContentLengthHeaderChange {
		for _, br := range test.Result.BodyResult {
			if br.Normal {
				continue
			}
			expLen := len(br.Expected)
			actLen := len(br.Actual)
			if expLen != actLen {
				hasDiff = true
				sb.WriteString("Path: content_length\n")
				sb.WriteString(fmt.Sprintf("  Old: %d\n", expLen))
				sb.WriteString(fmt.Sprintf("  New: %d\n\n", actLen))
				break
			}
		}
	}

	if !hasDiff {
		return "No differences found in status or headers."
	}
	return strings.TrimSpace(sb.String())
}
