package test

import (
	"time"

	"go.keploy.io/server/pkg/models"
)

type Tester interface {
	Test(path string, testReportPath string, appCmd string, options TestOptions) bool
	RunTestSet(testSet, path, testReportPath, appCmd, appContainer, appNetwork string, delay uint64, buildDelay time.Duration, pid uint32, testRunChan chan string, apiTimeout uint64, testcases map[string]bool, noiseConfig models.GlobalNoise, serveTest bool, testPath InitialiseTestReturn) models.TestRunStatus
	InitialiseTest(cfg *TestConfig) (InitialiseTestReturn, error)
	InitialiseRunTestSet(cfg *RunTestSetConfig) InitialiseRunTestSetReturn
	SimulateRequest(cfg *SimulateRequestConfig)
	FetchTestResults(cfg *FetchTestResultsConfig) models.TestRunStatus
}
