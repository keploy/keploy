package capture

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ══════════════════════════════════════════════════
// End-to-End Tests
// These tests simulate the full lifecycle:
// 1. Create a capture via the Manager
// 2. Validate the capture file
// 3. Analyze the capture file
// 4. Bundle the capture with mocks/config
// 5. Extract the bundle
// 6. Replay the capture against a mock server
// ══════════════════════════════════════════════════

func TestE2EFullLifecycle(t *testing.T) {
	dir := t.TempDir()

	// ── Step 1: Capture ──────────────────────────
	captureDir := filepath.Join(dir, "capture")
	mgr, err := NewManager(newTestLogger(), captureDir, "record")
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	// Simulate multiple connections with different protocols
	simulateHTTPConnection(mgr, 1)
	simulateMySQLConnection(mgr, 2)
	simulateRedisConnection(mgr, 3)
	simulatePostgresConnection(mgr, 4)
	simulateGenericConnection(mgr, 5)
	simulateErrorConnection(mgr, 6)

	stats := mgr.GetStats()
	if stats.TotalConnections != 6 {
		t.Fatalf("expected 6 connections, got %d", stats.TotalConnections)
	}

	captureFile := mgr.GetOutputPath()
	if err := mgr.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Verify file exists
	info, err := os.Stat(captureFile)
	if err != nil {
		t.Fatalf("capture file not found: %v", err)
	}
	if info.Size() == 0 {
		t.Fatal("capture file is empty")
	}

	// ── Step 2: Validate ─────────────────────────
	valResult, err := Validate(captureFile)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if !valResult.Valid {
		t.Fatalf("capture file invalid: %v", valResult.Errors)
	}
	if valResult.PacketCount == 0 {
		t.Fatal("expected non-zero packet count")
	}
	if valResult.ConnectionCount != 6 {
		t.Fatalf("expected 6 connections, got %d", valResult.ConnectionCount)
	}
	t.Logf("Validated: %d packets, %d bytes, %d connections",
		valResult.PacketCount, valResult.ByteCount, valResult.ConnectionCount)

	// ── Step 3: Analyze ──────────────────────────
	report, err := Analyze(captureFile)
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if len(report.Connections) != 6 {
		t.Fatalf("expected 6 connections in report, got %d", len(report.Connections))
	}

	// Check protocol breakdown
	if report.Protocols[ProtoHTTP] != 1 {
		t.Fatalf("expected 1 HTTP connection, got %d", report.Protocols[ProtoHTTP])
	}
	if report.Protocols[ProtoMySQL] != 2 { // includes the error connection which also uses MySQL protocol
		t.Fatalf("expected 2 MySQL connections, got %d", report.Protocols[ProtoMySQL])
	}
	if report.Protocols[ProtoRedis] != 1 {
		t.Fatalf("expected 1 Redis connection, got %d", report.Protocols[ProtoRedis])
	}

	// Check error detection
	if len(report.Errors) != 1 {
		t.Fatalf("expected 1 error, got %d", len(report.Errors))
	}

	// Verify report can be formatted
	formatted := FormatReport(report)
	if formatted == "" {
		t.Fatal("empty formatted report")
	}
	for _, keyword := range []string{"HTTP", "MySQL", "Redis", "Postgres", "Generic"} {
		if !strings.Contains(formatted, keyword) {
			t.Fatalf("expected %q in formatted report", keyword)
		}
	}
	t.Logf("Report contains all protocol mentions")

	// ── Step 4: Bundle ───────────────────────────
	// Create mock files
	mockDir := filepath.Join(dir, "mocks")
	os.MkdirAll(mockDir, 0755)
	os.WriteFile(filepath.Join(mockDir, "mock-1.yaml"), []byte("kind: HTTP\nversion: api.keploy.io/v1beta1\nspec:\n  req:\n    method: GET\n    url: /api/test"), 0644)
	os.WriteFile(filepath.Join(mockDir, "mock-2.yaml"), []byte("kind: MySQL\nversion: api.keploy.io/v1beta1\nspec:\n  req:\n    query: SELECT 1"), 0644)

	// Create config file
	configPath := filepath.Join(dir, "keploy.yml")
	os.WriteFile(configPath, []byte("path: .\nappName: e2e-test-app\nproxyPort: 16789"), 0644)

	// Create log file
	logPath := filepath.Join(dir, "debug.log")
	os.WriteFile(logPath, []byte("[INFO] Starting Keploy...\n[DEBUG] Proxy started at port:16789\n[ERROR] mock not found for connection\n"), 0644)

	bundlePath := filepath.Join(dir, "debug-bundle.tar.gz")
	bundleResult, err := CreateBundle(newTestLogger(), BundleOptions{
		CaptureFile: captureFile,
		MockDir:     mockDir,
		ConfigFile:  configPath,
		LogFile:     logPath,
		OutputPath:  bundlePath,
		AppName:     "e2e-test-app",
		Mode:        "record",
		Notes:       "E2E test: connection timeout on MySQL mock matching",
	})
	if err != nil {
		t.Fatalf("CreateBundle: %v", err)
	}
	bundleInfo, err := os.Stat(bundleResult)
	if err != nil {
		t.Fatalf("bundle file not found: %v", err)
	}
	if bundleInfo.Size() == 0 {
		t.Fatal("bundle is empty")
	}
	t.Logf("Bundle created: %s (%d bytes)", bundleResult, bundleInfo.Size())

	// ── Step 5: Extract ──────────────────────────
	extractDir := filepath.Join(dir, "extracted")
	manifest, err := ExtractBundle(bundleResult, extractDir)
	if err != nil {
		t.Fatalf("ExtractBundle: %v", err)
	}
	if manifest.AppName != "e2e-test-app" {
		t.Fatalf("manifest app: expected 'e2e-test-app', got %q", manifest.AppName)
	}
	if manifest.Mode != "record" {
		t.Fatalf("manifest mode: expected 'record', got %q", manifest.Mode)
	}
	if manifest.Notes != "E2E test: connection timeout on MySQL mock matching" {
		t.Fatalf("manifest notes mismatch")
	}

	// Verify extracted capture file can be read
	extractedCapturePath := filepath.Join(extractDir, manifest.CaptureFile)
	extractedReader, err := NewReader(extractedCapturePath)
	if err != nil {
		t.Fatalf("NewReader on extracted capture: %v", err)
	}
	extractedCF, err := extractedReader.ReadAll()
	extractedReader.Close()
	if err != nil {
		t.Fatalf("ReadAll on extracted capture: %v", err)
	}
	if len(extractedCF.Packets) != valResult.PacketCount {
		t.Fatalf("extracted capture packet count mismatch: expected %d, got %d",
			valResult.PacketCount, len(extractedCF.Packets))
	}
	t.Logf("Extracted and verified %d packets from bundle", len(extractedCF.Packets))

	// ── Step 6: Replay ───────────────────────────
	// Start a simple echo server for replay testing
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
					c.Write(buf[:n]) // echo back
				}
			}(conn)
		}
	}()

	// Create a simple capture for the echo server
	echoCapturePath := filepath.Join(dir, "echo.kpcap")
	echoWriter, err := NewWriter(echoCapturePath, "record")
	if err != nil {
		t.Fatalf("NewWriter for echo: %v", err)
	}

	now := time.Now()
	echoWriter.WritePacket(&Packet{Timestamp: now, ConnectionID: 100, Type: PacketTypeConnOpen, SrcAddr: "a", DstAddr: listener.Addr().String()})
	echoWriter.WritePacket(&Packet{Timestamp: now.Add(time.Millisecond), ConnectionID: 100, Type: PacketTypeData, Direction: DirClientToProxy, Payload: []byte("echo-test-data")})
	echoWriter.WritePacket(&Packet{Timestamp: now.Add(2 * time.Millisecond), ConnectionID: 100, Type: PacketTypeData, Direction: DirProxyToClient, Payload: []byte("echo-test-data")})
	echoWriter.WritePacket(&Packet{Timestamp: now.Add(3 * time.Millisecond), ConnectionID: 100, Type: PacketTypeConnClose})
	echoWriter.Close()

	replayer := NewReplayer(newTestLogger(), listener.Addr().String(), 5*time.Second)
	summary, err := replayer.ReplayFile(context.Background(), echoCapturePath)
	if err != nil {
		t.Fatalf("ReplayFile: %v", err)
	}
	if summary.MatchedConns != 1 {
		t.Fatalf("expected 1 matched connection, got %d", summary.MatchedConns)
		for _, r := range summary.Results {
			for _, e := range r.Errors {
				t.Logf("  error: %s", e)
			}
		}
	}
	t.Logf("Replay: %d/%d connections matched", summary.MatchedConns, summary.ReplayedConns)
}

