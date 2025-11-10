// Package pkg provides common utilities for gRPC and HTTP/2 handling
package pkg

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/protocolbuffers/protoscope"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"
)

// HTTP/2 constants
const (
	// DefaultMaxFrameSize is the default maximum frame size as per HTTP/2 spec (16KB)
	DefaultMaxFrameSize = 16384
	// MaxFrameSize is the maximum allowed frame size (16MB)
	MaxFrameSize = 16777215 // 2^24 - 1
	// HTTP2Preface is the HTTP/2 connection preface
	HTTP2Preface        = "PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n"
	MaxDynamicTableSize = 8192
)

// rawMessage is a wrapper for byte slices to satisfy the proto.Message interface.
type rawMessage struct {
	data []byte
}

func (m *rawMessage) Reset()         { *m = rawMessage{} }
func (m *rawMessage) String() string { return string(m.data) }
func (*rawMessage) ProtoMessage()    {}

// passthroughCodec keeps 'proto' on the wire but avoids re-encoding.
type passthroughCodec struct{}

func (passthroughCodec) Name() string { return "proto" } // server already knows this one
func (passthroughCodec) Marshal(v interface{}) ([]byte, error) {
	if m, ok := v.(*rawMessage); ok {
		return m.data, nil // send bytes exactly as we received them
	}
	return proto.Marshal(v.(proto.Message))
}
func (passthroughCodec) Unmarshal(data []byte, v interface{}) error {
	if m, ok := v.(*rawMessage); ok {
		m.data = append([]byte(nil), data...)
		return nil
	}
	return proto.Unmarshal(data, v.(proto.Message))
}

// createLengthPrefixedMessage creates a GrpcLengthPrefixedMessage from a raw message payload.
func createLengthPrefixedMessage(data []byte) models.GrpcLengthPrefixedMessage {
	return models.GrpcLengthPrefixedMessage{
		CompressionFlag: 0,
		MessageLength:   uint32(len(data)),
		DecodedData:     protoscope.Write(data, protoscope.WriterOptions{}),
	}
}

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

	// Timing
	startTime time.Time
	endTime   time.Time

	// ===== Request (incoming frames) =====
	reqHeaderFrags       [][]byte
	reqHeadersReceived   bool
	reqDataFrames        [][]byte
	reqEndStreamReceived bool

	// ===== Response (outgoing frames) =====
	respHeaderFrags       [][]byte
	respHeadersReceived   bool
	respTrailersReceived  bool
	respDataFrames        [][]byte
	respEndStreamReceived bool

	// gRPC specific state
	grpcReq  *models.GrpcReq
	grpcResp *models.GrpcResp

	// Completion
	isComplete bool
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
	buffer  []byte
	logger  *zap.Logger

	// Two separate HPACK decoders: one per direction
	decoderIn  *hpack.Decoder // incoming (client -> server)
	decoderOut *hpack.Decoder // outgoing (server -> client)
}

// NewStreamManager creates a new stream manager
func NewStreamManager(logger *zap.Logger) *DefaultStreamManager {
	return &DefaultStreamManager{
		streams:    make(map[uint32]*HTTP2StreamState),
		buffer:     make([]byte, 0, DefaultMaxFrameSize),
		logger:     logger,
		decoderIn:  hpack.NewDecoder(MaxDynamicTableSize, nil),
		decoderOut: hpack.NewDecoder(MaxDynamicTableSize, nil),
	}
}

