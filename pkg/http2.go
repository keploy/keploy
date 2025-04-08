// Package pkg provides common utilities for gRPC and HTTP/2 handling
package pkg

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/protocolbuffers/protoscope"
	"go.keploy.io/server/v2/pkg/models"
	"go.uber.org/zap"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
)

// HTTP/2 constants
const (
	// DefaultMaxFrameSize is the default maximum frame size as per HTTP/2 spec (16KB)
	DefaultMaxFrameSize = 16384
	// MaxFrameSize is the maximum allowed frame size (16MB)
	MaxFrameSize = 16777215 // 2^24 - 1
	// HTTP2Preface is the HTTP/2 connection preface
	HTTP2Preface = "PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n"
)

// ExtractHTTP2Frame attempts to extract an HTTP/2 frame from raw bytes
func ExtractHTTP2Frame(data []byte) (http2.Frame, int, error) {
	if len(data) < 9 { // Minimum frame size
		return nil, 0, fmt.Errorf("incomplete frame: got %d bytes, need at least 9", len(data))
	}

	// First, read the frame header to determine the full frame length
	// Length is a 24-bit unsigned integer
	length := (uint32(data[0]) << 16) | (uint32(data[1]) << 8) | uint32(data[2])

	// Validate length before proceeding
	if length > MaxFrameSize {
		return nil, 0, fmt.Errorf("frame length %d exceeds maximum allowed size %d", length, MaxFrameSize)
	}

	// Check if we have the complete frame
	totalSize := int(length) + 9 // frame header (9 bytes) + payload
	if len(data) < totalSize {
		return nil, 0, fmt.Errorf("incomplete frame: got %d bytes, need %d", len(data), totalSize)
	}

	// Create a reader for just this frame
	frameReader := bytes.NewReader(data[:totalSize])
	framer := http2.NewFramer(nil, frameReader)

	// Read the frame
	frame, err := framer.ReadFrame()
	if err != nil {
		return nil, 0, fmt.Errorf("failed to read frame: %v", err)
	}

	return frame, totalSize, nil
}

// HTTP2StreamState represents the state of an HTTP/2 stream
type HTTP2StreamState struct {
	// Stream identification
	ID        uint32
	RequestID string
	ParentID  string
	isRequest bool

	// Timing
	startTime time.Time
	endTime   time.Time

	// Message state
	headerBlockFragments [][]byte
	dataFrames           [][]byte
	isComplete           bool
	endStreamReceived    bool
	requestComplete      bool

	// gRPC specific state
	grpcReq  *models.GrpcReq
	grpcResp *models.GrpcResp

	// Header state
	headersReceived  bool
	trailersReceived bool
}

// HTTP2Stream represents a complete HTTP/2 stream
type HTTP2Stream struct {
	ID       uint32
	GRPCReq  *models.GrpcReq
	GRPCResp *models.GrpcResp
}

// DefaultStreamManager implements stream management
type DefaultStreamManager struct {
	mutex   sync.RWMutex
	streams map[uint32]*HTTP2StreamState
	// completed []*HTTP2Stream
	buffer  []byte
	logger  *zap.Logger
	decoder *hpack.Decoder
	// Context-aware header tables
	requestHeaders  map[string]string // For request headers
	responseHeaders map[string]string // For response headers
	trailerHeaders  map[string]string // For trailer headers
}

// NewStreamManager creates a new stream manager
func NewStreamManager(logger *zap.Logger) *DefaultStreamManager {
	return &DefaultStreamManager{
		streams: make(map[uint32]*HTTP2StreamState),
		buffer:  make([]byte, 0, DefaultMaxFrameSize),
		logger:  logger,
		decoder: hpack.NewDecoder(4096, nil),
		// Initialize separate header tables
		requestHeaders:  make(map[string]string),
		responseHeaders: make(map[string]string),
		trailerHeaders:  make(map[string]string),
	}
}

