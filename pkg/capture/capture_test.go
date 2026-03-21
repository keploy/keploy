package capture

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
)

// ──────────────────────────────────────────────────
// Writer / Reader Round-trip Tests
// ──────────────────────────────────────────────────

func TestWriterReaderRoundtrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.kpcap")

	// Write packets
	w, err := NewWriter(path, "record")
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}

	packets := []*Packet{
		{
			Timestamp:    time.Now(),
			ConnectionID: 1,
			Type:         PacketTypeConnOpen,
			SrcAddr:      "127.0.0.1:54321",
			DstAddr:      "10.0.0.1:3306",
			IsTLS:        false,
		},
		{
			Timestamp:    time.Now(),
			ConnectionID: 1,
			Type:         PacketTypeProtocol,
			Protocol:     ProtoMySQL,
		},
		{
			Timestamp:    time.Now(),
			ConnectionID: 1,
			Type:         PacketTypeData,
			Direction:    DirClientToProxy,
			Protocol:     ProtoMySQL,
			Payload:      []byte("SELECT 1"),
		},
		{
			Timestamp:    time.Now(),
			ConnectionID: 1,
			Type:         PacketTypeData,
			Direction:    DirProxyToClient,
			Protocol:     ProtoMySQL,
			Payload:      []byte{0x01, 0x00, 0x00, 0x01, 0x01}, // MySQL OK
		},
		{
			Timestamp:    time.Now(),
			ConnectionID: 1,
			Type:         PacketTypeConnClose,
		},
	}

	for _, pkt := range packets {
		if err := w.WritePacket(pkt); err != nil {
			t.Fatalf("WritePacket: %v", err)
		}
	}

	if w.PacketCount() != uint64(len(packets)) {
		t.Fatalf("expected %d packets, got %d", len(packets), w.PacketCount())
	}

	if err := w.Close(); err != nil {
		t.Fatalf("Close writer: %v", err)
	}

	// Read them back
	r, err := NewReader(path)
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	defer r.Close()

	// Verify header
	if r.Metadata().Mode != "record" {
		t.Fatalf("expected mode 'record', got %q", r.Metadata().Mode)
	}
	if r.Metadata().OS == "" {
		t.Fatal("expected non-empty OS in metadata")
	}

	// Read all packets and verify
	cf, err := r.ReadAll()
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}

	if len(cf.Packets) != len(packets) {
		t.Fatalf("expected %d packets, got %d", len(packets), len(cf.Packets))
	}

	// Verify first packet (ConnOpen)
	p0 := cf.Packets[0]
	if p0.Type != PacketTypeConnOpen {
		t.Fatalf("packet 0: expected ConnOpen, got %s", p0.Type)
	}
	if p0.SrcAddr != "127.0.0.1:54321" {
		t.Fatalf("packet 0: expected src addr 127.0.0.1:54321, got %s", p0.SrcAddr)
	}
	if p0.DstAddr != "10.0.0.1:3306" {
		t.Fatalf("packet 0: expected dst addr 10.0.0.1:3306, got %s", p0.DstAddr)
	}
	if p0.IsTLS {
		t.Fatal("packet 0: expected non-TLS")
	}

	// Verify protocol detection packet
	p1 := cf.Packets[1]
	if p1.Protocol != ProtoMySQL {
		t.Fatalf("packet 1: expected MySQL protocol, got %s", p1.Protocol)
	}

	// Verify data packet
	p2 := cf.Packets[2]
	if p2.Direction != DirClientToProxy {
		t.Fatalf("packet 2: expected DirClientToProxy, got %s", p2.Direction)
	}
	if string(p2.Payload) != "SELECT 1" {
		t.Fatalf("packet 2: expected payload 'SELECT 1', got %q", string(p2.Payload))
	}

	// Verify response packet
	p3 := cf.Packets[3]
	if p3.Direction != DirProxyToClient {
		t.Fatalf("packet 3: expected DirProxyToClient, got %s", p3.Direction)
	}
	if !bytes.Equal(p3.Payload, []byte{0x01, 0x00, 0x00, 0x01, 0x01}) {
		t.Fatalf("packet 3: payload mismatch")
	}

	// Verify close packet
	p4 := cf.Packets[4]
	if p4.Type != PacketTypeConnClose {
		t.Fatalf("packet 4: expected ConnClose, got %s", p4.Type)
	}
}

func TestWriterReaderEmptyPayload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.kpcap")

	w, err := NewWriter(path, "test")
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}

	pkt := &Packet{
		Timestamp:    time.Now(),
		ConnectionID: 42,
		Type:         PacketTypeConnOpen,
	}
	if err := w.WritePacket(pkt); err != nil {
		t.Fatalf("WritePacket: %v", err)
	}
	w.Close()

	r, err := NewReader(path)
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	defer r.Close()

	cf, err := r.ReadAll()
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(cf.Packets) != 1 {
		t.Fatalf("expected 1 packet, got %d", len(cf.Packets))
	}
	if cf.Packets[0].ConnectionID != 42 {
		t.Fatalf("expected connID 42, got %d", cf.Packets[0].ConnectionID)
	}
}

