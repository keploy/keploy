package report

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
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

	// performance: single buffered writer and a reusable pretty printer
	out     *bufio.Writer
	printer *pp.PrettyPrinter
}

const (
	ReportSuffix  = "-report"
	TestRunPrefix = "test-run-"
)

func New(logger *zap.Logger, cfg *config.Config, reportDB ReportDB, testDB TestDB) *Report {
	r := &Report{
		logger:   logger,
		config:   cfg,
		reportDB: reportDB,
		testDB:   testDB,
	}
	// 1MB buffered writer
	r.out = bufio.NewWriterSize(os.Stdout, 1<<20)
	// Reuse one pretty printer
	pr := pp.New()
	pr.WithLineInfo = false
	pr.SetColorScheme(models.GetFailingColorScheme())
	r.printer = pr
	return r
}

// collectReports loads whole test-set reports for summary.
func (r *Report) collectReports(ctx context.Context, runID string, testSetIDs []string) (map[string]*models.TestReport, error) {
	res := make(map[string]*models.TestReport, len(testSetIDs))
	for _, ts := range testSetIDs {
		// Check for context cancellation
		select {
		case <-ctx.Done():
			r.logger.Info("Report generation cancelled by user")
			return nil, ctx.Err()
		default:
		}

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
		// Check for context cancellation
		select {
		case <-ctx.Done():
			r.logger.Info("Report generation cancelled by user")
			return ctx.Err()
		default:
		}

		clean := strings.TrimSuffix(ts, ReportSuffix)
		rep, err := r.reportDB.GetReport(ctx, runID, clean)
		if err != nil || rep == nil {
			if err != nil {
				r.logger.Error("failed to get report for test-set", zap.String("test_set_id", clean), zap.Error(err))
			}
			continue
		}
		sel := r.filterTestsByIDs(rep.Tests, ids)
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
	return r.out.Flush()
}

// helper used by both file and DB paths
func (r *Report) printTests(tests []models.TestResult) error {
	for _, t := range tests {
		if t.Status == models.TestStatusFailed {
			if err := r.printSingleTestReport(t); err != nil {
				return err
			}
			continue
		}
		// Passed — minimize output and avoid pretty printer
		fmt.Fprintf(r.out, "Testcase %q (%s) PASSED ✅ (%s)\n", t.TestCaseID, t.Name, t.TimeTaken)
		fmt.Fprintln(r.out, "\n--------------------------------------------------------------------")
	}
	return r.out.Flush()
}

// printSummary prints the grand summary + per test-set table.
func (r *Report) printSummary(reports map[string]*models.TestReport) error {
	var total, passed, failed int
	type row struct {
		name              string
		total, pass, fail int
		dur               time.Duration
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

		rows = append(rows, row{name: name, total: rep.Total, pass: rep.Success, fail: rep.Failure, dur: dur})
	}

	// Sort by name for determinism
	sort.Slice(rows, func(i, j int) bool { return rows[i].name < rows[j].name })

	grandDur := time.Duration(0)
	for _, rr := range rows {
		grandDur += rr.dur
	}

	fmt.Fprintln(r.out, "<=========================================>")
	fmt.Fprintln(r.out, " COMPLETE TESTRUN SUMMARY.")
	fmt.Fprintf(r.out, "\tTotal tests: %d\n", total)
	fmt.Fprintf(r.out, "\tTotal test passed: %d\n", passed)
	fmt.Fprintf(r.out, "\tTotal test failed: %d\n", failed)
	if grandDur > 0 {
		fmt.Fprintf(r.out, "\tTotal time taken: %q\n", fmtDuration(grandDur))
	} else {
		fmt.Fprintf(r.out, "\tTotal time taken: %q\n", "N/A")
	}

	// Tabwriter over the same buffered writer.
	w := tabwriter.NewWriter(r.out, 0, 0, 3, ' ', 0)
	fmt.Fprintln(w, "\tTest Suite\tTotal\tPassed\tFailed\tTime Taken\t")
	for _, rrow := range rows {
		tt := "N/A"
		if rrow.dur > 0 {
			tt = fmtDuration(rrow.dur)
		}
		fmt.Fprintf(w, "\t%s\t%d\t%d\t%d\t%s\t\n", rrow.name, rrow.total, rrow.pass, rrow.fail, tt)
	}
	_ = w.Flush()

	fmt.Fprintln(r.out, "\nFAILED TEST CASES:")
	if failed == 0 {
		fmt.Fprintln(r.out, "\t(none)")
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
			fmt.Fprintf(r.out, "\t%s\n", rrow.name)
			for _, fc := range failedList {
				fmt.Fprintf(r.out, "\t  - %s\n", fc)
			}
		}
	}

	fmt.Fprintln(r.out, "<=========================================>")
	return r.out.Flush()
}

func (r *Report) filterTestsByIDs(tests []models.TestResult, ids []string) []models.TestResult {
	set := map[string]struct{}{}
	for _, id := range ids {
		set[strings.TrimSpace(id)] = struct{}{}
	}
	out := make([]models.TestResult, 0, len(ids))
	for _, t := range tests {
		if _, ok := set[t.TestCaseID]; ok {
			out = append(out, t)
		}
	}
	return out
}

