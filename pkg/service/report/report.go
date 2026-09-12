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
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg"
	matcherUtils "go.keploy.io/server/v3/pkg/matcher"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/service/tools"
	"go.keploy.io/server/v3/utils"
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

type item struct {
	idx int
	sb  strings.Builder
	err error
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
	if !cfg.DisableANSI {
		pr.SetColorScheme(models.GetFailingColorScheme())
	} else {
		pr.SetColoringEnabled(false)
	}
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
		if err := r.printTests(ctx, sel); err != nil {
			return fmt.Errorf("failed to print tests in printSpecificTestCases: %w", err)
		}
	}
	if !any {
		r.logger.Debug("No matching test-cases found in the selected test-sets", zap.Strings("ids", ids))
	}
	err := r.out.Flush()
	if err != nil {
		return fmt.Errorf("failed while flushing in printSpecificTestCases: %w", err)
	}
	return nil
}

// helper used by both file and DB paths
func (r *Report) printTests(ctx context.Context, tests []models.TestResult) error {
	for _, t := range tests {
		// FAILED/OBSOLETE keep the full diff. A test that merely lost a
		// dependency keeps its one-line status notice below and gains one
		// extra line, rather than being dressed up as a failure.
		if shouldRenderDiff(t) && !rendersDepNoticeOnly(t) {
			if err := r.printSingleTestReport(ctx, t); err != nil {
				return fmt.Errorf("failed to print single test report in printTests: %w", err)
			}
			continue
		}
		// Not rendering a diff — minimize output and avoid pretty printer.
		// The status is PRINTED, not assumed: this branch takes every
		// non-FAILED status, so hardcoding "PASSED ✅" made
		// `keploy report --test-case <obsolete>` announce a pass for a test
		// that was demoted, and the dependency notice below then printed
		// directly under a line asserting it passed.
		fmt.Fprintf(r.out, "Testcase %q (%s) %s (%s)\n", t.TestCaseID, t.Name, testStatusLabel(t.Status), t.TimeTaken)
		// ... but a PASSED test can still have stopped making a recorded
		// outgoing call. That is the silent-green case; it gets one line here
		// whether or not --assert-dependencies was passed.
		fmt.Fprint(r.out, models.FormatDepNotice(t.Result.DepResult))
		fmt.Fprintln(r.out, "\n--------------------------------------------------------------------")
	}
	err := r.out.Flush()
	if err != nil {
		return fmt.Errorf("failed while flushing in printTests: %w", err)
	}
	return nil
}

// testStatusLabel renders a status for the one-line, no-diff form of
// `keploy report`.
//
// PASSED keeps its exact historical bytes ("PASSED ✅"), which is what makes
// this change invisible for every report whose tests all passed; the other
// statuses stop borrowing it.
func testStatusLabel(status models.TestStatus) string {
	switch status {
	case models.TestStatusPassed:
		return "PASSED ✅"
	case models.TestStatusFailed:
		return "FAILED ❌"
	case models.TestStatusObsolete:
		return "OBSOLETE ⚠️"
	case models.TestStatusIgnored:
		return "IGNORED ⏭️"
	}
	// An unknown status is still better named than mislabelled as a pass.
	return string(status)
}