func TestWriterReaderLargePayload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "large.kpcap")

	w, err := NewWriter(path, "record")
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}

	// Create a 1MB payload
	payload := make([]byte, 1024*1024)
	for i := range payload {
		payload[i] = byte(i % 256)
	}

	pkt := &Packet{
		Timestamp:    time.Now(),
		ConnectionID: 1,
		Type:         PacketTypeData,
		Direction:    DirClientToProxy,
		Protocol:     ProtoHTTP,
		Payload:      payload,
	}
	if err := w.WritePacket(pkt); err != nil {
		t.Fatalf("WritePacket: %v", err)
	}
	w.Close()

	r, err := NewReader(path)
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	defer r.Close()

	readPkt, err := r.ReadPacket()
	if err != nil {
		t.Fatalf("ReadPacket: %v", err)
	}
	if !bytes.Equal(readPkt.Payload, payload) {
		t.Fatal("large payload mismatch")
	}
}

func TestWriterPayloadTooLarge(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "toolarge.kpcap")

	w, err := NewWriter(path, "record")
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	defer w.Close()

	pkt := &Packet{
		Timestamp: time.Now(),
		Type:      PacketTypeData,
		Payload:   make([]byte, MaxPayloadSize+1),
	}
	err = w.WritePacket(pkt)
	if err == nil {
		t.Fatal("expected error for oversized payload")
	}
}

func TestWriterClosedWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "closed.kpcap")

	w, err := NewWriter(path, "record")
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	w.Close()

	err = w.WritePacket(&Packet{Timestamp: time.Now()})
	if err == nil {
		t.Fatal("expected error writing to closed writer")
	}
}

func TestReaderInvalidMagic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.kpcap")

	err := os.WriteFile(path, []byte("NOT A KPCAP FILE"), 0644)
	if err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, err = NewReader(path)
	if err == nil {
		t.Fatal("expected error for invalid magic bytes")
	}
}

func TestReaderEmptyFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.kpcap")

	err := os.WriteFile(path, []byte{}, 0644)
	if err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, err = NewReader(path)
	if err == nil {
		t.Fatal("expected error for empty file")
	}
}

// ──────────────────────────────────────────────────
// Writer Concurrency Test
// ──────────────────────────────────────────────────

func TestWriterConcurrent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "concurrent.kpcap")

	w, err := NewWriter(path, "record")
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}

	const numGoroutines = 50
	const packetsPerGoroutine = 100

	var wg sync.WaitGroup
	wg.Add(numGoroutines)

	for g := 0; g < numGoroutines; g++ {
		go func(gID int) {
			defer wg.Done()
			for i := 0; i < packetsPerGoroutine; i++ {
				pkt := &Packet{
					Timestamp:    time.Now(),
					ConnectionID: uint64(gID),
					Type:         PacketTypeData,
					Direction:    DirClientToProxy,
					Protocol:     ProtoHTTP,
					Payload:      []byte(fmt.Sprintf("goroutine-%d-packet-%d", gID, i)),
				}
				if err := w.WritePacket(pkt); err != nil {
					t.Errorf("WritePacket from goroutine %d: %v", gID, err)
				}
			}
		}(g)
	}

	wg.Wait()

	expected := uint64(numGoroutines * packetsPerGoroutine)
	if w.PacketCount() != expected {
		t.Fatalf("expected %d packets, got %d", expected, w.PacketCount())
	}

	w.Close()

	// Verify readback
	r, err := NewReader(path)
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	defer r.Close()

	cf, err := r.ReadAll()
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(cf.Packets) != int(expected) {
		t.Fatalf("expected %d packets, got %d", expected, len(cf.Packets))
	}
}

// ──────────────────────────────────────────────────
// Manager Tests
// ──────────────────────────────────────────────────

