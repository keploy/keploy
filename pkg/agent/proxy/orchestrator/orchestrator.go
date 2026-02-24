// Package orchestrator provides async I/O handling for parsers.
// Parsers become read-only transformers, this orchestrator handles all writes.
package orchestrator

import (
	"context"
	"net"
	"sync"

	"go.uber.org/zap"
)

// Target specifies the write destination
type Target int

const (
	TargetClient Target = iota
	TargetDestination
)

// WriteRequest represents a request to write data to a connection
type WriteRequest struct {
	Target  Target
	Conn    net.Conn // Optional: Explicit connection to write to
	Data    []byte
	ErrChan chan error
}

// Orchestrator handles async I/O between client, parser, and destination
type Orchestrator struct {
	logger     *zap.Logger
	connMu     sync.Mutex // protects clientConn and destConn
	clientConn net.Conn
	destConn   net.Conn // nil in replay mode
	writeChan  chan WriteRequest
	mode       Mode
	wg         sync.WaitGroup
}

// Mode represents the operating mode
type Mode int

const (
	ModeRecord Mode = iota
	ModeReplay
)

// Config holds orchestrator configuration
type Config struct {
	Logger     *zap.Logger
	ClientConn net.Conn
	DestConn   net.Conn // nil for replay mode
	Mode       Mode
	BufferSize int // Write channel buffer size
}

// New creates a new Orchestrator
func New(cfg Config) *Orchestrator {
	bufSize := cfg.BufferSize
	if bufSize <= 0 {
		bufSize = 100
	}
	return &Orchestrator{
		logger:     cfg.Logger,
		clientConn: cfg.ClientConn,
		destConn:   cfg.DestConn,
		writeChan:  make(chan WriteRequest, bufSize),
		mode:       cfg.Mode,
	}
}

// Run starts the orchestrator's async write handler
func (o *Orchestrator) Run(ctx context.Context) {
	o.wg.Add(1)
	go func() {
		defer o.wg.Done()
		o.writeLoop()
	}()
}

// writeLoop handles all writes asynchronously.
// It runs until the channel is closed by Stop(), ensuring all queued writes
// are processed. This prevents deadlocks when producers are blocked on a full channel.
func (o *Orchestrator) writeLoop() {
	for req := range o.writeChan {
		var conn net.Conn

		// Use explicit connection if provided, otherwise fallback to stored targets
		if req.Conn != nil {
			conn = req.Conn
		} else {
			o.connMu.Lock()
			if req.Target == TargetClient {
				conn = o.clientConn
			} else {
				conn = o.destConn
			}
			o.connMu.Unlock()
		}

		var err error
		if conn != nil {
			_, err = conn.Write(req.Data)
		}

		if req.ErrChan != nil {
			req.ErrChan <- err
		}
	}
}

// SetClientConn updates the client connection held by the orchestrator.
// This must be called after in-protocol TLS upgrades (e.g., PostgreSQL SSLRequest)
// so that subsequent writes go through the TLS-wrapped connection.
func (o *Orchestrator) SetClientConn(conn net.Conn) {
	o.connMu.Lock()
	o.clientConn = conn
	o.connMu.Unlock()
}

// WriteToClient queues a write to the client connection
func (o *Orchestrator) WriteToClient(data []byte) error {
	return o.write(TargetClient, nil, data)
}

// WriteToDestination queues a write to the destination connection
func (o *Orchestrator) WriteToDestination(data []byte) error {
	return o.write(TargetDestination, nil, data)
}

// WriteToConn queues a write to a specific connection
func (o *Orchestrator) WriteToConn(conn net.Conn, data []byte) error {
	return o.write(TargetClient, conn, data) // Target ignored when Conn is present
}

// write sends a write request and waits for completion
func (o *Orchestrator) write(target Target, conn net.Conn, data []byte) error {
	errChan := make(chan error, 1)
	o.writeChan <- WriteRequest{
		Target:  target,
		Conn:    conn,
		Data:    data,
		ErrChan: errChan,
	}
	return <-errChan
}

// WriteAsync queues a write without waiting for completion
func (o *Orchestrator) WriteAsync(target Target, data []byte) {
	o.writeChan <- WriteRequest{
		Target:  target,
		Data:    data,
		ErrChan: nil, // No wait
	}
}

// WriteAsyncToConn queues a write to a specific connection without waiting for
// completion. Use this in hot loops (e.g., command phase) where pipelining
// matters and errors are detected by the reader goroutines.
func (o *Orchestrator) WriteAsyncToConn(conn net.Conn, data []byte) {
	o.writeChan <- WriteRequest{
		Conn:    conn,
		Data:    data,
		ErrChan: nil,
	}
}

// Stop closes the write channel and waits for completion
func (o *Orchestrator) Stop() {
	close(o.writeChan)
	o.wg.Wait()
}
