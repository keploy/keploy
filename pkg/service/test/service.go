package test

import (
	"context"

	"go.keploy.io/server/pkg/hooks"
	"go.keploy.io/server/pkg/models"
	"go.keploy.io/server/pkg/platform"
	"go.keploy.io/server/pkg/platform/yaml"
)

type Tester interface {
	Test(path string, proxyPort uint32, testReportPath string, appCmd string, testsets []string, appContainer, networkName string, Delay uint64, passThorughPorts []uint, apiTimeout uint64, globalNoise models.GlobalNoise, testsetNoise models.TestsetNoise) bool
	RunTestSet(testSet, path, testReportPath, appCmd, appContainer, appNetwork string, delay uint64, pid uint32, ys platform.TestCaseDB, loadedHook *hooks.Hook, testReportfs yaml.TestReportFS, testRunChan chan string, apiTimeout uint64, ctx context.Context, noiseConfig models.GlobalNoise, serveTest bool) models.TestRunStatus
}