func TestManagerDisabled(t *testing.T) {
	m, err := NewManager(newTestLogger(), "", "record")
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if m.IsEnabled() {
		t.Fatal("expected disabled manager")
	}

	// All operations should be no-ops when disabled
	m.RecordConnectionOpen(1, "a", "b", false)
	m.RecordData(1, DirClientToProxy, []byte("test"), ProtoHTTP)
	m.RecordConnectionClose(1)
	m.RecordError(1, "err")
	m.RecordProtocolDetected(1, ProtoHTTP)

	stats := m.GetStats()
	if stats.TotalConnections != 0 {
		t.Fatal("expected no connections when disabled")
	}

	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestManagerEnabled(t *testing.T) {
	dir := t.TempDir()

	m, err := NewManager(newTestLogger(), dir, "record")
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	defer m.Close()

	if !m.IsEnabled() {
		t.Fatal("expected enabled manager")
	}

	// Record a connection lifecycle
	m.RecordConnectionOpen(1, "127.0.0.1:5000", "10.0.0.1:3306", false)
	m.RecordProtocolDetected(1, ProtoMySQL)
	m.RecordData(1, DirClientToProxy, []byte("SELECT 1"), ProtoMySQL)
	m.RecordData(1, DirProxyToClient, []byte("OK"), ProtoMySQL)
	m.RecordConnectionClose(1)

	stats := m.GetStats()
	if stats.TotalConnections != 1 {
		t.Fatalf("expected 1 connection, got %d", stats.TotalConnections)
	}
	if stats.TotalPackets != 2 {
		t.Fatalf("expected 2 data packets, got %d", stats.TotalPackets)
	}
	if stats.TotalBytes != 10 { // "SELECT 1" + "OK"
		t.Fatalf("expected 10 bytes, got %d", stats.TotalBytes)
	}
	if stats.DroppedPackets != 0 {
		t.Fatalf("expected 0 dropped packets, got %d", stats.DroppedPackets)
	}

	// Verify output file exists
	outputPath := m.GetOutputPath()
	if outputPath == "" {
		t.Fatal("expected non-empty output path")
	}
	if _, err := os.Stat(outputPath); err != nil {
		t.Fatalf("output file not found: %v", err)
	}

	// Close and verify the file is readable
	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	r, err := NewReader(outputPath)
	if err != nil {
		t.Fatalf("NewReader on output: %v", err)
	}
	defer r.Close()

	cf, err := r.ReadAll()
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	// 5 packets: open, protocol, data, data, close
	if len(cf.Packets) != 5 {
		t.Fatalf("expected 5 packets in file, got %d", len(cf.Packets))
	}
}

func TestManagerShutdown(t *testing.T) {
	dir := t.TempDir()
	m, err := NewManager(newTestLogger(), dir, "test")
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	m.RecordData(1, DirClientToProxy, []byte("test"), ProtoHTTP)

	if err := m.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	if m.IsEnabled() {
		t.Fatal("expected disabled after shutdown")
	}
}

// ──────────────────────────────────────────────────
// CaptureConn Tests
// ──────────────────────────────────────────────────

func TestCaptureConnReadWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "conn.kpcap")

	w, err := NewWriter(path, "record")
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}

	// Use two separate pipes: one for read, one for write
	// Pipe 1: test reading from capture conn
	readClient, readServer := net.Pipe()
	cc := NewCaptureConn(readClient, nil, 1, DirClientToProxy, DirProxyToClient)
	cc.writer = w
	cc.SetProtocol(ProtoHTTP)

	go func() {
		readServer.Write([]byte("HTTP/1.1 200 OK\r\n"))
		readServer.Close()
	}()

	buf := make([]byte, 1024)
	n, _ := cc.Read(buf)
	if string(buf[:n]) != "HTTP/1.1 200 OK\r\n" {
		t.Fatalf("unexpected read data: %q", string(buf[:n]))
	}
	readClient.Close()

	// Pipe 2: test writing to capture conn
	writeClient, writeServer := net.Pipe()
	cc2 := NewCaptureConn(writeClient, nil, 1, DirClientToProxy, DirProxyToClient)
	cc2.writer = w
	cc2.SetProtocol(ProtoHTTP)

	go func() {
		io.ReadAll(writeServer)
		writeServer.Close()
	}()

	n, err = cc2.Write([]byte("GET / HTTP/1.1\r\n"))
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if n != 16 {
		t.Fatalf("expected 16 bytes written, got %d", n)
	}
	writeClient.Close()

	// Verify captured data
	w.Close()
	r, err := NewReader(path)
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	defer r.Close()

	cf, err := r.ReadAll()
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}

	if len(cf.Packets) != 2 {
		t.Fatalf("expected 2 packets, got %d", len(cf.Packets))
	}

	// First packet should be the read (client → proxy direction)
	if cf.Packets[0].Direction != DirClientToProxy {
		t.Fatalf("expected DirClientToProxy, got %s", cf.Packets[0].Direction)
	}
	if string(cf.Packets[0].Payload) != "HTTP/1.1 200 OK\r\n" {
		t.Fatalf("unexpected payload: %q", string(cf.Packets[0].Payload))
	}

	// Second packet should be the write (proxy → client direction)
	if cf.Packets[1].Direction != DirProxyToClient {
		t.Fatalf("expected DirProxyToClient, got %s", cf.Packets[1].Direction)
	}
	if string(cf.Packets[1].Payload) != "GET / HTTP/1.1\r\n" {
		t.Fatalf("unexpected payload: %q", string(cf.Packets[1].Payload))
	}
}

func TestCaptureConnNilWriter(t *testing.T) {
	// CaptureConn with nil writer should pass through without panicking
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()

	cc := NewCaptureConn(clientConn, nil, 1, DirClientToProxy, DirProxyToClient)
	// writer is nil - should be no-op

	go func() {
		serverConn.Write([]byte("hello"))
	}()

	buf := make([]byte, 10)
	n, err := cc.Read(buf)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(buf[:n]) != "hello" {
		t.Fatalf("unexpected: %q", string(buf[:n]))
	}
}

// ──────────────────────────────────────────────────
// Validation Tests
// ──────────────────────────────────────────────────

func TestValidate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "validate.kpcap")

	w, err := NewWriter(path, "record")
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}

	w.WritePacket(&Packet{
		Timestamp:    time.Now(),
		ConnectionID: 1,
		Type:         PacketTypeConnOpen,
		SrcAddr:      "a",
		DstAddr:      "b",
	})
	w.WritePacket(&Packet{
		Timestamp:    time.Now(),
		ConnectionID: 1,
		Type:         PacketTypeData,
		Payload:      []byte("hello"),
	})
	w.Close()

	result, err := Validate(path)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if !result.Valid {
		t.Fatal("expected valid")
	}
	if result.PacketCount != 2 {
		t.Fatalf("expected 2 packets, got %d", result.PacketCount)
	}
	if result.ByteCount != 5 {
		t.Fatalf("expected 5 bytes, got %d", result.ByteCount)
	}
	if result.ConnectionCount != 1 {
		t.Fatalf("expected 1 connection, got %d", result.ConnectionCount)
	}
}

// ──────────────────────────────────────────────────
// Analysis Tests
// ──────────────────────────────────────────────────