// GenerateReport orchestrates the entire report generation process
func (r *Report) GenerateReport(ctx context.Context) error {
	// Check for context cancellation at the start
	select {
	case <-ctx.Done():
		r.logger.Info("Report generation cancelled by user")
		return ctx.Err()
	default:
	}

	if r.config.Report.ReportPath != "" {
		// File mode (single test-set file)
		return r.generateReportFromFile(ctx, r.config.Report.ReportPath)
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

	if r.config.Report.Summary {
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

	if err := r.printFailedTestReports(ctx, failedTests); err != nil {
		r.logger.Error("failed to print failed test reports", zap.Error(err))
		return err
	}
	r.logger.Info(fmt.Sprintf("✂️ CLI output truncated - see the %s report file for the complete diff.", latestRunID))
	r.logger.Info("Report generation completed successfully")
	return nil
}

// generateReportFromFile loads a report from an absolute file path and prints diffs for failed tests
// OR summary / specific test cases if flags are set.
func (r *Report) generateReportFromFile(ctx context.Context, reportPath string) error {
	if !filepath.IsAbs(reportPath) {
		return fmt.Errorf("report-path must be absolute, got %q", reportPath)
	}
	f, err := os.Open(reportPath)
	if err != nil {
		r.logger.Error("failed to open report file", zap.String("report_path", reportPath), zap.Error(err))
		return err
	}
	defer f.Close()
	r.logger.Info("Generating report from file", zap.String("report_path", reportPath))

	dec := yaml.NewDecoder(f)

	// Attempt to parse the file into the canonical TestReport struct.
	var tr models.TestReport
	if err := dec.Decode(&tr); err == nil && (tr.Name != "" || len(tr.Tests) > 0) {
		// Summary-only
		if r.config.Report.Summary {
			m := map[string]*models.TestReport{tr.Name: &tr}
			return r.printSummary(m)
		}
		// Test-case filtering
		if len(r.config.Report.TestCaseIDs) > 0 {
			sel := r.filterTestsByIDs(tr.Tests, r.config.Report.TestCaseIDs)
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
		return r.printFailedTestReports(ctx, failed)
	}

	// Fallback for older/simpler report formats that only contain a 'tests' array.
	// Reopen and stream again for a clean decoder.
	if err := f.Close(); err != nil {
		// non-fatal
	}
	f2, err := os.Open(reportPath)
	if err != nil {
		return err
	}
	defer f2.Close()
	type legacy struct {
		Tests []models.TestResult `yaml:"tests"`
	}
	var lg legacy
	dec2 := yaml.NewDecoder(f2)
	if err := dec2.Decode(&lg); err != nil {
		r.logger.Error("failed to parse report file with legacy parser", zap.String("report_path", reportPath), zap.Error(err))
		return err
	}

	if r.config.Report.Summary {
		total, pass, fail := len(lg.Tests), 0, 0
		var failedCases []string
		for _, t := range lg.Tests {
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
		totalTime := estimateDuration(lg.Tests)
		printSingleSummaryTo(r.out, "file", total, pass, fail, totalTime, failedCases)
		return r.out.Flush()
	}

	if len(r.config.Report.TestCaseIDs) > 0 {
		sel := r.filterTestsByIDs(lg.Tests, r.config.Report.TestCaseIDs)
		if len(sel) == 0 {
			r.logger.Warn("No matching test-cases found in file (tests-only parse)", zap.Strings("ids", r.config.Report.TestCaseIDs))
			return nil
		}
		return r.printTests(sel)
	}

	failed := r.extractFailedTestsFromResults(lg.Tests)
	if len(failed) == 0 {
		r.logger.Info("No failed tests found in the provided report file")
		return nil
	}
	return r.printFailedTestReports(ctx, failed)
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
		// Check for context cancellation
		select {
		case <-ctx.Done():
			r.logger.Info("Report generation cancelled by user")
			return nil, ctx.Err()
		default:
		}

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

func (r *Report) printFailedTestReports(ctx context.Context, failedTests []models.TestResult) error {
	if r.config.Report.ShowFullBody {
		type item struct {
			idx int
			sb  strings.Builder
			err error
		}

		workers := runtime.GOMAXPROCS(0)
		if workers < 2 {
			workers = 2
		}
		sem := make(chan struct{}, workers)
		results := make([]item, len(failedTests))
		var wg sync.WaitGroup

		for i := range failedTests {
			// check cancellation early
			select {
			case <-ctx.Done():
				r.logger.Info("Report generation cancelled by user")
				return ctx.Err()
			default:
			}

			wg.Add(1)
			sem <- struct{}{}
			go func(i int) {
				defer wg.Done()
				defer func() { <-sem }()
				var sb strings.Builder
				if err := r.renderSingleFullBodyFailedTest(&sb, failedTests[i]); err != nil {
					results[i] = item{idx: i, err: err}
					return
				}
				results[i] = item{idx: i, sb: sb}
			}(i)
		}
		wg.Wait()

		for i := range results {
			if results[i].err != nil {
				return results[i].err
			}
			if _, err := r.out.WriteString(results[i].sb.String()); err != nil {
				return err
			}
		}
		return r.out.Flush()
	}

	type item struct {
		idx int
		sb  strings.Builder
		err error
	}
	workers := runtime.GOMAXPROCS(0)
	if workers < 2 {
		workers = 2
	}
	sem := make(chan struct{}, workers)
	results := make([]item, len(failedTests))
	var wg sync.WaitGroup

	for i := range failedTests {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			var sb strings.Builder
			if err := r.renderSingleFailedTest(&sb, failedTests[i]); err != nil {
				results[i] = item{idx: i, err: err}
				return
			}
			results[i] = item{idx: i, sb: sb}
		}(i)
	}
	wg.Wait()

	for i := range results {
		if results[i].err != nil {
			return results[i].err
		}
		if _, err := r.out.WriteString(results[i].sb.String()); err != nil {
			return err
		}
	}
	return r.out.Flush()
}

// renderSingleFailedTest writes the failed test report into sb (non-full-body mode).
func (r *Report) renderSingleFailedTest(sb *strings.Builder, test models.TestResult) error {
	// Header (keep same text)
	fmt.Fprintf(sb, "Testrun failed for %s/%s\n\n", test.Name, test.TestCaseID)

	// Status & header diffs (compact)
	metaDiff := GenerateStatusAndHeadersTableDiff(test)
	sb.WriteString(applyCliColorsToDiff(metaDiff))
	sb.WriteString("\n\n")

	sb.WriteString("=== CHANGES WITHIN THE RESPONSE BODY ===\n\n")

	// Body diffs
	for _, bodyResult := range test.Result.BodyResult {
		if bodyResult.Normal {
			continue
		}
		if strings.EqualFold(string(bodyResult.Type), "JSON") {
			diff, err := GenerateTableDiff(bodyResult.Expected, bodyResult.Actual)
			if err == nil {
				sb.WriteString(applyCliColorsToDiff(diff))
				sb.WriteString("\n")
			} else {
				tmp := *r
				tmp.out = bufio.NewWriterSize(&writerAdapter{sb: sb}, 64<<10)
				_ = tmp.printDefaultBodyDiff(bodyResult)
				_ = tmp.out.Flush()
			}
		} else {
			// Force the old compact format for non-JSON bodies (fast).
			diff := GeneratePlainOldNewDiff(bodyResult.Expected, bodyResult.Actual, bodyResult.Type)
			sb.WriteString(applyCliColorsToDiff(diff))
			sb.WriteString("\n\n")
		}
	}
	sb.WriteString("\n--------------------------------------------------------------------\n")
	return nil
}

// writerAdapter lets us reuse a bufio.Writer on top of strings.Builder.
type writerAdapter struct{ sb *strings.Builder }

func (w *writerAdapter) Write(p []byte) (int, error) { return w.sb.Write(p) }

func (r *Report) printSingleTestReport(test models.TestResult) error {
	if r.config.Report.ShowFullBody {
		var sb strings.Builder
		if err := r.renderSingleFullBodyFailedTest(&sb, test); err != nil {
			return err
		}
		if _, err := r.out.WriteString(sb.String()); err != nil {
			return err
		}
		return r.out.Flush()
	}

	// Non-full-body: unchanged
	var sb strings.Builder
	if err := r.renderSingleFailedTest(&sb, test); err != nil {
		return err
	}
	if _, err := r.out.WriteString(sb.String()); err != nil {
		return err
	}
	return r.out.Flush()
}

// renderSingleFullBodyFailedTest renders a single failed test in full-body mode into sb.
func (r *Report) renderSingleFullBodyFailedTest(sb *strings.Builder, test models.TestResult) error {
	// Write header via printer.Sprintf (no stdout)
	header := r.generateTestHeader(test, r.printer) // returns string via Sprintf already
	sb.WriteString(header)

	// Route DiffsPrinter output into this builder (no os.Stdout)
	localOut := &writerAdapter{sb: sb}
	logDiffs := matcherUtils.NewDiffsPrinterOut(localOut, test.Name)

	// status/header/body diffs
	if err := r.addStatusCodeDiffs(test, &logDiffs); err != nil {
		return err
	}
	if err := r.addHeaderDiffs(test, &logDiffs); err != nil {
		return err
	}
	if err := r.addBodyDiffs(test, &logDiffs); err != nil {
		return err
	}

	if err := logDiffs.Render(); err != nil {
		r.logger.Error("failed to render the diffs", zap.Error(err))
		return err
	}
	sb.WriteString("\n--------------------------------------------------------------------\n")
	return nil
}

// createFormattedPrinter: use r.printer (initialized in New)
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

// extractTestSetIDs extracts and cleans test set IDs from config
func (r *Report) extractTestSetIDs() []string {
	var testSetIDs []string
	for testSet := range r.config.Report.SelectedTestSets {
		testSetIDs = append(testSetIDs, strings.TrimSpace(testSet))
	}
	return testSetIDs
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
