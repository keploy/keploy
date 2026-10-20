// Package pkg provides common utilities for gRPC and HTTP/2 handling
package pkg

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/protocolbuffers/protoscope"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/utils/logcontent"
	"go.uber.org/zap"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
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

// FrameReader reads the frames of one direction of an HTTP/2 connection, one
// complete frame at a time, in order, with one Framer for all of them. A
// header block a peer splits across a HEADERS frame and CONTINUATION frames
// (one larger than a frame) is one block (RFC 9113 §6.10): the Framer accepts
// the CONTINUATION frames that follow a HEADERS frame without END_HEADERS,
// and refuses any other frame between them, as the RFC does. Each frame read
// alone, by a Framer that had seen no frame before it, a CONTINUATION was
// refused as following no HEADERS frame, and the parser of that direction gave
// up on the connection.
//
// A frame it returns is valid until the next ReadFrame (the Framer reuses its
// buffer).
type FrameReader struct {
	src frameSource
	fr  *http2.Framer
}

// frameSource is what a FrameReader's Framer reads: the frame ReadFrame was
// given.
type frameSource struct{ b []byte }

func (s *frameSource) Read(p []byte) (int, error) {
	if len(s.b) == 0 {
		return 0, io.EOF
	}
	n := copy(p, s.b)
	s.b = s.b[n:]
	return n, nil
}

// NewFrameReader is a FrameReader for one direction of a connection, from its
// first frame (after the client's preface).
func NewFrameReader() *FrameReader {
	r := &FrameReader{}
	r.fr = http2.NewFramer(nil, &r.src) // reads frames up to MaxFrameSize
	return r
}