func TestAnalyze(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "analyze.kpcap")

	w, err := NewWriter(path, "record")
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}

	now := time.Now()
	pkts := []*Packet{
		{Timestamp: now, ConnectionID: 1, Type: PacketTypeConnOpen, SrcAddr: "a", DstAddr: "b"},
		{Timestamp: now.Add(time.Millisecond), ConnectionID: 1, Type: PacketTypeProtocol, Protocol: ProtoHTTP},
		{Timestamp: now.Add(2 * time.Millisecond), ConnectionID: 1, Type: PacketTypeData, Direction: DirClientToProxy, Payload: []byte("GET / HTTP/1.1\r\n")},
		{Timestamp: now.Add(3 * time.Millisecond), ConnectionID: 1, Type: PacketTypeData, Direction: DirProxyToClient, Payload: []byte("HTTP/1.1 200 OK\r\n")},
		{Timestamp: now.Add(4 * time.Millisecond), ConnectionID: 1, Type: PacketTypeConnClose},
		{Timestamp: now.Add(5 * time.Millisecond), ConnectionID: 2, Type: PacketTypeConnOpen, SrcAddr: "c", DstAddr: "d", IsTLS: true},
		{Timestamp: now.Add(6 * time.Millisecond), ConnectionID: 2, Type: PacketTypeError, Payload: []byte("connection refused")},
		{Timestamp: now.Add(7 * time.Millisecond), ConnectionID: 2, Type: PacketTypeConnClose},
	}

	for _, pkt := range pkts {
		w.WritePacket(pkt)
	}
	w.Close()

	report, err := Analyze(path)
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}

	if len(report.Connections) != 2 {
		t.Fatalf("expected 2 connections, got %d", len(report.Connections))
	}
	if report.Protocols[ProtoHTTP] != 1 {
		t.Fatalf("expected 1 HTTP connection")
	}
	if len(report.Errors) != 1 {
		t.Fatalf("expected 1 error, got %d", len(report.Errors))
	}

	formatted := FormatReport(report)
	if formatted == "" {
		t.Fatal("expected non-empty formatted report")
	}
	if !bytes.Contains([]byte(formatted), []byte("HTTP")) {
		t.Fatal("expected HTTP in report")
	}
}

// ──────────────────────────────────────────────────
// Bundle Tests
// ──────────────────────────────────────────────────

func TestCreateAndExtractBundle(t *testing.T) {
	dir := t.TempDir()

	// Create a capture file
	capturePath := filepath.Join(dir, "test.kpcap")
	w, err := NewWriter(capturePath, "record")
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	w.WritePacket(&Packet{Timestamp: time.Now(), Type: PacketTypeConnOpen, SrcAddr: "a", DstAddr: "b"})
	w.Close()

	// Create a mock directory
	mockDir := filepath.Join(dir, "mocks")
	os.MkdirAll(mockDir, 0755)
	os.WriteFile(filepath.Join(mockDir, "mock-1.yaml"), []byte("kind: HTTP\nspec: {}"), 0644)

	// Create a config file
	configPath := filepath.Join(dir, "keploy.yml")
	os.WriteFile(configPath, []byte("path: .\nappName: test"), 0644)

	// Create bundle
	bundlePath := filepath.Join(dir, "bundle.tar.gz")
	result, err := CreateBundle(newTestLogger(), BundleOptions{
		CaptureFile: capturePath,
		MockDir:     mockDir,
		ConfigFile:  configPath,
		OutputPath:  bundlePath,
		AppName:     "test-app",
		Mode:        "record",
		Notes:       "test issue",
	})
	if err != nil {
		t.Fatalf("CreateBundle: %v", err)
	}

	if _, err := os.Stat(result); err != nil {
		t.Fatalf("bundle file not found: %v", err)
	}

	// Extract bundle
	extractDir := filepath.Join(dir, "extracted")
	manifest, err := ExtractBundle(result, extractDir)
	if err != nil {
		t.Fatalf("ExtractBundle: %v", err)
	}

	if manifest.AppName != "test-app" {
		t.Fatalf("expected app name 'test-app', got %q", manifest.AppName)
	}
	if manifest.Mode != "record" {
		t.Fatalf("expected mode 'record', got %q", manifest.Mode)
	}
	if manifest.Notes != "test issue" {
		t.Fatalf("expected notes 'test issue', got %q", manifest.Notes)
	}
	if manifest.CaptureFile == "" {
		t.Fatal("expected non-empty capture file in manifest")
	}

	// Verify extracted files exist
	extractedCapture := filepath.Join(extractDir, manifest.CaptureFile)
	if _, err := os.Stat(extractedCapture); err != nil {
		t.Fatalf("extracted capture file not found: %v", err)
	}
}

func TestCreateBundleMissingCapture(t *testing.T) {
	_, err := CreateBundle(newTestLogger(), BundleOptions{
		CaptureFile: "/nonexistent/file.kpcap",
	})
	if err == nil {
		t.Fatal("expected error for missing capture file")
	}
}

func TestCreateBundleEmptyCapture(t *testing.T) {
	_, err := CreateBundle(newTestLogger(), BundleOptions{})
	if err == nil {
		t.Fatal("expected error for empty capture file path")
	}
}

// ──────────────────────────────────────────────────
// Replay Tests
// ──────────────────────────────────────────────────