func TestE2EConcurrentCapture(t *testing.T) {
	dir := t.TempDir()
	captureDir := filepath.Join(dir, "capture")

	mgr, err := NewManager(newTestLogger(), captureDir, "record")
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	// Simulate 20 concurrent connections
	const numConns = 20
	done := make(chan struct{}, numConns)

	for i := uint64(0); i < numConns; i++ {
		go func(connID uint64) {
			defer func() { done <- struct{}{} }()

			mgr.RecordConnectionOpen(connID, fmt.Sprintf("127.0.0.1:%d", 50000+connID), fmt.Sprintf("10.0.0.1:%d", 3306), false)
			mgr.RecordProtocolDetected(connID, ProtoMySQL)

			// Simulate multiple request/response exchanges
			for j := 0; j < 10; j++ {
				mgr.RecordData(connID, DirClientToProxy,
					[]byte(fmt.Sprintf("SELECT * FROM table_%d WHERE id = %d", connID, j)), ProtoMySQL)
				mgr.RecordData(connID, DirProxyToClient,
					[]byte(fmt.Sprintf("row_%d_%d", connID, j)), ProtoMySQL)
			}

			mgr.RecordConnectionClose(connID)
		}(i)
	}

	// Wait for all goroutines
	for i := 0; i < numConns; i++ {
		<-done
	}

	stats := mgr.GetStats()
	if stats.TotalConnections != numConns {
		t.Fatalf("expected %d connections, got %d", numConns, stats.TotalConnections)
	}
	expectedPackets := uint64(numConns * 10 * 2) // 10 req/resp pairs per connection
	if stats.TotalPackets != expectedPackets {
		t.Fatalf("expected %d packets, got %d", expectedPackets, stats.TotalPackets)
	}
	if stats.DroppedPackets != 0 {
		t.Fatalf("expected 0 dropped packets, got %d", stats.DroppedPackets)
	}

	capturePath := mgr.GetOutputPath()
	mgr.Close()

	// Validate the file
	result, err := Validate(capturePath)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if !result.Valid {
		t.Fatalf("invalid: %v", result.Errors)
	}
	// Total packets: numConns * (1 open + 1 protocol + 20 data + 1 close) = numConns * 23
	expectedTotal := numConns * 23
	if result.PacketCount != expectedTotal {
		t.Fatalf("expected %d total packets in file, got %d", expectedTotal, result.PacketCount)
	}

	t.Logf("Concurrent capture: %d connections, %d packets, %d bytes",
		result.ConnectionCount, result.PacketCount, result.ByteCount)
}