// storeHeaders stores headers in the appropriate connection header table based on context
func (sm *DefaultStreamManager) storeHeaders(headers *models.GrpcHeaders, isRequest bool, isTrailer bool) {
	if headers == nil {
		return
	}

	// Select the appropriate header table
	var headerTable map[string]string
	if isTrailer {
		headerTable = sm.trailerHeaders
	} else if isRequest {
		headerTable = sm.requestHeaders
	} else {
		headerTable = sm.responseHeaders
	}

	// Store pseudo headers
	for k, v := range headers.PseudoHeaders {
		headerTable[k] = v
	}

	// Store ordinary headers
	for k, v := range headers.OrdinaryHeaders {
		headerTable[k] = v
	}
}

// rehydrateHeaders adds missing headers from the appropriate connection header table
func (sm *DefaultStreamManager) rehydrateHeaders(headers *models.GrpcHeaders, isRequest bool, isTrailer bool) {
	if headers == nil {
		return
	}

	// Select the appropriate header table
	var headerTable map[string]string
	if isTrailer {
		headerTable = sm.trailerHeaders
	} else if isRequest {
		headerTable = sm.requestHeaders
	} else {
		headerTable = sm.responseHeaders
	}

	// Initialize maps if nil
	if headers.PseudoHeaders == nil {
		headers.PseudoHeaders = make(map[string]string)
	}
	if headers.OrdinaryHeaders == nil {
		headers.OrdinaryHeaders = make(map[string]string)
	}

	// Add missing headers from the connection's header table
	for k, v := range headerTable {
		if strings.HasPrefix(k, ":") {
			// This is a pseudo-header
			if _, exists := headers.PseudoHeaders[k]; !exists {
				headers.PseudoHeaders[k] = v
			}
		} else {
			// This is an ordinary header
			if _, exists := headers.OrdinaryHeaders[k]; !exists {
				headers.OrdinaryHeaders[k] = v
			}
		}
	}
}

// HandleFrame processes an HTTP/2 frame
func (sm *DefaultStreamManager) HandleFrame(frame http2.Frame, isOutgoing bool, frameTime time.Time) error {
	sm.mutex.Lock()
	defer sm.mutex.Unlock()

	streamID := frame.Header().StreamID

	// Initialize stream if it doesn't exist
	if _, exists := sm.streams[streamID]; !exists {
		prefix := "Incoming_"
		if isOutgoing {
			prefix = "Outgoing_"
		}
		requestID := fmt.Sprintf(prefix+"%d", streamID)
		sm.streams[streamID] = &HTTP2StreamState{
			ID:        streamID,
			RequestID: requestID,
			isRequest: streamID != 0, // Stream 0 is for connection control
			startTime: frameTime,
		}
	}

	stream := sm.streams[streamID]

	switch f := frame.(type) {
	case *http2.HeadersFrame:
		// Store header block fragments for later processing
		stream.headerBlockFragments = append(stream.headerBlockFragments, f.HeaderBlockFragment())

		if f.HeadersEnded() {
			// Process complete headers
			headerBlock := bytes.Join(stream.headerBlockFragments, nil)
			stream.headerBlockFragments = nil
			fields, err := sm.decoder.DecodeFull(headerBlock)
			if err != nil {
				return fmt.Errorf("failed to decode headers: %v", err)
			}

			for _, field := range fields {
				if field.Name == ":status" {
					stream.isRequest = false
				}
			}

			headers := ProcessHeaders(fields)

			if !stream.headersReceived {
				// These are initial headers
				if stream.isRequest {
					// Rehydrate from request headers
					sm.rehydrateHeaders(headers, true, false)

					stream.grpcReq = &models.GrpcReq{
						Headers: *headers,
					}
					// Store headers for future requests
					sm.storeHeaders(headers, true, false)
				} else {
					// Rehydrate from response headers
					sm.rehydrateHeaders(headers, false, false)

					stream.grpcResp = &models.GrpcResp{
						Headers: *headers,
					}
					// Store headers for future responses
					sm.storeHeaders(headers, false, false)
				}
				stream.headersReceived = true
			} else if !stream.trailersReceived && !stream.isRequest {
				// These are trailers (only for responses)
				// Rehydrate from trailer headers
				sm.rehydrateHeaders(headers, false, true)

				if stream.grpcResp != nil {
					stream.grpcResp.Trailers = *headers
					// Store headers for future trailers
					sm.storeHeaders(headers, false, true)
				}
				stream.trailersReceived = true
			}
		}

		if f.StreamEnded() {
			stream.endStreamReceived = true
			// Process the complete message
			if err := sm.processCompleteMessage(stream); err != nil {
				return err
			}
			sm.checkStreamCompletion(streamID)

			// Clear header fragments and data frames after processing request part
			if stream.isRequest {
				stream.headerBlockFragments = nil
				stream.endStreamReceived = false
				stream.headersReceived = false
				stream.trailersReceived = false
			} else {
				stream.endTime = frameTime
			}
		}

	case *http2.DataFrame:
		// Store data frames for later processing
		stream.dataFrames = append(stream.dataFrames, f.Data())

		if f.StreamEnded() {
			stream.endStreamReceived = true
			// Process the complete message
			if err := sm.processCompleteMessage(stream); err != nil {
				return err
			}
			sm.checkStreamCompletion(streamID)

			// Clear header fragments and data frames after processing request part
			if stream.isRequest {
				stream.headerBlockFragments = nil
				stream.endStreamReceived = false
				stream.headersReceived = false
				stream.trailersReceived = false
			} else {
				stream.endTime = frameTime
			}
		}
	}

	return nil
}