// printSummary prints the grand summary + per test-set table.
func (r *Report) printSummary(reports map[string]*models.TestReport) error {
	var total, passed, failed, obsolete int
	var highRisk, mediumRisk, lowRisk int
	categoryCounts := make(map[models.FailureCategory]int)

	type row struct {
		name                        string
		total, pass, fail, obsolete int
		dur                         time.Duration
	}
	rows := make([]row, 0, len(reports))

	for name, rep := range reports {
		total += rep.Total
		passed += rep.Success
		failed += rep.Failure
		obsolete += rep.Obsolete

		// Count risk levels for failed tests that carry one.
		for _, test := range rep.Tests {
			if test.Status == models.TestStatusFailed && test.FailureInfo.Risk != "" {
				switch test.FailureInfo.Risk {
				case models.High:
					highRisk++
				case models.Medium:
					mediumRisk++
				case models.Low:
					lowRisk++
				}
			}

			// Category counting, widened by exactly as much as this slice
			// needs and no more.
			//
			// FAILED is counted INDEPENDENTLY of the risk level. Risk is set
			// by the response matcher; a test promoted to FAILED by
			// --assert-dependencies has a matching response and therefore no
			// risk at all, so the historical `Risk != ""` gate meant
			// DEPENDENCY_MISSING — the one category this slice adds — could
			// never reach the summary block that advertises it.
			//
			// OBSOLETE contributes ONLY DependencyMissing. An OBSOLETE test
			// has never been counted here, and counting its other categories
			// would silently change the numbers under a heading called FAILURE
			// CATEGORIES for reports written long before this slice: a report
			// with one FAILED and one OBSOLETE test both carrying
			// SCHEMA_BROKEN would print "SCHEMA_BROKEN: 2" next to "Total test
			// failed: 1". That is unreviewed breadth on a surface CI users
			// scrape, and none of it is needed to surface a lost dependency.
			switch test.Status {
			case models.TestStatusFailed:
				for _, category := range test.FailureInfo.Category {
					categoryCounts[category]++
				}
			case models.TestStatusObsolete:
				for _, category := range test.FailureInfo.Category {
					if category == models.DependencyMissing {
						categoryCounts[category]++
					}
				}
			}
		}

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

		rows = append(rows, row{name: name, total: rep.Total, pass: rep.Success, fail: rep.Failure, obsolete: rep.Obsolete, dur: dur})
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
	if obsolete > 0 {
		fmt.Fprintf(r.out, "\tTotal test obsolete: %d\n", obsolete)
	}

	// Dependencies that the recording says a test exercises but that the test
	// never made during replay. Reported for EVERY status, because the case
	// worth surfacing is the one where the response still matched and the test
	// is green — CI is otherwise silent about it.
	if depTotal, depPassed := depNoticeCounts(reports); depTotal > 0 {
		fmt.Fprintf(r.out, "\tDependencies not exercised: %d test(s)\n", depTotal)
		if depPassed > 0 {
			// "not observed during the test's own window" rather than "never
			// made": consumed mocks are drained when the response comes back,
			// so an outgoing call the app makes AFTER writing its response is
			// attributed to the next test. The data supports the weaker claim.
			fmt.Fprintf(r.out, "\t\t%d of them PASSED — the response matched, but a recorded outgoing call was not observed during the test's own window.\n", depPassed)
			fmt.Fprintln(r.out, "\t\tRun `keploy test --assert-dependencies` to fail on this.")
		}
	}

	// Add risk level statistics
	if failed > 0 {
		fmt.Fprintln(r.out, "\n\tFAILURE RISK DISTRIBUTION:")
		fmt.Fprintf(r.out, "\t\tHigh Risk: %d\n", highRisk)
		fmt.Fprintf(r.out, "\t\tMedium Risk: %d\n", mediumRisk)
		fmt.Fprintf(r.out, "\t\tLow Risk: %d\n", lowRisk)
	}

	// The block still appears whenever a test FAILED, exactly as it always
	// has. The ONE new trigger is a dependency finding on a run with no
	// failures at all — an OBSOLETE test labelled DEPENDENCY_MISSING, which
	// carries a real category with no failure and no risk behind it and would
	// otherwise be invisible in `--summary`.
	//
	// Deliberately NOT `len(categoryCounts) > 0`: that also printed a
	// brand-new three-line block on zero-failure runs of UNCHANGED pre-slice-4
	// reports, which is collateral this slice does not need.
	//
	// The counting loop above is not byte-identical for every pre-slice-4
	// report, and that part is deliberate. It used to be nested inside
	// `if Risk != ""`, and CreateFailedTestResult produces exactly the shape
	// that trips over: replay.go copies Category only when Risk != models.None,
	// then appends AppConnectionError unconditionally, leaving Risk empty. So a
	// report from a run where the app never answered rendered "No specific
	// categories identified" while carrying APP_CONNECTION_ERROR, and now
	// renders "APP_CONNECTION_ERROR: 1". That is a pre-existing bug this slice
	// fixes on the way past, not a side effect of the DEPENDENCY_MISSING
	// plumbing — it is pinned by
	// TestPrintSummary_CountsDependenciesAndCategories's "a FAILED test with a
	// category and NO risk level is now counted" row so it stays deliberate.
	if failed > 0 || categoryCounts[models.DependencyMissing] > 0 {
		fmt.Fprintln(r.out, "\n\tFAILURE CATEGORIES:")
		if len(categoryCounts) == 0 {
			fmt.Fprintln(r.out, "\t\tNo specific categories identified")
		} else {
			// Sort categories alphabetically for consistent output
			categories := make([]models.FailureCategory, 0, len(categoryCounts))
			for cat := range categoryCounts {
				categories = append(categories, cat)
			}
			sort.Slice(categories, func(i, j int) bool {
				return string(categories[i]) < string(categories[j])
			})

			for _, category := range categories {
				count := categoryCounts[category]
				fmt.Fprintf(r.out, "\t\t%s: %d\n", category, count)
			}
		}
	}

	if grandDur > 0 {
		fmt.Fprintf(r.out, "\n\tTotal time taken: %q\n", fmtDuration(grandDur))
	} else {
		fmt.Fprintf(r.out, "\n\tTotal time taken: %q\n", "N/A")
	}

	// Tabwriter over the same buffered writer.
	w := tabwriter.NewWriter(r.out, 0, 0, 3, ' ', 0)
	header := "\tTest Suite\tTotal\tPassed\tFailed"
	if obsolete > 0 {
		header += "\tObsolete"
	}
	header += "\tTime Taken\t"
	fmt.Fprintln(w, header)
	for _, rrow := range rows {
		tt := "N/A"
		if rrow.dur > 0 {
			tt = fmtDuration(rrow.dur)
		}
		if obsolete > 0 {
			fmt.Fprintf(w, "\t%s\t%d\t%d\t%d\t%d\t%s\t\n", rrow.name, rrow.total, rrow.pass, rrow.fail, rrow.obsolete, tt)
		} else {
			fmt.Fprintf(w, "\t%s\t%d\t%d\t%d\t%s\t\n", rrow.name, rrow.total, rrow.pass, rrow.fail, tt)
		}
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

					// Add risk level if available and not NONE
					if t.FailureInfo.Risk != "" && t.FailureInfo.Risk != models.None {
						label += fmt.Sprintf(" [%s-RISK]", t.FailureInfo.Risk)
					}

					// Add categories if available
					if len(t.FailureInfo.Category) > 0 {
						categories := make([]string, len(t.FailureInfo.Category))
						for i, cat := range t.FailureInfo.Category {
							categories[i] = string(cat)
						}
						label += fmt.Sprintf(" [%s]", strings.Join(categories, ", "))
					}

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
	err := r.out.Flush()
	if err != nil {
		return fmt.Errorf("failed while flushing in printSummary: %w", err)
	}
	return nil
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
		// Both machine formats project a whole RUN (they carry test_run_id /
		// per-suite aggregates); --report-path addresses a single test-set
		// file with no run identity, so the combination is rejected rather
		// than silently emitting a run-shaped document about one file.
		if config.IsMachineReportFormat(r.config.Report.Format) {
			return fmt.Errorf("--format %s is not supported with --report-path; use the database-backed report path instead", r.config.Report.Format)
		}
		// File mode (single test-set file)
		return r.generateReportFromFile(ctx, r.config.Report.ReportPath)
	}

	latestRunID, err := r.getLatestTestRunID(ctx)
	if err != nil {
		return fmt.Errorf("failed to get latest test run ID: %w", err)
	}
	if latestRunID == "" {
		r.logger.Debug("no test runs found")
		return nil
	}
	r.logger.Debug("latest run id is", zap.String("latest_run_id", latestRunID))

	testSetIDs := r.extractTestSetIDs()
	if len(testSetIDs) == 0 {
		r.logger.Info("No test sets selected for report generation, Generating report for all test sets")
		testSetIDs, err = r.testDB.GetReportTestSets(ctx, latestRunID)
		if err != nil {
			r.logger.Error("failed to get all test set ids", zap.Error(err))
			return fmt.Errorf("failed to get test sets for report: %w", err)
		}
		if len(testSetIDs) == 0 {
			r.logger.Debug("No test sets found for report generation")
			return nil
		}
	}

	if r.config.Report.Format == config.ReportFormatJUnit {
		reports, err := r.collectReports(ctx, latestRunID, testSetIDs)
		if err != nil {
			return fmt.Errorf("failed to collect reports for JUnit output: %w", err)
		}
		if len(r.config.Report.TestCaseIDs) > 0 {
			for name, rep := range reports {
				rep.Tests = r.filterTestsByIDs(rep.Tests, r.config.Report.TestCaseIDs)
				rep.Total = len(rep.Tests)
				reports[name] = rep
			}
		}
		return r.generateJUnit(reports)
	}

	// --format json (NDJSON, one object per test result). Deliberately placed
	// BEFORE the global --json branch below: --json dumps the whole report map
	// as one blob, which is a different (and much less agent-friendly)
	// document. When both are given, the explicit --format wins.
	if r.config.Report.Format == config.ReportFormatJSON {
		reports, err := r.collectReports(ctx, latestRunID, testSetIDs)
		if err != nil {
			return fmt.Errorf("failed to collect reports for ndjson output: %w", err)
		}
		if len(r.config.Report.TestCaseIDs) > 0 {
			for name, rep := range reports {
				rep.Tests = r.filterTestsByIDs(rep.Tests, r.config.Report.TestCaseIDs)
				rep.Total = len(rep.Tests)
				reports[name] = rep
			}
		}
		return r.generateNDJSON(latestRunID, reports)
	}

	if r.config.JSONOutput {
		reports, err := r.collectReports(ctx, latestRunID, testSetIDs)
		if err != nil {
			return fmt.Errorf("failed to collect reports for json output: %w", err)
		}
		if len(r.config.Report.TestCaseIDs) > 0 {
			for name, rep := range reports {
				rep.Tests = r.filterTestsByIDs(rep.Tests, r.config.Report.TestCaseIDs)
				// Recompute all counters to match filtered tests
				rep.Total = len(rep.Tests)
				rep.Success = 0
				rep.Failure = 0
				rep.Ignored = 0
				rep.Obsolete = 0
				for _, t := range rep.Tests {
					switch t.Status {
					case models.TestStatusPassed:
						rep.Success++
					case models.TestStatusFailed:
						rep.Failure++
					case models.TestStatusIgnored:
						rep.Ignored++
					case models.TestStatusObsolete:
						rep.Obsolete++
					}
				}
				reports[name] = rep
			}
		}
		// Through r.out, like every other branch: writing straight to
		// os.Stdout races the buffered writer the rest of this service uses
		// and makes the output unassertable in a test.
		if err := utils.NewJSONWriterOut(r.out, true).Write(reports); err != nil {
			return err
		}
		return r.out.Flush()
	}

	if r.config.Report.Summary {
		reports, err := r.collectReports(ctx, latestRunID, testSetIDs)
		if err != nil {
			return fmt.Errorf("failed to collect reports for summary: %w", err)
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
	defer func() {
		if err := f.Close(); err != nil {
			r.logger.Error("failed to close report file", zap.String("report_path", reportPath), zap.Error(err))
		}
	}()

	r.logger.Info("Generating report from file", zap.String("report_path", reportPath))

	dec := yaml.NewDecoder(f)

	// Attempt to parse the file into the canonical TestReport struct.
	var tr models.TestReport
	err = dec.Decode(&tr)
	if err == nil && (tr.Name != "" || len(tr.Tests) > 0) {
		if r.config.JSONOutput {
			return utils.NewJSONWriter(true).Write(tr)
		}
		// Summary-only
		if r.config.Report.Summary {
			m := map[string]*models.TestReport{tr.Name: &tr}
			return r.printSummary(m)
		}
		// Test-case filtering
		if len(r.config.Report.TestCaseIDs) > 0 {
			sel := r.filterTestsByIDs(tr.Tests, r.config.Report.TestCaseIDs)
			if len(sel) == 0 {
				r.logger.Debug("No matching test-cases found in file", zap.Strings("ids", r.config.Report.TestCaseIDs))
				return nil
			}
			return r.printTests(ctx, sel)
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
	return r.parseAndProcessLegacyReportFormat(ctx, reportPath)
}

// parseAndProcessLegacyReportFormat handles parsing and processing of legacy report formats
func (r *Report) parseAndProcessLegacyReportFormat(ctx context.Context, reportPath string) error {
	// Reopen the file for a clean decoder
	f, err := os.Open(reportPath)
	if err != nil {
		return err
	}
	defer func() {
		if err := f.Close(); err != nil {
			r.logger.Error("failed to close report file", zap.String("report_path", reportPath), zap.Error(err))
		}
	}()

	// Define legacy report structure
	type legacy struct {
		Tests []models.TestResult `yaml:"tests"`
	}

	var lg legacy
	dec := yaml.NewDecoder(f)
	err = dec.Decode(&lg)
	if err != nil {
		r.logger.Error("failed to parse report file with legacy parser", zap.String("report_path", reportPath), zap.Error(err))
		return err
	}

	if r.config.JSONOutput {
		return utils.NewJSONWriter(true).Write(lg)
	}

	// Handle summary request for legacy format
	if r.config.Report.Summary {
		return r.processLegacySummary(lg.Tests)
	}

	// Handle specific test case filtering for legacy format
	if len(r.config.Report.TestCaseIDs) > 0 {
		return r.processLegacyTestCaseFiltering(ctx, lg.Tests)
	}

	// Default: process failed tests for legacy format
	return r.processLegacyFailedTests(ctx, lg.Tests)
}

// processLegacySummary generates a summary report for legacy format
func (r *Report) processLegacySummary(tests []models.TestResult) error {
	total, pass, fail, obsolete := len(tests), 0, 0, 0
	var failedCases []string

	for _, t := range tests {
		switch t.Status {
		case models.TestStatusFailed:
			fail++
			label := t.TestCaseID
			if t.Name != "" {
				label = fmt.Sprintf("%s (%s)", t.TestCaseID, t.Name)
			}
			failedCases = append(failedCases, label)
		case models.TestStatusObsolete:
			obsolete++
		default:
			pass++
		}
	}

	totalTime := estimateDuration(tests)
	printSingleSummaryTo(r.out, "file", total, pass, fail, obsolete, totalTime, failedCases)
	err := r.out.Flush()
	if err != nil {
		return fmt.Errorf("failed while flushing in processLegacySummary: %w", err)
	}
	return nil
}

// processLegacyTestCaseFiltering filters and displays specific test cases from legacy format
func (r *Report) processLegacyTestCaseFiltering(ctx context.Context, tests []models.TestResult) error {
	sel := r.filterTestsByIDs(tests, r.config.Report.TestCaseIDs)
	if len(sel) == 0 {
		r.logger.Debug("No matching test-cases found in file (tests-only parse)", zap.Strings("ids", r.config.Report.TestCaseIDs))
		return nil
	}
	return r.printTests(ctx, sel)
}

// processLegacyFailedTests processes and displays failed tests from legacy format
func (r *Report) processLegacyFailedTests(ctx context.Context, tests []models.TestResult) error {
	failed := r.extractFailedTestsFromResults(tests)
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
			r.logger.Debug("no results found for test set", zap.String("test_set_id", cleanTestSetID))
			continue
		}

		failedTests = append(failedTests, r.extractFailedTestsFromResults(results.Tests)...)
	}

	return failedTests, nil
}

// extractFailedTestsFromResults filters out the tests worth rendering per-test
// detail for: FAILED tests (historical behaviour, unchanged) plus ANY test —
// whatever its status — carrying a MISSING dependency assertion.
//
// The dependency arm is deliberately STATUS-INDEPENDENT. The flagship
// silent-green case this slice exists to expose is a test whose response still
// matches while a recorded outgoing call vanished, and the replayer leaves
// exactly that test PASSED (replay.go's `case testPass:` arm ignores the mock
// mismatch when the response matched). Admitting only FAILED, or only
// OBSOLETE, left that regression invisible in text and in JUnit unless the
// user opted into --assert-dependencies — which would make VISIBILITY depend
// on a verdict knob. It does not: the knob decides whether it becomes a
// failure, never whether it is shown.
//
// Backward compatible by construction: DepResult had zero writers before this
// slice, so no report written before it can contain a Normal==false dependency
// row, and every such report is admitted exactly as before.
func (r *Report) extractFailedTestsFromResults(tests []models.TestResult) []models.TestResult {
	var failedTests []models.TestResult
	for _, result := range tests {
		if shouldRenderDiff(result) {
			failedTests = append(failedTests, result)
		}
	}
	return failedTests
}

// shouldRenderDiff reports whether a test result is admitted to the per-test
// detail renderer at all.
func shouldRenderDiff(result models.TestResult) bool {
	return result.Status == models.TestStatusFailed || result.Result.HasMissingDeps()
}

// rendersDepNoticeOnly reports whether an admitted result is summarised in the
// compact dependency block instead of getting the full diff apparatus.
//
// ONLY A GENUINELY FAILED TEST GETS THE APPARATUS. A test that did not fail
// has no response diff to show: printing the header, "CHANGES IN STATUS AND
// HEADERS" and "CHANGES WITHIN THE RESPONSE BODY" around a PASSED test would
// be actively misleading, and an OBSOLETE test is in the same position —
// OBSOLETE is only reachable when the mock set diverged, which under this
// build guarantees a MISSING row, so admitting OBSOLETE to the full renderer
// meant 100% of obsolete tests grew a failure block where before this slice
// they rendered nothing at all in `keploy report`. The brief asked for a
// compact notice; this is that, for every non-FAILED status.
func rendersDepNoticeOnly(result models.TestResult) bool {
	return result.Status != models.TestStatusFailed
}

// partitionDepNotices splits the admitted results into the ones that get the
// full diff apparatus and the ones that are folded into the compact
// dependency block.
func partitionDepNotices(tests []models.TestResult) (diffs, notices []models.TestResult) {
	for _, t := range tests {
		if rendersDepNoticeOnly(t) {
			notices = append(notices, t)
			continue
		}
		diffs = append(diffs, t)
	}
	return diffs, notices
}

// printFailedTestReports renders the per-test detail for the admitted results.
//
// The admitted set is split first: only genuinely FAILED tests get the diff
// apparatus, and every other status folds into ONE bounded, deduplicated
// dependency block at the end (renderDepNoticeSummary). Rendering a block per
// notice-only test instead turned a 16-line all-green report into 1,217 lines
// on a 300-test suite that had lost one shared dependency.
func (r *Report) printFailedTestReports(ctx context.Context, admitted []models.TestResult) error {
	failedTests, notices := partitionDepNotices(admitted)

	if r.config.Report.ShowFullBody {

		workers := max(runtime.GOMAXPROCS(0), 2)
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
				if err := r.renderSingleFullBodyFailedTest(ctx, &sb, failedTests[i]); err != nil {
					results[i] = item{idx: i, err: err}
					return
				}
				results[i] = item{idx: i, sb: sb}
			}(i)
		}
		wg.Wait()

		for i := range results {
			if results[i].err != nil {
				return fmt.Errorf("failed to render full body test report: %w", results[i].err)
			}
			if _, err := r.out.WriteString(results[i].sb.String()); err != nil {
				return fmt.Errorf("failed to write test report to output: %w", err)
			}
		}
		if err := r.writeDepNoticeSummary(notices, admitted); err != nil {
			return err
		}
		err := r.out.Flush()
		if err != nil {
			return fmt.Errorf("failed while flushing in printFailedTestReports (full body mode): %w", err)
		}
		return nil
	}

	workers := max(runtime.GOMAXPROCS(0), 2)
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
			if err := r.renderSingleFailedTest(ctx, &sb, failedTests[i]); err != nil {
				results[i] = item{idx: i, err: err}
				return
			}
			results[i] = item{idx: i, sb: sb}
		}(i)
	}
	wg.Wait()

	for i := range results {
		if results[i].err != nil {
			return fmt.Errorf("failed to render test report: %w", results[i].err)
		}
		if _, err := r.out.WriteString(results[i].sb.String()); err != nil {
			return fmt.Errorf("failed to write test report to output: %w", err)
		}
	}
	if err := r.writeDepNoticeSummary(notices, admitted); err != nil {
		return err
	}
	err := r.out.Flush()
	if err != nil {
		return fmt.Errorf("failed while flushing in printFailedTestReports: %w", err)
	}
	return nil
}

