package graph

import (
	"go.keploy.io/server/pkg/hooks"
	"go.keploy.io/server/pkg/platform"
	"go.keploy.io/server/pkg/service/test"
	"go.uber.org/zap"
)

// This file will not be regenerated automatically.
//
// It serves as dependency injection for your app, add any dependencies you require here.
var Emoji = "\U0001F430" + " Keploy:"

type Resolver struct {
	Tester             test.Tester
	TestReportFS       platform.TestReportDB
	Storage            platform.TestCaseDB
	LoadedHooks        *hooks.Hook
	Logger             *zap.Logger
	Path               string
	TestReportPath     string
	GenerateTestReport bool
	Delay              uint64
	AppPid             uint32
	ApiTimeout         uint64
	ServeTest          bool
}
