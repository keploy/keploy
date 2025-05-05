package replay

import (
	"context"
	"io"
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
	GetConsumedMocks(ctx context.Context, id uint64) ([]models.MockState, error)
	// Run is blocking call and will execute until error
	Run(ctx context.Context, id uint64, opts models.RunOptions) models.AppError

	GetContainerIP(ctx context.Context, id uint64) (string, error)
}

type Service interface {
	Start(ctx context.Context) error
	Instrument(ctx context.Context) (*InstrumentState, error)
	GetNextTestRunID(ctx context.Context) (string, error)
	GetAllTestSetIDs(ctx context.Context) ([]string, error)
	RunTestSet(ctx context.Context, testSetID string, testRunID string, appID uint64, serveTest bool) (models.TestSetStatus, error)
	GetTestSetStatus(ctx context.Context, testRunID string, testSetID string) (models.TestSetStatus, error)
	GetTestCases(ctx context.Context, testID string) ([]*models.TestCase, error)
	GetTestSetConf(ctx context.Context, testSetID string) (*models.TestSet, error)
	RunApplication(ctx context.Context, appID uint64, opts models.RunOptions) models.AppError
	Normalize(ctx context.Context) error
	DenoiseTestCases(ctx context.Context, testSetID string, noiseParams []*models.NoiseParams) ([]*models.NoiseParams, error)
	NormalizeTestCases(ctx context.Context, testRun string, testSetID string, selectedTestCaseIDs []string, testResult []models.TestResult) error
	DeleteTests(ctx context.Context, testSetID string, testCaseIDs []string) error
	DeleteTestSet(ctx context.Context, testSetID string) error
}

type TestDB interface {
	GetAllTestSetIDs(ctx context.Context) ([]string, error)
	GetTestCases(ctx context.Context, testSetID string) ([]*models.TestCase, error)
	UpdateTestCase(ctx context.Context, testCase *models.TestCase, testSetID string, enableLog bool) error
	DeleteTests(ctx context.Context, testSetID string, testCaseIDs []string) error
	DeleteTestSet(ctx context.Context, testSetID string) error
}

type MockDB interface {
	GetFilteredMocks(ctx context.Context, testSetID string, afterTime time.Time, beforeTime time.Time) ([]*models.Mock, error)
	GetUnFilteredMocks(ctx context.Context, testSetID string, afterTime time.Time, beforeTime time.Time) ([]*models.Mock, error)
	UpdateMocks(ctx context.Context, testSetID string, mockNames map[string]models.MockState) error
}

type ReportDB interface {
	GetAllTestRunIDs(ctx context.Context) ([]string, error)
	GetTestCaseResults(ctx context.Context, testRunID string, testSetID string) ([]models.TestResult, error)
	GetReport(ctx context.Context, testRunID string, testSetID string) (*models.TestReport, error)
	InsertTestCaseResult(ctx context.Context, testRunID string, testSetID string, result *models.TestResult) error
	InsertReport(ctx context.Context, testRunID string, testSetID string, testReport *models.TestReport) error
	UpdateReport(ctx context.Context, testRunID string, testCoverage any) error
}

type TestSetConfig interface {
	Read(ctx context.Context, testSetID string) (*models.TestSet, error)
	Write(ctx context.Context, testSetID string, testSet *models.TestSet) error
}

type Telemetry interface {
	TestSetRun(success int, failure int, testSet string, runStatus string)
	TestRun(success int, failure int, testSets int, runStatus string)
	MockTestRun(utilizedMocks int)
}

type TestHooks interface {
	SimulateRequest(ctx context.Context, appID uint64, tc *models.TestCase, testSetID string) (interface{}, error)
	GetConsumedMocks(ctx context.Context, id uint64) ([]models.MockState, error)
	BeforeTestSetRun(ctx context.Context, testSetID string) error
	AfterTestSetRun(ctx context.Context, testSetID string, status bool) error
	AfterTestRun(ctx context.Context, testRunID string, testSetIDs []string, coverage models.TestCoverage) error // hook executed after running all the test-sets
}

type Storage interface {
	Upload(ctx context.Context, file io.Reader, mockName string, appName string, jwtToken string) error
	Download(ctx context.Context, mockName string, appName string, userName string, jwtToken string) (io.Reader, error)
}

type InstrumentState struct {
	AppID      uint64
	HookCancel context.CancelFunc
}
