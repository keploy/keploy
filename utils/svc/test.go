// Package svc: This file contains the interface for the test utils.
package svc

import (
	"context"
	"fmt"

	"go.keploy.io/server/v2/pkg"
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

var testUtils TestUtils

type SimOptions struct {
	CmdType    utils.CmdType
	APITimeout uint64
}

type TestUtils interface {
	SimulateRequest(ctx context.Context, appID uint64, tc *models.TestCase, testSetID string, options SimOptions) (*models.HTTPResp, error)
}

type Test struct {
	logger *zap.Logger
}

func NewTestUtils(logger *zap.Logger) TestUtils {
	return &Test{
		logger: logger,
	}
}

func SetTestUtilInstance(instance TestUtils) {
	fmt.Println("Setting test utils")
	fmt.Printf("Instance: %v\n", instance)
	testUtils = instance
}

func GetTestUtilInstance() TestUtils {
	fmt.Println("Getting test utils")
	fmt.Printf("Instance: %v\n", testUtils)
	return testUtils
}

func (t *Test) SimulateRequest(ctx context.Context, _ uint64, tc *models.TestCase, testSetID string, opts SimOptions) (*models.HTTPResp, error) {
	fmt.Println("OSS SIMULAte")
	switch tc.Kind {
	case models.HTTP:
		t.logger.Debug("Before simulating the request", zap.Any("Test case", tc))
		t.logger.Debug(fmt.Sprintf("the url of the testcase: %v", tc.HTTPReq.URL))
		resp, err := pkg.SimulateHTTP(ctx, *tc, testSetID, t.logger, opts.APITimeout)
		t.logger.Debug("After simulating the request", zap.Any("test case id", tc.Name))
		t.logger.Debug("After GetResp of the request", zap.Any("test case id", tc.Name))
		return resp, err
	}
	return nil, nil
}
