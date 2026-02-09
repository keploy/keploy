// Package postgres provides an orchestrator-based wrapper for PostgreSQL v2 parser.
// This demonstrates the read-only parser pattern where all I/O goes through the orchestrator.
package postgres

import (
	"context"
	"io"
	"net"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/orchestrator"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// OrchestratedRecorder wraps the PostgreSQL recorder with orchestrator-based I/O.
// In this pattern:
// - Reads happen via io.Reader (read-only)
// - Writes happen via orchestrator's async channel
// - Parser logic remains focused on transformation
type OrchestratedRecorder struct {
	logger       *zap.Logger
	orchestrator *orchestrator.Orchestrator
	mocks        chan<- *models.Mock
}

// NewOrchestratedRecorder creates a new orchestrator-wrapped PostgreSQL recorder.
func NewOrchestratedRecorder(
	logger *zap.Logger,
	clientConn net.Conn,
	destConn net.Conn,
	mocks chan<- *models.Mock,
) *OrchestratedRecorder {
	orch := orchestrator.New(orchestrator.Config{
		Logger:     logger,
		ClientConn: clientConn,
		DestConn:   destConn,
		Mode:       orchestrator.ModeRecord,
		BufferSize: 100,
	})

	return &OrchestratedRecorder{
		logger:       logger,
		orchestrator: orch,
		mocks:        mocks,
	}
}

// Record starts the recording flow using the orchestrator pattern.
// The flow is:
// 1. Read request from client (read-only)
// 2. Parse request into ParseResult
// 3. Write to destination via orchestrator (async)
// 4. Read response from destination (read-only)
// 5. Parse response
// 6. Write to client via orchestrator (async)
// 7. Record the mock
func (r *OrchestratedRecorder) Record(ctx context.Context, reqBuf []byte) error {
	// Start the orchestrator's write handler
	r.orchestrator.Run(ctx)
	defer r.orchestrator.Stop()

	// 1. Initial request already read (reqBuf provided)
	reqResult := &orchestrator.ParseResult{
		RawData:   reqBuf,
		Timestamp: time.Now(),
		Operation: "StartupMessage", // Will be determined by parsing
	}

	// 2. Write to destination via orchestrator
	if err := r.orchestrator.WriteToDestination(reqResult.RawData); err != nil {
		return err
	}

	// 3-6. The existing PostgreSQL encoding logic would be refactored here
	// to use the orchestrator's write methods instead of direct conn.Write()

	// For now, this demonstrates the pattern. Full refactoring would
	// replace the 10 Write() calls in recorder/encode.go and replayer/replayer.go
	// with orchestrator.WriteToClient() and orchestrator.WriteToDestination()

	return nil
}

// OrchestratedReplayer wraps the PostgreSQL replayer with orchestrator-based I/O.
type OrchestratedReplayer struct {
	logger       *zap.Logger
	orchestrator *orchestrator.Orchestrator
}

// NewOrchestratedReplayer creates a new orchestrator-wrapped PostgreSQL replayer.
func NewOrchestratedReplayer(
	logger *zap.Logger,
	clientConn net.Conn,
) *OrchestratedReplayer {
	orch := orchestrator.New(orchestrator.Config{
		Logger:     logger,
		ClientConn: clientConn,
		DestConn:   nil, // No destination in replay mode
		Mode:       orchestrator.ModeReplay,
		BufferSize: 100,
	})

	return &OrchestratedReplayer{
		logger:       logger,
		orchestrator: orch,
	}
}

// Replay handles the replay flow using orchestrator pattern.
func (r *OrchestratedReplayer) Replay(ctx context.Context, reqBuf []byte, mockDb interface{}) error {
	r.orchestrator.Run(ctx)
	defer r.orchestrator.Stop()

	// 1. Parse request (already read)
	reqResult := &orchestrator.ParseResult{
		RawData:   reqBuf,
		Timestamp: time.Now(),
		Operation: "Query", // Will be determined by parsing
	}

	// 2. Look up mock response
	// (This would use the existing PostgreSQL match logic)
	mockResp := []byte{} // Placeholder - would be replaced with actual mock lookup
	_ = reqResult        // Use reqResult for matching

	// 3. Write mock response via orchestrator
	return r.orchestrator.WriteToClient(mockResp)
}

// WriteFunc is the callback signature for controlled writes.
// Parsers receive this function instead of net.Conn to perform writes.
type WriteFunc func(data []byte) error

// TransformFunc represents a pure transformation function.
// It takes raw bytes and returns parsed data without any I/O.
type TransformFunc func(ctx context.Context, data []byte) (*orchestrator.ParseResult, error)

// ConnectionPair provides read-only access to connections.
type ConnectionPair struct {
	ClientReader io.Reader
	DestReader   io.Reader
}

// NewConnectionPair creates read-only connection wrappers.
func NewConnectionPair(clientConn, destConn net.Conn) *ConnectionPair {
	return &ConnectionPair{
		ClientReader: orchestrator.NewReadOnlyConn(clientConn),
		DestReader:   orchestrator.NewReadOnlyConn(destConn),
	}
}
