package main

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"os"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"golang.org/x/sync/errgroup"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/agent/proxy"
	"go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.keploy.io/server/v3/pkg/models"
)

// MockDestInfo implements agent.DestInfo for testing
type MockDestInfo struct {
	DestPort uint32
}

func (m *MockDestInfo) Get(ctx context.Context, srcPort uint16) (*agent.NetworkAddress, error) {
	ip := net.ParseIP("127.0.0.1")
	ipInt, _ := util.ToIPV4(ip)
	// Always route to localhost:DestPort
	return &agent.NetworkAddress{
		Version:  4,
		IPv4Addr: ipInt,
		Port:     m.DestPort,
	}, nil
}

func (m *MockDestInfo) Delete(ctx context.Context, srcPort uint16) error {
	return nil
}

func main() {
	// 1. Setup Logger
	encoderConfig := zap.NewDevelopmentEncoderConfig()
	core := zapcore.NewCore(zapcore.NewConsoleEncoder(encoderConfig), zapcore.AddSync(os.Stdout), zap.DebugLevel)
	logger := zap.New(core)

	// 2. Start Fake Broker
	brokerPort := uint32(9092)
	go startFakeBroker(brokerPort)
	// Give broker time to start
	time.Sleep(100 * time.Millisecond)

	// 3. Setup Proxy
	proxyPort := uint32(0) // Let OS choose port
	destInfo := &MockDestInfo{DestPort: brokerPort}

	cfg := &config.Config{
		ProxyPort: proxyPort,
	}

	p := proxy.New(logger, destInfo, cfg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Setup error group for proxy
	g, gCtx := errgroup.WithContext(ctx)
	// Add error group to context as required by proxy
	gCtx = context.WithValue(gCtx, models.ErrGroupKey, g)

	// Start Proxy synchronously (it returns when ready)
	logger.Info("Starting Proxy...")
	opts := agent.ProxyOptions{}
	err := p.StartProxy(gCtx, opts)
	if err != nil {
		logger.Error("Proxy failed to start", zap.Error(err))
		return
	}

	// Get actual port
	addr := p.Listener.Addr().(*net.TCPAddr)
	actualProxyPort := uint32(addr.Port)
	logger.Info("Proxy started", zap.Uint32("Port", actualProxyPort))

	// Call Record to initialize session
	mocks := make(chan *models.Mock, 1)
	err = p.Record(ctx, mocks, models.OutgoingOptions{})
	if err != nil {
		logger.Error("Failed to start recording", zap.Error(err))
		return
	}

	// 4. Run Request through Proxy
	logger.Info("Sending Request through Proxy...")
	sendRequestThroughProxy(actualProxyPort)

	// Wait for mock
	select {
	case mock := <-mocks:
		logger.Info("Received Mock!", zap.Any("Kind", mock.Kind), zap.Any("Name", mock.Name))
		if mock.Kind == models.Kafka {
			logger.Info("SUCCESS: Kafka Mock Recorded!")
		} else {
			logger.Error("FAILURE: Generic Mock Recorded instead of Kafka")
		}
	case <-time.After(2 * time.Second):
		logger.Error("Timeout waiting for mock")
	}

	// Wait a bit for logs to flush
	time.Sleep(1 * time.Second)
	logger.Info("Test Finished.")
}

func sendRequestThroughProxy(proxyPort uint32) {
	conn, err := net.Dial("tcp", fmt.Sprintf("localhost:%d", proxyPort))
	if err != nil {
		fmt.Printf("Failed to connect to proxy: %v\n", err)
		return
	}
	defer conn.Close()

	// Create ApiVersions Request (ApiKey 18, Version 0)
	packet := createApiVersionsRequest(1234, "test-client")

	_, err = conn.Write(packet)
	if err != nil {
		fmt.Printf("Failed to write to proxy: %v\n", err)
		return
	}

	// Read response
	resp := make([]byte, 1024)
	n, err := conn.Read(resp)
	if err != nil {
		fmt.Printf("Failed to read from proxy: %v\n", err)
		return
	}
	fmt.Printf("Received response: %d bytes\n", n)
}

func createApiVersionsRequest(correlationID int32, clientID string) []byte {
	clientIDBytes := []byte(clientID)
	headerSize := 2 + 2 + 4 + 2 + len(clientIDBytes) // apiKey + apiVersion + corrID + clientIDLen + clientID

	packet := make([]byte, 4+headerSize)

	// Size (everything after this field)
	binary.BigEndian.PutUint32(packet[0:4], uint32(headerSize))
	// ApiKey = 18 (ApiVersions)
	binary.BigEndian.PutUint16(packet[4:6], 18)
	// ApiVersion = 0
	binary.BigEndian.PutUint16(packet[6:8], 0)
	// CorrelationID
	binary.BigEndian.PutUint32(packet[8:12], uint32(correlationID))
	// ClientID
	binary.BigEndian.PutUint16(packet[12:14], uint16(len(clientIDBytes)))
	copy(packet[14:], clientIDBytes)

	return packet
}

func startFakeBroker(port uint32) {
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		fmt.Printf("Could not start fake broker: %v\n", err)
		return
	}
	defer ln.Close()
	fmt.Printf("Fake Kafka broker listening on :%d\n", port)

	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go handleBrokerConn(conn)
	}
}

func handleBrokerConn(conn net.Conn) {
	defer conn.Close()

	// Read request header size (4 bytes)
	header := make([]byte, 4)
	_, err := io.ReadFull(conn, header)
	if err != nil {
		return
	}

	size := binary.BigEndian.Uint32(header)
	payload := make([]byte, size)
	_, err = io.ReadFull(conn, payload)
	if err != nil {
		return
	}

	// Echo back with same correlation ID
	if len(payload) >= 8 {
		corrID := binary.BigEndian.Uint32(payload[4:8])

		resp := make([]byte, 8)
		binary.BigEndian.PutUint32(resp[0:4], 4)      // size
		binary.BigEndian.PutUint32(resp[4:8], corrID) // correlation ID
		conn.Write(resp)
	}
}