func TestE2EMultiProtocolAnalysis(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "multi.kpcap")

	w, err := NewWriter(path, "record")
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}

	now := time.Now()

	// HTTP connection
	writeConnectionPackets(w, 1, now, ProtoHTTP,
		[]byte("GET /api/health HTTP/1.1\r\nHost: localhost\r\n\r\n"),
		[]byte("HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nOK"),
		"127.0.0.1:50001", "10.0.0.1:80", false)

	// HTTPS (TLS) connection
	writeConnectionPackets(w, 2, now.Add(10*time.Millisecond), ProtoHTTP,
		[]byte("GET /api/secure HTTP/1.1\r\nHost: secure.example.com\r\n\r\n"),
		[]byte("HTTP/1.1 200 OK\r\n\r\n{\"secure\":true}"),
		"127.0.0.1:50002", "10.0.0.1:443", true)

	// MySQL connection
	writeConnectionPackets(w, 3, now.Add(20*time.Millisecond), ProtoMySQL,
		[]byte("SELECT * FROM users WHERE id=1"),
		[]byte{0x01, 0x00, 0x00, 0x01, 0x02, 0x00, 0x00, 0x02},
		"127.0.0.1:50003", "10.0.0.2:3306", false)

	// Redis connection
	writeConnectionPackets(w, 4, now.Add(30*time.Millisecond), ProtoRedis,
		[]byte("*2\r\n$3\r\nGET\r\n$4\r\nuser\r\n"),
		[]byte("$15\r\n{\"name\":\"test\"}\r\n"),
		"127.0.0.1:50004", "10.0.0.3:6379", false)

	// Postgres connection
	writeConnectionPackets(w, 5, now.Add(40*time.Millisecond), ProtoPostgres,
		[]byte{0x51, 0x00, 0x00, 0x00, 0x0e, 0x53, 0x45, 0x4c, 0x45, 0x43, 0x54, 0x20, 0x31, 0x00},
		[]byte{0x54, 0x00, 0x00, 0x00, 0x21},
		"127.0.0.1:50005", "10.0.0.4:5432", false)

	// gRPC connection
	writeConnectionPackets(w, 6, now.Add(50*time.Millisecond), ProtoGRPC,
		[]byte("PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n"),
		[]byte{0x00, 0x00, 0x04, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00},
		"127.0.0.1:50006", "10.0.0.5:50051", true)

	// MongoDB connection
	writeConnectionPackets(w, 7, now.Add(60*time.Millisecond), ProtoMongo,
		[]byte{0x47, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0xdd, 0x07, 0x00, 0x00},
		[]byte{0x33, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00},
		"127.0.0.1:50007", "10.0.0.6:27017", false)

	// Kafka connection
	writeConnectionPackets(w, 8, now.Add(70*time.Millisecond), ProtoKafka,
		[]byte{0x00, 0x00, 0x00, 0x23, 0x00, 0x00, 0x00, 0x04, 0x00, 0x00, 0x00, 0x00},
		[]byte{0x00, 0x00, 0x00, 0x1a, 0x00, 0x00, 0x00, 0x00},
		"127.0.0.1:50008", "10.0.0.7:9092", false)

	// DNS connection
	writeConnectionPackets(w, 9, now.Add(80*time.Millisecond), ProtoDNS,
		[]byte{0xAA, 0xBB, 0x01, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00},
		[]byte{0xAA, 0xBB, 0x81, 0x80, 0x00, 0x01, 0x00, 0x01},
		"127.0.0.1:50009", "10.0.0.8:53", false)

	// Generic binary connection
	binaryPayload := make([]byte, 512)
	for i := range binaryPayload {
		binaryPayload[i] = byte(i % 256)
	}
	writeConnectionPackets(w, 10, now.Add(90*time.Millisecond), ProtoGeneric,
		binaryPayload,
		binaryPayload,
		"127.0.0.1:50010", "10.0.0.9:9999", false)

	w.Close()

	// Analyze the multi-protocol capture
	report, err := Analyze(path)
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}

	if len(report.Connections) != 10 {
		t.Fatalf("expected 10 connections, got %d", len(report.Connections))
	}

	// Verify all protocols are represented
	expectedProtocols := map[Protocol]int{
		ProtoHTTP:     2, // HTTP + HTTPS
		ProtoMySQL:    1,
		ProtoRedis:    1,
		ProtoPostgres: 1,
		ProtoGRPC:     1,
		ProtoMongo:    1,
		ProtoKafka:    1,
		ProtoDNS:      1,
		ProtoGeneric:  1,
	}
	for proto, expectedCount := range expectedProtocols {
		if report.Protocols[proto] != expectedCount {
			t.Errorf("protocol %s: expected %d, got %d", proto, expectedCount, report.Protocols[proto])
		}
	}

	// Verify TLS connections
	tlsCount := 0
	for _, conn := range report.Connections {
		if conn.IsTLS {
			tlsCount++
		}
	}
	if tlsCount != 2 { // HTTPS + gRPC
		t.Fatalf("expected 2 TLS connections, got %d", tlsCount)
	}

	// Verify formatted report contains all protocol names
	formatted := FormatReport(report)
	for _, keyword := range []string{"HTTP", "MySQL", "Redis", "Postgres", "gRPC", "MongoDB", "Kafka", "DNS", "Generic"} {
		if !strings.Contains(formatted, keyword) {
			t.Errorf("expected %q in formatted report", keyword)
		}
	}
	if !strings.Contains(formatted, "(TLS)") {
		t.Error("expected '(TLS)' in formatted report")
	}

	t.Logf("Multi-protocol analysis complete: %d connections across %d protocols",
		len(report.Connections), len(report.Protocols))
}