// HandleFrame processes an HTTP/2 frame
// isOutgoing=false => incoming (client->server) => REQUEST side
// isOutgoing=true  => outgoing (server->client) => RESPONSE side
func (sm *DefaultStreamManager) HandleFrame(frame http2.Frame, isOutgoing bool, frameTime time.Time) error {
	sm.mutex.Lock()
	defer sm.mutex.Unlock()

	streamID := frame.Header().StreamID
	if streamID == 0 {
		// connection control; ignore here
		return nil
	}

	// Init stream
	if _, exists := sm.streams[streamID]; !exists {
		prefix := "Incoming_"
		if isOutgoing {
			prefix = "Outgoing_"
		}
		requestID := fmt.Sprintf(prefix+"%d", streamID)
		sm.streams[streamID] = &HTTP2StreamState{
			ID:        streamID,
			RequestID: requestID,
			startTime: frameTime,
		}
	}
	s := sm.streams[streamID]

	switch f := frame.(type) {

	case *http2.HeadersFrame:
		if isOutgoing {
			s.respHeaderFrags = append(s.respHeaderFrags, f.HeaderBlockFragment())
			if f.HeadersEnded() {
				if err := sm.processHeaderBlock(s /*isOutgoing=*/, true); err != nil {
					return err
				}
			}
			if f.StreamEnded() {
				s.respEndStreamReceived = true
				if err := sm.processCompleteMessage(s /*isOutgoing=*/, true); err != nil {
					return err
				}
				s.endTime = frameTime
				sm.checkStreamCompletion(streamID)
			}
		} else {
			s.reqHeaderFrags = append(s.reqHeaderFrags, f.HeaderBlockFragment())
			if f.HeadersEnded() {
				if err := sm.processHeaderBlock(s /*isOutgoing=*/, false); err != nil {
					return err
				}
			}
			if f.StreamEnded() {
				s.reqEndStreamReceived = true
				if err := sm.processCompleteMessage(s /*isOutgoing=*/, false); err != nil {
					return err
				}
				sm.checkStreamCompletion(streamID)
			}
		}

	case *http2.ContinuationFrame:
		if isOutgoing {
			s.respHeaderFrags = append(s.respHeaderFrags, f.HeaderBlockFragment())
			if f.HeadersEnded() {
				if err := sm.processHeaderBlock(s /*isOutgoing=*/, true); err != nil {
					return err
				}
			}
		} else {
			s.reqHeaderFrags = append(s.reqHeaderFrags, f.HeaderBlockFragment())
			if f.HeadersEnded() {
				if err := sm.processHeaderBlock(s /*isOutgoing=*/, false); err != nil {
					return err
				}
			}
		}

	case *http2.DataFrame:
		if isOutgoing {
			s.respDataFrames = append(s.respDataFrames, f.Data())
			if f.StreamEnded() {
				s.respEndStreamReceived = true
				if err := sm.processCompleteMessage(s /*isOutgoing=*/, true); err != nil {
					return err
				}
				s.endTime = frameTime
				sm.checkStreamCompletion(streamID)
			}
		} else {
			s.reqDataFrames = append(s.reqDataFrames, f.Data())
			if f.StreamEnded() {
				s.reqEndStreamReceived = true
				if err := sm.processCompleteMessage(s /*isOutgoing=*/, false); err != nil {
					return err
				}
				sm.checkStreamCompletion(streamID)
			}
		}
	}

	return nil
}

