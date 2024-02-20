package graph

import (
	"go.keploy.io/server/v2/pkg/core/hooks"
	"go.keploy.io/server/v2/pkg/platform"
	"go.keploy.io/server/v2/pkg/service/replay"
	"go.uber.org/zap"
)

// This file will not be regenerated automatically.
//
// It serves as dependency injection for your app, add any dependencies you require here.
var Emoji = "\U0001F430" + " Keploy:"

type Resolver struct {
	Tester         replay.Tester
	TestReportFS   platform.TestReportDB
	Storage        platform.TestCaseDB
	LoadedHooks    *hooks.Hook
	Logger         *zap.Logger
	Path           string
	TestReportPath string
	Delay          uint64
	AppPid         uint32
	ApiTimeout     uint64
	ServeTest      bool
}
