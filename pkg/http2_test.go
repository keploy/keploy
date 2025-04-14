package pkg

import (
	"testing"

	"time"

	"bytes"
	"encoding/binary"

	"fmt"
	"io"

	"github.com/protocolbuffers/protoscope"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v2/pkg/models"
	"go.uber.org/zap"
)

// Test generated using Keploy
func TestStreamManager_StoreAndRehydrateHeaders_311(t *testing.T) {
	logger := zap.NewNop()
	sm := NewStreamManager(logger)

	reqHeaders := &models.GrpcHeaders{
		PseudoHeaders:   map[string]string{":method": "POST", ":path": "/service"},
		OrdinaryHeaders: map[string]string{"user-agent": "test-client", "req-key": "req-val"},
	}
	respHeaders := &models.GrpcHeaders{
		PseudoHeaders:   map[string]string{":status": "200"},
		OrdinaryHeaders: map[string]string{"content-type": "application/grpc", "resp-key": "resp-val"},
	}
	trailerHeaders := &models.GrpcHeaders{
		OrdinaryHeaders: map[string]string{"grpc-status": "0", "trailer-key": "trailer-val"},
	}

	// Store headers
	sm.storeHeaders(reqHeaders, true, false)
	sm.storeHeaders(respHeaders, false, false)
	sm.storeHeaders(trailerHeaders, false, true) // Trailers are always considered part of response context

	// --- Test Rehydration ---

	// 1. Rehydrate request headers (some existing, some new)
	partialReq := &models.GrpcHeaders{
		PseudoHeaders:   map[string]string{":method": "GET"}, // Overwrite existing
		OrdinaryHeaders: map[string]string{"req-key": "existing-val"},
	}
	sm.rehydrateHeaders(partialReq, true, false)
	assert.Equal(t, "GET", partialReq.PseudoHeaders[":method"])              // Existing overwritten
	assert.Equal(t, "/service", partialReq.PseudoHeaders[":path"])           // Added from store
	assert.Equal(t, "existing-val", partialReq.OrdinaryHeaders["req-key"])   // Not overwritten
	assert.Equal(t, "test-client", partialReq.OrdinaryHeaders["user-agent"]) // Added from store

	// 2. Rehydrate response headers (start with nil maps)
	emptyResp := &models.GrpcHeaders{} // No maps initialized
	sm.rehydrateHeaders(emptyResp, false, false)
	require.NotNil(t, emptyResp.PseudoHeaders)
	require.NotNil(t, emptyResp.OrdinaryHeaders)
	assert.Equal(t, "200", emptyResp.PseudoHeaders[":status"])
	assert.Equal(t, "application/grpc", emptyResp.OrdinaryHeaders["content-type"])
	assert.Equal(t, "resp-val", emptyResp.OrdinaryHeaders["resp-key"])

	// 3. Rehydrate trailer headers
	partialTrailer := &models.GrpcHeaders{
		OrdinaryHeaders: map[string]string{"grpc-status": "1"}, // Overwrite
	}
	sm.rehydrateHeaders(partialTrailer, false, true)
	assert.Equal(t, "1", partialTrailer.OrdinaryHeaders["grpc-status"])           // Overwritten
	assert.Equal(t, "trailer-val", partialTrailer.OrdinaryHeaders["trailer-key"]) // Added

	// 4. Rehydrate with nil input headers
	var nilHeaders *models.GrpcHeaders = nil
	sm.rehydrateHeaders(nilHeaders, true, false) // Should not panic
	assert.Nil(t, nilHeaders)

	// 5. Store nil headers
	sm.storeHeaders(nil, true, false)                        // Should not panic or modify stores
	assert.Equal(t, "req-val", sm.requestHeaders["req-key"]) // Verify store wasn't cleared
}

// Test generated using Keploy