func TestE2EReplayWithMismatchDiagnostics(t *testing.T) {
	// Test that replay correctly identifies mismatches and provides useful diagnostics

	// Start a server that modifies responses (simulating a "bug fix")
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
					// "Fixed" server responds with modified data
					resp := bytes.Replace(buf[:n], []byte("error"), []byte("fixed"), 1)
					c.Write(resp)
				}
			}(conn)
		}
	}()

	dir := t.TempDir()
	path := filepath.Join(dir, "diagnostic.kpcap")
	w, err := NewWriter(path, "record")
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}

	now := time.Now()
	// Capture from "before fix" - server responds with "error"
	w.WritePacket(&Packet{Timestamp: now, ConnectionID: 1, Type: PacketTypeConnOpen, SrcAddr: "a", DstAddr: listener.Addr().String()})
	w.WritePacket(&Packet{Timestamp: now.Add(time.Millisecond), ConnectionID: 1, Type: PacketTypeData, Direction: DirClientToProxy, Payload: []byte("request with error in response")})
	// Expected response was "error" in old version
	w.WritePacket(&Packet{Timestamp: now.Add(2 * time.Millisecond), ConnectionID: 1, Type: PacketTypeData, Direction: DirProxyToClient, Payload: []byte("request with error in response")})
	w.WritePacket(&Packet{Timestamp: now.Add(3 * time.Millisecond), ConnectionID: 1, Type: PacketTypeConnClose})
	w.Close()

	replayer := NewReplayer(newTestLogger(), listener.Addr().String(), 5*time.Second)
	summary, err := replayer.ReplayFile(context.Background(), path)
	if err != nil {
		t.Fatalf("ReplayFile: %v", err)
	}

	// Should detect mismatch because server now responds with "fixed" instead of "error"
	if summary.FailedConns != 1 {
		t.Fatalf("expected 1 failed connection (mismatch), got %d", summary.FailedConns)
	}

	result := summary.Results[0]
	if len(result.ByteMismatches) == 0 {
		t.Fatal("expected byte mismatches to be recorded")
	}

	mismatch := result.ByteMismatches[0]
	// The "error" -> "fixed" change starts at the position of "error" in the string
	if mismatch.Offset < 0 {
		t.Fatal("expected positive offset for mismatch")
	}

	t.Logf("Mismatch detected at offset %d: expected %d bytes, got %d bytes",
		mismatch.Offset, mismatch.Expected, mismatch.Actual)
}

