package test

import (
	"context"

	"go.keploy.io/server/pkg/hooks"
	"go.keploy.io/server/pkg/models"
	"go.keploy.io/server/pkg/platform"
	"go.keploy.io/server/pkg/platform/yaml"
)

type Tester interface {
	Test(path string, proxyPort uint32, testReportPath string, appCmd string, testsets []string, appContainer, networkName string, Delay uint64, passThorughPorts []uint, apiTimeout uint64) bool
	RunTestSet(testSet, path, testReportPath, appCmd, appContainer, appNetwork string, delay uint64, pid uint32, ys platform.TestCaseDB, loadedHook *hooks.Hook, testReportfs yaml.TestReportFS, testRunChan chan string, apiTimeout uint64, ctx context.Context)models.TestRunStatus
	InitialiseTest(path string, proxyPort uint32, testReportPath string, appCmd string, testsets *[]string, appContainer, appNetwork string, Delay uint64, passThorughPorts []uint, apiTimeout uint64) InitialiseTestReturn
	InitialiseRunTestSet(testSet, path, testReportPath, appCmd, appContainer, appNetwork string, delay uint64, pid uint32, ys platform.TestCaseDB, loadedHooks *hooks.Hook, testReportFS yaml.TestReportFS, testRunChan chan string, apiTimeout uint64, ctx context.Context) InitialiseRunTestSetReturn
	SimulateRequest(tc *models.TestCase, loadedHooks *hooks.Hook, appCmd string, userIp string, testSet string, apiTimeout uint64, success, failure *int, status *models.TestRunStatus, testReportFS yaml.TestReportFS, testReport *models.TestReport, path string, dIDE bool)
	FetchTestResults(testReportFS yaml.TestReportFS, testReport *models.TestReport, status *models.TestRunStatus, testSet string, success, failure *int, ctx context.Context, testReportPath, path string) models.TestRunStatus
}