func TestReplayerAgainstEchoServer(t *testing.T) {
	// Start a simple echo server that echoes back whatever it receives
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer listener.Close()

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				buf := make([]byte, 4096)
				for {
					n, err := c.Read(buf)
					if err != nil {
						return
					}
					c.Write(buf[:n])
				}
			}(conn)
		}
	}()

	// Create a capture file that sends "hello" and expects "hello" back
	dir := t.TempDir()
	path := filepath.Join(dir, "replay.kpcap")
	w, err := NewWriter(path, "record")
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}

	now := time.Now()
	w.WritePacket(&Packet{Timestamp: now, ConnectionID: 1, Type: PacketTypeConnOpen, SrcAddr: "a", DstAddr: listener.Addr().String()})
	w.WritePacket(&Packet{Timestamp: now.Add(time.Millisecond), ConnectionID: 1, Type: PacketTypeData, Direction: DirClientToProxy, Payload: []byte("hello")})
	w.WritePacket(&Packet{Timestamp: now.Add(2 * time.Millisecond), ConnectionID: 1, Type: PacketTypeData, Direction: DirProxyToClient, Payload: []byte("hello")})
	w.WritePacket(&Packet{Timestamp: now.Add(3 * time.Millisecond), ConnectionID: 1, Type: PacketTypeConnClose})
	w.Close()

	// Replay against the echo server
	replayer := NewReplayer(newTestLogger(), listener.Addr().String(), 5*time.Second)
	summary, err := replayer.ReplayFile(context.Background(), path)
	if err != nil {
		t.Fatalf("ReplayFile: %v", err)
	}

	if summary.TotalConns != 1 {
		t.Fatalf("expected 1 connection, got %d", summary.TotalConns)
	}
	if summary.ReplayedConns != 1 {
		t.Fatalf("expected 1 replayed, got %d", summary.ReplayedConns)
	}
	if summary.MatchedConns != 1 {
		t.Fatalf("expected 1 matched, got %d (failed: %d)", summary.MatchedConns, summary.FailedConns)
		for _, r := range summary.Results {
			for _, e := range r.Errors {
				t.Logf("  error: %s", e)
			}
			for _, mm := range r.ByteMismatches {
				t.Logf("  mismatch: expected %d, got %d at offset %d", mm.Expected, mm.Actual, mm.Offset)
			}
		}
	}
}

func TestReplayerMismatchDetection(t *testing.T) {
	// Start a server that always responds with "world"
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer listener.Close()

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				buf := make([]byte, 4096)
				for {
					_, err := c.Read(buf)
					if err != nil {
						return
					}
					c.Write([]byte("world"))
				}
			}(conn)
		}
	}()

	dir := t.TempDir()
	path := filepath.Join(dir, "mismatch.kpcap")
	w, err := NewWriter(path, "record")
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}

	now := time.Now()
	w.WritePacket(&Packet{Timestamp: now, ConnectionID: 1, Type: PacketTypeConnOpen, SrcAddr: "a", DstAddr: listener.Addr().String()})
	w.WritePacket(&Packet{Timestamp: now.Add(time.Millisecond), ConnectionID: 1, Type: PacketTypeData, Direction: DirClientToProxy, Payload: []byte("hello")})
	// Expect "hello" but server will respond "world" -> mismatch
	w.WritePacket(&Packet{Timestamp: now.Add(2 * time.Millisecond), ConnectionID: 1, Type: PacketTypeData, Direction: DirProxyToClient, Payload: []byte("hello")})
	w.WritePacket(&Packet{Timestamp: now.Add(3 * time.Millisecond), ConnectionID: 1, Type: PacketTypeConnClose})
	w.Close()

	replayer := NewReplayer(newTestLogger(), listener.Addr().String(), 5*time.Second)
	summary, err := replayer.ReplayFile(context.Background(), path)
	if err != nil {
		t.Fatalf("ReplayFile: %v", err)
	}

	if summary.MatchedConns != 0 {
		t.Fatalf("expected 0 matched, got %d", summary.MatchedConns)
	}
	if summary.FailedConns != 1 {
		t.Fatalf("expected 1 failed, got %d", summary.FailedConns)
	}
	if len(summary.Results[0].ByteMismatches) == 0 {
		t.Fatal("expected byte mismatches to be recorded")
	}
}

func TestReplayerTLSNotSkipped(t *testing.T) {
	// TLS connections contain decrypted plaintext (proxy does MITM termination
	// before the capture point), so they should NOT be skipped during replay.
	// We test that TLS connections are replayed like any other connection.

	// Start an echo server
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer listener.Close()

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				buf := make([]byte, 4096)
				for {
					n, err := c.Read(buf)
					if err != nil {
						return
					}
					c.Write(buf[:n])
				}
			}(conn)
		}
	}()

	dir := t.TempDir()
	path := filepath.Join(dir, "tls.kpcap")
	w, err := NewWriter(path, "record")
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}

	now := time.Now()
	// IsTLS=true but data is plaintext (this is how real captures work)
	w.WritePacket(&Packet{Timestamp: now, ConnectionID: 1, Type: PacketTypeConnOpen, SrcAddr: "a", DstAddr: listener.Addr().String(), IsTLS: true})
	w.WritePacket(&Packet{Timestamp: now.Add(time.Millisecond), ConnectionID: 1, Type: PacketTypeData, Direction: DirClientToProxy, Payload: []byte("plaintext over tls")})
	w.WritePacket(&Packet{Timestamp: now.Add(2 * time.Millisecond), ConnectionID: 1, Type: PacketTypeData, Direction: DirProxyToClient, Payload: []byte("plaintext over tls")})
	w.WritePacket(&Packet{Timestamp: now.Add(3 * time.Millisecond), ConnectionID: 1, Type: PacketTypeConnClose})
	w.Close()

	replayer := NewReplayer(newTestLogger(), listener.Addr().String(), 5*time.Second)
	summary, err := replayer.ReplayFile(context.Background(), path)
	if err != nil {
		t.Fatalf("ReplayFile: %v", err)
	}

	if summary.SkippedConns != 0 {
		t.Fatalf("TLS connections should not be skipped (got %d skipped)", summary.SkippedConns)
	}
	if summary.ReplayedConns != 1 {
		t.Fatalf("expected 1 replayed TLS conn, got %d", summary.ReplayedConns)
	}
	if summary.MatchedConns != 1 {
		t.Fatalf("expected 1 matched TLS conn, got %d", summary.MatchedConns)
	}
}

