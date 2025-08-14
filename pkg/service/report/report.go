package report

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

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
	testSetIDs := r.extractTestSetIDs()
	if len(testSetIDs) == 0 {
		r.logger.Info("No test sets selected for report generation, Generating report for all test sets")

		var err error

		testSetIDs, err = r.testDB.GetAllTestSetIDs(ctx)
		if err != nil {
			r.logger.Error("failed to get all test set ids", zap.Error(err))
			return err
		}

		if len(testSetIDs) == 0 {
			r.logger.Warn("No test sets found for report generation")
			return nil
		}
	}

	latestRunID, err := r.getLatestTestRunID(ctx)

	if err != nil {
		return err
	}

	if latestRunID == "" {
		r.logger.Warn("no test runs found")
		return nil
	}

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
		numi, erri := strconv.Atoi(strings.TrimPrefix(testRunIDs[i], "test-run-"))
		numj, errj := strconv.Atoi(strings.TrimPrefix(testRunIDs[j], "test-run-"))
		if erri != nil || errj != nil {
			return testRunIDs[i] < testRunIDs[j]
		}
		return numi < numj
	})

	return testRunIDs[len(testRunIDs)-1], nil
}

// collectFailedTests gathers all failed tests from the specified test sets
func (r *Report) collectFailedTests(ctx context.Context, runID string, testSetIDs []string) ([]models.TestResult, error) {
	var failedTests []models.TestResult

	for _, testSetID := range testSetIDs {
		results, err := r.reportDB.GetReport(ctx, runID, testSetID)
		if err != nil {
			r.logger.Error("failed to get test case results for test set",
				zap.String("test_set_id", testSetID), zap.Error(err))
			continue
		}

		if results == nil {
			r.logger.Warn("no results found for test set", zap.String("test_set_id", testSetID))
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