// GetCompleteStreams returns all completed HTTP/2 streams
func (sm *DefaultStreamManager) GetCompleteStreams() []*HTTP2Stream {
	sm.mutex.Lock()
	defer sm.mutex.Unlock()

	var completed []*HTTP2Stream

	for id, stream := range sm.streams {
		if stream.isComplete {
			stream.grpcReq.Timestamp = stream.startTime
			stream.grpcResp.Timestamp = stream.endTime
			http2Stream := &HTTP2Stream{
				ID:       id,
				GRPCReq:  stream.grpcReq,
				GRPCResp: stream.grpcResp,
			}
			completed = append(completed, http2Stream)
		}
	}

	return completed
}

// CleanupStream removes a stream from the manager
func (sm *DefaultStreamManager) CleanupStream(streamID uint32) {
	sm.mutex.Lock()
	defer sm.mutex.Unlock()
	delete(sm.streams, streamID)
}

// processCompleteMessage processes a complete HTTP/2 message
func (sm *DefaultStreamManager) processCompleteMessage(stream *HTTP2StreamState) error {
	if stream == nil {
		return fmt.Errorf("nil stream")
	}

	if len(stream.dataFrames) == 0 {
		return nil
	}

	// Combine all data frame fragments
	data := bytes.Join(stream.dataFrames, nil)

	// Process the complete gRPC message
	msg := CreateLengthPrefixedMessageFromPayload(data)

	if stream.isRequest {
		if stream.grpcReq == nil {
			stream.grpcReq = &models.GrpcReq{}
		}
		stream.grpcReq.Body = msg
	} else {
		if stream.grpcResp == nil {
			stream.grpcResp = &models.GrpcResp{}
		}
		stream.grpcResp.Body = msg
	}

	// Clear the data frames after processing
	stream.dataFrames = nil
	return nil
}

// checkStreamCompletion checks if a stream is complete and processes it accordingly
func (sm *DefaultStreamManager) checkStreamCompletion(streamID uint32) {
	stream := sm.streams[streamID]

	// For requests: mark the message part as complete but don't complete the stream
	if stream.isRequest {
		if stream.endStreamReceived && stream.headersReceived {
			// Mark request part as complete but keep stream open for response
			stream.requestComplete = true
		}
		return // Don't mark stream as complete yet
	}

	// For responses: check if both request and response are complete
	if !stream.isRequest && stream.requestComplete {
		if stream.endStreamReceived && stream.headersReceived && stream.trailersReceived { // For gRPC, ensure trailers are received

			sm.logger.Info("Stream completed", zap.Any("stream", stream))

			stream.isComplete = true
		}
	}
}

