package test

import (
	"go.keploy.io/server/pkg/hooks"
	"go.keploy.io/server/pkg/models"
	"go.keploy.io/server/pkg/platform"
	"go.keploy.io/server/pkg/platform/yaml"
)

type Tester interface {
	// Test(tcsPath, mockPath, testReportPath string, appCmd, appContainer, networkName string, Delay uint64) bool
	Test(path, testReportPath string, appCmd, appContainer, networkName string, Delay uint64, passThorughPorts []uint, apiTimeout uint64) bool
	RunTestSet(testSet, path, testReportPath, appCmd, appContainer, appNetwork string, delay uint64, pid uint32, ys platform.TestCaseDB, loadedHook *hooks.Hook, testReportfs yaml.TestReportFS, testRunChan chan string, apiTimeout uint64) models.TestRunStatus
}
