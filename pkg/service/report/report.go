package report

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/k0kubun/pp/v3"
	"go.keploy.io/server/v2/config"
	matcherUtils "go.keploy.io/server/v2/pkg/matcher"
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/pkg/service/tools"
	"go.uber.org/zap"
	"gopkg.in/yaml.v3"
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

// collectReports loads whole test-set reports for summary.
func (r *Report) collectReports(ctx context.Context, runID string, testSetIDs []string) (map[string]*models.TestReport, error) {
	res := make(map[string]*models.TestReport, len(testSetIDs))
	for _, ts := range testSetIDs {
		clean := strings.TrimSuffix(ts, ReportSuffix)
		rep, err := r.reportDB.GetReport(ctx, runID, clean)
		if err != nil {
			r.logger.Error("failed to get report for test-set", zap.String("test_set_id", clean), zap.Error(err))
			continue
		}
		if rep != nil {
			res[clean] = rep
		}
	}
	if len(res) == 0 {
		return nil, fmt.Errorf("no reports found for summary")
	}
	return res, nil
}

// print only selected test-cases (failed => with diff, passed => compact notice)
func (r *Report) printSpecificTestCases(ctx context.Context, runID string, testSetIDs []string, ids []string) error {
	any := false
	for _, ts := range testSetIDs {
		clean := strings.TrimSuffix(ts, ReportSuffix)
		rep, err := r.reportDB.GetReport(ctx, runID, clean)
		if err != nil || rep == nil {
			if err != nil {
				r.logger.Error("failed to get report for test-set", zap.String("test_set_id", clean), zap.Error(err))
			}
			continue
		}
		sel := filterTestsByIDs(rep.Tests, ids)
		if len(sel) == 0 {
			continue
		}
		any = true
		if err := r.printTests(sel); err != nil {
			return err
		}
	}
	if !any {
		r.logger.Warn("No matching test-cases found in the selected test-sets", zap.Strings("ids", ids))
	}
	return nil
}

// helper used by both file and DB paths
func (r *Report) printTests(tests []models.TestResult) error {
	// Respect full/compact body setting when printing failures
	for _, t := range tests {
		if t.Status == models.TestStatusFailed {
			if err := r.printSingleTestReport(t); err != nil {
				return err
			}
			continue
		}
		// Passed — print a small header so users see it was found and green
		printer := r.createFormattedPrinter()
		header := r.generateTestHeader(t, printer)
		if _, err := printer.Printf(header); err != nil {
			r.logger.Error("failed to print test header", zap.Error(err))
			return err
		}
		fmt.Printf("Testcase %q (%s) PASSED ✅ (%s)\n", t.TestCaseID, t.Name, t.TimeTaken)
		fmt.Println("\n--------------------------------------------------------------------")
	}
	return nil
}

// printSummary prints the grand summary + per test-set table.
// Time Taken uses the TimeTaken field from TestReport if available, otherwise estimates from tests.
func (r *Report) printSummary(reports map[string]*models.TestReport) error {
	var total, passed, failed int
	type row struct {
		name              string
		total, pass, fail int
		dur               time.Duration
		timeTaken         string
	}
	rows := make([]row, 0, len(reports))

	for name, rep := range reports {
		total += rep.Total
		passed += rep.Success
		failed += rep.Failure

		// Use TimeTaken from TestReport if available, otherwise estimate from tests
		var dur time.Duration
		if rep.TimeTaken != "" {
			if parsedDur, err := parseTimeString(rep.TimeTaken); err == nil {
				dur = parsedDur
			}
		}
		if dur == 0 {
			dur = estimateDuration(rep.Tests)
		}

		rows = append(rows, row{name: name, total: rep.Total, pass: rep.Success, fail: rep.Failure, dur: dur, timeTaken: rep.TimeTaken})
	}

	// Sort by name for determinism
	sort.Slice(rows, func(i, j int) bool { return rows[i].name < rows[j].name })

	grandDur := time.Duration(0)
	for _, r := range rows {
		grandDur += r.dur
	}

	fmt.Println("<=========================================>")
	fmt.Println(" COMPLETE TESTRUN SUMMARY.")
	fmt.Printf("\tTotal tests: %d\n", total)
	fmt.Printf("\tTotal test passed: %d\n", passed)
	fmt.Printf("\tTotal test failed: %d\n", failed)
	if grandDur > 0 {
		fmt.Printf("\tTotal time taken: %q\n", fmtDuration(grandDur))
	} else {
		fmt.Printf("\tTotal time taken: %q\n", "N/A")
	}

	// Initialize a new tabwriter.
	// The parameters are: output, minwidth, tabwidth, padding, padchar, flags
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)

	// Write the header to the tabwriter buffer. Use \t as a separator.
	fmt.Fprintln(w, "\tTest Suite\tTotal\tPassed\tFailed\tTime Taken\t")

	// Write each row of data to the buffer.
	for _, rrow := range rows {
		tt := "N/A"
		if rrow.dur > 0 {
			tt = fmtDuration(rrow.dur)
		}
		fmt.Fprintf(w, "\t%s\t%d\t%d\t%d\t%s\t\n",
			rrow.name, rrow.total, rrow.pass, rrow.fail, tt)
	}

	// Flush the buffer to standard output to print the formatted table.
	w.Flush()

	fmt.Println("\nFAILED TEST CASES:")
	if failed == 0 {
		fmt.Println("\t(none)")
	} else {
		for _, rrow := range rows {
			rep := reports[rrow.name]
			if rep == nil {
				continue
			}
			var failedList []string
			for _, t := range rep.Tests {
				if t.Status == models.TestStatusFailed {
					label := fmt.Sprintf("%s", t.TestCaseID)
					failedList = append(failedList, label)
				}
			}
			if len(failedList) == 0 {
				continue
			}
			fmt.Printf("\t%s\n", rrow.name)
			for _, fc := range failedList {
				fmt.Printf("\t  - %s\n", fc)
			}
		}
	}

	fmt.Println("<=========================================>")
	return nil
}