func TestE2EBundleWithAllComponents(t *testing.T) {
	dir := t.TempDir()

	// Create capture
	capDir := filepath.Join(dir, "debug")
	mgr, err := NewManager(newTestLogger(), capDir, "test")
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	simulateHTTPConnection(mgr, 1)
	simulateMySQLConnection(mgr, 2)
	capPath := mgr.GetOutputPath()
	mgr.Close()

	// Create mock files
	mockDir := filepath.Join(dir, "keploy", "mocks")
	os.MkdirAll(mockDir, 0755)
	os.WriteFile(filepath.Join(mockDir, "mock-http-1.yaml"), []byte("kind: Http\nversion: api.keploy.io/v1beta1"), 0644)
	os.WriteFile(filepath.Join(mockDir, "mock-mysql-1.yaml"), []byte("kind: MySQL\nversion: api.keploy.io/v1beta1"), 0644)

	// Create test files
	testDir := filepath.Join(dir, "keploy", "tests")
	os.MkdirAll(testDir, 0755)
	os.WriteFile(filepath.Join(testDir, "test-1.yaml"), []byte("kind: Http\nname: test-1"), 0644)

	// Create config
	configPath := filepath.Join(dir, "keploy.yml")
	os.WriteFile(configPath, []byte("path: .\nappName: bundle-test\ncommand: ./app\nproxyPort: 16789"), 0644)

	// Create log file
	logPath := filepath.Join(dir, "keploy-debug.log")
	os.WriteFile(logPath, []byte("2024-01-01T12:00:00Z INFO Starting\n2024-01-01T12:00:01Z ERROR mock mismatch on connection 2\n"), 0644)

	// Create bundle
	bundlePath := filepath.Join(dir, "full-bundle.tar.gz")
	result, err := CreateBundle(newTestLogger(), BundleOptions{
		CaptureFile: capPath,
		MockDir:     mockDir,
		TestDir:     testDir,
		ConfigFile:  configPath,
		LogFile:     logPath,
		OutputPath:  bundlePath,
		AppName:     "bundle-test",
		Mode:        "test",
		Notes:       "MySQL mock matching failure on test-set-0:test-1",
	})
	if err != nil {
		t.Fatalf("CreateBundle: %v", err)
	}

	// Extract and verify everything
	extractDir := filepath.Join(dir, "verify")
	manifest, err := ExtractBundle(result, extractDir)
	if err != nil {
		t.Fatalf("ExtractBundle: %v", err)
	}

	// Verify manifest
	if manifest.AppName != "bundle-test" {
		t.Fatalf("manifest app: %q", manifest.AppName)
	}
	if manifest.Mode != "test" {
		t.Fatalf("manifest mode: %q", manifest.Mode)
	}

	// Verify capture file is valid
	extractedCapture := filepath.Join(extractDir, manifest.CaptureFile)
	valResult, err := Validate(extractedCapture)
	if err != nil {
		t.Fatalf("Validate extracted capture: %v", err)
	}
	if !valResult.Valid {
		t.Fatalf("extracted capture invalid: %v", valResult.Errors)
	}

	t.Logf("Bundle verified: %s, %d packets", result, valResult.PacketCount)
}

