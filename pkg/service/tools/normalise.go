package tools

import (
	context "context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg"
	matcherUtils "go.keploy.io/server/v3/pkg/matcher"
	models "go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/templates"
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

	// The set's recorded template values: a digest header kept with a
	// placeholder is written as the digest of the body at these values, the
	// body every later run's digest rule starts from (templates.Run.Keep). A
	// program may hand NewTools no config store (k8s-proxy's normalize does):
	// the set then has no template values, as the replay reads it.
	var tmpl, secret map[string]interface{}
	if t.testSetConf != nil {
		if conf, err := t.testSetConf.Read(ctx, testSetID); err == nil && conf != nil {
			tmpl, secret = conf.Template, conf.Secret
		}
	}
	tmplRun := templates.NewRun(t.logger, tmpl, secret)

	for _, testCase := range selectedTestCases {
		testCaseResult, ok := testCaseResultMap[testCase.Name]
		if !ok {
			t.logger.Info("test case not found in the test report", zap.String("test-case-id", testCase.Name), zap.String("test-set-id", testSetID))
			continue
		}
		if testCaseResult.Status == models.TestStatusPassed || testCaseResult.Status == models.TestStatusObsolete {
			continue
		}
		if wasRefused(testCaseResult) {
			// Never sent: there is no answer to accept, --allow-high-risk
			// or not.
			t.logger.Info("not normalizing a test case refused when its test set loaded: it was never sent, so there is no answer to accept",
				zap.String("test-case-id", testCase.Name), zap.String("test-set-id", testSetID),
				zap.String("next_step", "edit the test case's placeholder as the reason in its report says, then run keploy test again"))
			continue
		}
		if testCaseResult.FailureInfo.Risk == models.High && !t.config.Normalize.AllowHighRisk {
			if len(testCaseResult.TemplateFailures) > 0 {
				// A template variable's place the answer did not hold: what
				// to write is the user's call, never this run's value.
				for _, f := range testCaseResult.TemplateFailures {
					t.logger.Info("not normalizing a test case that failed on a template variable",
						zap.String("test-case-id", testCase.Name), zap.String("test-set-id", testSetID),
						zap.String("where", f.Where), zap.String("placeholder", f.Placeholder), zap.String("why", f.Why),
						zap.String("next_step", "if the app issues this value anew each run, make the place that issues it a {{capture ...}}; if the value changed on purpose, edit the expected value or re-record the test case. --allow-high-risk writes this run's value over the placeholder, and the next run fails on it"))
				}
				continue
			}
			t.logger.Info(fmt.Sprintf("failed to normalize test case %s due to a high-risk failure. please confirm the schema compatibility with all consumers and then run with --allow-high-risk", testCase.Name))
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
			// The app's answer, with each placeholder of the expected
			// response put back where the run resolved it to exactly what
			// the app answered. A test case with no placeholder gets the
			// answer as it is.
			accepted, dropped := tmplRun.Keep(testCase.HTTPResp, resolvedExpected(testCaseResult.Result), testCaseResult.Res)
			for _, d := range dropped {
				t.logger.Warn("keploy normalize wrote the app's answer where a placeholder was: the run resolved it to another value than the app answered",
					zap.String("test-case-id", testCase.Name), zap.String("test-set-id", testSetID), zap.String("placeholder", d),
					zap.String("next_step", "if the value should still follow a template key, put the placeholder back in the test case by hand"))
			}
			testCase.HTTPResp = accepted
			// Restore the original timestamp after normalization
			testCase.HTTPResp.Timestamp = originalTimestamp

		case models.GRPC_EXPORT:
			// Store the original timestamp to preserve it during normalization
			originalTimestamp := testCase.GrpcResp.Timestamp
			// A gRPC expected response is written as the app's answer: each
			// placeholder it held is dropped, and said so.
			for _, d := range grpcPlaceholders(testCase.GrpcResp) {
				t.logger.Warn("keploy normalize wrote the app's gRPC answer where a placeholder was: a gRPC expected response is written as the answer",
					zap.String("test-case-id", testCase.Name), zap.String("test-set-id", testSetID), zap.String("placeholder", d),
					zap.String("next_step", "if the value should still follow a template key, put the placeholder back in the test case by hand"))
			}
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

// wasRefused reports whether a test case was refused when its set loaded,
// and so never sent.
func wasRefused(r models.TestResult) bool {
	for _, f := range r.TemplateFailures {
		if f.Refused {
			return true
		}
	}
	return false
}

// grpcPlaceholders names each placeholder of a gRPC expected response, by
// where it is.
func grpcPlaceholders(resp models.GrpcResp) []string {
	var out []string
	add := func(where, text string) {
		for _, p := range templates.Find(text) {
			out = append(out, where+": "+p.Text)
		}
	}
	for _, h := range []struct {
		where string
		m     map[string]string
	}{
		{"pseudo header ", resp.Headers.PseudoHeaders},
		{"header ", resp.Headers.OrdinaryHeaders},
		{"trailer pseudo header ", resp.Trailers.PseudoHeaders},
		{"trailer ", resp.Trailers.OrdinaryHeaders},
	} {
		for _, k := range sortedNames(h.m) {
			add(h.where+k, h.m[k])
		}
	}
	add("body", resp.Body.DecodedData)
	for i, m := range resp.AdditionalMessages {
		add(fmt.Sprintf("message %d", i+1), m.DecodedData)
	}
	return out
}

// resolvedExpected is the expected response a run compared with, as its
// report holds it: each placeholder at the value the run resolved it to.
func resolvedExpected(result models.Result) models.HTTPResp {
	out := models.HTTPResp{Header: map[string]string{}}
	if len(result.BodyResult) > 0 {
		out.Body = result.BodyResult[0].Expected
	}
	for _, h := range result.HeadersResult {
		if h.Expected.Key != "" && h.Expected.Value != nil {
			out.Header[h.Expected.Key] = strings.Join(h.Expected.Value, ",")
		}
	}
	return out
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
