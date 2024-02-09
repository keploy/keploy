package test

import (
	"context"
	"time"

	"go.keploy.io/server/pkg/hooks"
	"go.keploy.io/server/pkg/models"
	"go.keploy.io/server/pkg/platform"
	"go.keploy.io/server/pkg/platform/telemetry"
	"go.keploy.io/server/pkg/proxy"
)

type Tester interface {
	Start(ctx context.Context, cmd string, opt Options) bool
	RunTestSet(testSet, path, testReportPath, appCmd, appContainer, appNetwork string, delay uint64, buildDelay time.Duration, pid uint32, testRunChan chan string, apiTimeout uint64, testcases map[string]bool, noiseConfig models.GlobalNoise, serveTest bool, testEnv TestEnvironmentSetup) models.TestRunStatus
	InitTest(cfg *TestConfig) (TestEnvironmentSetup, error)
	InitRunTestSet(cfg *RunTestSetConfig) InitialiseRunTestSetReturn
	SimRequest(cfg *SimulateRequestConfig)
	FetchResults(cfg *FetchTestResultsConfig) models.TestRunStatus
}

type InitialiseRunTestSetReturn struct {
	Tcs           []*models.TestCase
	ErrChan       chan error
	TestReport    *models.TestReport
	DockerID      bool
	UserIP        string
	InitialStatus models.TestRunStatus
	TcsMocks      []*models.Mock
}

type TestEnvironmentSetup struct {
	Sessions                 []string
	TestReportFS             platform.TestReportDB
	Ctx                      context.Context
	AbortStopHooksForcefully bool
	ProxySet                 *proxy.ProxySet
	ExitCmd                  chan bool
	Storage                  platform.TestCaseDB
	LoadedHooks              *hooks.Hook
	AbortStopHooksInterrupt  chan bool
	IgnoreOrdering           bool
}

type TestConfig struct {
	Path               string
	Proxyport          uint32
	TestReportPath     string
	AppCmd             string
	MongoPassword      string
	AppContainer       string
	AppNetwork         string
	Delay              uint64
	BuildDelay         time.Duration
	PassThroughPorts   []uint
	ApiTimeout         uint64
	WithCoverage       bool
	CoverageReportPath string
	TestReport         platform.TestReportDB
	Storage            platform.TestCaseDB
	Tele               *telemetry.Telemetry
	PassThroughHosts   []models.Filters
	IgnoreOrdering     bool
}

type RunTestSetConfig struct {
	TestSet        string
	Path           string
	TestReportPath string
	AppCmd         string
	AppContainer   string
	AppNetwork     string
	Delay          uint64
	BuildDelay     time.Duration
	Pid            uint32
	Storage        platform.TestCaseDB
	LoadedHooks    *hooks.Hook
	TestReportFS   platform.TestReportDB
	TestRunChan    chan string
	ApiTimeout     uint64
	Ctx            context.Context
	ServeTest      bool
}

type SimulateRequestConfig struct {
	Tc             *models.TestCase
	LoadedHooks    *hooks.Hook
	AppCmd         string
	UserIP         string
	TestSet        string
	ApiTimeout     uint64
	Success        *int
	Failure        *int
	Status         *models.TestRunStatus
	TestReportFS   platform.TestReportDB
	TestReport     *models.TestReport
	Path           string
	DockerID       bool
	NoiseConfig    models.GlobalNoise
	IgnoreOrdering bool
}

type FetchTestResultsConfig struct {
	TestReportFS   platform.TestReportDB
	TestReport     *models.TestReport
	Status         *models.TestRunStatus
	TestSet        string
	Success        *int
	Failure        *int
	Ctx            context.Context
	TestReportPath string
	Path           string
}