func TestReadGRPCFrame_ReadScenarios_630(t *testing.T) {
	// Scenario 1: Successful read
	protoData := []byte{0x0A, 0x03, 'f', 'o', 'o'} // Example protobuf data
	var validFrameData bytes.Buffer
	validFrameData.WriteByte(0x00) // Compression flag
	lenBytes := make([]byte, 4)
	binary.BigEndian.PutUint32(lenBytes, uint32(len(protoData)))
	validFrameData.Write(lenBytes)  // Length
	validFrameData.Write(protoData) // Message data
	fullFrameBytes := validFrameData.Bytes()

	reader := bytes.NewReader(fullFrameBytes)
	frame, err := ReadGRPCFrame(reader)
	require.NoError(t, err)
	assert.Equal(t, fullFrameBytes, frame)

	// Scenario 2: EOF while reading compression flag
	readerEOF1 := bytes.NewReader([]byte{})
	frame, err = ReadGRPCFrame(readerEOF1)
	require.Error(t, err)
	assert.Equal(t, io.EOF, err, "Expected EOF error directly")

	// Scenario 3: EOF while reading length
	readerEOF2 := bytes.NewReader([]byte{0x00, 0x01, 0x02}) // Only 3 bytes available
	frame, err = ReadGRPCFrame(readerEOF2)
	require.Error(t, err)
	assert.ErrorIs(t, err, io.ErrUnexpectedEOF) // ReadFull returns ErrUnexpectedEOF
	assert.Contains(t, err.Error(), "failed to read message length")

	// Scenario 4: EOF while reading message data
	var partialFrameData bytes.Buffer
	partialFrameData.WriteByte(0x00)              // Compression flag
	binary.BigEndian.PutUint32(lenBytes, 10)      // Expect 10 bytes of data
	partialFrameData.Write(lenBytes)              // Length
	partialFrameData.Write([]byte{1, 2, 3, 4, 5}) // Only 5 bytes of data
	readerEOF3 := bytes.NewReader(partialFrameData.Bytes())

	frame, err = ReadGRPCFrame(readerEOF3)
	require.Error(t, err)
	assert.ErrorIs(t, err, io.ErrUnexpectedEOF)
	assert.Contains(t, err.Error(), "failed to read message")

	// Scenario 5: Other IO Error simulation (using a custom reader)
	errorReader := &errReader{err: fmt.Errorf("simulated IO error")}
	frame, err = ReadGRPCFrame(errorReader)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "simulated IO error")
	// The specific part of the error depends on where the error occurs first
	// assert.Contains(t, err.Error(), "failed to read compression flag") // Or length, or message

	// Scenario 6: Valid frame with zero length message
	var zeroLenFrameData bytes.Buffer
	zeroLenFrameData.WriteByte(0x01)        // Compression flag
	binary.BigEndian.PutUint32(lenBytes, 0) // Length 0
	zeroLenFrameData.Write(lenBytes)
	zeroLenBytes := zeroLenFrameData.Bytes()

	readerZero := bytes.NewReader(zeroLenBytes)
	frame, err = ReadGRPCFrame(readerZero)
	require.NoError(t, err)
	assert.Equal(t, zeroLenBytes, frame)

}

// Custom reader to simulate IO errors
type errReader struct {
	err       error
	readCount int
}

func (r *errReader) Read(p []byte) (n int, err error) {
	r.readCount++
	// Simulate error on the first read attempt
	if r.readCount == 1 {
		return 0, r.err
	}
	// Or allow one successful read then error
	// if r.readCount == 2 { return 0, r.err}
	// return len(p), nil // Simulate successful reads otherwise

	// Simulate immediate error for simplicity
	return 0, r.err
}

func TestGetCompleteStreams_ReturnsCompletedStreams_004(t *testing.T) {
	logger := zap.NewNop()
	sm := NewStreamManager(logger)

	sm.streams[1] = &HTTP2StreamState{
		ID:         1,
		isComplete: true,
		grpcReq:    &models.GrpcReq{},
		grpcResp:   &models.GrpcResp{},
		startTime:  time.Now(),
		endTime:    time.Now(),
	}
	sm.streams[2] = &HTTP2StreamState{
		ID:         2,
		isComplete: false,
		grpcReq:    &models.GrpcReq{},
		grpcResp:   &models.GrpcResp{},
		startTime:  time.Now(),
		endTime:    time.Now(),
	}

	completedStreams := sm.GetCompleteStreams()
	assert.Equal(t, 1, len(completedStreams))
	assert.Equal(t, uint32(1), completedStreams[0].ID)
}