// writeDepNoticeSummary appends the compact dependency block, if any.
func (r *Report) writeDepNoticeSummary(notices, admitted []models.TestResult) error {
	block := renderDepNoticeSummary(notices, admitted)
	if block == "" {
		return nil
	}
	if _, err := r.out.WriteString(block); err != nil {
		return fmt.Errorf("failed to write dependency notice summary: %w", err)
	}
	return nil
}

// renderSingleFailedTest writes the failed test report into sb (non-full-body mode).
func (r *Report) renderSingleFailedTest(_ context.Context, sb *strings.Builder, test models.TestResult) error {
	// Header with risk level and categories. Unconditionally "failed":
	// partitionDepNotices keeps every non-FAILED status out of this renderer.
	header := fmt.Sprintf("Testrun failed for %s/%s", test.Name, test.TestCaseID)

	// Add risk level if available and not NONE
	if test.FailureInfo.Risk != "" && test.FailureInfo.Risk != models.None {
		header += fmt.Sprintf(" [%s-RISK]", test.FailureInfo.Risk)
	}

	// Add categories if available
	if len(test.FailureInfo.Category) > 0 {
		categories := make([]string, len(test.FailureInfo.Category))
		for i, cat := range test.FailureInfo.Category {
			categories[i] = string(cat)
		}
		header += fmt.Sprintf(" [%s]", strings.Join(categories, ", "))
	}

	sb.WriteString(header + "\n")

	// Mock-miss diagnostics FIRST: when an outgoing call had no matching
	// mock, the response diff below is usually a downstream symptom (the app
	// reacting to keploy's synthetic error), so lead with the root cause.
	renderUnmatchedCalls(sb, test)

	// Status & header diffs (compact)
	metaDiff := GenerateStatusAndHeadersTableDiff(test)

	if !r.config.DisableANSI {
		sb.WriteString(applyCliColorsToDiff(metaDiff))
	} else {
		sb.WriteString(metaDiff)
	}

	sb.WriteString("\n")
	sb.WriteString("=== CHANGES WITHIN THE RESPONSE BODY ===\n")

	// Body size comparison (when body was skipped during recording)
	if test.Result.BodySizeResult.Expected != 0 || test.Result.BodySizeResult.Actual != 0 {
		if !test.Result.BodySizeResult.Normal {
			sb.WriteString(fmt.Sprintf("Body Size Mismatch:\n  Expected: %d bytes\n  Actual:   %d bytes\n\n",
				test.Result.BodySizeResult.Expected, test.Result.BodySizeResult.Actual))
		} else {
			sb.WriteString(fmt.Sprintf("Body Size Match: %d bytes (body was too large to store, size compared instead)\n\n",
				test.Result.BodySizeResult.Expected))
		}
	}

	// Body diffs
	for _, bodyResult := range test.Result.BodyResult {
		if bodyResult.Normal {
			continue
		}

		if bodyResult.Type == models.JSON || bodyResult.Type == models.GrpcData {
			if pkg.IsJSON([]byte(bodyResult.Expected)) && pkg.IsJSON([]byte(bodyResult.Actual)) {
				diff, err := GenerateTableDiff(bodyResult.Expected, bodyResult.Actual)
				if err == nil {
					if !r.config.DisableANSI {
						sb.WriteString(applyCliColorsToDiff(diff))
					} else {
						sb.WriteString(diff)
					}
					sb.WriteString("\n")
				} else {
					tmp := *r
					tmp.out = bufio.NewWriterSize(&writerAdapter{sb: sb}, 64<<10)
					_ = tmp.printDefaultBodyDiff(bodyResult)
					_ = tmp.out.Flush()
				}
				continue
			}
		}

		// Force the old compact format for non-JSON bodies (fast).
		diff := GeneratePlainOldNewDiff(bodyResult.Expected, bodyResult.Actual, bodyResult.Type)

		if !r.config.DisableANSI {
			sb.WriteString(applyCliColorsToDiff(diff))
		} else {
			sb.WriteString(diff)
		}
		sb.WriteString("\n\n")

	}
	// Dependency assertions, AFTER the response diffs. --full puts them here
	// too (the diff printer buffers and only flushes on Render), so the block
	// sits in the same place in both modes instead of moving depending on
	// whether --full was passed.
	renderDepResults(sb, test)

	sb.WriteString("\n--------------------------------------------------------------------\n")
	return nil
}

