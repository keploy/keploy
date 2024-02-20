package replay

import (
	"context"
	"time"

	"go.keploy.io/server/v2/graph/model"
	"go.keploy.io/server/v2/pkg/models"
)

type Instrumentation interface {
	//Setup prepares the environment for the recording
	Setup(ctx context.Context, cmd string, opts models.SetupOptions) (uint64, error)
	//Hook will load hooks and start the proxy server.
	Hook(ctx context.Context, id uint64, opts models.HookOptions) error
	MockOutgoing(ctx context.Context, id uint64, opts models.IncomingOptions) <-chan error
	// SetMocks Allows for setting mocks between test runs for better filtering and matching
	SetMocks(ctx context.Context, id uint64, filtered []models.Frame, unFiltered []models.Frame) error
	// Run is blocking call and will execute until error
	Run(ctx context.Context, id uint64, opts models.RunOptions) models.AppError
}

type Service interface {
	Replay(ctx context.Context) error
	BootReplay(ctx context.Context) (string, uint64, error)
	GetAllTestSetIds(ctx context.Context) ([]string, error)
	RunTestSet(ctx context.Context, testSetId string, testRunId string, appId uint64) (models.TestRunStatus, error)
	GetTestSetStatus(ctx context.Context, testRunId string, testSetId string) (model.TestSetStatus, error)
}

type TestDB interface {
	GetAllTestSetIds(ctx context.Context) ([]string, error)
	GetTestCases(ctx context.Context, testSetId string) ([]*models.TestCase, error)
}

type MockDB interface {
	GetFilteredMocks(ctx context.Context, testSetId string, afterTime time.Time, beforeTime time.Time) ([]*models.Mock, error)
	// TODO timestamps are added as in unfiltered also we filtering and put the filtered in unfiltered, need to discuss on this
	// TODO Need to decide who will do sorting
	// TODO define ctx
	GetUnFilteredMocks(ctx context.Context, testSetId string, afterTime time.Time, beforeTime time.Time) ([]*models.Mock, error)
}

type ReportDB interface {
	GetAllTestRunIds(ctx context.Context) ([]string, error)
	GetTestCaseResults(ctx context.Context, testRunId string, testSetId string) ([]*models.TestResult, error)
	InsertTestCaseResult(ctx context.Context, testRunId string, testSetId string, testCaseId string, result *models.TestResult) error
	InsertReport(ctx context.Context, testRunId string, testSetId string, testReport *models.TestReport) error
}

type Telemetry interface {
}
