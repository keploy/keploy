package packet

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"

	"go.uber.org/zap"
)

// ConnectionTracker manages connection lifecycle and graceful shutdown
type ConnectionTracker struct {
	mu          sync.RWMutex
	connections map[uint16]*managedConnection
	wg          sync.WaitGroup
	ctx         context.Context
	cancel      context.CancelFunc
	logger      *zap.Logger
}

// managedConnection wraps a connection with its lifecycle management
type managedConnection struct {
	conn      *net.TCPConn
	srcPort   uint16
	done      chan struct{}
	closed    chan struct{}
	closeOnce sync.Once
}

// NewConnectionTracker creates a new connection tracker
func NewConnectionTracker(ctx context.Context, logger *zap.Logger) *ConnectionTracker {
	ctx, cancel := context.WithCancel(ctx)
	return &ConnectionTracker{
		connections: make(map[uint16]*managedConnection),
		ctx:         ctx,
		cancel:      cancel,
		logger:      logger,
	}
}

// CreateConnection establishes a new connection and starts its reader goroutine
func (ct *ConnectionTracker) CreateConnection(srcPort uint16, proxyAddr string) (*net.TCPConn, error) {
	ct.mu.Lock()
	defer ct.mu.Unlock()

	// Check if connection already exists
	if existing, exists := ct.connections[srcPort]; exists {
		return existing.conn, nil
	}

	// Create new connection with timeout
	dialer := &net.Dialer{
		Timeout:   5 * time.Second,
		KeepAlive: 30 * time.Second,
	}

	conn, err := dialer.DialContext(ct.ctx, "tcp", proxyAddr)
	if err != nil {
		return nil, fmt.Errorf("dial proxy %s for srcPort %d: %w", proxyAddr, srcPort, err)
	}

	tcpConn, ok := conn.(*net.TCPConn)
	if !ok {
		conn.Close()
		return nil, fmt.Errorf("expected TCP connection for srcPort %d", srcPort)
	}

	// Configure TCP socket options
	_ = tcpConn.SetKeepAlive(true)
	_ = tcpConn.SetKeepAlivePeriod(30 * time.Second)
	_ = tcpConn.SetNoDelay(true)

	managedConn := &managedConnection{
		conn:    tcpConn,
		srcPort: srcPort,
		done:    make(chan struct{}),
		closed:  make(chan struct{}),
	}

	ct.connections[srcPort] = managedConn

	// Start reader goroutine for this connection
	ct.wg.Add(1)
	go ct.connectionReader(managedConn)

	ct.logger.Info("replay: connected to proxy",
		zap.Uint16("srcPort", srcPort),
		zap.String("proxyAddr", proxyAddr))

	return tcpConn, nil
}

// connectionReader handles reading from a connection until it's closed
func (ct *ConnectionTracker) connectionReader(mc *managedConnection) {
	defer func() {
		ct.wg.Done()
		close(mc.closed)
		ct.logger.Debug("replay: connection reader finished", zap.Uint16("srcPort", mc.srcPort))
	}()

	buf := make([]byte, 32<<10)

	for {
		select {
		case <-ct.ctx.Done():
			ct.logger.Debug("replay: connection reader stopping due to context cancellation",
				zap.Uint16("srcPort", mc.srcPort))
			return
		case <-mc.done:
			ct.logger.Debug("replay: connection reader stopping due to connection closure",
				zap.Uint16("srcPort", mc.srcPort))
			return
		default:
			// Set read timeout
			deadline := time.Now().Add(2 * time.Minute)
			if err := mc.conn.SetReadDeadline(deadline); err != nil {
				ct.logger.Warn("replay: failed to set read deadline",
					zap.Error(err), zap.Uint16("srcPort", mc.srcPort))
				return
			}

			_, err := mc.conn.Read(buf)
			if err != nil {
				if ne, ok := err.(net.Error); ok && ne.Timeout() {
					// Timeout is expected, continue reading
					continue
				}
				// EOF or other error stops the reader
				ct.logger.Debug("replay: connection read error",
					zap.Error(err), zap.Uint16("srcPort", mc.srcPort))
				return
			}
		}
	}
}