// renderUnmatchedCalls writes the structured mock-miss diagnostics
// (FailureInfo.UnmatchedCalls) for a test. Field-diff paths use the noise
// vocabulary, so the rendered output doubles as copy-paste material for
// test.globalNoise / spec.assertions.noise.
func renderUnmatchedCalls(sb *strings.Builder, test models.TestResult) {
	if len(test.FailureInfo.UnmatchedCalls) == 0 {
		return
	}
	sb.WriteString("=== OUTGOING CALLS WITH NO MATCHING MOCK (likely root cause) ===\n")
	// Set by any out-of-scope call below; the shared explanation is written
	// ONCE for the whole test after the loop, never once per call — see
	// models.OutOfScopeDestinationCauses.
	var outOfScope bool
	for _, uc := range test.FailureInfo.UnmatchedCalls {
		sb.WriteString(fmt.Sprintf("  [%s] %s", uc.Protocol, uc.ActualSummary))
		if uc.MatchPhase != "" {
			sb.WriteString(fmt.Sprintf(" (match stopped at: %s", uc.MatchPhase))
			if uc.CandidateCount > 0 {
				sb.WriteString(fmt.Sprintf(", %d candidate mock(s)", uc.CandidateCount))
			}
			sb.WriteString(")")
		}
		sb.WriteString("\n")
		// An upstream that nothing in the compared set targets has no
		// comparable "closest mock": the matcher picked the least-distant
		// mock in the pool, which belongs to a DIFFERENT host. Its field
		// diffs are worse than useless here — the paths in this block are
		// advertised as copy-pasteable noise configuration, so rendering them
		// invites the reader to noise a `path` difference between two
		// unrelated calls. The values stay on the UnmatchedCall for machine
		// consumers; only this human rendering drops them, and says so.
		if uc.DestinationScope == models.DestinationScopeNotInComparedSet {
			outOfScope = true
			// Name the destination. "this destination" only ever resolved
			// because the default NextSteps happens to spell the host out
			// two lines down, and a parser that supplies its own hint (via
			// WithNextSteps) removes that accident — leaving a line that
			// refers to a host it never prints.
			dest := uc.Destination
			if dest == "" {
				dest = "an unidentified upstream"
			}
			sb.WriteString(fmt.Sprintf("    no recorded mock in the compared set targets %s; closest-mock diff omitted (it would compare against a different upstream)\n", dest))
			if uc.NextSteps != "" {
				sb.WriteString(fmt.Sprintf("    next steps: %s\n", uc.NextSteps))
			}
			continue
		}
		if uc.ClosestMock != "" {
			sb.WriteString(fmt.Sprintf("    closest mock: %s\n", uc.ClosestMock))
		}
		if len(uc.FieldDiffs) > 0 {
			sb.WriteString("    field differences (paths are noise-config compatible):\n")
			for _, d := range uc.FieldDiffs {
				switch d.Kind {
				case models.DiffKindMissingInLive:
					sb.WriteString(fmt.Sprintf("      %s: recorded %q, absent in live call\n", d.Path, d.Expected))
				case models.DiffKindMissingInMock:
					sb.WriteString(fmt.Sprintf("      %s: live %q, absent in recording\n", d.Path, d.Actual))
				case models.DiffKindTypeChanged:
					sb.WriteString(fmt.Sprintf("      %s: type changed (recorded %s, live %s)\n", d.Path, d.Expected, d.Actual))
				default:
					sb.WriteString(fmt.Sprintf("      %s: recorded %q, live %q\n", d.Path, d.Expected, d.Actual))
				}
			}
		} else if uc.Diff != "" {
			sb.WriteString(fmt.Sprintf("    diff: %s\n", uc.Diff))
		}
		if uc.NextSteps != "" {
			sb.WriteString(fmt.Sprintf("    next steps: %s\n", uc.NextSteps))
		}
	}
	if outOfScope {
		sb.WriteString(models.RenderOutOfScopeDestinationCauses("  "))
		sb.WriteString("\n")
	}
	sb.WriteString("\n")
}

