package test

import (
	"time"

	"go.keploy.io/server/pkg/models"
	"go.keploy.io/server/pkg/platform"
	"go.keploy.io/server/pkg/platform/telemetry"
)

type Tester interface {
	Test(path string, testReportPath string, appCmd string, options TestOptions, tele *telemetry.Telemetry, testReportStorage platform.TestReportDB, tcsStorage platform.TestCaseDB, EnableColor *bool) bool
	StartTest(path string, testReportPath string, appCmd string, options TestOptions, enableTele bool) bool
	RunTestSet(testSet, path, testReportPath, appCmd, appContainer, appNetwork string, delay uint64, buildDelay time.Duration, pid uint32, testRunChan chan string, apiTimeout uint64, testcases map[string]bool, noiseConfig models.GlobalNoise, serveTest bool, EnableColor bool, testEnv TestEnvironmentSetup) models.TestRunStatus
	InitialiseTest(cfg *TestConfig) (TestEnvironmentSetup, error)
	InitialiseRunTestSet(cfg *RunTestSetConfig) InitialiseRunTestSetReturn
	SimulateRequest(cfg *SimulateRequestConfig)
	FetchTestResults(cfg *FetchTestResultsConfig) models.TestRunStatus
}
