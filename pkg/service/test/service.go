package test

import (
	"go.keploy.io/server/pkg/models"
)

type Tester interface {
	Test(path string, testReportPath string, appCmd string, mongoUri string, options TestOptions) bool
	RunTestSet(testSet, path, testReportPath, appCmd, appContainer, appNetwork string, delay uint64, pid uint32, ys InitialiseTestReturn, testRunChan chan string, apiTimeout uint64, noiseConfig models.GlobalNoise, serveTest bool) models.TestRunStatus
	InitialiseTest(cfg *TestConfig) (InitialiseTestReturn, error)
	InitialiseRunTestSet(cfg *RunTestSetConfig) InitialiseRunTestSetReturn
	SimulateRequest(cfg *SimulateRequestConfig)
	FetchTestResults(cfg *FetchTestResultsConfig) models.TestRunStatus
}
