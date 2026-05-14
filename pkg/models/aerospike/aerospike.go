// Package aerospike defines the on-disk mock model for Aerospike
// traffic. Layout mirrors the MySQL and Postgres model packages so the
// recorder/replayer wiring follows the same patterns.
//
// A single Mock groups one or more request/response pairs. Each pair
// carries the outer proto header in PacketInfo (version, type byte,
// 6-byte length) and a typed Message — info entries for type=1 frames
// and a structured AS_MSG envelope for type=3/4 frames.
package aerospike

import (
	"encoding/gob"
	"time"

	"gopkg.in/yaml.v3"
)

// FrameType is the on-disk spelling of the wire-level type byte. It is
// stored as a string rather than a uint8 so YAML mocks remain
// human-readable when edited or diffed.
type FrameType string

const (
	FrameInfo            FrameType = "Info"
	FrameAdmin           FrameType = "Admin"
	FrameMessage         FrameType = "Message"
	FrameMessageCompress FrameType = "MessageCompressed"
)

// Spec is the top-level container kept for symmetry with
// mysql.Spec / postgres.Spec. The recorder writes individual request /
// response pairs into Mock.Spec.AerospikeRequests /
// Mock.Spec.AerospikeResponses; this struct is preserved as the
// canonical envelope when an integration prefers a single grouped
// payload (e.g. a captured info-protocol burst).
// Spec mirrors mysql.Spec / postgres.Spec — required envelope
// fields (Metadata, Requests, Responses, Header, Message) are
// always serialised so a tool reading the on-disk YAML can
// rely on these keys being present even when their value is
// the zero value. Optional fields (Meta, CreatedAt, timestamps)
// keep `omitempty`.
type Spec struct {
	Metadata         map[string]string `json:"metadata" yaml:"metadata"`
	Requests         []Request         `json:"requests" yaml:"requests"`
	Responses        []Response        `json:"responses" yaml:"responses"`
	CreatedAt        int64             `json:"created,omitempty" yaml:"created,omitempty"`
	ReqTimestampMock time.Time         `json:"ReqTimestampMock,omitempty" yaml:"ReqTimestampMock,omitempty"`
	ResTimestampMock time.Time         `json:"ResTimestampMock,omitempty" yaml:"ResTimestampMock,omitempty"`
}

// Request is one client → server frame.
type Request struct {
	Header  *PacketInfo       `json:"header" yaml:"header"`
	Meta    map[string]string `json:"meta,omitempty" yaml:"meta,omitempty"`
	Message yaml.Node         `json:"message" yaml:"message"`
}

// Response is one server → client frame.
type Response struct {
	Header  *PacketInfo       `json:"header" yaml:"header"`
	Meta    map[string]string `json:"meta,omitempty" yaml:"meta,omitempty"`
	Message yaml.Node         `json:"message" yaml:"message"`
}

// PacketInfo is the decoded outer proto header plus a string-typed
// FrameType for human-readable diffs.
type PacketInfo struct {
	Header *Header   `json:"header" yaml:"header"`
	Type   FrameType `json:"packet_type" yaml:"packet_type"`
}

// Header mirrors wire.Header but stays in the on-disk model so the
// recorder/replayer can serialise mocks without dragging the wire
// package into the public model surface.
type Header struct {
	Version uint8  `json:"version" yaml:"version"`
	Type    uint8  `json:"type" yaml:"type"`
	Length  uint64 `json:"length" yaml:"length"`
}

// InfoMessage is the typed payload for FrameInfo frames.
type InfoMessage struct {
	Entries []InfoEntry `json:"entries,omitempty" yaml:"entries,omitempty"`
}

// InfoEntry mirrors wire.InfoEntry on-disk.
type InfoEntry struct {
	Name     string `json:"name" yaml:"name"`
	Value    string `json:"value,omitempty" yaml:"value,omitempty"`
	HasValue bool   `json:"has_value,omitempty" yaml:"has_value,omitempty"`
}

// AsMsgMessage is the typed payload for FrameMessage / FrameMessageCompressed
// frames. Fields and Ops are decoded so matchers can compare on
// namespace/set/key/op-type without re-decoding the raw payload, and
// RawBody preserves the original bytes for byte-exact replay.
type AsMsgMessage struct {
	Header2 *AsMsgHeader2 `json:"header2,omitempty" yaml:"header2,omitempty"`
	Fields  []AsMsgField  `json:"fields,omitempty" yaml:"fields,omitempty"`
	Ops     []AsMsgOp     `json:"ops,omitempty" yaml:"ops,omitempty"`
	// RawBody is the original body bytes (post-decompression for
	// FrameMessageCompressed). Recorder writes both Raw and structured
	// fields so a replay can byte-replay without round-tripping
	// through the encoder, but matchers still get a typed view.
	RawBody []byte `json:"raw_body,omitempty" yaml:"raw_body,omitempty"`
	// Compression names the original wrapper for FrameMessageCompressed
	// (currently "lz4" or "zlib"). Empty for FrameMessage.
	Compression string `json:"compression,omitempty" yaml:"compression,omitempty"`
}

// AsMsgHeader2 is the 22-byte secondary header that lives at the
// start of every AS_MSG body.
type AsMsgHeader2 struct {
	HeaderSize     uint8  `json:"header_size" yaml:"header_size"`
	Info1          uint8  `json:"info1" yaml:"info1"`
	Info2          uint8  `json:"info2" yaml:"info2"`
	Info3          uint8  `json:"info3" yaml:"info3"`
	ResultCode     uint8  `json:"result_code" yaml:"result_code"`
	Generation     uint32 `json:"generation" yaml:"generation"`
	RecordTTL      uint32 `json:"record_ttl" yaml:"record_ttl"`
	TransactionTTL uint32 `json:"transaction_ttl" yaml:"transaction_ttl"`
	NumFields      uint16 `json:"num_fields" yaml:"num_fields"`
	NumOps         uint16 `json:"num_ops" yaml:"num_ops"`
}

// AsMsgField is one decoded field. Type values are documented in
// wire/message.go.
type AsMsgField struct {
	Type uint8  `json:"type" yaml:"type"`
	Data []byte `json:"data,omitempty" yaml:"data,omitempty"`
}

// AsMsgOp is one decoded op carried inside AS_MSG. CDT sub-ops are
// decoded into the same struct: BinName + ParticleType identify the
// container, Data carries the CDT command bytes.
type AsMsgOp struct {
	OpType       uint8  `json:"op_type" yaml:"op_type"`
	ParticleType uint8  `json:"particle_type" yaml:"particle_type"`
	Version      uint8  `json:"version" yaml:"version"`
	BinName      string `json:"bin_name,omitempty" yaml:"bin_name,omitempty"`
	Data         []byte `json:"data,omitempty" yaml:"data,omitempty"`
}

func init() {
	gob.Register(&Spec{})
	gob.Register(&Request{})
	gob.Register(&Response{})
	gob.Register(&PacketInfo{})
	gob.Register(&Header{})
	gob.Register(&InfoMessage{})
	gob.Register(InfoEntry{})
	gob.Register(&AsMsgMessage{})
	gob.Register(&AsMsgHeader2{})
	gob.Register(AsMsgField{})
	gob.Register(AsMsgOp{})
}