// ──────────────────────────────────────────────────
// Simulation helpers
// ──────────────────────────────────────────────────

func simulateHTTPConnection(mgr *Manager, connID uint64) {
	mgr.RecordConnectionOpen(connID, fmt.Sprintf("127.0.0.1:%d", 50000+connID), "10.0.0.1:80", false)
	mgr.RecordProtocolDetected(connID, ProtoHTTP)
	mgr.RecordData(connID, DirClientToProxy, []byte("GET /api/users HTTP/1.1\r\nHost: example.com\r\n\r\n"), ProtoHTTP)
	mgr.RecordData(connID, DirProxyToClient, []byte("HTTP/1.1 200 OK\r\nContent-Type: application/json\r\n\r\n{\"users\":[]}"), ProtoHTTP)
	mgr.RecordConnectionClose(connID)
}

func simulateMySQLConnection(mgr *Manager, connID uint64) {
	mgr.RecordConnectionOpen(connID, fmt.Sprintf("127.0.0.1:%d", 50000+connID), "10.0.0.2:3306", false)
	mgr.RecordProtocolDetected(connID, ProtoMySQL)
	// MySQL handshake + query
	mgr.RecordData(connID, DirClientToProxy, []byte{0x21, 0x00, 0x00, 0x00, 0x03, 0x53, 0x45, 0x4c, 0x45, 0x43, 0x54, 0x20, 0x31}, ProtoMySQL)
	mgr.RecordData(connID, DirProxyToClient, []byte{0x01, 0x00, 0x00, 0x01, 0x01, 0x27, 0x00, 0x00, 0x02}, ProtoMySQL)
	mgr.RecordConnectionClose(connID)
}