// renderDepResults writes the per-test dependency-assertion block
// (models.Result.DepResult) next to the response diffs, in the same
// expected-vs-actual language: one line per failed DepMetaResult, and a
// compact "consumed (presence only)" line for rows that held.
//
// GATED ON SOMETHING ACTUALLY BEING MISSING, not merely on the rows existing.
// The writer emits rows only for dependencies that went missing, so the two
// conditions coincide today — but a row set from any other producer must not
// summon an "all dependencies consumed" section under every FAILED test, which
// tells a human nothing they can act on. The proof that the assertion ran
// stays where a machine reads it (Result.DepsChecked / the NDJSON
// `dependencies_checked`); the human sees a block only when a dependency is
// missing, which is the case worth their attention.
//
// So text output is byte-identical to pre-slice-4 for every test that lost
// nothing. The formatter itself lives in pkg/models so the compact renderer
// and --full cannot drift apart.
func renderDepResults(sb *strings.Builder, test models.TestResult) {
	// The consumer window summary, when this test had one. Gated on a nil
	// pointer that only a Kind: Consumer result ever carries, so every HTTP
	// and gRPC report renders byte-identically to a build without it. It is
	// written BEFORE the rows and outside the missing-deps guard because
	// end_reason is meaningful even when every effect matched: a window that
	// closed on its backstop was not fully observed.
	sb.WriteString(test.Consumer.FormatConsumerRun())
	if !test.Result.HasMissingDeps() {
		return
	}
	sb.WriteString(models.FormatDepResults(test.Result.DepResult))
}

