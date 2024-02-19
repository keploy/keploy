package replay

import (
	"context"

	"go.keploy.io/server/v2/pkg/models"
)

type Instrumentation interface {
	//Setup prepares the environment for the recording
	Setup(ctx context.Context, cmd string, opts models.SetupOptions) (int, error)
	//Hook will load hooks and start the proxy server.
	Hook(ctx context.Context, id int, opts models.HookOptions) error
	MockOutgoing(ctx context.Context, id int, mocks []models.Frame, opts models.IncomingOptions) error
	SetMocks(ctx context.Context, id int, mocks []models.Frame) error
	// Run is blocking call and will execute until error
	Run(ctx context.Context, id int, opts models.RunOptions) error
}

type Service interface {
	Replay(ctx context.Context) error
	SimulateTestCase(ctx context.Context, cfg models.SimulateRequestConfig) error
}

type TestDB interface {
	GetAllTestSetIds(ctx context.Context) ([]string, error)
	GetTestCases(ctx context.Context, testSetId string) ([]models.Frame, error)
}

type MockDB interface {
	GetMocks(ctx context.Context, testSetId string) ([]models.Frame, error)
}

type ReportDB interface {
	GetReport(ctx context.Context, testRunId string, testSetId string) (models.TestReport, error)
	InsertReport(ctx context.Context, testRunId string, testSetId string, testReport models.TestReport) error
}

type Telemetry interface {
}