// GetConnection returns an existing connection for the given srcPort
func (ct *ConnectionTracker) GetConnection(srcPort uint16) *net.TCPConn {
	ct.mu.RLock()
	defer ct.mu.RUnlock()

	if conn, exists := ct.connections[srcPort]; exists {
		return conn.conn
	}
	return nil
}

// CloseConnection closes a specific connection gracefully
func (ct *ConnectionTracker) CloseConnection(srcPort uint16) error {
	ct.mu.Lock()
	managedConn, exists := ct.connections[srcPort]
	if !exists {
		ct.mu.Unlock()
		return fmt.Errorf("connection for srcPort %d not found", srcPort)
	}
	delete(ct.connections, srcPort)
	ct.mu.Unlock()

	var err error
	managedConn.closeOnce.Do(func() {
		close(managedConn.done)

		// Close write side first (send FIN)
		if closeErr := managedConn.conn.CloseWrite(); closeErr != nil {
			ct.logger.Warn("replay: failed to close write side",
				zap.Error(closeErr), zap.Uint16("srcPort", srcPort))
			err = closeErr
		}

		ct.logger.Info("replay: closed connection writer", zap.Uint16("srcPort", srcPort))
	})

	return err
}

// CloseAll closes all connections gracefully
func (ct *ConnectionTracker) CloseAll() error {
	ct.mu.Lock()
	srcPorts := make([]uint16, 0, len(ct.connections))
	for srcPort := range ct.connections {
		srcPorts = append(srcPorts, srcPort)
	}
	ct.mu.Unlock()

	// Close all connections
	var firstErr error
	for _, srcPort := range srcPorts {
		if err := ct.CloseConnection(srcPort); err != nil && firstErr == nil {
			firstErr = err
		}
	}

	return firstErr
}

// WaitForAllClosed waits for all connections to be fully closed
func (ct *ConnectionTracker) WaitForAllClosed(timeout time.Duration) error {
	// First, get all managed connections that need to be waited for
	ct.mu.RLock()
	closedChannels := make([]<-chan struct{}, 0, len(ct.connections))
	for _, mc := range ct.connections {
		closedChannels = append(closedChannels, mc.closed)
	}
	ct.mu.RUnlock()

	if len(closedChannels) == 0 {
		return nil
	}

	// Wait for all connection readers to finish with timeout
	done := make(chan struct{})
	go func() {
		defer close(done)
		ct.wg.Wait() // Wait for all reader goroutines to finish
	}()

	select {
	case <-done:
		ct.logger.Info("replay: all connections closed successfully")
		return nil
	case <-time.After(timeout):
		ct.logger.Warn("replay: timeout waiting for connections to close")
		ct.cancel() // Cancel context to force goroutines to exit

		// Give a bit more time for forced shutdown
		select {
		case <-done:
			ct.logger.Info("replay: all connections closed after forced shutdown")
			return nil
		case <-time.After(5 * time.Second):
			return fmt.Errorf("timeout waiting for connections to close")
		}
	}
}

// Shutdown performs graceful shutdown of all connections
func (ct *ConnectionTracker) Shutdown(timeout time.Duration) error {
	ct.logger.Info("replay: starting connection tracker shutdown")

	// Cancel context to signal all goroutines to stop
	ct.cancel()

	// Close all connections
	if err := ct.CloseAll(); err != nil {
		ct.logger.Warn("replay: error during connection closure", zap.Error(err))
	}

	// Wait for all connections to be closed
	return ct.WaitForAllClosed(timeout)
}

// GetActiveConnectionCount returns the number of active connections
func (ct *ConnectionTracker) GetActiveConnectionCount() int {
	ct.mu.RLock()
	defer ct.mu.RUnlock()
	return len(ct.connections)
}
