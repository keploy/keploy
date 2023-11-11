package test

import (
	"context"

	"go.keploy.io/server/pkg/hooks"
	"go.keploy.io/server/pkg/models"
	"go.keploy.io/server/pkg/platform"
	"go.keploy.io/server/pkg/platform/yaml"
)

type Tester interface {
	Test(path, testReportPath string, appCmd string, options TestOptions) bool
	RunTestSet(testSet, path, testReportPath, appCmd, appContainer, appNetwork string, delay uint64, pid uint32, ys platform.TestCaseDB, loadedHook *hooks.Hook, testReportfs yaml.TestReportFS, testRunChan chan string, apiTimeout uint64, ctx context.Context, testcases map[string]bool, noiseConfig models.GlobalNoise, serveTest bool) models.TestRunStatus
}