// processHeaderBlock decodes accumulated header fragments for the given side
func (sm *DefaultStreamManager) processHeaderBlock(s *HTTP2StreamState, isOutgoing bool) error {
	var headerBlock []byte
	var fields []hpack.HeaderField
	var err error

	if isOutgoing {
		if len(s.respHeaderFrags) == 0 {
			return nil
		}
		headerBlock = bytes.Join(s.respHeaderFrags, nil)
		s.respHeaderFrags = nil

		fields, err = sm.decoderOut.DecodeFull(headerBlock)
		if err != nil {
			return fmt.Errorf("failed to decode response headers: %v", err)
		}

		h := ProcessHeaders(fields)

		if !s.respHeadersReceived {
			// Initial response headers
			if s.grpcResp == nil {
				s.grpcResp = &models.GrpcResp{}
			}
			s.grpcResp.Headers = *h
			s.respHeadersReceived = true
			return nil
		}

		// Subsequent headers on response side are trailers
		if s.grpcResp == nil {
			s.grpcResp = &models.GrpcResp{}
		}
		s.grpcResp.Trailers = *h
		s.respTrailersReceived = true
		return nil

	} else {
		if len(s.reqHeaderFrags) == 0 {
			return nil
		}
		headerBlock = bytes.Join(s.reqHeaderFrags, nil)
		s.reqHeaderFrags = nil

		fields, err = sm.decoderIn.DecodeFull(headerBlock)
		if err != nil {
			return fmt.Errorf("failed to decode request headers: %v", err)
		}

		h := ProcessHeaders(fields)

		// gRPC client rarely sends request trailers; treat first block as headers
		if !s.reqHeadersReceived {
			if s.grpcReq == nil {
				s.grpcReq = &models.GrpcReq{}
			}
			s.grpcReq.Headers = *h
			s.reqHeadersReceived = true
			return nil
		}

		// If you want to store client trailers, add a Trailers field to GrpcReq in your model and set it here.
		return nil
	}
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

// processCompleteMessage assembles DATA frames for the given side and parses gRPC payload
func (sm *DefaultStreamManager) processCompleteMessage(s *HTTP2StreamState, isOutgoing bool) error {
	if isOutgoing {
		if len(s.respDataFrames) == 0 {
			return nil
		}
		data := bytes.Join(s.respDataFrames, nil)
		s.respDataFrames = nil

		if s.grpcResp == nil {
			s.grpcResp = &models.GrpcResp{}
		}
		s.grpcResp.Body = CreateLengthPrefixedMessageFromPayload(data)
		return nil

	} else {
		if len(s.reqDataFrames) == 0 {
			return nil
		}
		data := bytes.Join(s.reqDataFrames, nil)
		s.reqDataFrames = nil

		if s.grpcReq == nil {
			s.grpcReq = &models.GrpcReq{}
		}
		s.grpcReq.Body = CreateLengthPrefixedMessageFromPayload(data)
		return nil
	}
}

// checkStreamCompletion decides when a stream is complete (request done + response done with trailers)
func (sm *DefaultStreamManager) checkStreamCompletion(streamID uint32) {
	s := sm.streams[streamID]
	if s == nil || s.isComplete == false {
		// Request considered "done" once END_STREAM seen on request side and headers received.
		reqDone := s.reqHeadersReceived && s.reqEndStreamReceived

		// Response considered "done" once headers + trailers received and END_STREAM on response.
		respDone := s.respHeadersReceived && s.respTrailersReceived && s.respEndStreamReceived

		if reqDone && respDone {
			sm.logger.Debug("Stream completed", zap.Any("stream", s))
			s.isComplete = true
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
// This is a simplified version using gRPC client instead of manual HTTP/2 frame handling
func SimulateGRPC(ctx context.Context, tc *models.TestCase, testSetID string, logger *zap.Logger) (*models.GrpcResp, error) {
	// Render any template values in the test case before simulation
	if len(utils.TemplatizedValues) > 0 || len(utils.SecretValues) > 0 {
		testCaseBytes, err := json.Marshal(tc)
		if err != nil {
			utils.LogError(logger, err, "failed to marshal the testcase for templating")
			return nil, err
		}

		// Build the template data
		templateData := make(map[string]interface{}, len(utils.TemplatizedValues)+len(utils.SecretValues))
		for k, v := range utils.TemplatizedValues {
			templateData[k] = v
		}
		if len(utils.SecretValues) > 0 {
			templateData["secret"] = utils.SecretValues
		}

		// Render only real Keploy placeholders ({{ .x }}, {{ string .y }}, etc.),
		// ignoring LaTeX/HTML like {{\pi}}.
		renderedStr, rerr := utils.RenderTemplatesInString(logger, string(testCaseBytes), templateData)
		if rerr != nil {
			logger.Debug("template rendering had recoverable errors", zap.Error(rerr))
		}

		err = json.Unmarshal([]byte(renderedStr), &tc)
		if err != nil {
			utils.LogError(logger, err, "failed to unmarshal the rendered testcase")
			return nil, err
		}
	}

	grpcReq := tc.GrpcReq

	logger.Info("starting test for", zap.String("test case", models.HighlightString(tc.Name)), zap.String("test set", models.HighlightString(testSetID)))

	// Extract target address from headers
	authority, ok := grpcReq.Headers.PseudoHeaders[":authority"]
	if !ok {
		return nil, fmt.Errorf("missing :authority header")
	}

	// Extract method path
	path, ok := grpcReq.Headers.PseudoHeaders[":path"]
	if !ok {
		return nil, fmt.Errorf("missing :path header")
	}

	// Create a TCP connection
	conn, err := net.Dial("tcp", authority)
	if err != nil {
		return nil, fmt.Errorf("failed to dial: %w", err)
	}
	defer func() {
		if cerr := conn.Close(); cerr != nil && !strings.Contains(cerr.Error(), "use of closed network connection") {
			logger.Error("failed to close connection", zap.Error(cerr))
		}
	}()

	// Create gRPC client connection using the TCP connection
	dialer := func(context.Context, string) (net.Conn, error) { return conn, nil }

	clientConn, err := grpc.DialContext(
		ctx,
		authority,
		grpc.WithContextDialer(dialer),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.ForceCodec(passthroughCodec{})),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create gRPC client connection: %w", err)
	}
	defer func() {
		if cerr := clientConn.Close(); cerr != nil && !strings.Contains(cerr.Error(), "use of closed network connection") {
			logger.Error("failed to close gRPC client connection", zap.Error(cerr))
		}
	}()

	// Prepare metadata from ordinary headers (excluding pseudo headers)
	md := metadata.New(nil)
	for k, v := range grpcReq.Headers.OrdinaryHeaders {
		// Skip pseudo-headers and certain internal headers
		if strings.HasPrefix(k, ":") {
			continue
		}
		md[k] = []string{v}
	}

	// Create context with metadata
	callCtx := metadata.NewOutgoingContext(ctx, md)

	// Create a new stream
	stream, err := clientConn.NewStream(callCtx, &grpc.StreamDesc{
		StreamName:    path,
		ServerStreams: true,
		ClientStreams: true,
	}, path)
	if err != nil {
		return nil, fmt.Errorf("failed to create stream: %w", err)
	}

	// Convert the request body to raw message format
	// For gRPC client with passthrough codec, we only need the protobuf bytes, not the full gRPC frame
	var requestPayload []byte
	if grpcReq.Body.DecodedData != "" {
		// Parse the protoscope format back to bytes
		scanner := protoscope.NewScanner(grpcReq.Body.DecodedData)
		var err error
		requestPayload, err = scanner.Exec()
		if err != nil {
			return nil, fmt.Errorf("failed to parse request payload: %w", err)
		}
	}

	requestMsg := &rawMessage{data: requestPayload}

	// Send the request
	if err := stream.SendMsg(requestMsg); err != nil {
		return nil, fmt.Errorf("failed to send request: %w", err)
	}

	// Close the send side of the stream
	if err := stream.CloseSend(); err != nil {
		return nil, fmt.Errorf("failed to close send: %w", err)
	}

	// Read the response headers
	respHeaders, err := stream.Header()
	if err != nil {
		return nil, fmt.Errorf("failed to get response headers: %w", err)
	}

	// Read the response message
	responseMsg := &rawMessage{}
	if err := stream.RecvMsg(responseMsg); err != nil && err != io.EOF {
		return nil, fmt.Errorf("failed to receive response: %w", err)
	}

	// Get trailers
	trailers := stream.Trailer()

	// Construct the response
	grpcResp := &models.GrpcResp{
		Headers: models.GrpcHeaders{
			PseudoHeaders:   make(map[string]string),
			OrdinaryHeaders: make(map[string]string),
		},
		Body: createLengthPrefixedMessage(responseMsg.data),
		Trailers: models.GrpcHeaders{
			PseudoHeaders:   make(map[string]string),
			OrdinaryHeaders: make(map[string]string),
		},
	}

	// Convert response headers
	for k, v := range respHeaders {
		val := strings.Join(v, ", ")
		if strings.HasPrefix(k, ":") {
			grpcResp.Headers.PseudoHeaders[k] = val
		} else {
			grpcResp.Headers.OrdinaryHeaders[k] = val
		}
	}

	// Add the :status pseudo-header since gRPC client abstracts it away
	// For successful gRPC calls, HTTP status is always 200
	if _, ok := grpcResp.Headers.PseudoHeaders[":status"]; !ok {
		grpcResp.Headers.PseudoHeaders[":status"] = "200"
	}

	// Add standard gRPC headers if not present
	if _, ok := grpcResp.Headers.OrdinaryHeaders["content-type"]; !ok {
		grpcResp.Headers.OrdinaryHeaders["content-type"] = "application/grpc"
	}

	// Convert trailers
	for k, v := range trailers {
		val := strings.Join(v, ", ")
		if strings.HasPrefix(k, ":") {
			grpcResp.Trailers.PseudoHeaders[k] = val
		} else {
			grpcResp.Trailers.OrdinaryHeaders[k] = val
		}
	}

	// Ensure mandatory gRPC status is present
	if _, ok := grpcResp.Trailers.OrdinaryHeaders["grpc-status"]; !ok {
		grpcResp.Trailers.OrdinaryHeaders["grpc-status"] = "0"
	}
	if _, ok := grpcResp.Trailers.OrdinaryHeaders["grpc-message"]; !ok {
		grpcResp.Trailers.OrdinaryHeaders["grpc-message"] = ""
	}

	logger.Info("successfully completed gRPC simulation", zap.String("method", path))
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
