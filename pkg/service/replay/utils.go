package replay

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/pkg"
	"go.keploy.io/server/v2/pkg/models"
	"go.uber.org/zap"
)

type TestReportVerdict struct {
	total  int
	passed int
	failed int
	status bool
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

func replaceHostToIP(currentURL string, ipAddress string) (string, error) {
	// Parse the current URL
	parsedURL, err := url.Parse(currentURL)

	if err != nil {
		// Return the original URL if parsing fails
		return currentURL, err
	}

	if ipAddress == "" {
		return currentURL, fmt.Errorf("failed to replace url in case of docker env")
	}

	// Replace hostname with the IP address
	parsedURL.Host = strings.Replace(parsedURL.Host, parsedURL.Hostname(), ipAddress, 1)
	// Return the modified URL
	return parsedURL.String(), nil
}

type testUtils struct {
	logger     *zap.Logger
	apiTimeout uint64
}

func NewTestUtils(apiTimeout uint64, logger *zap.Logger) RequestEmulator {
	return &testUtils{
		logger:     logger,
		apiTimeout: apiTimeout,
	}
}

func (t *testUtils) SimulateRequest(ctx context.Context, _ uint64, tc *models.TestCase, testSetID string) (*models.HTTPResp, error) {
	switch tc.Kind {
	case models.HTTP:
		t.logger.Debug("Before simulating the request", zap.Any("Test case", tc))
		t.logger.Debug(fmt.Sprintf("the url of the testcase: %v", tc.HTTPReq.URL))
		resp, err := pkg.SimulateHTTP(ctx, *tc, testSetID, t.logger, t.apiTimeout)
		t.logger.Debug("After simulating the request", zap.Any("test case id", tc.Name))
		t.logger.Debug("After GetResp of the request", zap.Any("test case id", tc.Name))
		return resp, err
	}
	return nil, nil
}