// depNoticeCounts counts, across a whole run, the tests that lost a recorded
// dependency and how many of those still PASSED. The passed count is the one
// that matters: those are the tests a green CI run currently says nothing
// about.
func depNoticeCounts(reports map[string]*models.TestReport) (total, passed int) {
	for _, rep := range reports {
		if rep == nil {
			continue
		}
		for _, t := range rep.Tests {
			if !t.Result.HasMissingDeps() {
				continue
			}
			total++
			if t.Status == models.TestStatusPassed {
				passed++
			}
		}
	}
	return total, passed
}

// writerAdapter lets us reuse a bufio.Writer on top of strings.Builder.
type writerAdapter struct{ sb *strings.Builder }

func (w *writerAdapter) Write(p []byte) (int, error) { return w.sb.Write(p) }

func (r *Report) printSingleTestReport(ctx context.Context, test models.TestResult) error {
	if r.config.Report.ShowFullBody {
		var sb strings.Builder
		if err := r.renderSingleFullBodyFailedTest(ctx, &sb, test); err != nil {
			return fmt.Errorf("failed to render full body test: %w", err)
		}
		if _, err := r.out.WriteString(sb.String()); err != nil {
			return fmt.Errorf("failed to write full body test to output: %w", err)
		}
		err := r.out.Flush()
		if err != nil {
			return fmt.Errorf("failed to flush output for full body test: %w", err)
		}
		return nil
	}

	// Non-full-body: unchanged
	var sb strings.Builder
	if err := r.renderSingleFailedTest(ctx, &sb, test); err != nil {
		return fmt.Errorf("failed to render test report: %w", err)
	}
	if _, err := r.out.WriteString(sb.String()); err != nil {
		return fmt.Errorf("failed to write test report to output: %w", err)
	}
	err := r.out.Flush()
	if err != nil {
		return fmt.Errorf("failed to flush output for test report: %w", err)
	}
	return nil
}

