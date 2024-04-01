package replay

import (
	"context"
	"fmt"
	"net/url"

	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/pkg"
	"go.keploy.io/server/v2/pkg/models"
	"go.uber.org/zap"
)

type TestReportVerdict struct {
	total   int
	passed  int
	failed  int
	ignored int
	status  bool
}

type Range struct {
	Start int
	End   int
}

func LeftJoinNoise(globalNoise config.GlobalNoise, tsNoise config.GlobalNoise) config.GlobalNoise {
	noise := globalNoise

	if _, ok := noise["body"]; !ok {
		noise["body"] = make(map[string][]string)
	}
	if tsNoiseBody, ok := tsNoise["body"]; ok {
		for field, regexArr := range tsNoiseBody {
			noise["body"][field] = regexArr
		}
	}

	if _, ok := noise["header"]; !ok {
		noise["header"] = make(map[string][]string)
	}
	if tsNoiseHeader, ok := tsNoise["header"]; ok {
		for field, regexArr := range tsNoiseHeader {
			noise["header"][field] = regexArr
		}
	}

	return noise
}

// ReplaceBaseURL replaces the baseUrl of the old URL with the new URL's.
func ReplaceBaseURL(newURL, oldURL string) (string, error) {
	parsedOldURL, err := url.Parse(oldURL)
	if err != nil {
		return "", fmt.Errorf("failed to parse the old URL: %v", err)
	}

	parsedNewURL, err := url.Parse(newURL)
	if err != nil {
		return "", fmt.Errorf("failed to parse the new URL: %v", err)
	}
	// if scheme is empty, then add the scheme from the old URL in order to parse it correctly
	if parsedNewURL.Scheme == "" {
		parsedNewURL.Scheme = parsedOldURL.Scheme
		parsedNewURL, err = url.Parse(parsedNewURL.String())
		if err != nil {
			return "", fmt.Errorf("failed to parse the scheme added new URL: %v", err)
		}
	}

	parsedOldURL.Scheme = parsedNewURL.Scheme
	parsedOldURL.Host = parsedNewURL.Host
	path, err := url.JoinPath(parsedNewURL.Path, parsedOldURL.Path)
	if err != nil {
		return "", fmt.Errorf("failed to join '%v' and '%v' paths: %v", parsedNewURL.Path, parsedOldURL.Path, err)
	}
	parsedOldURL.Path = path

	replacedURL := parsedOldURL.String()
	return replacedURL, nil
}

type requestMockUtil struct {
	logger     *zap.Logger
	path       string
	mockName   string
	apiTimeout uint64
	basePath   string
}

func NewRequestMockUtil(logger *zap.Logger, path, mockName string, apiTimeout uint64, basePath string) RequestMockHandler {
	return &requestMockUtil{
		path:       path,
		logger:     logger,
		mockName:   mockName,
		apiTimeout: apiTimeout,
		basePath:   basePath,
	}
}
func (t *requestMockUtil) SimulateRequest(ctx context.Context, _ uint64, tc *models.TestCase, testSetID string) (*models.HTTPResp, error) {
	switch tc.Kind {
	case models.HTTP:
		t.logger.Debug("Before simulating the request", zap.Any("Test case", tc))
		resp, err := pkg.SimulateHTTP(ctx, *tc, testSetID, t.logger, t.apiTimeout)
		t.logger.Debug("After simulating the request", zap.Any("test case id", tc.Name))
		return resp, err
	}
	return nil, nil
}

func (t *requestMockUtil) AfterTestHook(_ context.Context, testRunID, testSetID string, coverage models.TestCoverage, tsCnt int) (*models.TestReport, error) {
	t.logger.Debug("AfterTestHook", zap.Any("testRunID", testRunID), zap.Any("testSetID", testSetID), zap.Any("totalTestSetCount", tsCnt), zap.Any("coverage", coverage))
	return nil, nil
}

func (t *requestMockUtil) ProcessTestRunStatus(_ context.Context, status bool, testSetID string) {
	if status {
		t.logger.Debug("Test case passed for", zap.String("testSetID", testSetID))
	} else {
		t.logger.Debug("Test case failed for", zap.String("testSetID", testSetID))
	}
}

func (t *requestMockUtil) FetchMockName() string {
	return t.mockName
}

func (t *requestMockUtil) ProcessMockFile(_ context.Context, testSetID string) {
	if t.basePath != "" {
		t.logger.Debug("Mocking is disabled when basePath is given", zap.String("testSetID", testSetID), zap.String("basePath", t.basePath))
		return
	}
	t.logger.Debug("Mock file for test set", zap.String("testSetID", testSetID))
}

func mergeMaps(map1, map2 map[string][]string) map[string][]string {
	for key, values := range map2 {
		if _, exists := map1[key]; exists {
			map1[key] = append(map1[key], values...)
		} else {
			map1[key] = values
		}
	}
	return map1
}

func removeFromMap(map1, map2 map[string][]string) map[string][]string {
	for key := range map2 {
		delete(map1, key)
	}
	return map1
}
