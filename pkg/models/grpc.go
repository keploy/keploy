package models

import (
	"time"
)

type GrpcSpec struct {
	GrpcReq          GrpcReq                       `json:"grpcReq" yaml:"grpcReq"`
	GrpcResp         GrpcResp                      `json:"grpcResp" yaml:"grpcResp"`
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

type GrpcReq struct {
	Headers   GrpcHeaders               `json:"headers" yaml:"headers"`
	Body      GrpcLengthPrefixedMessage `json:"body" yaml:"body"`
	Timestamp time.Time                 `json:"timestamp" yaml:"timestamp"`
}

type GrpcResp struct {
	Headers   GrpcHeaders               `json:"headers" yaml:"headers"`
	Body      GrpcLengthPrefixedMessage `json:"body" yaml:"body"`
	Trailers  GrpcHeaders               `json:"trailers" yaml:"trailers"`
	Timestamp time.Time                 `json:"timestamp" yaml:"timestamp"`
}

// GrpcStream is a helper function to combine the request-response model in a single struct.
type GrpcStream struct {
	StreamID uint32
	GrpcReq  GrpcReq
	GrpcResp GrpcResp
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
