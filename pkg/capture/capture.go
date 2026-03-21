// Package capture provides network packet capture and replay functionality for debug diagnostics.
// When enabled (via --debug flag during record/test), it records all raw bytes flowing through
// the proxy into a .kpcap file. This file, combined with debug logs and mocks, allows the
// Keploy team to reproduce customer issues exactly.
package capture

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"go.uber.org/zap"
)

// Manager coordinates network packet capture across all proxy connections.
// It is safe for concurrent use by multiple goroutines.
type Manager struct {
	mu     sync.RWMutex
	logger *zap.Logger

	writer  *Writer
	enabled bool
	mode    string // "record" or "test"

	// metadata about the capture session
	startTime  time.Time
	outputPath string

	// stats
	stats CaptureStats
}

// CaptureStats tracks capture statistics.
type CaptureStats struct {
	TotalConnections uint64
	TotalPackets     uint64
	TotalBytes       uint64
	DroppedPackets   uint64
}

// NewManager creates a new capture manager. If outputDir is empty, capture is disabled.
func NewManager(logger *zap.Logger, outputDir string, mode string) (*Manager, error) {
	m := &Manager{
		logger:    logger,
		mode:      mode,
		startTime: time.Now(),
		enabled:   outputDir != "",
	}

	if !m.enabled {
		logger.Debug("Network capture disabled (no output directory specified)")
		return m, nil
	}

	// Ensure output directory exists
	if err := os.MkdirAll(outputDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create capture output directory %s: %w", outputDir, err)
	}

	// Generate capture filename with timestamp
	filename := fmt.Sprintf("capture_%s_%s.kpcap",
		mode,
		time.Now().Format("20060102_150405"))
	m.outputPath = filepath.Join(outputDir, filename)

	// Create the writer
	w, err := NewWriter(m.outputPath, mode)
	if err != nil {
		return nil, fmt.Errorf("failed to create capture writer: %w", err)
	}
	m.writer = w

	logger.Info("Network capture enabled",
		zap.String("output", m.outputPath),
		zap.String("mode", mode))

	return m, nil
}

// IsEnabled returns whether capture is currently active.
func (m *Manager) IsEnabled() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.enabled && m.writer != nil
}

// WrapConn wraps a net.Conn to capture all reads and writes.
// connID uniquely identifies the connection within this capture session.
// direction indicates whether this is the client-side or destination-side connection.
func (m *Manager) WrapConn(conn *CaptureConn) *CaptureConn {
	if !m.IsEnabled() {
		return conn
	}
	conn.SetWriter(m.writer)
	return conn
}

// RecordConnectionOpen records a new connection being established.
func (m *Manager) RecordConnectionOpen(connID uint64, srcAddr, dstAddr string, isTLS bool) {
	if !m.IsEnabled() {
		return
	}

	m.mu.Lock()
	m.stats.TotalConnections++
	m.mu.Unlock()

	pkt := &Packet{
		Timestamp:    time.Now(),
		ConnectionID: connID,
		Type:         PacketTypeConnOpen,
		SrcAddr:      srcAddr,
		DstAddr:      dstAddr,
		IsTLS:        isTLS,
	}

	if err := m.writer.WritePacket(pkt); err != nil {
		m.logger.Debug("failed to write connection open packet", zap.Error(err), zap.Uint64("connID", connID))
		m.mu.Lock()
		m.stats.DroppedPackets++
		m.mu.Unlock()
	}
}

// RecordConnectionClose records a connection being closed.
func (m *Manager) RecordConnectionClose(connID uint64) {
	if !m.IsEnabled() {
		return
	}

	pkt := &Packet{
		Timestamp:    time.Now(),
		ConnectionID: connID,
		Type:         PacketTypeConnClose,
	}

	if err := m.writer.WritePacket(pkt); err != nil {
		m.logger.Debug("failed to write connection close packet", zap.Error(err), zap.Uint64("connID", connID))
	}
}

// RecordData records raw data flowing through a connection.
func (m *Manager) RecordData(connID uint64, direction Direction, data []byte, protocol Protocol) {
	if !m.IsEnabled() || len(data) == 0 {
		return
	}

	m.mu.Lock()
	m.stats.TotalPackets++
	m.stats.TotalBytes += uint64(len(data))
	m.mu.Unlock()

	pkt := &Packet{
		Timestamp:    time.Now(),
		ConnectionID: connID,
		Type:         PacketTypeData,
		Direction:    direction,
		Protocol:     protocol,
		Payload:      data,
	}

	if err := m.writer.WritePacket(pkt); err != nil {
		m.logger.Debug("failed to write data packet",
			zap.Error(err),
			zap.Uint64("connID", connID),
			zap.Int("dataLen", len(data)))
		m.mu.Lock()
		m.stats.DroppedPackets++
		m.mu.Unlock()
	}
}

// RecordProtocolDetected records which protocol was detected for a connection.
func (m *Manager) RecordProtocolDetected(connID uint64, protocol Protocol) {
	if !m.IsEnabled() {
		return
	}

	pkt := &Packet{
		Timestamp:    time.Now(),
		ConnectionID: connID,
		Type:         PacketTypeProtocol,
		Protocol:     protocol,
	}

	if err := m.writer.WritePacket(pkt); err != nil {
		m.logger.Debug("failed to write protocol detection packet", zap.Error(err))
	}
}

// RecordError records an error that occurred during connection handling.
func (m *Manager) RecordError(connID uint64, errMsg string) {
	if !m.IsEnabled() {
		return
	}

	pkt := &Packet{
		Timestamp:    time.Now(),
		ConnectionID: connID,
		Type:         PacketTypeError,
		Payload:      []byte(errMsg),
	}

	if err := m.writer.WritePacket(pkt); err != nil {
		m.logger.Debug("failed to write error packet", zap.Error(err))
	}
}

// GetStats returns current capture statistics.
func (m *Manager) GetStats() CaptureStats {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.stats
}

// GetWriter returns the underlying writer for direct use by CaptureConn.
// Returns nil if capture is not enabled.
func (m *Manager) GetWriter() *Writer {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.writer
}

// GetOutputPath returns the path to the capture file.
func (m *Manager) GetOutputPath() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.outputPath
}

// Close finalizes the capture file and releases resources.
func (m *Manager) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.writer == nil {
		return nil
	}

	stats := m.stats
	m.logger.Info("Closing network capture",
		zap.String("output", m.outputPath),
		zap.Uint64("connections", stats.TotalConnections),
		zap.Uint64("packets", stats.TotalPackets),
		zap.Uint64("bytes", stats.TotalBytes),
		zap.Uint64("dropped", stats.DroppedPackets))

	err := m.writer.Close()
	m.writer = nil
	m.enabled = false
	return err
}

// Shutdown gracefully shuts down the capture manager.
func (m *Manager) Shutdown(_ context.Context) error {
	return m.Close()
}
