package models

import (
	"time"
)

type GrpcSpec struct {
	Metadata         map[string]string             `json:"metadata" yaml:"metadata"`
	GrpcReq          GrpcReq                       `json:"grpcReq" yaml:"grpcReq"`
	GrpcResp         GrpcResp                      `json:"grpcResp" yaml:"grpcResp"`
	AppPort          uint16                        `json:"app_port" yaml:"app_port,omitempty"`
	Created          int64                         `json:"created" yaml:"created"`
	Assertions       map[AssertionType]interface{} `json:"assertions" yaml:"assertions"`
	ReqTimestampMock time.Time                     `json:"reqTimestampMock" yaml:"reqTimestampMock,omitempty"`
	ResTimestampMock time.Time                     `json:"resTimestampMock" yaml:"resTimestampMock,omitempty"`
}

type GrpcHeaders struct {
	PseudoHeaders   map[string]string `json:"pseudo_headers" yaml:"pseudo_headers"`
	OrdinaryHeaders map[string]string `json:"ordinary_headers" yaml:"ordinary_headers"`
}

type GrpcLengthPrefixedMessage struct {
	CompressionFlag uint   `json:"compression_flag" yaml:"compression_flag"`
	MessageLength   uint32 `json:"message_length" yaml:"message_length"`
	DecodedData     string `json:"decoded_data" yaml:"decoded_data"`
}

// A gRPC direction carries a SEQUENCE of length-prefixed messages, not one.
// Unary sends exactly one each way; client-, server- and bidi-streaming send
// many, and server reflection is bidi-streaming, so this is not an exotic
// case.
//
// Body is message 0. AdditionalMessages is the TAIL — messages 1..N — and is
// empty for every unary call, which is why a unary mock is byte-identical on
// disk to one written before streams were representable.
//
// WHY A TAIL AND NOT A LIST. The obvious shape, retyping Body to a slice, is
// undeliverable: mocks travel from agent to CLI over gob with no version
// handshake (agent/routes/record.go -> platform/http/agent.go), and gob sends
// a type's whole transitive graph before the first value. Changing Body's type
// breaks decoding of EVERY mock in the stream, including HTTP mocks whose
// GrpcReq is nil, on any agent/CLI version skew. Adding a field does not.
//
// The tail also makes an illegal state unrepresentable. A full list alongside
// Body would be two sources of truth for message 0, and every existing
// in-place `Body.DecodedData = ...` write in the replay path would silently
// stop taking effect. With a tail there is exactly one place message 0 lives.
//
// Read through AllMessages(); write through SetMessages(). Do not append to
// AdditionalMessages directly.
type GrpcReq struct {
	Headers GrpcHeaders               `json:"headers" yaml:"headers"`
	Body    GrpcLengthPrefixedMessage `json:"body" yaml:"body"`
	// AdditionalMessages carries messages 1..N of a streaming request.
	// omitempty keeps it off disk entirely for unary.
	AdditionalMessages []GrpcLengthPrefixedMessage `json:"additional_messages,omitempty" yaml:"additional_messages,omitempty"`
	Timestamp          time.Time                   `json:"timestamp" yaml:"timestamp"`
}

type GrpcResp struct {
	Headers GrpcHeaders               `json:"headers" yaml:"headers"`
	Body    GrpcLengthPrefixedMessage `json:"body" yaml:"body"`
	// AdditionalMessages carries messages 1..N of a streaming response.
	AdditionalMessages []GrpcLengthPrefixedMessage `json:"additional_messages,omitempty" yaml:"additional_messages,omitempty"`
	Trailers           GrpcHeaders                 `json:"trailers" yaml:"trailers"`
	Timestamp          time.Time                   `json:"timestamp" yaml:"timestamp"`
}

// AllMessages returns the direction's messages in wire order.
//
// It ALWAYS returns at least one element, and that is load-bearing rather
// than defensive. A zero-value Body is a real recorded shape — the Health
// check test case stores message_length: 0 with an empty decoded_data, and
// replay sends exactly one message with a nil payload for it. Returning an
// empty slice for that would make `for range AllMessages()` send nothing and
// hang the RPC, turning a passing unary test into a timeout.
func (r GrpcReq) AllMessages() []GrpcLengthPrefixedMessage {
	return allMessages(r.Body, r.AdditionalMessages)
}

// AllMessages returns the response's messages in wire order. See GrpcReq.AllMessages.
func (r GrpcResp) AllMessages() []GrpcLengthPrefixedMessage {
	return allMessages(r.Body, r.AdditionalMessages)
}

func allMessages(head GrpcLengthPrefixedMessage, tail []GrpcLengthPrefixedMessage) []GrpcLengthPrefixedMessage {
	out := make([]GrpcLengthPrefixedMessage, 0, 1+len(tail))
	out = append(out, head)
	return append(out, tail...)
}

// SetMessages stores msgs in wire order, splitting head from tail.
//
// An empty msgs zeroes the direction rather than leaving a stale body, so
// callers cannot half-write a direction by passing nothing.
func (r *GrpcReq) SetMessages(msgs []GrpcLengthPrefixedMessage) {
	r.Body, r.AdditionalMessages = splitMessages(msgs)
}

// SetMessages stores msgs in wire order. See GrpcReq.SetMessages.
func (r *GrpcResp) SetMessages(msgs []GrpcLengthPrefixedMessage) {
	r.Body, r.AdditionalMessages = splitMessages(msgs)
}

func splitMessages(msgs []GrpcLengthPrefixedMessage) (GrpcLengthPrefixedMessage, []GrpcLengthPrefixedMessage) {
	if len(msgs) == 0 {
		return GrpcLengthPrefixedMessage{}, nil
	}
	if len(msgs) == 1 {
		// nil, not an empty slice: omitempty must keep it off disk so a
		// unary mock stays byte-identical to one written before streaming
		// was representable.
		return msgs[0], nil
	}
	tail := make([]GrpcLengthPrefixedMessage, len(msgs)-1)
	copy(tail, msgs[1:])
	return msgs[0], tail
}

// IsStream reports whether this direction carries more than one message.
func (r GrpcReq) IsStream() bool { return len(r.AdditionalMessages) > 0 }

// IsStream reports whether this direction carries more than one message.
func (r GrpcResp) IsStream() bool { return len(r.AdditionalMessages) > 0 }

// GrpcStream is a helper function to combine the request-response model in a single struct
type GrpcStream struct {
	StreamID uint32
	GrpcReq  GrpcReq
	GrpcResp GrpcResp

	// to handle request (coming in multiple frames)
	ReqRawData        []byte
	ReqPrefixParsed   bool
	ReqExpectedLength uint32

	// to handle response (coming in multiple frames)
	RespRawData        []byte
	RespPrefixParsed   bool
	RespExpectedLength uint32
}

// NewGrpcStream returns a GrpcStream with all the nested maps initialised.
func NewGrpcStream(streamID uint32) GrpcStream {
	return GrpcStream{
		StreamID: streamID,
		GrpcReq: GrpcReq{
			Headers: GrpcHeaders{
				PseudoHeaders:   make(map[string]string),
				OrdinaryHeaders: make(map[string]string),
			},
		},
		GrpcResp: GrpcResp{
			Headers: GrpcHeaders{
				PseudoHeaders:   make(map[string]string),
				OrdinaryHeaders: make(map[string]string),
			},
			Trailers: GrpcHeaders{
				PseudoHeaders:   make(map[string]string),
				OrdinaryHeaders: make(map[string]string),
			},
		},
	}
}

type ProtoConfig struct {
	ProtoFile    string
	ProtoDir     string
	ProtoInclude []string
	RequestURI   string
}
