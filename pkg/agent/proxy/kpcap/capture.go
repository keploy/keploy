// Package kpcap writes Keploy debug packet captures.
//
// A .kpcap file is an append-only JSON-lines stream. It is intentionally
// Keploy-specific instead of libpcap: the proxy records the byte streams it
// observes at the application/dependency boundaries, with per-flow metadata
// intended for record-vs-test divergence analysis.
package kpcap

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
)

const (
	fileFormatVersion = 1
	maxChunkBytes     = 64 * 1024

	DirectionAppToProxy      = "app->proxy"
	DirectionProxyToApp      = "proxy->app"
	DirectionProxyToUpstream = "proxy->upstream"
	DirectionUpstreamToProxy = "upstream->proxy"
	DirectionClientToIngress = "client->ingress"
	DirectionIngressToClient = "ingress->client"
	DirectionIngressToApp    = "ingress->app"
	DirectionAppToIngress    = "app->ingress"
)

var captureSeq atomic.Uint64

// PacketContext describes the connection/flow that produced a chunk.
// Every chunk carries a snapshot of this metadata so two captures can be
// compared without reconstructing state from earlier records.
type PacketContext struct {
	Flow            string `json:"flow,omitempty"`
	Component       string `json:"component,omitempty"`
	Mode            string `json:"mode,omitempty"`
	Protocol        string `json:"protocol,omitempty"`
	Parser          string `json:"parser,omitempty"`
	ConnID          string `json:"conn_id,omitempty"`
	PeerConnID      string `json:"peer_conn_id,omitempty"`
	LocalAddr       string `json:"local_addr,omitempty"`
	RemoteAddr      string `json:"remote_addr,omitempty"`
	SourceAddr      string `json:"source_addr,omitempty"`
	DestinationAddr string `json:"destination_addr,omitempty"`
	SourcePort      uint32 `json:"source_port,omitempty"`
	DestinationPort uint32 `json:"destination_port,omitempty"`
	AppPort         uint16 `json:"app_port,omitempty"`
}

type captureEvent struct {
	Version       int    `json:"v"`
	Type          string `json:"type"`
	TimestampNS   int64  `json:"ts_unix_nano"`
	Sequence      uint64 `json:"seq,omitempty"`
	SessionID     string `json:"session_id,omitempty"`
	CapturePath   string `json:"capture_path,omitempty"`
	StartedAt     string `json:"started_at,omitempty"`
	EndedAt       string `json:"ended_at,omitempty"`
	Hostname      string `json:"hostname,omitempty"`
	PID           int    `json:"pid,omitempty"`
	Direction     string `json:"direction,omitempty"`
	Size          int    `json:"size,omitempty"`
	ChunkIndex    int    `json:"chunk_index,omitempty"`
	ChunkCount    int    `json:"chunk_count,omitempty"`
	PayloadBase64 string `json:"payload_b64,omitempty"`
	PacketContext
}

// Capture owns one .kpcap output file. It is safe for concurrent use by all
// connection goroutines in one proxy component.
type Capture struct {
	logger    *zap.Logger
	enabled   bool
	file      *os.File
	path      string
	sessionID string
	mode      string
	component string
	mu        sync.Mutex
	closed    bool
}

// New creates a capture writer when debug capture is enabled for record/test.
// Capture errors are deliberately non-fatal: debug output must never break the
// customer's record/test run.
func New(logger *zap.Logger, mode, capturePath string, enabled bool, component string) *Capture {
	c := &Capture{logger: logger, mode: mode, component: sanitize(component)}
	if !enabled || (mode != "record" && mode != "test") {
		return c
	}

	basePath := resolveBasePath(capturePath)
	debugDir := filepath.Join(basePath, "debug")
	if err := os.MkdirAll(debugDir, 0o755); err != nil {
		if logger != nil {
			logger.Debug("failed to create kpcap debug directory; continuing without packet capture",
				zap.String("path", debugDir), zap.Error(err))
		}
		return c
	}

	now := time.Now().UTC()
	sessionID := fmt.Sprintf("%s-%s-%d-%d", mode, c.component, now.UnixNano(), os.Getpid())
	fileName := fmt.Sprintf("%s.kpcap", sessionID)
	path := filepath.Join(debugDir, fileName)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o644)
	if err != nil {
		if logger != nil {
			logger.Debug("failed to create kpcap file; continuing without packet capture",
				zap.String("path", path), zap.Error(err))
		}
		return c
	}

	c.enabled = true
	c.file = file
	c.path = path
	c.sessionID = sessionID

	hostname, _ := os.Hostname()
	c.writeEventLocked(captureEvent{
		Version:     fileFormatVersion,
		Type:        "capture-start",
		TimestampNS: now.UnixNano(),
		SessionID:   sessionID,
		CapturePath: path,
		StartedAt:   now.Format(time.RFC3339Nano),
		Hostname:    hostname,
		PID:         os.Getpid(),
		PacketContext: PacketContext{
			Mode:      mode,
			Component: c.component,
		},
	})

	if logger != nil {
		logger.Info("Keploy debug packet capture enabled", zap.String("kpcap", path), zap.String("mode", mode), zap.String("component", c.component))
	}
	return c
}

func resolveBasePath(capturePath string) string {
	capturePath = strings.TrimSpace(capturePath)
	if capturePath == "" || capturePath == "." {
		return filepath.Join(".", "keploy")
	}
	return capturePath
}

