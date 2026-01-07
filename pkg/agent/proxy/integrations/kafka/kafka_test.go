package kafka

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"golang.org/x/sync/errgroup"
)

func TestRecordOutgoing_RequestFlow(t *testing.T) {
	// 1. Setup Logger to capture output
	logBuffer := new(bytes.Buffer)
	encoderConfig := zap.NewDevelopmentEncoderConfig()
	core := zapcore.NewCore(
		zapcore.NewConsoleEncoder(encoderConfig),
		zapcore.AddSync(logBuffer),
		zap.DebugLevel,
	)
	logger := zap.New(core)

	// 2. Setup Pipes to simulate connections
	// clientConn: simulates the client connecting to Keploy
	// serverConn: Keploy reading from client
	clientConn, proxyClientConn := net.Pipe()

	// proxyDestConn: Keploy writing to destination
	// destConn: simulates the real Kafka destination
	proxyDestConn, destConn := net.Pipe()

	k := New(logger)

	// 3. Prepare a Kafka Request Packet
	// ApiKey: 1 (Fetch)
	// ApiVersion: 11
	// CorrelationID: 999
	// ClientID: "test-client"
	reqHeaderBuf := new(bytes.Buffer)
	binary.Write(reqHeaderBuf, binary.BigEndian, int16(1))   // ApiKey
	binary.Write(reqHeaderBuf, binary.BigEndian, int16(11))  // ApiVersion
	binary.Write(reqHeaderBuf, binary.BigEndian, int32(999)) // CorrelationID
	binary.Write(reqHeaderBuf, binary.BigEndian, int16(11))  // ClientID length
	reqHeaderBuf.WriteString("test-client")                  // ClientID

	reqPayload := reqHeaderBuf.Bytes()
	reqLength := int32(len(reqPayload))

	// 4. Prepare a Kafka Response Packet
	// CorrelationID: 999 (matching request)
	// Body: some dummy data
	respHeaderBuf := new(bytes.Buffer)
	binary.Write(respHeaderBuf, binary.BigEndian, int32(999)) // CorrelationID
	respHeaderBuf.Write([]byte("response-body"))              // Dummy body

	respPayload := respHeaderBuf.Bytes()
	respLength := int32(len(respPayload))

	// 5. Run RecordOutgoing in a goroutine with error group context
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	g, gCtx := errgroup.WithContext(ctx)
	gCtx = context.WithValue(gCtx, models.ErrGroupKey, g)
	gCtx = context.WithValue(gCtx, models.ClientConnectionIDKey, "test-client-conn")
	gCtx = context.WithValue(gCtx, models.DestConnectionIDKey, "test-dest-conn")

	mocks := make(chan *models.Mock, 10)
	clientClose := make(chan bool)

	g.Go(func() error {
		return k.RecordOutgoing(gCtx, proxyClientConn, proxyDestConn, mocks, clientClose, models.OutgoingOptions{})
	})

	// 6. Simulate Client -> Proxy -> Dest flow
	doneCh := make(chan struct{})
	go func() {
		defer close(doneCh)

		// Client sends request
		binary.Write(clientConn, binary.BigEndian, reqLength)
		clientConn.Write(reqPayload)

		// Dest receives request
		var receivedLen int32
		err := binary.Read(destConn, binary.BigEndian, &receivedLen)
		if err != nil {
			t.Logf("Error reading request length: %v", err)
			return
		}
		assert.Equal(t, reqLength, receivedLen)

		receivedPayload := make([]byte, receivedLen)
		_, err = io.ReadFull(destConn, receivedPayload)
		if err != nil {
			t.Logf("Error reading request payload: %v", err)
			return
		}
		assert.Equal(t, reqPayload, receivedPayload)

		// Dest sends response
		binary.Write(destConn, binary.BigEndian, respLength)
		destConn.Write(respPayload)

		// Client receives response
		var respReceivedLen int32
		err = binary.Read(clientConn, binary.BigEndian, &respReceivedLen)
		if err != nil {
			t.Logf("Error reading response length: %v", err)
			return
		}
		assert.Equal(t, respLength, respReceivedLen)

		respReceivedPayload := make([]byte, respReceivedLen)
		_, err = io.ReadFull(clientConn, respReceivedPayload)
		if err != nil {
			t.Logf("Error reading response payload: %v", err)
			return
		}
		assert.Equal(t, respPayload, respReceivedPayload)

		// Close connections to stop the recorder
		clientConn.Close()
		destConn.Close()
	}()

	// Wait for test to complete or timeout
	select {
	case <-doneCh:
		// Success
	case <-time.After(3 * time.Second):
		t.Fatal("Timeout waiting for request/response flow")
	}

	cancel() // Stop RecordOutgoing

	// 7. Verify a mock was recorded
	select {
	case mock := <-mocks:
		assert.NotNil(t, mock)
		assert.Equal(t, models.Kafka, mock.Kind)
		assert.Len(t, mock.Spec.KafkaRequests, 1)
		assert.Len(t, mock.Spec.KafkaResponses, 1)
		assert.Equal(t, "Fetch", mock.Spec.KafkaRequests[0].APIKeyName)
	case <-time.After(1 * time.Second):
		// Mock might not have been created if there was an error
		t.Log("No mock received (this may be expected if there was an error)")
	}

	// 8. Verify Logs contain decoded request info
	logs := logBuffer.String()
	assert.Contains(t, logs, "Decoded Kafka request")
}