// ReadFrame reads data, exactly one complete frame (its header and payload),
// as the next frame of the direction.
func (r *FrameReader) ReadFrame(data []byte) (http2.Frame, error) {
	r.src.b = data
	frame, err := r.fr.ReadFrame()
	if err != nil {
		if detail := r.fr.ErrorDetail(); detail != nil {
			return nil, fmt.Errorf("%w: %v", err, detail)
		}
		return nil, err
	}
	if left := len(r.src.b); left > 0 {
		r.src.b = nil
		return nil, fmt.Errorf("%d bytes after the frame", left)
	}
	return frame, nil
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
	// reqEndsWithBlock: a HEADERS frame with END_STREAM began the header
	// block still open; the stream ends at that block's end.
	reqEndsWithBlock bool

	// ===== Response (outgoing frames) =====
	respHeaderFrags       [][]byte
	respHeadersReceived   bool
	respTrailersReceived  bool
	respDataFrames        [][]byte
	respEndStreamReceived bool
	// respEndsWithBlock: as reqEndsWithBlock, for the response.
	respEndsWithBlock bool

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
		// Handle SETTINGS frames to keep HPACK decoders in sync with
		// the negotiated dynamic table size. Without this, the decoders
		// stay at the initial MaxDynamicTableSize (8192) and fail to
		// decode headers when the peer negotiates a larger table via
		// SETTINGS_HEADER_TABLE_SIZE.
		if sf, ok := frame.(*http2.SettingsFrame); ok && !sf.IsAck() {
			sm.updateDecoderTableSize(sf, isOutgoing)
		}
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
		}
	}
	s := sm.streams[streamID]
	if !isOutgoing && s.startTime.IsZero() {
		s.startTime = frameTime
	}

	switch f := frame.(type) {

	case *http2.HeadersFrame:
		// Copy the header block fragment — http2.Framer reuses its internal
		// buffer, so f.HeaderBlockFragment() becomes invalid after the next
		// ReadFrame() call.
		fragCopy := make([]byte, len(f.HeaderBlockFragment()))
		copy(fragCopy, f.HeaderBlockFragment())

		// A HEADERS frame with END_STREAM ends its stream with its header
		// block, the CONTINUATION frames that follow it included: they are
		// part of it (RFC 9113 §8.1). The stream ends at the block's end
		// (endHeaderBlock), and a response's end is the time of the frame
		// that ends the block, as a frame's time is that of the chunk that
		// completes it.
		if isOutgoing {
			s.respHeaderFrags = append(s.respHeaderFrags, fragCopy)
			if f.StreamEnded() {
				s.respEndsWithBlock = true
			}
		} else {
			s.reqHeaderFrags = append(s.reqHeaderFrags, fragCopy)
			if f.StreamEnded() {
				s.reqEndsWithBlock = true
			}
		}
		if f.HeadersEnded() {
			if err := sm.endHeaderBlock(s, isOutgoing, frameTime); err != nil {
				return err
			}
		}

	case *http2.ContinuationFrame:
		contCopy := make([]byte, len(f.HeaderBlockFragment()))
		copy(contCopy, f.HeaderBlockFragment())

		if isOutgoing {
			s.respHeaderFrags = append(s.respHeaderFrags, contCopy)
		} else {
			s.reqHeaderFrags = append(s.reqHeaderFrags, contCopy)
		}
		if f.HeadersEnded() {
			if err := sm.endHeaderBlock(s, isOutgoing, frameTime); err != nil {
				return err
			}
		}

	case *http2.DataFrame:
		// Copy the data — http2.Framer reuses its internal buffer, so
		// f.Data() becomes invalid after the next ReadFrame() call.
		dataCopy := make([]byte, len(f.Data()))
		copy(dataCopy, f.Data())

		if isOutgoing {
			s.respDataFrames = append(s.respDataFrames, dataCopy)
			if f.StreamEnded() {
				s.respEndStreamReceived = true
				if err := sm.processCompleteMessage(s /*isOutgoing=*/, true); err != nil {
					return err
				}
				s.endTime = frameTime
				sm.checkStreamCompletion(streamID)
			}
		} else {
			s.reqDataFrames = append(s.reqDataFrames, dataCopy)
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

// endHeaderBlock decodes the header block that ended on stream s, its
// fragments joined (processHeaderBlock), and, when the HEADERS frame that
// began it had END_STREAM, ends that side of the stream; a response's at
// frameTime, the time of the frame that ended the block. Stamped with the
// HEADERS frame's time instead, a block split across chunks ended its call
// before the chunk that completed it: the HEADERS frame's chunk was let go
// as completing no call, and a stop that asked before the next chunk was
// noted heard "clear", then a call from before it came.
func (sm *DefaultStreamManager) endHeaderBlock(s *HTTP2StreamState, isOutgoing bool, frameTime time.Time) error {
	if err := sm.processHeaderBlock(s, isOutgoing); err != nil {
		return err
	}
	if !isOutgoing {
		if !s.reqEndsWithBlock {
			return nil
		}
		s.reqEndsWithBlock = false
		s.reqEndStreamReceived = true
		if err := sm.processCompleteMessage(s /*isOutgoing=*/, false); err != nil {
			return err
		}
		sm.checkStreamCompletion(s.ID)
		return nil
	}
	if !s.respEndsWithBlock {
		return nil
	}
	s.respEndsWithBlock = false
	s.respEndStreamReceived = true
	s.endTime = frameTime
	// gRPC error responses send a single header block with END_STREAM that
	// merges initial headers and trailers (grpc-status, grpc-message).
	// processHeaderBlock treated it as initial headers, so respTrailersReceived
	// is still false. Promote the initial headers to trailers so that
	// checkStreamCompletion can mark the stream complete.
	if s.respHeadersReceived && !s.respTrailersReceived {
		if s.grpcResp == nil {
			s.grpcResp = &models.GrpcResp{}
		}
		s.grpcResp.Trailers = models.GrpcHeaders{
			PseudoHeaders:   maps.Clone(s.grpcResp.Headers.PseudoHeaders),
			OrdinaryHeaders: maps.Clone(s.grpcResp.Headers.OrdinaryHeaders),
		}
		s.respTrailersReceived = true
	}
	if err := sm.processCompleteMessage(s /*isOutgoing=*/, true); err != nil {
		return err
	}
	sm.checkStreamCompletion(s.ID)
	return nil
}

// updateDecoderTableSize reads SETTINGS_HEADER_TABLE_SIZE from a SETTINGS
// frame and updates the appropriate HPACK decoder.
//
// SETTINGS from the client (isOutgoing=false) tells the server what table size
// the client's decoder supports → the server's encoder will use that size →
// we update decoderOut (which decodes server responses).
//
// SETTINGS from the server (isOutgoing=true) tells the client what table size
// the server's decoder supports → the client's encoder will use that size →
// we update decoderIn (which decodes client requests).
// maxHPACKDynamicTableSize caps the HPACK dynamic table to prevent unbounded
// memory growth from a peer advertising a very large table via SETTINGS.
const maxHPACKDynamicTableSize uint32 = 64 * 1024

func (sm *DefaultStreamManager) updateDecoderTableSize(sf *http2.SettingsFrame, isOutgoing bool) {
	sf.ForeachSetting(func(s http2.Setting) error {
		if s.ID == http2.SettingHeaderTableSize {
			size := s.Val
			if size > maxHPACKDynamicTableSize {
				size = maxHPACKDynamicTableSize
			}
			if isOutgoing {
				sm.decoderIn.SetMaxDynamicTableSize(size)
				sm.logger.Debug("updated request HPACK decoder table size from server SETTINGS",
					zap.Uint32("requested_size", s.Val), zap.Uint32("applied_size", size))
			} else {
				sm.decoderOut.SetMaxDynamicTableSize(size)
				sm.logger.Debug("updated response HPACK decoder table size from client SETTINGS",
					zap.Uint32("requested_size", s.Val), zap.Uint32("applied_size", size))
			}
		}
		return nil
	})
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
		if s.grpcResp == nil {
			s.grpcResp = &models.GrpcResp{}
		}
		if len(s.respDataFrames) == 0 {
			// gRPC error responses have no DATA frame — body is empty.
			// Leave grpcResp.Body as zero value so the stream is still marked complete.
			return nil
		}
		data := bytes.Join(s.respDataFrames, nil)
		s.respDataFrames = nil
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
			sm.logger.Debug("Stream completed", logcontent.Any("stream", s))
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

// isGRPCConnectionLost reports whether err is a gRPC call whose connection to
// the app failed before the server answered: the HTTP/2 connection
// SimulateGRPC dialed reset, broke or closed before the server's preface came,
// or what came was not one (an HTTP/1 server on the port). grpc-go reports
// that as codes.Unavailable and keeps only the cause's text (toRPCErr), so the
// code is all there is to know it by.
func isGRPCConnectionLost(err error) bool {
	return status.Code(err) == codes.Unavailable
}

// isGRPCNoAnswer reports whether err is a gRPC call that got no answer in time
// or was cancelled. grpc-go turns the end of the call's context into
// codes.DeadlineExceeded or codes.Canceled and keeps no cause, so the code is
// all there is to know it by.
func isGRPCNoAnswer(err error) bool {
	switch status.Code(err) {
	case codes.DeadlineExceeded, codes.Canceled:
		return true
	}
	return false
}

// unreachableGRPCAuthority is err, or, when a gRPC call lost its connection
// after the dial went through (isGRPCConnectionLost) at an address reach says
// the app's server on appPort (the port the test was recorded on, 0 when not
// known) can never be reached at, the *UnreachableAppPortError saying why:
// docker accepts a connection to a published port and drops it when the app
// listens only on 127.0.0.1 in its container. Such a connection never brings
// the server's preface, so the call fails creating its stream.
func unreachableGRPCAuthority(ctx context.Context, reach AppPortReachability, authority string, appPort uint16, err error) error {
	if reach == nil || !isGRPCConnectionLost(err) {
		return err
	}
	host, port, splitErr := net.SplitHostPort(authority)
	if splitErr != nil {
		return err
	}
	if unreachable := unreachableAppPort(ctx, reach, host, port, appPort, err); unreachable != nil {
		return unreachable
	}
	return err
}

// SimulateGRPC simulates a gRPC call and returns the response
// This is a simplified version using gRPC client instead of manual HTTP/2 frame handling
// dialTCPWithConnRefusedRetry dials authority over TCP, retrying a pure
// connection-refused (the app is still coming up) up to maxConnRefusedRetries
// with the shared growing backoff. A refused dial sent zero bytes and consumed
// zero mocks, so the re-dial is idempotent; any other dial error, exhaustion of
// the attempts, or a cancelled context returns immediately, and so does a
// refusal at an address reach says the app's server on appPort (the port the
// test was recorded on, 0 when not known) can never be reached at.
func dialTCPWithConnRefusedRetry(ctx context.Context, logger *zap.Logger, authority string, reach AppPortReachability, appPort uint16) (net.Conn, error) {
	for attempt := 0; ; attempt++ {
		conn, err := net.Dial("tcp", authority)
		if err == nil {
			return conn, nil
		}
		if !isPreResponseConnRefused(err) {
			return nil, err
		}
		if attempt == 0 {
			if host, port, splitErr := net.SplitHostPort(authority); splitErr == nil {
				if unreachable := unreachableAppPort(ctx, reach, host, port, appPort, err); unreachable != nil {
					return nil, unreachable
				}
			}
		}
		if attempt >= maxConnRefusedRetries {
			return nil, err
		}
		logger.Debug("gRPC dial refused; the app may still be coming up — retrying",
			zap.Int("attempt", attempt+1), zap.Int("maxRetries", maxConnRefusedRetries), zap.Error(err))
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(connRefusedRetryBackoff * time.Duration(attempt+1)):
		}
	}
}

// SimulateGRPC sends tc's gRPC request to the app and returns its answer.
func SimulateGRPC(ctx context.Context, tc *models.TestCase, testSetID string, logger *zap.Logger, cfg SimulationConfig) (*models.GrpcResp, error) {
	// tc arrives as it is to be sent: a replay renders its placeholders
	// (pkg/templates) on a copy before handing it here.
	grpcReq := tc.GrpcReq

	logger.Info("starting test for", zap.String("test case", models.HighlightString(tc.Name)), zap.String("test set", models.HighlightString(testSetID)))

	// Extract target address from headers
	authority, ok := grpcReq.Headers.PseudoHeaders[":authority"]
	if !ok {
		return nil, fmt.Errorf("missing :authority header")
	}

	// Determine which port to use for test execution.
	var err error
	recorded := authority
	authority, err = ResolveTestTarget(authority, cfg.URLReplacements, cfg.PortMappings, cfg.ConfigHost, tc.AppPort, cfg.ConfigPort, false, logger)
	if err != nil {
		return nil, err
	}
	authority = PublishedTarget(ctx, cfg, recorded, authority, tc.AppPort, false)

	// Extract method path
	path, ok := grpcReq.Headers.PseudoHeaders[":path"]
	if !ok {
		return nil, fmt.Errorf("missing :path header")
	}

	// Create a TCP connection, retrying a pure connection-refused. A slow-starting
	// gRPC app (especially a docker app under CI load) can briefly refuse
	// connections while still coming up; a refused dial sent zero bytes and
	// consumed zero mocks, so the re-dial is safe. Mirrors the HTTP replay path's
	// doRequestWithConnRefusedRetry — a genuinely-down app still fails fast after
	// the bounded attempts.
	conn, err := dialTCPWithConnRefusedRetry(ctx, logger, authority, cfg.AppPortReachability, tc.AppPort)
	if err != nil {
		return nil, fmt.Errorf("failed to dial: %w", err)
	}
	defer func() {
		if cerr := conn.Close(); cerr != nil && !strings.Contains(cerr.Error(), "use of closed network connection") {
			logger.Error("failed to close connection", zap.Error(cerr))
		}
	}()

	// Create gRPC client connection using the TCP connection. Under the
	// proven switch the app's frames are watched (pingWatch): a stream it
	// keeps open past its recording ends on its keepalives (row W1).
	//
	// With no clock on the app's answer, the stall rule (answerWatch) ends an
	// exchange the app makes no progress on: the whole call waits on the
	// app, and AnswerStallGrace of keploy's runnable time with no frame of
	// the exchange (pingWatch tells it which: never a keepalive) cuts it.
	var dialed net.Conn = conn
	var pings *pingWatch
	answer := newAnswerWatch(cfg)
	defer answer.stop()
	defer answer.waiting()()
	answer.closeOnCut(conn)
	if provenswitch.On() {
		pings = newPingWatch(conn)
		pings.answer = answer
		dialed = pings
	}
	dialer := func(context.Context, string) (net.Conn, error) { return dialed, nil }

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

	// Bound the whole call, because the drain below is now a loop.
	//
	// A single RecvMsg was self-limiting. Draining to io.EOF is not: a bidi
	// stream the server holds open — server reflection is exactly that shape
	// — never reaches EOF, so without a deadline one bad test case blocks
	// until the entire test-run context dies, taking the rest of the test set
	// with it and attributing the hang to nothing in particular.
	//
	// APITimeout is the same knob the HTTP replay path uses. Zero means the
	// caller did not set one, so fall back rather than leaving it unbounded.
	//
	// Unbounded (the proven switch, and no apiTimeout of the user's) sets no
	// clock: the drain ends on the app's end of the stream (rule 15, row
	// W1), or, on a stream the app holds open past its recording, on its
	// keepalives (pingWatch): the test then has no verdict.
	if !cfg.Unbounded {
		timeout := time.Duration(cfg.APITimeout) * time.Second
		if timeout <= 0 {
			timeout = defaultGrpcCallTimeout
		}
		var cancelCall context.CancelFunc
		ctx, cancelCall = context.WithTimeout(ctx, timeout)
		defer cancelCall()
	}

	// Create context with metadata
	callCtx := metadata.NewOutgoingContext(ctx, md)
	// A stream the app keeps open past its recording is cut through its
	// own context (pingWatch).
	if pings != nil {
		streamCtx, cancelStream := context.WithCancel(ctx)
		defer cancelStream()
		callCtx = metadata.NewOutgoingContext(streamCtx, md)
		pings.arm(recordedMessagesComplete(tc.GrpcResp), cancelStream)
		answer.onCut(cancelStream)
	}

	reqMsgs := grpcReq.AllMessages()
	if answer != nil {
		answer.describe(path, grpcRequestSize(reqMsgs))
	}

	// Create a new stream
	stream, err := clientConn.NewStream(callCtx, &grpc.StreamDesc{
		StreamName:    path,
		ServerStreams: true,
		ClientStreams: true,
	}, path)
	if err != nil {
		if answer.didStall() {
			return nil, answer.err()
		}
		return nil, fmt.Errorf("failed to create stream: %w", unreachableGRPCAuthority(ctx, cfg.AppPortReachability, authority, tc.AppPort, err))
	}

	// Send every recorded request message, in order.
	//
	// A client-streaming or bidi call records N messages; sending only the
	// first replays a different request from the one that was captured, and
	// the server's response to it is not the response that was recorded.
	//
	// AllMessages() always yields at least one element, including for a
	// zero-value body — the health check records message_length 0 with empty
	// decoded_data and expects exactly one empty message. An empty payload
	// here is a real message, not the absence of one.
	for i, m := range reqMsgs {
		var requestPayload []byte
		if m.DecodedData != "" {
			// Parse the protoscope format back to bytes
			scanner := protoscope.NewScanner(m.DecodedData)
			var err error
			requestPayload, err = scanner.Exec()
			if err != nil {
				return nil, fmt.Errorf("failed to parse request payload (message %d of %d): %w", i, len(reqMsgs), err)
			}
		}
		if err := stream.SendMsg(&rawMessage{data: requestPayload}); err != nil {
			if answer.didStall() {
				return stalledStart(stream, answer), answer.err()
			}
			return nil, fmt.Errorf("failed to send request (message %d of %d): %w", i, len(reqMsgs), err)
		}
	}

	// Close the send side of the stream
	if err := stream.CloseSend(); err != nil {
		if answer.didStall() {
			return stalledStart(stream, answer), answer.err()
		}
		return nil, fmt.Errorf("failed to close send: %w", err)
	}

	// Read the response headers. grpc-go hands a call that fails here, a
	// connection lost after the request went among them, to RecvMsg below and
	// answers nil; the error check stays for a client that does not.
	respHeaders, err := stream.Header()
	if err != nil {
		if answer.didStall() {
			return stalledStart(stream, answer), answer.err()
		}
		return nil, fmt.Errorf("failed to get response headers: %w", err)
	}

	// Drain the response stream to io.EOF.
	//
	// A single RecvMsg captures the first message of a server-streaming or
	// bidi response and abandons the rest, which also tears the connection
	// down early — so the app handler aborts and any dependency calls behind
	// messages 2..N never run.
	//
	// TRAILERS MUST BE READ AFTER THE DRAIN, NOT AFTER THE FIRST MESSAGE.
	// grpc-go only populates Trailer() once RecvMsg has returned a non-nil
	// error. Reading it earlier returns an empty map, the grpc-status fixup
	// below then fabricates "0", and a stream that died partway replays as a
	// clean success — the worst possible outcome, because it looks correct.
	var respMsgs []models.GrpcLengthPrefixedMessage
	var drainErr error
	for {
		responseMsg := &rawMessage{}
		err := stream.RecvMsg(responseMsg)
		if err == io.EOF {
			break
		}
		if err != nil {
			// Not fatal: this error IS the RPC's outcome and belongs in the
			// mock. Stop draining and record it below, rather than discarding
			// a partial stream and the reason it ended.
			drainErr = err
			logger.Debug("gRPC response stream ended with an error; recording what arrived",
				zap.String("method", path), zap.Int("messages", len(respMsgs)), logcontent.Error(err))
			break
		}
		respMsgs = append(respMsgs, createLengthPrefixedMessage(responseMsg.data))
		if pings != nil && pings.gotMessage(len(respMsgs)) {
			break
		}
	}
	// Every recorded message came, then a keepalive beyond them before any
	// end: the app kept the stream open past its recording (pingWatch, on
	// the wire's order). The messages that came before the keepalive are
	// returned, with ErrStreamHeldOpen: the test has no verdict, and what the
	// app sent after it is not its answer. A message beyond the recording
	// among them still fails the comparison, as before.
	//
	// A stream that stalled short of its recording (two keepalives, and no
	// message between them) is returned with ErrStreamStalled: the messages
	// that came, then the recording's for the rest, which the app never
	// sent, so that only what came is compared.
	heldOpen, heldAt, stalled := pings.heldOpen()
	heldOpen = heldOpen && len(respMsgs) >= heldAt
	if heldOpen && stalled && heldAt == 0 {
		// Keepalives, and no message: nothing of the answer to compare.
		// It fails closed, not judged (ErrAnswerStalled), never a pass on
		// the recording's fill.
		headers, data := pings.heldAnswer()
		if headers {
			return headersOnly(respHeaders), keptOpenWithNothing(path, headers, data)
		}
		return nil, keptOpenWithNothing(path, headers, data)
	}
	if heldOpen {
		respMsgs, drainErr = respMsgs[:heldAt], nil
		if recorded := tc.GrpcResp.AllMessages(); stalled && heldAt < len(recorded) {
			respMsgs = append(respMsgs, recorded[heldAt:]...)
		}
	}
	// The stall rule cut the exchange, and no keepalive held it open: an
	// answer that never began, or none of whose messages came, is nothing to
	// compare (ErrAnswerStalled), and a stream the app went silent on is
	// judged on what came, the recording's rest in place of what did not
	// (the stall's own fill), not judged as a whole.
	var silent error
	if !heldOpen && drainErr != nil && answer.didStall() {
		if !answer.answered.Load() {
			return nil, answer.err()
		}
		if len(respMsgs) == 0 {
			return headersOnly(respHeaders), answer.err()
		}
		recorded := tc.GrpcResp.AllMessages()
		came := len(respMsgs)
		silent = streamCut(answer.err(), came, len(recorded), "message")
		if came < len(recorded) {
			respMsgs = append(respMsgs, recorded[came:]...)
		}
		drainErr = nil
		heldOpen = true
	}
	if len(respMsgs) == 0 {
		// A response carrying only trailers is legitimate (an error status, or
		// a client-streaming call the server rejects). Keep one empty message
		// so the recorded shape matches what a single RecvMsg used to produce.
		//
		// GrpcResp can now express "zero messages" honestly via NoMessages, and
		// this site deliberately does NOT use it. This is the ingress SIMULATE
		// path: the value built here is the ACTUAL response, compared field by
		// field against a mock recorded earlier. Every such mock predates the
		// flag and carries one empty message for this shape, so emitting zero
		// here would report a body.message_count mismatch (0 vs 1) on
		// recordings that are perfectly fine. The honest zero belongs on the
		// EGRESS record path, where both sides move together.
		respMsgs = append(respMsgs, createLengthPrefixedMessage(nil))
	}

	// Now that the stream is finished, the trailers are real. One the app
	// held open has none: the drain may have left it before its cut was
	// done.
	var trailers metadata.MD
	if !heldOpen {
		trailers = stream.Trailer()
	}

	// Construct the response
	grpcResp := &models.GrpcResp{
		Headers: grpcHeadersOf(respHeaders),
		Trailers: models.GrpcHeaders{
			PseudoHeaders:   make(map[string]string),
			OrdinaryHeaders: make(map[string]string),
		},
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

	// The real status comes from the terminal RecvMsg error, NOT from the
	// trailer map.
	//
	// grpc-go consumes grpc-status and grpc-message off the wire and turns
	// them into the error RecvMsg returns; Trailer() carries only the
	// application's own custom trailers. So a stream that ended INTERNAL has
	// no grpc-status key at all, and the default below would fabricate "0" —
	// replaying a failed RPC as a clean pass. Reading the status off the
	// error is what makes a mid-stream failure visible.
	if drainErr != nil {
		if st, ok := status.FromError(drainErr); ok {
			grpcResp.Trailers.OrdinaryHeaders["grpc-status"] = strconv.Itoa(int(st.Code()))
			if msg := st.Message(); msg != "" {
				grpcResp.Trailers.OrdinaryHeaders["grpc-message"] = msg
			}
		}
	}

	// Ensure mandatory gRPC status is present
	if _, ok := grpcResp.Trailers.OrdinaryHeaders["grpc-status"]; !ok {
		grpcResp.Trailers.OrdinaryHeaders["grpc-status"] = "0"
	}
	if _, ok := grpcResp.Trailers.OrdinaryHeaders["grpc-message"]; !ok {
		grpcResp.Trailers.OrdinaryHeaders["grpc-message"] = ""
	}

	grpcResp.SetMessages(respMsgs)

	logger.Info("successfully completed gRPC simulation",
		zap.String("method", path), zap.Int("response_messages", len(respMsgs)))
	if silent != nil {
		return grpcResp, silent
	}
	if heldOpen && stalled {
		return grpcResp, ErrStreamStalled
	}
	if heldOpen {
		return grpcResp, ErrStreamHeldOpen
	}
	return grpcResp, nil
}

// grpcHeadersOf is the response headers respHeaders as a GrpcResp holds them:
// with :status 200 and content-type application/grpc when the app sent none
// (the gRPC client hides :status; a successful call's is always 200).
func grpcHeadersOf(respHeaders metadata.MD) models.GrpcHeaders {
	h := models.GrpcHeaders{PseudoHeaders: make(map[string]string), OrdinaryHeaders: make(map[string]string)}
	for k, v := range respHeaders {
		val := strings.Join(v, ", ")
		if strings.HasPrefix(k, ":") {
			h.PseudoHeaders[k] = val
		} else {
			h.OrdinaryHeaders[k] = val
		}
	}
	if _, ok := h.PseudoHeaders[":status"]; !ok {
		h.PseudoHeaders[":status"] = "200"
	}
	if _, ok := h.OrdinaryHeaders["content-type"]; !ok {
		h.OrdinaryHeaders["content-type"] = "application/grpc"
	}
	return h
}

// headersOnly is the answer of a stream whose headers came and nothing more
// (an ErrAnswerStalled after its answer began): its headers, for the replay
// to judge them against the recording before it calls the stream not judged.
// nil with no metadata in hand: a start is never made up.
func headersOnly(respHeaders metadata.MD) *models.GrpcResp {
	if respHeaders == nil {
		return nil
	}
	return &models.GrpcResp{Headers: grpcHeadersOf(respHeaders)}
}

// stalledStart is what of the answer a call the stall rule cut while keploy
// still sent its request had: the app's headers, when they came before the
// cut (the cut call's Header returns at once), else nothing.
func stalledStart(stream grpc.ClientStream, answer *answerWatch) *models.GrpcResp {
	if !answer.answered.Load() {
		return nil
	}
	md, _ := stream.Header()
	return headersOnly(md)
}

// grpcRequestSize is the size of reqMsgs on the wire, their 5-byte prefixes
// with them: what the stall rule says of a request the app did not take
// whole. A payload that does not parse counts as empty.
func grpcRequestSize(reqMsgs []models.GrpcLengthPrefixedMessage) int64 {
	var n int64
	for _, m := range reqMsgs {
		n += 5
		if m.DecodedData != "" {
			if b, err := protoscope.NewScanner(m.DecodedData).Exec(); err == nil {
				n += int64(len(b))
			}
		}
	}
	return n
}

// recordedMessagesComplete is how many response messages complete resp, the
// recorded answer: a keepalive after them, under the proven switch, is one
// beyond the recording (pingWatch). One empty message is how a recording keeps
// an answer of trailers alone (SimulateGRPC), and also an empty message
// (google.protobuf.Empty): the recorded status, as a gRPC client reads it
// (models.GrpcResp.TrailersAsRead), tells them apart, an error being an
// answer of trailers alone, which none completes.
func recordedMessagesComplete(resp models.GrpcResp) int {
	msgs := resp.AllMessages()
	if len(msgs) == 1 && msgs[0].MessageLength == 0 && msgs[0].DecodedData == "" {
		if code := strings.TrimSpace(resp.TrailersAsRead().OrdinaryHeaders["grpc-status"]); code != "" && code != "0" {
			return 0
		}
	}
	return len(msgs)
}

// defaultGrpcCallTimeout bounds a replayed gRPC call when the caller supplies
// no APITimeout. It exists so the response drain can never hang a test set on
// a stream the server holds open; it is not a latency target.
const defaultGrpcCallTimeout = 30 * time.Second

// CreateLengthPrefixedMessageFromPayload creates a GrpcLengthPrefixedMessage
// from the FIRST length-prefixed message in data.
//
// It honours the declared length rather than protoscoping everything after
// byte 5. Those are the same thing for a well-formed single message, where
// 5+length == len(data), and different for a stream: decoding past the
// declared end swallows the NEXT message's 5-byte header as if it were
// protobuf payload, producing a DecodedData that disagrees with the
// MessageLength beside it. Prefer SplitLengthPrefixedMessages when the input
// may carry more than one message.
//
// A declared length longer than the data available means the capture was
// truncated. The bytes present are still decoded — callers that must not
// serve a partial body detect it by comparing MessageLength against the
// re-encoded length — but nothing is invented to fill the gap.
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

	end := 5 + int(msg.MessageLength)
	if end > len(data) || end < 5 {
		// Truncated capture, or a length so large it overflowed the add.
		end = len(data)
	}

	// The payload could be empty. We only parse it if it is present.
	if end > 5 {
		// Use protoscope to decode the message.
		msg.DecodedData = protoscope.Write(data[5:end], protoscope.WriterOptions{})
	}

	return msg
}

// ErrTrailingGrpcBytes reports that a length-prefixed chain did not consume
// its input exactly — the last message's declared length ran past the end of
// the buffer, or stopped short of it with fewer than 5 bytes left over.
var ErrTrailingGrpcBytes = errors.New("grpc: length-prefixed chain does not consume its buffer exactly")

// SplitLengthPrefixedMessages walks a contiguous stream of length-prefixed
// gRPC messages and returns them in wire order.
//
// THIS IS THE ONLY CORRECT WAY TO FIND MESSAGE BOUNDARIES, and the reason is
// worth stating because the obvious alternative is wrong. HTTP/2 DATA frame
// boundaries are NOT message boundaries: one message may span several frames,
// several messages may share one frame, and where the splits fall depends on
// MTU, flow-control windows and write batching. Recording one entry per frame
// would make the same RPC produce a different mock on every run and would
// replay a message split across two frames as two corrupt ones. Boundaries
// come from the 5-byte prefix and nowhere else.
//
// The walk is STRICT: the chain must consume the buffer exactly. A partial
// trailing message means the capture was cut off, and inventing a message
// from the remainder would record a body that cannot reproduce. Callers get
// ErrTrailingGrpcBytes and the messages decoded so far, so they can decide
// whether to skip the mock or record the prefix.
//
// An empty buffer yields no messages and no error — that is a legitimate
// direction (a client-streaming call whose response carries only trailers).
func SplitLengthPrefixedMessages(data []byte) ([]models.GrpcLengthPrefixedMessage, error) {
	var msgs []models.GrpcLengthPrefixedMessage
	for off := 0; off < len(data); {
		if len(data)-off < 5 {
			return msgs, fmt.Errorf("%w: %d trailing byte(s) after %d message(s), too few for a 5-byte prefix",
				ErrTrailingGrpcBytes, len(data)-off, len(msgs))
		}
		length := binary.BigEndian.Uint32(data[off+1 : off+5])
		end := off + 5 + int(length)
		if end < off+5 || end > len(data) {
			return msgs, fmt.Errorf("%w: message %d declares %d bytes but only %d remain",
				ErrTrailingGrpcBytes, len(msgs), length, len(data)-off-5)
		}
		msg := models.GrpcLengthPrefixedMessage{
			CompressionFlag: uint(data[off]),
			MessageLength:   length,
		}
		if length > 0 {
			msg.DecodedData = protoscope.Write(data[off+5:end], protoscope.WriterOptions{})
		}
		msgs = append(msgs, msg)
		off = end
	}
	return msgs, nil
}

// CreatePayloadFromLengthPrefixedMessages re-encodes a whole direction back
// into the contiguous wire form SplitLengthPrefixedMessages accepts.
func CreatePayloadFromLengthPrefixedMessages(msgs []models.GrpcLengthPrefixedMessage) ([]byte, error) {
	var out []byte
	for i, m := range msgs {
		b, err := CreatePayloadFromLengthPrefixedMessage(m)
		if err != nil {
			return nil, fmt.Errorf("message %d of %d: %w", i, len(msgs), err)
		}
		out = append(out, b...)
	}
	return out, nil
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

// MaxGRPCFrameLength bounds the message length ReadGRPCFrame is willing to
// allocate for. The length prefix is 4 attacker-supplied bytes off the wire,
// so without a bound a single frame header asks for up to 4 GiB before any of
// the payload has been read — a trivial remote memory-exhaustion of the proxy.
// The bound matches the other wire-buffer caps in this repo (relay
// DefaultPerConnCap, the agent's outgoing mock buffer) and is an order of
// magnitude above grpc-go's own 4 MiB default receive limit, so it cannot
// reject a message a real gRPC peer would have accepted.
const MaxGRPCFrameLength = 64 * 1024 * 1024

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
	// Bound BEFORE allocating: io.ReadFull can only reject an oversized
	// frame after make() has already committed the memory. The check also
	// keeps the 5+msgLen below from wrapping — in uint32 arithmetic
	// 5+0xFFFFFFFF is 4, which then panics in copy(frame[5:], msgBuf) with
	// "slice bounds out of range".
	if msgLen > MaxGRPCFrameLength {
		return nil, fmt.Errorf("grpc frame message length %d exceeds the %d byte limit", msgLen, MaxGRPCFrameLength)
	}

	// Read message data
	msgBuf := make([]byte, msgLen)
	if _, err := io.ReadFull(r, msgBuf); err != nil {
		return nil, fmt.Errorf("failed to read message: %w", err)
	}

	// Combine all parts. int arithmetic, not uint32: msgLen is bounded
	// above, but the widening makes the absence of wraparound local and
	// obvious rather than dependent on a check ten lines up.
	frame := make([]byte, 5+int(msgLen))
	frame[0] = compFlag[0]
	copy(frame[1:5], lenBuf)
	copy(frame[5:], msgBuf)

	return frame, nil
}