// Test generated using Keploy

func TestIsGRPCGatewayRequest_Scenarios_315(t *testing.T) {
	// Scenario 1: Contains gateway header
	streamWithGateway := &HTTP2Stream{
		GRPCReq: &models.GrpcReq{
			Headers: models.GrpcHeaders{
				OrdinaryHeaders: map[string]string{
					"grpc-gateway-user-agent": "foo",
					"other-header":            "bar",
				},
			},
		},
	}
	assert.True(t, IsGRPCGatewayRequest(streamWithGateway))

	// Scenario 2: Does not contain gateway header
	streamWithoutGateway := &HTTP2Stream{
		GRPCReq: &models.GrpcReq{
			Headers: models.GrpcHeaders{
				OrdinaryHeaders: map[string]string{
					"user-agent":   "normal-grpc",
					"other-header": "bar",
				},
			},
		},
	}
	assert.False(t, IsGRPCGatewayRequest(streamWithoutGateway))

	// Scenario 3: Empty ordinary headers
	streamEmptyHeaders := &HTTP2Stream{
		GRPCReq: &models.GrpcReq{
			Headers: models.GrpcHeaders{
				OrdinaryHeaders: map[string]string{},
			},
		},
	}
	assert.False(t, IsGRPCGatewayRequest(streamEmptyHeaders))

	// Scenario 4: Nil GRPCReq
	streamNilReq := &HTTP2Stream{
		GRPCReq: nil,
	}
	assert.False(t, IsGRPCGatewayRequest(streamNilReq))

	// Scenario 5: Nil HTTP2Stream
	assert.False(t, IsGRPCGatewayRequest(nil))
}

// Test generated using Keploy

func TestCreateLengthPrefixedMessage_PayloadSizes_420(t *testing.T) {
	// Scenario 1: Payload length < 5
	shortPayload := []byte{0x00, 0x01, 0x02, 0x03}
	msg := CreateLengthPrefixedMessageFromPayload(shortPayload)
	assert.Zero(t, msg.CompressionFlag)
	assert.Zero(t, msg.MessageLength)
	assert.Empty(t, msg.DecodedData)

	// Scenario 2: Payload length == 5 (no actual message data)
	headerOnlyPayload := []byte{0x01, 0x00, 0x00, 0x00, 0x00} // Compression=1, Length=0
	msg = CreateLengthPrefixedMessageFromPayload(headerOnlyPayload)
	assert.Equal(t, uint(1), msg.CompressionFlag)
	assert.Equal(t, uint32(0), msg.MessageLength)
	assert.Empty(t, msg.DecodedData)

	// Scenario 3: Payload length > 5
	protoData := []byte{0x08, 0x96, 0x01} // Example protobuf data (field 1, value 150)
	var fullPayload bytes.Buffer
	fullPayload.WriteByte(0x00) // Compression flag
	lenBytes := make([]byte, 4)
	binary.BigEndian.PutUint32(lenBytes, uint32(len(protoData)))
	fullPayload.Write(lenBytes)  // Length
	fullPayload.Write(protoData) // Message data

	msg = CreateLengthPrefixedMessageFromPayload(fullPayload.Bytes())
	assert.Equal(t, uint(0), msg.CompressionFlag)
	assert.Equal(t, uint32(len(protoData)), msg.MessageLength)
	expectedDecoded := protoscope.Write(protoData, protoscope.WriterOptions{})
	assert.Equal(t, expectedDecoded, msg.DecodedData)

	// Scenario 4: Empty payload
	emptyPayload := []byte{}
	msg = CreateLengthPrefixedMessageFromPayload(emptyPayload)
	assert.Zero(t, msg.CompressionFlag)
	assert.Zero(t, msg.MessageLength)
	assert.Empty(t, msg.DecodedData)
}

// Test generated using Keploy

func TestCleanupStream_RemovesStream_005(t *testing.T) {
	logger := zap.NewNop()
	sm := NewStreamManager(logger)

	sm.streams[1] = &HTTP2StreamState{
		ID: 1,
	}

	sm.CleanupStream(1)
	_, exists := sm.streams[1]
	assert.False(t, exists)
}

// Test generated using Keploy

// Test generated using Keploy