// ProcessHeaders converts HPACK header fields to a map
func ProcessHeaders(fields []hpack.HeaderField) *models.GrpcHeaders {
	headers := &models.GrpcHeaders{
		PseudoHeaders:   make(map[string]string),
		OrdinaryHeaders: make(map[string]string),
	}

	for _, field := range fields {
		if len(field.Name) > 0 && field.Name[0] == ':' {
			headers.PseudoHeaders[field.Name] = field.Value
		} else {
			headers.OrdinaryHeaders[field.Name] = field.Value
		}
	}

	return headers
}

// IsGRPCGatewayRequest checks if the stream appears to be from gRPC-gateway that proxies http requests to grpc services
func IsGRPCGatewayRequest(stream *HTTP2Stream) bool {
	if stream == nil || stream.GRPCReq == nil {
		return false
	}

	// Check for HTTP gateway specific headers
	headers := stream.GRPCReq.Headers.OrdinaryHeaders
	gatewayHeaders := []string{
		"grpc-gateway-user-agent",
	}

	for _, header := range gatewayHeaders {
		if _, exists := headers[header]; exists {
			return true
		}
	}

	return false
}

// SimulateGRPC simulates a gRPC call and returns the response
// This is a standalone version of the simulateGRPC method from Hooks
func SimulateGRPC(_ context.Context, tc *models.TestCase, testSetID string, logger *zap.Logger) (*models.GrpcResp, error) {
	grpcReq := tc.GrpcReq

	logger.Info("starting test for of", zap.Any("test case", models.HighlightString(tc.Name)), zap.Any("test set", models.HighlightString(testSetID)))

	// Create a TCP connection
	conn, err := net.Dial("tcp", grpcReq.Headers.PseudoHeaders[":authority"])
	if err != nil {
		return nil, fmt.Errorf("failed to dial: %w", err)
	}
	defer func() {
		if cerr := conn.Close(); cerr != nil {
			logger.Error("failed to close connection", zap.Error(cerr))
		}
	}()

	// Write HTTP/2 connection preface
	if _, err := conn.Write([]byte(http2.ClientPreface)); err != nil {
		return nil, fmt.Errorf("failed to write client preface: %w", err)
	}

	// Create HTTP/2 client connection
	framer := http2.NewFramer(conn, conn)
	framer.AllowIllegalWrites = true // Allow HTTP/2 without TLS

	// Initial sequence of frames that gRPC sends:
	// 1. Empty SETTINGS frame
	// 2. Wait for server SETTINGS and ACK
	// 3. Send SETTINGS ACK
	// 4. HEADERS frame
	// 5. DATA frame
	// 6. WINDOW_UPDATE frame (connection-level)
	// 7. PING frame

	// Send initial empty SETTINGS frame
	err = framer.WriteSettings()
	if err != nil {
		return nil, fmt.Errorf("failed to write settings: %w", err)
	}

	// Wait for server's SETTINGS and SETTINGS ACK
	settingsReceived := false
	settingsAckReceived := false

	for !settingsReceived || !settingsAckReceived {
		frame, err := framer.ReadFrame()
		if err != nil {
			return nil, fmt.Errorf("failed to read server settings: %w", err)
		}

		switch f := frame.(type) {
		case *http2.SettingsFrame:
			if f.IsAck() {
				settingsAckReceived = true
			} else {
				settingsReceived = true
				// Send ACK for server SETTINGS
				if err := framer.WriteSettingsAck(); err != nil {
					return nil, fmt.Errorf("failed to write settings ack: %w", err)
				}
			}
		}
	}

	// Create stream ID (client streams are odd-numbered)
	streamID := uint32(1)

	// Prepare HEADERS frame
	headerBuf := new(bytes.Buffer)
	encoder := hpack.NewEncoder(headerBuf)

	// Write pseudo-headers first (order matters in HTTP/2)
	pseudoHeaders := []struct {
		name, value string
	}{
		{":method", grpcReq.Headers.PseudoHeaders[":method"]},
		{":scheme", grpcReq.Headers.PseudoHeaders[":scheme"]},
		{":authority", grpcReq.Headers.PseudoHeaders[":authority"]},
		{":path", grpcReq.Headers.PseudoHeaders[":path"]},
	}

	for _, ph := range pseudoHeaders {
		if err := encoder.WriteField(hpack.HeaderField{Name: ph.name, Value: ph.value}); err != nil {
			return nil, fmt.Errorf("failed to encode pseudo-header %s: %w", ph.name, err)
		}
	}

	// Write regular headers in a specific order
	orderedHeaders := []struct {
		name, value string
	}{}

	// Add any remaining headers from the request
	for k, v := range grpcReq.Headers.OrdinaryHeaders {
		orderedHeaders = append(orderedHeaders, struct{ name, value string }{k, v})
	}

	for _, h := range orderedHeaders {
		if err := encoder.WriteField(hpack.HeaderField{Name: h.name, Value: h.value}); err != nil {
			return nil, fmt.Errorf("failed to encode header %s: %w", h.name, err)
		}
	}

	// Send HEADERS frame
	if err := framer.WriteHeaders(http2.HeadersFrameParam{
		StreamID:      streamID,
		BlockFragment: headerBuf.Bytes(),
		EndHeaders:    true,
		EndStream:     false,
	}); err != nil {
		return nil, fmt.Errorf("failed to write headers: %w", err)
	}

	// Create and send DATA frame
	payload, err := CreatePayloadFromLengthPrefixedMessage(grpcReq.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to create payload: %w", err)
	}

	if err := framer.WriteData(streamID, true, payload); err != nil {
		return nil, fmt.Errorf("failed to write data: %w", err)
	}

	// Send WINDOW_UPDATE frame for connection (stream 0)
	if err := framer.WriteWindowUpdate(0, 983041); err != nil {
		return nil, fmt.Errorf("failed to write window update: %w", err)
	}

	// Send PING frame
	pingData := [8]byte{1, 2, 3, 4, 5, 6, 7, 8} // Example ping data
	if err := framer.WritePing(false, pingData); err != nil {
		return nil, fmt.Errorf("failed to write ping: %w", err)
	}

	// Read response frames
	grpcResp := &models.GrpcResp{
		Headers: models.GrpcHeaders{
			PseudoHeaders:   make(map[string]string),
			OrdinaryHeaders: make(map[string]string),
		},
		Body: models.GrpcLengthPrefixedMessage{},
		Trailers: models.GrpcHeaders{
			PseudoHeaders:   make(map[string]string),
			OrdinaryHeaders: make(map[string]string),
		},
	}

	// Read frames until we get the end of stream
	streamEnded := false
	for !streamEnded {
		frame, err := framer.ReadFrame()
		if err != nil {
			if err == io.EOF {
				break
			}
			return nil, fmt.Errorf("failed to read frame: %w", err)
		}

		switch f := frame.(type) {
		case *http2.HeadersFrame:
			// If we already have headers, this must be trailers
			if len(grpcResp.Headers.OrdinaryHeaders) > 0 || len(grpcResp.Headers.PseudoHeaders) > 0 {
				decoder := hpack.NewDecoder(4096, func(f hpack.HeaderField) {
					if strings.HasPrefix(f.Name, ":") {
						grpcResp.Trailers.PseudoHeaders[f.Name] = f.Value
					} else {
						grpcResp.Trailers.OrdinaryHeaders[f.Name] = f.Value
					}
				})
				if _, err := decoder.Write(f.HeaderBlockFragment()); err != nil {
					return nil, fmt.Errorf("failed to decode trailers: %w", err)
				}
			} else {
				// These are headers
				decoder := hpack.NewDecoder(4096, func(f hpack.HeaderField) {
					if strings.HasPrefix(f.Name, ":") {
						grpcResp.Headers.PseudoHeaders[f.Name] = f.Value
					} else {
						grpcResp.Headers.OrdinaryHeaders[f.Name] = f.Value
					}
				})
				if _, err := decoder.Write(f.HeaderBlockFragment()); err != nil {
					return nil, fmt.Errorf("failed to decode headers: %w", err)
				}
			}
			if f.StreamEnded() {
				streamEnded = true
			}

		case *http2.DataFrame:
			frame, err := ReadGRPCFrame(bytes.NewReader(f.Data()))
			if err != nil {
				return nil, fmt.Errorf("failed to read gRPC frame: %w", err)
			}

			grpcResp.Body = CreateLengthPrefixedMessageFromPayload(frame)
			if f.StreamEnded() {
				streamEnded = true
			}

		case *http2.RSTStreamFrame:
			// Stream was reset by peer
			streamEnded = true

		case *http2.GoAwayFrame:
			// Connection is being closed
			streamEnded = true
		}
	}

	return grpcResp, nil
}

