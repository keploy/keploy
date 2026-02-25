package replay

import (
	"context"
	"io"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

type Instrumentation interface {
	//Setup prepares the environment for the recording
	Setup(ctx context.Context, cmd string, opts models.SetupOptions) error

	MockOutgoing(ctx context.Context, opts models.OutgoingOptions) error
	// GetConsumedMocks to log the names of the mocks that were consumed during the test run of failed test cases
	GetConsumedMocks(ctx context.Context) ([]models.MockState, error)
	// Run is blocking call and will execute until error
	Run(ctx context.Context, opts models.RunOptions) models.AppError
	// GetErrorChannel returns the error channel from the proxy for monitoring proxy errors
	GetErrorChannel() <-chan error
	BeforeSimulate(ctx context.Context, timestamp *time.Time, testSetID string, testCaseName string) error
	AfterSimulate(ctx context.Context, tcName string, testSetID string) error
	BeforeTestRun(ctx context.Context, testRunID string) error
	BeforeTestSetCompose(ctx context.Context, testRunID string, firstRun bool) error
	AfterTestRun(ctx context.Context, testRunID string, testSetIDs []string, coverage models.TestCoverage) error
	// New methods for improved mock management
	StoreMocks(ctx context.Context, filtered []*models.Mock, unFiltered []*models.Mock) error
	UpdateMockParams(ctx context.Context, params models.MockFilterParams) error
	MakeAgentReadyForDockerCompose(ctx context.Context) error
	// NotifyGracefulShutdown notifies the agent that the application is shutting down gracefully.
	// When this is called, connection errors will be logged as debug instead of error.
	NotifyGracefulShutdown(ctx context.Context) error
}

type Service interface {
	Start(ctx context.Context) error
	Instrument(ctx context.Context) (*InstrumentState, error)
	GetNextTestRunID(ctx context.Context) (string, error)
	GetAllTestSetIDs(ctx context.Context) ([]string, error)
	RunTestSet(ctx context.Context, testSetID string, testRunID string, serveTest bool) (models.TestSetStatus, error)
	GetTestSetStatus(ctx context.Context, testRunID string, testSetID string) (models.TestSetStatus, error)
	GetTestCases(ctx context.Context, testID string) ([]*models.TestCase, error)
	GetTestSetConf(ctx context.Context, testSetID string) (*models.TestSet, error)
	// UpdateTestSetTemplate persists the (possibly updated) template map for a test-set.
	// Used during re-record to dynamically refresh values like JWTs/IDs as soon as
	// their producing API responses are observed, so subsequent test cases use the
	// latest values rather than stale ones from the previous run.
	UpdateTestSetTemplate(ctx context.Context, testSetID string, template map[string]interface{}) error
	RunApplication(ctx context.Context, opts models.RunOptions) models.AppError
	DenoiseTestCases(ctx context.Context, testSetID string, noiseParams []*models.NoiseParams) ([]*models.NoiseParams, error)
	DeleteTests(ctx context.Context, testSetID string, testCaseIDs []string) error
	DeleteTestSet(ctx context.Context, testSetID string) error

	DownloadMocks(ctx context.Context) error
	UploadMocks(ctx context.Context, testSets []string) error

	StoreMappings(ctx context.Context, mapping *models.Mapping) error

	// CompareHTTPResp compares HTTP responses and returns match result with detailed diffs
	CompareHTTPResp(tc *models.TestCase, actualResponse *models.HTTPResp, testSetID string, emitFailureLogs bool) (bool, *models.Result)
	// CompareGRPCResp compares gRPC responses and returns match result with detailed diffs
	CompareGRPCResp(tc *models.TestCase, actualResp *models.GrpcResp, testSetID string, emitFailureLogs bool) (bool, *models.Result)
}

type TestDB interface {
	GetAllTestSetIDs(ctx context.Context) ([]string, error)
	GetTestCases(ctx context.Context, testSetID string) ([]*models.TestCase, error)
	UpdateTestCase(ctx context.Context, testCase *models.TestCase, testSetID string, enableLog bool) error
	DeleteTests(ctx context.Context, testSetID string, testCaseIDs []string) error
	DeleteTestSet(ctx context.Context, testSetID string) error
}

type MockDB interface {
	GetFilteredMocks(ctx context.Context, testSetID string, afterTime time.Time, beforeTime time.Time, mocksThatHaveMappings map[string]bool, mocksWeNeed map[string]bool) ([]*models.Mock, error)
	GetUnFilteredMocks(ctx context.Context, testSetID string, afterTime time.Time, beforeTime time.Time, mocksThatHaveMappings map[string]bool, mocksWeNeed map[string]bool) ([]*models.Mock, error)
	UpdateMocks(ctx context.Context, testSetID string, mockNames map[string]models.MockState) error
}

type ReportDB interface {
	GetAllTestRunIDs(ctx context.Context) ([]string, error)
	GetTestCaseResults(ctx context.Context, testRunID string, testSetID string) ([]models.TestResult, error)
	GetReport(ctx context.Context, testRunID string, testSetID string) (*models.TestReport, error)
	ClearTestCaseResults(_ context.Context, testRunID string, testSetID string)
	InsertTestCaseResult(ctx context.Context, testRunID string, testSetID string, result *models.TestResult) error // 1
	InsertReport(ctx context.Context, testRunID string, testSetID string, testReport *models.TestReport) error     // 2
	UpdateReport(ctx context.Context, testRunID string, testCoverage any) error
}

type TestSetConfig interface {
	Read(ctx context.Context, testSetID string) (*models.TestSet, error)
	Write(ctx context.Context, testSetID string, testSet *models.TestSet) error
	ReadSecret(ctx context.Context, testSetID string) (map[string]interface{}, error)
}

type Telemetry interface {
	TestSetRun(success int, failure int, testSet string, runStatus string)
	TestRun(success int, failure int, testSets int, runStatus string, metadata map[string]interface{})
	MockTestRun(utilizedMocks int)
}

type TestHooks interface {
	SimulateRequest(ctx context.Context, tc *models.TestCase, testSetID string) (interface{}, error)
	GetConsumedMocks(ctx context.Context) ([]models.MockState, error)
	BeforeTestRun(ctx context.Context, testRunID string) error
	BeforeTestSetCompose(ctx context.Context, testRunID string, firstRun bool) error
	BeforeTestSetRun(ctx context.Context, testSetID string) error
	BeforeTestResult(ctx context.Context, testRunID string, testSetID string, testCaseResults []models.TestResult) error
	AfterTestSetRun(ctx context.Context, testSetID string, status bool) error
	AfterTestRun(ctx context.Context, testRunID string, testSetIDs []string, coverage models.TestCoverage) error // hook executed after running all the test-sets
}

type Storage interface {
	Upload(ctx context.Context, file io.Reader, mockName string, appName string, jwtToken string) error // 3
	Download(ctx context.Context, mockName string, appName string, userName string, jwtToken string) (io.Reader, error)
}

type InstrumentState struct {
	HookCancel context.CancelFunc
}

type MappingDB interface {
	Insert(ctx context.Context, mapping *models.Mapping) error
	Get(ctx context.Context, testSetID string) (map[string][]string, bool, error)
}