func simulateRedisConnection(mgr *Manager, connID uint64) {
	mgr.RecordConnectionOpen(connID, fmt.Sprintf("127.0.0.1:%d", 50000+connID), "10.0.0.3:6379", false)
	mgr.RecordProtocolDetected(connID, ProtoRedis)
	mgr.RecordData(connID, DirClientToProxy, []byte("*2\r\n$3\r\nGET\r\n$7\r\nsession\r\n"), ProtoRedis)
	mgr.RecordData(connID, DirProxyToClient, []byte("$36\r\nabcdef-1234-5678-ghij-klmnopqrstuv\r\n"), ProtoRedis)
	mgr.RecordConnectionClose(connID)
}

func simulatePostgresConnection(mgr *Manager, connID uint64) {
	mgr.RecordConnectionOpen(connID, fmt.Sprintf("127.0.0.1:%d", 50000+connID), "10.0.0.4:5432", false)
	mgr.RecordProtocolDetected(connID, ProtoPostgres)
	mgr.RecordData(connID, DirClientToProxy, []byte{0x51, 0x00, 0x00, 0x00, 0x0e, 0x53, 0x45, 0x4c, 0x45, 0x43, 0x54, 0x20, 0x31, 0x00}, ProtoPostgres)
	mgr.RecordData(connID, DirProxyToClient, []byte{0x54, 0x00, 0x00, 0x00, 0x21, 0x00, 0x01}, ProtoPostgres)
	mgr.RecordConnectionClose(connID)
}

func simulateGenericConnection(mgr *Manager, connID uint64) {
	mgr.RecordConnectionOpen(connID, fmt.Sprintf("127.0.0.1:%d", 50000+connID), "10.0.0.5:9999", false)
	mgr.RecordProtocolDetected(connID, ProtoGeneric)
	payload := make([]byte, 128)
	for i := range payload {
		payload[i] = byte(i)
	}
	mgr.RecordData(connID, DirClientToProxy, payload, ProtoGeneric)
	mgr.RecordData(connID, DirProxyToClient, payload, ProtoGeneric)
	mgr.RecordConnectionClose(connID)
}

func simulateErrorConnection(mgr *Manager, connID uint64) {
	mgr.RecordConnectionOpen(connID, fmt.Sprintf("127.0.0.1:%d", 50000+connID), "10.0.0.6:3306", false)
	mgr.RecordProtocolDetected(connID, ProtoMySQL)
	mgr.RecordData(connID, DirClientToProxy, []byte("SELECT * FROM nonexistent"), ProtoMySQL)
	mgr.RecordError(connID, "mock not found for MySQL connection")
	mgr.RecordConnectionClose(connID)
}

func writeConnectionPackets(w *Writer, connID uint64, baseTime time.Time, proto Protocol, request, response []byte, srcAddr, dstAddr string, isTLS bool) {
	w.WritePacket(&Packet{Timestamp: baseTime, ConnectionID: connID, Type: PacketTypeConnOpen, SrcAddr: srcAddr, DstAddr: dstAddr, IsTLS: isTLS})
	w.WritePacket(&Packet{Timestamp: baseTime.Add(time.Millisecond), ConnectionID: connID, Type: PacketTypeProtocol, Protocol: proto})
	w.WritePacket(&Packet{Timestamp: baseTime.Add(2 * time.Millisecond), ConnectionID: connID, Type: PacketTypeData, Direction: DirClientToProxy, Protocol: proto, Payload: request})
	w.WritePacket(&Packet{Timestamp: baseTime.Add(3 * time.Millisecond), ConnectionID: connID, Type: PacketTypeData, Direction: DirProxyToClient, Protocol: proto, Payload: response})
	w.WritePacket(&Packet{Timestamp: baseTime.Add(4 * time.Millisecond), ConnectionID: connID, Type: PacketTypeConnClose})
}
