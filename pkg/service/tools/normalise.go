package tools

import (
	context "context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/pkg"
	matcherUtils "go.keploy.io/server/v2/pkg/matcher"
	models "go.keploy.io/server/v2/pkg/models"
	"go.uber.org/zap"
)

type lastUpdatedContextKey struct{}

func (t *Tools) Normalize(ctx context.Context) error {
	ctx = t.addLastUpdatedToContext(ctx)

	var testRun string
	if t.config.Normalize.TestRun == "" {
		testRunIDs, err := t.reportDB.GetAllTestRunIDs(ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return err
			}
			return fmt.Errorf("failed to get all test run ids: %w", err)
		}
		testRun = pkg.LastID(testRunIDs, models.TestRunTemplateName)
	}

	if len(t.config.Normalize.SelectedTests) == 0 {
		testSetIDs, err := t.testDB.GetAllTestSetIDs(ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return err
			}
			return fmt.Errorf("failed to get all test set ids: %w", err)
		}
		for _, testSetID := range testSetIDs {
			t.config.Normalize.SelectedTests = append(t.config.Normalize.SelectedTests, config.SelectedTests{TestSet: testSetID})
		}
	}

	for _, testSet := range t.config.Normalize.SelectedTests {
		testSetID := testSet.TestSet
		testCases := testSet.Tests

		// Check if test set is sanitized (has secret.yaml)
		// If yes, desanitize before normalization
		desanitized, err := t.DesanitizeTestSet(testSetID, t.config.Path)
		if err != nil {
			t.logger.Error("Failed to desanitize test set before normalization",
				zap.String("testSetID", testSetID),
				zap.Error(err))
			return fmt.Errorf("failed to desanitize test set %s: %w", testSetID, err)
		}
		if desanitized {
			t.logger.Info("Desanitized test set before normalization",
				zap.String("testSetID", testSetID))
		}

		// Normalize test cases
		err = t.NormalizeTestCases(ctx, testRun, testSetID, testCases, nil)
		if err != nil {
			return err
		}

		// Re-sanitize after normalization if it was originally sanitized
		if desanitized {
			testSetDir := filepath.Join(t.config.Path, testSetID)
			err = t.SanitizeTestSetDir(ctx, testSetDir)
			if err != nil {
				t.logger.Error("Failed to re-sanitize test set after normalization",
					zap.String("testSetID", testSetID),
					zap.Error(err))
				return fmt.Errorf("failed to re-sanitize test set %s: %w", testSetID, err)
			}
			t.logger.Info("Re-sanitized test set after normalization",
				zap.String("testSetID", testSetID))
		}
	}
	t.logger.Info("Normalized test cases successfully. Please run keploy tests to verify the changes.")
	return nil
}

func (t *Tools) addLastUpdatedToContext(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}

	if t == nil || t.config == nil {
		return ctx
	}

	editedBy := strings.TrimSpace(t.config.Normalize.EditedBy)
	if editedBy == "" {
		return ctx
	}

	return context.WithValue(ctx, lastUpdatedContextKey{}, models.LastUpdated{
		Author:    editedBy,
		Timestamp: time.Now().UTC(),
	})
}

func (t *Tools) NormalizeTestCases(ctx context.Context, testRun string, testSetID string, selectedTestCaseIDs []string, testCaseResults []models.TestResult) error {
	if ctx == nil {
		ctx = context.Background()
	}

	lastUpdated, hasLastUpdated := t.resolveLastUpdated(ctx)

	if len(testCaseResults) == 0 {
		testReport, err := t.reportDB.GetReport(ctx, testRun, testSetID)
		if err != nil {
			return fmt.Errorf("failed to get test report: %w", err)
		}
		testCaseResults = testReport.Tests
	}

	testCaseResultMap := make(map[string]models.TestResult)
	testCases, err := t.testDB.GetTestCases(ctx, testSetID)
	if err != nil {
		return fmt.Errorf("failed to get test cases: %w", err)
	}
	selectedTestCases := make([]*models.TestCase, 0, len(selectedTestCaseIDs))

	if len(selectedTestCaseIDs) == 0 {
		selectedTestCases = testCases
	} else {
		for _, testCase := range testCases {
			if _, ok := matcherUtils.ArrayToMap(selectedTestCaseIDs)[testCase.Name]; ok {
				selectedTestCases = append(selectedTestCases, testCase)
			}
		}
	}

	for _, testCaseResult := range testCaseResults {
		testCaseResultMap[testCaseResult.TestCaseID] = testCaseResult
	}

	for _, testCase := range selectedTestCases {
		testCaseResult, ok := testCaseResultMap[testCase.Name]
		if !ok {
			t.logger.Info("test case not found in the test report", zap.String("test-case-id", testCase.Name), zap.String("test-set-id", testSetID))
			continue
		}
		if testCaseResult.Status == models.TestStatusPassed {
			continue
		}
		if testCaseResult.FailureInfo.Risk == models.High && !t.config.Normalize.AllowHighRisk {
			t.logger.Warn(fmt.Sprintf("failed to normalize test case %s due to a high-risk failure. please confirm the schema compatibility with all consumers and then run with --allow-high-risk", testCase.Name))
			continue
		}
		if hasLastUpdated {
			testCase.LastUpdated = &models.LastUpdated{
				Author:    lastUpdated.Author,
				Timestamp: lastUpdated.Timestamp,
			}
		}
		// Handle normalization based on test case kind
		switch testCase.Kind {
		case models.HTTP:
			// Store the original timestamp to preserve it during normalization
			originalTimestamp := testCase.HTTPResp.Timestamp
			testCase.HTTPResp = testCaseResult.Res
			// Restore the original timestamp after normalization
			testCase.HTTPResp.Timestamp = originalTimestamp

		case models.GRPC_EXPORT:
			// Store the original timestamp to preserve it during normalization
			originalTimestamp := testCase.GrpcResp.Timestamp
			testCase.GrpcResp = testCaseResult.GrpcRes
			// Restore the original timestamp after normalization
			testCase.GrpcResp.Timestamp = originalTimestamp
		}
		err = t.testDB.UpdateTestCase(ctx, testCase, testSetID, true)
		if err != nil {
			return fmt.Errorf("failed to update test case: %w", err)
		}
	}
	return nil
}

func (t *Tools) resolveLastUpdated(ctx context.Context) (models.LastUpdated, bool) {
	lastUpdated, ok := ctx.Value(lastUpdatedContextKey{}).(models.LastUpdated)
	if ok {
		lastUpdated.Author = strings.TrimSpace(lastUpdated.Author)
		if lastUpdated.Author != "" && !lastUpdated.Timestamp.IsZero() {
			lastUpdated.Timestamp = lastUpdated.Timestamp.UTC()
			return lastUpdated, true
		}
	}

	if t == nil || t.config == nil {
		return models.LastUpdated{}, false
	}

	editedBy := strings.TrimSpace(t.config.Normalize.EditedBy)
	if editedBy == "" {
		return models.LastUpdated{}, false
	}

	return models.LastUpdated{
		Author:    editedBy,
		Timestamp: time.Now().UTC(),
	}, true
}