func TestReplayerConnectionTimeout(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "timeout.kpcap")
	w, err := NewWriter(path, "record")
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}

	now := time.Now()
	// Try to connect to a non-routable address
	w.WritePacket(&Packet{Timestamp: now, ConnectionID: 1, Type: PacketTypeConnOpen, SrcAddr: "a", DstAddr: "192.0.2.1:12345"})
	w.WritePacket(&Packet{Timestamp: now.Add(time.Millisecond), ConnectionID: 1, Type: PacketTypeData, Direction: DirClientToProxy, Payload: []byte("data")})
	w.WritePacket(&Packet{Timestamp: now.Add(2 * time.Millisecond), ConnectionID: 1, Type: PacketTypeConnClose})
	w.Close()

	// Use a non-routable address so connection will fail
	replayer := NewReplayer(newTestLogger(), "192.0.2.1:12345", 500*time.Millisecond)
	summary, err := replayer.ReplayFile(context.Background(), path)
	if err != nil {
		t.Fatalf("ReplayFile: %v", err)
	}

	if summary.FailedConns != 1 {
		t.Fatalf("expected 1 failed connection, got %d", summary.FailedConns)
	}
	if len(summary.Results[0].Errors) == 0 {
		t.Fatal("expected connection errors")
	}
}

// ──────────────────────────────────────────────────
// Connection Timeline Tests
// ──────────────────────────────────────────────────

func TestGetConnections(t *testing.T) {
	now := time.Now()
	cf := &CaptureFile{
		Packets: []*Packet{
			{Timestamp: now, ConnectionID: 1, Type: PacketTypeConnOpen, SrcAddr: "a", DstAddr: "b"},
			{Timestamp: now.Add(time.Millisecond), ConnectionID: 1, Type: PacketTypeProtocol, Protocol: ProtoHTTP},
			{Timestamp: now.Add(2 * time.Millisecond), ConnectionID: 1, Type: PacketTypeData, Payload: []byte("req")},
			{Timestamp: now.Add(3 * time.Millisecond), ConnectionID: 2, Type: PacketTypeConnOpen, SrcAddr: "c", DstAddr: "d", IsTLS: true},
			{Timestamp: now.Add(4 * time.Millisecond), ConnectionID: 2, Type: PacketTypeError, Payload: []byte("fail")},
			{Timestamp: now.Add(5 * time.Millisecond), ConnectionID: 1, Type: PacketTypeConnClose},
			{Timestamp: now.Add(6 * time.Millisecond), ConnectionID: 2, Type: PacketTypeConnClose},
		},
	}

	conns := cf.GetConnections()
	if len(conns) != 2 {
		t.Fatalf("expected 2 connections, got %d", len(conns))
	}

	c1 := conns[1]
	if c1.SrcAddr != "a" || c1.DstAddr != "b" {
		t.Fatalf("conn 1: unexpected addresses: %s → %s", c1.SrcAddr, c1.DstAddr)
	}
	if c1.Protocol != ProtoHTTP {
		t.Fatalf("conn 1: expected HTTP, got %s", c1.Protocol)
	}
	if len(c1.Packets) != 1 {
		t.Fatalf("conn 1: expected 1 data packet, got %d", len(c1.Packets))
	}

	c2 := conns[2]
	if !c2.IsTLS {
		t.Fatal("conn 2: expected TLS")
	}
	if len(c2.Errors) != 1 || c2.Errors[0] != "fail" {
		t.Fatalf("conn 2: expected 1 error 'fail', got %v", c2.Errors)
	}
}

// ──────────────────────────────────────────────────
// Type String Tests
// ──────────────────────────────────────────────────

func TestDirectionString(t *testing.T) {
	tests := []struct {
		d    Direction
		want string
	}{
		{DirClientToProxy, "client→proxy"},
		{DirProxyToDest, "proxy→dest"},
		{DirDestToProxy, "dest→proxy"},
		{DirProxyToClient, "proxy→client"},
		{Direction(99), "unknown"},
	}
	for _, tc := range tests {
		if got := tc.d.String(); got != tc.want {
			t.Errorf("Direction(%d).String() = %q, want %q", tc.d, got, tc.want)
		}
	}
}

func TestPacketTypeString(t *testing.T) {
	tests := []struct {
		pt   PacketType
		want string
	}{
		{PacketTypeData, "DATA"},
		{PacketTypeConnOpen, "CONN_OPEN"},
		{PacketTypeConnClose, "CONN_CLOSE"},
		{PacketTypeProtocol, "PROTOCOL"},
		{PacketTypeError, "ERROR"},
		{PacketTypeDNS, "DNS"},
		{PacketType(99), "UNKNOWN"},
	}
	for _, tc := range tests {
		if got := tc.pt.String(); got != tc.want {
			t.Errorf("PacketType(%d).String() = %q, want %q", tc.pt, got, tc.want)
		}
	}
}

