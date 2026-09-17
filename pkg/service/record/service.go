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
	// StreamPcapArtifacts opens long-lived HTTP streams to the
	// agent's pcap and TLS keylog channels and writes the bytes to
	// <destDir>/traffic.pcap and <destDir>/sslkeys.log respectively
	// as packets arrive. Blocks until ctx is cancelled. The
	// streaming model is required because the cluster live-recording
	// use case never stops; a fetch-on-stop model would never
	// deliver bytes.
	StreamPcapArtifacts(ctx context.Context, destDir string) error
}

type Service interface {
	Start(ctx context.Context) error
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
	Insert(ctx context.Context, mapping *models.Mapping, replace bool) error
	Upsert(ctx context.Context, testSetID string, testID string, mockEntries []models.MockEntry) error
	// UpsertBatch persists several tests' mappings in one file rewrite. Recording
	// must use this rather than a per-mapping Upsert: the per-mapping cost grows
	// with the file, and a slow consumer back-pressures the agent's mapping stream
	// into dropping entries (its send is non-blocking so capture is never stalled).
	UpsertBatch(ctx context.Context, testSetID string, byTest map[string][]models.MockEntry) error
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
	RecordSessionCompleted(testCount, mockCount int64, durationMs int64, status string, stopReason string)
}

type FrameChan struct {
	Incoming <-chan *models.TestCase
	Outgoing <-chan *models.Mock
	Mappings <-chan models.TestMockMapping

	// Abandon says that nothing will ever read the channels above, so the
	// forwarders filling them must stop waiting for a reader.
	//
	// It exists because the forwarders OUTLIVE the decision to consume
	// them. They run on reqCtx (WithoutCancel) and keep pulling from the
	// agent, while Start can return between creating them and spawning the
	// consumers -- today at exactly one point, its post-setup ctx.Err()
	// gate, and at any error return added between the two later. That
	// "later" is why Start uses a defer rather than a call beside the one
	// return it has: the failure mode of forgetting is a hang, not a
	// compile error.
	// Each forwarder hands over the item it has already taken from the
	// agent when ctx is cancelled, so the tail is not silently dropped; with
	// no reader that hand-over parked FOREVER. Measured on the mapping
	// channel it wedges on the first item, and on the other two on the
	// second. In production that is a 30s DrainErrGroup timeout on Ctrl+C
	// plus a goroutine held for the process lifetime, re-leaked per session
	// in the DaemonSet embedding.
	//
	// Calling it converts that park into a logged drop. The tail really is
	// lost on those paths -- nothing is left to persist it -- and saying so
	// once is the honest outcome; hanging is not.
	//
	// NON-NIL ON EVERY err == nil RETURN, including the early ones where the
	// channels come back already closed. The err != nil returns hand back a
	// zero FrameChan whose Abandon IS nil, so a caller must check the error
	// first -- the ordinary Go contract for a (value, error) pair.
	//
	// AND THAT IS NOT MERELY A CONTRACT DETAIL. Two of those error returns
	// come AFTER the incoming forwarder has been spawned, so a nil Abandon
	// there would leave a live forwarder that nothing could release --
	// measured at a 30s teardown and a goroutine leaked for the process
	// lifetime. GetTestAndMockChans therefore abandons on those paths from
	// its own defer, before returning the error; the caller is never asked
	// to.
	//
	// Idempotent, so a caller may also call it explicitly.
	Abandon func()
}