// renderSingleFullBodyFailedTest renders a single failed test in full-body mode into sb.
func (r *Report) renderSingleFullBodyFailedTest(ctx context.Context, sb *strings.Builder, test models.TestResult) error {
	// Write header via printer.Sprintf (no stdout)
	header := r.generateTestHeader(test, r.printer) // returns string via Sprintf already
	sb.WriteString(header)

	// Route DiffsPrinter output into this builder (no os.Stdout)
	localOut := &writerAdapter{sb: sb}
	logDiffs := matcherUtils.NewDiffsPrinterOut(localOut, test.Name)

	// status/header/body diffs
	if err := r.addStatusCodeDiffs(test, &logDiffs); err != nil {
		return fmt.Errorf("failed to add status code diffs: %w", err)
	}
	if err := r.addHeaderDiffs(test, &logDiffs); err != nil {
		return fmt.Errorf("failed to add header diffs: %w", err)
	}
	if err := r.addBodyDiffs(ctx, test, &logDiffs); err != nil {
		return fmt.Errorf("failed to add body diffs: %w", err)
	}

	// Render() is what actually flushes the buffered status/header/body diffs
	// into sb, so the dependency block has to be written AFTER it. Writing it
	// before put the block above every response diff in --full mode while the
	// compact renderer put it below them — same data, two positions.
	if err := logDiffs.Render(); err != nil {
		r.logger.Error("failed to render the diffs", zap.Error(err))
		return fmt.Errorf("failed to render diffs: %w", err)
	}

	// Dependency assertions sit after the body diffs, using the same
	// expected/actual vocabulary. No-op when the test carries no rows.
	renderDepResults(sb, test)

	sb.WriteString("\n--------------------------------------------------------------------\n")
	return nil
}