func TestProtocolString(t *testing.T) {
	tests := []struct {
		p    Protocol
		want string
	}{
		{ProtoHTTP, "HTTP"},
		{ProtoHTTP2, "HTTP2"},
		{ProtoGRPC, "gRPC"},
		{ProtoMySQL, "MySQL"},
		{ProtoPostgres, "Postgres"},
		{ProtoMongo, "MongoDB"},
		{ProtoRedis, "Redis"},
		{ProtoKafka, "Kafka"},
		{ProtoGeneric, "Generic"},
		{ProtoDNS, "DNS"},
		{ProtoUnknown, "Unknown"},
	}
	for _, tc := range tests {
		if got := tc.p.String(); got != tc.want {
			t.Errorf("Protocol(%d).String() = %q, want %q", tc.p, got, tc.want)
		}
	}
}

// ──────────────────────────────────────────────────
// Protocol-Specific Roundtrip Tests
// ──────────────────────────────────────────────────

func TestHTTPCapture(t *testing.T) {
	testProtocolRoundtrip(t, ProtoHTTP,
		[]byte("GET /api/users HTTP/1.1\r\nHost: example.com\r\nContent-Length: 0\r\n\r\n"),
		[]byte("HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 13\r\n\r\n{\"status\":\"ok\"}"))
}

func TestMySQLCapture(t *testing.T) {
	testProtocolRoundtrip(t, ProtoMySQL,
		[]byte{0x21, 0x00, 0x00, 0x00, 0x03, 0x53, 0x45, 0x4c, 0x45, 0x43, 0x54}, // MySQL COM_QUERY
		[]byte{0x01, 0x00, 0x00, 0x01, 0x01})
}

func TestPostgresCapture(t *testing.T) {
	testProtocolRoundtrip(t, ProtoPostgres,
		[]byte{0x51, 0x00, 0x00, 0x00, 0x0c, 0x53, 0x45, 0x4c, 0x45, 0x43, 0x54, 0x00}, // Postgres Query
		[]byte{0x54, 0x00, 0x00, 0x00, 0x06, 0x00, 0x00})
}

func TestRedisCapture(t *testing.T) {
	testProtocolRoundtrip(t, ProtoRedis,
		[]byte("*2\r\n$3\r\nGET\r\n$3\r\nkey\r\n"),
		[]byte("$5\r\nvalue\r\n"))
}

func TestGRPCCapture(t *testing.T) {
	// gRPC uses HTTP/2 frames
	testProtocolRoundtrip(t, ProtoGRPC,
		[]byte("PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n"),
		[]byte{0x00, 0x00, 0x04, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}) // SETTINGS frame
}

func TestMongoDBCapture(t *testing.T) {
	testProtocolRoundtrip(t, ProtoMongo,
		[]byte{0x39, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00}, // MongoDB OP_MSG
		[]byte{0x20, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00})
}

func TestKafkaCapture(t *testing.T) {
	testProtocolRoundtrip(t, ProtoKafka,
		[]byte{0x00, 0x00, 0x00, 0x1a, 0x00, 0x03, 0x00, 0x00}, // Kafka Metadata request
		[]byte{0x00, 0x00, 0x00, 0x0a, 0x00, 0x00, 0x00, 0x00})
}

func TestGenericBinaryCapture(t *testing.T) {
	// Test with pure binary data
	payload := make([]byte, 256)
	for i := range payload {
		payload[i] = byte(i)
	}
	testProtocolRoundtrip(t, ProtoGeneric, payload, payload)
}

func TestDNSCapture(t *testing.T) {
	testProtocolRoundtrip(t, ProtoDNS,
		[]byte{0xAA, 0xBB, 0x01, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}, // DNS query header
		[]byte{0xAA, 0xBB, 0x81, 0x80, 0x00, 0x01, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00})
}

func testProtocolRoundtrip(t *testing.T, proto Protocol, request, response []byte) {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, fmt.Sprintf("proto_%s.kpcap", proto))

	w, err := NewWriter(path, "record")
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}

	now := time.Now()
	pkts := []*Packet{
		{Timestamp: now, ConnectionID: 1, Type: PacketTypeConnOpen, SrcAddr: "127.0.0.1:50000", DstAddr: "10.0.0.1:5432"},
		{Timestamp: now.Add(time.Millisecond), ConnectionID: 1, Type: PacketTypeProtocol, Protocol: proto},
		{Timestamp: now.Add(2 * time.Millisecond), ConnectionID: 1, Type: PacketTypeData, Direction: DirClientToProxy, Protocol: proto, Payload: request},
		{Timestamp: now.Add(3 * time.Millisecond), ConnectionID: 1, Type: PacketTypeData, Direction: DirProxyToClient, Protocol: proto, Payload: response},
		{Timestamp: now.Add(4 * time.Millisecond), ConnectionID: 1, Type: PacketTypeConnClose},
	}

	for _, pkt := range pkts {
		if err := w.WritePacket(pkt); err != nil {
			t.Fatalf("WritePacket: %v", err)
		}
	}
	w.Close()

	// Read back and verify
	r, err := NewReader(path)
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	defer r.Close()

	cf, err := r.ReadAll()
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}

	if len(cf.Packets) != 5 {
		t.Fatalf("expected 5 packets, got %d", len(cf.Packets))
	}

	// Verify protocol detection
	if cf.Packets[1].Protocol != proto {
		t.Fatalf("expected protocol %s, got %s", proto, cf.Packets[1].Protocol)
	}

	// Verify request payload
	if !bytes.Equal(cf.Packets[2].Payload, request) {
		t.Fatalf("request payload mismatch for %s", proto)
	}

	// Verify response payload
	if !bytes.Equal(cf.Packets[3].Payload, response) {
		t.Fatalf("response payload mismatch for %s", proto)
	}

	// Verify the connection timeline
	conns := cf.GetConnections()
	if len(conns) != 1 {
		t.Fatalf("expected 1 connection, got %d", len(conns))
	}
	ct := conns[1]
	if ct.Protocol != proto {
		t.Fatalf("connection protocol: expected %s, got %s", proto, ct.Protocol)
	}
}

