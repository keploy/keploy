package test

import (
	"context"
	"time"

	"go.keploy.io/server/pkg/hooks"
	"go.keploy.io/server/pkg/models"
	"go.keploy.io/server/pkg/platform"
)

type Tester interface {
	Test(path string, testReportPath string, appCmd string, options TestOptions, enableTele ,mockAssert bool) bool
	RunTestSet(testSet, path, testReportPath, appCmd, appContainer, appNetwork string, delay uint64, buildDelay time.Duration, pid uint32, ys platform.TestCaseDB, loadedHook *hooks.Hook, testReportfs platform.TestReportDB, testRunChan chan string, apiTimeout uint64, ctx context.Context, testcases map[string]bool, noiseConfig models.GlobalNoise, serveTest bool) models.TestRunStatus
	InitialiseTest(cfg *TestConfig) (InitialiseTestReturn, error)
	InitialiseRunTestSet(cfg *RunTestSetConfig) InitialiseRunTestSetReturn
	SimulateRequest(cfg *SimulateRequestConfig)
	FetchTestResults(cfg *FetchTestResultsConfig) models.TestRunStatus
}