// createFormattedPrinter: use r.printer (initialized in New)
func (r *Report) generateTestHeader(test models.TestResult, printer *pp.PrettyPrinter) string {
	header := fmt.Sprintf("Testrun failed for %s/%s", test.Name, test.TestCaseID)

	// Add risk level if available and not NONE
	if test.FailureInfo.Risk != "" && test.FailureInfo.Risk != models.None {
		header += fmt.Sprintf(" [%s-RISK]", test.FailureInfo.Risk)
	}

	// Add categories if available
	if len(test.FailureInfo.Category) > 0 {
		categories := make([]string, len(test.FailureInfo.Category))
		for i, cat := range test.FailureInfo.Category {
			categories[i] = string(cat)
		}
		header += fmt.Sprintf(" [%s]", strings.Join(categories, ", "))
	}

	return printer.Sprintf(header + "\n")
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
func (r *Report) addBodyDiffs(_ context.Context, test models.TestResult, logDiffs *matcherUtils.DiffsPrinter) error {
	// Handle body size comparison result (when body was skipped during recording)
	if test.Result.BodySizeResult.Expected != 0 || test.Result.BodySizeResult.Actual != 0 {
		if !test.Result.BodySizeResult.Normal {
			logDiffs.PushBodyDiff(
				fmt.Sprintf("body_size: %d bytes", test.Result.BodySizeResult.Expected),
				fmt.Sprintf("body_size: %d bytes", test.Result.BodySizeResult.Actual),
				nil,
			)
		}
	}

	for _, bodyResult := range test.Result.BodyResult {
		if !bodyResult.Normal {
			actualValue, err := r.renderTemplateValue(bodyResult.Actual)
			if err != nil {
				return fmt.Errorf("failed to render actual body value: %w", err)
			}

			expectedValue, err := r.renderTemplateValue(bodyResult.Expected)
			if err != nil {
				return fmt.Errorf("failed to render expected body value: %w", err)
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
		return fmt.Errorf("failed to render actual value for default body diff: %w", err)
	}

	expectedValue, err := r.renderTemplateValue(bodyResult.Expected)
	if err != nil {
		return fmt.Errorf("failed to render expected value for default body diff: %w", err)
	}

	logDiffs.PushBodyDiff(fmt.Sprint(expectedValue), fmt.Sprint(actualValue), nil)

	if err := logDiffs.Render(); err != nil {
		r.logger.Error("failed to render the default body diffs", zap.Error(err))
		return fmt.Errorf("failed to render default body diffs: %w", err)
	}
	return nil
}
