package graph

import (
	"go.keploy.io/server/pkg/hooks"
	"go.keploy.io/server/pkg/platform"
	"go.keploy.io/server/pkg/platform/yaml"
	"go.keploy.io/server/pkg/service/test"
	"go.uber.org/zap"
	"time"
)

// This file will not be regenerated automatically.
//
// It serves as dependency injection for your app, add any dependencies you require here.
var Emoji = "\U0001F430" + " Keploy:"

type Resolver struct {
	Tester         test.Tester
	TestReportFS   yaml.TestReportFS
	YS             platform.TestCaseDB
	LoadedHooks    *hooks.Hook
	Logger         *zap.Logger
	Path           string
	TestReportPath string
	Delay          uint64
	BuildDelay     time.Duration
	AppPid         uint32
	ApiTimeout     uint64
	ServeTest      bool
}