// CreateLengthPrefixedMessageFromPayload creates a GrpcLengthPrefixedMessage from raw payload data
func CreateLengthPrefixedMessageFromPayload(data []byte) models.GrpcLengthPrefixedMessage {
	msg := models.GrpcLengthPrefixedMessage{}

	// If the body is not length prefixed, we return the default value.
	if len(data) < 5 {
		return msg
	}

	// The first byte is the compression flag.
	msg.CompressionFlag = uint(data[0])

	// The next 4 bytes are message length.
	msg.MessageLength = binary.BigEndian.Uint32(data[1:5])

	// The payload could be empty. We only parse it if it is present.
	if len(data) > 5 {
		// Use protoscope to decode the message.
		msg.DecodedData = protoscope.Write(data[5:], protoscope.WriterOptions{})
	}

	return msg
}

// CreatePayloadFromLengthPrefixedMessage converts a GrpcLengthPrefixedMessage to raw payload bytes
func CreatePayloadFromLengthPrefixedMessage(msg models.GrpcLengthPrefixedMessage) ([]byte, error) {
	scanner := protoscope.NewScanner(msg.DecodedData)
	encodedData, err := scanner.Exec()
	if err != nil {
		return nil, fmt.Errorf("could not encode grpc msg using protoscope: %v", err)
	}

	// Note that the encoded length is present in the msg, but it is also equal to the len of encodedData.
	// We should give the preference to the length of encodedData, since the mocks might have been altered.

	// Reserve 1 byte for compression flag, 4 bytes for length capture.
	payload := make([]byte, 1+4)
	payload[0] = uint8(msg.CompressionFlag)
	binary.BigEndian.PutUint32(payload[1:5], uint32(len(encodedData)))
	payload = append(payload, encodedData...)

	return payload, nil
}

// ReadGRPCFrame reads a gRPC frame from the given reader and returns it as a byte array
func ReadGRPCFrame(r io.Reader) ([]byte, error) {
	// Read compression flag (1 byte)
	compFlag := make([]byte, 1)
	if _, err := io.ReadFull(r, compFlag); err != nil {
		if err == io.EOF {
			return nil, err
		}
		return nil, fmt.Errorf("failed to read compression flag: %w", err)
	}

	// Read message length (4 bytes)
	lenBuf := make([]byte, 4)
	if _, err := io.ReadFull(r, lenBuf); err != nil {
		return nil, fmt.Errorf("failed to read message length: %w", err)
	}
	msgLen := binary.BigEndian.Uint32(lenBuf)

	// Read message data
	msgBuf := make([]byte, msgLen)
	if _, err := io.ReadFull(r, msgBuf); err != nil {
		return nil, fmt.Errorf("failed to read message: %w", err)
	}

	// Combine all parts
	frame := make([]byte, 5+msgLen)
	frame[0] = compFlag[0]
	copy(frame[1:5], lenBuf)
	copy(frame[5:], msgBuf)

	return frame, nil
}
