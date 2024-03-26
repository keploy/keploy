package models

import (
	"time"
)

type GrpcSpec struct {
	GrpcReq          GrpcFinalReq  `json:"grpcReq" yaml:"grpcReq"`
	GrpcResp         GrpcFinalResp `json:"grpcResp" yaml:"grpcResp"`
	ReqTimestampMock time.Time     `json:"reqTimestampMock" yaml:"reqTimestampMock,omitempty"`
	ResTimestampMock time.Time     `json:"resTimestampMock" yaml:"resTimestampMock,omitempty"`
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
type PrefMessagePointer struct {
	Body GrpcLengthPrefixedMessage
	Left *PrefMessagePointer
}

type GrpcReqStream struct {
	Headers  GrpcHeaders
	BodyPref *PrefMessagePointer
}

type GrpcRespStream struct {
	Headers  GrpcHeaders
	BodyPref *PrefMessagePointer
	Trailers GrpcHeaders
}
type GrpcFinalReq struct {
	Headers GrpcHeaders                 `json:"headers" yaml:"headers"`
	Body    []GrpcLengthPrefixedMessage `json:"body" yaml:"body"`
}
type GrpcFinalResp struct {
	Headers  GrpcHeaders                 `json:"headers" yaml:"headers"`
	Body     []GrpcLengthPrefixedMessage `json:"body" yaml:"body"`
	Trailers GrpcHeaders
}

// GrpcStream is a helper function to combine the request-response model in a single struct.
type GrpcStream struct {
	StreamID uint32
	GrpcReq  GrpcReqStream
	GrpcResp GrpcRespStream
}

// NewGrpcStream returns a GrpcStream with all the nested maps initialised.
func NewGrpcStream(streamID uint32) GrpcStream {
	return GrpcStream{
		StreamID: streamID,
		GrpcReq: GrpcReqStream{
			Headers: GrpcHeaders{
				PseudoHeaders:   make(map[string]string),
				OrdinaryHeaders: make(map[string]string),
			},
			BodyPref: &PrefMessagePointer{
				Body: GrpcLengthPrefixedMessage{},
				Left: nil,
			},
		},
		GrpcResp: GrpcRespStream{
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
