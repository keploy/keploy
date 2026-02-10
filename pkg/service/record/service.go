package record

import (
	"context"

	"go.keploy.io/server/v3/pkg/models"
)

type Instrumentation interface {
	//Setup prepares the environment for the recording
	Setup(ctx context.Context, cmd string, opts models.SetupOptions) error
	GetIncoming(ctx context.Context, opts models.IncomingOptions) (<-chan *models.TestCase, error)
	GetOutgoing(ctx context.Context, opts models.OutgoingOptions) (<-chan *models.Mock, error)
	GetMappings(ctx context.Context, opts models.IncomingOptions) (<-chan models.TestMockMapping, error)
	// Run is blocking call and will execute until error
	Run(ctx context.Context, opts models.RunOptions) models.AppError
	MakeAgentReadyForDockerCompose(ctx context.Context) error
	// NotifyGracefulShutdown notifies the agent that the application is shutting down gracefully.
	// When this is called, connection errors will be logged as debug instead of error.
	NotifyGracefulShutdown(ctx context.Context) error
}

type Service interface {
	Start(ctx context.Context, reRecordCfg models.ReRecordCfg) error
	SetGlobalMockChannel(mockCh chan<- *models.Mock)
	GetNextTestSetID(ctx context.Context) (string, error)
}

type TestDB interface {
	GetAllTestSetIDs(ctx context.Context) ([]string, error)
	InsertTestCase(ctx context.Context, tc *models.TestCase, testSetID string, enableLog bool) error
	// GetTestCases(ctx context.Context, testID string) ([]*models.TestCase, error)
}

type MockDB interface {
	InsertMock(ctx context.Context, mock *models.Mock, testSetID string) error
	DeleteMocksForSet(ctx context.Context, testSetID string) error
	GetCurrMockID() int64
	ResetCounterID()
}

type MappingDb interface {
	Insert(ctx context.Context, mapping *models.Mapping) error
	Upsert(ctx context.Context, testSetID string, testID string, mockIDs []string) error
}

type TestSetConfig interface {
	Read(ctx context.Context, testSetID string) (*models.TestSet, error)
	Write(ctx context.Context, testSetID string, testSet *models.TestSet) error
}

type Telemetry interface {
	RecordedTestSuite(testSet string, testsTotal int, mockTotal map[string]int, metadata map[string]interface{})
	RecordedTestCaseMock(mockType string)
	RecordedMocks(mockTotal map[string]int)
	RecordedTestAndMocks()
}

type FrameChan struct {
	Incoming <-chan *models.TestCase
	Outgoing <-chan *models.Mock
	Mappings <-chan models.TestMockMapping
}
