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
}

func New(logger *zap.Logger, cfg *config.Config, reportDB ReportDB) *Report {
	return &Report{
		logger:   logger,
		config:   cfg,
		reportDB: reportDB,
	}
}

func (r *Report) GenerateReport(ctx context.Context) error {

	var testSetIDs []string

	for testSet := range r.config.Report.SelectedTestSets {
		testSetIDs = append(testSetIDs, strings.TrimSpace(testSet))
	}

	if len(testSetIDs) == 0 {
		r.logger.Error("No test sets selected for report generation")
		return nil
	}

	testRunIDs, err := r.reportDB.GetAllTestRunIDs(ctx)
	if err != nil {
		r.logger.Error("failed to get all test run ids", zap.Error(err))
		return err
	}

	fmt.Println("All the test run ids:", testRunIDs)

	if len(testRunIDs) == 0 {
		r.logger.Warn("no test runs found")
		return nil
	}

	latestRunID := ""
	if len(testRunIDs) > 0 {
		sort.Slice(testRunIDs, func(i, j int) bool {
			numi, erri := strconv.Atoi(strings.TrimPrefix(testRunIDs[i], "test-run-"))
			numj, errj := strconv.Atoi(strings.TrimPrefix(testRunIDs[j], "test-run-"))
			if erri != nil || errj != nil {
				return testRunIDs[i] < testRunIDs[j]
			}
			return numi < numj
		})
		latestRunID = testRunIDs[len(testRunIDs)-1]
	}

	fmt.Println("Latest test run id:", latestRunID)

	var failedTests []models.TestResult
	for _, testSetID := range testSetIDs {
		results, err := r.reportDB.GetReport(ctx, latestRunID, testSetID)
		if err != nil {
			r.logger.Error("failed to get test case results for test set", zap.String("test_set_id", testSetID), zap.Error(err))
			continue
		}
		if results == nil {
			r.logger.Warn("no results found for test set", zap.String("test_set_id", testSetID))
			continue
		}

		for _, result := range results.Tests {
			if result.Status == models.TestStatusFailed {
				failedTests = append(failedTests, result)
			}
		}
	}

	if len(failedTests) == 0 {
		r.logger.Info("No failed tests found in the latest test run")
		return nil
	}

	for _, test := range failedTests {
		logDiffs := matcherUtils.NewDiffsPrinter(test.Name)

		newLogger := pp.New()
		newLogger.WithLineInfo = false
		newLogger.SetColorScheme(models.GetFailingColorScheme())
		var logs = ""

		logs = logs + newLogger.Sprintf("Testrun failed for testcase with id: %s\n\n--------------------------------------------------------------------\n\n", test.TestCaseID)

		if !test.Result.StatusCode.Normal {
			logDiffs.PushStatusDiff(fmt.Sprint(test.Result.StatusCode.Expected), fmt.Sprint(test.Result.StatusCode.Actual))
		}

		for _, j := range test.Result.HeadersResult {
			if !j.Normal {
				actualValue := strings.Join(j.Actual.Value, ", ")
				expectedValue := strings.Join(j.Expected.Value, ", ")
				logDiffs.PushHeaderDiff(expectedValue, actualValue, j.Actual.Key, nil)
			}
		}

		for _, j := range test.Result.BodyResult {
			if !j.Normal {
				_, actualValue, err := tools.RenderIfTemplatized(j.Actual)
				if err != nil {
					r.logger.Error("failed to render the actual body", zap.Error(err))
					return err
				}
				_, expectedValue, err := tools.RenderIfTemplatized(j.Expected)
				if err != nil {
					r.logger.Error("failed to render the expected body", zap.Error(err))
					return err
				}
				logDiffs.PushBodyDiff(fmt.Sprint(expectedValue), fmt.Sprint(actualValue), nil)
			}
		}

		_, err := newLogger.Printf(logs)
		if err != nil {
			r.logger.Error("failed to print the logs", zap.Error(err))
		}

		err = logDiffs.Render()
		if err != nil {
			r.logger.Error("failed to render the diffs", zap.Error(err))
		}
	}

	return nil
}
