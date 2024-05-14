package record

import (
	"context"

	"go.keploy.io/server/v2/pkg/models"
)

type Instrumentation interface {
	//Setup prepares the environment for the recording
	Setup(ctx context.Context, cmd string, opts models.SetupOptions) (uint64, error)
	//Hook will load hooks and start the proxy server.
	Hook(ctx context.Context, id uint64, opts models.HookOptions) error
	GetIncoming(ctx context.Context, id uint64, opts models.IncomingOptions) (<-chan *models.TestCase, error)
	GetOutgoing(ctx context.Context, id uint64, opts models.OutgoingOptions) (<-chan *models.Mock, error)
	// Run is blocking call and will execute until error
	Run(ctx context.Context, id uint64, opts models.RunOptions) models.AppError
	GetContainerIP(ctx context.Context, id uint64) (string, error)
}

type Service interface {
	Start(ctx context.Context) error
	StartMock(ctx context.Context) error
	ReRecord(ctx context.Context, appID uint64) error
}

type TestDB interface {
	GetAllTestSetIDs(ctx context.Context) ([]string, error)
	InsertTestCase(ctx context.Context, tc *models.TestCase, testSetID string) error
	GetTestCases(ctx context.Context, testID string) ([]*models.TestCase, error)
}

type MockDB interface {
	InsertMock(ctx context.Context, mock *models.Mock, testSetID string) error
}

type Telemetry interface {
	RecordedTestSuite(testSet string, testsTotal int, mockTotal map[string]int)
	RecordedTestCaseMock(mockType string)
	RecordedMocks(mockTotal map[string]int)
	RecordedTestAndMocks()
}