// GenerateReport orchestrates the entire report generation process
func (r *Report) GenerateReport(ctx context.Context) error {
	if r.config.Report.ReportPath != "" {
		// File mode (single test-set file)
		return r.generateReportFromFile(r.config.Report.ReportPath)
	}

	latestRunID, err := r.getLatestTestRunID(ctx)
	if err != nil {
		return err
	}
	if latestRunID == "" {
		r.logger.Warn("no test runs found")
		return nil
	}
	r.logger.Debug("latest run id is", zap.String("latest_run_id", latestRunID))

	testSetIDs := r.extractTestSetIDs()
	if len(testSetIDs) == 0 {
		r.logger.Info("No test sets selected for report generation, Generating report for all test sets")
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

	if r.config.Report.SummaryOnly {
		reports, err := r.collectReports(ctx, latestRunID, testSetIDs)
		if err != nil {
			return err
		}
		return r.printSummary(reports)
	}

	// Specific test-case(s)
	if len(r.config.Report.TestCaseIDs) > 0 {
		return r.printSpecificTestCases(ctx, latestRunID, testSetIDs, r.config.Report.TestCaseIDs)
	}

	// Original path: print only FAILED tests
	failedTests, err := r.collectFailedTests(ctx, latestRunID, testSetIDs)
	if err != nil {
		return err
	}
	if len(failedTests) == 0 {
		r.logger.Info("No failed tests found in the latest test run")
		return nil
	}

	if err := r.printFailedTestReports(failedTests); err != nil {
		r.logger.Error("failed to print failed test reports", zap.Error(err))
		return err
	}
	r.logger.Info(fmt.Sprintf("✂️ CLI output truncated - see the %s report file for the complete diff.", latestRunID))
	r.logger.Info("Report generation completed successfully")
	return nil
}

// generateReportFromFile loads a report from an absolute file path and prints diffs for failed tests
// OR summary / specific test cases if flags are set.
func (r *Report) generateReportFromFile(reportPath string) error {
	if !filepath.IsAbs(reportPath) {
		return fmt.Errorf("report-path must be absolute, got %q", reportPath)
	}
	data, err := os.ReadFile(reportPath)
	if err != nil {
		r.logger.Error("failed to read report file", zap.String("report_path", reportPath), zap.Error(err))
		return err
	}
	r.logger.Info("Generating report from file", zap.String("report_path", reportPath))

	// Attempt to parse the file into the canonical TestReport struct.
	var tr models.TestReport
	if err := yaml.Unmarshal(data, &tr); err == nil {
		// This is the successful, correct path.

		// Summary-only
		if r.config.Report.SummaryOnly {
			m := map[string]*models.TestReport{tr.Name: &tr}
			return r.printSummary(m)
		}
		// Test-case filtering
		if len(r.config.Report.TestCaseIDs) > 0 {
			sel := filterTestsByIDs(tr.Tests, r.config.Report.TestCaseIDs)
			if len(sel) == 0 {
				r.logger.Warn("No matching test-cases found in file", zap.Strings("ids", r.config.Report.TestCaseIDs))
				return nil
			}
			return r.printTests(sel)
		}
		// Default: only failed tests
		failed := r.extractFailedTestsFromResults(tr.Tests)
		if len(failed) == 0 {
			r.logger.Info("No failed tests found in the provided report file")
			return nil
		}
		return r.printFailedTestReports(failed)
	}

	// Fallback for older/simpler report formats that only contain a 'tests' array.
	r.logger.Debug("Could not parse as full TestReport, falling back to legacy test array parser", zap.Error(err))
	tests, err := r.parseReportTests(data)
	if err != nil {
		r.logger.Error("failed to parse report file with legacy parser", zap.String("report_path", reportPath), zap.Error(err))
		return err
	}
	if r.config.Report.SummaryOnly {
		// We don't have totals; print a compact synthetic summary of the array we have.
		total, pass, fail := len(tests), 0, 0
		var failedCases []string
		for _, t := range tests {
			if t.Status == models.TestStatusFailed {
				fail++
				label := t.TestCaseID
				if t.Name != "" {
					label = fmt.Sprintf("%s (%s)", t.TestCaseID, t.Name)
				}
				failedCases = append(failedCases, label)
			} else {
				pass++
			}
		}
		// Calculate total time from individual test results
		totalTime := estimateDuration(tests)
		printSingleSummary("file", total, pass, fail, totalTime, failedCases)
		return nil
	}
	if len(r.config.Report.TestCaseIDs) > 0 {
		sel := filterTestsByIDs(tests, r.config.Report.TestCaseIDs)
		if len(sel) == 0 {
			r.logger.Warn("No matching test-cases found in file (tests-only parse)", zap.Strings("ids", r.config.Report.TestCaseIDs))
			return nil
		}
		return r.printTests(sel)
	}
	failed := r.extractFailedTestsFromResults(tests)
	if len(failed) == 0 {
		r.logger.Info("No failed tests found in the provided report file")
		return nil
	}
	return r.printFailedTestReports(failed)
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
	// If full mode is ON, use the original pipeline (entire expected/actual bodies)
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
	return printer.Sprintf("Testrun failed for %s/%s\n\n", test.Name, test.TestCaseID)
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