func sanitize(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return "proxy"
	}
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	cleaned := strings.Trim(b.String(), "-")
	if cleaned == "" {
		return "proxy"
	}
	return cleaned
}

// Enabled reports whether chunks will be persisted.
func (c *Capture) Enabled() bool {
	return c != nil && c.enabled
}

// Path returns the .kpcap output path, or an empty string when disabled.
func (c *Capture) Path() string {
	if c == nil {
		return ""
	}
	return c.path
}

// Close flushes and closes the capture file.
func (c *Capture) Close() error {
	if c == nil || !c.enabled {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	endedAt := time.Now().UTC()
	c.writeEventLocked(captureEvent{
		Version:     fileFormatVersion,
		Type:        "capture-end",
		TimestampNS: endedAt.UnixNano(),
		SessionID:   c.sessionID,
		EndedAt:     endedAt.Format(time.RFC3339Nano),
		PacketContext: PacketContext{
			Mode:      c.mode,
			Component: c.component,
		},
	})
	c.closed = true
	if err := c.file.Sync(); err != nil {
		_ = c.file.Close()
		return err
	}
	return c.file.Close()
}

// RecordChunk persists payload bytes for a flow direction. Large writes are
// split into bounded JSON lines to keep files diffable and memory use stable.
func (c *Capture) RecordChunk(ctx PacketContext, direction string, payload []byte) {
	if c == nil || !c.enabled || len(payload) == 0 {
		return
	}
	ctx.Mode = firstNonEmpty(ctx.Mode, c.mode)
	ctx.Component = firstNonEmpty(ctx.Component, c.component)
	ctx.Protocol = firstNonEmpty(ctx.Protocol, "tcp")

	chunkCount := (len(payload) + maxChunkBytes - 1) / maxChunkBytes
	for i, offset := 0, 0; offset < len(payload); i, offset = i+1, offset+maxChunkBytes {
		end := offset + maxChunkBytes
		if end > len(payload) {
			end = len(payload)
		}
		chunk := payload[offset:end]
		event := captureEvent{
			Version:       fileFormatVersion,
			Type:          "chunk",
			TimestampNS:   time.Now().UTC().UnixNano(),
			Sequence:      captureSeq.Add(1),
			SessionID:     c.sessionID,
			Direction:     direction,
			Size:          len(chunk),
			ChunkIndex:    i,
			ChunkCount:    chunkCount,
			PayloadBase64: base64.StdEncoding.EncodeToString(chunk),
			PacketContext: ctx,
		}
		c.writeEvent(event)
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func (c *Capture) writeEvent(event captureEvent) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.writeEventLocked(event)
}

func (c *Capture) writeEventLocked(event captureEvent) {
	if c.closed || c.file == nil {
		return
	}
	data, err := json.Marshal(event)
	if err != nil {
		if c.logger != nil {
			c.logger.Debug("failed to marshal kpcap event", zap.Error(err))
		}
		return
	}
	data = append(data, '\n')
	if _, err := c.file.Write(data); err != nil && c.logger != nil {
		c.logger.Debug("failed to write kpcap event", zap.String("path", c.path), zap.Error(err))
	}
}

// WrapConn returns a net.Conn that records bytes read from and written to the
// connection while delegating all behavior to the underlying connection.
func (c *Capture) WrapConn(conn net.Conn, ctx PacketContext, readDirection, writeDirection string) net.Conn {
	if c == nil || !c.enabled || conn == nil {
		return conn
	}
	if ctx.LocalAddr == "" && conn.LocalAddr() != nil {
		ctx.LocalAddr = conn.LocalAddr().String()
	}
	if ctx.RemoteAddr == "" && conn.RemoteAddr() != nil {
		ctx.RemoteAddr = conn.RemoteAddr().String()
	}
	return &captureConn{
		Conn:           conn,
		capture:        c,
		ctx:            ctx,
		readDirection:  readDirection,
		writeDirection: writeDirection,
	}
}

// WrapWriter records bytes that are successfully written to sink.
func (c *Capture) WrapWriter(sink io.Writer, ctx PacketContext, direction string) io.Writer {
	if c == nil || !c.enabled || sink == nil {
		return sink
	}
	return &captureWriter{sink: sink, capture: c, ctx: ctx, direction: direction}
}

type captureConn struct {
	net.Conn
	capture        *Capture
	ctx            PacketContext
	readDirection  string
	writeDirection string
}

func (c *captureConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 {
		c.capture.RecordChunk(c.ctx, c.readDirection, p[:n])
	}
	return n, err
}

func (c *captureConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	if n > 0 {
		c.capture.RecordChunk(c.ctx, c.writeDirection, p[:n])
	}
	return n, err
}

func (c *captureConn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return nil
}

func (c *captureConn) CloseRead() error {
	if cr, ok := c.Conn.(interface{ CloseRead() error }); ok {
		return cr.CloseRead()
	}
	return nil
}

type captureWriter struct {
	sink      io.Writer
	capture   *Capture
	ctx       PacketContext
	direction string
}

func (w *captureWriter) Write(p []byte) (int, error) {
	n, err := w.sink.Write(p)
	if n > 0 {
		w.capture.RecordChunk(w.ctx, w.direction, p[:n])
	}
	return n, err
}