// ──────────────────────────────────────────────────
// Edge Case Tests
// ──────────────────────────────────────────────────

func TestMultipleConnectionsInterleaved(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "interleaved.kpcap")

	w, err := NewWriter(path, "record")
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}

	now := time.Now()
	// Simulate two interleaved connections
	pkts := []*Packet{
		{Timestamp: now, ConnectionID: 1, Type: PacketTypeConnOpen, SrcAddr: "a1", DstAddr: "b1"},
		{Timestamp: now.Add(time.Millisecond), ConnectionID: 2, Type: PacketTypeConnOpen, SrcAddr: "a2", DstAddr: "b2"},
		{Timestamp: now.Add(2 * time.Millisecond), ConnectionID: 1, Type: PacketTypeData, Direction: DirClientToProxy, Payload: []byte("conn1-req")},
		{Timestamp: now.Add(3 * time.Millisecond), ConnectionID: 2, Type: PacketTypeData, Direction: DirClientToProxy, Payload: []byte("conn2-req")},
		{Timestamp: now.Add(4 * time.Millisecond), ConnectionID: 1, Type: PacketTypeData, Direction: DirProxyToClient, Payload: []byte("conn1-resp")},
		{Timestamp: now.Add(5 * time.Millisecond), ConnectionID: 2, Type: PacketTypeData, Direction: DirProxyToClient, Payload: []byte("conn2-resp")},
		{Timestamp: now.Add(6 * time.Millisecond), ConnectionID: 1, Type: PacketTypeConnClose},
		{Timestamp: now.Add(7 * time.Millisecond), ConnectionID: 2, Type: PacketTypeConnClose},
	}

	for _, pkt := range pkts {
		w.WritePacket(pkt)
	}
	w.Close()

	r, err := NewReader(path)
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	defer r.Close()

	cf, err := r.ReadAll()
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}

	conns := cf.GetConnections()
	if len(conns) != 2 {
		t.Fatalf("expected 2 connections, got %d", len(conns))
	}

	c1 := conns[1]
	if c1.SrcAddr != "a1" {
		t.Fatalf("conn 1 src: expected 'a1', got %q", c1.SrcAddr)
	}
	if len(c1.Packets) != 2 {
		t.Fatalf("conn 1: expected 2 data packets, got %d", len(c1.Packets))
	}

	c2 := conns[2]
	if c2.SrcAddr != "a2" {
		t.Fatalf("conn 2 src: expected 'a2', got %q", c2.SrcAddr)
	}
	if len(c2.Packets) != 2 {
		t.Fatalf("conn 2: expected 2 data packets, got %d", len(c2.Packets))
	}
}

func TestUnicodeAndSpecialCharsInAddresses(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "special.kpcap")

	w, err := NewWriter(path, "record")
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}

	pkt := &Packet{
		Timestamp:    time.Now(),
		ConnectionID: 1,
		Type:         PacketTypeConnOpen,
		SrcAddr:      "[::1]:12345",                 // IPv6 loopback
		DstAddr:      "[2001:db8::1]:443",            // IPv6 address
	}
	w.WritePacket(pkt)
	w.Close()

	r, err := NewReader(path)
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	defer r.Close()

	readPkt, err := r.ReadPacket()
	if err != nil {
		t.Fatalf("ReadPacket: %v", err)
	}
	if readPkt.SrcAddr != "[::1]:12345" {
		t.Fatalf("src addr: expected [::1]:12345, got %q", readPkt.SrcAddr)
	}
	if readPkt.DstAddr != "[2001:db8::1]:443" {
		t.Fatalf("dst addr: expected [2001:db8::1]:443, got %q", readPkt.DstAddr)
	}
}

func TestTLSFlagRoundtrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tls.kpcap")

	w, err := NewWriter(path, "record")
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}

	w.WritePacket(&Packet{Timestamp: time.Now(), ConnectionID: 1, Type: PacketTypeConnOpen, IsTLS: true})
	w.WritePacket(&Packet{Timestamp: time.Now(), ConnectionID: 2, Type: PacketTypeConnOpen, IsTLS: false})
	w.Close()

	r, err := NewReader(path)
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	defer r.Close()

	pkt1, _ := r.ReadPacket()
	pkt2, _ := r.ReadPacket()

	if !pkt1.IsTLS {
		t.Fatal("expected TLS for conn 1")
	}
	if pkt2.IsTLS {
		t.Fatal("expected non-TLS for conn 2")
	}
}

// ──────────────────────────────────────────────────
// Helper
// ──────────────────────────────────────────────────

func newTestLogger() *zap.Logger {
	cfg := zap.NewDevelopmentConfig()
	cfg.Level = zap.NewAtomicLevelAt(zap.WarnLevel)
	l, _ := cfg.Build()
	return l
}
