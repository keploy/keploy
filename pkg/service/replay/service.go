package replay

import (
	"context"
	"time"

	"go.keploy.io/server/v2/pkg/models"
)

type Instrumentation interface {
	//Setup prepares the environment for the recording
	Setup(ctx context.Context, cmd string, opts models.SetupOptions) (uint64, error)
	//Hook will load hooks and start the proxy server.
	Hook(ctx context.Context, id uint64, opts models.HookOptions) error
	MockOutgoing(ctx context.Context, id uint64, opts models.OutgoingOptions) error
	// SetMocks Allows for setting mocks between test runs for better filtering and matching
	SetMocks(ctx context.Context, id uint64, filtered []*models.Mock, unFiltered []*models.Mock) error
	// GetConsumedMocks to log the names of the mocks that were consumed during the test run of failed test cases
	GetConsumedMocks(ctx context.Context, id uint64) ([]string, error)
	// Run is blocking call and will execute until error
	Run(ctx context.Context, id uint64, opts models.RunOptions) models.AppError

	GetContainerIP(ctx context.Context, id uint64) (string, error)
}

type Service interface {
	Start(ctx context.Context) error
	BootReplay(ctx context.Context) (string, uint64, context.CancelFunc, error)
	GetAllTestSetIDs(ctx context.Context) ([]string, error)
	RunTestSet(ctx context.Context, testSetID string, testRunID string, appID uint64, serveTest bool) (models.TestSetStatus, error)
	GetTestSetStatus(ctx context.Context, testRunID string, testSetID string) (models.TestSetStatus, error)
	RunApplication(ctx context.Context, appID uint64, opts models.RunOptions) models.AppError
	ProvideMocks(ctx context.Context) error
	Normalize(ctx context.Context) error
}

type TestDB interface {
	GetAllTestSetIDs(ctx context.Context) ([]string, error)
	GetTestCases(ctx context.Context, testSetID string) ([]*models.TestCase, error)
	UpdateTestCase(ctx context.Context, testCase *models.TestCase, testSetID string) error
}

type MockDB interface {
	GetFilteredMocks(ctx context.Context, testSetID string, afterTime time.Time, beforeTime time.Time) ([]*models.Mock, error)
	GetUnFilteredMocks(ctx context.Context, testSetID string, afterTime time.Time, beforeTime time.Time) ([]*models.Mock, error)
	UpdateMocks(ctx context.Context, testSetID string, mockNames map[string]bool) error
}

type ReportDB interface {
	GetAllTestRunIDs(ctx context.Context) ([]string, error)
	GetTestCaseResults(ctx context.Context, testRunID string, testSetID string) ([]models.TestResult, error)
	GetReport(ctx context.Context, testRunID string, testSetID string) (*models.TestReport, error)
	InsertTestCaseResult(ctx context.Context, testRunID string, testSetID string, result *models.TestResult) error
	InsertReport(ctx context.Context, testRunID string, testSetID string, testReport *models.TestReport) error
}

type Telemetry interface {
	TestSetRun(success int, failure int, testSet string, runStatus string)
	TestRun(success int, failure int, testSets int, runStatus string)
	MockTestRun(utilizedMocks int)
}

// RequestEmulator is used to simulate the API requests to the user API. The requests are read from
// the recorded test case of the user app.
type RequestEmulator interface {
	SimulateRequest(ctx context.Context, appID uint64, tc *models.TestCase, testSetID string) (*models.HTTPResp, error)
}
